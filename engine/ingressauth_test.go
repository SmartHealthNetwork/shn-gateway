package engine

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"github.com/golang-jwt/jwt/v5"
)

const testIngressBaseURL = "https://shn-ingress.test"

// ingressFixedClock returns a deterministic clock at the P5 harness epoch.
func ingressFixedClock() func() time.Time {
	t := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

// newTestClientKey returns an ES384 private key and its PEM SubjectPublicKeyInfo.
func newTestClientKey(t *testing.T) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatalf("marshal pub: %v", err)
	}
	return k, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

// newTestRSAKey returns a throwaway RSA key for the alg-confusion rows (no
// registration — it only has to produce a well-formed RS384 signature).
func newTestRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// mintAssertion builds a private_key_jwt client_assertion with the given claims.
func mintAssertion(t *testing.T, key *ecdsa.PrivateKey, method jwt.SigningMethod, claims jwt.MapClaims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(method, claims).SignedString(key)
	if err != nil {
		t.Fatalf("sign assertion: %v", err)
	}
	return s
}

// postToken posts a client_assertion to the token endpoint and returns the response.
func postToken(t *testing.T, h http.Handler, assertion string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{
		"grant_type":            {"client_credentials"},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {assertion},
	}
	req := httptest.NewRequest(http.MethodPost, testIngressBaseURL+"/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// postTokenScope is postToken with an explicit `scope` form field — the parameter the
// endpoint validates against the client's registration.
func postTokenScope(t *testing.T, h http.Handler, assertion, scope string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{
		"grant_type":            {"client_credentials"},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {assertion},
		"scope":                 {scope},
	}
	req := httptest.NewRequest(http.MethodPost, testIngressBaseURL+"/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func newTestAuthServer(t *testing.T, clientID string, pubPEM []byte, alg string) *ingressAuthServer {
	t.Helper()
	keys, err := newEphemeralKeyStore()
	if err != nil {
		t.Fatalf("newEphemeralKeyStore: %v", err)
	}
	s, err := newIngressAuthServer(testIngressBaseURL,
		map[string]IngressClientRegistration{clientID: {Alg: alg, PublicKeyPEM: pubPEM, Scopes: []string{ingressScope}}},
		ingressFixedClock(), keys, NewInMemoryReplayStore())
	if err != nil {
		t.Fatalf("newIngressAuthServer: %v", err)
	}
	return s
}

func validClaims(clientID, tokenURL string, now time.Time) jwt.MapClaims {
	return jwt.MapClaims{
		"iss": clientID, "sub": clientID, "aud": tokenURL,
		"jti": "jti-" + now.Format("150405.000000"),
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
	}
}

func TestIngressTokenEndpoint_IssuesBearer(t *testing.T) {
	key, pub := newTestClientKey(t)
	s := newTestAuthServer(t, "br-provider", pub, "ES384")
	now := ingressFixedClock()()
	a := mintAssertion(t, key, jwt.SigningMethodES384, validClaims("br-provider", s.tokenURL(), now))
	w := postToken(t, http.HandlerFunc(s.handleToken), a)
	if w.Code != http.StatusOK {
		t.Fatalf("token status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Scope       string `json:"scope"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &tr); err != nil {
		t.Fatalf("decode token: %v", err)
	}
	if tr.AccessToken == "" || strings.ToLower(tr.TokenType) != "bearer" {
		t.Fatalf("bad token response: %+v", tr)
	}
	// Fix 3: parse the issued bearer and assert its claims — pins the shape that
	// verifyBearer depends on (aud == baseURL, client_id, scope).
	clock := ingressFixedClock()
	_, signKey, err := s.keys.SigningKey(clock())
	if err != nil {
		t.Fatalf("signing key: %v", err)
	}
	var bearerClaims jwt.MapClaims
	_, err = jwt.NewParser(
		jwt.WithValidMethods([]string{"ES384"}),
		jwt.WithTimeFunc(clock),
	).ParseWithClaims(tr.AccessToken, &bearerClaims, func(*jwt.Token) (any, error) {
		return signKey.Public(), nil
	})
	if err != nil {
		t.Fatalf("parse bearer: %v", err)
	}
	if aud, _ := bearerClaims["aud"].(string); aud != s.baseURL {
		t.Errorf("bearer aud = %q, want %q", aud, s.baseURL)
	}
	if cid, _ := bearerClaims["client_id"].(string); cid != "br-provider" {
		t.Errorf("bearer client_id = %q, want %q", cid, "br-provider")
	}
	// scope in the bearer matches what was issued (empty string when not requested)
	if bearerClaims["scope"] != tr.Scope {
		t.Errorf("bearer scope = %v, want %q", bearerClaims["scope"], tr.Scope)
	}
	// Cache headers: RFC 6749
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

func TestIngressTokenEndpoint_RejectionRows(t *testing.T) {
	key, pub := newTestClientKey(t)
	wrongKey, _ := newTestClientKey(t)
	s := newTestAuthServer(t, "br-provider", pub, "ES384")
	now := ingressFixedClock()()
	tok := s.tokenURL()

	// alg-confusion: a token whose header alg != the registered ES384 must be
	// rejected by WithValidMethods (the classic JWT attack; here HS384). None of
	// these rows reach jti recording (they fail at client/sig/aud/sub first), so
	// the shared fixed-clock jti does not cross-contaminate.
	wrongAlg, err := jwt.NewWithClaims(jwt.SigningMethodHS384, validClaims("br-provider", tok, now)).SignedString([]byte("not-the-registered-key"))
	if err != nil {
		t.Fatalf("mint HS384: %v", err)
	}

	rows := []struct {
		name      string
		assertion string
	}{
		{"unknown client", mintAssertion(t, key, jwt.SigningMethodES384, validClaims("stranger", tok, now))},
		{"wrong key", mintAssertion(t, wrongKey, jwt.SigningMethodES384, validClaims("br-provider", tok, now))},
		{"wrong alg (HS384)", wrongAlg},
		{"wrong aud", mintAssertion(t, key, jwt.SigningMethodES384, validClaims("br-provider", "https://evil/oauth/token", now))},
		{"expired", mintAssertion(t, key, jwt.SigningMethodES384, jwt.MapClaims{
			"iss": "br-provider", "sub": "br-provider", "aud": tok, "jti": "exp1",
			"iat": now.Add(-10 * time.Minute).Unix(), "exp": now.Add(-5 * time.Minute).Unix()})},
		{"iss != sub", mintAssertion(t, key, jwt.SigningMethodES384, jwt.MapClaims{
			"iss": "br-provider", "sub": "someone-else", "aud": tok, "jti": "mismatch",
			"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix()})},
		// Fix 1: assertion exp > maxAssertionLifetime past now must be rejected so a
		// long-lived assertion can't be replayed after the jti window evicts its jti.
		{"exp too far out (1h)", mintAssertion(t, key, jwt.SigningMethodES384, jwt.MapClaims{
			"iss": "br-provider", "sub": "br-provider", "aud": tok, "jti": "long-exp-1",
			"iat": now.Unix(), "exp": now.Add(1 * time.Hour).Unix()})},
	}
	for _, r := range rows {
		w := postToken(t, http.HandlerFunc(s.handleToken), r.assertion)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", r.name, w.Code)
		}
		if strings.Contains(strings.ToLower(w.Body.String()), "crypto") || strings.Contains(w.Body.String(), "ecdsa") {
			t.Errorf("%s: response leaks internal jwt error: %s", r.name, w.Body.String())
		}
	}
}

func TestIngressTokenEndpoint_ReplayedJTIRejected(t *testing.T) {
	key, pub := newTestClientKey(t)
	s := newTestAuthServer(t, "br-provider", pub, "ES384")
	now := ingressFixedClock()()
	a := mintAssertion(t, key, jwt.SigningMethodES384, validClaims("br-provider", s.tokenURL(), now))
	if w := postToken(t, http.HandlerFunc(s.handleToken), a); w.Code != http.StatusOK {
		t.Fatalf("first use status = %d, want 200", w.Code)
	}
	if w := postToken(t, http.HandlerFunc(s.handleToken), a); w.Code != http.StatusUnauthorized {
		t.Errorf("replayed jti status = %d, want 401", w.Code)
	}
}

// newTestRSAClientKey returns an RSA-2048 private key + its PEM SubjectPublicKeyInfo.
func newTestRSAClientKey(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa genkey: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatalf("marshal rsa pub: %v", err)
	}
	return k, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

// TestIngressTokenEndpoint_RS384Client exercises the RS384 registration branch
// (ParseRSAPublicKeyFromPEM + RS384 alg-pinning) that .well-known/smart-configuration
// advertises as supported.
func TestIngressTokenEndpoint_RS384Client(t *testing.T) {
	key, pub := newTestRSAClientKey(t)
	s := newTestAuthServer(t, "rs-client", pub, "RS384")
	now := ingressFixedClock()()
	a, err := jwt.NewWithClaims(jwt.SigningMethodRS384, validClaims("rs-client", s.tokenURL(), now)).SignedString(key)
	if err != nil {
		t.Fatalf("sign RS384: %v", err)
	}
	if w := postToken(t, http.HandlerFunc(s.handleToken), a); w.Code != http.StatusOK {
		t.Fatalf("RS384 token status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// gatewayWithAuth builds a real-auth ingress Gateway (no bypass) with one registered
// client. Mirrors the canonical engine construction (originate_test.go:275): New()
// panics without SoR/Store AND Identity.SignPriv (gateway.go:169), so set all three.
func gatewayWithAuth(t *testing.T, clientID string, pubPEM []byte) *Gateway {
	t.Helper()
	_, signPriv := genED25519(t) // existing helper: (ed25519.PublicKey, ed25519.PrivateKey)
	sor := newCensusSoR()        // existing engine stub; satisfies both SoR and Store
	g := mustNew(t, Config{
		Role:           "provider",
		HolderID:       "provider",
		PayerRouter:    payerRouterFor(t, "payer"),
		Identity:       shnsdk.Identity{HolderID: "provider", SignPriv: signPriv}, // required
		IngressEnabled: true,
		IngressBaseURL: testIngressBaseURL,
		IngressClients: map[string]IngressClientRegistration{
			clientID: {Alg: "ES384", PublicKeyPEM: pubPEM, Scopes: []string{ingressScope}},
		},
		Reg:       shnsdk.NewRegistry(),
		Validator: shnsdk.NewFakeValidator(),
		SoR:       sor,
		Store:     sor,
		Clock:     ingressFixedClock(),
		HubURL:    "http://hub.test",
	})
	return g
}

func TestIngressBearer_AcceptedAndRejected(t *testing.T) {
	priv, pub := newTestClientKey(t)
	g := gatewayWithAuth(t, "br-provider", pub)
	if g.ingressAuth == nil {
		t.Fatal("ingressAuth not constructed for a real-auth ingress gateway")
	}
	now := ingressFixedClock()()
	// The two "signed with the server's own key" rows below come from the KEY STORE
	// (there is no process-local bearerKey any more) and carry the store's kid, so
	// each row still isolates the claim it names — without a kid verifyBearer would
	// reject at the pre-check and mask the aud/expiry binding under test.
	serverKID, serverKey, err := g.ingressAuth.keys.SigningKey(now)
	if err != nil {
		t.Fatalf("signing key: %v", err)
	}

	// Fix I1: drive the full /oauth/token route through g.Handler() — proves the
	// route is actually mounted (calling issueBearer directly is a near-tautology).
	assertion := mintAssertion(t, priv, jwt.SigningMethodES384, validClaims("br-provider", g.ingressAuth.tokenURL(), now))
	form := url.Values{
		"grant_type":            {"client_credentials"},
		"client_assertion_type": {assertionType},
		"client_assertion":      {assertion},
	}
	req := httptest.NewRequest(http.MethodPost, testIngressBaseURL+"/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("token roundtrip status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var tr struct {
		AccessToken string `json:"access_token"`
	}
	if err = json.Unmarshal(rec.Body.Bytes(), &tr); err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	bearer := tr.AccessToken

	withBearer := func(b string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, testIngressBaseURL+"/cds-services", nil)
		if b != "" {
			req.Header.Set("Authorization", "Bearer "+b)
		}
		return req
	}
	if !authOK(g, withBearer(bearer)) {
		t.Error("valid bearer rejected by ingressAuthOK")
	}
	for _, tc := range []struct {
		name, hdr string
	}{
		{"no bearer", ""},
		{"garbage", "not-a-jwt"},
		// Signed with the SERVER's own bearer key so the token passes signature
		// verification — this isolates the aud == baseURL binding in verifyBearer
		// (using the client key would reject on signature before aud is checked).
		{"wrong aud", mintAssertionKID(t, serverKey, serverKID, jwt.MapClaims{
			"client_id": "br-provider", "aud": "https://evil", "iat": now.Unix(), "exp": now.Add(time.Minute).Unix()})},
		// Fix M2: expired bearer — passes signature (server's own key) but fails
		// WithExpirationRequired()/expiry guard in verifyBearer.
		{"expired bearer", mintAssertionKID(t, serverKey, serverKID, jwt.MapClaims{
			"client_id": "br-provider", "aud": g.ingressAuth.baseURL,
			"iat": now.Add(-10 * time.Minute).Unix(), "exp": now.Add(-time.Minute).Unix()})},
	} {
		if authOK(g, withBearer(tc.hdr)) {
			t.Errorf("%s: ingressAuthOK returned true, want false", tc.name)
		}
	}
}

// Pin the carry-forward: a zero-value Gateway (nil ingressAuth, no bypass) must
// fail closed WITHOUT panicking.
func TestIngressAuthOK_NilServerFailsClosedNoPanic(t *testing.T) {
	g := &Gateway{}
	req := httptest.NewRequest(http.MethodPost, "/cds-services/x", nil)
	if authOK(g, req) {
		t.Fatal("nil ingressAuth + no bypass: ingressAuthOK = true, want false")
	}
}

// TestProviderIngressMetadata: the ingress edge publishes its per-role
// CapabilityStatement (FR-37) — present with ingress enabled, absent (404)
// without, like every other Da Vinci-facing route.
func TestProviderIngressMetadata(t *testing.T) {
	_, pub := newTestClientKey(t)
	g := gatewayWithAuth(t, "br-provider", pub)
	rr := httptest.NewRecorder()
	g.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metadata", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /metadata: want 200, got %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/fhir+json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if !strings.Contains(rr.Body.String(), `"SHN Provider Da Vinci Ingress"`) {
		t.Fatal("body is not the provider ingress CapabilityStatement")
	}

	off := &Gateway{cfg: Config{Role: "provider"}} // IngressEnabled defaults false
	rr2 := httptest.NewRecorder()
	off.Handler().ServeHTTP(rr2, httptest.NewRequest(http.MethodGet, "/metadata", nil))
	if rr2.Code != http.StatusNotFound {
		t.Fatalf("ingress disabled: want 404, got %d", rr2.Code)
	}
}

// gatewayWithAuthStores is gatewayWithAuth with explicit key/replay stores — the
// shape two replicas share. nil selects the engine defaults.
func gatewayWithAuthStores(t *testing.T, clientID string, pubPEM []byte, keys IngressKeyStore, replay ReplayStore, opts ...func(*Config)) *Gateway {
	t.Helper()
	_, signPriv := genED25519(t)
	sor := newCensusSoR()
	cfg := Config{
		Role:           "provider",
		HolderID:       "provider",
		PayerRouter:    payerRouterFor(t, "payer"),
		Identity:       shnsdk.Identity{HolderID: "provider", SignPriv: signPriv},
		IngressEnabled: true,
		IngressBaseURL: testIngressBaseURL,
		IngressClients: map[string]IngressClientRegistration{
			clientID: {Alg: "ES384", PublicKeyPEM: pubPEM, Scopes: []string{ingressScope}},
		},
		Reg:         shnsdk.NewRegistry(),
		Validator:   shnsdk.NewFakeValidator(),
		SoR:         sor,
		Store:       sor,
		Clock:       ingressFixedClock(),
		HubURL:      "http://hub.test",
		IngressKeys: keys,
		Replay:      replay,
	}
	for _, o := range opts {
		o(&cfg)
	}
	return mustNew(t, cfg)
}

// countingKeyStore wraps a store and counts VerificationKey calls, so the
// pre-check rows can prove a malformed kid never reaches the store.
type countingKeyStore struct {
	IngressKeyStore
	verifyCalls int
}

func (c *countingKeyStore) VerificationKey(kid string, now time.Time) (*ecdsa.PublicKey, bool, error) {
	c.verifyCalls++
	return c.IngressKeyStore.VerificationKey(kid, now)
}

// authOK is ingressAuthOK's admit/refuse half, for the rows that assert only that.
// The outage half (unavailable ⇒ 503) has its own rows.
func authOK(g *Gateway, r *http.Request) bool {
	ok, _ := g.ingressAuthOK(r)
	return ok
}

// failingKeyStore models a key store whose backend is down.
type failingKeyStore struct{}

func (failingKeyStore) SigningKey(time.Time) (string, *ecdsa.PrivateKey, error) {
	return "", nil, errors.New("store down")
}

// VerificationKey answers "unknown kid", not "could not tell": this store models a
// backend whose SIGNING side is down, and the 401-vs-503 distinction on the verify side
// has its own store (unavailableKeyStore) and rows.
func (failingKeyStore) VerificationKey(string, time.Time) (*ecdsa.PublicKey, bool, error) {
	return nil, false, nil
}

// mintAssertionKID mints an ES384 JWT carrying an explicit kid header.
func mintAssertionKID(t *testing.T, key *ecdsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodES384, claims)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// issuedBearer drives the real /oauth/token route and returns the issued bearer.
func issuedBearer(t *testing.T, g *Gateway, priv *ecdsa.PrivateKey, clientID string) string {
	t.Helper()
	now := ingressFixedClock()()
	rec := postToken(t, g.Handler(), mintAssertion(t, priv, jwt.SigningMethodES384, validClaims(clientID, g.ingressAuth.tokenURL(), now)))
	if rec.Code != http.StatusOK {
		t.Fatalf("token: %d %s", rec.Code, rec.Body.String())
	}
	var tr struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &tr)
	return tr.AccessToken
}

func cdsReq(bearer string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, testIngressBaseURL+"/cds-services", nil)
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	return r
}

func TestIngressBearer_CarriesKidInWireShape(t *testing.T) {
	priv, pub := newTestClientKey(t)
	g := gatewayWithAuth(t, "br-provider", pub)
	bearer := issuedBearer(t, g, priv, "br-provider")
	parsed, _, err := jwt.NewParser().ParseUnverified(bearer, jwt.MapClaims{})
	if err != nil {
		t.Fatal(err)
	}
	kid, _ := parsed.Header["kid"].(string)
	if !validKID(kid) {
		t.Fatalf("issued bearer kid %q not in wire shape", kid)
	}
	if !authOK(g, cdsReq(bearer)) {
		t.Fatal("no-DSN gateway must accept its own bearer")
	}
}

func TestIngressBearer_SharedKeyStore_TokenFromAAcceptedAtB(t *testing.T) {
	priv, pub := newTestClientKey(t)
	keys, _ := newEphemeralKeyStore()
	replay := NewInMemoryReplayStore()
	a := gatewayWithAuthStores(t, "br-provider", pub, keys, replay)
	b := gatewayWithAuthStores(t, "br-provider", pub, keys, replay)
	bearer := issuedBearer(t, a, priv, "br-provider")
	if !authOK(b, cdsReq(bearer)) {
		t.Fatal("bearer issued by A must verify at B behind a shared key store")
	}
	// Separate stores (today's failure) reject — the row that defines the gap.
	c := gatewayWithAuthStores(t, "br-provider", pub, nil, nil)
	if authOK(c, cdsReq(bearer)) {
		t.Fatal("bearer from A must NOT verify at a gateway with its own key")
	}
}

func TestIngressToken_SharedReplay_AssertionReplayedAtBIs401(t *testing.T) {
	priv, pub := newTestClientKey(t)
	keys, _ := newEphemeralKeyStore()
	replay := NewInMemoryReplayStore()
	a := gatewayWithAuthStores(t, "br-provider", pub, keys, replay)
	b := gatewayWithAuthStores(t, "br-provider", pub, keys, replay)
	now := ingressFixedClock()()
	assertion := mintAssertion(t, priv, jwt.SigningMethodES384, validClaims("br-provider", a.ingressAuth.tokenURL(), now))
	if rec := postToken(t, a.Handler(), assertion); rec.Code != http.StatusOK {
		t.Fatalf("A: %d %s", rec.Code, rec.Body.String())
	}
	if rec := postToken(t, b.Handler(), assertion); rec.Code != http.StatusUnauthorized {
		t.Fatalf("B: replayed assertion status = %d, want 401", rec.Code)
	}
}

func TestIngressToken_SameJTIDifferentClientsBothFirstUse(t *testing.T) {
	privA, pubA := newTestClientKey(t)
	privB, pubB := newTestClientKey(t)
	_, signPriv := genED25519(t)
	sor := newCensusSoR()
	g := mustNew(t, Config{
		Role: "provider", HolderID: "provider", PayerRouter: payerRouterFor(t, "payer"),
		Identity:       shnsdk.Identity{HolderID: "provider", SignPriv: signPriv},
		IngressEnabled: true, IngressBaseURL: testIngressBaseURL,
		IngressClients: map[string]IngressClientRegistration{
			"client-a": {Alg: "ES384", PublicKeyPEM: pubA, Scopes: []string{ingressScope}},
			"client-b": {Alg: "ES384", PublicKeyPEM: pubB, Scopes: []string{ingressScope}},
		},
		Reg: shnsdk.NewRegistry(), Validator: shnsdk.NewFakeValidator(), SoR: sor, Store: sor,
		Clock: ingressFixedClock(), HubURL: "http://hub.test",
	})
	now := ingressFixedClock()()
	ca := validClaims("client-a", g.ingressAuth.tokenURL(), now)
	cb := validClaims("client-b", g.ingressAuth.tokenURL(), now)
	ca["jti"], cb["jti"] = "shared-jti", "shared-jti"
	if rec := postToken(t, g.Handler(), mintAssertion(t, privA, jwt.SigningMethodES384, ca)); rec.Code != http.StatusOK {
		t.Fatalf("client-a: %d", rec.Code)
	}
	if rec := postToken(t, g.Handler(), mintAssertion(t, privB, jwt.SigningMethodES384, cb)); rec.Code != http.StatusOK {
		t.Fatalf("client-b with the same jti must be a first use: %d %s", rec.Code, rec.Body.String())
	}
}

func TestIngressBearer_KidPreChecksNeverReachStore(t *testing.T) {
	_, pub := newTestClientKey(t)
	eph, err := newEphemeralKeyStore()
	if err != nil {
		t.Fatal(err)
	}
	counting := &countingKeyStore{IngressKeyStore: eph}
	g := gatewayWithAuthStores(t, "br-provider", pub, counting, nil)
	now := ingressFixedClock()()
	kid, key, err := eph.SigningKey(now)
	if err != nil {
		t.Fatal(err)
	}
	good := jwt.MapClaims{"client_id": "br-provider", "aud": testIngressBaseURL, "iat": now.Unix(), "exp": now.Add(time.Minute).Unix()}
	for _, tc := range []struct{ name, bearer string }{
		{"no kid header", mintAssertion(t, key, jwt.SigningMethodES384, good)},
		{"kid wrong length", mintAssertionKID(t, key, kid[:31], good)},
		{"kid uppercase hex", mintAssertionKID(t, key, "0123456789ABCDEF0123456789ABCDEF", good)}, // literal: ToUpper of a random kid can be a no-op
		{"kid non-hex", mintAssertionKID(t, key, "zz"+kid[2:], good)},
	} {
		if authOK(g, cdsReq(tc.bearer)) {
			t.Errorf("%s: accepted", tc.name)
		}
	}
	if counting.verifyCalls != 0 {
		t.Fatalf("store consulted %d times for malformed kids, want 0", counting.verifyCalls)
	}
	// Wrong alg with a well-formed kid: also rejected before the store (WithValidMethods).
	// Signed RS384 with a throwaway RSA key.
	rsaKey := newTestRSAKey(t)
	tok := jwt.NewWithClaims(jwt.SigningMethodRS384, good)
	tok.Header["kid"] = kid
	rs, err := tok.SignedString(rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	if authOK(g, cdsReq(rs)) || counting.verifyCalls != 0 {
		t.Fatalf("RS384 bearer accepted or store consulted (%d)", counting.verifyCalls)
	}
	// Unknown but well-formed kid DOES reach the store exactly once and is rejected.
	other, err := newKID()
	if err != nil {
		t.Fatal(err)
	}
	if authOK(g, cdsReq(mintAssertionKID(t, key, other, good))) {
		t.Fatal("unknown kid accepted")
	}
	if counting.verifyCalls != 1 {
		t.Fatalf("store consulted %d times for an unknown well-formed kid, want 1", counting.verifyCalls)
	}
}

func TestIngressToken_KeyStoreDown_503NoStore_DirectBearerStillWorks(t *testing.T) {
	priv, pub := newTestClientKey(t)
	g := gatewayWithAuthStores(t, "br-provider", pub, failingKeyStore{}, nil)
	now := ingressFixedClock()()
	rec := postToken(t, g.Handler(), mintAssertion(t, priv, jwt.SigningMethodES384, validClaims("br-provider", g.ingressAuth.tokenURL(), now)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
	}
	if !strings.Contains(rec.Body.String(), `"server_error"`) {
		t.Fatalf("body = %s, want server_error", rec.Body.String())
	}
	// The UDAP direct-bearer path does not depend on the key store.
	direct := mintAssertion(t, priv, jwt.SigningMethodES384, jwt.MapClaims{
		"iss": "br-provider", "sub": "br-provider", "aud": testIngressBaseURL,
		"iat": now.Unix(), "exp": now.Add(time.Minute).Unix(), "jti": "d-1",
	})
	if !authOK(g, cdsReq(direct)) {
		t.Fatal("direct bearer must still verify with the key store down")
	}
}

// TestIngressEphemeralKeyBootLine pins the no-DSN boot posture line: an operator
// reading the log must be told, once, that this gateway's ingress bearer key lives
// in this process only — and must NOT be told that when a shared key store was
// supplied or the ingress is off.
//
// It captures the process-global logger. That is safe here only because no test in
// gateway/engine calls t.Parallel(), so nothing else writes to the logger while the
// capture is installed.
func TestIngressEphemeralKeyBootLine(t *testing.T) {
	const want = "ingress bearer key is ephemeral (no SHN_STORE_DATABASE_URL): run a single reachable instance"
	capture := func(build func()) string {
		var buf bytes.Buffer
		log.SetOutput(&buf)
		defer log.SetOutput(os.Stderr)
		build()
		return buf.String()
	}
	_, pub := newTestClientKey(t)

	// Ingress on, no key store supplied ⇒ the line, exactly once.
	out := capture(func() { gatewayWithAuthStores(t, "br-provider", pub, nil, nil) })
	if n := strings.Count(out, want); n != 1 {
		t.Errorf("ingress on + ephemeral store: line appeared %d times, want 1; log=%q", n, out)
	}

	// Ingress off ⇒ silent (no ingress bearer key exists at all).
	out = capture(func() {
		_, signPriv := genED25519(t)
		sor := newCensusSoR()
		mustNew(t, Config{
			Role: "provider", HolderID: "provider", PayerRouter: payerRouterFor(t, "payer"),
			Identity: shnsdk.Identity{HolderID: "provider", SignPriv: signPriv},
			Reg:      shnsdk.NewRegistry(), Validator: shnsdk.NewFakeValidator(), SoR: sor, Store: sor,
			Clock: ingressFixedClock(), HubURL: "http://hub.test",
		})
	})
	if strings.Contains(out, want) {
		t.Errorf("ingress disabled: line emitted; log=%q", out)
	}

	// Ingress on with a caller-supplied key store ⇒ silent (the key is not ephemeral
	// as far as this gateway knows; the store owns its lifetime).
	eph, err := newEphemeralKeyStore()
	if err != nil {
		t.Fatal(err)
	}
	out = capture(func() {
		gatewayWithAuthStores(t, "br-provider", pub, &countingKeyStore{IngressKeyStore: eph}, nil)
	})
	if strings.Contains(out, want) {
		t.Errorf("caller-supplied key store: line emitted; log=%q", out)
	}
}

// nilPubKeyStore models a third-party IngressKeyStore that answers "known kid"
// with a nil public key — the (nil, true) shape a store outside this module can
// return through the public Config.IngressKeys seam.
type nilPubKeyStore struct{ IngressKeyStore }

func (nilPubKeyStore) VerificationKey(string, time.Time) (*ecdsa.PublicKey, bool, error) {
	return nil, true, nil
}

// TestIngressBearer_CurrentKidWrongSignatureIs401: two rejection rows for the
// keyfunc, both driven through a real ingress route so the wire answer is pinned,
// not just the predicate.
//
//  1. A bearer carrying the store's CURRENT kid but signed by a different P-384
//     key — the kid pre-checks pass and the store resolves a key, so the only
//     thing standing between the caller and the route is signature verification.
//  2. A store that answers (nil, true) for that same kid — the public-seam shape
//     the keyfunc's nil guard exists for; without the guard a nil key would reach
//     ecdsa.Verify.
func TestIngressBearer_CurrentKidWrongSignatureIs401(t *testing.T) {
	_, pub := newTestClientKey(t)
	now := ingressFixedClock()()

	eph, err := newEphemeralKeyStore()
	if err != nil {
		t.Fatal(err)
	}
	kid, _, err := eph.SigningKey(now)
	if err != nil {
		t.Fatal(err)
	}
	claims := jwt.MapClaims{
		"client_id": "br-provider", "aud": testIngressBaseURL,
		"iat": now.Unix(), "exp": now.Add(time.Minute).Unix(),
	}
	forged, _ := newTestClientKey(t) // a different throwaway P-384 key
	bearer := mintAssertionKID(t, forged, kid, claims)

	for _, tc := range []struct {
		name  string
		keys  IngressKeyStore
		token string
	}{
		{"current kid, signature by another key", eph, bearer},
		{"store answers a nil key for a known kid", nilPubKeyStore{IngressKeyStore: eph}, bearer},
	} {
		g := gatewayWithAuthStores(t, "br-provider", pub, tc.keys, nil)
		if authOK(g, cdsReq(tc.token)) {
			t.Errorf("%s: ingressAuthOK returned true, want false", tc.name)
		}
		rec := httptest.NewRecorder()
		g.Handler().ServeHTTP(rec, cdsReq(tc.token))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: GET /cds-services = %d, want 401; body=%s", tc.name, rec.Code, rec.Body.String())
		}
	}
}

// The token endpoint must tell an OUTAGE apart from a replay. Under
// SHN_STORE_DATABASE_URL the replay record and the signing key share one pool, and the
// signing key is resolved first and the one-time-use record is written last — so "the
// database is unreachable" reaches handleToken as a signing-key error on the shared
// pool, and as a replay-store error when only the record's write fails. Answering 401 there
// would tell an honest client its credential is bad and is what the deployment docs
// promise it will not do.
func TestIngressToken_ReplayStoreOutageIs503NotReplay401(t *testing.T) {
	priv, pub := newTestClientKey(t)
	now := ingressFixedClock()()
	for _, tc := range []struct {
		name   string
		store  *stubReplayStore
		status int
		want   string
	}{
		{"store cannot tell: outage", &stubReplayStore{replay: true, err: errors.New("store down")}, http.StatusServiceUnavailable, `"server_error"`},
		{"store errors without claiming a replay", &stubReplayStore{err: errors.New("store down")}, http.StatusServiceUnavailable, `"server_error"`},
		{"store says replay", &stubReplayStore{replay: true}, http.StatusUnauthorized, `"invalid_client"`},
		{"healthy first use", &stubReplayStore{}, http.StatusOK, `"access_token"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gatewayWithAuthStores(t, "br-provider", pub, nil, tc.store)
			rec := postToken(t, g.Handler(), mintAssertion(t, priv, jwt.SigningMethodES384, validClaims("br-provider", g.ingressAuth.tokenURL(), now)))
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.status, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("body = %s, want %s", rec.Body.String(), tc.want)
			}
			// Every answer on this endpoint is uncacheable (RFC 6749), the 503 included:
			// a cache must not pin an outage answer past the outage.
			if rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
			}
			if tc.store.calls != 1 {
				t.Fatalf("replay store consulted %d times, want 1", tc.store.calls)
			}
		})
	}
}

// The realistic outage pairing: ONE database behind both stores, so the replay record
// and the signing key fail together. The engine must still answer 503 — the row above
// pairs a healthy key store with a broken record, and this one proves the combination a
// DSN deployment can actually be in does not degrade to 401. The UDAP direct-bearer
// path, which consults neither store, keeps working throughout.
func TestIngressToken_WholeStoreDown_503_DirectBearerStillWorks(t *testing.T) {
	priv, pub := newTestClientKey(t)
	down := &stubReplayStore{replay: true, err: errors.New("store down")}
	g := gatewayWithAuthStores(t, "br-provider", pub, failingKeyStore{}, down)
	now := ingressFixedClock()()
	rec := postToken(t, g.Handler(), mintAssertion(t, priv, jwt.SigningMethodES384, validClaims("br-provider", g.ingressAuth.tokenURL(), now)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	// The signing key is the stage that refused — it is resolved BEFORE the record, so
	// this refusal spends nothing — and the description says so, rather than sending an
	// operator hunting the store that was never reached.
	if !strings.Contains(rec.Body.String(), "signing key unavailable") {
		t.Fatalf("body = %s, want the signing key named", rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
	}
	direct := mintAssertion(t, priv, jwt.SigningMethodES384, jwt.MapClaims{
		"iss": "br-provider", "sub": "br-provider", "aud": testIngressBaseURL,
		"iat": now.Unix(), "exp": now.Add(time.Minute).Unix(), "jti": "d-1",
	})
	if !authOK(g, cdsReq(direct)) {
		t.Fatal("direct bearer must still verify with the whole store down")
	}
}

// metrics.go states that the store-error counter is how a database outage becomes
// visible to an operator. The token endpoint refuses on TWO different stores, and each
// must be attributable: the signing key (consulted first, so it is what a shared pool's
// outage reaches) and the one-time-use record. A refusal that only logs is an outage no
// alarm can see.
func TestIngressToken_StoreErrorMetricNamesTheFailingStore(t *testing.T) {
	priv, pub := newTestClientKey(t)
	now := ingressFixedClock()()
	for _, tc := range []struct {
		name   string
		keys   IngressKeyStore
		replay ReplayStore
		status int
		want   []string
	}{
		{"record unavailable", nil, &stubReplayStore{err: errors.New("store down")}, http.StatusServiceUnavailable, []string{storeErrReplay}},
		{"signing key unavailable", failingKeyStore{}, &stubReplayStore{}, http.StatusServiceUnavailable, []string{storeErrIngressKey}},
		{"healthy", nil, &stubReplayStore{}, http.StatusOK, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stores []string
			g := gatewayWithAuthStores(t, "br-provider", pub, tc.keys, tc.replay, func(c *Config) {
				c.StoreErrorMetric = func(s string) { stores = append(stores, s) }
			})
			rec := postToken(t, g.Handler(), mintAssertion(t, priv, jwt.SigningMethodES384, validClaims("br-provider", g.ingressAuth.tokenURL(), now)))
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.status, rec.Body.String())
			}
			if len(stores) != len(tc.want) {
				t.Fatalf("store-error metric calls = %v, want %v", stores, tc.want)
			}
			for i, w := range tc.want {
				if stores[i] != w {
					t.Fatalf("store-error metric calls = %v, want %v", stores, tc.want)
				}
			}
		})
	}
}

// unavailableKeyStore models the shared key store during a database outage: it can
// neither resolve nor deny a kid, which is the third shape of the IngressKeyStore seam.
type unavailableKeyStore struct {
	IngressKeyStore
	calls int
}

var errKeyStoreDown = errors.New("key store: database unreachable")

func (u *unavailableKeyStore) VerificationKey(string, time.Time) (*ecdsa.PublicKey, bool, error) {
	u.calls++
	return nil, false, errKeyStoreDown
}

// TestIngressBearer_KeyStoreUnavailableIs503: a bearer whose kid this replica cannot
// RESOLVE — because the shared key store could not be read — is refused with the same 503
// shape every other shared-state outage answers (503, Cache-Control: no-store, the cause
// named), never the 401 that tells an honest partner its credential is bad. The 503 states
// that this gateway could not tell; it promises no retry, and only the token endpoint
// does. The refusal is still a refusal: the leg is not processed. An outage is only
// actionable if it is counted, so the row pins the store-error metric too.
//
// The companion rows keep the distinction honest: an UNKNOWN kid (the store can tell, and
// says no) is still a 401, and the UDAP direct-bearer arm — which touches no shared
// store — still admits its caller straight through the outage.
func TestIngressBearer_KeyStoreUnavailableIs503(t *testing.T) {
	priv, pub := newTestClientKey(t)
	now := ingressFixedClock()()
	kid, err := newKID()
	if err != nil {
		t.Fatal(err)
	}
	bearerClaims := jwt.MapClaims{
		"client_id": "br-provider", "aud": testIngressBaseURL,
		"iat": now.Unix(), "exp": now.Add(time.Minute).Unix(),
	}
	// Signed by a key the store would never hand back: verification never gets that far —
	// the kid cannot be resolved at all.
	bearer := mintAssertionKID(t, priv, kid, bearerClaims)

	serve := func(t *testing.T, keys IngressKeyStore, body string) (*httptest.ResponseRecorder, []string, *Gateway) {
		t.Helper()
		var stores []string
		g := gatewayWithAuthStores(t, "br-provider", pub, keys, nil, func(c *Config) {
			c.StoreErrorMetric = func(s string) { stores = append(stores, s) }
		})
		req := httptest.NewRequest(http.MethodPost, testIngressBaseURL+"/cds-services/order-select-crd", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		g.Handler().ServeHTTP(rec, req)
		return rec, stores, g
	}

	// Control: the SAME bearer against a store that can tell is a plain 401 — so the 503
	// below is the outage and not the bearer.
	eph, err := newEphemeralKeyStore()
	if err != nil {
		t.Fatal(err)
	}
	rec, stores, _ := serve(t, eph, "{}")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown kid = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	if len(stores) != 0 {
		t.Fatalf("an unknown kid is not a store failure, but the metric fired %v", stores)
	}

	down := &unavailableKeyStore{IngressKeyStore: eph}
	rec, stores, g := serve(t, down, "{}")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("bearer during a key-store outage = %d, want 503 (a database outage must not answer 401); body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store (an outage answer must not be cached past the outage)", rec.Header().Get("Cache-Control"))
	}
	if len(stores) != 1 || stores[0] != storeErrIngressKey {
		t.Fatalf("store-error metric calls = %v, want exactly [%s]", stores, storeErrIngressKey)
	}
	if down.calls != 1 {
		t.Fatalf("key store consulted %d times, want 1", down.calls)
	}
	// The refusal is a refusal: the request never reached the CRD driver (a healthy
	// gate answers 400 "missing context.patientId" for this body), so no leg ran.
	if legs := g.ExchangeSnapshot(); len(legs) != 0 {
		t.Fatalf("the refused request recorded %d exchanges, want 0", len(legs))
	}

	// The UDAP direct-bearer arm consults no shared store and keeps answering.
	direct := mintAssertion(t, priv, jwt.SigningMethodES384, jwt.MapClaims{
		"iss": "br-provider", "sub": "br-provider", "aud": testIngressBaseURL,
		"iat": now.Unix(), "exp": now.Add(time.Minute).Unix(), "jti": "d-outage",
	})
	dg := gatewayWithAuthStores(t, "br-provider", pub, &unavailableKeyStore{IngressKeyStore: eph}, nil)
	if !authOK(dg, cdsReq(direct)) {
		t.Fatal("a direct bearer must still be admitted while the ingress key store is unreadable")
	}
	// …and the outage does not turn the direct-bearer arm's OWN refusals into 503s.
	if ok, unavailable := dg.ingressAuthOK(cdsReq("not-a-jwt")); ok || unavailable {
		t.Fatalf("garbage bearer during the outage = ok:%v unavailable:%v; want a plain refusal", ok, unavailable)
	}
}

// The token endpoint's half of the one-time-use key bound (see MaxReplayKeyBytes and
// TestReplayKeyLengthBound_RefusedByEveryCallerBeforeTheStore): an oversized jti is the
// client's own field and is refused as a bad credential — 401, before the record is
// consulted, with no store error counted. The control row keeps the bound honest: a jti
// AT the bound is a perfectly good credential and still issues a token.
func TestIngressToken_OversizedJTIIsRefusedBeforeTheRecord(t *testing.T) {
	priv, pub := newTestClientKey(t)
	now := ingressFixedClock()()
	eph, err := newEphemeralKeyStore()
	if err != nil {
		t.Fatal(err)
	}
	post := func(t *testing.T, jti string) (*httptest.ResponseRecorder, []string, int) {
		t.Helper()
		var stores []string
		consulted := 0
		replay := &recordingReplayStore{inner: NewInMemoryReplayStore(), onCall: func() { consulted++ }}
		g := gatewayWithAuthStores(t, "br-provider", pub, eph, replay, func(c *Config) {
			c.StoreErrorMetric = func(s string) { stores = append(stores, s) }
		})
		claims := validClaims("br-provider", g.ingressAuth.tokenURL(), now)
		claims["jti"] = jti
		return postToken(t, g.Handler(), mintAssertion(t, priv, jwt.SigningMethodES384, claims)), stores, consulted
	}

	rec, stores, consulted := post(t, strings.Repeat("j", MaxReplayKeyBytes+1))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("oversized jti = %d, want 401 (a bad credential, never the 503 an oversized index row would produce); body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"invalid_client"`) {
		t.Fatalf("body = %s, want invalid_client", rec.Body.String())
	}
	if consulted != 0 {
		t.Fatalf("the one-time-use record was consulted %d times for an oversized jti, want 0", consulted)
	}
	if len(stores) != 0 {
		t.Fatalf("an oversized jti counted a store error %v — it is a bad credential, not an outage", stores)
	}

	// Control: a jti exactly AT the bound issues.
	rec, _, consulted = post(t, strings.Repeat("j", MaxReplayKeyBytes))
	if rec.Code != http.StatusOK {
		t.Fatalf("jti at the bound = %d, want 200 (the bound must not cut off a legitimate credential); body=%s", rec.Code, rec.Body.String())
	}
	if consulted != 1 {
		t.Fatalf("record consulted %d times for an accepted jti, want 1", consulted)
	}
}

// recordingReplayStore counts consultations of the one-time-use record.
type recordingReplayStore struct {
	inner  ReplayStore
	onCall func()
}

func (r *recordingReplayStore) CheckAndRecord(scope, clientID, key string, now, expiresAt time.Time) (bool, error) {
	r.onCall()
	return r.inner.CheckAndRecord(scope, clientID, key, now, expiresAt)
}

// outageKeyStore is a shared key store whose SIGNING side can be taken down and brought
// back inside one row, so the same gateway can answer an outage and then a recovery.
// Verification delegates to the embedded store throughout.
type outageKeyStore struct {
	IngressKeyStore
	down bool
}

func (o *outageKeyStore) SigningKey(now time.Time) (string, *ecdsa.PrivateKey, error) {
	if o.down {
		return "", nil, errors.New("store down")
	}
	return o.IngressKeyStore.SigningKey(now)
}

// outageReplayStore is the one-time-use record with the same flippable outage, counting
// every consultation. A call made while it is down never reaches the inner store, so no
// record is written for it.
type outageReplayStore struct {
	inner ReplayStore
	down  bool
	calls int
}

func (o *outageReplayStore) CheckAndRecord(scope, clientID, key string, now, expiresAt time.Time) (bool, error) {
	o.calls++
	if o.down {
		return false, errors.New("store down")
	}
	return o.inner.CheckAndRecord(scope, clientID, key, now, expiresAt)
}

// The ORDERING rows: a 503 raised by a stage that runs BEFORE the one-time-use record
// leaves the jti unspent, so the same assertion still issues once the store is back.
// That is a fact about these two stages, NOT the endpoint's general rule — a store error
// on the record's own write is ambiguous and may well have spent the jti
// (TestIngressToken_AckLostRecordWrite_FreshAssertionIsTheRetry). The partner-facing rule
// is one rule for every 503: retry with a fresh assertion. What these rows pin is that
// nothing decidable is refused after the record, so the window where a client can lose an
// assertion to a 503 is exactly the record write and nothing more.
func TestIngressToken_RefusalsBeforeTheRecordLeaveTheAssertionUnspent(t *testing.T) {
	priv, pub := newTestClientKey(t)
	now := ingressFixedClock()()

	newStores := func(t *testing.T) (*outageKeyStore, *outageReplayStore) {
		t.Helper()
		eph, err := newEphemeralKeyStore()
		if err != nil {
			t.Fatal(err)
		}
		return &outageKeyStore{IngressKeyStore: eph}, &outageReplayStore{inner: NewInMemoryReplayStore()}
	}

	t.Run("signing key unavailable", func(t *testing.T) {
		keys, replay := newStores(t)
		keys.down = true
		var stores []string
		g := gatewayWithAuthStores(t, "br-provider", pub, keys, replay, func(c *Config) {
			c.StoreErrorMetric = func(s string) { stores = append(stores, s) }
		})
		assertion := mintAssertion(t, priv, jwt.SigningMethodES384, validClaims("br-provider", g.ingressAuth.tokenURL(), now))

		rec := postToken(t, g.Handler(), assertion)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "signing key unavailable") {
			t.Fatalf("body = %s, want the signing key named as the stage that refused", rec.Body.String())
		}
		if replay.calls != 0 {
			t.Fatalf("the one-time-use record was consulted %d times while the signing key was down, want 0 — a 503 that burns the jti makes the retry a 401", replay.calls)
		}
		if len(stores) != 1 || stores[0] != storeErrIngressKey {
			t.Fatalf("store-error metric calls = %v, want [%s]", stores, storeErrIngressKey)
		}

		// Recovery: the SAME assertion — the one the client already sent — must issue.
		keys.down = false
		rec = postToken(t, g.Handler(), assertion)
		if rec.Code != http.StatusOK {
			t.Fatalf("the same assertion after recovery = %d, want 200 (the 503 consumed the jti); body=%s", rec.Code, rec.Body.String())
		}
		if replay.calls != 1 {
			t.Fatalf("record consulted %d times across both requests, want 1 (only the issuing one)", replay.calls)
		}
	})

	t.Run("one-time-use record unavailable", func(t *testing.T) {
		keys, replay := newStores(t)
		replay.down = true
		var stores []string
		g := gatewayWithAuthStores(t, "br-provider", pub, keys, replay, func(c *Config) {
			c.StoreErrorMetric = func(s string) { stores = append(stores, s) }
		})
		assertion := mintAssertion(t, priv, jwt.SigningMethodES384, validClaims("br-provider", g.ingressAuth.tokenURL(), now))

		rec := postToken(t, g.Handler(), assertion)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "one-time-use record unavailable") {
			t.Fatalf("body = %s, want the record named as the stage that refused", rec.Body.String())
		}
		if len(stores) != 1 || stores[0] != storeErrReplay {
			t.Fatalf("store-error metric calls = %v, want [%s]", stores, storeErrReplay)
		}

		// Recovery: this store never reached its inner record while it was down, so
		// nothing landed and the same assertion is still first use. An outage store can
		// only prove THIS half; the ack-lost row covers the other.
		replay.down = false
		rec = postToken(t, g.Handler(), assertion)
		if rec.Code != http.StatusOK {
			t.Fatalf("the same assertion after recovery = %d, want 200 (the failed record write still consumed the jti); body=%s", rec.Code, rec.Body.String())
		}
		// …and it is one-time-use from there: the third presentation is a replay.
		rec = postToken(t, g.Handler(), assertion)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("re-presenting an assertion that DID issue = %d, want 401; body=%s", rec.Code, rec.Body.String())
		}
	})
}

// wrongCurveKeyStore hands out a key the ES384 signer cannot use (P-256, not P-384) while
// `bad` is set, and delegates to the real store once it is cleared. It is the only way to
// reach signBearer's failure branch without a store error, so the row below can pin what
// that branch answers — and prove the assertion survives it.
type wrongCurveKeyStore struct {
	IngressKeyStore
	bad *ecdsa.PrivateKey
}

func (w *wrongCurveKeyStore) SigningKey(now time.Time) (string, *ecdsa.PrivateKey, error) {
	if w.bad != nil {
		return "0123456789abcdef0123456789abcdef", w.bad, nil
	}
	return w.IngressKeyStore.SigningKey(now)
}

// A signing failure with a key already in hand is a `503` like any other, and it must not
// cost the client its assertion either: the bearer is signed into a local BEFORE the
// one-time-use record is written, so nothing is spent when the signature cannot be
// produced. This branch is reachable in production — the shared key table has more than
// one writer, and a row whose key is not the curve ES384 needs would otherwise turn every
// token request into a 503 that burns the jti it was handed.
func TestIngressToken_SigningFailureLeavesTheAssertionUnspent(t *testing.T) {
	priv, pub := newTestClientKey(t)
	now := ingressFixedClock()()
	p256, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	eph, err := newEphemeralKeyStore()
	if err != nil {
		t.Fatal(err)
	}
	keys := &wrongCurveKeyStore{IngressKeyStore: eph, bad: p256}
	replay := &outageReplayStore{inner: NewInMemoryReplayStore()}
	var stores []string
	g := gatewayWithAuthStores(t, "br-provider", pub, keys, replay, func(c *Config) {
		c.StoreErrorMetric = func(s string) { stores = append(stores, s) }
	})
	assertion := mintAssertion(t, priv, jwt.SigningMethodES384, validClaims("br-provider", g.ingressAuth.tokenURL(), now))

	rec := postToken(t, g.Handler(), assertion)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "fresh client_assertion") {
		t.Fatalf("body = %s: this 503 must not ask for a fresh assertion — it spends nothing", rec.Body.String())
	}
	if replay.calls != 0 {
		t.Fatalf("the one-time-use record was consulted %d times for a request that could not be signed, want 0", replay.calls)
	}
	if len(stores) != 0 {
		t.Fatalf("a signing failure counted a store error %v — no store was unreachable", stores)
	}

	// Recovery: the SAME assertion the client already sent must issue.
	keys.bad = nil
	rec = postToken(t, g.Handler(), assertion)
	if rec.Code != http.StatusOK {
		t.Fatalf("the same assertion after recovery = %d, want 200 (the signing failure spent the jti); body=%s", rec.Code, rec.Body.String())
	}
	if replay.calls != 1 {
		t.Fatalf("record consulted %d times across both requests, want 1 (only the issuing one)", replay.calls)
	}
}

// A scope the client is not registered for is a bad request, not a spent credential: the
// scope is checked BEFORE the one-time-use record, so a client that fixes the scope and
// retries with the same assertion still issues. Every other rejection on this endpoint
// already ran before the record; this was the last one that did not.
func TestIngressToken_BadScopeRefusedBeforeTheRecord(t *testing.T) {
	priv, pub := newTestClientKey(t)
	now := ingressFixedClock()()
	eph, err := newEphemeralKeyStore()
	if err != nil {
		t.Fatal(err)
	}
	replay := &outageReplayStore{inner: NewInMemoryReplayStore()}
	g := gatewayWithAuthStores(t, "br-provider", pub, eph, replay)
	assertion := mintAssertion(t, priv, jwt.SigningMethodES384, validClaims("br-provider", g.ingressAuth.tokenURL(), now))

	rec := postTokenScope(t, g.Handler(), assertion, "system/Patient.read")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unregistered scope = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"invalid_scope"`) {
		t.Fatalf("body = %s, want invalid_scope", rec.Body.String())
	}
	if replay.calls != 0 {
		t.Fatalf("the one-time-use record was consulted %d times for a refused scope, want 0", replay.calls)
	}

	// The same assertion, with a scope the client IS registered for, issues.
	rec = postTokenScope(t, g.Handler(), assertion, ingressScope)
	if rec.Code != http.StatusOK {
		t.Fatalf("the same assertion with a registered scope = %d, want 200 (the 400 spent the jti); body=%s", rec.Code, rec.Body.String())
	}
	// …and one-time-use still holds from there: the third presentation is a replay.
	rec = postTokenScope(t, g.Handler(), assertion, ingressScope)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("re-presenting an assertion that DID issue = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
}

// ackLostReplayStore is the AMBIGUOUS write: CheckAndRecord reaches the inner store — the
// record LANDS — and then, on the first call only, returns the error a lost acknowledgment
// produces (the connection dropped after the INSERT committed, or the statement deadline
// fired while the commit was already on its way back). It fails closed with replay=true,
// which is what pgstore.ReplayStore returns alongside its error.
//
// No outage stub can model this: an outage store never reaches the record at all, so it
// can only prove the case where nothing was spent. A store error never proves the record
// was not written, and this is the half that says so.
type ackLostReplayStore struct {
	inner ReplayStore
	calls int
}

func (a *ackLostReplayStore) CheckAndRecord(scope, clientID, key string, now, expiresAt time.Time) (bool, error) {
	a.calls++
	replayed, err := a.inner.CheckAndRecord(scope, clientID, key, now, expiresAt)
	if a.calls == 1 {
		return true, errors.New("store: connection reset after the record committed")
	}
	return replayed, err
}

// The retry contract when the one-time-use record's write is acknowledged but the
// acknowledgment is lost. The endpoint cannot tell a committed record from an
// uncommitted one, so it answers the same retryable 503 — and NO token. The client's rule
// is therefore ONE rule on every 503: mint a FRESH client_assertion (the private_key_jwt
// norm — one assertion per request). A retry that re-sends the SAME assertion may be
// refused as replayed, and that refusal is CORRECT: this row pins it as the contract
// rather than a bug, so nobody "fixes" it later with idempotent issuance.
func TestIngressToken_AckLostRecordWrite_FreshAssertionIsTheRetry(t *testing.T) {
	priv, pub := newTestClientKey(t)
	now := ingressFixedClock()()
	replay := &ackLostReplayStore{inner: NewInMemoryReplayStore()}
	var stores []string
	g := gatewayWithAuthStores(t, "br-provider", pub, nil, replay, func(c *Config) {
		c.StoreErrorMetric = func(s string) { stores = append(stores, s) }
	})
	claims := validClaims("br-provider", g.ingressAuth.tokenURL(), now)
	claims["jti"] = "ack-lost-1"
	assertion := mintAssertion(t, priv, jwt.SigningMethodES384, claims)

	// 1. The record commits, the acknowledgment is lost: 503, and no token on the wire.
	rec := postToken(t, g.Handler(), assertion)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"server_error"`) {
		t.Fatalf("body = %s, want server_error", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "access_token") {
		t.Fatalf("body = %s: a 503 must never carry a token", rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
	}
	// The outage is counted once, against the record — the store an operator has to go
	// look at. Nothing else on this request touched a store.
	if len(stores) != 1 || stores[0] != storeErrReplay {
		t.Fatalf("store-error metric calls = %v, want [%s]", stores, storeErrReplay)
	}

	// 2. The SAME assertion is refused as replayed — the record DID land. This is the
	// documented outcome of a same-assertion retry after a 503, not a broken promise.
	rec = postToken(t, g.Handler(), assertion)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("same assertion after the 503 = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"invalid_client"`) || !strings.Contains(rec.Body.String(), "replayed") {
		t.Fatalf("body = %s, want invalid_client / replayed", rec.Body.String())
	}
	// A refusal the store could answer is NOT a store outage: counting it would alarm on
	// a healthy database every time a client re-sent a spent assertion.
	if len(stores) != 1 {
		t.Fatalf("store-error metric calls = %v after the 401, want the 503's one call only", stores)
	}

	// 3. A FRESH assertion — the client's actual retry — issues, and the bearer verifies.
	fresh := validClaims("br-provider", g.ingressAuth.tokenURL(), now)
	fresh["jti"] = "ack-lost-2"
	rec = postToken(t, g.Handler(), mintAssertion(t, priv, jwt.SigningMethodES384, fresh))
	if rec.Code != http.StatusOK {
		t.Fatalf("fresh assertion after the 503 = %d, want 200 — the retry the contract asks for must work; body=%s", rec.Code, rec.Body.String())
	}
	var tr struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &tr); err != nil {
		t.Fatalf("decode token: %v", err)
	}
	if tr.AccessToken == "" {
		t.Fatalf("200 with no access_token: %s", rec.Body.String())
	}
	if !authOK(g, cdsReq(tr.AccessToken)) {
		t.Fatal("the bearer issued to the fresh assertion does not verify")
	}
	if replay.calls != 3 {
		t.Fatalf("record consulted %d times across the three requests, want 3", replay.calls)
	}
}
