//go:build !shn_coreonly

package engine

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A peer's relayed success answer is written to the participant's system with
// the media type the peer's system stated, exactly, whatever the type, as a
// direct client of that system would read it. An answer whose frame
// states none is written application/fhir+json, the type the protocol reads
// an unstated one as. These rows drive each route's ingress end to end, the
// peer's gateway framing its system's 2xx answer with the stated type, at
// every conformance level: the media type is no check a level turns on.

// The CDex submission's answer keeps the payer's stated type.
func TestCDexIngress_WritesThePayersStatedAnswerType(t *testing.T) {
	t.Parallel()
	for _, row := range statedAnswerTypes {
		for _, level := range allLevels {
			t.Run(statedTypeName(row.stated)+"/"+level.String(), func(t *testing.T) {
				_, rec, x := runCDexIngress(t, level, cdexSubmissionBody("MBR-COVERED", cdexPayerCMS), "",
					framedAnswer(cdexPayersAnswer, row.stated, http.StatusOK))
				wantWritten(t, rec, http.StatusOK, row.written, cdexPayersAnswer)
				if x.Outcome != ExchangeAnswered {
					t.Fatalf("recorded %s, want answered", x.Outcome)
				}
			})
		}
	}
}

// The CDex data request's answer, at the payer's gateway, keeps the
// provider's stated type.
func TestCDexRequestIngress_WritesTheProvidersStatedAnswerType(t *testing.T) {
	t.Parallel()
	for _, row := range statedAnswerTypes {
		for _, level := range allLevels {
			t.Run(statedTypeName(row.stated)+"/"+level.String(), func(t *testing.T) {
				_, rec, x := runCDexRequestIngress(t, level, cdexRequestTask(t),
					framedAnswer(cdexRequestAnswer, row.stated, http.StatusCreated))
				wantWritten(t, rec, http.StatusCreated, row.written, cdexRequestAnswer)
				if x.Outcome != ExchangeAnswered {
					t.Fatalf("recorded %s, want answered", x.Outcome)
				}
			})
		}
	}
}

// The coverage-eligibility answer keeps the payer's stated type.
func TestEligibilityIngress_WritesThePayersStatedAnswerType(t *testing.T) {
	t.Parallel()
	body := eligibilityRequest(t, "MBR-COVERED")
	answer := payersEligibilityAnswer("Patient/MBR-COVERED")
	for _, row := range statedAnswerTypes {
		for _, level := range allLevels {
			t.Run(statedTypeName(row.stated)+"/"+level.String(), func(t *testing.T) {
				env := newInProcessExchange(t)
				env.originator.cfg.ConformanceEnforcement = level
				env.payerReturns(framedAnswer(answer, row.stated, http.StatusOK))
				rec := httptest.NewRecorder()
				env.originator.ingressRoute(RouteEligibility)(rec, eligibilityIngressPost(body))
				wantWritten(t, rec, http.StatusOK, row.written, answer)
			})
		}
	}
}
