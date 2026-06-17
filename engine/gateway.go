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
	// Identity is the gateway's substrate identity: its holder signing key (used to
	// sign holder assertions and patient-access audit records) plus its envelope-
	// encryption keypair (used to open inbound sealed payloads and seal responses).
	Identity shnsdk.Identity
	AuthzURL string
	AuthzPub ed25519.PublicKey
	// HubTransportPub verifies the per-hop X-Hub-Assertion the Hub sends with
	// every /substrate/inbound forward (responder-enablement spec §A3). REQUIRED
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
	Client      *http.Client
	Clock       func() time.Time
	NPI         string
	// CorrelationGen generates a new correlation ID for each outbound scenario
	// request. Defaults to newCorrelationID (crypto-random 128-bit hex string).
	// Override in tests for deterministic IDs.
	CorrelationGen func() string
	// ConsentURL is the Trust-operated Global Person Consent service URL (facility
	// only). The facility's consent backstop (§8.4) re-confirms a TREAT permit here
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

	// paReplay rejects a patient-access correlationId re-presented within
	// paReplayWindow (consume-once replay binding on the direct Patient Access read).
	paReplay *shnsdk.ReplayGuard

	// hubJTI enforces one-time-use on the Hub's X-Hub-Assertion jti at
	// /substrate/inbound (spec §A3). In-memory per-replica; cross-replica replay
	// is bounded by the 2-minute assertion TTL (single-task sandbox today; a shared
	// store is the additive revisit if gateways ever scale horizontally).
	hubJTI *shnsdk.ReplayGuard
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
	if cfg.Role == "payer" && cfg.Adjudicator == nil {
		panic("gateway: Config.Adjudicator is required for the payer role (the four payer decision points route through it)")
	}
	return &Gateway{
		cfg:      cfg,
		pending:  map[string]pendState{},
		paReplay: shnsdk.NewReplayGuard(paReplayWindow, paReplayMaxEntries),
		hubJTI:   shnsdk.NewReplayGuard(shnsdk.MaxAssertionTTL, 1<<16),
	}
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
	filled      []filledItem
	needed      []string
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
		mux.HandleFunc("POST /scenario/uc08", g.handleUC08)
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
	case "payer":
		mux.HandleFunc("POST /substrate/inbound", g.handleInbound)
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

// validateFHIR runs the configured validator over a FHIR resource on the given
// leg, returning the gateway-standard (status,message) on failure. dir is
// "egress" or "ingress" purely for the error message; status is 0 on success.
func (g *Gateway) validateFHIR(ctx context.Context, resourceJSON []byte, dir string) (int, string) {
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
