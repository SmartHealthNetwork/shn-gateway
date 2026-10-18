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
// ledger plus its locally-projected decision EOB are derived from the answer
// (ownership #1) — Store side-effects, orthogonal to the relay. This file owns the
// shnsdk imports; native.go's read-only legs stay shnsdk-free. The PAS response is
// parsed with the SAME exported parsers the originator uses
// (gateway/engine/originate.go) — no new shnsdk symbol.
package engine

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// handlePASClaimUpdateNative is the CONFORMANT amended re-POST's native-forward
// (the pas-claim-update leg) — the only PA-update native-forward path.
//
// The conformant bundle's Claim.related[prior] key comes from
// readConformantPASUpdateFacts (the engine-local conformant extractor). The leg
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
	// The inbound bind judged the update's content and recorded any defect;
	// this read only takes the prior authorization, refusing what strict
	// refuses.
	f, status, _ := readConformantPASUpdateFacts(requestFHIR, func(rule string) bool {
		return n.conformance.Decide(KindContent, rule, VerdictInvalid) == Refuse
	})
	if status != 0 {
		return LegResult{}, fmt.Errorf("engine: nativePAS parse conformant update bundle: status %d", status) // our fault → 500
	}
	// Payer-edge identity mapping (payoredge.go): map the bundle's Coverage payer
	// identity (and a Claim.insurer naming this payer) BEFORE any store write, so a
	// refusal never leaves a claim pended with no way to release it. Off (the default) ⇒
	// the bundle is sent exactly.
	forward, refused, err := n.payorEdgeRequest(in, payorEdgePASBundle, "application/fhir+json", n.refusesRequest(ctx, RuleInsurer, requestFHIR))
	if err != nil {
		return LegResult{}, err
	}
	if refused.Status != 0 {
		return refused, nil
	}
	related := f.relatedClaim
	// The ledger records the amendment; it never decides whether the payer sees
	// it. An amendment that binds the pend this gateway recorded owns that row
	// until the payer answers. One that does not — no pend here, one the payer
	// already decided, another amendment still with the payer, or a ledger that
	// could not be read — is forwarded all the same, the payer decides, and the
	// reason is noted for the operator. Its answer is recorded only onto the
	// authorization this gateway already holds for the same requester, or for
	// none (unboundRecordable): an unbound amendment never creates a row, so it
	// can never make a correlation name a second authorization. An amendment of
	// another requester's authorization binds nothing and writes nothing.
	claimed, why := false, pendRefusalNoPriorClaim
	recordable := false
	if related != "" {
		// The pend ledger's reads and the claim it takes are its stage.
		stopLedger := exchangeOf(ctx).stage(addLedger)
		requester := requesterHolderOf(ctx)
		if n.heldByAnotherRequester(subjectPCI, related, requester) {
			// Another requester's authorization: never bound, never written.
			why = pendRefusalOtherRequester
		} else {
			var err error
			if claimed, why, err = n.beginClaimUpdate(subjectPCI, related); err != nil {
				claimed, why = false, pendRefusalLedgerUnavailable
			}
			recordable = claimed || n.unboundRecordable(subjectPCI, related, requester)
		}
		stopLedger()
	}
	var release func() error
	if claimed {
		// Once: a re-pend's Commit releases the claim itself, and the deferred
		// Rollback must not release it again (another amendment may hold it by
		// then).
		var once sync.Once
		var releaseErr error
		release = func() error {
			once.Do(func() {
				if releaseErr = n.store.ReleaseClaimUpdate(subjectPCI, related); releaseErr != nil {
					log.Printf("gateway: pas-claim-update: correlation %s: the amendment's claim could not be released: %v", corrID, releaseErr)
				}
			})
			return releaseErr
		}
	} else {
		log.Printf("gateway: pas-claim-update: correlation %s: amendment forwarded without binding a local pend (%s)", corrID, why)
		noteOn(ctx, PendAmendmentUnboundEvent+":"+string(why))
	}
	var rollback func()
	if release != nil {
		rollback = func() { _ = release() }
	}

	// Endpoint evidence: prefer the probe-retained, same-origin-validated #<line> $submit
	// endpoint for THIS routed pa.pas line (evidence absent ⇒ the PAS base, which is n.baseURL unless PAYER_DAVINCI_PAS_BASE_URL is set).
	// Whatever the payer answers is relayed, a conflict included: whether and when
	// to send the amendment again is the requester's decision.
	submitURL := n.resolvedURL(ctx, "pa.pas", n.pasBase(), "/Claim/$submit")
	up, bad, err := n.post(ctx, submitURL, "", forward, "pas-claim-update", "PAS update")
	if err != nil {
		return LegResult{Rollback: rollback}, err // post-Begin fault MUST still release the claim
	}
	if bad.Status != 0 {
		bad.Rollback = rollback // a post-Begin partner non-2xx MUST release the claim (relay path)
		return bad, nil
	}
	// FR-G28: validate the complete partner Bundle without changing its bytes. A
	// refusal is logged here, with the correlation id and the reference that
	// dangled, before the framed error goes back. Below strict an answer this
	// gateway cannot read is relayed exactly and nothing is written from it.
	read := answerGraphRead(answerCandidateLinesFrom(ctx, up.raw), up.raw)
	answerLine := read.Line
	n.recordLaterLine(ctx, RuleAnswerShape, read, up.raw)
	response, lr := validateNativePASResponseAt(up.raw, answerLine)
	if lr.Status != 0 {
		if n.refusesAnswer(ctx, RuleAnswerShape, up.raw) {
			_, lr = validateRelayedPASResponse(corrID, "pas-claim-update", answerLine, up.raw)
			lr.Rollback = rollback
			return lr, nil
		}
		return relayUnread(ctx, up, RuleAnswerShape, rollback), nil
	}
	// The answer is the payer's own bytes, whatever they say.
	answer := LegResult{
		ResponseSubjectForeign: true,
		Response:               relay.Exact(up.body, up.contentType),
		Rollback:               rollback,
	}
	pended, _, err := shnsdk.ParsePendedResponse(response)
	if err != nil {
		return LegResult{Status: http.StatusBadGateway, Message: "upstream payer PAS update response unparseable", Rollback: rollback}, nil
	}
	requester := requesterHolderOf(ctx)
	answerKeys, created := pasAnswerKeys(requester, response)
	if pended {
		// The payer re-pended: the claim returns to pended so a later amendment can
		// still bind, and the new response's identifiers join the ones already
		// recorded. Release is the ledger's own transition here, not a rollback, so
		// it runs as the Commit — the Rollback stays armed for the paths that never
		// reach it. An amendment that did not bind releases nothing: the re-pend is
		// recorded by the ledger's rules, which never reopen another amendment in
		// progress.
		if !recordable {
			return answer, nil // relayed; there is no prior claim to key a record on
		}
		pend := recordPASPend(ctx, n.store, subjectPCI, related,
			mergePendKeys(requester, answerKeys, pasRequestKeys(requestFHIR)), created)
		answer.Commit = pend
		if claimed {
			answer.Commit = func() error {
				if rerr := release(); rerr != nil {
					// The hold stays (it lapses on its own), but the payer's
					// re-pend is still recorded: its keys are indexed and the
					// held state is left alone.
					_ = pend()
					return rerr
				}
				return pend()
			}
		}
		return answer, nil
	}
	parsed, err := shnsdk.ParseClaimResponse(response)
	if err != nil {
		return LegResult{Status: http.StatusBadGateway, Message: "upstream payer PAS update response untranslatable", Rollback: rollback}, nil
	}
	if parsed.Outcome == "denied" && payerStatedAuthNumber(response) != "" {
		// As on the submit leg: relayed at every level, nothing written.
		return relayDecisionUnrecorded(ctx, up, rollback), nil
	}
	// A terminal decision on the update leg: relayed, and recorded as the decision
	// the payer dated. No EOB on the update leg. Rollback stays armed so a
	// post-Begin response-leg failure still releases.
	if recordable {
		answer.Commit = recordPASDecision(ctx, n.store, subjectPCI, related, pendOutcomeOf(parsed.Outcome), created, decisionKeys(requester, answerKeys, requestFHIR, parsed.PreAuthRef), nil)
	}
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

// PendAmendmentUnboundEvent is noted when an amendment reached the payer without
// binding a pend this gateway recorded; the reason (a PendRefusal, or
// pendRefusalLedgerUnavailable) follows a colon. The payer's answer is relayed
// and recorded either way.
const PendAmendmentUnboundEvent = "pend.amendment-unbound"

// The reasons an amendment did not bind beyond the ledger's own: the ledger
// could not be read, or the update names no prior claim to bind.
const (
	pendRefusalLedgerUnavailable PendRefusal = "ledger-unavailable"
	pendRefusalNoPriorClaim      PendRefusal = "no-prior-claim"
	pendRefusalOtherRequester    PendRefusal = "other-requester"
)

// heldByAnotherRequester reports whether the authorization an amendment names is
// recorded for a different requester. Such an amendment is forwarded all the
// same, but binds and records nothing: one requester never moves another's
// authorization.
func (n *nativeResponder) heldByAnotherRequester(subjectPCI, related, requester string) bool {
	ledger, ok := LedgerOf(n.store)
	if !ok || requester == "" {
		return false
	}
	rec, found, err := ledger.PendRecordOf(subjectPCI, related)
	return err == nil && found && rec.RequesterHolder != "" && rec.RequesterHolder != requester
}

// unboundRecordable reports whether the payer's answer to an amendment that did
// not bind may be recorded: only onto an authorization this gateway already
// holds for this patient under that correlation, recorded for the same
// requester (or for none). Anything else records nothing.
func (n *nativeResponder) unboundRecordable(subjectPCI, related, requester string) bool {
	ledger, ok := LedgerOf(n.store)
	if !ok || requester == "" {
		return false
	}
	rec, found, err := ledger.PendRecordOf(subjectPCI, related)
	if err != nil || !found {
		return false
	}
	return rec.RequesterHolder == "" || rec.RequesterHolder == requester
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
// The conformant bundle is read by readConformantPASSubjects (the engine-local conformant
// extractor): the EOB patientRef is the BOUND member (R-7, request-side — never the response member,
// which a real RI answers in its own namespace), and the procedure {system,code} comes from the
// conformant bundle's ServiceRequest (CPT or HCPCS — the SAME 72148 the EOB-provenance canary checks
// for the CPT persona). handlePASNativeInbound egress-$validates SideEffectFHIR + Commits.
func (n *nativeResponder) handlePASClaimNative(ctx context.Context, corrID, subjectPCI string, in relay.Body, requestFHIR []byte) (LegResult, error) {
	// The inbound bind judged the bundle's content and recorded any defect; this
	// read only takes the member and the order, refusing what strict refuses.
	s, status, msg := readConformantPASSubjects(requestFHIR, n.mapsPayerIdentity(), func(rule string) bool {
		return n.conformance.Decide(KindContent, rule, VerdictInvalid) == Refuse
	})
	if status != 0 {
		return LegResult{Status: status, Message: msg}, nil
	}
	// Payer-edge identity mapping (payoredge.go): map the bundle's Coverage payer
	// identity (and a Claim.insurer naming this payer) before it is posted (and before
	// any store side-effect below). Off (the default) ⇒ the bundle is sent exactly.
	forward, refused, err := n.payorEdgeRequest(in, payorEdgePASBundle, "application/fhir+json", n.refusesRequest(ctx, RuleInsurer, requestFHIR))
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
	// the framed error goes back — the bytes themselves are not relayed. Below
	// strict an answer this gateway cannot read is relayed exactly and nothing is
	// written from it.
	read := answerGraphRead(answerCandidateLinesFrom(ctx, up.raw), up.raw)
	answerLine := read.Line
	n.recordLaterLine(ctx, RuleAnswerShape, read, up.raw)
	response, lr := validateNativePASResponseAt(up.raw, answerLine)
	if lr.Status != 0 {
		if n.refusesAnswer(ctx, RuleAnswerShape, up.raw) {
			_, lr = validateRelayedPASResponse(corrID, "pas-claim", answerLine, up.raw)
			return lr, nil
		}
		return relayUnread(ctx, up, RuleAnswerShape, nil), nil
	}
	// The answer to send: the payer's Bundle, exactly.
	answer := LegResult{
		ResponseSubjectForeign: true,
		Response:               relay.Exact(up.body, up.contentType),
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
	// ClaimResponse's own preAuthRef field may be a partial certification, whose
	// authorization a denial reading would discard. These bytes do not say
	// which, so this gateway's reading cannot hold the decision. That is a limit
	// of this gateway's own record, never a reason to refuse the payer's decision:
	// it is relayed exactly at every level, nothing is written from it (no
	// decision, no EOB), and the operator is told why.
	if parsed.Outcome == "denied" && payerStatedAuthNumber(response) != "" {
		return relayDecisionUnrecorded(ctx, up, nil), nil
	}
	outcome := pendOutcomeOf(parsed.Outcome)
	if cpt == "" {
		// No recognized {CPT,HCPCS} product coding → no EOB side-effect (soft — the
		// decision is still recorded, and nothing is invented to carry it).
		answer.Commit = recordPASDecision(ctx, n.store, subjectPCI, corrID, outcome, created, decisionKeys(requester, answerKeys, requestFHIR, parsed.PreAuthRef), nil)
		return answer, nil
	}
	eobJSON, err := n.projectDecisionEOB(corrID, "Patient/"+s.member, procSystem, cpt, cptDisplay, parsed,
		decisionEOBSource{request: requestFHIR, itemIndex: -1, claimResponse: firstClaimResponse(up.raw), answer: up.raw})
	if err != nil {
		// The EOB cannot state the decision as its sources do (eobNotWritten):
		// a required element absent at its source, or payer detail the resource
		// cannot carry as written. This is this gateway's own record, not a
		// judgement of the payer's answer, so it refuses at no level: the
		// decision relays and is recorded without an EOB, and the operator is
		// told which detail.
		noteDetailOn(ctx, DecisionEOBNotWrittenEvent, err.Error())
		answer.Commit = recordPASDecision(ctx, n.store, subjectPCI, corrID, outcome, created, decisionKeys(requester, answerKeys, requestFHIR, parsed.PreAuthRef), nil)
		return answer, nil
	}
	eob := &EOBRecord{SubjectPCI: subjectPCI, EOBID: decisionEOBID(corrID), JSON: eobJSON}
	answer.SideEffectFHIR = [][]byte{eobJSON}
	answer.decisionEOBs = []*EOBRecord{eob}
	answer.Commit = recordPASDecision(ctx, n.store, subjectPCI, corrID, outcome, created, decisionKeys(requester, answerKeys, requestFHIR, parsed.PreAuthRef), eob)
	return answer, nil
}

// projectDecisionEOB builds the gateway-local decision EOB from the payer's
// decision and the request it answers (decisioneob.go): AuthNumber is the
// payer's parsed preAuthRef (never minted), CPTCode + CPTDisplay are the
// Claim's ServiceRequest procedure (never hardcoded), ProcedureSystem flows
// from the order so a HCPCS order yields a HCPCS-system EOB (FR-28),
// ProcessNotes are only the notes the payer's own decision carries, typed as
// it typed them (FR-22), the item's adjudication is the payer's own (its review
// action re-expressed in the EOB's own extension), and every other element is read from the request Claim or the payer's
// ClaimResponse. The resource claims no profile.
func (n *nativeResponder) projectDecisionEOB(corrID, patientRef, procSystem, cpt, cptDisplay string, parsed shnsdk.PriorAuthResult, src decisionEOBSource) ([]byte, error) {
	return decisionEOB(corrID, patientRef, procSystem, cpt, cptDisplay, parsed, src)
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

// refusesAnswer reports whether the payer's answer, which broke rule, is
// refused. The answer is this participant's own content: the rule is not
// checked at none, recorded at observe and refused at strict (the finding names
// the answer as the participant's own). A repeated member name is read one way
// only and refuses at every level.
func (n *nativeResponder) refusesAnswer(ctx context.Context, rule string, raw []byte) bool {
	if repeatsAMember(ctx, raw) {
		return true
	}
	fc := findingContextFrom(ctx)
	fc.Whose = "own"
	return guardDefect(withFindingContext(ctx, fc), n.conformance, n.emitFinding, KindContent, rule, VerdictInvalid, raw)
}

// recordLaterLine records that the payer's answer met rule only on a
// candidate line after its first (laterLineFinding), as its own content,
// wherever the level runs the rule.
func (n *nativeResponder) recordLaterLine(ctx context.Context, rule string, read answerLineRead, raw []byte) {
	if n.emitFinding == nil || !n.conformance.Runs(KindContent, rule) {
		return
	}
	fc := findingContextFrom(ctx)
	fc.Whose = "own"
	if f, ok := laterLineFinding(fc, n.conformance.Level().String(), rule, read, raw); ok {
		countingEmit(ctx, n.emitFinding)(f)
	}
}

// refusesRequest decides a defect of rule in the request this payer received
// (raw), when called: the request is the requester's content, so the rule is
// not checked at none, recorded at observe and refused at strict, and the
// finding names the request as the peer's (the leg context handleInbound set).
func (n *nativeResponder) refusesRequest(ctx context.Context, rule string, raw []byte) func() bool {
	return func() bool {
		return guardDefect(ctx, n.conformance, n.emitFinding, KindContent, rule, VerdictInvalid, raw)
	}
}

// decisionReadAsDenialWithAuthNumber is the finding detail for a payer
// decision this gateway's reading cannot hold.
const decisionReadAsDenialWithAuthNumber = "decision not recorded: the decision reads as a denial yet its ClaimResponse states preAuthRef; no decision or EOB written"

// relayDecisionUnrecorded relays a payer decision this gateway's own reading
// cannot hold, exactly and at every level: nothing is written from it, and the
// leg notes why (pend.decision-not-recorded). It never refuses the decision.
func relayDecisionUnrecorded(ctx context.Context, up upstreamReply, rollback func()) LegResult {
	noteDetailOn(ctx, pendDecisionNotRecordedEvent, decisionReadAsDenialWithAuthNumber)
	return relayUnread(ctx, up, RuleEOBDecision, rollback)
}

// relayUnread is the payer's answer relayed exactly as it arrived, not read for
// this gateway's own records: it carries no Commit and no side-effect, so no
// pend, decision or EOB is written, and the inbound handler reports the write
// it skipped (pasLeg.skipWrite, pa.local-write-skipped). rollback releases a
// claim the leg acquired.
func relayUnread(ctx context.Context, up upstreamReply, rule string, rollback func()) LegResult {
	pasLegOf(ctx).skipWrite(rule)
	return LegResult{
		ResponseSubjectForeign: true,
		Response:               relay.Exact(up.body, up.contentType),
		Rollback:               rollback,
	}
}
