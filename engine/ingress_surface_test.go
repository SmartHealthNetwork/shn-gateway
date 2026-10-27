//go:build !shn_coreonly

package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// providerIngressSurfaceFile is the public surface a provider gateway with
// its Da Vinci ingress serves to the participant's own system: every route it
// mounts but the operator's, with ingress authentication and the ingress
// base URL both set. A hosted edge in front of the gateway forwards exactly
// this surface; the hosted control plane's listener rules are checked against
// this file, so a route added here reaches the hosted edge or fails that
// check.
var providerIngressSurfaceFile = filepath.Join("testdata", "provider-ingress-surface.json")

func providerIngressSurface() []string {
	g := &Gateway{cfg: Config{Role: "provider", IngressEnabled: true, IngressBaseURL: "https://provider.example.test"}, ingressAuth: &ingressAuthServer{}}
	var out []string
	for _, declared := range [][]mountedRoute{g.coreRoutes("provider"), g.workstreamRoutes("provider"), g.moduleRoutes("provider")} {
		for _, rt := range declared {
			if !rt.operator {
				out = append(out, rt.pattern)
			}
		}
	}
	slices.Sort(out)
	return out
}

func TestProviderIngressSurfaceIsTheMountedRoutes(t *testing.T) {
	got := providerIngressSurface()
	raw, err := os.ReadFile(providerIngressSurfaceFile)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		body, _ := json.MarshalIndent(got, "", "  ")
		t.Fatalf("%s is not the routes a provider ingress gateway mounts; it should read:\n%s", providerIngressSurfaceFile, body)
	}
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
