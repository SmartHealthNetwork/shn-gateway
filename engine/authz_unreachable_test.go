//go:build !shn_coreonly

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
			if r.Outcome != ExchangeRefused || r.RefusedBy != RefusedByProviderGateway || r.Rule != RefusalAuthority || r.AuthorizeError != "unreachable" || r.Status != http.StatusServiceUnavailable {
				t.Fatalf("record = %+v, want refused on authority, the Framework unreachable", r)
			}
		})
	}
}

// Only a failure before the request was written is retried: authz answered
// nothing, so it recorded no decision. After the write it may have decided,
// and its decision record carries no correlation id a reconciliation could
// attribute a second one by, so that is not retried.
func TestAuthzUnreachable_RetriesOnlyBeforeTheWrite(t *testing.T) {
	t.Parallel()
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
			if rec.Code != http.StatusServiceUnavailable || r.Outcome != ExchangeRefused || r.Rule != RefusalAuthority {
				t.Fatalf("answer %d, record %+v: want 503, refused on authority, not retried", rec.Code, r)
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

// An answer from authz is never retried: a denial is the 403 it always was, an
// erroring or malformed answer a 502 naming its class, recorded as this
// gateway's refusal on authority: no authority could be obtained. A decision
// the Framework could not record is its refusal on the audit rule, and is
// recorded so; the caller is told the decision could not be recorded.
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
		{"authz 500", answer(http.StatusInternalServerError, `{"error":"policy error"}`), http.StatusBadGateway, wantAuthzFailed("500"), ExchangeRefused, RefusedByProviderGateway, RefusalAuthority},
		{"authz 502", answer(http.StatusBadGateway, `{"error":"decision audit failed","rule":"audit"}`), http.StatusBadGateway, wantAuthzNotRecorded,
			ExchangeRefused, RefusedByAuthorizationFramework, RefusalAudit},
		{"malformed 200", answer(http.StatusOK, `not json`), http.StatusBadGateway, wantAuthzNoToken, ExchangeRefused, RefusedByProviderGateway, RefusalAuthority},
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

// The Framework states the audit rule by structure, in its answer's "rule",
// and only on its 502: the record reads that field, never the error text. A
// 502 with no rule, an unknown one, a differently cased key or an unreadable
// body, and a 500 that names one, are no stated rule: no authority could be
// obtained, this gateway's refusal on authority, as for an authz that never
// answered. The caller is told the decision could not be recorded where the
// rule is stated, the Framework's status otherwise, never the rule's name.
func TestAuthzAuditRefusal_RecordedOnlyFromTheStatedRule(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name    string
		fault   func(*http.Request) (*http.Response, error)
		status  int
		msg     string
		outcome string
		by      string
		rule    string
	}{
		{"the audit rule stated", answer(http.StatusBadGateway, `{"error":"decision audit failed","rule":"audit"}`),
			http.StatusBadGateway, wantAuthzNotRecorded, ExchangeRefused, RefusedByAuthorizationFramework, RefusalAudit},
		{"the same text, no rule", answer(http.StatusBadGateway, `{"error":"decision audit failed"}`),
			http.StatusBadGateway, wantAuthzFailed("502"), ExchangeRefused, RefusedByProviderGateway, RefusalAuthority},
		{"a rule the gateway does not know", answer(http.StatusBadGateway, `{"error":"decision audit failed","rule":"ledger"}`),
			http.StatusBadGateway, wantAuthzFailed("502"), ExchangeRefused, RefusedByProviderGateway, RefusalAuthority},
		{"a network rule the Framework does not state here", answer(http.StatusBadGateway, `{"error":"consent check failed","rule":"consent"}`),
			http.StatusBadGateway, wantAuthzFailed("502"), ExchangeRefused, RefusedByProviderGateway, RefusalAuthority},
		{"a rule that is not a string", answer(http.StatusBadGateway, `{"error":"decision audit failed","rule":1}`),
			http.StatusBadGateway, wantAuthzFailed("502"), ExchangeRefused, RefusedByProviderGateway, RefusalAuthority},
		{"the rule under another key's case", answer(http.StatusBadGateway, `{"error":"decision audit failed","Rule":"audit"}`),
			http.StatusBadGateway, wantAuthzFailed("502"), ExchangeRefused, RefusedByProviderGateway, RefusalAuthority},
		{"a body that is not JSON", answer(http.StatusBadGateway, `rule: audit`),
			http.StatusBadGateway, wantAuthzFailed("502"), ExchangeRefused, RefusedByProviderGateway, RefusalAuthority},
		{"the rule on a 500", answer(http.StatusInternalServerError, `{"error":"decision audit failed","rule":"audit"}`),
			http.StatusBadGateway, wantAuthzFailed("500"), ExchangeRefused, RefusedByProviderGateway, RefusalAuthority},
		{"authz unreachable", transportFault(errors.New("tls: handshake failure"), false),
			http.StatusServiceUnavailable, "could not be reached", ExchangeRefused, RefusedByProviderGateway, RefusalAuthority},
		{"the rule on a proxy's 503", answer(http.StatusServiceUnavailable, `{"rule":"audit"}`),
			http.StatusServiceUnavailable, "could not be reached", ExchangeRefused, RefusedByProviderGateway, RefusalAuthority},
	} {
		t.Run(row.name, func(t *testing.T) {
			env, rec, r := authzRow(t, row.fault, row.fault)
			if rec.Code != row.status || !strings.Contains(rec.Body.String(), row.msg) {
				t.Fatalf("answer %d %s, want %d %q", rec.Code, rec.Body.String(), row.status, row.msg)
			}
			if strings.Contains(rec.Body.String(), "audit") {
				t.Fatalf("the caller's answer names the rule: %s", rec.Body.String())
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

// The leg metric counts an unreachable authz as unreachable, not failed: the
// leg's outcome reads the error through the refusal the record makes of it.
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
		{RouteEligibility, func(t *testing.T) *http.Request {
			return eligibilityIngressPost(eligibilityRequest(t, "MBR-COVERED"))
		}, false},
		{RouteFederatedQuery, func(t *testing.T) *http.Request {
			return fqIngressPost(fqTask(t, "MBR-UC05", "facility"))
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
			if row.route == RouteFederatedQuery { // its Task names a facility
				env = newFQExchange(t, EnforcementObserve)
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
			if r := got.only(t); r.Outcome != ExchangeRefused || r.RefusedBy != RefusedByProviderGateway || r.Rule != RefusalAuthority || r.AuthorizeError != "unreachable" {
				t.Fatalf("record %+v, want refused on authority, the Framework unreachable", r)
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

// Unknown is not empty: an authorize answer that carries no token, a null one
// or one the Framework did not sign is no authority, whatever its status. A
// request leg's token must also verify against the Framework's key, bound to
// exactly that leg, before it is sealed in. Either way the leg is refused at
// the requester on authority, a network rule, at every conformance level:
// answered with a 502 that its answer carried no token the leg can use, and it
// never reaches the Hub.
func TestAuthzToken_RequiredAndVerifiedBeforeTheLegIsSent(t *testing.T) {
	t.Parallel()
	_, strangerKey := genED25519(t)
	// minted answers the token the env's fake Framework would mint for the
	// request, with mutate applied before signing: by the Framework's own key,
	// or by another.
	minted := func(stranger bool, mutate func(*shnsdk.Token)) func(*inProcessExchange) func(*http.Request) (*http.Response, error) {
		return func(env *inProcessExchange) func(*http.Request) (*http.Response, error) {
			return func(r *http.Request) (*http.Response, error) {
				var req authorizeReq
				b, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(b, &req)
				tok := shnsdk.Token{Operation: req.Operation, Scope: "crd-context", Subject: req.SubjectPCI, Frame: req.Frame,
					Holder: "provider", CorrelationID: req.CorrelationID, Expiry: env.substrate.clock().Add(time.Hour), PayloadHash: req.PayloadHash}
				mutate(&tok)
				key := env.substrate.authzPriv
				if stranger {
					key = strangerKey
				}
				tok = signTestToken(tok, key)
				body, _ := json.Marshal(map[string]any{"token": tok})
				return answer(http.StatusOK, string(body))(r)
			}
		}
	}
	fixed := func(body string) func(*inProcessExchange) func(*http.Request) (*http.Response, error) {
		return func(*inProcessExchange) func(*http.Request) (*http.Response, error) {
			return answer(http.StatusOK, body)
		}
	}
	var outcomes []string
	run := func(t *testing.T, fault func(*inProcessExchange) func(*http.Request) (*http.Response, error)) (*inProcessExchange, *httptest.ResponseRecorder, ExchangeRecord) {
		t.Helper()
		env := newInProcessExchange(t)
		var got exchangeRecords
		env.originator.cfg.ExchangeObserved = got.observe
		outcomes = nil
		env.originator.cfg.TokenMetric = func(outcome string, _ time.Duration) { outcomes = append(outcomes, outcome) }
		env.payerReturns(LegResult{})
		env.substrate.authorizeFaults = []func(*http.Request) (*http.Response, error){fault(env)}
		rec := httptest.NewRecorder()
		env.originator.ingressRoute(RouteCRD)(rec, env.crdIngressRequest(t))
		return env, rec, got.only(t)
	}
	for _, row := range []struct {
		name  string
		fault func(*inProcessExchange) func(*http.Request) (*http.Response, error)
	}{
		{"no token", fixed(`{}`)},
		{"a null token", fixed(`{"token":null}`)},
		{"an unsigned token", fixed(`{"token":{"operation":"order-select","holder":"provider"}}`)},
		{"another field and no token", fixed(`{"rule":"audit"}`)},
		{"signed by another key", minted(true, func(*shnsdk.Token) {})},
		{"for another leg", minted(false, func(k *shnsdk.Token) { k.CorrelationID = "another-leg" })},
		{"for another payload", minted(false, func(k *shnsdk.Token) { k.PayloadHash = strings.Repeat("0", 64) })},
		{"for another patient", minted(false, func(k *shnsdk.Token) { k.Subject = "pci:another" })},
		{"for another holder", minted(false, func(k *shnsdk.Token) { k.Holder = "payer" })},
		{"for another operation", minted(false, func(k *shnsdk.Token) { k.Operation = "pas-claim" })},
		{"for another frame", minted(false, func(k *shnsdk.Token) { k.Frame = "payer-coverage" })},
		{"expired", minted(false, func(k *shnsdk.Token) { k.Expiry = k.Expiry.Add(-2 * time.Hour) })},
		{"for an involved patient", minted(false, func(k *shnsdk.Token) { k.Involvement = shnsdk.InvolvementRequestNamed })},
	} {
		t.Run(row.name, func(t *testing.T) {
			env, rec, r := run(t, row.fault)
			var body struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusBadGateway || body.Error != wantAuthzNoToken {
				t.Fatalf("answer %d %s, want 502 %q", rec.Code, rec.Body.String(), wantAuthzNoToken)
			}
			if env.routeHitCount() != 0 {
				t.Fatal("the leg was sent to the Hub")
			}
			if r.Outcome != ExchangeRefused || r.RefusedBy != RefusedByProviderGateway || r.Rule != RefusalAuthority || r.AuthorizeError != ExchangeOther {
				t.Fatalf("record = %+v, want refused by the provider gateway on authority", r)
			}
			if len(outcomes) != 1 || outcomes[0] != TokenFailed {
				t.Fatalf("token outcomes %v, want one %s", outcomes, TokenFailed)
			}
		})
	}
	// The Framework's own token for this leg is sent: the check refuses only
	// what fails it, so each row above fails for its own mutation.
	t.Run("the Framework's token for this leg", func(t *testing.T) {
		env, rec, r := run(t, minted(false, func(*shnsdk.Token) {}))
		if rec.Code != http.StatusOK || env.routeHitCount() != 1 || r.Outcome != ExchangeAnswered {
			t.Fatalf("answer %d, hub calls %d, record %+v: the leg's own token must be sent", rec.Code, env.routeHitCount(), r)
		}
		if len(outcomes) != 1 || outcomes[0] != TokenOK {
			t.Fatalf("token outcomes %v, want one %s", outcomes, TokenOK)
		}
	})
}

// The token outcome of an authorize answer without a usable token is failed,
// on every caller.
func TestTokenMetric_AnswerWithoutATokenFails(t *testing.T) {
	id, err := shnsdk.GenerateIdentity("h-token")
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"no token":          `{}`,
		"a null token":      `{"token":null}`,
		"an unsigned token": `{"token":{"operation":"op"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			client := &http.Client{Transport: certRoundTrip(tokenAnswer(http.StatusOK, body))}
			var got []string
			g := &Gateway{cfg: Config{HolderID: "h-token", Identity: id, Clock: time.Now,
				AuthzURL: "https://authz.example", AuthzClient: client,
				TokenMetric: func(outcome string, _ time.Duration) { got = append(got, outcome) }}}
			if _, err := g.authorizeRequest(httptest.NewRequest(http.MethodPost, "/", nil), authorizeReq{Frame: "frame", Operation: "op", SubjectPCI: "pci", CorrelationID: "corr", PayloadHash: "h"}); err == nil {
				t.Fatal("an answer without a token authorized the call")
			}
			if len(got) != 1 || got[0] != TokenFailed {
				t.Fatalf("token outcomes %v, want one %s", got, TokenFailed)
			}
		})
	}
}

// An involved patient whose authorize answer carries no token, or one with no
// signature, is left out as failed, not as a token minted without its label.
func TestInvolvedToken_AnswerWithoutATokenIsLeftOutFailed(t *testing.T) {
	for name, body := range map[string]string{
		"no token":          `{}`,
		"an unsigned token": `{"token":{"involvement":"request-named"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			env := newInProcessExchange(t)
			env.substrate.authorizeFaults = []func(*http.Request) (*http.Response, error){answer(http.StatusOK, body)}
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			if _, why := env.originator.mintInvolved(req, "f", "o", "c", "h", involvedPatient{pci: "pci:x", involvement: shnsdk.InvolvementRequestNamed}); why != involvedOmitFailed {
				t.Fatalf("left out as %q, want %q", why, involvedOmitFailed)
			}
		})
	}
}

// A payer's answer token must verify before the answer is sealed in: signed
// by the Framework's key and bound to exactly this answer (its frame,
// operation, correlation, the payer as holder, the leg's patient and the
// answer's payload hash). A network rule on per-leg authority and message
// integrity, so it refuses at every conformance level: the answer is not
// sent, the payer's gateway answers the forward with its attested 502
// (delivered yes, rule authority: its system answered), its record says it
// refused on authority after a usable backend answer, and the token outcome
// is failed. The Framework's own token
// for the answer is sent.
func TestAnswerToken_VerifiedBeforeTheAnswerIsSent(t *testing.T) {
	t.Parallel()
	_, strangerKey := genED25519(t)
	// minted answers the token a Framework would mint for the payer's
	// authorize request, with mutate applied before signing: by the
	// Framework's own key, or by another.
	minted := func(p *levelPayer, stranger bool, mutate func(*shnsdk.Token)) func(*http.Request) (*http.Response, error) {
		return func(r *http.Request) (*http.Response, error) {
			var req authorizeReq
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &req)
			tok := shnsdk.Token{Operation: req.Operation, Scope: "crd-context", Subject: req.SubjectPCI, Frame: req.Frame,
				Holder: p.g.cfg.HolderID, CorrelationID: req.CorrelationID, Expiry: p.g.cfg.Clock().Add(time.Hour), PayloadHash: req.PayloadHash}
			mutate(&tok)
			key := p.authzPriv
			if stranger {
				key = strangerKey
			}
			body, _ := json.Marshal(map[string]any{"token": signTestToken(tok, key)})
			resp, err := tokenAnswer(http.StatusOK, string(body))(r)
			resp.Request = r
			return resp, err
		}
	}
	run := func(t *testing.T, level ConformanceEnforcement, stranger bool, mutate func(*shnsdk.Token)) (payerAnswer, ExchangeRecord, []string) {
		t.Helper()
		p, got := newRecordingLevelPayer(t, level)
		var outcomes []string
		p.g.cfg.TokenMetric = func(outcome string, _ time.Duration) { outcomes = append(outcomes, outcome) }
		p.g.cfg.Client = &http.Client{Transport: stagedTransport{inner: p.g.cfg.Client.Transport, before: minted(p, stranger, mutate)}}
		ans := p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
		return ans, got.only(t), outcomes
	}
	refused := func(t *testing.T, ans payerAnswer, rec ExchangeRecord, outcomes []string) {
		t.Helper()
		// The payer gateway attests its system answered and names the rule
		// (attestAnswerRefused), so the requester learns both.
		if ans.status != http.StatusBadGateway || ans.framed || strings.TrimSpace(string(ans.body)) != `{"error":"answer token does not verify","rule":"authority"}` ||
			ans.header.Get(HubDeliveredHeader) != "yes" {
			t.Fatalf("answer %d %s (framed %v, delivered %q), want the attested 502 on authority, delivered yes", ans.status, ans.body, ans.framed, ans.header.Get(HubDeliveredHeader))
		}
		if rec.AnswerError != ExchangeOther || rec.Backend == nil {
			t.Fatalf("answer error %q, backend %+v; want other after a usable backend answer", rec.AnswerError, rec.Backend)
		}
		if rec.Outcome != ExchangeRefused || rec.RefusedBy != RefusedByPayerGateway || rec.Rule != RefusalAuthority {
			t.Fatalf("record %s by %q rule %q, want refused by the payer gateway on authority", rec.Outcome, rec.RefusedBy, rec.Rule)
		}
		if len(outcomes) != 1 || outcomes[0] != TokenFailed {
			t.Fatalf("token outcomes %v, want one %s", outcomes, TokenFailed)
		}
	}
	for _, row := range []struct {
		name     string
		stranger bool
		mutate   func(*shnsdk.Token)
	}{
		{"signed by another key", true, func(*shnsdk.Token) {}},
		{"for another leg", false, func(k *shnsdk.Token) { k.CorrelationID = "another-leg" }},
		{"for another payload", false, func(k *shnsdk.Token) { k.PayloadHash = strings.Repeat("0", 64) }},
		{"with no payload", false, func(k *shnsdk.Token) { k.PayloadHash = "" }},
		{"for another patient", false, func(k *shnsdk.Token) { k.Subject = "pci:another" }},
		{"for another holder", false, func(k *shnsdk.Token) { k.Holder = "requester" }},
		{"for another operation", false, func(k *shnsdk.Token) { k.Operation = "pas-claim-response" }},
		{"for another frame", false, func(k *shnsdk.Token) { k.Frame = "provider-tpo" }},
		{"expired", false, func(k *shnsdk.Token) { k.Expiry = k.Expiry.Add(-2 * time.Hour) }},
		{"for an involved patient", false, func(k *shnsdk.Token) { k.Involvement = shnsdk.InvolvementPayerHeld }},
	} {
		t.Run(row.name, func(t *testing.T) {
			ans, rec, outcomes := run(t, EnforcementObserve, row.stranger, row.mutate)
			refused(t, ans, rec, outcomes)
		})
	}
	// A network rule refuses at every conformance level, and the Framework's
	// own token for the answer is sent at each: each refusal above is its own
	// mutation's.
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict} {
		t.Run("at "+level.String(), func(t *testing.T) {
			t.Run("for another payload", func(t *testing.T) {
				ans, rec, outcomes := run(t, level, false, func(k *shnsdk.Token) { k.PayloadHash = strings.Repeat("0", 64) })
				refused(t, ans, rec, outcomes)
			})
			t.Run("the Framework's token for this answer", func(t *testing.T) {
				ans, rec, outcomes := run(t, level, false, func(*shnsdk.Token) {})
				if ans.status != http.StatusOK || !ans.framed || rec.AnswerError != "" || rec.Outcome != ExchangeAnswered {
					t.Fatalf("answer %d %s (framed %v), record %+v: the answer's own token must be sent", ans.status, ans.body, ans.framed, rec)
				}
				if len(outcomes) != 1 || outcomes[0] != TokenOK {
					t.Fatalf("token outcomes %v, want one %s", outcomes, TokenOK)
				}
			})
		})
	}
}

// Once the payer's system has answered, every failure to authorize its
// answer is attested: the payer gateway answers the Hub's forward with a raw
// 502 marked X-SHN-Delivered: yes, so the requester learns its system
// answered (it may have acted) and does not resend blindly. An answer with
// no authority is refused on authority whatever the Framework did: denied
// it, answered with no usable token, or could not be reached or failed (the
// record's answer error class says which); the Framework's audit refusal is
// audit. At every conformance level.
func TestAnswerAuthorization_AttestedOnceTheSystemAnswered(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name        string
		answer      func(*http.Request) (*http.Response, error)
		reason      string
		rule        string
		by          string
		answerError string
	}{
		{"denied", tokenAnswer(http.StatusForbidden, `{"error":"denied"}`), "answer not authorized", RefusalAuthority, RefusedByAuthorizationFramework, BackendAuth},
		{"no token", tokenAnswer(http.StatusOK, `{}`), "answer authorization carries no token", RefusalAuthority, RefusedByPayerGateway, ExchangeOther},
		{"a null token", tokenAnswer(http.StatusOK, `{"token":null}`), "answer authorization carries no token", RefusalAuthority, RefusedByPayerGateway, ExchangeOther},
		{"an unsigned token", tokenAnswer(http.StatusOK, `{"token":{"operation":"crd-cards","holder":"payer"}}`), "answer authorization carries no token", RefusalAuthority, RefusedByPayerGateway, ExchangeOther},
		{"an unreadable answer", tokenAnswer(http.StatusOK, `{"token":`), "answer authorization carries no token", RefusalAuthority, RefusedByPayerGateway, ExchangeOther},
		{"the Framework's audit refusal", tokenAnswer(http.StatusBadGateway, `{"error":"decision audit failed","rule":"audit"}`), "answer authorization not recorded", RefusalAudit, RefusedByAuthorizationFramework, ExchangeOther},
		{"the Framework erroring", tokenAnswer(http.StatusInternalServerError, `{"error":"boom"}`), "answer authorization unavailable", RefusalAuthority, RefusedByPayerGateway, ExchangeOther},
		{"the Framework unreachable", tokenFault(tokenConnRefused(), false), "answer authorization unavailable", RefusalAuthority, RefusedByPayerGateway, "unreachable"},
	} {
		for _, level := range allLevels {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				p, got := newRecordingLevelPayer(t, level)
				p.g.cfg.Client = &http.Client{Transport: stagedTransport{inner: p.g.cfg.Client.Transport, before: func(r *http.Request) (*http.Response, error) {
					resp, err := row.answer(r)
					if resp != nil {
						resp.Request = r
					}
					return resp, err
				}}}
				ans := p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
				var body map[string]string
				if ans.status != http.StatusBadGateway || ans.framed || json.Unmarshal(ans.body, &body) != nil || ans.header.Get(HubDeliveredHeader) != "yes" {
					t.Fatalf("answer %d %s (framed %v, delivered %q), want the attested 502, delivered yes", ans.status, ans.body, ans.framed, ans.header.Get(HubDeliveredHeader))
				}
				if body["error"] != row.reason || body["rule"] != row.rule {
					t.Fatalf("attested %q on %q, want %q on %q", body["error"], body["rule"], row.reason, row.rule)
				}
				rec := got.only(t)
				if rec.Backend == nil || rec.Backend.Status != http.StatusOK || rec.AnswerError != closed(AnswerErrorClasses, row.answerError) {
					t.Fatalf("backend %+v answer error %q, want the system's 200 then answer error %q", rec.Backend, rec.AnswerError, row.answerError)
				}
				if rec.Outcome != ExchangeRefused || rec.RefusedBy != row.by || rec.Rule != row.rule {
					t.Fatalf("record %s by %q rule %q, want refused by %s on %s", rec.Outcome, rec.RefusedBy, rec.Rule, row.by, row.rule)
				}
			})
		}
	}
}

// failingAnswerAuthorize answers every authorize call the gateway makes from
// now on with a 200 and no token: the answer cannot be authorized.
func failingAnswerAuthorize(p *levelPayer) {
	p.g.cfg.Client = &http.Client{Transport: stagedTransport{inner: p.g.cfg.Client.Transport, before: func(r *http.Request) (*http.Response, error) {
		resp, err := tokenAnswer(http.StatusOK, `{}`)(r)
		resp.Request = r
		return resp, err
	}}}
}

// wantNotAttested holds an answer to the plain 502 authorization failed, with
// no X-SHN-Delivered: yes: nothing may claim the system answered.
func wantNotAttested(t *testing.T, ans payerAnswer) {
	t.Helper()
	if ans.status != http.StatusBadGateway || strings.TrimSpace(string(ans.body)) != `{"error":"authorization failed"}` || ans.header.Get(HubDeliveredHeader) == "yes" {
		t.Fatalf("answer %d %s (delivered %q), want the plain 502 authorization failed, not attested", ans.status, ans.body, ans.header.Get(HubDeliveredHeader))
	}
}

// Only the participant's system answering the forwarded operation makes a
// failed authorize attested delivered: anything short of that would tell the
// requester the recipient answered, and may have acted, when it was asked
// nothing. At every conformance level, each through a recording gateway, since
// the record is what decides.
func TestAnswerAuthorization_NotAttestedBeforeTheSystemAnswered(t *testing.T) {
	t.Parallel()
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			// The CDS service listing is a read on the way to the forward, with a
			// real status: the system has still been asked nothing to act on.
			t.Run("the listing read, then this gateway's own refusal", func(t *testing.T) {
				p, got := newRecordingLevelPayer(t, level)
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					_, _ = w.Write([]byte(`{"services":[]}`))
				}))
				t.Cleanup(srv.Close)
				n := NewNativeResponder(srv.Client(), srv.URL, fixedClock, WithPASStore(p.store))
				n.bindFindingEmitter(p.g.emitFinding)
				p.g.cfg.Responder = n
				failingAnswerAuthorize(p)
				ans := p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
				wantNotAttested(t, ans)
				if rec := got.only(t); rec.Backend == nil || rec.Backend.Status != http.StatusOK || rec.Rule == RefusalAuthority {
					t.Fatalf("record %+v backend %+v, want the listing's read and no authority refusal", rec, rec.Backend)
				}
			})
			// A forward that got no answer: delivery stays unknown.
			t.Run("a forward with no answer", func(t *testing.T) {
				p, got := newRecordingLevelPayer(t, level)
				n := p.g.cfg.Responder.(*nativeResponder)
				n.client = &http.Client{Transport: forwardFault{inner: n.client.Transport, noAnswer: true}}
				failingAnswerAuthorize(p)
				ans := p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
				wantNotAttested(t, ans)
				if rec := got.only(t); rec.Backend == nil || rec.Backend.Status != 0 || rec.Outcome != ExchangeUpstreamError {
					t.Fatalf("record %+v backend %+v, want the upstream error with no status", rec, rec.Backend)
				}
			})
			// An answer the gateway builds from the participant's records: reads of
			// its system of record, and no operation forwarded.
			t.Run("an answer built from the records", func(t *testing.T) {
				p, got := newRecordingLevelPayer(t, level)
				failingAnswerAuthorize(p)
				ans := p.send(t, "coverage-eligibility", "", eligibilityRequest(t, dtrFrameMember))
				if p.partner.lastPath != "" {
					t.Fatalf("the system was asked %s: this row is about an answer from the records", p.partner.lastPath)
				}
				wantNotAttested(t, ans)
				if rec := got.only(t); rec.Rule == RefusalAuthority || rec.BackendCalls == 0 || rec.Backend == nil || rec.Backend.Status != 0 {
					t.Fatalf("record %+v backend %+v: want the system of record read and no authority refusal", rec, rec.Backend)
				}
			})
		})
	}
	// The gateway's own refusal, made before the system is asked anything:
	// strict's, since below it the same request is carried. It keeps its own
	// rule, also when the requester left while it was being authorized.
	for name, fail := range map[string]func(*levelPayer){
		"a failed authorize": failingAnswerAuthorize,
		"a requester that left": func(p *levelPayer) {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			p.ctx = ctx
			p.g.cfg.Client = &http.Client{Transport: stagedTransport{inner: p.g.cfg.Client.Transport, before: func(*http.Request) (*http.Response, error) {
				cancel()
				return nil, context.Canceled
			}}}
		},
	} {
		t.Run("a refusal before the forward, then "+name, func(t *testing.T) {
			own := resourceParam("coverage", dtrCoverage("cov-1", dtrFrameMember))
			body := dtrPackage(own, resourceParam("order", dtrOrder(dtrOtherMember)))
			p, got := newRecordingLevelPayer(t, EnforcementStrict)
			fail(p)
			ans := p.send(t, "dtr-questionnaire-fetch", shnsdk.FrameOperationQuestionnairePackage, body)
			if p.partner.lastPath != "" {
				t.Fatalf("the system was asked %s: this row is about a refusal before it", p.partner.lastPath)
			}
			wantNotAttested(t, ans)
			if rec := got.only(t); rec.Outcome != ExchangeRefused || rec.Rule == "" || rec.Rule == RefusalAuthority {
				t.Fatalf("record %s rule %q, want the refusal on its own rule", rec.Outcome, rec.Rule)
			}
		})
	}
}

// A refusal this gateway made of its system's answer keeps its rule when the
// requester leaves while that refusal is being authorized: the requester
// leaving decides nothing about the answer. At every level, since a repeated
// member name is a network rule.
func TestAnswerAuthorization_ARefusalOfTheAnswerKeepsItsRuleWhenTheRequesterLeaves(t *testing.T) {
	t.Parallel()
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p, got := newRecordingLevelPayer(t, level)
			p.partner.respByPath[pasSubmitPath] = []byte(`{"resourceType":"Bundle","resourceType":"Bundle","type":"collection","entry":[]}`)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p.ctx = ctx
			p.g.cfg.Client = &http.Client{Transport: stagedTransport{inner: p.g.cfg.Client.Transport, before: func(*http.Request) (*http.Response, error) {
				cancel()
				return nil, context.Canceled
			}}}
			ans := p.send(t, "pas-claim", "", originatorBuiltConformantBundle(t, "MBR-COVERED"))
			wantNotAttested(t, ans)
			rec := got.only(t)
			wantRefusal(t, rec, ans.status, RefusedByPayerGateway, RefusalIntegrity)
			if rec.AnswerError != BackendCancelled || rec.Backend == nil || rec.Backend.Status != http.StatusOK {
				t.Fatalf("answer error %q backend %+v, want cancelled after the system's 200", rec.AnswerError, rec.Backend)
			}
		})
	}
}

// A non-2xx answer is the system answering: it may have acted, so a failed
// authorize of it is attested like any other.
func TestAnswerAuthorization_AttestedOnANon2xxAnswer(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p, got := newRecordingLevelPayer(t, level)
			p.partner.status = http.StatusServiceUnavailable
			failingAnswerAuthorize(p)
			ans := p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
			if ans.status != http.StatusBadGateway || ans.header.Get(HubDeliveredHeader) != "yes" {
				t.Fatalf("answer %d %s (delivered %q), want the attested 502", ans.status, ans.body, ans.header.Get(HubDeliveredHeader))
			}
			if rec := got.only(t); rec.Backend == nil || rec.Backend.Status != http.StatusServiceUnavailable || rec.Outcome != ExchangeRefused || rec.Rule != RefusalAuthority {
				t.Fatalf("record %+v backend %+v, want refused on authority after the system's 503", rec, rec.Backend)
			}
		})
	}
}

// forwardFault breaks a forwarded operation (a POST; the CDS service listing is
// a GET): with noAnswer the system never answers; otherwise it answers and
// reading that answer fails.
type forwardFault struct {
	inner    http.RoundTripper
	noAnswer bool
}

func (f forwardFault) RoundTrip(r *http.Request) (*http.Response, error) {
	if f.noAnswer && r.Method == http.MethodPost {
		return nil, errors.New("connection reset")
	}
	inner := f.inner
	if inner == nil {
		inner = http.DefaultTransport
	}
	resp, err := inner.RoundTrip(r)
	if err == nil && r.Method == http.MethodPost {
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(brokenBody{})
	}
	return resp, err
}

type brokenBody struct{}

func (brokenBody) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

// What the wire attested is final: a responder failure that unwinds after the
// attestation (its own class is an upstream error) does not change the record,
// so the record and the attested answer cannot differ.
func TestAnswerAuthorization_TheAttestedRecordIsFinal(t *testing.T) {
	p, got := newRecordingLevelPayer(t, EnforcementObserve)
	n := p.g.cfg.Responder.(*nativeResponder)
	n.client = &http.Client{Transport: forwardFault{inner: n.client.Transport}}
	failingAnswerAuthorize(p)
	ans := p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
	if ans.status != http.StatusBadGateway || ans.header.Get(HubDeliveredHeader) != "yes" {
		t.Fatalf("answer %d %s (delivered %q), want the attested 502", ans.status, ans.body, ans.header.Get(HubDeliveredHeader))
	}
	if rec := got.only(t); rec.Outcome != ExchangeRefused || rec.Rule != RefusalAuthority {
		t.Fatalf("record %s rule %q, want what the wire attested: refused on authority", rec.Outcome, rec.Rule)
	}
}

// A requester that left is no refusal of the answer: with no refusal already
// made, the failed authorize is not attested, is recorded other, not on a rule,
// and counts in no refusal.
func TestAnswerAuthorization_ARequesterThatLeftIsNotARefusal(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p, got := newRecordingLevelPayer(t, level)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p.ctx = ctx
			p.g.cfg.Client = &http.Client{Transport: stagedTransport{inner: p.g.cfg.Client.Transport, before: func(*http.Request) (*http.Response, error) {
				cancel()
				return nil, context.Canceled
			}}}
			ans := p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
			wantNotAttested(t, ans)
			rec := got.only(t)
			if rec.Outcome != ExchangeOther || rec.Rule != "" || rec.RefusedBy != "" || rec.AnswerError != BackendCancelled || rec.Backend == nil || rec.Backend.Status != http.StatusOK {
				t.Fatalf("record %s by %q rule %q answer error %q backend %+v, want other, on no rule, cancelled, after the system's 200", rec.Outcome, rec.RefusedBy, rec.Rule, rec.AnswerError, rec.Backend)
			}
		})
	}
}

// No authority for a request leg is a network rule: the leg is refused on
// authority at every conformance level, whatever kept the authority away, and
// the record says what did, while the caller's answer is what it was.
func TestRequestLeg_NoAuthorityIsRefusedAtEveryLevel(t *testing.T) {
	t.Parallel()
	for _, level := range allLevels {
		for _, row := range []struct {
			name   string
			fault  func(*http.Request) (*http.Response, error)
			status int
			class  string
		}{
			{"no token", answer(http.StatusOK, `{}`), http.StatusBadGateway, ExchangeOther},
			{"a token that does not verify", answer(http.StatusOK, `{"token":{"operation":"order-select","holder":"provider","signature":"AAAA"}}`), http.StatusBadGateway, ExchangeOther},
			{"the Framework erroring", answer(http.StatusInternalServerError, `{"error":"boom"}`), http.StatusBadGateway, ExchangeOther},
			{"the Framework unreachable", transportFault(dialRefused(), false), http.StatusServiceUnavailable, "unreachable"},
		} {
			t.Run(level.String()+"/"+row.name, func(t *testing.T) {
				env := newInProcessExchange(t)
				env.originator.cfg.ConformanceEnforcement = level
				var got exchangeRecords
				env.originator.cfg.ExchangeObserved = got.observe
				env.payerReturns(LegResult{})
				env.substrate.authorizeFaults = []func(*http.Request) (*http.Response, error){row.fault, row.fault}
				rec := httptest.NewRecorder()
				env.originator.ingressRoute(RouteCRD)(rec, env.crdIngressRequest(t))
				if rec.Code != row.status || env.routeHitCount() != 0 {
					t.Fatalf("answer %d %s (%d sent), want %d and nothing sent", rec.Code, rec.Body.String(), env.routeHitCount(), row.status)
				}
				r := got.only(t)
				if r.Outcome != ExchangeRefused || r.RefusedBy != RefusedByProviderGateway || r.Rule != RefusalAuthority || r.AuthorizeError != row.class {
					t.Fatalf("record %s by %q rule %q class %q, want refused by the provider gateway on authority, %s", r.Outcome, r.RefusedBy, r.Rule, r.AuthorizeError, row.class)
				}
			})
		}
	}
}

// A requester that left refused nothing: its authorize call being cut short is
// no refusal on authority, and the record says it was cancelled.
func TestRequestLeg_ARequesterThatLeftIsNotARefusal(t *testing.T) {
	env := newInProcessExchange(t)
	var got exchangeRecords
	env.originator.cfg.ExchangeObserved = got.observe
	env.payerReturns(LegResult{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env.substrate.authorizeFaults = []func(*http.Request) (*http.Response, error){func(*http.Request) (*http.Response, error) {
		cancel()
		return nil, context.Canceled
	}}
	env.originator.ingressRoute(RouteCRD)(httptest.NewRecorder(), env.crdIngressRequest(t).WithContext(ctx))
	if r := got.only(t); r.Outcome == ExchangeRefused || r.RefusedBy != "" || r.Rule != "" || r.AuthorizeError != BackendCancelled {
		t.Fatalf("record %s by %q rule %q class %q, want no refusal, cancelled", r.Outcome, r.RefusedBy, r.Rule, r.AuthorizeError)
	}
}
