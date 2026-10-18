package fhirsor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
	"github.com/SmartHealthNetwork/shn-gateway/engine"
	"github.com/SmartHealthNetwork/shn-gateway/internal/fhirclient"
)

// unpooled reads with a new connection each time: net/http sends a GET again
// by itself when a connection it reused drops, which would hide whether the
// connector tried again.
func unpooled() *http.Client {
	return &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
}

// triedAgain fails the test unless a second try came after the pause.
func triedAgain(t *testing.T, began time.Time) {
	t.Helper()
	if waited := time.Since(began); waited < fhirclient.RetryPause {
		t.Fatalf("answered after %s, before any second try", waited)
	}
}

// A system of record that drops a read's first try, or answers it as a proxy
// with no task to reach, answers the read on the second; a denial is
// a denial on the first, and an outage past the pause is still an outage.
func TestReadTriesOnceMoreOnlyAfterNoAnswerOrAProxyStatus(t *testing.T) {
	for _, c := range []struct {
		name     string
		first    int // 0: hang up without an answer
		then     int
		requests int32
		found    bool
		status   int // the gateway's answer; 0 when found
	}{
		{"dropped, then found", 0, 200, 2, true, 0},
		{"503, then found", 503, 200, 2, true, 0},
		{"502, then found", 502, 200, 2, true, 0},
		{"504, then found", 504, 200, 2, true, 0},
		{"503 twice", 503, 503, 2, false, 503},
		{"denied", 403, 200, 1, false, 502},
		{"unauthenticated", 401, 200, 1, false, 502},
		{"rate limited", 429, 200, 1, false, 503},
		{"server error", 500, 200, 1, false, 503},
	} {
		t.Run(c.name, func(t *testing.T) {
			var n atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				code := c.then
				if n.Add(1) == 1 {
					code = c.first
				}
				if code == 0 {
					conn, _, _ := w.(http.Hijacker).Hijack()
					_ = conn.Close()
					return
				}
				w.WriteHeader(code)
				if code == 200 {
					_, _ = w.Write([]byte(searchResource(failurePatient)))
				}
			}))
			defer srv.Close()
			began := time.Now()
			_, _, found, err := NewFromURL(srv.URL, unpooled()).ResolvePatientContext(context.Background(), "MBR-OP")
			if found != c.found || n.Load() != c.requests {
				t.Fatalf("found=%v after %d requests (%v)", found, n.Load(), err)
			}
			if c.requests == 2 {
				triedAgain(t, began)
			}
			if c.status == 0 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			assertFailure(t, err, c.status)
		})
	}
}

// A search page, the first or a next one, is tried once more the same way.
func TestSearchTriesAPageOnceMore(t *testing.T) {
	for _, c := range []struct {
		name     string
		page     string // the page whose first try fails
		first    int    // 0: hang up without an answer
		then     int
		requests int32
		want     engine.SearchOutcome // "" for the whole result
	}{
		{"first page 503", "", 503, 200, 3, ""},
		{"next page dropped", "1", 0, 200, 3, ""},
		{"next page 504", "1", 504, 200, 3, ""},
		{"first page 503 twice", "", 503, 503, 2, engine.SearchUnavailable},
		{"first page 404", "", 404, 200, 1, engine.SearchUnsupported},
		{"first page 403", "", 403, 200, 1, engine.SearchUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			var failed atomic.Int32
			p := newPagedServer(t, func(p *pagedServer, w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("page") == c.page {
					code := c.then
					if failed.Add(1) == 1 {
						code = c.first
					}
					if code == 0 {
						conn, _, _ := w.(http.Hijacker).Hijack()
						_ = conn.Close()
						return
					}
					if code != 200 {
						w.WriteHeader(code)
						return
					}
				}
				if r.URL.Query().Get("page") == "" {
					_, _ = w.Write([]byte(searchPage(nextURL(p.srv.URL, 1), "", obs("o1"))))
					return
				}
				_, _ = w.Write([]byte(searchPage("", "", obs("o2"))))
			})
			began := time.Now()
			res, err := NewFromURL(p.srv.URL+searchBasePath, unpooled()).SearchPatientContext(context.Background(), "Observation", "p1")
			if failed.Load() == 2 {
				triedAgain(t, began)
			}
			if p.hits.Load() != c.requests {
				t.Fatalf("%d requests, want %d (%v)", p.hits.Load(), c.requests, err)
			}
			if c.want != "" {
				wantOutcome(t, err, c.want)
				return
			}
			if err != nil || len(res.Entries) != 2 || len(res.Pages) != 2 {
				t.Fatalf("%+v %v", res, err)
			}
		})
	}
}

// A SMART token request that gets no answer is tried again with the read,
// under a new token request; one refused is not.
func TestReadTriesOnceMoreAfterATokenRequestWithNoAnswer(t *testing.T) {
	for _, c := range []struct {
		name          string
		dropFirst     bool // hang up on the first token request; else refuse it 401
		tokens, reads int32
		found         bool
	}{
		{"dropped", true, 2, 1, true},
		{"refused", false, 1, 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var tokens, reads atomic.Int32
			token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tokens.Add(1) == 1 {
					if c.dropFirst {
						conn, _, _ := w.(http.Hijacker).Hijack()
						_ = conn.Close()
						return
					}
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				_, _ = w.Write([]byte(`{"access_token":"private-token-sentinel","expires_in":3600}`))
			}))
			defer token.Close()
			fhir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads.Add(1)
				_, _ = w.Write([]byte(searchResource(failurePatient)))
			}))
			defer fhir.Close()
			hc, err := smartauth.NewHTTPClient(smartauth.Config{TokenURL: token.URL, ClientID: "client", ClientSecret: "private-secret-sentinel"})
			if err != nil {
				t.Fatal(err)
			}
			_, _, found, err := NewFromURL(fhir.URL, hc).ResolvePatientContext(context.Background(), "MBR-OP")
			if found != c.found || tokens.Load() != c.tokens || reads.Load() != c.reads {
				t.Fatalf("found=%v after %d token requests, %d reads (%v)", found, tokens.Load(), reads.Load(), err)
			}
			if !c.found {
				assertFailure(t, err, 502)
			}
		})
	}
}
