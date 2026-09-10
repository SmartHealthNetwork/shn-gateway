package app

// populate_smart_test.go: the PROVIDER_DTR_POPULATE_* SMART Backend Services
// credential block — the operated $populate connector's own quad, with the
// same exactly-one-mode, both-or-neither fail-loud posture as PAYER_DAVINCI_*
// and FHIR_*. Each row mirrors an existing PAYER_DAVINCI_* row (app_test.go) so
// the three credential blocks cannot drift apart.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	checks "github.com/SmartHealthNetwork/shn-gateway/checks"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// populateQuadEnv is a provider env carrying a complete private_key_jwt populate
// block; rows mutate one variable at a time.
func populateQuadEnv() map[string]string {
	return map[string]string{
		"ROLE": "provider", "SHN_SECRETS": "/x", "SHN_DISCOVERY_URL": "https://d",
		"PROVIDER_DTR_POPULATE_URL":        "https://populate.test/fhir/Questionnaire/$populate",
		"PROVIDER_DTR_POPULATE_TOKEN_URL":  "https://populate.test/token",
		"PROVIDER_DTR_POPULATE_CLIENT_ID":  "populate-gw",
		"PROVIDER_DTR_POPULATE_CLIENT_KEY": "/keys/populate.pem",
		"PROVIDER_DTR_POPULATE_CLIENT_ALG": "ES384",
	}
}

func loadEnv(env map[string]string) (config, error) {
	return loadConfig(func(k string) string { return env[k] })
}

func TestLoadConfig_ProviderDTRPopulateQuadIsOK(t *testing.T) {
	cfg, err := loadEnv(populateQuadEnv())
	if err != nil {
		t.Fatalf("complete private_key_jwt block should load: %v", err)
	}
	if cfg.ProviderDTRPopulateTokenURL != "https://populate.test/token" ||
		cfg.ProviderDTRPopulateClientID != "populate-gw" ||
		cfg.ProviderDTRPopulateClientKey != "/keys/populate.pem" ||
		cfg.ProviderDTRPopulateClientAlg != "ES384" {
		t.Errorf("cfg = %+v", cfg)
	}
	if cfg.ProviderDTRPopulateScope != "system/*.read" {
		t.Errorf("scope default = %q, want system/*.read", cfg.ProviderDTRPopulateScope)
	}
}

func TestLoadConfig_ProviderDTRPopulateScopeAndKIDLoad(t *testing.T) {
	env := populateQuadEnv()
	env["PROVIDER_DTR_POPULATE_SCOPE"] = "system/Questionnaire.read"
	env["PROVIDER_DTR_POPULATE_CLIENT_KID"] = "kid-1"
	cfg, err := loadEnv(env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ProviderDTRPopulateScope != "system/Questionnaire.read" || cfg.ProviderDTRPopulateClientKID != "kid-1" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestLoadConfig_ProviderDTRPopulateSecretOnlyIsOK(t *testing.T) {
	env := populateQuadEnv()
	delete(env, "PROVIDER_DTR_POPULATE_CLIENT_KEY")
	delete(env, "PROVIDER_DTR_POPULATE_CLIENT_ALG")
	env["PROVIDER_DTR_POPULATE_CLIENT_SECRET"] = "s3cret"
	cfg, err := loadEnv(env)
	if err != nil {
		t.Fatalf("secret-only block should load: %v", err)
	}
	if cfg.ProviderDTRPopulateClientSecret != "s3cret" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestLoadConfig_ProviderDTRPopulatePartialQuadIsError(t *testing.T) {
	env := populateQuadEnv()
	delete(env, "PROVIDER_DTR_POPULATE_CLIENT_ID")
	delete(env, "PROVIDER_DTR_POPULATE_CLIENT_KEY")
	delete(env, "PROVIDER_DTR_POPULATE_CLIENT_ALG")
	_, err := loadEnv(env)
	if err == nil || !strings.Contains(err.Error(), "PROVIDER_DTR_POPULATE_TOKEN_URL requires PROVIDER_DTR_POPULATE_CLIENT_ID") {
		t.Fatalf("want partial-quad error, got %v", err)
	}
}

func TestLoadConfig_ProviderDTRPopulateTokenURLWithoutCredentialsIsError(t *testing.T) {
	env := populateQuadEnv()
	delete(env, "PROVIDER_DTR_POPULATE_CLIENT_KEY")
	delete(env, "PROVIDER_DTR_POPULATE_CLIENT_ALG")
	_, err := loadEnv(env)
	if err == nil || !strings.Contains(err.Error(), "PROVIDER_DTR_POPULATE_TOKEN_URL requires credentials") {
		t.Fatalf("want credentials-required error, got %v", err)
	}
}

func TestLoadConfig_ProviderDTRPopulateTokenURLRequiresPopulateURL(t *testing.T) {
	env := populateQuadEnv()
	delete(env, "PROVIDER_DTR_POPULATE_URL")
	_, err := loadEnv(env)
	if err == nil || !strings.Contains(err.Error(), "PROVIDER_DTR_POPULATE_TOKEN_URL set requires PROVIDER_DTR_POPULATE_URL") {
		t.Fatalf("want token-URL-without-populate-URL error, got %v", err)
	}
}

func TestLoadConfig_ProviderDTRPopulateMixedModesIsError(t *testing.T) {
	env := populateQuadEnv()
	env["PROVIDER_DTR_POPULATE_CLIENT_SECRET"] = "s3cret"
	_, err := loadEnv(env)
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("want mixed-modes error, got %v", err)
	}
}

func TestLoadConfig_ProviderDTRPopulateAlgAlongsideSecretIsError(t *testing.T) {
	env := populateQuadEnv()
	delete(env, "PROVIDER_DTR_POPULATE_CLIENT_KEY")
	env["PROVIDER_DTR_POPULATE_CLIENT_SECRET"] = "s3cret"
	_, err := loadEnv(env)
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("want mixed-modes error, got %v", err)
	}
}

func TestLoadConfig_ProviderDTRPopulateAlgWithoutKeyIsError(t *testing.T) {
	env := populateQuadEnv()
	delete(env, "PROVIDER_DTR_POPULATE_CLIENT_KEY")
	_, err := loadEnv(env)
	if err == nil || !strings.Contains(err.Error(), "PROVIDER_DTR_POPULATE_CLIENT_ALG set requires PROVIDER_DTR_POPULATE_CLIENT_KEY") {
		t.Fatalf("want alg-without-key error, got %v", err)
	}
}

func TestLoadConfig_ProviderDTRPopulateKeyWithoutAlgIsError(t *testing.T) {
	env := populateQuadEnv()
	delete(env, "PROVIDER_DTR_POPULATE_CLIENT_ALG")
	_, err := loadEnv(env)
	// A KEY with no ALG is an alg violation (checkClientAuthMode's existing shape).
	if err == nil || !strings.Contains(err.Error(), "PROVIDER_DTR_POPULATE_CLIENT_ALG must be ES384|RS384") {
		t.Fatalf("want key-without-alg error, got %v", err)
	}
}

func TestLoadConfig_ProviderDTRPopulateBadAlgIsError(t *testing.T) {
	env := populateQuadEnv()
	env["PROVIDER_DTR_POPULATE_CLIENT_ALG"] = "HS256"
	_, err := loadEnv(env)
	if err == nil || !strings.Contains(err.Error(), "must be ES384|RS384") {
		t.Fatalf("want alg error, got %v", err)
	}
}

func TestLoadConfig_ProviderDTRPopulateTokenURLMalformed(t *testing.T) {
	env := populateQuadEnv()
	env["PROVIDER_DTR_POPULATE_TOKEN_URL"] = "notaurl"
	_, err := loadEnv(env)
	if err == nil || !strings.Contains(err.Error(), "PROVIDER_DTR_POPULATE_TOKEN_URL") {
		t.Fatalf("want malformed-token-URL error, got %v", err)
	}
}

// Zero credentials is the deliberate-unauthenticated mode (warned at build, not
// errored at load) — the payer block's posture, unchanged for the populate connector.
func TestLoadConfig_ProviderDTRPopulateURLOnlyIsOK(t *testing.T) {
	env := map[string]string{
		"ROLE": "provider", "SHN_SECRETS": "/x", "SHN_DISCOVERY_URL": "https://d",
		"PROVIDER_DTR_POPULATE_URL": "https://populate.test/fhir/Questionnaire/$populate",
	}
	cfg, err := loadEnv(env)
	if err != nil {
		t.Fatalf("URL-only should load: %v", err)
	}
	if cfg.ProviderDTRPopulateTokenURL != "" {
		t.Errorf("cfg = %+v", cfg)
	}
}

// TestCheckTargets_ProviderDTRPopulateTokenURL: the populate token endpoint joins
// /internal/checks as a credential check (KindToken with a TokenFetch closure), the
// populate URL itself stays a plain reachable probe (a 401 from the auth gate in
// front of it counts as reachable), and neither appears when unset.
func TestCheckTargets_ProviderDTRPopulateTokenURL(t *testing.T) {
	find := func(targets []checks.Target, id string) (checks.Target, bool) {
		for _, tgt := range targets {
			if tgt.ID == id {
				return tgt, true
			}
		}
		return checks.Target{}, false
	}
	cfg := config{
		ProviderDTRPopulateURL:          "https://populate.test/fhir/Questionnaire/$populate",
		ProviderDTRPopulateTokenURL:     "https://populate.test/token",
		ProviderDTRPopulateClientID:     "populate-gw",
		ProviderDTRPopulateClientSecret: "s3cret",
		ProviderDTRPopulateScope:        "system/*.read",
	}
	targets := checkTargets(cfg)
	tok, ok := find(targets, "PROVIDER_DTR_POPULATE_TOKEN_URL")
	if !ok {
		t.Fatal("PROVIDER_DTR_POPULATE_TOKEN_URL target missing")
	}
	if tok.Kind != checks.KindToken {
		t.Fatalf("token Kind = %q, want %q", tok.Kind, checks.KindToken)
	}
	if tok.TokenFetch == nil {
		t.Fatal("token target has no TokenFetch closure")
	}
	if tok.URL != cfg.ProviderDTRPopulateTokenURL {
		t.Fatalf("token URL = %q, want %q", tok.URL, cfg.ProviderDTRPopulateTokenURL)
	}
	pop, ok := find(targets, "PROVIDER_DTR_POPULATE_URL")
	if !ok {
		t.Fatal("PROVIDER_DTR_POPULATE_URL target missing")
	}
	if pop.Kind != checks.KindReachable {
		t.Fatalf("populate Kind = %q, want %q", pop.Kind, checks.KindReachable)
	}
	if _, ok := find(checkTargets(config{}), "PROVIDER_DTR_POPULATE_TOKEN_URL"); ok {
		t.Fatal("PROVIDER_DTR_POPULATE_TOKEN_URL target present with nothing configured")
	}
}

// TestProviderDTRPopulateTokenFetch_MintsFreshTokenPerInvocation is
// TestFhirTokenFetch_MintsFreshTokenPerInvocation's PROVIDER_DTR_POPULATE_*
// counterpart — same fix, same regression shape.
func TestProviderDTRPopulateTokenFetch_MintsFreshTokenPerInvocation(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":3600}`))
	}))
	defer srv.Close()

	cfg := config{
		ProviderDTRPopulateTokenURL:     srv.URL,
		ProviderDTRPopulateClientID:     "populate-gw",
		ProviderDTRPopulateClientSecret: "s3cret",
		ProviderDTRPopulateScope:        "system/*.read",
	}
	fetch := providerDTRPopulateTokenFetch(cfg)

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

// TestProviderDTRPopulateTokenFetch_Reports401AsStatus: a token endpoint that
// refuses the credential surfaces as a *checks.StatusError carrying the 401, so
// /internal/checks reports the credential failure by status (never redacted to
// a generic error) — the same classification FHIR_TOKEN_URL gets.
func TestProviderDTRPopulateTokenFetch_Reports401AsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
	}))
	defer srv.Close()

	cfg := config{
		ProviderDTRPopulateTokenURL:     srv.URL,
		ProviderDTRPopulateClientID:     "populate-gw",
		ProviderDTRPopulateClientSecret: "wrong",
		ProviderDTRPopulateScope:        "system/*.read",
	}
	err := providerDTRPopulateTokenFetch(cfg)(context.Background())
	var se *checks.StatusError
	if !errors.As(err, &se) || se.Code != http.StatusUnauthorized {
		t.Fatalf("fetch err = %v, want *checks.StatusError{Code: 401}", err)
	}
}

// A missing key file is captured at target construction and replayed as the
// closure's error on every probe (fhirTokenFetch's contract).
func TestProviderDTRPopulateTokenFetch_MissingKeyReplays(t *testing.T) {
	cfg := config{
		ProviderDTRPopulateTokenURL:  "https://populate.test/token",
		ProviderDTRPopulateClientID:  "populate-gw",
		ProviderDTRPopulateClientKey: "/nonexistent/populate.pem",
		ProviderDTRPopulateClientAlg: "ES384",
	}
	err := providerDTRPopulateTokenFetch(cfg)(context.Background())
	if err == nil || !strings.Contains(err.Error(), "populate.pem") {
		t.Fatalf("want key-load error, got %v", err)
	}
}

// buildProviderForPopulate boots a ROLE=provider gateway through build() with
// the given PROVIDER_DTR_POPULATE_* variables and returns build's stdout.
func buildProviderForPopulate(t *testing.T, populate map[string]string) (built, string, error) {
	t.Helper()
	fhirSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/metadata") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"resourceType":"CapabilityStatement","fhirVersion":"4.0.1"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(fhirSrv.Close)

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyBody := fmt.Sprintf(`{"pubkey":%q}`, base64.StdEncoding.EncodeToString(pub))
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(keyBody))
	}))
	t.Cleanup(keys.Close)
	disc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"endpoints":{},"authzPublicKeyURL":%q,"hubTransportKeyURL":%q}`, keys.URL, keys.URL)
	}))
	t.Cleanup(disc.Close)

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
	}
	for k, v := range populate {
		env[k] = v
	}
	var out bytes.Buffer
	b, err := build(context.Background(), func(k string) string { return env[k] }, &out, nil)
	return b, out.String(), err
}

const populateUnauthWarning = "gateway: WARNING PROVIDER_DTR_POPULATE_URL set without PROVIDER_DTR_POPULATE_TOKEN_URL — populating UNAUTHENTICATED"

// TestBuild_ProviderDTRPopulateUnauthenticatedWarning: a populate URL that is
// actually wired (the demo profile) with no credential block boots — the
// deliberate zero-config posture — but says so on stdout, exactly as the payer
// block does; with the block set the warning is silent.
func TestBuild_ProviderDTRPopulateUnauthenticatedWarning(t *testing.T) {
	t.Run("url only warns", func(t *testing.T) {
		_, out, err := buildProviderForPopulate(t, map[string]string{
			"PROVIDER_DTR_POPULATE_URL": "https://populate.test/fhir/Questionnaire/$populate",
		})
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		if !strings.Contains(out, populateUnauthWarning) {
			t.Fatalf("stdout = %q, want the UNAUTHENTICATED warning", out)
		}
	})
	t.Run("quad is silent", func(t *testing.T) {
		_, out, err := buildProviderForPopulate(t, map[string]string{
			"PROVIDER_DTR_POPULATE_URL":           "https://populate.test/fhir/Questionnaire/$populate",
			"PROVIDER_DTR_POPULATE_TOKEN_URL":     "https://populate.test/token",
			"PROVIDER_DTR_POPULATE_CLIENT_ID":     "populate-gw",
			"PROVIDER_DTR_POPULATE_CLIENT_SECRET": "s3cret",
		})
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		if strings.Contains(out, "populating UNAUTHENTICATED") {
			t.Fatalf("stdout = %q, want no UNAUTHENTICATED warning when the credential block is set", out)
		}
	})
	t.Run("missing key file fails the build", func(t *testing.T) {
		_, _, err := buildProviderForPopulate(t, map[string]string{
			"PROVIDER_DTR_POPULATE_URL":        "https://populate.test/fhir/Questionnaire/$populate",
			"PROVIDER_DTR_POPULATE_TOKEN_URL":  "https://populate.test/token",
			"PROVIDER_DTR_POPULATE_CLIENT_ID":  "populate-gw",
			"PROVIDER_DTR_POPULATE_CLIENT_KEY": "/nonexistent/populate.pem",
			"PROVIDER_DTR_POPULATE_CLIENT_ALG": "ES384",
		})
		if err == nil || !strings.Contains(err.Error(), "populate client key") {
			t.Fatalf("build err = %v, want the populate key-load failure", err)
		}
	})
}
