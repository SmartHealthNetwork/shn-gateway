package engine

// eligibility_ingress_test.go — a participant's own system sends a coverage
// eligibility request through its gateway: POST
// /CoverageEligibilityRequest/$submit, the base R4 operation (IHE FAIS
// ITI-124). The request is carried to the payer exactly as sent, routed by
// the payer it names, and the payer's answer is relayed exactly.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

const eligibilitySubmitPath = "/CoverageEligibilityRequest/$submit"

// eligibilityAsParameters is the operation's Parameters form: its one
// `resource` parameter holding the request.
func eligibilityAsParameters(t *testing.T, cer []byte) []byte {
	t.Helper()
	return []byte(`{"resourceType":"Parameters","parameter":[{"name":"resource","resource":` + string(cer) + `}]}`)
}

// eligibilityWith is the eligibility request for member with fields set or
// replaced (nil removes one).
func eligibilityWith(t *testing.T, member string, fields map[string]any) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(eligibilityRequest(t, member), &doc); err != nil {
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

// eligibilityPayorIdentifier is a payer identifier in the CMS payer system.
func eligibilityPayorIdentifier(value string) map[string]any {
	return map[string]any{"system": shnsdk.CMSPayerIdentity.System, "value": value}
}

// eligibilityCoverage is a contained Coverage of the member with id and one
// payor.
func eligibilityCoverage(id string, payor map[string]any) map[string]any {
	return map[string]any{"resourceType": "Coverage", "id": id, "status": "active",
		"beneficiary": map[string]any{"reference": "Patient/MBR-COVERED"}, "payor": []any{payor}}
}

// eligibilityCoverageIdentified is cov with identifier as its business
// identifier.
func eligibilityCoverageIdentified(cov, identifier map[string]any) map[string]any {
	out := map[string]any{"identifier": []any{identifier}}
	for k, v := range cov {
		out[k] = v
	}
	return out
}

// eligibilityByCoverages is the request for MBR-COVERED whose insurer names
// no payer, containing contained and naming refs in insurance.
func eligibilityByCoverages(t *testing.T, contained []any, refs ...string) []byte {
	t.Helper()
	var insurance []any
	for _, ref := range refs {
		insurance = append(insurance, map[string]any{"coverage": map[string]any{"reference": ref}})
	}
	return eligibilityWith(t, "MBR-COVERED", map[string]any{"insurer": map[string]any{"display": "the plan"},
		"contained": contained, "insurance": insurance})
}

// eligibilityIngressPost is the EHR's POST of body to the operation.
func eligibilityIngressPost(body []byte) *http.Request {
	req := httptest.NewRequest(http.MethodPost, eligibilitySubmitPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/fhir+json")
	return req
}

// runEligibilityIngress sends body through the provider's eligibility route
// at level with the payer answering answer, and returns the exchange, the
// caller's answer and the one exchange record.
func runEligibilityIngress(t *testing.T, level ConformanceEnforcement, body, answer []byte) (*inProcessExchange, *httptest.ResponseRecorder, ExchangeRecord) {
	t.Helper()
	env := newInProcessExchange(t)
	env.originator.cfg.ConformanceEnforcement = level
	env.payerReturns(LegResult{Response: testResponse(answer)})
	var got exchangeRecords
	env.originator.cfg.ExchangeObserved = got.observe
	rec := httptest.NewRecorder()
	env.originator.ingressRoute(RouteEligibility)(rec, eligibilityIngressPost(body))
	return env, rec, got.only(t)
}

// Both standard forms of the operation are carried to the payer exactly as
// sent, routed by the payer the request names, and the payer's answer is
// relayed exactly, at every level. The Parameters form is read as the request
// it holds: routed to the same payer as the resource form.
func TestEligibilityIngress_CarriesEachFormAsSent(t *testing.T) {
	cer := eligibilityRequest(t, "MBR-COVERED")
	answer := payersEligibilityAnswer("Patient/MBR-COVERED")
	for name, body := range map[string][]byte{"resource": cer, "parameters": eligibilityAsParameters(t, cer)} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env, rec, x := runEligibilityIngress(t, level, body, answer)
				if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), answer) {
					t.Fatalf("answer %d %s, want the payer's answer relayed exactly", rec.Code, rec.Body)
				}
				if sent := env.lastRequestPayload(); !bytes.Equal(sent, body) {
					t.Fatalf("the payer's side received\n%s\nwant the request as sent\n%s", sent, body)
				}
				if x.Outcome != ExchangeAnswered || x.Route != RouteEligibility || x.Exchange != "coverage-eligibility" ||
					x.Recipient != env.payerID || len(x.Edits) != 0 {
					t.Fatalf("record %+v, want an answered eligibility exchange with the payer, carried unedited", x)
				}
			})
		}
	}
}

// The request is routed by its insurer: an identifier, or an Organization it
// contains; failing that, by the payor of a Coverage it contains.
func TestEligibilityIngress_RoutesByThePayerTheRequestNames(t *testing.T) {
	cms := eligibilityPayorIdentifier(shnsdk.CMSPayerIdentity.Value)
	org := map[string]any{"resourceType": "Organization", "id": "ins", "identifier": []any{cms}}
	cov := eligibilityCoverage("cov", map[string]any{"identifier": cms})
	unregistered := eligibilityCoverage("other", map[string]any{"identifier": eligibilityPayorIdentifier("99999")})
	answer := payersEligibilityAnswer("Patient/MBR-COVERED")
	for name, body := range map[string][]byte{
		"insurer identifier":         eligibilityRequest(t, "MBR-COVERED"),
		"contained insurer":          eligibilityWith(t, "MBR-COVERED", map[string]any{"insurer": map[string]any{"reference": "#ins"}, "contained": []any{org}}),
		"contained Coverage's payor": eligibilityByCoverages(t, []any{cov}, "#cov"),
		// The payor is an Organization the request contains: the Coverage is
		// contained in the request too, so the reference resolves there.
		"contained Coverage's contained payor": eligibilityByCoverages(t, []any{eligibilityCoverage("cov", map[string]any{"reference": "#ins"}), org}, "#cov"),
		"coverages naming one payer":           eligibilityByCoverages(t, []any{cov, eligibilityCoverage("cov2", map[string]any{"reference": "#ins"}), org}, "#cov", "#cov2"),
		// An insurer naming no payer identifier the request carries (a
		// reference to a resource it does not contain) is routed by its
		// coverages.
		"insurer not resolved, coverage routes": eligibilityWith(t, "MBR-COVERED", map[string]any{"insurer": map[string]any{"reference": "Organization/elsewhere"},
			"contained": []any{cov}, "insurance": []any{map[string]any{"coverage": map[string]any{"reference": "#cov"}}}}),
		// A Coverage the request contains is read for its payor alone, as
		// before: members it should not carry (contained in contained) do
		// not keep it from naming its payer.
		"contained Coverage carrying contained": eligibilityByCoverages(t, []any{func() map[string]any {
			c := eligibilityCoverage("cov", map[string]any{"identifier": cms})
			c["contained"] = "x"
			return c
		}()}, "#cov"),
		// The insurer is the request's recipient: a coverage naming another
		// payer does not route it elsewhere.
		"insurer over its coverages": eligibilityWith(t, "MBR-COVERED", map[string]any{"contained": []any{unregistered},
			"insurance": []any{map[string]any{"coverage": map[string]any{"reference": "#other"}}}}),
	} {
		t.Run(name, func(t *testing.T) {
			env, rec, x := runEligibilityIngress(t, EnforcementObserve, body, answer)
			if rec.Code != http.StatusOK || env.lastCorrelation() == "" || x.Recipient != env.payerID {
				t.Fatalf("answer %d %s (recipient %q), want routed to the payer and answered", rec.Code, rec.Body, x.Recipient)
			}
		})
	}
}

// A request whose payer cannot be resolved, or is not registered, is refused
// as routing before anything is sent, at every level, and says what is
// missing.
func TestEligibilityIngress_UnroutableRequestIsRefused(t *testing.T) {
	cms := eligibilityPayorIdentifier(shnsdk.CMSPayerIdentity.Value)
	org := func(identifier string) map[string]any {
		return map[string]any{"resourceType": "Organization", "id": "ins", identifier: []any{cms}}
	}
	insurer := func(ref string, contained ...any) []byte {
		return eligibilityWith(t, "MBR-COVERED", map[string]any{"insurer": map[string]any{"reference": ref}, "contained": contained})
	}
	cov := eligibilityCoverage("cov", map[string]any{"identifier": cms})
	for name, row := range map[string]struct {
		body []byte
		msg  string
	}{
		"no payer named":       {eligibilityWith(t, "MBR-COVERED", map[string]any{"insurer": map[string]any{"display": "the plan"}}), eligibilityNoPayer},
		"insurer not resolved": {insurer("Organization/elsewhere"), eligibilityInsurerUnresolved + remedyRelativeInsurer + `: insurer \"Organization/elsewhere\"`},
		"payer not registered": {eligibilityWith(t, "MBR-COVERED", map[string]any{"insurer": map[string]any{"identifier": eligibilityPayorIdentifier("99999")}}), "no registered payer for identifier " + shnsdk.CMSPayerIdentity.System + "|99999"},
		// A reference resolves to one contained resource of the type it names.
		// A Coverage's own business identifier is not a payer identifier.
		"insurer names a contained Coverage": {insurer("#cov", eligibilityCoverageIdentified(cov, cms)), eligibilityNoPayer},
		"contained id shared":                {insurer("#ins", org("identifier"), org("identifier")), `ambiguous reference for routing: the eligibility request contains more than one resource with id \"ins\"`},
		"coverage id shared":                 {eligibilityByCoverages(t, []any{cov, cov}, "#cov"), "ambiguous reference for routing"},
		// Member names are read exactly, as a FHIR reader reads them.
		"identifier named in another case": {insurer("#ins", org("Identifier")), eligibilityNoPayer},
		"id named in another case":         {insurer("#ins", map[string]any{"resourceType": "Organization", "ID": "ins", "identifier": []any{cms}}), eligibilityInsurerUnresolved + remedyContainedInsurer + `: insurer \"#ins\"`},
		"payor named in another case": {eligibilityByCoverages(t, []any{map[string]any{"resourceType": "Coverage", "id": "cov",
			"Payor": []any{map[string]any{"identifier": cms}}}}, "#cov"), eligibilityCoverageNoPayer},
		"coverage is a contained Bundle": {eligibilityByCoverages(t, []any{map[string]any{"resourceType": "Bundle", "id": "cov", "type": "collection",
			"entry": []any{map[string]any{"resource": cov}}}}, "#cov"), eligibilityCoverageNoPayer},
		"payor outside the request": {eligibilityByCoverages(t, []any{eligibilityCoverage("cov", map[string]any{"reference": "Organization/elsewhere"})}, "#cov"),
			eligibilityPayorUnresolved + remedyRelativePayor + `: insurance[0].coverage \"#cov\", payor \"Organization/elsewhere\"`},
		"coverage outside the request": {eligibilityWith(t, "MBR-COVERED", map[string]any{"insurer": map[string]any{"display": "the plan"},
			"insurance": []any{map[string]any{"coverage": map[string]any{"reference": "Coverage/elsewhere"}}}}), eligibilityCoverageUnresolved + remedyRelativeCoverage + `: insurance[0].coverage \"Coverage/elsewhere\"`},
		"insurer identifier with no value":  {eligibilityWith(t, "MBR-COVERED", map[string]any{"insurer": map[string]any{"identifier": map[string]any{"system": shnsdk.CMSPayerIdentity.System}}}), eligibilityNoPayer},
		"insurer identifier with no system": {eligibilityWith(t, "MBR-COVERED", map[string]any{"insurer": map[string]any{"identifier": map[string]any{"value": "00001"}}}), eligibilityNoPayer},
		"payor id shared": {eligibilityByCoverages(t, []any{eligibilityCoverage("cov", map[string]any{"reference": "#ins"}), org("identifier"), org("identifier")}, "#cov"),
			`ambiguous reference for routing: the eligibility request contains more than one resource with id \"ins\"`},
		// Every coverage the request names must name a payer: one that names
		// none is not passed over for another that does.
		"a coverage naming no payer beside one that does": {eligibilityByCoverages(t, []any{eligibilityCoverage("covA", map[string]any{"reference": "Organization/elsewhere"}), cov}, "#covA", "#cov"),
			eligibilityPayorUnresolved + remedyRelativePayor + `: insurance[0].coverage \"#covA\", payor \"Organization/elsewhere\"`},
		// One leg, one recipient.
		"coverages naming two payers": {eligibilityByCoverages(t, []any{cov,
			eligibilityCoverage("cov2", map[string]any{"identifier": eligibilityPayorIdentifier("99999")})}, "#cov", "#cov2"), eligibilityTwoPayers},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env, rec, x := runEligibilityIngress(t, level, row.body, nil)
				if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), row.msg) {
					t.Fatalf("answer %d %s, want 422 %q", rec.Code, rec.Body, row.msg)
				}
				if env.routeHitCount() != 0 {
					t.Fatalf("a refused request crossed the network (%d)", env.routeHitCount())
				}
				wantRefusal(t, x, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalRouting)
			})
		}
	}
}

// A body that is not the operation's input, an input that does not read one
// way only, or a request that names no patient, is refused before anything
// is sent, at every level.
func TestEligibilityIngress_RefusesWhatIsNoEligibilityRequest(t *testing.T) {
	cer := eligibilityRequest(t, "MBR-COVERED")
	param := func(members string) []byte {
		return []byte(`{"resourceType":"Parameters","parameter":[` + members + `]}`)
	}
	const unreadInput = "parse eligibility request failed: "
	const unreadRouting = `"parse eligibility request failed"`
	for name, row := range map[string]struct {
		body []byte
		msg  string
	}{
		"not JSON":                    {[]byte("{"), unreadInput},
		"another resource":            {[]byte(`{"resourceType":"Claim"}`), unreadInput + "expected a CoverageEligibilityRequest"},
		"Parameters with no resource": {param(`{"name":"other","valueString":"x"}`), unreadInput + "the Parameters holds no resource parameter"},
		"Parameters with two":         {param(`{"name":"resource","resource":` + string(cer) + `},{"name":"resource","resource":` + string(cer) + `}`), unreadInput + "the Parameters holds more than one resource parameter"},
		"resource parameter holding a value": {param(`{"name":"resource","valueString":"x"},{"name":"resource","resource":` + string(cer) + `}`),
			unreadInput + "the resource parameter holds no resource"},
		"Parameters holding a Claim": {eligibilityAsParameters(t, []byte(`{"resourceType":"Claim"}`)), unreadInput + "the resource parameter holds no CoverageEligibilityRequest"},
		// The input is read one way only: a repeated member, or a member named
		// only in another case, is not read as either.
		"resource member repeated":                    {param(`{"name":"resource","resource":` + string(cer) + `,"resource":` + string(cer) + `}`), `eligibility request repeats the JSON member \"resource\" in parameter[0]: it can be read two ways, so it is not read`},
		"resource member in another case":             {param(`{"name":"resource","Resource":` + string(cer) + `}`), `eligibility request names \"Resource\" for \"resource\": member names are case-sensitive, so the request names no patient to bind`},
		"another parameter's name in another case":    {param(`{"Name":"other","valueString":"x"},{"name":"resource","resource":` + string(cer) + `}`), `eligibility request names \"Name\" for \"name\": member names are case-sensitive, so the request names no patient to bind`},
		"insurer member repeated":                     {bytes.Replace(cer, []byte(`"insurer":`), []byte(`"insurer":{"display":"x"},"insurer":`), 1), `eligibility request repeats the JSON member \"insurer\" in the top-level object: it can be read two ways, so it is not read`},
		"insurer member in another case":              {bytes.Replace(cer, []byte(`"insurer":`), []byte(`"Insurer":`), 1), unreadRouting},
		"request's resourceType in another case":      {bytes.Replace(cer, []byte(`"resourceType":"CoverageEligibilityRequest"`), []byte(`"RESOURCETYPE":"CoverageEligibilityRequest"`), 1), `eligibility request names \"RESOURCETYPE\" for \"resourceType\": member names are case-sensitive, so the request names no patient to bind`},
		"input's resourceType in another case":        {bytes.Replace(eligibilityAsParameters(t, cer), []byte(`"resourceType":"Parameters"`), []byte(`"RESOURCETYPE":"Parameters"`), 1), `eligibility request names \"RESOURCETYPE\" for \"resourceType\": member names are case-sensitive, so the request names no patient to bind`},
		"held request's resourceType in another case": {eligibilityAsParameters(t, bytes.Replace(cer, []byte(`"resourceType":"CoverageEligibilityRequest"`), []byte(`"RESOURCETYPE":"CoverageEligibilityRequest"`), 1)), `eligibility request names \"RESOURCETYPE\" for \"resourceType\": member names are case-sensitive, so the request names no patient to bind`},
		"no patient": {eligibilityWith(t, "MBR-COVERED", map[string]any{"patient": nil}), "parse member failed"},
		// A Bundle submits a request only in one of the operation's forms.
		"Bundle with no request":                      {[]byte(`{"resourceType":"Bundle","type":"collection"}`), unreadInput + "the Bundle holds no CoverageEligibilityRequest"},
		"Parameters holding a Bundle with no request": {eligibilityAsParameters(t, []byte(`{"resourceType":"Bundle","type":"collection"}`)), unreadInput + "the Bundle holds no CoverageEligibilityRequest"},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env, rec, _ := runEligibilityIngress(t, level, row.body, nil)
				refusedBeforeTheNetwork(t, env, rec, http.StatusBadRequest, row.msg)
			})
		}
	}
}

// A gateway with no payer router routes no request: refused as routing, at
// every level.
func TestEligibilityIngress_NoPayerRouterIsRefused(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.ConformanceEnforcement = level
			env.originator.cfg.PayerRouter = nil
			var got exchangeRecords
			env.originator.cfg.ExchangeObserved = got.observe
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteEligibility)(rec, eligibilityIngressPost(eligibilityRequest(t, "MBR-COVERED")))
			refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, "no payer router configured")
			wantRefusal(t, got.only(t), http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalRouting)
		})
	}
}

// The request's patient is bound as on the other native legs: a member this
// participant's own system does not hold is carried, bound from the request,
// unless the participant opted in to checking members; a system that cannot
// answer is its own failure. At every level.
func TestEligibilityIngress_BindsThePatientAsTheOtherLegs(t *testing.T) {
	unknown := eligibilityRequest(t, "MBR-NOBODY")
	answer := payersEligibilityAnswer("Patient/MBR-NOBODY")
	sorErr := &SoRReadError{Kind: SoRUnavailable}
	sorStatus, sorMsg := SoRFailureResponse(sorErr)
	for _, level := range allLevels {
		t.Run("unknown member carried/"+level.String(), func(t *testing.T) {
			env, rec, _ := runEligibilityIngress(t, level, unknown, answer)
			if rec.Code != http.StatusOK || !bytes.Equal(env.lastRequestPayload(), unknown) {
				t.Fatalf("answer %d %s, want a member the system does not hold carried as sent", rec.Code, rec.Body)
			}
		})
		t.Run("unknown member checked/"+level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.ConformanceEnforcement = level
			env.originator.cfg.RequireKnownMembers = true
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteEligibility)(rec, eligibilityIngressPost(unknown))
			refusedBeforeTheNetwork(t, env, rec, http.StatusBadRequest, "unknown member")
		})
		t.Run("system of record fails/"+level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.ConformanceEnforcement = level
			env.originator.cfg.SoR = failingSoR{SystemOfRecord: env.originator.cfg.SoR, err: sorErr}
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteEligibility)(rec, eligibilityIngressPost(eligibilityRequest(t, "MBR-COVERED")))
			refusedBeforeTheNetwork(t, env, rec, sorStatus, sorMsg)
		})
	}
}

// The payer's error answer is relayed as the payer sent it, at every level.
func TestEligibilityIngress_RelaysThePayersError(t *testing.T) {
	oo := []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"business-rule"}]}`)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.ConformanceEnforcement = level
			env.payerReturns(LegResult{Status: http.StatusUnprocessableEntity, Response: testResponse(oo)})
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteEligibility)(rec, eligibilityIngressPost(eligibilityRequest(t, "MBR-COVERED")))
			if rec.Code != http.StatusUnprocessableEntity || !bytes.Equal(rec.Body.Bytes(), oo) {
				t.Fatalf("answer %d %s, want the payer's 422 relayed exactly", rec.Code, rec.Body)
			}
		})
	}
}

// A payer answer that repeats a member name can be read two ways: refused at
// every level, recorded as integrity, and not relayed.
func TestEligibilityIngress_AnswerRepeatingAMemberIsRefused(t *testing.T) {
	answer := bytes.Replace(payersEligibilityAnswer("Patient/MBR-COVERED"), []byte(`"status" : "active"`), []byte(`"status" : "active", "status" : "cancelled"`), 1)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			_, rec, x := runEligibilityIngress(t, level, eligibilityRequest(t, "MBR-COVERED"), answer)
			if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), `payer eligibility answer repeats the JSON member \"status\"`) {
				t.Fatalf("answer %d %s, want 502 naming the repeated member", rec.Code, rec.Body)
			}
			wantRefusal(t, x, http.StatusBadGateway, RefusedByProviderGateway, RefusalIntegrity)
		})
	}
}

// The payer's answer is validated at this gateway's level, as the answer to
// an eligibility request it builds itself is: relayed unchecked at none,
// relayed and recorded at observe, refused at structural for a structural
// issue (a deeper one is recorded and relayed) and at strict.
func TestEligibilityIngress_AnswerIsValidatedAtTheLevel(t *testing.T) {
	answer := payersEligibilityAnswer("Patient/MBR-COVERED")
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
			env := newInProcessExchange(t)
			env.originator.cfg.ConformanceEnforcement = row.level
			env.originator.cfg.Validator = &shnsdk.FakeValidator{RejectIfContains: `"CoverageEligibilityResponse"`, RejectIssue: row.issue}
			env.payerReturns(LegResult{Response: testResponse(answer)})
			var findings []ConformanceFinding
			var mu sync.Mutex
			env.originator.cfg.Observer = func(e ObserverEvent) {
				var f ConformanceFinding
				if e.Kind == ConformanceObservedEvent && json.Unmarshal([]byte(e.Detail), &f) == nil && f.Kind == string(KindFHIRIngress) {
					mu.Lock()
					findings = append(findings, f)
					mu.Unlock()
				}
			}
			var got exchangeRecords
			env.originator.cfg.ExchangeObserved = got.observe
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteEligibility)(rec, eligibilityIngressPost(eligibilityRequest(t, "MBR-COVERED")))
			env.originator.drainObserveChecks()
			if row.refused {
				if rec.Code != http.StatusBadGateway {
					t.Fatalf("answer %d %s, want the invalid answer refused 502", rec.Code, rec.Body)
				}
				wantRefusal(t, got.only(t), http.StatusBadGateway, RefusedByProviderGateway, RefusalConformance)
			} else if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), answer) {
				t.Fatalf("answer %d %s, want the payer's answer relayed exactly", rec.Code, rec.Body)
			}
			mu.Lock()
			defer mu.Unlock()
			if want := map[bool]int{true: 0, false: 1}[row.level == EnforcementNone]; len(findings) != want {
				t.Fatalf("%d answer findings, want %d", len(findings), want)
			}
		})
	}
}

// A 2xx answer that is not a CoverageEligibilityResponse naming its patient
// is relayed at none and observe (recorded at observe), and refused at
// structural and strict, as a payer's gateway treats its own system's answer.
func TestEligibilityIngress_AnswerThatIsNoEligibilityResponse(t *testing.T) {
	answer := []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"informational"}]}`)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.ConformanceEnforcement = level
			env.payerReturns(LegResult{Response: testResponse(answer)})
			ev := observeLevelEvents(env.originator)
			var got exchangeRecords
			env.originator.cfg.ExchangeObserved = got.observe
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteEligibility)(rec, eligibilityIngressPost(eligibilityRequest(t, "MBR-COVERED")))
			if want := map[bool]int{true: 1, false: 0}[level != EnforcementNone]; len(ev.findings) != want || (want == 1 && ev.findings[0].Rule != RuleAnswerShape) {
				t.Fatalf("findings %+v, want %d %s finding", ev.findings, want, RuleAnswerShape)
			}
			x := got.only(t)
			if level == EnforcementNone || level == EnforcementObserve {
				if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), answer) {
					t.Fatalf("answer %d %s, want the payer's answer relayed exactly", rec.Code, rec.Body)
				}
				return
			}
			if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "payer eligibility answer is not a CoverageEligibilityResponse naming its patient") {
				t.Fatalf("answer %d %s, want 502", rec.Code, rec.Body)
			}
			wantRefusal(t, x, http.StatusBadGateway, RefusedByProviderGateway, RefusalConformance)
		})
	}
}

// An answer naming its patient by identifier only is not refused for its
// shape at any level: which patient it names is the payer's gateway's check.
func TestEligibilityIngress_AnswerNamingItsPatientByIdentifierIsRelayed(t *testing.T) {
	answer := bytes.Replace(payersEligibilityAnswer("Patient/MBR-COVERED"), []byte(`"patient" : { "reference" : "Patient/MBR-COVERED" }`),
		[]byte(`"patient" : { "identifier" : { "system" : "`+shnsdk.MemberSystem+`", "value" : "MBR-COVERED" } }`), 1)
	if bytes.Contains(answer, []byte(`"reference" : "Patient/`)) {
		t.Fatal("the fixture names its patient otherwise")
	}
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			_, rec, _ := runEligibilityIngress(t, level, eligibilityRequest(t, "MBR-COVERED"), answer)
			if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), answer) {
				t.Fatalf("answer %d %s, want the payer's answer relayed exactly", rec.Code, rec.Body)
			}
		})
	}
}

// A validator this gateway cannot reach for the payer's answer is its own
// failure: refused 500 at strict, never as the payer's answer; below strict
// the answer is relayed.
func TestEligibilityIngress_AnswerValidatorOutageIsTheGatewaysOwn(t *testing.T) {
	answer := payersEligibilityAnswer("Patient/MBR-COVERED")
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.ConformanceEnforcement = level
			env.originator.cfg.Validator = eligibilityAnswerOutage{shnsdk.NewFakeValidator()}
			env.payerReturns(LegResult{Response: testResponse(answer)})
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteEligibility)(rec, eligibilityIngressPost(eligibilityRequest(t, "MBR-COVERED")))
			env.originator.drainObserveChecks()
			if level == EnforcementStrict {
				if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "validator unavailable") {
					t.Fatalf("answer %d %s, want 500 validator unavailable", rec.Code, rec.Body)
				}
				return
			}
			if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), answer) {
				t.Fatalf("answer %d %s, want the payer's answer relayed", rec.Code, rec.Body)
			}
		})
	}
}

// The Authorization Framework decides authority per leg: a denial is
// refused, recorded as authority, at every level.
func TestEligibilityIngress_AuthorityDeniedIsRefused(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.ConformanceEnforcement = level
			env.substrate.authorizeFaults = []func(*http.Request) (*http.Response, error){answer(http.StatusForbidden, `{"error":"denied"}`)}
			var got exchangeRecords
			env.originator.cfg.ExchangeObserved = got.observe
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteEligibility)(rec, eligibilityIngressPost(eligibilityRequest(t, "MBR-COVERED")))
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "authorization denied") || env.routeHitCount() != 0 {
				t.Fatalf("answer %d %s (network hits %d), want 403 authorization denied before the network", rec.Code, rec.Body, env.routeHitCount())
			}
			wantRefusal(t, got.only(t), http.StatusForbidden, RefusedByAuthorizationFramework, RefusalAuthority)
		})
	}
}

// The payer side reads the Parameters form as the request it holds: answered
// from the payer's records about its member, carried byte for byte to a
// payer's own endpoint, and a token for another patient carried and observed
// (one patient-match rule), at every level.
func TestEligibility_ParametersFormIsReadAsItsRequest(t *testing.T) {
	body := eligibilityAsParameters(t, eligibilityRequest(t, dtrFrameMember))
	for _, level := range eligibilityLevels {
		t.Run("records/"+level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, false)
			other, _, _ := p.store.ResolvePatient(anotherHeldMember)
			run := runObserved(t, p, "coverage-eligibility", body, other)
			if ref, err := ParseCoverageEligibilityResponsePatient(run.answer.body); run.answer.status != http.StatusOK || err != nil || ref != "Patient/"+dtrFrameMember {
				t.Fatalf("answer %d %s, want answered about the request's member", run.answer.status, run.answer.body)
			}
			if run.differs != 1 {
				t.Fatalf("%d binding-differs events, want the token's other patient observed once", run.differs)
			}
		})
		t.Run("no request/"+level.String(), func(t *testing.T) {
			for _, declared := range []bool{false, true} {
				p := eligibilityPayer(t, level, declared)
				ans := p.send(t, "coverage-eligibility", "", []byte(`{"resourceType":"Parameters","parameter":[]}`))
				if ans.status != http.StatusBadRequest || !strings.Contains(string(ans.body), "parse member failed") || p.partner.calls.Load() != 0 {
					t.Fatalf("declared %v: answer %d %s (payer system calls %d), want 400 parse member failed before the payer's system", declared, ans.status, ans.body, p.partner.calls.Load())
				}
			}
		})
		// A payer binds a member it does not hold from the request the input
		// holds, as the requester's gateway bound it: a Patient elsewhere in
		// the Parameters is not the request's.
		t.Run("forward binds from the request/"+level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, true)
			p.partner.respByPath[eligibilityPath] = payersEligibilityAnswer("Patient/MBR-NOBODY")
			cer := eligibilityRequest(t, "MBR-NOBODY")
			demo, _ := carriedPatientDemographics(cer, "MBR-NOBODY")
			requesters := derivedPCI("MBR-NOBODY", demo.BirthDate, demo.FamilyName)
			input := []byte(`{"resourceType":"Parameters","parameter":[{"name":"resource","resource":` + string(cer) +
				`},{"name":"note","resource":{"resourceType":"Patient","id":"MBR-NOBODY","birthDate":"1970-01-01","name":[{"family":"Elsewhere"}]}}]}`)
			run := runObserved(t, p, "coverage-eligibility", input, requesters)
			if run.answer.status != http.StatusOK || run.differs != 0 {
				t.Fatalf("answer %d %s, %d binding-differs events; want answered with the requester's own binding", run.answer.status, run.answer.body, run.differs)
			}
		})
		t.Run("forward/"+level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, true)
			answer := payersEligibilityAnswer("Patient/" + dtrFrameMember)
			p.partner.respByPath[eligibilityPath] = answer
			ans := p.send(t, "coverage-eligibility", "", body)
			if ans.status != http.StatusOK || !bytes.Equal(ans.body, answer) || !bytes.Equal(p.partner.lastBody, body) {
				t.Fatalf("answer %d %s; the payer's endpoint received %s; want the Parameters carried as sent", ans.status, ans.body, p.partner.lastBody)
			}
		})
	}
}
