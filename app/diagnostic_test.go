package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SmartHealthNetwork/shn-gateway/diagnostics"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDiagnosticSourceOffAndBrokenConfigNeutral(t *testing.T) {
	for _, env := range []map[string]string{nil, {"DIAGNOSTIC_SINK_URL": "http://collector/evidence", "DIAGNOSTIC_SOURCE": "test-provider", "DIAGNOSTIC_KEY_FILE": "/absent"}} {
		d := newDiagnosticSource(func(k string) string { return env[k] }, "provider", io.Discard, time.Now)
		if d != nil {
			t.Fatal("disabled/broken configuration captured traffic")
		}
	}
}
func TestDiagnosticSourceBuffersFiniteExchangeBurst(t *testing.T) {
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("synthetic-observation-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"DIAGNOSTIC_SINK_URL": "http://collector.test/events", "DIAGNOSTIC_SOURCE": "test-provider", "DIAGNOSTIC_KEY_FILE": key}
	d := newDiagnosticSource(func(k string) string { return env[k] }, "provider", io.Discard, time.Now)
	if d == nil {
		t.Fatal("missing configured source")
	}
	for i := 0; i < 256; i++ {
		if !d.emit(diagnostics.Event{Kind: "http-request", Body: []byte("synthetic")}) {
			t.Fatalf("finite burst shed observation %d", i)
		}
	}
}
func TestDiagnosticSourcePublishesBoundedEvents(t *testing.T) {
	got := make(chan diagnostics.Event, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/health") {
			w.WriteHeader(204)
			return
		}
		var e diagnostics.Event
		json.NewDecoder(r.Body).Decode(&e)
		got <- e
		w.WriteHeader(204)
	}))
	defer srv.Close()
	key := filepath.Join(t.TempDir(), "key")
	os.WriteFile(key, []byte("synthetic-observation-secret"), 0600)
	env := map[string]string{"DIAGNOSTIC_SINK_URL": srv.URL + "/events", "DIAGNOSTIC_SOURCE": "configured-test-provider", "DIAGNOSTIC_KEY_FILE": key}
	d := newDiagnosticSource(func(k string) string { return env[k] }, "provider", io.Discard, time.Now)
	if d == nil {
		t.Fatal("missing configured source")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { d.run(ctx); close(done) }()
	d.emit(diagnostics.Event{Kind: "test", Body: []byte("exact bytes"), BodyComplete: true})
	select {
	case e := <-got:
		if e.Source != "configured-test-provider" || string(e.Body) != "exact bytes" {
			t.Fatalf("wrong published event: %+v", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("publication timed out")
	}
	cancel()
	<-done
}

// ROLE's resolved default is provider, including trace attribution.
func TestDiagnosticTraceUsesResolvedRole(t *testing.T) {
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("synthetic-trace-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"", "provider", "payer"} {
		t.Run("role="+role, func(t *testing.T) {
			env := map[string]string{"PROVIDER_DTR_POPULATE_URL": "https://populate.test/fhir/Questionnaire/$populate", "ROLE": role, "SHN_SECRETS": t.TempDir(), "SHN_DISCOVERY_URL": "http://discovery.test", "DIAGNOSTIC_SINK_URL": "http://collector.test/events", "DIAGNOSTIC_SOURCE": "test", "DIAGNOSTIC_KEY_FILE": key, "DIAGNOSTIC_TRACE_KEY_FILE": key}
			getenv := func(k string) string { return env[k] }
			cfg, err := loadConfig(getenv)
			if err != nil {
				t.Fatal(err)
			}
			d := newDiagnosticSource(getenv, cfg.Role, io.Discard, time.Now)
			if d == nil {
				t.Fatal("missing diagnostics")
			}
			if got, want := len(d.traceKey) > 0, cfg.Role == "provider"; got != want {
				t.Fatalf("trace configured=%v, resolved role=%s", got, cfg.Role)
			}
		})
	}
}

func TestGatewayBuildAndServeWithoutDiagnosticKey(t *testing.T) {
	b, _, err := buildProviderForPopulate(t, map[string]string{
		"PROVIDER_DTR_POPULATE_URL": "https://populate.test/fhir/Questionnaire/$populate",
		"DIAGNOSTIC_SINK_URL":       "https://collector.test/internal/observations",
		"DIAGNOSTIC_SOURCE":         "test-provider", "DIAGNOSTIC_KEY_FILE": filepath.Join(t.TempDir(), "missing-key"),
	})
	if err != nil {
		t.Fatalf("optional secret prevented gateway startup: %v", err)
	}
	srv := httptest.NewServer(b.handler)
	defer srv.Close()
	res, err := srv.Client().Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("gateway unavailable with missing diagnostic key: %d", res.StatusCode)
	}
}

// The publisher's batch endpoint is the sink path plus /batch, it drains for
// up to 10 s at a stop, and its queue holds 4,096 events.
func TestDiagnosticSourceBatchEndpointAndQueue(t *testing.T) {
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("synthetic-observation-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"DIAGNOSTIC_SINK_URL": "https://collector.test/internal/pa-test/observations", "DIAGNOSTIC_SOURCE": "configured-test-payer", "DIAGNOSTIC_KEY_FILE": key}
	d := newDiagnosticSource(func(k string) string { return env[k] }, "payer", io.Discard, time.Now)
	if d == nil {
		t.Fatal("missing configured source")
	}
	if d.publisher.BatchURL != "https://collector.test/internal/pa-test/observations/batch" || d.publisher.HealthURL != "https://collector.test/internal/pa-test/health" || d.publisher.Drain != 10*time.Second {
		t.Fatalf("batch %q health %q drain %v", d.publisher.BatchURL, d.publisher.HealthURL, d.publisher.Drain)
	}
	for i := 0; i < 4096; i++ {
		if !d.emit(diagnostics.Event{Kind: diagnostics.KindAccess}) {
			t.Fatalf("event %d refused below the queue's 4,096", i+1)
		}
	}
	if d.emit(diagnostics.Event{Kind: diagnostics.KindAccess}) {
		t.Fatal("the queue took more than 4,096 events")
	}
}

// The diagnostic publisher outlives its caller's context: an event emitted
// after that context ends (an exchange finishing during shutdown, or the
// gateway's own close) is still delivered, and stopping the publisher waits
// for it.
func TestDiagnosticPublisherOutlivesItsContext(t *testing.T) {
	got := make(chan diagnostics.Event, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/health") {
			w.WriteHeader(204)
			return
		}
		var e diagnostics.Event
		_ = json.NewDecoder(r.Body).Decode(&e)
		got <- e
		w.WriteHeader(204)
	}))
	defer srv.Close()
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("synthetic-observation-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"DIAGNOSTIC_SINK_URL": srv.URL + "/events", "DIAGNOSTIC_SOURCE": "configured-test-payer", "DIAGNOSTIC_KEY_FILE": key}
	d := newDiagnosticSource(func(k string) string { return env[k] }, "payer", io.Discard, time.Now)
	if d == nil {
		t.Fatal("missing configured source")
	}
	ctx, cancel := context.WithCancel(context.Background())
	stop := built{diagnostic: d}.startDiagnostic(ctx)
	cancel() // the listener's shutdown begins
	// An exchange finishes later; a drain begun at the cancel, with an empty
	// queue, would long have ended. The publisher must still be running.
	time.Sleep(200 * time.Millisecond)
	d.emit(diagnostics.Event{Kind: "late", Time: time.Now()})
	select {
	case e := <-got:
		if e.Kind != "late" {
			t.Fatalf("published %+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an event emitted after the caller's context ended was not delivered")
	}
	stop()
}

// A request still running when the stop's grace runs out is cut, and its
// handler's last event is still delivered: the stop waits for the handler to
// return before the publisher drains.
func TestStopServingDeliversTheEventsOfCutRequests(t *testing.T) {
	pubStopped := make(chan struct{})
	var mu sync.Mutex
	var kinds []string
	// Like the network's ingest, an event is stored once by its source,
	// incarnation and sequence: delivery is at least once, and a resend of
	// the same bytes is answered as the first was (diagnostics' resend rows).
	stored := map[string][]byte{}
	held := true
	ingest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/health") {
			w.WriteHeader(204)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var e diagnostics.Event
		_ = json.Unmarshal(raw, &e)
		mu.Lock()
		first := held
		held = false
		mu.Unlock()
		if first {
			// Held until the stop, then refused: the event is sent again,
			// by the publisher's retry or its drain.
			<-pubStopped
			w.WriteHeader(503)
			return
		}
		key := fmt.Sprintf("%s/%s/%d", e.Source, e.Incarnation, e.Sequence)
		mu.Lock()
		defer mu.Unlock()
		if prev, ok := stored[key]; ok {
			if !bytes.Equal(prev, raw) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(409)
				_, _ = w.Write([]byte(`{"code":"conflict"}`))
				return
			}
			w.WriteHeader(204)
			return
		}
		stored[key] = raw
		kinds = append(kinds, e.Kind)
		w.WriteHeader(204)
	}))
	defer ingest.Close()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(pubStopped) }) }
	defer release() // runs before ingest.Close: a failure must not leave the held request hanging
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("synthetic-observation-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"DIAGNOSTIC_SINK_URL": ingest.URL + "/internal/pa-test/observations", "DIAGNOSTIC_SOURCE": "configured-test-payer", "DIAGNOSTIC_KEY_FILE": key}
	d := newDiagnosticSource(func(k string) string { return env[k] }, "payer", io.Discard, time.Now)
	if d == nil {
		t.Fatal("missing configured source")
	}
	stop := built{diagnostic: d}.startDiagnostic(context.Background())

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	var handlers sync.WaitGroup
	srv := &http.Server{Handler: countHandlers(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done() // runs until the stop cuts it
		time.Sleep(20 * time.Millisecond)
		d.emit(diagnostics.Event{Kind: "late", Time: time.Now()})
	}), &handlers)}
	go func() { _ = srv.Serve(ln) }()
	go func() {
		if resp, err := http.Get("http://" + ln.Addr().String() + "/"); err == nil {
			resp.Body.Close()
		}
	}()
	<-entered
	if err := stopServing(srv, &handlers, 100*time.Millisecond, 2*time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stopServing: %v, want the grace's deadline", err)
	}
	release()
	stop()
	mu.Lock()
	defer mu.Unlock()
	if len(kinds) != 1 || kinds[0] != "late" {
		t.Fatalf("the ingest got %v, want the cut request's event", kinds)
	}
}
