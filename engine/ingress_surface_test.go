//go:build !shn_coreonly

package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/pa/cdexroute"
)

// providerIngressSurfaceFile is the public surface a provider gateway with
// its Da Vinci ingress serves to the participant's own system: every route it
// mounts but the operator's, with ingress authentication and the ingress
// base URL both set, and the payer's Subscription to its Tasks' changes taken
// (PROVIDER_CDEX_SUBSCRIPTION_SCOPE), which mounts the notification route. A hosted edge in front of the gateway forwards exactly
// this surface; the hosted control plane's listener rules are checked against
// this file, so a route added here reaches the hosted edge or fails that
// check.
var providerIngressSurfaceFile = filepath.Join("testdata", "provider-ingress-surface.json")

// payerIngressSurfaceFile is the same for a payer gateway with its Da Vinci
// ingress (the payer's own system creating its attachment-request Task), the
// Hub's inbound route among it: every route a payer gateway with its ingress
// on mounts but the operator's.
var payerIngressSurfaceFile = filepath.Join("testdata", "payer-ingress-surface.json")

// ingressSurface is every route a gateway of role mounts with its ingress on,
// its authentication and base set, and every setting that mounts a route of
// its own set, but the operator's, sorted.
func ingressSurface(role string) []string {
	cfg := Config{Role: role, IngressEnabled: true, IngressBaseURL: "https://" + role + ".example.test"}
	if role == "provider" {
		// The notification route is mounted only where the gateway takes the
		// payer's Subscription.
		cfg.ProviderCDexTaskURL, cfg.ProviderCDexSubscriptionScope = "https://ehr.example.test/fhir/Task", "system/Subscription.cruds"
	}
	g := &Gateway{cfg: cfg, ingressAuth: &ingressAuthServer{}}
	var out []string
	for _, declared := range [][]mountedRoute{g.coreRoutes(role), g.workstreamRoutes(role), g.moduleRoutes(role)} {
		for _, rt := range declared {
			if !rt.operator {
				out = append(out, rt.pattern)
			}
		}
	}
	slices.Sort(out)
	return out
}

func providerIngressSurface() []string { return ingressSurface("provider") }

// wantSurface checks file reads exactly got.
func wantSurface(t *testing.T, file, role string, got []string) {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		body, _ := json.MarshalIndent(got, "", "  ")
		t.Fatalf("%s is not the routes a %s ingress gateway mounts; it should read:\n%s", file, role, body)
	}
}

// The payer's surface is the routes a payer gateway with its ingress mounts,
// and holds the payer ingress's routes: a hosted payer's edge is checked
// against it as a provider's is against its own.
func TestPayerIngressSurfaceIsTheMountedRoutes(t *testing.T) {
	got := ingressSurface("payer")
	wantSurface(t, payerIngressSurfaceFile, "payer", got)
	for _, route := range []string{"POST " + cdexroute.ProviderPrefix + "{provider}/Task", "GET " + CDexProvidersPath, "POST /oauth/token", "GET /.well-known/smart-configuration"} {
		if !slices.Contains(got, route) {
			t.Errorf("the payer's ingress surface %v leaves out %s", got, route)
		}
	}
}

func TestProviderIngressSurfaceIsTheMountedRoutes(t *testing.T) {
	got := providerIngressSurface()
	wantSurface(t, providerIngressSurfaceFile, "provider", got)
	// Each operator route is left out, and only those.
	operator := &Gateway{cfg: Config{Role: "provider"}}
	declareOriginationProfile(operator, "demo")
	if len(operator.moduleRoutes("provider")) == 0 {
		t.Fatal("control: a provider with an origination profile declares no operator route; the check below would be vacuous")
	}
	for _, rt := range operator.moduleRoutes("provider") {
		if slices.Contains(got, rt.pattern) {
			t.Errorf("operator route %s is on the public surface", rt.pattern)
		}
	}
}
