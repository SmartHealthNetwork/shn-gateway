// crd_dispatch_responder_test.go — D-S7K-13 responder-parity correction:
// proves the SANDBOX payer genuinely adjudicates the crd-order-dispatch leg. Before this
// change, "crd-order-dispatch" had no case in sandboxResponder.Handle at all — it fell to the
// switch's default and errored "unhandled leg" (surfaced as 500 at the payer, then 502 "hub
// routing failed" at the Hub) for every in-process payer with no native-forward wired
// (devstack, test/harness, Kit's own counterparty). composite.go always forwards this leg to a
// REAL Da Vinci partner when native mode is configured (never falls back to sandbox) — so this
// gap was invisible everywhere the leg had been exercised before: cloud's brpayer-gw natively
// forwards to a live br-payer oracle (Docker-only, test/tworilive's HomeOxygen gate), and
// originate_homeoxygen_test.go cans the ENTIRE wire round-trip at the substrate-stub level
// (homeoxygenSubstrate), never touching a real sandboxResponder. This test closes the sandbox
// fallback so the leg has a genuine (not fake) DEF-4 verdict path when no native partner is
// configured — the same posture already covering every OTHER conformant leg in the switch.
package engine

import (
	"context"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// dispatchReqE1390 is a minimal conformant order-dispatch CDS Hooks request carrying a
// dispatched E1390 DeviceRequest (the UC-03 HomeOxygenDispatch persona's HCPCS code) —
// shaped like crd_dispatch_native_test.go's fixtures.
const dispatchReqE1390 = `{"hook":"order-dispatch","context":{"patientId":"MBR-COVERED","dispatchedOrders":["DeviceRequest/dr1"],"performer":"Organization/dme1"},"prefetch":{"deviceHistory":{"resourceType":"Bundle","entry":[{"fullUrl":"DeviceRequest/dr1","resource":{"resourceType":"DeviceRequest","id":"dr1","subject":{"reference":"Patient/MBR-COVERED"},"codeCodeableConcept":{"coding":[{"system":"http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets","code":"E1390","display":"Stationary Oxygen Concentrator"}]}}}]},"coverage":{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/MBR-COVERED"}}}]}}}`

// TestSandboxResponder_CRDOrderDispatch_AdvisoryCard is the RED->GREEN pin: the sandbox
// responder must answer crd-order-dispatch with a genuine ADVISORY conditional-coverage card
// (NeedsDTR true, PARequired false — mirrors originate_homeoxygen.go's documented gate), not
// the "unhandled leg" 500.
func TestSandboxResponder_CRDOrderDispatch_AdvisoryCard(t *testing.T) {
	responder := newSandboxResponderForTest(t)

	res, err := responder.Handle(context.Background(), "crd-order-dispatch", "corr-dispatch-1", "pci:mbr-covered", []byte(dispatchReqE1390))
	if err != nil {
		t.Fatalf("Handle(crd-order-dispatch): %v (want a genuine advisory card, not an unhandled-leg error)", err)
	}
	if res.Status != 0 {
		t.Fatalf("Handle(crd-order-dispatch): status=%d message=%q, want a 200 advisory card", res.Status, res.Message)
	}
	cov, err := shnsdk.ParseCards(res.ResponseFHIR)
	if err != nil {
		t.Fatalf("ParseCards: %v", err)
	}
	if !cov.NeedsDTR() {
		t.Errorf("card NeedsDTR() = false, want true — the card must advertise a questionnaire")
	}
	if cov.PARequired() {
		t.Errorf("card PARequired() = true, want false — order-dispatch is ADVISORY (conditional coverage), never auth-needed")
	}
	if len(cov.Questionnaires) != 1 || cov.Questionnaires[0] != shnsdk.QuestionnaireCanonicalLumbarMRI {
		t.Errorf("card Questionnaires = %v, want [%s] (DEF-4 stub-reuse, same precedent as the L8000 HCPCS case)", cov.Questionnaires, shnsdk.QuestionnaireCanonicalLumbarMRI)
	}
}

// TestSandboxAdjudicator_OrderSelect_HomeOxygenCodes pins the two order-dispatch HCPCS codes
// (E0431 = MBR-OX HomeOxygen, E1390 = MBR-PD-UC03 HomeOxygenDispatch) into the sandbox
// adjudicator's OrderSelect table, alongside the existing 72148/L8000 rows.
func TestSandboxAdjudicator_OrderSelect_HomeOxygenCodes(t *testing.T) {
	adj := NewSandboxAdjudicator(NewStubHolderData(), nil)
	for _, code := range []string{"E0431", "E1390"} {
		needsDTR, canonical := adj.OrderSelect(code)
		if !needsDTR {
			t.Errorf("OrderSelect(%q) needsDTR = false, want true", code)
		}
		if canonical != shnsdk.QuestionnaireCanonicalLumbarMRI {
			t.Errorf("OrderSelect(%q) canonical = %q, want %q", code, canonical, shnsdk.QuestionnaireCanonicalLumbarMRI)
		}
	}
}

// responderRoutedLegs is inbound.go's switch (:99-116) restricted to the legs that route
// through a payer's LegResponder (coverage-eligibility/crd-order-select/dtr-questionnaire-fetch/
// pas-claim/pas-claim-update/crd-order-dispatch) — federated-query and patient-dtr are NOT
// Responder-routed (separate FQS/PHG-surface handlers) and are deliberately excluded. This is
// the erosion-alarm pin D-S7K-13 asked for: every leg the ENGINE's inbound catalog routes to a
// payer's content Responder must have a genuine case in sandboxResponder.Handle — never silently
// regress to the switch's "unhandled leg" default, which every in-process payer with no
// native-forward configured (devstack/harness/Kit) would surface as a 500 (then a 502 at the
// Hub). Kept as a literal, hand-maintained list (not introspection over inbound.go) — this
// hand-list cannot auto-flag a NEW leg added to inbound.go's switch; extend this list when
// adding an inbound leg that routes through Responder.Handle, or that leg's sandbox coverage
// goes unpinned here.
var responderRoutedLegs = []string{
	"coverage-eligibility",
	"crd-order-select",
	"dtr-questionnaire-fetch",
	"pas-claim",
	"pas-claim-update",
	"crd-order-dispatch",
}

// TestSandboxResponder_CoversEngineInboundLegCatalog is the D-S7K-13 parity pin: garbage input
// for every Responder-routed leg must fail on PARSING that leg's body (a legitimate 400/500 from
// the leg's own case), never on the switch's generic "unhandled leg" default — the signal that a
// leg the engine's inbound catalog dispatches to has no sandbox content path at all. Before this
// change, "crd-order-dispatch" was exactly that silent gap.
func TestSandboxResponder_CoversEngineInboundLegCatalog(t *testing.T) {
	responder := newSandboxResponderForTest(t)
	for _, leg := range responderRoutedLegs {
		_, err := responder.Handle(context.Background(), leg, "corr-parity", "pci:parity", []byte(`{}`))
		if err != nil && err.Error() == `sandboxResponder: unhandled leg "`+leg+`"` {
			t.Errorf("leg %q: sandboxResponder.Handle has no case for it (engine/inbound.go routes it to a payer Responder, but the sandbox fallback silently omits it)", leg)
		}
	}
}
