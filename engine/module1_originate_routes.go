package engine

import "net/http"

// originationRoutes are on-behalf building's routes for role: the provider's
// /scenario/* origination API. Every route that ORIGINATES (builds and sends
// a request of this gateway's own) is marked so, and refuses while
// Config.OriginationProfile is unset (requireOriginationProfile). The routes
// that only read or drop this gateway's own pended state — cancel, pending,
// reset — serve whatever the profile. Every route here is the operator
// surface: each caller is authenticated (requireScenarioCaller).
func (g *Gateway) originationRoutes(role string) []mountedRoute {
	if role != "provider" {
		return nil
	}
	var out []mountedRoute
	originates := func(pattern string, h http.HandlerFunc) {
		out = append(out, mountedRoute{pattern: pattern, handler: h, originates: true, operator: true})
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
	return out
}
