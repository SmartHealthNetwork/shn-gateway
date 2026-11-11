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
	"sync/atomic"
	"testing"
)

// countingServer is an http server that counts the requests it receives,
// answering with answer.
func countingServer(t *testing.T, answer func(*http.Request, http.ResponseWriter)) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.ReadAll(r.Body)
		answer(r, w)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// bodyClients are the clients a request that carries a body goes through: one
// with no redirect policy, one with its own, one ParticipantClient already
// wrapped, and NewHTTPClient's policy.
func bodyClients() map[string]func() *http.Client {
	return map[string]func() *http.Client{
		"a client with no policy": func() *http.Client { return ParticipantClient(&http.Client{}) },
		"a client with its own policy": func() *http.Client {
			return ParticipantClient(&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }})
		},
		"a client with configured headers": func() *http.Client {
			return ParticipantClientWith(&http.Client{}, http.Header{"X-Route-Tenant": {"t"}})
		},
		"NewHTTPClient's policy": func() *http.Client { return &http.Client{CheckRedirect: CheckRedirect} },
	}
}

// postBody sends a POST carrying a body to url through c.
func postBody(ctx context.Context, c *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(`{"resourceType":"Bundle"}`))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/fhir+json")
	return c.Do(req)
}

// A request that carries a body follows no redirect to another origin,
// whatever its code: a 307 or 308 would replay the body (a patient's data)
// there, and a 301, 302 or 303 would answer it with a GET it never made. The
// other origin receives no request at all; the request fails with a
// CrossOriginRedirectError naming the redirect's status and the origin only;
// and the hop is recorded as cross-origin-refused. Every client shape.
func TestBodyRedirect_ACrossOriginRedirectIsNeverFollowed(t *testing.T) {
	other, otherHits := countingServer(t, okAnswer)
	var code int
	home, homeHits := countingServer(t, func(r *http.Request, w http.ResponseWriter) {
		http.Redirect(w, r, other.URL+"/collect?patient=x", code)
	})
	for _, c := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect, http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther} {
		for name, mk := range bodyClients() {
			t.Run(fmt.Sprintf("%d/%s", c, name), func(t *testing.T) {
				code = c
				otherHits.Store(0)
				homeHits.Store(0)
				ctx, obs := WithRedirectObservation(context.Background())
				resp, err := postBody(ctx, mk(), home.URL+"/Claim/$submit")
				if resp != nil {
					resp.Body.Close()
				}
				var cross *CrossOriginRedirectError
				if !errors.Is(err, ErrCrossOriginBodyRedirect) || !errors.As(err, &cross) {
					t.Fatalf("err %v, want a refused cross-origin redirect", err)
				}
				if cross.StatusCode != c || cross.Origin != other.URL || strings.Contains(cross.Error(), "collect") || strings.Contains(cross.Error(), "patient=") {
					t.Fatalf("refusal %+v (%q), want status %d and origin %s only", cross, cross.Error(), c, other.URL)
				}
				if otherHits.Load() != 0 || homeHits.Load() != 1 {
					t.Fatalf("requests: other origin %d, home %d; want 0 and 1", otherHits.Load(), homeHits.Load())
				}
				if want := []string{RedirectCrossOriginRefused}; !slices.Equal(obs.Hops(), want) {
					t.Fatalf("hops %q, want %q", obs.Hops(), want)
				}
			})
		}
	}
}

// A request that carries a body follows a redirect within its own origin,
// its body replayed on a 307, as before.
func TestBodyRedirect_ASameOriginRedirectIsFollowed(t *testing.T) {
	var home *httptest.Server
	var bodies []string
	home = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if r.URL.Path == "/first" {
			http.Redirect(w, r, home.URL+"/second", http.StatusTemporaryRedirect)
			return
		}
		okAnswer(r, w)
	}))
	t.Cleanup(home.Close)
	for name, mk := range bodyClients() {
		t.Run(name, func(t *testing.T) {
			bodies = nil
			ctx, obs := WithRedirectObservation(context.Background())
			resp, err := postBody(ctx, mk(), home.URL+"/first")
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK || len(bodies) != 2 || bodies[1] != `{"resourceType":"Bundle"}` {
				t.Fatalf("answer %d, bodies %q; want the second hop to carry the body", resp.StatusCode, bodies)
			}
			// A client with its own policy records no followed hop: its own
			// policy decides it (ParticipantClient).
			want := []string{RedirectSameOrigin}
			if name == "a client with its own policy" {
				want = nil
			}
			if !slices.Equal(obs.Hops(), want) {
				t.Fatalf("hops %q, want %q", obs.Hops(), want)
			}
		})
	}
}

// The rule is decided by the request the chain began with: a 303 within the
// origin turns the request into a GET, and a later hop to another origin is
// still refused, since the request the participant sent carried a body.
func TestBodyRedirect_DecidedByTheOriginalRequest(t *testing.T) {
	other, otherHits := countingServer(t, okAnswer)
	var home *httptest.Server
	home = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		if r.URL.Path == "/submit" {
			http.Redirect(w, r, home.URL+"/result", http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, other.URL+"/elsewhere", http.StatusFound)
	}))
	t.Cleanup(home.Close)
	ctx, obs := WithRedirectObservation(context.Background())
	resp, err := postBody(ctx, ParticipantClient(&http.Client{}), home.URL+"/submit")
	if resp != nil {
		resp.Body.Close()
	}
	if !errors.Is(err, ErrCrossOriginBodyRedirect) || otherHits.Load() != 0 {
		t.Fatalf("err %v, other origin hits %d; want a refused cross-origin redirect and none", err, otherHits.Load())
	}
	if want := []string{RedirectSameOrigin, RedirectCrossOriginRefused}; !slices.Equal(obs.Hops(), want) {
		t.Fatalf("hops %q, want %q", obs.Hops(), want)
	}
}

// A bodiless request still follows a redirect to another origin, without its
// credentials, as before.
func TestBodyRedirect_ABodilessRequestStillFollowsAcrossOrigins(t *testing.T) {
	other, otherHits := countingServer(t, okAnswer)
	home, _ := countingServer(t, redirectTo(other.URL+"/x"))
	for name, mk := range bodyClients() {
		t.Run(name, func(t *testing.T) {
			otherHits.Store(0)
			resp := get(t, context.Background(), mk(), home.URL)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK || otherHits.Load() != 1 {
				t.Fatalf("answer %d, other origin hits %d; want the other origin's answer", resp.StatusCode, otherHits.Load())
			}
		})
	}
}

// A redirect of a request that carries a body from https to http is still
// refused as a downgrade (ErrCleartextRedirect), whatever its origin: that
// rule is checked first.
func TestBodyRedirect_AnHTTPSToHTTPRedirectIsStillADowngrade(t *testing.T) {
	plain, plainHits := countingServer(t, okAnswer)
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/x", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(tlsSrv.Close)
	ctx, obs := WithRedirectObservation(context.Background())
	resp, err := postBody(ctx, ParticipantClient(tlsSrv.Client()), tlsSrv.URL)
	if resp != nil {
		resp.Body.Close()
	}
	if !errors.Is(err, ErrCleartextRedirect) || errors.Is(err, ErrCrossOriginBodyRedirect) || plainHits.Load() != 0 {
		t.Fatalf("err %v, http origin hits %d; want the downgrade refused and nothing sent", err, plainHits.Load())
	}
	if want := []string{RedirectCleartextRefused}; !slices.Equal(obs.Hops(), want) {
		t.Fatalf("hops %q, want %q", obs.Hops(), want)
	}
}

// The rule reads the request's method as well as its body: any method but
// GET and HEAD is refused a redirect to another origin even with no body (an
// empty POST), and a bodiless GET or HEAD follows one.
func TestBodyRedirect_DecidedByMethodAndBody(t *testing.T) {
	other, otherHits := countingServer(t, okAnswer)
	home, _ := countingServer(t, redirectTo(other.URL+"/x"))
	for _, tc := range []struct {
		name   string
		method string
		body   io.Reader
		follow bool
	}{
		{"a POST with no body", http.MethodPost, nil, false},
		{"a POST with http.NoBody", http.MethodPost, http.NoBody, false},
		{"a PUT with no body", http.MethodPut, nil, false},
		{"a GET", http.MethodGet, nil, true},
		{"a HEAD", http.MethodHead, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			otherHits.Store(0)
			req, _ := http.NewRequest(tc.method, home.URL, tc.body)
			resp, err := ParticipantClient(&http.Client{}).Do(req)
			if resp != nil {
				resp.Body.Close()
			}
			if tc.follow {
				if err != nil || otherHits.Load() != 1 {
					t.Fatalf("err %v, other origin hits %d; want the redirect followed", err, otherHits.Load())
				}
				return
			}
			if !errors.Is(err, ErrCrossOriginBodyRedirect) || otherHits.Load() != 0 {
				t.Fatalf("err %v, other origin hits %d; want the redirect refused and nothing sent", err, otherHits.Load())
			}
		})
	}
}
