package backoff

import (
	"testing"
	"time"
)

func TestExponentialDoublesToItsCap(t *testing.T) {
	e := Exponential{Base: 15 * time.Second, Max: 5 * time.Minute}
	for failures, want := range map[int]time.Duration{1: 15 * time.Second, 2: 30 * time.Second, 3: time.Minute, 5: 4 * time.Minute, 6: 5 * time.Minute, 40: 5 * time.Minute} {
		if got := e.Wait(failures); got != want {
			t.Errorf("Wait(%d) = %v, want %v", failures, got, want)
		}
	}
}

// Jitter spreads callers that failed together over the upper half of the
// step, so they do not retry in lockstep.
func TestExponentialJitterSpreadsLockstepRetries(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 10; i++ {
		u := float64(i) / 10
		w := Exponential{Base: 15 * time.Second, Max: 5 * time.Minute, Jitter: func() float64 { return u }}.Wait(2)
		if w < 15*time.Second || w > 30*time.Second {
			t.Fatalf("jittered wait %v outside [15s, 30s]", w)
		}
		seen[w] = true
	}
	if len(seen) != 10 {
		t.Fatalf("ten jitter draws gave %d distinct waits", len(seen))
	}
}
