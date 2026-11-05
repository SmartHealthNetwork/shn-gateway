package engine

import (
	"errors"
	"io"
	"net/http"

	"github.com/SmartHealthNetwork/shn-gateway/internal/contentcoding"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// bodycap.go is the one bounded read of a request body. A body over the
// network's limit is refused 413 as it is, never cut short and then refused as
// whatever the cut-off bytes fail to parse as (the published protocol states
// the limit and its 413). The refusal is recorded as routing: no route on the
// network carries a body over its limit.

// errRequestTooLarge is the published answer to a request body over the
// limit, {"error":"request body too large"}.
const errRequestTooLarge = "request body too large"

// readCapped reads rd to its end and returns what it read, reading at most
// limit+1 bytes, so a stream that would inflate past the limit (a decoded
// body) is never read further. A body over limit is an *http.MaxBytesError;
// any other read error is returned as it is, with what was read before it.
// A caller discards the bytes returned with an error. The limit is applied to
// the bytes read, never to a stated length, which a decoded stream does not
// have.
func readCapped(rd io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(rd, limit+1))
	if err != nil {
		return b, err
	}
	if int64(len(b)) > limit {
		return nil, &http.MaxBytesError{Limit: limit}
	}
	return b, nil
}

// isBodyTooLarge reports whether err is a body over its limit: readCapped's,
// or one an http.MaxBytesReader beneath it returned.
func isBodyTooLarge(err error) bool {
	var tooLarge *http.MaxBytesError
	return errors.As(err, &tooLarge)
}

// errUnsupportedCoding is the answer to a request body sent in a content
// coding the gateway does not decode (contentcoding.ErrUnsupported).
const errUnsupportedCoding = "unsupported content coding"

// writeUnsupportedCoding answers a body sent in a content coding the gateway
// does not decode: 415, naming gzip, the one it accepts (RFC 9110 §15.5.16).
// Its bytes cannot be read as their sender declared them, so the caller
// records a refusal on message integrity, at every level.
func writeUnsupportedCoding(w http.ResponseWriter) {
	contentcoding.Advertise(w.Header())
	writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": errUnsupportedCoding})
}

// readIngressBody reads an ingress request's body up to limit. A body over it
// is refused 413 as routing; a body in a content coding the gateway does not
// decode, 415 as message integrity; any other read failure is answered 400 as
// before. It reports whether the body was read whole.
func readIngressBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	body, err := readCapped(r.Body, limit)
	if isBodyTooLarge(err) {
		exchangeOf(r.Context()).refusingFor(RefusalRouting, reasonRequestTooLarge)
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": errRequestTooLarge})
		return nil, false
	}
	if errors.Is(err, contentcoding.ErrUnsupported) {
		exchangeOf(r.Context()).refusingFor(RefusalIntegrity, reasonUnsupportedCoding)
		writeUnsupportedCoding(w)
		return nil, false
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body failed"})
		return nil, false
	}
	return body, true
}

// maxEnvelopeBytes is the largest envelope any leg may carry: an envelope is
// read up to it, and held to its own leg's limit (shnsdk.MaxRequestBytesFor)
// once its metadata names the leg.
var maxEnvelopeBytes = func() int {
	n := shnsdk.MaxRequestBytes
	for flat := range shnsdk.PrefixedTransactionTypes() {
		n = max(n, shnsdk.MaxRequestBytesFor(flat))
	}
	return n
}()

// errResponseTooLarge is the reason an answer over the network's limit is
// refused for: it is never relayed cut short. The recipient's system answered
// (it may have acted on the request), so the refusal says the request was
// delivered (HubDeliveredHeader yes) and the caller never resends it blindly.
const errResponseTooLarge = "response body too large"

// errAnswerRefusedDelivered is the requester's gateway's answer to its caller
// when the recipient's gateway refused its system's answer on a rule other
// than its size: delivered, so the caller checks before resending.
const errAnswerRefusedDelivered = "the recipient received this request and answered, but its gateway refused to send the answer; it may have acted on the request: check its outcome before resending"

// errAnswerTooLarge is a participant's system answering with a body over the
// limit this gateway reads.
var errAnswerTooLarge = errors.New(errResponseTooLarge)

// writeAnswerTooLarge is the requester's gateway's answer to its caller on an
// answer refused for size after the recipient answered: 502
// {"error":"response body too large"}, with HubDeliveredHeader yes.
func writeAnswerTooLarge(w http.ResponseWriter) {
	w.Header().Set(HubDeliveredHeader, "yes")
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": errResponseTooLarge})
}

// attestAnswerRefused is a recipient's gateway refusing to send its system's
// answer after the system answered: the one raw answer to the Hub on which a
// gateway sets HubDeliveredHeader yes, its statement that its system answered
// (it may have acted). The body names the refusal's rule beside its reason,
// {"error": reason, "rule": rule}, so the Hub relays which refusal it was by
// structure. rule is one of the closed refusal rules (vocab.RefusalRules).
func attestAnswerRefused(w http.ResponseWriter, rule, reason string) {
	w.Header().Set(HubDeliveredHeader, "yes")
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": reason, "rule": rule})
}

// answerRefusalRules are buildResponseLeg's refusals of an answer its system
// gave, by the reason it returns, each with its rule: each is attested
// delivered (attestAnswerRefused). Any other refusal of buildResponseLeg is
// this gateway's own failure, written as the {"error"} it names. A fidelity
// entry needs the attestation to state its stage first: the requester records
// a fidelity refusal only with its stage, so without one it would read as
// other.
var answerRefusalRules = map[string]string{
	errResponseTooLarge:               RefusalRouting,
	errAnswerTokenUnverified:          RefusalAuthority,
	errAnswerNoToken:                  RefusalAuthority,
	errAnswerDenied:                   RefusalAuthority,
	errAnswerAuditRefused:             RefusalAudit,
	errAnswerAuthorizationUnavailable: RefusalAuthority,
}

// writeResponseLegFailure writes buildResponseLeg's refusal. msg is this
// gateway's own, from buildResponseLeg.
func writeResponseLegFailure(w http.ResponseWriter, status int, msg string) {
	if rule, ok := answerRefusalRules[msg]; ok {
		attestAnswerRefused(w, rule, msg)
		return
	}
	writeJSON(w, status, map[string]string{"error": msg})
}
