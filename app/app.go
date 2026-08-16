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
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	checks "github.com/SmartHealthNetwork/shn-gateway/checks"
	fhirsor "github.com/SmartHealthNetwork/shn-gateway/connectors/fhirsor"
	pgstore "github.com/SmartHealthNetwork/shn-gateway/connectors/pgstore"
	smartauth "github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
	observer "github.com/SmartHealthNetwork/shn-gateway/observer"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	health "github.com/SmartHealthNetwork/shn-sdk/health"
	metrics "github.com/SmartHealthNetwork/shn-sdk/metrics"
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

	// ObserverAddr is the loopback-only bind address for the observer SSE stream:
	// opt-in structured edge events for local inspection tooling (see STABILITY.md).
	// Empty = off — the published-binary default. Set by the SHN Kit's shnkitd
	// daemon when launching the gateway child.
	ObserverAddr string

	// TLSCertFile/TLSKeyFile enable in-container TLS on the main listener.
	// Both-or-neither (loadConfig enforces); empty = plain HTTP, the default.
	// Terminating at a load balancer instead is the common deployment and stays
	// fully supported — this exists for hops that must not carry plaintext even
	// inside the operator's own network (e.g. an EHR/interface-engine push into
	// the Da Vinci ingress, which carries PHI upstream of sealing and is
	// authenticated with short-lived bearers). Conventionally paired with
	// PORT=8443. See gateway/docs/DEPLOYMENT.md.
	TLSCertFile string
	TLSKeyFile  string

	// MetricsService enables CloudWatch EMF metric emission (LegOutcome /
	// LegError) and names this gateway's Service dimension.
	// Empty = off — the published-binary default; the preview deployment sets
	// it per ECS service. Namespace/env dims follow the monitor's conventions.
	MetricsService   string
	MetricsNamespace string
	MetricsEnv       string

	// Endpoint overrides (normally discovery-resolved; explicit env wins).
	AuthzURL        string
	HubURL          string
	ConsentURL      string
	AuditURL        string
	PHGURL          string
	RegistrarURL    string
	FHIRValidateURL string
	// FHIRValidateURL21 / FHIRValidateURL22 are the per-LINE $validate lanes
	// (FHIR_VALIDATE_URL_2_1 / FHIR_VALIDATE_URL_2_2).
	// A HAPI instance can host exactly ONE version of an IG, so a deployment that
	// DECLARES a 2.1 or 2.2 contract line must run a validator for that line;
	// FHIR_VALIDATE_URL remains the canonical (2.0) lane. Boot fails closed when a
	// declared non-canonical line has no lane (FR-36/FR-G29) — see
	// validatorLanesForDeclared.
	FHIRValidateURL21 string
	FHIRValidateURL22 string
	// ContractVersions is the operator-DECLARED exchange-contract token set
	// (SHN_CONTRACT_VERSIONS, comma-separated). Boot-validated: token grammar +
	// membership of shnsdk.NativeContractVersions(). Empty env ⇒ this build's
	// default declaration. Single-sourced (D1a): it drives leg selection, the
	// published CapabilityStatements / davinci-configuration, AND the registry
	// declaration peers select this holder against.
	ContractVersions []string
	NPI              string

	// DemoEgressNativeLines (SHN_DEMO_EGRESS_NATIVE_LINES, kit-bridging demo
	// only) narrows engine.Config.EgressNativeLines (D1c): restricts arm (2)'s
	// native-reach view so transform chains can fire against skewed peers, the
	// same seam the cross-line pair test suite uses. Empty env ⇒ nil (unset —
	// the production default: full native reach). Boot-validated: every token
	// must be a line of shnsdk.NativeContractVersions(). Loud by name and by
	// boot log — never set in any shipped deploy config.
	DemoEgressNativeLines []string

	// Trust-anchor key-fetch URL overrides (first-class operator config):
	// override the discovery-advertised key URL when the gateway runs in the same
	// network as the substrate. firstNonEmpty(env, discovery); discovery is the default.
	AuthzPubkeyURL     string
	HubTransportKeyURL string

	StoreDatabaseURL string // Postgres DSN for the durable pgstore Store (else memstub).

	// Optional FHIR connector block (else memstub SoR/Store).
	FHIRDataURL      string
	FHIRTokenURL     string
	FHIRClientID     string
	FHIRClientKey    string
	FHIRClientAlg    string
	FHIRClientScope  string
	FHIRClientKID    string
	FHIRClientSecret string // value, not a path (unlike *ClientKey)

	// Optional native-forward payer block (the PARTNER Da Vinci payer is a different
	// external party from the FHIR SoR — distinct credentials). Setting
	// PayerDavinciBaseURL switches the payer's read-only legs to native forwarding.
	PayerDavinciBaseURL string
	// PayerDavinciCDSBaseURL is the base for the partner's CDS Hooks (CRD) posts when it
	// is NOT co-located with the FHIR base — e.g. br-payer serves CDS Hooks at root
	// /cds-services but FHIR ops under /fhir. Empty ⇒ CDS uses PayerDavinciBaseURL
	// (co-located default). FR-G28 / OWD-G8.
	PayerDavinciCDSBaseURL   string
	PayerDavinciTokenURL     string
	PayerDavinciClientID     string
	PayerDavinciClientKey    string
	PayerDavinciClientAlg    string
	PayerDavinciScope        string
	PayerDavinciClientKID    string
	PayerDavinciClientSecret string // value, not a path (unlike *ClientKey)
	PayerDavinciPASNative    bool
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
	// PayerDavinciCRDCoverageBundle wraps the CRD request's bare prefetch.coverage into a
	// searchset Bundle on egress — for a partner whose order-sign `coverage` prefetch
	// is a SEARCH template demanding a Bundle (bare → 412). The SHN spine carries a bare
	// Coverage, so this is a partner-scoped egress transform run after the bind. Off ⇒ verbatim
	// (br-payer untouched). PAYER_DAVINCI_CRD_COVERAGE_BUNDLE=true.
	PayerDavinciCRDCoverageBundle bool

	// PayerDavinciContractVersions is the operator-declared per-peer contract
	// token set ("<contract>@<line>", comma-separated) the payer FHIR-metadata
	// probe verifies published evidence against (checks.FailVersionDrift).
	// PAYER_DAVINCI_CONTRACT_VERSIONS. Requires
	// PayerDavinciBaseURL — there is nothing to verify against otherwise.
	PayerDavinciContractVersions []string

	// PayerDavinciStrictExtensions carries PAYER_DAVINCI_STRICT_EXTENSIONS
	// (the per-peer strict-extensions overlay, FR-G52) into engine.WithStrictExtensions on the
	// native responder — nativeResponder.strictExtensions, DORMANT plumbing
	// (no Handle-filter consult, no behavior delta; see that field's
	// comment in gateway/engine/native.go and g.strictPeer's comment in
	// originate.go for why this flag has NO live routing effect anywhere
	// today). PAS_NATIVE's bool-flag precedent.
	PayerDavinciStrictExtensions bool

	// OriginationProfile selects the per-UC origination lane: "" / "sandbox"
	// keep the CPT/lumbar order shape; "provider-data" originates every UC off the
	// provider's seeded SoR and drives real br-payer verdicts. ORIGINATION_PROFILE.
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

	// ChecksToken gates the operator connectivity-probe surface at
	// /internal/checks. Set by CHECKS_TOKEN. Empty (the default)
	// falls back to loopback-only access — see checks.Handler.
	ChecksToken string
}

var validRoles = map[string]bool{
	"provider": true,
	"payer":    true,
	"facility": true,
	"phg":      true,
}

// contractVersionTokenRe mirrors the registrar admission grammar
// (internal/registrar/service.go contractVersionRe) — the gateway cannot
// import internal/, so the pattern is pinned here verbatim.
var contractVersionTokenRe = regexp.MustCompile(`^[a-z0-9]+(\.[a-z0-9]+)*@[0-9]+(\.[0-9]+)*$`)

// splitTrimmed splits a comma-separated env value, trims whitespace off each
// element, and drops empties (a trailing comma or blank entry is silently
// ignored rather than becoming a spurious "" element downstream).
func splitTrimmed(v string) []string {
	if v == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		if t := strings.TrimSpace(part); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// checkClientAuthMode enforces exactly one outbound client-auth mode per
// credential block: private_key_jwt (KEY+ALG, preferred) or client_secret_post
// (SECRET). prefix is the env family, "FHIR" or "PAYER_DAVINCI".
func checkClientAuthMode(prefix, key, alg, kid, secret string) error {
	hasJWT := key != "" || alg != "" || kid != ""
	switch {
	case secret != "" && hasJWT:
		return fmt.Errorf("gateway: %s_CLIENT_SECRET and %s_CLIENT_KEY/_ALG/_KID are mutually exclusive — configure private_key_jwt (KEY+ALG) or client_secret (SECRET), not both", prefix, prefix)
	case secret != "":
		return nil
	case key == "" && alg == "":
		return fmt.Errorf("gateway: %s_TOKEN_URL requires credentials — either %s_CLIENT_KEY + %s_CLIENT_ALG (private_key_jwt, preferred) or %s_CLIENT_SECRET (client_secret_post)", prefix, prefix, prefix, prefix)
	case key == "":
		return fmt.Errorf("gateway: %s_CLIENT_ALG set requires %s_CLIENT_KEY", prefix, prefix)
	case alg != "ES384" && alg != "RS384":
		return fmt.Errorf("gateway: %s_CLIENT_ALG must be ES384|RS384, got %q", prefix, alg)
	}
	return nil
}

// loadConfig reads the collapsed PUBLIC surface from getenv. Mirrors the
// substrate cmd/gateway loadConfig validation (collapsed-surface URL checks,
// ROLE/PORT bounds, the FHIR/SMART credential block guards) MINUS the seed/SHN_MANIFEST
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
		Role:              role,
		Addr:              host + ":" + port,
		SecretsDir:        secretsDir,
		DiscoveryURL:      discoveryURL,
		AuthzURL:          getenv("AUTHZ_URL"),
		HubURL:            getenv("HUB_URL"),
		ConsentURL:        getenv("CONSENT_URL"),
		AuditURL:          getenv("AUDIT_URL"),
		PHGURL:            getenv("PHG_URL"),
		RegistrarURL:      getenv("REGISTRAR_URL"),
		FHIRValidateURL:   getenv("FHIR_VALIDATE_URL"),
		FHIRValidateURL21: getenv("FHIR_VALIDATE_URL_2_1"),
		FHIRValidateURL22: getenv("FHIR_VALIDATE_URL_2_2"),
		StoreDatabaseURL:  getenv("SHN_STORE_DATABASE_URL"),
		NPI:               def("NPI", "1234567890"),
		FHIRDataURL:       getenv("FHIR_DATA_URL"),
		FHIRTokenURL:      getenv("FHIR_TOKEN_URL"),
		FHIRClientID:      getenv("FHIR_CLIENT_ID"),
		FHIRClientKey:     getenv("FHIR_CLIENT_KEY"),
		FHIRClientAlg:     getenv("FHIR_CLIENT_ALG"),
		FHIRClientScope:   def("FHIR_CLIENT_SCOPE", "system/*.read"),
		FHIRClientKID:     getenv("FHIR_CLIENT_KID"),
		FHIRClientSecret:  getenv("FHIR_CLIENT_SECRET"),

		PayerDavinciBaseURL:           getenv("PAYER_DAVINCI_BASE_URL"),
		PayerDavinciCDSBaseURL:        getenv("PAYER_DAVINCI_CDS_BASE_URL"),
		PayerDavinciTokenURL:          getenv("PAYER_DAVINCI_TOKEN_URL"),
		PayerDavinciClientID:          getenv("PAYER_DAVINCI_CLIENT_ID"),
		PayerDavinciClientKey:         getenv("PAYER_DAVINCI_CLIENT_KEY"),
		PayerDavinciClientAlg:         getenv("PAYER_DAVINCI_CLIENT_ALG"),
		PayerDavinciScope:             def("PAYER_DAVINCI_SCOPE", "system/*.read"),
		PayerDavinciClientKID:         getenv("PAYER_DAVINCI_CLIENT_KID"),
		PayerDavinciClientSecret:      getenv("PAYER_DAVINCI_CLIENT_SECRET"),
		PayerDavinciPASNative:         getenv("PAYER_DAVINCI_PAS_NATIVE") == "true",
		PayerDavinciCRDServiceID:      getenv("PAYER_DAVINCI_CRD_SERVICE_ID"),
		PayerDavinciCRDHook:           getenv("PAYER_DAVINCI_CRD_HOOK"),
		PayerDavinciDispatchServiceID: getenv("PAYER_DAVINCI_DISPATCH_SERVICE_ID"),
		PayerDavinciDispatchHook:      getenv("PAYER_DAVINCI_DISPATCH_HOOK"),
		PayerDavinciCRDCoverageBundle: getenv("PAYER_DAVINCI_CRD_COVERAGE_BUNDLE") == "true",
		PayerDavinciContractVersions:  splitTrimmed(getenv("PAYER_DAVINCI_CONTRACT_VERSIONS")),
		PayerDavinciStrictExtensions:  getenv("PAYER_DAVINCI_STRICT_EXTENSIONS") == "true",
		OriginationProfile:            getenv("ORIGINATION_PROFILE"),

		ProviderDTRNative:      getenv("PROVIDER_DTR_NATIVE") == "true",
		ProviderDTRPopulateURL: getenv("PROVIDER_DTR_POPULATE_URL"),
		ProviderDavinciIngress: getenv("PROVIDER_DAVINCI_INGRESS") != "",

		ProviderDavinciIngressBaseURL:     getenv("PROVIDER_DAVINCI_INGRESS_BASE_URL"),
		ProviderDavinciIngressClientsFile: getenv("INGRESS_CLIENTS_FILE"),

		AuthzPubkeyURL:     getenv("AUTHZ_PUBKEY_URL"),
		HubTransportKeyURL: getenv("HUB_TRANSPORT_KEY_URL"),
	}

	for _, pair := range optionalURLs(cfg) {
		if err := checkOptionalURL(pair[0], pair[1]); err != nil {
			return config{}, fmt.Errorf("gateway: %w", err)
		}
	}

	cfg.ChecksToken = getenv("CHECKS_TOKEN")

	// D1a: the operator-declared contract set, boot-validated ONCE here (grammar +
	// membership of NativeContractVersions) so a typo'd or unbuildable token is a
	// refusal to start, never a peer-visible declaration this build cannot honor.
	declared, derr := shnsdk.ParseDeclaredContractVersions(getenv("SHN_CONTRACT_VERSIONS"))
	if derr != nil {
		return config{}, fmt.Errorf("gateway: SHN_CONTRACT_VERSIONS: %w", derr)
	}
	cfg.ContractVersions = declared

	// Demo-only egress narrowing (kit bridging demo): restricts arm (2)'s
	// native-reach view so transform chains can fire against skewed peers.
	// Loud by name and by log — never set in any shipped deploy config.
	if raw := strings.TrimSpace(getenv("SHN_DEMO_EGRESS_NATIVE_LINES")); raw != "" {
		native := map[string]bool{}
		for _, tok := range shnsdk.NativeContractVersions() {
			native[shnsdk.LineOf(tok)] = true
		}
		for _, l := range strings.Split(raw, ",") {
			l = strings.TrimSpace(l)
			if l == "" {
				continue
			}
			if !native[l] {
				return config{}, fmt.Errorf("gateway: SHN_DEMO_EGRESS_NATIVE_LINES: unknown line %q (native lines only)", l)
			}
			cfg.DemoEgressNativeLines = append(cfg.DemoEgressNativeLines, l)
		}
		// A set-but-lineless value (e.g. ",") must refuse, not silently run
		// un-narrowed — the knob's premise is loudness.
		if len(cfg.DemoEgressNativeLines) == 0 {
			return config{}, fmt.Errorf("gateway: SHN_DEMO_EGRESS_NATIVE_LINES is set but names no line (got %q)", raw)
		}
	}

	if cfg.FHIRTokenURL != "" {
		if cfg.FHIRDataURL == "" {
			return config{}, fmt.Errorf("gateway: FHIR_TOKEN_URL set requires FHIR_DATA_URL (auth needs a FHIR server to authenticate to)")
		}
		if cfg.FHIRClientID == "" {
			return config{}, fmt.Errorf("gateway: FHIR_TOKEN_URL requires FHIR_CLIENT_ID")
		}
		if err := checkClientAuthMode("FHIR", cfg.FHIRClientKey, cfg.FHIRClientAlg, cfg.FHIRClientKID, cfg.FHIRClientSecret); err != nil {
			return config{}, err
		}
	}

	// Exactly-one-mode partner-payer credentials: a partial or mixed block is a
	// misconfig (someone intended auth and fat-fingered it) → hard error. Zero creds
	// is the deliberate-unauthenticated mode (warned at build, not errored here).
	if cfg.PayerDavinciTokenURL != "" {
		if cfg.PayerDavinciBaseURL == "" {
			return config{}, fmt.Errorf("gateway: PAYER_DAVINCI_TOKEN_URL set requires PAYER_DAVINCI_BASE_URL")
		}
		if cfg.PayerDavinciClientID == "" {
			return config{}, fmt.Errorf("gateway: PAYER_DAVINCI_TOKEN_URL requires PAYER_DAVINCI_CLIENT_ID")
		}
		if err := checkClientAuthMode("PAYER_DAVINCI", cfg.PayerDavinciClientKey, cfg.PayerDavinciClientAlg, cfg.PayerDavinciClientKID, cfg.PayerDavinciClientSecret); err != nil {
			return config{}, err
		}
	}

	if len(cfg.PayerDavinciContractVersions) > 0 {
		if cfg.PayerDavinciBaseURL == "" {
			return config{}, fmt.Errorf("gateway: PAYER_DAVINCI_CONTRACT_VERSIONS set requires PAYER_DAVINCI_BASE_URL")
		}
		for _, tok := range cfg.PayerDavinciContractVersions {
			if len(tok) < 3 || len(tok) > 48 || !contractVersionTokenRe.MatchString(tok) {
				return config{}, fmt.Errorf("gateway: PAYER_DAVINCI_CONTRACT_VERSIONS token %q must match <contract>@<line> (e.g. pa.pas@2.0)", tok)
			}
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

	// Observer stream: off unless OBSERVER_ADDR is set, and REFUSED unless the
	// bind host is loopback — the observer carries edge payloads; it is a
	// local diagnostic surface, never a network service.
	if addr := getenv("OBSERVER_ADDR"); addr != "" {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return config{}, fmt.Errorf("gateway: OBSERVER_ADDR %q: %v", addr, err)
		}
		if host != "127.0.0.1" && host != "::1" && host != "localhost" {
			return config{}, fmt.Errorf("gateway: OBSERVER_ADDR %q is not loopback; the observer stream binds loopback only", addr)
		}
		cfg.ObserverAddr = addr
	}

	// In-container TLS: opt-in, both-or-neither. A half-configured pair is a boot
	// error — never a silent fall back to plaintext.
	cfg.TLSCertFile = getenv("TLS_CERT_FILE")
	cfg.TLSKeyFile = getenv("TLS_KEY_FILE")
	if (cfg.TLSCertFile == "") != (cfg.TLSKeyFile == "") {
		return config{}, fmt.Errorf("gateway: TLS_CERT_FILE and TLS_KEY_FILE must be set together (got cert=%q key=%q)", cfg.TLSCertFile, cfg.TLSKeyFile)
	}

	// EMF metrics opt-in: off unless METRICS_SERVICE names this gateway's
	// Service dimension (the preview ECS service key). Emission is additive
	// and fire-and-forget; the published binary keeps it off by default.
	cfg.MetricsService = getenv("METRICS_SERVICE")
	cfg.MetricsNamespace = getenv("METRICS_NAMESPACE")
	if cfg.MetricsNamespace == "" {
		cfg.MetricsNamespace = "SHN/Preview"
	}
	cfg.MetricsEnv = getenv("METRICS_ENV")
	if cfg.MetricsEnv == "" {
		cfg.MetricsEnv = "shn-preview"
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

// optionalURLs is the single table of every URL-shaped config field, keyed by
// its env var name — SHN_DISCOVERY_URL included (it's required, but its
// well-formedness still rides this same check). loadConfig's boot-time
// well-formedness loop and checkTargets (the /internal/checks probe target
// list) BOTH walk this exact table so the two can never diverge:
// a URL added here is automatically both boot-validated and, when non-empty,
// probed. No entry is hand-kept in a second place. The single sanctioned
// exception is checkTargets' PAYER_DAVINCI_WELL_KNOWN companion target: it
// reuses this table's already-boot-validated PAYER_DAVINCI_BASE_URL at a
// different path, rather than being an independently configured URL of its
// own.
func optionalURLs(cfg config) [][2]string {
	return [][2]string{
		{"AUTHZ_URL", cfg.AuthzURL},
		{"HUB_URL", cfg.HubURL},
		{"CONSENT_URL", cfg.ConsentURL},
		{"AUDIT_URL", cfg.AuditURL},
		{"PHG_URL", cfg.PHGURL},
		{"FHIR_VALIDATE_URL", cfg.FHIRValidateURL},
		{"FHIR_VALIDATE_URL_2_1", cfg.FHIRValidateURL21},
		{"FHIR_VALIDATE_URL_2_2", cfg.FHIRValidateURL22},
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
	}
}

// checkTargets derives the /internal/checks probe targets from
// optionalURLs(cfg) — the exact table checkOptionalURL walks at boot, so a
// target can never be added or dropped independently of that well-formedness
// gate. Skips unset pairs. Kind overlay: the two FHIR-facing base URLs probe
// as fhir-metadata (a live $/metadata fetch); the two SMART token endpoints
// probe as a live credential check via a closure over the gateway's own
// outbound token client (fhirTokenFetch/payerDavinciTokenFetch — reusing the
// exact SMART config the traffic path authenticates with, never a second
// hand-rolled OAuth client); every other configured pair — including the
// in-VPC substrate URLs (AUTHZ_URL, HUB_URL, …) that hosted tenants never set
// but self-hosted gateways do, where probing them is exactly what an
// operator wants — probes as a plain reachable GET. PAYER_DIRECTORY is a
// file path, not a URL; it is not in optionalURLs and is never probed. One
// derived companion target is appended after the loop, below —
// PAYER_DAVINCI_WELL_KNOWN — reusing PAYER_DAVINCI_BASE_URL's already
// boot-validated URL at a different path rather than adding a second
// independently configured entry.
func checkTargets(cfg config) []checks.Target {
	var out []checks.Target
	for _, pair := range optionalURLs(cfg) {
		name, u := pair[0], pair[1]
		if u == "" {
			continue
		}
		t := checks.Target{ID: name, URL: u}
		if name == "PAYER_DAVINCI_BASE_URL" {
			t.DeclaredVersions = cfg.PayerDavinciContractVersions
		}
		switch name {
		case "FHIR_DATA_URL", "PAYER_DAVINCI_BASE_URL":
			t.Kind = checks.KindFHIRMetadata
		case "FHIR_TOKEN_URL":
			t.Kind = checks.KindToken
			t.TokenFetch = fhirTokenFetch(cfg)
		case "PAYER_DAVINCI_TOKEN_URL":
			t.Kind = checks.KindToken
			t.TokenFetch = payerDavinciTokenFetch(cfg)
		default:
			t.Kind = checks.KindReachable
		}
		out = append(out, t)
	}

	// Derived companion probe (not a new table entry — same boot-validated
	// base URL as PAYER_DAVINCI_BASE_URL, different path): the HRex
	// .well-known/davinci-configuration, absence-tolerant (an evidence
	// path). Kept out of optionalURLs so the "one table" rule still holds:
	// this URL is never independently configured.
	if cfg.PayerDavinciBaseURL != "" {
		out = append(out, checks.Target{
			ID:               "PAYER_DAVINCI_WELL_KNOWN",
			Kind:             checks.KindDavinciConfig,
			URL:              cfg.PayerDavinciBaseURL,
			DeclaredVersions: cfg.PayerDavinciContractVersions,
		})
	}
	return out
}

// classifyTokenErr turns a smartauth token-fetch error into a
// *checks.StatusError when the failure was an HTTP status from the token
// endpoint (smartauth.TokenSource.fetch's "token endpoint status %d: ..."
// message — see connectors/smartauth/tokensource.go), so the /internal/checks
// result can report the status code. checks.probeToken already refuses to
// surface anything but a *StatusError's Code (the redaction rule): this
// function reads the raw error text only to pull the numeric status out of
// it, never returning that text itself — an error that doesn't match this
// shape (a dial failure, a wrapped context error, …) passes through
// unmodified and collapses to the fixed "credential check failed" string
// downstream.
func classifyTokenErr(err error) error {
	if err == nil {
		return nil
	}
	var code int
	if n, _ := fmt.Sscanf(err.Error(), "smartauth: token endpoint status %d:", &code); n == 1 {
		return &checks.StatusError{Code: code}
	}
	return err
}

// fhirTokenFetch builds the /internal/checks credential-check closure for
// FHIR_TOKEN_URL: a *smartauth.TokenSource built from the SAME Config
// fhirHTTPClient authenticates the FHIR SoR connector with — the minimal
// in-app seam the task calls for (a TokenSource, not a new smartauth export)
// so this package never hand-rolls a second OAuth client. A key/PEM load
// failure at target-construction time is captured and replayed as the
// closure's error on every probe (rather than probed to death at boot) —
// build() itself still fails fast via fhirHTTPClient before serving.
//
// A FRESH *smartauth.TokenSource is constructed INSIDE the returned closure,
// on every invocation — never hoisted to a shared variable captured by the
// closure. TokenSource.Token caches and serves a still-valid token without
// hitting the network (that's correct behavior for the traffic-path client,
// which wants to avoid minting on every request), but this is supposed to be
// a LIVE credential check: a shared, closed-over TokenSource would report ok
// in ~0ms off a cached token for up to TTL-RefreshSkew after a secret
// rotation or IdP outage, silently stopping being a check. The Runner's own
// 30s cooldown (checks.go) is what bounds how often this actually mints
// against the partner IdP — it, not caching here, is the partner-lockout
// guard.
func fhirTokenFetch(cfg config) func(context.Context) error {
	sc := smartauth.Config{TokenURL: cfg.FHIRTokenURL, ClientID: cfg.FHIRClientID, Scope: cfg.FHIRClientScope}
	if cfg.FHIRClientSecret != "" {
		sc.ClientSecret = cfg.FHIRClientSecret
	} else {
		key, err := loadSmartKey(cfg.FHIRClientKey, cfg.FHIRClientAlg)
		if err != nil {
			return func(context.Context) error { return err }
		}
		sc.Alg, sc.Key, sc.KID = cfg.FHIRClientAlg, key, cfg.FHIRClientKID
	}
	return func(ctx context.Context) error {
		return classifyTokenErr(errFromToken(&smartauth.TokenSource{Config: sc}, ctx))
	}
}

// payerDavinciTokenFetch is fhirTokenFetch's PAYER_DAVINCI_* counterpart,
// mirroring payerDavinciHTTPClient's Config construction. Same fresh-
// TokenSource-per-invocation rule applies (see fhirTokenFetch's doc).
func payerDavinciTokenFetch(cfg config) func(context.Context) error {
	sc := smartauth.Config{TokenURL: cfg.PayerDavinciTokenURL, ClientID: cfg.PayerDavinciClientID, Scope: cfg.PayerDavinciScope}
	if cfg.PayerDavinciClientSecret != "" {
		sc.ClientSecret = cfg.PayerDavinciClientSecret
	} else {
		key, err := loadSmartKey(cfg.PayerDavinciClientKey, cfg.PayerDavinciClientAlg)
		if err != nil {
			return func(context.Context) error { return err }
		}
		sc.Alg, sc.Key, sc.KID = cfg.PayerDavinciClientAlg, key, cfg.PayerDavinciClientKID
	}
	return func(ctx context.Context) error {
		return classifyTokenErr(errFromToken(&smartauth.TokenSource{Config: sc}, ctx))
	}
}

// errFromToken discards the minted token, keeping only the error — Token
// itself has no error-only form.
func errFromToken(ts *smartauth.TokenSource, ctx context.Context) error {
	_, err := ts.Token(ctx)
	return err
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

	// tlsCert is the optional in-container TLS keypair for the MAIN listener,
	// nil when TLS_CERT_FILE/TLS_KEY_FILE are unset (the default). The observer
	// listener is deliberately excluded — it binds loopback only.
	tlsCert *tls.Certificate

	// observerAddr/observerHandler: the loopback SSE stream (see STABILITY.md),
	// non-empty/non-nil only when OBSERVER_ADDR is configured. Only Run starts
	// this listener — HandlerWithClock embedders get no observer endpoint.
	observerAddr    string
	observerHandler http.Handler

	// healthCell is the registrar-poller /health check cell — non-nil only when
	// registrarURL != "" (mirrors the poller-goroutine gate in Run). pollFeed's
	// Record* calls are nil-safe, so a nil cell here is a no-op, not a crash.
	healthCell *health.PollerCell

	// checksRunner drives /internal/checks — the operator
	// connectivity-probe surface. Run starts its boot-time probe in a
	// goroutine (never build: the listener must open without waiting on
	// partner endpoints), and it is never registered as a health.Check —
	// checks never gate /health.
	checksRunner *checks.Runner

	// nativeResponder is the checks-runner→responder endpoint-evidence sink built()
	// wires checksRunner.OnResults into (nil when native-forward mode is
	// off). Exposed for TEST OBSERVATION ONLY — TestProbeEvidenceReachesResponder
	// reads it back (a *nativeResponder-satisfied anonymous interface
	// assertion) to prove evidence that arrived via a REAL checksRunner.Run
	// landed here; the WRITE path under test is exactly this field's
	// production wiring, never a second, test-only injection route.
	nativeResponder engine.EndpointEvidenceSetter
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

	// Fail fast on a role/bundle mismatch: a bundle registered as one role but
	// mounted under another ROLE boots today and then default-denies at runtime
	// (opaque 502 at the first origination). Manifest.Role is what `shn register
	// --role` stamped — a pure-local check that catches mounting the wrong bundle;
	// the authoritative role stays server-side (registry). An empty manifest role
	// (pre-role-stamp bundle) is tolerated.
	if r := bundle.Manifest.Role; r != "" && r != cfg.Role {
		return b, fmt.Errorf("gateway: ROLE=%s but this bundle registered as %s — mount the matching bundle or fix ROLE", cfg.Role, r)
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
	// Per-LINE lanes (F7): fail-closed when a DECLARED multi-line contract line has
	// no validator to answer for it. Runs here, at boot, so the refusal is a startup
	// error naming the missing env — never a surprise 500 mid-exchange.
	validatorLanes, err := validatorLanesForDeclared(getenv, cfg.ContractVersions, validator, cfg)
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
		if _, err := convergeRegistry(ctx, client, registrarURL, reg); err != nil {
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
		hc, herr := fhirHTTPClient(cfg) // smartauth.NewHTTPClient when the SMART credential block is set, else nil (unauthenticated)
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
	// pool is hoisted out of the if so the /health wiring below can register a
	// DBPing check when (and only when) this branch ran.
	var pool *pgxpool.Pool
	if cfg.StoreDatabaseURL != "" {
		p, perr := pgxpool.New(ctx, cfg.StoreDatabaseURL)
		if perr != nil {
			return b, fmt.Errorf("gateway: pgxpool.New(store): %w", perr)
		}
		pg, serr := pgstore.NewPgStore(ctx, p, bundle.Identity.HolderID)
		if serr != nil {
			return b, fmt.Errorf("gateway: pgstore.NewPgStore: %w", serr)
		}
		store = pg
		pool = p
	}

	// /health: holder id as the service name — holder ids
	// are already public via the /holders feed, non-sensitive, and distinguish the
	// two payer-role gateways from each other. feedCell is created ONLY when a
	// registrar is configured (mirrors the poller-goroutine gate in Run); its
	// Check is NOT nil-safe, so it must only be Register'd inside that branch.
	hreg := health.New(bundle.Identity.HolderID, getenv("SHN_VERSION"))
	var feedCell *health.PollerCell
	if registrarURL != "" {
		feedCell = health.NewPollerCell("registrar-poller", 30*time.Second)
		hreg.Register(feedCell.Check)
	}
	if pool != nil {
		hreg.Register(health.DBPing("store", pool.Ping))
	}

	// PayerRouter (FR-G40/G41): default to feed-derived discovery off the converged /holders
	// registry (FeedPayerRouter). PAYER_DIRECTORY, when set, overrides with a static
	// provider-maintained map (test/bootstrap fallback). No default payer holder either way.
	var payerRouter engine.PayerRouter = engine.NewFeedPayerRouter(reg)
	if dir := getenv("PAYER_DIRECTORY"); dir != "" {
		entries, derr := engine.LoadPayerDirectory(dir)
		if derr != nil {
			return b, fmt.Errorf("gateway: PAYER_DIRECTORY: %w", derr)
		}
		pr, derr := engine.NewConfigPayerRouter(entries)
		if derr != nil {
			return b, fmt.Errorf("gateway: PAYER_DIRECTORY: %w", derr)
		}
		payerRouter = pr
	}

	gwCfg := engine.Config{
		Role:             cfg.Role,
		HolderID:         bundle.Identity.HolderID,
		PayerRouter:      payerRouter,
		Identity:         bundle.Identity,
		AuthzURL:         firstNonEmpty(cfg.AuthzURL, endpoints.Authz),
		AuthzPub:         trust.AuthzPub,
		HubTransportPub:  trust.HubTransportPub,
		HubURL:           firstNonEmpty(cfg.HubURL, endpoints.Hub),
		Reg:              reg, // populated by the snapshot above
		Validator:        validator,
		ValidatorsByLine: validatorLanes,
		// D1a: the boot-validated declared set — read by leg selection, the published
		// CapabilityStatements / davinci-configuration, and (through the registrar
		// registration) the declaration peers select this holder against.
		DeclaredContractVersions: cfg.ContractVersions,
		// EgressNativeLines (D1c): nil in every shipped deploy; the kit-bridging
		// demo's SHN_DEMO_EGRESS_NATIVE_LINES is the sole non-test way to set it.
		EgressNativeLines: cfg.DemoEgressNativeLines,
		SoR:               sor,
		Store:             store,
		Adjudicator:       engine.NewSandboxAdjudicator(sor, clock),
		Clock:             clock, // production: time.Now; hermetic tests: the harness's injected clock (HandlerWithClock)
		Client:            client,
		NPI:               cfg.NPI,
		ConsentURL:        firstNonEmpty(cfg.ConsentURL, endpoints.Consent),
		AuditURL:          firstNonEmpty(cfg.AuditURL, endpoints.Audit),
		PHGURL:            firstNonEmpty(cfg.PHGURL, endpoints.PHG),

		OriginationProfile: cfg.OriginationProfile,
		// Strict extensions (FR-G52): g.strictPeer is production-dormant BY DESIGN (always false —
		// see its comment, gateway/engine/originate.go) — there is
		// deliberately no engine.Config field here feeding it from env.
	}
	if len(cfg.DemoEgressNativeLines) > 0 {
		fmt.Fprintf(stdout, "gateway: demo: egress-native lines narrowed to %v — arm-2 native reach restricted; transform chains may fire (SHN_DEMO_EGRESS_NATIVE_LINES)\n", cfg.DemoEgressNativeLines)
	}
	// evidenceSink is set below iff native-forward mode is on — the
	// checks-runner→responder endpoint-evidence wiring target (nil-safe: no native responder,
	// no sink to feed).
	var evidenceSink engine.EndpointEvidenceSetter
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
		// WithDeclaredContractVersions is scoped to the NATIVE responder only (the
		// foreign-endpoint filter): NewCompositeResponder routes read-only
		// legs to native and PAS to the sandbox fallback unless PayerDavinciPASNative
		// (composite.go), so a sandbox-fallback PAS leg is correctly NOT filtered by
		// this foreign declaration — the sandbox payer is not the foreign peer.
		native := engine.NewNativeResponder(pdc, cfg.PayerDavinciBaseURL, crdSvcID, store, clock,
			engine.WithCDSBaseURL(cfg.PayerDavinciCDSBaseURL),
			engine.WithCRDHook(cfg.PayerDavinciCRDHook),
			engine.WithCRDDispatchService(cfg.PayerDavinciDispatchServiceID, cfg.PayerDavinciDispatchHook),
			engine.WithCRDCoverageBundle(cfg.PayerDavinciCRDCoverageBundle),
			engine.WithDeclaredContractVersions(cfg.PayerDavinciContractVersions),
			// The foreign-peer filter's OWN half is this deployment's declared
			// set — the same accessor selection, the CapabilityStatements and the
			// registry declaration read — never the library build constant.
			engine.WithOwnContractVersions(cfg.ContractVersions),
			// Strict extensions (FR-G52): DORMANT plumbing (native.go's Handle never consults it, and
			// g.strictPeer never reads it either — strictPeer is
			// unconditionally false, its own comment explains why). Goes
			// live together with transform-at-the-native-forward-edge.
			engine.WithStrictExtensions(cfg.PayerDavinciStrictExtensions),
			// Endpoint evidence: same-origin-drop notes ride the file's existing WARNING-line
			// stdout precedent (e.g. the unauthenticated-forward warning above).
			engine.WithEndpointEvidenceObserver(func(note string) { fmt.Fprintf(stdout, "gateway: %s\n", note) }))
		evidenceSink = native
		fallback := engine.NewSandboxResponder(gwCfg.Adjudicator, sor, store, clock)
		gwCfg.Responder = engine.NewCompositeResponder(native, fallback, cfg.PayerDavinciPASNative)
		// The native-forward DTR response is a foreign Da Vinci package SHN can't $validate
		// (R-8 near-relay): tell the engine to skip the DTR egress foreign-$validate (FR-G28).
		gwCfg.PayerDavinciNative = true
	}
	if cfg.ProviderDTRNative {
		gwCfg.Populator = engine.NewNativePopulator(client, cfg.ProviderDTRPopulateURL)
	} else if cfg.OriginationProfile == "provider-data" {
		// Operated-CQL $populate against the provider tenant (the crux of the
		// provider-data lane). PROVIDER_DTR_POPULATE_URL is validated at loadConfig.
		gwCfg.Populator = engine.NewNativePopulator(client, cfg.ProviderDTRPopulateURL)
	}
	gwCfg.IngressEnabled = cfg.ProviderDavinciIngress
	gwCfg.IngressBaseURL = cfg.IngressBaseURL
	gwCfg.IngressClients = cfg.IngressClients

	// Observer stream: hub + engine callback, only when configured. The demo
	// endpoint (POST /demo/transform) rides the SAME mux via
	// composeObserverHandler — it inherits this branch's OBSERVER_ADDR gate
	// and the loopback-only bind validation already enforced at config load,
	// so there is no separate opt-in or listener for it.
	var obsHandler http.Handler
	if cfg.ObserverAddr != "" {
		hub := observer.NewHub()
		gwCfg.Observer = hub.Emit
		obsHandler = composeObserverHandler(hub.Handler())
	}

	// EMF leg metrics: opt-in via METRICS_SERVICE. EMF rides
	// stdout → awslogs → CloudWatch; sdk/metrics is fire-and-forget and
	// conformance-neutral (TestLegMetric_ConformanceNeutral).
	if cfg.MetricsService != "" {
		em := metrics.New(stdout, cfg.MetricsNamespace, map[string]string{"Env": cfg.MetricsEnv}, nil)
		gwCfg.LegMetric = legMetricHook(em, cfg.MetricsService, cfg.Role)
	}

	tlsCert, err := loadTLSCert(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		return b, err
	}
	scheme := "http"
	if tlsCert != nil {
		scheme = "https"
	}

	fmt.Fprintf(stdout, "gateway: role=%s holder=%s listening on %s://%s\n", cfg.Role, bundle.Identity.HolderID, scheme, cfg.Addr)

	// /internal/* is the operator/control-plane surface: never forwarded at the
	// hosted edge (the hosted control plane pins that), token- or loopback-gated
	// here. Inserted at this seam — shared by every role — rather
	// than the per-role engine mux.
	checksRunner := checks.NewRunner(checkTargets(cfg), client, clock)
	// Endpoint evidence: feed each completed checks cycle's PAYER_DAVINCI_WELL_KNOWN
	// (davinci-config probe) evidence into the native responder — the REAL
	// app-wiring hook (checks.Runner.OnResults, checks.go) from a live
	// checks cycle into evidenceSink.SetEndpointEvidence
	// (TestProbeEvidenceReachesResponder). nil evidenceSink (no native
	// responder configured) makes this a no-op.
	if evidenceSink != nil {
		checksRunner.OnResults = func(results []checks.Result) {
			for _, res := range results {
				if res.ID == "PAYER_DAVINCI_WELL_KNOWN" && res.Capability != nil {
					evidenceSink.SetEndpointEvidence(res.Capability.EndpointURLs)
				}
			}
		}
	}
	checksH := checks.Handler(checksRunner, cfg.ChecksToken)
	inner := health.Wrap(hreg, engine.New(gwCfg).Handler())
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/checks" {
			checksH.ServeHTTP(w, r)
			return
		}
		inner.ServeHTTP(w, r)
	})

	b = built{
		addr:            cfg.Addr,
		handler:         handler,
		reg:             reg,
		registrarURL:    registrarURL,
		client:          client,
		tlsCert:         tlsCert,
		observerAddr:    cfg.ObserverAddr,
		observerHandler: obsHandler,
		healthCell:      feedCell,
		checksRunner:    checksRunner,
		nativeResponder: evidenceSink,
	}
	return b, nil
}

// loadTLSCert loads the optional in-container TLS keypair. Both paths empty =>
// (nil, nil): TLS off, the default. Loading happens at BUILD time so a bad cert
// is a boot failure with a clear message, not a serve-time error surfacing after
// the gateway has already logged that it is listening.
func loadTLSCert(certFile, keyFile string) (*tls.Certificate, error) {
	if certFile == "" && keyFile == "" {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("gateway: loading TLS keypair (cert=%q key=%q): %w", certFile, keyFile, err)
	}
	return &cert, nil
}

// serverFor builds the main listener's *http.Server. A nil cert yields the plain
// HTTP server that has always been the default; a non-nil cert pins TLS 1.2 as
// the floor (below that is not acceptable for PHI-bearing hops).
func serverFor(addr string, h http.Handler, cert *tls.Certificate) *http.Server {
	s := &http.Server{Addr: addr, Handler: h}
	if cert != nil {
		s.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{*cert},
			MinVersion:   tls.VersionTLS12,
		}
	}
	return s
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
		go pollFeed(ctx, b.client, b.registrarURL, b.reg, 3*time.Second, b.healthCell)
	}
	// Boot-time connectivity check: after the listener is up, not
	// blocking it — build() must return without waiting on partner endpoints.
	go b.checksRunner.Run(ctx) //nolint:errcheck — results land in Last()
	errc := make(chan error, 2)
	main := serverFor(b.addr, b.handler, b.tlsCert)
	go func() {
		if b.tlsCert != nil {
			// Cert/key already loaded into TLSConfig at build time.
			errc <- main.ListenAndServeTLS("", "")
			return
		}
		errc <- main.ListenAndServe()
	}()
	if b.observerAddr != "" {
		// A bind failure here kills the gateway via the shared errc — FAIL-FAST by
		// intent: OBSERVER_ADDR is explicit opt-in (the Kit supervisor sets it), and
		// a silently-missing inspector stream is worse than a dead child the
		// supervisor detects and restarts.
		go func() { errc <- http.ListenAndServe(b.observerAddr, b.observerHandler) }()
	}
	return <-errc
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

// validatorLanesForDeclared builds the per-LINE $validate lane map (spec
// 2026-08-11, F7) and FAILS CLOSED when a declared line has no lane.
//
// The rule, and why it is fail-closed: a HAPI instance can host exactly one
// version of an IG, so validating a 2.2 payload against a 2.0 lane does not
// "mostly work" — it reports errors (or passes) for reasons unrelated to the
// payload. A deployment that DECLARES a line is telling peers it can produce and
// answer at that line; FR-36 says everything it produces is validated. Without a
// lane those two promises cannot both hold, so the gateway refuses to start
// rather than quietly validate against the wrong IG (FR-36/FR-G29).
//
// canonical is the already-resolved canonical-lane validator (selectValidator's
// result). Under SHN_FAKE_VALIDATOR=1 the fake serves EVERY line — the harness,
// e2e and every hermetic test keep working unchanged, at every line.
// Scope: only MULTI-LINE contracts (pa.crd/pa.dtr/pa.pas — those whose native set
// carries more than one line) can produce line-varying payloads and therefore need
// per-line lanes. A single-line contract (pa.pdex, native at 2.1 only) has nothing
// to choose between and rides the canonical lane, exactly as it did before this
// slice. That scoping is DERIVED from NativeContractVersions(), not hardcoded, so a
// contract that gains a second line automatically starts demanding lanes.
func validatorLanesForDeclared(getenv func(string) string, declared []string, canonical shnsdk.Validator, cfg config) (map[string]shnsdk.Validator, error) {
	fake := getenv("SHN_FAKE_VALIDATOR") == "1"
	// The canonical line — the one FHIR_VALIDATE_URL has always served — is the line
	// this build DEFAULT-declares for its multi-line contracts.
	canonicalLine := shnsdk.LineOf(shnsdk.ContractPAPAS20)
	urlForLine := map[string]string{"2.1": cfg.FHIRValidateURL21, "2.2": cfg.FHIRValidateURL22}

	linesPerContract := map[string]map[string]bool{}
	for _, tok := range shnsdk.NativeContractVersions() {
		contract, line, ok := strings.Cut(tok, "@")
		if !ok {
			continue
		}
		if linesPerContract[contract] == nil {
			linesPerContract[contract] = map[string]bool{}
		}
		linesPerContract[contract][line] = true
	}

	lanes := map[string]shnsdk.Validator{}
	for _, tok := range declared {
		contract, line, ok := strings.Cut(tok, "@")
		if !ok || line == "" || len(linesPerContract[contract]) < 2 {
			continue // malformed (already rejected upstream) or a single-line contract
		}
		if lanes[line] != nil {
			continue
		}
		switch {
		case fake:
			lanes[line] = canonical // the fake validates any line — harness/e2e unchanged
		case line == canonicalLine:
			lanes[line] = canonical
		case urlForLine[line] != "":
			lanes[line] = shnsdk.NewOperationValidator(urlForLine[line])
		default:
			// The refusal names the env to SET, never a way to turn validation OFF:
			// SHN_FAKE_VALIDATOR is a hermetic-test opt-in, and an operator reading a
			// production boot failure must not be handed "disable FR-36" as a remedy.
			envName := "FHIR_VALIDATE_URL_" + strings.ReplaceAll(line, ".", "_")
			return nil, fmt.Errorf("gateway: SHN_CONTRACT_VERSIONS declares %s but no FHIR validator lane is configured for line %s: set %s to a $validate endpoint hosting that line's IG packages (one HAPI hosts exactly one version of an IG) — refusing to declare a line this gateway cannot validate (FR-36/FR-G29)", tok, line, envName)
		}
	}
	// Lane map: widen beyond DECLARED — any NATIVE line of a multi-line contract with
	// a configured lane (FHIR_VALIDATE_URL_<line>, the canonical line, or
	// fake-mode) enters the map even when this deployment doesn't DECLARE it.
	// This is the exact widening the recorded route-selection deviation names:
	// arm (2) native-reach needs the lane map to cover more than the declared
	// set, and the request-frame native∩laned INBOUND-honor predicate reads this
	// SAME map — so the opt-in is bidirectional (CONFIGURATION.md states both
	// consequences). This block only ADDS lines; the declared-without-
	// lane fail-closed check above is unaffected — a declared line with no
	// configured lane still refuses boot.
	for _, lines := range linesPerContract {
		if len(lines) < 2 {
			continue // single-line contract: rides the canonical lane, unchanged
		}
		for line := range lines {
			if lanes[line] != nil {
				continue
			}
			switch {
			case fake:
				lanes[line] = canonical
			case line == canonicalLine:
				lanes[line] = canonical
			case urlForLine[line] != "":
				lanes[line] = shnsdk.NewOperationValidator(urlForLine[line])
			default:
				// No env configured for this undeclared native line — stays
				// absent from the map (validatorForLine resolves it to
				// unlaned, exactly as before D1a). This is an OPT-IN, never a
				// requirement to lane every native line.
			}
		}
	}
	// A single-line contract's line may be absent from lanes (pa.pdex@2.1 when 2.1 is
	// not otherwise declared). engine.validatorForLine treats an ABSENT line in a
	// non-empty lane map as unlaned, so map it to the canonical lane explicitly.
	for _, tok := range declared {
		contract, line, ok := strings.Cut(tok, "@")
		if ok && line != "" && len(linesPerContract[contract]) < 2 && lanes[line] == nil {
			lanes[line] = canonical
		}
	}
	return lanes, nil
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
// The returned count is the number of holders actually converged (successful
// reg.Set iterations, i.e. excluding entries skipped for a malformed EncPub) —
// callers feed it to the /health registrar-poller cell as the observed feed size.
func convergeRegistry(ctx context.Context, c *http.Client, registrarURL string, reg shnsdk.Registry) (int, error) {
	holders, err := shnsdk.FetchHolders(ctx, c, registrarURL)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, h := range holders {
		encPub, err := h.EncKey() // base64 → *[32]byte (sdk/holders.go)
		if err != nil {
			continue
		}
		var signPub ed25519.PublicKey
		if raw, derr := base64.StdEncoding.DecodeString(h.SignPub); derr == nil && len(raw) == ed25519.PublicKeySize {
			signPub = ed25519.PublicKey(raw)
		}
		reg.Set(h.ID, shnsdk.RegistryEntry{ID: h.ID, Role: h.Role, EncPub: encPub, SignPub: signPub, BaseURL: h.BaseURL, PayerIDs: h.PayerIDs, MessageFrames: h.MessageFrames, ContractVersions: h.ContractVersions, RequestFrames: h.RequestFrames})
		n++
	}
	return n, nil
}

// pollFeed re-converges the registry on an interval (post-boot peer registration/
// rotation). cell records each attempt/outcome onto the service's /health
// registrar-poller check — this is this path's first error visibility EVER:
// a transient feed error used to vanish silently
// (best-effort), now it surfaces as a degraded check until the next success.
// cell may be nil (registrarURL configured post-boot is not a supported path
// today, but PollerCell's Record* methods are nil-safe regardless).
func pollFeed(ctx context.Context, c *http.Client, registrarURL string, reg shnsdk.Registry, every time.Duration, cell *health.PollerCell) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cell.RecordAttempt()
			if n, err := convergeRegistry(ctx, c, registrarURL, reg); err != nil {
				cell.RecordError(err)
			} else {
				cell.RecordSuccess(n)
			}
		}
	}
}

// fhirHTTPClient builds the HTTP client for the FHIR SoR connector. When the SMART
// credential block (FHIR_TOKEN_URL + FHIR_CLIENT_ID + ...) is set it authenticates
// per the configured mode: RFC 7523 signed-JWT client-credentials (private_key_jwt,
// preferred) or client_secret_post when FHIR_CLIENT_SECRET is set; else nil ⇒
// unauthenticated (sandbox default). Mirrors cmd/gateway/main.go's connector branch.
func fhirHTTPClient(cfg config) (*http.Client, error) {
	if cfg.FHIRTokenURL == "" {
		return nil, nil // unauthenticated (sandbox default)
	}
	sc := smartauth.Config{
		TokenURL: cfg.FHIRTokenURL, ClientID: cfg.FHIRClientID, Scope: cfg.FHIRClientScope,
	}
	if cfg.FHIRClientSecret != "" {
		sc.ClientSecret = cfg.FHIRClientSecret // client_secret_post; no key material
	} else {
		key, err := loadSmartKey(cfg.FHIRClientKey, cfg.FHIRClientAlg)
		if err != nil {
			return nil, fmt.Errorf("load FHIR client key: %w", err)
		}
		sc.Alg, sc.Key, sc.KID = cfg.FHIRClientAlg, key, cfg.FHIRClientKID
	}
	hc, err := smartauth.NewHTTPClient(sc)
	if err != nil {
		return nil, fmt.Errorf("smartauth client: %w", err)
	}
	return hc, nil
}

// payerDavinciHTTPClient returns the client the native-forward Responder uses to reach
// the partner Da Vinci payer. When the PAYER_DAVINCI SMART credential block is set it
// authenticates per the configured mode: RFC 7523 signed-JWT client-credentials
// (private_key_jwt, preferred) or client_secret_post when PAYER_DAVINCI_CLIENT_SECRET
// is set; else nil ⇒ unauthenticated (deliberate sandbox mode).
func payerDavinciHTTPClient(cfg config) (*http.Client, error) {
	if cfg.PayerDavinciTokenURL == "" {
		return nil, nil // unauthenticated (deliberate; warned at build)
	}
	sc := smartauth.Config{
		TokenURL: cfg.PayerDavinciTokenURL, ClientID: cfg.PayerDavinciClientID, Scope: cfg.PayerDavinciScope,
	}
	if cfg.PayerDavinciClientSecret != "" {
		sc.ClientSecret = cfg.PayerDavinciClientSecret // client_secret_post; no key material
	} else {
		key, err := loadSmartKey(cfg.PayerDavinciClientKey, cfg.PayerDavinciClientAlg)
		if err != nil {
			return nil, fmt.Errorf("load payer-davinci client key: %w", err)
		}
		sc.Alg, sc.Key, sc.KID = cfg.PayerDavinciClientAlg, key, cfg.PayerDavinciClientKID
	}
	hc, err := smartauth.NewHTTPClient(sc)
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
