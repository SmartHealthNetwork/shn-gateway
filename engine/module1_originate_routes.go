//go:build !shn_coreonly

package engine

import (
	"net/http"
	"strings"
)

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

// originationRoute is one row of module 1's route table: its pattern,
// whether it originates (builds and sends a request of this gateway's own),
// and its handler, a method of the gateway it is mounted on.
type originationRoute struct {
	pattern    string
	originates bool
	handler    func(*Gateway, http.ResponseWriter, *http.Request)
}

// originationRouteTable is every route on-behalf building declares, in mount
// order: the provider's /scenario/* origination API. It is static, so the
// exchange routes it names are known at package init (exchangeRoutes);
// originationRoutes mounts it for a gateway.
var originationRouteTable = []originationRoute{
	{pattern: "POST /scenario/uc01", originates: true, handler: (*Gateway).handleScenario},
	{pattern: "POST /scenario/uc02", originates: true, handler: (*Gateway).handleUC02},
	// FR-G41 live routing lanes off the /holders feed: uc02-payerb resolves a SECOND payer
	// identity (MBR-PD-UC02-PB / 00078); uc02-unknownpayer fails closed 422 (MBR-UNKNOWN-PAYER /
	// 00099 → no registered payer). Both share the handleUC02 no-PA CRD body. Whether 00078
	// resolves is a deployment fact — see handleUC02PayerB.
	{pattern: "POST /scenario/uc02-payerb", originates: true, handler: (*Gateway).handleUC02PayerB},
	{pattern: "POST /scenario/uc02-unknownpayer", originates: true, handler: (*Gateway).handleUC02UnknownPayer},
	{pattern: "POST /scenario/uc03", originates: true, handler: (*Gateway).handleUC03},
	{pattern: "POST /scenario/uc04", originates: true, handler: (*Gateway).handleUC04},
	{pattern: "POST /scenario/uc05", originates: true, handler: (*Gateway).handleUC05},
	{pattern: "POST /scenario/uc06", originates: true, handler: (*Gateway).handleUC06},
	{pattern: "POST /scenario/uc07", originates: true, handler: (*Gateway).handleUC07},
	{pattern: "POST /scenario/uc07hcpcs", originates: true, handler: (*Gateway).handleUC07HCPCS},
	{pattern: "POST /scenario/uc08", originates: true, handler: (*Gateway).handleUC08},
	{pattern: "POST /scenario/homeoxygen", originates: true, handler: (*Gateway).handleHomeOxygen},
	{pattern: "POST /scenario/dispatch", originates: true, handler: (*Gateway).handleDispatch},
	{pattern: "POST /scenario/uc06/start", originates: true, handler: (*Gateway).handleUC06Start},
	{pattern: "POST /scenario/uc06/complete", originates: true, handler: (*Gateway).handleUC06Complete},
	{pattern: "POST /scenario/uc06/cancel", originates: false, handler: (*Gateway).handleScenarioCancel},
	{pattern: "POST /scenario/uc07/start", originates: true, handler: (*Gateway).handleUC07Start},
	{pattern: "POST /scenario/uc07/complete", originates: true, handler: (*Gateway).handleUC07Complete},
	{pattern: "POST /scenario/uc07/cancel", originates: false, handler: (*Gateway).handleScenarioCancel},
	{pattern: "GET /scenario/uc07/pending", originates: false, handler: (*Gateway).handleUC07Pending},
	// Continue a prior-authorization decision this gateway pended earlier
	// (module1_originate_wait.go). It belongs on this surface and nowhere else: the
	// continuation is server-held state for the gateway's OWN originator
	// flows, and a participant's own system inquires through its own
	// `POST /Claim/$inquire` ingress instead, with no SHN state involved.
	// Like the rest of /scenario/*, it is never publicly routed.
	{pattern: "POST /scenario/pa/inquire", originates: true, handler: (*Gateway).handlePAInquire},
	// Admin reset of the in-memory pended-scenario store. The reset caller
	// (the platform's cloud smoke, after the holder simulator's POST /admin/reset) hits this route so a
	// pended UC-06/07 does not survive as an orphaned questionnaire (which would
	// 502 on a stale patient submit). Internal — provider-gw is not public.
	{pattern: "POST /scenario/reset", originates: false, handler: (*Gateway).handleScenarioReset},
}

// originationExchangeRoute is the closed route name an exchange record names
// for an originating route: its path, without the leading slash, its
// segments joined by "-" ("POST /scenario/uc07/complete" is
// "scenario-uc07-complete").
func originationExchangeRoute(pattern string) string {
	_, path, _ := strings.Cut(pattern, " ")
	return strings.ReplaceAll(strings.TrimPrefix(path, "/"), "/", "-")
}

// refusalReasons: module 1's (module1_refusalreason.go).
func (moduleOne) refusalReasons() []refusalReason { return moduleOneRefusalReasons() }

// exchangeRoutes are the routes module 1 originates exchanges from: every
// originating route of its table, by its exchange route name.
func (moduleOne) exchangeRoutes() []string {
	var out []string
	for _, rt := range originationRouteTable {
		if rt.originates {
			out = append(out, originationExchangeRoute(rt.pattern))
		}
	}
	return out
}

// originationRoutes are on-behalf building's routes for role: its route
// table (originationRouteTable), mounted for the gateway. Module 1 runs only
// where the participant declares it, by its origination profile
// (Config.OriginationProfile): with none, no route here is mounted, and a
// /scenario path is the mux's own 404. Every route that ORIGINATES (builds and
// sends a request of this gateway's own) reads the Claim facts the request
// states (readsClaimFacts) and records its exchange (exchangeRoute: routes()
// mounts the exchange record outermost, direction ingress, so each call's
// record, a refusal's included, exists before any handler can refuse); the
// routes that only read or drop this gateway's own pended state (cancel,
// pending, reset) do neither. Every route here is the operator surface: each
// caller is authenticated (requireScenarioCaller).
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
	for _, rt := range originationRouteTable {
		h := func(w http.ResponseWriter, r *http.Request) { rt.handler(g, w, r) }
		if !rt.originates {
			out = append(out, mountedRoute{pattern: rt.pattern, handler: h, operator: true, credential: carriesCredential})
			continue
		}
		out = append(out, mountedRoute{pattern: rt.pattern, handler: g.readsClaimFacts(h), operator: true, exchangeRoute: originationExchangeRoute(rt.pattern), credential: carriesCredential})
	}
	return out, nil
}
