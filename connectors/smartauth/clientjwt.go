package smartauth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// CDS Hooks client authentication ("Trusting CDS Clients", CDS Hooks 2.0):
// every call to a CDS service that requires authentication carries
// Authorization: Bearer <JWT>, a JWT the client signs itself with the private
// key of a key pair whose public half it publishes as a JWK Set. Da Vinci CRD
// adopts these rules by reference. This file is that client: a transport that
// signs a fresh JWT for each request it sends, and the public JWK Set of its key.

// CDSClientJWKSPath is the path, under a gateway's own public base, at which it
// serves the public JWK Set of its CDS client key. It is not under
// /.well-known: the key set at /.well-known/jwks.json is conventionally an
// authorization server's, a different key, and a well-known name is
// registered (RFC 8615).
const CDSClientJWKSPath = "/cds-client/jwks.json"

// CDSClientJWKSURL is the URL of the JWK Set a gateway whose public base is
// base serves: the one string the jku header, the gateway's boot line and SHN
// Cloud's tenant page all name. base is an absolute http(s) URL with no query
// or fragment; a trailing slash is dropped.
func CDSClientJWKSURL(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", fmt.Errorf("smartauth: %q is not a base URL a JWK Set can be served under", base)
	}
	return strings.TrimRight(base, "/") + CDSClientJWKSPath, nil
}

// CDSClientJWTLifetime is how long a CDS client JWT is valid: exp is iat plus
// this. CDS Hooks sets no maximum; a JWT is minted for one request and sent at
// once, so it needs only to outlast the request's delivery and the service's
// clock skew.
const CDSClientJWTLifetime = 5 * time.Minute

// ErrCDSClientIssuerUnknown is a CDS request that was not sent: the client JWT
// it must carry names its issuer, and none is known yet. The request is never
// sent with another credential instead.
var ErrCDSClientIssuerUnknown = errors.New("smartauth: CDS client JWT not sent: no issuer is known (set PAYER_DAVINCI_CDS_JWT_ISS, PAYER_DAVINCI_INGRESS_BASE_URL, or wait for this gateway's own registration on the network feed)")

// ClientJWTConfig is how a CDS client's JWTs are issued and sent; the key they
// are signed with is a CDSClientKey (NewCDSClientKey).
type ClientJWTConfig struct {
	// Issuer is the iss of each JWT, read for each request: ok false is no
	// issuer known yet, and the request is not sent (ErrCDSClientIssuerUnknown).
	Issuer func() (iss string, ok bool)
	// JKU is the jku header, read for each request: "" omits it.
	JKU func() string
	// Transport sends the signed request. Nil is http.DefaultTransport.
	Transport http.RoundTripper
	// Now is the clock iat and exp are read from. Nil is time.Now.
	Now func() time.Time
	// Rand is the source of each jti. Nil is crypto/rand.
	Rand io.Reader
}

// CDSClientKey is a validated CDS client signing key: its algorithm, key id and
// public JWK Set.
type CDSClientKey struct {
	alg    string
	method jwt.SigningMethod
	key    crypto.PrivateKey
	kid    string
	jwks   []byte
}

// NewCDSClientKey validates key as alg's kind and builds its public JWK Set,
// whose one key is named kid (its thumbprint when kid is empty). The set is
// checked to hold public members only before it is returned.
func NewCDSClientKey(alg string, key crypto.PrivateKey, kid string) (*CDSClientKey, error) {
	method, err := signingMethod(alg)
	if err != nil {
		return nil, err
	}
	var pub crypto.PublicKey
	switch k := key.(type) {
	case *ecdsa.PrivateKey:
		if alg != "ES384" || k.Curve != elliptic.P384() {
			return nil, fmt.Errorf("smartauth: alg %s does not take a P-384 EC key", alg)
		}
		pub = k.Public()
	case *rsa.PrivateKey:
		if alg == "ES384" {
			return nil, fmt.Errorf("smartauth: alg ES384 needs a P-384 EC key, got RSA")
		}
		pub = k.Public()
	default:
		return nil, fmt.Errorf("smartauth: unsupported CDS client key %T", key)
	}
	jwk, thumb, err := publicJWK(pub, alg)
	if err != nil {
		return nil, err
	}
	if kid == "" {
		kid = thumb
	}
	jwk.Kid = kid
	set, err := json.Marshal(struct {
		Keys []publicJWKMembers `json:"keys"`
	}{[]publicJWKMembers{jwk}})
	if err != nil {
		return nil, fmt.Errorf("smartauth: JWK Set: %w", err)
	}
	if err := CheckPublicJWKSet(set); err != nil {
		return nil, err
	}
	return &CDSClientKey{alg: alg, method: method, key: key, kid: kid, jwks: set}, nil
}

// KID is the key's id, as its JWK Set and every JWT header name it.
func (k *CDSClientKey) KID() string { return k.kid }

// Alg is the key's JWS algorithm.
func (k *CDSClientKey) Alg() string { return k.alg }

// JWKSet is the public JWK Set (RFC 7517) of the key: one key, public members only.
func (k *CDSClientKey) JWKSet() []byte { return append([]byte(nil), k.jwks...) }

// publicJWKMembers is the only shape the served JWK Set is built from: a key's
// public members, its id and its use. It has no field for a private member.
type publicJWKMembers struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	Crv string `json:"crv,omitempty"`
	X   string `json:"x,omitempty"`
	Y   string `json:"y,omitempty"`
	N   string `json:"n,omitempty"`
	E   string `json:"e,omitempty"`
}

// publicJWK is pub's public members and its RFC 7638 thumbprint, built as the
// hosted control plane builds the JWK it gives a payer for the same key.
func publicJWK(pub crypto.PublicKey, alg string) (publicJWKMembers, string, error) {
	b64 := base64.RawURLEncoding.EncodeToString
	var j publicJWKMembers
	var canonical string
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		size := (k.Curve.Params().BitSize + 7) / 8
		j = publicJWKMembers{Kty: "EC", Crv: "P-384", X: b64(k.X.FillBytes(make([]byte, size))), Y: b64(k.Y.FillBytes(make([]byte, size)))}
		canonical = fmt.Sprintf(`{"crv":%q,"kty":"EC","x":%q,"y":%q}`, j.Crv, j.X, j.Y)
	case *rsa.PublicKey:
		j = publicJWKMembers{Kty: "RSA", N: b64(k.N.Bytes()), E: b64(big.NewInt(int64(k.E)).Bytes())}
		canonical = fmt.Sprintf(`{"e":%q,"kty":"RSA","n":%q}`, j.E, j.N)
	default:
		return publicJWKMembers{}, "", fmt.Errorf("smartauth: unsupported public key %T", pub)
	}
	sum := sha256.Sum256([]byte(canonical))
	j.Alg, j.Use = alg, "sig"
	return j, b64(sum[:]), nil
}

// privateJWKMembers are the JWK members that carry private key material
// (RFC 7518 §6.2.2, §6.3.2 and §6.4.1): an EC or RSA private key's, the
// RSA primes' and CRT values', and a symmetric key's.
var privateJWKMembers = []string{"d", "p", "q", "dp", "dq", "qi", "oth", "k"}

// CheckPublicJWKSet refuses a JWK Set any of whose keys carries a private
// member, or that is not a JWK Set at all. A set the gateway serves passes it
// first.
func CheckPublicJWKSet(set []byte) error {
	var parsed struct {
		Keys []map[string]json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(set, &parsed); err != nil {
		return fmt.Errorf("smartauth: not a JWK Set: %w", err)
	}
	if parsed.Keys == nil {
		return errors.New("smartauth: not a JWK Set: no keys member")
	}
	for i, k := range parsed.Keys {
		for _, m := range privateJWKMembers {
			if _, ok := k[m]; ok {
				return fmt.Errorf("smartauth: JWK Set key %d carries the private member %q: it is never served", i, m)
			}
		}
	}
	return nil
}

// NewClientJWTHTTPClient returns an *http.Client whose every request carries a
// CDS client JWT signed with key: Authorization: Bearer <JWT>. Each request it
// sends, every redirect hop included, gets a JWT of its own: a new jti, and
// aud the exact URL that request is sent to. A 401 is the service's answer
// and is not sent again: a fresh JWT from the same key cannot change it. A
// hop to another origin, and every later hop of its chain, is sent without
// it, as NewHTTPClient's bearer is.
func NewClientJWTHTTPClient(key *CDSClientKey, cfg ClientJWTConfig) (*http.Client, error) {
	if key == nil {
		return nil, errors.New("smartauth: CDS client key required")
	}
	if cfg.Issuer == nil {
		return nil, errors.New("smartauth: CDS client issuer required")
	}
	t := &clientJWTTransport{key: key, issuer: cfg.Issuer, jku: cfg.JKU, base: cfg.Transport, now: cfg.Now, rand: cfg.Rand}
	if t.base == nil {
		t.base = http.DefaultTransport
	}
	if t.now == nil {
		t.now = time.Now
	}
	if t.rand == nil {
		t.rand = rand.Reader
	}
	return &http.Client{Timeout: 30 * time.Second, Transport: t, CheckRedirect: CheckRedirect}, nil
}

type clientJWTTransport struct {
	key    *CDSClientKey
	issuer func() (string, bool)
	jku    func() string
	base   http.RoundTripper
	now    func() time.Time
	rand   io.Reader
}

// RoundTrip sends a clone of req carrying a JWT minted for it.
func (t *clientJWTTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r2 := req.Clone(req.Context())
	if chainLeftOrigin(req) {
		r2.Header.Del("Authorization")
		return t.base.RoundTrip(r2)
	}
	iss, ok := t.issuer()
	if !ok || iss == "" {
		closeBody(req)
		return nil, ErrCDSClientIssuerUnknown
	}
	tok, err := t.mint(iss, req.URL)
	if err != nil {
		closeBody(req)
		return nil, err
	}
	r2.Header.Set("Authorization", "Bearer "+tok)
	return t.base.RoundTrip(r2)
}

// closeBody closes req's body, as a RoundTripper must even when it sends nothing.
func closeBody(req *http.Request) {
	if req.Body != nil {
		req.Body.Close()
	}
}

// mint signs a JWT for one request to target: aud is the URL the request is
// sent to, exactly as on the wire, so without any user information (which
// net/http sends as a header, never in the URL).
func (t *clientJWTTransport) mint(iss string, target *url.URL) (string, error) {
	aud := *target
	aud.User = nil
	nonce := make([]byte, 16)
	if _, err := io.ReadFull(t.rand, nonce); err != nil {
		return "", fmt.Errorf("smartauth: CDS client JWT jti: %w", err)
	}
	now := t.now()
	tok := jwt.NewWithClaims(t.key.method, jwt.MapClaims{
		"iss": iss,
		"aud": aud.String(),
		"iat": now.Unix(),
		"exp": now.Add(CDSClientJWTLifetime).Unix(),
		"jti": base64.RawURLEncoding.EncodeToString(nonce),
	})
	tok.Header["typ"] = "JWT"
	tok.Header["kid"] = t.key.kid
	if t.jku != nil {
		if jku := t.jku(); jku != "" {
			tok.Header["jku"] = jku
		}
	}
	signed, err := tok.SignedString(t.key.key)
	if err != nil {
		return "", fmt.Errorf("smartauth: sign CDS client JWT: %w", err)
	}
	return signed, nil
}
