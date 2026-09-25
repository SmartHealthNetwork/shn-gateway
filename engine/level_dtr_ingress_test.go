package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// Per-level rows for the provider DTR ingress ($questionnaire-package). None
// and observe refuse nothing a participant's payload is judged by; network
// rules (the subject, message integrity) and routing refuse at every level.

const dtrLeg = "dtr-questionnaire-fetch"

// levelDTRRow posts body to the provider DTR ingress at level, the payer
// declaring framed operations and answering with packageAnswer, and returns
// the exchange, the EHR's answer and the content findings recorded. enrich
// runs the ingress with the participant opted in to enrichment (E-04, E-05), with a system of
// record that derives the subject from its own Patient record.
func levelDTRRow(t *testing.T, s *prefetchSoR, level ConformanceEnforcement, enrich bool, body []byte) (*inProcessExchange, *httptest.ResponseRecorder, []ConformanceFinding) {
	t.Helper()
	env := newInProcessExchange(t)
	env.originator.cfg.SoR = s.sor()
	if enrich {
		env.originator.cfg.SoR = recordSoR{searchingPrefetchSoR{s}}
		env.originator.cfg.EnrichNativeRequests = true
	}
	env.originator.cfg.ConformanceEnforcement = level
	var findings []ConformanceFinding
	env.originator.cfg.Observer = func(e ObserverEvent) {
		var f ConformanceFinding
		if e.Kind == ConformanceObservedEvent && json.Unmarshal([]byte(e.Detail), &f) == nil && f.Kind == string(KindContent) {
			findings = append(findings, f)
		}
	}
	declareFramedDTR(t, env, true)
	env.payerReturns(LegResult{Response: testResponse(packageAnswer)})
	return env, postDTRIngress(env, body), findings
}

// wantDTRLevelOutcome asserts a content-rule row on the DTR ingress and, below
// strict, that the payer's side received the EHR's bytes as sent.
func wantDTRLevelOutcome(t *testing.T, level ConformanceEnforcement, env *inProcessExchange, rec *httptest.ResponseRecorder, findings []ConformanceFinding, body []byte, rule string, status int, msg string) {
	t.Helper()
	wantLegLevelOutcome(t, dtrLeg, level, env, rec, findings, rule, status, msg)
	if refusesAt(level, rule) {
		return
	}
	if _, sent := sentOperation(t, env); !bytes.Equal(sent, body) {
		t.Fatalf("at %s the EHR's request must be carried as sent:\n got %s\nwant %s", level, sent, body)
	}
}

// dtrOtherPatient rewrites a parameter to name the patient "other".
func dtrOtherPatient(s string) string {
	return strings.ReplaceAll(s, "Patient/"+prefetchMember, "Patient/other")
}

// a coverage or order naming no patient beside one that names the
// subject is patient mixing inside one request.
func TestLevelDTRIngress_ParameterNamesNoPatient(t *testing.T) {
	for name, row := range map[string]struct {
		body []byte
		msg  string
	}{
		"an order": {ehrParams(ehrCoverageParam(prefetchMember, "00001"),
			`{"name":"order","resource":{"resourceType":"ServiceRequest","id":"sr3","status":"active","intent":"order"}}`, dtrQuestionnaire),
			"questionnaire-package order names no patient"},
		"a coverage": {ehrParams(ehrOrderParam("sr1", prefetchMember),
			strings.Replace(ehrCoverageParam(prefetchMember, "00001"), `"beneficiary":{"reference":"Patient/`+prefetchMember+`"},`, "", 1), dtrQuestionnaire),
			"questionnaire-package coverage names no patient"},
		"a coverage naming a Group": {ehrParams(ehrOrderParam("sr1", prefetchMember),
			strings.Replace(ehrCoverageParam(prefetchMember, "00001"), "Patient/"+prefetchMember, "Group/g1", 1), dtrQuestionnaire),
			"questionnaire-package coverage names no patient"},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env, rec, findings := levelDTRRow(t, newPrefetchSoR(), level, false, row.body)
				wantDTRLevelOutcome(t, level, env, rec, findings, row.body, RulePatientMixed, http.StatusForbidden, row.msg)
			})
		}
	}
}

// two patients named by the coverages and orders of one request.
func TestLevelDTRIngress_InconsistentPatient(t *testing.T) {
	for name, extra := range map[string]string{
		"a second coverage": dtrOtherPatient(ehrCoverageParam(prefetchMember, "00001")),
		"a second order":    dtrOtherPatient(ehrOrderParam("sr2", prefetchMember)),
	} {
		body := ehrParams(ehrCoverageParam(prefetchMember, "00001"), ehrOrderParam("sr1", prefetchMember), extra)
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env, rec, findings := levelDTRRow(t, newPrefetchSoR(), level, false, body)
				wantDTRLevelOutcome(t, level, env, rec, findings, body, RulePatientMixed, http.StatusForbidden, "inconsistent patient reference in ingress payload")
			})
		}
	}
}

// Below strict a request naming two patients is carried bound to its
// subject: the patient its coverage names, whatever order the parameters
// come in.
func TestLevelDTRIngress_MixedRequestBindsTheCoverageSubject(t *testing.T) {
	body := ehrParams(dtrOtherPatient(ehrOrderParam("sr0", prefetchMember)), ehrCoverageParam(prefetchMember, "00001"), dtrQuestionnaire)
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural} {
		t.Run(level.String(), func(t *testing.T) {
			g := prefetchGateway(newPrefetchSoR())
			g.cfg.ConformanceEnforcement = level
			p, status, msg := g.prepareDTRPackageRequest(context.Background(), body)
			if status != 0 || p.member != prefetchMember || p.pci != "pci-example" {
				t.Fatalf("%d %s: bound %q (%q), want the coverage's patient", status, msg, p.member, p.pci)
			}
		})
	}
}

// the compartment fence over the resources the request carries.
func TestLevelDTRIngress_CarriedResourceFence(t *testing.T) {
	valid := []string{ehrCoverageParam(prefetchMember, "00001"), ehrOrderParam("sr1", prefetchMember), dtrQuestionnaire}
	for name, row := range map[string]struct {
		extra string
		msg   string
	}{
		"a referenced record about another patient": {`{"name":"referenced","resource":{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Observation","id":"o1","status":"final","code":{"text":"x"},"subject":{"reference":"Patient/other"}}}]}}`,
			"parameter referenced refused"},
		"another patient's record": {`{"name":"referenced","resource":{"resourceType":"Patient","id":"other"}}`, "parameter referenced refused"},
		"a record in a part about another patient": {`{"name":"x","part":[{"name":"y","resource":{"resourceType":"Condition","id":"c1","subject":{"reference":"Patient/other"}}}]}`,
			"parameter x.y refused"},
		"a resource that is not a resource": {`{"name":"referenced","resource":"text"}`, "parameter referenced refused: resource refused: not a resource"},
	} {
		body := ehrParams(append(append([]string{}, valid...), row.extra)...)
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env, rec, findings := levelDTRRow(t, newPrefetchSoR(), level, false, body)
				wantDTRLevelOutcome(t, level, env, rec, findings, body, RulePatientMixed, http.StatusForbidden, row.msg)
			})
		}
	}
}

// The Patient fill under enrichment: a system of record that cannot supply the
// patient's record refuses at strict; below strict the request is carried as
// the EHR sent it, with nothing obtained.
func TestLevelDTRIngress_PatientFillFailsCarriesAsSent(t *testing.T) {
	body := ehrParams(ehrOrderParam("sr1", prefetchMember), ehrCoverageParam(prefetchMember, "00001"), dtrQuestionnaire)
	for name, row := range map[string]struct {
		record []byte // the system's Patient read; nil: not held
		status int
		msg    string
	}{
		"not held":      {nil, http.StatusUnprocessableEntity, "patient not found in system of record"},
		"not a Patient": {[]byte(`{"resourceType":"Observation","id":"example"}`), http.StatusBadGateway, ""},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				s := newPrefetchSoR()
				delete(s.reads, "Patient/"+prefetchSoRID)
				if row.record != nil {
					s.reads["Patient/"+prefetchSoRID] = row.record
				}
				env, rec, findings := levelDTRRow(t, s, level, true, body)
				status, msg := row.status, row.msg
				if msg == "" {
					status, msg = SoRFailureResponse(&SoRReadError{Kind: SoRInvalidResponse})
				}
				wantDTRLevelOutcome(t, level, env, rec, findings, body, RulePrefetchFill, status, msg)
				if level != EnforcementNone && findings[0].Verdict != "unavailable" {
					t.Fatalf("a fill that could not complete is recorded as unavailable, got %+v", findings[0])
				}
				if level == EnforcementObserve || level == EnforcementStructural {
					wantRoutedCorrelation(t, env, findings[0])
				}
			})
		}
	}
}

// A request whose Patient fill failed and that is refused before it is
// carried (here: the payer declares no framed DTR operations) records no fill
// finding, as on the CDS Hooks ingress.
func TestLevelDTRIngress_PatientFillFindingOnlyOnceRouted(t *testing.T) {
	body := ehrParams(ehrOrderParam("sr1", prefetchMember), ehrCoverageParam(prefetchMember, "00001"), dtrQuestionnaire)
	s := newPrefetchSoR()
	delete(s.reads, "Patient/"+prefetchSoRID)
	env := newInProcessExchange(t)
	env.originator.cfg.SoR = recordSoR{searchingPrefetchSoR{s}}
	env.originator.cfg.ConformanceEnforcement = EnforcementObserve
	var findings []ConformanceFinding
	env.originator.cfg.Observer = func(e ObserverEvent) {
		var f ConformanceFinding
		if e.Kind == ConformanceObservedEvent && json.Unmarshal([]byte(e.Detail), &f) == nil && f.Kind == string(KindContent) {
			findings = append(findings, f)
		}
	}
	declareFramedDTR(t, env, false)
	rec := postDTRIngress(env, body)
	if rec.Code != http.StatusBadGateway || env.routeHitCount() != 0 {
		t.Fatalf("want the framed-operation refusal before the network, got %d %s (network hits %d)", rec.Code, rec.Body.String(), env.routeHitCount())
	}
	if len(findings) != 0 {
		t.Fatalf("a request refused before it is carried records no fill finding, got %+v", findings)
	}
}

// The Patient fill still runs below strict when the system of record can
// supply it.
func TestLevelDTRIngress_PatientFillSucceedsAtEveryLevel(t *testing.T) {
	body := ehrParams(ehrOrderParam("sr1", prefetchMember), ehrCoverageParam(prefetchMember, "00001"), dtrQuestionnaire)
	sorPatient := string(newPrefetchSoR().reads["Patient/"+prefetchSoRID])
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env, rec, findings := levelDTRRow(t, newPrefetchSoR(), level, true, body)
			if rec.Code != http.StatusOK || len(findings) != 0 {
				t.Fatalf("answer %d %s, findings %+v", rec.Code, rec.Body.String(), findings)
			}
			if _, sent := sentOperation(t, env); !bytes.Contains(sent, []byte(`{"name":"referenced","resource":`+sorPatient+`}`)) {
				t.Fatalf("the patient must be obtained from the system of record at %s, sent %s", level, sent)
			}
		})
	}
}

// The fill fence is SHN's own edit: a record of another patient read from the
// system of record is never inserted, at any level.
func TestLevelDTRIngress_PatientFillFenceRefusesAtEveryLevel(t *testing.T) {
	body := ehrParams(ehrOrderParam("sr1", prefetchMember), ehrCoverageParam(prefetchMember, "00001"), dtrQuestionnaire)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			s := newPrefetchSoR()
			s.reads["Patient/"+prefetchSoRID] = []byte(`{"resourceType":"Patient","id":"other","name":[{"family":"Other"}],"birthDate":"1960-01-01"}`)
			env, rec, _ := levelDTRRow(t, s, level, true, body)
			refusedBeforeTheNetwork(t, env, rec, http.StatusBadGateway, fillFencedOtherPatient)
		})
	}
}

// Network rules and routing refuse at every level, recording no content
// finding: a body that cannot be read one way, a request whose subject cannot
// be read or does not resolve, a coverage parameter the request cannot be
// routed by, a Coverage the system of record cannot supply for a request that
// carries none, a payer that cannot take a framed operation and a coverage
// that names no registered payer.
func TestLevelDTRIngress_NetworkRulesRefuseAtEveryLevel(t *testing.T) {
	noCoverage := ehrParams(ehrOrderParam("sr1", prefetchMember), dtrQuestionnaire)
	coverage := ehrCoverageParam(prefetchMember, "00001")
	duplicate := strings.Replace(coverage, `"resourceType":"Coverage",`, `"resourceType":"Coverage","resourceType":"Coverage",`, 1)
	if duplicate == coverage {
		t.Fatal("fixture: the member was not repeated")
	}
	for name, row := range map[string]struct {
		body    []byte
		status  int
		msg     string
		prepare func(*inProcessExchange)
	}{
		"not a Parameters":  {[]byte(`{"canonical":"http://x/q"}`), http.StatusBadRequest, "parse questionnaire-package parameters failed", nil},
		"a repeated member": {ehrParams(duplicate, dtrQuestionnaire), http.StatusBadRequest, "parse questionnaire-package parameters failed", nil},
		"a coverage without a resource": {ehrParams(ehrOrderParam("sr1", prefetchMember), `{"name":"coverage","valueReference":{"reference":"Coverage/cov-1"}}`),
			http.StatusBadRequest, "coverage parameter carries no resource", nil},
		"a coverage that is not a Coverage": {ehrParams(ehrOrderParam("sr1", prefetchMember), `{"name":"coverage","resource":{"resourceType":"Patient","id":"example"}}`),
			http.StatusBadRequest, "questionnaire-package coverage parameter is not a Coverage", nil},
		"no coverage or order names a patient": {ehrParams(`{"name":"order","resource":{"resourceType":"ServiceRequest","id":"sr3","status":"active","intent":"order"}}`,
			strings.Replace(coverage, `"beneficiary":{"reference":"Patient/`+prefetchMember+`"},`, "", 1)),
			http.StatusForbidden, "questionnaire-package order names no patient", nil},
		"no patient":                                    {ehrParams(dtrQuestionnaire), http.StatusUnprocessableEntity, "cannot bind the request to a patient", nil},
		"a patient that does not resolve":               {ehrParams(ehrCoverageParam("MBR-UNKNOWN", "00001"), dtrQuestionnaire), http.StatusForbidden, "request patient does not resolve", nil},
		"a Coverage the system of record does not hold": {noCoverage, http.StatusUnprocessableEntity, "no coverage in request or system of record", nil},
		"a payer without framed operations": {ehrParams(coverage, dtrQuestionnaire), http.StatusBadGateway, shnsdk.ErrFramedDTRUnsupported.Error(),
			func(env *inProcessExchange) { declareFramedDTR(t, env, false) }},
		"an unregistered payer": {ehrParams(ehrCoverageParam(prefetchMember, "99999"), dtrQuestionnaire), http.StatusUnprocessableEntity, "no registered payer", nil},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env := newInProcessExchange(t)
				env.originator.cfg.SoR = newPrefetchSoR().sor()
				env.originator.cfg.ConformanceEnforcement = level
				// The participant opted in to checking members, so an unknown member is refused.
				env.originator.cfg.RequireKnownMembers = true
				var findings []ConformanceFinding
				env.originator.cfg.Observer = func(e ObserverEvent) {
					var f ConformanceFinding
					if e.Kind == ConformanceObservedEvent && json.Unmarshal([]byte(e.Detail), &f) == nil && f.Kind == string(KindContent) {
						findings = append(findings, f)
					}
				}
				declareFramedDTR(t, env, true)
				if row.prepare != nil {
					row.prepare(env)
				}
				refusedBeforeTheNetwork(t, env, postDTRIngress(env, row.body), row.status, row.msg)
				if len(findings) != 0 {
					t.Fatalf("a network or routing refusal records no content finding, got %+v", findings)
				}
			})
		}
	}
}
