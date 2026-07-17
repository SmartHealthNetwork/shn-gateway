// native_relay_test.go — TestNativePost_* cover the relay-vs-fault split in
// nativeResponder.post/get (relay-recipient-response): an upstream that PRODUCED
// an HTTP response (any status) is a relayable LegResult; a no-response fault
// (build/dial/read) or an over-cap non-2xx body is an error return.
package engine

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNativePost_UpstreamNon2xx_RelaysStatusAndBody(t *testing.T) {
	oo := `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing","diagnostics":"Failure to submit prior auth"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(oo))
	}))
	defer srv.Close()
	n := &nativeResponder{client: srv.Client()}
	body, lr, err := n.post(context.Background(), srv.URL, "/x", []byte("{}"), "PAS submit")
	if err != nil {
		t.Fatalf("upstream 502 must not be an error return, got %v", err)
	}
	if body != nil {
		t.Fatalf("expected nil body on non-2xx, got %s", body)
	}
	if lr.Status != http.StatusBadGateway {
		t.Fatalf("lr.Status = %d, want 502", lr.Status)
	}
	if string(lr.ResponseFHIR) != oo {
		t.Fatalf("lr.ResponseFHIR not the upstream body:\n got %s\nwant %s", lr.ResponseFHIR, oo)
	}
}

func TestNativePost_Unreachable_IsErrorReturn(t *testing.T) {
	n := &nativeResponder{client: &http.Client{}}
	// 127.0.0.1:1 refuses; a build/dial fault must be an error return, not a relayable LegResult.
	_, lr, err := n.post(context.Background(), "http://127.0.0.1:1", "/x", []byte("{}"), "PAS submit")
	if err == nil {
		t.Fatal("unreachable upstream must be an error return")
	}
	if lr.Status != 0 {
		t.Fatalf("unreachable must not carry a relayable Status, got %d", lr.Status)
	}
}

// TestNativePost_OverCapNon2xxBody_DegradesToError covers the relayBodyCap headroom check —
// an upstream non-2xx body too large to relay is NOT a relayable LegResult — it
// degrades to a no-response-shaped fault (error return), same as build/dial/read.
func TestNativePost_OverCapNon2xxBody_DegradesToError(t *testing.T) {
	oversized := bytes.Repeat([]byte("a"), relayBodyCap+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write(oversized)
	}))
	defer srv.Close()
	n := &nativeResponder{client: srv.Client()}
	body, lr, err := n.post(context.Background(), srv.URL, "/x", []byte("{}"), "PAS submit")
	if err == nil {
		t.Fatal("over-cap non-2xx body must be an error return, not a relayable LegResult")
	}
	if body != nil {
		t.Fatalf("expected nil body on over-cap fault, got %d bytes", len(body))
	}
	if lr.Status != 0 {
		t.Fatalf("over-cap fault must not carry a relayable Status, got %d", lr.Status)
	}
}
