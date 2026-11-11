package smartauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// hopServer is a server that records the Authorization each request carried,
// answering with answer(r, w).
type hopServer struct {
	*httptest.Server
	mu   sync.Mutex
	auth []string
}

func newHopServer(t *testing.T, answer func(r *http.Request, w http.ResponseWriter)) *hopServer {
	t.Helper()
	h := &hopServer{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		h.mu.Lock()
		h.auth = append(h.auth, r.Header.Get("Authorization"))
		h.mu.Unlock()
		answer(r, w)
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *hopServer) seen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.auth)
}

func redirectTo(url string) func(*http.Request, http.ResponseWriter) {
	return func(r *http.Request, w http.ResponseWriter) {
		http.Redirect(w, r, url, http.StatusTemporaryRedirect)
	}
}

func okAnswer(_ *http.Request, w http.ResponseWriter) { _, _ = io.WriteString(w, "ok") }

// get sends a bodiless GET to url through hc: a request that follows a
// redirect to another origin (one that carries a body never does).
func get(t *testing.T, ctx context.Context, hc *http.Client, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// A redirect to another path of the same origin keeps the bearer: the token
// was issued for that origin. The hop is recorded as same-origin.
func TestRedirect_SameOriginKeepsTheBearer(t *testing.T) {
	var tokens int32
	ts := countingTokenServer(t, &tokens, 0)
	var srv *hopServer
	srv = newHopServer(t, func(r *http.Request, w http.ResponseWriter) {
		if r.URL.Path == "/first" {
			http.Redirect(w, r, srv.URL+"/second", http.StatusTemporaryRedirect)
			return
		}
		okAnswer(r, w)
	})
	ctx, obs := WithRedirectObservation(context.Background())
	resp := post(t, ctx, refreshClient(t, ts.URL), srv.URL+"/first", []byte(`{}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !slices.Equal(srv.seen(), []string{"Bearer AT-1", "Bearer AT-1"}) {
		t.Fatalf("answer %d, bearers %q; want 200 with the bearer on both hops", resp.StatusCode, srv.seen())
	}
	if want := []string{RedirectSameOrigin}; !slices.Equal(obs.Hops(), want) {
		t.Fatalf("hops %q, want %q", obs.Hops(), want)
	}
}

// A bodiless request's redirect to another origin (another host, or another
// port) is followed without Authorization, at every hop of a chain, and its
// answer is the answer. The hops are recorded as cross-origin.
func TestRedirect_CrossOriginNeverCarriesTheBearer(t *testing.T) {
	var tokens int32
	ts := countingTokenServer(t, &tokens, 0)
	last := newHopServer(t, okAnswer)
	middle := newHopServer(t, redirectTo(last.URL+"/end"))
	first := newHopServer(t, redirectTo(middle.URL+"/next"))
	ctx, obs := WithRedirectObservation(context.Background())
	resp := get(t, ctx, refreshClient(t, ts.URL), first.URL+"/start")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(b) != "ok" {
		t.Fatalf("answer %d %q, want the last origin's answer", resp.StatusCode, b)
	}
	if !slices.Equal(first.seen(), []string{"Bearer AT-1"}) || !slices.Equal(middle.seen(), []string{""}) || !slices.Equal(last.seen(), []string{""}) {
		t.Fatalf("bearers: first %q, middle %q, last %q; want the bearer on the first origin only", first.seen(), middle.seen(), last.seen())
	}
	if want := []string{RedirectCrossOrigin, RedirectCrossOrigin}; !slices.Equal(obs.Hops(), want) {
		t.Fatalf("hops %q, want %q", obs.Hops(), want)
	}
}

// A chain that leaves the token's origin and comes back to it carries no
// bearer on the hop back, nor on any later hop: once the chain has left the
// origin, another origin chose where the hop goes (any path, and with a 307
// or 308 the method and body), so the token issued for the origin is not sent
// for it. The hop back is recorded as same-origin. A 401 the origin answers
// that bearerless hop with is its answer: no token was sent, so none is
// dropped or fetched again, and nothing is resent.
func TestRedirect_BackToTheOriginCarriesNoBearer(t *testing.T) {
	for name, back := range map[string]func(*http.Request, http.ResponseWriter){
		"answered":          okAnswer,
		"answered with 401": func(_ *http.Request, w http.ResponseWriter) { w.WriteHeader(http.StatusUnauthorized) },
	} {
		t.Run(name, func(t *testing.T) {
			var tokens int32
			ts := countingTokenServer(t, &tokens, 0)
			var home *hopServer
			away := newHopServer(t, func(r *http.Request, w http.ResponseWriter) {
				http.Redirect(w, r, home.URL+"/back", http.StatusTemporaryRedirect)
			})
			home = newHopServer(t, func(r *http.Request, w http.ResponseWriter) {
				if r.URL.Path == "/start" {
					http.Redirect(w, r, away.URL+"/x", http.StatusTemporaryRedirect)
					return
				}
				back(r, w)
			})
			ctx, obs := WithRedirectObservation(context.Background())
			ctx, refresh := WithCredentialRefreshObservation(ctx)
			resp := get(t, ctx, refreshClient(t, ts.URL), home.URL+"/start")
			resp.Body.Close()
			if !slices.Equal(home.seen(), []string{"Bearer AT-1", ""}) || !slices.Equal(away.seen(), []string{""}) {
				t.Fatalf("home %q, away %q; want the bearer on the first request only", home.seen(), away.seen())
			}
			if want := []string{RedirectCrossOrigin, RedirectSameOrigin}; !slices.Equal(obs.Hops(), want) {
				t.Fatalf("hops %q, want %q", obs.Hops(), want)
			}
			if atomic.LoadInt32(&tokens) != 1 || len(refresh.Resends()) != 0 {
				t.Fatalf("tokens fetched %d, resends %q; want one token and no resend", atomic.LoadInt32(&tokens), refresh.Resends())
			}
		})
	}
}

// A chain that stays within the token's origin carries the bearer on every
// hop.
func TestRedirect_AChainWithinTheOriginCarriesTheBearerOnEveryHop(t *testing.T) {
	var tokens int32
	ts := countingTokenServer(t, &tokens, 0)
	var home *hopServer
	home = newHopServer(t, func(r *http.Request, w http.ResponseWriter) {
		switch r.URL.Path {
		case "/a":
			http.Redirect(w, r, home.URL+"/b", http.StatusTemporaryRedirect)
		case "/b":
			http.Redirect(w, r, home.URL+"/c", http.StatusTemporaryRedirect)
		default:
			okAnswer(r, w)
		}
	})
	resp := post(t, context.Background(), refreshClient(t, ts.URL), home.URL+"/a", []byte(`{}`))
	resp.Body.Close()
	if want := []string{"Bearer AT-1", "Bearer AT-1", "Bearer AT-1"}; !slices.Equal(home.seen(), want) {
		t.Fatalf("bearers %q, want %q", home.seen(), want)
	}
}

// A 401 from another origin is that origin's answer: the token was never sent
// there, so it is neither dropped nor refreshed, and nothing is resent.
func TestRedirect_ACrossOrigin401DoesNotRefresh(t *testing.T) {
	var tokens int32
	ts := countingTokenServer(t, &tokens, 0)
	away := newHopServer(t, func(_ *http.Request, w http.ResponseWriter) {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		http.Error(w, "who are you", http.StatusUnauthorized)
	})
	home := newHopServer(t, redirectTo(away.URL+"/x"))
	hc := refreshClient(t, ts.URL)
	ctx, refresh := WithCredentialRefreshObservation(context.Background())
	resp := get(t, ctx, hc, home.URL)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || len(away.seen()) != 1 || len(home.seen()) != 1 || len(refresh.Resends()) != 0 {
		t.Fatalf("answer %d, away %d, home %d sends, resends %q; want the other origin's 401, once each, none", resp.StatusCode, len(away.seen()), len(home.seen()), refresh.Resends())
	}
	// The cached token is kept: the next request needs no fetch.
	resp = post(t, context.Background(), hc, newHopServer(t, okAnswer).URL, []byte(`{}`))
	resp.Body.Close()
	if atomic.LoadInt32(&tokens) != 1 {
		t.Fatalf("%d token fetches, want 1: a cross-origin 401 is not this token's refusal", tokens)
	}
}

// A redirect from https to http is refused before the hop is sent, whatever
// its origin: a 307 or 308 replays the body, which would travel in cleartext.
func TestRedirect_HTTPSToHTTPIsRefusedBeforeItIsSent(t *testing.T) {
	var tokens int32
	ts := countingTokenServer(t, &tokens, 0)
	var plainHits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plainHits.Add(1)
		okAnswer(r, w)
	}))
	t.Cleanup(plain.Close)
	for name, target := range map[string]func(tlsURL string) string{
		"to another origin": func(string) string { return plain.URL + "/x" },
		"to the same host":  func(tlsURL string) string { return "http://" + strings.TrimPrefix(tlsURL, "https://") + "/x" },
	} {
		t.Run(name, func(t *testing.T) {
			plainHits.Store(0)
			var tlsSrv *httptest.Server
			tlsSrv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target(tlsSrv.URL), http.StatusPermanentRedirect)
			}))
			t.Cleanup(tlsSrv.Close)
			hc := refreshClient(t, ts.URL)
			hc.Transport.(*bearerTransport).base = tlsSrv.Client().Transport
			ctx, obs := WithRedirectObservation(context.Background())
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, tlsSrv.URL+"/start", strings.NewReader(`{"phi":true}`))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := hc.Do(req)
			if resp != nil {
				resp.Body.Close()
			}
			if !errors.Is(err, ErrCleartextRedirect) {
				t.Fatalf("err %v, want ErrCleartextRedirect", err)
			}
			if want := []string{RedirectCleartextRefused}; plainHits.Load() != 0 || !slices.Equal(obs.Hops(), want) {
				t.Fatalf("the http origin received %d requests, hops %q; want none, hops %q", plainHits.Load(), obs.Hops(), want)
			}
		})
	}
}

// The same policy holds on a client that carries no bearer: CheckRedirect is
// the one redirect policy for every client that reaches a participant's
// system, authenticated or not.
func TestCheckRedirect_RefusesHTTPSToHTTPWithoutABearer(t *testing.T) {
	var plainHits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plainHits.Add(1)
		okAnswer(r, w)
	}))
	t.Cleanup(plain.Close)
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/x", http.StatusFound)
	}))
	t.Cleanup(tlsSrv.Close)
	hc := tlsSrv.Client()
	hc.CheckRedirect = CheckRedirect
	ctx, obs := WithRedirectObservation(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tlsSrv.URL+"/Patient/1", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := hc.Do(req)
	if !errors.Is(err, ErrCleartextRedirect) || resp == nil || resp.StatusCode != http.StatusFound {
		t.Fatalf("err %v, resp %v; want ErrCleartextRedirect with the 302 that asked for it", err, resp)
	}
	if want := []string{RedirectCleartextRefused}; plainHits.Load() != 0 || !slices.Equal(obs.Hops(), want) {
		t.Fatalf("the http origin received %d requests, hops %q; want none, hops %q", plainHits.Load(), obs.Hops(), want)
	}
}

// A client that follows no redirect at all still names a downgrade as one:
// RefuseCleartextRedirect returns ErrCleartextRedirect and records it, and
// nil for any other hop, recording nothing (the caller refuses it its own way).
func TestRefuseCleartextRedirect(t *testing.T) {
	mk := func(raw string) *http.Request {
		r, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	first := mk("https://sor.example/Patient")
	for name, tc := range map[string]struct {
		next string
		err  error
		hops []string
	}{
		"https to http":        {"http://sor.example/Patient", ErrCleartextRedirect, []string{RedirectCleartextRefused}},
		"https to https":       {"https://other.example/Patient", nil, nil},
		"https to HTTP (case)": {"HTTP://sor.example/Patient", ErrCleartextRedirect, []string{RedirectCleartextRefused}},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, obs := WithRedirectObservation(context.Background())
			next := mk(tc.next).WithContext(ctx)
			err := RefuseCleartextRedirect(next, []*http.Request{first})
			if (tc.err == nil && err != nil) || (tc.err != nil && !errors.Is(err, tc.err)) {
				t.Fatalf("err %v, want %v", err, tc.err)
			}
			if !slices.Equal(obs.Hops(), tc.hops) {
				t.Fatalf("hops %q, want %q", obs.Hops(), tc.hops)
			}
		})
	}
	// An http chain that stays http is not a downgrade.
	ctx, obs := WithRedirectObservation(context.Background())
	if err := RefuseCleartextRedirect(mk("http://a.example/").WithContext(ctx), []*http.Request{mk("http://b.example/")}); err != nil || obs.Hops() != nil {
		t.Fatalf("err %v hops %q, want none", err, obs.Hops())
	}
}

// Every value the observation records is in the closed list.
func TestRedirectValues_Closed(t *testing.T) {
	want := []string{RedirectSameOrigin, RedirectCrossOrigin, RedirectCleartextRefused, RedirectTokenRefused, RedirectCrossOriginRefused}
	if !slices.Equal(RedirectValues, want) {
		t.Fatalf("RedirectValues %q, want %q", RedirectValues, want)
	}
}

// ParticipantClient gives a client with no redirect policy CheckRedirect, and
// keeps a client's own policy after refusing a downgrade: one that refuses
// every redirect stays as strict, and still names a downgrade as one.
func TestParticipantClient(t *testing.T) {
	if ParticipantClient(nil) != nil {
		t.Fatal("a nil client did not stay nil")
	}
	var plainHits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plainHits.Add(1)
		okAnswer(r, w)
	}))
	t.Cleanup(plain.Close)
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { okAnswer(r, w) }))
	t.Cleanup(other.Close)
	var target string
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusFound)
	}))
	t.Cleanup(tlsSrv.Close)
	errOwn := errors.New("own policy")
	for name, tc := range map[string]struct {
		own    func(*http.Request, []*http.Request) error
		target string
		err    error
		hops   []string
	}{
		"no policy, a downgrade":          {nil, plain.URL, ErrCleartextRedirect, []string{RedirectCleartextRefused}},
		"no policy, another https origin": {nil, other.URL, nil, []string{RedirectCrossOrigin}},
		"its own policy, a downgrade":     {func(*http.Request, []*http.Request) error { return errOwn }, plain.URL, ErrCleartextRedirect, []string{RedirectCleartextRefused}},
		"its own policy, another origin":  {func(*http.Request, []*http.Request) error { return errOwn }, other.URL, errOwn, nil},
	} {
		t.Run(name, func(t *testing.T) {
			plainHits.Store(0)
			target = tc.target
			base := tlsSrv.Client()
			base.CheckRedirect = tc.own
			c := ParticipantClient(base)
			ctx, obs := WithRedirectObservation(context.Background())
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, tlsSrv.URL, nil)
			resp, err := c.Do(req)
			if resp != nil {
				resp.Body.Close()
			}
			if (tc.err == nil && err != nil) || (tc.err != nil && !errors.Is(err, tc.err)) {
				t.Fatalf("err %v, want %v", err, tc.err)
			}
			if plainHits.Load() != 0 || !slices.Equal(obs.Hops(), tc.hops) {
				t.Fatalf("http origin hits %d, hops %q; want 0, %q", plainHits.Load(), obs.Hops(), tc.hops)
			}
			if fmt.Sprintf("%p", base.CheckRedirect) != fmt.Sprintf("%p", tc.own) {
				t.Fatal("the caller's client changed")
			}
		})
	}
}

// The token endpoint is the participant's own authorization server: a
// redirect of the token request from https to http is never followed, so its
// client secret never travels in cleartext, whether the token client is the
// default or one the deployment sets. The fetch fails as a transport failure
// that says why.
func TestTokenFetch_AnHTTPSToHTTPRedirectIsNeverFollowed(t *testing.T) {
	var plainHits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plainHits.Add(1)
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"AT-LEAKED","token_type":"bearer","expires_in":300}`)
	}))
	t.Cleanup(plain.Close)
	tokenSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/collect?secret=x", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(tokenSrv.Close)
	for name, tokenClient := range map[string]func() *http.Client{
		"a token client the deployment sets": func() *http.Client { return tokenSrv.Client() },
		"the default token client":           func() *http.Client { return nil },
	} {
		t.Run(name, func(t *testing.T) {
			plainHits.Store(0)
			cfg := Config{TokenURL: tokenSrv.URL + "/token", ClientID: "gw", ClientSecret: "OPAQUE-VALUE-3686", Scope: "system/*.read", Clock: time.Now, HTTPClient: tokenClient()}
			if cfg.HTTPClient == nil {
				// The default client cannot verify the test server's
				// certificate: trust it process-wide for this row only.
				prev := http.DefaultTransport
				http.DefaultTransport = tokenSrv.Client().Transport
				t.Cleanup(func() { http.DefaultTransport = prev })
			}
			hc, err := NewHTTPClient(cfg)
			if err != nil {
				t.Fatal(err)
			}
			srv := newHopServer(t, okAnswer)
			ctx, obs := WithRedirectObservation(context.Background())
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
			resp, err := hc.Do(req)
			if resp != nil {
				resp.Body.Close()
			}
			var transport *TokenTransportError
			if !errors.As(err, &transport) || !errors.Is(err, ErrCleartextRedirect) {
				t.Fatalf("err %v, want a token transport failure that is a refused downgrade", err)
			}
			if msg := err.Error(); strings.Contains(msg, "collect") || strings.Contains(msg, "secret=") {
				t.Fatalf("error %q carries the Location's path or query", msg)
			}
			if want := []string{RedirectCleartextRefused}; !slices.Equal(obs.Hops(), want) {
				t.Fatalf("hops %q, want %q", obs.Hops(), want)
			}
			if plainHits.Load() != 0 || len(srv.seen()) != 0 {
				t.Fatalf("the http origin received %d token requests and the system %d; want none", plainHits.Load(), len(srv.seen()))
			}
		})
	}
}

// Authorization never goes to another origin, whatever set it: a fixed
// header a deployment configures on a client with no bearer is dropped on a
// hop to another port of the same host (which net/http's own rule keeps), and
// kept on a hop within the origin. Both policies, CheckRedirect and the one
// ParticipantClient composes, drop it.
func TestRedirect_AuthorizationNeverGoesToAnotherOrigin(t *testing.T) {
	other := newHopServer(t, okAnswer)
	var home *hopServer
	home = newHopServer(t, func(r *http.Request, w http.ResponseWriter) {
		switch r.URL.Path {
		case "/to-other":
			http.Redirect(w, r, other.URL+"/x", http.StatusTemporaryRedirect)
		case "/to-home":
			http.Redirect(w, r, home.URL+"/end", http.StatusTemporaryRedirect)
		default:
			okAnswer(r, w)
		}
	})
	for name, mk := range map[string]func() *http.Client{
		"CheckRedirect": func() *http.Client { return &http.Client{CheckRedirect: CheckRedirect} },
		"a client with its own policy": func() *http.Client {
			return ParticipantClient(&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }})
		},
		"a client with no policy, wrapped": func() *http.Client { return ParticipantClient(&http.Client{}) },
	} {
		t.Run(name, func(t *testing.T) {
			before := len(other.seen())
			for _, path := range []string{"/to-other", "/to-home"} {
				req, _ := http.NewRequest(http.MethodGet, home.URL+path, nil)
				req.Header.Set("Authorization", "Basic OPAQUE-VALUE-3686")
				resp, err := mk().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
			}
			seen := other.seen()[before:]
			if !slices.Equal(seen, []string{""}) {
				t.Fatalf("the other origin received Authorization %q, want none", seen)
			}
			homeSeen := home.seen()
			if last := homeSeen[len(homeSeen)-1]; last != "Basic OPAQUE-VALUE-3686" {
				t.Fatalf("the hop within the origin carried %q, want the header kept", last)
			}
		})
	}
}
