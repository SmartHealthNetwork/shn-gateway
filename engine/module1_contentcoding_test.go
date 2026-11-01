//go:build !shn_coreonly

package engine

import (
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/scenarioauth"
)

// Every operator route refuses a body in a coding the gateway does not decode
// 415 naming gzip, an originating route recording its refusal on message
// integrity: with no caller to verify, before the route runs; with callers,
// once the caller has stated an assertion (an unstated one is refused on
// authentication first) and the body is read to verify it.
func TestScenarioRoutes_AnUnsupportedCodingIs415(t *testing.T) {
	t.Parallel()
	for _, authenticated := range []bool{false, true} {
		for _, rt := range originationRouteTable {
			t.Run(rt.pattern, func(t *testing.T) {
				g, _, _ := crdTestSystem(t, uc03Coverage())
				declareOriginationProfile(g, "demo")
				if authenticated {
					pub, _ := genED25519(t)
					g.cfg.ScenarioCallers = map[string]ed25519.PublicKey{"operator": pub}
				}
				var got exchangeRecords
				g.cfg.ExchangeObserved = got.observe
				method, path, _ := strings.Cut(rt.pattern, " ")
				r := httptest.NewRequest(method, path, strings.NewReader(`{}`))
				r.Header.Set("Content-Encoding", "br")
				if authenticated {
					r.Header.Set(scenarioauth.HeaderName, "stated")
				}
				rec := httptest.NewRecorder()
				g.Handler().ServeHTTP(rec, r)
				wantUnsupportedCoding(t, rec)
				if rt.originates {
					wantRefusal(t, got.only(t), http.StatusUnsupportedMediaType, RefusedByProviderGateway, RefusalIntegrity)
				}
			})
		}
	}
}

// TestScenarioRoutes_ACallerWithNoAssertionIsRefusedBeforeTheCoding: with
// callers to verify, an operator route refuses a caller that states no
// assertion on authentication, 401, whatever the coding of its body.
func TestScenarioRoutes_ACallerWithNoAssertionIsRefusedBeforeTheCoding(t *testing.T) {
	for _, rt := range originationRouteTable {
		t.Run(rt.pattern, func(t *testing.T) {
			g, _, _ := crdTestSystem(t, uc03Coverage())
			declareOriginationProfile(g, "demo")
			pub, _ := genED25519(t)
			g.cfg.ScenarioCallers = map[string]ed25519.PublicKey{"operator": pub}
			var got exchangeRecords
			g.cfg.ExchangeObserved = got.observe
			method, path, _ := strings.Cut(rt.pattern, " ")
			r := httptest.NewRequest(method, path, strings.NewReader(`{}`))
			r.Header.Set("Content-Encoding", "br")
			rec := httptest.NewRecorder()
			g.Handler().ServeHTTP(rec, r)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if rt.originates {
				wantRefusal(t, got.only(t), http.StatusUnauthorized, RefusedByProviderGateway, RefusalAuthentication)
			}
		})
	}
}

// TestScenarioRoutes_AnswersAreNeverCompressed: an operator route's answers
// (a scenario's resume token among them) are never gzip-coded, and do not
// vary by Accept-Encoding: the routes are not wrapped at all. Control: the
// same gateway's other routes are.
func TestScenarioRoutes_AnswersAreNeverCompressed(t *testing.T) {
	g, _, _ := crdTestSystem(t, uc03Coverage())
	declareOriginationProfile(g, "demo")
	g.cfg.IngressEnabled = true
	h := &acceptsGzip{g.Handler()}
	for _, rt := range originationRouteTable {
		method, path, _ := strings.Cut(rt.pattern, " ")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(`{}`)))
		if ce, v := rec.Header().Get("Content-Encoding"), rec.Header().Get("Vary"); ce != "" || v != "" {
			t.Fatalf("%s: Content-Encoding %q, Vary %q", rt.pattern, ce, v)
		}
	}
	controls := 0
	for _, rt := range append(g.coreRoutes("provider"), g.workstreamRoutes("provider")...) {
		method, path, _ := strings.Cut(rt.pattern, " ")
		if rt.credential != noCredential || strings.Contains(path, "{") {
			continue
		}
		controls++
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(`{}`)))
		if rec.Header().Get("Vary") != "Accept-Encoding" {
			t.Fatalf("control: %s answered %d, Vary %q", rt.pattern, rec.Code, rec.Header().Get("Vary"))
		}
	}
	if controls == 0 {
		t.Fatal("control: the gateway mounts no other route to compare")
	}
}
