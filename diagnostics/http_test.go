package diagnostics

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestObserveHTTPIsTransparentAndCapturesConsumedBytes(t *testing.T) {
	body := []byte("not-json Authorization=synthetic")
	var got Event
	h := ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 8)
		_, _ = io.ReadFull(r.Body, buf)
		w.Header().Set("X-Test", "yes")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("reply"))
	}), func(e Event) bool {
		if e.Kind == "request" {
			got = e
		}
		return true
	}, func(*http.Request) HTTPInfo { return HTTPInfo{CallID: "c", Kind: "request"} }, func() time.Time { return time.Unix(2, 0) }, 1024, NewCaptureBudget(4096, 2))
	r := httptest.NewRequest("POST", "https://example.test/a%2Fb?b=2&a=1", bytes.NewReader(body))
	r.Header.Set("Authorization", "Synthetic credential")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 201 || w.Body.String() != "reply" || w.Header().Get("X-Test") != "yes" {
		t.Fatalf("response %d %q %#v", w.Code, w.Body.String(), w.Header())
	}
	if string(got.Body) != string(body[:8]) || got.BodyComplete {
		t.Fatalf("body=%q complete=%v", got.Body, got.BodyComplete)
	}
	if got.Headers.Get("Authorization") != "Synthetic credential" || got.URL != "/a%2Fb?b=2&a=1" {
		t.Fatalf("event=%+v", got)
	}
	if got.RequestFingerprint.Complete || got.RequestFingerprint.ObservedBytes != 8 {
		t.Fatalf("fingerprint=%+v", got.RequestFingerprint)
	}
}

type failingWriter struct {
	header http.Header
	status int
	wrote  []byte
}

func (w *failingWriter) Header() http.Header { return w.header }
func (w *failingWriter) WriteHeader(s int)   { w.status = s }
func (w *failingWriter) Write(p []byte) (int, error) {
	w.wrote = append(w.wrote, p[:2]...)
	return 2, errors.New("synthetic write")
}

func TestObserveHTTPPreservesWriteErrorAndCapturesSuccessfulPrefix(t *testing.T) {
	var got Event
	h := ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n, err := w.Write([]byte("abcdef"))
		if n != 2 || err == nil {
			t.Fatalf("write=%d %v", n, err)
		}
	}), func(e Event) bool {
		if e.Kind == "http-request-response" {
			got = e
		}
		return true
	}, nil, time.Now, 20, NewCaptureBudget(100, 2))
	w := &failingWriter{header: make(http.Header)}
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if string(w.wrote) != "ab" || string(got.Body) != "ab" || got.BodyComplete {
		t.Fatalf("wire=%q event=%q complete=%v", w.wrote, got.Body, got.BodyComplete)
	}
}

type flushWriter struct {
	*httptest.ResponseRecorder
	flushed bool
}

func (w *flushWriter) Flush() { w.flushed = true }
func TestObserveHTTPPreservesFlushAndDefaultStatus(t *testing.T) {
	var got Event
	h := ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.(http.Flusher).Flush() }), func(e Event) bool { got = e; return true }, nil, time.Now, 10, NewCaptureBudget(100, 1))
	w := &flushWriter{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if !w.flushed || got.Status != 200 {
		t.Fatalf("flushed=%v status=%d", w.flushed, got.Status)
	}
}

func TestObserveTransportDoesNotDrainAndPreservesCanceledRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reads := 0
	body := readCloserFunc{read: func([]byte) (int, error) { reads++; return 0, ctx.Err() }}
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, nil
	})
	var got Event
	tr := ObserveTransport(base, func(e Event) bool { got = e; return true }, time.Now, 100, NewCaptureBudget(100, 1))
	resp, err := tr.RoundTrip(httptest.NewRequest("POST", "https://x.test/a", nil))
	if err != nil {
		t.Fatal(err)
	}
	if reads != 0 {
		t.Fatal("transport pre-read response")
	}
	_, err = resp.Body.Read(make([]byte, 1))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	_ = resp.Body.Close()
	if reads != 1 || got.BodyComplete {
		t.Fatalf("reads=%d event=%+v", reads, got)
	}
}

func TestCaptureBudgetNeverWaitsAndReleases(t *testing.T) {
	b := NewCaptureBudget(4, 1)
	var mu sync.Mutex
	events := 0
	h := ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("abcdef")) }), func(Event) bool { mu.Lock(); events++; mu.Unlock(); return false }, nil, time.Now, 10, b)
	for i := 0; i < 100; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
		if w.Body.String() != "abcdef" {
			t.Fatal("changed response")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if events != 200 {
		t.Fatalf("events=%d", events)
	}
}

func TestRetentionCapDoesNotMakeFullyReadFingerprintPartial(t *testing.T) {
	body := []byte("longer-than-cap")
	var got Event
	h := ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}), func(e Event) bool {
		if e.Kind == "http-request" {
			got = e
		}
		return true
	}, nil, time.Now, 4, NewCaptureBudget(100, 1))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/path?b=2&a=1", bytes.NewReader(body)))
	if string(got.Body) != "long" || got.BodyComplete {
		t.Fatalf("body=%q complete=%v", got.Body, got.BodyComplete)
	}
	if !got.RequestFingerprint.Complete || got.RequestFingerprint.ObservedBytes != int64(len(body)) {
		t.Fatalf("fingerprint=%+v", got.RequestFingerprint)
	}
}

func TestUnavailableCaptureSlotForwardsConcurrentRequests(t *testing.T) {
	budget := NewCaptureBudget(32, 1)
	entered := make(chan struct{})
	release := make(chan struct{})
	var eventsMu sync.Mutex
	var events []Event
	h := ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hold" {
			close(entered)
			<-release
		}
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte("ok"))
	}), func(e Event) bool {
		eventsMu.Lock()
		events = append(events, e)
		eventsMu.Unlock()
		return true
	}, nil, time.Now, 32, budget)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/hold", bytes.NewReader([]byte("first"))))
	}()
	<-entered
	startRejected := make(chan struct{})
	var rejectedWG sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		rejectedWG.Add(1)
		go func() {
			defer wg.Done()
			defer rejectedWG.Done()
			<-startRejected
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("POST", "/rejected", bytes.NewReader([]byte("secret"))))
			if w.Code != 200 || w.Body.String() != "ok" {
				t.Errorf("response %d %q", w.Code, w.Body.String())
			}
		}()
	}
	close(startRejected)
	rejectedWG.Wait()
	close(release)
	wg.Wait()
	eventsMu.Lock()
	defer eventsMu.Unlock()
	partials := 0
	for _, e := range events {
		if e.Kind == "http-request" && e.URL == "/rejected" && !e.RequestFingerprint.Complete && len(e.Body) == 0 {
			partials++
		}
	}
	if partials != 200 {
		t.Fatalf("partial requests=%d", partials)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type readCloserFunc struct{ read func([]byte) (int, error) }

func (r readCloserFunc) Read(p []byte) (int, error) { return r.read(p) }
func (readCloserFunc) Close() error                 { return nil }
