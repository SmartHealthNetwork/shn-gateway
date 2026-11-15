package engine

import (
	"encoding/json"
	"net/http"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// carriedRequestAccepted is the recipient's gateway's check of a carried
// request it receives, at its participant's own conformance level (owner
// decision D47(2)): a workstream's handler calls it after the leg's subject
// binding and before its participant's system sees the request (the PA
// workstream's PAS $submit, update and $inquire). It reports whether the
// request goes on; when it does not, it has written the refusal.
//
//   - none and observe run nothing here: no validator call, and the request is
//     carried exactly as sent (observe keeps the evidence certification every
//     leg already gets, and records no finding of its own).
//   - structural and strict judge the request through the governed choke point
//     at its candidate lines (the routed line first, then the lines it claims,
//     then 2.2, 2.1 and 2.0), valid when any line accepts it, so a request is
//     never refused for the line it was built at. A request that declares a
//     profile is judged against what it declares; one that declares none
//     against the profile its workstream names for it at its line
//     (requestLineProfile), read as the workstream reads that profile
//     (validateAt). A code system the lane cannot load is a gap, never a defect.
//   - A refusal is conformance at the recipient's own level, never a network
//     primitive: 422 "ingress validation failed" with the bounded issues (an
//     OperationOutcome on a leg whose answer envelope is FHIR), or 500 when the
//     check could not run at strict (the choke point's message: validator
//     unavailable, or profile could not be checked for a declared profile no
//     lane holds). Nothing reaches the participant's system.
//
// The bytes are never changed: the check reads the request, and the request
// the participant's system receives is the one the requester sent.
func (g *Gateway) carriedRequestAccepted(w http.ResponseWriter, r *http.Request, leg inboundLeg, env shnsdk.Envelope, tok shnsdk.Token, answerTok string, requestJSON []byte) bool {
	mod := g.conformance()
	if level := mod.Level(); level != EnforcementStructural && level != EnforcementStrict {
		return true
	}
	ctx := r.Context()
	contract, line := strings.SplitN(answerTok, "@", 2)[0], shnsdk.LineOf(answerTok)
	profile := ""
	if !requestDeclaresProfile(requestJSON) {
		profile = requestLineProfile(contract, requestJSON, line, leg.tx)
	}
	lanes := g.candidateLanes(contract, line, requestJSON)
	gr := g.validateGovernedLinesBy(ctx, mod, findingContextFrom(ctx), lanes, requestJSON, "ingress", line, profile, false, true)
	switch {
	case gr.Status == 0:
		return true
	case gr.NoLane:
		g.refuseInbound(w, r, leg, env, tok, answerTok, http.StatusInternalServerError, "no FHIR validator lane configured for this leg", nil)
	case gr.Status == http.StatusInternalServerError:
		// The choke point's own text: validator unavailable, or, for a
		// declared profile no lane holds, profile could not be checked.
		g.refuseInbound(w, r, leg, env, tok, answerTok, http.StatusInternalServerError, gr.Msg, nil)
	default:
		g.refuseInbound(w, r, leg, env, tok, answerTok, http.StatusUnprocessableEntity, refusalIngressValidation,
			map[string]any{"error": refusalIngressValidation, "issues": gr.Issues})
	}
	return false
}

// requestDeclaresProfile reports whether payload's own meta.profile names any
// profile.
func requestDeclaresProfile(payload []byte) bool {
	var head struct {
		Meta struct {
			Profile []string `json:"profile"`
		} `json:"meta"`
	}
	return json.Unmarshal(payload, &head) == nil && len(head.Meta.Profile) > 0
}
