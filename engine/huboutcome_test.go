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
		// A Hub 200 comes only after the recipient answered.
		"Hub 200 that is not an envelope": {route: &answer{200, `not an envelope`, nil},
			status: http.StatusBadGateway, contains: "its answer could not be accepted (the Hub's answer is not an envelope)"},
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
// verification — is reported as delivered-and-lost: the payer acted.
func TestIngressAnswerThatFailsVerificationIsReportedAsDelivered(t *testing.T) {
	env := newInProcessExchange(t)
	env.substrate.mutateResp = func(b []byte) []byte {
		return bytes.Replace(b, []byte(`"authzToken":"`), []byte(`"authzToken":"x`), 1)
	}
	rec := httptest.NewRecorder()
	env.originator.withIngressCorrelation(env.originator.handleCRDIngress)(rec, env.crdIngressRequest(t))
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "its answer could not be accepted (response leg authorization failed)") {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

// The recipient gateway's not-acted refusal, relayed by the Hub as a 503 not
// delivered carrying NotActedHeader, reaches the caller as a retryable 503
// that says nothing was done and when to resend. Each row that misses one
// condition (another status, another delivery value, another header value) is
// reported as it was before.
func TestIngressReportsTheRecipientsNotActedRefusal(t *testing.T) {
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
