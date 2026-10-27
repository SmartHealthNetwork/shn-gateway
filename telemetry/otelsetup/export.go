package otelsetup

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
	"github.com/SmartHealthNetwork/shn-gateway/telemetry"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// Export failure codes: the /internal/checks codes a failed export maps to
// (MON-R13). They are the checks package's existing codes, spelled here so
// this package does not import it.
const (
	CodeUnreachable        = "unreachable"
	CodeCredentialRejected = "credential-rejected"
	CodeHTTPStatus         = "http-status"
	CodeInternal           = "internal"
)

// ExportState is the cached outcome of the newest export, read by the
// telemetry-export check without contacting the vendor (MON-R13).
type ExportState struct {
	// Present is whether an OTLP exporter is on: the check exists.
	Present bool
	// Exported is whether any export has completed.
	Exported bool
	OK       bool
	// Code and Hint classify a failure: Hint is a status ("HTTP 401",
	// "gRPC Unauthenticated"), never an error's text, a URL or a body
	// (MON-R10).
	Code string
	Hint string
	At   time.Time
	// Failures is the number of failed exports in a row.
	Failures int
}

// attempt is what the transport saw of the last request an export sent:
// classified structurally, from a status, never from an error's text.
type attempt struct {
	sent      bool
	transport bool // the request never got an answer: connect, TLS or timeout
	httpCode  int
	grpcCode  codes.Code
	isGRPC    bool
}

// probe records each request's attempt for the export in progress. Exports
// are serial (one periodic reader), so the last attempt is the export's.
type probe struct {
	mu   sync.Mutex
	last attempt
}

func (p *probe) reset() {
	p.mu.Lock()
	p.last = attempt{}
	p.mu.Unlock()
}

func (p *probe) set(a attempt) {
	p.mu.Lock()
	p.last = a
	p.mu.Unlock()
}

func (p *probe) get() attempt {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

// roundTripper records each HTTP export request's answer.
type roundTripper struct {
	next  http.RoundTripper
	probe *probe
}

func (t roundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(r)
	if err != nil {
		t.probe.set(attempt{sent: true, transport: true})
		return resp, err
	}
	t.probe.set(attempt{sent: true, httpCode: resp.StatusCode})
	return resp, nil
}

// unaryInterceptor records each gRPC export call's status.
func (p *probe) unaryInterceptor(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	err := invoker(ctx, method, req, reply, cc, opts...)
	p.set(attempt{sent: true, isGRPC: true, grpcCode: status.Code(err)})
	return err
}

// peerCertificate records the collector's certificate from each gRPC
// call's peer, under otlp (MON-R25): a gRPC answer carries no
// *http.Response for the recorder's wrapper to read. The handshake has
// verified it already; recording never fails a call.
func peerCertificate(rec *endpointcert.Recorder) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		var p peer.Peer
		err := invoker(ctx, method, req, reply, cc, append(opts, grpc.Peer(&p))...)
		if info, ok := p.AuthInfo.(credentials.TLSInfo); ok {
			rec.RecordConnection(endpointcert.Of(endpointcert.OTLP), info.State)
		}
		return err
	}
}

// exporter wraps the OTLP exporter: it classifies and counts each export
// (MON-R13), keeps the cached state, and logs a failure on its 1st, 2nd,
// 4th, 8th… occurrence in a row with its class only (MON-R10). It returns
// nil to the reader, so no raw export error reaches the SDK's handler.
type exporter struct {
	sdkmetric.Exporter
	probe *probe
	reg   atomic.Pointer[telemetry.Registry]
	now   func() time.Time
	logf  func(string, ...any)

	mu      sync.Mutex
	state   ExportState
	dropped atomic.Int64
}

func (e *exporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	e.probe.reset()
	err := e.Exporter.Export(ctx, rm)
	if err != nil && errors.Is(ctx.Err(), context.Canceled) {
		// The stop cut a periodic export short: it is neither accepted nor
		// refused, so it is neither counted nor logged; the final export
		// follows.
		return nil
	}
	v := classify(err, e.probe.get())
	outcome, code, hint := v.outcome, v.code, v.hint
	e.reg.Load().Add(telemetry.TelemetryExports, 1, outcome)

	e.mu.Lock()
	prev := e.state.Failures
	e.state = ExportState{Present: true, Exported: true, OK: outcome != "failed", Code: v.code, Hint: v.hint, At: e.now()}
	if outcome == "failed" {
		e.state.Failures = prev + 1
	}
	n := e.state.Failures
	e.mu.Unlock()

	switch {
	case outcome == "failed" && n&(n-1) == 0:
		e.logf("gateway: telemetry: OTLP export failed (%s%s); %d in a row, logged at 1, 2, 4, 8 and so on", code, hintSuffix(hint), n)
	case outcome != "failed" && prev > 0:
		e.logf("gateway: telemetry: OTLP export recovered after %d failed in a row", prev)
	}
	if outcome == "dropped" {
		if d := e.dropped.Add(1); d&(d-1) == 0 {
			e.logf("gateway: telemetry: the OTLP endpoint accepted an export but rejected some of its data points (%d so far, logged at 1, 2, 4, 8 and so on)", d)
		}
	}
	return nil
}

func hintSuffix(h string) string {
	if h == "" {
		return ""
	}
	return ", " + h
}

// verdict is one export's classification: its outcome and, for a failure,
// its check code and hint.
type verdict struct{ outcome, code, hint string }

// classify maps an export to its outcome and, for a failure, its check code
// and hint (MON-R13): connect, TLS and timeout are unreachable; 401 and 403
// credential-rejected; any other failing status http-status; the rest
// internal. An export the endpoint accepted while the exporter still
// reported an error was accepted with data points rejected: dropped.
func classify(err error, a attempt) verdict {
	accepted := a.sent && !a.transport && ((a.isGRPC && a.grpcCode == codes.OK) || (!a.isGRPC && a.httpCode >= 200 && a.httpCode <= 299))
	switch {
	case err == nil:
		return verdict{outcome: "ok"}
	case accepted:
		return verdict{outcome: "dropped"}
	case a.transport:
		return verdict{outcome: "failed", code: CodeUnreachable}
	case a.isGRPC:
		switch a.grpcCode {
		case codes.Unauthenticated, codes.PermissionDenied:
			return verdict{outcome: "failed", code: CodeCredentialRejected, hint: "gRPC " + a.grpcCode.String()}
		case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
			return verdict{outcome: "failed", code: CodeUnreachable, hint: "gRPC " + a.grpcCode.String()}
		}
		return verdict{outcome: "failed", code: CodeHTTPStatus, hint: "gRPC " + a.grpcCode.String()}
	case a.sent && (a.httpCode == http.StatusUnauthorized || a.httpCode == http.StatusForbidden):
		return verdict{outcome: "failed", code: CodeCredentialRejected, hint: fmt.Sprintf("HTTP %d", a.httpCode)}
	case a.sent && a.httpCode != 0:
		return verdict{outcome: "failed", code: CodeHTTPStatus, hint: fmt.Sprintf("HTTP %d", a.httpCode)}
	case ctxDone(err):
		// The export's own deadline ran out before any request was answered.
		return verdict{outcome: "failed", code: CodeUnreachable}
	}
	return verdict{outcome: "failed", code: CodeInternal}
}

func ctxDone(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

func (e *exporter) State() ExportState {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state
}
