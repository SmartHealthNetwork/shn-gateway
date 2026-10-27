//go:build !shn_coreonly

package app

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/pgstore"
	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
	"github.com/SmartHealthNetwork/shn-gateway/telemetry"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// opsMetricInterval is how often the operational gauges are sampled. They
// move in hours and days, so once a minute.
const opsMetricInterval = time.Minute

// opsSources are what each operational sample reads (MON-R25 to R27, R45).
// Each sample replaces the whole snapshot of each gauge, so a value that
// stops being known (a certificate kind that ages out) is absent rather than
// its last reading (MON-R15).
type opsSources struct {
	// info is the build info: one point per declared IG line.
	info []telemetry.Point
	// certs are the certificate readings.
	certs *endpointcert.Recorder
	// keys reads the shared ingress key store's times; nil without a store.
	keys func() pgstore.IngressKeyTimes
	// feed is the registrar feed's freshness; nil without a registrar.
	feed *feedFreshness
	now  func() time.Time
}

// gatewayInfoWire is the wire protocol shn.gateway.info names: the version
// this gateway sends (wireVersionSent), never shnsdk.WireProtocolVersion,
// which is the version of whichever shn-sdk the module compiles against. A
// variable only so a test can set the two apart.
var gatewayInfoWire = wireVersionSent

// infoPoints are shn.gateway.info's points: 1 for each IG line the declared
// contract versions name, with the wire protocol this gateway sends and the
// conformance level.
func infoPoints(wire string, contracts []string, level string) []telemetry.Point {
	var lines []string
	for _, c := range contracts {
		if l := shnsdk.LineOf(c); l != "" && !slices.Contains(lines, l) {
			lines = append(lines, l)
		}
	}
	slices.Sort(lines)
	points := make([]telemetry.Point, 0, len(lines))
	for _, l := range lines {
		points = append(points, telemetry.Point{Value: 1, Attributes: []string{wire, level, l}})
	}
	return points
}

// emitOps records one sample of every operational gauge.
func emitOps(reg *telemetry.Registry, src opsSources) {
	now := src.now()
	reg.RecordSnapshot(telemetry.GatewayInfo, src.info)

	var certs []telemetry.Point
	for _, r := range src.certs.Readings() {
		line := r.Tag.IGLine()
		if line == "" {
			line = telemetry.NoIGLine
		}
		certs = append(certs, telemetry.Point{Value: r.NotAfter.Sub(now).Seconds(), Attributes: []string{r.Tag.Kind().String(), r.Role.String(), line}})
	}
	reg.RecordSnapshot(telemetry.CertificateExpiry, certs)

	var remaining, age []telemetry.Point
	if src.keys != nil {
		kt := src.keys()
		if kt.HasSigning {
			remaining = append(remaining, telemetry.Point{Value: kt.SigningNotAfter.Sub(now).Seconds()})
		}
		if kt.HasNewest {
			age = append(age, telemetry.Point{Value: now.Sub(kt.NewestCreated).Seconds()})
		}
	}
	reg.RecordSnapshot(telemetry.IngressKeyRemaining, remaining)
	reg.RecordSnapshot(telemetry.IngressKeyAge, age)

	var feedAge, peers []telemetry.Point
	if last, n, ok := src.feed.read(); ok {
		feedAge = append(feedAge, telemetry.Point{Value: now.Sub(last).Seconds(), Attributes: []string{"registrar-feed"}})
		peers = append(peers, telemetry.Point{Value: float64(n)})
	}
	reg.RecordSnapshot(telemetry.DependencyAge, feedAge)
	reg.RecordSnapshot(telemetry.PeersKnown, peers)
}

// opsMetricLoop returns the Run-time sampler of the operational gauges: one
// sample immediately and then every interval, until ctx is done. Run starts
// it, not build, as with the pool's sampler.
func opsMetricLoop(reg *telemetry.Registry, src opsSources, interval time.Duration) func(context.Context) {
	return func(ctx context.Context) {
		t := time.NewTicker(interval)
		defer t.Stop()
		emitOps(reg, src)
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				emitOps(reg, src)
			}
		}
	}
}

// feedFreshness is the registrar feed's last successful read, on the
// gateway's clock, and how many holders it converged. Boot's read, the
// poller's and the engine's on-miss refresh all count. Nil-safe: a gateway
// with no registrar has none, and reports neither its age nor its peers.
type feedFreshness struct {
	now   func() time.Time
	mu    sync.Mutex
	last  time.Time
	peers int
	ok    bool
}

func newFeedFreshness(now func() time.Time) *feedFreshness { return &feedFreshness{now: now} }

// succeeded records a read that converged n holders.
func (f *feedFreshness) succeeded(n int) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last, f.peers, f.ok = f.now(), n, true
}

// read is the last successful read's time and holder count; ok is false
// before the first.
func (f *feedFreshness) read() (last time.Time, peers int, ok bool) {
	if f == nil {
		return time.Time{}, 0, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last, f.peers, f.ok
}
