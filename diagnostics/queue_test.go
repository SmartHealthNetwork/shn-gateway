package diagnostics

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestHealthReportsPendingAcrossPublication(t *testing.T) {
	q := NewQueue(Limits{})
	q.TryEmit(Event{Kind: "first"})
	q.TryEmit(Event{Kind: "second"})
	check := func(want float64) {
		t.Helper()
		raw, err := json.Marshal(q.Health(time.Unix(1, 0)))
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		if body["pending"] != want {
			t.Fatalf("pending=%v want=%v", body["pending"], want)
		}
	}
	check(2)
	e, _ := q.Next(context.Background())
	check(2)
	q.Acknowledge(e.Sequence)
	check(1)
	e, _ = q.Next(context.Background())
	q.discard(e.Sequence)
	check(0)
}

func TestQueueShedsWithoutRetainingCallerSlice(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 1, MaxBytes: 4096, MaxBodyBytes: 1024})
	b := []byte("not-json secret=synthetic")
	if !q.TryEmit(Event{Body: b}) {
		t.Fatal("first observation rejected")
	}
	b[0] = 'X'
	if q.TryEmit(Event{Body: []byte("next")}) {
		t.Fatal("full queue admitted another observation")
	}
	e, err := q.Next(context.Background())
	if err != nil || string(e.Body) != "not-json secret=synthetic" {
		t.Fatalf("%q %v", e.Body, err)
	}
	if q.TryEmit(Event{Body: []byte("still full")}) {
		t.Fatal("Next released reservation")
	}
	q.Acknowledge(e.Sequence)
	if !q.TryEmit(Event{Body: []byte("next")}) {
		t.Fatal("ack did not release reservation")
	}
}

func TestQueueByteLimitCountsHeadersAndTruncatesBody(t *testing.T) {
	q := NewQueue(Limits{MaxEvents: 2, MaxBytes: 163, MaxBodyBytes: 3})
	if !q.TryEmit(Event{Headers: http.Header{"X": {"yy"}}, Body: []byte("abcdef")}) {
		t.Fatal("event rejected")
	}
	e, _ := q.Next(context.Background())
	if string(e.Body) != "abc" || e.BodyComplete {
		t.Fatalf("body=%q complete=%v", e.Body, e.BodyComplete)
	}
	if q.TryEmit(Event{Body: []byte("12345678")}) {
		t.Fatal("header reservation was not counted")
	}
	q.Drop(e.Sequence)
	h := q.Health(time.Unix(1, 0))
	if h.LastSequence != 2 || h.Dropped != 2 || h.LastAcknowledged != 0 {
		t.Fatalf("health=%+v", h)
	}
}

func TestQueueCanceledNext(t *testing.T) {
	q := NewQueue(Limits{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := q.Next(ctx); err != context.Canceled {
		t.Fatalf("err=%v", err)
	}
}

func TestQueueCopiesHeaders(t *testing.T) {
	h := http.Header{"Authorization": {"Synthetic abc"}}
	q := NewQueue(Limits{})
	if !q.TryEmit(Event{Headers: h}) {
		t.Fatal("rejected")
	}
	h.Set("Authorization", "changed")
	e, _ := q.Next(context.Background())
	if e.Headers.Get("Authorization") != "Synthetic abc" {
		t.Fatalf("header=%q", e.Headers.Get("Authorization"))
	}
}
