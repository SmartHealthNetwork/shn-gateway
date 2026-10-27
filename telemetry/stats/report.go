package stats

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"
)

// Report is /internal/stats' answer (MON-R30). Fields may be added in a
// minor release: decode tolerantly.
type Report struct {
	// Replica is the answering replica's service.instance.id.
	Replica string `json:"replica"`
	// Since is when this replica's aggregator started; no window reaches
	// before it.
	Since       time.Time `json:"since"`
	GeneratedAt time.Time `json:"generatedAt"`
	// SketchParameters are what a reader checks before merging two
	// replicas' sketches: equal parameters merge exactly.
	SketchParameters SketchParameters `json:"sketch"`
	Windows          []Window         `json:"windows"`
}

// SketchParameters name the sketch's bucketing.
type SketchParameters struct {
	RelativeError float64 `json:"relativeError"`
	MinSeconds    float64 `json:"minSeconds"`
	MaxSeconds    float64 `json:"maxSeconds"`
}

// Window is one trailing window: its last N complete minutes and the
// current partial one, from Start to the report's GeneratedAt.
type Window struct {
	Window string    `json:"window"`
	Start  time.Time `json:"start"`
	// Exchanges are the series with any exchange in the window, by
	// direction and exchange kind; Backends the participant's own system's
	// calls, by exchange kind.
	Exchanges []ExchangeSeries `json:"exchanges"`
	Backends  []BackendSeries  `json:"backends"`
}

// ExchangeSeries is one direction and exchange kind over a window.
type ExchangeSeries struct {
	Direction string `json:"direction"`
	Exchange  string `json:"exchange"`
	Count     uint64 `json:"count"`
	// Rate is exchanges per second over the window's span.
	Rate     float64           `json:"rate"`
	Outcomes map[string]uint64 `json:"outcomes"`
	Latency  Latency           `json:"latency"`
}

// BackendSeries is one exchange kind's calls to the participant's own
// system over a window.
type BackendSeries struct {
	Exchange string  `json:"exchange"`
	Count    uint64  `json:"count"`
	Latency  Latency `json:"latency"`
}

// Latency is a window's latency in seconds: its quantiles, each within the
// sketch's relative error and absent when no duration was recorded, and the
// sketch they were read from.
type Latency struct {
	P50    *float64 `json:"p50,omitempty"`
	P95    *float64 `json:"p95,omitempty"`
	P99    *float64 `json:"p99,omitempty"`
	Max    *float64 `json:"max,omitempty"`
	Sketch Sketch   `json:"sketch"`
}

// latencyOf reads a sketch's quantiles.
func latencyOf(s Sketch) Latency {
	l := Latency{Sketch: s}
	if s.Count() == 0 {
		return l
	}
	q := func(p float64) *float64 { v, _ := s.Quantile(p); return &v }
	l.P50, l.P95, l.P99 = q(0.50), q(0.95), q(0.99)
	m := s.Max
	l.Max = &m
	return l
}

// Report reads every window now.
func (a *Aggregator) Report() Report {
	now := a.now()
	cur := now.Unix() / 60
	r := Report{Replica: a.replica, Since: a.since.UTC(), GeneratedAt: now.UTC(),
		SketchParameters: SketchParameters{RelativeError: RelativeError, MinSeconds: MinSeconds, MaxSeconds: MaxSeconds}}
	for _, w := range Windows {
		from := cur - int64(w.Minutes)
		start := time.Unix(from*60, 0).UTC()
		if start.Before(a.since) {
			start = a.since.UTC()
		}
		span := now.Sub(start).Seconds()
		win := Window{Window: w.Name, Start: start, Exchanges: []ExchangeSeries{}, Backends: []BackendSeries{}}
		for key, s := range a.exchanges {
			sm := a.sum(s, from, cur)
			if sm.count == 0 && sm.zero == 0 && sm.max == 0 {
				continue
			}
			es := ExchangeSeries{Direction: key[0], Exchange: key[1], Count: sm.count, Outcomes: map[string]uint64{},
				Latency: latencyOf(sparse(sm.zero, sm.dense, sm.max))}
			if span > 0 {
				es.Rate = float64(sm.count) / span
			}
			for j, o := range a.outcomes {
				if sm.outcomes[j] > 0 {
					es.Outcomes[o] = sm.outcomes[j]
				}
			}
			win.Exchanges = append(win.Exchanges, es)
		}
		for kind, s := range a.backends {
			sm := a.sum(s, from, cur)
			if sm.count == 0 && sm.zero == 0 && sm.max == 0 {
				continue
			}
			win.Backends = append(win.Backends, BackendSeries{Exchange: kind, Count: sm.count, Latency: latencyOf(sparse(sm.zero, sm.dense, sm.max))})
		}
		sort.Slice(win.Exchanges, func(i, j int) bool {
			if win.Exchanges[i].Direction != win.Exchanges[j].Direction {
				return win.Exchanges[i].Direction < win.Exchanges[j].Direction
			}
			return win.Exchanges[i].Exchange < win.Exchanges[j].Exchange
		})
		sort.Slice(win.Backends, func(i, j int) bool { return win.Backends[i].Exchange < win.Backends[j].Exchange })
		r.Windows = append(r.Windows, win)
	}
	return r
}

// Handler serves GET /internal/stats: the report, as JSON. The caller wraps
// it in /internal/checks' access rule.
func (a *Aggregator) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "method not allowed"})
			return
		}
		_ = json.NewEncoder(w).Encode(a.Report())
	})
}
