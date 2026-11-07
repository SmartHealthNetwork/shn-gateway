//go:build !shn_coreonly

// Package app is the federation-capable PUBLIC gateway runtime. It boots
// config-only, loads its `shn register` bundle (shnsdk.LoadBundle), resolves
// trust anchors + endpoints + the FHIR validator URL from /discovery, populates
// the peer Registry from the registrar /holders feed (the federation core),
// requires a FHIR system of record (FHIR_DATA_URL; a native-forward payer may keep none)
// with an in-memory Store default, and defaults the validator to the
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
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	checks "github.com/SmartHealthNetwork/shn-gateway/checks"
	fhirsor "github.com/SmartHealthNetwork/shn-gateway/connectors/fhirsor"
	pgstore "github.com/SmartHealthNetwork/shn-gateway/connectors/pgstore"
	smartauth "github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
	"github.com/SmartHealthNetwork/shn-gateway/diagnostics"
	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
	"github.com/SmartHealthNetwork/shn-gateway/internal/auditappend"
	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
	observer "github.com/SmartHealthNetwork/shn-gateway/observer"
	"github.com/SmartHealthNetwork/shn-gateway/pa/cdexroute"
	"github.com/SmartHealthNetwork/shn-gateway/scenarioauth"
	"github.com/SmartHealthNetwork/shn-gateway/telemetry"
	"github.com/SmartHealthNetwork/shn-gateway/telemetry/otelsetup"
	"github.com/SmartHealthNetwork/shn-gateway/telemetry/stats"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	health "github.com/SmartHealthNetwork/shn-sdk/health"
	metrics "github.com/SmartHealthNetwork/shn-sdk/metrics"
)

// config is the collapsed PUBLIC config surface. Required:
// SHN_DISCOVERY_URL (the single anchor), ROLE, SHN_SECRETS (the bundle dir), and
// FHIR_DATA_URL (the holder's own FHIR system of record — every role but a
// native-forward payer, which may keep none; see build()).
// A payer additionally requires its content occupant (PAYER_DAVINCI_BASE_URL, or an
// injected Config.Responder). Everything else is discovery-resolved or an optional
// override. The seed /
// SHN_MANIFEST path of the substrate cmd/gateway is intentionally dropped — the
// public binary is partner-path only.
type config struct {
	diagnostic *diagnosticSource
	// certs records each outbound endpoint's certificate expiry by kind
	// (MON-R25); every outbound client takes its transport from it. Set by
	// build(); nil (tests, loadConfig alone) ⇒ clients are left as they were.
	certs *endpointcert.Recorder
	// tokenNotes receives the outbound token clients' departure notes
	// (smartauth.Config.Observer: a partner authorization server read as sent
	// but out of specification). Set by build() to the operator-visible
	// "gateway: ..." stdout line; nil (tests, loadConfig alone) ⇒ silent.
	tokenNotes func(note string)

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
	RegistrarURL    string
	FHIRValidateURL string
	// FHIRValidateURL21 / FHIRValidateURL22 are the per-LINE $validate lanes
	// (FHIR_VALIDATE_URL_2_1 / FHIR_VALIDATE_URL_2_2).
	// A HAPI instance can host exactly ONE version of an IG, so a deployment that
	// DECLARES a 2.1 or 2.2 contract line must run a validator for that line;
	// FHIR_VALIDATE_URL remains the canonical (2.0) lane. Boot fails closed when a
	// declared non-canonical line has no lane (FR-36/FR-G29), at every level but
	// none — see validatorLanesForDeclared.
	FHIRValidateURL21 string
	FHIRValidateURL22 string
	// FHIRCertifyURL21 / FHIRCertifyURL22 (FHIR_CERTIFY_URL_2_1 / _2_2) are
	// $validate addresses used ONLY by the certification evidence for that line.
	// They are never routing lanes: a certify-only address does not enter
	// ValidatorsByContractLine, so it neither honours an inbound frame at that line nor
	// lets origination target it. For evidence the precedence is
	// FHIR_CERTIFY_URL_<line>, then FHIR_VALIDATE_URL_<line>, then the default
	// lane once routing has qualified it (certificationValidators).
	FHIRCertifyURL21 string
	FHIRCertifyURL22 string
	// DefaultValidatorLanes (FHIR_DEFAULT_VALIDATOR_LANES) is "" or "none".
	// "none" states this gateway's network has no Compose default validator
	// services: it creates no default lane, so it never probes their names,
	// and a line without FHIR_VALIDATE_URL_<line> / FHIR_CERTIFY_URL_<line> is
	// simply unconfigured. Unset keeps the default lanes.
	DefaultValidatorLanes string
	// laneRetry is how default lanes retry their qualification (nil: the
	// default schedule, lanes.go); never read from the environment.
	laneRetry *laneRetry
	// ContractVersions is the operator-DECLARED exchange-contract token set
	// (SHN_CONTRACT_VERSIONS, comma-separated). Boot-validated: token grammar +
	// membership of engine.NativeContractVersions(). Empty env ⇒ this build's
	// default declaration. Single-sourced (D1a): it drives leg selection, the
	// published CapabilityStatements / davinci-configuration, AND the registry
	// declaration peers select this holder against.
	ContractVersions []string
	// ContractVersionsDerived: SHN_CONTRACT_VERSIONS was unset on a payer that
	// forwards to its own system, so ContractVersions was derived from
	// PAYER_DAVINCI_CONTRACT_VERSIONS (engine.DeriveDeclaredContractVersions).
	// Boot refusals about the declared set then name where it came from.
	ContractVersionsDerived bool
	NPI                     string

	// DemoEgressNativeLines (SHN_DEMO_EGRESS_NATIVE_LINES, kit-bridging demo
	// only) narrows engine.Config.EgressNativeLines (D1c): restricts arm (2)'s
	// native-reach view so transform chains can fire against skewed peers, the
	// same seam the cross-line pair test suite uses. Empty env ⇒ nil (unset —
	// the production default: full native reach). Boot-validated: every token
	// must be a line of engine.NativeContractVersions(). Loud by name and by
	// boot log — never set in any shipped deploy config.
	DemoEgressNativeLines []string

	// DemoEdgeCapture (SHN_DEMO_EDGE_CAPTURE, demonstration/inspection only)
	// turns on engine.Config.DemoEdgeCapture: the bounded pre-seal
	// edge-capture store that lets this gateway retrieve each transformed
	// leg's own before/after payload pair by correlation id. false (unset,
	// the production default) ⇒ no store is ever built. Ordinary loadConfig
	// bool idiom (`== "true"`) — never "1", which is the SHN_FAKE_VALIDATOR-
	// only spelling inside build(). Loud by name and by boot log — never set
	// in any shipped deploy config.
	DemoEdgeCapture bool

	// ConformanceEnforcement (CONFORMANCE_ENFORCEMENT) is this participant's
	// choice about which payload conformance checks its gateway runs and what
	// a defect does: "none" (no payload conformance check runs and nothing is
	// recorded), "observe" (every check runs, each defect is recorded as a
	// finding and the message is relayed as sent), "structural" (every check runs,
	// a message whose structure is broken is refused and every other defect is
	// recorded as at observe) or "strict" (a defect refuses the message). Absent means "none" (D116):
	// loadConfig remaps it, and that remap is the ONLY place in the tree where
	// a level that is not strict comes from an omission — engine.ConformanceEnforcement's zero
	// value is strict everywhere else. Network rules (authentication,
	// authority, consent, routing, replay, message integrity) and the check of
	// a payload this gateway itself translated between IG lines refuse at
	// every level.
	ConformanceEnforcement engine.ConformanceEnforcement
	// ConformanceEnforcementUnset is whether CONFORMANCE_ENFORCEMENT was absent,
	// so ConformanceEnforcement is none by default rather than by choice. It
	// changes nothing the gateway does; the boot line says so.
	ConformanceEnforcementUnset bool

	// ConformanceToleratePeers (CONFORMANCE_TOLERATE_PEERS) is the payers whose
	// answers this participant tolerates (D122): <system>|<value> payer
	// identities, comma-separated. A defect the FHIR profile check of a listed
	// payer's DTR or PAS answer would refuse is recorded instead; unset tolerates
	// no one.
	ConformanceToleratePeers []shnsdk.PayerIdentifier

	// AdvertisedCDSHooks (CDS_ADVERTISE_HOOKS) narrows the CDS Hooks services
	// the provider ingress advertises and dispatches to the hooks named, a
	// comma-separated list. Unset advertises every hook the network carries.
	// An interim, optional override for a lane whose payers carry no
	// order-dispatch leg; a value naming a hook the network does not carry
	// refuses to boot.
	AdvertisedCDSHooks []string

	// Trust-anchor key-fetch URL overrides (first-class operator config):
	// override the discovery-advertised key URL when the gateway runs in the same
	// network as the substrate. firstNonEmpty(env, discovery); discovery is the default.
	AuthzPubkeyURL     string
	HubTransportKeyURL string

	StoreDatabaseURL string // Postgres DSN for the durable pgstore Store (else the in-memory Store).

	// StoreMaxConns (SHN_STORE_MAX_CONNS, default 8) is the shared-state pool's
	// MaxConns. Sized explicitly rather than left to pgx's max(4, NumCPU): on a
	// 0.25-vCPU task that default floors at four connections for the four consumers
	// behind one DSN (business state, replay, ingress key, exchange), and AppendLeg
	// pins one for the length of its transaction — so modest ingress concurrency
	// queues replay checks behind it until the store context expires. 8 is twice that
	// floor and still leaves the fleet's burst ceiling (this value × the number of
	// gateways sharing the database) under a small instance's connection limit — size
	// it against that product, not against one task. Positive by boot validation. It
	// overrides any pool_max_conns carried in the DSN itself.
	StoreMaxConns int32

	// ExchangeTTL (EXCHANGE_TTL, default 168h) is how long an exchange correlation
	// record is retained on whichever exchange store is selected. Positive by boot
	// validation: a zero or negative TTL would expire every record the instant it
	// was written, silently breaking response correlation.
	ExchangeTTL time.Duration

	// FHIR connector block. FHIRDataURL is REQUIRED on every role but a native-forward payer
	// (there is no in-process persona stub to fall back on); the credential
	// fields below are optional.
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
	PayerDavinciCDSBaseURL string
	// PayerDavinciDTRBaseURL / PayerDavinciPASBaseURL are the per-operation FHIR bases
	// (PAYER_DAVINCI_DTR_BASE_URL / PAYER_DAVINCI_PAS_BASE_URL) for a partner that
	// serves DTR and PAS from different bases. Empty ⇒ that operation uses
	// PayerDavinciBaseURL (co-located default). Either set without the shared base is
	// a boot error: the fallback must always exist.
	PayerDavinciDTRBaseURL   string
	PayerDavinciPASBaseURL   string
	PayerDavinciTokenURL     string
	PayerDavinciClientID     string
	PayerDavinciClientKey    string
	PayerDavinciClientAlg    string
	PayerDavinciScope        string
	PayerDavinciClientKID    string
	PayerDavinciClientSecret string // value, not a path (unlike *ClientKey)
	// PayerDavinciCRDServiceID optionally names the partner's CDS service for the
	// order-select leg (hooks order-select and order-sign). Empty ⇒ each request goes to
	// the one service the partner's {CDS base}/cds-services listing offers for the
	// request's hook (FR-G26). Either way the named service must be listed for the
	// request's hook. PAYER_DAVINCI_CRD_SERVICE_ID.
	PayerDavinciCRDServiceID string
	// PayerDavinciDispatchServiceID optionally names the partner's CDS service for the
	// crd-order-dispatch leg, with the same rules. PAYER_DAVINCI_DISPATCH_SERVICE_ID.
	PayerDavinciDispatchServiceID string

	// PayerDavinciContractVersions is the operator-declared per-peer contract
	// token set ("<contract>@<line>", comma-separated) the payer FHIR-metadata
	// probe verifies published evidence against (checks.FailVersionDrift).
	// PAYER_DAVINCI_CONTRACT_VERSIONS. Requires
	// PayerDavinciBaseURL — there is nothing to verify against otherwise.
	PayerDavinciContractVersions []string

	// PayerDavinciPayorOwnRaw / PayerDavinciPayorBackendRaw are the raw
	// PAYER_DAVINCI_PAYOR_OWN / PAYER_DAVINCI_PAYOR_BACKEND env values ("system|value"),
	// parsed + validated by loadConfig into PayerDavinciPayorOwn/Backend below. Feeds
	// engine.WithPayorEdgeIdentity (gateway/engine/payoredge.go). All-or-nothing:
	// loadConfig refuses boot on a partial set; both empty ⇒ the payer-edge identity
	// mapping seam is off (verbatim relay, the prior behavior).
	PayerDavinciBackendHeadersRaw string
	PayerDavinciBackendHeaders    http.Header
	// PayerDavinciBackendCorrelationOff stops the leg's id being sent to the
	// payer's own system as X-Correlation-Id (PAYER_DAVINCI_BACKEND_CORRELATION=off),
	// for a system that validates that header its own way. Default: sent.
	PayerDavinciBackendCorrelationOff bool
	payerDavinciBackendCorrelationRaw string
	// PayerDavinciBackendTimeout bounds each call to the payer's own system
	// (PAYER_DAVINCI_BACKEND_TIMEOUT, default defaultPayerBackendTimeout): a
	// system slower than that is answered this gateway's framed 504 while the
	// requester's leg budget still has room to carry it.
	PayerDavinciBackendTimeout    time.Duration
	payerDavinciBackendTimeoutRaw string
	// PayerEligibilityURL is the payer's own coverage-eligibility endpoint, when
	// it declares one: the absolute URL its system takes a POST of a
	// CoverageEligibilityRequest on. The request is then carried there exactly
	// and the payer's answer relayed. Unset: the gateway answers eligibility
	// from the payer's records. Set by PAYER_ELIGIBILITY_URL (payer role only).
	PayerEligibilityURL         string
	PayerDavinciPayorOwnRaw     string
	PayerDavinciPayorBackendRaw string
	PayerDavinciPayorOwn        shnsdk.PayerIdentifier
	PayerDavinciPayorBackend    shnsdk.PayerIdentifier
	PayerDavinciPayorEdge       bool // true iff both env vars were set and parsed clean

	// OriginationProfile selects the per-UC origination lane: "demo" keeps the
	// self-contained demo order shape; "provider-data" originates every UC off the
	// provider's seeded SoR and drives real reference-payer verdicts. ORIGINATION_PROFILE.
	// Unset means module 1 is off: no origination, and no /scenario route. Any other
	// value refuses to boot (validOriginationProfile).
	OriginationProfile string

	// Optional native DTR population (provider-local). PROVIDER_DTR_NATIVE switches DTR
	// population from the in-house managed backend to forwarding the provider's own SDC
	// Questionnaire/$populate endpoint.
	ProviderDTRNative      bool
	ProviderDTRPopulateURL string
	// PROVIDER_DTR_POPULATE_* SMART Backend Services credential block for the
	// $populate endpoint — the same exactly-one-mode shape as PAYER_DAVINCI_* /
	// FHIR_* (private_key_jwt via CLIENT_KEY+CLIENT_ALG[+CLIENT_KID], or
	// client_secret_post via CLIENT_SECRET). Unset TOKEN_URL ⇒ unauthenticated
	// (warned at build where the populator is wired).
	ProviderDTRPopulateTokenURL     string
	ProviderDTRPopulateClientID     string
	ProviderDTRPopulateClientKey    string
	ProviderDTRPopulateClientAlg    string
	ProviderDTRPopulateScope        string
	ProviderDTRPopulateClientKID    string
	ProviderDTRPopulateClientSecret string // value, not a path (unlike *ClientKey)

	// PROVIDER_CDEX_TASK_URL is the provider's own Task endpoint, the full URL
	// a payer's attachment-request Task is carried to. Set, the provider takes
	// the payer's attachment request (cdex-attachment-request): it declares
	// it and serves it. PROVIDER_CDEX_* is the forward's SMART Backend
	// Services credential block, by the PAYER_DAVINCI_* pattern, required with
	// the endpoint (no Task is carried unauthenticated), its scope stated
	// (no default), and RS256 admitted beside ES384 and RS384 for this block
	// alone. Provider role only.
	ProviderCDexTaskURL      string
	ProviderCDexTokenURL     string
	ProviderCDexClientID     string
	ProviderCDexClientKey    string
	ProviderCDexClientAlg    string
	ProviderCDexScope        string
	ProviderCDexClientKID    string
	ProviderCDexClientSecret string // value, not a path (unlike *ClientKey)

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
	// Required when ProviderDavinciIngress or PayerDavinciIngress is set.
	ProviderDavinciIngressClientsFile string

	// PayerDavinciIngress mounts the payer's Da Vinci ingress for the CDex
	// attachment request on the payer role: a Task create at each provider's
	// base (POST /cdex/provider/{provider}/Task) and the bases' listing
	// (GET /cdex/providers), behind the same SMART Backend Services
	// token endpoint as the provider's ingress. Set by PAYER_DAVINCI_INGRESS
	// (any non-empty value enables). When enabled,
	// PAYER_DAVINCI_INGRESS_BASE_URL and INGRESS_CLIENTS_FILE are required, as
	// for the provider's (FR-G13 all-or-nothing).
	PayerDavinciIngress bool
	// PayerDavinciIngressBaseURL is the payer ingress's CONFIG-PINNED SMART aud
	// and the base every per-provider base is built under. Set by
	// PAYER_DAVINCI_INGRESS_BASE_URL. Required when PayerDavinciIngress is set.
	PayerDavinciIngressBaseURL string

	// RequireKnownMembers is the participant's opt-in to checking members: on the Da
	// Vinci CRD/DTR/PAS legs, a subject its system of record does not hold is refused.
	// By default (unset or "false") such a subject binds by member id plus the
	// demographics of the Patient the request carries (the id alone when it carries
	// none). Set by REQUIRE_KNOWN_MEMBERS ("true" or "false").
	RequireKnownMembers bool
	// RequireSharedLine is the participant's opt-in to refusing, as routing, a
	// pair that shares no declared IG line. By default (unset or "false") such
	// a pair's messages are carried as sent and recorded. Set by
	// REQUIRE_SHARED_LINE ("true" or "false").
	RequireSharedLine bool
	// TranslateIGLines is the participant's opt-in to translating between IG
	// lines (module 2): a message the gateway builds is carried to a peer's
	// line where a certified translation exists, and a questionnaire package
	// answer reaches its system under its own DTR line's names. By default
	// (unset or "false") nothing is translated. Set by TRANSLATE_IG_LINES
	// ("true" or "false").
	TranslateIGLines bool
	// AttachmentCeiling is the largest documentation attachment submission
	// body this gateway sends or accepts. Set by ATTACHMENT_CEILING_BYTES, a
	// positive integer no larger than the network's ceiling
	// (shnsdk.MaxAttachmentBytes); unset, it is the network's ceiling.
	AttachmentCeiling int64
	// AttachmentInflightBytes bounds the bytes of documentation attachments and
	// large envelopes held at once (ATTACHMENT_INFLIGHT_BYTES), from the task's
	// memory, at boot, when it is unset (attachmentInflightDefault).
	AttachmentInflightBytes int64
	// PayerAttachmentMaxBytes is the largest single inline attachment (file),
	// in bytes after base64 decoding, the payer's system accepts in one
	// documentation submission, as this gateway declares it on the network
	// (attachmentMaxBytes). nil when unset: the payer declares no limit, which
	// is unknown, never zero. Set by PAYER_ATTACHMENT_MAX_BYTES on a payer
	// gateway, a positive integer up to 2^53-1, declared as set: a value above
	// a sender's ceiling stays the payer's statement.
	PayerAttachmentMaxBytes *int64
	// EnrichNativeRequests is the participant's opt-in to having its gateway add
	// what a Da Vinci-native request leaves out, from its own system of record:
	// the CDS Hooks prefetch fill (E-02) and the questionnaire-package Coverage
	// (E-04) and Patient (E-05) appends. By default (unset or "false") a native
	// request is carried as sent. Set by ENRICH_NATIVE_REQUESTS ("true" or
	// "false").
	EnrichNativeRequests bool
	// FHIRServerRead is how the gateway reads a CRD request's Coverage through
	// the request's own fhirServer, only to route, when the request carries
	// none and the system of record does not hold the member: "private" (the default, unset too: a
	// server in the participant's own network or on the internet, never a
	// loopback, link-local or metadata address), "public" (only a public
	// server on 443, for a gateway whose network is not its participant's) or
	// "off" (never). Set by CDS_FHIR_SERVER_READ; a value other than off needs
	// PROVIDER_DAVINCI_INGRESS.
	FHIRServerRead string
	// ignoredSettingLines holds a boot warning for each of ignoredSettings still
	// set: nothing reads them any more, so they only warn. It also holds one
	// for each of noEffectSettings set: a dependency reads them, and the
	// gateway makes them do nothing.
	ignoredSettingLines []string

	// IngressBaseURL and IngressClients are resolved from ProviderDavinciIngressBaseURL
	// + IngressClientsFile by loadConfig and passed directly into engine.Config.
	IngressBaseURL string
	IngressClients map[string]engine.IngressClientRegistration

	// ScenarioCallersFile names the callers a provider gateway's /scenario
	// routes admit: a JSON array of {label, public_key}, the key a
	// base64 Ed25519 public key. Set by SCENARIO_CALLERS_FILE. A malformed file
	// refuses boot; an empty array admits no caller. Unset leaves the routes
	// unauthenticated, as they were: a temporary default, until every stack
	// sets it, that becomes refuse. ScenarioCallers is what loadConfig reads
	// from it.
	ScenarioCallersFile string
	ScenarioCallers     map[string]ed25519.PublicKey

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

// validOriginationProfile reports whether v is one of the ORIGINATION_PROFILE values
// the engine accepts (engine.OriginationProfiles, the origination lanes). Unset is not
// one: it is module 1 off.
func validOriginationProfile(v string) bool {
	return slices.Contains(engine.OriginationProfiles(), v)
}

// contractVersionTokenRe mirrors the registrar admission grammar
// (the platform registrar's contractVersionRe) — the gateway cannot import
// the registrar, so the pattern is pinned here verbatim.
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

// parsePayerIdentifier parses a "system|value" env value into a shnsdk.PayerIdentifier
// (PAYER_DAVINCI_PAYOR_OWN / PAYER_DAVINCI_PAYOR_BACKEND) — fail-closed on anything
// else (no "|", or an empty system/value either side of it).
func parsePayerIdentifier(envName, raw string) (shnsdk.PayerIdentifier, error) {
	system, value, ok := strings.Cut(raw, "|")
	if !ok || system == "" || value == "" {
		return shnsdk.PayerIdentifier{}, fmt.Errorf("gateway: %s must be \"system|value\" (both non-empty), got %q", envName, raw)
	}
	return shnsdk.PayerIdentifier{System: system, Value: value}, nil
}

// providerCDexSettings are the PROVIDER_CDEX_* settings, each with whether
// it is set.
func providerCDexSettings(cfg config) []struct {
	key string
	set bool
} {
	return []struct {
		key string
		set bool
	}{
		{"PROVIDER_CDEX_TASK_URL", cfg.ProviderCDexTaskURL != ""},
		{"PROVIDER_CDEX_TOKEN_URL", cfg.ProviderCDexTokenURL != ""},
		{"PROVIDER_CDEX_CLIENT_ID", cfg.ProviderCDexClientID != ""},
		{"PROVIDER_CDEX_CLIENT_KEY", cfg.ProviderCDexClientKey != ""},
		{"PROVIDER_CDEX_CLIENT_ALG", cfg.ProviderCDexClientAlg != ""},
		{"PROVIDER_CDEX_SCOPE", cfg.ProviderCDexScope != ""},
		{"PROVIDER_CDEX_CLIENT_KID", cfg.ProviderCDexClientKID != ""},
		{"PROVIDER_CDEX_CLIENT_SECRET", cfg.ProviderCDexClientSecret != ""},
	}
}

// checkProviderCDex is the provider's own Task endpoint's admission: a
// provider's setting, required with its whole credential block and with the
// Da Vinci ingress whose base the Task's documentation return path is built
// under (E-11); a credential without the endpoint is a partial block.
func checkProviderCDex(role string, cfg config) error {
	var set []string
	for _, s := range providerCDexSettings(cfg) {
		if s.set {
			set = append(set, s.key)
		}
	}
	if len(set) == 0 {
		return nil
	}
	if role != "provider" {
		return fmt.Errorf("gateway: %s is provider-only (role=%q): only a provider takes the payer's attachment request", strings.Join(set, ", "), role)
	}
	if cfg.ProviderCDexTaskURL == "" {
		return fmt.Errorf("gateway: %s set requires PROVIDER_CDEX_TASK_URL, the provider's own Task endpoint", strings.Join(set, ", "))
	}
	if !cfg.ProviderDavinciIngress || cfg.ProviderDavinciIngressBaseURL == "" {
		return fmt.Errorf("gateway: PROVIDER_CDEX_TASK_URL requires PROVIDER_DAVINCI_INGRESS and PROVIDER_DAVINCI_INGRESS_BASE_URL: the Task's payer-url is re-keyed to the documentation return path under that base")
	}
	for _, req := range []struct{ key, val string }{
		{"PROVIDER_CDEX_TOKEN_URL", cfg.ProviderCDexTokenURL},
		{"PROVIDER_CDEX_CLIENT_ID", cfg.ProviderCDexClientID},
		{"PROVIDER_CDEX_SCOPE", cfg.ProviderCDexScope},
	} {
		if strings.TrimSpace(req.val) == "" {
			return fmt.Errorf("gateway: PROVIDER_CDEX_TASK_URL requires %s: the Task is forwarded to the provider's own system only with its own credentials", req.key)
		}
	}
	return checkClientAuthModeAlgs("PROVIDER_CDEX", cfg.ProviderCDexClientKey, cfg.ProviderCDexClientAlg, cfg.ProviderCDexClientKID, cfg.ProviderCDexClientSecret, providerCDexAlgs)
}

// providerCDexAlgs are the private_key_jwt algs the provider's Task forward
// signs with: SMART Backend Services' two, and RS256, the one the Da Vinci
// reference provider's authorization server verifies.
var providerCDexAlgs = []string{"ES384", "RS384", "RS256"}

// smartAlgs are the private_key_jwt algs every other credential block signs
// with (SMART Backend Services').
var smartAlgs = []string{"ES384", "RS384"}

// checkClientAuthMode enforces exactly one outbound client-auth mode per
// credential block: private_key_jwt (KEY+ALG, preferred) or client_secret_post
// (SECRET). prefix is the env family: "FHIR", "PAYER_DAVINCI" or
// "PROVIDER_DTR_POPULATE".
func checkClientAuthMode(prefix, key, alg, kid, secret string) error {
	return checkClientAuthModeAlgs(prefix, key, alg, kid, secret, smartAlgs)
}

// checkClientAuthModeAlgs is checkClientAuthMode with the block's own algs.
func checkClientAuthModeAlgs(prefix, key, alg, kid, secret string, algs []string) error {
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
	case !slices.Contains(algs, alg):
		return fmt.Errorf("gateway: %s_CLIENT_ALG must be %s, got %q", prefix, strings.Join(algs, "|"), alg)
	}
	return nil
}

// reportCDSServices logs which hook each configured CDS service answers, and
// warns about a configured service the payer does not list.
func reportCDSServices(stdout io.Writer, services []engine.CDSService, cfg config) {
	for _, configured := range []struct{ key, id string }{
		{"PAYER_DAVINCI_CRD_SERVICE_ID", cfg.PayerDavinciCRDServiceID},
		{"PAYER_DAVINCI_DISPATCH_SERVICE_ID", cfg.PayerDavinciDispatchServiceID},
	} {
		if configured.id == "" {
			continue
		}
		hook := ""
		for _, s := range services {
			if s.ID == configured.id {
				hook = s.Hook
				break
			}
		}
		if hook == "" {
			fmt.Fprintf(stdout, "gateway: WARNING %s=%s is not in the payer's CDS service listing; its CRD requests are refused\n", configured.key, configured.id)
			continue
		}
		fmt.Fprintf(stdout, "gateway: %s=%s is the payer's CDS service for hook %s; requests with another hook are refused\n", configured.key, configured.id, hook)
	}
}

// removedSettings are settings this gateway no longer reads. A deployment that
// still sets one does not start, so the change in behavior is never silent.
var removedSettings = []struct{ key, replacedBy string }{
	{"PAYER_DAVINCI_CRD_COVERAGE_BUNDLE", "the payer receives the CDS Hooks request as the provider's system sent it; a coverage prefetch template that is a search is answered with a searchset Bundle"},
	{"PAYER_DAVINCI_CRD_HOOK", "the request's hook is never changed; the payer's CDS service is chosen by the request's hook (PAYER_DAVINCI_CRD_SERVICE_ID still names one)"},
	{"PAYER_DAVINCI_DISPATCH_HOOK", "the request's hook is never changed; the payer's CDS service is chosen by the request's hook (PAYER_DAVINCI_DISPATCH_SERVICE_ID still names one)"},
}

// ignoredSettings are settings this gateway no longer reads, each with the
// release from which it changed nothing and what it served. Unlike removedSettings, a
// deployment that still sets one starts as before (the setting switched nothing
// it relays or refuses) and logs one warning per setting, so the leftover is
// never silent either.
var ignoredSettings = []struct{ key, release, served string }{
	{"PATIENT_ACCESS_API", "v0.63.0", "the Patient Access API"},
	{"AUDIT_URL", "v0.63.0", "the Patient Access API's audit record"},
	{"PHG_URL", "v0.63.0", "the provider gateway's read of the PHG's denial view"},
	{"PAYER_DAVINCI_PAS_NATIVE", "v0.39.0", "its switch: PAS submit and update always forward to the payer's own system"},
	{"PAYER_DAVINCI_STRICT_EXTENSIONS", "v1.0.0", "the strict-extensions overlay it reserved, which switched nothing"},
	{"SHN_ACCEPT_UNKNOWN_MEMBERS", "v1.0.0", "the alias of carrying members the system of record does not hold, which is the default (REQUIRE_KNOWN_MEMBERS=true opts in to refusing them)"},
}

// noEffectSettings are settings a dependency the gateway links reads for
// itself, and the gateway makes a no-op, each with the dependency and how. A
// deployment that sets one starts as before and logs one warning, so the
// setting never seems to work when it does nothing.
var noEffectSettings = []struct{ key, reader, why string }{
	{"OTEL_GO_X_DEPRECATED_RUNTIME_METRICS", "the OpenTelemetry Go runtime instrumentation",
		"the deprecated process.runtime.go.* and runtime.uptime metrics it would add are dropped from every export"},
}

// noEffectSettingLines returns the boot warning for each of noEffectSettings
// that getenv sets, in table order.
func noEffectSettingLines(getenv func(string) string) []string {
	var lines []string
	for _, s := range noEffectSettings {
		if getenv(s.key) != "" {
			lines = append(lines, fmt.Sprintf("gateway: WARNING: %s has no effect: %s reads it, and %s", s.key, s.reader, s.why))
		}
	}
	return lines
}

// ignoredSettingLines returns the boot warning for each of ignoredSettings that
// getenv sets, in ignoredSettings order.
func ignoredSettingLines(getenv func(string) string) []string {
	var lines []string
	for _, ignored := range ignoredSettings {
		if getenv(ignored.key) != "" {
			lines = append(lines, fmt.Sprintf("gateway: WARNING: %s is set but no longer read: shn-gateway %s removed %s", ignored.key, ignored.release, ignored.served))
		}
	}
	return lines
}

// errNoSystemOfRecord refuses a gateway with no FHIR_DATA_URL that must read
// its members from one: every role but a payer that forwards natively.
var errNoSystemOfRecord = fmt.Errorf("gateway: FHIR_DATA_URL is required — the gateway reads its members, coverage and " +
	"clinical facts from the holder's own FHIR system of record (the in-process persona stub is gone). " +
	"Point it at your US Core server (e.g. http://hapi:8080/fhir), or at your tenant partition. " +
	"Only a payer that forwards to its own system (PAYER_DAVINCI_BASE_URL) may run without one")

// payerNative reports whether the gateway is a payer whose own system answers
// every exchange behind PAYER_DAVINCI_BASE_URL.
func payerNative(cfg config) bool { return cfg.Role == "payer" && cfg.PayerDavinciBaseURL != "" }

// declaresAny reports whether a SHN_CONTRACT_VERSIONS value names any token;
// one that names none means the build default (engine.ParseDeclaredContractVersions).
func declaresAny(csv string) bool {
	for _, tok := range strings.Split(csv, ",") {
		if strings.TrimSpace(tok) != "" {
			return true
		}
	}
	return false
}

// loadConfig reads the gateway's public configuration from getenv: the URL
// checks, the ROLE/PORT bounds and the FHIR/SMART credential guards. The
// public binary requires SHN_DISCOVERY_URL; it has no seed-manifest path.
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

	for _, removed := range removedSettings {
		if getenv(removed.key) != "" {
			return config{}, fmt.Errorf("gateway: %s was removed and must be unset: %s", removed.key, removed.replacedBy)
		}
	}
	// Before any level or lane is read: a released build refuses the validator test
	// double whatever else is configured, a validator URL and level none included
	// (testdouble.go).
	if getenv("SHN_FAKE_VALIDATOR") != "" && !testDoubleAvailable() {
		return config{}, errors.New(testDoubleRefusal)
	}

	secretsDir := getenv("SHN_SECRETS")
	if secretsDir == "" {
		return config{}, fmt.Errorf("gateway: SHN_SECRETS (the shn register / Init bundle dir) is required")
	}
	discoveryURL := getenv("SHN_DISCOVERY_URL")
	if discoveryURL == "" {
		return config{}, fmt.Errorf("gateway: SHN_DISCOVERY_URL is required (the single anchor that resolves substrate endpoints + trust anchors)")
	}

	cfg := config{
		Role:                  role,
		Addr:                  host + ":" + port,
		SecretsDir:            secretsDir,
		DiscoveryURL:          discoveryURL,
		AuthzURL:              getenv("AUTHZ_URL"),
		HubURL:                getenv("HUB_URL"),
		ConsentURL:            getenv("CONSENT_URL"),
		RegistrarURL:          getenv("REGISTRAR_URL"),
		FHIRValidateURL:       getenv("FHIR_VALIDATE_URL"),
		FHIRValidateURL21:     getenv("FHIR_VALIDATE_URL_2_1"),
		FHIRValidateURL22:     getenv("FHIR_VALIDATE_URL_2_2"),
		FHIRCertifyURL21:      getenv("FHIR_CERTIFY_URL_2_1"),
		FHIRCertifyURL22:      getenv("FHIR_CERTIFY_URL_2_2"),
		DefaultValidatorLanes: getenv("FHIR_DEFAULT_VALIDATOR_LANES"),
		StoreDatabaseURL:      getenv("SHN_STORE_DATABASE_URL"),
		NPI:                   getenv("NPI"),
		FHIRDataURL:           getenv("FHIR_DATA_URL"),
		FHIRTokenURL:          getenv("FHIR_TOKEN_URL"),
		FHIRClientID:          getenv("FHIR_CLIENT_ID"),
		FHIRClientKey:         getenv("FHIR_CLIENT_KEY"),
		FHIRClientAlg:         getenv("FHIR_CLIENT_ALG"),
		FHIRClientScope:       def("FHIR_CLIENT_SCOPE", "system/*.read"),
		FHIRClientKID:         getenv("FHIR_CLIENT_KID"),
		FHIRClientSecret:      getenv("FHIR_CLIENT_SECRET"),

		PayerDavinciBaseURL:               getenv("PAYER_DAVINCI_BASE_URL"),
		PayerDavinciCDSBaseURL:            getenv("PAYER_DAVINCI_CDS_BASE_URL"),
		PayerDavinciDTRBaseURL:            getenv("PAYER_DAVINCI_DTR_BASE_URL"),
		PayerDavinciPASBaseURL:            getenv("PAYER_DAVINCI_PAS_BASE_URL"),
		PayerDavinciTokenURL:              getenv("PAYER_DAVINCI_TOKEN_URL"),
		PayerDavinciClientID:              getenv("PAYER_DAVINCI_CLIENT_ID"),
		PayerDavinciClientKey:             getenv("PAYER_DAVINCI_CLIENT_KEY"),
		PayerDavinciClientAlg:             getenv("PAYER_DAVINCI_CLIENT_ALG"),
		PayerDavinciScope:                 def("PAYER_DAVINCI_SCOPE", "system/*.read"),
		PayerDavinciClientKID:             getenv("PAYER_DAVINCI_CLIENT_KID"),
		PayerDavinciClientSecret:          getenv("PAYER_DAVINCI_CLIENT_SECRET"),
		PayerDavinciCRDServiceID:          getenv("PAYER_DAVINCI_CRD_SERVICE_ID"),
		PayerDavinciDispatchServiceID:     getenv("PAYER_DAVINCI_DISPATCH_SERVICE_ID"),
		PayerDavinciContractVersions:      splitTrimmed(getenv("PAYER_DAVINCI_CONTRACT_VERSIONS")),
		PayerDavinciBackendHeadersRaw:     getenv("PAYER_DAVINCI_BACKEND_HEADERS"),
		payerDavinciBackendCorrelationRaw: getenv("PAYER_DAVINCI_BACKEND_CORRELATION"),
		payerDavinciBackendTimeoutRaw:     getenv("PAYER_DAVINCI_BACKEND_TIMEOUT"),
		PayerEligibilityURL:               getenv("PAYER_ELIGIBILITY_URL"),
		PayerDavinciPayorOwnRaw:           getenv("PAYER_DAVINCI_PAYOR_OWN"),
		PayerDavinciPayorBackendRaw:       getenv("PAYER_DAVINCI_PAYOR_BACKEND"),
		OriginationProfile:                getenv("ORIGINATION_PROFILE"),

		ProviderDTRNative:      getenv("PROVIDER_DTR_NATIVE") == "true",
		ProviderDTRPopulateURL: getenv("PROVIDER_DTR_POPULATE_URL"),

		ProviderDTRPopulateTokenURL:     getenv("PROVIDER_DTR_POPULATE_TOKEN_URL"),
		ProviderDTRPopulateClientID:     getenv("PROVIDER_DTR_POPULATE_CLIENT_ID"),
		ProviderDTRPopulateClientKey:    getenv("PROVIDER_DTR_POPULATE_CLIENT_KEY"),
		ProviderDTRPopulateClientAlg:    getenv("PROVIDER_DTR_POPULATE_CLIENT_ALG"),
		ProviderDTRPopulateScope:        def("PROVIDER_DTR_POPULATE_SCOPE", "system/*.read"),
		ProviderDTRPopulateClientKID:    getenv("PROVIDER_DTR_POPULATE_CLIENT_KID"),
		ProviderDTRPopulateClientSecret: getenv("PROVIDER_DTR_POPULATE_CLIENT_SECRET"),

		ProviderCDexTaskURL:      strings.TrimSpace(getenv("PROVIDER_CDEX_TASK_URL")),
		ProviderCDexTokenURL:     getenv("PROVIDER_CDEX_TOKEN_URL"),
		ProviderCDexClientID:     getenv("PROVIDER_CDEX_CLIENT_ID"),
		ProviderCDexClientKey:    getenv("PROVIDER_CDEX_CLIENT_KEY"),
		ProviderCDexClientAlg:    getenv("PROVIDER_CDEX_CLIENT_ALG"),
		ProviderCDexScope:        getenv("PROVIDER_CDEX_SCOPE"),
		ProviderCDexClientKID:    getenv("PROVIDER_CDEX_CLIENT_KID"),
		ProviderCDexClientSecret: getenv("PROVIDER_CDEX_CLIENT_SECRET"),

		ProviderDavinciIngress: getenv("PROVIDER_DAVINCI_INGRESS") != "",

		ProviderDavinciIngressBaseURL:     getenv("PROVIDER_DAVINCI_INGRESS_BASE_URL"),
		ProviderDavinciIngressClientsFile: getenv("INGRESS_CLIENTS_FILE"),
		PayerDavinciIngress:               getenv("PAYER_DAVINCI_INGRESS") != "",
		PayerDavinciIngressBaseURL:        getenv("PAYER_DAVINCI_INGRESS_BASE_URL"),
		ScenarioCallersFile:               getenv("SCENARIO_CALLERS_FILE"),

		ignoredSettingLines: append(ignoredSettingLines(getenv), noEffectSettingLines(getenv)...),

		AuthzPubkeyURL:     getenv("AUTHZ_PUBKEY_URL"),
		HubTransportKeyURL: getenv("HUB_TRANSPORT_KEY_URL"),
	}

	// ORIGINATION_PROFILE is the participant's declaration that its system cannot
	// speak Da Vinci itself, so this gateway runs module 1 for it. Unset, module 1
	// is off: the gateway serves its ingress and relays, and mounts no /scenario
	// route.
	// Any other value is refused on every role. The engine's origination predicates
	// compare the value exactly, so an unrecognised one (a retired lane, a typo, another
	// case, stray whitespace) matched no lane and built its PAS request with a payer
	// Organization the payer's own system never supplied, rather than failing.
	if cfg.OriginationProfile != "" && !validOriginationProfile(cfg.OriginationProfile) {
		return config{}, fmt.Errorf("gateway: invalid ORIGINATION_PROFILE %q (must be %s, or unset)", cfg.OriginationProfile, strings.Join(engine.OriginationProfiles(), "|"))
	}
	// NPI is optional: it names the ordering clinician only where the
	// participant's own records name none. A value that is not an NPI would be
	// stated on the participant's behalf as one, so it refuses boot.
	if cfg.NPI != "" && !engine.ValidNPI(cfg.NPI) {
		return config{}, fmt.Errorf("gateway: NPI must be a ten-digit NPI with a valid check digit, got %q", cfg.NPI)
	}

	for _, pair := range optionalURLs(cfg) {
		if err := checkOptionalURL(pair[0], pair[1]); err != nil {
			return config{}, fmt.Errorf("gateway: %w", err)
		}
	}
	if cfg.DefaultValidatorLanes != "" && cfg.DefaultValidatorLanes != defaultValidatorLanesNone {
		return config{}, fmt.Errorf("gateway: FHIR_DEFAULT_VALIDATOR_LANES=%q: the only value is %q (unset keeps the default lanes)", cfg.DefaultValidatorLanes, defaultValidatorLanesNone)
	}
	// A certification address is dialed like a lane: a hostless value refuses
	// boot here, not per exchange behind a hashed error in the evidence.
	for _, pair := range [][2]string{{"FHIR_CERTIFY_URL_2_1", cfg.FHIRCertifyURL21}, {"FHIR_CERTIFY_URL_2_2", cfg.FHIRCertifyURL22}} {
		if pair[1] == "" {
			continue
		}
		if err := checkValidatorLaneURL(pair[0], pair[1]); err != nil {
			return config{}, fmt.Errorf("gateway: %w", err)
		}
	}

	cfg.ChecksToken = getenv("CHECKS_TOKEN")

	// D1a: the operator-declared contract set, boot-validated ONCE here (grammar +
	// membership of NativeContractVersions) so a typo'd or unbuildable token is a
	// refusal to start, never a peer-visible declaration this build cannot honor.
	declared, derr := engine.ParseDeclaredContractVersions(getenv("SHN_CONTRACT_VERSIONS"))
	if derr != nil {
		return config{}, fmt.Errorf("gateway: SHN_CONTRACT_VERSIONS: %w", derr)
	}
	cfg.ContractVersions = declared

	// Demo-only egress narrowing (kit bridging demo): restricts arm (2)'s
	// native-reach view so transform chains can fire against skewed peers.
	// Loud by name and by log — never set in any shipped deploy config.
	if raw := strings.TrimSpace(getenv("SHN_DEMO_EGRESS_NATIVE_LINES")); raw != "" {
		native := map[string]bool{}
		for _, tok := range engine.NativeContractVersions() {
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

	// Demo-only edge capture (bounded pre-seal payload inspection): the
	// ordinary loadConfig bool idiom, matching every other config bool.
	cfg.DemoEdgeCapture = getenv("SHN_DEMO_EDGE_CAPTURE") == "true"

	// THE default: an absent CONFORMANCE_ENFORCEMENT means none (D116). Payload
	// conformance is a participant's opt-in, never the network's default: a
	// gateway that states no level runs no payload check and records no
	// finding, exactly as at none (one code path), and needs no validator. A
	// participant opts in to recording (observe) or to refusal (structural for
	// structure, strict for every defect). The network rules and module 2's
	// certification of a payload this gateway translated refuse at every level,
	// this one included. This is the ONLY place it happens. Everything else in the tree is strict by
	// engine.ConformanceEnforcement's zero value, and every gate pins its
	// level explicitly. A DEPLOYED gateway states its level explicitly too,
	// but not always to strict: a lane a partner's bytes can reach runs
	// observe, so the partner sees their defect recorded rather than meeting a
	// refusal produced by our configuration. Which lanes those are, and the evidence
	// for each, is in the platform invariant suite's TestInvariant_EveryGateRunsStrict.
	if raw := getenv("CONFORMANCE_ENFORCEMENT"); raw != "" {
		level, err := engine.ParseConformanceEnforcement(raw)
		if err != nil {
			return config{}, fmt.Errorf("gateway: %w", err)
		}
		cfg.ConformanceEnforcement = level
	} else {
		cfg.ConformanceEnforcement = engine.EnforcementNone
		cfg.ConformanceEnforcementUnset = true
	}
	peers, err := engine.ParseToleratePeers(getenv("CONFORMANCE_TOLERATE_PEERS"))
	if err != nil {
		return config{}, fmt.Errorf("gateway: %w", err)
	}
	cfg.ConformanceToleratePeers = peers

	// Members the system of record does not hold are carried unless the
	// participant opts in to checking them. Only "true" and "false" are values.
	switch raw := getenv("REQUIRE_KNOWN_MEMBERS"); raw {
	case "", "false":
	case "true":
		cfg.RequireKnownMembers = true
	default:
		return config{}, fmt.Errorf("gateway: REQUIRE_KNOWN_MEMBERS must be true or false, got %q", raw)
	}
	// A payer that keeps no system of record (build refuses one on any
	// other gateway) has no members to know: knowing them needs a store.
	if cfg.FHIRDataURL == "" && payerNative(cfg) && cfg.RequireKnownMembers {
		return config{}, fmt.Errorf("gateway: REQUIRE_KNOWN_MEMBERS=true needs FHIR_DATA_URL: with no system of record there are no members to know")
	}

	// A pair that shares no declared IG line is carried unless the participant
	// opts in to refusing it. Only "true" and "false" are values.
	switch raw := getenv("REQUIRE_SHARED_LINE"); raw {
	case "", "false":
	case "true":
		cfg.RequireSharedLine = true
	default:
		return config{}, fmt.Errorf("gateway: REQUIRE_SHARED_LINE must be true or false, got %q", raw)
	}

	// Nothing is translated between IG lines unless the participant opts in.
	// Only "true" and "false" are values.
	switch raw := getenv("TRANSLATE_IG_LINES"); raw {
	case "", "false":
	case "true":
		cfg.TranslateIGLines = true
	default:
		return config{}, fmt.Errorf("gateway: TRANSLATE_IG_LINES must be true or false, got %q", raw)
	}

	// A documentation attachment submission is bounded by the network's
	// ceiling, the bound the strict JSON scan reads a body to. A participant
	// may set a lower one; a higher one refuses to boot.
	cfg.AttachmentCeiling = shnsdk.MaxAttachmentBytes
	if raw := getenv("ATTACHMENT_CEILING_BYTES"); raw != "" {
		n, perr := strconv.ParseInt(raw, 10, 64)
		if perr != nil || n < 1 || n > shnsdk.MaxAttachmentBytes {
			return config{}, fmt.Errorf("gateway: ATTACHMENT_CEILING_BYTES must be an integer in [1, %d] (the network's ceiling), got %q", shnsdk.MaxAttachmentBytes, raw)
		}
		cfg.AttachmentCeiling = n
	}

	// A full submission always fits the budget for large bodies, so a value
	// under the ceiling refuses to boot.
	if raw := getenv("ATTACHMENT_INFLIGHT_BYTES"); raw != "" {
		n, perr := strconv.ParseInt(raw, 10, 64)
		if perr != nil || n < cfg.AttachmentCeiling {
			return config{}, fmt.Errorf("gateway: ATTACHMENT_INFLIGHT_BYTES must be an integer of at least %d (the attachment ceiling), got %q", cfg.AttachmentCeiling, raw)
		}
		cfg.AttachmentInflightBytes = n
	}

	// A payer declares the largest attachment its system accepts only when it
	// sets one: unset is no declaration (unknown), never zero.
	if raw := getenv("PAYER_ATTACHMENT_MAX_BYTES"); raw != "" {
		if cfg.Role != "payer" {
			return config{}, fmt.Errorf("gateway: PAYER_ATTACHMENT_MAX_BYTES is a payer gateway setting; this gateway's ROLE is %q", cfg.Role)
		}
		n, perr := strconv.ParseInt(raw, 10, 64)
		if perr != nil || n < 1 || n > maxDeclaredAttachmentBytes {
			return config{}, fmt.Errorf("gateway: PAYER_ATTACHMENT_MAX_BYTES must be an integer in [1, %d], got %q", int64(maxDeclaredAttachmentBytes), raw)
		}
		cfg.PayerAttachmentMaxBytes = &n
	}

	// A Da Vinci-native request is carried as sent unless the participant opts
	// in to enrichment. Only "true" and "false" are values.
	switch raw := getenv("ENRICH_NATIVE_REQUESTS"); raw {
	case "", "false":
	case "true":
		cfg.EnrichNativeRequests = true
	default:
		return config{}, fmt.Errorf("gateway: ENRICH_NATIVE_REQUESTS must be true or false, got %q", raw)
	}

	// A CRD request's Coverage is read through its own fhirServer unless the
	// participant turns the read off. Only the listed values are values.
	if raw := getenv("CDS_FHIR_SERVER_READ"); raw != "" {
		if !engine.ValidFHIRServerRead(raw) {
			return config{}, fmt.Errorf("gateway: CDS_FHIR_SERVER_READ must be private, public or off, got %q", raw)
		}
		if raw != engine.FHIRServerReadOff && !cfg.ProviderDavinciIngress {
			return config{}, fmt.Errorf("gateway: CDS_FHIR_SERVER_READ=%s needs PROVIDER_DAVINCI_INGRESS: the read serves only the provider's CRD ingress", raw)
		}
		cfg.FHIRServerRead = raw
	}

	if raw := getenv("CDS_ADVERTISE_HOOKS"); raw != "" {
		hooks, err := engine.ParseAdvertisedCDSHooks(raw)
		if err != nil {
			return config{}, fmt.Errorf("gateway: %w", err)
		}
		cfg.AdvertisedCDSHooks = hooks
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
	if raw := cfg.PayerDavinciBackendHeadersRaw; raw != "" {
		if cfg.PayerDavinciBaseURL == "" {
			return config{}, fmt.Errorf("gateway: PAYER_DAVINCI_BACKEND_HEADERS set requires PAYER_DAVINCI_BASE_URL")
		}
		h, err := parseBackendHeaders(raw)
		if err != nil {
			return config{}, fmt.Errorf("gateway: PAYER_DAVINCI_BACKEND_HEADERS: %w", err)
		}
		cfg.PayerDavinciBackendHeaders = h
	}
	switch cfg.payerDavinciBackendCorrelationRaw {
	case "", "on":
	case "off":
		if cfg.PayerDavinciBaseURL == "" {
			return config{}, fmt.Errorf("gateway: PAYER_DAVINCI_BACKEND_CORRELATION=off requires PAYER_DAVINCI_BASE_URL")
		}
		cfg.PayerDavinciBackendCorrelationOff = true
	default:
		return config{}, fmt.Errorf("gateway: PAYER_DAVINCI_BACKEND_CORRELATION must be on or off, got %q", cfg.payerDavinciBackendCorrelationRaw)
	}
	cfg.PayerDavinciBackendTimeout = defaultPayerBackendTimeout
	if raw := cfg.payerDavinciBackendTimeoutRaw; raw != "" {
		if cfg.PayerDavinciBaseURL == "" {
			return config{}, fmt.Errorf("gateway: PAYER_DAVINCI_BACKEND_TIMEOUT set requires PAYER_DAVINCI_BASE_URL")
		}
		d, err := time.ParseDuration(raw)
		if err != nil || d < minPayerBackendTimeout || d > maxPayerBackendTimeout {
			return config{}, fmt.Errorf("gateway: PAYER_DAVINCI_BACKEND_TIMEOUT must be a duration from %s to %s (it leaves the requester's %s leg budget room to carry this gateway's answer), got %q", minPayerBackendTimeout, maxPayerBackendTimeout, requesterLegBudget, raw)
		}
		cfg.PayerDavinciBackendTimeout = d
	}

	// The operated $populate connector's credential block: same exactly-one-mode
	// rule, same partial-block-is-a-misconfig posture. Zero creds is the
	// deliberate-unauthenticated mode (warned where the populator is wired).
	if cfg.ProviderDTRPopulateTokenURL != "" {
		if cfg.ProviderDTRPopulateURL == "" {
			return config{}, fmt.Errorf("gateway: PROVIDER_DTR_POPULATE_TOKEN_URL set requires PROVIDER_DTR_POPULATE_URL")
		}
		if cfg.ProviderDTRPopulateClientID == "" {
			return config{}, fmt.Errorf("gateway: PROVIDER_DTR_POPULATE_TOKEN_URL requires PROVIDER_DTR_POPULATE_CLIENT_ID")
		}
		if err := checkClientAuthMode("PROVIDER_DTR_POPULATE", cfg.ProviderDTRPopulateClientKey, cfg.ProviderDTRPopulateClientAlg, cfg.ProviderDTRPopulateClientKID, cfg.ProviderDTRPopulateClientSecret); err != nil {
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

	// FR-G48: a payer that forwards to its own system declares the lines that
	// system speaks. With SHN_CONTRACT_VERSIONS unset and the system's own
	// versions declared, the declaration is derived from them, not the build
	// default: the forward refuses every leg that shares no line with the
	// system, so the default would advertise lines this gateway then refuses.
	// Explicit or derived, a declaration with no CRD, DTR or PAS line leaves the
	// payer unreachable for prior authorization, and refuses boot.
	if payerNative(cfg) {
		if !declaresAny(getenv("SHN_CONTRACT_VERSIONS")) && len(cfg.PayerDavinciContractVersions) > 0 {
			derived, err := engine.DeriveDeclaredContractVersions(cfg.PayerDavinciContractVersions)
			if err != nil {
				return config{}, fmt.Errorf("gateway: %w", err)
			}
			cfg.ContractVersions = derived
			cfg.ContractVersionsDerived = true
		}
		if err := engine.RequireForwardedContractLine(cfg.ContractVersions); err != nil {
			return config{}, fmt.Errorf("gateway: %w", err)
		}
	}

	// Per-operation bases are overrides of PAYER_DAVINCI_BASE_URL, never a
	// replacement for it: the shared base is the fallback every other forward and the
	// well-known probe use, so a per-operation base without it is a boot error.
	for _, pair := range [][2]string{
		{"PAYER_DAVINCI_DTR_BASE_URL", cfg.PayerDavinciDTRBaseURL},
		{"PAYER_DAVINCI_PAS_BASE_URL", cfg.PayerDavinciPASBaseURL},
	} {
		if pair[1] != "" && cfg.PayerDavinciBaseURL == "" {
			return config{}, fmt.Errorf("gateway: %s set requires PAYER_DAVINCI_BASE_URL", pair[0])
		}
	}

	// A payer's own eligibility endpoint: an absolute http(s) URL, on a payer
	// gateway that forwards to its payer's system (PAYER_DAVINCI_BASE_URL).
	if u := cfg.PayerEligibilityURL; u != "" {
		if cfg.Role != "payer" {
			return config{}, fmt.Errorf("gateway: PAYER_ELIGIBILITY_URL is a payer gateway setting; this gateway's ROLE is %q", cfg.Role)
		}
		if cfg.PayerDavinciBaseURL == "" {
			return config{}, fmt.Errorf("gateway: PAYER_ELIGIBILITY_URL set requires PAYER_DAVINCI_BASE_URL")
		}
		parsed, err := url.Parse(u)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
			return config{}, fmt.Errorf("gateway: PAYER_ELIGIBILITY_URL must be an absolute http(s) URL, got %q", u)
		}
	}

	// Payer-edge identity mapping seam (gateway/engine/payoredge.go): all-or-nothing —
	// a partial set (one env var fat-fingered, the other forgotten) is a hard startup
	// error, mirroring the exactly-one-mode credential-block precedent above. Both
	// unset ⇒ the seam stays off (verbatim relay — every existing deployment's
	// behavior is unaffected).
	switch ownRaw, backendRaw := cfg.PayerDavinciPayorOwnRaw, cfg.PayerDavinciPayorBackendRaw; {
	case ownRaw == "" && backendRaw == "":
		// seam off
	case ownRaw == "" || backendRaw == "":
		return config{}, fmt.Errorf("gateway: PAYER_DAVINCI_PAYOR_OWN and PAYER_DAVINCI_PAYOR_BACKEND must be set together (both or neither)")
	default:
		own, oerr := parsePayerIdentifier("PAYER_DAVINCI_PAYOR_OWN", ownRaw)
		if oerr != nil {
			return config{}, oerr
		}
		backend, berr := parsePayerIdentifier("PAYER_DAVINCI_PAYOR_BACKEND", backendRaw)
		if berr != nil {
			return config{}, berr
		}
		cfg.PayerDavinciPayorOwn = own
		cfg.PayerDavinciPayorBackend = backend
		cfg.PayerDavinciPayorEdge = true
	}

	if cfg.ProviderDTRNative && cfg.ProviderDTRPopulateURL == "" {
		return config{}, fmt.Errorf("gateway: PROVIDER_DTR_NATIVE=true requires PROVIDER_DTR_POPULATE_URL")
	}
	// Every origination lane that talks to a REAL payer needs an OPERATED $populate: the
	// questionnaires a real payer advertises are its own resources, and the managed filler
	// only ever knew the retired in-process questionnaire tree. So "demo" carries the same
	// requirement "provider-data" always did. Only a provider originates: a payer's or a
	// facility's gateway that sets a profile populates nothing, so it needs no endpoint.
	if requiresOperatedPopulate(cfg.Role, cfg.OriginationProfile) && cfg.ProviderDTRPopulateURL == "" {
		return config{}, fmt.Errorf("gateway: ORIGINATION_PROFILE=%s requires PROVIDER_DTR_POPULATE_URL (the operated $populate endpoint)", cfg.OriginationProfile)
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
		// The documentation return paths the gateway writes into a pended
		// answer (E-10) are built under this base: a base they cannot be
		// built under refuses to boot, so a serving gateway never meets an
		// answer it cannot re-key.
		if _, err := cdexroute.ReturnBase(cfg.ProviderDavinciIngressBaseURL, "boot-check"); err != nil {
			return config{}, fmt.Errorf("gateway: invalid PROVIDER_DAVINCI_INGRESS_BASE_URL %q: %v", cfg.ProviderDavinciIngressBaseURL, err)
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
	// The payer's ingress, by the same all-or-nothing rule (FR-G13).
	if cfg.PayerDavinciIngress {
		if role != "payer" {
			return config{}, fmt.Errorf("gateway: PAYER_DAVINCI_INGRESS is payer-only (role=%q)", role)
		}
		if cfg.PayerDavinciIngressBaseURL == "" {
			return config{}, fmt.Errorf("gateway: PAYER_DAVINCI_INGRESS requires PAYER_DAVINCI_INGRESS_BASE_URL (the config-pinned SMART aud)")
		}
		// Every per-provider base the gateway lists is built under this base:
		// a base they cannot be built under refuses to boot, so a serving
		// gateway never lists a base it cannot build.
		if _, err := cdexroute.ProviderBase(cfg.PayerDavinciIngressBaseURL, "boot-check"); err != nil {
			return config{}, fmt.Errorf("gateway: invalid PAYER_DAVINCI_INGRESS_BASE_URL %q: %v", cfg.PayerDavinciIngressBaseURL, err)
		}
		clients, err := loadIngressClientsScoped(cfg.ProviderDavinciIngressClientsFile, engine.TaskWriteScope)
		if err != nil {
			return config{}, fmt.Errorf("gateway: ingress clients: %w", err)
		}
		if len(clients) == 0 {
			return config{}, fmt.Errorf("gateway: PAYER_DAVINCI_INGRESS requires INGRESS_CLIENTS_FILE with >=1 registered client (a mounted-but-universally-401 ingress is a footgun)")
		}
		// Every route the payer ingress mounts takes only a bearer granted
		// engine.TaskWriteScope: a client that cannot be granted it would be
		// refused on every call, so it is a registration error, not a client.
		for id, c := range clients {
			if !slices.Contains(c.Scopes, engine.TaskWriteScope) {
				return config{}, fmt.Errorf("gateway: ingress client %q lists no %s: every route of the payer ingress takes only a bearer granted it (list it, or leave scopes out)", id, engine.TaskWriteScope)
			}
		}
		cfg.IngressBaseURL = cfg.PayerDavinciIngressBaseURL
		cfg.IngressClients = clients
	}
	if err := checkProviderCDex(role, cfg); err != nil {
		return config{}, err
	}
	if cfg.ScenarioCallersFile != "" {
		if role != "provider" {
			return config{}, fmt.Errorf("gateway: SCENARIO_CALLERS_FILE is provider-only (role=%q): no other role serves /scenario", role)
		}
		if cfg.OriginationProfile == "" {
			return config{}, fmt.Errorf("gateway: SCENARIO_CALLERS_FILE requires ORIGINATION_PROFILE: with none, module 1 is off and no route serves /scenario")
		}
		callers, err := loadScenarioCallers(cfg.ScenarioCallersFile)
		if err != nil {
			return config{}, fmt.Errorf("gateway: scenario callers: %w", err)
		}
		cfg.ScenarioCallers = callers
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

	// SHN_DEMO_EDGE_CAPTURE is only ever readable back through
	// GET /demo/capture/{correlationId} on the observer loopback listener
	// (demo_endpoint.go) — with OBSERVER_ADDR unset there is no way to ever
	// read a captured entry, so leaving the store on would silently burn
	// memory recording payloads nothing can retrieve. A quiet downgrade
	// (never a boot refusal): an operator who forgot OBSERVER_ADDR still
	// gets a working, just capture-less, gateway; build() logs the
	// requested-but-disabled line so it isn't silent.
	if cfg.DemoEdgeCapture && cfg.ObserverAddr == "" {
		cfg.DemoEdgeCapture = false
	}

	// In-container TLS: opt-in, both-or-neither. A half-configured pair is a boot
	// error — never a silent fall back to plaintext.
	cfg.TLSCertFile = getenv("TLS_CERT_FILE")
	cfg.TLSKeyFile = getenv("TLS_KEY_FILE")
	if (cfg.TLSCertFile == "") != (cfg.TLSKeyFile == "") {
		return config{}, fmt.Errorf("gateway: TLS_CERT_FILE and TLS_KEY_FILE must be set together (got cert=%q key=%q)", cfg.TLSCertFile, cfg.TLSKeyFile)
	}

	// Exchange correlation-record lifetime. Unset ⇒ the 168h default; set-but-
	// unparsable or non-positive is a hard boot error naming the variable (an
	// operator who meant "no expiry" and typed 0 would otherwise get the exact
	// opposite — every record expired on write).
	cfg.ExchangeTTL = 168 * time.Hour
	if raw := getenv("EXCHANGE_TTL"); raw != "" {
		d, derr := time.ParseDuration(raw)
		if derr != nil || d <= 0 {
			return config{}, fmt.Errorf("gateway: EXCHANGE_TTL must be a positive Go duration (e.g. 168h), got %q", raw)
		}
		cfg.ExchangeTTL = d
	}

	// Shared-state pool size. Unset ⇒ the default above; set-but-unparsable, non-positive
	// or past math.MaxInt32 is a hard boot error naming the variable. Zero would leave the
	// gateway unable to open a single connection, and the upper bound is the pool field's
	// own width: pgxpool's MaxConns is an int32, so a larger value would WRAP — 2147483648
	// to a negative pool pgxpool rejects with an error naming nothing the operator set,
	// 2147483649 to a one-connection pool that boots looking healthy.
	cfg.StoreMaxConns = defaultStoreMaxConns
	if raw := getenv("SHN_STORE_MAX_CONNS"); raw != "" {
		n, cerr := strconv.Atoi(raw)
		if cerr != nil || n <= 0 || n > math.MaxInt32 {
			return config{}, fmt.Errorf("gateway: SHN_STORE_MAX_CONNS must be an integer in [1, %d], got %q", math.MaxInt32, raw)
		}
		cfg.StoreMaxConns = int32(n)
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

// loadScenarioCallers reads the callers a provider gateway's /scenario routes
// admit: a JSON array of {label, public_key}, the key base64 Ed25519. An empty
// array is an empty, non-nil set: no caller is admitted. An empty or repeated
// label, or a key that is not an Ed25519 public key, refuses the file.
func loadScenarioCallers(path string) (map[string]ed25519.PublicKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	callers, err := scenarioauth.ParseCallers(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return callers, nil
}

// loadIngressClients parses the inbound-client registration file: a JSON array of
// {client_id, alg, public_key_pem, scopes, module}. alg must be ES384|RS384 and the PEM must
// parse — a malformed entry is a hard boot error (the FR-G13 all-or-nothing posture).
// module, when set, registers the client as a module ingress client of that compiled-in
// module (engine.ModuleIDs; the engine refuses another); the builders such a client may
// declare are its module's, so an entry that lists builders is refused.
// Returns nil (not error) when path is empty — the caller then enforces the
// must-have->=1-client invariant.
func loadIngressClients(path string) (map[string]engine.IngressClientRegistration, error) {
	return loadIngressClientsScoped(path, "system/Davinci.write")
}

// loadIngressClientsScoped is loadIngressClients with the scope a client that
// lists none is registered for: the provider ingress's system/Davinci.write,
// or, on a payer's, the attachment-request Task routes' engine.TaskWriteScope.
func loadIngressClientsScoped(path, defaultScope string) (map[string]engine.IngressClientRegistration, error) {
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
		Module       string   `json:"module"`
	}
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var keys []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for i, k := range keys {
		if _, listed := k["builders"]; listed {
			return nil, fmt.Errorf("entry %d: builders is not configurable: a module client may declare its module's builders (engine.ModuleBuilders), set by the build", i)
		}
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
			scopes = []string{defaultScope}
		}
		out[c.ClientID] = engine.IngressClientRegistration{Alg: c.Alg, PublicKeyPEM: pemBytes, Scopes: scopes, Module: c.Module}
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
		{"FHIR_VALIDATE_URL", cfg.FHIRValidateURL},
		{"FHIR_VALIDATE_URL_2_1", cfg.FHIRValidateURL21},
		{"FHIR_VALIDATE_URL_2_2", cfg.FHIRValidateURL22},
		{"FHIR_CERTIFY_URL_2_1", cfg.FHIRCertifyURL21},
		{"FHIR_CERTIFY_URL_2_2", cfg.FHIRCertifyURL22},
		{"FHIR_DATA_URL", cfg.FHIRDataURL},
		{"REGISTRAR_URL", cfg.RegistrarURL},
		{"FHIR_TOKEN_URL", cfg.FHIRTokenURL},
		{"PAYER_DAVINCI_BASE_URL", cfg.PayerDavinciBaseURL},
		{"PAYER_DAVINCI_DTR_BASE_URL", cfg.PayerDavinciDTRBaseURL},
		{"PAYER_DAVINCI_PAS_BASE_URL", cfg.PayerDavinciPASBaseURL},
		{"PAYER_DAVINCI_TOKEN_URL", cfg.PayerDavinciTokenURL},
		{"PROVIDER_DTR_POPULATE_URL", cfg.ProviderDTRPopulateURL},
		{"PROVIDER_DTR_POPULATE_TOKEN_URL", cfg.ProviderDTRPopulateTokenURL},
		{"PROVIDER_CDEX_TASK_URL", cfg.ProviderCDexTaskURL},
		{"PROVIDER_CDEX_TOKEN_URL", cfg.ProviderCDexTokenURL},
		{"PROVIDER_DAVINCI_INGRESS_BASE_URL", cfg.ProviderDavinciIngressBaseURL},
		{"PAYER_DAVINCI_INGRESS_BASE_URL", cfg.PayerDavinciIngressBaseURL},
		{"SHN_DISCOVERY_URL", cfg.DiscoveryURL},
		{"AUTHZ_PUBKEY_URL", cfg.AuthzPubkeyURL},
		{"HUB_TRANSPORT_KEY_URL", cfg.HubTransportKeyURL},
	}
}

// payerDavinciPartnerBases names the partner payer system's bases among
// optionalURLs: the targets PAYER_DAVINCI_BACKEND_HEADERS is sent to. Named one
// by one, never by prefix: PAYER_DAVINCI_TOKEN_URL and
// PAYER_DAVINCI_INGRESS_BASE_URL (this gateway's own base) share it.
var payerDavinciPartnerBases = map[string]bool{
	"PAYER_DAVINCI_BASE_URL":     true,
	"PAYER_DAVINCI_DTR_BASE_URL": true,
	"PAYER_DAVINCI_PAS_BASE_URL": true,
}

// checkTargets derives the /internal/checks probe targets from
// optionalURLs(cfg) — the exact table checkOptionalURL walks at boot, so a
// target can never be added or dropped independently of that well-formedness
// gate. Skips unset pairs. Kind overlay: the two FHIR-facing base URLs probe
// as fhir-metadata (a live $/metadata fetch, authenticated through
// probeAuthClient when that base's own token URL is set); the SMART token endpoints
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
		if payerDavinciPartnerBases[name] {
			// The partner's bases are probed the way they are called; the token
			// endpoint and this gateway's own ingress base never see the
			// partner's routing headers.
			t.Headers = cfg.PayerDavinciBackendHeaders
		}
		switch name {
		case "FHIR_DATA_URL":
			t.Kind = checks.KindFHIRMetadata
			if cfg.FHIRTokenURL != "" {
				t.AuthClient = probeAuthClient(fhirSmartConfig(cfg))
			}
		case "PAYER_DAVINCI_BASE_URL":
			t.Kind = checks.KindFHIRMetadata
			if cfg.PayerDavinciTokenURL != "" {
				t.AuthClient = probeAuthClient(payerDavinciSmartConfig(cfg))
			}
		case "FHIR_TOKEN_URL":
			t.Kind = checks.KindToken
			t.TokenFetch = fhirTokenFetch(cfg)
		case "PAYER_DAVINCI_TOKEN_URL":
			t.Kind = checks.KindToken
			t.TokenFetch = payerDavinciTokenFetch(cfg)
		case "PROVIDER_DTR_POPULATE_TOKEN_URL":
			t.Kind = checks.KindToken
			t.TokenFetch = providerDTRPopulateTokenFetch(cfg)
		case "PROVIDER_CDEX_TOKEN_URL":
			t.Kind = checks.KindToken
			t.TokenFetch = providerCDexTokenFetch(cfg)
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
			Headers:          cfg.PayerDavinciBackendHeaders,
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

// probeAuthClient is the client a fhir-metadata check of a partner base reads
// through when the gateway's own calls to that base authenticate: the
// same SMART config, so the check measures the request the gateway makes. It
// is a client of its own, outside diagnostic capture: a check is not an
// exchange. A token it cannot obtain reaches the checks package as the token
// endpoint's status (*checks.StatusError) or checks.ErrCredential, never the
// token endpoint's error text; a key that does not load fails every read the
// same way (build() has already refused to serve on it).
func probeAuthClient(sc smartauth.Config, err error) *http.Client {
	if err == nil {
		var hc *http.Client
		if hc, err = smartauth.NewHTTPClient(sc); err == nil {
			hc.Transport = probeCredentialTransport{base: hc.Transport}
			return hc
		}
	}
	return &http.Client{Transport: probeCredentialTransport{err: err}}
}

// probeCredentialTransport maps a failure to obtain the probe's token onto the
// checks package's credential errors; err set ⇒ every request fails so.
type probeCredentialTransport struct {
	base http.RoundTripper
	err  error
}

func (t probeCredentialTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.err != nil {
		return nil, checks.ErrCredential
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil && smartauth.IsTokenAcquisitionError(err) {
		var te *smartauth.TokenEndpointError
		if errors.As(err, &te) {
			return nil, &checks.StatusError{Code: te.StatusCode}
		}
		return nil, checks.ErrCredential
	}
	return resp, err
}

// fhirSmartConfig is the FHIR_* SMART config the system-of-record connector
// authenticates with (fhirHTTPClient) and its checks reuse, without diagnostic
// capture. Call it only when FHIR_TOKEN_URL is set.
func fhirSmartConfig(cfg config) (smartauth.Config, error) {
	sc := smartauth.Config{
		TokenURL: cfg.FHIRTokenURL, ClientID: cfg.FHIRClientID, Scope: cfg.FHIRClientScope,
		Observer: cfg.tokenNotes,
	}
	tagSmart(cfg.certs, &sc, cfg.certs.Wrap(endpointcert.Of(endpointcert.FHIR), nil), endpointcert.FHIRToken)
	if cfg.FHIRClientSecret != "" {
		sc.ClientSecret = cfg.FHIRClientSecret // client_secret_post; no key material
		return sc, nil
	}
	key, err := loadSmartKey(cfg.FHIRClientKey, cfg.FHIRClientAlg)
	if err != nil {
		return sc, fmt.Errorf("load FHIR client key: %w", err)
	}
	sc.Alg, sc.Key, sc.KID = cfg.FHIRClientAlg, key, cfg.FHIRClientKID
	return sc, nil
}

// payerDavinciSmartConfig is fhirSmartConfig's PAYER_DAVINCI_* counterpart:
// the config the native-forward client (payerDavinciHTTPClient) authenticates
// with. Call it only when PAYER_DAVINCI_TOKEN_URL is set.
func payerDavinciSmartConfig(cfg config) (smartauth.Config, error) {
	sc := smartauth.Config{
		TokenURL: cfg.PayerDavinciTokenURL, ClientID: cfg.PayerDavinciClientID, Scope: cfg.PayerDavinciScope,
		Observer: cfg.tokenNotes,
	}
	tagSmart(cfg.certs, &sc, payerTransport(cfg.certs), endpointcert.PayerToken)
	if cfg.PayerDavinciClientSecret != "" {
		sc.ClientSecret = cfg.PayerDavinciClientSecret // client_secret_post; no key material
		return sc, nil
	}
	key, err := loadSmartKey(cfg.PayerDavinciClientKey, cfg.PayerDavinciClientAlg)
	if err != nil {
		return sc, fmt.Errorf("load payer-davinci client key: %w", err)
	}
	sc.Alg, sc.Key, sc.KID = cfg.PayerDavinciClientAlg, key, cfg.PayerDavinciClientKID
	return sc, nil
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
	sc, err := fhirSmartConfig(cfg)
	if err != nil {
		return func(context.Context) error { return err }
	}
	return func(ctx context.Context) error {
		return classifyTokenErr(errFromToken(&smartauth.TokenSource{Config: sc}, ctx))
	}
}

// payerDavinciTokenFetch is fhirTokenFetch's PAYER_DAVINCI_* counterpart,
// mirroring payerDavinciHTTPClient's Config construction. Same fresh-
// TokenSource-per-invocation rule applies (see fhirTokenFetch's doc).
func payerDavinciTokenFetch(cfg config) func(context.Context) error {
	sc, err := payerDavinciSmartConfig(cfg)
	if err != nil {
		return func(context.Context) error { return err }
	}
	return func(ctx context.Context) error {
		return classifyTokenErr(errFromToken(&smartauth.TokenSource{Config: sc}, ctx))
	}
}

// providerDTRPopulateTokenFetch is fhirTokenFetch's PROVIDER_DTR_POPULATE_*
// counterpart, mirroring providerDTRPopulateHTTPClient's Config construction.
// Same fresh-TokenSource-per-invocation rule applies (see fhirTokenFetch's doc).
func providerDTRPopulateTokenFetch(cfg config) func(context.Context) error {
	sc := providerDTRPopulateSmartBase(cfg)
	if cfg.ProviderDTRPopulateClientSecret != "" {
		sc.ClientSecret = cfg.ProviderDTRPopulateClientSecret
	} else {
		key, err := loadSmartKey(cfg.ProviderDTRPopulateClientKey, cfg.ProviderDTRPopulateClientAlg)
		if err != nil {
			return func(context.Context) error { return err }
		}
		sc.Alg, sc.Key, sc.KID = cfg.ProviderDTRPopulateClientAlg, key, cfg.ProviderDTRPopulateClientKID
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
	// Consent is discovery-advertised; explicit env still wins via
	// firstNonEmpty in build (same pattern as Authz/Hub).
	Consent string
	// HubAcceptsInvolved: the network's Hub reads an envelope's involved list
	// (the descriptor lists it in hubAccepts). Read once, at start.
	HubAcceptsInvolved bool
	// Audit is discovery's endpoints.audit, where the audit outbox sends. Read
	// once, at start, with no setting to override it; empty is "no audit
	// endpoint known", never an empty endpoint.
	Audit string
}

// resolveDiscovery fetches the /discovery descriptor and resolves the trust
// anchors (AuthzPub via disc.AuthzPublicKeyURL /pubkey; HubTransportPub via
// disc.HubTransportKeyURL) + endpoint URLs (incl. Registrar + FHIRValidate).
// Mirrors cmd/gateway/main.go resolveIdentity's discovery branch using public
// shnsdk symbols only. FAIL-CLOSED: an unreachable discovery / key fetch errors.
func resolveDiscovery(ctx context.Context, c *http.Client, cfg config) (trustAnchors, resolvedEndpoints, error) {
	boot := func(k endpointcert.Kind) *http.Client {
		return bootClient(cfg.certs.Client(endpointcert.Of(k), c), bootFetchWaits, bootFetchBudget)
	}
	return resolveDiscoveryWaiting(ctx, discoveryClients{doc: boot(endpointcert.Discovery), authzKey: boot(endpointcert.AuthorizationKey), hubKey: boot(endpointcert.HubKey)}, cfg)
}

// discoveryClients are the boot clients of the discovery document and the
// two trust-anchor keys, each with its own setting's endpoint kind: a key
// URL may name a host of its own. The keys are read once, at boot, so their
// certificates are the ones that boot's handshakes saw.
type discoveryClients struct{ doc, authzKey, hubKey *http.Client }

// resolveDiscoveryWaiting is resolveDiscovery with its clients already the
// boot clients.
func resolveDiscoveryWaiting(ctx context.Context, c discoveryClients, cfg config) (trustAnchors, resolvedEndpoints, error) {
	var ta trustAnchors
	var ep resolvedEndpoints

	var raw json.RawMessage
	if err := getJSON(ctx, c.doc, cfg.DiscoveryURL, &raw); err != nil {
		return ta, ep, wireUnknown(err)
	}
	var disc shnsdk.Discovery
	if err := json.Unmarshal(raw, &disc); err != nil {
		return ta, ep, wireUnknown(err)
	}
	if err := checkWireAccepted(raw, cfg.DiscoveryURL); err != nil {
		return ta, ep, err
	}

	authzPub, err := fetchEd25519Key(ctx, c.authzKey, firstNonEmpty(cfg.AuthzPubkeyURL, disc.AuthzPublicKeyURL))
	if err != nil {
		return ta, ep, fmt.Errorf("fetch authz pubkey: %w", err)
	}
	hubTxPub, err := shnsdk.FetchHubTransportKey(ctx, c.hubKey, firstNonEmpty(cfg.HubTransportKeyURL, disc.HubTransportKeyURL))
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

		HubAcceptsInvolved: disc.HubAcceptsField(shnsdk.HubAcceptsInvolved),
		Audit:              disc.Endpoints.Audit,
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
	// httpServerMetered is whether build wrapped the main listener in the HTTP
	// server duration (MON-R29), recorded where it wraps it, so this package's
	// tests prove a gateway that exports nothing leaves the listener as it is
	// (MON-R1).
	httpServerMetered bool
	// requireKnownMembers is the known-member opt-in exactly as build() handed it
	// to engine.New, recorded so this package's tests prove the variable reaches
	// the engine without a published test accessor.
	requireKnownMembers bool
	// requireSharedLine is the shared-line opt-in exactly as build() handed it
	// to engine.New, recorded for the same reason.
	requireSharedLine bool
	// attachmentCeiling and attachmentInflight are the attachment ceiling and
	// the budget for large bodies exactly as build() handed them to engine.New,
	// for the wiring test.
	attachmentCeiling  int64
	attachmentInflight int64
	// translateIGLines is the module 2 opt-in exactly as build() handed it to
	// engine.New, recorded for the same reason.
	translateIGLines bool
	// enrichNativeRequests is the enrichment opt-in exactly as build() handed it
	// to engine.New, recorded for the same reason.
	enrichNativeRequests bool
	// fhirServerRead is the fhirServer read mode exactly as build() handed it
	// to engine.New, recorded for the same reason.
	fhirServerRead string
	// operatedPopulate reports whether build() handed engine.New the operated
	// $populate populator, recorded for the same reason.
	operatedPopulate bool

	diagnostic *diagnosticSource
	gateway    *engine.Gateway
	// responder is the leg responder build() handed to engine.New (nil when
	// none was), recorded so this package's tests can drive one leg through it.
	responder    engine.LegResponder
	addr         string
	handler      http.Handler
	reg          shnsdk.Registry // shared-reference value type; the poller mutates the same state the engine reads
	registrarURL string
	// identity and capabilities are this gateway's holder and what its system
	// declares it takes (declareCapabilities), under registrarURL.
	identity     shnsdk.Identity
	capabilities shnsdk.CapabilityDeclaration
	declared     *declarationCell
	client       *http.Client // the registrar poller's: registrar-tagged

	// certs is the outbound certificate recorder every client above was
	// tagged by (MON-R25), for the expiry gauge.
	certs *endpointcert.Recorder

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

	// closeStore closes the shared-state pool. Non-nil only under
	// SHN_STORE_DATABASE_URL. Run and managed-handler cleanup close it. Tests also
	// use it directly: it is how the pg-gated
	// outage rows make a BUILT gateway's database unreachable (pg_multi_test.go
	// TestPgMulti_StoreOutageTokenIs503) instead of testing a pool the gateway does
	// not use. built is unexported, so it is not a partner-visible surface.
	closeStore func()
	lanes      *laneManager
	// certification is the evidence client map build handed the engine, kept
	// so the app's own rows can read what was wired without an engine seam.
	certification map[string]shnsdk.Validator
	// warmCertification is whether Run warms the certification clients at boot
	// (startWorkers): only where certification evidence is collected.
	warmCertification bool

	// auditAppender is the audit outbox's client: Run starts its sender in
	// startWorkers and closes it there.
	auditAppender *auditappend.Appender

	// keyRefresh is the shared ingress-key background reload
	// ((*pgstore.IngressKeyStore).RunRefresh), non-nil only when
	// SHN_STORE_DATABASE_URL selected the shared stores. Only Run starts it —
	// build starts only bounded default-lane qualification workers, not pollers.
	keyRefresh func(context.Context)

	// poolStats samples the shared-state pool into the metrics every
	// storePoolMetricInterval. Non-nil only when BOTH a store DSN and a
	// metrics sink are on. Only Run starts it, for the same reason as keyRefresh.
	poolStats func(context.Context)

	// laneStats samples the validator lanes' qualification into the metrics
	// (the ValidatorLane gauge) every validatorLaneMetricInterval. Non-nil
	// only when a metrics sink is on; only Run starts it.
	laneStats func(context.Context)
	// opsStats samples the operational gauges (opsmetrics.go) into the
	// metrics every opsMetricInterval; nil with no metrics sink.
	opsStats func(context.Context)
	// feed is the registrar feed's freshness; nil without a registrar.
	feed *feedFreshness

	// activeStats writes the EMF ActiveRequests gauge, the peak in flight,
	// every activeRequestsMetricInterval (MON-R23). Non-nil only when the EMF
	// sink is on; only Run starts it.
	activeStats func(context.Context)

	// telemetry is the monitoring export (MON-R1): Run starts its export loop
	// with the other workers, and its final export runs last at stop, after
	// the audit outbox drains and the observe checks flush (MON-R14).
	telemetry *otelsetup.Telemetry
}

// The payer gateway's deadline for its own system (PAYER_DAVINCI_BACKEND_TIMEOUT):
// under the requester's leg budget (the SDK client's timeout every gateway's
// Hub leg runs under) by enough that this gateway's framed answer still
// reaches the requester through the Hub.
var (
	requesterLegBudget         = shnsdk.NewClient().Timeout
	defaultPayerBackendTimeout = requesterLegBudget - 5*time.Second
	maxPayerBackendTimeout     = requesterLegBudget - 2*time.Second
)

const minPayerBackendTimeout = time.Second

// maxDeclaredAttachmentBytes is the largest attachment limit a payer may
// declare (2^53-1): the largest integer every JSON reader of its registry
// entry holds exactly.
const maxDeclaredAttachmentBytes = 1<<53 - 1

// Shared-state pool sizing (see config.StoreMaxConns).
const (
	defaultStoreMaxConns int32 = 8
	storeMinConns        int32 = 2
	// storeConnectTimeout bounds ONE attempt to open a connection to the shared-state
	// database. pgx leaves this unset, so a database that accepts packets but never
	// completes a handshake (a failed-over instance, a security group that started
	// dropping) makes every acquire wait for the operating system's own TCP timeout —
	// far past the storeTimeout each store call thinks it is bounded by, since that
	// bound covers the query and not the connect underneath it. One second: a healthy
	// in-VPC connect is a few milliseconds, and the whole point is to fail fast enough
	// that the request-path seams stay inside their own budget.
	storeConnectTimeout = time.Second
	// storePoolMetricInterval is how often Run samples the pool's stats when
	// METRICS_SERVICE opts in. Long enough to be free, short enough that a
	// saturation that starts expiring store contexts is visible within a minute.
	storePoolMetricInterval = 30 * time.Second
	// validatorLaneMetricInterval is how often the ValidatorLane gauge is
	// sampled: a lane's state changes rarely, so once a minute.
	validatorLaneMetricInterval = time.Minute
	// activeRequestsMetricInterval is how often the EMF ActiveRequests gauge
	// writes the peak in flight since the last sample.
	activeRequestsMetricInterval = time.Minute
)

// storePoolConfig parses the shared-state DSN and applies the fleet's own pool posture:
// explicit sizing (see config.StoreMaxConns), a warm MinConns floor so the first request
// after an idle period does not pay a connect (never above MaxConns — an operator who
// pins the pool to one connection gets one), and a bounded connect
// (storeConnectTimeout). Split out of build so the posture is assertable without a
// database: ParseConfig opens nothing.
func storePoolConfig(dsn string, maxConns int32) (*pgxpool.Config, error) {
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("gateway: SHN_STORE_DATABASE_URL is not a usable Postgres DSN: %w", err)
	}
	poolCfg.MaxConns = maxConns
	poolCfg.MinConns = storeMinConns
	if poolCfg.MinConns > poolCfg.MaxConns {
		poolCfg.MinConns = poolCfg.MaxConns
	}
	poolCfg.ConnConfig.ConnectTimeout = storeConnectTimeout
	return poolCfg, nil
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
	cfg.certs = endpointcert.NewWithClock(clock)
	recordTrustRoots(cfg.certs, getenv)
	cfg.diagnostic = newDiagnosticSource(getenv, cfg.Role, stdout, clock)
	if cfg.diagnostic != nil {
		cfg.diagnostic.publisher.Client = cfg.certs.Client(endpointcert.Of(endpointcert.Diagnostic), cfg.diagnostic.publisher.Client)
	}
	cfg.tokenNotes = func(note string) { fmt.Fprintf(stdout, "gateway: %s\n", note) }

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

	// template is what every network client below is a tagged copy of
	// (outbound.go): the same 30 s timeout and the same transport, Wrapped
	// with its kind. Only the checks runner holds it itself, as the fallback
	// for a target with no client of its own; every target that probes over
	// the network has one.
	template := shnsdk.NewClient()
	kindClient := func(k endpointcert.Kind) *http.Client { return cfg.certs.Client(endpointcert.Of(k), template) }

	// Discovery resolution: trust anchors + endpoints (incl. registrar + validator); explicit env wins.
	trust, endpoints, err := resolveDiscovery(ctx, template, cfg)
	if err != nil {
		return b, err
	}
	registrarURL := firstNonEmpty(cfg.RegistrarURL, endpoints.Registrar)

	// Validator: REAL operation-level, FAIL-CLOSED. Fake only on explicit opt-in. (pure helper, unit-tested)
	validator, err := selectValidator(getenv, firstNonEmpty(cfg.FHIRValidateURL, endpoints.FHIRValidate), cfg.ConformanceEnforcement)
	if err != nil {
		return b, err
	}
	if fakeValidator(getenv) {
		log.Printf("gateway: WARNING: SHN_FAKE_VALIDATOR=1 — every validator lane is the test double, which checks a few PAS/DTR cardinalities and bindings and accepts everything else: nothing this gateway reports as validated was checked against its IG. Test builds only; a released gateway refuses to boot with it set")
	}
	// Default declared lanes qualify before traffic admission. Explicit per-line
	// URLs retain their existing startup semantics; undeclared defaults qualify
	// in bounded background workers owned by this app lifecycle.
	if ov, ok := validator.(*shnsdk.OperationValidator); ok {
		engine.TagValidatorClient(ov, cfg.certs, validatorLane(canonicalLine()))
	}
	// One qualifier value for routing and certification alike: certification
	// takes routing's pass on a default lane only from the same qualifier.
	qualify := laneQualifier(cfg.certs)
	validatorLanes, lanes, err := discoverValidatorLanes(ctx, getenv, cfg.ContractVersions, validator, cfg, engine.DefaultLaneURL, qualify)
	if err != nil {
		return b, err
	}
	if validator == nil && len(validatorLanes) == 0 && len(lanes.defaults) == 0 {
		fmt.Fprintln(stdout, noValidatorLog)
	}

	admitted := false
	defer func() {
		if !admitted {
			lanes.Close()
		}
	}()

	// FEDERATION CORE: populate the peer registry from the live /holders feed (boot-time
	// SNAPSHOT). The engine resolves recipient EncPub via cfg.Reg.Lookup, so Reg MUST be
	// populated before serving. This is the public, SDK-based replacement for the
	// substrate's internal registrar.RunPoller (un-importable here, and rejected by the
	// gateway boundary fence). Run() additionally starts a background poller (below) for
	// post-boot convergence; the boot gate drives build() and asserts this snapshot
	// resolves the counterpart — deterministic, no ticker.
	reg := shnsdk.NewRegistry()
	registrarClient := kindClient(endpointcert.Registrar)
	var feed *feedFreshness
	if registrarURL != "" {
		feed = newFeedFreshness(clock)
		n, err := convergeRegistry(ctx, registrarClient, registrarURL, reg)
		if err != nil {
			return b, fmt.Errorf("converge peer registry from %s: %w", registrarURL, err)
		}
		feed.succeeded(n)
	}

	// SoR: the holder's own FHIR system of record. FAIL-CLOSED — there is no in-process
	// persona census to fall back on any more (§4.1), so an unset FHIR_DATA_URL is a boot
	// error naming the env rather than a silent stub. The one exception is a payer whose
	// own system answers every exchange behind PAYER_DAVINCI_BASE_URL: it may keep no
	// system of record at all, and binds each member from the request.
	var sor engine.SystemOfRecord
	if cfg.FHIRDataURL == "" {
		if !payerNative(cfg) {
			return b, errNoSystemOfRecord
		}
		sor = engine.NoSystemOfRecord()
		fmt.Fprintf(stdout, "gateway: no system of record (FHIR_DATA_URL unset): each member is bound from the request that names it, and eligibility is answered only through PAYER_ELIGIBILITY_URL\n")
	} else {
		hc, herr := fhirHTTPClient(cfg) // smartauth.NewHTTPClient when the SMART credential block is set, else nil (unauthenticated)
		if herr != nil {
			return b, herr
		}
		if hc == nil {
			hc = cfg.certs.Client(endpointcert.Of(endpointcert.FHIR), http.DefaultClient)
			if cfg.diagnostic != nil {
				hc = cfg.diagnostic.client(hc)
			}
		}
		sor = fhirsor.NewFromURL(cfg.FHIRDataURL, hc)
	}
	// Store: the gateway's OWN business state (auth numbers, pended-claim ledger).
	// In-memory by default; the SHN_STORE_DATABASE_URL override below swaps in pgstore.
	var store engine.Store = engine.NewMemStore()

	// Durable Store override: pgstore when SHN_STORE_DATABASE_URL is set, else the
	// in-memory Store selected above. pgxpool.New is lazy; NewPgStore's advisory-locked
	// EnsureSchema is the fail-fast (and the 4-gateways-one-DB race guard). Holder-
	// scoped by construction: NewPgStore captures the bundle's HolderID.
	// pool is hoisted out of the if so the /health wiring below can register a
	// DBPing check when (and only when) this branch ran.
	//
	// The same DSN also selects the SHARED state seams — the ingress bearer signing
	// key, the one-time-use replay records and the exchange correlation records.
	// Nil leaves each engine default in place (in-process, correct at exactly one
	// replica), so the posture is stated on the boot log either way.
	var (
		pool        *pgxpool.Pool
		ingressKeys engine.IngressKeyStore
		replay      engine.ReplayStore
		exchanges   engine.ExchangeStore
		keyRefresh  func(context.Context)
		closeStore  func()
		poolStats   func(context.Context)
		laneStats   func(context.Context)
		opsStats    func(context.Context)
		keyTimes    func() pgstore.IngressKeyTimes
		activeStats func(context.Context)
		// storeErrHooks are the pgstore seams' own error hooks, wired to the same EMF
		// counter as the engine's once the emitter exists (below). They exist for the
		// failures a store ABSORBS rather than returns — the exchange seam's best-effort
		// insert, and the ingress key store's BACKGROUND refresh loop — so an outage that
		// no request-path error reports is not a log line no alarm watches. The key store's
		// request-path errors are NOT wired here: they reach the engine
		// (VerificationKey's third return, SigningKey's error), which counts them once.
		storeErrHooks []func(func(store string))
		// auditStore is the audit outbox: gw_audit_outbox under a store database,
		// else in-process and lost at restart.
		auditStore auditappend.Store = auditappend.NewMemStore()
	)
	if cfg.StoreDatabaseURL != "" {
		poolCfg, perr := storePoolConfig(cfg.StoreDatabaseURL, cfg.StoreMaxConns)
		if perr != nil {
			return b, perr
		}
		tagStoreTLS(cfg.certs, &poolCfg.ConnConfig.Config)
		p, perr := pgxpool.NewWithConfig(ctx, poolCfg)
		if perr != nil {
			return b, fmt.Errorf("gateway: pgxpool.New(store): %w", perr)
		}
		pg, serr := pgstore.NewPgStore(ctx, p, bundle.Identity.HolderID)
		if serr != nil {
			return b, fmt.Errorf("gateway: pgstore.NewPgStore: %w", serr)
		}
		store = pg
		pool = p
		auditStore = pgstore.NewAuditOutbox(p, bundle.Identity.HolderID)

		replay = pgstore.NewReplayStore(p, bundle.Identity.HolderID, clock)
		exs := pgstore.NewExchangeStore(p, bundle.Identity.HolderID, cfg.ExchangeTTL, clock)
		exchanges = exs
		storeErrHooks = append(storeErrHooks, exs.SetErrorHook)
		closeStore = p.Close
		// The shared ingress bearer key is only meaningful where the ingress runs: with
		// PROVIDER_DAVINCI_INGRESS unset no /oauth/token and no ingress route is mounted,
		// so persisting (and rotating daily, and reloading every 30 s) a signing key for
		// this holder would be key material and database traffic no request can use.
		// Replay and exchange stay unconditional — the Hub-assertion guard and the
		// exchange seam run on every role.
		ingressKey := "off"
		if cfg.ProviderDavinciIngress || cfg.PayerDavinciIngress {
			ks := pgstore.NewIngressKeyStore(p, bundle.Identity.HolderID, clock)
			ingressKeys = ks
			keyTimes = ks.KeyTimes
			// The refresh loop's own error hook — the only key-store failure the engine
			// never sees (nothing is waiting on a background reload, so nothing is
			// returned). It is set below, with the emitter, and stays nil when metrics are
			// off; Run starts the loop after build returns, so the write is ordered before
			// the read by the goroutine's own start.
			var refreshErr func(store string)
			storeErrHooks = append(storeErrHooks, func(h func(store string)) { refreshErr = h })
			// Carried out on the built value below; only Run starts it — build
			// starts no goroutines.
			keyRefresh = func(ctx context.Context) { ks.RunRefresh(ctx, refreshErr) }
			ingressKey = "on"
		}
		log.Printf("gateway: shared state: postgres (replay, exchange; ingress key %s; exchange TTL %v)", ingressKey, cfg.ExchangeTTL)
	}
	if pool == nil {
		log.Printf("gateway: shared state: in-process (ephemeral ingress key; single-instance only; exchange TTL %v)", cfg.ExchangeTTL)
	}

	// /health: holder id as the service name — holder ids
	// are already public via the /holders feed, non-sensitive, and distinguish the
	// two payer-role gateways from each other. feedCell is created ONLY when a
	// registrar is configured (mirrors the poller-goroutine gate in Run); its
	// Check is NOT nil-safe, so it must only be Register'd inside that branch.
	healthVersion, versionLine := buildVersion(getenv("SHN_VERSION"))
	log.Print(versionLine)
	hreg := health.New(bundle.Identity.HolderID, healthVersion)
	var feedCell *health.PollerCell
	declared := &declarationCell{}
	if registrarURL != "" {
		feedCell = health.NewPollerCell("registrar-poller", 30*time.Second)
		hreg.Register(feedCell.Check)
		hreg.Register(declared.Check)
	}
	if pool != nil {
		hreg.Register(health.DBPing("store", pool.Ping))
	}

	// PayerRouter (FR-G40/G41): default to feed-derived discovery off the converged /holders
	// registry (FeedPayerRouter). PAYER_DIRECTORY, when set, overrides with a static
	// provider-maintained map (a fallback for bootstrap or testing). No default payer holder either way.
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
		Role:                     cfg.Role,
		HolderID:                 bundle.Identity.HolderID,
		PayerRouter:              payerRouter,
		Identity:                 bundle.Identity,
		AuthzURL:                 firstNonEmpty(cfg.AuthzURL, endpoints.Authz),
		AuthzPub:                 trust.AuthzPub,
		HubTransportPub:          trust.HubTransportPub,
		HubURL:                   firstNonEmpty(cfg.HubURL, endpoints.Hub),
		Reg:                      reg, // populated by the snapshot above
		RegistryRefresh:          registryRefresh(registrarClient, registrarURL, reg, feed),
		Validator:                validator,
		ValidatorsByContractLine: engine.ContractLineLanes(validatorLanes, lanes.defaults, lanes.fallbacks, cfg.ContractVersions),
		// D1a: the boot-validated declared set — read by leg selection, the published
		// CapabilityStatements / davinci-configuration, and (through the registrar
		// registration) the declaration peers select this holder against.
		DeclaredContractVersions: cfg.ContractVersions,
		// EgressNativeLines is set by SHN_DEMO_EGRESS_NATIVE_LINES. The hosted
		// preview, reference-participant composition and desktop gateway child
		// use "2.0" for the existing bridging exhibits.
		EgressNativeLines: cfg.DemoEgressNativeLines,
		// DemoEdgeCapture: false in every shipped deploy; SHN_DEMO_EDGE_CAPTURE
		// is the sole non-test way to set it.
		DemoEdgeCapture: cfg.DemoEdgeCapture,
		// ConformanceEnforcement: none unless CONFORMANCE_ENFORCEMENT says
		// observe, structural or strict (loadConfig above is what makes an absent
		// value none).
		ConformanceEnforcement:   cfg.ConformanceEnforcement,
		ConformanceToleratePeers: cfg.ConformanceToleratePeers,
		AdvertisedCDSHooks:       cfg.AdvertisedCDSHooks,
		SoR:                      sor,
		Store:                    store,
		Clock:                    clock, // production: time.Now; hermetic tests: the harness's injected clock (HandlerWithClock)
		Client:                   kindClient(endpointcert.Hub),
		AuthzClient:              kindClient(endpointcert.Authorization),
		ConsentClient:            kindClient(endpointcert.Consent),
		CertRecorder:             cfg.certs,
		NPI:                      cfg.NPI,
		ConsentURL:               firstNonEmpty(cfg.ConsentURL, endpoints.Consent),

		OriginationProfile: cfg.OriginationProfile,
		// Strict extensions (FR-G52): g.strictPeer is production-dormant BY DESIGN (always false —
		// see its comment, gateway/engine/linereach.go) — there is
		// deliberately no engine.Config field here feeding it from env.
	}
	// Shared-state seams: nil on every field unless the store DSN selected the
	// Postgres stores above; the engine substitutes its in-process defaults.
	gwCfg.IngressKeys = ingressKeys
	gwCfg.Replay = replay
	gwCfg.Exchanges = exchanges
	gwCfg.ExchangeTTL = cfg.ExchangeTTL

	if len(cfg.DemoEgressNativeLines) > 0 {
		fmt.Fprintf(stdout, "gateway: demo: egress-native lines narrowed to %v — arm-2 native reach restricted; transform chains may fire (SHN_DEMO_EGRESS_NATIVE_LINES)\n", cfg.DemoEgressNativeLines)
	}
	if cfg.DemoEdgeCapture {
		fmt.Fprintln(stdout, "gateway: demo: edge capture enabled — bounded in-memory inspection of this gateway's own pre-seal egress payloads (SHN_DEMO_EDGE_CAPTURE)")
	} else if getenv("SHN_DEMO_EDGE_CAPTURE") == "true" {
		// The flag was requested but loadConfig gated it off because
		// OBSERVER_ADDR is unset — say so loudly rather than silently
		// running without it.
		fmt.Fprintln(stdout, "gateway: demo: edge capture requested but OBSERVER_ADDR is unset — capture disabled (nothing could read it) (SHN_DEMO_EDGE_CAPTURE)")
	}
	// evidenceSink is set below iff native-forward mode is on — the
	// checks-runner→responder endpoint-evidence wiring target (nil-safe: no native responder,
	// no sink to feed).
	var evidenceSink engine.EndpointEvidenceSetter
	// Native-forward payer mode: EVERY Da Vinci leg forwards to the payer's own endpoint.
	// This block is the payer role's ONLY content occupant in the published binary — with
	// no PAYER_DAVINCI_BASE_URL a ROLE=payer boot fails closed in engine.New (§3.2),
	// because the in-process stand-in responder it used to fall back to is deleted.
	if cfg.PayerDavinciBaseURL != "" {
		if cfg.PayerDavinciTokenURL == "" {
			fmt.Fprintf(stdout, "gateway: WARNING PAYER_DAVINCI_BASE_URL set without PAYER_DAVINCI_TOKEN_URL — forwarding to the payer UNAUTHENTICATED\n")
		}
		pdc, perr := payerDavinciHTTPClient(cfg)
		if perr != nil {
			return b, perr
		}
		if pdc == nil {
			// unauthenticated forward: the template's timeout over the payer's tagged transports
			pdc = cfg.diagnostic.client(&http.Client{Timeout: template.Timeout, Transport: payerTransport(cfg.certs)})
		}
		// FR-G26: the partner's CDS services are chosen per request by the request's hook
		// (engine/workstream_pa_cds_services.go). The listing is read here once, best effort, to report which
		// hook each configured service answers; a listing that cannot be read yet does not
		// stop boot — CRD requests are refused (502) until it can be read. CDS Hooks may
		// live on a different base than the FHIR ops (e.g. br-payer: CDS at root, FHIR
		// under /fhir); cdsBase defaults to the FHIR base when unset (FR-G28).
		cdsBase := cfg.PayerDavinciBaseURL
		if cfg.PayerDavinciCDSBaseURL != "" {
			cdsBase = cfg.PayerDavinciCDSBaseURL
		}
		cdsServices, discErr := engine.DiscoverCDSServices(endpointcert.WithTag(ctx, endpointcert.Of(endpointcert.CDS)), pdc, cdsBase, cfg.PayerDavinciBackendHeaders)
		if discErr != nil {
			fmt.Fprintf(stdout, "gateway: WARNING the payer's CDS service listing is not readable yet (%v); CRD requests are refused until it is\n", discErr)
		} else {
			reportCDSServices(stdout, cdsServices, cfg)
		}
		// WithDeclaredContractVersions is the foreign-endpoint filter: every leg this
		// responder answers goes to the partner endpoint, so the partner's declared set
		// gates all of them (the split counterparty that used to route some legs to a native
		// arm and the rest to an in-process fallback is deleted — §3.2).
		nativeOpts := []engine.NativeOption{
			engine.WithCDSBaseURL(cfg.PayerDavinciCDSBaseURL),
			engine.WithDTRBaseURL(cfg.PayerDavinciDTRBaseURL),
			engine.WithPASBaseURL(cfg.PayerDavinciPASBaseURL),
			engine.WithCRDDispatchService(cfg.PayerDavinciDispatchServiceID),
			engine.WithDeclaredContractVersions(cfg.PayerDavinciContractVersions),
			// A leg whose contract shares no line with the partner's declared
			// set is forwarded as sent unless the participant opts in to
			// refusing it.
			engine.WithRequireSharedLine(cfg.RequireSharedLine),
			// The foreign-peer filter's OWN half is this deployment's declared
			// set — the same accessor selection, the CapabilityStatements and the
			// registry declaration read — never the library build constant.
			engine.WithOwnContractVersions(cfg.ContractVersions),
			// Endpoint evidence: same-origin-drop notes ride the file's existing WARNING-line
			// stdout precedent (e.g. the unauthenticated-forward warning above).
			engine.WithEndpointEvidenceObserver(func(note string) { fmt.Fprintf(stdout, "gateway: %s\n", note) }),
			// The native CRD responder gets the same enforcement policy as the
			// rest of the gateway — a deployed gateway with CONFORMANCE_ENFORCEMENT=none
			// must reach none on this path too, not just the in-process Gateway.
			engine.WithConformancePolicy(engine.NewConformancePolicy(cfg.ConformanceEnforcement)),
		}
		if len(cfg.PayerDavinciBackendHeaders) > 0 {
			nativeOpts = append(nativeOpts, engine.WithBackendHeaders(cfg.PayerDavinciBackendHeaders))
			fmt.Fprintf(stdout, "gateway: partner requests carry %d fixed header(s) (PAYER_DAVINCI_BACKEND_HEADERS)\n", len(cfg.PayerDavinciBackendHeaders))
		}
		// Every call to the payer's own system has a deadline under the
		// requester's leg budget, so a slow system is answered, and recorded,
		// as its own timeout rather than cut short when the requester leaves.
		nativeOpts = append(nativeOpts, engine.WithBackendDeadline(cfg.PayerDavinciBackendTimeout))
		if cfg.payerDavinciBackendTimeoutRaw != "" {
			fmt.Fprintf(stdout, "gateway: partner requests time out after %s (PAYER_DAVINCI_BACKEND_TIMEOUT)\n", cfg.PayerDavinciBackendTimeout)
		}
		if cfg.PayerDavinciBackendCorrelationOff {
			nativeOpts = append(nativeOpts, engine.WithoutBackendCorrelation())
			fmt.Fprintf(stdout, "gateway: partner requests carry no X-Correlation-Id (PAYER_DAVINCI_BACKEND_CORRELATION=off)\n")
		}
		if cfg.diagnostic != nil {
			nativeOpts = append(nativeOpts, engine.WithNativeDiagnostic(cfg.diagnostic.emit))
		}
		// Payer-edge identity mapping seam (gateway/engine/payoredge.go): both env vars
		// set (loadConfig's all-or-nothing rule) ⇒ CRD/DTR/PAS re-stamp the Coverage
		// payor to PayerDavinciPayorBackend. Unset (the
		// default) ⇒ no option appended, seam off, byte-identical to every existing
		// deployment.
		//
		// Which requests this payer owns is decided against the identities this holder
		// itself publishes on the converged /holders feed (reg, kept live by the
		// registrar poller) as well as the configured one: the routing directory is
		// many-to-many, so a provider routes here on ANY published identity and the
		// edge must answer on any of them. Nothing is minted — the feed carries the
		// participant's own attested claims — and the configured value remains an
		// optional override that governs alone when this gateway cannot see its own
		// feed entry.
		if cfg.PayerDavinciPayorEdge {
			nativeOpts = append(nativeOpts,
				engine.WithPayorEdgeIdentity(cfg.PayerDavinciPayorOwn, cfg.PayerDavinciPayorBackend),
				engine.WithPayorEdgePublishedIdentities(reg, bundle.Identity.HolderID,
					func(note string) { fmt.Fprintf(stdout, "gateway: %s\n", note) }),
			)
		}
		// Stated in every state, so a deployment's eligibility posture can be read
		// from its boot line.
		if cfg.PayerEligibilityURL != "" {
			nativeOpts = append(nativeOpts, engine.WithEligibilityURL(cfg.PayerEligibilityURL))
			fmt.Fprintf(stdout, "gateway: PAYER_ELIGIBILITY_URL set — a coverage-eligibility request is carried to the payer's own endpoint and its answer relayed\n")
		} else if cfg.FHIRDataURL == "" {
			fmt.Fprintf(stdout, "gateway: PAYER_ELIGIBILITY_URL unset — with no system of record, a coverage-eligibility request is answered 501 \"coverage eligibility is not offered by this payer\"\n")
		} else {
			fmt.Fprintf(stdout, "gateway: PAYER_ELIGIBILITY_URL unset — coverage eligibility is answered from the payer's records\n")
		}
		nativeOpts = append(nativeOpts, engine.WithCRDService(cfg.PayerDavinciCRDServiceID), engine.WithPASStore(store))
		native := engine.NewNativeResponder(pdc, cfg.PayerDavinciBaseURL, clock, nativeOpts...)
		if discErr == nil {
			native.PrimeCDSServices(cdsServices)
		}
		evidenceSink = native
		gwCfg.Responder = native
		// The native-forward DTR response is a foreign Da Vinci package SHN can't $validate
		// (R-8 near-relay): tell the engine to skip the DTR egress foreign-$validate (FR-G28).
		gwCfg.PayerDavinciNative = true
	}
	// gwOpts are the gateway's options (engine.Option), applied by New.
	var gwOpts []engine.Option
	operatedPopulate := false
	if cfg.ProviderDTRNative || requiresOperatedPopulate(cfg.Role, cfg.OriginationProfile) {
		// Operated-CQL $populate against the provider tenant (the crux of the provider-data
		// and demo lanes — both answer a REAL payer's questionnaire, which the managed
		// filler cannot fill). PROVIDER_DTR_POPULATE_URL is validated at loadConfig.
		// The connector authenticates with its OWN SMART Backend Services block
		// (PROVIDER_DTR_POPULATE_*), never the SoR's; with no block it forwards
		// unauthenticated and says so — the payer block's posture, mirrored.
		if cfg.ProviderDTRPopulateTokenURL == "" {
			fmt.Fprintf(stdout, "gateway: WARNING PROVIDER_DTR_POPULATE_URL set without PROVIDER_DTR_POPULATE_TOKEN_URL — populating UNAUTHENTICATED\n")
		}
		ppc, perr := providerDTRPopulateHTTPClient(cfg)
		if perr != nil {
			return b, perr
		}
		if ppc == nil {
			ppc = cfg.diagnostic.client(kindClient(endpointcert.Populate)) // unauthenticated forward
		}
		gwOpts = append(gwOpts, engine.WithPopulator(engine.NewNativePopulatorWithFailureObserver(ppc, cfg.ProviderDTRPopulateURL, populateFailureObserver(stdout))))
		operatedPopulate = true
	}
	// The provider's own Task endpoint: the payer's attachment request is
	// forwarded there with the client authorized to the provider's own
	// system (loadConfig requires the block with the endpoint).
	if cfg.ProviderCDexTaskURL != "" {
		tc, err := providerCDexHTTPClient(cfg)
		if err != nil {
			return b, fmt.Errorf("gateway: PROVIDER_CDEX_*: %w", err)
		}
		gwCfg.ProviderCDexTaskURL = cfg.ProviderCDexTaskURL
		gwCfg.ProviderCDexTaskClient = tc
	}
	gwCfg.IngressEnabled = cfg.ProviderDavinciIngress || cfg.PayerDavinciIngress
	gwCfg.IngressBaseURL = cfg.IngressBaseURL
	gwCfg.IngressClients = cfg.IngressClients
	gwCfg.ScenarioCallers = cfg.ScenarioCallers
	gwCfg.RequireKnownMembers = cfg.RequireKnownMembers
	gwCfg.RequireSharedLine = cfg.RequireSharedLine
	gwCfg.TranslateIGLines = cfg.TranslateIGLines
	gwCfg.AttachmentCeiling = cfg.AttachmentCeiling
	inflightFrom := inflightFromSetting
	if cfg.AttachmentInflightBytes == 0 {
		cfg.AttachmentInflightBytes, inflightFrom = attachmentInflightDefault(ctx, cfg.AttachmentCeiling, os.ReadFile, getenv(taskMetadataEnv), taskMetadataClient())
	}
	gwCfg.AttachmentInflightBytes = cfg.AttachmentInflightBytes
	gwCfg.EnrichNativeRequests = cfg.EnrichNativeRequests
	gwCfg.FHIRServerRead = cfg.FHIRServerRead
	gwCfg.HubAcceptsInvolved = endpoints.HubAcceptsInvolved
	// Stated in both states: whether this gateway names the other patients a
	// prior-authorization exchange involves is the network's choice, read from
	// its discovery descriptor when the gateway started.
	if endpoints.HubAcceptsInvolved {
		log.Printf("gateway: the network's Hub reads involved patients — each prior-authorization leg names the other patients it involves, identified through this participant's system of record, so the exchange is recorded under each of them")
	} else {
		log.Printf("gateway: the network's Hub does not list involved patients in hubAccepts — legs carry no involved list; restart the gateway after the network starts listing it")
	}
	// Stated in every state, so whether a provider gateway runs module 1, and
	// whom its /scenario routes admit, can be read from its boot line.
	if gwCfg.Role == "provider" {
		if cfg.OriginationProfile == "" {
			log.Printf("gateway: ORIGINATION_PROFILE unset — module 1 is off, and no /scenario route is served")
			if unused := moduleOneSettingsSet(cfg); len(unused) > 0 {
				log.Printf("gateway: WARNING %s set but unused: only module 1 reads them, and it is off (ORIGINATION_PROFILE unset)", strings.Join(unused, ", "))
			}
		} else {
			log.Printf("gateway: %s", scenarioCallersLine(gwCfg.ScenarioCallers))
		}
	}
	// Stated in both states, so a deployment's enrichment posture can be read
	// from its boot line.
	if cfg.EnrichNativeRequests {
		log.Printf("gateway: ENRICH_NATIVE_REQUESTS=true — a Da Vinci-native CRD or questionnaire-package request gains the prefetch, Coverage and Patient it leaves out, read from the system of record")
	} else {
		log.Printf("gateway: ENRICH_NATIVE_REQUESTS=false — a Da Vinci-native CRD or questionnaire-package request is carried as sent; a coverage it leaves out is read only to choose the payer")
	}
	if cfg.ProviderDavinciIngress {
		switch cfg.FHIRServerRead {
		case engine.FHIRServerReadPublic:
			log.Printf("gateway: CDS_FHIR_SERVER_READ=public — a CRD request with no coverage, for a member the system of record does not hold, has its Coverage read through its own fhirServer (a public https server on port 443 only), only to choose the payer; nothing read is carried")
		case engine.FHIRServerReadOff:
			log.Printf("gateway: CDS_FHIR_SERVER_READ=off — the gateway never calls a request's fhirServer; a CRD request with no coverage to route by is refused (412)")
		default:
			log.Printf("gateway: CDS_FHIR_SERVER_READ=private (the default) — a CRD request with no coverage, for a member the system of record does not hold, has its Coverage read through its own fhirServer (an https server in this participant's network or on the internet, never a loopback, link-local or metadata address), only to choose the payer; nothing read is carried; CDS_FHIR_SERVER_READ=off turns it off")
		}
	}
	for _, line := range cfg.ignoredSettingLines {
		log.Print(line)
	}
	if cfg.RequireKnownMembers {
		log.Printf("gateway: REQUIRE_KNOWN_MEMBERS=true — a Da Vinci CRD/DTR/PAS subject the system of record does not hold is refused")
	}
	if cfg.RequireSharedLine {
		log.Printf("gateway: REQUIRE_SHARED_LINE=true — a pair that shares no declared IG line is refused as routing")
	}
	if cfg.AttachmentCeiling < shnsdk.MaxAttachmentBytes {
		log.Printf("gateway: ATTACHMENT_CEILING_BYTES=%d — a documentation attachment submission over it is refused 413 as routing (the network's ceiling is %d)", cfg.AttachmentCeiling, shnsdk.MaxAttachmentBytes)
	}
	log.Printf("gateway: attachment budget %d bytes, %s (a documentation attachment or large envelope that does not fit is refused 503 with Retry-After)", cfg.AttachmentInflightBytes, inflightFrom)
	if cfg.PayerAttachmentMaxBytes != nil {
		log.Printf("gateway: PAYER_ATTACHMENT_MAX_BYTES=%d — the largest single inline attachment this payer's system accepts, stated in its capability declaration", *cfg.PayerAttachmentMaxBytes)
	}
	if cfg.TranslateIGLines {
		log.Printf("gateway: TRANSLATE_IG_LINES=true — a message this gateway builds is translated to a peer's IG line where a certified translation exists, and a questionnaire package answer is carried into this gateway's DTR line's names")
	}

	var accessEmit func(diagnostics.Event) bool
	if cfg.diagnostic != nil {
		gwCfg.Diagnostic = cfg.diagnostic.emit
		gwCfg.DiagnosticTraceKey = cfg.diagnostic.traceKey
		accessEmit = cfg.diagnostic.emit
	}
	// One access line per exchange, always and at every conformance level
	// (access.go); published to the diagnostic sink too when capture is
	// configured.
	access := accessHook(log.Printf, accessEmit, clock)
	// Observer stream: hub + engine callback, only when configured. The demo
	// endpoints (POST /demo/transform, GET /demo/capture/{correlationId})
	// ride the SAME mux via composeObserverHandler — they inherit this
	// branch's OBSERVER_ADDR gate and the loopback-only bind validation
	// already enforced at config load, so there is no separate opt-in or
	// listener for either. The hub itself is created here (gwCfg.Observer
	// must be set before engine.New below), but the mux composition waits
	// until the *engine.Gateway exists, since the capture-fetch endpoint
	// reads that gateway's own edge-capture store.
	var hub *observer.Hub
	if cfg.ObserverAddr != "" {
		hub = observer.NewHub()
		gwCfg.Observer = hub.Emit
	}

	// Metrics: one registry, the one recording point (MON-R16), feeding the
	// built-in per-exchange summary always (/internal/stats, MON-R30), the EMF
	// sink when METRICS_SERVICE is set (EMF rides stdout → awslogs →
	// CloudWatch) and the OpenTelemetry sink when a monitoring setting turns an
	// exporter on; with neither, nothing leaves the gateway (MON-R1). Recording is
	// fire-and-forget and conformance-neutral (TestLegMetric_ConformanceNeutral),
	// and no monitoring setting stops a gateway from booting (MON-R44).
	tel := otelsetup.Build(otelsetup.Load(getenv, otelSettingNames()), otelsetup.Identity{
		Version: healthVersion, HolderID: bundle.Identity.HolderID, Role: cfg.Role,
		MetricsService: cfg.MetricsService, MetricsEnv: getenv("METRICS_ENV"), Hostname: os.Hostname,
	}, otelsetup.Options{Now: clock, Certs: cfg.certs})
	for _, line := range tel.Lines() {
		log.Print(line)
	}
	defer func() {
		if !admitted {
			// A gateway that does not start exports nothing.
			tel.Discard()
		}
	}()
	var sinks []telemetry.Sink
	if cfg.MetricsService != "" {
		emf := telemetry.NewEMF(metrics.New(stdout, cfg.MetricsNamespace, map[string]string{"Env": cfg.MetricsEnv}, nil), cfg.MetricsService, cfg.Role)
		sinks = append(sinks, emf)
		activeStats = activeRequestsMetricLoop(emf, activeRequestsMetricInterval)
	}
	if s := tel.Sink(); s != nil {
		sinks = append(sinks, s)
	}
	// The built-in per-exchange summary is always on (MON-R30), named for
	// this replica as its exported metrics are (MON-R46). It reads only the
	// exchange and backend instruments, so the other hooks, and their loops,
	// run only when something exports.
	exported := len(sinks) > 0
	exchangeStats := stats.New(tel.ServiceInstanceID(), clock)
	sinks = append(sinks, exchangeStats)
	metricsReg := telemetry.New(sinks...)
	tel.Bind(metricsReg)
	exchangeMetrics := exchangeMetricHook(metricsReg)
	var auditM auditappend.Metrics = auditMetrics{reg: metricsReg}
	if exported {
		gwCfg.LegMetric = legMetricHook(metricsReg)
		storeErr := storeErrorMetricHook(metricsReg)
		gwCfg.StoreErrorMetric = storeErr
		gwCfg.InvolvedMetric = involvedOmittedMetricHook(metricsReg)
		gwCfg.TokenMetric = tokenMetricHook(metricsReg)
		// The exchange additions (MON-R23, MON-R24).
		gwCfg.ActiveRequestMetric = activeRequestHook(metricsReg)
		gwCfg.FindingMetric = findingMetricHook(metricsReg)
		gwCfg.ValidatorMetric = validatorMetricHook(metricsReg)
		gwCfg.ObserveCheckDroppedMetric = observeCheckDroppedHook(metricsReg)
		// The validator lanes: every default lane's qualification attempt, and
		// every lane's state as a gauge, so a lane left unqualified is a number
		// an alarm reads, not a log line.
		attempted := validatorLaneAttemptHook(metricsReg)
		lanes.attempted.Store(&attempted)
		laneStats = validatorLaneMetricLoop(metricsReg, func() []laneGauge {
			return laneGauges(lanes, gwCfg.CertificationValidatorsByLine, payloadChecksConsumeLanes(cfg.ConformanceEnforcement))
		}, validatorLaneMetricInterval)
		opsStats = opsMetricLoop(metricsReg, opsSources{
			info:  infoPoints(gatewayInfoWire, cfg.ContractVersions, cfg.ConformanceEnforcement.String()),
			certs: cfg.certs, keys: keyTimes, feed: feed, now: clock,
		}, opsMetricInterval)
		// One counter, one `store` dimension, whichever layer noticed the failure.
		for _, wire := range storeErrHooks {
			wire(storeErr)
		}
		if pool != nil {
			// Saturation of the one pool every seam shares is what turns a healthy
			// database into expiring store contexts; it is invisible without this.
			p := pool
			poolStats = storePoolMetricLoop(metricsReg, func() storePoolStat {
				st := p.Stat()
				return storePoolStat{
					TotalConns:        st.TotalConns(),
					AcquiredConns:     st.AcquiredConns(),
					IdleConns:         st.IdleConns(),
					EmptyAcquireCount: st.EmptyAcquireCount(),
				}
			}, storePoolMetricInterval)
		}
	}

	// Every consumer of the per-exchange record joins here.
	gwCfg.ExchangeObserved = exchangeObservers(access, exchangeMetrics)

	// gw is held as a named variable (rather than the previous inline
	// engine.New(gwCfg).Handler()) so the observer mux composed just below
	// can wire the demo capture-fetch endpoint against this SAME gateway
	// instance's own edge-capture store.
	gwCfg.CertificationValidatorsByLine, err = certificationValidators(getenv, cfg, firstNonEmpty(cfg.FHIRValidateURL, endpoints.FHIRValidate), lanes.defaults, qualify)
	if err != nil {
		return b, err
	}
	gw, err := engine.New(gwCfg, gwOpts...)
	if err != nil {
		// No worker will ever own the gated clients: close them (nothing has
		// started their qualification loops, which start with the worker).
		engine.CloseCertificationClients(gwCfg.CertificationValidatorsByLine)
		return b, err
	}

	defer func() {
		if !admitted {
			_ = gw.Close()
		}
	}()
	var obsHandler http.Handler
	if hub != nil {
		obsHandler = composeObserverHandler(hub.HandlerWithBarrier(gw.WaitObserverCompletion), gw, cfg.DemoEdgeCapture)
	}

	tlsCert, err := loadTLSCert(cfg.TLSCertFile, cfg.TLSKeyFile)
	cfg.certs.RecordOwn(endpointcert.Ingress, "TLS_CERT_FILE", tlsCert)
	if err != nil {
		return b, err
	}
	scheme := "http"
	if tlsCert != nil {
		scheme = "https"
	}

	fmt.Fprintf(stdout, "gateway: role=%s holder=%s listening on %s://%s\n", cfg.Role, bundle.Identity.HolderID, scheme, cfg.Addr)
	if cfg.ContractVersionsDerived {
		// Peers select against the registry entry, not this process, so the
		// operator must see which set the entry has to declare.
		fmt.Fprintf(stdout, "gateway: declaring %s, derived from PAYER_DAVINCI_CONTRACT_VERSIONS (SHN_CONTRACT_VERSIONS is unset); the registry entry peers select this gateway against must declare the same set\n", strings.Join(cfg.ContractVersions, ", "))
	}
	// The resolved level, on the operator log at boot: a fleet check reads it here
	// rather than from the task definition, where an absent value and a delivered
	// value the binary ignored would look the same.
	if cfg.ConformanceEnforcementUnset {
		fmt.Fprintf(stdout, "gateway: conformance enforcement=%s (CONFORMANCE_ENFORCEMENT unset)\n", cfg.ConformanceEnforcement)
	} else {
		fmt.Fprintf(stdout, "gateway: conformance enforcement=%s\n", cfg.ConformanceEnforcement)
	}
	// The payers tolerated, likewise: none is the default, and an operator sees which.
	tolerated := "none"
	if len(cfg.ConformanceToleratePeers) > 0 {
		ids := make([]string, len(cfg.ConformanceToleratePeers))
		for i, p := range cfg.ConformanceToleratePeers {
			ids[i] = p.System + "|" + p.Value
		}
		tolerated = strings.Join(ids, ",")
	}
	fmt.Fprintf(stdout, "gateway: conformance tolerated peers=%s\n", tolerated)

	// /internal/* is the operator/control-plane surface: never forwarded at the
	// hosted edge (the hosted control plane pins that), token- or loopback-gated
	// here. Inserted at this seam — shared by every role — rather
	// than the per-role engine mux.
	// The audit outbox: the gateway's own records, checked,
	// signed with its holder key, queued and sent to the Audit Plane. Nothing
	// appends yet.
	auditAppender, err := auditappend.New(auditappend.Config{
		Holder: bundle.Identity.HolderID, SignPriv: bundle.Identity.SignPriv,
		Endpoint: endpoints.Audit, Store: auditStore, Client: kindClient(endpointcert.Audit), Metrics: auditM, Now: clock, Jitter: rand.Float64,
		Logf: log.Printf,
	})
	if err != nil {
		return b, fmt.Errorf("gateway: audit outbox: %w", err)
	}
	if !auditappend.HolderOK(bundle.Identity.HolderID) {
		log.Printf("gateway: audit: holder id is outside the shape an audit record allows; every record would be refused (counted dropped{invalid})")
	}
	outbox := "in-process, lost at restart"
	if auditStore.Durable() {
		outbox = "postgres"
	}
	if auditAppender.EndpointKnown() {
		log.Printf("gateway: audit: records go to %s (outbox: %s)", checksTargetOf(endpoints.Audit), outbox)
	} else {
		log.Printf("gateway: audit: discovery names no audit endpoint; records are queued, not sent (outbox: %s; see /internal/checks AUDIT_ENDPOINT)", outbox)
	}

	// The telemetry targets are local (KindLocal): they read cached state and
	// dial nothing, so they carry no client of their own.
	checksRunner := checks.NewRunner(append(tagCheckTargets(cfg.certs, append(append(checkTargets(cfg), auditCheckTarget(endpoints.Audit)), lanes.checkTargets()...), template), telemetryCheckTargets(tel, tel.Endpoint())...), template, clock)
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
	var handler http.Handler = mainListener(checksRunner, cfg.ChecksToken, exchangeStats.Handler(), health.Wrap(hreg, gw.Handler()))
	// The HTTP server duration (MON-R29), only when something exports: the
	// per-exchange summary does not read it.
	httpServerMetered := false
	if exported {
		handler = httpServerMetrics(metricsReg, clock, handler)
		httpServerMetered = true
	}

	b = built{
		httpServerMetered:    httpServerMetered,
		requireKnownMembers:  gwCfg.RequireKnownMembers,
		requireSharedLine:    gwCfg.RequireSharedLine,
		attachmentCeiling:    gwCfg.AttachmentCeiling,
		attachmentInflight:   gwCfg.AttachmentInflightBytes,
		translateIGLines:     gwCfg.TranslateIGLines,
		enrichNativeRequests: gwCfg.EnrichNativeRequests,
		fhirServerRead:       gwCfg.FHIRServerRead,
		operatedPopulate:     operatedPopulate,
		responder:            gwCfg.Responder,

		diagnostic:      cfg.diagnostic,
		gateway:         gw,
		addr:            cfg.Addr,
		handler:         handler,
		reg:             reg,
		registrarURL:    registrarURL,
		identity:        bundle.Identity,
		capabilities:    capabilityDeclaration(cfg),
		declared:        declared,
		client:          registrarClient,
		certs:           cfg.certs,
		tlsCert:         tlsCert,
		observerAddr:    cfg.ObserverAddr,
		observerHandler: obsHandler,
		healthCell:      feedCell,
		feed:            feed,
		checksRunner:    checksRunner,
		nativeResponder: evidenceSink,
		keyRefresh:      keyRefresh,
		closeStore:      closeStore,
		lanes:           lanes,
		poolStats:       poolStats,
		auditAppender:   auditAppender,
		laneStats:       laneStats,
		opsStats:        opsStats,
		activeStats:     activeStats,
		telemetry:       tel,
		certification:   gwCfg.CertificationValidatorsByLine,
		// Certification evidence is collected only where payload checks consume
		// lanes (the engine starts no certification worker otherwise), so only
		// then is there anything to warm.
		warmCertification: payloadChecksConsumeLanes(gwCfg.ConformanceEnforcement),
	}
	admitted = true
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
	// The diagnostic publisher stops last: after the listener has shut down
	// and the gateway has closed, so the events their final exchanges emit
	// are delivered by its drain.
	stopDiagnostic := b.startDiagnostic(ctx)
	defer stopDiagnostic()
	// The final metrics export runs just before it: after the listener, the
	// cut handlers, the audit outbox's drain and the observe flush, so it
	// carries what they counted (MON-R14).
	defer b.telemetry.Shutdown(context.Background())
	defer func() {
		_ = b.gateway.Close()
		b.lanes.Close()
		if b.closeStore != nil {
			b.closeStore()
		}
	}()

	// Stop and join Run-owned workers on every exit, including listener errors,
	// before the deferred gateway/lane/store cleanup releases their dependencies.
	stopWorkers := b.startWorkers(ctx)
	defer stopWorkers()
	errc := make(chan error, 2)
	// Requests in flight are counted, so a stop that has to cut them can let
	// their handlers return, and emit their last events, before the
	// diagnostic publisher drains.
	var handlers sync.WaitGroup
	main := serverFor(b.addr, countHandlers(b.handler, &handlers), b.tlsCert)
	defer main.Close()
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
		observer := &http.Server{Addr: b.observerAddr, Handler: b.observerHandler}
		defer observer.Close()
		go func() { errc <- observer.ListenAndServe() }()
	}
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		return stopServing(main, &handlers, listenerShutdownWait, cutHandlerWait)
	}
}

// The first two steps of a stop (DEPLOYMENT.md, "Stopping the gateway"):
// listenerShutdownWait bounds how long the listener waits for the requests in
// flight, and cutHandlerWait how long a stop then waits for the handlers of
// the requests it cut to return.
const (
	listenerShutdownWait = 5 * time.Second
	cutHandlerWait       = 2 * time.Second
)

// countHandlers counts h's requests in flight in handlers.
func countHandlers(h http.Handler, handlers *sync.WaitGroup) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers.Add(1)
		defer handlers.Done()
		h.ServeHTTP(w, r)
	})
}

// stopServing shuts srv down, waiting up to grace for the requests in
// flight. Past it, it cuts them and waits up to cutWait for their handlers
// (counted in handlers) to return, so the events they emit on the way out
// are queued before the diagnostic publisher, stopped after this, drains.
// It returns Shutdown's error.
func stopServing(srv *http.Server, handlers *sync.WaitGroup, grace, cutWait time.Duration) error {
	shutdown, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	err := srv.Shutdown(shutdown)
	if err != nil {
		_ = srv.Close()
		returned := make(chan struct{})
		go func() { handlers.Wait(); close(returned) }()
		select {
		case <-returned:
		case <-time.After(cutWait):
		}
	}
	return err
}

// startDiagnostic runs the diagnostic publisher until the returned stop,
// which ends it and waits for its drain. The publisher does not end with ctx:
// a caller stops it once nothing more can emit.
func (b built) startDiagnostic(ctx context.Context) func() {
	if b.diagnostic == nil {
		return func() {}
	}
	pubctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() { defer close(done); b.diagnostic.run(pubctx) }()
	return func() { cancel(); <-done }
}

// startWorkers owns the goroutines that exist only while Run serves. Its
// cleanup cancels even when the caller's context remains live (a listener error)
// and joins in-flight requests before their dependencies can be released.
func (b built) startWorkers(parent context.Context) func() {
	ctx, cancel := context.WithCancel(parent)
	var workers sync.WaitGroup
	start := func(run func(context.Context)) {
		workers.Go(func() { run(ctx) })
	}
	if b.registrarURL != "" {
		start(func(ctx context.Context) {
			pollFeed(ctx, b.client, b.registrarURL, b.reg, 3*time.Second, b.healthCell, b.feed)
		})
		start(func(ctx context.Context) {
			declareCapabilities(ctx, b.client, b.registrarURL, b.identity, b.capabilities, declareCapabilitiesRetry, b.declared)
		})
	}
	// Shared ingress-key reload: non-nil only under SHN_STORE_DATABASE_URL, and
	// started here rather than in build for the same reason as the poller above —
	// the boot gate drives build() and must not inherit a ticker.
	if b.keyRefresh != nil {
		start(b.keyRefresh)
	}
	// Shared-state pool stats: non-nil only with both a store DSN and METRICS_SERVICE,
	// started here and not in build for the same reason.
	if b.poolStats != nil {
		start(b.poolStats)
	}
	if b.laneStats != nil {
		start(b.laneStats)
	}
	if b.opsStats != nil {
		start(b.opsStats)
	}
	if b.activeStats != nil {
		start(b.activeStats)
	}
	// The monitoring export loop (MON-R4): a no-op when nothing is exported.
	start(b.telemetry.Run)
	// The Prometheus pull listener (MON-R6), on its own bind: a no-op when
	// pull is off, and a bind that fails turns pull off, never the gateway
	// (MON-R44).
	start(b.telemetry.ServePull)
	// Boot-time connectivity checks run without blocking listener startup;
	// build() must return without waiting on partner endpoints.
	start(func(ctx context.Context) { _, _ = b.checksRunner.Run(ctx) }) // results land in Last()
	// The certification warm-up likewise runs beside the listener, never before
	// it (warmCertification).
	if b.warmCertification {
		start(func(ctx context.Context) { warmCertification(ctx, b.certification, b.certs) })
	}
	// The audit outbox's sender. It ends with the other workers at the stop
	// signal; Close runs in the cleanup below, which Run calls only after the
	// listener has drained (stopServing), so an in-memory outbox is drained and
	// counted once nothing more can append.
	if b.auditAppender != nil {
		start(b.auditAppender.Run)
	}
	return func() {
		cancel()
		workers.Wait()
		if b.auditAppender != nil {
			b.auditAppender.Close()
		}
	}
}

// managedHandler retains the complete runtime's cleanup without changing the
// public constructors' http.Handler return type. Close is idempotent and joins
// qualification workers before releasing the shared-state pool.
type managedHandler struct {
	http.Handler
	once  sync.Once
	close func()
}

func (h *managedHandler) Close() error {
	h.once.Do(h.close)
	return nil
}

// HandlerForTest builds the complete runtime and returns a cleanup that cancels
// and joins default-lane workers. Call cleanup before closing test dependencies.
func HandlerForTest(ctx context.Context, getenv func(string) string, stdout io.Writer) (http.Handler, func(), error) {
	return HandlerForTestWithClock(ctx, getenv, stdout, time.Now)
}

// HandlerForTestWithClock adds deterministic time to HandlerForTest while retaining
// its explicit cleanup barrier. Cleanup is safe to call more than once.
func HandlerForTestWithClock(ctx context.Context, getenv func(string) string, stdout io.Writer, clock func() time.Time) (http.Handler, func(), error) {
	h, err := HandlerWithClock(ctx, getenv, stdout, clock)
	if err != nil {
		return nil, nil, err
	}
	return h, func() { _ = h.(io.Closer).Close() }, nil
}

// HandlerWithClock is Handler with an injected clock. The engine's per-op/per-hop
// authority (assertion issuance + expiry, VerifyBound) is time-sensitive, so a
// hermetic test must align the gateway with its peers' clock. The returned handler
// implements io.Closer: the caller must Close it after stopping HTTP service and
// before releasing dependencies, to cancel and join default-lane workers.
func HandlerWithClock(ctx context.Context, getenv func(string) string, stdout io.Writer, clock func() time.Time) (http.Handler, error) {
	b, err := build(ctx, getenv, stdout, clock)
	if err != nil {
		return nil, err
	}
	stopDiagnostic := b.startDiagnostic(ctx)
	return &managedHandler{Handler: b.handler, close: func() {
		_ = b.gateway.Close()
		b.lanes.Close()
		if b.closeStore != nil {
			b.closeStore()
		}
		// The final metrics export, after everything that counts (MON-R14).
		b.telemetry.Shutdown(context.Background())
		// Last, so the events emitted while the gateway and its lanes close
		// are delivered by the publisher's drain.
		stopDiagnostic()
	}}, nil
}

// Handler builds the full public runtime without serving. The returned handler
// implements io.Closer; embedders must close it after stopping HTTP service.
// Tests may instead use HandlerForTest's explicit cleanup callback. Run owns its
// own lifecycle and serves the production process.
func Handler(ctx context.Context, getenv func(string) string, stdout io.Writer) (http.Handler, error) {
	return HandlerWithClock(ctx, getenv, stdout, time.Now)
}

// validatorLanesForDeclared builds the per-LINE $validate lane map and
// FAILS CLOSED when a declared line has no lane, at every level but none.
//
// The rule, and why it is fail-closed: a HAPI instance can host exactly one
// version of an IG, so validating a 2.2 payload against a 2.0 lane does not
// "mostly work" — it reports errors (or passes) for reasons unrelated to the
// payload. A deployment that DECLARES a line is telling peers it can produce and
// answer at that line; FR-36 says everything it produces is validated. Without a
// lane those two promises cannot both hold, so the gateway refuses to start
// rather than quietly validate against the wrong IG (FR-36/FR-G29). At none
// it validates nothing, so the line is served unlaned instead.
//
// canonical is the already-resolved canonical-lane validator (selectValidator's
// result; nil at none with no FHIR_VALIDATE_URL). Under SHN_FAKE_VALIDATOR=1 each line gets its own structural fake
// validator with the line-specific cardinality and binding checks.
// Scope: only MULTI-LINE contracts (pa.crd/pa.dtr/pa.pas — those whose native set
// carries more than one line) can produce line-varying payloads and therefore need
// per-line lanes. A single-line contract has nothing to choose between and rides
// the canonical lane. No native contract is single-line today (pa.pdex was, until
// no gateway declared it), so ParseDeclaredContractVersions refuses any declared
// token this branch would alias. That scoping is DERIVED from
// NativeContractVersions(), not hardcoded, so a contract that gains a second line
// automatically starts demanding lanes.
func validatorLanesForDeclared(getenv func(string) string, declared []string, canonical shnsdk.Validator, cfg config) (map[string]shnsdk.Validator, error) {
	fake := fakeValidator(getenv)
	// The canonical line — the one FHIR_VALIDATE_URL has always served — is the line
	// this build DEFAULT-declares for its multi-line contracts.
	canonicalLine := shnsdk.LineOf(engine.CanonicalContractVersion())
	urlForLine := map[string]string{"2.1": cfg.FHIRValidateURL21, "2.2": cfg.FHIRValidateURL22}

	linesPerContract := map[string]map[string]bool{}
	for _, tok := range engine.NativeContractVersions() {
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
			v, err := lineFakeValidator(line)
			if err != nil {
				return nil, err
			}
			lanes[line] = v
		case line == canonicalLine && canonical != nil:
			lanes[line] = canonical
		case urlForLine[line] != "":
			lanes[line] = taggedValidator(cfg.certs, line, urlForLine[line])
		case !payloadChecksConsumeLanes(cfg.ConformanceEnforcement):
			// No payload check consumes a lane, so a declared line needs none to
			// be built and answered at: it is served unlaned (the engine's
			// servesLine), and no IG line is bridged to or from it without one.
			continue
		default:
			// The refusal names the env to SET, or the level that checks nothing
			// (FR-G29), never the test double: SHN_FAKE_VALIDATOR is a hermetic-test
			// opt-in, and an operator reading a production boot failure must not be
			// handed a validator that only appears to validate.
			envName := "FHIR_VALIDATE_URL_" + strings.ReplaceAll(line, ".", "_")
			source := "SHN_CONTRACT_VERSIONS"
			if cfg.ContractVersionsDerived {
				source = "the declaration derived from PAYER_DAVINCI_CONTRACT_VERSIONS (SHN_CONTRACT_VERSIONS is unset)"
			}
			return nil, fmt.Errorf("gateway: %s declares %s but no FHIR validator lane is configured for line %s: set %s to a $validate endpoint hosting that line's IG packages (one HAPI hosts exactly one version of an IG) — refusing to declare a line this gateway cannot validate (FR-36/FR-G29); "+noneRemedy, source, tok, line, envName)
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
	// configured lane still refuses boot at every level but none.
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
				v, err := lineFakeValidator(line)
				if err != nil {
					return nil, err
				}
				lanes[line] = v
			case line == canonicalLine && canonical != nil:
				lanes[line] = canonical
			case urlForLine[line] != "":
				lanes[line] = taggedValidator(cfg.certs, line, urlForLine[line])
			default:
				// No env configured for this undeclared native line — stays
				// absent from the map (its contract lines resolve unlaned,
				// exactly as before D1a). This is an OPT-IN, never a
				// requirement to lane every native line.
			}
		}
	}
	// A single-line contract's line may be absent from lanes (a line not otherwise
	// declared; no native contract is single-line today). The engine treats an ABSENT contract line in a
	// non-empty lane map as unlaned, so map it to the canonical lane explicitly.
	for _, tok := range declared {
		contract, line, ok := strings.Cut(tok, "@")
		if ok && line != "" && len(linesPerContract[contract]) < 2 && lanes[line] == nil && canonical != nil {
			lanes[line] = canonical
		}
	}
	return lanes, nil
}

// payloadChecksConsumeLanes reports whether the gateway's payload conformance
// checks use validator lanes at all: every level but none, which checks no
// payload. Boot reads it wherever a missing lane is refused, waited for or
// warmed. It answers as the engine's own predicate of the same name does
// (TestPayloadChecksConsumeLanes in both packages pins the same table).
func payloadChecksConsumeLanes(level engine.ConformanceEnforcement) bool {
	return level != engine.EnforcementNone
}

// selectValidator is the FAIL-CLOSED validator decision (pure, unit-tested
// directly): explicit fake opt-in in a test build (testdouble.go) → fake; else a
// resolved URL → real $validate; else, where no payload check consumes a lane
// (none), no validator (nil, nil): such a gateway checks no payload and bridges
// no line it has no lane for (FR-G29); else ERROR (never a silent fake
// fallback — FR-36).
func selectValidator(getenv func(string) string, validatorURL string, level engine.ConformanceEnforcement) (shnsdk.Validator, error) {
	switch {
	case fakeValidator(getenv):
		return lineFakeValidator(shnsdk.LineOf(engine.CanonicalContractVersion()))
	case validatorURL != "":
		return shnsdk.NewOperationValidator(validatorURL), nil // $validate wrapper (NOT a thin HTTP validator)
	case !payloadChecksConsumeLanes(level):
		return nil, nil
	default:
		return nil, errors.New("no FHIR validator URL (not in /discovery, not in env): refusing to run without per-message validation (FR-36); set FHIR_VALIDATE_URL to an IG-loaded $validate endpoint, " + noneRemedy)
	}
}

// noneRemedy ends every boot refusal for a missing validator: the one level that
// needs none. A gateway at none checks no payload, certifies nothing it builds
// and bridges no IG line without a lane, so it claims nothing a validator would
// have to back.
const noneRemedy = "or set CONFORMANCE_ENFORCEMENT=none, or leave it unset, to check no payload (this gateway then needs no validator)"

// noValidatorLog is the boot line of a gateway with no validator anywhere: no
// canonical validator, no per-line lane and no default lane. Only none boots so.
const noValidatorLog = "gateway: no FHIR validator: conformance enforcement is none, and no FHIR_VALIDATE_URL (env or /discovery), FHIR_VALIDATE_URL_<line> or default validator lane is configured: " +
	"no payload is checked, each declared line is served unlaned, and no IG line is bridged"

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
		reg.Set(h.ID, shnsdk.RegistryEntry{ID: h.ID, Role: h.Role, EncPub: encPub, SignPub: signPub, BaseURL: h.BaseURL, PayerIDs: h.PayerIDs, ProviderIDs: h.ProviderIDs, MessageFrames: h.MessageFrames, ContractVersions: h.ContractVersions, RequestFrames: h.RequestFrames, Legs: h.Legs, AttachmentMaxBytes: h.AttachmentMaxBytes})
		n++
	}
	return n, nil
}

// declareCapabilitiesRetry is the first wait before a failed declaration is
// sent again; it doubles to declareCapabilitiesMaxRetry.
const (
	declareCapabilitiesRetry    = 15 * time.Second
	declareCapabilitiesMaxRetry = 10 * time.Minute
	// declareCapabilitiesAbsentTries is how many 404s or 405s pass before the
	// gateway says so once and falls to a try an hour: the holder may be a moment
	// from registered, or the registrar older than the declaration. Those tries
	// never end, because a registrar updated later reads a gateway that stopped
	// as one that takes no marked leg, and the Hub would refuse it until someone
	// restarted it.
	declareCapabilitiesAbsentTries = 4
	declareCapabilitiesSlowFactor  = 240 // of the first wait: an hour
)

// capabilityDeclaration is what this gateway's system takes as a recipient: the
// network's legs it compiles for its role, and each one its role takes only
// once its participant configures it whose setting is set (engine.TakenLegs:
// the payer's attachment request, with PROVIDER_CDEX_TASK_URL); and, for a
// payer, the request frames it serves (declaredRequestFrames).
func capabilityDeclaration(cfg config) shnsdk.CapabilityDeclaration {
	return shnsdk.CapabilityDeclaration{
		Legs:               networkLegs(engine.Config{Role: cfg.Role, ProviderCDexTaskURL: cfg.ProviderCDexTaskURL}.TakenLegs()),
		AttachmentMaxBytes: cfg.PayerAttachmentMaxBytes,
		RequestFrames:      declaredRequestFrames(cfg.Role),
	}
}

// declaredRequestFrames is the request frames a gateway of role declares as
// its own: a payer's gateway serves every frame this build decodes and
// v1opsub, since it tags the adaptive addresses it issues, reads a round's
// operationSubpath and restores the address (E-13). A provider's gateway
// declares none of its own (nil, sent as nothing), so its registration's set
// stands. A gateway rolled back to a release that declares none gives back,
// at its first start, the set this one declared.
func declaredRequestFrames(role string) []string {
	if role != "payer" {
		return nil
	}
	return append(shnsdk.SupportedRequestFrames(), shnsdk.RequestFrameV1OpSub)
}

// networkLegs is legs without any leg the SDK does not name: a test profile's,
// which the network does not route. The release check holds every leg of a
// released gateway to the SDK it pins, so a released gateway drops none.
func networkLegs(legs []string) []string {
	return slices.DeleteFunc(legs, func(leg string) bool {
		_, ok := shnsdk.FlatTransactionType(leg)
		return !ok
	})
}

// nextDeclareRetry is the wait after retry: twice as long, to the cap, or slow
// once the tries have slowed.
func nextDeclareRetry(retry time.Duration, slowed bool, slow time.Duration) time.Duration {
	if slowed {
		return slow
	}
	return min(2*retry, declareCapabilitiesMaxRetry)
}

// declareCapabilities declares d to the registrar, as this gateway's system does
// each time it starts, and sends it again after a failure until the registrar
// has it or refuses it with a 409. A failure never stops the gateway: until the
// declaration lands, a peer reads what the registrar holds. A 409 is final: the
// registrar holds a declaration issued no earlier, so a gateway whose clock is
// behind the last declarer's waits for its next start, though a later resend
// could land; or the holder was founded with the network and the registrar is
// older than the one that applies such a holder's declaration. For a holder
// founded with the network, a current registrar applies the declared request
// frames only, and its legs and limit stay those it was founded with. After
// declareCapabilitiesAbsentTries answers of 404 or 405 it logs once and tries
// hourly, in silence: the holder is not registered, or the registrar is older
// than the declaration, and until one lands the gateway runs as one that declared
// nothing.
func declareCapabilities(ctx context.Context, c *http.Client, registrarURL string, id shnsdk.Identity, d shnsdk.CapabilityDeclaration, retry time.Duration, cell *declarationCell) {
	answered := &answerStatus{next: c.Transport}
	client := *c
	client.Transport = answered
	absent, slowed, slow := 0, false, declareCapabilitiesSlowFactor*retry
	for {
		_, err := id.DeclareCapabilities(ctx, &client, registrarURL, d)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			cell.set(declarationStored)
			return
		}
		if answered.status == http.StatusConflict {
			cell.set(declarationNotApplied)
			log.Printf("gateway: capability declaration not applied, not resending: %v", err)
			return
		}
		cell.set(declarationFailing)
		noRoute := answered.status == http.StatusNotFound || answered.status == http.StatusMethodNotAllowed
		if noRoute {
			if absent++; absent == declareCapabilitiesAbsentTries {
				slowed, retry = true, slow
				log.Printf("gateway: capability declaration not applied, trying hourly: this holder is not registered, or the registrar takes no declaration: %v", err)
			}
		}
		switch {
		case !slowed || !noRoute:
			// Any other failure is said, at the slow pace too: it is not the
			// absent route the tries slowed for.
			if slowed {
				log.Printf("gateway: capability declaration not stored: %v", err)
			} else {
				log.Printf("gateway: capability declaration not stored, retrying in %s: %v", retry, err)
			}
		default:
			cell.set(declarationNoRoute)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
		retry = nextDeclareRetry(retry, slowed, slow)
	}
}

// What a capability declaration came to, as /health states it. A failure is the
// check's lastError, a coarse class.
const (
	declarationPending    = ""
	declarationStored     = "stored"
	declarationNotApplied = "not-applied"
	declarationNoRoute    = "registrar-takes-no-declaration"
	declarationFailing    = "failing"
)

// declarationCell is the /health check of the capability declaration. Its
// status is always ok: a gateway a registrar has not taken a declaration from
// serves as before, and on a network whose registrar is older than the
// declaration every gateway would otherwise read unhealthy. lastError says it was
// not stored, and lastSuccess when it was.
type declarationCell struct {
	mu     sync.Mutex
	state  string
	stored time.Time
}

// set records state. c may be nil, as pollFeed's cell may.
func (c *declarationCell) set(state string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = state
	if state == declarationStored {
		c.stored = time.Now()
	}
}

// Check is the cell's health.CheckFunc.
func (c *declarationCell) Check(context.Context) health.Check {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := health.Check{Name: "capability-declaration", Status: health.StatusOK}
	switch c.state {
	case declarationPending:
	case declarationStored:
		out.LastSuccess = c.stored.UTC().Format(time.RFC3339)
	default:
		out.LastError = c.state
	}
	return out
}

// answerStatus is a transport that keeps the status of the last answer: the
// SDK's error for a refused declaration is text, and a refusal is read by its
// status, never by that text. One goroutine uses it.
type answerStatus struct {
	next   http.RoundTripper
	status int
}

func (a *answerStatus) RoundTrip(r *http.Request) (*http.Response, error) {
	a.status = 0
	resp, err := a.next.RoundTrip(r)
	if err == nil {
		a.status = resp.StatusCode
	}
	return resp, err
}

// registryRefresh is the engine's on-miss registry refresh: one read of the
// registrar feed into reg, the same read pollFeed makes on its interval. nil
// without a registrar (a static registry has nothing to re-read). It and the
// poller may converge reg at the same time; a read older than the other's
// writes is overwritten again by the next poll.
func registryRefresh(c *http.Client, registrarURL string, reg shnsdk.Registry, feed *feedFreshness) func(context.Context) error {
	if registrarURL == "" {
		return nil
	}
	return func(ctx context.Context) error {
		n, err := convergeRegistry(ctx, c, registrarURL, reg)
		if err == nil {
			feed.succeeded(n)
		}
		return err
	}
}

// pollFeed re-converges the registry on an interval (post-boot peer registration/
// rotation). cell records each attempt/outcome onto the service's /health
// registrar-poller check — this is this path's first error visibility EVER:
// a transient feed error used to vanish silently
// (best-effort), now it surfaces as a degraded check until the next success.
// cell may be nil (registrarURL configured post-boot is not a supported path
// today, but PollerCell's Record* methods are nil-safe regardless).
func pollFeed(ctx context.Context, c *http.Client, registrarURL string, reg shnsdk.Registry, every time.Duration, cell *health.PollerCell, feed *feedFreshness) {
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
				feed.succeeded(n)
			}
		}
	}
}

// fhirHTTPClient builds the HTTP client for the FHIR SoR connector. When the SMART
// credential block (FHIR_TOKEN_URL + FHIR_CLIENT_ID + ...) is set it authenticates
// per the configured mode: RFC 7523 signed-JWT client-credentials (private_key_jwt,
// preferred) or client_secret_post when FHIR_CLIENT_SECRET is set; else nil ⇒
// unauthenticated (the zero-config default). Mirrors cmd/gateway/main.go's connector branch.
func fhirHTTPClient(cfg config) (*http.Client, error) {
	if cfg.FHIRTokenURL == "" {
		return nil, nil // unauthenticated (the zero-config default)
	}
	sc, err := fhirSmartConfig(cfg)
	if err != nil {
		return nil, err
	}
	cfg.diagnostic.smart(&sc)
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
// is set; else nil ⇒ unauthenticated (a deliberate zero-config demo posture).
func payerDavinciHTTPClient(cfg config) (*http.Client, error) {
	if cfg.PayerDavinciTokenURL == "" {
		return nil, nil // unauthenticated (deliberate; warned at build)
	}
	sc, err := payerDavinciSmartConfig(cfg)
	if err != nil {
		return nil, err
	}
	cfg.diagnostic.smart(&sc)
	hc, err := smartauth.NewHTTPClient(sc)
	if err != nil {
		return nil, fmt.Errorf("payer-davinci smartauth client: %w", err)
	}
	return hc, nil
}

// providerDTRPopulateHTTPClient returns the client the operated $populate connector
// posts with. When the PROVIDER_DTR_POPULATE SMART credential block is set it
// authenticates per the configured mode (private_key_jwt or client_secret_post),
// through the SAME smartauth client the SoR and payer connectors use — this package
// never hand-rolls a second OAuth client; else nil ⇒ unauthenticated (warned at
// the populator wiring site). A token refusal surfaces as the populate request's
// error, which the engine maps to its upstream failure (consumer 502): there is no
// unauthenticated fallback.
func providerDTRPopulateHTTPClient(cfg config) (*http.Client, error) {
	if cfg.ProviderDTRPopulateTokenURL == "" {
		return nil, nil // unauthenticated (deliberate; warned at build)
	}
	sc := providerDTRPopulateSmartBase(cfg)
	if cfg.ProviderDTRPopulateClientSecret != "" {
		sc.ClientSecret = cfg.ProviderDTRPopulateClientSecret // client_secret_post; no key material
	} else {
		key, err := loadSmartKey(cfg.ProviderDTRPopulateClientKey, cfg.ProviderDTRPopulateClientAlg)
		if err != nil {
			return nil, fmt.Errorf("load populate client key: %w", err)
		}
		sc.Alg, sc.Key, sc.KID = cfg.ProviderDTRPopulateClientAlg, key, cfg.ProviderDTRPopulateClientKID
	}
	cfg.diagnostic.smart(&sc)
	hc, err := smartauth.NewHTTPClient(sc)
	if err != nil {
		return nil, fmt.Errorf("populate smartauth client: %w", err)
	}
	return hc, nil
}

// providerCDexHTTPClient is the client the provider's Task forward posts
// with: authenticated per its PROVIDER_CDEX_* block's mode, through the same
// smartauth client every connector uses. A token refusal surfaces as the
// forward's error; there is no unauthenticated fallback.
func providerCDexHTTPClient(cfg config) (*http.Client, error) {
	sc, err := providerCDexSmartConfig(cfg)
	if err != nil {
		return nil, err
	}
	cfg.diagnostic.smart(&sc)
	hc, err := smartauth.NewHTTPClient(sc)
	if err != nil {
		return nil, fmt.Errorf("Task forward smartauth client: %w", err)
	}
	return hc, nil
}

// providerCDexSmartConfig is the PROVIDER_CDEX_* SMART config, its
// credential loaded, tagged cdex-task (the forward) and cdex-task-token
// (its token endpoint).
func providerCDexSmartConfig(cfg config) (smartauth.Config, error) {
	sc := smartauth.Config{
		TokenURL: cfg.ProviderCDexTokenURL, ClientID: cfg.ProviderCDexClientID, Scope: cfg.ProviderCDexScope,
		Observer: cfg.tokenNotes,
	}
	tagSmart(cfg.certs, &sc, cfg.certs.Wrap(endpointcert.Of(endpointcert.CDexTask), nil), endpointcert.CDexTaskToken)
	if cfg.ProviderCDexClientSecret != "" {
		sc.ClientSecret = cfg.ProviderCDexClientSecret
		return sc, nil
	}
	key, err := loadSmartKey(cfg.ProviderCDexClientKey, cfg.ProviderCDexClientAlg)
	if err != nil {
		return smartauth.Config{}, fmt.Errorf("load Task forward client key: %w", err)
	}
	sc.Alg, sc.Key, sc.KID = cfg.ProviderCDexClientAlg, key, cfg.ProviderCDexClientKID
	return sc, nil
}

// providerCDexTokenFetch is fhirTokenFetch's PROVIDER_CDEX_* counterpart.
// Same fresh-TokenSource-per-invocation rule applies (see fhirTokenFetch's doc).
func providerCDexTokenFetch(cfg config) func(context.Context) error {
	sc, err := providerCDexSmartConfig(cfg)
	if err != nil {
		return func(context.Context) error { return err }
	}
	return func(ctx context.Context) error {
		return classifyTokenErr(errFromToken(&smartauth.TokenSource{Config: sc}, ctx))
	}
}

// providerDTRPopulateSmartBase is the PROVIDER_DTR_POPULATE_* SMART config
// before its credential: the client, scope and tagged transports both the
// $populate client and its token check use.
func providerDTRPopulateSmartBase(cfg config) smartauth.Config {
	sc := smartauth.Config{
		TokenURL: cfg.ProviderDTRPopulateTokenURL, ClientID: cfg.ProviderDTRPopulateClientID, Scope: cfg.ProviderDTRPopulateScope,
		Observer: cfg.tokenNotes,
	}
	tagSmart(cfg.certs, &sc, cfg.certs.Wrap(endpointcert.Of(endpointcert.Populate), nil), endpointcert.PopulateToken)
	return sc
}

// loadSmartKey reads a PEM-encoded EC or RSA private key from path. ES384,
// RS384 and, for the blocks that admit it (checkClientAuthModeAlgs), RS256
// are supported (AI-11 / OWD-6: no shared-secret algorithms).
func loadSmartKey(path, alg string) (crypto.PrivateKey, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read key %q: %w", path, err)
	}
	switch alg {
	case "ES384":
		return jwt.ParseECPrivateKeyFromPEM(pemBytes)
	case "RS384", "RS256":
		return jwt.ParseRSAPrivateKeyFromPEM(pemBytes)
	default:
		return nil, fmt.Errorf("unsupported alg %q", alg)
	}
}

// operatedPopulateProfile reports whether an origination profile REQUIRES the operated
// $populate endpoint. Both real-payer lanes do: the questionnaire on the wire is the
// payer's own resource, and the managed filler only knew the retired in-process tree.
func operatedPopulateProfile(profile string) bool {
	return profile == "provider-data" || profile == "demo"
}

// requiresOperatedPopulate reports whether a gateway of role, with origination profile
// profile, needs the operated $populate endpoint: only a provider originates, so only a
// provider's profile does.
func requiresOperatedPopulate(role, profile string) bool {
	return role == "provider" && operatedPopulateProfile(profile)
}

func populateFailureObserver(stdout io.Writer) func(engine.PopulateFailure) {
	// Serialize concurrent population records.
	var mu sync.Mutex
	return func(note engine.PopulateFailure) {
		if note.Status != 0 && (note.Status < 100 || note.Status > 599) {
			return
		}
		valid := false
		switch note.Stage {
		case "request_build", "token_acquisition", "transport", "body_read":
			valid = note.Reason == "canceled" || note.Reason == "deadline" || note.Reason == "other"
		case "http_status":
			valid = note.Reason == "non_2xx"
		case "qr_extract":
			valid = note.Reason == "invalid_json" || note.Reason == "wrong_resource_type"
		}
		if !valid {
			return
		}
		// Every interpolated string is allowlisted above; no caller text or payload
		// can enter this versioned JSON line. Writer errors do not alter HTTP results.
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(stdout, "gateway: populate_failure {\"version\":1,\"stage\":%q,\"reason\":%q,\"status\":%d}\n", note.Stage, note.Reason, note.Status)
	}
}

// certificationValidators constructs read-only clients with independent pools
// for every line that has an address the evidence may use, and never invents
// one. Per line, in order: FHIR_CERTIFY_URL_<line>, an address for the evidence
// alone; FHIR_VALIDATE_URL_<line>, the line's routing lane; and the default
// lane routing is qualifying (defaults), gated on that qualification — until the
// default has answered a qualification the line's evidence says no lane is
// configured and nothing is dialed, because the default is a Compose service
// name and in a deployment where it does not resolve every exchange would
// otherwise record a hashed DNS failure as its verdict. A 2.1 or 2.2 address is
// gated the same way, on its own qualification by the same qualifier (a lane
// still warming answers errors that are not a verdict on the payload); until it
// passes the line's evidence says the lane is not qualified and nothing is
// dialed. A line with none of the three has no client, which the collector
// records as unavailable. The canonical line certifies on its own endpoint, the
// gateway's own routing validator. Nothing here feeds routing: a certify-only
// address is not a lane (validatorLanesForDeclared never reads it), and a gated
// client's own qualification never changes routing's lanes. Each gated client
// runs its own background qualification loop from the certification worker's
// start, only where evidence is collected. qualify is the same qualifier
// routing uses, so a default lane's client defers to routing's retries and
// takes routing's identical pass as its first qualification; a validator that
// comes up late is certified against within one retry wait of coming up.
func certificationValidators(getenv func(string) string, cfg config, canonical string, defaults map[string]*engine.DiscoveredLane, qualify engine.LaneQualifier) (map[string]shnsdk.Validator, error) {
	endpoints := map[string]string{
		"2.0": canonical,
		"2.1": firstNonEmpty(cfg.FHIRCertifyURL21, cfg.FHIRValidateURL21),
		"2.2": firstNonEmpty(cfg.FHIRCertifyURL22, cfg.FHIRValidateURL22),
	}
	out := make(map[string]shnsdk.Validator, len(endpoints))
	for line, endpoint := range endpoints {
		if fakeValidator(getenv) {
			v, err := lineFakeValidator(line)
			if err != nil {
				engine.CloseCertificationClients(out)
				return nil, err
			}
			out[line] = v
			continue
		}
		if endpoint == "" {
			suffix := strings.ReplaceAll(line, ".", "_")
			if d := defaults[line]; d != nil {
				g := engine.NewGatedCertificationValidator(d, qualify, "FHIR_CERTIFY_URL_"+suffix+" and FHIR_VALIDATE_URL_"+suffix+" are not configured")
				engine.TagValidatorClient(g.CertificationClient(), cfg.certs, certifyLane(line))
				out[line] = g
			} else if cfg.DefaultValidatorLanes == defaultValidatorLanesNone && line != "2.0" {
				// No default lane exists to qualify: the evidence names the lane to
				// configure, and nothing is ever dialed for it.
				out[line] = unconfiguredCertification{reason: "FHIR_CERTIFY_URL_" + suffix + " and FHIR_VALIDATE_URL_" + suffix + " are not configured, and this network has no default validator lanes"}
			}
			continue
		}
		if line == "2.0" {
			out[line] = certifyClient(cfg.certs, line, endpoint)
			continue
		}
		out[line] = engine.NewQualifiedCertificationValidator(line, certifyClient(cfg.certs, line, endpoint), qualify)
	}
	return out, nil
}

// certificationClient builds the client for a certification endpoint configured
// by address: certificationValidators' clients and each boot warm-up's own.
// Its dedicated transport dials with its own dialer, never through
// http.DefaultTransport, so a test that fixtures http.DefaultTransport
// replaces this too; otherwise its fixture host is resolved on the live
// network. A default lane's gated client builds its own in engine, and
// dials only once that lane qualifies and a certification runs.
var certificationClient = engine.NewCertificationOperationValidator

// backendHeaderReserved are the header names PAYER_DAVINCI_BACKEND_HEADERS may
// not set: the ones this gateway itself owns on a partner request (the leg's id
// in X-Correlation-Id among them), the request-target fields, and the
// hop-by-hop set of RFC 9110 §7.6.1.
var backendHeaderReserved = map[string]bool{
	"Authorization": true, "Content-Type": true, "Accept": true, "Host": true, "Content-Length": true, "X-Correlation-Id": true,
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true, "Proxy-Authorization": true,
	"Proxy-Connection": true, "Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true,
}

// validHeaderFieldName reports whether name is an RFC 9110 token.
func validHeaderFieldName(name string) bool {
	if name == "" {
		return false
	}
	const tokenPunct = "!#$%&'*+-.^_`|~"
	for _, c := range name {
		if c >= 0x80 || (!unicode.IsLetter(c) && !unicode.IsDigit(c) && !strings.ContainsRune(tokenPunct, c)) {
			return false
		}
	}
	return true
}

// validHeaderFieldValue reports whether value is an RFC 9110 field value:
// visible characters, space and horizontal tab only (no CR, LF, NUL or
// other controls, which would end or corrupt the field on the wire).
func validHeaderFieldValue(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c == '\t' {
			continue
		}
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// parseBackendHeaders reads PAYER_DAVINCI_BACKEND_HEADERS: comma-separated
// `Name: value` pairs, each a well-formed HTTP field that this gateway does
// not own itself. Names are canonicalized; a name repeated, a name or value
// that is not a valid field, or a reserved name refuses the boot — a
// malformed or overriding header would be sent on every partner request.
func parseBackendHeaders(raw string) (http.Header, error) {
	h := http.Header{}
	for _, field := range strings.Split(raw, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		name, value, ok := strings.Cut(field, ":")
		if !ok {
			return nil, fmt.Errorf("%q is not a `name: value` pair", field)
		}
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if name == "" {
			return nil, fmt.Errorf("%q: empty header name", field)
		}
		if value == "" {
			return nil, fmt.Errorf("%q: empty header value", field)
		}
		if !validHeaderFieldName(name) {
			return nil, fmt.Errorf("%q: not a valid HTTP field name", name)
		}
		if !validHeaderFieldValue(value) {
			return nil, fmt.Errorf("%s: not a valid HTTP field value", name)
		}
		canon := http.CanonicalHeaderKey(name)
		if backendHeaderReserved[canon] {
			return nil, fmt.Errorf("%s is reserved (set by the gateway, or hop-by-hop), not a partner routing header", canon)
		}
		if _, dup := h[canon]; dup {
			return nil, fmt.Errorf("%s is repeated", canon)
		}
		h[canon] = []string{value}
	}
	if len(h) == 0 {
		return nil, fmt.Errorf("is set but names no header (got %q)", raw)
	}
	return h, nil
}

// unconfiguredCertification answers a line with no lane at all: the verdict
// is unavailable, naming what to configure, and nothing is dialed.
type unconfiguredCertification struct{ reason string }

func (u unconfiguredCertification) Validate(context.Context, []byte, string) (shnsdk.Result, error) {
	return shnsdk.Result{}, &engine.CertificationLaneUnavailable{Reason: u.reason}
}

// moduleOneSettingsSet are the settings cfg sets that only module 1 reads: the
// clinician NPI and the operated $populate connector.
func moduleOneSettingsSet(cfg config) []string {
	var set []string
	for _, s := range []struct {
		key string
		on  bool
	}{
		{"NPI", cfg.NPI != ""},
		{"PROVIDER_DTR_NATIVE", cfg.ProviderDTRNative},
		{"PROVIDER_DTR_POPULATE_URL", cfg.ProviderDTRPopulateURL != ""},
		{"PROVIDER_DTR_POPULATE_TOKEN_URL", cfg.ProviderDTRPopulateTokenURL != ""},
	} {
		if s.on {
			set = append(set, s.key)
		}
	}
	return set
}

// scenarioCallersLine is the boot line about a provider gateway's /scenario
// callers: whom it admits, none, or a warning that it does not authenticate
// them.
func scenarioCallersLine(callers map[string]ed25519.PublicKey) string {
	switch {
	case callers == nil:
		return "WARNING: SCENARIO_CALLERS_FILE unset — the /scenario routes do not authenticate their caller"
	case len(callers) == 0:
		return "/scenario admits no caller (SCENARIO_CALLERS_FILE lists none)"
	}
	labels := slices.Sorted(maps.Keys(callers))
	return "/scenario admits only its callers: " + strings.Join(labels, ", ")
}
