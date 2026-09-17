// payoredge.go — the payer-edge identity mapping: a payer-role gateway translating
// its network payer identity into the identifier its own payer system expects, on the
// request it sends that system (nativeResponder).
//
// The mapping is an ownership assertion, not a rewrite of the request:
//   - It maps only a request whose Coverages name this gateway's OWN configured payer
//     identity (payorEdgeOwn). A Coverage naming another payer, or no readable payer, is
//     refused with a legible 400-class LegResult, never a bare error (which the engine
//     would answer with an opaque 500); Coverages naming different payers, and a
//     reference that resolves to no resource or to several (a Coverage's payor, or a
//     prior-authorization Claim's insurer), are refused with a 422.
//   - It changes only the payer identifier's system and value strings (inline on the
//     Coverage's routing payor, payor[0], or on the Organization it references) and, for
//     a prior-authorization request, the Claim insurer's when it names this payer (an
//     insurer naming another identity is left as sent). The
//     edit is the registered payer-identity edit (relayedit.go), made by relay.Apply:
//     every other byte of the request is sent exactly as it arrived, and an edit that
//     would change signed content is refused (422) rather than made.
//   - A mapping to the identity the request already carries makes no edit: the request
//     is sent exactly.
//
// Coverage carriage spans the legs this mapping covers: both CRD legs,
// crd-order-select and crd-order-dispatch (prefetch.coverage, a bare Coverage or a
// Bundle), dtr-questionnaire-fetch (the request's coverage), and
// pas-claim/pas-claim-update (every Coverage entry of the $submit Bundle, and each
// Claim.insurer).
package engine

import (
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// payorEdgeRefusal builds the fail-closed LegResult for a request that names no
// readable payer, or another payer: a 400-class, legible refusal naming the mismatch,
// never a bare error (which the engine maps to an opaque "hub routing failed" 500 — this
// refusal must be visible to the caller as a policy decision, not a transport fault).
func payorEdgeRefusal(own, got shnsdk.PayerIdentifier, gotOK bool) LegResult {
	if !gotOK {
		return LegResult{
			Status: http.StatusBadRequest,
			Message: fmt.Sprintf(
				"payer backend identity mapping: inbound Coverage carries no resolvable payor identifier (this gateway's own payer identity is %s|%s); refusing rather than adjudicating for an unrecognized payer",
				own.System, own.Value),
		}
	}
	return LegResult{
		Status: http.StatusBadRequest,
		Message: fmt.Sprintf(
			"payer backend identity mapping: inbound Coverage payor %s|%s does not match this gateway's own payer identity %s|%s; refusing rather than adjudicating as a different payer",
			got.System, got.Value, own.System, own.Value),
	}
}

// payorEdgeRequest returns the request to send the payer's own system: the network's
// request in exactly, or, when the mapping is configured and changes something, in with
// the payer-identity edit applied. A refusal is returned as a LegResult (Status set); a
// gateway fault as an error.
func (n *nativeResponder) payorEdgeRequest(in relay.Body, carrier payorEdgeCarrier, contentType string) (relay.Payload, LegResult, error) {
	var none relay.Payload
	if n.payorEdgeOwn == nil {
		return relay.Exact(in, contentType), LegResult{}, nil
	}
	doc, err := relay.Doc(in)
	if err != nil {
		return none, LegResult{Status: http.StatusBadRequest, Message: "payer backend identity mapping: request is not one well-formed JSON document"}, nil
	}
	ops, err := locatePayorEdge(doc, carrier, *n.payorEdgeOwn, *n.payorEdgeBackend)
	var refused *payorEdgeRefused
	if errors.As(err, &refused) {
		return none, refused.legResult(), nil
	}
	if err != nil {
		return none, LegResult{}, fmt.Errorf("engine: payor edge: %w", err)
	}
	if len(ops) == 0 {
		return relay.Exact(in, contentType), LegResult{}, nil
	}
	p, err := relay.Apply(in, contentType, relay.EditPayorEdgeRestamp, ops...)
	var signed *relay.SignedContentError
	if errors.As(err, &signed) {
		return none, LegResult{Status: http.StatusUnprocessableEntity, Message: signed.Error()}, nil
	}
	if err != nil {
		return none, LegResult{}, fmt.Errorf("engine: payor edge: %w", err)
	}
	return p, LegResult{}, nil
}

// interimShapingInput reads the bytes of a request that an interim shaping step
// rebuilds next (the step then seals its own output under its builder). Only the
// network's request, exact or with the payer-identity edit, is accepted.
func interimShapingInput(p relay.Payload) ([]byte, error) {
	return relay.Transmit(p, func(p relay.Payload) error {
		switch p.Ownership() {
		case relay.OwnershipRelayed:
			return nil
		case relay.OwnershipEdited:
			if slices.Equal(p.Edits(), []relay.EditID{relay.EditPayorEdgeRestamp}) {
				return nil
			}
		}
		return fmt.Errorf("engine: a %s payload is not an input to request shaping", p.Ownership())
	})
}
