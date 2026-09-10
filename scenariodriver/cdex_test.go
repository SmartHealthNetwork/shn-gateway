package scenariodriver

import (
	"bytes"
	"encoding/json"
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

func TestFacilityCDexEvidencePreservesPolicyAndLogicalSource(t *testing.T) {
	_, raw, err := FacilityCDexEvidence("MBR-UC05", time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Agent []struct {
			Who struct {
				Reference  string
				Identifier struct{ System, Value string }
			}
		}
		Policy []string
		Reason []struct {
			Coding []struct{ System, Code string }
		}
	}
	if err = json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Agent) != 1 || p.Agent[0].Who.Reference != "" || p.Agent[0].Who.Identifier.System != "http://smarthealth.network/ids/holder" || p.Agent[0].Who.Identifier.Value != "metro-spine" {
		t.Fatalf("source=%+v", p)
	}
	if len(p.Policy) != 1 || p.Policy[0] != "Consent/uc05-treat" || len(p.Reason) != 1 || len(p.Reason[0].Coding) != 1 || p.Reason[0].Coding[0].System != "http://terminology.hl7.org/CodeSystem/v3-ActReason" || p.Reason[0].Coding[0].Code != "TREAT" {
		t.Fatalf("policy/reason=%+v", p)
	}
}
