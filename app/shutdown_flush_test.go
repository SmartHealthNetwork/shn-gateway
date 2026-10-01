package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/diagnostics"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// shutdownFlush is a payer gateway's surroundings at observe: a validator
// that holds one check until released, and a diagnostic ingest that records
// the kind of every event it receives.
type shutdownFlush struct {
	env       map[string]string
	heartbeat chan struct{}
	held      chan struct{}
	release   func()
	authzPriv ed25519.PrivateKey
	mu        sync.Mutex
	kinds     []string
	findings  []string // each conformance finding's detail
}

func newShutdownFlush(t *testing.T) *shutdownFlush {
	t.Helper()
	f := &shutdownFlush{heartbeat: make(chan struct{}, 16), held: make(chan struct{})}
	// The ingest signals each heartbeat. It never says it takes batches, so
	// events arrive one per request.
	ingest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/health") {
			select {
			case f.heartbeat <- struct{}{}:
			default:
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/batch") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var e diagnostics.Event
		_ = json.NewDecoder(r.Body).Decode(&e)
		f.mu.Lock()
		f.kinds = append(f.kinds, e.Kind)
		if e.Kind == diagnostics.KindConformanceFinding {
			f.findings = append(f.findings, e.Detail)
		}
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(ingest.Close)

	// The validator holds the canonical lane's $validate of the patient-access
	// searchset until released, then finds it invalid. Every other call (the
	// lanes' qualification and warm-up) is answered at once.
	release := make(chan struct{})
	var heldOnce, releaseOnce sync.Once
	f.release = func() { releaseOnce.Do(func() { close(release) }) }
	validator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/fhir+json")
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/metadata") {
			_, _ = w.Write([]byte(`{"resourceType":"CapabilityStatement","fhirVersion":"4.0.1"}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/fhir/Bundle/$validate" && bytes.Contains(body, []byte(`"searchset"`)) {
			heldOnce.Do(func() { close(f.held) })
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"structure","diagnostics":"synthetic defect"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"informational","diagnostics":"ok"}]}`))
	}))
	t.Cleanup(validator.Close)
	t.Cleanup(f.release) // runs before validator.Close: a failure must not leave the held call hanging

	authzPub, authzPriv, _ := ed25519.GenerateKey(rand.Reader)
	f.authzPriv = authzPriv
	keyBody := fmt.Sprintf(`{"pubkey":%q}`, base64.StdEncoding.EncodeToString(authzPub))
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(keyBody)) }))
	t.Cleanup(keys.Close)
	disc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"endpoints":{},"authzPublicKeyURL":%q,"hubTransportKeyURL":%q}`, keys.URL, keys.URL)
	}))
	t.Cleanup(disc.Close)
	// The payer's own systems and the audit plane: a patient-access read
	// reaches only the audit plane; the boot checks may probe the others.
	system := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"services":[]}`))
	}))
	t.Cleanup(system.Close)
	audit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(audit.Close)

	dir := t.TempDir()
	id, err := shnsdk.GenerateIdentity("h-payer-shutdown")
	if err != nil {
		t.Fatal(err)
	}
	if err := shnsdk.WriteBundle(dir, id, "payer", "https://holder.example"); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("synthetic-observation-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	f.env = map[string]string{
		"ROLE": "payer", "SHN_SECRETS": dir, "SHN_DISCOVERY_URL": disc.URL,
		"CONFORMANCE_ENFORCEMENT": "observe",
		"FHIR_VALIDATE_URL":       validator.URL + "/fhir",
		"FHIR_VALIDATE_URL_2_1":   validator.URL + "/fhir21",
		"FHIR_VALIDATE_URL_2_2":   validator.URL + "/fhir22",
		"FHIR_DATA_URL":           system.URL + "/fhir",
		"PAYER_DAVINCI_BASE_URL":  system.URL,
		"AUDIT_URL":               audit.URL,
		"DIAGNOSTIC_SINK_URL":     ingest.URL + "/internal/pa-test/observations",
		"DIAGNOSTIC_SOURCE":       "configured-test-payer",
		"DIAGNOSTIC_KEY_FILE":     key,
	}
	return f
}

func (f *shutdownFlush) getenv(k string) string { return f.env[k] }

// read is one patient-access read, whose searchset is checked at observe off
// the request path.
func (f *shutdownFlush) read(target string) *http.Request {
	tok := shnsdk.Token{
		Operation: "patient-access-read", Scope: "patient-access-only", Subject: "p-shutdown-1",
		Frame: "patient-access", Holder: "phg", CorrelationID: "corr-shutdown-1",
		Expiry: time.Now().Add(time.Hour),
	}
	unsigned, _ := json.Marshal(tok)
	tok.Signature = ed25519.Sign(f.authzPriv, unsigned)
	raw, _ := json.Marshal(tok)
	req := httptest.NewRequest(http.MethodGet, target+"/ExplanationOfBenefit?patient=p-shutdown-1", nil)
	req.RequestURI = ""
	req.Header.Set("Authorization", "Bearer "+base64.StdEncoding.EncodeToString(raw))
	return req
}

func awaitFlush(t *testing.T, ch <-chan struct{}, d time.Duration, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(d):
		t.Fatalf("timed out waiting for %s", label)
	}
}

// deliversFlushedFinding drives the shutdown with the check still held: shut
// begins it and returns a channel closed once it is done. The validator is
// released at the publisher's next heartbeat (its last one, if the publisher
// has already stopped) or after a short bound, so a publisher stopped before
// the gateway closes has drained before the finding is emitted.
func (f *shutdownFlush) deliversFlushedFinding(t *testing.T, shut func() <-chan struct{}) {
	t.Helper()
	awaitFlush(t, f.held, 5*time.Second, "the queued check's $validate")
	for len(f.heartbeat) > 0 { // only a heartbeat sent after the shutdown begins
		<-f.heartbeat
	}
	done := shut()
	select {
	case <-f.heartbeat:
	case <-done:
	case <-time.After(500 * time.Millisecond):
	}
	f.release()
	// The gateway's flush and the publisher's drain are each bounded at 10 s.
	awaitFlush(t, done, 25*time.Second, "the shutdown")
	f.mu.Lock()
	got, findings := slices.Clone(f.kinds), slices.Clone(f.findings)
	f.mu.Unlock()
	if !slices.Contains(got, diagnostics.KindConformanceFinding) {
		t.Fatalf("the ingest got %v, want the %s the shutdown flushed", got, diagnostics.KindConformanceFinding)
	}
	// The flush waited for the validator's verdict rather than cancelling the
	// held check into "unavailable".
	for _, d := range findings {
		if strings.Contains(d, `"verdict":"unavailable"`) || !strings.Contains(d, `"issues"`) {
			t.Fatalf("the flushed finding does not carry the validator's verdict: %s", d)
		}
	}
}

// A check the observe queue still holds at shutdown is flushed by the
// gateway's close, and its finding reaches the diagnostic ingest: both the
// managed handler's Close and Run close the gateway before they stop the
// diagnostic publisher.
func TestShutdownDeliversTheFindingsItFlushes(t *testing.T) {
	t.Run("HandlerWithClock", func(t *testing.T) {
		f := newShutdownFlush(t)
		h, err := HandlerWithClock(context.Background(), f.getenv, io.Discard, time.Now)
		if err != nil {
			t.Fatalf("HandlerWithClock: %v", err)
		}
		closer := h.(io.Closer)
		t.Cleanup(func() { f.release(); _ = closer.Close() }) // idempotent; joins the runtime if the test fails early
		awaitFlush(t, f.heartbeat, 5*time.Second, "the publisher's first heartbeat")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, f.read(""))
		if rec.Code != http.StatusOK {
			t.Fatalf("patient-access read = %d %s, want 200 with its check queued", rec.Code, rec.Body)
		}
		f.deliversFlushedFinding(t, func() <-chan struct{} {
			done := make(chan struct{})
			go func() { _ = closer.Close(); close(done) }()
			return done
		})
	})
	t.Run("Run", func(t *testing.T) {
		f := newShutdownFlush(t)
		f.env["HOST"] = "127.0.0.1"
		f.env["PORT"] = freePort(t)
		addr := net.JoinHostPort(f.env["HOST"], f.env["PORT"])
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		var runErr error
		go func() { runErr = Run(ctx, f.getenv, io.Discard); close(done) }()
		t.Cleanup(func() { cancel(); f.release(); <-done })
		awaitFlush(t, f.heartbeat, 5*time.Second, "the publisher's first heartbeat")
		// The listener starts after the publisher; Run says nothing when it is
		// up, so the read waits, bounded, for it to accept.
		deadline := time.Now().Add(5 * time.Second)
		for {
			c, err := net.Dial("tcp", addr)
			if err == nil {
				c.Close()
				break
			}
			select {
			case <-done:
				t.Fatalf("Run returned before serving: %v", runErr)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("Run's listener at %s did not accept: %v", addr, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
		resp, err := http.DefaultClient.Do(f.read("http://" + addr))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("patient-access read = %d %s, want 200 with its check queued", resp.StatusCode, body)
		}
		f.deliversFlushedFinding(t, func() <-chan struct{} { cancel(); return done })
		if runErr != nil {
			t.Fatalf("Run: %v", runErr)
		}
	})
}
