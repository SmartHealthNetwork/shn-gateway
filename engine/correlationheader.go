package engine

import (
	"context"
	"log"
	"net/http"
	"regexp"
)

// CorrelationHeader is the response header every Da Vinci ingress answer carries:
// the value a participant's system quotes to find its call. A caller may send its
// own X-Correlation-Id; a well-formed one (1–64 characters of letters, digits,
// `.`, `_` or `-`) is the request's TRACE value and comes back unchanged, so the
// value the caller already tracks is the one it quotes. Anything else is ignored
// and the answer carries the leg's id instead. A PAS submit or update that names
// its correlation in the Claim (`urn:shn:correlation`) is answered with the
// Claim's value.
//
// The caller's value is never the leg's own id. Each call's leg is sent under a
// freshly minted correlation id (or, on a PAS submit or update, the Claim's own
// `urn:shn:correlation`, the payer's key for that authorization), so a caller
// that reuses its trace id, or retries, is never refused for it and never lands
// on another request's payer-side record. LegIDHeader names the leg's id on
// every answer, and each call logs its trace value beside the leg's id, so the
// operator goes from the value the caller quotes to the leg.
const CorrelationHeader = "X-Correlation-Id"

// LegIDHeader is the response header carrying the id this call's leg was sent
// under — the correlation id this gateway's and the payer gateway's leg lines are
// logged under — or, on an answer given before any leg was sent (an
// authentication or routing refusal), the id it would have been sent under.
const LegIDHeader = "X-SHN-Leg-Id"

// correlationShape bounds a caller-supplied correlation id: one log token, no
// whitespace, quotes or control characters, short enough to quote.
var correlationShape = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type ingressCorrelationKey struct{}

// ingressIDs is what the wrapper settles for one request: the caller's trace
// value ("" when it sent none, or one outside the shape) and the leg's id.
type ingressIDs struct{ trace, leg string }

// withIngressCorrelation settles the request's trace value and leg id before the
// handler runs and stamps both on the response, so every answer the handler
// writes — the earliest 401 included — carries them.
func (g *Gateway) withIngressCorrelation(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ids := ingressIDs{leg: g.mintCorrelationID()}
		if sent := r.Header.Get(CorrelationHeader); correlationShape.MatchString(sent) {
			ids.trace = sent
		}
		stampIngressIDs(w, ids.trace, ids.leg)
		h(w, r.WithContext(context.WithValue(r.Context(), ingressCorrelationKey{}, ids)))
	}
}

// stampIngressIDs writes the two headers: the trace value the caller quotes (the
// leg's id when there is none) and the leg's id.
func stampIngressIDs(w http.ResponseWriter, trace, leg string) {
	quoted := trace
	if quoted == "" {
		quoted = leg
	}
	w.Header().Set(CorrelationHeader, quoted)
	w.Header().Set(LegIDHeader, leg)
}

// ingressCorrelation returns the leg id settled for this request, and logs the
// caller's trace value beside it. A handler reached without the wrapper (a test
// driving it directly) mints one here and stamps it, so the header contract
// holds either way.
func (g *Gateway) ingressCorrelation(w http.ResponseWriter, r *http.Request) string {
	ids, ok := r.Context().Value(ingressCorrelationKey{}).(ingressIDs)
	if !ok || ids.leg == "" {
		ids = ingressIDs{leg: g.mintCorrelationID()}
		stampIngressIDs(w, "", ids.leg)
	}
	noteIngressTrace(r, ids.trace, ids.leg)
	return ids.leg
}

// ingressTrace is the caller's trace value for this request, or "".
func ingressTrace(r *http.Request) string {
	ids, _ := r.Context().Value(ingressCorrelationKey{}).(ingressIDs)
	return ids.trace
}

// noteIngressTrace logs which leg a caller's trace value became, so the value
// the caller quotes finds the leg lines (which carry the leg's id).
func noteIngressTrace(r *http.Request, trace, leg string) {
	if trace != "" && trace != leg {
		log.Printf("gateway: ingress %s %s: trace %s → leg %s", r.Method, r.URL.Path, trace, leg)
	}
}

// mintCorrelationID uses the configured generator, or the default when a
// Gateway was built without the constructor (tests).
func (g *Gateway) mintCorrelationID() string {
	if g.cfg.CorrelationGen != nil {
		return g.cfg.CorrelationGen()
	}
	return newCorrelationID()
}
