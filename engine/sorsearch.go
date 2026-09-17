package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

// SearchSystemOfRecord is an optional extension of SystemOfRecord: a bounded
// search of the participant's own FHIR server for one patient's records of a
// resource type. The gateway uses it to obtain CDS Hooks prefetch values the
// requesting system left out.
//
// SearchPatientContext runs exactly `<resourceType>?patient=Patient/<id>`
// (SoRSearchQuery), with the includes searchIncludes names for the type (a
// Coverage search includes each Coverage's payor Organization) and, for each
// date range given, the range on its date search parameter, against the system
// of record; follows the server's paging
// within the bounds below; and returns the raw bytes of every page with the
// span of every entry. A classified failure is returned as a *SearchError. A
// search that finds nothing is a result with no entries, not an error.
type SearchSystemOfRecord interface {
	SearchPatientContext(ctx context.Context, resourceType, sorPatientID string, dates ...SearchDateRange) (SearchResult, error)
}

// SearchDateRange narrows a search to records whose Param date search
// parameter falls within [From, To] (FHIR dates, either may be empty). The
// server's date matching only narrows the search; the gateway still selects
// records by their own dates.
type SearchDateRange struct {
	Param    string
	From, To string
}

// Search bounds. They are fixed. Exceeding the page, entry or size bound
// ends the search with SearchBound; running out of time ends it with
// SearchUnavailable (reason "time bound").
const (
	SoRSearchMaxPages    = 10
	SoRSearchMaxEntries  = 200
	SoRSearchMaxBytes    = 4 << 20
	SoRSearchMaxDuration = 5 * time.Second
)

// SearchOutcome classifies a system-of-record search.
type SearchOutcome string

const (
	// SearchOK: at least one match entry was found within the bounds.
	SearchOK SearchOutcome = "ok"
	// SearchZero: the search succeeded and found no match entry.
	SearchZero SearchOutcome = "zero"
	// SearchBound: the page, entry or size bound was exceeded.
	SearchBound SearchOutcome = "bound"
	// SearchUnavailable: a transport failure, a timeout (including the
	// search's own time bound), or an error status
	// that says the server could not answer.
	SearchUnavailable SearchOutcome = "unavailable"
	// SearchMalformed: the answer is not JSON, not a searchset Bundle, repeats
	// a member name, redirects, or pages outside the system of record.
	SearchMalformed SearchOutcome = "malformed"
	// SearchUnsupported: the connector cannot search, or the server does not
	// support the search.
	SearchUnsupported SearchOutcome = "unsupported"
	// SearchNotRun: the gateway did not run the search (the reason says
	// why); it is never a connector's answer.
	SearchNotRun SearchOutcome = "not-run"
)

// EntrySpan locates one Bundle entry object: Pages[Page][Start:End].
type EntrySpan struct {
	Page       int
	Start, End int
}

// SearchResult is a search's raw answer. Total is the number of entries the
// pages hold; a server's Bundle.total is never read.
type SearchResult struct {
	Pages   [][]byte
	Entries []EntrySpan
	Total   int
}

// SearchError is a classified search failure. Reason is safe to log: it
// never holds a URL, an identifier or response content.
type SearchError struct {
	Outcome SearchOutcome
	Reason  string
}

func (e *SearchError) Error() string {
	return "system of record search " + string(e.Outcome) + ": " + e.Reason
}

// SearchPage is what ParseSearchPage reads from one page.
type SearchPage struct {
	Entries []EntrySpan // Page is 0; the caller sets it
	// Resources[i] is the span of Entries[i]'s resource, and Match[i]
	// whether that entry is a match.
	Resources []EntrySpan
	Match     []bool
	Matches   int    // match entries: search mode "match", or no mode on a resource other than an OperationOutcome
	Next      string // the "next" link, or ""
	// modes[i] is Entries[i]'s search mode ("match", "include", "outcome",
	// or the mode the server gave); an entry without one is "match", or
	// "outcome" for an OperationOutcome.
	modes []string
}

// ParseSearchPage reads one searchset page strictly: one JSON document with
// no repeated member names, a Bundle of type searchset whose entries are
// objects each holding a resource. Every match entry's resource must be of
// resourceType. An entry is a match when its search mode is "match", or when
// it has no search mode and its resource is not an OperationOutcome. A failure is a *SearchError with SearchMalformed.
func ParseSearchPage(page []byte, resourceType string) (SearchPage, error) {
	bad := func(reason string) (SearchPage, error) {
		return SearchPage{}, &SearchError{Outcome: SearchMalformed, Reason: reason}
	}
	doc, err := relay.Doc(relay.NewBody(page, relay.OriginUpstreamResponse))
	if err != nil {
		if errors.Is(err, relay.ErrDuplicateKey) {
			return bad("repeated member name")
		}
		return bad("not a JSON document")
	}
	root := doc.Root()
	if doc.Kind(root) != relay.KindObject {
		return bad("not a Bundle")
	}
	if s, ok := stringMember(doc, root, "resourceType"); !ok || s != "Bundle" {
		return bad("not a Bundle")
	}
	if s, ok := stringMember(doc, root, "type"); !ok || s != "searchset" {
		return bad("not a searchset")
	}
	var out SearchPage
	if links, ok := doc.Member(root, "link"); ok {
		if doc.Kind(links) != relay.KindArray {
			return bad("link is not an array")
		}
		for _, l := range doc.Elems(links) {
			if doc.Kind(l) != relay.KindObject {
				return bad("link is not an object")
			}
			rel, _ := stringMember(doc, l, "relation")
			if rel != "next" {
				continue
			}
			u, ok := stringMember(doc, l, "url")
			if !ok || u == "" || out.Next != "" {
				return bad("unusable next link")
			}
			out.Next = u
		}
	}
	entries, ok := doc.Member(root, "entry")
	if !ok {
		return out, nil
	}
	if doc.Kind(entries) != relay.KindArray {
		return bad("entry is not an array")
	}
	for _, e := range doc.Elems(entries) {
		if doc.Kind(e) != relay.KindObject {
			return bad("entry is not an object")
		}
		res, ok := doc.Member(e, "resource")
		if !ok || doc.Kind(res) != relay.KindObject {
			return bad("entry has no resource")
		}
		rt, ok := stringMember(doc, res, "resourceType")
		if !ok || rt == "" {
			return bad("entry resource has no type")
		}
		mode := "match"
		if rt == "OperationOutcome" {
			mode = "outcome"
		}
		if search, ok := doc.Member(e, "search"); ok {
			if doc.Kind(search) != relay.KindObject {
				return bad("entry search is not an object")
			}
			if m, ok := doc.Member(search, "mode"); ok {
				v, err := doc.StringValue(m)
				if err != nil {
					return bad("entry search mode is not a string")
				}
				mode = v
			}
		}
		match := mode == "match"
		if match {
			if rt != resourceType {
				return bad("entry resource is not of the searched type")
			}
			out.Matches++
		}
		s, end := doc.Span(e)
		out.Entries = append(out.Entries, EntrySpan{Start: s, End: end})
		rs, re := doc.Span(res)
		out.Resources = append(out.Resources, EntrySpan{Start: rs, End: re})
		out.Match = append(out.Match, match)
		out.modes = append(out.modes, mode)
	}
	return out, nil
}

func stringMember(doc *relay.Document, obj relay.NodeID, key string) (string, bool) {
	n, ok := doc.Member(obj, key)
	if !ok || doc.Kind(n) != relay.KindString {
		return "", false
	}
	s, err := doc.StringValue(n)
	return s, err == nil
}

var (
	searchTypeRE  = regexp.MustCompile(`^[A-Z][A-Za-z]{1,63}$`)
	fhirIDRE      = regexp.MustCompile(`^[A-Za-z0-9\-.]{1,64}$`)
	searchParamRE = regexp.MustCompile(`^[a-z][a-z-]{0,63}$`)
	fhirDateRE    = regexp.MustCompile(`^[0-9]{4}(-[0-9]{2}(-[0-9]{2})?)?$`)
)

// searchIncludes are the _include values a search of a type carries. A payer
// resolves what a record references from the request alone (it has no route
// into the provider's system), so a Coverage search includes the payor
// Organization, and a DeviceRequest search each order's performer (the
// supplier a dispatched order names).
var searchIncludes = map[string]string{
	"Coverage":      "Coverage:payor",
	"DeviceRequest": "DeviceRequest:performer",
}

// SoRSearchQuery returns the relative search a connector runs, exactly the
// advertised prefetch template with the patient filled in, or an error when
// either input is not a FHIR type name or id.
func SoRSearchQuery(resourceType, sorPatientID string, dates ...SearchDateRange) (string, error) {
	bad := &SearchError{Outcome: SearchMalformed, Reason: "invalid search input"}
	if !searchTypeRE.MatchString(resourceType) || !fhirIDRE.MatchString(sorPatientID) {
		return "", bad
	}
	q := url.Values{"patient": {"Patient/" + sorPatientID}}
	for _, d := range dates {
		if !searchParamRE.MatchString(d.Param) || d.Param == "patient" || (d.From != "" && !fhirDateRE.MatchString(d.From)) || (d.To != "" && !fhirDateRE.MatchString(d.To)) {
			return "", bad
		}
		if d.From != "" {
			q.Add(d.Param, "ge"+d.From)
		}
		if d.To != "" {
			q.Add(d.Param, "le"+d.To)
		}
	}
	query := resourceType + "?" + q.Encode()
	if inc, ok := searchIncludes[resourceType]; ok {
		query += "&_include=" + url.QueryEscape(inc)
	}
	return query, nil
}

// sorSearchset is a search the gateway ran for a prefetch value, classified.
type sorSearchset struct {
	Outcome SearchOutcome
	Reason  string
	Query   string
	Pages   int
	Count   int
	// Value is the searchset to use, set only for SearchOK: the
	// sor-searchset assembly of the search's pages (assembleSoRSearchset),
	// however many pages the server returned.
	Value []byte
	// Payload seals Value (relay.OwnershipAuthored,
	// relay.BuilderSoRSearchset).
	Payload relay.Payload
	// pages, matches and includes are the search's pages, its match
	// entries' resources and the resources it included, in order, set for
	// SearchOK.
	pages    [][]byte
	matches  []searchMatch
	includes []searchMatch
}

// searchMatch is one match entry's resource: pages[page][start:end].
type searchMatch struct {
	page, start, end int
}

// searchSystemOfRecord runs a bounded patient search through sor and
// classifies the answer. It re-checks everything a connector reports: the
// bounds, each page's shape, and that the reported entry spans are exactly
// the entries each page holds.
func searchSystemOfRecord(ctx context.Context, sor SystemOfRecord, resourceType, sorPatientID string) sorSearchset {
	return runSoRSearch(ctx, sor, resourceType, sorPatientID, true)
}

// runSoRSearch is searchSystemOfRecord; assemble=false skips building the
// joined searchset (Value and Payload stay empty) for callers that use only
// the match entries.
func runSoRSearch(ctx context.Context, sor SystemOfRecord, resourceType, sorPatientID string, assemble bool, dates ...SearchDateRange) sorSearchset {
	query, err := SoRSearchQuery(resourceType, sorPatientID, dates...)
	if err != nil {
		return failedSearch("", err)
	}
	out := sorSearchset{Query: query}
	searcher, ok := sor.(SearchSystemOfRecord)
	if !ok {
		out.Outcome, out.Reason = SearchUnsupported, "connector does not search"
		return out
	}
	res, err := searcher.SearchPatientContext(ctx, resourceType, sorPatientID, dates...)
	if err != nil {
		return failedSearch(query, err)
	}
	return classifySearch(query, resourceType, res, assemble)
}

func failedSearch(query string, err error) sorSearchset {
	var se *SearchError
	if errors.As(err, &se) {
		return sorSearchset{Query: query, Outcome: se.Outcome, Reason: se.Reason}
	}
	return sorSearchset{Query: query, Outcome: SearchUnavailable, Reason: "search failed"}
}

func classifySearch(query, resourceType string, res SearchResult, assemble bool) sorSearchset {
	fail := func(o SearchOutcome, reason string) sorSearchset {
		return sorSearchset{Query: query, Pages: len(res.Pages), Outcome: o, Reason: reason}
	}
	out := sorSearchset{Query: query, Pages: len(res.Pages)}
	if len(res.Pages) == 0 {
		return fail(SearchMalformed, "no pages")
	}
	if len(res.Pages) > SoRSearchMaxPages {
		return fail(SearchBound, "page bound")
	}
	size := 0
	for _, p := range res.Pages {
		size += len(p)
	}
	if size > SoRSearchMaxBytes {
		return fail(SearchBound, "size bound")
	}
	var want []EntrySpan
	var carried []assemblyEntry
	matches := 0
	for i, p := range res.Pages {
		page, err := ParseSearchPage(p, resourceType)
		if err != nil {
			return failedSearch(query, err).withPages(len(res.Pages))
		}
		if (page.Next != "") != (i < len(res.Pages)-1) {
			return fail(SearchMalformed, "pages do not follow the next links")
		}
		for j, e := range page.Entries {
			e.Page = i
			want = append(want, e)
			r := page.Resources[j]
			r.Page = i
			if page.Match[j] {
				out.matches = append(out.matches, searchMatch{page: i, start: r.Start, end: r.End})
			}
			if page.modes[j] == "include" {
				out.includes = append(out.includes, searchMatch{page: i, start: r.Start, end: r.End})
			}
			if m := page.modes[j]; m == "match" || m == "include" {
				carried = append(carried, assemblyEntry{res: r, mode: m})
			}
		}
		matches += page.Matches
	}
	if len(want) > SoRSearchMaxEntries {
		return fail(SearchBound, "entry bound")
	}
	if len(want) != len(res.Entries) || res.Total != len(want) {
		return fail(SearchMalformed, "reported entries do not match the pages")
	}
	for i := range want {
		if want[i] != res.Entries[i] {
			return fail(SearchMalformed, "reported entries do not match the pages")
		}
	}
	out.Count = matches
	if matches == 0 {
		out.Outcome = SearchZero
		return out
	}
	out.pages = res.Pages
	if !assemble {
		out.Outcome = SearchOK
		return out
	}
	value, payload, err := assembleSoRSearchset(res.Pages, carried, matches)
	if err != nil {
		return fail(SearchMalformed, "assembly failed")
	}
	out.Outcome, out.Value, out.Payload = SearchOK, value, payload
	return out
}

func (s sorSearchset) withPages(n int) sorSearchset { s.Pages = n; return s }

const fhirJSON = "application/fhir+json"

// assembleFaultHook, when set by a test, changes the assembled bytes before
// they are sealed, to prove the embed verification catches it.
var assembleFaultHook func([]byte)

// assemblyEntry is one record an assembled searchset carries: the span of
// its resource in the search's pages, and its search mode ("match" or
// "include").
type assemblyEntry struct {
	res  EntrySpan
	mode string
}

// assembleSoRSearchset writes the searchset the gateway sends for a search it
// ran:
//
//	{"resourceType":"Bundle","type":"searchset","total":N,"entry":[
//	  {"fullUrl":"urn:uuid:<id>","resource":<record>,"search":{"mode":"match"}},…]}
//
// Each record (a match, or a record the search included, such as a
// Coverage's payor Organization) is copied byte for byte from its page and
// declared as an embed, which relay.Authored verifies. Nothing else of the
// server's answer is carried: not its links, its entry addresses, its
// Bundle id or meta, nor its messages about the search (OperationOutcome
// entries). The payer is never handed an address in the provider's system.
// Each entry's fullUrl is a urn:uuid the gateway derives from the record and
// its position (the same search answer gives the same bytes). total is the
// number of match entries.
func assembleSoRSearchset(pages [][]byte, entries []assemblyEntry, total int) (value []byte, sealed relay.Payload, err error) {
	bodies := make([]relay.Body, len(pages))
	for i, p := range pages {
		bodies[i] = relay.NewBody(p, relay.OriginUpstreamResponse)
	}
	b := []byte(`{"resourceType":"Bundle","type":"searchset","total":` + strconv.Itoa(total) + `,"entry":[`)
	embeds := make([]relay.Embed, 0, len(entries))
	for i, e := range entries {
		r := e.res
		if r.Page < 0 || r.Page >= len(pages) || r.Start < 0 || r.End > len(pages[r.Page]) || r.Start >= r.End {
			err = fmt.Errorf("entry %d out of range", i)
			return nil, sealed, err
		}
		if e.mode != "match" && e.mode != "include" {
			err = fmt.Errorf("entry %d: search mode %q is not carried", i, e.mode)
			return nil, sealed, err
		}
		record := pages[r.Page][r.Start:r.End]
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"fullUrl":"`+entryURN(i, record)+`","resource":`...)
		embeds = append(embeds, relay.Embed{Source: bodies[r.Page], Start: r.Start, End: r.End, At: len(b)})
		b = append(b, record...)
		b = append(b, `,"search":{"mode":"`+e.mode+`"}}`...)
	}
	b = append(b, "]}"...)
	if assembleFaultHook != nil {
		assembleFaultHook(b)
	}
	if sealed, err = relay.Authored(relay.BuilderSoRSearchset, b, fhirJSON, embeds...); err != nil {
		return nil, sealed, err
	}
	return b, sealed, nil
}

// entryURN is the fullUrl of the i-th entry of an assembled searchset: a
// urn:uuid (RFC 9562 version 8) derived from the position and the record.
func entryURN(i int, record []byte) string {
	h := sha256.New()
	h.Write([]byte(strconv.Itoa(i)))
	h.Write([]byte{0})
	h.Write(record)
	u := h.Sum(nil)[:16]
	u[6] = (u[6] & 0x0f) | 0x80
	u[8] = (u[8] & 0x3f) | 0x80
	x := hex.EncodeToString(u)
	return "urn:uuid:" + x[0:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:32]
}
