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
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

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
	// New refuses any other non-empty value (OriginationProfiles). Unset, a provider
	// still serves its ingress and relays, but every route that originates refuses
	// with 503 "origination profile not set: set ORIGINATION_PROFILE (demo|provider-data)"
	// before anything is read, built or sent (requireOriginationProfile).
	OriginationProfile string
	// Identity is the gateway's substrate identity: its holder signing key (used to
	// sign holder assertions) plus its envelope-encryption keypair (used to open
	// inbound sealed payloads and seal responses).
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
	// RegistryRefresh, when set, re-reads Reg from the deployment's registrar
	// at once (gateway/app wires its registrar feed read). The inbound route
	// calls it, single-flight and bounded, when a leg's requester is not in
	// Reg, before refusing the leg as a requester not known yet (requester.go).
	// nil: Reg is refreshed only by whatever keeps it (the registrar poller, or
	// nothing for a static registry), and an unknown requester is refused at
	// once.
	RegistryRefresh func(context.Context) error
	// Validator is the CANONICAL-lane FHIR $validate client (FR-36). It stays the
	// only required validator: a deployment that speaks one line needs one lane.
	Validator shnsdk.Validator
	// ValidatorsByContractLine are the $validate lanes, by contract-version
	// token ("pa.pas@2.0"): the validator that resolves that contract's IG
	// packages at that line. A value may be a *DiscoveredLane, which serves
	// only once it has qualified. A single-line contract's line may name the
	// canonical validator; a multi-line contract's never borrows it.
	//
	// EMPTY map = an un-laned deployment: every contract line falls back to
	// Validator. NON-EMPTY map = the deployment is AUTHORITATIVE about which
	// contract lines it can validate, so one absent from it is UNLANED and
	// fails closed (see validatorForContractLine). ContractLineLanes derives it
	// from per-line lanes.
	ValidatorsByContractLine map[string]shnsdk.Validator
	// Deprecated: set ValidatorsByContractLine (ContractLineLanes derives it
	// from per-line lanes). New derives it from ValidatorsByLine,
	// CanonicalFallbackLines and DefaultValidatorsByLine when it is unset;
	// these stay until v1.0.0.
	//
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
	// CertificationValidatorsByLine are independent observational clients, never routing lanes.
	CertificationValidatorsByLine map[string]shnsdk.Validator
	certificationDisabled         bool
	// DefaultValidatorsByLine are lifecycle-managed candidates, admitted only after qualification.
	//
	// Deprecated: see ValidatorsByContractLine.
	DefaultValidatorsByLine map[string]*DiscoveredLane
	// CanonicalFallbackLines identifies single-native-contract compatibility aliases.
	//
	// Deprecated: see ValidatorsByContractLine.
	CanonicalFallbackLines map[string]bool
	// DeclaredContractVersions is the operator-declared exchange-contract token set
	// (SHN_CONTRACT_VERSIONS, boot-validated in gateway/app: grammar + ⊆
	// NativeContractVersions). Empty ⇒ DefaultContractVersions(). Read ONLY
	// through g.declaredContractVersions() (D1a) so selection, the published
	// CapabilityStatements, and the registry declaration peers select against cannot
	// diverge.
	DeclaredContractVersions []string
	// EgressNativeLines (D1c, productized in the kit-bridging slice) restricts
	// arm (2)'s (native-reach) view of NativeContractVersions() to exactly
	// these lines when non-nil. nil (the PRODUCTION default) = the real native
	// set — every published line is reachable natively, so arm (3) transform
	// chains stay dormant within {2.0,2.1,2.2}. Non-nil comes from exactly two
	// places: tests (the cross-line pair suite), and the loudly-named demo env
	// SHN_DEMO_EGRESS_NATIVE_LINES (gateway/app) — the SHN Kit's bridging demo
	// simulates "a build that predates the newer lines" with it. Never read
	// outside nativeLinesView (linereach.go).
	EgressNativeLines []string
	// StrictPeerForTest (per-peer strict extensions, FR-G52) is a TEST-ONLY
	// seam (EgressNativeLines' naming precedent) that forces
	// g.strictPeer's answer to true for every consult. PRODUCTION default
	// (false, the zero value, never set from env): strictPeer is dormant —
	// see its comment in linereach.go for why arm (3)'s per-peer
	// gated-overlay input stays hardwired false today. Never read outside
	// strictPeer; never wired from env/config in app.go.
	StrictPeerForTest bool
	// SoR reads the holder's backing system of record (resolve/coverage/clinical/
	// supplemental/facility-records). E2 swaps in a FHIR client; demo uses the stub.
	SoR SystemOfRecord
	// RequireKnownMembers is the participant's opt-in to checking members against
	// its own system of record: on the Da Vinci CRD/DTR/PAS legs, a subject the
	// system of record does not hold is refused ("unknown member"). The default
	// (false, the zero value) stays out of the way: such a subject binds by member id
	// and the Patient the request carries (resolveSubjectPCI). Set from
	// REQUIRE_KNOWN_MEMBERS (gateway/app).
	RequireKnownMembers bool
	// RequireSharedLine is the participant's opt-in to refusing a pair that
	// shares no declared IG line. The default (false, the zero value) carries
	// every message as sent, whatever the two participants' lines, and records
	// it (LineCarriedEvent). Set, the gateway refuses such a pair as routing:
	// a request it would send to a peer with no shared line that no native
	// build or certified translation reaches, a request whose frame claims a
	// line it cannot answer at, and an answer stamped with another line than
	// the leg routed. It never changes which line is chosen when the pair
	// shares one. Set from REQUIRE_SHARED_LINE (gateway/app).
	RequireSharedLine bool
	// TranslateIGLines is the participant's opt-in to module 2, the IG-line
	// bridge: translating a message it builds to a peer's IG line through the
	// registered compatibility steps where a certified translation exists
	// (arm (3)), and carrying a questionnaire package answer into its own DTR
	// line's parameter names (E-08). The default (false, the zero value)
	// translates nothing: a pair that shares no line is built natively at a
	// line the peer declares where this build can (arm (2)), else carried as
	// sent, and an answer reaches the participant's system exactly as it
	// arrived. Set from TRANSLATE_IG_LINES (gateway/app).
	TranslateIGLines bool
	// EnrichNativeRequests is the participant's opt-in to having its gateway add
	// what a Da Vinci-native request leaves out, from the participant's own system
	// of record. It gates the three native-ingress enrichments: the CDS
	// Hooks prefetch fill (E-02), the questionnaire-package Coverage append (E-04)
	// and the questionnaire-package Patient append (E-05). The default (false, the
	// zero value) adds nothing from the system of record: the request is carried
	// as the participant's client sent it, apart from the callback strip (E-01)
	// and the coverage carry (E-07) that follows a fhirServer read. A Coverage the
	// request does not carry is still read from the system of record to choose
	// the payer, and is not inserted. Requests the gateway builds itself (origination) are
	// unaffected. Set from ENRICH_NATIVE_REQUESTS (gateway/app). New installs
	// the enrichment hook (workstream_pa_enrich.go), in the PA workstream's
	// state, only when it is set.
	EnrichNativeRequests bool
	// FHIRServerRead (CDS_FHIR_SERVER_READ) is how a provider gateway reads a
	// CRD request's Coverage through the request's own fhirServer, to route
	// by, when the request carries none for a member its system of record
	// does not hold (or cannot name); what routing used is then carried as
	// prefetch.coverage (E-07, workstream_pa_fhirserver_carry.go): FHIRServerReadPrivate
	// ("" too, the default: the zero value reads), FHIRServerReadPublic, or
	// FHIRServerReadOff, which never reads and carries nothing
	// (fhirserver_read.go).
	FHIRServerRead string
	// fhirServerResolve, fhirServerRoots and fhirServerDialed replace the
	// fhirServer read's resolver, TLS roots and dial hook in tests; the address
	// check is never replaced.
	fhirServerResolve func(ctx context.Context, host string) ([]netip.Addr, error)
	fhirServerRoots   *x509.CertPool
	fhirServerDialed  func(netip.AddrPort)
	fhirServerDial    func(ctx context.Context, network, address string) (net.Conn, error)
	// fhirServerBudget, when set, replaces the time every read for one
	// request shares (fhirServerReadTimeout) in tests.
	fhirServerBudget time.Duration
	// Store is the gateway's own business state (auth numbers, pended-claim ledger).
	// Required: New panics without one. The published gateway (gateway/app)
	// sets NewMemStore, or gateway/connectors/pgstore when SHN_STORE_DATABASE_URL is set.
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
	// IngressBaseURL is the gateway's CONFIG-PINNED public base URL: the SMART
	// Backend Services aud (assertion + bearer) and the advertised token endpoint.
	// Never request-derived (no Host-header spoof). Required when IngressEnabled and
	// not bypassed. Set from PROVIDER_DAVINCI_INGRESS_BASE_URL by app.go.
	IngressBaseURL string
	// ScenarioCallers are the callers this gateway's /scenario operator routes
	// admit, by label: each request must carry the caller's own
	// assertion (gateway/scenarioauth), verified before the route runs. An
	// empty set admits nothing. nil leaves the routes unauthenticated, as they
	// were: a temporary default, until every stack configures its callers,
	// that becomes refuse. Set from SCENARIO_CALLERS_FILE (gateway/app).
	ScenarioCallers map[string]ed25519.PublicKey

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
	// Observer, when non-nil, receives a structured ObserverEvent at each
	// gateway-edge seam (origination legs, Da Vinci ingress, $validate calls).
	// PAYLOADS INCLUDED — cleartext FHIR as seen at this edge. nil (the
	// default) = no observation. MAY BE CALLED CONCURRENTLY (handlers run on
	// concurrent goroutines); implementations must be goroutine-safe —
	// observer.Hub.Emit locks internally. Additive instrumentation only:
	// emission must not change exchange behavior
	// (TestObserver_ConformanceNeutral). See STABILITY.md; the SHN Kit's
	// supervisor always sets it, prod deployments never have it on unless the
	// operator opts in via OBSERVER_ADDR.
	Observer func(ObserverEvent)
	// Diagnostic is an optional prompt, concurrency-safe, nonblocking sink.
	// It must reserve bounded memory before retaining event bytes. Nil disables it.
	Diagnostic DiagnosticSink
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
	// ExchangeObserved, when non-nil, receives one ExchangeRecord per call this
	// gateway answers on a Da Vinci ingress route or on /substrate/inbound,
	// after the answer is written — every refusal and a panicking handler
	// included (exchangerecord.go). Metadata only, never a payload. MAY BE
	// CALLED CONCURRENTLY; implementations must be goroutine-safe and must
	// never block. A panic in it is recovered. nil (the default) records
	// nothing. Additive instrumentation only: it never changes an answer.
	ExchangeObserved func(ExchangeRecord)
	// Replay is the one-time-use record behind the Hub-assertion jti and ingress
	// client_assertion jti guards. Nil selects a
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
	//   "replay"     — the one-time-use record could not be consulted, on both
	//                  scopes: the ingress client_assertion jti (/oauth/token answers
	//                  503) and the Hub assertion (/substrate/inbound answers 503).
	//   "ingresskey" — the signing key could not be resolved, so no bearer can be
	//                  issued (/oauth/token answers 503).
	//   "exchange"   — the best-effort correlation seam failed (the request itself is
	//                  unaffected).
	// The store implementations report their own internal failures — an ingress key
	// RELOAD error behind a verification miss, a failed exchange insert — through the
	// same counter via their own error hook, wired at the application layer.
	StoreErrorMetric func(store string)
	// HubAcceptsInvolved reports that the network's Hub reads an envelope's
	// involved list: its discovery descriptor lists shnsdk.HubAcceptsInvolved in
	// hubAccepts (read once at start by gateway/app). Only then does a
	// prior-authorization leg carry the other patients it involves (involved.go),
	// so that the Hub records the exchange under each of them too. The zero
	// value sends none, reads nothing more from the system of record and
	// requests no further token.
	HubAcceptsInvolved bool
	// InvolvedBudget bounds the whole involved pass of one leg: the
	// system-of-record reads and the token requests it makes (involved.go).
	// A patient not named when it runs out is left out, and the leg is sent.
	// Zero selects 2s.
	InvolvedBudget time.Duration
	// InvolvedMetric, when set, is called once for each involved patient a leg
	// leaves out of its list, with the reason (involved.go). Leaving one out
	// never changes the leg; this count is how an operator sees it.
	InvolvedMetric func(reason string)
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
	// level (§3 of the conformance-enforcement-levels design). The ZERO VALUE
	// IS STRICT, and it stays strict for every in-process construction here or
	// in any test harness that does not say otherwise. A DEPLOYED gateway is
	// different: the gateway/app env loader maps an ABSENT
	// CONFORMANCE_ENFORCEMENT to EnforcementObserve, which is the one and only
	// place a non-strict level comes from an omission. Every gate, reference
	// participant and hosted gate therefore pins its level explicitly
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
	cfg Config
	// pending is module 1's in-flight pend state (module1_pending.go).
	pending moduleOnePending

	// states is each compiled-in workstream's own state on this gateway, by
	// workstream id, built once by New (workstream.newGatewayState). A
	// Gateway assembled without New holds none.
	states map[string]gatewayState

	// regRefresh runs Config.RegistryRefresh single-flight for the inbound
	// requester guard; regRefreshBudget overrides registryRefreshBudget (tests).
	regRefresh       registryRefresher
	regRefreshBudget time.Duration

	// exchanges is the Layer-2 Exchange-correlation seam (the DaVinciIngress origination
	// driver groups each ingress call's legs under one Exchange.ID). In-memory default
	// (bounded by ExchangeTTL, correct at exactly one replica); Config.Exchanges swaps in
	// a durable, shared backend — the application layer selects the Postgres one under
	// SHN_STORE_DATABASE_URL.
	exchanges ExchangeStore

	// replay is the shared one-time-use record for every replay guard: the
	// Hub's X-Hub-Assertion jti (window
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
	edgeCapture   atomic.Pointer[edgeCaptureStore]
	certification *certificationWorker
	operations    operationTracker
	// explicitLanes are the explicitly configured lanes whose reachability
	// module 2 reads (lanehealth.go), stopped by Close.
	explicitLanes []*explicitLane

	// observeOnce/observeChecks: observe-level checks run off the request path
	// (observe_async.go), started on first use.
	observeOnce   sync.Once
	observeChecks atomic.Pointer[observeChecks]

	// fallbackStore is the in-memory Store whose workstream state a request
	// falls back to when the configured Store keeps none of its own: today the
	// prior-authorization continuation store (workstream_pa_continuation.go).
	// Every Store this repository ships keeps one, so this is the seam for a
	// participant's own Store written before continuations existed — it keeps
	// such a deployment working, and it reports itself as non-durable, which is
	// the fact the Kit's notice and CONFIGURATION state.
	//
	// An atomic.Pointer built lazily by CompareAndSwap, for the same reason
	// edgeCapture above is: a Gateway assembled directly (bypassing New, a common
	// test pattern in this package) must still have one, and two requests may
	// reach for it at once.
	fallbackStore atomic.Pointer[MemStore]
}

// New constructs a Gateway. The clock defaults to time.Now and the client to
// http.DefaultClient when unset.
//
// It returns an ERROR (never a panic) for the conditions a DEPLOYMENT can hit with
// otherwise-valid config: a payer role with no content occupant (§3.2 — the
// fail-closed payer boot), an unusable ingress client registration, and an
// OriginationProfile that names no origination lane (OriginationProfiles). The remaining
// checks stay panics: they fire only on a caller that omitted a required field, which is
// a programming error, not a deployment one.
//
// Caveat: the observer decoration below (Config.Validator/ValidatorsByContractLine
// -> observingValidator) only wraps whatever the two fields hold AT construction
// time. A handful of test sites assign to g.cfg.ValidatorsByContractLine directly
// AFTER New returns (test-only pattern — production code never does this,
// since app.go always builds the full ValidatorsByContractLine map before
// calling New) — those post-construction lanes bypass decoration and never emit
// validate.result even with Observer set. Harmless where it happens (those
// tests don't assert on the observer stream for that lane), but worth
// knowing before adding a new one that does.
func New(cfg Config) (*Gateway, error) {
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
	if err := workstreamStoreFault(cfg.Store); err != nil {
		return nil, fmt.Errorf("gateway: %w", err)
	}
	// Every role signs holder assertions, so a gateway with no signing key is
	// never valid. Fail fast at
	// construction: the Config→Identity reshape made a forgotten Identity a
	// single omitted field that leaves all key material nil and panics deep in
	// Seal/Open at first use — this turns that into a clear construction error.
	if cfg.Identity.SignPriv == nil {
		panic("gateway: Config.Identity.SignPriv is required (holder assertions)")
	}
	deriveContractLanes(&cfg)
	// With module 2 on, each explicitly configured lane says whether it can
	// be reached, from its own failures, so a chain is planned only to a lane
	// that can certify it (lanehealth.go). A fresh map: the caller's is never
	// mutated.
	var explicit []*explicitLane
	if cfg.TranslateIGLines && len(cfg.ValidatorsByContractLine) > 0 {
		cfg.ValidatorsByContractLine, explicit = explicitLanes(cfg.ValidatorsByContractLine)
	}
	switch cfg.Role {
	case "payer", "facility", "phg":
		if len(cfg.HubTransportPub) == 0 {
			panic("gateway: HubTransportPub required for role " + cfg.Role + " (mounts /substrate/inbound; hop-auth has no off state)")
		}
	}
	// Observer seam: decorate the SoR BEFORE the Populator derivation below — it
	// captures cfg.SoR at construction (newManagedPopulator), and decorating at the
	// validator's later site would leave them reading the raw SoR forever.
	// g does not exist yet here, so the decorator closes over Observer +
	// Clock (already defaulted above).
	// Idempotence guard: never double-wrap (double emission) if a caller
	// passes an already-observed SoR back through New.
	if cfg.Observer != nil && cfg.SoR != nil {
		if _, already := cfg.SoR.(observingSoR); !already {
			cfg.SoR = observingSoR{inner: cfg.SoR, observer: cfg.Observer, clock: cfg.Clock}
		}
	}
	// FAIL-CLOSED PAYER BOOT (§3.2). A payer gateway answers Da Vinci legs out of a
	// content OCCUPANT and nothing else: either a native-forward Responder built against a
	// real Da Vinci endpoint (PAYER_DAVINCI_BASE_URL) or a LegResponder a partner injects
	// directly. There is NO derived in-process fallback any more — the in-process responder
	// that used to be synthesized here from Config.Adjudicator is deleted, so a payer with
	// no occupant refuses to boot instead of quietly serving invented verdicts.
	if err := checkOriginationProfile(cfg.OriginationProfile); err != nil {
		return nil, err
	}
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
		cfg:           cfg,
		explicitLanes: explicit,
	}
	g.states = workstreamGatewayStates(g)
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
	// The contract-line lanes must be decorated too, and for the same reason:
	// once ValidatorsByContractLine is non-empty, validatorForContractLine
	// resolves EVERY line-scoped $validate through that map (lanes.go) and
	// never through cfg.Validator — so wrapping only cfg.Validator left the
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
		if len(g.cfg.ValidatorsByContractLine) > 0 {
			// A fresh map: the caller's (gateway/app builds one per boot, and a test
			// may share one across gateways) must never be mutated from in here.
			// A discovered lane is observed when it serves (readyLane), not here.
			lanes := make(map[string]shnsdk.Validator, len(g.cfg.ValidatorsByContractLine))
			for key, v := range g.cfg.ValidatorsByContractLine {
				_, discovered := v.(*DiscoveredLane)
				if _, already := v.(observingValidator); already || discovered || v == nil {
					lanes[key] = v
					continue
				}
				lanes[key] = observingValidator{inner: v, g: g}
			}
			g.cfg.ValidatorsByContractLine = lanes
		}
	}
	// Build the inbound auth server only for a real-auth ingress (not under the
	// test bypass — body-conformance tests don't register clients). app.go has
	// already validated registrations; a failure here is a config invariant.
	if cfg.IngressEnabled && !cfg.ingressAuthBypass {
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

// noPayerIdentifier is the routing refusal for a Coverage whose payor names no
// payer identifier that can be read.
const noPayerIdentifier = "no payer identifier on member coverage"

// recipientForWith resolves the payer holder for an exchange from a Coverage (FR-G40). resolveRef
// resolves an EXTERNAL Coverage.payor Organization reference ("<Type>/<id>"): at origination that is
// the provider SoR; on INGRESS it is the inbound payload's OWN resources — a conformant partner's
// payor Organization lives in its bundle/prefetch/parameters, NOT the provider SoR (bundleRefs
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
		return "", shnsdk.PayerIdentifier{}, http.StatusUnprocessableEntity, noPayerIdentifier
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

// resetExchanges clears the holder's exchange records in the configured store,
// in place: never reassigned, or a shared or durable store would silently
// revert to a process-local one for the rest of the process's life. The store
// holds only metadata-only LegRecords. A store with no Reset keeps its
// records, and that is logged; a store whose Reset fails returns the error,
// counted. The store is write-once in New, and a durable store's Reset is a
// bounded DB round trip, so no lock is held.
func (g *Gateway) resetExchanges() error {
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

// Handler returns the role-appropriate HTTP routes.
func (g *Gateway) Handler() http.Handler {
	mux := g.routes()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		done := g.operations.begin()
		defer done()
		mux.ServeHTTP(w, r)
	})
}

// mountedRoute is one route a piece of the gateway declares for its role.
// originates marks a route that builds and sends a request of this gateway's
// own (on-behalf building): it is mounted behind requireOriginationProfile,
// so it refuses while Config.OriginationProfile is unset. operator marks a
// route of the gateway's operator surface (/scenario): it is mounted behind
// requireScenarioCaller, outermost, so an unauthenticated caller is refused
// before anything else runs.
type mountedRoute struct {
	pattern    string
	handler    http.HandlerFunc
	originates bool
	operator   bool
}

// routes mounts what each piece of this gateway declares for its role
// (AI-G10): the core's own routes, every compiled-in workstream adapter's
// (workstreamRoutes), and on-behalf building's. It is the only place a route
// is mounted, the only place an originating route is put behind
// requireOriginationProfile, and the only place an operator route is put
// behind requireScenarioCaller.
func (g *Gateway) routes() *http.ServeMux {
	mux := http.NewServeMux()
	role := g.cfg.Role
	for _, declared := range [][]mountedRoute{g.coreRoutes(role), g.workstreamRoutes(role), g.originationRoutes(role)} {
		for _, rt := range declared {
			h := rt.handler
			if rt.originates {
				h = g.requireOriginationProfile(h)
			}
			if rt.operator {
				h = g.requireScenarioCaller(h)
			}
			mux.HandleFunc(rt.pattern, h)
		}
	}
	return mux
}

// coreRoutes are the core's own routes: a responding role's Hub-routed legs.
func (g *Gateway) coreRoutes(role string) []mountedRoute {
	switch role {
	case "payer", "facility", "phg":
		return []mountedRoute{{pattern: "POST /substrate/inbound", handler: g.inboundRoute()}}
	}
	return nil
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
func (g *Gateway) ingressAuthOK(r *http.Request) (ok bool, unavailable bool) {
	_, ok, unavailable = g.ingressAuthClient(r)
	return ok, unavailable
}

// ingressAuthClient is ingressAuthOK and the client it verified: the
// client_id of the bearer this gateway issued, or the iss of a registered
// client's direct bearer. "" under the test bypass, which verifies no client.
func (g *Gateway) ingressAuthClient(r *http.Request) (client string, ok bool, unavailable bool) {
	if g.cfg.ingressAuthBypass {
		return "", true, false
	}
	if g.ingressAuth == nil {
		return "", false, false // fail-closed: no inbound auth configured
	}
	// SMART Backend Services issued bearer (token-exchange) OR a UDAP B2B direct bearer
	// (a registered client's self-signed private_key_jwt, the form br-provider sends).
	// Token-shape disjoint, so the OR cannot fail open (FR-G28 UDAP B2B).
	//
	// ORDER IS LOAD-BEARING: verifyBearer is the only arm that touches the ingress key
	// store, and the direct-bearer arm keeps answering through a key-store outage — so a
	// store outage is reported only when NEITHER arm admitted the caller.
	if client, ok, unavailable := g.ingressAuth.verifyBearer(r); ok {
		return client, true, false
	} else if client, ok := g.ingressAuth.verifyDirectBearer(r); ok {
		return client, true, false
	} else {
		return "", false, unavailable
	}
}

// ingressAuthRefused writes the refusal an ingress route owes when the caller is not
// admitted, and reports whether it wrote one (the handler must then stop). A key-store
// outage answers the 503 every other shared-state refusal answers — the same shape the
// token endpoint's oauthUnavailable uses — never the 401 that tells an honest partner its
// credential is bad. The ingress client is the one that decides to retry; this call is
// refused.
func (g *Gateway) ingressAuthRefused(w http.ResponseWriter, r *http.Request) bool {
	client, ok, unavailable := g.ingressAuthClient(r)
	if ok {
		noteIngressCaller(r.Context(), client)
		return false
	}
	if unavailable {
		writeStoreUnavailable(w, "ingress key store unavailable")
		return true
	}
	exchangeOf(r.Context()).refused(RefusalAuthentication)
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "ingress authentication required"})
	return true
}

// originationProfileNotSet is the refusal an originating route answers on a provider
// whose Config.OriginationProfile is unset. Stable text: callers may match it.
var originationProfileNotSet = "origination profile not set: set ORIGINATION_PROFILE (" + strings.Join(originationProfiles, "|") + ")"

// requireOriginationProfile guards a route that originates. With no origination
// profile no lane is chosen, and the origination paths, which compare the profile
// exactly, would build a PAS request carrying a payer Organization no system of record
// supplied. So the route refuses before anything is read, built or sent: 503, uncacheable,
// the package's {"error": …} envelope (writeUnavailable). 503, not 4xx: the request is
// well-formed and the caller cannot fix it; this gateway cannot serve the route as
// configured, the same cause-stating refusal writeStoreUnavailable gives for an outage.
func (g *Gateway) requireOriginationProfile(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if g.cfg.OriginationProfile == "" {
			writeUnavailable(w, originationProfileNotSet)
			return
		}
		// The Claim facts the request states (module1_claimfacts.go), read before any
		// handler decodes the body for its own keys.
		h(w, withOriginationClaimInput(r))
	}
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
	writeUnavailable(w, msg)
}

// writeUnavailable answers 503, uncacheable, in this package's {"error": …} envelope:
// the route cannot be served as this gateway stands, and msg names why.
func writeUnavailable(w http.ResponseWriter, msg string) {
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
	// THIS payload (AI-2). Empty for non-envelope ops.
	PayloadHash string `json:"payloadHash,omitempty"`
	// Involvement asks for a token for another patient the leg involves
	// (shnsdk.Involvement*), carried in the envelope's involved list. Empty for
	// the leg's own token.
	Involvement string `json:"involvement,omitempty"`
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

// errAuthzUnreachable marks an authorize call the Authorization Framework
// never answered: the connection was refused, reset or closed, or failed some
// other way before any response, including no answer within the client's
// timeout; or a 503 or 504, which the Framework never answers itself, so it
// came from a proxy or load balancer in front of it with no Framework behind.
// It is the network being unavailable, not an authority verdict, so the
// provider's route answers its own 503 and the leg is not sent to the Hub; a
// denial (errAuthorizationDenied) and any other answer keep their own
// answers.
var errAuthzUnreachable = errors.New("the authorization service could not be reached, so this leg was not sent to the payer")

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

// HubDeliveredHeader is the header the Hub sets on every POST /route error,
// saying whether the recipient's gateway received the request: "no" when the Hub
// refused before forwarding or the recipient's gateway was not reached or
// refused it at its edge; "yes" when it answered and the answer was lost on the
// way back (a response the Hub could not read, verify, decode or audit);
// "unknown" when it may have received the request (it answered 5xx, the
// connection failed after the request was sent, or it did not answer in time).
// An error without it did not come from the Hub's route handler, and a 5xx of
// that kind is read as "unknown". internal/hubsvc sets it; the root module's
// tests pin the two names equal.
const HubDeliveredHeader = "X-SHN-Delivered"

// hubRefusalError is the Hub's own non-2xx answer to POST /route: its status,
// its reason ({"error": ...}) and whether the recipient was reached. The
// requester reports it as it is instead of one generic routing failure.
type hubRefusalError struct {
	status    int
	reason    string
	delivered string // "" (not delivered), "yes" or "unknown" (HubDeliveredHeader)
	// fromHub says the Hub's route handler wrote the answer: it carried
	// HubDeliveredHeader. An answer without it came from in front of the Hub
	// (a load balancer), or was built here for a failed connection.
	fromHub bool
	// notActed is the recipient gateway's attestation, relayed by the Hub on
	// its 503 (NotActedHeader), that it refused the leg before acting because
	// it does not know this gateway yet; retryAfter is the Retry-After the Hub
	// relayed with it, in seconds.
	notActed   bool
	retryAfter int
}

func (e *hubRefusalError) Error() string {
	return fmt.Sprintf("hub refused the exchange (%d): %s", e.status, e.reason)
}

// answerLostError is a leg the recipient answered whose answer this gateway
// could not accept: a response leg that failed verification or decryption, a
// frame it could not decode, or a frame declaring another contract line than
// the leg was routed at. The recipient received the request and may have
// acted on it, so it is reported as such — never as a routing
// failure the caller might blindly resend.
//
// rule is the network rule the refusal is recorded under; empty is integrity.
type answerLostError struct{ cause, rule string }

func (e *answerLostError) Error() string { return e.cause }

// hubTimeoutError is the error a timed-out Hub leg returns. budget is the
// gateway's own leg deadline when that is what ended the wait; zero when the
// caller's request deadline ended it first, in which case no number is
// claimed.
// sent says the request had been written to the Hub when the wait ended, so
// the recipient may have received it.
type hubTimeoutError struct {
	budget time.Duration
	sent   bool
}

func (e *hubTimeoutError) Error() string {
	msg := errHubTimeout.Error()
	if e.budget > 0 {
		msg = fmt.Sprintf("no answer on the hub leg within %s (hub leg timeout)", e.budget)
	}
	if e.sent {
		msg += "; the recipient may have received this request: check its outcome before resending"
	}
	return msg
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
	return g.authorizeRequest(r, authorizeReq{Frame: frame, Operation: operation, SubjectPCI: subjectPCI, Custodian: custodian, CorrelationID: correlationID, PayloadHash: payloadHash})
}

// authorizeRequest is authorize with the whole request, so a token for an
// involved patient can carry its involvement (involved.go).
func (g *Gateway) authorizeRequest(r *http.Request, req authorizeReq) (shnsdk.Token, error) {
	// H1: authenticate to the Authorization Framework with a holder assertion for
	// the "authz" audience so the policy can bind authority to THIS holder. The
	// provider authorizes as "provider", the payer as "payer" (via cfg.HolderID).
	body, err := json.Marshal(req)
	if err != nil {
		return shnsdk.Token{}, err
	}
	// One retry, and only for a call authz cannot have received: a connection
	// refused, reset or closed before the request was written. After
	// the write authz may have decided and recorded the decision, and its
	// decision record carries no correlation id a second decision could be
	// attributed by, so that call is not repeated. An answer is never retried.
	tok, retry, err := g.authorizeOnce(r.Context(), body)
	if retry && r.Context().Err() == nil {
		tok, _, err = g.authorizeOnce(r.Context(), body)
	}
	return tok, err
}

// authorizeOnce makes one authorize call, with its own holder assertion: authz
// consumes each assertion's jti once, so a repeated call needs a fresh one. It
// reports whether the call failed before its request was written in a way a
// second attempt could clear.
func (g *Gateway) authorizeOnce(ctx context.Context, body []byte) (shnsdk.Token, bool, error) {
	// H1: authenticate to the Authorization Framework with a holder assertion for
	// the "authz" audience so the policy can bind authority to THIS holder. The
	// provider authorizes as "provider", the payer as "payer" (via cfg.HolderID).
	assertion := shnsdk.IssueAssertion(g.cfg.HolderID, "authz", g.cfg.Identity.SignPriv, g.cfg.Clock(), time.Hour)
	assertionJSON, err := json.Marshal(assertion)
	if err != nil {
		return shnsdk.Token{}, false, err
	}
	var wrote atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				wrote.Store(true)
			}
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.cfg.AuthzURL+"/authorize", bytes.NewReader(body))
	if err != nil {
		return shnsdk.Token{}, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Holder-Assertion", base64.StdEncoding.EncodeToString(assertionJSON))
	client := g.cfg.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		retry := !wrote.Load() && (errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) ||
			errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF))
		return shnsdk.Token{}, retry, fmt.Errorf("%w (%w)", errAuthzUnreachable, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, shnsdk.MaxResponseBytes))
	if err != nil {
		return shnsdk.Token{}, false, fmt.Errorf("read authorize answer: %w", err)
	}
	// A 403 is a policy/consent DENIAL (not a transport failure); surface it as
	// the typed sentinel so callers can distinguish it from the Authorization
	// Framework being unreachable or erroring.
	if resp.StatusCode == http.StatusForbidden {
		return shnsdk.Token{}, false, errAuthorizationDenied
	}
	// The Framework never answers 503 or 504 itself: the proxy in front of it
	// (a service mesh sidecar, a load balancer) had no Framework to reach. Not
	// retried: the request reached that proxy.
	if resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout {
		return shnsdk.Token{}, false, fmt.Errorf("%w (the authorize call was answered %d)", errAuthzUnreachable, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return shnsdk.Token{}, false, &shnsdk.StatusError{Code: resp.StatusCode, Body: string(respBody)}
	}
	var out authorizeResp
	if err := json.Unmarshal(respBody, &out); err != nil {
		return shnsdk.Token{}, false, fmt.Errorf("decode authorize answer: %w", err)
	}
	return out.Token, false, nil
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

	// Whether the request was written decides what a transport failure means:
	// before, nothing reached the Hub; after, it may have forwarded the request.
	var wrote atomic.Bool // once any attempt was written, it may have been received
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				wrote.Store(true)
			}
		},
	}))
	resp, err := client.Do(req)
	if err != nil {
		if wrote.Load() {
			return shnsdk.Envelope{}, &hubSentError{err: err}
		}
		return shnsdk.Envelope{}, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, shnsdk.MaxResponseBytes))
	if err != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// A Hub 200 comes only after the recipient answered and the Hub verified it.
		return shnsdk.Envelope{}, &answerLostError{cause: "the Hub's answer could not be read"}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		g.diagnosticHubRefusal(ctx, resp, respBody)
		return shnsdk.Envelope{}, withNotActed(hubRefusal(resp.StatusCode, respBody, resp.Header.Get(HubDeliveredHeader)), resp.Header)
	}
	env, err := shnsdk.DecodeEnvelope(respBody)
	if err != nil {
		return shnsdk.Envelope{}, &answerLostError{cause: "the Hub's answer is not an envelope"}
	}
	return env, nil
}

// hubSentError is a POST to the Hub whose connection failed after the request
// was written: the Hub may have forwarded it.
type hubSentError struct{ err error }

func (e *hubSentError) Error() string { return e.err.Error() }
func (e *hubSentError) Unwrap() error { return e.err }

// withNotActed marks e as the recipient's not-acted refusal when the Hub's
// route handler answered 503, not delivered, relaying NotActedHeader with
// NotActedRequesterUnknown. Retry-After is read as a whole number of seconds
// from 1 to 60; anything else reads as requesterUnknownRetryAfter.
func withNotActed(e *hubRefusalError, h http.Header) *hubRefusalError {
	if !e.fromHub || e.status != http.StatusServiceUnavailable || e.delivered != "" || h.Get(NotActedHeader) != NotActedRequesterUnknown {
		return e
	}
	e.notActed = true
	e.retryAfter = requesterUnknownRetryAfter
	if n, err := strconv.Atoi(h.Get("Retry-After")); err == nil && n >= 1 && n <= 60 {
		e.retryAfter = n
	}
	return e
}

// hubRefusal reads the Hub's {"error": ...} body (or names the status when
// there is none) into a hubRefusalError.
func hubRefusal(status int, body []byte, delivered string) *hubRefusalError {
	var e struct {
		Error string `json:"error"`
	}
	reason := ""
	if json.Unmarshal(body, &e) == nil {
		reason = strings.TrimSpace(e.Error)
	}
	if reason == "" {
		reason = http.StatusText(status)
	}
	if len(reason) > 300 {
		cut := 300
		for cut > 0 && !utf8.RuneStart(reason[cut]) {
			cut--
		}
		reason = reason[:cut]
	}
	fromHub := true
	switch delivered {
	case "yes", "unknown":
	case "no":
		delivered = ""
	default:
		fromHub = false
		// Unmarked: not the Hub's route handler (a load balancer in front of
		// it). A 5xx of that kind may have come after the Hub forwarded.
		delivered = ""
		if status >= 500 {
			delivered = "unknown"
		}
	}
	return &hubRefusalError{status: status, reason: reason, delivered: delivered, fromHub: fromHub}
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
	if g.cfg.Diagnostic != nil {
		ctx = context.WithValue(ctx, diagnosticLegKey{}, &diagnosticLeg{sender: g.cfg.HolderID, recipient: recipient, correlation: correlationID})
	}
	requestBytes, err := g.admit(content.Payload, contentRequestKey(txType, content))
	if err != nil {
		g.diagnosticStage(ctx, "relay.ownership-refused", txType, nil, 500, err.Error())
	}
	if err != nil {
		return nil, err
	}
	g.observe(ObserverEvent{
		Kind: "leg.originated", Direction: "originate", LegType: txType,
		CorrelationID: correlationID, Counterpart: recipient,
		AuthorityFrame: reqFrame, Op: op, Payload: json.RawMessage(requestBytes),
		Route: content.Route,
	})
	g.diagnosticStage(ctx, "leg.originated", txType, requestBytes, 0, "")
	g.legMetric(LegOutcomeRouted)
	x := exchangeOf(ctx)
	x.peers(g.cfg.HolderID, recipient)
	if content.Route != nil {
		x.line(shnsdk.LineOf(content.Route.Token))
	}
	respPayload, err := g.roundTripInner(ctx, r, recipient, reqFrame, respFrame, op, respOp, txType, scope, pci, correlationID, custodian, content)
	x.legOutcome(err)
	if err != nil {
		var re *RelayError
		if errors.As(err, &re) {
			g.diagnosticStage(ctx, "leg.response", txType, re.Body, re.Status, "")
			// The recipient answered non-2xx — observed as a response (with status), not a failure.
			g.observe(ObserverEvent{
				Kind: "leg.response", Direction: "originate", LegType: txType,
				CorrelationID: correlationID, Counterpart: recipient,
				AuthorityFrame: respFrame, Op: respOp, Status: re.Status,
				Payload: json.RawMessage(re.Body),
			})
			g.legMetric(LegOutcomeAnswered)
			return nil, err
		}
		outcome := LegOutcomeFailed
		switch {
		case errors.Is(err, errAuthorizationDenied):
			outcome = LegOutcomeDenied
		case errors.Is(err, errHubUnreachable), errors.Is(err, errHubTimeout), errors.Is(err, errAuthzUnreachable):
			outcome = LegOutcomeUnreachable
		default:
			var refused *hubRefusalError
			if errors.As(err, &refused) && refused.delivered == "" {
				outcome = LegOutcomeUnreachable
			}
		}
		g.diagnosticStage(ctx, "leg.failed", txType, nil, 0, err.Error())
		g.legMetric(outcome)
		g.observe(ObserverEvent{
			Kind: "leg.failed", Direction: "originate", LegType: txType,
			CorrelationID: correlationID, Counterpart: recipient, Detail: err.Error(),
		})
		return nil, err
	}
	g.observe(ObserverEvent{
		Kind: "leg.response", Direction: "originate", LegType: txType,
		CorrelationID: correlationID, Counterpart: recipient,
		AuthorityFrame: respFrame, Op: respOp, Payload: json.RawMessage(respPayload),
	})
	g.diagnosticStage(ctx, "leg.response", txType, respPayload, 200, "")
	g.legMetric(LegOutcomeAnswered)
	return respPayload, nil
}

// errFramedDTRUnsupported refuses a framed DTR operation to a recipient that
// has not declared shnsdk.RequestFrameV1Op: such a recipient drops the
// operation header and would misread the body.
var errFramedDTRUnsupported = shnsdk.ErrFramedDTRUnsupported

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
func (g *Gateway) roundTripInner(ctx context.Context, r *http.Request, recipient, reqFrame, respFrame, op, respOp, txType, scope, pci, correlationID, custodian string, content Content) ([]byte, error) {
	_ = scope // policy-derived server-side; kept for contract clarity
	// The request is checked against the leg's ownership row at the boundary
	// itself (content.ProfileID is read below to verify the response frame's
	// stamp).
	reqKey := contentRequestKey(txType, content)
	payload, err := relay.Transmit(content.Payload, relay.Check(reqKey))
	if err != nil {
		g.ownershipRefused(reqKey, err)
		return nil, err
	}
	request := payload // the request as carried, before any frame

	recipientHolder, ok := g.cfg.Reg.Lookup(recipient)
	if !ok {
		return nil, fmt.Errorf("recipient %q not in registry", recipient)
	}

	// Request framing — the REQUEST-line claim. Once payloads genuinely
	// differ per line, "which line is this request built at?" can no longer be
	// re-derived by the receiver: a pended leg resumes on a PINNED line that a
	// fresh recomputation would not reproduce. So the originator states it, in the
	// frame the sealed-envelope machinery already carries, INSIDE the seal (the Hub still
	// sees only ciphertext).
	//
	// Gated exactly like messageFrames: framed IFF the leg is contract-mapped
	// (content.ProfileID non-empty) AND the recipient's registry entry declares
	// requestFrames v1. A peer that never declares it receives BYTE-IDENTICAL bare
	// requests — the additive-in-both-directions fence
	// (TestRequestNotFramedToNonDeclaringPeer).
	//
	// The frame's status field is inert on a request (it exists for answers); 200
	// is the filler the codec requires (100..599) and no receiver reads it.
	//
	// A DTR operation (content.Operation) is always framed, with the operation
	// header, and only to a recipient that declares v1op: a receiver that does
	// not know the header drops it and would misread the body.
	if content.Operation != "" && !shnsdk.SupportsRequestFrameV1Op(recipientHolder.RequestFrames) {
		return nil, errFramedDTRUnsupported
	}
	if content.Operation != "" || (content.ProfileID != "" && shnsdk.SupportsRequestFrameV1(recipientHolder.RequestFrames)) {
		// The body's own media type (a CDS Hooks request is application/json),
		// or the participant's declared FHIR media type when one was carried.
		frameType := content.Payload.ContentType()
		if frameType == "" {
			frameType = mediaFHIRJSON
		}
		if content.MediaType != "" {
			frameType = content.MediaType
		}
		headers := map[string]string{
			"Content-Type":                    frameType,
			shnsdk.FrameHeaderContractVersion: content.ProfileID,
		}
		if content.Operation != "" {
			headers[shnsdk.FrameHeaderOperation] = content.Operation
		}
		framed, ferr := shnsdk.EncodeHTTPFrameHeaders(http.StatusOK, headers, payload)
		if ferr != nil {
			return nil, fmt.Errorf("request frame encode failed")
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
		return nil, fmt.Errorf("seal failed")
	}

	if leg, ok := ctx.Value(diagnosticLegKey{}).(*diagnosticLeg); ok {
		leg.hash = sha256hex(env.Ciphertext)
	}
	exchangeOf(ctx).requestHash(sha256hex(env.Ciphertext))
	// The sealed leg's event names the registered edits the request was
	// sealed with; the record names them once the leg is sent (below).
	edits := content.Payload.Edits()
	g.diagnosticSealed(ctx, env, recipient, correlationID, txType, content.ProfileID, payload, edits)
	tok, err := g.authorize(r, reqFrame, op, pci, correlationID, custodian, sha256hex(env.Ciphertext))
	if err != nil {
		// Preserve a genuine authority DENIAL as the typed sentinel (UC-05's
		// no-consent branch depends on telling it apart from an authz outage); any
		// other authorize failure stays an opaque "authorization failed".
		if errors.Is(err, errAuthorizationDenied) {
			return nil, errAuthorizationDenied
		}
		// Authz never answered: this leg was not sent. The cause rides along
		// for the leg's own records; the caller's answer is the fixed text.
		if errors.Is(err, errAuthzUnreachable) {
			return nil, err
		}
		return nil, fmt.Errorf("authorization failed")
	}
	tokStr, err := tokenJSON(tok)
	if err != nil {
		return nil, fmt.Errorf("token marshal failed")
	}
	env.Metadata.AuthzToken = tokStr
	env.Metadata.ConsentRef = tok.ConsentRef // empty for non-federated exchanges
	// Every other patient the request carries rides in the involved list with
	// its own token, so the Hub records the exchange under each of them too.
	// Recording only, within its own budget: nothing here fails or holds up
	// the leg (involved.go).
	env.Metadata.Involved = g.involvedForRequest(ctx, r, txType, reqFrame, op, correlationID, sha256hex(env.Ciphertext), request, pci)

	body, err := shnsdk.EncodeEnvelope(env)
	if err != nil {
		return nil, fmt.Errorf("encode failed")
	}

	assertion := shnsdk.IssueAssertion(g.cfg.HolderID, "hub", g.cfg.Identity.SignPriv, g.cfg.Clock(), time.Hour)
	assertionJSON, err := json.Marshal(assertion)
	if err != nil {
		return nil, fmt.Errorf("assertion marshal failed")
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
	// The registered edits the request was sent with are recorded once the
	// bytes have left this gateway: the Hub answered (any answer, a refusal or
	// one that could not be read included), or the connection failed after
	// the request was written. A leg refused before the POST, or whose POST
	// failed before it was written, names none (the ingress's own check of
	// the same payload is not a send either).
	var refused *hubRefusalError
	var lost *answerLostError
	var sent *hubSentError
	wasSent := errors.As(err, &sent)
	if err == nil || errors.As(err, &refused) || errors.As(err, &lost) || wasSent {
		exchangeOf(ctx).sent(content.Payload)
	}
	if err == nil {
		exchangeOf(ctx).responseHash(sha256hex(respEnv.Ciphertext))
	}
	if refused != nil {
		return nil, refused
	}
	if lost != nil {
		return nil, lost
	}
	if err != nil {
		err = classifyHubLegError(ctx, legCtx, budget)
		var timedOut *hubTimeoutError
		if errors.As(err, &timedOut) {
			timedOut.sent = wasSent
		}
		if errors.Is(err, errHubTimeout) {
			log.Printf("gateway: hub leg %s to %q timed out: %s (correlation %s)", txType, recipient, err.Error(), correlationID)
			return nil, err
		}
		if wasSent {
			return nil, &hubRefusalError{status: http.StatusBadGateway, reason: "the connection to the Hub failed after the request was sent", delivered: "unknown"}
		}
		return nil, err
	}

	// C1/H2b: the response leg must be authorized just like the request leg, bound
	// to the ORIGINAL request correlationID (not the response envelope's own CID),
	// and must come from the expected counterpart holder.
	var respTok shnsdk.Token
	if err := json.Unmarshal([]byte(respEnv.Metadata.AuthzToken), &respTok); err != nil {
		return nil, &answerLostError{cause: "response leg authorization failed"}
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
		return nil, &answerLostError{cause: "response leg authorization failed"}
	}
	if respEnv.Metadata.CorrelationID != correlationID {
		return nil, &answerLostError{cause: "response correlation mismatch"}
	}
	if respEnv.Metadata.Sender != recipient {
		return nil, &answerLostError{cause: "response sender mismatch"}
	}

	respPayload, err := shnsdk.Open(respEnv, g.cfg.Identity.EncPub, g.cfg.Identity.EncPriv)
	if err != nil {
		return nil, &answerLostError{cause: "response decryption failed"}
	}
	// A frame-negotiated recipient (registry messageFrames) seals
	// EVERY application answer — any status — as a v1 message frame; surface non-2xx
	// as the typed *RelayError sentinel so every OriginateLeg caller's `if err != nil`
	// aborts the exchange and handlers can relay the verbatim answer.
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
			return nil, &answerLostError{cause: "response frame decode failed"}
		}
		if hdr.Status/100 != 2 {
			return nil, &RelayError{Status: hdr.Status, Body: body, ContentType: hdr.Headers["Content-Type"], leg: txType}
		}
		// A 204 answer has no body to write, so a frame stating 204 with
		// one says two things and is not read.
		if hdr.Status == http.StatusNoContent && len(body) > 0 {
			return nil, &answerLostError{cause: "response frame states 204 No Content and carries a body"}
		}
		// Version-stamp verification: a 2xx framed answer
		// declaring a DIFFERENT contract line than this leg routed is rejected
		// before the body reaches any parser or validator — tamper or skew,
		// either way not the payload we negotiated. Absent stamp = pre-version
		// responder (tolerated, the frames-absent precedent); version-neutral
		// legs (empty ProfileID) ignore stamps. Non-2xx frames return above as
		// *RelayError — relayed verbatim, never parsed as contract content.
		// The answer came after the recipient acted, so the refusal says so,
		// as every refusal of an answer does: it is never a failure the caller
		// might blindly resend.
		// By default an answer stamped with another line is relayed and the
		// difference recorded, as the two participants would see it directly;
		// the participant's own setting (RequireSharedLine) refuses it as
		// routing.
		if expected := content.ProfileID; expected != "" {
			if stamped := hdr.Headers[shnsdk.FrameHeaderContractVersion]; stamped != "" && stamped != expected {
				if g.cfg.RequireSharedLine {
					return nil, &answerLostError{cause: fmt.Sprintf("response contract version mismatch: frame declares %s, leg routed %s", stamped, expected), rule: RefusalRouting}
				}
				g.noteLineCarried(ctx, LineCarriedAnswer, ObserverEvent{
					Direction: "originate", LegType: txType, CorrelationID: correlationID, Counterpart: recipient,
					Detail: "answer stamped " + boundedLine(stamped) + "; leg routed " + expected,
				})
			}
		}
		// The answer's stated media type and status, for a caller that
		// relays it on.
		noteAnswerMediaType(ctx, hdr.Headers["Content-Type"])
		noteAnswerStatus(ctx, hdr.Status)
		return body, nil
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
	return respPayload, nil
}

// contentRequestKey is the transmit content's request on leg is checked
// against: carried when the participant's system sent it as its own message;
// originated when it is the gateway's own, built by its workflow or by a
// registered module ingress client (OnBehalf), even when it arrived at the
// ingress.
func contentRequestKey(leg string, content Content) relay.Key {
	return requestKey(leg, content.Carried && !content.OnBehalf)
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
// from the catalog of the workstream content is tagged with (catalogFor; the PA
// Layer-3 module's paCatalog today) instead of taking them as positional
// literals. recipient stays a parameter (payer legs derive it from the patient's Coverage
// via recipientFor — no default; facility/phg legs target a LookupByRole result). An
// unknown legType is a caller bug (the Originator
// only passes catalog legTypes) and fails closed with an error.
//
// The content.WorkstreamType lookup is the SELECTION SEAM: content tagged with a
// workstream this build does not compile in is refused before anything is sent. The
// catalog is single-source-of-truth across both edges (origination and the
// handleInbound FulfillLeg dispatch, which resolves the same catalogs by transaction
// type through legSpecFor, hence "cannot drift"). This is the origination MIRROR of the
// payer-side FulfillLeg pattern.
func (g *Gateway) OriginateLeg(ctx context.Context, r *http.Request, recipient, legType, pci, correlationID, custodian string, content Content) ([]byte, error) {
	catalog, served := catalogFor(content.WorkstreamType)
	if !served {
		return nil, fmt.Errorf("OriginateLeg: content workstream %q not served by this gateway", content.WorkstreamType)
	}
	spec, ok := catalog[legType]
	if !ok {
		return nil, fmt.Errorf("OriginateLeg: unknown legType %q", legType)
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
	// select-before-build primitive, lineselect.go) is where the reachability
	// arms live; this legacy/neutral-caller path never routes through it.
	if content.ProfileID == "" {
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
			return nil, err
		}
		content.ProfileID = tok
		// content.Route stays nil here by construction: selectLegToken is arm-1-
		// only and nothing was SELECTED for this leg in the routeInfoFor sense
		// (no BuildLine/Chain decision was made, just an intersection token) —
		// the caller already built its payload at some line before reaching
		// OriginateLeg, so there is no route story to synthesize after the fact.
		// roundTrip's leg.originated therefore carries Route: nil on this path.
	}
	if content.Route != nil && content.Route.Pinned != "" {
		g.notePinCarried(ctx, recipient, legType, correlationID, content.Route.Pinned, content.ProfileID)
	} else {
		g.noteRequestLine(ctx, recipient, legType, correlationID, content.ProfileID)
	}
	if target, ok := ctx.Value(certificationTargetKey{}).(*string); ok {
		*target = content.ProfileID
	}
	answer, err := g.roundTrip(ctx, r, recipient, spec.ReqFrame, spec.RespFrame, spec.Op, spec.RespOp, legType, spec.Scope, pci, correlationID, custodian, content)
	check := originatedAnswerCheckFor(legType)
	if err != nil || content.Carried || check == nil {
		return answer, err
	}
	// An answer to a leg this gateway originates is certified by the check its
	// workstream declares (originatedAnswerCheck), at the routed line and this
	// gateway's own level, before anything reads it. A carried leg (an ingress)
	// certifies the answer itself, so it is not checked twice.
	actx := withFindingContext(ctx, findingContext{LegType: legType, CorrelationID: correlationID, Seam: "originate", Whose: "peer"})
	if refused := check(actx, g.conformance(), g.emitFinding, answer, shnsdk.LineOf(content.ProfileID), "peer"); refused.Status != 0 {
		return nil, errors.New(refused.Message)
	}
	return answer, nil
}

// validatorForLine is the validator for a check that names no contract
// line (line "": a version-neutral leg or a non-contract resource): the
// deployment's canonical Validator. A check that names a line resolves its
// lane by contract (validatorForContractLine); a line without its contract
// has no lane, and fails closed.
func (g *Gateway) validatorForLine(line string) shnsdk.Validator {
	if line == "" {
		return g.cfg.Validator
	}
	return nil
}

// validateFHIR runs the line's configured validator over a FHIR resource on the
// given leg, returning the gateway-standard (status,message) on failure. dir is
// "egress" or "ingress" purely for the error message; status is 0 on success.
// line is the contract line the resource was BUILT at ("" = no line in play);
// an unlaned line fails closed with a 500 naming it (FR-36/FR-G29 — never a
// silent fallback to the canonical lane).
//
// This function NEVER skips — every call always validates. The one ingress skip
// lives ONLY in validateFHIRPayerIngress below, keyed on the leg's counterparty
// being one of the network's reference payers on a lane that relays their bytes. A
// skip keyed on the lane alone once exempted every ingress check on the demo lane,
// including handleUC05's facility federated-query searchset, which is not a payer's
// answer at all; keeping the skip in its own function makes the set of skip-eligible
// legs a grep (call sites of validateFHIRPayerIngress), never something a new ingress
// site inherits by accident.
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

// govResult is what one governed check produced: the refusal (zero Status when
// the message proceeds) and, for a refusal only, the bounded validator issues
// the body may echo. Issues is empty at every other outcome.
type govResult struct {
	Status int
	Msg    string
	Issues []string
	// NoLane marks a refusal because no validator lane serves the line, so a
	// call site with its own missing-lane status and text keeps them.
	NoLane bool
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

// ConformanceLevelForTest exposes this Gateway's own effective conformance
// enforcement level — test-only introspection (the ValidatorReadinessForTest/
// RecordEdgeCaptureForTest pattern) proving that a level set through
// Config.ConformanceEnforcement actually reached the constructed Gateway,
// not just whatever struct a caller assembled.
func (g *Gateway) ConformanceLevelForTest() ConformanceEnforcement {
	return g.conformance().Level()
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

// kindForDirection names the governed check kind of a participant's payload.
// A bridged payload, one this gateway transformed between IG lines, is
// KindFHIRBridged, named by judgeLines: SHN's own registered edit, never the
// participant's data, decided by module 2's bridgedDecision (§2).
func kindForDirection(dir string) CheckKind {
	if dir == "egress" {
		return KindFHIREgress
	}
	return KindFHIRIngress
}

// lineLane is one IG line a governed check certifies against, and the validator
// lane that serves it (nil when no lane does).
type lineLane struct {
	Line string
	V    shnsdk.Validator
	// Decisive marks the routed line, or a line the payload itself claims: when
	// no line is valid and a decisive line could not be judged, the check is
	// unavailable rather than decided on what the other lines said.
	Decisive bool
	// Extra marks a candidate beyond the routed line; its call is bounded by
	// payerAnswerCandidateTimeout.
	Extra bool
}

// payerAnswerCandidateTimeout bounds each extra candidate line's validator call
// (the routed line keeps the validator client's own timeout): a hung lane on a line
// the answer was not routed at is recorded as unavailable for that line rather than
// holding the answer. It is the certification layer's per-candidate bound; a test
// may shorten it.
var payerAnswerCandidateTimeout = certificationCandidateTimeout

// validateGoverned is the governed check of one resource at one line: the whole
// of validateGovernedLines with a single lane. At none the check does not run: no
// validator call, no finding. Every invalid verdict emits its finding first, at
// every level that runs the check, and only then is the decision acted on. A check
// that could not run (validator outage, no lane for the line) refuses at strict and
// for a bridged payload, with no finding; at observe and structural it is recorded
// as unavailable and the message is relayed. An invalid verdict is classified
// (classifyFHIR) before it is decided: at structural a structural or unclassified
// defect refuses and a deeper one is recorded; a validator that answered without
// reading the payload is unavailable at every level.
func (g *Gateway) validateGoverned(ctx context.Context, fc findingContext, v shnsdk.Validator, resourceJSON []byte, dir, line, profile string, bridged bool) govResult {
	defer exchangeOf(ctx).validation()()
	return g.validateGovernedLines(ctx, fc, []lineLane{{Line: line, V: v}}, resourceJSON, dir, line, profile, bridged, false)
}

// validateGovernedLines is the ONE place a runtime FHIR $validate verdict becomes
// a refusal or a record. lanes are tried in order until one answers valid; with
// one lane this is exactly validateGoverned's contract above. line is the line the
// leg was routed at, named by an unavailable finding and a missing-lane refusal.
//
// candidates marks a candidate-line certification (validateFHIRPayerIngress): the
// finding names line as the routed line (DeclaredLine) and lists every tried line's
// verdict (Lines), and a valid answer on a line after the first still emits one
// finding saying so. The verdict decided is the best any line gave — valid,
// then deeper, then structural — and unavailable when no line's validator
// answered or, with no line valid, when a Decisive lane could not be judged. The
// finding's Line names the line that verdict came from.
func (g *Gateway) validateGovernedLines(ctx context.Context, fc findingContext, lanes []lineLane, resourceJSON []byte, dir, line, profile string, bridged, candidates bool) govResult {
	defer exchangeOf(ctx).validation()()
	// A bridged payload is SHN's own edit, module 2's, refused at every level
	// ahead of module 3. A participant's payload is module 3's check.
	if bridged {
		return g.judgeLines(ctx, g.conformance(), fc, lanes, resourceJSON, dir, line, profile, bridged, candidates)
	}
	return g.conformance().fhir(ctx, fc, lanes, resourceJSON, dir, line, profile, candidates)
}

// fhir is module 3's governed check of a participant's payload. At observe the
// check can only record, so the leg does not wait for it: it is queued and its
// finding emitted when the validator answers (observe_async.go). At every
// other level it is judged now.
func (m gatewayConformance) fhir(ctx context.Context, fc findingContext, lanes []lineLane, resourceJSON []byte, dir, line, profile string, candidates bool) govResult {
	if m.Level() == EnforcementObserve {
		m.g.enqueueObserveCheck(observeCheck{fc: fc, lanes: lanes, payload: resourceJSON, dir: dir, line: line, profile: profile, candidates: candidates,
			binding: findingBindingFrom(ctx), tally: legTallyFrom(ctx)})
		return govResult{}
	}
	return m.g.judgeLines(ctx, m, fc, lanes, resourceJSON, dir, line, profile, false, candidates)
}

// judgeLines runs a governed check and acts on its verdict: on the request path
// for every level that can refuse, and on an observe worker for a check that can
// only record (observe_async.go). mod is module 3, which decides a
// participant's payload and names the level a finding records; a bridged
// check is decided by module 2's bridgedDecision.
func (g *Gateway) judgeLines(ctx context.Context, mod conformanceModule, fc findingContext, lanes []lineLane, resourceJSON []byte, dir, line, profile string, bridged, candidates bool) govResult {
	kind := kindForDirection(dir)
	whose := fc.Whose
	switch {
	case bridged:
		kind, whose = KindFHIRBridged, "network"
	case !mod.Runs(kind, ""):
		return govResult{}
	}
	decide := func(v Verdict) Decision { return mod.Decide(kind, "", v) }
	if bridged {
		decide = bridgedDecision
	}
	var tried []LineVerdictSummary
	// spent is the time this check's validator calls took, the lanes tried
	// together.
	var spent time.Duration
	declaredLine := ""
	if candidates {
		declaredLine = line
	}
	note := func(l string, v Verdict, took time.Duration) {
		if candidates {
			tried = append(tried, LineVerdictSummary{Line: l, Verdict: findingVerdict(v), Ms: took.Milliseconds()})
		}
	}
	unavailable := func(refused govResult, at string) govResult {
		if decide(VerdictUnavailable) == Refuse {
			return refused
		}
		g.emitFindingIn(ctx, ConformanceFinding{
			Kind: string(kind), Direction: dir, LegType: fc.LegType, CorrelationID: fc.CorrelationID,
			Seam: fc.Seam, Whose: whose, Line: at, Profile: profile,
			Level: mod.Level().String(), Verdict: "unavailable", Decision: Record.String(),
			PayloadSHA256: sha256hex(resourceJSON), DeclaredLine: declaredLine, Lines: tried,
			ValidatorMs: spent.Milliseconds(),
		})
		return govResult{}
	}
	var (
		best             shnsdk.Result
		bestLine         string
		bestVerdict      Verdict
		answered         bool
		laned            bool
		noLaneLine       string
		decisiveUnjudged bool
	)
	for _, lane := range lanes {
		if lane.V == nil {
			note(lane.Line, VerdictUnavailable, 0)
			if lane.Decisive {
				decisiveUnjudged = true
				if noLaneLine == "" {
					noLaneLine = lane.Line
				}
			}
			continue
		}
		laned = true
		callCtx, cancel := ctx, context.CancelFunc(func() {})
		if lane.Extra {
			callCtx, cancel = context.WithTimeout(ctx, payerAnswerCandidateTimeout)
		}
		callStart := g.exchangeClock()
		req, reqProfile := validateRequest(resourceJSON, profile)
		res, err := lane.V.Validate(callCtx, req, reqProfile)
		took := g.exchangeClock().Sub(callStart)
		spent += took
		cancel()
		if err != nil {
			note(lane.Line, VerdictUnavailable, took)
			decisiveUnjudged = decisiveUnjudged || lane.Decisive
			continue
		}
		if res.Valid {
			if len(tried) > 0 {
				note(lane.Line, VerdictValid, took)
				g.emitFindingIn(ctx, ConformanceFinding{
					Kind: string(kind), Direction: dir, LegType: fc.LegType, CorrelationID: fc.CorrelationID,
					Seam: fc.Seam, Whose: whose, Line: lane.Line, Profile: profile,
					Level: mod.Level().String(), Verdict: findingVerdict(VerdictValid), Decision: Record.String(),
					PayloadSHA256: sha256hex(resourceJSON), DeclaredLine: declaredLine, Lines: tried,
					ValidatorMs: spent.Milliseconds(),
				})
			}
			return govResult{}
		}
		verdict := classifyFHIR(res, resourceJSON, lane.Line, profile)
		note(lane.Line, verdict, took)
		if verdict == VerdictUnavailable {
			decisiveUnjudged = decisiveUnjudged || lane.Decisive
			continue
		}
		if !answered || verdict == VerdictDeeper && bestVerdict != VerdictDeeper {
			best, bestLine, bestVerdict, answered = res, lane.Line, verdict, true
		}
	}
	// No line was valid. If the routed line, or a line the answer itself claims,
	// could not be judged, what the other lines said does not decide it: a line the
	// answer is not at reports its own profiles' failures, which says nothing about
	// the line the answer is at. The check is then unavailable.
	if !answered || decisiveUnjudged {
		switch {
		case noLaneLine != "":
			return unavailable(govResult{Status: http.StatusInternalServerError, Msg: "no FHIR validator lane configured for contract line " + noLaneLine + " (FR-36/FR-G29)", NoLane: true}, noLaneLine)
		case !laned:
			return unavailable(govResult{Status: http.StatusInternalServerError, Msg: "no FHIR validator lane configured for contract line " + line + " (FR-36/FR-G29)", NoLane: true}, line)
		}
		return unavailable(govResult{Status: http.StatusInternalServerError, Msg: "validator unavailable"}, line)
	}
	decision := decide(bestVerdict)
	g.emitFindingIn(ctx, ConformanceFinding{
		Kind:          string(kind),
		Direction:     dir,
		LegType:       fc.LegType,
		CorrelationID: fc.CorrelationID,
		Seam:          fc.Seam,
		Whose:         whose,
		Line:          bestLine,
		Profile:       profile,
		Level:         mod.Level().String(),
		Decision:      decision.String(),
		PayloadSHA256: sha256hex(resourceJSON),
		Issues:        best.Issues,
		DeclaredLine:  declaredLine,
		Lines:         tried,
		ValidatorMs:   spent.Milliseconds(),
	})
	if decision == Record {
		return govResult{}
	}
	return govResult{Status: http.StatusUnprocessableEntity, Msg: dir + " validation failed", Issues: boundIssues(best.Issues)}
}

// validateFHIRPayerIngress is the ingress check of a payer's answer on the legs a
// provider gateway originates to a payer: the DTR $questionnaire-package and
// $next-question answers and the PAS ClaimResponse answers (dtr-questionnaire-fetch,
// pas-claim, pas-claim-update). line is the line the leg was routed at — the payer's
// declared line, or this build's own line for a payer that declared none — and payer
// is the payer identity the member's Coverage named, which the leg was routed by.
//
// Reference payers. The check is skipped only when BOTH halves hold: this lane relays
// reference-payer bytes (relaysReferencePayerBytes) AND payer is one of the network's
// reference payer identities (isReferencePayer). Those answers are the reference
// implementation's own bytes, relayed verbatim, and do not yet conform (the 2.0
// reference payer's DTR package fails DTR 2.0.1), so certifying them would refuse the
// network's own reference traffic. Every other payer, on any lane, is governed at this
// gateway's level: none runs nothing, observe records, structural refuses a structural
// defect, strict refuses every defect and a check that could not run.
//
// Candidate lines. A payer's declaration may be this gateway's default rather than the
// payer's own claim, and the registry cannot tell the two apart, so an answer is not
// judged only at the routed line. It is certified against the lines this build
// supports, in order: the routed line first (the line the request was built for), then
// the lines the answer itself claims (versioned meta.profile, then structural
// markers), then 2.2, 2.1 and 2.0 — each line once, only where this build has a
// validator lane for it, and each validator once (where one validator serves every
// line the answer is judged once, at the routed line). The first valid line ends the
// check (valid on any line is valid). Otherwise, if the routed line or a line the
// answer claims could not be judged (its validator was unavailable, or no lane serves
// it), the verdict is unavailable: another line's profiles failing says nothing about
// the line the answer is at. Otherwise the verdict is the best one any line gave
// (deeper beats structural), and unavailable if no line's validator answered. The
// policy then decides as for one line. A further line is tried only after the
// previous one failed, each extra line's call is bounded by
// payerAnswerCandidateTimeout, and the one finding names the routed line and each
// tried line's verdict; it can record an answer valid only on a line other than its
// routed one, which is not a defect. Judging a payer's confirmed line alone, more
// strictly, would be a separate rule.
//
// Every OTHER ingress leg — federated-query (UC-05, the facility), patient-dtr (UC-07,
// the PHG), and any inbound leg the PAYER role itself validates from a provider — calls
// plain validateFHIR, which always validates at the routed line. Do not add a call
// site here unless the leg's counterparty is a payer answering a leg this gateway
// originated.
func (g *Gateway) validateFHIRPayerIngress(ctx context.Context, resourceJSON []byte, line, contract string, payer shnsdk.PayerIdentifier) (int, string) {
	if relaysReferencePayerBytes(g.cfg.OriginationProfile) && isReferencePayer(payer) {
		return 0, ""
	}
	return g.validatePayerAnswerAtCandidates(ctx, resourceJSON, line, contract)
}

// validatePayerAnswerAtCandidates certifies a payer's answer against the candidate
// lines, as validateFHIRPayerIngress describes, for every payer: it has no
// reference-payer skip. The provider ingress calls it directly for the answers it
// carries for its own participant's system (carriedAnswerRefused), a path that never
// had the skip; the tolerance it would extend is to leave the core, as a participant's
// opt-in.
func (g *Gateway) validatePayerAnswerAtCandidates(ctx context.Context, resourceJSON []byte, line, contract string) (int, string) {
	lanes := g.candidateLanes(contract, line, resourceJSON)
	if !anyLaned(lanes) {
		// No lane serves any line: the routed line alone, which reports the missing
		// lane as the check being unavailable.
		return g.validateFHIRForContract(ctx, resourceJSON, "ingress", contract, line, "")
	}
	return g.validateGovernedLines(ctx, findingContextFrom(ctx), lanes, resourceJSON, "ingress", line, "", false, true).refusal()
}

// candidateLanes is the ordered set of lines a payer's answer is certified against:
// routed first, then the contract's workstream's order over the answer (answerLineClaims), each of the
// contract's lines once. A line with no validator lane is left out, unless it is
// decisive (routed, or claimed by the answer), when it stays in with no validator so
// that it is recorded as unavailable. A validator that already serves an earlier line
// is not called again for a later one: where one validator serves every line, the
// answer is judged once, at the routed line.
func (g *Gateway) candidateLanes(contract, routed string, payload []byte) []lineLane {
	var used []shnsdk.Validator
	var out []lineLane
	for _, c := range g.answerCandidates(contract, routed, payload) {
		switch {
		case c.V != nil && servesAlready(used, c.V):
			continue
		case c.V != nil:
			used = append(used, c.V)
		}
		out = append(out, c)
	}
	return out
}

// answerCandidates is the order and set of lines a payer's answer is judged at
// (FR-G29), one list for every check that reads an answer by its line: the
// routed line first, then the contract's workstream's order over the answer (answerLineClaims), each of
// the contract's lines once, a line with no validator lane left out unless it
// is decisive (routed, or claimed by the answer). candidateLanes certifies by
// it; answer.shape reads the answer's graph by it (answerGraphLine).
func (g *Gateway) answerCandidates(contract, routed string, payload []byte) []lineLane {
	native := map[string]bool{}
	for _, l := range nativeLinesForContract(contract) {
		native[l] = true
	}
	order, claimed := answerLineClaims(contract, payload)
	seen := map[string]bool{}
	var out []lineLane
	for _, l := range append([]string{routed}, order...) {
		if !native[l] || seen[l] {
			continue
		}
		seen[l] = true
		decisive := l == routed || claimed[l]
		v := g.validatorForContractLine(contract, l)
		if v == nil && !decisive {
			continue
		}
		out = append(out, lineLane{Line: l, V: v, Decisive: decisive, Extra: l != routed})
	}
	return out
}

// answerCandidateLines is the lines an answer can be valid at, in order: the
// lines certification judges it at (candidateLanes) that have a validator,
// and the routed line. A line the answer claims with no lane is one
// certification can only record as unavailable, and a line whose validator
// already served an earlier one it does not judge again, so no other rule
// reads the answer valid there either.
func (g *Gateway) answerCandidateLines(contract, routed string, payload []byte) []string {
	var lines []string
	for _, c := range g.candidateLanes(contract, routed, payload) {
		if c.V != nil || c.Line == routed {
			lines = append(lines, c.Line)
		}
	}
	return lines
}

// anyLaned reports whether any lane in lanes has a validator.
func anyLaned(lanes []lineLane) bool {
	for _, l := range lanes {
		if l.V != nil {
			return true
		}
	}
	return false
}

// servesAlready reports whether v is one of used: the same validator object. A
// validator whose type cannot be compared is taken to be distinct.
func servesAlready(used []shnsdk.Validator, v shnsdk.Validator) (same bool) {
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	for _, u := range used {
		if u == v {
			return true
		}
	}
	return false
}

// egressAdapt applies route's transform chain (if any) to payload before it
// is sent, builds the transform Provenance from the
// chain's LossReports, and emits leg.transformed. Arms (1)/(2) carry
// route.Chain == nil, so this is a pure pass-through for the entire
// production mesh today (arm (3), the only path that reaches applyChain, is
// production-dormant within {2.0,2.1,2.2} per the recorded route-selection
// consequence — every
// published line is native). Its caller, egressBuild, certifies the
// returned bytes at the TARGET lane before sealing (module 2's Certify when
// the route has a chain) — transformed-output-invalid is a hard failure
// THERE, never silently swallowed here.
func (g *Gateway) egressAdapt(route legRoute, payload []byte, x ExchangeIdentity) ([]byte, []LossReport, error) {
	out, reports, record, err := g.egressWalk(route, payload, x)
	if err != nil {
		return nil, nil, err
	}
	if err := record(); err != nil {
		return nil, nil, err
	}
	return out, reports, nil
}

// egressWalk is egressAdapt without its record: it walks payload along
// route's chain and returns record, which emits leg.transformed and the
// transform Provenance for that walk. A caller that may not apply the walk
// (a request carried as sent when the walk would change it) calls record only
// when it does, so no transform is recorded for bytes never sent. record is
// never nil. A route with no chain (arms (1) and (2), and every carry) is a
// pass-through; a chain is module 2's, walked by its AdaptRequest.
func (g *Gateway) egressWalk(route legRoute, payload []byte, x ExchangeIdentity) ([]byte, []LossReport, func() error, error) {
	noRecord := func() error { return nil }
	if len(route.Chain) == 0 {
		return payload, nil, noRecord, nil
	}
	b := g.bridge()
	if b == nil {
		return nil, nil, noRecord, errBridgeOff
	}
	return b.AdaptRequest(route, payload, x)
}

// adaptAnswer is the payload this gateway gives its participant's system for
// answer, the answer on key to a leg routed at routed: module 2's
// (AdaptAnswer) when it is on, else the answer exactly as it arrived, written
// in the line it was routed at. wrote, status and msg are AdaptAnswer's.
func (g *Gateway) adaptAnswer(ctx context.Context, key relay.Key, answer []byte, contentType, routed string, x ExchangeIdentity) (relay.Payload, string, int, string) {
	b := g.bridge()
	if b == nil {
		return relay.Exact(relay.NewBody(answer, relay.OriginPeerFrame), contentType), routed, 0, ""
	}
	return b.AdaptAnswer(ctx, key, answer, contentType, routed, g.declaredContractVersions(), x)
}

// recordTransformed records a transform chain a gateway ran over a payload
// (ev, the leg.transformed event, and the chain's reports): the transform
// Provenance (module ids, source and target lines, the loss report), checked
// to round-trip to the same reports, on the observer stream, and, when the
// demonstration capture is on, the before/after pair. The edges that run a
// chain share it: module 2's request walk (AdaptRequest, through egressWalk),
// and the provider's carry of a questionnaire package answer across DTR lines
// (carryDTRAnswer).
func (g *Gateway) recordTransformed(ev ObserverEvent, x ExchangeIdentity, contract, fromLine, targetLine string, chain []CompatStep, reports []LossReport, before, after []byte) error {
	// The transform Provenance (module id, source->target lines,
	// the shn-loss-report extension) rides INSIDE the transformed payload
	// where the target profile tolerates the extra Bundle.entry; else it
	// rides the observer stream only — never the envelope, never Hub-visible.
	sdkLoss := toSDKLossReports(reports)
	provJSON, perr := shnsdk.BuildTransformProvenance(
		"urn:shn:leg:"+x.CorrelationID, "Organization/"+g.cfg.HolderID,
		ev.Detail, fromLine, targetLine, sdkLoss, g.cfg.Clock(),
	)
	if perr != nil {
		return fmt.Errorf("engine: %s: build transform Provenance: %w", ev.Direction, perr)
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
		return fmt.Errorf("engine: %s: %w", ev.Direction, verr)
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
			From:          fromLine,
			To:            targetLine,
			Chain:         chainStepsFrom(fromLine, chain),
			LossReports:   reports,
			Before:        before,
			After:         after,
			CapturedAt:    g.cfg.Clock(),
		})
	}
	return nil
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

// msgUnknownLeg is the refusal of a request on a leg this gateway's catalog
// does not define. handleInbound refuses a transaction type no leg serves
// before it reaches unframeRequest, so this is defence in depth; should it
// ever be written, it names no internal detail.
const msgUnknownLeg = "transaction type is not defined by this gateway"

// readRequestFrame reads a request's frame: the unframed body, the line the
// frame claims, and, when this gateway cannot answer at that line, why
// (unanswerable). status is non-zero for a leg this gateway does not define
// or a frame it cannot decode.
func (g *Gateway) readRequestFrame(legType string, payload []byte) (body []byte, claimed, unanswerable string, status int, msg string) {
	contract, err := legContract(legType)
	if err != nil {
		return nil, "", "", http.StatusInternalServerError, msgUnknownLeg
	}
	if !shnsdk.IsFramed(payload) {
		return payload, "", "", 0, "" // bare: the caller recomputes (unframeRequestFrom)
	}
	hdr, body, ferr := shnsdk.DecodeHTTPFrame(payload)
	if ferr != nil {
		return nil, "", "", http.StatusBadRequest, "request frame decode failed"
	}
	claimed = hdr.Headers[shnsdk.FrameHeaderContractVersion]
	if claimed == "" || contract == "" {
		// A framed request with no version claim is the frames-without-versions
		// case, treated exactly like a bare request; a version-neutral leg has
		// no line, so a claim on one is ignored.
		return body, "", "", 0, ""
	}
	if !nativeContractToken(claimed) || !strings.HasPrefix(claimed, contract+"@") {
		return body, claimed, "request declares contract version " + claimed + ", which this gateway cannot build for leg " + legType +
			" (it speaks " + strings.Join(sortedTokens(contract, contractLineSet(NativeContractVersions(), contract)), ",") + ")", 0, ""
	}
	line := shnsdk.LineOf(claimed)
	if !g.servesLine(contract, line) {
		return body, claimed, "request declares contract version " + claimed + " but this gateway has no FHIR validator lane for line " + line +
			" — refusing to answer at an unvalidatable line", 0, ""
	}
	return body, claimed, "", 0, ""
}

// unframeRequestFrom implements the request-framing RECEIVER rule for one
// inbound leg.
//
// A framed request carries the line the ORIGINATOR built its payload at. This
// build answers at that line iff the token is BOTH:
//   - native-buildable (a member of NativeContractVersions() for THIS leg's
//     contract) — we can actually produce the answer at that line; and
//   - served (servesLine): laned, so we can actually VALIDATE at that line, or,
//     at none, which validates nothing, a line this gateway declares.
//
// The predicate is native∩laned rather than the routing rule's "declared" — a
// RECORDED deviation: it is what makes the declaration-change window benign,
// because legs routed off a peer's stale (smaller) view of our declaration
// still complete.
//
// A claim it cannot answer at is the two participants' IG lines differing, and
// by default it is carried to the participant: answered at the line a bare
// request gets (below), with carried naming the claim so the caller records
// the carry. Only the participant's own setting (RequireSharedLine) refuses it,
// with a legible 422 naming what is missing. A claim on a version-neutral leg
// is ignored, as an answer's stamp is.
//
// When the sender sent no version claim, the answer line is the SYMMETRIC RECOMPUTATION of
// the originator's own selection — the sender's registry-declared set × this
// build's declared set, highest common line, with a silent sender falling back to
// this build's own canonical line (selectContractToken's rules, verbatim). That
// keeps a pre-framing or version-neutral sender answered exactly as before.
//
// A recomputation that REFUSES is not an error here: the originator already made
// the routing decision, and refusing an in-flight leg on the responder side would
// break the D1a mismatch window. It degrades to this build's own canonical line,
// which is the pre-framing answer.
func (g *Gateway) unframeRequestFrom(sender, legType string, payload []byte) ([]byte, string, string, int, string) {
	body, claimed, unanswerable, status, msg := g.readRequestFrame(legType, payload)
	if status != 0 {
		return nil, "", "", status, msg
	}
	carried := ""
	switch {
	case unanswerable == "" && claimed != "":
		return body, claimed, "", 0, ""
	case unanswerable != "" && g.cfg.RequireSharedLine:
		return nil, "", "", http.StatusUnprocessableEntity, unanswerable
	case unanswerable != "":
		carried = claimed
	}
	contract, err := legContract(legType)
	if err != nil {
		return nil, "", "", http.StatusInternalServerError, msgUnknownLeg
	}
	if contract == "" {
		return body, "", "", 0, "" // version-neutral leg: no answer line, never stamped
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
			return body, "", carried, 0, ""
		}
		return body, contract + "@" + highestLine(ownLines), carried, 0, ""
	}
	return body, tok, carried, 0, ""
}

// nativeContractToken reports whether tok is a contract-version token THIS build
// can natively build (the "native" half of the receiver rule).
func nativeContractToken(tok string) bool {
	for _, n := range NativeContractVersions() {
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
// frame. The header is defined only for a leg whose taxonomy declares frame
// operations (legSpec.OperationHeader): a frame that names an operation on any
// other leg is refused (400). Which operation values are served is the leg's
// decision. A payload that is not a frame names none.
func inboundFrameOperation(legType string, payload []byte) (string, int, string) {
	if !shnsdk.IsFramed(payload) {
		return "", 0, ""
	}
	hdr, _, err := shnsdk.DecodeHTTPFrame(payload)
	if err != nil {
		return "", http.StatusBadRequest, "request frame decode failed"
	}
	op := hdr.Headers[shnsdk.FrameHeaderOperation]
	if spec, _ := legSpecFor(legType); op != "" && !spec.OperationHeader {
		return "", http.StatusBadRequest, "operation header is not defined for this transaction type"
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

// withAnswerCandidateLines tags ctx with the lines a payer's answer on this leg
// is judged at (answerCandidateLines, for the answer line's contract), so the
// payer's content seam reads an answer's graph by the same lines its gateway's
// certification does. Set beside the answer line, in handleInbound.
func withAnswerCandidateLines(ctx context.Context, lines func(payload []byte) []string) context.Context {
	return context.WithValue(ctx, answerCandidateLinesKey{}, lines)
}

type answerCandidateLinesKey struct{}

// answerCandidateLinesFrom is the lines ctx says an answer is judged at, or,
// outside the inbound dispatch, the answer line alone (none when unresolved).
func answerCandidateLinesFrom(ctx context.Context, payload []byte) []string {
	if lines, ok := ctx.Value(answerCandidateLinesKey{}).(func([]byte) []string); ok && lines != nil {
		return lines(payload)
	}
	if l := answerLineFrom(ctx); l != "" {
		return []string{l}
	}
	return nil
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
		declared = DefaultContractVersions()
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
// it (UC-05 facility responses pass the backstop-authenticated ref), which
// equals the request's. The Hub records the request token's signed reference on
// both legs of the exchange, not this value. Exchanges with no consent (payer
// responses) pass "".
//
// Direction symmetry: requester is the inbound envelope's Sender — whoever
// initiated this leg — rather than a hardcoded holder id. This means a future
// payer-originated push needs no retrofit: buildResponseLeg already replies to
// whoever sent the inbound envelope.
func (g *Gateway) buildResponseLeg(r *http.Request, respFrame, respOp, txType, inboundCorrID string, p relay.Payload, k relay.Key, frame func([]byte) ([]byte, error), subjectPCI, requester, consentRef string) (out []byte, status int, msg string) {
	defer exchangeOf(r.Context()).stage(func(s *LegStages, d time.Duration) { s.Seal += d })()
	payload, err := relay.Transmit(p, relay.Check(k))
	if err != nil {
		g.ownershipRefused(k, err)
		exchangeOf(r.Context()).refusing(RefusalFidelity)
		return nil, http.StatusInternalServerError, errOwnershipFault
	}
	if frame != nil {
		if payload, err = frame(payload); err != nil {
			return nil, http.StatusBadGateway, "invalid application status"
		}
	}
	requesterHolder, ok := g.cfg.Reg.Lookup(requester)
	if !ok {
		// The inbound route resolves the requester before a leg is opened
		// (requester.go), so a miss here is the registry dropping it mid-leg:
		// this gateway's fault, not a requester not known yet.
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
		// The answer was decided and cannot be sent: say why, under the leg's
		// id, without the error's text (the Hub's answer is unchanged).
		class := answerErrorClass(r.Context(), err)
		exchangeOf(r.Context()).answerError(class)
		log.Printf("gateway: %s answer not authorized: %s (correlation %s)", txType, class, boundedID(inboundCorrID))
		return nil, http.StatusBadGateway, "authorization failed"
	}
	respTokStr, err := tokenJSON(respTok)
	if err != nil {
		return nil, http.StatusInternalServerError, "token marshal failed"
	}
	respEnv.Metadata.AuthzToken = respTokStr
	// The payer's own binding of the member, when it differs from the leg
	// token's patient, rides in the involved list with its own token, so the
	// Hub records the answer under it too. Recording only: nothing here fails
	// the answer.
	respEnv.Metadata.Involved = g.involvedForAnswer(r, txType, respFrame, respOp, inboundCorrID, sha256hex(respEnv.Ciphertext), subjectPCI)

	out, err = shnsdk.EncodeEnvelope(respEnv)
	if err != nil {
		return nil, http.StatusInternalServerError, "encode failed"
	}
	x := exchangeOf(r.Context())
	x.responseHash(sha256hex(respEnv.Ciphertext))
	x.decided(answerOutcome(k.Outcome), "", "")
	x.answered(p)
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
// header — SUCCESS frames only; respondLegError's non-2xx
// frames are relayed verbatim and deliberately unstamped. An encode error
// means an out-of-range status literal (caller bug) — fall back to bare so the
// exchange still answers.
func (g *Gateway) framePayload(requester string, status int, contentType, contractToken string, payload []byte) []byte {
	out, _ := g.frameAnswer(requester, status, contentType, contractToken, payload)
	return out
}

// frameAnswer is framePayload, reporting whether the answer was framed.
func (g *Gateway) frameAnswer(requester string, status int, contentType, contractToken string, payload []byte) ([]byte, bool) {
	if !g.frameNegotiated(requester) {
		return payload, false
	}
	framed, err := shnsdk.EncodeHTTPFrameHeaders(status, map[string]string{
		"Content-Type":                    contentType,
		shnsdk.FrameHeaderContractVersion: contractToken, // "" is omitted by the encoder
	}, payload)
	if err != nil {
		return payload, false
	}
	return framed, true
}

// successFrame is the buildResponseLeg frame for a 2xx answer (framePayload),
// framed with status (LegResult.answerStatus), which the leg's record keeps.
// A requester that negotiated no frame gets the answer bare, which it reads
// as the 200 the leg is answered to the Hub with, and the record keeps that.
func (g *Gateway) successFrame(ctx context.Context, requester string, status int, contentType, contractToken string) func([]byte) ([]byte, error) {
	return func(payload []byte) ([]byte, error) {
		out, framed := g.frameAnswer(requester, status, contentType, contractToken, payload)
		if framed {
			exchangeOf(ctx).answer(status)
		}
		return out, nil
	}
}

// successMediaType is the media type a success frame states for p: a
// participant's answer, relayed, keeps the one its system stated (a payer's
// CDS Hooks answer may be text/json); an answer this gateway built is FHIR
// JSON, as is a relayed one that states none.
func successMediaType(p relay.Payload) string {
	if (LegResult{Response: p}).ResponseRelayed() && p.ContentType() != "" {
		return p.ContentType()
	}
	return "application/fhir+json"
}

// respondLeg builds and writes a response leg in one call. Used by the legs that
// do NOT commit holder state between build and write (eligibility, CRD, DTR,
// federated query). The PAS legs call buildResponseLeg/writeLeg explicitly so
// they can commit state ONLY after a successful build. The
// success payload is sealed as a v1 frame(answerStatus, successMediaType) for a
// frame-negotiated requester, bare legacy otherwise. answerStatus is
// LegResult.answerStatus for a responder's answer, 200 for one this gateway
// built.
//
// builtToken is the contract-version token this answer's payload was BUILT at —
// the honored/recomputed answer line — and becomes the frame's contractVersion
// stamp. "" leaves the stamp at this build's own declared line for the leg
// (version-neutral legs are never stamped either way).
//
// A payload that is the participant's own message (relayed, possibly with
// registered edits) holds bytes THIS BUILD DID NOT PRODUCE. Stamp honesty: such
// an answer is left UNSTAMPED — the stamp is content-descriptive, and SHN cannot
// vouch for the contract line of a partner's bytes; absence is tolerated by
// design, a wrong claim is not.
func (g *Gateway) respondLeg(w http.ResponseWriter, r *http.Request, respFrame, respOp, txType, inboundCorrID string, p relay.Payload, answerStatus int, subjectPCI, requester, consentRef, builtToken string) {
	ct := successMediaType(p)
	stamp, terr := g.contractTokenForLeg(txType, builtToken)
	if terr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": terr.Error()})
		return
	}
	if (LegResult{Response: p}).ResponseRelayed() {
		stamp = "" // stamp honesty: never stamp bytes this build did not produce (the encoder omits "")
	}
	out, status, msg := g.buildResponseLeg(r, respFrame, respOp, txType, inboundCorrID, p, answerKey(txType, relay.OutcomeAnswered),
		g.successFrame(r.Context(), requester, answerStatus, ct, stamp), subjectPCI, requester, consentRef)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	writeLeg(w, out)
}

// msgUnframedRequester is the gateway's refusal, in place of a participant's
// application error, to a requester that negotiated no message frame.
const msgUnframedRequester = "%s: recipient answered %d; its answer is carried only in a message frame"

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
// builtToken is deliberately UNUSED on the genuine error path: non-2xx frames are
// relayed verbatim and never carry a contractVersion stamp (
// stamping is a SUCCESS-frame property, and a relayed application error may not even
// be this build's bytes). It exists only to be forwarded on the 2xx-misuse reroute
// below, so the success seal still stamps correctly. Do not "fix" the unused
// parameter by stamping error frames.
func (g *Gateway) respondLegError(w http.ResponseWriter, r *http.Request, respFrame, respOp, txType, corrID string, result LegResult, subjectPCI, requester, consentRef, builtToken string) {
	if result.Status/100 == 2 { // connector misuse guard: a 2xx belongs on the success seal
		g.respondLeg(w, r, respFrame, respOp, txType, corrID, result.Response, result.answerStatus(), subjectPCI, requester, consentRef, builtToken)
		return
	}
	// A connector/responder can answer non-2xx with NO body and NO LegResult.Message at
	// all — internal/brpayermirror's loopback mirror
	// does this on every one of its own rejection paths (a truly empty response body),
	// and gateway/engine/native.go's upstream-relay path only ever sets LegResult.Status +
	// Response (never Message) for a relayed non-2xx. Both of those are pre-existing,
	// separately-owned surfaces (a demo/test fixture and a verbatim-relay responder that
	// legitimately may not know the upstream's own error shape) — this is the ONE choke
	// point every leg's non-2xx answer passes through before a requester ever sees it, so
	// this is where the fail-closed guard belongs: an empty result.Message never reaches
	// the requester as a bare `{"error":""}`, which tells a partner nothing and is
	// indistinguishable from "the field was simply omitted". legibleLegErrorMessage names
	// the leg and status instead. Pinned by TestRespondLegError_NeverEmptyMessage.
	msg := legibleLegErrorMessage(txType, result.Status, result.Message)
	// With no Response the answer is this gateway's (or the connector's) own
	// refusal, written as {"error": msg}. A Response the refusal writer
	// authored is this gateway's refusal with a body of its own (for example
	// the hooks a payer offers). Otherwise it is the participant's application
	// error, relayed exactly as it answered: an empty body stays empty, and its
	// media type is its own.
	p, k := result.Response, answerKey(txType, relay.OutcomeUpstreamError)
	ownRefusal := p.Ownership() == relay.OwnershipAuthored && p.Builder() == relay.BuilderGatewayRefusal
	gatewayRefusal := p.Ownership() == 0
	if !gatewayRefusal && !ownRefusal && !g.frameNegotiated(requester) {
		// A requester that negotiated no frame cannot receive the
		// participant's answer: a bare answer reaches the payload-blind Hub
		// unsealed. Its ownership is still checked, and the gateway answers
		// with its own refusal, so the participant's bytes never leave it
		// unsealed.
		if _, err := relay.Transmit(p, relay.Check(k)); err != nil {
			g.ownershipRefused(k, err)
			writeOwnershipFault(w)
			return
		}
		gatewayRefusal, msg = true, fmt.Sprintf(msgUnframedRequester, txType, result.Status)
	}
	if gatewayRefusal || ownRefusal {
		k = answerKey(txType, relay.OutcomeRefused)
	}
	if gatewayRefusal {
		body, _ := json.Marshal(map[string]string{"error": msg})
		if !g.frameNegotiated(requester) {
			body = append(body, '\n') // the bare answer's json.Encoder framing
		}
		sp, err := relay.Authored(relay.BuilderGatewayRefusal, body, "application/json")
		if err != nil {
			g.ownershipRefused(k, err)
			writeOwnershipFault(w)
			return
		}
		p = sp
	}
	// The participant's media type travels with its error bytes; a non-empty
	// body that states none is read as FHIR JSON. An empty body keeps what
	// the participant stated, nothing included.
	ct := p.ContentType()
	if ct == "" && p.Len() > 0 && !gatewayRefusal && !ownRefusal {
		ct = "application/fhir+json"
	}
	if !g.frameNegotiated(requester) {
		// Legacy peer: the pre-v0.27.0 contract — bare non-2xx, which the
		// payload-blind Hub reports as its generic mechanical 502.
		g.writePayload(w, result.Status, ct, p, k)
		return
	}
	appStatus := result.Status
	exchangeOf(r.Context()).answer(appStatus)
	out, status, msg := g.buildResponseLeg(r, respFrame, respOp, txType, corrID, p, k, func(b []byte) ([]byte, error) {
		return shnsdk.EncodeHTTPFrame(appStatus, ct, b)
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
