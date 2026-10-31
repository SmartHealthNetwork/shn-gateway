//go:build !shn_coreonly

// native_relay_test.go — TestNativePost_* cover the relay-vs-fault split in
// nativeResponder.post/get (relay-recipient-response): an upstream that PRODUCED
// an HTTP response (any status) is a relayable LegResult; a no-response fault
// (build/dial/read) or an over-cap non-2xx body is an error return.
package engine

import (
	"bytes"
	"context"
	"errors"
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

// TestNativePost_AnswerOverTheReadLimitIsRefusedForSize: an upstream answer,
// 2xx or not, over what the gateway reads is NOT a relayable LegResult and is
// never kept cut short: it is an error return the system answered
// (errAnswerTooLarge), with no body. Whether a smaller answer fits the sealed
// answer is decided when it is sealed (buildResponseLeg).
func TestNativePost_AnswerOverTheReadLimitIsRefusedForSize(t *testing.T) {
	oversized := bytes.Repeat([]byte("a"), maxPartnerBody+1)
	for _, status := range []int{http.StatusOK, http.StatusBadGateway} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write(oversized)
		}))
		n := &nativeResponder{client: srv.Client()}
		up, lr, err := n.post(context.Background(), srv.URL, "/x", testRequest([]byte("{}")), "pas-claim", "PAS submit")
		srv.Close()
		var uf *upstreamFailure
		if !errors.Is(err, errAnswerTooLarge) || !errors.As(err, &uf) || !uf.sent {
			t.Fatalf("%d: err %v, want the system's answer refused for size", status, err)
		}
		if up.raw != nil || lr.Status != 0 {
			t.Fatalf("%d: kept %d bytes, status %d; want nothing relayable", status, len(up.raw), lr.Status)
		}
	}
}
