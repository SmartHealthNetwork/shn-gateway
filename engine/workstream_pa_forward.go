package engine

import (
	"context"
	"net/http"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// forward: each PA leg the native responder sends to the payer's own system.
//
// There is deliberately NO "coverage-eligibility" case. Eligibility is a
// first-class engine handler (R11): handleEligibilityInbound answers it from the
// member's own Coverage in the payer's SoR, or, when the payer declares its own
// eligibility endpoint (WithEligibilityURL), carries it there through
// forwardEligibility. It never reaches the native responder.
func (paWorkstream) forward(leg string) forwardHandler {
	switch leg {
	case "crd-order-select", "crd-order-dispatch":
		return func(n *nativeResponder, ctx context.Context, contract, leg, _, _ string, in relay.Body, _ []byte) (LegResult, error) {
			return n.forwardCRD(ctx, contract, leg, in)
		}
	case "dtr-questionnaire-fetch":
		return forwardDTRQuestionnaireFetch
	case "pas-claim":
		return func(n *nativeResponder, ctx context.Context, _, _, corrID, subjectPCI string, in relay.Body, requestFHIR []byte) (LegResult, error) {
			return n.handlePASClaimNative(ctx, corrID, subjectPCI, in, requestFHIR)
		}
	case "pas-claim-update":
		return func(n *nativeResponder, ctx context.Context, _, _, corrID, subjectPCI string, in relay.Body, requestFHIR []byte) (LegResult, error) {
			return n.handlePASClaimUpdateNative(ctx, corrID, subjectPCI, in, requestFHIR)
		}
	case "pas-claim-inquire":
		// A read of the payer's own record about an authorization it pended
		// (workstream_pa_inquire.go). It acquires no claim and writes nothing here: the ledger
		// effect is derived by the payer gateway from the answer.
		return func(n *nativeResponder, ctx context.Context, contract, _, _, _ string, in relay.Body, requestFHIR []byte) (LegResult, error) {
			return n.handlePASInquireNative(ctx, contract, in, requestFHIR)
		}
	}
	return nil
}

// forwardDTRQuestionnaireFetch sends a DTR request to the payer's system. A
// request frame that names the operation carries that operation's own input,
// which is sent to the payer's system exactly (or with only the payer identity
// mapped). A request that names none is refused.
func forwardDTRQuestionnaireFetch(n *nativeResponder, ctx context.Context, contract, leg, _, _ string, in relay.Body, _ []byte) (LegResult, error) {
	switch op := RequestFrameOperation(ctx); op {
	case shnsdk.FrameOperationQuestionnairePackage:
		return n.forwardDTROperation(ctx, contract, in, "/Questionnaire/$questionnaire-package", payorEdgeDTRParameters, leg, "DTR")
	case shnsdk.FrameOperationNextQuestion:
		return n.forwardDTROperation(ctx, contract, in, "/Questionnaire/$next-question", 0, leg, "DTR next-question")
	case "":
		// The older request envelope, which carried a canonical and a coverage
		// in place of the operation's own input, is no longer read.
		return LegResult{Status: http.StatusBadRequest, Message: refusalDTRUnframed}, nil
	default:
		return LegResult{Status: http.StatusBadRequest, Message: "unsupported DTR operation"}, nil
	}
}
