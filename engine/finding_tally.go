package engine

import (
	"context"
	"sync"
)

// findingBinding is what ties a captured finding to its call, the same fields
// the gateway's other captured records carry: the door call id on a provider,
// the leg's ciphertext hash, sender and recipient on a payer. It is taken from
// the request's context when the check runs, or when an observe check is
// queued, never when the finding is emitted.
type findingBinding struct {
	callID, hash, sender, recipient string
}

type findingBindingKey struct{}

func withFindingBinding(ctx context.Context, b findingBinding) context.Context {
	return context.WithValue(ctx, findingBindingKey{}, b)
}

// legTally follows one recorded leg's findings at observe, where checks run
// off the request path: once the leg has ended and none of its checks is still
// queued or running, the gateway captures one result event with the leg's
// finding count, so zero finding events reads as a clean leg only when the
// result says so.
type legTally struct {
	mu         sync.Mutex
	pending    int
	ended      bool
	done       bool
	count      int
	emitted    int
	truncated  bool
	incomplete bool
	rec        ExchangeRecord
}

type legTallyKey struct{}

func withLegTally(ctx context.Context, t *legTally) context.Context {
	if t == nil {
		return ctx
	}
	return context.WithValue(ctx, legTallyKey{}, t)
}

// legTallyFrom is the tally of the leg ctx belongs to: an observe worker's
// own, or the recorded call's.
func legTallyFrom(ctx context.Context) *legTally {
	if t, ok := ctx.Value(legTallyKey{}).(*legTally); ok {
		return t
	}
	if x := exchangeOf(ctx); x != nil {
		return x.tally
	}
	return nil
}

// hold notes a check queued for this leg; release, that it has run.
func (t *legTally) hold() {
	t.mu.Lock()
	t.pending++
	t.mu.Unlock()
}

func (t *legTally) release(g *Gateway) {
	t.mu.Lock()
	t.pending--
	g.finishLeg(t)
}

// dropped notes a check of this leg whose finding was not recorded, because it
// was dropped unrun or failed while running: its result is incomplete.
func (t *legTally) dropped() {
	t.mu.Lock()
	t.incomplete = true
	t.mu.Unlock()
}

// finding counts one finding and says whether it may still be captured as its
// own event (the per-leg cap); past the cap the result says truncated.
func (t *legTally) finding() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.count++
	if t.emitted >= findingEventsPerLeg {
		t.truncated = true
		return false
	}
	t.emitted++
	return true
}

// end notes the leg's call has ended, with its record.
func (t *legTally) end(g *Gateway, rec ExchangeRecord) {
	t.mu.Lock()
	t.ended, t.rec = true, rec
	g.finishLeg(t)
}

// finishLeg is called with t.mu held and releases it: once the leg has ended
// and no check of it is outstanding, its result is captured, once.
func (g *Gateway) finishLeg(t *legTally) {
	if !t.ended || t.pending > 0 || t.done {
		t.mu.Unlock()
		return
	}
	t.done = true
	rec, count, truncated, incomplete := t.rec, t.count, t.truncated, t.incomplete
	t.mu.Unlock()
	g.diagnosticLegResult(rec, count, truncated, incomplete)
}
