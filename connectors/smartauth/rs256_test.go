package smartauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// rsaTokenServer is a hermetic SMART token endpoint that verifies the
// client_assertion only under method (alg pinning, as an authorization
// server that admits one alg does) against pub.
func rsaTokenServer(t *testing.T, pub *rsa.PublicKey, method string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if _, err := jwt.Parse(r.FormValue("client_assertion"),
			func(*jwt.Token) (any, error) { return pub, nil },
			jwt.WithValidMethods([]string{method}), jwt.WithExpirationRequired(),
			jwt.WithAudience(srv.URL)); err != nil {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "AT-RS", "token_type": "bearer", "expires_in": 300})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// An RS256 client signs its assertion RS256, which an authorization server
// that verifies RS256 alone accepts, and one that verifies RS384 alone
// refuses: the alg is the one asked for, never another.
func TestTokenSource_RS256SignsUnderRS256(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	for method, want := range map[string]bool{"RS256": true, "RS384": false} {
		srv := rsaTokenServer(t, &key.PublicKey, method)
		ts := &TokenSource{Config: Config{
			TokenURL: srv.URL, ClientID: "gw-provider", Scope: "system/Task.c",
			Alg: "RS256", Key: key, Clock: testClock(now), HTTPClient: srv.Client(),
		}}
		tok, err := ts.Token(context.Background())
		if got := err == nil && tok == "AT-RS"; got != want {
			t.Errorf("an RS256 assertion to a server verifying %s: token %q, %v; accepted %v, want %v", method, tok, err, got, want)
		}
	}
}

// A key of the wrong kind for its alg refuses the configuration, rather than
// every token request after it.
func TestConfig_AKeyOfTheWrongKindForItsAlgIsRefused(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ecKey, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	for name, c := range map[string]struct {
		alg  string
		key  any
		want string
	}{
		"RS256 with an EC key":  {"RS256", ecKey, "alg RS256 needs an RSA private key"},
		"RS384 with an EC key":  {"RS384", ecKey, "alg RS384 needs an RSA private key"},
		"ES384 with an RSA key": {"ES384", rsaKey, "alg ES384 needs an EC private key"},
	} {
		if _, err := NewHTTPClient(Config{TokenURL: "https://t", ClientID: "x", Alg: c.alg, Key: c.key}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error containing %q", name, err, c.want)
		}
	}
	for alg, key := range map[string]any{"RS256": rsaKey, "RS384": rsaKey, "ES384": ecKey} {
		if _, err := NewHTTPClient(Config{TokenURL: "https://t", ClientID: "x", Alg: alg, Key: key}); err != nil {
			t.Errorf("%s with its own kind of key: %v", alg, err)
		}
	}
}

// The RS256 assertion an authorization server that stores a registered key
// with no kid can select (the Da Vinci reference provider's): no kid header
// when KID is unset (never a computed thumbprint), and one when set; aud is
// the token endpoint exactly as configured; iss and sub are the client id;
// exp is stated.
func TestTokenSource_RS256AssertionHeaderAndClaims(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for kid, wantKid := range map[string]bool{"": false, "k1": true} {
		var header map[string]any
		var claims jwt.MapClaims
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			tok, err := jwt.Parse(r.FormValue("client_assertion"), func(*jwt.Token) (any, error) { return &key.PublicKey, nil },
				jwt.WithValidMethods([]string{"RS256"}))
			if err != nil {
				http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
				return
			}
			header, claims = tok.Header, tok.Claims.(jwt.MapClaims)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "AT", "token_type": "bearer", "expires_in": 300})
		}))
		tokenURL := srv.URL + "/oauth2/token"
		ts := &TokenSource{Config: Config{TokenURL: tokenURL, ClientID: "gw-provider", Scope: "system/Task.c",
			Alg: "RS256", Key: key, KID: kid, HTTPClient: srv.Client()}}
		if _, err := ts.Token(context.Background()); err != nil {
			t.Fatalf("kid %q: %v", kid, err)
		}
		srv.Close()
		got, has := header["kid"]
		if has != wantKid || (wantKid && got != kid) {
			t.Errorf("KID %q: header kid %v (present %v), want present %v", kid, got, has, wantKid)
		}
		if header["alg"] != "RS256" {
			t.Errorf("header alg %v, want RS256", header["alg"])
		}
		if claims["aud"] != tokenURL || claims["iss"] != "gw-provider" || claims["sub"] != "gw-provider" {
			t.Errorf("claims aud %v iss %v sub %v, want %s and the client id", claims["aud"], claims["iss"], claims["sub"], tokenURL)
		}
		if _, ok := claims["exp"]; !ok {
			t.Error("no exp claim")
		}
	}
}
