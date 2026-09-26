package engine

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
)

// refusedURL is an address no server answers: port 0 is never held by a
// listener, so the dial fails at once on every OS and nothing is written. A
// server started and then closed is not a substitute, because the freed port
// can be handed to the next listener.
const refusedURL = "http://127.0.0.1:0"

// unreachableClient fails every request at the transport, the way a refused
// connection does. At cleanup it requires that the transport was reached, and
// only with the expected method and path, so a row cannot pass through a
// different branch.
func unreachableClient(t *testing.T, method, path string) *http.Client {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		if len(seen) == 0 {
			t.Error("transport was never reached: the failure did not come from the unreachable branch")
		}
		for _, s := range seen {
			if s != method+" "+path {
				t.Errorf("request = %s, want %s %s", s, method, path)
			}
		}
	})
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		return nil, errors.New("connect: connection refused")
	})}
}

// A native forward that got no usable answer tells the requester whether the
// payer's system saw the request: never reached (the dial failed) is not the
// same as received and then lost (the connection dropped after the request was
// read, or the answer was cut short) — the latter may have been acted on.
func TestResponderFailure_SaysWhetherThePayerSawTheRequest(t *testing.T) {
	rows := map[string]struct {
		url  func(t *testing.T) string
		want string
	}{
		"never reached": {func(*testing.T) string { return refusedURL }, errUpstreamNotReached},
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
	var refused, issued atomic.Int32
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refused.Add(1)
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer refusing.Close()
	issuing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		issued.Add(1)
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"t","token_type":"bearer","expires_in":300}`))
	}))
	defer issuing.Close()
	// A live payer that counts: the refused-token row must never reach it.
	var payerHits atomic.Int32
	payer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payerHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer payer.Close()
	for _, row := range []struct {
		name, tokenURL, payerURL string
		refused, issued          int32
	}{
		{"token refused", refusing.URL, payer.URL, 1, 0},
		{"token issued, payer unreachable", issuing.URL, refusedURL, 0, 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			refused.Store(0)
			issued.Store(0)
			payerHits.Store(0)
			client, err := smartauth.NewHTTPClient(smartauth.Config{TokenURL: row.tokenURL, ClientID: "gw", ClientSecret: "synthetic-secret"})
			if err != nil {
				t.Fatal(err)
			}
			responderFailureIs(t, NewNativeResponder(client, row.payerURL, "shn-order-select", newCensusSoR(), fixedClock), errUpstreamNotReached)
			// Each row's failure comes from its own step: the refused token
			// stops before the payer; the issued token goes on to the payer
			// dial, which fails.
			if refused.Load() != row.refused || issued.Load() != row.issued {
				t.Fatalf("token requests refused=%d issued=%d, want refused=%d issued=%d", refused.Load(), issued.Load(), row.refused, row.issued)
			}
			if n := payerHits.Load(); n != 0 {
				t.Fatalf("payer reached %d times, want none", n)
			}
		})
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
