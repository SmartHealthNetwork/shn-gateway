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
	// One Claim: PAS 2.0.1's request Bundle allows one, so the update names its prior by
	// identifier and does not carry it.
	if got := countClaims(t, out); got != 1 {
		t.Fatalf("claims in amend bundle = %d, want 1 (the update alone)", got)
	}
	var b struct {
		Entry []struct{ Resource map[string]any }
	}
	if err := json.Unmarshal(out, &b); err != nil {
		t.Fatal(err)
	}
	claim := b.Entry[0].Resource
	if claim["resourceType"] != "Claim" {
		t.Fatalf("entry[0] is %v, want the update Claim", claim["resourceType"])
	}
	if prof := claim["meta"].(map[string]any)["profile"].([]any); len(prof) != 1 || prof[0] != pasProfileClaimUpdate {
		t.Fatalf("update Claim declares %v, want %s", prof, pasProfileClaimUpdate)
	}
	rel := claim["related"].([]any)[0].(map[string]any)["claim"].(map[string]any)
	if _, ok := rel["reference"]; ok {
		t.Fatalf("related claim carries a reference %v, want the identifier alone", rel["reference"])
	}
	if id := rel["identifier"].(map[string]any); id["system"] != "urn:shn:correlation" || id["value"] != "corr-submit" {
		t.Fatalf("related claim identifier %v, want urn:shn:correlation|corr-submit", id)
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
