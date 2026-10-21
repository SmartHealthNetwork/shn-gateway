package engine

import (
	"net/http"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The PA legs as their recipient handlers answer them (inboundLeg).
var (
	legCRDOrderSelect   = inboundLeg{"payer-coverage", "crd-cards", "crd-order-select"}
	legCRDOrderDispatch = inboundLeg{"payer-coverage", "crd-dispatch-cards", "crd-order-dispatch"}
	legPASClaim         = inboundLeg{"payer-coverage", "pas-response", "pas-claim"}
	legPASClaimUpdate   = inboundLeg{"payer-coverage", "pas-update-response", "pas-claim-update"}
	legPASClaimInquire  = inboundLeg{"payer-coverage", "pas-inquire-response", "pas-claim-inquire"}
	legEligibility      = inboundLeg{"payer-coverage", "eligibility-response", "coverage-eligibility"}
	legDTR              = inboundLeg{"payer-coverage", "dtr-questionnaire", "dtr-questionnaire-fetch"}
	legPatientDTR       = inboundLeg{"patient-authorship", "patient-dtr-response", "patient-dtr"}
)

// answer: each PA leg's recipient handler. The three PAS legs first fence
// attested QuestionnaireResponse items (paAttested).
func (paWorkstream) answer(leg string) answerHandler {
	switch leg {
	case "coverage-eligibility":
		return (*Gateway).handleEligibilityInbound
	case "crd-order-select":
		return (*Gateway).handleCRDNativeInbound
	case "crd-order-dispatch":
		return (*Gateway).handleCRDDispatchInbound
	case "dtr-questionnaire-fetch":
		return (*Gateway).handleDTRInbound
	case "pas-claim":
		return paAttested(legPASClaim, (*Gateway).handlePASNativeInbound)
	case "pas-claim-update":
		return paAttested(legPASClaimUpdate, (*Gateway).handlePASUpdateNativeInbound)
	case "pas-claim-inquire":
		// An inquiry's profile gives it no QuestionnaireResponse, so the fence
		// is expected to pass, but the property belongs to any QR item wherever
		// it arrives, and a fence that runs on two of three PAS legs is a gap
		// waiting for the third to carry one.
		return paAttested(legPASClaimInquire, (*Gateway).handlePASInquireInbound)
	case "patient-dtr":
		return (*Gateway).handlePatientDTRInbound
	}
	return nil
}

// paAttested is answer fenced on attested items first (R8 re-home,
// FR-16/FR-27): an unattested clinician or patient QR item is nonconformant
// whichever handler would otherwise run. The attestation is the QR's own
// content (RuleAttestation): not checked at none, recorded at observe and
// forwarded, refused at strict.
func paAttested(leg inboundLeg, answer answerHandler) answerHandler {
	return func(g *Gateway, w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, body []byte, answerTok string) {
		if reason, ok := fenceAttestedItems(body); !ok && g.guard(r.Context(), KindContent, RuleAttestation, body) {
			g.refuseInbound(w, r, leg, env, tok, answerTok, http.StatusForbidden, reason, nil)
			return
		}
		answer(g, w, r, env, tok, body, answerTok)
	}
}
