package app

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	checks "github.com/SmartHealthNetwork/shn-gateway/checks"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"github.com/SmartHealthNetwork/shn-sdk/health"
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

// TestLoadConfig_ObserverAddrLoopbackOnly: OBSERVER_ADDR must be loopback —
// fail-closed at config load, not a runtime warning: the observer stream
// carries edge payloads and must never be reachable off-host. Empty =
// off (the published-gateway default; the rejection row's config half).
func TestLoadConfig_ObserverAddrLoopbackOnly(t *testing.T) {
	cases := []struct {
		addr    string
		wantErr bool
	}{
		{"", false},                // off — the default
		{"127.0.0.1:9411", false},  // loopback ok
		{"localhost:9411", false},  // loopback ok
		{"[::1]:9411", false},      // v6 loopback ok
		{"0.0.0.0:9411", true},     // wildcard — refused
		{"192.168.1.5:9411", true}, // LAN — refused
		{"127.0.0.1", true},        // missing port — refused
	}
	for _, c := range cases {
		env := map[string]string{
			"ROLE":              "provider",
			"SHN_SECRETS":       "/etc/shn/bundles/provider",
			"SHN_DISCOVERY_URL": "http://accounts:8088/discovery",
			"OBSERVER_ADDR":     c.addr,
		}
		cfg, err := loadConfig(func(k string) string { return env[k] })
		if c.wantErr && err == nil {
			t.Fatalf("OBSERVER_ADDR=%q: want error, got cfg %+v", c.addr, cfg)
		}
		if !c.wantErr && err != nil {
			t.Fatalf("OBSERVER_ADDR=%q: unexpected error %v", c.addr, err)
		}
		if !c.wantErr && cfg.ObserverAddr != c.addr {
			t.Fatalf("OBSERVER_ADDR=%q: cfg.ObserverAddr = %q", c.addr, cfg.ObserverAddr)
		}
	}
}

// TestLoadConfig_TLSPairOrNeither: TLS is opt-in and both halves are required.
// One-without-the-other is a boot error, never a silent fallback to plaintext —
// an operator who set one and typoed the other must not get a plaintext listener.
func TestLoadConfig_TLSPairOrNeither(t *testing.T) {
	cases := []struct {
		name    string
		cert    string
		key     string
		wantErr bool
	}{
		{"neither — off, the default", "", "", false},
		{"both set", "/etc/shn/tls/tls.crt", "/etc/shn/tls/tls.key", false},
		{"cert without key", "/etc/shn/tls/tls.crt", "", true},
		{"key without cert", "", "/etc/shn/tls/tls.key", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := map[string]string{
				"ROLE":              "provider",
				"SHN_SECRETS":       "/etc/shn/bundles/provider",
				"SHN_DISCOVERY_URL": "http://accounts:8088/discovery",
				"TLS_CERT_FILE":     c.cert,
				"TLS_KEY_FILE":      c.key,
			}
			cfg, err := loadConfig(func(k string) string { return env[k] })
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error, got cfg %+v", cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			if cfg.TLSCertFile != c.cert || cfg.TLSKeyFile != c.key {
				t.Fatalf("cfg TLS = (%q, %q), want (%q, %q)",
					cfg.TLSCertFile, cfg.TLSKeyFile, c.cert, c.key)
			}
		})
	}
}

// TestLoadConfig_MetricsDefaults: METRICS_SERVICE off by default (the
// published-binary default); namespace/env still get their compiled defaults
// so an operator only has to set METRICS_SERVICE to opt in.
func TestLoadConfig_MetricsDefaults(t *testing.T) {
	env := map[string]string{
		"ROLE":              "provider",
		"SHN_SECRETS":       "/etc/shn/bundles/provider",
		"SHN_DISCOVERY_URL": "http://accounts:8088/discovery",
	}
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.MetricsService != "" {
		t.Fatalf("MetricsService default must be empty (off), got %q", cfg.MetricsService)
	}
	if cfg.MetricsNamespace != "SHN/Preview" || cfg.MetricsEnv != "shn-preview" {
		t.Fatalf("metrics defaults wrong: ns=%q env=%q", cfg.MetricsNamespace, cfg.MetricsEnv)
	}
}

// TestLoadConfig_MetricsServiceReadThrough: METRICS_SERVICE/METRICS_NAMESPACE/
// METRICS_ENV are all read through verbatim when set.
func TestLoadConfig_MetricsServiceReadThrough(t *testing.T) {
	env := map[string]string{
		"ROLE":              "provider",
		"SHN_SECRETS":       "/etc/shn/bundles/provider",
		"SHN_DISCOVERY_URL": "http://accounts:8088/discovery",
		"METRICS_SERVICE":   "provider-data-gw",
		"METRICS_NAMESPACE": "X",
		"METRICS_ENV":       "Y",
	}
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.MetricsService != "provider-data-gw" || cfg.MetricsNamespace != "X" || cfg.MetricsEnv != "Y" {
		t.Fatalf("metrics read-through wrong: service=%q ns=%q env=%q", cfg.MetricsService, cfg.MetricsNamespace, cfg.MetricsEnv)
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

func TestLoadConfig_PayerDavinciPartialQuadIsError(t *testing.T) {
	env := map[string]string{
		"ROLE": "payer", "SHN_SECRETS": "/x", "SHN_DISCOVERY_URL": "https://d",
		"PAYER_DAVINCI_BASE_URL":  "https://payer.example",
		"PAYER_DAVINCI_TOKEN_URL": "https://payer.example/token",
		// CLIENT_ID / CLIENT_KEY deliberately missing → hard error.
	}
	_, err := loadConfig(func(k string) string { return env[k] })
	if err == nil || !strings.Contains(err.Error(), "PAYER_DAVINCI_TOKEN_URL requires") {
		t.Fatalf("want partial-quad error, got %v", err)
	}
}

func TestLoadConfig_PayerDavinciBaseOnlyIsOK(t *testing.T) {
	env := map[string]string{
		"ROLE": "payer", "SHN_SECRETS": "/x", "SHN_DISCOVERY_URL": "https://d",
		"PAYER_DAVINCI_BASE_URL": "https://payer.example", // zero creds → deliberate unauthenticated
	}
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("base-only should load: %v", err)
	}
	if cfg.PayerDavinciBaseURL != "https://payer.example" || cfg.PayerDavinciTokenURL != "" {
		t.Errorf("cfg = %+v", cfg)
	}
}

// TestLoadConfig_PayerDavinciContractVersions: the per-peer declared-versions
// block (spec 2026-08-10 §3 path 2, "seeded by config"). Tokens follow the
// registrar admission grammar; malformed or base-less declarations are BOOT
// failures, not runtime surprises.
func TestLoadConfig_PayerDavinciContractVersions(t *testing.T) {
	// happy: parsed, split, whitespace-trimmed
	env := map[string]string{
		"ROLE": "payer", "SHN_SECRETS": "/x", "SHN_DISCOVERY_URL": "https://d",
		"PAYER_DAVINCI_BASE_URL":          "https://payer.example",
		"PAYER_DAVINCI_CONTRACT_VERSIONS": "pa.pas@2.0, pa.crd@2.0",
	}
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("happy path should load: %v", err)
	}
	if len(cfg.PayerDavinciContractVersions) != 2 || cfg.PayerDavinciContractVersions[0] != "pa.pas@2.0" || cfg.PayerDavinciContractVersions[1] != "pa.crd@2.0" {
		t.Fatalf("parsed = %v", cfg.PayerDavinciContractVersions)
	}

	// malformed token → boot error
	env = map[string]string{
		"ROLE": "payer", "SHN_SECRETS": "/x", "SHN_DISCOVERY_URL": "https://d",
		"PAYER_DAVINCI_BASE_URL":          "https://payer.example",
		"PAYER_DAVINCI_CONTRACT_VERSIONS": "PA.PAS@2.0",
	}
	if _, err := loadConfig(func(k string) string { return env[k] }); err == nil || !strings.Contains(err.Error(), "PAYER_DAVINCI_CONTRACT_VERSIONS") {
		t.Fatalf("want malformed-token error, got %v", err)
	}

	// declared without a base URL → boot error (nothing to verify against)
	env = map[string]string{
		"ROLE": "payer", "SHN_SECRETS": "/x", "SHN_DISCOVERY_URL": "https://d",
		"PAYER_DAVINCI_CONTRACT_VERSIONS": "pa.pas@2.0",
	}
	if _, err := loadConfig(func(k string) string { return env[k] }); err == nil || !strings.Contains(err.Error(), "PAYER_DAVINCI_CONTRACT_VERSIONS") {
		t.Fatalf("want base-URL-required error, got %v", err)
	}
}

// SHN_DEMO_EGRESS_NATIVE_LINES: empty = unset (SHN_CONTRACT_VERSIONS parser
// precedent — a copycat parse must handle this deliberately); unknown line =
// boot refusal; valid lines land on engine Config.EgressNativeLines (via
// config.DemoEgressNativeLines, wired in build()). The boot log build() emits
// when the knob is set is NOT asserted here: build() fails before reaching
// gwCfg construction with this test file's minimal env (no live discovery),
// and no stdout-capturing build() harness exists yet elsewhere in this file —
// per the task brief, the config-field assertion below stands in for it
// rather than growing new harness machinery for one log line.
func TestDemoEgressNativeLinesEnv(t *testing.T) {
	baseEnv := map[string]string{
		"ROLE": "provider", "SHN_SECRETS": "/x", "SHN_DISCOVERY_URL": "https://d",
	}

	t.Run("empty is unset", func(t *testing.T) {
		env := map[string]string{}
		for k, v := range baseEnv {
			env[k] = v
		}
		env["SHN_DEMO_EGRESS_NATIVE_LINES"] = ""
		cfg, err := loadConfig(func(k string) string { return env[k] })
		if err != nil {
			t.Fatalf("empty env should load: %v", err)
		}
		if cfg.DemoEgressNativeLines != nil {
			t.Fatalf("DemoEgressNativeLines = %v, want nil (unset)", cfg.DemoEgressNativeLines)
		}
	})

	t.Run("unknown line refuses boot", func(t *testing.T) {
		env := map[string]string{}
		for k, v := range baseEnv {
			env[k] = v
		}
		env["SHN_DEMO_EGRESS_NATIVE_LINES"] = "9.9"
		_, err := loadConfig(func(k string) string { return env[k] })
		if err == nil || !strings.Contains(err.Error(), "SHN_DEMO_EGRESS_NATIVE_LINES") {
			t.Fatalf("want an SHN_DEMO_EGRESS_NATIVE_LINES-naming refusal, got %v", err)
		}
	})

	t.Run("set but no parseable line refuses boot", func(t *testing.T) {
		// "," (or any all-separator value) must never be a silent no-op: the
		// knob's whole premise is loudness, and a set-but-empty parse leaving
		// DemoEgressNativeLines nil would run un-narrowed while the operator
		// believes the demo narrowing is on.
		env := map[string]string{}
		for k, v := range baseEnv {
			env[k] = v
		}
		env["SHN_DEMO_EGRESS_NATIVE_LINES"] = ","
		_, err := loadConfig(func(k string) string { return env[k] })
		if err == nil || !strings.Contains(err.Error(), "SHN_DEMO_EGRESS_NATIVE_LINES") {
			t.Fatalf("want an SHN_DEMO_EGRESS_NATIVE_LINES-naming refusal for a no-line value, got %v", err)
		}
	})

	t.Run("valid narrows", func(t *testing.T) {
		env := map[string]string{}
		for k, v := range baseEnv {
			env[k] = v
		}
		env["SHN_DEMO_EGRESS_NATIVE_LINES"] = "2.0"
		cfg, err := loadConfig(func(k string) string { return env[k] })
		if err != nil {
			t.Fatalf("valid line should load: %v", err)
		}
		if len(cfg.DemoEgressNativeLines) != 1 || cfg.DemoEgressNativeLines[0] != "2.0" {
			t.Fatalf("DemoEgressNativeLines = %v, want [\"2.0\"]", cfg.DemoEgressNativeLines)
		}
	})
}

func TestLoadConfig_ProviderDTRNativeRequiresPopulateURL(t *testing.T) {
	env := map[string]string{
		"ROLE": "provider", "SHN_SECRETS": "/x", "SHN_DISCOVERY_URL": "https://d",
		"PROVIDER_DTR_NATIVE": "true",
		// PROVIDER_DTR_POPULATE_URL deliberately unset.
	}
	_, err := loadConfig(func(k string) string { return env[k] })
	if err == nil {
		t.Fatal("want error: PROVIDER_DTR_NATIVE=true without PROVIDER_DTR_POPULATE_URL")
	}
}

// A malformed PROVIDER_DTR_POPULATE_URL fails loud at build (checkOptionalURL),
// consistent with every other URL field — and regardless of mode (the loop validates
// any set URL; NATIVE off here proves the well-formedness check is independent of the
// both-or-neither presence check).
func TestLoadConfig_ProviderDTRPopulateURLMalformed(t *testing.T) {
	env := map[string]string{
		"ROLE": "provider", "SHN_SECRETS": "/x", "SHN_DISCOVERY_URL": "https://d",
		"PROVIDER_DTR_POPULATE_URL": "notaurl", // scheme-less → rejected by checkOptionalURL
	}
	_, err := loadConfig(func(k string) string { return env[k] })
	if err == nil {
		t.Fatal("want error: malformed PROVIDER_DTR_POPULATE_URL")
	}
}

// ---- Ingress config tests ----

// baseProviderEnv returns the minimum env that reaches the ingress validation block:
// ROLE=provider, SHN_SECRETS (required by loadConfig), SHN_DISCOVERY_URL (required),
// and PROVIDER_DAVINCI_INGRESS=1 (enables the ingress block we want to test).
func baseProviderEnv() map[string]string {
	return map[string]string{
		"ROLE":                     "provider",
		"SHN_SECRETS":              "/etc/shn",
		"SHN_DISCOVERY_URL":        "https://disc.test",
		"PROVIDER_DAVINCI_INGRESS": "1",
	}
}

// writeClientsFile writes body to a temp file and returns its path.
func writeClientsFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "clients.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// testValidClientsJSON generates a real ES384 SubjectPublicKeyInfo PEM, embeds it
// in a JSON registration array, and returns the JSON string. Generating the PEM in
// Go avoids truncated-PEM literals that wouldn't parse.
func testValidClientsJSON(t *testing.T) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ES384 key: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatalf("marshal PKIX pubkey: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})

	type clientEntry struct {
		ClientID     string   `json:"client_id"`
		Alg          string   `json:"alg"`
		PublicKeyPEM string   `json:"public_key_pem"`
		Scopes       []string `json:"scopes"`
	}
	entries := []clientEntry{
		{
			ClientID:     "br-provider",
			Alg:          "ES384",
			PublicKeyPEM: string(pemBytes),
			Scopes:       []string{"system/Davinci.write"},
		},
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshal clients JSON: %v", err)
	}
	return string(raw)
}

// TestLoadConfig_IngressRequiresBaseURLAndClients: ingress on, but no base URL / no
// clients → hard boot error (all-or-nothing FR-G13 posture).
func TestLoadConfig_IngressRequiresBaseURLAndClients(t *testing.T) {
	e := baseProviderEnv()
	// No PROVIDER_DAVINCI_INGRESS_BASE_URL; no INGRESS_CLIENTS_FILE.
	if _, err := loadConfig(func(k string) string { return e[k] }); err == nil {
		t.Fatal("ingress enabled with no base URL/clients: want error, got nil")
	}
}

// TestLoadConfig_IngressBaseURLMalformed: a scheme-less base URL fails the
// checkOptionalURL loop before reaching the ingress block.
func TestLoadConfig_IngressBaseURLMalformed(t *testing.T) {
	e := baseProviderEnv()
	e["PROVIDER_DAVINCI_INGRESS_BASE_URL"] = "notaurl"
	e["INGRESS_CLIENTS_FILE"] = writeClientsFile(t, testValidClientsJSON(t))
	_, err := loadConfig(func(k string) string { return e[k] })
	if err == nil {
		t.Fatal("malformed PROVIDER_DAVINCI_INGRESS_BASE_URL: want error, got nil")
	}
	if !strings.Contains(err.Error(), "PROVIDER_DAVINCI_INGRESS_BASE_URL") {
		t.Fatalf("error should reference the env var name, got: %v", err)
	}
}

// TestLoadConfig_IngressBaseURLSetButNoClients: the most likely misconfig — operator
// sets PROVIDER_DAVINCI_INGRESS and PROVIDER_DAVINCI_INGRESS_BASE_URL but forgets
// INGRESS_CLIENTS_FILE → hard error on the zero-clients branch.
func TestLoadConfig_IngressBaseURLSetButNoClients(t *testing.T) {
	e := baseProviderEnv()
	e["PROVIDER_DAVINCI_INGRESS_BASE_URL"] = "https://gw.test"
	// INGRESS_CLIENTS_FILE deliberately unset.
	_, err := loadConfig(func(k string) string { return e[k] })
	if err == nil {
		t.Fatal("ingress with base URL but no clients file: want error, got nil")
	}
	if !strings.Contains(err.Error(), "INGRESS_CLIENTS_FILE") {
		t.Fatalf("error should reference INGRESS_CLIENTS_FILE, got: %v", err)
	}
}

// TestLoadConfig_IngressRejectsBadAlg: alg HS256 is rejected outright (only ES384|RS384 allowed).
func TestLoadConfig_IngressRejectsBadAlg(t *testing.T) {
	e := baseProviderEnv()
	e["PROVIDER_DAVINCI_INGRESS_BASE_URL"] = "https://gw.test"
	e["INGRESS_CLIENTS_FILE"] = writeClientsFile(t,
		`[{"client_id":"x","alg":"HS256","public_key_pem":"irrelevant"}]`)
	if _, err := loadConfig(func(k string) string { return e[k] }); err == nil {
		t.Fatal("bad-alg (HS256): want error, got nil")
	}
}

// TestLoadConfig_IngressRejectsBadPEM: valid alg (ES384) but malformed PEM → hard error.
func TestLoadConfig_IngressRejectsBadPEM(t *testing.T) {
	e := baseProviderEnv()
	e["PROVIDER_DAVINCI_INGRESS_BASE_URL"] = "https://gw.test"
	e["INGRESS_CLIENTS_FILE"] = writeClientsFile(t,
		`[{"client_id":"x","alg":"ES384","public_key_pem":"nope"}]`)
	if _, err := loadConfig(func(k string) string { return e[k] }); err == nil {
		t.Fatal("bad-PEM (ES384/nope): want error, got nil")
	}
}

// TestLoadConfig_IngressValid: base URL + a real ES384 registration → success.
func TestLoadConfig_IngressValid(t *testing.T) {
	e := baseProviderEnv()
	e["PROVIDER_DAVINCI_INGRESS_BASE_URL"] = "https://gw.test"
	e["INGRESS_CLIENTS_FILE"] = writeClientsFile(t, testValidClientsJSON(t))
	cfg, err := loadConfig(func(k string) string { return e[k] })
	if err != nil {
		t.Fatalf("valid ingress config: %v", err)
	}
	if len(cfg.IngressClients) != 1 {
		t.Fatalf("IngressClients: want 1, got %d", len(cfg.IngressClients))
	}
	if cfg.IngressBaseURL != "https://gw.test" {
		t.Fatalf("IngressBaseURL = %q, want %q", cfg.IngressBaseURL, "https://gw.test")
	}
}

// ---- provider-data origination config tests ----

// TestLoadConfig_ProviderDataRequiresPopulateURL: ORIGINATION_PROFILE=provider-data
// without PROVIDER_DTR_POPULATE_URL is a boot error (the operated $populate endpoint
// is the crux of the provider-data lane).
func TestLoadConfig_ProviderDataRequiresPopulateURL(t *testing.T) {
	e := map[string]string{
		"ROLE":                "provider",
		"SHN_SECRETS":         "/x",
		"SHN_DISCOVERY_URL":   "https://d",
		"ORIGINATION_PROFILE": "provider-data",
		// PROVIDER_DTR_POPULATE_URL deliberately unset.
	}
	_, err := loadConfig(func(k string) string { return e[k] })
	if err == nil {
		t.Fatal("want error: ORIGINATION_PROFILE=provider-data without PROVIDER_DTR_POPULATE_URL")
	}
	if !strings.Contains(err.Error(), "PROVIDER_DTR_POPULATE_URL") {
		t.Fatalf("error should reference PROVIDER_DTR_POPULATE_URL, got: %v", err)
	}
}

// TestLoadConfig_ProviderDataWithPopulateURLIsOK: the minimum valid provider-data config
// loads cleanly and the OriginationProfile is carried.
func TestLoadConfig_ProviderDataWithPopulateURLIsOK(t *testing.T) {
	e := map[string]string{
		"ROLE":                      "provider",
		"SHN_SECRETS":               "/x",
		"SHN_DISCOVERY_URL":         "https://d",
		"ORIGINATION_PROFILE":       "provider-data",
		"PROVIDER_DTR_POPULATE_URL": "https://populate.test/fhir/Questionnaire/$populate",
	}
	cfg, err := loadConfig(func(k string) string { return e[k] })
	if err != nil {
		t.Fatalf("valid provider-data config: %v", err)
	}
	if cfg.OriginationProfile != "provider-data" {
		t.Fatalf("OriginationProfile = %q, want %q", cfg.OriginationProfile, "provider-data")
	}
	if cfg.ProviderDTRPopulateURL != "https://populate.test/fhir/Questionnaire/$populate" {
		t.Fatalf("ProviderDTRPopulateURL = %q, want full URL", cfg.ProviderDTRPopulateURL)
	}
}

// TestLoadConfig_DispatchEnvVars: PAYER_DAVINCI_DISPATCH_SERVICE_ID and
// PAYER_DAVINCI_DISPATCH_HOOK are carried into the config fields used by
// WithCRDDispatchService (the crd-order-dispatch leg).
func TestLoadConfig_DispatchEnvVars(t *testing.T) {
	e := map[string]string{
		"ROLE":                              "payer",
		"SHN_SECRETS":                       "/x",
		"SHN_DISCOVERY_URL":                 "https://d",
		"PAYER_DAVINCI_DISPATCH_SERVICE_ID": "order-dispatch-crd",
		"PAYER_DAVINCI_DISPATCH_HOOK":       "order-dispatch",
	}
	cfg, err := loadConfig(func(k string) string { return e[k] })
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.PayerDavinciDispatchServiceID != "order-dispatch-crd" {
		t.Fatalf("PayerDavinciDispatchServiceID = %q; want %q", cfg.PayerDavinciDispatchServiceID, "order-dispatch-crd")
	}
	if cfg.PayerDavinciDispatchHook != "order-dispatch" {
		t.Fatalf("PayerDavinciDispatchHook = %q; want %q", cfg.PayerDavinciDispatchHook, "order-dispatch")
	}
}

// TestConvergeRegistry_CarriesPayerIDs verifies convergeRegistry copies a fed
// holder's PayerIDs onto the resulting RegistryEntry (FR-G41) — the gateway's
// converged in-memory registry is FeedPayerRouter's index source, so a payer
// holder's claims must survive the /holders → Registry snapshot.
func TestConvergeRegistry_CarriesPayerIDs(t *testing.T) {
	var enc [32]byte
	enc[0], enc[31] = 7, 9
	var signPub [ed25519.PublicKeySize]byte
	signPub[0] = 3
	holder := shnsdk.Holder{
		ID:       "payer-b",
		Role:     "payer",
		EncPub:   base64.StdEncoding.EncodeToString(enc[:]),
		SignPub:  base64.StdEncoding.EncodeToString(signPub[:]),
		BaseURL:  "https://payer-b.example",
		PayerIDs: []shnsdk.PayerIdentifier{{System: "urn:oid:2.16.840.1.113883.6.300", Value: "00078"}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]shnsdk.Holder{holder})
	}))
	defer srv.Close()

	reg := shnsdk.NewRegistry()
	if _, err := convergeRegistry(context.Background(), http.DefaultClient, srv.URL, reg); err != nil {
		t.Fatalf("convergeRegistry: %v", err)
	}
	entry, ok := reg.Lookup("payer-b")
	if !ok {
		t.Fatal("payer-b missing from converged registry")
	}
	if len(entry.PayerIDs) != 1 || entry.PayerIDs[0] != holder.PayerIDs[0] {
		t.Fatalf("PayerIDs not converged: want %+v, got %+v", holder.PayerIDs, entry.PayerIDs)
	}
}

// TestConvergeRegistry_CarriesMessageFrames verifies convergeRegistry copies a
// fed holder's MessageFrames onto the resulting RegistryEntry — the peer cache
// must thread the feed's self-declared frame capability so the responder-side
// reader (Gateway.frameNegotiated, which looks the requester's entry up in this
// registry) can negotiate on it (opaque-payload frame spec §4). This test proves
// the field survives the /holders → Registry snapshot that reader depends on.
func TestConvergeRegistry_CarriesMessageFrames(t *testing.T) {
	var enc [32]byte
	enc[0], enc[31] = 7, 9
	var signPub [ed25519.PublicKeySize]byte
	signPub[0] = 3
	holder := shnsdk.Holder{
		ID:            "payer-b",
		Role:          "payer",
		EncPub:        base64.StdEncoding.EncodeToString(enc[:]),
		SignPub:       base64.StdEncoding.EncodeToString(signPub[:]),
		BaseURL:       "https://payer-b.example",
		MessageFrames: []string{"v1"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]shnsdk.Holder{holder})
	}))
	defer srv.Close()

	reg := shnsdk.NewRegistry()
	if _, err := convergeRegistry(context.Background(), http.DefaultClient, srv.URL, reg); err != nil {
		t.Fatalf("convergeRegistry: %v", err)
	}
	entry, ok := reg.Lookup("payer-b")
	if !ok {
		t.Fatal("payer-b missing from converged registry")
	}
	if !shnsdk.SupportsMessageFrameV1(entry.MessageFrames) {
		t.Fatalf("MessageFrames not converged: want v1 support, got %v", entry.MessageFrames)
	}
}

// TestConvergeRegistry_CarriesContractVersions verifies convergeRegistry copies
// a fed holder's ContractVersions onto the resulting RegistryEntry — the peer
// cache must thread the feed's self-declared contract lines so the
// version-aware recipient filter can read them (spec 2026-08-10 §4).
func TestConvergeRegistry_CarriesContractVersions(t *testing.T) {
	var enc [32]byte
	enc[0], enc[31] = 7, 9
	var signPub [ed25519.PublicKeySize]byte
	signPub[0] = 3
	holder := shnsdk.Holder{
		ID:               "payer-b",
		Role:             "payer",
		EncPub:           base64.StdEncoding.EncodeToString(enc[:]),
		SignPub:          base64.StdEncoding.EncodeToString(signPub[:]),
		BaseURL:          "https://payer-b.example",
		ContractVersions: []string{"pa.pas@2.0"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]shnsdk.Holder{holder})
	}))
	defer srv.Close()

	reg := shnsdk.NewRegistry()
	if _, err := convergeRegistry(context.Background(), http.DefaultClient, srv.URL, reg); err != nil {
		t.Fatalf("convergeRegistry: %v", err)
	}
	entry, ok := reg.Lookup("payer-b")
	if !ok {
		t.Fatal("payer-b missing from converged registry")
	}
	if len(entry.ContractVersions) != 1 || entry.ContractVersions[0] != "pa.pas@2.0" {
		t.Fatalf("ContractVersions not converged: got %v", entry.ContractVersions)
	}
}

// TestConvergeRegistry_ReturnsCount verifies the count return equals the number
// of holders actually converged into reg (successful reg.Set iterations) — the
// count feeds cell.RecordSuccess(n) so /health's registrar-poller check reports
// a real feed size, not just "no error".
func TestConvergeRegistry_ReturnsCount(t *testing.T) {
	var enc1, enc2 [32]byte
	enc1[0] = 1
	enc2[0] = 2
	holders := []shnsdk.Holder{
		{ID: "h1", Role: "provider", EncPub: base64.StdEncoding.EncodeToString(enc1[:]), BaseURL: "https://h1.example"},
		{ID: "h2", Role: "payer", EncPub: base64.StdEncoding.EncodeToString(enc2[:]), BaseURL: "https://h2.example"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(holders)
	}))
	defer srv.Close()

	reg := shnsdk.NewRegistry()
	n, err := convergeRegistry(context.Background(), http.DefaultClient, srv.URL, reg)
	if err != nil {
		t.Fatalf("convergeRegistry: %v", err)
	}
	if n != 2 {
		t.Fatalf("n = %d, want 2", n)
	}
	if _, ok := reg.Lookup("h1"); !ok {
		t.Fatal("h1 missing from converged registry")
	}
	if _, ok := reg.Lookup("h2"); !ok {
		t.Fatal("h2 missing from converged registry")
	}
}

// TestPollFeed_RecordsCell drives pollFeed against a flip-flop feed (first tick
// 500s, second tick returns a valid one-holder /holders JSON) and asserts the
// PollerCell sees BOTH the transient error (this path's first-ever error
// visibility) and the eventual success.
func TestPollFeed_RecordsCell(t *testing.T) {
	var calls int32
	var enc [32]byte
	enc[0] = 1
	holder := shnsdk.Holder{ID: "h1", Role: "provider", EncPub: base64.StdEncoding.EncodeToString(enc[:]), BaseURL: "https://h1.example"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode([]shnsdk.Holder{holder})
	}))
	defer srv.Close()

	cell := health.NewPollerCell("registrar-poller", time.Minute)
	reg := shnsdk.NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pollFeed(ctx, srv.Client(), srv.URL, reg, 5*time.Millisecond, cell)

	var sawError bool
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c := cell.Check(context.Background())
		if c.Status == health.StatusDegraded && c.LastError != "" {
			sawError = true
		}
		if c.LastSuccess != "" && c.HolderCount == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !sawError {
		t.Fatal("expected the first (500) tick to record a degraded/error state on the cell before success")
	}
	final := cell.Check(context.Background())
	if final.LastSuccess == "" || final.HolderCount != 1 {
		t.Fatalf("final cell state = %+v, want a recorded success with holderCount 1", final)
	}
	if _, ok := reg.Lookup("h1"); !ok {
		t.Fatal("h1 missing from registry after pollFeed converged")
	}
}

func TestLoadConfig_PayerDavinciSecretOnlyIsOK(t *testing.T) {
	env := map[string]string{
		"ROLE": "payer", "SHN_SECRETS": "/x", "SHN_DISCOVERY_URL": "https://d",
		"PAYER_DAVINCI_BASE_URL":      "https://payer.example",
		"PAYER_DAVINCI_TOKEN_URL":     "https://payer.example/token",
		"PAYER_DAVINCI_CLIENT_ID":     "gw",
		"PAYER_DAVINCI_CLIENT_SECRET": "s3cret",
	}
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("secret-only block should load: %v", err)
	}
	if cfg.PayerDavinciClientSecret != "s3cret" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestLoadConfig_PayerDavinciMixedModesIsError(t *testing.T) {
	env := map[string]string{
		"ROLE": "payer", "SHN_SECRETS": "/x", "SHN_DISCOVERY_URL": "https://d",
		"PAYER_DAVINCI_BASE_URL":      "https://payer.example",
		"PAYER_DAVINCI_TOKEN_URL":     "https://payer.example/token",
		"PAYER_DAVINCI_CLIENT_ID":     "gw",
		"PAYER_DAVINCI_CLIENT_KEY":    "/keys/k.pem",
		"PAYER_DAVINCI_CLIENT_ALG":    "ES384",
		"PAYER_DAVINCI_CLIENT_SECRET": "s3cret",
	}
	_, err := loadConfig(func(k string) string { return env[k] })
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("want mixed-modes error, got %v", err)
	}
}

func TestLoadConfig_PayerDavinciAlgAlongsideSecretIsError(t *testing.T) {
	env := map[string]string{
		"ROLE": "payer", "SHN_SECRETS": "/x", "SHN_DISCOVERY_URL": "https://d",
		"PAYER_DAVINCI_BASE_URL":      "https://payer.example",
		"PAYER_DAVINCI_TOKEN_URL":     "https://payer.example/token",
		"PAYER_DAVINCI_CLIENT_ID":     "gw",
		"PAYER_DAVINCI_CLIENT_ALG":    "ES384", // jwt-mode var without its KEY, plus SECRET
		"PAYER_DAVINCI_CLIENT_SECRET": "s3cret",
	}
	_, err := loadConfig(func(k string) string { return env[k] })
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("want mixed-modes error, got %v", err)
	}
}

func TestLoadConfig_FHIRSecretOnlyIsOK(t *testing.T) {
	env := map[string]string{
		"ROLE": "provider", "SHN_SECRETS": "/x", "SHN_DISCOVERY_URL": "https://d",
		"FHIR_DATA_URL":      "https://sor.example/fhir",
		"FHIR_TOKEN_URL":     "https://sor.example/token",
		"FHIR_CLIENT_ID":     "gw",
		"FHIR_CLIENT_SECRET": "s3cret",
	}
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("secret-only FHIR block should load: %v", err)
	}
	if cfg.FHIRClientSecret != "s3cret" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestLoadConfig_FHIRMixedModesIsError(t *testing.T) {
	env := map[string]string{
		"ROLE": "provider", "SHN_SECRETS": "/x", "SHN_DISCOVERY_URL": "https://d",
		"FHIR_DATA_URL":      "https://sor.example/fhir",
		"FHIR_TOKEN_URL":     "https://sor.example/token",
		"FHIR_CLIENT_ID":     "gw",
		"FHIR_CLIENT_KEY":    "/keys/k.pem",
		"FHIR_CLIENT_ALG":    "ES384",
		"FHIR_CLIENT_SECRET": "s3cret",
	}
	_, err := loadConfig(func(k string) string { return env[k] })
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("want mixed-modes error, got %v", err)
	}
}

// buildEnvWithBundle writes a real registration bundle with the given role into a
// temp dir and returns the minimal env for build(). SHN_DISCOVERY_URL points at a
// closed local port so build() fails fast at discovery AFTER the bundle checks —
// hermetic, no live network.
func buildEnvWithBundle(t *testing.T, bundleRole, envRole string) map[string]string {
	t.Helper()
	dir := t.TempDir()
	id, err := shnsdk.GenerateIdentity("h-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := shnsdk.WriteBundle(dir, id, bundleRole, "https://holder.example"); err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"ROLE": envRole, "SHN_SECRETS": dir, "SHN_DISCOVERY_URL": "http://127.0.0.1:1",
	}
}

func TestBuild_RoleBundleMismatchFailsFast(t *testing.T) {
	env := buildEnvWithBundle(t, "payer", "provider")
	_, err := build(context.Background(), func(k string) string { return env[k] }, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "bundle registered as payer") {
		t.Fatalf("want role-mismatch boot error, got %v", err)
	}
}

func TestBuild_RoleBundleMatchPassesCheck(t *testing.T) {
	env := buildEnvWithBundle(t, "provider", "provider")
	_, err := build(context.Background(), func(k string) string { return env[k] }, io.Discard, nil)
	// build() then fails at discovery (closed port) — but NOT with the role error.
	if err == nil || strings.Contains(err.Error(), "registered as") {
		t.Fatalf("matching role must pass the bundle check, got %v", err)
	}
}

func TestBuild_EmptyBundleRoleSkipsCheck(t *testing.T) {
	env := buildEnvWithBundle(t, "", "provider")
	_, err := build(context.Background(), func(k string) string { return env[k] }, io.Discard, nil)
	if err == nil || strings.Contains(err.Error(), "registered as") {
		t.Fatalf("pre-role-stamp bundle must skip the check, got %v", err)
	}
}

// writeTestCert generates a self-signed localhost cert into t.TempDir() and
// returns (certPath, keyPath, pool). Hermetic: no network, no fixture files.
func writeTestCert(t *testing.T) (string, string, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("append cert to pool")
	}
	return certPath, keyPath, pool
}

// TestLoadTLSCert_OffWhenUnset: no config, no cert, no error — TLS is opt-in.
func TestLoadTLSCert_OffWhenUnset(t *testing.T) {
	cert, err := loadTLSCert("", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cert != nil {
		t.Fatalf("cert = %v, want nil when unconfigured", cert)
	}
}

// TestLoadTLSCert_FailsFastOnBadPath: an unreadable cert is a boot error, not a
// serve-time surprise after the gateway already logged "listening".
func TestLoadTLSCert_FailsFastOnBadPath(t *testing.T) {
	_, err := loadTLSCert(filepath.Join(t.TempDir(), "nope.crt"), filepath.Join(t.TempDir(), "nope.key"))
	if err == nil {
		t.Fatal("want error for missing cert files, got nil")
	}
}

// TestServerFor_PlainWhenNoCert: nil cert => no TLSConfig, unchanged default.
func TestServerFor_PlainWhenNoCert(t *testing.T) {
	s := serverFor("127.0.0.1:0", http.NewServeMux(), nil)
	if s.TLSConfig != nil {
		t.Fatalf("TLSConfig = %v, want nil for a plaintext listener", s.TLSConfig)
	}
}

// TestApp_ChecksEndpoint_TokenGatedAndHealthUnaffected boots the app for real
// (build(), not Run() — no live listener/registrar poller needed) with
// CHECKS_TOKEN set and a fake FHIR SoR, then drives /internal/checks and
// /health over the SAME wrapped handler build() returns (the health.Wrap
// seam checks.Handler is spliced into, app.go build()). Asserts: an
// unauthenticated request is refused; a bearer-authenticated POST runs the
// probes and surfaces a CapabilityStatement result for FHIR_DATA_URL; and
// /health stays healthy even though AUDIT_URL — a well-formed but
// unreachable target, probed as plain "reachable" per checkTargets'
// kind overlay — fails its probe, because checks are never registered as a
// health.Check.
func TestApp_ChecksEndpoint_TokenGatedAndHealthUnaffected(t *testing.T) {
	fhirSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/metadata") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"resourceType":"CapabilityStatement","fhirVersion":"4.0.1"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer fhirSrv.Close()

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyBody := fmt.Sprintf(`{"pubkey":%q}`, base64.StdEncoding.EncodeToString(pub))
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(keyBody))
	}))
	defer keys.Close()
	disc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"endpoints":{},"authzPublicKeyURL":%q,"hubTransportKeyURL":%q}`, keys.URL, keys.URL)
	}))
	defer disc.Close()

	dir := t.TempDir()
	id, err := shnsdk.GenerateIdentity("h-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := shnsdk.WriteBundle(dir, id, "provider", "https://holder.example"); err != nil {
		t.Fatal(err)
	}

	env := map[string]string{
		"ROLE":               "provider",
		"SHN_SECRETS":        dir,
		"SHN_DISCOVERY_URL":  disc.URL,
		"SHN_FAKE_VALIDATOR": "1",
		"FHIR_DATA_URL":      fhirSrv.URL,
		"CHECKS_TOKEN":       "t",
		"AUDIT_URL":          "http://127.0.0.1:1", // well-formed, unreachable
	}
	getenv := func(k string) string { return env[k] }

	b, err := build(context.Background(), getenv, io.Discard, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	srv := httptest.NewServer(b.handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/internal/checks")
	if err != nil {
		t.Fatalf("GET /internal/checks: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET status = %d, want 401", resp.StatusCode)
	}

	postReq, err := http.NewRequest(http.MethodPost, srv.URL+"/internal/checks", nil)
	if err != nil {
		t.Fatal(err)
	}
	postReq.Header.Set("Authorization", "Bearer t")
	resp, err = http.DefaultClient.Do(postReq)
	if err != nil {
		t.Fatalf("POST /internal/checks: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated POST status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Results []checks.Result `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var sawFHIR, sawAudit bool
	for _, r := range body.Results {
		switch r.ID {
		case "FHIR_DATA_URL":
			sawFHIR = true
			if !r.OK || !strings.Contains(r.Detail, "CapabilityStatement") {
				t.Errorf("FHIR_DATA_URL result = %+v, want OK with a CapabilityStatement detail", r)
			}
		case "AUDIT_URL":
			sawAudit = true
			if r.OK {
				t.Errorf("AUDIT_URL result = %+v, want !OK (unreachable target)", r)
			}
		}
	}
	if !sawFHIR {
		t.Fatalf("no FHIR_DATA_URL result in %+v", body.Results)
	}
	if !sawAudit {
		t.Fatalf("no AUDIT_URL result in %+v", body.Results)
	}

	healthResp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer healthResp.Body.Close()
	if healthResp.StatusCode != http.StatusOK {
		t.Fatalf("/health status = %d, want 200 (healthy) despite the failing AUDIT_URL probe", healthResp.StatusCode)
	}
}

// TestProbeEvidenceReachesResponder (per-line endpoint evidence): the REAL
// app-wiring hook, end to end — build() wires checksRunner.OnResults into
// the native responder's SetEndpointEvidence (gateway/app/app.go, right
// after checksRunner is constructed, since the responder is built earlier at
// the PAYER_DAVINCI_BASE_URL block above it); a genuine POST /internal/checks
// (the SAME live path an operator or cloudctl drives) runs the probes for
// real against a fake payer serving davinci-configuration, and the evidence
// must land on the built native responder — read back via
// EndpointEvidenceForTest, never injected through a second, test-only route.
func TestProbeEvidenceReachesResponder(t *testing.T) {
	var payerURL string
	payer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/davinci-configuration" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"endpoints":{"davinci_dtr_qpackage_endpoint#2.2":%q}}`,
				payerURL+"/Questionnaire/$questionnaire-package-v22")
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer payer.Close()
	payerURL = payer.URL

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyBody := fmt.Sprintf(`{"pubkey":%q}`, base64.StdEncoding.EncodeToString(pub))
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(keyBody))
	}))
	defer keys.Close()
	disc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"endpoints":{},"authzPublicKeyURL":%q,"hubTransportKeyURL":%q}`, keys.URL, keys.URL)
	}))
	defer disc.Close()

	dir := t.TempDir()
	id, err := shnsdk.GenerateIdentity("h-test-payer")
	if err != nil {
		t.Fatal(err)
	}
	if err := shnsdk.WriteBundle(dir, id, "payer", "https://holder.example"); err != nil {
		t.Fatal(err)
	}

	env := map[string]string{
		"ROLE":                         "payer",
		"SHN_SECRETS":                  dir,
		"SHN_DISCOVERY_URL":            disc.URL,
		"SHN_FAKE_VALIDATOR":           "1",
		"PAYER_DAVINCI_BASE_URL":       payer.URL,
		"PAYER_DAVINCI_CRD_SERVICE_ID": "svc", // override: skip live /cds-services discovery
		"CHECKS_TOKEN":                 "t",
	}
	getenv := func(k string) string { return env[k] }

	b, err := build(context.Background(), getenv, io.Discard, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if b.nativeResponder == nil {
		t.Fatal("built.nativeResponder is nil — expected native-forward mode to build one (PAYER_DAVINCI_BASE_URL set)")
	}
	reader, ok := b.nativeResponder.(interface{ EndpointEvidenceForTest() map[string]string })
	if !ok {
		t.Fatalf("nativeResponder %T does not expose EndpointEvidenceForTest", b.nativeResponder)
	}
	if got := reader.EndpointEvidenceForTest(); len(got) != 0 {
		t.Fatalf("evidence before any checks cycle = %v, want empty", got)
	}

	srv := httptest.NewServer(b.handler)
	defer srv.Close()

	postReq, err := http.NewRequest(http.MethodPost, srv.URL+"/internal/checks", nil)
	if err != nil {
		t.Fatal(err)
	}
	postReq.Header.Set("Authorization", "Bearer t")
	resp, err := http.DefaultClient.Do(postReq)
	if err != nil {
		t.Fatalf("POST /internal/checks: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /internal/checks status = %d, want 200", resp.StatusCode)
	}

	want := payerURL + "/Questionnaire/$questionnaire-package-v22"
	got := reader.EndpointEvidenceForTest()
	if got["pa.dtr@2.2"] != want {
		t.Fatalf("evidence after the checks cycle = %v, want pa.dtr@2.2 -> %q", got, want)
	}
}

// TestFhirTokenFetch_MintsFreshTokenPerInvocation pins the fix for
// IMPORTANT-1 (task-18 review): the /internal/checks credential-check
// closure must construct a FRESH *smartauth.TokenSource on every invocation,
// never a shared one hoisted outside the closure — a shared TokenSource
// would serve its cached (still-valid) token instead of re-authenticating,
// so a mid-window secret rotation or IdP outage would go unreported for up
// to TTL-RefreshSkew. Calls the closure directly twice in immediate
// succession (nothing at this layer imposes the Runner's 30s cooldown —
// that cooldown is what bounds real-world mint frequency, per
// checks.Runner.Run) and asserts the token endpoint was hit twice: this test
// FAILS if fhirTokenFetch reverts to a shared/hoisted TokenSource (hits
// would be 1, the second call served from cache).
func TestFhirTokenFetch_MintsFreshTokenPerInvocation(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":3600}`))
	}))
	defer srv.Close()

	cfg := config{
		FHIRTokenURL:     srv.URL,
		FHIRClientID:     "gw",
		FHIRClientSecret: "s3cret",
		FHIRClientScope:  "system/*.read",
	}
	fetch := fhirTokenFetch(cfg)

	if err := fetch(context.Background()); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if err := fetch(context.Background()); err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("token endpoint hits = %d, want 2 (each invocation must mint fresh, not serve a cached token)", got)
	}
}

// TestPayerDavinciTokenFetch_MintsFreshTokenPerInvocation is
// TestFhirTokenFetch_MintsFreshTokenPerInvocation's PAYER_DAVINCI_*
// counterpart — same fix, same regression shape.
func TestPayerDavinciTokenFetch_MintsFreshTokenPerInvocation(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":3600}`))
	}))
	defer srv.Close()

	cfg := config{
		PayerDavinciTokenURL:     srv.URL,
		PayerDavinciClientID:     "gw",
		PayerDavinciClientSecret: "s3cret",
		PayerDavinciScope:        "system/*.read",
	}
	fetch := payerDavinciTokenFetch(cfg)

	if err := fetch(context.Background()); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if err := fetch(context.Background()); err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("token endpoint hits = %d, want 2 (each invocation must mint fresh, not serve a cached token)", got)
	}
}

// TestServerFor_RealTLSHandshake: a real client completes a real handshake
// against the configured cert and gets the handler's response. Loopback only.
func TestServerFor_RealTLSHandshake(t *testing.T) {
	certPath, keyPath, pool := writeTestCert(t)
	cert, err := loadTLSCert(certPath, keyPath)
	if err != nil {
		t.Fatalf("loadTLSCert: %v", err)
	}
	if cert == nil {
		t.Fatal("cert = nil, want a loaded keypair")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("pong"))
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := serverFor(ln.Addr().String(), mux, cert)
	if srv.TLSConfig == nil {
		t.Fatal("TLSConfig = nil, want TLS configured")
	}
	if srv.TLSConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %x, want TLS 1.2 (%x)", srv.TLSConfig.MinVersion, tls.VersionTLS12)
	}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}}
	resp, err := client.Get("https://" + ln.Addr().String() + "/ping")
	if err != nil {
		t.Fatalf("https GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestCheckTargets_PayerDavinciWellKnown: checkTargets derives the
// PAYER_DAVINCI_WELL_KNOWN companion target from PAYER_DAVINCI_BASE_URL —
// same URL, same DeclaredVersions, KindDavinciConfig — alongside the base
// target itself (KindFHIRMetadata). Neither target's URL is independently
// configured (app.go's one-table invariant).
func TestCheckTargets_PayerDavinciWellKnown(t *testing.T) {
	find := func(targets []checks.Target, id string) (checks.Target, bool) {
		for _, tgt := range targets {
			if tgt.ID == id {
				return tgt, true
			}
		}
		return checks.Target{}, false
	}

	t.Run("base set", func(t *testing.T) {
		cfg := config{
			PayerDavinciBaseURL:          "https://payer.example/fhir",
			PayerDavinciContractVersions: []string{"pa.pas@2.2", "pa.dtr@2.0"},
		}
		targets := checkTargets(cfg)

		base, ok := find(targets, "PAYER_DAVINCI_BASE_URL")
		if !ok {
			t.Fatal("PAYER_DAVINCI_BASE_URL target missing")
		}
		if base.Kind != checks.KindFHIRMetadata {
			t.Fatalf("base Kind = %q, want %q", base.Kind, checks.KindFHIRMetadata)
		}
		if base.URL != cfg.PayerDavinciBaseURL {
			t.Fatalf("base URL = %q, want %q", base.URL, cfg.PayerDavinciBaseURL)
		}
		if strings.Join(base.DeclaredVersions, ",") != strings.Join(cfg.PayerDavinciContractVersions, ",") {
			t.Fatalf("base DeclaredVersions = %v, want %v", base.DeclaredVersions, cfg.PayerDavinciContractVersions)
		}

		wellKnown, ok := find(targets, "PAYER_DAVINCI_WELL_KNOWN")
		if !ok {
			t.Fatal("PAYER_DAVINCI_WELL_KNOWN target missing")
		}
		if wellKnown.Kind != checks.KindDavinciConfig {
			t.Fatalf("well-known Kind = %q, want %q", wellKnown.Kind, checks.KindDavinciConfig)
		}
		if wellKnown.URL != cfg.PayerDavinciBaseURL {
			t.Fatalf("well-known URL = %q, want %q (same base URL)", wellKnown.URL, cfg.PayerDavinciBaseURL)
		}
		if strings.Join(wellKnown.DeclaredVersions, ",") != strings.Join(cfg.PayerDavinciContractVersions, ",") {
			t.Fatalf("well-known DeclaredVersions = %v, want %v", wellKnown.DeclaredVersions, cfg.PayerDavinciContractVersions)
		}
	})

	t.Run("base unset", func(t *testing.T) {
		targets := checkTargets(config{})
		if _, ok := find(targets, "PAYER_DAVINCI_BASE_URL"); ok {
			t.Fatal("PAYER_DAVINCI_BASE_URL target present with no base URL configured")
		}
		if _, ok := find(targets, "PAYER_DAVINCI_WELL_KNOWN"); ok {
			t.Fatal("PAYER_DAVINCI_WELL_KNOWN target present with no base URL configured")
		}
	})
}
