package otelsetup

import (
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"

	"go.opentelemetry.io/contrib/otelconf"
)

// The configuration file (OTEL_CONFIG_FILE, MON-R2, MON-R8): otelconf reads it,
// substituting ${VAR} references and checking it against the configuration
// schema, and the gateway maps what it supports onto the same Config the
// environment fills, so both modes build through one path: one OTLP exporter,
// classified exports and their check, the self-metric, and a final export run
// last at stop (MON-R10, R13, R14). What the file asks for that this gateway
// does not do is reported (MON-R44) and turns off only the piece it affects.

// fileFix is the fix for a file the gateway cannot use at all.
const fileFix = "name a readable OpenTelemetry configuration file (file_format 1.0) that parses, or unset " + envConfigFile

// loadFile resolves the configuration from the file at path. It never fails:
// a file that cannot be read or parsed turns monitoring export off. getenv
// supplies a pull reader's protection (METRICS_PULL_*), which the file's
// schema has no place for.
func (c *Config) loadFile(path string, getenv func(string) string) {
	b, err := os.ReadFile(path)
	if err != nil {
		c.problem(envConfigFile, "monitoring export", fileFix)
		return
	}
	// The parse error is not reported: it can quote the file, and a file can
	// hold a header's value (MON-R10).
	f, err := otelconf.ParseYAML(b)
	if err != nil {
		c.problem(envConfigFile, "monitoring export", fileFix)
		return
	}
	if f.Disabled != nil && *f.Disabled {
		c.Mode = ModeDisabled
		c.DisabledBy = envConfigFile + "'s disabled: true"
		return
	}
	// The schema version: 1.0 and its release candidates are what the
	// gateway reads; any other is read as 1.0, which may not be what it means.
	if !strings.HasPrefix(f.FileFormat, "1.0") {
		c.Problems = append(c.Problems, Problem{Setting: "file_format", Effect: "it is read as file_format 1.0", Fix: `use file_format "1.0"`})
	}
	if f.AttributeLimits != nil {
		c.Problems = append(c.Problems, Problem{Setting: "attribute_limits", Effect: "it is ignored: the gateway's catalogue bounds every attribute", Fix: "remove it"})
	}
	var nonMetric []string
	if f.TracerProvider != nil {
		nonMetric = append(nonMetric, "tracer_provider")
	}
	if f.LoggerProvider != nil {
		nonMetric = append(nonMetric, "logger_provider")
	}
	if f.Propagator != nil {
		nonMetric = append(nonMetric, "propagator")
	}
	if len(nonMetric) > 0 {
		c.Warnings = append(c.Warnings, "gateway: WARNING: telemetry: the gateway exports metrics only; these sections of "+envConfigFile+" are ignored: "+strings.Join(nonMetric, ", "))
	}
	c.fileResource(f.Resource)
	mp := f.MeterProvider
	if mp == nil {
		return
	}
	if mp.ExemplarFilter != nil && *mp.ExemplarFilter != otelconf.ExemplarFilterAlwaysOff {
		c.Problems = append(c.Problems, Problem{Setting: "meter_provider.exemplar_filter",
			Effect: "exemplars stay off", Fix: "use always_off or leave it unset"})
	}
	c.fileViews(mp.Views)
	var env map[string]string
	var rename map[string]fileSetting
	for i, r := range mp.Readers {
		at := fmt.Sprintf("meter_provider.readers[%d]", i)
		switch {
		case r.Periodic != nil && env != nil:
			c.Problems = append(c.Problems, Problem{Setting: at, Effect: "only one periodic OTLP reader is supported, so this one is off",
				Fix: "keep one periodic reader, and send to several destinations through an OpenTelemetry Collector"})
		case r.Periodic != nil:
			env, rename = c.filePeriodic(at+".periodic", r.Periodic)
		case r.Pull != nil && c.PullRequested:
			c.Problems = append(c.Problems, Problem{Setting: at, Effect: "only one pull reader is supported, so this one is off",
				Fix: "keep one pull reader: one listener serves every scrape"})
		case r.Pull != nil:
			c.PullRequested = true
			if len(r.Pull.Producers) > 0 {
				c.Problems = append(c.Problems, Problem{Setting: at + ".pull.producers", Effect: "producers are ignored", Fix: "remove the producers"})
			}
			if r.Pull.CardinalityLimits != nil {
				c.Problems = append(c.Problems, Problem{Setting: at + ".pull.cardinality_limits",
					Effect: "the pull listener keeps the gateway's limits", Fix: "remove them; the catalogue bounds every series"})
			}
			if ex, err := filePullExporter(b, i, getenv); err != nil {
				// otelconf parsed this file, so this is not expected; the
				// error is not quoted (MON-R10).
				c.problem(at+".pull", "Prometheus pull", "give the pull reader a "+pullExporterKey+" exporter")
			} else {
				c.Pull = c.filePull(at+".pull", ex, getenv)
			}
		default:
			c.Problems = append(c.Problems, Problem{Setting: at, Effect: "a reader that is neither periodic nor pull is off", Fix: "use a periodic or a pull reader"})
		}
	}
	// The text exposition has no exponential histogram: the pull listener
	// serves a view's instruments on their explicit buckets instead (pullViews).
	for i, v := range mp.Views {
		if c.Pull != nil && v.Stream.Aggregation != nil && v.Stream.Aggregation.Base2ExponentialBucketHistogram != nil {
			c.Problems = append(c.Problems, Problem{Setting: fmt.Sprintf("meter_provider.views[%d].stream.aggregation", i),
				Effect: "the Prometheus pull listener serves these instruments on explicit buckets; OTLP push uses the exponential histogram",
				Fix:    "use explicit_bucket_histogram for one aggregation on both"})
		}
	}
	if env == nil {
		return
	}
	before := len(c.Problems)
	c.push(func(k string) string { return env[k] })
	// A problem the shared rules found names the setting it read as; here
	// that is the file's own setting.
	for i := before; i < len(c.Problems); i++ {
		p := &c.Problems[i]
		if s, ok := rename[p.Setting]; ok {
			p.Setting, p.Fix = s.path, s.fix
		}
		if s, ok := rename[p.Instead]; ok {
			p.Instead = s.path
		}
	}
}

// fileSetting is where a value the shared rules read came from in the file,
// and the fix in the file's own terms.
type fileSetting struct{ path, fix string }

// filePeriodic maps a periodic reader with an OTLP exporter onto the settings
// the shared rules read, and the file paths those settings came from. A reader
// whose exporter this gateway does not support maps onto nothing.
func (c *Config) filePeriodic(at string, r *otelconf.PeriodicMetricReader) (map[string]string, map[string]fileSetting) {
	env := map[string]string{}
	rename := map[string]fileSetting{}
	set := func(k, path, fix, v string) {
		env[k] = v
		rename[k] = fileSetting{path, fix}
	}
	// alias names a setting the file left unset, for a problem the shared
	// rules report about its absence.
	alias := func(k, path, fix string) { rename[k] = fileSetting{path, fix} }
	const msFix = "use a positive whole number of milliseconds (0, no limit, is not supported)"
	if r.Interval != nil {
		set(envExportInterval, at+".interval", msFix, strconv.Itoa(*r.Interval))
	}
	if r.Timeout != nil {
		set(envExportTimeout, at+".timeout", msFix, strconv.Itoa(*r.Timeout))
	}
	if len(r.Producers) > 0 {
		c.Problems = append(c.Problems, Problem{Setting: at + ".producers", Effect: "producers are ignored", Fix: "remove the producers"})
	}
	e := r.Exporter
	switch {
	case e.OTLPHttp != nil:
		x := e.OTLPHttp
		ex := at + ".exporter.otlp_http"
		if x.Encoding != nil && *x.Encoding != otelconf.OTLPHttpEncodingProtobuf {
			c.problem(ex+".encoding", "OTLP export", "use protobuf, the only encoding this gateway sends")
			return nil, nil
		}
		set(envMetricsProtocol, ex, "", ProtocolHTTP)
		c.fileExporter(set, ex, x.Endpoint, x.Headers, x.HeadersList, x.Timeout, x.Compression, x.TemporalityPreference, x.DefaultHistogramAggregation)
		if x.Tls != nil {
			fileTLS(set, alias, ex+".tls", x.Tls.CaFile, x.Tls.CertFile, x.Tls.KeyFile)
		}
	case e.OTLPGrpc != nil:
		x := e.OTLPGrpc
		ex := at + ".exporter.otlp_grpc"
		set(envMetricsProtocol, ex, "", ProtocolGRPC)
		c.fileExporter(set, ex, x.Endpoint, x.Headers, x.HeadersList, x.Timeout, x.Compression, x.TemporalityPreference, x.DefaultHistogramAggregation)
		if x.Tls != nil {
			fileTLS(set, alias, ex+".tls", x.Tls.CaFile, x.Tls.CertFile, x.Tls.KeyFile)
			if x.Tls.Insecure != nil && *x.Tls.Insecure {
				c.Problems = append(c.Problems, Problem{Setting: ex + ".tls.insecure", Effect: "it is ignored: the endpoint's scheme decides (http:// is plain, https:// uses TLS)", Fix: "leave it unset and use an http:// endpoint for plain gRPC"})
			}
		}
	case e.Console != nil:
		c.Problems = append(c.Problems, Problem{Setting: at + ".exporter.console", Effect: "a console exporter is not supported, so this reader is off", Fix: "use otlp_http or otlp_grpc"})
		return nil, nil
	default:
		c.Problems = append(c.Problems, Problem{Setting: at + ".exporter", Effect: "an exporter this gateway does not support, so this reader is off", Fix: "use otlp_http or otlp_grpc"})
		return nil, nil
	}
	// Only a reader that exports sets limits: a reader turned off leaves the
	// live one's alone (MON-R44).
	c.fileReaderLimits(at+".cardinality_limits", r.CardinalityLimits)
	return env, rename
}

// fileExporter maps an OTLP exporter's own settings. The endpoint is used as
// given (no /v1/metrics is added), as a metrics endpoint is in the
// environment.
func (c *Config) fileExporter(set func(k, path, fix, v string), ex string, endpoint *string, headers []otelconf.NameStringValuePair, headersList *string,
	timeout *int, compression *string, temporality *otelconf.ExporterTemporalityPreference, histogram *otelconf.ExporterDefaultHistogramAggregation) {
	if endpoint != nil {
		set(envMetricsEndpoint, ex+".endpoint", "use an http:// or https:// URL with a host", *endpoint)
	}
	// headers_list first, headers over it, as the configuration schema orders
	// them; encoded as the shared rules read headers.
	h := map[string]string{}
	if headersList != nil {
		parsed, ok := parseHeaders(*headersList)
		if !ok {
			// The shared rules report it, under the file's own setting, and
			// turn OTLP export off as for a bad header in the environment.
			set(envMetricsHeaders, ex+".headers_list", "use comma-separated key=value pairs, values percent-encoded", *headersList)
			return
		}
		h = parsed
	}
	for i, p := range headers {
		if p.Value == nil {
			// A header with no value (often a ${VAR} that is unset) is not
			// sent, as otelconf does; said, since it can be the credential.
			c.Problems = append(c.Problems, Problem{Setting: fmt.Sprintf("%s.headers[%d].value", ex, i),
				Effect: "the header is not sent", Fix: "give the header a value (an unset or empty ${VAR} leaves it null)"})
			continue
		}
		h[p.Name] = *p.Value
	}
	if len(h) > 0 {
		keys := make([]string, 0, len(h))
		for k := range h {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+url.PathEscape(h[k]))
		}
		set(envMetricsHeaders, ex+".headers", "use header names and values a request can carry", strings.Join(parts, ","))
	}
	if timeout != nil {
		set(envMetricsTimeout, ex+".timeout", "use a positive whole number of milliseconds (0, no limit, is not supported)", strconv.Itoa(*timeout))
	}
	if compression != nil {
		set(envMetricsCompression, ex+".compression", "use gzip or none", *compression)
	}
	if temporality != nil {
		set(envTemporality, ex+".temporality_preference", "use cumulative, delta or low_memory", fileTemporality(string(*temporality)))
	}
	if histogram != nil {
		set(envHistogram, ex+".default_histogram_aggregation", "use explicit_bucket_histogram or base2_exponential_bucket_histogram", string(*histogram))
	}
}

// fileTemporality spells the file's low_memory as the environment's lowmemory.
func fileTemporality(v string) string {
	if v == "low_memory" {
		return TemporalityLowMemory
	}
	return v
}

// fileTLS maps an exporter's TLS files. A client certificate without its key,
// or the reverse, is reported under the file's half that is missing.
func fileTLS(set func(k, path, fix, v string), alias func(k, path, fix string), at string, ca, cert, key *string) {
	const pairFix = "set both cert_file and key_file, or neither"
	switch {
	case cert != nil && key == nil:
		alias(envClientKey, at+".key_file", pairFix)
		alias(envMetricsClientKey, at+".key_file", pairFix)
	case key != nil && cert == nil:
		alias(envClientCert, at+".cert_file", pairFix)
		alias(envMetricsClientCert, at+".cert_file", pairFix)
	}
	if ca != nil {
		set(envMetricsCertificate, at+".ca_file", "name a readable PEM file of CA certificates", *ca)
	}
	if cert != nil {
		set(envMetricsClientCert, at+".cert_file", "name a readable PEM client certificate and its matching key", *cert)
	}
	if key != nil {
		set(envMetricsClientKey, at+".key_file", "name a readable PEM client key", *key)
	}
}

// fileReaderLimits records a reader's cardinality limits by instrument kind.
// They narrow the catalogue's limits and never raise them (MON-R18).
func (c *Config) fileReaderLimits(at string, l *otelconf.CardinalityLimits) {
	if l == nil {
		return
	}
	c.CardinalityLimits = map[string]int{}
	for kind, v := range map[string]*int{
		"default": l.Default, "counter": l.Counter, "gauge": l.Gauge, "histogram": l.Histogram,
		"observable_counter": l.ObservableCounter, "observable_gauge": l.ObservableGauge,
		"observable_up_down_counter": l.ObservableUpDownCounter, "up_down_counter": l.UpDownCounter,
	} {
		if v == nil {
			continue
		}
		if *v <= 0 {
			c.fallback(at+"."+kind, "the gateway's limit", "use a positive whole number")
			continue
		}
		c.CardinalityLimits[kind] = *v
	}
}

// fileResource maps the file's resource attributes. The gateway's own
// attributes are not the file's to set (MON-R5).
func (c *Config) fileResource(r *otelconf.Resource) {
	if r == nil {
		return
	}
	if r.SchemaUrl != nil {
		c.Problems = append(c.Problems, Problem{Setting: "resource.schema_url", Effect: "it is ignored", Fix: "remove it"})
	}
	attrs := map[string]string{}
	if r.AttributesList != nil {
		parsed, ok := parseHeaders(*r.AttributesList)
		if !ok {
			c.problem("resource.attributes_list", "resource attributes", "use comma-separated key=value pairs, values percent-encoded")
		} else {
			attrs = parsed
		}
	}
	for _, a := range r.Attributes {
		attrs[a.Name] = attrValue(a.Value)
	}
	var dropped []string
	for k := range attrs {
		if reserved(k) {
			dropped = append(dropped, k)
			delete(attrs, k)
		}
	}
	if len(dropped) > 0 {
		sort.Strings(dropped)
		c.Warnings = append(c.Warnings, "gateway: WARNING: telemetry: "+envConfigFile+"'s resource cannot set the gateway's own resource attributes; ignored: "+strings.Join(dropped, ", "))
	}
	if v, ok := attrs["service.name"]; ok {
		c.ServiceName = v
		delete(attrs, "service.name")
	}
	if len(attrs) > 0 {
		c.ResourceAttrs = attrs
	}
}

// attrValue renders a resource attribute's value as the resource carries it:
// a scalar as itself, an array as its elements comma-separated.
func attrValue(v any) string {
	switch x := v.(type) {
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			parts = append(parts, fmt.Sprint(e))
		}
		return strings.Join(parts, ",")
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}

// fileViews maps the file's views. A view is the participant's own override:
// it may rename a stream or drop attributes, never raise a limit or add an
// attribute (setup.go applies it over the gateway's own view; its limit is
// reported and ignored, so the reader's per-kind limits hold).
func (c *Config) fileViews(views []otelconf.View) {
	for i, v := range views {
		at := fmt.Sprintf("meter_provider.views[%d]", i)
		var out View
		s := v.Selector
		// What the SDK would refuse, and so drop without a word, is reported
		// here and the view left out.
		// An empty string selects nothing, as an absent criterion does.
		if (s.InstrumentName == nil || *s.InstrumentName == "") && (s.InstrumentType == nil || *s.InstrumentType == "") &&
			(s.Unit == nil || *s.Unit == "") && (s.MeterName == nil || *s.MeterName == "") &&
			(s.MeterVersion == nil || *s.MeterVersion == "") && (s.MeterSchemaUrl == nil || *s.MeterSchemaUrl == "") {
			c.Problems = append(c.Problems, Problem{Setting: at + ".selector", Effect: "a view that selects nothing is ignored", Fix: "name an instrument, an instrument type, a unit or a meter"})
			continue
		}
		if v.Stream.Name != nil && s.InstrumentName != nil && strings.ContainsAny(*s.InstrumentName, "*?") {
			c.Problems = append(c.Problems, Problem{Setting: at + ".stream.name", Effect: "this view is ignored: one name cannot rename every instrument a wildcard selects", Fix: "select one instrument by name, or leave the stream's name unset"})
			continue
		}
		if s.InstrumentName != nil {
			out.InstrumentName = *s.InstrumentName
		}
		if s.InstrumentType != nil {
			out.InstrumentKind = string(*s.InstrumentType)
		}
		if s.Unit != nil {
			out.Unit = *s.Unit
		}
		if s.MeterName != nil {
			out.MeterName = *s.MeterName
		}
		if s.MeterVersion != nil {
			out.MeterVersion = *s.MeterVersion
		}
		if s.MeterSchemaUrl != nil {
			out.MeterSchemaURL = *s.MeterSchemaUrl
		}
		st := v.Stream
		if st.Name != nil {
			out.Name = *st.Name
		}
		if st.Description != nil {
			out.Description = *st.Description
		}
		if st.AggregationCardinalityLimit != nil {
			// The SDK holds limits per instrument kind, by reader, not per
			// stream; a view's limit could only be applied to a whole kind.
			c.Problems = append(c.Problems, Problem{Setting: at + ".stream.aggregation_cardinality_limit",
				Effect: "it is ignored, and the gateway's limit for the instrument's kind applies", Fix: "set limits by instrument kind in the reader's cardinality_limits"})
		}
		if st.AttributeKeys != nil {
			out.Include = append(out.Include, st.AttributeKeys.Included...)
			out.Exclude = append(out.Exclude, st.AttributeKeys.Excluded...)
		}
		if a := st.Aggregation; a != nil {
			switch {
			case a.Default != nil:
			case a.Drop != nil:
				out.Aggregation = "drop"
			case a.Sum != nil:
				out.Aggregation = "sum"
			case a.LastValue != nil:
				out.Aggregation = "last_value"
			case a.ExplicitBucketHistogram != nil:
				x := a.ExplicitBucketHistogram
				ag := at + ".stream.aggregation.explicit_bucket_histogram"
				if x.RecordMinMax != nil {
					c.Problems = append(c.Problems, Problem{Setting: ag + ".record_min_max", Effect: "it is ignored: the gateway's histograms record no minimum or maximum", Fix: "remove it"})
				}
				if !increasing(x.Boundaries) {
					c.Problems = append(c.Problems, Problem{Setting: ag + ".boundaries", Effect: "this view is ignored", Fix: "list the boundaries in increasing order"})
					continue
				}
				// No boundaries is explicit buckets all the same, the
				// instrument's own: the gateway's for its histograms, the
				// SDK's default otherwise (gatewayView), whatever the reader's
				// default aggregation.
				out.Aggregation = HistogramExplicit
				out.Boundaries = append(out.Boundaries, x.Boundaries...)
			case a.Base2ExponentialBucketHistogram != nil:
				x := a.Base2ExponentialBucketHistogram
				ag := at + ".stream.aggregation.base2_exponential_bucket_histogram"
				for _, s := range []struct {
					name string
					set  bool
				}{{"max_scale", x.MaxScale != nil}, {"max_size", x.MaxSize != nil}, {"record_min_max", x.RecordMinMax != nil}} {
					if s.set {
						c.Problems = append(c.Problems, Problem{Setting: ag + "." + s.name, Effect: "it is ignored: the gateway's exponential histogram (160 buckets, scale up to 20) applies", Fix: "remove it"})
					}
				}
				out.Aggregation = HistogramExponential
			default:
				c.Problems = append(c.Problems, Problem{Setting: at + ".stream.aggregation", Effect: "it is ignored, and the gateway's aggregation is kept", Fix: "use default, drop, sum, last_value, explicit_bucket_histogram or base2_exponential_bucket_histogram"})
			}
		}
		c.Views = append(c.Views, out)
	}
}

// increasing reports whether bs is strictly increasing (an empty list is).
func increasing(bs []float64) bool {
	for i := 1; i < len(bs); i++ {
		if bs[i] <= bs[i-1] {
			return false
		}
	}
	return true
}
