package fhirsor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine"
)

var _ engine.SearchSystemOfRecord = (*SoR)(nil)

// errRedirectRefused stops the search client at the first redirect: a
// system-of-record search is answered by the configured server or not at all.
var errRedirectRefused = errors.New("fhirsor: redirect refused")

// searchClient returns a copy of hc that refuses every redirect. The caller's
// client is not changed; its transport (and any authorization it adds) is
// shared.
func searchClient(hc *http.Client) *http.Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	c := *hc
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return errRedirectRefused }
	return &c
}

func searchFailure(o engine.SearchOutcome, reason string) error {
	return &engine.SearchError{Outcome: o, Reason: reason}
}

// SearchPatientContext searches the system of record for one patient's
// records of resourceType: exactly `<resourceType>?patient=Patient/<id>`.
//
// Paging follows each page's next link only when the link has the configured
// base URL's scheme, host and port, no user information, and a path at or
// under the configured base path; a link already requested ends the search.
// Redirects are refused. The engine's bounds apply: page count, entry count
// and total bytes end the search as SearchBound; running out of time since
// the search began ends it as SearchUnavailable (reason "time bound"). The
// pages are returned byte for byte, with the span of every entry.
func (s *SoR) SearchPatientContext(ctx context.Context, resourceType, sorPatientID string, dates ...engine.SearchDateRange) (engine.SearchResult, error) {
	query, err := engine.SoRSearchQuery(resourceType, sorPatientID, dates...)
	if err != nil {
		return engine.SearchResult{}, err
	}
	base, err := url.Parse(s.fc.BaseURL())
	if err != nil || base.Host == "" {
		return engine.SearchResult{}, searchFailure(engine.SearchUnavailable, "system of record URL unusable")
	}
	now := s.now
	if now == nil {
		now = time.Now
	}
	start := now()
	remaining := func() time.Duration { return engine.SoRSearchMaxDuration - now().Sub(start) }
	sctx, cancel := context.WithTimeout(ctx, engine.SoRSearchMaxDuration)
	defer cancel()
	hc := searchClient(s.fc.HTTPClient())

	var res engine.SearchResult
	size := 0
	next := s.fc.BaseURL() + "/" + query
	seen := map[string]bool{}
	first, err := url.Parse(next)
	if err != nil {
		return engine.SearchResult{}, searchFailure(engine.SearchUnavailable, "system of record URL unusable")
	}
	for {
		if len(res.Pages) == engine.SoRSearchMaxPages {
			return engine.SearchResult{}, searchFailure(engine.SearchBound, "page bound")
		}
		if remaining() <= 0 {
			return engine.SearchResult{}, searchFailure(engine.SearchUnavailable, "time bound")
		}
		if len(res.Pages) == 0 {
			seen[pageKey(first)] = true
		}
		page, err := s.searchPage(ctx, sctx, hc, next, engine.SoRSearchMaxBytes-size)
		if err != nil {
			return engine.SearchResult{}, err
		}
		if remaining() < 0 {
			return engine.SearchResult{}, searchFailure(engine.SearchUnavailable, "time bound")
		}
		size += len(page)
		parsed, err := engine.ParseSearchPage(page, resourceType)
		if err != nil {
			return engine.SearchResult{}, err
		}
		for _, e := range parsed.Entries {
			e.Page = len(res.Pages)
			res.Entries = append(res.Entries, e)
		}
		res.Pages = append(res.Pages, page)
		if len(res.Entries) > engine.SoRSearchMaxEntries {
			return engine.SearchResult{}, searchFailure(engine.SearchBound, "entry bound")
		}
		if parsed.Next == "" {
			break
		}
		link, ok := sameSystemOfRecord(base, parsed.Next)
		if !ok {
			return engine.SearchResult{}, searchFailure(engine.SearchMalformed, "next link outside the system of record")
		}
		key := pageKey(link)
		if seen[key] {
			return engine.SearchResult{}, searchFailure(engine.SearchMalformed, "next link repeats a page")
		}
		seen[key] = true
		next = link.String()
	}
	res.Total = len(res.Entries)
	return res, nil
}

// searchPage GETs one page, reading at most limit bytes. A failure is
// classified: the search's own deadline is the time bound (unavailable); a transport
// failure, a caller cancellation or an error status the server may recover
// from is unavailable; any other error status means the server does not
// support the search; a redirect is malformed.
func (s *SoR) searchPage(parent, ctx context.Context, hc *http.Client, target string, limit int) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, searchFailure(engine.SearchMalformed, "unusable page URL")
	}
	req.Header.Set("Accept", "application/fhir+json")
	timedOut := func() bool { return ctx.Err() != nil && parent.Err() == nil }
	resp, err := hc.Do(req)
	if err != nil {
		switch {
		case errors.Is(err, errRedirectRefused):
			return nil, searchFailure(engine.SearchMalformed, "redirect refused")
		case timedOut():
			return nil, searchFailure(engine.SearchUnavailable, "time bound")
		}
		return nil, searchFailure(engine.SearchUnavailable, "system of record unavailable")
	}
	defer resp.Body.Close()
	switch code := resp.StatusCode; {
	case code/100 == 2:
	case code == http.StatusUnauthorized, code == http.StatusForbidden, code == http.StatusTooManyRequests, code >= 500 && code != http.StatusNotImplemented:
		return nil, searchFailure(engine.SearchUnavailable, "system of record unavailable")
	case code/100 == 3:
		return nil, searchFailure(engine.SearchMalformed, "redirect refused")
	default:
		return nil, searchFailure(engine.SearchUnsupported, "search not supported")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		if timedOut() {
			return nil, searchFailure(engine.SearchUnavailable, "time bound")
		}
		return nil, searchFailure(engine.SearchUnavailable, "system of record unavailable")
	}
	if len(body) > limit {
		return nil, searchFailure(engine.SearchBound, "size bound")
	}
	return body, nil
}

// sameSystemOfRecord resolves a next link and reports whether it stays on
// the configured system of record: the same scheme, host and port, no user
// information, and a path equal to or under the base path. Only absolute
// links are followed. A path holding a dot segment (`.` or `..`, plain or
// percent-encoded) or an encoded slash or backslash is refused, since a
// server may decode it into a path outside the base path.
func sameSystemOfRecord(base *url.URL, link string) (*url.URL, bool) {
	u, err := url.Parse(link)
	if err != nil || !u.IsAbs() || u.User != nil || u.Opaque != "" {
		return nil, false
	}
	if !strings.EqualFold(u.Scheme, base.Scheme) || !strings.EqualFold(u.Hostname(), base.Hostname()) || portOf(u) != portOf(base) {
		return nil, false
	}
	raw := strings.ToLower(u.EscapedPath())
	if strings.Contains(raw, "%2f") || strings.Contains(raw, "%5c") || strings.Contains(u.Path, "\\") {
		return nil, false
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == "." || seg == ".." {
			return nil, false
		}
	}
	basePath := strings.TrimRight(base.Path, "/")
	if basePath != "" && u.Path != basePath && !strings.HasPrefix(u.Path, basePath+"/") {
		return nil, false
	}
	u.Fragment, u.RawFragment = "", ""
	return u, true
}

// pageKey is the form in which page URLs are compared to stop a paging
// cycle: scheme and host case, a default port, query encoding and order do
// not make a page new.
func pageKey(u *url.URL) string {
	q, err := url.ParseQuery(u.RawQuery)
	query := u.RawQuery
	if err == nil {
		query = q.Encode()
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Hostname()) + ":" + portOf(u) + u.EscapedPath() + "?" + query
}

func portOf(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}
