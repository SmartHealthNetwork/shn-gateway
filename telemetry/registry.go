// Package telemetry is the gateway's one recording point for its metrics
// (MON-R16). Every call site records into a Registry, against an Instrument
// from the catalogue; the Registry folds each attribute value into its closed
// list (MON-R18) and hands the measurement to its sinks: the CloudWatch EMF
// sink (EMF), the OpenTelemetry sink built by package otelsetup, and any other
// Sink. No call site writes to a backend directly.
//
// The package imports no OpenTelemetry module: otelsetup is the only importer
// (TestOnlyOtelsetupImportsOpenTelemetry).
//
// Recording never blocks or fails a caller (MON-R12): a nil *Registry is a
// silent no-op, and a measurement that does not fit its instrument (a wrong
// kind or a wrong number of attribute values) is dropped.
package telemetry

import (
	"fmt"
	"log"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/recording"
)

// Other is the value an attribute records for anything its closed list does
// not name (MON-R18).
const Other = recording.ExchangeOther

// Kind is what an instrument measures, and how a sink aggregates it.
type Kind int

const (
	// Counter is a monotonic sum: Registry.Add with a non-negative value.
	Counter Kind = iota + 1
	// UpDownCounter is a sum that can go down: Registry.Add.
	UpDownCounter
	// Histogram is a distribution: Registry.Record, or Registry.RecordDuration
	// for an instrument in seconds.
	Histogram
	// Gauge is the latest value: Registry.Record.
	Gauge
	// CumulativeCounter is a monotonic sum the source keeps itself (a pool's
	// lifetime count): Registry.Record with the running total, never a delta.
	CumulativeCounter
	// Snapshot is a gauge whose whole set of series is replaced at each
	// sample: Registry.RecordSnapshot. A series the latest snapshot leaves
	// out is absent, never its last value (MON-R15), so it suits a value that
	// can stop being known (a certificate that ages out).
	Snapshot
)

// String names k as the catalogue does.
func (k Kind) String() string {
	switch k {
	case Counter, CumulativeCounter:
		return "counter"
	case UpDownCounter:
		return "updowncounter"
	case Histogram:
		return "histogram"
	case Gauge, Snapshot:
		return "gauge"
	}
	return "unknown"
}

// Attribute is one attribute key and its closed list of values. A value the
// list does not name records as Other, or as OtherValue when it is set.
type Attribute struct {
	Key    string
	Values []string
	// OtherValue, when set, is what a value outside the list records as, in
	// place of Other: OpenTelemetry's semantic conventions fold an unknown
	// value to _OTHER.
	OtherValue string
	// Int says an OpenTelemetry sink exports the value as an integer (an HTTP
	// status code). Values stay strings here; a value that is not an integer
	// (the fold value) is exported as the string it is.
	Int bool
	// Optional says the value "" records the attribute as absent: a sink
	// leaves the key out (a semantic convention's attribute that is set only
	// when it applies). "" is then one of the values it can record.
	Optional bool
}

// other is the value a value outside the list records as.
func (a Attribute) other() string {
	if a.OtherValue != "" {
		return a.OtherValue
	}
	return Other
}

// cardinality is how many values the attribute can record: its list, its
// fold value when the list does not already name it, and absence when it is
// Optional.
func (a Attribute) cardinality() int {
	n := len(a.Values)
	if !slices.Contains(a.Values, a.other()) {
		n++
	}
	if a.Optional && !slices.Contains(a.Values, "") {
		n++
	}
	return n
}

// fold is v when the list names it (or v is "" on an Optional attribute),
// else its fold value.
func (a Attribute) fold(v string) string {
	if slices.Contains(a.Values, v) || a.Optional && v == "" {
		return v
	}
	return a.other()
}

// Instrument is one metric in the catalogue (MON-R20). Its fields are fixed
// at declaration; a sink reads them, nothing writes them.
type Instrument struct {
	// Name is the OpenTelemetry instrument name (shn.gateway.*).
	Name string
	// Kind says how it aggregates.
	Kind Kind
	// Unit is the UCUM unit: s for durations, By for sizes, an annotation
	// such as {exchange} for a count, empty for a unitless gauge.
	Unit string
	// Description is one sentence for the exported metadata.
	Description string
	// Attributes are its closed attributes, in the order a call site passes
	// their values.
	Attributes []Attribute
	// Buckets are a histogram's explicit bucket boundaries when explicit
	// buckets are selected (DurationBuckets, SizeBuckets).
	Buckets []float64
	// Bound, when set, is the instrument's series bound in place of the
	// product of its attributes' cardinalities: for attributes that are not
	// independent (an HTTP error.type is its status code's), the product
	// overstates what it can record. A test computes it from the closed
	// lists and pins it.
	Bound int
}

// SeriesBound is the most distinct attribute sets the instrument can record:
// Bound when set, else the product of its attributes' cardinalities
// (MON-R18). A sink's cardinality limit is set from it.
func (i *Instrument) SeriesBound() int {
	if i.Bound > 0 {
		return i.Bound
	}
	n := 1
	for _, a := range i.Attributes {
		n *= a.cardinality()
	}
	return n
}

// AttributeKeys are its attribute keys, in call-site order.
func (i *Instrument) AttributeKeys() []string {
	keys := make([]string, len(i.Attributes))
	for n, a := range i.Attributes {
		keys[n] = a.Key
	}
	return keys
}

// DurationBuckets are the default explicit buckets for a duration, in
// seconds: they reach past the 25 s backend deadline and the 30 s leg budget.
var DurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 15, 20, 25, 30, 60}

// SizeBuckets are the default explicit buckets for a body size, in bytes:
// 1 KiB to 64 MiB in powers of 4.
var SizeBuckets = []float64{1 << 10, 1 << 12, 1 << 14, 1 << 16, 1 << 18, 1 << 20, 1 << 22, 1 << 24, 1 << 26}

// Measurement is one recorded value, its attributes already folded into
// their closed lists.
type Measurement struct {
	Instrument *Instrument
	// Value is the value in the instrument's unit: seconds for a duration.
	Value float64
	// Duration is set, and Value is its seconds, when it was recorded by
	// RecordDuration; a sink that reports milliseconds (EMF) reads it here so
	// it truncates exactly as before.
	Duration time.Duration
	// IsDuration says Duration is set.
	IsDuration bool
	// Attributes are the values, one per Instrument.Attributes entry.
	Attributes []string
}

// Sink receives every measurement a Registry accepts. Record must not block
// or fail its caller.
type Sink interface {
	Record(Measurement)
}

// SnapshotSink is a Sink that takes a Snapshot instrument's whole set of
// series at once, in place of the set it took last. A Sink that is not one
// receives each series of a snapshot through Record. RecordSnapshot must not
// block or fail its caller, and it owns the slice it is handed.
type SnapshotSink interface {
	Sink
	RecordSnapshot(i *Instrument, series []Measurement)
}

// Point is one series of a snapshot: its value, in the instrument's unit,
// and its attribute values, in call-site order.
type Point struct {
	Value      float64
	Attributes []string
}

// Registry is the one recording point. Its sinks are fixed at New. Safe for
// concurrent use when its sinks are.
type Registry struct {
	sinks []Sink
}

// New returns a Registry recording into sinks; nil sinks are skipped. With
// none it records nothing.
func New(sinks ...Sink) *Registry {
	r := &Registry{}
	for _, s := range sinks {
		if s != nil {
			r.sinks = append(r.sinks, s)
		}
	}
	return r
}

// Add adds v to a Counter or UpDownCounter. A Counter refuses a negative v.
func (r *Registry) Add(i *Instrument, v float64, attrs ...string) {
	if i == nil || (i.Kind != Counter && i.Kind != UpDownCounter) || (i.Kind == Counter && v < 0) {
		return
	}
	r.record(i, Measurement{Value: v}, attrs)
}

// Record records v on a Histogram, a Gauge or a CumulativeCounter (whose v
// is the running total).
func (r *Registry) Record(i *Instrument, v float64, attrs ...string) {
	if i == nil || (i.Kind != Histogram && i.Kind != Gauge && i.Kind != CumulativeCounter) {
		return
	}
	r.record(i, Measurement{Value: v}, attrs)
}

// RecordDuration records d on a Histogram in seconds.
func (r *Registry) RecordDuration(i *Instrument, d time.Duration, attrs ...string) {
	if i == nil || i.Kind != Histogram || i.Unit != "s" {
		return
	}
	r.record(i, Measurement{Value: d.Seconds(), Duration: d, IsDuration: true}, attrs)
}

// RecordSnapshot replaces a Snapshot instrument's series with points; no
// points clears them all. Each point's attributes fold into their closed
// lists, so a snapshot never holds more series than the instrument's
// SeriesBound. Points that fold into one series report the least of their
// values (the soonest of a kind's certificates). The series are sorted by
// their attribute values, so what a sink sees never depends on the order
// the points came in. A point with the wrong number of attribute values, or
// a value that is not finite, is dropped.
func (r *Registry) RecordSnapshot(i *Instrument, points []Point) {
	if r == nil || len(r.sinks) == 0 || i == nil || i.Kind != Snapshot {
		return
	}
	index := map[string]int{}
	var series []Measurement
	for _, p := range points {
		if len(p.Attributes) != len(i.Attributes) || math.IsNaN(p.Value) || math.IsInf(p.Value, 0) {
			continue
		}
		folded := make([]string, len(p.Attributes))
		for n, a := range i.Attributes {
			folded[n] = a.fold(p.Attributes[n])
		}
		key := strings.Join(folded, "\x00")
		if n, ok := index[key]; ok {
			series[n].Value = min(series[n].Value, p.Value)
			continue
		}
		index[key] = len(series)
		series = append(series, Measurement{Instrument: i, Value: p.Value, Attributes: folded})
	}
	slices.SortFunc(series, func(a, b Measurement) int { return slices.Compare(a.Attributes, b.Attributes) })
	for _, s := range r.sinks {
		recordSnapshot(s, i, slices.Clone(series))
	}
}

// recordSnapshot hands series to s, whole when s is a SnapshotSink and one
// by one otherwise, recovering a panic as record does (MON-R12).
func recordSnapshot(s Sink, i *Instrument, series []Measurement) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("gateway: telemetry: a metrics sink panicked recording %s: %q", i.Name, fmt.Sprint(p))
		}
	}()
	if ss, ok := s.(SnapshotSink); ok {
		ss.RecordSnapshot(i, series)
		return
	}
	for _, m := range series {
		s.Record(m)
	}
}

func (r *Registry) record(i *Instrument, m Measurement, attrs []string) {
	if r == nil || len(r.sinks) == 0 || len(attrs) != len(i.Attributes) {
		return
	}
	folded := make([]string, len(attrs))
	for n, a := range i.Attributes {
		folded[n] = a.fold(attrs[n])
	}
	m.Instrument, m.Attributes = i, folded
	for _, s := range r.sinks {
		record(s, m)
	}
}

// record hands m to s, recovering a panic so that neither the caller nor the
// sinks after s are affected (MON-R12).
func record(s Sink, m Measurement) {
	defer func() {
		if p := recover(); p != nil {
			// The panic's value, quoted so nothing it carries can start a
			// line of its own; never a measurement's values.
			log.Printf("gateway: telemetry: a metrics sink panicked recording %s: %q", m.Instrument.Name, fmt.Sprint(p))
		}
	}()
	s.Record(m)
}
