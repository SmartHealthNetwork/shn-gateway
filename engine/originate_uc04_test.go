package engine

import (
	"os"
	"strings"
	"testing"
)

// handleUC04 must profile-gate the provider-data lane: ATTEST the questionnaire off the seeded
// order + run the lean single-shot PAS tail (no amendment), persisting against the REAL seeded
// order ref — while the sandbox lane keeps its operative-DiagnosticReport amendment tail
// byte-unchanged. Static source guard (the live e2e/tworilive gate exercises the runtime path).
func TestHandleUC04_ProviderDataAttestsAndLeanTail(t *testing.T) {
	src, err := os.ReadFile("originate.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	fn := extractFunc(t, string(src), "handleUC04")

	// The provider-data branch must come BEFORE the sandbox amendment block.
	gateIdx := strings.Index(fn, `g.cfg.OriginationProfile == "provider-data"`)
	if gateIdx < 0 {
		t.Fatalf("handleUC04 does not profile-gate on OriginationProfile == \"provider-data\"")
	}

	// provider-data lane: attest off the seeded order, then the lean tail.
	for _, want := range []string{
		"uc04AttestationAnswers(res.srJSON)",       // build the attestation map FROM the seeded order
		"FillQuestionnaireFromAnswers",             // attest the questionnaire (re-fill, $populate auto-pops nothing)
		"g.submitClaimAndResolve(ctx, r, res.pci,", // the lean single-shot PAS tail (no member param)
		"attestedAnswerValues(answers)",            // surface the traces-to-seed evidence
	} {
		if !strings.Contains(fn, want) {
			t.Fatalf("handleUC04 provider-data branch missing %q", want)
		}
	}

	// Bug-2: persist against the REAL seeded order ref (resourceRef(res.srJSON)), never the
	// sandbox `srRef` literal, in the provider-data lane.
	pdBranch := fn[gateIdx:]
	// Trim to the provider-data branch (ends at the closing `return` before the sandbox comment).
	if end := strings.Index(pdBranch, "sandbox: the operative-DiagnosticReport amendment tail"); end > 0 {
		pdBranch = pdBranch[:end]
	}
	if !strings.Contains(pdBranch, "orderRef, ok := resourceRef(res.srJSON)") {
		t.Fatalf("provider-data lane must derive the order ref via resourceRef(res.srJSON)")
	}
	if strings.Contains(pdBranch, "StoreAuthNumber(srRef") {
		t.Fatalf("provider-data lane must persist against the seeded order ref, not the sandbox srRef literal")
	}

	// The sandbox lane keeps its amendment tail (pas-claim-update + SupplementalReport).
	for _, want := range []string{
		`"pas-claim-update"`,
		`g.cfg.SoR.SupplementalReport("MBR-UC04")`,
		"BuildConformantClaimUpdateBundle",
	} {
		if !strings.Contains(fn, want) {
			t.Fatalf("handleUC04 sandbox amendment tail missing %q (must stay byte-unchanged)", want)
		}
	}
}
