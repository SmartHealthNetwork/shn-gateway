package checks

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a manually-advanced clock so CheckedAt/latency/cooldown
// assertions are deterministic (context deadlines still use real time —
// see TestGlobalDeadline).
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// 1. fhir-metadata OK.
func TestFHIRMetadataOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metadata" {
			t.Errorf("path = %q, want /metadata", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"resourceType":"CapabilityStatement","fhirVersion":"4.0.1"}`))
	}))
	defer srv.Close()

	clock := newFakeClock(time.Unix(0, 0))
	rn := NewRunner([]Target{{ID: "fhir", Kind: KindFHIRMetadata, URL: srv.URL}}, srv.Client(), clock.Now)
	results, err := rn.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	got := results[0]
	if !got.OK {
		t.Errorf("OK = false, want true; detail=%q", got.Detail)
	}
	if want := "CapabilityStatement (FHIR 4.0.1)"; got.Detail != want {
		t.Errorf("Detail = %q, want %q", got.Detail, want)
	}
}

// 2. fhir-metadata wrong shape.
func TestFHIRMetadataWrongShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"resourceType":"OperationOutcome"}`))
	}))
	defer srv.Close()

	rn := NewRunner([]Target{{ID: "fhir", Kind: KindFHIRMetadata, URL: srv.URL}}, srv.Client(), time.Now)
	results, err := rn.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := results[0]
	if got.OK {
		t.Errorf("OK = true, want false")
	}
	want := `resourceType "OperationOutcome", want CapabilityStatement`
	if !strings.Contains(got.Detail, want) {
		t.Errorf("Detail = %q, want to contain %q", got.Detail, want)
	}
}

// 3. fhir-metadata unreachable (closed port): Detail begins "GET <target>/metadata:"
// and never leaks a URL query or userinfo.
func TestFHIRMetadataUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	target := srv.URL
	srv.Close() // closed port: connections now refused

	rn := NewRunner([]Target{{ID: "fhir", Kind: KindFHIRMetadata, URL: target}}, http.DefaultClient, time.Now)
	results, err := rn.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := results[0]
	if got.OK {
		t.Errorf("OK = true, want false")
	}
	wantPrefix := fmt.Sprintf("GET %s/metadata:", targetOf(target))
	if !strings.HasPrefix(got.Detail, wantPrefix) {
		t.Errorf("Detail = %q, want prefix %q", got.Detail, wantPrefix)
	}
	if strings.ContainsAny(got.Detail, "?@") {
		t.Errorf("Detail leaks URL query/userinfo: %q", got.Detail)
	}
}

// 3b. fhir-metadata unreachable with a credential-bearing target URL: a
// transport error's *url.Error wraps the FULL request URL (Go's
// url.Error.Error() masks only the password — the username and the whole
// query string, which may itself be a credential like ?apikey=..., survive
// verbatim), so this pins that neither ever reaches Detail. Unlike
// TestFHIRMetadataUnreachable (whose target has no query/userinfo, so the
// "?@" check there can never fire), this target is deliberately
// credential-bearing and FAILS against the pre-fix code, which formatted
// %v directly over the raw client.Do error.
func TestFHIRMetadataUnreachableRedactsCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closed, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	srv.Close() // closed port: connections now refused

	closed.User = url.UserPassword("svcuser", "hunter2")
	closed.RawQuery = "apikey=SUPERSECRET"
	target := closed.String()

	rn := NewRunner([]Target{{ID: "fhir", Kind: KindFHIRMetadata, URL: target}}, http.DefaultClient, time.Now)
	results, err := rn.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := results[0]
	if got.OK {
		t.Errorf("OK = true, want false")
	}
	if strings.Contains(got.Detail, "SUPERSECRET") {
		t.Errorf("Detail leaks query-string secret: %q", got.Detail)
	}
	if strings.Contains(got.Detail, "svcuser") {
		t.Errorf("Detail leaks username: %q", got.Detail)
	}
	if strings.ContainsAny(got.Detail, "?@") {
		t.Errorf("Detail leaks URL query/userinfo markers: %q", got.Detail)
	}
}

// 3c (IMPORTANT-1 regression). A base URL that already carries a query
// string must not be corrupted by metadata-path construction: the
// "/metadata" path segment must land before the query, not be appended
// after it as a literal suffix.
func TestFHIRMetadataPreservesPathAndQuery(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"resourceType":"CapabilityStatement","fhirVersion":"4.0.1"}`))
	}))
	defer srv.Close()

	base := srv.URL + "/fhir?apikey=X"
	rn := NewRunner([]Target{{ID: "fhir", Kind: KindFHIRMetadata, URL: base}}, srv.Client(), time.Now)
	results, err := rn.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].OK {
		t.Errorf("OK = false, want true; detail=%q", results[0].Detail)
	}
	if gotPath != "/fhir/metadata" {
		t.Errorf("request path = %q, want /fhir/metadata", gotPath)
	}
	if gotQuery != "apikey=X" {
		t.Errorf("request query = %q, want apikey=X", gotQuery)
	}
}

// 3d (IMPORTANT-2 regression). A non-2xx response must not be decoded as if
// it might be a CapabilityStatement (nor surface its body in Detail); it's
// reported !OK with just the status.
func TestFHIRMetadataNon2xxStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"resourceType":"OperationOutcome","issue":[{"details":{"text":"invalid bearer token abc123"}}]}`))
	}))
	defer srv.Close()

	rn := NewRunner([]Target{{ID: "fhir", Kind: KindFHIRMetadata, URL: srv.URL}}, srv.Client(), time.Now)
	results, err := rn.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := results[0]
	if got.OK {
		t.Errorf("OK = true, want false")
	}
	if want := "HTTP 401"; got.Detail != want {
		t.Errorf("Detail = %q, want %q", got.Detail, want)
	}
	if strings.Contains(got.Detail, "abc123") {
		t.Errorf("Detail leaks response body: %q", got.Detail)
	}
}

// 4. token OK / token error: error text must never surface verbatim (redaction).
func TestToken(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		rn := NewRunner([]Target{{
			ID:         "tok",
			Kind:       KindToken,
			URL:        "https://idp.example",
			TokenFetch: func(ctx context.Context) error { return nil },
		}}, http.DefaultClient, time.Now)
		results, err := rn.Run(context.Background())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !results[0].OK {
			t.Errorf("OK = false, want true")
		}
	})

	t.Run("error", func(t *testing.T) {
		const secret = "client_secret=sk_live_totally_a_real_secret"
		rn := NewRunner([]Target{{
			ID:   "tok",
			Kind: KindToken,
			URL:  "https://idp.example",
			TokenFetch: func(ctx context.Context) error {
				return fmt.Errorf("token request failed: %s", secret)
			},
		}}, http.DefaultClient, time.Now)
		results, err := rn.Run(context.Background())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		got := results[0]
		if got.OK {
			t.Errorf("OK = true, want false")
		}
		if strings.Contains(got.Detail, secret) {
			t.Errorf("Detail leaks secret: %q", got.Detail)
		}
		if want := "credential check failed"; got.Detail != want {
			t.Errorf("Detail = %q, want %q", got.Detail, want)
		}
	})

	t.Run("status error classifies HTTP code without raw error text", func(t *testing.T) {
		rn := NewRunner([]Target{{
			ID:   "tok",
			Kind: KindToken,
			URL:  "https://idp.example",
			TokenFetch: func(ctx context.Context) error {
				return &StatusError{Code: 401}
			},
		}}, http.DefaultClient, time.Now)
		results, err := rn.Run(context.Background())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		got := results[0]
		if got.OK {
			t.Errorf("OK = true, want false")
		}
		if want := "credential check failed (HTTP 401)"; got.Detail != want {
			t.Errorf("Detail = %q, want %q", got.Detail, want)
		}
	})
}

// 5. reachable: 200 and 404 are OK (edge exists), 502 is not.
func TestReachable(t *testing.T) {
	for _, tc := range []struct {
		status int
		wantOK bool
	}{
		{http.StatusOK, true},
		{http.StatusNotFound, true},
		{http.StatusBadGateway, false},
	} {
		t.Run(fmt.Sprintf("status=%d", tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			rn := NewRunner([]Target{{ID: "svc", Kind: KindReachable, URL: srv.URL}}, srv.Client(), time.Now)
			results, err := rn.Run(context.Background())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if results[0].OK != tc.wantOK {
				t.Errorf("status %d: OK = %v, want %v (detail=%q)", tc.status, results[0].OK, tc.wantOK, results[0].Detail)
			}
		})
	}
}

// 6. single-flight: a second Run while the first is blocked returns ErrBusy.
func TestSingleFlight(t *testing.T) {
	handlerStarted := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(handlerStarted)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rn := NewRunner([]Target{{ID: "svc", Kind: KindReachable, URL: srv.URL}}, srv.Client(), time.Now)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := rn.Run(context.Background()); err != nil {
			t.Errorf("first Run: %v", err)
		}
	}()
	<-handlerStarted // by now r.busy is set: Run() locks/sets busy before issuing the HTTP request

	if _, err := rn.Run(context.Background()); !errors.Is(err, ErrBusy) {
		t.Errorf("second Run err = %v, want ErrBusy", err)
	}

	close(release)
	<-done
}

// 7. cooldown: two sequential Runs inside 30s (injected clock) return the same
// cached results without hitting the server again.
func TestCooldown(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	clock := newFakeClock(time.Unix(1_000_000, 0))
	rn := NewRunner([]Target{{ID: "svc", Kind: KindReachable, URL: srv.URL}}, srv.Client(), clock.Now)

	first, err := rn.Run(context.Background())
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	clock.Advance(10 * time.Second) // still within the 30s cooldown

	second, err := rn.Run(context.Background())
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("server hit count = %d, want 1 (second Run should be served from cache)", got)
	}
	if len(second) != len(first) || second[0].ID != first[0].ID || second[0].CheckedAt != first[0].CheckedAt {
		t.Errorf("second Run = %+v, want cached copy of first Run %+v", second, first)
	}

	// IMPORTANT-3 regression: cooldown must actually expire, not cache
	// forever. Advance past the 30s window (measured from the second Run's
	// completion) and confirm a fresh probe hits the server again.
	clock.Advance(31 * time.Second)
	third, err := rn.Run(context.Background())
	if err != nil {
		t.Fatalf("third Run: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("server hit count = %d, want 2 (cooldown should have expired and re-probed)", got)
	}
	if third[0].CheckedAt == second[0].CheckedAt {
		t.Errorf("third Run CheckedAt = %v, want it to have advanced past the cached value %v", third[0].CheckedAt, second[0].CheckedAt)
	}
}

// 8. Last before any run.
func TestLastBeforeAnyRun(t *testing.T) {
	rn := NewRunner(nil, http.DefaultClient, time.Now)
	results, at, ok := rn.Last()
	if ok {
		t.Errorf("ok = true, want false before the first run")
	}
	if results != nil {
		t.Errorf("results = %v, want nil", results)
	}
	if !at.IsZero() {
		t.Errorf("at = %v, want zero time", at)
	}
}

// 9. redaction: a target with userinfo and a query string renders scheme://host only.
func TestTargetOfRedaction(t *testing.T) {
	got := targetOf("https://u:p@h.example/path?x=1")
	want := "https://h.example"
	if got != want {
		t.Errorf("targetOf = %q, want %q", got, want)
	}
}

// 10. global deadline: probes against a server that never responds must not
// blow past the run's deadline; probes that never got a turn report the fixed
// "not checked" detail. Uses a short overridden deadline (test-only field) —
// the injected clock can't fake a context deadline.
func TestGlobalDeadline(t *testing.T) {
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // never responds; only unblocks when the client cancels
	}))
	defer hang.Close()

	targets := []Target{
		{ID: "a", Kind: KindReachable, URL: hang.URL},
		{ID: "b", Kind: KindReachable, URL: hang.URL},
		{ID: "c", Kind: KindReachable, URL: hang.URL},
	}
	rn := NewRunner(targets, hang.Client(), time.Now)
	rn.deadline = 100 * time.Millisecond // unexported test-overridable field

	start := time.Now()
	results, err := rn.Run(context.Background())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed > runDeadline {
		t.Errorf("Run took %v, want well under the package runDeadline (%v)", elapsed, runDeadline)
	}
	if len(results) != len(targets) {
		t.Fatalf("got %d results, want %d", len(results), len(targets))
	}

	var neverRan int
	for _, res := range results {
		if res.OK {
			t.Errorf("result %s: OK = true, want false", res.ID)
		}
		if res.Detail == "not checked — run deadline exceeded" {
			neverRan++
		}
	}
	if neverRan == 0 {
		t.Errorf("expected at least one probe reported as not checked, got none in %+v", results)
	}
}
