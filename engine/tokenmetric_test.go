package engine

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// unreadable is an answer whose body fails mid-read: the connection was cut
// after the status line.
type unreadable struct{}

func (unreadable) Read([]byte) (int, error) { return 0, tokenConnReset() }
func (unreadable) Close() error             { return nil }

// tokenConnRefused and tokenConnReset are a dial to a stopped listener and a
// connection reset by the peer.
func tokenConnRefused() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}
}

func tokenConnReset() error {
	return &net.OpError{Op: "read", Net: "tcp", Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}}
}

// tokenFault fails an /authorize call with err and no response, after
// reporting the request written when written is set, as net/http's
// transport does through the request's client trace.
func tokenFault(err error, written bool) func(*http.Request) (*http.Response, error) {
	return func(req *http.Request) (*http.Response, error) {
		if tr := httptrace.ContextClientTrace(req.Context()); written && tr != nil && tr.WroteRequest != nil {
			tr.WroteRequest(httptrace.WroteRequestInfo{})
		}
		return nil, err
	}
}

// tokenAnswer answers an /authorize call with status and body.
func tokenAnswer(status int, body string) func(*http.Request) (*http.Response, error) {
	return func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
	}
}

// signTestToken returns a signed copy of tok using authzPriv. Replicates the
// tokenSigningPayload logic (marshal with Signature=nil) since the public
// SDK's tokenSigningPayload is unexported. json.Marshal of a well-formed
// shnsdk.Token never errors, so the return value is always valid.
func signTestToken(tok shnsdk.Token, authzPriv ed25519.PrivateKey) shnsdk.Token {
	c := tok
	c.Signature = nil
	payload, _ := json.Marshal(c) // Token fields are all JSON-safe; never errors
	tok.Signature = ed25519.Sign(authzPriv, payload)
	return tok
}

// MON-R26: each Authorization Framework token acquisition reports one
// outcome, decided by status and error class, and its duration end to end,
// the one pre-write retry included. A call whose request could not be built
// never reached the wire and reports nothing.
func TestTokenMetric_OneObservationPerAcquisition_MON_R26(t *testing.T) {
	id, err := shnsdk.GenerateIdentity("h-token")
	if err != nil {
		t.Fatal(err)
	}
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	tok := signTestToken(shnsdk.Token{Frame: "frame", Operation: "op", Subject: "pci", CorrelationID: "corr", PayloadHash: "h",
		Holder: "h-token", Expiry: time.Date(2026, 10, 25, 13, 0, 0, 0, time.UTC)}, key)
	answer, err := json.Marshal(authorizeResp{Token: &tok})
	if err != nil {
		t.Fatal(err)
	}
	signed := string(answer)
	for _, row := range []struct {
		name    string
		answers []func(*http.Request) (*http.Response, error)
		want    string
		took    time.Duration
	}{
		{"a token", []func(*http.Request) (*http.Response, error){tokenAnswer(http.StatusOK, signed)}, TokenOK, time.Second},
		{"refused before the write, then a token", []func(*http.Request) (*http.Response, error){tokenFault(tokenConnRefused(), false), tokenAnswer(http.StatusOK, signed)}, TokenOK, 2 * time.Second},
		{"assertion refused", []func(*http.Request) (*http.Response, error){tokenAnswer(http.StatusUnauthorized, `{"error":"assertion"}`)}, TokenDenied, time.Second},
		{"request denied", []func(*http.Request) (*http.Response, error){tokenAnswer(http.StatusForbidden, ``)}, TokenDenied, time.Second},
		{"refused before the write, twice", []func(*http.Request) (*http.Response, error){tokenFault(tokenConnRefused(), false), tokenFault(tokenConnRefused(), false)}, TokenUnavailable, 2 * time.Second},
		{"reset after the write", []func(*http.Request) (*http.Response, error){tokenFault(tokenConnReset(), true)}, TokenUnavailable, time.Second},
		{"answer cut off", []func(*http.Request) (*http.Response, error){func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: unreadable{}}, nil
		}}, TokenUnavailable, time.Second},
		{"throttled", []func(*http.Request) (*http.Response, error){tokenAnswer(http.StatusTooManyRequests, ``)}, TokenUnavailable, time.Second},
		{"its own dependency down", []func(*http.Request) (*http.Response, error){tokenAnswer(http.StatusBadGateway, `{"error":"consent check failed"}`)}, TokenUnavailable, time.Second},
		{"its decision not recorded", []func(*http.Request) (*http.Response, error){tokenAnswer(http.StatusBadGateway, `{"error":"decision audit failed","rule":"audit"}`)}, TokenUnavailable, time.Second},
		{"a proxy's 503", []func(*http.Request) (*http.Response, error){tokenAnswer(http.StatusServiceUnavailable, ``)}, TokenUnavailable, time.Second},
		{"a proxy's 504", []func(*http.Request) (*http.Response, error){tokenAnswer(http.StatusGatewayTimeout, ``)}, TokenUnavailable, time.Second},
		{"bad request", []func(*http.Request) (*http.Response, error){tokenAnswer(http.StatusBadRequest, `{"error":"bad request"}`)}, TokenFailed, time.Second},
		{"too large", []func(*http.Request) (*http.Response, error){tokenAnswer(http.StatusRequestEntityTooLarge, ``)}, TokenFailed, time.Second},
		{"policy evaluation failed", []func(*http.Request) (*http.Response, error){tokenAnswer(http.StatusInternalServerError, ``)}, TokenFailed, time.Second},
		{"an unreadable success", []func(*http.Request) (*http.Response, error){tokenAnswer(http.StatusOK, `not json`)}, TokenFailed, time.Second},
	} {
		t.Run(row.name, func(t *testing.T) {
			now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
			calls := 0
			client := &http.Client{Transport: certRoundTrip(func(r *http.Request) (*http.Response, error) {
				now = now.Add(time.Second) // each attempt takes a second on the clock
				f := row.answers[min(calls, len(row.answers)-1)]
				calls++
				return f(r)
			})}
			var got []string
			var took []time.Duration
			g := &Gateway{cfg: Config{HolderID: "h-token", Identity: id, Clock: func() time.Time { return now },
				AuthzURL: "https://authz.example", AuthzClient: client, AuthzPub: key.Public().(ed25519.PublicKey),
				TokenMetric: func(outcome string, d time.Duration) { got, took = append(got, outcome), append(took, d) }}}
			g.authorizeRequest(httptest.NewRequest(http.MethodPost, "/", nil), authorizeReq{Frame: "frame", Operation: "op", SubjectPCI: "pci", CorrelationID: "corr", PayloadHash: "h"})
			if len(got) != 1 || got[0] != row.want || took[0] != row.took {
				t.Fatalf("observations %v %v after %d attempts, want one %s taking %v", got, took, calls, row.want, row.took)
			}
		})
	}

	// A request that could not be built never reached the wire.
	var got []string
	g := &Gateway{cfg: Config{HolderID: "h-token", Identity: id, Clock: time.Now, AuthzURL: "http://[::1",
		AuthzClient: &http.Client{Transport: certRoundTrip(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("sent")
		})},
		TokenMetric: func(outcome string, _ time.Duration) { got = append(got, outcome) }}}
	if _, err := g.authorizeRequest(httptest.NewRequest(http.MethodPost, "/", nil), authorizeReq{Frame: "frame", Operation: "op", SubjectPCI: "pci", CorrelationID: "corr"}); err == nil || len(got) != 0 {
		t.Fatalf("an unbuildable request: err %v, observations %v; want an error and none", err, got)
	}
}
