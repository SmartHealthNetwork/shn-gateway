package observer

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"github.com/SmartHealthNetwork/shn-gateway/connectors/scaffold"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine"
)

func TestInspectionRetentionBoundsAndClose(t *testing.T) {
	h := NewHub()
	for i := 0; i < bufSize+5; i++ {
		h.Emit(engine.ObserverEvent{Kind: "leg.response", Payload: []byte(`{"resourceType":"Patient"}`)})
	}
	if len(h.buf) != bufSize || h.retained <= 0 || h.retained > maxRetainedBytes {
		t.Fatal("unbounded ring", len(h.buf), h.retained)
	}
	s, ok := h.subscribe(0)
	if !ok {
		t.Fatal("subscription refused")
	}
	h.Close()
	if h.retained != 0 {
		t.Fatal("close retained ring/replay/channel bytes", h.retained)
	}
	s.close()
	h.Close()
	if h.retained != 0 {
		t.Fatal("duplicate release")
	}
}
func TestInspectionSubscriberBoundAndLoss(t *testing.T) {
	h := NewHub()
	defer h.Close()
	for i := 0; i < maxSubscribers; i++ {
		if _, ok := h.subscribe(0); !ok {
			t.Fatal("early rejection")
		}
	}
	if _, ok := h.subscribe(0); ok {
		t.Fatal("unbounded subscribers")
	}
	if h.dropped != 1 {
		t.Fatal("missing admission loss")
	}
	for i := 0; i < subDepth+1; i++ {
		h.Emit(engine.ObserverEvent{Kind: "leg.response"})
	}
	if h.dropped != 1+maxSubscribers {
		t.Fatal("slow subscriber loss", h.dropped)
	}
}
func TestInspectionSerializationExpansionDropsAndMalformedPositive(t *testing.T) {
	h := NewHub()
	defer h.Close()
	h.Emit(engine.ObserverEvent{Kind: "ingress.received", Payload: []byte("malformed\x00<")})
	if h.seq != 1 || !strings.Contains(string(h.buf[0].data), "payload was not valid JSON") {
		t.Fatal("malformed inspection lost")
	}
	h.Emit(engine.ObserverEvent{Kind: "ingress.received", Payload: []byte(strings.Repeat("\x00", 8<<20))})
	if h.dropped != 1 || h.seq != 1 {
		t.Fatal("expansion admission bypass")
	}
}

func TestInspectionActualEngineSharedPressure(t *testing.T) {
	h := NewHub()
	defer h.Close()
	id, err := shnsdk.GenerateIdentity("observer-synthetic")
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	clients := map[string]engine.IngressClientRegistration{"synthetic": {Alg: "ES384", PublicKeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})}}
	g, err := engine.New(engine.Config{Role: "provider", HolderID: "observer-synthetic", Identity: id, SoR: scaffold.New(), Store: engine.NewMemStore(), Observer: h.Emit, ConformanceEnforcement: engine.EnforcementNone, IngressEnabled: true, IngressBaseURL: "http://observer.test", IngressClients: clients})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	request := func() {
		w := httptest.NewRecorder()
		g.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/Claim/$submit", strings.NewReader("{malformed")))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("native refusal changed %d %s", w.Code, w.Body.String())
		}
	}
	request()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := g.WaitObserverCompletion(ctx); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	owner := h.owner
	held := h.retained
	useful := false
	for _, b := range h.buf {
		useful = useful || bytes.Contains(b.data, []byte("{malformed"))
	}
	h.mu.Unlock()
	if owner == nil || held == 0 || !useful {
		t.Fatal("actual engine owner missing")
	}
	pressure, ok := owner.Reserve(maxRetainedBytes - held)
	if !ok {
		t.Fatal("unexpected budget ownership")
	}
	request()
	if err := g.WaitObserverCompletion(ctx); err == nil {
		t.Fatal("loss claimed completed inspection")
	}
	rows, drops := g.ConformanceObservationsForTest()
	if len(rows) != 0 || drops != 0 {
		t.Fatal("none inspection counted as conformance")
	}
	h.Close()
	pressure.Release()
	pressure.Release()
	all, ok := owner.Reserve(maxRetainedBytes)
	if !ok {
		t.Fatal("shared ownership leaked")
	}
	all.Release()
}

type heldInspectionWriter struct {
	header           http.Header
	entered, release chan struct{}
}

func (w *heldInspectionWriter) Header() http.Header { return w.header }
func (w *heldInspectionWriter) WriteHeader(int)     {}
func (w *heldInspectionWriter) Flush()              {}
func (w *heldInspectionWriter) Write(b []byte) (int, error) {
	if bytes.Contains(b, []byte(`"payload"`)) {
		close(w.entered)
		<-w.release
		return 0, io.ErrClosedPipe
	}
	return len(b), nil
}
func TestInspectionSlowWriterOwnsUntilReturn(t *testing.T) {
	h := NewHub()
	h.Emit(engine.ObserverEvent{Kind: "leg.response", Payload: []byte(`{"opaque":"bytes"}`)})
	w := &heldInspectionWriter{header: make(http.Header), entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() { defer close(done); h.handleEvents(w, httptest.NewRequest("GET", "/events", nil)) }()
	<-w.entered
	h.Close()
	h.mu.Lock()
	held := h.retained
	h.mu.Unlock()
	if held == 0 {
		t.Fatal("blocked writer memory released early")
	}
	close(w.release)
	<-done
	if h.retained != 0 || len(h.subs) != 0 {
		t.Fatal("writer error leaked ownership")
	}
}
func TestInspectionLossFailsBarrier(t *testing.T) {
	h := NewHub()
	defer h.Close()
	h.Emit(engine.ObserverEvent{Payload: []byte(strings.Repeat("x", 8<<20))})
	w := httptest.NewRecorder()
	h.HandlerWithBarrier(func(context.Context) error { return nil }).ServeHTTP(w, httptest.NewRequest("POST", "/barrier", nil))
	if w.Code != 503 {
		t.Fatal("loss returned successful barrier", w.Code)
	}
}

func TestInspectionMalformedSerializationMatchesLegacy(t *testing.T) {
	for _, payload := range [][]byte{[]byte("bad\n\t\b\f\r"), {0xff}, []byte("\x00<&>\u2028\u2029"), []byte("valid utf8: \ufffd"), []byte(`{"valid":"<value>"}`)} {
		e := engine.ObserverEvent{Kind: "ingress.received", Payload: payload, Detail: "existing detail"}
		h := NewHub()
		h.Emit(e)
		if !json.Valid(payload) {
			e.Payload, _ = json.Marshal(string(payload))
			e.Detail += "; payload was not valid JSON (delivered as string)"
		}
		want, err := json.Marshal(sequenced{Seq: 1, ObserverEvent: e})
		if err != nil {
			t.Fatal(err)
		}
		if len(h.buf) != 1 || !bytes.Equal(h.buf[0].data, want) {
			t.Errorf("payload=%q got=%s want=%s", payload, h.buf[0].data, want)
		}
		h.Close()
	}
}
