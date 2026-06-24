// nativepas.go — the native PAS legs of the native-forward Responder.
// They forward pas-claim / pas-claim-update to the partner's /Claim/$submit, normalize
// the partner's Bundle response into SHN's canonical shape (normalizePASResponse,
// FR-G28), and drive the gateway-owned shadow ledger + locally-projected PDex EOB
// (ownership #1). This file owns the shnsdk imports; native.go's read-only legs stay
// shnsdk-free. The PAS response is parsed with the SAME exported parsers the originator
// uses (gateway/engine/originate.go) — no new shnsdk symbol.
package engine

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// handlePASClaimUpdateNative is the CONFORMANT amended re-POST's native-forward (the
// pas-claim-update leg) — the only PA-update native-forward path.
// The conformant bundle's Claim.related[prior] key comes from parseConformantPASUpdateFacts
// (the engine-local conformant extractor). It runs BeginClaimUpdate over the DERIVED shadow ledger
// (FR-21/FR-6), fail-safe on divergence (409), the verbatim relay to the partner's /Claim/$submit,
// and the shadow FinalizeClaimUpdate on approval.
// "Pure relay" is a WIRE property; the shadow finalize is an ORTHOGONAL Store side-effect.
// NO EOB on the update leg. CRITICAL: Rollback:release is armed on EVERY post-Begin exit —
// including partner failure — because the read-only post() returns a 502 WITHOUT Rollback; a
// `return bad, nil` after Begin would strand the claim permanently.
func (n *nativeResponder) handlePASClaimUpdateNative(ctx context.Context, corrID, subjectPCI string, requestFHIR []byte) (LegResult, error) {
	f, status, _ := parseConformantPASUpdateFacts(requestFHIR)
	if status != 0 {
		return LegResult{}, fmt.Errorf("engine: nativePAS parse conformant update bundle: status %d", status) // our fault → 500
	}
	related := f.relatedClaim
	claimed, err := n.store.BeginClaimUpdate(subjectPCI, related)
	if err != nil {
		return LegResult{Status: http.StatusBadGateway, Message: "holder write failed (begin update)"}, nil
	}
	if !claimed {
		// Derived-ledger fail-safe: divergence / no prior pend / replay ⇒ 409, never a
		// silent transition.
		return LegResult{Status: http.StatusConflict, Message: "ClaimUpdate references no pending claim available for this patient"}, nil
	}
	release := func() { _ = n.store.ReleaseClaimUpdate(subjectPCI, related) }

	body, bad := n.post(ctx, n.baseURL, "/Claim/$submit", requestFHIR, "PAS update")
	if bad.Status != 0 {
		bad.Rollback = release // a post-Begin partner failure MUST release the claim
		return bad, nil
	}
	// FR-G28: normalize the partner's Bundle into SHN canonical shape before parsing.
	norm, lr := normalizePASResponse(body)
	if lr.Status != 0 {
		lr.Rollback = release
		return lr, nil
	}
	pended, _, err := shnsdk.ParsePendedResponse(norm)
	if err != nil {
		return LegResult{Status: http.StatusBadGateway, Message: "upstream payer PAS update response unparseable", Rollback: release}, nil
	}
	if pended {
		// Partner re-pended ⇒ still insufficient → 422 + release.
		return LegResult{Status: http.StatusUnprocessableEntity, Message: "amendment still insufficient", Rollback: release}, nil
	}
	parsed, err := shnsdk.ParseClaimResponse(norm)
	if err != nil {
		return LegResult{Status: http.StatusBadGateway, Message: "upstream payer PAS update response untranslatable", Rollback: release}, nil
	}
	if parsed.Outcome != "approved" {
		// Non-approved (incl. a terminal A3 denial) on the update leg → 422 + release:
		// defensive sandbox parity (adjudicator.go:278-282); terminal-denial-on-update is
		// out of scope.
		return LegResult{Status: http.StatusUnprocessableEntity, Message: "amendment still insufficient", Rollback: release}, nil
	}
	// Approved: forward normalized; Finalize completes pended→approved AFTER the write leg.
	// No EOB on the update leg. Rollback stays armed so a post-Begin write/
	// egress-$validate failure still releases.
	return LegResult{
		ResponseFHIR: norm,
		Commit:       func() error { return n.store.FinalizeClaimUpdate(subjectPCI, related) },
		Rollback:     release,
	}, nil
}

// handlePASClaimNative is the CONFORMANT PAS submit leg's native-forward: post the verbatim
// conformant bundle to the partner's /Claim/$submit, normalize the Bundle response to SHN canonical,
// relay it AND project the Store side-effects. The wire is byte-verbatim to br-payer (the
// FR-G25 fidelity asymmetry, same as crd-order-select); the EOB + pended-ledger writes are
// ORTHOGONAL Store side-effects derived from the response (the governing principle). The conformant
// bundle is read by parseConformantPASSubjects (the engine-local conformant extractor; the strict
// shnsdk.ParseClaimBundle the minimized leg used is no longer part of the contract):
// the EOB patientRef is the BOUND member (R-7, request-side — never the response member, which
// a real RI answers in its own namespace), and the CPT comes from the conformant bundle's
// ServiceRequest (the SAME 72148 the EOB-provenance canary checks). handlePASNativeInbound
// egress-$validates SideEffectFHIR + Commits.
func (n *nativeResponder) handlePASClaimNative(ctx context.Context, corrID, subjectPCI string, requestFHIR []byte) (LegResult, error) {
	s, status, msg := parseConformantPASSubjects(requestFHIR)
	if status != 0 {
		return LegResult{Status: status, Message: msg}, nil
	}
	// Extract CPT for the EOB side-effect. A real partner's ServiceRequest may use HCPCS
	// (e.g. L8000 / E0424 from br-payer goldens) rather than the AMA CPT system. The FORWARD is
	// unconditional (never gated on CPT parseability — a HCPCS-coded request is valid); the EOB is
	// built only when a CPT code is present. An absent CPT means no EOB side-effect this leg (the
	// relay still happens, pend/approve still threaded). The EOB is "soft":
	// the EOB is a Store projection keyed to the CPT, not a relay gate.
	cpt, _ := shnsdk.ParseServiceRequestCPT(s.srJSON) // best-effort; empty → no EOB built below
	body, bad := n.post(ctx, n.baseURL, "/Claim/$submit", requestFHIR, "PAS submit")
	if bad.Status != 0 {
		return bad, nil
	}
	norm, lr := normalizePASResponse(body)
	if lr.Status != 0 {
		return lr, nil
	}
	pended, _, err := shnsdk.ParsePendedResponse(norm)
	if err != nil {
		return LegResult{Status: http.StatusBadGateway, Message: "upstream payer PAS submit response unparseable"}, nil
	}
	if pended {
		// FR-21/FR-6: record the pend (payer-local, metadata-only) so the follow-up conformant
		// ClaimUpdate (pas-claim-update) BeginClaimUpdate can bind to a REAL prior pend.
		return LegResult{
			ResponseFHIR: norm,
			Commit:       func() error { return n.store.RecordPendedClaim(subjectPCI, corrID) },
		}, nil
	}
	parsed, err := shnsdk.ParseClaimResponse(norm)
	if err != nil {
		// A 2xx we cannot translate is an upstream problem, not ours → 502.
		return LegResult{Status: http.StatusBadGateway, Message: "upstream payer PAS submit response untranslatable"}, nil
	}
	if cpt == "" {
		// No CPT coding → no EOB side-effect (soft — relay complete, no Store write).
		return LegResult{ResponseFHIR: norm}, nil
	}
	eobJSON, err := n.projectDecisionEOB(corrID, "Patient/"+s.member, cpt, parsed)
	if err != nil {
		return LegResult{}, fmt.Errorf("engine: nativePAS build conformant EOB: %w", err) // our build fault → 500
	}
	eobID := "eob-" + corrID
	return LegResult{
		ResponseFHIR:   norm,
		SideEffectFHIR: [][]byte{eobJSON},
		Commit:         func() error { return n.store.RecordEOB(subjectPCI, eobID, eobJSON) },
	}, nil
}

// projectDecisionEOB synthesises the gateway-local PDex EOB SINGLE-SOURCED from the
// partner's decision: AuthNumber is the partner's parsed preAuthRef (never
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
