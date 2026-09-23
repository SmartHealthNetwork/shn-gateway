// Package gateway is the Smart Gateway: the holder-side integration point that
// composes identity, per-operation authorization, payload-blind envelopes, FHIR
// mapping, and per-message profile validation. One binary, two roles (provider
// and payer), wired by Config.Role. This is the integration heart of UC-01.
package engine

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/diagnostics"
	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// oswestryLinkID is the questionnaire linkId for the Oswestry functional-status
// item, shared across handleUC07Pending, completePatient, and the UC-06 path.
// These sites must agree: the patient's answer is validated against this exact
// linkId, so it is hoisted to a single const.
const oswestryLinkID = "functional-status-oswestry"

// hhaFunctionalStatusLinkID is the HomeHealthAssessment questionnaire's free-text functional-status
// item (the Oswestry analog), the clinician-entered manual item for provider-data UC-06 — linkId
// "3.2" "Functional limitations" (type text, 0-CQL), confirmed against br-payer's live
// $questionnaire-package.
const hhaFunctionalStatusLinkID = "3.2"

// defaultHHAFunctionalLimitations is the operator-supplied free-text functional-status narrative for
// the provider-data UC-06 clinician attestation when none is provided (the free-text analog of the
// retired stub's Oswestry "42"; D-2RI-1 — operator-supplied, NOT derived from a clinical SoR fact).
const defaultHHAFunctionalLimitations = "Impaired ambulation and reduced lower-extremity strength limiting independent mobility; skilled physical therapy indicated."

// defaultHHAFunctionalLimitationsPatient is the operator-supplied free-text functional-status narrative
// for provider-data UC-07 when the patient provides none (the patient analog of the clinician default;
// D-2RI-1 — operator-supplied, NOT a real patient's authored value, which is DEF-9/OIDC-LOCAL).
const defaultHHAFunctionalLimitationsPatient = "I have trouble walking and standing without help and need physical therapy to regain my strength and mobility."

// Per-message FHIR validation: the payload-blind Hub cannot validate payloads
// (AI-2), so validation lives at the gateways on egress and ingress. Every
// Validate call passes an EMPTY profile = base-R4 validation via $validate
// (structural + terminology). US Core profile pinning for Coverage/ServiceRequest
// is carried in the resource's meta.profile (set by internal/fhirmap), so the
// IG-enabled compose HAPI validates them against US Core even with an empty
// profile param; Da Vinci CRD/DTR/PAS profile pinning is a tracked fast-follow.
// The Validator interface keeps the profile param so explicit per-call pinning
// stays a one-line change (see validateFHIR).

// Config wires a gateway. The same struct constructs all roles; provider-only
// fields (HubURL) are unused on the payer/facility side.
type Config struct {
	Role     string // "provider" | "payer" | "facility"
	HolderID string
	// PayerRouter resolves a Coverage.payor identity → payer holder id (FR-G40). REQUIRED on the
	// provider side: there is NO default payer. Every payer leg's recipient is derived from the
	// patient's own Coverage via recipientFor (AI-G11 / OWD-G10) — a miss (no coverage / no
	// parseable payer / no directory mapping) fails closed with a legible 422, never a default
	// route. Replaced the deleted CounterpartID. The payer side does not use it — it replies to
	// the inbound envelope's Sender.
	PayerRouter PayerRouter
	// OriginationProfile selects the per-UC behavior lane: "demo" = originate the
	// family-keyed order tuples (E0250/L8000/G0151/J3490) off the MBR-D-UC0N roster;
	// "provider-data" = originate every UC off the provider's seeded SoR and drive real
	// br-payer verdicts (Mode A). targetsBrPayer keys the contained-insurer /
	// absolute-refs / R-8 ingress-skip handling on it. Spec 2B-bis/2C, §4.3.
	OriginationProfile string
	// Identity is the gateway's substrate identity: its holder signing key (used to
	// sign holder assertions and patient-access audit records) plus its envelope-
	// encryption keypair (used to open inbound sealed payloads and seal responses).
	Identity shnsdk.Identity
	AuthzURL string
	AuthzPub ed25519.PublicKey
	// HubTransportPub verifies the per-hop X-Hub-Assertion the Hub sends with
	// every /substrate/inbound forward (per-hop transport auth). REQUIRED
	// for roles that mount /substrate/inbound (payer/facility/phg): New panics
	// without it — mandatory enforcement, no "configured off" state.
	HubTransportPub ed25519.PublicKey
	HubURL          string // provider only
	Reg             shnsdk.Registry
	// Validator is the CANONICAL-lane FHIR $validate client (FR-36). It stays the
	// only required validator: a deployment that speaks one line needs one lane.
	Validator shnsdk.Validator
	// PayerEOBValidator certifies an explicitly requested payer-owned EOB record
	// against PDex. It is independent of optional native conformance lanes, so
	// native none stays no-check while this local clinical write remains gated.
	PayerEOBValidator shnsdk.Validator
	// ValidatorsByLine are the per-contract-LINE $validate lanes:
	// "2.0"/"2.1"/"2.2" → the validator that resolves THAT line's IG
	// packages. A HAPI instance can host exactly one version of an IG, so a tri-line
	// deployment runs one validator per line; gateway/app wires FHIR_VALIDATE_URL
	// (canonical) plus FHIR_VALIDATE_URL_2_1 / _2_2, or maps every native line to
	// the fake validator under SHN_FAKE_VALIDATOR=1.
	//
	// EMPTY map = an un-laned deployment: every line falls back to Validator (the
	// pre-multi-line behavior every in-process test and the harness rely on). NON-EMPTY
	// map = the deployment is AUTHORITATIVE about which lines it can validate, so a
	// line absent from it is UNLANED and fails closed (see validatorForLine).
	ValidatorsByLine map[string]shnsdk.Validator
	// AdaptationValidator supplies mandatory proof checkers only for an explicit
	// transform. It must not be used for native carriage or optional observation.
	AdaptationValidator func(contract, line string) shnsdk.Validator
	// CertificationValidatorsByLine is retained for source compatibility. New closes
	// these legacy clients; registry observations use the actual validation lanes.
	CertificationValidatorsByLine map[string]shnsdk.Validator
	certificationDisabled         bool
	// DefaultValidatorsByLine are lifecycle-managed candidates, admitted only after qualification.
	DefaultValidatorsByLine map[string]*DiscoveredLane
	// CanonicalFallbackLines identifies single-native-contract compatibility aliases.
	CanonicalFallbackLines map[string]bool
	// DeclaredContractVersions is the operator-declared exchange-contract token set
	// (SHN_CONTRACT_VERSIONS, boot-validated in gateway/app: grammar + ⊆
	// NativeContractVersions). Empty ⇒ shnsdk.SupportedContractVersions(). Read ONLY
	// through g.declaredContractVersions() for authored selection. A configured
	// native receiver has a separate peer-visible endpoint declaration.
	DeclaredContractVersions []string
	// EgressNativeLines (D1c, productized in the kit-bridging slice) restricts
	// arm (2)'s (native-reach) view of NativeContractVersions() to exactly
	// these lines when non-nil. nil (the PRODUCTION default) = the real native
	// set — every published line is reachable natively, so arm (3) transform
	// chains stay dormant within {2.0,2.1,2.2}. Non-nil comes from exactly two
	// places: tests (the cross-line pair suite), and the loudly-named demo env
	// SHN_DEMO_EGRESS_NATIVE_LINES (gateway/app) — the SHN Kit's bridging demo
	// simulates "a build that predates the newer lines" with it. Never read
	// outside selectNativeReachRoute/nativeLinesView/selectResumeRoute.
	EgressNativeLines []string
	// StrictPeerForTest (per-peer strict extensions, FR-G52) is a TEST-ONLY
	// seam (EgressNativeLines' naming precedent) that forces
	// g.strictPeer's answer to true for every consult. PRODUCTION default
	// (false, the zero value, never set from env): strictPeer is dormant —
	// see its comment in originate.go for why arm (3)'s per-peer
	// gated-overlay input stays hardwired false today. Never read outside
	// strictPeer; never wired from env/config in app.go.
	StrictPeerForTest bool
	// SoR reads the holder's backing system of record (resolve/coverage/clinical/
	// supplemental/facility-records). E2 swaps in a FHIR client; demo uses the stub.
	SoR SystemOfRecord
	// SubjectReferenceResolver resolves authoritative, holder-scoped identity links.
	// Legacy demographic/member derivation is never a conformance identity source.
	SubjectReferenceResolver SubjectReferenceResolver
	// AcceptUnknownMembers is retained for source compatibility for one release.
	// Deprecated: it has no effect on identity, admission or source disclosure.
	AcceptUnknownMembers bool
	// Store is the gateway's own business state (auth numbers, pended-claim ledger,
	// issued EOBs). Demo: in-memory stub; separated: holdersim; later: gateway Postgres.
	Store Store
	// Adjudicator is the partner's decision surface (order-select/questionnaire/
	// prior-auth; Eligibility is served by the standalone SDK Responder only — R11 makes
	// eligibility an engine-side Coverage read). It no longer DERIVES a Responder: a
	// partner that wants the engine to answer PA legs from its own decisions builds a
	// LegResponder around this interface and sets Responder. Optional.
	//
	// Deprecated: the engine no longer consumes this field (the payer retirement
	// removed the derived in-process responder), so setting it alone does nothing.
	// It is kept for source compatibility until the seam-promotion decision lands;
	// implement shnsdk.Adjudicator behind the standalone SDK Responder, or set
	// Config.Responder, instead.
	Adjudicator shnsdk.Adjudicator
	// Responder is the payer content OCCUPANT and the only source of Da Vinci leg
	// answers. REQUIRED for the payer role — New returns an error without it (§3.2's
	// fail-closed payer boot). Normally a native-forward responder over the payer's own
	// Da Vinci endpoint (NewNativeResponder); a partner MAY inject any LegResponder.
	Responder LegResponder
	// PayerDavinciNative reports that the payer Responder native-forwards the read-only
	// legs to a REAL partner Da Vinci endpoint (PAYER_DAVINCI_BASE_URL set). When true the
	// DTR $questionnaire-package response is a FOREIGN Da Vinci Bundle (dtr-std-questionnaire /
	// dtr-questionnaireresponse profiles) that SHN — which hosts US Core only — cannot
	// $validate; like the conformant crd-order-select / pas-claim legs (R-8),
	// the DTR response is a NEAR-RELAY: the trust-critical subject fence still runs, but the
	// engine does NOT foreign-$validate it. false ⇒ an SHN-produced, US-Core-resolvable
	// package, which still egress-$validates byte-identically. FR-G28.
	PayerDavinciNative bool
	// Populator is the DTR population seam (provider-local). Normally left nil and
	// DEFAULTED to the managed backend (today's FillQuestionnaire) in New; the native
	// pass-through backend is injected by config (PROVIDER_DTR_NATIVE). A test MAY
	// inject a custom Populator.
	Populator Populator
	// IngressEnabled mounts the Da Vinci ingress routes on the provider role.
	// Set from PROVIDER_DAVINCI_INGRESS by app.go. The routes fail-closed without
	// ingressAuthBypass (real inbound UDAP auth is a planned future enhancement), so
	// enabling them in prod is safe — they reject every call.
	IngressEnabled bool
	// PayerEOBActionsEnabled exposes the payer's explicit, authenticated local
	// EOB recording action. It never runs during native PAS delivery.
	PayerEOBActionsEnabled bool
	// IngressBaseURL is the gateway's CONFIG-PINNED public base URL: the SMART
	// Backend Services aud (assertion + bearer) and the advertised token endpoint.
	// Never request-derived (no Host-header spoof). Required when IngressEnabled and
	// not bypassed. Set from PROVIDER_DAVINCI_INGRESS_BASE_URL by app.go.
	IngressBaseURL string
	// IngressClients are the config-registered inbound clients (client_id →
	// public key + scopes). Required (>=1) when IngressEnabled and not bypassed.
	IngressClients map[string]IngressClientRegistration
	// ingressAuthBypass skips the (deferred) inbound participant auth on the ingress.
	// UNEXPORTED and set ONLY by EnableIngressForTest — never read from env, never set
	// by build() (image purity, scaffold pattern).
	ingressAuthBypass bool
	Client            *http.Client
	Clock             func() time.Time
	NPI               string
	// CorrelationGen generates a new correlation ID for each outbound scenario
	// request. Defaults to newCorrelationID (crypto-random 128-bit hex string).
	// Override in tests for deterministic IDs.
	CorrelationGen func() string
	// ConsentURL is the Trust-operated Global Person Consent service URL (facility
	// only). The facility's consent backstop re-confirms a TREAT permit here
	// before releasing any records. When empty the backstop fails closed (no consent
	// service ⇒ no disclosure).
	ConsentURL string
	// AuditURL is the Audit Plane's base URL. Used by the payer to append a
	// patient-access-read record (FR-29/FR-33) when it serves a Patient Access API
	// read. The Patient Access read path is FAIL-CLOSED: a gateway with no AuditURL
	// has no audit capability, so serveEOB disables the read (502) rather than
	// serving it unaudited, and a failed audit append also blocks the read (502).
	AuditURL string
	// PHGURL is the Trust-operated PHG base URL. Used by the provider scenario for
	// UC-08 demo orchestration: after the PAS denial, the provider queries the PHG
	// denial view (GET /denial?pci=<pci>) to surface the patient-rendered reason.
	// This stands in for the patient app in the Connectathon demo (provider→PHG
	// call is orchestration only, not a substrate leg). Empty → skip the PHG query.
	PHGURL string
	// Observer opts into participant-scoped transient edge inspection, including
	// raw payload snapshots. Delivery is asynchronous, bounded and lossy under
	// pressure, with one dispatcher per gateway. Callbacks must return promptly
	// and treat snapshots as read-only; retaining bytes requires caller-owned
	// bounded storage. Panics and blocked callbacks cannot affect native delivery.
	// None disables conformance work but does not disable this separate opt-in.
	// WaitObserverCompletion reports notification loss independently of exchange.
	Observer                     func(ObserverEvent)
	observerQueueCapacityForTest int
	// Diagnostic is an optional prompt, concurrency-safe, nonblocking sink.
	// It must reserve bounded memory before retaining event bytes. Nil disables it.
	Diagnostic func(diagnostics.Event) bool
	// DiagnosticTraceKey verifies optional private ingress attribution, never authority.
	DiagnosticTraceKey []byte
	// LegMetric, when non-nil, receives one outcome string per origination-leg
	// event at the roundTrip choke point: LegOutcomeRouted when a leg is
	// attempted, then exactly one terminal outcome — Answered (the counterpart
	// responded: 2xx or relayed app non-2xx), Denied (the Authorization
	// Framework denied the leg — a policy decision, not an error), Unreachable
	// (the Hub leg did not complete, including a Hub refusal after an invalid
	// recipient response envelope; it does not prove the responder itself was
	// unreachable), or Failed (anything else). nil (the default, and the
	// published-gateway posture) = no
	// emission. MAY BE CALLED CONCURRENTLY; implementations must be
	// goroutine-safe and must never block — the callback sits on the request
	// path. Additive instrumentation only: emission must not change exchange
	// behavior (TestLegMetric_ConformanceNeutral). Carries NO payloads.
	LegMetric func(outcome string)
	// Replay is the one-time-use record behind the Hub-assertion jti, ingress
	// client_assertion jti and patient-access correlationId guards. Nil selects a
	// process-local record, correct only at one replica; a shared store makes the
	// guards hold across replicas.
	Replay ReplayStore
	// IngressKeys signs issued ingress bearers and resolves their kid. Nil selects
	// one process-local key (correct only at one replica). "Nil" here means a nil
	// interface value: a typed-nil pointer stored in the interface is a configured
	// store, not "unset", and the ephemeral fallback will not be selected for it.
	IngressKeys IngressKeyStore
	// Exchanges is the correlation seam. Nil selects an in-memory store bounded by
	// ExchangeTTL (correct only at one replica; lost on restart).
	Exchanges ExchangeStore
	// ExchangeTTL bounds how long an exchange is retained on either backend. Zero
	// selects 168h. The application layer validates the operator's value.
	ExchangeTTL time.Duration
	// StoreErrorMetric, when set, is called once per shared-state store failure with
	// the failing store's name. It is how a database outage becomes visible to an
	// operator, so EVERY path that refuses or degrades on a store error reports here:
	//   "replay"     — the one-time-use record could not be consulted, on all three
	//                  scopes: the ingress client_assertion jti (/oauth/token answers
	//                  503), the Hub assertion (/substrate/inbound answers 503) and the
	//                  patient-access correlation (the read answers 503).
	//   "ingresskey" — the signing key could not be resolved, so no bearer can be
	//                  issued (/oauth/token answers 503).
	//   "exchange"   — the best-effort correlation seam failed (the request itself is
	//                  unaffected).
	// The store implementations report their own internal failures — an ingress key
	// RELOAD error behind a verification miss, a failed exchange insert — through the
	// same counter via their own error hook, wired at the application layer.
	StoreErrorMetric func(store string)
	// DemoEdgeCapture (SHN_DEMO_EDGE_CAPTURE) turns on the bounded pre-seal
	// edge-capture store (edgecapture.go): egressAdapt records each
	// transformed leg's own before/after payload pair for local inspection,
	// retrievable by correlation id via EdgeCaptureFor. false (the
	// production default) = no store is ever built and egressAdapt's capture
	// hook is skipped entirely — conformance-neutral by construction (see
	// TestEgressAdapt_EdgeCaptureOffIsConformanceNeutral). Loud by name: this
	// is a local demonstration/inspection surface, never the wire, the audit
	// record, or any conformance surface.
	DemoEdgeCapture bool
	// ConformanceEnforcement is this participant's conformance enforcement
	// level. The zero value is EnforcementNone, matching an absent
	// CONFORMANCE_ENFORCEMENT in gateway/app. Conformance certification gates
	// and fixtures pin strict explicitly
	// (test/invariants' TestInvariant_EveryGateRunsStrict).
	ConformanceEnforcement ConformanceEnforcement
	// AdvertisedCDSHooks narrows which CDS Hooks services the provider ingress
	// advertises and dispatches (ParseAdvertisedCDSHooks). Nil advertises every
	// hook the network carries. An interim, optional override: the registry
	// carries no declaration of the legs a payer offers, so a lane whose payers
	// carry no order-dispatch leg otherwise advertises a service that fails at
	// routing. It removes a false promise; it never adds a capability.
	AdvertisedCDSHooks []string
}

// Gateway is a constructed holder gateway.
type Gateway struct {
	cfg     Config
	mu      sync.Mutex
	pending map[string]pendState

	checkerAvailability checkerAvailability

	// exchanges is the Layer-2 Exchange-correlation seam (the DaVinciIngress origination
	// driver groups each ingress call's legs under one Exchange.ID). In-memory default
	// (bounded by ExchangeTTL, correct at exactly one replica); Config.Exchanges swaps in
	// a durable, shared backend — the application layer selects the Postgres one under
	// SHN_STORE_DATABASE_URL.
	exchanges ExchangeStore

	// replay is the shared one-time-use record for every replay guard: the
	// patient-access correlationId (paReplayWindow, consume-once on the direct
	// Patient Access read), the Hub's X-Hub-Assertion jti (window
	// shnsdk.MaxAssertionTTL, the assertion's maximum lifetime) and the ingress
	// client_assertion jti (ingressJTIWindow).
	replay ReplayStore

	// ingressAuth is the gateway-hosted SMART Backend Services authorization server +
	// bearer verifier for the DaVinciIngress. nil when the ingress is disabled OR
	// running under the test-only bypass; ingressAuthOK is nil-safe.
	ingressAuth *ingressAuthServer

	// edgeCapture is the bounded pre-seal edge-capture store (nil unless
	// Config.DemoEdgeCapture is set — zero allocation in the production
	// default). An atomic.Pointer, not a bare field: egressAdapt's
	// lazy-build-on-first-capture and edgeCaptureLookup's read both run on
	// the concurrent request path (a request capturing while an inspector
	// reads), and a bare pointer field read with no synchronization would be
	// a data race under the Go memory model — sync.Once alone does not fix
	// this, since it only orders OTHER Do callers against each other, not an
	// unrelated bare read outside Do. Lazily built via CompareAndSwap on
	// first capture, so a Gateway assembled directly (bypassing New, a
	// common test pattern in this package) still works without a separate
	// construction step.
	edgeCapture       atomic.Pointer[edgeCaptureStore]
	certification     *certificationWorker
	observerDispatch  observerDispatcher
	observationMemory observationBudget
	operations        operationTracker

	// fallbackContinuations is the in-memory prior-authorization continuation
	// store used when the configured Store does not ship one (continuation.go).
	// Every Store this repository ships does, so this is the seam for a
	// participant's own Store written before continuations existed — it keeps
	// such a deployment working, and it reports itself as non-durable, which is
	// the fact the Kit's notice and CONFIGURATION state.
	//
	// An atomic.Pointer built lazily by CompareAndSwap, for the same reason
	// edgeCapture above is: a Gateway assembled directly (bypassing New, a common
	// test pattern in this package) must still have one, and two requests may
	// reach for it at once.
	fallbackContinuations atomic.Pointer[MemContinuations]
}

// New constructs a Gateway. The clock defaults to time.Now and the client to
// http.DefaultClient when unset.
//
// It returns an ERROR (never a panic) for the two conditions a DEPLOYMENT can hit with
// otherwise-valid config: a payer role with no content occupant (§3.2 — the
// fail-closed payer boot) and an unusable ingress client registration. The remaining
// checks stay panics: they fire only on a caller that omitted a required field, which is
// a programming error, not a deployment one.
//
// Caveat: the observer decoration below (Config.Validator/ValidatorsByLine ->
// observingValidator) only wraps whatever the two fields hold AT construction
// time. A handful of test sites assign to g.cfg.ValidatorsByLine directly
// AFTER New returns (test-only pattern — production code never does this,
// since app.go always builds the full ValidatorsByLine map before calling
// New) — those post-construction lanes bypass decoration and never emit
// validate.result even with Observer set. Harmless where it happens (those
// tests don't assert on the observer stream for that lane), but worth
// knowing before adding a new one that does.
func New(cfg Config) (*Gateway, error) {
	if !cfg.ConformanceEnforcement.valid() {
		return nil, fmt.Errorf("gateway: invalid conformance enforcement level %d", cfg.ConformanceEnforcement)
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Client == nil {
		cfg.Client = http.DefaultClient
	}
	if cfg.CorrelationGen == nil {
		cfg.CorrelationGen = newCorrelationID
	}
	if cfg.SoR == nil {
		panic("gateway: Config.SoR (SystemOfRecord) is required")
	}
	if err := checkSystemOfRecordSignatures(cfg.SoR); err != nil {
		return nil, fmt.Errorf("gateway: %w", err)
	}
	if cfg.Store == nil {
		panic("gateway: Config.Store is required")
	}
	// Every role signs holder assertions (and the payer signs patient-access audit
	// appends), so a gateway with no signing key is never valid. Fail fast at
	// construction: the Config→Identity reshape made a forgotten Identity a
	// single omitted field that leaves all key material nil and panics deep in
	// Seal/Open at first use — this turns that into a clear construction error.
	// (EncPub/EncPriv are intentionally NOT checked here: a patient-access-only
	// gateway legitimately runs SignPriv-only and never seals/opens an envelope.)
	if cfg.Identity.SignPriv == nil {
		panic("gateway: Config.Identity.SignPriv is required (holder assertions + audit signing)")
	}
	switch cfg.Role {
	case "payer", "facility", "phg":
		if len(cfg.HubTransportPub) == 0 {
			panic("gateway: HubTransportPub required for role " + cfg.Role + " (mounts /substrate/inbound; hop-auth has no off state)")
		}
	}
	// Capture the explicit identity capability before optional observation wrapping.
	if cfg.SubjectReferenceResolver == nil {
		cfg.SubjectReferenceResolver, _ = cfg.SoR.(SubjectReferenceResolver)
	}
	var observationGateway *Gateway
	// Observer seam: decorate the SoR BEFORE the Populator derivation below — it
	// captures cfg.SoR at construction (newManagedPopulator), and decorating at the
	// validator's later site would leave them reading the raw SoR forever.
	// g does not exist yet here; the closure binds its dispatcher after construction.
	// Idempotence guard: never double-wrap (double emission) if a caller
	// passes an already-observed SoR back through New.
	if cfg.Observer != nil && cfg.SoR != nil {
		if prior, already := cfg.SoR.(observingSoR); already {
			cfg.SoR = prior.inner
		}
		cfg.SoR = observingSoR{inner: cfg.SoR, observer: func(e ObserverEvent) { observationGateway.observe(e) }, clock: cfg.Clock}
	}
	// FAIL-CLOSED PAYER BOOT (§3.2). A payer gateway answers Da Vinci legs out of a
	// content OCCUPANT and nothing else: either a native-forward Responder built against a
	// real Da Vinci endpoint (PAYER_DAVINCI_BASE_URL) or a LegResponder a partner injects
	// directly. There is NO derived in-process fallback any more — the in-process responder
	// that used to be synthesized here from Config.Adjudicator is deleted, so a payer with
	// no occupant refuses to boot instead of quietly serving invented verdicts.
	if cfg.Role == "payer" && cfg.Responder == nil {
		return nil, errors.New("gateway: a payer gateway has no content occupant — set Config.Responder " +
			"(a native-forward responder over the payer's own Da Vinci endpoint, or your own LegResponder). " +
			"From the published binary: set PAYER_DAVINCI_BASE_URL (plus PAYER_DAVINCI_TOKEN_URL/PAYER_DAVINCI_CLIENT_ID/" +
			"PAYER_DAVINCI_CLIENT_SECRET for an authenticated forward). Config.Adjudicator no longer derives a responder")
	}
	if cfg.Populator == nil {
		cfg.Populator = newManagedPopulator(cfg.SoR)
	}
	g := &Gateway{
		cfg:     cfg,
		pending: map[string]pendState{},
	}
	observationGateway = g
	g.exchanges = cfg.Exchanges
	if g.exchanges == nil {
		g.exchanges = NewInMemoryExchangeStore(cfg.ExchangeTTL, cfg.Clock)
	}
	g.replay = cfg.Replay
	if g.replay == nil {
		g.replay = NewInMemoryReplayStore()
	}
	// Observer seam: decorate the validator so every $validate emits
	// validate.result. Only when observing — the nil path keeps the
	// validator untouched. Idempotence guard: never double-wrap (double
	// emission) if a caller passes an already-observed cfg back through New.
	//
	// The PER-LINE lanes must be decorated too, and for the same reason: once
	// ValidatorsByLine is non-empty, validatorForLine resolves EVERY
	// line-scoped $validate through that map (gateway.go's validatorForLine)
	// and never through cfg.Validator — so wrapping only cfg.Validator left the
	// observer stream silently missing the validate.result for every
	// version-bearing leg, which is exactly the class of honesty gap the
	// observer exists to close. Found by regenerating ui/kit's captured
	// fixtures: the ehr uc03 capture had dropped from 6
	// validate.result events to 2 — the 4 line-scoped ones.
	if g.cfg.Observer != nil {
		if g.cfg.Validator != nil {
			if _, already := g.cfg.Validator.(observingValidator); !already {
				g.cfg.Validator = observingValidator{inner: g.cfg.Validator, g: g}
			}
		}
		if len(g.cfg.ValidatorsByLine) > 0 {
			// A fresh map: the caller's (gateway/app builds one per boot, and a test
			// may share one across gateways) must never be mutated from in here.
			lanes := make(map[string]shnsdk.Validator, len(g.cfg.ValidatorsByLine))
			for line, v := range g.cfg.ValidatorsByLine {
				if _, already := v.(observingValidator); already || v == nil {
					lanes[line] = v
					continue
				}
				lanes[line] = observingValidator{inner: v, g: g}
			}
			g.cfg.ValidatorsByLine = lanes
		}
	}
	// Build the inbound auth server only for a real-auth ingress (not under the
	// test bypass — body-conformance tests don't register clients). app.go has
	// already validated registrations; a failure here is a config invariant.
	if (cfg.IngressEnabled || cfg.PayerEOBActionsEnabled) && !cfg.ingressAuthBypass {
		keys := cfg.IngressKeys
		if keys == nil {
			ek, err := newEphemeralKeyStore()
			if err != nil {
				return nil, fmt.Errorf("gateway: ingress auth: %w", err)
			}
			keys = ek
			// INFO, not a fault: this is the normal posture of every gateway booted
			// without a shared store. Said once, at boot, because the consequence is
			// invisible at one replica and total at two — a sibling replica rejects
			// this one's bearers, since the signing key never left this process.
			log.Print("ingress bearer key is ephemeral (no SHN_STORE_DATABASE_URL): run a single reachable instance")
		}
		ia, err := newIngressAuthServer(cfg.IngressBaseURL, cfg.IngressClients, cfg.Clock, keys, g.replay)
		if err != nil {
			return nil, fmt.Errorf("gateway: ingress auth: %w", err)
		}
		ia.storeErr = g.noteStoreError
		g.ingressAuth = ia
	}
	// A responder that can take the finding emitter gets it now: it could not
	// be an option, because no gateway existed when the responder was built.
	if binder, ok := cfg.Responder.(findingEmitterBinder); ok {
		binder.bindFindingEmitter(g.emitFinding)
	}
	g.startCertification()
	return g, nil
}

// recipientForWith resolves the payer holder for an exchange from a Coverage (FR-G40). resolveRef
// resolves an EXTERNAL Coverage.payor Organization reference ("<Type>/<id>"): at origination that is
// the provider SoR; on INGRESS it is the inbound payload's OWN resources — a conformant partner's
// payor Organization lives in its bundle/prefetch/parameters, NOT the provider SoR (bundleRefResolver
// et al.). No default: any miss fails closed with a legible 422 (AI-G11 / OWD-G10). status==0 ⇒ ok.
// It also returns the parsed PayerIdentifier so an origination site can REUSE it as the emitted payer
// (one parse, one external-Org lookup) instead of re-parsing the same Coverage.
func (g *Gateway) recipientForWith(coverageJSON []byte, resolveRef func(string) ([]byte, bool)) (holderID string, pid shnsdk.PayerIdentifier, status int, msg string) {
	// Fail closed when the gateway was deployed without a payer directory: no router ⇒ no routing
	// ⇒ no default (AI-G11 / OWD-G10). Guard BEFORE the nil-interface Resolve call so a provider
	// missing PAYER_DIRECTORY returns a legible 422 instead of panicking.
	if g.cfg.PayerRouter == nil {
		return "", shnsdk.PayerIdentifier{}, http.StatusUnprocessableEntity, "no payer router configured"
	}
	parsed, err := shnsdk.ParseCoveragePayer(coverageJSON, resolveRef)
	switch {
	case errors.Is(err, shnsdk.ErrAmbiguousCoveragePayer):
		return "", shnsdk.PayerIdentifier{}, http.StatusUnprocessableEntity, "ambiguous coverage for routing: the coverage names more than one payer"
	case err != nil:
		return "", shnsdk.PayerIdentifier{}, http.StatusUnprocessableEntity, "no payer identifier on member coverage"
	}
	holder, ok := g.cfg.PayerRouter.Resolve(parsed)
	if !ok {
		return "", shnsdk.PayerIdentifier{}, http.StatusUnprocessableEntity,
			fmt.Sprintf("no registered payer for identifier %s|%s", parsed.System, parsed.Value)
	}
	return holder, parsed, 0, ""
}

// recipientFor resolves the payer holder from the patient's own Coverage (FR-G40) using the provider
// SoR as the external-payor resolver — the origination default. Thin wrapper over recipientForWith
// that discards the parsed identity, kept for callers that only route (payerrouting_test.go).
func (g *Gateway) recipientFor(ctx context.Context, coverageJSON []byte) (holderID string, status int, msg string) {
	holder, _, status, msg := g.recipientForSoR(ctx, coverageJSON)
	return holder, status, msg
}

// pendState is the provider's own in-flight PA workflow state for a PENDED
// attestation scenario (UC-06/UC-07), held under an opaque resume token between
// the run-to-PENDED step and the resume-to-APPROVED step. It is the provider's
// orchestration state, never substrate state; the browser only ever holds the
// opaque token. The store is in-memory, Reset-cleared, no TTL — a documented
// single-operator demo simplification (a production EHR would persist + expire).
type pendState struct {
	pasReply   ConsumptionAttempt
	qrSource   *dtrBuildSource
	pasDTRLine string // DTR generation paired with the pinned PAS target.
	dtrLine    string // DTR line selected before population, retained through completion.
	scenario   string // "uc06" or "uc07"
	qrJSON     []byte
	// questionnaireJSON is the bare Questionnaire the pended QR answers (extracted
	// from the fetched $questionnaire-package at origination). The resume legs amend
	// qrJSON with the attested item through shnsdk.AmendQRWithItemIn, which needs it
	// to place that item where the questionnaire puts it — inside its group, or
	// under its parent question's answer — rather than at the top level. In-memory
	// like the rest of pendState.
	questionnaireJSON []byte
	srJSON            []byte
	patientRef        string
	coverageRef       string
	// coverage is the member's OWN Coverage record the pended submit named, pinned
	// at run-to-PENDED: the resume ClaimUpdate is made under the SAME policy, and a
	// payer that stored the authorization under one coverage and is amended under
	// another has two requests rather than one. In-memory like the rest of pendState.
	coverage []byte
	// insurer is the payer's own Organization record the pended submit named, pinned
	// for the same reason the coverage is: the resume ClaimUpdate names the payer the
	// submission named, and the payer scopes an inquiry's search by the insurer.
	insurer []byte
	// member is the BARE member id the pended submit named; the resume ClaimUpdate
	// names the SAME member, so it is pinned here beside coverageRef (which stays
	// the Reference-shaped value the QR-context / native-lane roles need).
	// In-memory like the rest of pendState.
	member string
	// memberSystem is the namespace the participant's own system names that
	// member under, pinned at run-to-PENDED beside member itself: the resume
	// ClaimUpdate names the member exactly as the submission did, so the payer
	// matching an inquiry finds one authorization rather than none.
	memberSystem string
	pci          string
	pasCorr      string
	filled       []FilledItem
	needed       []string
	qrAnswers    map[string]string      // provider-data UC-06: the org-attested base answer trace (1.1/3.1), surfaced in the response as FR-17 mixed-provenance evidence
	payer        shnsdk.PayerIdentifier // the member's REAL payer identity (parsed from OpenCoverage at run-to-PENDED) — threads to the resume ClaimUpdate builders so the payload's payer derives from the patient's real Coverage (FR-G40)
	recipient    string                 // the payer HOLDER id the resume legs route to, resolved from the member's real Coverage at run-to-PENDED (recipientFor) — no default (FR-G40 / AI-G11 / OWD-G10)
	pasToken     string                 // the pa.pas contract token selected at run-to-PENDED — the PENDED-LINE PIN. Threads to the resume pas-claim-update legs as Content.ProfileID so a pended exchange finishes on the line it started on, regardless of registry drift. Lives HERE by settled decision (AI-1: never ExchangeStore); in-memory/Reset-cleared like the recipient pin beside it — a durable pend store inherits it.
	priorClaim   []byte                 // Claim resource from this participant's actual PAS submit, retained for source-complete FR-21 amendment.
	// carriedEntries is the pended pas-claim leg's own declared CARRY record
	// (the multi-version spec's verifyCarryPresent obligation) — the
	// Carried LossEntries the pend's transform chain reported, pinned beside
	// the routed-token pin above and for the same reason: it is cross-leg
	// state that must survive the pend window verbatim, and the ONLY seam
	// that holds any across the strip window. It gives the resume leg's
	// RESTORING chain an independent record to verify the payload against
	// (verifyPendCarryIntact), which a payload-only comparison cannot supply
	// — pasRestoreCarriedExtensions cannot itself tell "never carried" from
	// "carried, then stripped". Additive: empty for every flow this build
	// originates today (no SHN builder emits a 2.2-only top-level Claim
	// extension — transform_pas.go's pas22OnlyClaimExtensions note), so the
	// guard is a no-op with zero cost on every existing path.
	carriedEntries []shnsdk.LossEntry
}

// storePending saves st under a fresh opaque resume token and returns it.
func (g *Gateway) storePending(st pendState) string {
	token := newCorrelationID() // crypto-random hex (16 bytes)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pending[token] = st
	return token
}

// loadPending returns the state for token, if present.
func (g *Gateway) loadPending(token string) (pendState, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st, ok := g.pending[token]
	return st, ok
}

// dropPending deletes token's state (idempotent).
func (g *Gateway) dropPending(token string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.pending, token)
}

// Reset clears the pending-scenario store (called by the devstack admin reset so
// a fresh demo run starts with no stale in-flight PAs).
//
// The two halves have different reach, by design. The pending map is per-process demo
// session state (the two-phase /scenario/* routes), so a reset clears it at the replica
// that received the call and at no other; the exchange Reset below is holder-wide on a
// shared store, so it clears the holder's records everywhere.
//
// That asymmetry is acceptable because of WHERE this is reachable from. The route that
// calls it, POST /scenario/reset, is mounted on the provider role only — beside the rest
// of the /scenario/* demo surface, on a gateway that is never given a public host — and
// the demo session is meant to be driven against one instance anyway (DEPLOYMENT.md says
// so). It is deliberately NOT mounted on the payer role: that mux is the public FHIR
// front door, and an unauthenticated route whose store half is holder-wide has no
// business there. Nothing on the substrate exchange path reads the pending map.
//
// Reset returns the store half's error. A reset that could not clear the holder's
// exchange records is a FAILED reset: reporting success while they are still there is
// how a fresh demo run starts on stale state with nobody told. The caller answers 503.
// The pending map is cleared first and stays cleared either way — clearing it cannot
// fail, it is what the demo session needs, and leaving it stale would add a second
// failure to the one being reported; a retry re-runs both halves.
func (g *Gateway) Reset() error {
	g.mu.Lock()
	g.pending = map[string]pendState{}
	g.mu.Unlock()
	// The store reset runs OUTSIDE g.mu: g.mu guards only the pending map, g.exchanges is
	// write-once in New, and a durable store's Reset is a bounded DB round trip — holding
	// the mutex across it would stall every in-flight pend/resolve for its duration.
	//
	// A fresh demo run starts with no stale exchanges, consistent with the pending map.
	// The Exchange store holds only metadata-only LegRecords. Reset the CONFIGURED
	// store in place — never reassign it, or a shared/durable store silently reverts
	// to a process-local one for the rest of the process's life.
	rs, ok := g.exchanges.(resettableStore)
	if !ok {
		log.Printf("gateway: exchange store: configured store has no Reset; exchanges retained")
		return nil
	}
	if err := rs.Reset(); err != nil {
		log.Printf("gateway: exchange store: reset: %v", err)
		g.noteStoreError(storeErrExchange)
		return fmt.Errorf("exchange store reset: %w", err)
	}
	return nil
}

// The store names StoreErrorMetric is called with (see Config.StoreErrorMetric).
const (
	storeErrExchange   = "exchange"
	storeErrReplay     = "replay"
	storeErrIngressKey = "ingresskey"
	storeErrPended     = "pended"
)

// noteStoreError counts one shared-state store failure. Nil-safe: a gateway with no
// metric hook configured (every hermetic test, and any deployment without
// METRICS_SERVICE) is unaffected.
func (g *Gateway) noteStoreError(store string) {
	if g.cfg.StoreErrorMetric != nil {
		g.cfg.StoreErrorMetric(store)
	}
}

// recordLeg appends a leg to the correlation seam. The seam is best-effort:
// a failure is logged with the exchange id and counted, never returned — a store
// outage must never change what the ingress caller sees.
func (g *Gateway) recordLeg(exchangeID string, rec LegRecord) {
	if err := g.exchanges.AppendLeg(exchangeID, rec); err != nil {
		log.Printf("gateway: exchange store: append leg %s to %s: %v", rec.Type, exchangeID, err)
		g.noteStoreError(storeErrExchange)
	}
}

// ExchangeSnapshot returns a copy of the gateway's current Exchanges (test observability of the
// metadata-only correlation seam). It is a DEV/TEST-ONLY accessor, NOT a stable cross-impl API:
// it returns nil for any store that is not the in-memory impl (a durable store would need its own
// observability), so Gate-1 tests rely on it only against the in-memory default.
func (g *Gateway) ExchangeSnapshot() []Exchange {
	if m, ok := g.exchanges.(*inMemoryExchangeStore); ok {
		return m.snapshot()
	}
	return nil
}

// pendingForPatient returns the resume token of a pended scenario for (scenario,
// pci), if any. Read-only over the same store the two-phase start/complete use.
func (g *Gateway) pendingForPatient(scenario, pci string) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	// Assumes at most one in-flight pend per (scenario, pci); returns an arbitrary match otherwise (map iteration order).
	for token, st := range g.pending {
		if st.scenario == scenario && st.pci == pci {
			return token, true
		}
	}
	return "", false
}

// uc07PendingResp is the patient-facing pending-questionnaire descriptor: the
// Oswestry functional-status item + the opaque resume token (internal — phgsvc
// re-resolves it server-side and never exposes it to the patient app).
type uc07PendingResp struct {
	LinkID      string `json:"linkId"`
	Text        string `json:"text"`
	ResumeToken string `json:"resumeToken"`
}

// handleUC07Pending is the read-only by-patient lookup of a pended UC-07 awaiting
// the patient's functional-status attestation. Returns the Oswestry item + resume
// token, or {} when this patient has none. Internal (provider-gw is not public).
func (g *Gateway) handleUC07Pending(w http.ResponseWriter, r *http.Request) {
	pci := r.URL.Query().Get("patient")
	token, ok := g.pendingForPatient("uc07", pci)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]string{})
		return
	}
	writeJSON(w, http.StatusOK, uc07PendingResp{
		LinkID:      oswestryLinkID,
		Text:        "What is your current Oswestry Disability Index score (0–100)?",
		ResumeToken: token,
	})
}

// handleScenarioReset clears the provider's in-memory pended-scenario store and the
// holder's exchange records so a fresh demo run starts with no stale in-flight PAs.
// Idempotent. A store half that failed answers 503: the caller (the console's reset
// fan-out) shows a failed reset instead of starting a run on records that are still
// there. The body names the store and nothing else — never the database's own error.
func (g *Gateway) handleScenarioReset(w http.ResponseWriter, r *http.Request) {
	if err := g.Reset(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "exchange store reset failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// Handler returns the role-appropriate HTTP routes.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	switch g.cfg.Role {
	case "provider":
		mux.HandleFunc("POST /scenario/uc01", g.handleScenario)
		mux.HandleFunc("POST /scenario/uc02", g.handleUC02)
		// FR-G41 live routing lanes off the /holders feed: uc02-payerb resolves a SECOND payer
		// identity (MBR-PD-UC02-PB / 00078); uc02-unknownpayer fails closed 422 (MBR-UNKNOWN-PAYER /
		// 00099 → no registered payer). Both share the handleUC02 no-PA CRD body. Whether 00078
		// resolves is a deployment fact — see handleUC02PayerB.
		mux.HandleFunc("POST /scenario/uc02-payerb", g.handleUC02PayerB)
		mux.HandleFunc("POST /scenario/uc02-unknownpayer", g.handleUC02UnknownPayer)
		mux.HandleFunc("POST /scenario/uc03", g.handleUC03)
		mux.HandleFunc("POST /scenario/uc04", g.handleUC04)
		mux.HandleFunc("POST /scenario/uc05", g.handleUC05)
		mux.HandleFunc("POST /scenario/uc06", g.handleUC06)
		mux.HandleFunc("POST /scenario/uc07", g.handleUC07)
		mux.HandleFunc("POST /scenario/uc07hcpcs", g.handleUC07HCPCS)
		mux.HandleFunc("POST /scenario/uc08", g.handleUC08)
		mux.HandleFunc("POST /scenario/homeoxygen", g.handleHomeOxygen)
		mux.HandleFunc("POST /scenario/dispatch", g.handleDispatch)
		mux.HandleFunc("POST /scenario/uc06/start", g.handleUC06Start)
		mux.HandleFunc("POST /scenario/uc06/complete", g.handleUC06Complete)
		mux.HandleFunc("POST /scenario/uc06/cancel", g.handleScenarioCancel)
		mux.HandleFunc("POST /scenario/uc07/start", g.handleUC07Start)
		mux.HandleFunc("POST /scenario/uc07/complete", g.handleUC07Complete)
		mux.HandleFunc("POST /scenario/uc07/cancel", g.handleScenarioCancel)
		mux.HandleFunc("GET /scenario/uc07/pending", g.handleUC07Pending)
		// Continue a prior-authorization decision this gateway pended earlier
		// (originate_wait.go). It belongs on this surface and nowhere else: the
		// continuation is server-held state for the gateway's OWN originator
		// flows, and a participant's own system inquires through its own
		// `POST /Claim/$inquire` ingress instead, with no SHN state involved.
		// Like the rest of /scenario/*, it is never publicly routed.
		mux.HandleFunc("POST /scenario/pa/inquire", g.handlePAInquire)
		// Admin reset of the in-memory pended-scenario store. The in-process devstack
		// calls g.Reset() directly; in the SEPARATED deployment the console reset hits
		// this route so a pended UC-06/07 does not survive as an orphaned questionnaire
		// (which would 502 on a stale patient submit). Internal — provider-gw is not public.
		mux.HandleFunc("POST /scenario/reset", g.handleScenarioReset)
		if g.cfg.IngressEnabled {
			// Every ingress answer carries X-Correlation-Id (correlationheader.go):
			// the id the leg is logged under, settled before the handler runs.
			mux.HandleFunc("GET /cds-services", g.withIngressCorrelation(g.handleCDSDiscovery))
			mux.HandleFunc("POST /cds-services/{id}", g.observeIngress("crd-ingress", g.withIngressCorrelation(g.handleCRDIngress)))
			mux.HandleFunc("POST /Questionnaire/$questionnaire-package", g.observeIngress("dtr-ingress", g.withIngressCorrelation(g.handleDTRIngress)))
			mux.HandleFunc("POST /Claim/$submit", g.observeIngress("pas-ingress", g.withIngressCorrelation(g.handlePASIngress)))
			mux.HandleFunc("POST /Claim/$inquire", g.observeIngress("pas-inquire-ingress", g.withIngressCorrelation(g.handlePASInquireIngress)))
			// FR-37: the ingress edge's own CapabilityStatement (per-role
			// statements — the payer's /metadata precedent at gateway.go:517).
			mux.HandleFunc("GET /metadata", g.handleIngressMetadata)
			if g.ingressAuth != nil {
				mux.HandleFunc("POST /oauth/token", g.ingressAuth.handleToken)
				mux.HandleFunc("GET /.well-known/smart-configuration", g.ingressAuth.handleSmartConfig)
			}
			if g.cfg.IngressBaseURL != "" {
				// HRex 1.2.0 endpoint discovery:
				// version-specific endpoint codes for the Da Vinci-facing edge.
				mux.HandleFunc("GET /.well-known/davinci-configuration", g.handleDavinciConfiguration)
			}
		}
	case "payer":
		mux.HandleFunc("POST /substrate/inbound", g.observeInbound(g.handleInbound))
		if g.cfg.PayerEOBActionsEnabled {
			mux.HandleFunc("POST /local/payer/eob-record", g.handlePayerEOBRecord)
			if g.ingressAuth != nil {
				mux.HandleFunc("POST /oauth/token", g.ingressAuth.handleToken)
				mux.HandleFunc("GET /.well-known/smart-configuration", g.ingressAuth.handleSmartConfig)
			}
		}
		// FR-28: CMS-0057 Patient Access API — conformant FHIR search + instance read
		// over the PDex PA EOB, gated by a patient-access authority token. Distinct
		// from the sealed substrate legs. FR-37: the CapabilityStatement for this
		// surface is published at the standard FHIR /metadata endpoint.
		mux.HandleFunc("GET /metadata", g.handlePatientAccessMetadata)
		mux.HandleFunc("GET /ExplanationOfBenefit", g.handlePatientAccessEOB)
		mux.HandleFunc("GET /ExplanationOfBenefit/{id}", g.handlePatientAccessEOBByID)
	case "facility":
		mux.HandleFunc("POST /substrate/inbound", g.observeInbound(g.handleInbound))
	case "phg":
		mux.HandleFunc("POST /substrate/inbound", g.observeInbound(g.handleInbound))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(diagnostics.WithBodyBudget(r.Context(), &g.observationMemory))
		done := g.operations.begin()
		defer done()
		mux.ServeHTTP(w, r)
	})
}

// EnableIngressForTest enables the Da Vinci ingress routes AND the test-only auth bypass on
// cfg. It is the ONLY affordance that sets ingressAuthBypass; build()/main MUST never call it
// (enforced by ingress_imagepurity_test.go — the scaffold image-purity pattern).
func EnableIngressForTest(cfg *Config) {
	cfg.IngressEnabled = true
	cfg.ingressAuthBypass = true
}

// ingressAuthOK gates every ingress route. Real SMART Backend Services bearer
// verification at the gateway edge; the test-only bypass is the only other path to
// true (build-time-absent). Nil-safe: a Gateway with no auth server (zero value, or
// ingress disabled) fails closed WITHOUT panicking.
//
// unavailable says the refusal is a shared-store OUTAGE (the key store could not resolve
// a well-formed kid), which the route answers with 503 instead of a 401 — see
// ingressAuthRefused.
func (g *Gateway) ingressAuthOK(r *http.Request) (bool, bool) {
	_, ok, unavailable := g.ingressPrincipal(r)
	return ok, unavailable
}

// ingressPrincipal returns identity only after verifying one of the existing
// credential forms. The test bypass conveys no registered connector identity.
func (g *Gateway) ingressPrincipal(r *http.Request) (IngressPrincipal, bool, bool) {
	if g.cfg.ingressAuthBypass {
		return IngressPrincipal{}, true, false
	}
	if g.ingressAuth == nil {
		return IngressPrincipal{}, false, false
	}
	p, ok, unavailable := g.ingressAuth.verifyBearerPrincipal(r)
	if ok {
		return p, true, false
	}
	if p, ok := g.ingressAuth.verifyDirectBearerPrincipal(r); ok {
		return p, true, false
	}
	return IngressPrincipal{}, false, unavailable
}

// ingressAuthRefused writes the refusal an ingress route owes when the caller is not
// admitted, and reports whether it wrote one (the handler must then stop). A key-store
// outage answers the 503 every other shared-state refusal answers — the same shape the
// token endpoint's oauthUnavailable uses — never the 401 that tells an honest partner its
// credential is bad. The ingress client is the one that decides to retry; this call is
// refused.
func (g *Gateway) ingressAuthRefused(w http.ResponseWriter, r *http.Request) bool {
	ok, unavailable := g.ingressAuthOK(r)
	if ok {
		return false
	}
	if unavailable {
		writeStoreUnavailable(w, "ingress key store unavailable")
		return true
	}
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "ingress authentication required"})
	return true
}

// writeStoreUnavailable is the refusal a route answers when a shared-state record could
// not be consulted: 503 (not a 401/403 denial, which would accuse an honest caller of a
// bad credential), uncacheable so nothing pins the outage answer past the outage.
//
// 503 states the CAUSE; it does not promise the request survives. Whether anything
// retries is the caller's business, and it differs per route: a partner client calling
// the token endpoint or an ingress route can retry (with a fresh client_assertion at the
// token endpoint — see oauthUnavailable), while a Hub-forwarded delivery cannot, because
// the Hub has no retry and turns any non-2xx from /substrate/inbound into its own 502
// "forward to recipient failed". The envelope shape is this package's
// ordinary {"error": …}, or OperationOutcome on a FHIR operation; the token
// endpoint's OAuth2 twin is oauthUnavailable.
func writeStoreUnavailable(w http.ResponseWriter, msg string) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": msg})
}

// randRead is crypto/rand.Read, indirected so a test can drive the failure branch
// below (the in-memory exchange store's Begin and the pg mirror's must refuse
// identically). Never reassigned outside tests.
var randRead = rand.Read

func newCorrelationID() string {
	var b [16]byte
	if _, err := randRead(b[:]); err != nil {
		// crypto/rand failing is unrecoverable; fail closed (never emit a weak or
		// empty correlation id that would undermine per-leg binding/replay defenses).
		panic(fmt.Sprintf("gateway: crypto/rand failed generating correlation id: %v", err))
	}
	return hex.EncodeToString(b[:])
}

// tokenJSON marshals an shnsdk.Token to a JSON string for carriage in envelope
// metadata (AuthzToken is a string field).
func tokenJSON(t shnsdk.Token) (string, error) {
	b, err := json.Marshal(t)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// sha256hex returns the lowercase hex-encoded SHA-256 of b. It is the payload-hash
// the gateway binds into an authz token (AI-2): the recipient recomputes it over
// the ciphertext it received and asserts it matches token.PayloadHash.
func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type authorizeReq struct {
	Frame         string `json:"frame"`
	Operation     string `json:"operation"`
	SubjectPCI    string `json:"subjectPCI"`
	Custodian     string `json:"custodian,omitempty"`
	CorrelationID string `json:"correlationId,omitempty"`
	// PayloadHash is sha256hex(envelope ciphertext): the gateway seals the payload
	// FIRST, then authorizes against that exact ciphertext so the minted token binds
	// THIS payload (AI-2). Empty for non-envelope ops (patient-access-read).
	PayloadHash string `json:"payloadHash,omitempty"`
}

type authorizeResp struct {
	Token shnsdk.Token `json:"token"`
}

// errAuthorizationDenied marks an authority DENIAL — a 403 from the Authorization
// Framework, e.g. the UC-05 consent gate refusing a federated query — as distinct
// from a transport/integrity failure. Callers that have a legitimate "denied" path
// (UC-05's no-consent branch leaves the PA pended) check errors.Is against this so
// they never report a facility outage or a tampered response as "consent denied".
var errAuthorizationDenied = errors.New("authorization denied")

// LegOutcome values passed to Config.LegMetric: "routed" when an
// origination leg is attempted, then exactly one terminal outcome.
const (
	LegOutcomeRouted      = "routed"
	LegOutcomeAnswered    = "answered"
	LegOutcomeDenied      = "denied"
	LegOutcomeUnreachable = "unreachable"
	LegOutcomeFailed      = "failed"
)

// errHubUnreachable marks a Hub-leg transport/routing failure (the Hub could
// not be reached or refused to route). Same user-facing message as before —
// it is now a typed sentinel so roundTrip can classify the leg outcome as
// "unreachable" rather than the opaque "failed".
var errHubUnreachable = errors.New("hub routing failed")

// errHubTimeout marks a Hub leg that produced no answer within the wait the
// originating gateway gave it: the Hub, the counterpart's gateway or the
// counterpart's own system took longer than the leg's timeout budget. It is a
// transport failure like errHubUnreachable (the leg did not complete, so the
// outcome stays "unreachable"), but the caller reads a 504 that states the
// fact and the budget instead of the generic routing failure. Match it with
// errors.Is; the value returned is a *hubTimeoutError carrying the budget.
var errHubTimeout = errors.New("hub leg timed out")

// hubTimeoutError is the error a timed-out Hub leg returns. budget is the
// gateway's own leg deadline when that is what ended the wait; zero when the
// caller's request deadline ended it first, in which case no number is
// claimed.
type hubTimeoutError struct{ budget time.Duration }

func (e *hubTimeoutError) Error() string {
	if e.budget <= 0 {
		return errHubTimeout.Error()
	}
	return fmt.Sprintf("no answer on the hub leg within %s (hub leg timeout)", e.budget)
}

func (e *hubTimeoutError) Is(target error) bool { return target == errHubTimeout }

// classifyHubLegError names a failed POST to the Hub from the deadlines the
// gateway owns, never from the shape of the transport error. The gateway
// posts under legCtx, its own deadline of budget (the client's Timeout);
// the number is claimed only when that deadline ended the wait while the
// caller's ctx was still live. A caller whose own deadline expired first
// ended the wait itself, so the timeout is stated without a number. Every
// other failure — including a dial or TLS handshake that timed out before
// the Hub was reached, or an error merely shaped like a timeout — is the
// generic routing failure: the gateway cannot say the leg was under way.
func classifyHubLegError(ctx, legCtx context.Context, budget time.Duration) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &hubTimeoutError{}
	}
	if ctx.Err() == nil && errors.Is(legCtx.Err(), context.DeadlineExceeded) {
		return &hubTimeoutError{budget: budget}
	}
	return errHubUnreachable
}

// legMetric dispatches one LegOutcome value to the configured hook; nil-safe.
func (g *Gateway) legMetric(outcome string) {
	if g.cfg.LegMetric != nil {
		g.cfg.LegMetric(outcome)
	}
}

// authorize fetches a scope-bound token from the Authorization Framework. The
// correlationID binds the minted token to the envelope it will ride in (C2).
// custodian is forwarded for federated-query operations so the Authorization
// Framework can resolve consent for the specific source facility; it is empty
// for all other operations (provider↔payer exchanges).
func (g *Gateway) authorize(r *http.Request, frame, operation, subjectPCI, correlationID, custodian, payloadHash string) (shnsdk.Token, error) {
	// H1: authenticate to the Authorization Framework with a holder assertion for
	// the "authz" audience so the policy can bind authority to THIS holder. The
	// provider authorizes as "provider", the payer as "payer" (via cfg.HolderID).
	assertion := shnsdk.IssueAssertion(g.cfg.HolderID, "authz", g.cfg.Identity.SignPriv, g.cfg.Clock(), time.Hour)
	assertionJSON, err := json.Marshal(assertion)
	if err != nil {
		return shnsdk.Token{}, err
	}
	headers := map[string]string{
		"X-Holder-Assertion": base64.StdEncoding.EncodeToString(assertionJSON),
	}

	var out authorizeResp
	err = shnsdk.PostJSON(r.Context(), g.cfg.Client, g.cfg.AuthzURL+"/authorize",
		authorizeReq{Frame: frame, Operation: operation, SubjectPCI: subjectPCI, Custodian: custodian, CorrelationID: correlationID, PayloadHash: payloadHash}, &out, headers)
	if err != nil {
		// A 403 is a policy/consent DENIAL (not a transport failure); surface it as
		// the typed sentinel so callers can distinguish it from the Authorization
		// Framework being unreachable or erroring (502-class).
		var se *shnsdk.StatusError
		if errors.As(err, &se) && se.Code == http.StatusForbidden {
			return shnsdk.Token{}, errAuthorizationDenied
		}
		return shnsdk.Token{}, err
	}
	return out.Token, nil
}

// postEnvelope POSTs an encoded envelope and the holder assertion header to url,
// decoding the response body as an Envelope.
func (g *Gateway) postEnvelope(ctx context.Context, client *http.Client, url string, body []byte, assertionHeader string) (shnsdk.Envelope, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return shnsdk.Envelope{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Holder-Assertion", assertionHeader)

	resp, err := client.Do(req)
	if err != nil {
		return shnsdk.Envelope{}, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, shnsdk.MaxResponseBytes))
	if err != nil {
		return shnsdk.Envelope{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if g.cfg.Diagnostic != nil {
			headers, complete := diagnosticHeaders(resp.Header)
			g.diagnosticEvent(ctx, diagnostics.Event{Kind: "leg.failed", Status: resp.StatusCode, Body: respBody, BodyComplete: len(respBody) < shnsdk.MaxResponseBytes, Headers: headers, HeadersComplete: complete, Detail: "Hub response"})
		}
		return shnsdk.Envelope{}, fmt.Errorf("gateway: hub returned %d: %s", resp.StatusCode, string(respBody))
	}
	return shnsdk.DecodeEnvelope(respBody)
}

// roundTrip performs one authorized sealed exchange with the counterpart
// holder through the Hub (see roundTripInner for the mechanics). It is ALSO
// the observer seam's origination choke point: every origination leg emits
// leg.originated before the exchange and leg.response / leg.failed after —
// one seam covers every UC flow in both lanes (see STABILITY.md).
//
// The request payload is checked against the leg's ownership row before
// anything is observed or sent; a refused payload is returned as an error that
// isOwnershipFault recognizes (the caller answers a 500 local fault).
func (g *Gateway) roundTrip(ctx context.Context, r *http.Request, recipient, reqFrame, respFrame, op, respOp, txType, scope, pci, correlationID, custodian string, content Content) ([]byte, error) {
	reply, err := g.roundTripMessage(ctx, r, recipient, reqFrame, respFrame, op, respOp, txType, scope, pci, correlationID, custodian, content)
	return reply.legacy(txType, err)
}

func (g *Gateway) roundTripMessage(ctx context.Context, r *http.Request, recipient, reqFrame, respFrame, op, respOp, txType, scope, pci, correlationID, custodian string, content Content) (ApplicationReply, error) {
	if g.cfg.Diagnostic != nil {
		ctx = context.WithValue(ctx, diagnosticLegKey{}, &diagnosticLeg{sender: g.cfg.HolderID, recipient: recipient, correlation: correlationID})
	}
	requestBytes, err := g.admit(content.Payload, requestKey(txType, content.Carried))
	if err != nil {
		g.diagnosticStage(ctx, "relay.ownership-refused", txType, nil, 500, err.Error())
	}
	if err != nil {
		return ApplicationReply{}, err
	}
	g.observe(ObserverEvent{
		Kind: "leg.originated", Direction: "originate", LegType: txType,
		CorrelationID: correlationID, Counterpart: recipient,
		AuthorityFrame: reqFrame, Op: op, Payload: json.RawMessage(requestBytes),
		Route: content.Route,
	})
	g.diagnosticStage(ctx, "leg.originated", txType, requestBytes, 0, "")
	g.legMetric(LegOutcomeRouted)
	respPayload, err := g.roundTripInner(ctx, r, recipient, reqFrame, respFrame, op, respOp, txType, scope, pci, correlationID, custodian, content)
	if err != nil {
		outcome := LegOutcomeFailed
		switch {
		case errors.Is(err, errAuthorizationDenied):
			outcome = LegOutcomeDenied
		case errors.Is(err, errHubUnreachable), errors.Is(err, errHubTimeout):
			outcome = LegOutcomeUnreachable
		}
		g.diagnosticStage(ctx, "leg.failed", txType, nil, 0, err.Error())
		g.legMetric(outcome)
		g.observe(ObserverEvent{
			Kind: "leg.failed", Direction: "originate", LegType: txType,
			CorrelationID: correlationID, Counterpart: recipient, Detail: err.Error(),
		})
		return ApplicationReply{}, err
	}
	responseBytes, err := respPayload.bytes(txType)
	if err != nil {
		return ApplicationReply{}, err
	}
	g.observe(ObserverEvent{
		Kind: "leg.response", Direction: "originate", LegType: txType,
		CorrelationID: correlationID, Counterpart: recipient,
		AuthorityFrame: respFrame, Op: respOp, Status: respPayload.Status, Payload: json.RawMessage(responseBytes),
	})
	g.diagnosticStage(ctx, "leg.response", txType, responseBytes, respPayload.Status, "")
	g.legMetric(LegOutcomeAnswered)
	return respPayload, nil
}

// roundTripInner performs one authorized sealed exchange with the counterpart holder
// through the Hub: authorize(op) → seal(txType/reqFrame) → POST hub
// /route with a holder assertion → verify the response leg (VerifyBound respOp
// + the SAME correlationID + Sender==recipient, envelope CorrelationID match) →
// decrypt and return the response payload. recipient is the counterpart holder
// id (payer legs derive it from the patient's Coverage via recipientFor — no default;
// facility/phg legs pass a LookupByRole result).
// reqFrame/respFrame are the authority frames for the request and response legs
// respectively (provider→payer uses "provider-tpo"/"payer-coverage"; provider→
// facility uses "provider-tpo"/"facility-disclosure"). custodian is forwarded to
// the Authorization Framework for federated-query operations (consent gate); it
// is empty for all other operations. The scope param documents the policy-derived
// min-necessary scope for this exchange; the authz service derives the actual
// scope from policy, so it is not sent on the wire.
func (g *Gateway) roundTripInner(ctx context.Context, r *http.Request, recipient, reqFrame, respFrame, op, respOp, txType, scope, pci, correlationID, custodian string, content Content) (ApplicationReply, error) {
	_ = scope // policy-derived server-side; kept for contract clarity
	// The request is checked against the leg's ownership row at the boundary
	// itself (content.ProfileID is read below to verify the response frame's
	// stamp).
	reqKey := requestKey(txType, content.Carried)
	payload, err := relay.Transmit(content.Payload, relay.Check(reqKey))
	if err != nil {
		g.ownershipRefused(reqKey, err)
		return ApplicationReply{}, err
	}

	recipientHolder, ok := g.cfg.Reg.Lookup(recipient)
	if !ok {
		return ApplicationReply{}, fmt.Errorf("recipient %q not in registry", recipient)
	}

	// Request framing — the REQUEST-line claim. Once payloads genuinely
	// differ per line, "which line is this request built at?" can no longer be
	// re-derived by the receiver: a pended leg resumes on a PINNED line that a
	// fresh recomputation would not reproduce. So the originator states it, in the
	// frame the sealed-envelope machinery already carries, INSIDE the seal (the Hub still
	// sees only ciphertext).
	//
	// Framed when the recipient advertises requestFrames v1, even if the
	// producer supplied no version declaration. A peer that never declares it receives BYTE-IDENTICAL bare
	// requests — the additive-in-both-directions fence
	// (TestRequestNotFramedToNonDeclaringPeer).
	//
	// The frame's status field is inert on a request (it exists for answers); 200
	// is the filler the codec requires (100..599) and no receiver reads it.
	//
	// A DTR operation (content.Operation) is always framed, with the operation
	// header, and only to a recipient that declares v1op: a receiver that does
	// not know the header drops it and would misread the body.
	if content.CRDHook != "" {
		if !validCRDHook(txType, content.CRDHook) {
			return ApplicationReply{}, contextError(http.StatusForbidden, "context_invalid")
		}
		if !shnsdk.SupportsRequestFrameV1CRD(recipientHolder.RequestFrames) {
			return ApplicationReply{}, errFramedCRDUnsupported
		}
	}
	if content.Operation != "" && !shnsdk.SupportsRequestFrameV1Op(recipientHolder.RequestFrames) {
		return ApplicationReply{}, errFramedDTRUnsupported
	}
	if content.CRDHook != "" || content.Operation != "" || shnsdk.SupportsRequestFrameV1(recipientHolder.RequestFrames) {
		headers := map[string]string{
			"Content-Type":                    content.Payload.ContentType(),
			shnsdk.FrameHeaderContractVersion: requestVersion(content),
		}
		if content.Operation != "" {
			headers[shnsdk.FrameHeaderOperation] = content.Operation
		}
		if content.CRDHook != "" {
			headers[shnsdk.FrameHeaderCRDHook] = content.CRDHook
		}
		framed, ferr := shnsdk.EncodeHTTPFrameHeaders(http.StatusOK, headers, payload)
		if ferr != nil {
			return ApplicationReply{}, fmt.Errorf("request frame encode failed")
		}
		payload = framed
	}

	// AI-2 (seal-then-authorize): seal the payload FIRST so the ciphertext exists,
	// then authorize against sha256hex(ciphertext) so the minted token is BOUND to
	// THIS exact payload. AuthzToken/ConsentRef are cleartext metadata (Seal encrypts
	// only the payload), so they are stamped onto the envelope AFTER minting.
	meta := shnsdk.Metadata{
		Sender:          g.cfg.HolderID,
		Recipient:       recipient,
		TransactionType: txType,
		AuthorityFrame:  reqFrame,
		Timestamp:       g.cfg.Clock().Format(time.RFC3339),
		CorrelationID:   correlationID,
	}
	env, err := shnsdk.Seal(meta, payload, recipientHolder.EncPub)
	if err != nil {
		return ApplicationReply{}, fmt.Errorf("seal failed")
	}

	if leg, ok := ctx.Value(diagnosticLegKey{}).(*diagnosticLeg); ok {
		leg.hash = sha256hex(env.Ciphertext)
	}
	g.diagnostic(diagnostics.Event{Kind: "leg.sealed", CallID: diagnostics.CallID(ctx), RequestFingerprint: diagnostics.IngressFingerprint(ctx), RequestCiphertextHash: sha256hex(env.Ciphertext), Sender: g.cfg.HolderID, Recipient: recipient, CorrelationID: correlationID, LegType: txType, ContractLine: content.ProfileID, Body: payload, BodyComplete: true})
	tok, err := g.authorize(r, reqFrame, op, pci, correlationID, custodian, sha256hex(env.Ciphertext))
	if err != nil {
		// Preserve a genuine authority DENIAL as the typed sentinel (UC-05's
		// no-consent branch depends on telling it apart from an authz outage); any
		// other authorize failure stays an opaque "authorization failed".
		if errors.Is(err, errAuthorizationDenied) {
			return ApplicationReply{}, errAuthorizationDenied
		}
		return ApplicationReply{}, fmt.Errorf("authorization failed")
	}
	tokStr, err := tokenJSON(tok)
	if err != nil {
		return ApplicationReply{}, fmt.Errorf("token marshal failed")
	}
	env.Metadata.AuthzToken = tokStr
	env.Metadata.ConsentRef = tok.ConsentRef // empty for non-federated exchanges

	body, err := shnsdk.EncodeEnvelope(env)
	if err != nil {
		return ApplicationReply{}, fmt.Errorf("encode failed")
	}

	assertion := shnsdk.IssueAssertion(g.cfg.HolderID, "hub", g.cfg.Identity.SignPriv, g.cfg.Clock(), time.Hour)
	assertionJSON, err := json.Marshal(assertion)
	if err != nil {
		return ApplicationReply{}, fmt.Errorf("assertion marshal failed")
	}
	assertionHeader := base64.StdEncoding.EncodeToString(assertionJSON)

	// The gateway owns the leg's deadline: the client's Timeout is applied as
	// a context deadline on this POST (the client itself is used without its
	// Timeout so exactly one timer decides), and a failure is named from which
	// deadline fired — see classifyHubLegError.
	legCtx, budget := ctx, g.cfg.Client.Timeout
	hubClient := g.cfg.Client
	if budget > 0 {
		var cancel context.CancelFunc
		legCtx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
		untimed := *g.cfg.Client
		untimed.Timeout = 0
		hubClient = &untimed
	}
	respEnv, err := g.postEnvelope(legCtx, hubClient, g.cfg.HubURL+"/route", body, assertionHeader)
	if err != nil {
		err = classifyHubLegError(ctx, legCtx, budget)
		if errors.Is(err, errHubTimeout) {
			log.Printf("gateway: hub leg %s to %q timed out: %s (correlation %s)", txType, recipient, err.Error(), correlationID)
		}
		return ApplicationReply{}, err
	}

	// C1/H2b: the response leg must be authorized just like the request leg, bound
	// to the ORIGINAL request correlationID (not the response envelope's own CID),
	// and must come from the expected counterpart holder.
	var respTok shnsdk.Token
	if err := json.Unmarshal([]byte(respEnv.Metadata.AuthzToken), &respTok); err != nil {
		return ApplicationReply{}, fmt.Errorf("response leg authorization failed")
	}
	// H1: the response token's Holder must be the responder (the counterpart). The
	// envelope Sender is asserted == recipient just below, so pinning the token's
	// holder to respEnv.Metadata.Sender stops a token minted for another holder
	// being lifted into the counterpart's response. The pci the provider authorized
	// this REQUEST with is pinned as the response token's subject: a payer-coverage
	// response token for a DIFFERENT patient under the same correlation is rejected
	// (H1).
	if err := shnsdk.VerifyBound(respTok, g.cfg.AuthzPub, g.cfg.Clock(),
		respFrame, respOp, correlationID, respEnv.Metadata.Sender, pci, sha256hex(respEnv.Ciphertext)); err != nil {
		return ApplicationReply{}, fmt.Errorf("response leg authorization failed")
	}
	if respEnv.Metadata.CorrelationID != correlationID {
		return ApplicationReply{}, fmt.Errorf("response correlation mismatch")
	}
	if respEnv.Metadata.Sender != recipient {
		return ApplicationReply{}, fmt.Errorf("response sender mismatch")
	}

	respPayload, err := shnsdk.Open(respEnv, g.cfg.Identity.EncPub, g.cfg.Identity.EncPriv)
	if err != nil {
		return ApplicationReply{}, fmt.Errorf("response decryption failed")
	}
	// A frame-negotiated recipient (registry messageFrames) seals
	// EVERY application answer — any status — as a v1 message frame. The raw
	// API preserves it; legacy wrappers convert non-2xx to *RelayError.
	//
	// Decode ANY payload bearing the frame magic, regardless of the recipient's
	// advertised frames (hardened at final review). This is safe by the spec's own
	// collision argument — 0x00 cannot begin any bare payload we carry (JSON/X12/XML/
	// HL7v2 text) — and closes the INVERSE stale-feed window: a responder that
	// correctly frames to a v1-advertising requester while OUR view of the recipient
	// is still pre-upgrade (dynamic re-registration; a rolling deploy where provision
	// stamps capability off the registrar's build). The recipient's advertised frames
	// govern only expectation/observability — an advertised-but-bare answer is the
	// (forward) stale-feed downgrade, logged loudly AND emitted on the observer seam.
	if shnsdk.IsFramed(respPayload) {
		hdr, body, ferr := shnsdk.DecodeHTTPFrame(respPayload)
		if ferr != nil {
			return ApplicationReply{}, fmt.Errorf("response frame decode failed")
		}
		if hdr.Headers[shnsdk.FrameHeaderCRDHook] != "" {
			return ApplicationReply{}, fmt.Errorf("CRD hook is request-only")
		}
		// Preserve authenticated producer metadata. The registered version-consistency
		// rule applies the participant's policy after transport completes.
		version := hdr.Headers[shnsdk.FrameHeaderContractVersion]
		source := ""
		if version != "" {
			source = "producer"
		}
		return ApplicationReply{Status: hdr.Status, Payload: relay.Exact(relay.NewBody(body, relay.OriginPeerFrame), hdr.Headers["Content-Type"]), DeclaredVersion: version, VersionSource: source}, nil
	}
	if shnsdk.SupportsMessageFrameV1(recipientHolder.MessageFrames) {
		log.Printf("gateway: recipient %q advertises frame v1 but answered bare; processing as legacy (stale-feed downgrade)", recipient)
		g.observe(ObserverEvent{
			Kind: "leg.downgrade", Direction: "originate", LegType: txType,
			CorrelationID: correlationID, Counterpart: recipient, Op: respOp,
			Detail: "recipient advertises frame v1 but answered bare; processing as legacy (stale-feed downgrade)",
		})
	}
	// Legacy path: a non-frame-negotiated recipient answers a bare application payload
	// (pre-v0.27.0 contract) — a 2xx success body the caller consumes as-is; the Hub
	// reports any application non-2xx as its own generic mechanical fault.
	return ApplicationReply{Status: http.StatusOK, Payload: relay.Exact(relay.NewBody(respPayload, relay.OriginPeerFrame), "")}, nil
}

// requestKey is the transmit a requester's request on leg is checked against:
// carried from its participant's system, or originated by its own workflow.
func requestKey(leg string, carried bool) relay.Key {
	outcome := relay.OutcomeOriginated
	if carried {
		outcome = relay.OutcomeCarried
	}
	return relay.Key{Leg: leg, Role: relay.RoleRequester, Direction: relay.DirectionRequest, Outcome: outcome}
}

// OriginateLeg is the origination leg-primitive: it runs one authorized, sealed,
// Hub-routed exchange for legType, reading the authority frames / operations / scope
// from `paCatalog` (the PA Layer-3 module) instead of taking them as positional
// literals. recipient stays a parameter (payer legs derive it from the patient's Coverage
// via recipientFor — no default; facility/phg legs target a LookupByRole result). An
// unknown legType is a caller bug (the Originator
// only passes catalog legTypes) and fails closed with an error.
//
// The content.WorkstreamType guard is the SELECTION SEAM in embryo: today exactly one
// module exists, so it fail-closes anything not tagged workstreamPA; when a second
// workstream lands this becomes `catalogFor(content.WorkstreamType)`. So the catalog is
// single-source-of-truth across both edges NOW (origination + the handleInbound
// FulfillLeg dispatch read the same `paCatalog`, hence "cannot drift"), and
// module-neutral LATER — this seam is reserved but not yet code-enforced (the primitive
// still names `paCatalog`). This is the origination MIRROR of the payer-side FulfillLeg
// pattern.
func (g *Gateway) OriginateLeg(ctx context.Context, r *http.Request, recipient, legType, pci, correlationID, custodian string, content Content) ([]byte, error) {
	reply, err := g.OriginateLegMessage(ctx, r, recipient, legType, pci, correlationID, custodian, content)
	return reply.legacy(legType, err)
}

// OriginateLegMessage runs the same authorized exchange and returns the peer's
// actual answer independently of transport or local refusal errors.
func (g *Gateway) OriginateLegMessage(ctx context.Context, r *http.Request, recipient, legType, pci, correlationID, custodian string, content Content) (ApplicationReply, error) {
	ctx = diagnostics.WithBodyBudget(ctx, &g.observationMemory)
	if content.WorkstreamType != workstreamPA {
		return ApplicationReply{}, fmt.Errorf("OriginateLeg: content workstream %q not served by this gateway", content.WorkstreamType)
	}
	spec, ok := paCatalog[legType]
	if !ok {
		return ApplicationReply{}, fmt.Errorf("OriginateLeg: unknown legType %q", legType)
	}
	// Version filter: select the highest common contract
	// line for this leg, fail-closed and legible when none is shared. A
	// non-empty ProfileID is the PENDED-LINE PIN (set at run-to-PENDED, stored
	// in pendState — AI-1 keeps it out of ExchangeStore) and is honored
	// verbatim: a resume leg never re-negotiates. The selected token is the
	// reserved IR field's intended writer/reader pair: it drives the response-
	// frame stamp check now and builder/validator selection in slices 4-5.
	// This fallback stays ARM-1-ONLY (selectLegToken,
	// intersection-only) BY CONSTRUCTION — callers reaching here already built
	// their payload at SOME line with no egress-adapt run, so an arm-2/3 token
	// would mis-stamp bytes built at another line. selectLegLine (the
	// select-before-build primitive, originate.go) is where the reachability
	// arms live; this legacy/neutral-caller path never routes through it.
	if content.Carried && !g.canCarryNative(recipient, legType, content.DeclaredVersion) {
		contract, _ := legContract(legType)
		peer, _ := g.cfg.Reg.Lookup(recipient)
		return ApplicationReply{}, &RouteRefusalError{Contract: contract, LegType: legType, Recipient: recipient,
			Own:  sortedTokens(contract, contractLineSet(g.declaredContractVersions(), contract)),
			Peer: sortedTokens(contract, contractLineSet(peer.ContractVersions, contract)), BridgeIssue: "no registered native endpoint for this operation and representation"}
	}
	if !content.Carried && content.ProfileID == "" {
		tok, err := g.selectLegToken(recipient, legType)
		if err != nil {
			var rre *RouteRefusalError
			var ri *RouteInfo
			if errors.As(err, &rre) {
				ri = refusalRouteInfo(rre)
			}
			g.observe(ObserverEvent{
				Kind: "leg.refused", Direction: "originate", LegType: legType,
				CorrelationID: correlationID, Counterpart: recipient, Detail: err.Error(),
				Route: ri,
			})
			return ApplicationReply{}, err
		}
		content.ProfileID = tok
		// content.Route stays nil here by construction: selectLegToken is arm-1-
		// only and nothing was SELECTED for this leg in the routeInfoFor sense
		// (no BuildLine/Chain decision was made, just an intersection token) —
		// the caller already built its payload at some line before reaching
		// OriginateLeg, so there is no route story to synthesize after the fact.
		// roundTrip's leg.originated therefore carries Route: nil on this path.
	}

	ex, supplied := ctx.Value(nativeExchangeKey{}).(ExchangeContext)
	if !supplied {
		ex = ExchangeContext{holder: g.cfg.HolderID, recipient: recipient, legType: legType, subjectPCI: pci, correlationID: correlationID, custodian: custodian, operation: content.Operation, contractVersion: requestVersion(content), contentType: content.Payload.ContentType(), policy: g.policy()}
	}
	raw, err := g.admit(content.Payload, requestKey(legType, content.Carried))
	if err != nil {
		if g.cfg.Diagnostic != nil {
			ctx = context.WithValue(ctx, diagnosticLegKey{}, &diagnosticLeg{sender: g.cfg.HolderID, recipient: recipient, correlation: correlationID})
		}
		g.diagnosticStage(ctx, "relay.ownership-refused", legType, nil, 500, err.Error())
		return ApplicationReply{}, err
	}
	requestOwner := "own"
	if content.Payload.Ownership() == relay.OwnershipEdited {
		requestOwner = "network"
	}
	if err := g.enforceContent(ctx, CheckInput{Exchange: ex, Direction: "request", Body: raw, DeclaredVersion: ex.contractVersion,
		finding: contentFinding(ctx, ex, requestOwner, "originate")}); err != nil {
		return ApplicationReply{}, err
	}
	reply, err := g.roundTripMessage(ctx, r, recipient, spec.ReqFrame, spec.RespFrame, spec.Op, spec.RespOp, legType, spec.Scope, pci, correlationID, custodian, content)
	if err != nil {
		return reply, err
	}
	raw, err = reply.bytes(legType)
	if err != nil {
		return reply, err
	}
	err = g.enforceContent(ctx, CheckInput{Exchange: ex, Direction: "response", Status: reply.Status, Body: raw, DeclaredVersion: reply.DeclaredVersion,
		finding: contentFinding(ctx, ex, "peer", "originate")})
	return reply, err
}

// validatorForLine resolves the $validate lane for a contract LINE ("2.0", "2.1",
// "2.2"; "" = no line in play — a version-neutral leg or a non-contract resource).
// A HAPI instance hosts exactly ONE version of an IG, so per-line validation
// needs per-line lanes, never one lane reused.
//
// Resolution order, and why:
//   - an explicitly configured lane for the line always wins;
//   - a deployment with NO lanes configured (ValidatorsByLine empty) serves every
//     line from Config.Validator — this is the pre-multi-line wiring every in-process
//     test and the scenario harness use, and it must keep behaving identically;
//   - a deployment WITH lanes configured is authoritative: a line it did not
//     configure is UNLANED and resolves to nil, so callers fail closed rather than
//     validating 2.2 bytes against a 2.0 IG (which would pass or fail for reasons
//     unrelated to the payload). Absence of a lane is a configuration fact, never
//     a licence to guess.
func (g *Gateway) validatorForLine(line string) shnsdk.Validator {
	if line == "" {
		return g.cfg.Validator
	}
	if v := g.cfg.ValidatorsByLine[line]; v != nil && !g.cfg.CanonicalFallbackLines[line] {
		return v
	}
	if v := g.qualifiedDefault(line); v != nil {
		return v
	}
	if len(g.cfg.ValidatorsByLine) > 0 {
		return g.cfg.ValidatorsByLine[line]
	}
	if len(g.cfg.DefaultValidatorsByLine) > 0 {
		return nil
	}
	return g.cfg.Validator
}

// validateFHIR applies the participant's deep-check policy to a resource on
// its selected line. Strict requires structured profile and terminology evidence;
// an unavailable lane returns 503. Optional checks do not run synchronously at
// none, observe or basic. Explicit transformation proof uses a separate path.
func (g *Gateway) validateFHIR(ctx context.Context, resourceJSON []byte, dir, line string) (int, string) {
	return g.validateFHIRAtProfile(ctx, resourceJSON, dir, line, "")
}

// validateFHIRAtProfile preserves the selected lane and all validation refusals.
func (g *Gateway) validateFHIRAtProfile(ctx context.Context, resourceJSON []byte, dir, line, profile string) (int, string) {
	return g.validateGoverned(ctx, findingContextFrom(ctx), g.validatorForLine(line), resourceJSON, dir, line, profile, false).refusal()
}

func (g *Gateway) validateFHIRForContract(ctx context.Context, resourceJSON []byte, dir, contract, line, profile string) (int, string) {
	return g.validateGoverned(ctx, findingContextFrom(ctx), g.validatorForContractLine(contract, line), resourceJSON, dir, line, profile, false).refusal()
}

// validateFHIREgressOrBridged is the target-line egress check that follows
// egressAdapt. bridged says whether the payload actually went through a
// transform chain (len(route.Chain) > 0 at the call site): if it did, these
// bytes are SHN's own registered edit and the check refuses at every level;
// if it did not, they are the participant's own and the level governs.
//
// This is the ONE exception to "the validateFHIR* call lines keep their
// signatures": the eleven PAS-bundle sites each pass the flag, and
// conformance_sources_test.go lists exactly those eleven. The adaptive-DTR
// site is deliberately NOT among them: its leg is an envelope leg, so
// egressAdapt transforms nothing there, and a check that cannot see an SHN
// edit has nothing to verify.
func (g *Gateway) validateFHIREgressOrBridged(ctx context.Context, resourceJSON []byte, contract, targetLine string, bridged bool) (int, string) {
	if bridged {
		return g.certifyBridgedEgressTarget(ctx, resourceJSON, contract, targetLine, findingContextFrom(ctx))
	}
	return g.validateGoverned(ctx, findingContextFrom(ctx), g.validatorForContractLine(contract, targetLine),
		resourceJSON, "egress", targetLine, "", false).refusal()
}

// certifyBridgedEgressTarget verifies this gateway's actual PAS submit/update
// edit against the target-line request Bundle profile before a caller seals it.
// Unchanged native carriage remains governed by the optional policy above.
func (g *Gateway) certifyBridgedEgressTarget(ctx context.Context, resourceJSON []byte, contract, targetLine string, fc findingContext) (int, string) {
	if ctx.Err() != nil || contract != "pa.pas" || (fc.LegType != "pas-claim" && fc.LegType != "pas-claim-update") {
		return http.StatusServiceUnavailable, "adaptation_unavailable"
	}
	profile, ok := profileFor("PASRequestBundle", targetLine, fc.LegType)
	if !ok {
		return http.StatusServiceUnavailable, "adaptation_unavailable"
	}
	v := g.adaptationValidator(contract, targetLine)
	if v == nil {
		return http.StatusServiceUnavailable, "adaptation_unavailable"
	}
	// A checker cannot mutate the bytes that the caller will seal and send.
	ev, checkErr := delegateValidatorEvidence(ctx, v, bytes.Clone(resourceJSON), profile)
	if ctx.Err() != nil || checkErr != nil || !ev.ExecutionAttempted {
		return http.StatusServiceUnavailable, "adaptation_unavailable"
	}
	result := validationResult(ev.Profile, nil)
	if result.State == CheckValid {
		return 0, ""
	}
	g.emitFinding(ConformanceFinding{
		Kind: string(KindFHIRBridged), Direction: "egress", LegType: fc.LegType,
		CorrelationID: fc.CorrelationID, Seam: fc.Seam, Whose: "network",
		Line: targetLine, Profile: profile, Level: g.policy().Level().String(),
		Rule: "fhir.profile", Action: "refused", Decision: "refused", State: result.State,
		CheckIssues: result.Issues, PayloadSHA256: sha256hex(resourceJSON),
	})
	if result.State == CheckInvalid {
		return http.StatusBadGateway, "adaptation_failed"
	}
	return http.StatusServiceUnavailable, "adaptation_unavailable"
}

// govResult is what one governed check produced: the refusal (zero Status when
// the message proceeds) and, for a refusal only, the bounded validator issues
// the body may echo. Issues is empty at every other outcome.
type govResult struct {
	Status int
	Msg    string
	Issues []string
}

// refusal flattens a govResult for the 42 wrapper call lines that write
// {"error": msg}: the bounded issues are appended to the message, so a refused
// sender learns WHAT was wrong without any call line changing.
func (r govResult) refusal() (int, string) {
	if r.Status == 0 || len(r.Issues) == 0 {
		return r.Status, r.Msg
	}
	return r.Status, r.Msg + ": " + strings.Join(r.Issues, "; ")
}

// policy is the gateway's conformance policy, built from the configured level.
// It is configuration, never request-scoped.
func (g *Gateway) policy() ConformancePolicy {
	return NewConformancePolicy(g.cfg.ConformanceEnforcement)
}

// ConformanceLevelForTest exposes this Gateway's own effective conformance
// enforcement level — test-only introspection (the ValidatorReadinessForTest/
// RecordEdgeCaptureForTest pattern) proving that a level set through
// Config.ConformanceEnforcement actually reached the constructed Gateway,
// not just whatever struct a caller assembled.
func (g *Gateway) ConformanceLevelForTest() ConformanceEnforcement {
	return g.policy().Level()
}

// ResponderForTest exposes this Gateway's own configured Responder (a payer's
// content occupant) — test-only introspection letting a caller outside this
// package reach the SAME responder value Handle dispatches to, e.g. to type-
// assert for a *nativeResponder's own ConformanceLevelForTest and prove its
// enforcement policy is the one the deployment actually configured, not just
// whatever engine.Config a caller assembled.
func (g *Gateway) ResponderForTest() LegResponder {
	return g.cfg.Responder
}

// kindForDirection names the governed check kind. A bridged payload — one this
// gateway transformed between IG lines — is SHN's own registered edit, never
// the participant's data, and is refused at every level (§2).
func kindForDirection(dir string, bridged bool) CheckKind {
	switch {
	case bridged:
		return KindFHIRBridged
	case dir == "egress":
		return KindFHIREgress
	default:
		return KindFHIRIngress
	}
}

// validateGoverned governs legacy resource checks while full message checks use
// the rule registry. Optional classes never execute synchronously at none,
// observe or basic. Mandatory transformation certification is independent.
func (g *Gateway) validateGoverned(ctx context.Context, fc findingContext, v shnsdk.Validator, resourceJSON []byte, dir, line, profile string, bridged bool) govResult {
	if !bridged {
		switch g.policy().Action(CheckDeep) {
		case CheckOff:
			return govResult{}
		case CheckObserve:
			g.observeAuthoredTarget(authoredValidationTarget{validator: v, finding: fc, direction: dir, line: line, profile: profile}, resourceJSON)
			return govResult{}
		case CheckEnforce:
			return g.validateGovernedEvidence(ctx, fc, v, resourceJSON, dir, line, profile)
		}
		return govResult{}
	}
	if v == nil {
		return govResult{Status: http.StatusInternalServerError, Msg: "no FHIR validator lane configured for contract line " + line + " (FR-36/FR-G29)"}
	}
	res, err := v.Validate(ctx, resourceJSON, profile)
	if err != nil {
		return govResult{Status: http.StatusInternalServerError, Msg: "validator unavailable"}
	}
	if res.Valid {
		return govResult{}
	}
	kind := kindForDirection(dir, bridged)
	decision := g.policy().Decide(kind, "", VerdictInvalid)
	whose := fc.Whose
	if bridged {
		whose = "network"
	}
	g.emitFinding(ConformanceFinding{
		Kind:          string(kind),
		Direction:     dir,
		LegType:       fc.LegType,
		CorrelationID: fc.CorrelationID,
		Seam:          fc.Seam,
		Whose:         whose,
		Line:          line,
		Profile:       profile,
		Level:         g.policy().Level().String(),
		Decision:      decision.String(),
		State:         CheckInvalid,
		PayloadSHA256: sha256hex(resourceJSON),
		Issues:        res.Issues,
	})
	if decision == Record {
		return govResult{}
	}
	return govResult{Status: http.StatusUnprocessableEntity, Msg: dir + " validation failed", Issues: boundIssues(res.Issues)}
}

// validateFHIRPayerIngress applies the local policy independently of the
// counterparty's implementation or the workflow's origination profile.
func (g *Gateway) validateFHIRPayerIngress(ctx context.Context, resourceJSON []byte, line, contract string) (int, string) {
	return g.validateFHIRForContract(ctx, resourceJSON, "ingress", contract, line, "")
}

// validatePASApplicationReply checks the payer's bytes on the payer's own
// authenticated response declaration. The request's selected build line is
// never evidence for the response: the reference payer accepts PAS 2.2 input
// while independently producing PAS 2.0 output. An absent or unsupported
// declaration has no qualified profile lane; strict reports unavailable and
// observe records that uncertainty without borrowing a default checker.
func (g *Gateway) validatePASApplicationReply(ctx context.Context, resourceJSON []byte, reply ApplicationReply) (int, string) {
	line := ""
	if contract, declaredLine, ok := strings.Cut(reply.DeclaredVersion, "@"); ok && contract == "pa.pas" {
		if _, supported := shnsdk.PASLineDef(declaredLine); supported {
			line = declaredLine
		}
	}
	var validator shnsdk.Validator
	if line != "" {
		validator = g.validatorForContractLine("pa.pas", line)
	}
	return g.validateGoverned(ctx, findingContextFrom(ctx), validator, resourceJSON, "ingress", line, "", false).refusal()
}

// envelopeEgressLegs is the DTR-fetch-ONLY non-FHIR carve-out (the
// multi-version spec's recorded DTR-fetch known-gap obligation, discharged).
// Membership means egressAdapt walks
// route.Chain for the routing/observer story but never hands the bytes to a
// step function — safe only because dtr-questionnaire-fetch's payload is
// the operation's own input, carried to the payer's line unchanged: the
// ingress and the originator refuse a walk that would change a byte of it
// (handleDTRIngress, carryUnchanged), so no pa.dtr compat-manifest step
// ever rewrites it.
//
// CRD legs (crd-order-select, crd-order-dispatch) must NEVER join this set:
// their arm-3 byte-identity rests on the identity chain genuinely RUNNING
// (TestD7CRDArm3IdentityChainIsBytePreserving) — a CDS Hooks payload IS FHIR
// content the pa.crd manifest models, so carving it out here would misname
// it as an envelope, would be redundant (applyChain's nil-func identity path
// is already a byte-pure pass-through with identical reports, transform.go's
// applyChain), and would silently bypass any FUTURE real CRD module instead
// of making it run or refuse honestly. TestCRDLegsNeverJoinEnvelopeCarveOut
// pins the set's exact membership.
//
// Terminology note: the commit that discharged it calls this "proven
// safe by byte-identity guard" — the envelope carve-out has no runtime
// equality check because its output is the original payload. Its guard is
// TestEnvelopeLegChainIsByteIdenticalPassThrough. The separate CRD identity
// chain below runs step functions and checks their output against the source.
var envelopeEgressLegs = map[string]bool{"dtr-questionnaire-fetch": true}

// verbatimChainLegs are legs whose payload the compat chain WALKS — so the
// routing and observer story stays honest — and never rewrites.
//
// pas-claim-inquire is one, for two reasons that both have to hold.
//
// FIRST, the chain has nothing to do to it. Every pa.pas step models a
// difference between the SUBMIT/AMENDMENT profiles (profile-claim,
// profile-claim-update) or the response profile: the Claim.item line detail and
// the related-claim relationship, and ClaimResponse.request. An inquiry's Claim
// is profile-claim-inquiry, a third profile that states none of them — the same
// distinction the certification layer draws (species.go) and the validator lane
// scopes (linefake.go). Measured: running the chain over an inquiry changed not
// one fact, only the key order of the re-marshalled JSON.
//
// SECOND, that re-marshal is exactly what an inquiry must not suffer. Its
// payload CARRIES the participant's own Patient, Coverage, provider and payer
// records as verbatim spans, sealed as embeds — that is what makes the bytes on
// the wire provably the participant's records rather than a rendering of them.
// A step that re-serialized them would reorder their keys and leave the seal
// describing a message that no longer exists.
//
// inquireContinuation keeps the runtime byte-equality check that refuses to send
// under a broken seal, so a future inquiry-aware step that DID change the bytes
// is refused rather than waved through.
var verbatimChainLegs = map[string]bool{"pas-claim-inquire": true}

// egressAdapt applies route's transform chain (if any) to payload before it
// is sent, builds the transform Provenance from the chain's LossReports,
// and emits leg.transformed. Arms (1)/(2) carry route.Chain == nil and pass
// through unchanged. A real PAS chain first certifies the exact source bytes
// against the built line; FHIR callers certify transformed output at the
// target lane before sealing. CRD requests are CDS Hooks envelopes, so their
// registered identity chain is checked byte-for-byte here instead.
func (g *Gateway) egressAdapt(ctx context.Context, route legRoute, payload []byte, x ExchangeIdentity) ([]byte, []LossReport, error) {
	if len(route.Chain) == 0 {
		return payload, nil, nil
	}
	contract := route.Chain[0].Contract

	var out []byte
	var reports []LossReport
	var err error
	if envelopeEgressLegs[x.LegType] || verbatimChainLegs[x.LegType] {
		// Non-FHIR carve-out (obligation discharged — see
		// envelopeEgressLegs's own doc comment and the multi-version spec's
		// recorded DTR-fetch known-gap entry): envelope legs are
		// safe BY CONSTRUCTION — the chain is walked (envelopeChainReports
		// mirrors applyChain's own walk-direction switch, below) so the
		// routing/observer story stays honest, but the step funcs never see
		// the bytes, so they can never re-marshal (and therefore never
		// reorder) the envelope's JSON. No runtime byte-equality guard
		// follows this assignment — it would be structurally unreachable
		// (out IS payload, never a copy that could diverge), the exact
		// anti-pattern obligation 2 exists to eliminate.
		// TestEnvelopeLegChainIsByteIdenticalPassThrough is the enforcement
		// fence in place of the impossible envelope $validate.
		out, reports = payload, envelopeChainReports(route.Chain, route.BuildLine)
	} else {
		if contract != "pa.crd" { // CRD's registered steps are guarded byte-identities below.
			err = g.certifyEgressSource(ctx, route, payload, x)
		}
		if err == nil {
			input := payload
			if contract == "pa.crd" {
				// Every registered CRD step is identity. Run it on an owned copy:
				// a faulty step may edit its input slice before returning it.
				input = bytes.Clone(payload)
			}
			out, reports, err = applyChain(route.Chain, route.BuildLine, input, x)
			if err == nil && contract == "pa.crd" && !bytes.Equal(out, payload) {
				err = contextError(http.StatusBadGateway, "adaptation_failed")
			}
		}
	}
	if err != nil {
		// Observer honesty: a transform-chain refusal used to be observer-SILENT (every
		// egressAdapt call site precedes roundTrip, at the time the only
		// other leg.failed producer; guardPendCarry now emits on this same
		// seam too — observer.go's kinds table names all three) — a §6-grade
		// honesty gap the observer stream must not have. Route carries the
		// attempted chain (routeInfoFor) so the refusal is legible on the
		// SAME seam leg.transformed/leg.originated use, not just a bare
		// error string.
		g.observe(ObserverEvent{
			Kind: "leg.failed", Direction: "originate",
			LegType: x.LegType, CorrelationID: x.CorrelationID,
			Counterpart: x.Counterpart, Detail: err.Error(),
			Route: routeInfoFor(route),
		})
		return nil, nil, err
	}
	targetLine := shnsdk.LineOf(route.Token)

	ev := transformedObserverEvent(reports)
	ev.Direction = "originate"
	ev.CorrelationID = x.CorrelationID
	ev.LegType = x.LegType
	ev.Counterpart = x.Counterpart
	if spec, ok := paCatalog[x.LegType]; ok { // same lookup OriginateLeg uses
		ev.AuthorityFrame = spec.ReqFrame
	}

	// The transform Provenance (module id, source->target lines,
	// the shn-loss-report extension) rides INSIDE the transformed payload
	// where the target profile tolerates the extra Bundle.entry; else it
	// rides the observer stream only — never the envelope, never Hub-visible.
	sdkLoss := toSDKLossReports(reports)
	provJSON, perr := shnsdk.BuildTransformProvenance(
		"urn:shn:leg:"+x.CorrelationID, "Organization/"+g.cfg.HolderID,
		ev.Detail, route.BuildLine, targetLine, sdkLoss, g.cfg.Clock(),
	)
	if perr != nil {
		return nil, nil, fmt.Errorf("engine: egressAdapt: build transform Provenance: %w", perr)
	}
	// Loss-RECORD completeness guard (the never-silently-drop invariant
	// applied to the Provenance itself, not just the payload): the
	// Provenance just built must round-trip back to the SAME reports the
	// chain produced. This can never fire from our own freshly-built bytes
	// (TestBuildTransformProvenanceLossRoundTrip already pins the sdk
	// builder's own honesty) — it exists as the one place that can still
	// refuse before anything seals, so a future refactor that breaks the
	// round trip fails LOUDLY here rather than shipping a Provenance whose
	// loss record silently doesn't match what happened.
	if verr := verifyLossRoundTrip(provJSON, sdkLoss); verr != nil {
		return nil, nil, fmt.Errorf("engine: egressAdapt: %w", verr)
	}

	if provenanceTolerated(contract) {
		// No contract's target profile has live-validate evidence for an
		// added Provenance Bundle.entry yet — provenanceTolerated is false
		// for every contract this build ships (the sanctioned safe default
		// when evidence is absent). This branch is therefore unreached today; it stays
		// explicit rather than speculatively implemented (transform-iff) —
		// a future contract's Bundle.entry-append helper lands here once a
		// specific placement is proven, live, against its target lane.
	} else {
		ev.Payload = provJSON
	}
	g.observe(ev)

	// Local demonstration/inspection only (SHN_DEMO_EDGE_CAPTURE /
	// Config.DemoEdgeCapture, default off): record this leg's own pre-seal
	// before/after payload pair. Covers both branches above (applyChain and
	// the envelope carve-out) since they share this single return — never
	// the refusal path above, which returns before reaching here (nothing
	// was sent). See edgecapture.go's doc comments for the store's bounds,
	// the deep-copy-on-Record aliasing guard (out may be the SAME slice as
	// payload on the carve-out path), and the single-writer-per-id
	// assumption.
	if g.cfg.DemoEdgeCapture {
		g.edgeCaptureStoreForWrite().Record(EdgeCapture{
			CorrelationID: x.CorrelationID,
			LegType:       x.LegType,
			Contract:      contract,
			From:          route.BuildLine,
			To:            targetLine,
			Chain:         chainStepsFrom(route.BuildLine, route.Chain),
			LossReports:   reports,
			Before:        payload,
			After:         out,
			CapturedAt:    g.cfg.Clock(),
		})
	}

	return out, reports, nil
}

// certifyEgressSource proves the exact pre-transform PAS request at the line
// the builder used. PCV-15 requires this for a real transformation at every
// optional conformance level. Unsupported operation/profile pairs fail closed;
// envelope and registered identity chains are handled separately above.
func (g *Gateway) certifyEgressSource(ctx context.Context, route legRoute, payload []byte, x ExchangeIdentity) error {
	if ctx.Err() != nil {
		return contextError(http.StatusServiceUnavailable, "adaptation_unavailable")
	}
	if route.Chain[0].Contract != "pa.pas" || (x.LegType != "pas-claim" && x.LegType != "pas-claim-update") {
		return contextError(http.StatusServiceUnavailable, "adaptation_unavailable")
	}
	profile, ok := profileFor("PASRequestBundle", route.BuildLine, x.LegType)
	if !ok {
		return contextError(http.StatusServiceUnavailable, "adaptation_unavailable")
	}
	v := g.adaptationValidator("pa.pas", route.BuildLine)
	if v == nil {
		return contextError(http.StatusServiceUnavailable, "adaptation_unavailable")
	}
	// The checker may retain or mutate its argument. It never owns the bytes
	// applyChain will consume or the source holder supplied.
	ev, checkErr := delegateValidatorEvidence(ctx, v, bytes.Clone(payload), profile)
	if ctx.Err() != nil || checkErr != nil || !ev.ExecutionAttempted {
		return contextError(http.StatusServiceUnavailable, "adaptation_unavailable")
	}
	// FR-G54 proves this request's species profile at its source line.
	// Terminology coverage is a separate optional deep/strict check: the
	// production OperationValidator reports it unavailable even after a
	// successful $validate, so it cannot gate the transformation here.
	switch validationResult(ev.Profile, nil).State {
	case CheckValid:
		return nil
	case CheckInvalid:
		return contextError(http.StatusBadGateway, "adaptation_failed")
	default:
		return contextError(http.StatusServiceUnavailable, "adaptation_unavailable")
	}
}

// adaptationRefusalStatus preserves the source-certification distinction
// between unavailable proof and a failed edit at the existing caller seams.
func adaptationRefusalStatus(err error) int {
	var classified *ingressContextError
	if errors.As(err, &classified) {
		return classified.status
	}
	return http.StatusBadGateway
}

// edgeCaptureStoreForWrite returns g.edgeCapture, building it on first use.
// Safe under concurrent callers: g.edgeCapture is an atomic.Pointer, and a
// CompareAndSwap loss (another goroutine won the race to build it first)
// falls back to the winner's store rather than the caller's own discarded
// one — there is never more than one live store per Gateway.
func (g *Gateway) edgeCaptureStoreForWrite() *edgeCaptureStore {
	if s := g.edgeCapture.Load(); s != nil {
		return s
	}
	fresh := newEdgeCaptureStore(edgeCaptureCap)
	if g.edgeCapture.CompareAndSwap(nil, fresh) {
		return fresh
	}
	return g.edgeCapture.Load()
}

// edgeCaptureLookup reads back one leg's captured pre-seal before/after
// payload pair by correlation id. Reports a miss when the flag was never on
// for this Gateway (no store was ever built) or when no leg has recorded
// under that id. Safe to call concurrently with in-flight captures (the
// store pointer is read atomically). The one lookup implementation behind
// EdgeCaptureFor; engine package tests call it directly (unexported, same
// package) rather than through a separate exported test seam.
func (g *Gateway) edgeCaptureLookup(id string) (EdgeCapture, bool) {
	s := g.edgeCapture.Load()
	if s == nil {
		return EdgeCapture{}, false
	}
	return s.Get(id)
}

// EdgeCaptureFor is the production read seam over the bounded edge-capture
// store (SHN_DEMO_EDGE_CAPTURE / Config.DemoEdgeCapture): the gateway's own
// loopback demo capture-fetch endpoint
// (gateway/app/demo_endpoint.go's handleDemoCapture) reads through this.
func (g *Gateway) EdgeCaptureFor(id string) (EdgeCapture, bool) {
	return g.edgeCaptureLookup(id)
}

// RecordEdgeCaptureForTest seeds the bounded edge-capture store directly
// with e, bypassing egressAdapt's own capture hook — a test-only seam for
// exercising a reader (e.g. the demo capture-fetch endpoint) against a
// known entry without driving a full leg through the engine. Builds the
// store on first use, exactly like the real egressAdapt capture hook.
func (g *Gateway) RecordEdgeCaptureForTest(e EdgeCapture) {
	g.edgeCaptureStoreForWrite().Record(e)
}

// provenanceTolerated reports whether contract's target profile has
// live-validate evidence that it tolerates an added Provenance Bundle.entry.
// No contract has that evidence yet — the live derivations behind the wired
// transform steps predate the Provenance builder, so egressAdapt
// observer-streams every transform's Provenance today (the sanctioned safe
// default when evidence is absent).
// Flipping a contract to true is additive future work once a specific
// Bundle.entry placement is proven against its target lane, live.
func provenanceTolerated(contract string) bool {
	switch contract {
	case "pa.crd", "pa.dtr", "pa.pas":
		return false
	default:
		return false
	}
}

// toSDKLossReports element-wise converts the engine's own []LossReport
// (engine-internal, no sdk twin) into []shnsdk.LossReport (the canonical wire
// schema) — a type conversion, not a re-derivation, so the two
// encodings can never drift (TestLossReportSDKSchemaParity pins the schema
// match; this is the one place the conversion actually happens, the seam
// sdk/provenance.go's own layering note describes). Unexported: the demo
// capture-fetch endpoint (gateway/app/demo_endpoint.go's handleDemoCapture)
// does NOT use this — engine.LossReport already carries the same json tags
// as shnsdk.LossReport (TestLossReportSDKSchemaParity again), so that
// endpoint marshals the engine type directly, exactly like its
// /demo/transform sibling — there is no outside caller for this conversion.
func toSDKLossReports(reports []LossReport) []shnsdk.LossReport {
	if reports == nil {
		return nil
	}
	out := make([]shnsdk.LossReport, len(reports))
	for i, r := range reports {
		out[i] = shnsdk.LossReport{
			Module: r.Module, Source: r.Source, Target: r.Target,
			Carried:     toSDKLossEntries(r.Carried),
			Synthesized: toSDKLossEntries(r.Synthesized),
		}
	}
	return out
}

func toSDKLossEntries(entries []LossEntry) []shnsdk.LossEntry {
	if entries == nil {
		return nil
	}
	out := make([]shnsdk.LossEntry, len(entries))
	for i, e := range entries {
		out[i] = shnsdk.LossEntry{Path: e.Path, Detail: e.Detail}
	}
	return out
}

// verifyLossRoundTrip is the loss-RECORD completeness guard (the
// never-silently-drop invariant applied to the Provenance itself, not just
// the payload): the shn-loss-report extension INSIDE provenanceJSON must
// restore back to exactly `want`. A downcast that genuinely lost content
// (want has Carried/Synthesized entries) whose Provenance was stripped of
// that record — by a bug, or by a mutation applied after building it — must
// never be treated as accounted for.
func verifyLossRoundTrip(provenanceJSON []byte, want []shnsdk.LossReport) error {
	var prov struct {
		Extension []json.RawMessage `json:"extension"`
	}
	if err := json.Unmarshal(provenanceJSON, &prov); err != nil {
		return fmt.Errorf("verifyLossRoundTrip: unmarshal Provenance: %w", err)
	}
	var restored []shnsdk.LossReport
	found := false
	for _, ext := range prov.Extension {
		loss, err := shnsdk.RestoreTransformLoss(ext)
		if err != nil {
			continue // not the loss-report extension (or malformed — keep looking)
		}
		restored = loss
		found = true
		break
	}
	if !found {
		return fmt.Errorf("verifyLossRoundTrip: no shn-loss-report extension found on Provenance")
	}
	if want == nil {
		want = []shnsdk.LossReport{}
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		return fmt.Errorf("verifyLossRoundTrip: marshal want: %w", err)
	}
	gotJSON, err := json.Marshal(restored)
	if err != nil {
		return fmt.Errorf("verifyLossRoundTrip: marshal restored: %w", err)
	}
	if string(wantJSON) != string(gotJSON) {
		return fmt.Errorf("verifyLossRoundTrip: Provenance loss record does not match chain output (want %s, got %s)", wantJSON, gotJSON)
	}
	return nil
}

// VerifyTransformLossRoundTripForTest is a thin exported wrapper around
// egressAdapt's Provenance loss-record integrity guard for package
// adversarial, which cannot see unexported engine symbols — the same
// cross-module-boundary rationale as TransformPASForTest/TransformDTRForTest.
func VerifyTransformLossRoundTripForTest(provenanceJSON []byte, want []shnsdk.LossReport) error {
	return verifyLossRoundTrip(provenanceJSON, want)
}

// verifyCarryPresent (the "carry-stripped-detected-on-upcast"
// adversarial row) confirms every declared Carried LossEntry has a matching
// shn-carried-content wrapper genuinely present somewhere in payload's
// top-level resource(s) extension array(s) — checked BEFORE a restore step
// (pasRestoreCarriedExtensions) would otherwise silently no-op over an
// ALREADY-ABSENT wrapper (that function's own doc comment: "a no-op when the
// array is empty or carries no shn-carried-content entries" — by design, it
// cannot itself distinguish "nothing was ever carried" from "something was
// carried and then stripped"). A downcast leg's own loss record (Provenance's
// LossReport, or the engine's pre-Provenance []LossReport) declaring a carry
// the payload no longer bears — the wrapper stripped from the payload
// independently of the loss record — is a typed error here.
//
// HONEST SCOPE NOTE: this can only ever detect a PAYLOAD-ONLY strip —
// `declared` (typically read straight from a LossReport.Carried list) must
// still name the carry. Stripping BOTH the payload's wrapper AND the
// declaring LossReport/Provenance together is undetectable BY CONSTRUCTION:
// once neither side declares the loss there is no third witness left to
// compare against — the mirror-image limitation of
// TestAdversarial_TransformProvenanceLossReportStripped (which detects the
// Provenance-only strip, the opposite half of the same pair).
//
// Scoped to the pa.pas top-level-extension carry shape (Claim/ClaimResponse.
// extension) — pasCollectResources walks a bare resource or a Bundle's
// entries the same way pasRestoreCarriedExtensions itself does, so this
// checks exactly the surface that function would restore from. The pa.dtr
// carry shape (QuestionnaireResponse.item.answer.value.extension:itemWeight,
// dtrStep2122Down's own doc comment) nests one level deeper (inside
// item.answer, not a resource's top-level extension array) and would need
// its own walker; not added — no adversarial row names it, and the
// cross-line pair suite already drives pa.dtr's carry mechanism directly
// via TransformDTRForTest's Restore(Carry(x))==x round trip.
func verifyCarryPresent(declared []shnsdk.LossEntry, payload []byte) error {
	var top map[string]any
	if err := json.Unmarshal(payload, &top); err != nil {
		return fmt.Errorf("engine: verifyCarryPresent: unmarshal payload: %w", err)
	}
	present := map[string]bool{}
	for _, resources := range pasCollectResources(top) {
		for _, res := range resources {
			extAny, _ := res["extension"].([]any)
			for _, e := range extAny {
				em, ok := e.(map[string]any)
				if !ok || em["url"] != shnsdk.CarriedContentExtURL {
					continue
				}
				raw, err := json.Marshal(em)
				if err != nil {
					continue
				}
				path, _, _, err := shnsdk.RestoreCarried(raw)
				if err != nil {
					continue // malformed wrapper — not a match either way
				}
				present[path] = true
			}
		}
	}
	for _, e := range declared {
		if !present[e.Path] {
			return fmt.Errorf("engine: verifyCarryPresent: declared carry %q not found in the payload about to be restored — the payload no longer bears content its own loss record declares carried", e.Path)
		}
	}
	return nil
}

// VerifyCarryRestoredForTest is a thin exported wrapper around
// verifyCarryPresent for package adversarial, which cannot see unexported
// engine symbols — the same cross-module-boundary rationale as
// VerifyTransformLossRoundTripForTest/TransformPASForTest.
func VerifyCarryRestoredForTest(declared []shnsdk.LossEntry, payload []byte) error {
	return verifyCarryPresent(declared, payload)
}

// carriedEntriesFrom flattens every Carried LossEntry a transform chain's
// reports declared, in chain order, into the sdk wire type — through
// toSDKLossEntries, the ONE conversion seam (toSDKLossReports's own doc
// comment), so the pend record and the Provenance loss record can never
// disagree about what "carried" means. nil when the chain carried nothing,
// which is EVERY flow this build originates today (produce-iff: no SHN
// builder emits a 2.2-only top-level Claim extension — transform_pas.go's
// pas22OnlyClaimExtensions note), so pendState.carriedEntries stays empty and
// verifyPendCarryIntact stays a no-op on every existing path.
func carriedEntriesFrom(reports []LossReport) []shnsdk.LossEntry {
	var out []shnsdk.LossEntry
	for _, r := range reports {
		out = append(out, toSDKLossEntries(r.Carried)...)
	}
	return out
}

// chainRestoresCarry reports whether chain, walked from buildLine, will run at
// least one RESTORING step: a StepCarry row taken in the Up direction, whose
// Up half is the inverse of the Down half that created the
// shn-carried-content wrappers (pasStep2122Up / pasStep2122Down). The walk
// mirrors applyChain's own curLine-vs-step.From/To direction switch
// (transform.go) exactly, as envelopeChainReports and routeInfoFor do.
//
// This is verifyPendCarryIntact's gate, and it has to be direction-aware: a
// chain walked DOWN creates wrappers rather than restoring them, so a freshly
// built payload entering it legitimately bears none yet. Gating on "the pend
// declared a carry" alone would refuse that perfectly honest flow.
//
// ASYMMETRY WITH GATE 1, deliberate and inert today — the re-adjudication
// trigger if it stops being inert: this gate keys on Class == StepCarry, while
// gate 1 (len(declared) > 0) accepts Carried entries from a report of ANY
// class. They agree because only the two 2.1<->2.2 StepCarry Down halves emit
// Carried at all (the 2.0<->2.1 row is StepGated and carries nothing). A
// FUTURE manifest row that is not StepCarry but whose Down half nonetheless
// carries would satisfy gate 1 and NOT gate 2 — the guard would under-fire,
// silently. Anyone adding such a row must widen this predicate (key on "the
// Up half restores", not on the row's declared class) rather than assume the
// two gates still describe the same set.
func chainRestoresCarry(chain []CompatStep, buildLine string) bool {
	curLine := buildLine
	for _, s := range chain {
		if curLine == s.To { // walking Down — this step CREATES wrappers, never restores
			curLine = s.From
			continue
		}
		// curLine == s.From — walking Up (chainFor never yields a disconnected
		// row; applyChain's default case treats one as a caller bug).
		if s.Class == StepCarry && s.Up != nil {
			return true
		}
		curLine = s.To
	}
	return false
}

// verifyPendCarryIntact is verifyCarryPresent's PRODUCTION enforcement point
// (the multi-version spec's "verifyCarryPresent has no production
// caller" obligation, whose own text requires "a real production enforcement
// point on the restore path before arm 3 goes live"). It runs on the pinned
// resume leg, BEFORE egressAdapt hands the payload to a chain that would
// restore: declared is the pend's own record of what its down-leg carried
// (pendState.carriedEntries), so a payload that no longer bears a wrapper the
// record names is refused here rather than silently no-opping through
// pasRestoreCarriedExtensions.
//
// Two gates, both required, both cheap:
//   - declared empty ⇒ nothing was ever carried; nothing to verify. True of
//     every SHN-originated flow today, so this costs one len() on the live path.
//   - the chain restores nothing (chainRestoresCarry) ⇒ no restore can no-op,
//     and the payload is not expected to bear wrappers yet.
//
// SCOPE (verifyCarryPresent's own doc comment carries the full note, restated
// here because this is the wired site): the detector walks pa.pas TOP-LEVEL
// resource extensions only, BY DOCUMENTED DESIGN — the same surface
// pasRestoreCarriedExtensions restores from. pa.dtr's itemWeight carry nests
// inside item.answer and is NOT covered here; it is proven instead by the
// cross-line pair suite's Restore(Carry(x))==x round trip. Nobody should read
// this guard as itemWeight coverage.
func verifyPendCarryIntact(declared []shnsdk.LossEntry, route legRoute, payload []byte) error {
	if len(declared) == 0 || !chainRestoresCarry(route.Chain, route.BuildLine) {
		return nil
	}
	if err := verifyCarryPresent(declared, payload); err != nil {
		return fmt.Errorf("engine: pended carry not intact at resume (pin %s): %w", route.Token, err)
	}
	return nil
}

// VerifyPendCarryIntactForTest is a thin exported wrapper around the WIRED
// guard — gate included, driving the REAL compat-manifest chain for
// (contract, buildLine, targetLine) — for package adversarial, which cannot
// see unexported engine symbols. Same cross-module-boundary rationale as
// VerifyCarryRestoredForTest, which exposes the bare detector; this one
// exposes the enforcement point as production calls it.
func VerifyPendCarryIntactForTest(declared []shnsdk.LossEntry, contract, buildLine, targetLine string, payload []byte) error {
	return verifyPendCarryIntact(declared, legRoute{
		Token:     contract + "@" + targetLine,
		BuildLine: buildLine,
		Chain:     chainFor(contract, buildLine, targetLine),
	}, payload)
}

// unframeRequest validates the closed request-frame metadata and this endpoint's
// consumer boundary. The built-in native adapter admits its configured backend's
// representation at dispatch; other consumers retain builder-bound admission.
// Validator availability is an operation-specific
// conformance concern, never a transport capability or declaration source.
func (g *Gateway) unframeRequest(legType string, payload []byte) ([]byte, string, int, string) {
	contract, err := legContract(legType)
	if err != nil {
		return nil, "", http.StatusInternalServerError, err.Error()
	}
	if !shnsdk.IsFramed(payload) {
		return payload, "", 0, "" // bare: the caller recomputes (unframeRequestFrom)
	}
	hdr, body, ferr := shnsdk.DecodeHTTPFrame(payload)
	if ferr != nil {
		return nil, "", http.StatusBadRequest, "request frame decode failed"
	}
	claimed := hdr.Headers[shnsdk.FrameHeaderContractVersion]
	if claimed == "" {
		// A framed request with no version claim is the frames-without-versions
		// case: treat it exactly like a bare request (absence is tolerated).
		return body, "", 0, ""
	}
	if contract == "" {
		return nil, "", http.StatusUnprocessableEntity,
			"request declares contract version " + claimed + " on version-neutral leg " + legType
	}
	if !validNativeContractToken(claimed, contract) || (!nativeContractToken(claimed) && !g.nativeFrameConsumer(legType)) {
		return nil, "", http.StatusUnprocessableEntity,
			"request declares contract version " + claimed + ", which this gateway cannot build for leg " + legType +
				" (it speaks " + strings.Join(sortedTokens(contract, contractLineSet(shnsdk.NativeContractVersions(), contract)), ",") + ")"
	}
	return body, claimed, 0, ""
}

// unframeRequestFrom is unframeRequest plus the BARE-request fallback: when the
// sender sent no version claim, the answer line is the SYMMETRIC RECOMPUTATION of
// the originator's own selection — the sender's registry-declared set × this
// build's declared set, highest common line, with a silent sender falling back to
// this build's own canonical line (selectContractToken's rules, verbatim). That
// keeps a pre-framing or version-neutral sender answered exactly as before.
//
// A recomputation that REFUSES is not an error here: the originator already made
// the routing decision, and refusing an in-flight leg on the responder side would
// break the D1a mismatch window. It degrades to this build's own canonical line,
// which is the pre-framing answer.
func (g *Gateway) unframeRequestFrom(sender, legType string, payload []byte) ([]byte, string, int, string) {
	body, claimed, status, msg := g.unframeRequest(legType, payload)
	if status != 0 || claimed != "" {
		return body, claimed, status, msg
	}
	contract, err := legContract(legType)
	if err != nil {
		return nil, "", http.StatusInternalServerError, err.Error()
	}
	if contract == "" {
		return body, "", 0, "" // version-neutral leg: no answer line, never stamped
	}
	own := g.declaredContractVersions()
	var peer []string
	if entry, ok := g.cfg.Reg.Lookup(sender); ok {
		peer = entry.ContractVersions
	}
	tok, refused := selectContractToken(own, peer, len(peer) > 0, contract)
	if refused || tok == "" {
		ownLines := contractLineSet(own, contract)
		if len(ownLines) == 0 {
			return body, "", 0, ""
		}
		return body, contract + "@" + highestLine(ownLines), 0, ""
	}
	return body, tok, 0, ""
}

// nativeContractToken reports whether tok is a contract-version token THIS build
// can natively build (the "native" half of the receiver rule).
func nativeContractToken(tok string) bool {
	for _, n := range shnsdk.NativeContractVersions() {
		if n == tok {
			return true
		}
	}
	return false
}

// answerLineKey / declaredSetKey are the request-scoped carriers for the answer
// token (the contract-version token the RESPONDER must build its answer at) and for
// this deployment's DECLARED set. They ride the context rather than
// LegResponder.Handle's signature because LegResponder is a PUBLIC
// partner-implementable seam (gateway/engine is a published module) — a signature
// change would break every partner responder, while a context value is additive: a
// responder that never reads it keeps producing this build's canonical line,
// exactly as before.
type (
	answerLineKey     struct{}
	declaredSetKey    struct{}
	frameOperationKey struct{}
)

// withRequestFrameOperation tags ctx with the DTR operation the request frame
// names ("" is a no-op).
func withRequestFrameOperation(ctx context.Context, operation string) context.Context {
	if operation == "" {
		return ctx
	}
	return context.WithValue(ctx, frameOperationKey{}, operation)
}

// RequestFrameOperation returns the DTR operation the inbound request frame
// names (shnsdk.FrameHeaderOperation) for the request a LegResponder is
// answering: shnsdk.FrameOperationQuestionnairePackage when the request body
// is the $questionnaire-package input Parameters, and
// shnsdk.FrameOperationNextQuestion when it is the SDC $next-question input.
// It returns "" for a request that names no operation, which the leg refuses.
func RequestFrameOperation(ctx context.Context) string {
	op, _ := ctx.Value(frameOperationKey{}).(string)
	return op
}

// inboundFrameOperation reads the operation header of an inbound request
// frame. The header is defined only for dtr-questionnaire-fetch: a frame that
// names an operation on any other leg, or an unsupported DTR operation, is
// refused before optional content checks. A payload that is not a frame names none.
func inboundFrameOperation(legType string, payload []byte) (string, int, string) {
	if _, status, msg := inboundFrameCRDHook(legType, payload); status != 0 {
		return "", status, msg
	}
	if !shnsdk.IsFramed(payload) {
		return "", 0, ""
	}
	hdr, _, err := shnsdk.DecodeHTTPFrame(payload)
	if err != nil {
		return "", http.StatusBadRequest, "request frame decode failed"
	}
	op := hdr.Headers[shnsdk.FrameHeaderOperation]
	if op != "" && legType != "dtr-questionnaire-fetch" {
		return "", http.StatusBadRequest, "operation header is not defined for this transaction type"
	}
	if op != "" && op != shnsdk.FrameOperationQuestionnairePackage && op != shnsdk.FrameOperationNextQuestion {
		return "", http.StatusBadRequest, "unsupported DTR operation"
	}
	return op, 0, ""
}

// withAnswerLine tags ctx with the answer token for this leg ("" is a no-op).
func withAnswerLine(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}
	return context.WithValue(ctx, answerLineKey{}, token)
}

// withDeclaredContractVersions tags ctx with this deployment's declared set, so a
// builder that must fall back (no answer line resolved) falls back to what this
// deployment DECLARES rather than to the library build constant (D1a). Set once,
// beside the answer line, in handleInbound.
func withDeclaredContractVersions(ctx context.Context, declared []string) context.Context {
	if len(declared) == 0 {
		return ctx
	}
	return context.WithValue(ctx, declaredSetKey{}, declared)
}

// declaredContractVersionsFrom reads the ctx-carried declared set; nil when absent
// (a responder invoked outside the inbound dispatch).
func declaredContractVersionsFrom(ctx context.Context) []string {
	declared, _ := ctx.Value(declaredSetKey{}).([]string)
	return declared
}

// answerLineFrom reads the answer LINE ("2.0"/"2.1"/"2.2") off ctx; "" when no
// line was resolved (version-neutral leg, or a caller outside the inbound path),
// which every builder reads as "this build's canonical line".
func answerLineFrom(ctx context.Context) string {
	tok, _ := ctx.Value(answerLineKey{}).(string)
	return shnsdk.LineOf(tok)
}

// answerLineOr is answerLineFrom with the fallback a BUILDER needs when no answer
// line was resolved (a version-neutral leg, or a responder invoked outside the
// inbound dispatch — the SDK Responder path, direct-drive tests).
//
// The fallback is the contract's highest line in THIS DEPLOYMENT'S DECLARED SET
// (D1a), read from the ctx carrier handleInbound sets, and only then the library
// build constant. Falling back to the build constant while the deployment declares
// something else would make a builder produce a line the gateway does not advertise
// — the same single-accessor breach D1a exists to prevent. contract == "" yields "",
// which every AtLine builder rejects; callers only pass a contract-mapped leg's
// contract.
func answerLineOr(ctx context.Context, contract string) string {
	if l := answerLineFrom(ctx); l != "" {
		return l
	}
	declared := declaredContractVersionsFrom(ctx)
	if len(declared) == 0 {
		declared = shnsdk.SupportedContractVersions()
	}
	return highestLine(contractLineSet(declared, contract))
}

// buildResponseLeg performs every fail-prone step of a response leg — check the
// answer against the ownership table, frame it, authorize, marshal the token,
// look up the requester, seal, encode — WITHOUT writing to w or committing any
// state. On failure it returns the gateway-standard (status, msg) with out==nil;
// on success it returns (out, 0, ""). Callers that mutate holder state for a leg
// MUST call this and check the status BEFORE committing state, so a
// constructible response-leg failure (a refused payload, unknown requester,
// seal, encode) cannot orphan payer state.
//
// p is the answer and k the transmit it is checked against (the recipient's
// response to the network); a refused payload is a 500 local fault. frame
// wraps the checked bytes for the requester (nil sends them bare); a framing
// error is a 502 "invalid application status".
//
// respFrame is the authority frame for the response: payer responses use
// "payer-coverage"; facility responses use "facility-disclosure". Passing it
// explicitly keeps the frame in one place per handler and avoids drift.
//
// consentRef anchors a consent-gated DISCLOSURE leg to the permit that authorized
// it (UC-05 facility responses pass the backstop-authenticated ref). The Hub
// copies it into the "answered" audit record, so the metadata-only audit view is
// consent-anchored on BOTH legs of a federated exchange — not just the request.
// Exchanges with no consent (payer responses) pass "".
//
// Direction symmetry: requester is the inbound envelope's Sender — whoever
// initiated this leg — rather than a hardcoded holder id. This means a future
// payer-originated push needs no retrofit: buildResponseLeg already replies to
// whoever sent the inbound envelope.
func (g *Gateway) buildResponseLeg(r *http.Request, respFrame, respOp, txType, inboundCorrID string, p relay.Payload, k relay.Key, frame func([]byte) ([]byte, error), subjectPCI, requester, consentRef string) (out []byte, status int, msg string) {
	payload, err := relay.Transmit(p, relay.Check(k))
	if err != nil {
		g.ownershipRefused(k, err)
		return nil, http.StatusInternalServerError, errOwnershipFault
	}
	if frame != nil {
		if payload, err = frame(payload); err != nil {
			return nil, http.StatusBadGateway, "invalid application status"
		}
	}
	requesterHolder, ok := g.cfg.Reg.Lookup(requester)
	if !ok {
		return nil, http.StatusInternalServerError, "requester not in registry"
	}
	// AI-2 (seal-then-authorize): seal the response payload FIRST, then authorize
	// against sha256hex(ciphertext) so the response token binds THIS payload. The
	// AuthzToken is cleartext metadata stamped onto the envelope after minting.
	respMeta := shnsdk.Metadata{
		Sender:          g.cfg.HolderID,
		Recipient:       requester,
		TransactionType: txType,
		AuthorityFrame:  respFrame,
		ConsentRef:      consentRef, // empty for non-consent exchanges (payer legs)
		Timestamp:       g.cfg.Clock().Format(time.RFC3339),
		CorrelationID:   inboundCorrID,
	}
	g.diagnosticStage(r.Context(), "recipient.response", txType, payload, 0, "")
	respEnv, err := shnsdk.Seal(respMeta, payload, requesterHolder.EncPub)
	if err != nil {
		return nil, http.StatusInternalServerError, "seal failed"
	}
	// C2: bind the response token to the SAME correlationID as the inbound leg so
	// the requester can verify the response leg is authorized for this exchange.
	respTok, err := g.authorize(r, respFrame, respOp, subjectPCI, inboundCorrID, "", sha256hex(respEnv.Ciphertext))
	if err != nil {
		return nil, http.StatusBadGateway, "authorization failed"
	}
	respTokStr, err := tokenJSON(respTok)
	if err != nil {
		return nil, http.StatusInternalServerError, "token marshal failed"
	}
	respEnv.Metadata.AuthzToken = respTokStr

	out, err = shnsdk.EncodeEnvelope(respEnv)
	if err != nil {
		return nil, http.StatusInternalServerError, "encode failed"
	}
	return out, 0, ""
}

// answerKey is the transmit a recipient's answer on leg is checked against.
func answerKey(leg string, outcome relay.Outcome) relay.Key {
	return relay.Key{Leg: leg, Role: relay.RoleRecipient, Direction: relay.DirectionResponse, Outcome: outcome}
}

// writeLeg writes an already-built response-leg envelope as the 200 response.
// The envelope comes from buildResponseLeg, which checked its payload; only
// the functions that called buildResponseLeg write it. The response leg is
// audited by the trusted Hub (fail-closed), not here.
func writeLeg(w http.ResponseWriter, out []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// frameNegotiated reports whether requester's registry entry advertises frame v1
// Capability is two-sided — the responder only frames to a peer
// that declared it can decode. Absent ⇒ the pre-v0.27.0 bare-payload contract.
func (g *Gateway) frameNegotiated(requester string) bool {
	h, ok := g.cfg.Reg.Lookup(requester)
	return ok && shnsdk.SupportsMessageFrameV1(h.MessageFrames)
}

// framePayload wraps an application answer in the v1 HTTP frame when requester
// negotiates it; legacy requesters get the payload bare (pre-v0.27.0 contract).
// contractToken, when non-empty, is stamped as the frame's contractVersion
// header. Relayed answers carry only explicit producer declarations. An encode error
// means an out-of-range status literal (caller bug) — fall back to bare so the
// exchange still answers.
func (g *Gateway) framePayload(requester string, status int, contentType, contractToken string, payload []byte) []byte {
	if !g.frameNegotiated(requester) {
		return payload
	}
	framed, err := shnsdk.EncodeHTTPFrameHeaders(status, map[string]string{
		"Content-Type":                    contentType,
		shnsdk.FrameHeaderContractVersion: contractToken, // "" is omitted by the encoder
	}, payload)
	if err != nil {
		return payload
	}
	return framed
}

// successFrame is the buildResponseLeg frame for a 2xx answer (framePayload).
func (g *Gateway) successFrame(requester string, status int, contentType, contractToken string) func([]byte) ([]byte, error) {
	return func(payload []byte) ([]byte, error) {
		return g.framePayload(requester, status, contentType, contractToken, payload), nil
	}
}

// respondLegPayload is the compatibility helper for answers built locally.
// respondLeg builds and writes a response leg in one call. Used by the legs that
// do NOT commit holder state between build and write (eligibility, CRD, DTR,
// federated query). The PAS legs call buildResponseLeg/writeLeg explicitly so
// they can commit state ONLY after a successful build. The
// answer is sealed with its actual status and media type for a frame-negotiated
// requester, bare legacy otherwise.
//
// builtToken is the contract-version token this answer's payload was BUILT at —
// the honored/recomputed answer line — and becomes the frame's contractVersion
// stamp. "" leaves the stamp at this build's own declared line for the leg
// (version-neutral legs are never stamped either way).
//
// A payload that is the participant's own message (relayed, possibly with
// registered edits) holds bytes THIS BUILD DID NOT PRODUCE. Stamp honesty: such
// an answer carries only its explicit producer declaration; the gateway never
// substitutes its own selected line for a producer declaration.
func (g *Gateway) respondLegPayload(w http.ResponseWriter, r *http.Request, respFrame, respOp, txType, inboundCorrID string, p relay.Payload, subjectPCI, requester, consentRef, builtToken string) {
	g.respondLeg(w, r, respFrame, respOp, txType, inboundCorrID, LegResult{Response: p}, subjectPCI, requester, consentRef, builtToken)
}

func (g *Gateway) respondLeg(w http.ResponseWriter, r *http.Request, respFrame, respOp, txType, inboundCorrID string, result LegResult, subjectPCI, requester, consentRef, builtToken string) {
	result, err := normalizeResult(result)
	if err != nil {
		g.responderFailed(w, txType, err)
		return
	}
	p := result.Response
	stamp, terr := g.contractTokenForLeg(txType, builtToken)
	if terr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": terr.Error()})
		return
	}
	stamp = stampForBuiltAnswer(result, stamp)
	out, status, msg := g.buildResponseLeg(r, respFrame, respOp, txType, inboundCorrID, p, answerKey(txType, relay.OutcomeAnswered),
		g.successFrame(requester, result.ApplicationStatus, p.ContentType(), stamp), subjectPCI, requester, consentRef)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	writeLeg(w, out)
}

// legibleLegErrorMessage returns msg unchanged when it is non-empty; otherwise it
// substitutes a fallback naming the leg and status, so respondLegError can never ship a
// requester a bare `{"error":""}` — the fail-closed guard for any connector/responder that
// answers non-2xx with no message of its own.
func legibleLegErrorMessage(txType string, status int, msg string) string {
	if msg != "" {
		return msg
	}
	return fmt.Sprintf("%s: recipient answered %d with no error detail", txType, status)
}

// respondLegError seals a recipient's application NON-2xx answer as a v1 message
// frame carrying the app status and relays it 200-to-Hub (verbatim — the
// payload-blind Hub never reinterprets an application answer), the
// error-branch sibling of respondLeg. buildResponseLeg is reused
// unchanged: the frame is just its payload. A non-negotiated (legacy) requester
// gets the pre-v0.27.0 bare non-2xx (which the payload-blind Hub reports as its
// generic mechanical 502). Callers MUST invoke this in the leg handler's
// `if result.Status != 0` branch, which returns BEFORE the R-7 fence, R-8
// $validate, and PAS Commit() — a rejected claim must not commit, and its armed
// defer-rollback must still fire.
//
// A set result.Response is the participant's application error, checked as an
// upstream error on the leg. An unset one is a refusal the gateway (or the
// connector) makes: its body is this gateway's {"error": Message}.
//
// builtToken is not used to label application errors. Only the producer's
// ResponseContractVersion, when supplied, describes a relayed error.
func (g *Gateway) respondLegError(w http.ResponseWriter, r *http.Request, respFrame, respOp, txType, corrID string, result LegResult, subjectPCI, requester, consentRef, builtToken string) {
	result, err := normalizeResult(result)
	if err != nil {
		g.responderFailed(w, txType, err)
		return
	}

	if result.ApplicationStatus/100 == 2 { // connector misuse guard: a 2xx belongs on the success seal
		g.respondLeg(w, r, respFrame, respOp, txType, corrID, result, subjectPCI, requester, consentRef, builtToken)
		return
	}
	// Ownership distinguishes an empty application answer from a local refusal.
	p, k := result.Response, answerKey(txType, relay.OutcomeUpstreamError)
	ownRefusal := p.Ownership() == relay.OwnershipAuthored && p.Builder() == relay.BuilderGatewayRefusal
	if p.Ownership() == 0 || ownRefusal {
		k = answerKey(txType, relay.OutcomeRefused)
		if p.Ownership() == 0 {
			body, _ := json.Marshal(map[string]string{"error": legibleLegErrorMessage(txType, result.Status, result.Message)})
			if !g.frameNegotiated(requester) {
				body = append(body, '\n')
			}
			var err error
			p, err = relay.Authored(relay.BuilderGatewayRefusal, body, "application/json")
			if err != nil {
				g.responderFailed(w, txType, err)
				return
			}
		}
	}
	ct := p.ContentType()
	if !g.frameNegotiated(requester) {
		// The legacy peer receives a bare application error. The payload-blind Hub
		// still reports its documented mechanical 502 to the requester.
		g.writePayload(w, result.Status, ct, p, k)
		return
	}
	appStatus := result.Status
	out, status, msg := g.buildResponseLeg(r, respFrame, respOp, txType, corrID, p, k, func(b []byte) ([]byte, error) {
		return shnsdk.EncodeHTTPFrameHeaders(appStatus, map[string]string{"Content-Type": ct, shnsdk.FrameHeaderContractVersion: result.ResponseContractVersion}, b)
	}, subjectPCI, requester, consentRef)
	if status != 0 { // refused payload, bad app status, or seal/authz BUILD failure → gateway fault
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	writeLeg(w, out)
}

// ValidatorReadinessForTest snapshots the existing PAS routing lanes without
// qualification or validation work. It is independent of source evidence.
func (g *Gateway) ValidatorReadinessForTest() map[string]bool {
	out := map[string]bool{}
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		v := g.validatorForContractLine("pa.pas", line)
		ready := v != nil
		if r, ok := v.(interface{ Ready() bool }); ok {
			ready = r.Ready()
		}
		out[line] = ready
	}
	return out
}
