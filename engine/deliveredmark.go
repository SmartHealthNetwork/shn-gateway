package engine

import (
	"context"
	"net/http"
	"sync/atomic"
)

// deliveryMark is one call's record, on the requester's side, that a leg of
// it has handed back an answer verified as its recipient's own: from then on
// a recipient may have acted on the request, so every error this gateway
// writes on the call states it delivered yes (writeRefusal), whatever refused
// it and whichever leg it was at. It is set at one point only, roundTrip's
// success return (legAnswered), which comes after the answer passed
// VerifyBound, the sender and correlation checks, decryption and the frame
// decode. A call before that point, or with no leg answered, states no
// delivery of its own (a refusal before the send is unmarked).
type deliveryMark struct{ answered atomic.Bool }

type deliveryMarkKey struct{}

// deliveryWriter carries a call's deliveryMark to the writers that are handed
// only the writer (writeJSON, writeDeliveredMaybe).
type deliveryWriter struct {
	http.ResponseWriter
	mark *deliveryMark
}

// Unwrap exposes the underlying writer (http.ResponseController, scopeOf).
func (w *deliveryWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// WriteHeader is the mark's own floor: an error status written on the call
// after a leg was answered states delivered yes even when the writer that
// wrote it set no HubDeliveredHeader (every error this gateway builds goes
// through writeRefusal, which sets it with the body's marks; an error answer
// carried from a later leg's recipient does not). A value a writer set is kept.
func (w *deliveryWriter) WriteHeader(code int) {
	if code >= http.StatusBadRequest && w.mark.answered.Load() && w.Header().Get(HubDeliveredHeader) == "" {
		w.Header().Set(HubDeliveredHeader, string(deliveredYes))
	}
	w.ResponseWriter.WriteHeader(code)
}

// withDeliveryMark gives one requester call its deliveryMark, on its context
// for roundTrip and on its writer for the refusal writers. recordExchange
// installs it on every route this gateway's own participant calls
// (DirectionIngress), before anything else runs, whether or not the exchange
// record is observed: every route that sends a leg is one of them.
func withDeliveryMark(w http.ResponseWriter, r *http.Request) (http.ResponseWriter, *http.Request) {
	m := &deliveryMark{}
	return &deliveryWriter{ResponseWriter: w, mark: m}, r.WithContext(context.WithValue(r.Context(), deliveryMarkKey{}, m))
}

// legAnswered records that the call ctx belongs to has had a leg answered
// with an answer verified as its recipient's own. roundTrip calls it at its
// one success return; nothing else may (TestDeliveryMark_SetOnlyByRoundTrip).
func legAnswered(ctx context.Context) {
	if m, _ := ctx.Value(deliveryMarkKey{}).(*deliveryMark); m != nil {
		m.answered.Store(true)
	}
}

// recipientMayHaveActed reports whether the call w answers has had a leg
// answered (legAnswered): a recipient may have acted on the request.
func recipientMayHaveActed(w http.ResponseWriter) bool {
	for w != nil {
		if d, ok := w.(*deliveryWriter); ok {
			return d.mark.answered.Load()
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return false
		}
		w = u.Unwrap()
	}
	return false
}
