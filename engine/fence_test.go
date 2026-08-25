package engine

import (
	"net/http"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// TestFenceResponseSubject_DTR verifies the (C) fence for the dtr-questionnaire-fetch
// leg: a Questionnaire that unexpectedly carries a subject is rejected with 403;
// a patient-agnostic Questionnaire passes (status 0).
func TestFenceResponseSubject_DTR(t *testing.T) {
	g := &Gateway{}
	// A package whose Questionnaire entry carries a subject must be rejected (403) —
	// the fence walks entries (the bare-resource probe would miss it on the wrapper).
	withSubject := []byte(`{"resourceType":"Bundle","type":"collection","entry":[` +
		`{"resource":{"resourceType":"Library","id":"l1"}},` +
		`{"resource":{"resourceType":"Questionnaire","subject":{"reference":"Patient/X"}}}]}`)
	if status, _ := g.fenceResponseSubject("dtr-questionnaire-fetch", "", LegResult{ResponseFHIR: withSubject}); status != http.StatusForbidden {
		t.Fatalf("subject-bearing Questionnaire entry: got status %d, want 403", status)
	}
	// A package whose Questionnaire entry is subjectless passes (0). The Questionnaire is
	// spelled inline (patient-agnostic by construction) rather than borrowed from a
	// content library: the fence's subject matters here, nothing else about the resource.
	clean, err := testQuestionnairePackage([]byte(`{"resourceType":"Questionnaire","id":"q-fence","status":"active",` +
		`"url":"http://example.org/fhir/Questionnaire/q-fence","item":[{"linkId":"1","type":"boolean","text":"answer"}]}`))
	if err != nil {
		t.Fatalf("wrap clean questionnaire: %v", err)
	}
	if status, _ := g.fenceResponseSubject("dtr-questionnaire-fetch", "", LegResult{ResponseFHIR: clean}); status != 0 {
		t.Fatalf("subjectless package: got status %d, want 0", status)
	}
}

// claimResponseFor returns a minimal bare ClaimResponse JSON whose patient.reference is ref —
// the shape ParsePASResponsePatients reads (fhirread.go).
func claimResponseFor(t *testing.T, ref string) []byte {
	t.Helper()
	return []byte(`{"resourceType":"ClaimResponse","patient":{"reference":"` + ref + `"}}`)
}

// eobFor returns a minimal ExplanationOfBenefit JSON whose patient.reference is ref — the shape
// parseEOBPatient reads (fhirread.go).
func eobFor(t *testing.T, ref string) []byte {
	t.Helper()
	return []byte(`{"resourceType":"ExplanationOfBenefit","patient":{"reference":"` + ref + `"}}`)
}

// The converged conformant PAS legs (pas-claim / pas-claim-update) carry the (C)
// outbound fence under TWO independent flags: member-fence the
// ClaimResponse iff !ResponseSubjectForeign (R-7), and the SHN-produced EOB side-effect is fenced
// UNCONDITIONALLY. SHN-produced posture = both flags false = strict (fail-closed). Native posture =
// both set via markForeignRelay (a real br-payer answers in its OWN namespace).

func TestFenceConformantPAS_SubjectSwap_Rejected(t *testing.T) {
	g := &Gateway{} // fenceResponseSubject reads no Gateway state for the PAS arm
	// SHN-produced posture: both flags false (zero value). A response naming a DIFFERENT member must 403.
	res := LegResult{ResponseFHIR: claimResponseFor(t, "Patient/MBR-OTHER")}
	if status, _ := g.fenceResponseSubject("pas-claim", "Patient/MBR-COVERED", res); status != http.StatusForbidden {
		t.Fatalf("subject swap: status=%d, want 403", status)
	}
}

func TestFenceConformantPAS_ForeignRelay_StandsDown(t *testing.T) {
	g := &Gateway{}
	// native posture: ResponseSubjectForeign=true. A foreign-namespace ClaimResponse must PASS (R-7).
	res := LegResult{ResponseFHIR: claimResponseFor(t, "Patient/SubscriberExample"), ResponseSubjectForeign: true, ResponseRelayed: true}
	if status, msg := g.fenceResponseSubject("pas-claim", "Patient/MBR-COVERED", res); status != 0 {
		t.Fatalf("foreign relay stand-down: status=%d msg=%q, want 0", status, msg)
	}
}

func TestFenceConformantPAS_ForeignRelay_WrongEOB_Rejected(t *testing.T) {
	g := &Gateway{}
	// Even under a foreign relay, the SHN-produced EOB side-effect is fenced UNCONDITIONALLY.
	res := LegResult{
		ResponseFHIR:           claimResponseFor(t, "Patient/SubscriberExample"),
		SideEffectFHIR:         [][]byte{eobFor(t, "Patient/MBR-OTHER")},
		ResponseSubjectForeign: true, ResponseRelayed: true,
	}
	if status, _ := g.fenceResponseSubject("pas-claim", "Patient/MBR-COVERED", res); status != http.StatusForbidden {
		t.Fatalf("wrong-member EOB under relay: status=%d, want 403", status)
	}
}

func TestFenceConformantPASUpdate_SubjectSwap_Rejected(t *testing.T) {
	g := &Gateway{}
	res := LegResult{ResponseFHIR: claimResponseFor(t, "Patient/MBR-OTHER")}
	if status, _ := g.fenceResponseSubject("pas-claim-update", "Patient/MBR-COVERED", res); status != http.StatusForbidden {
		t.Fatalf("update subject swap: status=%d, want 403", status)
	}
}

// TestFenceResponseSubject_Eligibility is the direct rejection-test for the (C) fence's
// coverage-eligibility arm: a CoverageEligibilityResponse whose
// patient does not match the bound request patient must 403. This is the arm's OWN unit
// test — TestAdversarial_ResponseSubjectSwap_Eligibility (test/adversarial) proves a
// DIFFERENT, complementary property (the promoted handler never lets an injected
// occupant reach this leg at all, so the fence has nothing to catch there); it does not
// exercise fenceResponseSubject's "coverage-eligibility" case directly, so it cannot
// stand in for this test.
func TestFenceResponseSubject_Eligibility(t *testing.T) {
	g := &Gateway{}
	t0 := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)

	// The swap: a CRR built for Patient/Y, fenced against a request bound to Patient/X.
	crrForY, err := shnsdk.BuildEligibilityResponse("corr-1", "Patient/Y", true, "", shnsdk.PayerIdentifier{}, t0)
	if err != nil {
		t.Fatalf("BuildEligibilityResponse: %v", err)
	}
	if status, msg := g.fenceResponseSubject("coverage-eligibility", "Patient/X", LegResult{ResponseFHIR: crrForY}); status != http.StatusForbidden {
		t.Fatalf("foreign-patient CRR: status=%d msg=%q, want 403", status, msg)
	}

	// Non-vacuous control: a CRR correctly built for the SAME bound patient passes (0).
	crrForX, err := shnsdk.BuildEligibilityResponse("corr-1", "Patient/X", true, "", shnsdk.PayerIdentifier{}, t0)
	if err != nil {
		t.Fatalf("BuildEligibilityResponse: %v", err)
	}
	if status, msg := g.fenceResponseSubject("coverage-eligibility", "Patient/X", LegResult{ResponseFHIR: crrForX}); status != 0 {
		t.Fatalf("matching-patient CRR: status=%d msg=%q, want 0", status, msg)
	}
}
