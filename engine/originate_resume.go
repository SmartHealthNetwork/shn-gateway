// originate_resume.go — the provider's mid-flow pend/resume machinery for the
// attestation scenarios (UC-06 clinician, UC-07 patient): run-to-PENDED, the
// single-call resume completers, and the two-phase start/complete/cancel handlers.
// Part of package gateway (the Smart Gateway runs every holder role; this file is
// the provider-origination resume surface). Behavior-preserving split of gateway.go
// (finding C); no logic change. See gateway.go for the package doc.
package engine

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// scenarioToPend runs the shared run-to-PENDED prefix for the attestation
// scenarios (UC-06/UC-07): the CRD→DTR auto-fill prefix, the PAS submit, and the
// assertion that the payer PENDED awaiting the functional-status attestation. On
// any failure it writes the HTTP error and returns ok=false. The returned
// pendState carries everything completeClinician/completePatient needs to resume.
func (g *Gateway) scenarioToPend(w http.ResponseWriter, r *http.Request, scenario, member string) (pendState, bool) {
	ctx := r.Context()
	codes := originationCodes(g.cfg.OriginationProfile)
	var o orderTuple
	switch scenario {
	case "uc06":
		o = codes.uc06
	case "uc07":
		o = codes.uc07
	default:
		o = codes.uc03 // unreachable for current callers; safe default
	}
	res, ok := g.runCRDThenDTROrder(w, r, member, o.system, o.code, o.display, o.dx, false)
	if !ok {
		return pendState{}, false
	}
	// provider-data UC-06: the operated $populate on the 0-CQL HomeHealthAssessment auto-pops
	// nothing, so org-attest the base items from the seeded order (the UC-04 lane's attestation)
	// BEFORE the pended submit. The pended QR then carries org-sourced base provenance (1.1/3.1),
	// against which completeClinician's clinician-entered functional-status item contrasts (FR-17
	// mixed provenance). Verdict-INERT — HHA is 0-CQL and br-payer's A4→A1 is its pend-resolution
	// timer. composite/sandbox and UC-07 keep res.qrJSON byte-unchanged.
	qrForSubmit := res.qrJSON
	var baseTrace map[string]string
	if g.cfg.OriginationProfile == "provider-data" && (scenario == "uc06" || scenario == "uc07") {
		answers, err := uc04AttestationAnswers(res.srJSON)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return pendState{}, false
		}
		orderRef, ok := resourceRef(res.srJSON)
		if !ok {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "order missing id"})
			return pendState{}, false
		}
		qrForSubmit, err = shnsdk.FillQuestionnaireFromAnswers(res.questionnaireJSON, answers,
			"Organization/"+g.cfg.HolderID,
			shnsdk.QRContext{PatientRef: res.patientRef, CoverageRef: res.coverageRef, OrderRef: orderRef, Authored: g.cfg.Clock()})
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "attest base questionnaire failed"})
			return pendState{}, false
		}
		if status, msg := g.validateFHIR(ctx, qrForSubmit, "egress"); status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return pendState{}, false
		}
		baseTrace = attestedAnswerValues(answers)
	}
	pasCorr := g.cfg.CorrelationGen()
	bundleJSON, err := shnsdk.BuildConformantClaimBundle(shnsdk.ConformantClaimInputs{
		QR: qrForSubmit, SR: res.srJSON, PatientRef: res.patientRef, CoverageRef: res.coverageRef,
		Corr: pasCorr, Created: g.cfg.Clock(),
		ContainedInsurer: targetsBrPayer(g.cfg.OriginationProfile),
		AbsoluteRefs:     targetsBrPayer(g.cfg.OriginationProfile),
		PayerOrgEntry:    targetsBrPayer(g.cfg.OriginationProfile), // payer Org as a resolvable PAS bundle entry (br-payer findInBundle)
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build bundle failed"})
		return pendState{}, false
	}
	if status, msg := g.validateFHIR(ctx, bundleJSON, "egress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return pendState{}, false
	}
	pendedResp, err := g.OriginateLeg(ctx, r, g.cfg.CounterpartID, "pas-claim", res.pci, pasCorr, "", Content{WorkstreamType: workstreamPA, Bytes: bundleJSON})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return pendState{}, false
	}
	if status, msg := g.validateFHIR(ctx, pendedResp, "ingress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return pendState{}, false
	}
	pended, neededItems, err := shnsdk.ParsePendedResponse(pendedResp)
	if err != nil || !pended {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "expected pended response"})
		return pendState{}, false
	}
	needed := neededItemCodes(neededItems)
	return pendState{
		scenario:    scenario,
		qrJSON:      qrForSubmit,
		srJSON:      res.srJSON,
		patientRef:  res.patientRef,
		coverageRef: res.coverageRef,
		pci:         res.pci,
		pasCorr:     pasCorr,
		filled:      res.filled,
		needed:      needed,
		qrAnswers:   baseTrace,
	}, true
}

// handleUC06 is the single-call clinician-attestation path (FR-16/17), preserved
// for the harness + make smoke + conformance: run to PENDED, then resume to
// APPROVED with the operator-supplied (or default) score + NPI in one request.
func (g *Gateway) handleUC06(w http.ResponseWriter, r *http.Request) {
	// The clinician's score + NPI may be supplied by the console attestation modal.
	// An EMPTY body is allowed (defaults below — harness/smoke); a MALFORMED body 400s.
	var body struct {
		Answer string `json:"answer"`
		NPI    string `json:"npi"`
	}
	if _, err := shnsdk.DecodeJSONBody(w, r, &body); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	st, ok := g.scenarioToPend(w, r, "uc06", g.sceneMember("MBR-UC06", "MBR-PD-UC06"))
	if !ok {
		return
	}
	g.completeClinician(w, r, st, body.Answer, body.NPI)
}

// completeClinician resumes a PENDED UC-06 PA: it amends the QR with the
// clinician-attested functional-status item (score + NPI), builds the Provenance
// (FR-32) and ClaimUpdate, exchanges it, and asserts APPROVED — writing the
// approval on success. On failure it writes the HTTP error and returns false so
// the caller keeps the resume token (the operator can retry). An empty score/NPI
// defaults to the preserved demo values ("42" / g.cfg.NPI), keeping the
// single-call path byte-identical.
func (g *Gateway) completeClinician(w http.ResponseWriter, r *http.Request, st pendState, score, npi string) bool {
	ctx := r.Context()
	srRef := "ServiceRequest/sr-uc06" // composite/sandbox literal
	linkID := oswestryLinkID
	const uc06QRID = "qr-uc06"
	// provider-data UC-06: bind to the REAL seeded order ref (not the composite literal) and attest
	// the HHA's free-text functional-status item (clinician-entered manual entry), NOT the
	// 72148/lumbar oswestry item. The attested value is operator-supplied (D-2RI-1); verdict-inert
	// (the A4→A1 is the pend-resolution timer).
	if g.cfg.OriginationProfile == "provider-data" {
		ref, ok := resourceRef(st.srJSON)
		if !ok {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "order missing id"})
			return false
		}
		srRef = ref
		linkID = hhaFunctionalStatusLinkID
		if score == "" {
			score = defaultHHAFunctionalLimitations
		}
	}
	if score == "" {
		score = "42" // preserved composite default (Oswestry score)
	}
	if npi == "" {
		npi = g.cfg.NPI
	}
	if npi == "" {
		npi = "1999999999"
	}
	itemJSON, err := shnsdk.BuildManualAttestedItem(linkID, score,
		shnsdk.Attestation{NPI: npi, Text: "I attest these are my clinical findings.", When: g.cfg.Clock().Format("2006-01-02")})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build manual item failed"})
		return false
	}
	amendedQR, err := shnsdk.AmendQRWithItem(st.qrJSON, itemJSON)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "amend qr failed"})
		return false
	}
	amendedQR, err = shnsdk.SetQuestionnaireResponseID(amendedQR, uc06QRID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "set qr id failed"})
		return false
	}
	if status, msg := g.validateFHIR(ctx, amendedQR, "egress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return false
	}
	provJSON, err := shnsdk.BuildProvenance("QuestionnaireResponse/"+uc06QRID, "Practitioner/"+npi, g.cfg.Clock())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build provenance failed"})
		return false
	}
	if status, msg := g.validateFHIR(ctx, provJSON, "egress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return false
	}
	updateCorr := g.cfg.CorrelationGen()
	// UC-06: diagnosticReport=nil; the amended QR is the supplemental data.
	updateBundle, err := shnsdk.BuildConformantClaimUpdateBundle(shnsdk.ConformantClaimUpdateInputs{
		QR: amendedQR, SR: st.srJSON, PatientRef: st.patientRef, CoverageRef: st.coverageRef,
		Provenance: provJSON, DiagnosticReport: nil, Corr: updateCorr, OriginalCorr: st.pasCorr, Created: g.cfg.Clock(),
		ContainedInsurer: targetsBrPayer(g.cfg.OriginationProfile),
		AbsoluteRefs:     targetsBrPayer(g.cfg.OriginationProfile),
		PayerOrgEntry:    targetsBrPayer(g.cfg.OriginationProfile), // payer Org as a resolvable PAS bundle entry (br-payer findInBundle)
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build update bundle failed"})
		return false
	}
	if status, msg := g.validateFHIR(ctx, updateBundle, "egress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return false
	}
	updateResp, err := g.OriginateLeg(ctx, r, g.cfg.CounterpartID, "pas-claim-update", st.pci, updateCorr, "", Content{WorkstreamType: workstreamPA, Bytes: updateBundle})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return false
	}
	if status, msg := g.validateFHIR(ctx, updateResp, "ingress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return false
	}
	// The composite amendment resolves to a genuine terminal A1 (the payer-gw polled
	// br-payer's timer A4→A1). UC-06's distinctive is DTR fill + clinician attestation → Attested.
	// AmendmentCorr is the evidence the attestation re-POST leg ran.
	parsed, approved := g.classifyResolution(updateResp)
	if !approved {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "preauthorization not approved after amendment"})
		return false
	}
	if err := g.cfg.Store.StoreAuthNumber(srRef, parsed.PreAuthRef); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "holder write failed (auth number)"})
		return false
	}
	writeJSON(w, http.StatusOK, uc03Resp{PARequired: true, AuthNumber: parsed.PreAuthRef, ValidUntil: parsed.ValidUntil, AmendmentCorr: updateCorr, QRItems: st.filled, PendedItems: st.needed, Attested: true, QRAnswers: st.qrAnswers})
	return true
}

// handleUC07 is the single-call patient-attestation path (FR-21), preserved for
// the harness + make smoke + conformance.
func (g *Gateway) handleUC07(w http.ResponseWriter, r *http.Request) {
	// The patient's answer arrives from the console patient form (default for the
	// hermetic harness). A malformed body is rejected (not silently defaulted).
	var body struct {
		Answer string `json:"answer"`
	}
	if tooLarge, err := shnsdk.DecodeJSONBody(w, r, &body); err != nil && !errors.Is(err, io.EOF) {
		if tooLarge {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	st, ok := g.scenarioToPend(w, r, "uc07", g.sceneMember("MBR-UC07", "MBR-PD-UC07"))
	if !ok {
		return
	}
	g.completePatient(w, r, st, body.Answer)
}

// completePatient resumes a PENDED UC-07 PA: it runs the patient-DTR exchange
// (provider↔Hub↔PHG) to obtain the patient-authored, signature-attested item,
// amends the QR, builds the patient-attributed Provenance (FR-32) and ClaimUpdate,
// and asserts APPROVED — writing the approval on success. On failure it writes the
// HTTP error and returns false so the caller keeps the resume token. An empty
// score defaults to "42", keeping the single-call path byte-identical.
func (g *Gateway) completePatient(w http.ResponseWriter, r *http.Request, st pendState, score string) bool {
	ctx := r.Context()
	srRef := "ServiceRequest/sr-uc07" // composite/sandbox literal
	linkID := oswestryLinkID
	const uc07QRID = "qr-uc07"
	// provider-data UC-07: bind to the REAL seeded order ref (not the composite literal) and attest
	// the HHA's free-text functional-status item (patient-entered), NOT the 72148/lumbar oswestry
	// item. The attested value is operator-supplied (D-2RI-1); verdict-inert (the A4→A1 is the
	// pend-resolution timer). Patient analog of completeClinician's provider-data branch.
	if g.cfg.OriginationProfile == "provider-data" {
		ref, ok := resourceRef(st.srJSON)
		if !ok {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "order missing id"})
			return false
		}
		srRef = ref
		linkID = hhaFunctionalStatusLinkID
		if score == "" {
			score = defaultHHAFunctionalLimitationsPatient
		}
	} else {
		if score == "" {
			score = "42" // default Oswestry score for the demo/harness
		}
		// #5: fail fast on an invalid provided score before burning the sealed
		// patient-dtr leg — a clean 400 for the console rather than an opaque round-trip
		// error. The empty→"42" demo default above is validated here too (it passes).
		if err := shnsdk.ValidatePatientAnswer(oswestryLinkID, score); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid patient answer: " + err.Error()})
			return false
		}
	}

	// --- Patient-DTR exchange: ask the Trust PHG to have the patient author + attest. ---
	phg, ok := g.cfg.Reg.LookupByRole("phg")
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "no PHG registered"})
		return false
	}
	pdReq, err := json.Marshal(patientDTRRequest{LinkID: linkID, Answer: score, PatientRef: st.patientRef})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build patient-dtr request failed"})
		return false
	}
	pdCorr := g.cfg.CorrelationGen()
	pdRespJSON, err := g.OriginateLeg(ctx, r, phg.ID, "patient-dtr", st.pci, pdCorr, "", Content{WorkstreamType: workstreamPA, Bytes: pdReq})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "patient-dtr exchange failed: " + err.Error()})
		return false
	}
	var pdResp patientDTRResponse
	if err := json.Unmarshal(pdRespJSON, &pdResp); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "parse patient-dtr response failed"})
		return false
	}

	// Amend the QR with the patient-authored attested item (FR-21).
	amendedQR, err := shnsdk.AmendQRWithItem(st.qrJSON, pdResp.AttestedItem)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "amend qr failed"})
		return false
	}
	amendedQR, err = shnsdk.SetQuestionnaireResponseID(amendedQR, uc07QRID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "set qr id failed"})
		return false
	}
	if status, msg := g.validateFHIR(ctx, amendedQR, "egress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return false
	}
	// Provenance attributes the patient-authored QR to the PATIENT (FR-32, source=patient).
	provJSON, err := shnsdk.BuildProvenance("QuestionnaireResponse/"+uc07QRID, st.patientRef, g.cfg.Clock())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build provenance failed"})
		return false
	}
	if status, msg := g.validateFHIR(ctx, provJSON, "egress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return false
	}
	updateCorr := g.cfg.CorrelationGen()
	updateBundle, err := shnsdk.BuildConformantClaimUpdateBundle(shnsdk.ConformantClaimUpdateInputs{
		QR: amendedQR, SR: st.srJSON, PatientRef: st.patientRef, CoverageRef: st.coverageRef,
		Provenance: provJSON, DiagnosticReport: nil, Corr: updateCorr, OriginalCorr: st.pasCorr, Created: g.cfg.Clock(),
		ContainedInsurer: targetsBrPayer(g.cfg.OriginationProfile),
		AbsoluteRefs:     targetsBrPayer(g.cfg.OriginationProfile),
		PayerOrgEntry:    targetsBrPayer(g.cfg.OriginationProfile), // payer Org as a resolvable PAS bundle entry (br-payer findInBundle)
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build update bundle failed"})
		return false
	}
	if status, msg := g.validateFHIR(ctx, updateBundle, "egress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return false
	}
	updateResp, err := g.OriginateLeg(ctx, r, g.cfg.CounterpartID, "pas-claim-update", st.pci, updateCorr, "", Content{WorkstreamType: workstreamPA, Bytes: updateBundle})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return false
	}
	if status, msg := g.validateFHIR(ctx, updateResp, "ingress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return false
	}
	// UC-07 is patient attestation → Attested. Composite UC-07 (L8000) → A1 approve
	// directly (no pend); the amendment-resolution path is shared with UC-04/06. AmendmentCorr is
	// the evidence the attestation re-POST leg ran.
	parsed, approved := g.classifyResolution(updateResp)
	if !approved {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "preauthorization not approved after patient attestation"})
		return false
	}
	if err := g.cfg.Store.StoreAuthNumber(srRef, parsed.PreAuthRef); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "holder write failed (auth number)"})
		return false
	}
	writeJSON(w, http.StatusOK, uc03Resp{PARequired: true, AuthNumber: parsed.PreAuthRef, ValidUntil: parsed.ValidUntil, AmendmentCorr: updateCorr, QRItems: st.filled, PendedItems: st.needed, Attested: true, QRAnswers: st.qrAnswers})
	return true
}

// startResp is the /scenario/<uc>/start JSON: the PA ran to PENDED and is parked
// under resumeToken awaiting the attestation.
type startResp struct {
	Pended      bool     `json:"pended"`
	Needed      []string `json:"needed"`
	ResumeToken string   `json:"resumeToken"`
}

// handleUC06Start runs UC-06 to PENDED and parks it under a resume token.
func (g *Gateway) handleUC06Start(w http.ResponseWriter, r *http.Request) {
	st, ok := g.scenarioToPend(w, r, "uc06", g.sceneMember("MBR-UC06", "MBR-PD-UC06"))
	if !ok {
		return
	}
	token := g.storePending(st)
	writeJSON(w, http.StatusOK, startResp{Pended: true, Needed: st.needed, ResumeToken: token})
}

// handleUC07Start runs UC-07 to PENDED and parks it under a resume token.
func (g *Gateway) handleUC07Start(w http.ResponseWriter, r *http.Request) {
	st, ok := g.scenarioToPend(w, r, "uc07", g.sceneMember("MBR-UC07", "MBR-PD-UC07"))
	if !ok {
		return
	}
	token := g.storePending(st)
	writeJSON(w, http.StatusOK, startResp{Pended: true, Needed: st.needed, ResumeToken: token})
}

// resumeReq is the body of /complete and /cancel.
type resumeReq struct {
	ResumeToken string `json:"resumeToken"`
	Answer      string `json:"answer"`
	NPI         string `json:"npi"`
}

// handleUC06Complete resumes a parked UC-06 PA with the operator's clinician
// attestation. The token is deleted only on success (a ClaimUpdate failure leaves
// it parked so the operator can retry).
func (g *Gateway) handleUC06Complete(w http.ResponseWriter, r *http.Request) {
	var body resumeReq
	if _, err := shnsdk.DecodeJSONBody(w, r, &body); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	st, ok := g.loadPending(body.ResumeToken)
	if !ok || st.scenario != "uc06" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such pending scenario"})
		return
	}
	if g.completeClinician(w, r, st, body.Answer, body.NPI) {
		g.dropPending(body.ResumeToken)
	}
}

// handleUC07Complete resumes a parked UC-07 PA with the patient's attestation.
func (g *Gateway) handleUC07Complete(w http.ResponseWriter, r *http.Request) {
	var body resumeReq
	if _, err := shnsdk.DecodeJSONBody(w, r, &body); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	st, ok := g.loadPending(body.ResumeToken)
	if !ok || st.scenario != "uc07" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such pending scenario"})
		return
	}
	if g.completePatient(w, r, st, body.Answer) {
		g.dropPending(body.ResumeToken)
	}
}

// handleScenarioCancel drops a parked PA (the PA stays PENDED at the payer; the
// demo just abandons it). Idempotent — dropping an absent token is still 200.
func (g *Gateway) handleScenarioCancel(w http.ResponseWriter, r *http.Request) {
	var body resumeReq
	if _, err := shnsdk.DecodeJSONBody(w, r, &body); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	g.dropPending(body.ResumeToken)
	writeJSON(w, http.StatusOK, map[string]bool{"cancelled": true})
}
