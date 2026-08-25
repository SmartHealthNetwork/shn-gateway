// legcatalog_test.go — the payer content occupant must have a case for every leg the
// engine's inbound dispatch routes to it.
//
// Re-homed from the retired in-process responder's parity pin. The subject moved with the
// role: the occupant is now the NATIVE responder (a payer answers every Da Vinci leg out of
// its own endpoint, §3.2), so the completeness question is about that responder's switch. A
// leg the inbound dispatch routes to an occupant that has no case for it surfaces as a 500
// at the payer and a 502 "hub routing failed" at the Hub — a silent gap, which is exactly
// what this pin exists to make loud.
package engine

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// responderRoutedLegs is every leg engine inbound dispatch hands to Config.Responder.
// Kept as a literal, hand-maintained list (not introspection over the inbound switch): it
// cannot auto-flag a NEW leg, so extend it when adding an inbound leg that routes through
// Responder.Handle, or that leg's occupant coverage goes unpinned here.
//
// coverage-eligibility is deliberately ABSENT: it is answered engine-side off the member's
// own Coverage and never reaches an occupant (R11). TestResponder_EligibilityIsNotRouted
// below pins that absence rather than leaving it to this comment.
var responderRoutedLegs = []string{
	"crd-order-select",
	"crd-order-dispatch",
	"dtr-questionnaire-fetch",
	"pas-claim",
	"pas-claim-update",
}

// catalogResponder builds a native responder whose partner endpoint is unreachable. Garbage
// bodies are fine and deliberate: the question is whether a leg REACHES its case at all, not
// what that case then does with the bytes — a leg with a case fails on parsing or on the
// transport, a leg without one fails on the switch's default.
func catalogResponder(t *testing.T) LegResponder {
	t.Helper()
	return NewNativeResponder(&http.Client{Transport: errTransport{}}, "http://partner.invalid",
		"svc", newCensusSoR(), func() time.Time { return time.Unix(1700000000, 0).UTC() })
}

// unhandledLeg reports whether err is the switch's generic "no case for this leg" default.
func unhandledLeg(err error, leg string) bool {
	return err != nil && strings.Contains(err.Error(), "unhandled leg") && strings.Contains(err.Error(), leg)
}

// TestNativeResponder_CoversEngineInboundLegCatalog: every Responder-routed leg must reach a
// real case. Failing on the body or the unreachable partner is fine; failing on the switch's
// default is the regression.
func TestNativeResponder_CoversEngineInboundLegCatalog(t *testing.T) {
	n := catalogResponder(t)
	// Control FIRST: the detector must actually fire. coverage-eligibility is a real PA-catalog
	// leg with deliberately NO case (R11 — see TestResponder_EligibilityIsNotRouted), so it is
	// the one input that reaches the switch's default. Without this, every row below could pass
	// because the detector never fires at all. (A made-up leg name would NOT do: it is refused
	// earlier, at contract resolution, and never reaches the switch.)
	if _, err := n.Handle(context.Background(), "coverage-eligibility", "corr-parity", "pci:parity", []byte(`{}`)); !unhandledLeg(err, "coverage-eligibility") {
		t.Fatalf("control: a catalog leg with no case must report unhandled, got err=%v — this test cannot detect a gap", err)
	}
	for _, leg := range responderRoutedLegs {
		_, err := n.Handle(context.Background(), leg, "corr-parity", "pci:parity", []byte(`{}`))
		if unhandledLeg(err, leg) {
			t.Errorf("leg %q: the payer's content occupant has no case for it — engine inbound routes it to Responder, "+
				"so this leg would 500 at the payer and 502 at the Hub", leg)
		}
	}
}

// TestResponder_EligibilityIsNotRouted is the R11 half of the same pin, inverted: eligibility
// is an engine-side Coverage read, so the occupant must NOT have a case for it. If a case
// ever reappears here, an injected occupant could start deciding coverage — the exact fork
// R11 exists to prevent — and it would look like a feature, not a regression.
func TestResponder_EligibilityIsNotRouted(t *testing.T) {
	n := catalogResponder(t)
	_, err := n.Handle(context.Background(), "coverage-eligibility", "corr-parity", "pci:parity", []byte(`{}`))
	if !unhandledLeg(err, "coverage-eligibility") {
		t.Fatalf("the payer's content occupant answered coverage-eligibility (err=%v) — eligibility must never "+
			"reach an occupant (R11); it is decided engine-side from the member's own Coverage", err)
	}
}
