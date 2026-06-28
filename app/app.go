// Package app is the federation-capable PUBLIC gateway runtime. It boots
// config-only, loads its `shn register` bundle (shnsdk.LoadBundle), resolves
// trust anchors + endpoints + the FHIR validator URL from /discovery, populates
// the peer Registry from the registrar /holders feed (the federation core),
// defaults SoR/Store to the in-memory memstub, and defaults the validator to the
// REAL operation-level validator FAIL-CLOSED. It reuses shn-sdk for all
// participation and NEVER imports the private substrate's internal packages — the
// gateway boundary fence (gateway/boundary_test.go) enforces this structurally (AI-11).
package app

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	fhirsor "github.com/SmartHealthNetwork/shn-gateway/connectors/fhirsor"
	pgstore "github.com/SmartHealthNetwork/shn-gateway/connectors/pgstore"
	smartauth "github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// config is the collapsed PUBLIC config surface. Required:
// SHN_DISCOVERY_URL (the single anchor), ROLE, SHN_SECRETS (the bundle dir).
// Everything else is discovery-resolved or an optional override. The seed /
// SHN_MANIFEST path of the substrate cmd/gateway is intentionally dropped — the
// public binary is partner-path only.
type config struct {
	Role         string
	Addr         string
	SecretsDir   string
	DiscoveryURL string

	// Endpoint overrides (normally discovery-resolved; explicit env wins).
	AuthzURL        string
	HubURL          string
	CounterpartID   string
	ConsentURL      string
	AuditURL        string
	PHGURL          string
	RegistrarURL    string
	FHIRValidateURL string
	NPI             string

	// Trust-anchor key-fetch URL overrides (first-class operator config):
	// override the discovery-advertised key URL when the gateway runs in the same
	// network as the substrate. firstNonEmpty(env, discovery); discovery is the default.
	AuthzPubkeyURL     string
	HubTransportKeyURL string

	StoreDatabaseURL string // Postgres DSN for the durable pgstore Store (else memstub).

	// Optional FHIR connector block (else memstub SoR/Store).
	FHIRDataURL     string
	FHIRTokenURL    string
	FHIRClientID    string
	FHIRClientKey   string
	FHIRClientAlg   string
	FHIRClientScope string
	FHIRClientKID   string

	// Optional native-forward payer block (the PARTNER Da Vinci payer is a different
	// external party from the FHIR SoR — distinct credentials). Setting
	// PayerDavinciBaseURL switches the payer's read-only legs to native forwarding.
	PayerDavinciBaseURL string
	// PayerDavinciCDSBaseURL is the base for the partner's CDS Hooks (CRD) posts when it
	// is NOT co-located with the FHIR base — e.g. br-payer serves CDS Hooks at root
	// /cds-services but FHIR ops under /fhir. Empty ⇒ CDS uses PayerDavinciBaseURL
	// (co-located default). FR-G28 / OWD-G8.
	PayerDavinciCDSBaseURL string
	PayerDavinciTokenURL   string
	PayerDavinciClientID   string
	PayerDavinciClientKey  string
	PayerDavinciClientAlg  string
	PayerDavinciScope      string
	PayerDavinciClientKID  string
	PayerDavinciPASNative  bool
	// PayerDavinciCRDServiceID is the escape-hatch override for the partner's CDS
	// Hooks order-select service id. When empty, DiscoverCRDServiceID fetches
	// {PAYER_DAVINCI_BASE_URL}/cds-services at boot and selects the single
	// "order-select" service (FR-G26). Set explicitly when the partner's CRD service
	// uses a different hook name — e.g. br-payer's "order-sign-crd" (hook:order-sign).
	PayerDavinciCRDServiceID string
	// PayerDavinciCRDHook is the CDS Hooks hook value to stamp on the CRD request before
	// forwarding, matching the partner's CRD service (br-payer's order-sign-crd ⇒ order-sign).
	// Empty ⇒ forward the originator's hook verbatim. PAYER_DAVINCI_CRD_HOOK.
	PayerDavinciCRDHook string
	// PayerDavinciDispatchServiceID is the partner's CDS service id for the
	// crd-order-dispatch leg. Empty ⇒ dispatch leg fails closed (502). PAYER_DAVINCI_DISPATCH_SERVICE_ID.
	PayerDavinciDispatchServiceID string
	// PayerDavinciDispatchHook is the CDS Hooks hook value to stamp on the order-dispatch
	// request before forwarding. Empty ⇒ forward the originator's hook verbatim. PAYER_DAVINCI_DISPATCH_HOOK.
	PayerDavinciDispatchHook string

	// OriginationProfile selects the per-UC origination lane: "" / "sandbox"
	// keep the CPT/lumbar order shape; "composite" originates the HCPCS codes the real
	// br-payer adjudicates. ORIGINATION_PROFILE.
	OriginationProfile string

	// Optional native DTR population (provider-local). PROVIDER_DTR_NATIVE switches DTR
	// population from the in-house managed backend to forwarding the provider's own SDC
	// Questionnaire/$populate endpoint. Unauthenticated this slice.
	ProviderDTRNative      bool
	ProviderDTRPopulateURL string

	// ProviderDavinciIngress mounts the Da Vinci ingress routes (CRD /cds-services,
	// DTR $questionnaire-package, PAS $submit) on the provider role. Set by
	// PROVIDER_DAVINCI_INGRESS (any non-empty value enables). When enabled,
	// PROVIDER_DAVINCI_INGRESS_BASE_URL and INGRESS_CLIENTS_FILE are required —
	// a mounted-but-universally-401 ingress is a footgun (FR-G13 all-or-nothing).
	ProviderDavinciIngress bool

	// ProviderDavinciIngressBaseURL is the CONFIG-PINNED SMART Backend Services aud
	// (token endpoint aud + bearer aud). Never request-derived. Set by
	// PROVIDER_DAVINCI_INGRESS_BASE_URL. Required when ProviderDavinciIngress is set.
	ProviderDavinciIngressBaseURL string
	// ProviderDavinciIngressClientsFile is the path to the JSON registration file
	// ([{client_id, alg, public_key_pem, scopes}]). Set by INGRESS_CLIENTS_FILE.
	// Required when ProviderDavinciIngress is set.
	ProviderDavinciIngressClientsFile string

	// IngressBaseURL and IngressClients are resolved from ProviderDavinciIngressBaseURL
	// + IngressClientsFile by loadConfig and passed directly into engine.Config.
	IngressBaseURL string
	IngressClients map[string]engine.IngressClientRegistration
}

var validRoles = map[string]bool{
	"provider": true,
	"payer":    true,
	"facility": true,
	"phg":      true,
}

// loadConfig reads the collapsed PUBLIC surface from getenv. Mirrors the
// substrate cmd/gateway loadConfig validation (collapsed-surface URL checks,
// ROLE/PORT bounds, the FHIR/SMART quad guards) MINUS the seed/SHN_MANIFEST
// path — the public binary requires SHN_DISCOVERY_URL.
func loadConfig(getenv func(string) string) (config, error) {
	def := func(k, d string) string {
		if v := getenv(k); v != "" {
			return v
		}
		return d
	}

	role := def("ROLE", "provider")
	if !validRoles[role] {
		return config{}, fmt.Errorf("gateway: invalid ROLE %q (must be provider|payer|facility|phg)", role)
	}

	port := def("PORT", "8080")
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return config{}, fmt.Errorf("gateway: invalid PORT %q", port)
	}
	host := def("HOST", "0.0.0.0")

	secretsDir := getenv("SHN_SECRETS")
	if secretsDir == "" {
		return config{}, fmt.Errorf("gateway: SHN_SECRETS (the shn register / Init bundle dir) is required")
	}
	discoveryURL := getenv("SHN_DISCOVERY_URL")
	if discoveryURL == "" {
		return config{}, fmt.Errorf("gateway: SHN_DISCOVERY_URL is required (the single anchor that resolves substrate endpoints + trust anchors)")
	}

	cfg := config{
		Role:             role,
		Addr:             host + ":" + port,
		SecretsDir:       secretsDir,
		DiscoveryURL:     discoveryURL,
		AuthzURL:         getenv("AUTHZ_URL"),
		HubURL:           getenv("HUB_URL"),
		CounterpartID:    getenv("COUNTERPART_ID"),
		ConsentURL:       getenv("CONSENT_URL"),
		AuditURL:         getenv("AUDIT_URL"),
		PHGURL:           getenv("PHG_URL"),
		RegistrarURL:     getenv("REGISTRAR_URL"),
		FHIRValidateURL:  getenv("FHIR_VALIDATE_URL"),
		StoreDatabaseURL: getenv("SHN_STORE_DATABASE_URL"),
		NPI:              def("NPI", "1234567890"),
		FHIRDataURL:      getenv("FHIR_DATA_URL"),
		FHIRTokenURL:     getenv("FHIR_TOKEN_URL"),
		FHIRClientID:     getenv("FHIR_CLIENT_ID"),
		FHIRClientKey:    getenv("FHIR_CLIENT_KEY"),
		FHIRClientAlg:    getenv("FHIR_CLIENT_ALG"),
		FHIRClientScope:  def("FHIR_CLIENT_SCOPE", "system/*.read"),
		FHIRClientKID:    getenv("FHIR_CLIENT_KID"),

		PayerDavinciBaseURL:           getenv("PAYER_DAVINCI_BASE_URL"),
		PayerDavinciCDSBaseURL:        getenv("PAYER_DAVINCI_CDS_BASE_URL"),
		PayerDavinciTokenURL:          getenv("PAYER_DAVINCI_TOKEN_URL"),
		PayerDavinciClientID:          getenv("PAYER_DAVINCI_CLIENT_ID"),
		PayerDavinciClientKey:         getenv("PAYER_DAVINCI_CLIENT_KEY"),
		PayerDavinciClientAlg:         getenv("PAYER_DAVINCI_CLIENT_ALG"),
		PayerDavinciScope:             def("PAYER_DAVINCI_SCOPE", "system/*.read"),
		PayerDavinciClientKID:         getenv("PAYER_DAVINCI_CLIENT_KID"),
		PayerDavinciPASNative:         getenv("PAYER_DAVINCI_PAS_NATIVE") == "true",
		PayerDavinciCRDServiceID:      getenv("PAYER_DAVINCI_CRD_SERVICE_ID"),
		PayerDavinciCRDHook:           getenv("PAYER_DAVINCI_CRD_HOOK"),
		PayerDavinciDispatchServiceID: getenv("PAYER_DAVINCI_DISPATCH_SERVICE_ID"),
		PayerDavinciDispatchHook:      getenv("PAYER_DAVINCI_DISPATCH_HOOK"),
		OriginationProfile:            getenv("ORIGINATION_PROFILE"),

		ProviderDTRNative:      getenv("PROVIDER_DTR_NATIVE") == "true",
		ProviderDTRPopulateURL: getenv("PROVIDER_DTR_POPULATE_URL"),
		ProviderDavinciIngress: getenv("PROVIDER_DAVINCI_INGRESS") != "",

		ProviderDavinciIngressBaseURL:     getenv("PROVIDER_DAVINCI_INGRESS_BASE_URL"),
		ProviderDavinciIngressClientsFile: getenv("INGRESS_CLIENTS_FILE"),

		AuthzPubkeyURL:     getenv("AUTHZ_PUBKEY_URL"),
		HubTransportKeyURL: getenv("HUB_TRANSPORT_KEY_URL"),
	}

	for _, pair := range [][2]string{
		{"AUTHZ_URL", cfg.AuthzURL},
		{"HUB_URL", cfg.HubURL},
		{"CONSENT_URL", cfg.ConsentURL},
		{"AUDIT_URL", cfg.AuditURL},
		{"PHG_URL", cfg.PHGURL},
		{"FHIR_VALIDATE_URL", cfg.FHIRValidateURL},
		{"FHIR_DATA_URL", cfg.FHIRDataURL},
		{"REGISTRAR_URL", cfg.RegistrarURL},
		{"FHIR_TOKEN_URL", cfg.FHIRTokenURL},
		{"PAYER_DAVINCI_BASE_URL", cfg.PayerDavinciBaseURL},
		{"PAYER_DAVINCI_TOKEN_URL", cfg.PayerDavinciTokenURL},
		{"PROVIDER_DTR_POPULATE_URL", cfg.ProviderDTRPopulateURL},
		{"PROVIDER_DAVINCI_INGRESS_BASE_URL", cfg.ProviderDavinciIngressBaseURL},
		{"SHN_DISCOVERY_URL", cfg.DiscoveryURL},
		{"AUTHZ_PUBKEY_URL", cfg.AuthzPubkeyURL},
		{"HUB_TRANSPORT_KEY_URL", cfg.HubTransportKeyURL},
	} {
		if err := checkOptionalURL(pair[0], pair[1]); err != nil {
			return config{}, fmt.Errorf("gateway: %w", err)
		}
	}

	if cfg.FHIRTokenURL != "" {
		if cfg.FHIRDataURL == "" {
			return config{}, fmt.Errorf("gateway: FHIR_TOKEN_URL set requires FHIR_DATA_URL (auth needs a FHIR server to authenticate to)")
		}
		if cfg.FHIRClientID == "" || cfg.FHIRClientKey == "" {
			return config{}, fmt.Errorf("gateway: FHIR_TOKEN_URL requires FHIR_CLIENT_ID and FHIR_CLIENT_KEY")
		}
		if cfg.FHIRClientAlg != "ES384" && cfg.FHIRClientAlg != "RS384" {
			return config{}, fmt.Errorf("gateway: FHIR_CLIENT_ALG must be ES384|RS384, got %q", cfg.FHIRClientAlg)
		}
	}

	// All-or-nothing partner-payer credentials: a partial block is a
	// misconfig (someone intended auth and fat-fingered it) → hard error. Zero creds is
	// the deliberate-unauthenticated mode (warned at build, not errored here).
	if cfg.PayerDavinciTokenURL != "" {
		if cfg.PayerDavinciBaseURL == "" {
			return config{}, fmt.Errorf("gateway: PAYER_DAVINCI_TOKEN_URL set requires PAYER_DAVINCI_BASE_URL")
		}
		if cfg.PayerDavinciClientID == "" || cfg.PayerDavinciClientKey == "" {
			return config{}, fmt.Errorf("gateway: PAYER_DAVINCI_TOKEN_URL requires PAYER_DAVINCI_CLIENT_ID and PAYER_DAVINCI_CLIENT_KEY")
		}
		if cfg.PayerDavinciClientAlg != "ES384" && cfg.PayerDavinciClientAlg != "RS384" {
			return config{}, fmt.Errorf("gateway: PAYER_DAVINCI_CLIENT_ALG must be ES384|RS384, got %q", cfg.PayerDavinciClientAlg)
		}
	}

	if cfg.ProviderDTRNative && cfg.ProviderDTRPopulateURL == "" {
		return config{}, fmt.Errorf("gateway: PROVIDER_DTR_NATIVE=true requires PROVIDER_DTR_POPULATE_URL")
	}
	if cfg.OriginationProfile == "provider-data" && cfg.ProviderDTRPopulateURL == "" {
		return config{}, fmt.Errorf("gateway: ORIGINATION_PROFILE=provider-data requires PROVIDER_DTR_POPULATE_URL (the operated $populate endpoint)")
	}

	// All-or-nothing ingress registration (FR-G13): PROVIDER_DAVINCI_INGRESS
	// requires a config-pinned base URL AND >=1 valid registered client. A provider that
	// enables the ingress without registered clients gets a universally-401 ingress —
	// that is a footgun, not a safe default, so we refuse to boot.
	if cfg.ProviderDavinciIngress {
		if role != "provider" {
			return config{}, fmt.Errorf("gateway: PROVIDER_DAVINCI_INGRESS is provider-only (role=%q)", role)
		}
		if cfg.ProviderDavinciIngressBaseURL == "" {
			return config{}, fmt.Errorf("gateway: PROVIDER_DAVINCI_INGRESS requires PROVIDER_DAVINCI_INGRESS_BASE_URL (the config-pinned SMART aud)")
		}
		clients, err := loadIngressClients(cfg.ProviderDavinciIngressClientsFile)
		if err != nil {
			return config{}, fmt.Errorf("gateway: ingress clients: %w", err)
		}
		if len(clients) == 0 {
			return config{}, fmt.Errorf("gateway: PROVIDER_DAVINCI_INGRESS requires INGRESS_CLIENTS_FILE with >=1 registered client (a mounted-but-universally-401 ingress is a footgun)")
		}
		cfg.IngressBaseURL = cfg.ProviderDavinciIngressBaseURL
		cfg.IngressClients = clients
	}

	return cfg, nil
}

// loadIngressClients parses the inbound-client registration file: a JSON array of
// {client_id, alg, public_key_pem, scopes}. alg must be ES384|RS384 and the PEM must
// parse — a malformed entry is a hard boot error (the FR-G13 all-or-nothing posture).
// Returns nil (not error) when path is empty — the caller then enforces the
// must-have->=1-client invariant.
func loadIngressClients(path string) (map[string]engine.IngressClientRegistration, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var arr []struct {
		ClientID     string   `json:"client_id"`
		Alg          string   `json:"alg"`
		PublicKeyPEM string   `json:"public_key_pem"`
		Scopes       []string `json:"scopes"`
	}
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	out := map[string]engine.IngressClientRegistration{}
	for i, c := range arr {
		if c.ClientID == "" {
			return nil, fmt.Errorf("entry %d: empty client_id", i)
		}
		if c.Alg != "ES384" && c.Alg != "RS384" {
			return nil, fmt.Errorf("client %q: alg must be ES384|RS384, got %q", c.ClientID, c.Alg)
		}
		pemBytes := []byte(c.PublicKeyPEM)
		// Parse the PEM here for fail-fast boot-time error attribution. Note:
		// engine.newIngressAuthServer re-parses the same bytes into its pubKeys map —
		// this parse is intentional and must not be removed as an "optimization".
		switch c.Alg {
		case "ES384":
			if _, err := jwt.ParseECPublicKeyFromPEM(pemBytes); err != nil {
				return nil, fmt.Errorf("client %q: bad ES384 public key: %w", c.ClientID, err)
			}
		case "RS384":
			if _, err := jwt.ParseRSAPublicKeyFromPEM(pemBytes); err != nil {
				return nil, fmt.Errorf("client %q: bad RS384 public key: %w", c.ClientID, err)
			}
		}
		scopes := c.Scopes
		if len(scopes) == 0 {
			scopes = []string{"system/Davinci.write"}
		}
		out[c.ClientID] = engine.IngressClientRegistration{Alg: c.Alg, PublicKeyPEM: pemBytes, Scopes: scopes}
	}
	return out, nil
}

func checkOptionalURL(name, v string) error {
	if v == "" {
		return nil
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("invalid %s %q", name, v)
	}
	return nil
}

// trustAnchors carries the two ed25519 trust anchors resolved from /discovery.
type trustAnchors struct {
	AuthzPub        ed25519.PublicKey
	HubTransportPub ed25519.PublicKey
}

// resolvedEndpoints carries the substrate endpoint URLs resolved from /discovery
// (explicit env still wins; applied via firstNonEmpty in build).
type resolvedEndpoints struct {
	Authz        string
	Hub          string
	Registrar    string
	FHIRValidate string
	// Consent/Audit/PHG are discovery-advertised; explicit env still
	// wins via firstNonEmpty in build (same pattern as Authz/Hub).
	Consent string
	Audit   string
	PHG     string
}

// resolveDiscovery fetches the /discovery descriptor and resolves the trust
// anchors (AuthzPub via disc.AuthzPublicKeyURL /pubkey; HubTransportPub via
// disc.HubTransportKeyURL) + endpoint URLs (incl. Registrar + FHIRValidate).
// Mirrors cmd/gateway/main.go resolveIdentity's discovery branch using public
// shnsdk symbols only. FAIL-CLOSED: an unreachable discovery / key fetch errors.
func resolveDiscovery(ctx context.Context, c *http.Client, cfg config) (trustAnchors, resolvedEndpoints, error) {
	var ta trustAnchors
	var ep resolvedEndpoints

	var disc shnsdk.Discovery
	if err := getJSON(ctx, c, cfg.DiscoveryURL, &disc); err != nil {
		return ta, ep, fmt.Errorf("fetch discovery: %w", err)
	}

	authzPub, err := fetchEd25519Key(ctx, c, firstNonEmpty(cfg.AuthzPubkeyURL, disc.AuthzPublicKeyURL))
	if err != nil {
		return ta, ep, fmt.Errorf("fetch authz pubkey: %w", err)
	}
	hubTxPub, err := shnsdk.FetchHubTransportKey(ctx, c, firstNonEmpty(cfg.HubTransportKeyURL, disc.HubTransportKeyURL))
	if err != nil {
		return ta, ep, fmt.Errorf("fetch hub transport key: %w", err)
	}
	ta = trustAnchors{AuthzPub: authzPub, HubTransportPub: hubTxPub}

	ep = resolvedEndpoints{
		Authz:        disc.Endpoints.Authz,
		Hub:          disc.Endpoints.Hub,
		Registrar:    disc.Endpoints.Registrar,
		FHIRValidate: disc.FHIRValidateURL,
		Consent:      disc.Endpoints.Consent,
		Audit:        disc.Endpoints.Audit,
		PHG:          disc.Endpoints.PHG,
	}
	return ta, ep, nil
}

// getJSON fetches rawURL and JSON-decodes the response body into v.
func getJSON(ctx context.Context, c *http.Client, rawURL string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("GET %s: status %d", rawURL, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// fetchEd25519Key fetches a published ed25519 public key from rawURL. Wire
// format (pinned against the substrate authz GET /pubkey handler): JSON
// {"pubkey":"<base64.StdEncoding of 32-byte key>"}.
func fetchEd25519Key(ctx context.Context, c *http.Client, rawURL string) (ed25519.PublicKey, error) {
	var envelope struct {
		Pubkey string `json:"pubkey"`
	}
	if err := getJSON(ctx, c, rawURL, &envelope); err != nil {
		return nil, fmt.Errorf("fetchEd25519Key %s: %w", rawURL, err)
	}
	if envelope.Pubkey == "" {
		return nil, fmt.Errorf("fetchEd25519Key %s: response missing \"pubkey\" field", rawURL)
	}
	dec, err := base64.StdEncoding.DecodeString(envelope.Pubkey)
	if err != nil {
		return nil, fmt.Errorf("fetchEd25519Key %s: decode base64: %w", rawURL, err)
	}
	if len(dec) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("fetchEd25519Key %s: want %d bytes, got %d", rawURL, ed25519.PublicKeySize, len(dec))
	}
	return ed25519.PublicKey(dec), nil
}

// firstNonEmpty returns the first non-empty string (explicit env override wins
// over the discovery-resolved fallback).
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// built is everything Run needs to serve + keep the peer registry converged. The
// serve-seam: build() does ALL config/identity/discovery/validator/wiring + the
// boot-time registry SNAPSHOT, and returns the handler WITHOUT serving, so the
// boot gate can drive it.
type built struct {
	addr         string
	handler      http.Handler
	reg          shnsdk.Registry // shared-reference value type; the poller mutates the same state the engine reads
	registrarURL string
	client       *http.Client
}

func build(ctx context.Context, getenv func(string) string, stdout io.Writer, clock func() time.Time) (built, error) {
	var b built
	if clock == nil {
		clock = time.Now
	}
	cfg, err := loadConfig(getenv)
	if err != nil {
		return b, err
	}

	// Identity bundle (shn register / Init output) — recovers HolderID from manifest.json.
	bundle, err := shnsdk.LoadBundle(cfg.SecretsDir)
	if err != nil {
		return b, fmt.Errorf("load bundle: %w", err)
	}

	client := shnsdk.NewClient()

	// Discovery resolution: trust anchors + endpoints (incl. registrar + validator); explicit env wins.
	trust, endpoints, err := resolveDiscovery(ctx, client, cfg)
	if err != nil {
		return b, err
	}
	registrarURL := firstNonEmpty(cfg.RegistrarURL, endpoints.Registrar)

	// Validator: REAL operation-level, FAIL-CLOSED. Fake only on explicit opt-in. (pure helper, unit-tested)
	validator, err := selectValidator(getenv, firstNonEmpty(cfg.FHIRValidateURL, endpoints.FHIRValidate))
	if err != nil {
		return b, err
	}

	// FEDERATION CORE: populate the peer registry from the live /holders feed (boot-time
	// SNAPSHOT). The engine resolves recipient EncPub via cfg.Reg.Lookup, so Reg MUST be
	// populated before serving. This is the public, SDK-based replacement for the
	// substrate's internal registrar.RunPoller (un-importable here, and rejected by the
	// gateway boundary fence). Run() additionally starts a background poller (below) for
	// post-boot convergence; the boot gate drives build() and asserts this snapshot
	// resolves the counterpart — deterministic, no ticker.
	reg := shnsdk.NewRegistry()
	if registrarURL != "" {
		if err := convergeRegistry(ctx, client, registrarURL, reg); err != nil {
			return b, fmt.Errorf("converge peer registry from %s: %w", registrarURL, err)
		}
	}

	// SoR/Store: memstub data-default; FHIR connector is the opt-in override.
	var sor engine.SystemOfRecord
	var store engine.Store
	if cfg.FHIRDataURL == "" {
		stub := engine.NewStubHolderData()
		sor, store = stub, stub
	} else {
		hc, herr := fhirHTTPClient(cfg) // smartauth.NewHTTPClient when the SMART quad is set, else nil (unauthenticated)
		if herr != nil {
			return b, herr
		}
		sor = fhirsor.NewFromURL(cfg.FHIRDataURL, hc)
		store = engine.NewStubHolderData() // default Store; the SHN_STORE_DATABASE_URL override below swaps in pgstore
	}

	// Durable Store override: pgstore when SHN_STORE_DATABASE_URL is set, else the
	// memstub selected above. pgxpool.New is lazy; NewPgStore's advisory-locked
	// EnsureSchema is the fail-fast (and the 4-gateways-one-DB race guard). Holder-
	// scoped by construction: NewPgStore captures the bundle's HolderID.
	if cfg.StoreDatabaseURL != "" {
		pool, perr := pgxpool.New(ctx, cfg.StoreDatabaseURL)
		if perr != nil {
			return b, fmt.Errorf("gateway: pgxpool.New(store): %w", perr)
		}
		pg, serr := pgstore.NewPgStore(ctx, pool, bundle.Identity.HolderID)
		if serr != nil {
			return b, fmt.Errorf("gateway: pgstore.NewPgStore: %w", serr)
		}
		store = pg
	}

	gwCfg := engine.Config{
		Role:            cfg.Role,
		HolderID:        bundle.Identity.HolderID,
		CounterpartID:   cfg.CounterpartID,
		Identity:        bundle.Identity,
		AuthzURL:        firstNonEmpty(cfg.AuthzURL, endpoints.Authz),
		AuthzPub:        trust.AuthzPub,
		HubTransportPub: trust.HubTransportPub,
		HubURL:          firstNonEmpty(cfg.HubURL, endpoints.Hub),
		Reg:             reg, // populated by the snapshot above
		Validator:       validator,
		SoR:             sor,
		Store:           store,
		Adjudicator:     engine.NewSandboxAdjudicator(sor, clock),
		Clock:           clock, // production: time.Now; hermetic tests: the harness's injected clock (HandlerWithClock)
		Client:          client,
		NPI:             cfg.NPI,
		ConsentURL:      firstNonEmpty(cfg.ConsentURL, endpoints.Consent),
		AuditURL:        firstNonEmpty(cfg.AuditURL, endpoints.Audit),
		PHGURL:          firstNonEmpty(cfg.PHGURL, endpoints.PHG),

		OriginationProfile: cfg.OriginationProfile,
	}
	// Native-forward payer mode: the read-only legs forward to a partner
	// Da Vinci endpoint; PAS stays on the sandbox fallback. Setting Responder here means
	// engine.New uses it directly (it only derives from Adjudicator when Responder==nil).
	if cfg.PayerDavinciBaseURL != "" {
		if cfg.PayerDavinciTokenURL == "" {
			fmt.Fprintf(stdout, "gateway: WARNING PAYER_DAVINCI_BASE_URL set without PAYER_DAVINCI_TOKEN_URL — forwarding to the payer UNAUTHENTICATED\n")
		}
		pdc, perr := payerDavinciHTTPClient(cfg)
		if perr != nil {
			return b, perr
		}
		if pdc == nil {
			pdc = client // the substrate HTTP client; unauthenticated forward
		}
		// Fail loud: a PAS-native gateway MUST have a real payer Store for the shadow
		// ledger + EOB (mirrors the payer-role derive-then-guard at
		// gateway/engine/gateway.go:163-171). Without it a PAS leg would dispatch into
		// a nil store and panic at runtime.
		if cfg.PayerDavinciPASNative && store == nil {
			return b, fmt.Errorf("gateway: PAYER_DAVINCI_PAS_NATIVE=true requires a payer Store")
		}
		// FR-G26: discover the partner's CDS Hooks order-select service id at boot.
		// If PAYER_DAVINCI_CRD_SERVICE_ID is set it wins (escape hatch — needed for
		// partners whose CRD service uses a different hook name, e.g. br-payer's
		// "order-sign-crd" which registers hook:order-sign rather than order-select).
		// Fail-closed: an ambiguous or absent order-select service aborts boot.
		// CDS Hooks may live on a different base than the FHIR ops (e.g. br-payer: CDS at
		// root, FHIR under /fhir). cdsBase defaults to the FHIR base when unset (FR-G28).
		cdsBase := cfg.PayerDavinciBaseURL
		if cfg.PayerDavinciCDSBaseURL != "" {
			cdsBase = cfg.PayerDavinciCDSBaseURL
		}
		crdSvcID, discErr := engine.DiscoverCRDServiceID(ctx, pdc, cdsBase, cfg.PayerDavinciCRDServiceID)
		if discErr != nil {
			return b, fmt.Errorf("gateway: CRD service-id discovery: %w", discErr)
		}
		native := engine.NewNativeResponder(pdc, cfg.PayerDavinciBaseURL, crdSvcID, store, clock,
			engine.WithCDSBaseURL(cfg.PayerDavinciCDSBaseURL),
			engine.WithCRDHook(cfg.PayerDavinciCRDHook),
			engine.WithCRDDispatchService(cfg.PayerDavinciDispatchServiceID, cfg.PayerDavinciDispatchHook))
		fallback := engine.NewSandboxResponder(gwCfg.Adjudicator, sor, store, clock)
		gwCfg.Responder = engine.NewCompositeResponder(native, fallback, cfg.PayerDavinciPASNative)
		// The native-forward DTR response is a foreign Da Vinci package SHN can't $validate
		// (R-8 near-relay): tell the engine to skip the DTR egress foreign-$validate (FR-G28).
		gwCfg.PayerDavinciNative = true
	}
	if cfg.ProviderDTRNative {
		gwCfg.Populator = engine.NewNativePopulator(client, cfg.ProviderDTRPopulateURL)
	} else if cfg.OriginationProfile == "composite" {
		// Mode A composite: the foreign Da Vinci questionnaires are manual-entry; fill their
		// required items from the recorded persona DTR answer book (honesty invariant). The
		// author is the provider/clinician that recorded the answers (dtrx-1 needs an author).
		npi := cfg.NPI
		if npi == "" {
			npi = "1234567890" // matches the app-config default (def("NPI", "1234567890"))
		}
		gwCfg.Populator = engine.NewSeededPopulator("Practitioner/" + npi)
	} else if cfg.OriginationProfile == "provider-data" {
		// Operated-CQL $populate against the provider tenant (the crux of the
		// provider-data lane). PROVIDER_DTR_POPULATE_URL is validated at loadConfig.
		gwCfg.Populator = engine.NewNativePopulator(client, cfg.ProviderDTRPopulateURL)
	}
	gwCfg.IngressEnabled = cfg.ProviderDavinciIngress
	gwCfg.IngressBaseURL = cfg.IngressBaseURL
	gwCfg.IngressClients = cfg.IngressClients
	fmt.Fprintf(stdout, "gateway: role=%s holder=%s listening on %s\n", cfg.Role, bundle.Identity.HolderID, cfg.Addr)
	b = built{addr: cfg.Addr, handler: engine.New(gwCfg).Handler(), reg: reg, registrarURL: registrarURL, client: client}
	return b, nil
}

// Run wires the runtime (build), starts the background feed poller for post-boot
// peer convergence (production honesty; the boot gate uses build's snapshot, not
// this), then serves.
func Run(ctx context.Context, getenv func(string) string, stdout io.Writer) error {
	b, err := build(ctx, getenv, stdout, time.Now)
	if err != nil {
		return err
	}
	if b.registrarURL != "" {
		go pollFeed(ctx, b.client, b.registrarURL, b.reg, 3*time.Second)
	}
	return http.ListenAndServe(b.addr, b.handler)
}

// HandlerWithClock is Handler with an injected clock. The engine's per-op/per-hop
// authority (assertion issuance + expiry, VerifyBound) is time-sensitive, so a
// HERMETIC test driving the gateway against a fixed-clock substrate must align the
// gateway to that same clock. This surfaces gateway/engine's already-public
// Config.Clock at the app layer for tests/embedders; production uses Run (time.Now).
func HandlerWithClock(ctx context.Context, getenv func(string) string, stdout io.Writer, clock func() time.Time) (http.Handler, error) {
	b, err := build(ctx, getenv, stdout, clock)
	if err != nil {
		return nil, err
	}
	return b.handler, nil
}

// Handler is the EXPORTED test seam: it runs the full build (config/identity/
// discovery/validator/registry-snapshot/wiring) and returns the configured
// gateway http.Handler WITHOUT serving — so a cross-module test (the substrate
// boot gate) can drive the public runtime hermetically via httptest. main() uses
// Run (which serves); the gate uses Handler/HandlerWithClock.
func Handler(ctx context.Context, getenv func(string) string, stdout io.Writer) (http.Handler, error) {
	b, err := build(ctx, getenv, stdout, time.Now)
	if err != nil {
		return nil, err
	}
	return b.handler, nil
}

// selectValidator is the FAIL-CLOSED validator decision (pure, unit-tested
// directly): explicit fake opt-in → fake; else a resolved URL → real $validate;
// else ERROR (never a silent fake fallback — FR-36).
func selectValidator(getenv func(string) string, validatorURL string) (shnsdk.Validator, error) {
	switch {
	case getenv("SHN_FAKE_VALIDATOR") == "1":
		return shnsdk.NewFakeValidator(), nil
	case validatorURL != "":
		return shnsdk.NewOperationValidator(validatorURL), nil // $validate wrapper (NOT a thin HTTP validator)
	default:
		return nil, fmt.Errorf("no FHIR validator URL (not in /discovery, not in env) and SHN_FAKE_VALIDATOR not set: refusing to run without per-message validation (FR-36)")
	}
}

// convergeRegistry snapshots the /holders feed into reg (Holder → RegistryEntry).
// The engine reads EncPub from reg to seal to a recipient; a malformed/missing
// entry simply fails closed on Lookup. SDK-only — passes the gateway boundary fence.
func convergeRegistry(ctx context.Context, c *http.Client, registrarURL string, reg shnsdk.Registry) error {
	holders, err := shnsdk.FetchHolders(ctx, c, registrarURL)
	if err != nil {
		return err
	}
	for _, h := range holders {
		encPub, err := h.EncKey() // base64 → *[32]byte (sdk/holders.go)
		if err != nil {
			continue
		}
		var signPub ed25519.PublicKey
		if raw, derr := base64.StdEncoding.DecodeString(h.SignPub); derr == nil && len(raw) == ed25519.PublicKeySize {
			signPub = ed25519.PublicKey(raw)
		}
		reg.Set(h.ID, shnsdk.RegistryEntry{ID: h.ID, Role: h.Role, EncPub: encPub, SignPub: signPub, BaseURL: h.BaseURL})
	}
	return nil
}

// pollFeed re-converges the registry on an interval (post-boot peer registration/rotation).
func pollFeed(ctx context.Context, c *http.Client, registrarURL string, reg shnsdk.Registry, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = convergeRegistry(ctx, c, registrarURL, reg) // best-effort; transient feed errors are non-fatal
		}
	}
}

// fhirHTTPClient builds the HTTP client for the FHIR SoR connector. When the SMART
// quad (FHIR_TOKEN_URL + FHIR_CLIENT_ID/KEY/ALG) is set it authenticates via SMART
// Backend Services (RFC 7523 signed-JWT client-credentials); else nil ⇒
// unauthenticated (sandbox default). Mirrors cmd/gateway/main.go's connector branch.
func fhirHTTPClient(cfg config) (*http.Client, error) {
	if cfg.FHIRTokenURL == "" {
		return nil, nil // unauthenticated (sandbox default)
	}
	key, err := loadSmartKey(cfg.FHIRClientKey, cfg.FHIRClientAlg)
	if err != nil {
		return nil, fmt.Errorf("load FHIR client key: %w", err)
	}
	hc, err := smartauth.NewHTTPClient(smartauth.Config{
		TokenURL: cfg.FHIRTokenURL, ClientID: cfg.FHIRClientID, Scope: cfg.FHIRClientScope,
		Alg: cfg.FHIRClientAlg, Key: key, KID: cfg.FHIRClientKID,
	})
	if err != nil {
		return nil, fmt.Errorf("smartauth client: %w", err)
	}
	return hc, nil
}

// payerDavinciHTTPClient returns the client the native-forward Responder uses to reach
// the partner Da Vinci payer. When the PAYER_DAVINCI SMART quad is set it authenticates
// via SMART Backend Services; else nil ⇒ unauthenticated (deliberate sandbox mode).
func payerDavinciHTTPClient(cfg config) (*http.Client, error) {
	if cfg.PayerDavinciTokenURL == "" {
		return nil, nil // unauthenticated (deliberate; warned at build)
	}
	key, err := loadSmartKey(cfg.PayerDavinciClientKey, cfg.PayerDavinciClientAlg)
	if err != nil {
		return nil, fmt.Errorf("load payer-davinci client key: %w", err)
	}
	hc, err := smartauth.NewHTTPClient(smartauth.Config{
		TokenURL: cfg.PayerDavinciTokenURL, ClientID: cfg.PayerDavinciClientID, Scope: cfg.PayerDavinciScope,
		Alg: cfg.PayerDavinciClientAlg, Key: key, KID: cfg.PayerDavinciClientKID,
	})
	if err != nil {
		return nil, fmt.Errorf("payer-davinci smartauth client: %w", err)
	}
	return hc, nil
}

// loadSmartKey reads a PEM-encoded EC or RSA private key from path. Only ES384
// and RS384 are supported (AI-11 / OWD-6: no shared-secret algorithms).
func loadSmartKey(path, alg string) (crypto.PrivateKey, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read key %q: %w", path, err)
	}
	switch alg {
	case "ES384":
		return jwt.ParseECPrivateKeyFromPEM(pemBytes)
	case "RS384":
		return jwt.ParseRSAPrivateKeyFromPEM(pemBytes)
	default:
		return nil, fmt.Errorf("unsupported alg %q", alg)
	}
}
