package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// Per-level rows for the provider PAS ingress, POST /Claim/$submit. None and
// observe refuse nothing a participant's payload is judged by; network rules
// refuse at every level.

// levelPASPayerAnswer is a readable, bound PAS answer: the reference payer's
// pended submit response.
var levelPASPayerAnswer = []byte(assemblyRealPending)

// levelIngressEvents collects what a level row observes: the content findings
// and the local-write-skipped events.
type levelIngressEvents struct {
	findings []ConformanceFinding
	skipped  []ObserverEvent
}

func observeLevelEvents(g *Gateway) *levelIngressEvents {
	ev := &levelIngressEvents{}
	g.cfg.Observer = func(e ObserverEvent) {
		switch e.Kind {
		case ConformanceObservedEvent:
			var f ConformanceFinding
			if json.Unmarshal([]byte(e.Detail), &f) == nil && f.Kind == string(KindContent) {
				ev.findings = append(ev.findings, f)
			}
		case LocalWriteSkippedEvent:
			ev.skipped = append(ev.skipped, e)
		}
	}
	return ev
}

// levelPASRow runs the provider PAS ingress at level with the payer answering
// answer, and returns the exchange, the EHR's answer and what was observed.
func levelPASRow(t *testing.T, level ConformanceEnforcement, body string, answer []byte) (*inProcessExchange, *httptest.ResponseRecorder, *levelIngressEvents) {
	t.Helper()
	env := newInProcessExchange(t)
	env.originator.cfg.ConformanceEnforcement = level
	env.payerReturns(LegResult{Response: testResponse(answer)})
	ev := observeLevelEvents(env.originator)
	rec := httptest.NewRecorder()
	env.originator.handlePASIngress(rec, httptest.NewRequest(http.MethodPost, "/Claim/$submit", strings.NewReader(body)))
	return env, rec, ev
}

// lastLegOutcome is the outcome the exchange store recorded for the last leg.
func lastLegOutcome(t *testing.T, g *Gateway) string {
	t.Helper()
	exs := g.ExchangeSnapshot()
	if len(exs) == 0 || len(exs[len(exs)-1].Legs) == 0 {
		t.Fatal("no exchange leg was recorded")
	}
	legs := exs[len(exs)-1].Legs
	return legs[len(legs)-1].Outcome
}

// wantCarriedExactly asserts the participant's request reached the payer's
// side byte for byte.
func wantCarriedExactly(t *testing.T, env *inProcessExchange, body string) {
	t.Helper()
	if got := sentRequest(t, env); !bytes.Equal(got, []byte(body)) {
		t.Fatalf("the request must be carried exactly as sent:\n got %s\nwant %s", got, body)
	}
}

// wantAnswerLevelOutcome asserts an answer-rule row: strict refuses with
// status and msg after the network, records the rule and records the leg as
// an error; below strict the payer's bytes are relayed exactly, the leg is
// recorded "answered" and the local-write-skipped event names the rule;
// observe records a relayed finding on leg, none records nothing.
func wantAnswerLevelOutcome(t *testing.T, leg string, level ConformanceEnforcement, env *inProcessExchange, rec *httptest.ResponseRecorder, ev *levelIngressEvents, answer []byte, rule string, status int, msg string) {
	t.Helper()
	if env.routeHitCount() != 1 {
		t.Fatalf("the request must be carried once, network hits %d", env.routeHitCount())
	}
	var got []string
	for _, f := range ev.findings {
		got = append(got, f.Rule+"/"+f.Decision)
	}
	if level == EnforcementStrict {
		if rec.Code != status || !strings.Contains(rec.Body.String(), msg) {
			t.Fatalf("answer %d %s, want %d %q", rec.Code, rec.Body.String(), status, msg)
		}
		if len(ev.findings) == 0 || ev.findings[0].Rule != rule || ev.findings[0].Decision != "refused" {
			t.Fatalf("at strict want a refused %s finding, got %v", rule, got)
		}
		if o := lastLegOutcome(t, env.originator); o != "error" {
			t.Fatalf("at strict the leg is recorded error, got %q", o)
		}
		if len(ev.skipped) != 0 {
			t.Fatalf("a refused answer skips nothing, got %+v", ev.skipped)
		}
		return
	}
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), answer) {
		t.Fatalf("at %s the payer's answer must be relayed exactly: %d %s", level, rec.Code, rec.Body.String())
	}
	switch level {
	case EnforcementNone:
		if len(ev.findings) != 0 {
			t.Fatalf("at none nothing is recorded, got %v", got)
		}
	case EnforcementObserve:
		if len(ev.findings) != 1 || ev.findings[0].Rule != rule || ev.findings[0].Decision != "relayed" || ev.findings[0].LegType != leg || ev.findings[0].Whose != "peer" {
			t.Fatalf("at observe want one relayed %s finding on %s from the peer, got %+v", rule, leg, ev.findings)
		}
	}
	if o := lastLegOutcome(t, env.originator); o != "answered" {
		t.Fatalf("an answer relayed unread is recorded answered, got %q", o)
	}
	if len(ev.skipped) != 1 || ev.skipped[0].LegType != leg || !strings.Contains(ev.skipped[0].Detail, rule) || len(ev.skipped[0].Payload) != 0 {
		t.Fatalf("want one %s event on %s naming %s and carrying no payload, got %+v", LocalWriteSkippedEvent, leg, rule, ev.skipped)
	}
}

func levelPASBundleWithout(t *testing.T, remove string) string {
	t.Helper()
	body := pasIngressBundle("00001", "")
	out := strings.Replace(body, remove, "", 1)
	if out == body {
		t.Fatalf("fixture: %q not removed", remove)
	}
	return out
}

func levelPASBundleWithEntry(entry string) string {
	return strings.Replace(pasIngressBundle("00001", ""), `"entry":[`, `"entry":[`+entry+`,`, 1)
}

// An amendment (a Claim carrying related[]) whose Provenance holds a value of
// the wrong type. Below strict the value is read past and the amendment
// routes the update leg; at strict the facts cannot be read and it routes
// pas-claim, as it always has. The payer's gateway judges the update, so the
// provider ingress records nothing for it.
func TestLevelPASIngress_AmendmentWithWrongTypedContentPicksTheUpdateLeg(t *testing.T) {
	body := levelPASBundleWithEntry(`{"resource":{"resourceType":"Provenance","target":[{"reference":"QuestionnaireResponse/qr1"}],"agent":[{"who":{"reference":"Practitioner/p1"}}],"policy":"x"}}`)
	body = strings.Replace(body, `{"resourceType":"Claim",`, `{"resourceType":"Claim","related":[{"claim":{"identifier":{"value":"PRIOR-1"}}}],`, 1)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env, _, ev := levelPASRow(t, level, body, levelPASPayerAnswer)
			exs := env.originator.ExchangeSnapshot()
			if len(exs) == 0 || len(exs[len(exs)-1].Legs) == 0 {
				t.Fatal("no leg was routed")
			}
			legs := exs[len(exs)-1].Legs
			want := "pas-claim-update"
			if level == EnforcementStrict {
				want = "pas-claim"
			}
			if got := legs[len(legs)-1].Type; got != want {
				t.Fatalf("at %s the amendment routes %s, got %s", level, want, got)
			}
			for _, f := range ev.findings {
				if f.Rule == RuleRequestShape {
					t.Fatalf("the provider ingress records nothing for the update's content, got %+v", ev.findings)
				}
			}
			wantCarriedExactly(t, env, body)
		})
	}
}

// No order.
func TestLevelPASIngress_MissingOrder(t *testing.T) {
	body := levelPASBundleWithout(t, `{"resource":{"resourceType":"ServiceRequest","subject":{"reference":"Patient/MBR-COVERED"}}},`)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env, rec, ev := levelPASRow(t, level, body, levelPASPayerAnswer)
			wantLegLevelOutcome(t, "pas-claim", level, env, rec, ev.findings, RuleRequestShape, http.StatusBadRequest, "PAS bundle missing order (ServiceRequest or DeviceRequest)")
			if level != EnforcementStrict {
				wantCarriedExactly(t, env, body)
			}
		})
	}
}

// A Coverage with no beneficiary. The subject is Claim.patient and routing
// reads Coverage.payor, so the beneficiary is the request's own shape.
func TestLevelPASIngress_CoverageWithoutBeneficiary(t *testing.T) {
	body := levelPASBundleWithout(t, `"beneficiary":{"reference":"Patient/MBR-COVERED"},`)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env, rec, ev := levelPASRow(t, level, body, levelPASPayerAnswer)
			wantLegLevelOutcome(t, "pas-claim", level, env, rec, ev.findings, RuleRequestShape, http.StatusBadRequest, "PAS bundle missing Coverage.beneficiary")
			if level != EnforcementStrict {
				wantCarriedExactly(t, env, body)
			}
		})
	}
}

// An order, Coverage, QuestionnaireResponse or DiagnosticReport naming
// another patient.
func TestLevelPASIngress_AnotherPatient(t *testing.T) {
	rows := map[string]string{
		"order":    strings.Replace(pasIngressBundle("00001", ""), `"ServiceRequest","subject":{"reference":"Patient/MBR-COVERED"}`, `"ServiceRequest","subject":{"reference":"Patient/MBR-NOTCOVERED"}`, 1),
		"coverage": strings.Replace(pasIngressBundle("00001", ""), `"beneficiary":{"reference":"Patient/MBR-COVERED"}`, `"beneficiary":{"reference":"Patient/MBR-NOTCOVERED"}`, 1),
		"qr":       levelPASBundleWithEntry(`{"resource":{"resourceType":"QuestionnaireResponse","status":"completed","subject":{"reference":"Patient/MBR-NOTCOVERED"}}}`),
		"dr":       levelPASBundleWithEntry(`{"resource":{"resourceType":"DiagnosticReport","status":"final","subject":{"reference":"Patient/MBR-NOTCOVERED"}}}`),
	}
	for name, body := range rows {
		if body == pasIngressBundle("00001", "") {
			t.Fatalf("fixture %s: nothing changed", name)
		}
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env, rec, ev := levelPASRow(t, level, body, levelPASPayerAnswer)
				wantLegLevelOutcome(t, "pas-claim", level, env, rec, ev.findings, RulePatientMixed, http.StatusForbidden, "inconsistent patient in PAS bundle")
				if level != EnforcementStrict {
					wantCarriedExactly(t, env, body)
				}
			})
		}
	}
}

// A QuestionnaireResponse naming no patient beside the bound subject.
func TestLevelPASIngress_QRWithoutSubject(t *testing.T) {
	body := levelPASBundleWithEntry(`{"resource":{"resourceType":"QuestionnaireResponse","status":"completed"}}`)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env, rec, ev := levelPASRow(t, level, body, levelPASPayerAnswer)
			wantLegLevelOutcome(t, "pas-claim", level, env, rec, ev.findings, RulePatientMixed, http.StatusForbidden, "PAS bundle QuestionnaireResponse missing subject")
			if level != EnforcementStrict {
				wantCarriedExactly(t, env, body)
			}
		})
	}
}

// A clinician-sourced QR item without its FR-16/FR-17 attestation.
func TestLevelPASIngress_UnattestedItem(t *testing.T) {
	item, err := shnsdk.BuildManualAttestedItem("functional-status-oswestry", "42", shnsdk.Attestation{NPI: "1999999999", Text: "I attest these are my clinical findings.", When: "2026-06-04"})
	if err != nil {
		t.Fatal(err)
	}
	body := levelPASBundleWithEntry(`{"resource":{"resourceType":"QuestionnaireResponse","subject":{"reference":"Patient/MBR-COVERED"},"item":[` + string(stripItemExtension(t, item)) + `]}}`)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env, rec, ev := levelPASRow(t, level, body, levelPASPayerAnswer)
			wantLegLevelOutcome(t, "pas-claim", level, env, rec, ev.findings, RuleAttestation, http.StatusForbidden, "is clinician-sourced (FR-17)")
			if level != EnforcementStrict {
				wantCarriedExactly(t, env, body)
			}
		})
	}
}

// An answer graph this gateway cannot read, and a decision it cannot read.
func TestLevelPASIngress_UnreadableAnswer(t *testing.T) {
	rows := map[string]struct {
		answer []byte
		msg    string
	}{
		"graph":    {[]byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`), "invalid native PAS response Bundle"},
		"decision": {[]byte(strings.Replace(assemblyRealPending, `"outcome":"queued"`, `"outcome":7`, 1)), "invalid native PAS response decision"},
	}
	for name, row := range rows {
		if _, bad := validateNativePASResponse(row.answer); bad.Status == 0 || !strings.Contains(bad.Message, row.msg) {
			t.Fatalf("fixture %s: want %q, got %+v", name, row.msg, bad)
		}
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env, rec, ev := levelPASRow(t, level, pasIngressBundle("00001", ""), row.answer)
				wantAnswerLevelOutcome(t, "pas-claim", level, env, rec, ev, row.answer, RuleAnswerShape, http.StatusBadGateway, row.msg)
			})
		}
	}
}

// An answer whose subjects do not bind to its ClaimResponse's patient.
func TestLevelPASIngress_AnswerPatientLinkage(t *testing.T) {
	answer := []byte(strings.Replace(assemblyRealPending, `"entry":[`, `"entry":[{"fullUrl":"http://localhost:8081/fhir/Patient/Other","resource":{"resourceType":"Patient","id":"Other"}},`, 1))
	if _, bad := validateNativePASResponse(answer); bad.Status != 0 {
		t.Fatalf("fixture: the answer must be readable, got %+v", bad)
	}
	if pasResponseSubjectMismatch(answer) == nil {
		t.Fatal("fixture: the answer's subjects must not bind")
	}
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env, rec, ev := levelPASRow(t, level, pasIngressBundle("00001", ""), answer)
			wantAnswerLevelOutcome(t, "pas-claim", level, env, rec, ev, answer, RulePatientAnswer, http.StatusBadGateway, "PAS response has inconsistent patient linkage")
		})
	}
}

// wantAnswerRefusedAtEveryLevel asserts a network-rule answer row: the
// request was carried once, and at every level the answer is refused with
// status and msg (strict's refusal), the leg is recorded as an error, and no
// finding or local-write skip is recorded.
func wantAnswerRefusedAtEveryLevel(t *testing.T, env *inProcessExchange, rec *httptest.ResponseRecorder, ev *levelIngressEvents, status int, msg string) {
	t.Helper()
	if env.routeHitCount() != 1 {
		t.Fatalf("the request must be carried once, network hits %d", env.routeHitCount())
	}
	if rec.Code != status || !strings.Contains(rec.Body.String(), msg) {
		t.Fatalf("answer %d %s, want %d %q", rec.Code, rec.Body.String(), status, msg)
	}
	if o := lastLegOutcome(t, env.originator); o != "error" {
		t.Fatalf("a refused answer is recorded error, got %q", o)
	}
	if len(ev.findings) != 0 || len(ev.skipped) != 0 {
		t.Fatalf("a network rule records no finding and skips nothing: %+v %+v", ev.findings, ev.skipped)
	}
}

// A payer answer that repeats a member name is read one way only: refused at
// every level with strict's refusal, never relayed.
func TestLevelPASIngress_AnswerDuplicateKeyRefusesAtEveryLevel(t *testing.T) {
	answer := []byte(strings.Replace(assemblyRealPending, `"outcome":"queued"`, `"outcome":"queued","outcome":"queued"`, 1))
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env, rec, ev := levelPASRow(t, level, pasIngressBundle("00001", ""), answer)
			wantAnswerRefusedAtEveryLevel(t, env, rec, ev, http.StatusBadGateway, "invalid native PAS response Bundle")
		})
	}
}

// A readable, bound answer is recorded from its decision at every level, and
// nothing is skipped.
func TestLevelPASIngress_ReadableAnswerRecordsItsDecision(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env, rec, ev := levelPASRow(t, level, pasIngressBundle("00001", ""), levelPASPayerAnswer)
			if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), levelPASPayerAnswer) {
				t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
			}
			if o := lastLegOutcome(t, env.originator); o != "pended" {
				t.Fatalf("the leg outcome is read from the decision, got %q", o)
			}
			if len(ev.findings) != 0 || len(ev.skipped) != 0 {
				t.Fatalf("a readable answer records nothing and skips nothing: %+v %+v", ev.findings, ev.skipped)
			}
		})
	}
}

// Network rules and routing refuse at every level, before the network, with the
// same status and body.
func TestLevelPASIngress_NetworkRulesRefuseAtEveryLevel(t *testing.T) {
	base := pasIngressBundle("00001", "")
	rows := map[string]struct {
		body   string
		status int
		msg    string
		setup  func(*testing.T, *inProcessExchange)
	}{
		"unreadable":          {"{", http.StatusBadRequest, "parse claim bundle failed", nil},
		"duplicate key":       {strings.Replace(base, `"type":"collection"`, `"type":"collection","type":"collection"`, 1), http.StatusBadRequest, "parse claim bundle failed", nil},
		"not a Bundle":        {`{"resourceType":"Parameters"}`, http.StatusBadRequest, "PAS request is not a Bundle", nil},
		"unreadable entry":    {strings.Replace(base, `"resourceType":"Patient","id":"MBR-COVERED"`, `"resourceType":7,"id":"MBR-COVERED"`, 1), http.StatusBadRequest, "parse bundle entry failed", nil},
		"no Claim.patient":    {strings.Replace(base, `"resourceType":"Claim","patient":{"reference":"Patient/MBR-COVERED"}`, `"resourceType":"Claim"`, 1), http.StatusBadRequest, "PAS bundle missing Claim.patient", nil},
		"no Coverage":         {levelPASBundleWithout(t, `,{"resource":{"resourceType":"Coverage","id":"cov1","beneficiary":{"reference":"Patient/MBR-COVERED"},"payor":[{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}}]}}`), http.StatusBadRequest, "PAS bundle missing Coverage.beneficiary", nil},
		"unknown member":      {strings.ReplaceAll(base, "MBR-COVERED", "MBR-NOBODY"), http.StatusBadRequest, "unknown member", nil},
		"no registered payer": {pasIngressBundle("99999", ""), http.StatusUnprocessableEntity, "no registered payer for identifier", nil},
		"route": {base, http.StatusUnprocessableEntity, "no shared contract line", func(t *testing.T, env *inProcessExchange) {
			declareRecipientVersions(t, env, []string{"pa.crd@2.0"})
		}},
	}
	for name, row := range rows {
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
				env.originator.handlePASIngress(rec, httptest.NewRequest(http.MethodPost, "/Claim/$submit", strings.NewReader(row.body)))
				refusedBeforeTheNetwork(t, env, rec, row.status, row.msg)
				if len(ev.findings) != 0 {
					t.Fatalf("a network rule records no conformance finding, got %+v", ev.findings)
				}
			})
		}
	}
}
