package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// env builds a getenv func from a map.
func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// The fail-closed rule is a PURE decision — unit-test it directly via selectValidator,
// not through Run/build (whose LoadBundle+discovery steps run BEFORE the validator gate,
// so an empty-env call never reaches it). The INTEGRATED fail-closed assertion lives in the
// boot gate, which supplies a real bundle + a validator-less /discovery.

// No validator URL + no fake opt-in ⇒ error (FR-36 fail-closed).
func TestSelectValidator_FailsClosed(t *testing.T) {
	if _, err := selectValidator(env(map[string]string{}), ""); err == nil {
		t.Fatal("selectValidator(no url, no fake) = nil error, want fail-closed error")
	}
}

// Explicit fake opt-in ⇒ fake validator, no error.
func TestSelectValidator_FakeOptIn(t *testing.T) {
	v, err := selectValidator(env(map[string]string{"SHN_FAKE_VALIDATOR": "1"}), "")
	if err != nil || v == nil {
		t.Fatalf("fake opt-in: v=%v err=%v, want non-nil validator, nil error", v, err)
	}
}

// Resolved URL ⇒ real operation-level validator.
func TestSelectValidator_RealWhenURL(t *testing.T) {
	v, err := selectValidator(env(map[string]string{}), "http://validator.example/fhir")
	if err != nil || v == nil {
		t.Fatalf("real url: v=%v err=%v, want non-nil validator, nil error", v, err)
	}
}

func TestLoadConfig_ParsesStoreDatabaseURL(t *testing.T) {
	env := map[string]string{
		"ROLE":                   "payer",
		"SHN_SECRETS":            "/etc/shn/bundles/payer",
		"SHN_DISCOVERY_URL":      "http://accounts:8088/discovery",
		"SHN_STORE_DATABASE_URL": "postgres://postgres:shn@postgres:5432/shn_gateway?sslmode=disable",
	}
	getenv := func(k string) string { return env[k] }
	cfg, err := loadConfig(getenv)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	// A postgres:// DSN is NOT an http(s) URL — it must be carried, not rejected by
	// the http-URL validation loop.
	if cfg.StoreDatabaseURL != env["SHN_STORE_DATABASE_URL"] {
		t.Fatalf("StoreDatabaseURL = %q; want %q", cfg.StoreDatabaseURL, env["SHN_STORE_DATABASE_URL"])
	}
}

func TestResolveDiscovery_AnchorKeyURLOverride(t *testing.T) {
	// Discovery advertises one (public) key URL; the env override points at another.
	// The override must win (firstNonEmpty(env, discovery)).
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyBody := fmt.Sprintf(`{"pubkey":%q}`, base64.StdEncoding.EncodeToString(pub))

	var overrideHit bool
	override := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		overrideHit = true
		_, _ = w.Write([]byte(keyBody))
	}))
	defer override.Close()
	disc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /discovery doc points the anchor key URLs at a DIFFERENT (unused) place.
		fmt.Fprintf(w, `{"endpoints":{},"authzPublicKeyURL":"http://unused.invalid/pubkey","hubTransportKeyURL":%q}`, override.URL)
	}))
	defer disc.Close()

	cfg := config{
		DiscoveryURL:       disc.URL,
		AuthzPubkeyURL:     override.URL, // env override → must be used instead of the disc value
		HubTransportKeyURL: override.URL,
	}
	_, _, err := resolveDiscovery(context.Background(), shnsdk.NewClient(), cfg)
	if err != nil {
		t.Fatalf("resolveDiscovery: %v", err)
	}
	if !overrideHit {
		t.Fatal("anchor-key override URL was not fetched — env override did not win over discovery")
	}
}

func TestResolveDiscovery_ResolvesTrustPlanes(t *testing.T) {
	// /discovery advertises consent/audit/phg; the gateway resolves them WITHOUT env.
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyBody := fmt.Sprintf(`{"pubkey":%q}`, base64.StdEncoding.EncodeToString(pub))
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(keyBody))
	}))
	defer keys.Close()
	disc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"endpoints":{"consent":"http://consent.invalid","audit":"http://audit.invalid","phg":"http://phg.invalid"},"authzPublicKeyURL":%q,"hubTransportKeyURL":%q}`, keys.URL, keys.URL)
	}))
	defer disc.Close()

	cfg := config{DiscoveryURL: disc.URL} // NO CONSENT_URL/AUDIT_URL/PHG_URL env
	_, ep, err := resolveDiscovery(context.Background(), shnsdk.NewClient(), cfg)
	if err != nil {
		t.Fatalf("resolveDiscovery: %v", err)
	}
	if ep.Consent != "http://consent.invalid" || ep.Audit != "http://audit.invalid" || ep.PHG != "http://phg.invalid" {
		t.Fatalf("planes not resolved from discovery: consent=%q audit=%q phg=%q", ep.Consent, ep.Audit, ep.PHG)
	}
}

func TestFetchEd25519Key_BadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()
	if _, err := fetchEd25519Key(context.Background(), srv.Client(), srv.URL+"/missing"); err == nil {
		t.Error("expected error on 404, got nil")
	}
}

func TestFetchEd25519Key_WrongSize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"pubkey": base64.StdEncoding.EncodeToString(make([]byte, 16))}) //nolint:errcheck
	}))
	defer srv.Close()
	if _, err := fetchEd25519Key(context.Background(), srv.Client(), srv.URL+"/"); err == nil {
		t.Error("expected error on wrong key size, got nil")
	}
}

// resolveDiscovery surfaces disc.FHIRValidateURL into endpoints.FHIRValidate; the
// gateway then applies firstNonEmpty(cfg.FHIRValidateURL, endpoints.FHIRValidate) so an
// explicit env wins and the discovery value applies when env is empty.
func TestResolveDiscovery_SurfacesFHIRValidate(t *testing.T) {
	const wantURL = "https://validator.example/fhir"
	// resolveDiscovery fetches the trust-anchor keys BEFORE returning endpoints, so the
	// advertised key URLs must be reachable (mirrors TestResolveDiscovery_ResolvesTrustPlanes).
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyBody := fmt.Sprintf(`{"pubkey":%q}`, base64.StdEncoding.EncodeToString(pub))
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(keyBody))
	}))
	defer keys.Close()
	discSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(shnsdk.Discovery{ //nolint:errcheck
			Endpoints:          shnsdk.DiscoveryEndpoints{Registrar: "https://reg.example", Hub: "https://hub.example", Authz: "https://authz.example"},
			AuthzPublicKeyURL:  keys.URL,
			HubTransportKeyURL: keys.URL,
			FHIRValidateURL:    wantURL,
		})
	}))
	defer discSrv.Close()
	_, endpoints, err := resolveDiscovery(context.Background(), discSrv.Client(), config{DiscoveryURL: discSrv.URL})
	if err != nil {
		t.Fatalf("resolveDiscovery: %v", err)
	}
	if endpoints.FHIRValidate != wantURL {
		t.Fatalf("endpoints.FHIRValidate = %q, want %q", endpoints.FHIRValidate, wantURL)
	}
	if got := firstNonEmpty("", endpoints.FHIRValidate); got != wantURL {
		t.Fatalf("env-empty precedence: got %q, want %q", got, wantURL)
	}
	const envURL = "https://local-validator.example/fhir"
	if got := firstNonEmpty(envURL, endpoints.FHIRValidate); got != envURL {
		t.Fatalf("explicit-env precedence: got %q, want %q", got, envURL)
	}
}
