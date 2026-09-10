package smartauth

import (
	"context"
	"sync/atomic"
)

// TokenAcquisitionObservation records whether the bearer transport returned a
// token acquisition failure for a request carrying its context. It retains no
// error or payload. The zero value is unmarked; use a fresh handle for each Do.
// A handle must not be copied after first use.
type TokenAcquisitionObservation struct{ failed atomic.Bool }

// Failed reports an observed token acquisition failure, not an acquisition in
// progress, a timeout by itself, or a resource-request failure. It is safe to
// read concurrently; an observed failure remains true for this handle's lifetime.
func (o *TokenAcquisitionObservation) Failed() bool { return o.failed.Load() }

type tokenAcquisitionObservationKey struct{}

// WithTokenAcquisitionObservation derives a context with fresh request-scoped
// token acquisition evidence, shadowing any inherited observation while retaining
// the parent's values, deadline and cancellation. Pair the returned handle with a
// single client.Do; this evidence survives net/http replacing a timeout's error
// chain. Creating or canceling the context does not mark a token failure.
func WithTokenAcquisitionObservation(ctx context.Context) (context.Context, *TokenAcquisitionObservation) {
	observation := &TokenAcquisitionObservation{}
	return context.WithValue(ctx, tokenAcquisitionObservationKey{}, observation), observation
}
