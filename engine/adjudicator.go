package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// sandboxDenyRationale is the sandbox deny-branch rationale the connector writes
// into the denied ClaimResponse when the Adjudicator returns no DenyReason (FR-22).
// A custom Adjudicator sets dec.DenyReason instead; this is the deterministic
// fallback. Hoisted to a package const so the connector is the single owner (it
// was inline in handlePASInbound before the PAS leg moved behind LegResponder).
const sandboxDenyRationale = "Conservative therapy of at least 6 weeks is not documented (4 weeks on record); request does not meet the payer's medical-necessity policy for advanced lumbar imaging."

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
// payer decision that reads it.
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

// sandboxResponder is the gateway's default LegResponder: it owns payer content
// generation (the decision via adj, the shnsdk.Build* responses, the pended ledger,
// the EOB Store writes). The engine wraps it with authority/sealing/validate/audit.
// DEF-4 holds here while AI-9 holds (deterministic sandbox policy, not real
// medical-necessity adjudication). adj stays the partner-swappable decision façade.
type sandboxResponder struct {
	adj   shnsdk.Adjudicator
	sor   SystemOfRecord
	store Store
	clock func() time.Time
}

var _ LegResponder = (*sandboxResponder)(nil)

func NewSandboxResponder(adj shnsdk.Adjudicator, sor SystemOfRecord, store Store, clock func() time.Time) LegResponder {
	if clock == nil {
		clock = time.Now
	}
	return &sandboxResponder{adj: adj, sor: sor, store: store, clock: clock}
}

// crdCardsForCPT runs the shared CRD adjudication tail: CPT → coverage decision → rendered cards.
// Used by the conformant crd-order-select case.
func (s *sandboxResponder) crdCardsForCPT(cpt string) (LegResult, error) {
	paRequired, canonical := s.adj.OrderSelect(cpt)
	cov := shnsdk.CardCoverage{Covered: "covered"}
	if paRequired {
		cov.PANeeded, cov.Questionnaires = "auth-needed", []string{canonical}
	} else {
		cov.PANeeded = "no-auth"
	}
	cardsJSON, err := shnsdk.BuildCards(cov)
	if err != nil {
		return LegResult{}, fmt.Errorf("build cards: %w", err)
	}
	return LegResult{ResponseFHIR: cardsJSON}, nil
}

func (s *sandboxResponder) Handle(ctx context.Context, leg, corrID, subjectPCI string, requestFHIR []byte) (LegResult, error) {
	switch leg {
	case "coverage-eligibility":
		member, err := shnsdk.ParseEligibilityRequestMember(requestFHIR)
		if err != nil {
			return LegResult{}, fmt.Errorf("parse member: %w", err)
		}
		inforce, reason := s.adj.Eligibility(member)
		crrJSON, err := shnsdk.BuildEligibilityResponse(corrID, "Patient/"+member, inforce, reason, s.clock())
		if err != nil {
			return LegResult{}, fmt.Errorf("build eligibility response: %w", err)
		}
		return LegResult{ResponseFHIR: crrJSON}, nil
	case "crd-order-select":
		srJSON, ok := extractConformantSR(requestFHIR)
		if !ok {
			return LegResult{Status: http.StatusBadRequest, Message: "no ServiceRequest in draftOrders"}, nil
		}
		cpt, err := shnsdk.ParseServiceRequestCPT(srJSON)
		if err != nil {
			return LegResult{Status: http.StatusBadRequest, Message: "parse CPT failed"}, nil
		}
		return s.crdCardsForCPT(cpt)
	case "dtr-questionnaire-fetch":
		var fetch shnsdk.QuestionnaireFetchRequest
		if err := json.Unmarshal(requestFHIR, &fetch); err != nil {
			// Malformed CLIENT request body → 400 (today's handleDTRInbound returned
			// 400 "parse questionnaire fetch failed"), NOT the generic 500 the error
			// return maps to. Surface via Status, like the unknown-canonical 400.
			return LegResult{Status: http.StatusBadRequest, Message: "parse questionnaire fetch failed"}, nil
		}
		questionnaireJSON, ok := s.adj.Questionnaire(fetch.Canonical)
		if !ok {
			return LegResult{Status: http.StatusBadRequest, Message: "unknown questionnaire canonical"}, nil
		}
		// Uniform leg shape — wrap the bare Questionnaire into a one-entry
		// $questionnaire-package collection Bundle (honestly deps-free; the sandbox
		// has none). The consumer extracts the bare Questionnaire on the far side.
		pkg, err := buildQuestionnairePackage(questionnaireJSON)
		if err != nil {
			return LegResult{}, fmt.Errorf("wrap questionnaire package: %w", err)
		}
		return LegResult{ResponseFHIR: pkg}, nil
	case "pas-claim":
		// The conformant leg's live path forwards to the real RI (the composite routes it to
		// native, which relays byte-verbatim AND projects the same EOB + pend Store side-effects
		// — "pure relay" is a wire property, the EOB is an orthogonal
		// side-effect). Here (no real RI) the
		// SANDBOX adjudicates the conformant bundle AND records the same Store side-effects the
		// minimized pas-claim case does — the four-cell submit cell:
		//   - PASPended  → Commit RecordPendedClaim (THE load-bearing handoff: the later
		//                  pas-claim-update leg's BeginClaimUpdate can only find a recorded pend).
		//   - PASApproved/PASDenied → BuildPADecisionEOB + SideEffectFHIR + Commit RecordEOB
		//                  (FR-28/FR-34 Patient-Access EOB).
		// Lifted from the minimized pas-claim case above (not re-derived) so the two stay
		// consistent until the minimized case is removed (rekey-is-net; both coexist now).
		s2, status, msg := parseConformantPASSubjects(requestFHIR)
		if status != 0 {
			return LegResult{Status: status, Message: msg}, nil
		}
		if s2.qrJSON == nil {
			// The bind allows a QR-less conformant bundle (R-5), but the sandbox must adjudicate.
			return LegResult{Status: http.StatusBadRequest, Message: "conformant PAS bundle missing QuestionnaireResponse"}, nil
		}
		// Source the CPT + display from the conformant Claim's ServiceRequest (the EOB's
		// productOrService coding — FR-28 — so the patient surface shows the ACTUAL ordered
		// service, never a hardcoded persona). A CPT-less SR is a malformed CLIENT request →
		// 400, not a generic 500.
		cpt, cptDisplay, cerr := shnsdk.ParseServiceRequestProcedure(s2.srJSON)
		if cerr != nil {
			return LegResult{Status: http.StatusBadRequest, Message: "claim missing service request CPT"}, nil
		}
		dec, err := s.adj.PriorAuth(s2.qrJSON, s2.hasDR)
		if err != nil {
			return LegResult{Status: http.StatusUnprocessableEntity, Message: err.Error()}, nil
		}
		patientRef := "Patient/" + s2.member
		coverageRef := "Coverage/" + s2.member
		switch dec.Outcome {
		case shnsdk.PASPended:
			pendedJSON, err := shnsdk.BuildPendedResponse(patientRef, corrID, dec.NeededItems, s.clock())
			if err != nil {
				return LegResult{}, fmt.Errorf("build pended: %w", err)
			}
			// FR-21/FR-6: record this pended claim (payer-local, metadata-only) so the follow-up
			// ClaimUpdate (pas-claim-update) can bind to a REAL prior pend, keyed by subject PCI +
			// corr. This is the pend→update handoff.
			return LegResult{
				ResponseFHIR: pendedJSON,
				Commit:       func() error { return s.store.RecordPendedClaim(subjectPCI, corrID) },
			}, nil
		case shnsdk.PASApproved:
			crJSON, err := shnsdk.BuildClaimResponse(dec.PreAuthRef, dec.ValidUntil, patientRef, corrID, s.clock())
			if err != nil {
				return LegResult{}, fmt.Errorf("build claim response: %w", err)
			}
			// FR-28: build the PDex PA EOB for the approved decision (Store side-effect via RecordEOB,
			// readable via the Patient Access API, carrying the auth number).
			eobJSON, err := shnsdk.BuildPADecisionEOB(shnsdk.PADecisionEOBParams{
				ID:          "eob-" + corrID,
				PatientRef:  patientRef,
				CoverageRef: coverageRef,
				CPTCode:     cpt,
				CPTDisplay:  cptDisplay,
				Decision:    shnsdk.PADecisionApproved,
				AuthNumber:  dec.PreAuthRef,
				Created:     s.clock(),
			})
			if err != nil {
				return LegResult{}, fmt.Errorf("build EOB: %w", err)
			}
			eobID := "eob-" + corrID
			return LegResult{
				ResponseFHIR:   crJSON,
				SideEffectFHIR: [][]byte{eobJSON},
				Commit:         func() error { return s.store.RecordEOB(subjectPCI, eobID, eobJSON) },
			}, nil
		default: // shnsdk.PASDenied
			rationale := dec.DenyReason
			if rationale == "" {
				rationale = sandboxDenyRationale
			}
			denJSON, err := shnsdk.BuildDeniedResponse(patientRef, corrID, rationale, s.clock())
			if err != nil {
				return LegResult{}, fmt.Errorf("build denied: %w", err)
			}
			// FR-28: build the PDex PA EOB for the patient surface (denied form, CARC 50) —
			// same Store side-effect as the approved branch, AuthNumber empty.
			eobJSON, err := shnsdk.BuildPADecisionEOB(shnsdk.PADecisionEOBParams{
				ID:          "eob-" + corrID,
				PatientRef:  patientRef,
				CoverageRef: coverageRef,
				CPTCode:     cpt,
				CPTDisplay:  cptDisplay,
				Decision:    shnsdk.PADecisionDenied,
				AuthNumber:  "",
				Created:     s.clock(),
			})
			if err != nil {
				return LegResult{}, fmt.Errorf("build EOB: %w", err)
			}
			eobID := "eob-" + corrID
			return LegResult{
				ResponseFHIR:   denJSON,
				SideEffectFHIR: [][]byte{eobJSON},
				Commit:         func() error { return s.store.RecordEOB(subjectPCI, eobID, eobJSON) },
			}, nil
		}
	case "pas-claim-update":
		// CANARY #1: the four-cell UPDATE cell on the CONFORMANT leg — the in-process
		// pend→approve resolution (FinalizeClaimUpdate) two stateless-on-update RIs cannot
		// co-demonstrate. Lifted from the minimized "pas-claim-update" case above (not re-derived)
		// so the two stay consistent until the minimized one is removed (rekey-is-net; both
		// coexist now). The ONLY deltas are the conformant reads: the prior-claim key comes from
		// parseConformantPASUpdateFacts (the strict ParseClaimBundle cannot parse the conformant
		// bundle), and the QR/member/hasDR come from parseConformantPASSubjects. No EOB on the
		// update leg.
		s2, status, msg := parseConformantPASSubjects(requestFHIR)
		if status != 0 {
			return LegResult{Status: status, Message: msg}, nil
		}
		if s2.qrJSON == nil {
			// The bind tolerates a QR-less conformant bundle (R-5), but the sandbox must adjudicate.
			return LegResult{Status: http.StatusBadRequest, Message: "conformant ClaimUpdate missing QuestionnaireResponse"}, nil
		}
		f, fstatus, fmsg := parseConformantPASUpdateFacts(requestFHIR)
		if fstatus != 0 {
			return LegResult{Status: fstatus, Message: fmsg}, nil
		}
		related := f.relatedClaim
		// FR-21 + FR-6: ATOMIC test-and-set — current-state authority + serialization (mirror :386).
		claimed, err := s.store.BeginClaimUpdate(subjectPCI, related)
		if err != nil {
			return LegResult{Status: http.StatusBadGateway, Message: "holder write failed (begin update)"}, nil
		}
		if !claimed {
			return LegResult{Status: http.StatusConflict, Message: "ClaimUpdate references no pending claim available for this patient"}, nil
		}
		// Rollback = ReleaseClaimUpdate: armed on EVERY post-Begin path (mirror :393-399).
		release := func() { _ = s.store.ReleaseClaimUpdate(subjectPCI, related) }
		dec, err := s.adj.PriorAuth(s2.qrJSON, s2.hasDR)
		if err != nil {
			return LegResult{Status: http.StatusUnprocessableEntity, Message: err.Error(), Rollback: release}, nil
		}
		if dec.Outcome != shnsdk.PASApproved {
			// Still insufficient: release back to pended so a later complete amendment can transition it.
			return LegResult{Status: http.StatusUnprocessableEntity, Message: "amendment still insufficient", Rollback: release}, nil
		}
		// Approved: build the ClaimResponse. The update leg builds NO EOB (only submit does).
		patientRef := "Patient/" + s2.member
		crJSON, err := shnsdk.BuildClaimResponse(dec.PreAuthRef, dec.ValidUntil, patientRef, corrID, s.clock())
		if err != nil {
			return LegResult{Rollback: release}, fmt.Errorf("build claim response: %w", err)
		}
		// FR-21: Commit = FinalizeClaimUpdate completes the pended→approved transition AFTER
		// buildResponseLeg, so a replayed update no longer finds the claim.
		return LegResult{
			ResponseFHIR: crJSON,
			Commit:       func() error { return s.store.FinalizeClaimUpdate(subjectPCI, related) },
			Rollback:     release,
		}, nil
	default:
		return LegResult{}, fmt.Errorf("sandboxResponder: unhandled leg %q", leg)
	}
}
