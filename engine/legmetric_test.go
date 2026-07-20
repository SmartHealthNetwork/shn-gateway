// legmetric_test.go — hermetic tests for the LegMetric hook.
// Like the Observer seam it is ADDITIVE instrumentation: nil = zero emission
// (the published-gateway default); neutrality is asserted on/off.
package engine

import (
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func uc03Coverage() shnsdk.CardCoverage {
	return shnsdk.CardCoverage{Covered: shnsdk.CoveredCovered, PANeeded: shnsdk.PANeededAuthNeeded, Questionnaires: []string{"http://example.org/q"}}
}

// TestLegMetric_OriginationOutcomes drives UC-03 through the hermetic
// stubSubstrate harness with a capturing LegMetric hook: the CRD leg answers
// (routed+answered) and the DTR leg dies at the stub Hub's 500
// (routed+unreachable). roundTrip is the single choke point, so this covers
// the emit sites for every origination leg in both lanes.
func TestLegMetric_OriginationOutcomes(t *testing.T) {
	gw, _, _ := crdTestSystem(t, uc03Coverage())
	var got []string
	gw.cfg.LegMetric = func(outcome string) { got = append(got, outcome) }

	callUC03(t, gw) // HTTP outcome irrelevant; the stub 500s leg 2 by design

	want := []string{LegOutcomeRouted, LegOutcomeAnswered, LegOutcomeRouted, LegOutcomeUnreachable}
	if len(got) != len(want) {
		t.Fatalf("outcomes = %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("outcome %d = %q, want %q (all: %v)", i, got[i], w, got)
		}
	}
}

// TestLegMetric_DeniedOutcome: an authz 403 (errAuthorizationDenied) counts as
// "denied", NOT "failed" — a policy denial (the canary's uc05-noconsent PASS
// condition) must not ride the per-service leg-error alarm.
func TestLegMetric_DeniedOutcome(t *testing.T) {
	gw, stub, _ := crdTestSystem(t, uc03Coverage())
	stub.denyAuthorize = true
	var got []string
	gw.cfg.LegMetric = func(outcome string) { got = append(got, outcome) }

	callUC03(t, gw)

	want := []string{LegOutcomeRouted, LegOutcomeDenied}
	if len(got) != len(want) {
		t.Fatalf("outcomes = %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("outcome %d = %q, want %q (all: %v)", i, got[i], w, got)
		}
	}
}

// TestLegMetric_FailedOutcome: a non-403 authorize failure is an opaque
// "failed" — neither denied nor unreachable.
func TestLegMetric_FailedOutcome(t *testing.T) {
	gw, stub, _ := crdTestSystem(t, uc03Coverage())
	stub.failAuthorize = true
	var got []string
	gw.cfg.LegMetric = func(outcome string) { got = append(got, outcome) }

	callUC03(t, gw)

	want := []string{LegOutcomeRouted, LegOutcomeFailed}
	if len(got) != len(want) {
		t.Fatalf("outcomes = %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("outcome %d = %q, want %q (all: %v)", i, got[i], w, got)
		}
	}
}

// TestLegMetric_NilIsNoop: the published-gateway default (no hook) must not panic.
func TestLegMetric_NilIsNoop(t *testing.T) {
	gw, _, _ := crdTestSystem(t, uc03Coverage())
	gw.legMetric(LegOutcomeRouted) // must not panic
	callUC03(t, gw)                // full drive with nil hook
}

// TestLegMetric_ConformanceNeutral: responses byte-identical hook-on vs hook-off
// — mirror TestObserver_ConformanceNeutral (observer_test.go:225) exactly,
// replacing the Observer with a LegMetric func that appends to a slice.
func TestLegMetric_ConformanceNeutral(t *testing.T) {
	respWith := driveUC03Body(t, true)
	respWithout := driveUC03Body(t, false)
	if respWith != respWithout {
		t.Fatalf("LegMetric emission changed the response body:\nwith:    %s\nwithout: %s", respWith, respWithout)
	}
}

// driveUC03Body runs one UC-03 drive with/without a LegMetric hook and returns
// the HTTP response body. callUC03 (originate_test.go:313) returns
// *httptest.ResponseRecorder, not a body string — mirror the recorder pattern
// TestObserver_ConformanceNeutral uses (observer_test.go:236-237) rather than
// the brief's literal string return.
func driveUC03Body(t *testing.T, hook bool) string {
	t.Helper()
	gw, _, _ := crdTestSystem(t, uc03Coverage())
	if hook {
		gw.cfg.LegMetric = func(string) {}
	}
	rec := callUC03(t, gw)
	return rec.Body.String()
}
