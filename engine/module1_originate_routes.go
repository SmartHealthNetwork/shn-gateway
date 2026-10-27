//go:build !shn_coreonly

package engine

import "net/http"

// moduleOne is on-behalf building as a module of the gateway (gatewayModule):
// New builds its state, and routes() mounts its routes, through the module
// registry, so the core names none of its code.
type moduleOne struct{}

// moduleOneID is the id module 1's state is kept under on a gateway.
const moduleOneID = "module1"

// moduleOneState is module 1's own state on a gateway, built once by New:
// its in-flight pend state, and the routes it declared for the gateway's role
// (originationRoutes).
type moduleOneState struct {
	pending     moduleOnePending
	origination []mountedRoute
}

func (moduleOne) id() string { return moduleOneID }

// newGatewayState checks the origination profile and declares module 1's
// routes for g's role, once; a profile that names no lane refuses New.
func (moduleOne) newGatewayState(g *Gateway) (gatewayState, error) {
	st := &moduleOneState{}
	var err error
	if st.origination, err = g.originationRoutes(g.cfg.Role); err != nil {
		return nil, err
	}
	return st, nil
}

// routes are the routes module 1 declared when New built its state; none on a
// gateway assembled without New. They were declared for the gateway's own
// role, the one routes() mounts for.
func (moduleOne) routes(g *Gateway, _ string) []mountedRoute {
	if st := g.moduleOneState(); st != nil {
		return st.origination
	}
	return nil
}

// moduleOneState is module 1's state on g, or nil on a gateway assembled
// without New.
func (g *Gateway) moduleOneState() *moduleOneState {
	st, _ := g.moduleStates[moduleOneID].(*moduleOneState)
	return st
}

// originationRoutes are on-behalf building's routes for role: the provider's
// /scenario/* origination API. Module 1 runs only where the participant
// declares it, by its origination profile (Config.OriginationProfile): with
// none, no route here is mounted, and a /scenario path is the mux's own 404.
// Every route that ORIGINATES (builds and sends a request of this gateway's
// own) is marked so, and reads the Claim facts the request states
// (readsClaimFacts); the routes that only read or drop this gateway's own
// pended state (cancel, pending, reset) do not. Every route here is the
// operator surface: each caller is authenticated (requireScenarioCaller).
// A profile that names no lane is refused first, on every role
// (checkOriginationProfile): module 1's newGatewayState calls this once, as New
// builds its state, New refuses with its error, and the routes are kept for
// routes() to mount.
func (g *Gateway) originationRoutes(role string) ([]mountedRoute, error) {
	if err := checkOriginationProfile(g.cfg.OriginationProfile); err != nil {
		return nil, err
	}
	if role != "provider" || g.cfg.OriginationProfile == "" {
		return nil, nil
	}
	var out []mountedRoute
	originates := func(pattern string, h http.HandlerFunc) {
		out = append(out, mountedRoute{pattern: pattern, handler: g.readsClaimFacts(h), operator: true})
	}
	local := func(pattern string, h http.HandlerFunc) {
		out = append(out, mountedRoute{pattern: pattern, handler: h, operator: true})
	}
	originates("POST /scenario/uc01", g.handleScenario)
	originates("POST /scenario/uc02", g.handleUC02)
	// FR-G41 live routing lanes off the /holders feed: uc02-payerb resolves a SECOND payer
	// identity (MBR-PD-UC02-PB / 00078); uc02-unknownpayer fails closed 422 (MBR-UNKNOWN-PAYER /
	// 00099 → no registered payer). Both share the handleUC02 no-PA CRD body. Whether 00078
	// resolves is a deployment fact — see handleUC02PayerB.
	originates("POST /scenario/uc02-payerb", g.handleUC02PayerB)
	originates("POST /scenario/uc02-unknownpayer", g.handleUC02UnknownPayer)
	originates("POST /scenario/uc03", g.handleUC03)
	originates("POST /scenario/uc04", g.handleUC04)
	originates("POST /scenario/uc05", g.handleUC05)
	originates("POST /scenario/uc06", g.handleUC06)
	originates("POST /scenario/uc07", g.handleUC07)
	originates("POST /scenario/uc07hcpcs", g.handleUC07HCPCS)
	originates("POST /scenario/uc08", g.handleUC08)
	originates("POST /scenario/homeoxygen", g.handleHomeOxygen)
	originates("POST /scenario/dispatch", g.handleDispatch)
	originates("POST /scenario/uc06/start", g.handleUC06Start)
	originates("POST /scenario/uc06/complete", g.handleUC06Complete)
	local("POST /scenario/uc06/cancel", g.handleScenarioCancel)
	originates("POST /scenario/uc07/start", g.handleUC07Start)
	originates("POST /scenario/uc07/complete", g.handleUC07Complete)
	local("POST /scenario/uc07/cancel", g.handleScenarioCancel)
	local("GET /scenario/uc07/pending", g.handleUC07Pending)
	// Continue a prior-authorization decision this gateway pended earlier
	// (module1_originate_wait.go). It belongs on this surface and nowhere else: the
	// continuation is server-held state for the gateway's OWN originator
	// flows, and a participant's own system inquires through its own
	// `POST /Claim/$inquire` ingress instead, with no SHN state involved.
	// Like the rest of /scenario/*, it is never publicly routed.
	originates("POST /scenario/pa/inquire", g.handlePAInquire)
	// Admin reset of the in-memory pended-scenario store. The reset caller
	// (tools/cloudsmoke, after holdersim's POST /admin/reset) hits this route so a
	// pended UC-06/07 does not survive as an orphaned questionnaire (which would
	// 502 on a stale patient submit). Internal — provider-gw is not public.
	local("POST /scenario/reset", g.handleScenarioReset)
	return out, nil
}
