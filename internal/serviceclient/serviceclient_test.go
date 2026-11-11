package serviceclient

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
)

const planted = "/secret-path?q=secret-query"

// counted is a server that counts the requests it receives.
func counted(t *testing.T, tls bool, h http.HandlerFunc) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var n atomic.Int64
	f := func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		if h != nil {
			h(w, r)
		}
	}
	var s *httptest.Server
	if tls {
		s = httptest.NewTLSServer(http.HandlerFunc(f))
	} else {
		s = httptest.NewServer(http.HandlerFunc(f))
	}
	t.Cleanup(s.Close)
	return s, &n
}

// noSecret fails when s names the planted path or query.
func noSecret(t *testing.T, s string) {
	t.Helper()
	if strings.Contains(s, "secret-path") || strings.Contains(s, "secret-query") {
		t.Fatalf("names the Location's path or query: %q", s)
	}
}

// Every 3xx code is the call's failure, never followed: the other origin
// receives nothing, the error is a *RedirectError with the status and the
// Location's origin, and it names no path or query.
func TestEveryRedirectCodeIsRefused(t *testing.T) {
	other, otherHits := counted(t, false, nil)
	for code := 300; code <= 308; code++ {
		src, _ := counted(t, false, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", other.URL+planted)
			w.WriteHeader(code)
		})
		resp, err := ServiceClient(nil).Post(src.URL+"/call", "application/json", strings.NewReader(`{}`))
		if resp != nil {
			resp.Body.Close()
		}
		var re *RedirectError
		if !errors.As(err, &re) || re.StatusCode != code || re.Origin != other.URL || re.Cleartext || errors.Is(err, smartauth.ErrCleartextRedirect) || !IsRedirectRefused(err) || RedirectStatus(err) != code || !strings.Contains(err.Error(), RedirectRefused) {
			t.Fatalf("%d: err = %v, want a *RedirectError %d from %s", code, err, code, other.URL)
		}
		noSecret(t, err.Error())
	}
	if n := otherHits.Load(); n != 0 {
		t.Fatalf("the redirect's origin received %d requests", n)
	}
}

// A same-origin redirect is not followed either: the path it names receives
// nothing.
func TestSameOriginRedirectIsRefused(t *testing.T) {
	for _, code := range []int{http.StatusFound, http.StatusTemporaryRedirect} {
		var other atomic.Int64
		var src *httptest.Server
		src, _ = counted(t, false, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/other" {
				other.Add(1)
				return
			}
			w.Header().Set("Location", "/other?q=secret-query")
			w.WriteHeader(code)
		})
		_, err := ServiceClient(src.Client()).Get(src.URL + "/call")
		var re *RedirectError
		if !errors.As(err, &re) || re.StatusCode != code || re.Origin != src.URL {
			t.Fatalf("%d: err = %v", code, err)
		}
		noSecret(t, err.Error())
		if other.Load() != 0 {
			t.Fatalf("%d: the same-origin path was requested", code)
		}
	}
}

// A redirect from https to http is ErrCleartextRedirect, never sent.
func TestHTTPSToHTTPIsCleartextRefused(t *testing.T) {
	plain, plainHits := counted(t, false, nil)
	src, _ := counted(t, true, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", plain.URL+planted)
		w.WriteHeader(http.StatusPermanentRedirect)
	})
	_, err := ServiceClient(src.Client()).Post(src.URL+"/call", "text/plain", strings.NewReader("x"))
	var re *RedirectError
	if !errors.Is(err, smartauth.ErrCleartextRedirect) || !IsRedirectRefused(err) || !errors.As(err, &re) || !re.Cleartext || RedirectStatus(err) != http.StatusPermanentRedirect || re.Origin != plain.URL {
		t.Fatalf("err = %v, want ErrCleartextRedirect, a cleartext *RedirectError 308 from %s", err, plain.URL)
	}
	if !strings.Contains(err.Error(), RedirectRefused) {
		t.Fatalf("err %q does not name the class", err)
	}
	noSecret(t, err.Error())
	if plainHits.Load() != 0 {
		t.Fatal("the cleartext origin was requested")
	}
}

// A 300 with no Location, and one with a Location that cannot be read, fail
// the same way, with no origin.
func TestRedirectWithoutReadableLocation(t *testing.T) {
	for name, loc := range map[string]string{"none": "", "unreadable": "http://[::1%zz/secret-path?q=secret-query"} {
		src, _ := counted(t, false, func(w http.ResponseWriter, r *http.Request) {
			if loc != "" {
				w.Header()["Location"] = []string{loc}
			}
			w.WriteHeader(http.StatusMultipleChoices)
		})
		_, err := ServiceClient(nil).Get(src.URL)
		var re *RedirectError
		if !errors.As(err, &re) || re.StatusCode != 300 || re.Origin != "" {
			t.Fatalf("%s: err = %v", name, err)
		}
		noSecret(t, err.Error())
	}
}

// The caller's client is unchanged; nil gets a fresh client; a client with
// its own CheckRedirect still follows nothing; wrapping twice adds no second
// layer; a tagged transport keeps its tag outermost.
func TestServiceClientShape(t *testing.T) {
	other, otherHits := counted(t, false, nil)
	src, _ := counted(t, false, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+planted, http.StatusTemporaryRedirect)
	})

	own := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
	c := ServiceClient(own)
	if own.Transport != nil {
		t.Fatal("the caller's client was changed")
	}
	if _, err := c.Get(src.URL); !IsRedirectRefused(err) {
		t.Fatalf("own CheckRedirect: err = %v", err)
	}
	if otherHits.Load() != 0 {
		t.Fatal("a client with its own CheckRedirect followed")
	}

	if n := ServiceClient(nil); n == nil || !Refuses(n.Transport) {
		t.Fatal("nil did not get a fresh client under the policy")
	}

	twice := ServiceClient(c)
	if twice.Transport != c.Transport {
		t.Fatal("wrapping twice added a layer")
	}
	if base, ok := Base(c.Transport); !ok || base != nil {
		t.Fatalf("base %T %v, want the client's own (nil)", base, ok)
	}

	rec := endpointcert.New()
	tagged := rec.Client(endpointcert.Of(endpointcert.Hub), &http.Client{})
	wrapped := ServiceClient(tagged)
	base, tag, ok := endpointcert.Unwrap(wrapped.Transport)
	if !ok || tag != endpointcert.Of(endpointcert.Hub) || !Refuses(wrapped.Transport) {
		t.Fatalf("tagged: %v %v, refuses %v", tag, ok, Refuses(wrapped.Transport))
	}
	if inner, ok := Base(base); !ok || inner != nil {
		t.Fatalf("tagged: the policy is not directly over the tag's own transport: %T", inner)
	}
	if ServiceClient(wrapped).Transport != wrapped.Transport {
		t.Fatal("wrapping a tagged client twice added a layer")
	}
	if _, err := wrapped.Get(src.URL); !IsRedirectRefused(err) {
		t.Fatalf("tagged: err = %v", err)
	}
	if otherHits.Load() != 0 {
		t.Fatal("a tagged client followed")
	}
}

// A hop from https to http another client refused, a participant's
// (smartauth.ErrCleartextRedirect alone), is not this policy's refusal: it
// keeps that client's own failure class.
func TestAParticipantsCleartextRefusalIsNotThisPolicys(t *testing.T) {
	err := fmt.Errorf("Get %q: %w", "https://system.example", smartauth.ErrCleartextRedirect)
	if IsRedirectRefused(err) || RedirectStatus(err) != 0 {
		t.Fatalf("a participant client's cleartext refusal read as this policy's: %v", err)
	}
}

// A 2xx and a 4xx pass through as answers.
func TestNonRedirectAnswersPass(t *testing.T) {
	for _, code := range []int{200, 204, 404, 500} {
		src, _ := counted(t, false, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) })
		resp, err := ServiceClient(nil).Get(src.URL)
		if err != nil || resp.StatusCode != code {
			t.Fatalf("%d: %v %v", code, resp, err)
		}
		resp.Body.Close()
	}
}
