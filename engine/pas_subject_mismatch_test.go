package engine

import (
	"encoding/json"
	"testing"
)

// TestPASResponseSubjectMismatchNamesItsCause: the subject-binding read of a
// payer's answer says which subject did not bind and why, and reads a subject
// under an entry identified by a URN by the same identity as the closure walk.
// A ClaimResponse whose own patient reference resolves to nothing, or to two
// entries, is a graph that does not close, and the cause says so with the
// closure walk's own wording (that answer is refused before the subject read).
// The shape is a payer echoing a request whose entries sit under urn:uuid with
// relative references: its ClaimResponse at urn:uuid names "Patient/p", and the
// Bundle carries one Patient whose RESTful identity ends in it.
func TestPASResponseSubjectMismatchNamesItsCause(t *testing.T) {
	const crURN = "urn:uuid:10000000-0000-4000-8000-000000000001"
	const covURN = "urn:uuid:10000000-0000-4000-8000-000000000002"
	const patient = "https://payer.test/fhir/Patient/p"
	shaped := func() map[string]any {
		b := assemblySmallGraph()
		cr := b["entry"].([]any)[0].(map[string]any)
		cr["fullUrl"] = crURN
		delete(cr["resource"].(map[string]any), "id")
		b["entry"] = append(b["entry"].([]any), map[string]any{"fullUrl": covURN, "resource": map[string]any{
			"resourceType": "Coverage", "status": "active",
			"beneficiary": map[string]any{"reference": "Patient/p"},
			"contained":   []any{map[string]any{"resourceType": "RelatedPerson", "id": "rp", "patient": map[string]any{"reference": "Patient/p"}}},
		}})
		return b
	}
	rows := []struct {
		name   string
		mutate func(b map[string]any)
		want   string
	}{
		{"URN ClaimResponse and Coverage, relative subjects, one RESTful Patient", func(b map[string]any) {}, ""},
		{"two candidate Patients", func(b map[string]any) {
			b["entry"] = append(b["entry"].([]any), map[string]any{"fullUrl": "https://other.test/fhir/Patient/p", "resource": map[string]any{"resourceType": "Patient", "id": "p"}})
		}, `the response graph does not close: entry 0 (ClaimResponse ` + crURN + `) /patient references Patient "Patient/p", which is relative under entry 0 (ClaimResponse ` + crURN + `), which is identified by a URN, and 2 entries have a RESTful identity ending in Patient/p (https://payer.test/fhir/Patient/p, https://other.test/fhir/Patient/p)`},
		{"no candidate Patient", func(b map[string]any) {
			b["entry"].([]any)[0].(map[string]any)["resource"].(map[string]any)["patient"] = map[string]any{"reference": "Patient/absent"}
		}, `the response graph does not close: entry 0 (ClaimResponse ` + crURN + `) /patient references Patient "Patient/absent", which is relative under entry 0 (ClaimResponse ` + crURN + `), which is identified by a URN, and no entry of the Bundle has a RESTful identity ending in Patient/absent`},
		{"Coverage beneficiary under a URN names another patient", func(b map[string]any) {
			b["entry"].([]any)[3].(map[string]any)["resource"].(map[string]any)["beneficiary"] = map[string]any{"reference": "Patient/q"}
			b["entry"] = append(b["entry"].([]any), map[string]any{"fullUrl": "https://payer.test/fhir/Patient/q", "resource": map[string]any{"resourceType": "Patient", "id": "q"}})
		}, `entry 3 (Coverage ` + covURN + `) /beneficiary names "Patient/q" (https://payer.test/fhir/Patient/q), which is not the ClaimResponse's patient ` + patient},
		{"contained resource under a URN owner names another patient", func(b map[string]any) {
			b["entry"].([]any)[3].(map[string]any)["resource"].(map[string]any)["contained"].([]any)[0].(map[string]any)["patient"] = map[string]any{"reference": "Patient/q"}
			b["entry"] = append(b["entry"].([]any), map[string]any{"fullUrl": "https://payer.test/fhir/Patient/q", "resource": map[string]any{"resourceType": "Patient", "id": "q"}})
		}, `entry 3 (Coverage ` + covURN + `) /contained/0/patient names "Patient/q" (https://payer.test/fhir/Patient/q), which is not the ClaimResponse's patient ` + patient},
		{"identifier-only subject", func(b map[string]any) {
			b["entry"].([]any)[3].(map[string]any)["resource"].(map[string]any)["beneficiary"] = map[string]any{"identifier": map[string]any{"system": "urn:mbr", "value": "p"}}
		}, `entry 3 (Coverage ` + covURN + `) /beneficiary names a subject without a reference`},
		{"ClaimResponse names no patient", func(b map[string]any) {
			delete(b["entry"].([]any)[0].(map[string]any)["resource"].(map[string]any), "patient")
		}, `entry 0 (ClaimResponse ` + crURN + `) names no patient by reference`},
		// An empty reference string is refused by the closure walk before the
		// subject read; the subject read's own guard (a patient without a
		// reference) is the row above, and never resolves "" against a base.
		{"ClaimResponse patient reference is empty", func(b map[string]any) {
			b["entry"].([]any)[0].(map[string]any)["resource"].(map[string]any)["patient"] = map[string]any{"reference": ""}
		}, `the response graph does not close: entry 0 (ClaimResponse ` + crURN + `) carries an empty reference at /patient`},
		{"another Patient entry", func(b map[string]any) {
			b["entry"] = append(b["entry"].([]any), map[string]any{"fullUrl": "https://payer.test/fhir/Patient/q", "resource": map[string]any{"resourceType": "Patient", "id": "q"}})
		}, `entry 4 (Patient/q) is a Patient that is not the ClaimResponse's patient ` + patient},
		{"graph does not close", func(b map[string]any) {
			b["entry"].([]any)[0].(map[string]any)["resource"].(map[string]any)["patient"] = map[string]any{"reference": "https://payer.test/fhir/Patient/gone"}
		}, `the response graph does not close: entry 0 (ClaimResponse ` + crURN + `) /patient references Patient "https://payer.test/fhir/Patient/gone", which is no entry of the Bundle`},
		{"RESTful owner keeps base resolution", func(b map[string]any) {
			b["entry"].([]any)[2].(map[string]any)["resource"].(map[string]any)["patient"] = map[string]any{"reference": "https://payer.test/fhir/Patient/p"}
			b["entry"].([]any)[2].(map[string]any)["fullUrl"] = "https://elsewhere.test/fhir/Claim/c"
			b["entry"].([]any)[0].(map[string]any)["resource"].(map[string]any)["request"] = map[string]any{"reference": "https://elsewhere.test/fhir/Claim/c"}
		}, ""},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			b := shaped()
			row.mutate(b)
			raw, _ := json.Marshal(b)
			got := ""
			if r := pasResponseSubjectMismatch(raw); r != nil {
				got = r.Why
			}
			if got != row.want {
				t.Fatalf("mismatch cause:\n got %q\nwant %q", got, row.want)
			}
			if consistentPASResponseSubjects(raw) != (row.want == "") {
				t.Fatal("the boolean read disagrees with the cause")
			}
		})
	}
}
