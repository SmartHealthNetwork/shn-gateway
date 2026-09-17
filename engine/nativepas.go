// nativepas.go — the native PAS legs of the native-forward Responder.
// They forward pas-claim / pas-claim-update to the partner's /Claim/$submit, retain
// the partner's native response Bundle (validateNativePASResponse,
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

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
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
// including a no-response fault AND a relayed partner non-2xx — because post() never attaches
// Rollback itself; a bare `return LegResult{}, err` or `return bad, nil` after Begin would strand
// the claim permanently.
func (n *nativeResponder) handlePASClaimUpdateNative(ctx context.Context, corrID, subjectPCI string, in relay.Body, requestFHIR []byte) (LegResult, error) {
	f, status, _ := parseConformantPASUpdateFacts(requestFHIR)
	if status != 0 {
		return LegResult{}, fmt.Errorf("engine: nativePAS parse conformant update bundle: status %d", status) // our fault → 500
	}
	// Payer-edge identity mapping (payoredge.go): map the bundle's Coverage payer
	// identity (and a Claim.insurer naming this payer) BEFORE any store write, so a
	// refusal never leaves a claim pended with no way to release it. Off (the default) ⇒
	// the bundle is sent exactly.
	forward, refused, err := n.payorEdgeRequest(in, payorEdgePASBundle, "application/fhir+json")
	if err != nil {
		return LegResult{}, err
	}
	if refused.Status != 0 {
		return refused, nil
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

	// Endpoint evidence: prefer the probe-retained, same-origin-validated #<line> $submit
	// endpoint for THIS routed pa.pas line (evidence absent ⇒ n.baseURL, byte-identical).
	submitURL := n.resolvedURL(ctx, "pa.pas", n.baseURL, "/Claim/$submit")
	up, bad, err := n.post(ctx, submitURL, "", forward, "pas-claim-update", "PAS update")
	if err != nil {
		return LegResult{Rollback: release}, err // post-Begin fault MUST still release the claim
	}
	if bad.Status == http.StatusConflict && isPayerVersionConflict(up.raw) {
		// The payer's store refused the amendment's write because its own pend-resolution
		// timer wrote the same ClaimResponse first (nativepas_conflict.go): the amendment was
		// not persisted, so re-issue it exactly once after that write has landed. Whatever
		// the re-issue answers takes the ordinary path below — relayed if non-2xx.
		if werr := sleepCtx(ctx, payerVersionConflictRetryDelay); werr != nil {
			return LegResult{Rollback: release}, werr
		}
		up, bad, err = n.post(ctx, submitURL, "", forward, "pas-claim-update", "PAS update (re-issued after the payer's version conflict)")
		if err != nil {
			return LegResult{Rollback: release}, err
		}
	}
	if bad.Status != 0 {
		bad.Rollback = release // a post-Begin partner non-2xx MUST release the claim (relay path)
		return bad, nil
	}
	// FR-G28: validate the complete partner Bundle without changing its bytes.
	response, lr := validateNativePASResponse(up.raw)
	if lr.Status != 0 {
		lr.Rollback = release
		return lr, nil
	}
	pended, _, err := shnsdk.ParsePendedResponse(response)
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
		// The re-pend has TWO wire shapes, both classified pended by validateNativePASResponse: outcome
		// "queued" (the amendment landed before the timer) and outcome "complete" + A4 item (it landed
		// AFTER the timer already approved the prior pend — the outcome is stale, the A4 is the truth).
		// Either way the rescheduled timer is what approves it, so both poll below.
		// The amendment genuinely ran (br-payer accepted + re-evaluated it); the TIMER is what approves it.
		// Poll GET ClaimResponse/{id} until the timer flips it to A1. The deadline starts HERE — after
		// the ClaimUpdate $submit rescheduled the timer (user note: poll the rescheduled timer).
		crID := claimResponseIDFromPASResponse(response)
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
		// Resolved to A1: assemble the retained Bundle and finalize the shadow ledger (same as the
		// directly-approved path below). No EOB on the update leg. Rollback stays armed.
		assembled, aerr := assembleTerminalPASBundle(response, resolved, n.clock())
		if aerr != nil {
			return LegResult{Status: http.StatusBadGateway, Message: "invalid PAS terminal assembly", Rollback: release}, nil
		}
		answer, serr := assembledPASAnswer(assembled)
		if serr != nil {
			return LegResult{Status: http.StatusBadGateway, Message: "invalid PAS terminal assembly", Rollback: release}, nil
		}
		return LegResult{
			ResponseAssembled:      true,
			ResponseSubjectForeign: true,
			Response:               answer,
			Commit:                 func() error { return n.store.FinalizeClaimUpdate(subjectPCI, related) },
			Rollback:               release,
		}, nil
	}
	parsed, err := shnsdk.ParseClaimResponse(response)
	if err != nil {
		return LegResult{Status: http.StatusBadGateway, Message: "upstream payer PAS update response untranslatable", Rollback: release}, nil
	}
	if parsed.Outcome != "approved" {
		// Non-approved (incl. a terminal A3 denial) on the update leg → 422 + release:
		// defensive in-process parity; terminal-denial-on-update is
		// out of scope.
		return LegResult{Status: http.StatusUnprocessableEntity, Message: "amendment still insufficient", Rollback: release}, nil
	}
	// Approved: forward the complete original Bundle; Finalize follows response sealing.
	// No EOB on the update leg. Rollback stays armed so a post-Begin write/
	// egress-$validate failure still releases.
	return LegResult{
		ResponseSubjectForeign: true,
		Response:               relay.Exact(up.body, "application/fhir+json"),
		Commit:                 func() error { return n.store.FinalizeClaimUpdate(subjectPCI, related) },
		Rollback:               release,
	}, nil
}

// handlePASClaimNative is the CONFORMANT PAS submit leg's native-forward: post the verbatim
// conformant bundle to the partner's /Claim/$submit, retain the native response Bundle,
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
func (n *nativeResponder) handlePASClaimNative(ctx context.Context, corrID, subjectPCI string, in relay.Body, requestFHIR []byte) (LegResult, error) {
	s, status, msg := parseConformantPASSubjects(requestFHIR)
	if status != 0 {
		return LegResult{Status: status, Message: msg}, nil
	}
	// Payer-edge identity mapping (payoredge.go): map the bundle's Coverage payer
	// identity (and a Claim.insurer naming this payer) before it is posted (and before
	// any store side-effect below). Off (the default) ⇒ the bundle is sent exactly.
	forward, refused, err := n.payorEdgeRequest(in, payorEdgePASBundle, "application/fhir+json")
	if err != nil {
		return LegResult{}, err
	}
	if refused.Status != 0 {
		return refused, nil
	}
	// Source the order's procedure {system, code, display} (CPT or HCPCS) for the EOB side-effect.
	// system flows from the order so a HCPCS order yields a HCPCS-system EOB (FR-28) — threaded as
	// a unit. The FORWARD is unconditional; an unrecognized system → empty code → no EOB (soft).
	// ParseOrderProductCoding handles BOTH a ServiceRequest order and a DeviceRequest order (the
	// HomeOxygen provider-data lane), so a DME DeviceRequest still yields its HCPCS EOB.
	procSystem, cpt, cptDisplay, _ := shnsdk.ParseOrderProductCoding(s.srJSON) // best-effort; empty cpt → no EOB built below
	// Endpoint evidence: prefer the probe-retained, same-origin-validated #<line> $submit
	// endpoint for THIS routed pa.pas line (evidence absent ⇒ n.baseURL, byte-identical).
	submitURL := n.resolvedURL(ctx, "pa.pas", n.baseURL, "/Claim/$submit")
	up, bad, err := n.post(ctx, submitURL, "", forward, "pas-claim", "PAS submit")
	if err != nil {
		return LegResult{}, err // no-response fault → engine 500 → "hub routing failed"
	}
	if bad.Status != 0 {
		return bad, nil // upstream non-2xx → relayable LegResult (Response carries the body)
	}
	response, lr := validateNativePASResponse(up.raw)
	// The answer to send: the payer's Bundle exactly, unless it is replaced by
	// a terminal assembly below.
	answer := relay.Exact(up.body, "application/fhir+json")
	if lr.Status != 0 {
		return lr, nil
	}
	assembled := false
	pended, _, err := shnsdk.ParsePendedResponse(response)
	if err != nil {
		return LegResult{Status: http.StatusBadGateway, Message: "upstream payer PAS submit response unparseable"}, nil
	}
	if pended {
		// SINGLE-SHOT submit → POLL the timer-resolved terminal A1. Two single-shot lanes both poll
		// the SAME GET ClaimResponse/{id} machinery (the one the ClaimUpdate amendment path uses):
		//   1. a DeviceRequest order (HomeOxygen provider-data DME lane) — no amendment leg exists; and
		//   2. a ServiceRequest order whose submit bundle signals "resolve to terminal" via the Da Vinci
		//      PAS infoChanged item extension (the provider-data order-select single-shot lane, D-PD-1).
		// br-payer's conditional-coverage pend (A4) auto-resolves on its own timer
		// (PasPendedResolutionService, PAS_PENDED_RESOLUTION_DELAY_SECONDS) — flipping A4→A1 IN PLACE on
		// the same ClaimResponse id, reachable by a bare GET (no amendment needed). infoChanged here is
		// purely SHN's POLL DISCRIMINATOR, NOT a verdict input: on a fresh submit (no Claim.related[prior])
		// it is benign on br-payer (its re-evaluation is gated on a prior claim), so the verdict is still
		// br-payer's code-keyed CQL constant and the A4→A1 is still the timer. A ServiceRequest WITHOUT
		// infoChanged keeps the prior behavior — return the A4 pend so the UC-04/06 amendment
		// leg can bind to it — so this does NOT regress the amendment lanes.
		if orderIsDeviceRequest(s.srJSON) || requestClaimHasInfoChanged(requestFHIR) {
			crID := claimResponseIDFromPASResponse(response)
			if crID == "" {
				return LegResult{Status: http.StatusBadGateway, Message: "pended PAS submit response has no ClaimResponse id to re-query"}, nil
			}
			resolved, rerr := n.pollClaimResponseUntilApproved(ctx, crID)
			if rerr != nil {
				return LegResult{Status: http.StatusBadGateway, Message: "PAS pend re-query failed"}, nil
			}
			if resolved == nil {
				// Never resolved within the bound → genuine non-resolution, never a silent pass.
				return LegResult{Status: http.StatusUnprocessableEntity, Message: "single-shot PAS still pended after re-query"}, nil
			}
			response, err = assembleTerminalPASBundle(response, resolved, n.clock())
			if err != nil {
				return LegResult{Status: http.StatusBadGateway, Message: "invalid PAS terminal assembly"}, nil
			}
			if answer, err = assembledPASAnswer(response); err != nil {
				return LegResult{Status: http.StatusBadGateway, Message: "invalid PAS terminal assembly"}, nil
			}
			assembled = true
		} else {
			// FR-21/FR-6: record the pend (payer-local, metadata-only) so the follow-up conformant
			// ClaimUpdate (pas-claim-update) BeginClaimUpdate can bind to a REAL prior pend.
			return LegResult{
				ResponseSubjectForeign: true,
				Response:               answer,
				Commit:                 func() error { return n.store.RecordPendedClaim(subjectPCI, corrID) },
			}, nil
		}
	}
	parsed, err := shnsdk.ParseClaimResponse(response)
	if err != nil {
		// A 2xx we cannot translate is an upstream problem, not ours → 502.
		return LegResult{Status: http.StatusBadGateway, Message: "upstream payer PAS submit response untranslatable"}, nil
	}
	// A decision that reads as a denial yet states an authorization number in the
	// ClaimResponse's own preAuthRef field is contradictory: it may be a partial
	// certification, whose authorization a denial reading would discard. These
	// bytes do not say which, and a denial projected over an authorization the
	// payer issued would reach the member as a refusal of covered care, so refuse
	// the leg instead of choosing. A payer that states the number on its review
	// action is read as the partial certification it is; this refusal is only for
	// the placement the decision reader cannot yet tell apart, and it goes away
	// once that reader takes both placements.
	if parsed.Outcome == "denied" && payerStatedAuthNumber(response) != "" {
		return LegResult{Status: http.StatusBadGateway, Message: "payer decision states both a denial and an authorization number"}, nil
	}
	if cpt == "" {
		// No recognized {CPT,HCPCS} product coding → no EOB side-effect (soft — relay complete, no Store write).
		return LegResult{Response: answer, ResponseSubjectForeign: true, ResponseAssembled: assembled}, nil
	}
	eobJSON, err := n.projectDecisionEOB(corrID, "Patient/"+s.member, procSystem, cpt, cptDisplay, parsed)
	if err != nil {
		// The projection states the payer's own decision detail — its review
		// action, its reason codes and its notes. Detail the decision resource
		// cannot carry (a reason code outside the bound code systems, a note
		// type outside the resource's) is upstream content, so refuse loudly
		// (502) rather than drop it or substitute a code the payer never sent.
		return LegResult{Status: http.StatusBadGateway, Message: "payer decision detail cannot be stated on a decision EOB"}, nil
	}
	eobID := "eob-" + corrID
	return LegResult{
		ResponseSubjectForeign: true,
		ResponseAssembled:      assembled,
		Response:               answer,
		SideEffectFHIR:         [][]byte{eobJSON},
		Commit:                 func() error { return n.store.RecordEOB(subjectPCI, eobID, eobJSON) },
	}, nil
}

// assembledPASAnswer seals a terminal PAS assembly. It replaces the payer's
// pended answer, which the ownership table still lists as an interim builder.
func assembledPASAnswer(assembled []byte) (relay.Payload, error) {
	return relay.Authored(relay.BuilderInterimPASAssembly, assembled, "application/fhir+json")
}

// projectDecisionEOB synthesises the gateway-local PDex EOB SINGLE-SOURCED from the
// partner's decision: AuthNumber is the partner's parsed preAuthRef (never
// minted), CPTCode + CPTDisplay are the Claim's ServiceRequest procedure (never
// hardcoded), ProcedureSystem flows from the order so a HCPCS order yields a
// HCPCS-system EOB (FR-28), ProcessNotes are only the notes the payer's own
// decision carries, typed as it typed them (FR-22), and a denial states the
// payer's own review action and reason codes instead of a reason code the
// payer never sent. Reason codes the payer supplied on a decision that is NOT a
// denial are detail the resource cannot state, so they are refused here rather
// than dropped — the projection either states the payer's own detail in full or
// refuses. No engine guard on the construction itself — it makes the
// single-sourcing true; the adversarial row makes a mint/pin loud.
func (n *nativeResponder) projectDecisionEOB(corrID, patientRef, procSystem, cpt, cptDisplay string, parsed shnsdk.PriorAuthResult) ([]byte, error) {
	decision, authNumber := shnsdk.PADecisionApproved, parsed.PreAuthRef
	// The EOB carries only the notes the payer's own decision carries (never a
	// fixed appeal text): a non-nil, possibly empty list, each note typed as
	// the payer typed it.
	notes := parsed.ProcessNotes
	if notes == nil {
		notes = []shnsdk.PASProcessNote{}
	}
	// A denial states the payer's own decision: one denialreason for each
	// reason code the payer supplied, plus its review action (or, when it
	// stated none, the X12 code of the denial itself). A non-nil list is what
	// states the decision, so the projection never carries the fixed reason
	// code the payer did not send.
	var denialReasons []shnsdk.PASCoding
	if parsed.Outcome == "denied" {
		decision, authNumber = shnsdk.PADecisionDenied, ""
		denialReasons = parsed.DenialReasons
		if denialReasons == nil {
			denialReasons = []shnsdk.PASCoding{}
		}
	} else if len(parsed.DenialReasons) > 0 {
		// The decision resource states reason codes only on a denial, so reason
		// codes the payer supplied on a decision that is not a denial are detail
		// this projection cannot state. Refuse them here rather than build an EOB
		// that silently omits the payer's own lines.
		return nil, fmt.Errorf("reason codes on a decision that is not a denial")
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
		ProcessNotes:    notes,
		ReviewAction:    parsed.ReviewAction,
		DenialReasons:   denialReasons,
	})
}

// orderIsDeviceRequest reports whether the PAS order entry is a DeviceRequest (the HomeOxygen DME
// provider-data lane) vs a ServiceRequest (the procedure lanes). The single-shot DME lane has no
// amendment leg, so its conditional-coverage pend is auto-resolved on submit; ServiceRequest lanes
// keep the pend for their amendment. "" / unparseable ⇒ false (treated as the procedure default).
func orderIsDeviceRequest(orderJSON []byte) bool {
	var p struct {
		ResourceType string `json:"resourceType"`
	}
	if json.Unmarshal(orderJSON, &p) != nil {
		return false
	}
	return p.ResourceType == "DeviceRequest"
}

// pollClaimResponseUntilApproved polls the partner's GET ClaimResponse/{id} until it resolves to an
// approved (A1) ClaimResponse, or the bound (pendReQueryTimeout/Interval) is exhausted. br-payer
// auto-approves a pended (A4) item after pas.pended-resolution-delay-seconds
// (PasPendedResolutionService → PasResponseBuilder.finalizePendedItems flips A4→A1 IN PLACE on the
// same id). Returns the bare terminal A1 resource for retained-Bundle assembly, or
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
		body, bad, err := n.get(ctx, n.baseURL, "/ClaimResponse/"+claimResponseID, "PAS re-query")
		if err != nil {
			return nil, fmt.Errorf("PAS re-query failed")
		}
		if bad.Status != 0 {
			return nil, fmt.Errorf("PAS re-query upstream status %d", bad.Status)
		}
		var resource map[string]any
		if decodePASObject(body, &resource) != nil || resource["resourceType"] != "ClaimResponse" || resource["id"] != claimResponseID {
			return nil, fmt.Errorf("invalid PAS polling response identity")
		}
		pending, _, perr := shnsdk.ParsePendedResponse(body)
		if perr != nil {
			return nil, fmt.Errorf("invalid PAS polling decision")
		}
		if !pending {
			res, perr := shnsdk.ParseClaimResponse(body)
			if perr != nil {
				return nil, fmt.Errorf("invalid PAS polling decision")
			}
			if res.Outcome == "approved" {
				return body, nil
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
	if err := decodeMessage(requestFHIR, &b); err != nil {
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

// payerStatedAuthNumber reads the authorization number the payer's own
// ClaimResponse states in its top-level preAuthRef field, from the same
// graph-validated response resource the id is read from. "" when the payer
// states none there, or when the answer carries no readable response graph.
func payerStatedAuthNumber(body []byte) string {
	g, err := readPASGraph(body)
	if err != nil {
		return ""
	}
	ref, _ := g.response.resource["preAuthRef"].(string)
	return ref
}

// claimResponseIDFromPASResponse reads only the graph-validated resource id.
// No fullUrl fallback may create an arbitrary polling request path.
func claimResponseIDFromPASResponse(body []byte) string {
	g, err := readPASGraph(body)
	if err != nil {
		return ""
	}
	return g.response.resource["id"].(string)
}
