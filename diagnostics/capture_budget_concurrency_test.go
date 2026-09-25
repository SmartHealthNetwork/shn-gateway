package diagnostics

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// budgetIdle reports whether every reservation and slot has been returned.
func budgetIdle(b *CaptureBudget) (int64, int, bool) {
	b.mu.Lock()
	used := b.usedBytes
	b.mu.Unlock()
	return used, len(b.slots), used == 0 && len(b.slots) == 0
}

func waitBudgetIdle(t *testing.T, b *CaptureBudget) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		used, slots, idle := budgetIdle(b)
		if idle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("capture budget not returned: usedBytes=%d slots=%d", used, slots)
		}
		time.Sleep(time.Millisecond)
	}
}

type eventLog struct {
	mu     sync.Mutex
	events []Event
}

func (l *eventLog) emit(e Event) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
	return true
}

func (l *eventLog) byKind(kind string) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Event
	for _, e := range l.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// barrierUpstream answers each request with its own body only once n requests
// are in flight together, so every capture in the exchanges is held at once.
func barrierUpstream(t *testing.T, n int) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	arrived := 0
	all := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		arrived++
		if arrived == n {
			close(all)
		}
		mu.Unlock()
		select {
		case <-all:
		case <-time.After(5 * time.Second):
			http.Error(w, "barrier timeout", http.StatusGatewayTimeout)
			return
		}
		w.Header().Set("Content-Type", "application/fhir+json")
		_, _ = w.Write(append([]byte("answer to "), body...))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A relay that captures its inbound exchange and its forward leg holds four
// bodies per call. Concurrent calls whose bodies fit the shared budget must
// each record their own bodies completely; the budget bounds bytes actually
// retained, not the per-body cap.
func TestObserveHTTPConcurrentRelayedCallsCaptureCompleteBodies(t *testing.T) {
	const calls = 3
	budget := NewCaptureBudget(16<<20, 8)
	upstream := barrierUpstream(t, calls)
	inbound, forward := &eventLog{}, &eventLog{}
	client := &http.Client{Transport: ObserveTransport(http.DefaultTransport, forward.emit, time.Now, 5<<20, budget)}
	relay := ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		resp, err := client.Post(upstream.URL, "application/fhir+json", bytes.NewReader(body))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		_, _ = io.Copy(w, resp.Body)
	}), inbound.emit, func(*http.Request) HTTPInfo { return HTTPInfo{Kind: "door.request"} }, time.Now, 5<<20, budget)
	srv := httptest.NewServer(relay)
	defer srv.Close()

	var wg sync.WaitGroup
	answers := make([][]byte, calls)
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := http.Post(srv.URL, "application/fhir+json", strings.NewReader(fmt.Sprintf(`{"resourceType":"Bundle","id":"call-%d"}`, i)))
			if err != nil {
				t.Errorf("call %d: %v", i, err)
				return
			}
			defer resp.Body.Close()
			answers[i], _ = io.ReadAll(resp.Body)
		}(i)
	}
	wg.Wait()
	srv.Close()

	for kind, log := range map[string]*eventLog{"door.request": inbound, "door.request-response": inbound, "http-request": forward, "http-response": forward} {
		events := log.byKind(kind)
		if len(events) != calls {
			t.Fatalf("%s: %d events, want %d", kind, len(events), calls)
		}
		for _, e := range events {
			if !e.BodyComplete || len(e.Body) == 0 {
				t.Fatalf("%s: BodyComplete=%v body=%q; a small body under a concurrent call must be captured whole", kind, e.BodyComplete, e.Body)
			}
		}
	}
	for i, answer := range answers {
		want := fmt.Sprintf(`answer to {"resourceType":"Bundle","id":"call-%d"}`, i)
		if string(answer) != want {
			t.Fatalf("call %d answered %q, want %q", i, answer, want)
		}
		found := false
		for _, e := range inbound.byKind("door.request-response") {
			found = found || bytes.Equal(e.Body, answer)
		}
		if !found {
			t.Fatalf("call %d: no response capture holds its own answer %q", i, answer)
		}
	}
	waitBudgetIdle(t, budget)
}

// The gateway's own forward-leg capture: concurrent round trips whose bodies
// fit the budget each record complete request and response bodies.
func TestObserveTransportConcurrentRoundTripsCaptureCompleteBodies(t *testing.T) {
	const trips = 4
	budget := NewCaptureBudget(16<<20, 8)
	upstream := barrierUpstream(t, trips)
	log := &eventLog{}
	client := &http.Client{Transport: ObserveTransport(http.DefaultTransport, log.emit, time.Now, 8<<20, budget)}
	var wg sync.WaitGroup
	for i := 0; i < trips; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := client.Post(upstream.URL, "application/fhir+json", strings.NewReader(fmt.Sprintf("leg-%d", i)))
			if err != nil {
				t.Errorf("trip %d: %v", i, err)
				return
			}
			_, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
		}(i)
	}
	wg.Wait()
	for _, kind := range []string{"http-request", "http-response"} {
		events := log.byKind(kind)
		if len(events) != trips {
			t.Fatalf("%s: %d events, want %d", kind, len(events), trips)
		}
		for _, e := range events {
			if !e.BodyComplete || len(e.Body) == 0 {
				t.Fatalf("%s: BodyComplete=%v body=%q under concurrent round trips", kind, e.BodyComplete, e.Body)
			}
		}
	}
	waitBudgetIdle(t, budget)
}

// A body larger than what the budget has left is recorded as an honest
// partial: a prefix no longer than the grant, BodyComplete=false, and the
// caller still receives every byte.
func TestObserveHTTPBodyBeyondRemainingBudgetIsPartial(t *testing.T) {
	budget := NewCaptureBudget(64, 2)
	holder, _, releaseHolder := captureSession(budget, 1024)
	holder.add(bytes.Repeat([]byte("h"), 40)) // another exchange holds 40 of 64
	log := &eventLog{}
	big := bytes.Repeat([]byte("b"), 100)
	h := ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(big)
	}), log.emit, nil, time.Now, 1024, budget)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if !bytes.Equal(rec.Body.Bytes(), big) {
		t.Fatalf("capture must not alter the wire: got %d bytes", rec.Body.Len())
	}
	resp := log.byKind("http-request-response")
	if len(resp) != 1 {
		t.Fatalf("response events=%d", len(resp))
	}
	if resp[0].BodyComplete || len(resp[0].Body) == 0 || len(resp[0].Body) > 24 || !bytes.HasPrefix(big, resp[0].Body) {
		t.Fatalf("want an honest partial prefix of at most the 24 remaining bytes, got complete=%v len=%d", resp[0].BodyComplete, len(resp[0].Body))
	}
	releaseHolder()
	waitBudgetIdle(t, budget)
}

// Budget freed by another exchange after a byte was missed must not resume the
// capture: the retained bytes stay a prefix of the body and never join bytes
// that were apart on the wire.
func TestPartialCaptureStaysAPrefixWhenBudgetFreesMidBody(t *testing.T) {
	budget := NewCaptureBudget(64, 2)
	holder, _, releaseHolder := captureSession(budget, 1024)
	holder.add(bytes.Repeat([]byte("h"), 40))
	s, _, release := captureSession(budget, 1024)
	defer release()
	first, second := bytes.Repeat([]byte("a"), 30), bytes.Repeat([]byte("b"), 10)
	s.add(first)
	releaseHolder()
	s.add(second)
	s.mu.Lock()
	data, observed := append([]byte(nil), s.data...), s.observed
	s.mu.Unlock()
	wire := append(append([]byte(nil), first...), second...)
	if observed != int64(len(wire)) || len(data) == 0 || len(data) >= len(wire) || !bytes.HasPrefix(wire, data) {
		t.Fatalf("partial capture %q (observed %d) is not a strict prefix of the wire %q", data, observed, wire)
	}
}

// Growth keeps every retained byte paid for: the allocation never exceeds the
// reservation, the reservation never exceeds the cap, and small bodies do not
// reserve the cap.
func TestCaptureGrowthReservesWhatItRetains(t *testing.T) {
	budget := NewCaptureBudget(1<<20, 1)
	s, _, release := captureSession(budget, 1<<20)
	for i := 0; i < 300; i++ {
		s.add([]byte("0123456789"))
		s.mu.Lock()
		if int64(cap(s.data)) > s.held || s.held > 1<<20 {
			s.mu.Unlock()
			t.Fatalf("after %d writes: len=%d cap=%d held=%d", i+1, len(s.data), cap(s.data), s.held)
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	held, n := s.held, len(s.data)
	s.mu.Unlock()
	if n != 3000 || held > 2*3000 {
		t.Fatalf("3000 captured bytes held %d (len %d); growth must track bytes seen, not the cap", held, n)
	}
	release()
	s.add(bytes.Repeat([]byte("r"), 8192)) // beyond the current allocation: would have to grow
	if used, slots, idle := budgetIdle(budget); !idle {
		t.Fatalf("a released capture reserved again: usedBytes=%d slots=%d", used, slots)
	}
}

type errorAfterReadTransport struct{}

func (errorAfterReadTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		_, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
	}
	return nil, errors.New("upstream unreachable")
}

// Every reservation is returned whether the exchange completes, fails in the
// transport, is cancelled mid-body, or the handler panics.
func TestCaptureBudgetReturnedOnCompletionErrorAndCancel(t *testing.T) {
	t.Run("transport error", func(t *testing.T) {
		budget := NewCaptureBudget(1<<20, 2)
		client := &http.Client{Transport: ObserveTransport(errorAfterReadTransport{}, (&eventLog{}).emit, time.Now, 1<<20, budget)}
		if _, err := client.Post("http://upstream.invalid/", "text/plain", strings.NewReader("request body")); err == nil {
			t.Fatal("want the transport error")
		}
		waitBudgetIdle(t, budget)
	})
	t.Run("cancelled mid response", func(t *testing.T) {
		budget := NewCaptureBudget(1<<20, 2)
		started := make(chan struct{})
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("first chunk"))
			w.(http.Flusher).Flush()
			close(started)
			<-r.Context().Done()
		}))
		defer upstream.Close()
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, "POST", upstream.URL, strings.NewReader("request body"))
		client := &http.Client{Transport: ObserveTransport(http.DefaultTransport, (&eventLog{}).emit, time.Now, 1<<20, budget)}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		<-started
		buf := make([]byte, 4)
		_, _ = resp.Body.Read(buf)
		cancel()
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		waitBudgetIdle(t, budget)
	})
	t.Run("handler panic", func(t *testing.T) {
		budget := NewCaptureBudget(1<<20, 2)
		h := ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			_, _ = w.Write([]byte("partial answer"))
			panic(http.ErrAbortHandler)
		}), (&eventLog{}).emit, nil, time.Now, 1<<20, budget)
		func() {
			defer func() { _ = recover() }()
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/", strings.NewReader("request body")))
		}()
		waitBudgetIdle(t, budget)
	})
	t.Run("completion", func(t *testing.T) {
		budget := NewCaptureBudget(1<<20, 2)
		h := ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(w, r.Body)
		}), (&eventLog{}).emit, nil, time.Now, 1<<20, budget)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/", strings.NewReader("request body")))
		waitBudgetIdle(t, budget)
	})
}
