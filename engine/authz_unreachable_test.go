package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The Authorization Framework not answering at all is the network being
// unavailable, not an authority verdict: the provider's route answers
// its own 503 and nothing crosses to the Hub. A denial and an erroring or
// malformed authz answer keep their answers, and only a failure before the
// request was written is retried, once, with a fresh assertion.

// dialRefused is the error a dial to a stopped listener returns.
func dialRefused() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}
}

// connReset is a connection reset by the peer.
func connReset() error {
	return &net.OpError{Op: "read", Net: "tcp", Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}}
}

// transportFault fails an /authorize call with err and no response. written
// says whether the request was written first: the fake reports it through the
// request's client trace, as net/http's transport does.
func transportFault(err error, written bool) func(*http.Request) (*http.Response, error) {
	return func(req *http.Request) (*http.Response, error) {
		if written {
			if tr := httptrace.ContextClientTrace(req.Context()); tr != nil && tr.WroteRequest != nil {
				tr.WroteRequest(httptrace.WroteRequestInfo{})
			}
		}
		return nil, err
	}
}

// answer answers an /authorize call with status and body.
func answer(status int, body string) func(*http.Request) (*http.Response, error) {
	return func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)),
			Header: http.Header{"Content-Type": []string{"application/json"}}}, nil
	}
}

// authzRow drives one CRD through the provider's ingress route with the
// substrate's /authorize answering faults in order, and returns the answer
// and the one exchange record.
func authzRow(t *testing.T, faults ...func(*http.Request) (*http.Response, error)) (*inProcessExchange, *httptest.ResponseRecorder, ExchangeRecord) {
	t.Helper()
	env := newInProcessExchange(t)
	var got exchangeRecords
	env.originator.cfg.ExchangeObserved = got.observe
	env.payerReturns(LegResult{})
	env.substrate.authorizeFaults = faults
	rec := httptest.NewRecorder()
	env.originator.ingressRoute(RouteCRD)(rec, env.crdIngressRequest(t))
	return env, rec, got.only(t)
}

func TestAuthzUnreachable_Answers503NothingSent(t *testing.T) {
	for name, fault := range map[string]func(*http.Request) (*http.Response, error){
		"refused, not written":   transportFault(dialRefused(), false),
		"reset, not written":     transportFault(connReset(), false),
		"EOF, not written":       transportFault(io.EOF, false),
		"EOF after the write":    transportFault(io.EOF, true),
		"reset after the write":  transportFault(connReset(), true),
		"some other no-response": transportFault(errors.New("tls: handshake failure"), false),
		// The Framework never answers these itself: a proxy in front of it had
		// no Framework to reach.
		"a proxy's 503": answer(http.StatusServiceUnavailable, "no healthy upstream"),
		"a proxy's 504": answer(http.StatusGatewayTimeout, "upstream request timeout"),
	} {
		t.Run(name, func(t *testing.T) {
			// Every attempt fails the same way.
			env, rec, r := authzRow(t, fault, fault)
			if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "the authorization service could not be reached") {
				t.Fatalf("answer %d %s, want 503 naming the authorization service", rec.Code, rec.Body.String())
			}
			if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
				t.Fatalf("Cache-Control %q, want no-store", cc)
			}
			if n := env.routeHitCount(); n != 0 {
				t.Fatalf("the request crossed to the Hub (%d) without a token", n)
			}
			if r.Outcome != ExchangeUnreachable || r.RefusedBy != "" || r.Rule != "" || r.Status != http.StatusServiceUnavailable {
				t.Fatalf("record = %+v, want unreachable with no refusing party", r)
			}
		})
	}
}

// Only a failure before the request was written is retried: authz answered
// nothing, so it recorded no decision. After the write it may have decided,
// and its decision record carries no correlation id a reconciliation could
// attribute a second one by, so that is not retried.
func TestAuthzUnreachable_RetriesOnlyBeforeTheWrite(t *testing.T) {
	for _, row := range []struct {
		name  string
		fault func(*http.Request) (*http.Response, error)
		hits  int
	}{
		{"refused, not written", transportFault(dialRefused(), false), 2},
		{"reset, not written", transportFault(connReset(), false), 2},
		{"EOF, not written", transportFault(io.EOF, false), 2},
		{"EOF after the write", transportFault(io.EOF, true), 1},
		{"reset after the write", transportFault(connReset(), true), 1},
		{"another error, not written", transportFault(errors.New("tls: handshake failure"), false), 1},
		{"a proxy's 503", answer(http.StatusServiceUnavailable, "no healthy upstream"), 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			env, rec, r := authzRow(t, row.fault) // the second attempt, if any, succeeds
			if got := env.substrate.authorizeHits; got != row.hits {
				t.Fatalf("authorize calls %d, want %d", got, row.hits)
			}
			if row.hits == 2 {
				if rec.Code != http.StatusOK || r.Outcome != ExchangeAnswered {
					t.Fatalf("answer %d %s, record %+v: want the retried leg answered", rec.Code, rec.Body.String(), r)
				}
				return
			}
			if rec.Code != http.StatusServiceUnavailable || r.Outcome != ExchangeUnreachable {
				t.Fatalf("answer %d, record %+v: want 503 unreachable, not retried", rec.Code, r)
			}
		})
	}
}

// Authz consumes each holder assertion's jti once: a retry of the same
// assertion would be refused as a replay. The retry mints a fresh one.
func TestAuthzUnreachable_RetryCarriesAFreshAssertion(t *testing.T) {
	var jtis []string
	capture := func(next func(*http.Request) (*http.Response, error)) func(*http.Request) (*http.Response, error) {
		return func(req *http.Request) (*http.Response, error) {
			raw, err := base64.StdEncoding.DecodeString(req.Header.Get("X-Holder-Assertion"))
			if err != nil {
				t.Fatalf("assertion header: %v", err)
			}
			var a shnsdk.Assertion
			if err := json.Unmarshal(raw, &a); err != nil || a.JTI == "" {
				t.Fatalf("assertion %s: %v", raw, err)
			}
			jtis = append(jtis, a.JTI)
			if next == nil {
				return nil, nil // fall through to the ordinary token
			}
			return next(req)
		}
	}
	_, rec, _ := authzRow(t, capture(transportFault(dialRefused(), false)), capture(nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
	}
	if len(jtis) != 2 || jtis[0] == jtis[1] {
		t.Fatalf("assertion jtis %v, want two distinct", jtis)
	}
}

// An answer from authz is never retried and keeps what it answers today: a
// denial is the 403 it always was, an erroring or malformed answer the opaque
// 502.
func TestAuthzAnswered_NotRetriedAnswersAsBefore(t *testing.T) {
	for _, row := range []struct {
		name    string
		fault   func(*http.Request) (*http.Response, error)
		status  int
		msg     string
		outcome string
		by      string
		rule    string
	}{
		{"denial", answer(http.StatusForbidden, `{"error":"forbidden"}`), http.StatusForbidden, "authorization denied",
			ExchangeRefused, RefusedByAuthorizationFramework, RefusalAuthority},
		{"authz 500", answer(http.StatusInternalServerError, `{"error":"policy error"}`), http.StatusBadGateway, "authorization failed", ExchangeOther, "", ""},
		{"authz 502", answer(http.StatusBadGateway, `{"error":"decision audit failed"}`), http.StatusBadGateway, "authorization failed", ExchangeOther, "", ""},
		{"malformed 200", answer(http.StatusOK, `not json`), http.StatusBadGateway, "authorization failed", ExchangeOther, "", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			env, rec, r := authzRow(t, row.fault, row.fault)
			if rec.Code != row.status || !strings.Contains(rec.Body.String(), row.msg) {
				t.Fatalf("answer %d %s, want %d %q", rec.Code, rec.Body.String(), row.status, row.msg)
			}
			if got := env.substrate.authorizeHits; got != 1 {
				t.Fatalf("authorize calls %d, want 1: an answer is never retried", got)
			}
			if env.routeHitCount() != 0 {
				t.Fatal("the request crossed to the Hub")
			}
			if r.Outcome != row.outcome || r.RefusedBy != row.by || r.Rule != row.rule {
				t.Fatalf("record = %+v, want %s/%s/%s", r, row.outcome, row.by, row.rule)
			}
		})
	}
}

// A caller whose own request has ended gets no retry.
func TestAuthzUnreachable_NoRetryAfterTheCallerLeft(t *testing.T) {
	env := newInProcessExchange(t)
	env.substrate.authorizeFaults = []func(*http.Request) (*http.Response, error){transportFault(dialRefused(), false)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(nil)).WithContext(ctx)
	_, err := env.originator.authorizeRequest(req, authorizeReq{Frame: "f", Operation: "o", SubjectPCI: "pci:x", CorrelationID: "c"})
	if !errors.Is(err, errAuthzUnreachable) {
		t.Fatalf("err %v, want errAuthzUnreachable", err)
	}
	if got := env.substrate.authorizeHits; got != 1 {
		t.Fatalf("authorize calls %d, want 1", got)
	}
}

// The leg metric counts an unreachable authz as unreachable, not failed.
func TestAuthzUnreachable_LegMetric(t *testing.T) {
	env := newInProcessExchange(t)
	var got []string
	env.originator.cfg.LegMetric = func(outcome string) { got = append(got, outcome) }
	env.payerReturns(LegResult{})
	f := transportFault(dialRefused(), false)
	env.substrate.authorizeFaults = []func(*http.Request) (*http.Response, error){f, f}
	env.originator.ingressRoute(RouteCRD)(httptest.NewRecorder(), env.crdIngressRequest(t))
	if len(got) != 2 || got[0] != LegOutcomeRouted || got[1] != LegOutcomeUnreachable {
		t.Fatalf("outcomes %v, want [routed unreachable]", got)
	}
}

// The FHIR operation routes answer the same 503 as an OperationOutcome
// (transient), through the same chokepoint.
func TestAuthzUnreachable_FHIROperationRoutes(t *testing.T) {
	for _, row := range []struct {
		route string
		req   func(t *testing.T) *http.Request
		sor   bool // the DTR route: a system of record, and the payer's framed operations
	}{
		{RoutePAS, func(*testing.T) *http.Request {
			return httptest.NewRequest(http.MethodPost, "/Claim/$submit", strings.NewReader(pasIngressBundle("00001", "")))
		}, false},
		{RoutePASInquire, func(t *testing.T) *http.Request {
			return httptest.NewRequest(http.MethodPost, "/Claim/$inquire", strings.NewReader(levelInquiry(t, "MBR-COVERED", "")))
		}, false},
		{RouteDTR, func(t *testing.T) *http.Request {
			req := httptest.NewRequest(http.MethodPost, "/Questionnaire/$questionnaire-package", bytes.NewReader(dtrFixture(t, "dtr-package-params-2.0.json")))
			req.Header.Set("Content-Type", "application/fhir+json")
			return req
		}, true},
	} {
		t.Run(row.route, func(t *testing.T) {
			env := newInProcessExchange(t)
			if row.sor {
				env.originator.cfg.SoR = newPrefetchSoR().sor()
				declareFramedDTR(t, env, true)
			}
			var got exchangeRecords
			env.originator.cfg.ExchangeObserved = got.observe
			env.payerReturns(LegResult{})
			f := transportFault(dialRefused(), false)
			env.substrate.authorizeFaults = []func(*http.Request) (*http.Response, error){f, f}
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(row.route)(rec, row.req(t))
			var oo struct {
				ResourceType string `json:"resourceType"`
				Issue        []struct {
					Code        string `json:"code"`
					Diagnostics string `json:"diagnostics"`
				} `json:"issue"`
			}
			if rec.Code != http.StatusServiceUnavailable || json.Unmarshal(rec.Body.Bytes(), &oo) != nil || oo.ResourceType != "OperationOutcome" ||
				len(oo.Issue) != 1 || oo.Issue[0].Code != "transient" || !strings.Contains(oo.Issue[0].Diagnostics, "the authorization service could not be reached") {
				t.Fatalf("answer %d %s, want a 503 transient OperationOutcome", rec.Code, rec.Body.String())
			}
			if env.routeHitCount() != 0 {
				t.Fatal("the request crossed to the Hub")
			}
			if r := got.only(t); r.Outcome != ExchangeUnreachable || r.RefusedBy != "" {
				t.Fatalf("record %+v, want unreachable", r)
			}
		})
	}
}

// realAuthz points the gateway's authorize call at addr through a real
// net/http Transport that counts its dials, with timeout as the client's.
func realAuthz(t *testing.T, addr string, timeout time.Duration) (*inProcessExchange, *atomic.Int32) {
	t.Helper()
	env := newInProcessExchange(t)
	var dials atomic.Int32
	tr := &http.Transport{DialContext: func(ctx context.Context, network, a string) (net.Conn, error) {
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, network, a)
	}}
	t.Cleanup(tr.CloseIdleConnections)
	env.originator.cfg.Client = &http.Client{Transport: tr, Timeout: timeout}
	env.originator.cfg.AuthzURL = "http://" + addr
	return env, &dials
}

// authzListener accepts connections on loopback and hands each to serve,
// counting them, and closes each connection when serve returns; the listener
// is closed at the end of the test.
func authzListener(t *testing.T, serve func(net.Conn)) (string, *atomic.Int32) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var conns atomic.Int32
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			go func() {
				defer c.Close()
				serve(c)
			}()
		}
	}()
	return l.Addr().String(), &conns
}

// readRequest reads one HTTP request off c.
func readRequest(c net.Conn) {
	_, _ = http.ReadRequest(bufio.NewReader(c))
}

// Through a real Transport: a refused dial is retried once; a connection
// closed after the request was read is not; nor is a Framework that answers
// nothing within the client's timeout.
func TestAuthzUnreachable_RealTransport(t *testing.T) {
	authorize := func(env *inProcessExchange) error {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		_, err := env.originator.authorizeRequest(req, authorizeReq{Frame: "f", Operation: "o", SubjectPCI: "pci:x", CorrelationID: "c"})
		return err
	}
	t.Run("refused: retried once", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		l.Close() // nothing listens: every dial is refused
		env, dials := realAuthz(t, addr, 5*time.Second)
		if err := authorize(env); !errors.Is(err, errAuthzUnreachable) {
			t.Fatalf("err %v, want errAuthzUnreachable", err)
		}
		if n := dials.Load(); n != 2 {
			t.Fatalf("dials %d, want 2 (one retry)", n)
		}
	})
	t.Run("closed after the request was read: not retried", func(t *testing.T) {
		addr, conns := authzListener(t, readRequest)
		env, _ := realAuthz(t, addr, 5*time.Second)
		if err := authorize(env); !errors.Is(err, errAuthzUnreachable) {
			t.Fatalf("err %v, want errAuthzUnreachable", err)
		}
		if n := conns.Load(); n != 1 {
			t.Fatalf("connections %d, want 1 (no retry after the write)", n)
		}
	})
	t.Run("no answer within the client timeout: not retried", func(t *testing.T) {
		hold := make(chan struct{})
		t.Cleanup(func() { close(hold) })
		addr, conns := authzListener(t, func(c net.Conn) { readRequest(c); <-hold })
		env, _ := realAuthz(t, addr, 200*time.Millisecond)
		if err := authorize(env); !errors.Is(err, errAuthzUnreachable) {
			t.Fatalf("err %v, want errAuthzUnreachable", err)
		}
		if n := conns.Load(); n != 1 {
			t.Fatalf("connections %d, want 1", n)
		}
	})
}

// An involved patient's token that cannot be obtained leaves that patient out
// of the list (the leg itself goes on), after the same single retry.
func TestAuthzUnreachable_InvolvedTokenLeftOut(t *testing.T) {
	env := newInProcessExchange(t)
	f := transportFault(dialRefused(), false)
	env.substrate.authorizeFaults = []func(*http.Request) (*http.Response, error){f, f}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	if _, why := env.originator.mintInvolved(req, "f", "o", "c", "h", involvedPatient{pci: "pci:x", involvement: "other"}); why != involvedOmitFailed {
		t.Fatalf("left out as %q, want %q", why, involvedOmitFailed)
	}
	if got := env.substrate.authorizeHits; got != 2 {
		t.Fatalf("authorize calls %d, want 2", got)
	}
}
