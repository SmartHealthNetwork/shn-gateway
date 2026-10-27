// Package backoff is the retry schedule gateway callers share: each
// failure in a row doubles the wait, up to a cap, and jitter spreads the
// waits of callers that failed together so they do not retry together. Each
// caller sets its own parameters and owns its timer and clock.
package backoff

import "time"

// Exponential waits Base after the first failure, doubling with each further
// one up to Max. Jitter, when set, returns a value in [0, 1) and draws each
// wait from the upper half of its step, [step/2, step]; a caller seeds its
// source so tests are deterministic. A nil Jitter waits the whole step. Base
// and Max must be positive: a caller with defaults applies them first.
type Exponential struct {
	Base, Max time.Duration
	Jitter    func() float64
}

// Wait is how long to wait after failures consecutive failures (at least 1).
func (e Exponential) Wait(failures int) time.Duration {
	step := e.Base
	for i := 1; i < failures && step < e.Max; i++ {
		step *= 2
	}
	if step > e.Max {
		step = e.Max
	}
	if e.Jitter == nil {
		return step
	}
	return step/2 + time.Duration(e.Jitter()*float64(step/2))
}
