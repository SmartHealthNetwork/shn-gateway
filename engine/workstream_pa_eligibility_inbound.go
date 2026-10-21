package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// handleEligibilityInbound is the UC-01 coverage-eligibility inbound logic,
// unchanged from the original handleInbound body.
func (g *Gateway) handleEligibilityInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, cerJSON []byte, answerTok string) {
	ctx := r.Context()

	// Data minimization at the holder boundary: bind the token subject to the
	// payload's patient via a CHEAP json.Unmarshal-level parse BEFORE any external
	// $validate. A wrong-patient payload must be rejected here so it never leaves
	// the holder boundary for the (shared) validator. The Parse* helpers operate on
	// un-$validate'd JSON and return errors (not panics) on malformed input, so a
	// malformed payload fails closed with 400 before $validate — nothing leaks.
	// The requests the operation's input submits (the request itself, a
	// Bundle of one with its referenced resources or of several, or a
	// Parameters holding either), and the member each one's patient names, in
	// any reference form (patientref.go, eligibilitySubmissionMember). The
	// input is forwarded as it arrived; the requests are what this gateway
	// reads and answers.
	//
	// A request that repeats a member name, exactly or under case folding,
	// can be read two ways, so two readers could bind two members from it:
	// message integrity, refused at every level before anything is read from
	// it. The refusal names the member, never a value.
	if repeatsAName(cerJSON) {
		dup, ok := repeatedMember(cerJSON)
		if !ok {
			dup = "a member name"
		}
		exchangeOf(ctx).refusing(RefusalIntegrity)
		g.refuseInbound(w, r, legEligibility, env, tok, answerTok, http.StatusBadRequest, "eligibility request repeats "+dup+": it can be read two ways, so it is not read", nil)
		return
	}
	// A request naming its type or patient only in another case names no
	// patient (member names are case-sensitive): the patient the exchange is
	// for cannot be identified, so it is refused at every level (the
	// patient-match rule), never bound by a reading that folds case
	// (eligibilityNamedInAnotherCase).
	unread := func(err error) {
		if msg, ok := eligibilityNamedInAnotherCase(err); ok {
			exchangeOf(ctx).refusing(RefusalAuthority)
			g.refuseInbound(w, r, legEligibility, env, tok, answerTok, http.StatusBadRequest, msg, nil)
			return
		}
		g.refuseInbound(w, r, legEligibility, env, tok, answerTok, http.StatusBadRequest, "parse member failed", nil)
	}
	subs, err := eligibilitySubmissions(cerJSON)
	if err != nil {
		unread(err)
		return
	}
	if len(subs) > maxEligibilityBatch {
		exchangeOf(ctx).refusing(RefusalLimit)
		g.refuseInbound(w, r, legEligibility, env, tok, answerTok, http.StatusRequestEntityTooLarge, eligibilityBatchTooLarge(len(subs)), nil)
		return
	}
	members := make([]string, len(subs))
	for i, sub := range subs {
		if members[i], err = eligibilitySubmissionMember(sub); err != nil {
			unread(err)
			return
		}
	}

	// A payer that declares its own eligibility endpoint answers the requests
	// itself, as it would direct ones (forwardEligibilityInbound).
	if f, ok := g.cfg.Responder.(eligibilityForwarder); ok && f.forwardsEligibility() {
		g.forwardEligibilityInbound(w, r, env, tok, cerJSON, subs, members, answerTok, f)
		return
	}

	// A payer gateway that keeps no system of record has no records
	// to answer eligibility from itself: the operation is not offered, as a
	// FHIR server without it would say. Not a network rule.
	if !hasSystemOfRecord(g.cfg.SoR) {
		g.refuseInbound(w, r, legEligibility, env, tok, answerTok, http.StatusNotImplemented, refusalEligibilityNotOffered, nil)
		return
	}
	// This gateway answers from the payer's records one request at a time:
	// a batch is not answered here (the payer gateway's own answer, as a FHIR
	// server not offering it would say; not a network rule). A payer whose own
	// endpoint answers batches declares it (PAYER_ELIGIBILITY_URL).
	if len(subs) > 1 {
		g.refuseInbound(w, r, legEligibility, env, tok, answerTok, http.StatusNotImplemented, refusalEligibilityBatchNotAnswered, nil)
		return
	}
	submitted, member := subs[0].request, members[0]

	// The payer answers about the member the request names, from its own
	// records: a member they do not hold is its own answer (unknown member).
	// A token naming another patient is carried, as on the prior-authorization
	// legs: authority here is the operation's, no consent is checked, and the
	// two sides' records may legitimately bind one person differently. The
	// payer's binding is observed and named on the answer's involved list
	// (noteSubjectBinding).
	pci, _, found, readErr := ReadSystemOfRecord(g.cfg.SoR).ResolvePatientContext(r.Context(), member)
	if g.refuseSoRFailure(w, r, legEligibility, env, tok, answerTok, readErr) {
		return
	}
	if !found {
		g.refuseInbound(w, r, legEligibility, env, tok, answerTok, http.StatusBadRequest, refusalUnknownMember, nil)
		return
	}
	involvedCollectorFrom(ctx).bound(pci, true)
	g.noteSubjectBinding(ctx, "coverage-eligibility", env.Metadata.CorrelationID, tok.Subject, pci)

	// Only AFTER the subject binds is the request validated
	// (validateEligibilityRequest).
	if !g.validateEligibilityRequest(w, r, env, tok, submitted, answerTok) {
		return
	}

	boundPatientRef := "Patient/" + member

	// Coverage-derived direct read (R11): eligibility is a data-plane
	// read of the payer's OWN SoR, not adjudication, so it is answered directly —
	// NEVER through the injected Adjudicator/Responder occupant (which is consulted
	// only for the PA legs: OrderSelect/Questionnaire/PriorAuth). One Coverage read
	// feeds both facts, the payer twin of FR-G40's "one payer fact, read once":
	// CoverageInforce is the verdict, OpenCoverage's payor (parsed the SAME way the
	// origination-side machinery does, gateway.go's recipientForWith /
	// shnsdk.ParsePayerIdentifier) is the insurer identity — replacing the
	// hardcoded Organization/payer literal BuildEligibilityResponse used to stamp.
	inforce, reason, readErr := ReadSystemOfRecord(g.cfg.SoR).CoverageInforceContext(r.Context(), member)
	if g.refuseSoRFailure(w, r, legEligibility, env, tok, answerTok, readErr) {
		return
	}
	coverageJSON, hasCoverage, status, msg := g.memberCoverage(r.Context(), member)
	if status != 0 {
		g.refuseInbound(w, r, legEligibility, env, tok, answerTok, status, msg, nil)
		return
	}
	var insurer shnsdk.PayerIdentifier
	switch {
	case hasCoverage:
		// The member HAS a Coverage record: its own payor is the insurer. An
		// unresolvable payor on an EXISTING Coverage fails closed (R4:
		// CoverageEligibilityResponse.insurer is 1..1) — a hollow or
		// stale-literal response is never built.
		var ok bool
		resolve, readErr := sorReferenceCallback(ctx, g.cfg.SoR)
		insurer, ok = shnsdk.ParsePayerIdentifier(coverageJSON, resolve)
		if g.refuseSoRFailure(w, r, legEligibility, env, tok, answerTok, *readErr) {
			return
		}
		if !ok {
			g.refuseInbound(w, r, legEligibility, env, tok, answerTok, http.StatusUnprocessableEntity, noPayerIdentifier, nil)
			return
		}
	default:
		// No Coverage row at all (a genuinely unenrolled member, or the
		// MBR-UC04/MBR-UC08-class fixture gap — the member itself already resolved,
		// via ResolvePatient above, so this is a Coverage absence, not an unknown
		// member) is the PRE-EXISTING business answer: a valid not-covered response
		// (CoverageInforce already returns (false,"") for this case — no disposition
		// text, matching the pre-promotion behavior), never a 422. There is no
		// member Coverage to name an insurer from, so this reads the payer's OWN
		// well-known Organization instead (Organization/payer — the same literal
		// every other payor reference in this codebase already resolves against,
		// seeded with an identifier by internal/fhirseed) — one self-read, not a
		// fabricated identity.
		orgJSON, orgFound, readErr := ReadSystemOfRecord(g.cfg.SoR).ResolveByReferenceContext(r.Context(), "Organization/payer")
		if g.refuseSoRFailure(w, r, legEligibility, env, tok, answerTok, readErr) {
			return
		}
		if !orgFound {
			g.refuseInbound(w, r, legEligibility, env, tok, answerTok, http.StatusUnprocessableEntity, "payer's own well-known Organization is not on file", nil)
			return
		}
		var ok bool
		insurer, ok = shnsdk.ParseOrganizationIdentifier(orgJSON)
		if !ok {
			g.refuseInbound(w, r, legEligibility, env, tok, answerTok, http.StatusUnprocessableEntity, "payer's own well-known Organization has no identifier", nil)
			return
		}
	}
	crrJSON, err := shnsdk.BuildEligibilityResponseExact(env.Metadata.CorrelationID, boundPatientRef, inforce, reason, insurer, g.cfg.Clock())
	if err != nil {
		// Build/marshal fault (gateway's own) → 500, parity with today's
		// StatusInternalServerError build-failure paths. NOT 502.
		g.refuseInbound(w, r, legEligibility, env, tok, answerTok, http.StatusInternalServerError, "build eligibility response failed", nil)
		return
	}
	answer, err := relay.Authored(relay.BuilderSDKEligibility, crrJSON, "application/fhir+json")
	if err != nil {
		g.refuseInbound(w, r, legEligibility, env, tok, answerTok, http.StatusInternalServerError, "build eligibility response failed", nil)
		return
	}
	result := LegResult{Response: answer}
	responseFHIR, err := g.admit(result.Response, answerKey("coverage-eligibility", relay.OutcomeAnswered))
	if err != nil {
		g.refuseInbound(w, r, legEligibility, env, tok, answerTok, http.StatusInternalServerError, errOwnershipFault, nil)
		return
	}
	if status, msg := g.fenceResponseSubjectWith(r.Context(), "coverage-eligibility", boundPatientRef, env.Metadata.CorrelationID, result, nil); status != 0 {
		g.refuseInbound(w, r, legEligibility, env, tok, answerTok, status, msg, nil)
		return
	}
	// Egress $validate, behavior-identical to the pre-seam inline path: validator
	// error → 500 "validator unavailable"; !Valid → 500 "egress validation failed".
	// (NOT g.validateFHIR, which returns 422 on !Valid — that would change the
	// failure-path contract.)
	// F7: per-line lane selection, same "" (version-neutral) line as the ingress site
	// above — and, as the note says, deliberately NOT g.validateFHIR: this site's
	// !Valid answer is a 500, not a 422, and that distinction is the contract.
	// Egress is unconditional by definition (R-8 is an ingress-only carve-out) — crrJSON
	// here is always SHN-produced
	// (shnsdk.BuildEligibilityResponseExact), so this was never in scope for the skip either way.
	egressValidator := g.validatorForContractLine(strings.SplitN(answerTok, "@", 2)[0], shnsdk.LineOf(answerTok))
	// Routed through the choke point so an invalid egress response still emits
	// its conformance finding. responseFHIR is THIS gateway's own built answer,
	// not the inbound request's bytes, so Whose is overridden to "own" (the
	// context the handler entry tagged names the peer's inbound leg). Both
	// outcomes here answer 500 (unchanged from before the migration) — the
	// choke point's own message already matches this site's literals
	// byte-for-byte in both cases, so it is relayed directly.
	fc := findingContextFrom(ctx)
	fc.Whose = "own"
	if gr := g.validateGoverned(ctx, fc, egressValidator, responseFHIR, "egress", shnsdk.LineOf(answerTok), "", false); gr.Status != 0 {
		msg := gr.Msg
		if gr.NoLane {
			msg = "no FHIR validator lane configured for this leg (FR-36/FR-G29)"
		}
		g.refuseInbound(w, r, legEligibility, env, tok, answerTok, http.StatusInternalServerError, msg, nil)
		return
	}

	g.respondLeg(w, r, "payer-coverage", "eligibility-response", "coverage-eligibility", env.Metadata.CorrelationID, result.Response, result.answerStatus(), tok.Subject, env.Metadata.Sender, "", answerTok)
}

// validateEligibilityRequest ingress-validates a coverage-eligibility request,
// once its subject is bound, and writes the refusal when it fails (false).
func (g *Gateway) validateEligibilityRequest(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, cerJSON []byte, answerTok string) bool {
	ctx := r.Context()
	// Only AFTER the subject binds do we ingress-validate the clinical payload via
	// the external $validate — fail-closed as before.
	//
	// F7: the lane is selected per LINE like every other validate, but this
	// site deliberately does NOT route through g.validateFHIR — its failure contract
	// (422 on !Valid, with the choke point's bounded govResult.Issues echoed)
	// differs from validateFHIR's, and unifying them would change the wire.
	// The line comes from the same
	// shnsdk.LineOf(answerTok) call every other site uses — it is not special-cased
	// here — and it evaluates to "" (the canonical lane) because coverage-eligibility
	// is version-neutral (its catalog Contract is ""), so answerTok itself is always "".
	// A nil lane keeps THIS site's 500 rather than borrowing another's.
	// cerJSON here is the requester's request: one its gateway built
	// (module1_originate.go's UC-01 sites, shnsdk.BuildEligibilityRequestExact), or one
	// a provider's own system sent through its gateway's ingress, carried as
	// sent (the request a Parameters input holds, read unwrapped). Either is a
	// peer's request, validated at this gateway's conformance level by the
	// choke point below, like any other.
	ingressValidator := g.validatorForContractLine(strings.SplitN(answerTok, "@", 2)[0], shnsdk.LineOf(answerTok))
	// Routed through the choke point so an invalid inbound request emits its
	// conformance finding. handleInbound already tagged this leg's context
	// (Whose "peer" — these are the requester's own bytes), so it is read as-is.
	// This site's own status/message contract is preserved explicitly below.
	if gr := g.validateGoverned(ctx, findingContextFrom(ctx), ingressValidator, cerJSON, "ingress", shnsdk.LineOf(answerTok), "", false); gr.Status != 0 {
		if gr.NoLane {
			g.refuseInbound(w, r, legEligibility, env, tok, answerTok, http.StatusInternalServerError, "no FHIR validator lane configured for this leg (FR-36/FR-G29)", nil)
			return false
		}
		if gr.Status == http.StatusInternalServerError {
			g.refuseInbound(w, r, legEligibility, env, tok, answerTok, http.StatusInternalServerError, "validator unavailable", nil)
			return false
		}
		// The issues echo is now BOUNDED (findingIssuesShown + "and N more"),
		// where it was unbounded before the migration — see the PR body.
		g.refuseInbound(w, r, legEligibility, env, tok, answerTok, http.StatusUnprocessableEntity, refusalIngressValidation,
			map[string]any{"error": refusalIngressValidation, "issues": gr.Issues})
		return false
	}
	return true

}

// forwardEligibilityInbound carries coverage-eligibility requests to the
// payer's own endpoint, when the payer declares one, and relays its answer as
// it would reach a direct requester. The member each request names is bound
// by this payer's own records (bindInboundSubject), from the request and the
// resources carried with it: the first request's is the leg's patient, and a
// token naming another patient is observed rather than refused
// (noteSubjectBinding); every other request's is named on the answer's
// involved list, so whatever the network records about the exchange is filed
// under each request's own patient as well, never under the token's alone.
// Each request is validated at this gateway's conformance level, and the input
// sent exactly. A non-2xx answer is relayed as the payer's error. A 2xx answer
// is the payer's content, fenced (fenceEligibilityAnswer) and $validate'd at
// the level. Nothing is answered from the payer's records in the payer's
// place, whatever the payer's system answers or fails to answer.
func (g *Gateway) forwardEligibilityInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, cerJSON []byte, subs []eligibilitySubmission, members []string, answerTok string, f eligibilityForwarder) {
	ctx := r.Context()
	const leg = "coverage-eligibility"
	for i, sub := range subs {
		pci, status, msg := g.bindInboundSubject(ctx, members[i], sub.scope)
		if status != 0 {
			g.refuseInbound(w, r, legEligibility, env, tok, answerTok, status, msg, nil)
			return
		}
		if i == 0 {
			g.noteSubjectBinding(ctx, leg, env.Metadata.CorrelationID, tok.Subject, pci)
		} else if pci != tok.Subject {
			involvedCollectorFrom(ctx).differs(pci)
		}
	}
	for _, sub := range subs {
		if !g.validateEligibilityRequest(w, r, env, tok, sub.request, answerTok) {
			return
		}
	}
	result, err := f.forwardEligibility(withResponderCorrelation(ctx, env.Metadata.CorrelationID), cerJSON)
	if err != nil {
		g.responderFailed(w, r, legEligibility, env, tok, answerTok, err)
		return
	}
	if result.Status != 0 {
		g.respondLegError(w, r, "payer-coverage", "eligibility-response", leg, env.Metadata.CorrelationID, result, tok.Subject, env.Metadata.Sender, "", answerTok)
		return
	}
	// The table lets this leg's answer be authored (the records path builds
	// it); a forwarded answer is only ever the payer's own, relayed.
	k := answerKey(leg, relay.OutcomeAnswered)
	responseFHIR, err := g.admit(result.Response, k)
	if err == nil && result.Response.Ownership() != relay.OwnershipRelayed {
		err = fmt.Errorf("%s answer from a declared eligibility endpoint is %s, want relayed", leg, result.Response.Ownership())
		g.ownershipRefused(k, err)
	}
	if err != nil {
		g.refuseInbound(w, r, legEligibility, env, tok, answerTok, http.StatusInternalServerError, errOwnershipFault, nil)
		return
	}
	// The answer is this participant's own content, relayed.
	fc := findingContextFrom(ctx)
	fc.Whose = "own"
	ctx = withFindingContext(ctx, fc)
	if status, msg := g.fenceEligibilityAnswer(ctx, responseFHIR, members); status != 0 {
		g.refuseInbound(w, r, legEligibility, env, tok, answerTok, status, msg, nil)
		return
	}
	validator := g.validatorForContractLine(strings.SplitN(answerTok, "@", 2)[0], shnsdk.LineOf(answerTok))
	if gr := g.validateGoverned(ctx, fc, validator, responseFHIR, "egress", shnsdk.LineOf(answerTok), "", false); gr.Status != 0 {
		// A validator this gateway cannot reach is this gateway's own fault
		// (500), as on the records path; an answer the validator refused is
		// the payer's (502).
		if gr.Status == http.StatusInternalServerError {
			msg := gr.Msg
			if gr.NoLane {
				msg = "no FHIR validator lane configured for this leg (FR-36/FR-G29)"
			}
			g.refuseInbound(w, r, legEligibility, env, tok, answerTok, http.StatusInternalServerError, msg, nil)
		} else {
			g.refuseInbound(w, r, legEligibility, env, tok, answerTok, http.StatusBadGateway, "payer eligibility answer refused: "+gr.Msg, nil)
		}
		return
	}
	g.respondLeg(w, r, "payer-coverage", "eligibility-response", leg, env.Metadata.CorrelationID, result.Response, result.answerStatus(), tok.Subject, env.Metadata.Sender, "", answerTok)
}

// fenceEligibilityAnswer fences a payer's own coverage-eligibility answer to
// requests about members, relayed: a CoverageEligibilityResponse, or a Bundle
// of them (eligibilityAnswerResponses). A repeated member name refuses at
// every level (the answer could be read two ways). The rest is the payer's
// content and refuses only where the conformance level says so. An answer
// with a response that cannot be read as a CoverageEligibilityResponse, or a
// Bundle holding none (RuleAnswerShape, 502), is recorded at observe and
// refused at structural and strict. One with a response naming a patient none
// of the requests names, or naming its patient by no reference
// (RulePatientAnswer, 403), is recorded at observe and structural and refused
// at strict. The payer's own Patient for a member (the record its binding
// read) is the same patient named by the payer's own id. It is read only when
// a response names another patient and the check runs (never at none). A
// failed read leaves the check unfinished: strict refuses with the system of
// record's failure, and observe and structural record it unavailable and
// relay.
func (g *Gateway) fenceEligibilityAnswer(ctx context.Context, responseFHIR []byte, members []string) (int, string) {
	if repeatsAMember(ctx, responseFHIR) {
		dup, _ := repeatedMember(responseFHIR)
		return http.StatusForbidden, "response repeats " + dup + ": it can be read two ways, so it is not carried"
	}
	responses, ok := eligibilityAnswerResponses(responseFHIR)
	if !ok {
		if g.guard(ctx, KindContent, RuleAnswerShape, responseFHIR) {
			return http.StatusBadGateway, "payer eligibility answer names no readable patient"
		}
		return 0, ""
	}
	// Every response is read for its shape first, then for its patient, so
	// the verdict does not depend on the order of the responses: one finding
	// per answer, the shape's before the patient's.
	var refs []string
	byNoRef := false
	for _, resp := range responses {
		ref, err := ParseCoverageEligibilityResponsePatient(resp.response)
		switch {
		case errors.Is(err, errNoEligibilityPatientRef):
			byNoRef = true
		case err != nil:
			if g.guard(ctx, KindContent, RuleAnswerShape, responseFHIR) {
				return http.StatusBadGateway, "payer eligibility answer names no readable patient"
			}
			return 0, ""
		default:
			refs = append(refs, resp.patientRef(ref))
		}
	}
	if byNoRef {
		if g.guard(ctx, KindContent, RulePatientAnswer, responseFHIR) {
			return http.StatusForbidden, "payer eligibility answer names no patient by reference"
		}
		return 0, ""
	}
	if !g.conformance().Runs(KindContent, RulePatientAnswer) {
		return 0, ""
	}
	named := map[string]bool{}
	for _, m := range members {
		named["Patient/"+m] = true
	}
	ownRead := false
	for _, ref := range refs {
		if named[ref] {
			continue
		}
		if !ownRead {
			ownRead = true
			for _, m := range members {
				own, found, readErr := ReadSystemOfRecord(g.cfg.SoR).PatientFHIRRefContext(ctx, m)
				if readErr != nil {
					if g.guardUnavailable(ctx, KindContent, RulePatientAnswer, responseFHIR) {
						return SoRFailureResponse(readErr)
					}
					return 0, ""
				}
				if found {
					named[own] = true
				}
			}
		}
		if !named[ref] && g.guard(ctx, KindContent, RulePatientAnswer, responseFHIR) {
			return http.StatusForbidden, "response patient does not match request patient"
		}
		if !named[ref] {
			return 0, ""
		}
	}
	return 0, ""
}
