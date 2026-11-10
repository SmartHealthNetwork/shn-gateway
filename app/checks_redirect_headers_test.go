//go:build !shn_coreonly

package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	checks "github.com/SmartHealthNetwork/shn-gateway/checks"
	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
)

// The /internal/checks probes of the partner's bases carry the configured
// PAYER_DAVINCI_BACKEND_HEADERS to the partner's own system, the
// authenticated read and the anonymous one alike, and none of them, nor the
// bearer, to another origin the partner's system redirects them to.
func TestCheckTargets_ACrossOriginRedirectCarriesNoConfiguredHeader(t *testing.T) {
	var mu sync.Mutex
	var home, away []http.Header
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		away = append(away, r.Header.Clone())
		mu.Unlock()
		_, _ = io.WriteString(w, `{"resourceType":"CapabilityStatement","fhirVersion":"4.0.1"}`)
	}))
	t.Cleanup(other.Close)
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"minted","expires_in":3600}`)
	}))
	t.Cleanup(tok.Close)
	partner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		home = append(home, r.Header.Clone())
		mu.Unlock()
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(partner.Close)
	configured, err := parseBackendHeaders("X-Route-Tenant: tenant-7, X-Partner-Key: OPAQUE-PARTNER-KEY")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config{
		PayerDavinciBaseURL: partner.URL + "/fhir", PayerDavinciTokenURL: tok.URL,
		PayerDavinciClientID: "c1", PayerDavinciClientSecret: "s1",
		PayerDavinciBackendHeaders: configured,
	}
	targets := tagCheckTargets(endpointcert.New(), checkTargets(cfg), http.DefaultClient)
	var only []checks.Target
	for _, tgt := range targets {
		if tgt.ID == "PAYER_DAVINCI_BASE_URL" {
			only = append(only, tgt)
		}
	}
	if len(only) != 1 || only[0].AuthClient == nil {
		t.Fatalf("targets %+v, want the partner base with an authenticated read", only)
	}
	if _, err := checks.NewRunner(only, http.DefaultClient, nil).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(home) != 2 || len(away) != 2 {
		t.Fatalf("requests: partner %d, other origin %d; want the authenticated and the anonymous read at each", len(home), len(away))
	}
	for _, h := range home {
		if h.Get("X-Route-Tenant") != "tenant-7" || h.Get("X-Partner-Key") != "OPAQUE-PARTNER-KEY" {
			t.Errorf("the partner's own system received %v, want every configured header", h)
		}
	}
	for _, h := range away {
		for _, k := range []string{"X-Route-Tenant", "X-Partner-Key", "Authorization"} {
			if v, ok := h[k]; ok {
				t.Errorf("the other origin received %s %q, want none", k, v)
			}
		}
	}
}
