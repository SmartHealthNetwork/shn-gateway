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
	res, ok := g.runCRDThenDTR(w, r, member)
	if !ok {
		return pendState{}, false
	}
	pasCorr := g.cfg.CorrelationGen()
	bundleJSON, err := shnsdk.BuildClaimBundle(res.qrJSON, res.srJSON, res.patientRef, res.coverageRef, pasCorr, g.cfg.Clock())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build bundle failed"})
		return pendState{}, false
	}
	if status, msg := g.validateFHIR(ctx, bundleJSON, "egress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return pendState{}, false
	}
	pendedResp, err := g.roundTrip(ctx, r, g.cfg.CounterpartID, "provider-tpo", "payer-coverage", "pas-submit", "pas-response", "pas-claim", "pas-bundle", res.pci, pasCorr, "", bundleJSON)
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
		qrJSON:      res.qrJSON,
		srJSON:      res.srJSON,
		patientRef:  res.patientRef,
		coverageRef: res.coverageRef,
		pci:         res.pci,
		pasCorr:     pasCorr,
		filled:      res.filled,
		needed:      needed,
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
	st, ok := g.scenarioToPend(w, r, "uc06", "MBR-UC06")
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
	const srRef = "ServiceRequest/sr-uc06"
	const uc06QRID = "qr-uc06"
	if score == "" {
		score = "42" // preserved default
	}
	if npi == "" {
		npi = g.cfg.NPI
	}
	if npi == "" {
		npi = "1999999999"
	}
	itemJSON, err := shnsdk.BuildManualAttestedItem(oswestryLinkID, score,
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
	updateBundle, err := shnsdk.BuildClaimUpdateBundle(amendedQR, st.srJSON, nil, provJSON, st.patientRef, st.coverageRef, updateCorr, st.pasCorr, g.cfg.Clock())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build update bundle failed"})
		return false
	}
	if status, msg := g.validateFHIR(ctx, updateBundle, "egress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return false
	}
	updateResp, err := g.roundTrip(ctx, r, g.cfg.CounterpartID, "provider-tpo", "payer-coverage", "pas-update-submit", "pas-update-response", "pas-claim-update", "pas-update-bundle", st.pci, updateCorr, "", updateBundle)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return false
	}
	if status, msg := g.validateFHIR(ctx, updateResp, "ingress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return false
	}
	parsed, err := shnsdk.ParseClaimResponse(updateResp)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "claim response parse failed"})
		return false
	}
	if parsed.Outcome != "approved" {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "preauthorization not approved after amendment"})
		return false
	}
	if err := g.cfg.Store.StoreAuthNumber(srRef, parsed.PreAuthRef); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "holder write failed (auth number)"})
		return false
	}
	writeJSON(w, http.StatusOK, uc03Resp{PARequired: true, AuthNumber: parsed.PreAuthRef, ValidUntil: parsed.ValidUntil, QRItems: st.filled, PendedItems: st.needed})
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
	st, ok := g.scenarioToPend(w, r, "uc07", "MBR-UC07")
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
	const srRef = "ServiceRequest/sr-uc07"
	const uc07QRID = "qr-uc07"
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

	// --- Patient-DTR exchange: ask the Trust PHG to have the patient author + attest. ---
	phg, ok := g.cfg.Reg.LookupByRole("phg")
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "no PHG registered"})
		return false
	}
	pdReq, err := json.Marshal(patientDTRRequest{LinkID: oswestryLinkID, Answer: score, PatientRef: st.patientRef})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build patient-dtr request failed"})
		return false
	}
	pdCorr := g.cfg.CorrelationGen()
	pdRespJSON, err := g.roundTrip(ctx, r, phg.ID, "provider-tpo", "patient-authorship", "patient-dtr-request", "patient-dtr-response", "patient-dtr", "patient-authorship-only", st.pci, pdCorr, "", pdReq)
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
	updateBundle, err := shnsdk.BuildClaimUpdateBundle(amendedQR, st.srJSON, nil, provJSON, st.patientRef, st.coverageRef, updateCorr, st.pasCorr, g.cfg.Clock())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build update bundle failed"})
		return false
	}
	if status, msg := g.validateFHIR(ctx, updateBundle, "egress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return false
	}
	updateResp, err := g.roundTrip(ctx, r, g.cfg.CounterpartID, "provider-tpo", "payer-coverage", "pas-update-submit", "pas-update-response", "pas-claim-update", "pas-update-bundle", st.pci, updateCorr, "", updateBundle)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return false
	}
	if status, msg := g.validateFHIR(ctx, updateResp, "ingress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return false
	}
	parsed, err := shnsdk.ParseClaimResponse(updateResp)
	if err != nil || parsed.Outcome != "approved" {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "preauthorization not approved after patient attestation"})
		return false
	}
	if err := g.cfg.Store.StoreAuthNumber(srRef, parsed.PreAuthRef); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "holder write failed (auth number)"})
		return false
	}
	writeJSON(w, http.StatusOK, uc03Resp{PARequired: true, AuthNumber: parsed.PreAuthRef, ValidUntil: parsed.ValidUntil, QRItems: st.filled, PendedItems: st.needed})
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
	st, ok := g.scenarioToPend(w, r, "uc06", "MBR-UC06")
	if !ok {
		return
	}
	token := g.storePending(st)
	writeJSON(w, http.StatusOK, startResp{Pended: true, Needed: st.needed, ResumeToken: token})
}

// handleUC07Start runs UC-07 to PENDED and parks it under a resume token.
func (g *Gateway) handleUC07Start(w http.ResponseWriter, r *http.Request) {
	st, ok := g.scenarioToPend(w, r, "uc07", "MBR-UC07")
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
