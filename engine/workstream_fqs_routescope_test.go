//go:build !shn_coreonly

package engine

import (
	"fmt"
	"net/http"
	"testing"
)

// TestFederatedQueryRouteScope: the federated query's route takes a scope that
// permits creating a Task: a client registered for none is refused 403
// insufficient_scope at every conformance level, recorded as authority; one
// registered for system/Task.c, a wildcard with c, or the umbrella is
// admitted.
func TestFederatedQueryRouteScope(t *testing.T) {
	priv, pub := newTestClientKey(t)
	route := paRouteRequest{"federated query", http.MethodPost, "/Task", scopeFederatedQuery, "system/Task.c"}
	for _, level := range allLevels {
		for _, registered := range []string{"system/Subscription.*", "system/Task.r", "system/Claim.c", "system/*.rs"} {
			t.Run(fmt.Sprintf("%s/%s", level, registered), func(t *testing.T) {
				var got exchangeRecords
				g := providerScopeGateway(t, pub, []string{registered}, func(c *Config) {
					c.ConformanceEnforcement = level
					c.ExchangeObserved = got.observe
				})
				wantScopeRefused(t, providerCall(g, route.method, route.path, providerDirectBearer(t, priv, "")), &got, route)
			})
		}
	}
	for _, registered := range []string{"system/Task.c", "system/Task.write", "system/*.c", ProviderUmbrellaScope} {
		t.Run("admitted/"+registered, func(t *testing.T) {
			g := providerScopeGateway(t, pub, []string{registered})
			wantScopeAdmitted(t, providerCall(g, route.method, route.path, providerDirectBearer(t, priv, "")))
		})
	}
}
