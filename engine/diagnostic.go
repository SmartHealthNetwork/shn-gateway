package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"

	"github.com/SmartHealthNetwork/shn-gateway/diagnostics"
	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// findingEventsPerLeg caps how many conformance findings one leg captures as
// their own events at observe; the leg's result event still carries the true
// count, so a busy exchange cannot multiply the capture's volume.
const findingEventsPerLeg = diagnostics.FindingEventsPerLeg

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

// diagnosticFinding captures a conformance finding as its own metadata-only
// event (detail is the finding as its log line has it), keyed by the leg's
// correlation id. It speaks for this gateway's holder: a payer's as the leg's
// recipient, any other gateway's as its sender.
func (g *Gateway) diagnosticFinding(f ConformanceFinding, detail string) {
	// At observe the leg's tally counts every finding, and past the per-leg
	// cap the finding is not captured on its own; the result carries it.
	if f.tally != nil && !f.tally.finding() {
		return
	}
	if g.cfg.Diagnostic == nil {
		return
	}
	b := f.binding
	e := diagnostics.Event{Kind: diagnostics.KindConformanceFinding, CorrelationID: f.CorrelationID, LegType: f.LegType, ContractLine: f.Line, Detail: detail,
		CallID: b.callID, RequestCiphertextHash: b.hash, Sender: b.sender, Recipient: b.recipient}
	g.ownSide(&e)
	g.diagnostic(e)
}

// ownSide makes a captured record speak for this gateway's holder: a payer's
// as the leg's recipient, any other gateway's as its sender.
func (g *Gateway) ownSide(e *diagnostics.Event) {
	if g.cfg.Responder != nil {
		e.Recipient = g.cfg.HolderID
	} else if e.Sender == "" {
		e.Sender = g.cfg.HolderID
	}
}

// findingBindingFrom is the call a finding made under ctx belongs to: an
// observe worker's snapshot, or the request's own door call id and leg.
func findingBindingFrom(ctx context.Context) findingBinding {
	if b, ok := ctx.Value(findingBindingKey{}).(findingBinding); ok {
		return b
	}
	b := findingBinding{callID: diagnostics.CallID(ctx)}
	if leg, ok := ctx.Value(diagnosticLegKey{}).(*diagnosticLeg); ok {
		b.hash, b.sender, b.recipient = leg.hash, leg.sender, leg.recipient
	}
	return b
}

// diagnosticLegResult captures, at observe, one leg's conformance result: how
// many findings its checks recorded, whether per-finding events were capped
// (truncated) and whether a check was dropped unrun (incomplete). It is bound
// like the leg's access line, so the stats can tell a clean leg from one whose
// findings are still missing.
func (g *Gateway) diagnosticLegResult(rec ExchangeRecord, count int, truncated, incomplete bool) {
	// A leg refused before it was verified names no leg id, so nothing could
	// join its result; such a leg runs no payload check either.
	if g.cfg.Diagnostic == nil || rec.CorrelationID == "" {
		return
	}
	detail, _ := json.Marshal(struct {
		Count      int  `json:"count"`
		Truncated  bool `json:"truncated,omitempty"`
		Incomplete bool `json:"incomplete,omitempty"`
	}{count, truncated, incomplete})
	e := diagnostics.Event{Kind: diagnostics.KindConformanceResult, CorrelationID: rec.CorrelationID, LegType: rec.Exchange, ContractLine: rec.ContractLine,
		CallID: rec.CallID, RequestCiphertextHash: rec.RequestCiphertextHash, Sender: rec.Sender, Recipient: rec.Recipient, Detail: string(detail)}
	g.ownSide(&e)
	g.diagnostic(e)
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

// diagnosticSealed records an origination leg's payload as sealed for
// recipient, naming the registered edits it was transmitted with.
func (g *Gateway) diagnosticSealed(ctx context.Context, env shnsdk.Envelope, recipient, correlationID, txType, contractLine string, payload []byte, edits []relay.EditID) {
	g.diagnostic(diagnostics.Event{Kind: "leg.sealed", CallID: diagnostics.CallID(ctx), RequestFingerprint: diagnostics.IngressFingerprint(ctx), RequestCiphertextHash: sha256hex(env.Ciphertext), Sender: g.cfg.HolderID, Recipient: recipient, CorrelationID: correlationID, LegType: txType, ContractLine: contractLine, Body: payload, BodyComplete: true, Detail: relayEditsDetail(edits)})
}

// relayEditsDetail is the Detail of a captured transmit that applied edits
// (diagnostics.RelayEditsDetail): their registered ids, each once, in order;
// "" when it applied none, so such a transmit is captured as before.
func relayEditsDetail(edits []relay.EditID) string {
	var ids []string
	for _, id := range edits {
		if slices.Contains(relay.EditIDs(), id) && !slices.Contains(ids, string(id)) {
			ids = append(ids, string(id))
		}
	}
	if len(ids) == 0 {
		return ""
	}
	b, err := json.Marshal(diagnostics.RelayEditsDetail{RelayEdits: ids})
	if err != nil {
		return ""
	}
	return string(b)
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
	n.emitDiagnosticDetail(ctx, kind, body, detail == "", status, detail, r, headers)
}

// emitDiagnosticDetail is emitDiagnostic for an event whose detail is not a
// note on its body's completeness (a forward naming its edits).
func (n *nativeResponder) emitDiagnosticDetail(ctx context.Context, kind string, body []byte, complete bool, status int, detail string, r *http.Request, headers http.Header) {
	if n.diagnostic == nil {
		return
	}
	defer func() { _ = recover() }()
	e := diagnostics.RequestIdentity(ctx, diagnostics.Event{Time: n.clock(), Kind: kind, Body: body, BodyComplete: complete, Status: status, Detail: detail, Method: r.Method, URL: r.URL.String()})
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
