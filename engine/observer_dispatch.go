package engine

import (
	"bytes"
	"context"
	"errors"
	"sync"
)

const observationMessageLimit = 8 << 20
const observationBodyBudget = 32 << 20
const observationNotificationCapacity = 32
const maxObserverProofQueueCapacity = 384

// SetObserverQueueCapacityForTest reserves a bounded, finite proof stream before
// gateway construction. The deployed app keeps the 32-notification default.
// The body-byte budget and loss-reporting completion barrier remain unchanged.
func SetObserverQueueCapacityForTest(cfg *Config, capacity int) error {
	if cfg == nil || cfg.Observer == nil || capacity <= observationNotificationCapacity || capacity > maxObserverProofQueueCapacity {
		return errors.New("unsupported observer proof queue capacity")
	}
	cfg.observerQueueCapacityForTest = capacity
	return nil
}

func (g *Gateway) observerQueueCapacity() int {
	if g.cfg.observerQueueCapacityForTest != 0 {
		return g.cfg.observerQueueCapacityForTest
	}
	return observationNotificationCapacity
}

// The budget covers all gateway-owned queued and in-flight observation copies,
// including the explicitly enabled participant inspection stream at none.
type observationBudget struct {
	mu    sync.Mutex
	bytes int
}

func (b *observationBudget) reserve(n int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n < 0 || n > observationBodyBudget-b.bytes {
		return false
	}
	b.bytes += n
	return true
}
func (b *observationBudget) release(n int) { b.mu.Lock(); b.bytes -= n; b.mu.Unlock() }

type observerNotification struct {
	event    ObserverEvent
	size     int
	sequence uint64
}
type observerDispatcher struct {
	once                                 sync.Once
	mu                                   sync.Mutex
	queue                                chan observerNotification
	stop                                 chan struct{}
	changed                              chan struct{}
	closed                               bool
	accepted, completed, dropped, panics uint64
}

// Only one dispatcher exists per gateway. A callback must return promptly and
// must not retain or mutate its immutable snapshot. A hostile blocked callback
// can strand this one goroutine and its bounded snapshot until process exit;
// it cannot strand request handlers or trigger replacement dispatchers.
func (g *Gateway) enqueueObserver(e ObserverEvent) {
	d := &g.observerDispatch
	d.once.Do(func() {
		d.queue = make(chan observerNotification, g.observerQueueCapacity())
		d.stop = make(chan struct{})
		d.changed = make(chan struct{})
		go g.runObserver(d)
	})
	n := len(e.Payload) + len(e.Detail) + len(e.Kind) + len(e.LegType) + len(e.Direction) + len(e.CorrelationID) + len(e.Counterpart) + len(e.AuthorityFrame) + len(e.Op)
	if e.Route != nil {
		n += len(e.Route.Token) + len(e.Route.BuildLine) + len(e.Route.BridgeIssue)
		for _, v := range e.Route.Chain {
			n += len(v.Module) + len(v.From) + len(v.To) + len(v.Class)
		}
		for _, v := range e.Route.Own {
			n += len(v)
		}
		for _, v := range e.Route.Peer {
			n += len(v)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || len(e.Payload) > observationMessageLimit || n-len(e.Payload) > observationMessageLimit || len(d.queue) == cap(d.queue) || !g.observationMemory.reserve(n) {
		d.dropped++
		return
	}
	e.inspection = &ObserverInspection{gateway: g}
	e.Payload = bytes.Clone(e.Payload)
	if e.Route != nil {
		r := *e.Route
		r.Chain = append([]ChainStep(nil), r.Chain...)
		r.Own = append([]string(nil), r.Own...)
		r.Peer = append([]string(nil), r.Peer...)
		e.Route = &r
	}
	d.accepted++
	d.queue <- observerNotification{e, n, d.accepted}
}
func (g *Gateway) runObserver(d *observerDispatcher) {
	for {
		select {
		case <-d.stop:
			return
		case n := <-d.queue:
			d.mu.Lock()
			closed := d.closed
			d.mu.Unlock()
			panicked := false
			if !closed {
				func() {
					defer func() {
						if recover() != nil {
							panicked = true
						}
					}()
					g.cfg.Observer(n.event)
				}()
			}
			g.observationMemory.release(n.size)
			d.mu.Lock()
			d.completed = n.sequence
			if closed {
				d.dropped++
			}
			if panicked {
				d.panics++
			}
			close(d.changed)
			d.changed = make(chan struct{})
			d.mu.Unlock()
		}
	}
}
func (g *Gateway) closeObserver() {
	d := &g.observerDispatch
	// Initializing the channels without starting a dispatcher prevents a late
	// producer from reopening the stream after Close.
	d.once.Do(func() {
		d.queue = make(chan observerNotification, g.observerQueueCapacity())
		d.stop = make(chan struct{})
		d.changed = make(chan struct{})
	})
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	d.closed = true
	close(d.stop)
	for {
		select {
		case n := <-d.queue:
			g.observationMemory.release(n.size)
			d.dropped++
		default:
			close(d.changed)
			d.changed = make(chan struct{})
			return
		}
	}
}
func (g *Gateway) waitObserver(ctx context.Context) error {
	d := &g.observerDispatch
	d.mu.Lock()
	defer d.mu.Unlock()
	barrier := d.accepted
	for d.completed < barrier && !d.closed {
		changed := d.changed
		d.mu.Unlock()
		select {
		case <-ctx.Done():
			d.mu.Lock()
			return ctx.Err()
		case <-changed:
		}
		d.mu.Lock()
	}
	if d.dropped > 0 || d.panics > 0 || d.completed < barrier {
		return errors.New("observer delivery incomplete")
	}
	return ctx.Err()
}

func (b *observationBudget) TryReserve(n int) bool { return b.reserve(n) }
func (b *observationBudget) Release(n int)         { b.release(n) }
