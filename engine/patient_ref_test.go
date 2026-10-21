package engine

// patient_ref_test.go — a patient reference is read the same way in every
// form FHIR allows: relative, absolute (with or without a version) and
// contained. The eligibility and federated-query legs read the member it
// names, never the literal reference.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func TestPatientRefMember(t *testing.T) {
	member := map[string]any{"resourceType": "Patient", "id": "p1", "identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": "MBR-X"}}}
	byID := map[string]any{"resourceType": "Patient", "id": "MBR-Y"}
	other := map[string]any{"resourceType": "Organization", "id": "o1"}
	contained := []map[string]any{member, byID, other}
	for _, row := range []struct {
		ref, want string
		ok        bool
	}{
		{"Patient/MBR-X", "MBR-X", true},
		{"https://provider.example/fhir/Patient/MBR-X", "MBR-X", true},
		{"https://provider.example/fhir/Patient/MBR-X/_history/2", "MBR-X", true},
		{"#p1", "MBR-X", true}, // a contained Patient's member identifier
		{"#MBR-Y", "", false},  // no member identifier: a contained id is local, never a member
		{"#p9", "", false},     // names no contained resource
		{"#o1", "", false},     // names a contained resource that is no Patient
		{"Organization/o1", "", false},
		{"https://provider.example/fhir/Organization/o1", "", false},
		{"", "", false},
		{"#", "", false},
		// Not a reference's shape: a bare id, a trailing path or operation, an
		// escaped character, an encoded absolute reference.
		{"MBR-X", "", false},
		{"Patient/MBR-X/", "", false},
		{"Patient/MBR-X/$everything", "", false},
		{"Patient/MBR-X/%zz", "", false},
		{"Patient/MBR-X/Observation/1", "", false},
		{"https://x/Patient/MBR-X/x%26date%3Dge1900-01-01", "", false},
		{"https%3A%2F%2Fx%2FPatient%2FMBR-X", "", false},
		{"Patient/MBR-X/_history/", "", false},
		// A character outside a FHIR id, in the id or the base.
		{"Patient/MBR-X%41", "", false},
		{"Patient/A$x", "", false},
		{"https://x/a%2Fb/Patient/MBR-X", "", false},
		// A contained reference is "#<id>" and nothing else.
		{"Patient/Y#p1", "", false},
		{"x#p1", "", false},
		{"#p1/x", "", false},
		{"#p1/Observation/1", "", false},
		// An id is at most 64 characters.
		{"Patient/" + strings.Repeat("a", 64), strings.Repeat("a", 64), true},
		{"Patient/" + strings.Repeat("a", 65), "", false},
		{"#" + strings.Repeat("a", 65), "", false},
	} {
		got, ok := patientRefMember(row.ref, contained)
		if got != row.want || ok != row.ok {
			t.Errorf("patientRefMember(%q) = (%q, %v), want (%q, %v)", row.ref, got, ok, row.want, row.ok)
		}
	}
	// Two contained Patients with one id name more than one patient.
	if got, ok := patientRefMember("#p1", []map[string]any{member, member}); ok {
		t.Errorf("a repeated contained id resolved to %q", got)
	}
	// A contained resource with no id is never named.
	noID := map[string]any{"resourceType": "Patient"}
	// A contained resource that is not a Patient names no patient, whatever
	// member identifier it carries.
	related := map[string]any{"resourceType": "RelatedPerson", "id": "r1", "identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": "MBR-X"}}}
	if got, ok := patientRefMember("#r1", []map[string]any{related}); ok {
		t.Errorf("a contained RelatedPerson resolved to %q", got)
	}
	// An identifier from another system (a record number) is no member id.
	mrn := map[string]any{"resourceType": "Patient", "id": "p6", "identifier": []any{map[string]any{"system": "urn:mrn", "value": "MBR-OTHER"}}}
	if got, ok := patientRefMember("#p6", []map[string]any{mrn}); ok {
		t.Errorf("a contained Patient with only a record number resolved to %q", got)
	}
	// A member identifier's value is an identifier's, not limited to an id's
	// shape (the federated query writes only an id-shaped one into its
	// query); a blank one names no one, wherever it sits.
	smuggle := map[string]any{"resourceType": "Patient", "id": "p4", "identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": "A&date=ge1900-01-01"}}}
	if got, ok := patientRefMember("#p4", []map[string]any{smuggle}); !ok || got != "A&date=ge1900-01-01" {
		t.Errorf("a member identifier's value resolved to (%q, %v)", got, ok)
	}
	for _, values := range [][]string{{"", "MBR-X"}, {"MBR-X", ""}} {
		var ids []any
		for _, v := range values {
			ids = append(ids, map[string]any{"system": shnsdk.MemberSystem, "value": v})
		}
		blank := map[string]any{"resourceType": "Patient", "id": "p5", "identifier": ids}
		if got, ok := patientRefMember("#p5", []map[string]any{blank}); !ok || got != "MBR-X" {
			t.Errorf("member identifiers %q resolved to (%q, %v), want MBR-X", values, got, ok)
		}
	}
	twoMembers := map[string]any{"resourceType": "Patient", "id": "p3", "identifier": []any{
		map[string]any{"system": shnsdk.MemberSystem, "value": "MBR-X"}, map[string]any{"system": shnsdk.MemberSystem, "value": "MBR-Z"}}}
	if got, ok := patientRefMember("#p3", []map[string]any{twoMembers}); ok {
		t.Errorf("a contained Patient naming two members resolved to %q", got)
	}
	odd := map[string]any{"resourceType": "Patient", "id": "p2", "identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": "A/$everything"}}}
	if got, ok := patientRefMember("#", []map[string]any{noID, odd}); ok {
		t.Errorf("patientRefMember(\"#\") = %q, want none", got)
	}
	// The odd identifier is read as it is: the federated query writes only an
	// id-shaped member into its query (TestFederatedQuery_MalformedPatientReferenceIsRefused).
	if got, ok := patientRefMember("#p2", []map[string]any{noID, odd}); !ok || got != "A/$everything" {
		t.Errorf("patientRefMember(\"#p2\") = (%q, %v), want the identifier's value", got, ok)
	}
}

// eligibilityWithPatient is the eligibility request for member with its
// patient named by ref, and contained, when set, carried as its contained
// resources.
func eligibilityWithPatient(t *testing.T, member, ref string, contained ...map[string]any) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(eligibilityRequest(t, member), &doc); err != nil {
		t.Fatal(err)
	}
	doc["patient"] = map[string]any{"reference": ref}
	if len(contained) > 0 {
		doc["contained"] = contained
	}
	return mustJSON(t, doc)
}

func containedMember(id, member string) map[string]any {
	return map[string]any{"resourceType": "Patient", "id": id, "identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": member}}}
}

// patientForms are a member's patient reference in each form, with what each
// form needs contained.
func patientForms(member string) map[string]struct {
	ref       string
	contained []map[string]any
} {
	return map[string]struct {
		ref       string
		contained []map[string]any
	}{
		"relative":  {"Patient/" + member, nil},
		"absolute":  {"https://provider.example/fhir/Patient/" + member, nil},
		"versioned": {"https://provider.example/fhir/Patient/" + member + "/_history/3", nil},
		"contained": {"#pat", []map[string]any{containedMember("pat", member)}},
	}
}

// Eligibility, answered from the payer's records: every form of the
// request's own member is answered about that member and binds to the token's
// patient (nothing differs), at every level.
func TestEligibility_EveryPatientReferenceFormNamesItsMember(t *testing.T) {
	for name, form := range patientForms(dtrFrameMember) {
		for _, level := range eligibilityLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p := eligibilityPayer(t, level, false)
				run := runObserved(t, p, "coverage-eligibility", eligibilityWithPatient(t, dtrFrameMember, form.ref, form.contained...), p.pci)
				if run.answer.status != http.StatusOK {
					t.Fatalf("answer %d %s, want answered", run.answer.status, run.answer.body)
				}
				if ref, err := ParseCoverageEligibilityResponsePatient(run.answer.body); err != nil || ref != "Patient/"+dtrFrameMember {
					t.Fatalf("the answer is about %q (%v), want the request's member", ref, err)
				}
				if run.differs != 0 {
					t.Fatalf("the member was bound to another patient (%d binding-differs events)", run.differs)
				}
			})
		}
	}
}

// Eligibility, forwarded to the payer's own endpoint: every form is carried
// byte for byte and binds to the token's patient, at every level.
func TestEligibility_ForwardedPatientReferenceFormsNameTheirMember(t *testing.T) {
	for name, form := range patientForms(dtrFrameMember) {
		for _, level := range eligibilityLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p := eligibilityPayer(t, level, true)
				answer := payersEligibilityAnswer("Patient/" + dtrFrameMember)
				p.partner.respByPath[eligibilityPath] = answer
				body := eligibilityWithPatient(t, dtrFrameMember, form.ref, form.contained...)
				run := runObserved(t, p, "coverage-eligibility", body, p.pci)
				if run.answer.status != http.StatusOK || !bytes.Equal(run.answer.body, answer) {
					t.Fatalf("answer %d %s, want the payer's answer relayed", run.answer.status, run.answer.body)
				}
				if !bytes.Equal(p.partner.lastBody, body) {
					t.Fatalf("the payer's endpoint received\n%s\nwant the request as sent\n%s", p.partner.lastBody, body)
				}
				if run.differs != 0 {
					t.Fatalf("the member was bound to another patient (%d binding-differs events)", run.differs)
				}
			})
		}
	}
}

// Every form naming another patient than the token's is carried as the
// relative one is: answered about the patient it names, and the difference
// observed.
func TestEligibility_PatientReferenceFormsToAnotherPatientAreCarried(t *testing.T) {
	for name, form := range patientForms(anotherHeldMember) {
		t.Run(name, func(t *testing.T) {
			p := eligibilityPayer(t, EnforcementStrict, false)
			run := runObserved(t, p, "coverage-eligibility", eligibilityWithPatient(t, anotherHeldMember, form.ref, form.contained...), p.pci)
			if ref, err := ParseCoverageEligibilityResponsePatient(run.answer.body); run.answer.status != http.StatusOK || err != nil || ref != "Patient/"+anotherHeldMember {
				t.Fatalf("answer %d about %q (%v), want carried about %s", run.answer.status, ref, err, anotherHeldMember)
			}
			if run.differs != 1 {
				t.Fatalf("%d binding-differs events, want the difference observed once", run.differs)
			}
		})
	}
}

// A contained reference that names no contained Patient, or more than one,
// names no patient: refused before the payer's records are read, at every
// level.
func TestEligibility_UnresolvablePatientReferenceIsRefused(t *testing.T) {
	for name, row := range map[string]struct {
		ref       string
		contained []map[string]any
	}{
		"contained id not carried": {"#nobody", []map[string]any{containedMember("pat", dtrFrameMember)}},
		"contained twice":          {"#pat", []map[string]any{containedMember("pat", dtrFrameMember), containedMember("pat", anotherHeldMember)}},
		"contained, trailing path": {"#pat/x", []map[string]any{containedMember("pat", dtrFrameMember)}},
	} {
		for _, level := range eligibilityLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p := eligibilityPayer(t, level, false)
				ans := p.sendAs(t, "coverage-eligibility", "", eligibilityWithPatient(t, dtrFrameMember, row.ref, row.contained...), p.pci)
				if ans.status != http.StatusBadRequest || !strings.Contains(string(ans.body), "parse member failed") {
					t.Fatalf("answer %d %s, want 400 parse member failed", ans.status, ans.body)
				}
			})
		}
	}
}

// Any other reference that is not a well-formed one is read as before, by the
// SDK's reader, which takes what follows "Patient/" as the member: a member
// id is an identifier value, not limited to a FHIR id's shape. At every level.
func TestEligibility_OtherReferencesAreReadAsBefore(t *testing.T) {
	for name, row := range map[string]struct {
		ref      string
		answered bool // the SDK's reader names a held member
	}{
		"bare id":              {dtrFrameMember, true},
		"not a Patient":        {"Organization/payer", false},
		"underscore in the id": {"Patient/MBR_1", false},
		"trailing slash":       {"Patient/" + dtrFrameMember + "/", false},
		"lowercase type":       {"patient/" + dtrFrameMember, false},
		"id over 64":           {"Patient/" + strings.Repeat("a", 65), false},
	} {
		for _, level := range eligibilityLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p := eligibilityPayer(t, level, false)
				ans := p.sendAs(t, "coverage-eligibility", "", eligibilityWithPatient(t, dtrFrameMember, row.ref), p.pci)
				if row.answered {
					if ref, err := ParseCoverageEligibilityResponsePatient(ans.body); ans.status != http.StatusOK || err != nil || ref != "Patient/"+dtrFrameMember {
						t.Fatalf("answer %d %s, want answered about the member, as before", ans.status, ans.body)
					}
					return
				}
				if ans.status != http.StatusBadRequest || strings.TrimSpace(string(ans.body)) != `{"error":"unknown member"}` {
					t.Fatalf("answer %d %s, want the payer's 400 unknown member, as before", ans.status, ans.body)
				}
			})
		}
	}
}

// cdexTaskWithPatient is the federated query for member with Task.for named
// forRef and the query's patient parameter named queryRef.
func cdexTaskWithPatient(t *testing.T, member, forRef, queryRef string, contained ...map[string]any) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(cdexTaskFor(t, member), &doc); err != nil {
		t.Fatal(err)
	}
	doc["for"] = map[string]any{"reference": forRef}
	if len(contained) > 0 {
		doc["contained"] = contained
	}
	inputs, _ := doc["input"].([]any)
	rewrote := false
	for _, in := range inputs {
		m, _ := in.(map[string]any)
		if q, ok := m["valueString"].(string); ok && strings.Contains(q, "patient=Patient/"+member) {
			m["valueString"] = strings.Replace(q, "patient=Patient/"+member, "patient="+queryRef, 1)
			rewrote = true
		}
	}
	if !rewrote {
		t.Fatal("the federated query names no patient parameter to rewrite")
	}
	return mustJSON(t, doc)
}

// A federated query names the consented patient in any form, in Task.for and
// in its query: the bind passes and the facility reads the member's records,
// at every level. One naming another patient in any form is refused with the
// relative form's answer.
func TestFederatedQuery_PatientReferenceForms(t *testing.T) {
	abs := func(m string) string { return "https://provider.example/fhir/Patient/" + m }
	for name, form := range map[string]struct {
		forRef, queryRef func(string) string
		contained        func(string) []map[string]any
	}{
		"absolute":  {abs, abs, nil},
		"versioned": {func(m string) string { return abs(m) + "/_history/2" }, func(m string) string { return abs(m) + "/_history/2" }, nil},
		"contained for and query": {func(string) string { return "#pat" }, func(string) string { return "#pat" },
			func(m string) []map[string]any { return []map[string]any{containedMember("pat", m)} }},
		"absolute for, relative query": {abs, func(m string) string { return "Patient/" + m }, nil},
		"contained for, relative query": {func(string) string { return "#pat" }, func(m string) string { return "Patient/" + m },
			func(m string) []map[string]any { return []map[string]any{containedMember("pat", m)} }},
	} {
		for _, level := range bindLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p, got := newRecordingLevelPayer(t, level)
				p.g.cfg.ConsentURL = "http://consent.test"
				p.g.cfg.Client = &http.Client{Transport: consentPermits{inner: p.g.cfg.Client.Transport}}
				reads := &recordReadCounter{SystemOfRecord: p.g.cfg.SoR, ContextSystemOfRecord: ReadSystemOfRecord(p.g.cfg.SoR)}
				p.g.cfg.SoR = reads
				task := func(m string) []byte {
					var c []map[string]any
					if form.contained != nil {
						c = form.contained(m)
					}
					return cdexTaskWithPatient(t, m, form.forRef(m), form.queryRef(m), c...)
				}
				own := sendConsented(t, p, "federated-query", task(dtrFrameMember), p.pci, "Consent/c-1")
				if bytes.Contains(own.body, []byte("query rejected")) || bytes.Contains(own.body, []byte(refusalPatientNotConsented)) || reads.n.Load() == 0 {
					t.Fatalf("answer %d %s after %d record reads, want the bind passed and the records read", own.status, own.body, reads.n.Load())
				}
				reads.n.Store(0)
				other, _, _ := p.store.ResolvePatient(anotherHeldMember)
				got.reset()
				relative := sendConsented(t, p, "federated-query", cdexTaskFor(t, dtrFrameMember), other, "Consent/c-1")
				got.reset()
				form := sendConsented(t, p, "federated-query", task(dtrFrameMember), other, "Consent/c-1")
				wantRefusal(t, got.only(t), form.status, RefusedByPayerGateway, RefusalConsent)
				if form.status != relative.status || !bytes.Equal(form.body, relative.body) {
					t.Fatalf("another patient's query answered %d %s, the relative form %d %s", form.status, form.body, relative.status, relative.body)
				}
				if n := reads.n.Load(); n != 0 {
					t.Fatalf("the facility read records %d times for refused queries", n)
				}
			})
		}
	}
}

// A federated query whose Task.for or query patient is not a well-formed
// reference, or names no patient or more than one, is refused before any
// record is read, at every level: never read as a narrower query.
func TestFederatedQuery_MalformedPatientReferenceIsRefused(t *testing.T) {
	rows := map[string]struct {
		forRef, queryRef string
		contained        []map[string]any
	}{
		"operation after the id":      {"Patient/" + dtrFrameMember + "/$everything", "Patient/" + dtrFrameMember + "/$everything", nil},
		"escaped tail":                {"Patient/" + dtrFrameMember, "Patient/" + dtrFrameMember + "/%zz", nil},
		"double-escaped tail":         {"Patient/" + dtrFrameMember, "Patient/" + dtrFrameMember + "/%2525", nil},
		"smuggled date bound":         {"Patient/" + dtrFrameMember, "https://x/Patient/" + dtrFrameMember + "/x%26date%3Dge1900-01-01", nil},
		"contained member, not an id": {"#pat", "#pat", []map[string]any{containedMember("pat", "A/x")}},
		"trailing slash and resource": {"Patient/" + dtrFrameMember + "/", "Patient/" + dtrFrameMember + "/Observation/1", nil},
		"contained id not carried":    {"#nobody", "Patient/" + dtrFrameMember, []map[string]any{containedMember("pat", dtrFrameMember)}},
		"contained twice":             {"#pat", "Patient/" + dtrFrameMember, []map[string]any{containedMember("pat", dtrFrameMember), containedMember("pat", anotherHeldMember)}},
		"not a Patient":               {"Organization/payer", "Patient/" + dtrFrameMember, nil},
	}
	for name, row := range rows {
		for _, level := range bindLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p, _ := newRecordingLevelPayer(t, level)
				p.g.cfg.ConsentURL = "http://consent.test"
				p.g.cfg.Client = &http.Client{Transport: consentPermits{inner: p.g.cfg.Client.Transport}}
				reads := &recordReadCounter{SystemOfRecord: p.g.cfg.SoR, ContextSystemOfRecord: ReadSystemOfRecord(p.g.cfg.SoR)}
				p.g.cfg.SoR = reads
				ans := sendConsented(t, p, "federated-query", cdexTaskWithPatient(t, dtrFrameMember, row.forRef, row.queryRef, row.contained...), p.pci, "Consent/c-1")
				if ans.status != http.StatusForbidden || !bytes.Contains(ans.body, []byte("query rejected")) {
					t.Fatalf("answer %d %s, want 403 query rejected", ans.status, ans.body)
				}
				if n := reads.n.Load(); n != 0 {
					t.Fatalf("the facility read records %d times for a refused query", n)
				}
			})
		}
	}
}

// The narrowness check reads the federated-query Task through the SDK's
// reader after the patient references are put in the relative form. For
// every Task, what the check reads equals what the SDK's reader reads from
// the Task as carried, or, where a well-formed reference was rewritten, from
// the same Task written in the relative form. A Task with a repeated member
// name or one that only folds to a field name (the shape that could make two
// readers see two queries) is read exactly as carried.
func TestFederatedQuery_CheckReadsWhatTheSDKReads(t *testing.T) {
	base := cdexTaskFor(t, dtrFrameMember)
	other := cdexTaskFor(t, anotherHeldMember)
	query := func(task []byte) string {
		var doc map[string]any
		if err := json.Unmarshal(task, &doc); err != nil {
			t.Fatal(err)
		}
		for _, in := range doc["input"].([]any) {
			if q, ok := in.(map[string]any)["valueString"].(string); ok && strings.Contains(q, "patient=") {
				return q
			}
		}
		t.Fatal("no data query")
		return ""
	}
	q, otherQ := query(base), query(other)
	quote := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	asCarried := map[string][]byte{
		"relative": base,
		// The query under a name that only folds to valueString, ahead of the
		// real one naming another patient: the SDK's reader takes the last.
		"folded name": bytes.Replace(other, []byte(`"valueString":`+quote(otherQ)),
			[]byte(`"valueſtring":`+quote(q)+`,"valueString":`+quote(otherQ)), 1),
		// The query agreeing with Task.for under the folded name, the real one
		// naming another patient: the SDK's reader refuses the mismatch.
		"folded name, mismatched": bytes.Replace(base, []byte(`"valueString":`+quote(q)),
			[]byte(`"valueſtring":`+quote(q)+`,"valueString":`+quote(otherQ)), 1),
		"case-variant for": bytes.Replace(base, []byte(`"for":`), []byte(`"For":{"reference":"Patient/`+anotherHeldMember+`"},"for":`), 1),
		"repeated for":     bytes.Replace(base, []byte(`"for":`), []byte(`"for":{"reference":"Patient/`+anotherHeldMember+`"},"for":`), 1),
		"repeated query":   bytes.Replace(base, []byte(`"valueString":`+quote(q)), []byte(`"valueString":`+quote(otherQ)+`,"valueString":`+quote(q)), 1),
		// Ambiguous Tasks carrying a reference the check would otherwise
		// rewrite: read exactly as carried, so refused as before.
		"repeated for, absolute last": bytes.Replace(base, []byte(`"for":`),
			[]byte(`"for":{"reference":"Patient/`+dtrFrameMember+`"},"for":{"reference":"https://provider.example/fhir/Patient/`+dtrFrameMember+`"},"ignored":`), 1),
		// A name that only folds to for, not repeated: the SDK's reader takes
		// it, and refuses its absolute reference as before.
		"folded for, absolute": bytes.Replace(cdexTaskWithPatient(t, dtrFrameMember, "https://provider.example/fhir/Patient/"+dtrFrameMember, "Patient/"+dtrFrameMember),
			[]byte(`"for":`), []byte(`"For":`), 1),
		"folded contained": bytes.Replace(cdexTaskWithPatient(t, dtrFrameMember, "#pat", "Patient/"+dtrFrameMember, containedMember("pat", dtrFrameMember)),
			[]byte(`"contained":`), []byte(`"Contained":`), 1),
	}
	parse := func(task []byte) string {
		got, err := shnsdk.ParseCDexTaskDataRequest(task)
		if err != nil {
			return "error: " + err.Error()
		}
		b, _ := json.Marshal(got)
		return string(b)
	}
	for name, task := range asCarried {
		if bytes.Equal(task, base) && name != "relative" {
			t.Fatalf("%s: the fixture did not change the Task", name)
		}
		if got, want := parse(relativeCDexPatients(task)), parse(task); got != want {
			t.Errorf("%s: the check reads %s, the SDK's reader %s", name, got, want)
		}
	}
	abs := "https://provider.example/fhir/Patient/" + dtrFrameMember
	for name, task := range map[string][]byte{
		"absolute":  cdexTaskWithPatient(t, dtrFrameMember, abs, abs),
		"versioned": cdexTaskWithPatient(t, dtrFrameMember, abs+"/_history/2", "Patient/"+dtrFrameMember),
		"contained": cdexTaskWithPatient(t, dtrFrameMember, "#pat", "#pat", containedMember("pat", dtrFrameMember)),
	} {
		if got, want := parse(relativeCDexPatients(task)), parse(base); got != want {
			t.Errorf("%s: the check reads %s, want the relative form's %s", name, got, want)
		}
	}
}

// eligibilityAmbiguous is the eligibility request for member with its patient
// key repeated: first the relative reference, then last.
func eligibilityAmbiguous(t *testing.T, member, last string) []byte {
	t.Helper()
	body := eligibilityWithPatient(t, member, "Patient/"+member)
	rel := []byte(`"patient":{"reference":"Patient/` + member + `"}`)
	if !bytes.Contains(body, rel) {
		t.Fatalf("the request names its patient otherwise: %s", body)
	}
	return bytes.Replace(body, rel, append(append([]byte{}, rel...), []byte(`,"patient":{"reference":"`+last+`"}`)...), 1)
}

// An eligibility request whose patient is named twice is read two ways and
// refused, whichever form the second reference takes (no longer the SDK's
// reading of the last one), as message integrity. A body that is no
// CoverageEligibilityRequest names no member. At every level.
func TestEligibility_AmbiguousOrWrongRequestIsRefused(t *testing.T) {
	for _, level := range eligibilityLevels {
		t.Run(level.String(), func(t *testing.T) {
			for _, last := range []string{"Patient/" + dtrFrameMember, "https://provider.example/fhir/Patient/" + dtrFrameMember} {
				p := eligibilityPayer(t, level, false)
				var got exchangeRecords
				p.g.cfg.ExchangeObserved = got.observe
				ans := p.sendAs(t, "coverage-eligibility", "", eligibilityAmbiguous(t, dtrFrameMember, last), p.pci)
				if ans.status != http.StatusBadRequest || !strings.Contains(string(ans.body), `eligibility request repeats the JSON member \"patient\" in the top-level object: it can be read two ways`) {
					t.Fatalf("last %s: %d %s, want 400 naming the repeated member", last, ans.status, ans.body)
				}
				wantRefusal(t, got.only(t), http.StatusBadRequest, RefusedByPayerGateway, RefusalIntegrity)
			}
			p := eligibilityPayer(t, level, false)
			notCER := bytes.Replace(eligibilityRequest(t, dtrFrameMember), []byte(`"CoverageEligibilityRequest"`), []byte(`"Claim"`), 1)
			wrong := p.sendAs(t, "coverage-eligibility", "", notCER, p.pci)
			if wrong.status != http.StatusBadRequest || !strings.Contains(string(wrong.body), "parse member failed") {
				t.Fatalf("not a CoverageEligibilityRequest: %d %s, want 400 parse member failed", wrong.status, wrong.body)
			}
		})
	}
}

// A contained list holding something other than resources is read past: a
// relative patient reference is read as before.
func TestEligibility_ContainedNonObjectIsReadPast(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(eligibilityRequest(t, dtrFrameMember), &doc); err != nil {
		t.Fatal(err)
	}
	doc["contained"] = []any{1, "x"}
	for _, level := range eligibilityLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, false)
			ans := p.sendAs(t, "coverage-eligibility", "", mustJSON(t, doc), p.pci)
			if ref, err := ParseCoverageEligibilityResponsePatient(ans.body); ans.status != http.StatusOK || err != nil || ref != "Patient/"+dtrFrameMember {
				t.Fatalf("answer %d %s, want answered about the member", ans.status, ans.body)
			}
		})
	}
}

// A request the strict reader cannot read but the SDK's reader reads is read
// as before: its relative patient names its member, at every level. The
// strict reader's limits are not a new refusal.
func TestEligibility_StrictlyUnreadableRequestIsReadAsBefore(t *testing.T) {
	withField := func(name, raw string) []byte {
		body := eligibilityRequest(t, dtrFrameMember)
		return append(append(append([]byte{}, body[:len(body)-1]...), []byte(`,"`+name+`":`+raw)...), '}')
	}
	deep := strings.Repeat("[", 70) + strings.Repeat("]", 70)
	for name, body := range map[string][]byte{
		"contained as an object":         withField("contained", `{"resourceType":"Organization","id":"o1"}`),
		"invalid UTF-8 in another field": withField("note", "\"\xff\""),
		"nesting past the strict bound":  withField("extraNesting", deep),
	} {
		for _, level := range eligibilityLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p := eligibilityPayer(t, level, false)
				ans := p.sendAs(t, "coverage-eligibility", "", body, p.pci)
				if ref, err := ParseCoverageEligibilityResponsePatient(ans.body); ans.status != http.StatusOK || err != nil || ref != "Patient/"+dtrFrameMember {
					t.Fatalf("answer %d %s, want answered about the member as before", ans.status, ans.body)
				}
			})
		}
	}
}

// A request the strict reader cannot read for its own limits is still read
// one way only, by each gateway: one that also repeats a member name is
// refused as message integrity, and one naming its type or patient only in
// another case is refused, never bound (the patient-match rule), at every
// level.
func TestEligibility_StrictlyUnreadableRequestReadTwoWaysIsRefused(t *testing.T) {
	const patient = `"patient":{"reference":"Patient/` + dtrFrameMember + `"}`
	limits := map[string]string{
		"invalid UTF-8 in another field": "\"\xff\"",
		"nesting past the strict bound":  strings.Repeat("[", 70) + strings.Repeat("]", 70),
	}
	mutations := []struct {
		name, find, replace string
		repeats             bool
	}{
		{"patient repeated", patient, patient + `,"patient":{"reference":"Patient/` + anotherHeldMember + `"}`, true},
		{"patient repeated under case folding", patient, patient + `,"PATIENT":{"reference":"Patient/` + anotherHeldMember + `"}`, true},
		{"patient named in another case", `"patient":`, `"Patient":`, false},
		{"reference named in another case", `"reference":"Patient/`, `"Reference":"Patient/`, false},
		{"type named in another case", `"resourceType":`, `"ResourceType":`, false},
	}
	for limit, raw := range limits {
		for _, m := range mutations {
			body := eligibilityRequest(t, dtrFrameMember)
			body = append(append(append([]byte{}, body[:len(body)-1]...), []byte(`,"note":`+raw)...), '}')
			mutated := bytes.Replace(body, []byte(m.find), []byte(m.replace), 1)
			if bytes.Equal(mutated, body) {
				t.Fatalf("%s: the mutation did not apply", m.name)
			}
			for _, level := range eligibilityLevels {
				t.Run(limit+"/"+m.name+"/"+level.String(), func(t *testing.T) {
					p := eligibilityPayer(t, level, false)
					var got exchangeRecords
					p.g.cfg.ExchangeObserved = got.observe
					ans := p.sendAs(t, "coverage-eligibility", "", mutated, p.pci)
					want, rule := "member names are case-sensitive, so the request names no patient to bind", RefusalAuthority
					if m.repeats {
						want, rule = "eligibility request repeats the JSON member", RefusalIntegrity
					}
					if ans.status != http.StatusBadRequest || !strings.Contains(string(ans.body), want) {
						t.Fatalf("answer %d %s, want 400 %s", ans.status, ans.body, want)
					}
					wantRefusal(t, got.only(t), http.StatusBadRequest, RefusedByPayerGateway, rule)
				})
				t.Run("provider/"+limit+"/"+m.name+"/"+level.String(), func(t *testing.T) {
					want, rule := "member names are case-sensitive, so the request names no patient to bind", RefusalAuthority
					if m.repeats {
						want, rule = "eligibility request repeats the JSON member", RefusalIntegrity
					}
					env, rec, x := runEligibilityIngress(t, level, mutated, nil)
					refusedBeforeTheNetwork(t, env, rec, http.StatusBadRequest, want)
					wantRefusal(t, x, http.StatusBadRequest, RefusedByProviderGateway, rule)
				})
			}
		}
	}
}

// A repeat the strict reader never reaches (its limit comes first in the
// body) is still found and refused as message integrity; the refusal says a
// member name is repeated, since only that reader names which.
func TestEligibility_RepeatPastTheStrictReadersLimitIsRefused(t *testing.T) {
	const patient = `"patient":{"reference":"Patient/` + dtrFrameMember + `"}`
	cer := eligibilityRequest(t, dtrFrameMember)
	body := append([]byte("{\"note\":\"\xff\","), cer[1:]...)
	body = bytes.Replace(body, []byte(patient), []byte(patient+`,"patient":{"reference":"Patient/`+anotherHeldMember+`"}`), 1)
	for _, level := range eligibilityLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, false)
			var got exchangeRecords
			p.g.cfg.ExchangeObserved = got.observe
			ans := p.sendAs(t, "coverage-eligibility", "", body, p.pci)
			if ans.status != http.StatusBadRequest || !strings.Contains(string(ans.body), "eligibility request repeats a member name: it can be read two ways") {
				t.Fatalf("answer %d %s, want 400 naming a repeated member name", ans.status, ans.body)
			}
			wantRefusal(t, got.only(t), http.StatusBadRequest, RefusedByPayerGateway, RefusalIntegrity)
		})
	}
}

// repeatsAName finds a repeated member name wherever encoding/json reads a
// body the strict reader refuses for its own limits, and nothing else.
func TestRepeatsAName_PastTheStrictReadersLimits(t *testing.T) {
	deep := strings.Repeat("[", 70) + strings.Repeat("]", 70)
	for body, want := range map[string]bool{
		`{"a":1,"a":2}`:                                  true,
		`{"a":1,"A":2}`:                                  true,
		`{"x":` + deep + `,"k":1,"k":2}`:                 true,
		`{"x":` + deep + `,"k":1,"K":2}`:                 true,
		`{"x":` + deep + `,"o":{"k":1,"k":2}}`:           true,
		`{"x":` + deep + `,"a":[{"k":1},{"k":2,"K":3}]}`: true,
		`{"x":` + deep + `,"\u0041":1,"a":2}`:            true,
		"{\"x\":\"\xff\",\"k\":1,\"k\":2}":               true,
		`{"x":` + deep + `,"k":1}`:                       false,
		`{"x":` + deep + `,"a":[{"k":1},{"k":2}]}`:       false,
		`{"x":` + deep + `,"o":{"k":1},"p":{"k":2}}`:     false,
		`{"x":` + deep + `,"k":{"k":1}}`:                 false,
		"{\"x\":\"\xff\",\"k\":1}":                       false,
		`{"a":1,"b":2}`:                                  false,
		`{`:                                              false,
	} {
		if got := repeatsAName([]byte(body)); got != want {
			t.Errorf("repeatsAName(%q) = %v, want %v", body, got, want)
		}
	}
}

// On eligibility a contained Patient's member identifier is read whatever its
// shape, as the SDK's reader reads what follows "Patient/": a member the
// payer does not hold is its own unknown member, and the request is carried
// to a payer's own endpoint. At every level.
func TestEligibility_ContainedMemberOfAnyShapeIsRead(t *testing.T) {
	body := func(t *testing.T) []byte {
		return eligibilityWithPatient(t, dtrFrameMember, "#pat", containedMember("pat", "MBR_1"))
	}
	for _, level := range eligibilityLevels {
		t.Run("records/"+level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, false)
			ans := p.sendAs(t, "coverage-eligibility", "", body(t), p.pci)
			if ans.status != http.StatusBadRequest || strings.TrimSpace(string(ans.body)) != `{"error":"unknown member"}` {
				t.Fatalf("answer %d %s, want the payer's 400 unknown member", ans.status, ans.body)
			}
		})
		t.Run("forward/"+level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, true)
			answer := payersEligibilityAnswer("Patient/MBR_1")
			p.partner.respByPath[eligibilityPath] = answer
			sent := body(t)
			ans := p.sendAs(t, "coverage-eligibility", "", sent, p.pci)
			if ans.status != http.StatusOK || !bytes.Equal(p.partner.lastBody, sent) {
				t.Fatalf("answer %d %s, want the request carried to the payer's endpoint", ans.status, ans.body)
			}
		})
	}
}
