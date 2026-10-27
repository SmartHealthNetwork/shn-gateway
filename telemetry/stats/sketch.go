// Package stats is the gateway's built-in per-exchange summary (MON-R30): a
// telemetry.Sink that keeps rolling one-minute slots of counts and latency
// sketches for each direction and exchange kind, and the /internal/stats
// handler that reports them over 1m, 5m and 1h windows. Each window carries
// its mergeable sketch, so answers from several replicas combine exactly.
// Nothing per peer is kept (MON-R19).
package stats

import (
	"math"
	"sync/atomic"
)

// The sketch is log-bucketed: a value v in (minSeconds, maxSeconds] falls in
// bucket ceil(log(v)/log(gamma)), and a quantile read from bucket i is
// 2·gamma^i/(gamma+1), within relativeError of every value the bucket holds
// (MON-R31). Two sketches with the same parameters merge by adding buckets.
const (
	// RelativeError bounds a quantile's error relative to the true value.
	RelativeError = 0.02
	// MinSeconds is the smallest value the sketch resolves: a value at or
	// below it counts in the zero bucket and reads as 0.
	MinSeconds = 1e-4
	// MaxSeconds is the largest value it resolves: a larger one counts in
	// the top bucket and reads as that bucket's value (about 299 s), and the
	// window's max still states it exactly.
	MaxSeconds = 300.0
)

var (
	gamma    = (1 + RelativeError) / (1 - RelativeError)
	logGamma = math.Log(gamma)
	// minIndex and maxIndex are the buckets of MinSeconds and MaxSeconds.
	minIndex = int(math.Ceil(math.Log(MinSeconds) / logGamma))
	maxIndex = int(math.Ceil(math.Log(MaxSeconds) / logGamma))
	// bucketCount is how many buckets a sketch holds.
	bucketCount = maxIndex - minIndex + 1
)

// bucketOf is the bucket index of v, a duration in seconds; ok is false for
// the zero bucket.
func bucketOf(v float64) (index int, ok bool) {
	if !(v > MinSeconds) {
		return 0, false
	}
	i := int(math.Ceil(math.Log(v) / logGamma))
	if i > maxIndex {
		i = maxIndex
	}
	if i < minIndex {
		i = minIndex
	}
	return i, true
}

// valueOf is the value bucket i reads as.
func valueOf(i int) float64 {
	return 2 * math.Pow(gamma, float64(i)) / (gamma + 1)
}

// sketch is one slot's latency sketch, recorded into with atomic adds.
type sketch struct {
	zero    atomic.Uint64
	buckets []atomic.Uint32 // bucketCount of them
	maxBits atomic.Uint64   // the largest value, as float64 bits
}

func newSketch() *sketch { return &sketch{buckets: make([]atomic.Uint32, bucketCount)} }

func (s *sketch) record(v float64) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return // not a duration
	}
	if i, ok := bucketOf(v); ok {
		s.buckets[i-minIndex].Add(1)
	} else {
		s.zero.Add(1)
	}
	for {
		old := s.maxBits.Load()
		if math.Float64frombits(old) >= v || s.maxBits.CompareAndSwap(old, math.Float64bits(v)) {
			return
		}
	}
}

func (s *sketch) reset() {
	s.zero.Store(0)
	for i := range bucketCount {
		s.buckets[i].Store(0)
	}
	s.maxBits.Store(0)
}

// Sketch is a sketch as /internal/stats reports it, and as a reader merges
// several: the zero bucket's count, each non-empty bucket as [index, count]
// (ascending; an index can be negative), and the largest value recorded.
// Its bucketing is the report's SketchParameters: sketches merge only when
// those are equal, and a reader must not merge them otherwise.
type Sketch struct {
	Zero    uint64     `json:"zero"`
	Buckets [][2]int64 `json:"buckets"`
	Max     float64    `json:"max"`
}

// addTo adds s's counts into the dense buckets and max.
func (s *sketch) addTo(zero *uint64, dense []uint64, max *float64) {
	*zero += s.zero.Load()
	for i := range bucketCount {
		dense[i] += uint64(s.buckets[i].Load())
	}
	if m := math.Float64frombits(s.maxBits.Load()); m > *max {
		*max = m
	}
}

// sparse is the reported form of dense buckets.
func sparse(zero uint64, dense []uint64, max float64) Sketch {
	out := Sketch{Zero: zero, Buckets: [][2]int64{}, Max: max}
	for i, n := range dense {
		if n > 0 {
			out.Buckets = append(out.Buckets, [2]int64{int64(i + minIndex), int64(n)})
		}
	}
	return out
}

// Count is how many values the sketch holds.
func (s Sketch) Count() uint64 {
	n := s.Zero
	for _, b := range s.Buckets {
		n += uint64(b[1])
	}
	return n
}

// Quantile is the q-quantile (0 ≤ q ≤ 1) within RelativeError, never above
// Max; ok is false for an empty sketch, which has no quantile. It reads the
// nearest rank: the first bucket, in index order, at which the running count
// reaches q·n, the bucket Prometheus' histogram_quantile reads a classic
// histogram's estimate from (for a native histogram at q ≥ 0.5 it can read
// the next value up: where (1−q)·n comes out whole, always at the median).
func (s Sketch) Quantile(q float64) (v float64, ok bool) {
	n := s.Count()
	if n == 0 {
		return 0, false
	}
	rank := q * float64(n)
	seen := s.Zero
	if seen > 0 && float64(seen) >= rank {
		return 0, true
	}
	for _, b := range s.Buckets {
		seen += uint64(b[1])
		if float64(seen) >= rank {
			return math.Min(valueOf(int(b[0])), s.Max), true
		}
	}
	return s.Max, true
}
