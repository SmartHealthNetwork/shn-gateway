package otelsetup

import (
	"context"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/telemetry"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// MON-R29: an Int attribute is exported as an integer, every code net/http
// can write included, so the attribute has one type; an Optional attribute
// recorded as "" is left out of the set.
func TestSinkAttributeTypes_MONR29(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	s, err := newSink(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test"), []*telemetry.Instrument{telemetry.HTTPServerRequestDuration})
	if err != nil {
		t.Fatal(err)
	}
	reg := telemetry.New(s)
	for _, status := range []string{"100", "299", "503", "999"} {
		reg.RecordDuration(telemetry.HTTPServerRequestDuration, time.Millisecond, "GET", status, "/health", "http", "1.1", "")
	}
	reg.RecordDuration(telemetry.HTTPServerRequestDuration, time.Millisecond, "GET", "", "", "http", "1.1", telemetry.SemconvOther)
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	points := rm.ScopeMetrics[0].Metrics[0].Data.(metricdata.Histogram[float64]).DataPoints
	if len(points) != 5 {
		t.Fatalf("%d series, want 5", len(points))
	}
	ints := map[int64]bool{}
	absent := 0
	for _, p := range points {
		status, hasStatus := p.Attributes.Value(telemetry.KeyHTTPStatusCode)
		_, hasRoute := p.Attributes.Value(telemetry.KeyHTTPRoute)
		_, hasError := p.Attributes.Value(telemetry.KeyErrorType)
		switch {
		case !hasStatus:
			if hasRoute || !hasError {
				t.Errorf("the panic before a status: %v", p.Attributes.ToSlice())
			}
			absent++
		case status.Type() != attribute.INT64:
			t.Errorf("status %v exported as %v, not an integer", status.Emit(), status.Type())
		default:
			ints[status.AsInt64()] = true
		}
	}
	if absent != 1 || len(ints) != 4 || !ints[100] || !ints[299] || !ints[503] || !ints[999] {
		t.Fatalf("statuses %v, %d absent: want 100, 299, 503 and 999 as integers and one absent", ints, absent)
	}
}
