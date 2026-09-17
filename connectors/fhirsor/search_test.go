package fhirsor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine"
)

const searchBasePath = "/fhir/TENANT-A"

func obs(id string) string {
	return `{"resourceType":"Observation","id":"` + id + `","status":"final","code":{"text":"x"},"subject":{"reference":"Patient/p1"}}`
}

func searchPage(next string, total string, resources ...string) string {
	var b strings.Builder
	b.WriteString("{\n  \"resourceType\" : \"Bundle\",\n  \"type\" : \"searchset\"")
	if total != "" {
		b.WriteString(",\n  \"total\" : " + total)
	}
	if next != "" {
		b.WriteString(",\n  \"link\" : [ { \"relation\" : \"self\", \"url\" : \"ignored\" }, { \"relation\" : \"next\", \"url\" : \"" + next + "\" } ]")
	}
	if len(resources) > 0 {
		b.WriteString(",\n  \"entry\" : [ ")
		for i, r := range resources {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString("{ \"fullUrl\" : \"x/" + fmt.Sprint(i) + "\", \"resource\" : " + r + ", \"search\" : { \"mode\" : \"match\" } }")
		}
		b.WriteString(" ]")
	}
	b.WriteString("\n}")
	return b.String()
}

// pagedServer serves pages[i] at searchBasePath with ?page=i (page 0 is the
// search itself). Each page's next link is built by link(srvURL, i+1) while
// more pages remain.
type pagedServer struct {
	srv   *httptest.Server
	hits  atomic.Int32
	mu    sync.Mutex
	paths []string
}

func newPagedServer(t *testing.T, handler func(p *pagedServer, w http.ResponseWriter, r *http.Request)) *pagedServer {
	t.Helper()
	p := &pagedServer{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.hits.Add(1)
		p.mu.Lock()
		p.paths = append(p.paths, r.URL.RequestURI())
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/fhir+json")
		handler(p, w, r)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *pagedServer) sor() *SoR { return NewFromURL(p.srv.URL+searchBasePath, p.srv.Client()) }

// servePages answers the search and each page=N request from pages.
func servePages(pages func(base string, i int) (string, bool)) func(p *pagedServer, w http.ResponseWriter, r *http.Request) {
	return func(p *pagedServer, w http.ResponseWriter, r *http.Request) {
		i := 0
		if v := r.URL.Query().Get("page"); v != "" {
			fmt.Sscan(v, &i)
		}
		body, ok := pages(p.srv.URL, i)
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}
}

func nextURL(base string, i int) string {
	return fmt.Sprintf("%s%s?page=%d", base, searchBasePath, i)
}

func wantOutcome(t *testing.T, err error, want engine.SearchOutcome) {
	t.Helper()
	var se *engine.SearchError
	if !errors.As(err, &se) {
		t.Fatalf("error = %v, want a SearchError %q", err, want)
	}
	if se.Outcome != want {
		t.Fatalf("outcome = %q (%s), want %q", se.Outcome, se.Reason, want)
	}
}

func TestSearch_QueryIsExactlyTheTemplate(t *testing.T) {
	p := newPagedServer(t, servePages(func(_ string, i int) (string, bool) { return searchPage("", "", obs("o1")), i == 0 }))
	if _, err := p.sor().SearchPatientContext(context.Background(), "Observation", "p1"); err != nil {
		t.Fatal(err)
	}
	if len(p.paths) != 1 || p.paths[0] != searchBasePath+"/Observation?patient=Patient%2Fp1" {
		t.Fatalf("requests = %q", p.paths)
	}
}

func TestSearch_CoverageIncludesPayor(t *testing.T) {
	page := `{"resourceType":"Bundle","type":"searchset","entry":[` +
		`{"resource":{"resourceType":"Coverage","id":"c1","beneficiary":{"reference":"Patient/p1"},"payor":[{"reference":"Organization/o1"}]},"search":{"mode":"match"}},` +
		`{"resource":{"resourceType":"Organization","id":"o1"},"search":{"mode":"include"}}]}`
	p := newPagedServer(t, servePages(func(_ string, i int) (string, bool) { return page, i == 0 }))
	res, err := p.sor().SearchPatientContext(context.Background(), "Coverage", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.paths) != 1 || p.paths[0] != searchBasePath+"/Coverage?patient=Patient%2Fp1&_include=Coverage%3Apayor" {
		t.Fatalf("requests = %q", p.paths)
	}
	if len(res.Pages) != 1 || string(res.Pages[0]) != page || len(res.Entries) != 2 {
		t.Fatalf("result %+v", res)
	}
}

func TestSearch_SinglePageExactBytes(t *testing.T) {
	page := searchPage("", "", obs("o1"), obs("o2"))
	p := newPagedServer(t, servePages(func(_ string, i int) (string, bool) { return page, i == 0 }))
	res, err := p.sor().SearchPatientContext(context.Background(), "Observation", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Pages) != 1 || !bytes.Equal(res.Pages[0], []byte(page)) {
		t.Fatalf("page bytes changed:\n%s", res.Pages)
	}
	if res.Total != 2 || len(res.Entries) != 2 {
		t.Fatalf("entries = %+v total=%d", res.Entries, res.Total)
	}
	for i, e := range res.Entries {
		got := string(res.Pages[e.Page][e.Start:e.End])
		if !strings.HasPrefix(got, `{ "fullUrl" : "x/`+fmt.Sprint(i)) || !strings.HasSuffix(got, "}") {
			t.Fatalf("entry %d span = %q", i, got)
		}
	}
}

func TestSearch_ZeroMatches(t *testing.T) {
	p := newPagedServer(t, servePages(func(_ string, i int) (string, bool) { return searchPage("", "0"), i == 0 }))
	res, err := p.sor().SearchPatientContext(context.Background(), "Observation", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Pages) != 1 || len(res.Entries) != 0 || res.Total != 0 {
		t.Fatalf("result = %+v", res)
	}
}

func TestSearch_MultiplePagesKeepEachPage(t *testing.T) {
	pages := func(base string, i int) (string, bool) {
		switch i {
		case 0:
			return searchPage(nextURL(base, 1), "", obs("o1")), true
		case 1:
			return searchPage(nextURL(base, 2), "", obs("o2"), obs("o3")), true
		case 2:
			return searchPage("", "", obs("o4")), true
		}
		return "", false
	}
	p := newPagedServer(t, servePages(pages))
	res, err := p.sor().SearchPatientContext(context.Background(), "Observation", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Pages) != 3 || res.Total != 4 {
		t.Fatalf("pages=%d total=%d", len(res.Pages), res.Total)
	}
	for i := range 3 {
		want, _ := pages(p.srv.URL, i)
		if !bytes.Equal(res.Pages[i], []byte(want)) {
			t.Fatalf("page %d bytes changed", i)
		}
	}
	wantPages := []int{0, 1, 1, 2}
	for i, e := range res.Entries {
		if e.Page != wantPages[i] {
			t.Fatalf("entry %d on page %d, want %d", i, e.Page, wantPages[i])
		}
	}
}

// Bundle.total is never used to bound or count: a server claiming a huge
// total with two entries is two entries; one claiming zero with entries is
// not zero.
func TestSearch_BundleTotalIgnored(t *testing.T) {
	for _, total := range []string{"100000", "0", "\"x\""} {
		page := searchPage("", total, obs("o1"), obs("o2"))
		p := newPagedServer(t, servePages(func(_ string, i int) (string, bool) { return page, i == 0 }))
		res, err := p.sor().SearchPatientContext(context.Background(), "Observation", "p1")
		if err != nil {
			t.Fatalf("total %s: %v", total, err)
		}
		if res.Total != 2 {
			t.Fatalf("total %s: Total = %d", total, res.Total)
		}
	}
}

func TestSearch_RedirectRefused(t *testing.T) {
	var target atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target.Add(1)
		_, _ = w.Write([]byte(searchPage("", "", obs("o1"))))
	}))
	t.Cleanup(other.Close)
	p := newPagedServer(t, func(_ *pagedServer, w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+searchBasePath+"/Observation", http.StatusFound)
	})
	_, err := p.sor().SearchPatientContext(context.Background(), "Observation", "p1")
	wantOutcome(t, err, engine.SearchMalformed)
	if target.Load() != 0 {
		t.Fatal("the redirect was followed")
	}
}

func TestSearch_NextLinkOutsideTheSystemOfRecordRefused(t *testing.T) {
	var foreign atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreign.Add(1)
		_, _ = w.Write([]byte(searchPage("", "", obs("o2"))))
	}))
	t.Cleanup(other.Close)
	cases := map[string]func(base string) string{
		"cross origin": func(string) string { return other.URL + searchBasePath + "?page=1" },
		"other scheme": func(base string) string {
			return strings.Replace(base, "http://", "https://", 1) + searchBasePath + "?page=1"
		},
		"other port":           func(base string) string { return otherPort(base) + searchBasePath + "?page=1" },
		"same host other path": func(base string) string { return base + "/fhir/TENANT-B?page=1" },
		"path prefix only":     func(base string) string { return base + "/fhir/TENANT-AB?page=1" },
		"parent path":          func(base string) string { return base + "/fhir?page=1" },
		"user info": func(base string) string {
			return strings.Replace(base, "http://", "http://u:p@", 1) + searchBasePath + "?page=1"
		},
		"relative":  func(string) string { return "Observation?page=1" },
		"not a URL": func(string) string { return "http://%zz" },
		// Dot segments, plain or percent-encoded, and encoded slashes can
		// walk out of the base path once the server decodes them.
		"encoded dot-dot":       func(base string) string { return base + searchBasePath + "/%2e%2e/TENANT-B?page=1" },
		"upper encoded dot-dot": func(base string) string { return base + searchBasePath + "/%2E%2E/TENANT-B?page=1" },
		"mixed dot-dot":         func(base string) string { return base + searchBasePath + "/.%2e/TENANT-B?page=1" },
		"encoded slash":         func(base string) string { return base + searchBasePath + "/..%2fTENANT-B?page=1" },
		"encoded slash, upper":  func(base string) string { return base + searchBasePath + "/x%2Fy?page=1" },
		"encoded dot":           func(base string) string { return base + searchBasePath + "/%2e/x?page=1" },
		"plain dot-dot":         func(base string) string { return base + searchBasePath + "/../TENANT-B?page=1" },
		"trailing dot":          func(base string) string { return base + searchBasePath + "/.?page=1" },
		"trailing dot-dot":      func(base string) string { return base + searchBasePath + "/..?page=1" },
		"trailing encoded dot":  func(base string) string { return base + searchBasePath + "/%2E?page=1" },
		"encoded backslash":     func(base string) string { return base + searchBasePath + "/..%5cTENANT-B?page=1" },
	}
	for name, link := range cases {
		t.Run(name, func(t *testing.T) {
			p := newPagedServer(t, servePages(func(base string, i int) (string, bool) {
				if i == 0 {
					return searchPage(link(base), "", obs("o1")), true
				}
				return searchPage("", "", obs("o2")), true
			}))
			_, err := p.sor().SearchPatientContext(context.Background(), "Observation", "p1")
			wantOutcome(t, err, engine.SearchMalformed)
			if p.hits.Load() != 1 || foreign.Load() != 0 {
				t.Fatalf("followed the link: local=%d foreign=%d", p.hits.Load(), foreign.Load())
			}
		})
	}
}

// otherPort returns base with its port changed.
func otherPort(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		panic(err)
	}
	port, _ := strconv.Atoi(u.Port())
	u.Host = u.Hostname() + ":" + strconv.Itoa(port+1)
	return u.String()
}

func TestSearch_NextLinkUnderTheBasePathFollowed(t *testing.T) {
	p := newPagedServer(t, servePages(func(base string, i int) (string, bool) {
		switch i {
		case 0:
			return searchPage(base+searchBasePath+"/Observation?page=1", "", obs("o1")), true
		case 1:
			return searchPage("", "", obs("o2")), true
		}
		return "", false
	}))
	res, err := p.sor().SearchPatientContext(context.Background(), "Observation", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Pages) != 2 {
		t.Fatalf("pages = %d", len(res.Pages))
	}
}

func TestSearch_CycleStops(t *testing.T) {
	for name, back := range map[string]int{"self": 1, "first page": 0} {
		t.Run(name, func(t *testing.T) {
			p := newPagedServer(t, servePages(func(base string, i int) (string, bool) {
				switch i {
				case 0:
					return searchPage(nextURL(base, 1), "", obs("o1")), true
				case 1:
					if back == 0 {
						return searchPage(p0(base), "", obs("o2")), true
					}
					return searchPage(nextURL(base, 1), "", obs("o2")), true
				}
				return "", false
			}))
			_, err := p.sor().SearchPatientContext(context.Background(), "Observation", "p1")
			wantOutcome(t, err, engine.SearchMalformed)
			if p.hits.Load() != 2 {
				t.Fatalf("requests = %d, want 2", p.hits.Load())
			}
		})
	}
}

// p0 is the URL of the first request, as the server would echo it.
func p0(base string) string { return base + searchBasePath + "/Observation?patient=Patient%2Fp1" }

func TestSearch_PageBound(t *testing.T) {
	p := newPagedServer(t, servePages(func(base string, i int) (string, bool) {
		return searchPage(nextURL(base, i+1), "", obs(fmt.Sprint("o", i))), true
	}))
	_, err := p.sor().SearchPatientContext(context.Background(), "Observation", "p1")
	wantOutcome(t, err, engine.SearchBound)
	if got := p.hits.Load(); got != engine.SoRSearchMaxPages {
		t.Fatalf("requests = %d, want %d", got, engine.SoRSearchMaxPages)
	}
}

func TestSearch_TenPagesWithinBound(t *testing.T) {
	p := newPagedServer(t, servePages(func(base string, i int) (string, bool) {
		next := nextURL(base, i+1)
		if i == engine.SoRSearchMaxPages-1 {
			next = ""
		}
		return searchPage(next, "", obs(fmt.Sprint("o", i))), true
	}))
	res, err := p.sor().SearchPatientContext(context.Background(), "Observation", "p1")
	if err != nil || len(res.Pages) != engine.SoRSearchMaxPages {
		t.Fatalf("pages=%d err=%v", len(res.Pages), err)
	}
}

func TestSearch_EntryBound(t *testing.T) {
	many := func(n, from int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = obs(fmt.Sprint("o", from+i))
		}
		return out
	}
	for name, tc := range map[string]struct {
		first, second int
		want          engine.SearchOutcome
	}{
		"one page over":    {201, 0, engine.SearchBound},
		"two pages over":   {150, 51, engine.SearchBound},
		"exactly at":       {150, 50, engine.SearchOK},
		"one page exactly": {200, 0, engine.SearchOK},
	} {
		t.Run(name, func(t *testing.T) {
			p := newPagedServer(t, servePages(func(base string, i int) (string, bool) {
				switch {
				case i == 0 && tc.second == 0:
					return searchPage("", "", many(tc.first, 0)...), true
				case i == 0:
					return searchPage(nextURL(base, 1), "", many(tc.first, 0)...), true
				case i == 1:
					return searchPage("", "", many(tc.second, tc.first)...), true
				}
				return "", false
			}))
			res, err := p.sor().SearchPatientContext(context.Background(), "Observation", "p1")
			if tc.want == engine.SearchOK {
				if err != nil || res.Total != tc.first+tc.second {
					t.Fatalf("total=%d err=%v", res.Total, err)
				}
				return
			}
			wantOutcome(t, err, tc.want)
		})
	}
}

func TestSearch_SizeBound(t *testing.T) {
	big := func(id string, n int) string {
		return `{"resourceType":"Observation","id":"` + id + `","status":"final","code":{"text":"` + strings.Repeat("a", n) + `"}}`
	}
	for name, tc := range map[string]struct {
		pages []int // text sizes per page
		want  engine.SearchOutcome
	}{
		"one page over": {[]int{engine.SoRSearchMaxBytes}, engine.SearchBound},
		"summed over":   {[]int{engine.SoRSearchMaxBytes / 2, engine.SoRSearchMaxBytes / 2}, engine.SearchBound},
		"within":        {[]int{engine.SoRSearchMaxBytes / 4, engine.SoRSearchMaxBytes / 4}, engine.SearchOK},
	} {
		t.Run(name, func(t *testing.T) {
			p := newPagedServer(t, servePages(func(base string, i int) (string, bool) {
				if i >= len(tc.pages) {
					return "", false
				}
				next := ""
				if i < len(tc.pages)-1 {
					next = nextURL(base, i+1)
				}
				return searchPage(next, "", big(fmt.Sprint("o", i), tc.pages[i])), true
			}))
			_, err := p.sor().SearchPatientContext(context.Background(), "Observation", "p1")
			if tc.want == engine.SearchOK {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			wantOutcome(t, err, tc.want)
		})
	}
}

// fakeClock advances only when the server says so, so the time bound is
// tested without waiting.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestSearch_TimeBound(t *testing.T) {
	for name, tc := range map[string]struct {
		step time.Duration
		want engine.SearchOutcome
	}{
		"over":   {3 * time.Second, engine.SearchUnavailable},
		"within": {time.Second, engine.SearchOK},
	} {
		t.Run(name, func(t *testing.T) {
			clock := &fakeClock{now: time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)}
			p := newPagedServer(t, servePages(func(base string, i int) (string, bool) {
				clock.Advance(tc.step)
				switch i {
				case 0, 1:
					return searchPage(nextURL(base, i+1), "", obs(fmt.Sprint("o", i))), true
				case 2:
					return searchPage("", "", obs("o2")), true
				}
				return "", false
			}))
			s := p.sor()
			s.now = clock.Now
			_, err := s.SearchPatientContext(context.Background(), "Observation", "p1")
			if tc.want == engine.SearchOK {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			wantOutcome(t, err, tc.want)
			wantReason(t, err, "time bound")
			if p.hits.Load() != 2 {
				t.Fatalf("requests = %d, want 2 (no page after the bound)", p.hits.Load())
			}
		})
	}
}

// A last page that arrives after the time bound is not used either.
func TestSearch_TimeBoundOnLastPage(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)}
	p := newPagedServer(t, servePages(func(base string, i int) (string, bool) {
		switch i {
		case 0:
			clock.Advance(time.Second)
			return searchPage(nextURL(base, 1), "", obs("o0")), true
		case 1:
			clock.Advance(5 * time.Second)
			return searchPage("", "", obs("o1")), true
		}
		return "", false
	}))
	s := p.sor()
	s.now = clock.Now
	_, err := s.SearchPatientContext(context.Background(), "Observation", "p1")
	wantOutcome(t, err, engine.SearchUnavailable)
	wantReason(t, err, "time bound")
}

// The other bounds stay "bound", never "unavailable".
func wantReason(t *testing.T, err error, reason string) {
	t.Helper()
	var se *engine.SearchError
	if !errors.As(err, &se) || se.Reason != reason {
		t.Fatalf("error = %v, want reason %q", err, reason)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

type failingTransport struct{ err error }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, f.err }

func TestSearch_UnavailableOnServerErrorOrTimeout(t *testing.T) {
	for _, code := range []int{500, 502, 503, 504, 429, 401, 403} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			p := newPagedServer(t, func(_ *pagedServer, w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
				_, _ = w.Write([]byte(`{"resourceType":"OperationOutcome"}`))
			})
			_, err := p.sor().SearchPatientContext(context.Background(), "Observation", "p1")
			wantOutcome(t, err, engine.SearchUnavailable)
		})
	}
	t.Run("second page fails", func(t *testing.T) {
		p := newPagedServer(t, func(p *pagedServer, w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page") == "" {
				_, _ = w.Write([]byte(searchPage(nextURL(p.srv.URL, 1), "", obs("o1"))))
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		})
		_, err := p.sor().SearchPatientContext(context.Background(), "Observation", "p1")
		wantOutcome(t, err, engine.SearchUnavailable)
	})
	for name, rtErr := range map[string]error{
		"transport timeout":  timeoutErr{},
		"deadline":           context.DeadlineExceeded,
		"connection refused": errors.New("connection refused"),
	} {
		t.Run(name, func(t *testing.T) {
			s := NewFromURL("http://sor.invalid"+searchBasePath, &http.Client{Transport: failingTransport{rtErr}})
			_, err := s.SearchPatientContext(context.Background(), "Observation", "p1")
			wantOutcome(t, err, engine.SearchUnavailable)
		})
	}
	t.Run("caller cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		p := newPagedServer(t, servePages(func(_ string, i int) (string, bool) { return searchPage("", "", obs("o1")), true }))
		_, err := p.sor().SearchPatientContext(ctx, "Observation", "p1")
		wantOutcome(t, err, engine.SearchUnavailable)
	})
}

func TestSearch_UnsupportedSearch(t *testing.T) {
	for _, code := range []int{400, 404, 405, 501} {
		p := newPagedServer(t, func(_ *pagedServer, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) })
		_, err := p.sor().SearchPatientContext(context.Background(), "Observation", "p1")
		wantOutcome(t, err, engine.SearchUnsupported)
	}
}

// escapedI is the JSON escape for "i", built at run time so no tool can
// turn it into the letter.
var escapedI = string([]byte{'\\', 'u', '0', '0', '6', '9'})

func TestSearch_Malformed(t *testing.T) {
	cases := map[string]string{
		"not JSON":               `not json`,
		"trailing document":      searchPage("", "", obs("o1")) + `{}`,
		"not a Bundle":           obs("o1"),
		"not a searchset":        `{"resourceType":"Bundle","type":"collection","entry":[{"resource":` + obs("o1") + `}]}`,
		"duplicate top key":      `{"resourceType":"Bundle","type":"searchset","type":"searchset"}`,
		"duplicate nested key":   `{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"Observation","id":"a","id":"b"}}]}`,
		"escaped duplicate key":  `{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"Observation","id":"a","` + escapedI + `d":"b"}}]}`,
		"entry not an object":    `{"resourceType":"Bundle","type":"searchset","entry":[1]}`,
		"entry without resource": `{"resourceType":"Bundle","type":"searchset","entry":[{"fullUrl":"x"}]}`,
		"wrong match type":       `{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"Patient","id":"p1"}}]}`,
		"two next links":         `{"resourceType":"Bundle","type":"searchset","link":[{"relation":"next","url":"a"},{"relation":"next","url":"b"}]}`,
		"entry not an array":     `{"resourceType":"Bundle","type":"searchset","entry":{}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if name == "escaped duplicate key" && (escapedI[0] != 0x5c || !strings.Contains(body, escapedI)) {
				t.Fatal("fixture lost its escape")
			}
			p := newPagedServer(t, func(_ *pagedServer, w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
			_, err := p.sor().SearchPatientContext(context.Background(), "Observation", "p1")
			wantOutcome(t, err, engine.SearchMalformed)
		})
	}
}

func TestSearch_IncludedAndOutcomeEntriesKept(t *testing.T) {
	page := `{"resourceType":"Bundle","type":"searchset","entry":[` +
		`{"resource":` + obs("o1") + `,"search":{"mode":"match"}},` +
		`{"resource":{"resourceType":"OperationOutcome","issue":[]},"search":{"mode":"outcome"}}]}`
	p := newPagedServer(t, servePages(func(_ string, i int) (string, bool) { return page, i == 0 }))
	res, err := p.sor().SearchPatientContext(context.Background(), "Observation", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 2 || !bytes.Equal(res.Pages[0], []byte(page)) {
		t.Fatalf("result = %+v", res)
	}
}

func TestSearch_InvalidInputNeverSent(t *testing.T) {
	p := newPagedServer(t, servePages(func(_ string, i int) (string, bool) { return searchPage("", ""), true }))
	for _, in := range [][2]string{{"Observation", "p1&_include=*"}, {"Observation", ""}, {"observation", "p1"}, {"Observation/x", "p1"}} {
		_, err := p.sor().SearchPatientContext(context.Background(), in[0], in[1])
		wantOutcome(t, err, engine.SearchMalformed)
	}
	if p.hits.Load() != 0 {
		t.Fatalf("requests = %d", p.hits.Load())
	}
}

// The search client never changes the caller's http.Client.
func TestSearch_CallerClientUnchanged(t *testing.T) {
	p := newPagedServer(t, func(_ *pagedServer, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	})
	hc := p.srv.Client()
	if hc.CheckRedirect != nil {
		t.Fatal("test client already refuses redirects")
	}
	_, err := NewFromURL(p.srv.URL+searchBasePath, hc).SearchPatientContext(context.Background(), "Observation", "p1")
	wantOutcome(t, err, engine.SearchMalformed)
	if hc.CheckRedirect != nil {
		t.Fatal("caller client modified")
	}
	// The caller's own client still follows redirects.
	resp, err := hc.Get(p.srv.URL + "/start")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if p.hits.Load() < 3 {
		t.Fatalf("caller client did not follow its redirect (requests = %d)", p.hits.Load())
	}
}

// A 3xx without a Location is not followed or read either.
func TestSearch_RedirectStatusWithoutLocation(t *testing.T) {
	p := newPagedServer(t, func(_ *pagedServer, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusFound)
		_, _ = w.Write([]byte(searchPage("", "", obs("o1"))))
	})
	_, err := p.sor().SearchPatientContext(context.Background(), "Observation", "p1")
	wantOutcome(t, err, engine.SearchMalformed)
}

func TestSameSystemOfRecord(t *testing.T) {
	cases := []struct {
		base, link string
		ok         bool
	}{
		{"http://sor.example/fhir/T", "http://sor.example/fhir/T?page=2", true},
		{"http://sor.example/fhir/T", "http://SOR.Example/fhir/T?page=2", true},
		{"http://sor.example/fhir/T", "HTTP://sor.example/fhir/T?page=2", true},
		{"http://sor.example/fhir/T", "http://sor.example:80/fhir/T?page=2", true},
		{"https://sor.example:443/fhir/T", "https://sor.example/fhir/T/x?page=2", true},
		{"http://sor.example", "http://sor.example/Observation?page=2", true},
		{"http://sor.example/fhir/T", "http://sor.example:8080/fhir/T?page=2", false},
		{"https://sor.example/fhir/T", "https://sor.example:80/fhir/T?page=2", false},
		{"http://sor.example/fhir/T", "http://sor.example/fhir/T/%2e%2e/U", false},
		{"http://sor.example/fhir/T", "http://sor.example/fhir/T/a%2Fb", false},
		{"http://sor.example/fhir/T", "http://sor.example/fhir/T/.", false},
		{"http://sor.example/fhir/T", "http://sor.example/fhir/T/./x", false},
		{"http://sor.example/fhir/T", "http://sor.example/fhir/T#frag", true},
	}
	for _, tc := range cases {
		base, err := url.Parse(tc.base)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := sameSystemOfRecord(base, tc.link); ok != tc.ok {
			t.Errorf("%s under %s: ok=%v, want %v", tc.link, tc.base, ok, tc.ok)
		}
	}
}

// The first request's URL is compared in the same form as later links, so
// a next link that spells it differently still stops the search.
func TestSearch_CycleThroughRespelledFirstURL(t *testing.T) {
	p := newPagedServer(t, servePages(func(base string, i int) (string, bool) {
		if i == 0 {
			if base == "" {
				return "", false
			}
			return searchPage(strings.Replace(base, "http://", "HTTP://", 1)+searchBasePath+"/Observation?patient=Patient/p1#again", "", obs("o1")), true
		}
		return searchPage("", "", obs("o2")), true
	}))
	_, err := p.sor().SearchPatientContext(context.Background(), "Observation", "p1")
	wantOutcome(t, err, engine.SearchMalformed)
	if p.hits.Load() != 1 {
		t.Fatalf("requests = %d, want 1", p.hits.Load())
	}
}

// A date range narrows the search with the type's date search parameter.
func TestSearch_DateRangeNarrowsTheQuery(t *testing.T) {
	p := newPagedServer(t, servePages(func(_ string, i int) (string, bool) { return searchPage("", "", obs("o1")), i == 0 }))
	_, err := p.sor().SearchPatientContext(context.Background(), "Observation", "p1",
		engine.SearchDateRange{Param: "date", From: "2024-01-01", To: "2026-09-17"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.paths) != 1 || p.paths[0] != searchBasePath+"/Observation?date=ge2024-01-01&date=le2026-09-17&patient=Patient%2Fp1" {
		t.Fatalf("requests = %q", p.paths)
	}
	for _, bad := range []engine.SearchDateRange{
		{Param: "date", From: "2024-01-01&_include=*"},
		{Param: "date", To: "tomorrow"},
		{Param: "patient", From: "2024"},
		{Param: "_lastUpdated", From: "2024"},
		{Param: "", From: "2024"},
	} {
		if _, err := p.sor().SearchPatientContext(context.Background(), "Observation", "p1", bad); err == nil {
			t.Errorf("range %+v accepted", bad)
		} else {
			wantOutcome(t, err, engine.SearchMalformed)
		}
	}
	if len(p.paths) != 1 {
		t.Fatalf("an invalid range was sent: %q", p.paths)
	}
}
