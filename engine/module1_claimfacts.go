//go:build !shn_coreonly

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
	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
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
//	"reportType": {Coding}          // X12 755, optional, routes that send an amendment
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
	ReportType        json.RawMessage       `json:"reportType"`
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

// readsClaimFacts wraps an originating route: the Claim facts the request
// states are read before its handler decodes the body for its own keys.
func (g *Gateway) readsClaimFacts(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h(w, withOriginationClaimInput(r))
	}
}

// withOriginationClaimInput reads the claim input once, at the origination
// route's entry (readsClaimFacts), before any handler decodes the body for its
// own keys, and keeps it on the request context.
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
// 422, with the SDK's text naming the element, recorded at the build stage.
// So is an amendment whose recorded submission cannot be carried as the
// requester submitted it, in the SDK's own words: this gateway never rebuilds
// a submitted Claim. Any other build failure stays an internal error under
// fallback: this gateway's own fault, not a refusal.
func pasBuildFailure(w http.ResponseWriter, r *http.Request, err error, fallback string) {
	if errors.Is(err, shnsdk.ErrPASClaimFactAbsent) || errors.Is(err, shnsdk.ErrPASClaimFactInvalid) ||
		errors.Is(err, shnsdk.ErrPASPriorClaimNotCarried) {
		refuseBuilder(w, err.Error())
		return
	}
	// This gateway's own failure, never a refusal.
	exchangeOf(r.Context()).ownFailure()
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": fallback})
}

// pasSubmitBuildFailure answers a PAS submit's build failure to recipient: a
// payer no record names is the fidelity of SHN's own build (errNoPayerRecord),
// and anything else is pasBuildFailure's.
func (g *Gateway) pasSubmitBuildFailure(w http.ResponseWriter, r *http.Request, recipient string, err error, fallback string) {
	if errors.Is(err, errNoPayerRecord) {
		g.refuseOwnBuild(w, recipient, "pas-claim", msgNoPayerRecord)
		return
	}
	pasBuildFailure(w, r, err, fallback)
}

// pasSubmissionRecord is what an amendment needs of the submission it
// continues, read off the bytes that submission's leg sent
// (shnsdk.PASPriorClaimOf): the submitted Claim as sent, which the amendment
// carries as its prior-Claim entry and derives its infoChanged marker from.
type pasSubmissionRecord struct {
	prior shnsdk.PASPriorClaim
	// pend is the payer's pended answer to the submission, exactly as
	// received: the amendment links its attached answer to the items this
	// pend asks it for (ConformantClaimUpdateInputs.Pend), and to no other.
	pend []byte
}

// pendFor is the pend an amendment sent on route links its attached answer by.
// A line whose supportingInfo slice requires a report type
// (PASDef.SupportingInfoDocumentInformationRequired, PAS 2.2) links only on the
// requester's statement, which the builder holds it to. A build at a lower line
// bridged up to such a line cannot state one and would carry a link the line
// refuses, so it gets no pend and links nothing.
func (s pasSubmissionRecord) pendFor(route legRoute) []byte {
	target := shnsdk.LineOf(route.Token)
	def, ok := shnsdk.PASLineDef(target)
	if !ok || (def.SupportingInfoDocumentInformationRequired && route.BuildLine != target) {
		return nil
	}
	return s.pend
}

// reportTypeFor is the report type an amendment sent on route is built with:
// the requester's statement, on a build at the line the amendment reaches. A
// bridged build takes none. The extension that carries it exists only at PAS
// 2.2, so a lower build could not state it and a 2.2 build bridged down would
// send it to a line that has no such extension.
func reportTypeFor(route legRoute, stated json.RawMessage) json.RawMessage {
	if route.BuildLine != shnsdk.LineOf(route.Token) {
		return nil
	}
	return stated
}

// pasExtDocumentInformation is the PAS 2.2 extension whose reportTypeCode
// carries a requester's stated report type on Claim.supportingInfo.
const pasExtDocumentInformation = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-documentInformation"

// recordReportTypeNotCarried records a report type the requester stated that
// the built amendment does not carry: the amendment goes on without it, and the
// statement does not vanish unrecorded. updateBundle is read as built: on any
// line, whatever kept the link from being made.
func recordReportTypeNotCarried(ctx context.Context, stated json.RawMessage, updateBundle []byte) {
	if t := bytes.TrimSpace(stated); len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return
	}
	if !amendmentCarriesReportType(updateBundle) {
		exchangeOf(ctx).editNotMade(EditNotMadeReportTypeNotCarried)
	}
}

// amendmentCarriesReportType reports whether the amending Claim, the Bundle's
// first entry (the prior Claim rides after it), has a supportingInfo entry
// stating a report type.
func amendmentCarriesReportType(bundle []byte) bool {
	var b struct {
		Entry []struct {
			Resource struct {
				SupportingInfo []struct {
					Extension []struct {
						URL string `json:"url"`
					} `json:"extension"`
				} `json:"supportingInfo"`
			} `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(bundle, &b); err != nil || len(b.Entry) == 0 {
		return false
	}
	for _, si := range b.Entry[0].Resource.SupportingInfo {
		for _, e := range si.Extension {
			if e.URL == pasExtDocumentInformation {
				return true
			}
		}
	}
	return false
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
// claim SHN builds would carry one order's facts on another order. It is
// recorded on the call's exchange record as a fidelity refusal at
// recording.RefusalStageBuild; the request's other faults (no order named, an order
// with no product coding) are not fidelity refusals.
func statedOrderCheck(ctx context.Context, in originationClaimInput, order []byte) (int, string) {
	if in.Order == nil || strings.TrimSpace(in.Order.System) == "" || strings.TrimSpace(in.Order.Code) == "" {
		return http.StatusBadRequest, `the request names no "order" ({"system","code"}): the claim facts it states are stated for an order`
	}
	system, code, _, err := shnsdk.ParseOrderProductCoding(order)
	if err != nil {
		return http.StatusBadGateway, "open order has no recognized product coding"
	}
	if system != in.Order.System || code != in.Order.Code {
		exchangeOf(ctx).refusedFidelity(vocab.RefusalStageBuild)
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
	if status, msg := statedOrderCheck(r.Context(), in, order); status != 0 {
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

type amendsItsOwnClaimKey struct{}

// routeAmendsItsOwnClaim marks an origination request whose route, once the
// payer pends the claim it submits, sends the amendment of that claim in the
// same request with no new statement between them: the UC-04 demo lane's
// in-flow amendment and the single-call UC-06 and UC-07. Such an amendment
// carries the submitted claim whole (PAS 2.1 and later), so the route checks,
// just before the claim is sent, what of it the amendment certainly cannot
// carry (amendedClaimCarriesItsFacts). UC-05 is not marked: its consented
// branch can still end before any amendment (consentDenied), so a claim it
// sends can succeed without one. /complete is not marked either: its pend was
// made by an earlier request.
func routeAmendsItsOwnClaim(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), amendsItsOwnClaimKey{}, true))
}

func amendsItsOwnClaim(r *http.Request) bool {
	v, _ := r.Context().Value(amendsItsOwnClaimKey{}).(bool)
	return v
}

// amendedClaimCarriesItsFacts is the check a route that amends its own claim
// runs just before that claim's pas-claim leg, at the line it is built at: an
// amendment carries the submitted claim whole, so a reference the request
// stated in a claim fact must resolve in the amendment's bundle. It refuses
// the references CheckPASSubmittedFactsCarriedAtLine can rule out before any
// build: one to a resource type the amendment's bundle has no entry of, an
// absolute one under another base, and one that is not a type and an id. Past
// this point the payer would hold a pend no amendment of this request could
// answer. Such a reference is refused as the amendment's build would refuse it
// (pasBuildFailure: 422, the fidelity of SHN's own build at the build stage),
// with no claim sent. A reference to a type the bundle does hold, or a local
// one, is still decided at the amendment's build, after the claim. It runs
// here, not before the first leg, because a request that can still end
// without a claim (a CRD not-covered answer) is carried. A request no route
// marks passes. On a refusal it writes the response and returns false.
func amendedClaimCarriesItsFacts(w http.ResponseWriter, r *http.Request, line string, facts shnsdk.PASClaimFacts) bool {
	if !amendsItsOwnClaim(r) {
		return true
	}
	if err := shnsdk.CheckPASSubmittedFactsCarriedAtLine(line, facts); err != nil {
		pasBuildFailure(w, r, err, "build bundle failed: "+err.Error())
		return false
	}
	return true
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
		pasBuildFailure(w, r, err, "build bundle failed: "+err.Error())
		return false
	}
	if sendsAmendment(r) {
		if err := shnsdk.CheckPASUpdateFactsAtLine(route.BuildLine, in.facts(), in.CertificationType); err != nil {
			pasBuildFailure(w, r, err, "build update bundle failed")
			return false
		}
		if err := shnsdk.CheckPASReportType(in.ReportType); err != nil {
			pasBuildFailure(w, r, err, "build update bundle failed")
			return false
		}
	}
	return true
}

// pasOriginationBeforeAnyLeg is the one check every route that builds a PAS
// claim passes before its first leg: the request states its claim facts for
// order, the order the member holds (claimInputForOrder), and states every fact
// the PAS build at the line routed to recipient requires, the amendment's too
// on a route that sends one (claimFactsBeforeAnyLeg). It also reads the
// records the order's requester leads to (orderRequesterRecords), which every
// PAS request about the order carries: read once, here, so a clinician the
// participant's system cannot supply is refused before anything is sent. Both
// prefixes a PAS origination runs, order-select (runCRDThenDTROrder) and
// order-dispatch (runCRDDispatch), call it once the recipient is resolved and
// before their first leg; TestPASOriginationsCheckFactsBeforeAnyLeg pins that
// statically. On a refusal it writes the response and returns false.
func (g *Gateway) pasOriginationBeforeAnyLeg(w http.ResponseWriter, r *http.Request, recipient string, order []byte) ([][]byte, bool) {
	if _, ok := claimInputForOrder(w, r, order); !ok {
		return nil, false
	}
	if !g.claimFactsBeforeAnyLeg(w, r, recipient) {
		return nil, false
	}
	requester, refusal := g.orderRequesterRecords(r.Context(), order)
	switch {
	case refusal != nil && refusal.fidelity:
		// The PAS leg this origination would end in cannot be built
		// faithfully, so nothing is sent: the fidelity of SHN's own build.
		g.refuseOwnBuild(w, recipient, "pas-claim", refusal.msg)
		return nil, false
	case refusal != nil:
		writeJSON(w, refusal.status, map[string]string{"error": refusal.msg})
		return nil, false
	}
	// An origination that attests with a clinician NPI and holds every source
	// of it now (attestsBeforeAnyLeg) resolves the attesting clinician here,
	// from the records just read: no source naming one refuses before any leg.
	if attestsBeforeAnyLeg(r.Context()) {
		if _, status, msg := g.attestingNPI(r.Context(), order, requester); status == http.StatusUnprocessableEntity {
			g.refuseOwnBuild(w, recipient, "pas-claim", msg)
			return nil, false
		} else if status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return nil, false
		}
	}
	return requester, true
}

// resumeAmendment is what a resume (/complete, or the single call's resume
// half) amends with: the route its amendment is built at (the pend's pin), and
// the facts, certificationType and report type the resume request states.
type resumeAmendment struct {
	route             legRoute
	facts             shnsdk.PASClaimFacts
	certificationType json.RawMessage
	reportType        json.RawMessage
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
	a := resumeAmendment{route: route, facts: st.claimFacts, certificationType: in.CertificationType, reportType: in.ReportType}
	if in.ClaimFacts != nil {
		a.facts = *in.ClaimFacts
	}
	if err := shnsdk.CheckPASUpdateFactsAtLine(route.BuildLine, a.facts, a.certificationType); err != nil {
		pasBuildFailure(w, r, err, "build update bundle failed")
		return resumeAmendment{}, false
	}
	if err := shnsdk.CheckPASReportType(a.reportType); err != nil {
		pasBuildFailure(w, r, err, "build update bundle failed")
		return resumeAmendment{}, false
	}
	return a, true
}
