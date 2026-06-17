// payer.go — the payer-side PA adjudication responders (CRD cards, DTR
// questionnaire, PAS submit + update) and the shared PAS bundle-subject binding.
// Part of package gateway (the Smart Gateway runs every holder role; this file is
// the payer-adjudication surface). Behavior-preserving split of gateway.go
// (finding C); no logic change. See gateway.go for the package doc.
package engine

import (
	"encoding/json"
	"net/http"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// handleCRDInbound answers a CRD order-select: parse the CDS Hooks request,
// ingress-validate the draft ServiceRequest, derive the CPT, and respond with
// the CDS cards verdict. Cards JSON is not a FHIR resource — not $validate'd.
func (g *Gateway) handleCRDInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token) {
	ctx := r.Context()

	reqJSON, err := shnsdk.Open(env, g.cfg.Identity.EncPub, g.cfg.Identity.EncPriv)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decryption failed"})
		return
	}

	osReq, err := shnsdk.ParseOrderSelectRequest(reqJSON)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "parse order-select failed"})
		return
	}

	// Index 0 is guaranteed: ParseOrderSelectRequest rejects empty DraftOrders.
	srJSON := []byte(osReq.Context.DraftOrders[0])

	// Data minimization at the holder boundary: do the CHEAP json-level patient
	// extraction + token-subject binding (H2) BEFORE any external $validate, so a
	// wrong-patient clinical payload is rejected here and never reaches the (shared)
	// validator. The Parse* helpers operate on un-$validate'd JSON and return errors
	// (not panics) on malformed input — a malformed payload fails closed with 400
	// before $validate.
	//
	// H2: bind the token subject to the order-select patient across the WHOLE
	// payload, not just one field. The SR subject, the Coverage beneficiary, and
	// the CDS context.patientId must all reference the SAME patient; then that
	// patient must resolve to the token's subject PCI. Normalize every reference to
	// the bare member ("Patient/" stripped) before comparing, because context.
	// patientId may be sent in either form.
	srSubjectRef, err := shnsdk.ParseServiceRequestSubject(srJSON)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "parse order-select failed"})
		return
	}
	covBeneRef, err := shnsdk.ParseCoverageBeneficiary([]byte(osReq.Prefetch.Coverage))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "parse order-select failed"})
		return
	}
	srMember := strings.TrimPrefix(srSubjectRef, "Patient/")
	covMember := strings.TrimPrefix(covBeneRef, "Patient/")
	ctxMember := strings.TrimPrefix(osReq.Context.PatientID, "Patient/")
	if srMember != covMember || srMember != ctxMember {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "inconsistent patient in order-select"})
		return
	}

	pci, _, found := g.cfg.SoR.ResolvePatient(srMember)
	if !found {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown member"})
		return
	}
	if pci != tok.Subject {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "token subject does not match request patient"})
		return
	}

	// Only AFTER the subject binds do we ingress-validate the FHIR resources via
	// the external $validate — every FHIR resource crossing the substrate is
	// validated (spec §3), fail-closed as before.
	if status, msg := g.validateFHIR(ctx, srJSON, "ingress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	if len(osReq.Prefetch.Coverage) > 0 {
		if status, msg := g.validateFHIR(ctx, []byte(osReq.Prefetch.Coverage), "ingress"); status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
	}

	cpt, err := shnsdk.ParseServiceRequestCPT(srJSON)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "parse CPT failed"})
		return
	}

	paRequired, canonical := g.cfg.Adjudicator.OrderSelect(cpt)
	cardsJSON, err := shnsdk.BuildCards(paRequired, canonical)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build cards failed"})
		return
	}

	g.respondLeg(w, r, "payer-coverage", "crd-cards", "crd-order-select", env.Metadata.CorrelationID, cardsJSON, tok.Subject, env.Metadata.Sender, "")
}

// handleDTRInbound answers a DTR questionnaire fetch: parse the canonical, look
// up the Questionnaire fixture (unknown → 400), egress-validate it, and respond.
func (g *Gateway) handleDTRInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token) {
	ctx := r.Context()

	reqJSON, err := shnsdk.Open(env, g.cfg.Identity.EncPub, g.cfg.Identity.EncPriv)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decryption failed"})
		return
	}

	var fetch shnsdk.QuestionnaireFetchRequest
	if err := json.Unmarshal(reqJSON, &fetch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "parse questionnaire fetch failed"})
		return
	}

	questionnaireJSON, ok := g.cfg.Adjudicator.Questionnaire(fetch.Canonical)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown questionnaire canonical"})
		return
	}

	if status, msg := g.validateFHIR(ctx, questionnaireJSON, "egress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	g.respondLeg(w, r, "payer-coverage", "dtr-questionnaire", "dtr-questionnaire-fetch", env.Metadata.CorrelationID, questionnaireJSON, tok.Subject, env.Metadata.Sender, "")
}

// bindBundleSubject enforces PAS bundle-internal patient consistency and binds it
// to the authorized patient (H2/H3, FR-32 §5): the Claim patient must resolve to a
// known member whose PCI equals the token subject, and the ServiceRequest, the
// QuestionnaireResponse, and (when present) the supplemental DiagnosticReport must
// all reference that SAME patient. The QR subject is REQUIRED — subjectless
// evidence (a QR with no subject) could otherwise approve a Claim for a different
// patient; likewise a DiagnosticReport for patient B must not ride a Claim for
// patient A. Returns status 0 on success, or an (HTTP status, message) to write.
// Shared by handlePASInbound and handlePASUpdateInbound so this trust-critical
// check lives in ONE place — duplicated subject binding is where drift bugs live.
func (g *Gateway) bindBundleSubject(cb shnsdk.ClaimBundle, tok shnsdk.Token) (status int, msg string) {
	member := strings.TrimPrefix(cb.ClaimPatient, "Patient/")
	pci, _, found := g.cfg.SoR.ResolvePatient(member)
	if !found {
		return http.StatusBadRequest, "unknown member"
	}
	if pci != tok.Subject {
		return http.StatusForbidden, "token subject does not match request patient"
	}
	if cb.QRSubject == "" {
		return http.StatusForbidden, "PAS bundle QuestionnaireResponse missing subject"
	}
	if strings.TrimPrefix(cb.SRSubject, "Patient/") != member {
		return http.StatusForbidden, "inconsistent patient in PAS bundle"
	}
	if strings.TrimPrefix(cb.QRSubject, "Patient/") != member {
		return http.StatusForbidden, "inconsistent patient in PAS bundle"
	}
	if cb.HasDiagnosticReport && strings.TrimPrefix(cb.DiagnosticReportSubject, "Patient/") != member {
		return http.StatusForbidden, "inconsistent patient in PAS bundle"
	}
	return 0, ""
}

// handlePASInbound adjudicates a PAS preauthorization: ingress-validate the
// bundle, bind the token subject to the bundle's patient (H2a), adjudicate from
// the QR, build the ClaimResponse, egress-validate it, and respond.
func (g *Gateway) handlePASInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token) {
	ctx := r.Context()

	bundleJSON, err := shnsdk.Open(env, g.cfg.Identity.EncPub, g.cfg.Identity.EncPriv)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decryption failed"})
		return
	}

	// Data minimization at the holder boundary: do the CHEAP json-level parse +
	// token-subject binding (H2a/H2/H3) BEFORE the external $validate, so a
	// wrong-patient bundle is rejected here and never reaches the (shared)
	// validator. ParseClaimBundle operates on un-$validate'd JSON and returns
	// errors (not panics) on malformed input — a malformed bundle fails closed
	// with 400 before $validate. It parses the bundle ONCE and surfaces every
	// patient reference, so the binding below re-reads nothing.
	cb, err := shnsdk.ParseClaimBundle(bundleJSON)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "parse bundle failed"})
		return
	}

	// Bind the token subject to the WHOLE bundle (Claim==SR==QR==DiagnosticReport
	// ==token subject) BEFORE the external $validate, so a wrong-patient bundle is
	// rejected at the holder boundary. One shared helper — no drift (FR-32 §5).
	if status, msg := g.bindBundleSubject(cb, tok); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	// Only AFTER the subject binds do we ingress-validate the bundle via the
	// external $validate — fail-closed as before.
	if status, msg := g.validateFHIR(ctx, bundleJSON, "ingress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	// FR-20: pass cb.HasDiagnosticReport so the pended branch fires when the submit
	// bundle lacks an operative DiagnosticReport (prior-surgery case, UC-04). The
	// Adjudicator owns the auth-number randomness (sandbox: crypto/rand, unguessable).
	dec, err := g.cfg.Adjudicator.PriorAuth(cb.QRJSON, cb.HasDiagnosticReport)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}

	switch dec.Outcome {
	case shnsdk.PASPended:
		// FR-20: build the pended response (Bundle with ClaimResponse+Task) and
		// respond so the provider can attach supplemental data for exchange-2.
		pendedJSON, err := shnsdk.BuildPendedResponse(cb.ClaimPatient, env.Metadata.CorrelationID, dec.NeededItems, g.cfg.Clock())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build pended response failed"})
			return
		}
		if status, msg := g.validateFHIR(ctx, pendedJSON, "egress"); status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
		// review-fixes-6 #1: build the response leg BEFORE committing payer state,
		// so a response-leg failure (unknown requester, seal, encode) cannot orphan
		// the pended-claim ledger. The only irreversible residual is a failure of the
		// final HTTP write in writeLeg AFTER the commit, which needs an outbox/ack
		// model (deferred).
		//
		// FR-21/FR-6: record this pended claim (payer-local, metadata-only) so the
		// follow-up ClaimUpdate can be bound to a REAL prior pend (see
		// handlePASUpdateInbound). Keyed by subject PCI + this exchange's correlation.
		respBytes, status, msg := g.buildResponseLeg(r, "payer-coverage", "pas-response", "pas-claim", env.Metadata.CorrelationID, pendedJSON, tok.Subject, env.Metadata.Sender, "")
		if status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
		if err := g.cfg.Store.RecordPendedClaim(tok.Subject, env.Metadata.CorrelationID); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "holder write failed (pended claim)"})
			return
		}
		writeLeg(w, respBytes)

	case shnsdk.PASApproved:
		crJSON, err := shnsdk.BuildClaimResponse(dec.PreAuthRef, dec.ValidUntil, cb.ClaimPatient, env.Metadata.CorrelationID, g.cfg.Clock())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build claim response failed"})
			return
		}
		if status, msg := g.validateFHIR(ctx, crJSON, "egress"); status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
		// FR-28: build the PDex PA EOB for the approved decision and store it in the
		// payer's own decision state (AI-1-compatible), readable via the Patient
		// Access API — mirrors the denied branch so the patient's Smart Health
		// account can show APPROVED prior authorizations, carrying the auth number.
		eobJSON, err := shnsdk.BuildPADecisionEOB(shnsdk.PADecisionEOBParams{
			ID:          "eob-" + env.Metadata.CorrelationID,
			PatientRef:  cb.ClaimPatient,
			CoverageRef: "Coverage/" + strings.TrimPrefix(cb.ClaimPatient, "Patient/"),
			CPTCode:     "72148",
			Decision:    shnsdk.PADecisionApproved,
			AuthNumber:  dec.PreAuthRef,
			Created:     g.cfg.Clock(),
		})
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build EOB failed"})
			return
		}
		if status, msg := g.validateFHIR(ctx, eobJSON, "egress"); status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
		// review-fixes-6 #1: build the response leg BEFORE committing payer state
		// so a response-leg failure (unknown requester, seal, encode) cannot orphan
		// the EOB. The residual write-after-commit gap is the deferred outbox/ack.
		respBytes, status, msg := g.buildResponseLeg(r, "payer-coverage", "pas-response", "pas-claim", env.Metadata.CorrelationID, crJSON, tok.Subject, env.Metadata.Sender, "")
		if status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
		if err := g.cfg.Store.RecordEOB(tok.Subject, "eob-"+env.Metadata.CorrelationID, eobJSON); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "holder write failed (eob)"})
			return
		}
		writeLeg(w, respBytes)

	default: // shnsdk.PASDenied
		// FR-22 (UC-08): a real denied ClaimResponse with the PAS reviewAction (A3),
		// rationale, appeal window, and peer-to-peer instruction.
		rationale := dec.DenyReason
		if rationale == "" {
			rationale = "Conservative therapy of at least 6 weeks is not documented (4 weeks on record); request does not meet the payer's medical-necessity policy for advanced lumbar imaging."
		}
		denJSON, err := shnsdk.BuildDeniedResponse(cb.ClaimPatient, env.Metadata.CorrelationID, rationale, g.cfg.Clock())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build denied response failed"})
			return
		}
		if status, msg := g.validateFHIR(ctx, denJSON, "egress"); status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
		// FR-28: build the PDex PA EOB for the patient surface and store it in the
		// payer's own decision state (AI-1-compatible), readable via the Patient
		// Access API. Coverage ref derived from the patient ref by stripping the
		// "Patient/" prefix and using "Coverage/" as the prefix (payer-local ref).
		eobJSON, err := shnsdk.BuildPADecisionEOB(shnsdk.PADecisionEOBParams{
			ID:          "eob-" + env.Metadata.CorrelationID,
			PatientRef:  cb.ClaimPatient,
			CoverageRef: "Coverage/" + strings.TrimPrefix(cb.ClaimPatient, "Patient/"),
			CPTCode:     "72148",
			Decision:    shnsdk.PADecisionDenied,
			AuthNumber:  "",
			Created:     g.cfg.Clock(),
		})
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build EOB failed"})
			return
		}
		if status, msg := g.validateFHIR(ctx, eobJSON, "egress"); status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
		// review-fixes-6 #1: build the response leg BEFORE committing payer state
		// so a response-leg failure (unknown requester, seal, encode) cannot orphan
		// the EOB. The residual write-after-commit gap is the deferred outbox/ack.
		respBytes, status, msg := g.buildResponseLeg(r, "payer-coverage", "pas-response", "pas-claim", env.Metadata.CorrelationID, denJSON, tok.Subject, env.Metadata.Sender, "")
		if status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
		if err := g.cfg.Store.RecordEOB(tok.Subject, "eob-"+env.Metadata.CorrelationID, eobJSON); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "holder write failed (eob)"})
			return
		}
		writeLeg(w, respBytes)
	}
}

// handlePASUpdateInbound adjudicates a PAS ClaimUpdate amendment (FR-21): it parses
// the amended bundle (which carries supplemental data + Provenance), binds the token
// subject across the bundle, REQUIRES a Provenance (FR-32), then RE-ADJUDICATES the
// now-complete bundle — which must approve (else the amendment is still insufficient).
func (g *Gateway) handlePASUpdateInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token) {
	ctx := r.Context()

	bundleJSON, err := shnsdk.Open(env, g.cfg.Identity.EncPub, g.cfg.Identity.EncPriv)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decryption failed"})
		return
	}

	cb, err := shnsdk.ParseClaimBundle(bundleJSON)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "parse bundle failed"})
		return
	}

	// Bind subject across the WHOLE amended bundle (Claim==SR==QR==DiagnosticReport
	// ==token subject) BEFORE the pend lock, so a wrong-subject token is rejected
	// with 403 (not 409) and the atomic ledger is never touched for tokens that
	// do not belong to this patient (AI-11, H2a). Same shared check as submit.
	// For UC-04 this is exactly where a mismatched supplemental DiagnosticReport
	// (patient B's report on patient A's Claim) is also rejected (FR-32 §5).
	if status, msg := g.bindBundleSubject(cb, tok); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	// FR-21 + FR-6: a ClaimUpdate must reference the original request (Claim.related)
	// AND that original must be a REAL claim this payer actually pended for THIS
	// patient — authority is evaluated against current payer state and the
	// pended→approved transition is genuine. The payer-local pended-claim ledger
	// (metadata only, AI-1-compatible) enforces this.
	if cb.RelatedClaim == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "ClaimUpdate missing original-claim reference (Claim.related)"})
		return
	}
	// ATOMIC claim: this single test-and-set is the current-state authority check AND
	// the serialization point — only one update can be in flight for a given pended
	// claim. No prior pend, a replay of an already-approved update, or a second
	// concurrent update all get false → 409.
	claimed, err := g.cfg.Store.BeginClaimUpdate(tok.Subject, cb.RelatedClaim)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "holder write failed (begin update)"})
		return
	}
	if !claimed {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "ClaimUpdate references no pending claim available for this patient"})
		return
	}
	// Release the claim back to pended unless this update actually approves (set just
	// before the approved response), so a mid-adjudication failure or an insufficient
	// amendment never strands the claim and a later complete amendment can transition it.
	approved := false
	defer func() {
		if !approved {
			_ = g.cfg.Store.ReleaseClaimUpdate(tok.Subject, cb.RelatedClaim)
		}
	}()

	// FR-32: a ClaimUpdate MUST carry Provenance ATTRIBUTING the supplemental data —
	// not merely present. The Provenance must name an agent AND target the EXACT
	// supplemental resource in this bundle (resolved by id), not just any resource of
	// the right type: the DiagnosticReport (UC-04) or, when there is none, the
	// QuestionnaireResponse (UC-06). A Provenance for an unrelated/wrong-id resource,
	// or with no agent, does not attribute the evidence and is rejected.
	if cb.ProvenanceJSON == nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "ClaimUpdate missing Provenance"})
		return
	}
	if len(cb.ProvenanceAgents) == 0 {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "ClaimUpdate Provenance missing agent"})
		return
	}
	var wantTarget string
	if cb.HasDiagnosticReport {
		if cb.DiagnosticReportID == "" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "supplemental DiagnosticReport missing id"})
			return
		}
		wantTarget = "DiagnosticReport/" + cb.DiagnosticReportID
	} else {
		if cb.QRID == "" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "supplemental QuestionnaireResponse missing id"})
			return
		}
		wantTarget = "QuestionnaireResponse/" + cb.QRID
	}
	targeted := false
	for _, ref := range cb.ProvenanceTargets {
		if ref == wantTarget {
			targeted = true
			break
		}
	}
	if !targeted {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "ClaimUpdate Provenance does not target the supplemental data"})
		return
	}

	if status, msg := g.validateFHIR(ctx, bundleJSON, "ingress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	dec, err := g.cfg.Adjudicator.PriorAuth(cb.QRJSON, cb.HasDiagnosticReport)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	if dec.Outcome != shnsdk.PASApproved {
		// Still insufficient: the deferred ReleaseClaimUpdate returns the claim to
		// pended so a later, complete amendment can still transition it.
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "amendment still insufficient"})
		return
	}

	crJSON, err := shnsdk.BuildClaimResponse(dec.PreAuthRef, dec.ValidUntil, cb.ClaimPatient, env.Metadata.CorrelationID, g.cfg.Clock())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build claim response failed"})
		return
	}
	if status, msg := g.validateFHIR(ctx, crJSON, "egress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	// review-fixes-6 #1: build the response leg BEFORE finalizing. If the build
	// fails, we return with approved still false, so the deferred ReleaseClaimUpdate
	// returns the claim to pended (the provider can retry) — no finalized-but-
	// unanswered claim. The residual outbox/ack gap (write failure after finalize)
	// is deferred.
	//
	// FR-21: only NOW — after the approved ClaimResponse is built and egress-validated
	// — complete the pended→approved transition. Finalize (remove) the claim so a
	// replayed update no longer finds it, and mark approved so the deferred release
	// does not run. A failure ABOVE this point leaves the claim claimable again
	// (defer releases it back to pended), so the provider can retry.
	respBytes, status, msg := g.buildResponseLeg(r, "payer-coverage", "pas-update-response", "pas-claim-update", env.Metadata.CorrelationID, crJSON, tok.Subject, env.Metadata.Sender, "")
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	if err := g.cfg.Store.FinalizeClaimUpdate(tok.Subject, cb.RelatedClaim); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "holder write failed (finalize update)"})
		return
	}
	approved = true
	writeLeg(w, respBytes)
}
