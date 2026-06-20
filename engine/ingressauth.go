// gateway/engine/ingressauth.go
//
// ingressauth.go — the gateway-hosted SMART Backend Services authorization server +
// bearer verifier for the DaVinciIngress. The gateway is BOTH the authorization server
// (issues bearers at /oauth/token) and the resource server (verifies them on every
// ingress route). This is a gateway-EDGE credential — it is NOT substrate authority:
// authorize() (gateway.go) mints the per-leg token + Hub assertion from the gateway's
// OWN identity and never reads the inbound Authorization header (AI-11/OWD-6; same
// boundary as gateway/connectors/smartauth, inbound twin).
//
// Bearers are signed with an EPHEMERAL in-process ES384 key (5-min TTL; a restart makes
// clients re-fetch). That is sound ONLY while the ingress is single-reachable-instance:
// cloud public exposure is a planned future enhancement, and the shared-key/JWKS +
// shared-jti requirement rides that future public-ingress slice.
//
// These routes are PUBLIC once cloud exposure lands — no debug surface,
// return generic errors — never raw JWT/crypto detail in a response body.
package engine

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"github.com/golang-jwt/jwt/v5"
)

const (
	ingressTokenPath = "/oauth/token"
	ingressBearerTTL = 5 * time.Minute
	ingressJTIWindow = 5 * time.Minute
	ingressJTIMax    = 4096
	ingressScope     = "system/Davinci.write"
	assertionType    = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"

	// maxAssertionLifetime caps the client_assertion lifetime (RFC 7523 §3 / SMART
	// Backend Services recommends <= 5 min). It MUST be <= ingressJTIWindow so a jti is
	// remembered for the assertion's entire valid life — otherwise a long-lived assertion
	// could be replayed after the jti window evicts it.
	maxAssertionLifetime = 5 * time.Minute
)

// IngressClientRegistration is a config-registered inbound client (a provider EHR)
// the ingress trusts: its public key + permitted scopes. Registration is config-based;
// UDAP dynamic client registration is a planned future enhancement.
type IngressClientRegistration struct {
	Alg          string   // "ES384" | "RS384"
	PublicKeyPEM []byte   // PEM SubjectPublicKeyInfo
	Scopes       []string // permitted scopes
}

type ingressAuthServer struct {
	baseURL   string                               // config-pinned; aud (never request-derived)
	clients   map[string]IngressClientRegistration // client_id → registration
	pubKeys   map[string]any                       // client_id → *ecdsa/*rsa PublicKey
	bearerKey *ecdsa.PrivateKey                    // ephemeral ES384, self-signed bearers
	jti       *shnsdk.ReplayGuard                  // one-time-use on the ASSERTION jti
	now       func() time.Time
}

func newIngressAuthServer(baseURL string, clients map[string]IngressClientRegistration, now func() time.Time) (*ingressAuthServer, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("ingress auth: baseURL (aud) required")
	}
	if len(clients) == 0 {
		return nil, fmt.Errorf("ingress auth: at least one registered client required")
	}
	if now == nil {
		now = time.Now
	}
	bk, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("ingress auth: bearer key: %w", err)
	}
	s := &ingressAuthServer{
		baseURL:   strings.TrimRight(baseURL, "/"),
		clients:   clients,
		pubKeys:   map[string]any{},
		bearerKey: bk,
		jti:       shnsdk.NewReplayGuard(ingressJTIWindow, ingressJTIMax),
		now:       now,
	}
	for id, reg := range clients {
		var (
			pk  any
			err error
		)
		switch reg.Alg {
		case "ES384":
			pk, err = jwt.ParseECPublicKeyFromPEM(reg.PublicKeyPEM)
		case "RS384":
			pk, err = jwt.ParseRSAPublicKeyFromPEM(reg.PublicKeyPEM)
		default:
			err = fmt.Errorf("client %q: unsupported alg %q", id, reg.Alg)
		}
		if err != nil {
			return nil, fmt.Errorf("ingress auth: registration %q: %w", id, err)
		}
		s.pubKeys[id] = pk
	}
	return s, nil
}

func (s *ingressAuthServer) tokenURL() string { return s.baseURL + ingressTokenPath }

// oauthErr writes a generic OAuth2 error — never an internal jwt/crypto detail.
// RFC 6749 §5.2: token-error responses MUST carry Cache-Control: no-store.
func oauthErr(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": desc})
}

func (s *ingressAuthServer) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		oauthErr(w, http.StatusMethodNotAllowed, "invalid_request", "POST required")
		return
	}
	if err := r.ParseForm(); err != nil {
		oauthErr(w, http.StatusBadRequest, "invalid_request", "bad form")
		return
	}
	if r.FormValue("grant_type") != "client_credentials" {
		oauthErr(w, http.StatusBadRequest, "unsupported_grant_type", "want client_credentials")
		return
	}
	if r.FormValue("client_assertion_type") != assertionType {
		oauthErr(w, http.StatusBadRequest, "invalid_request", "bad client_assertion_type")
		return
	}
	assertion := r.FormValue("client_assertion")

	// Peek the issuer (unverified) to select the registered key + alg.
	var unv jwt.MapClaims
	if _, _, err := jwt.NewParser().ParseUnverified(assertion, &unv); err != nil {
		oauthErr(w, http.StatusUnauthorized, "invalid_client", "unparseable assertion")
		return
	}
	clientID, _ := unv["iss"].(string)
	reg, ok := s.clients[clientID]
	pub := s.pubKeys[clientID]
	if !ok || pub == nil {
		oauthErr(w, http.StatusUnauthorized, "invalid_client", "unknown client")
		return
	}

	claims := jwt.MapClaims{}
	_, err := jwt.NewParser(
		jwt.WithValidMethods([]string{reg.Alg}), // alg pinned from registration
		jwt.WithExpirationRequired(),
		jwt.WithAudience(s.tokenURL()), // CONFIG-pinned aud, never r.Host
		jwt.WithTimeFunc(s.now),
	).ParseWithClaims(assertion, claims, func(*jwt.Token) (any, error) { return pub, nil })
	if err != nil {
		oauthErr(w, http.StatusUnauthorized, "invalid_client", "assertion verification failed")
		return
	}
	if sub, _ := claims["sub"].(string); sub != clientID {
		oauthErr(w, http.StatusUnauthorized, "invalid_client", "iss != sub")
		return
	}
	// Cap assertion lifetime: exp must not be more than maxAssertionLifetime past now
	// (RFC 7523 §3 / SMART Backend Services). WithExpirationRequired guarantees exp is
	// present and non-nil after a successful parse.
	expTime, err := claims.GetExpirationTime()
	if err != nil || expTime == nil || expTime.Time.After(s.now().Add(maxAssertionLifetime)) {
		oauthErr(w, http.StatusUnauthorized, "invalid_client", "assertion lifetime too long")
		return
	}
	jtiVal, _ := claims["jti"].(string)
	// One-time-use on the ASSERTION jti (NOT the issued bearer — the bearer is
	// reusable within its lifetime). replay==true ⇒ reject.
	if jtiVal == "" || s.jti.CheckAndRecord(jtiVal, s.now()) {
		oauthErr(w, http.StatusUnauthorized, "invalid_client", "missing or replayed jti")
		return
	}
	scope := r.FormValue("scope")
	if scope != "" && !scopeAllowed(scope, reg.Scopes) {
		oauthErr(w, http.StatusBadRequest, "invalid_scope", "scope not allowed")
		return
	}

	bearer, err := s.issueBearer(clientID, scope)
	if err != nil {
		oauthErr(w, http.StatusInternalServerError, "server_error", "issue bearer")
		return
	}
	// RFC 6749 §5.1: successful token responses MUST carry Cache-Control: no-store.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": bearer, "token_type": "bearer",
		"expires_in": int(ingressBearerTTL.Seconds()), "scope": scope,
	})
}

func (s *ingressAuthServer) issueBearer(clientID, scope string) (string, error) {
	now := s.now()
	return jwt.NewWithClaims(jwt.SigningMethodES384, jwt.MapClaims{
		"client_id": clientID, "scope": scope, "aud": s.baseURL,
		"iat": now.Unix(), "exp": now.Add(ingressBearerTTL).Unix(),
	}).SignedString(s.bearerKey)
}

func scopeAllowed(requested string, allowed []string) bool {
	for _, a := range allowed {
		if a == requested {
			return true
		}
	}
	return false
}

// verifyBearer checks the Authorization bearer against the server's own (ephemeral)
// signing key, ES384-pinned, config-pinned aud. No one-time-use: a bearer is reusable
// within its 5-min lifetime (standard SMART Backend Services).
func (s *ingressAuthServer) verifyBearer(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	// Strict canonical casing is intentional: SMART Backend Services clients send
	// canonical "Bearer "; the case-insensitive variant isn't worth the cost (mirrors
	// the smartauthproxy sister).
	if !strings.HasPrefix(h, "Bearer ") {
		return false
	}
	raw := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	_, err := jwt.Parse(raw, func(*jwt.Token) (any, error) { return s.bearerKey.Public(), nil },
		jwt.WithValidMethods([]string{"ES384"}),
		jwt.WithExpirationRequired(),
		jwt.WithAudience(s.baseURL),
		jwt.WithTimeFunc(s.now))
	return err == nil
}

func (s *ingressAuthServer) handleSmartConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token_endpoint":                                   s.tokenURL(),
		"grant_types_supported":                            []string{"client_credentials"},
		"token_endpoint_auth_methods_supported":            []string{"private_key_jwt"},
		"token_endpoint_auth_signing_alg_values_supported": []string{"ES384", "RS384"},
		"scopes_supported":                                 []string{ingressScope},
	})
}
