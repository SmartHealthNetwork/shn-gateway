package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A payer that declares its own eligibility endpoint (WithEligibilityURL,
// PAYER_ELIGIBILITY_URL) has a coverage-eligibility request carried to it and
// its answer relayed, as it would answer a direct request. Without the
// declaration the payer's gateway answers from the payer's records, as before.

const eligibilityPath = "/CoverageEligibilityRequest/$submit"

// eligibilityRequest is the requester's coverage-eligibility request for member.
func eligibilityRequest(t *testing.T, member string) []byte {
	t.Helper()
	b, err := shnsdk.BuildEligibilityRequest(member, "1234567890", fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// payersEligibilityAnswer is the payer system's own answer about patientRef, in
// its own layout (the relay must carry it byte for byte).
func payersEligibilityAnswer(patientRef string) []byte {
	return []byte(`{ "resourceType" : "CoverageEligibilityResponse", "id" : "payer-own-1", "status" : "active",
  "purpose" : [ "validation" ], "patient" : { "reference" : "` + patientRef + `" }, "created" : "2026-09-25",
  "request" : { "reference" : "CoverageEligibilityRequest/x" }, "outcome" : "complete",
  "insurer" : { "identifier" : { "system" : "urn:oid:2.16.840.1.113883.6.300", "value" : "00001" } },
  "insurance" : [ { "coverage" : { "reference" : "Coverage/payer-cov-9" }, "inforce" : true } ] }`)
}

// eligibilityLevels are the four conformance levels.
var eligibilityLevels = []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict}

// eligibilityPayer is a payer gateway at level whose native forward declares
// the payer's own eligibility endpoint (on its stub system) when declared is set.
func eligibilityPayer(t *testing.T, level ConformanceEnforcement, declared bool) *levelPayer {
	t.Helper()
	lp := newLevelPayer(t, level)
	if declared {
		WithEligibilityURL(lp.partner.srv.URL + eligibilityPath)(lp.g.cfg.Responder.(*nativeResponder))
	}
	return lp
}

func TestEligibility_UndeclaredAnswersFromThePayersRecords(t *testing.T) {
	p := eligibilityPayer(t, EnforcementObserve, false)
	got := p.send(t, "coverage-eligibility", "", eligibilityRequest(t, dtrFrameMember))
	if got.status != http.StatusOK || !bytes.Contains(got.body, []byte(`"CoverageEligibilityResponse"`)) {
		t.Fatalf("answer %d %s", got.status, got.body)
	}
	if p.partner.lastPath != "" {
		t.Fatalf("the payer's own system was called (%s) though it declared no eligibility endpoint", p.partner.lastPath)
	}
	if bytes.Contains(got.body, []byte("payer-own-1")) {
		t.Fatal("the answer is not the one this gateway builds from the payer's records")
	}
}

func TestEligibility_DeclaredForwardsAndRelaysExactly(t *testing.T) {
	for _, level := range eligibilityLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, true)
			answer := payersEligibilityAnswer("Patient/" + dtrFrameMember)
			p.partner.respByPath[eligibilityPath] = answer
			req := eligibilityRequest(t, dtrFrameMember)
			got := p.send(t, "coverage-eligibility", "", req)
			if got.status != http.StatusOK || !bytes.Equal(got.body, answer) {
				t.Fatalf("answer %d %s\nwant the payer's own bytes %s", got.status, got.body, answer)
			}
			if p.partner.lastPath != eligibilityPath || !bytes.Equal(p.partner.lastBody, req) {
				t.Fatalf("the payer's system received %s %s, want the request exactly at %s", p.partner.lastPath, p.partner.lastBody, eligibilityPath)
			}
		})
	}
}

// The payer's refusal is its own answer: relayed with its status and body,
// never replaced by one built from the payer's records.
func TestEligibility_DeclaredRelaysThePayersErrorWithoutFallback(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			p := eligibilityPayer(t, EnforcementObserve, true)
			outcome := []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing","diagnostics":"payer says no"}]}`)
			payer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/fhir+json")
				w.WriteHeader(status)
				_, _ = w.Write(outcome)
			}))
			t.Cleanup(payer.Close)
			WithEligibilityURL(payer.URL + eligibilityPath)(p.g.cfg.Responder.(*nativeResponder))
			got := p.send(t, "coverage-eligibility", "", eligibilityRequest(t, dtrFrameMember))
			if !got.framed || got.status != status || !bytes.Equal(got.body, outcome) {
				t.Fatalf("answer %d (framed %v) %s, want the payer's %d relayed", got.status, got.framed, got.body, status)
			}
		})
	}
}

// A payer system that gives no answer is not answered for: the requester is
// told, framed, whether the payer's system saw the request (as on the other
// legs a payer's own system answers), and no answer is built from the payer's
// records in its place.
func TestEligibility_DeclaredWithoutAnAnswerHasNoFallback(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := closed.URL
	closed.Close()
	dropped := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
	}))
	t.Cleanup(dropped.Close)
	for name, row := range map[string]struct{ url, want string }{
		"never reached":                      {closedURL, errUpstreamNotReached},
		"dropped after the request was read": {dropped.URL, errUpstreamNoUsableAnswer},
	} {
		t.Run(name, func(t *testing.T) {
			p := eligibilityPayer(t, EnforcementObserve, true)
			p.g.cfg.Responder.(*nativeResponder).eligibilityURL = row.url + eligibilityPath
			counter := &coverageReadCounter{censusSoR: p.store}
			p.g.cfg.SoR = counter
			got := p.send(t, "coverage-eligibility", "", eligibilityRequest(t, dtrFrameMember))
			if !got.framed || got.status != http.StatusBadGateway || !strings.Contains(string(got.body), row.want) {
				t.Fatalf("answer %d (framed %v) %s, want a framed 502 %q", got.status, got.framed, got.body, row.want)
			}
			if bytes.Contains(got.body, []byte("CoverageEligibilityResponse")) || counter.reads != 0 {
				t.Fatalf("an answer was built from the records (%d Coverage reads): %s", counter.reads, got.body)
			}
		})
	}
}

// An answer about a patient other than the request's is the payer's content:
// relayed at none, observe and structural (recorded at observe and structural),
// refused at strict.
func TestEligibility_DeclaredAnswerAboutAnotherPatientFollowsTheLevel(t *testing.T) {
	for _, level := range eligibilityLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, true)
			answer := payersEligibilityAnswer("Patient/someone-else")
			p.partner.respByPath[eligibilityPath] = answer
			got := p.send(t, "coverage-eligibility", "", eligibilityRequest(t, dtrFrameMember))
			if level == EnforcementStrict {
				if got.status != http.StatusForbidden || !strings.Contains(string(got.body), "response patient does not match request patient") {
					t.Fatalf("strict: answer %d %s, want 403", got.status, got.body)
				}
				return
			}
			if got.status != http.StatusOK || !bytes.Equal(got.body, answer) {
				t.Fatalf("%s: answer %d %s, want the payer's bytes relayed", level, got.status, got.body)
			}
			recorded := false
			for _, f := range p.content() {
				if f.Rule == string(RulePatientAnswer) {
					recorded = true
				}
			}
			if recorded != (level != EnforcementNone) {
				t.Fatalf("%s: RulePatientAnswer recorded = %v, findings %v", level, recorded, findingsText(p.content()))
			}
		})
	}
}

// The payer side binds the member its request names by its own records and
// does not refuse a token naming another patient: the request is forwarded
// and the payer's answer relayed, and the difference is observed.
func TestEligibility_DeclaredTokenSubjectDifferenceIsForwarded(t *testing.T) {
	p := eligibilityPayer(t, EnforcementStrict, true)
	var differs int
	observe := p.g.cfg.Observer
	p.g.cfg.Observer = func(e ObserverEvent) {
		if e.Kind == SubjectBindingDiffersEvent && e.LegType == "coverage-eligibility" {
			differs++
		}
		observe(e)
	}
	answer := payersEligibilityAnswer("Patient/" + dtrFrameMember)
	p.partner.respByPath[eligibilityPath] = answer
	got := p.sendAs(t, "coverage-eligibility", "", eligibilityRequest(t, dtrFrameMember), "pci-of-another-patient")
	if got.status != http.StatusOK || !bytes.Equal(got.body, answer) {
		t.Fatalf("answer %d %s, want the payer's answer relayed", got.status, got.body)
	}
	if differs != 1 {
		t.Fatalf("subject.binding-differs raised %d times, want once", differs)
	}
}

// A member the payer's records do not hold is carried to the payer's own
// system, which answers for it as it would directly.
func TestEligibility_DeclaredUnknownMemberIsCarried(t *testing.T) {
	p := eligibilityPayer(t, EnforcementStrict, true)
	answer := payersEligibilityAnswer("Patient/MBR-NOT-HELD")
	p.partner.respByPath[eligibilityPath] = answer
	got := p.sendAs(t, "coverage-eligibility", "", eligibilityRequest(t, "MBR-NOT-HELD"), shnsdk.ResolvePCI("MBR-NOT-HELD", "", ""))
	if got.status != http.StatusOK || !bytes.Equal(got.body, answer) {
		t.Fatalf("answer %d %s, want the payer's answer relayed", got.status, got.body)
	}
}

// coverageReadCounter counts the payer's own Coverage reads: the reads an
// answer built from the payer's records makes.
type coverageReadCounter struct {
	*censusSoR
	reads int
}

func (c *coverageReadCounter) CoverageInforce(key string) (bool, string) {
	c.reads++
	return c.censusSoR.CoverageInforce(key)
}

func (c *coverageReadCounter) OpenCoverage(key string) ([]byte, bool) {
	c.reads++
	return c.censusSoR.OpenCoverage(key)
}

// The payer's own endpoint answers: its Coverage records are not read for the
// verdict (they are for an undeclared payer).
func TestEligibility_DeclaredReadsNoCoverageForTheVerdict(t *testing.T) {
	for _, declared := range []bool{false, true} {
		p := eligibilityPayer(t, EnforcementObserve, declared)
		counter := &coverageReadCounter{censusSoR: p.store}
		p.g.cfg.SoR = counter
		p.partner.respByPath[eligibilityPath] = payersEligibilityAnswer("Patient/" + dtrFrameMember)
		if got := p.send(t, "coverage-eligibility", "", eligibilityRequest(t, dtrFrameMember)); got.status != http.StatusOK {
			t.Fatalf("declared=%v: answer %d %s", declared, got.status, got.body)
		}
		if (counter.reads > 0) == declared {
			t.Fatalf("declared=%v: %d Coverage reads", declared, counter.reads)
		}
	}
}

// The request is validated at the gateway's level before it is carried: at
// none it is not validated; at observe an invalid request is recorded and
// carried exactly; at structural (an issue that breaks its structure or cannot
// be classified) and strict it is refused (422) and the payer's system is not
// called.
func TestEligibility_DeclaredValidatesTheRequestFirst(t *testing.T) {
	for _, level := range eligibilityLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, true)
			p.g.cfg.Validator.(*shnsdk.FakeValidator).RejectIfContains = `"CoverageEligibilityRequest"`
			answer := payersEligibilityAnswer("Patient/" + dtrFrameMember)
			p.partner.respByPath[eligibilityPath] = answer
			req := eligibilityRequest(t, dtrFrameMember)
			got := p.send(t, "coverage-eligibility", "", req)
			ingress := 0
			for _, f := range p.findings {
				if f.Kind == string(KindFHIRIngress) && f.LegType == "coverage-eligibility" {
					ingress++
				}
			}
			switch level {
			case EnforcementStrict, EnforcementStructural:
				if got.status != http.StatusUnprocessableEntity || !strings.Contains(string(got.body), refusalIngressValidation) {
					t.Fatalf("%s: answer %d %s, want 422", level, got.status, got.body)
				}
				if p.partner.lastPath != "" {
					t.Fatalf("%s: an invalid request reached the payer's system (%s)", level, p.partner.lastPath)
				}
			default:
				if got.status != http.StatusOK || !bytes.Equal(got.body, answer) || !bytes.Equal(p.partner.lastBody, req) {
					t.Fatalf("%s: answer %d %s (payer received %s), want the request carried exactly and the answer relayed", level, got.status, got.body, p.partner.lastBody)
				}
			}
			if (ingress == 1) != (level != EnforcementNone) {
				t.Fatalf("%s: %d ingress findings", level, ingress)
			}
		})
	}
}

// eligibilityAnswerOutage is a validator that cannot be reached for the
// payer's answer and passes the request.
type eligibilityAnswerOutage struct{ *shnsdk.FakeValidator }

func (v eligibilityAnswerOutage) Validate(ctx context.Context, b []byte, profile string) (shnsdk.Result, error) {
	if bytes.Contains(b, []byte(`"CoverageEligibilityResponse"`)) {
		return shnsdk.Result{}, errors.New("validator down")
	}
	return v.FakeValidator.Validate(ctx, b, profile)
}

// A validator this gateway cannot reach for the payer's answer is this
// gateway's own fault: recorded unavailable and the answer relayed at observe
// and structural, refused with 500 at strict (as on the records path), never
// reported as the payer's refused answer.
func TestEligibility_DeclaredAnswerValidatorOutageFollowsTheLevel(t *testing.T) {
	for _, level := range eligibilityLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, true)
			p.g.cfg.Validator = eligibilityAnswerOutage{shnsdk.NewFakeValidator()}
			answer := payersEligibilityAnswer("Patient/" + dtrFrameMember)
			p.partner.respByPath[eligibilityPath] = answer
			got := p.send(t, "coverage-eligibility", "", eligibilityRequest(t, dtrFrameMember))
			if level == EnforcementStrict {
				if !got.framed || got.status != http.StatusInternalServerError || string(got.body) != `{"error":"validator unavailable"}` {
					t.Fatalf("strict: answer %d (framed %v) %s, want framed 500 validator unavailable", got.status, got.framed, got.body)
				}
				return
			}
			if got.status != http.StatusOK || !bytes.Equal(got.body, answer) {
				t.Fatalf("%s: answer %d %s, want the payer's answer relayed", level, got.status, got.body)
			}
			unavailable := 0
			for _, f := range p.findings {
				if f.Kind == string(KindFHIREgress) && f.Verdict == "unavailable" {
					unavailable++
				}
			}
			if (unavailable == 1) != (level != EnforcementNone) {
				t.Fatalf("%s: %d unavailable egress findings", level, unavailable)
			}
		})
	}
}

// The payer's answer is validated at the level: relayed at none (unchecked)
// and observe (recorded), refused at strict (502), and at structural refused
// for an issue that breaks its structure or cannot be classified (a deeper
// issue is TestEligibility_DeclaredDeeperIssuesAtStructuralAreRecorded).
func TestEligibility_DeclaredAnswerValidationFollowsTheLevel(t *testing.T) {
	for _, level := range eligibilityLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, true)
			answer := bytes.Replace(payersEligibilityAnswer("Patient/"+dtrFrameMember), []byte(`"payer-own-1"`), []byte(`"payer-own-INVALID"`), 1)
			p.partner.respByPath[eligibilityPath] = answer
			p.g.cfg.Validator.(*shnsdk.FakeValidator).RejectIfContains = "payer-own-INVALID"
			got := p.send(t, "coverage-eligibility", "", eligibilityRequest(t, dtrFrameMember))
			if level == EnforcementStrict || level == EnforcementStructural {
				if got.status != http.StatusBadGateway || !strings.Contains(string(got.body), "payer eligibility answer refused") {
					t.Fatalf("%s: answer %d %s, want 502", level, got.status, got.body)
				}
				return
			}
			if got.status != http.StatusOK || !bytes.Equal(got.body, answer) {
				t.Fatalf("%s: answer %d %s, want the payer's bytes relayed", level, got.status, got.body)
			}
			recorded := false
			for _, f := range p.findings {
				if f.Kind == string(KindFHIREgress) {
					recorded = true
				}
			}
			if recorded != (level == EnforcementObserve) {
				t.Fatalf("%s: egress finding recorded = %v", level, recorded)
			}
		})
	}
}

// An answer that cannot be read for its patient (not a
// CoverageEligibilityResponse, or one whose patient is absent, null or empty)
// is the payer's content: relayed at none and observe (recorded at observe),
// refused at structural (an answer the gateway cannot read) and strict (502).
func TestEligibility_DeclaredUnreadableAnswerFollowsTheLevel(t *testing.T) {
	own := payersEligibilityAnswer("Patient/" + dtrFrameMember)
	patient := []byte(`"patient" : { "reference" : "Patient/` + dtrFrameMember + `" }, `)
	if !bytes.Contains(own, patient) {
		t.Fatal("fixture: the answer does not name its patient as expected")
	}
	unreadable := map[string][]byte{
		"not an eligibility answer": []byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`),
		"no patient":                bytes.Replace(own, patient, nil, 1),
		"a null patient":            bytes.Replace(own, patient, []byte(`"patient" : null, `), 1),
		"an empty patient":          bytes.Replace(own, patient, []byte(`"patient" : { }, `), 1),
	}
	for name, answer := range unreadable {
		for _, level := range eligibilityLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p := eligibilityPayer(t, level, true)
				p.partner.respByPath[eligibilityPath] = answer
				got := p.send(t, "coverage-eligibility", "", eligibilityRequest(t, dtrFrameMember))
				if level == EnforcementStrict || level == EnforcementStructural {
					if got.status != http.StatusBadGateway || !strings.Contains(string(got.body), "names no readable patient") {
						t.Fatalf("%s: answer %d %s, want 502", level, got.status, got.body)
					}
				} else if got.status != http.StatusOK || !bytes.Equal(got.body, answer) {
					t.Fatalf("%s: answer %d %s, want the payer's bytes relayed", level, got.status, got.body)
				}
				var decisions []string
				for _, f := range p.content() {
					if f.Rule == string(RuleAnswerShape) {
						decisions = append(decisions, f.Decision)
					}
				}
				want := map[ConformanceEnforcement]string{EnforcementObserve: "relayed", EnforcementStructural: "refused", EnforcementStrict: "refused"}[level]
				if (want == "" && len(decisions) != 0) || (want != "" && (len(decisions) != 1 || decisions[0] != want)) {
					t.Fatalf("%s: RuleAnswerShape decisions %v, want %q", level, decisions, want)
				}
			})
		}
	}
}

// An answer that names its patient by no reference (an identifier only) is
// readable, and its patient cannot be compared with the request's: the patient
// check (RulePatientAnswer) records it at observe and structural and refuses
// it at strict (403), as on the prior-authorization answers.
func TestEligibility_DeclaredAnswerWithoutAPatientReferenceFollowsTheLevel(t *testing.T) {
	for _, level := range eligibilityLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, true)
			answer := bytes.Replace(payersEligibilityAnswer("Patient/"+dtrFrameMember),
				[]byte(`"patient" : { "reference" : "Patient/`+dtrFrameMember+`" }`),
				[]byte(`"patient" : { "identifier" : { "system" : "urn:shn:member", "value" : "`+dtrFrameMember+`" } }`), 1)
			if bytes.Contains(answer, []byte(`"reference" : "Patient/`)) {
				t.Fatal("fixture: the answer still names its patient by reference")
			}
			p.partner.respByPath[eligibilityPath] = answer
			got := p.send(t, "coverage-eligibility", "", eligibilityRequest(t, dtrFrameMember))
			if level == EnforcementStrict {
				if got.status != http.StatusForbidden || !strings.Contains(string(got.body), "names no patient by reference") {
					t.Fatalf("strict: answer %d %s, want 403", got.status, got.body)
				}
			} else if got.status != http.StatusOK || !bytes.Equal(got.body, answer) {
				t.Fatalf("%s: answer %d %s, want the payer's bytes relayed", level, got.status, got.body)
			}
			var patient, shape int
			for _, f := range p.content() {
				switch f.Rule {
				case string(RulePatientAnswer):
					patient++
				case string(RuleAnswerShape):
					shape++
				}
			}
			if shape != 0 || (patient == 1) != (level != EnforcementNone) {
				t.Fatalf("%s: %d RulePatientAnswer and %d RuleAnswerShape findings, findings %v", level, patient, shape, findingsText(p.content()))
			}
		})
	}
}

// A payer that requires known members refuses one its records do not hold
// before the request is carried, as on the prior-authorization legs.
func TestEligibility_DeclaredRequireKnownMembersRefusesUnknown(t *testing.T) {
	p := eligibilityPayer(t, EnforcementObserve, true)
	p.g.cfg.RequireKnownMembers = true
	got := p.sendAs(t, "coverage-eligibility", "", eligibilityRequest(t, "MBR-NOT-HELD"), shnsdk.ResolvePCI("MBR-NOT-HELD", "", ""))
	if got.status != http.StatusBadRequest || !strings.Contains(string(got.body), "unknown member") {
		t.Fatalf("answer %d %s, want 400 unknown member", got.status, got.body)
	}
	if p.partner.lastPath != "" {
		t.Fatalf("the request reached the payer's system (%s)", p.partner.lastPath)
	}
}

// ownIDSoR is the payer's records naming the member's Patient by the payer's
// own id, as a real payer's system may.
type ownIDSoR struct {
	*censusSoR
	own string
}

func (o ownIDSoR) PatientFHIRRef(member string) (string, bool) {
	if _, ok := o.censusSoR.PatientFHIRRef(member); !ok {
		return "", false
	}
	return "Patient/" + o.own, true
}

// At strict an answer naming the patient by the payer's own id for the member
// the request names is the same patient, recognized through the payer's own
// binding, and is relayed; one naming any other patient is refused.
func TestEligibility_DeclaredStrictRecognizesThePayersOwnPatientID(t *testing.T) {
	for _, row := range []struct {
		name, ref string
		status    int
	}{
		{"the payer's own id for the member", "Patient/payer-own-77", http.StatusOK},
		{"another patient", "Patient/someone-else", http.StatusForbidden},
	} {
		t.Run(row.name, func(t *testing.T) {
			p := eligibilityPayer(t, EnforcementStrict, true)
			p.g.cfg.SoR = ownIDSoR{censusSoR: p.store, own: "payer-own-77"}
			answer := payersEligibilityAnswer(row.ref)
			p.partner.respByPath[eligibilityPath] = answer
			got := p.send(t, "coverage-eligibility", "", eligibilityRequest(t, dtrFrameMember))
			if got.status != row.status {
				t.Fatalf("answer %d %s, want %d", got.status, got.body, row.status)
			}
			if row.status == http.StatusOK && !bytes.Equal(got.body, answer) {
				t.Fatalf("the payer's answer was not relayed exactly: %s", got.body)
			}
		})
	}
}

// passingCallCounter counts $validate calls and passes what the fake passes.
type passingCallCounter struct {
	*shnsdk.FakeValidator
	calls int
}

func (c *passingCallCounter) Validate(ctx context.Context, b []byte, profile string) (shnsdk.Result, error) {
	c.calls++
	return c.FakeValidator.Validate(ctx, b, profile)
}

// At none no conformance check runs: neither the request nor the payer's
// answer reaches a validator. observe, structural and strict validate both.
func TestEligibility_DeclaredNoneCallsNoValidator(t *testing.T) {
	for _, level := range eligibilityLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, true)
			v := &passingCallCounter{FakeValidator: shnsdk.NewFakeValidator()}
			p.g.cfg.Validator = v
			p.partner.respByPath[eligibilityPath] = payersEligibilityAnswer("Patient/" + dtrFrameMember)
			if got := p.send(t, "coverage-eligibility", "", eligibilityRequest(t, dtrFrameMember)); got.status != http.StatusOK {
				t.Fatalf("answer %d %s", got.status, got.body)
			}
			if (v.calls == 0) != (level == EnforcementNone) {
				t.Fatalf("%s: %d validator calls", level, v.calls)
			}
		})
	}
}

// When the payer answers about the member by another Patient id and the
// payer's records cannot be read to tell whether that id is the member's own,
// the patient check is unfinished and follows the level: at none the records
// are not read and the answer is relayed; at observe and structural the check
// is recorded unavailable and the answer relayed; at strict it is recorded and the
// requester is told, framed, that the system of record failed, never the read
// error's own text.
func TestEligibility_DeclaredOwnPatientReadFailureFollowsTheLevel(t *testing.T) {
	for _, level := range eligibilityLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, true)
			answer := payersEligibilityAnswer("Patient/payer-own-77")
			p.partner.respByPath[eligibilityPath] = answer
			readErr := errors.New("private-upstream-sentinel")
			var reads int
			p.g.cfg.SoR = scriptedReadSoR{t: t, base: ReadSystemOfRecord(p.store), before: func(_ context.Context, op, _ string) error {
				if op == "PatientFHIRRef" {
					reads++
					return readErr
				}
				return nil
			}}
			got := p.send(t, "coverage-eligibility", "", eligibilityRequest(t, dtrFrameMember))
			if p.partner.lastPath != eligibilityPath {
				t.Fatalf("the payer's system was not called (%q): refused before it answered", p.partner.lastPath)
			}
			var unavailable int
			for _, f := range p.content() {
				if f.Rule == string(RulePatientAnswer) && f.Verdict == "unavailable" {
					unavailable++
				}
			}
			switch level {
			case EnforcementStrict:
				status, msg := SoRFailureResponse(readErr)
				if !got.framed || got.status != status || !strings.Contains(string(got.body), msg) || bytes.Contains(got.body, []byte("sentinel")) {
					t.Fatalf("strict: answer %d (framed %v) %s, want framed %d %q", got.status, got.framed, got.body, status, msg)
				}
			default:
				if got.status != http.StatusOK || !bytes.Equal(got.body, answer) {
					t.Fatalf("%s: answer %d %s, want the payer's answer relayed", level, got.status, got.body)
				}
			}
			if level == EnforcementNone && reads != 0 {
				t.Fatalf("none: the payer's records were read %d times for a check that does not run", reads)
			}
			if wantRecorded := level != EnforcementNone; (unavailable == 1) != wantRecorded {
				t.Fatalf("%s: %d unavailable RulePatientAnswer findings, findings %v", level, unavailable, findingsText(p.content()))
			}
		})
	}
}

// An answer naming the request's own patient needs no read of the payer's
// records: a records failure then cannot refuse it, at any level.
func TestEligibility_DeclaredAnswerAboutTheRequestPatientReadsNoRecords(t *testing.T) {
	for _, level := range eligibilityLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, true)
			answer := payersEligibilityAnswer("Patient/" + dtrFrameMember)
			p.partner.respByPath[eligibilityPath] = answer
			p.g.cfg.SoR = scriptedReadSoR{t: t, base: ReadSystemOfRecord(p.store), before: func(_ context.Context, op, _ string) error {
				if op == "PatientFHIRRef" {
					return errors.New("private-upstream-sentinel")
				}
				return nil
			}}
			got := p.send(t, "coverage-eligibility", "", eligibilityRequest(t, dtrFrameMember))
			if got.status != http.StatusOK || !bytes.Equal(got.body, answer) {
				t.Fatalf("answer %d %s, want the payer's answer relayed", got.status, got.body)
			}
		})
	}
}

// builtEligibilityForwarder is a payer system whose forward returns an answer
// this gateway built rather than the payer's own.
type builtEligibilityForwarder struct {
	*nativeResponder
	built relay.Payload
}

func (b builtEligibilityForwarder) forwardEligibility(context.Context, []byte) (LegResult, error) {
	return LegResult{Response: b.built}, nil
}

// A declared endpoint's answer is only ever relayed: one this gateway built is
// refused as this gateway's own fault, framed, and observed, never sent in the
// payer's place.
func TestEligibility_DeclaredAnswerMustBeRelayed(t *testing.T) {
	p := eligibilityPayer(t, EnforcementNone, true)
	built, err := relay.Authored(relay.BuilderSDKEligibility, payersEligibilityAnswer("Patient/"+dtrFrameMember), "application/fhir+json")
	if err != nil {
		t.Fatal(err)
	}
	p.g.cfg.Responder = builtEligibilityForwarder{nativeResponder: p.g.cfg.Responder.(*nativeResponder), built: built}
	var refused int
	observe := p.g.cfg.Observer
	p.g.cfg.Observer = func(e ObserverEvent) {
		if e.Kind == relay.RefusedEvent && e.LegType == "coverage-eligibility" {
			refused++
		}
		observe(e)
	}
	got := p.send(t, "coverage-eligibility", "", eligibilityRequest(t, dtrFrameMember))
	if !got.framed || got.status != http.StatusInternalServerError || !strings.Contains(string(got.body), errOwnershipFault) {
		t.Fatalf("answer %d (framed %v) %s, want the framed ownership fault", got.status, got.framed, got.body)
	}
	if refused != 1 {
		t.Fatalf("the refusal was observed %d times, want once", refused)
	}
}

// At structural a deeper FHIR issue (a code outside its code list) in the
// request or in the payer's answer is recorded, not refused: the request is
// carried to the payer's endpoint exactly and its answer relayed exactly.
func TestEligibility_DeclaredDeeperIssuesAtStructuralAreRecorded(t *testing.T) {
	p := eligibilityPayer(t, EnforcementStructural, true)
	p.g.cfg.Validator = &shnsdk.FakeValidator{RejectIfContains: `"CoverageEligibility`,
		RejectIssue: &shnsdk.Issue{Severity: "error", Code: "processing", MessageID: "Terminology_TX_NoValid_1_CC"}}
	answer := payersEligibilityAnswer("Patient/" + dtrFrameMember)
	p.partner.respByPath[eligibilityPath] = answer
	req := eligibilityRequest(t, dtrFrameMember)
	got := p.send(t, "coverage-eligibility", "", req)
	if got.status != http.StatusOK || !bytes.Equal(got.body, answer) {
		t.Fatalf("answer %d %s, want the payer's answer relayed", got.status, got.body)
	}
	if !bytes.Equal(p.partner.lastBody, req) {
		t.Fatalf("the payer's system received %s, want the request exactly", p.partner.lastBody)
	}
	count := map[string]int{}
	for _, f := range p.findings {
		if f.LegType != "coverage-eligibility" {
			continue
		}
		if f.Level != "structural" || f.Decision != "relayed" || f.Verdict != "" || len(f.Issues) == 0 {
			t.Fatalf("want a relayed deeper finding with its issues, got %+v", f)
		}
		count[f.Kind]++
	}
	if count[string(KindFHIRIngress)] != 1 || count[string(KindFHIREgress)] != 1 || len(count) != 2 {
		t.Fatalf("want one deeper ingress and one deeper egress finding, got %v", count)
	}
}
