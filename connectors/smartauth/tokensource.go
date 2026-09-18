// Package smartauth lets a gateway authenticate to its FHIR data server via SMART
// Backend Services (RFC 7523 signed-JWT client-credentials). This is the gateway →
// FHIR-data-server edge ONLY — backend data access, where SMART Backend Services
// belongs. It is NOT the per-operation substrate authority on the sealed Hub legs
// (OWD-6/AI-11 still refuse a reusable bearer there); see the credentialing posture.
// Signing/verification use golang-jwt/jwt/v5 (the internal/accountsvc pattern,
// WithValidMethods alg-pinning); no hand-rolled JWS.
// Client auth is private_key_jwt (asymmetric, preferred) or — for partner servers
// that cannot issue asymmetric credentials — client_secret_post (Config.ClientSecret);
// exactly one mode per Config.
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
	"strconv"
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
	TokenURL string            // SMART token endpoint
	ClientID string            // registered client id (iss/sub of the assertion)
	Scope    string            // requested scope, e.g. "system/*.read"
	Alg      string            // "ES384" | "RS384"
	Key      crypto.PrivateKey // *ecdsa.PrivateKey (ES384) | *rsa.PrivateKey (RS384)
	KID      string            // optional JWS header kid
	// ClientSecret switches the grant to client_secret_post (OAuth2
	// client_credentials with client_id+client_secret in the form body) for
	// authorization servers that cannot issue asymmetric credentials. Mutually
	// exclusive with Key/Alg/KID; private_key_jwt is the preferred mode.
	ClientSecret string
	AssertionTTL time.Duration    // default 5m
	RefreshSkew  time.Duration    // re-mint when within this of expiry; default 60s
	Clock        func() time.Time // default time.Now
	HTTPClient   *http.Client     // default a 10s-timeout client
	// Observer, when set, receives one redaction-safe note per TokenSource the
	// first time the authorization server departs from RFC 6749 in a way the
	// client absorbs (today: expires_in sent as a JSON string). The departure is
	// read as sent either way; the note is how an operator learns the peer is
	// out of specification instead of that being silently absorbed forever.
	// nil ⇒ silent.
	Observer func(note string)
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

// validateMode enforces exactly one client-auth mode: private_key_jwt (Key+Alg,
// preferred) or client_secret_post (ClientSecret).
func (c Config) validateMode() error {
	hasJWT := c.Key != nil || c.Alg != "" || c.KID != ""
	switch {
	case hasJWT && c.ClientSecret != "":
		return fmt.Errorf("smartauth: configure private_key_jwt (Key+Alg) or ClientSecret, not both")
	case c.ClientSecret != "":
		return nil
	case c.Key == nil:
		return fmt.Errorf("smartauth: Key required (or set ClientSecret for the client_secret_post grant)")
	default:
		_, err := signingMethod(c.Alg)
		return err
	}
}

// TokenSource mints, caches, and re-mints a SMART Backend Services bearer token.
// Token() is concurrency-safe and FAIL-CLOSED: any failure returns an error and
// never a stale/empty token.
type TokenSource struct {
	Config
	mu        sync.Mutex
	acquiring chan struct{}
	cached    string
	exp       time.Time
	noted     sync.Once // Observer fires once per TokenSource, not per refresh
}

// Token returns a bearer valid for at least RefreshSkew, minting a fresh one if
// the cache is empty or near expiry.
func (s *TokenSource) Token(ctx context.Context) (string, error) {
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		s.mu.Lock()
		if s.acquiring == nil {
			s.acquiring = make(chan struct{})
			s.mu.Unlock()
			break
		}
		done := s.acquiring
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-done:
		}
	}
	defer func() { s.mu.Lock(); close(s.acquiring); s.acquiring = nil; s.mu.Unlock() }()
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
	var form url.Values
	if s.ClientSecret != "" {
		// client_secret_post: id+secret in the form body. client_secret_basic
		// (HTTP Basic) is deliberately not offered — additive later if a partner
		// needs it.
		form = url.Values{
			"grant_type":    {"client_credentials"},
			"client_id":     {s.ClientID},
			"client_secret": {s.ClientSecret},
		}
	} else {
		assertion, err := s.assertion()
		if err != nil {
			return "", 0, err
		}
		form = url.Values{
			"grant_type":            {"client_credentials"},
			"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
			"client_assertion":      {assertion},
		}
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
		return "", 0, &TokenTransportError{Cause: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenBody+1))
	if err != nil {
		return "", 0, &TokenTransportError{Cause: err}
	}
	if resp.StatusCode/100 != 2 {
		return "", 0, &TokenEndpointError{StatusCode: resp.StatusCode}
	}
	if len(body) > maxTokenBody {
		return "", 0, fmt.Errorf("smartauth: oversized token response")
	}
	var tr struct {
		AccessToken string    `json:"access_token"`
		ExpiresIn   expiresIn `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", 0, fmt.Errorf("smartauth: decode token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", 0, fmt.Errorf("smartauth: empty access_token")
	}
	if tr.ExpiresIn.fromString && s.Observer != nil {
		s.noted.Do(func() {
			s.Observer(fmt.Sprintf("smartauth: token endpoint %s returned expires_in as a JSON string; RFC 6749 §5.1 defines it as a number (read as sent)", redactTokenURL(s.TokenURL)))
		})
	}
	ttl = time.Duration(tr.ExpiresIn.seconds) * time.Second
	if ttl <= 0 {
		ttl = defaultAssertionTTL // conservative when the server omits expires_in
	}
	return tr.AccessToken, ttl, nil
}

// expiresIn is the token response's expires_in lifetime in seconds. RFC 6749
// defines it as a number, but real authorization servers answer it as a JSON
// string too (Azure AD's v1 issuer, met behind a payer's token proxy, sends
// "expires_in":"3599"); the partner's bytes are read as sent and the departure
// is reported once through Config.Observer. A number or a numeric string is
// the lifetime; absent or null is zero (the caller applies the default TTL);
// anything else is a decode error naming the field.
type expiresIn struct {
	seconds    float64
	fromString bool
}

func (e *expiresIn) UnmarshalJSON(b []byte) error {
	raw := strings.TrimSpace(string(b))
	if raw == "null" {
		*e = expiresIn{}
		return nil
	}
	quoted := len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"'
	if quoted {
		raw = strings.TrimSpace(raw[1 : len(raw)-1])
	}
	n, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return fmt.Errorf("expires_in: not a number of seconds: %q", string(b))
	}
	*e = expiresIn{seconds: n, fromString: quoted}
	return nil
}

// redactTokenURL keeps scheme and host of the token endpoint for a note and
// drops path, query and userinfo (a token URL can carry a tenant id in its path).
func redactTokenURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(unparseable)"
	}
	return u.Scheme + "://" + u.Host
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
