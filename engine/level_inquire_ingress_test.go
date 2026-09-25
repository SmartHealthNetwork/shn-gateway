package engine

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Per-level rows for the provider PAS inquiry ingress, POST /Claim/$inquire.
// None and observe refuse nothing a participant's payload is judged by;
// network rules refuse at every level.

// levelInquiry is inquiryBundle routable in the in-process harness: its
// Coverage names the payer id payerRouterFor maps to the harness's payer.
func levelInquiry(t *testing.T, member, coverageMember string) string {
	t.Helper()
	body := string(inquiryBundle(member, coverageMember, "TRN-1", "72148"))
	out := strings.Replace(body, `"resourceType":"Coverage",`, `"resourceType":"Coverage","payor":[{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}}],`, 1)
	if out == body {
		t.Fatal("fixture: the Coverage gained no payor")
	}
	return out
}

func levelInquiryWith(t *testing.T, old, new string) string {
	t.Helper()
	body := levelInquiry(t, "MBR-COVERED", "")
	out := strings.Replace(body, old, new, 1)
	if out == body {
		t.Fatalf("fixture: %q not replaced", old)
	}
	return out
}

// levelInquiryPayerAnswer is a readable answer naming one patient: the
// published 2.0 inquiry response.
func levelInquiryPayerAnswer(t *testing.T) []byte {
	t.Helper()
	return inquiryFixture(t, "pas-inquiry-response-2.0.json")
}

func levelInquireRow(t *testing.T, level ConformanceEnforcement, body string, answer []byte) (*inProcessExchange, *httptest.ResponseRecorder, *levelIngressEvents) {
	t.Helper()
	env := newInProcessExchange(t)
	env.originator.cfg.ConformanceEnforcement = level
	env.payerReturns(LegResult{Response: testResponse(answer)})
	ev := observeLevelEvents(env.originator)
	rec := httptest.NewRecorder()
	env.originator.handlePASInquireIngress(rec, httptest.NewRequest(http.MethodPost, "/Claim/$inquire", strings.NewReader(body)))
	return env, rec, ev
}

// A Bundle that is not a collection, an entry
// carrying a request, and a Claim that is not a preauthorization.
func TestLevelInquireIngress_RequestShape(t *testing.T) {
	rows := map[string]struct {
		body string
		msg  string
	}{
		"not a collection":     {levelInquiryWith(t, `"type":"collection"`, `"type":"searchset"`), "PAS inquiry Bundle is not a collection"},
		"entry request":        {levelInquiryWith(t, `{"fullUrl":"urn:uuid:patient",`, `{"fullUrl":"urn:uuid:patient","request":{"method":"GET","url":"Patient/MBR-COVERED"},`), "PAS inquiry Bundle entries carry no request or response"},
		"not preauthorization": {levelInquiryWith(t, `"use":"preauthorization"`, `"use":"claim"`), "PAS inquiry Claim is not a preauthorization"},
		// A value of the wrong type off the subject path.
		"Claim.use of the wrong type":   {levelInquiryWith(t, `"use":"preauthorization"`, `"use":5`), "parse inquiry Claim failed"},
		"Bundle.type of the wrong type": {levelInquiryWith(t, `"type":"collection"`, `"type":5`), "parse inquiry bundle failed"},
		// The id of an entry that is not a Patient.
		"Coverage id of the wrong type": {levelInquiryWith(t, `"resourceType":"Coverage",`, `"resourceType":"Coverage","id":7,`), "parse inquiry bundle entry failed"},
		"Claim id of the wrong type":    {levelInquiryWith(t, `"resourceType":"Claim",`, `"resourceType":"Claim","id":7,`), "parse inquiry bundle entry failed"},
	}
	for name, row := range rows {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env, rec, ev := levelInquireRow(t, level, row.body, levelInquiryPayerAnswer(t))
				wantLegLevelOutcome(t, "pas-claim-inquire", level, env, rec, ev.findings, RuleRequestShape, http.StatusBadRequest, row.msg)
				if level != EnforcementStrict {
					wantCarriedExactly(t, env, row.body)
				}
			})
		}
	}
}

// Another patient inside one inquiry.
func TestLevelInquireIngress_AnotherPatient(t *testing.T) {
	body := levelInquiry(t, "MBR-COVERED", "MBR-NOTCOVERED")
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env, rec, ev := levelInquireRow(t, level, body, levelInquiryPayerAnswer(t))
			wantLegLevelOutcome(t, "pas-claim-inquire", level, env, rec, ev.findings, RulePatientMixed, http.StatusForbidden, "inconsistent patient in PAS inquiry")
			if level != EnforcementStrict {
				wantCarriedExactly(t, env, body)
			}
		})
	}
}

// An answer this gateway cannot read.
func TestLevelInquireIngress_UnreadableAnswer(t *testing.T) {
	answer := []byte(`{"resourceType":"Patient","id":"x"}`)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env, rec, ev := levelInquireRow(t, level, levelInquiry(t, "MBR-COVERED", ""), answer)
			wantAnswerLevelOutcome(t, "pas-claim-inquire", level, env, rec, ev, answer, RuleAnswerShape, http.StatusBadGateway, "invalid prior-authorization inquiry answer")
		})
	}
}

// An answer naming more than one patient.
func TestLevelInquireIngress_AnswerPatientLinkage(t *testing.T) {
	answer := []byte(`{"resourceType":"Bundle","type":"collection","entry":[
		{"resource":{"resourceType":"ClaimResponse","patient":{"reference":"Patient/A"}}},
		{"resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/B"}}}]}`)
	if validatePASInquiryAnswer(answer).Status != 0 || consistentPASInquiryAnswerSubjects(answer) {
		t.Fatal("fixture: want a readable answer naming two patients")
	}
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env, rec, ev := levelInquireRow(t, level, levelInquiry(t, "MBR-COVERED", ""), answer)
			wantAnswerLevelOutcome(t, "pas-claim-inquire", level, env, rec, ev, answer, RulePatientAnswer, http.StatusBadGateway, "PAS inquiry answer has inconsistent patient linkage")
		})
	}
}

// An inquiry answer that repeats a member name is read one way only: refused
// at every level with strict's refusal, never relayed.
func TestLevelInquireIngress_AnswerDuplicateKeyRefusesAtEveryLevel(t *testing.T) {
	answer := []byte(strings.Replace(string(levelInquiryPayerAnswer(t)), `"language": "en"`, `"language": "en", "language": "en"`, 1))
	if string(answer) == string(levelInquiryPayerAnswer(t)) {
		t.Fatal("fixture: no member name repeated")
	}
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env, rec, ev := levelInquireRow(t, level, levelInquiry(t, "MBR-COVERED", ""), answer)
			wantAnswerRefusedAtEveryLevel(t, env, rec, ev, http.StatusBadGateway, "invalid prior-authorization inquiry answer")
		})
	}
}

// A readable, bound answer is recorded "ok" at every level, and nothing is
// skipped.
func TestLevelInquireIngress_ReadableAnswerRecordedOK(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			answer := levelInquiryPayerAnswer(t)
			env, rec, ev := levelInquireRow(t, level, levelInquiry(t, "MBR-COVERED", ""), answer)
			if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), answer) {
				t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
			}
			if o := lastLegOutcome(t, env.originator); o != "ok" {
				t.Fatalf("a readable answer is recorded ok, got %q", o)
			}
			if len(ev.findings) != 0 || len(ev.skipped) != 0 {
				t.Fatalf("a readable answer records nothing and skips nothing: %+v %+v", ev.findings, ev.skipped)
			}
		})
	}
}

// Network rules and routing refuse at every level, before the network, with the
// same status and body.
func TestLevelInquireIngress_NetworkRulesRefuseAtEveryLevel(t *testing.T) {
	base := levelInquiry(t, "MBR-COVERED", "")
	rows := map[string]struct {
		body   string
		status int
		msg    string
		setup  func(*testing.T, *inProcessExchange)
	}{
		"unreadable":                       {"{", http.StatusBadRequest, "parse inquiry bundle failed", nil},
		"duplicate key":                    {strings.Replace(base, `"type":"collection"`, `"type":"collection","type":"collection"`, 1), http.StatusBadRequest, "parse inquiry bundle failed", nil},
		"not a Bundle":                     {`{"resourceType":"Parameters"}`, http.StatusBadRequest, "PAS inquiry is not a Bundle", nil},
		"unreadable entry":                 {strings.Replace(base, `"resourceType":"Patient"`, `"resourceType":7`, 1), http.StatusBadRequest, "parse inquiry bundle entry failed", nil},
		"Patient id of the wrong type":     {strings.Replace(base, `"resourceType":"Patient","id":"MBR-COVERED"`, `"resourceType":"Patient","id":7`, 1), http.StatusBadRequest, "parse inquiry bundle entry failed", nil},
		"Claim.patient of the wrong type":  {strings.Replace(base, `"patient":{"reference":"Patient/MBR-COVERED"},`, `"patient":7,`, 1), http.StatusBadRequest, "parse inquiry Claim failed", nil},
		"entry resource of the wrong type": {strings.Replace(base, `"entry":[`, `"entry":[{"fullUrl":7},`, 1), http.StatusBadRequest, "parse inquiry bundle failed", nil},
		"no Claim.patient":                 {strings.Replace(base, `"patient":{"reference":"Patient/MBR-COVERED"},`, ``, 1), http.StatusBadRequest, "PAS inquiry Claim missing patient", nil},
		"unknown member":                   {strings.ReplaceAll(base, "MBR-COVERED", "MBR-NOBODY"), http.StatusBadRequest, "unknown member", nil},
		"no registered payer":              {strings.Replace(base, `"value":"00001"`, `"value":"99999"`, 1), http.StatusUnprocessableEntity, "no registered payer for identifier", nil},
		"route": {base, http.StatusUnprocessableEntity, "no shared contract line", func(t *testing.T, env *inProcessExchange) {
			declareRecipientVersions(t, env, []string{"pa.crd@2.0"})
		}},
	}
	for name, row := range rows {
		if row.body == base && row.setup == nil {
			t.Fatalf("fixture %s: nothing changed", name)
		}
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env := newInProcessExchange(t)
				env.originator.cfg.ConformanceEnforcement = level
				// The participant opted in to checking members, so an unknown member is refused.
				env.originator.cfg.RequireKnownMembers = true
				if row.setup != nil {
					row.setup(t, env)
				}
				ev := observeLevelEvents(env.originator)
				rec := httptest.NewRecorder()
				env.originator.handlePASInquireIngress(rec, httptest.NewRequest(http.MethodPost, "/Claim/$inquire", strings.NewReader(row.body)))
				refusedBeforeTheNetwork(t, env, rec, row.status, row.msg)
				if len(ev.findings) != 0 {
					t.Fatalf("a network rule records no conformance finding, got %+v", ev.findings)
				}
			})
		}
	}
}
