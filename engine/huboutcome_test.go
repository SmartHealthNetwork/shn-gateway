//go:build !shn_coreonly

package engine

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"testing"
)

// Through the provider's real CRD ingress, each way the leg can fail behind the
// Hub reaches the caller as what it was, not one
// generic 502 "hub routing failed".
func TestIngressReportsTheRealOutcomeBehindTheHub(t *testing.T) {
	t.Parallel()
	notDelivered := map[string]string{HubDeliveredHeader: "no"}
	type answer struct {
		status int
		body   string
		header map[string]string
	}
	rows := map[string]struct {
		authorize *answer // the Authorization Framework's answer, when overridden
		route     *answer // the Hub's answer, when overridden
		status    int
		contains  string
	}{
		"Hub replay refusal": {route: &answer{409, `{"error":"replay detected"}`, notDelivered},
			status: http.StatusConflict, contains: "hub refused the exchange: replay detected"},
		// Any other Hub 4xx is about this gateway's standing with the Hub, not
		// the caller's request: a 502 with the Hub's reason.
		"Hub clock-skew refusal": {route: &answer{400, `{"error":"stale or future timestamp"}`, notDelivered},
			status: http.StatusBadGateway, contains: "hub refused the exchange: stale or future timestamp"},
		"Hub token refusal": {route: &answer{403, `{"error":"authz token verification failed"}`, notDelivered},
			status: http.StatusBadGateway, contains: "hub refused the exchange: authz token verification failed"},
		"Hub unknown sender": {route: &answer{401, `{"error":"unknown sender"}`, notDelivered},
			status: http.StatusBadGateway, contains: "hub refused the exchange: unknown sender"},
		"Hub unknown recipient": {route: &answer{502, `{"error":"unknown recipient"}`, notDelivered},
			status: http.StatusBadGateway, contains: "hub refused the exchange: unknown recipient"},
		"recipient's gateway refused at its edge": {route: &answer{502, `{"error":"forward to recipient failed: the recipient refused it (403)"}`, notDelivered},
			status: http.StatusBadGateway, contains: "the recipient's gateway refused the exchange (403)"},
		"recipient answered, answer lost at the Hub": {route: &answer{502, `{"error":"response audit append failed"}`, map[string]string{HubDeliveredHeader: "yes"}},
			status: http.StatusBadGateway, contains: "the recipient received this request and answered, but its answer was lost on the way back (response audit append failed)"},
		"recipient may have received it": {route: &answer{502, `{"error":"forward to recipient failed: the recipient answered 503"}`, map[string]string{HubDeliveredHeader: "unknown"}},
			status: http.StatusBadGateway, contains: "the recipient may have received this request (forward to recipient failed: the recipient answered 503)"},
		// Unmarked: not the Hub's route handler (a load balancer in front of it
		// replacing a task mid-forward). A 5xx of that kind may have come after
		// the Hub forwarded.
		"unmarked 5xx in front of the Hub": {route: &answer{502, `<html>Bad Gateway</html>`, nil},
			status: http.StatusBadGateway, contains: "the recipient may have received this request (Bad Gateway)"},
		// A Hub 200 that is not an envelope may have come from in front of
		// the Hub: the recipient may have received the request, no more.
		"Hub 200 that is not an envelope": {route: &answer{200, `not an envelope`, nil},
			status: http.StatusBadGateway, contains: "the recipient may have received this request (the Hub's answer is not an envelope)"},
		"authorization denied": {authorize: &answer{403, `{"error":"denied"}`, nil},
			status: http.StatusForbidden, contains: "authorization denied"},
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			env := newInProcessExchange(t)
			base := env.originator.cfg.Client.Transport
			env.originator.cfg.Client.Transport = diagnosticRoundTripper(func(r *http.Request) (*http.Response, error) {
				var a *answer
				switch {
				case strings.HasSuffix(r.URL.Path, "/route"):
					a = row.route
				case strings.HasSuffix(r.URL.Path, "/authorize"):
					a = row.authorize
				}
				if a == nil {
					return base.RoundTrip(r)
				}
				h := http.Header{"Content-Type": {"application/json"}}
				for k, v := range a.header {
					h.Set(k, v)
				}
				return &http.Response{StatusCode: a.status, Body: io.NopCloser(bytes.NewReader([]byte(a.body))), Header: h}, nil
			})
			rec := httptest.NewRecorder()
			env.originator.withIngressCorrelation(env.originator.handleCRDIngress)(rec, env.crdIngressRequest(t))
			var got struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &got)
			if rec.Code != row.status || !strings.Contains(got.Error, row.contains) {
				t.Fatalf("got %d %s, want %d containing %q", rec.Code, rec.Body.String(), row.status, row.contains)
			}
			if strings.Contains(got.Error, "hub routing failed") {
				t.Fatalf("the real outcome was collapsed into the generic routing failure: %s", rec.Body.String())
			}
		})
	}
}

// The Hub itself unreachable is still the one generic routing failure: there
// is no answer to report.
func TestIngressHubUnreachableIsTheRoutingFailure(t *testing.T) {
	env := newInProcessExchange(t)
	base := env.originator.cfg.Client.Transport
	env.originator.cfg.Client.Transport = diagnosticRoundTripper(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/route") {
			return nil, io.ErrUnexpectedEOF
		}
		return base.RoundTrip(r)
	})
	rec := httptest.NewRecorder()
	env.originator.withIngressCorrelation(env.originator.handleCRDIngress)(rec, env.crdIngressRequest(t))
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "hub routing failed") {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

// A connection to the Hub that fails after the request was written may have
// been forwarded: it is reported as possibly received, never as the routing
// failure a caller would resend.
func TestIngressHubConnectionLostAfterSendMayHaveBeenReceived(t *testing.T) {
	env := newInProcessExchange(t)
	base := env.originator.cfg.Client.Transport
	env.originator.cfg.Client.Transport = diagnosticRoundTripper(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/route") {
			if trace := httptrace.ContextClientTrace(r.Context()); trace != nil && trace.WroteRequest != nil {
				trace.WroteRequest(httptrace.WroteRequestInfo{})
			}
			return nil, io.ErrUnexpectedEOF
		}
		return base.RoundTrip(r)
	})
	rec := httptest.NewRecorder()
	env.originator.withIngressCorrelation(env.originator.handleCRDIngress)(rec, env.crdIngressRequest(t))
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "the recipient may have received this request (the connection to the Hub failed after the request was sent)") {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

// A response leg the provider cannot accept — here, one that fails its
// verification — is reported as possibly received: nothing verified says the
// payer answered, so it is never said to have.
func TestIngressAnswerThatFailsVerificationIsReportedAsDelivered(t *testing.T) {
	env := newInProcessExchange(t)
	env.substrate.mutateResp = func(b []byte) []byte {
		return bytes.Replace(b, []byte(`"authzToken":"`), []byte(`"authzToken":"x`), 1)
	}
	rec := httptest.NewRecorder()
	env.originator.withIngressCorrelation(env.originator.handleCRDIngress)(rec, env.crdIngressRequest(t))
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "the recipient may have received this request (response leg authorization failed)") ||
		strings.Contains(rec.Body.String(), "answered") {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

// The Hub's refusal of a large envelope it has no room to hold, and of every
// leg while an instance has not yet read the network's membership, reaches the
// caller as a 503 with the Hub's wait, passed on as the Hub sent it, on the
// leg's own route as on any other. A wait that cannot be read is that refusal's default; a 503 with
// another reason, or one the Hub did not write, states no wait.
func TestIngressRelaysTheHubsWaitOnTheRefusalsThatStateOne(t *testing.T) {
	for reason, fallback := range map[string]string{hubReasonEnvelopeBudget: "5", hubReasonNotYetLoaded: "3"} {
		refusal := `{"error":"` + reason + `"}`
		rows := map[string]struct {
			body, delivered, retryAfter string
			want                        string
		}{
			"the Hub's wait":      {refusal, "no", "7", "7"},
			"the longest wait":    {refusal, "no", "60", "60"},
			"no wait stated":      {refusal, "no", "", fallback},
			"a wait out of range": {refusal, "no", "600", fallback},
			"a wait of zero":      {refusal, "no", "0", fallback},
			"a wait below zero":   {refusal, "no", "-1", fallback},
			"a wait not a number": {refusal, "no", "soon", fallback},
			"another reason":      {`{"error":"forward to recipient failed"}`, "no", "7", ""},
			"not from the Hub":    {refusal, "", "7", ""},
		}
		for name, row := range rows {
			t.Run(reason+"/"+name, func(t *testing.T) {
				env := newInProcessExchange(t)
				base := env.originator.cfg.Client.Transport
				env.originator.cfg.Client.Transport = diagnosticRoundTripper(func(r *http.Request) (*http.Response, error) {
					if !strings.HasSuffix(r.URL.Path, "/route") {
						return base.RoundTrip(r)
					}
					h := http.Header{"Content-Type": []string{"application/json"}}
					if row.delivered != "" {
						h.Set(HubDeliveredHeader, row.delivered)
					}
					if row.retryAfter != "" {
						h.Set("Retry-After", row.retryAfter)
					}
					return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader(row.body)), Header: h}, nil
				})
				rec := httptest.NewRecorder()
				env.originator.ingressRoute(cdexRoute(""))(rec, cdexPost(cdexSubmissionBody("MBR-COVERED", cdexPayerCMS), ""))
				if got := rec.Header().Get("Retry-After"); got != row.want {
					t.Errorf("Retry-After = %q, want %q (answer %d %.200s)", got, row.want, rec.Code, rec.Body)
				}
				if row.want != "" && (rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), reason)) {
					t.Errorf("answer %d %.200s, want 503 naming %q", rec.Code, rec.Body, reason)
				}
			})
		}
	}
}

// A requester that does not know a not-acted value, as a v1.0.0 gateway does not
// know at-capacity, still gives its caller a safe answer: 503, and the Hub's own
// reason, which says nothing was done and to resend. It states no wait of its
// own. withNotActed reads only the values it names, so a later value read here
// is that release's reading.
func TestIngressReadsANotActedValueItDoesNotKnowSafely(t *testing.T) {
	const reason = `{"error":"forward to recipient failed: the recipient's gateway has no room for this envelope and took no action; resend with a freshly sealed envelope"}`
	env := newInProcessExchange(t)
	base := env.originator.cfg.Client.Transport
	env.originator.cfg.Client.Transport = diagnosticRoundTripper(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/route") {
			return base.RoundTrip(r)
		}
		h := http.Header{"Content-Type": []string{"application/json"}}
		h.Set(HubDeliveredHeader, "no")
		h.Set(NotActedHeader, "a-value-from-a-later-release")
		h.Set("Retry-After", "5")
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader(reason)), Header: h}, nil
	})
	rec := httptest.NewRecorder()
	env.originator.withIngressCorrelation(env.originator.handleCRDIngress)(rec, env.crdIngressRequest(t))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "took no action; resend with a freshly sealed envelope") {
		t.Fatalf("got %d %s, want 503 saying nothing was done and to resend", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After = %q, want none: only a value it knows states a wait", got)
	}
	if got := rec.Header().Get(HubDeliveredHeader); got == "yes" {
		t.Fatalf("%s = %q on a leg the recipient did not act on", HubDeliveredHeader, got)
	}
}

// The recipient gateway's not-acted refusal, relayed by the Hub as a 503 not
// delivered carrying NotActedHeader, reaches the caller as a retryable 503
// that says nothing was done and when to resend. Each row that misses one
// condition (another status, another delivery value, another header value) is
// reported as it was before.
func TestIngressReportsTheRecipientsNotActedRefusal(t *testing.T) {
	t.Parallel()
	const reason = `{"error":"forward to recipient failed: the recipient does not know the sender yet and took no action; resend with a freshly sealed envelope"}`
	notActed := func(status int, delivered, value, retryAfter string) http.Header {
		h := http.Header{}
		h.Set("Content-Type", "application/json")
		h.Set(HubDeliveredHeader, delivered)
		h.Set(NotActedHeader, value)
		if retryAfter != "" {
			h.Set("Retry-After", retryAfter)
		}
		return h
	}
	rows := map[string]struct {
		status     int
		header     http.Header
		wantStatus int
		contains   string
		retryAfter string
	}{
		"relayed attestation": {503, notActed(503, "no", NotActedRequesterUnknown, "5"),
			http.StatusServiceUnavailable, errRecipientNotYetKnowing + "; resend after 5s", "5"},
		"no Retry-After": {503, notActed(503, "no", NotActedRequesterUnknown, ""),
			http.StatusServiceUnavailable, "resend after 3s", "3"},
		"Retry-After out of range": {503, notActed(503, "no", NotActedRequesterUnknown, "600"),
			http.StatusServiceUnavailable, "resend after 3s", "3"},
		"Retry-After not a number": {503, notActed(503, "no", NotActedRequesterUnknown, "Fri, 31 Dec 1999 23:59:59 GMT"),
			http.StatusServiceUnavailable, "resend after 3s", "3"},
		"at capacity": {503, notActed(503, "no", NotActedAtCapacity, "7"),
			http.StatusServiceUnavailable, errRecipientAtCapacity + "; resend after 7s", "7"},
		"at capacity, no Retry-After": {503, notActed(503, "no", NotActedAtCapacity, ""),
			http.StatusServiceUnavailable, errRecipientAtCapacity + "; resend after 5s", "5"},
		"another header value": {503, notActed(503, "no", "something-else", "5"),
			http.StatusServiceUnavailable, "hub refused the exchange: forward to recipient failed", ""},
		"not a 503": {502, notActed(502, "no", NotActedRequesterUnknown, "5"),
			http.StatusBadGateway, "hub refused the exchange: forward to recipient failed", ""},
		"delivery unknown": {503, notActed(503, "unknown", NotActedRequesterUnknown, "5"),
			http.StatusBadGateway, "the recipient may have received this request", ""},
		"not from the Hub": {503, func() http.Header {
			h := notActed(503, "", NotActedRequesterUnknown, "5")
			h.Del(HubDeliveredHeader)
			return h
		}(),
			http.StatusBadGateway, "the recipient may have received this request", ""},
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			env := newInProcessExchange(t)
			base := env.originator.cfg.Client.Transport
			env.originator.cfg.Client.Transport = diagnosticRoundTripper(func(r *http.Request) (*http.Response, error) {
				if !strings.HasSuffix(r.URL.Path, "/route") {
					return base.RoundTrip(r)
				}
				return &http.Response{StatusCode: row.status, Body: io.NopCloser(strings.NewReader(reason)), Header: row.header.Clone()}, nil
			})
			rec := httptest.NewRecorder()
			env.originator.withIngressCorrelation(env.originator.handleCRDIngress)(rec, env.crdIngressRequest(t))
			if rec.Code != row.wantStatus || !strings.Contains(rec.Body.String(), row.contains) {
				t.Fatalf("got %d %s, want %d containing %q", rec.Code, rec.Body.String(), row.wantStatus, row.contains)
			}
			if got := rec.Header().Get("Retry-After"); got != row.retryAfter {
				t.Errorf("Retry-After = %q, want %q", got, row.retryAfter)
			}
		})
	}
}
