package app

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	if err := convergeRegistry(context.Background(), http.DefaultClient, srv.URL, reg); err != nil {
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
