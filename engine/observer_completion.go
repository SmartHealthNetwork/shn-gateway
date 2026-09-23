package engine

import (
	"context"
	"sync"
)

// operationTracker retains only live HTTP operations; completion is observational.
// Its zero value is ready for use.
type operationTracker struct {
	mu      sync.Mutex
	next    uint64
	pending map[uint64]struct{}
	changed chan struct{}
}

func (o *operationTracker) begin() func() {
	o.mu.Lock()
	if o.pending == nil {
		o.pending = make(map[uint64]struct{})
	}
	if o.changed == nil {
		o.changed = make(chan struct{})
	}
	o.next++
	ticket := o.next
	o.pending[ticket] = struct{}{}
	o.mu.Unlock()
	return func() {
		o.mu.Lock()
		defer o.mu.Unlock()
		if _, ok := o.pending[ticket]; ok {
			delete(o.pending, ticket)
			close(o.changed)
			o.changed = make(chan struct{})
		}
	}
}
func (o *operationTracker) wait(ctx context.Context) error {
	o.mu.Lock()
	cutoff := o.next
	for {
		pending := false
		for ticket := range o.pending {
			if ticket <= cutoff {
				pending = true
				break
			}
		}
		if !pending {
			o.mu.Unlock()
			return ctx.Err()
		}
		changed := o.changed
		o.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
		o.mu.Lock()
	}
}

// WaitObserverCompletion waits for entered HTTP operations and their accepted
// certification observations, including observer callback delivery. It does not
// certify payloads or change routing/readiness. Direct OriginateLeg calls and
// requests that have not entered Handler are outside the operation snapshot.
func (g *Gateway) WaitObserverCompletion(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := g.operations.wait(ctx); err != nil {
		return err
	}
	if err := g.waitCertification(ctx); err != nil {
		return err
	}
	return g.waitObserver(ctx)
}
