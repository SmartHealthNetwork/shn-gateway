package fhirseed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/internal/testrecord"
)

// The warm-up's answers are what a real multitenant HAPI data server answered
// it (testdata/recordings/README.md), replayed strictly: a request that differs
// from the recorded one in method, path, Content-Type or body gets no answer and
// fails the test, and every recorded exchange must be asked for.
func replayWarm(t *testing.T, name string) *testrecord.Recording {
	t.Helper()
	return testrecord.Load(t, filepath.Join("testdata", "recordings", name))
}

// slowFirstAnswer serves rec, holding the first answer back for delay. It is a
// test-side hook, not part of the recording: a recording carries bytes, not
// timing. The capture behind warm-default.json measured the real cold first
// $validate at 1.84 s and the warm second one at 29 ms; the delay here is
// shorter, and still far past the consumer timeout the test sets.
func slowFirstAnswer(t *testing.T, rec *testrecord.Recording, delay time.Duration) *httptest.Server {
	t.Helper()
	replay := rec.Server().Config.Handler
	var asked atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !asked.Swap(true) {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		replay.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Each warm-up posts exactly one $validate to the tenant's Patient/$validate:
// the recording holds the capture's two warm-ups (cold, then warm), so a
// warm-up that asked twice would use up the second's answer and the second
// would get none.
func TestWarmValidate_PostsOneValidateAndReportsElapsed(t *testing.T) {
	srv := replayWarm(t, "warm-default.json").Server()
	var logged []string
	c := &Client{Base: srv.URL + "/fhir", Logf: func(f string, a ...any) { logged = append(logged, f) }}
	for i := range 2 {
		elapsed, err := c.WarmValidate(context.Background(), "DEFAULT")
		if err != nil {
			t.Fatalf("warm-up %d: %v", i+1, err)
		}
		if elapsed <= 0 {
			t.Fatalf("warm-up %d: elapsed = %s", i+1, elapsed)
		}
	}
	if len(logged) != 2 || !strings.Contains(logged[0], "validator warm in") || !strings.Contains(logged[1], "validator warm in") {
		t.Fatalf("logged = %v", logged)
	}
}

// The warm-up warms the server whatever the tenant, and says nothing about the
// tenant. A type-level $validate does not resolve the partition, so the real
// server answered a tenant it has no partition for with the ordinary
// validation outcome (200, one dom-6 warning), and the warm-up succeeds. Its
// contract is only to pay the server-wide validation initialisation; the
// seeder's next request writes to the tenant and fails there.
func TestWarmValidate_WarmsTheServerAndSaysNothingAboutTheTenant(t *testing.T) {
	srv := replayWarm(t, "warm-unknown-tenant.json").Server()
	c := &Client{Base: srv.URL + "/fhir"}
	if _, err := c.WarmValidate(context.Background(), "nosuchtenant"); err != nil {
		t.Fatalf("the server answered the warm-up for a tenant it does not have; the warm-up must pass: %v", err)
	}
}

// The warm-up's deadline is its own, not the consumer client's: a Client whose HTTP
// timeout could never carry a cold $validate still warms, and a server slower than
// the deadline is refused with the deadline named.
func TestWarmValidate_UsesItsOwnDeadlineNotTheConsumerClient(t *testing.T) {
	// The recorded cold answer, held back past c.HTTP's timeout, then the
	// recorded warm answer.
	srv := slowFirstAnswer(t, replayWarm(t, "warm-default.json"), 50*time.Millisecond)
	c := &Client{Base: srv.URL + "/fhir", HTTP: &http.Client{Timeout: time.Nanosecond}}
	if _, err := c.warmValidate(context.Background(), "DEFAULT", 5*time.Second); err != nil {
		t.Fatalf("a slow cold warm-up failed although only c.HTTP's timeout is tiny: %v", err)
	}
	if _, err := c.warmValidate(context.Background(), "DEFAULT", 5*time.Second); err != nil {
		t.Fatalf("the warm second warm-up failed: %v", err)
	}

	// Hand-written fault: a server that answers only after the deadline. A
	// recording holds answers, not hangs. The handler waits on a channel the test
	// closes after the assertion (the server's request context is not reliably
	// cancelled by a client that gave up, so the handler cannot wait on it).
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

// Hand-written fault: no capture holds a 5xx from the data server's $validate
// (the server does not produce one on demand). This row is a plain-text 500,
// as a proxy in front of the server would answer, and only that. The server's
// own error answers are OperationOutcomes, and a 5xx whose body is an
// OperationOutcome is read as an answer today, so it would count as warm;
// whether it should is open, and no row here pins it either way.
func TestWarmValidate_PlainTextServerErrorIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL + "/fhir"}
	if _, err := c.WarmValidate(context.Background(), "DEFAULT"); err == nil {
		t.Fatal("a plain-text 500 (a proxy in front of the server) from $validate must not count as warm")
	}
}

func TestWarmDeadlineIsFarAboveTheConsumerBudget(t *testing.T) {
	// The consumers give a $validate 30 s (sdk/transport.go); the warm-up must be
	// able to absorb a cold first call that is several times that.
	if WarmDeadline < 5*30*time.Second {
		t.Fatalf("WarmDeadline = %s, want at least five consumer budgets", WarmDeadline)
	}
}
