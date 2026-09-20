package engine

import (
	"encoding/json"
	"net/http"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A payer gateway's answer to an inbound request is either the participant's
// APPLICATION ANSWER — its verdict about the request it was sent — or an
// exchange-MACHINERY failure at its own edge. The protocol (PARTICIPANT_PROTOCOL
// "Mechanical vs. application status") carries the first framed with 200 to the
// Hub, real status inside, so the requester receives the payer's own status and
// body; only the second is a raw non-2xx, which the payload-blind Hub reports as
// its generic routing failure. The responder's results already travel that way
// (respondLegError). The checks a handler runs BEFORE the responder — subject
// bind, order presence, ingress $validate — were written raw regardless, so a
// requester could not tell "the payer does not hold this member" from "the Hub
// is down". refuseInbound is the one place that decides which is which.

// Named refusals shared by more than one handler. The classification below
// does not depend on the text: it is the status that says whose failure it is.
const (
	refusalUnknownMember     = "unknown member"
	refusalIngressValidation = "ingress validation failed"
	// refusalDTRUnframed refuses a questionnaire request that names no
	// operation: the older request envelope, which carried a canonical and a
	// coverage in place of the operation's own input, is no longer accepted.
	refusalDTRUnframed = "questionnaire request names no operation: send the questionnaire-package or next-question operation's own input in a request frame naming it"
)

// isApplicationRefusal reports whether a refusal a handler writes after the
// leg is authenticated is the participant's verdict about the request (an
// answer, framed) rather than the gateway's own fault (machinery, raw). The
// protocol names machinery exhaustively — a bad hop assertion, an envelope
// that fails to decode, a token that fails verification, a replay, an unknown
// transaction type, a failure building the response leg — and every one of
// those is decided before a handler runs (handleInbound) or is a fault of the
// gateway's own (a 5xx). So a 4xx a handler writes about the request it was
// sent — a member it does not hold, a request it cannot read, a subject that
// does not match the token, a validation failure, a consent it cannot confirm
// — is its answer. A 5xx — validator unavailable, ownership fault, a holder
// write that failed, a consent service that did not answer — is not.
func isApplicationRefusal(status int, _ string) bool {
	return status/100 == 4
}

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

// refuseInbound writes a payer-inbound bind/validate refusal. An application
// answer travels through respondLegError like a responder's non-2xx result —
// framed, 200 to the Hub, the payer's status and message inside — so the
// requester receives it as the payer sent it. Machinery stays a raw non-2xx
// through writeJSON, the same transmit site it used before. detail, when set,
// is the refusal's own JSON object (a 422 that echoes its issues) and must
// carry "error"; otherwise the body is {"error": msg}.
func (g *Gateway) refuseInbound(w http.ResponseWriter, r *http.Request, leg inboundLeg, env shnsdk.Envelope, tok shnsdk.Token, answerTok string, status int, msg string, detail map[string]any) {
	if !isApplicationRefusal(status, msg) {
		if detail != nil {
			writeJSON(w, status, detail)
			return
		}
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
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
