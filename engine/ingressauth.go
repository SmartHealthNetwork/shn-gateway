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
// Bearers are ES384, 5-min TTL, signed with the key the IngressKeyStore hands out and
// carrying its kid so any replica behind the same store can resolve the verification
// key. The no-DSN default store is one process-local key (a restart makes clients
// re-fetch), which is correct only at a single reachable instance — New says so once
// in the log at boot. The client_assertion jti is one-time-use through the ReplayStore,
// so the guard holds across replicas behind a shared store as well.
//
// These routes are PUBLIC once cloud exposure lands — no debug surface,
// return generic errors — never raw JWT/crypto detail in a response body.
package engine

import (
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	ingressTokenPath = "/oauth/token"
	// IngressBearerTTL is the lifetime of an issued ingress bearer. Exported so a
	// shared key store can size a key's verification life from it.
	IngressBearerTTL = 5 * time.Minute
	ingressJTIWindow = 5 * time.Minute
	ingressScope     = "system/Davinci.write"
	assertionType    = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"

	// maxAssertionLifetime caps the client_assertion lifetime (RFC 7523 / SMART
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
	baseURL string                               // config-pinned; aud (never request-derived)
	clients map[string]IngressClientRegistration // client_id → registration
	pubKeys map[string]any                       // client_id → *ecdsa/*rsa PublicKey
	keys    IngressKeyStore                      // signs issued bearers; resolves kid → public key
	replay  ReplayStore                          // one-time-use on the ASSERTION jti (ReplayScopeIngressJTI)
	now     func() time.Time
	// storeErr counts a shared-state store failure (Config.StoreErrorMetric, via
	// Gateway.noteStoreError). Set by New; nil in a bare-constructed server, and
	// every call site is nil-guarded.
	storeErr func(store string)
}

// noteStoreError counts one store failure on the token endpoint. Nil-safe.
func (s *ingressAuthServer) noteStoreError(store string) {
	if s.storeErr != nil {
		s.storeErr(store)
	}
}

func newIngressAuthServer(baseURL string, clients map[string]IngressClientRegistration, now func() time.Time, keys IngressKeyStore, replay ReplayStore) (*ingressAuthServer, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("ingress auth: baseURL (aud) required")
	}
	if len(clients) == 0 {
		return nil, fmt.Errorf("ingress auth: at least one registered client required")
	}
	if keys == nil || replay == nil {
		return nil, fmt.Errorf("ingress auth: key store and replay store required")
	}
	if now == nil {
		now = time.Now
	}
	s := &ingressAuthServer{
		baseURL: strings.TrimRight(baseURL, "/"),
		clients: clients,
		pubKeys: map[string]any{},
		keys:    keys,
		replay:  replay,
		now:     now,
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
// RFC 6749: token-error responses MUST carry Cache-Control: no-store.
func oauthErr(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": desc})
}

// oauthUnavailable is the token endpoint's 503: a shared-store dependency could not be
// reached, so this replica cannot complete an issuance it would otherwise complete. 503
// (not 500) tells the client to retry, and the retry carries a FRESH client_assertion:
// a 503 never returns a token, but the one that follows a failed one-time-use record
// write cannot prove the record was not written (the write can commit and its
// acknowledgment be lost), so re-presenting the same assertion may be refused as
// replayed. Every refusal decided before that write leaves the assertion unspent, which
// is why the record is the endpoint's last step. no-store keeps a cache from pinning the
// outage (oauthErr sets it too; stated here so the header is not an accident of the
// helper).
func oauthUnavailable(w http.ResponseWriter, desc string) {
	w.Header().Set("Cache-Control", "no-store")
	oauthErr(w, http.StatusServiceUnavailable, "server_error", desc)
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
	// (RFC 7523 / SMART Backend Services). WithExpirationRequired guarantees exp is
	// present and non-nil after a successful parse.
	expTime, err := claims.GetExpirationTime()
	if err != nil || expTime == nil || expTime.Time.After(s.now().Add(maxAssertionLifetime)) {
		oauthErr(w, http.StatusUnauthorized, "invalid_client", "assertion lifetime too long")
		return
	}
	jtiVal, _ := claims["jti"].(string)
	// One-time-use on the ASSERTION jti, per client_id (RFC 7523 §3), through the
	// shared record — so the guard holds across replicas, not just in this process.
	// NOT the issued bearer: that stays reusable within its lifetime.
	// replay==true ⇒ reject.
	now := s.now()
	if jtiVal == "" {
		oauthErr(w, http.StatusUnauthorized, "invalid_client", "missing or replayed jti")
		return
	}
	if len(jtiVal) > MaxReplayKeyBytes {
		// Refused as a bad credential, BEFORE the record is consulted: an oversized jti is
		// the client's own field, and letting it reach the store would turn it into a 503
		// (and a counted store error) on a perfectly healthy database.
		oauthErr(w, http.StatusUnauthorized, "invalid_client", "invalid jti")
		return
	}
	// ORDER IS LOAD-BEARING: EVERYTHING that can refuse this request runs before the
	// one-time-use record is written, and the record is the last step before the response.
	// A refusal raised after the record would answer on a credential already spent, so
	// the endpoint resolves the signing key, checks the scope and produces the signature
	// FIRST, into a local, and only then spends the jti — every refusal this endpoint can
	// decide for itself therefore leaves the assertion untouched. The record's own write is the one step
	// that cannot be moved earlier, and a failure THERE is ambiguous by nature (see
	// below): that is why the client's rule is one fresh assertion per request rather
	// than a retry of the one it already sent. Nothing is on the wire until the record has
	// been written, so CheckAndRecord remains the single atomic gate against a concurrent
	// replica: a replayed assertion costs one wasted signature and is still refused here.
	kid, key, kerr := s.keys.SigningKey(now)
	if kerr != nil {
		// The signing key lives in the shared store; without it no replica can issue.
		// Nothing has been recorded here, so this same assertion is still valid — but the
		// client's rule on ANY 503 from this endpoint is the same one rule: retry with a
		// FRESH client_assertion (one per request, the private_key_jwt norm). Re-sending
		// this one merely also happens to work.
		s.noteStoreError(storeErrIngressKey)
		oauthUnavailable(w, "signing key unavailable")
		return
	}
	scope := r.FormValue("scope")
	if scope != "" && !scopeAllowed(scope, reg.Scopes) {
		// A bad request, not a spent credential: the client fixes the scope and retries
		// with the assertion it already holds.
		oauthErr(w, http.StatusBadRequest, "invalid_scope", "scope not allowed")
		return
	}
	bearer, serr := signBearer(kid, key, clientID, scope, s.baseURL, now)
	if serr != nil {
		// Signing failed with a key already in hand — a key row this build cannot sign
		// with, not an unreachable store, so nothing is counted against a store. Reachable
		// on a shared key table with more than one writer, which is exactly why it runs
		// before the record: nothing is spent here. As above, the client's rule is still a
		// FRESH assertion on any 503; the one it already holds also still works here.
		oauthUnavailable(w, "token could not be signed")
		return
	}
	replayed, rerr := s.replay.CheckAndRecord(ReplayScopeIngressJTI, clientID, jtiVal, now, now.Add(ingressJTIWindow))
	if rerr != nil {
		// The record could not be CONFIRMED — which is not the same as "not written". The
		// statement may have committed and only its acknowledgment been lost (a dropped
		// connection, a deadline fired while the commit was on its way back), so this jti
		// may or may not be spent; a store error never proves it was not.
		//
		// The assertion is not known to be replayed, so 401 would accuse an honest client
		// of a bad credential; answer the same retryable 503 the signing key answers. NO
		// token goes out: the bearer signed above is discarded. The client's retry carries
		// a FRESH client_assertion — a retry that re-sends THIS one may be refused as
		// replayed, and that refusal is correct, not a broken promise. Rows:
		// TestIngressToken_AckLostRecordWrite_FreshAssertionIsTheRetry (here) and the
		// Postgres twin in connectors/pgstore.
		s.noteStoreError(storeErrReplay)
		oauthUnavailable(w, "one-time-use record unavailable")
		return
	}
	if replayed {
		oauthErr(w, http.StatusUnauthorized, "invalid_client", "missing or replayed jti")
		return
	}
	// RFC 6749: successful token responses MUST carry Cache-Control: no-store.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": bearer, "token_type": "bearer",
		"expires_in": int(IngressBearerTTL.Seconds()), "scope": scope,
	})
}

// signBearer signs a bearer with an ALREADY-RESOLVED signing key and stamps its kid in
// the header, so a sibling replica behind the same store can resolve the verification key
// for it. Pure CPU: the key is passed in and the result is returned to the caller, which
// signs into a local BEFORE it writes the one-time-use record — so neither a key-store
// outage nor a key this build cannot sign with can consume the client's assertion.
func signBearer(kid string, key *ecdsa.PrivateKey, clientID, scope, aud string, now time.Time) (string, error) {
	tok := jwt.NewWithClaims(jwt.SigningMethodES384, jwt.MapClaims{
		"client_id": clientID, "scope": scope, "aud": aud,
		"iat": now.Unix(), "exp": now.Add(IngressBearerTTL).Unix(),
	})
	tok.Header["kid"] = kid
	return tok.SignedString(key)
}

func scopeAllowed(requested string, allowed []string) bool {
	for _, a := range allowed {
		if a == requested {
			return true
		}
	}
	return false
}

// verifyBearer checks the Authorization bearer against the key store's public key
// for the bearer's kid, ES384-pinned, config-pinned aud. The alg and kid shape are
// checked BEFORE the store is consulted so a forged header cannot drive lookups;
// an unknown kid, a store error or a timeout rejects. No one-time-use: a bearer is
// reusable within its lifetime (standard SMART Backend Services).
//
// The second return says WHY a false refused (the shape verifyHubAssertion uses):
// unavailable=true means the KEY STORE could not tell — its backing store errored or
// timed out — so the route owes a retryable 503 rather than the 401 that reads as a bad
// credential. The refusal itself is the same either way: a bearer whose kid cannot be
// resolved is never admitted. The outage is counted here, at the one place that knows a
// store call failed.
func (s *ingressAuthServer) verifyBearer(r *http.Request) (ok bool, unavailable bool) {
	h := r.Header.Get("Authorization")
	// Strict canonical casing is intentional: SMART Backend Services clients send
	// canonical "Bearer "; the case-insensitive variant isn't worth the cost (mirrors
	// the smartauthproxy sister).
	if !strings.HasPrefix(h, "Bearer ") {
		return false, false
	}
	raw := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	storeDown := false
	_, err := jwt.Parse(raw, func(tok *jwt.Token) (any, error) {
		if tok.Method.Alg() != jwt.SigningMethodES384.Alg() {
			return nil, fmt.Errorf("alg")
		}
		kid, _ := tok.Header["kid"].(string)
		if !validKID(kid) {
			return nil, fmt.Errorf("kid")
		}
		pub, found, kerr := s.keys.VerificationKey(kid, s.now())
		if kerr != nil {
			// A well-formed kid the store could not look up. Distinguished from an
			// unknown kid: this one is an outage, and only the caller of the store can
			// say so. (A malformed kid never gets here, so this cannot be driven by a
			// forged header.)
			storeDown = true
			return nil, fmt.Errorf("key store unavailable: %w", kerr)
		}
		// IngressKeyStore is a public seam: a third-party store answering
		// (nil, true) must reject like an unknown kid, never reach ecdsa.Verify.
		if !found || pub == nil {
			return nil, fmt.Errorf("unknown kid")
		}
		return pub, nil
	},
		jwt.WithValidMethods([]string{"ES384"}),
		jwt.WithExpirationRequired(),
		jwt.WithAudience(s.baseURL),
		jwt.WithTimeFunc(s.now))
	if err != nil && storeDown {
		s.noteStoreError(storeErrIngressKey)
		return false, true
	}
	return err == nil, false
}

// audUnder reports whether aud is the config base itself or a path strictly under it.
// PATH-BOUNDARY-SAFE: the base+"/" check rejects the suffix-of-authority bypass — for
// base "https://x:8080", aud "https://x:8080.evil.com/y" must NOT pass (a naive
// strings.HasPrefix(aud, base) would let it through). Empty aud never passes.
// FR-G28 UDAP B2B.
func audUnder(aud, base string) bool {
	return aud != "" && (aud == base || strings.HasPrefix(aud, base+"/"))
}

// verifyDirectBearer implements the UDAP B2B direct-bearer path (per the UDAP
// B2B ingress-auth design): a CONFIG-registered
// client's self-signed private_key_jwt presented DIRECTLY as the Authorization bearer (the
// form br-provider's BFF sends), verified per-call against the registered key. Distinct
// from verifyBearer (the gateway's OWN ephemeral ES384 bearer); the two are token-shape
// DISJOINT (an issued bearer carries no iss), so the OR in ingressAuthOK cannot fail open.
// It touches no shared store, so it keeps answering through a key-store outage.
// Authority is edge-only (never reaches authorize()); org-level TPO; scope is advisory and
// NOT enforced on either path. A registered client implicitly gains both auth modes.
func (s *ingressAuthServer) verifyDirectBearer(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return false
	}
	raw := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))

	// Peek iss (unverified) → select the registered key + alg. An issued ephemeral bearer
	// has no iss, so it fails here — keeping the two paths disjoint.
	var unv jwt.MapClaims
	if _, _, err := jwt.NewParser().ParseUnverified(raw, &unv); err != nil {
		return false
	}
	clientID, _ := unv["iss"].(string)
	if clientID == "" {
		return false
	}
	reg, ok := s.clients[clientID]
	pub := s.pubKeys[clientID]
	if !ok || pub == nil {
		return false
	}

	// alg PINNED to the registration (no alg confusion); exp required; signature vs the
	// registered key. aud is NOT checked by the parser (jwt.WithAudience is exact-match and
	// br-provider's aud varies per endpoint) — audUnder below does the path-boundary pin.
	claims := jwt.MapClaims{}
	if _, err := jwt.NewParser(
		jwt.WithValidMethods([]string{reg.Alg}),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(s.now),
	).ParseWithClaims(raw, claims, func(*jwt.Token) (any, error) { return pub, nil }); err != nil {
		return false
	}
	// sub is OPTIONAL: br-provider's CDS client JWT omits it (verified against the real
	// token — it emits iss/aud/exp/iat/jti only). RFC 7523 client auth identifies the
	// client by iss, which we verify against the REGISTERED key; an absent sub is accepted,
	// but a PRESENT sub that differs from iss is rejected (no identity confusion).
	if sub, present := claims["sub"].(string); present && sub != clientID {
		return false
	}
	// aud path-boundary-pinned to the config base (never r.Host). At least one aud entry
	// must be the base or a path under it.
	auds, err := claims.GetAudience()
	if err != nil {
		return false
	}
	audOK := false
	for _, a := range auds {
		if audUnder(a, s.baseURL) {
			audOK = true
			break
		}
	}
	if !audOK {
		return false
	}
	// exp capped to <= now + maxAssertionLifetime (same as the assertion path).
	expTime, err := claims.GetExpirationTime()
	if err != nil || expTime == nil || expTime.Time.After(s.now().Add(maxAssertionLifetime)) {
		return false
	}
	// jti PRESENCE required but NOT one-time-use: a presented bearer is reusable within its
	// exp, matching the replayable issued bearer (single-use would wedge
	// br-provider's discovery-GET-then-hook-POST with one JWT and buys no security over the
	// existing replayable-bearer posture).
	if jtiVal, _ := claims["jti"].(string); jtiVal == "" {
		return false
	}
	return true
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
