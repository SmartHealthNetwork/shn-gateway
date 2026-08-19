// gateway/engine/manifest_export_test.go
package engine

import "testing"

// ManifestSteps must return every compatManifest row, in order, as a copy —
// mutating the result must not touch the real table.
func TestManifestStepsCopiesTable(t *testing.T) {
	got := ManifestSteps()
	if len(got) != len(compatManifest) {
		t.Fatalf("ManifestSteps len = %d, want %d", len(got), len(compatManifest))
	}
	for i, s := range got {
		w := compatManifest[i]
		if s.Contract != w.Contract || s.From != w.From || s.To != w.To || s.Class != w.Class {
			t.Fatalf("row %d = %+v, want %+v", i, s, w)
		}
	}
	got[0].Contract = "mutated"
	if compatManifest[0].Contract == "mutated" {
		t.Fatal("ManifestSteps returned the live table, not a copy")
	}
}
