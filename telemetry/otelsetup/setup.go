// Package otelsetup builds the gateway's OpenTelemetry meter provider from
// its configuration, and is the only package of the gateway that imports
// OpenTelemetry (TestOnlyOtelsetupImportsOpenTelemetry). The provider feeds
// a telemetry.Sink, so every instrument it exports is recorded through the
// telemetry registry (MON-R16).
//
// Nothing here stops a gateway from booting (MON-R44): a setting that is not
// valid turns off the piece it affects, logs a boot line naming the setting,
// never its value, and fails the telemetry-config check.
package otelsetup

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
	"github.com/SmartHealthNetwork/shn-gateway/internal/serviceclient"
	"github.com/SmartHealthNetwork/shn-gateway/telemetry"
	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/exemplar"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// ShutdownBudget bounds the final export at stop (MON-R14).
const ShutdownBudget = 5 * time.Second

// shutdownBudget is ShutdownBudget; a test shortens it.
var shutdownBudget = ShutdownBudget

// scopeName is the instrumentation scope every gateway instrument is
// exported under.
const scopeName = "github.com/SmartHealthNetwork/shn-gateway/telemetry"

// Telemetry is the built monitoring export: a meter provider and its sink
// when an exporter is on, and the configuration's state for the checks.
type Telemetry struct {
	cfg        Config
	instanceID string
	provider   *sdkmetric.MeterProvider
	reader     *sdkmetric.ManualReader
	sink       *sink
	exp        *exporter
	buildFault bool
	pull       *pullServer
	pullFault  bool
	lines      []string
	logf       func(string, ...any)

	exportMu sync.Mutex // one collection and export at a time
	stopOnce sync.Once
}

// Options are Build's dependencies, each defaulted when zero.
type Options struct {
	// Logf logs a line (log.Printf).
	Logf func(string, ...any)
	// Now is the clock (time.Now).
	Now func() time.Time
	// Instruments is the catalogue to export (telemetry.Instruments()).
	Instruments []*telemetry.Instrument
	// Certs records the certificates the exporters meet (MON-R25): the
	// collector's, under otlp, and the client and pull listener
	// certificates, as the gateway's own. Nil records none.
	Certs *endpointcert.Recorder
}

// Build builds the monitoring export c describes. It never fails: with no
// exporter on it builds no meter provider and opens nothing (MON-R1). It
// starts no goroutine, dials nothing and binds nothing: Run exports,
// ServePull serves the pull listener, and Shutdown runs the final export.
func Build(c Config, id Identity, opts Options) *Telemetry {
	if opts.Logf == nil {
		opts.Logf = log.Printf
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Instruments == nil {
		opts.Instruments = telemetry.Instruments()
	}
	t := &Telemetry{cfg: c, logf: opts.Logf, instanceID: InstanceID(id.Hostname)}
	if v, set := c.ResourceAttrs["service.instance.id"]; set {
		t.instanceID = v
	}
	for _, p := range c.Problems {
		t.lines = append(t.lines, p.Line())
	}
	t.lines = append(t.lines, c.Warnings...)
	if c.OTLP == nil && c.Pull == nil {
		switch {
		case c.Mode == ModeDisabled && len(c.Problems) == 0:
			t.lines = append(t.lines, "gateway: telemetry: "+c.DisabledBy+", so nothing is exported")
		case c.Mode == ModeFile && !c.PullRequested && len(c.Problems) == 0:
			t.lines = append(t.lines, "gateway: telemetry: "+envConfigFile+" configures no OTLP reader, so nothing is exported")
		case c.Mode == ModeOff && c.Set && !c.PullRequested && len(c.Problems) == 0:
			t.lines = append(t.lines, "gateway: telemetry: no exporter selected (OTEL_METRICS_EXPORTER is unset or none), so nothing is exported")
		}
		return t
	}
	installGlobalHandlers()
	instance := t.instanceID
	if c.OTLP != nil {
		if err := t.build(id, opts); err != nil {
			// Validated settings that the SDK still refuses: the piece is
			// off, and the error is not logged, since it can carry the
			// endpoint or a header (MON-R10).
			t.buildFault = true
			t.provider, t.sink, t.exp = nil, nil, nil
			t.lines = append(t.lines, "gateway: WARNING: telemetry: the OTLP exporter could not be built from these settings, so OTLP export is off")
		} else {
			o := c.OTLP
			host := ""
			if u, err := url.Parse(o.Endpoint); err == nil {
				host = u.Host
			}
			t.lines = append(t.lines, fmt.Sprintf("gateway: telemetry: OTLP export on (%s): %s to %s every %s (timeout %s), %s temporality, %s histograms, service.instance.id %s",
				c.Mode, o.Protocol, host, c.Interval, c.ExportTimeout, c.Temporality, c.Histogram, instance))
		}
	}
	if c.Pull != nil {
		if err := t.buildPull(id, opts); err != nil {
			t.pullFault = true
			t.pull = nil
			t.lines = append(t.lines, "gateway: WARNING: telemetry: the Prometheus exporter could not be built, so Prometheus pull is off")
		} else {
			t.lines = append(t.lines, fmt.Sprintf("gateway: telemetry: Prometheus pull on: GET /metrics at %s (%s), service.instance.id %s",
				c.Pull.Addr, pullProtection(*c.Pull), instance))
		}
	}
	return t
}

// pullProtection says how the pull listener is protected, never with a
// secret.
func pullProtection(p Pull) string {
	var parts []string
	if p.TLS == nil {
		parts = append(parts, "plain HTTP, this machine only")
	} else {
		parts = append(parts, "TLS")
	}
	if p.Token != "" {
		parts = append(parts, "bearer token")
	}
	if p.ClientCA {
		parts = append(parts, "client certificate")
	}
	return strings.Join(parts, ", ")
}

// exporterRefusal, when set, is returned as the SDK's refusal of a
// validated exporter: a test seam, nil in every build, for the path no
// validated setting reaches.
var exporterRefusal error

func (t *Telemetry) build(id Identity, opts Options) error {
	c, o := t.cfg, t.cfg.OTLP
	pr := &probe{}
	var inner sdkmetric.Exporter
	var err error
	temporality := temporalitySelector(c.Temporality)
	aggregation := aggregationSelector(c.Histogram)
	switch o.Protocol {
	case ProtocolGRPC:
		gopts := []otlpmetricgrpc.Option{
			otlpmetricgrpc.WithEndpointURL(o.Endpoint),
			otlpmetricgrpc.WithHeaders(o.Headers),
			otlpmetricgrpc.WithTimeout(o.Timeout),
			otlpmetricgrpc.WithTemporalitySelector(temporality),
			otlpmetricgrpc.WithAggregationSelector(aggregation),
			otlpmetricgrpc.WithDialOption(grpc.WithChainUnaryInterceptor(pr.unaryInterceptor, peerCertificate(opts.Certs))),
		}
		if o.Gzip {
			gopts = append(gopts, otlpmetricgrpc.WithCompressor("gzip"))
		}
		if o.TLS != nil {
			gopts = append(gopts, otlpmetricgrpc.WithTLSCredentials(credentials.NewTLS(o.TLS)))
		}
		inner, err = otlpmetricgrpc.New(context.Background(), gopts...)
	default:
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = o.TLS
		compression := otlpmetrichttp.NoCompression
		if o.Gzip {
			compression = otlpmetrichttp.GzipCompression
		}
		inner, err = otlpmetrichttp.New(context.Background(),
			otlpmetrichttp.WithEndpointURL(o.Endpoint),
			otlpmetrichttp.WithHeaders(o.Headers),
			otlpmetrichttp.WithCompression(compression),
			otlpmetrichttp.WithTemporalitySelector(temporality),
			otlpmetrichttp.WithAggregationSelector(aggregation),
			// The client carries the per-request timeout and records each
			// request's answer for the export's classification.
			// The collector's certificate is recorded from each answer, over
			// the exporter's own transport (MON-R25), which follows no
			// redirect (serviceclient).
			otlpmetrichttp.WithHTTPClient(&http.Client{Timeout: o.Timeout, Transport: roundTripper{next: opts.Certs.Wrap(endpointcert.Of(endpointcert.OTLP), serviceclient.Transport(transport)), probe: pr}}),
		)
	}
	if err == nil && exporterRefusal != nil {
		err = exporterRefusal
	}
	if err != nil {
		return err
	}
	if o.TLS != nil && len(o.TLS.Certificates) > 0 {
		opts.Certs.RecordOwn(endpointcert.OTLP, o.ClientCertSetting, &o.TLS.Certificates[0])
	}
	t.exp = &exporter{Exporter: inner, probe: pr, now: opts.Now, logf: opts.Logf}
	// A manual reader: the export loop is Run's, started with the gateway's
	// other workers, and the final export is Shutdown's, run last at stop.
	t.reader = sdkmetric.NewManualReader(
		sdkmetric.WithTemporalitySelector(temporality),
		sdkmetric.WithAggregationSelector(aggregation),
		sdkmetric.WithCardinalityLimitSelector(cardinalityLimits(opts.Instruments, c.CardinalityLimits)))
	kvs := []attribute.KeyValue{}
	for _, kv := range resourceAttributes(c, id, t.instanceID) {
		kvs = append(kvs, attribute.String(kv[0], kv[1]))
	}
	t.provider = sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(resource.NewSchemaless(kvs...)),
		sdkmetric.WithReader(t.reader),
		sdkmetric.WithView(gatewayView(opts.Instruments, c.Histogram, c.Views)),
		sdkmetric.WithExemplarFilter(exemplar.AlwaysOffFilter),
		// Each instrument kind's limit comes from the reader's selector; a
		// kind the catalogue does not use keeps this one, or a configuration
		// file's lower limit for it.
		sdkmetric.WithCardinalityLimit(providerCardinalityLimit),
	)
	s, err := newSink(t.provider.Meter(scopeName, metric.WithInstrumentationVersion(id.Version)), opts.Instruments)
	if err != nil {
		_ = t.provider.Shutdown(context.Background())
		return err
	}
	t.sink = s
	startRuntimeMetrics(t.provider, id.Version, opts.Now, opts.Logf)
	return nil
}

// Sink is the OpenTelemetry sink, nil when nothing is exported: the push
// provider's, the pull listener's, or both.
func (t *Telemetry) Sink() telemetry.Sink {
	if t == nil {
		return nil
	}
	var out sinks
	if t.sink != nil {
		out = append(out, t.sink)
	}
	if t.pull != nil {
		out = append(out, t.pull)
	}
	switch len(out) {
	case 0:
		return nil
	case 1:
		return out[0]
	}
	return out
}

// sinks records each measurement on every sink, a panic in one affecting
// neither the caller nor the others, as the registry treats its own sinks
// (MON-R12).
type sinks []telemetry.Sink

func (s sinks) Record(m telemetry.Measurement) {
	for _, x := range s {
		recordIsolated(x, m)
	}
}

// Every sink Sink returns takes a snapshot whole: one left to Record would
// drop it (sink.Record has no Snapshot instruments).
var (
	_ telemetry.SnapshotSink = (*sink)(nil)
	_ telemetry.SnapshotSink = (*pullServer)(nil)
	_ telemetry.SnapshotSink = sinks(nil)
)

// RecordSnapshot implements telemetry.SnapshotSink: each sink takes a copy
// of the whole snapshot, isolated as Record is.
func (s sinks) RecordSnapshot(i *telemetry.Instrument, series []telemetry.Measurement) {
	for _, x := range s {
		snapshotIsolated(x, i, slices.Clone(series))
	}
}

func snapshotIsolated(s telemetry.Sink, i *telemetry.Instrument, series []telemetry.Measurement) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("gateway: telemetry: a metrics sink panicked recording %s: %q", i.Name, fmt.Sprint(p))
		}
	}()
	if ss, ok := s.(telemetry.SnapshotSink); ok {
		ss.RecordSnapshot(i, series)
	}
}

func recordIsolated(s telemetry.Sink, m telemetry.Measurement) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("gateway: telemetry: a metrics sink panicked recording %s: %q", m.Instrument.Name, fmt.Sprint(p))
		}
	}()
	s.Record(m)
}

// Bind gives the exporter the registry it counts exports in (MON-R13). The
// registry is built after the sink, which it holds.
func (t *Telemetry) Bind(reg *telemetry.Registry) {
	if t != nil && t.exp != nil {
		t.exp.reg.Store(reg)
	}
}

// Lines are the boot lines: what is exported and where (the host, never a
// header), each piece turned off and why, and the warnings.
func (t *Telemetry) Lines() []string { return append([]string(nil), t.lines...) }

// ServiceInstanceID is this replica's service.instance.id (MON-R46): the
// one the resource carries when metrics are exported, and the name
// /internal/stats reports whether or not they are.
func (t *Telemetry) ServiceInstanceID() string { return t.instanceID }

// Endpoint is the OTLP endpoint URL, empty when nothing is pushed. It names
// the destination's host in the telemetry-export check, which renders
// scheme://host only.
func (t *Telemetry) Endpoint() string {
	if t == nil || t.exp == nil || t.cfg.OTLP == nil {
		return ""
	}
	return t.cfg.OTLP.Endpoint
}

// Run exports every interval until ctx is done (MON-R4). It runs no final
// export: Shutdown does, after everything else has stopped. Nil-safe.
func (t *Telemetry) Run(ctx context.Context) {
	if t == nil || t.provider == nil {
		return
	}
	tick := time.NewTicker(t.cfg.Interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			t.export(ctx, t.cfg.ExportTimeout)
		}
	}
}

// export collects and exports once within timeout.
func (t *Telemetry) export(ctx context.Context, timeout time.Duration) {
	t.exportMu.Lock()
	defer t.exportMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var rm metricdata.ResourceMetrics
	if err := t.reader.Collect(ctx, &rm); err != nil {
		otel.Handle(err)
		return
	}
	_ = t.exp.Export(ctx, &rm)
}

// Discard stops the providers without exporting: the cleanup of a gateway
// that does not start. Nil-safe and idempotent with Shutdown.
func (t *Telemetry) Discard() {
	if t == nil || t.provider == nil && t.pull == nil {
		return
	}
	t.stopOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownBudget)
		defer cancel()
		if t.provider != nil {
			_ = t.exp.Shutdown(ctx)
			_ = t.provider.Shutdown(ctx)
		}
		t.shutdownPull(ctx)
	})
}

// shutdownPull stops the pull listener's provider. The listener itself
// closes with ServePull's context.
func (t *Telemetry) shutdownPull(ctx context.Context) {
	if t.pull != nil {
		_ = t.pull.provider.Shutdown(ctx)
	}
}

// Shutdown runs the final export, so the counts recorded while the gateway
// stopped are exported, and stops the provider, within ctx and at most
// ShutdownBudget in all (MON-R14). The caller runs it last. Nil-safe and
// idempotent.
func (t *Telemetry) Shutdown(ctx context.Context) {
	if t == nil || t.provider == nil && t.pull == nil {
		return
	}
	t.stopOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownBudget)
		defer cancel()
		if t.provider != nil {
			t.export(ctx, shutdownBudget)
			_ = t.exp.Shutdown(ctx)
			_ = t.provider.Shutdown(ctx)
		}
		t.shutdownPull(ctx)
	})
}

// ConfigState is the telemetry-config check's state (MON-R44).
type ConfigState struct {
	// Present is whether any monitoring setting is set.
	Present bool
	// OK is whether every monitoring setting is valid.
	OK bool
	// Settings names the settings that are not valid, never their values.
	Settings []string
	// Off is whether one of them turned a piece of monitoring off; when not,
	// each fell back (to a default or to its general form) and export
	// continues.
	Off bool
	// Summaries say, per setting, what the gateway did instead
	// (Problem.Summary), never a value.
	Summaries []string
}

// ConfigCheck is the telemetry-config check's state.
func (t *Telemetry) ConfigCheck() ConfigState {
	if t == nil || !t.cfg.Set {
		return ConfigState{}
	}
	st := ConfigState{Present: true}
	// A build fault is the exporter settings': the file's in file mode.
	exportSetting := envMetricsExport
	if t.cfg.Mode == ModeFile {
		exportSetting = envConfigFile
	}
	for _, p := range t.cfg.Problems {
		if !containsString(st.Settings, p.Setting) {
			st.Settings = append(st.Settings, p.Setting)
		}
		st.Off = st.Off || p.Piece != ""
		st.Summaries = append(st.Summaries, p.Summary())
	}
	if t.buildFault {
		if !containsString(st.Settings, exportSetting) {
			st.Settings = append(st.Settings, exportSetting)
		}
		st.Off = true
		st.Summaries = append(st.Summaries, "the OTLP exporter could not be built from these settings, so OTLP export is off")
	}
	if t.pullFault {
		if !containsString(st.Settings, exportSetting) {
			st.Settings = append(st.Settings, exportSetting)
		}
		st.Off = true
		st.Summaries = append(st.Summaries, "the Prometheus exporter could not be built, so Prometheus pull is off")
	}
	if t.pull != nil && t.pull.notLoopback.Load() {
		host, _ := t.pull.cfg.settings()
		if !containsString(st.Settings, host) {
			st.Settings = append(st.Settings, host)
		}
		st.Off = true
		st.Summaries = append(st.Summaries, host+" bound an address other than this machine's loopback, so Prometheus pull is off")
	}
	if t.pull != nil && t.pull.bindFault.Load() {
		host, port := t.pull.cfg.settings()
		for _, s := range []string{host, port} {
			if !containsString(st.Settings, s) {
				st.Settings = append(st.Settings, s)
			}
		}
		st.Off = true
		st.Summaries = append(st.Summaries, "the Prometheus pull listener could not bind its address, so Prometheus pull is off")
	}
	st.OK = len(st.Settings) == 0
	return st
}

// ExportCheck is the telemetry-export check's cached state (MON-R13): its
// Present is whether an OTLP exporter is on.
func (t *Telemetry) ExportCheck() ExportState {
	if t == nil || t.exp == nil {
		return ExportState{}
	}
	st := t.exp.State()
	st.Present = true
	return st
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// temporalitySelector is the OTLP temporality preference (MON-R4), as the
// specification defines each.
func temporalitySelector(pref string) sdkmetric.TemporalitySelector {
	switch pref {
	case TemporalityDelta:
		return func(k sdkmetric.InstrumentKind) metricdata.Temporality {
			switch k {
			case sdkmetric.InstrumentKindCounter, sdkmetric.InstrumentKindHistogram, sdkmetric.InstrumentKindObservableCounter:
				return metricdata.DeltaTemporality
			}
			return metricdata.CumulativeTemporality
		}
	case TemporalityLowMemory:
		return func(k sdkmetric.InstrumentKind) metricdata.Temporality {
			switch k {
			case sdkmetric.InstrumentKindCounter, sdkmetric.InstrumentKindHistogram:
				return metricdata.DeltaTemporality
			}
			return metricdata.CumulativeTemporality
		}
	}
	return sdkmetric.DefaultTemporalitySelector
}

// exponentialHistogram is the base-2 exponential aggregation (MON-R4).
var exponentialHistogram = sdkmetric.AggregationBase2ExponentialHistogram{MaxSize: 160, MaxScale: 20}

// aggregationSelector is the default aggregation for an instrument outside
// the catalogue's views: histograms explicit on the SHN duration buckets, or
// exponential.
func aggregationSelector(histogram string) sdkmetric.AggregationSelector {
	return func(k sdkmetric.InstrumentKind) sdkmetric.Aggregation {
		if k == sdkmetric.InstrumentKindHistogram {
			if histogram == HistogramExponential {
				return exponentialHistogram
			}
			return sdkmetric.AggregationExplicitBucketHistogram{Boundaries: telemetry.DurationBuckets}
		}
		return sdkmetric.DefaultAggregationSelector(k)
	}
}

// gatewayView is the provider's one view (MON-R8, R18). Each catalogue
// histogram gets its own buckets, or the base-2 exponential aggregation the
// configuration chose; a configuration file's views, the participant's own
// override, apply over that, the first matching view winning. A view may
// rename a stream, re-describe it, change its aggregation or drop attributes.
// It can add no attribute (an attribute filter only removes) and sets no
// cardinality limit: the reader's per-kind limits hold (file.go). One view,
// rather than one per rule, so no instrument is exported as two streams.
func gatewayView(instruments []*telemetry.Instrument, histogram string, file []View) sdkmetric.View {
	base := map[string]sdkmetric.Aggregation{}
	buckets := map[string][]float64{}
	for _, inst := range instruments {
		if inst.Kind != telemetry.Histogram {
			continue
		}
		buckets[inst.Name] = inst.Buckets
		var agg sdkmetric.Aggregation = sdkmetric.AggregationExplicitBucketHistogram{Boundaries: inst.Buckets}
		if histogram == HistogramExponential {
			agg = exponentialHistogram
		}
		base[inst.Name] = agg
	}
	overrides := make([]sdkmetric.View, 0, len(file))
	for _, v := range file {
		overrides = append(overrides, fileView(v))
	}
	return func(i sdkmetric.Instrument) (sdkmetric.Stream, bool) {
		// The deprecated runtime instruments are dropped before any view of a
		// configuration file can match them (deprecatedRuntime).
		if deprecatedRuntime(i) {
			return sdkmetric.Stream{Aggregation: sdkmetric.AggregationDrop{}}, true
		}
		var s sdkmetric.Stream
		matched := false
		if agg, ok := base[i.Name]; ok && i.Scope.Name == scopeName {
			s = sdkmetric.Stream{Name: i.Name, Description: i.Description, Unit: i.Unit, Aggregation: agg}
			matched = true
		}
		for j, o := range overrides {
			if os, ok := o(i); ok {
				switch {
				case file[j].Aggregation == HistogramExplicit && len(file[j].Boundaries) == 0:
					// Explicit buckets without boundaries: the instrument's
					// own, never the exponential default.
					if b, ok := buckets[i.Name]; ok && i.Scope.Name == scopeName {
						os.Aggregation = sdkmetric.AggregationExplicitBucketHistogram{Boundaries: b}
					} else {
						os.Aggregation = sdkmetric.DefaultAggregationSelector(sdkmetric.InstrumentKindHistogram)
					}
				case os.Aggregation == nil && matched:
					os.Aggregation = s.Aggregation
				}
				s, matched = os, true
				break
			}
		}
		return s, matched
	}
}

// fileView is one configuration-file view, matched as the SDK matches a view.
func fileView(v View) sdkmetric.View {
	criteria := sdkmetric.Instrument{Name: v.InstrumentName, Unit: v.Unit,
		Scope: instrumentation.Scope{Name: v.MeterName, Version: v.MeterVersion, SchemaURL: v.MeterSchemaURL}}
	if k, ok := fileKinds[v.InstrumentKind]; ok {
		criteria.Kind = k
	}
	mask := sdkmetric.Stream{Name: v.Name, Description: v.Description}
	switch v.Aggregation {
	case "drop":
		mask.Aggregation = sdkmetric.AggregationDrop{}
	case "sum":
		mask.Aggregation = sdkmetric.AggregationSum{}
	case "last_value":
		mask.Aggregation = sdkmetric.AggregationLastValue{}
	case HistogramExplicit:
		// Without boundaries, gatewayView gives the instrument's own buckets.
		if len(v.Boundaries) > 0 {
			mask.Aggregation = sdkmetric.AggregationExplicitBucketHistogram{Boundaries: v.Boundaries}
		}
	case HistogramExponential:
		mask.Aggregation = exponentialHistogram
	}
	switch {
	case len(v.Include) > 0:
		keep := map[attribute.Key]bool{}
		for _, k := range v.Include {
			keep[attribute.Key(k)] = true
		}
		for _, k := range v.Exclude {
			delete(keep, attribute.Key(k))
		}
		mask.AttributeFilter = func(kv attribute.KeyValue) bool { return keep[kv.Key] }
	case len(v.Exclude) > 0:
		drop := map[attribute.Key]bool{}
		for _, k := range v.Exclude {
			drop[attribute.Key(k)] = true
		}
		mask.AttributeFilter = func(kv attribute.KeyValue) bool { return !drop[kv.Key] }
	}
	return sdkmetric.NewView(criteria, mask)
}

// fileKinds are the instrument kinds a configuration file names.
var fileKinds = map[string]sdkmetric.InstrumentKind{
	"counter":                    sdkmetric.InstrumentKindCounter,
	"up_down_counter":            sdkmetric.InstrumentKindUpDownCounter,
	"histogram":                  sdkmetric.InstrumentKindHistogram,
	"gauge":                      sdkmetric.InstrumentKindGauge,
	"observable_counter":         sdkmetric.InstrumentKindObservableCounter,
	"observable_up_down_counter": sdkmetric.InstrumentKindObservableUpDownCounter,
	"observable_gauge":           sdkmetric.InstrumentKindObservableGauge,
}

// kindLimits are the limits by SDK instrument kind: the largest series
// bound among the catalogue's instruments of that kind, narrowed by a
// configuration file's reader limits (by kind, or its default) and never
// raised by them.
func kindLimits(instruments []*telemetry.Instrument, file map[string]int) map[sdkmetric.InstrumentKind]int {
	limits := map[sdkmetric.InstrumentKind]int{}
	for _, inst := range instruments {
		k := sdkKind[inst.Kind]
		if b := inst.SeriesBound() + 1; b > limits[k] {
			limits[k] = b
		}
	}
	// The runtime and process instruments share these kinds' limits with
	// the catalogue's, and record at most runtimeSeriesBound series each
	// (process.cpu.time by cpu.mode, go.memory.used by go.memory.type); a
	// configuration file's lower limit still applies below.
	for _, k := range runtimeKinds {
		if l, ok := limits[k]; ok && l < runtimeSeriesBound+1 {
			limits[k] = runtimeSeriesBound + 1
		}
	}
	for name, k := range fileKinds {
		cap, ok := file[name]
		if !ok {
			cap, ok = file["default"]
		}
		// A kind the catalogue does not use (the runtime's observable
		// up-down counters) narrows from the provider's own limit.
		l, has := limits[k]
		if !has {
			l = providerCardinalityLimit
		}
		if ok && cap < l {
			limits[k] = cap
		}
	}
	return limits
}

// providerCardinalityLimit is the push provider's limit for an instrument
// kind the catalogue does not use; a configuration file can only narrow it.
const providerCardinalityLimit = 2000

// sdkKind is the SDK instrument kind each catalogue kind is created as.
var sdkKind = map[telemetry.Kind]sdkmetric.InstrumentKind{
	telemetry.Counter:           sdkmetric.InstrumentKindCounter,
	telemetry.UpDownCounter:     sdkmetric.InstrumentKindUpDownCounter,
	telemetry.Histogram:         sdkmetric.InstrumentKindHistogram,
	telemetry.Gauge:             sdkmetric.InstrumentKindGauge,
	telemetry.CumulativeCounter: sdkmetric.InstrumentKindObservableCounter,
	telemetry.Snapshot:          sdkmetric.InstrumentKindObservableGauge,
}

// cardinalityLimits sets each instrument kind's cardinality limit from the
// series bounds of the catalogue's instruments of that kind (MON-R18): the
// largest bound, plus the SDK's overflow series, or a configuration file's
// lower limit. The SDK sets limits per kind, not per instrument; since every
// attribute value is folded into its closed list first, no instrument reaches
// the catalogue's limit for its kind, though a file's lower one can fold its
// series into the overflow series.
func cardinalityLimits(instruments []*telemetry.Instrument, file map[string]int) sdkmetric.CardinalityLimitSelector {
	limits := kindLimits(instruments, file)
	return func(k sdkmetric.InstrumentKind) (int, bool) {
		if l, ok := limits[k]; ok {
			return l, false
		}
		return 0, true
	}
}

var (
	installOnce sync.Once
	sdkErrors   atomic.Int64
)

// installGlobalHandlers replaces the OpenTelemetry SDK's error handler and
// logger (MON-R10): a raw SDK error can carry an endpoint URL or a response
// body, so neither is ever logged. Export failures are classified by the
// exporter wrapper; anything else the SDK reports is counted and named by a
// fixed line on its 1st, 2nd, 4th, 8th… occurrence.
func installGlobalHandlers() {
	installOnce.Do(func() {
		otel.SetLogger(logr.Discard())
		otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) {
			if n := sdkErrors.Add(1); n&(n-1) == 0 {
				log.Printf("gateway: telemetry: the OpenTelemetry SDK reported an internal error (%d so far, logged at 1, 2, 4, 8 and so on)", n)
			}
		}))
	})
}
