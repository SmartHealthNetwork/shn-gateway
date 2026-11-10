//go:build !shn_coreonly

package engine

import "testing"

func TestLiteralReferenceType(t *testing.T) {
	for ref, want := range map[string]string{
		"Patient/1":                          "Patient",
		"RelatedPerson/1/_history/2":         "RelatedPerson",
		"https://ehr.example/fhir/Patient/1": "Patient",
		"Organization/9":                     "Organization",
		"#rp":                                "",
		"urn:uuid:9a6e":                      "",
		"":                                   "",
		"plain":                              "",
	} {
		if got := literalReferenceType(ref); got != want {
			t.Errorf("literalReferenceType(%q) = %q, want %q", ref, got, want)
		}
	}
}
