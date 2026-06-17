package smartauth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewHTTPClient_InjectsBearer(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	var hits int32
	tsrv := tokenServer(t, &key.PublicKey, &hits) // from tokensource_test.go (same package)

	var gotAuth string
	resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "ok")
	}))
	defer resource.Close()

	hc, err := NewHTTPClient(Config{
		TokenURL: tsrv.URL, ClientID: "gw", Scope: "system/*.read", Alg: "ES384",
		Key: key, Clock: func() time.Time { return time.Now() }, HTTPClient: tsrv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := hc.Get(resource.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotAuth != "Bearer AT-123" {
		t.Fatalf("Authorization = %q, want %q", gotAuth, "Bearer AT-123")
	}
}

func TestNewHTTPClient_ValidatesConfig(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if _, err := NewHTTPClient(Config{TokenURL: "", ClientID: "x", Alg: "ES384", Key: key}); err == nil {
		t.Fatal("want error for empty TokenURL")
	}
	if _, err := NewHTTPClient(Config{TokenURL: "https://t", ClientID: "", Alg: "ES384", Key: key}); err == nil {
		t.Fatal("want error for empty ClientID")
	}
	if _, err := NewHTTPClient(Config{TokenURL: "https://t", ClientID: "x", Alg: "ES384", Key: nil}); err == nil {
		t.Fatal("want error for nil Key")
	}
	if _, err := NewHTTPClient(Config{TokenURL: "https://t", ClientID: "x", Alg: "BOGUS", Key: key}); err == nil {
		t.Fatal("want error for unknown alg")
	}
}

func TestNewHTTPClient_TokenErrorFailsRequest(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	defer down.Close()
	resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer resource.Close()
	hc, err := NewHTTPClient(Config{
		TokenURL: down.URL, ClientID: "gw", Alg: "ES384", Key: key, HTTPClient: down.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hc.Get(resource.URL); err == nil {
		t.Fatal("want request error when token fetch fails (fail-closed)")
	}
}

// TestBearerTransport_DoesNotMutateCallerRequest asserts that bearerTransport clones
// the request before injecting Authorization so the CALLER'S original request header
// is never touched (spec §6 / RoundTripper contract). This is the belt-and-suspenders
// assertion for the RoundTripper non-mutation contract.
func TestBearerTransport_DoesNotMutateCallerRequest(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	var hits int32
	tsrv := tokenServer(t, &key.PublicKey, &hits)

	// Resource server echoes back whatever Authorization it receives.
	resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer resource.Close()

	hc, err := NewHTTPClient(Config{
		TokenURL: tsrv.URL, ClientID: "gw", Scope: "system/*.read", Alg: "ES384",
		Key: key, Clock: func() time.Time { return time.Now() }, HTTPClient: tsrv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest(http.MethodGet, resource.URL, nil)
	// The caller's request carries no Authorization header.
	before := req.Header.Get("Authorization")

	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// The ORIGINAL req must still have no Authorization header after the round-trip.
	after := req.Header.Get("Authorization")
	if after != before {
		t.Fatalf("bearerTransport mutated caller's request: Authorization changed from %q to %q", before, after)
	}
}
