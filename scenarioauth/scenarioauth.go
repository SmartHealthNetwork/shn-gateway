// Package scenarioauth is the caller's half of the authentication of a
// provider gateway's /scenario routes, its operator surface: each request
// carries an X-Holder-Assertion for the gateway it is sent to (its audience is
// AudienceFor that gateway's holder id), signed with the caller's own key
// under its label and bound to the request (method, target and body), so an
// assertion minted for one request, or for one gateway, cannot be spent on
// another. Verify is the check a gateway runs: it admits only the callers it
// is given, by label, only for its own holder id, and a participant's holder
// key is never one of them. The gateway spends each admitted assertion's jti
// once.
//
// A caller names the gateway it calls by that gateway's holder id. A
// participant's own system knows its gateway's holder id from its bundle; a
// caller that runs beside a gateway rather than inside the participant's
// system, as SHN's own operator callers do, is told it in GatewayEnv, beside
// the URL it dials.
package scenarioauth

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// AudiencePrefix starts the audience of every /scenario assertion.
const AudiencePrefix = "scenario:"

// AudienceFor is the audience of a /scenario assertion for the gateway whose
// holder id is gateway: AudiencePrefix and the holder id. It is never the bare
// holder id, which a gateway's Hub assertions are addressed to, so neither can
// be spent as the other.
func AudienceFor(gateway string) string { return AudiencePrefix + gateway }

// The callers SHN's own deployments run, by the label each signs under: the
// deploy smoke (and the load probe it runs), the monitor's scenario canary,
// and the PHG's patient-authored DTR completion.
const (
	CallerDeploySmoke = "deploy-smoke"
	CallerMonitor     = "monitor"
	CallerPHG         = "phg"
)

// HeaderName is the request header that carries the assertion, base64 JSON.
const HeaderName = "X-Holder-Assertion"

// PathPrefix is the path prefix of the routes the assertion is for.
const PathPrefix = "/scenario/"

// TTL is how long an assertion is valid. Each request mints its own; the jti
// is one-time.
const TTL = time.Minute

// MaxIssueSkew is how far ahead of the verifier's clock an assertion may be
// issued. The SDK allows minutes; a caller's assertion is minted for the
// request it signs, so a pre-minted one is refused.
const MaxIssueSkew = 30 * time.Second

// Binding is the bytes an assertion's bh hashes: the method, a space, the
// request target (path and raw query, exactly as sent), a newline and the
// body. The query changes what a scenario does, and a GET has no body, so
// the request line is bound with the body.
func Binding(method, requestURI string, body []byte) []byte {
	b := make([]byte, 0, len(method)+len(requestURI)+2+len(body))
	b = append(b, method...)
	b = append(b, ' ')
	b = append(b, requestURI...)
	b = append(b, '\n')
	return append(b, body...)
}

// BindingHash is hex(sha256(Binding(...))), the value an assertion's bh
// carries.
func BindingHash(method, requestURI string, body []byte) string {
	sum := sha256.Sum256(Binding(method, requestURI, body))
	return hex.EncodeToString(sum[:])
}

// Sign stamps req with an assertion by label for the gateway whose holder id
// is gateway, bound to req. It reads req's body and puts it back, so req can
// still be sent.
func Sign(req *http.Request, label, gateway string, priv ed25519.PrivateKey, now time.Time) error {
	if len(priv) != ed25519.PrivateKeySize {
		return fmt.Errorf("scenarioauth: %s has no ed25519 private key", label)
	}
	if gateway == "" {
		return fmt.Errorf("scenarioauth: %s names no gateway to sign for", label)
	}
	var body []byte
	if req.Body != nil && req.Body != http.NoBody {
		b, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return fmt.Errorf("scenarioauth: read the request body: %w", err)
		}
		body = b
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		req.ContentLength = int64(len(body))
	}
	a := shnsdk.IssueAssertionForBody(label, AudienceFor(gateway), priv, now, TTL, Binding(req.Method, req.URL.RequestURI(), body))
	raw, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("scenarioauth: encode the assertion: %w", err)
	}
	req.Header.Set(HeaderName, base64.StdEncoding.EncodeToString(raw))
	return nil
}

// Transport signs, as Label for the gateway whose holder id is Gateway, every
// request it carries to a /scenario route (a path that starts with
// PathPrefix, from the root), and passes every other request on as it is.
// Base nil means http.DefaultTransport; Clock nil means time.Now.
//
// A signed request goes out on a connection of its own, never a reused one:
// when Base is an *http.Transport, on a copy of it with keep-alives off. Go's
// transport resends an idempotent request on its own when a reused connection
// drops before the answer, below RoundTrip, so the resend would carry the same
// assertion, which the gateway may already have spent and would refuse as a
// replay; on a connection that was never reused it does not resend. A Base of
// another kind is used as it is, and any retry it makes is its own. The copy
// is made from the Base of the first signed request, so set Base before the
// Transport carries one.
type Transport struct {
	Base    http.RoundTripper
	Label   string
	Gateway string
	Priv    ed25519.PrivateKey
	Clock   func() time.Time

	freshOnce sync.Once
	fresh     http.RoundTripper
}

// signedBase is the transport a signed request goes out on: Base with no
// connection reuse when Base is an *http.Transport, else Base itself.
func (t *Transport) signedBase(base http.RoundTripper) http.RoundTripper {
	t.freshOnce.Do(func() {
		t.fresh = base
		if ht, ok := base.(*http.Transport); ok {
			c := ht.Clone()
			c.DisableKeepAlives = true
			t.fresh = c
		}
	})
	return t.fresh
}

// RoundTrip signs a clone of a /scenario request and sends it.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	if !strings.HasPrefix(req.URL.Path, PathPrefix) {
		return base.RoundTrip(req)
	}
	now := time.Now
	if t.Clock != nil {
		now = t.Clock
	}
	r := req.Clone(req.Context())
	if err := Sign(r, t.Label, t.Gateway, t.Priv, now()); err != nil {
		// A RoundTripper closes the request body, even on error.
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	return t.signedBase(base).RoundTrip(r)
}

// Client returns a copy of c (nil means a zero http.Client) whose /scenario
// requests are signed as label for the gateway whose holder id is gateway. It
// follows no redirect to a /scenario route: the caller gets the redirect
// response instead, so a request it did not address itself is never signed.
func Client(c *http.Client, label, gateway string, priv ed25519.PrivateKey, clock func() time.Time) *http.Client {
	out := &http.Client{}
	if c != nil {
		*out = *c
	}
	out.Transport = &Transport{Base: out.Transport, Label: label, Gateway: gateway, Priv: priv, Clock: clock}
	follow := out.CheckRedirect
	out.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if strings.HasPrefix(req.URL.Path, PathPrefix) {
			return http.ErrUseLastResponse
		}
		if follow != nil {
			return follow(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return out
}

// The environment variables a caller reads its key from: KeyEnv holds the key
// itself (base64), KeyFileEnv the path of a file that holds it. KeyEnv wins
// when both are set. GatewayEnv holds the holder id of the gateway the caller
// calls, which its assertions are for.
const (
	KeyEnv     = "SCENARIO_CALLER_KEY"
	KeyFileEnv = "SCENARIO_CALLER_KEY_FILE"
	GatewayEnv = "SCENARIO_GATEWAY"
)

// ErrNoGateway is ClientFromEnv's error for a key with no GatewayEnv: the
// caller has a key but no gateway to sign for, so its requests go unsigned,
// and a gateway that authenticates its /scenario callers refuses every one.
var ErrNoGateway = errors.New("scenarioauth: " + GatewayEnv + " is not set, so the /scenario requests go unsigned and a gateway that authenticates its callers refuses them")

// ParseKey reads a caller key: a base64 ed25519 private key, the format of a
// bundle's secrets/scenario-caller-<label>.sign.
func ParseKey(s string) (ed25519.PrivateKey, error) {
	k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(k) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("scenarioauth: not a base64 ed25519 private key")
	}
	return ed25519.PrivateKey(k), nil
}

// LoadKeyFile reads the caller key at path.
func LoadKeyFile(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	k, err := ParseKey(string(raw))
	if err != nil {
		return nil, fmt.Errorf("scenarioauth: %s: %w", path, err)
	}
	return k, nil
}

// KeyFromEnv reads a caller key from KeyEnv, else from the file KeyFileEnv
// names. It returns nil and no error when neither is set: the caller's
// requests then go unsigned, and a gateway that authenticates its /scenario
// callers refuses them.
func KeyFromEnv(getenv func(string) string) (ed25519.PrivateKey, error) {
	if v := getenv(KeyEnv); v != "" {
		k, err := ParseKey(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", KeyEnv, err)
		}
		return k, nil
	}
	if p := getenv(KeyFileEnv); p != "" {
		return LoadKeyFile(p)
	}
	return nil, nil
}

// ClientFromEnv returns c signed as label, with the key the environment
// names, for the gateway GatewayEnv names; c unchanged when it names no key;
// and c unchanged with ErrNoGateway when it names a key but no gateway.
func ClientFromEnv(c *http.Client, label string, getenv func(string) string) (*http.Client, error) {
	if c == nil {
		c = &http.Client{}
	}
	k, err := KeyFromEnv(getenv)
	if err != nil || k == nil {
		return c, err
	}
	gateway := strings.TrimSpace(getenv(GatewayEnv))
	if gateway == "" {
		return c, ErrNoGateway
	}
	return Client(c, label, gateway, k, nil), nil
}

// ParseCallers reads a callers file, the file a provider gateway's
// SCENARIO_CALLERS_FILE names: a JSON array of {"label", "public_key"}, the
// key the base64 of an ed25519 public key. It returns the callers keyed by
// label, the set Verify takes. An entry with no label, a label listed twice,
// or a key that is not one is an error; an empty array admits no caller.
func ParseCallers(raw []byte) (map[string]ed25519.PublicKey, error) {
	var arr []struct {
		Label     string `json:"label"`
		PublicKey string `json:"public_key"`
	}
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("parse the callers: %w", err)
	}
	out := make(map[string]ed25519.PublicKey, len(arr))
	for i, c := range arr {
		if c.Label == "" {
			return nil, fmt.Errorf("entry %d: empty label", i)
		}
		if _, dup := out[c.Label]; dup {
			return nil, fmt.Errorf("caller %q is listed twice", c.Label)
		}
		k, err := base64.StdEncoding.DecodeString(c.PublicKey)
		if err != nil || len(k) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("caller %q: public_key is not a base64 ed25519 public key", c.Label)
		}
		out[c.Label] = ed25519.PublicKey(k)
	}
	return out, nil
}

// Caller is what a verified assertion establishes: the admitted caller's
// label, and the assertion's jti and expiry, for the one-time-use check the
// gateway keeps.
type Caller struct {
	Label  string
	JTI    string
	Expiry time.Time
}

// Verify checks a /scenario request's assertion (the HeaderName value)
// against the callers a gateway admits, keyed by label. gateway is the
// verifying gateway's own holder id, and requestURI the target as it received
// it, r.URL.RequestURI(). It admits the request only when the assertion:
//   - is signed by the key of the caller its holder id names, one of callers;
//   - is for AudienceFor(gateway), unexpired, issued no later than
//     MaxIssueSkew from now, and lives no longer than TTL;
//   - binds this request: method, target (path and raw query) and body.
//
// An empty callers set, or an empty gateway, admits nothing. An assertion for
// another gateway is refused, so one captured on its way to a gateway cannot
// be presented to any other. Verify does not record the jti: the caller of
// Verify spends it, once the request is otherwise admitted.
func Verify(header, method, requestURI string, body []byte, gateway string, callers map[string]ed25519.PublicKey, now time.Time) (Caller, error) {
	if header == "" {
		return Caller{}, fmt.Errorf("scenarioauth: no %s", HeaderName)
	}
	if gateway == "" {
		return Caller{}, fmt.Errorf("scenarioauth: the gateway names no holder id to verify for")
	}
	raw, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		return Caller{}, fmt.Errorf("scenarioauth: %s is not base64", HeaderName)
	}
	var a shnsdk.Assertion
	if err := json.Unmarshal(raw, &a); err != nil {
		return Caller{}, fmt.Errorf("scenarioauth: %s is not an assertion", HeaderName)
	}
	pub, ok := callers[a.HolderID]
	if !ok || len(pub) != ed25519.PublicKeySize {
		return Caller{}, fmt.Errorf("scenarioauth: %q is not an admitted caller", a.HolderID)
	}
	if err := shnsdk.VerifyAssertion(a, AudienceFor(gateway), pub, now); err != nil {
		return Caller{}, fmt.Errorf("scenarioauth: %w", err)
	}
	if a.Expiry.Sub(a.IssuedAt) > TTL {
		return Caller{}, fmt.Errorf("scenarioauth: the assertion lives longer than %v", TTL)
	}
	if a.IssuedAt.After(now.Add(MaxIssueSkew)) {
		return Caller{}, fmt.Errorf("scenarioauth: the assertion is issued more than %v ahead", MaxIssueSkew)
	}
	if a.BodyHash != BindingHash(method, requestURI, body) {
		return Caller{}, fmt.Errorf("scenarioauth: the assertion is bound to another request")
	}
	return Caller{Label: a.HolderID, JTI: a.JTI, Expiry: a.Expiry}, nil
}

// SignerOf reports the label c signs its /scenario requests under, and false
// when c does not sign them: no signing transport, no usable key, or no
// gateway to sign for. A caller logs it at startup, so whether it signs, and
// as whom, is in its log from the first line.
func SignerOf(c *http.Client) (string, bool) {
	if t := signer(c); t != nil {
		return t.Label, true
	}
	return "", false
}

// signer is c's signing transport when it can sign, else nil.
func signer(c *http.Client) *Transport {
	if c == nil {
		return nil
	}
	if t, ok := c.Transport.(*Transport); ok && len(t.Priv) == ed25519.PrivateKeySize && t.Gateway != "" {
		return t
	}
	return nil
}

// StartupLine is the line a caller logs at startup about its /scenario
// requests: that it signs them as its label for its gateway, or a warning
// that it does not.
func StartupLine(c *http.Client) string {
	if t := signer(c); t != nil {
		return "signing /scenario requests as " + t.Label + " for " + t.Gateway
	}
	return "WARNING: /scenario requests go unsigned (no usable " + KeyEnv + " or " + KeyFileEnv + ", or no " + GatewayEnv + "): a gateway that authenticates its /scenario callers refuses them"
}
