//go:build !shn_coreonly

package engine

import "slices"

// workstream_pa_routescope.go — the scopes the PA workstream's provider
// ingress routes take (routeScope, ingressscope.go): each a system-level
// scope on the FHIR resource type the route's URL names, permitting what the
// operation does, decided by the route alone, never by reading the request.
// ProviderUmbrellaScope permits every one of them but the plain base's
// Subscriptions, and a CDS service takes it alone. The Da Vinci IGs name a
// scope for none of these but DTR's Questionnaire operations
// (dtrScopeAliases), so each is the reading SMART App Launch 2 gives the
// route's resource and interaction.
var (
	// scopeCDSService: CDS Hooks discovery and each hook (CRD). A CDS
	// service names no FHIR resource, so no resource or wildcard scope
	// permits it.
	scopeCDSService = routeScope{reason: reasonScopeCDSService}
	// scopeDTR: the questionnaire package and each $next-question, which read
	// a payer's Questionnaire.
	scopeDTR = routeScope{resources: []string{"Questionnaire"}, need: taskRead, reason: reasonScopeDTR}
	// scopePASSubmit: Claim/$submit at either base, which creates a prior
	// authorization request (an update or a cancel is submitted so too).
	scopePASSubmit = routeScope{resources: []string{"Claim"}, need: taskCreate, reason: reasonScopePASSubmit}
	// scopePASInquire: Claim/$inquire at either base, which reads the answer
	// to one.
	scopePASInquire = routeScope{resources: []string{"Claim"}, need: taskRead, reason: reasonScopePASInquire}
	// scopeEligibility: CoverageEligibilityRequest/$submit.
	scopeEligibility = routeScope{resources: []string{"CoverageEligibilityRequest"}, need: taskCreate, reason: reasonScopeEligibility}
	// scopeAttachment: $submit-attachment at each of its addresses. An
	// attachment is a DocumentReference or a QuestionnaireResponse, so a
	// scope that permits creating either permits the route.
	scopeAttachment = routeScope{resources: []string{"DocumentReference", "QuestionnaireResponse"}, need: taskCreate, reason: reasonScopeAttachment}
)

// plainSubscriptionScope is the scope a Subscription interaction at the plain
// base or at a payer's PAS base takes, as a payer's Subscription routes take
// theirs: a Subscription scope that permits need, which the umbrella is not.
func plainSubscriptionScope(need taskPermission, reason refusalReason) routeScope {
	return routeScope{resources: []string{"Subscription"}, need: need, noUmbrella: true, reason: reason}
}

// paScopedRoutes are the PA workstream's provider ingress routes that take a
// scope, each pattern as paRoutes mounts it, with the scope its handler
// checks (TestPARouteScope_EveryMountedRouteTakesAScope holds the two to one
// another), but the listings of the payers' bases (paListingScopes).
var paScopedRoutes = []scopedPattern{
	{"GET /cds-services", scopeCDSService},
	{"POST /cds-services/{id}", scopeCDSService},
	{"POST /Questionnaire/$questionnaire-package", scopeDTR},
	{"POST /dtr/payer/{payer}/{rest...}", scopeDTR},
	{"POST /dtr/fetch-server/{payer}/{line}/{tag}/Questionnaire/$next-question", scopeDTR},
	{"POST /Questionnaire/$next-question", scopeDTR},
	{"POST /Claim/$submit", scopePASSubmit},
	{"POST /Claim/$inquire", scopePASInquire},
	{"POST /pas/payer/{payer}/Claim/$submit", scopePASSubmit},
	{"POST /pas/payer/{payer}/Claim/$inquire", scopePASInquire},
	{"POST /Subscription", plainSubscriptionScope(taskCreate, reasonScopeSubscriptionCreate)},
	{"GET /Subscription", plainSubscriptionScope(taskSearch, reasonScopeSubscriptionSearch)},
	{"GET /Subscription/{rest...}", plainSubscriptionScope(taskRead, reasonScopeSubscriptionRead)},
	{"PUT /Subscription/{id}", plainSubscriptionScope(taskUpdate, reasonScopeSubscriptionUpdate)},
	{"PATCH /Subscription/{id}", plainSubscriptionScope(taskUpdate, reasonScopeSubscriptionUpdate)},
	{"DELETE /Subscription/{id}", plainSubscriptionScope(taskDelete, reasonScopeSubscriptionDelete)},
	{"POST /CoverageEligibilityRequest/$submit", scopeEligibility},
	{"POST /$submit-attachment", scopeAttachment},
	{"POST /cdex/payer/{payer}", scopeAttachment},
	{"POST /cdex/payer/{payer}/$submit-attachment", scopeAttachment},
}

// paPASSubscriptionScopedRoutes are the provider's own Subscription to the PAS
// topic at a payer's base (handlePASSubscriptionBase), mounted only where its
// participant sets PROVIDER_PAS_SUBSCRIPTION: each interaction takes what the
// plain base's does, and a PATCH, refused 405 past its scope, an update's.
var paPASSubscriptionScopedRoutes = []scopedPattern{
	{"POST /pas/payer/{payer}/Subscription", plainSubscriptionScope(taskCreate, reasonScopeSubscriptionCreate)},
	{"GET /pas/payer/{payer}/Subscription", plainSubscriptionScope(taskSearch, reasonScopeSubscriptionSearch)},
	{"GET /pas/payer/{payer}/Subscription/{rest...}", plainSubscriptionScope(taskRead, reasonScopeSubscriptionRead)},
	{"PUT /pas/payer/{payer}/Subscription/{rest...}", plainSubscriptionScope(taskUpdate, reasonScopeSubscriptionUpdate)},
	{"DELETE /pas/payer/{payer}/Subscription/{rest...}", plainSubscriptionScope(taskDelete, reasonScopeSubscriptionDelete)},
	{"PATCH /pas/payer/{payer}/Subscription/{rest...}", plainSubscriptionScope(taskUpdate, reasonScopeSubscriptionUpdate)},
}

// The listings of the payers' bases: each takes any scope some route under
// the bases it lists takes (listingScope), so a route added under them widens
// it.
var (
	// scopePASPayers: GET /pas/payers, the payers' PAS bases, where the
	// provider's own Subscription to the PAS topic is mounted under them;
	// scopePASPayersUnsubscribed where it is not (pasPayersScope).
	scopePASPayers             = listingScope(pasPayerBasePattern, slices.Concat(paScopedRoutes, paPASSubscriptionScopedRoutes), reasonScopePASPayers)
	scopePASPayersUnsubscribed = listingScope(pasPayerBasePattern, paScopedRoutes, reasonScopePASPayers)
	// scopeCDexPayers: GET /cdex/payers, the payers' attachment addresses.
	scopeCDexPayers = listingScope(cdexPayerBasePattern, paScopedRoutes, reasonScopeCDexPayers)
)

// The bases the listings list, as their routes' patterns name them.
const (
	pasPayerBasePattern  = "/pas/payer/{payer}"
	cdexPayerBasePattern = "/cdex/payer/{payer}"
)

// paListingScopes are the listings' routes, as paRoutes mounts them.
var paListingScopes = []scopedPattern{
	{"GET /pas/payers", scopePASPayers},
	{"GET /cdex/payers", scopeCDexPayers},
}

// routeScopes: the PA workstream's, its routes' then its listings', each as a
// gateway mounting every route takes it.
func (paWorkstream) routeScopes() []scopedPattern {
	return slices.Concat(paScopedRoutes, paPASSubscriptionScopedRoutes, paListingScopes)
}

// pasPayersScope is the scope GET /pas/payers takes on this gateway: any
// scope some route it mounts under the PAS bases takes.
func (g *Gateway) pasPayersScope() routeScope {
	if g.cfg.ProviderPASSubscription {
		return scopePASPayers
	}
	return scopePASPayersUnsubscribed
}
