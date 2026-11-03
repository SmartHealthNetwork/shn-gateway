//go:build !shn_coreonly

package app

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// cdexTaskEnv is a provider with the Da Vinci ingress and its own Task
// endpoint, with a private_key_jwt RS256 credential block.
func cdexTaskEnv(t *testing.T) map[string]string {
	t.Helper()
	e := baseProviderEnv()
	e["PROVIDER_DAVINCI_INGRESS"] = "1"
	e["PROVIDER_DAVINCI_INGRESS_BASE_URL"] = "https://provider-gw.test"
	e["INGRESS_CLIENTS_FILE"] = writeClientsFile(t, testValidClientsJSON(t))
	for k, v := range map[string]string{
		"PROVIDER_CDEX_TASK_URL":   "https://provider.test/fhir/Task",
		"PROVIDER_CDEX_TOKEN_URL":  "https://provider.test/oauth2/token",
		"PROVIDER_CDEX_CLIENT_ID":  "provider-gw",
		"PROVIDER_CDEX_SCOPE":      "system/Task.c",
		"PROVIDER_CDEX_CLIENT_KEY": "/keys/cdex.pem",
		"PROVIDER_CDEX_CLIENT_ALG": "RS256",
	} {
		e[k] = v
	}
	return e
}

// A provider with its own Task endpoint, its whole credential block and the
// ingress loads, and declares the payer's attachment request beside what
// its role takes unconditionally; with no endpoint it declares neither.
func TestLoadConfig_ProviderCDexTaskEndpointTakesTheAttachmentRequest(t *testing.T) {
	cfg, err := loadEnv(cdexTaskEnv(t))
	if err != nil {
		t.Fatalf("a complete PROVIDER_CDEX_* block should load: %v", err)
	}
	if cfg.ProviderCDexTaskURL != "https://provider.test/fhir/Task" || cfg.ProviderCDexClientAlg != "RS256" || cfg.ProviderCDexScope != "system/Task.c" {
		t.Fatalf("loaded %+v", cfg)
	}
	if got := capabilityDeclaration(cfg).Legs; !slices.Contains(got, "cdex-attachment-request") {
		t.Fatalf("a provider with its Task endpoint declares %v, want the attachment request", got)
	}
	if got := capabilityDeclaration(config{Role: "provider"}).Legs; slices.Contains(got, "cdex-attachment-request") {
		t.Fatalf("a provider without one declares %v", got)
	}
	for _, alg := range []string{"ES384", "RS384"} {
		e := cdexTaskEnv(t)
		e["PROVIDER_CDEX_CLIENT_ALG"] = alg
		if _, err := loadEnv(e); err != nil {
			t.Errorf("PROVIDER_CDEX_CLIENT_ALG=%s: %v", alg, err)
		}
	}
	e := cdexTaskEnv(t)
	delete(e, "PROVIDER_CDEX_CLIENT_KEY")
	delete(e, "PROVIDER_CDEX_CLIENT_ALG")
	e["PROVIDER_CDEX_CLIENT_SECRET"] = "s3cret"
	if _, err := loadEnv(e); err != nil {
		t.Errorf("the client_secret_post mode: %v", err)
	}
}

// Every way the block is incomplete or misplaced refuses to boot, naming
// what is missing: no Task is forwarded unauthenticated, nor without the
// return path its payer-url is re-keyed to, nor by another role.
func TestLoadConfig_ProviderCDexTaskEndpointRefusals(t *testing.T) {
	for name, c := range map[string]struct {
		edit func(e map[string]string)
		want string
	}{
		"on a payer": {func(e map[string]string) {
			for k := range e {
				if strings.HasPrefix(k, "PROVIDER_DAVINCI_INGRESS") || k == "INGRESS_CLIENTS_FILE" {
					delete(e, k)
				}
			}
			e["ROLE"] = "payer"
		}, "is provider-only (role=\"payer\")"},
		"credentials without the endpoint": {func(e map[string]string) { delete(e, "PROVIDER_CDEX_TASK_URL") },
			"set requires PROVIDER_CDEX_TASK_URL"},
		"without the ingress": {func(e map[string]string) {
			for k := range e {
				if strings.HasPrefix(k, "PROVIDER_DAVINCI_INGRESS") || k == "INGRESS_CLIENTS_FILE" {
					delete(e, k)
				}
			}
		}, "requires PROVIDER_DAVINCI_INGRESS and PROVIDER_DAVINCI_INGRESS_BASE_URL"},
		"without the token URL": {func(e map[string]string) { delete(e, "PROVIDER_CDEX_TOKEN_URL") }, "requires PROVIDER_CDEX_TOKEN_URL"},
		"without the client id": {func(e map[string]string) { delete(e, "PROVIDER_CDEX_CLIENT_ID") }, "requires PROVIDER_CDEX_CLIENT_ID"},
		"without a scope":       {func(e map[string]string) { delete(e, "PROVIDER_CDEX_SCOPE") }, "requires PROVIDER_CDEX_SCOPE"},
		// A blank scope mints a token with none, which the provider's
		// system then refuses: it is no scope.
		"a blank scope": {func(e map[string]string) { e["PROVIDER_CDEX_SCOPE"] = "  " }, "requires PROVIDER_CDEX_SCOPE"},
		"without a credential": {func(e map[string]string) {
			delete(e, "PROVIDER_CDEX_CLIENT_KEY")
			delete(e, "PROVIDER_CDEX_CLIENT_ALG")
		}, "PROVIDER_CDEX_TOKEN_URL requires credentials"},
		"both modes":        {func(e map[string]string) { e["PROVIDER_CDEX_CLIENT_SECRET"] = "s" }, "not both"},
		"an alg not here":   {func(e map[string]string) { e["PROVIDER_CDEX_CLIENT_ALG"] = "HS256" }, "PROVIDER_CDEX_CLIENT_ALG must be ES384|RS384|RS256"},
		"a malformed URL":   {func(e map[string]string) { e["PROVIDER_CDEX_TASK_URL"] = "notaurl" }, "PROVIDER_CDEX_TASK_URL"},
		"a malformed token": {func(e map[string]string) { e["PROVIDER_CDEX_TOKEN_URL"] = "notaurl" }, "PROVIDER_CDEX_TOKEN_URL"},
	} {
		t.Run(name, func(t *testing.T) {
			e := cdexTaskEnv(t)
			c.edit(e)
			if _, err := loadEnv(e); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("loadConfig: %v, want an error containing %q", err, c.want)
			}
		})
	}
}

// RS256 is admitted for the provider's Task forward alone: every other
// credential block keeps SMART Backend Services' two algs.
func TestLoadConfig_RS256IsTheTaskForwardsAlone(t *testing.T) {
	env := populateQuadEnv()
	env["PROVIDER_DTR_POPULATE_CLIENT_ALG"] = "RS256"
	if _, err := loadEnv(env); err == nil || !strings.Contains(err.Error(), "PROVIDER_DTR_POPULATE_CLIENT_ALG must be ES384|RS384,") {
		t.Fatalf("RS256 on the populate block: %v, want the ES384|RS384 refusal", err)
	}
}

// The deployed shape the provider's Task forward runs under: its token
// client's own timeout (10s, as for every SMART block) is shorter than the
// forward's deadline (the requester's leg budget less five seconds, 25s), so
// a token endpoint that never answers is cut at the token client and read as
// the provider's system not reached (502), inside the requester's budget.
func TestProviderCDexTokenClientTimeoutIsShorterThanTheForwardsDeadline(t *testing.T) {
	cfg, err := loadEnv(func() map[string]string {
		e := cdexTaskEnv(t)
		delete(e, "PROVIDER_CDEX_CLIENT_KEY")
		delete(e, "PROVIDER_CDEX_CLIENT_ALG")
		e["PROVIDER_CDEX_CLIENT_SECRET"] = "s"
		return e
	}())
	if err != nil {
		t.Fatal(err)
	}
	cfg.certs = endpointcert.New()
	sc, err := providerCDexSmartConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	deadline := shnsdk.NewClient().Timeout - 5*time.Second
	if sc.HTTPClient == nil || sc.HTTPClient.Timeout != 10*time.Second || sc.HTTPClient.Timeout >= deadline {
		t.Fatalf("the token client's timeout is %v, want 10s, shorter than the forward's %s deadline", sc.HTTPClient, deadline)
	}
}
