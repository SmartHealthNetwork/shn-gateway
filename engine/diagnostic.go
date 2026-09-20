package engine

import (
	"context"
	"net/http"

	"github.com/SmartHealthNetwork/shn-gateway/diagnostics"
)

// diagnostic passes read-only bytes to the configured bounded sink. Sink faults
// cannot escape into participant exchange handling.
func (g *Gateway) diagnostic(e diagnostics.Event) {
	if g == nil || g.cfg.Diagnostic == nil {
		return
	}
	defer func() { _ = recover() }()
	if e.Time.IsZero() {
		e.Time = g.cfg.Clock()
	}
	g.cfg.Diagnostic(e)
}
func (g *Gateway) diagnosticStage(ctx context.Context, kind, leg string, body []byte, status int, detail string) {
	if g.cfg.Diagnostic == nil {
		return
	}
	g.diagnosticEvent(ctx, diagnostics.Event{Kind: kind, LegType: leg, Body: body, BodyComplete: true, Status: status, Detail: detail})
}
func (g *Gateway) diagnosticEvent(ctx context.Context, e diagnostics.Event) {
	e = diagnostics.RequestIdentity(ctx, e)
	e.RequestFingerprint = diagnostics.IngressFingerprint(ctx)
	if leg, ok := ctx.Value(diagnosticLegKey{}).(*diagnosticLeg); ok {
		e.RequestCiphertextHash, e.Sender, e.Recipient, e.CorrelationID = leg.hash, leg.sender, leg.recipient, leg.correlation
	}
	g.diagnostic(e)
}

// WithNativeDiagnostic observes native boundary stages. A nil sink disables it.
// Sinks must be concurrency-safe and reserve bounded memory before retaining bytes.
func WithNativeDiagnostic(sink func(diagnostics.Event) bool) NativeOption {
	return func(n *nativeResponder) { n.diagnostic = sink }
}
func (n *nativeResponder) emitDiagnostic(ctx context.Context, kind string, body []byte, status int, detail string, r *http.Request, headers http.Header) {
	if n.diagnostic == nil {
		return
	}
	defer func() { _ = recover() }()
	e := diagnostics.RequestIdentity(ctx, diagnostics.Event{Time: n.clock(), Kind: kind, Body: body, BodyComplete: detail == "", Status: status, Detail: detail, Method: r.Method, URL: r.URL.String()})
	e.Headers, e.HeadersComplete = diagnosticHeaders(headers)
	n.diagnostic(e)
}
func diagnosticReadDetail(err error, n int) string {
	if err != nil {
		return "read failed; observed prefix only"
	}
	if n >= maxPartnerBody {
		return "read limit reached; completeness unknown"
	}
	return ""
}

type diagnosticLegKey struct{}
type diagnosticLeg struct{ hash, sender, recipient, correlation string }

func (g *Gateway) observeInbound(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if g.cfg.Diagnostic == nil {
			h(w, r)
			return
		}
		leg := &diagnosticLeg{}
		r = r.WithContext(context.WithValue(r.Context(), diagnosticLegKey{}, leg))
		diagnostics.ObserveHTTP(h, func(e diagnostics.Event) bool {
			e.RequestCiphertextHash, e.Sender, e.Recipient, e.CorrelationID = leg.hash, leg.sender, leg.recipient, leg.correlation
			g.diagnostic(e)
			return true
		}, func(*http.Request) diagnostics.HTTPInfo { return diagnostics.HTTPInfo{Kind: "recipient.http"} }, g.cfg.Clock, 8<<20, nil).ServeHTTP(w, r)
	}
}

// Headers share the HTTP observer's finite per-boundary budget. An over-budget
// header block is explicitly unavailable; the exchange and captured body remain unchanged.
func diagnosticHeaders(h http.Header) (http.Header, bool) {
	size := 0
	for k, values := range h {
		size += 256 + len(k)
		if size > 64<<10 {
			return nil, false
		}
		for _, v := range values {
			size += 32 + len(v)
			if size > 64<<10 {
				return nil, false
			}
		}
	}
	// Like Body, this is a synchronous read-only view. The bounded sink reserves
	// capacity before copying it; do not allocate a second pre-reservation snapshot.
	return h, true
}
