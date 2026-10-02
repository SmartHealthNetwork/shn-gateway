package diagnostics

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A capture that finds no free slot keeps neither headers nor body, and both
// of its events say the budget left the body out: on a handler and on a
// transport alike.
func TestBudgetDetailNamesABodyTheBudgetLeftOut(t *testing.T) {
	budget := NewCaptureBudget(1<<20, 1)
	_, _, releaseHolder := captureSession(budget, 1024) // another exchange holds the only slot
	defer releaseHolder()

	log := &eventLog{}
	h := ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte("answer"))
	}), log.emit, nil, time.Now, 1024, budget)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/", strings.NewReader("request")))
	if rec.Body.String() != "answer" {
		t.Fatalf("capture must not alter the wire: got %q", rec.Body.String())
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte("upstream answer"))
	}))
	defer upstream.Close()
	client := &http.Client{Transport: ObserveTransport(http.DefaultTransport, log.emit, time.Now, 1024, budget)}
	resp, err := client.Post(upstream.URL, "text/plain", strings.NewReader("forward"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	for kind, n := range map[string]int{"http-request": 2, "http-request-response": 1, "http-response": 1} {
		evs := log.byKind(kind)
		if len(evs) != n { // the handler's request and the transport's
			t.Fatalf("%d %s events, want %d", len(evs), kind, n)
		}
		for _, e := range evs {
			if e.BodyComplete || len(e.Body) != 0 || e.Detail != BodyNotKeptCaptureBudget {
				t.Fatalf("%s: complete=%v body=%d detail=%q, want the body not kept and %q", kind, e.BodyComplete, len(e.Body), e.Detail, BodyNotKeptCaptureBudget)
			}
		}
	}
}

// A capture that found a slot but ran out of the budget's bytes keeps a
// prefix and says the budget cut it.
func TestBudgetDetailNamesAPartialBudgetCapture(t *testing.T) {
	budget := NewCaptureBudget(64, 2)
	holder, _, releaseHolder := captureSession(budget, 1024)
	holder.add(bytes.Repeat([]byte("h"), 40)) // another exchange holds 40 of 64
	defer releaseHolder()
	log := &eventLog{}
	h := ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("b"), 100))
	}), log.emit, nil, time.Now, 1024, budget)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	resp := log.byKind("http-request-response")
	if len(resp) != 1 || resp[0].BodyComplete || len(resp[0].Body) == 0 || resp[0].Detail != BodyPartialCaptureBudget {
		t.Fatalf("want a partial prefix with %q, got %+v", BodyPartialCaptureBudget, resp)
	}
	if req := log.byKind("http-request"); len(req) != 1 || req[0].Detail != "" {
		t.Fatalf("the request the budget held whole names no budget: %+v", req)
	}
}

// A body cut by the body cap, with budget to spare, is partial for the cap,
// not the budget; a body captured whole carries no detail.
func TestBodyCapIsNotTheBudget(t *testing.T) {
	budget := NewCaptureBudget(1<<20, 4)
	log := &eventLog{}
	h := ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("b"), 100))
	}), log.emit, nil, time.Now, 32, budget)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	resp := log.byKind("http-request-response")
	if len(resp) != 1 || resp[0].BodyComplete || len(resp[0].Body) != 32 || resp[0].Detail != "" {
		t.Fatalf("want a 32-byte prefix cut by the cap with no budget detail, got %+v", resp)
	}

	whole := &eventLog{}
	h = ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("answer"))
	}), whole.emit, nil, time.Now, 1024, budget)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if len(whole.events) != 2 {
		t.Fatalf("a whole capture emitted %d events, want 2", len(whole.events))
	}
	for _, e := range whole.events {
		if !e.BodyComplete || e.Detail != "" {
			t.Fatalf("a whole capture: complete=%v detail=%q", e.BodyComplete, e.Detail)
		}
	}
	waitBudgetIdle(t, budget)
}

// An event whose body the queue's retained-body limit later cuts keeps the
// budget's detail.
func TestQueueTrimKeepsTheBudgetDetail(t *testing.T) {
	q := NewQueue(Limits{MaxBodyBytes: 4})
	if !q.TryEmit(Event{Kind: "http-response", Body: []byte("a longer prefix"), Detail: BodyPartialCaptureBudget}) {
		t.Fatal("not queued")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	e, err := q.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Body) > 4 || e.Detail != BodyPartialCaptureBudget {
		t.Fatalf("trimmed to %d bytes with detail %q, want the budget's", len(e.Body), e.Detail)
	}
}
