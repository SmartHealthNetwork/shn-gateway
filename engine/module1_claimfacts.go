package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The request facts a prior-authorization Claim states — its claim type,
// priority, item category, place of service and request type — are the
// requester's to state. No published mapping derives any of them from the
// order or an Encounter (the PAS IG defers the X12 278 mapping to X12 and its
// profiles carry no ConceptMap), so this gateway, building the Claim on the
// requester's behalf, takes them exactly as the request to its origination
// route states them and carries them. It reads them from nowhere else and
// supplies none of them.
//
// A fact the Claim requires at the line (PAS Claim profile snapshot) and the
// request does not state is refused by the SDK builder before anything is
// built or sent (pasBuildFailure answers 422 with the element named). That
// refusal is the fidelity of SHN's own build: SHN building a participant's
// wire message cannot source the field, so it does not build the message. It
// is not a judgement of anyone's payload — the network refuses only on its own
// primitives, and the fidelity of SHN's own edits is one of them.

// originationClaimInput is what an origination route's request states about
// the Claim it asks this gateway to build: the request facts (claimFacts) and,
// on a route that sends an amendment, the amendment's certificationType.
//
// Wire contract (JSON request body, beside the route's existing keys):
//
//	"claimFacts": {"claimType":{Coding},"priority":{Coding},"serviceCategory":{Coding},
//	               "placeOfService":{Coding},"requestType":{Coding}}
//	"certificationType": {Coding}   // X12 1322, routes that send an amendment
//	"member": "<member id>"         // every origination route
//	"order": {"system":..,"code":..} // routes that build a PAS claim
//
// The request names its member and the order its claim facts are stated for:
// the facts are one requester's statement about one order, so a request whose
// member is not the one the route originates for, or whose order is not the
// one that member holds, is refused before anything is built (see
// requestNamesMember and claimInputForOrder).
type originationClaimInput struct {
	ClaimFacts        *shnsdk.PASClaimFacts `json:"claimFacts"`
	CertificationType json.RawMessage       `json:"certificationType"`
	Member            string                `json:"member"`
	Order             *statedOrder          `json:"order"`
}

// statedOrder is the order a request states its claim facts for: the product
// coding's system and code, as the requester's discovery statement names it.
type statedOrder struct {
	System string `json:"system"`
	Code   string `json:"code"`
}

// facts is the stated claim facts; none stated is the zero value, which the
// builder refuses wherever a fact is required.
func (in originationClaimInput) facts() shnsdk.PASClaimFacts {
	if in.ClaimFacts == nil {
		return shnsdk.PASClaimFacts{}
	}
	return *in.ClaimFacts
}

// readOriginationClaimInput reads the claim input from the request body and
// puts the body back, so a handler that decodes the body for its own keys
// still sees all of it. An empty body states nothing; a body that is not a
// JSON object is an error.
func readOriginationClaimInput(r *http.Request) (originationClaimInput, error) {
	var in originationClaimInput
	if r.Body == nil {
		return in, nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, shnsdk.MaxRequestBytes+1))
	_ = r.Body.Close()
	if err != nil {
		return in, fmt.Errorf("read the request body: %w", err)
	}
	// The body goes back as read, oversize included, so a handler decoding
	// it for its own keys answers its size as it always did (413).
	r.Body = io.NopCloser(bytes.NewReader(raw))
	if len(raw) > shnsdk.MaxRequestBytes {
		return in, errOriginationBodyTooLarge
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return in, nil
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return in, fmt.Errorf("the request body is not a JSON object carrying claimFacts: %w", err)
	}
	return in, nil
}

// errOriginationBodyTooLarge is a request body larger than the gateway
// reads: answered 413, never a claim-input 400.
var errOriginationBodyTooLarge = errors.New("request body too large")

type claimInputKey struct{}

type claimInputResult struct {
	in  originationClaimInput
	err error
}

// withOriginationClaimInput reads the claim input once, at the origination
// route's entry (requireOriginationProfile), before any handler decodes the
// body for its own keys, and keeps it on the request context.
func withOriginationClaimInput(r *http.Request) *http.Request {
	in, err := readOriginationClaimInput(r)
	return r.WithContext(context.WithValue(r.Context(), claimInputKey{}, claimInputResult{in, err}))
}

// ensureOriginationClaimInput is withOriginationClaimInput for a handler that
// decodes the body for its own keys: it reads the claim input first unless the
// route entry already did, so the handler's own decode never consumes it.
func ensureOriginationClaimInput(r *http.Request) *http.Request {
	if _, ok := r.Context().Value(claimInputKey{}).(claimInputResult); ok {
		return r
	}
	return withOriginationClaimInput(r)
}

// claimInputOrFail is the claim input the origination request stated, for a
// handler: an unreadable body is answered 400 and ok=false. A request that did
// not pass the route entry (a handler driven directly) is read here.
func claimInputOrFail(w http.ResponseWriter, r *http.Request) (originationClaimInput, bool) {
	in, err := claimInputOf(r)
	if errors.Is(err, errOriginationBodyTooLarge) {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": err.Error()})
		return originationClaimInput{}, false
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return originationClaimInput{}, false
	}
	return in, true
}

// claimInputOf is the claim input the origination request stated.
func claimInputOf(r *http.Request) (originationClaimInput, error) {
	if r == nil {
		return originationClaimInput{}, nil
	}
	res, ok := r.Context().Value(claimInputKey{}).(claimInputResult)
	if !ok {
		res.in, res.err = readOriginationClaimInput(r)
	}
	return res.in, res.err
}

// pasBuildFailure answers a PAS request build that failed. A build refused
// because a request fact the request did not state is required at the line,
// or is not a coding its element can carry, is the fidelity refusal above:
// 422, with the SDK's text naming the element. Any other build failure stays
// an internal error under fallback.
func pasBuildFailure(w http.ResponseWriter, err error, fallback string) {
	if errors.Is(err, shnsdk.ErrPASClaimFactAbsent) || errors.Is(err, shnsdk.ErrPASClaimFactInvalid) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": fallback})
}

// pasSubmissionRecord is what an amendment needs of the submission it
// continues, read off the bytes that submission actually built
// (shnsdk.PASPriorClaimOf): the submitted Claim's type and priority as sent
// (the prior-Claim entry restates them), its created, and its items as sent
// (the amendment's infoChanged marker is derived from them).
type pasSubmissionRecord struct {
	prior shnsdk.PASPriorClaim
	// pend is the payer's pended answer to the submission, exactly as
	// received: the amendment links its attached answer to the items this
	// pend asks it for (ConformantClaimUpdateInputs.Pend), and to no other.
	pend []byte
}

// pendFor is the pend an amendment that reaches the payer at targetLine links
// its attached answer by: none where that line's supportingInfo slice requires
// a report type no source states (PASDef.SupportingInfoDocumentInformationRequired,
// PAS 2.2). A build at a lower line bridged up to such a line would otherwise
// carry a link the line refuses; it links nothing instead.
func (s pasSubmissionRecord) pendFor(targetLine string) []byte {
	if def, ok := shnsdk.PASLineDef(targetLine); !ok || def.SupportingInfoDocumentInformationRequired {
		return nil
	}
	return s.pend
}

// recordPASSubmission reads the submission record off a submitted PAS Bundle.
// A value the bytes do not carry stays empty, and the update builder refuses an
// amendment built from it — never fills it.
func recordPASSubmission(bundle []byte) (pasSubmissionRecord, error) {
	prior, err := shnsdk.PASPriorClaimOf(bundle)
	if err != nil {
		return pasSubmissionRecord{}, err
	}
	return pasSubmissionRecord{prior: prior}, nil
}

// requestNamesMember checks the member the request names against the member
// the route resolves for this scenario, lane and persona set. A request that
// names none is refused 400; one that names another member is refused 422: the
// claim facts it states are that member's statement, not this one's.
func requestNamesMember(w http.ResponseWriter, r *http.Request, resolved string) bool {
	in, ok := claimInputOrFail(w, r)
	if !ok {
		return false
	}
	named := strings.TrimSpace(in.Member)
	if named == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `the request names no "member": an origination route takes the member it originates for in its request body`})
		return false
	}
	if named != resolved {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": fmt.Sprintf("this route originates for %s; the request names %s", resolved, named)})
		return false
	}
	return true
}

// statedOrderCheck compares the order the request states its claim facts for
// with the order the member holds (the product coding of the order read from
// the system of record). Returns (status, msg); status 0 == the same order.
//
// A request stating facts for an order the member does not hold is refused
// 422 before anything is built. That is the fidelity of SHN's own build: a
// claim SHN builds would carry one order's facts on another order.
func statedOrderCheck(in originationClaimInput, order []byte) (int, string) {
	if in.Order == nil || strings.TrimSpace(in.Order.System) == "" || strings.TrimSpace(in.Order.Code) == "" {
		return http.StatusBadRequest, `the request names no "order" ({"system","code"}): the claim facts it states are stated for an order`
	}
	system, code, _, err := shnsdk.ParseOrderProductCoding(order)
	if err != nil {
		return http.StatusBadGateway, "open order has no recognized product coding"
	}
	if system != in.Order.System || code != in.Order.Code {
		return http.StatusUnprocessableEntity, fmt.Sprintf(
			"the request states its claim facts for order %s|%s and the member's open order is %s|%s: SHN does not build a claim carrying facts stated for an order the member does not hold (fidelity of SHN's own build)",
			in.Order.System, in.Order.Code, system, code)
	}
	return 0, ""
}

// claimInputForOrder is the claim input the request stated, checked to be
// stated for order (statedOrderCheck); on a refusal it writes the response
// and returns ok=false.
func claimInputForOrder(w http.ResponseWriter, r *http.Request, order []byte) (originationClaimInput, bool) {
	in, ok := claimInputOrFail(w, r)
	if !ok {
		return originationClaimInput{}, false
	}
	if status, msg := statedOrderCheck(in, order); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return originationClaimInput{}, false
	}
	return in, true
}

type sendsAmendmentKey struct{}

// routeSendsAmendment marks an origination request whose route sends, in the
// same request, an amendment after its submission (the UC-04 in-flow
// amendment, UC-05, and the single-call UC-06/UC-07): the amendment's facts and
// its certificationType are then checked before the first leg too
// (claimFactsBeforeAnyLeg).
func routeSendsAmendment(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), sendsAmendmentKey{}, true))
}

func sendsAmendment(r *http.Request) bool {
	v, _ := r.Context().Value(sendsAmendmentKey{}).(bool)
	return v
}

// claimFactsBeforeAnyLeg refuses, before the first leg of an origination is
// sent, a request whose stated facts the PAS build at the routed line would
// refuse: the submission's facts and, on a route that also sends an
// amendment, the amendment's certificationType. It asks the SDK builder's own
// rule (CheckPASClaimFactsAtLine / CheckPASUpdateFactsAtLine, the resolver the
// builders run), so the check and the build cannot drift, and answers exactly
// as the build would (pasBuildFailure: 422 naming the element). A request it
// passes is built at its own site as before; a pas-claim route this gateway
// cannot select is refused there, unchanged.
//
// The line is the one the submission's own selection picks (selectLegRoute, a
// pure read of the declared lines): the submission pins it, and an amendment
// in the same request is built at the line the resume's selection picks for
// that pin (selectResumeRoute). Within one request both selections read the
// same declared, lane and narrowing state, and in every state a gateway boots
// in they pick the same build line or the resume refuses the route outright
// (it never carries away from a pin it was just given); that is pinned over
// every such state by TestSingleCallAmendmentLine_IsTheSubmissionsBuildLine.
//
// A route reaches it through pasOriginationBeforeAnyLeg, never directly.
func (g *Gateway) claimFactsBeforeAnyLeg(w http.ResponseWriter, r *http.Request, recipient string) bool {
	in, ok := claimInputOrFail(w, r)
	if !ok {
		return false
	}
	route, err := g.routeFor("pas-claim", recipient, relay.BuilderSDKPASSubmit)
	if err != nil || route.BuildLine == "" {
		return true
	}
	if err := shnsdk.CheckPASClaimFactsAtLine(route.BuildLine, in.facts()); err != nil {
		pasBuildFailure(w, err, "build bundle failed: "+err.Error())
		return false
	}
	if sendsAmendment(r) {
		if err := shnsdk.CheckPASUpdateFactsAtLine(route.BuildLine, in.facts(), in.CertificationType); err != nil {
			pasBuildFailure(w, err, "build update bundle failed")
			return false
		}
	}
	return true
}

// pasOriginationBeforeAnyLeg is the one check every route that builds a PAS
// claim passes before its first leg: the request states its claim facts for
// order, the order the member holds (claimInputForOrder), and states every fact
// the PAS build at the line routed to recipient requires, the amendment's too
// on a route that sends one (claimFactsBeforeAnyLeg). Both prefixes a PAS
// origination runs, order-select (runCRDThenDTROrder) and order-dispatch
// (runCRDDispatch), call it once the recipient is resolved and before their
// first leg; TestPASOriginationsCheckFactsBeforeAnyLeg pins that statically.
// On a refusal it writes the response and returns false.
func (g *Gateway) pasOriginationBeforeAnyLeg(w http.ResponseWriter, r *http.Request, recipient string, order []byte) bool {
	if _, ok := claimInputForOrder(w, r, order); !ok {
		return false
	}
	return g.claimFactsBeforeAnyLeg(w, r, recipient)
}

// resumeAmendment is what a resume (/complete, or the single call's resume
// half) amends with: the route its amendment is built at (the pend's pin), and
// the facts and certificationType the resume request states.
type resumeAmendment struct {
	route             legRoute
	facts             shnsdk.PASClaimFacts
	certificationType json.RawMessage
}

// resumeAmendmentOrFail reads and checks, before the resume sends any leg (the
// patient-DTR exchange of UC-07 included), everything the amendment's build
// needs from the resume request: it names the pend's member, states its facts
// for the pended order, and states the facts and certificationType the build
// at the pinned line requires — the facts the resume request states, or when
// it states none the ones the pended submission's request stated. Refusals
// answer as the build would; on one it writes the response and ok=false.
func (g *Gateway) resumeAmendmentOrFail(w http.ResponseWriter, r *http.Request, st pendState) (resumeAmendment, bool) {
	if !requestNamesMember(w, r, st.member) {
		return resumeAmendment{}, false
	}
	in, ok := claimInputForOrder(w, r, st.srJSON)
	if !ok {
		return resumeAmendment{}, false
	}
	// Resume RE-RUNS arm selection with the TARGET FIXED to the pin
	// (selectResumeRoute): the amendment must answer the pend it references
	// (the pended-pin rule), but the declared/lane sets may have grown since
	// origination, so native reach can now beat a chain even at resume.
	route, err := g.selectResumeRoute(st.pasToken, st.recipient, "pas-claim-update", relay.BuilderSDKPASUpdate)
	if err != nil {
		if !g.relayOriginationError(w, err) {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		}
		return resumeAmendment{}, false
	}
	a := resumeAmendment{route: route, facts: st.claimFacts, certificationType: in.CertificationType}
	if in.ClaimFacts != nil {
		a.facts = *in.ClaimFacts
	}
	if err := shnsdk.CheckPASUpdateFactsAtLine(route.BuildLine, a.facts, a.certificationType); err != nil {
		pasBuildFailure(w, err, "build update bundle failed")
		return resumeAmendment{}, false
	}
	return a, true
}
