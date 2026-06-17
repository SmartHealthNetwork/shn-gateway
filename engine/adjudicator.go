package engine

import (
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// sandboxAdjudicator is the gateway's default payer-decisioning implementation:
// it wraps the gateway's OWN internal crd/dtr/pas logic and the SoR's coverage
// read, reproducing the sandbox decisions exactly (behavior byte-unchanged).
// A partner injects their own shnsdk.Adjudicator via Config.Adjudicator to run
// real payer decisioning; the same interface backs the public SDK Responder, so
// one Adjudicator works against both surfaces.
//
// DEF-4 (ordinary-compute PA adjudication) holds here while AI-9 holds: this is
// a deterministic sandbox policy, not real medical-necessity adjudication.
type sandboxAdjudicator struct {
	sor   SystemOfRecord
	clock func() time.Time
}

var _ shnsdk.Adjudicator = (*sandboxAdjudicator)(nil)

// NewSandboxAdjudicator builds the default Adjudicator over a SystemOfRecord.
// clock is used only by PriorAuth (validUntil); a nil clock defaults to time.Now.
func NewSandboxAdjudicator(sor SystemOfRecord, clock func() time.Time) shnsdk.Adjudicator {
	if clock == nil {
		clock = time.Now
	}
	return &sandboxAdjudicator{sor: sor, clock: clock}
}

// Eligibility consults the US Core Coverage RECORD (SoR.CoverageInforce) and
// returns the determination. CMS-0057: Coverage is a record; eligibility is a
// payer decision that reads it (spec §1).
func (s *sandboxAdjudicator) Eligibility(memberID string) (bool, string) {
	return s.sor.CoverageInforce(memberID)
}

// OrderSelect decides PA-required for a CPT and, when required, the DTR
// questionnaire canonical. Inlines the CPT-72148 rule (points at the shnsdk
// constants for PARequired / QuestionnaireCanonicalLumbarMRI).
// crd.BuildCards is pure rendering and takes the verdict from here.
func (s *sandboxAdjudicator) OrderSelect(cpt string) (bool, string) {
	if cpt == "72148" {
		return true, shnsdk.QuestionnaireCanonicalLumbarMRI
	}
	return false, ""
}

// Questionnaire returns the FHIR Questionnaire for a canonical this payer
// advertises. Wraps shnsdk.SandboxLumbarQuestionnaire (sourced from the SDK).
func (s *sandboxAdjudicator) Questionnaire(canonical string) ([]byte, bool) {
	if canonical == shnsdk.QuestionnaireCanonicalLumbarMRI {
		return shnsdk.SandboxLumbarQuestionnaire(), true
	}
	return nil, false
}

// PriorAuth adjudicates a PAS submission. Delegates to shnsdk.SandboxAdjudicate
// (the SDK returns shnsdk.PASDecision directly, with no outcome remapping).
// nil randSource → crypto/rand (as today).
// DenyReason is left empty: the handler supplies the sandbox rationale on the
// deny branch (a custom Adjudicator can set it instead), mirroring the SDK.
func (s *sandboxAdjudicator) PriorAuth(qrJSON []byte, hasDiagnosticReport bool) (shnsdk.PASDecision, error) {
	dec, err := shnsdk.SandboxAdjudicate(qrJSON, hasDiagnosticReport, s.clock(), nil)
	if err != nil {
		return shnsdk.PASDecision{Outcome: shnsdk.PASDenied}, err
	}
	return dec, nil
}
