package diagnostics

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// fakeIngest answers a publisher's requests on a fake clock: heartbeats with
// 204, batches through batch, single events through single. Each request
// costs rtt of fake time. It stops the publisher at the first heartbeat after
// every event has left the queue, or after a fake hour.
type fakeIngest struct {
	t      *testing.T
	q      *Queue
	now    time.Time
	rtt    time.Duration
	cancel context.CancelFunc
	batch  func(f *fakeIngest, events []Event) (int, string)
	single func(f *fakeIngest, e Event) (int, string)
	// batches holds each batch request's sequences; singles each single
	// request's sequence.
	batches [][]uint64
	singles []uint64
	signed  bool
	// noBatches: the ingest's heartbeat answers do not say it takes batches.
	noBatches bool
	// healthStatus, when set, is the heartbeat answer's status.
	healthStatus int
	// batchAt and singleAt are when each request was answered.
	batchAt, singleAt []time.Time
	// drain is the publisher's Drain; lastHealth the last heartbeat it sent,
	// and heartbeats how many it sent.
	drain      time.Duration
	lastHealth Health
	heartbeats int
	// With rate set, the gateway emits rate events a second of fake time,
	// total in all, as the clock advances.
	rate, total, emitted int
	owed                 float64
	// kinds, when set, are the emitted events' kinds in turn.
	kinds []string
}

// advance moves the fake clock and emits what the gateway would have in
// that time.
func (f *fakeIngest) advance(d time.Duration) {
	f.now = f.now.Add(d)
	if f.rate == 0 {
		return
	}
	f.owed += d.Seconds() * float64(f.rate)
	for ; f.owed >= 1 && f.emitted < f.total; f.owed-- {
		kind := KindAccess
		if len(f.kinds) > 0 {
			kind = f.kinds[f.emitted%len(f.kinds)]
		}
		f.q.TryEmit(Event{Kind: kind})
		f.emitted++
	}
}

func newFakeIngest(t *testing.T, q *Queue) *fakeIngest {
	return &fakeIngest{t: t, q: q, now: time.Unix(0, 0), signed: true}
}

func answer(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

// resultsAll answers every event in a batch with status.
func resultsAll(events []Event, status string) string {
	var a batchAnswer
	for _, e := range events {
		a.Results = append(a.Results, batchResult{Sequence: e.Sequence, Status: status})
	}
	raw, _ := json.Marshal(a)
	return string(raw)
}

func (f *fakeIngest) run(ctx context.Context, batchURL string) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	f.cancel = cancel
	start := f.now
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		if Sign([]byte("k"), r.Header.Get(HeaderEvidenceSource), r.Header.Get(HeaderEvidenceIncarnation), r.Header.Get(HeaderEvidenceTime), raw) != r.Header.Get(HeaderEvidenceSignature) {
			f.signed = false
		}
		f.advance(f.rtt)
		switch {
		case strings.HasSuffix(r.URL.Path, "/health"):
			f.heartbeats++
			_ = json.Unmarshal(raw, &f.lastHealth)
			if (f.q.Health(f.now).Pending == 0 && f.emitted == f.total) || f.now.Sub(start) > time.Hour {
				cancel()
			}
			status := 204
			if f.healthStatus != 0 {
				status = f.healthStatus
			}
			a := answer(status, "")
			if !f.noBatches {
				a.Header.Set(HeaderEvidenceBatch, "1")
			}
			return a, nil
		case strings.HasSuffix(r.URL.Path, "/batch"):
			var req struct {
				Events []Event `json:"events"`
			}
			if err := json.Unmarshal(raw, &req); err != nil {
				f.t.Errorf("batch body: %v", err)
			}
			var seqs []uint64
			for _, e := range req.Events {
				if e.Source != "payer" || e.Incarnation != "boot" {
					f.t.Errorf("event %d carries %q/%q", e.Sequence, e.Source, e.Incarnation)
				}
				seqs = append(seqs, e.Sequence)
			}
			f.batches = append(f.batches, seqs)
			f.batchAt = append(f.batchAt, f.now)
			status, body := f.batch(f, req.Events)
			return answer(status, body), nil
		default:
			var e Event
			_ = json.Unmarshal(raw, &e)
			f.singles = append(f.singles, e.Sequence)
			f.singleAt = append(f.singleAt, f.now)
			if f.single == nil {
				return answer(204, ""), nil
			}
			status, body := f.single(f, e)
			return answer(status, body), nil
		}
	})}
	_ = RunPublisher(ctx, f.q, PublisherConfig{Source: "payer", Incarnation: "boot", Key: []byte("k"),
		URL: "https://sink/internal/pa-test/observations", HealthURL: "https://sink/internal/pa-test/health", BatchURL: batchURL, Drain: f.drain, Client: client, Heartbeat: 100 * time.Millisecond,
		Jitter: func(d time.Duration) time.Duration { return d },
		Clock:  func() time.Time { return f.now },
		Wait: func(ctx context.Context, d time.Duration) error {
			f.advance(d)
			return ctx.Err()
		}})
}

func emitN(t *testing.T, q *Queue, n int, kind string) {
	t.Helper()
	for i := 0; i < n; i++ {
		if !q.TryEmit(Event{Kind: kind}) {
			t.Fatalf("event %d was not queued", i+1)
		}
	}
}

func checkHealth(t *testing.T, q *Queue, now time.Time, want Health) {
	t.Helper()
	h := q.Health(now)
	if h.Pending != 0 || h.Acknowledged != want.Acknowledged || h.Discarded != want.Discarded || h.Dropped != want.Dropped || h.DroppedBy != want.DroppedBy || !h.CountsAgree() {
		t.Fatalf("health = %+v\nwant counts %+v", h, want)
	}
}

const batchURL = "https://sink/internal/pa-test/observations/batch"

// A backlog goes in batches of at most maxBatchEvents, in order, each signed
// over its whole body, and every event is acknowledged.
func TestBatchDeliversABacklogInFewRequests(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 4096, MaxBytes: 64 << 20})
	emitN(t, q, 300, "x")
	f := newFakeIngest(t, q)
	f.batch = func(_ *fakeIngest, events []Event) (int, string) { return 200, resultsAll(events, batchAccepted) }
	f.run(context.Background(), batchURL)
	if len(f.batches) != 2 || len(f.batches[0]) != maxBatchEvents || len(f.batches[1]) != 300-maxBatchEvents || len(f.singles) != 0 {
		t.Fatalf("batches %d (%d, …), singles %d", len(f.batches), len(f.batches[0]), len(f.singles))
	}
	for i, s := range append(f.batches[0], f.batches[1]...) {
		if s != uint64(i+1) {
			t.Fatalf("sequence %d sent in place %d", s, i+1)
		}
	}
	if !f.signed {
		t.Fatal("a request's signature does not cover its body")
	}
	checkHealth(t, q, f.now, Health{Acknowledged: 300})
	if h := q.Health(f.now); h.LastAcknowledged != 300 {
		t.Fatalf("lastAcknowledged %d", h.LastAcknowledged)
	}
}

// Each event in a batch is settled by its own result. An event the ingest
// could not take now is sent again, alone with the others still owed, and an
// event waiting on its binding is sent again after its wait.
func TestBatchSettlesEachEventByItsResult(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 64, MaxBytes: 1 << 20})
	emitN(t, q, 8, "x")
	f := newFakeIngest(t, q)
	first := map[string]string{"1": batchAccepted, "2": batchScopeIgnored, "3": batchKindNotAdmitted, "4": batchInvalid,
		"5": batchConflict, "6": batchUnavailable, "7": batchBindingPending, "8": "a_later_status"}
	f.batch = func(f *fakeIngest, events []Event) (int, string) {
		var a batchAnswer
		for _, e := range events {
			status := batchAccepted
			if len(f.batches) == 1 {
				status = first[jsonUint(e.Sequence)]
			}
			a.Results = append(a.Results, batchResult{Sequence: e.Sequence, Status: status})
		}
		raw, _ := json.Marshal(a)
		return 200, string(raw)
	}
	f.run(context.Background(), batchURL)
	// 6 (unavailable), 7 (binding) and 8 (an unknown status) come back; the
	// rest are settled at once.
	var again []uint64
	for _, b := range f.batches[1:] {
		again = append(again, b...)
	}
	if len(again) != 3 || !hasAll(again, 6, 7, 8) {
		t.Fatalf("sent again: %v (batches %v)", again, f.batches)
	}
	checkHealth(t, q, f.now, Health{Acknowledged: 4, Discarded: 2, Dropped: 2, DroppedBy: DropCounts{Invalid: 2}})
}

// The ingest refusing the whole batch leaves every event owed: each is sent
// again after a backoff until its ownership window runs out, then counted as
// expired. A test event that was not accepted is never retried.
func TestBatchRetriesARefusedBatchUntilItsEventsExpire(t *testing.T) {
	for _, status := range []int{503, 401, 400, 500} {
		q := NewQueue(Limits{MaxEvents: 64, MaxBytes: 1 << 20})
		emitN(t, q, 3, "x")
		emitN(t, q, 1, "test")
		f := newFakeIngest(t, q)
		f.rtt = 100 * time.Millisecond
		f.batch = func(*fakeIngest, []Event) (int, string) { return status, `{"code":"unavailable"}` }
		f.run(context.Background(), batchURL)
		if len(f.batches) < 3 || len(f.singles) != 0 {
			t.Fatalf("%d: %d batches, %d singles", status, len(f.batches), len(f.singles))
		}
		for _, b := range f.batches[1:] {
			for _, s := range b {
				if s == 4 {
					t.Fatalf("%d: the test event was retried", status)
				}
			}
		}
		checkHealth(t, q, f.now, Health{Dropped: 4, DroppedBy: DropCounts{Expired: 3, Test: 1}})
	}
}

// An event the ingest keeps answering unavailable is retried until its
// ownership window runs out; the events accepted beside it are not resent.
func TestBatchRetriesAnUnavailableEventUntilItExpires(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 64, MaxBytes: 1 << 20})
	emitN(t, q, 3, "x")
	f := newFakeIngest(t, q)
	f.rtt = 50 * time.Millisecond
	f.batch = func(_ *fakeIngest, events []Event) (int, string) {
		var a batchAnswer
		for _, e := range events {
			status := batchAccepted
			if e.Sequence == 2 {
				status = batchUnavailable
			}
			a.Results = append(a.Results, batchResult{Sequence: e.Sequence, Status: status})
		}
		raw, _ := json.Marshal(a)
		return 200, string(raw)
	}
	f.run(context.Background(), batchURL)
	for _, b := range f.batches[1:] {
		if len(b) != 1 || b[0] != 2 {
			t.Fatalf("resent %v", b)
		}
	}
	if len(f.batches) < 3 {
		t.Fatalf("the unavailable event was not retried: %v", f.batches)
	}
	checkHealth(t, q, f.now, Health{Acknowledged: 2, Dropped: 1, DroppedBy: DropCounts{Expired: 1}})
}

// A sink that does not speak the batch protocol, by refusing the path or by a
// success with no result for each event, gets single-event requests for the
// rest of the run, and nothing it was sent in the batch is lost or counted
// twice.
func TestBatchFallsBackToSingleEventsForASinkWithoutBatches(t *testing.T) {
	for name, reply := range map[string]func([]Event) (int, string){
		"404":                    func([]Event) (int, string) { return 404, "" },
		"405":                    func([]Event) (int, string) { return 405, "" },
		"a bare success":         func([]Event) (int, string) { return 204, "" },
		"a success with no JSON": func([]Event) (int, string) { return 200, "ok" },
		"too few results":        func(e []Event) (int, string) { return 200, resultsAll(e[1:], batchAccepted) },
		"results out of order":   func(e []Event) (int, string) { return 200, resultsAll([]Event{e[1], e[0], e[2]}, batchAccepted) },
		// Every result is there, but the answer runs past what the
		// publisher reads, so it is not read as the ingest's verdict.
		"a cut-off answer": func(e []Event) (int, string) {
			return 200, resultsAll(e, batchAccepted) + strings.Repeat(" ", maxBatchAnswer)
		},
	} {
		// The header was seen (every heartbeat answer carries it), so this
		// is also an ingest rolled back below batches after saying it took
		// them.
		q := NewQueue(Limits{MaxEvents: 64, MaxBytes: 1 << 20})
		emitN(t, q, 3, "x")
		f := newFakeIngest(t, q)
		f.rtt = 10 * time.Millisecond
		f.batch = func(_ *fakeIngest, e []Event) (int, string) { return reply(e) }
		f.run(context.Background(), batchURL)
		if len(f.batches) != 1 || len(f.singles) != 3 {
			t.Fatalf("%s: %d batches, singles %v", name, len(f.batches), f.singles)
		}
		// The events go again at once, without a retry's backoff.
		if gap := f.singleAt[0].Sub(f.batchAt[0]); gap >= minRetryWait {
			t.Fatalf("%s: the first single went %v after the batch", name, gap)
		}
		checkHealth(t, q, f.now, Health{Acknowledged: 3})
	}
}

// A 413 halves the batch for the rest of the run, and everything is still
// delivered in batches.
func TestBatchShrinksOnTooLarge(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 4096, MaxBytes: 64 << 20})
	emitN(t, q, 100, "x")
	f := newFakeIngest(t, q)
	f.rtt = 10 * time.Millisecond
	f.batch = func(_ *fakeIngest, events []Event) (int, string) {
		if len(events) > 30 {
			return 413, `{"code":"body_too_large"}`
		}
		return 200, resultsAll(events, batchAccepted)
	}
	f.run(context.Background(), batchURL)
	var sizes []int
	for _, b := range f.batches {
		sizes = append(sizes, len(b))
	}
	if len(sizes) < 4 || sizes[0] != 100 || sizes[1] != 50 || sizes[2] != 25 || sizes[3] != 50 || len(f.singles) != 0 {
		t.Fatalf("batch sizes %v, singles %d: want halving, then growing back after a full success", sizes, len(f.singles))
	}
	checkHealth(t, q, f.now, Health{Acknowledged: 100})
}

// A batch is bounded by its events' retained cost too; an event costlier than
// the bound alone goes alone.
func TestBatchIsBoundedByCost(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 64, MaxBytes: 64 << 20, MaxBodyBytes: 8 << 20})
	body := []byte(strings.Repeat("b", maxBatchCost/3))
	for i := 0; i < 4; i++ {
		q.TryEmit(Event{Kind: "x", Body: body})
	}
	q.TryEmit(Event{Kind: "x", Body: []byte(strings.Repeat("b", 2*maxBatchCost))})
	f := newFakeIngest(t, q)
	f.batch = func(_ *fakeIngest, events []Event) (int, string) { return 200, resultsAll(events, batchAccepted) }
	f.run(context.Background(), batchURL)
	var sizes []int
	for _, b := range f.batches {
		sizes = append(sizes, len(b))
	}
	if len(sizes) != 3 || sizes[0] != 2 || sizes[1] != 2 || sizes[2] != 1 {
		t.Fatalf("batch sizes %v", sizes)
	}
	checkHealth(t, q, f.now, Health{Acknowledged: 5})
}

// The publisher stopping while a batch is out counts its events as stopped.
func TestBatchStoppedWhileSent(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 64, MaxBytes: 1 << 20})
	emitN(t, q, 3, "x")
	f := newFakeIngest(t, q)
	f.batch = func(f *fakeIngest, _ []Event) (int, string) {
		f.cancel()
		return 503, ""
	}
	f.run(context.Background(), batchURL)
	checkHealth(t, q, f.now, Health{Dropped: 3, DroppedBy: DropCounts{Stopped: 3}})
}

// A measured loss, replayed: a gateway emitting faster than one event per
// ingest round trip (60 ms) fills its queue and drops what does not fit. At
// 27 events a second, one event per request into a 512-event queue drops
// events as queue full; batches take three times that rate for two minutes
// into a 4,096-event queue with no drop.
func TestBatchKeepsUpWhereOneEventPerRequestDropped(t *testing.T) {
	run := func(batch string, rate, maxEvents int) Health {
		q := NewQueue(Limits{MaxEvents: maxEvents, MaxBytes: 64 << 20})
		f := newFakeIngest(t, q)
		f.rtt, f.rate, f.total = 60*time.Millisecond, rate, rate*120
		f.batch = func(_ *fakeIngest, events []Event) (int, string) { return 200, resultsAll(events, batchAccepted) }
		f.run(context.Background(), batch)
		if f.emitted != f.total {
			t.Fatalf("emitted %d of %d", f.emitted, f.total)
		}
		return q.Health(f.now)
	}
	if h := run(batchURL, 81, 4096); h.Acknowledged != 81*120 || h.Dropped != 0 || h.LastSequence != 81*120 {
		t.Fatalf("batched at 81/s: %+v", h)
	}
	if h := run("", 27, 512); h.DroppedBy.QueueFull == 0 || h.Dropped != h.DroppedBy.QueueFull || !h.CountsAgree() {
		t.Fatalf("one event per request kept up at 27/s: %+v", h)
	}
}

func jsonUint(n uint64) string { raw, _ := json.Marshal(n); return string(raw) }

func hasAll(got []uint64, want ...uint64) bool {
	seen := map[uint64]bool{}
	for _, g := range got {
		seen[g] = true
	}
	for _, w := range want {
		if !seen[w] {
			return false
		}
	}
	return true
}

// A sink that never says it takes batches, such as a participant's own sink
// that answers every path, gets one event per request and never a batch
// request; one that says so gets batches.
func TestBatchOnlyWhenTheIngestSaysSo(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 64, MaxBytes: 1 << 20})
	emitN(t, q, 5, "x")
	f := newFakeIngest(t, q)
	f.noBatches = true
	f.batch = func(_ *fakeIngest, events []Event) (int, string) { return 204, "" }
	f.run(context.Background(), batchURL)
	if len(f.batches) != 0 || len(f.singles) != 5 {
		t.Fatalf("batches %v, singles %v", f.batches, f.singles)
	}
	checkHealth(t, q, f.now, Health{Acknowledged: 5})
}

// After a fallback, a heartbeat answer that says batches starts them again.
func TestBatchResumesWhenTheIngestSaysSoAgain(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 64, MaxBytes: 1 << 20})
	emitN(t, q, 3, "x")
	f := newFakeIngest(t, q)
	f.rtt = 10 * time.Millisecond
	f.rate, f.total = 20, 40 // more events arrive across later heartbeats
	f.batch = func(f *fakeIngest, events []Event) (int, string) {
		if len(f.batches) == 1 {
			return 404, ""
		}
		return 200, resultsAll(events, batchAccepted)
	}
	f.run(context.Background(), batchURL)
	if len(f.batches) < 2 || len(f.singles) == 0 {
		t.Fatalf("batches %d, singles %d: no fallback then resume", len(f.batches), len(f.singles))
	}
	checkHealth(t, q, f.now, Health{Acknowledged: 43})
}

// A 413 for a batch of one event is that event too large for the ingest: it
// is dropped as oversized after one try, and the batches after it keep their
// size.
func TestBatchDropsAnEventTheIngestCannotTake(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 4096, MaxBytes: 64 << 20, MaxBodyBytes: 8 << 20})
	q.TryEmit(Event{Kind: "x", Body: []byte(strings.Repeat("b", 2*maxBatchCost))})
	emitN(t, q, 299, "x")
	f := newFakeIngest(t, q)
	f.rtt = 60 * time.Millisecond
	f.batch = func(_ *fakeIngest, events []Event) (int, string) {
		for _, e := range events {
			if len(e.Body) > maxBatchCost {
				return 413, `{"code":"body_too_large"}`
			}
		}
		return 200, resultsAll(events, batchAccepted)
	}
	f.run(context.Background(), batchURL)
	var sizes []int
	for _, b := range f.batches {
		sizes = append(sizes, len(b))
	}
	if len(sizes) != 3 || sizes[0] != 1 || sizes[1] != maxBatchEvents || sizes[2] != 299-maxBatchEvents {
		t.Fatalf("batch sizes %v", sizes)
	}
	checkHealth(t, q, f.now, Health{Acknowledged: 299, Dropped: 1, DroppedBy: DropCounts{Oversized: 1}})
}

// A heartbeat answer that refuses the heartbeat starts no batching, even with
// the header.
func TestBatchNotStartedByARefusedHeartbeat(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 64, MaxBytes: 1 << 20})
	emitN(t, q, 3, "x")
	f := newFakeIngest(t, q)
	f.healthStatus = 503
	f.batch = func(_ *fakeIngest, events []Event) (int, string) { return 200, resultsAll(events, batchAccepted) }
	f.run(context.Background(), batchURL)
	if len(f.batches) != 0 || len(f.singles) != 3 {
		t.Fatalf("batches %d, singles %d", len(f.batches), len(f.singles))
	}
}

// A batch refused whole waits out the backoff before it is sent again.
func TestBatchRefusedWholeWaitsBeforeItIsSentAgain(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 64, MaxBytes: 1 << 20})
	emitN(t, q, 3, "x")
	f := newFakeIngest(t, q)
	f.batch = func(f *fakeIngest, events []Event) (int, string) {
		if len(f.batches) == 1 {
			return 503, `{"code":"unavailable"}`
		}
		return 200, resultsAll(events, batchAccepted)
	}
	f.run(context.Background(), batchURL)
	if len(f.batches) != 2 || f.batchAt[1].Sub(f.batchAt[0]) < minRetryWait {
		t.Fatalf("batches %v at %v", f.batches, f.batchAt)
	}
	checkHealth(t, q, f.now, Health{Acknowledged: 3})
}

// Events the publisher cannot send are dropped before the request, by their
// reason, and the rest of the batch goes.
func TestBatchDropsUnsendableEventsBeforeTheRequest(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 64, MaxBytes: 1 << 20, MaxBodyBytes: 1 << 20})
	q.TryEmit(Event{Kind: "x", Time: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}) // unencodable
	q.TryEmit(Event{Kind: "x", Body: []byte(strings.Repeat("b", 3*maxPublisherIdentityBytes))})
	emitN(t, q, 2, "x")
	q.mu.Lock()
	q.items[1].bytes = 0 // admitted, then found too large to retain at send
	q.limits.MaxBytes = 0
	q.mu.Unlock()
	f := newFakeIngest(t, q)
	f.batch = func(_ *fakeIngest, events []Event) (int, string) { return 200, resultsAll(events, batchAccepted) }
	f.run(context.Background(), batchURL)
	if len(f.batches) != 1 || len(f.batches[0]) != 2 {
		t.Fatalf("batches %v", f.batches)
	}
	checkHealth(t, q, f.now, Health{Acknowledged: 2, Dropped: 2, DroppedBy: DropCounts{Unencodable: 1, Oversized: 1}})
}

// An event emitted without a time is stamped once: every attempt sends the
// same bytes, so a resend of an event the ingest already holds is not a
// conflict.
func TestBatchStampsAnEventOnce(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 64, MaxBytes: 1 << 20})
	q.TryEmit(Event{Kind: "x"})
	f := newFakeIngest(t, q)
	var times []time.Time
	f.batch = func(f *fakeIngest, events []Event) (int, string) {
		times = append(times, events[0].Time)
		f.now = f.now.Add(time.Second)
		if len(f.batches) == 1 {
			return 503, ""
		}
		return 200, resultsAll(events, batchAccepted)
	}
	f.run(context.Background(), batchURL)
	if len(times) != 2 || !times[0].Equal(times[1]) || times[0].IsZero() {
		t.Fatalf("sent with times %v", times)
	}
}

// The publisher stopping while a batch is out, when the ingest's answer has a
// result for each event, settles the batch by that answer: nothing it took
// is counted as stopped.
func TestBatchStoppedWithAnAnswerSettlesIt(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 64, MaxBytes: 1 << 20})
	emitN(t, q, 3, "x")
	f := newFakeIngest(t, q)
	f.batch = func(f *fakeIngest, events []Event) (int, string) {
		f.cancel()
		return 200, resultsAll(events, batchAccepted)
	}
	f.run(context.Background(), batchURL)
	checkHealth(t, q, f.now, Health{Acknowledged: 3})
}
