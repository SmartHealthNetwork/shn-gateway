package engine

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/scenarioauth"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// scenarioCallerProvider is unsetProfileProvider's provider with its origination
// profile set and its /scenario callers configured, every outbound request
// counted and every observer event recorded.
func scenarioCallerProvider(t *testing.T, callers map[string]ed25519.PublicKey, mutate ...func(*Config)) (*Gateway, *countingTransport, *[]ObserverEvent) {
	t.Helper()
	g, _, counter, events := unsetProfileProvider(t, false)
	cfg := g.cfg
	cfg.OriginationProfile = "demo"
	cfg.ScenarioCallers = callers
	for _, m := range mutate {
		m(&cfg)
	}
	g2 := mustNew(t, cfg)
	*events = nil
	return g2, counter, events
}

// signedScenario is a request to route, signed as label with priv for the
// provider, the holder id scenarioCallerProvider's gateway runs as.
func signedScenario(t *testing.T, route, body, label string, priv ed25519.PrivateKey, now time.Time) *http.Request {
	t.Helper()
	return signedScenarioFor(t, "provider", route, body, label, priv, now)
}

// signedScenarioFor is a request to route, signed as label with priv for the
// gateway whose holder id is gateway.
func signedScenarioFor(t *testing.T, gateway, route, body, label string, priv ed25519.PrivateKey, now time.Time) *http.Request {
	t.Helper()
	method, path, _ := strings.Cut(route, " ")
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if err := scenarioauth.Sign(r, label, gateway, priv, now); err != nil {
		t.Fatal(err)
	}
	return r
}

func callerKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

// wantUnauthorized asserts the one answer every refused caller gets.
func wantUnauthorized(t *testing.T, name string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusUnauthorized || strings.TrimSpace(rec.Body.String()) != `{"error":"unauthorized caller"}` {
		t.Errorf("%s: %d %s, want 401 unauthorized caller", name, rec.Code, rec.Body.String())
	}
}

// Every /scenario route refuses an unsigned caller, at every conformance
// level, before it reads, builds or sends anything: no outbound request, no
// observer event.
func TestScenarioCaller_UnsignedRefusedBeforeAnything(t *testing.T) {
	pub, _ := callerKey(t)
	for _, level := range allLevels {
		g, counter, events := scenarioCallerProvider(t, map[string]ed25519.PublicKey{"monitor": pub}, func(c *Config) { c.ConformanceEnforcement = level })
		for _, route := range append(append([]string{}, originatingRoutes...), localScenarioRoutes...) {
			rec := serve(g, route, `{"continuation":"c1","resumeToken":"r1","answers":{}}`)
			wantUnauthorized(t, level.String()+" "+route, rec)
		}
		if counter.n != 0 || len(*events) != 0 {
			t.Fatalf("%s: refused callers caused %d outbound requests and %d events, want none", level, counter.n, len(*events))
		}
	}
}

// A caller the gateway admits reaches the route; its assertion is spent, so
// the same request again is refused.
func TestScenarioCaller_AdmitsAnAdmittedCallerOnce(t *testing.T) {
	pub, priv := callerKey(t)
	g, _, _ := scenarioCallerProvider(t, map[string]ed25519.PublicKey{"deploy-smoke": pub})
	now := g.cfg.Clock()
	r := signedScenario(t, "POST /scenario/reset", `{}`, "deploy-smoke", priv, now)
	header := r.Header.Get(scenarioauth.HeaderName)
	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("an admitted caller's reset: %d %s, want 200", rec.Code, rec.Body.String())
	}
	again := httptest.NewRequest(http.MethodPost, "/scenario/reset", strings.NewReader(`{}`))
	again.Header.Set(scenarioauth.HeaderName, header)
	rec = httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, again)
	wantUnauthorized(t, "the same assertion again", rec)

	// An originating route is reached too: whatever it answers, it is not the
	// caller refusal.
	rec = httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, signedScenario(t, "POST /scenario/uc03", `{}`, "deploy-smoke", priv, now))
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("an admitted caller's uc03 was refused: %s", rec.Body.String())
	}
}

// Every assertion that is not an admitted caller's, for this request, is
// refused: another audience, a label the gateway does not admit, a caller's
// label with another key, the provider's or a payer's own holder key under
// an admitted caller's label, an expired assertion, one bound to another
// query or body.
func TestScenarioCaller_RefusesEveryBadAssertion(t *testing.T) {
	pub, priv := callerKey(t)
	_, other := callerKey(t)
	g, counter, _ := scenarioCallerProvider(t, map[string]ed25519.PublicKey{"monitor": pub})
	now := g.cfg.Clock()
	route := "POST /scenario/reset"
	header := func(a shnsdk.Assertion) string {
		raw, err := json.Marshal(a)
		if err != nil {
			t.Fatal(err)
		}
		return base64.StdEncoding.EncodeToString(raw)
	}
	bound := scenarioauth.Binding(http.MethodPost, "/scenario/reset", []byte(`{}`))
	providerHolder := g.cfg.Identity.SignPriv
	_, payerHolder := callerKey(t)
	for name, h := range map[string]string{
		"the bare holder id":      header(shnsdk.IssueAssertionForBody("monitor", "provider", priv, now, scenarioauth.TTL, bound)),
		"the bare scenario":       header(shnsdk.IssueAssertionForBody("monitor", "scenario", priv, now, scenarioauth.TTL, bound)),
		"another gateway":         header(shnsdk.IssueAssertionForBody("monitor", scenarioauth.AudienceFor("provider-data"), priv, now, scenarioauth.TTL, bound)),
		"a label not admitted":    header(shnsdk.IssueAssertionForBody("phg", scenarioauth.AudienceFor("provider"), priv, now, scenarioauth.TTL, bound)),
		"another key":             header(shnsdk.IssueAssertionForBody("monitor", scenarioauth.AudienceFor("provider"), other, now, scenarioauth.TTL, bound)),
		"the provider holder key": header(shnsdk.IssueAssertionForBody("monitor", scenarioauth.AudienceFor("provider"), providerHolder, now, scenarioauth.TTL, bound)),
		"a payer holder key":      header(shnsdk.IssueAssertionForBody("monitor", scenarioauth.AudienceFor("provider"), payerHolder, now, scenarioauth.TTL, bound)),
		"expired":                 header(shnsdk.IssueAssertionForBody("monitor", scenarioauth.AudienceFor("provider"), priv, now.Add(-2*time.Minute), scenarioauth.TTL, bound)),
		"another query":           header(shnsdk.IssueAssertionForBody("monitor", scenarioauth.AudienceFor("provider"), priv, now, scenarioauth.TTL, scenarioauth.Binding(http.MethodPost, "/scenario/reset?x=1", []byte(`{}`)))),
		"another body":            header(shnsdk.IssueAssertionForBody("monitor", scenarioauth.AudienceFor("provider"), priv, now, scenarioauth.TTL, scenarioauth.Binding(http.MethodPost, "/scenario/reset", []byte(`{"x":1}`)))),
		"not an assertion":        "%%%",
	} {
		_, path, _ := strings.Cut(route, " ")
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		r.Header.Set(scenarioauth.HeaderName, h)
		rec := httptest.NewRecorder()
		g.Handler().ServeHTTP(rec, r)
		wantUnauthorized(t, name, rec)
	}
	if counter.n != 0 {
		t.Fatalf("refused callers caused %d outbound requests", counter.n)
	}
}

// A request refused for its body does not spend its assertion: the same
// assertion on the request it was minted for is admitted.
func TestScenarioCaller_ARefusedRequestDoesNotSpendItsAssertion(t *testing.T) {
	pub, priv := callerKey(t)
	g, _, _ := scenarioCallerProvider(t, map[string]ed25519.PublicKey{"monitor": pub})
	signed := signedScenario(t, "POST /scenario/reset", `{}`, "monitor", priv, g.cfg.Clock())
	h := signed.Header.Get(scenarioauth.HeaderName)

	wrong := httptest.NewRequest(http.MethodPost, "/scenario/reset", strings.NewReader(`{"x":1}`))
	wrong.Header.Set(scenarioauth.HeaderName, h)
	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, wrong)
	wantUnauthorized(t, "the assertion on another body", rec)

	right := httptest.NewRequest(http.MethodPost, "/scenario/reset", strings.NewReader(`{}`))
	right.Header.Set(scenarioauth.HeaderName, h)
	rec = httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, right)
	if rec.Code != http.StatusOK {
		t.Fatalf("the assertion on its own request after a refusal: %d %s, want 200", rec.Code, rec.Body.String())
	}
}

// failingReplay is a one-time-use record that cannot be consulted.
type failingReplay struct{}

func (failingReplay) CheckAndRecord(string, string, string, time.Time, time.Time) (bool, error) {
	return false, errors.New("database down")
}

// A one-time-use record that cannot be consulted refuses with 503: an
// unrecorded assertion is never admitted.
func TestScenarioCaller_StoreOutageRefuses(t *testing.T) {
	pub, priv := callerKey(t)
	g, _, _ := scenarioCallerProvider(t, map[string]ed25519.PublicKey{"monitor": pub}, func(c *Config) { c.Replay = failingReplay{} })
	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, signedScenario(t, "POST /scenario/reset", `{}`, "monitor", priv, g.cfg.Clock()))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("with the record down: %d %s, want 503", rec.Code, rec.Body.String())
	}
}

// An empty set admits no caller. No set at all (nil) leaves the routes
// unauthenticated, as they were, until every stack configures its callers
// (a temporary default that becomes refuse).
func TestScenarioCaller_EmptySetAdmitsNothingNilIsUnchanged(t *testing.T) {
	_, priv := callerKey(t)
	empty, _, _ := scenarioCallerProvider(t, map[string]ed25519.PublicKey{})
	rec := httptest.NewRecorder()
	empty.Handler().ServeHTTP(rec, signedScenario(t, "POST /scenario/reset", `{}`, "monitor", priv, empty.cfg.Clock()))
	wantUnauthorized(t, "an empty set", rec)

	none, _, _ := scenarioCallerProvider(t, nil)
	if rec := serve(none, "POST /scenario/reset", `{}`); rec.Code != http.StatusOK {
		t.Fatalf("with no callers configured: %d %s, want the route served as before", rec.Code, rec.Body.String())
	}
}

// The gateway's other routes are not the operator surface: the Da Vinci
// ingress serves its own clients whatever the /scenario callers.
func TestScenarioCaller_IngressIsNotAnOperatorRoute(t *testing.T) {
	pub, _ := callerKey(t)
	g, _, _, _ := unsetProfileProvider(t, true)
	cfg := g.cfg
	cfg.ScenarioCallers = map[string]ed25519.PublicKey{"monitor": pub}
	g = mustNew(t, cfg)
	if rec := serve(g, "GET /cds-services", ""); rec.Code != http.StatusOK {
		t.Fatalf("CDS discovery with /scenario callers configured: %d %s, want 200", rec.Code, rec.Body.String())
	}
}

// The one-time-use record is the gateway's shared one: an assertion spent at
// one replica is refused at another.
func TestScenarioCaller_ReplayRefusedAcrossReplicas(t *testing.T) {
	pub, priv := callerKey(t)
	shared := NewInMemoryReplayStore()
	a, _, _ := scenarioCallerProvider(t, map[string]ed25519.PublicKey{"phg": pub}, func(c *Config) { c.Replay = shared })
	b, _, _ := scenarioCallerProvider(t, map[string]ed25519.PublicKey{"phg": pub}, func(c *Config) { c.Replay = shared })
	r := signedScenario(t, "POST /scenario/reset", `{}`, "phg", priv, a.cfg.Clock())
	h := r.Header.Get(scenarioauth.HeaderName)
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("replica A: %d %s, want 200", rec.Code, rec.Body.String())
	}
	again := httptest.NewRequest(http.MethodPost, "/scenario/reset", strings.NewReader(`{}`))
	again.Header.Set(scenarioauth.HeaderName, h)
	rec = httptest.NewRecorder()
	b.Handler().ServeHTTP(rec, again)
	wantUnauthorized(t, "replica B, the assertion A spent", rec)
}

// An assertion names the gateway it is for: one minted for one gateway is
// refused at another that admits the same caller, and a gateway that names
// no holder id admits nothing.
func TestScenarioCaller_AnAssertionForAnotherGatewayIsRefused(t *testing.T) {
	pub, priv := callerKey(t)
	callers := map[string]ed25519.PublicKey{"deploy-smoke": pub}
	provider, _, _ := scenarioCallerProvider(t, callers)
	providerData, counter, _ := scenarioCallerProvider(t, callers, func(c *Config) { c.HolderID = "provider-data" })
	noHolder, _, _ := scenarioCallerProvider(t, callers, func(c *Config) { c.HolderID = "" })

	rec := httptest.NewRecorder()
	providerData.Handler().ServeHTTP(rec, signedScenarioFor(t, "provider-data", "POST /scenario/reset", `{}`, "deploy-smoke", priv, providerData.cfg.Clock()))
	if rec.Code != http.StatusOK {
		t.Fatalf("an assertion for provider-data at provider-data: %d %s, want 200", rec.Code, rec.Body.String())
	}
	before := counter.n
	rec = httptest.NewRecorder()
	providerData.Handler().ServeHTTP(rec, signedScenarioFor(t, "provider", "POST /scenario/reset", `{}`, "deploy-smoke", priv, providerData.cfg.Clock()))
	wantUnauthorized(t, "an assertion for provider at provider-data", rec)
	if counter.n != before {
		t.Fatalf("the refused request caused %d outbound requests", counter.n-before)
	}
	rec = httptest.NewRecorder()
	provider.Handler().ServeHTTP(rec, signedScenarioFor(t, "provider-data", "POST /scenario/reset", `{}`, "deploy-smoke", priv, provider.cfg.Clock()))
	wantUnauthorized(t, "an assertion for provider-data at provider", rec)
	rec = httptest.NewRecorder()
	noHolder.Handler().ServeHTTP(rec, signedScenarioFor(t, "provider", "POST /scenario/reset", `{}`, "deploy-smoke", priv, noHolder.cfg.Clock()))
	wantUnauthorized(t, "a gateway with no holder id", rec)
}

// The route an admitted caller reaches reads exactly the bytes the caller
// signed, with their length; a body over the request limit, and an assertion
// whose jti is too long to record, are refused before the route runs.
func TestScenarioCaller_TheRouteReadsTheSignedBody(t *testing.T) {
	pub, priv := callerKey(t)
	g, _, _ := scenarioCallerProvider(t, map[string]ed25519.PublicKey{"monitor": pub})
	var got []byte
	var gotLen int64
	reached := false
	route := g.requireScenarioCaller(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		got, _ = io.ReadAll(r.Body)
		gotLen = r.ContentLength
		w.WriteHeader(http.StatusOK)
	})
	body := `{"member":"MBR-1","answers":{"q1":"yes"}}`
	req := signedScenario(t, "POST /scenario/uc07/complete", body, "monitor", priv, g.cfg.Clock())
	req.ContentLength = -1 // sent chunked: the route learns the length from the guard
	rec := httptest.NewRecorder()
	route(rec, req)
	if rec.Code != http.StatusOK || string(got) != body || gotLen != int64(len(body)) {
		t.Fatalf("the route read %q (length %d, status %d), want the signed body %q", got, gotLen, rec.Code, body)
	}

	reached = false
	big := strings.Repeat("x", shnsdk.MaxRequestBytes+1)
	rec = httptest.NewRecorder()
	route(rec, signedScenario(t, "POST /scenario/uc07/complete", big, "monitor", priv, g.cfg.Clock()))
	wantUnauthorized(t, "a body over the request limit", rec)
	if reached {
		t.Fatal("a body over the request limit reached the route")
	}

	a := shnsdk.IssueAssertionForBody("monitor", scenarioauth.AudienceFor("provider"), priv, g.cfg.Clock(), scenarioauth.TTL, scenarioauth.Binding(http.MethodPost, "/scenario/reset", []byte(`{}`)))
	a.JTI = strings.Repeat("j", MaxReplayKeyBytes+1)
	a.Sig = nil
	payload, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	a.Sig = ed25519.Sign(priv, payload)
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/scenario/reset", strings.NewReader(`{}`))
	r.Header.Set(scenarioauth.HeaderName, base64.StdEncoding.EncodeToString(raw))
	rec = httptest.NewRecorder()
	route(rec, r)
	wantUnauthorized(t, "a jti too long to record", rec)
	if reached {
		t.Fatal("a jti too long to record reached the route")
	}
}

// The caller is refused before anything else a route checks: on a gateway
// with no origination profile, an unsigned request to an originating route
// is refused 401 as an unauthorized caller, not 503 for the profile, and
// sends nothing.
func TestScenarioCaller_RefusedBeforeTheProfileCheck(t *testing.T) {
	pub, _ := callerKey(t)
	g, counter, events := scenarioCallerProvider(t, map[string]ed25519.PublicKey{"monitor": pub}, func(c *Config) { c.OriginationProfile = "" })
	body := testRouteBody(g, "/scenario/uc03", "") // reads the fixture's order: before the count
	*events = nil
	wantUnauthorized(t, "an unsigned uc03 with no profile", serve(g, "POST /scenario/uc03", body))
	if counter.n != 0 || len(*events) != 0 {
		t.Fatalf("the refused caller caused %d outbound requests and %d events, want none", counter.n, len(*events))
	}
}
