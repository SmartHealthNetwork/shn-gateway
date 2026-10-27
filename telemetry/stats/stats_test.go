package stats

import (
	"cmp"
	"encoding/json"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
	"github.com/SmartHealthNetwork/shn-gateway/telemetry"
)

// sketchOf records vs into a fresh slot sketch and reads it out.
func sketchOf(vs []float64) Sketch {
	s := newSketch()
	for _, v := range vs {
		s.record(v)
	}
	var zero uint64
	var max float64
	dense := make([]uint64, bucketCount)
	s.addTo(&zero, dense, &max)
	return sparse(zero, dense, max)
}

// exact is the nearest-rank q-quantile of sorted vs: the first value at which
// the running count reaches q·n, the rank Quantile reads.
func exact(sorted []float64, q float64) float64 {
	for i, v := range sorted {
		if float64(i+1) >= q*float64(len(sorted)) {
			return v
		}
	}
	return sorted[len(sorted)-1]
}

// MON-R31: every quantile is within 2% of the true value, across the range
// a latency takes (seeded, so deterministic).
func TestSketchAccuracy_MON_R31(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for _, c := range []struct {
		name     string
		mu, sigm float64 // log-normal, in log seconds
	}{
		{"milliseconds", math.Log(0.005), 0.8},
		{"a second", math.Log(1), 0.5},
		{"tens of seconds", math.Log(20), 0.3},
		{"wide", math.Log(0.2), 2},
	} {
		vs := make([]float64, 20000)
		for i := range vs {
			vs[i] = math.Min(math.Exp(c.mu+c.sigm*r.NormFloat64()), MaxSeconds)
			if vs[i] <= MinSeconds {
				vs[i] = MinSeconds * 2
			}
		}
		s := sketchOf(vs)
		sort.Float64s(vs)
		for _, q := range []float64{0, 0.5, 0.9, 0.95, 0.99, 1} {
			got, ok := s.Quantile(q)
			want := exact(vs, q)
			if !ok || math.Abs(got-want) > RelativeError*want+1e-12 {
				t.Errorf("%s q=%.2f: %g, want %g within %g%%", c.name, q, got, want, RelativeError*100)
			}
		}
		if s.Max != vs[len(vs)-1] || s.Count() != uint64(len(vs)) {
			t.Errorf("%s: max %g count %d", c.name, s.Max, s.Count())
		}
	}
	if _, ok := (Sketch{}).Quantile(0.5); ok {
		t.Error("an empty sketch has no quantile")
	}
	// At or below MinSeconds a value counts in the zero bucket and reads as
	// 0; the max still states it.
	if s := sketchOf([]float64{0, MinSeconds}); s.Max != MinSeconds {
		t.Errorf("max %g, want %g", s.Max, MinSeconds)
	} else if v, _ := s.Quantile(1); v != 0 {
		t.Errorf("q=1 of zero-bucket values reads %g, want 0", v)
	}
	if v, _ := sketchOf([]float64{0, 0, 1}).Quantile(0.5); v != 0 {
		t.Errorf("a zero-bucket quantile reads %g", v)
	}
	// Just above MinSeconds a value is still within 2%: the lowest bucket
	// holds it. (A larger value keeps the max from capping the read.)
	for _, v := range []float64{MinSeconds * 1.00001, MinSeconds * 1.005, MinSeconds * 1.03} {
		if got, _ := sketchOf([]float64{v, 1}).Quantile(0); math.Abs(got-v) > RelativeError*v {
			t.Errorf("%g reads %g, more than 2%% off", v, got)
		}
	}
	// NaN and infinities are not durations: they are not counted.
	if s := sketchOf([]float64{math.NaN(), math.Inf(1), math.Inf(-1)}); s.Count() != 0 || s.Max != 0 {
		t.Errorf("non-durations recorded: %+v", s)
	}
	// Above MaxSeconds a value reads as the top bucket's value; max states it.
	if s := sketchOf([]float64{1000}); s.Max != 1000 {
		t.Errorf("max %g, want 1000", s.Max)
	} else if got, _ := s.Quantile(1); got != valueOf(maxIndex) {
		t.Errorf("1000 s reads %g, want the top bucket's %g", got, valueOf(maxIndex))
	}
}

// MON-R30, MON-R31: a quantile reads the nearest rank, the element ceil(q·n),
// as histogram_quantile picks its bucket; floor(q·(n-1)) would read a lower
// element in the rows marked so. Every value here is in a bucket of its own.
func TestQuantileIsNearestRank_MON_R31(t *testing.T) {
	ten := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	four := []float64{1, 2, 3, 4}
	for _, c := range []struct {
		vs      []float64
		q       float64
		element int // 1-based, in ascending order
	}{
		{ten, 0, 1},
		{ten, 0.05, 1},
		{ten, 0.25, 3}, // floor rule: the third too
		{ten, 0.5, 5},
		{ten, 0.95, 10}, // floor rule: the ninth
		{ten, 1, 10},
		{four, 0.5, 2},
		{four, 0.95, 4}, // floor rule: the third
	} {
		s := sketchOf(c.vs)
		got, _ := s.Quantile(c.q)
		i, _ := bucketOf(c.vs[c.element-1])
		if want := math.Min(valueOf(i), s.Max); got != want {
			t.Errorf("n=%d q=%g: %g, want %g (element %d, %g s)", len(c.vs), c.q, got, want, c.element, c.vs[c.element-1])
		}
	}
}

// merge is CONFIGURATION's merge recipe for two sketches with equal
// parameters: zero counts add, counts of equal indexes add, the larger max.
// The tests hold it to exactness. A reader that merges replicas must carry
// its own and refuse answers whose parameters differ.
func merge(s, o Sketch) Sketch {
	counts := map[int64]int64{}
	for _, b := range s.Buckets {
		counts[b[0]] += b[1]
	}
	for _, b := range o.Buckets {
		counts[b[0]] += b[1]
	}
	// Every index is kept, inside the bucket range or not, so no positive
	// count is lost in a merge.
	out := Sketch{Zero: s.Zero + o.Zero, Buckets: [][2]int64{}, Max: math.Max(s.Max, o.Max)}
	for i, n := range counts {
		if n > 0 {
			out.Buckets = append(out.Buckets, [2]int64{i, n})
		}
	}
	slices.SortFunc(out.Buckets, func(a, b [2]int64) int { return cmp.Compare(a[0], b[0]) })
	return out
}

// MON-R30, MON-R32: two replicas' sketches merge into exactly the sketch of
// both value sets.
func TestSketchMergeIsExact_MON_R30(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	var a, b []float64
	for range 5000 {
		a = append(a, math.Exp(math.Log(0.3)+r.NormFloat64()))
		b = append(b, math.Exp(math.Log(2)+0.5*r.NormFloat64()))
	}
	merged := merge(sketchOf(a), sketchOf(b))
	if both := sketchOf(append(append([]float64(nil), a...), b...)); !reflect.DeepEqual(merged, both) {
		t.Fatal("the merge of two sketches is not the sketch of both value sets")
	}
	if !reflect.DeepEqual(merge(merged, Sketch{}), merged) {
		t.Fatal("merging an empty sketch changes it")
	}
	// An index outside this build's bucket range (another build's, or a
	// malformed answer) is kept, never dropped: no count is lost.
	odd := Sketch{Buckets: [][2]int64{{int64(minIndex) - 5, 2}, {int64(maxIndex) + 5, 3}}, Max: 400}
	if m := merge(merged, odd); m.Count() != merged.Count()+5 || m.Buckets[0][0] != int64(minIndex)-5 || m.Buckets[len(m.Buckets)-1][0] != int64(maxIndex)+5 {
		t.Fatalf("merge lost or misordered out-of-range buckets: %v", m.Buckets)
	}
}

// clock is a settable test clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// kind is a compiled-in leg, whichever workstreams the build carries.
var kind = engine.ExchangeKinds[0]

func exchange(reg *telemetry.Registry, outcome string, d time.Duration) {
	reg.Add(telemetry.Exchanges, 1, "inbound", kind, outcome)
	reg.RecordDuration(telemetry.ExchangeDuration, d, "inbound", kind)
}

func window(t *testing.T, r Report, name string) Window {
	t.Helper()
	for _, w := range r.Windows {
		if w.Window == name {
			return w
		}
	}
	t.Fatalf("no %s window", name)
	return Window{}
}

// MON-R30: each window is its last N complete minutes plus the current
// partial one, with its start, count, rate and outcomes; a slot is reset
// when its minute comes round again.
func TestWindows_MON_R30(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 25, 12, 0, 30, 0, time.UTC)}
	a := New("replica-a", c.now)
	reg := telemetry.New(a)
	exchange(reg, "answered", time.Second) // minute 0
	c.add(2 * time.Minute)
	exchange(reg, "answered", 2*time.Second) // minute 2
	exchange(reg, "refused", 3*time.Second)
	c.add(3 * time.Minute) // now minute 5, 12:05:30
	exchange(reg, "answered", 4*time.Second)

	r := a.Report()
	if r.Replica != "replica-a" || !r.Since.Equal(time.Date(2026, 10, 25, 12, 0, 30, 0, time.UTC)) || !r.GeneratedAt.Equal(c.now()) {
		t.Fatalf("report header %+v", r)
	}
	one := window(t, r, "1m")
	if !one.Start.Equal(time.Date(2026, 10, 25, 12, 4, 0, 0, time.UTC)) || len(one.Exchanges) != 1 || one.Exchanges[0].Count != 1 {
		t.Fatalf("1m window %+v", one)
	}
	five := window(t, r, "5m")
	if len(five.Exchanges) != 1 {
		t.Fatalf("5m window %+v", five)
	}
	f := five.Exchanges[0]
	// The last five complete minutes (0 to 4) and the current one (5): the
	// window would start at 12:00:00, and is clamped to since.
	if f.Count != 4 || f.Outcomes["answered"] != 3 || f.Outcomes["refused"] != 1 || !five.Start.Equal(r.Since) {
		t.Fatalf("5m series %+v from %v", f, five.Start)
	}
	if want := 4 / c.now().Sub(r.Since).Seconds(); math.Abs(f.Rate-want) > 1e-9 {
		t.Fatalf("rate %g, want %g", f.Rate, want)
	}
	// Latencies 1, 2, 3 and 4 s: P50 is the second value (q·n = 2), 2 s,
	// within 2%.
	if f.Latency.Max == nil || *f.Latency.Max != 4 || f.Latency.P50 == nil || math.Abs(*f.Latency.P50-2) > 0.04 {
		t.Fatalf("latency %+v", f.Latency)
	}
	if h := window(t, r, "1h"); h.Exchanges[0].Count != 4 {
		t.Fatalf("1h count %d", h.Exchanges[0].Count)
	}

	// 61 minutes on, minute 5's slot (one answered exchange of 4 s) holds the
	// new minute only: its count, outcomes and latency all start again.
	c.add(61 * time.Minute)
	exchange(reg, "refused", 500*time.Millisecond)
	r = a.Report()
	h := window(t, r, "1h")
	if len(h.Exchanges) != 1 {
		t.Fatalf("after a full ring, 1h %+v", h)
	}
	e := h.Exchanges[0]
	if e.Count != 1 || !reflect.DeepEqual(e.Outcomes, map[string]uint64{"refused": 1}) {
		t.Fatalf("after a full ring, the reused slot counts %d %v", e.Count, e.Outcomes)
	}
	if e.Latency.Sketch.Count() != 1 || e.Latency.Max == nil || *e.Latency.Max != 0.5 {
		t.Fatalf("after a full ring, the reused slot's latency %+v", e.Latency)
	}
}

// MON-R30: the ring rotates on the injected clock's minute, lazily: a read
// after idle minutes finds no slot older than its window, though nothing
// was written to rotate them.
func TestIdleMinutesLeaveNoStaleSlot_MON_R30(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 25, 12, 0, 30, 0, time.UTC)}
	a := New("replica-a", c.now)
	reg := telemetry.New(a)
	exchange(reg, "answered", time.Second) // minute 0
	c.add(3 * time.Minute)                 // three idle minutes
	r := a.Report()
	if one := window(t, r, "1m"); len(one.Exchanges) != 0 {
		t.Fatalf("after 3 idle minutes the 1m window holds %+v", one.Exchanges)
	}
	if five := window(t, r, "5m"); len(five.Exchanges) != 1 || five.Exchanges[0].Count != 1 {
		t.Fatalf("after 3 idle minutes the 5m window holds %+v", five.Exchanges)
	}
	c.add(61 * time.Minute) // a full ring, still idle
	for _, w := range a.Report().Windows {
		if len(w.Exchanges) != 0 || len(w.Backends) != 0 {
			t.Fatalf("after a full idle ring the %s window holds %+v %+v", w.Window, w.Exchanges, w.Backends)
		}
	}
}

// Backend calls report their own count and latency, by exchange kind.
func TestBackendSeries_MON_R30(t *testing.T) {
	c := &clock{t: time.Unix(1_761_393_600, 0)}
	a := New("r", c.now)
	reg := telemetry.New(a)
	reg.Add(telemetry.BackendCalls, 1, kind, "ok")
	reg.RecordDuration(telemetry.BackendDuration, 250*time.Millisecond, kind)
	reg.Add(telemetry.BackendCalls, 1, kind, "timeout")
	b := window(t, a.Report(), "1m").Backends
	if len(b) != 1 || b[0].Exchange != kind || b[0].Count != 2 || b[0].Latency.Max == nil || *b[0].Latency.Max != 0.25 {
		t.Fatalf("backends %+v", b)
	}
	// An exchange whose duration was not recorded reports no quantiles.
	reg.Add(telemetry.Exchanges, 1, "ingress", kind, "answered")
	for _, e := range window(t, a.Report(), "1m").Exchanges {
		if e.Direction == "ingress" && (e.Latency.P50 != nil || e.Latency.Max != nil) {
			t.Fatalf("a series with no duration reports quantiles: %+v", e.Latency)
		}
	}
}

// MON-R19: nothing per peer, no identifier: every series key comes from the
// closed lists, and what a caller passes outside them folds to other.
func TestNoPerPeerData_MON_R19(t *testing.T) {
	a := New("r", nil)
	reg := telemetry.New(a)
	reg.Add(telemetry.Exchanges, 1, "holder-peer-1", "Patient/123", "corr-abc")
	reg.RecordDuration(telemetry.ExchangeDuration, time.Second, "holder-peer-1", "Patient/123")
	raw, err := json.Marshal(a.Report())
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"holder-peer-1", "Patient/123", "corr-abc"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("%q reached /internal/stats: %s", leak, raw)
		}
	}
	if !strings.Contains(string(raw), `"direction":"other","exchange":"other"`) {
		t.Fatalf("the folded series is missing: %s", raw)
	}
}

// MON-R31: at the series bound, every slot of every series allocated, the
// aggregator holds at most 8 MiB.
func TestFootprintAtTheSeriesBound_MON_R31(t *testing.T) {
	a := New("r", nil)
	perSlot := int(unsafe.Sizeof(slot{})) + len(a.outcomes)*int(unsafe.Sizeof(uint64(0))) +
		int(unsafe.Sizeof(sketch{})) + bucketCount*int(unsafe.Sizeof(uint32(0)))
	seriesCount := len(a.exchanges) + len(a.backends)
	total := seriesCount * (int(unsafe.Sizeof(series{})) + ringSlots*perSlot)
	if total > 8<<20 {
		t.Fatalf("%d series x %d slots x %d bytes = %d bytes, over 8 MiB", seriesCount, ringSlots, perSlot, total)
	}
	if want := telemetry.ExchangeDuration.SeriesBound() + telemetry.BackendDuration.SeriesBound(); seriesCount != want {
		t.Fatalf("%d series, want the exchange and backend duration bounds' %d", seriesCount, want)
	}
	t.Logf("worst case %d bytes for %d series", total, seriesCount)
}

// Recording from many goroutines while reports are read loses nothing.
func TestConcurrentRecording(t *testing.T) {
	c := &clock{t: time.Unix(1_761_393_600, 0)}
	a := New("r", c.now)
	reg := telemetry.New(a)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 1000 {
				exchange(reg, "answered", 10*time.Millisecond)
			}
		})
	}
	wg.Go(func() {
		for range 50 {
			_ = a.Report()
		}
	})
	wg.Wait()
	if e := window(t, a.Report(), "1m").Exchanges; len(e) != 1 || e[0].Count != 8000 || e[0].Latency.Sketch.Count() != 8000 {
		t.Fatalf("exchanges %+v", e)
	}
}

func TestHandler(t *testing.T) {
	a := New("r", nil)
	exchange(telemetry.New(a), "answered", time.Second)
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/internal/stats", nil))
	var r Report
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil || w.Code != http.StatusOK || len(r.Windows) != 3 || r.SketchParameters.RelativeError != RelativeError {
		t.Fatalf("GET %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/internal/stats", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST %d", w.Code)
	}
}

// /internal/stats carries no gauges: a snapshot of every operational gauge,
// handed whole or series by series, leaves the report as it was.
func TestSnapshotsAreNotStats(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 10, 26, 3, 0, 0, 0, time.UTC) }
	a := New("r", now)
	before, err := json.Marshal(a.Report())
	if err != nil {
		t.Fatal(err)
	}
	reg := telemetry.New(a)
	reg.RecordSnapshot(telemetry.CertificateExpiry, []telemetry.Point{{Value: 86400, Attributes: []string{"hub", "server", telemetry.NoIGLine}}})
	reg.RecordSnapshot(telemetry.DependencyAge, []telemetry.Point{{Value: 3, Attributes: []string{"registrar-feed"}}})
	reg.RecordSnapshot(telemetry.PeersKnown, []telemetry.Point{{Value: 4}})
	reg.RecordSnapshot(telemetry.GatewayInfo, []telemetry.Point{{Value: 1, Attributes: []string{"2.0.0", "none", "2.2"}}})
	after, err := json.Marshal(a.Report())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("snapshots reached /internal/stats:\nbefore %s\nafter  %s", before, after)
	}
}
