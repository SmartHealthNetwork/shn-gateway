package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// clientTimeout is the error a client gives when its deadline passed before
// the answer came: the ingest may have stored the event all the same.
type clientTimeout struct{}

func (clientTimeout) Error() string {
	return "context deadline exceeded (Client.Timeout exceeded while awaiting headers)"
}
func (clientTimeout) Timeout() bool   { return true }
func (clientTimeout) Temporary() bool { return true }

// Delivery is at least once: an event the ingest stored but whose answer the
// publisher never got in time (its client deadline passed) is sent again,
// byte for byte, the Time it was stamped with included. An ingest that keys
// events by source, incarnation and sequence, as the network's does, stores
// it once and answers the resend as it answered the first; the publisher
// acknowledges it once and drops nothing.
func TestResendAfterALostAnswerIsStoredOnce(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 64, MaxBytes: 1 << 20})
	q.TryEmit(Event{Kind: "door.response"}) // no Time: the queue stamps it once
	var mu sync.Mutex
	stored := map[string][]byte{}
	var posts [][]byte
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/health") {
			return answer(204, ""), nil
		}
		var e Event
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Errorf("event body: %v", err)
		}
		key := fmt.Sprintf("%s/%s/%d", e.Source, e.Incarnation, e.Sequence)
		mu.Lock()
		defer mu.Unlock()
		posts = append(posts, raw)
		if held, ok := stored[key]; ok {
			cancel() // the resend is answered: nothing more to deliver
			if !bytes.Equal(held, raw) {
				return answer(409, `{"code":"conflict"}`), nil
			}
			return answer(204, ""), nil
		}
		stored[key] = raw
		return nil, clientTimeout{} // stored, but the answer is lost
	})}
	err := RunPublisher(ctx, q, PublisherConfig{Source: "door", Incarnation: "boot", URL: "https://ingest.example/internal/pa-test/observations",
		HealthURL: "https://ingest.example/internal/pa-test/health", Key: []byte("k"), Client: client,
		Wait: func(ctx context.Context, _ time.Duration) error { return ctx.Err() }})
	if err != nil && ctx.Err() == nil {
		t.Fatalf("publisher: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(posts) != 2 || !bytes.Equal(posts[0], posts[1]) {
		t.Fatalf("posts %d, identical %v: want the event sent twice, byte for byte", len(posts), len(posts) == 2 && bytes.Equal(posts[0], posts[1]))
	}
	if len(stored) != 1 {
		t.Fatalf("stored %d events, want 1", len(stored))
	}
	var e Event
	_ = json.Unmarshal(posts[0], &e)
	if e.Time.IsZero() {
		t.Fatal("the event was sent without a Time")
	}
	if h := q.Health(time.Now()); h.Acknowledged != 1 || h.Dropped != 0 || h.Pending != 0 {
		t.Fatalf("health %+v: want the event acknowledged once, nothing dropped", h)
	}
}

// The stop arriving while a stored event's answer is still out sends it again
// in the drain, taken afresh from the queue: still byte for byte, stored
// once, acknowledged once.
func TestDrainResendAfterAStopIsStoredOnce(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 64, MaxBytes: 1 << 20})
	q.TryEmit(Event{Kind: "door.response"})
	var mu sync.Mutex
	stored := map[string][]byte{}
	var posts [][]byte
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/health") {
			return answer(204, ""), nil
		}
		var e Event
		_ = json.Unmarshal(raw, &e)
		key := fmt.Sprintf("%s/%s/%d", e.Source, e.Incarnation, e.Sequence)
		mu.Lock()
		defer mu.Unlock()
		posts = append(posts, raw)
		if held, ok := stored[key]; ok {
			if !bytes.Equal(held, raw) {
				return answer(409, `{"code":"conflict"}`), nil
			}
			return answer(204, ""), nil
		}
		stored[key] = raw
		cancel() // the stop arrives while the answer is out
		return nil, context.Canceled
	})}
	_ = RunPublisher(ctx, q, PublisherConfig{Source: "door", Incarnation: "boot", URL: "https://ingest.example/internal/pa-test/observations",
		HealthURL: "https://ingest.example/internal/pa-test/health", Key: []byte("k"), Client: client, Drain: 5 * time.Second,
		Wait: func(ctx context.Context, _ time.Duration) error { return ctx.Err() }})
	mu.Lock()
	defer mu.Unlock()
	if len(posts) != 2 || !bytes.Equal(posts[0], posts[1]) || len(stored) != 1 {
		t.Fatalf("posts %d (identical %v), stored %d: want the event sent again in the drain, byte for byte, and stored once",
			len(posts), len(posts) == 2 && bytes.Equal(posts[0], posts[1]), len(stored))
	}
	if h := q.Health(time.Now()); h.Acknowledged != 1 || h.Dropped != 0 || h.Pending != 0 {
		t.Fatalf("health %+v: want the event acknowledged once, nothing dropped", h)
	}
}
