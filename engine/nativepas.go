// nativepas.go — the native PAS legs of the native-forward Responder.
//
// They forward pas-claim / pas-claim-update to the participant's own
// /Claim/$submit and RELAY WHAT COMES BACK. A pend, a re-pend, a denial and an
// approval are all the payer's answer to the operation the requester performed;
// this gateway states none of them differently and substitutes none of them with
// a later answer to a different operation. Nothing here polls and nothing here
// assembles: a decision that arrives later comes from a separate `Claim/$inquire`
// the requester performs explicitly (inquire.go).
//
// The partner Bundle is validated as a complete response graph without changing
// its bytes (validateNativePASResponse, FR-G28), and the payer gateway's own pend
// ledger plus its locally-projected PDex EOB are derived from the answer
// (ownership #1) — Store side-effects, orthogonal to the relay. This file owns the
// shnsdk imports; native.go's read-only legs stay shnsdk-free. The PAS response is
// parsed with the SAME exported parsers the originator uses
// (gateway/engine/originate.go) — no new shnsdk symbol.
package engine

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// handlePASClaimUpdateNative is the CONFORMANT amended re-POST's native-forward
// (the pas-claim-update leg) — the only PA-update native-forward path.
//
// The conformant bundle's Claim.related[prior] key comes from
// parseConformantPASUpdateFacts (the engine-local conformant extractor). The leg
// binds that prior authorization for one amendment, relays the amendment to the
// participant's own /Claim/$submit, and RELAYS WHATEVER THE PAYER ANSWERS — a
// re-pend, a denial and an approval alike. SHN's own "amendment still
// insufficient" 422 is gone: it replaced the payer's word with this gateway's.
//
// "Pure relay" is a WIRE property; the ledger effect is an ORTHOGONAL Store
// side-effect. NO EOB on the update leg. CRITICAL: Rollback:release is armed on
// EVERY post-Begin exit — including a no-response fault AND a relayed partner
// non-2xx — because post() never attaches Rollback itself; a bare
// `return LegResult{}, err` or `return bad, nil` after Begin would strand the
// claim permanently.
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
	claimed, why, err := n.beginClaimUpdate(subjectPCI, related)
	if err != nil {
		return LegResult{Status: http.StatusBadGateway, Message: "holder write failed (begin update)"}, nil
	}
	if !claimed {
		// Fail-safe: divergence, no prior pend, a concurrent amendment or an
		// authorization the payer has already decided ⇒ 409 stating WHICH, never a
		// silent transition. Amending an authorization the payer decided is the
		// limitation this gateway discloses rather than papers over.
		return LegResult{Status: http.StatusConflict, Message: claimUpdateRefusal(why)}, nil
	}
	release := func() { _ = n.store.ReleaseClaimUpdate(subjectPCI, related) }

	// Endpoint evidence: prefer the probe-retained, same-origin-validated #<line> $submit
	// endpoint for THIS routed pa.pas line (evidence absent ⇒ the PAS base, which is n.baseURL unless PAYER_DAVINCI_PAS_BASE_URL is set).
	submitURL := n.resolvedURL(ctx, "pa.pas", n.pasBase(), "/Claim/$submit")
	up, bad, err := n.post(ctx, submitURL, "", forward, "pas-claim-update", "PAS update")
	if err != nil {
		return LegResult{Rollback: release}, err // post-Begin fault MUST still release the claim
	}
	if bad.Status == http.StatusConflict && isPayerVersionConflict(up.raw) {
		// The payer's store refused the amendment's write because its own pend-resolution
		// timer wrote the same ClaimResponse first (nativepas_conflict.go): the amendment was
		// not persisted, so re-issue THE IDENTICAL payload exactly once after that write has
		// landed. Whatever the re-issue answers takes the ordinary path below — relayed
		// either way. The second attempt is recorded for the operator.
		if werr := sleepCtx(ctx, payerVersionConflictRetryDelay); werr != nil {
			return LegResult{Rollback: release}, werr
		}
		noteOn(ctx, RetryVersionConflictEvent)
		up, bad, err = n.post(ctx, submitURL, "", forward, "pas-claim-update", "PAS update (re-issued after the payer's version conflict)")
		if err != nil {
			return LegResult{Rollback: release}, err
		}
	}
	if bad.Status != 0 {
		bad.Rollback = release // a post-Begin partner non-2xx MUST release the claim (relay path)
		return bad, nil
	}
	// FR-G28: validate the complete partner Bundle without changing its bytes. A
	// refusal is logged here, with the correlation id and the reference that
	// dangled, before the framed error goes back.
	response, lr := validateRelayedPASResponse(corrID, "pas-claim-update", up.raw)
	if lr.Status != 0 {
		lr.Rollback = release
		return lr, nil
	}
	// The answer is the payer's own bytes, whatever they say.
	answer := LegResult{
		ResponseSubjectForeign: true,
		Response:               relay.Exact(up.body, "application/fhir+json"),
		Rollback:               release,
	}
	pended, _, err := shnsdk.ParsePendedResponse(response)
	if err != nil {
		return LegResult{Status: http.StatusBadGateway, Message: "upstream payer PAS update response unparseable", Rollback: release}, nil
	}
	requester := requesterHolderOf(ctx)
	answerKeys, created := pasAnswerKeys(requester, response)
	if pended {
		// The payer re-pended: the claim returns to pended so a later amendment can
		// still bind, and the new response's identifiers join the ones already
		// recorded. Release is the ledger's own transition here, not a rollback, so
		// it runs as the Commit — the Rollback stays armed for the paths that never
		// reach it.
		answer.Commit = func() error {
			if rerr := n.store.ReleaseClaimUpdate(subjectPCI, related); rerr != nil {
				return rerr
			}
			return recordPASPend(ctx, n.store, subjectPCI, related,
				mergePendKeys(requester, answerKeys, pasRequestKeys(requestFHIR)), created)()
		}
		return answer, nil
	}
	parsed, err := shnsdk.ParseClaimResponse(response)
	if err != nil {
		return LegResult{Status: http.StatusBadGateway, Message: "upstream payer PAS update response untranslatable", Rollback: release}, nil
	}
	if parsed.Outcome == "denied" && payerStatedAuthNumber(response) != "" {
		return LegResult{Status: http.StatusBadGateway, Message: "payer decision states both a denial and an authorization number", Rollback: release}, nil
	}
	// A terminal decision on the update leg: relayed, and recorded as the decision
	// the payer dated. No EOB on the update leg. Rollback stays armed so a
	// post-Begin response-leg failure still releases.
	answer.Commit = recordPASDecision(ctx, n.store, subjectPCI, related, pendOutcomeOf(parsed.Outcome), created, nil)
	return answer, nil
}

// beginClaimUpdate binds the prior authorization for one amendment, reporting WHY
// it refused. A Store with a pend ledger answers the reason itself; one without
// can only say "not pended", which is the same answer it has always given.
func (n *nativeResponder) beginClaimUpdate(subjectPCI, related string) (bool, PendRefusal, error) {
	if ledger, ok := LedgerOf(n.store); ok {
		return ledger.BeginClaimUpdateReason(subjectPCI, related)
	}
	claimed, err := n.store.BeginClaimUpdate(subjectPCI, related)
	if err != nil || claimed {
		return claimed, PendRefusalNone, err
	}
	return false, PendRefusalNotPended, nil
}

// claimUpdateRefusal states why an amendment could not bind. The reasons stay
// distinguishable: an authorization this payer has already decided is a different
// answer from one it never pended, and a requester can act on the difference.
func claimUpdateRefusal(why PendRefusal) string {
	switch why {
	case PendRefusalDecided:
		return "claim already decided; amendment of a decided authorization is not supported"
	case PendRefusalInProgress:
		return "an amendment of this authorization is already in progress"
	default:
		return "ClaimUpdate references no pending claim available for this patient"
	}
}

// pendOutcomeOf maps a parsed PAS decision to the ledger's outcome. Only approved
// and denied are decisions; anything else is recorded as approved would be this
// gateway deciding, so the pre-existing reading (anything not denied is an
// approval, the same reading the decision EOB takes) is kept in ONE place.
func pendOutcomeOf(outcome string) string {
	if outcome == "denied" {
		return PendOutcomeDenied
	}
	return PendOutcomeApproved
}

// handlePASClaimNative is the CONFORMANT PAS submit leg's native-forward: post the verbatim
// conformant bundle to the participant's own /Claim/$submit, validate the native response Bundle
// as a complete graph, RELAY IT, and project the Store side-effects. The wire is byte-verbatim to
// the payer's own system (the FR-G25 fidelity asymmetry, same as crd-order-select); the EOB and
// the pend-ledger writes are ORTHOGONAL Store side-effects derived from the response (the governing
// principle).
//
// Whatever the payer answered is what the requester receives. A pend is recorded so a later
// amendment binds and a later `Claim/$inquire` resolves; a decision is recorded with its EOB in one
// write. Nothing is polled and nothing is assembled.
//
// The conformant bundle is read by parseConformantPASSubjects (the engine-local conformant
// extractor): the EOB patientRef is the BOUND member (R-7, request-side — never the response member,
// which a real RI answers in its own namespace), and the procedure {system,code} comes from the
// conformant bundle's ServiceRequest (CPT or HCPCS — the SAME 72148 the EOB-provenance canary checks
// for the CPT persona). handlePASNativeInbound egress-$validates SideEffectFHIR + Commits.
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
	// endpoint for THIS routed pa.pas line (evidence absent ⇒ the PAS base, which is n.baseURL unless PAYER_DAVINCI_PAS_BASE_URL is set).
	submitURL := n.resolvedURL(ctx, "pa.pas", n.pasBase(), "/Claim/$submit")
	up, bad, err := n.post(ctx, submitURL, "", forward, "pas-claim", "PAS submit")
	if err != nil {
		return LegResult{}, err // no-response fault → engine 500 → "hub routing failed"
	}
	if bad.Status != 0 {
		return bad, nil // upstream non-2xx → relayable LegResult (Response carries the body)
	}
	// FR-G28: the payer's Bundle must carry every resource it names. A refusal is
	// logged here, with the correlation id and the reference that dangled, before
	// the framed error goes back — the bytes themselves are not relayed.
	response, lr := validateRelayedPASResponse(corrID, "pas-claim", up.raw)
	if lr.Status != 0 {
		return lr, nil
	}
	// The answer to send: the payer's Bundle, exactly.
	answer := LegResult{
		ResponseSubjectForeign: true,
		Response:               relay.Exact(up.body, "application/fhir+json"),
	}
	pended, _, err := shnsdk.ParsePendedResponse(response)
	if err != nil {
		return LegResult{Status: http.StatusBadGateway, Message: "upstream payer PAS submit response unparseable"}, nil
	}
	requester := requesterHolderOf(ctx)
	answerKeys, created := pasAnswerKeys(requester, response)
	if pended {
		// FR-21/FR-6: record the pend (payer-local, metadata-only) under the keys the
		// payer's answer and this submission state, so the follow-up conformant
		// ClaimUpdate binds to a REAL prior pend and a `Claim/$inquire` about the same
		// authorization resolves to it.
		answer.Commit = recordPASPend(ctx, n.store, subjectPCI, corrID,
			mergePendKeys(requester, answerKeys, pasRequestKeys(requestFHIR)), created)
		return answer, nil
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
	outcome := pendOutcomeOf(parsed.Outcome)
	if cpt == "" {
		// No recognized {CPT,HCPCS} product coding → no EOB side-effect (soft — the
		// decision is still recorded, and nothing is invented to carry it).
		answer.Commit = recordPASDecision(ctx, n.store, subjectPCI, corrID, outcome, created, nil)
		return answer, nil
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
	eob := &EOBRecord{SubjectPCI: subjectPCI, EOBID: decisionEOBID(corrID), JSON: eobJSON}
	answer.SideEffectFHIR = [][]byte{eobJSON}
	answer.Commit = recordPASDecision(ctx, n.store, subjectPCI, corrID, outcome, created, eob)
	return answer, nil
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
	return decisionEOB(n.clock, corrID, patientRef, procSystem, cpt, cptDisplay, parsed)
}

// decisionEOB is the projection itself, shared by every leg that records a payer
// decision: the submit leg, and the inquiry leg that learns a decision later
// (inquire.go). One projection means the two legs cannot state a payer's own
// decision differently.
func decisionEOB(clock func() time.Time, corrID, patientRef, procSystem, cpt, cptDisplay string, parsed shnsdk.PriorAuthResult) ([]byte, error) {
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
		ID:              decisionEOBID(corrID),
		PatientRef:      patientRef,
		CoverageRef:     "Coverage/" + strings.TrimPrefix(patientRef, "Patient/"),
		CPTCode:         cpt,
		CPTDisplay:      cptDisplay,
		ProcedureSystem: procSystem,
		Decision:        decision,
		AuthNumber:      authNumber,
		Created:         clock(),
		ProcessNotes:    notes,
		ReviewAction:    parsed.ReviewAction,
		DenialReasons:   denialReasons,
	})
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
