// inbound.go — recipient-side envelope dispatch and the facility / PHG responders
// plus shared inbound helpers. Part of package gateway (the Smart Gateway runs
// every holder role; this file is the inbound-dispatch, facility-disclosure, and
// patient-authorship surface). Behavior-preserving split of gateway.go (finding C);
// no logic change. See gateway.go for the package doc.
package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// ---- Payer role ----

func (g *Gateway) handleInbound(w http.ResponseWriter, r *http.Request) {
	// Per-hop transport auth: verify the Hub's X-Hub-Assertion FIRST, header only,
	// before the body is read or the envelope decoded — an unauthenticated caller
	// never reaches the decoder. Sig + issuer pin ("hub") + audience (this holder)
	// + bounds + jti one-time-use. The jti guard is in-memory ⇒ PER-REPLICA;
	// cross-replica replay is dominated by the 2-minute TTL (single-task sandbox
	// services today; a shared store is the additive revisit if gateways ever
	// scale horizontally).
	if !g.verifyHubAssertion(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "missing or invalid hub assertion"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, shnsdk.MaxRequestBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body failed"})
		return
	}
	env, err := shnsdk.DecodeEnvelope(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode envelope failed"})
		return
	}

	// Require the binding-critical metadata fields even though the Hub already
	// guards them — the payer must not assume an implicitly-trusted Hub channel
	// (network-separation). An empty CorrelationID would otherwise skip the token's
	// correlation binding below (VerifyBound treats "" as skip) AND be echoed into
	// the response leg (respondLeg reuses it), so a downstream audit could carry an
	// empty correlation. AuthorityFrame is required for the same defense-in-depth
	// reason; the frame is additionally pinned to the literal "provider-tpo" below.
	if env.Metadata.AuthorityFrame == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing authority frame"})
		return
	}
	if env.Metadata.CorrelationID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing correlation id"})
		return
	}
	// Per-hop transport auth: the Hub's X-Hub-Assertion was verified at the top of
	// this handler; the recipient check below is cheap defense-in-depth (a misrouted
	// or directly-injected envelope addressed to someone else is rejected). The bound
	// authz token below is the AUTHORITY check (AI-11) — both are required, neither
	// substitutes for the other.
	if env.Metadata.Recipient != g.cfg.HolderID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "envelope not addressed to this holder"})
		return
	}

	// FulfillLeg dispatch: the inbound leg's expected request op + authority frame come
	// from the SAME PA catalog the origination side reads (workstream_pa.go), so the two
	// edges cannot drift. An unknown legType has no catalog entry and is rejected 400
	// BEFORE any token verification — it is not part of the protocol surface.
	spec, known := paCatalog[env.Metadata.TransactionType]
	if !known {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown transaction type"})
		return
	}

	// Per-hop transport auth: the Hub's X-Hub-Assertion was verified at the top of
	// this handler; the bound authz token below is the AUTHORITY check (AI-11) —
	// both are required, neither substitutes for the other.
	var tok shnsdk.Token
	if err := json.Unmarshal([]byte(env.Metadata.AuthzToken), &tok); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "invalid authz token"})
		return
	}
	// C2/H1: the inbound token must be bound to THIS envelope — its operation
	// (pinned per TransactionType), frame, correlationID, AND Holder (the envelope
	// Sender) must match the envelope it arrived in. This stops a valid token being
	// lifted into a different envelope/operation and replayed, and stops one holder
	// routing using another holder's token.
	if err := shnsdk.VerifyBound(tok, g.cfg.AuthzPub, g.cfg.Clock(),
		spec.ReqFrame, spec.Op, env.Metadata.CorrelationID, env.Metadata.Sender, "", sha256hex(env.Ciphertext)); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "authz verification failed"})
		return
	}

	switch env.Metadata.TransactionType {
	case "coverage-eligibility":
		g.handleEligibilityInbound(w, r, env, tok)
	case "crd-order-select":
		g.handleCRDNativeInbound(w, r, env, tok)
	case "dtr-questionnaire-fetch":
		g.handleDTRInbound(w, r, env, tok)
	case "pas-claim":
		g.handlePASNativeInbound(w, r, env, tok)
	case "pas-claim-update":
		g.handlePASUpdateNativeInbound(w, r, env, tok)
	case "federated-query":
		g.handleFederatedQueryInbound(w, r, env, tok)
	case "patient-dtr":
		g.handlePatientDTRInbound(w, r, env, tok)
	}
}

// handleEligibilityInbound is the UC-01 coverage-eligibility inbound logic,
// unchanged from the original handleInbound body.
func (g *Gateway) handleEligibilityInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token) {
	ctx := r.Context()

	cerJSON, err := shnsdk.Open(env, g.cfg.Identity.EncPub, g.cfg.Identity.EncPriv)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decryption failed"})
		return
	}

	// Data minimization at the holder boundary: bind the token subject to the
	// payload's patient via a CHEAP json.Unmarshal-level parse BEFORE any external
	// $validate. A wrong-patient payload must be rejected here so it never leaves
	// the holder boundary for the (shared) validator. The Parse* helpers operate on
	// un-$validate'd JSON and return errors (not panics) on malformed input, so a
	// malformed payload fails closed with 400 before $validate — nothing leaks.
	member, err := shnsdk.ParseEligibilityRequestMember(cerJSON)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "parse member failed"})
		return
	}

	// H2a: bind the token's subject to the payload's patient. The token authorizes
	// a specific PCI; resolving the CER's member must yield that same PCI. This
	// stops a token authorizing patient A being paired with a payload for patient B.
	pci, _, found := g.cfg.SoR.ResolvePatient(member)
	if !found {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown member"})
		return
	}
	if pci != tok.Subject {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "token subject does not match request patient"})
		return
	}

	// Only AFTER the subject binds do we ingress-validate the clinical payload via
	// the external $validate — fail-closed as before.
	ingress, err := g.cfg.Validator.Validate(ctx, cerJSON, "")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "validator unavailable"})
		return
	}
	if !ingress.Valid {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "ingress validation failed",
			"issues": ingress.Issues,
		})
		return
	}

	boundPatientRef := "Patient/" + member
	result, err := g.cfg.Responder.Handle(ctx, "coverage-eligibility", env.Metadata.CorrelationID, tok.Subject, cerJSON)
	if err != nil {
		// Handle's error return is a build/marshal fault (gateway's own) → 500, parity
		// with today's StatusInternalServerError build-failure paths. NOT 502.
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "responder failed"})
		return
	}
	// result.Status carries a connector-signalled HTTP outcome (e.g. PAS 409/422);
	// the eligibility leg never sets it today, but the shared pipeline shape checks it.
	if result.Status != 0 {
		writeJSON(w, result.Status, map[string]string{"error": result.Message})
		return
	}
	if status, msg := g.fenceResponseSubject("coverage-eligibility", boundPatientRef, result); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	// Egress $validate, behavior-identical to the pre-seam inline path: validator
	// error → 500 "validator unavailable"; !Valid → 500 "egress validation failed".
	// (NOT g.validateFHIR, which returns 422 on !Valid — that would change the
	// failure-path contract.)
	egress, err := g.cfg.Validator.Validate(ctx, result.ResponseFHIR, "")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "validator unavailable"})
		return
	}
	if !egress.Valid {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "egress validation failed"})
		return
	}

	g.respondLeg(w, r, "payer-coverage", "eligibility-response", "coverage-eligibility", env.Metadata.CorrelationID, result.ResponseFHIR, tok.Subject, env.Metadata.Sender, "")
}

// handleFederatedQueryInbound is the facility source-side handler (UC-05, consent
// backstop). The Hub-verified request token is already bound (handleInbound), but
// the facility re-enforces, independently: (1) the leg carries a consent ref; (2)
// consentsvc confirms a TREAT permit whose custodian is THIS facility and whose
// recipient is the requester (defense in depth, AI-10/AI-13); (3) the token
// subject binds to the queried patient; (4) the query is NARROW — named allowed
// types only, never bulk (FR-26, AI-1). It then returns ONLY the named records
// with a source Provenance whose .policy cites the consent (FR-32).
//
// DEF-10: single-step federated query (query and response in one Hub round-trip)
// is the connectathon topology; an async/paged query is an additive fast-follow
// (AI-9 holds: the consent check + narrowness enforcement here are unchanged when
// adding pagination, so no one-way door is taken).
func (g *Gateway) handleFederatedQueryInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token) {
	ctx := r.Context()

	queryJSON, err := shnsdk.Open(env, g.cfg.Identity.EncPub, g.cfg.Identity.EncPriv)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decryption failed"})
		return
	}

	// (1) The leg MUST carry a consent reference.
	if env.Metadata.ConsentRef == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "federated query missing consent reference"})
		return
	}

	// (4) Narrowness: parse + validate the query shape BEFORE any disclosure. A
	// missing/disallowed type or absent patient fails here (no bulk, FR-26).
	q, err := shnsdk.ParseQuery(queryJSON)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "query rejected: " + err.Error()})
		return
	}

	// (3) Bind the token subject to the queried patient.
	member := strings.TrimPrefix(q.PatientRef, "Patient/")
	pci, _, found := g.cfg.SoR.ResolvePatient(member)
	if !found {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown member"})
		return
	}
	if pci != tok.Subject {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "token subject does not match queried patient"})
		return
	}

	// (2) Consent backstop: independently confirm the four-way permit AND learn the
	// authenticated consent reference the consent service reports. The facility
	// refuses to release even if a bad token reached it. The returned ref —
	// not the provider-supplied wire field — is what anchors the Provenance below
	// (attribution integrity, FR-32/C11).
	consentRef, status, msg := g.consentBackstop(ctx, pci, env.Metadata.Sender)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	// Defense in depth: the carried wire ref must match the authenticated one, so a
	// forged Metadata.ConsentRef cannot diverge from the permit that authorized this.
	if env.Metadata.ConsentRef != consentRef {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "consent reference mismatch"})
		return
	}

	// Disclose ONLY the named records for THIS member (minimum-necessary).
	held, ok := g.cfg.SoR.FacilityRecords(member)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no records for member"})
		return
	}
	resources := make([][]byte, 0, len(q.Types)+1)
	var drRef, fallbackRef string
	for _, typ := range q.Types {
		res, present := held[typ]
		if !present {
			continue // named type the facility happens not to hold; skip (min-necessary)
		}
		if !q.InRange(recordClinicalDate(res)) {
			continue // FR-24: only disclose named records within the stated date range
		}
		if status, msg := g.validateFHIR(ctx, res, "egress"); status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
		resources = append(resources, res)
		ref, ok := resourceRef(res)
		if !ok {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "disclosed record missing id"})
			return
		}
		if fallbackRef == "" {
			fallbackRef = ref
		}
		if typ == "DiagnosticReport" {
			drRef = ref
		}
	}
	if len(resources) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "named records not held"})
		return
	}

	// Source Provenance: targets the disclosed DiagnosticReport, agent = this
	// facility, .policy cites the AUTHENTICATED consent ref (from the backstop, not
	// the wire field), reason = TREAT (FR-32/C11).
	provTarget := drRef
	if provTarget == "" {
		provTarget = fallbackRef
	}
	provJSON, err := shnsdk.BuildProvenanceWithPolicy(provTarget, "Organization/"+g.cfg.HolderID,
		consentRef, shnsdk.PurposeTreatment, g.cfg.Clock())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build provenance failed"})
		return
	}
	if status, msg := g.validateFHIR(ctx, provJSON, "egress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	resources = append(resources, provJSON)

	bundleJSON, err := shnsdk.BuildRecordsBundle(resources)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build records bundle failed"})
		return
	}

	g.respondLeg(w, r, "facility-disclosure", "federated-query-response", "federated-query", env.Metadata.CorrelationID, bundleJSON, tok.Subject, env.Metadata.Sender, consentRef)
}

// recordClinicalDate pulls the date a facility record is filtered on for FR-24:
// DiagnosticReport.effectiveDateTime or DocumentReference.date. "" if absent.
func recordClinicalDate(resJSON []byte) string {
	var r struct {
		EffectiveDateTime string `json:"effectiveDateTime"`
		Date              string `json:"date"`
	}
	_ = json.Unmarshal(resJSON, &r)
	if r.EffectiveDateTime != "" {
		return r.EffectiveDateTime
	}
	return r.Date
}

// consentBackstop independently confirms a TREAT permit (custodian = this
// facility, recipient = requester) via the Trust-operated consent service and
// returns the AUTHENTICATED consent reference the service reports — the ref that
// anchors the disclosure's source Provenance (never the provider-supplied wire
// field). When ConsentURL is unset it fails closed (no consent service ⇒ no
// disclosure). Returns (consentRef, 0, "") on permit, or ("", status, msg) on refusal.
func (g *Gateway) consentBackstop(ctx context.Context, pci, requester string) (string, int, string) {
	if g.cfg.ConsentURL == "" {
		return "", http.StatusForbidden, "consent service unavailable"
	}
	var out shnsdk.ConsentCheckResponse
	req := shnsdk.ConsentCheckRequest{PCI: pci, Purpose: shnsdk.PurposeTreatment, Custodian: g.cfg.HolderID, Recipient: requester}
	raw, err := json.Marshal(req)
	if err != nil {
		return "", http.StatusInternalServerError, "consent auth"
	}
	assertionHdr, err := g.cfg.Identity.AssertionForBody("consent", g.cfg.Clock(), shnsdk.MaxAssertionTTL, raw)
	if err != nil {
		return "", http.StatusInternalServerError, "consent auth"
	}
	hdr := map[string]string{"X-Holder-Assertion": assertionHdr}
	if err := shnsdk.PostRaw(ctx, g.cfg.Client, g.cfg.ConsentURL+"/check", raw, &out, hdr); err != nil {
		return "", http.StatusBadGateway, "consent check failed"
	}
	if !out.Permit {
		return "", http.StatusForbidden, "no active consent for this disclosure"
	}
	return out.ConsentRef, 0, ""
}

// patientDTRRequest is the provider→PHG ask: please have the patient author + attest
// this questionnaire item. It carries the patient's raw answer (entered at the patient
// surface) + the item link id + the patient ref. The PHG applies the patient
// attestation (the questionnaireresponse-signature) on the patient's authority.
type patientDTRRequest struct {
	LinkID     string `json:"linkId"`
	Answer     string `json:"answer"`
	PatientRef string `json:"patientRef"`
}

// patientDTRResponse carries the patient-authored, attested QR item back to the provider.
type patientDTRResponse struct {
	AttestedItem json.RawMessage `json:"attestedItem"`
}

// handlePatientDTRInbound is the Trust-operated PHG responder (UC-07). It binds the
// token subject to the patient, builds the patient-authored attested item REQUEST-
// SCOPED (questionnaireresponse-signature, who=Patient), and responds on the
// patient-authorship frame. The PHG persists NOTHING (OWD-8/AI-3): the patient's
// answer arrives in the request and the attested item leaves in the response.
func (g *Gateway) handlePatientDTRInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token) {
	reqJSON, err := shnsdk.Open(env, g.cfg.Identity.EncPub, g.cfg.Identity.EncPriv)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decryption failed"})
		return
	}
	var req patientDTRRequest
	if err := json.Unmarshal(reqJSON, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "parse patient-dtr request failed"})
		return
	}
	// H2: bind the token subject to the patient the request names (the patient whose
	// authorship the Trust surface is exercising).
	member := strings.TrimPrefix(req.PatientRef, "Patient/")
	pci, _, found := g.cfg.SoR.ResolvePatient(member)
	if !found {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown member"})
		return
	}
	if pci != tok.Subject {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "token subject does not match patient"})
		return
	}

	// #5: the Trust signer validates the answer against the item's constraint
	// before attesting — it must not sign a non-conformant patient answer. This
	// is the authoritative, un-bypassable guard (AI-10 fiduciary surface).
	if err := shnsdk.ValidatePatientAnswer(req.LinkID, req.Answer); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid patient answer: " + err.Error()})
		return
	}

	attested, err := shnsdk.BuildPatientAttestedItem(req.LinkID, req.Answer, req.PatientRef, g.cfg.Clock().Format("2006-01-02"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build patient-attested item failed"})
		return
	}
	respJSON, err := json.Marshal(patientDTRResponse{AttestedItem: attested})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "marshal response failed"})
		return
	}
	g.respondLeg(w, r, "patient-authorship", "patient-dtr-response", "patient-dtr", env.Metadata.CorrelationID, respJSON, tok.Subject, env.Metadata.Sender, "")
}

// verifyHubAssertion checks the X-Hub-Assertion header: a shnsdk.Assertion
// signed by the Hub's transport key with HolderID "hub" (issuer pin — a second
// Hub would join the verified contract here) and Audience == this holder, within
// bounds, jti unused. Fails closed on every malformed input.
func (g *Gateway) verifyHubAssertion(r *http.Request) bool {
	hdr := r.Header.Get("X-Hub-Assertion")
	if hdr == "" {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(hdr)
	if err != nil {
		return false
	}
	var a shnsdk.Assertion
	if err := json.Unmarshal(raw, &a); err != nil {
		return false
	}
	if a.HolderID != "hub" {
		return false
	}
	if shnsdk.VerifyAssertion(a, g.cfg.HolderID, g.cfg.HubTransportPub, g.cfg.Clock()) != nil {
		return false
	}
	return !g.hubJTI.CheckAndRecord(a.JTI, g.cfg.Clock())
}
