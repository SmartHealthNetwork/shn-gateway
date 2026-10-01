package engine

import (
	"bytes"
	"context"
	"log"
	"math/bits"
	"sync"
	"time"
)

// Observe-level checks off the request path. At observe a payload check can
// only record, never refuse, so the leg does not wait for its verdict: the check
// is queued and a small pool of workers runs it against the validator and emits
// its finding. Below the queue's capacity nothing is lost; past it the newest
// check is dropped, counted and logged, never waited for. structural and strict
// refuse on the verdict, and a bridged payload (SHN's own edit) refuses at every
// level, so those checks stay on the request path.
const (
	// observeCheckQueueCapacity bounds the checks waiting for a worker.
	observeCheckQueueCapacity = 256
	// observeCheckWorkers is how many checks run against the validator at once,
	// which also bounds this gateway's share of a shared validator lane.
	observeCheckWorkers = 4
)

// observeFlushTimeout bounds how long Close waits for queued checks to finish
// before cancelling the validator calls still running; a cancelled call is
// recorded as unavailable, so a stuck validator cannot hold a shutdown.
var observeFlushTimeout = 10 * time.Second

// decisionEOBObserveDeadline bounds, at observe, the checks that stay on the
// request path at every level: the decision ExplanationOfBenefits a payer gateway
// builds from its participant's answer, whose verdicts decide whether each
// decision is written (the decision-EOB rule). It is one budget for all of a
// leg's decision EOBs; a check the budget does not reach in time is unavailable,
// recorded as such, and its decision written, as for any validator outage. It is
// a fixed bound, documented in CONFIGURATION.md, not a setting; a test may
// shorten it.
var decisionEOBObserveDeadline = 2 * time.Second

// observeCheck is one queued check: everything validateGovernedLines needs,
// captured when the leg asked for it. payload is the queue's own copy.
type observeCheck struct {
	fc         findingContext
	lanes      []lineLane
	payload    []byte
	dir        string
	line       string
	profile    string
	candidates bool
	// binding and tally are the leg's, snapshotted when the check was queued:
	// the request's context is gone by the time a worker runs it.
	binding findingBinding
	tally   *legTally
	// ticket is the check's place in queue order, for the completion barrier.
	ticket uint64
	// run, when set, is an observational check that is not a governed one
	// (CRD's embedded resources): the worker runs it instead of judgeLines.
	run func(context.Context)
}

// observeChecks is the queue and its workers. One mutex and one condition
// variable serve the workers' wake-up, drainObserveChecks's wait for idle, and
// Close's flush; issued, pending and changed serve the observer completion
// barrier, which must honour its context. Workers finish out of order, so the
// barrier waits on the tickets issued before it was called, not on a count.
type observeChecks struct {
	mu      sync.Mutex
	cond    *sync.Cond
	queue   []observeCheck
	running int
	closed  bool
	dropped uint64
	issued  uint64
	pending map[uint64]struct{}
	changed chan struct{}
	exited  sync.WaitGroup
	ctx     context.Context
	cancel  context.CancelFunc
}

func newObserveChecks() *observeChecks {
	ctx, cancel := context.WithCancel(context.Background())
	q := &observeChecks{queue: make([]observeCheck, 0, observeCheckQueueCapacity), pending: map[uint64]struct{}{}, ctx: ctx, cancel: cancel, changed: make(chan struct{})}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// observeQueue starts the queue and its workers on first use, so a Gateway built
// as a struct literal (tests) and one built by New behave alike. Once Close has
// run it returns Close's closed queue, so a late check is dropped.
func (g *Gateway) observeQueue() *observeChecks {
	g.observeOnce.Do(func() {
		q := newObserveChecks()
		for i := 0; i < observeCheckWorkers; i++ {
			q.exited.Add(1)
			go g.runObserveChecks(q)
		}
		g.observeChecks.Store(q)
	})
	return g.observeChecks.Load()
}

// enqueueObserveCheck queues a check and returns at once. A full or closed queue
// drops the check, counts it and logs it: the leg is never held and never
// refused, and the leg's result says it is incomplete.
func (g *Gateway) enqueueObserveCheck(c observeCheck) {
	q := g.observeQueue()
	c.payload = bytes.Clone(c.payload)
	c.lanes = append([]lineLane(nil), c.lanes...)
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || len(q.queue) >= observeCheckQueueCapacity {
		q.dropped++
		if c.tally != nil {
			c.tally.dropped()
		}
		// Logged on the 1st, 2nd, 4th, 8th… drop, so a sustained overflow is
		// visible without a line per check.
		if bits.OnesCount64(q.dropped) == 1 {
			log.Printf("gateway: observe check dropped (%d so far): the observe queue is full or closed, so this check's finding is not recorded", q.dropped)
		}
		return
	}
	if c.tally != nil {
		c.tally.hold()
	}
	q.issued++
	c.ticket = q.issued
	q.pending[c.ticket] = struct{}{}
	q.queue = append(q.queue, c)
	q.cond.Broadcast() // a drain waiter shares the condition, so wake them all
}

// runObserveChecks is one worker: it takes the oldest check, runs it outside any
// request, and exits once the queue is closed and empty, so Close flushes.
func (g *Gateway) runObserveChecks(q *observeChecks) {
	defer q.exited.Done()
	for {
		q.mu.Lock()
		for len(q.queue) == 0 && !q.closed {
			q.cond.Wait()
		}
		if len(q.queue) == 0 {
			q.mu.Unlock()
			return
		}
		c := q.queue[0]
		q.queue = q.queue[1:]
		q.running++
		q.mu.Unlock()

		g.runObserveCheck(q.ctx, c)
		if c.tally != nil {
			c.tally.release(g)
		}

		q.mu.Lock()
		q.running--
		delete(q.pending, c.ticket)
		close(q.changed)
		q.changed = make(chan struct{})
		q.cond.Broadcast()
		q.mu.Unlock()
	}
}

// runObserveCheck runs one check. A panic in it (an Observer callback, a
// validator) ends that check only, as net/http contains one in a request
// handler: it is logged and the leg's result says incomplete.
func (g *Gateway) runObserveCheck(base context.Context, c observeCheck) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("gateway: observe check failed, so its finding is not recorded: %v", r)
			if c.tally != nil {
				c.tally.dropped()
			}
		}
	}()
	// The validator call carries the leg's finding context, as it did on the
	// request path (observingValidator and lane fakes read it).
	ctx := withLegTally(withFindingBinding(withFindingContext(base, c.fc), c.binding), c.tally)
	if c.run != nil {
		c.run(ctx)
		return
	}
	g.judgeLines(ctx, c.fc, c.lanes, c.payload, c.dir, c.line, c.profile, false, c.candidates)
}

// closeObserveChecks flushes the queue: no new check is accepted, and it returns
// once every queued and running check has emitted its finding. A gateway closed
// before its first check gets a closed queue with no workers, so a late check is
// dropped rather than starting workers nothing will stop.
func (g *Gateway) closeObserveChecks() {
	g.observeOnce.Do(func() { g.observeChecks.Store(newObserveChecks()) })
	q := g.observeChecks.Load()
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
	exited := make(chan struct{})
	go func() { q.exited.Wait(); close(exited) }()
	select {
	case <-exited:
	case <-time.After(observeFlushTimeout):
		q.cancel()
		<-exited
	}
	q.cancel()
}

// pendingThrough reports whether a check with a ticket at or before barrier is
// still queued or running. q.mu is held.
func (q *observeChecks) pendingThrough(barrier uint64) bool {
	for t := range q.pending {
		if t <= barrier {
			return true
		}
	}
	return false
}

// waitObserveChecks waits for every check queued before it was called, as the
// observer completion barrier waits for certification: an observe check's
// events belong to the operation that queued it.
func (g *Gateway) waitObserveChecks(ctx context.Context) error {
	q := g.observeChecks.Load()
	if q == nil {
		return ctx.Err()
	}
	q.mu.Lock()
	barrier := q.issued
	for q.pendingThrough(barrier) {
		changed := q.changed
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
		q.mu.Lock()
	}
	q.mu.Unlock()
	return ctx.Err()
}

// judgedInlineKey marks a check whose verdict the leg acts on below strict, so
// it stays on the request path at observe too (validateFHIRDecisionEOB).
type judgedInlineKey struct{}

func withJudgedInline(ctx context.Context) context.Context {
	return context.WithValue(ctx, judgedInlineKey{}, true)
}

func judgedInline(ctx context.Context) bool {
	v, _ := ctx.Value(judgedInlineKey{}).(bool)
	return v
}

// validateDecisionEOBs checks a leg's decision ExplanationOfBenefits in order,
// as the payer PAS legs do before answering. At observe the whole loop shares one
// budget (decisionEOBObserveDeadline): an EOB the budget does not reach is
// recorded as unavailable and its decision written, never skipped. It stops at
// the first refusal (strict, or a bridged payload) and reports whether any EOB
// was recorded invalid and so is not written.
func (g *Gateway) validateDecisionEOBs(ctx context.Context, eobs [][]byte) (status int, msg string, invalid bool) {
	if g.policy().Level() == EnforcementObserve && len(eobs) > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, decisionEOBObserveDeadline)
		defer cancel()
	}
	for _, b := range eobs {
		s, m, inv := g.validateFHIRDecisionEOB(ctx, b)
		if s != 0 {
			return s, m, invalid
		}
		invalid = invalid || inv
	}
	return 0, "", invalid
}

// drainObserveChecks waits until no check is queued or running: for tests that
// read a finding the leg queued. It starts nothing.
func (g *Gateway) drainObserveChecks() {
	q := g.observeChecks.Load()
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.queue) > 0 || q.running > 0 {
		q.cond.Wait()
	}
}

// WaitObserveChecksForTest waits until no observe-level check is queued or
// running, so a harness that reads findings sees every check its calls queued.
func (g *Gateway) WaitObserveChecksForTest() { g.drainObserveChecks() }

// observeChecksDropped is how many observe-level checks were dropped because the
// queue was full or closed.
func (g *Gateway) observeChecksDropped() uint64 {
	q := g.observeChecks.Load()
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.dropped
}
