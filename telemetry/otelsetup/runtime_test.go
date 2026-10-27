package otelsetup

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"os"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
	"github.com/SmartHealthNetwork/shn-gateway/internal/otlptest"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
)

// readings are rec's readings as "kind/role" strings.
func readings(rec *endpointcert.Recorder) []string {
	var out []string
	for _, r := range rec.Readings() {
		out = append(out, r.Tag.Kind().String()+"/"+r.Role.String())
	}
	return out
}

// MON-R25: each OTLP protocol records the collector's certificate under
// otlp from a real TLS handshake, and the client certificate as the
// gateway's own; a plain endpoint records no server certificate.
func TestExport_RecordsTheCollectorAndClientCertificates_MON_R25(t *testing.T) {
	pki := newTestPKI(t)
	server, err := tls.LoadX509KeyPair(pki.serverCert, pki.serverKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, protocol := range []string{ProtocolHTTP, ProtocolGRPC} {
		t.Run(protocol, func(t *testing.T) {
			col := otlptest.New()
			endpoint := map[string]string{ProtocolHTTP: col.HTTPS(t, server).URL + "/v1/metrics", ProtocolGRPC: col.GRPCS(t, server)}[protocol]
			rec := endpointcert.New()
			c := Load(env(map[string]string{
				"OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_PROTOCOL": protocol, "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": endpoint,
				"OTEL_EXPORTER_OTLP_CERTIFICATE": pki.caFile, "OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE": pki.clientCert,
				"OTEL_EXPORTER_OTLP_CLIENT_KEY": pki.clientKey,
			}), nil)
			if c.OTLP == nil {
				t.Fatalf("OTLP off: %+v", c.Problems)
			}
			tel := Build(c, testIdentity, Options{Logf: func(string, ...any) {}, Certs: rec})
			defer tel.Shutdown(context.Background())
			if got := readings(rec); !slices.Equal(got, []string{"otlp/self"}) {
				t.Fatalf("readings after build %v, want the client certificate only", got)
			}
			exportOnce(tel)
			if len(col.Exports()) != 1 {
				t.Fatalf("%d exports, want 1: %+v", len(col.Exports()), tel.ExportCheck())
			}
			if got := readings(rec); !slices.Equal(got, []string{"otlp/self", "otlp/server"}) {
				t.Fatalf("readings after an export %v, want the collector's certificate too", got)
			}
		})
	}
	// Plain HTTP: no handshake, no server reading.
	col := otlptest.New()
	rec := endpointcert.New()
	plain := Build(Load(env(map[string]string{"OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": col.HTTP(t).URL + "/v1/metrics"}), nil),
		testIdentity, Options{Logf: func(string, ...any) {}, Certs: rec})
	defer plain.Shutdown(context.Background())
	exportOnce(plain)
	if got := readings(rec); len(got) != 0 {
		t.Fatalf("a plain endpoint recorded %v", got)
	}
}

// MON-R25: the pull listener's own certificate is recorded under
// metrics-pull when it is loaded; plain loopback pull records nothing.
func TestPull_RecordsItsOwnCertificate_MON_R25(t *testing.T) {
	pki := newTestPKI(t)
	rec := endpointcert.New()
	tel := Build(Load(env(pullEnv("OTEL_EXPORTER_PROMETHEUS_HOST", "0.0.0.0", "METRICS_PULL_TOKEN", pullSecret,
		"METRICS_PULL_TLS_CERT_FILE", pki.serverCert, "METRICS_PULL_TLS_KEY_FILE", pki.serverKey)), nil),
		testIdentity, Options{Logf: func(string, ...any) {}, Certs: rec})
	defer tel.Shutdown(context.Background())
	if got := readings(rec); !slices.Equal(got, []string{"metrics-pull/self"}) {
		t.Fatalf("readings %v, want the pull listener's certificate", got)
	}
	plain := endpointcert.New()
	tel2 := Build(Load(env(pullEnv()), nil), testIdentity, Options{Logf: func(string, ...any) {}, Certs: plain})
	defer tel2.Shutdown(context.Background())
	if got := readings(plain); len(got) != 0 {
		t.Fatalf("plain pull recorded %v", got)
	}
}

// runtimeNames are instruments of each runtime family that arrive whatever
// the platform.
var runtimeNames = []string{"go.memory.used", "go.goroutine.count", "process.cpu.time", "process.uptime"}

// MON-R28: the Go runtime and process instruments arrive over push and
// over pull, beside the catalogue's, with no deprecated runtime.go.* name
// even when the library's own switch asks for them; process.memory.usage
// arrives exactly where /proc/self/statm can be read.
func TestRuntimeMetrics_ArriveOverPushAndPull_MON_R28(t *testing.T) {
	t.Setenv(deprecatedRuntimeEnv, "true")
	_, statmErr := os.ReadFile("/proc/self/statm")
	// go.memory.limit is reported only while a memory limit is set.
	prev := debug.SetMemoryLimit(1 << 40)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })
	col := otlptest.New()
	settings := pullEnv("OTEL_METRICS_EXPORTER", "otlp,prometheus", "OTEL_EXPORTER_OTLP_PROTOCOL", ProtocolHTTP,
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", col.HTTP(t).URL+"/v1/metrics")
	settings[deprecatedRuntimeEnv] = "true"
	c := Load(env(settings), []string{deprecatedRuntimeEnv})
	tel := Build(c, testIdentity, Options{Logf: func(string, ...any) {}})
	defer tel.Shutdown(context.Background())
	exportOnce(tel)
	push := otlptest.Render(col.Exports()[0].Request, otlptest.Options{})
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + servePull(t, tel) + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	pull := string(b)
	names := append(slices.Clone(runtimeNames), ConditionalRuntimeMetrics...)
	for _, n := range names {
		promName := map[string]string{
			"go.memory.used": "go_memory_used_bytes", "go.goroutine.count": "go_goroutine_count",
			"process.cpu.time": "process_cpu_time_seconds_total", "process.uptime": "process_uptime_seconds",
			"process.memory.usage": "process_memory_usage_bytes", "go.memory.limit": "go_memory_limit_bytes",
		}[n]
		wantIt := n != "process.memory.usage" || statmErr == nil
		if strings.Contains(push, "  "+n+" ") != wantIt {
			t.Errorf("push: %s present=%t, want %t:\n%s", n, !wantIt, wantIt, push)
		}
		if strings.Contains(pull, "\n"+promName) != wantIt {
			t.Errorf("pull: %s present=%t, want %t:\n%s", promName, !wantIt, wantIt, pull)
		}
	}
	// Every series of an instrument whose SDK kind the catalogue shares with
	// a one-series instrument arrives, none as the overflow series.
	// Units, for the families the goldens leave out.
	for _, want := range []string{`process.memory.usage unit="By"`, `go.memory.limit unit="By"`} {
		if (statmErr == nil || !strings.HasPrefix(want, "process.memory.usage")) && !strings.Contains(push, want) {
			t.Errorf("push lacks %s:\n%s", want, push)
		}
	}
	for _, typ := range []string{"stack", "other"} {
		if !strings.Contains(push, "{go.memory.type="+typ+"}") {
			t.Errorf("push: go.memory.used lacks go.memory.type=%s:\n%s", typ, push)
		}
	}
	for _, mode := range []string{"user", "system"} {
		if !strings.Contains(push, "{cpu.mode="+mode+"}") || !strings.Contains(pull, `cpu_mode="`+mode+`"`) {
			t.Errorf("process.cpu.time lacks cpu.mode=%s:\npush:\n%s\npull:\n%s", mode, push, pull)
		}
	}
	if strings.Contains(push, "otel.metric.overflow") || strings.Contains(pull, "otel_metric_overflow") {
		t.Errorf("a series overflowed its kind's limit:\npush:\n%s\npull:\n%s", push, pull)
	}
	if strings.Contains(push, "process.runtime.go.") || strings.Contains(push, "runtime.uptime") ||
		strings.Contains(pull, "process_runtime_go_") || strings.Contains(pull, "runtime_uptime") {
		t.Fatalf("a deprecated runtime.go.* metric was exported:\npush:\n%s\npull:\n%s", push, pull)
	}
}

// MON-R15: process.memory.usage is the statm resident pages times the page
// size, and absent when statm cannot be read or parsed, never 0; CPU time is
// absent when the platform cannot give it; uptime runs from the start.
func TestProcessMetrics_UnknownIsAbsent_MON_R15(t *testing.T) {
	page := int64(os.Getpagesize())
	for name, tc := range map[string]struct {
		statm string
		err   error
		want  int64
		ok    bool
	}{
		"read":         {statm: "1000 25 10 1 0 30 0\n", want: 25 * page, ok: true},
		"unreadable":   {err: errors.New("no /proc here")},
		"malformed":    {statm: "1000\n"},
		"not a number": {statm: "1000 lots 3\n"},
	} {
		got, ok := statmRSS(func(string) ([]byte, error) { return []byte(tc.statm), tc.err })
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: statmRSS = %d, %t; want %d, %t", name, got, ok, tc.want, tc.ok)
		}
	}

	start := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	for _, known := range []bool{true, false} {
		reader := sdkmetric.NewManualReader()
		p := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
		src := processSource{
			cpu:   func() (time.Duration, time.Duration, bool) { return 3 * time.Second, time.Second, known },
			rss:   func() (int64, bool) { return 4096, known },
			start: start, now: func() time.Time { return start.Add(90 * time.Second) },
		}
		if err := startProcessMetrics(p.Meter(processScope), src); err != nil {
			t.Fatal(err)
		}
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				switch d := m.Data.(type) {
				case metricdata.Sum[float64]:
					for _, dp := range d.DataPoints {
						mode, _ := dp.Attributes.Value("cpu.mode")
						got[m.Name+"/"+mode.AsString()] = m.Unit + " " + strconv.FormatFloat(dp.Value, 'f', -1, 64)
					}
				case metricdata.Sum[int64]:
					for _, dp := range d.DataPoints {
						got[m.Name] = m.Unit + " " + strconv.FormatInt(dp.Value, 10)
					}
				case metricdata.Gauge[float64]:
					for _, dp := range d.DataPoints {
						got[m.Name] = m.Unit + " " + strconv.FormatFloat(dp.Value, 'f', -1, 64)
					}
				}
			}
		}
		want := map[string]string{"process.uptime": "s 90"}
		if known {
			want["process.cpu.time/user"] = "s 3"
			want["process.cpu.time/system"] = "s 1"
			want["process.memory.usage"] = "By 4096"
		}
		if len(got) != len(want) {
			t.Fatalf("known=%t: %v, want %v", known, got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("known=%t: %s = %q, want %q", known, k, got[k], v)
			}
		}
	}
}

// The deprecated runtime flag is the runtime library's, not an unread
// OpenTelemetry setting: Load never calls it ignored (app's noEffectSettings
// says what it does instead).
func TestDeprecatedRuntimeFlagIsNotCalledUnread(t *testing.T) {
	c := Load(env(map[string]string{"OTEL_METRICS_EXPORTER": "prometheus", deprecatedRuntimeEnv: "true"}), []string{deprecatedRuntimeEnv})
	for _, w := range c.Warnings {
		if strings.Contains(w, deprecatedRuntimeEnv) {
			t.Errorf("Load warns about %s, which the runtime library reads: %q", deprecatedRuntimeEnv, w)
		}
	}
}

// process.uptime runs from when the provider is built, on the clock it is
// handed, so an injected clock gives a true uptime rather than one measured
// against the real clock.
func TestProcessUptimeUsesOneClock(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	src := ownProcess(func() time.Time { return now })
	now = now.Add(90 * time.Second)
	if got := src.now().Sub(src.start); got != 90*time.Second {
		t.Fatalf("uptime %v, want 1m30s on the injected clock", got)
	}
}

// RuntimeMetrics is exactly what the runtime and process scopes export: every
// instrument they carry is listed with its unit and closed attributes, and
// every listed one arrives (process.memory.usage where statm can be read).
func TestRuntimeMetricsAreTheList(t *testing.T) {
	prev := debug.SetMemoryLimit(1 << 40)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })
	_, statmErr := os.ReadFile("/proc/self/statm")
	col := otlptest.New()
	tel, _, _ := build(t, ProtocolHTTP, col.HTTP(t).URL+"/v1/metrics", nil)
	exportOnce(tel)
	seen := map[string]bool{}
	for _, rm := range col.Exports()[0].Request.GetResourceMetrics() {
		for _, sm := range rm.GetScopeMetrics() {
			if !slices.Contains(RuntimeScopes, sm.GetScope().GetName()) {
				continue
			}
			for _, m := range sm.GetMetrics() {
				want, ok := RuntimeMetrics[m.GetName()]
				if !ok {
					t.Errorf("%s (scope %s) is not in RuntimeMetrics", m.GetName(), sm.GetScope().GetName())
					continue
				}
				seen[m.GetName()] = true
				if m.GetUnit() != want.Unit {
					t.Errorf("%s unit %q, RuntimeMetrics says %q", m.GetName(), m.GetUnit(), want.Unit)
				}
				for _, kv := range pointAttributes(m) {
					if vals, ok := want.Attributes[kv[0]]; !ok || !slices.Contains(vals, kv[1]) {
						t.Errorf("%s carries %s=%s, outside RuntimeMetrics", m.GetName(), kv[0], kv[1])
					}
				}
			}
		}
	}
	for name := range RuntimeMetrics {
		if !seen[name] && (name != "process.memory.usage" || statmErr == nil) {
			t.Errorf("%s is listed but was not exported", name)
		}
	}
}

// pointAttributes are every data point's attributes of m, as key/value pairs.
func pointAttributes(m *metricpb.Metric) [][2]string {
	var out [][2]string
	add := func(kvs []*commonpb.KeyValue) {
		for _, kv := range kvs {
			out = append(out, [2]string{kv.GetKey(), kv.GetValue().GetStringValue()})
		}
	}
	switch d := m.GetData().(type) {
	case *metricpb.Metric_Sum:
		for _, p := range d.Sum.GetDataPoints() {
			add(p.GetAttributes())
		}
	case *metricpb.Metric_Gauge:
		for _, p := range d.Gauge.GetDataPoints() {
			add(p.GetAttributes())
		}
	case *metricpb.Metric_Histogram:
		for _, p := range d.Histogram.GetDataPoints() {
			add(p.GetAttributes())
		}
	}
	return out
}
