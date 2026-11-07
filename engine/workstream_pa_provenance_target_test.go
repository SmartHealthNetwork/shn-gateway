//go:build !shn_coreonly

package engine

import "testing"

// The payer's gateway reads an update's Provenance target by the SDK
// Responder's rule (shnsdk.ProvenanceTargetNames): exactly the relative
// "<type>/<id>" or exactly the supplemental entry's own fullUrl, version
// specific only at the resource's own meta.versionId. A version the resource
// does not carry, or a reference under another base, names no entry of the
// Bundle, whatever its path ends in. A target the rule accepts is no defect, so
// no level records or refuses it; one it does not accept is the
// pas.update-provenance defect, decided by the level as ever.
func TestUpdateProvenanceDefect_TargetRule(t *testing.T) {
	const own = "https://shn.example/fhir/DiagnosticReport/dr-1"
	report := func(version string, targets ...string) conformantUpdateFacts {
		return conformantUpdateFacts{
			provenanceJSON: []byte(`{}`), provenanceAgents: []string{"Organization/provider"},
			provenanceTargets: targets, hasDR: true, diagnosticReportID: "dr-1", diagnosticReportVersion: version,
			diagnosticReportFullURL: own,
		}
	}
	const notTargeted = "ClaimUpdate Provenance does not target the supplemental data"
	for _, row := range []struct {
		name  string
		facts conformantUpdateFacts
		want  string
	}{
		{"unversioned", report("2", "DiagnosticReport/dr-1"), ""},
		{"the report's own version", report("2", "DiagnosticReport/dr-1/_history/2"), ""},
		{"the report entry's own fullUrl", report("2", own), ""},
		{"the report entry's own fullUrl, at its own version", report("2", own+"/_history/2"), ""},
		{"another version", report("2", "DiagnosticReport/dr-1/_history/3"), notTargeted},
		{"a version of a report that carries none", report("", "DiagnosticReport/dr-1/_history/7"), notTargeted},
		{"no version, on a report that carries none", report("", "DiagnosticReport/dr-1"), ""},
		{"another report", report("2", "DiagnosticReport/dr-2/_history/2"), notTargeted},
		{"the same type and id under another base", report("2", "https://facility.example/fhir/DiagnosticReport/dr-1"), notTargeted},
		{"the same type and id under another base, versioned", report("2", "https://facility.example/fhir/DiagnosticReport/dr-1/_history/2"), notTargeted},
		{"a longer type name ending in the type", report("2", "https://shn.example/fhir/SupplementalDiagnosticReport/dr-1"), notTargeted},
		{"the questionnaire response's own version", conformantUpdateFacts{
			provenanceJSON: []byte(`{}`), provenanceAgents: []string{"Organization/provider"},
			provenanceTargets: []string{"QuestionnaireResponse/qr-1/_history/5"}, qrID: "qr-1", qrVersion: "5",
		}, ""},
	} {
		if got := updateProvenanceDefect(row.facts); got != row.want {
			t.Errorf("%s: %q, want %q", row.name, got, row.want)
		}
	}
	// The facts carry each supplemental entry's fullUrl and version as the
	// Bundle states them, read by exact member name.
	bundle := []byte(`{"resourceType":"Bundle","type":"collection","entry":[` +
		`{"fullUrl":"` + own + `","resource":{"resourceType":"DiagnosticReport","id":"dr-1","meta":{"versionId":"2"}}},` +
		`{"fullUrl":"https://shn.example/fhir/QuestionnaireResponse/qr-1","resource":{"resourceType":"QuestionnaireResponse","id":"qr-1","meta":{"VersionId":"9"}}}]}`)
	f, status, msg := parseConformantPASUpdateFacts(bundle)
	if status != 0 {
		t.Fatalf("%d %s", status, msg)
	}
	if f.diagnosticReportFullURL != own || f.diagnosticReportVersion != "2" || f.qrFullURL != "https://shn.example/fhir/QuestionnaireResponse/qr-1" || f.qrVersion != "" {
		t.Errorf("facts %+v", f)
	}
}
