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
		// The network's body limit: a size refusal, recorded as routing.
		exchangeOf(r.Context()).refusedFor(RefusalRouting, reasonRequestTooLarge)
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": afterLegs(r.Context(), err.Error())})
		return originationClaimInput{}, false
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": afterLegs(r.Context(), err.Error())})
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
	if isPASBuildFidelity(err) {
		refuseBuilder(w, withCarriedLegs(r.Context(), err.Error()))
		return
	}
	// This gateway's own failure, never a refusal.
	exchangeOf(r.Context()).ownFailure()
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": afterLegs(r.Context(), fallback)})
}

// isPASBuildFidelity reports whether a PAS builder refused the request on
// the fidelity of SHN's own build: a claim fact the request does not state or
// states invalidly, a submitted Claim an amendment cannot carry as submitted,
// a person the member's Coverage names whose record cannot be carried, a
// report or Provenance an amendment cannot carry as written, or a record of
// the order's requester the gateway cannot splice in as read
// (errRecordNotCarried). Every site that answers a PAS build's failure decides
// by it, so they cannot drift.
func isPASBuildFidelity(err error) bool {
	return errors.Is(err, shnsdk.ErrPASClaimFactAbsent) || errors.Is(err, shnsdk.ErrPASClaimFactInvalid) ||
		errors.Is(err, shnsdk.ErrPASPriorClaimNotCarried) || errors.Is(err, shnsdk.ErrPASCoveragePartyNotCarried) ||
		errors.Is(err, shnsdk.ErrPASRecordNotCarried) || errors.Is(err, errRecordNotCarried)
}

// pasSubmitBuildFailure answers a PAS submit's build failure to recipient: a
// payer no record names is the fidelity of SHN's own build (errNoPayerRecord),
// and anything else is pasBuildFailure's.
func (g *Gateway) pasSubmitBuildFailure(w http.ResponseWriter, r *http.Request, recipient string, err error, fallback string) {
	if errors.Is(err, errNoPayerRecord) {
		g.refuseOwnBuild(r.Context(), w, recipient, "pas-claim", msgNoPayerRecord)
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

// statedMemberFault checks the member the request names against the member
// the route resolves for this scenario, lane and persona set (scenarioMember,
// which never reads the stated member: it vouches only for the claim facts the
// request states, and selects nothing). It returns why they differ, or "":
// the claim facts a request states are its persona's statement, so a PAS leg
// built from a request naming another member, or none, would carry one
// member's facts for another. A route checks it at the build of the leg that
// consumes those facts (pasClaimSourcesAt), never before an earlier leg.
func statedMemberFault(in originationClaimInput, resolved string) string {
	named := strings.TrimSpace(in.Member)
	if named == "" {
		return `the request names no "member": the claim facts it states are a member's statement, and an origination route takes the member it originates for in its request body`
	}
	if named != resolved {
		return fmt.Sprintf("this route originates for %s; the request names %s, and SHN does not build a claim carrying the facts it states for another member (fidelity of SHN's own build)", resolved, named)
	}
	return ""
}

// statedOrderFault compares the order the request states its claim facts for
// with the order the member holds (the product coding of the order read from
// the system of record), and returns why they differ, or "". The PAS leg is the
// first that consumes the stated order: CRD and DTR carry the member's order
// of record, so a route checks it at the PAS build (pasClaimSourcesAt). A
// request stating no order, stating facts for an order the member does not
// hold, or an order of record with no product coding the PAS item can name, is
// refused there: the fidelity of SHN's own build, since a claim SHN builds
// would carry one order's facts on another order, or name no product.
func statedOrderFault(in originationClaimInput, order []byte) string {
	if in.Order == nil || strings.TrimSpace(in.Order.System) == "" || strings.TrimSpace(in.Order.Code) == "" {
		return `the request names no "order" ({"system","code"}): the claim facts it states are stated for an order`
	}
	system, code, _, err := shnsdk.ParseOrderProductCoding(order)
	if err != nil {
		return "the member's open order has no recognized product coding, which the claim's item names (fidelity of SHN's own build)"
	}
	if system != in.Order.System || code != in.Order.Code {
		return fmt.Sprintf(
			"the request states its claim facts for order %s|%s and the member's open order is %s|%s: SHN does not build a claim carrying facts stated for an order the member does not hold (fidelity of SHN's own build)",
			in.Order.System, in.Order.Code, system, code)
	}
	return ""
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

// pasOriginationRequestOrFail is the one check of a PAS origination's request
// that stays before its first leg: the body states its claim input readably
// (claimInputOrFail). A body this gateway cannot read is the caller's request
// to its own gateway malformed, and no leg built from it can succeed. What the
// claim input states is checked at the build of the leg that consumes it
// (pasClaimSourcesAt), so a request that can still succeed without that leg (a
// not-covered CRD answer) is carried. On a refusal it writes the response and
// returns false.
func pasOriginationRequestOrFail(w http.ResponseWriter, r *http.Request) bool {
	_, ok := claimInputOrFail(w, r)
	return ok
}

// pasDeferred is what a PAS origination read of the participant's records
// before its first leg and could not supply, kept for the build of the leg
// that consumes it: the PAS claim. Nothing earlier carries it, so the request
// is refused there, after the legs it could send, never before them.
type pasDeferred struct {
	// payerOrgStatus and payerOrgMsg are the refusal memberPayerOrganization
	// answered for the payer's Organization record: 422 for a record the
	// member's Coverage names and the system of record cannot supply (the
	// fidelity of SHN's own build), or a read failure's status. 0 when it
	// supplied the record or the Coverage names none.
	payerOrgStatus int
	payerOrgMsg    string
	// payerOrgErr is the read's failure when the system did not answer, which
	// is recorded as a backend failure, never a refusal (sorFailure).
	payerOrgErr error
	// supplierStatus, supplierMsg and supplierErr are the same for the
	// supplier's record a dispatched order names as its performer
	// (dispatchOrder).
	supplierStatus int
	supplierMsg    string
	supplierErr    error
}

// pasBuildRefusal is a refusal of a PAS leg's build: fidelity says it is the
// fidelity of SHN's own build (422, recorded at the build stage); otherwise it
// is answered status as it stands (a read failure of the participant's own
// system).
type pasBuildRefusal struct {
	status   int
	msg      string
	fidelity bool
	// readErr is the system of record's failure behind a refusal that is not
	// fidelity: recorded as what it is (sorFailure), never as a refusal.
	readErr error
}

func fidelityRefusal(msg string) *pasBuildRefusal {
	return &pasBuildRefusal{status: http.StatusUnprocessableEntity, msg: msg, fidelity: true}
}

// pasClaimSourcesAt reads and checks, at the build of a PAS claim, what that
// build needs of the request and of the participant's records and no earlier
// leg consumed, in this order:
//   - the request names the member the route originates for
//     (statedMemberFault);
//   - the payer's Organization record the member's Coverage names, which the
//     claim names as its insurer (deferred.payerOrgStatus);
//   - the request states its claim facts for the order the member holds, and
//     that order names a product (statedOrderFault);
//   - the supplier's record a dispatched order names (deferred.supplierStatus);
//   - the records the order's requester leads to, which every PAS request
//     about the order carries (orderRequesterRecords);
//   - the records of the people the member's Coverage names beside the member
//     (coveragePartyRecords), which every PAS request under that Coverage
//     carries.
//
// The facts the build at the routed line requires are the build's own to
// refuse (pasBuildFailure), as is a payer no record names (errNoPayerRecord).
// member is the route's resolved member, or "" on a route whose member the
// request itself names (dispatch: there the member addresses the request).
// It returns what it read, or the refusal.
func (g *Gateway) pasClaimSourcesAt(ctx context.Context, in originationClaimInput, member string, order, coverage []byte, deferred pasDeferred) (pasClaimSources, *pasBuildRefusal) {
	if member != "" {
		if why := statedMemberFault(in, member); why != "" {
			return pasClaimSources{}, fidelityRefusal(why)
		}
	}
	switch {
	case deferred.payerOrgStatus == http.StatusUnprocessableEntity:
		return pasClaimSources{}, fidelityRefusal(deferred.payerOrgMsg)
	case deferred.payerOrgStatus != 0:
		return pasClaimSources{}, &pasBuildRefusal{status: deferred.payerOrgStatus, msg: deferred.payerOrgMsg, readErr: deferred.payerOrgErr}
	}
	if why := statedOrderFault(in, order); why != "" {
		return pasClaimSources{}, fidelityRefusal(why)
	}
	switch {
	case deferred.supplierStatus == http.StatusUnprocessableEntity:
		return pasClaimSources{}, fidelityRefusal(deferred.supplierMsg)
	case deferred.supplierStatus != 0:
		return pasClaimSources{}, &pasBuildRefusal{status: deferred.supplierStatus, msg: deferred.supplierMsg, readErr: deferred.supplierErr}
	}
	requester, refusal := g.orderRequesterRecords(ctx, order)
	switch {
	case refusal != nil && refusal.fidelity:
		return pasClaimSources{}, fidelityRefusal(refusal.msg)
	case refusal != nil:
		return pasClaimSources{}, &pasBuildRefusal{status: refusal.status, msg: refusal.msg, readErr: refusal.readErr}
	}
	parties, partyRefusal := g.coveragePartyRecords(ctx, coverage)
	if partyRefusal != nil {
		return pasClaimSources{}, partyRefusal
	}
	return pasClaimSources{requester: requester, coverageParties: parties}, nil
}

// pasClaimSources is what pasClaimSourcesAt read of the participant's records
// for a PAS claim's build: the records the order's requester leads to, and the
// records of the people the member's Coverage names beside the member.
type pasClaimSources struct {
	requester       [][]byte
	coverageParties [][]byte
}

// coveragePartyRecords reads, from the participant's own system, the Patient
// record of each person the member's Coverage names as its subscriber or
// policyHolder by a literal Patient reference other than its beneficiary's (a
// dependent's parent, the PAS IG's subscriber), for the PAS builders to carry
// as a Patient entry of its own (shnsdk.ConformantClaimInputs.CoverageParties).
// Which of them rides, and how, is the builder's rule: a person whose record
// the system does not hold is not supplied, so the builder refuses the request
// as the fidelity of SHN's own build (shnsdk.ErrPASCoveragePartyNotCarried,
// isPASBuildFidelity); it never makes the record up. A read the system fails
// to answer is the refusal returned, a backend failure, and so is a record
// that is not the Patient asked for (an invalid response). A Coverage naming
// no such person, or whose beneficiary names no literal Patient (the builder
// carries no party then), needs no read.
func (g *Gateway) coveragePartyRecords(ctx context.Context, coverage []byte) ([][]byte, *pasBuildRefusal) {
	// Each slot's reference is read by its exact member names, as the SDK
	// builder reads them: a member spelled in another case names nothing here.
	var slots map[string]json.RawMessage
	if json.Unmarshal(coverage, &slots) != nil {
		return nil, nil
	}
	reference := func(slot string) string {
		var members map[string]json.RawMessage
		var ref string
		if json.Unmarshal(slots[slot], &members) != nil || json.Unmarshal(members["reference"], &ref) != nil {
			return ""
		}
		return ref
	}
	member := literalPatientKey(reference("beneficiary"))
	if member == "" {
		return nil, nil
	}
	var out [][]byte
	seen := map[string]bool{}
	for _, ref := range []string{reference("subscriber"), reference("policyHolder")} {
		key := literalPatientKey(ref)
		if key == "" || key == member || seen[key] {
			continue
		}
		seen[key] = true
		id := strings.TrimPrefix(key, "Patient/")
		if !fhirIDRE.MatchString(id) {
			// No record can carry this id: the builder refuses the slot.
			continue
		}
		record, found, err := ReadSystemOfRecord(g.cfg.SoR).ResolveByReferenceContext(ctx, key)
		if err == nil && found {
			var head struct {
				ResourceType string `json:"resourceType"`
				ID           string `json:"id"`
			}
			if json.Unmarshal(record, &head) != nil || head.ResourceType != "Patient" || head.ID != id {
				err = &SoRReadError{Kind: SoRInvalidResponse}
			}
		}
		if err != nil {
			status, msg := SoRFailureResponse(err)
			return nil, &pasBuildRefusal{status: status, msg: msg, readErr: err}
		}
		if found {
			out = append(out, record)
		}
	}
	return out, nil
}

// literalPatientKey is "Patient/<id>" for a relative or absolute reference
// to a Patient record, and "" when ref names no Patient (a contained #id, a
// urn:uuid, another type, none). It is the SDK PAS builder's own rule for a
// Coverage slot's Patient (shn-sdk's patientKey): the last "Patient/" that
// starts the reference or follows a "/", and the segment after it, whatever
// follows that segment (/_history/<version>, a trailing slash). Reading by
// another rule would leave unread a record the builder then refuses as not
// supplied.
func literalPatientKey(ref string) string {
	i := strings.LastIndex(ref, "Patient/")
	if i < 0 || (i > 0 && ref[i-1] != '/') {
		return ""
	}
	id := ref[i+len("Patient/"):]
	if j := strings.Index(id, "/"); j >= 0 {
		id = id[:j]
	}
	if id == "" {
		return ""
	}
	return "Patient/" + id
}

// writePASBuildRefusal answers a PAS leg's build refusal to recipient, saying
// which legs the request already sent were carried (withCarriedLegs): a
// fidelity refusal through refuseOwnBuild, anything else with its own status.
func (g *Gateway) writePASBuildRefusal(ctx context.Context, w http.ResponseWriter, recipient, leg string, ref *pasBuildRefusal) {
	if ref.fidelity {
		g.refuseOwnBuild(ctx, w, recipient, leg, ref.msg)
		return
	}
	if ref.readErr != nil {
		exchangeOf(ctx).sorFailure(ref.readErr)
	}
	writeJSON(w, ref.status, map[string]string{"error": withCarriedLegs(ctx, ref.msg)})
}

// pasClaimInputOrRefuse is pasClaimSourcesAt for a route's own pas-claim
// build: the claim input the request states, with res.requester read. On a
// refusal it writes the response and returns ok=false.
func (g *Gateway) pasClaimInputOrRefuse(w http.ResponseWriter, r *http.Request, res *crdDtrResult) (originationClaimInput, bool) {
	in, ok := claimInputOrFail(w, r)
	if !ok {
		return originationClaimInput{}, false
	}
	read, refusal := g.pasClaimSourcesAt(r.Context(), in, res.member, res.srJSON, res.coverage, res.pasDeferred)
	if refusal != nil {
		g.writePASBuildRefusal(r.Context(), w, res.recipient, "pas-claim", refusal)
		return originationClaimInput{}, false
	}
	res.requester, res.coverageParties = read.requester, read.coverageParties
	return in, true
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

// resumeAmendmentOrFail reads and checks, at the amendment's build (just
// before its pas-claim-update leg, after any leg the resume sends first, the
// patient-DTR exchange of UC-07 included), everything that build needs from
// the resume request: it names the pend's member, states its facts for the
// pended order, and states the facts and certificationType the build at the
// pinned line requires — the facts the resume request states, or when it
// states none the ones the pended submission's request stated. Refusals answer
// as the build would, saying which legs were carried; on one it writes the
// response and ok=false.
func (g *Gateway) resumeAmendmentOrFail(w http.ResponseWriter, r *http.Request, st pendState) (resumeAmendment, bool) {
	in, ok := claimInputOrFail(w, r)
	if !ok {
		return resumeAmendment{}, false
	}
	why := statedMemberFault(in, st.member)
	if why == "" {
		why = statedOrderFault(in, st.srJSON)
	}
	if why != "" {
		g.refuseOwnBuild(r.Context(), w, st.recipient, "pas-claim-update", why)
		return resumeAmendment{}, false
	}
	// Resume RE-RUNS arm selection with the TARGET FIXED to the pin
	// (selectResumeRoute): the amendment must answer the pend it references
	// (the pended-pin rule), but the declared/lane sets may have grown since
	// origination, so native reach can now beat a chain even at resume.
	route, err := g.selectResumeRoute(st.pasToken, st.recipient, "pas-claim-update", relay.BuilderSDKPASUpdate)
	if err != nil {
		if !g.relayOriginationError(w, err) {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": afterLegs(r.Context(), err.Error())})
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
