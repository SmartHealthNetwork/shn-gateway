// patientaccess.go — the CMS-0057 Patient Access REST edge (FR-28). Part of package
// gateway (the Smart Gateway runs every holder role; this file is the payer's
// patient-access surface). Behavior-preserving split of gateway.go (finding C); no
// logic change. See gateway.go for the package doc.
package engine

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// ---- Payer Patient Access API (CMS-0057, FR-28) ----

// handlePatientAccessMetadata serves the Patient Access CapabilityStatement at the
// standard FHIR conformance endpoint (FR-37). A conformance statement is public
// (like any FHIR /metadata); the EOB reads it describes are per-operation
// patient-access-token gated.
func (g *Gateway) handlePatientAccessMetadata(w http.ResponseWriter, _ *http.Request) {
	cs, err := shnsdk.BuildPatientAccessCapabilityStatement(g.cfg.Clock())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build capabilitystatement failed"})
		return
	}
	w.Header().Set("Content-Type", "application/fhir+json")
	w.WriteHeader(http.StatusOK)
	w.Write(cs) //nolint:errcheck
}

// handlePatientAccessEOB is the payer's CMS-0057 Patient Access API read (FR-28):
// GET /ExplanationOfBenefit?patient={pci} (searchset) or ?_id={eobID} (read). It
// is NOT a sealed substrate leg — it is a conformant FHIR read, gated by a
// patient-access authority token (per-op, subject-bound) presented in the
// Authorization header (Bearer <token-json-base64>). The payer verifies the token
// (signature + frame patient-access + op patient-access-read + subject == requested
// patient) and audits the read. The patient surface (PHG) holds nothing; the payer
// is the authoritative source of its own decisions (AI-1/AI-3, FR-33).
func (g *Gateway) handlePatientAccessEOB(w http.ResponseWriter, r *http.Request) {
	tok, ok := g.patientAccessToken(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or invalid patient-access token"})
		return
	}
	// FHIR search: GET /ExplanationOfBenefit?patient={pci}. Subject binding — the
	// token authorizes exactly one PCI, so the requested patient must equal it.
	patient := r.URL.Query().Get("patient")
	if patient != "" && patient != tok.Subject {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "token subject does not match requested patient"})
		return
	}
	eobs, found := g.cfg.Store.EOBsForPatient(tok.Subject)
	if !found {
		eobs = nil // an empty searchset is a valid response (no PA decisions yet)
	}
	// CapabilityStatement advertises _id search for ExplanationOfBenefit (FR-37).
	// Filter over the SUBJECT's slice only — never a cross-patient read-by-id.
	// Subject-scoping is already established by EOBsForPatient above (AI-1/AI-3).
	if id := r.URL.Query().Get("_id"); id != "" {
		var filtered [][]byte
		for _, e := range eobs {
			if eobResourceID(e) == id {
				filtered = append(filtered, e)
			}
		}
		eobs = filtered // an _id outside the subject's EOBs yields an empty searchset
	}
	// Reuse shnsdk.BuildRecordsBundle: it emits a type=searchset Bundle, exactly
	// the PDex Patient Access search response shape (FR-28).
	out, err := shnsdk.BuildRecordsBundle(eobs)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build bundle failed"})
		return
	}
	g.serveEOB(w, r, tok.Subject, out)
}

// handlePatientAccessEOBByID is the FHIR-canonical instance read
// GET /ExplanationOfBenefit/{id} (CMS-0057 Patient Access, FR-28). Same
// patient-access token gate + subject binding as search; the EOB must belong to
// the token subject.
func (g *Gateway) handlePatientAccessEOBByID(w http.ResponseWriter, r *http.Request) {
	tok, ok := g.patientAccessToken(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or invalid patient-access token"})
		return
	}
	eob, found := g.cfg.Store.EOBByID(r.PathValue("id"))
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such ExplanationOfBenefit"})
		return
	}
	// Subject confinement: the instance must be one of the token subject's own EOBs.
	eobsForSubject, _ := g.cfg.Store.EOBsForPatient(tok.Subject)
	if !containsBytes(eobsForSubject, eob) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "ExplanationOfBenefit not for this patient"})
		return
	}
	g.serveEOB(w, r, tok.Subject, eob)
}

// serveEOB validates the served FHIR body, audits the read with the resulting
// outcome (MANDATORY — §10 every-operation-audited, FR-33), and only THEN writes
// the body. Order is validate → audit → write: a payload that fails egress
// validation is audited as a REJECTED read (never a false "answered") and is never
// disclosed (fail closed). A gateway that mounts the Patient Access API with NO
// AuditURL has no audit capability, so the read is disabled (502) rather than
// served unaudited. If the audit append itself fails the read is not served (502),
// consistent with the Hub's audit-before-forward model.
func (g *Gateway) serveEOB(w http.ResponseWriter, r *http.Request, subjectPCI string, out []byte) {
	if g.cfg.AuditURL == "" {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "patient-access read unavailable: no audit plane configured"})
		return
	}
	// FR-28 / two-gate posture: validate the served searchset (US-Core/base — NOT
	// Da Vinci) BEFORE auditing, so the audit outcome reflects whether the read
	// actually succeeded.
	status, msg := g.validateFHIR(r.Context(), out, "egress")
	outcome := "answered"
	if status != 0 {
		outcome = "rejected"
	}
	// §10: BOTH the disclosed read and the rejected read are recorded. If the audit
	// cannot be written the read is not served (502).
	if err := g.auditPatientAccess(r.Context(), subjectPCI, outcome); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "patient-access read audit failed"})
		return
	}
	if status != 0 { // validation failed — audited as rejected, fail closed (no disclosure)
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	w.Header().Set("Content-Type", "application/fhir+json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// patientAccessToken extracts + verifies a patient-access token from the
// Authorization: Bearer <base64(token-json)> header. Returns ok=false on any
// failure (missing header, decode, signature/expiry, wrong frame/op).
func (g *Gateway) patientAccessToken(r *http.Request) (shnsdk.Token, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return shnsdk.Token{}, false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, prefix))
	if err != nil {
		return shnsdk.Token{}, false
	}
	var tok shnsdk.Token
	if err := json.Unmarshal(raw, &tok); err != nil {
		return shnsdk.Token{}, false
	}
	// Pin the authenticated holder to the PHG (the only role OPA permits for
	// patient-access-read) and require a non-empty subject — the subject is bound
	// to the returned EOB downstream, so an empty subject must never reach that path.
	// patient-access-read is the ONE non-envelope op: it carries NO payload hash, so
	// it uses VerifyBoundNoPayload (which also asserts no hash is present, so an
	// envelope-bound token can't be replayed onto this bearer read path — AI-2).
	if err := shnsdk.VerifyBoundNoPayload(tok, g.cfg.AuthzPub, g.cfg.Clock(),
		"patient-access", "patient-access-read", "", "phg", ""); err != nil {
		return shnsdk.Token{}, false
	}
	if tok.Subject == "" {
		return shnsdk.Token{}, false
	}
	// #4: bind the token to one read. Require a correlation (the PHG always mints
	// one) and consume it once — a captured/replayed bearer token re-presents a
	// seen correlation and is rejected (the Hub's replay guard does not cover this
	// direct REST read). AI-11 per-leg/correlation binding at the patient surface.
	if tok.CorrelationID == "" {
		return shnsdk.Token{}, false
	}
	if g.paReplay.CheckAndRecord(tok.CorrelationID, g.cfg.Clock()) {
		return shnsdk.Token{}, false
	}
	return tok, true
}

// eobResourceID parses an ExplanationOfBenefit's resource id for the FHIR _id
// search param. Returns "" if absent/unparseable (matching no _id query value).
// Used by handlePatientAccessEOB to filter over the subject's own EOBs only
// (never a cross-patient read-by-id). AI-1/AI-3, FR-28.
func eobResourceID(eobJSON []byte) string {
	var x struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(eobJSON, &x)
	return x.ID
}

// paReplayWindow is how long a patient-access correlationId is remembered for
// replay rejection on the direct read. It equals the token TTL so a captured token
// cannot be replayed for the life of its token (a token older than the window has
// already failed the expiry check). paReplayMaxEntries bounds the set so a chatty
// caller cannot grow it without bound.
const paReplayWindow = time.Hour
const paReplayMaxEntries = 1 << 16 // 65536

// containsBytes returns true if set contains an entry with identical bytes to b.
func containsBytes(set [][]byte, b []byte) bool {
	for _, x := range set {
		if bytes.Equal(x, b) {
			return true
		}
	}
	return false
}

// auditPatientAccess appends a metadata-only "patient-access-read" record to the
// Audit Plane so the read surfaces in the patient projection (FR-29/FR-33), signed
// with the payer's holder key (an authorized audit signer in harness/devstack). It
// is MANDATORY, not best-effort: §10 requires EVERY operation to produce a
// tamper-evident AuditEvent, so — consistent with the Hub's audit-before-forward
// model — the read is NOT served if it cannot be audited (serveEOB returns 502
// before disclosing). The AuditURL=="" no-op branch below is defensive: serveEOB
// already rejects the read when no Audit Plane is configured, so on the read path
// this function is never called with an empty AuditURL.
func (g *Gateway) auditPatientAccess(ctx context.Context, subjectPCI, outcome string) error {
	if g.cfg.AuditURL == "" {
		return nil
	}
	rec := shnsdk.AuditRecord{
		Timestamp:       g.cfg.Clock().Format(time.RFC3339),
		Sender:          "phg",
		Recipient:       g.cfg.HolderID,
		TransactionType: "patient-access-read",
		AuthorityFrame:  "patient-access",
		Scope:           "patient-access-only",
		Outcome:         outcome,
		SubjectPCI:      subjectPCI,
	}
	sig := ed25519.Sign(g.cfg.Identity.SignPriv, shnsdk.SignableContent(rec))
	appendReq := shnsdk.AuditAppendRequest{AuditRecord: rec, Signatures: []string{base64.StdEncoding.EncodeToString(sig)}}
	return shnsdk.PostJSON(ctx, g.cfg.Client, g.cfg.AuditURL+"/append", appendReq, nil, nil)
}
