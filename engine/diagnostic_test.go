package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SmartHealthNetwork/shn-gateway/diagnostics"
	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type diagnosticBrokenBody struct{ reads int }

func (b *diagnosticBrokenBody) Read([]byte) (int, error) {
	b.reads++
	return 0, errors.New("broken body")
}
func (*diagnosticBrokenBody) Close() error { return nil }

func TestObserverIngressUnauthenticatedBrokenBodyNeutral(t *testing.T) {
	for _, observed := range []bool{false, true} {
		g := &Gateway{cfg: Config{Clock: time.Now}}
		if observed {
			g.cfg.Observer = func(ObserverEvent) {}
		}
		body := &diagnosticBrokenBody{}
		r := httptest.NewRequest(http.MethodPost, "/Claim/$submit", nil)
		r.Body = body
		w := httptest.NewRecorder()
		g.observeIngress("pas-ingress", g.handlePASIngress)(w, r)
		if w.Code != 401 || body.reads != 0 {
			t.Fatalf("observed=%v status=%d reads=%d; want untouched 401", observed, w.Code, body.reads)
		}
	}
}

func TestDiagnosticIngressTraceOnlyAttributes(t *testing.T) {
	now := time.Unix(1700000000, 0)
	key := []byte("synthetic-trace-verification-key")
	for _, proof := range []string{"", "invalid", diagnostics.TraceProof(key, "call-one", "POST", "/Claim/$submit", now)} {
		for _, broken := range []bool{false, true} {
			var events []diagnostics.Event
			g := &Gateway{cfg: Config{Clock: func() time.Time { return now }, DiagnosticTraceKey: key, Diagnostic: func(e diagnostics.Event) bool { events = append(events, e); return true }}}
			r := httptest.NewRequest("POST", "/Claim/$submit", strings.NewReader("invalid-json"))
			r.Header.Set("X-SHN-Test-Trace", proof)
			if broken {
				r.Body = &diagnosticBrokenBody{}
			}
			w := httptest.NewRecorder()
			g.observeIngress("pas-ingress", g.handlePASIngress)(w, r)
			if w.Code != 401 {
				t.Fatalf("proof=%q broken=%v status=%d", proof, broken, w.Code)
			}
			if len(events) != 2 {
				t.Fatalf("got %d events", len(events))
			}
			want := ""
			if proof != "" && proof != "invalid" {
				want = "call-one"
			}
			if events[0].CallID != want || events[0].BodyComplete {
				t.Fatalf("unread request attributed incorrectly: %+v", events[0])
			}
		}
	}
}

func TestDiagnosticIngressFingerprintAvailableDuringHandling(t *testing.T) {
	var got diagnostics.Event
	g := &Gateway{cfg: Config{Clock: time.Now, Diagnostic: func(e diagnostics.Event) bool {
		if e.Kind == "stage" {
			got = e
		}
		return true
	}}}
	h := g.observeIngress("test", func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		g.diagnostic(diagnostics.Event{Kind: "stage", RequestFingerprint: diagnostics.IngressFingerprint(r.Context())})
		w.Write([]byte("raw-response"))
	})
	h(httptest.NewRecorder(), httptest.NewRequest("POST", "/x?q=1", strings.NewReader("exact-body")))
	if !got.RequestFingerprint.Complete || got.RequestFingerprint.BodySHA256 != sha256hex([]byte("exact-body")) || got.RequestFingerprint.RequestURI != "/x?q=1" {
		t.Fatalf("missing live fingerprint: %+v", got)
	}
}

func TestDiagnosticSealedConcurrentReusedCorrelation(t *testing.T) {
	env := newInProcessExchange(t)
	var mu sync.Mutex
	var events []diagnostics.Event
	env.originator.cfg.Diagnostic = func(e diagnostics.Event) bool { mu.Lock(); defer mu.Unlock(); events = append(events, e); return true }
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "crd-order-select", "pci-1", "reused", "", Content{WorkstreamType: workstreamPA, Payload: testRequest(env.crdReq)})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var sealed []diagnostics.Event
	for _, e := range events {
		if e.Kind == "leg.sealed" {
			sealed = append(sealed, e)
		}
	}
	if len(sealed) != 2 {
		t.Fatalf("got %d sealed observations", len(sealed))
	}
	if sealed[0].RequestCiphertextHash == "" || sealed[0].RequestCiphertextHash == sealed[1].RequestCiphertextHash {
		t.Fatal("different seals linked by reused correlation")
	}
	for _, e := range sealed {
		if e.Sender != "provider" || e.Recipient != env.payerID || e.CorrelationID != "reused" {
			t.Fatalf("wrong seal binding: %+v", e)
		}
	}
}

func TestNativeDiagnosticRawNonJSONResponse(t *testing.T) {
	var events []diagnostics.Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "9")
		w.WriteHeader(502)
		w.Write([]byte("raw upstream failure"))
	}))
	defer srv.Close()
	n := NewNativeResponder(srv.Client(), srv.URL, "crd", nil, time.Now, WithNativeDiagnostic(func(e diagnostics.Event) bool { events = append(events, e); return true }))
	ctx := diagnostics.WithRequestIdentity(context.Background(), "actual-ciphertext", "provider", "payer", "reused")
	_, result, err := n.post(ctx, srv.URL, "/x", testRequest([]byte("{}")), "pas-claim", "submit")
	if err != nil || result.Status != 502 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	var found bool
	for _, e := range events {
		if e.Kind == "native.response" {
			found = true
			if !e.HeadersComplete || e.Headers.Get("Retry-After") != "9" {
				t.Error("native response headers missing")
			}
			if string(e.Body) != "raw upstream failure" || e.RequestCiphertextHash != "actual-ciphertext" || e.Status != 502 {
				t.Fatalf("wrong native snapshot: %+v", e)
			}
		}
	}
	if !found {
		t.Fatal("missing raw response observation")
	}
}

func TestDiagnosticSealedFailureAndRefusalSnapshots(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		deny      bool
		ownership bool
	}{{"recipient refusal", 422, false, false}, {"authority denial", 0, true, false}, {"ownership refusal", 0, false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			env := newInProcessExchange(t)
			var events []diagnostics.Event
			env.originator.cfg.Diagnostic = func(e diagnostics.Event) bool { events = append(events, e); return true }
			if tc.status != 0 {
				env.payerReturns(LegResult{Status: tc.status, Response: testResponse([]byte("raw refusal"))})
			}
			if tc.deny {
				base := env.originator.cfg.Client.Transport
				env.originator.cfg.Client.Transport = diagnosticRoundTripper(func(r *http.Request) (*http.Response, error) {
					if strings.HasSuffix(r.URL.Path, "/authorize") {
						return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader("denied")), Header: make(http.Header)}, nil
					}
					return base.RoundTrip(r)
				})
			}
			p := testRequest(env.crdReq)
			if tc.ownership {
				var unset relay.Payload
				p = unset
			}
			_, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "crd-order-select", "pci-1", "same", "", Content{WorkstreamType: workstreamPA, Payload: p})
			if err == nil {
				t.Fatal("refusal accepted")
			}
			var sealed, terminal *diagnostics.Event
			for i := range events {
				e := &events[i]
				if e.Kind == "leg.sealed" {
					sealed = e
				}
				if e.Kind == "leg.response" || e.Kind == "leg.failed" || e.Kind == "relay.ownership-refused" {
					terminal = e
				}
			}
			if terminal == nil {
				t.Fatalf("missing refusal diagnostic: %+v", events)
			}
			if !tc.ownership && (sealed == nil || terminal.RequestCiphertextHash != sealed.RequestCiphertextHash) {
				t.Fatalf("lost attempt link: sealed=%+v terminal=%+v", sealed, terminal)
			}
		})
	}
}

type diagnosticRoundTripper func(*http.Request) (*http.Response, error)

func (f diagnosticRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNativeDiagnosticReadFailureIsPartial(t *testing.T) {
	var events []diagnostics.Event
	client := &http.Client{Transport: diagnosticRoundTripper(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 502, Body: &diagnosticPrefixFailure{}, Header: make(http.Header)}, nil
	})}
	n := NewNativeResponder(client, "http://payer", "crd", nil, time.Now, WithNativeDiagnostic(func(e diagnostics.Event) bool { events = append(events, e); return true }))
	_, _, err := n.post(context.Background(), "http://payer", "/x", testRequest([]byte("{}")), "pas-claim", "submit")
	if err == nil {
		t.Fatal("read fault disappeared")
	}
	for _, e := range events {
		if e.Kind == "native.response" {
			if e.BodyComplete || string(e.Body) != "prefix" {
				t.Fatalf("wrong partial evidence: %+v", e)
			}
			return
		}
	}
	t.Fatal("missing partial response")
}

type diagnosticPrefixFailure struct{ read bool }

func (b *diagnosticPrefixFailure) Read(p []byte) (int, error) {
	if b.read {
		return 0, errors.New("torn")
	}
	b.read = true
	return copy(p, "prefix"), errors.New("torn")
}
func (*diagnosticPrefixFailure) Close() error { return nil }

func TestDiagnosticIngressCollectorContract(t *testing.T) {
	now := time.Unix(1700000000, 0)
	key := []byte("synthetic-trace-verification-key")
	var events []diagnostics.Event
	g := &Gateway{cfg: Config{HolderID: "provider-holder", Clock: func() time.Time { return now }, DiagnosticTraceKey: key, Diagnostic: func(e diagnostics.Event) bool { events = append(events, e); return true }}}
	r := httptest.NewRequest("POST", "/Claim/$submit", strings.NewReader(`{"raw":1.00}`))
	r.Header.Set("X-SHN-Test-Trace", diagnostics.TraceProof(key, "call-one", "POST", "/Claim/$submit", now))
	g.observeIngress("pas-ingress", func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write([]byte(`{"response":true}`))
	})(httptest.NewRecorder(), r)
	if len(events) != 2 {
		t.Fatal("request/response observations missing")
	}
	for i, kind := range []string{"provider.ingress.request", "provider.ingress.response"} {
		e := events[i]
		if e.Kind != kind || e.Sender != "provider-holder" || e.CallID != "call-one" || !e.RequestFingerprint.Complete {
			t.Fatalf("collector contract: kind=%s sender=%s call=%s complete=%v", e.Kind, e.Sender, e.CallID, e.RequestFingerprint.Complete)
		}
	}
}

func TestNativeDiagnosticHeaderBudget(t *testing.T) {
	var got diagnostics.Event
	n := NewNativeResponder(nil, "", "", nil, time.Now, WithNativeDiagnostic(func(e diagnostics.Event) bool { got = e; return true }))
	req := httptest.NewRequest("POST", "http://partner/x", nil)
	req.Header.Set("Large", strings.Repeat("x", 65537))
	n.emitDiagnostic(context.Background(), "native.request", []byte("exact"), 0, "", req, req.Header)
	if got.HeadersComplete || len(got.Headers) != 0 || string(got.Body) != "exact" {
		t.Fatal("oversized headers must be explicitly partial without changing body")
	}
	req.Header = make(http.Header)
	for i := 0; i < 4096; i++ {
		req.Header.Set(fmt.Sprintf("X-%d", i), "")
	}
	n.emitDiagnostic(context.Background(), "native.request", nil, 0, "", req, req.Header)
	if got.HeadersComplete {
		t.Fatal("header container allocations exceed budget")
	}

}
func TestDiagnosticVerifiedLegRequiresBoundAuthority(t *testing.T) {
	for _, mutation := range []string{"valid", "ciphertext", "sender", "correlation", "hub"} {
		t.Run(mutation, func(t *testing.T) {
			g, requester := newInboundTestGateway(t, false)
			key := g.cfg.Client.Transport.(*inboundAuthzStub).authzPriv
			var events []diagnostics.Event
			g.cfg.Diagnostic = func(e diagnostics.Event) bool { events = append(events, e); return true }
			env, err := shnsdk.Seal(shnsdk.Metadata{Sender: requester.ID, Recipient: "payer", TransactionType: "crd-order-select", AuthorityFrame: "provider-tpo", CorrelationID: "bound", Timestamp: g.cfg.Clock().Format(time.RFC3339)}, []byte(`{}`), g.cfg.Identity.EncPub)
			if err != nil {
				t.Fatal(err)
			}
			hash := sha256hex(env.Ciphertext)
			tok := signTestToken(shnsdk.Token{Operation: paCatalog[env.Metadata.TransactionType].Op, Frame: "provider-tpo", Holder: requester.ID, CorrelationID: "bound", PayloadHash: hash, Expiry: g.cfg.Clock().Add(time.Minute)}, key)
			raw, _ := json.Marshal(tok)
			env.Metadata.AuthzToken = string(raw)
			switch mutation {
			case "ciphertext":
				env.Ciphertext[0] ^= 1
			case "sender":
				env.Metadata.Sender = "other"
			case "correlation":
				env.Metadata.CorrelationID = "other"
			}
			raw, _ = json.Marshal(env)
			req := httptest.NewRequest("POST", "/substrate/inbound", bytes.NewReader(raw))
			assertion := shnsdk.IssueAssertion("hub", "payer", key, g.cfg.Clock(), time.Minute)
			raw, _ = json.Marshal(assertion)
			if mutation != "hub" {
				req.Header.Set("X-Hub-Assertion", base64.StdEncoding.EncodeToString(raw))
			}
			g.observeInbound(g.handleInbound)(httptest.NewRecorder(), req)
			found := false
			for _, e := range events {
				if e.Kind == "leg.verified" {
					found = true
					if e.RequestCiphertextHash != hash || e.Sender != requester.ID || e.Recipient != "payer" || e.CorrelationID != "bound" {
						t.Fatal("verified identity changed")
					}
				}
			}
			if found != (mutation == "valid") {
				t.Fatalf("verified observation=%v mutation=%s", found, mutation)
			}
		})
	}
}

func TestDiagnosticHubRefusalRetainsActualStatusAndBytes(t *testing.T) {
	env := newInProcessExchange(t)
	raw := []byte(`{"error":"replay detected"}`)
	var events []diagnostics.Event
	env.originator.cfg.Diagnostic = func(e diagnostics.Event) bool { events = append(events, e); return true }
	base := env.originator.cfg.Client.Transport
	env.originator.cfg.Client.Transport = diagnosticRoundTripper(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/route") {
			return &http.Response{StatusCode: 409, Body: io.NopCloser(bytes.NewReader(raw)), Header: make(http.Header)}, nil
		}
		return base.RoundTrip(r)
	})
	_, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "crd-order-select", "pci-1", "same", "", Content{WorkstreamType: workstreamPA, Payload: testRequest(env.crdReq)})
	// The caller sees the Hub's own refusal, not a generic routing failure.
	var refused *hubRefusalError
	if !errors.As(err, &refused) || refused.status != 409 || refused.reason != "replay detected" || refused.delivered != "" {
		t.Fatalf("caller error: %v, want the Hub's 409 replay detected", err)
	}
	hash := ""
	found := false
	for _, e := range events {
		if e.Kind == "leg.sealed" {
			hash = e.RequestCiphertextHash
		}
		if e.Kind == "leg.failed" && e.Status == 409 && e.BodyComplete && bytes.Equal(e.Body, raw) {
			found = true
			if e.RequestCiphertextHash == "" || e.RequestCiphertextHash != hash {
				t.Fatal("actual refusal lost sealed attempt")
			}
		}
	}
	if !found {
		t.Fatal("actual Hub response lost behind caller-facing error")
	}
}
