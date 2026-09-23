package diagnostics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type testBodyBudget struct {
	mu       sync.Mutex
	n, limit int
}

func (b *testBodyBudget) TryReserve(n int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > b.limit-b.n {
		return false
	}
	b.n += n
	return true
}
func (b *testBodyBudget) Release(n int) { b.mu.Lock(); b.n -= n; b.mu.Unlock() }
func TestSharedCaptureAndQueueBudget(t *testing.T) {
	b := &testBodyBudget{limit: 8}
	q := NewQueue(Limits{})
	calls := 0
	h := ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("12345678")) }), func(e Event) bool { calls++; return q.TryEmit(e) }, nil, time.Now, 8, NewCaptureBudget(100, 2))
	r := httptest.NewRequest("POST", "/", strings.NewReader(""))
	r = r.WithContext(WithBodyBudget(context.Background(), b))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != "12345678" || calls != 2 {
		t.Fatal("capture altered exchange")
	}
	// The response copy overlaps the entire captured body and must be shed.
	if q.Health(time.Now()).Dropped != 1 {
		t.Fatalf("overlap not accounted: %+v", q.Health(time.Now()))
	}
	if b.n != 0 {
		t.Fatalf("capture reservation leaked %d", b.n)
	}
	e := WithEventBudget(Event{Body: []byte("12345678")}, b)
	if !q.TryEmit(e) {
		t.Fatal("released budget unavailable")
	}
	if q.TryEmit(e) {
		t.Fatal("queue copied without reservation")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for q.Health(time.Now()).Pending > 0 {
		v, err := q.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		q.Acknowledge(v.Sequence)
	}
	if b.n != 0 {
		t.Fatal("queue budget leaked")
	}
}
