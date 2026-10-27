package otelsetup

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/SmartHealthNetwork/shn-gateway/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// sink records the registry's measurements on OpenTelemetry instruments, one
// per catalogue instrument, created once from the provider's meter.
type sink struct {
	record   map[*telemetry.Instrument]func(float64, attribute.Set)
	snapshot map[*telemetry.Instrument]*atomic.Pointer[[]observation]
	sets     sync.Map // string key → attribute.Set, at most each instrument's SeriesBound
}

// observation is one series of a Snapshot instrument's latest snapshot.
type observation struct {
	set attribute.Set
	v   float64
}

// newSink creates every instrument of the catalogue on meter.
func newSink(meter metric.Meter, instruments []*telemetry.Instrument) (*sink, error) {
	s := &sink{record: map[*telemetry.Instrument]func(float64, attribute.Set){}, snapshot: map[*telemetry.Instrument]*atomic.Pointer[[]observation]{}}
	for _, inst := range instruments {
		desc, unit := metric.WithDescription(inst.Description), metric.WithUnit(inst.Unit)
		switch inst.Kind {
		case telemetry.Counter:
			c, err := meter.Float64Counter(inst.Name, desc, unit)
			if err != nil {
				return nil, err
			}
			s.record[inst] = func(v float64, set attribute.Set) { c.Add(context.Background(), v, metric.WithAttributeSet(set)) }
		case telemetry.UpDownCounter:
			c, err := meter.Float64UpDownCounter(inst.Name, desc, unit)
			if err != nil {
				return nil, err
			}
			s.record[inst] = func(v float64, set attribute.Set) { c.Add(context.Background(), v, metric.WithAttributeSet(set)) }
		case telemetry.Histogram:
			h, err := meter.Float64Histogram(inst.Name, desc, unit)
			if err != nil {
				return nil, err
			}
			s.record[inst] = func(v float64, set attribute.Set) { h.Record(context.Background(), v, metric.WithAttributeSet(set)) }
		case telemetry.Gauge:
			g, err := meter.Float64Gauge(inst.Name, desc, unit)
			if err != nil {
				return nil, err
			}
			s.record[inst] = func(v float64, set attribute.Set) { g.Record(context.Background(), v, metric.WithAttributeSet(set)) }
		case telemetry.Snapshot:
			// Observed from the latest snapshot, swapped in whole: a series
			// the snapshot leaves out is not observed, so the SDK drops it
			// (MON-R15) where a synchronous gauge would export its last value
			// for good. The callback only loads a pointer, so it never waits
			// on a snapshot being recorded (MON-R12).
			latest := &atomic.Pointer[[]observation]{}
			if _, err := meter.Float64ObservableGauge(inst.Name, desc, unit, metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
				if obs := latest.Load(); obs != nil {
					for _, l := range *obs {
						o.Observe(l.v, metric.WithAttributeSet(l.set))
					}
				}
				return nil
			})); err != nil {
				return nil, err
			}
			s.snapshot[inst] = latest
		case telemetry.CumulativeCounter:
			// The source keeps the running total, so it is observed: each
			// collection reports the latest total recorded per attribute set.
			var mu sync.Mutex
			latest := map[attribute.Distinct]struct {
				set attribute.Set
				v   float64
			}{}
			if _, err := meter.Float64ObservableCounter(inst.Name, desc, unit, metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
				mu.Lock()
				defer mu.Unlock()
				for _, l := range latest {
					o.Observe(l.v, metric.WithAttributeSet(l.set))
				}
				return nil
			})); err != nil {
				return nil, err
			}
			s.record[inst] = func(v float64, set attribute.Set) {
				mu.Lock()
				defer mu.Unlock()
				latest[set.Equivalent()] = struct {
					set attribute.Set
					v   float64
				}{set, v}
			}
		}
	}
	return s, nil
}

// Record implements telemetry.Sink.
func (s *sink) Record(m telemetry.Measurement) {
	rec := s.record[m.Instrument]
	if rec == nil {
		return
	}
	rec(m.Value, s.set(m))
}

// RecordSnapshot implements telemetry.SnapshotSink: series replaces the
// instrument's last snapshot.
func (s *sink) RecordSnapshot(i *telemetry.Instrument, series []telemetry.Measurement) {
	latest := s.snapshot[i]
	if latest == nil {
		return
	}
	obs := make([]observation, len(series))
	for n, m := range series {
		obs[n] = observation{set: s.set(m), v: m.Value}
	}
	latest.Store(&obs)
}

// set is m's attribute set, built once per distinct value combination: the
// values are closed, so the cache holds at most each instrument's bound.
func (s *sink) set(m telemetry.Measurement) attribute.Set {
	if len(m.Attributes) == 0 {
		return *attribute.EmptySet()
	}
	key := m.Instrument.Name + "\x00" + strings.Join(m.Attributes, "\x00")
	if v, ok := s.sets.Load(key); ok {
		return v.(attribute.Set)
	}
	kvs := make([]attribute.KeyValue, 0, len(m.Attributes))
	for n, a := range m.Instrument.Attributes {
		v := m.Attributes[n]
		switch {
		case a.Optional && v == "":
			// Absent: the attribute applies only when it has a value.
		case a.Int:
			if i, err := strconv.ParseInt(v, 10, 64); err == nil {
				kvs = append(kvs, attribute.Int64(a.Key, i))
			} else {
				kvs = append(kvs, attribute.String(a.Key, v))
			}
		default:
			kvs = append(kvs, attribute.String(a.Key, v))
		}
	}
	set := attribute.NewSet(kvs...)
	s.sets.Store(key, set)
	return set
}
