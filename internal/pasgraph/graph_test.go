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

// policy is a ReferencePolicy that matches or not, and allows or not.
type policy struct{ match, allow bool }

func (p policy) Matches(*Graph) bool                        { return p.match }
func (p policy) Allows(*Entry, string, map[string]any) bool { return p.allow }

// A nil policy allows no reference; a policy allows only what it names, and
// a policy made for another graph is refused by its own cause.
func TestReferencePolicy(t *testing.T) {
	err := danglingGraph(t).ValidateWithReferencePolicy(nil)
	r := RefusalOf(err)
	if r == nil || r.Holder() != "ClaimResponse" || r.Path != "/insurer" || RefusalCause(err) != RefusalReference {
		t.Fatalf("nil policy: %v", err)
	}
	if err := danglingGraph(t).ValidateWithReferencePolicy(policy{match: true, allow: false}); RefusalOf(err) == nil {
		t.Fatalf("a policy allowing nothing let the reference go: %v", err)
	}
	if err := danglingGraph(t).ValidateWithReferencePolicy(policy{match: true, allow: true}); err != nil {
		t.Fatalf("a policy allowing the reference: %v", err)
	}
	err = danglingGraph(t).ValidateWithReferencePolicy(policy{match: false, allow: true})
	var c *CauseError
	if err == nil || RefusalCause(err) != RefusalPolicy || !asCause(err, &c) || c.Cause() != RefusalPolicy {
		t.Fatalf("a policy for another graph: %v", err)
	}
	if err.Error() != "engine: invalid or incomplete PAS response graph" {
		t.Fatalf("refusal text changed: %q", err.Error())
	}
}

func asCause(err error, c **CauseError) bool {
	v, ok := err.(*CauseError)
	*c = v
	return ok
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
