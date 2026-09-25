// transmit.go — the helpers every transmit boundary shares. A boundary sends
// only a relay.Payload, and only after relay.Transmit has checked it against
// the leg ownership table (relay.Check). A refused payload is a fault in this
// gateway: nothing is sent, the caller gets a 500, and the refusal is
// reported on the observer seam as relay.RefusedEvent.
//
// Every JSON answer the gateway writes itself goes through writeJSON: an error
// answer (status >= 400) is sealed as this gateway's own refusal and checked
// like any other payload; a success answer is a local API summary (scenario
// and console endpoints), which is not an exchange message.
package engine

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// transmitScope rides on a response writer and says which side of which leg
// its answers belong to, so a refusal written deep in a handler is checked
// against the right row. Writes pass straight through.
type transmitScope struct {
	http.ResponseWriter
	g    *Gateway
	role relay.Role
	leg  string
}

// Unwrap exposes the underlying writer to http.ResponseController and to
// scopeOf.
func (s *transmitScope) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// scopeOf finds the transmitScope in w's wrapper chain, or nil.
func scopeOf(w http.ResponseWriter) *transmitScope {
	for w != nil {
		if s, ok := w.(*transmitScope); ok {
			return s
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return nil
		}
		w = u.Unwrap()
	}
	return nil
}

// withScope returns w scoped to role on a leg not yet established. A writer
// already scoped keeps its scope.
func (g *Gateway) withScope(w http.ResponseWriter, role relay.Role) (http.ResponseWriter, *transmitScope) {
	if s := scopeOf(w); s != nil {
		return w, s
	}
	s := &transmitScope{ResponseWriter: w, g: g, role: role, leg: relay.LegUnestablished}
	return s, s
}

// refusalKey is the transmit a refusal written to w is checked against. A
// writer with no scope answers the gateway's own caller (a scenario or local
// API caller): the requester's side, before any leg.
func refusalKey(w http.ResponseWriter) (relay.Key, *Gateway) {
	if s := scopeOf(w); s != nil {
		return relay.Key{Leg: s.leg, Role: s.role, Direction: relay.DirectionResponse, Outcome: relay.OutcomeRefused}, s.g
	}
	return relay.Key{Leg: relay.LegUnestablished, Role: relay.RoleRequester, Direction: relay.DirectionResponse, Outcome: relay.OutcomeRefused}, nil
}

// ownershipRefused reports a payload the ownership table refused. g may be
// nil (a writer with no scope); the refusal is still logged.
func (g *Gateway) ownershipRefused(k relay.Key, err error) {
	log.Printf("gateway: %s: %v", relay.RefusedEvent, err)
	if g == nil {
		return
	}
	g.observe(ObserverEvent{Kind: relay.RefusedEvent, LegType: k.Leg, Direction: k.Role.String() + "-" + k.Direction.String(), Detail: err.Error()})
}

// errOwnershipFault is the local-fault message a refused transmit answers.
const errOwnershipFault = "gateway fault: payload not permitted on this leg"

// isOwnershipFault reports whether err is a refusal by the ownership table
// (or an unset payload).
func isOwnershipFault(err error) bool {
	return errors.Is(err, relay.ErrOwnershipRefused) || errors.Is(err, relay.ErrUnsetOwnership)
}

// admit checks p against transmit k and returns a copy of its bytes for the
// engine's own reading (fences, $validate, parsing) before the transmit
// itself. A refusal is reported; the caller answers a 500.
func (g *Gateway) admit(p relay.Payload, k relay.Key) ([]byte, error) {
	b, err := relay.Transmit(p, relay.Check(k))
	if err != nil {
		g.ownershipRefused(k, err)
		return nil, err
	}
	return b, nil
}

// writePayload is the response writer for payloads: it checks p against
// transmit k and writes it with status and contentType ("" sets no header).
// A refused payload writes a 500 local fault instead.
func (g *Gateway) writePayload(w http.ResponseWriter, status int, contentType string, p relay.Payload, k relay.Key) {
	b, err := relay.Transmit(p, relay.Check(k))
	if err != nil {
		g.ownershipRefused(k, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": errOwnershipFault})
		return
	}
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// writeJSON writes a JSON answer the gateway builds itself. A FHIR operation
// route (fhirOperationWriter) answers an error as an OperationOutcome.
func writeJSON(w http.ResponseWriter, status int, v any) {
	contentType := "application/json"
	if _, ok := w.(*fhirOperationWriter); ok {
		v = fhirOperationValue(status, v)
		contentType = "application/fhir+json"
	}
	b, err := json.Marshal(v)
	if err == nil {
		b = append(b, '\n') // the json.Encoder framing every caller has always sent
	} else {
		b = nil
	}
	if status < http.StatusBadRequest {
		writeLocalJSON(w, status, contentType, b)
		return
	}
	k, g := refusalKey(w)
	p, perr := relay.Authored(relay.BuilderGatewayRefusal, b, contentType)
	if perr == nil {
		b, perr = relay.Transmit(p, relay.Check(k))
	}
	if perr != nil {
		// The refusal itself was refused: answer a bare 500 rather than
		// recurse.
		g.ownershipRefused(k, perr)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// writeLocalJSON writes a successful local API answer (a scenario or console
// summary built from decoded values). Such an answer is not an exchange
// message, so it has no row in the ownership table.
func writeLocalJSON(w http.ResponseWriter, status int, contentType string, b []byte) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// sealRequest seals b as a request this gateway's own workflow built with
// builder id. A body the builder table refuses (for example, one that is not a
// single well-formed JSON document) is logged and yields the unset Payload,
// which the request boundary then refuses as a local fault.
func sealRequest(id relay.BuilderID, b []byte, contentType string) relay.Payload {
	p, err := relay.Authored(id, b, contentType)
	if err != nil {
		log.Printf("gateway: %s: request from %s not sealed: %v", relay.RefusedEvent, id, err)
		var unset relay.Payload
		return unset
	}
	return p
}

// responderFailed answers a LegResponder error on leg with a 500. A request
// the responder could not send to its own system because the ownership table
// refused it is also reported on the observer seam.
func (g *Gateway) responderFailed(w http.ResponseWriter, r *http.Request, leg inboundLeg, env shnsdk.Envelope, tok shnsdk.Token, answerTok string, err error) {
	status, msg := g.responderFailure(leg.tx, err)
	g.refuseInbound(w, r, leg, env, tok, answerTok, status, msg, nil)
}

// responderFailure decides how a LegResponder error on leg is answered, framed
// as this gateway's answer: the requester sees whose failure it was —
// the payer's system not answering (502) or this gateway's own fault (500) —
// never the error's own text. An ownership refusal is observed here.
func (g *Gateway) responderFailure(leg string, err error) (int, string) {
	if isOwnershipFault(err) {
		k := relay.Key{Leg: leg, Role: relay.RoleRecipient, Direction: relay.DirectionRequest, Outcome: relay.OutcomeCarried}
		g.observe(ObserverEvent{Kind: relay.RefusedEvent, LegType: k.Leg, Direction: k.Role.String() + "-" + k.Direction.String(), Detail: err.Error()})
	}
	var up *upstreamFailure
	if errors.As(err, &up) {
		if up.sent {
			return http.StatusBadGateway, errUpstreamNoUsableAnswer
		}
		return http.StatusBadGateway, errUpstreamNotReached
	}
	return http.StatusInternalServerError, "responder failed"
}

// upstreamFailure is a native forward that got no usable answer from the
// payer's own system. sent says the request was written to it: a connection
// that failed after that, a read that failed, or a body too large to relay.
// Before that (a dial, TLS or token failure) the payer's system never saw it.
type upstreamFailure struct {
	err  error
	sent bool
}

func (e *upstreamFailure) Error() string { return e.err.Error() }
func (e *upstreamFailure) Unwrap() error { return e.err }

// What a requester is told when the payer's own system gave no answer this
// gateway could carry: whether it never saw the request, or may have acted on
// it.
const (
	errUpstreamNotReached     = "the payer's system could not be reached"
	errUpstreamNoUsableAnswer = "the payer's system received this request but gave no answer this gateway could carry; it may have acted on it: check its outcome before resending"
)
