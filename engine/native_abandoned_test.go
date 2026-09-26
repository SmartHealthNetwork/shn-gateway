package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var abandonedAfter = regexp.MustCompile(`upstream payer CRD call abandoned after \d+\.\ds: `)

// capturedLog collects the process log for the duration of a row.
func capturedLog(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return buf.Write(p) }))
	t.Cleanup(func() { log.SetOutput(old) })
	return func() string { mu.Lock(); defer mu.Unlock(); return buf.String() }
}

// When the requester stops waiting (its hub-leg budget ran out) while the
// payer's system is still answering, the payer gateway logs one line of its
// own: elapsed time, upstream host, leg, correlation and whether the request
// was written. Never the body, headers, path or query.
func TestNativePostLogsWhenTheRequesterStopsWaiting(t *testing.T) {
	logs := capturedLog(t)
	held := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-held }))
	defer srv.Close()
	defer close(held)
	n := &nativeResponder{client: srv.Client()}
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), responderCorrelationKey{}, "corr-1121"), 200*time.Millisecond)
	defer cancel()
	_, _, err := n.post(ctx, srv.URL, "/cds-services/order-select?token=querysecret", testRequest([]byte(`{"secret":"bodysecret"}`)), "crd-order-select", "CRD")
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("the error the engine maps is unchanged: %v", err)
	}
	out := logs()
	if strings.Count(out, "call abandoned") != 1 {
		t.Fatalf("want exactly one abandoned line, got:\n%s", out)
	}
	host := strings.TrimPrefix(srv.URL, "http://")
	host = host[:strings.LastIndex(host, ":")]
	if !abandonedAfter.MatchString(out) {
		t.Errorf("abandoned line lacks the elapsed time:\n%s", out)
	}
	for _, want := range []string{"the request it serves ended (context deadline exceeded)", "host " + host + ",", "leg crd-order-select", "correlation corr-1121", "request written: yes"} {
		if !strings.Contains(out, want) {
			t.Errorf("abandoned line lacks %q:\n%s", want, out)
		}
	}
	for _, leak := range []string{"bodysecret", "querysecret", "order-select?", "Content-Type"} {
		if strings.Contains(out, leak) {
			t.Errorf("abandoned line leaked %q:\n%s", leak, out)
		}
	}
}

// Rejection rows: an answer in time, and an unreachable upstream while the
// requester is still waiting, write no abandoned line.
func TestNativePostWritesNoAbandonedLineOtherwise(t *testing.T) {
	logs := capturedLog(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cards":[]}`))
	}))
	defer srv.Close()
	n := &nativeResponder{client: srv.Client()}
	if _, _, err := n.post(context.Background(), srv.URL, "/x", testRequest([]byte("{}")), "crd-order-select", "CRD"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (&nativeResponder{client: &http.Client{}}).post(context.Background(), "http://127.0.0.1:1", "/x", testRequest([]byte("{}")), "crd-order-select", "CRD"); err == nil {
		t.Fatal("an unreachable upstream must fail")
	}
	if out := logs(); strings.Contains(out, "call abandoned") {
		t.Fatalf("an abandoned line without a stopped requester:\n%s", out)
	}
}

// Through Handle: the correlation id the network carried reaches the line.
func TestNativeHandleNamesTheCorrelationWhenAbandoned(t *testing.T) {
	logs := capturedLog(t)
	held := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/cds-services" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"services": referencePayerServices})
			return
		}
		<-held
	}))
	defer srv.Close()
	defer close(held)
	n := NewNativeResponder(srv.Client(), srv.URL, "", nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := n.Handle(ctx, "crd-order-select", "corr-handle", "pci", cdsRequest("order-select")); err == nil {
		t.Fatal("an abandoned upstream call must fail the leg")
	}
	if out := logs(); !strings.Contains(out, "correlation corr-handle") || !strings.Contains(out, "leg crd-order-select") {
		t.Fatalf("the abandoned line must name the carried correlation and leg:\n%s", out)
	}
}

// The answer's read is abandoned too: the payer's system sent its headers and
// then held the body when the request ended. The request was written.
func TestNativePostLogsWhenTheAnswerReadIsAbandoned(t *testing.T) {
	logs := capturedLog(t)
	held := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cards":[`))
		w.(http.Flusher).Flush()
		<-held
	}))
	defer srv.Close()
	defer close(held)
	n := &nativeResponder{client: srv.Client()}
	ctx, cancel := context.WithTimeout(withResponderCorrelation(context.Background(), "corr-read"), 200*time.Millisecond)
	defer cancel()
	_, _, err := n.post(ctx, srv.URL, "/cds-services/order-select", testRequest([]byte("{}")), "crd-order-select", "CRD")
	if err == nil || !strings.Contains(err.Error(), "read failed") {
		t.Fatalf("want the read failure, got %v", err)
	}
	out := logs()
	if strings.Count(out, "call abandoned") != 1 || !strings.Contains(out, "correlation corr-read") || !strings.Contains(out, "request written: yes") {
		t.Fatalf("want one abandoned line for the read, written:\n%s", out)
	}
}

// A request that ended before this gateway wrote anything says so.
func TestNativePostLogsAnUnwrittenAbandonedCall(t *testing.T) {
	logs := capturedLog(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	n := &nativeResponder{client: srv.Client()}
	ctx, cancel := context.WithCancel(withResponderCorrelation(context.Background(), "corr-unwritten"))
	cancel()
	if _, _, err := n.post(ctx, srv.URL, "/x", testRequest([]byte("{}")), "crd-order-select", "CRD"); err == nil {
		t.Fatal("a request that already ended must fail")
	}
	out := logs()
	if strings.Count(out, "call abandoned") != 1 || !strings.Contains(out, "(context canceled)") || !strings.Contains(out, "request written: no") {
		t.Fatalf("want one abandoned line, not written:\n%s", out)
	}
}

// correlationForwarder is a declared eligibility endpoint that records the
// correlation id its call carries.
type correlationForwarder struct {
	*nativeResponder
	seen *string
}

func (c correlationForwarder) forwardEligibility(ctx context.Context, _ []byte) (LegResult, error) {
	*c.seen, _ = ctx.Value(responderCorrelationKey{}).(string)
	return LegResult{Status: http.StatusServiceUnavailable, Message: "held"}, nil
}

// The eligibility forward does not pass through Handle; its call carries the
// network's correlation id all the same, so an abandoned line can name it.
func TestEligibilityForwardCarriesTheCorrelation(t *testing.T) {
	p := eligibilityPayer(t, EnforcementNone, true)
	var seen string
	p.g.cfg.Responder = correlationForwarder{nativeResponder: p.g.cfg.Responder.(*nativeResponder), seen: &seen}
	p.send(t, "coverage-eligibility", "", eligibilityRequest(t, dtrFrameMember))
	if !strings.HasPrefix(seen, "corr-coverage-eligibility-") {
		t.Fatalf("the eligibility forward carried correlation %q, want the envelope's", seen)
	}
}
