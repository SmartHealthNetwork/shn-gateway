package subscription

import (
	"net/http"
	"strings"

	"golang.org/x/net/http/httpguts"
)

// HeaderFields reads a Subscription's channel.header values as the HTTP
// fields the subscriber's gateway presents to the subscriber's endpoint with
// each notification: each
// "name: value", split at the first colon, the value's surrounding spaces and
// tabs trimmed, as a server reads a field line. ok is false when a value is
// not one (no colon, a name that is not an HTTP token, or a value an HTTP
// field cannot carry): such a value could never be presented as written, so
// it is refused where it is written and where it would be delivered, and
// never named in the refusal, since it may be the subscriber's secret.
func HeaderFields(values []string) (http.Header, bool) {
	h := http.Header{}
	for _, v := range values {
		name, value, found := strings.Cut(v, ":")
		value = strings.Trim(value, " \t")
		if !found || !httpguts.ValidHeaderFieldName(name) || !httpguts.ValidHeaderFieldValue(value) {
			return nil, false
		}
		h.Add(name, value)
	}
	return h, true
}

// PresentableHeaders reports whether s's channel.header values, when it
// states any, are each an HTTP field HeaderFields reads (at create, the
// subscriber's gateway refuses a channel it could never deliver with). A
// header that is not an array of strings is not judged here: the seal
// (SealOutbound) refuses it on its own rule.
func (s *Subscription) PresentableHeaders() bool {
	values, ok := s.headers()
	if !ok {
		return true
	}
	_, ok = HeaderFields(values)
	return ok
}
