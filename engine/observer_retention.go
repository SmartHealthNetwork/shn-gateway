package engine

import "sync"

// ObserverInspection is the nonserialized owner of retained inspection copies.
// The shipped SSE sink uses it to share the gateway budget and report loss.
// Other callbacks remain responsible for bounding their own allocations.
type ObserverInspection struct{ gateway *Gateway }

// Inspection returns the optional reservation owner. Hand-authored events have
// no gateway owner; a sink must still enforce its own bounded storage.
func (e ObserverEvent) Inspection() *ObserverInspection { return e.inspection }

// Reserve charges bytes before a sink allocates or serializes a retained copy.
func (o *ObserverInspection) Reserve(n int) (*ObserverReservation, bool) {
	if o == nil {
		return nil, true
	}
	if !o.gateway.observationMemory.reserve(n) {
		return nil, false
	}
	return &ObserverReservation{budget: &o.gateway.observationMemory, bytes: n}, true
}

// Drop makes inspection loss visible to the gateway completion barrier. It does
// not count as conformance work or affect participant delivery.
func (o *ObserverInspection) Drop() {
	if o == nil {
		return
	}
	d := &o.gateway.observerDispatch
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dropped++
	if d.changed != nil {
		close(d.changed)
		d.changed = make(chan struct{})
	}
}

// ObserverReservation holds a distinct retained copy's charge until its last
// owner releases it. Release and Shrink are idempotent under concurrent cleanup.
type ObserverReservation struct {
	mu     sync.Mutex
	budget *observationBudget
	bytes  int
}

func (r *ObserverReservation) Release() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.budget.release(r.bytes)
	r.bytes = 0
}
func (r *ObserverReservation) Shrink(n int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if n >= 0 && n < r.bytes {
		r.budget.release(r.bytes - n)
		r.bytes = n
	}
}
