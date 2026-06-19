package engine

import (
	"strings"
	"testing"
)

// TestBuildPopulateParameters_UsesSubjectFHIRRef: the $populate subject is the FHIR-store ref
// (a scoped id), not the logical SHN ref — so the engine's CQL retrieves hit the right
// compartment. Falls back to PatientRef when SubjectFHIRRef is empty.
func TestBuildPopulateParameters_UsesSubjectFHIRRef(t *testing.T) {
	q := []byte(`{"resourceType":"Questionnaire","url":"u"}`)
	b, err := buildPopulateParameters(q, PopulateContext{PatientRef: "Patient/MBR-COVERED", SubjectFHIRRef: "Patient/pat-mbrcovered-provider"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(string(b), `"reference":"Patient/pat-mbrcovered-provider"`) {
		t.Fatalf("subject is not the FHIR ref:\n%s", b)
	}
	if strings.Contains(string(b), `"reference":"Patient/MBR-COVERED"`) {
		t.Fatalf("used the logical ref instead of the FHIR ref:\n%s", b)
	}
	b2, err := buildPopulateParameters(q, PopulateContext{PatientRef: "Patient/MBR-COVERED"})
	if err != nil {
		t.Fatalf("build (fallback): %v", err)
	}
	if !strings.Contains(string(b2), `"reference":"Patient/MBR-COVERED"`) {
		t.Fatalf("empty SubjectFHIRRef did not fall back to PatientRef:\n%s", b2)
	}
}

// TestSetQuestionnaireResponseSubject: rewrites subject.reference, preserves other fields.
func TestSetQuestionnaireResponseSubject(t *testing.T) {
	in := []byte(`{"resourceType":"QuestionnaireResponse","status":"in-progress","subject":{"reference":"Patient/pat-x-provider"},"item":[{"linkId":"a"}]}`)
	out := setQuestionnaireResponseSubject(in, "Patient/MBR-COVERED")
	subj, err := questionnaireResponseSubject(out)
	if err != nil || subj != "Patient/MBR-COVERED" {
		t.Fatalf("subject = %q (err=%v), want Patient/MBR-COVERED", subj, err)
	}
	for _, keep := range []string{`"status":"in-progress"`, `"linkId":"a"`, `"resourceType":"QuestionnaireResponse"`} {
		if !strings.Contains(string(out), keep) {
			t.Fatalf("normalization dropped %q:\n%s", keep, out)
		}
	}
}
