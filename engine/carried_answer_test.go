package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// carriedAnswerLeg is one provider-ingress leg whose payer answer the gateway
// carries for its own participant's system: the request its system sends and the
// handler that serves it.
type carriedAnswerLeg struct {
	leg    string
	body   func(t *testing.T) string
	answer func(t *testing.T) []byte
	route  string
	path   string
}

func carriedAnswerLegs() []carriedAnswerLeg {
	update := func(t *testing.T) string { return string(readConformantGolden(t, "pas-update-request.json")) }
	submitAnswer := func(*testing.T) []byte { return levelPASPayerAnswer }
	return []carriedAnswerLeg{
		{"pas-claim", func(*testing.T) string { return pasIngressBundle("00001", "") }, submitAnswer, RoutePAS, "/Claim/$submit"},
		{"pas-claim-update", update, submitAnswer, RoutePAS, "/Claim/$submit"},
		{"pas-claim-inquire", func(t *testing.T) string { return levelInquiry(t, "MBR-COVERED", "MBR-COVERED") }, levelInquiryPayerAnswer, RoutePASInquire, "/Claim/$inquire"},
	}
}

// carriedAnswerRun serves leg at level with validator judging the payer's answer,
// and returns the EHR's answer, the payer answer's ingress findings and the
// exchange record.
func carriedAnswerRun(t *testing.T, leg carriedAnswerLeg, level ConformanceEnforcement, profile string, validator shnsdk.Validator) (*inProcessExchange, *httptest.ResponseRecorder, []ConformanceFinding, ExchangeRecord, []byte) {
	t.Helper()
	answer := leg.answer(t)
	env := newInProcessExchange(t)
	env.originator.cfg.ConformanceEnforcement = level
	env.originator.cfg.OriginationProfile = profile
	env.originator.cfg.Validator = validator
	env.payerReturns(LegResult{Response: testResponse(answer)})
	var mu sync.Mutex
	var findings []ConformanceFinding
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
	req := httptest.NewRequest(http.MethodPost, leg.path, strings.NewReader(leg.body(t)))
	req.Header.Set("Content-Type", "application/fhir+json")
	env.originator.ingressRoute(leg.route)(rec, req)
	env.originator.drainObserveChecks()
	mu.Lock()
	defer mu.Unlock()
	return env, rec, append([]ConformanceFinding(nil), findings...), got.only(t), answer
}

// The answer a payer gives to a PAS submit, update or inquiry that this gateway
// carries for its own participant's system is validated at the gateway's level,
// against the candidate lines: relayed unchecked at none, relayed and recorded at
// observe, refused at structural for a structural defect (a deeper one is recorded
// and relayed) and at strict. An answer that conforms to no line is therefore not
// carried silently where the participant asked for checking.
func TestPASIngress_CarriedAnswerIsValidatedAtTheLevel(t *testing.T) {
	structural := &shnsdk.Issue{Severity: "error", Code: "structure", MessageID: "Extension_EXTP_Context_Wrong"}
	deeper := &shnsdk.Issue{Severity: "error", Code: "processing", MessageID: "Terminology_TX_NoValid_1_CC"}
	for _, leg := range carriedAnswerLegs() {
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
			t.Run(leg.leg+"/"+row.level.String()+"/"+row.issue.MessageID, func(t *testing.T) {
				v := &shnsdk.FakeValidator{RejectIfContains: `"ClaimResponse"`, RejectIssue: row.issue}
				env, rec, findings, xr, answer := carriedAnswerRun(t, leg, row.level, "", v)
				if env.routeHitCount() != 1 {
					t.Fatalf("the request must be carried once, network hits %d", env.routeHitCount())
				}
				if row.refused {
					if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "ingress validation failed") {
						t.Fatalf("answer %d %s, want the invalid answer refused 502", rec.Code, rec.Body)
					}
					wantRefusal(t, xr, http.StatusBadGateway, RefusedByProviderGateway, RefusalConformance)
					if o := lastLegOutcome(t, env.originator); o != "error" {
						t.Fatalf("a refused answer's leg is recorded error, got %q", o)
					}
				} else if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), answer) {
					t.Fatalf("answer %d %s, want the payer's answer relayed exactly", rec.Code, rec.Body)
				}
				if row.level == EnforcementNone {
					if len(findings) != 0 {
						t.Fatalf("at none nothing is checked or recorded, got %+v", findings)
					}
					return
				}
				want := map[bool]string{true: "refused", false: "relayed"}[row.refused]
				if len(findings) != 1 || findings[0].Decision != want || findings[0].LegType != leg.leg ||
					findings[0].Whose != "peer" || findings[0].Seam != "provider-ingress" {
					t.Fatalf("want one %s finding on %s from the peer at the provider ingress, got %+v", want, leg.leg, findings)
				}
			})
		}
	}
}

// A conformant answer is carried exactly at every level, and recorded at no level.
func TestPASIngress_CarriedConformantAnswerIsCarriedAtEveryLevel(t *testing.T) {
	for _, leg := range carriedAnswerLegs() {
		for _, level := range allLevels {
			t.Run(leg.leg+"/"+level.String(), func(t *testing.T) {
				_, rec, findings, _, answer := carriedAnswerRun(t, leg, level, "", shnsdk.NewFakeValidator())
				if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), answer) {
					t.Fatalf("answer %d %s, want the payer's answer relayed exactly", rec.Code, rec.Body)
				}
				for _, f := range findings {
					if f.Decision != "relayed" || len(f.Issues) != 0 {
						t.Fatalf("a conformant answer records no defect, got %+v", findings)
					}
				}
			})
		}
	}
}

// The carried check skips no reference payer's answer, on an origination lane that
// relays reference-payer bytes too: unlike the answers to the legs the gateway
// originates for itself, a participant that asks its gateway to judge what it
// receives has every payer's answer judged. The submit names payer 00001, a
// reference payer identity.
func TestPASIngress_CarriedAnswerSkipsNoReferencePayer(t *testing.T) {
	if !isReferencePayer(shnsdk.PayerIdentifier{System: referencePayerSystem, Value: "00001"}) || !relaysReferencePayerBytes("demo") {
		t.Fatal("fixture: 00001 must be a reference payer and demo a lane that relays its bytes")
	}
	for _, leg := range carriedAnswerLegs() {
		t.Run(leg.leg, func(t *testing.T) {
			v := &shnsdk.FakeValidator{RejectIfContains: `"ClaimResponse"`}
			_, rec, findings, _, _ := carriedAnswerRun(t, leg, EnforcementStrict, "demo", v)
			if rec.Code != http.StatusBadGateway || len(findings) != 1 || findings[0].Decision != "refused" {
				t.Fatalf("at strict the reference payer's invalid answer is refused: %d %s %+v", rec.Code, rec.Body, findings)
			}
		})
	}
}

// A check that could not run is the gateway's own failure at strict (500), never
// the payer's; below strict the answer is relayed.
func TestPASIngress_CarriedAnswerCheckUnavailable(t *testing.T) {
	for _, leg := range carriedAnswerLegs() {
		for _, level := range []ConformanceEnforcement{EnforcementObserve, EnforcementStrict} {
			t.Run(leg.leg+"/"+level.String(), func(t *testing.T) {
				_, rec, _, _, answer := carriedAnswerRun(t, leg, level, "", &shnsdk.FakeValidator{Err: errors.New("validator down")})
				if level == EnforcementStrict {
					if rec.Code != http.StatusInternalServerError {
						t.Fatalf("answer %d %s, want the gateway's own failure 500", rec.Code, rec.Body)
					}
					return
				}
				if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), answer) {
					t.Fatalf("answer %d %s, want the payer's answer relayed exactly", rec.Code, rec.Body)
				}
			})
		}
	}
}
