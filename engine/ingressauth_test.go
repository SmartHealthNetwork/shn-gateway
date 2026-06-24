package engine

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func newTestAuthServer(t *testing.T, clientID string, pubPEM []byte, alg string) *ingressAuthServer {
	t.Helper()
	s, err := newIngressAuthServer(testIngressBaseURL,
		map[string]IngressClientRegistration{clientID: {Alg: alg, PublicKeyPEM: pubPEM, Scopes: []string{ingressScope}}},
		ingressFixedClock())
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
	var bearerClaims jwt.MapClaims
	_, err := jwt.NewParser(
		jwt.WithValidMethods([]string{"ES384"}),
		jwt.WithTimeFunc(clock),
	).ParseWithClaims(tr.AccessToken, &bearerClaims, func(*jwt.Token) (any, error) {
		return s.bearerKey.Public(), nil
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
	sor := NewStubHolderData()   // existing engine stub; satisfies both SoR and Store
	g := New(Config{
		Role:           "provider",
		HolderID:       "provider",
		CounterpartID:  "payer",
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
	if err := json.Unmarshal(rec.Body.Bytes(), &tr); err != nil {
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
	if !g.ingressAuthOK(withBearer(bearer)) {
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
		{"wrong aud", mintAssertion(t, g.ingressAuth.bearerKey, jwt.SigningMethodES384, jwt.MapClaims{
			"client_id": "br-provider", "aud": "https://evil", "iat": now.Unix(), "exp": now.Add(time.Minute).Unix()})},
		// Fix M2: expired bearer — passes signature (server's own key) but fails
		// WithExpirationRequired()/expiry guard in verifyBearer.
		{"expired bearer", mintAssertion(t, g.ingressAuth.bearerKey, jwt.SigningMethodES384, jwt.MapClaims{
			"client_id": "br-provider", "aud": g.ingressAuth.baseURL,
			"iat": now.Add(-10 * time.Minute).Unix(), "exp": now.Add(-time.Minute).Unix()})},
	} {
		if g.ingressAuthOK(withBearer(tc.hdr)) {
			t.Errorf("%s: ingressAuthOK returned true, want false", tc.name)
		}
	}
}

// Pin the carry-forward: a zero-value Gateway (nil ingressAuth, no bypass) must
// fail closed WITHOUT panicking.
func TestIngressAuthOK_NilServerFailsClosedNoPanic(t *testing.T) {
	g := &Gateway{}
	req := httptest.NewRequest(http.MethodPost, "/cds-services/x", nil)
	if g.ingressAuthOK(req) {
		t.Fatal("nil ingressAuth + no bypass: ingressAuthOK = true, want false")
	}
}
