package app

import (
	"context"
	"encoding/json"
	"github.com/SmartHealthNetwork/shn-gateway/diagnostics"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
			env := map[string]string{"ORIGINATION_PROFILE": "relay-only", "ROLE": role, "SHN_SECRETS": t.TempDir(), "SHN_DISCOVERY_URL": "http://discovery.test", "DIAGNOSTIC_SINK_URL": "http://collector.test/events", "DIAGNOSTIC_SOURCE": "test", "DIAGNOSTIC_KEY_FILE": key, "DIAGNOSTIC_TRACE_KEY_FILE": key}
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
