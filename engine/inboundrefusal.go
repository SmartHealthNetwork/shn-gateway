package engine

import (
	"encoding/json"
	"net/http"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A payer gateway's answer to an inbound request is either the participant's
// APPLICATION ANSWER or an exchange-MACHINERY failure at its own edge. The
// protocol (PARTICIPANT_PROTOCOL "Mechanical vs. application status") carries the
// first framed with 200 to the Hub, real status inside, so the requester receives
// the payer gateway's own status and body; only the second is a raw non-2xx,
// which the payload-blind Hub reports as a failed forward naming the status.
//
// The protocol names machinery exhaustively — a bad hop assertion, an envelope
// that fails to decode, a token that fails verification, a replay, an unknown
// transaction type, a failure building the response leg — and every one of those
// is decided before a handler runs (handleInbound) or inside the response-leg
// builder itself. So everything a handler writes once the leg is authenticated
// is its answer: a 4xx about the request it was sent (a member it does not hold,
// a subject that does not match the token, a validation failure) and a 5xx of its
// own (its payer's system not answering, a validator it cannot reach, a holder
// write that failed). refuseInbound writes every one of them framed.

// Named refusals shared by more than one handler.
const (
	refusalUnknownMember     = "unknown member"
	refusalIngressValidation = "ingress validation failed"
	// refusalDTRUnframed refuses a questionnaire request that names no
	// operation: the older request envelope, which carried a canonical and a
	// coverage in place of the operation's own input, is no longer accepted.
	refusalDTRUnframed = "questionnaire request names no operation: send the questionnaire-package or next-question operation's own input in a request frame naming it"
)

// inboundLeg names the response frame, operation and transaction type an
// inbound handler answers on — the same triple its success and responder
// paths already pass to respondLeg / respondLegError.
type inboundLeg struct{ frame, op, tx string }

var (
	legCRDOrderSelect   = inboundLeg{"payer-coverage", "crd-cards", "crd-order-select"}
	legCRDOrderDispatch = inboundLeg{"payer-coverage", "crd-dispatch-cards", "crd-order-dispatch"}
	legPASClaim         = inboundLeg{"payer-coverage", "pas-response", "pas-claim"}
	legPASClaimUpdate   = inboundLeg{"payer-coverage", "pas-update-response", "pas-claim-update"}
	legPASClaimInquire  = inboundLeg{"payer-coverage", "pas-inquire-response", "pas-claim-inquire"}
	legEligibility      = inboundLeg{"payer-coverage", "eligibility-response", "coverage-eligibility"}
	legDTR              = inboundLeg{"payer-coverage", "dtr-questionnaire", "dtr-questionnaire-fetch"}
	legFederatedQuery   = inboundLeg{"facility-disclosure", "federated-query-response", "federated-query"}
	legPatientDTR       = inboundLeg{"patient-authorship", "patient-dtr-response", "patient-dtr"}
)

// refuseInbound writes a refusal a handler makes once the leg is
// authenticated. It travels through respondLegError like a responder's non-2xx
// result — framed, 200 to the Hub, the payer gateway's status and message
// inside — so the requester receives it as the payer gateway sent it (to a
// requester that does not decode frames, respondLegError writes the bare
// status). detail, when set, is the refusal's own JSON object (a 422 that
// echoes its issues) and must carry "error"; otherwise the body is
// {"error": msg}.
func (g *Gateway) refuseInbound(w http.ResponseWriter, r *http.Request, leg inboundLeg, env shnsdk.Envelope, tok shnsdk.Token, answerTok string, status int, msg string, detail map[string]any) {
	result := LegResult{Status: status, Message: msg}
	if detail != nil {
		body, err := json.Marshal(detail)
		if err == nil {
			var p relay.Payload
			if p, err = relay.Authored(relay.BuilderGatewayRefusal, body, "application/json"); err == nil {
				result.Response = p
			}
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": errOwnershipFault})
			return
		}
	}
	// A consent-gated leg's refusal is anchored to the consent reference the
	// leg carried (empty on every other leg, and before one was carried at all).
	g.respondLegError(w, r, leg.frame, leg.op, leg.tx, env.Metadata.CorrelationID, result, tok.Subject, env.Metadata.Sender, env.Metadata.ConsentRef, answerTok)
}
