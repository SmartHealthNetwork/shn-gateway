package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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

// shutdownMember is the member the delivered CRD order-select names. Only the
// order's own $validate carries it in a ServiceRequest, which is how the
// validator stub tells that call from every other.
const shutdownMember = "MBR-shutdown"

// shutdownFlush is a payer gateway's surroundings at observe: a validator
// that holds one governed check until released, a diagnostic ingest that
// records the kind of every event it receives, and the network a sealed leg
// arrives through (the Hub's and the Authorization Framework's key, and a
// registrar that lists the requester).
type shutdownFlush struct {
	env       map[string]string
	heartbeat chan struct{}
	held      chan struct{}
	release   func()
	netPriv   ed25519.PrivateKey // signs the Hub assertion and the leg's token
	payer     shnsdk.Identity
	requester shnsdk.Identity
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

	// The validator holds the payer's observe-level ingress check of the
	// order the CRD request carries — the canonical lane's
	// POST /fhir/ServiceRequest/$validate whose body names shutdownMember —
	// until released, then finds it invalid. Every other call (the Coverage's
	// check, the lanes' qualification and warm-up) is answered at once.
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
		if r.URL.Path == "/fhir/ServiceRequest/$validate" && bytes.Contains(body, []byte(shutdownMember)) {
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

	// One key stands for both the Hub and the Authorization Framework:
	// discovery names it for both.
	netPub, netPriv, _ := ed25519.GenerateKey(rand.Reader)
	f.netPriv = netPriv
	keyBody := fmt.Sprintf(`{"pubkey":%q}`, base64.StdEncoding.EncodeToString(netPub))
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(keyBody)) }))
	t.Cleanup(keys.Close)
	// The Authorization Framework grants the payer's answer whatever it asks for.
	authz := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req shnsdk.AuthorizeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"token": f.sign(shnsdk.Token{
			Operation: req.Operation, Subject: req.SubjectPCI, Frame: req.Frame, Holder: f.payer.HolderID,
			CorrelationID: req.CorrelationID, PayloadHash: req.PayloadHash, Expiry: time.Now().Add(time.Hour),
		})})
	}))
	t.Cleanup(authz.Close)
	disc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"endpoints":{"authz":%q},"authzPublicKeyURL":%q,"hubTransportKeyURL":%q}`, authz.URL, keys.URL, keys.URL)
	}))
	t.Cleanup(disc.Close)
	var err error
	if f.requester, err = shnsdk.GenerateIdentity("h-provider-shutdown"); err != nil {
		t.Fatal(err)
	}
	// The registrar's feed lists the requester, whose key the answer is sealed to.
	holders, err := json.Marshal([]shnsdk.Holder{{
		ID: f.requester.HolderID, Role: "provider", BaseURL: "https://provider.example",
		EncPub:  base64.StdEncoding.EncodeToString(f.requester.EncPub[:]),
		SignPub: base64.StdEncoding.EncodeToString(f.requester.SignPub),
	}})
	if err != nil {
		t.Fatal(err)
	}
	registrar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(holders)
	}))
	t.Cleanup(registrar.Close)
	// The payer's own CDS service, which the gateway forwards the leg to: its
	// discovery lists the service, and it answers with no cards.
	system := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"services":[{"id":"svc","hook":"order-select"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"cards":[]}`))
	}))
	t.Cleanup(system.Close)

	dir := t.TempDir()
	if f.payer, err = shnsdk.GenerateIdentity("h-payer-shutdown"); err != nil {
		t.Fatal(err)
	}
	if err := shnsdk.WriteBundle(dir, f.payer, "payer", "https://holder.example"); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("synthetic-observation-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	f.env = map[string]string{
		"ROLE": "payer", "SHN_SECRETS": dir, "SHN_DISCOVERY_URL": disc.URL,
		"REGISTRAR_URL":                registrar.URL,
		"CONFORMANCE_ENFORCEMENT":      "observe",
		"FHIR_VALIDATE_URL":            validator.URL + "/fhir",
		"FHIR_VALIDATE_URL_2_1":        validator.URL + "/fhir21",
		"FHIR_VALIDATE_URL_2_2":        validator.URL + "/fhir22",
		"PAYER_DAVINCI_BASE_URL":       system.URL,
		"PAYER_DAVINCI_CRD_SERVICE_ID": "svc",
		"DIAGNOSTIC_SINK_URL":          ingest.URL + "/internal/pa-test/observations",
		"DIAGNOSTIC_SOURCE":            "configured-test-payer",
		"DIAGNOSTIC_KEY_FILE":          key,
	}
	return f
}

func (f *shutdownFlush) getenv(k string) string { return f.env[k] }

// sign signs tok as the Authorization Framework does.
func (f *shutdownFlush) sign(tok shnsdk.Token) shnsdk.Token {
	tok.Signature = nil
	unsigned, _ := json.Marshal(tok)
	tok.Signature = ed25519.Sign(f.netPriv, unsigned)
	return tok
}

// shutdownCRDRequest is a CRD order-select whose order, coverage and context
// all name shutdownMember.
const shutdownCRDRequest = `{"hook":"order-select","hookInstance":"hi-shutdown",` +
	`"context":{"userId":"Practitioner/p1","patientId":"` + shutdownMember + `",` +
	`"draftOrders":{"resourceType":"Bundle","type":"collection","entry":[{"fullUrl":"urn:uuid:sr1","resource":` +
	`{"resourceType":"ServiceRequest","id":"sr1","status":"draft","intent":"order","subject":{"reference":"Patient/` + shutdownMember + `"},` +
	`"code":{"coding":[{"system":"http://www.ama-assn.org/go/cpt","code":"72148"}]}}}]},"selections":["ServiceRequest/sr1"]},` +
	`"prefetch":{"coverage":{"resourceType":"Coverage","id":"c1","beneficiary":{"reference":"Patient/` + shutdownMember + `"}}}}`

// deliver is one CRD order-select leg the Hub forwards to the payer gateway at
// base: sealed to the payer, authorized by a token bound to its ciphertext,
// and carrying the Hub's assertion. The payer checks the order at observe off
// the request path, forwards the leg to its system and answers.
func (f *shutdownFlush) deliver(t *testing.T, base string) {
	t.Helper()
	now := time.Now()
	corr := "corr-shutdown-" + randHex(t, 4)
	env, err := shnsdk.Seal(shnsdk.Metadata{
		Sender: f.requester.HolderID, Recipient: f.payer.HolderID, TransactionType: "crd-order-select",
		AuthorityFrame: "provider-tpo", Timestamp: now.Format(time.RFC3339), CorrelationID: corr,
	}, []byte(shutdownCRDRequest), f.payer.EncPub)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(env.Ciphertext)
	tok := shnsdk.Token{
		Operation: "crd-order-select", Scope: "crd-context", Subject: shnsdk.ResolvePCI(shutdownMember, "1970-01-01", "Shutdown"),
		Frame: "provider-tpo", Holder: f.requester.HolderID, CorrelationID: corr,
		PayloadHash: hex.EncodeToString(digest[:]), Expiry: now.Add(time.Hour),
	}
	tokJSON, _ := json.Marshal(f.sign(tok))
	env.Metadata.AuthzToken = string(tokJSON)
	raw, err := shnsdk.EncodeEnvelope(env)
	if err != nil {
		t.Fatal(err)
	}
	as, _ := json.Marshal(shnsdk.IssueAssertion("hub", f.payer.HolderID, f.netPriv, now, time.Minute))
	req, err := http.NewRequest(http.MethodPost, base+"/substrate/inbound", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Hub-Assertion", base64.StdEncoding.EncodeToString(as))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("crd-order-select leg = %d %s, want 200 with its check queued", resp.StatusCode, body)
	}
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
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		awaitFlush(t, f.heartbeat, 5*time.Second, "the publisher's first heartbeat")
		f.deliver(t, srv.URL)
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
		// up, so the leg waits, bounded, for it to accept.
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
		f.deliver(t, "http://"+addr)
		f.deliversFlushedFinding(t, func() <-chan struct{} { cancel(); return done })
		if runErr != nil {
			t.Fatalf("Run: %v", runErr)
		}
	})
}
