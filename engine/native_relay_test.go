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
	"sync/atomic"
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
	up, lr, err := n.post(context.Background(), srv.URL, "/x", testRequest([]byte("{}")), "pas-claim", "PAS submit")
	if err != nil {
		t.Fatalf("upstream 502 must not be an error return, got %v", err)
	}
	if string(up.raw) != oo {
		t.Fatalf("the upstream reply must carry the non-2xx body as read, got %s", up.raw)
	}
	if !lr.ResponseRelayed() {
		t.Fatalf("a relayed non-2xx body must be relayed, got %v", lr.Response)
	}
	if lr.Status != http.StatusBadGateway {
		t.Fatalf("lr.Status = %d, want 502", lr.Status)
	}
	if string(responseBytes(lr)) != oo {
		t.Fatalf("responseBytes(lr) not the upstream body:\n got %s\nwant %s", responseBytes(lr), oo)
	}
}

func TestNativePost_Unreachable_IsErrorReturn(t *testing.T) {
	n := &nativeResponder{client: &http.Client{}}
	// 127.0.0.1:1 refuses; a build/dial fault must be an error return, not a relayable LegResult.
	_, lr, err := n.post(context.Background(), "http://127.0.0.1:1", "/x", testRequest([]byte("{}")), "pas-claim", "PAS submit")
	if err == nil {
		t.Fatal("unreachable upstream must be an error return")
	}
	if lr.Status != 0 {
		t.Fatalf("unreachable must not carry a relayable Status, got %d", lr.Status)
	}
}

// TestNativePost_OverCapNon2xxBody_DegradesToError covers the relayBodyCap headroom check
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
	up, lr, err := n.post(context.Background(), srv.URL, "/x", testRequest([]byte("{}")), "pas-claim", "PAS submit")
	if err == nil {
		t.Fatal("over-cap non-2xx body must be an error return, not a relayable LegResult")
	}
	if up.raw != nil {
		t.Fatalf("expected nil body on over-cap fault, got %d bytes", len(up.raw))
	}
	if lr.Status != 0 {
		t.Fatalf("over-cap fault must not carry a relayable Status, got %d", lr.Status)
	}
}

// TestNativeQuestionnaireFetchRefusesRepeatedMembers: the payer's gateway
// reads the network's questionnaire request by exact member names before its
// own system sees anything; a repeated or case-folded member is refused 400.
func TestNativeQuestionnaireFetchRefusesRepeatedMembers(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`))
	}))
	defer srv.Close()
	n := &nativeResponder{client: srv.Client(), baseURL: srv.URL}
	for _, row := range []struct{ name, body string }{
		{"control", `{"canonical":"http://example.org/q"}`},
		{"duplicate", `{"canonical":"http://example.org/q","canonical":"http://example.org/other"}`},
		{"case-folded duplicate", `{"canonical":"http://example.org/q","Canonical":"http://example.org/other"}`},
		{"case-mismatched", `{"Canonical":"http://example.org/q"}`},
	} {
		t.Run(row.name, func(t *testing.T) {
			before := calls.Load()
			res, err := n.Handle(context.Background(), "dtr-questionnaire-fetch", "corr-1", "pci-1", []byte(row.body))
			if err != nil {
				t.Fatal(err)
			}
			if row.name == "control" {
				if res.Status != 0 || calls.Load() != before+1 {
					t.Fatalf("control: status %d, calls %d", res.Status, calls.Load()-before)
				}
				return
			}
			if res.Status != http.StatusBadRequest || res.Message != "parse questionnaire fetch failed" || calls.Load() != before {
				t.Fatalf("status %d %q, calls %d", res.Status, res.Message, calls.Load()-before)
			}
		})
	}
}
