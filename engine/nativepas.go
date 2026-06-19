// nativepas.go — the native PAS legs of the native-forward Responder (design §2/§3).
// They forward pas-claim / pas-claim-update to the partner's /Claim/$submit, forward
// the partner's response VERBATIM (corrID is envelope-level, not payload — design §1.2),
// and drive the gateway-owned shadow ledger + locally-projected PDex EOB (ownership #1).
// This file owns the shnsdk imports; native.go's read-only legs stay shnsdk-free. The
// PAS response is parsed with the SAME exported parsers the originator uses
// (gateway/engine/originate.go) — no new shnsdk symbol (design §1.3).
package engine

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// handlePASClaim forwards a PAS submission, parses the partner's outcome, forwards the
// response verbatim, and drives the gateway ledger/EOB (design §2).
func (n *nativeResponder) handlePASClaim(ctx context.Context, corrID, subjectPCI string, requestFHIR []byte) (LegResult, error) {
	cb, err := shnsdk.ParseClaimBundle(requestFHIR)
	if err != nil {
		return LegResult{}, fmt.Errorf("engine: nativePAS parse claim bundle: %w", err) // our fault → 500
	}
	cpt, err := shnsdk.ParseServiceRequestCPT(cb.SRJSON)
	if err != nil {
		// Malformed CLIENT Claim (no usable ServiceRequest CPT) → 400 (design §1.4).
		return LegResult{Status: http.StatusBadRequest, Message: "claim missing service request CPT"}, nil
	}
	body, bad := n.post(ctx, "/Claim/$submit", requestFHIR, "PAS submit")
	if bad.Status != 0 {
		return bad, nil
	}
	pended, _, err := shnsdk.ParsePendedResponse(body)
	if err != nil {
		return LegResult{Status: http.StatusBadGateway, Message: "upstream payer PAS submit response unparseable"}, nil
	}
	if pended {
		return LegResult{
			ResponseFHIR: body, // verbatim (§1.2)
			Commit:       func() error { return n.store.RecordPendedClaim(subjectPCI, corrID) },
		}, nil
	}
	parsed, err := shnsdk.ParseClaimResponse(body)
	if err != nil {
		// A 2xx we cannot translate is an upstream problem, not ours → 502 (§2).
		return LegResult{Status: http.StatusBadGateway, Message: "upstream payer PAS submit response untranslatable"}, nil
	}
	eobJSON, err := n.projectDecisionEOB(corrID, cb.ClaimPatient, cpt, parsed)
	if err != nil {
		return LegResult{}, fmt.Errorf("engine: nativePAS build EOB: %w", err) // our build fault → 500
	}
	eobID := "eob-" + corrID
	return LegResult{
		ResponseFHIR:   body, // verbatim
		SideEffectFHIR: [][]byte{eobJSON},
		Commit:         func() error { return n.store.RecordEOB(subjectPCI, eobID, eobJSON) },
	}, nil
}

// handlePASClaimUpdate forwards an amendment; BeginClaimUpdate runs INSIDE Handle over
// the DERIVED shadow ledger (FR-21/FR-6 serialization + current-state authority), fail-
// safe on divergence (design §3). CRITICAL: Rollback:release is armed on EVERY post-Begin
// exit — including partner failure — because the read-only post() returns a 502 WITHOUT
// Rollback; a `return bad, nil` after Begin would strand the claim permanently.
func (n *nativeResponder) handlePASClaimUpdate(ctx context.Context, corrID, subjectPCI string, requestFHIR []byte) (LegResult, error) {
	cb, err := shnsdk.ParseClaimBundle(requestFHIR)
	if err != nil {
		return LegResult{}, fmt.Errorf("engine: nativePAS parse update bundle: %w", err) // our fault → 500
	}
	claimed, err := n.store.BeginClaimUpdate(subjectPCI, cb.RelatedClaim)
	if err != nil {
		return LegResult{Status: http.StatusBadGateway, Message: "holder write failed (begin update)"}, nil
	}
	if !claimed {
		// Derived-ledger fail-safe: divergence / no prior pend / replay ⇒ 409, never a
		// silent transition (design §3).
		return LegResult{Status: http.StatusConflict, Message: "ClaimUpdate references no pending claim available for this patient"}, nil
	}
	related := cb.RelatedClaim
	release := func() { _ = n.store.ReleaseClaimUpdate(subjectPCI, related) }

	body, bad := n.post(ctx, "/Claim/$submit", requestFHIR, "PAS update")
	if bad.Status != 0 {
		bad.Rollback = release // §3: a post-Begin partner failure MUST release the claim
		return bad, nil
	}
	pended, _, err := shnsdk.ParsePendedResponse(body)
	if err != nil {
		return LegResult{Status: http.StatusBadGateway, Message: "upstream payer PAS update response unparseable", Rollback: release}, nil
	}
	if pended {
		// Partner re-pended ⇒ still insufficient → 422 + release (design §3).
		return LegResult{Status: http.StatusUnprocessableEntity, Message: "amendment still insufficient", Rollback: release}, nil
	}
	parsed, err := shnsdk.ParseClaimResponse(body)
	if err != nil {
		return LegResult{Status: http.StatusBadGateway, Message: "upstream payer PAS update response untranslatable", Rollback: release}, nil
	}
	if parsed.Outcome != "approved" {
		// Non-approved (incl. a terminal A3 denial) on the update leg → 422 + release:
		// defensive sandbox parity (adjudicator.go:278-282); terminal-denial-on-update is
		// out of scope (design §3/§8).
		return LegResult{Status: http.StatusUnprocessableEntity, Message: "amendment still insufficient", Rollback: release}, nil
	}
	// Approved: forward verbatim; Finalize completes pended→approved AFTER the write leg.
	// No EOB on the update leg (design §3). Rollback stays armed so a post-Begin write/
	// egress-$validate failure still releases.
	return LegResult{
		ResponseFHIR: body,
		Commit:       func() error { return n.store.FinalizeClaimUpdate(subjectPCI, related) },
		Rollback:     release,
	}, nil
}

// projectDecisionEOB synthesises the gateway-local PDex EOB SINGLE-SOURCED from the
// partner's decision (design §4): AuthNumber is the partner's parsed preAuthRef (never
// minted), CPTCode is the Claim's procedure (never hardcoded). No engine guard — the
// construction makes it true; the adversarial row makes a mint/pin loud.
func (n *nativeResponder) projectDecisionEOB(corrID, patientRef, cpt string, parsed shnsdk.PriorAuthResult) ([]byte, error) {
	decision, authNumber := shnsdk.PADecisionApproved, parsed.PreAuthRef
	if parsed.Outcome == "denied" {
		decision, authNumber = shnsdk.PADecisionDenied, ""
	}
	return shnsdk.BuildPADecisionEOB(shnsdk.PADecisionEOBParams{
		ID:          "eob-" + corrID,
		PatientRef:  patientRef,
		CoverageRef: "Coverage/" + strings.TrimPrefix(patientRef, "Patient/"),
		CPTCode:     cpt,
		Decision:    decision,
		AuthNumber:  authNumber,
		Created:     n.clock(),
	})
}
