package otelsetup

import (
	"encoding/pem"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// MON-R1: with no monitoring setting nothing is configured, and nothing about
// monitoring is reported, not even a check.
func TestLoad_OffByDefault_MON_R1(t *testing.T) {
	for name, m := range map[string]map[string]string{
		"nothing set":     {},
		"unrelated":       {"ROLE": "payer", "METRICS_SERVICE": "gw"},
		"set but empty":   {"OTEL_METRICS_EXPORTER": ""},
		"exporter none":   {"OTEL_METRICS_EXPORTER": "none"},
		"disabled anyway": {"OTEL_SDK_DISABLED": "false"},
	} {
		t.Run(name, func(t *testing.T) {
			c := Load(env(m), []string{"OTEL_METRICS_EXPORTER"})
			if c.OTLP != nil || len(c.Problems) != 0 || len(c.Warnings) != 0 {
				t.Fatalf("Load = %+v, want nothing built, no problem, no warning", c)
			}
			if c.Mode != ModeOff {
				t.Fatalf("mode %s, want off", c.Mode)
			}
			tel := Build(c, Identity{}, Options{})
			if tel.Sink() != nil || tel.provider != nil {
				t.Fatal("a meter provider was built with no exporter selected")
			}
			if tel.ExportCheck().Present {
				t.Fatal("telemetry-export reported with nothing exported")
			}
			// A setting that asks for nothing is still a monitoring setting:
			// its check, if any, passes.
			if st := tel.ConfigCheck(); st.Present && !st.OK {
				t.Fatalf("telemetry-config fails with nothing invalid: %+v", st)
			}
		})
	}
	if c := Load(env(map[string]string{}), nil); c.Set {
		t.Fatal("Set with nothing set")
	}
}

// MON-R2, MON-R3, MON-R4: the environment subset resolves with the
// specification's defaults and precedence.
func TestLoad_Environment_MON_R2_R3_R4(t *testing.T) {
	for _, c := range []struct {
		name     string
		env      map[string]string
		protocol string
		endpoint string
		check    func(t *testing.T, c Config)
	}{
		{name: "defaults over http", env: map[string]string{"OTEL_METRICS_EXPORTER": "otlp"},
			protocol: ProtocolHTTP, endpoint: "http://localhost:4318/v1/metrics",
			check: func(t *testing.T, c Config) {
				if c.Interval != 60*time.Second || c.ExportTimeout != 30*time.Second || c.OTLP.Timeout != 10*time.Second {
					t.Fatalf("interval %s timeout %s otlp timeout %s", c.Interval, c.ExportTimeout, c.OTLP.Timeout)
				}
				if c.Temporality != TemporalityCumulative || c.Histogram != HistogramExplicit || c.OTLP.Gzip {
					t.Fatalf("temporality %s histogram %s gzip %t", c.Temporality, c.Histogram, c.OTLP.Gzip)
				}
			}},
		{name: "general endpoint gets the metrics path", env: map[string]string{"OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector.example:4318/"},
			protocol: ProtocolHTTP, endpoint: "https://collector.example:4318/v1/metrics"},
		{name: "general endpoint with a query keeps it", env: map[string]string{"OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector.example/base?tenant=t1"},
			protocol: ProtocolHTTP, endpoint: "https://collector.example/base/v1/metrics?tenant=t1"},
		{name: "metrics endpoint used as given and wins", env: map[string]string{"OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://ignored:4318", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://collector:4318/custom"},
			protocol: ProtocolHTTP, endpoint: "http://collector:4318/custom"},
		{name: "grpc default", env: map[string]string{"OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"},
			protocol: ProtocolGRPC, endpoint: "http://localhost:4317"},
		{name: "grpc general endpoint as given", env: map[string]string{"OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_PROTOCOL": "grpc", "OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector:4317"},
			protocol: ProtocolGRPC, endpoint: "https://collector:4317"},
		{name: "protocol in any case", env: map[string]string{"OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_PROTOCOL": "GRPC"},
			protocol: ProtocolGRPC, endpoint: "http://localhost:4317"},
		{name: "a header gRPC can carry", env: map[string]string{"OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_PROTOCOL": "grpc", "OTEL_EXPORTER_OTLP_HEADERS": "Authorization=Bearer%20t,x-tenant-bin=caf%C3%A9"},
			protocol: ProtocolGRPC, endpoint: "http://localhost:4317"},
		{name: "metrics protocol wins", env: map[string]string{"OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL": "grpc"},
			protocol: ProtocolGRPC, endpoint: "http://localhost:4317"},
		{name: "exporter list and case", env: map[string]string{"OTEL_METRICS_EXPORTER": " none , OTLP "},
			protocol: ProtocolHTTP, endpoint: "http://localhost:4318/v1/metrics"},
		{name: "every setting", env: map[string]string{
			"OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_HEADERS": "authorization=Bearer%20abc, x-tenant = t1",
			"OTEL_EXPORTER_OTLP_METRICS_TIMEOUT": "2500", "OTEL_EXPORTER_OTLP_COMPRESSION": "gzip",
			"OTEL_METRIC_EXPORT_INTERVAL": "2000", "OTEL_METRIC_EXPORT_TIMEOUT": "5000",
			"OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE":        "Delta",
			"OTEL_EXPORTER_OTLP_METRICS_DEFAULT_HISTOGRAM_AGGREGATION": "base2_exponential_bucket_histogram",
			"OTEL_SERVICE_NAME": "svc", "OTEL_RESOURCE_ATTRIBUTES": "team=ops,service.instance.id=pod-1"},
			protocol: ProtocolHTTP, endpoint: "http://localhost:4318/v1/metrics",
			check: func(t *testing.T, c Config) {
				if c.OTLP.Headers["authorization"] != "Bearer abc" || c.OTLP.Headers["x-tenant"] != "t1" || len(c.OTLP.Headers) != 2 {
					t.Fatalf("headers %v", c.OTLP.Headers)
				}
				if c.OTLP.Timeout != 2500*time.Millisecond || !c.OTLP.Gzip || c.Interval != 2*time.Second || c.ExportTimeout != 5*time.Second {
					t.Fatalf("timeout %s gzip %t interval %s export timeout %s", c.OTLP.Timeout, c.OTLP.Gzip, c.Interval, c.ExportTimeout)
				}
				if c.Temporality != TemporalityDelta || c.Histogram != HistogramExponential || c.ServiceName != "svc" {
					t.Fatalf("temporality %s histogram %s service %s", c.Temporality, c.Histogram, c.ServiceName)
				}
				if c.ResourceAttrs["team"] != "ops" || c.ResourceAttrs["service.instance.id"] != "pod-1" {
					t.Fatalf("resource attributes %v", c.ResourceAttrs)
				}
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := Load(env(c.env), nil)
			if len(got.Problems) != 0 {
				t.Fatalf("problems %+v", got.Problems)
			}
			if got.OTLP == nil || got.Mode != ModeEnvironment || !got.Set {
				t.Fatalf("OTLP not built: %+v", got)
			}
			if got.OTLP.Protocol != c.protocol || got.OTLP.Endpoint != c.endpoint {
				t.Fatalf("protocol %s endpoint %s, want %s %s", got.OTLP.Protocol, got.OTLP.Endpoint, c.protocol, c.endpoint)
			}
			if c.check != nil {
				c.check(t, got)
			}
		})
	}
}

// writeFile writes content to a new file in a test directory.
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// MON-R44, MON-R42: each invalid monitoring value turns off only the piece
// it affects and is a Problem naming the setting and the fix, never the
// value. Load never fails, so a gateway never refuses to boot for one.
func TestLoad_InvalidSettingTurnsItsPieceOff_MON_R44(t *testing.T) {
	notPEM := writeFile(t, "ca.pem", "not a certificate")
	otlp := func(kv ...string) map[string]string {
		m := map[string]string{"OTEL_METRICS_EXPORTER": "otlp"}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	for _, c := range []struct {
		name, setting, value string
		env                  map[string]string
		otlpOn               bool // the piece it affects is not OTLP export
		piece                string
	}{
		{"protocol http/json", "OTEL_EXPORTER_OTLP_PROTOCOL", "http/json", nil, false, "OTLP export"},
		{"metrics protocol unknown", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "carrier-pigeon", nil, false, "OTLP export"},
		{"endpoint scheme", "OTEL_EXPORTER_OTLP_ENDPOINT", "ftp://collector:21", nil, false, "OTLP export"},
		{"endpoint without host", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "http:///v1/metrics", nil, false, "OTLP export"},
		{"endpoint with credentials", "OTEL_EXPORTER_OTLP_ENDPOINT", "https://user:pw-in-url@collector", nil, false, "OTLP export"},
		{"headers not key=value", "OTEL_EXPORTER_OTLP_HEADERS", "Bearer-token-alone", nil, false, "OTLP export"},
		{"headers bad escape", "OTEL_EXPORTER_OTLP_METRICS_HEADERS", "authorization=Bearer%zz-secret", nil, false, "OTLP export"},
		{"timeout not a number", "OTEL_EXPORTER_OTLP_TIMEOUT", "ten-seconds", nil, true, "default 10000"},
		{"timeout zero", "OTEL_EXPORTER_OTLP_METRICS_TIMEOUT", "0", nil, true, "default 10000"},
		{"compression", "OTEL_EXPORTER_OTLP_COMPRESSION", "brotli", nil, true, "default none"},
		{"interval negative", "OTEL_METRIC_EXPORT_INTERVAL", "-60000", nil, true, "default 60000"},
		{"export timeout", "OTEL_METRIC_EXPORT_TIMEOUT", "30s", nil, true, "default 30000"},
		{"temporality", "OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE", "sometimes", nil, true, "default cumulative"},
		{"histogram aggregation", "OTEL_EXPORTER_OTLP_METRICS_DEFAULT_HISTOGRAM_AGGREGATION", "linear", nil, true, "default explicit_bucket_histogram"},
		{"certificate not PEM", "OTEL_EXPORTER_OTLP_CERTIFICATE", notPEM, map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector"}, false, "OTLP export"},
		{"certificate missing", "OTEL_EXPORTER_OTLP_METRICS_CERTIFICATE", "/nonexistent/ca-secret.pem", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector"}, false, "OTLP export"},
		{"client key without certificate", "OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE", "", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector", "OTEL_EXPORTER_OTLP_CLIENT_KEY": notPEM}, false, "OTLP export"},
		{"client pair unreadable", "OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE", notPEM, map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector", "OTEL_EXPORTER_OTLP_CLIENT_KEY": notPEM}, false, "OTLP export"},
		{"certificate over plain http", "OTEL_EXPORTER_OTLP_CERTIFICATE", notPEM, map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector"}, false, "OTLP export"},
		{"exporter console", "OTEL_METRICS_EXPORTER", "console", map[string]string{"OTEL_METRICS_EXPORTER": "console"}, false, "effect"},
		{"exporter console beside otlp", "OTEL_METRICS_EXPORTER", "otlp,console", map[string]string{"OTEL_METRICS_EXPORTER": "otlp,console"}, true, "effect"},
		{"resource attributes", "OTEL_RESOURCE_ATTRIBUTES", "novalue-secret", nil, true, "resource attributes"},
		{"sdk disabled not a boolean reads as false", "OTEL_SDK_DISABLED", "maybe", nil, true, "effect"},
		{"grpc header name gRPC cannot carry", "OTEL_EXPORTER_OTLP_HEADERS", "x!y=secret-value", map[string]string{"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"}, false, "OTLP export"},
		{"grpc header value not printable ASCII", "OTEL_EXPORTER_OTLP_HEADERS", "x-tenant=caf%C3%A9-secret", map[string]string{"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"}, false, "OTLP export"},
		{"grpc header value with a tab", "OTEL_EXPORTER_OTLP_HEADERS", "x-tenant=a%09b-secret", map[string]string{"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"}, false, "OTLP export"},
		{"header value with a newline", "OTEL_EXPORTER_OTLP_HEADERS", "authorization=Bearer%0Ainjected-secret", nil, false, "OTLP export"},
		{"header name with a space", "OTEL_EXPORTER_OTLP_HEADERS", "bad key=secret-value", nil, false, "OTLP export"},
		{"header name with a separator", "OTEL_EXPORTER_OTLP_METRICS_HEADERS", "x(y)=secret-value", nil, false, "OTLP export"},
		{"configuration file", "OTEL_CONFIG_FILE", "/etc/otel-secret.yaml", nil, false, "monitoring export"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := otlp()
			for k, v := range c.env {
				m[k] = v
			}
			if c.value != "" {
				m[c.setting] = c.value
			}
			got := Load(env(m), nil)
			var hit *Problem
			for i := range got.Problems {
				if got.Problems[i].Setting == c.setting {
					hit = &got.Problems[i]
				}
			}
			if hit == nil {
				t.Fatalf("no problem names %s: %+v", c.setting, got.Problems)
			}
			// A tuning setting falls back to its default and turns nothing
			// off (its row's piece names the default); a setting that decides
			// where metrics go or how they authenticate turns its piece off.
			if c.piece == "effect" {
				if hit.Effect == "" || hit.Piece != "" || hit.Default != "" || !strings.Contains(hit.Line(), hit.Effect) {
					t.Fatalf("problem %+v, want an effect and nothing turned off", hit)
				}
			} else if def, tuning := strings.CutPrefix(c.piece, "default "); tuning {
				if hit.Default != def || hit.Piece != "" || !strings.Contains(hit.Line(), "the default "+def+" is used") {
					t.Fatalf("problem %+v (line %q), want the default %s used", hit, hit.Line(), def)
				}
			} else if hit.Piece != c.piece || hit.Default != "" {
				t.Fatalf("problem %+v, want piece %q turned off", hit, c.piece)
			}
			if hit.Fix == "" {
				t.Fatalf("problem %+v has no fix", hit)
			}
			if (got.OTLP != nil) != c.otlpOn {
				t.Fatalf("OTLP built=%t, want %t", got.OTLP != nil, c.otlpOn)
			}
			tel := Build(got, Identity{}, Options{Logf: func(string, ...any) {}})
			defer tel.Shutdown(t.Context())
			for _, l := range append(tel.Lines(), hit.Line()) {
				if c.value != "" && len(c.value) > 3 && strings.Contains(l, c.value) {
					t.Fatalf("a boot line carries the value: %q", l)
				}
			}
			if !strings.Contains(hit.Line(), c.setting) {
				t.Fatalf("boot line %q does not name %s", hit.Line(), c.setting)
			}
			st := tel.ConfigCheck()
			if !st.Present || st.OK || !containsString(st.Settings, c.setting) {
				t.Fatalf("telemetry-config %+v, want failing on %s", st, c.setting)
			}
			if wantOff := c.piece != "effect" && !strings.HasPrefix(c.piece, "default "); st.Off != wantOff {
				t.Fatalf("telemetry-config off=%t, want %t", st.Off, wantOff)
			}
		})
	}
}

// Trace, log and propagator settings are named in one warning and
// ignored; other unsupported OTEL_* settings in another, names only.
func TestLoad_NonMetricSettingsWarnByName_MON_R9(t *testing.T) {
	m := map[string]string{
		"OTEL_TRACES_EXPORTER": "otlp", "OTEL_PROPAGATORS": "tracecontext", "OTEL_EXPORTER_OTLP_LOGS_HEADERS": "authorization=log-secret",
		"OTEL_GO_X_EXEMPLAR": "value-secret", "OTEL_EXPORTER_OTLP_INSECURE": "true",
	}
	names := []string{"OTEL_TRACES_EXPORTER", "OTEL_PROPAGATORS", "OTEL_EXPORTER_OTLP_LOGS_HEADERS", "OTEL_GO_X_EXEMPLAR", "OTEL_EXPORTER_OTLP_INSECURE", "OTEL_TRACES_EXPORTER"}
	c := Load(env(m), names)
	if len(c.Warnings) != 2 {
		t.Fatalf("warnings %q, want two", c.Warnings)
	}
	if !strings.Contains(c.Warnings[0], "OTEL_EXPORTER_OTLP_LOGS_HEADERS, OTEL_PROPAGATORS, OTEL_TRACES_EXPORTER") {
		t.Fatalf("non-metric warning %q", c.Warnings[0])
	}
	if !strings.Contains(c.Warnings[1], "OTEL_EXPORTER_OTLP_INSECURE, OTEL_GO_X_EXEMPLAR") {
		t.Fatalf("unsupported warning %q", c.Warnings[1])
	}
	for _, w := range c.Warnings {
		if strings.Contains(w, "secret") {
			t.Fatalf("warning carries a value: %q", w)
		}
	}
	if len(c.Problems) != 0 || c.OTLP != nil {
		t.Fatalf("a warning is not a problem: %+v", c)
	}
	if !c.Set {
		t.Fatal("an OTEL_ setting is a monitoring setting: the check reports it")
	}
}

// MON-R11: plain http:// is allowed; to a host other than this one it warns.
func TestLoad_PlaintextWarnsOffLoopback_MON_R11(t *testing.T) {
	for endpoint, warns := range map[string]bool{
		"http://collector.example:4318": true, "http://localhost:4318": false, "http://127.0.0.1:4318": false,
		"http://[::1]:4318": false, "https://collector.example": false,
	} {
		c := Load(env(map[string]string{"OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_ENDPOINT": endpoint}), nil)
		if c.OTLP == nil {
			t.Fatalf("%s: OTLP off: %+v", endpoint, c.Problems)
		}
		got := false
		for _, w := range c.Warnings {
			got = got || strings.Contains(w, "plain http://")
		}
		if got != warns {
			t.Errorf("%s: warned=%t, want %t", endpoint, got, warns)
		}
	}
}

// MON-R5: OTEL_RESOURCE_ATTRIBUTES cannot set the gateway's own attributes.
func TestLoad_ReservedResourceAttributes_MON_R5(t *testing.T) {
	c := Load(env(map[string]string{"OTEL_METRICS_EXPORTER": "otlp", "OTEL_RESOURCE_ATTRIBUTES": "shn.holder_id=h-other,service.version=9,shn.role=hub,team=ops"}), nil)
	if len(c.ResourceAttrs) != 1 || c.ResourceAttrs["team"] != "ops" {
		t.Fatalf("resource attributes %v", c.ResourceAttrs)
	}
	if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "service.version, shn.holder_id, shn.role") || strings.Contains(c.Warnings[0], "h-other") {
		t.Fatalf("warnings %q", c.Warnings)
	}
}

// OTEL_SDK_DISABLED=true turns export off whatever else is set, with one
// line and no problem; OTEL_CONFIG_FILE governs over it (MON-R2).
func TestLoad_Precedence_MON_R2(t *testing.T) {
	c := Load(env(map[string]string{"OTEL_SDK_DISABLED": "TRUE", "OTEL_METRICS_EXPORTER": "otlp"}), nil)
	if c.Mode != ModeDisabled || c.OTLP != nil || len(c.Problems) != 0 {
		t.Fatalf("disabled: %+v", c)
	}
	if lines := Build(c, Identity{}, Options{}).Lines(); len(lines) != 1 || !strings.Contains(lines[0], "OTEL_SDK_DISABLED=true") {
		t.Fatalf("lines %q", lines)
	}
	c = Load(env(map[string]string{"OTEL_CONFIG_FILE": "/etc/otel.yaml", "OTEL_SDK_DISABLED": "true", "OTEL_METRICS_EXPORTER": "otlp"}), nil)
	if c.Mode != ModeFile || c.OTLP != nil || len(c.Problems) != 1 || c.Problems[0].Setting != "OTEL_CONFIG_FILE" {
		t.Fatalf("file mode must govern and never fall back to the environment: %+v", c)
	}
}

// MON-R44: a tuning setting that is not valid keeps the specification's
// default, so export continues as if it were unset; the check still fails.
func TestLoad_TuningSettingsKeepTheirDefaults_MON_R44(t *testing.T) {
	c := Load(env(map[string]string{
		"OTEL_METRICS_EXPORTER": "otlp", "OTEL_METRIC_EXPORT_INTERVAL": "soon", "OTEL_METRIC_EXPORT_TIMEOUT": "-1",
		"OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE": "both", "OTEL_EXPORTER_OTLP_METRICS_DEFAULT_HISTOGRAM_AGGREGATION": "linear",
		"OTEL_EXPORTER_OTLP_TIMEOUT": "fast", "OTEL_EXPORTER_OTLP_COMPRESSION": "zstd",
	}), nil)
	if c.OTLP == nil {
		t.Fatalf("export turned off by a tuning setting: %+v", c.Problems)
	}
	if c.Interval != 60*time.Second || c.ExportTimeout != 30*time.Second || c.Temporality != TemporalityCumulative ||
		c.Histogram != HistogramExplicit || c.OTLP.Timeout != 10*time.Second || c.OTLP.Gzip {
		t.Fatalf("not the defaults: interval %s timeout %s temporality %s histogram %s otlp timeout %s gzip %t",
			c.Interval, c.ExportTimeout, c.Temporality, c.Histogram, c.OTLP.Timeout, c.OTLP.Gzip)
	}
	if len(c.Problems) != 6 {
		t.Fatalf("%d problems, want one per setting: %+v", len(c.Problems), c.Problems)
	}
	tel := Build(c, Identity{}, Options{Logf: func(string, ...any) {}})
	defer tel.Shutdown(t.Context())
	if st := tel.ConfigCheck(); !st.Present || st.OK || len(st.Settings) != 6 || st.Off {
		t.Fatalf("telemetry-config %+v: want six settings, none turning export off", st)
	}
}

// testCAPEM writes a PEM CA certificate a test can name.
func testCAPEM(t *testing.T) string {
	t.Helper()
	srv := httptest.NewTLSServer(nil)
	defer srv.Close()
	return writeFile(t, "ca.pem", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})))
}

// MON-R44, MON-R3: a metrics form that is not valid is treated as unset, so
// a valid general form applies; export is off only when no valid form of a
// destination or credential setting remains, never sent to the built-in
// localhost default in its place.
func TestLoad_MetricsFormFallsBackToGeneral_MON_R44(t *testing.T) {
	ca := testCAPEM(t)
	for _, c := range []struct {
		name    string
		env     map[string]string
		on      bool
		check   func(t *testing.T, c Config)
		instead string // the metrics setting reported with the general one applying
		off     []string
	}{
		{name: "endpoint falls back to the general endpoint", on: true, instead: "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
			env: map[string]string{"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "collector-without-scheme", "OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector.example"},
			check: func(t *testing.T, c Config) {
				if c.OTLP.Endpoint != "https://collector.example/v1/metrics" {
					t.Fatalf("endpoint %s", c.OTLP.Endpoint)
				}
			}},
		{name: "endpoint with no general form is off, not localhost",
			env: map[string]string{"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "collector-without-scheme"}, off: []string{"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"}},
		{name: "endpoint with an invalid general form is off",
			env: map[string]string{"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "ftp://a", "OTEL_EXPORTER_OTLP_ENDPOINT": "ftp://b"},
			off: []string{"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_EXPORTER_OTLP_ENDPOINT"}},
		{name: "protocol falls back to the general protocol", on: true, instead: "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL",
			env: map[string]string{"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL": "http/json", "OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"},
			check: func(t *testing.T, c Config) {
				if c.OTLP.Protocol != ProtocolGRPC || c.OTLP.Endpoint != "http://localhost:4317" {
					t.Fatalf("protocol %s endpoint %s", c.OTLP.Protocol, c.OTLP.Endpoint)
				}
			}},
		{name: "headers fall back to the general headers", on: true, instead: "OTEL_EXPORTER_OTLP_METRICS_HEADERS",
			env: map[string]string{"OTEL_EXPORTER_OTLP_METRICS_HEADERS": "no-equals-sign", "OTEL_EXPORTER_OTLP_HEADERS": "authorization=Bearer%20t"},
			check: func(t *testing.T, c Config) {
				if c.OTLP.Headers["authorization"] != "Bearer t" {
					t.Fatalf("headers %v", c.OTLP.Headers)
				}
			}},
		{name: "certificate falls back to the general certificate", on: true, instead: "OTEL_EXPORTER_OTLP_METRICS_CERTIFICATE",
			env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector.example", "OTEL_EXPORTER_OTLP_METRICS_CERTIFICATE": "/nonexistent/ca.pem", "OTEL_EXPORTER_OTLP_CERTIFICATE": ca},
			check: func(t *testing.T, c Config) {
				if c.OTLP.TLS == nil || c.OTLP.TLS.RootCAs == nil {
					t.Fatal("the general certificate is not the trust root")
				}
			}},
		{name: "timeout falls back to the general timeout", on: true, instead: "OTEL_EXPORTER_OTLP_METRICS_TIMEOUT",
			env: map[string]string{"OTEL_EXPORTER_OTLP_METRICS_TIMEOUT": "soon", "OTEL_EXPORTER_OTLP_TIMEOUT": "4000"},
			check: func(t *testing.T, c Config) {
				if c.OTLP.Timeout != 4*time.Second {
					t.Fatalf("timeout %s", c.OTLP.Timeout)
				}
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := map[string]string{"OTEL_METRICS_EXPORTER": "otlp"}
			for k, v := range c.env {
				m[k] = v
			}
			got := Load(env(m), nil)
			if (got.OTLP != nil) != c.on {
				t.Fatalf("OTLP built=%t, want %t: %+v", got.OTLP != nil, c.on, got.Problems)
			}
			if c.instead != "" {
				if len(got.Problems) != 1 || got.Problems[0].Setting != c.instead || got.Problems[0].Instead == "" || !strings.Contains(got.Problems[0].Line(), "applies instead") {
					t.Fatalf("problems %+v, want %s reported with the general form applying", got.Problems, c.instead)
				}
			}
			for _, setting := range c.off {
				found := false
				for _, p := range got.Problems {
					found = found || (p.Setting == setting && p.Piece == "OTLP export")
				}
				if !found {
					t.Fatalf("problems %+v, want %s turning OTLP export off", got.Problems, setting)
				}
			}
			if c.check != nil {
				c.check(t, got)
			}
		})
	}
}

// MON-R44: an entry of OTEL_METRICS_EXPORTER the gateway does not recognize
// is dropped and named, and the valid entries still run.
func TestLoad_ExporterSelectionDropsUnknownEntries_MON_R44(t *testing.T) {
	for value, want := range map[string]struct{ otlp, pull bool }{
		"otlp,bogus":       {otlp: true},
		"bogus, otlp":      {otlp: true},
		"prometheus,bogus": {pull: true},
		"otlp,bogus,junk":  {otlp: true},
		"bogus":            {},
	} {
		c := Load(env(map[string]string{"OTEL_METRICS_EXPORTER": value}), nil)
		if (c.OTLP != nil) != want.otlp || c.PullRequested != want.pull {
			t.Errorf("%q: otlp=%t pull=%t, want %+v", value, c.OTLP != nil, c.PullRequested, want)
		}
		named := false
		for _, p := range c.Problems {
			if p.Setting == "OTEL_METRICS_EXPORTER" && p.Effect != "" {
				if named {
					t.Errorf("%q: the unknown entries are reported twice", value)
				}
				named = true
			}
		}
		if !named {
			t.Errorf("%q: the unknown entry is not named: %+v", value, c.Problems)
		}
	}
}

// The guard that refuses certificate settings over a plain http:// endpoint:
// a valid CA over http:// is that one problem and turns OTLP export off; a CA
// that is not valid is its own one problem, never reported twice.
func TestLoad_CertificateNeedsHTTPS_MON_R44(t *testing.T) {
	for _, c := range []struct {
		name, ca string
		fix      string
	}{
		{"a valid CA over http://", testCAPEM(t), "certificate settings need an https:// endpoint"},
		{"a CA that is not valid over http://", writeFile(t, "bad.pem", "not a certificate"), "name a readable PEM file of CA certificates"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := Load(env(map[string]string{"OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector.example:4318", "OTEL_EXPORTER_OTLP_CERTIFICATE": c.ca}), nil)
			if got.OTLP != nil {
				t.Fatal("OTLP export is on with a certificate over http://")
			}
			if len(got.Problems) != 1 || got.Problems[0].Setting != "OTEL_EXPORTER_OTLP_CERTIFICATE" || got.Problems[0].Piece != "OTLP export" || got.Problems[0].Fix != c.fix {
				t.Fatalf("problems %+v, want exactly one: %q", got.Problems, c.fix)
			}
		})
	}
}
