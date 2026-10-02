package engine

import (
	"strings"
	"testing"
)

// pasGuardBundle is a minimal PAS request for MBR-COVERED: its Claim, the
// member's Patient entry (patient, the member identifier and MRN-1 by
// default) and extra entries.
func pasGuardBundle(patient, extra string) string {
	if patient == "" {
		patient = `{"fullUrl":"https://shn.example/fhir/Patient/MBR-COVERED","resource":{"resourceType":"Patient","id":"MBR-COVERED","identifier":[{"system":"urn:shn:member","value":"MBR-COVERED"},{"system":"http://example.org/mrn","value":"MRN-1"}]}}`
	}
	if extra != "" {
		extra = "," + extra
	}
	return `{"resourceType":"Bundle","type":"collection","entry":[{"fullUrl":"https://shn.example/fhir/Claim/c","resource":{"resourceType":"Claim","id":"c","patient":{"reference":"Patient/MBR-COVERED"}}},` + patient + extra + `]}`
}

// The walk's rules, each at the guard. The gateway's engine and the SDK's
// Responder each read this same table (TestPASCarriesAnotherPatientGuard).
func TestPASCarriesAnotherPatientGuard(t *testing.T) {
	rows := []struct {
		name, bundle string
		another      bool
	}{
		{"a reference by an MRN the member's Patient entry carries",
			pasGuardBundle("", `{"resource":{"resourceType":"RelatedPerson","id":"rp","patient":{"identifier":{"system":"http://example.org/mrn","value":"MRN-1"}}}}`), false},
		{"an MRN the member's Patient entry does not carry, in a subject element",
			pasGuardBundle("", `{"resource":{"resourceType":"RelatedPerson","id":"rp","patient":{"identifier":{"system":"http://example.org/mrn","value":"MRN-OTHER"}}}}`), true},
		{"a reference typed Patient by another MRN, outside a subject element",
			pasGuardBundle("", `{"resource":{"resourceType":"Organization","id":"o","extension":[{"url":"http://example.org/about","valueReference":{"type":"Patient","identifier":{"system":"http://example.org/mrn","value":"MRN-OTHER"}}}]}}`), true},
		{"an untyped reference by another MRN, outside a subject element",
			pasGuardBundle("", `{"resource":{"resourceType":"Organization","id":"o","extension":[{"url":"http://example.org/about","valueReference":{"identifier":{"system":"http://example.org/mrn","value":"MRN-OTHER"}}}]}}`), false},
		{"a Patient entry that is the member by its member identifier alone",
			pasGuardBundle(`{"fullUrl":"https://shn.example/fhir/Patient/srv-9","resource":{"resourceType":"Patient","id":"srv-9","identifier":[{"system":"urn:shn:member","value":"MBR-COVERED"}]}}`, ""), false},
		{"a Patient entry with neither the member's id nor its member identifier",
			pasGuardBundle(`{"fullUrl":"https://shn.example/fhir/Patient/srv-9","resource":{"resourceType":"Patient","id":"srv-9"}}`, ""), true},
		{"the Bundle's own signature names another member",
			strings.Replace(pasGuardBundle("", ""), `"type":"collection",`, `"type":"collection","signature":{"when":"2026-06-04T12:00:00Z","who":{"reference":"Patient/MBR-NOTCOVERED"},"data":"AA=="},`, 1), true},
		{"a decimal outside float64's range, about the member only",
			pasGuardBundle("", `{"resource":{"resourceType":"RelatedPerson","id":"rp","extension":[{"url":"http://example.org/amount","valueDecimal":1e400}],"patient":{"reference":"Patient/MBR-COVERED"}}}`), false},
		{"a decimal outside float64's range beside another member",
			pasGuardBundle("", `{"resource":{"resourceType":"RelatedPerson","id":"rp","extension":[{"url":"http://example.org/amount","valueDecimal":1e400}],"patient":{"reference":"Patient/MBR-NOTCOVERED"}}}`), true},
		{"a bundle it cannot read", `{"resourceType":"Bundle","entry":[`, true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if got := pasCarriesAnotherPatient([]byte(row.bundle), "MBR-COVERED"); got != row.another {
				t.Fatalf("pasCarriesAnotherPatient = %v, want %v", got, row.another)
			}
		})
	}
}
