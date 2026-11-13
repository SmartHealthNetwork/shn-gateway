// transmit.go — the helpers every transmit boundary shares. A boundary sends
// only a relay.Payload, and only after relay.Transmit has checked it against
// the leg ownership table (relay.Check). A refused payload is a fault in this
// gateway: nothing is sent, the caller gets a 500, and the refusal is
// reported on the observer seam as relay.RefusedEvent.
//
// Every JSON answer the gateway writes itself goes through writeJSON: an error
// answer (status >= 400) is sealed as this gateway's own refusal and checked
// like any other payload; a success answer is a local API summary (the
// scenario endpoints), which is not an exchange message.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
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

// writeOwnershipFault answers a payload the ownership table refused: this
// gateway's fidelity refusal of its own transmit, a message it built failing
// its own verification.
func writeOwnershipFault(w http.ResponseWriter) {
	exchangeOfWriter(w).refusedFidelity(vocab.RefusalStageVerify)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": errOwnershipFault})
}

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
	g.writeAnswer(w, status, contentType, nil, p, k)
}

// writeAnswer is writePayload with the answer's own headers, set only once
// the payload passes its check: a local fault states none of them. It reports
// whether it wrote the payload.
func (g *Gateway) writeAnswer(w http.ResponseWriter, status int, contentType string, headers map[string]string, p relay.Payload, k relay.Key) bool {
	b, err := relay.Transmit(p, relay.Check(k))
	if err != nil {
		g.ownershipRefused(k, err)
		writeOwnershipFault(w)
		return false
	}
	exchangeOfWriter(w).answered(p)
	if k.Role == relay.RoleRecipient && k.Direction == relay.DirectionResponse {
		// An answer to a requester without a message frame: its outcome is
		// the one it was checked against.
		exchangeOfWriter(w).decided(answerOutcome(k.Outcome), "", "")
	}
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	for name, value := range headers {
		w.Header().Set(name, value)
	}
	// A header edit's header (relay.HeaderEdited), after the answer's own:
	// written from the payload alone, as the core's check of the edit, by its
	// id, passed it.
	if name, value, ok := p.EditedHeader(); ok {
		w.Header().Set(name, value)
	}
	if k.Role == relay.RoleRequester && k.Direction == relay.DirectionResponse {
		setEditedHeader(w.Header(), p)
	}
	w.WriteHeader(status)
	_, _ = w.Write(b)
	return true
}

// writeJSON writes a JSON answer the gateway builds itself. A FHIR operation
// route (fhirOperationWriter) answers an error as an OperationOutcome. An error
// is a refusal (writeRefusal): it states delivery once a leg of the call was
// answered.
func writeJSON(w http.ResponseWriter, status int, v any) {
	if status >= http.StatusBadRequest {
		writeRefusal(w, status, v, "")
		return
	}
	contentType := "application/json"
	if isFHIROperation(w) {
		v = fhirOperationValue(status, v)
		contentType = "application/fhir+json"
	}
	b, err := json.Marshal(v)
	if err == nil {
		b = append(b, '\n') // the json.Encoder framing every caller has always sent
	} else {
		b = nil
	}
	writeLocalJSON(w, status, contentType, b)
}

// writeRefusal writes v, an error answer (status 400 or more) this gateway
// builds itself, as writeJSON always has: on a FHIR operation route the
// OperationOutcome (fhirOperationValue), elsewhere v as JSON. It is the one
// writer of every such answer (TestRefusals_AllGoThroughWriteRefusal), so it
// decides by structure whether the answer states delivery:
//   - delivered is what the failure itself says (relayOriginationError's
//     reading of a leg's error, through writeDeliveredMaybe), "" for none;
//   - once a leg of the call has handed back an answer verified as its
//     recipient's own (recipientMayHaveActed), a recipient may have acted on
//     the request, so the answer is delivered yes whatever refused it and
//     whatever delivered says, at every level and on every route;
//   - a delivered answer carries HubDeliveredHeader and, in its body, the
//     marks markDelivered adds to it, its status and every member it had kept.
//
// A refusal before any leg was answered states no delivery: it is written as
// before.
func writeRefusal(w http.ResponseWriter, status int, v any, delivered deliveredMaybe) {
	if recipientMayHaveActed(w) {
		delivered = deliveredYes
	}
	contentType := "application/json"
	if isFHIROperation(w) {
		v = fhirOperationValue(status, v)
		contentType = "application/fhir+json"
	}
	b, err := json.Marshal(v)
	if err == nil {
		b = append(b, '\n') // the json.Encoder framing every caller has always sent
	} else {
		b = nil
	}
	if delivered != "" {
		w.Header().Set(HubDeliveredHeader, string(delivered))
		if marked, problem := markDelivered(b, status, delivered); marked != nil {
			b = marked
			if problem && problemDetailsRoute(w) {
				contentType = "application/problem+json"
			}
		}
	}
	writeRefusalBody(w, status, contentType, b)
}

// writeRefusalBody writes b, a refusal this gateway built itself, as its
// answer: authored by the gateway's refusal builder and transmitted under the
// writer's refusal key, so the exchange record holds what was sent.
func writeRefusalBody(w http.ResponseWriter, status int, contentType string, b []byte) {
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
	exchangeOfWriter(w).answered(p)
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// writeLocalJSON writes a successful local API answer (a scenario route's
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

// responderFailed answers a LegResponder error on leg as this gateway's own
// framed failure (responderFailure: 502, 504 or 500). A request
// the responder could not send to its own system because the ownership table
// refused it is also reported on the observer seam.
func (g *Gateway) responderFailed(w http.ResponseWriter, r *http.Request, leg inboundLeg, env shnsdk.Envelope, tok shnsdk.Token, answerTok string, err error) {
	// The system's answer was over the limit: refused, never relayed cut
	// short, as delivered (the system answered and may have acted).
	if errors.Is(err, errAnswerTooLarge) {
		exchangeOf(r.Context()).refused(RefusalRouting)
		attestAnswerRefused(w, RefusalRouting, errResponseTooLarge)
		return
	}
	// The system redirected the forward from https to http, and the hop was
	// not sent (smartauth.ErrCleartextRedirect): refused on message
	// integrity, as delivered (the system answered, with its 3xx, and may
	// have acted). A token endpoint's downgrade never reaches here: nothing
	// was sent, and it is answered as a token failure.
	if uf := (*upstreamFailure)(nil); errors.As(err, &uf) && uf.cleartextRedirect {
		exchangeOf(r.Context()).refused(RefusalIntegrity)
		attestAnswerRefused(w, RefusalIntegrity, fmt.Sprintf(errCleartextRedirectFor, responderParty(leg.tx)))
		return
	}
	// The system redirected a request that carries a body to another origin,
	// and the hop was not sent (smartauth.ErrCrossOriginBodyRedirect): refused
	// on routing, the request's addressing, as delivered (the system answered,
	// with its 3xx, and may have acted). The reason names the origin only.
	if uf := (*upstreamFailure)(nil); errors.As(err, &uf) && uf.crossOriginRedirect != "" {
		exchangeOf(r.Context()).refused(RefusalRouting)
		attestAnswerRefused(w, RefusalRouting, fmt.Sprintf(errCrossOriginRedirectFor, uf.crossOriginRedirect))
		return
	}
	status, msg := g.responderFailure(r.Context(), leg.tx, err)
	g.refuseInbound(w, r, leg, env, tok, answerTok, status, msg, nil)
	// The answer is this gateway's, but the record says whose failure it was:
	// its participant's system not answering usably is an upstream error.
	var uf *upstreamFailure
	switch x := exchangeOf(r.Context()); {
	case isOwnershipFault(err):
		x.refusedFidelity(vocab.RefusalStageVerify)
	case errors.As(err, &uf):
		x.decided(ExchangeUpstreamError, "", "")
	default:
		x.decided(ExchangeOther, "", "")
	}
}

// responderFailure decides how a LegResponder error on leg is answered, framed
// as this gateway's answer: the requester sees whose failure it was — the
// payer's system not answering (502), or not answering within the
// responder's deadline (504), or this gateway's own fault (500) — never the
// error's own text. This gateway's own fault says whether the request had
// been forwarded to the payer's system, which then may have acted on it. An
// ownership refusal is observed here.
func (g *Gateway) responderFailure(ctx context.Context, leg string, err error) (int, string) {
	if isOwnershipFault(err) {
		k := relay.Key{Leg: leg, Role: relay.RoleRecipient, Direction: relay.DirectionRequest, Outcome: relay.OutcomeCarried}
		g.observe(ObserverEvent{Kind: relay.RefusedEvent, LegType: k.Leg, Direction: k.Role.String() + "-" + k.Direction.String(), Detail: err.Error()})
	}
	party := responderParty(leg)
	var spent *deadlineSpent
	if errors.As(err, &spent) {
		return http.StatusGatewayTimeout, fmt.Sprintf(errDeadlineSpentFor, party)
	}
	var up *upstreamFailure
	if errors.As(err, &up) {
		switch {
		case up.timedOut && up.sent:
			return http.StatusGatewayTimeout, fmt.Sprintf(errUpstreamTimedOutFor, party)
		case up.timedOut:
			return http.StatusGatewayTimeout, fmt.Sprintf(errUpstreamNotReachedInTimeFor, party)
		case up.sent:
			return http.StatusBadGateway, fmt.Sprintf(errUpstreamNoUsableAnswerFor, party)
		}
		return http.StatusBadGateway, fmt.Sprintf(errUpstreamNotReachedFor, party)
	}
	// Whether the request was forwarded is known only when the built-in
	// native responder answers the leg itself: a connector, one that wraps it
	// included, may call its system on its own.
	forwarded, known := exchangeOf(ctx).forwardedToSystem()
	_, native := g.cfg.Responder.(*nativeResponder)
	if marked, _ := ctx.Value(nativeForwardKey{}).(bool); marked {
		native = true
	}
	if !native && !forwarded {
		known = false
	}
	switch {
	case !known:
		return http.StatusInternalServerError, fmt.Sprintf(errResponderFailedUnknownFor, party)
	case forwarded:
		return http.StatusInternalServerError, fmt.Sprintf(errResponderFailedAfterForwardFor, party)
	}
	return http.StatusInternalServerError, fmt.Sprintf(errResponderFailedBeforeForwardFor, party)
}

// The recipient gateway's own failures, by whether the request had been
// forwarded to its system: after, it may have acted on it. Each names the
// recipient's role (responderParty).
const (
	errResponderFailedBeforeForwardFor = "the %[1]s's gateway failed before forwarding this request to the %[1]s's system"
	errResponderFailedAfterForwardFor  = "the %[1]s's gateway failed after forwarding this request to the %[1]s's system, which may have acted on it: check its outcome before resending"
	errResponderFailedUnknownFor       = "the %[1]s's gateway failed, and whether the %[1]s's system received this request is not known: check its outcome before resending"
)

// The answers above for the payer's gateway, every provider's leg's.
var (
	errResponderFailedBeforeForward = fmt.Sprintf(errResponderFailedBeforeForwardFor, "payer")
	errResponderFailedAfterForward  = fmt.Sprintf(errResponderFailedAfterForwardFor, "payer")
	errResponderFailedUnknown       = fmt.Sprintf(errResponderFailedUnknownFor, "payer")
)

// nativeForwardKey marks a leg whose handler forwards to its participant's
// own system through a native responder of its own (the provider's Task
// forward), so a failure can say whether that system was asked as it can
// for Config.Responder's.
type nativeForwardKey struct{}

// withNativeForward returns ctx so marked.
func withNativeForward(ctx context.Context) context.Context {
	return context.WithValue(ctx, nativeForwardKey{}, true)
}

// upstreamFailure is a native forward that got no usable answer from the
// payer's own system. sent says the request was written to it: a connection
// that failed after that, a read that failed, or an answer too large to read
// (errAnswerTooLarge).
// Before that (a dial, TLS or token failure) the payer's system never saw it.
type upstreamFailure struct {
	err  error
	sent bool
	// timedOut: the call ran past the responder's own deadline
	// (WithBackendDeadline) while the request was still live.
	timedOut bool
	// cleartextRedirect: the system answered the request with a redirect
	// from https to http, and the hop was not sent: refused on message
	// integrity, as delivered (responderFailed). Set only where the system's
	// own answer was the redirect, never for a token endpoint's.
	cleartextRedirect bool
	// crossOriginRedirect is the origin (scheme, host and port) the system's
	// redirect named, when the system answered a request that carried a body
	// with a redirect to another origin and the hop was not sent: refused on
	// routing, as delivered (responderFailed). Empty otherwise. Never the
	// redirect's path or query.
	crossOriginRedirect string
}

func (e *upstreamFailure) Error() string { return e.err.Error() }
func (e *upstreamFailure) Unwrap() error { return e.err }

// What a requester is told when the recipient's own system gave no answer
// this gateway could carry: whether it never saw the request, or may have
// acted on it. Each names whose system it is, the role the leg's row names
// for its recipient (responderParty): the payer's on every provider's leg,
// the provider's on the payer's attachment request.
const (
	errUpstreamNotReachedFor     = "the %[1]s's system could not be reached"
	errUpstreamNoUsableAnswerFor = "the %[1]s's system received this request but gave no answer this gateway could carry; it may have acted on it: check its outcome before resending"
	// A call past the recipient gateway's deadline for its own system.
	errUpstreamTimedOutFor         = "the %[1]s's system received this request but did not answer in time; it may have acted on it: check its outcome before resending"
	errUpstreamNotReachedInTimeFor = "the %[1]s's system could not be reached in time"
	// The gateway's own work before the call used its deadline: the operation
	// was not sent to the recipient's system (a read that ran first, such as
	// the CDS service listing, may have reached it).
	errDeadlineSpentFor = "the %[1]s's gateway ran out of time before it could send this request to the %[1]s's system; the %[1]s's system did not receive it"
)

// errCleartextRedirectFor is the reason a forward is refused when the
// recipient's system redirected it from https to http (smartauth.CheckRedirect):
// the hop is not sent, whatever its origin, since a 307 or 308 replays the
// body. It names the recipient's role (responderParty).
const errCleartextRedirectFor = "the %[1]s's system redirected this request from https to http, where it would travel in cleartext: it was not sent there"

// errCrossOriginRedirectFor is the reason a forward is refused when the
// recipient's system answered it, a request that carries a body, with a
// redirect to another origin (smartauth.ErrCrossOriginBodyRedirect), naming
// that origin: such a request goes only to the recipient's registered
// endpoint, and its operator is the one who can point it at the final
// address.
const errCrossOriginRedirectFor = "the recipient's system redirected this request to another origin (%s), where this gateway does not send it; the recipient's operator must point its registered endpoint at the final address"

// The answers above for the payer's system, every provider's leg's.
var (
	errCleartextRedirect        = fmt.Sprintf(errCleartextRedirectFor, "payer")
	errUpstreamNotReached       = fmt.Sprintf(errUpstreamNotReachedFor, "payer")
	errUpstreamNoUsableAnswer   = fmt.Sprintf(errUpstreamNoUsableAnswerFor, "payer")
	errUpstreamTimedOut         = fmt.Sprintf(errUpstreamTimedOutFor, "payer")
	errUpstreamNotReachedInTime = fmt.Sprintf(errUpstreamNotReachedInTimeFor, "payer")
	errDeadlineSpent            = fmt.Sprintf(errDeadlineSpentFor, "payer")
)

// responderParty is whose system answers leg: the role its row names for
// its recipient, "payer" for a leg this gateway does not know.
func responderParty(leg string) string {
	if spec, ok := legSpecFor(leg); ok && len(spec.RecipientRoles) > 0 {
		return spec.RecipientRoles[0]
	}
	return "payer"
}
