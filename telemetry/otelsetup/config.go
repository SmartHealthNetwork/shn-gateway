package otelsetup

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http/httpguts"
)

// The settings the gateway reads (MON-R2 to MON-R5): a closed subset of the
// OpenTelemetry SDK environment variables. A value is never logged, only a
// setting's name (MON-R10).
const (
	envConfigFile     = "OTEL_CONFIG_FILE"
	envSDKDisabled    = "OTEL_SDK_DISABLED"
	envMetricsExport  = "OTEL_METRICS_EXPORTER"
	envServiceName    = "OTEL_SERVICE_NAME"
	envResourceAttrs  = "OTEL_RESOURCE_ATTRIBUTES"
	envExportInterval = "OTEL_METRIC_EXPORT_INTERVAL"
	envExportTimeout  = "OTEL_METRIC_EXPORT_TIMEOUT"
	envTemporality    = "OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE"
	envHistogram      = "OTEL_EXPORTER_OTLP_METRICS_DEFAULT_HISTOGRAM_AGGREGATION"
)

// The OTLP exporter settings come in a general form and a metrics form, the
// metrics form winning (MON-R3).
const (
	envEndpoint           = "OTEL_EXPORTER_OTLP_ENDPOINT"
	envMetricsEndpoint    = "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"
	envProtocol           = "OTEL_EXPORTER_OTLP_PROTOCOL"
	envMetricsProtocol    = "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL"
	envHeaders            = "OTEL_EXPORTER_OTLP_HEADERS"
	envMetricsHeaders     = "OTEL_EXPORTER_OTLP_METRICS_HEADERS"
	envTimeout            = "OTEL_EXPORTER_OTLP_TIMEOUT"
	envMetricsTimeout     = "OTEL_EXPORTER_OTLP_METRICS_TIMEOUT"
	envCompression        = "OTEL_EXPORTER_OTLP_COMPRESSION"
	envMetricsCompression = "OTEL_EXPORTER_OTLP_METRICS_COMPRESSION"
	envCertificate        = "OTEL_EXPORTER_OTLP_CERTIFICATE"
	envMetricsCertificate = "OTEL_EXPORTER_OTLP_METRICS_CERTIFICATE"
	envClientKey          = "OTEL_EXPORTER_OTLP_CLIENT_KEY"
	envMetricsClientKey   = "OTEL_EXPORTER_OTLP_METRICS_CLIENT_KEY"
	envClientCert         = "OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE"
	envMetricsClientCert  = "OTEL_EXPORTER_OTLP_METRICS_CLIENT_CERTIFICATE"
)

// The Prometheus pull settings (MON-R6, MON-R7): the bind is the
// specification's own pair; the protection of a bind off this machine is the
// gateway's.
const (
	envPromHost     = "OTEL_EXPORTER_PROMETHEUS_HOST"
	envPromPort     = "OTEL_EXPORTER_PROMETHEUS_PORT"
	envPullToken    = "METRICS_PULL_TOKEN"
	envPullClientCA = "METRICS_PULL_CLIENT_CA_FILE"
	envPullTLSCert  = "METRICS_PULL_TLS_CERT_FILE"
	envPullTLSKey   = "METRICS_PULL_TLS_KEY_FILE"
)

// pullSettings are the Prometheus pull settings, in the order a warning
// names them.
var pullSettings = []string{envPromHost, envPromPort, envPullToken, envPullClientCA, envPullTLSCert, envPullTLSKey}

// pullProtectionSettings are the pull listener's protection settings, which apply in
// both modes (a configuration file has no place for them).
var pullProtectionSettings = []string{envPullToken, envPullClientCA, envPullTLSCert, envPullTLSKey}

// knownSettings is every setting this package reads.
var knownSettings = []string{
	envConfigFile, envSDKDisabled, envMetricsExport, envServiceName, envResourceAttrs,
	envExportInterval, envExportTimeout, envTemporality, envHistogram,
	envEndpoint, envMetricsEndpoint, envProtocol, envMetricsProtocol, envHeaders, envMetricsHeaders,
	envTimeout, envMetricsTimeout, envCompression, envMetricsCompression, envCertificate, envMetricsCertificate,
	envClientKey, envMetricsClientKey, envClientCert, envMetricsClientCert,
	envPromHost, envPromPort, envPullToken, envPullClientCA, envPullTLSCert, envPullTLSKey,
}

// nonMetricPrefixes name the trace, log and propagator settings:
// ignored, and named in one boot warning.
var nonMetricPrefixes = []string{
	"OTEL_TRACES_", "OTEL_LOGS_", "OTEL_EXPORTER_OTLP_TRACES_", "OTEL_EXPORTER_OTLP_LOGS_",
	"OTEL_PROPAGATORS", "OTEL_BSP_", "OTEL_BLRP_", "OTEL_SPAN_", "OTEL_LOGRECORD_",
	"OTEL_EVENT_ATTRIBUTE_COUNT_LIMIT", "OTEL_LINK_ATTRIBUTE_COUNT_LIMIT", "OTEL_ATTRIBUTE_",
	"OTEL_EXPORTER_ZIPKIN_", "OTEL_EXPORTER_JAEGER_",
}

// Protocols, temporalities and histogram aggregations, as the settings spell
// them.
const (
	ProtocolHTTP = "http/protobuf"
	ProtocolGRPC = "grpc"

	TemporalityCumulative = "cumulative"
	TemporalityDelta      = "delta"
	TemporalityLowMemory  = "lowmemory"

	HistogramExplicit    = "explicit_bucket_histogram"
	HistogramExponential = "base2_exponential_bucket_histogram"
)

// Mode is how monitoring export is configured.
type Mode string

const (
	// ModeOff: no setting asks for export, so no meter provider is built
	// (MON-R1).
	ModeOff Mode = "off"
	// ModeDisabled: OTEL_SDK_DISABLED turns export off.
	ModeDisabled Mode = "disabled"
	// ModeEnvironment: the OTEL_* environment subset governs.
	ModeEnvironment Mode = "environment"
	// ModeFile: OTEL_CONFIG_FILE governs, and every other OTEL_* setting is
	// ignored (MON-R2).
	ModeFile Mode = "file"
)

// Problem is one setting that is not valid (MON-R44). It names the setting
// and the fix, never the value. A setting that decides where metrics go or
// how they authenticate turns its piece off: sending to a guessed place, or
// unauthenticated, is worse than sending nothing. A setting that only tunes
// export falls back to the specification's default, and export continues.
type Problem struct {
	Setting string
	// Piece is what it turned off: "OTLP export", "Prometheus pull",
	// "monitoring export", "resource attributes". Empty for a tuning
	// setting, which turns nothing off.
	Piece string
	// Default is the value a tuning setting falls back to; empty otherwise.
	Default string
	// Instead names the general setting that applies in place of a metrics
	// form that is not valid; empty otherwise.
	Instead string
	// Effect says what happened instead, when neither a piece turned off,
	// a default nor a general form does (an exporter entry dropped, a
	// value read as false).
	Effect string
	// Fix says what the setting accepts.
	Fix string
}

// Summary says what the gateway did about p, in a sentence that names the
// setting and never its value: the check's detail.
func (p Problem) Summary() string {
	switch {
	case p.Effect != "":
		return fmt.Sprintf("%s is not valid: %s", p.Setting, p.Effect)
	case p.Instead != "":
		return fmt.Sprintf("%s is not valid, so %s applies instead; export continues", p.Setting, p.Instead)
	case p.Default != "":
		return fmt.Sprintf("%s is not valid, so the default %s is used; export continues", p.Setting, p.Default)
	}
	return fmt.Sprintf("%s is not valid, so %s is off", p.Setting, p.Piece)
}

// Line is the loud boot line for p.
func (p Problem) Line() string {
	if p.Effect != "" {
		return fmt.Sprintf("gateway: WARNING: telemetry: %s is not valid: %s; %s", p.Setting, p.Effect, p.Fix)
	}
	if p.Instead != "" {
		return fmt.Sprintf("gateway: WARNING: telemetry: %s is not valid, so %s applies instead: %s", p.Setting, p.Instead, p.Fix)
	}
	if p.Default != "" {
		return fmt.Sprintf("gateway: WARNING: telemetry: %s is not valid, so the default %s is used: %s", p.Setting, p.Default, p.Fix)
	}
	return fmt.Sprintf("gateway: WARNING: telemetry: %s is not valid, so %s is off: %s", p.Setting, p.Piece, p.Fix)
}

// OTLP is the resolved OTLP push exporter.
type OTLP struct {
	Protocol string
	// Endpoint is the full URL: for HTTP the /v1/metrics path is already
	// appended to a general endpoint (MON-R3).
	Endpoint string
	// Headers are sent on every export. Their values are secrets: never
	// logged (MON-R10).
	Headers map[string]string
	// Timeout bounds one export request.
	Timeout time.Duration
	// Gzip selects gzip compression.
	Gzip bool
	// TLS is the client TLS configuration for an https endpoint, nil for
	// http.
	TLS *tls.Config
	// ClientCertSetting names the setting the client certificate in TLS was
	// read from (the general or the metrics form), empty without one.
	ClientCertSetting string
}

// Pull is the resolved Prometheus pull listener (MON-R6, MON-R7).
type Pull struct {
	// Addr is the bind, host:port.
	Addr string
	// Loopback is whether the bind is reachable from this machine only.
	Loopback bool
	// TLS is the listener's TLS configuration, nil for plain HTTP, which only
	// a loopback bind is served over. With a client CA it requires and
	// verifies a client certificate.
	TLS *tls.Config
	// Token is the bearer token every scrape must present, empty for none.
	// A secret: never logged (MON-R10).
	Token string
	// ClientCA is whether every scrape must present a client certificate
	// the CA file's certificates sign.
	ClientCA bool
	// hostSetting and portSetting name where the bind came from, for the
	// problems and lines about it: the environment's settings, or a
	// configuration file's paths. Empty means the environment's.
	hostSetting, portSetting string
}

// settings are the names of the settings the bind came from.
func (p Pull) settings() (host, port string) {
	host, port = p.hostSetting, p.portSetting
	if host == "" {
		host = envPromHost
	}
	if port == "" {
		port = envPromPort
	}
	return host, port
}

// Config is the resolved monitoring configuration.
type Config struct {
	Mode Mode
	// Set is whether any monitoring setting is present; when not, nothing
	// about monitoring changes, not even /internal/checks (MON-R1).
	Set bool
	// OTLP is the push exporter, nil when off.
	OTLP *OTLP
	// PullRequested is whether OTEL_METRICS_EXPORTER names prometheus.
	PullRequested bool
	// Pull is the pull listener, nil when off.
	Pull          *Pull
	Interval      time.Duration
	ExportTimeout time.Duration
	Temporality   string
	Histogram     string
	ServiceName   string
	ResourceAttrs map[string]string
	// DisabledBy names the setting that turned export off in ModeDisabled:
	// OTEL_SDK_DISABLED, or the configuration file's disabled key.
	DisabledBy string
	// Views are the configuration file's views, applied over the gateway's
	// own (MON-R8); none in environment mode.
	Views []View
	// CardinalityLimits narrow the catalogue's per-kind limits by instrument
	// kind (the configuration file's reader limits); a limit is never raised
	// (MON-R18).
	CardinalityLimits map[string]int
	Problems          []Problem
	Warnings          []string
}

// View is one configuration-file view (MON-R8): a selector and the stream
// fields it overrides. Empty fields select or override nothing.
type View struct {
	// Selector.
	InstrumentName, InstrumentKind, Unit, MeterName, MeterVersion, MeterSchemaURL string
	// Stream.
	Name, Description string
	// Aggregation is one of the aggregation names the file spells, empty to
	// keep the gateway's.
	Aggregation string
	// Boundaries are an explicit-bucket aggregation's bounds, when it names
	// them.
	Boundaries []float64
	// Include and Exclude filter the attribute keys a stream keeps. A filter
	// only drops attributes: it never adds one.
	Include, Exclude []string
}

// Defaults (MON-R4).
const (
	defaultInterval      = 60 * time.Second
	defaultExportTimeout = 30 * time.Second
	defaultOTLPTimeout   = 10 * time.Second
)

// Load resolves the configuration from getenv. names are the OTEL_* setting
// names set (non-empty) in the process environment: only named, in the
// warnings of the ones the gateway does not read; every value is read
// through getenv. It never fails: an invalid value is a Problem that turns
// its piece off (MON-R44).
func Load(getenv func(string) string, names []string) Config {
	c := Config{Mode: ModeOff, Interval: defaultInterval, ExportTimeout: defaultExportTimeout,
		Temporality: TemporalityCumulative, Histogram: HistogramExplicit}
	for _, k := range knownSettings {
		if getenv(k) != "" {
			c.Set = true
			break
		}
	}
	var nonMetric, unsupported []string
	for _, n := range dedupe(names) {
		if slices.Contains(knownSettings, n) {
			continue
		}
		c.Set = true
		if n == deprecatedRuntimeEnv {
			continue // the runtime library's, a no-op here: app's noEffectSettings warns
		} else if hasAnyPrefix(n, nonMetricPrefixes) {
			nonMetric = append(nonMetric, n)
		} else if strings.HasPrefix(n, "OTEL_") {
			unsupported = append(unsupported, n)
		}
	}
	if len(nonMetric) > 0 {
		c.Warnings = append(c.Warnings, "gateway: WARNING: telemetry: the gateway exports metrics only; these trace, log and propagator settings are ignored: "+strings.Join(nonMetric, ", "))
	}
	if len(unsupported) > 0 {
		c.Warnings = append(c.Warnings, "gateway: WARNING: telemetry: these OpenTelemetry settings are not supported and are ignored: "+strings.Join(unsupported, ", "))
	}

	// 1. The configuration file governs when set (MON-R2): every other OTEL_*
	// setting is ignored, OTEL_SDK_DISABLED included, and a file that cannot
	// be read turns export off rather than falling back to the environment the
	// file says to ignore (MON-R8).
	if path := getenv(envConfigFile); path != "" {
		c.Mode = ModeFile
		c.loadFile(path, getenv)
		if set := setNames(getenv, pullProtectionSettings); !c.PullRequested && len(set) > 0 {
			c.Warnings = append(c.Warnings, "gateway: WARNING: telemetry: these Prometheus pull settings take effect only when "+envConfigFile+" has a pull reader, and are ignored: "+strings.Join(set, ", "))
		}
		return c
	}
	// 2. OTEL_SDK_DISABLED. Any value but true reads as false, as the
	// specification says; one that is not a boolean is still reported.
	switch v := strings.ToLower(strings.TrimSpace(getenv(envSDKDisabled))); v {
	case "", "false":
	case "true":
		c.Mode = ModeDisabled
		c.DisabledBy = envSDKDisabled + "=true"
		return c
	default:
		c.Problems = append(c.Problems, Problem{Setting: envSDKDisabled, Effect: "it reads as false, so export is not disabled", Fix: "use true or false"})
	}
	// 3. OTEL_METRICS_EXPORTER: unset means none, the one deviation from the
	// specification's otlp default (MON-R2).
	otlp, unknown := false, false
	for _, e := range strings.Split(getenv(envMetricsExport), ",") {
		switch e = strings.ToLower(strings.TrimSpace(e)); e {
		case "", "none":
		case "otlp":
			otlp = true
		case "prometheus":
			c.PullRequested = true
		default:
			unknown = true
		}
	}
	if unknown {
		c.Problems = append(c.Problems, Problem{Setting: envMetricsExport,
			Effect: "an entry this gateway does not recognize is dropped, and the entries it recognizes run",
			Fix:    "use otlp, prometheus, both (comma-separated) or none"})
	}
	if c.PullRequested {
		c.Pull = c.resolvePull(getenv(envPromHost), getenv(envPromPort), envPromHost, envPromPort, getenv)
	} else if set := setNames(getenv, pullSettings); len(set) > 0 {
		c.Warnings = append(c.Warnings, "gateway: WARNING: telemetry: these Prometheus pull settings take effect only when OTEL_METRICS_EXPORTER names prometheus, and are ignored: "+strings.Join(set, ", "))
	}
	c.ServiceName = getenv(envServiceName)
	c.ResourceAttrs = c.resourceAttrs(getenv)
	if c.Pull != nil {
		c.Mode = ModeEnvironment
	}
	if !otlp {
		return c
	}
	c.Mode = ModeEnvironment
	c.push(getenv)
	return c
}

// push resolves the OTLP push exporter and its tuning from getenv: the
// environment's settings, or the ones a configuration file's reader maps
// onto (file.go), so both modes share one set of rules.
func (c *Config) push(getenv func(string) string) {
	// The tuning settings: one that is not valid keeps its default.
	if d, set, valid := millis(getenv(envExportInterval)); set && !valid {
		c.fallback(envExportInterval, "60000", "use a positive whole number of milliseconds")
	} else if set {
		c.Interval = d
	}
	if d, set, valid := millis(getenv(envExportTimeout)); set && !valid {
		c.fallback(envExportTimeout, "30000", "use a positive whole number of milliseconds")
	} else if set {
		c.ExportTimeout = d
	}
	switch v := strings.ToLower(strings.TrimSpace(getenv(envTemporality))); v {
	case "":
	case TemporalityCumulative, TemporalityDelta, TemporalityLowMemory:
		c.Temporality = v
	default:
		c.fallback(envTemporality, TemporalityCumulative, "use cumulative, delta or lowmemory")
	}
	switch v := strings.ToLower(strings.TrimSpace(getenv(envHistogram))); v {
	case "":
	case HistogramExplicit, HistogramExponential:
		c.Histogram = v
	default:
		c.fallback(envHistogram, HistogramExplicit, "use explicit_bucket_histogram or base2_exponential_bucket_histogram")
	}
	if o, valid := c.otlp(getenv); valid {
		c.OTLP = o
	}
}

func (c *Config) problem(setting, piece, fix string) {
	c.Problems = append(c.Problems, Problem{Setting: setting, Piece: piece, Fix: fix})
}

// fallback reports a tuning setting that is not valid and keeps its default.
func (c *Config) fallback(setting, def, fix string) {
	c.Problems = append(c.Problems, Problem{Setting: setting, Default: def, Fix: fix})
}

// otlpValue is the metrics form of an OTLP setting when set, else the
// general form, with the name of the one it read.
func otlpValue(getenv func(string) string, general, metrics string) (name, value string) {
	if v := getenv(metrics); v != "" {
		return metrics, v
	}
	return general, getenv(general)
}

// resolve reads an OTLP setting's metrics form, else its general form, as
// the specification orders them (MON-R3), with valid deciding each value.
// A metrics form that is not valid is treated as unset, so a valid general
// form applies; a form that is not valid is reported, and ok is false only
// when no valid form remains (MON-R44). set is whether a valid form gave the
// value. name is the setting that gave it, or the one at fault.
func (c *Config) resolve(getenv func(string) string, general, metrics, fix string, valid func(string) bool) (name, value string, set, ok bool) {
	m, g := strings.TrimSpace(getenv(metrics)), strings.TrimSpace(getenv(general))
	gValid := g != "" && valid(g)
	if m != "" {
		if valid(m) {
			return metrics, m, true, true
		}
		if gValid {
			c.Problems = append(c.Problems, Problem{Setting: metrics, Instead: general, Fix: fix})
			return general, g, true, true
		}
		c.problem(metrics, "OTLP export", fix)
		if g != "" {
			c.problem(general, "OTLP export", fix)
		}
		return metrics, "", false, false
	}
	if g == "" {
		return general, "", false, true
	}
	if !gValid {
		c.problem(general, "OTLP export", fix)
		return general, "", false, false
	}
	return general, g, true, true
}

// otlp resolves the OTLP exporter; valid is false when a setting turned it
// off, each such setting a Problem.
func (c *Config) otlp(getenv func(string) string) (*OTLP, bool) {
	o := &OTLP{Protocol: ProtocolHTTP, Timeout: defaultOTLPTimeout, Headers: map[string]string{}}
	_, protocol, set, ok := c.resolve(getenv, envProtocol, envMetricsProtocol, "use http/protobuf or grpc",
		func(v string) bool { v = strings.ToLower(v); return v == ProtocolHTTP || v == ProtocolGRPC })
	if set {
		o.Protocol = strings.ToLower(protocol)
	}
	endpoint, endpointOK := c.endpoint(getenv, o.Protocol)
	ok = ok && endpointOK
	o.Endpoint = endpoint
	_, headers, set, headersOK := c.resolve(getenv, envHeaders, envMetricsHeaders, "use comma-separated key=value pairs, values percent-encoded",
		func(v string) bool { h, valid := parseHeaders(v); return valid && validHeaders(h, o.Protocol) })
	ok = ok && headersOK
	if set {
		o.Headers, _ = parseHeaders(headers)
	}
	// The request timeout and the compression only tune export: with no
	// valid form, the default applies.
	if v, set := c.resolveTuning(getenv, envTimeout, envMetricsTimeout, "10000", "use a positive whole number of milliseconds",
		func(v string) bool { _, _, valid := millis(v); return valid }); set {
		o.Timeout, _, _ = millis(v)
	}
	if v, set := c.resolveTuning(getenv, envCompression, envMetricsCompression, "none", "use gzip or none",
		func(v string) bool { v = strings.ToLower(v); return v == "gzip" || v == "none" }); set {
		o.Gzip = strings.EqualFold(v, "gzip")
	}
	tlsCfg, tlsSet, tlsOK, certSetting := c.clientTLS(getenv)
	ok = ok && tlsOK
	if u, perr := url.Parse(endpoint); perr == nil && endpoint != "" {
		if u.Scheme == "https" {
			o.TLS = tlsCfg
			if len(tlsCfg.Certificates) > 0 {
				o.ClientCertSetting = certSetting
			}
		} else if tlsSet && tlsOK {
			c.problem(tlsSetting(getenv), "OTLP export", "certificate settings need an https:// endpoint")
			ok = false
		} else if !isLoopback(u.Hostname()) {
			// MON-R11: allowed, and said.
			c.Warnings = append(c.Warnings, "gateway: WARNING: telemetry: the OTLP endpoint is plain http:// to a host other than this machine; metrics and any export headers travel unencrypted")
		}
	}
	return o, ok
}

// resolveTuning is resolve for a tuning setting: a form that is not valid
// is reported but turns nothing off, and with no valid form the default def
// applies (MON-R44). set is whether a valid form gave the value.
func (c *Config) resolveTuning(getenv func(string) string, general, metrics, def, fix string, valid func(string) bool) (value string, set bool) {
	m, g := strings.TrimSpace(getenv(metrics)), strings.TrimSpace(getenv(general))
	gValid := g != "" && valid(g)
	if m != "" {
		if valid(m) {
			return m, true
		}
		if gValid {
			c.Problems = append(c.Problems, Problem{Setting: metrics, Instead: general, Fix: fix})
			return g, true
		}
		c.fallback(metrics, def, fix)
	}
	if g != "" && !gValid {
		c.fallback(general, def, fix)
	}
	if m == "" && gValid {
		return g, true
	}
	return "", false
}

// endpoint resolves the endpoint URL. A general HTTP endpoint gets
// /v1/metrics appended; a metrics endpoint is used as given (MON-R3). With
// neither set it is the specification's localhost default; with one set and
// none valid, ok is false: OTLP export is off rather than sent to a guessed
// place (MON-R44).
func (c *Config) endpoint(getenv func(string) string, protocol string) (endpoint string, ok bool) {
	const fix = "use an http:// or https:// URL with a host"
	name, v, set, ok := c.resolve(getenv, envEndpoint, envMetricsEndpoint, fix, validEndpoint)
	switch {
	case !ok:
		return "", false
	case !set && protocol == ProtocolGRPC:
		return "http://localhost:4317", true
	case !set:
		return "http://localhost:4318/v1/metrics", true
	case name == envMetricsEndpoint || protocol == ProtocolGRPC:
		return v, true
	}
	joined, err := url.JoinPath(v, "v1/metrics")
	if err != nil {
		c.problem(envEndpoint, "OTLP export", fix)
		return "", false
	}
	return joined, true
}

func validEndpoint(v string) bool {
	u, err := url.Parse(v)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil
}

// clientTLS reads the CA certificate and the client key pair. set is whether
// any of them is set; ok is false when one is invalid; certSetting names the
// setting the client certificate was read from.
func (c *Config) clientTLS(getenv func(string) string) (cfg *tls.Config, set, ok bool, certSetting string) {
	cfg = &tls.Config{MinVersion: tls.VersionTLS12}
	readablePEM := func(path string) bool {
		pem, err := os.ReadFile(path)
		return err == nil && x509.NewCertPool().AppendCertsFromPEM(pem)
	}
	readable := func(path string) bool { _, err := os.Stat(path); return err == nil }
	_, caPath, caSet, caOK := c.resolve(getenv, envCertificate, envMetricsCertificate, "name a readable PEM file of CA certificates", readablePEM)
	certName, certPath, certSet, certOK := c.resolve(getenv, envClientCert, envMetricsClientCert, "name a readable PEM client certificate", readable)
	keyName, keyPath, keySet, keyOK := c.resolve(getenv, envClientKey, envMetricsClientKey, "name a readable PEM client key", readable)
	ok = caOK && certOK && keyOK
	set = caSet || certSet || keySet || !ok
	if caSet {
		pool := x509.NewCertPool()
		pem, _ := os.ReadFile(caPath)
		pool.AppendCertsFromPEM(pem)
		cfg.RootCAs = pool
	}
	switch {
	case !certOK || !keyOK:
	case certSet != keySet:
		missing := certName
		if !keySet {
			missing = keyName
		}
		c.problem(missing, "OTLP export", "set both the client certificate and the client key, or neither")
		ok = false
	case certSet:
		pair, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			c.problem(certName, "OTLP export", "name a readable PEM client certificate and its matching key ("+keyName+")")
			ok = false
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	return cfg, set, ok, certName
}

// tlsSetting names the first certificate setting that is set.
func tlsSetting(getenv func(string) string) string {
	if name, v := otlpValue(getenv, envCertificate, envMetricsCertificate); v != "" {
		return name
	}
	if name, v := otlpValue(getenv, envClientCert, envMetricsClientCert); v != "" {
		return name
	}
	name, _ := otlpValue(getenv, envClientKey, envMetricsClientKey)
	return name
}

// defaultPullPort is the specification's default Prometheus port; the
// default host is localhost (MON-R6).
const defaultPullPort = 9464

// resolvePull resolves the Prometheus pull listener; nil when a setting
// turned it off, each such setting a Problem (MON-R44). The bind's host and
// port come from the environment or a configuration file, named by
// hostSetting and portSetting; its protection comes from METRICS_PULL_*
// through getenv in both modes, so one rule secures every bind. A bind other
// than this machine's is served only over TLS and only with a bearer token,
// a client CA or both (MON-R7): an unprotected one serves nothing, rather
// than metrics to anyone who can reach the port.
func (c *Config) resolvePull(hostValue, portValue, hostSetting, portSetting string, getenv func(string) string) *Pull {
	const piece = "Prometheus pull"
	ok := true
	host := strings.TrimSpace(hostValue)
	switch {
	case host == "":
		host = "localhost"
	case !validBindHost(host):
		c.problem(hostSetting, piece, "use a host name or an IP address, without a port or brackets")
		ok = false
	}
	port := defaultPullPort
	if v := strings.TrimSpace(portValue); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			c.problem(portSetting, piece, "use a port number from 1 to 65535")
			ok = false
		} else {
			port = n
		}
	}
	p := &Pull{Addr: net.JoinHostPort(host, strconv.Itoa(port)), Loopback: isLoopback(host)}
	if hostSetting != envPromHost || portSetting != envPromPort {
		p.hostSetting, p.portSetting = hostSetting, portSetting
	}

	if v := getenv(envPullToken); v != "" {
		if !validBearerToken(v) {
			c.problem(envPullToken, piece, "use a bearer token of letters, digits and -._~+/ (optionally ending in =)")
			ok = false
		}
		p.Token = v
	}
	certPath, keyPath := strings.TrimSpace(getenv(envPullTLSCert)), strings.TrimSpace(getenv(envPullTLSKey))
	switch {
	case certPath == "" && keyPath == "":
	case certPath == "" || keyPath == "":
		missing := envPullTLSCert
		if keyPath == "" {
			missing = envPullTLSKey
		}
		c.problem(missing, piece, "set both the TLS certificate and the TLS key, or neither")
		ok = false
	default:
		pair, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			c.problem(envPullTLSCert, piece, "name a readable PEM certificate and its matching key ("+envPullTLSKey+")")
			ok = false
			break
		}
		p.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}
	}
	if caPath := strings.TrimSpace(getenv(envPullClientCA)); caPath != "" {
		pool := x509.NewCertPool()
		pem, err := os.ReadFile(caPath)
		switch {
		case err != nil || !pool.AppendCertsFromPEM(pem):
			c.problem(envPullClientCA, piece, "name a readable PEM file of CA certificates")
			ok = false
		case certPath == "" && keyPath == "":
			c.problem(envPullClientCA, piece, "client certificates need TLS: set METRICS_PULL_TLS_CERT_FILE and METRICS_PULL_TLS_KEY_FILE")
			ok = false
		case p.TLS != nil:
			p.TLS.ClientCAs = pool
			p.TLS.ClientAuth = tls.RequireAndVerifyClientCert
			p.ClientCA = true
		}
	}
	if ok && !p.Loopback && (p.TLS == nil || p.Token == "" && !p.ClientCA) {
		c.problem(hostSetting, piece, "a bind other than this machine needs TLS (METRICS_PULL_TLS_CERT_FILE and METRICS_PULL_TLS_KEY_FILE) with METRICS_PULL_TOKEN, METRICS_PULL_CLIENT_CA_FILE or both; or bind to localhost")
		ok = false
	}
	if !ok {
		return nil
	}
	return p
}

// alphanumerics are the ASCII letters and digits.
const alphanumerics = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// onlyRunesOf reports whether every rune of s is in set.
func onlyRunesOf(s, set string) bool {
	for _, r := range s {
		if !strings.ContainsRune(set, r) {
			return false
		}
	}
	return true
}

// validBindHost reports whether host can name a bind: an IP address, or a
// host name of letters, digits, dots and hyphens.
func validBindHost(host string) bool {
	if net.ParseIP(host) != nil {
		return true
	}
	return len(host) <= 253 && onlyRunesOf(host, alphanumerics+".-")
}

// validBearerToken reports whether v is a bearer token's token68 (RFC 6750
// §2.1), which a scrape can send as given.
func validBearerToken(v string) bool {
	body := strings.TrimRight(v, "=")
	return body != "" && onlyRunesOf(body, alphanumerics+"-._~+/")
}

// setNames are the names of the settings set (non-empty), in order.
func setNames(getenv func(string) string, settings []string) []string {
	var out []string
	for _, s := range settings {
		if getenv(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

// reserved reports the resource attributes the gateway sets itself
// and OTEL_RESOURCE_ATTRIBUTES cannot override (MON-R5).
func reserved(key string) bool {
	return key == "service.version" || strings.HasPrefix(key, "shn.")
}

// resourceAttrs parses OTEL_RESOURCE_ATTRIBUTES. An invalid value drops all
// of them (MON-R44); a reserved key is dropped with a warning.
func (c *Config) resourceAttrs(getenv func(string) string) map[string]string {
	v := getenv(envResourceAttrs)
	if v == "" {
		return nil
	}
	attrs, ok := parseHeaders(v)
	if !ok {
		c.problem(envResourceAttrs, "resource attributes", "use comma-separated key=value pairs, values percent-encoded")
		return nil
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
		c.Warnings = append(c.Warnings, "gateway: WARNING: telemetry: "+envResourceAttrs+" cannot set the gateway's own resource attributes; ignored: "+strings.Join(dropped, ", "))
	}
	return attrs
}

// validHeaders reports whether every header is one the protocol can carry:
// for HTTP a token name and a value with no control character; for gRPC
// metadata, which the exporter lowercases, a name of [0-9a-z-_.] and a
// printable ASCII value (a -bin name carries any bytes). A header that is not
// is refused here, rather than failing every export.
func validHeaders(h map[string]string, protocol string) bool {
	for k, v := range h {
		if !httpguts.ValidHeaderFieldName(k) || !httpguts.ValidHeaderFieldValue(v) {
			return false
		}
		if protocol != ProtocolGRPC {
			continue
		}
		k = strings.ToLower(k)
		for _, r := range k {
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r == '-' || r == '_' || r == '.') {
				return false
			}
		}
		if strings.HasSuffix(k, "-bin") {
			continue
		}
		for i := 0; i < len(v); i++ {
			if v[i] < 0x20 || v[i] > 0x7e {
				return false
			}
		}
	}
	return true
}

// parseHeaders parses the comma-separated key=value list the OTLP headers
// and resource attributes settings share, values percent-decoded.
func parseHeaders(v string) (map[string]string, bool) {
	out := map[string]string{}
	for _, pair := range strings.Split(v, ",") {
		if strings.TrimSpace(pair) == "" {
			continue
		}
		k, val, found := strings.Cut(pair, "=")
		k = strings.TrimSpace(k)
		if !found || k == "" {
			return nil, false
		}
		dec, err := url.PathUnescape(strings.TrimSpace(val))
		if err != nil {
			return nil, false
		}
		out[k] = dec
	}
	return out, true
}

// millis parses a positive whole number of milliseconds.
func millis(v string) (d time.Duration, set, valid bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false, true
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 || n > int64(time.Duration(1<<63-1)/time.Millisecond) {
		return 0, true, false
	}
	return time.Duration(n) * time.Millisecond, true, true
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func dedupe(names []string) []string {
	out := append([]string(nil), names...)
	sort.Strings(out)
	return slices.Compact(out)
}
