package engine

import (
	"context"
	"errors"
	"net/http"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

// legclient.go is the core's in-process LegClient (refactor step 8):
// module 1's one way to its legs. A module 1 site asks routeFor for the route
// the core will send its leg on, builds its request at that route's build
// line, and hands it to sendLeg, which takes it the rest of the way: walked,
// checked and sealed by egressBuild when the site built it, then sent through
// OriginateLeg. Module 1 calls neither of those itself (moduleBuildSites). The
// core's own ingress replaces this for every leg the ingress serves, as the
// registered on-behalf client (step 8's end state).

// legSend is one module 1 request for its leg.
type legSend struct {
	Workstream, Leg, Recipient, PCI, Corr string
	// Custodian is the source the Authorization Framework gates consent for on
	// this leg; empty when the recipient is the custodian.
	Custodian string
	// Route is the route the request was built for; nil for a leg the site
	// sends with no route (a version-neutral leg).
	Route *legRoute
	// Build is the request as the site built it at Route's build line, for
	// egressBuild; nil when Sealed is the request, sealed and checked by the
	// site itself.
	Build     *moduleBuild
	Sealed    relay.Payload
	Operation string
}

// legSent is a request taken to its leg. Route is the route it was sent on,
// and every read after the send uses it, not the route planned: the line the
// answer is checked at, the decision that follows it, a pend's pin. Bytes and
// Reports are what egressBuild made of a built request (nil for a sealed one);
// Answer is the recipient's answer.
type legSent struct {
	Route   legRoute
	Bytes   []byte
	Reports []LossReport
	Answer  []byte
}

// legFailure is a request that got no answer. Status and Msg are what the
// site's caller is told unless it reads Err: the walk's failure (Status from
// adaptFailureStatus), or the leg's own (FromLeg, 502), which a site answers
// through relayOriginationError when it is a framed relay error. Err is nil for
// a check's refusal.
type legFailure struct {
	Status  int
	Msg     string
	Err     error
	FromLeg bool
}

// routeFor is the route the core will send leg to recipient on: the
// read-only query a module 1 site asks before it builds, so that it builds at
// the line the core routes (design §3.2, Line choice). It observes nothing; a
// site that refuses on its error records the refusal itself (selectLegLine).
func (g *Gateway) routeFor(leg, recipient string, builder relay.BuilderID) (legRoute, error) {
	return g.selectLegRoute(recipient, leg, builder)
}

// sendLeg takes s to its leg. A built request goes through egressBuild first,
// and a walk or check that refuses it stops it there, with nothing sent. A
// failure of the leg itself still returns what was sent (Route, Bytes and
// Reports), which a site may keep.
func (g *Gateway) sendLeg(ctx context.Context, r *http.Request, s legSend) (legSent, *legFailure) {
	var route legRoute
	if s.Route != nil {
		route = *s.Route
	}
	sent := legSent{Route: route}
	payload := s.Sealed
	if s.Build != nil {
		built, status, msg, err := g.egressBuild(ctx, route, ExchangeIdentity{CorrelationID: s.Corr, LegType: s.Leg, Counterpart: s.Recipient}, *s.Build)
		if err != nil {
			return legSent{}, &legFailure{Status: adaptFailureStatus(err), Msg: err.Error(), Err: err}
		}
		if status != 0 {
			return legSent{}, &legFailure{Status: status, Msg: msg}
		}
		payload, sent.Bytes, sent.Reports = built.Payload, built.Bytes, built.Reports
	}
	content := Content{WorkstreamType: s.Workstream, Payload: payload, Operation: s.Operation}
	if s.Route != nil {
		content.ProfileID, content.Route = route.Token, routeInfoFor(route)
	}
	answer, err := g.OriginateLeg(ctx, r, s.Recipient, s.Leg, s.PCI, s.Corr, s.Custodian, content)
	if err != nil {
		return sent, &legFailure{Status: http.StatusBadGateway, Msg: err.Error(), Err: err, FromLeg: true}
	}
	sent.Answer = answer
	return sent, nil
}

// adaptFailureStatus is the status an egressAdapt failure answers with on the
// product paths. The typed *SemanticChangeError is a designed refusal: the
// request cannot be carried to the recipient's line without changing what it
// asserts, so nothing is sent and the caller gets the same legible 422 a
// RouteRefusalError gets (no shared line). Any other failure (a parse error
// inside a step, a disconnected chain, a Provenance that does not round-trip)
// is this gateway's own fault and stays a 502.
func adaptFailureStatus(err error) int {
	var sce *SemanticChangeError
	if errors.As(err, &sce) {
		return http.StatusUnprocessableEntity
	}
	return http.StatusBadGateway
}
