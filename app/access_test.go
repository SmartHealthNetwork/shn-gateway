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
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"

	"github.com/SmartHealthNetwork/shn-gateway/diagnostics"
	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
)

func refusedRecord() engine.ExchangeRecord {
	return engine.ExchangeRecord{
		Start: time.Date(2026, 10, 7, 14, 0, 0, 0, time.UTC), CorrelationID: "leg-1", Trace: "caller-1", CallID: "call-1",
		Direction: engine.DirectionInbound, Route: engine.RouteSubstrateInbound, Exchange: "pas-claim", Operation: "",
		ContractLine: "2.1", Sender: "provider-a", Recipient: "payer-b",
		RequestCiphertextHash: strings.Repeat("a", 64), ResponseCiphertextHash: strings.Repeat("b", 64),
		Outcome: engine.ExchangeRefused, RefusedBy: engine.RefusedByPayerGateway, Rule: engine.RefusalConformance,
		Status: 400, Latency: 1500 * time.Millisecond,
		Backend: &engine.BackendCall{Status: 503, Latency: 250 * time.Millisecond, ErrorClass: engine.BackendHTTP5xx}, BackendCalls: 2,
		Findings: engine.FindingSummary{Count: 2, Refused: true, Kinds: []string{"content"}},
	}
}

// The access line is the record, in the JSON the network's stats service
// reads: durations in milliseconds, a refusal only on a refused outcome, the
// backend only when the participant's system was called.
func TestAccessLineFor(t *testing.T) {
	b, err := json.Marshal(accessLineFor(refusedRecord()))
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"time":"2026-10-07T14:00:00Z","correlationId":"leg-1","trace":"caller-1","callId":"call-1","direction":"inbound","route":"substrate-inbound","exchange":"pas-claim","contractLine":"2.1","sender":"provider-a","recipient":"payer-b",` +
		`"requestCiphertextHash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","responseCiphertextHash":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",` +
		`"outcome":"refused","refusal":{"by":"payer-gateway","rule":"conformance"},"status":400,"latencyMs":1500,"backend":{"status":503,"latencyMs":250,"errorClass":"http-5xx","calls":2},"findings":{"count":2,"refused":true,"kinds":["content"]}}`
	if string(b) != want {
		t.Fatalf("access line\n got %s\nwant %s", b, want)
	}

	answered := engine.ExchangeRecord{Direction: engine.DirectionIngress, Route: engine.RouteCRD, Exchange: "crd-order-select", Outcome: engine.ExchangeAnswered, Status: 200}
	line := accessLineFor(answered)
	if line.Refusal != nil || line.Backend != nil {
		t.Fatalf("an answered ingress call rendered %+v", line)
	}
}

// The line is always written; with capture configured the same JSON is
// published as a metadata-only event that carries no body and no headers.
func TestAccessHook(t *testing.T) {
	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	now := time.Date(2026, 10, 7, 14, 0, 2, 0, time.UTC)

	accessHook(logf, nil, func() time.Time { return now })(refusedRecord())
	if len(logged) != 1 || !strings.HasPrefix(logged[0], accessLinePrefix) {
		t.Fatalf("logged %q", logged)
	}
	detail := strings.TrimPrefix(logged[0], accessLinePrefix)
	var line diagnostics.AccessLine
	if err := json.Unmarshal([]byte(detail), &line); err != nil || line.CorrelationID != "leg-1" {
		t.Fatalf("the logged line is not the access line JSON: %v %q", err, detail)
	}

	var events []diagnostics.Event
	logged = nil
	accessHook(logf, func(e diagnostics.Event) bool { events = append(events, e); return true }, func() time.Time { return now })(refusedRecord())
	if len(logged) != 1 || len(events) != 1 {
		t.Fatalf("logged %d lines and published %d events, want 1 and 1", len(logged), len(events))
	}
	e := events[0]
	if e.Kind != diagnostics.KindAccess || e.Detail != detail || e.CorrelationID != "leg-1" || e.CallID != "call-1" ||
		e.RequestCiphertextHash != strings.Repeat("a", 64) || e.Sender != "provider-a" || e.Recipient != "payer-b" ||
		e.LegType != "pas-claim" || e.Status != 400 || e.DurationNanos != int64(1500*time.Millisecond) || !e.Time.Equal(now) {
		t.Fatalf("event = %+v", e)
	}
	if e.Body != nil || e.Headers != nil || e.URL != "" {
		t.Fatalf("the access event carries more than metadata: %+v", e)
	}
}

// One consumer that panics does not stop the next.
func TestExchangeObservers_IsolatesPanics(t *testing.T) {
	var logBuf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(prev)
	var ran []string
	exchangeObservers(
		func(engine.ExchangeRecord) { ran = append(ran, "first"); panic("boom\ngateway: access: {}") },
		nil,
		func(engine.ExchangeRecord) { ran = append(ran, "second") },
	)(engine.ExchangeRecord{})
	if strings.Join(ran, ",") != "first,second" {
		t.Fatalf("ran %v", ran)
	}
	// The panic is visible, and cannot start an access line of its own.
	if got := logBuf.String(); !strings.Contains(got, `an exchange observer panicked: "boom\ngateway: access: {}"`) || strings.Contains(got, "\n"+accessLinePrefix) {
		t.Fatalf("log: %q", got)
	}
}

// The access line is on in every built gateway, with nothing to configure:
// the earliest refusal a payer's gateway gives — a call without the Hub's
// assertion — is written as one.
func TestBuild_WritesAnAccessLinePerExchange(t *testing.T) {
	dir := t.TempDir()
	id, err := shnsdk.GenerateIdentity("h-payer")
	if err != nil {
		t.Fatal(err)
	}
	if err := shnsdk.WriteBundle(dir, id, "payer", "https://holder.example"); err != nil {
		t.Fatal(err)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyBody := fmt.Sprintf(`{"pubkey":%q}`, base64.StdEncoding.EncodeToString(pub))
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(keyBody)) }))
	defer keys.Close()
	disc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"endpoints":{},"authzPublicKeyURL":%q,"hubTransportKeyURL":%q}`, keys.URL, keys.URL)
	}))
	defer disc.Close()
	// The payer's own system, never called by this refusal.
	payerSystem := httptest.NewServer(http.NotFoundHandler())
	defer payerSystem.Close()
	env := map[string]string{
		"ROLE": "payer", "SHN_SECRETS": dir, "SHN_DISCOVERY_URL": disc.URL, "SHN_FAKE_VALIDATOR": "1",
		"FHIR_DATA_URL": "https://fhir.test", "PAYER_DAVINCI_BASE_URL": payerSystem.URL,
	}
	// Swaps the process-global logger: gateway/app has no t.Parallel() test.
	var logBuf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(prev)
	b, err := build(context.Background(), func(k string) string { return env[k] }, io.Discard, nil)
	t.Cleanup(func() {
		if b.gateway != nil {
			_ = b.gateway.Close()
		}
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	w := httptest.NewRecorder()
	b.handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/substrate/inbound", strings.NewReader("{}")))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var line diagnostics.AccessLine
	for _, l := range strings.Split(logBuf.String(), "\n") {
		if _, after, ok := strings.Cut(l, accessLinePrefix); ok {
			if err := json.Unmarshal([]byte(after), &line); err != nil {
				t.Fatalf("access line is not JSON: %v %q", err, l)
			}
		}
	}
	if line.Outcome != engine.ExchangeRefused || line.Refusal == nil || line.Refusal.Rule != engine.RefusalAuthentication ||
		line.Direction != engine.DirectionInbound || line.Status != http.StatusForbidden {
		t.Fatalf("access line = %+v in log:\n%s", line, logBuf.String())
	}
}

// PAYER_DAVINCI_BACKEND_CORRELATION=off reaches the payer's responder: the
// built gateway says its partner requests carry no X-Correlation-Id, and
// says nothing of it by default.
// buildPayerAt builds a payer gateway whose own system is at payerSystem, with
// extra environment on top of the minimal set; the gateway is closed at the
// test's end.
func buildPayerAt(t *testing.T, payerSystem string, extra map[string]string) (built, string) {
	t.Helper()
	dir := t.TempDir()
	id, err := shnsdk.GenerateIdentity("h-payer")
	if err != nil {
		t.Fatal(err)
	}
	if err := shnsdk.WriteBundle(dir, id, "payer", "https://holder.example"); err != nil {
		t.Fatal(err)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyBody := fmt.Sprintf(`{"pubkey":%q}`, base64.StdEncoding.EncodeToString(pub))
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(keyBody)) }))
	t.Cleanup(keys.Close)
	disc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"endpoints":{},"authzPublicKeyURL":%q,"hubTransportKeyURL":%q}`, keys.URL, keys.URL)
	}))
	t.Cleanup(disc.Close)
	env := map[string]string{
		"ROLE": "payer", "SHN_SECRETS": dir, "SHN_DISCOVERY_URL": disc.URL, "SHN_FAKE_VALIDATOR": "1",
		"FHIR_DATA_URL": "https://fhir.test", "PAYER_DAVINCI_BASE_URL": payerSystem,
	}
	for k, v := range extra {
		env[k] = v
	}
	var stdout bytes.Buffer
	b, err := build(context.Background(), func(k string) string { return env[k] }, &stdout, nil)
	if b.gateway != nil {
		t.Cleanup(func() { _ = b.gateway.Close() })
	}
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return b, stdout.String()
}

// orderSelectHook is a minimal CRD order-select request.
var orderSelectHook = []byte(`{"hook":"order-select","hookInstance":"d1577c69-dfbe-44ad-ba6d-3e05e953b2ea","context":{"userId":"Practitioner/p1","patientId":"p1","selections":["DeviceRequest/d1"],"draftOrders":{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"DeviceRequest","id":"d1","status":"draft","intent":"order","subject":{"reference":"Patient/p1"}}}]}}}`)

func TestBuild_BackendCorrelationOffReachesTheResponder(t *testing.T) {
	// The payer's system lists one CRD service and answers every hook with no
	// cards, recording the X-Correlation-Id each hook call carried.
	var mu sync.Mutex
	var sent []string
	payerSystem := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"services":[{"id":"crd-select","hook":"order-select"}]}`))
			return
		}
		mu.Lock()
		sent = append(sent, r.Header.Get("X-Correlation-Id"))
		mu.Unlock()
		_, _ = w.Write([]byte(`{"cards":[]}`))
	}))
	defer payerSystem.Close()
	for value, want := range map[string]string{"": "leg-0001", "on": "leg-0001", "off": ""} {
		b, _ := buildPayerAt(t, payerSystem.URL, map[string]string{"PAYER_DAVINCI_BACKEND_CORRELATION": value})
		mu.Lock()
		sent = nil
		mu.Unlock()
		res, err := b.responder.Handle(context.Background(), "crd-order-select", "leg-0001", "", orderSelectHook)
		mu.Lock()
		got := slices.Clone(sent)
		mu.Unlock()
		if err != nil || len(got) != 1 {
			t.Fatalf("%q: the hook was not forwarded once: %d calls, %+v %v", value, len(got), res, err)
		}
		if got[0] != want {
			t.Errorf("%q: the payer's system was sent X-Correlation-Id %q, want %q", value, got[0], want)
		}
	}
}

// PAYER_DAVINCI_BACKEND_TIMEOUT reaches the responder: a payer's system slower
// than it fails the forward as its own timeout, well before its answer.
func TestBuild_BackendTimeoutReachesTheResponder(t *testing.T) {
	payerSystem := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"services":[{"id":"crd-select","hook":"order-select"}]}`))
			return
		}
		// Read the request first: net/http then ends r.Context() when the
		// gateway gives up, and the stub returns with it.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte(`{"cards":[]}`))
	}))
	defer payerSystem.Close()
	b, stdout := buildPayerAt(t, payerSystem.URL, map[string]string{"PAYER_DAVINCI_BACKEND_TIMEOUT": "1s"})
	if !strings.Contains(stdout, "partner requests time out after 1s (PAYER_DAVINCI_BACKEND_TIMEOUT)") {
		t.Errorf("boot does not say the timeout:\n%s", stdout)
	}
	start := time.Now()
	_, err := b.responder.Handle(context.Background(), "crd-order-select", "leg-0001", "", orderSelectHook)
	if waited := time.Since(start); err == nil || !strings.Contains(err.Error(), "did not answer within 1s") || waited > 4*time.Second {
		t.Fatalf("after %s: %v, want the payer system's timeout at 1s", waited, err)
	}
}
