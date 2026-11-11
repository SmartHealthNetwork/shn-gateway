//go:build !shn_coreonly

package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/checks"
	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
	"github.com/SmartHealthNetwork/shn-gateway/internal/otlptest"
	"github.com/SmartHealthNetwork/shn-gateway/telemetry"
	"github.com/SmartHealthNetwork/shn-gateway/telemetry/otelsetup"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// otlpGoldenPath is the decoded OTLP of the golden fixture. Set
// SHN_UPDATE_OTLP_GOLDEN=1 to rewrite it; a change to it is a change to every
// exported name, unit, attribute or value shape.
const otlpGoldenPath = "testdata/otlp_golden.txt"

// MON-R40: the fixture's measurements, exported over HTTP and gRPC, decode to
// the golden: every name, unit, kind, temporality, attribute and value. An
// upgrade of a dependency that renames anything fails here. No identifier
// the fixture's records carry reaches the export (MON-R19).
func TestOTLPGolden_MON_R40(t *testing.T) {
	clearOTELEnv(t)
	col := otlptest.New()
	endpoints := map[string]string{otelsetup.ProtocolHTTP: col.HTTP(t).URL + "/v1/metrics", otelsetup.ProtocolGRPC: col.GRPC(t)}
	rendered := map[string]string{}
	for _, protocol := range []string{otelsetup.ProtocolHTTP, otelsetup.ProtocolGRPC} {
		before := len(col.Exports())
		cfg := otelsetup.Load(func(k string) string {
			return map[string]string{
				"OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_PROTOCOL": protocol,
				"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": endpoints[protocol], "OTEL_SERVICE_NAME": "golden-gw",
			}[k]
		}, nil)
		tel := otelsetup.Build(cfg, otelsetup.Identity{Version: "v1.0.0-golden", HolderID: "h-golden-own", Role: "payer", MetricsEnv: "golden",
			Hostname: func() (string, error) { return "golden-host", nil }}, otelsetup.Options{Logf: t.Logf})
		reg := telemetry.New(tel.Sink())
		tel.Bind(reg)
		driveTelemetryFixture(reg)
		tel.Shutdown(context.Background())
		got := col.Exports()
		if len(got) != before+1 {
			t.Fatalf("%s: %d exports, want the one final export", protocol, len(got)-before)
		}
		rendered[protocol] = otlptest.Render(got[len(got)-1].Request, otlptest.Options{NamesOnly: otelsetup.RuntimeScopes, Omit: otelsetup.ConditionalRuntimeMetrics})
	}
	if rendered[otelsetup.ProtocolHTTP] != rendered[otelsetup.ProtocolGRPC] {
		t.Fatalf("HTTP and gRPC exports differ:\n--- http\n%s\n--- grpc\n%s", rendered[otelsetup.ProtocolHTTP], rendered[otelsetup.ProtocolGRPC])
	}
	text := rendered[otelsetup.ProtocolHTTP]
	for _, leak := range []string{"order-sign", "holder-a", "holder-b", "leg-1", "trace-1", "caller-chosen", "made-up", "novel", "sideways", "Patient/123"} {
		if strings.Contains(text, leak) {
			t.Errorf("%q reached the export", leak)
		}
	}
	if os.Getenv("SHN_UPDATE_OTLP_GOLDEN") == "1" {
		if err := os.WriteFile(otlpGoldenPath, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(otlpGoldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if text != string(want) {
		t.Fatalf("the decoded export differs from %s (SHN_UPDATE_OTLP_GOLDEN=1 rewrites it):\n%s", otlpGoldenPath, text)
	}
}

// prometheusGoldenPath is the golden fixture's /metrics scrape. Set
// SHN_UPDATE_PROMETHEUS_GOLDEN=1 to rewrite it; a change to it is a change to
// every scraped name, label, type or value shape.
const prometheusGoldenPath = "testdata/prometheus_golden.txt"

// MON-R40, MON-R6: the fixture's measurements, scraped from the pull
// listener's GET /metrics, are the golden byte for byte: every name, suffix,
// label, type, bucket and value, and target_info's resource. An exporter
// upgrade that renames anything fails here. No identifier the fixture's
// records carry reaches the scrape (MON-R19).
func TestPrometheusGolden_MON_R40(t *testing.T) {
	clearOTELEnv(t)
	cfg := otelsetup.Load(func(k string) string {
		return map[string]string{"OTEL_METRICS_EXPORTER": "prometheus", "OTEL_SERVICE_NAME": "golden-gw"}[k]
	}, nil)
	tel := otelsetup.Build(cfg, otelsetup.Identity{Version: "v1.0.0-golden", HolderID: "h-golden-own", Role: "payer", MetricsEnv: "golden",
		Hostname: func() (string, error) { return "golden-host", nil }}, otelsetup.Options{Logf: t.Logf})
	defer tel.Shutdown(context.Background())
	if tel.PullHandler() == nil {
		t.Fatalf("pull is off: %q", tel.Lines())
	}
	reg := telemetry.New(tel.Sink())
	tel.Bind(reg)
	driveTelemetryFixture(reg)
	w := httptest.NewRecorder()
	tel.PullHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("scrape %d", w.Code)
	}
	text := w.Body.String()
	for _, leak := range []string{"order-sign", "holder-a", "holder-b", "leg-1", "trace-1", "caller-chosen", "made-up", "novel", "sideways"} {
		if strings.Contains(text, leak) {
			t.Errorf("%q reached the scrape", leak)
		}
	}
	text = runtimeNamesOnly(text)
	if os.Getenv("SHN_UPDATE_PROMETHEUS_GOLDEN") == "1" {
		if err := os.WriteFile(prometheusGoldenPath, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(prometheusGoldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if text != string(want) {
		t.Fatalf("the scrape differs from %s (SHN_UPDATE_PROMETHEUS_GOLDEN=1 rewrites it):\n%s", prometheusGoldenPath, text)
	}
}

// runtimeNamesOnly keeps a scrape's Go runtime and process families as
// their HELP and TYPE lines only: their values change from run to run, so the
// golden pins their names, units and types (MON-R28, MON-R40). The families
// present only on some platforms or settings
// (otelsetup.ConditionalRuntimeMetrics) are left out altogether; their own
// test pins them.
func runtimeNamesOnly(text string) string {
	conditional := []string{"process_memory_usage_bytes", "go_memory_limit_bytes"}
	var out []string
	for _, l := range strings.SplitAfter(text, "\n") {
		if strings.HasPrefix(l, "go_") || strings.HasPrefix(l, "process_") {
			continue
		}
		if f := strings.Fields(l); len(f) > 2 && f[0] == "#" && slices.Contains(conditional, f[2]) {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "")
}

// MON-R18: each instrument's series bound, pinned for a release build. A
// change is a change to what every backend stores per gateway: update the
// bound documented for the instrument with it.
func TestSeriesBounds_MON_R18(t *testing.T) {
	want := map[string]int{
		"shn.gateway.exchanges": 0, "shn.gateway.exchange.duration": 0, "shn.gateway.backend.calls": 0, "shn.gateway.backend.duration": 0,
		"shn.gateway.legs": 6, "shn.gateway.store.errors": 5, "shn.gateway.involved.omitted": 8,
		"shn.gateway.store.pool.connections": 4, "shn.gateway.store.pool.empty_acquires": 1, "shn.gateway.audit.appends": 1,
		"shn.gateway.audit.retries": 6, "shn.gateway.audit.drops": 7, "shn.gateway.audit.queue.depth": 1, "shn.gateway.audit.endpoint.unknown": 1,
		"shn.gateway.validator.lane.qualified": 12, "shn.gateway.validator.lane.attempts": 36, "shn.gateway.telemetry.exports": 4,
		"shn.gateway.info": 60, "shn.gateway.certificate.expiry": 600, "shn.gateway.ingress_key.remaining": 1, "shn.gateway.ingress_key.age": 1,
		"shn.gateway.dependency.age": 2, "shn.gateway.peers.known": 1, "shn.gateway.authorization.token.duration": 1,
		"shn.gateway.authorization.tokens": 5,
	}
	kinds := len(engine.ExchangeKinds)
	dirs, outcomes := len(engine.ExchangeDirections), len(engine.ExchangeOutcomes)
	want["shn.gateway.exchanges"] = dirs * kinds * outcomes
	want["shn.gateway.exchange.duration"] = dirs * kinds
	want["shn.gateway.backend.calls"] = kinds * len(telemetry.BackendClasses)
	want["shn.gateway.backend.duration"] = kinds
	// The exchange additions: each list's own values, and other where a list
	// does not name it.
	plusOther := func(list []string) int { return len(list) + 1 }
	want["shn.gateway.refusals"] = dirs * kinds * len(engine.RefusalRules) * len(engine.RefusalParties) * len(telemetry.RefusalStages)
	want["shn.gateway.exchange.stage.duration"] = dirs * kinds * plusOther(telemetry.Stages)
	want["shn.gateway.active_requests"] = dirs
	want["shn.gateway.exchange.request.size"] = dirs * kinds
	want["shn.gateway.exchange.response.size"] = dirs * kinds
	want["shn.gateway.conformance.findings"] = kinds * plusOther(engine.ConformanceLevels) * plusOther(engine.CheckKinds) *
		plusOther(engine.FindingActions) * plusOther(telemetry.Booleans)
	want["shn.gateway.validator.duration"] = plusOther(telemetry.IGLines) * plusOther(telemetry.LaneKinds)
	want["shn.gateway.conformance.checks.dropped"] = 1
	// Each bound and listener, and other for each.
	want["shn.gateway.listener.cuts"] = plusOther(telemetry.ListenerBounds) * plusOther(telemetry.Listeners)
	want["http.server.request.duration"] = telemetry.HTTPServerRequestDuration.Bound // pinned by TestHTTPServerBound_MON_R18
	for _, inst := range telemetry.Instruments() {
		if got := inst.SeriesBound(); got != want[inst.Name] {
			t.Errorf("%s: bound %d, pinned %d", inst.Name, got, want[inst.Name])
		}
	}
	if !toyProfileBuild && kinds != 12 {
		t.Errorf("%d exchange kinds in a release build; the exchange instruments' bounds follow it", kinds)
	}
}

// clearOTELEnv empties every OTEL_* setting of the process environment for
// the test: the gateway names those it does not read, and a monitoring
// setting there would reach a test that sets none.
func clearOTELEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "OTEL_") {
			t.Setenv(name, "")
		}
	}
}

// monitoringGateway is a payer gateway's surroundings for build: a bundle,
// discovery, keys and its own system, never called.
func monitoringGateway(t *testing.T, extra map[string]string) map[string]string {
	t.Helper()
	clearOTELEnv(t)
	dir := t.TempDir()
	id, err := shnsdk.GenerateIdentity("h-payer-mon")
	if err != nil {
		t.Fatal(err)
	}
	if err := shnsdk.WriteBundle(dir, id, "payer", "https://holder.example"); err != nil {
		t.Fatal(err)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyBody := fmt.Sprintf(`{"pubkey":%q}`, base64.StdEncoding.EncodeToString(pub))
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(keyBody)) }))
	t.Cleanup(keys.Close)
	disc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{`+acceptingWire+`"endpoints":{},"authzPublicKeyURL":%q,"hubTransportKeyURL":%q}`, keys.URL, keys.URL)
	}))
	t.Cleanup(disc.Close)
	system := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(system.Close)
	// The system of record, local: the checks this test runs probe it.
	sor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/fhir+json")
		_, _ = w.Write([]byte(`{"resourceType":"CapabilityStatement","fhirVersion":"4.0.1"}`))
	}))
	t.Cleanup(sor.Close)
	env := map[string]string{
		"ROLE": "payer", "SHN_SECRETS": dir, "SHN_DISCOVERY_URL": disc.URL, "SHN_FAKE_VALIDATOR": "1",
		"FHIR_DATA_URL": sor.URL, "PAYER_DAVINCI_BASE_URL": system.URL,
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

// buildMonitored builds the gateway and returns it with its checks' results.
func buildMonitored(t *testing.T, env map[string]string) (built, map[string]checks.Result) {
	t.Helper()
	b, err := build(context.Background(), func(k string) string { return env[k] }, io.Discard, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	t.Cleanup(func() {
		b.telemetry.Shutdown(context.Background())
		_ = b.gateway.Close()
	})
	results, err := b.checksRunner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]checks.Result{}
	for _, r := range results {
		byID[r.ID] = r
	}
	return b, byID
}

// MON-R1: with no monitoring setting a gateway builds no meter provider and
// reports nothing new in /internal/checks.
func TestBuild_MonitoringOffByDefault_MON_R1(t *testing.T) {
	b, results := buildMonitored(t, monitoringGateway(t, nil))
	if b.telemetry.Sink() != nil {
		t.Fatal("a meter provider was built with no monitoring setting")
	}
	for id := range results {
		if strings.HasPrefix(id, "telemetry-") {
			t.Fatalf("check %s reported with no monitoring setting", id)
		}
	}
}

// MON-R44: with each invalid monitoring value in turn the gateway boots,
// still answers, and telemetry-config names the setting.
func TestBuild_InvalidMonitoringNeverStopsBoot_MON_R44(t *testing.T) {
	for setting, value := range map[string]string{
		"OTEL_EXPORTER_OTLP_PROTOCOL":         "http/json",
		"OTEL_METRICS_EXPORTER":               "console",
		"OTEL_CONFIG_FILE":                    "/etc/otel.yaml",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "collector-without-scheme",
		"OTEL_METRIC_EXPORT_INTERVAL":         "often",
		"OTEL_SDK_DISABLED":                   "perhaps",
	} {
		t.Run(setting, func(t *testing.T) {
			extra := map[string]string{"OTEL_METRICS_EXPORTER": "otlp", setting: value}
			b, results := buildMonitored(t, monitoringGateway(t, extra))
			w := httptest.NewRecorder()
			b.handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/substrate/inbound", strings.NewReader("{}")))
			if w.Code != http.StatusForbidden {
				t.Fatalf("the gateway does not answer as before: %d %s", w.Code, w.Body.String())
			}
			r, ok := results["telemetry-config"]
			if !ok || r.OK || r.Failure == nil || r.Failure.Code != checks.FailInvalidTelemetryConfiguration || !strings.Contains(r.Failure.Hint, setting) {
				t.Fatalf("telemetry-config = %+v (failure %+v), want failing on %s", r, r.Failure, setting)
			}
			if strings.Contains(r.Detail+r.Failure.Hint, value) {
				t.Fatalf("the check carries the value: %+v", r)
			}
		})
	}
}

// MON-R7, MON-R44, MON-R42: a pull bind off this machine without TLS and a
// token or client CA serves nothing, and the gateway boots, answers as before
// and names the setting in telemetry-config.
func TestBuild_UnprotectedPullBindServesNothing_MON_R7_R44(t *testing.T) {
	for name, extra := range map[string]map[string]string{
		"all interfaces":                {"OTEL_EXPORTER_PROMETHEUS_HOST": "0.0.0.0"},
		"all interfaces, token only":    {"OTEL_EXPORTER_PROMETHEUS_HOST": "0.0.0.0", "METRICS_PULL_TOKEN": "pull-token-not-enough"},
		"a host name, nothing set":      {"OTEL_EXPORTER_PROMETHEUS_HOST": "metrics.internal"},
		"IPv6 all interfaces, port set": {"OTEL_EXPORTER_PROMETHEUS_HOST": "::", "OTEL_EXPORTER_PROMETHEUS_PORT": "19464"},
	} {
		t.Run(name, func(t *testing.T) {
			extra["OTEL_METRICS_EXPORTER"] = "prometheus"
			b, results := buildMonitored(t, monitoringGateway(t, extra))
			if b.telemetry.PullAddr() != "" || b.telemetry.PullHandler() != nil || b.telemetry.Sink() != nil {
				t.Fatalf("an unprotected bind is served at %q", b.telemetry.PullAddr())
			}
			w := httptest.NewRecorder()
			b.handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/substrate/inbound", strings.NewReader("{}")))
			if w.Code != http.StatusForbidden {
				t.Fatalf("the gateway does not answer as before: %d %s", w.Code, w.Body.String())
			}
			r, ok := results["telemetry-config"]
			if !ok || r.OK || !r.Advisory || r.Failure == nil || r.Failure.Code != checks.FailInvalidTelemetryConfiguration ||
				!strings.Contains(r.Failure.Hint, "OTEL_EXPORTER_PROMETHEUS_HOST") || !strings.Contains(r.Detail, "Prometheus pull is off") {
				t.Fatalf("telemetry-config = %+v (failure %+v)", r, r.Failure)
			}
			if strings.Contains(r.Detail+r.Failure.Hint, "pull-token-not-enough") || strings.Contains(r.Detail+r.Failure.Hint, "metrics.internal") {
				t.Fatalf("the check carries a value: %+v", r)
			}
		})
	}
}

// MON-R6: GET /metrics is the pull listener's alone, never the ingress
// listener's; the pull listener scrapes what the gateway records, and Run's
// workers serve it on its own bind.
func TestPull_OwnListenerNeverIngress_MON_R6(t *testing.T) {
	// The test binds the pull port itself and hands the listener to
	// ServePull, so the port is this test's from the bind to the close: no
	// other test can take it between a probe and the gateway's bind, and a
	// scrape of it can only reach this gateway.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	otelsetup.HandListenerForTest(t, ln)
	port := fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
	b, _ := buildMonitored(t, monitoringGateway(t, map[string]string{"OTEL_METRICS_EXPORTER": "prometheus",
		"OTEL_EXPORTER_PROMETHEUS_HOST": "127.0.0.1", "OTEL_EXPORTER_PROMETHEUS_PORT": port}))
	if !b.httpServerMetered {
		t.Fatal("a gateway that exports left the main listener without the HTTP server metric")
	}
	if b.telemetry.PullAddr() != "127.0.0.1:"+port {
		t.Fatalf("pull addr %q", b.telemetry.PullAddr())
	}
	w := httptest.NewRecorder()
	b.handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if strings.Contains(w.Body.String(), "shn_gateway_") || strings.Contains(w.Body.String(), "target_info") {
		t.Fatalf("the ingress listener serves metrics: %d", w.Code)
	}
	stop := b.startWorkers(context.Background())
	// The port is bound already, so the connection queues until Run's
	// listener serves it: one scrape, no retry.
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get("http://127.0.0.1:" + port + "/metrics")
	if err != nil {
		stop()
		t.Fatalf("scrape of Run's pull listener: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	stop()
	body := string(raw)
	if !b.telemetry.ConfigCheck().OK {
		t.Fatalf("telemetry-config is not OK on a pull listener served from a listener the test bound: %+v", b.telemetry.ConfigCheck())
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "target_info") || !strings.Contains(body, `shn_holder_id="h-payer-mon"`) {
		t.Fatalf("scrape of Run's pull listener: %d\n%s", resp.StatusCode, body)
	}
	// The main listener's own request, answered 404 with no route, is
	// recorded by the HTTP server metric of a gateway that exports (MON-R29).
	if !strings.Contains(body, `http_server_request_duration_seconds_count{http_request_method="GET",http_response_status_code="404",`) {
		t.Fatalf("the main listener's request is not in the scrape:\n%s", body)
	}
	// The pull listener does not outlive its workers: stopping them closed
	// the listener itself, so nothing can be accepted on it.
	err = ln.(*net.TCPListener).SetDeadline(time.Now().Add(time.Millisecond))
	if err == nil {
		_, err = ln.Accept()
	}
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("the pull listener outlives its workers: %v", err)
	}
}

// MON-R1: a gateway that exports nothing (no METRICS_SERVICE, no
// OpenTelemetry exporter) leaves the main listener unwrapped, so it records
// no http.server.request.duration, and answers as one that exports does.
func TestBuild_NothingExportedLeavesTheListener_MON_R1(t *testing.T) {
	answer := func(b built) (int, string) {
		w := httptest.NewRecorder()
		b.handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
		return w.Code, w.Header().Get("Content-Type")
	}
	off, _ := buildMonitored(t, monitoringGateway(t, nil))
	if off.httpServerMetered {
		t.Fatal("a gateway that exports nothing wrapped the main listener in the HTTP server metric")
	}
	srv := otlptest.New().HTTP(t)
	on, _ := buildMonitored(t, monitoringGateway(t, map[string]string{"OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_ENDPOINT": srv.URL}))
	if !on.httpServerMetered {
		t.Fatal("a gateway that exports left the main listener without the HTTP server metric")
	}
	offCode, offType := answer(off)
	onCode, onType := answer(on)
	if offCode != onCode || offType != onType {
		t.Fatalf("the metric changed the answer: %d %q without it, %d %q with it", offCode, offType, onCode, onType)
	}
}

// MON-R13: with OTLP on, telemetry-export reads the newest export's cached
// state and telemetry-config passes; neither contacts the collector.
func TestBuild_TelemetryChecks_MON_R13(t *testing.T) {
	col := otlptest.New()
	col.Set(http.StatusUnauthorized, 0)
	srv := col.HTTP(t)
	b, results := buildMonitored(t, monitoringGateway(t, map[string]string{"OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_ENDPOINT": srv.URL,
		"OTEL_METRIC_EXPORT_TIMEOUT": "500"}))
	if r := results["telemetry-config"]; !r.OK {
		t.Fatalf("telemetry-config = %+v", r)
	}
	// The monitoring checks, and no other, are advisory: decided by the
	// result's field, never by its name.
	for id, r := range results {
		if want := id == "telemetry-config" || id == "telemetry-export"; r.Advisory != want {
			t.Errorf("check %s advisory=%t, want %t", id, r.Advisory, want)
		}
	}
	if len(results) < 4 {
		t.Fatalf("only %d results; the gateway's other checks must be there to hold advisory to the two", len(results))
	}
	r := results["telemetry-export"]
	if r.OK || r.Failure == nil || r.Failure.Code != checks.FailNotChecked || r.Target != srv.URL {
		t.Fatalf("telemetry-export before any export = %+v", r)
	}
	if len(col.Exports()) != 0 {
		t.Fatal("a check contacted the collector")
	}
	// /health never reflects export: a failing export leaves it as it is.
	health := func() (int, string) {
		w := httptest.NewRecorder()
		b.handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
		return w.Code, w.Body.String()
	}
	before, beforeBody := health()
	b.telemetry.Shutdown(context.Background()) // an export the refusing collector fails
	if st := b.telemetry.ExportCheck(); !st.Exported || st.OK {
		t.Fatalf("the export did not fail: %+v", st)
	}
	after, afterBody := health()
	if before != after || strings.Contains(afterBody, "telemetry") || strings.Contains(beforeBody, "telemetry") {
		t.Fatalf("/health %d %s, then %d %s: it must not reflect export", before, beforeBody, after, afterBody)
	}
	// The configuration check says whether monitoring is off or only fell
	// back, never claiming off for a setting that kept a default.
	for env, want := range map[string]string{
		"OTEL_METRIC_EXPORT_INTERVAL":         "OTEL_METRIC_EXPORT_INTERVAL is not valid, so the default 60000 is used; export continues",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT is not valid, so OTLP export is off",
	} {
		cfg := otelsetup.Load(func(k string) string {
			return map[string]string{"OTEL_METRICS_EXPORTER": "otlp", env: "not-valid-value"}[k]
		}, nil)
		ok, detail, f := configCheck(otelsetup.Build(cfg, otelsetup.Identity{}, otelsetup.Options{Logf: t.Logf}).ConfigCheck())
		if ok || detail != want || f.Hint != env || strings.Contains(detail, "not-valid-value") {
			t.Errorf("%s: detail %q hint %q, want %q", env, detail, f.Hint, want)
		}
	}
	for st, want := range map[otelsetup.ExportState]string{
		{Exported: true, OK: true}: "",
		{Exported: true, Code: "credential-rejected", Hint: "HTTP 401", Failures: 2}: "credential-rejected",
		{Exported: true, Code: "unreachable", Failures: 1}:                           "unreachable",
	} {
		ok, detail, f := exportCheck(st)
		if ok != (want == "") || (f != nil) == (want == "") || (f != nil && (f.Code != want || f.Hint != st.Hint)) || detail == "" {
			t.Errorf("exportCheck(%+v) = %t %q %+v", st, ok, detail, f)
		}
	}
}

// MON-R14: at stop, the final export runs after the listener's in-flight
// exchange returns and after the observe flush, and before the diagnostic
// publisher's drain, which stays last. An exchange the payer's system holds
// until the stop has begun is answered during the listener's grace period,
// and the observe check of the same leg is held at the validator through the
// gateway's close: the export waits for both, carries the exchange, and
// arrives before the publisher's last heartbeat.
func TestRun_FinalExportOrder_MON_R14(t *testing.T) {
	clearOTELEnv(t)
	col := otlptest.New()
	srv := col.HTTP(t)
	f := newShutdownFlush(t)
	// The payer's own CDS service holds the forwarded leg until released.
	arrived, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	system := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"services":[{"id":"svc","hook":"order-select"}]}`))
			return
		}
		once.Do(func() { close(arrived) })
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte(`{"cards":[]}`))
	}))
	t.Cleanup(system.Close)
	var releaseOnce sync.Once
	releaseSystem := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseSystem)
	// The diagnostic ingest records when each heartbeat arrives.
	var mu sync.Mutex
	var heartbeats []time.Time
	ingest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/health"):
			mu.Lock()
			heartbeats = append(heartbeats, time.Now())
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/batch"):
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(ingest.Close)
	f.env["DIAGNOSTIC_SINK_URL"] = ingest.URL + "/internal/pa-test/observations"
	f.env["PAYER_DAVINCI_BASE_URL"] = system.URL
	f.env["HOST"] = "127.0.0.1"
	f.env["PORT"] = freePort(t)
	f.env["OTEL_METRICS_EXPORTER"] = "otlp"
	f.env["OTEL_EXPORTER_OTLP_ENDPOINT"] = srv.URL
	f.env["OTEL_METRIC_EXPORT_INTERVAL"] = "3600000" // only the final export
	addr := net.JoinHostPort(f.env["HOST"], f.env["PORT"])
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var runErr error
	go func() { runErr = Run(ctx, f.getenv, io.Discard); close(done) }()
	t.Cleanup(func() { cancel(); releaseSystem(); f.release(); <-done })
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			c.Close()
			break
		}
		select {
		case <-done:
			t.Fatalf("Run returned before serving: %v", runErr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("Run's listener at %s did not accept: %v", addr, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	delivered := make(chan struct{})
	go func() { defer close(delivered); f.deliver(t, "http://"+addr) }()
	awaitFlush(t, arrived, 10*time.Second, "the leg at the payer's system")
	awaitFlush(t, f.held, 10*time.Second, "the leg's observe check at the validator")
	cancel()
	// The stop has begun; an export run when it began has had time to run.
	time.Sleep(300 * time.Millisecond)
	if n := len(col.Exports()); n != 0 {
		t.Fatalf("%d exports before the stop's in-flight exchange returned", n)
	}
	releaseSystem()
	awaitFlush(t, delivered, 10*time.Second, "the held leg's answer")
	// The gateway's close waits for the held observe check; the export
	// waits for the close.
	time.Sleep(300 * time.Millisecond)
	if n := len(col.Exports()); n != 0 {
		t.Fatalf("%d exports before the observe flush finished", n)
	}
	f.release()
	awaitFlush(t, done, 40*time.Second, "the shutdown")
	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}
	got := col.Exports()
	if len(got) != 1 {
		t.Fatalf("%d exports, want the one final export", len(got))
	}
	exportAt := got[0].At
	text := otlptest.Render(got[0].Request, otlptest.Options{})
	if !strings.Contains(text, "{shn.direction=inbound,shn.exchange=crd-order-select,shn.outcome=answered} 1") {
		t.Fatalf("the final export lacks the exchange answered during the stop:\n%s", text)
	}
	// The leg's observe check, held at the validator through the gateway's
	// close, is counted by the flush that finished it: its finding is in the
	// same final export.
	if !strings.Contains(text, "{shn.conformance.level=observe,shn.exchange=crd-order-select,shn.finding.action=relayed,shn.finding.kind=fhir-ingress,shn.finding.tolerated=false} 1") {
		t.Fatalf("the final export lacks the finding of the observe check the stop flushed:\n%s", text)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(heartbeats) == 0 || !heartbeats[len(heartbeats)-1].After(exportAt) {
		t.Fatalf("the diagnostic publisher's last heartbeat (%v) is not after the final export (%v): its drain must stay last", heartbeats, exportAt)
	}
}

// MON-R27: shn.gateway.info names the wire protocol this gateway sends, not
// the linked shn-sdk's WireProtocolVersion. The two come apart in a build
// pinned to an SDK that states another version; the row sets them apart in
// whichever SDK the build links, and reads the OTLP label and the EMF
// dimension the build path writes.
func TestBuild_GatewayInfoNamesTheWireSent_MON_R27(t *testing.T) {
	if !slices.Contains(telemetry.WireProtocols, wireVersionSent) {
		t.Fatalf("the version sent, %s, is not in the closed list %v: its label would read as other", wireVersionSent, telemetry.WireProtocols)
	}
	// The version the row sends: one the closed list keeps and the linked SDK
	// does not state, the version sent itself where the two already differ.
	sent := ""
	for _, v := range append([]string{wireVersionSent}, telemetry.WireProtocols...) {
		if v != shnsdk.WireProtocolVersion && slices.Contains(telemetry.WireProtocols, v) {
			sent = v
			break
		}
	}
	if sent == "" {
		t.Fatalf("fixture: every version in %v is the linked SDK's %s, so the row cannot set the two apart", telemetry.WireProtocols, shnsdk.WireProtocolVersion)
	}
	// Where the linked SDK states the version sent, the two values agree, so
	// the default is checked by what it names, not by its value.
	f, err := parser.ParseFile(token.NewFileSet(), "opsmetrics.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var def ast.Expr
	ast.Inspect(f, func(n ast.Node) bool {
		if vs, ok := n.(*ast.ValueSpec); ok && len(vs.Names) == 1 && vs.Names[0].Name == "gatewayInfoWire" && len(vs.Values) == 1 {
			def = vs.Values[0]
		}
		return def == nil
	})
	if id, ok := def.(*ast.Ident); !ok || id.Name != "wireVersionSent" {
		named := "none"
		if def != nil {
			named = types.ExprString(def)
		}
		t.Fatalf("gatewayInfoWire's default is %s, want wireVersionSent, the version the gateway sends", named)
	}
	defer func(w string) { gatewayInfoWire = w }(gatewayInfoWire)
	gatewayInfoWire = sent

	col := otlptest.New()
	env := monitoringGateway(t, map[string]string{
		"OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_ENDPOINT": col.HTTP(t).URL,
		"OTEL_METRIC_EXPORT_INTERVAL": "3600000", "METRICS_SERVICE": "info-gw",
	})
	var stdout lockedBuffer
	b, err := build(context.Background(), func(k string) string { return env[k] }, &stdout, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	t.Cleanup(func() { _ = b.gateway.Close() })
	if b.opsStats == nil {
		t.Fatal("no operational gauges with METRICS_SERVICE and an exporter on")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); b.opsStats(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(stdout.String(), `"GatewayInfo"`) {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("no GatewayInfo line on stdout")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	b.telemetry.Shutdown(context.Background())

	var emf int
	for _, line := range strings.Split(stdout.String(), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil || m["GatewayInfo"] == nil {
			continue
		}
		emf++
		if m["wireProtocol"] != sent {
			t.Errorf("EMF GatewayInfo wireProtocol = %v, want the version sent, %s (the SDK states %s)", m["wireProtocol"], sent, shnsdk.WireProtocolVersion)
		}
	}
	exports := col.Exports()
	if emf == 0 || len(exports) == 0 {
		t.Fatalf("%d EMF GatewayInfo lines, %d exports: want both", emf, len(exports))
	}
	text := otlptest.Render(exports[len(exports)-1].Request, otlptest.Options{NamesOnly: otelsetup.RuntimeScopes, Omit: otelsetup.ConditionalRuntimeMetrics})
	_, info, ok := strings.Cut(text, "shn.gateway.info ")
	if !ok {
		t.Fatalf("no shn.gateway.info in the export:\n%s", text)
	}
	info, _, _ = strings.Cut(info, "\n  shn.")
	if n := strings.Count(info, "shn.wire_protocol="+sent+"}"); n == 0 || n != strings.Count(info, "shn.wire_protocol=") {
		t.Errorf("shn.gateway.info's points name a wire protocol other than the version sent, %s (the SDK states %s):\n%s", sent, shnsdk.WireProtocolVersion, info)
	}
}
