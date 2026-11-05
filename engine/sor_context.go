package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

func safeSoRError(err error) *SoRReadError {
	status, _ := SoRFailureResponse(err)
	if status == http.StatusServiceUnavailable {
		return &SoRReadError{Kind: SoRUnavailable}
	}
	var typed *SoRReadError
	if errors.As(err, &typed) && typed != nil && typed.Kind == SoRAuthenticationFailed {
		return &SoRReadError{Kind: SoRAuthenticationFailed}
	}
	if errors.Is(err, relay.ErrDuplicateKey) {
		return SoRRepeatedMember()
	}
	return &SoRReadError{Kind: SoRInvalidResponse}
}

// refuseSoRFailure answers a failed system-of-record read on an inbound leg as
// this participant's answer: framed to the requester with the safe
// status and message, never the read error's own text.
func (g *Gateway) refuseSoRFailure(w http.ResponseWriter, r *http.Request, leg inboundLeg, env shnsdk.Envelope, tok shnsdk.Token, answerTok string, err error) bool {
	if err == nil {
		return false
	}
	status, msg := SoRFailureResponse(err)
	g.refuseInbound(w, r, leg, env, tok, answerTok, status, msg, nil)
	exchangeOf(r.Context()).sorFailure(err)
	return true
}

// memberRecordsUnreadable reports a system-of-record failure that only a
// member the system holds records for can produce: its records for the member
// answered out of shape (SoRFailureResponse's invalid response, for example
// two Patients for one member). An outage or a failed authentication meets
// every member's read alike, so it says nothing about any one patient.
func memberRecordsUnreadable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var readErr *SoRReadError
	if errors.As(err, &readErr) && readErr != nil {
		return readErr.Kind == SoRInvalidResponse
	}
	return true
}

// refuseUnreadableMember answers a bind whose member's records are out of
// shape (memberRecordsUnreadable) with the leg's own patient-match refusal,
// the answer a member this participant does not hold gets, so the answer does
// not say it holds records for that patient; the exchange record keeps its
// system's failure.
func (g *Gateway) refuseUnreadableMember(w http.ResponseWriter, r *http.Request, leg inboundLeg, env shnsdk.Envelope, tok shnsdk.Token, answerTok, msg string, err error) bool {
	if !memberRecordsUnreadable(err) {
		return false
	}
	g.refuseInbound(w, r, leg, env, tok, answerTok, http.StatusForbidden, msg, nil)
	exchangeOf(r.Context()).sorFailure(err)
	return true
}

// Reference callbacks are scoped to one SDK invocation. Once a read fails,
// further callback calls do no work; callers check the captured error first.
func sorReferenceCallback(ctx context.Context, sor SystemOfRecord) (func(string) ([]byte, bool), *error) {
	var readErr error
	return func(ref string) ([]byte, bool) {
		if readErr != nil || sor == nil {
			return nil, false
		}
		b, found, err := ReadSystemOfRecord(sor).ResolveByReferenceContext(ctx, ref)
		if err != nil {
			readErr = safeSoRError(err)
			return nil, false
		}
		return b, found
	}, &readErr
}

// memberCoverage reads the member's Coverage records from the system of record
// and returns the one to route on. found is false when there is none. A read
// failure is the system-of-record failure status. Several records are
// accepted only when every one of them names the same payer; otherwise the
// member's payer is ambiguous and the answer is 422, never a guess.
//
// With several same-payer records the first one the system returned is used;
// the routing identity is the same whichever is used. Every Coverage counts
// here, cancelled ones too. It is the read of eligibility, on both sides: the
// UC-01 eligibility request this gateway originates and the payer side's
// answer. The CRD, DTR and PAS legs this gateway originates, and the PAS
// inquiry, read memberRoutingCoverage instead, which applies the routing
// choice first.
func (g *Gateway) memberCoverage(ctx context.Context, member string) (coverage []byte, found bool, status int, msg string) {
	return g.chosenMemberCoverage(ctx, member, nil)
}

// memberRoutingCoverage is the Coverage a CRD request this gateway originates
// for member is routed by: memberCoverage's rule over the Coverages
// routingCoverageChoice picks (the active ones when any is active, else all
// of them). It is the same choice as the coverage the request carries
// (obtainRoutingCoverage), so every leg of the exchange (CRD, DTR, PAS) names
// the same coverage.
//
// Coverage records that do not name one payer are also handed on as
// errAmbiguousCoverage, and a read the system failed to answer as its error.
func (g *Gateway) memberRoutingCoverage(ctx context.Context, member string) (coverage []byte, found bool, status int, msg string, err error) {
	coverage, found, ambiguous, status, msg, readErr := g.readMemberCoverage(ctx, member, routingCoverageChoice)
	err = readErr
	if ambiguous {
		err = errAmbiguousCoverage
	}
	return coverage, found, status, msg, err
}

// chosenMemberCoverage is the one read of a member's Coverage records: of
// those choose picks (every one when choose is nil), the one to route on.
func (g *Gateway) chosenMemberCoverage(ctx context.Context, member string, choose func([][]byte) []int) (coverage []byte, found bool, status int, msg string) {
	coverage, found, _, status, msg, _ = g.readMemberCoverage(ctx, member, choose)
	return coverage, found, status, msg
}

// originCoverage reads the Coverage a call this gateway originates for member
// routes on (memberCoverage's read, or memberRoutingCoverage's when routing),
// and answers the call itself when there is none to route on, returning
// false: a system-of-record failure with its own status, recorded as one
// (sorFailure) and saying which legs were carried, and, as this
// gateway's routing refusal (no one payer can be addressed), Coverage
// records that do not name one payer (422 ambiguous coverage) or no Coverage
// at all (422 no coverage on file for member). The same read elsewhere (the
// payer's own answers, a waiting inquiry that keeps its pend) records
// nothing of it.
func (g *Gateway) originCoverage(ctx context.Context, w http.ResponseWriter, member string, routing bool) ([]byte, bool) {
	var choose func([][]byte) []int
	if routing {
		choose = routingCoverageChoice
	}
	coverage, found, ambiguous, status, msg, readErr := g.readMemberCoverage(ctx, member, choose)
	switch {
	case ambiguous:
		exchangeOf(ctx).refusedFor(RefusalRouting, reasonMemberCoverageTwoPayers)
		writeJSON(w, status, map[string]string{"error": msg})
		return nil, false
	case status != 0:
		// The system failed to answer: a backend failure, never a refusal
		// (an answer repeating a member name is recorded as message
		// integrity, as on every leg).
		exchangeOf(ctx).sorFailure(readErr)
		writeJSON(w, status, map[string]string{"error": withCarriedLegs(ctx, msg)})
		return nil, false
	case !found:
		exchangeOf(ctx).refusedFor(RefusalRouting, reasonNoCoverageOnFile)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": msgNoCoverageOnFile})
		return nil, false
	}
	return coverage, true
}

// msgAmbiguousCoverage is the refusal of Coverage records that do not name
// one payer.
const msgAmbiguousCoverage = "ambiguous coverage for routing: the member's Coverage records do not name one payer"

// msgNoCoverageOnFile is the refusal of a call to originate for a member whose
// system of record holds no Coverage to route on.
const msgNoCoverageOnFile = "no coverage on file for member"

// errAmbiguousCoverage marks that refusal for a caller that hands the read's
// refusal on as an error (memberRoutingCoverage); relayOriginationError
// answers it as the routing refusal it is.
var errAmbiguousCoverage = errors.New(msgAmbiguousCoverage)

// readMemberCoverage is chosenMemberCoverage reporting, by structure, a 422
// for Coverage records that do not name one payer (ambiguous).
func (g *Gateway) readMemberCoverage(ctx context.Context, member string, choose func([][]byte) []int) (coverage []byte, found, ambiguous bool, status int, msg string, readErr error) {
	covs, err := ReadSystemOfRecord(g.cfg.SoR).OpenCoverageContext(ctx, member)
	if err != nil {
		status, msg := SoRFailureResponse(err)
		return nil, false, false, status, msg, err
	}
	if choose != nil {
		all := covs
		covs = nil
		for _, i := range choose(all) {
			covs = append(covs, all[i])
		}
	}
	switch len(covs) {
	case 0:
		return nil, false, false, 0, "", nil
	case 1:
		return covs[0], true, false, 0, "", nil
	}
	var first shnsdk.PayerIdentifier
	for i, c := range covs {
		resolve, readErr := sorReferenceCallback(ctx, g.cfg.SoR)
		pid, parseErr := shnsdk.ParseCoveragePayer(c, resolve)
		if *readErr != nil {
			status, msg := SoRFailureResponse(*readErr)
			return nil, false, false, status, msg, *readErr
		}
		if parseErr != nil || (i > 0 && pid != first) {
			return nil, false, true, http.StatusUnprocessableEntity, msgAmbiguousCoverage, nil
		}
		first = pid
	}
	return covs[0], true, false, 0, "", nil
}

// recipientForSoR resolves the payer holder from the member's own Coverage,
// the participant's system resolving its references. A coverage that names no
// payer this gateway can route to (no payer router, no payer identifier, or
// one no participant registers) is a routing refusal, recorded so on the
// call's exchange record; a failed read of the system is that read's answer,
// recorded as a backend failure.
func (g *Gateway) recipientForSoR(ctx context.Context, coverage []byte) (string, shnsdk.PayerIdentifier, int, string) {
	if g.cfg.PayerRouter == nil {
		exchangeOf(ctx).refusedFor(RefusalRouting, reasonNoPayerRouter)
		return "", shnsdk.PayerIdentifier{}, http.StatusUnprocessableEntity, msgNoPayerRouter
	}
	resolve, readErr := sorReferenceCallback(ctx, g.cfg.SoR)
	parsed, ok := shnsdk.ParsePayerIdentifier(coverage, resolve)
	if *readErr != nil {
		// The system failed to answer for the payer the Coverage names: a
		// backend failure, recorded as one, never a refusal.
		exchangeOf(ctx).sorFailure(*readErr)
		status, msg := SoRFailureResponse(*readErr)
		return "", shnsdk.PayerIdentifier{}, status, msg
	}
	if !ok {
		exchangeOf(ctx).refusedFor(RefusalRouting, reasonNoPayerIdentifier)
		return "", shnsdk.PayerIdentifier{}, http.StatusUnprocessableEntity, noPayerIdentifier
	}
	holder, ok := g.cfg.PayerRouter.Resolve(parsed)
	if !ok {
		exchangeOf(ctx).refusedFor(RefusalRouting, reasonUnregisteredPayer)
		return "", shnsdk.PayerIdentifier{}, http.StatusUnprocessableEntity, unregisteredPayer(parsed)
	}
	return holder, parsed, 0, ""
}

// memberPayerOrganization reads the payer's own Organization record — the one
// the member's Coverage names as payor — from the participant's system.
//
// It is the SAME read the inquiry leg already makes (inquiryRecords), moved to
// the origination side because both legs have to name one payer organization.
// The reference payer resolves an Organization to one it holds only through an
// NPI, which a payer organization does not carry, so whatever a submission names
// is what a later inquiry has to name: a submission naming an organization the
// SDK minted is one this participant's own inquiry can never match. Measured
// live, and isolated to exactly that one id.
//
// A coverage that names an organization this system cannot supply is a refusal
// that names the record. A coverage that names NO organization at all — one
// whose payor is an inline identifier, which is a shape this network routes on
// perfectly well — is not refused here: eligibility and coverage-check legs do
// not carry a payer organization, and only the prior-authorization leg does, so
// that absence surfaces where it matters, at the request builder, with its own
// named reason (errNoPayerRecord). Nothing is minted either way.
//
// A read the system of record failed to answer returns its error too, beside
// the status it is answered with: a backend failure, never a refusal, which
// a caller that answers it later records as such (sorFailure).
func (g *Gateway) memberPayerOrganization(ctx context.Context, coverage []byte) ([]byte, int, string, error) {
	payorRef := coveragePayorRef(coverage)
	if payorRef == "" {
		return nil, 0, "", nil
	}
	// A coverage may carry its payer organization INSIDE itself. That is still the
	// participant's own record — read it where the record put it rather than asking
	// the system for a reference that names nothing outside this resource.
	if strings.HasPrefix(payorRef, "#") {
		org, ok := containedResource(coverage, strings.TrimPrefix(payorRef, "#"))
		if !ok {
			return nil, http.StatusUnprocessableEntity, "the member's coverage names contained payer organization " + payorRef + " and does not contain it", nil
		}
		return org, 0, "", nil
	}
	org, found, err := ReadSystemOfRecord(g.cfg.SoR).ResolveByReferenceContext(ctx, payorRef)
	if err != nil {
		status, msg := SoRFailureResponse(err)
		return nil, status, msg, err
	}
	if !found {
		return nil, http.StatusUnprocessableEntity, "the system of record has no payer organization " + payorRef, nil
	}
	return org, 0, "", nil
}

// containedResource lifts a resource a record contains, by its local id, and
// gives it that id as its own. A contained resource is the participant's record
// too; it just travels inside another one.
func containedResource(record []byte, id string) ([]byte, bool) {
	var probe struct {
		Contained []json.RawMessage `json:"contained"`
	}
	if json.Unmarshal(record, &probe) != nil {
		return nil, false
	}
	for _, c := range probe.Contained {
		var head struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(c, &head) == nil && head.ID == id {
			return c, true
		}
	}
	return nil, false
}
