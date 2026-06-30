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
	"net/http"
	"sync"
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
// composite Oswestry "42"; D-2RI-1 — operator-supplied, NOT derived from a clinical SoR fact).
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
	// CounterpartID is the holder this gateway transacts WITH on the provider side
	// (the payer's id). Replaces the literal "payer" in routing call sites.
	// The payer side does not use it — it replies to the inbound envelope's Sender.
	CounterpartID string
	// OriginationProfile selects the per-UC behavior lane: "" / "sandbox" = the sandbox
	// shape (default); "composite" = drive real br-payer verdicts (Mode A, harness-provider-gw
	// only). A5 reads it to treat a legitimate PAS pend as a terminal success in composite
	// (br-payer never resolves A4→A1); B1 also keys the HCPCS code map on it. Spec 2B-bis/2C.
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
	Validator       shnsdk.Validator
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
}

// New constructs a Gateway. The clock defaults to time.Now and the client to
// http.DefaultClient when unset.
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

// pendState is the provider's own in-flight PA workflow state for a PENDED
// attestation scenario (UC-06/UC-07), held under an opaque resume token between
// the run-to-PENDED step and the resume-to-APPROVED step. It is the provider's
// orchestration state, never substrate state; the browser only ever holds the
// opaque token. The store is in-memory, Reset-cleared, no TTL — a documented
// single-operator demo simplification (a production EHR would persist + expire).
type pendState struct {
	scenario    string // "uc06" or "uc07"
	qrJSON      []byte
	srJSON      []byte
	patientRef  string
	coverageRef string
	pci         string
	pasCorr     string
	filled      []FilledItem
	needed      []string
	qrAnswers   map[string]string // provider-data UC-06: the org-attested base answer trace (1.1/3.1), surfaced in the response as FR-17 mixed-provenance evidence
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
		mux.HandleFunc("POST /scenario/uc03", g.handleUC03)
		mux.HandleFunc("POST /scenario/uc04", g.handleUC04)
		mux.HandleFunc("POST /scenario/uc05", g.handleUC05)
		mux.HandleFunc("POST /scenario/uc06", g.handleUC06)
		mux.HandleFunc("POST /scenario/uc07", g.handleUC07)
		mux.HandleFunc("POST /scenario/uc07hcpcs", g.handleUC07HCPCS)
		mux.HandleFunc("POST /scenario/uc08", g.handleUC08)
		mux.HandleFunc("POST /scenario/homeoxygen", g.handleHomeOxygen)
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
			mux.HandleFunc("POST /cds-services/{id}", g.handleCRDIngress)
			mux.HandleFunc("POST /Questionnaire/$questionnaire-package", g.handleDTRIngress)
			mux.HandleFunc("POST /Claim/$submit", g.handlePASIngress)
			if g.ingressAuth != nil {
				mux.HandleFunc("POST /oauth/token", g.ingressAuth.handleToken)
				mux.HandleFunc("GET /.well-known/smart-configuration", g.ingressAuth.handleSmartConfig)
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

// roundTrip performs one authorized sealed exchange with the counterpart holder
// through the Hub: authorize(op) → seal(txType/reqFrame) → POST hub
// /route with a holder assertion → verify the response leg (VerifyBound respOp
// + the SAME correlationID + Sender==recipient, envelope CorrelationID match) →
// decrypt and return the response payload. recipient is the counterpart holder
// id (passed by callers via Config.CounterpartID rather than hardcoded).
// reqFrame/respFrame are the authority frames for the request and response legs
// respectively (provider→payer uses "provider-tpo"/"payer-coverage"; provider→
// facility uses "provider-tpo"/"facility-disclosure"). custodian is forwarded to
// the Authorization Framework for federated-query operations (consent gate); it
// is empty for all other operations. The scope param documents the policy-derived
// min-necessary scope for this exchange; the authz service derives the actual
// scope from policy, so it is not sent on the wire.
func (g *Gateway) roundTrip(ctx context.Context, r *http.Request, recipient, reqFrame, respFrame, op, respOp, txType, scope, pci, correlationID, custodian string, payload []byte) ([]byte, error) {
	_ = scope // policy-derived server-side; kept for contract clarity

	recipientHolder, ok := g.cfg.Reg.Lookup(recipient)
	if !ok {
		return nil, fmt.Errorf("recipient %q not in registry", recipient)
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
		return nil, fmt.Errorf("hub routing failed")
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
	return respPayload, nil
}

// OriginateLeg is the origination leg-primitive: it runs one authorized, sealed,
// Hub-routed exchange for legType, reading the authority frames / operations / scope
// from `paCatalog` (the PA Layer-3 module) instead of taking them as positional
// literals. recipient stays a parameter (payer legs target CounterpartID; facility/phg
// legs target a LookupByRole result). An unknown legType is a caller bug (the Originator
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
	return g.roundTrip(ctx, r, recipient, spec.ReqFrame, spec.RespFrame, spec.Op, spec.RespOp, legType, spec.Scope, pci, correlationID, custodian, content.Bytes)
}

// validateFHIR runs the configured validator over a FHIR resource on the given
// leg, returning the gateway-standard (status,message) on failure. dir is
// "egress" or "ingress" purely for the error message; status is 0 on success.
func (g *Gateway) validateFHIR(ctx context.Context, resourceJSON []byte, dir string) (int, string) {
	// br-payer-targeting lanes (composite, provider-data) relay the counterparty's FOREIGN bytes
	// on ingress — SHN does not $validate foreign bundles (R-8/FR-36: SHN certifies only what it
	// PRODUCES and hosts US Core only; the br-payer's responses stay relayed:true). The sandbox
	// lane (SHN-produced responses) still validates ingress; egress (always SHN-produced) always
	// validates. br-payer's Da Vinci DTR/PAS bytes fail a US-Core-only validator.
	if dir == "ingress" && targetsBrPayer(g.cfg.OriginationProfile) {
		return 0, ""
	}
	res, err := g.cfg.Validator.Validate(ctx, resourceJSON, "")
	if err != nil {
		return http.StatusInternalServerError, "validator unavailable"
	}
	if !res.Valid {
		return http.StatusUnprocessableEntity, dir + " validation failed"
	}
	return 0, ""
}

// buildResponseLeg performs every fail-prone step of a response leg — authorize,
// marshal the token, look up the requester, seal, encode — WITHOUT writing to w
// or committing any state. On failure it returns the gateway-standard (status,
// msg) with out==nil; on success it returns (out, 0, ""). Callers that mutate
// holder state for a leg MUST call this and check the status BEFORE committing
// state, so a constructible response-leg failure (unknown requester, seal,
// encode) cannot orphan payer state (review-fixes-6 #1).
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

// respondLeg builds and writes a response leg in one call. Used by the legs that
// do NOT commit holder state between build and write (eligibility, CRD, DTR,
// federated query). The PAS legs call buildResponseLeg/writeLeg explicitly so
// they can commit state ONLY after a successful build (review-fixes-6 #1).
func (g *Gateway) respondLeg(w http.ResponseWriter, r *http.Request, respFrame, respOp, txType, inboundCorrID string, payload []byte, subjectPCI, requester, consentRef string) {
	out, status, msg := g.buildResponseLeg(r, respFrame, respOp, txType, inboundCorrID, payload, subjectPCI, requester, consentRef)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	writeLeg(w, out)
}
