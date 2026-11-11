package smartauth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// seen is one request a test server received: its method and body.
type seen struct{ method, body string }

// recordingServer answers each request with answer and records what it
// received, in order.
func recordingServer(t *testing.T, answer func(srv *httptest.Server, r *http.Request, w http.ResponseWriter)) (*httptest.Server, func() []seen) {
	t.Helper()
	var mu sync.Mutex
	var got []seen
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, seen{r.Method, string(b)})
		mu.Unlock()
		answer(srv, r, w)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []seen {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}

// redirectOnce redirects the first path to /next with code, and answers /next
// with 200.
func redirectOnce(code int) func(*httptest.Server, *http.Request, http.ResponseWriter) {
	return func(srv *httptest.Server, r *http.Request, w http.ResponseWriter) {
		if r.URL.Path == "/next" {
			okAnswer(r, w)
			return
		}
		w.Header().Set("Location", srv.URL+"/next")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, "moved")
	}
}

// A 301, 302 or 303 within the origin that would turn a request with a body
// into a GET with none is not followed, for every method that carries one and
// every client shape: the system received the one request it was sent, the
// client returns the system's own 3xx and its body as the answer, with no
// error, and the hop is recorded as method-change-refused.
func TestMethodRedirect_AMethodChangeWithinTheOriginIsNotFollowed(t *testing.T) {
	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			for name, mk := range bodyClients() {
				t.Run(fmt.Sprintf("%d/%s/%s", code, method, name), func(t *testing.T) {
					srv, got := recordingServer(t, redirectOnce(code))
					ctx, obs := WithRedirectObservation(context.Background())
					req, _ := http.NewRequestWithContext(ctx, method, srv.URL+"/cds-services/order-select", strings.NewReader(`{"hook":"order-select"}`))
					resp, err := mk().Do(req)
					if err != nil {
						t.Fatalf("err %v, want the system's %d as the answer", err, code)
					}
					b, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					if resp.StatusCode != code || string(b) != "moved" {
						t.Fatalf("answer %d %q, want the system's %d and its body", resp.StatusCode, b, code)
					}
					if want := []seen{{method, `{"hook":"order-select"}`}}; !slices.Equal(got(), want) {
						t.Fatalf("the system received %q, want only %q", got(), want)
					}
					if want := []string{RedirectMethodChangeRefused}; !slices.Equal(obs.Hops(), want) {
						t.Fatalf("hops %q, want %q", obs.Hops(), want)
					}
				})
			}
		}
	}
}

// A request with a body that a redirect would drop is not followed even when
// its method stays: net/http keeps a GET a GET on a 302 and drops its body.
func TestMethodRedirect_ADroppedBodyIsNotFollowed(t *testing.T) {
	srv, got := recordingServer(t, redirectOnce(http.StatusFound))
	ctx, obs := WithRedirectObservation(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/search", strings.NewReader("q=1"))
	resp, err := ParticipantClient(&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || len(got()) != 1 {
		t.Fatalf("answer %d, requests %q; want the system's 302 and one request", resp.StatusCode, got())
	}
	if want := []string{RedirectMethodChangeRefused}; !slices.Equal(obs.Hops(), want) {
		t.Fatalf("hops %q, want %q", obs.Hops(), want)
	}
}

// A 307 or 308 within the origin keeps the method and replays the body, and
// is followed as before; an empty POST's 307 keeps the request too.
func TestMethodRedirect_A307Or308IsFollowedWithTheBody(t *testing.T) {
	for _, code := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for _, body := range []string{`{"resourceType":"Bundle"}`, ""} {
			for name, mk := range bodyClients() {
				t.Run(fmt.Sprintf("%d/body %q/%s", code, body, name), func(t *testing.T) {
					srv, got := recordingServer(t, redirectOnce(code))
					ctx, obs := WithRedirectObservation(context.Background())
					var r io.Reader
					if body != "" {
						r = strings.NewReader(body)
					}
					req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/Claim/$submit", r)
					resp, err := mk().Do(req)
					if err != nil {
						t.Fatal(err)
					}
					resp.Body.Close()
					if resp.StatusCode != http.StatusOK {
						t.Fatalf("answer %d, want the redirect followed", resp.StatusCode)
					}
					if want := []seen{{http.MethodPost, body}, {http.MethodPost, body}}; !slices.Equal(got(), want) {
						t.Fatalf("the system received %q, want %q", got(), want)
					}
					// A client with its own policy records no followed hop.
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
	}
}

// A GET or HEAD with no body follows a 301, 302 or 303 as before: the hop is
// the request it was.
func TestMethodRedirect_ABodilessReadStillFollows(t *testing.T) {
	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			t.Run(fmt.Sprintf("%d/%s", code, method), func(t *testing.T) {
				srv, got := recordingServer(t, redirectOnce(code))
				ctx, obs := WithRedirectObservation(context.Background())
				req, _ := http.NewRequestWithContext(ctx, method, srv.URL+"/Patient/1", nil)
				resp, err := ParticipantClient(&http.Client{}).Do(req)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK || len(got()) != 2 || got()[1].method != method {
					t.Fatalf("answer %d, requests %q; want the redirect followed as a %s", resp.StatusCode, got(), method)
				}
				if want := []string{RedirectSameOrigin}; !slices.Equal(obs.Hops(), want) {
					t.Fatalf("hops %q, want %q", obs.Hops(), want)
				}
			})
		}
	}
}

// A chain that keeps the request on its first hop and would change it on a
// later one stops at that hop, with that hop's 3xx as the answer.
func TestMethodRedirect_ALaterHopIsCheckedToo(t *testing.T) {
	srv, got := recordingServer(t, func(srv *httptest.Server, r *http.Request, w http.ResponseWriter) {
		switch r.URL.Path {
		case "/first":
			http.Redirect(w, r, srv.URL+"/second", http.StatusTemporaryRedirect)
		case "/second":
			http.Redirect(w, r, srv.URL+"/third", http.StatusFound)
		default:
			okAnswer(r, w)
		}
	})
	ctx, obs := WithRedirectObservation(context.Background())
	resp, err := postBody(ctx, ParticipantClient(&http.Client{}), srv.URL+"/first")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || len(got()) != 2 {
		t.Fatalf("answer %d, requests %q; want the second hop's 302 and two requests", resp.StatusCode, got())
	}
	if want := []string{RedirectSameOrigin, RedirectMethodChangeRefused}; !slices.Equal(obs.Hops(), want) {
		t.Fatalf("hops %q, want %q", obs.Hops(), want)
	}
}

// A redirect of a request with a body to another origin is still refused as
// a cross-origin redirect, never answered as a method change: the
// cross-origin rule is checked first.
func TestMethodRedirect_ACrossOriginRedirectStaysCrossOriginRefused(t *testing.T) {
	other, otherGot := recordingServer(t, redirectOnce(http.StatusOK))
	home, _ := recordingServer(t, func(_ *httptest.Server, r *http.Request, w http.ResponseWriter) {
		http.Redirect(w, r, other.URL+"/x", http.StatusFound)
	})
	ctx, obs := WithRedirectObservation(context.Background())
	resp, err := postBody(ctx, ParticipantClient(&http.Client{}), home.URL)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "another origin") || len(otherGot()) != 0 {
		t.Fatalf("err %v, other origin requests %q; want the cross-origin refusal and none", err, otherGot())
	}
	if want := []string{RedirectCrossOriginRefused}; !slices.Equal(obs.Hops(), want) {
		t.Fatalf("hops %q, want %q", obs.Hops(), want)
	}
}

// A client with its own redirect policy never sees the hop: the method change
// is refused before its policy runs, so a policy that follows everything
// cannot let it through.
func TestMethodRedirect_RefusedBeforeAClientsOwnPolicy(t *testing.T) {
	srv, got := recordingServer(t, redirectOnce(http.StatusSeeOther))
	called := false
	c := ParticipantClient(&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { called = true; return nil }})
	resp, err := postBody(context.Background(), c, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if called || resp.StatusCode != http.StatusSeeOther || len(got()) != 1 {
		t.Fatalf("own policy called %v, answer %d, requests %q; want it never called, the 303, one request", called, resp.StatusCode, got())
	}
}

func TestRefuseMethodChangeRedirect(t *testing.T) {
	post := func() *http.Request {
		r, _ := http.NewRequest(http.MethodPost, "https://payer.example/a", strings.NewReader("{}"))
		return r
	}
	emptyPost, _ := http.NewRequest(http.MethodPost, "https://payer.example/a", nil)
	get, _ := http.NewRequest(http.MethodGet, "https://payer.example/b", nil)
	replay := post()
	replay.URL.Path = "/b"
	emptyReplay, _ := http.NewRequest(http.MethodPost, "https://payer.example/b", nil)
	// A request that names no method is a GET to net/http, and its 302 hop a
	// GET: the same request.
	unnamed := &http.Request{URL: get.URL}
	// A zero-length reader is no body: http.NewRequest makes it http.NoBody.
	emptyReaderGet, _ := http.NewRequest(http.MethodGet, "https://payer.example/a", strings.NewReader(""))
	for _, tc := range []struct {
		name    string
		hop     *http.Request
		via     []*http.Request
		refused bool
	}{
		{"not a redirect", get, nil, false},
		{"a POST turned into a GET", get, []*http.Request{post()}, true},
		{"a POST replayed", replay, []*http.Request{post()}, false},
		{"an empty POST replayed", emptyReplay, []*http.Request{emptyPost}, false},
		{"an empty POST turned into a GET", get, []*http.Request{emptyPost}, true},
		{"a GET stays a GET", get, []*http.Request{get}, false},
		{"the last hop decides", get, []*http.Request{post(), replay}, true},
		{"a request naming no method stays a GET", get, []*http.Request{unnamed}, false},
		{"a GET with an empty body loses nothing", get, []*http.Request{emptyReaderGet}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := RefuseMethodChangeRedirect(tc.hop, tc.via); (err == http.ErrUseLastResponse) != tc.refused || (err != nil && err != http.ErrUseLastResponse) {
				t.Fatalf("err %v, refused %v", err, tc.refused)
			}
		})
	}
}
