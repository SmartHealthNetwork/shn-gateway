//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// wantNosniff fails unless h, an answer's headers, carries exactly one
// X-Content-Type-Options value, nosniff. The names are spelled out here, not
// read from the package that sets them, so a change to either is caught.
func wantNosniff(t *testing.T, h http.Header, what string) {
	t.Helper()
	if got := h.Values("X-Content-Type-Options"); len(got) != 1 || got[0] != "nosniff" {
		t.Errorf("%s: X-Content-Type-Options %q, want exactly [nosniff]", what, got)
	}
}

// routeWildcard is a path wildcard of a ServeMux pattern, {x} or {x...}.
var routeWildcard = regexp.MustCompile(`\{[^}]*\}`)

// nosniffProbe is a request that names the route pattern p, each wildcard
// filled in by a literal segment, with a body of {} for a method that takes
// one. It fails unless the request matches p on a ServeMux holding p alone.
func nosniffProbe(t *testing.T, ctx context.Context, p string) *http.Request {
	t.Helper()
	method, path, ok := strings.Cut(p, " ")
	if !ok {
		method, path = http.MethodGet, p
	}
	var body io.Reader
	if method != http.MethodGet {
		body = strings.NewReader(`{}`)
	}
	r := httptest.NewRequestWithContext(ctx, method, routeWildcard.ReplaceAllString(path, "probe"), body)
	// Control: the request is one the route serves, not one the mux answers
	// 404 or 405 around it.
	alone := http.NewServeMux()
	alone.Handle(p, http.NotFoundHandler())
	if _, got := alone.Handler(r); got != p {
		t.Fatalf("control: %s %s matches pattern %q, want %q", r.Method, r.URL.Path, got, p)
	}
	return r
}

// noNetwork is a transport that refuses every request, and records its
// host, so no answer a route writes waits on, or reaches, the network.
type noNetwork struct {
	mu    sync.Mutex
	hosts []string
}

func (n *noNetwork) RoundTrip(r *http.Request) (*http.Response, error) {
	n.mu.Lock()
	n.hosts = append(n.hosts, r.URL.Host)
	n.mu.Unlock()
	return nil, fmt.Errorf("nosniff test: no network: %s", r.URL.Host)
}

// nosniffGateways are gateways New builds, one per shape a participant can
// configure, that between them mount every route a role can: a provider
// with nothing set, a provider with its ingress (behind its own
// authorization server, with its base URL), its origination profile and its
// own Task endpoint, a payer with and without its ingress, a facility and a
// PHG. Every client they hold refuses the network (net), and the operator
// routes are behind a caller to verify. (routeGateway's bare gateways mount
// the same routes but cannot serve them: a route reads what New builds.)
func nosniffGateways(t *testing.T, net *noNetwork) map[string]*Gateway {
	t.Helper()
	client := &http.Client{Transport: net}
	_, clientPub := newTestClientKey(t)
	operator, _ := genED25519(t)
	hubKey, _ := genED25519(t)
	build := func(role string, ingress bool, set func(*Config)) *Gateway {
		cfg := mountConfig(t, role)
		if role == "payer" {
			cfg.Responder = NewNativeResponder(client, "http://payer.test", fixedClock)
		}
		cfg.Client = client
		cfg.HubURL = "http://hub.test"
		cfg.Reg = shnsdk.NewRegistry()
		cfg.Validator = shnsdk.NewFakeValidator()
		cfg.HubTransportPub = hubKey
		cfg.ScenarioCallers = map[string]ed25519.PublicKey{"operator": operator}
		if ingress {
			cfg.IngressEnabled = true
			cfg.IngressBaseURL = testIngressBaseURL
			cfg.IngressClients = map[string]IngressClientRegistration{
				"ehr": {Alg: "ES384", PublicKeyPEM: clientPub, Scopes: []string{ingressScope}},
			}
		}
		if set != nil {
			set(&cfg)
		}
		return mustNew(t, cfg)
	}
	return map[string]*Gateway{
		"provider with nothing set": build("provider", false, nil),
		"provider with everything set": build("provider", true, func(c *Config) {
			c.OriginationProfile = "demo"
			c.ProviderCDexTaskURL = "https://provider.test/fhir/Task"
			c.ProviderCDexTaskClient = client
		}),
		"payer": build("payer", false, nil),
		"payer with ingress": build("payer", true, func(cfg *Config) {
			// and with its CDS client key's JWK Set
			cfg.CDSClientJWKS = []byte(`{"keys":[]}`)
		}),
		"facility": build("facility", false, nil),
		"phg":      build("phg", false, nil),
	}
}

// Every answer the engine's own Handler writes carries
// X-Content-Type-Options: nosniff, whatever its status, so a program that
// embeds the engine serves it without the gateway's listener: every route
// each gateway mounts, sent with no credential, and a path no route serves.
// The answers are the routes' refusals or unauthenticated answers, all back
// before a short deadline, and none reaches the network. Control: the
// routes walked are every route any role mounts under any condition
// (everyKnownRoute), so a route added later is walked with no new row.
func TestHandler_EveryRouteCarriesNosniff(t *testing.T) {
	t.Parallel()
	net := &noNetwork{}
	walked := map[string]bool{}
	for name, g := range nosniffGateways(t, net) {
		h := g.Handler()
		for _, p := range g.RoutePatternsForTest() {
			walked[p] = true
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, nosniffProbe(t, ctx, p))
			if ctx.Err() != nil {
				t.Errorf("%s: %s answered %d only at the deadline", name, p, rec.Code)
			}
			cancel()
			wantNosniff(t, rec.Result().Header, fmt.Sprintf("%s: %s answered %d", name, p, rec.Code))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/no-such-route", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: GET /no-such-route answered %d, want the mux's 404", name, rec.Code)
		}
		wantNosniff(t, rec.Result().Header, name+": the mux's 404")
	}
	var unwalked []string
	for _, p := range everyKnownRoute() {
		if !walked[p] {
			unwalked = append(unwalked, p)
		}
	}
	if len(unwalked) > 0 || len(walked) == 0 {
		t.Errorf("control: routes a role mounts but none walked: %q (%d walked)", unwalked, len(walked))
	}
	if len(net.hosts) > 0 {
		t.Errorf("a route reached for the network: %q", net.hosts)
	}
}

// An operator route's refusal of a caller that states no assertion, 401,
// carries nosniff; so does a gzip-coded answer, beside its
// Content-Encoding: the header is set before the coding wraps the answer.
// Control: the refusal is the 401, and the answer is gzip-coded.
func TestHandler_RefusalsAndCodedAnswersCarryNosniff(t *testing.T) {
	t.Parallel()
	t.Run("operator route refused", func(t *testing.T) {
		g := routeGateway("provider", routeConditions{module1: true})
		pub, _ := genED25519(t)
		g.cfg.ScenarioCallers = map[string]ed25519.PublicKey{"operator": pub}
		rec := httptest.NewRecorder()
		g.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/scenario/uc01", strings.NewReader(`{}`)))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("control: POST /scenario/uc01 with no assertion answered %d, want 401", rec.Code)
		}
		wantNosniff(t, rec.Result().Header, "the operator route's 401")
	})
	t.Run("gzip-coded answer", func(t *testing.T) {
		_, pub := newTestClientKey(t)
		g := gatewayWithAuth(t, "br-provider", pub)
		rec := httptest.NewRecorder()
		(&acceptsGzip{g.Handler()}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metadata", nil))
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "gzip" {
			t.Fatalf("control: GET /metadata accepting gzip answered %d, Content-Encoding %q, want 200 gzip", rec.Code, rec.Header().Get("Content-Encoding"))
		}
		wantNosniff(t, rec.Result().Header, "the gzip-coded answer")
	})
}

// A payer's answer relayed to the EHR keeps the payer's own Content-Type,
// byte for byte, and its own body, with nosniff added beside them, at every
// level: the header is the gateway's, and never a reason to rewrite a peer's
// media type. The answer is a CDex submission's, driven through the provider
// gateway's own Handler so the header's wrapper is on the path.
func TestHandler_APeersContentTypeIsCarriedBesideNosniff(t *testing.T) {
	t.Parallel()
	for _, row := range []struct{ sent, written string }{
		{"application/json", "application/json"},
		{"application/fhir+json; fhirVersion=4.0", "application/fhir+json; fhirVersion=4.0"},
	} {
		for _, level := range allLevels {
			t.Run(row.sent+"/"+level.String(), func(t *testing.T) {
				env := newInProcessExchange(t)
				env.originator.cfg.ConformanceEnforcement = level
				env.payerReturns(LegResult{Response: relay.ForTest(cdexPayersAnswer, row.sent), AnswerStatus: http.StatusOK})
				rec := httptest.NewRecorder()
				env.originator.Handler().ServeHTTP(rec, cdexPost(cdexSubmissionBody("MBR-COVERED", cdexPayerCMS), ""))
				if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), cdexPayersAnswer) {
					t.Fatalf("answer %d %s, want the payer's 200 relayed exactly", rec.Code, rec.Body)
				}
				if got := rec.Result().Header.Values("Content-Type"); !slices.Equal(got, []string{row.written}) {
					t.Fatalf("Content-Type %q, want exactly %q for the payer's %q", got, row.written, row.sent)
				}
				wantNosniff(t, rec.Result().Header, "the relayed answer")
			})
		}
	}
}
