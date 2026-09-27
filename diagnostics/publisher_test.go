package diagnostics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPublisherSignsRawEventAndAcknowledges(t *testing.T) {
	q := NewQueue(Limits{})
	q.TryEmit(Event{Kind: "test", Body: []byte("not-json synthetic-secret")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var reqBody []byte
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		reqBody, _ = io.ReadAll(r.Body)
		if r.Header.Get("X-SHN-Evidence-Signature") != Sign([]byte("key"), "src", "boot", r.Header.Get("X-SHN-Evidence-Time"), reqBody) {
			t.Error("bad signature")
		}
		cancel()
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	err := RunPublisher(ctx, q, PublisherConfig{Source: "src", Incarnation: "boot", URL: "https://sink.test", Key: []byte("key"), Client: client, Clock: func() time.Time { return time.Unix(1, 2) }, Wait: func(ctx context.Context, d time.Duration) error { return ctx.Err() }})
	if err != context.Canceled {
		t.Fatalf("err=%v", err)
	}
	var e Event
	if err := json.Unmarshal(reqBody, &e); err != nil {
		t.Fatal(err)
	}
	if e.Source != "src" || e.Incarnation != "boot" || e.Sequence != 1 {
		t.Fatalf("event=%+v", e)
	}
	if h := q.Health(time.Now()); h.LastAcknowledged != 1 {
		t.Fatalf("health=%+v", h)
	}
}

func TestPublisherRetryScheduleExpiresAndReleases(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 1, MaxBytes: 1024, MaxBodyBytes: 100})
	q.TryEmit(Event{Kind: "evidence", Body: []byte("x")})
	now := time.Unix(0, 0)
	var waits []time.Duration
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	ctx, cancel := context.WithCancel(context.Background())
	wait := func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		now = now.Add(d)
		if now.Sub(time.Unix(0, 0)) >= 30*time.Second {
			cancel()
		}
		return nil
	}
	_ = RunPublisher(ctx, q, PublisherConfig{Source: "s", Incarnation: "i", URL: "https://sink", Key: []byte("k"), Client: client, Clock: func() time.Time { return now }, Wait: wait})
	// The first two waits are 50 ms and 100 ms, jittered into [d/2, d].
	if len(waits) < 2 || waits[0] < 25*time.Millisecond || waits[0] > 50*time.Millisecond || waits[1] < 50*time.Millisecond || waits[1] > 100*time.Millisecond {
		t.Fatalf("waits=%v", waits)
	}
	if h := q.Health(now); h.Dropped != 1 {
		t.Fatalf("health=%+v", h)
	}
	if !q.TryEmit(Event{Body: []byte("released")}) {
		t.Fatal("expired item pinned memory")
	}
}

func TestPublisherCancellationDoesNotLeak(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 1})
	q.TryEmit(Event{Kind: "evidence"})
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	go func() { <-entered; cancel() }()
	if err := RunPublisher(ctx, q, PublisherConfig{URL: "https://sink", Client: client}); err != context.Canceled {
		t.Fatalf("err=%v", err)
	}
	if !q.TryEmit(Event{Kind: "released"}) {
		t.Fatal("canceled publication retained reservation")
	}
}

func TestPublisherSendsHealthHeartbeat(t *testing.T) {
	q := NewQueue(Limits{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var body []byte
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ = io.ReadAll(r.Body)
		cancel()
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	err := RunPublisher(ctx, q, PublisherConfig{Source: "src", Incarnation: "boot", URL: "https://sink", Key: []byte("key"), Client: client, Heartbeat: time.Millisecond})
	if err != context.Canceled {
		t.Fatalf("err=%v", err)
	}
	var health Health
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatal(err)
	}
	if health.Source != "src" || health.Incarnation != "boot" || health.State != "healthy" {
		t.Fatalf("health=%+v", health)
	}
}

func TestPublisherDiscardsConfirmedOutOfScopeSeparately(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 1})
	q.TryEmit(Event{Kind: "evidence", Body: []byte("x")})
	ctx, cancel := context.WithCancel(context.Background())
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		cancel()
		return &http.Response{StatusCode: http.StatusUnprocessableEntity, Body: io.NopCloser(strings.NewReader(`{"code":"scope_ignored"}`)), Header: make(http.Header)}, nil
	})}
	_ = RunPublisher(ctx, q, PublisherConfig{Source: "src", Incarnation: "boot", URL: "https://sink", Key: []byte("key"), Client: client})
	if got := q.Health(time.Now()).Dropped; got != 0 {
		t.Fatalf("out-of-scope counted as capture drop: %d", got)
	}
	if !q.TryEmit(Event{Body: []byte("released")}) {
		t.Fatal("out-of-scope item retained its reservation")
	}
}

func TestPublisherGeneratesIncarnationWhenUnset(t *testing.T) {
	q := NewQueue(Limits{})
	q.TryEmit(Event{Kind: "test"})
	ctx, cancel := context.WithCancel(context.Background())
	var incarnation string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		incarnation = r.Header.Get(HeaderEvidenceIncarnation)
		cancel()
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	_ = RunPublisher(ctx, q, PublisherConfig{Source: "src", URL: "https://sink", Key: []byte("key"), Client: client})
	if incarnation == "" {
		t.Fatal("publisher did not generate a boot incarnation")
	}
}

func TestPublisherPendingBindingYieldsToQueuedPrerequisite(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 2, MaxBytes: 4096})
	q.TryEmit(Event{Kind: "leg.sealed", Body: []byte("sealed exact bytes")})
	q.TryEmit(Event{Kind: "provider.ingress.request", Body: []byte("ingress exact bytes")})
	now := time.Unix(0, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prerequisite, seal := false, false
	attempts := []string{}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var e Event
		json.NewDecoder(r.Body).Decode(&e)
		attempts = append(attempts, fmt.Sprintf("%d:%s", e.Sequence, e.Kind))
		status, body := 204, ""
		if e.Kind == "provider.ingress.request" {
			prerequisite = true
		}
		if e.Kind == "leg.sealed" {
			if !prerequisite {
				status = 409
				body = `{"code":"binding_pending"}`
			} else {
				seal = true
				cancel()
			}
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	_ = RunPublisher(ctx, q, PublisherConfig{Source: "provider", Incarnation: "boot", URL: "https://sink", Client: client, Clock: func() time.Time { return now }, Wait: func(_ context.Context, d time.Duration) error {
		now = now.Add(d)
		if now.Sub(time.Unix(0, 0)) >= 5*time.Second {
			cancel()
		}
		return nil
	}})
	if !prerequisite || !seal || q.Health(now).Dropped != 0 {
		t.Fatalf("same-source prerequisite blocked: prerequisite=%v sealed=%v drops=%d", prerequisite, seal, q.Health(now).Dropped)
	}
	if want := []string{"1:leg.sealed", "2:provider.ingress.request", "1:leg.sealed"}; !reflect.DeepEqual(attempts, want) {
		t.Fatalf("publisher changed out-of-order identity: got %v want %v", attempts, want)
	}
	if !q.TryEmit(Event{Body: bytesForTest(3000)}) {
		t.Fatal("acknowledged deferred event retained its byte reservation")
	}
}

func TestPublisherWaitsForCrossSourceBindingAfterFiveSeconds(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 2, MaxBytes: 4096})
	q.TryEmit(Event{Kind: "leg.received"})
	start := time.Unix(0, 0)
	now := start
	// The real-time bound is only a safety net: success cancels at once.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var e Event
		if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
			t.Fatal(err)
		}
		status, body := http.StatusNoContent, ""
		if e.Kind != "" && now.Sub(start) < 8*time.Second {
			status, body = http.StatusConflict, `{"code":"binding_pending"}`
		} else if e.Kind != "" {
			cancel()
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	_ = RunPublisher(ctx, q, PublisherConfig{Source: "payer", Incarnation: "boot", URL: "https://sink", Client: client, Heartbeat: 100 * time.Millisecond, Clock: func() time.Time { return now }, Wait: func(_ context.Context, d time.Duration) error {
		now = now.Add(d)
		return nil
	}})
	health := q.Health(now)
	if health.Dropped != 0 || health.LastAcknowledged != 1 {
		t.Fatalf("cross-source prerequisite lost: elapsed=%s ack=%d dropped=%d", now.Sub(start), health.LastAcknowledged, health.Dropped)
	}
}
func bytesForTest(n int) []byte { return []byte(strings.Repeat("x", n)) }

func TestPublisherDeferredBindingKeepsOriginalDeadlineAndBudget(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 1, MaxBytes: 1024})
	q.TryEmit(Event{Kind: "leg.sealed", Body: []byte("immutable")})
	now := time.Unix(0, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var event Event
		json.NewDecoder(r.Body).Decode(&event)
		if event.Kind == "" {
			return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
		}
		if attempts++; attempts > 700 {
			cancel() // re-posting without waiting fails the check below, not the test timeout
		}
		if q.TryEmit(Event{Kind: "extra"}) {
			t.Error("deferred event freed its item reservation")
		}
		return &http.Response{StatusCode: 409, Body: io.NopCloser(strings.NewReader(`{"code":"binding_pending"}`)), Header: make(http.Header)}, nil
	})}
	_ = RunPublisher(ctx, q, PublisherConfig{Source: "s", Incarnation: "i", URL: "https://sink", Client: client, Clock: func() time.Time { return now }, Wait: func(_ context.Context, d time.Duration) error {
		now = now.Add(d)
		if now.Sub(time.Unix(0, 0)) >= 30*time.Second {
			cancel()
		}
		return nil
	}})
	if now.Sub(time.Unix(0, 0)) > 30*time.Second || attempts > 700 {
		t.Fatalf("deferral renewed ownership deadline: elapsed=%s attempts=%d", now.Sub(time.Unix(0, 0)), attempts)
	}
	if !q.TryEmit(Event{Body: []byte("released")}) {
		t.Fatal("cancellation retained deferred event")
	}
}

// A dependent observation the ingest holds as binding_pending backs off: its
// requests to the one-slot ingest decay instead of re-posting every 50 ms.
// A fixed short re-post kept the slot busy exactly when the prerequisite's
// publisher, at its backoff cap, came back, so the prerequisite could not
// land (a burst stalled past its drain bound).
func TestPublisherPendingObservationBacksOffTheIngest(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 1, MaxBytes: 1024})
	q.TryEmit(Event{Kind: "leg.verified", Body: []byte("waits on its seal")})
	start := time.Unix(0, 0)
	now := start
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var posts int
	var waits []time.Duration
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var e Event
		json.NewDecoder(r.Body).Decode(&e)
		if e.Kind == "" {
			return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
		}
		if posts++; posts > 100 {
			cancel() // re-posting without waiting fails the count below, not the test timeout
		}
		return &http.Response{StatusCode: 409, Body: io.NopCloser(strings.NewReader(`{"code":"binding_pending"}`)), Header: make(http.Header)}, nil
	})}
	_ = RunPublisher(ctx, q, PublisherConfig{Source: "payer", Incarnation: "boot", URL: "https://sink", Client: client, Heartbeat: time.Hour, Clock: func() time.Time { return now }, Wait: func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		now = now.Add(d)
		if now.Sub(start) >= 10*time.Second {
			cancel()
		}
		return nil
	}})
	// Doubling from 50 ms to the 500 ms cap, each jittered into [d/2, d]:
	// at most 5 + 10s/250ms = 45 posts in ten seconds. The fixed 50 ms
	// re-post made 200.
	if posts > 45 || posts < 15 {
		t.Fatalf("a pending observation posted %d times in 10s, want its waits to back off", posts)
	}
	if len(waits) < 6 || waits[0] > minRetryWait || waits[5] < maxRetryWait/2 {
		t.Fatalf("binding waits did not grow to the cap: %v", waits[:min(8, len(waits))])
	}
}

// Every retry wait is jittered from a source seeded by the publisher's
// identity: two publishers retrying against one busy ingest never stay in
// step, and one publisher's schedule is reproducible.
func TestPublisherRetryJitterIsSeededPerPublisher(t *testing.T) {
	schedule := func(incarnation string) []time.Duration {
		q := NewQueue(Limits{MaxEvents: 1, MaxBytes: 1024})
		q.TryEmit(Event{Kind: "evidence", Body: []byte("x")})
		now := time.Unix(0, 0)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var waits []time.Duration
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
		})}
		_ = RunPublisher(ctx, q, PublisherConfig{Source: "provider", Incarnation: incarnation, URL: "https://sink", Client: client, Heartbeat: time.Hour, Clock: func() time.Time { return now }, Wait: func(_ context.Context, d time.Duration) error {
			waits = append(waits, d)
			now = now.Add(d)
			if len(waits) == 12 {
				cancel()
			}
			return nil
		}})
		return waits
	}
	a, again, b := schedule("replica-a"), schedule("replica-a"), schedule("replica-b")
	if !reflect.DeepEqual(a, again) {
		t.Fatalf("one publisher's schedule is not reproducible:\n%v\n%v", a, again)
	}
	if reflect.DeepEqual(a, b) {
		t.Fatalf("two publishers retry in step: %v", a)
	}
	for i, w := range a {
		ceiling := minRetryWait << i
		if ceiling > maxRetryWait {
			ceiling = maxRetryWait
		}
		if w < ceiling/2 || w > ceiling {
			t.Fatalf("wait %d = %v, want within [%v, %v]", i, w, ceiling/2, ceiling)
		}
	}
}

// A pending observation waits on its own: the events behind it, including a
// prerequisite emitted while it waits, are published without waiting out its
// backoff. A publisher that slept the wait held the whole queue, so a burst
// of dependents stalled their prerequisite past its ownership window.
func TestPublisherPendingObservationDoesNotHoldUpTheQueue(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 4, MaxBytes: 4096})
	q.TryEmit(Event{Kind: "leg.verified"})
	start := time.Unix(0, 0)
	now := start
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var posts []string
	pending, sealed := 0, false
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var e Event
		json.NewDecoder(r.Body).Decode(&e)
		if e.Kind == "" {
			return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
		}
		if posts = append(posts, fmt.Sprintf("%s@%dms", e.Kind, now.Sub(start).Milliseconds())); len(posts) > 50 {
			cancel() // a publisher that re-posts without waiting fails here, not at the test timeout
		}
		status, body := 204, ""
		switch {
		case e.Kind == "leg.sealed":
			sealed = true
		case !sealed:
			if pending++; pending == 5 {
				q.TryEmit(Event{Kind: "leg.sealed"})
			}
			status, body = 409, `{"code":"binding_pending"}`
		default:
			cancel()
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	_ = RunPublisher(ctx, q, PublisherConfig{Source: "payer", Incarnation: "boot", URL: "https://sink", Client: client, Heartbeat: time.Hour,
		Jitter: func(d time.Duration) time.Duration { return d },
		Clock:  func() time.Time { return now },
		Wait: func(_ context.Context, d time.Duration) error {
			if now = now.Add(d); now.Sub(start) >= 10*time.Second {
				cancel()
			}
			return nil
		}})
	want := []string{"leg.verified@0ms", "leg.verified@50ms", "leg.verified@150ms", "leg.verified@350ms", "leg.verified@750ms", "leg.sealed@750ms", "leg.verified@1250ms"}
	if !reflect.DeepEqual(posts, want) {
		t.Fatalf("posts = %v\nwant    %v (the prerequisite at once, the pending observation when due)", posts, want)
	}
}

// The binding wait is jittered like every other retry wait: the livelock it
// fixes came from publishers re-posting in step.
func TestPublisherBindingWaitIsJittered(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 1, MaxBytes: 1024})
	q.TryEmit(Event{Kind: "leg.verified"})
	now := time.Unix(0, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var waits []time.Duration
	posts := 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		if posts++; posts > 50 {
			cancel() // a publisher that re-posts without waiting fails here, not at the test timeout
		}
		return &http.Response{StatusCode: 409, Body: io.NopCloser(strings.NewReader(`{"code":"binding_pending"}`)), Header: make(http.Header)}, nil
	})}
	_ = RunPublisher(ctx, q, PublisherConfig{Source: "payer", Incarnation: "boot", URL: "https://sink", Client: client, Heartbeat: time.Hour,
		Jitter: func(d time.Duration) time.Duration { return d / 2 },
		Clock:  func() time.Time { return now },
		Wait: func(_ context.Context, d time.Duration) error {
			waits = append(waits, d)
			now = now.Add(d)
			if len(waits) == 7 {
				cancel()
			}
			return nil
		}})
	ms := time.Millisecond
	if want := []time.Duration{25 * ms, 50 * ms, 100 * ms, 200 * ms, 250 * ms, 250 * ms, 250 * ms}; !reflect.DeepEqual(waits, want) {
		t.Fatalf("binding waits = %v, want %v", waits, want)
	}
}

// An event's binding wait leaves with it: the next pending observation starts
// again at the minimum.
func TestPublisherBindingWaitLeavesWithItsEvent(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 2, MaxBytes: 4096})
	q.TryEmit(Event{Kind: "first"})
	now := time.Unix(0, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var waits []time.Duration
	firstPosts, posts := 0, 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var e Event
		json.NewDecoder(r.Body).Decode(&e)
		if posts++; posts > 50 {
			cancel() // a publisher that re-posts without waiting fails here, not at the test timeout
		}
		if e.Kind == "first" {
			if firstPosts++; firstPosts == 5 {
				q.TryEmit(Event{Kind: "second"})
				return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
			}
		}
		return &http.Response{StatusCode: 409, Body: io.NopCloser(strings.NewReader(`{"code":"binding_pending"}`)), Header: make(http.Header)}, nil
	})}
	_ = RunPublisher(ctx, q, PublisherConfig{Source: "payer", Incarnation: "boot", URL: "https://sink", Client: client, Heartbeat: time.Hour,
		Jitter: func(d time.Duration) time.Duration { return d },
		Clock:  func() time.Time { return now },
		Wait: func(_ context.Context, d time.Duration) error {
			waits = append(waits, d)
			now = now.Add(d)
			if len(waits) == 6 {
				cancel()
			}
			return nil
		}})
	ms := time.Millisecond
	if want := []time.Duration{50 * ms, 100 * ms, 200 * ms, 400 * ms, 50 * ms, 100 * ms}; !reflect.DeepEqual(waits, want) {
		t.Fatalf("binding waits = %v, want %v (the second event starting over)", waits, want)
	}
}

// While a pending observation waits to fall due, the heartbeat still goes
// out on time: no wait runs past it.
func TestPublisherPendingObservationKeepsTheHeartbeat(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 1, MaxBytes: 1024})
	q.TryEmit(Event{Kind: "leg.verified"})
	start := time.Unix(0, 0)
	now := start
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	heartbeat := 100 * time.Millisecond
	var beats []time.Duration
	posts := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var e Event
		json.NewDecoder(r.Body).Decode(&e)
		if e.Kind == "" {
			beats = append(beats, now.Sub(start))
			return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
		}
		if posts++; posts > 200 {
			cancel() // re-posting without waiting fails the heartbeat count, not the test timeout
		}
		return &http.Response{StatusCode: 409, Body: io.NopCloser(strings.NewReader(`{"code":"binding_pending"}`)), Header: make(http.Header)}, nil
	})}
	_ = RunPublisher(ctx, q, PublisherConfig{Source: "payer", Incarnation: "boot", URL: "https://sink", Client: client, Heartbeat: heartbeat,
		Jitter: func(d time.Duration) time.Duration { return d },
		Clock:  func() time.Time { return now },
		Wait: func(_ context.Context, d time.Duration) error {
			if now = now.Add(d); now.Sub(start) >= 3*time.Second {
				cancel()
			}
			return nil
		}})
	if len(beats) < 25 {
		t.Fatalf("%d heartbeats in 3s at a %v interval: %v", len(beats), heartbeat, beats)
	}
	var prev time.Duration
	for i, beat := range beats {
		if gap := beat - prev; gap > heartbeat {
			t.Fatalf("heartbeat %d came %v after the one before, want at most %v", i, gap, heartbeat)
		}
		prev = beat
	}
}
