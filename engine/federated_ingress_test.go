package engine

// federated_ingress_test.go — a participant's own system sends a consented
// federated query through its gateway: POST /Task, the Da Vinci CDex
// task-based data request. The Task is carried to the facility its owner
// names exactly as sent, and the facility's answer is relayed exactly.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// fqTask is the CDex data-request Task for member's DiagnosticReports, owned
// by owner.
func fqTask(t *testing.T, member, owner string) []byte {
	t.Helper()
	b, err := shnsdk.BuildCDexTaskDataRequest("Patient/"+member, "DiagnosticReport", "2024-01-01", "2026-10-18",
		shnsdk.CDexTaskMeta{AuthoredOn: time.Unix(1700000000, 0).UTC(), Requester: "provider", Owner: owner})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fqTaskWith is fqTask for member owned by "facility" with fields set or
// replaced (nil removes one).
func fqTaskWith(t *testing.T, member string, fields map[string]any) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(fqTask(t, member, "facility"), &doc); err != nil {
		t.Fatal(err)
	}
	for k, v := range fields {
		if v == nil {
			delete(doc, k)
			continue
		}
		doc[k] = v
	}
	return mustJSON(t, doc)
}

// fqRecords is a records Bundle with one DiagnosticReport about subject.
func fqRecords(subject string) []byte {
	return []byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"DiagnosticReport","id":"dr1","status":"final",` +
		`"code":{"coding":[{"system":"http://loinc.org","code":"24627-2"}]},"subject":{"reference":"` + subject + `"}}}]}`)
}

// fqAnswer is the facility's answer to task: the Task fulfilled with records.
func fqAnswer(t *testing.T, task, records []byte) []byte {
	t.Helper()
	_, fulfilled, err := sealCDexFulfillment(task, records)
	if err != nil {
		t.Fatal(err)
	}
	return fulfilled
}

// fqIngressPost is the EHR's POST of body to the route.
func fqIngressPost(body []byte) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/Task", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/fhir+json")
	return req
}

// newFQExchange is an in-process exchange whose registry also holds a
// facility (sealed for with the stub recipient's keys) and a second payer.
func newFQExchange(t *testing.T, level ConformanceEnforcement) *inProcessExchange {
	t.Helper()
	env := newInProcessExchange(t)
	env.originator.cfg.ConformanceEnforcement = level
	payer, ok := env.originator.cfg.Reg.Lookup("payer")
	if !ok {
		t.Fatal("the exchange registers no payer")
	}
	facility := payer
	facility.ID, facility.Role = "facility", "facility"
	env.originator.cfg.Reg.Set("facility", facility)
	return env
}

// runFQIngress sends body through the provider's federated-query route at
// level with the facility answering answer, and returns the exchange, the
// caller's answer and the one exchange record.
func runFQIngress(t *testing.T, level ConformanceEnforcement, body, answer []byte) (*inProcessExchange, *httptest.ResponseRecorder, ExchangeRecord) {
	t.Helper()
	env := newFQExchange(t, level)
	env.payerReturns(LegResult{Response: testResponse(answer)})
	var got exchangeRecords
	env.originator.cfg.ExchangeObserved = got.observe
	rec := httptest.NewRecorder()
	env.originator.ingressRoute(RouteFederatedQuery)(rec, fqIngressPost(body))
	return env, rec, got.only(t)
}

// The Task is carried to the facility its owner names exactly as sent, the
// leg is authorized for the patient with that facility as the custodian whose
// consent is checked, and the facility's answer is relayed exactly, at every
// level.
func TestFederatedQueryIngress_CarriesTheTaskAsSent(t *testing.T) {
	task := fqTask(t, "MBR-UC05", "facility")
	answer := fqAnswer(t, task, fqRecords("Patient/MBR-UC05"))
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env := newFQExchange(t, level)
			env.payerReturns(LegResult{Response: testResponse(answer)})
			var authorized []authorizeReq
			env.substrate.authorizeFaults = []func(*http.Request) (*http.Response, error){func(r *http.Request) (*http.Response, error) {
				b, _ := io.ReadAll(r.Body)
				var a authorizeReq
				if err := json.Unmarshal(b, &a); err != nil {
					t.Errorf("authorize request: %v", err)
				}
				authorized = append(authorized, a)
				return nil, nil
			}}
			var got exchangeRecords
			env.originator.cfg.ExchangeObserved = got.observe
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteFederatedQuery)(rec, fqIngressPost(task))
			if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), answer) {
				t.Fatalf("answer %d %s, want the facility's answer relayed exactly", rec.Code, rec.Body)
			}
			if sent := env.lastRequestPayload(); !bytes.Equal(sent, task) {
				t.Fatalf("the facility's side received\n%s\nwant the Task as sent\n%s", sent, task)
			}
			if len(authorized) != 1 || authorized[0].Operation != "federated-query-submit" || authorized[0].Custodian != "facility" || authorized[0].SubjectPCI == "" {
				t.Fatalf("authorized %+v, want one federated-query-submit for the patient with the facility as custodian", authorized)
			}
			x := got.only(t)
			if x.Outcome != ExchangeAnswered || x.Route != RouteFederatedQuery || x.Exchange != "federated-query" ||
				x.Recipient != "facility" || len(x.Edits) != 0 {
				t.Fatalf("record %+v, want an answered federated query with the facility, carried unedited", x)
			}
		})
	}
}

// A Task whose owner names no facility the network registers is refused as
// routing before anything is sent, at every level, and says what is missing.
// There is no default facility.
func TestFederatedQueryIngress_UnroutableTaskIsRefused(t *testing.T) {
	holder := func(system, value string) map[string]any {
		return map[string]any{"identifier": map[string]any{"system": system, "value": value}}
	}
	for name, row := range map[string]struct {
		owner any
		msg   string
	}{
		"no owner":                           {nil, federatedQueryNoOwner},
		"owner by reference only":            {map[string]any{"reference": "Organization/facility"}, federatedQueryNoOwner},
		"owner in another identifier system": {holder("http://hl7.org/fhir/sid/us-npi", "facility"), federatedQueryNoOwner},
		"owner identifier with no value":     {holder(holderIdentifierSystem, ""), federatedQueryNoOwner},
		"owner identifier with no system":    {map[string]any{"identifier": map[string]any{"value": "facility"}}, federatedQueryNoOwner},
		"owner not registered":               {holder(holderIdentifierSystem, "elsewhere"), `the Task's owner \"elsewhere\" is not a holder the network registers`},
		"owner a payer":                      {holder(holderIdentifierSystem, "payer"), `the Task's owner \"payer\" is a payer, not a facility`},
		"owner the provider itself":          {holder(holderIdentifierSystem, "provider"), `the Task's owner \"provider\" is a provider, not a facility`},
		// A value of the wrong type reads one way: an owner that names no
		// holder identifier, refused as routing.
		"owner a string":          {"facility", federatedQueryNoOwner},
		"owner identifier a list": {map[string]any{"identifier": []any{map[string]any{"system": holderIdentifierSystem, "value": "facility"}}}, federatedQueryNoOwner},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env, rec, x := runFQIngress(t, level, fqTaskWith(t, "MBR-UC05", map[string]any{"owner": row.owner}), nil)
				if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), row.msg) {
					t.Fatalf("answer %d %s, want 422 %q", rec.Code, rec.Body, row.msg)
				}
				if env.routeHitCount() != 0 || env.substrate.authorizeHits != 0 {
					t.Fatalf("a refused query crossed the network (%d legs, %d authorizations)", env.routeHitCount(), env.substrate.authorizeHits)
				}
				wantRefusal(t, x, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalRouting)
			})
		}
	}
}

// A Task that is no data request for one patient's named documents is
// refused before the network at every level, as the facility would refuse
// it: the patient is read as the facility reads it. Member names are read
// exactly.
func TestFederatedQueryIngress_RefusesWhatIsNoDataRequest(t *testing.T) {
	task := fqTask(t, "MBR-UC05", "facility")
	var doc map[string]any
	if err := json.Unmarshal(task, &doc); err != nil {
		t.Fatal(err)
	}
	inputs, _ := doc["input"].([]any)
	var dataQuery any
	for _, in := range inputs {
		if strings.Contains(string(mustJSON(t, in)), "data-query") {
			dataQuery = in
		}
	}
	if dataQuery == nil {
		t.Fatal("the fixture has no data-query input")
	}
	const unreadable = "federated query rejected: the Task cannot be read one way only"
	for name, row := range map[string]struct {
		body []byte
		msg  string
		rule string // what reads as no data request is the leg's scope; what reads two ways, integrity
	}{
		"not JSON":                    {[]byte("{"), unreadable, RefusalIntegrity},
		"another resource":            {[]byte(`{"resourceType":"Claim"}`), `expected Task, got \"Claim\"`, RefusalAuthority},
		"no patient":                  {fqTaskWith(t, "MBR-UC05", map[string]any{"for": nil}), "Task.for missing/invalid", RefusalAuthority},
		"two data queries":            {fqTaskWith(t, "MBR-UC05", map[string]any{"input": append(append([]any{}, inputs...), dataQuery)}), "exactly one data-query input required", RefusalAuthority},
		"another task code":           {fqTaskWith(t, "MBR-UC05", map[string]any{"code": map[string]any{"coding": []any{map[string]any{"system": "http://example.org", "code": "x"}}}}), "Task.code is not data-request-query", RefusalAuthority},
		"contained patient not there": {fqTaskWith(t, "MBR-UC05", map[string]any{"for": map[string]any{"reference": "#pt"}}), "federated query rejected: ", RefusalAuthority},
		// A value of the wrong type reads one way: refused as the leg's scope.
		"for a string": {fqTaskWith(t, "MBR-UC05", map[string]any{"for": "Patient/MBR-UC05"}), "federated query rejected: ", RefusalAuthority},
		// Member names are read exactly, the owner's among them: a Task a FHIR
		// reader would read another way is not carried.
		"for in another case":   {bytes.Replace(task, []byte(`"for":`), []byte(`"For":`), 1), unreadable, RefusalIntegrity},
		"owner in another case": {bytes.Replace(task, []byte(`"owner":`), []byte(`"Owner":`), 1), unreadable, RefusalIntegrity},
		"owner repeated":        {bytes.Replace(task, []byte(`"owner":`), []byte(`"owner":{"identifier":{"system":"`+holderIdentifierSystem+`","value":"payer"}},"owner":`), 1), unreadable, RefusalIntegrity},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env, rec, x := runFQIngress(t, level, row.body, nil)
				refusedBeforeTheNetwork(t, env, rec, http.StatusBadRequest, row.msg)
				if env.substrate.authorizeHits != 0 {
					t.Fatalf("a refused query was authorized (%d)", env.substrate.authorizeHits)
				}
				wantRefusal(t, x, http.StatusBadRequest, RefusedByProviderGateway, row.rule)
			})
		}
	}
}

// The patient is bound as the other native legs bind one: a member this
// participant's system does not hold is carried (its consent is then the
// network's to check), refused only when the participant asks for known
// members; a system of record that fails refuses as it does elsewhere.
func TestFederatedQueryIngress_BindsThePatientAsTheOtherLegs(t *testing.T) {
	unknown := fqTask(t, "MBR-NOBODY", "facility")
	answer := fqAnswer(t, unknown, fqRecords("Patient/MBR-NOBODY"))
	sorErr := &SoRReadError{Kind: SoRUnavailable}
	sorStatus, sorMsg := SoRFailureResponse(sorErr)
	for _, level := range allLevels {
		t.Run("unknown member carried/"+level.String(), func(t *testing.T) {
			env, rec, _ := runFQIngress(t, level, unknown, answer)
			if rec.Code != http.StatusOK || !bytes.Equal(env.lastRequestPayload(), unknown) {
				t.Fatalf("answer %d %s, want a member the system does not hold carried as sent", rec.Code, rec.Body)
			}
		})
		t.Run("unknown member checked/"+level.String(), func(t *testing.T) {
			env := newFQExchange(t, level)
			env.originator.cfg.RequireKnownMembers = true
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteFederatedQuery)(rec, fqIngressPost(unknown))
			refusedBeforeTheNetwork(t, env, rec, http.StatusBadRequest, "unknown member")
		})
		t.Run("system of record fails/"+level.String(), func(t *testing.T) {
			env := newFQExchange(t, level)
			env.originator.cfg.SoR = failingSoR{SystemOfRecord: env.originator.cfg.SoR, err: sorErr}
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteFederatedQuery)(rec, fqIngressPost(fqTask(t, "MBR-UC05", "facility")))
			refusedBeforeTheNetwork(t, env, rec, sorStatus, sorMsg)
		})
	}
}

// The facility's refusal (a consent refusal among them) is relayed exactly.
func TestFederatedQueryIngress_RelaysTheFacilitysRefusal(t *testing.T) {
	oo := []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"forbidden"}]}`)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env := newFQExchange(t, level)
			env.payerReturns(LegResult{Status: http.StatusForbidden, Response: testResponse(oo)})
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteFederatedQuery)(rec, fqIngressPost(fqTask(t, "MBR-UC05", "facility")))
			if rec.Code != http.StatusForbidden || !bytes.Equal(rec.Body.Bytes(), oo) {
				t.Fatalf("answer %d %s, want the facility's 403 relayed exactly", rec.Code, rec.Body)
			}
		})
	}
}

// An answer repeating a member name, which could be read as two patients'
// records, is refused at every level.
func TestFederatedQueryIngress_AnswerRepeatingAMemberIsRefused(t *testing.T) {
	task := fqTask(t, "MBR-UC05", "facility")
	answer := bytes.Replace(fqAnswer(t, task, fqRecords("Patient/MBR-UC05")), []byte(`"status":"completed"`), []byte(`"status":"completed","status":"failed"`), 1)
	if !bytes.Contains(answer, []byte(`"status":"failed"`)) {
		t.Fatal("the fixture repeats no member")
	}
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			_, rec, x := runFQIngress(t, level, task, answer)
			if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), `facility federated query answer repeats the JSON member \"status\"`) {
				t.Fatalf("answer %d %s, want 502 naming the repeated member", rec.Code, rec.Body)
			}
			wantRefusal(t, x, http.StatusBadGateway, RefusedByProviderGateway, RefusalIntegrity)
		})
	}
}

// An answer that reads as about a patient other than the queried one is the
// facility's content (consent itself is enforced at the facility's gateway):
// relayed at none, relayed and recorded at observe and structural, refused at
// strict (RulePatientAnswer), as for an eligibility answer. The check reads
// the answer Task's own patient and every resource the answer contains.
func TestFederatedQueryIngress_AnswerAboutAnotherPatient(t *testing.T) {
	task := fqTask(t, "MBR-UC05", "facility")
	good := fqAnswer(t, task, fqRecords("Patient/MBR-UC05"))
	// with is the good answer with an output and a contained resource added.
	with := func(t *testing.T, output, contained any) []byte {
		t.Helper()
		var doc map[string]any
		if err := json.Unmarshal(good, &doc); err != nil {
			t.Fatal(err)
		}
		if output != nil {
			outs, _ := doc["output"].([]any)
			doc["output"] = append([]any{output}, outs...)
		}
		if contained != nil {
			c, _ := doc["contained"].([]any)
			doc["contained"] = append(c, contained)
		}
		return mustJSON(t, doc)
	}
	var others map[string]any
	if err := json.Unmarshal(fqRecords("Patient/MBR-OTHER"), &others); err != nil {
		t.Fatal(err)
	}
	others["id"] = "others"
	dataQuery := map[string]any{"type": map[string]any{"coding": []any{map[string]any{"system": "http://hl7.org/fhir/us/davinci-hrex/CodeSystem/hrex-temp", "code": "data-query"}}},
		"valueReference": map[string]any{"reference": "#others"}}
	for name, answer := range map[string][]byte{
		"the named records":                   fqAnswer(t, task, fqRecords("Patient/MBR-OTHER")),
		"another data-query output":           with(t, dataQuery, others),
		"a contained Bundle no output names":  with(t, nil, others),
		"the answer Task for another patient": fqAnswer(t, fqTask(t, "MBR-OTHER", "facility"), fqRecords("Patient/MBR-UC05")),
		// Its own patient must be named by a reference this gateway reads.
		"the answer Task naming its patient by identifier only": func() []byte {
			var doc map[string]any
			if err := json.Unmarshal(good, &doc); err != nil {
				t.Fatal(err)
			}
			doc["for"] = map[string]any{"identifier": map[string]any{"system": shnsdk.MemberSystem, "value": "MBR-OTHER"}}
			return mustJSON(t, doc)
		}(),
		// Its own patient is read whether or not it reads as a data request.
		"the answer Task for another patient, with no code": func() []byte {
			var doc map[string]any
			if err := json.Unmarshal(fqAnswer(t, fqTask(t, "MBR-OTHER", "facility"), fqRecords("Patient/MBR-UC05")), &doc); err != nil {
				t.Fatal(err)
			}
			delete(doc, "code")
			return mustJSON(t, doc)
		}(),
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env := newFQExchange(t, level)
				env.payerReturns(LegResult{Response: testResponse(answer)})
				ev := observeLevelEvents(env.originator)
				var got exchangeRecords
				env.originator.cfg.ExchangeObserved = got.observe
				rec := httptest.NewRecorder()
				env.originator.ingressRoute(RouteFederatedQuery)(rec, fqIngressPost(task))
				recorded := 0
				for _, f := range ev.findings {
					if f.Rule == RulePatientAnswer {
						recorded++
					}
				}
				if want := map[bool]int{true: 0, false: 1}[level == EnforcementNone]; recorded != want {
					t.Fatalf("%d %s findings, want %d", recorded, RulePatientAnswer, want)
				}
				if level != EnforcementStrict {
					if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), answer) {
						t.Fatalf("answer %d %s, want the facility's answer relayed exactly", rec.Code, rec.Body)
					}
					return
				}
				if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "facility federated query answer is about a patient other than the queried one: ") ||
					strings.Contains(rec.Body.String(), "MBR-OTHER") {
					t.Fatalf("answer %d %s, want 502 naming no patient", rec.Code, rec.Body)
				}
				wantRefusal(t, got.only(t), http.StatusBadGateway, RefusedByProviderGateway, RefusalConformance)
			})
		}
	}
}

// An answer that is not a Task fulfilled with a records Bundle is
// recorded at observe and refused at structural and strict
// (RuleAnswerShape); at none and observe it is relayed exactly.
func TestFederatedQueryIngress_AnswerThatIsNoFulfilledTask(t *testing.T) {
	answer := []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"informational"}]}`)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env := newFQExchange(t, level)
			env.payerReturns(LegResult{Response: testResponse(answer)})
			ev := observeLevelEvents(env.originator)
			var got exchangeRecords
			env.originator.cfg.ExchangeObserved = got.observe
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteFederatedQuery)(rec, fqIngressPost(fqTask(t, "MBR-UC05", "facility")))
			if want := map[bool]int{true: 1, false: 0}[level != EnforcementNone]; len(ev.findings) != want || (want == 1 && ev.findings[0].Rule != RuleAnswerShape) {
				t.Fatalf("findings %+v, want %d %s finding", ev.findings, want, RuleAnswerShape)
			}
			x := got.only(t)
			if level == EnforcementNone || level == EnforcementObserve {
				if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), answer) {
					t.Fatalf("answer %d %s, want the facility's answer relayed exactly", rec.Code, rec.Body)
				}
				return
			}
			if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "facility federated query answer is not a Task fulfilled with a records Bundle") {
				t.Fatalf("answer %d %s, want 502", rec.Code, rec.Body)
			}
			wantRefusal(t, x, http.StatusBadGateway, RefusedByProviderGateway, RefusalConformance)
		})
	}
}

// The answer is validated at this gateway's level: recorded at observe,
// refused at structural for a structural issue, and at strict for any.
func TestFederatedQueryIngress_AnswerIsValidatedAtTheLevel(t *testing.T) {
	task := fqTask(t, "MBR-UC05", "facility")
	answer := fqAnswer(t, task, fqRecords("Patient/MBR-UC05"))
	structural := &shnsdk.Issue{Severity: "error", Code: "structure", MessageID: "Extension_EXTP_Context_Wrong"}
	deeper := &shnsdk.Issue{Severity: "error", Code: "processing", MessageID: "Terminology_TX_NoValid_1_CC"}
	for _, row := range []struct {
		level   ConformanceEnforcement
		issue   *shnsdk.Issue
		refused bool
	}{
		{EnforcementNone, structural, false},
		{EnforcementObserve, structural, false},
		{EnforcementStructural, deeper, false},
		{EnforcementStructural, structural, true},
		{EnforcementStrict, deeper, true},
	} {
		t.Run(row.level.String()+"/"+row.issue.MessageID, func(t *testing.T) {
			env := newFQExchange(t, row.level)
			env.originator.cfg.Validator = &shnsdk.FakeValidator{RejectIfContains: `"output"`, RejectIssue: row.issue}
			env.payerReturns(LegResult{Response: testResponse(answer)})
			var got exchangeRecords
			env.originator.cfg.ExchangeObserved = got.observe
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteFederatedQuery)(rec, fqIngressPost(task))
			env.originator.drainObserveChecks()
			if row.refused {
				if rec.Code != http.StatusBadGateway {
					t.Fatalf("answer %d %s, want the invalid answer refused 502", rec.Code, rec.Body)
				}
				wantRefusal(t, got.only(t), http.StatusBadGateway, RefusedByProviderGateway, RefusalConformance)
			} else if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), answer) {
				t.Fatalf("answer %d %s, want the facility's answer relayed exactly", rec.Code, rec.Body)
			}
		})
	}
}

// fqAnswerOutage is a validator that cannot be reached for the facility's
// answer (a Task with output) and answers everything else.
type fqAnswerOutage struct{ *shnsdk.FakeValidator }

func (v fqAnswerOutage) Validate(ctx context.Context, b []byte, profile string) (shnsdk.Result, error) {
	if bytes.Contains(b, []byte(`"output"`)) {
		return shnsdk.Result{}, errors.New("validator down")
	}
	return v.FakeValidator.Validate(ctx, b, profile)
}

// A validator this gateway cannot reach for the facility's answer is its own
// failure: 500 where the level refuses on it (strict), never the facility's.
func TestFederatedQueryIngress_AnswerValidatorOutageIsTheGatewaysOwn(t *testing.T) {
	task := fqTask(t, "MBR-UC05", "facility")
	answer := fqAnswer(t, task, fqRecords("Patient/MBR-UC05"))
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env := newFQExchange(t, level)
			env.originator.cfg.Validator = fqAnswerOutage{shnsdk.NewFakeValidator()}
			env.payerReturns(LegResult{Response: testResponse(answer)})
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteFederatedQuery)(rec, fqIngressPost(task))
			env.originator.drainObserveChecks()
			if level == EnforcementStrict {
				if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "validator unavailable") {
					t.Fatalf("answer %d %s, want 500 validator unavailable", rec.Code, rec.Body)
				}
				return
			}
			if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), answer) {
				t.Fatalf("answer %d %s, want the facility's answer relayed", rec.Code, rec.Body)
			}
		})
	}
}

// A gateway with no validator for the answer is its own failure where the
// level refuses on it (strict): 500, never the facility's; below strict the
// answer is relayed.
func TestFederatedQueryIngress_AnswerWithNoValidatorLane(t *testing.T) {
	task := fqTask(t, "MBR-UC05", "facility")
	answer := fqAnswer(t, task, fqRecords("Patient/MBR-UC05"))
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env := newFQExchange(t, level)
			env.originator.cfg.Validator = nil
			env.payerReturns(LegResult{Response: testResponse(answer)})
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteFederatedQuery)(rec, fqIngressPost(task))
			if level == EnforcementStrict {
				if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "no FHIR validator lane configured for this leg") {
					t.Fatalf("answer %d %s, want 500 no validator lane", rec.Code, rec.Body)
				}
				return
			}
			if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), answer) {
				t.Fatalf("answer %d %s, want the facility's answer relayed", rec.Code, rec.Body)
			}
		})
	}
}
