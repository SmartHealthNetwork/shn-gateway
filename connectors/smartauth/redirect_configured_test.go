package smartauth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
)

// headerServer records every request's headers, answering with answer(r, w).
type headerServer struct {
	*httptest.Server
	mu   sync.Mutex
	seen []http.Header
}

func newHeaderServer(t *testing.T, tls bool, answer func(r *http.Request, w http.ResponseWriter)) *headerServer {
	t.Helper()
	h := &headerServer{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		h.mu.Lock()
		h.seen = append(h.seen, r.Header.Clone())
		h.mu.Unlock()
		answer(r, w)
	})
	if tls {
		h.Server = httptest.NewTLSServer(handler)
	} else {
		h.Server = httptest.NewServer(handler)
	}
	t.Cleanup(h.Close)
	return h
}

func (h *headerServer) requests() []http.Header {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.seen)
}

// configuredForTest are a deployment's fixed headers for a participant's
// system: names chosen to hold nothing a list of credential names would know.
func configuredForTest() http.Header {
	return http.Header{"X-Route-Tenant": {"tenant-7"}, "X-Partner-Key": {"OPAQUE-PARTNER-KEY"}}
}

// sendConfigured sends a GET to url through c carrying the configured headers,
// a bearer, and one header nobody configured, which a hop to another origin
// drops too (crossOriginHeaders).
func sendConfigured(t *testing.T, c *http.Client, url string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	for k, v := range configuredForTest() {
		req.Header[k] = append([]string(nil), v...)
	}
	req.Header.Set("Authorization", "Bearer AT-CONFIGURED")
	req.Header.Set("X-Not-Configured", "set-for-the-origin")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

// The clients ParticipantClientWith is given: one with no redirect policy, one
// with its own, and one ParticipantClient already wrapped.
func configuredClients() map[string]func() *http.Client {
	return map[string]func() *http.Client{
		"a client with no policy": func() *http.Client {
			return ParticipantClientWith(&http.Client{}, configuredForTest())
		},
		"a client with its own policy": func() *http.Client {
			return ParticipantClientWith(&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}, configuredForTest())
		},
		"a client ParticipantClient already wrapped": func() *http.Client {
			return ParticipantClientWith(ParticipantClient(&http.Client{}), configuredForTest())
		},
		"NewHTTPClient's policy, wrapped": func() *http.Client {
			return ParticipantClientWith(&http.Client{CheckRedirect: CheckRedirect}, configuredForTest())
		},
	}
}

// A hop to another origin carries none of the headers the deployment
// configured for the participant's system, and no Authorization, whatever the
// configured names are: the configured set decides, not a list of known
// credential names. A header nobody configured is not carried either: only
// the headers crossOriginHeaders names are. The other origin's answer is the
// answer.
func TestParticipantClientWith_ACrossOriginHopCarriesNoConfiguredHeader(t *testing.T) {
	for name, mk := range configuredClients() {
		t.Run(name, func(t *testing.T) {
			other := newHeaderServer(t, false, okAnswer)
			home := newHeaderServer(t, false, redirectTo(other.URL+"/x"))
			sendConfigured(t, mk(), home.URL+"/start")
			got := other.requests()
			if len(got) != 1 {
				t.Fatalf("the other origin received %d requests, want 1", len(got))
			}
			for _, k := range []string{"X-Route-Tenant", "X-Partner-Key", "Authorization"} {
				if v, ok := got[0][k]; ok {
					t.Errorf("the other origin received %s %q, want none", k, v)
				}
			}
			if v, ok := got[0]["X-Not-Configured"]; ok {
				t.Errorf("the other origin received a header nobody configured, %q", v)
			}
			first := home.requests()[0]
			if first.Get("X-Partner-Key") != "OPAQUE-PARTNER-KEY" || first.Get("X-Route-Tenant") != "tenant-7" || first.Get("Authorization") != "Bearer AT-CONFIGURED" {
				t.Errorf("the participant's own system received %v, want every configured header and the bearer", first)
			}
		})
	}
}

// A chain that stays within the origin (A to A to A) keeps every configured
// header, the bearer and every other header on every hop: they were set for
// that origin.
func TestParticipantClientWith_AChainWithinTheOriginKeepsTheConfiguredHeaders(t *testing.T) {
	for name, mk := range configuredClients() {
		t.Run(name, func(t *testing.T) {
			var home *headerServer
			home = newHeaderServer(t, false, func(r *http.Request, w http.ResponseWriter) {
				switch r.URL.Path {
				case "/a":
					http.Redirect(w, r, home.URL+"/b", http.StatusTemporaryRedirect)
				case "/b":
					http.Redirect(w, r, home.URL+"/c", http.StatusTemporaryRedirect)
				default:
					okAnswer(r, w)
				}
			})
			sendConfigured(t, mk(), home.URL+"/a")
			got := home.requests()
			if len(got) != 3 {
				t.Fatalf("the participant's origin received %d requests, want 3", len(got))
			}
			for i, h := range got {
				if h.Get("X-Partner-Key") != "OPAQUE-PARTNER-KEY" || h.Get("X-Route-Tenant") != "tenant-7" || h.Get("Authorization") != "Bearer AT-CONFIGURED" || h.Get("X-Not-Configured") != "set-for-the-origin" {
					t.Errorf("hop %d carried %v, want every configured header, the bearer and the header nobody configured", i, h)
				}
			}
		})
	}
}

// A chain that leaves the origin and comes back (A to B to A) carries no
// configured header and no Authorization on the hop away nor on the hop back:
// once the chain has left the origin, another origin chose where it goes (any
// path, and with a 307 the method and body), so nothing configured for the
// origin is sent for it, for the rest of the chain.
func TestParticipantClientWith_AChainBackToTheOriginCarriesNoConfiguredHeader(t *testing.T) {
	for name, mk := range configuredClients() {
		t.Run(name, func(t *testing.T) {
			var home *headerServer
			other := newHeaderServer(t, false, func(r *http.Request, w http.ResponseWriter) {
				http.Redirect(w, r, home.URL+"/back", http.StatusTemporaryRedirect)
			})
			home = newHeaderServer(t, false, func(r *http.Request, w http.ResponseWriter) {
				if r.URL.Path == "/start" {
					http.Redirect(w, r, other.URL+"/x", http.StatusTemporaryRedirect)
					return
				}
				okAnswer(r, w)
			})
			sendConfigured(t, mk(), home.URL+"/start")
			got, away := home.requests(), other.requests()
			if len(got) != 2 || len(away) != 1 {
				t.Fatalf("requests: origin %d, other %d; want 2 and 1", len(got), len(away))
			}
			if got[0].Get("X-Partner-Key") != "OPAQUE-PARTNER-KEY" || got[0].Get("Authorization") != "Bearer AT-CONFIGURED" || got[0].Get("X-Not-Configured") == "" {
				t.Errorf("the first request carried %v, want every configured header and the bearer", got[0])
			}
			for who, h := range map[string]http.Header{"the other origin": away[0], "the hop back to the origin": got[1]} {
				for _, k := range []string{"X-Route-Tenant", "X-Partner-Key", "Authorization"} {
					if v, ok := h[k]; ok {
						t.Errorf("%s received %s %q, want none", who, k, v)
					}
				}
				if v, ok := h["X-Not-Configured"]; ok {
					t.Errorf("%s received a header nobody configured, %q", who, v)
				}
			}
		})
	}
}

// A configured name matches without regard to case: a header map keyed in
// another case than the configured set still loses it on a hop away.
func TestParticipantClientWith_ConfiguredNamesMatchWithoutCase(t *testing.T) {
	other := newHeaderServer(t, false, okAnswer)
	home := newHeaderServer(t, false, redirectTo(other.URL+"/x"))
	c := ParticipantClientWith(&http.Client{}, http.Header{"x-partner-key": {"v"}})
	req, _ := http.NewRequest(http.MethodGet, home.URL, nil)
	req.Header["X-PARTNER-KEY"] = []string{"OPAQUE-PARTNER-KEY"}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for k := range other.requests()[0] {
		if http.CanonicalHeaderKey(k) == "X-Partner-Key" {
			t.Fatalf("the other origin received %s", k)
		}
	}
}

// With configured headers, a hop from https to http is still refused before
// it is sent, whatever the client's own policy: nothing reaches the http
// origin, and the refusal is recorded.
func TestParticipantClientWith_AnHTTPSToHTTPHopIsStillRefused(t *testing.T) {
	var plainHits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plainHits.Add(1)
		okAnswer(r, w)
	}))
	t.Cleanup(plain.Close)
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(tlsSrv.Close)
	for name, own := range map[string]func(*http.Request, []*http.Request) error{
		"no policy":       nil,
		"its own policy":  func(*http.Request, []*http.Request) error { return nil },
		"NewHTTPClient's": CheckRedirect,
	} {
		t.Run(name, func(t *testing.T) {
			plainHits.Store(0)
			base := tlsSrv.Client()
			base.CheckRedirect = own
			ctx, obs := WithRedirectObservation(context.Background())
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, tlsSrv.URL, nil)
			for k, v := range configuredForTest() {
				req.Header[k] = v
			}
			resp, err := ParticipantClientWith(base, configuredForTest()).Do(req)
			if resp != nil {
				resp.Body.Close()
			}
			if !errors.Is(err, ErrCleartextRedirect) || plainHits.Load() != 0 {
				t.Fatalf("err %v, http origin hits %d; want ErrCleartextRedirect and none", err, plainHits.Load())
			}
			if want := []string{RedirectCleartextRefused}; !slices.Equal(obs.Hops(), want) {
				t.Fatalf("hops %q, want %q", obs.Hops(), want)
			}
		})
	}
}

// A followed hop is recorded by its origin class with configured headers as
// without, and the caller's client is not changed; nil stays nil, and nil or
// empty configured is ParticipantClient.
func TestParticipantClientWith_RecordsAndLeavesTheCallersClient(t *testing.T) {
	if ParticipantClientWith(nil, configuredForTest()) != nil {
		t.Fatal("a nil client did not stay nil")
	}
	c := &http.Client{}
	_ = ParticipantClientWith(c, configuredForTest())
	if c.CheckRedirect != nil {
		t.Fatal("the caller's client gained a redirect policy")
	}
	other := newHeaderServer(t, false, okAnswer)
	var home *headerServer
	home = newHeaderServer(t, false, func(r *http.Request, w http.ResponseWriter) {
		if r.URL.Path == "/a" {
			http.Redirect(w, r, home.URL+"/b", http.StatusTemporaryRedirect)
			return
		}
		http.Redirect(w, r, other.URL+"/c", http.StatusTemporaryRedirect)
	})
	ctx, obs := WithRedirectObservation(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, home.URL+"/a", nil)
	resp, err := ParticipantClientWith(&http.Client{}, configuredForTest()).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if want := []string{RedirectSameOrigin, RedirectCrossOrigin}; !slices.Equal(obs.Hops(), want) {
		t.Fatalf("hops %q, want %q", obs.Hops(), want)
	}
	for _, empty := range []http.Header{nil, {}} {
		if got := ParticipantClientWith(&http.Client{}, empty); got.CheckRedirect == nil {
			t.Fatal("an empty configured set left no redirect policy")
		}
	}
}
