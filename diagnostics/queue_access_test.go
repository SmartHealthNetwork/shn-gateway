package diagnostics

import (
	"context"
	"strings"
	"testing"
	"time"
)

// next takes the next due event, failing the test when none is queued.
func next(t *testing.T, q *Queue) Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	e, err := q.Next(ctx)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	return e
}

func TestQueueAccessEventShedsTheNewestBodyEvent(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 2})
	if !q.TryEmit(Event{Kind: "door.request", Body: []byte("a")}) || !q.TryEmit(Event{Kind: "door.response", Body: []byte("b")}) {
		t.Fatal("body events rejected below the limit")
	}
	if !q.TryEmit(Event{Kind: KindAccess, Detail: `{"outcome":"answered"}`}) {
		t.Fatal("full queue shed the access event")
	}
	if e := next(t, q); e.Kind != "door.request" {
		t.Fatalf("first = %q, want the oldest body event kept", e.Kind)
	}
	if e := next(t, q); e.Kind != KindAccess || e.Sequence != 3 {
		t.Fatalf("second = %q seq %d, want the access event", e.Kind, e.Sequence)
	}
	h := q.Health(time.Unix(1, 0))
	if h.Dropped != 1 || h.State != "degraded" || h.Pending != 2 {
		t.Fatalf("health = %+v, want one shed body event", h)
	}
}

func TestQueueAccessEventShedsBodyEventsUntilItsBytesFit(t *testing.T) {
	access := Event{Kind: KindAccess, Detail: strings.Repeat("x", 40)}
	q := NewQueue(Limits{MaxEvents: 8, MaxBytes: eventCost(access) + 30})
	for _, b := range []string{"0123456789", "abcdefghij", "ABCDEFGHIJ"} {
		if !q.TryEmit(Event{Kind: "door.response", Body: []byte(b)}) {
			t.Fatal("body event rejected below the byte limit")
		}
	}
	if !q.TryEmit(access) {
		t.Fatal("access event shed while body events held its bytes")
	}
	if e := next(t, q); string(e.Body) != "0123456789" {
		t.Fatalf("kept %q, want only the oldest body event", e.Body)
	}
	if e := next(t, q); e.Kind != KindAccess {
		t.Fatalf("second = %q, want the access event", e.Kind)
	}
	if h := q.Health(time.Unix(1, 0)); h.Dropped != 2 {
		t.Fatalf("dropped = %d, want the two newest body events", h.Dropped)
	}
}

func TestQueueAccessEventNeverShedsAnInFlightEvent(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 1})
	q.TryEmit(Event{Kind: "door.response", Body: []byte("in flight")})
	inFlight := next(t, q)
	if q.TryEmit(Event{Kind: KindAccess, Detail: "access"}) {
		t.Fatal("access event admitted by shedding an in-flight event")
	}
	q.Acknowledge(inFlight.Sequence)
	if h := q.Health(time.Unix(1, 0)); h.Dropped != 1 || h.LastAcknowledged != inFlight.Sequence || h.Pending != 0 {
		t.Fatalf("health = %+v", h)
	}
}

func TestQueueAccessEventNeverShedsAnotherAccessEvent(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 1})
	if !q.TryEmit(Event{Kind: KindAccess, Detail: "first"}) {
		t.Fatal("access event rejected below the limit")
	}
	if q.TryEmit(Event{Kind: KindAccess, Detail: "second"}) {
		t.Fatal("access event admitted by shedding another access event")
	}
	if e := next(t, q); e.Detail != "first" {
		t.Fatalf("kept %q, want the first access event", e.Detail)
	}
}

func TestQueueAccessEventWithABodyHasNoPriority(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 1})
	q.TryEmit(Event{Kind: "door.response", Body: []byte("kept")})
	if q.TryEmit(Event{Kind: KindAccess, Body: []byte("not metadata")}) {
		t.Fatal("an access event carrying a body shed a body event")
	}
	if e := next(t, q); string(e.Body) != "kept" {
		t.Fatalf("kept %q", e.Body)
	}
}

// A shed event's bytes return to the budget: after the survivors are
// acknowledged, the queue admits as much as it did when empty.
func TestQueueShedReleasesItsBytes(t *testing.T) {
	body := Event{Kind: "door.response", Body: []byte("0123456789abcdef")}
	q := NewQueue(Limits{MaxEvents: 8, MaxBytes: 2 * eventCost(body)})
	for range 2 {
		if !q.TryEmit(body) {
			t.Fatal("body event rejected below the byte limit")
		}
	}
	if !q.TryEmit(Event{Kind: KindAccess}) {
		t.Fatal("access event shed")
	}
	for range 2 {
		q.Acknowledge(next(t, q).Sequence)
	}
	for i := range 2 {
		if !q.TryEmit(body) {
			t.Fatalf("empty queue refused body event %d: a shed event still holds its bytes", i+1)
		}
	}
}

// A refused shed touches nothing: the queued event, the drop count and the
// byte budget are as they were, apart from the refused event's own drop.
func TestQueueAccessEventTooLargeForAnEmptyQueueShedsNothing(t *testing.T) {
	kept := Event{Kind: "door.response", Body: []byte("kept")}
	q := NewQueue(Limits{MaxEvents: 4, MaxBytes: 64})
	if !q.TryEmit(kept) {
		t.Fatal("body event rejected")
	}
	if q.TryEmit(Event{Kind: KindAccess, Detail: strings.Repeat("x", 128)}) {
		t.Fatal("oversized access event admitted")
	}
	if h := q.Health(time.Unix(1, 0)); h.Dropped != 1 || h.Pending != 1 {
		t.Fatalf("health = %+v, want the refused event's drop only", h)
	}
	over := Event{Kind: "door.response"}
	over.Body = []byte(strings.Repeat("y", int(64-eventCost(kept)-eventCost(over)+1)))
	if q.TryEmit(over) {
		t.Fatal("the refused shed released bytes it never freed")
	}
	if e := next(t, q); string(e.Body) != "kept" {
		t.Fatalf("body event shed for an access event that could never fit: %q", e.Body)
	}
}

// The kind is the wire contract with the emitter and the ingest.
func TestKindAccessIsTheWireKind(t *testing.T) {
	if KindAccess != "access" {
		t.Fatalf("KindAccess = %q", KindAccess)
	}
}

// Only an access event sheds: a body-less event of any other kind (a
// call-started marker, a failed leg) is refused like any other.
func TestQueueBodylessNonAccessEventNeverSheds(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 1})
	q.TryEmit(Event{Kind: "door.response", Body: []byte("kept")})
	if q.TryEmit(Event{Kind: "leg.failed", Detail: "no body"}) {
		t.Fatal("a body-less non-access event shed a body event")
	}
	if e := next(t, q); string(e.Body) != "kept" {
		t.Fatalf("kept %q", e.Body)
	}
	if h := q.Health(time.Unix(1, 0)); h.Dropped != 1 {
		t.Fatalf("dropped = %d, want the refused event only", h.Dropped)
	}
}

// An access event that exactly fills the bytes one shed frees is admitted,
// and sheds no more than that one event.
func TestQueueAccessEventExactlyFillingTheFreedBytes(t *testing.T) {
	body := Event{Kind: "door.response", Body: []byte("0123456789abcdef")}
	access := Event{Kind: KindAccess}
	access.Detail = strings.Repeat("x", int(eventCost(body)-eventCost(access)))
	if eventCost(access) != eventCost(body) {
		t.Fatalf("padding: access costs %d, body %d", eventCost(access), eventCost(body))
	}
	q := NewQueue(Limits{MaxEvents: 8, MaxBytes: 2 * eventCost(body)})
	for range 2 {
		if !q.TryEmit(body) {
			t.Fatal("body event rejected below the byte limit")
		}
	}
	if !q.TryEmit(access) {
		t.Fatal("an access event that fits exactly after one shed was refused")
	}
	if e := next(t, q); e.Kind != "door.response" || e.Sequence != 1 {
		t.Fatalf("first = %q seq %d, want the older body event kept", e.Kind, e.Sequence)
	}
	if h := q.Health(time.Unix(1, 0)); h.Dropped != 1 {
		t.Fatalf("dropped = %d, want exactly one shed", h.Dropped)
	}
}

// Even a one-byte body takes an access event's priority away.
func TestQueueAccessEventWithAOneByteBodyHasNoPriority(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 1})
	q.TryEmit(Event{Kind: "door.response", Body: []byte("kept")})
	if q.TryEmit(Event{Kind: KindAccess, Body: []byte("x")}) {
		t.Fatal("an access event with a one-byte body shed a body event")
	}
}

// A refused access event marks the queue degraded like any refused emit.
func TestQueueRefusedAccessEventDegradesTheQueue(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 1})
	q.TryEmit(Event{Kind: KindAccess, Detail: "first"})
	if h := q.Health(time.Unix(1, 0)); h.State != "healthy" {
		t.Fatalf("state = %q before any refusal", h.State)
	}
	if q.TryEmit(Event{Kind: KindAccess, Detail: "second"}) {
		t.Fatal("access event admitted into a queue full of access events")
	}
	if h := q.Health(time.Unix(1, 0)); h.State != "degraded" || h.Dropped != 1 {
		t.Fatalf("health = %+v, want degraded with one drop", h)
	}
}

// A queue of one undelivered body event sheds it: the oldest slot is
// eligible too.
func TestQueueAccessEventShedsTheOnlyQueuedEvent(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 1})
	q.TryEmit(Event{Kind: "door.response", Body: []byte("x")})
	if !q.TryEmit(Event{Kind: KindAccess}) {
		t.Fatal("access event refused while the only queued event could be shed")
	}
	if e := next(t, q); e.Kind != KindAccess {
		t.Fatalf("queued %q, want the access event", e.Kind)
	}
}

// Shedding steps past a protected access event to an older body event, and
// removes only the shed event.
func TestQueueAccessEventShedsBehindAProtectedEvent(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 2})
	q.TryEmit(Event{Kind: "door.response", Body: []byte("x")})
	q.TryEmit(Event{Kind: KindAccess, Detail: "a1"})
	if !q.TryEmit(Event{Kind: KindAccess, Detail: "a2"}) {
		t.Fatal("access event refused while an older body event could be shed")
	}
	if h := q.Health(time.Unix(1, 0)); h.Pending != 2 || h.Dropped != 1 {
		t.Fatalf("health = %+v, want both access events kept and one shed", h)
	}
	for _, want := range []string{"a1", "a2"} {
		if e := next(t, q); e.Detail != want {
			t.Fatalf("got %q, want %q", e.Detail, want)
		}
	}
}

// Two sheds return both events' bytes: once the survivors are acknowledged,
// the queue admits its full budget again.
func TestQueueTwoShedsReleaseBothEventsBytes(t *testing.T) {
	body := Event{Kind: "door.response", Body: []byte("0123456789")}
	c := eventCost(body)
	q := NewQueue(Limits{MaxEvents: 8, MaxBytes: 3 * c})
	for range 3 {
		q.TryEmit(body)
	}
	big := Event{Kind: KindAccess}
	big.Detail = strings.Repeat("x", int(2*c-eventCost(big)))
	if !q.TryEmit(big) {
		t.Fatal("access event refused")
	}
	for range 2 {
		q.Acknowledge(next(t, q).Sequence)
	}
	for i := range 3 {
		if !q.TryEmit(body) {
			t.Fatalf("empty queue refused body event %d: shed bytes were not all released", i+1)
		}
	}
}

func TestQueueBodyEventNeverShedsAnotherEvent(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 1})
	q.TryEmit(Event{Kind: "door.request", Body: []byte("first")})
	if q.TryEmit(Event{Kind: "door.response", Body: []byte("second")}) {
		t.Fatal("body event admitted into a full queue")
	}
	if e := next(t, q); string(e.Body) != "first" {
		t.Fatalf("kept %q", e.Body)
	}
}
