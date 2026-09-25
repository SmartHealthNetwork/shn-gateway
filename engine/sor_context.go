package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
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
	return &SoRReadError{Kind: SoRInvalidResponse}
}

// writeSoRFailure answers a failed system-of-record read on a request this
// participant sent (an ingress or origination route). A leg received from the
// network answers through refuseSoRFailure instead.
func writeSoRFailure(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	status, msg := SoRFailureResponse(err)
	writeJSON(w, status, map[string]string{"error": msg})
	return true
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
// the routing identity is the same whichever is used. Gateway-originated CRD
// requests are to carry the whole Coverage search result instead (a planned
// change that keeps this routing rule).
func (g *Gateway) memberCoverage(ctx context.Context, member string) (coverage []byte, found bool, status int, msg string) {
	covs, err := ReadSystemOfRecord(g.cfg.SoR).OpenCoverageContext(ctx, member)
	if err != nil {
		status, msg := SoRFailureResponse(err)
		return nil, false, status, msg
	}
	switch len(covs) {
	case 0:
		return nil, false, 0, ""
	case 1:
		return covs[0], true, 0, ""
	}
	var first shnsdk.PayerIdentifier
	for i, c := range covs {
		resolve, readErr := sorReferenceCallback(ctx, g.cfg.SoR)
		pid, parseErr := shnsdk.ParseCoveragePayer(c, resolve)
		if *readErr != nil {
			status, msg := SoRFailureResponse(*readErr)
			return nil, false, status, msg
		}
		if parseErr != nil || (i > 0 && pid != first) {
			return nil, false, http.StatusUnprocessableEntity, "ambiguous coverage for routing: the member's Coverage records do not name one payer"
		}
		first = pid
	}
	return covs[0], true, 0, ""
}

func (g *Gateway) recipientForSoR(ctx context.Context, coverage []byte) (string, shnsdk.PayerIdentifier, int, string) {
	if g.cfg.PayerRouter == nil {
		return "", shnsdk.PayerIdentifier{}, http.StatusUnprocessableEntity, "no payer router configured"
	}
	resolve, readErr := sorReferenceCallback(ctx, g.cfg.SoR)
	parsed, ok := shnsdk.ParsePayerIdentifier(coverage, resolve)
	if *readErr != nil {
		status, msg := SoRFailureResponse(*readErr)
		return "", shnsdk.PayerIdentifier{}, status, msg
	}
	if !ok {
		return "", shnsdk.PayerIdentifier{}, http.StatusUnprocessableEntity, "no payer identifier on member coverage"
	}
	holder, ok := g.cfg.PayerRouter.Resolve(parsed)
	if !ok {
		return "", shnsdk.PayerIdentifier{}, http.StatusUnprocessableEntity, fmt.Sprintf("no registered payer for identifier %s|%s", parsed.System, parsed.Value)
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
// named reason. Nothing is minted either way.
func (g *Gateway) memberPayerOrganization(ctx context.Context, coverage []byte) ([]byte, int, string) {
	payorRef := coveragePayorRef(coverage)
	if payorRef == "" {
		return nil, 0, ""
	}
	// A coverage may carry its payer organization INSIDE itself. That is still the
	// participant's own record — read it where the record put it rather than asking
	// the system for a reference that names nothing outside this resource.
	if strings.HasPrefix(payorRef, "#") {
		org, ok := containedResource(coverage, strings.TrimPrefix(payorRef, "#"))
		if !ok {
			return nil, http.StatusUnprocessableEntity, "the member's coverage names contained payer organization " + payorRef + " and does not contain it"
		}
		return org, 0, ""
	}
	org, found, err := ReadSystemOfRecord(g.cfg.SoR).ResolveByReferenceContext(ctx, payorRef)
	if err != nil {
		status, msg := SoRFailureResponse(err)
		return nil, status, msg
	}
	if !found {
		return nil, http.StatusUnprocessableEntity, "the system of record has no payer organization " + payorRef
	}
	return org, 0, ""
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
