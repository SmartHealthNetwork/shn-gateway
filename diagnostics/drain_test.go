package diagnostics

import (
	"context"
	"testing"
	"time"
)

// A publisher stopped with events queued delivers them before it returns,
// the batch it had in flight included, and its last heartbeat says so.
func TestDrainDeliversWhatIsQueuedOnStop(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 4096, MaxBytes: 64 << 20})
	emitN(t, q, 300, "x")
	f := newFakeIngest(t, q)
	f.rtt, f.drain = 20*time.Millisecond, 10*time.Second
	f.batch = func(f *fakeIngest, events []Event) (int, string) {
		if len(f.batches) == 1 {
			// The stop arrives while the first batch is out; it is not taken.
			f.cancel()
			return 503, ""
		}
		return 200, resultsAll(events, batchAccepted)
	}
	f.run(context.Background(), batchURL)
	checkHealth(t, q, f.now, Health{Acknowledged: 300})
	if f.lastHealth.Acknowledged != 300 || f.lastHealth.Pending != 0 || f.lastHealth.Dropped != 0 {
		t.Fatalf("last heartbeat %+v", f.lastHealth)
	}
	if f.now.Sub(f.batchAt[0]) > f.drain {
		t.Fatalf("the drain ran %v, past its %v", f.now.Sub(f.batchAt[0]), f.drain)
	}
}

// A drain that cannot finish ends at its deadline: what is left is counted
// as stopped, and the last heartbeat carries the count.
func TestDrainEndsAtItsDeadlineAndCountsTheRest(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 4096, MaxBytes: 64 << 20})
	emitN(t, q, 40, "x")
	f := newFakeIngest(t, q)
	f.rtt, f.drain = 100*time.Millisecond, 2*time.Second
	var stoppedAt time.Time
	f.batch = func(f *fakeIngest, events []Event) (int, string) {
		if len(f.batches) == 1 {
			f.cancel()
			stoppedAt = f.now
		}
		return 503, `{"code":"unavailable"}`
	}
	f.run(context.Background(), batchURL)
	if len(f.batches) < 3 {
		t.Fatalf("the drain did not retry: %d batches", len(f.batches))
	}
	if over := f.batchAt[len(f.batchAt)-1].Sub(stoppedAt); over > f.drain {
		t.Fatalf("a batch went %v after the stop, past the %v drain", over, f.drain)
	}
	checkHealth(t, q, f.now, Health{Dropped: 40, DroppedBy: DropCounts{Stopped: 40}})
	if f.lastHealth.DroppedBy.Stopped != 40 || f.lastHealth.Pending != 0 || !f.lastHealth.CountsAgree() {
		t.Fatalf("last heartbeat %+v", f.lastHealth)
	}
}

// The drain works one event per request too: the event in flight at the
// stop is sent again, not dropped.
func TestDrainDeliversOneEventPerRequest(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 64, MaxBytes: 1 << 20})
	emitN(t, q, 5, "x")
	f := newFakeIngest(t, q)
	f.noBatches = true
	f.rtt, f.drain = 10*time.Millisecond, 10*time.Second
	f.single = func(f *fakeIngest, _ Event) (int, string) {
		if len(f.singles) == 1 {
			f.cancel()
			return 503, ""
		}
		return 204, ""
	}
	f.run(context.Background(), batchURL)
	if f.singles[0] != f.singles[1] {
		t.Fatalf("the event in flight at the stop was not sent again: %v", f.singles)
	}
	checkHealth(t, q, f.now, Health{Acknowledged: 5})
	if f.lastHealth.Acknowledged != 5 {
		t.Fatalf("last heartbeat %+v", f.lastHealth)
	}
}

// Without a drain, a stop is as before: what is in flight is dropped as
// stopped at once, and no last heartbeat is sent.
func TestNoDrainStopsAtOnce(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 64, MaxBytes: 1 << 20})
	emitN(t, q, 5, "x")
	f := newFakeIngest(t, q)
	f.batch = func(f *fakeIngest, events []Event) (int, string) {
		f.cancel()
		return 503, ""
	}
	f.run(context.Background(), batchURL)
	beats := f.heartbeats
	h := q.Health(f.now)
	if h.DroppedBy.Stopped != 5 || h.Pending != 0 || len(f.batches) != 1 || beats != 1 {
		t.Fatalf("health %+v, %d batches, %d heartbeats", h, len(f.batches), beats)
	}
}
