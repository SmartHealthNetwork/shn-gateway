package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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
		osReq, err := shnsdk.ParseOrderSelectRequest(requestFHIR)
		if err != nil {
			return LegResult{}, fmt.Errorf("parse order-select: %w", err)
		}
		cpt, err := shnsdk.ParseServiceRequestCPT([]byte(osReq.Context.DraftOrders[0]))
		if err != nil {
			// PRESERVE today's exact status+message: a malformed CPT is a 400
			// (handleCRDInbound returned 400 "parse CPT failed" today), NOT the
			// generic 500 the error return maps to. Surface via Status, like the
			// DTR unknown-canonical 400.
			return LegResult{Status: http.StatusBadRequest, Message: "parse CPT failed"}, nil
		}
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
		// §6.2: uniform leg shape — wrap the bare Questionnaire into a one-entry
		// $questionnaire-package collection Bundle (honestly deps-free; the sandbox
		// has none). The consumer extracts the bare Questionnaire on the far side.
		pkg, err := buildQuestionnairePackage(questionnaireJSON)
		if err != nil {
			return LegResult{}, fmt.Errorf("wrap questionnaire package: %w", err)
		}
		return LegResult{ResponseFHIR: pkg}, nil
	case "pas-claim":
		cb, err := shnsdk.ParseClaimBundle(requestFHIR)
		if err != nil {
			return LegResult{}, fmt.Errorf("parse bundle: %w", err)
		}
		// Design §1.4 (lineage gap #1): source the CPT from the Claim's
		// ServiceRequest once and use it for the EOB. A ServiceRequest whose
		// CPT coding is absent is a malformed CLIENT request → 400, not a
		// generic 500 (mirrors the crd-order-select "parse CPT failed" 400).
		cpt, cerr := shnsdk.ParseServiceRequestCPT(cb.SRJSON)
		if cerr != nil {
			return LegResult{Status: http.StatusBadRequest, Message: "claim missing service request CPT"}, nil
		}
		// FR-20: pass cb.HasDiagnosticReport so the pended branch fires when the
		// submit bundle lacks an operative DiagnosticReport (prior-surgery case,
		// UC-04). The Adjudicator owns the auth-number randomness (sandbox:
		// crypto/rand, unguessable).
		dec, err := s.adj.PriorAuth(cb.QRJSON, cb.HasDiagnosticReport)
		if err != nil {
			// A PriorAuth DECISION error is a 422 via Status (mirrors today's
			// handlePASInbound), NOT the generic 500 the error return maps to. spec §2 D.
			return LegResult{Status: http.StatusUnprocessableEntity, Message: err.Error()}, nil
		}
		switch dec.Outcome {
		case shnsdk.PASPended:
			// FR-20: build the pended response (Bundle with ClaimResponse+Task) so the
			// provider can attach supplemental data for exchange-2. FR-21/FR-6: record
			// this pended claim (payer-local, metadata-only) via Commit so the follow-up
			// ClaimUpdate can be bound to a REAL prior pend, keyed by subject PCI + corr.
			pendedJSON, err := shnsdk.BuildPendedResponse(cb.ClaimPatient, corrID, dec.NeededItems, s.clock())
			if err != nil {
				return LegResult{}, fmt.Errorf("build pended: %w", err)
			}
			return LegResult{
				ResponseFHIR: pendedJSON,
				Commit:       func() error { return s.store.RecordPendedClaim(subjectPCI, corrID) },
			}, nil
		case shnsdk.PASApproved:
			crJSON, err := shnsdk.BuildClaimResponse(dec.PreAuthRef, dec.ValidUntil, cb.ClaimPatient, corrID, s.clock())
			if err != nil {
				return LegResult{}, fmt.Errorf("build claim response: %w", err)
			}
			// FR-28: build the PDex PA EOB for the approved decision; it surfaces as a
			// Store side-effect (RecordEOB) readable via the Patient Access API, carrying
			// the auth number. The engine egress-$validates it before Commit.
			eobJSON, err := shnsdk.BuildPADecisionEOB(shnsdk.PADecisionEOBParams{
				ID:          "eob-" + corrID,
				PatientRef:  cb.ClaimPatient,
				CoverageRef: "Coverage/" + strings.TrimPrefix(cb.ClaimPatient, "Patient/"),
				CPTCode:     cpt,
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
			// FR-22 (UC-08): a real denied ClaimResponse with the PAS reviewAction (A3),
			// rationale, appeal window, and peer-to-peer instruction.
			rationale := dec.DenyReason
			if rationale == "" {
				rationale = sandboxDenyRationale
			}
			denJSON, err := shnsdk.BuildDeniedResponse(cb.ClaimPatient, corrID, rationale, s.clock())
			if err != nil {
				return LegResult{}, fmt.Errorf("build denied: %w", err)
			}
			// FR-28: build the PDex PA EOB for the patient surface (denied form, CARC 50)
			// — same Store side-effect as the approved branch, AuthNumber empty.
			eobJSON, err := shnsdk.BuildPADecisionEOB(shnsdk.PADecisionEOBParams{
				ID:          "eob-" + corrID,
				PatientRef:  cb.ClaimPatient,
				CoverageRef: "Coverage/" + strings.TrimPrefix(cb.ClaimPatient, "Patient/"),
				CPTCode:     cpt,
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
		cb, err := shnsdk.ParseClaimBundle(requestFHIR)
		if err != nil {
			return LegResult{}, fmt.Errorf("parse bundle: %w", err)
		}
		// FR-21 + FR-6: ATOMIC test-and-set is the current-state authority check AND the
		// serialization point — only one update can be in flight for a given pended claim.
		// It runs INSIDE Handle (the connector's serialization point over its OWN ledger),
		// pre-decision. A store-write error → 502 via Status (parity with today's begin-update
		// 502); !claimed (no prior pend / replay of an approved update / a second concurrent
		// update) → 409 via Status. Neither routes via the error return.
		claimed, err := s.store.BeginClaimUpdate(subjectPCI, cb.RelatedClaim)
		if err != nil {
			return LegResult{Status: http.StatusBadGateway, Message: "holder write failed (begin update)"}, nil
		}
		if !claimed {
			return LegResult{Status: http.StatusConflict, Message: "ClaimUpdate references no pending claim available for this patient"}, nil
		}
		// Rollback = ReleaseClaimUpdate: returned on EVERY post-Begin path (the 422
		// insufficient/PriorAuth-error paths AND the BuildClaimResponse-error path) so a
		// mid-adjudication failure or an insufficient amendment never strands the claim and a
		// later complete amendment can transition it. The engine arms a defer on the returned
		// result BEFORE checking err, so the build-error path releases too. The ReleaseClaimUpdate
		// error is ignored exactly as today (`_ =`).
		release := func() { _ = s.store.ReleaseClaimUpdate(subjectPCI, cb.RelatedClaim) }
		dec, err := s.adj.PriorAuth(cb.QRJSON, cb.HasDiagnosticReport)
		if err != nil {
			// A PriorAuth DECISION error is a 422 via Status (mirrors today's update leg),
			// NOT the generic 500 the error return maps to. Claim released via Rollback.
			return LegResult{Status: http.StatusUnprocessableEntity, Message: err.Error(), Rollback: release}, nil
		}
		if dec.Outcome != shnsdk.PASApproved {
			// Still insufficient: the claim is released (Rollback) back to pended so a later,
			// complete amendment can still transition it.
			return LegResult{Status: http.StatusUnprocessableEntity, Message: "amendment still insufficient", Rollback: release}, nil
		}
		// Approved: build the ClaimResponse. NOTE: the update leg builds NO EOB (only submit
		// does). A build error returns Rollback alongside the error so the engine defer (armed
		// before the err check) still releases the claim.
		crJSON, err := shnsdk.BuildClaimResponse(dec.PreAuthRef, dec.ValidUntil, cb.ClaimPatient, corrID, s.clock())
		if err != nil {
			return LegResult{Rollback: release}, fmt.Errorf("build claim response: %w", err)
		}
		// FR-21: Commit = FinalizeClaimUpdate completes the pended→approved transition AFTER
		// buildResponseLeg, so a replayed update no longer finds the claim.
		related := cb.RelatedClaim
		return LegResult{
			ResponseFHIR: crJSON,
			Commit:       func() error { return s.store.FinalizeClaimUpdate(subjectPCI, related) },
			Rollback:     release,
		}, nil
	default:
		return LegResult{}, fmt.Errorf("sandboxResponder: unhandled leg %q", leg)
	}
}
