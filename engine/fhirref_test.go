package engine

import "testing"

func TestResourceRef(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantRef string
		wantOK  bool
	}{
		{"diagnostic report", `{"resourceType":"DiagnosticReport","id":"op-xyz","status":"final"}`, "DiagnosticReport/op-xyz", true},
		{"document reference", `{"resourceType":"DocumentReference","id":"doc-9"}`, "DocumentReference/doc-9", true},
		{"missing id", `{"resourceType":"DiagnosticReport","status":"final"}`, "", false},
		{"missing type", `{"id":"x"}`, "", false},
		{"not json", `not json`, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ref, ok := resourceRef([]byte(c.in))
			if ref != c.wantRef || ok != c.wantOK {
				t.Errorf("resourceRef = (%q,%v), want (%q,%v)", ref, ok, c.wantRef, c.wantOK)
			}
		})
	}
}
