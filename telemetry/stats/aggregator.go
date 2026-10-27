package stats

import (
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/telemetry"
)

// ringSlots is the ring's length: 60 complete minutes for the 1h window plus
// the current, partial one.
const ringSlots = 61

// Windows are the windows /internal/stats reports, in minutes: each is the
// last N complete one-minute slots plus the current partial one.
var Windows = []struct {
	Name    string
	Minutes int
}{{"1m", 1}, {"5m", 5}, {"1h", 60}}

// slot is one minute of one series: written with atomic adds while its
// series' read lock is held, reset under the write lock when its minute
// comes round again.
type slot struct {
	minute   int64 // the minute it holds (unix seconds / 60)
	count    atomic.Uint64
	outcomes []atomic.Uint64 // exchange series: by outcome, in outcome order
	latency  *sketch
}

// series is one ring: an exchange series is one direction and exchange
// kind, counted by outcome with the exchange's latency; a backend series is
// one exchange kind's calls to the participant's own system, with their
// latency.
type series struct {
	mu    sync.RWMutex
	slots [ringSlots]*slot
}

// Aggregator is the stats sink. Recording takes a series' read lock and
// adds atomically, so recordings never wait on each other, except briefly
// when the first recording of a new minute in a series takes the write lock
// to reset that slot (MON-R12). Its memory is bounded by the closed lists
// (MON-R31).
type Aggregator struct {
	now      func() time.Time
	since    time.Time
	replica  string
	outcomes []string

	exchanges map[[2]string]*series // direction, exchange
	backends  map[string]*series    // exchange
}

// New returns an aggregator for the replica named replica
// (service.instance.id), its windows read from now.
func New(replica string, now func() time.Time) *Aggregator {
	if now == nil {
		now = time.Now
	}
	a := &Aggregator{now: now, since: now(), replica: replica, exchanges: map[[2]string]*series{}, backends: map[string]*series{}}
	directions := values(telemetry.Exchanges.Attributes[0])
	kinds := values(telemetry.Exchanges.Attributes[1])
	a.outcomes = values(telemetry.Exchanges.Attributes[2])
	for _, d := range directions {
		for _, k := range kinds {
			a.exchanges[[2]string{d, k}] = &series{}
		}
	}
	for _, k := range kinds {
		a.backends[k] = &series{}
	}
	return a
}

// values are an attribute's closed values with Other, as the registry folds
// them.
func values(attr telemetry.Attribute) []string {
	out := slices.Clone(attr.Values)
	if !slices.Contains(out, telemetry.Other) {
		out = append(out, telemetry.Other)
	}
	return out
}

// Record implements telemetry.Sink: the exchange and backend instruments
// feed it, every other instrument is ignored.
func (a *Aggregator) Record(m telemetry.Measurement) {
	switch m.Instrument {
	case telemetry.Exchanges:
		s := a.exchanges[[2]string{m.Attributes[0], m.Attributes[1]}]
		if s == nil {
			return
		}
		a.write(s, func(sl *slot) {
			sl.count.Add(uint64(m.Value))
			if i := slices.Index(a.outcomes, m.Attributes[2]); i >= 0 {
				sl.outcomes[i].Add(uint64(m.Value))
			}
		})
	case telemetry.ExchangeDuration:
		if s := a.exchanges[[2]string{m.Attributes[0], m.Attributes[1]}]; s != nil {
			a.write(s, func(sl *slot) { sl.latency.record(m.Value) })
		}
	case telemetry.BackendCalls:
		if s := a.backends[m.Attributes[0]]; s != nil {
			a.write(s, func(sl *slot) { sl.count.Add(uint64(m.Value)) })
		}
	case telemetry.BackendDuration:
		if s := a.backends[m.Attributes[0]]; s != nil {
			a.write(s, func(sl *slot) { sl.latency.record(m.Value) })
		}
	}
}

// write runs add on s's slot for the current minute, resetting the slot
// first when it last held an older minute.
func (a *Aggregator) write(s *series, add func(*slot)) {
	minute := a.now().Unix() / 60
	i := int((minute%ringSlots + ringSlots) % ringSlots)
	if a.fast(s, i, minute, add) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sl := s.slots[i]
	switch {
	case sl == nil:
		sl = &slot{outcomes: make([]atomic.Uint64, len(a.outcomes)), latency: newSketch()}
		s.slots[i] = sl
	case sl.minute != minute:
		sl.count.Store(0)
		for j := range sl.outcomes {
			sl.outcomes[j].Store(0)
		}
		sl.latency.reset()
	}
	sl.minute = minute
	add(sl)
}

// fast runs add on slot i under the read lock when it already holds minute.
func (a *Aggregator) fast(s *series, i int, minute int64, add func(*slot)) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sl := s.slots[i]
	if sl == nil || sl.minute != minute {
		return false
	}
	add(sl)
	return true
}

// sum is a series over the minutes [from, to].
type sum struct {
	count    uint64
	outcomes []uint64
	zero     uint64
	dense    []uint64
	max      float64
}

func (a *Aggregator) sum(s *series, from, to int64) sum {
	out := sum{outcomes: make([]uint64, len(a.outcomes)), dense: make([]uint64, bucketCount)}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sl := range s.slots {
		if sl == nil || sl.minute < from || sl.minute > to {
			continue
		}
		out.count += sl.count.Load()
		for j := range sl.outcomes {
			out.outcomes[j] += sl.outcomes[j].Load()
		}
		sl.latency.addTo(&out.zero, out.dense, &out.max)
	}
	return out
}
