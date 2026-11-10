//go:build !shn_coreonly

package engine

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// workstream_pa_authz_failure_cause_test.go: a request leg no authority could be obtained
// for is refused on authority before it is sent, at every conformance level,
// and its caller is told why, by the Framework's status and its class alone,
// and that nothing was sent: never the Framework's body, and no header of its
// but a well-formed Retry-After on a 429.

// answerWith answers an /authorize call with status, body and the extra
// headers.
func answerWith(status int, body string, header http.Header) func(*http.Request) (*http.Response, error) {
	return func(*http.Request) (*http.Response, error) {
		h := http.Header{"Content-Type": []string{"application/json"}}
		for k, v := range header {
			h[k] = v
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: h}, nil
	}
}

// failingBody fails every read after its status has arrived.
type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (failingBody) Close() error             { return nil }

// authzUnreadableAnswer answers an /authorize call with status and a body that cannot be read.
func authzUnreadableAnswer(status int) func(*http.Request) (*http.Response, error) {
	return func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Body: failingBody{}, Header: http.Header{}}, nil
	}
}

const (
	wantAuthzUnreadable  = "the authorization service's answer could not be read, so this leg was not sent"
	wantAuthzFailedPlain = "the authorization service failed, so this leg was not sent"
	wantAuthzLimited     = "the authorization service is limiting this gateway, so this leg was not sent"
	wantAuthzNotRecorded = "the authorization service could not record its decision, so this leg was not sent"
	wantAuthzNoToken     = "the authorization service's answer carried no token this leg can use, so this leg was not sent"
)

func wantAuthzFailed(status string) string {
	return "the authorization service failed (answered " + status + "), so this leg was not sent"
}

func wantAuthzRefusedThisGateway(status string) string {
	return "the authorization service refused this gateway's request (answered " + status + "), so this leg was not sent"
}

// The Framework's answer, by class, at every level: its status and class reach
// the caller with the statement that nothing was sent; the leg is refused on
// authority by this gateway (on audit by the Framework, where it states that
// rule), and never reaches the Hub.
func TestRequestLeg_AuthorizeFailureAnswersItsCause(t *testing.T) {
	t.Parallel()
	retryAfter := func(v string) http.Header { return http.Header{"Retry-After": []string{v}} }
	rows := []struct {
		name       string
		fault      func(*http.Request) (*http.Response, error)
		status     int
		msg        string
		retryAfter string // the Retry-After the caller gets; "" none
		by, rule   string
		class      string
	}{
		{"a 500", answer(http.StatusInternalServerError, `{"error":"policy error"}`), http.StatusBadGateway, wantAuthzFailed("500"), "", RefusedByProviderGateway, RefusalAuthority, ExchangeOther},
		{"a 502 stating no rule", answer(http.StatusBadGateway, `{"error":"decision audit failed"}`), http.StatusBadGateway, wantAuthzFailed("502"), "", RefusedByProviderGateway, RefusalAuthority, ExchangeOther},
		{"a 501", answer(http.StatusNotImplemented, ``), http.StatusBadGateway, wantAuthzFailed("501"), "", RefusedByProviderGateway, RefusalAuthority, ExchangeOther},
		{"a 302", answer(http.StatusFound, ``), http.StatusBadGateway, wantAuthzFailed("302"), "", RefusedByProviderGateway, RefusalAuthority, ExchangeOther},
		{"a 400", answer(http.StatusBadRequest, `{"error":"bad assertion"}`), http.StatusBadGateway, wantAuthzRefusedThisGateway("400"), "", RefusedByProviderGateway, RefusalAuthority, ExchangeOther},
		{"a 401", answer(http.StatusUnauthorized, `{"error":"assertion expired"}`), http.StatusBadGateway, wantAuthzRefusedThisGateway("401"), "", RefusedByProviderGateway, RefusalAuthority, ExchangeOther},
		{"a 404", answer(http.StatusNotFound, ``), http.StatusBadGateway, wantAuthzRefusedThisGateway("404"), "", RefusedByProviderGateway, RefusalAuthority, ExchangeOther},
		{"the audit rule stated", answer(http.StatusBadGateway, `{"error":"decision audit failed","rule":"audit"}`), http.StatusBadGateway, wantAuthzNotRecorded, "", RefusedByAuthorizationFramework, RefusalAudit, ""}, // the Framework's refusal: no authorize-error class
		{"no token", answer(http.StatusOK, `{}`), http.StatusBadGateway, wantAuthzNoToken, "", RefusedByProviderGateway, RefusalAuthority, ExchangeOther},
		{"a null token", answer(http.StatusOK, `{"token":null}`), http.StatusBadGateway, wantAuthzNoToken, "", RefusedByProviderGateway, RefusalAuthority, ExchangeOther},
		{"a token with no signature", answer(http.StatusOK, `{"token":{"operation":"order-select","holder":"provider"}}`), http.StatusBadGateway, wantAuthzNoToken, "", RefusedByProviderGateway, RefusalAuthority, ExchangeOther},
		{"an answer that is not JSON", answer(http.StatusOK, `not json`), http.StatusBadGateway, wantAuthzNoToken, "", RefusedByProviderGateway, RefusalAuthority, ExchangeOther},
		{"a token that does not verify", answer(http.StatusOK, `{"token":{"operation":"order-select","holder":"provider","signature":"AAAA"}}`), http.StatusBadGateway, wantAuthzNoToken, "", RefusedByProviderGateway, RefusalAuthority, ExchangeOther},
		{"an answer that could not be read", authzUnreadableAnswer(http.StatusOK), http.StatusBadGateway, wantAuthzUnreadable, "", RefusedByProviderGateway, RefusalAuthority, ExchangeOther},
		{"a 500 that could not be read", authzUnreadableAnswer(http.StatusInternalServerError), http.StatusBadGateway, wantAuthzUnreadable, "", RefusedByProviderGateway, RefusalAuthority, ExchangeOther},
		// A 429 is the Framework limiting this gateway: transient, as for an
		// authorization service that cannot be reached, and its Retry-After
		// reaches the caller only as it was sent and well-formed.
		{"a 429 with a Retry-After in seconds", answerWith(http.StatusTooManyRequests, ``, retryAfter("7")), http.StatusServiceUnavailable, wantAuthzLimited, "7", RefusedByProviderGateway, RefusalAuthority, ExchangeUnreachable},
		{"a 429 with a Retry-After date", answerWith(http.StatusTooManyRequests, ``, retryAfter("Wed, 21 Oct 2026 07:28:00 GMT")), http.StatusServiceUnavailable, wantAuthzLimited, "Wed, 21 Oct 2026 07:28:00 GMT", RefusedByProviderGateway, RefusalAuthority, ExchangeUnreachable},
		{"a 429 with no Retry-After", answer(http.StatusTooManyRequests, `{"error":"slow down"}`), http.StatusServiceUnavailable, wantAuthzLimited, "", RefusedByProviderGateway, RefusalAuthority, ExchangeUnreachable},
		{"a 429 with a Retry-After that is no delay", answerWith(http.StatusTooManyRequests, ``, retryAfter("soon")), http.StatusServiceUnavailable, wantAuthzLimited, "", RefusedByProviderGateway, RefusalAuthority, ExchangeUnreachable},
		{"a 429 with a negative Retry-After", answerWith(http.StatusTooManyRequests, ``, retryAfter("-5")), http.StatusServiceUnavailable, wantAuthzLimited, "", RefusedByProviderGateway, RefusalAuthority, ExchangeUnreachable},
		{"a 429 with a fractional Retry-After", answerWith(http.StatusTooManyRequests, ``, retryAfter("1.5")), http.StatusServiceUnavailable, wantAuthzLimited, "", RefusedByProviderGateway, RefusalAuthority, ExchangeUnreachable},
		{"a 429 with an empty Retry-After", answerWith(http.StatusTooManyRequests, ``, retryAfter("")), http.StatusServiceUnavailable, wantAuthzLimited, "", RefusedByProviderGateway, RefusalAuthority, ExchangeUnreachable},
		{"a 429 with a Retry-After date in an obsolete form", answerWith(http.StatusTooManyRequests, ``, retryAfter("Wednesday, 21-Oct-26 07:28:00 GMT")), http.StatusServiceUnavailable, wantAuthzLimited, "", RefusedByProviderGateway, RefusalAuthority, ExchangeUnreachable},
		{"a 429 with a Retry-After date on the wrong weekday", answerWith(http.StatusTooManyRequests, ``, retryAfter("Thu, 21 Oct 2026 07:28:00 GMT")), http.StatusServiceUnavailable, wantAuthzLimited, "", RefusedByProviderGateway, RefusalAuthority, ExchangeUnreachable},
		{"a 429 with a Retry-After date in another zone", answerWith(http.StatusTooManyRequests, ``, retryAfter("Wed, 21 Oct 2026 07:28:00 UTC")), http.StatusServiceUnavailable, wantAuthzLimited, "", RefusedByProviderGateway, RefusalAuthority, ExchangeUnreachable},
		{"a 429 with two Retry-After values", answerWith(http.StatusTooManyRequests, ``, http.Header{"Retry-After": []string{"7", "9"}}), http.StatusServiceUnavailable, wantAuthzLimited, "", RefusedByProviderGateway, RefusalAuthority, ExchangeUnreachable},
	}
	for _, level := range allLevels {
		for _, row := range rows {
			t.Run(level.String()+"/"+row.name, func(t *testing.T) {
				env := newInProcessExchange(t)
				env.originator.cfg.ConformanceEnforcement = level
				var got exchangeRecords
				env.originator.cfg.ExchangeObserved = got.observe
				env.payerReturns(LegResult{})
				env.substrate.authorizeFaults = []func(*http.Request) (*http.Response, error){row.fault, row.fault}
				rec := httptest.NewRecorder()
				env.originator.ingressRoute(RouteCRD)(rec, env.crdIngressRequest(t))
				var body struct {
					Error string `json:"error"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != row.status || body.Error != row.msg {
					t.Fatalf("answer %d %s, want %d %q", rec.Code, rec.Body.String(), row.status, row.msg)
				}
				if got := rec.Header().Values("Retry-After"); (row.retryAfter == "" && len(got) != 0) || (row.retryAfter != "" && (len(got) != 1 || got[0] != row.retryAfter)) {
					t.Fatalf("Retry-After %q, want %q", got, row.retryAfter)
				}
				if env.substrate.authorizeHits != 1 {
					t.Fatalf("authorize calls %d, want 1: an answer is never retried", env.substrate.authorizeHits)
				}
				if n := env.routeHitCount(); n != 0 {
					t.Fatalf("the request crossed to the Hub (%d)", n)
				}
				r := got.only(t)
				if r.Outcome != ExchangeRefused || r.RefusedBy != row.by || r.Rule != row.rule || r.AuthorizeError != row.class {
					t.Fatalf("record %s by %q rule %q class %q, want refused by %q on %q, class %q", r.Outcome, r.RefusedBy, r.Rule, r.AuthorizeError, row.by, row.rule, row.class)
				}
			})
		}
	}
}

// Only the Framework's status and its class reach the caller: a marker in the
// Framework's body, or in any header of its answer, never does, and no header
// of its answer but a well-formed Retry-After on a 429 is carried.
func TestRequestLeg_AuthorizeFailureCarriesNothingOfTheFrameworksAnswer(t *testing.T) {
	t.Parallel()
	const marker = "SHN-980-FRAMEWORK-MARKER"
	leaky := http.Header{"X-Framework-Debug": []string{marker}, "Www-Authenticate": []string{`Bearer error="` + marker + `"`}, "Retry-After": []string{"3"}}
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway} {
		for _, body := range []string{
			`{"error":"` + marker + `"}`,
			`{"error":"decision audit failed ` + marker + `","rule":"audit"}`,
			marker,
		} {
			for _, level := range allLevels {
				t.Run(level.String()+"/"+http.StatusText(status)+"/"+body, func(t *testing.T) {
					env := newInProcessExchange(t)
					env.originator.cfg.ConformanceEnforcement = level
					env.payerReturns(LegResult{})
					fault := answerWith(status, body, leaky)
					env.substrate.authorizeFaults = []func(*http.Request) (*http.Response, error){fault, fault}
					rec := httptest.NewRecorder()
					env.originator.ingressRoute(RouteCRD)(rec, env.crdIngressRequest(t))
					if strings.Contains(rec.Body.String(), marker) {
						t.Fatalf("the caller's answer carries the Framework's body: %s", rec.Body.String())
					}
					for k, vs := range rec.Header() {
						for _, v := range vs {
							if strings.Contains(v, marker) {
								t.Fatalf("the caller's answer carries the Framework's header %s: %s", k, v)
							}
						}
					}
					if rec.Header().Get("X-Framework-Debug") != "" || rec.Header().Get("Www-Authenticate") != "" {
						t.Fatalf("the caller's answer carries a header of the Framework's answer: %v", rec.Header())
					}
					if ra := rec.Header().Values("Retry-After"); (status == http.StatusTooManyRequests) != (len(ra) == 1 && ra[0] == "3") {
						t.Fatalf("Retry-After %q on a %d", ra, status)
					}
					if n := env.routeHitCount(); n != 0 {
						t.Fatalf("the request crossed to the Hub (%d)", n)
					}
				})
			}
		}
	}
}

// An authorize call that fails in no class above (here, one this gateway could
// not even make) is answered that the authorization service failed, with no
// status, at every level: nothing was sent, recorded as today.
func TestRequestLeg_AnUnclassifiedAuthorizeFailureAnswersFailed(t *testing.T) {
	t.Parallel()
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.ConformanceEnforcement = level
			env.originator.cfg.AuthzURL = "http://%zz"
			var got exchangeRecords
			env.originator.cfg.ExchangeObserved = got.observe
			env.payerReturns(LegResult{})
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteCRD)(rec, env.crdIngressRequest(t))
			var body struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusBadGateway || body.Error != wantAuthzFailedPlain {
				t.Fatalf("answer %d %s, want 502 %q", rec.Code, rec.Body.String(), wantAuthzFailedPlain)
			}
			if n := env.routeHitCount(); n != 0 {
				t.Fatalf("the request crossed to the Hub (%d)", n)
			}
			r := got.only(t)
			if r.Outcome != ExchangeRefused || r.RefusedBy != RefusedByProviderGateway || r.Rule != RefusalAuthority || r.AuthorizeError != ExchangeOther {
				t.Fatalf("record %s by %q rule %q class %q, want refused by the provider gateway on authority, other", r.Outcome, r.RefusedBy, r.Rule, r.AuthorizeError)
			}
		})
	}
}

// The FHIR operation routes answer each class through the same chokepoint, as
// an OperationOutcome naming it: the 429 a transient 503 with the Framework's
// well-formed Retry-After, the others a 502.
func TestRequestLeg_AuthorizeFailureOnTheFHIROperationRoutes(t *testing.T) {
	t.Parallel()
	routes := []struct {
		route string
		req   func(t *testing.T) *http.Request
		sor   bool
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
	}
	classes := []struct {
		name       string
		fault      func(*http.Request) (*http.Response, error)
		status     int
		msg        string
		retryAfter string
	}{
		{"a 429", answerWith(http.StatusTooManyRequests, ``, http.Header{"Retry-After": []string{"7"}}), http.StatusServiceUnavailable, wantAuthzLimited, "7"},
		{"a 500", answer(http.StatusInternalServerError, `{"error":"policy error"}`), http.StatusBadGateway, wantAuthzFailed("500"), ""},
		{"the audit rule stated", answer(http.StatusBadGateway, `{"error":"decision audit failed","rule":"audit"}`), http.StatusBadGateway, wantAuthzNotRecorded, ""},
		{"no token", answer(http.StatusOK, `{}`), http.StatusBadGateway, wantAuthzNoToken, ""},
	}
	for _, level := range allLevels {
		for _, row := range routes {
			for _, c := range classes {
				t.Run(level.String()+"/"+row.route+"/"+c.name, func(t *testing.T) {
					env := newInProcessExchange(t)
					env.originator.cfg.ConformanceEnforcement = level
					if row.sor {
						env.originator.cfg.SoR = newPrefetchSoR().sor()
						declareFramedDTR(t, env, true)
					}
					if row.route == RouteFederatedQuery {
						env = newFQExchange(t, level)
					}
					env.payerReturns(LegResult{})
					env.substrate.authorizeFaults = []func(*http.Request) (*http.Response, error){c.fault, c.fault}
					rec := httptest.NewRecorder()
					env.originator.ingressRoute(row.route)(rec, row.req(t))
					var oo struct {
						ResourceType string `json:"resourceType"`
						Issue        []struct {
							Diagnostics string `json:"diagnostics"`
						} `json:"issue"`
					}
					if rec.Code != c.status || json.Unmarshal(rec.Body.Bytes(), &oo) != nil || oo.ResourceType != "OperationOutcome" ||
						len(oo.Issue) != 1 || oo.Issue[0].Diagnostics != c.msg {
						t.Fatalf("answer %d %s, want %d an OperationOutcome %q", rec.Code, rec.Body.String(), c.status, c.msg)
					}
					if got := rec.Header().Get("Retry-After"); got != c.retryAfter {
						t.Fatalf("Retry-After %q, want %q", got, c.retryAfter)
					}
					if env.routeHitCount() != 0 {
						t.Fatal("the request crossed to the Hub")
					}
				})
			}
		}
	}
}

// A 429 is counted as the Framework unavailable: the leg's outcome is
// unreachable, and the authorize call's token outcome unavailable, as for a
// Framework that cannot be reached.
func TestRequestLeg_AuthorizeLimitedIsCountedUnreachable(t *testing.T) {
	t.Parallel()
	env := newInProcessExchange(t)
	var legs, tokens []string
	env.originator.cfg.LegMetric = func(outcome string) { legs = append(legs, outcome) }
	env.originator.cfg.TokenMetric = func(outcome string, _ time.Duration) { tokens = append(tokens, outcome) }
	env.payerReturns(LegResult{})
	f := answerWith(http.StatusTooManyRequests, ``, http.Header{"Retry-After": []string{"7"}})
	env.substrate.authorizeFaults = []func(*http.Request) (*http.Response, error){f, f}
	env.originator.ingressRoute(RouteCRD)(httptest.NewRecorder(), env.crdIngressRequest(t))
	if len(legs) != 2 || legs[0] != LegOutcomeRouted || legs[1] != LegOutcomeUnreachable {
		t.Fatalf("leg outcomes %v, want [routed unreachable]", legs)
	}
	if len(tokens) != 1 || tokens[0] != TokenUnavailable {
		t.Fatalf("token outcomes %v, want one %s", tokens, TokenUnavailable)
	}
}
