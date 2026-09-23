package engine

import (
	"os"
	"strings"
	"testing"
)

// handleUC04 must profile-gate the provider-data lane: ATTEST the questionnaire off the seeded
// order + run the lean single-shot PAS tail (no amendment), persisting against the REAL seeded
// order ref — while every other lane keeps its operative-DiagnosticReport amendment tail
// byte-unchanged. Static source guard (the live e2e/tworilive gate exercises the runtime path).
func TestHandleUC04_ProviderDataAttestsAndLeanTail(t *testing.T) {
	src, err := os.ReadFile("originate.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	fn := extractFunc(t, string(src), "handleUC04")

	// The provider-data branch must come BEFORE the amendment block.
	gateIdx := strings.Index(fn, `g.cfg.OriginationProfile == "provider-data"`)
	if gateIdx < 0 {
		t.Fatalf("handleUC04 does not profile-gate on OriginationProfile == \"provider-data\"")
	}

	// provider-data lane: attest off the seeded order, then the lean tail.
	for _, want := range []string{
		"uc04AttestationAnswers(res.srJSON, resolve)",          // build the attestation map FROM the seeded order (+ its supportingInfo)
		"g.attestAdaptiveQuestionnaire(ctx, r, &res, answers,", // attest the questionnaire — adaptive-aware ($next-question first), re-fill ($populate auto-pops nothing)
		"g.submitClaimAndFollow(ctx, r, pasFollowInputs{",      // the lean single-shot PAS tail (no amendment leg), reporting the payer's own determination
		"attestedAnswerValues(answers)",                        // surface the traces-to-seed evidence
	} {
		if !strings.Contains(fn, want) {
			t.Fatalf("handleUC04 provider-data branch missing %q", want)
		}
	}

	// Bug-2, generalized: the order reference this handler files the authorization
	// under is derived from the ORDER — once, for every lane — and never from a
	// per-scenario literal. The literal named an order no participant's system held,
	// so both the stored authorization and any later inquiry were keyed on a
	// reference that resolved to nothing.
	if !strings.Contains(fn, "srRef, ok := orderRefOrFail(w, res.srJSON, res.attempt)") {
		t.Fatalf("handleUC04 must derive the order ref from the order the system of record supplied")
	}
	for _, literal := range []string{`"ServiceRequest/sr-uc04"`, `"ServiceRequest/sr-uc03"`} {
		if strings.Contains(fn, literal) {
			t.Fatalf("handleUC04 names a built-order literal %s — the order ref must come from the order", literal)
		}
	}
	pdBranch := fn[gateIdx:]
	// Trim to the provider-data branch (ends at the closing `return` before the amendment comment).
	if end := strings.Index(pdBranch, "the operative-DiagnosticReport amendment tail"); end > 0 {
		pdBranch = pdBranch[:end]
	}
	if !strings.Contains(pdBranch, "orderRef := srRef") {
		t.Fatalf("provider-data lane must use the one order reference this handler derived")
	}

	// Every other lane keeps its amendment tail (pas-claim-update + SupplementalReport).
	// SupplementalReport threads the resolved `member` (not the MBR-UC04 literal) so the
	// canary twin (personaSet=canary) attaches ITS OWN operative DiagnosticReport instead of
	// the original member's — see TestHandleUC04_ThreadsSceneMember in
	// originate_members_test.go for the dedicated wiring guard.
	for _, want := range []string{
		`"pas-claim-update"`,
		`ReadSystemOfRecord(g.cfg.SoR).SupplementalReportContext(ctx, member)`,
		"buildAuthoredPASUpdate",
	} {
		if !strings.Contains(fn, want) {
			t.Fatalf("handleUC04 amendment tail missing %q", want)
		}
	}
}
