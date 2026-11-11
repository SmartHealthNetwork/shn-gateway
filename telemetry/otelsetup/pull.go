package otelsetup

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
	"github.com/SmartHealthNetwork/shn-gateway/internal/nosniff"
	"github.com/SmartHealthNetwork/shn-gateway/telemetry"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/otlptranslator"
	"go.opentelemetry.io/otel/attribute"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/exemplar"
	"go.opentelemetry.io/otel/sdk/resource"
)

// pullServer is the Prometheus pull listener (MON-R6): its own meter
// provider, read by the official exporter on each scrape, served on its own
// bind, never the ingress listener.
type pullServer struct {
	cfg      Pull
	provider *sdkmetric.MeterProvider
	sink     *sink
	handler  http.Handler
	// bindFault is set when the bind failed: pull is off, and the
	// telemetry-config check says so (MON-R44).
	bindFault atomic.Bool
	// notLoopback is set when a host taken for this machine's (localhost)
	// bound an address other than a loopback one: nothing is served, since
	// the bind had no protection (MON-R7).
	notLoopback atomic.Bool
}

// off reports whether the listener turned off after Build.
func (p *pullServer) off() bool { return p.bindFault.Load() || p.notLoopback.Load() }

// Record implements telemetry.Sink: nothing is recorded once the listener is
// off, since nothing can scrape it.
func (p *pullServer) Record(m telemetry.Measurement) {
	if !p.off() {
		p.sink.Record(m)
	}
}

// RecordSnapshot implements telemetry.SnapshotSink, as Record does.
func (p *pullServer) RecordSnapshot(i *telemetry.Instrument, series []telemetry.Measurement) {
	if !p.off() {
		p.sink.RecordSnapshot(i, series)
	}
}

// buildPull builds the pull listener's provider and handler. Its provider is
// separate from the OTLP push provider: the scrape is always cumulative and
// always on SHN's explicit buckets, whatever the OTLP temporality and
// histogram settings, which name the push exporter alone.
func (t *Telemetry) buildPull(id Identity, opts Options) error {
	reg := prometheus.NewRegistry()
	// Every naming option is explicit (MON-R6), so an exporter upgrade that
	// changes a default changes nothing here; the golden /metrics pins the
	// result (MON-R40).
	exp, err := otelprom.New(
		// The gateway's own registry: no process or Go collector the
		// default registry carries, and nothing another package registers.
		otelprom.WithRegisterer(reg),
		// shn.gateway.exchange.duration → shn_gateway_exchange_duration_seconds;
		// counters gain _total; annotation units such as {exchange} add
		// nothing.
		otelprom.WithTranslationStrategy(otlptranslator.UnderscoreEscapingWithSuffixes),
		otelprom.WithNamespace(""),
		// No scope labels: the gateway's own instruments, the Go runtime's
		// and the process's have names that never collide, so a scope label
		// on every series would say nothing.
		otelprom.WithoutScopeInfo(),
		// The resource stays on target_info alone (no WithoutTargetInfo, no
		// WithResourceAsConstantLabels).
		otelprom.WithAggregationSelector(aggregationSelector(HistogramExplicit)),
	)
	if err == nil && pullExporterRefusal != nil {
		err = pullExporterRefusal
	}
	if err != nil {
		return err
	}
	kvs := []attribute.KeyValue{}
	for _, kv := range resourceAttributes(t.cfg, id, t.instanceID) {
		kvs = append(kvs, attribute.String(kv[0], kv[1]))
	}
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(resource.NewSchemaless(kvs...)),
		sdkmetric.WithReader(exp),
		sdkmetric.WithView(gatewayView(opts.Instruments, HistogramExplicit, pullViews(t.cfg.Views))),
		sdkmetric.WithExemplarFilter(exemplar.AlwaysOffFilter),
		// The exporter's reader takes no cardinality selector, so the limit
		// is the provider's: the largest catalogue series bound plus the
		// overflow series (MON-R18).
		sdkmetric.WithCardinalityLimit(largestSeriesBound(opts.Instruments)+1),
	)
	s, err := newSink(provider.Meter(scopeName, metric.WithInstrumentationVersion(id.Version)), opts.Instruments)
	if err != nil {
		_ = provider.Shutdown(context.Background())
		return err
	}
	startRuntimeMetrics(provider, id.Version, opts.Now, opts.Logf)
	if tc := t.cfg.Pull.TLS; tc != nil && len(tc.Certificates) > 0 {
		opts.Certs.RecordOwn(endpointcert.MetricsPull, envPullTLSCert, &tc.Certificates[0])
	}
	p := &pullServer{cfg: *t.cfg.Pull, provider: provider, sink: s}
	p.handler = pullHandler(promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		// A metric that fails to collect or convert leaves the rest of the
		// scrape served. Nothing logs it: no ErrorLog, since an error's text
		// is never logged raw (MON-R10).
		ErrorHandling: promhttp.ContinueOnError,
		// The text format only: OpenMetrics would add _created series and
		// unit metadata the golden does not pin.
		EnableOpenMetrics: false,
		// Scrapes at once, and the bound on one.
		MaxRequestsInFlight: 4,
		Timeout:             10 * time.Second,
	}), p.cfg.Token)
	t.pull = p
	return nil
}

// pullViews are a configuration file's views as the pull listener applies
// them: an exponential histogram aggregation keeps the instrument's explicit
// buckets, since the text exposition carries none (loadFile reports it).
func pullViews(views []View) []View {
	out := make([]View, len(views))
	for i, v := range views {
		if v.Aggregation == HistogramExponential {
			v.Aggregation = ""
		}
		out[i] = v
	}
	return out
}

// largestSeriesBound is the largest series bound among instruments.
func largestSeriesBound(instruments []*telemetry.Instrument) int {
	n := 0
	for _, inst := range instruments {
		n = max(n, inst.SeriesBound())
	}
	return n
}

// pullHandler serves GET /metrics from metrics, every request first
// presenting token as a bearer token when token is set (MON-R7). A request
// without it, or with another, gets 401 whatever its path, so nothing about
// the listener is learned without the token.
func pullHandler(metrics http.Handler, token string) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics)
	if token == "" {
		return nosniff.Handler(mux)
	}
	want := sha256.Sum256([]byte(token))
	return nosniff.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !bearerMatches(r.Header.Get("Authorization"), want) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	}))
}

// bearerMatches reports whether header carries the bearer token whose
// SHA-256 is want, compared in constant time over the digests, so neither the
// token's bytes nor its length is timed.
func bearerMatches(header string, want [sha256.Size]byte) bool {
	scheme, presented, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	got := sha256.Sum256([]byte(strings.TrimSpace(presented)))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

// pullExporterRefusal, when set, is returned as the SDK's refusal of the
// Prometheus exporter: a test seam, nil in every build.
var pullExporterRefusal error

// listen is net.Listen; a test replaces it. In a test build, listen_fortest.go
// wraps it to serve a listener a test bound (HandListenerForTest).
var listen = net.Listen

// ServePull serves the Prometheus pull listener until ctx is done, then
// closes it. A bind that fails turns pull off with a boot line and the
// telemetry-config check, and the gateway carries on (MON-R44). Nil-safe: a
// no-op when pull is off.
func (t *Telemetry) ServePull(ctx context.Context) {
	if t == nil || t.pull == nil {
		return
	}
	p := t.pull
	hostSetting, portSetting := p.cfg.settings()
	ln, err := listen("tcp", p.cfg.Addr)
	if err != nil {
		// The error names the address, never a secret, but the line names
		// the settings alone, as every telemetry line does.
		p.bindFault.Store(true)
		t.logf("gateway: WARNING: telemetry: the Prometheus pull listener could not bind its address, so Prometheus pull is off: choose a free " + portSetting + " or another " + hostSetting)
		return
	}
	// A host taken for this machine's by its name (localhost) is served
	// without protection only where it bound a loopback address: a name
	// resolves through the hosts file and DNS, and the bind decides.
	if tcp, isTCP := ln.Addr().(*net.TCPAddr); p.cfg.Loopback && (!isTCP || !tcp.IP.IsLoopback()) {
		_ = ln.Close()
		p.notLoopback.Store(true)
		t.logf("gateway: WARNING: telemetry: " + hostSetting + " bound an address other than this machine's loopback, so Prometheus pull is off: name a loopback address (127.0.0.1 or ::1), or name another host and protect its bind with TLS and a token or client CA")
		return
	}
	if p.cfg.TLS != nil {
		ln = tls.NewListener(ln, p.cfg.TLS)
	}
	srv := &http.Server{
		Handler:           p.handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		// A failed handshake is the caller's (a scrape without a valid client
		// certificate, a port scanner); logging each would let anyone who
		// reaches the port fill the log.
		ErrorLog: log.New(io.Discard, "", 0),
	}
	served := make(chan struct{})
	go func() { defer close(served); _ = srv.Serve(ln) }()
	<-ctx.Done()
	_ = srv.Close()
	<-served
}

// PullHandler is the handler ServePull serves, token check included; nil
// when pull is off.
func (t *Telemetry) PullHandler() http.Handler {
	if t == nil || t.pull == nil {
		return nil
	}
	return t.pull.handler
}

// PullAddr is the pull listener's bind, empty when pull is off.
func (t *Telemetry) PullAddr() string {
	if t == nil || t.pull == nil {
		return ""
	}
	return t.pull.cfg.Addr
}
