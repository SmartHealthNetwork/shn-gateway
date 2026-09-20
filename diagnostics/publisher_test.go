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
	if len(waits) < 2 || waits[0] != 50*time.Millisecond || waits[1] != 100*time.Millisecond {
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
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
	_ = RunPublisher(ctx, q, PublisherConfig{Source: "payer", Incarnation: "boot", URL: "https://sink", Client: client, Heartbeat: time.Millisecond, Clock: func() time.Time { return now }, Wait: func(_ context.Context, d time.Duration) error {
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
		attempts++
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
