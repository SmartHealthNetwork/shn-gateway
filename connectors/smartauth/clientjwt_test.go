package smartauth

import (
	"bytes"
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
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// cdsTestKey is a fresh key of alg's kind.
func cdsTestKey(t *testing.T, alg string) crypto.PrivateKey {
	t.Helper()
	if alg == "ES384" {
		k, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// jwkSetKeys reads a served JWK Set as a CDS service would: each key's public
// half, by kid.
func jwkSetKeys(t *testing.T, set []byte) map[string]crypto.PublicKey {
	t.Helper()
	var parsed struct {
		Keys []struct {
			Kty, Kid, Alg, Use, Crv, X, Y, N, E string
		} `json:"keys"`
	}
	if err := json.Unmarshal(set, &parsed); err != nil {
		t.Fatalf("JWK Set: %v", err)
	}
	dec := func(s string) *big.Int {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatalf("JWK member: %v", err)
		}
		return new(big.Int).SetBytes(b)
	}
	out := map[string]crypto.PublicKey{}
	for _, k := range parsed.Keys {
		switch k.Kty {
		case "EC":
			if k.Crv != "P-384" {
				t.Fatalf("EC key on %q", k.Crv)
			}
			out[k.Kid] = &ecdsa.PublicKey{Curve: elliptic.P384(), X: dec(k.X), Y: dec(k.Y)}
		case "RSA":
			out[k.Kid] = &rsa.PublicKey{N: dec(k.N), E: int(dec(k.E).Int64())}
		default:
			t.Fatalf("JWK kty %q", k.Kty)
		}
		if k.Use != "sig" || k.Alg == "" {
			t.Fatalf("JWK use %q alg %q", k.Use, k.Alg)
		}
	}
	return out
}

// seenJWT is one JWT a CDS service stub verified.
type seenJWT struct {
	url    string
	header map[string]any
	claims jwt.MapClaims
}

// cdsStub is a CDS service that requires a client JWT: it verifies each
// request's JWT against the client's published JWK Set, signature, alg, kid
// and typ, and records it. A request it cannot verify is answered 401.
type cdsStub struct {
	t    *testing.T
	jwks []byte
	alg  string
	// now is the service's clock; nil is the wall clock.
	now  func() time.Time
	mu   sync.Mutex
	seen []seenJWT
	bad  []string
}

func (s *cdsStub) verify(r *http.Request, url string) bool {
	keys := jwkSetKeys(s.t, s.jwks)
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		s.fail("%s: no bearer JWT", url)
		return false
	}
	claims := jwt.MapClaims{}
	opts := []jwt.ParserOption{jwt.WithValidMethods([]string{s.alg}), jwt.WithIssuedAt(), jwt.WithExpirationRequired()}
	if s.now != nil {
		opts = append(opts, jwt.WithTimeFunc(s.now))
	}
	tok, err := jwt.ParseWithClaims(raw, claims, func(tok *jwt.Token) (any, error) {
		kid, _ := tok.Header["kid"].(string)
		k, ok := keys[kid]
		if !ok {
			return nil, fmt.Errorf("kid %q not in the JWK Set", kid)
		}
		return k, nil
	}, opts...)
	if err != nil || !tok.Valid {
		s.fail("%s: JWT does not verify against the JWK Set: %v", url, err)
		return false
	}
	if tok.Header["typ"] != "JWT" {
		s.fail("%s: typ %v, want JWT", url, tok.Header["typ"])
	}
	s.mu.Lock()
	s.seen = append(s.seen, seenJWT{url: url, header: tok.Header, claims: claims})
	s.mu.Unlock()
	return true
}

func (s *cdsStub) fail(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bad = append(s.bad, fmt.Sprintf(format, args...))
}

func (s *cdsStub) jwts() []seenJWT {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.bad) > 0 {
		s.t.Fatalf("CDS service stub refused: %s", strings.Join(s.bad, "; "))
	}
	return append([]seenJWT(nil), s.seen...)
}

// newCDSServer is the stub as an HTTP server: GET /cds-services answers an
// empty listing, POST /cds-services/{id} an empty card set, and GET /moved
// redirects (same origin) to /cds-services.
func newCDSServer(t *testing.T, s *cdsStub) *httptest.Server {
	srv := httptest.NewServer(nil)
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.verify(r, srv.URL+r.URL.Path) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/moved":
			http.Redirect(w, r, "/cds-services", http.StatusFound)
		case r.URL.Path == "/cds-services":
			_, _ = io.WriteString(w, `{"services":[]}`)
		default:
			_, _ = io.ReadAll(r.Body)
			_, _ = io.WriteString(w, `{"cards":[]}`)
		}
	})
	t.Cleanup(srv.Close)
	return srv
}

const testIssuer = "https://payer-gw.example"

func fixedIssuer(iss string) func() (string, bool) { return func() (string, bool) { return iss, true } }

// TestCDSClientJWT_EveryCallCarriesAFreshVerifiableJWTForTheExactURL: the
// discovery GET and each hook POST, and a redirect hop, each carry a JWT that
// verifies against the client's published JWK Set (alg, kid, typ JWT); aud is
// the exact URL that request was sent to; iss is the issuer; iat is the clock's
// now and exp five minutes on; jku names the JWK Set; and no jti repeats,
// across sends and the hop's resend. For each algorithm the key takes, with
// an assigned kid and with the thumbprint.
func TestCDSClientJWT_EveryCallCarriesAFreshVerifiableJWTForTheExactURL(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	for _, alg := range []string{"ES384", "RS384", "RS256"} {
		for _, kid := range []string{"", "payer-key-1"} {
			t.Run(alg+"/kid="+kid, func(t *testing.T) {
				key, err := NewCDSClientKey(alg, cdsTestKey(t, alg), kid)
				if err != nil {
					t.Fatal(err)
				}
				stub := &cdsStub{t: t, jwks: key.JWKSet(), alg: alg, now: func() time.Time { return now }}
				srv := newCDSServer(t, stub)
				const jku = "https://payer-gw.example/cds-client/jwks.json"
				hc, err := NewClientJWTHTTPClient(key, ClientJWTConfig{
					Issuer: fixedIssuer(testIssuer), JKU: func() string { return jku },
					Now: func() time.Time { return now },
				})
				if err != nil {
					t.Fatal(err)
				}
				var want []string
				do := func(method, path, body string) {
					t.Helper()
					var rd io.Reader
					if body != "" {
						rd = strings.NewReader(body)
					}
					req, _ := http.NewRequest(method, srv.URL+path, rd)
					resp, err := hc.Do(req)
					if err != nil {
						t.Fatalf("%s %s: %v", method, path, err)
					}
					_, _ = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					if resp.StatusCode != http.StatusOK {
						t.Fatalf("%s %s: %d", method, path, resp.StatusCode)
					}
				}
				do(http.MethodGet, "/cds-services", "")
				want = append(want, srv.URL+"/cds-services")
				for range 3 {
					do(http.MethodPost, "/cds-services/coverage-requirements", `{"hook":"order-sign"}`)
					want = append(want, srv.URL+"/cds-services/coverage-requirements")
				}
				do(http.MethodGet, "/moved", "")
				want = append(want, srv.URL+"/moved", srv.URL+"/cds-services")

				seen := stub.jwts()
				if len(seen) != len(want) {
					t.Fatalf("service saw %d JWTs, want %d", len(seen), len(want))
				}
				jtis := map[string]bool{}
				wantKID := kid
				if wantKID == "" {
					wantKID = key.KID()
				}
				for i, s := range seen {
					if s.url != want[i] || s.claims["aud"] != want[i] {
						t.Errorf("request %d to %s: aud %v, want the exact URL %s", i, s.url, s.claims["aud"], want[i])
					}
					if s.claims["iss"] != testIssuer {
						t.Errorf("request %d: iss %v, want %s", i, s.claims["iss"], testIssuer)
					}
					iat, _ := s.claims["iat"].(float64)
					exp, _ := s.claims["exp"].(float64)
					if int64(iat) != now.Unix() || int64(exp)-int64(iat) != int64(CDSClientJWTLifetime/time.Second) || exp <= iat {
						t.Errorf("request %d: iat %v exp %v, want iat now and exp iat+%s", i, iat, exp, CDSClientJWTLifetime)
					}
					if s.header["alg"] != alg || s.header["kid"] != wantKID || s.header["jku"] != jku {
						t.Errorf("request %d: header %v, want alg %s kid %s jku %s", i, s.header, alg, wantKID, jku)
					}
					jti, _ := s.claims["jti"].(string)
					if jti == "" || jtis[jti] {
						t.Errorf("request %d: jti %q empty or repeated", i, jti)
					}
					jtis[jti] = true
				}
			})
		}
	}
}

// TestCDSClientJWT_KIDIsTheThumbprintWhenUnassigned: with no kid assigned the
// key's id is its RFC 7638 thumbprint, recomputed here from the served members.
func TestCDSClientJWT_KIDIsTheThumbprintWhenUnassigned(t *testing.T) {
	for _, alg := range []string{"ES384", "RS384"} {
		key, err := NewCDSClientKey(alg, cdsTestKey(t, alg), "")
		if err != nil {
			t.Fatal(err)
		}
		var set struct {
			Keys []map[string]string `json:"keys"`
		}
		if err := json.Unmarshal(key.JWKSet(), &set); err != nil || len(set.Keys) != 1 {
			t.Fatalf("%s: JWK Set %s: %v", alg, key.JWKSet(), err)
		}
		k := set.Keys[0]
		var canonical string
		if alg == "ES384" {
			canonical = fmt.Sprintf(`{"crv":%q,"kty":"EC","x":%q,"y":%q}`, k["crv"], k["x"], k["y"])
		} else {
			canonical = fmt.Sprintf(`{"e":%q,"kty":"RSA","n":%q}`, k["e"], k["n"])
		}
		sum := sha256.Sum256([]byte(canonical))
		if want := base64.RawURLEncoding.EncodeToString(sum[:]); key.KID() != want || k["kid"] != want {
			t.Errorf("%s: kid %q (set %q), want the thumbprint %q", alg, key.KID(), k["kid"], want)
		}
	}
}

// TestCDSClientJWT_JKUOmittedWhenNone: a JKU that names nothing leaves the
// header out, and so does no JKU at all.
func TestCDSClientJWT_JKUOmittedWhenNone(t *testing.T) {
	key, err := NewCDSClientKey("ES384", cdsTestKey(t, "ES384"), "")
	if err != nil {
		t.Fatal(err)
	}
	for name, jku := range map[string]func() string{"empty": func() string { return "" }, "nil": nil} {
		stub := &cdsStub{t: t, jwks: key.JWKSet(), alg: "ES384"}
		srv := newCDSServer(t, stub)
		hc, _ := NewClientJWTHTTPClient(key, ClientJWTConfig{Issuer: fixedIssuer(testIssuer), JKU: jku})
		resp, err := hc.Get(srv.URL + "/cds-services")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if seen := stub.jwts(); len(seen) != 1 || seen[0].header["jku"] != nil {
			t.Errorf("%s: header %v, want no jku", name, seen)
		}
	}
}

// TestCDSClientJWT_401IsTheAnswerAndNotResent: a service that refuses the JWT
// is answered once; a fresh JWT from the same key cannot change its verdict.
func TestCDSClientJWT_401IsTheAnswerAndNotResent(t *testing.T) {
	key, _ := NewCDSClientKey("ES384", cdsTestKey(t, "ES384"), "")
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	hc, _ := NewClientJWTHTTPClient(key, ClientJWTConfig{Issuer: fixedIssuer(testIssuer)})
	resp, err := hc.Post(srv.URL+"/cds-services/x", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || calls != 1 {
		t.Fatalf("status %d after %d calls, want the 401 after one", resp.StatusCode, calls)
	}
}

// TestCDSClientJWT_CrossOriginHopCarriesNoJWT: a redirect to another origin is
// followed without the JWT, as the bearer client's is.
func TestCDSClientJWT_CrossOriginHopCarriesNoJWT(t *testing.T) {
	key, _ := NewCDSClientKey("ES384", cdsTestKey(t, "ES384"), "")
	var otherAuth []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherAuth = append(otherAuth, r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, `{"services":[]}`)
	}))
	defer other.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Error("the first origin got no JWT")
		}
		http.Redirect(w, r, other.URL+"/cds-services", http.StatusFound)
	}))
	defer first.Close()
	hc, _ := NewClientJWTHTTPClient(key, ClientJWTConfig{Issuer: fixedIssuer(testIssuer)})
	resp, err := hc.Get(first.URL + "/cds-services")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(otherAuth) != 1 || otherAuth[0] != "" {
		t.Fatalf("the other origin got Authorization %q, want none", otherAuth)
	}
}

// TestCDSClientJWT_NoIssuerNothingSent: with no issuer known, nothing is sent,
// with another credential or without one, and the error says why.
func TestCDSClientJWT_NoIssuerNothingSent(t *testing.T) {
	key, _ := NewCDSClientKey("ES384", cdsTestKey(t, "ES384"), "")
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer srv.Close()
	for name, iss := range map[string]func() (string, bool){
		"unknown": func() (string, bool) { return "", false },
		"empty":   func() (string, bool) { return "", true },
	} {
		hc, _ := NewClientJWTHTTPClient(key, ClientJWTConfig{Issuer: iss})
		_, err := hc.Post(srv.URL+"/cds-services/x", "application/json", strings.NewReader(`{}`))
		if !errors.Is(err, ErrCDSClientIssuerUnknown) {
			t.Errorf("%s: err %v, want ErrCDSClientIssuerUnknown", name, err)
		}
	}
	if calls != 0 {
		t.Fatalf("%d requests reached the service with no issuer known", calls)
	}
}

// TestNewCDSClientKey_RefusesAKeyNotTheAlgsKind: the key must be alg's kind,
// an EC key on P-384 for ES384; no other algorithm is taken.
func TestNewCDSClientKey_RefusesAKeyNotTheAlgsKind(t *testing.T) {
	p256, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	for _, c := range []struct {
		alg string
		key crypto.PrivateKey
	}{
		{"ES384", cdsTestKey(t, "RS384")},
		{"RS384", cdsTestKey(t, "ES384")},
		{"ES384", p256},
		{"HS256", cdsTestKey(t, "RS384")},
		{"none", cdsTestKey(t, "RS384")},
		{"ES384", nil},
	} {
		if _, err := NewCDSClientKey(c.alg, c.key, ""); err == nil {
			t.Errorf("alg %s with %T: accepted", c.alg, c.key)
		}
	}
}

// TestCheckPublicJWKSet_RefusesAPrivateMember: the key set the gateway builds
// passes; the same set with any private member planted in its key fails, and
// so does anything that is not a JWK Set.
func TestCheckPublicJWKSet_RefusesAPrivateMember(t *testing.T) {
	for _, alg := range []string{"ES384", "RS384"} {
		key, err := NewCDSClientKey(alg, cdsTestKey(t, alg), "")
		if err != nil {
			t.Fatal(err)
		}
		if err := CheckPublicJWKSet(key.JWKSet()); err != nil {
			t.Fatalf("%s: the built set fails: %v", alg, err)
		}
		if bytes.Contains(key.JWKSet(), []byte(`"d"`)) {
			t.Fatalf("%s: the built set names d: %s", alg, key.JWKSet())
		}
		for _, m := range privateJWKMembers {
			var set map[string][]map[string]any
			if err := json.Unmarshal(key.JWKSet(), &set); err != nil {
				t.Fatal(err)
			}
			set["keys"][0][m] = "AQAB"
			planted, _ := json.Marshal(set)
			if err := CheckPublicJWKSet(planted); err == nil {
				t.Errorf("%s: a set with %q planted passes", alg, m)
			}
		}
	}
	for _, notASet := range []string{``, `[]`, `{}`, `{"keys":null}`} {
		if err := CheckPublicJWKSet([]byte(notASet)); err == nil {
			t.Errorf("%q passes as a JWK Set", notASet)
		}
	}
}

// TestCDSClientJWKSURL is the one URL: the base, its trailing slash dropped,
// and the path; never under a base it cannot be served under.
func TestCDSClientJWKSURL(t *testing.T) {
	for base, want := range map[string]string{
		"https://gw.example":       "https://gw.example/cds-client/jwks.json",
		"https://gw.example/":      "https://gw.example/cds-client/jwks.json",
		"https://gw.example/payer": "https://gw.example/payer/cds-client/jwks.json",
		"http://payer-gw:8080":     "http://payer-gw:8080/cds-client/jwks.json",
		"ftp://gw.example":         "",
		"https://gw.example/?q=1":  "",
		"https://gw.example/#f":    "",
		"https://u:p@gw.example":   "",
		"gw.example":               "",
		"":                         "",
	} {
		got, err := CDSClientJWKSURL(base)
		if (err != nil) != (want == "") || got != want {
			t.Errorf("CDSClientJWKSURL(%q) = %q, %v; want %q", base, got, err, want)
		}
	}
}

// TestCDSClientJWT_AudHoldsNoUserInformation: a URL that carries user
// information sends it as a header, never on the wire's URL, so aud, the URL
// called, never holds it.
func TestCDSClientJWT_AudHoldsNoUserInformation(t *testing.T) {
	key, _ := NewCDSClientKey("ES384", cdsTestKey(t, "ES384"), "")
	stub := &cdsStub{t: t, jwks: key.JWKSet(), alg: "ES384"}
	srv := newCDSServer(t, stub)
	hc, _ := NewClientJWTHTTPClient(key, ClientJWTConfig{Issuer: fixedIssuer(testIssuer)})
	withUser := strings.Replace(srv.URL, "http://", "http://user:secret@", 1)
	resp, err := hc.Get(withUser + "/cds-services")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	seen := stub.jwts()
	if len(seen) != 1 || seen[0].claims["aud"] != srv.URL+"/cds-services" {
		t.Fatalf("aud %v, want %s with no user information", seen, srv.URL+"/cds-services")
	}
}
