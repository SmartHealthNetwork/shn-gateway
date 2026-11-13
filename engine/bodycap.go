package engine

import (
	"bytes"
	"encoding/json"
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
// "response body too large", delivered yes (writeDeliveredMaybe).
func writeAnswerTooLarge(w http.ResponseWriter) {
	writeDeliveredMaybe(w, http.StatusBadGateway, deliveredYes, errResponseTooLarge)
}

// deliveredMaybe is what the requester's gateway tells its caller about a
// request that failed after it may have reached the recipient, as
// HubDeliveredHeader states it: yes, the recipient received it and answered
// (it may have acted on it); unknown, it may have received it. A request the
// recipient did not receive has no value here: its answer is an ordinary
// refusal (writeJSON), which states no delivery.
type deliveredMaybe string

const (
	deliveredYes     deliveredMaybe = "yes"
	deliveredUnknown deliveredMaybe = "unknown"
)

// deliveredSystem names what a delivered answer states: its problem type is
// deliveredSystem + "/" + the value, and on a FHIR operation route it is the
// system of the issue's details coding, whose code is the value.
const deliveredSystem = "tag:smarthealthnetwork.org,2026:gateway/delivered"

// deliveredTitles is the fixed title of each delivered answer's problem type.
// The unknown title never says the recipient answered. The yes title names a
// recipient, not the one: on a call that sends several legs, the recipient
// that answered may be an earlier leg's.
var deliveredTitles = map[deliveredMaybe]string{
	deliveredYes:     "A recipient received this request and may have acted on it",
	deliveredUnknown: "The recipient may have received this request and acted on it",
}

// problemDetailsWriter marks a route whose error media type is outside the
// gateway's full promise: an operator route (routes() mounts every one behind
// it), the /scenario routes, which STABILITY.md classes as test tooling. Its
// delivered answers are application/problem+json. Every other
// route keeps the media type its errors have always had (application/json,
// or an OperationOutcome on a FHIR operation route), and gains the members
// and the header alone.
type problemDetailsWriter struct{ http.ResponseWriter }

// Unwrap exposes the underlying writer (http.ResponseController, scopeOf).
func (w *problemDetailsWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// withProblemDetails is h writing through a problemDetailsWriter: an
// operator route's handler, as routes() mounts it.
func withProblemDetails(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { h(&problemDetailsWriter{w}, r) }
}

// problemDetailsRoute reports whether w, or a writer it wraps, is a
// problemDetailsWriter.
func problemDetailsRoute(w http.ResponseWriter) bool {
	for w != nil {
		if _, ok := w.(*problemDetailsWriter); ok {
			return true
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return false
		}
		w = u.Unwrap()
	}
	return false
}

// writeDeliveredMaybe is the requester's gateway's answer to its caller on a
// request that failed after the recipient may have received it (delivered yes
// or unknown): the Hub's error marked so, or one this gateway reads so itself
// (relayOriginationError). The status is what the failure answers; text says
// what happened. It is the refusal {"error": text} written by writeRefusal,
// which states delivered by structure (markDelivered), so a system never has
// to read text to know not to resend blindly: yes once a leg of the call was
// answered, whatever this failure says.
func writeDeliveredMaybe(w http.ResponseWriter, status int, delivered deliveredMaybe, text string) {
	writeRefusal(w, status, map[string]string{"error": text}, delivered)
}

// isFHIROperation reports whether w is a FHIR operation route's writer, as
// writeJSON reads it.
func isFHIROperation(w http.ResponseWriter) bool {
	_, ok := w.(*fhirOperationWriter)
	return ok
}

// markDelivered is b, an error answer this gateway built (writeRefusal), with
// the marks that state delivered by structure, decided by the body's own
// shape, every member it had kept and its status unchanged:
//   - an OperationOutcome (a FHIR operation route's refusal) gains, on its
//     first issue, details: a CodeableConcept whose one coding has system
//     deliveredSystem and code delivered; its other issues (a validator's
//     echoed issues) are kept;
//   - any other JSON object (the {"error"} body every other route answers)
//     gains the RFC 9457 problem details members after its own: type
//     (deliveredSystem + "/" + delivered), title (deliveredTitles), status,
//     and detail, the text of its error member; its error member keeps its
//     place and text, so {"error": text} reads
//     {"error":text,"type":…,"title":…,"status":…,"detail":text}.
//
// It edits only a body this gateway builds (writeRefusal is its one caller,
// TestRefusals_AllGoThroughWriteRefusal): an answer carried from a recipient
// keeps its bytes, and gains only the header (deliveryWriter.WriteHeader).
//
// problem reports the second, whose media type is application/problem+json
// on a route outside the full promise (problemDetailsWriter). A body that is
// neither, or that already has one of the members, is left as it is (nil):
// the header alone states it.
func markDelivered(b []byte, status int, delivered deliveredMaybe) (marked []byte, problem bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var doc map[string]any
	if len(b) == 0 || dec.Decode(&doc) != nil || doc == nil {
		return nil, false
	}
	if doc["resourceType"] == "OperationOutcome" {
		issues, _ := doc["issue"].([]any)
		if len(issues) == 0 {
			return nil, false
		}
		first, ok := issues[0].(map[string]any)
		if _, has := first["details"]; !ok || has {
			return nil, false
		}
		first["details"] = map[string]any{"coding": []any{map[string]any{"system": deliveredSystem, "code": string(delivered)}}}
		out, err := json.Marshal(doc)
		if err != nil {
			return nil, false
		}
		return append(out, '\n'), false
	}
	for _, member := range []string{"type", "title", "status", "detail"} {
		if _, has := doc[member]; has {
			return nil, false
		}
	}
	detail, _ := doc["error"].(string)
	members, err := json.Marshal(struct {
		Type   string `json:"type"`
		Title  string `json:"title"`
		Status int    `json:"status"`
		Detail string `json:"detail,omitempty"`
	}{deliveredSystem + "/" + string(delivered), deliveredTitles[delivered], status, detail})
	body := bytes.TrimRight(b, "\n")
	if err != nil || len(body) < 2 || body[0] != '{' || body[len(body)-1] != '}' {
		return nil, false
	}
	out := append([]byte(nil), body[:len(body)-1]...)
	if len(bytes.TrimSpace(body[1:len(body)-1])) > 0 {
		out = append(out, ',')
	}
	out = append(out, members[1:len(members)-1]...)
	return append(out, '}', '\n'), true
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
