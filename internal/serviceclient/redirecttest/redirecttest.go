// Package redirecttest is the shared fixture for the rows that prove a
// network-service or sidecar client follows no redirect (serviceclient): an
// endpoint that answers every request with a redirect, a second origin and a
// second path that count what reaches them, and the planted path and query
// no record, answer or log line may name. Tests only.
package redirecttest

import (
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// Planted is the path and query every redirect's Location carries.
const Planted = "/secret-path?q=secret-query"

// The kinds of redirect a Case answers with.
const (
	CrossOrigin = iota // to another origin, with Planted
	SameOrigin         // to Planted, a path of the endpoint's own origin
	HTTPSToHTTP        // from the TLS endpoint to a plain origin
	NoLocation         // no Location at all
)

// Case is one redirect an endpoint answers with.
type Case struct {
	Name string
	Code int
	Kind int
}

// TLS reports whether the endpoint is served over https.
func (c Case) TLS() bool { return c.Kind == HTTPSToHTTP }

// Cases are the rows every client runs: a cross-origin 307 and 308, a
// same-origin 302 and 307, a 308 from https to http, and a 300 with no
// Location.
func Cases() []Case {
	return []Case{
		{"a cross-origin 307", http.StatusTemporaryRedirect, CrossOrigin},
		{"a cross-origin 308", http.StatusPermanentRedirect, CrossOrigin},
		{"a same-origin 302", http.StatusFound, SameOrigin},
		{"a same-origin 307", http.StatusTemporaryRedirect, SameOrigin},
		{"https to http", http.StatusPermanentRedirect, HTTPSToHTTP},
		{"a 300 with no Location", http.StatusMultipleChoices, NoLocation},
	}
}

// Fixture is a redirecting endpoint and the places a followed redirect
// would reach.
type Fixture struct {
	// URL is the endpoint's base URL: every path but Planted's answers the
	// case's redirect.
	URL string
	// Client is a plain client an embedding program might pass: no redirect
	// policy of its own, and trusting the endpoint's certificate for an https
	// case.
	Client *http.Client
	// Hits counts the requests the endpoint answered with its redirect.
	Hits      atomic.Int64
	cert      *x509.Certificate
	elsewhere atomic.Int64
	otherPath atomic.Int64
}

// Serve starts c's endpoint and its other origin, closed at the test's end.
func Serve(t testing.TB, c Case) *Fixture {
	t.Helper()
	f := &Fixture{}
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { f.elsewhere.Add(1) }))
	t.Cleanup(other.Close)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/secret-path" {
			f.otherPath.Add(1)
			return
		}
		f.Hits.Add(1)
		switch c.Kind {
		case CrossOrigin, HTTPSToHTTP:
			w.Header().Set("Location", other.URL+Planted)
		case SameOrigin:
			w.Header().Set("Location", Planted)
		}
		w.WriteHeader(c.Code)
	})
	var srv *httptest.Server
	if c.TLS() {
		srv = httptest.NewTLSServer(h)
		f.Client = &http.Client{Transport: srv.Client().Transport}
		f.cert = srv.Certificate()
	} else {
		srv = httptest.NewServer(h)
		f.Client = &http.Client{}
	}
	t.Cleanup(srv.Close)
	f.URL = srv.URL
	return f
}

// Certificate is the https endpoint's certificate, nil for a plain one.
func (f *Fixture) Certificate() *x509.Certificate { return f.cert }

// NotFollowed fails t when the redirect reached its other origin or the
// endpoint's other path, or the endpoint was never asked.
func (f *Fixture) NotFollowed(t testing.TB) {
	t.Helper()
	if n := f.elsewhere.Load(); n != 0 {
		t.Errorf("the redirect's other origin received %d requests", n)
	}
	if n := f.otherPath.Load(); n != 0 {
		t.Errorf("the redirect's other path received %d requests", n)
	}
	if f.Hits.Load() == 0 {
		t.Error("the endpoint was never asked: the row proves nothing")
	}
}

// NoSecret fails t when s, from where, names the planted path or query.
func NoSecret(t testing.TB, where, s string) {
	t.Helper()
	if strings.Contains(s, "secret-path") || strings.Contains(s, "secret-query") {
		t.Errorf("%s names the Location's path or query: %q", where, s)
	}
}
