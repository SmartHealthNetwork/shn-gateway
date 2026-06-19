// payer.go — the payer-side PA adjudication responders (CRD cards, DTR
// questionnaire, PAS submit + update) and the shared PAS bundle-subject binding.
// Part of package gateway (the Smart Gateway runs every holder role; this file is
// the payer-adjudication surface). Behavior-preserving split of gateway.go
// (finding C); no logic change. See gateway.go for the package doc.
package engine

import (
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

	result, err := g.cfg.Responder.Handle(ctx, "crd-order-select", env.Metadata.CorrelationID, tok.Subject, reqJSON)
	if err != nil {
		// Handle's error return is a build/marshal fault (gateway's own) → 500.
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "responder failed"})
		return
	}
	if result.Status != 0 {
		// e.g. 400 "parse CPT failed" — surfaced by the connector, not an error.
		writeJSON(w, result.Status, map[string]string{"error": result.Message})
		return
	}
	// CRD cards are not a FHIR resource: no (C) fence, no egress-$validate.
	g.respondLeg(w, r, "payer-coverage", "crd-cards", "crd-order-select", env.Metadata.CorrelationID, result.ResponseFHIR, tok.Subject, env.Metadata.Sender, "")
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

	result, err := g.cfg.Responder.Handle(ctx, "dtr-questionnaire-fetch", env.Metadata.CorrelationID, tok.Subject, reqJSON)
	if err != nil {
		// build/marshal fault (gateway's own) → 500
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "responder failed"})
		return
	}
	if result.Status != 0 {
		// e.g. 400 "unknown questionnaire canonical"
		writeJSON(w, result.Status, map[string]string{"error": result.Message})
		return
	}
	if status, msg := g.fenceResponseSubject("dtr-questionnaire-fetch", "", result); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	if status, msg := g.validateFHIR(ctx, result.ResponseFHIR, "egress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	g.respondLeg(w, r, "payer-coverage", "dtr-questionnaire", "dtr-questionnaire-fetch", env.Metadata.CorrelationID, result.ResponseFHIR, tok.Subject, env.Metadata.Sender, "")
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

// fenceResponseSubject is the (C) outbound fence: a connector must not swap the
// patient between the request it was handed and the response it returned. It
// compares the response resource's patient ref to boundPatientRef (the inbound
// member-namespace ref, e.g. "Patient/<member>", already proven by (A) to resolve
// to pci == tok.Subject). NOT compared to tok.Subject, which is a derived PCI.
// Returns (0,"") on pass or (status, msg) to write. Per-leg arms are added as each
// leg moves behind the seam.
func (g *Gateway) fenceResponseSubject(leg, boundPatientRef string, res LegResult) (int, string) {
	switch leg {
	case "coverage-eligibility":
		ref, err := ParseCoverageEligibilityResponsePatient(res.ResponseFHIR)
		if err != nil {
			return http.StatusInternalServerError, "parse response subject failed"
		}
		if ref != boundPatientRef {
			return http.StatusForbidden, "response patient does not match request patient"
		}
	case "crd-order-select":
		// cards are not FHIR / patient-agnostic — no outbound subject to fence.
	case "dtr-questionnaire-fetch":
		// §6.2: the response is a $questionnaire-package Bundle; walk its Questionnaire
		// entries (the wrapper has no top-level subject) and reject if any carries one.
		if packageQuestionnaireHasSubject(res.ResponseFHIR) {
			return http.StatusForbidden, "questionnaire response unexpectedly carries a subject"
		}
	case "pas-claim", "pas-claim-update":
		// The sealed ClaimResponse leg: every ClaimResponse patient (bare CR for
		// approved/denied, the Bundle's CR for pended) must equal the bound request
		// patient. (pas-claim-update is included now so the update leg needs no fence
		// change when it moves behind the seam; it is harmless until then.)
		refs, err := ParsePASResponsePatients(res.ResponseFHIR)
		if err != nil {
			return http.StatusInternalServerError, "parse response subject failed"
		}
		for _, ref := range refs {
			if ref != boundPatientRef {
				return http.StatusForbidden, "response patient does not match request patient"
			}
		}
		// The EOB Store side-effect (RecordEOB) never travels a sealed leg, so the
		// response check above cannot see it — fence its subject here too.
		for _, se := range res.SideEffectFHIR {
			ref, err := parseEOBPatient(se)
			if err != nil {
				return http.StatusInternalServerError, "parse side-effect subject failed"
			}
			if ref != boundPatientRef {
				return http.StatusForbidden, "side-effect patient does not match request patient"
			}
		}
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

	// The decision/build/branch tail now lives behind the LegResponder seam
	// (sandboxResponder.Handle "pas-claim"): the connector owns the PriorAuth
	// decision, the three response builds (pended/approved/denied), the EOB
	// side-effect, and the Store-write Commit. The engine keeps authority (the
	// (A)/(B) bindBundleSubject above + the (C) fenceResponseSubject below),
	// sealing, edge-$validate, and audit. This is the first MUTATING leg: the EOB
	// surfaces as result.SideEffectFHIR (egress-$validated before Commit, FR-36),
	// and result.Commit does the Store write (RecordEOB/RecordPendedClaim) AFTER
	// buildResponseLeg and BEFORE writeLeg — exactly today's
	// buildResponseLeg → RecordEOB/RecordPended → writeLeg ordering.
	boundPatientRef := cb.ClaimPatient
	result, err := g.cfg.Responder.Handle(ctx, "pas-claim", env.Metadata.CorrelationID, tok.Subject, bundleJSON)
	if err != nil {
		// build/marshal fault (gateway's own) → 500.
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "responder failed"})
		return
	}
	// Arm defer-rollback-unless-committed: a claim acquired in Handle is released
	// on any pre-commit early return. (Submit acquires no claim today — Rollback is
	// nil — but the seam carries it for the update leg.)
	committed := false
	defer func() {
		if !committed && result.Rollback != nil {
			result.Rollback()
		}
	}()
	if result.Status != 0 {
		// e.g. 422 from a PriorAuth decision error.
		writeJSON(w, result.Status, map[string]string{"error": result.Message})
		return
	}
	// (C) outbound fence: the connector must not have swapped the patient between
	// the request and the response/side-effect. Checks the ClaimResponse subject(s)
	// AND the EOB side-effect against boundPatientRef.
	if status, msg := g.fenceResponseSubject("pas-claim", boundPatientRef, result); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	// Egress-$validate the response AND every side-effect (the EOB) before the Store
	// write — matching today's per-branch egress-validate of the response then the
	// EOB before RecordEOB. PAS egress = g.validateFHIR (422 on a profile failure).
	for _, b := range append([][]byte{result.ResponseFHIR}, result.SideEffectFHIR...) {
		if status, msg := g.validateFHIR(ctx, b, "egress"); status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
	}
	// review-fixes-6 #1: build the response leg BEFORE committing payer state, so a
	// response-leg failure (unknown requester, seal, encode) cannot orphan the EOB /
	// pended-claim ledger. The only irreversible residual is a failure of the final
	// HTTP write in writeLeg AFTER the commit (deferred outbox/ack).
	respBytes, status, msg := g.buildResponseLeg(r, "payer-coverage", "pas-response", "pas-claim", env.Metadata.CorrelationID, result.ResponseFHIR, tok.Subject, env.Metadata.Sender, "")
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	if result.Commit != nil {
		if err := result.Commit(); err != nil {
			// Store-write failure → 502 (parity with today's RecordEOB/RecordPended 502).
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "holder write failed"})
			return
		}
	}
	committed = true
	writeLeg(w, respBytes)
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
	// (metadata only, AI-1-compatible) enforces this. The Claim.related PRESENCE check
	// stays engine-side (it is an (A)/(B) authority precondition); the atomic
	// BeginClaimUpdate test-and-set now runs INSIDE the connector's Handle (its own
	// ledger serialization point) — see sandboxResponder "pas-claim-update".
	if cb.RelatedClaim == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "ClaimUpdate missing original-claim reference (Claim.related)"})
		return
	}

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

	// The decision/build tail + the pended ledger now live behind the LegResponder seam
	// (sandboxResponder.Handle "pas-claim-update"): the connector owns the atomic
	// BeginClaimUpdate (pre-decision serialization), the PriorAuth re-adjudication, the
	// approved ClaimResponse build, and the Store transition (Commit=FinalizeClaimUpdate,
	// Rollback=ReleaseClaimUpdate). The engine keeps authority (the (A)/(B) checks above +
	// the (C) fence below), sealing, edge-$validate, and audit. The update leg builds NO
	// EOB (only submit does) and carries NO SideEffectFHIR — the egress-$validate is just
	// result.ResponseFHIR.
	boundPatientRef := cb.ClaimPatient
	result, err := g.cfg.Responder.Handle(ctx, "pas-claim-update", env.Metadata.CorrelationID, tok.Subject, bundleJSON)
	// Arm defer-rollback-unless-committed on the returned result BEFORE checking err: a
	// BuildClaimResponse error returns LegResult{Rollback: release}, err — so the claim
	// acquired in BeginClaimUpdate is still released by this defer. This is the subtlest
	// correctness point of the refactor.
	committed := false
	defer func() {
		if !committed && result.Rollback != nil {
			result.Rollback()
		}
	}()
	if err != nil {
		// build/marshal fault (gateway's own) → 500; the defer above still releases the
		// claim because result.Rollback was set alongside the error.
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "responder failed"})
		return
	}
	if result.Status != 0 {
		// 409 (no pending claim), 422 (insufficient amendment / PriorAuth decision error),
		// or 502 (begin-update store fail). Insufficient/PriorAuth-error already released
		// the claim via the defer (Rollback set); the 409 acquired nothing (no Rollback).
		writeJSON(w, result.Status, map[string]string{"error": result.Message})
		return
	}
	// (C) outbound fence: the connector must not have swapped the patient between the
	// request and the response. Same fence arm as submit (pas-claim/pas-claim-update).
	if status, msg := g.fenceResponseSubject("pas-claim-update", boundPatientRef, result); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	if status, msg := g.validateFHIR(ctx, result.ResponseFHIR, "egress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	// review-fixes-6 #1: build the response leg BEFORE committing (FinalizeClaimUpdate). If
	// the build fails we return with committed still false, so the deferred Rollback
	// (ReleaseClaimUpdate) returns the claim to pended (the provider can retry) — no
	// finalized-but-unanswered claim. The residual outbox/ack gap (write failure after
	// finalize) is deferred — exactly today's buildResponseLeg → FinalizeClaimUpdate →
	// writeLeg ordering.
	respBytes, status, msg := g.buildResponseLeg(r, "payer-coverage", "pas-update-response", "pas-claim-update", env.Metadata.CorrelationID, result.ResponseFHIR, tok.Subject, env.Metadata.Sender, "")
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	if result.Commit != nil {
		if err := result.Commit(); err != nil {
			// FinalizeClaimUpdate store-write failure → 502 (parity with today).
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "holder write failed (finalize update)"})
			return
		}
	}
	committed = true
	writeLeg(w, respBytes)
}
