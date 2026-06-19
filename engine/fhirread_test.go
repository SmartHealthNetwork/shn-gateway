// fhirread_test.go — unit tests for the (C)-fence subject readers relocated from
// sdk/ into the gateway module (standalone-build closure fix). Fixtures are built
// with shnsdk builders (published v0.7.0) so the tests work against the same
// wire shapes the engine produces.
package engine

import (
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// TestParseCoverageEligibilityResponsePatient checks the (C) outbound-fence
// reader: it returns the full "Patient/<member>" ref from a CER response, and
// rejects a wrong resourceType / a response missing patient.reference.
// Round-trips against a builder-produced response so it reads the real wire shape.
func TestParseCoverageEligibilityResponsePatient(t *testing.T) {
	t0 := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)

	// Round-trip: builder-produced response → parse → full patient ref.
	b, err := shnsdk.BuildEligibilityResponse("corr-1", "Patient/MBR-COVERED", true, "", t0)
	if err != nil {
		t.Fatalf("BuildEligibilityResponse: %v", err)
	}
	ref, err := ParseCoverageEligibilityResponsePatient(b)
	if err != nil {
		t.Fatalf("ParseCoverageEligibilityResponsePatient: %v", err)
	}
	if ref != "Patient/MBR-COVERED" {
		t.Errorf("ref = %q, want Patient/MBR-COVERED", ref)
	}

	// Rejects wrong resourceType.
	if _, err := ParseCoverageEligibilityResponsePatient([]byte(`{"resourceType":"Patient"}`)); err == nil {
		t.Error("should reject a Patient resource")
	}

	// Rejects a response missing patient.reference.
	noPatRef := `{"resourceType":"CoverageEligibilityResponse","status":"active"}`
	if _, err := ParseCoverageEligibilityResponsePatient([]byte(noPatRef)); err == nil {
		t.Error("should reject a CER response missing patient.reference")
	}
}

// TestParsePASResponsePatients covers the three PAS response shapes the (C)
// outbound fence must subject-bind: an approved bare ClaimResponse (one ref), a
// pended Bundle (ClaimResponse + Task → one ref), and a denied bare ClaimResponse
// (one ref). Fixtures are built with the SDK builders.
func TestParsePASResponsePatients(t *testing.T) {
	created := time.Date(2026, 6, 4, 0, 0, 0, 0, time.UTC)

	// approved: bare ClaimResponse.
	approved, err := shnsdk.BuildClaimResponse("PA-abc123", "2026-12-31", "Patient/MBR-COVERED", "corr-1", created)
	if err != nil {
		t.Fatalf("BuildClaimResponse: %v", err)
	}
	refs, err := ParsePASResponsePatients(approved)
	if err != nil {
		t.Fatalf("approved: %v", err)
	}
	if len(refs) != 1 || refs[0] != "Patient/MBR-COVERED" {
		t.Fatalf("approved: got %v, want [Patient/MBR-COVERED]", refs)
	}

	// pended: Bundle (ClaimResponse + Task). Only the ClaimResponse carries a patient.
	pended, err := shnsdk.BuildPendedResponse("Patient/MBR-UC04", "corr-2", []string{"operative-diagnostic-report"}, created)
	if err != nil {
		t.Fatalf("BuildPendedResponse: %v", err)
	}
	refs, err = ParsePASResponsePatients(pended)
	if err != nil {
		t.Fatalf("pended: %v", err)
	}
	if len(refs) != 1 || refs[0] != "Patient/MBR-UC04" {
		t.Fatalf("pended: got %v, want [Patient/MBR-UC04]", refs)
	}

	// denied: bare ClaimResponse.
	denied, err := shnsdk.BuildDeniedResponse("Patient/MBR-UC08", "corr-3", "not medically necessary", created)
	if err != nil {
		t.Fatalf("BuildDeniedResponse: %v", err)
	}
	refs, err = ParsePASResponsePatients(denied)
	if err != nil {
		t.Fatalf("denied: %v", err)
	}
	if len(refs) != 1 || refs[0] != "Patient/MBR-UC08" {
		t.Fatalf("denied: got %v, want [Patient/MBR-UC08]", refs)
	}

	// no ClaimResponse / no patient → error (fail loud).
	if _, err := ParsePASResponsePatients([]byte(`{"resourceType":"Bundle","entry":[]}`)); err == nil {
		t.Fatal("empty Bundle: expected error, got nil")
	}
	if _, err := ParsePASResponsePatients([]byte(`{"resourceType":"ClaimResponse"}`)); err == nil {
		t.Fatal("ClaimResponse without patient: expected error, got nil")
	}
	if _, err := ParsePASResponsePatients([]byte(`{`)); err == nil {
		t.Fatal("malformed: expected error, got nil")
	}
}

// TestQuestionnaireHasSubject checks the (C) fence helper: a Questionnaire with
// a subject element returns true; a patient-agnostic Questionnaire (the DTR
// sandbox fixture) returns false; and malformed JSON returns false.
func TestQuestionnaireHasSubject(t *testing.T) {
	// POSITIVE: subject element present → true.
	withSubject := []byte(`{"resourceType":"Questionnaire","subject":{"reference":"Patient/X"}}`)
	if !questionnaireHasSubject(withSubject) {
		t.Error("questionnaireHasSubject: subject-bearing Questionnaire should return true")
	}

	// NEGATIVE: SandboxLumbarQuestionnaire is patient-agnostic → false.
	clean := shnsdk.SandboxLumbarQuestionnaire()
	if questionnaireHasSubject(clean) {
		t.Error("questionnaireHasSubject: SandboxLumbarQuestionnaire should return false (no subject)")
	}

	// MALFORMED: returns false (no valid subject; egress-$validate catches shape).
	if questionnaireHasSubject([]byte("{")) {
		t.Error("questionnaireHasSubject: malformed JSON should return false")
	}
}

// TestParseEOBPatient round-trips a BuildPADecisionEOB EOB through parseEOBPatient
// and asserts the rejections (wrong resourceType, missing patient, malformed).
func TestParseEOBPatient(t *testing.T) {
	created := time.Date(2026, 6, 4, 0, 0, 0, 0, time.UTC)
	eob, err := shnsdk.BuildPADecisionEOB(shnsdk.PADecisionEOBParams{
		ID:          "eob-corr-1",
		PatientRef:  "Patient/MBR-COVERED",
		CoverageRef: "Coverage/MBR-COVERED",
		CPTCode:     "72148",
		Decision:    shnsdk.PADecisionApproved,
		AuthNumber:  "PA-abc123",
		Created:     created,
	})
	if err != nil {
		t.Fatalf("BuildPADecisionEOB: %v", err)
	}
	ref, err := parseEOBPatient(eob)
	if err != nil {
		t.Fatalf("parseEOBPatient: %v", err)
	}
	if ref != "Patient/MBR-COVERED" {
		t.Fatalf("got %q, want Patient/MBR-COVERED", ref)
	}

	if _, err := parseEOBPatient([]byte(`{"resourceType":"ClaimResponse","patient":{"reference":"Patient/X"}}`)); err == nil {
		t.Fatal("wrong resourceType: expected error, got nil")
	}
	if _, err := parseEOBPatient([]byte(`{"resourceType":"ExplanationOfBenefit"}`)); err == nil {
		t.Fatal("missing patient: expected error, got nil")
	}
	if _, err := parseEOBPatient([]byte(`{`)); err == nil {
		t.Fatal("malformed: expected error, got nil")
	}
}
