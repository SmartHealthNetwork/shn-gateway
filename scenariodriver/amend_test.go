package scenariodriver

import (
	"bytes"
	"encoding/json"
	"testing"
)

func countClaims(t *testing.T, bundle []byte) int {
	t.Helper()
	var b struct {
		Entry []struct {
			Resource struct {
				ResourceType string `json:"resourceType"`
			} `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(bundle, &b); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range b.Entry {
		if e.Resource.ResourceType == "Claim" {
			n++
		}
	}
	return n
}

func TestBuildAmendedRePOST(t *testing.T) {
	out, err := BuildAmendedRePOST("MBR-COVERED", "corr-submit", "corr-amend")
	if err != nil {
		t.Fatal(err)
	}
	// Two Claims: the amend Claim + the physically-included prior clone (br-payer requires it).
	if got := countClaims(t, out); got != 2 {
		t.Fatalf("claims in amend bundle = %d, want 2 (amend + prior clone)", got)
	}
	for _, want := range []string{`"corr-submit"`, `"corr-amend"`, `"prior"`,
		`"QuestionnaireResponse"`, `"Provenance"`, `Patient/MBR-COVERED`,
		`"urn:oid:2.16.840.1.113883.6.300"`} {
		if !bytes.Contains(out, []byte(want)) {
			t.Fatalf("amend bundle missing %s", want)
		}
	}
}

// Source attribution is a holder business identity, not a missing Organization resource.
func TestAmendmentSourceAttribution(t *testing.T) {
	dr := []byte(`{"resourceType":"DiagnosticReport","id":"report","subject":{"reference":"Patient/MBR-COVERED"}}`)
	for _, build := range []func() ([]byte, error){
		func() ([]byte, error) { return BuildAmendedRePOST("MBR-COVERED", "s", "a") },
		func() ([]byte, error) { return BuildFederatedAmendedRePOST("MBR-COVERED", "s", "a", dr) },
	} {
		raw, err := build()
		if err != nil {
			t.Fatal(err)
		}
		var b struct {
			Entry []struct{ Resource map[string]any }
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, e := range b.Entry {
			if e.Resource["resourceType"] != "Provenance" {
				continue
			}
			found = true
			who := e.Resource["agent"].([]any)[0].(map[string]any)["who"].(map[string]any)
			if _, exists := who["reference"]; exists {
				t.Fatalf("dangling source reference: %v", who)
			}
			id, _ := who["identifier"].(map[string]any)
			if id["system"] != "http://smarthealth.network/ids/holder" || id["value"] != "provider" {
				t.Fatalf("incorrect holder attribution: %v", who)
			}
		}
		if !found {
			t.Fatal("missing source provenance")
		}
	}
}
