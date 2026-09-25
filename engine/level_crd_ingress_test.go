package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Per-level rows for the provider CRD ingress. None and observe refuse nothing
// a participant's payload is judged by; network rules refuse at every level.

var allLevels = []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict}

// refusesAt reports whether level refuses an invalid content defect of rule
// (a check that could not finish is recorded below strict, whatever the rule):
// strict
// refuses every one; structural refuses only a request or an answer the
// gateway cannot read, written out here rather than read from the
// policy so these rows state the table they check; none and observe refuse
// none.
func refusesAt(level ConformanceEnforcement, rule string) bool {
	switch level {
	case EnforcementStrict:
		return true
	case EnforcementStructural:
		return rule == RuleRequestShape || rule == RuleAnswerShape
	}
	return false
}

// wantContentFinding asserts the first content finding a row records at a
// level that runs checks: refused or relayed as refusesAt says, naming rule.
func wantContentFinding(t *testing.T, level ConformanceEnforcement, findings []ConformanceFinding, rule string) {
	t.Helper()
	want := "relayed"
	if refusesAt(level, rule) {
		want = "refused"
	}
	if len(findings) == 0 || findings[0].Rule != rule || findings[0].Decision != want {
		t.Fatalf("at %s want a %s %s finding, got %+v", level, want, rule, findings)
	}
}

// levelIngressRow runs the provider CRD ingress at level, with the
// participant's default (no enrichment), and returns the exchange, the EHR's
// answer and the content findings recorded.
func levelIngressRow(t *testing.T, s *prefetchSoR, level ConformanceEnforcement, body []byte) (*inProcessExchange, *httptest.ResponseRecorder, []ConformanceFinding) {
	t.Helper()
	return levelIngressRowWith(t, s, level, body, false)
}

// levelFillRow is levelIngressRow with the participant opted in to
// enrichment (Config.EnrichNativeRequests): the prefetch fill (E-02) runs.
func levelFillRow(t *testing.T, s *prefetchSoR, level ConformanceEnforcement, body []byte) (*inProcessExchange, *httptest.ResponseRecorder, []ConformanceFinding) {
	t.Helper()
	return levelIngressRowWith(t, s, level, body, true)
}

func levelIngressRowWith(t *testing.T, s *prefetchSoR, level ConformanceEnforcement, body []byte, enrich bool) (*inProcessExchange, *httptest.ResponseRecorder, []ConformanceFinding) {
	t.Helper()
	env := newInProcessExchange(t)
	env.originator.cfg.SoR = s.sor()
	env.originator.cfg.ConformanceEnforcement = level
	env.originator.cfg.EnrichNativeRequests = enrich
	var findings []ConformanceFinding
	env.originator.cfg.Observer = func(e ObserverEvent) {
		if e.Kind != ConformanceObservedEvent {
			return
		}
		var f ConformanceFinding
		if json.Unmarshal([]byte(e.Detail), &f) == nil && f.Kind == string(KindContent) {
			findings = append(findings, f)
		}
	}
	rec := httptest.NewRecorder()
	env.originator.handleCRDIngress(rec, crdIngressPost(body))
	return env, rec, findings
}

// wantLevelOutcome asserts a content-rule row on the provider CRD ingress
// (wantLegLevelOutcome on crd-order-select).
func wantLevelOutcome(t *testing.T, level ConformanceEnforcement, env *inProcessExchange, rec *httptest.ResponseRecorder, findings []ConformanceFinding, rule string, status int, msg string) {
	t.Helper()
	wantLegLevelOutcome(t, "crd-order-select", level, env, rec, findings, rule, status, msg)
}

// wantLegLevelOutcome asserts a content-rule row on leg: strict refuses with
// status and msg before the network and records the rule; observe carries
// the request and records the rule on leg; none carries it and records
// nothing.
func wantLegLevelOutcome(t *testing.T, leg string, level ConformanceEnforcement, env *inProcessExchange, rec *httptest.ResponseRecorder, findings []ConformanceFinding, rule string, status int, msg string) {
	t.Helper()
	if refusesAt(level, rule) {
		refusedBeforeTheNetwork(t, env, rec, status, msg)
	} else if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
		t.Fatalf("at %s the request must be carried: answer %d %s, network hits %d", level, rec.Code, rec.Body.String(), env.routeHitCount())
	}
	var got []string
	for _, f := range findings {
		got = append(got, f.Rule+"/"+f.Decision)
	}
	switch level {
	case EnforcementNone:
		if len(findings) != 0 {
			t.Fatalf("at none nothing is recorded, got %v", got)
		}
	case EnforcementObserve:
		if len(findings) == 0 || findings[0].Rule != rule || findings[0].Decision != "relayed" || findings[0].LegType != leg {
			t.Fatalf("at observe want a relayed %s finding on %s, got %+v", rule, leg, findings)
		}
	case EnforcementStructural:
		wantContentFinding(t, level, findings, rule)
		if !refusesAt(level, rule) && findings[0].LegType != leg {
			t.Fatalf("at structural want the finding on %s, got %+v", leg, findings)
		}
	case EnforcementStrict:
		if len(findings) == 0 || findings[0].Rule != rule || findings[0].Decision != "refused" {
			t.Fatalf("at strict want a refused %s finding, got %v", rule, got)
		}
	}
}

func TestLevelCRDIngress_HookMismatch(t *testing.T) {
	body := bytes.Replace(conformantCRDRequest("MBR-COVERED"), []byte(`"hook":"order-select"`), []byte(`"hook":"order-sign"`), 1)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.ConformanceEnforcement = level
			var findings []ConformanceFinding
			env.originator.cfg.Observer = func(e ObserverEvent) {
				var f ConformanceFinding
				if e.Kind == ConformanceObservedEvent && json.Unmarshal([]byte(e.Detail), &f) == nil && f.Kind == string(KindContent) {
					findings = append(findings, f)
				}
			}
			rec := ingressAt(t, env, "shn-order-select", body)
			wantLevelOutcome(t, level, env, rec, findings, RuleRequestShape, http.StatusBadRequest, "is for hook")
		})
	}
}

func TestLevelCRDIngress_KeptPrefetchForAnotherPatient(t *testing.T) {
	value := `{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"ServiceRequest","id":"x","status":"active","intent":"order","subject":{"reference":"Patient/other"}}}]}`
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env, rec, findings := levelIngressRow(t, newPrefetchSoR(), level, ehrRequest(supported+`,"serviceHistory":`+value))
			wantLevelOutcome(t, level, env, rec, findings, RulePatientMixed, http.StatusForbidden, "prefetch serviceHistory refused")
			if !refusesAt(level, RulePatientMixed) {
				if got, _ := valueOf(t, sentRequest(t, env), "prefetch", "serviceHistory"); got != value {
					t.Fatalf("the EHR's value must be carried exactly, got %s", got)
				}
			}
		})
	}
}

func TestLevelCRDIngress_KeptBinaryPrefetch(t *testing.T) {
	value := `{"resourceType":"Binary","id":"b1","contentType":"text/plain","data":"aGk="}`
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env, rec, findings := levelIngressRow(t, newPrefetchSoR(), level, ehrRequest(supported+`,"serviceHistory":`+value))
			wantLevelOutcome(t, level, env, rec, findings, RulePatientMixed, http.StatusForbidden, "prefetch serviceHistory refused")
		})
	}
}

func TestLevelCRDIngress_DraftOrderWithoutSubject(t *testing.T) {
	body := bytes.Replace(ehrRequest(supported), []byte(`"subject" : { "reference" : "Patient/example" }, `), nil, 1)
	if bytes.Equal(body, ehrRequest(supported)) {
		t.Fatal("fixture: the draft order's subject was not removed")
	}
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env, rec, findings := levelIngressRow(t, newPrefetchSoR(), level, body)
			wantLevelOutcome(t, level, env, rec, findings, RulePatientMixed, http.StatusForbidden, "draft order missing patient subject")
		})
	}
}

// The fill rows below run with the participant opted in to enrichment
// (levelFillRow); without it nothing is filled (TestLevelCRDIngress_DefaultFillsNothing).
//
// A prefetch value the system of record cannot supply (it names the patient
// by another id): strict refuses; below strict that value is left out and the
// request is carried with the EHR's own coverage (only the callback is
// removed).
func TestLevelCRDIngress_FillFailsCarriesAsSent(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			s := namedDifferently(t)
			env, rec, findings := levelFillRow(t, s, level, ehrRequest(`"coverage":`+ehrCoverage))
			wantLevelOutcome(t, level, env, rec, findings, RulePrefetchFill, http.StatusUnprocessableEntity, patientNamedDifferently)
			if refusesAt(level, RulePrefetchFill) {
				return
			}
			if level == EnforcementObserve || level == EnforcementStructural {
				wantRoutedCorrelation(t, env, findings[0])
			}
			sent := sentRequest(t, env)
			if _, ok := valueOf(t, sent, "prefetch", "patient"); ok {
				t.Fatal("nothing may be obtained when the fill failed")
			}
			if got, _ := valueOf(t, sent, "prefetch", "coverage"); got != ehrCoverage {
				t.Fatalf("the EHR's coverage must be carried exactly, got %s", got)
			}
			if _, ok := valueOf(t, sent, "fhirAuthorization"); ok {
				t.Fatal("the callback strip applies at every level")
			}
		})
	}
}

// wantRoutedCorrelation asserts a finding recorded once the request was
// routed carries the routed leg's correlation id.
func wantRoutedCorrelation(t *testing.T, env *inProcessExchange, f ConformanceFinding) {
	t.Helper()
	exs := env.originator.ExchangeSnapshot()
	if len(exs) == 0 || len(exs[len(exs)-1].Legs) == 0 {
		t.Fatal("no leg was routed")
	}
	legs := exs[len(exs)-1].Legs
	if corr := legs[len(legs)-1].CorrelationID; corr == "" || f.CorrelationID != corr {
		t.Fatalf("the fill finding must carry the routed leg's correlation id %q, got %+v", corr, f)
	}
}

// A request whose fill failed and that is refused before it is carried (here
// by routing: the payer declares no pa.crd line) records no fill finding.
func TestLevelCRDIngress_FillFindingOnlyOnceRouted(t *testing.T) {
	env := newInProcessExchange(t)
	env.originator.cfg.SoR = namedDifferently(t).sor()
	env.originator.cfg.ConformanceEnforcement = EnforcementObserve
	env.originator.cfg.EnrichNativeRequests = true
	declareRecipientVersions(t, env, []string{"pa.pas@2.0"})
	var findings []ConformanceFinding
	env.originator.cfg.Observer = func(e ObserverEvent) {
		var f ConformanceFinding
		if e.Kind == ConformanceObservedEvent && json.Unmarshal([]byte(e.Detail), &f) == nil && f.Kind == string(KindContent) {
			findings = append(findings, f)
		}
	}
	rec := httptest.NewRecorder()
	env.originator.handleCRDIngress(rec, crdIngressPost(ehrRequest(`"coverage":`+ehrCoverage)))
	if rec.Code != http.StatusUnprocessableEntity || env.routeHitCount() != 0 {
		t.Fatalf("want the routing refusal before the network, got %d %s (network hits %d)", rec.Code, rec.Body.String(), env.routeHitCount())
	}
	if len(findings) != 0 {
		t.Fatalf("a request refused before it is carried records no fill finding, got %+v", findings)
	}
}

// The patient read fails and the coverage search succeeds: strict refuses as
// it always has; below strict only the patient is left out, and the coverage
// the request is routed by is still obtained and inserted.
func TestLevelCRDIngress_PatientFillFailsCoverageStillFills(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			s := newPrefetchSoR()
			delete(s.reads, "Patient/"+prefetchSoRID)
			page := searchPage(sorCoverage("cov-1", "00001"))
			s.answer(t, "Coverage", page)
			env, rec, findings := levelFillRow(t, s, level, ehrRequest(""))
			wantLevelOutcome(t, level, env, rec, findings, RulePrefetchFill, http.StatusUnprocessableEntity, "patient not found in system of record")
			if refusesAt(level, RulePrefetchFill) {
				return
			}
			sent := sentRequest(t, env)
			if _, ok := valueOf(t, sent, "prefetch", "patient"); ok {
				t.Fatal("the patient the system of record could not supply must be left out")
			}
			if got, ok := valueOf(t, sent, "prefetch", "coverage"); !ok || got != sorAssembly(t, "Coverage", page) {
				t.Fatalf("the coverage must still be obtained and inserted, got %q", got)
			}
		})
	}
}

// The system of record cannot answer for the patient, and the request carries
// its own coverage: strict refuses with the system's failure; below strict
// nothing is obtained and the request is carried with its coverage, the
// finding recorded as unavailable at observe.
func TestLevelCRDIngress_PatientUnnamedRequestCarriesCoverage(t *testing.T) {
	sorErr := &SoRReadError{Kind: SoRUnavailable}
	wantStatus, wantMsg := SoRFailureResponse(sorErr)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			s := newPrefetchSoR()
			s.refErr = sorErr
			env, rec, findings := levelFillRow(t, s, level, ehrRequest(`"coverage":`+ehrCoverage))
			wantLevelOutcome(t, level, env, rec, findings, RulePrefetchFill, wantStatus, wantMsg)
			if (level == EnforcementObserve || level == EnforcementStructural) && findings[0].Verdict != "unavailable" {
				t.Fatalf("the check could not run: want an unavailable finding, got %+v", findings[0])
			}
			if refusesAt(level, RulePrefetchFill) {
				return
			}
			sent := sentRequest(t, env)
			if _, ok := valueOf(t, sent, "prefetch", "patient"); ok {
				t.Fatal("nothing may be obtained when the system cannot name the patient")
			}
			if got, _ := valueOf(t, sent, "prefetch", "coverage"); got != ehrCoverage {
				t.Fatalf("the EHR's coverage must be carried exactly, got %s", got)
			}
		})
	}
}

// Obtaining the coverage is routing: the request is routed by it. A request
// that carries none and for which none can be obtained is refused at every
// level with the status strict has always given, and records no finding.
func TestLevelCRDIngress_CoverageObtainFailureRefusesAtEveryLevel(t *testing.T) {
	sorErr := &SoRReadError{Kind: SoRUnavailable}
	unavailableStatus, unavailableMsg := SoRFailureResponse(sorErr)
	notObject := bytes.Replace(ehrRequest(""), []byte(`"prefetch" : {}`), []byte(`"prefetch" : []`), 1)
	if bytes.Equal(notObject, ehrRequest("")) {
		t.Fatal("fixture: prefetch not replaced")
	}
	rows := map[string]struct {
		sor    func(*testing.T) *prefetchSoR
		body   []byte
		status int
		msg    string
	}{
		"the system of record names the patient differently": {namedDifferently, ehrRequest(patientOnly), http.StatusUnprocessableEntity, patientNamedDifferently},
		"the system of record cannot answer for the patient": {func(*testing.T) *prefetchSoR {
			s := newPrefetchSoR()
			s.refErr = sorErr
			return s
		}, ehrRequest(patientOnly), unavailableStatus, unavailableMsg},
		"the coverage search fails": {func(*testing.T) *prefetchSoR {
			s := newPrefetchSoR()
			s.searches["Coverage"] = searchAnswer{err: &SearchError{Outcome: SearchUnavailable, Reason: "server error"}}
			return s
		}, ehrRequest(patientOnly), http.StatusServiceUnavailable, "coverage unavailable from system of record"},
		// The prefetch is read with the subject: it carries the coverage.
		"prefetch is not an object": {func(*testing.T) *prefetchSoR { return newPrefetchSoR() }, notObject, http.StatusBadRequest, "parse cds request failed"},
	}
	for name, row := range rows {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env, rec, findings := levelFillRow(t, row.sor(t), level, row.body)
				refusedBeforeTheNetwork(t, env, rec, row.status, row.msg)
				if len(findings) != 0 {
					t.Fatalf("a routing refusal records no content finding, got %+v", findings)
				}
			})
		}
	}
}

// The fill still runs below strict when the system of record can supply it.
func TestLevelCRDIngress_FillSucceedsAtEveryLevel(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env, rec, _ := levelFillRow(t, newPrefetchSoR(), level, ehrRequest(`"coverage":`+ehrCoverage))
			if rec.Code != http.StatusOK {
				t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
			}
			if got, ok := valueOf(t, sentRequest(t, env), "prefetch", "patient"); !ok || !strings.Contains(got, `"id" : "example"`) {
				t.Fatalf("the patient must be obtained from the system of record at %s, got %q", level, got)
			}
		})
	}
}

// The fill fence is SHN's own edit: a record of another patient read from the
// system of record is never inserted, at any level.
func TestLevelCRDIngress_FillFenceRefusesAtEveryLevel(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			s := newPrefetchSoR()
			s.answer(t, "ServiceRequest", searchPage(sorRequest("h1", "Patient/"+prefetchSoRID), sorRequest("h2", "Patient/other")))
			env, rec, _ := levelFillRow(t, s, level, ehrRequest(supported))
			refusedBeforeTheNetwork(t, env, rec, http.StatusBadGateway, fillFencedOtherPatient)
		})
	}
}

// crdIngressWithFindings posts body to the provider CRD ingress at level and
// returns the exchange, the answer and the content findings recorded.
func crdIngressWithFindings(t *testing.T, level ConformanceEnforcement, body []byte) (*inProcessExchange, *httptest.ResponseRecorder, []ConformanceFinding) {
	t.Helper()
	env := newInProcessExchange(t)
	env.originator.cfg.ConformanceEnforcement = level
	var findings []ConformanceFinding
	env.originator.cfg.Observer = func(e ObserverEvent) {
		var f ConformanceFinding
		if e.Kind == ConformanceObservedEvent && json.Unmarshal([]byte(e.Detail), &f) == nil && f.Kind == string(KindContent) {
			findings = append(findings, f)
		}
	}
	return env, ingressAt(t, env, "shn-order-select", body), findings
}

// A value of the wrong type off the subject and routing path is the request's
// own shape: strict refuses as it always has; below strict the request is
// carried with the value as sent, recorded once at observe.
func TestLevelCRDIngress_ValueOfWrongTypeOffTheSubjectPath(t *testing.T) {
	base := conformantCRDRequest("MBR-COVERED")
	for name, row := range map[string]struct{ old, new string }{
		"hookInstance": {`"hookInstance":"hi-1"`, `"hookInstance":7`},
		"hook":         {`"hook":"order-select"`, `"hook":7`},
		"userId":       {`"userId":"Practitioner/p1"`, `"userId":7`},
	} {
		body := bytes.Replace(base, []byte(row.old), []byte(row.new), 1)
		if bytes.Equal(body, base) {
			t.Fatalf("fixture %s: nothing replaced", name)
		}
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env, rec, findings := crdIngressWithFindings(t, level, body)
				wantLevelOutcome(t, level, env, rec, findings, RuleRequestShape, http.StatusBadRequest, "parse cds request failed")
				if (level == EnforcementObserve || level == EnforcementStructural) && len(findings) != 1 {
					t.Fatalf("one defect is recorded once, got %+v", findings)
				}
				if !refusesAt(level, RuleRequestShape) && !bytes.Contains(sentRequest(t, env), []byte(row.new)) {
					t.Fatalf("the value must be carried as sent:\n%s", sentRequest(t, env))
				}
			})
		}
	}
}

// Network rules refuse at every level: a subject that does not resolve, and a
// body with a repeated member name.
func TestLevelCRDIngress_NetworkRulesRefuseAtEveryLevel(t *testing.T) {
	unknown := bytes.ReplaceAll(conformantCRDRequest("MBR-COVERED"), []byte("MBR-COVERED"), []byte("MBR-NOBODY"))
	duplicate := bytes.Replace(conformantCRDRequest("MBR-COVERED"), []byte(`"hook":"order-select",`), []byte(`"hook":"order-select","hook":"order-select",`), 1)
	for _, level := range allLevels {
		for name, row := range map[string]struct {
			body   []byte
			status int
			msg    string
		}{
			"unknown member, known members required": {unknown, http.StatusBadRequest, "unknown member"},
			"duplicate key":                          {duplicate, http.StatusBadRequest, "parse cds request failed"},
			"not JSON":                               {[]byte(`{"hook":`), http.StatusBadRequest, "parse cds request failed"},
			"subject of the wrong type": {bytes.Replace(conformantCRDRequest("MBR-COVERED"), []byte(`"patientId":"MBR-COVERED"`), []byte(`"patientId":7`), 1),
				http.StatusBadRequest, "parse cds request failed"},
			"context of the wrong type": {[]byte(`{"hook":"order-select","hookInstance":"hi-1","context":[],"prefetch":{}}`), http.StatusBadRequest, "parse cds request failed"},
		} {
			t.Run(level.String()+"/"+name, func(t *testing.T) {
				env := newInProcessExchange(t)
				env.originator.cfg.ConformanceEnforcement = level
				// The participant opted in to checking members, so an unknown member is refused.
				env.originator.cfg.RequireKnownMembers = true
				rec := ingressAt(t, env, "shn-order-select", row.body)
				refusedBeforeTheNetwork(t, env, rec, row.status, row.msg)
			})
		}
	}
}
