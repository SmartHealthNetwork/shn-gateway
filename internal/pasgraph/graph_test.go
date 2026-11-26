package pasgraph

import (
	"encoding/json"
	"testing"
)

func danglingGraph(t *testing.T) *Graph {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"resourceType": "Bundle", "type": "collection", "entry": []any{
		map[string]any{"fullUrl": "https://payer.test/fhir/ClaimResponse/cr", "resource": map[string]any{"resourceType": "ClaimResponse", "id": "cr", "insurer": map[string]any{"reference": "Organization/o"}}},
	}})
	g, err := Read(raw)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// Validate refuses a reference nothing in the Bundle resolves, naming its
// holder and path.
func TestValidateRefusesADanglingReference(t *testing.T) {
	err := danglingGraph(t).Validate()
	r := RefusalOf(err)
	if r == nil || r.Holder() != "ClaimResponse" || r.Path != "/insurer" || RefusalCause(err) != RefusalReference {
		t.Fatalf("dangling reference: %v", err)
	}
}

// A trailing version is one segment pair; a base holding /_history/ elsewhere
// is part of the reference.
func TestSplitHistory(t *testing.T) {
	for ref, want := range map[string][3]string{
		"Patient/p/_history/2":                 {"Patient/p", "2", "true"},
		"https://h/fhir/Patient/p/_history/2":  {"https://h/fhir/Patient/p", "2", "true"},
		"https://host/_history/fhir/Patient/p": {"https://host/_history/fhir/Patient/p", "", "false"},
		"Patient/p":                            {"Patient/p", "", "false"},
		"Patient/p/_history/":                  {"Patient/p", "", "true"},
	} {
		base, version, versioned := SplitHistory(ref)
		if base != want[0] || version != want[1] || (want[2] == "true") != versioned {
			t.Errorf("%s: %q %q %v", ref, base, version, versioned)
		}
		if StripHistoryRef(ref) != want[0] {
			t.Errorf("%s: stripped to %q", ref, StripHistoryRef(ref))
		}
	}
}
