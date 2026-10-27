package otelsetup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
	"github.com/SmartHealthNetwork/shn-gateway/internal/otlptest"
	"github.com/SmartHealthNetwork/shn-gateway/telemetry"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/grpc/codes"
)

// kind is a compiled-in leg, whichever workstreams the build carries.
var kind = engine.ExchangeKinds[0]

// secret values the tests plant where a careless path would log them.
const (
	secretToken = "s3cr3t-export-token"
	secretPath  = "/secret-collector-path"
)

var testIdentity = Identity{Version: "v1.0.0-test", HolderID: "h-own-gateway", Role: "payer", MetricsService: "svc-from-metrics", MetricsEnv: "test-env",
	Hostname: func() (string, error) { return "host-a", nil }}

// logs captures lines logged through Options.Logf and the process logger.
type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logs) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// captureProcessLog sends the process logger to a buffer for the test.
func captureProcessLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// build builds the export to the collector at endpoint over protocol, with
// extra settings, a registry over its sink, and the captured logs.
func build(t *testing.T, protocol, endpoint string, extra map[string]string) (*Telemetry, *telemetry.Registry, *logs) {
	t.Helper()
	m := map[string]string{
		"OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_PROTOCOL": protocol,
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": endpoint,
		"OTEL_EXPORTER_OTLP_HEADERS":          "authorization=Bearer%20" + secretToken,
		"OTEL_METRIC_EXPORT_TIMEOUT":          "2000",
		"OTEL_EXPORTER_OTLP_TIMEOUT":          "2000",
	}
	for k, v := range extra {
		m[k] = v
	}
	c := Load(env(m), nil)
	if c.OTLP == nil {
		t.Fatalf("OTLP off: %+v", c.Problems)
	}
	l := &logs{}
	tel := Build(c, testIdentity, Options{Logf: l.logf})
	if tel.Sink() == nil {
		t.Fatalf("no sink built: %q", tel.Lines())
	}
	t.Cleanup(func() { tel.Shutdown(context.Background()) })
	reg := telemetry.New(tel.Sink())
	tel.Bind(reg)
	return tel, reg, l
}

// exportOnce runs one collection and export.
func exportOnce(tel *Telemetry) { tel.export(context.Background(), tel.cfg.ExportTimeout) }

// endpoints are the collector's endpoint per protocol.
func endpoints(t *testing.T, c *otlptest.Collector) map[string]string {
	return map[string]string{
		ProtocolHTTP: c.HTTP(t).URL + "/v1/metrics",
		ProtocolGRPC: c.GRPC(t),
	}
}

func findMetric(t *testing.T, text, name string) string {
	t.Helper()
	i := strings.Index(text, "  "+name+" ")
	if i < 0 {
		t.Fatalf("%s not exported:\n%s", name, text)
	}
	rest := text[i+2:]
	if j := strings.Index(rest, "\n  shn."); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// MON-R3, MON-R5: both protocols deliver the measurements with the headers
// set and the gateway's resource.
func TestExport_DeliversOverHTTPAndGRPC_MON_R3_R5(t *testing.T) {
	for _, protocol := range []string{ProtocolHTTP, ProtocolGRPC} {
		t.Run(protocol, func(t *testing.T) {
			col := otlptest.New()
			tel, reg, _ := build(t, protocol, endpoints(t, col)[protocol], nil)
			reg.Add(telemetry.Exchanges, 1, "inbound", kind, "answered")
			reg.RecordDuration(telemetry.ExchangeDuration, 1500*time.Millisecond, "inbound", kind)
			exportOnce(tel)
			got := col.Exports()
			if len(got) != 1 {
				t.Fatalf("%d exports, want 1", len(got))
			}
			if got[0].Headers["authorization"] != "Bearer "+secretToken {
				t.Fatalf("authorization header %q", got[0].Headers["authorization"])
			}
			text := otlptest.Render(got[0].Request, otlptest.Options{})
			for _, want := range []string{
				"service.name=svc-from-metrics", "service.version=v1.0.0-test", "service.instance.id=" + InstanceID(testIdentity.Hostname),
				"deployment.environment.name=test-env", "shn.holder_id=h-own-gateway", "shn.role=payer",
				"scope " + scopeName + " v1.0.0-test",
				`shn.gateway.exchanges unit="{exchange}" sum monotonic=true AGGREGATION_TEMPORALITY_CUMULATIVE`,
				"{shn.direction=inbound,shn.exchange=" + kind + ",shn.outcome=answered} 1",
				`shn.gateway.exchange.duration unit="s" histogram AGGREGATION_TEMPORALITY_CUMULATIVE`,
				"count=1 sum=1.5 bounds=[0.005 0.01 0.025 0.05 0.1 0.25 0.5 1 2.5 5 10 15 20 25 30 60]",
			} {
				if !strings.Contains(text, want) {
					t.Errorf("export lacks %q:\n%s", want, text)
				}
			}
			if st := tel.ExportCheck(); !st.Present || !st.Exported || !st.OK {
				t.Fatalf("export check %+v", st)
			}
		})
	}
}

// MON-R4: delta temporality and exponential histograms are what the
// settings select.
func TestExport_TemporalityAndHistogramSelection_MON_R4(t *testing.T) {
	col := otlptest.New()
	tel, reg, _ := build(t, ProtocolHTTP, col.HTTP(t).URL+"/v1/metrics", map[string]string{
		"OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE":        "delta",
		"OTEL_EXPORTER_OTLP_METRICS_DEFAULT_HISTOGRAM_AGGREGATION": "base2_exponential_bucket_histogram",
	})
	reg.Add(telemetry.Exchanges, 1, "inbound", kind, "answered")
	reg.RecordDuration(telemetry.ExchangeDuration, 2*time.Second, "inbound", kind)
	reg.Record(telemetry.AuditQueueDepth, 4)
	exportOnce(tel)
	text := otlptest.Render(col.Exports()[0].Request, otlptest.Options{})
	for _, want := range []string{
		`shn.gateway.exchanges unit="{exchange}" sum monotonic=true AGGREGATION_TEMPORALITY_DELTA`,
		`shn.gateway.exchange.duration unit="s" exponential_histogram AGGREGATION_TEMPORALITY_DELTA`,
		`shn.gateway.audit.queue.depth unit="{record}" gauge`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("export lacks %q:\n%s", want, text)
		}
	}
}

func TestTemporalitySelector_MON_R4(t *testing.T) {
	kinds := []sdkmetric.InstrumentKind{sdkmetric.InstrumentKindCounter, sdkmetric.InstrumentKindUpDownCounter, sdkmetric.InstrumentKindHistogram,
		sdkmetric.InstrumentKindGauge, sdkmetric.InstrumentKindObservableCounter, sdkmetric.InstrumentKindObservableUpDownCounter, sdkmetric.InstrumentKindObservableGauge}
	d, c := metricdata.DeltaTemporality, metricdata.CumulativeTemporality
	for pref, want := range map[string][]metricdata.Temporality{
		TemporalityCumulative: {c, c, c, c, c, c, c},
		TemporalityDelta:      {d, c, d, c, d, c, c},
		TemporalityLowMemory:  {d, c, d, c, c, c, c},
	} {
		sel := temporalitySelector(pref)
		for i, k := range kinds {
			if got := sel(k); got != want[i] {
				t.Errorf("%s %v = %v, want %v", pref, k, got, want[i])
			}
		}
	}
}

// MON-R18: each kind's cardinality limit is the largest series bound of the
// catalogue's instruments of that kind, plus the overflow series; a kind the
// catalogue does not use keeps the provider's limit.
func TestCardinalityLimits_MON_R18(t *testing.T) {
	sel := cardinalityLimits(telemetry.Instruments(), nil)
	for _, inst := range telemetry.Instruments() {
		limit, fallback := sel(sdkKind[inst.Kind])
		if fallback || limit < inst.SeriesBound()+1 {
			t.Errorf("%s: limit %d (fallback %t) below its bound %d plus overflow", inst.Name, limit, fallback, inst.SeriesBound())
		}
	}
	if _, fallback := sel(sdkmetric.InstrumentKindObservableUpDownCounter); !fallback {
		t.Error("a kind outside the catalogue must fall back to the provider's limit")
	}
	largest := 0
	for _, inst := range telemetry.Instruments() {
		if inst.Kind == telemetry.Counter && inst.SeriesBound() > largest {
			largest = inst.SeriesBound()
		}
	}
	if l, _ := sel(sdkmetric.InstrumentKindCounter); l != largest+1 {
		t.Errorf("counter limit %d, want the largest counter bound %d + 1", l, largest)
	}
	// A Snapshot is an observable gauge, limited by the largest snapshot.
	if l, _ := sel(sdkmetric.InstrumentKindObservableGauge); l != telemetry.CertificateExpiry.SeriesBound()+1 {
		t.Errorf("observable gauge limit %d, want certificate.expiry's bound %d + 1", l, telemetry.CertificateExpiry.SeriesBound())
	}
	// The SDK limits a kind, not an instrument: observable counters share a
	// limit with the catalogue's one-series pool counter, and still fit
	// process.cpu.time's two modes.
	if l, _ := sel(sdkmetric.InstrumentKindObservableCounter); l < runtimeSeriesBound+1 {
		t.Errorf("observable counter limit %d, below a runtime instrument's %d series + 1", l, runtimeSeriesBound)
	}
}

// MON-R13: the self-metric counts each export's outcome, and the check
// reads the newest one.
func TestExport_SelfMetricAndCheck_MON_R13(t *testing.T) {
	for _, protocol := range []string{ProtocolHTTP, ProtocolGRPC} {
		t.Run(protocol, func(t *testing.T) {
			col := otlptest.New()
			tel, _, _ := build(t, protocol, endpoints(t, col)[protocol], nil)
			if st := tel.ExportCheck(); st.Exported {
				t.Fatal("exported before any export")
			}
			exportOnce(tel) // ok
			col.Set(http.StatusUnauthorized, codes.Unauthenticated)
			exportOnce(tel) // failed
			if st := tel.ExportCheck(); st.OK || st.Code != CodeCredentialRejected || st.Failures != 1 {
				t.Fatalf("after a 401: %+v", st)
			}
			col.Set(0, codes.OK)
			exportOnce(tel)
			got := col.Exports()
			text := otlptest.Render(got[len(got)-1].Request, otlptest.Options{})
			exports := findMetric(t, text, "shn.gateway.telemetry.exports")
			for _, want := range []string{"{shn.outcome=ok} 1", "{shn.outcome=failed} 1"} {
				if !strings.Contains(exports, want) {
					t.Errorf("exports metric lacks %q:\n%s", want, exports)
				}
			}
			if st := tel.ExportCheck(); !st.OK || st.Failures != 0 {
				t.Fatalf("after recovery: %+v", st)
			}
		})
	}
}

// MON-R13: each failure maps to an existing check code, classified from the
// status the transport saw.
func TestExport_FailureClassification_MON_R13(t *testing.T) {
	type row struct {
		name     string
		protocol string
		http     int
		grpc     codes.Code
		rejected int64
		down     bool
		outcome  string
		code     string
		hint     string
	}
	for _, r := range []row{
		{name: "http 401", protocol: ProtocolHTTP, http: 401, outcome: "failed", code: CodeCredentialRejected, hint: "HTTP 401"},
		{name: "http 403", protocol: ProtocolHTTP, http: 403, outcome: "failed", code: CodeCredentialRejected, hint: "HTTP 403"},
		{name: "http 500", protocol: ProtocolHTTP, http: 500, outcome: "failed", code: CodeHTTPStatus, hint: "HTTP 500"},
		{name: "http 400", protocol: ProtocolHTTP, http: 400, outcome: "failed", code: CodeHTTPStatus, hint: "HTTP 400"},
		{name: "http partial success", protocol: ProtocolHTTP, rejected: 3, outcome: "dropped"},
		{name: "http collector down", protocol: ProtocolHTTP, down: true, outcome: "failed", code: CodeUnreachable},
		{name: "grpc unauthenticated", protocol: ProtocolGRPC, grpc: codes.Unauthenticated, outcome: "failed", code: CodeCredentialRejected, hint: "gRPC Unauthenticated"},
		{name: "grpc permission denied", protocol: ProtocolGRPC, grpc: codes.PermissionDenied, outcome: "failed", code: CodeCredentialRejected, hint: "gRPC PermissionDenied"},
		{name: "grpc invalid argument", protocol: ProtocolGRPC, grpc: codes.InvalidArgument, outcome: "failed", code: CodeHTTPStatus, hint: "gRPC InvalidArgument"},
		{name: "grpc partial success", protocol: ProtocolGRPC, rejected: 2, outcome: "dropped"},
		{name: "grpc collector down", protocol: ProtocolGRPC, down: true, outcome: "failed", code: CodeUnreachable, hint: "gRPC Unavailable"},
	} {
		t.Run(r.name, func(t *testing.T) {
			col := otlptest.New()
			col.Set(r.http, r.grpc)
			col.Rejected = r.rejected
			endpoint := endpoints(t, col)[r.protocol]
			if r.down {
				// A listener that is gone: nothing answers there.
				dead := httptest.NewServer(http.NotFoundHandler())
				endpoint = dead.URL + "/v1/metrics"
				if r.protocol == ProtocolGRPC {
					endpoint = dead.URL
				}
				dead.Close()
			}
			tel, _, _ := build(t, r.protocol, endpoint, map[string]string{"OTEL_METRIC_EXPORT_TIMEOUT": "700", "OTEL_EXPORTER_OTLP_TIMEOUT": "700"})
			tel.exp.reg.Store(telemetry.New(outcomeSink{t: t, want: r.outcome}))
			exportOnce(tel)
			st := tel.ExportCheck()
			if st.OK != (r.outcome != "failed") || st.Code != r.code || st.Hint != r.hint {
				t.Fatalf("state %+v, want outcome %s code %q hint %q", st, r.outcome, r.code, r.hint)
			}
		})
	}
}

// outcomeSink fails the test unless every export it sees counts want.
type outcomeSink struct {
	t    *testing.T
	want string
}

func (s outcomeSink) Record(m telemetry.Measurement) {
	if m.Instrument == telemetry.TelemetryExports && m.Attributes[0] != s.want {
		s.t.Errorf("export counted %q, want %q", m.Attributes[0], s.want)
	}
}

func TestClassify_MON_R13(t *testing.T) {
	err := errors.New("any")
	for _, c := range []struct {
		err                 error
		a                   attempt
		outcome, code, hint string
	}{
		{nil, attempt{sent: true, httpCode: 200}, "ok", "", ""},
		{err, attempt{sent: true, httpCode: 202}, "dropped", "", ""},
		{err, attempt{sent: true, transport: true}, "failed", CodeUnreachable, ""},
		{err, attempt{sent: true, httpCode: 401}, "failed", CodeCredentialRejected, "HTTP 401"},
		{err, attempt{sent: true, httpCode: 503}, "failed", CodeHTTPStatus, "HTTP 503"},
		{err, attempt{sent: true, isGRPC: true, grpcCode: codes.Unavailable}, "failed", CodeUnreachable, "gRPC Unavailable"},
		{err, attempt{sent: true, isGRPC: true, grpcCode: codes.DeadlineExceeded}, "failed", CodeUnreachable, "gRPC DeadlineExceeded"},
		{context.DeadlineExceeded, attempt{}, "failed", CodeUnreachable, ""},
		{err, attempt{}, "failed", CodeInternal, ""},
	} {
		v := classify(c.err, c.a)
		o, code, hint := v.outcome, v.code, v.hint
		if o != c.outcome || code != c.code || hint != c.hint {
			t.Errorf("classify(%v, %+v) = %s %s %q, want %s %s %q", c.err, c.a, o, code, hint, c.outcome, c.code, c.hint)
		}
	}
}

// MON-R13, MON-R10: a failing export is logged on its 1st, 2nd, 4th and 8th
// occurrence in a row with its class only, and its recovery once.
func TestExport_FailureLogging_MON_R13_R10(t *testing.T) {
	col := otlptest.New()
	col.Set(http.StatusUnauthorized, codes.OK)
	srv := col.HTTP(t)
	process := captureProcessLog(t)
	tel, _, l := build(t, ProtocolHTTP, srv.URL+secretPath, nil)
	for range 9 {
		exportOnce(tel)
	}
	col.Set(0, codes.OK)
	exportOnce(tel)
	var failed []string
	for _, line := range l.all() {
		if strings.Contains(line, "OTLP export failed") {
			failed = append(failed, line)
		}
	}
	want := []string{"1 in a row", "2 in a row", "4 in a row", "8 in a row"}
	if len(failed) != len(want) {
		t.Fatalf("failure lines %q, want 4", failed)
	}
	for i, w := range want {
		if !strings.Contains(failed[i], w) || !strings.Contains(failed[i], "credential-rejected, HTTP 401") {
			t.Errorf("line %q, want %q", failed[i], w)
		}
	}
	if last := l.all()[len(l.all())-1]; !strings.Contains(last, "recovered after 9 failed in a row") {
		t.Errorf("last line %q", last)
	}
	assertNoSecrets(t, append(l.all(), process.String()))
}

// assertNoSecrets fails when any text carries a planted secret or a
// collector's own answer.
func assertNoSecrets(t *testing.T, texts []string) {
	t.Helper()
	for _, text := range texts {
		for _, s := range []string{secretToken, secretPath, "collector-body-detail", "collector-status-detail", "collector-partial-detail", "Bearer"} {
			if strings.Contains(text, s) {
				t.Errorf("%q reached a log or report: %q", s, text)
			}
		}
	}
}

// MON-R10: no header value, endpoint path or collector answer reaches a log,
// a boot line or a check, on any path: success, each failure, a partial
// success, the SDK's own error handler and shutdown.
func TestExport_NoSecrets_MON_R10(t *testing.T) {
	process := captureProcessLog(t)
	for _, protocol := range []string{ProtocolHTTP, ProtocolGRPC} {
		col := otlptest.New()
		ep := endpoints(t, col)[protocol]
		if protocol == ProtocolHTTP {
			ep = strings.TrimSuffix(ep, "/v1/metrics") + secretPath
		}
		tel, reg, l := build(t, protocol, ep, nil)
		reg.Add(telemetry.Exchanges, 1, "inbound", kind, "answered")
		var reports []string
		for _, set := range []func(){
			func() { col.Set(0, codes.OK) },
			func() { col.Set(401, codes.Unauthenticated) },
			func() { col.Set(500, codes.Internal) },
			func() { col.Set(0, codes.OK); col.Rejected = 5 },
		} {
			set()
			exportOnce(tel)
			st := tel.ExportCheck()
			reports = append(reports, fmt.Sprintf("%+v", st))
		}
		reports = append(reports, tel.Lines()...)
		reports = append(reports, strings.Join(tel.ConfigCheck().Settings, ","))
		tel.Shutdown(context.Background())
		assertNoSecrets(t, append(append(reports, l.all()...), process.String()))
	}
	// The SDK's own error handler never logs the error it is handed.
	installGlobalHandlers()
	sdkErrors.Store(0) // the count is the process's: start it where the first line is logged
	otel.Handle(errors.New("export to https://collector" + secretPath + " failed: " + secretToken))
	assertNoSecrets(t, []string{process.String()})
	if !strings.Contains(process.String(), "the OpenTelemetry SDK reported an internal error") {
		t.Fatalf("the SDK error was not named: %q", process.String())
	}
}

// MON-R12: recording never waits on an export, however slow the collector.
func TestRecording_NeverBlocksOnExport_MON_R12(t *testing.T) {
	col := otlptest.New()
	hold := make(chan struct{})
	col.SetHold(hold)
	tel, reg, _ := build(t, ProtocolHTTP, col.HTTP(t).URL+"/v1/metrics", nil)
	done := make(chan struct{})
	go func() { exportOnce(tel); close(done) }()
	select {
	case <-col.Arrived():
	case <-time.After(10 * time.Second):
		t.Fatal("the export never reached the collector")
	}
	start := time.Now()
	for i := range 10000 {
		reg.Add(telemetry.Exchanges, 1, "inbound", kind, "answered")
		reg.RecordDuration(telemetry.ExchangeDuration, time.Duration(i)*time.Millisecond, "inbound", kind)
	}
	// The pinned bound: 20,000 measurements while an export is held take
	// well under a second (they take milliseconds); a recording path that
	// waited on the export would take until the hold's release.
	if took := time.Since(start); took > time.Second {
		t.Fatalf("recording took %s while an export was held", took)
	}
	close(hold)
	<-done
}

// MON-R14: the final export runs at Shutdown, carries what was recorded up
// to then, and is bounded even when the collector never answers.
func TestShutdown_FinalExportBounded_MON_R14(t *testing.T) {
	prev := shutdownBudget
	shutdownBudget = 300 * time.Millisecond
	t.Cleanup(func() { shutdownBudget = prev })

	col := otlptest.New()
	tel, reg, _ := build(t, ProtocolHTTP, col.HTTP(t).URL+"/v1/metrics", map[string]string{"OTEL_METRIC_EXPORT_INTERVAL": "3600000"})
	reg.Add(telemetry.AuditDrops, 3, "stopped")
	tel.Shutdown(context.Background())
	got := col.Exports()
	if len(got) != 1 || !strings.Contains(otlptest.Render(got[0].Request, otlptest.Options{}), "{shn.reason=stopped} 3") {
		t.Fatalf("final export missing or without the count recorded before it: %d exports", len(got))
	}
	tel.Shutdown(context.Background()) // idempotent
	if len(col.Exports()) != 1 {
		t.Fatal("a second Shutdown exported again")
	}

	held := otlptest.New()
	held.SetHold(make(chan struct{}))
	tel, _, _ = build(t, ProtocolHTTP, held.HTTP(t).URL+"/v1/metrics", nil)
	start := time.Now()
	tel.Shutdown(context.Background())
	if took := time.Since(start); took > shutdownBudget+2*time.Second {
		t.Fatalf("Shutdown took %s against a collector that never answers, budget %s", took, shutdownBudget)
	}
}

// Run exports every interval and returns when its context ends, without a
// final export of its own.
func TestRun_ExportsEachInterval(t *testing.T) {
	col := otlptest.New()
	tel, _, _ := build(t, ProtocolHTTP, col.HTTP(t).URL+"/v1/metrics", map[string]string{"OTEL_METRIC_EXPORT_INTERVAL": "20"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { tel.Run(ctx); close(done) }()
	for range 2 {
		select {
		case <-col.Arrived():
		case <-time.After(10 * time.Second):
			t.Fatal("no export each interval")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return when its context ended")
	}
	var nilTel *Telemetry
	nilTel.Run(ctx)
	nilTel.Shutdown(ctx)
}

// An exporter the SDK refuses, from settings that passed validation, turns
// OTLP export off with a line that names no setting's value, and fails the
// telemetry-config check; the gateway still boots (MON-R44).
func TestBuild_ExporterTheSDKRefuses_MON_R44(t *testing.T) {
	exporterRefusal = errors.New("refused for https://collector" + secretPath + " with " + secretToken)
	t.Cleanup(func() { exporterRefusal = nil })
	c := Load(env(map[string]string{"OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_HEADERS": "authorization=" + secretToken}), nil)
	tel := Build(c, testIdentity, Options{Logf: func(string, ...any) {}})
	if tel.Sink() != nil || tel.ExportCheck().Present {
		t.Fatal("a refused exporter still exports")
	}
	if st := tel.ConfigCheck(); !st.Present || st.OK || !st.Off || !containsString(st.Settings, "OTEL_METRICS_EXPORTER") {
		t.Fatalf("telemetry-config %+v", st)
	}
	assertNoSecrets(t, tel.Lines())
	if !strings.Contains(strings.Join(tel.Lines(), "\n"), "could not be built") {
		t.Fatalf("lines %q", tel.Lines())
	}
	tel.Shutdown(context.Background())
	tel.Discard()
}

// A gateway that does not start discards its export without sending it.
func TestDiscard_SendsNothing(t *testing.T) {
	col := otlptest.New()
	tel, reg, _ := build(t, ProtocolHTTP, col.HTTP(t).URL+"/v1/metrics", nil)
	reg.Add(telemetry.Legs, 1, "failed")
	tel.Discard()
	tel.Shutdown(context.Background())
	if n := len(col.Exports()); n != 0 {
		t.Fatalf("%d exports from a discarded export", n)
	}
}

// A periodic export the stop cuts short is neither counted nor logged.
func TestExport_CutShortByTheStop(t *testing.T) {
	col := otlptest.New()
	hold := make(chan struct{})
	col.SetHold(hold)
	defer close(hold)
	tel, _, l := build(t, ProtocolHTTP, col.HTTP(t).URL+"/v1/metrics", nil)
	counted := &recorderSink{}
	tel.Bind(telemetry.New(counted))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { tel.export(ctx, time.Minute); close(done) }()
	<-col.Arrived()
	cancel()
	<-done
	if st := tel.ExportCheck(); st.Exported || len(counted.got) != 0 || len(l.all()) != 0 {
		t.Fatalf("a cut-short export was reported: state %+v, counted %d, lines %q", st, len(counted.got), l.all())
	}
}

// recorderSink keeps the export counts it is handed.
type recorderSink struct {
	mu  sync.Mutex
	got []telemetry.Measurement
}

func (r *recorderSink) Record(m telemetry.Measurement) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, m)
}
