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
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

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
		// A carry-forward amendment (no infoChanged → br-payer keeps the prior decision, does NOT
		// re-evaluate) surfaces the re-pend AS-IS — the two-RI carry+adjudicate observation (D-2RI-6:
		// the carried evidence does not DRIVE br-payer's code-constant verdict). Only an amendment
		// that REQUESTED re-evaluation (infoChanged) polls for the timer-resolved terminal A1.
		if !requestClaimHasInfoChanged(requestFHIR) {
			return LegResult{Status: http.StatusUnprocessableEntity, Message: "amendment still insufficient", Rollback: release}, nil
		}
		// A real Da Vinci payer (br-payer) RE-PENDS an infoChanged amendment — persistUpdatePath
		// re-evaluates the item (G0151 conditional → A4) and reschedules — and resolves A4→A1 ONLY on its
		// own timer (PasPendedResolutionService.resolveAuthorization flips A4→A1 IN PLACE on the same id).
		// The amendment genuinely ran (br-payer accepted + re-evaluated it); the TIMER is what approves it.
		// Poll GET ClaimResponse/{id} until the timer flips it to A1. The deadline starts HERE — after
		// the ClaimUpdate $submit rescheduled the timer (user note: poll the rescheduled timer).
		crID := claimResponseIDFromPASResponse(norm)
		if crID == "" {
			return LegResult{Status: http.StatusBadGateway, Message: "re-pended PAS update response has no ClaimResponse id to re-query", Rollback: release}, nil
		}
		resolved, rerr := n.pollClaimResponseUntilApproved(ctx, crID)
		if rerr != nil {
			return LegResult{Status: http.StatusBadGateway, Message: "PAS pend re-query failed", Rollback: release}, nil
		}
		if resolved == nil {
			// Never resolved within the bound → genuine non-resolution, never a silent pass.
			return LegResult{Status: http.StatusUnprocessableEntity, Message: "amendment still pended after re-query", Rollback: release}, nil
		}
		// Resolved to A1: relay the resolved ClaimResponse + Finalize the shadow ledger (same as the
		// directly-approved path below). No EOB on the update leg. Rollback stays armed.
		return LegResult{
			ResponseFHIR: resolved,
			Commit:       func() error { return n.store.FinalizeClaimUpdate(subjectPCI, related) },
			Rollback:     release,
		}, nil
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
// a real RI answers in its own namespace), and the procedure {system,code} comes from the
// conformant bundle's ServiceRequest (CPT or HCPCS — the SAME 72148 the EOB-provenance canary
// checks for the CPT persona). handlePASNativeInbound
// egress-$validates SideEffectFHIR + Commits.
func (n *nativeResponder) handlePASClaimNative(ctx context.Context, corrID, subjectPCI string, requestFHIR []byte) (LegResult, error) {
	s, status, msg := parseConformantPASSubjects(requestFHIR)
	if status != 0 {
		return LegResult{Status: status, Message: msg}, nil
	}
	// Source the order's procedure {system, code, display} (CPT or HCPCS) for the EOB side-effect.
	// system flows from the order so a HCPCS order yields a HCPCS-system EOB (FR-28) — threaded as
	// a unit. The FORWARD is unconditional; an unrecognized system → empty code → no EOB (soft).
	procSystem, cpt, cptDisplay, _ := shnsdk.ParseServiceRequestProductCoding(s.srJSON) // best-effort; empty cpt → no EOB built below
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
		// No recognized {CPT,HCPCS} product coding → no EOB side-effect (soft — relay complete, no Store write).
		return LegResult{ResponseFHIR: norm}, nil
	}
	eobJSON, err := n.projectDecisionEOB(corrID, "Patient/"+s.member, procSystem, cpt, cptDisplay, parsed)
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
// minted), CPTCode + CPTDisplay are the Claim's ServiceRequest procedure (never
// hardcoded), ProcedureSystem flows from the order so a HCPCS order yields a
// HCPCS-system EOB (FR-28). No engine guard — the construction makes it true;
// the adversarial row makes a mint/pin loud.
func (n *nativeResponder) projectDecisionEOB(corrID, patientRef, procSystem, cpt, cptDisplay string, parsed shnsdk.PriorAuthResult) ([]byte, error) {
	decision, authNumber := shnsdk.PADecisionApproved, parsed.PreAuthRef
	if parsed.Outcome == "denied" {
		decision, authNumber = shnsdk.PADecisionDenied, ""
	}
	return shnsdk.BuildPADecisionEOB(shnsdk.PADecisionEOBParams{
		ID:              "eob-" + corrID,
		PatientRef:      patientRef,
		CoverageRef:     "Coverage/" + strings.TrimPrefix(patientRef, "Patient/"),
		CPTCode:         cpt,
		CPTDisplay:      cptDisplay,
		ProcedureSystem: procSystem,
		Decision:        decision,
		AuthNumber:      authNumber,
		Created:         n.clock(),
	})
}

// pollClaimResponseUntilApproved polls the partner's GET ClaimResponse/{id} until it resolves to an
// approved (A1) ClaimResponse, or the bound (pendReQueryTimeout/Interval) is exhausted. br-payer
// auto-approves a pended (A4) item after pas.pended-resolution-delay-seconds
// (PasPendedResolutionService → PasResponseBuilder.finalizePendedItems flips A4→A1 IN PLACE on the
// same id). Returns the resolved A1 response bytes (canonical, via normalizePASResponse), or
// (nil,nil) if it never resolved within the bound — a non-error so the caller 422s (no silent pass).
// Count-bounded (no clock dependency); the deadline starts at the call (post-ClaimUpdate, the
// rescheduled timer).
func (n *nativeResponder) pollClaimResponseUntilApproved(ctx context.Context, claimResponseID string) ([]byte, error) {
	interval := n.pendReQueryInterval
	if interval <= 0 {
		interval = defaultPendReQueryInterval
	}
	attempts := int(n.pendReQueryTimeout / interval)
	if attempts < 1 {
		attempts = 1
	}
	for i := 0; i < attempts; i++ {
		body, bad := n.get(ctx, n.baseURL, "/ClaimResponse/"+claimResponseID, "PAS re-query")
		if bad.Status != 0 {
			return nil, fmt.Errorf("PAS re-query ClaimResponse/%s: upstream status %d", claimResponseID, bad.Status)
		}
		norm, lr := normalizePASResponse(body)
		if lr.Status == 0 {
			if res, perr := shnsdk.ParseClaimResponse(norm); perr == nil && res.Outcome == "approved" {
				return norm, nil
			}
		}
		if i < attempts-1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(interval):
			}
		}
	}
	return nil, nil // never resolved within the bound
}

// pasInfoChangedExtURL is the Da Vinci PAS Claim-item infoChanged extension (the engine-local mirror
// of the SDK's pasInfoChangedExtensionURL — different modules). Its presence on the amendment's
// operative Claim item is what distinguishes a re-evaluation-requesting amendment (poll for the
// timer-resolved A1) from a carry-forward amendment (surface the re-pend as-is).
const pasInfoChangedExtURL = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-infoChanged"

// requestClaimHasInfoChanged reports whether the amendment's operative Claim item carries the PAS
// infoChanged extension. br-payer re-evaluates an infoChanged item (handleUpdate) then re-pends a
// conditional code (G0151) → A4, which its timer resolves to A1 — so only these poll. A no-infoChanged
// amendment is a carry-forward (br-payer keeps the prior decision); this leg surfaces that re-pend
// as-is (the two-RI carry+adjudicate observation, D-2RI-6).
func requestClaimHasInfoChanged(requestFHIR []byte) bool {
	var b struct {
		Entry []struct {
			Resource struct {
				ResourceType string `json:"resourceType"`
				Item         []struct {
					Extension []struct {
						URL string `json:"url"`
					} `json:"extension"`
				} `json:"item"`
			} `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(requestFHIR, &b); err != nil {
		return false
	}
	for _, e := range b.Entry {
		if e.Resource.ResourceType != "Claim" {
			continue
		}
		for _, it := range e.Resource.Item {
			for _, ext := range it.Extension {
				if ext.URL == pasInfoChangedExtURL {
					return true
				}
			}
		}
	}
	return false
}

// claimResponseIDFromPASResponse extracts the (server-assigned) ClaimResponse id from a normalized
// PAS response. br-payer re-pends the SAME ClaimResponse in place (persistUpdatePath wraps the
// existing CR), so its id is the GET re-query target. Reads ClaimResponse.id, falling back to the
// entry fullUrl's last segment. Empty when none found.
func claimResponseIDFromPASResponse(norm []byte) string {
	var probe struct {
		ResourceType string `json:"resourceType"`
		ID           string `json:"id"`
		Entry        []struct {
			FullURL  string `json:"fullUrl"`
			Resource struct {
				ResourceType string `json:"resourceType"`
				ID           string `json:"id"`
			} `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(norm, &probe); err != nil {
		return ""
	}
	if probe.ResourceType == "ClaimResponse" && probe.ID != "" {
		return probe.ID
	}
	for _, e := range probe.Entry {
		if e.Resource.ResourceType != "ClaimResponse" {
			continue
		}
		if e.Resource.ID != "" {
			return e.Resource.ID
		}
		if i := strings.LastIndex(e.FullURL, "ClaimResponse/"); i >= 0 {
			return e.FullURL[i+len("ClaimResponse/"):]
		}
	}
	return ""
}
