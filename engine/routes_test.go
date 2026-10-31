//go:build !shn_coreonly

package engine

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
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
		"POST /Claim/$submit",
		"POST /Claim/$inquire",
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
	// davinciConfigRoutePatterns are mounted when the ingress names its base
	// URL.
	davinciConfigRoutePatterns = []string{"GET /.well-known/davinci-configuration"}
)

// routeConditions is one combination of the configuration that gates a route.
// module1 is a declared origination profile: without one, module 1 mounts no
// /scenario route.
type routeConditions struct {
	ingress, ingressAuth, baseURL, module1 bool
}

func (c routeConditions) String() string {
	var on []string
	for name, set := range map[string]bool{"ingress": c.ingress, "ingressAuth": c.ingressAuth, "baseURL": c.baseURL, "module1": c.module1} {
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

// everyRouteCondition is every combination of the four conditions.
func everyRouteCondition() []routeConditions {
	var out []routeConditions
	for bits := range 16 {
		out = append(out, routeConditions{bits&1 != 0, bits&2 != 0, bits&4 != 0, bits&8 != 0})
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
			}
			if c.baseURL {
				want = append(want, davinciConfigRoutePatterns...)
			}
		}
	case "payer", "facility", "phg":
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

func TestRoutes_EachRoleMountsItsPinnedSet(t *testing.T) {
	for _, role := range []string{"provider", "payer", "facility", "phg", "", "unknown"} {
		for _, c := range everyRouteCondition() {
			got := mountedRoutes(routeGateway(role, c).routes(), everyKnownRoute())
			if want := wantRoutes(role, c); !slices.Equal(got, want) {
				t.Errorf("role %q under %s mounts\n  %v\nwant\n  %v", role, c, got, want)
			}
		}
	}
}

// The split above is the classified lists, and every condition's set is a
// subset of what the classification says the role registers.
func TestRoutes_PinnedSetsAreTheClassifiedRoutes(t *testing.T) {
	split := slices.Concat(ingressRoutePatterns, ingressAuthRoutePatterns, davinciConfigRoutePatterns)
	slices.Sort(split)
	other := slices.Clone(providerOtherRoutes)
	slices.Sort(other)
	if !slices.Equal(split, other) {
		t.Fatalf("the provider's ingress routes split by condition\n  %v\nare not providerOtherRoutes\n  %v", split, other)
	}
	for _, role := range []string{"payer", "facility", "phg"} {
		if want := wantRoutes(role, routeConditions{}); !slices.Equal(want, otherRoleRoutes[role]) {
			t.Fatalf("%s routes %v, classified %v", role, want, otherRoleRoutes[role])
		}
	}
}
