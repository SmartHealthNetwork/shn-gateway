package diagnostics

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type receivedDiagnostic struct {
	path   string
	raw    []byte
	header http.Header
}

func TestPublisherUsesSeparateSignedEventAndHealthEndpoints(t *testing.T) {
	q := NewQueue(Limits{})
	q.TryEmit(Event{Kind: "test", Body: []byte("synthetic")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var got []receivedDiagnostic
	base := time.Now()
	var nowNanos atomic.Int64
	nowNanos.Store(base.UnixNano())
	clock := func() time.Time { return time.Unix(0, nowNanos.Load()) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, receivedDiagnostic{r.URL.Path, raw, r.Header.Clone()})
		mu.Unlock()
		if r.URL.Path == "/observations" {
			nowNanos.Store(base.Add(time.Second).UnixNano())
		}
		if r.URL.Path == "/health" {
			cancel()
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	err := RunPublisher(ctx, q, PublisherConfig{Source: "src", Incarnation: "boot", URL: server.URL + "/observations", HealthURL: server.URL + "/health", Key: []byte("key"), Clock: clock, Wait: func(context.Context, time.Duration) error { t.Fatal("unexpected retry"); return nil }, Heartbeat: time.Millisecond})
	if err != context.Canceled {
		t.Fatalf("run: %v", err)
	}
	mu.Lock()
	requests := append([]receivedDiagnostic(nil), got...)
	mu.Unlock()
	if len(requests) != 2 || requests[0].path != "/observations" || requests[1].path != "/health" {
		t.Fatalf("paths: %+v", requests)
	}
	for _, item := range requests {
		h := item.header
		if h.Get(HeaderEvidenceSignature) != Sign([]byte("key"), "src", "boot", h.Get(HeaderEvidenceTime), item.raw) {
			t.Fatalf("bad signature for %s", item.path)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(item.raw, &fields); err != nil {
			t.Fatal(err)
		}
		if item.path == "/observations" && (fields["kind"] == nil || fields["state"] != nil) {
			t.Fatalf("event schema: %s", item.raw)
		}
		if item.path == "/health" && (fields["state"] == nil || fields["kind"] != nil) {
			t.Fatalf("health schema: %s", item.raw)
		}
	}
	if q.Health(time.Now()).LastAcknowledged != 1 {
		t.Fatal("event was not acknowledged")
	}
}

func TestPublisherWrongHealthEndpointLeavesVisibleCoverageGap(t *testing.T) {
	q := NewQueue(Limits{})
	q.TryEmit(Event{Kind: "test"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var events, rejectedHealth, acceptedHealth int
	base := time.Now()
	var nowNanos atomic.Int64
	nowNanos.Store(base.UnixNano())
	clock := func() time.Time { return time.Unix(0, nowNanos.Load()) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		switch r.URL.Path {
		case "/observations":
			events++
			nowNanos.Store(base.Add(time.Second).UnixNano())
		case "/wrong-health":
			rejectedHealth++
			w.WriteHeader(http.StatusNotFound)
			cancel()
		case "/health":
			acceptedHealth++
		}
		mu.Unlock()
	}))
	defer server.Close()
	err := RunPublisher(ctx, q, PublisherConfig{URL: server.URL + "/observations", HealthURL: server.URL + "/wrong-health", Clock: clock, Wait: func(context.Context, time.Duration) error { t.Fatal("unexpected retry"); return nil }, Heartbeat: time.Millisecond})
	if err != context.Canceled {
		t.Fatalf("run: %v", err)
	}
	mu.Lock()
	e, rejected, accepted := events, rejectedHealth, acceptedHealth
	mu.Unlock()
	if e != 1 || rejected != 1 || accepted != 0 || q.Health(time.Now()).LastAcknowledged != 1 {
		t.Fatalf("event=%d rejectedHealth=%d acceptedHealth=%d queue=%+v", e, rejected, accepted, q.Health(time.Now()))
	}
}

func TestPublisherRejectsInvalidConfiguredHealthURLAtStartup(t *testing.T) {
	q := NewQueue(Limits{})
	q.TryEmit(Event{Kind: "test"})
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, nil })}
	err := RunPublisher(context.Background(), q, PublisherConfig{URL: "https://example.test/observations", HealthURL: "not a URL", Client: client})
	if err == nil || !strings.Contains(err.Error(), "URL") || calls != 0 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}
