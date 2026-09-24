package diagnostics

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type readerOnly struct{ io.Reader }
type shortReaderFromWriter struct {
	plainWriter
	got []byte
	err error
}

func (w *shortReaderFromWriter) ReadFrom(r io.Reader) (int64, error) {
	b := make([]byte, 3)
	n, _ := io.ReadFull(r, b)
	w.got = append(w.got, b[:n]...)
	return int64(n), w.err
}
func TestReaderFromDelegatesExactCountErrorAndCapture(t *testing.T) {
	want := errors.New("synthetic readerfrom")
	base := &shortReaderFromWriter{plainWriter: plainWriter{h: make(http.Header)}, err: want}
	var response Event
	ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n, err := io.Copy(w, readerOnly{strings.NewReader("abcdef")})
		if n != 3 || !errors.Is(err, want) {
			t.Fatalf("copy=%d %v", n, err)
		}
	}), func(e Event) bool {
		if e.Kind == "http-request-response" {
			response = e
		}
		return true
	}, nil, time.Now, 64, NewCaptureBudget(1024, 1)).ServeHTTP(base, httptest.NewRequest("GET", "/", nil))
	if string(base.got) != "abc" || string(response.Body) != "abc" {
		t.Fatalf("wire=%q capture=%q", base.got, response.Body)
	}
}

func TestCaptureRetainedCapacityNeverExceedsCredits(t *testing.T) {
	b := NewCaptureBudget(13, 1)
	first, _, release := captureSession(b, 13)
	defer release()
	for _, p := range [][]byte{[]byte("abc"), []byte("defg"), []byte("hijklmn")} {
		first.add(p)
	}
	first.mu.Lock()
	defer first.mu.Unlock()
	if int64(cap(first.data)) > first.held || first.held > 13 {
		t.Fatalf("len=%d cap=%d held=%d", len(first.data), cap(first.data), first.held)
	}
}

func TestPublisherRejectsUnboundedIdentityBeforeMarshal(t *testing.T) {
	q := NewQueue(Limits{})
	q.TryEmit(Event{Kind: "evidence"})
	calls := 0
	err := RunPublisher(context.Background(), q, PublisherConfig{Source: strings.Repeat("s", maxPublisherIdentityBytes+1), Incarnation: "i", URL: "https://sink", Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("called") })}})
	if err == nil || calls != 0 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestPublisherExpiresBeforeHeartbeatAndReleasesAutonomously(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 1})
	q.TryEmit(Event{Kind: "evidence"})
	now := time.Unix(0, 0)
	attempts, heartbeats := 0, 0
	var heartbeatTimes []time.Time
	reached := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), `"state"`) {
			heartbeats++
			if q.Health(now).Dropped == 0 {
				heartbeatTimes = append(heartbeatTimes, now)
			}
		} else {
			attempts++
		}
		return &http.Response{StatusCode: 503, Header: make(http.Header), Body: http.NoBody}, nil
	})}
	done := make(chan error, 1)
	go func() {
		done <- RunPublisher(ctx, q, PublisherConfig{Source: "s", Incarnation: "i", URL: "https://sink", Client: client, Heartbeat: time.Second, Clock: func() time.Time { return now }, Wait: func(context.Context, time.Duration) error {
			now = now.Add(time.Second)
			if now.Sub(time.Unix(0, 0)) >= 30*time.Second {
				select {
				case reached <- struct{}{}:
				default:
				}
			}
			return nil
		}})
	}()
	<-reached
	deadline := time.After(time.Second)
	for q.Health(now).Dropped == 0 {
		select {
		case <-deadline:
			t.Fatal("item did not expire autonomously")
		default:
		}
	}
	cancel()
	<-done
	if !q.TryEmit(Event{Kind: "released"}) {
		t.Fatal("expired item retained reservation")
	}
	for _, at := range heartbeatTimes {
		if !at.Before(time.Unix(0, 0).Add(30 * time.Second)) {
			t.Fatalf("heartbeat at expiry: %v", at)
		}
	}
	if attempts == 0 {
		t.Fatalf("attempts=%d heartbeats=%d", attempts, heartbeats)
	}
}

func TestHeadersCompleteMarksCapAndBudgetOmission(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget *CaptureBudget
		value  string
	}{{"cap", NewCaptureBudget(1<<20, 1), strings.Repeat("x", int(maxCapturedHeaderBytes))}, {"credits", NewCaptureBudget(1, 1), "x"}} {
		t.Run(tc.name, func(t *testing.T) {
			var got Event
			r := httptest.NewRequest("GET", "/", nil)
			r.Header.Set("X-Large", tc.value)
			ObserveHTTP(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), func(e Event) bool {
				if e.Kind == "http-request" {
					got = e
				}
				return true
			}, nil, time.Now, 8, tc.budget).ServeHTTP(httptest.NewRecorder(), r)
			if got.HeadersComplete {
				t.Fatal("omitted headers marked complete")
			}
		})
	}
}
