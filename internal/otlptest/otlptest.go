// Package otlptest is an in-process OTLP metrics collector for hermetic
// tests: it receives exports over HTTP (protobuf) and gRPC, keeps each
// decoded request with the headers it carried, answers as told, and renders
// a request as stable text for golden comparison.
package otlptest

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Export is one received export request.
type Export struct {
	Request *colmetricpb.ExportMetricsServiceRequest
	// Headers are the request's headers (gRPC metadata), lower-cased keys.
	Headers map[string]string
	Path    string
	// At is when the collector received it.
	At time.Time
}

// Collector records exports. HTTPStatus and GRPCCode, when set, are the
// answer to every export (the request is still recorded); Hold, when set,
// blocks each export until it is closed or the request ends.
type Collector struct {
	mu         sync.Mutex
	exports    []Export
	HTTPStatus int
	GRPCCode   codes.Code
	Rejected   int64 // data points a successful answer reports rejected (partial success)
	Hold       chan struct{}
	arrived    chan struct{}
}

// New returns an empty Collector.
func New() *Collector { return &Collector{arrived: make(chan struct{}, 64)} }

// Arrived receives once per export received.
func (c *Collector) Arrived() <-chan struct{} { return c.arrived }

func (c *Collector) record(e Export) {
	e.At = time.Now()
	c.mu.Lock()
	c.exports = append(c.exports, e)
	c.mu.Unlock()
	select {
	case c.arrived <- struct{}{}:
	default:
	}
}

// Exports returns the exports received so far.
func (c *Collector) Exports() []Export {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Export(nil), c.exports...)
}

func (c *Collector) settings() (int, codes.Code, int64, chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.HTTPStatus, c.GRPCCode, c.Rejected, c.Hold
}

// Set changes the answer under the collector's lock.
func (c *Collector) Set(httpStatus int, grpcCode codes.Code) {
	c.mu.Lock()
	c.HTTPStatus, c.GRPCCode = httpStatus, grpcCode
	c.mu.Unlock()
}

// SetHold sets Hold under the collector's lock.
func (c *Collector) SetHold(h chan struct{}) {
	c.mu.Lock()
	c.Hold = h
	c.mu.Unlock()
}

// HTTP starts the collector's OTLP/HTTP endpoint; its URL is the base, and
// exports go to <URL>/v1/metrics.
func (c *Collector) HTTP(t testing.TB) *httptest.Server {
	srv := httptest.NewServer(c.httpHandler())
	t.Cleanup(srv.Close)
	return srv
}

// HTTPS is HTTP over TLS, presenting cert.
func (c *Collector) HTTPS(t testing.TB, cert tls.Certificate) *httptest.Server {
	srv := httptest.NewUnstartedServer(c.httpHandler())
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func (c *Collector) httpHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req colmetricpb.ExportMetricsServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		h := map[string]string{}
		for k, v := range r.Header {
			h[strings.ToLower(k)] = strings.Join(v, ",")
		}
		c.record(Export{Request: &req, Headers: h, Path: r.URL.Path})
		st, _, rejected, hold := c.settings()
		if hold != nil {
			select {
			case <-hold:
			case <-r.Context().Done():
				return
			}
		}
		if st != 0 && st != http.StatusOK {
			w.WriteHeader(st)
			_, _ = w.Write([]byte("collector-body-detail"))
			return
		}
		resp := &colmetricpb.ExportMetricsServiceResponse{}
		if rejected > 0 {
			resp.PartialSuccess = &colmetricpb.ExportMetricsPartialSuccess{RejectedDataPoints: rejected, ErrorMessage: "collector-partial-detail"}
		}
		out, _ := proto.Marshal(resp)
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(out)
	})
}

type grpcService struct {
	colmetricpb.UnimplementedMetricsServiceServer
	c *Collector
}

func (s grpcService) Export(ctx context.Context, req *colmetricpb.ExportMetricsServiceRequest) (*colmetricpb.ExportMetricsServiceResponse, error) {
	h := map[string]string{}
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		for k, v := range md {
			h[strings.ToLower(k)] = strings.Join(v, ",")
		}
	}
	s.c.record(Export{Request: req, Headers: h})
	_, code, rejected, hold := s.c.settings()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if code != codes.OK {
		return nil, status.Error(code, "collector-status-detail")
	}
	resp := &colmetricpb.ExportMetricsServiceResponse{}
	if rejected > 0 {
		resp.PartialSuccess = &colmetricpb.ExportMetricsPartialSuccess{RejectedDataPoints: rejected, ErrorMessage: "collector-partial-detail"}
	}
	return resp, nil
}

// GRPC starts the collector's OTLP/gRPC endpoint and returns its address as
// an http:// URL.
func (c *Collector) GRPC(t testing.TB) string {
	return c.serveGRPC(t, "http://")
}

// GRPCS is GRPC over TLS, presenting cert; its address is an https:// URL.
func (c *Collector) GRPCS(t testing.TB, cert tls.Certificate) string {
	return c.serveGRPC(t, "https://", grpc.Creds(credentials.NewServerTLSFromCert(&cert)))
}

func (c *Collector) serveGRPC(t testing.TB, scheme string, opts ...grpc.ServerOption) string {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(opts...)
	colmetricpb.RegisterMetricsServiceServer(srv, grpcService{c: c})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return scheme + lis.Addr().String()
}

// Options select what Render leaves out: values that differ from run to run.
type Options struct {
	// MaskResource names resource attributes whose values render as "<set>".
	MaskResource []string
	// NamesOnly names instrumentation scopes whose metrics render their
	// name, unit and type only: values that change from run to run (the Go
	// runtime's, the process's).
	NamesOnly []string
	// Omit names metrics left out altogether: those present on some
	// platforms or settings only.
	Omit []string
}

// Render is req as stable text: the resource's attributes, then each scope,
// sorted by name, with its metrics sorted by name, each with its unit, type,
// temporality and data points sorted by attributes, without timestamps.
func Render(req *colmetricpb.ExportMetricsServiceRequest, o Options) string {
	var b strings.Builder
	for _, rm := range req.GetResourceMetrics() {
		b.WriteString("resource\n")
		for _, kv := range sortedKVs(rm.GetResource().GetAttributes()) {
			v := anyValue(kv.GetValue())
			for _, m := range o.MaskResource {
				if m == kv.GetKey() {
					v = "<set>"
				}
			}
			fmt.Fprintf(&b, "  %s=%s\n", kv.GetKey(), v)
		}
		sms := append([]*metricpb.ScopeMetrics(nil), rm.GetScopeMetrics()...)
		sort.Slice(sms, func(i, j int) bool { return sms[i].GetScope().GetName() < sms[j].GetScope().GetName() })
		for _, sm := range sms {
			fmt.Fprintf(&b, "scope %s %s\n", sm.GetScope().GetName(), sm.GetScope().GetVersion())
			ms := append([]*metricpb.Metric(nil), sm.GetMetrics()...)
			sort.Slice(ms, func(i, j int) bool { return ms[i].GetName() < ms[j].GetName() })
			namesOnly := slices.Contains(o.NamesOnly, sm.GetScope().GetName())
			for _, m := range ms {
				if !slices.Contains(o.Omit, m.GetName()) {
					renderMetric(&b, m, namesOnly)
				}
			}
		}
	}
	return b.String()
}

func renderMetric(b *strings.Builder, m *metricpb.Metric, namesOnly bool) {
	var lines []string
	kind := ""
	switch d := m.GetData().(type) {
	case *metricpb.Metric_Sum:
		kind = fmt.Sprintf("sum monotonic=%t %s", d.Sum.GetIsMonotonic(), d.Sum.GetAggregationTemporality())
		for _, p := range d.Sum.GetDataPoints() {
			lines = append(lines, fmt.Sprintf("{%s} %s", attrs(p.GetAttributes()), number(p)))
		}
	case *metricpb.Metric_Gauge:
		kind = "gauge"
		for _, p := range d.Gauge.GetDataPoints() {
			lines = append(lines, fmt.Sprintf("{%s} %s", attrs(p.GetAttributes()), number(p)))
		}
	case *metricpb.Metric_Histogram:
		kind = "histogram " + d.Histogram.GetAggregationTemporality().String()
		for _, p := range d.Histogram.GetDataPoints() {
			lines = append(lines, fmt.Sprintf("{%s} count=%d sum=%s bounds=%s buckets=%v", attrs(p.GetAttributes()), p.GetCount(), f(p.GetSum()), floats(p.GetExplicitBounds()), p.GetBucketCounts()))
		}
	case *metricpb.Metric_ExponentialHistogram:
		kind = "exponential_histogram " + d.ExponentialHistogram.GetAggregationTemporality().String()
		for _, p := range d.ExponentialHistogram.GetDataPoints() {
			lines = append(lines, fmt.Sprintf("{%s} count=%d sum=%s scale=%d", attrs(p.GetAttributes()), p.GetCount(), f(p.GetSum()), p.GetScale()))
		}
	default:
		kind = "unknown"
	}
	sort.Strings(lines)
	fmt.Fprintf(b, "  %s unit=%q %s\n", m.GetName(), m.GetUnit(), kind)
	if namesOnly {
		return
	}
	for _, l := range lines {
		fmt.Fprintf(b, "    %s\n", l)
	}
}

func number(p *metricpb.NumberDataPoint) string {
	switch v := p.GetValue().(type) {
	case *metricpb.NumberDataPoint_AsDouble:
		return f(v.AsDouble)
	case *metricpb.NumberDataPoint_AsInt:
		return strconv.FormatInt(v.AsInt, 10)
	}
	return "?"
}

func attrs(kvs []*commonpb.KeyValue) string {
	var parts []string
	for _, kv := range sortedKVs(kvs) {
		parts = append(parts, kv.GetKey()+"="+anyValue(kv.GetValue()))
	}
	return strings.Join(parts, ",")
}

func sortedKVs(kvs []*commonpb.KeyValue) []*commonpb.KeyValue {
	out := append([]*commonpb.KeyValue(nil), kvs...)
	sort.Slice(out, func(i, j int) bool { return out[i].GetKey() < out[j].GetKey() })
	return out
}

func anyValue(v *commonpb.AnyValue) string {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	case *commonpb.AnyValue_BoolValue:
		return strconv.FormatBool(x.BoolValue)
	case *commonpb.AnyValue_DoubleValue:
		return f(x.DoubleValue)
	}
	return fmt.Sprint(v)
}

func f(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

func floats(vs []float64) string {
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = f(v)
	}
	return "[" + strings.Join(parts, " ") + "]"
}
