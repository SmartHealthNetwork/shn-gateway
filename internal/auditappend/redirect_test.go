package auditappend

import (
	"context"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The audit endpoint is a network service: a redirect it answers /append
// with is never followed, by the default client or one an embedding program
// passes, whatever its code or origin. The record stays queued and is
// retried, counted as redirect-refused, never as unreachable; no other
// origin, and no other path of the same one, receives anything; and the
// Location's path and query appear in no log line.
func TestAppendFollowsNoRedirect(t *testing.T) {
	var elsewhere atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) }))
	defer other.Close()
	const planted = "/secret-path?q=secret-query"
	rows := []struct {
		name     string
		tls      bool
		code     int
		location func(self string) string
	}{
		{"a cross-origin 307", false, http.StatusTemporaryRedirect, func(string) string { return other.URL + planted }},
		{"a cross-origin 308", false, http.StatusPermanentRedirect, func(string) string { return other.URL + planted }},
		{"a same-origin 302", false, http.StatusFound, func(string) string { return planted }},
		{"a same-origin 307 to another path", false, http.StatusTemporaryRedirect, func(self string) string { return self + planted }},
		{"https to http", true, http.StatusPermanentRedirect, func(string) string { return other.URL + planted }},
		{"a 300 with no Location", false, http.StatusMultipleChoices, func(string) string { return "" }},
	}
	for _, row := range rows {
		for _, client := range []struct {
			name string
			c    func(srv *httptest.Server) *http.Client
		}{
			{"default client", func(srv *httptest.Server) *http.Client {
				if row.tls {
					return srv.Client()
				}
				return nil
			}},
			{"an embedder's plain client", func(srv *httptest.Server) *http.Client {
				if row.tls {
					return &http.Client{Transport: srv.Client().Transport}
				}
				return &http.Client{}
			}},
		} {
			t.Run(row.name+"/"+client.name, func(t *testing.T) {
				elsewhere.Store(0)
				var otherPath atomic.Int64
				h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/append" {
						otherPath.Add(1)
						return
					}
					if loc := row.location("https://" + r.Host); loc != "" {
						if !row.tls && strings.HasPrefix(loc, "https://") {
							loc = "http://" + strings.TrimPrefix(loc, "https://")
						}
						w.Header().Set("Location", loc)
					}
					w.WriteHeader(row.code)
				})
				var srv *httptest.Server
				if row.tls {
					srv = httptest.NewTLSServer(h)
				} else {
					srv = httptest.NewServer(h)
				}
				defer srv.Close()
				s := NewMemStore()
				m := newCounts()
				a, err := New(Config{
					Holder: holder, SignPriv: testKey, Endpoint: srv.URL, Store: s, Metrics: m, Client: client.c(srv),
					Now:    func() time.Time { return time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC) },
					Jitter: rand.New(rand.NewPCG(1, 2)).Float64,
				})
				if err != nil {
					t.Fatal(err)
				}
				logs := logTo(a)
				a.Append(fresh())
				a.sendOne(context.Background(), 1)
				sent, retried, dropped := m.snapshot()
				if sent != 0 || retried[RetryRedirectRefused] != 1 || retried[RetryUnreachable] != 0 || len(dropped) != 0 {
					t.Fatalf("sent %d, retried %v, dropped %v: want one retry, redirect-refused", sent, retried, dropped)
				}
				if n, _ := s.Depth(context.Background()); n != 1 {
					t.Fatalf("Depth = %d, want the record still queued", n)
				}
				if elsewhere.Load() != 0 || otherPath.Load() != 0 {
					t.Fatalf("followed: the other origin got %d, another path %d", elsewhere.Load(), otherPath.Load())
				}
				for _, l := range logs.all() {
					if strings.Contains(l, "secret-path") || strings.Contains(l, "secret-query") {
						t.Fatalf("a log line names the Location's path or query: %q", l)
					}
				}
			})
		}
	}
}
