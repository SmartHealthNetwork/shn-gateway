package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func (b *testBodyBudget) held() int { b.mu.Lock(); defer b.mu.Unlock(); return b.n }

func TestPublisherSharedSerializationPressure(t *testing.T) {
	b := &testBodyBudget{limit: 32 << 20, n: 24 << 20}
	q := NewQueue(Limits{})
	if !q.TryEmit(WithEventBudget(Event{Kind: "test", Body: make([]byte, 8<<20)}, b)) {
		t.Fatal("queue admission")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("must shed before transport")
	})}
	_ = RunPublisher(ctx, q, PublisherConfig{URL: "https://sink.test", Client: client, Heartbeat: time.Hour})
	if calls != 0 {
		t.Fatalf("serialized without shared reservation: transport calls=%d", calls)
	}
	if h := q.Health(time.Now()); h.Dropped != 1 || h.Pending != 0 || h.LastAcknowledged != 0 {
		t.Fatalf("loss=%+v", h)
	}
	if b.held() != 24<<20 {
		t.Fatalf("held=%d", b.held())
	}
}

func TestPublisherCancellationRetainsActiveAndDropsQueued(t *testing.T) {
	b := &testBodyBudget{limit: 32 << 20}
	q := NewQueue(Limits{})
	for _, body := range []string{"active raw bytes", "pending raw bytes"} {
		if !q.TryEmit(WithEventBudget(Event{Kind: "test", Body: []byte(body)}, b)) {
			t.Fatal("admission")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release, done := make(chan int, 1), make(chan struct{}), make(chan error, 1)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		entered <- int(r.ContentLength)
		<-release
		return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	go func() {
		done <- RunPublisher(ctx, q, PublisherConfig{URL: "https://sink.test", Client: client, Heartbeat: time.Hour})
	}()
	encoded := <-entered
	if got := b.held(); got < len("active raw bytes")+len("pending raw bytes")+encoded {
		t.Errorf("active encoded bytes uncharged: held=%d encoded=%d", got, encoded)
	}
	cancel()
	deadline := time.Now().Add(time.Second)
	for q.Health(time.Now()).Dropped == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if h := q.Health(time.Now()); h.Pending != 1 || h.Dropped != 1 || h.LastAcknowledged != 0 {
		t.Errorf("cancellation did not drain pending: %+v", h)
	}
	if got := b.held(); got < len("active raw bytes")+encoded {
		t.Errorf("released active owner early: %d", got)
	}
	close(release)
	<-done
	if b.held() != 0 {
		t.Errorf("body charges retained after consumer return: %d", b.held())
	}
	if h := q.Health(time.Now()); h.Pending != 0 || h.Dropped != 2 || h.LastAcknowledged != 0 {
		t.Errorf("final loss=%+v", h)
	}
	if q.TryEmit(Event{Kind: "late"}) {
		t.Error("shutdown admitted another event")
	}
}

func TestPublisherSerializedBytesMatchLegacyAndRelease(t *testing.T) {
	for _, body := range [][]byte{nil, []byte("raw\x00\n<&>"), {0xff, 0, 1, 2, 3}} {
		b := &testBodyBudget{limit: 32 << 20}
		q := NewQueue(Limits{})
		e := Event{Kind: "test", Body: body, Detail: "metadata <escaped>", Headers: http.Header{"X-Test": []string{"value"}}}
		if !q.TryEmit(WithEventBudget(e, b)) {
			t.Fatal("admission")
		}
		ctx, cancel := context.WithCancel(context.Background())
		clock := func() time.Time { return time.Unix(1, 2) }
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			wantEvent := e
			wantEvent.Sequence = 1
			wantEvent.Source = "src"
			wantEvent.Incarnation = "boot"
			wantEvent.Time = clock()
			want, _ := json.Marshal(wantEvent)
			if !bytes.Equal(raw, want) {
				t.Errorf("raw=%s want=%s", raw, want)
			}
			if b.held() < len(body)+len(raw) {
				t.Errorf("serialization not charged: %d", b.held())
			}
			cancel()
			return &http.Response{StatusCode: 204, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
		})}
		_ = RunPublisher(ctx, q, PublisherConfig{URL: "https://sink.test", Source: "src", Incarnation: "boot", Clock: clock, Client: client})
		q.Close()
		q.Close()
		if b.held() != 0 || q.Health(clock()).LastAcknowledged != 1 {
			t.Fatalf("ownership=%d health=%+v", b.held(), q.Health(clock()))
		}
	}
}
