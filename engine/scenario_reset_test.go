package engine

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestScenarioResetRoute is the regression guard for the cloud Mode A composite reset
// (separated-reset-clears-gateway-state): the separated/cloud console resets BOTH the provider
// gateway and the native-forward (composite) payer gateway via POST /scenario/reset. The route
// must therefore be registered for the provider role AND for the native-forward payer — but NOT
// for the built-in sandbox payer, whose payer-gw is PUBLIC (fhir.<apex>) and must never expose an
// unauthenticated state-clearing endpoint. handleScenarioReset is generic g.Reset() (clears
// pending+exchanges), so a zero-value Gateway exercises the routing without any substrate.
func TestScenarioResetRoute(t *testing.T) {
	cases := []struct {
		name        string
		role        string
		payerNative bool
		wantStatus  int
	}{
		{"provider (internal harness)", "provider", false, http.StatusOK},
		{"composite payer (native-forward, internal)", "payer", true, http.StatusOK},
		{"sandbox payer (built-in, PUBLIC)", "payer", false, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &Gateway{cfg: Config{Role: tc.role, PayerDavinciNative: tc.payerNative}}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/scenario/reset", nil)
			g.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("POST /scenario/reset for %s: got %d, want %d", tc.name, rec.Code, tc.wantStatus)
			}
		})
	}
}
