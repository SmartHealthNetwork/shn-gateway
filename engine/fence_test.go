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
