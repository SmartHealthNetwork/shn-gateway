// Package smartauth lets a gateway authenticate to its FHIR data server via SMART
// Backend Services (RFC 7523 signed-JWT client-credentials). This is the gateway →
// FHIR-data-server edge ONLY — backend data access, where SMART Backend Services
// belongs. It is NOT the per-operation substrate authority on the sealed Hub legs
// (OWD-6/AI-11 still refuse a reusable bearer there); see the credentialing posture.
// Signing/verification use golang-jwt/jwt/v5 (the internal/accountsvc pattern,
// WithValidMethods alg-pinning); no hand-rolled JWS.
package smartauth

import (
	"context"
	"crypto"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	defaultAssertionTTL = 5 * time.Minute
	defaultRefreshSkew  = 60 * time.Second
	maxTokenBody        = 1 << 20 // 1 MiB cap on the token response
)

// Config wires a SMART Backend Services token source.
type Config struct {
	TokenURL     string            // SMART token endpoint
	ClientID     string            // registered client id (iss/sub of the assertion)
	Scope        string            // requested scope, e.g. "system/*.read"
	Alg          string            // "ES384" | "RS384"
	Key          crypto.PrivateKey // *ecdsa.PrivateKey (ES384) | *rsa.PrivateKey (RS384)
	KID          string            // optional JWS header kid
	AssertionTTL time.Duration     // default 5m
	RefreshSkew  time.Duration     // re-mint when within this of expiry; default 60s
	Clock        func() time.Time  // default time.Now
	HTTPClient   *http.Client      // default a 10s-timeout client
}

func (c Config) clock() time.Time {
	if c.Clock != nil {
		return c.Clock()
	}
	return time.Now()
}

func signingMethod(alg string) (jwt.SigningMethod, error) {
	switch alg {
	case "ES384":
		return jwt.SigningMethodES384, nil
	case "RS384":
		return jwt.SigningMethodRS384, nil
	default:
		return nil, fmt.Errorf("smartauth: unsupported alg %q (want ES384|RS384)", alg)
	}
}

// TokenSource mints, caches, and re-mints a SMART Backend Services bearer token.
// Token() is concurrency-safe and FAIL-CLOSED: any failure returns an error and
// never a stale/empty token.
type TokenSource struct {
	Config
	mu     sync.Mutex
	cached string
	exp    time.Time
}

// Token returns a bearer valid for at least RefreshSkew, minting a fresh one if
// the cache is empty or near expiry.
func (s *TokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	skew := s.RefreshSkew
	if skew == 0 {
		skew = defaultRefreshSkew
	}
	if s.cached != "" && s.clock().Add(skew).Before(s.exp) {
		return s.cached, nil
	}
	tok, ttl, err := s.fetch(ctx)
	if err != nil {
		return "", err // fail-closed: do not return the stale cache
	}
	s.cached = tok
	s.exp = s.clock().Add(ttl)
	return tok, nil
}

func (s *TokenSource) fetch(ctx context.Context) (token string, ttl time.Duration, err error) {
	assertion, err := s.assertion()
	if err != nil {
		return "", 0, err
	}
	form := url.Values{
		"grant_type":            {"client_credentials"},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {assertion},
	}
	if s.Scope != "" {
		form.Set("scope", s.Scope)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("smartauth: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	hc := s.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("smartauth: token endpoint: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenBody))
	if err != nil {
		return "", 0, fmt.Errorf("smartauth: read token response body: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return "", 0, fmt.Errorf("smartauth: token endpoint status %d: %s", resp.StatusCode, string(body))
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", 0, fmt.Errorf("smartauth: decode token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", 0, fmt.Errorf("smartauth: empty access_token")
	}
	ttl = time.Duration(tr.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = defaultAssertionTTL // conservative when the server omits expires_in
	}
	return tr.AccessToken, ttl, nil
}

func (s *TokenSource) assertion() (string, error) {
	method, err := signingMethod(s.Alg)
	if err != nil {
		return "", err
	}
	ttl := s.AssertionTTL
	if ttl == 0 {
		ttl = defaultAssertionTTL
	}
	// Use real wall time for JWT timestamps so the assertion is valid from the
	// token endpoint's perspective. Config.Clock is a cache-eviction hook only
	// (governs when TokenSource decides to refresh), not a signing-time override.
	now := time.Now()
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return "", fmt.Errorf("smartauth: jti: %w", err)
	}
	claims := jwt.MapClaims{
		"iss": s.ClientID,
		"sub": s.ClientID,
		"aud": s.TokenURL,
		"jti": hex.EncodeToString(jti),
		"iat": now.Unix(),
		"exp": now.Add(ttl).Unix(),
	}
	tok := jwt.NewWithClaims(method, claims)
	if s.KID != "" {
		tok.Header["kid"] = s.KID
	}
	signed, err := tok.SignedString(s.Key)
	if err != nil {
		return "", fmt.Errorf("smartauth: sign assertion: %w", err)
	}
	return signed, nil
}
