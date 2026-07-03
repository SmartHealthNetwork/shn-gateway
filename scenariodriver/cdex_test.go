package scenariodriver

import (
	"bytes"
	"testing"
	"time"
)

func TestFacilityCDexEvidence_AndFederatedRePOST(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	dr, prov, err := FacilityCDexEvidence("MBR-UC05", now)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(dr, []byte("dr-uc05-operative")) {
		t.Fatalf("DR is not the operative report: %.300s", dr)
	}
	if !bytes.Contains(prov, []byte("DiagnosticReport/dr-uc05-operative")) {
		t.Fatalf("Provenance does not target the DR: %.300s", prov)
	}
	out, err := BuildFederatedAmendedRePOST("MBR-COVERED", "cs", "ca", dr)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"DiagnosticReport"`, `DiagnosticReport/dr-uc05-operative`,
		`Patient/MBR-COVERED`, `"cs"`, `"ca"`} {
		if !bytes.Contains(out, []byte(want)) {
			t.Fatalf("federated amend bundle missing %s", want)
		}
	}
	if got := countClaims(t, out); got != 2 {
		t.Fatalf("claims = %d, want 2", got)
	}
}
