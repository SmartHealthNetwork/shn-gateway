//go:build !shn_coreonly

package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/diagnostics"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A Hub or Authorization Framework instance that has not yet loaded the
// network's membership refuses before it acts (503, X-SHN-Delivered: no,
// {"error": hubReasonNotYetLoaded}). The gateway sends the call again, at most
// twice, each time on a new connection and with a fresh assertion; every
// other answer keeps its handling. These rows run the in-process exchange over
// real loopback connections, so a retry that reused a pooled connection is
// seen.

// connLog serves an in-process RoundTripper over a loopback server and records
// which connection each call arrived on.
type connLog struct {
	mu    sync.Mutex
	conns atomic.Int32
	calls []connCall
}

type connCall struct {
	seq       int
	path, jti string
	conn      int32
}

type connIDKey struct{}

// realConns serves rt on a loopback server and returns a client whose own
// *http.Transport dials it for every host (the harness's URLs are unchanged),
// keep-alives on as in production, and the server never closing a connection
// on its own: a retry reaches a new connection only if the gateway made one.
// A RoundTripper error drops the connection without an answer.
func realConns(t *testing.T, rt http.RoundTripper) (*http.Client, *connLog) {
	t.Helper()
	return realConnsFailing(t, rt, 0)
}

// realConnsFailing is realConns whose dials, from the failFrom'th on (0:
// none), are refused: a call needing a new connection then fails before its
// request is written.
func realConnsFailing(t *testing.T, rt http.RoundTripper, failFrom int32) (*http.Client, *connLog) {
	t.Helper()
	l := &connLog{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		l.calls = append(l.calls, connCall{seq: len(l.calls), path: r.URL.Path, jti: assertionJTI(r.Header.Get("X-Holder-Assertion")), conn: r.Context().Value(connIDKey{}).(int32)})
		l.mu.Unlock()
		out := r.Clone(r.Context())
		out.RequestURI = ""
		out.URL.Scheme, out.URL.Host = "http", "relay-stub.test"
		resp, err := rt.RoundTrip(out)
		if err != nil {
			if c, _, herr := w.(http.Hijacker).Hijack(); herr == nil {
				c.Close()
			}
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body) //nolint:errcheck
	}))
	srv.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		return context.WithValue(ctx, connIDKey{}, l.conns.Add(1))
	}
	srv.Start()
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	var dials atomic.Int32
	tr := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		if n := dials.Add(1); failFrom > 0 && n >= failFrom {
			return nil, dialRefused()
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}, l
}

// at returns the calls to path, in order.
func (l *connLog) at(path string) []connCall {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []connCall
	for _, c := range l.calls {
		if strings.HasSuffix(c.path, path) {
			out = append(out, c)
		}
	}
	return out
}

func assertionJTI(h string) string {
	raw, err := base64.StdEncoding.DecodeString(h)
	if err != nil {
		return ""
	}
	var a shnsdk.Assertion
	if json.Unmarshal(raw, &a) != nil {
		return ""
	}
	return a.JTI
}

// wantFreshEach fails unless every call of calls after the first arrived on a
// connection no earlier call (of any path, in all) used, and each carried a
// jti no earlier attempt carried.
func wantFreshEach(t *testing.T, all []connCall, calls []connCall) {
	t.Helper()
	for i, c := range calls {
		if i == 0 {
			continue
		}
		for _, earlier := range all[:c.seq] {
			if earlier.conn == c.conn {
				t.Fatalf("retry %d arrived on connection %d, which an earlier call used: %+v", i, c.conn, all)
			}
		}
		for _, p := range calls[:i] {
			if p.jti == "" || p.jti == c.jti {
				t.Fatalf("retry %d carried jti %q, an earlier attempt's: %+v", i, c.jti, calls)
			}
		}
	}
}

// notReadyHub answers the first n /route calls with ans, then passes them on.
type notReadyHub struct {
	next http.RoundTripper
	n    *atomic.Int32
	ans  func() (*http.Response, error)
}

func (h notReadyHub) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/route") && h.n.Add(-1) >= 0 {
		io.Copy(io.Discard, req.Body) //nolint:errcheck
		return h.ans()
	}
	return h.next.RoundTrip(req)
}

// refusalAnswer is a 503 with the given X-SHN-Delivered values and body.
func refusalAnswer(status int, delivered []string, body string) func() (*http.Response, error) {
	return func() (*http.Response, error) {
		h := http.Header{"Content-Type": []string{"application/json"}, "Retry-After": []string{"3"}}
		for _, d := range delivered {
			h.Add(HubDeliveredHeader, d)
		}
		return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}, nil
	}
}

func namedRefusal() func() (*http.Response, error) {
	return refusalAnswer(http.StatusServiceUnavailable, []string{"no"}, notYetLoadedBody)
}

// retryEnv is the in-process exchange at level, its Hub and Framework behind
// rt over real connections, with the waits and retries it makes recorded.
type retryEnv struct {
	*inProcessExchange
	log     *connLog
	got     *exchangeRecords
	waits   []time.Duration
	retries []string
}

func newRetryEnv(t *testing.T, level ConformanceEnforcement, wrap func(http.RoundTripper) http.RoundTripper) *retryEnv {
	t.Helper()
	return newRetryEnvFailing(t, level, wrap, 0)
}

// newRetryEnvFailing is newRetryEnv over realConnsFailing.
func newRetryEnvFailing(t *testing.T, level ConformanceEnforcement, wrap func(http.RoundTripper) http.RoundTripper, failFrom int32) *retryEnv {
	t.Helper()
	env := newInProcessExchange(t)
	r := &retryEnv{inProcessExchange: env, got: &exchangeRecords{}}
	env.originator.cfg.ExchangeObserved = r.got.observe
	env.originator.cfg.ConformanceEnforcement = level
	env.payerReturns(LegResult{})
	var mu sync.Mutex
	env.originator.waitForRetry = func(ctx context.Context, d time.Duration) bool {
		mu.Lock()
		r.waits = append(r.waits, d)
		mu.Unlock()
		return ctx.Err() == nil
	}
	env.originator.cfg.NetworkRetryMetric = func(service, result string) {
		mu.Lock()
		r.retries = append(r.retries, service+"/"+result)
		mu.Unlock()
	}
	rt := http.RoundTripper(env.substrate)
	if wrap != nil {
		rt = wrap(rt)
	}
	env.originator.cfg.Client, r.log = realConnsFailing(t, rt, failFrom)
	return r
}

func (r *retryEnv) drive(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r.originator.ingressRoute(RouteCRD)(rec, r.crdIngressRequest(t))
	return rec
}

func hubRefusedNTimes(n int32, ans func() (*http.Response, error)) func(http.RoundTripper) http.RoundTripper {
	return func(next http.RoundTripper) http.RoundTripper {
		c := &atomic.Int32{}
		c.Store(n)
		return notReadyHub{next: next, n: c, ans: ans}
	}
}

// The Hub's not-yet-loaded refusal is sent again, each time on a new
// connection and with a fresh assertion, at every conformance level: once
// refused then answered, twice refused then answered.
func TestNotYetLoaded_HubRetriedOnANewConnection(t *testing.T) {
	t.Parallel()
	for _, level := range everyLevel {
		name := fmt.Sprint(level)
		for _, row := range []struct {
			refusals int32
			retries  []string
			waits    []time.Duration
		}{
			{1, []string{"hub/answered"}, []time.Duration{250 * time.Millisecond}},
			{2, []string{"hub/refused", "hub/answered"}, []time.Duration{250 * time.Millisecond, 500 * time.Millisecond}},
		} {
			t.Run(fmt.Sprintf("%s/%d refusals", name, row.refusals), func(t *testing.T) {
				env := newRetryEnv(t, level, hubRefusedNTimes(row.refusals, namedRefusal()))
				rec := env.drive(t)
				if rec.Code != http.StatusOK {
					t.Fatalf("answer %d %s, want the leg answered after %d refusals", rec.Code, rec.Body, row.refusals)
				}
				if r := env.got.only(t); r.Outcome != ExchangeAnswered {
					t.Fatalf("record %+v, want answered", r)
				}
				routes := env.log.at("/route")
				if len(routes) != int(row.refusals)+1 {
					t.Fatalf("%d POSTs to the Hub, want %d", len(routes), row.refusals+1)
				}
				wantFreshEach(t, env.log.calls, routes)
				if strings.Join(env.retries, ",") != strings.Join(row.retries, ",") {
					t.Fatalf("retries %v, want %v", env.retries, row.retries)
				}
				if len(env.waits) != len(row.waits) || (len(row.waits) > 1 && env.waits[1] != row.waits[1]) || env.waits[0] != row.waits[0] {
					t.Fatalf("waits %v, want %v", env.waits, row.waits)
				}
			})
		}
	}
}

// Refused three times, the leg is refused exactly as before: the Hub's 503
// with its reason and wait, recorded as routing, and no fourth POST.
func TestNotYetLoaded_HubRefusedEveryTimeIsCarried(t *testing.T) {
	t.Parallel()
	for _, level := range everyLevel {
		name := fmt.Sprint(level)
		t.Run(name, func(t *testing.T) {
			env := newRetryEnv(t, level, hubRefusedNTimes(5, namedRefusal()))
			rec := env.drive(t)
			if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "hub refused the exchange: "+hubReasonNotYetLoaded) {
				t.Fatalf("answer %d %s, want the Hub's 503 with its reason", rec.Code, rec.Body)
			}
			if got := rec.Header().Get("Retry-After"); got != "3" {
				t.Fatalf("Retry-After %q, want the Hub's 3", got)
			}
			if r := env.got.only(t); r.Outcome != ExchangeRefused || r.RefusedBy != RefusedByHub || r.Rule != RefusalRouting {
				t.Fatalf("record %s %s/%s, want refused hub/routing", r.Outcome, r.RefusedBy, r.Rule)
			}
			routes := env.log.at("/route")
			if len(routes) != 1+notYetLoadedRetries {
				t.Fatalf("%d POSTs to the Hub, want %d", len(routes), 1+notYetLoadedRetries)
			}
			wantFreshEach(t, env.log.calls, routes)
			if strings.Join(env.retries, ",") != "hub/refused,hub/refused" {
				t.Fatalf("retries %v, want two refused", env.retries)
			}
		})
	}
}

// Nothing but the named refusal is sent again: one POST each, at every level.
func TestNotYetLoaded_HubOtherAnswersAreNotRetried(t *testing.T) {
	t.Parallel()
	rows := map[string]func() (*http.Response, error){
		"a 503 without X-SHN-Delivered":           refusalAnswer(503, nil, notYetLoadedBody),
		"a 503 delivered unknown":                 refusalAnswer(503, []string{"unknown"}, notYetLoadedBody),
		"a 503 with two delivered values":         refusalAnswer(503, []string{"no", "no"}, notYetLoadedBody),
		"a 503 with another reason":               refusalAnswer(503, []string{"no"}, `{"error":"`+hubReasonEnvelopeBudget+`"}`),
		"a 503 with the reason in a longer error": refusalAnswer(503, []string{"no"}, `{"error":"`+hubReasonNotYetLoaded+` (retry)"}`),
		"a 503 with the reason padded":            refusalAnswer(503, []string{"no"}, `{"error":" `+hubReasonNotYetLoaded+`"}`),
		"a 503 with the reason as plain text":     refusalAnswer(503, []string{"no"}, hubReasonNotYetLoaded),
		"a 503 with the reason in another field":  refusalAnswer(503, []string{"no"}, `{"reason":"`+hubReasonNotYetLoaded+`"}`),
		"a 504 with the reason":                   refusalAnswer(504, []string{"no"}, notYetLoadedBody),
		"a 502 with the reason":                   refusalAnswer(502, []string{"no"}, notYetLoadedBody),
		"a 429":                                   refusalAnswer(429, []string{"no"}, `{"error":"too many requests"}`),
		"a reset":                                 func() (*http.Response, error) { return nil, connReset() },
	}
	for _, lv := range everyLevel {
		level := fmt.Sprint(lv)
		for name, ans := range rows {
			t.Run(level+"/"+name, func(t *testing.T) {
				env := newRetryEnv(t, lv, hubRefusedNTimes(5, ans))
				env.drive(t)
				if n := len(env.log.at("/route")); n != 1 {
					t.Fatalf("%d POSTs to the Hub, want 1: only the named refusal is sent again", n)
				}
				if len(env.retries) != 0 || len(env.waits) != 0 {
					t.Fatalf("retries %v, waits %v, want none", env.retries, env.waits)
				}
			})
		}
	}
}

// A retry that would reuse the refused connection is the defect this rule
// exists for: the refusing instance's answer here keeps its connection open
// (as a Hub before the connection-closing change did), so only a new
// connection reaches another call. The Hub's calls arrive on connections no
// earlier call used, which a pooled client would fail.
func TestNotYetLoaded_RetryNeverReusesTheConnection(t *testing.T) {
	t.Parallel()
	env := newRetryEnv(t, EnforcementStrict, hubRefusedNTimes(1, namedRefusal()))
	env.drive(t)
	routes := env.log.at("/route")
	if len(routes) != 2 {
		t.Fatalf("%d POSTs to the Hub, want 2", len(routes))
	}
	if routes[1].conn == routes[0].conn {
		t.Fatalf("the retry reused connection %d", routes[0].conn)
	}
	// The control: the same client, its connection pooled, sends the next
	// call on the connection it already holds.
	env2 := newRetryEnv(t, EnforcementStrict, nil)
	env2.drive(t)
	calls := env2.log.calls
	if len(calls) < 2 || calls[len(calls)-1].conn != calls[0].conn {
		t.Fatalf("control: the pooled client did not reuse its connection: %+v", calls)
	}
}

// The wait before a retry is inside the call's own context: ended there, the
// refusal is carried with no further POST.
func TestNotYetLoaded_HubWaitEndsWithTheCall(t *testing.T) {
	t.Parallel()
	env := newRetryEnv(t, EnforcementStrict, hubRefusedNTimes(5, namedRefusal()))
	env.originator.waitForRetry = func(context.Context, time.Duration) bool { return false }
	rec := env.drive(t)
	if rec.Code != http.StatusServiceUnavailable || len(env.log.at("/route")) != 1 || len(env.retries) != 0 {
		t.Fatalf("answer %d, %d POSTs, retries %v: want the refusal carried after one POST", rec.Code, len(env.log.at("/route")), env.retries)
	}
}

// A client whose transport is not an *http.Transport cannot be guaranteed a
// new connection, so the refusal is carried at once, as before.
func TestNotYetLoaded_NoRetryWithoutAnHTTPTransport(t *testing.T) {
	t.Parallel()
	env := newInProcessExchange(t)
	var routes atomic.Int32
	env.originator.cfg.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/route") {
			routes.Add(1)
			return namedRefusal()()
		}
		return env.substrate.RoundTrip(req)
	})}
	env.originator.cfg.NetworkRetryMetric = func(string, string) { t.Error("a retry was counted") }
	rec := httptest.NewRecorder()
	env.originator.ingressRoute(RouteCRD)(rec, env.crdIngressRequest(t))
	if rec.Code != http.StatusServiceUnavailable || routes.Load() != 1 {
		t.Fatalf("answer %d after %d POSTs, want the refusal after one", rec.Code, routes.Load())
	}
}

// authzNotReady answers /authorize faults through the substrate's fault list.
func authzRefusals(env *retryEnv, faults ...func(*http.Request) (*http.Response, error)) {
	env.substrate.authorizeFaults = faults
}

func authzNamed(*http.Request) (*http.Response, error) { return namedRefusal()() }

// The Framework's not-yet-loaded refusal is sent again the same way: each on
// a new connection with a fresh assertion, at every level.
func TestNotYetLoaded_AuthzRetriedOnANewConnection(t *testing.T) {
	t.Parallel()
	for _, level := range everyLevel {
		name := fmt.Sprint(level)
		for _, n := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s/%d refusals", name, n), func(t *testing.T) {
				env := newRetryEnv(t, level, nil)
				faults := make([]func(*http.Request) (*http.Response, error), n)
				for i := range faults {
					faults[i] = authzNamed
				}
				authzRefusals(env, faults...)
				rec := env.drive(t)
				if rec.Code != http.StatusOK {
					t.Fatalf("answer %d %s, want the leg answered after %d refusals", rec.Code, rec.Body, n)
				}
				calls := env.log.at("/authorize")
				if len(calls) != n+1 {
					t.Fatalf("%d authorize calls, want %d", len(calls), n+1)
				}
				wantFreshEach(t, env.log.calls, calls)
				want := []string{"authz/answered"}
				if n == 2 {
					want = []string{"authz/refused", "authz/answered"}
				}
				if strings.Join(env.retries, ",") != strings.Join(want, ",") {
					t.Fatalf("retries %v, want %v", env.retries, want)
				}
			})
		}
	}
}

// Refused three times, the authorize call is unreachable exactly as before:
// the provider's own 503 and nothing sent to the Hub.
func TestNotYetLoaded_AuthzRefusedEveryTimeIsCarried(t *testing.T) {
	t.Parallel()
	for _, level := range everyLevel {
		name := fmt.Sprint(level)
		t.Run(name, func(t *testing.T) {
			env := newRetryEnv(t, level, nil)
			authzRefusals(env, authzNamed, authzNamed, authzNamed, authzNamed)
			rec := env.drive(t)
			if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "the authorization service could not be reached") {
				t.Fatalf("answer %d %s, want the provider's 503 naming the authorization service", rec.Code, rec.Body)
			}
			if r := env.got.only(t); r.Outcome != ExchangeRefused || r.Rule != RefusalAuthority || r.AuthorizeError != "unreachable" {
				t.Fatalf("record %+v, want refused on authority, the Framework unreachable", r)
			}
			if n := len(env.log.at("/authorize")); n != 1+notYetLoadedRetries {
				t.Fatalf("%d authorize calls, want %d", n, 1+notYetLoadedRetries)
			}
			if len(env.log.at("/route")) != 0 {
				t.Fatal("the leg crossed to the Hub without a token")
			}
			if strings.Join(env.retries, ",") != "authz/refused,authz/refused" {
				t.Fatalf("retries %v, want two refused", env.retries)
			}
		})
	}
}

// Nothing but the named refusal is sent again to the Framework.
func TestNotYetLoaded_AuthzOtherAnswersAreNotRetried(t *testing.T) {
	t.Parallel()
	asFault := func(ans func() (*http.Response, error)) func(*http.Request) (*http.Response, error) {
		return func(*http.Request) (*http.Response, error) { return ans() }
	}
	rows := map[string]func(*http.Request) (*http.Response, error){
		"a 503 without X-SHN-Delivered":       asFault(refusalAnswer(503, nil, notYetLoadedBody)),
		"a 503 with another reason":           asFault(refusalAnswer(503, []string{"no"}, `{"error":"no healthy upstream"}`)),
		"a 503 with the reason as plain text": asFault(refusalAnswer(503, []string{"no"}, hubReasonNotYetLoaded)),
		"a 504 with the reason":               asFault(refusalAnswer(504, []string{"no"}, notYetLoadedBody)),
		"a 429":                               asFault(refusalAnswer(429, []string{"no"}, `{"error":"slow down"}`)),
	}
	for _, lv := range everyLevel {
		level := fmt.Sprint(lv)
		for name, f := range rows {
			t.Run(level+"/"+name, func(t *testing.T) {
				env := newRetryEnv(t, lv, nil)
				authzRefusals(env, f, f, f)
				env.drive(t)
				if n := len(env.log.at("/authorize")); n != 1 {
					t.Fatalf("%d authorize calls, want 1", n)
				}
				if len(env.retries) != 0 {
					t.Fatalf("retries %v, want none", env.retries)
				}
			})
		}
	}
}

// A refused attempt sent the request's bytes, so the leg's record names what
// it sent (its ownership and edits) even when the retry then fails before its
// own request is written: here the retry's new connection is refused.
func TestNotYetLoaded_HubRetryFailingBeforeTheWriteKeepsWhatWasSent(t *testing.T) {
	t.Parallel()
	for _, level := range everyLevel {
		t.Run(fmt.Sprint(level), func(t *testing.T) {
			// The first dial carries /authorize and the first /route (pooled);
			// the retry's own dial, the second, is refused.
			env := newRetryEnvFailing(t, level, hubRefusedNTimes(1, namedRefusal()), 2)
			env.drive(t)
			r := env.got.only(t)
			if r.Ownership == "" {
				t.Fatalf("record %+v: the refused attempt sent the request, so its ownership is recorded", r)
			}
			if len(env.log.at("/route")) != 1 || strings.Join(env.retries, ",") != "hub/failed" {
				t.Fatalf("%d POSTs reached the Hub, retries %v: want the one refused POST and a failed retry", len(env.log.at("/route")), env.retries)
			}
		})
	}
}

// The wait before a Hub retry is bound by the leg's own deadline, and before
// an authorize retry by the call's own context: each wait is handed that
// context.
func TestNotYetLoaded_WaitIsBoundByTheCall(t *testing.T) {
	t.Parallel()
	t.Run("hub", func(t *testing.T) {
		env := newRetryEnv(t, EnforcementStrict, hubRefusedNTimes(1, namedRefusal()))
		env.originator.cfg.Client.Timeout = 30 * time.Second
		var deadline time.Time
		env.originator.waitForRetry = func(ctx context.Context, _ time.Duration) bool {
			deadline, _ = ctx.Deadline()
			return true
		}
		start := time.Now()
		env.drive(t)
		if deadline.IsZero() || deadline.Before(start.Add(29*time.Second)) || deadline.After(time.Now().Add(31*time.Second)) {
			t.Fatalf("the Hub retry waited under deadline %v: want the leg's own, 30 s from its POST", deadline)
		}
	})
	t.Run("authz", func(t *testing.T) {
		type callKey struct{}
		env := newRetryEnv(t, EnforcementStrict, nil)
		authzRefusals(env, authzNamed)
		var got any
		env.originator.waitForRetry = func(ctx context.Context, _ time.Duration) bool {
			got = ctx.Value(callKey{})
			return true
		}
		rec := httptest.NewRecorder()
		req := env.crdIngressRequest(t)
		env.originator.ingressRoute(RouteCRD)(rec, req.WithContext(context.WithValue(req.Context(), callKey{}, "this call")))
		if got != "this call" {
			t.Fatalf("the authorize retry waited under %v: want the call's own context", got)
		}
	})
}

// A retry that meets another answer is counted by it: the Hub's other 503
// as answered (and carried), a proxy's 503 in front of the Framework as
// failed.
func TestNotYetLoaded_RetryCountedByWhatItMet(t *testing.T) {
	t.Parallel()
	t.Run("hub", func(t *testing.T) {
		var n atomic.Int32
		env := newRetryEnv(t, EnforcementStrict, func(next http.RoundTripper) http.RoundTripper {
			return roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if !strings.HasSuffix(req.URL.Path, "/route") {
					return next.RoundTrip(req)
				}
				io.Copy(io.Discard, req.Body) //nolint:errcheck
				if n.Add(1) == 1 {
					return namedRefusal()()
				}
				return refusalAnswer(503, []string{"no"}, `{"error":"replay guard unavailable"}`)()
			})
		})
		rec := env.drive(t)
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "replay guard unavailable") {
			t.Fatalf("answer %d %s, want the retry's own answer carried", rec.Code, rec.Body)
		}
		if strings.Join(env.retries, ",") != "hub/answered" || len(env.log.at("/route")) != 2 {
			t.Fatalf("retries %v after %d POSTs, want one answered retry", env.retries, len(env.log.at("/route")))
		}
	})
	t.Run("authz", func(t *testing.T) {
		env := newRetryEnv(t, EnforcementStrict, nil)
		authzRefusals(env, authzNamed, func(*http.Request) (*http.Response, error) {
			return refusalAnswer(503, nil, "no healthy upstream")()
		})
		env.drive(t)
		if strings.Join(env.retries, ",") != "authz/failed" || len(env.log.at("/authorize")) != 2 {
			t.Fatalf("retries %v after %d calls, want one failed retry", env.retries, len(env.log.at("/authorize")))
		}
	})
}

// hubCaptures collects the diagnostic captures of the Hub's answers: the
// leg.failed events whose detail begins "Hub response".
type hubCaptures struct {
	mu     sync.Mutex
	events []diagnostics.Event
}

func (c *hubCaptures) observe(e diagnostics.Event) bool {
	if e.Kind == "leg.failed" && strings.HasPrefix(e.Detail, "Hub response") {
		c.mu.Lock()
		c.events = append(c.events, e)
		c.mu.Unlock()
	}
	return true
}

func (c *hubCaptures) details() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, e := range c.events {
		out = append(out, e.Detail)
	}
	return out
}

// Each not-yet-loaded refusal is captured once, with the Hub's own status and
// bytes, its detail saying whether the leg was sent again: refused every
// time, two captures naming their retries and the last as "Hub response";
// answered on the first retry, one naming it; with no retry possible, one
// "Hub response".
func TestNotYetLoaded_EachRefusalIsCapturedOnce(t *testing.T) {
	t.Parallel()
	retried := func(n int) string {
		return fmt.Sprintf("Hub response: refused before acting; sent again on a new connection (retry %d of %d)", n, notYetLoadedRetries)
	}
	for name, row := range map[string]struct {
		refusals int32
		want     []string
	}{
		"refused every time":          {5, []string{retried(1), retried(2), "Hub response"}},
		"answered on the first retry": {1, []string{retried(1)}},
	} {
		t.Run(name, func(t *testing.T) {
			env := newRetryEnv(t, EnforcementStrict, hubRefusedNTimes(row.refusals, namedRefusal()))
			var c hubCaptures
			env.originator.cfg.Diagnostic = c.observe
			env.drive(t)
			if got := c.details(); strings.Join(got, "|") != strings.Join(row.want, "|") {
				t.Fatalf("captures %q, want %q", got, row.want)
			}
			for _, e := range c.events {
				if e.Status != http.StatusServiceUnavailable || string(e.Body) != notYetLoadedBody || !e.BodyComplete {
					t.Fatalf("capture %d %q, want the Hub's own 503 and bytes", e.Status, e.Body)
				}
			}
		})
	}
	t.Run("no retry possible", func(t *testing.T) {
		env := newInProcessExchange(t)
		var c hubCaptures
		env.originator.cfg.Diagnostic = c.observe
		env.originator.cfg.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if strings.HasSuffix(req.URL.Path, "/route") {
				io.Copy(io.Discard, req.Body) //nolint:errcheck
				return namedRefusal()()
			}
			return env.substrate.RoundTrip(req)
		})}
		rec := httptest.NewRecorder()
		env.originator.ingressRoute(RouteCRD)(rec, env.crdIngressRequest(t))
		if got := c.details(); len(got) != 1 || got[0] != "Hub response" {
			t.Fatalf("captures %q, want one \"Hub response\"", got)
		}
	})
}
