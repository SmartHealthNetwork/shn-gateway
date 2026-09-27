package engine

import (
	"context"
	"net/http"

	"github.com/SmartHealthNetwork/shn-gateway/diagnostics"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// DiagnosticSink receives diagnostic events (Config.Diagnostic,
// WithNativeDiagnostic). The engine's uses of the diagnostics package live in
// this file and observer.go, the capture call sites, so only they import it.
type DiagnosticSink = func(diagnostics.Event) bool

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

// diagnosticHubRefusal records the Hub's non-2xx answer to an origination leg.
func (g *Gateway) diagnosticHubRefusal(ctx context.Context, resp *http.Response, respBody []byte) {
	if g.cfg.Diagnostic == nil {
		return
	}
	headers, complete := diagnosticHeaders(resp.Header)
	g.diagnosticEvent(ctx, diagnostics.Event{Kind: "leg.failed", Status: resp.StatusCode, Body: respBody, BodyComplete: len(respBody) < shnsdk.MaxResponseBytes, Headers: headers, HeadersComplete: complete, Detail: "Hub response"})
}

// diagnosticSealed records an origination leg's payload as sealed for recipient.
func (g *Gateway) diagnosticSealed(ctx context.Context, env shnsdk.Envelope, recipient, correlationID, txType, contractLine string, payload []byte) {
	g.diagnostic(diagnostics.Event{Kind: "leg.sealed", CallID: diagnostics.CallID(ctx), RequestFingerprint: diagnostics.IngressFingerprint(ctx), RequestCiphertextHash: sha256hex(env.Ciphertext), Sender: g.cfg.HolderID, Recipient: recipient, CorrelationID: correlationID, LegType: txType, ContractLine: contractLine, Body: payload, BodyComplete: true})
}

// withDiagnosticIdentity attributes r's diagnostics to a verified inbound
// envelope.
func withDiagnosticIdentity(r *http.Request, env shnsdk.Envelope) *http.Request {
	return r.WithContext(diagnostics.WithRequestIdentity(r.Context(), sha256hex(env.Ciphertext), env.Metadata.Sender, env.Metadata.Recipient, env.Metadata.CorrelationID))
}

// WithNativeDiagnostic observes native boundary stages. A nil sink disables it.
// Sinks must be concurrency-safe and reserve bounded memory before retaining bytes.
func WithNativeDiagnostic(sink DiagnosticSink) NativeOption {
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
