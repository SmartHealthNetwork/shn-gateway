package engine

import (
	"encoding/json"
	"fmt"
	"strings"
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
			// The read is the same at the lines that close the graph: 2.0.1
			// and 2.2.1 (whose only exemption is the request Claim, which
			// every row here carries).
			for _, line := range []string{"2.0", "2.2"} {
				at := ""
				if r := pasAnswerSubjectMismatch(raw, line); r != nil {
					at = r.Why
				}
				if at != got {
					t.Fatalf("at %s the cause is %q, want the line-less %q", line, at, got)
				}
			}
		})
	}
}

// At 2.1.0 an answer may return anything the request sent as a reference only,
// so the walk reads a subject no entry resolves by the patient its
// Patient/<id> names, and the Bundle need not carry the ClaimResponse's
// patient. A Patient it does carry, and every subject, must still be that
// patient.
func TestPASAnswerSubjectMismatchAt21(t *testing.T) {
	const crURN = "urn:uuid:10000000-0000-4000-8000-000000000001"
	drop := func(typ string) func(b map[string]any) {
		return func(b map[string]any) {
			var kept []any
			for _, e := range b["entry"].([]any) {
				if e.(map[string]any)["resource"].(map[string]any)["resourceType"] != typ {
					kept = append(kept, e)
				}
			}
			b["entry"] = kept
		}
	}
	underURN := func(b map[string]any) {
		cr := b["entry"].([]any)[0].(map[string]any)
		cr["fullUrl"] = crURN
		delete(cr["resource"].(map[string]any), "id")
	}
	addCoverage := func(beneficiary string) func(b map[string]any) {
		return func(b map[string]any) {
			b["entry"] = append(b["entry"].([]any), map[string]any{"fullUrl": "urn:uuid:10000000-0000-4000-8000-000000000002", "resource": map[string]any{
				"resourceType": "Coverage", "status": "active", "beneficiary": map[string]any{"reference": beneficiary}}})
		}
	}
	both := func(fs ...func(map[string]any)) func(map[string]any) {
		return func(b map[string]any) {
			for _, f := range fs {
				f(b)
			}
		}
	}
	for _, row := range []struct {
		name   string
		mutate func(map[string]any)
		at20   bool // the line-less (2.0.1) read binds it too
		bound  bool
		why    string // part of the 2.1 refusal, when it is pinned
	}{
		{"complete", func(map[string]any) {}, true, true, ""},
		{"the Patient returned as a reference only", drop("Patient"), false, true, ""},
		{"the ClaimResponse's patient no entry resolves, a subject naming it by id", both(underURN, drop("Patient")), false, true, ""},
		{"a subject no entry resolves, naming another patient", both(underURN, drop("Patient"), addCoverage("Patient/q")), false, false, ""},
		{"a subject naming another patient", func(b map[string]any) {
			b["entry"].([]any)[2].(map[string]any)["resource"].(map[string]any)["patient"] = map[string]any{"reference": "Patient/q"}
		}, false, false, ""},
		{"another Patient entry", func(b map[string]any) {
			b["entry"] = append(b["entry"].([]any), map[string]any{"fullUrl": "https://payer.test/fhir/Patient/q", "resource": map[string]any{"resourceType": "Patient", "id": "q"}})
		}, false, false, ""},
		// A reference that resolves to more than one entry is ambiguous at
		// every line, never read by its id alone.
		{"the ClaimResponse's patient resolving to two entries", both(underURN, func(b map[string]any) {
			b["entry"] = append(b["entry"].([]any), map[string]any{"fullUrl": "https://other.test/fhir/Patient/p", "resource": map[string]any{"resourceType": "Patient", "id": "p"}})
		}), false, false, `/patient "Patient/p" is relative under entry 0 (ClaimResponse ` + crURN + `), which is identified by a URN, and 2 entries have a RESTful identity ending in Patient/p`},
		{"a subject resolving to two entries", both(underURN, func(b map[string]any) {
			b["entry"] = append(b["entry"].([]any),
				map[string]any{"fullUrl": "urn:uuid:10000000-0000-4000-8000-000000000004", "resource": map[string]any{
					"resourceType": "Coverage", "status": "active", "beneficiary": map[string]any{"reference": "Patient/p"}}},
				map[string]any{"fullUrl": "https://other.test/fhir/Patient/p", "resource": map[string]any{"resourceType": "Patient", "id": "p"}})
			b["entry"].([]any)[0].(map[string]any)["resource"].(map[string]any)["patient"] = map[string]any{"reference": "https://payer.test/fhir/Patient/p"}
		}), false, false, "/beneficiary names \"Patient/p\", which is relative under entry 3 (Coverage urn:uuid:10000000-0000-4000-8000-000000000004), which is identified by a URN, and 2 entries have a RESTful identity ending in Patient/p"},
		// Read by its id alone, a subject is Patient/<id> exactly: in
		// "Group/Patient/p" the segment before the id is no Patient type.
		{"a subject naming Group/Patient/<the patient's id>", both(underURN, drop("Patient"), func(b map[string]any) {
			b["entry"] = append(b["entry"].([]any), map[string]any{"fullUrl": "urn:uuid:10000000-0000-4000-8000-000000000003", "resource": map[string]any{
				"resourceType": "Coverage", "status": "active", "beneficiary": map[string]any{"reference": "Group/Patient/p"}}})
		}), false, false, "/beneficiary names \"Group/Patient/p\""},
		// Bound by an id alone, the patient is one identity: the same id at
		// another base is another patient.
		{"subjects naming the patient's id at two bases", both(underURN, drop("Patient"), drop("Claim"), func(b map[string]any) {
			b["entry"].([]any)[0].(map[string]any)["resource"].(map[string]any)["request"] = map[string]any{"identifier": map[string]any{"system": "urn:example:claims", "value": "c-1"}}
			for i, base := range []string{"https://a.test/fhir", "https://b.test/fhir"} {
				id := fmt.Sprintf("cov-%d", i)
				b["entry"] = append(b["entry"].([]any), map[string]any{"fullUrl": base + "/Coverage/" + id, "resource": map[string]any{
					"resourceType": "Coverage", "id": id, "status": "active", "beneficiary": map[string]any{"reference": "Patient/p"}}})
			}
		}), false, false, ""},
		{"a subject naming the patient's id at one base", both(underURN, drop("Patient"), drop("Claim"), func(b map[string]any) {
			b["entry"].([]any)[0].(map[string]any)["resource"].(map[string]any)["request"] = map[string]any{"identifier": map[string]any{"system": "urn:example:claims", "value": "c-1"}}
			b["entry"] = append(b["entry"].([]any), map[string]any{"fullUrl": "https://a.test/fhir/Coverage/cov", "resource": map[string]any{
				"resourceType": "Coverage", "id": "cov", "status": "active", "beneficiary": map[string]any{"reference": "Patient/p"}}})
		}), false, true, ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			b := assemblySmallGraph()
			row.mutate(b)
			raw, _ := json.Marshal(b)
			r := pasAnswerSubjectMismatch(raw, "2.1")
			if (r == nil) != row.bound {
				t.Fatalf("at 2.1 bound=%v (%v), want %v", r == nil, r, row.bound)
			}
			if row.why != "" && !strings.Contains(r.Why, row.why) {
				t.Fatalf("at 2.1 the refusal is %q, want it to say %q", r.Why, row.why)
			}
			if r := pasResponseSubjectMismatch(raw); (r == nil) != row.at20 {
				t.Fatalf("line-less bound=%v (%v), want %v", r == nil, r, row.at20)
			}
		})
	}
}

// An answer is read at its candidate lines (FR-G29: valid on any line is
// valid), the patient walk at the line its graph rule held on; a rule one line
// states (PAS 2.2.1 §spec-44) refuses only where every candidate line carries
// it.
func TestAnswerReadAtCandidateLines(t *testing.T) {
	// The reference payer's real pended answer, its Patient returned as a
	// reference only: incomplete at 2.0.1 and 2.2.1, complete at 2.1.0.
	var b map[string]any
	if err := json.Unmarshal([]byte(assemblyRealPending), &b); err != nil {
		t.Fatal(err)
	}
	if _, lr := validateNativePASResponseAt([]byte(assemblyRealPending), "2.0"); lr.Status != 0 {
		t.Fatalf("fixture: the unmutated answer is refused: %s", lr.Message)
	}
	var kept []any
	for _, e := range b["entry"].([]any) {
		if e.(map[string]any)["resource"].(map[string]any)["resourceType"] != "Patient" {
			kept = append(kept, e)
		}
	}
	b["entry"] = kept
	incomplete, _ := json.Marshal(b)
	for _, row := range []struct {
		name  string
		lines []string
		line  string
		tried int
	}{
		{"2.0.1 only: refused at it", []string{"2.0"}, "2.0", 1},
		{"routed 2.2, every line laned: read at 2.1", []string{"2.2", "2.1", "2.0"}, "2.1", 2},
		{"no candidate line: the line-less rule", nil, "", 0},
	} {
		t.Run(row.name, func(t *testing.T) {
			read := answerGraphRead(row.lines, incomplete)
			if read.Line != row.line || len(read.Tried) != row.tried {
				t.Fatalf("read %+v, want line %q after %d tried", read, row.line, row.tried)
			}
			_, lr := validateNativePASResponseAt(incomplete, read.Line)
			if refused := lr.Status != 0; refused != (row.line != "2.1") {
				t.Fatalf("at %q refused=%v", read.Line, refused)
			}
			if r := pasAnswerSubjectMismatch(incomplete, read.Line); (r == nil) != (row.line == "2.1") {
				t.Fatalf("the walk at %q: %v", read.Line, r)
			}
		})
	}
	// The finding names the line read at and every line tried before it.
	read := answerGraphRead([]string{"2.2", "2.1", "2.0"}, incomplete)
	f, ok := laterLineFinding(findingContext{LegType: "pas-claim", Whose: "peer"}, "observe", RuleAnswerShape, read, incomplete)
	if !ok || f.Line != "2.1" || f.DeclaredLine != "2.2" || f.Verdict != "valid" || f.Decision != Record.String() || len(f.Lines) != 2 || f.Lines[0].Verdict != "structural" {
		t.Fatalf("finding %+v (ok=%v)", f, ok)
	}
	if _, ok := laterLineFinding(findingContext{}, "observe", RuleAnswerShape, answerGraphRead([]string{"2.1", "2.0"}, incomplete), incomplete); ok {
		t.Fatal("an answer read at its first line recorded a later-line finding")
	}

	inquiry := func(request map[string]any) []byte {
		cr := map[string]any{"resourceType": "ClaimResponse", "id": "cr", "patient": map[string]any{"reference": "Patient/p"}}
		if request != nil {
			cr["request"] = request
		}
		out, _ := json.Marshal(map[string]any{"resourceType": "Bundle", "type": "collection", "entry": []any{
			map[string]any{"fullUrl": "https://payer.test/fhir/ClaimResponse/cr", "resource": cr}}})
		return out
	}
	dar := map[string]any{"extension": []any{map[string]any{"url": "http://hl7.org/fhir/StructureDefinition/data-absent-reason", "valueCode": "unknown"}}}
	for _, row := range []struct {
		name   string
		answer []byte
		ok22   bool
	}{
		{"names its Claim by reference", inquiry(map[string]any{"reference": "Claim/c"}), true},
		{"names its Claim by identifier", inquiry(map[string]any{"identifier": map[string]any{"system": "urn:example:claims", "value": "c-1"}}), true},
		{"a Data Absent Reason", inquiry(dar), true},
		{"names no Claim", inquiry(nil), false},
		{"names an Organization, not a Claim", inquiry(map[string]any{"reference": "Organization/ins"}), false},
		{"names its Claim by the urn:uuid of a Claim entry", func() []byte {
			var b map[string]any
			_ = json.Unmarshal(inquiry(map[string]any{"reference": "urn:uuid:20000000-0000-4000-8000-000000000002"}), &b)
			b["entry"] = append(b["entry"].([]any), map[string]any{"fullUrl": "urn:uuid:20000000-0000-4000-8000-000000000002", "resource": map[string]any{"resourceType": "Claim", "status": "active"}})
			out, _ := json.Marshal(b)
			return out
		}(), true},
		{"names the urn:uuid of an Organization entry", func() []byte {
			var b map[string]any
			_ = json.Unmarshal(inquiry(map[string]any{"reference": "urn:uuid:20000000-0000-4000-8000-000000000003"}), &b)
			b["entry"] = append(b["entry"].([]any), map[string]any{"fullUrl": "urn:uuid:20000000-0000-4000-8000-000000000003", "resource": map[string]any{"resourceType": "Organization", "name": "x"}})
			out, _ := json.Marshal(b)
			return out
		}(), false},
		{"names no Claim, inside Parameters", func() []byte {
			out, _ := json.Marshal(map[string]any{"resourceType": "Parameters", "parameter": []any{map[string]any{"name": "return", "resource": json.RawMessage(inquiry(nil))}}})
			return out
		}(), false},
	} {
		t.Run("inquiry/"+row.name, func(t *testing.T) {
			if got := validatePASInquiryAnswerAt(row.answer, "2.2").Status == 0; got != row.ok22 {
				t.Fatalf("at 2.2 read=%v, want %v", got, row.ok22)
			}
			for _, line := range []string{"2.0", "2.1", ""} {
				if lr := validatePASInquiryAnswerAt(row.answer, line); lr.Status != 0 {
					t.Fatalf("at %q refused: %s", line, lr.Message)
				}
			}
			// §spec-44 refuses only where 2.2 is the only candidate line.
			only := inquiryAnswerRead([]string{"2.2"}, row.answer)
			if refused := validatePASInquiryAnswerAt(row.answer, only.Line).Status != 0; refused == row.ok22 {
				t.Fatalf("2.2 only: refused=%v", refused)
			}
			every := inquiryAnswerRead([]string{"2.2", "2.1", "2.0"}, row.answer)
			if validatePASInquiryAnswerAt(row.answer, every.Line).Status != 0 {
				t.Fatalf("every line laned: refused at %q", every.Line)
			}
		})
	}
}
