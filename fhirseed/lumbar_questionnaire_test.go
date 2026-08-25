package fhirseed

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// lumbarQuestionnaireDriftGuardSHA256 is the sha256 of the demo lumbar-MRI questionnaire
// fixture, shared (as an identical literal) by every one of its four module-local homes'
// own tests: this package, the sdk module (sdk/lumbar_fixture_test.go), internal/dtr, and
// tools/sampleparticipant. A hand-edit to any ONE copy that the other three don't also
// receive fails THAT copy's own test immediately. See test/fixtureparity for a direct
// byte-comparison of this package's copy against internal/dtr's (the two homes reachable
// from a single root-module test). Recompute with
// `shasum -a 256 gateway/fhirseed/testdata/lumbar-mri-questionnaire.json` after any
// deliberate, synchronized edit to all four copies.
const lumbarQuestionnaireDriftGuardSHA256 = "7c5a062da8beacc2d21f9e6361705e4b78b52859825e5521a7aca13b74218773"

// TestDemoLumbarQuestionnaire_MatchesDriftGuardHash pins this package's own copy of the
// demo lumbar-MRI questionnaire against the shared drift-guard hash — the guard that
// catches exactly the failure mode ("a stand-in copy silently drifts from its twin")
// that produced three live defects on this branch.
func TestDemoLumbarQuestionnaire_MatchesDriftGuardHash(t *testing.T) {
	sum := sha256.Sum256(lumbarQuestionnaireJSON)
	if got := hex.EncodeToString(sum[:]); got != lumbarQuestionnaireDriftGuardSHA256 {
		t.Fatalf("gateway/fhirseed's demo lumbar questionnaire fixture drifted from the shared hash: got sha256 %s, want %s (sdk, internal/dtr, and tools/sampleparticipant all carry byte-identical copies pinned to the same hash — if this fixture changed on purpose, recompute and update the constant in all four places together)", got, lumbarQuestionnaireDriftGuardSHA256)
	}
}
