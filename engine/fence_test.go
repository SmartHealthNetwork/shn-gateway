package engine

import (
	"context"
	"net/http"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// TestSandboxResponder_DTRMalformedFetch400 is a regression test for the behavior-identity
// fix: a malformed DTR fetch body must return 400 "parse questionnaire fetch failed" via
// LegResult.Status (client error), NOT an error return (which the engine maps to 500).
func TestSandboxResponder_DTRMalformedFetch400(t *testing.T) {
	r := NewSandboxResponder(NewSandboxAdjudicator(nil, nil), nil, nil, nil)
	res, err := r.Handle(context.Background(), "dtr-questionnaire-fetch", "corr-1", "pci:x", []byte("{not json"))
	if err != nil {
		t.Fatalf("expected no error return (400 via Status), got err: %v", err)
	}
	if res.Status != http.StatusBadRequest || res.Message != "parse questionnaire fetch failed" {
		t.Fatalf("got (%d, %q), want (400, \"parse questionnaire fetch failed\")", res.Status, res.Message)
	}
}

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
	// A package whose Questionnaire entry is subjectless passes (0).
	clean, err := buildQuestionnairePackage(shnsdk.SandboxLumbarQuestionnaire())
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
// UNCONDITIONALLY. Sandbox posture = both flags false = strict (fail-closed). Native posture =
// both set via markForeignRelay (a real br-payer answers in its OWN namespace).

func TestFenceConformantPAS_SandboxSwap_Rejected(t *testing.T) {
	g := &Gateway{} // fenceResponseSubject reads no Gateway state for the PAS arm
	// sandbox posture: both flags false (zero value). A response naming a DIFFERENT member must 403.
	res := LegResult{ResponseFHIR: claimResponseFor(t, "Patient/MBR-OTHER")}
	if status, _ := g.fenceResponseSubject("pas-claim", "Patient/MBR-COVERED", res); status != http.StatusForbidden {
		t.Fatalf("sandbox swap: status=%d, want 403", status)
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

func TestFenceConformantPASUpdate_SandboxSwap_Rejected(t *testing.T) {
	g := &Gateway{}
	res := LegResult{ResponseFHIR: claimResponseFor(t, "Patient/MBR-OTHER")}
	if status, _ := g.fenceResponseSubject("pas-claim-update", "Patient/MBR-COVERED", res); status != http.StatusForbidden {
		t.Fatalf("update sandbox swap: status=%d, want 403", status)
	}
}
