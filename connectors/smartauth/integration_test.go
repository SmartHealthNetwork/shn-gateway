package smartauth_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
	"github.com/golang-jwt/jwt/v5"
)

// twoServerWorld builds a SMART token endpoint (verifies the ES384 client_assertion,
// issues a self-signed bearer) + a bearer-protected FHIR resource server (verifies
// the bearer). Returns the FHIR base URL + a smartauth-built *http.Client.
func twoServerWorld(t *testing.T, key *ecdsa.PrivateKey) (string, *http.Client) {
	t.Helper()
	var tokenURL string
	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if _, err := jwt.Parse(r.FormValue("client_assertion"),
			func(*jwt.Token) (any, error) { return &key.PublicKey, nil },
			jwt.WithValidMethods([]string{"ES384"}), jwt.WithExpirationRequired(),
			jwt.WithAudience(tokenURL)); err != nil {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		bearer, _ := jwt.NewWithClaims(jwt.SigningMethodES384, jwt.MapClaims{
			"exp": time.Now().Add(5 * time.Minute).Unix(), "scope": "system/*.read",
		}).SignedString(key)
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": bearer, "token_type": "bearer", "expires_in": 300})
	}))
	t.Cleanup(tsrv.Close)
	tokenURL = tsrv.URL

	fsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := ""
		if h := r.Header.Get("Authorization"); len(h) > 7 {
			raw = h[7:]
		}
		if _, err := jwt.Parse(raw, func(*jwt.Token) (any, error) { return &key.PublicKey, nil },
			jwt.WithValidMethods([]string{"ES384"}), jwt.WithExpirationRequired()); err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/fhir+json")
		_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[]}`))
	}))
	t.Cleanup(fsrv.Close)

	hc, err := smartauth.NewHTTPClient(smartauth.Config{
		TokenURL: tsrv.URL, ClientID: "gw", Scope: "system/*.read", Alg: "ES384",
		Key: key, Clock: func() time.Time { return time.Now() }, HTTPClient: tsrv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return fsrv.URL, hc
}

func TestFHIRClientReadsThroughSmartAuth(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	base, hc := twoServerWorld(t, key)
	resp, err := hc.Get(base + "/Patient?identifier=MBR-COVERED")
	if err != nil {
		t.Fatalf("authenticated GET failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated GET: want 200, got %d", resp.StatusCode)
	}
}

func TestFHIRClientRejectedWithoutAuth(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	base, _ := twoServerWorld(t, key)
	// no smartauth → no bearer → resource server must reject
	resp, err := http.DefaultClient.Get(base + "/Patient")
	if err != nil {
		t.Fatalf("unauthenticated GET: unexpected transport error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET: want 401, got %d", resp.StatusCode)
	}
}
