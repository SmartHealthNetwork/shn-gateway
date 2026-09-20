package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestTransportPreservesRequestAndNoBodySentinel(t *testing.T) {
	r := httptest.NewRequest("GET", "https://example.test/", nil)
	original := r.Body
	base := roundTripFunc(func(got *http.Request) (*http.Response, error) {
		if got == r {
			t.Error("caller request passed directly")
		}
		if got.Body != http.NoBody {
			t.Errorf("body=%T, want http.NoBody", got.Body)
		}
		return &http.Response{StatusCode: 204, Header: make(http.Header), Body: http.NoBody}, nil
	})
	resp, err := ObserveTransport(base, func(Event) bool { return true }, time.Now, 32, NewCaptureBudget(128, 1)).RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if r.Body != original {
		t.Fatal("caller request mutated")
	}
}

type asyncBodyTransport struct{ start, done chan struct{} }

func (a asyncBodyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	go func() { close(a.start); _, _ = io.Copy(io.Discard, r.Body); _ = r.Body.Close(); close(a.done) }()
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: http.NoBody}, nil
}
func TestTransportFinalizesRequestOnAsynchronousClose(t *testing.T) {
	start, done := make(chan struct{}), make(chan struct{})
	events := make(chan Event, 2)
	r := httptest.NewRequest("POST", "https://x.test/p", bytes.NewReader([]byte("abcdef")))
	resp, err := ObserveTransport(asyncBodyTransport{start, done}, func(e Event) bool { events <- e; return true }, time.Now, 64, NewCaptureBudget(128, 1)).RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	<-start
	_ = resp.Body.Close()
	<-done
	var req Event
	for i := 0; i < 2; i++ {
		e := <-events
		if e.Kind == "http-request" {
			req = e
		}
	}
	if !req.RequestFingerprint.Complete || req.RequestFingerprint.ObservedBytes != 6 {
		t.Fatalf("fingerprint=%+v", req.RequestFingerprint)
	}
}

type plainWriter struct{ h http.Header }

func (w *plainWriter) Header() http.Header       { return w.h }
func (*plainWriter) Write(p []byte) (int, error) { return len(p), nil }
func (*plainWriter) WriteHeader(int)             {}
func TestWriterDoesNotInventOptionalInterfaces(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, ok := w.(http.Flusher); ok {
			t.Error("invented Flusher")
		}
		if _, ok := w.(http.Hijacker); ok {
			t.Error("invented Hijacker")
		}
		if _, ok := w.(http.Pusher); ok {
			t.Error("invented Pusher")
		}
		if _, ok := w.(io.ReaderFrom); ok {
			t.Error("invented ReaderFrom")
		}
	})
	ObserveHTTP(next, nil, nil, time.Now, 8, NewCaptureBudget(64, 1)).ServeHTTP(&plainWriter{h: make(http.Header)}, httptest.NewRequest("GET", "/", nil))
}

type flushErrorWriter struct {
	plainWriter
	err error
}

func (*flushErrorWriter) Flush()              {}
func (w *flushErrorWriter) FlushError() error { return w.err }
func TestResponseControllerPreservesFlushError(t *testing.T) {
	want := errors.New("synthetic flush")
	base := &flushErrorWriter{plainWriter: plainWriter{h: make(http.Header)}, err: want}
	ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := http.NewResponseController(w).Flush(); !errors.Is(err, want) {
			t.Fatalf("flush error=%v", err)
		}
	}), nil, nil, time.Now, 8, NewCaptureBudget(64, 1)).ServeHTTP(base, httptest.NewRequest("GET", "/", nil))
}

func TestQueueRemovalZerosVacatedEntry(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 2, MaxBytes: 1024, MaxBodyBytes: 100})
	q.TryEmit(Event{Body: []byte("secret")})
	e, _ := q.Next(context.Background())
	q.Acknowledge(e.Sequence)
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) != 0 || (cap(q.items) > 0 && q.items[:cap(q.items)][0].event.Body != nil) {
		t.Fatal("vacated queue entry retained body")
	}
}

func TestContentLengthZeroNonEmptyBodyRequiresEOF(t *testing.T) {
	var got Event
	r := httptest.NewRequest("POST", "/", bytes.NewReader([]byte("x")))
	r.ContentLength = 0
	ObserveHTTP(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), func(e Event) bool {
		if e.Kind == "http-request" {
			got = e
		}
		return true
	}, nil, time.Now, 8, NewCaptureBudget(64, 1)).ServeHTTP(httptest.NewRecorder(), r)
	if got.RequestFingerprint.Complete {
		t.Fatal("unread non-nil body marked complete")
	}
}

func TestQueueWakesTwoWaitingConsumers(t *testing.T) {
	q := NewQueue(Limits{})
	got := make(chan Event, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, err := q.Next(context.Background())
			if err == nil {
				got <- e
			}
		}()
	}
	time.Sleep(time.Millisecond)
	q.TryEmit(Event{Kind: "a"})
	q.TryEmit(Event{Kind: "b"})
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("second waiter remained blocked")
	}
}

func TestStatus101IsFinal(t *testing.T) {
	var got Event
	ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(101) }), func(e Event) bool { got = e; return true }, nil, time.Now, 8, NewCaptureBudget(64, 1)).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if got.Status != 101 {
		t.Fatalf("status=%d", got.Status)
	}
}

func TestDiagnosticCallbackPanicsDoNotSkipHandler(t *testing.T) {
	called := false
	w := httptest.NewRecorder()
	ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called = true; w.WriteHeader(201) }), nil, func(*http.Request) HTTPInfo { panic("boom") }, func() time.Time { panic("clock") }, 8, NewCaptureBudget(64, 1)).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if !called || w.Code != 201 {
		t.Fatalf("called=%v status=%d", called, w.Code)
	}
}

func TestGeneric422IsRetriedNotDiscarded(t *testing.T) {
	q := NewQueue(Limits{})
	q.TryEmit(Event{Kind: "evidence"})
	now := time.Unix(0, 0)
	attempts := 0
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		return &http.Response{StatusCode: 422, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString(`{"code":"other"}`))}, nil
	})}
	_ = RunPublisher(ctx, q, PublisherConfig{Source: "s", Incarnation: "i", URL: "https://sink", Client: client, Clock: func() time.Time { return now }, Wait: func(context.Context, time.Duration) error {
		now = now.Add(time.Second)
		return nil
	}})
	if attempts < 2 || q.Health(now).Dropped != 1 {
		t.Fatalf("attempts=%d health=%+v", attempts, q.Health(now))
	}
}

func TestScopeIgnoredRequiresExactBoundedJSON(t *testing.T) {
	q := NewQueue(Limits{})
	q.TryEmit(Event{Kind: "evidence"})
	ctx, cancel := context.WithCancel(context.Background())
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		cancel()
		return &http.Response{StatusCode: 422, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString(`{"code":"scope_ignored"}`))}, nil
	})}
	_ = RunPublisher(ctx, q, PublisherConfig{Source: "s", Incarnation: "i", URL: "https://sink", Client: client})
	if q.Health(time.Now()).Dropped != 0 {
		t.Fatal("exact scope_ignored counted as capture failure")
	}
}

func TestHeartbeatContinuesDuringEvidenceRetries(t *testing.T) {
	q := NewQueue(Limits{})
	q.TryEmit(Event{Kind: "evidence"})
	now := time.Unix(0, 0)
	ctx, cancel := context.WithCancel(context.Background())
	heartbeats := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		var h Health
		if json.Unmarshal(raw, &h) == nil && h.State != "" {
			heartbeats++
			cancel()
		}
		return &http.Response{StatusCode: 503, Header: make(http.Header), Body: http.NoBody}, nil
	})}
	_ = RunPublisher(ctx, q, PublisherConfig{Source: "s", Incarnation: "i", URL: "https://sink", Client: client, Heartbeat: time.Second, Clock: func() time.Time { return now }, Wait: func(context.Context, time.Duration) error { now = now.Add(time.Second); return nil }})
	if heartbeats == 0 {
		t.Fatal("continuous retries suppressed heartbeat")
	}
}
