package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type observerHeldBody struct {
	entered, release chan struct{}
	once             sync.Once
}

func (b *observerHeldBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return 0, io.EOF
}
func (b *observerHeldBody) Close() error { return nil }

func TestObserverBarrierAppBinding(t *testing.T) {
	b, _, err := buildProviderForPopulate(t, map[string]string{
		"PROVIDER_DTR_POPULATE_URL": "https://populate.test/fhir/Questionnaire/$populate",
		"OBSERVER_ADDR":             "127.0.0.1:9411",
	})
	if err != nil {
		t.Fatal(err)
	}
	if b.observerHandler == nil || b.observerAddr != "127.0.0.1:9411" {
		t.Fatal("observer listener not constructed")
	}
	body := &observerHeldBody{entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	defer once.Do(func() { close(body.release) })
	clinical := httptest.NewRecorder()
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		b.handler.ServeHTTP(clinical, httptest.NewRequest(http.MethodPost, "/scenario/dispatch", body))
	}()
	<-body.entered
	// A held clinical request coexists with immediate health and a diagnostic
	// timeout. Below, clinical bytes and a successful follow-up barrier are
	// unchanged; channel-driven engine tests cover source cutoff and callbacks.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	barrier := httptest.NewRecorder()
	barrierDone := make(chan struct{})
	go func() {
		defer close(barrierDone)
		b.observerHandler.ServeHTTP(barrier, httptest.NewRequest(http.MethodPost, "/barrier", nil).WithContext(ctx))
	}()
	health := httptest.NewRecorder()
	b.handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("ordinary health blocked/refused: %d %s", health.Code, health.Body.String())
	}
	observerHealth := httptest.NewRecorder()
	b.observerHandler.ServeHTTP(observerHealth, httptest.NewRequest(http.MethodGet, "/health", nil))
	var metadata struct {
		Protocol    int    `json:"protocol"`
		Incarnation string `json:"incarnation"`
	}
	if err := json.Unmarshal(observerHealth.Body.Bytes(), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Protocol != 1 || metadata.Incarnation == "" {
		t.Fatalf("capability missing: %s", observerHealth.Body.String())
	}
	<-barrierDone
	if barrier.Code != http.StatusGatewayTimeout || strings.Contains(barrier.Body.String(), `"events"`) {
		t.Fatalf("busy source barrier: %d %s", barrier.Code, barrier.Body.String())
	}
	once.Do(func() { close(body.release) })
	<-requestDone
	if clinical.Code != http.StatusBadRequest || clinical.Body.String() != `{"error":"member is required"}`+"\n" {
		t.Fatalf("ordinary response changed: %d %s", clinical.Code, clinical.Body.String())
	}
	barrier = httptest.NewRecorder()
	b.observerHandler.ServeHTTP(barrier, httptest.NewRequest(http.MethodPost, "/barrier", nil))
	if barrier.Code != http.StatusOK {
		t.Fatalf("completed source barrier: %d %s", barrier.Code, barrier.Body.String())
	}
	public := httptest.NewRecorder()
	b.handler.ServeHTTP(public, httptest.NewRequest(http.MethodPost, "/barrier", nil))
	if public.Code != http.StatusNotFound {
		t.Fatal("barrier exposed on clinical listener")
	}
}
func TestObserverBarrierOffByDefault(t *testing.T) {
	b, _, err := buildProviderForPopulate(t, map[string]string{"PROVIDER_DTR_POPULATE_URL": "https://populate.test/fhir/Questionnaire/$populate"})
	if err != nil {
		t.Fatal(err)
	}
	if b.observerHandler != nil || b.observerAddr != "" {
		t.Fatal("observer enabled by default")
	}
	response := httptest.NewRecorder()
	b.handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/barrier", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("default barrier: %d", response.Code)
	}
}
