package engine

import (
	"net/http"
	"sync"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// module1_pending.go is module 1's in-flight state: the pend records of the
// two-phase /scenario/* routes, the patient-facing UC-07 pending lookup, the
// scenario reset, and the questionnaire items the resume paths answer.

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

// pendState is the provider's own in-flight PA workflow state for a PENDED
// attestation scenario (UC-06/UC-07), held under an opaque resume token between
// the run-to-PENDED step and the resume-to-APPROVED step. It is the provider's
// orchestration state, never substrate state; the browser only ever holds the
// opaque token. The store is in-memory, Reset-cleared, no TTL — a documented
// single-operator demo simplification (a production EHR would persist + expire).
type pendState struct {
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
	// claimFacts are the request facts the pended submission's request stated
	// (originationClaimInput), and submitted is that submission as it was
	// built (recordPASSubmission). The resume amendment states the facts its
	// own request states, or — when that request states none — these, the
	// requester's own earlier statement; it restates the submission's own
	// type, priority and created. Its certificationType is always the resume
	// request's. In-memory like the rest of pendState.
	claimFacts shnsdk.PASClaimFacts
	submitted  pasSubmissionRecord
}

// moduleOnePending is module 1's in-flight pend state: the resume tokens of
// the two-phase /scenario/* routes, per process. Its zero value is empty.
type moduleOnePending struct {
	mu      sync.Mutex
	byToken map[string]pendState
}

// storePending saves st under a fresh opaque resume token and returns it.
func (g *Gateway) storePending(st pendState) string {
	token := newCorrelationID() // crypto-random hex (16 bytes)
	g.pending.mu.Lock()
	defer g.pending.mu.Unlock()
	if g.pending.byToken == nil {
		g.pending.byToken = map[string]pendState{}
	}
	g.pending.byToken[token] = st
	return token
}

// loadPending returns the state for token, if present.
func (g *Gateway) loadPending(token string) (pendState, bool) {
	g.pending.mu.Lock()
	defer g.pending.mu.Unlock()
	st, ok := g.pending.byToken[token]
	return st, ok
}

// dropPending deletes token's state (idempotent).
func (g *Gateway) dropPending(token string) {
	g.pending.mu.Lock()
	defer g.pending.mu.Unlock()
	delete(g.pending.byToken, token)
}

// Reset clears the pending-scenario store (POST /scenario/reset calls it, so a
// fresh demo run starts with no stale in-flight PAs).
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
	g.pending.mu.Lock()
	g.pending.byToken = nil
	g.pending.mu.Unlock()
	// The store reset runs outside the pend lock, which guards only the pend
	// state: holding it across a durable store's round trip would stall every
	// in-flight pend and resolve for its duration. A fresh demo run starts with
	// no stale exchanges, consistent with the pend state (resetExchanges).
	return g.resetExchanges()
}

// pendingForPatient returns the resume token of a pended scenario for (scenario,
// pci), if any. Read-only over the same store the two-phase start/complete use.
func (g *Gateway) pendingForPatient(scenario, pci string) (string, bool) {
	g.pending.mu.Lock()
	defer g.pending.mu.Unlock()
	// Assumes at most one in-flight pend per (scenario, pci); returns an arbitrary match otherwise (map iteration order).
	for token, st := range g.pending.byToken {
		if st.scenario == scenario && st.pci == pci {
			return token, true
		}
	}
	return "", false
}

// uc07PendingResp is the patient-facing pending-questionnaire descriptor: the
// Oswestry functional-status item + the opaque resume token (internal — phgsvc
// re-resolves it server-side and never exposes it to the patient app).
//
// Member and Order are the pended request's own: the member it was made for and
// the order its claim facts were stated for, read from this gateway's own pend
// record (the pendState the start stored), never from a request or a default.
// A completer that holds only the patient (the PHG) echoes them back on
// /scenario/uc07/complete — so the completion's member and order are this
// gateway's own record, not a statement the PHG makes — and the route still
// refuses a member or order that is not the pend's.
type uc07PendingResp struct {
	LinkID      string       `json:"linkId"`
	Text        string       `json:"text"`
	ResumeToken string       `json:"resumeToken"`
	Member      string       `json:"member,omitempty"`
	Order       *statedOrder `json:"order,omitempty"`
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
	resp := uc07PendingResp{
		LinkID:      oswestryLinkID,
		Text:        "What is your current Oswestry Disability Index score (0–100)?",
		ResumeToken: token,
	}
	if st, found := g.loadPending(token); found {
		resp.Member = st.member
		if system, code, _, err := shnsdk.ParseOrderProductCoding(st.srJSON); err == nil {
			resp.Order = &statedOrder{System: system, Code: code}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleScenarioReset clears the provider's in-memory pended-scenario store and the
// holder's exchange records so a fresh demo run starts with no stale in-flight PAs.
// Idempotent. A store half that failed answers 503: the caller (tools/cloudsmoke's
// reset) fails the run instead of reporting a clean reset while records are still
// there. The body names the store and nothing else — never the database's own error.
func (g *Gateway) handleScenarioReset(w http.ResponseWriter, r *http.Request) {
	if err := g.Reset(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "exchange store reset failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
