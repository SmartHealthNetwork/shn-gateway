package scenarioauth

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

var t0 = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

func testKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

// decode reads the assertion a request carries.
func decode(t *testing.T, h http.Header) shnsdk.Assertion {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(h.Get(HeaderName))
	if err != nil {
		t.Fatalf("header is not base64: %v", err)
	}
	var a shnsdk.Assertion
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatalf("header is not an assertion: %v", err)
	}
	return a
}

// A signed request carries an assertion by the caller's label, for the
// gateway it is sent to, valid for TTL, verifiable with the caller's public key
// and bound to its method, target and body; the body still reaches the
// server.
func TestSign_BindsTheRequest(t *testing.T) {
	pub, priv := testKey(t)
	var got shnsdk.Assertion
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = decode(t, r.Header)
		gotBody, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()

	body := `{"scenario":"uc01"}`
	c := Client(&http.Client{}, "monitor", "provider", priv, func() time.Time { return t0 })
	resp, err := c.Post(srv.URL+"/scenario/uc01?wait=30", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if string(gotBody) != body {
		t.Fatalf("the server received %q, want the body as sent", gotBody)
	}
	if err := shnsdk.VerifyAssertion(got, AudienceFor("provider"), pub, t0); err != nil {
		t.Fatalf("the assertion does not verify: %v", err)
	}
	if got.HolderID != "monitor" || got.Audience != "scenario:provider" || got.Expiry.Sub(got.IssuedAt) != TTL {
		t.Fatalf("assertion %+v: want holder monitor, audience scenario:provider, TTL %v", got, TTL)
	}
	if want := BindingHash(http.MethodPost, "/scenario/uc01?wait=30", []byte(body)); got.BodyHash != want {
		t.Fatalf("bh %q, want %q", got.BodyHash, want)
	}
}

// The binding changes with the method, the path, the query and the body, so
// an assertion for one request is no assertion for another.
func TestBinding_CoversMethodTargetQueryAndBody(t *testing.T) {
	base := BindingHash("POST", "/scenario/uc01?wait=30", []byte(`{}`))
	for name, h := range map[string]string{
		"method": BindingHash("GET", "/scenario/uc01?wait=30", []byte(`{}`)),
		"path":   BindingHash("POST", "/scenario/uc03?wait=30", []byte(`{}`)),
		"query":  BindingHash("POST", "/scenario/uc01?wait=0", []byte(`{}`)),
		"body":   BindingHash("POST", "/scenario/uc01?wait=30", []byte(`{"x":1}`)),
		// The newline keeps the target and the body apart.
		"split": BindingHash("POST", "/scenario/uc01?wait=30\n{", []byte(`}`)),
	} {
		if h == base {
			t.Errorf("a different %s binds the same", name)
		}
	}
}

// Only /scenario requests are signed: the same client carries a gateway's
// other routes as they are.
func TestTransport_SignsOnlyScenarioRoutes(t *testing.T) {
	_, priv := testKey(t)
	signed := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		signed[r.URL.Path] = r.Header.Get(HeaderName) != ""
	}))
	defer srv.Close()
	c := Client(nil, "deploy-smoke", "provider", priv, nil)
	for _, p := range []string{"/scenario/uc07/pending", "/health", "/scenario", "/scenarios/x", "/metadata"} {
		resp, err := c.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	want := map[string]bool{"/scenario/uc07/pending": true, "/health": false, "/scenario": false, "/scenarios/x": false, "/metadata": false}
	for p, w := range want {
		if signed[p] != w {
			t.Errorf("%s signed=%v, want %v", p, signed[p], w)
		}
	}
}

// Each request mints its own assertion: two sends never share a jti.
func TestTransport_FreshAssertionPerRequest(t *testing.T) {
	_, priv := testKey(t)
	var jtis []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jtis = append(jtis, decode(t, r.Header).JTI)
	}))
	defer srv.Close()
	c := Client(nil, "phg", "provider", priv, nil)
	for range 2 {
		resp, err := c.Post(srv.URL+"/scenario/uc07/complete", "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if len(jtis) != 2 || jtis[0] == "" || jtis[0] == jtis[1] {
		t.Fatalf("jtis %v, want two distinct", jtis)
	}
}

// A caller's key comes from KeyEnv, else from the file KeyFileEnv names, and
// its gateway from GatewayEnv; with no key the client is unchanged; a
// malformed key, or a key with no gateway, is an error, never an unsigned
// client that looks configured.
func TestKeyFromEnv(t *testing.T) {
	_, priv := testKey(t)
	b64 := base64.StdEncoding.EncodeToString(priv)
	path := filepath.Join(t.TempDir(), "scenario-caller-monitor.sign")
	if err := os.WriteFile(path, []byte(b64+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }

	if k, err := KeyFromEnv(env(map[string]string{KeyEnv: b64, KeyFileEnv: "/nonexistent"})); err != nil || !k.Equal(priv) {
		t.Fatalf("KeyEnv: %v %v", k, err)
	}
	if k, err := KeyFromEnv(env(map[string]string{KeyFileEnv: path})); err != nil || !k.Equal(priv) {
		t.Fatalf("KeyFileEnv: %v %v", k, err)
	}
	if k, err := KeyFromEnv(env(nil)); err != nil || k != nil {
		t.Fatalf("unset: %v %v, want no key and no error", k, err)
	}
	for name, kv := range map[string]map[string]string{
		"not base64":   {KeyEnv: "%%%"},
		"short key":    {KeyEnv: base64.StdEncoding.EncodeToString([]byte("short"))},
		"missing file": {KeyFileEnv: filepath.Join(t.TempDir(), "absent")},
	} {
		if _, err := KeyFromEnv(env(kv)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	base := &http.Client{Timeout: time.Second}
	if c, err := ClientFromEnv(base, "monitor", env(nil)); err != nil || c != base {
		t.Fatal("with no key, ClientFromEnv must return the client as it is")
	}
	c, err := ClientFromEnv(base, "monitor", env(map[string]string{KeyEnv: b64, GatewayEnv: "provider"}))
	if err != nil {
		t.Fatal(err)
	}
	if tr, ok := c.Transport.(*Transport); !ok || tr.Label != "monitor" || tr.Gateway != "provider" || c.Timeout != time.Second {
		t.Fatalf("ClientFromEnv = %+v, want a copy of the client signing as monitor for provider", c)
	}
	if _, err := ClientFromEnv(base, "monitor", env(map[string]string{KeyEnv: "%%%", GatewayEnv: "provider"})); err == nil {
		t.Fatal("a malformed key must be an error")
	}
	// A key with no gateway to sign for is ErrNoGateway, and the client is
	// returned as it is: unsigned, never signing for a guessed gateway.
	for name, kv := range map[string]map[string]string{
		"no gateway":    {KeyEnv: b64},
		"a blank one":   {KeyEnv: b64, GatewayEnv: "  "},
		"key file only": {KeyFileEnv: path},
	} {
		c, err := ClientFromEnv(base, "monitor", env(kv))
		if !errors.Is(err, ErrNoGateway) || c != base {
			t.Errorf("%s: %v, %v; want ErrNoGateway and the client as it is", name, c, err)
		}
	}
	if !strings.Contains(ErrNoGateway.Error(), GatewayEnv) {
		t.Errorf("ErrNoGateway %q does not name %s", ErrNoGateway, GatewayEnv)
	}
}

// Signing without a key, or without a gateway to sign for, fails rather than
// sending a request unsigned or for no gateway.
func TestSign_RefusesWithoutAKeyOrAGateway(t *testing.T) {
	_, priv := testKey(t)
	req := httptest.NewRequest(http.MethodPost, "/scenario/uc01", strings.NewReader(`{}`))
	if err := Sign(req, "monitor", "provider", nil, t0); err == nil {
		t.Fatal("signing with no key must fail")
	}
	if err := Sign(req, "monitor", "", priv, t0); err == nil || req.Header.Get(HeaderName) != "" {
		t.Fatalf("signing for no gateway: %v, header %q; want an error and no header", err, req.Header.Get(HeaderName))
	}
}

// The audience names the gateway, apart from its holder id.
func TestAudienceFor(t *testing.T) {
	if got := AudienceFor("provider"); got != "scenario:provider" {
		t.Fatalf("AudienceFor(provider) = %q", got)
	}
}

// headerFor is the header value an assertion encodes to.
func headerFor(t *testing.T, a shnsdk.Assertion) string {
	t.Helper()
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// Verify admits exactly a request signed by an admitted caller, for this
// gateway, short-lived, and bound to that request. Every other assertion is
// refused: missing or garbled, for another gateway or for none, the bare
// holder id or the bare scenario audience, an unknown or participant label, a
// key that is not the label's, expired, long-lived, issued in the future,
// bound to another method, target, query or body. An empty set, or a gateway
// with no holder id, admits nothing.
func TestVerify(t *testing.T) {
	pub, priv := testKey(t)
	otherPub, otherPriv := testKey(t)
	callers := map[string]ed25519.PublicKey{"monitor": pub, "phg": otherPub}
	method, uri, body := http.MethodPost, "/scenario/uc01?wait=30", []byte(`{"scenario":"uc01"}`)
	bound := Binding(method, uri, body)
	issue := func(label, aud string, k ed25519.PrivateKey, at time.Time, ttl time.Duration, b []byte) string {
		return headerFor(t, shnsdk.IssueAssertionForBody(label, aud, k, at, ttl, b))
	}

	aud := AudienceFor("provider")
	ok := issue("monitor", aud, priv, t0, TTL, bound)
	c, err := Verify(ok, method, uri, body, "provider", callers, t0.Add(10*time.Second))
	if err != nil || c.Label != "monitor" || c.JTI == "" || !c.Expiry.Equal(t0.Add(TTL)) {
		t.Fatalf("a well-formed request: %+v %v, want admitted as monitor", c, err)
	}

	for name, h := range map[string]string{
		"missing":                "",
		"not base64":             "%%%",
		"not an assertion":       base64.StdEncoding.EncodeToString([]byte(`[1,2]`)),
		"the bare holder id":     issue("monitor", "provider", priv, t0, TTL, bound),
		"the bare scenario":      issue("monitor", "scenario", priv, t0, TTL, bound),
		"another gateway":        issue("monitor", AudienceFor("provider-data"), priv, t0, TTL, bound),
		"no gateway":             issue("monitor", AudienceFor(""), priv, t0, TTL, bound),
		"a gateway's prefix":     issue("monitor", AudienceFor("provider-"), priv, t0, TTL, bound),
		"unknown label":          issue("provider", aud, priv, t0, TTL, bound),
		"another caller's key":   issue("monitor", aud, otherPriv, t0, TTL, bound),
		"expired":                issue("monitor", aud, priv, t0.Add(-2*time.Minute), TTL, bound),
		"longer than TTL":        issue("monitor", aud, priv, t0, 2*TTL, bound),
		"issued in the future":   issue("monitor", aud, priv, t0.Add(10*time.Minute), TTL, bound),
		"issued a minute ahead":  issue("monitor", aud, priv, t0.Add(10*time.Second+time.Minute), TTL, bound),
		"bound to another body":  issue("monitor", aud, priv, t0, TTL, Binding(method, uri, []byte(`{}`))),
		"bound to another query": issue("monitor", aud, priv, t0, TTL, Binding(method, "/scenario/uc01?wait=0", body)),
		"bound to another route": issue("monitor", aud, priv, t0, TTL, Binding(method, "/scenario/uc03?wait=30", body)),
		"bound to another verb":  issue("monitor", aud, priv, t0, TTL, Binding(http.MethodGet, uri, body)),
		"bound to nothing":       headerFor(t, shnsdk.IssueAssertion("monitor", aud, priv, t0, TTL)),
	} {
		if c, err := Verify(h, method, uri, body, "provider", callers, t0.Add(10*time.Second)); err == nil {
			t.Errorf("%s: admitted as %+v", name, c)
		}
	}
	if _, err := Verify(ok, method, uri, body, "provider", nil, t0); err == nil {
		t.Error("an empty callers set admitted a request")
	}
	// The assertion for provider is no assertion at any other gateway, nor at
	// a gateway that names no holder id.
	for _, gw := range []string{"provider-data", "", "scenario:provider", "provider "} {
		if c, err := Verify(ok, method, uri, body, gw, callers, t0.Add(10*time.Second)); err == nil {
			t.Errorf("an assertion for provider admitted at %q as %+v", gw, c)
		}
	}
	// An admitted caller whose public key is malformed (a truncated entry in
	// a callers file) refuses; it never reaches the signature check.
	if _, err := Verify(ok, method, uri, body, "provider", map[string]ed25519.PublicKey{"monitor": pub[:16]}, t0.Add(10*time.Second)); err == nil {
		t.Error("a malformed admitted key admitted a request")
	}
}

// The client follows no redirect to a /scenario route, so it never signs a
// request its caller did not address: here a redirect to another gateway's
// /scenario/reset, and one between two /scenario routes.
func TestClient_FollowsNoRedirectToAScenarioRoute(t *testing.T) {
	_, priv := testKey(t)
	var reached []string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = append(reached, r.URL.Path+" "+r.Header.Get(HeaderName))
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/scenario/reset", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	c := Client(nil, "phg", "provider", priv, nil)
	for _, path := range []string{"/keys/abc", "/scenario/uc07"} {
		resp, err := c.Post(redirector.URL+path, "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusTemporaryRedirect {
			t.Errorf("%s: status %d, want the redirect itself", path, resp.StatusCode)
		}
	}
	if len(reached) != 0 {
		t.Fatalf("the client followed a redirect to a /scenario route: %v", reached)
	}
	// A redirect to any other path is still followed.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/keys/abc", http.StatusFound)
	}))
	defer other.Close()
	resp, err := c.Get(other.URL + "/keys/old")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(reached) != 1 || reached[0] != "/keys/abc " {
		t.Fatalf("a redirect to another path: reached %v, want it followed, unsigned", reached)
	}

	// The caller's own redirect policy still decides every other redirect.
	reached = nil
	never := Client(&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, "phg", "provider", priv, nil)
	resp, err = never.Get(other.URL + "/keys/old")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || len(reached) != 0 {
		t.Fatalf("a caller that follows no redirect: status %d, reached %v, want the redirect itself", resp.StatusCode, reached)
	}

	// With no policy of its own, a redirect loop stops after 10 hops.
	hops := 0
	var loop *httptest.Server
	loop = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops++
		http.Redirect(w, r, loop.URL+"/keys/again", http.StatusFound)
	}))
	defer loop.Close()
	if _, err := c.Get(loop.URL + "/keys/again"); err == nil || hops != 10 {
		t.Fatalf("a redirect loop: %d hops, %v; want stopped after 10", hops, err)
	}
}

// A request the client transport signs is one Verify admits.
func TestVerify_AdmitsWhatTheTransportSigns(t *testing.T) {
	pub, priv := testKey(t)
	var err error
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, err = Verify(r.Header.Get(HeaderName), r.Method, r.URL.RequestURI(), body, "provider", map[string]ed25519.PublicKey{"phg": pub}, time.Now())
	}))
	defer srv.Close()
	resp, perr := Client(nil, "phg", "provider", priv, nil).Post(srv.URL+"/scenario/uc07/complete?x=1", "application/json", strings.NewReader(`{"a":1}`))
	if perr != nil {
		t.Fatal(perr)
	}
	resp.Body.Close()
	if err != nil {
		t.Fatalf("Verify refused what the transport signed: %v", err)
	}
}

// The startup line says whether a client signs, as whom and for which
// gateway.
func TestStartupLine(t *testing.T) {
	_, priv := testKey(t)
	if got := StartupLine(Client(nil, "monitor", "provider", priv, nil)); got != "signing /scenario requests as monitor for provider" {
		t.Errorf("signed: %q", got)
	}
	for name, c := range map[string]*http.Client{"nil": nil, "plain": {}, "no key": Client(nil, "monitor", "provider", nil, nil), "no gateway": Client(nil, "monitor", "", priv, nil)} {
		if got := StartupLine(c); !strings.HasPrefix(got, "WARNING: /scenario requests go unsigned") {
			t.Errorf("%s: %q, want the unsigned warning", name, got)
		}
	}
}

// A /scenario request is never resent with an assertion the gateway may have
// spent. Go's transport retries an idempotent request on its own when a
// reused connection drops before the answer, below the signing RoundTrip, so
// the retry would carry the same assertion, spent by the first attempt, and
// be refused as a replay. The gateway here answers the first GET, keeping the
// connection alive, then spends the second's jti and drops the connection
// without answering: no request may arrive again with that jti.
func TestTransport_NeverResendsASpentAssertion(t *testing.T) {
	pub, priv := testKey(t)
	callers := map[string]ed25519.PublicKey{"phg": pub}
	var mu sync.Mutex
	spent := map[string]bool{}
	var requests, replays int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Verify(r.Header.Get(HeaderName), r.Method, r.URL.RequestURI(), nil, "provider", callers, time.Now())
		mu.Lock()
		requests++
		n := requests
		replayed := err == nil && spent[c.JTI]
		if replayed {
			replays++
		}
		if err == nil {
			spent[c.JTI] = true
		}
		mu.Unlock()
		// As a gateway does: a spent assertion is refused as a replay.
		if err != nil || replayed {
			http.Error(w, `{"error":"unauthorized caller"}`, http.StatusUnauthorized)
			return
		}
		if n == 2 {
			// The answer is lost: the connection drops after the gateway
			// has spent the assertion.
			conn, _, herr := w.(http.Hijacker).Hijack()
			if herr == nil {
				_ = conn.Close()
			}
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := Client(&http.Client{Timeout: 5 * time.Second}, "phg", "provider", priv, nil)
	resp, err := c.Get(srv.URL + "/scenario/uc07/pending?patient=PCI-1")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the first request: %d, want admitted", resp.StatusCode)
	}
	if resp, err := c.Get(srv.URL + "/scenario/uc07/pending?patient=PCI-1"); err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			t.Error("the dropped request came back 401: it was resent with its spent assertion")
		}
	}
	mu.Lock()
	n := replays
	mu.Unlock()
	if n != 0 {
		t.Fatalf("%d request(s) arrived again with an assertion already spent", n)
	}
	// Each request still goes out signed, and the next one is admitted.
	resp, err = c.Get(srv.URL + "/scenario/uc07/pending?patient=PCI-1")
	if err == nil {
		resp.Body.Close()
	}
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("a request after the dropped one: %v %v, want admitted", resp, err)
	}
}

// Signed requests each open a connection of their own; the client's other
// requests keep reusing the base transport's pooled connections.
func TestTransport_OnlySignedRequestsSkipConnectionReuse(t *testing.T) {
	_, priv := testKey(t)
	var mu sync.Mutex
	conns := 0
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			mu.Lock()
			conns++
			mu.Unlock()
		}
	}
	srv.Start()
	defer srv.Close()
	c := Client(&http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}, "phg", "provider", priv, nil)
	newConns := func(path string) int {
		t.Helper()
		mu.Lock()
		before := conns
		mu.Unlock()
		for range 3 {
			resp, err := c.Get(srv.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		mu.Lock()
		defer mu.Unlock()
		return conns - before
	}
	if n := newConns("/health"); n != 1 {
		t.Errorf("three unsigned requests opened %d connections, want one reused", n)
	}
	if n := newConns("/scenario/uc07/pending"); n != 3 {
		t.Errorf("three signed requests opened %d connections, want one each", n)
	}
}

// ParseCallers reads a callers file into the set Verify takes, and refuses
// one the gateway could not boot with: not an array, an entry with no label,
// a label listed twice, a key that is not a base64 ed25519 public key.
func TestParseCallers(t *testing.T) {
	pub, _ := testKey(t)
	k := base64.StdEncoding.EncodeToString(pub)
	got, err := ParseCallers([]byte(`[{"label":"monitor","public_key":"` + k + `"},{"label":"phg","public_key":"` + k + `"}]`))
	if err != nil || len(got) != 2 || !got["monitor"].Equal(pub) {
		t.Fatalf("two callers: %v %v", got, err)
	}
	if got, err := ParseCallers([]byte(`[]`)); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("an empty array: %v %v, want an empty set", got, err)
	}
	for name, raw := range map[string]string{
		"not an array":  `{"label":"monitor"}`,
		"no label":      `[{"public_key":"` + k + `"}]`,
		"a label twice": `[{"label":"monitor","public_key":"` + k + `"},{"label":"monitor","public_key":"` + k + `"}]`,
		"not base64":    `[{"label":"monitor","public_key":"%%%"}]`,
		"a short key":   `[{"label":"monitor","public_key":"` + base64.StdEncoding.EncodeToString(pub[:16]) + `"}]`,
		"another field": `[{"label":"monitor","key":"` + k + `"}]`,
	} {
		if _, err := ParseCallers([]byte(raw)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}
