package fhirseed

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const okOutcome = `{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"informational","diagnostics":"No issues detected during validation"}]}`

func TestWarmValidate_PostsOneValidateAndReportsElapsed(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/fhir+json")
		_, _ = w.Write([]byte(okOutcome))
	}))
	defer srv.Close()
	var logged []string
	c := &Client{Base: srv.URL + "/fhir", Logf: func(f string, a ...any) { logged = append(logged, f) }}
	elapsed, err := c.WarmValidate(context.Background(), "DEFAULT")
	if err != nil {
		t.Fatal(err)
	}
	if elapsed <= 0 {
		t.Fatalf("elapsed = %s", elapsed)
	}
	if len(paths) != 1 || paths[0] != "POST /fhir/DEFAULT/Patient/$validate" {
		t.Fatalf("requests = %v, want exactly one POST to the tenant's Patient/$validate", paths)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "validator warm in") {
		t.Fatalf("logged = %v", logged)
	}
}

// The warm-up's deadline is its own, not the consumer client's: a Client whose HTTP
// timeout could never carry a cold $validate still warms, and a server slower than
// the deadline is refused with the deadline named.
func TestWarmValidate_UsesItsOwnDeadlineNotTheConsumerClient(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/fhir+json")
		_, _ = w.Write([]byte(okOutcome))
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL + "/fhir", HTTP: &http.Client{Timeout: time.Nanosecond}}

	go func() { time.Sleep(50 * time.Millisecond); close(release) }()
	if _, err := c.warmValidate(context.Background(), "DEFAULT", 5*time.Second); err != nil {
		t.Fatalf("warm with a generous deadline failed although only c.HTTP's timeout is tiny: %v", err)
	}

	// A server that answers only after the deadline: the handler waits on a channel
	// the test closes after the assertion (the server's request context is not
	// reliably cancelled by a client that gave up, so the handler cannot wait on it).
	hold := make(chan struct{})
	stuck := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hold
	}))
	c2 := &Client{Base: stuck.URL + "/fhir"}
	_, err := c2.warmValidate(context.Background(), "DEFAULT", 30*time.Millisecond)
	close(hold)
	stuck.Close()
	if err == nil || !strings.Contains(err.Error(), "did not answer within 30ms") {
		t.Fatalf("a server slower than the deadline must be refused naming the deadline; got %v", err)
	}
}

func TestWarmValidate_ServerErrorIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL + "/fhir"}
	if _, err := c.WarmValidate(context.Background(), "DEFAULT"); err == nil {
		t.Fatal("a 500 from $validate must not count as warm")
	}
}

func TestWarmDeadlineIsFarAboveTheConsumerBudget(t *testing.T) {
	// The consumers give a $validate 30 s (sdk/transport.go); the warm-up must be
	// able to absorb a cold first call that is several times that.
	if WarmDeadline < 5*30*time.Second {
		t.Fatalf("WarmDeadline = %s, want at least five consumer budgets", WarmDeadline)
	}
}

// Warming requires an interpretable execution, not necessarily valid content.
// Source preparation independently refuses invalid content before writing it.
func TestWarmValidate_InterpretableExecution(t *testing.T) {
	captured, err := os.ReadFile("testdata/hapi-dom6-warning.json")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%x", sha256.Sum256(captured)) != "f1d5187bafe37f28125f7b3248cf84bfadfb6d14608de9da57cbe5f12d98385e" {
		t.Fatal("captured response changed")
	}
	for _, tc := range []struct {
		name, body string
		status     int
		wantErr    bool
	}{
		{"captured advisory", string(captured), 200, false},
		{"invalid content", `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"structure"}]}`, 200, false},
		{"unsupported", `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"not-supported"}]}`, 200, true},
		{"uninterpretable", `{"resourceType":"OperationOutcome","issue":[{"severity":"warning","code":"unknown"}]}`, 200, true},
		{"malformed", `{`, 200, true},
		{"server failure", string(captured), 500, true},
		{"warning non-content status", string(captured), 422, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != warmBody || r.Method != "POST" || r.URL.RequestURI() != "/fhir/DEFAULT/Patient/$validate" {
					t.Errorf("warm request changed: %s %s %s %v", r.Method, r.URL.RequestURI(), body, err)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			c := &Client{Base: srv.URL + "/fhir"}
			_, err := c.WarmValidate(context.Background(), "DEFAULT")
			if (err != nil) != tc.wantErr || calls != 1 {
				t.Fatalf("warm err=%v calls=%d", err, calls)
			}
		})
	}
}

func TestWarmValidate_TransportFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()
	c := &Client{Base: srv.URL + "/fhir"}
	if _, err := c.WarmValidate(context.Background(), "DEFAULT"); err == nil {
		t.Fatal("transport failure counted as warm")
	}
}
