//go:build !shn_coreonly

package engine

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

// credentialRoutes are the routes whose answers may carry a reusable
// credential or session (contentcoding's package doc): the ingress's token
// endpoint, which issues a bearer. Every operator route carries one too (a
// scenario's resume token, which completes or cancels its pended request
// until it ends, repeated on every answer about it), so the operator
// surface is declared by its flag, not listed here.
var credentialRoutes = []string{"POST /oauth/token"}

// TestRoutes_EveryRouteStatesItsCredential: every route each role mounts,
// under every condition that gates one, states whether its answers carry a
// reusable credential, and exactly credentialRoutes say they do. A route that
// states nothing is never compressed, and fails here.
func TestRoutes_EveryRouteStatesItsCredential(t *testing.T) {
	seen, operators := map[string]bool{}, 0
	for _, role := range []string{"provider", "payer", "facility", "phg"} {
		for _, c := range everyRouteCondition() {
			g := routeGateway(role, c)
			for _, declared := range [][]mountedRoute{g.coreRoutes(role), g.workstreamRoutes(role), g.moduleRoutes(role)} {
				for _, rt := range declared {
					seen[rt.pattern] = true
					if rt.operator {
						operators++
					}
					switch want := slices.Contains(credentialRoutes, rt.pattern) || rt.operator; {
					case rt.credential == credentialUnstated:
						t.Errorf("%s (%s, %s) does not state whether it carries a credential", rt.pattern, role, c)
					case want && rt.credential != carriesCredential:
						t.Errorf("%s (%s, %s) issues a credential but declares none", rt.pattern, role, c)
					case !want && rt.credential != noCredential:
						t.Errorf("%s (%s, %s) declares a credential; add it to credentialRoutes if it carries one", rt.pattern, role, c)
					}
				}
			}
		}
	}
	for _, p := range credentialRoutes {
		if !seen[p] {
			t.Errorf("control: %s is mounted under no condition; the check is vacuous for it", p)
		}
	}
	if operators == 0 {
		t.Error("control: no operator route is mounted under any condition; the check is vacuous for them")
	}
}

// TestIngressToken_AnswerIsNeverCompressed: the token endpoint's answer, a
// bearer, is never gzip-coded, even for a caller that accepts gzip, and
// does not vary by Accept-Encoding: it is not wrapped at all. Control: the
// same gateway's SMART configuration is.
func TestIngressToken_AnswerIsNeverCompressed(t *testing.T) {
	priv, pub := newTestClientKey(t)
	g := gatewayWithAuth(t, "br-provider", pub)
	now := ingressFixedClock()()
	assertion := mintAssertion(t, priv, jwt.SigningMethodES384, validClaims("br-provider", g.ingressAuth.tokenURL(), now))
	h := &acceptsGzip{g.Handler()}
	rec := postToken(t, h, assertion)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "access_token") {
		t.Fatalf("token: %d %s", rec.Code, rec.Body.String())
	}
	if ce, v := rec.Header().Get("Content-Encoding"), rec.Header().Get("Vary"); ce != "" || v != "" {
		t.Fatalf("token answer: Content-Encoding %q, Vary %q", ce, v)
	}
	control := httptest.NewRecorder()
	h.ServeHTTP(control, httptest.NewRequest(http.MethodGet, testIngressBaseURL+"/.well-known/smart-configuration", nil))
	if control.Code != http.StatusOK || control.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("control: %d, Vary %q", control.Code, control.Header().Get("Vary"))
	}
}

// codedIngressPost is ingressPostTo with its body sent in coding, served
// through the route as mounted (codedRoute).
func codedIngressPost(env *inProcessExchange, route, coding string, body []byte) *httptest.ResponseRecorder {
	r := ingressPostTo(route, body)
	r.Header.Set("Content-Encoding", coding)
	rec := httptest.NewRecorder()
	codedRoute(int64(len(ingressLimitBody(route))-1), noCredential, env.originator.ingressRoute(route)).ServeHTTP(rec, r)
	return rec
}

// Every provider ingress route refuses a body in a coding the gateway does
// not decode, or in a stack of codings, 415 naming gzip, recorded as the
// provider gateway's refusal on message integrity, at every level, and sends
// nothing. It is a network rule: the bytes cannot be read as their sender
// declared them.
func TestIngress_AnUnsupportedCodingIs415(t *testing.T) {
	t.Parallel()
	for _, route := range workstreamIngressRouteNames() {
		for _, level := range allLevels {
			for _, coding := range []string{"br", "gzip, gzip"} {
				t.Run(route+"/"+coding+"/"+level.String(), func(t *testing.T) {
					env := newInProcessExchange(t)
					env.originator.cfg.ConformanceEnforcement = level
					var got exchangeRecords
					env.originator.cfg.ExchangeObserved = got.observe
					rec := codedIngressPost(env, route, coding, []byte(`{}`))
					wantUnsupportedCoding(t, rec)
					refusedBeforeTheNetwork(t, env, rec, http.StatusUnsupportedMediaType, errUnsupportedCoding)
					wantRefusal(t, got.only(t), http.StatusUnsupportedMediaType, RefusedByProviderGateway, RefusalIntegrity)
				})
			}
		}
	}
}

// An ingress caller with no bearer is refused on authentication, 401, before
// its body is read: a body in a coding the gateway does not decode is never
// reached. The token endpoint is the exception, its caller's credential being
// in the form (TestIngressToken_ContentCoding).
func TestIngress_AnUnauthenticatedCallerIsRefusedBeforeTheCoding(t *testing.T) {
	_, pub := newTestClientKey(t)
	g := gatewayWithAuth(t, "br-provider", pub)
	posts := 0
	for _, rt := range g.workstreamRoutes("provider") {
		method, path, _ := strings.Cut(rt.pattern, " ")
		if method != http.MethodPost || rt.credential == carriesCredential {
			continue
		}
		posts++
		path = strings.ReplaceAll(path, "{id}", "shn-order-select")
		r := httptest.NewRequest(method, testIngressBaseURL+path, strings.NewReader(`{}`))
		r.Header.Set("Content-Encoding", "br")
		rec := httptest.NewRecorder()
		g.Handler().ServeHTTP(rec, r)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d %s, want 401", rt.pattern, rec.Code, rec.Body.String())
		}
	}
	if posts == 0 {
		t.Fatal("control: no ingress POST route is mounted")
	}
}

// codedLimitBodies is body and body less its last byte, gzip-coded once however
// many routes share them: compressing a body at the attachment ceiling is dear.
var codedLimitBodies = func() func(*testing.T, []byte) ([]byte, []byte) {
	type pair struct{ over, at []byte }
	done := map[int]pair{}
	return func(t *testing.T, body []byte) ([]byte, []byte) {
		t.Helper()
		p, ok := done[len(body)]
		if !ok {
			p = pair{gzipT(t, body), gzipT(t, body[:len(body)-1])}
			done[len(body)] = p
		}
		return p.over, p.at
	}
}()

// A gzip-coded body is read decoded, through the one bounded read: decoded
// past the route's limit (a small body that inflates past it) it is the
// 413 an identity body over the limit is, and decoded at the limit it is
// read whole. A body that is not a whole gzip stream is the 400 any body that
// cannot be read is.
func TestIngress_AGzipBodyIsReadDecodedUpToTheLimit(t *testing.T) {
	t.Parallel()
	for _, route := range workstreamIngressRouteNames() {
		t.Run(route, func(t *testing.T) {
			body := ingressLimitBody(route)
			over, at := codedLimitBodies(t, body)
			if len(over) > 1<<16 {
				t.Fatalf("control: the coded body is %d bytes; the row is about a small body that inflates", len(over))
			}
			env := newInProcessExchange(t)
			var got exchangeRecords
			env.originator.cfg.ExchangeObserved = got.observe
			rec := codedIngressPost(env, route, "gzip", over)
			want := `"diagnostics":"` + errRequestTooLarge + `"`
			if route == RouteCRD {
				want = `{"error":"` + errRequestTooLarge + `"}`
			}
			refusedBeforeTheNetwork(t, env, rec, http.StatusRequestEntityTooLarge, want)
			wantRefusal(t, got.only(t), http.StatusRequestEntityTooLarge, RefusedByProviderGateway, RefusalRouting)

			if rec := codedIngressPost(newInProcessExchange(t), route, "gzip", at); rec.Code == http.StatusRequestEntityTooLarge ||
				strings.Contains(rec.Body.String(), "read body failed") {
				t.Fatalf("decoded at the limit: %d %s; want it read whole", rec.Code, rec.Body.String())
			}
			if rec := codedIngressPost(newInProcessExchange(t), route, "gzip", []byte(`{}`)); rec.Code != http.StatusBadRequest ||
				!strings.Contains(rec.Body.String(), "read body failed") {
				t.Fatalf("not a gzip stream: %d %s; want 400 read body failed", rec.Code, rec.Body.String())
			}
		})
	}
}

// The payer's gateway refuses an envelope in a coding it does not decode 415
// naming gzip, recorded as its refusal on message integrity, at every level,
// and calls nothing; the same envelope gzip-coded is carried, answered as
// the uncoded one is.
func TestInbound_ContentCoding(t *testing.T) {
	t.Parallel()
	answer := eligResponses(t, "Patient/"+dtrFrameMember)
	for _, level := range allLevels {
		t.Run("unsupported/"+level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, true)
			p.partner.respByPath[eligibilityPath] = answer
			var got exchangeRecords
			p.g.cfg.ExchangeObserved = got.observe
			p.coding = "br"
			ans := p.send(t, "coverage-eligibility", "", eligibilityRequest(t, dtrFrameMember))
			if ans.status != http.StatusUnsupportedMediaType || string(bytes.TrimSpace(ans.body)) != `{"error":"`+errUnsupportedCoding+`"}` ||
				ans.header.Get("Accept-Encoding") != "gzip" || p.partner.calls.Load() != 0 {
				t.Fatalf("answer %d %s (Accept-Encoding %q, payer system calls %d), want 415 naming gzip", ans.status, ans.body, ans.header.Get("Accept-Encoding"), p.partner.calls.Load())
			}
			wantRefusal(t, got.only(t), http.StatusUnsupportedMediaType, RefusedByPayerGateway, RefusalIntegrity)
		})
		t.Run("unauthenticated/"+level.String(), func(t *testing.T) {
			// A delivery with no valid Hub assertion is refused on
			// authentication, 403, before its body is read: its coding is
			// never reached.
			p := eligibilityPayer(t, level, true)
			var got exchangeRecords
			p.g.cfg.ExchangeObserved = got.observe
			r := httptest.NewRequest(http.MethodPost, "/substrate/inbound", strings.NewReader(`{}`))
			r.Header.Set("X-Hub-Assertion", "not-an-assertion")
			r.Header.Set("Content-Encoding", "br")
			rec := httptest.NewRecorder()
			codedRoute(p.g.coreRoutes("payer")[0].bodyLimit, noCredential, p.g.inboundRoute()).ServeHTTP(rec, r)
			if rec.Code != http.StatusForbidden || p.partner.calls.Load() != 0 {
				t.Fatalf("answer %d %s (payer system calls %d), want 403", rec.Code, rec.Body.String(), p.partner.calls.Load())
			}
			wantRefusal(t, got.only(t), http.StatusForbidden, RefusedByPayerGateway, RefusalAuthentication)
		})
		t.Run("gzip/"+level.String(), func(t *testing.T) {
			control := eligibilityPayer(t, level, true)
			control.partner.respByPath[eligibilityPath] = answer
			want := control.send(t, "coverage-eligibility", "", eligibilityRequest(t, dtrFrameMember))
			p := eligibilityPayer(t, level, true)
			p.partner.respByPath[eligibilityPath] = answer
			p.coding = "gzip"
			p.envelope = func(raw []byte) []byte { return gzipT(t, raw) }
			ans := p.send(t, "coverage-eligibility", "", eligibilityRequest(t, dtrFrameMember))
			if ans.status != want.status || !bytes.Equal(ans.body, want.body) || p.partner.calls.Load() != 1 {
				t.Fatalf("gzip: %d %s (calls %d); uncoded: %d %s", ans.status, ans.body, p.partner.calls.Load(), want.status, want.body)
			}
		})
	}
}

// The ingress token endpoint refuses a form in a coding it does not decode
// 415 naming gzip, and reads a gzip-coded form decoded.
func TestIngressToken_ContentCoding(t *testing.T) {
	priv, pub := newTestClientKey(t)
	g := gatewayWithAuth(t, "br-provider", pub)
	now := ingressFixedClock()()
	post := func(coding string, encode func([]byte) []byte) *httptest.ResponseRecorder {
		form := url.Values{
			"grant_type":            {"client_credentials"},
			"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
			"client_assertion":      {mintAssertion(t, priv, jwt.SigningMethodES384, validClaims("br-provider", g.ingressAuth.tokenURL(), now))},
		}
		req := httptest.NewRequest(http.MethodPost, testIngressBaseURL+"/oauth/token", bytes.NewReader(encode([]byte(form.Encode()))))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Content-Encoding", coding)
		w := httptest.NewRecorder()
		g.Handler().ServeHTTP(w, req)
		return w
	}
	rec := post("br", func(b []byte) []byte { return b })
	if rec.Code != http.StatusUnsupportedMediaType || rec.Header().Get("Accept-Encoding") != "gzip" || !strings.Contains(rec.Body.String(), errUnsupportedCoding) {
		t.Fatalf("br: %d (Accept-Encoding %q) %s, want 415 naming gzip", rec.Code, rec.Header().Get("Accept-Encoding"), rec.Body.String())
	}
	if rec := post("gzip", func(b []byte) []byte { return gzipT(t, b) }); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "access_token") {
		t.Fatalf("gzip: %d %s, want a token", rec.Code, rec.Body.String())
	}
	// ParseForm reads a body only under a form Content-Type: a coded body under
	// another, with the credentials in the query, is refused all the same.
	query := url.Values{
		"grant_type":            {"client_credentials"},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {mintAssertion(t, priv, jwt.SigningMethodES384, validClaims("br-provider", g.ingressAuth.tokenURL(), now))},
	}
	req := httptest.NewRequest(http.MethodPost, testIngressBaseURL+"/oauth/token?"+query.Encode(), strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "br")
	w := httptest.NewRecorder()
	g.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnsupportedMediaType || strings.Contains(w.Body.String(), "access_token") {
		t.Fatalf("br under another Content-Type: %d %s, want 415 and no token", w.Code, w.Body.String())
	}
}
