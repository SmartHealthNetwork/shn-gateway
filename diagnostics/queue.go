package diagnostics

import (
	"context"
	"errors"
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
}

var ErrQueueClosed = errors.New("diagnostics: queue closed")

type Queue struct {
	closed                                  bool
	stopped                                 chan struct{}
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
	return &Queue{limits: l, state: "healthy", ready: make(chan struct{}, 1), stopped: make(chan struct{})}
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
	if q.closed || len(q.items) >= q.limits.MaxEvents || cost > q.limits.MaxBytes-q.retainedBytes {
		q.dropped++
		q.state = "degraded"
		return false
	}
	if e.bodyBudget != nil && !e.bodyBudget.TryReserve(len(costEvent.Body)) {
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
		q.mu.Lock()
		if q.closed {
			q.mu.Unlock()
			return Event{}, ErrQueueClosed
		}
		for i := range q.items {
			if !q.items[i].delivered {
				q.items[i].delivered = true
				e := q.items[i].event
				for j := i + 1; j < len(q.items); j++ {
					if !q.items[j].delivered {
						select {
						case q.ready <- struct{}{}:
						default:
						}
						break
					}
				}
				q.mu.Unlock()
				return e, nil
			}
		}
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return Event{}, ctx.Err()
		case <-q.ready:
		case <-q.stopped:
			return Event{}, ErrQueueClosed
		}
	}
}

func (q *Queue) finish(sequence uint64, acknowledged, dropped bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.items {
		if q.items[i].event.Sequence == sequence {
			if b := q.items[i].event.bodyBudget; b != nil {
				b.Release(len(q.items[i].event.Body))
			}
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
	return Health{Time: now, LastSequence: q.nextSequence, LastAcknowledged: q.lastAcknowledged, Pending: uint64(len(q.items)), Dropped: q.dropped, State: q.state, Closed: q.closed}
}

// deferredBinding keeps the original queue reservation and absolute ownership
// deadline while moving a dependency-blocked event behind already queued work.
func (q *Queue) deferredBinding(sequence uint64) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		q.Drop(sequence)
		return
	}
	defer q.mu.Unlock()
	for i := range q.items {
		if q.items[i].event.Sequence == sequence {
			item := q.items[i]
			item.delivered = false
			copy(q.items[i:], q.items[i+1:])
			q.items[len(q.items)-1] = item
			select {
			case q.ready <- struct{}{}:
			default:
			}
			return
		}
	}
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

// Close stops admission and drops pending entries. A Next consumer keeps its
// delivered body's charge until Acknowledge/Drop after its actual return.
// Repeated Close calls never double-release ownership.
func (q *Queue) Close() {
	if q == nil {
		return
	}
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.closed = true
	close(q.stopped)
	pending := make([]queuedEvent, 0, len(q.items))
	retained := q.items[:0]
	for _, item := range q.items {
		if item.delivered {
			retained = append(retained, item)
		} else {
			pending = append(pending, item)
			q.retainedBytes -= item.bytes
			q.dropped++
		}
	}
	clear(q.items[len(retained):])
	q.items = retained
	if len(pending) > 0 {
		q.state = "degraded"
	}
	q.mu.Unlock()
	for i := range pending {
		if b := pending[i].event.bodyBudget; b != nil {
			b.Release(len(pending[i].event.Body))
		}
		pending[i] = queuedEvent{}
	}
}
