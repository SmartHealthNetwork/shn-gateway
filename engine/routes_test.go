//go:build !shn_coreonly

package engine

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
)

// The routes each role mounts, under each condition that gates one, are
// pinned: of the classified routes, mounting them from what each piece of the
// gateway declares (AI-G10) mounts exactly these (a route outside the lists
// fails TestScenarioRoutes_EveryRouteIsClassified). The route lists are the
// ones that test classifies; providerOtherRoutes is split here by the
// condition each is mounted under.
var (
	// ingressRoutePatterns are mounted when the Da Vinci ingress is enabled.
	ingressRoutePatterns = []string{
		"GET /cds-services",
		"POST /cds-services/{id}",
		"POST /Questionnaire/$questionnaire-package",
		"POST /Questionnaire/$next-question",
		"POST /dtr/payer/{payer}/{rest...}",
		"POST /dtr/fetch-server/{payer}/{line}/{tag}/Questionnaire/$next-question",
		"POST /Claim/$submit",
		"POST /Claim/$inquire",
		"POST /pas/payer/{payer}/Claim/$submit",
		"POST /pas/payer/{payer}/Claim/$inquire",
		"GET /pas/payers",
		"POST /Subscription",
		"GET /Subscription",
		"GET /Subscription/{rest...}",
		"PUT /Subscription/{id}",
		"PATCH /Subscription/{id}",
		"DELETE /Subscription/{id}",
		"POST /CoverageEligibilityRequest/$submit",
		"POST /$submit-attachment",
		"POST /cdex/payer/{payer}",
		"POST /cdex/payer/{payer}/$submit-attachment",
		"GET /cdex/payers",
		"POST /Task",
		"GET /metadata",
	}
	// ingressAuthRoutePatterns are mounted with the ingress's own
	// authorization server.
	ingressAuthRoutePatterns = []string{
		"POST /oauth/token",
		"GET /.well-known/smart-configuration",
	}
	// providerIngressAuthRoutePatterns are mounted on a provider whose
	// ingress authenticates its callers: a payer's base's SMART
	// configuration.
	providerIngressAuthRoutePatterns = []string{"GET /pas/payer/{payer}/.well-known/smart-configuration"}
	// davinciConfigRoutePatterns are mounted when the ingress names its base
	// URL.
	davinciConfigRoutePatterns = []string{"GET /.well-known/davinci-configuration"}
	// payerIngressRoutePatterns are mounted on a payer when its Da Vinci
	// ingress is enabled: the attachment-request Task routes, at a
	// provider's base and at the plain Task endpoint; the Task's read and
	// vread at both (the plain endpoint's answered 404); each base's
	// CapabilityStatement; and the Subscription routes at a provider's base
	// (at the plain base, refused).
	payerIngressRoutePatterns = []string{
		"POST /cdex/provider/{provider}/Task",
		"POST /Task",
		"GET /cdex/providers",
		"GET /cdex/provider/{provider}/Task/{id}",
		"GET /cdex/provider/{provider}/Task/{id}/_history/{vid}",
		"GET /Task/{id}",
		"GET /Task/{id}/_history/{vid}",
		"GET /metadata",
		"GET /cdex/provider/{provider}/metadata",
		"POST /cdex/provider/{provider}/Subscription",
		"GET /cdex/provider/{provider}/Subscription",
		"GET /cdex/provider/{provider}/Subscription/{rest...}",
		"PUT /cdex/provider/{provider}/Subscription/{rest...}",
		"DELETE /cdex/provider/{provider}/Subscription/{rest...}",
		"PATCH /cdex/provider/{provider}/Subscription/{rest...}",
		"POST /Subscription",
		"GET /Subscription",
		"GET /Subscription/{rest...}",
	}
	// payerIngressAuthRoutePatterns are mounted on a payer whose ingress
	// authenticates its callers: a provider's base's SMART configuration.
	payerIngressAuthRoutePatterns = []string{"GET /cdex/provider/{provider}/.well-known/smart-configuration"}
	// cdexTaskRoutePatterns are mounted on a provider that sets its own Task
	// endpoint: it takes the payer's attachment request.
	cdexTaskRoutePatterns = []string{"POST /substrate/inbound"}
	// notifyRoutePatterns are mounted on a provider whose ingress is enabled
	// and that sets its subscription scope: its own system's notifications of
	// a payer's Tasks' changes, at the addresses its gateway writes.
	notifyRoutePatterns = []string{"POST /notify/cdex-task/{subscriber}/{kid}/{sealed}"}
	// pasSubscriptionRoutePatterns are mounted on a provider whose ingress is
	// enabled and that takes the PAS topic's notifications: its own system's
	// Subscription to the PAS topic at each payer's base.
	pasSubscriptionRoutePatterns = []string{
		"POST /pas/payer/{payer}/Subscription",
		"GET /pas/payer/{payer}/Subscription",
		"GET /pas/payer/{payer}/Subscription/{rest...}",
		"PUT /pas/payer/{payer}/Subscription/{rest...}",
		"DELETE /pas/payer/{payer}/Subscription/{rest...}",
		"PATCH /pas/payer/{payer}/Subscription/{rest...}",
	}
	// pasNotifyRoutePatterns are mounted on a payer whose ingress is enabled
	// and that sets its subscription scope: its own system's notifications of
	// a provider's Subscription to the PAS topic, at the addresses its
	// gateway writes.
	pasNotifyRoutePatterns = []string{"POST /notify/pas/{subscriber}/{kid}/{sealed}"}
	// cdsClientJWKSRoutePatterns are mounted on a payer whose CDS calls carry
	// a CDS Hooks client JWT: the public JWK Set of its key, with or without
	// the ingress.
	cdsClientJWKSRoutePatterns = []string{"GET " + smartauth.CDSClientJWKSPath}
)

// routeConditions is one combination of the configuration that gates a route.
// module1 is a declared origination profile: without one, module 1 mounts no
// /scenario route.
type routeConditions struct {
	ingress, ingressAuth, baseURL, module1, cdexTask, cdsClientJWKS, cdexRead, cdexSubscription, payerSubscription, providerPAS bool
}

func (c routeConditions) String() string {
	var on []string
	for name, set := range map[string]bool{"ingress": c.ingress, "ingressAuth": c.ingressAuth, "baseURL": c.baseURL, "module1": c.module1, "cdexTask": c.cdexTask, "cdsClientJWKS": c.cdsClientJWKS, "cdexRead": c.cdexRead, "cdexSubscription": c.cdexSubscription, "payerSubscription": c.payerSubscription, "providerPAS": c.providerPAS} {
		if set {
			on = append(on, name)
		}
	}
	slices.Sort(on)
	if len(on) == 0 {
		return "none"
	}
	return strings.Join(on, "+")
}

// everyRouteCondition is every combination of the ten conditions.
func everyRouteCondition() []routeConditions {
	var out []routeConditions
	for bits := range 1024 {
		out = append(out, routeConditions{bits&1 != 0, bits&2 != 0, bits&4 != 0, bits&8 != 0, bits&16 != 0, bits&32 != 0, bits&64 != 0, bits&128 != 0, bits&256 != 0, bits&512 != 0})
	}
	return out
}

// wantRoutes is the route set a role mounts under c.
func wantRoutes(role string, c routeConditions) []string {
	var want []string
	switch role {
	case "provider":
		if c.module1 {
			want = append(want, originatingRoutes...)
			want = append(want, localScenarioRoutes...)
		}
		if c.ingress {
			want = append(want, ingressRoutePatterns...)
			if c.ingressAuth {
				want = append(want, ingressAuthRoutePatterns...)
				want = append(want, providerIngressAuthRoutePatterns...)
			}
			if c.baseURL {
				want = append(want, davinciConfigRoutePatterns...)
			}
		}
		// A provider whose participant sets its own Task endpoint takes
		// the payer's attachment request, one that sets the read scope the
		// payer's read of its Task, and one that asks for the PAS topic's
		// notifications a payer's, each delivered on the inbound route.
		if c.cdexTask || c.cdexRead || c.cdexSubscription || c.providerPAS {
			want = append(want, cdexTaskRoutePatterns...)
		}
		// One that sets the subscription scope, with its ingress, takes its
		// own system's notifications at the addresses it writes.
		if c.ingress && c.cdexSubscription {
			want = append(want, notifyRoutePatterns...)
		}
		// One that takes the PAS topic's notifications, with its ingress,
		// takes its own system's Subscription to that topic at a payer's
		// base.
		if c.ingress && c.providerPAS {
			want = append(want, pasSubscriptionRoutePatterns...)
		}
	case "payer":
		want = append(want, "POST /substrate/inbound")
		if c.cdsClientJWKS {
			want = append(want, cdsClientJWKSRoutePatterns...)
		}
		if c.ingress {
			want = append(want, payerIngressRoutePatterns...)
			if c.ingressAuth {
				want = append(want, ingressAuthRoutePatterns...)
				want = append(want, payerIngressAuthRoutePatterns...)
			}
			// One that sets the subscription scope takes its own system's
			// PAS notifications at the addresses it writes.
			if c.payerSubscription {
				want = append(want, pasNotifyRoutePatterns...)
			}
		}
	case "facility", "phg":
		want = append(want, "POST /substrate/inbound")
	}
	slices.Sort(want)
	return slices.Compact(want)
}

// everyKnownRoute is the union of every role's routes under every condition.
func everyKnownRoute() []string {
	var all []string
	for _, role := range []string{"provider", "payer", "facility", "phg"} {
		for _, c := range everyRouteCondition() {
			all = append(all, wantRoutes(role, c)...)
		}
	}
	slices.Sort(all)
	return slices.Compact(all)
}

// routeGateway is a gateway with only what mounting its routes reads: role,
// the gating configuration, and module 1's state built from them, as New
// builds it. Nothing is served through it.
func routeGateway(role string, c routeConditions) *Gateway {
	g := &Gateway{cfg: Config{Role: role, IngressEnabled: c.ingress}}
	if c.ingressAuth {
		g.ingressAuth = &ingressAuthServer{}
	}
	if c.baseURL {
		g.cfg.IngressBaseURL = "https://provider.example"
	}
	if c.module1 {
		g.cfg.OriginationProfile = "demo"
	}
	if c.cdexTask {
		g.cfg.ProviderCDexTaskURL = "https://provider.example/fhir/Task"
	}
	if c.cdsClientJWKS {
		g.cfg.CDSClientJWKS = []byte(`{"keys":[]}`)
	}
	if c.cdexRead {
		g.cfg.ProviderCDexReadScope = "system/Task.r"
	}
	if c.cdexSubscription {
		g.cfg.ProviderCDexSubscriptionScope = "system/Subscription.cruds"
	}
	if c.payerSubscription {
		g.cfg.PayerSubscriptionScope = "system/Subscription.cruds"
	}
	if c.providerPAS {
		g.cfg.ProviderPASSubscription = true
	}
	return withModuleOne(g)
}

// probeRequest is a request that names the route pattern p, with each
// wildcard filled in.
func probeRequest(p string) *http.Request {
	method, path, _ := strings.Cut(p, " ")
	path = strings.ReplaceAll(path, "{id}", "probe")
	return httptest.NewRequest(method, path, nil)
}

// mountedRoutes is the subset of candidates mux serves under exactly that
// pattern.
func mountedRoutes(mux *http.ServeMux, candidates []string) []string {
	var got []string
	for _, p := range candidates {
		if _, pattern := mux.Handler(probeRequest(p)); pattern == p {
			got = append(got, p)
		}
	}
	return got
}

// Each role mounts exactly its pinned set under every combination of the
// route conditions. Each role runs as a top-level test of its own, so the
// engine's hash shards (tools/ci/test-shard.sh -hash, which splits by
// top-level test name) spread the 1,024 conditions' gateways across shards
// rather than build every role's in one; TestRoutes_PinnedSetSplitRunsEveryRole
// holds the six.
func TestRoutes_TheProviderMountsItsPinnedSet(t *testing.T)   { roleMountsItsPinnedSet(t, "provider") }
func TestRoutes_ThePayerMountsItsPinnedSet(t *testing.T)      { roleMountsItsPinnedSet(t, "payer") }
func TestRoutes_TheFacilityMountsItsPinnedSet(t *testing.T)   { roleMountsItsPinnedSet(t, "facility") }
func TestRoutes_ThePHGMountsItsPinnedSet(t *testing.T)        { roleMountsItsPinnedSet(t, "phg") }
func TestRoutes_NoRoleMountsItsPinnedSet(t *testing.T)        { roleMountsItsPinnedSet(t, "") }
func TestRoutes_AnUnknownRoleMountsItsPinnedSet(t *testing.T) { roleMountsItsPinnedSet(t, "unknown") }

// roleMountsItsPinnedSet holds role's mounted routes to its pinned set under
// every condition.
func roleMountsItsPinnedSet(t *testing.T, role string) {
	t.Parallel()
	for _, c := range everyRouteCondition() {
		got := mountedRoutes(routeGateway(role, c).routes(), everyKnownRoute())
		if want := wantRoutes(role, c); !slices.Equal(got, want) {
			t.Errorf("role %q under %s mounts\n  %v\nwant\n  %v", role, c, got, want)
		}
	}
}

// The split above is the classified lists, and every condition's set is a
// subset of what the classification says the role registers.
func TestRoutes_PinnedSetsAreTheClassifiedRoutes(t *testing.T) {
	split := slices.Concat(ingressRoutePatterns, ingressAuthRoutePatterns, providerIngressAuthRoutePatterns, davinciConfigRoutePatterns, cdexTaskRoutePatterns, notifyRoutePatterns, pasSubscriptionRoutePatterns)
	slices.Sort(split)
	other := slices.Clone(providerOtherRoutes)
	slices.Sort(other)
	if !slices.Equal(split, other) {
		t.Fatalf("the provider's routes split by condition\n  %v\nare not providerOtherRoutes\n  %v", split, other)
	}
	for _, role := range []string{"payer", "facility", "phg"} {
		var want []string
		for _, c := range everyRouteCondition() {
			want = append(want, wantRoutes(role, c)...)
		}
		slices.Sort(want)
		want = slices.Compact(want)
		classified := slices.Clone(otherRoleRoutes[role])
		slices.Sort(classified)
		if !slices.Equal(want, classified) {
			t.Fatalf("%s routes %v, classified %v", role, want, classified)
		}
	}
}
