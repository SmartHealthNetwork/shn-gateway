package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

// nonSearchingSoR is the census without its search: a connector that cannot
// search.
type nonSearchingSoR struct {
	*censusSoR
	noSearch
}

// searchingSoR is the census plus a scripted search.
type searchingSoR struct {
	*censusSoR
	res   SearchResult
	err   error
	calls *int
}

func (s searchingSoR) SearchPatientContext(_ context.Context, resourceType, id string, _ ...SearchDateRange) (SearchResult, error) {
	if s.calls != nil {
		*s.calls++
	}
	return s.res, s.err
}

// lt is the JSON escape for "<", built at run time so no tool rewrites it.
var lt = string([]byte{0x5c, 'u', '0', '0', '3', 'c'})

func condition(id string) string {
	return "{\n    \"resourceType\" : \"Condition\",\n    \"id\" : \"" + id + "\",\n    \"note\" : [ { \"text\" : \"a " + lt + " b & c > d\" } ],\n    \"onsetQuantity\" : { \"value\" : 1.50 },\n    \"subject\" : { \"reference\" : \"Patient/p1\" }\n  }"
}

func page(total, next string, entries ...string) []byte {
	var b strings.Builder
	b.WriteString("{\r\n  \"resourceType\": \"Bundle\",\r\n\t\"type\": \"searchset\"")
	if total != "" {
		b.WriteString(",\r\n  \"total\": " + total)
	}
	if next != "" {
		b.WriteString(",\r\n  \"link\": [{\"relation\": \"next\", \"url\": \"" + next + "\"}]")
	}
	if len(entries) > 0 {
		b.WriteString(",\r\n  \"entry\": [\r\n    " + strings.Join(entries, ",\r\n    ") + "\r\n  ]")
	}
	b.WriteString("\r\n}")
	return []byte(b.String())
}

func matchEntry(res string) string {
	return "{ \"fullUrl\": \"https://sor.example/fhir/Condition/x\", \"resource\": " + res + ", \"search\": { \"mode\": \"match\", \"score\": 1 } }"
}

// resultOf builds the SearchResult a correct connector reports for pages.
func resultOf(t *testing.T, resourceType string, pages ...[]byte) SearchResult {
	t.Helper()
	var r SearchResult
	for i, p := range pages {
		parsed, err := ParseSearchPage(p, resourceType)
		if err != nil {
			t.Fatalf("page %d: %v", i, err)
		}
		for _, e := range parsed.Entries {
			e.Page = i
			r.Entries = append(r.Entries, e)
		}
		r.Pages = append(r.Pages, p)
	}
	r.Total = len(r.Entries)
	return r
}

func search(t *testing.T, sor SystemOfRecord) sorSearchset {
	t.Helper()
	return searchSystemOfRecord(context.Background(), sor, "Condition", "p1")
}

// assembledEntry is one entry of an assembled searchset, as a test reads it.
type assembledEntry struct {
	FullURL  string `json:"fullUrl"`
	Resource json.RawMessage
	Search   struct {
		Mode string `json:"mode"`
	} `json:"search"`
	Other map[string]json.RawMessage
}

var urnUUIDRE = regexp.MustCompile(`^urn:uuid:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// checkAssembly checks that value is the gateway's searchset assembly: only
// resourceType, type, total and entry at the top; each entry only a
// gateway-assigned urn:uuid fullUrl, the resource and its search mode; each
// resource a byte-identical copy of want[i] (the server's resource spans, in
// order); and no server address anywhere outside the copied resources.
func checkAssembly(t *testing.T, value []byte, total int, want []string, modes []string) {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(value, &top); err != nil {
		t.Fatalf("assembly is not JSON: %v\n%s", err, value)
	}
	for k := range top {
		if k != "resourceType" && k != "type" && k != "total" && k != "entry" {
			t.Fatalf("assembly carries %q:\n%s", k, value)
		}
	}
	if string(top["total"]) != strconv.Itoa(total) {
		t.Fatalf("total = %s, want %d", top["total"], total)
	}
	parsed, err := ParseSearchPage(value, "Condition")
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Resources) != len(want) {
		t.Fatalf("assembly has %d entries, want %d:\n%s", len(parsed.Resources), len(want), value)
	}
	seen := map[string]bool{}
	outside := bytes.Clone(value)
	for i, r := range parsed.Resources {
		if got := string(value[r.Start:r.End]); got != want[i] {
			t.Fatalf("entry %d resource is not an exact copy:\n got %s\nwant %s", i, got, want[i])
		}
		for k := r.Start; k < r.End; k++ {
			outside[k] = ' '
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(value[parsed.Entries[i].Start:parsed.Entries[i].End], &raw); err != nil {
			t.Fatal(err)
		}
		for k := range raw {
			if k != "fullUrl" && k != "resource" && k != "search" {
				t.Fatalf("entry %d carries %q", i, k)
			}
		}
		var e assembledEntry
		if err := json.Unmarshal(value[parsed.Entries[i].Start:parsed.Entries[i].End], &e); err != nil {
			t.Fatal(err)
		}
		if !urnUUIDRE.MatchString(e.FullURL) || seen[e.FullURL] {
			t.Fatalf("entry %d fullUrl %q is not a fresh urn:uuid", i, e.FullURL)
		}
		seen[e.FullURL] = true
		if e.Search.Mode != modes[i] {
			t.Fatalf("entry %d search mode %q, want %q", i, e.Search.Mode, modes[i])
		}
	}
	if bytes.Contains(outside, []byte("sor.example")) || bytes.Contains(outside, []byte("http")) {
		t.Fatalf("a server address is carried outside the records:\n%s", outside)
	}
}

// sorAssembly is the searchset the gateway sends for a search answered with
// pages: every match and included record, in order, each exactly as the
// server returned it.
func sorAssembly(t *testing.T, resourceType string, pages ...[]byte) string {
	t.Helper()
	total, n := 0, 0
	var entries []string
	for i, p := range pages {
		parsed, err := ParseSearchPage(p, resourceType)
		if err != nil {
			t.Fatalf("page %d: %v", i, err)
		}
		total += parsed.Matches
		for j, r := range parsed.Resources {
			mode := parsed.modes[j]
			if mode != "match" && mode != "include" {
				continue
			}
			rec := p[r.Start:r.End]
			entries = append(entries, `{"fullUrl":"`+entryURN(n, rec)+`","resource":`+string(rec)+`,"search":{"mode":"`+mode+`"}}`)
			n++
		}
	}
	return `{"resourceType":"Bundle","type":"searchset","total":` + strconv.Itoa(total) + `,"entry":[` + strings.Join(entries, ",") + `]}`
}

// resourceOf returns the resource span of an entry string built by the
// test helpers.
func resourceOf(t *testing.T, entry string) string {
	t.Helper()
	p, err := ParseSearchPage(page("", "", entry), "Condition")
	if err != nil || len(p.Resources) != 1 {
		t.Fatalf("entry %s: %v", entry, err)
	}
	b := page("", "", entry)
	return string(b[p.Resources[0].Start:p.Resources[0].End])
}

// A search the gateway obtains is always sent as its own searchset: each
// matched record exactly as the server returned it, under an entry address
// the gateway assigns, with its search mode — never the server's page, its
// links or its entry addresses (the payer is never handed a route into the
// provider's system).
func TestSoRSearch_SinglePageAssembledWithoutServerAddresses(t *testing.T) {
	c1, c2 := condition("c1"), condition("c2")
	p := page("7", "", matchEntry(c1), matchEntry(c2))
	p = bytes.Replace(p, []byte(`"type": "searchset"`), []byte(`"type": "searchset",  "id": "srv-page-1", "meta": {"lastUpdated": "2026-09-17T00:00:00Z"}, "link": [{"relation": "self", "url": "https://sor.example/fhir/Condition?patient=Patient/p1"}]`), 1)
	got := search(t, searchingSoR{censusSoR: newCensusSoR(), res: resultOf(t, "Condition", p)})
	if got.Outcome != SearchOK || got.Count != 2 || got.Pages != 1 {
		t.Fatalf("result = %+v", got)
	}
	checkAssembly(t, got.Value, 2, []string{c1, c2}, []string{"match", "match"})
	if string(got.Value) != sorAssembly(t, "Condition", p) {
		t.Fatalf("assembly layout:\n%s", got.Value)
	}
	if !strings.Contains(string(got.Value), lt) {
		t.Fatal("an escape was not kept")
	}
	if got.Payload.Ownership() != relay.OwnershipAuthored || got.Payload.Builder() != relay.BuilderSoRSearchset {
		t.Fatalf("payload = %v", got.Payload)
	}
	if !bytes.Equal(relay.BytesForTest(got.Payload), got.Value) {
		t.Fatal("payload and value differ")
	}
	if got.Query != "Condition?patient=Patient%2Fp1" {
		t.Fatalf("query = %q", got.Query)
	}
	again := search(t, searchingSoR{censusSoR: newCensusSoR(), res: resultOf(t, "Condition", p)})
	if !bytes.Equal(again.Value, got.Value) {
		t.Fatal("the assembly is not deterministic")
	}
}

// TestSoRSearch_IdenticalRecordsGetDistinctEntryAddresses: a server that
// returns the same record twice in one search gets two entries with distinct
// fullUrls (a Bundle's entry fullUrls must be unique, bdl-7).
func TestSoRSearch_IdenticalRecordsGetDistinctEntryAddresses(t *testing.T) {
	c1 := condition("c1")
	p := page("", "", matchEntry(c1), matchEntry(c1))
	got := search(t, searchingSoR{censusSoR: newCensusSoR(), res: resultOf(t, "Condition", p)})
	if got.Outcome != SearchOK || got.Count != 2 {
		t.Fatalf("result = %+v", got)
	}
	var b struct {
		Entry []struct {
			FullURL string `json:"fullUrl"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(got.Value, &b); err != nil || len(b.Entry) != 2 {
		t.Fatalf("assembly %s: %v", got.Value, err)
	}
	if b.Entry[0].FullURL == b.Entry[1].FullURL || !strings.HasPrefix(b.Entry[0].FullURL, "urn:uuid:") {
		t.Fatalf("entry addresses %q and %q", b.Entry[0].FullURL, b.Entry[1].FullURL)
	}
}

// Included records (a Coverage search's payor Organization) are carried as
// search mode include; the server's messages about the search
// (OperationOutcome entries) are not carried.
func TestSoRSearch_IncludedRecordsCarriedOutcomesLeftOut(t *testing.T) {
	c1 := condition("c1")
	org := `{"resourceType":"Organization","id":"payer-1","identifier":[{"system":"urn:x","value":"p"}]}`
	inc := `{"fullUrl":"https://sor.example/fhir/Organization/payer-1","resource":` + org + `,"search":{"mode":"include"}}`
	outcome := `{"fullUrl":"https://sor.example/x","resource":{"resourceType":"OperationOutcome","issue":[{"severity":"warning","code":"informational","diagnostics":"https://sor.example/fhir"}]},"search":{"mode":"outcome"}}`
	untagged := `{"resource":{"resourceType":"OperationOutcome","issue":[]}}`
	p := page("", "", matchEntry(c1), inc, outcome, untagged)
	got := search(t, searchingSoR{censusSoR: newCensusSoR(), res: resultOf(t, "Condition", p)})
	if got.Outcome != SearchOK || got.Count != 1 {
		t.Fatalf("result = %+v", got)
	}
	checkAssembly(t, got.Value, 1, []string{c1, org}, []string{"match", "include"})
	if bytes.Contains(got.Value, []byte("OperationOutcome")) {
		t.Fatalf("a search message was carried:\n%s", got.Value)
	}
}

func TestSoRSearch_MultiplePagesAssembledFromExactEntries(t *testing.T) {
	c1, c2, c3 := condition("c1"), condition("c2"), condition("c3")
	e1, e2, e3 := matchEntry(c1), matchEntry(c2), matchEntry(c3)
	outcome := `{"resource":{"resourceType":"OperationOutcome","issue":[]},"search":{"mode":"outcome"}}`
	p1 := page("999", "https://sor.example/fhir?page=2", e1)
	p2 := page("999", "https://sor.example/fhir?page=3", e2, outcome)
	p3 := page("", "", e3)
	got := search(t, searchingSoR{censusSoR: newCensusSoR(), res: resultOf(t, "Condition", p1, p2, p3)})
	if got.Outcome != SearchOK || got.Count != 3 || got.Pages != 3 {
		t.Fatalf("result = %+v", got)
	}
	checkAssembly(t, got.Value, 3, []string{c1, c2, c3}, []string{"match", "match", "match"})
	if got.Payload.Ownership() != relay.OwnershipAuthored || got.Payload.Builder() != relay.BuilderSoRSearchset {
		t.Fatalf("payload = %v", got.Payload)
	}
	if !bytes.Equal(relay.BytesForTest(got.Payload), got.Value) {
		t.Fatal("payload and value differ")
	}
	_ = resourceOf
}

// The assembly's embeds are verified: a copy that differs from its source by
// one byte, or points at a different span, is refused.
func TestSoRSearch_AssemblyEmbedsVerified(t *testing.T) {
	p1 := page("", "n", matchEntry(condition("c1")))
	p2 := page("", "", matchEntry(condition("c2")))
	res := resultOf(t, "Condition", p1, p2)
	entries := assemblyEntriesOf(t, res)
	if _, _, err := assembleSoRSearchset(res.Pages, entries, 2); err != nil {
		t.Fatal(err)
	}
	tampered := [][]byte{bytes.Clone(p1), p2}
	e := entries[0].res
	idx := bytes.Index(tampered[0][e.Start:e.End], []byte("c1")) + e.Start
	tampered[0][idx+1] = '9'
	// The tampered page is what gets copied, but the declared source is the
	// original: build the embed against the original and the bytes from the
	// tampered copy.
	b := []byte(`{"resourceType":"Bundle","type":"searchset","total":1,"entry":[{"resource":`)
	at := len(b)
	b = append(b, tampered[0][e.Start:e.End]...)
	b = append(b, "}]}"...)
	_, err := relay.Authored(relay.BuilderSoRSearchset, b, fhirJSON, relay.Embed{Source: relay.NewBody(p1, relay.OriginUpstreamResponse), Start: e.Start, End: e.End, At: at})
	if !errors.Is(err, relay.ErrEmbedMismatch) {
		t.Fatalf("tampered embed: %v", err)
	}
	shifted := []assemblyEntry{{res: EntrySpan{Page: 0, Start: e.Start + 1, End: e.End}, mode: "match"}}
	if _, _, err := assembleSoRSearchset(res.Pages, shifted, 1); err == nil {
		t.Fatal("a span that is not a whole value was accepted")
	}
	for _, bad := range []EntrySpan{{Page: 2, Start: 0, End: 1}, {Page: 0, Start: -1, End: 1}, {Page: 0, Start: 0, End: len(p1) + 1}, {Page: 0, Start: 5, End: 5}} {
		if _, _, err := assembleSoRSearchset(res.Pages, []assemblyEntry{{res: bad, mode: "match"}}, 1); err == nil {
			t.Fatalf("span %+v accepted", bad)
		}
	}
	if _, _, err := assembleSoRSearchset(res.Pages, []assemblyEntry{{res: e, mode: "outcome"}}, 1); err == nil {
		t.Fatal("an entry mode other than match or include was accepted")
	}
}

// assemblyEntriesOf is what classifySearch assembles from res: every match
// entry's resource.
func assemblyEntriesOf(t *testing.T, res SearchResult) []assemblyEntry {
	t.Helper()
	var out []assemblyEntry
	for i, p := range res.Pages {
		parsed, err := ParseSearchPage(p, "Condition")
		if err != nil {
			t.Fatal(err)
		}
		for j, r := range parsed.Resources {
			if parsed.Match[j] {
				r.Page = i
				out = append(out, assemblyEntry{res: r, mode: "match"})
			}
		}
	}
	return out
}

func TestSoRSearch_BundleTotalIgnored(t *testing.T) {
	for _, total := range []string{"0", "1000000", `"many"`, "-1"} {
		p1 := page(total, "n", matchEntry(condition("c1")))
		p2 := page(total, "", matchEntry(condition("c2")))
		got := search(t, searchingSoR{censusSoR: newCensusSoR(), res: resultOf(t, "Condition", p1, p2)})
		if got.Outcome != SearchOK || got.Count != 2 || !strings.Contains(string(got.Value), `"total":2,`) {
			t.Fatalf("total %s: %+v %s", total, got, got.Value)
		}
		one := page(total, "")
		if got := search(t, searchingSoR{censusSoR: newCensusSoR(), res: resultOf(t, "Condition", one)}); got.Outcome != SearchZero || got.Value != nil {
			t.Fatalf("total %s with no entries: %+v", total, got)
		}
	}
}

// Zero is decided on match entries: a page that holds only an
// OperationOutcome (marked as one, or with no search mode) found nothing.
func TestSoRSearch_ZeroCountsMatchesOnly(t *testing.T) {
	outcomeOnly := page("", "", `{"resource":{"resourceType":"OperationOutcome","issue":[]},"search":{"mode":"outcome"}}`)
	untagged := page("", "", `{"resource":{"resourceType":"OperationOutcome","issue":[]}}`)
	twoPages := [][]byte{
		page("", "n", `{"resource":{"resourceType":"OperationOutcome","issue":[]},"search":{"mode":"outcome"}}`),
		page("", "", `{"resource":{"resourceType":"OperationOutcome","issue":[]}}`),
	}
	for name, pages := range map[string][][]byte{"outcome": {outcomeOnly}, "untagged outcome": {untagged}, "two pages": twoPages} {
		t.Run(name, func(t *testing.T) {
			got := search(t, searchingSoR{censusSoR: newCensusSoR(), res: resultOf(t, "Condition", pages...)})
			if got.Outcome != SearchZero || got.Count != 0 || got.Value != nil || got.Payload.Ownership() != 0 {
				t.Fatalf("result = %+v", got)
			}
		})
	}
	// An OperationOutcome explicitly marked as a match is the wrong type.
	wrong := page("", "", `{"resource":{"resourceType":"OperationOutcome","issue":[]},"search":{"mode":"match"}}`)
	if _, err := ParseSearchPage(wrong, "Condition"); err == nil {
		t.Fatal("an OperationOutcome match was accepted")
	}
}

// The assembly declares every entry as an embed and relay.Authored verifies
// it: a byte changed inside a copied entry after copying is refused.
func TestSoRSearch_AssemblyFaultCaught(t *testing.T) {
	p1 := page("", "n", matchEntry(condition("c1")))
	p2 := page("", "", matchEntry(condition("c2")))
	res := resultOf(t, "Condition", p1, p2)
	for _, target := range []string{`"c1"`, `"c2"`} {
		t.Run(target, func(t *testing.T) {
			assembleFaultHook = func(b []byte) {
				i := bytes.Index(b, []byte(target))
				if i < 0 {
					t.Fatalf("assembly lacks %s", target)
				}
				b[i+2] = '9'
			}
			t.Cleanup(func() { assembleFaultHook = nil })
			_, p, err := assembleSoRSearchset(res.Pages, assemblyEntriesOf(t, res), 2)
			if !errors.Is(err, relay.ErrEmbedMismatch) || p.Ownership() != 0 {
				t.Fatalf("fault not caught: %v %v", err, p)
			}
			if got := searchSystemOfRecord(context.Background(), searchingSoR{censusSoR: newCensusSoR(), res: res}, "Condition", "p1"); got.Outcome != SearchMalformed || got.Value != nil {
				t.Fatalf("search with a faulty assembly = %+v", got)
			}
		})
	}
}

func TestSoRSearch_Outcomes(t *testing.T) {
	good := page("", "", matchEntry(condition("c1")))
	big := func(n int) []byte {
		return page("", "", matchEntry(`{"resourceType":"Condition","id":"c","note":[{"text":"`+strings.Repeat("a", n)+`"}]}`))
	}
	manyEntries := func(n int) []byte {
		es := make([]string, n)
		for i := range es {
			es[i] = matchEntry(condition("c"))
		}
		return page("", "", es...)
	}
	chain := func(n int) SearchResult {
		pages := make([][]byte, n)
		for i := range pages {
			next := "n"
			if i == n-1 {
				next = ""
			}
			pages[i] = page("", next, matchEntry(condition("c")))
		}
		return resultOf(t, "Condition", pages...)
	}
	shifted := resultOf(t, "Condition", good)
	shifted.Entries[0].Start++
	undercount := resultOf(t, "Condition", good)
	undercount.Total = 0
	dropped := resultOf(t, "Condition", page("", "", matchEntry(condition("c1")), matchEntry(condition("c2"))))
	dropped.Entries, dropped.Total = dropped.Entries[:1], 1
	cases := map[string]struct {
		sor  SystemOfRecord
		want SearchOutcome
	}{
		"connector cannot search": {nonSearchingSoR{censusSoR: newCensusSoR()}, SearchUnsupported},
		"zero":                    {searchingSoR{censusSoR: newCensusSoR(), res: resultOf(t, "Condition", page("", ""))}, SearchZero},
		"ten pages":               {searchingSoR{censusSoR: newCensusSoR(), res: chain(SoRSearchMaxPages)}, SearchOK},
		"eleven pages":            {searchingSoR{censusSoR: newCensusSoR(), res: chain(SoRSearchMaxPages + 1)}, SearchBound},
		"200 entries":             {searchingSoR{censusSoR: newCensusSoR(), res: resultOf(t, "Condition", manyEntries(SoRSearchMaxEntries))}, SearchOK},
		"201 entries":             {searchingSoR{censusSoR: newCensusSoR(), res: resultOf(t, "Condition", manyEntries(SoRSearchMaxEntries+1))}, SearchBound},
		"over 4 MiB":              {searchingSoR{censusSoR: newCensusSoR(), res: resultOf(t, "Condition", big(SoRSearchMaxBytes))}, SearchBound},
		"connector bound":         {searchingSoR{censusSoR: newCensusSoR(), err: &SearchError{Outcome: SearchBound, Reason: "time bound"}}, SearchBound},
		"connector unavailable":   {searchingSoR{censusSoR: newCensusSoR(), err: &SearchError{Outcome: SearchUnavailable, Reason: "x"}}, SearchUnavailable},
		"connector unsupported":   {searchingSoR{censusSoR: newCensusSoR(), err: &SearchError{Outcome: SearchUnsupported, Reason: "x"}}, SearchUnsupported},
		"unclassified error":      {searchingSoR{censusSoR: newCensusSoR(), err: errors.New("private-upstream-sentinel")}, SearchUnavailable},
		"no pages":                {searchingSoR{censusSoR: newCensusSoR(), res: SearchResult{}}, SearchMalformed},
		"not a searchset":         {searchingSoR{censusSoR: newCensusSoR(), res: SearchResult{Pages: [][]byte{[]byte(`{"resourceType":"Bundle","type":"collection"}`)}}}, SearchMalformed},
		"duplicate keys":          {searchingSoR{censusSoR: newCensusSoR(), res: SearchResult{Pages: [][]byte{[]byte(`{"resourceType":"Bundle","type":"searchset","type":"searchset"}`)}}}, SearchMalformed},
		"shifted span":            {searchingSoR{censusSoR: newCensusSoR(), res: shifted}, SearchMalformed},
		"total disagrees":         {searchingSoR{censusSoR: newCensusSoR(), res: undercount}, SearchMalformed},
		"dropped entry":           {searchingSoR{censusSoR: newCensusSoR(), res: dropped}, SearchMalformed},
		"last page has next":      {searchingSoR{censusSoR: newCensusSoR(), res: resultOf(t, "Condition", page("", "n", matchEntry(condition("c1"))))}, SearchMalformed},
		"middle page lacks next":  {searchingSoR{censusSoR: newCensusSoR(), res: resultOf(t, "Condition", good, good)}, SearchMalformed},
		"other resource type":     {searchingSoR{censusSoR: newCensusSoR(), res: SearchResult{Pages: [][]byte{page("", "", matchEntry(`{"resourceType":"Observation","id":"o"}`))}, Entries: []EntrySpan{{}}, Total: 1}}, SearchMalformed},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := search(t, tc.sor)
			if got.Outcome != tc.want {
				t.Fatalf("outcome = %q (%s), want %q", got.Outcome, got.Reason, tc.want)
			}
			if strings.Contains(got.Reason, "sentinel") {
				t.Fatalf("reason leaks the upstream error: %q", got.Reason)
			}
			if tc.want != SearchOK && (got.Value != nil || got.Payload.Ownership() != 0) {
				t.Fatalf("a %s search carries a value", got.Outcome)
			}
			if tc.want == SearchOK && got.Payload.Ownership() == 0 {
				t.Fatal("an ok search has no payload")
			}
		})
	}
}

func TestSoRSearch_InvalidInputNeverSearched(t *testing.T) {
	calls := 0
	sor := searchingSoR{censusSoR: newCensusSoR(), res: resultOf(t, "Condition", page("", "")), calls: &calls}
	for _, in := range [][2]string{{"Condition", "p1&_include=*"}, {"Condition", ""}, {"condition", "p1"}, {"Condition?x=1", "p1"}, {"Condition", strings.Repeat("a", 65)}} {
		if got := searchSystemOfRecord(context.Background(), sor, in[0], in[1]); got.Outcome != SearchMalformed {
			t.Fatalf("%q: %+v", in, got)
		}
	}
	if calls != 0 {
		t.Fatalf("connector called %d times", calls)
	}
}

func TestSoRSearch_ThroughObserver(t *testing.T) {
	p := page("", "", matchEntry(condition("c1")))
	for name, tc := range map[string]struct {
		inner  SystemOfRecord
		want   SearchOutcome
		detail string
	}{
		"searches":   {searchingSoR{censusSoR: newCensusSoR(), res: resultOf(t, "Condition", p)}, SearchOK, "found 1 entries in 1 pages"},
		"no search":  {nonSearchingSoR{censusSoR: newCensusSoR()}, SearchUnsupported, "unsupported"},
		"classified": {searchingSoR{censusSoR: newCensusSoR(), err: &SearchError{Outcome: SearchBound, Reason: "page bound"}}, SearchBound, "bound"},
		"other":      {searchingSoR{censusSoR: newCensusSoR(), err: errors.New("private-upstream-sentinel")}, SearchUnavailable, "unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			var events []ObserverEvent
			o := observingSoR{inner: tc.inner, clock: func() time.Time { return time.Unix(1700000000, 0) }, observer: func(e ObserverEvent) { events = append(events, e) }}
			got := searchSystemOfRecord(context.Background(), o, "Condition", "p1")
			if got.Outcome != tc.want {
				t.Fatalf("outcome = %q", got.Outcome)
			}
			if len(events) != 1 || events[0].Op != "SearchPatient" || events[0].Detail != tc.detail || len(events[0].Payload) != 0 {
				t.Fatalf("events = %+v", events)
			}
		})
	}
}

// TestSoRSearchQuery_CoverageIncludesPayor: a Coverage search includes each
// Coverage's payor Organization (a payer resolves the payor from the request
// alone), a device search each order's performer (the supplier a dispatched
// order names), and the advertised templates say so; other searches carry no
// include.
func TestSoRSearchQuery_CoverageIncludesPayor(t *testing.T) {
	q, err := SoRSearchQuery("Coverage", "p1")
	if err != nil || q != "Coverage?patient=Patient%2Fp1&_include=Coverage%3Apayor" {
		t.Fatalf("coverage query %q %v", q, err)
	}
	if q, err := SoRSearchQuery("DeviceRequest", "p1"); err != nil || q != "DeviceRequest?patient=Patient%2Fp1&_include=DeviceRequest%3Aperformer" {
		t.Fatalf("device query %q %v", q, err)
	}
	for _, rt := range []string{"ServiceRequest", "MedicationRequest", "QuestionnaireResponse", "DiagnosticReport", "DocumentReference"} {
		if q, _ := SoRSearchQuery(rt, "p1"); strings.Contains(q, "_include") {
			t.Fatalf("%s query %q", rt, q)
		}
	}
	if got := prefetchTemplate("coverage"); got != "Coverage?patient=Patient/{{context.patientId}}&_include=Coverage:payor" {
		t.Fatalf("coverage template %q", got)
	}
	if got := prefetchTemplate("deviceHistory"); got != "DeviceRequest?patient=Patient/{{context.patientId}}&_include=DeviceRequest:performer" {
		t.Fatalf("device template %q", got)
	}
}
