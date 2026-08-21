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
// sandbox Oswestry "42"; D-2RI-1 — operator-supplied, NOT derived from a clinical SoR fact).
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
	// OriginationProfile selects the per-UC behavior lane: "" / "sandbox" = the sandbox
	// shape (default, SHN-produced, byte-unchanged); "provider-data" = originate every UC off
	// the provider's seeded SoR and drive real br-payer verdicts (Mode A). targetsBrPayer keys
	// the contained-insurer / absolute-refs / R-8 ingress-skip handling on it. Spec 2B-bis/2C.
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
	// DeclaredContractVersions is the operator-declared exchange-contract token set
	// (SHN_CONTRACT_VERSIONS, boot-validated in gateway/app: grammar + ⊆
	// NativeContractVersions). Empty ⇒ shnsdk.SupportedContractVersions(). Read ONLY
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
	// Store is the gateway's own business state (auth numbers, pended-claim ledger,
	// issued EOBs). Demo: in-memory stub; separated: holdersim; later: gateway Postgres.
	Store Store
	// Adjudicator is the payer's decision surface (eligibility/order-select/
	// questionnaire/prior-auth). REQUIRED for the payer role; New panics without
	// it. The default is NewSandboxAdjudicator (the same sandbox decisions the
	// gateway made inline before); a partner injects their own. The same interface backs the public SDK
	// Responder, so one Adjudicator works against both surfaces (Edge premise).
	Adjudicator shnsdk.Adjudicator
	// Responder is the payer content seam. Normally left nil and DERIVED from
	// Adjudicator in NewGateway (keeping the STABILITY-supported Adjudicator
	// injection seam working). A test/partner MAY inject a custom LegResponder.
	Responder LegResponder
	// PayerDavinciNative reports that the payer Responder native-forwards the read-only
	// legs to a REAL partner Da Vinci endpoint (PAYER_DAVINCI_BASE_URL set). When true the
	// DTR $questionnaire-package response is a FOREIGN Da Vinci Bundle (dtr-std-questionnaire /
	// dtr-questionnaireresponse profiles) that SHN — which hosts US Core only — cannot
	// $validate; like the conformant crd-order-select / pas-claim legs (R-8),
	// the DTR response is a NEAR-RELAY: the trust-critical subject fence still runs, but the
	// engine does NOT foreign-$validate it. false ⇒ the sandbox path (SHN's own
	// US-Core-resolvable package), which still egress-$validates byte-identically. FR-G28.
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
	// LegMetric, when non-nil, receives one outcome string per origination-leg
	// event at the roundTrip choke point: LegOutcomeRouted when a leg is
	// attempted, then exactly one terminal outcome — Answered (the counterpart
	// responded: 2xx or relayed app non-2xx), Denied (the Authorization
	// Framework denied the leg — a policy decision, not an error), Unreachable
	// (the Hub could not be reached / did not route), or Failed (anything
	// else). nil (the default, and the published-gateway posture) = no
	// emission. MAY BE CALLED CONCURRENTLY; implementations must be
	// goroutine-safe and must never block — the callback sits on the request
	// path. Additive instrumentation only: emission must not change exchange
	// behavior (TestLegMetric_ConformanceNeutral). Carries NO payloads.
	LegMetric func(outcome string)
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
}

// Gateway is a constructed holder gateway.
type Gateway struct {
	cfg     Config
	mu      sync.Mutex
	pending map[string]pendState

	// exchanges is the Layer-2 Exchange-correlation seam (the DaVinciIngress origination
	// driver groups each ingress call's legs under one Exchange.ID). In-memory default;
	// a durable/expiring/shared impl is a planned future drop-in behind ExchangeStore.
	exchanges ExchangeStore

	// paReplay rejects a patient-access correlationId re-presented within
	// paReplayWindow (consume-once replay binding on the direct Patient Access read).
	paReplay *shnsdk.ReplayGuard

	// hubJTI enforces one-time-use on the Hub's X-Hub-Assertion jti at
	// /substrate/inbound. In-memory per-replica; cross-replica replay
	// is bounded by the 2-minute assertion TTL (single-task sandbox today; a shared
	// store is the additive revisit if gateways ever scale horizontally).
	hubJTI *shnsdk.ReplayGuard

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
	edgeCapture atomic.Pointer[edgeCaptureStore]
}

// New constructs a Gateway. The clock defaults to time.Now and the client to
// http.DefaultClient when unset.
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
func New(cfg Config) *Gateway {
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
	// Observer seam: decorate the SoR BEFORE the Responder/Populator
	// derivations below — both capture cfg.SoR at construction
	// (NewSandboxResponder / newManagedPopulator), and decorating at the
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
	// Derive the default content seam from the injected Adjudicator (the
	// partner-facing field), so a partner constructing the engine directly with an
	// Adjudicator still works (STABILITY seam). EVERY payer leg now routes through
	// Responder — no leg calls cfg.Adjudicator directly — so the derived Responder
	// is the sole content surface and Adjudicator's only role is to derive it.
	if cfg.Responder == nil && cfg.Adjudicator != nil {
		cfg.Responder = NewSandboxResponder(cfg.Adjudicator, cfg.SoR, cfg.Store, cfg.Clock)
	}
	// A payer REQUIRES a content Responder. Adjudicator is the SUPPORTED partner
	// decision seam: setting it auto-derives the default Responder above (every
	// existing caller takes this path). A test/partner MAY instead inject a custom
	// LegResponder directly (the native-forward case) with no Adjudicator. Either
	// way the Responder must be non-nil, so the guard is now Responder-nil.
	if cfg.Role == "payer" && cfg.Responder == nil {
		panic("gateway: the payer role requires a content Responder — set Config.Adjudicator (the supported decision seam; it derives the Responder) or inject Config.Responder")
	}
	if cfg.Populator == nil {
		cfg.Populator = newManagedPopulator(cfg.SoR)
	}
	g := &Gateway{
		cfg:       cfg,
		pending:   map[string]pendState{},
		exchanges: NewInMemoryExchangeStore(),
		paReplay:  shnsdk.NewReplayGuard(paReplayWindow, paReplayMaxEntries),
		hubJTI:    shnsdk.NewReplayGuard(shnsdk.MaxAssertionTTL, 1<<16),
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
	if cfg.IngressEnabled && !cfg.ingressAuthBypass {
		ia, err := newIngressAuthServer(cfg.IngressBaseURL, cfg.IngressClients, cfg.Clock)
		if err != nil {
			panic("gateway: " + err.Error())
		}
		g.ingressAuth = ia
	}
	return g
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
	parsed, ok := shnsdk.ParsePayerIdentifier(coverageJSON, resolveRef)
	if !ok {
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
func (g *Gateway) recipientFor(coverageJSON []byte) (holderID string, status int, msg string) {
	var resolveRef func(string) ([]byte, bool)
	if g.cfg.SoR != nil {
		resolveRef = g.cfg.SoR.ResolveByReference
	}
	holder, _, status, msg := g.recipientForWith(coverageJSON, resolveRef)
	return holder, status, msg
}

// pendState is the provider's own in-flight PA workflow state for a PENDED
// attestation scenario (UC-06/UC-07), held under an opaque resume token between
// the run-to-PENDED step and the resume-to-APPROVED step. It is the provider's
// orchestration state, never substrate state; the browser only ever holds the
// opaque token. The store is in-memory, Reset-cleared, no TTL — a documented
// single-operator demo simplification (a production EHR would persist + expire).
type pendState struct {
	scenario string // "uc06" or "uc07"
	qrJSON   []byte
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
	// member is the BARE member id the pended submit stamped as the Coverage's
	// urn:shn:coverage MB identifier value (that identifier is a member number, not a
	// reference) and as the Claim's insurance[0].coverage LOGICAL reference — the same
	// bare value in both roles; the resume ClaimUpdate must stamp the SAME value
	// in both places, so it is pinned here beside coverageRef (which stays the Reference-shaped
	// value the QR-context / native-lane roles need). In-memory like the rest of pendState.
	member    string
	pci       string
	pasCorr   string
	filled    []FilledItem
	needed    []string
	qrAnswers map[string]string      // provider-data UC-06: the org-attested base answer trace (1.1/3.1), surfaced in the response as FR-17 mixed-provenance evidence
	payer     shnsdk.PayerIdentifier // the member's REAL payer identity (parsed from OpenCoverage at run-to-PENDED) — threads to the resume ClaimUpdate builders so the payload's payer derives from the patient's real Coverage (FR-G40)
	recipient string                 // the payer HOLDER id the resume legs route to, resolved from the member's real Coverage at run-to-PENDED (recipientFor) — no default (FR-G40 / AI-G11 / OWD-G10)
	pasToken  string                 // the pa.pas contract token selected at run-to-PENDED — the PENDED-LINE PIN. Threads to the resume pas-claim-update legs as Content.ProfileID so a pended exchange finishes on the line it started on, regardless of registry drift. Lives HERE by settled decision (AI-1: never ExchangeStore); in-memory/Reset-cleared like the recipient pin beside it — a durable pend store inherits it.
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
func (g *Gateway) Reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pending = map[string]pendState{}
	// A fresh demo run starts with no stale exchanges, consistent with the pending map.
	// The Exchange store holds only metadata-only LegRecords.
	g.exchanges = NewInMemoryExchangeStore()
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

// handleScenarioReset clears the provider's in-memory pended-scenario store so a
// fresh demo run starts with no stale in-flight PAs. Idempotent.
func (g *Gateway) handleScenarioReset(w http.ResponseWriter, r *http.Request) {
	g.Reset()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// Handler returns the role-appropriate HTTP routes.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	switch g.cfg.Role {
	case "provider":
		mux.HandleFunc("POST /scenario/uc01", g.handleScenario)
		mux.HandleFunc("POST /scenario/uc02", g.handleUC02)
		// FR-G41 live routing proofs off the /holders feed: uc02-payerb self-discovers a
		// SECOND payer holder (MBR-PD-UC02-PB / 00078 → `payer-b`); uc02-unknownpayer fails closed 422
		// (MBR-UNKNOWN-PAYER / 00099 → no registered payer). Both share the handleUC02 no-PA CRD body.
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
		// Admin reset of the in-memory pended-scenario store. The in-process devstack
		// calls g.Reset() directly; in the SEPARATED deployment the console reset hits
		// this route so a pended UC-06/07 does not survive as an orphaned questionnaire
		// (which would 502 on a stale patient submit). Internal — provider-gw is not public.
		mux.HandleFunc("POST /scenario/reset", g.handleScenarioReset)
		if g.cfg.IngressEnabled {
			mux.HandleFunc("GET /cds-services", g.handleCDSDiscovery)
			mux.HandleFunc("POST /cds-services/{id}", g.observeIngress("crd-ingress", g.handleCRDIngress))
			mux.HandleFunc("POST /Questionnaire/$questionnaire-package", g.observeIngress("dtr-ingress", g.handleDTRIngress))
			mux.HandleFunc("POST /Claim/$submit", g.observeIngress("pas-ingress", g.handlePASIngress))
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
		mux.HandleFunc("POST /substrate/inbound", g.handleInbound)
		// The native-forward (composite) payer is an INTERNAL conformance-harness participant
		// that holds in-memory pending/exchanges across runs; the separated/cloud console reset
		// clears them too (separated-reset-clears-gateway-state). Gated on PayerDavinciNative so
		// the PUBLIC built-in sandbox payer-gw never exposes an unauthenticated state-clearing
		// route. Generic g.Reset() (clears pending+exchanges), same handler as the provider lane.
		if g.cfg.PayerDavinciNative {
			mux.HandleFunc("POST /scenario/reset", g.handleScenarioReset)
		}
		// FR-28: CMS-0057 Patient Access API — conformant FHIR search + instance read
		// over the PDex PA EOB, gated by a patient-access authority token. Distinct
		// from the sealed substrate legs. FR-37: the CapabilityStatement for this
		// surface is published at the standard FHIR /metadata endpoint.
		mux.HandleFunc("GET /metadata", g.handlePatientAccessMetadata)
		mux.HandleFunc("GET /ExplanationOfBenefit", g.handlePatientAccessEOB)
		mux.HandleFunc("GET /ExplanationOfBenefit/{id}", g.handlePatientAccessEOBByID)
	case "facility":
		mux.HandleFunc("POST /substrate/inbound", g.handleInbound)
	case "phg":
		mux.HandleFunc("POST /substrate/inbound", g.handleInbound)
	}
	return mux
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
func (g *Gateway) ingressAuthOK(r *http.Request) bool {
	if g.cfg.ingressAuthBypass {
		return true
	}
	if g.ingressAuth == nil {
		return false // fail-closed: no inbound auth configured
	}
	// SMART Backend Services issued bearer (token-exchange) OR a UDAP B2B direct bearer
	// (a registered client's self-signed private_key_jwt, the form br-provider sends).
	// Token-shape disjoint, so the OR cannot fail open (FR-G28 UDAP B2B).
	return g.ingressAuth.verifyBearer(r) || g.ingressAuth.verifyDirectBearer(r)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func newCorrelationID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
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
func (g *Gateway) postEnvelope(ctx context.Context, url string, body []byte, assertionHeader string) (shnsdk.Envelope, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return shnsdk.Envelope{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Holder-Assertion", assertionHeader)

	resp, err := g.cfg.Client.Do(req)
	if err != nil {
		return shnsdk.Envelope{}, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, shnsdk.MaxResponseBytes))
	if err != nil {
		return shnsdk.Envelope{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return shnsdk.Envelope{}, fmt.Errorf("gateway: hub returned %d: %s", resp.StatusCode, string(respBody))
	}
	return shnsdk.DecodeEnvelope(respBody)
}

// roundTrip performs one authorized sealed exchange with the counterpart
// holder through the Hub (see roundTripInner for the mechanics). It is ALSO
// the observer seam's origination choke point: every origination leg emits
// leg.originated before the exchange and leg.response / leg.failed after —
// one seam covers every UC flow in both lanes (see STABILITY.md).
func (g *Gateway) roundTrip(ctx context.Context, r *http.Request, recipient, reqFrame, respFrame, op, respOp, txType, scope, pci, correlationID, custodian string, content Content) ([]byte, error) {
	g.observe(ObserverEvent{
		Kind: "leg.originated", Direction: "originate", LegType: txType,
		CorrelationID: correlationID, Counterpart: recipient,
		AuthorityFrame: reqFrame, Op: op, Payload: json.RawMessage(content.Bytes),
		Route: content.Route,
	})
	g.legMetric(LegOutcomeRouted)
	respPayload, err := g.roundTripInner(ctx, r, recipient, reqFrame, respFrame, op, respOp, txType, scope, pci, correlationID, custodian, content)
	if err != nil {
		var re *RelayError
		if errors.As(err, &re) {
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
		case errors.Is(err, errHubUnreachable):
			outcome = LegOutcomeUnreachable
		}
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
func (g *Gateway) roundTripInner(ctx context.Context, r *http.Request, recipient, reqFrame, respFrame, op, respOp, txType, scope, pci, correlationID, custodian string, content Content) ([]byte, error) {
	_ = scope                // policy-derived server-side; kept for contract clarity
	payload := content.Bytes // content.ProfileID is read below to verify the response frame's stamp

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
	if content.ProfileID != "" && shnsdk.SupportsRequestFrameV1(recipientHolder.RequestFrames) {
		framed, ferr := shnsdk.EncodeHTTPFrameHeaders(http.StatusOK, map[string]string{
			"Content-Type":                    "application/fhir+json",
			shnsdk.FrameHeaderContractVersion: content.ProfileID,
		}, payload)
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

	tok, err := g.authorize(r, reqFrame, op, pci, correlationID, custodian, sha256hex(env.Ciphertext))
	if err != nil {
		// Preserve a genuine authority DENIAL as the typed sentinel (UC-05's
		// no-consent branch depends on telling it apart from an authz outage); any
		// other authorize failure stays an opaque "authorization failed".
		if errors.Is(err, errAuthorizationDenied) {
			return nil, errAuthorizationDenied
		}
		return nil, fmt.Errorf("authorization failed")
	}
	tokStr, err := tokenJSON(tok)
	if err != nil {
		return nil, fmt.Errorf("token marshal failed")
	}
	env.Metadata.AuthzToken = tokStr
	env.Metadata.ConsentRef = tok.ConsentRef // empty for non-federated exchanges

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

	respEnv, err := g.postEnvelope(ctx, g.cfg.HubURL+"/route", body, assertionHeader)
	if err != nil {
		return nil, errHubUnreachable
	}

	// C1/H2b: the response leg must be authorized just like the request leg, bound
	// to the ORIGINAL request correlationID (not the response envelope's own CID),
	// and must come from the expected counterpart holder.
	var respTok shnsdk.Token
	if err := json.Unmarshal([]byte(respEnv.Metadata.AuthzToken), &respTok); err != nil {
		return nil, fmt.Errorf("response leg authorization failed")
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
		return nil, fmt.Errorf("response leg authorization failed")
	}
	if respEnv.Metadata.CorrelationID != correlationID {
		return nil, fmt.Errorf("response correlation mismatch")
	}
	if respEnv.Metadata.Sender != recipient {
		return nil, fmt.Errorf("response sender mismatch")
	}

	respPayload, err := shnsdk.Open(respEnv, g.cfg.Identity.EncPub, g.cfg.Identity.EncPriv)
	if err != nil {
		return nil, fmt.Errorf("response decryption failed")
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
			return nil, fmt.Errorf("response frame decode failed")
		}
		if hdr.Status/100 != 2 {
			return nil, &RelayError{Status: hdr.Status, Body: body, ContentType: hdr.Headers["Content-Type"]}
		}
		// Version-stamp verification: a 2xx framed answer
		// declaring a DIFFERENT contract line than this leg routed is rejected
		// before the body reaches any parser or validator — tamper or skew,
		// either way not the payload we negotiated. Absent stamp = pre-version
		// responder (tolerated, the frames-absent precedent); version-neutral
		// legs (empty ProfileID) ignore stamps. Non-2xx frames return above as
		// *RelayError — relayed verbatim, never parsed as contract content.
		if expected := content.ProfileID; expected != "" {
			if stamped := hdr.Headers[shnsdk.FrameHeaderContractVersion]; stamped != "" && stamped != expected {
				return nil, fmt.Errorf("response contract version mismatch: frame declares %s, leg routed %s", stamped, expected)
			}
		}
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
	if content.WorkstreamType != workstreamPA {
		return nil, fmt.Errorf("OriginateLeg: content workstream %q not served by this gateway", content.WorkstreamType)
	}
	spec, ok := paCatalog[legType]
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
	// select-before-build primitive, originate.go) is where the reachability
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
	return g.roundTrip(ctx, r, recipient, spec.ReqFrame, spec.RespFrame, spec.Op, spec.RespOp, legType, spec.Scope, pci, correlationID, custodian, content)
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
	if line != "" && len(g.cfg.ValidatorsByLine) > 0 {
		v, ok := g.cfg.ValidatorsByLine[line]
		if !ok {
			return nil
		}
		return v
	}
	return g.cfg.Validator
}

// validateFHIR runs the line's configured validator over a FHIR resource on the
// given leg, returning the gateway-standard (status,message) on failure. dir is
// "egress" or "ingress" purely for the error message; status is 0 on success.
// line is the contract line the resource was BUILT at ("" = no line in play);
// an unlaned line fails closed with a 500 naming it (FR-36/FR-G29 — never a
// silent fallback to the canonical lane).
func (g *Gateway) validateFHIR(ctx context.Context, resourceJSON []byte, dir, line string) (int, string) {
	// the br-payer-targeting lane (provider-data) relays the counterparty's FOREIGN bytes
	// on ingress — SHN does not $validate foreign bundles (R-8/FR-36: SHN certifies only what it
	// PRODUCES and hosts US Core only; the br-payer's responses stay relayed:true). The sandbox
	// lane (SHN-produced responses) still validates ingress; egress (always SHN-produced) always
	// validates. br-payer's Da Vinci DTR/PAS bytes fail a US-Core-only validator.
	if dir == "ingress" && targetsBrPayer(g.cfg.OriginationProfile) {
		return 0, ""
	}
	v := g.validatorForLine(line)
	if v == nil {
		return http.StatusInternalServerError, "no FHIR validator lane configured for contract line " + line + " (FR-36/FR-G29)"
	}
	res, err := v.Validate(ctx, resourceJSON, "")
	if err != nil {
		return http.StatusInternalServerError, "validator unavailable"
	}
	if !res.Valid {
		return http.StatusUnprocessableEntity, dir + " validation failed"
	}
	return 0, ""
}

// envelopeEgressLegs is the DTR-fetch-ONLY non-FHIR carve-out (the
// multi-version spec's recorded DTR-fetch known-gap obligation, discharged).
// Membership means egressAdapt walks
// route.Chain for the routing/observer story but never hands the bytes to a
// step function — safe only because dtr-questionnaire-fetch's payload
// (QuestionnaireFetchRequest) is a transport envelope, not a FHIR resource
// any pa.dtr compat-manifest row models.
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
// safe by byte-identity guard" — there is no RUNTIME guard (no equality
// check anywhere in egressAdapt below; out is never a copy, so one would be
// structurally unreachable). "Guard" there means
// TestEnvelopeLegChainIsByteIdenticalPassThrough, the test-time pin that
// enforces byte-identity by construction, not a production code path.
var envelopeEgressLegs = map[string]bool{"dtr-questionnaire-fetch": true}

// egressAdapt applies route's transform chain (if any) to payload before it
// is sent, builds the transform Provenance from the
// chain's LossReports, and emits leg.transformed. Arms (1)/(2) carry
// route.Chain == nil, so this is a pure pass-through for the entire
// production mesh today (arm (3), the only path that reaches applyChain, is
// production-dormant within {2.0,2.1,2.2} per the recorded route-selection
// consequence — every
// published line is native). The caller's EXISTING
// validateFHIR(ctx, bytes, "egress", LineOf(route.Token)) call certifies the
// returned bytes at the TARGET lane before sealing — transformed-output-
// invalid is a hard failure THERE, never silently swallowed here.
func (g *Gateway) egressAdapt(route legRoute, payload []byte, x ExchangeIdentity) ([]byte, []LossReport, error) {
	if len(route.Chain) == 0 {
		return payload, nil, nil
	}

	var out []byte
	var reports []LossReport
	var err error
	if envelopeEgressLegs[x.LegType] {
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
		out, reports, err = applyChain(route.Chain, route.BuildLine, payload, x)
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
	contract := route.Chain[0].Contract
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

// unframeRequest implements the request-framing RECEIVER rule for one inbound
// leg. It is the sender-agnostic half — see unframeRequestFrom for
// the bare-request recomputation that needs the sender's declaration.
//
// A framed request carries the line the ORIGINATOR built its payload at. This
// build honors that claim iff the token is BOTH:
//   - native-buildable (a member of NativeContractVersions() for THIS leg's
//     contract) — we can actually produce the answer at that line; and
//   - laned (validatorForLine resolves) — we can actually VALIDATE at that line.
//
// Anything else is a legible 422 naming what is missing. The predicate is
// native∩laned rather than the routing rule's "declared" — a RECORDED deviation: it
// is what makes the declaration-change window benign, because legs routed off a
// peer's stale (smaller) view of our declaration still complete.
//
// Returns the UNFRAMED body, the answer token to build/validate/stamp at, and the
// gateway-standard (status,msg) — status 0 on accept.
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
	if !nativeContractToken(claimed) || !strings.HasPrefix(claimed, contract+"@") {
		return nil, "", http.StatusUnprocessableEntity,
			"request declares contract version " + claimed + ", which this gateway cannot build for leg " + legType +
				" (it speaks " + strings.Join(sortedTokens(contract, contractLineSet(shnsdk.NativeContractVersions(), contract)), ",") + ")"
	}
	line := shnsdk.LineOf(claimed)
	if g.validatorForLine(line) == nil {
		return nil, "", http.StatusUnprocessableEntity,
			"request declares contract version " + claimed + " but this gateway has no FHIR validator lane for line " + line +
				" — refusing to answer at an unvalidatable line (FR-36/FR-G29)"
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
	answerLineKey  struct{}
	declaredSetKey struct{}
)

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

// buildResponseLeg performs every fail-prone step of a response leg — authorize,
// marshal the token, look up the requester, seal, encode — WITHOUT writing to w
// or committing any state. On failure it returns the gateway-standard (status,
// msg) with out==nil; on success it returns (out, 0, ""). Callers that mutate
// holder state for a leg MUST call this and check the status BEFORE committing
// state, so a constructible response-leg failure (unknown requester, seal,
// encode) cannot orphan payer state.
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
func (g *Gateway) buildResponseLeg(r *http.Request, respFrame, respOp, txType, inboundCorrID string, payload []byte, subjectPCI, requester, consentRef string) (out []byte, status int, msg string) {
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

// writeLeg writes an already-built response-leg envelope as the 200 response.
// The response leg is audited by the trusted Hub (fail-closed), not here.
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

// respondLeg builds and writes a response leg in one call. Used by the legs that
// do NOT commit holder state between build and write (eligibility, CRD, DTR,
// federated query). The PAS legs call buildResponseLeg/writeLeg explicitly so
// they can commit state ONLY after a successful build. The
// success payload is sealed as a v1 frame(200, application/fhir+json) for a
// frame-negotiated requester, bare legacy otherwise.
//
// builtToken is the contract-version token this answer's payload was BUILT at —
// the honored/recomputed answer line — and becomes the frame's contractVersion
// stamp. "" leaves the stamp at this build's own declared line for the leg
// (version-neutral legs are never stamped either way).
//
// relayed marks the payload as bytes THIS BUILD DID NOT PRODUCE (a verbatim
// foreign-partner answer). Stamp honesty: such an answer is left UNSTAMPED — the stamp is
// content-descriptive, and SHN cannot vouch for the contract line of a partner's
// bytes; absence is tolerated by design, a wrong claim is not. It is a distinct
// parameter rather than a magic builtToken value because "" already means "fall
// back to this build's declared line", which is the opposite of omission.
func (g *Gateway) respondLeg(w http.ResponseWriter, r *http.Request, respFrame, respOp, txType, inboundCorrID string, payload []byte, subjectPCI, requester, consentRef, builtToken string, relayed bool) {
	// Success frames seal application/fhir+json by invariant: every success leg
	// today emits FHIR (crd cards, dtr questionnaire, eligibility, the PAS
	// ClaimResponse, the federated-query Bundle, the patient-dtr QuestionnaireResponse,
	// and the native-forward relay of a Da Vinci payer's fhir+json answer). The
	// error branch (respondLegError) already threads a real Content-Type because a
	// relayed non-2xx can be bespoke JSON. Thread a per-leg Content-Type here only
	// when a success leg first emits non-FHIR bytes OR the originator grows a
	// success-frame CT consumer (unframeAnswer drops success CT today) — until then
	// a reserved field would be unread plumbing.
	stamp, terr := g.contractTokenForLeg(txType, builtToken)
	if terr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": terr.Error()})
		return
	}
	if relayed {
		stamp = "" // stamp honesty: never stamp bytes this build did not produce (the encoder omits "")
	}
	framed := g.framePayload(requester, http.StatusOK, "application/fhir+json", stamp, payload)
	out, status, msg := g.buildResponseLeg(r, respFrame, respOp, txType, inboundCorrID, framed, subjectPCI, requester, consentRef)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	writeLeg(w, out)
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
// builtToken is deliberately UNUSED on the genuine error path: non-2xx frames are
// relayed verbatim and never carry a contractVersion stamp (
// stamping is a SUCCESS-frame property, and a relayed application error may not even
// be this build's bytes). It exists only to be forwarded on the 2xx-misuse reroute
// below, so the success seal still stamps correctly. Do not "fix" the unused
// parameter by stamping error frames.
func (g *Gateway) respondLegError(w http.ResponseWriter, r *http.Request, respFrame, respOp, txType, corrID string, result LegResult, subjectPCI, requester, consentRef, builtToken string) {
	if result.Status/100 == 2 { // connector misuse guard: a 2xx belongs on the success seal
		g.respondLeg(w, r, respFrame, respOp, txType, corrID, result.ResponseFHIR, subjectPCI, requester, consentRef, builtToken, result.ResponseRelayed)
		return
	}
	if !g.frameNegotiated(requester) {
		// Legacy peer: the pre-v0.27.0 contract — bare non-2xx, which the
		// payload-blind Hub reports as its generic mechanical 502.
		writeJSON(w, result.Status, map[string]string{"error": result.Message})
		return
	}
	body := result.ResponseFHIR
	ct := "application/fhir+json"
	if len(body) == 0 {
		body, _ = json.Marshal(map[string]string{"error": result.Message})
		ct = "application/json"
	}
	framed, err := shnsdk.EncodeHTTPFrame(result.Status, ct, body)
	if err != nil { // out-of-range connector status: fail legible, mechanical
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "invalid application status"})
		return
	}
	out, status, msg := g.buildResponseLeg(r, respFrame, respOp, txType, corrID, framed, subjectPCI, requester, consentRef)
	if status != 0 { // seal/authz BUILD failure (not the relayed app-status) → gateway fault
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	writeLeg(w, out)
}
