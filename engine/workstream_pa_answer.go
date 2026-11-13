//go:build !shn_coreonly

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
		return paAddressIssuer((*Gateway).handleDTRInbound)
	case "pas-claim":
		return paAddressIssuer(paAttested(legPASClaim, (*Gateway).handlePASNativeInbound))
	case "pas-claim-update":
		return paAddressIssuer(paAttested(legPASClaimUpdate, (*Gateway).handlePASUpdateNativeInbound))
	case "pas-claim-inquire":
		// An inquiry's profile gives it no QuestionnaireResponse, so the fence
		// is expected to pass, but the property belongs to any QR item wherever
		// it arrives, and a fence that runs on two of three PAS legs is a gap
		// waiting for the third to carry one.
		return paAddressIssuer(paAttested(legPASClaimInquire, (*Gateway).handlePASInquireInbound))
	case "patient-dtr":
		return (*Gateway).handlePatientDTRInbound
	case "cdex-attachment-submit":
		return paAddressIssuer((*Gateway).handleCDexSubmitInbound)
	case "cdex-attachment-request":
		return (*Gateway).handleCDexRequestInbound
	case "cdex-task-status":
		return (*Gateway).handleCDexTaskReadInbound
	}
	return nil
}

// paAddressIssuer is answer with, on a payer gateway, the DTR adaptive-address
// issuer on the request's context: its tag key, its own participant id, and
// the authenticated provider that sent the request. The DTR fetch tags its
// answer's addresses with it and verifies a round's address; the forward of
// every leg that can carry the questionnaire back restores the addresses it
// issued (E-13). On any other role it adds nothing.
func paAddressIssuer(answer answerHandler) answerHandler {
	return func(g *Gateway, w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, body []byte, answerTok string) {
		if st := g.paState(); st != nil && !st.addressKey.IsZero() {
			r = r.WithContext(withAddressIssuer(r.Context(), addressIssuer{
				key: st.addressKey, payer: g.cfg.HolderID, provider: env.Metadata.Sender,
				// The event is the fact alone (a stable promise from its
				// first release): kind, direction, leg and correlation, never
				// the address, its tag or any participant's identifier.
				carried: func() {
					g.observe(ObserverEvent{Kind: "dtr.adaptive-address-carried", Direction: "ingress", LegType: env.Metadata.TransactionType,
						CorrelationID: env.Metadata.CorrelationID})
				},
			}))
		}
		answer(g, w, r, env, tok, body, answerTok)
	}
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
