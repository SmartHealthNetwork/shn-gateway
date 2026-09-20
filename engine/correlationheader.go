package engine

import (
	"context"
	"net/http"
	"regexp"
)

// CorrelationHeader is the response header every Da Vinci ingress answer carries:
// the correlation id this gateway logs the request's leg under (the `certify:`
// and `leg.failed` lines), so a participant's system can quote one value and the
// operator can find the exchange it names. A refusal answered before any leg was
// originated carries it too, so the call can still be found at the edge that
// answered it.
//
// A caller may send its own X-Correlation-Id. A well-formed one (1–64 characters
// of letters, digits, `.`, `_` or `-`) becomes the correlation id of the leg, so
// the value the caller already tracks is the value in this gateway's log; anything
// else is ignored and an id is minted. A PAS submit that names its correlation in
// the Claim (`urn:shn:correlation`) keeps that contract: the Claim's value wins
// and the header reports it.
const CorrelationHeader = "X-Correlation-Id"

// correlationShape bounds a caller-supplied correlation id: one log token, no
// whitespace, quotes or control characters, short enough to quote.
var correlationShape = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type ingressCorrelationKey struct{}

// withIngressCorrelation settles the request's correlation id before the
// handler runs and stamps it on the response, so every answer the handler writes
// — the earliest 401 included — carries it.
func (g *Gateway) withIngressCorrelation(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(CorrelationHeader)
		if !correlationShape.MatchString(id) {
			id = g.mintCorrelationID()
		}
		w.Header().Set(CorrelationHeader, id)
		h(w, r.WithContext(context.WithValue(r.Context(), ingressCorrelationKey{}, id)))
	}
}

// ingressCorrelation returns the correlation id settled for this request. A
// handler reached without the wrapper (a test driving it directly) mints one
// here and stamps it, so the header contract holds either way.
func (g *Gateway) ingressCorrelation(w http.ResponseWriter, r *http.Request) string {
	if id, ok := r.Context().Value(ingressCorrelationKey{}).(string); ok && id != "" {
		return id
	}
	id := g.mintCorrelationID()
	w.Header().Set(CorrelationHeader, id)
	return id
}

// mintCorrelationID uses the configured generator, or the default when a
// Gateway was built without the constructor (tests).
func (g *Gateway) mintCorrelationID() string {
	if g.cfg.CorrelationGen != nil {
		return g.cfg.CorrelationGen()
	}
	return newCorrelationID()
}
