package otelsetup

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/internal/otlptest"
	"github.com/SmartHealthNetwork/shn-gateway/telemetry"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// loadFileYAML loads a configuration whose only setting is OTEL_CONFIG_FILE,
// naming a file holding yaml, beside any other OTEL_* settings in extra.
func loadFileYAML(t *testing.T, yaml string, extra map[string]string) Config {
	t.Helper()
	m := map[string]string{"OTEL_CONFIG_FILE": writeFile(t, "otel.yaml", yaml)}
	for k, v := range extra {
		m[k] = v
	}
	return Load(env(m), nil)
}

// MON-R8: one periodic OTLP reader maps onto the same settings the
// environment fills: endpoint as given, headers from headers_list and
// headers (headers over it), timeout, compression, temporality, histogram
// aggregation, the reader's interval and timeout, and the resource.
func TestLoadFile_MapsOneOTLPReader_MON_R8(t *testing.T) {
	for _, format := range []string{"1.0", "1.0-rc.2"} {
		t.Run(format, func(t *testing.T) {
			c := loadFileYAML(t, `file_format: "`+format+`"
resource:
  attributes:
    - name: service.name
      value: svc-from-file
    - name: deployment.environment.name
      value: file-env
  attributes_list: "team=edge"
meter_provider:
  readers:
    - periodic:
        interval: 2000
        timeout: 1500
        exporter:
          otlp_http:
            endpoint: https://collector.example/v1/metrics
            headers_list: "x-a=1,authorization=old"
            headers:
              - name: authorization
                value: Bearer s3cret
            timeout: 900
            compression: gzip
            temporality_preference: low_memory
            default_histogram_aggregation: base2_exponential_bucket_histogram
`, nil)
			if c.Mode != ModeFile || c.OTLP == nil || len(c.Problems) != 0 {
				t.Fatalf("mode %s, OTLP %v, problems %+v", c.Mode, c.OTLP, c.Problems)
			}
			o := c.OTLP
			if o.Protocol != ProtocolHTTP || o.Endpoint != "https://collector.example/v1/metrics" || !o.Gzip || o.Timeout != 900*time.Millisecond || o.TLS == nil {
				t.Errorf("OTLP %+v", o)
			}
			if o.Headers["authorization"] != "Bearer s3cret" || o.Headers["x-a"] != "1" || len(o.Headers) != 2 {
				t.Errorf("headers %v: headers over headers_list", o.Headers)
			}
			if c.Interval != 2*time.Second || c.ExportTimeout != 1500*time.Millisecond || c.Temporality != TemporalityLowMemory || c.Histogram != HistogramExponential {
				t.Errorf("interval %s timeout %s temporality %s histogram %s", c.Interval, c.ExportTimeout, c.Temporality, c.Histogram)
			}
			if c.ServiceName != "svc-from-file" || c.ResourceAttrs["deployment.environment.name"] != "file-env" || c.ResourceAttrs["team"] != "edge" {
				t.Errorf("service %q resource %v", c.ServiceName, c.ResourceAttrs)
			}
		})
	}
}

// MON-R8: a grpc reader keeps its endpoint as given, with TLS files read by
// the same rules as the environment's.
func TestLoadFile_GRPCReaderAndTLS_MON_R8(t *testing.T) {
	ca := testCAPEM(t)
	c := loadFileYAML(t, `file_format: "1.0"
meter_provider:
  readers:
    - periodic:
        exporter:
          otlp_grpc:
            endpoint: https://collector.example:4317
            temporality_preference: delta
            tls:
              ca_file: `+ca+`
`, nil)
	if c.OTLP == nil || c.OTLP.Protocol != ProtocolGRPC || c.OTLP.Endpoint != "https://collector.example:4317" || c.OTLP.TLS == nil || c.OTLP.TLS.RootCAs == nil || c.Temporality != TemporalityDelta {
		t.Fatalf("OTLP %+v temporality %s problems %+v", c.OTLP, c.Temporality, c.Problems)
	}
}

// MON-R8: ${VAR} references are substituted from the process environment.
func TestLoadFile_SubstitutesEnvironmentReferences_MON_R8(t *testing.T) {
	t.Setenv("GW_TEST_OTLP_TOKEN", "from-env-token")
	c := loadFileYAML(t, `file_format: "1.0"
meter_provider:
  readers:
    - periodic:
        exporter:
          otlp_http:
            endpoint: http://localhost:4318/v1/metrics
            headers:
              - name: authorization
                value: Bearer ${GW_TEST_OTLP_TOKEN}
`, nil)
	if c.OTLP == nil || c.OTLP.Headers["authorization"] != "Bearer from-env-token" {
		t.Fatalf("OTLP %+v problems %+v", c.OTLP, c.Problems)
	}
}

// MON-R2: the file governs. Every other OTEL_* setting is ignored,
// OTEL_SDK_DISABLED and OTEL_METRICS_EXPORTER included, and the file's
// disabled key turns export off.
func TestLoadFile_GovernsOverTheEnvironment_MON_R2(t *testing.T) {
	file := `file_format: "1.0"
meter_provider:
  readers:
    - periodic:
        exporter:
          otlp_http:
            endpoint: http://localhost:4318/v1/metrics
`
	c := loadFileYAML(t, file, map[string]string{"OTEL_SDK_DISABLED": "true", "OTEL_METRICS_EXPORTER": "none",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://elsewhere:4318/v1/metrics"})
	if c.Mode != ModeFile || c.OTLP == nil || c.OTLP.Endpoint != "http://localhost:4318/v1/metrics" {
		t.Fatalf("the environment was read in file mode: mode %s OTLP %+v", c.Mode, c.OTLP)
	}
	c = loadFileYAML(t, `file_format: "1.0"
disabled: true
`+strings.TrimPrefix(file, `file_format: "1.0"`+"\n"), map[string]string{"OTEL_METRICS_EXPORTER": "otlp"})
	if c.Mode != ModeDisabled || c.OTLP != nil || !strings.Contains(c.DisabledBy, "OTEL_CONFIG_FILE") {
		t.Fatalf("disabled: true: mode %s OTLP %+v by %q", c.Mode, c.OTLP, c.DisabledBy)
	}
	tel := Build(c, testIdentity, Options{Logf: func(string, ...any) {}})
	if got := strings.Join(tel.Lines(), "\n"); !strings.Contains(got, "OTEL_CONFIG_FILE's disabled: true, so nothing is exported") {
		t.Errorf("boot lines %q", got)
	}
}

// MON-R8, MON-R44, MON-R10: a file that cannot be read or does not parse
// turns monitoring export off. It never falls back to the environment the
// file says to ignore, and the report never quotes the file.
func TestLoadFile_UnreadableOrUnparsableTurnsExportOff_MON_R8_R44(t *testing.T) {
	envOTLP := map[string]string{"OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://localhost:4318/v1/metrics"}
	for name, path := range map[string]string{
		"missing":     "/nonexistent/otel.yaml",
		"not yaml":    writeFile(t, "a.yaml", "file_format: [\nheaders: s3cret-in-file"),
		"wrong shape": writeFile(t, "b.yaml", "file_format: \"1.0\"\nmeter_provider: s3cret-in-file\n"),
	} {
		t.Run(name, func(t *testing.T) {
			m := map[string]string{"OTEL_CONFIG_FILE": path}
			for k, v := range envOTLP {
				m[k] = v
			}
			c := Load(env(m), nil)
			if c.Mode != ModeFile || c.OTLP != nil || len(c.Problems) != 1 || c.Problems[0].Setting != "OTEL_CONFIG_FILE" || c.Problems[0].Piece != "monitoring export" {
				t.Fatalf("mode %s OTLP %+v problems %+v", c.Mode, c.OTLP, c.Problems)
			}
			if p := c.Problems[0]; strings.Contains(p.Line()+p.Summary(), "s3cret") {
				t.Fatalf("the report quotes the file: %q", p.Line())
			}
		})
	}
}

// The trace, log and propagator sections are removed and named in one
// warning; the metrics still export.
func TestLoadFile_NonMetricSectionsWarnOnce_MON_R9(t *testing.T) {
	c := loadFileYAML(t, `file_format: "1.0"
tracer_provider:
  processors:
    - simple:
        exporter:
          console: {}
logger_provider:
  processors:
    - simple:
        exporter:
          console: {}
propagator: {}
meter_provider:
  readers:
    - periodic:
        exporter:
          otlp_http:
            endpoint: http://localhost:4318/v1/metrics
`, nil)
	if c.OTLP == nil || len(c.Problems) != 0 {
		t.Fatalf("OTLP %+v problems %+v", c.OTLP, c.Problems)
	}
	if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "tracer_provider, logger_provider, propagator") {
		t.Fatalf("warnings %q", c.Warnings)
	}
}

// MON-R44, MON-R42: what the file asks for that the gateway does not do is
// reported under the file's own setting and turns off only its piece.
func TestLoadFile_UnsupportedIsReportedAndContained_MON_R44(t *testing.T) {
	const otlp = `
    - periodic:
        exporter:
          otlp_http:
            endpoint: http://localhost:4318/v1/metrics`
	clientPEM := writeFile(t, "client.pem", "a file that exists")
	grpcTLS := func(tls string) string {
		return "\n    - periodic:\n        exporter:\n          otlp_grpc:\n            endpoint: https://localhost:4317\n            tls:\n              " + tls
	}
	for _, row := range []struct {
		name, readers, extra string
		wantOTLP             bool
		setting              string
		pieceOrEffect        string
	}{
		{"a second periodic reader", otlp + otlp, "", true, "meter_provider.readers[1]", "only one periodic OTLP reader"},
		{"a console exporter", "\n    - periodic:\n        exporter:\n          console: {}", "", false, "meter_provider.readers[0].periodic.exporter.console", "not supported, so this reader is off"},
		{"a second pull reader", "\n    - pull:\n        exporter:\n          prometheus/development: {}\n    - pull:\n        exporter:\n          prometheus/development: {}" + otlp, "", true, "meter_provider.readers[1]", "only one pull reader"},
		{"producers", otlp + "\n        producers:\n          - opencensus: {}", "", true, "meter_provider.readers[0].periodic.producers", "ignored"},
		{"an exemplar filter that keeps exemplars", otlp, "  exemplar_filter: always_on\n", true, "meter_provider.exemplar_filter", "exemplars stay off"},
		{"JSON encoding", otlp + "\n            encoding: json", "", false, "meter_provider.readers[0].periodic.exporter.otlp_http.encoding", "OTLP export"},
		{"an endpoint that is not a URL", "\n    - periodic:\n        exporter:\n          otlp_http:\n            endpoint: not-a-url", "", false, "meter_provider.readers[0].periodic.exporter.otlp_http.endpoint", "OTLP export"},
		{"a headers list that does not parse", otlp + "\n            headers_list: \"novalue\"", "", false, "meter_provider.readers[0].periodic.exporter.otlp_http.headers_list", "OTLP export"},
		{"tls.insecure", "\n    - periodic:\n        exporter:\n          otlp_grpc:\n            endpoint: http://localhost:4317\n            tls:\n              insecure: true", "", true, "meter_provider.readers[0].periodic.exporter.otlp_grpc.tls.insecure", "ignored"},
		{"an exporter this gateway does not support", "\n    - periodic:\n        exporter:\n          otlp_file/development: {}", "", false, "meter_provider.readers[0].periodic.exporter", "does not support"},
		{"a reader that is neither periodic nor pull", "\n    - {}" + otlp, "", true, "meter_provider.readers[0]", "neither periodic nor pull"},
		{"a client certificate without its key", grpcTLS("cert_file: " + clientPEM), "", false, "meter_provider.readers[0].periodic.exporter.otlp_grpc.tls.key_file", "OTLP export"},
		{"a client key without its certificate", grpcTLS("key_file: " + clientPEM), "", false, "meter_provider.readers[0].periodic.exporter.otlp_grpc.tls.cert_file", "OTLP export"},
		{"a header with no value", otlp + "\n            headers:\n              - name: authorization\n                value: null", "", true, "meter_provider.readers[0].periodic.exporter.otlp_http.headers[0].value", "not sent"},
		{"a view's cardinality limit", otlp, "  views:\n    - selector:\n        instrument_name: shn.gateway.exchanges\n      stream:\n        aggregation_cardinality_limit: 100000\n", true, "meter_provider.views[0].stream.aggregation_cardinality_limit", "ignored"},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := loadFileYAML(t, "file_format: \"1.0\"\nmeter_provider:\n"+row.extra+"  readers:"+row.readers+"\n", nil)
			if (c.OTLP != nil) != row.wantOTLP {
				t.Fatalf("OTLP %+v, want on=%v (problems %+v)", c.OTLP, row.wantOTLP, c.Problems)
			}
			var found *Problem
			for i, p := range c.Problems {
				if p.Setting == row.setting {
					found = &c.Problems[i]
				}
			}
			if found == nil || !strings.Contains(found.Piece+found.Effect, row.pieceOrEffect) || found.Fix == "" {
				t.Fatalf("no problem %s mentioning %q: %+v", row.setting, row.pieceOrEffect, c.Problems)
			}
			tel := Build(c, testIdentity, Options{Logf: func(string, ...any) {}})
			defer tel.Shutdown(context.Background())
			if !strings.Contains(strings.Join(tel.Lines(), "\n"), row.setting) {
				t.Errorf("boot lines do not name %s: %q", row.setting, tel.Lines())
			}
		})
	}
}

// MON-R44: a tuning value from the file that is not valid keeps its default,
// reported under the file's own setting, and export continues.
func TestLoadFile_TuningKeepsDefaults_MON_R44(t *testing.T) {
	c := loadFileYAML(t, `file_format: "1.0"
meter_provider:
  readers:
    - periodic:
        interval: 0
        exporter:
          otlp_http:
            endpoint: http://localhost:4318/v1/metrics
            compression: brotli
`, nil)
	if c.OTLP == nil || c.Interval != defaultInterval || c.OTLP.Gzip {
		t.Fatalf("OTLP %+v interval %s problems %+v", c.OTLP, c.Interval, c.Problems)
	}
	settings := map[string]bool{}
	for _, p := range c.Problems {
		if p.Default == "" {
			t.Errorf("problem %+v is not a fallback", p)
		}
		settings[p.Setting] = true
	}
	for _, want := range []string{"meter_provider.readers[0].periodic.interval", "meter_provider.readers[0].periodic.exporter.otlp_http.compression"} {
		if !settings[want] {
			t.Errorf("no fallback reported for %s: %+v", want, c.Problems)
		}
	}
}

// MON-R5: the file's resource cannot set the gateway's own attributes.
func TestLoadFile_ReservedResourceAttributes_MON_R5(t *testing.T) {
	c := loadFileYAML(t, `file_format: "1.0"
resource:
  attributes:
    - name: shn.role
      value: not-mine
    - name: service.version
      value: v9
    - name: team
      value: edge
meter_provider:
  readers:
    - periodic:
        exporter:
          otlp_http:
            endpoint: http://localhost:4318/v1/metrics
`, nil)
	if _, ok := c.ResourceAttrs["shn.role"]; ok || c.ResourceAttrs["team"] != "edge" {
		t.Fatalf("resource %v", c.ResourceAttrs)
	}
	if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "service.version, shn.role") {
		t.Fatalf("warnings %q", c.Warnings)
	}
}

// MON-R8, MON-R18: a reader's cardinality limits narrow the catalogue's
// per-kind limits and never raise them.
func TestLoadFile_CardinalityLimitsOnlyNarrow_MON_R8_R18(t *testing.T) {
	c := loadFileYAML(t, `file_format: "1.0"
meter_provider:
  readers:
    - periodic:
        cardinality_limits:
          default: 5
          counter: 1000000
        exporter:
          otlp_http:
            endpoint: http://localhost:4318/v1/metrics
`, nil)
	base := kindLimits(telemetry.Instruments(), nil)
	got := kindLimits(telemetry.Instruments(), c.CardinalityLimits)
	if got[sdkmetric.InstrumentKindCounter] != base[sdkmetric.InstrumentKindCounter] {
		t.Errorf("counter limit %d: a file limit above the catalogue's %d raised it", got[sdkmetric.InstrumentKindCounter], base[sdkmetric.InstrumentKindCounter])
	}
	if got[sdkmetric.InstrumentKindHistogram] != 5 || base[sdkmetric.InstrumentKindHistogram] <= 5 {
		t.Errorf("histogram limit %d (catalogue %d): the default did not narrow it", got[sdkmetric.InstrumentKindHistogram], base[sdkmetric.InstrumentKindHistogram])
	}
	// A kind the catalogue does not use (the runtime's observable up-down
	// counters) is narrowed too, from the provider's own limit, never raised.
	if _, has := base[sdkmetric.InstrumentKindObservableUpDownCounter]; has {
		t.Fatal("the catalogue uses observable up-down counters; pick a kind it does not")
	}
	if l := got[sdkmetric.InstrumentKindObservableUpDownCounter]; l != 5 {
		t.Errorf("observable up-down counter limit %d: the file's default did not narrow a kind outside the catalogue", l)
	}
	raise := loadFileYAML(t, `file_format: "1.0"
meter_provider:
  readers:
    - periodic:
        cardinality_limits:
          observable_up_down_counter: 1000000
        exporter:
          otlp_http:
            endpoint: http://localhost:4318/v1/metrics
`, nil)
	if l, has := kindLimits(telemetry.Instruments(), raise.CardinalityLimits)[sdkmetric.InstrumentKindObservableUpDownCounter]; has {
		t.Errorf("observable up-down counter limit %d: a file limit above the provider's %d raised it", l, providerCardinalityLimit)
	}
}

// MON-R8: a view is the participant's own override. It renames a stream and
// drops attributes, and keeps the gateway's histogram buckets when it names no
// aggregation. An attribute filter can add no attribute, whatever it names.
func TestGatewayView_FileViewsOverride_MON_R8(t *testing.T) {
	c := loadFileYAML(t, `file_format: "1.0"
meter_provider:
  views:
    - selector:
        instrument_name: shn.gateway.exchanges
      stream:
        name: gw.exchanges
        attribute_keys:
          excluded: [shn.outcome]
    - selector:
        instrument_name: shn.gateway.exchange.duration
      stream:
        attribute_keys:
          included: [shn.exchange, shn.not.recorded, team]
  readers:
    - periodic:
        exporter:
          otlp_http:
            endpoint: http://localhost:4318/v1/metrics
`, nil)
	if len(c.Views) != 2 || len(c.Problems) != 0 {
		t.Fatalf("views %+v problems %+v", c.Views, c.Problems)
	}
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithView(gatewayView(telemetry.Instruments(), HistogramExplicit, c.Views)))
	defer mp.Shutdown(context.Background())
	s, err := newSink(mp.Meter(scopeName), telemetry.Instruments())
	if err != nil {
		t.Fatal(err)
	}
	reg := telemetry.New(s)
	reg.Add(telemetry.Exchanges, 1, "inbound", kind, "answered")
	reg.RecordDuration(telemetry.ExchangeDuration, 2*time.Second, "inbound", kind)
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	byName := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			byName[m.Name] = m
		}
	}
	if _, ok := byName["shn.gateway.exchanges"]; ok {
		t.Error("the renamed stream is also exported under its catalogue name")
	}
	sum, ok := byName["gw.exchanges"].Data.(metricdata.Sum[float64])
	if !ok || len(sum.DataPoints) != 1 {
		t.Fatalf("gw.exchanges: %+v", byName["gw.exchanges"])
	}
	if _, has := sum.DataPoints[0].Attributes.Value("shn.outcome"); has {
		t.Error("the excluded attribute was exported")
	}
	hist, ok := byName["shn.gateway.exchange.duration"].Data.(metricdata.Histogram[float64])
	if !ok || len(hist.DataPoints) != 1 {
		t.Fatalf("duration: %+v", byName["shn.gateway.exchange.duration"])
	}
	dp := hist.DataPoints[0]
	if len(dp.Bounds) != len(telemetry.ExchangeDuration.Buckets) {
		t.Errorf("bounds %v: the view without an aggregation lost the gateway's buckets", dp.Bounds)
	}
	for _, kv := range dp.Attributes.ToSlice() {
		if kv.Key != attribute.Key("shn.exchange") {
			t.Errorf("attribute %s exported: an include list added or kept an attribute it should not", kv.Key)
		}
	}
	if dp.Attributes.Len() != 1 {
		t.Errorf("attributes %v, want shn.exchange alone", dp.Attributes.ToSlice())
	}
}

// The spec's Config file criterion (MON-R8): the same intent through the
// environment and through a configuration file exports the same metrics,
// resource and headers.
func TestConfigFile_SameIntentSameExport_MON_R8(t *testing.T) {
	for _, protocol := range []string{ProtocolHTTP, ProtocolGRPC} {
		t.Run(protocol, func(t *testing.T) {
			col := otlptest.New()
			endpoint := endpoints(t, col)[protocol]
			exporter := map[string]string{ProtocolHTTP: "otlp_http", ProtocolGRPC: "otlp_grpc"}[protocol]
			fromEnv := Load(env(map[string]string{
				"OTEL_METRICS_EXPORTER":                                    "otlp",
				"OTEL_EXPORTER_OTLP_PROTOCOL":                              protocol,
				"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT":                      endpoint,
				"OTEL_EXPORTER_OTLP_HEADERS":                               "authorization=Bearer%20" + secretToken,
				"OTEL_SERVICE_NAME":                                        "svc-same",
				"OTEL_RESOURCE_ATTRIBUTES":                                 "team=edge",
				"OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE":        "delta",
				"OTEL_EXPORTER_OTLP_METRICS_DEFAULT_HISTOGRAM_AGGREGATION": HistogramExponential,
				"OTEL_METRIC_EXPORT_TIMEOUT":                               "2000",
				"OTEL_EXPORTER_OTLP_TIMEOUT":                               "2000",
			}), nil)
			fromFile := loadFileYAML(t, `file_format: "1.0"
resource:
  attributes:
    - name: service.name
      value: svc-same
    - name: team
      value: edge
meter_provider:
  readers:
    - periodic:
        timeout: 2000
        exporter:
          `+exporter+`:
            endpoint: `+endpoint+`
            headers:
              - name: authorization
                value: Bearer `+secretToken+`
            timeout: 2000
            temporality_preference: delta
            default_histogram_aggregation: base2_exponential_bucket_histogram
`, nil)
			var renders, headers []string
			for _, c := range []Config{fromEnv, fromFile} {
				if c.OTLP == nil || len(c.Problems) != 0 {
					t.Fatalf("mode %s: OTLP %+v problems %+v", c.Mode, c.OTLP, c.Problems)
				}
				before := len(col.Exports())
				tel := Build(c, testIdentity, Options{Logf: func(string, ...any) {}})
				reg := telemetry.New(tel.Sink())
				tel.Bind(reg)
				reg.Add(telemetry.Exchanges, 1, "inbound", kind, "answered")
				reg.RecordDuration(telemetry.ExchangeDuration, 1500*time.Millisecond, "inbound", kind)
				exportOnce(tel)
				tel.Discard()
				got := col.Exports()
				if len(got) != before+1 {
					t.Fatalf("%d exports, want one more", len(got)-before)
				}
				// The runtime and process values differ from one export to the next;
				// their names, units and types are the same intent.
				renders = append(renders, otlptest.Render(got[before].Request, otlptest.Options{NamesOnly: RuntimeScopes, Omit: ConditionalRuntimeMetrics}))
				headers = append(headers, got[before].Headers["authorization"])
			}
			if renders[0] != renders[1] {
				t.Errorf("environment and file exports differ:\n--- environment\n%s\n--- file\n%s", renders[0], renders[1])
			}
			if headers[0] != headers[1] || headers[0] != "Bearer "+secretToken {
				t.Errorf("headers %q", headers)
			}
		})
	}
}

// MON-R44: top-level settings the gateway does not read are reported under
// their own path, and export goes on.
func TestLoadFile_TopLevelSettingsReported_MON_R44(t *testing.T) {
	const reader = "meter_provider:\n  readers:\n    - periodic:\n        exporter:\n          otlp_http:\n            endpoint: http://localhost:4318/v1/metrics\n"
	for _, row := range []struct{ name, head, setting, says string }{
		{"another file_format", "file_format: \"0.3\"\n", "file_format", "read as file_format 1.0"},
		{"attribute limits", "file_format: \"1.0\"\nattribute_limits:\n  attribute_count_limit: 5\n", "attribute_limits", "ignored"},
		{"a resource schema URL", "file_format: \"1.0\"\nresource:\n  schema_url: https://opentelemetry.io/schemas/1.26.0\n", "resource.schema_url", "ignored"},
		{"a resource attributes list that does not parse", "file_format: \"1.0\"\nresource:\n  attributes_list: \"novalue\"\n", "resource.attributes_list", "resource attributes"},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := loadFileYAML(t, row.head+reader, nil)
			if c.OTLP == nil || len(c.Problems) != 1 || c.Problems[0].Setting != row.setting ||
				!strings.Contains(c.Problems[0].Piece+c.Problems[0].Effect, row.says) || c.Problems[0].Fix == "" {
				t.Fatalf("OTLP %+v problems %+v", c.OTLP, c.Problems)
			}
		})
	}
	if c := loadFileYAML(t, "file_format: \"1.0-rc.2\"\n"+reader, nil); len(c.Problems) != 0 {
		t.Fatalf("a 1.0 release candidate is reported: %+v", c.Problems)
	}
}

// MON-R18, R44: only the reader that exports sets cardinality limits; a reader
// turned off leaves the live reader's limits as the catalogue's.
func TestLoadFile_LimitsOnlyFromTheExportingReader_MON_R44(t *testing.T) {
	c := loadFileYAML(t, `file_format: "1.0"
meter_provider:
  readers:
    - periodic:
        cardinality_limits:
          default: 5
        exporter:
          console: {}
    - periodic:
        exporter:
          otlp_http:
            endpoint: http://localhost:4318/v1/metrics
`, nil)
	if c.OTLP == nil || c.CardinalityLimits != nil {
		t.Fatalf("OTLP %+v, limits %v: the console reader's limits reached the live reader", c.OTLP, c.CardinalityLimits)
	}
	c = loadFileYAML(t, `file_format: "1.0"
meter_provider:
  readers:
    - periodic:
        cardinality_limits:
          default: 5
        exporter:
          otlp_http:
            endpoint: http://localhost:4318/v1/metrics
`, nil)
	if c.CardinalityLimits["default"] != 5 {
		t.Fatalf("the exporting reader's limits were not kept: %v", c.CardinalityLimits)
	}
}

// MON-R8, R44: a view the SDK would refuse, which it drops without a word, is
// reported and left out; a view setting the gateway does not apply is
// reported; an explicit histogram without boundaries keeps the instrument's
// own buckets.
func TestLoadFile_ViewsTheGatewayCannotApplyAreReported_MON_R44(t *testing.T) {
	const reader = "  readers:\n    - periodic:\n        exporter:\n          otlp_http:\n            endpoint: http://localhost:4318/v1/metrics\n"
	view := func(selector, stream string) string {
		return "file_format: \"1.0\"\nmeter_provider:\n  views:\n    - selector: " + selector + "\n      stream:\n" + stream + reader
	}
	const dur = "{instrument_name: shn.gateway.exchange.duration}"
	for _, row := range []struct {
		name, file, setting string
		kept                bool // the view still applies
	}{
		{"a selector that selects nothing", view("{}", "        name: renamed\n"), "meter_provider.views[0].selector", false},
		{"a wildcard renamed", view("{instrument_name: \"shn.*\"}", "        name: renamed\n"), "meter_provider.views[0].stream.name", false},
		{"boundaries out of order", view(dur, "        aggregation:\n          explicit_bucket_histogram:\n            boundaries: [5, 1]\n"), "meter_provider.views[0].stream.aggregation.explicit_bucket_histogram.boundaries", false},
		{"repeated boundaries", view(dur, "        aggregation:\n          explicit_bucket_histogram:\n            boundaries: [1, 1]\n"), "meter_provider.views[0].stream.aggregation.explicit_bucket_histogram.boundaries", false},
		{"explicit record_min_max", view(dur, "        aggregation:\n          explicit_bucket_histogram:\n            boundaries: [1, 5]\n            record_min_max: false\n"), "meter_provider.views[0].stream.aggregation.explicit_bucket_histogram.record_min_max", true},
		{"an exponential max_scale", view(dur, "        aggregation:\n          base2_exponential_bucket_histogram:\n            max_scale: 10\n"), "meter_provider.views[0].stream.aggregation.base2_exponential_bucket_histogram.max_scale", true},
		{"an exponential max_size", view(dur, "        aggregation:\n          base2_exponential_bucket_histogram:\n            max_size: 40\n"), "meter_provider.views[0].stream.aggregation.base2_exponential_bucket_histogram.max_size", true},
		{"an exponential record_min_max", view(dur, "        aggregation:\n          base2_exponential_bucket_histogram:\n            record_min_max: true\n"), "meter_provider.views[0].stream.aggregation.base2_exponential_bucket_histogram.record_min_max", true},
		{"an aggregation it does not name", view(dur, "        aggregation: {}\n"), "meter_provider.views[0].stream.aggregation", true},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := loadFileYAML(t, row.file, nil)
			if c.OTLP == nil || len(c.Problems) != 1 || c.Problems[0].Setting != row.setting || c.Problems[0].Fix == "" {
				t.Fatalf("OTLP %+v problems %+v", c.OTLP, c.Problems)
			}
			if (len(c.Views) == 1) != row.kept {
				t.Fatalf("views %+v, want the view kept=%v", c.Views, row.kept)
			}
		})
	}
	c := loadFileYAML(t, view(dur, "        aggregation:\n          explicit_bucket_histogram: {}\n"), nil)
	if len(c.Problems) != 0 || len(c.Views) != 1 || c.Views[0].Aggregation != HistogramExplicit || len(c.Views[0].Boundaries) != 0 {
		t.Fatalf("an explicit histogram without boundaries: views %+v problems %+v", c.Views, c.Problems)
	}
	// Exported, it is the instrument's own explicit buckets, even under an
	// exponential default.
	mr := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(mr), sdkmetric.WithView(gatewayView(telemetry.Instruments(), HistogramExponential, c.Views)))
	defer mp.Shutdown(context.Background())
	s, err := newSink(mp.Meter(scopeName), telemetry.Instruments())
	if err != nil {
		t.Fatal(err)
	}
	telemetry.New(s).RecordDuration(telemetry.ExchangeDuration, 2*time.Second, "inbound", kind)
	var rm metricdata.ResourceMetrics
	if err := mr.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != telemetry.ExchangeDuration.Name {
				continue
			}
			found = true
			h, ok := m.Data.(metricdata.Histogram[float64])
			if !ok || len(h.DataPoints) != 1 || len(h.DataPoints[0].Bounds) != len(telemetry.ExchangeDuration.Buckets) {
				t.Fatalf("the duration under an exponential default is %T %+v, want the catalogue's explicit buckets", m.Data, m.Data)
			}
		}
	}
	if !found {
		t.Fatal("no duration exported")
	}
	// An empty string selects nothing.
	if c := loadFileYAML(t, view(`{instrument_name: ""}`, "        name: renamed\n"), nil); len(c.Views) != 0 || len(c.Problems) != 1 || c.Problems[0].Setting != "meter_provider.views[0].selector" {
		t.Fatalf("an empty-string selector: views %+v problems %+v", c.Views, c.Problems)
	}
}

// MON-R7: the pull listener's protection settings set beside a file with no
// pull reader do nothing, and a warning names them, never their values.
func TestLoadFile_PullSettingsWithoutAPullReaderWarn(t *testing.T) {
	c := loadFileYAML(t, "file_format: \"1.0\"\nmeter_provider:\n  readers:\n    - periodic:\n        exporter:\n          otlp_http:\n            endpoint: http://localhost:4318/v1/metrics\n",
		map[string]string{"METRICS_PULL_TOKEN": pullSecret})
	if c.Pull != nil || len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "METRICS_PULL_TOKEN") || strings.Contains(c.Warnings[0], pullSecret) {
		t.Fatalf("pull %+v warnings %q", c.Pull, c.Warnings)
	}
}

// MON-R44: in file mode, an exporter the SDK refuses fails telemetry-config
// under the file, not under a setting the file mode ignores.
func TestConfigCheck_FileModeFaultsNameTheFile_MON_R44(t *testing.T) {
	refused := errors.New("refused")
	for name, seam := range map[string]*error{"OTLP": &exporterRefusal, "pull": &pullExporterRefusal} {
		t.Run(name, func(t *testing.T) {
			*seam = refused
			t.Cleanup(func() { *seam = nil })
			c := loadFileYAML(t, pullFile()+"    - periodic:\n        exporter:\n          otlp_http:\n            endpoint: http://localhost:4318/v1/metrics\n", nil)
			tel := Build(c, testIdentity, Options{Logf: func(string, ...any) {}})
			defer tel.Shutdown(context.Background())
			st := tel.ConfigCheck()
			if st.OK || !st.Off || !containsString(st.Settings, envConfigFile) || containsString(st.Settings, envMetricsExport) {
				t.Fatalf("telemetry-config %+v", st)
			}
		})
	}
}
