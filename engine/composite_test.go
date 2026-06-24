package engine

import (
	"context"
	"testing"
)

// recordingResponder records which legs it was asked to handle and returns a tagged
// ResponseFHIR so the test can tell which delegate ran.
type recordingResponder struct {
	tag  string
	legs *[]string
}

func (r recordingResponder) Handle(_ context.Context, leg, _, _ string, _ []byte) (LegResult, error) {
	*r.legs = append(*r.legs, r.tag+":"+leg)
	return LegResult{ResponseFHIR: []byte(r.tag)}, nil
}

func TestCompositeResponder_RoutesDaVinciToNativeEligibilityToFallback(t *testing.T) {
	var nativeLegs, fallbackLegs []string
	native := recordingResponder{tag: "native", legs: &nativeLegs}
	fallback := recordingResponder{tag: "fallback", legs: &fallbackLegs}
	c := NewCompositeResponder(native, fallback, false)

	// CRD and DTR forward native; no-Da-Vinci-RI implements eligibility → managed fallback.
	for _, leg := range []string{"crd-order-select", "dtr-questionnaire-fetch"} {
		res, err := c.Handle(context.Background(), leg, "corr", "pci", nil)
		if err != nil {
			t.Fatalf("%s: %v", leg, err)
		}
		if string(res.ResponseFHIR) != "native" {
			t.Errorf("%s routed to %s, want native", leg, res.ResponseFHIR)
		}
	}
	// pasNative=false, so the conformant PAS pair routes to the managed fallback too.
	for _, leg := range []string{"coverage-eligibility", "pas-claim", "pas-claim-update"} {
		res, err := c.Handle(context.Background(), leg, "corr", "pci", nil)
		if err != nil {
			t.Fatalf("%s: %v", leg, err)
		}
		if string(res.ResponseFHIR) != "fallback" {
			t.Errorf("%s routed to %s, want fallback", leg, res.ResponseFHIR)
		}
	}
	if len(nativeLegs) != 2 || len(fallbackLegs) != 3 {
		t.Errorf("routing counts: native=%v fallback=%v", nativeLegs, fallbackLegs)
	}
}

// routeName drives one leg through the composite and returns the tag of whichever
// delegate handled it (recordingResponder tags its ResponseFHIR with its own name).
func routeName(t *testing.T, c LegResponder, leg string) string {
	t.Helper()
	res, err := c.Handle(context.Background(), leg, "", "", nil)
	if err != nil {
		t.Fatalf("%s: %v", leg, err)
	}
	return string(res.ResponseFHIR)
}

func TestComposite_PASNativeRoutesPairBothOrNeither(t *testing.T) {
	var nl, fl []string
	native := recordingResponder{tag: "native", legs: &nl}
	fallback := recordingResponder{tag: "fallback", legs: &fl}

	off := NewCompositeResponder(native, fallback, false)
	for _, leg := range []string{"pas-claim", "pas-claim-update"} {
		if got := routeName(t, off, leg); got != "fallback" {
			t.Fatalf("PAS off: leg %s routed to %s, want fallback", leg, got)
		}
	}
	on := NewCompositeResponder(native, fallback, true)
	for _, leg := range []string{"pas-claim", "pas-claim-update"} {
		if got := routeName(t, on, leg); got != "native" {
			t.Fatalf("PAS on: leg %s routed to %s, want native", leg, got)
		}
	}
	// coverage-eligibility always routes to fallback regardless of pasNative (managed).
	for _, c := range []LegResponder{off, on} {
		if got := routeName(t, c, "coverage-eligibility"); got != "fallback" {
			t.Fatalf("coverage-eligibility routed to %s, want fallback (always managed)", got)
		}
	}
}

// failingFallback fails the test if invoked. Used as the composite fallback to prove the
// forwarded legs route to NATIVE, never the sandbox — FR-G28 / finding #3:
// crd-order-select (the conformant CRD leg the P5 ingress emits) used to fall
// through to the sandbox, masking the real br-payer in the opt-in two-RI gate.
type failingFallback struct{ t *testing.T }

func (f failingFallback) Handle(_ context.Context, leg, _, _ string, _ []byte) (LegResult, error) {
	f.t.Fatalf("fallback invoked for forwarded leg %q — should route to native", leg)
	return LegResult{}, nil
}

func TestComposite_ForwardsConformantCRDNative(t *testing.T) {
	var nl []string
	native := recordingResponder{tag: "native", legs: &nl}
	c := NewCompositeResponder(native, failingFallback{t}, true) // pasNative on
	for _, leg := range []string{"crd-order-select", "dtr-questionnaire-fetch", "pas-claim", "pas-claim-update"} {
		if got := routeName(t, c, leg); got != "native" {
			t.Errorf("leg %q routed to %s, want native", leg, got)
		}
	}
}
