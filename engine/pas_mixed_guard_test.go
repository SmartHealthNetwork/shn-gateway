//go:build !shn_coreonly

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

// pasGuardParentURL is the fullUrl of a dependent's parent carried as a
// Patient entry of its own, under the request's base.
const pasGuardParentURL = "https://shn.example/fhir/Patient/MBR-PARENT"

// pasGuardCoverage is the member's Coverage entry, its beneficiary the
// member's Patient entry, with slots (JSON members) added.
func pasGuardCoverage(slots string) string {
	if slots != "" {
		slots = "," + slots
	}
	return `{"fullUrl":"https://shn.example/fhir/Coverage/cov","resource":{"resourceType":"Coverage","id":"cov","beneficiary":{"reference":"Patient/MBR-COVERED"}` + slots + `}}`
}

// pasGuardParent is the parent's Patient entry at fullURL, carrying its own
// member identifier and MRN-P, with extra (JSON members) added.
func pasGuardParent(fullURL, extra string) string {
	return `{"fullUrl":"` + fullURL + `","resource":{"resourceType":"Patient","id":"MBR-PARENT","identifier":[{"system":"urn:shn:member","value":"MBR-PARENT"},{"system":"http://example.org/mrn","value":"MRN-P"}]` + extra + `}}`
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
		// A dependent's parent carried as a Patient entry of its own is the
		// Coverage's party (CoverageEntryParties) when only the subscriber or
		// policyHolder of Coverage entries name it, by its fullUrl or a
		// relative reference resolving to it from the Coverage entry.
		{"a parent entry the subscriber names by a relative reference that resolves",
			pasGuardBundle("", pasGuardCoverage(`"subscriber":{"reference":"Patient/MBR-PARENT"}`)+","+pasGuardParent(pasGuardParentURL, "")), false},
		{"a parent entry both slots name by its fullUrl, typed, beside an MRN the parent carries",
			pasGuardBundle("", pasGuardCoverage(`"subscriber":{"reference":"`+pasGuardParentURL+`","type":"Patient","identifier":{"system":"http://example.org/mrn","value":"MRN-P"}},"policyHolder":{"reference":"`+pasGuardParentURL+`"}`)+","+pasGuardParent(pasGuardParentURL, "")), false},
		{"a parent entry under a urn:uuid the subscriber names",
			pasGuardBundle("", pasGuardCoverage(`"subscriber":{"reference":"urn:uuid:parent"}`)+","+pasGuardParent("urn:uuid:parent", "")), false},
		{"a parent entry whose slot names another member by identifier beside it",
			pasGuardBundle("", pasGuardCoverage(`"subscriber":{"reference":"Patient/MBR-PARENT","identifier":{"system":"urn:shn:member","value":"MBR-NOTCOVERED"}}`)+","+pasGuardParent(pasGuardParentURL, "")), true},
		{"a parent entry no Coverage names",
			pasGuardBundle("", pasGuardCoverage("")+","+pasGuardParent(pasGuardParentURL, "")), true},
		{"a parent entry the subscriber names by a relative reference that does not resolve",
			pasGuardBundle("", pasGuardCoverage(`"subscriber":{"reference":"Patient/MBR-PARENT"}`)+","+pasGuardParent("https://other.example/fhir/Patient/MBR-PARENT", "")), true},
		{"a parent entry under a urn:uuid the Claim names too",
			strings.Replace(pasGuardBundle("", pasGuardCoverage(`"subscriber":{"reference":"urn:uuid:parent"}`)+","+pasGuardParent("urn:uuid:parent", "")), `"patient":{"reference":"Patient/MBR-COVERED"}`, `"patient":{"reference":"Patient/MBR-COVERED"},"extension":[{"url":"http://example.org/about","valueReference":{"reference":"urn:uuid:parent"}}]`, 1), true},
		{"a parent entry only another member's Coverage names",
			pasGuardBundle("", pasGuardCoverage("")+","+pasGuardParent(pasGuardParentURL, "")+
				`,{"fullUrl":"https://shn.example/fhir/Patient/MBR-NOTCOVERED","resource":{"resourceType":"Patient","id":"MBR-NOTCOVERED"}}`+
				`,{"fullUrl":"https://shn.example/fhir/Coverage/cov-other","resource":{"resourceType":"Coverage","id":"cov-other","beneficiary":{"reference":"Patient/MBR-NOTCOVERED"},"subscriber":{"reference":"Patient/MBR-PARENT"}}}`), true},
		{"a parent entry the subscriber names, naming another member itself",
			pasGuardBundle("", pasGuardCoverage(`"subscriber":{"reference":"Patient/MBR-PARENT"}`)+","+pasGuardParent(pasGuardParentURL, `,"link":[{"type":"seealso","other":{"reference":"Patient/MBR-NOTCOVERED"}}]`)), true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if got := pasCarriesAnotherPatient([]byte(row.bundle), "MBR-COVERED"); got != row.another {
				t.Fatalf("pasCarriesAnotherPatient = %v, want %v", got, row.another)
			}
		})
	}
}
