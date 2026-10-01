package diagnostics

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"time"
)

const (
	defaultMaxEvents    = 64
	defaultMaxBytes     = 16 << 20
	defaultMaxBodyBytes = 8 << 20
	ownershipWindow     = 30 * time.Second
	mapEntryCost        = 64
	sliceEntryCost      = 24
)

// KindAccess is the metadata-only per-exchange access event. It is small and
// exchange counts are built from it, so a full queue sheds other undelivered
// events to admit it (TryEmit), never the reverse. An access event that
// carries a body has no such priority.
const KindAccess = "access"

// KindConformanceFinding is one conformance finding a gateway's checks recorded:
// metadata only (the finding as its log line has it), keyed by the leg's
// correlation id, with no body. At observe a payload check runs off the request
// path, so its finding is not on the call's access line; the stats join these
// events to the exchange instead. It has no shedding priority: a full queue
// sheds it before an access event.
const KindConformanceFinding = "conformance.finding"

// KindConformanceResult closes one leg's findings at observe: its detail is
// {"count":N}, plus "truncated" when per-finding events were capped at
// FindingEventsPerLeg and "incomplete" when a check's finding was not recorded
// (dropped unrun, or failed while running). Until it arrives, a leg whose
// access line defers its findings has an unknown count, never zero.
const KindConformanceResult = "conformance.result"

// FindingEventsPerLeg caps how many KindConformanceFinding events one leg
// captures; its KindConformanceResult still counts every finding.
const FindingEventsPerLeg = 32

type Limits struct {
	MaxEvents    int
	MaxBytes     int64
	MaxBodyBytes int64
}
type queuedEvent struct {
	event     Event
	bytes     int64
	delivered bool
	deadline  time.Time
	// bindingWait is the event's current binding_pending wait: it grows while
	// the ingest holds the event pending, and leaves with the event.
	bindingWait time.Duration
	// notBefore is when a pending event is due again. Until then the
	// publisher takes the events behind it: one event's wait never holds up
	// the queue.
	notBefore time.Time
}
type Queue struct {
	mu                                      sync.Mutex
	items                                   []queuedEvent
	retainedBytes                           int64
	limits                                  Limits
	nextSequence, lastAcknowledged, dropped uint64
	// Every sequenced event is counted once, as it leaves the queue:
	// acknowledged, discarded or dropped (by reason).
	acknowledged, discarded uint64
	droppedBy               DropCounts
	// suppressedKinds holds the kinds the ingest does not admit from this
	// publisher, each until it is probed again; suppressed counts the
	// events discarded for it without being sent.
	suppressedKinds map[string]time.Time
	suppressed      uint64
	clock           func() time.Time
	state           string
	ready           chan struct{}
}

func NewQueue(l Limits) *Queue {
	if l.MaxEvents <= 0 {
		l.MaxEvents = defaultMaxEvents
	}
	if l.MaxBytes <= 0 {
		l.MaxBytes = defaultMaxBytes
	}
	if l.MaxBodyBytes <= 0 {
		l.MaxBodyBytes = defaultMaxBodyBytes
	}
	return &Queue{limits: l, state: "healthy", ready: make(chan struct{}, 1)}
}

func eventCost(e Event) int64 {
	n := int64(len(e.Body))
	for _, s := range []string{
		e.Source, e.Incarnation, e.Kind, e.CallID, e.CorrelationID,
		e.RequestCiphertextHash, e.Sender, e.Recipient, e.LegType,
		e.ContractLine, e.Method, e.URL, e.Detail,
		e.Identity.VerifiedClientID, e.Identity.ClaimedClientID,
		e.Identity.RegisteredClientID, e.Identity.AuthResult,
		e.RequestFingerprint.Algorithm, e.RequestFingerprint.Method,
		e.RequestFingerprint.RequestURI, e.RequestFingerprint.BodySHA256,
	} {
		n += int64(len(s))
	}
	for k, vs := range e.Headers {
		n += mapEntryCost + int64(len(k)) + sliceEntryCost
		for _, v := range vs {
			n += sliceEntryCost + int64(len(v))
		}
	}
	return n
}
func cloneEvent(e Event, bodyCap int64) Event {
	e.Headers = cloneHeader(e.Headers)
	original := len(e.Body)
	if int64(original) > bodyCap {
		original = int(bodyCap)
		e.BodyComplete = false
		if e.Detail == "" {
			e.Detail = "body capture partial: retention cap reached"
		}
	}
	body := make([]byte, original)
	copy(body, e.Body[:original])
	e.Body = body
	return e
}
func cloneHeader(h http.Header) http.Header {
	if h == nil {
		return nil
	}
	return h.Clone()
}

func (q *Queue) TryEmit(e Event) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.nextSequence++
	e.Sequence = q.nextSequence
	if probeAt, ok := q.suppressedKinds[e.Kind]; ok {
		if q.now().Before(probeAt) {
			q.discarded++
			q.suppressed++
			return true
		}
		// The suppression is over: this event and those after it go to the
		// ingest again, which says whether the kind is still not admitted.
		delete(q.suppressedKinds, e.Kind)
	}
	costEvent := e
	if int64(len(costEvent.Body)) > q.limits.MaxBodyBytes {
		costEvent.Body = costEvent.Body[:q.limits.MaxBodyBytes]
		if costEvent.Detail == "" {
			costEvent.Detail = "body capture partial: retention cap reached"
		}
	}
	cost := eventCost(costEvent)
	if !q.fits(cost) && (e.Kind != KindAccess || len(e.Body) != 0 || !q.shedFor(cost)) {
		q.dropped++
		q.droppedBy.add(queueFull)
		q.state = "degraded"
		return false
	}
	copyEvent := cloneEvent(e, q.limits.MaxBodyBytes)
	q.items = append(q.items, queuedEvent{event: copyEvent, bytes: cost})
	q.retainedBytes += cost
	select {
	case q.ready <- struct{}{}:
	default:
	}
	return true
}

func (q *Queue) fits(cost int64) bool {
	return len(q.items) < q.limits.MaxEvents && cost <= q.limits.MaxBytes-q.retainedBytes
}

// shedFor makes room for an access event of the given cost by shedding
// undelivered non-access events, newest first. An event deferred while its
// binding is pending sits at the tail, so it goes first; like an event whose
// ownership window expires, it counts as dropped. It sheds nothing unless that
// makes room: delivered events are in flight and access events are never
// shed for one another.
func (q *Queue) shedFor(cost int64) bool {
	events, bytes := len(q.items), q.retainedBytes
	var shed []int
	for i := len(q.items) - 1; i >= 0 && (events >= q.limits.MaxEvents || cost > q.limits.MaxBytes-bytes); i-- {
		if it := q.items[i]; !it.delivered && it.event.Kind != KindAccess {
			shed = append(shed, i)
			events--
			bytes -= it.bytes
		}
	}
	if events >= q.limits.MaxEvents || cost > q.limits.MaxBytes-bytes {
		return false
	}
	for _, i := range shed { // descending, so earlier indexes stay valid
		q.items = slices.Delete(q.items, i, i+1) // zeroes the vacated slot
	}
	q.retainedBytes = bytes
	q.dropped += uint64(len(shed))
	for range shed {
		q.droppedBy.add(queueFull)
	}
	q.state = "degraded"
	return true
}

func (q *Queue) Next(ctx context.Context) (Event, error) {
	for {
		e, ok, due := q.take(time.Now())
		if ok {
			return e, nil
		}
		var later <-chan time.Time
		var timer *time.Timer
		if due > 0 {
			timer = time.NewTimer(due)
			later = timer.C
		}
		select {
		case <-ctx.Done():
		case <-q.ready:
		case <-later:
		}
		if timer != nil {
			timer.Stop()
		}
		if err := ctx.Err(); err != nil {
			return Event{}, err
		}
	}
}

// take marks the first undelivered event due at now delivered and returns
// it. With none due, it returns how long until the earliest undelivered
// event is due, or zero when there is none.
func (q *Queue) take(now time.Time) (Event, bool, time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var due time.Duration
	for i := range q.items {
		if q.items[i].delivered {
			continue
		}
		if wait := q.items[i].notBefore.Sub(now); wait > 0 {
			if due == 0 || wait < due {
				due = wait
			}
			continue
		}
		q.items[i].delivered = true
		if q.items[i].event.Time.IsZero() {
			// Stamped once, so a resent event carries the same bytes.
			q.items[i].event.Time = now
		}
		for j := i + 1; j < len(q.items); j++ {
			if !q.items[j].delivered {
				select {
				case q.ready <- struct{}{}:
				default:
				}
				break
			}
		}
		return q.items[i].event, true, 0
	}
	return Event{}, false, due
}

// kindReprobe is how long a kind the ingest does not admit is withheld
// before an event of it is sent again: the ingest's admission can widen with
// a deploy, and a long-running publisher must find that out.
const kindReprobe = 10 * time.Minute

// suppressKind withholds kind until kindReprobe from now.
func (q *Queue) suppressKind(kind string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.suppressedKinds == nil {
		q.suppressedKinds = map[string]time.Time{}
	}
	q.suppressedKinds[kind] = q.now().Add(kindReprobe)
}

// setClock sets the queue's clock; nil is the wall clock.
func (q *Queue) setClock(clock func() time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.clock = clock
}

func (q *Queue) now() time.Time {
	if q.clock == nil {
		return time.Now()
	}
	return q.clock()
}

// takeBatch marks up to max due undelivered events, in queue order,
// delivered and returns them, stopping before an event that would take their
// summed cost past maxCost; the first due event is always taken. With none
// due, it returns how long until the earliest undelivered event is due, or
// zero when there is none.
func (q *Queue) takeBatch(now time.Time, max int, maxCost int64) ([]Event, time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []Event
	var cost int64
	var due time.Duration
	for i := range q.items {
		it := &q.items[i]
		if it.delivered {
			continue
		}
		if wait := it.notBefore.Sub(now); wait > 0 {
			if due == 0 || wait < due {
				due = wait
			}
			continue
		}
		if len(out) == max || (len(out) > 0 && cost+it.bytes > maxCost) {
			break
		}
		it.delivered = true
		if it.event.Time.IsZero() {
			// Stamped once, so a resent event carries the same bytes.
			it.event.Time = now
		}
		cost += it.bytes
		out = append(out, it.event)
	}
	if len(out) > 0 {
		return out, 0
	}
	return nil, due
}

// empty reports whether no event is queued.
func (q *Queue) empty() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items) == 0
}

// dropRemaining drops every queued event for reason.
func (q *Queue) dropRemaining(reason dropReason) {
	q.mu.Lock()
	var seqs []uint64
	for _, it := range q.items {
		seqs = append(seqs, it.event.Sequence)
	}
	q.mu.Unlock()
	for _, s := range seqs {
		q.dropFor(s, reason)
	}
}

// retryAt makes a delivered event due again at notBefore, in its place and
// with its ownership deadline kept.
func (q *Queue) retryAt(sequence uint64, notBefore time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.items {
		if q.items[i].event.Sequence == sequence {
			q.items[i].delivered = false
			q.items[i].notBefore = notBefore
			select {
			case q.ready <- struct{}{}:
			default:
			}
			return
		}
	}
}

// waitDue waits d for the earliest pending event to fall due, returning early
// when an event is emitted. It returns only once its watcher has stopped, so
// the watcher can never take a signal meant for the publisher's next wait.
func (q *Queue) waitDue(ctx context.Context, wait func(context.Context, time.Duration) error, d time.Duration) error {
	wake, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-q.ready:
			stop()
		case <-wake.Done():
		}
	}()
	err := wait(wake, d)
	stop()
	<-done
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

// outcome is how an event leaves the queue.
type outcome int

const (
	acknowledged outcome = iota
	discarded
	dropped
)

// dropReason names the DropCounts field a drop is counted in.
type dropReason int

const (
	notDropped dropReason = iota
	queueFull
	expired
	oversized
	unencodable
	testNotAccepted
	stopped
	invalid
	// dropReasons is one past the last reason.
	dropReasons
)

func (d *DropCounts) add(r dropReason) {
	switch r {
	case queueFull:
		d.QueueFull++
	case expired:
		d.Expired++
	case oversized:
		d.Oversized++
	case unencodable:
		d.Unencodable++
	case testNotAccepted:
		d.Test++
	case stopped:
		d.Stopped++
	case invalid:
		d.Invalid++
	}
}

// finish removes an event and counts how it left, once: an event already
// gone is not counted again.
func (q *Queue) finish(sequence uint64, how outcome, reason dropReason) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.items {
		if q.items[i].event.Sequence == sequence {
			q.retainedBytes -= q.items[i].bytes
			copy(q.items[i:], q.items[i+1:])
			q.items[len(q.items)-1] = queuedEvent{}
			q.items = q.items[:len(q.items)-1]
			switch how {
			case acknowledged:
				q.acknowledged++
				if sequence > q.lastAcknowledged {
					q.lastAcknowledged = sequence
				}
			case discarded:
				q.discarded++
			case dropped:
				q.dropped++
				q.droppedBy.add(reason)
				q.state = "degraded"
			}
			break
		}
	}
	for _, it := range q.items {
		if !it.delivered {
			select {
			case q.ready <- struct{}{}:
			default:
				{
				}
			}
			break
		}
	}
}
func (q *Queue) Acknowledge(sequence uint64) { q.finish(sequence, acknowledged, notDropped) }

// Drop drops an event whose ownership window ran out.
func (q *Queue) Drop(sequence uint64) { q.dropFor(sequence, expired) }

func (q *Queue) dropFor(sequence uint64, reason dropReason) { q.finish(sequence, dropped, reason) }
func (q *Queue) discard(sequence uint64)                    { q.finish(sequence, discarded, notDropped) }

// Health is the queue's heartbeat: its sequence, what is pending, and how
// every sequenced event left it.
func (q *Queue) Health(now time.Time) Health {
	q.mu.Lock()
	defer q.mu.Unlock()
	return Health{Time: now, LastSequence: q.nextSequence, LastAcknowledged: q.lastAcknowledged, Pending: uint64(len(q.items)), Dropped: q.dropped, State: q.state,
		Acknowledged: q.acknowledged, Discarded: q.discarded, DroppedBy: q.droppedBy, Suppressed: q.suppressed}
}

// deferredBinding keeps the original queue reservation and absolute ownership
// deadline while moving a dependency-blocked event behind already queued work,
// due again after its binding wait (grown by nextRetryWait each time it is
// held pending, spread by jitter, and never past its ownership deadline). It
// returns the wait.
func (q *Queue) deferredBinding(sequence uint64, now time.Time, jitter func(time.Duration) time.Duration) time.Duration {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.items {
		if q.items[i].event.Sequence == sequence {
			item := q.items[i]
			item.delivered = false
			item.bindingWait = nextRetryWait(item.bindingWait)
			wait := jitter(item.bindingWait)
			if left := item.deadline.Sub(now); !item.deadline.IsZero() && left < wait {
				wait = max(left, 0)
			}
			item.notBefore = now.Add(wait)
			copy(q.items[i:], q.items[i+1:])
			q.items[len(q.items)-1] = item
			select {
			case q.ready <- struct{}{}:
			default:
			}
			return wait
		}
	}
	return 0
}
func (q *Queue) ownershipDeadline(sequence uint64, now time.Time) time.Time {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.items {
		if q.items[i].event.Sequence == sequence {
			if q.items[i].deadline.IsZero() {
				q.items[i].deadline = now.Add(ownershipWindow)
			}
			return q.items[i].deadline
		}
	}
	return now
}
