package engine

import (
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/internal/lanequalify"
)

// The boot warm-up's request (lanequalify.CertificationRow) names the profile
// a certification of a PAS request bundle at the same line names, so it warms
// the path a real certification takes.
func TestCertificationRow_NamesTheCertifiedProfile(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		_, got, ok := lanequalify.CertificationRow(line)
		want, known := profileFor("PASRequestBundle", line, "pas-claim")
		if !ok || !known || got != want {
			t.Errorf("%s: warm-up profile %q (%v), certification profile %q (%v)", line, got, ok, want, known)
		}
	}
	if _, _, ok := lanequalify.CertificationRow("1.0"); ok {
		t.Error("an unknown line has a warm-up row")
	}
}
