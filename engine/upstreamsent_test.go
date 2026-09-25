package engine

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
)

// A native forward that got no usable answer tells the requester whether the
// payer's system saw the request: never reached (a closed port) is not the
// same as received and then lost (the connection dropped after the request was
// read, or the answer was cut short) — the latter may have been acted on.
func TestResponderFailure_SaysWhetherThePayerSawTheRequest(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := closed.URL
	closed.Close()
	rows := map[string]struct {
		url  func(t *testing.T) string
		want string
	}{
		"never reached": {func(*testing.T) string { return closedURL }, errUpstreamNotReached},
		"dropped after the request was read": {func(t *testing.T) string {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.ReadAll(r.Body)
				if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
					_ = conn.Close()
				}
			}))
			t.Cleanup(srv.Close)
			return srv.URL
		}, errUpstreamNoUsableAnswer},
		"answer cut short": {func(t *testing.T) string {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Length", "1000")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"resourceType":`))
				w.(http.Flusher).Flush()
				if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
					_ = conn.Close()
				}
			}))
			t.Cleanup(srv.Close)
			return srv.URL
		}, errUpstreamNoUsableAnswer},
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			n := NewNativeResponder(&http.Client{}, row.url(t), "shn-order-select", newCensusSoR(), fixedClock)
			responderFailureIs(t, n, row.want)
		})
	}
}

// A payer behind SMART Backend Services: a token that cannot be obtained, or a
// token obtained and then a payer that cannot be dialled, never reached the
// payer's system — the token request is the gateway's own, not the payer's.
func TestResponderFailure_TokenRequestIsNotThePayerRequest(t *testing.T) {
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer refusing.Close()
	issuing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"t","token_type":"bearer","expires_in":300}`))
	}))
	defer issuing.Close()
	var payerHits atomic.Int32
	payer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { payerHits.Add(1) }))
	payerURL := payer.URL
	payer.Close()
	for name, tokenURL := range map[string]string{"token refused": refusing.URL, "token issued, payer not dialled": issuing.URL} {
		t.Run(name, func(t *testing.T) {
			client, err := smartauth.NewHTTPClient(smartauth.Config{TokenURL: tokenURL, ClientID: "gw", ClientSecret: "synthetic-secret"})
			if err != nil {
				t.Fatal(err)
			}
			responderFailureIs(t, NewNativeResponder(client, payerURL, "shn-order-select", newCensusSoR(), fixedClock), errUpstreamNotReached)
		})
	}
	if payerHits.Load() != 0 {
		t.Fatal("the payer was reached")
	}
}

func responderFailureIs(t *testing.T, n *nativeResponder, want string) {
	t.Helper()
	_, err := n.Handle(context.Background(), "pas-claim", "corr-sent", "PCI-1", originatorBuiltConformantBundle(t, "MBR-COVERED"))
	if err == nil {
		t.Fatal("want a forward failure")
	}
	status, msg := (&Gateway{}).responderFailure("pas-claim", err)
	if status != http.StatusBadGateway || msg != want {
		t.Fatalf("got %d %q, want 502 %q", status, msg, want)
	}
}
