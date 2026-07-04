// observer_test.go — hermetic tests for the SSE hub (see STABILITY.md).
package observer

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine"
)

// readSSE reads SSE frames off resp until n data lines or timeout, returning
// the data payloads in order.
func readSSE(t *testing.T, resp *http.Response, n int) []string {
	t.Helper()
	var out []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
		for sc.Scan() && len(out) < n {
			line := sc.Text()
			if strings.HasPrefix(line, "data: ") {
				out = append(out, strings.TrimPrefix(line, "data: "))
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		resp.Body.Close()
		<-done
	}
	if len(out) != n {
		t.Fatalf("read %d SSE events, want %d: %v", len(out), n, out)
	}
	return out
}

// TestHub_BufferedReplayThenLive: events emitted before a subscriber connects
// replay from the ring buffer in order; the health endpoint counts them.
func TestHub_BufferedReplayThenLive(t *testing.T) {
	hub := NewHub()
	srv := httptest.NewServer(hub.Handler())
	defer srv.Close()

	hub.Emit(engine.ObserverEvent{Kind: "leg.originated", LegType: "crd-order-select"})
	hub.Emit(engine.ObserverEvent{Kind: "leg.response", LegType: "crd-order-select"})

	resp, err := http.Get(srv.URL + "/events")
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	got := readSSE(t, resp, 2)
	if !strings.Contains(got[0], `"leg.originated"`) || !strings.Contains(got[1], `"leg.response"`) {
		t.Fatalf("replayed events wrong/misordered: %v", got)
	}
	if !strings.Contains(got[0], `"seq":1`) || !strings.Contains(got[1], `"seq":2`) {
		t.Fatalf("events must carry monotonic seq: %v", got)
	}

	hr, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer hr.Body.Close()
	if hr.StatusCode != http.StatusOK {
		t.Fatalf("/health = %d, want 200", hr.StatusCode)
	}
}

// TestHub_LastEventIDSkipsReplayed: a reconnecting subscriber presenting
// Last-Event-ID only receives events after that seq.
func TestHub_LastEventIDSkipsReplayed(t *testing.T) {
	hub := NewHub()
	srv := httptest.NewServer(hub.Handler())
	defer srv.Close()

	hub.Emit(engine.ObserverEvent{Kind: "leg.originated"})
	hub.Emit(engine.ObserverEvent{Kind: "leg.response"})
	hub.Emit(engine.ObserverEvent{Kind: "validate.result"})

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/events", nil)
	req.Header.Set("Last-Event-ID", "2")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer resp.Body.Close()
	got := readSSE(t, resp, 1)
	if !strings.Contains(got[0], `"validate.result"`) || !strings.Contains(got[0], `"seq":3`) {
		t.Fatalf("want only seq 3 (validate.result), got %v", got)
	}
}

// TestHub_NonJSONPayloadStillDelivered: a garbage ingress body must not cost
// the inspector the event (plan-review finding 1) — the payload re-encodes as
// a JSON string, the Detail says so, and the event still flows.
func TestHub_NonJSONPayloadStillDelivered(t *testing.T) {
	hub := NewHub()
	srv := httptest.NewServer(hub.Handler())
	defer srv.Close()

	hub.Emit(engine.ObserverEvent{Kind: "ingress.received", Payload: []byte("{definitely not json")})

	resp, err := http.Get(srv.URL + "/events")
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer resp.Body.Close()
	got := readSSE(t, resp, 1)
	if !strings.Contains(got[0], `"ingress.received"`) {
		t.Fatalf("non-JSON-payload event was dropped: %v", got)
	}
	if !strings.Contains(got[0], "payload was not valid JSON") {
		t.Fatalf("re-encoded payload must be flagged in Detail: %v", got)
	}
}
