package engine

import (
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// Exact native delivery preserves the payer's note text and ordering inside
// its ClaimResponse; a separate source-complete EOB action may record them.
func TestNativeSubmit_PayerNotesRemainInExactReply(t *testing.T) {
	for _, tc := range []struct {
		name  string
		notes []shnsdk.PASProcessNote
	}{
		{"none", nil},
		{"multilingual and punctuation", []shnsdk.PASProcessNote{{Type: "display", Text: "Appeal within 60 days — **call** 1-800-555-0100 <ref #7>"}, {Type: "print", Text: "Segunda nota: revisão por par disponível"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertNativeDecisionUntouched(t, decisionClaimResponse("A3", "", "", "", tc.notes))
			assertNativeDecisionUntouched(t, decisionClaimResponse("A1", "", "", "AUTH-1", tc.notes))
		})
	}
}
