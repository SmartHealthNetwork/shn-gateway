package diagnostics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// slotless serves one request through ObserveHTTP while every capture slot
// is held, and returns the request event it emitted.
func slotless(t *testing.T, budget *CaptureBudget, header http.Header) Event {
	t.Helper()
	e, err := serveSlotless(budget, header)
	if err != "" {
		t.Fatal(err)
	}
	return e
}

// serveSlotless is slotless without a test, for use off the test goroutine.
func serveSlotless(budget *CaptureBudget, header http.Header) (Event, string) {
	log := &eventLog{}
	h := ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte("answer"))
	}), log.emit, func(*http.Request) HTTPInfo {
		return HTTPInfo{CallID: "call-1", Kind: "provider.ingress.request"}
	}, time.Now, 1024, budget)
	req := httptest.NewRequest("POST", "/fhir/Claim/$submit", strings.NewReader("request"))
	for k, vs := range header {
		req.Header[k] = vs
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Body.String() != "answer" {
		return Event{}, "capture must not alter the wire: got " + rec.Body.String()
	}
	evs := log.byKind("provider.ingress.request")
	if len(evs) != 1 {
		return Event{}, "not one ingress event"
	}
	return evs[0], ""
}

// holdAll holds every capture slot of budget until the returned func runs.
func holdAll(budget *CaptureBudget, slots int) func() {
	var releases []func()
	for range slots {
		_, _, release := captureSession(budget, 1024)
		releases = append(releases, release)
	}
	return func() {
		for _, r := range releases {
			r()
		}
	}
}

// An exchange that finds no capture slot keeps no body and no header but the
// door's trace proof, so its ingress still links to the door call:
// the proof alone, its first value, as sent; every other header is dropped,
// and the headers are not complete.
func TestSlotlessCaptureKeepsTheTraceProofAlone(t *testing.T) {
	budget := NewCaptureBudget(1<<20, 1)
	defer holdAll(budget, 1)()
	proof := TraceProof([]byte("door-key"), "call-1", "POST", "/fhir/Claim/$submit", time.Now())
	e := slotless(t, budget, http.Header{
		"X-Shn-Test-Trace": {proof, "a-second-value"},
		"Authorization":    {"Bearer secret-token"},
		"X-Other":          {"other"},
	})
	if got := e.Headers.Values("X-SHN-Test-Trace"); len(got) != 1 || got[0] != proof {
		t.Fatalf("trace proof = %q, want the first value alone", got)
	}
	if len(e.Headers) != 1 || e.HeadersComplete {
		t.Fatalf("headers = %v (complete %v), want the trace proof alone and not complete", e.Headers, e.HeadersComplete)
	}
	if e.BodyComplete || len(e.Body) != 0 || e.Detail != BodyNotKeptCaptureBudget {
		t.Fatalf("body complete=%v len=%d detail=%q, want it not kept", e.BodyComplete, len(e.Body), e.Detail)
	}
}

// A trace header past its bound is not kept without a slot, and a request
// without one keeps no header at all.
func TestSlotlessCaptureKeepsNoOversizedOrMissingProof(t *testing.T) {
	budget := NewCaptureBudget(1<<20, 1)
	defer holdAll(budget, 1)()
	for name, h := range map[string]http.Header{
		"oversized":   {"X-Shn-Test-Trace": {strings.Repeat("a", maxSlotlessTraceBytes+1)}, "X-Other": {"other"}},
		"at the cap":  {"X-Shn-Test-Trace": {strings.Repeat("a", maxSlotlessTraceBytes)}},
		"none":        {"X-Other": {"other"}},
		"empty value": {"X-Shn-Test-Trace": {""}},
	} {
		e := slotless(t, budget, h)
		want := 0
		if name == "at the cap" {
			want = 1
		}
		if len(e.Headers) != want {
			t.Errorf("%s: headers = %v, want %d", name, e.Headers, want)
		}
		// The proof alone, kept whole, is every header: complete.
		if name == "at the cap" && !e.HeadersComplete {
			t.Errorf("%s: the proof was the only header, yet the headers are not complete", name)
		}
	}
	// The bound is 2 KiB, as STABILITY.md states it.
	if maxSlotlessTraceBytes != 2048 {
		t.Fatalf("maxSlotlessTraceBytes = %d, want 2048", maxSlotlessTraceBytes)
	}
	if e := slotless(t, budget, http.Header{"X-Shn-Test-Trace": {strings.Repeat("a", 2049)}}); len(e.Headers) != 0 {
		t.Errorf("a 2049-byte proof was kept: %v", e.Headers)
	}
}

// Slotless exchanges reserve nothing from the capture budget, and each event
// keeps its proof alone, at most maxSlotlessTraceBytes of header capture, so
// N of them at once hold at most N times that.
func TestSlotlessTraceCaptureIsBounded(t *testing.T) {
	const n = 32
	budget := NewCaptureBudget(1<<20, 2)
	defer holdAll(budget, 2)()
	big := strings.Repeat("p", maxSlotlessTraceBytes)
	var wg sync.WaitGroup
	events, errs := make([]Event, n), make([]string, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			events[i], errs[i] = serveSlotless(budget, http.Header{"X-Shn-Test-Trace": {big}, "X-Padding": {strings.Repeat("x", 8192)}})
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != "" {
			t.Fatal(err)
		}
	}
	var total int
	for _, e := range events {
		// Each kept the proof, and only the proof.
		if got := e.Headers.Values("X-SHN-Test-Trace"); len(e.Headers) != 1 || len(got) != 1 || got[0] != big {
			t.Fatalf("a slotless event keeps %d headers, not the proof alone", len(e.Headers))
		}
		size := 0
		for k, vs := range e.Headers {
			size += len(k)
			for _, v := range vs {
				size += len(v)
			}
		}
		if size > len("X-Shn-Test-Trace")+maxSlotlessTraceBytes {
			t.Fatalf("a slotless event holds %d header bytes", size)
		}
		total += size
	}
	if max := n * (len("X-Shn-Test-Trace") + maxSlotlessTraceBytes); total > max {
		t.Fatalf("%d slotless events hold %d header bytes, over %d", n, total, max)
	}
	budget.mu.Lock()
	used := budget.usedBytes
	budget.mu.Unlock()
	if used != 0 {
		t.Fatalf("slotless captures reserved %d budget bytes", used)
	}
}
