package otelsetup

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/internal/otlptest"
	"github.com/SmartHealthNetwork/shn-gateway/telemetry"
)

// MON-R15: a series one snapshot holds and the next leaves out is absent
// from the next collection, under both temporalities, where a synchronous
// gauge would keep exporting its last value, and an instrument whose
// snapshot empties is gone from the collection altogether.
func TestSnapshot_SeriesLeftOutIsAbsent_MON_R15(t *testing.T) {
	for _, pref := range []string{TemporalityCumulative, TemporalityDelta} {
		t.Run(pref, func(t *testing.T) {
			col := otlptest.New()
			tel, reg, _ := build(t, ProtocolHTTP, col.HTTP(t).URL+"/v1/metrics", map[string]string{
				"OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE": pref,
			})
			reg.RecordSnapshot(telemetry.CertificateExpiry, []telemetry.Point{
				{Value: 864000, Attributes: []string{"hub", "server", telemetry.NoIGLine}},
				{Value: 3600, Attributes: []string{"cds", "server", telemetry.NoIGLine}},
			})
			reg.RecordSnapshot(telemetry.IngressKeyRemaining, []telemetry.Point{{Value: 3900}})
			exportOnce(tel)
			reg.RecordSnapshot(telemetry.CertificateExpiry, []telemetry.Point{
				{Value: 863940, Attributes: []string{"hub", "server", telemetry.NoIGLine}},
			})
			reg.RecordSnapshot(telemetry.IngressKeyRemaining, nil)
			reg.RecordSnapshot(telemetry.DependencyAge, []telemetry.Point{{Value: 2, Attributes: []string{"registrar-feed"}}})
			exportOnce(tel)
			got := col.Exports()
			if len(got) != 2 {
				t.Fatalf("%d exports, want 2", len(got))
			}
			first, second := otlptest.Render(got[0].Request, otlptest.Options{}), otlptest.Render(got[1].Request, otlptest.Options{})
			for _, want := range []string{
				`shn.gateway.certificate.expiry unit="s" gauge`,
				"{shn.cert.role=server,shn.endpoint.kind=cds,shn.ig_line=none} 3600",
				"{shn.cert.role=server,shn.endpoint.kind=hub,shn.ig_line=none} 864000",
				`shn.gateway.ingress_key.remaining unit="s" gauge`,
				"3900",
			} {
				if !strings.Contains(first, want) {
					t.Errorf("first collection lacks %q:\n%s", want, first)
				}
			}
			if strings.Contains(first, "shn.gateway.dependency.age") {
				t.Errorf("dependency.age before any snapshot of it:\n%s", first)
			}
			if !strings.Contains(second, "{shn.cert.role=server,shn.endpoint.kind=hub,shn.ig_line=none} 863940") ||
				!strings.Contains(second, "{shn.dependency=registrar-feed} 2") {
				t.Errorf("second collection lacks the new snapshot:\n%s", second)
			}
			for _, gone := range []string{"shn.endpoint.kind=cds", "shn.gateway.ingress_key.remaining"} {
				if strings.Contains(second, gone) {
					t.Errorf("second collection still reports %q, which its snapshot left out:\n%s", gone, second)
				}
			}
		})
	}
}

// MON-R12: the collection callback never waits on a snapshot being recorded,
// and the two never race (run under -race).
func TestSnapshot_CollectionAndRecordingDoNotRace_MON_R12(t *testing.T) {
	col := otlptest.New()
	tel, reg, _ := build(t, ProtocolHTTP, col.HTTP(t).URL+"/v1/metrics", nil)
	var wg sync.WaitGroup
	for n := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				reg.RecordSnapshot(telemetry.PeersKnown, []telemetry.Point{{Value: float64(n*1000 + i)}})
			}
		}()
	}
	for range 5 {
		exportOnce(tel)
	}
	wg.Wait()
	exportOnce(tel)
	last := col.Exports()
	if text := otlptest.Render(last[len(last)-1].Request, otlptest.Options{}); !strings.Contains(text, `shn.gateway.peers.known unit="{peer}" gauge`) {
		t.Fatalf("peers.known not exported after the race:\n%s", text)
	}
}

// MON-R15 on the pull path: a scrape after a snapshot that leaves a series
// out does not show it, with pull alone and with pull beside OTLP push (one
// combined sink, which must hand each sink the snapshot whole).
func TestSnapshot_SeriesLeftOutIsAbsentFromPull_MON_R15(t *testing.T) {
	for _, withPush := range []bool{false, true} {
		t.Run(map[bool]string{false: "pull", true: "pull and push"}[withPush], func(t *testing.T) {
			settings := pullEnv()
			var col *otlptest.Collector
			if withPush {
				col = otlptest.New()
				settings = pullEnv("OTEL_METRICS_EXPORTER", "otlp,prometheus", "OTEL_EXPORTER_OTLP_PROTOCOL", ProtocolHTTP,
					"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", col.HTTP(t).URL+"/v1/metrics")
			}
			tel := Build(Load(env(settings), nil), testIdentity, Options{Logf: func(string, ...any) {}})
			defer tel.Shutdown(context.Background())
			reg := telemetry.New(tel.Sink())
			tel.Bind(reg)
			url := "http://" + servePull(t, tel) + "/metrics"
			scrape := func() string {
				t.Helper()
				resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(url)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				b, _ := io.ReadAll(resp.Body)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("%d\n%s", resp.StatusCode, b)
				}
				return string(b)
			}
			reg.RecordSnapshot(telemetry.CertificateExpiry, []telemetry.Point{
				{Value: 864000, Attributes: []string{"hub", "server", telemetry.NoIGLine}},
				{Value: 3600, Attributes: []string{"cds", "server", telemetry.NoIGLine}},
			})
			if first := scrape(); !strings.Contains(first, `shn_endpoint_kind="cds"`) || !strings.Contains(first, `shn_endpoint_kind="hub"`) {
				t.Fatalf("first scrape lacks the snapshot's series:\n%s", first)
			}
			reg.RecordSnapshot(telemetry.CertificateExpiry, []telemetry.Point{
				{Value: 863940, Attributes: []string{"hub", "server", telemetry.NoIGLine}},
			})
			second := scrape()
			if strings.Contains(second, `shn_endpoint_kind="cds"`) || !strings.Contains(second, `shn_endpoint_kind="hub"`) {
				t.Fatalf("second scrape still shows the series its snapshot left out, or lost the one it kept:\n%s", second)
			}
			if withPush {
				exportOnce(tel)
				got := col.Exports()
				if len(got) != 1 {
					t.Fatalf("%d exports, want 1", len(got))
				}
				text := otlptest.Render(got[0].Request, otlptest.Options{})
				if !strings.Contains(text, "shn.endpoint.kind=hub") || strings.Contains(text, "shn.endpoint.kind=cds") {
					t.Fatalf("the push beside pull does not carry the latest snapshot:\n%s", text)
				}
			}
		})
	}
}
