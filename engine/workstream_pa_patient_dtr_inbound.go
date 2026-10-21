package engine

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

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
func (g *Gateway) handlePatientDTRInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, reqJSON []byte, answerTok string) {
	var req patientDTRRequest
	if err := decodeMessage(reqJSON, &req); err != nil {
		g.refuseInbound(w, r, legPatientDTR, env, tok, answerTok, http.StatusBadRequest, "parse patient-dtr request failed", nil)
		return
	}
	// H2: bind the token subject to the patient the request names (the patient whose
	// authorship the Trust surface is exercising). The PHG signs as that patient, so
	// it must be the one the leg's authority names: any other is refused at every
	// level. One the PHG does not hold reads the same as one that is someone else,
	// so the refusal does not tell the requester whether the PHG holds the patient.
	member := strings.TrimPrefix(req.PatientRef, "Patient/")
	if member == "" {
		g.refuseInbound(w, r, legPatientDTR, env, tok, answerTok, http.StatusBadRequest, refusalPatientDTRNoPatient, nil)
		return
	}
	pci, _, found, readErr := ReadSystemOfRecord(g.cfg.SoR).ResolvePatientContext(r.Context(), member)
	if g.refuseUnreadableMember(w, r, legPatientDTR, env, tok, answerTok, refusalPatientNotAuthorized, readErr) ||
		g.refuseSoRFailure(w, r, legPatientDTR, env, tok, answerTok, readErr) {
		return
	}
	if !found || pci != tok.Subject {
		exchangeOf(r.Context()).refusing(RefusalAuthority)
		g.refuseInbound(w, r, legPatientDTR, env, tok, answerTok, http.StatusForbidden, refusalPatientNotAuthorized, nil)
		return
	}

	// #5: the Trust signer validates the answer against the item's constraint
	// before attesting — it must not sign a non-conformant patient answer. This
	// is the authoritative, un-bypassable guard (AI-10 fiduciary surface).
	if err := shnsdk.ValidatePatientAnswer(req.LinkID, req.Answer); err != nil {
		g.refuseInbound(w, r, legPatientDTR, env, tok, answerTok, http.StatusBadRequest, "invalid patient answer: "+err.Error(), nil)
		return
	}

	attested, err := shnsdk.BuildPatientAttestedItem(req.LinkID, req.Answer, req.PatientRef, g.cfg.Clock().Format("2006-01-02"))
	if err != nil {
		g.refuseInbound(w, r, legPatientDTR, env, tok, answerTok, http.StatusInternalServerError, "build patient-attested item failed", nil)
		return
	}
	respJSON, err := json.Marshal(patientDTRResponse{AttestedItem: attested})
	if err != nil {
		g.refuseInbound(w, r, legPatientDTR, env, tok, answerTok, http.StatusInternalServerError, "marshal response failed", nil)
		return
	}
	answer, err := relay.Authored(relay.BuilderSDKPatientDTR, respJSON, "application/json")
	if err != nil {
		g.refuseInbound(w, r, legPatientDTR, env, tok, answerTok, http.StatusInternalServerError, "marshal response failed", nil)
		return
	}
	g.respondLeg(w, r, "patient-authorship", "patient-dtr-response", "patient-dtr", env.Metadata.CorrelationID, answer, http.StatusOK, tok.Subject, env.Metadata.Sender, "", answerTok)
}
