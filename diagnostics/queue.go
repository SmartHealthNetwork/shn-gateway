package diagnostics

import (
	"context"
	"net/http"
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
	state                                   string
	ready                                   chan struct{}
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
	costEvent := e
	if int64(len(costEvent.Body)) > q.limits.MaxBodyBytes {
		costEvent.Body = costEvent.Body[:q.limits.MaxBodyBytes]
		if costEvent.Detail == "" {
			costEvent.Detail = "body capture partial: retention cap reached"
		}
	}
	cost := eventCost(costEvent)
	if len(q.items) >= q.limits.MaxEvents || cost > q.limits.MaxBytes-q.retainedBytes {
		q.dropped++
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

func (q *Queue) finish(sequence uint64, acknowledged, dropped bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.items {
		if q.items[i].event.Sequence == sequence {
			q.retainedBytes -= q.items[i].bytes
			copy(q.items[i:], q.items[i+1:])
			q.items[len(q.items)-1] = queuedEvent{}
			q.items = q.items[:len(q.items)-1]
			if acknowledged && sequence > q.lastAcknowledged {
				q.lastAcknowledged = sequence
			}
			if dropped {
				q.dropped++
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
func (q *Queue) Acknowledge(sequence uint64) { q.finish(sequence, true, false) }
func (q *Queue) Drop(sequence uint64)        { q.finish(sequence, false, true) }
func (q *Queue) discard(sequence uint64)     { q.finish(sequence, false, false) }
func (q *Queue) Health(now time.Time) Health {
	q.mu.Lock()
	defer q.mu.Unlock()
	return Health{Time: now, LastSequence: q.nextSequence, LastAcknowledged: q.lastAcknowledged, Pending: uint64(len(q.items)), Dropped: q.dropped, State: q.state}
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
