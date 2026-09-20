package diagnostics

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type cumulativeReaderFromWriter struct {
	h    http.Header
	wire []byte
}

func (w *cumulativeReaderFromWriter) Header() http.Header { return w.h }
func (w *cumulativeReaderFromWriter) WriteHeader(int)     {}
func (w *cumulativeReaderFromWriter) Write(p []byte) (int, error) {
	w.wire = append(w.wire, p...)
	return len(p), nil
}
func (w *cumulativeReaderFromWriter) ReadFrom(r io.Reader) (int64, error) {
	b, err := io.ReadAll(r)
	w.wire = append(w.wire, b...)
	return int64(len(b)), err
}

func TestReaderFromPreservesPriorAndRepeatedWrites(t *testing.T) {
	base := &cumulativeReaderFromWriter{h: make(http.Header)}
	var response Event
	ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("pre"))
		rf := w.(io.ReaderFrom)
		_, _ = rf.ReadFrom(strings.NewReader("abc"))
		_, _ = rf.ReadFrom(strings.NewReader("xyz"))
	}), func(e Event) bool {
		if e.Kind == "http-request-response" {
			response = e
		}
		return true
	}, nil, time.Now, 64, NewCaptureBudget(1024, 1)).ServeHTTP(base, httptest.NewRequest("GET", "/", nil))
	if string(base.wire) != "preabcxyz" || string(response.Body) != "preabcxyz" {
		t.Fatalf("wire=%q capture=%q", base.wire, response.Body)
	}
}

type emptyReaderFromWriter struct {
	h        http.Header
	statuses []int
}

func (w *emptyReaderFromWriter) Header() http.Header             { return w.h }
func (*emptyReaderFromWriter) Write([]byte) (int, error)         { return 0, nil }
func (w *emptyReaderFromWriter) WriteHeader(s int)               { w.statuses = append(w.statuses, s) }
func (*emptyReaderFromWriter) ReadFrom(io.Reader) (int64, error) { return 0, nil }
func TestEmptyReaderFromDoesNotCommitImplicitStatus(t *testing.T) {
	base := &emptyReaderFromWriter{h: make(http.Header)}
	var response Event
	ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n, err := w.(io.ReaderFrom).ReadFrom(bytes.NewReader(nil))
		if n != 0 || err != nil {
			t.Fatalf("readfrom=%d %v", n, err)
		}
		w.WriteHeader(201)
	}), func(e Event) bool {
		if e.Kind == "http-request-response" {
			response = e
		}
		return true
	}, nil, time.Now, 64, NewCaptureBudget(1024, 1)).ServeHTTP(base, httptest.NewRequest("GET", "/", nil))
	if response.Status != 201 || len(base.statuses) != 1 || base.statuses[0] != 201 {
		t.Fatalf("status=%d wire=%v", response.Status, base.statuses)
	}
}

func TestHeartbeatEncodingCannotStartAfterEvidenceDeadline(t *testing.T) {
	q := NewQueue(Limits{})
	q.TryEmit(Event{Kind: "evidence"})
	base := time.Now()
	var nowNanos atomic.Int64
	nowNanos.Store(base.UnixNano())
	var phase, phaseCalls, httpCalls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		httpCalls.Add(1)
		return &http.Response{StatusCode: 503, Header: make(http.Header), Body: http.NoBody}, nil
	})}
	clock := func() time.Time {
		if phase.Load() == 1 && phaseCalls.Add(1) == 4 {
			nowNanos.Store(base.Add(30 * time.Second).UnixNano())
		}
		return time.Unix(0, nowNanos.Load())
	}
	wait := func(context.Context, time.Duration) error {
		nowNanos.Store(base.Add(time.Second).UnixNano())
		phase.Store(1)
		return nil
	}
	done := make(chan error, 1)
	go func() {
		done <- RunPublisher(ctx, q, PublisherConfig{Source: "s", Incarnation: "i", URL: "https://sink", Client: client, Heartbeat: time.Second, Clock: clock, Wait: wait})
	}()
	deadline := time.After(time.Second)
	for q.Health(time.Unix(0, nowNanos.Load())).Dropped == 0 {
		select {
		case <-deadline:
			t.Fatal("evidence did not expire")
		default:
		}
	}
	cancel()
	<-done
	if httpCalls.Load() != 1 {
		t.Fatalf("HTTP started after encoding crossed deadline; calls=%d", httpCalls.Load())
	}
}
