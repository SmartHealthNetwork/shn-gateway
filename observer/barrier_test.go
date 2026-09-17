package observer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine"
)

func TestBarrierWaitsWhileHealthIsImmediate(t *testing.T) {
	hub := newHub("synthetic-incarnation")
	entered, release := make(chan struct{}), make(chan struct{})
	handler := hub.HandlerWithBarrier(func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second {
			t.Error("missing five-second maximum")
		}
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	finished := make(chan *http.Response, 1)
	go func() {
		response, err := http.Post(server.URL+"/barrier", "application/json", nil)
		if err != nil {
			t.Error(err)
		}
		finished <- response
	}()
	<-entered
	health, err := http.Get(server.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	assertObserverMetadata(t, health, 0)
	select {
	case <-finished:
		t.Fatal("barrier returned before completion")
	default:
	}
	hub.Emit(engine.ObserverEvent{Kind: "leg.certified"})
	close(release)
	response := <-finished
	if response == nil {
		t.Fatal("missing barrier response")
	}
	assertObserverMetadata(t, response, 1)
}
func assertObserverMetadata(t *testing.T, response *http.Response, events uint64) {
	t.Helper()
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "application/json" || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("response %d %v", response.StatusCode, response.Header)
	}
	var got struct {
		Protocol    int    `json:"protocol"`
		Incarnation string `json:"incarnation"`
		Events      uint64 `json:"events"`
	}
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Protocol != 1 || got.Incarnation != "synthetic-incarnation" || got.Events != events {
		t.Fatalf("metadata %+v", got)
	}
}
func TestBarrierRejections(t *testing.T) {
	for _, row := range []struct {
		name, method string
		wait         func(context.Context) error
		status       int
	}{
		{"wrong method", http.MethodGet, func(context.Context) error { return nil }, http.StatusMethodNotAllowed},
		{"absent waiter", http.MethodPost, nil, http.StatusNotFound},
		{"canceled", http.MethodPost, func(context.Context) error { return context.Canceled }, http.StatusServiceUnavailable},
		{"deadline", http.MethodPost, func(context.Context) error { return context.DeadlineExceeded }, http.StatusGatewayTimeout},
		{"source failure", http.MethodPost, func(context.Context) error { return errors.New("source closed") }, http.StatusServiceUnavailable},
		{"canceled but nil waiter result", http.MethodPost, func(ctx context.Context) error { <-ctx.Done(); return nil }, http.StatusGatewayTimeout},
	} {
		t.Run(row.name, func(t *testing.T) {
			handler := newHub("synthetic-incarnation").HandlerWithBarrier(row.wait)
			request := httptest.NewRequest(row.method, "/barrier", nil)
			if row.name == "canceled but nil waiter result" {
				ctx, cancel := context.WithDeadline(request.Context(), time.Now().Add(-time.Second))
				defer cancel()
				request = request.WithContext(ctx)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != row.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), `"events"`) {
				t.Fatal("failure advertised successful count")
			}
		})
	}
}
func TestBarrierClientCancellation(t *testing.T) {
	entered, stopped := make(chan struct{}), make(chan error, 1)
	server := httptest.NewServer(newHub("synthetic-incarnation").HandlerWithBarrier(func(ctx context.Context) error {
		close(entered)
		<-ctx.Done()
		stopped <- ctx.Err()
		return ctx.Err()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/barrier", nil)
	finished := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(request)
		if response != nil {
			response.Body.Close()
		}
		finished <- err
	}()
	<-entered
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("client: %v", err)
	}
	select {
	case err := <-stopped:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server waiter survived disconnect")
	}
}
func TestBareHubDoesNotAdvertiseBarrier(t *testing.T) {
	for _, handler := range []http.Handler{newHub("synthetic-incarnation").Handler(), newHub("synthetic-incarnation").HandlerWithBarrier(nil)} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
		if response.Code != 200 || response.Body.String() != `{"events":0}` {
			t.Fatalf("legacy health changed: %d %s", response.Code, response.Body.String())
		}
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/barrier", nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("unsupported barrier: %d", response.Code)
		}
	}
}
func TestHubIncarnationPreservesSSEBytes(t *testing.T) {
	hub := newHub("synthetic-incarnation")
	hub.Emit(engine.ObserverEvent{Kind: "leg.certified", Detail: `{"literal":"<foreign>&bytes"}`})
	server := httptest.NewServer(hub.Handler())
	defer server.Close()
	response, err := http.Get(server.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.Header.Get("X-SHN-Observer-Incarnation") != "synthetic-incarnation" {
		t.Fatalf("incarnation: %v", response.Header)
	}
	want := "id: 1\ndata: " + `{"seq":1,"time":"0001-01-01T00:00:00Z","kind":"leg.certified","detail":"{\"literal\":\"\u003cforeign\u003e\u0026bytes\"}"}` + "\n\n"
	raw := make([]byte, len(want))
	if _, err := io.ReadFull(response.Body, raw); err != nil {
		t.Fatal(err)
	}
	if string(raw) != want {
		t.Fatalf("SSE bytes changed: %q", raw)
	}
}
func TestHubProductionIncarnationsAreNonemptyAndDistinct(t *testing.T) {
	first, second := NewHub(), NewHub()
	if first.incarnation == "" || second.incarnation == "" || first.incarnation == second.incarnation {
		t.Fatal("hub identity reused or empty")
	}
}
