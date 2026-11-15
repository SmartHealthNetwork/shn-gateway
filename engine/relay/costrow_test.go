package relay

import (
	"math"
	"runtime"
	"runtime/debug"
	"testing"
	"time"
)

// A cost row times a check on a large input and on one costFactor times
// smaller. A check linear in the input takes about costFactor times as long
// on the large one, a check quadratic in it about costFactor squared; the
// bound sits at their geometric middle, costFactor^1.5, a factor of
// √costFactor from each:
//   - on the linear side that factor, 16, covers the processor's caches: the
//     small input fits in them and the large one does not, which makes each
//     step of a check on it several times slower on some machines (4.75 times
//     on a release runner, some 6 times in a 2-processor Linux container),
//     and a log factor;
//   - on the quadratic side it is the margin, 16, by which a check comparing
//     every part of the input with every other still fails.
//
// The price of that margin: a check partly linear and partly quadratic fails
// a row only once its quadratic part is some 16 times its linear part on the
// large input. A bound nearer the linear ratio would sit within the caches'
// effect: with a gap of 16 times and a bound of 64, a margin of 4 times on
// the linear side, these rows failed on a release runner at 74 and 76 times
// though their checks are linear. So each row's large input is as
// large as the gateway's body limits (splice.DefaultLimits) comfortably admit,
// which leaves a quadratic part the most room to show.
//
// The times hold on a shared runner, with every package of the module tested
// at once, under -race and -cover:
//   - they are the process's processor time (cpuTime), not the wall clock, so
//     the time the process waits for a processor is not counted, and garbage
//     collection is off while they are taken;
//   - a timed run calls the check back to back, k times, k set from the
//     faster of two first calls so that the run lasts about costRun, so the
//     clock's resolution is a small part of it;
//   - the two sizes' runs alternate, and each size's time is the best of
//     costTries runs, each after a collection.
const (
	costFactor = 256
	costRun    = 25 * time.Millisecond
	costTries  = 7
)

// costBound is costFactor^1.5.
var costBound = math.Pow(costFactor, 1.5)

// timedCall is a check's call on one input, k times back to back in each
// timed run.
type timedCall struct {
	call func()
	k    int
}

// newTimedCall sets k from the faster of two calls, so that a run of k calls
// lasts about costRun (the first calls, with collection on, may be slower).
func newTimedCall(call func()) timedCall {
	once := time.Duration(math.MaxInt64)
	for range 2 {
		start := cpuTime()
		call()
		once = min(once, cpuTime()-start)
	}
	return timedCall{call: call, k: int(costRun/max(once, time.Microsecond)) + 1}
}

// run is one call's time in a timed run of k calls, after a collection.
func (c timedCall) run() time.Duration {
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	start := cpuTime()
	for range c.k {
		c.call()
	}
	return (cpuTime() - start) / time.Duration(c.k)
}

// wantLinearCost fails t when one call of a check on an input of large takes
// more than costBound times one on an input of large/costFactor. input(n)
// builds an input of n, checks the call's result on it once, and returns the
// call; a check that keeps what it decides builds its state in the call.
func wantLinearCost(t *testing.T, what string, large int, input func(n int) func()) {
	t.Helper()
	sizes := [2]timedCall{newTimedCall(input(large / costFactor)), newTimedCall(input(large))}
	best := [2]time.Duration{math.MaxInt64, math.MaxInt64}
	for range costTries {
		for i, c := range sizes {
			best[i] = min(best[i], c.run())
		}
	}
	ratio := float64(best[1]) / float64(best[0])
	t.Logf("%s: %s, then %s at %d times the input (%d and %d calls a run): %.0f times the time (bound %.0f)", what, best[0], best[1], costFactor, sizes[0].k, sizes[1].k, ratio, costBound)
	if ratio > costBound {
		t.Errorf("%s: %s, then %s at %d times the input: %.0f times the time, past %.0f", what, best[0], best[1], costFactor, ratio, costBound)
	}
}

// eachAt is, for each element of the array at path, the node rest names
// from it. A cost row builds its edits from it: at resolves a whole path for
// each edit, and reads an array's elements and an object's members in turn,
// so a row that called it once per edit would spend time quadratic in its
// input building it.
func eachAt(t *testing.T, d *Document, path []string, rest ...string) []NodeID {
	t.Helper()
	els := d.Elems(at(t, d, path...))
	for i, el := range els {
		els[i] = atFrom(t, d, el, rest...)
	}
	return els
}
