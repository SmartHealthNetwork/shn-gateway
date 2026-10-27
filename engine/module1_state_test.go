//go:build !shn_coreonly

package engine

import (
	"crypto/ed25519"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// withModuleOne builds module 1's state on g, a gateway assembled without
// New, as New does at boot: its empty pend state, and the routes it declares
// for g's role and origination profile. A test's profile always names a lane.
func withModuleOne(g *Gateway) *Gateway {
	st, err := moduleOne{}.newGatewayState(g)
	if err != nil {
		panic(err)
	}
	if g.moduleStates == nil {
		g.moduleStates = map[string]gatewayState{}
	}
	g.moduleStates[moduleOneID] = st
	return g
}

// declareOriginationProfile sets g's origination profile and builds module
// 1's state for it, as New does at boot, on a gateway built without that
// profile (withModuleOne).
func declareOriginationProfile(g *Gateway, profile string) {
	g.cfg.OriginationProfile = profile
	withModuleOne(g)
}

// moduleOneRoutes are the routes on-behalf building declares for a provider
// with an origination profile, in the order it declares them: every one the
// operator surface, mounted behind requireScenarioCaller.
var moduleOneRoutes = []string{
	"POST /scenario/uc01",
	"POST /scenario/uc02",
	"POST /scenario/uc02-payerb",
	"POST /scenario/uc02-unknownpayer",
	"POST /scenario/uc03",
	"POST /scenario/uc04",
	"POST /scenario/uc05",
	"POST /scenario/uc06",
	"POST /scenario/uc07",
	"POST /scenario/uc07hcpcs",
	"POST /scenario/uc08",
	"POST /scenario/homeoxygen",
	"POST /scenario/dispatch",
	"POST /scenario/uc06/start",
	"POST /scenario/uc06/complete",
	"POST /scenario/uc06/cancel",
	"POST /scenario/uc07/start",
	"POST /scenario/uc07/complete",
	"POST /scenario/uc07/cancel",
	"GET /scenario/uc07/pending",
	"POST /scenario/pa/inquire",
	"POST /scenario/reset",
}

// A gateway New builds mounts module 1's routes through the module registry
// exactly as before it was one: the same patterns, in the same order, each
// the operator surface (routes() mounts them after the core's and the
// workstreams' own: TestScenarioRoutes_EveryRouteIsClassified); and with
// scenario callers configured, each refuses a caller with no assertion
// before its handler runs. Any other role declares none, and so does a
// provider with no origination profile.
func TestModuleOne_RoutesArePinned(t *testing.T) {
	for _, profile := range OriginationProfiles() {
		cfg := originationProfileConfig(t, "provider", profile)
		pub, _ := genED25519(t)
		cfg.ScenarioCallers = map[string]ed25519.PublicKey{"operator": pub}
		g := mustNew(t, cfg)
		got := g.moduleRoutes("provider")
		if !slices.Equal(routePatterns(got), moduleOneRoutes) {
			t.Fatalf("%s: module 1 declares\n  %v\nwant\n  %v", profile, routePatterns(got), moduleOneRoutes)
		}
		for _, rt := range got {
			if !rt.operator || rt.handler == nil {
				t.Errorf("%s: %s is declared operator=%v handler=%v, want the operator surface with a handler", profile, rt.pattern, rt.operator, rt.handler != nil)
			}
		}
		h := g.Handler()
		for _, p := range moduleOneRoutes {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, probeRequest(p))
			if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), errUnauthorizedCaller) {
				t.Errorf("%s: %s with no caller assertion = %d %s, want the scenario caller refusal", profile, p, rec.Code, rec.Body.String())
			}
		}
	}
	for _, role := range originationRoles {
		for _, profile := range append(OriginationProfiles(), "") {
			if role == "provider" && profile != "" {
				continue
			}
			if got := mustNew(t, originationProfileConfig(t, role, profile)).moduleRoutes(role); len(got) != 0 {
				t.Errorf("%s with profile %q declares %v, want none", role, profile, routePatterns(got))
			}
		}
	}
}

// Reset on a gateway assembled without New, which keeps no module 1 state,
// still runs its store half and reports that half's error, as it did when
// the Gateway held the pend map itself; with the state, it clears the pend
// map too.
func TestReset_WithoutModuleOneState(t *testing.T) {
	fake := &fakeExchangeStore{mem: NewInMemoryExchangeStore(time.Hour, time.Now)}
	g := &Gateway{exchanges: fake}
	if err := g.Reset(); err != nil || fake.resets != 1 {
		t.Fatalf("Reset without module 1's state = %v after %d store resets, want nil after 1", err, fake.resets)
	}
	failing := &fakeExchangeStore{mem: NewInMemoryExchangeStore(time.Hour, time.Now), failReset: errors.New("store down")}
	if err := (&Gateway{exchanges: failing}).Reset(); err == nil {
		t.Fatal("Reset without module 1's state hid the store half's error")
	}
	with := withModuleOne(&Gateway{exchanges: fake})
	token := with.storePending(pendState{scenario: "uc06"})
	if err := with.Reset(); err != nil {
		t.Fatal(err)
	}
	if _, ok := with.loadPending(token); ok {
		t.Fatal("Reset left a pend in module 1's state")
	}
}
