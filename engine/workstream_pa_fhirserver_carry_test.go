package engine

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The coverage read through the request's own fhirServer is carried (E-07):
// these rows pin what is carried, when, and what never is.

// carriedSearchset is a carried prefetch.coverage, read from the bytes the
// payer's side received: its top-level members, total, each entry's fullUrl
// and mode, and the exact bytes of each match and include resource.
type carriedSearchset struct {
	members           []string
	total             int
	fullURLs, modes   []string
	matches, includes []string
	// entryMembers holds each entry's member names.
	entryMembers [][]string
}

func carriedSearchsetOf(t *testing.T, sent []byte) carriedSearchset {
	t.Helper()
	v, ok := valueOf(t, sent, "prefetch", "coverage")
	if !ok {
		t.Fatalf("no prefetch.coverage carried: %s", sent)
	}
	b := []byte(v)
	doc, err := relay.Doc(relay.NewBody(b, relay.OriginIngressRequest))
	if err != nil {
		t.Fatalf("carried coverage does not scan: %v", err)
	}
	var out carriedSearchset
	out.members = names(membersOf(t, b))
	if rt, _ := valueOf(t, b, "resourceType"); rt != `"Bundle"` {
		t.Fatalf("carried coverage is %s", rt)
	}
	if typ, _ := valueOf(t, b, "type"); typ != `"searchset"` {
		t.Fatalf("carried coverage type %s", typ)
	}
	total, _ := valueOf(t, b, "total")
	if out.total, err = strconv.Atoi(total); err != nil {
		t.Fatalf("total %q", total)
	}
	list, _ := doc.Member(doc.Root(), "entry")
	for _, e := range doc.Elems(list) {
		var em []string
		for _, m := range doc.Members(e) {
			em = append(em, m.Name)
		}
		out.entryMembers = append(out.entryMembers, em)
		fu, _ := doc.Member(e, "fullUrl")
		s, _ := doc.StringValue(fu)
		out.fullURLs = append(out.fullURLs, s)
		search, _ := doc.Member(e, "search")
		mode, _ := doc.Member(search, "mode")
		m, _ := doc.StringValue(mode)
		out.modes = append(out.modes, m)
		res, _ := doc.Member(e, "resource")
		rs, re := doc.Span(res)
		switch m {
		case "match":
			out.matches = append(out.matches, string(b[rs:re]))
		case "include":
			out.includes = append(out.includes, string(b[rs:re]))
		default:
			t.Fatalf("entry mode %q carried", m)
		}
	}
	wantOnlyAbsolutePayorIncludesAddressed(t, out)
	return out
}

// wantOnlyAbsolutePayorIncludesAddressed holds for every carried coverage:
// each entry's fullUrl is a urn:uuid, except an included Organization's,
// which may instead be an absolute payor reference a carried Coverage itself
// writes, naming that Organization (the one exception to the urn:uuid rule).
// No other entry ever carries an address.
func wantOnlyAbsolutePayorIncludesAddressed(t *testing.T, got carriedSearchset) {
	t.Helper()
	var refs []string
	for _, c := range got.matches {
		refs = append(refs, payorReferences([]byte(c))...)
	}
	inc := 0
	for i, u := range got.fullURLs {
		if strings.HasPrefix(u, "urn:uuid:") {
			if got.modes[i] == "include" {
				inc++
			}
			continue
		}
		if got.modes[i] != "include" {
			t.Fatalf("a %s entry carries the address %q", got.modes[i], u)
		}
		_, id := entryHead(json.RawMessage(got.includes[inc]))
		inc++
		if !slices.Contains(refs, u) || !strings.HasSuffix(u, "/Organization/"+id) || !strings.Contains(u, "://") {
			t.Fatalf("an include carries %q, which no carried Coverage names it by", u)
		}
	}
}

// serverResource is the exact bytes of the resource of the answer's entry
// whose resource is typ/id.
func serverResource(t *testing.T, answer, typ, id string) string {
	t.Helper()
	b := []byte(answer)
	doc, err := relay.Doc(relay.NewBody(b, relay.OriginUpstreamResponse))
	if err != nil {
		t.Fatal(err)
	}
	list, _ := doc.Member(doc.Root(), "entry")
	for _, e := range doc.Elems(list) {
		res, _ := doc.Member(e, "resource")
		rt, _ := stringMember(doc, res, "resourceType")
		rid, _ := stringMember(doc, res, "id")
		if rt == typ && rid == id {
			s, end := doc.Span(res)
			return answer[s:end]
		}
	}
	t.Fatalf("no %s/%s in the answer", typ, id)
	return ""
}

// serverAnswer is the EHR server's own searchset, as a FHIR server writes
// one: a Bundle id and meta, links on its base, an entry address on its base
// for every record, the member's active Coverage (in the server's own layout,
// with a decimal and an escape a re-encoder would change), its payor
// Organization included, and a message about the search.
func serverAnswer(base string) string {
	return `{"resourceType":"Bundle","id":"srv-bundle-1","meta":{"lastUpdated":"2026-09-30T10:00:00Z"},"type":"searchset","total":1,` +
		`"link":[{"relation":"self","url":"` + base + `/Coverage?patient=` + strangerMember + `"},{"relation":"first","url":"` + base + `/Coverage?patient=` + strangerMember + `&_page=1"}],"entry":[` +
		`{"fullUrl":"` + base + `/Coverage/c9","resource":{ "resourceType" : "Coverage", "id" : "c9", "status" : "active", ` +
		`"beneficiary" : { "reference" : "Patient/` + strangerMember + `" }, "payor" : [ { "reference" : "Organization/o1" } ], ` +
		`"costToBeneficiary" : [ { "valueMoney" : { "value" : 20.50, "currency" : "USD" } } ], "class" : [ { "name" : "Gold é" } ] },"search":{"mode":"match"}},` +
		`{"fullUrl":"` + base + `/Organization/o1","resource":{"resourceType":"Organization","id":"o1",` +
		`"identifier":[{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}],"name":"Payer One"},"search":{"mode":"include"}},` +
		`{"resource":{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"informational","diagnostics":"searched ` + base + `"}]},"search":{"mode":"outcome"}}]}`
}

// wantGatewayWritten asserts the carried searchset is the gateway's own: its
// members are exactly resourceType, type, total and entry; each entry is
// fullUrl, resource and search, under a urn:uuid; total is the match count;
// and nothing in it, or in the whole request, names the server.
func wantGatewayWritten(t *testing.T, sent []byte, got carriedSearchset, server string) {
	t.Helper()
	if !slices.Equal(got.members, []string{"resourceType", "type", "total", "entry"}) {
		t.Fatalf("carried searchset members %v: no link, id or meta of the server's", got.members)
	}
	if got.total != len(got.matches) {
		t.Fatalf("total %d, %d matches", got.total, len(got.matches))
	}
	for i, u := range got.fullURLs {
		if !strings.HasPrefix(u, "urn:uuid:") {
			t.Fatalf("entry %d fullUrl %q is not a urn:uuid", i, u)
		}
		if !slices.Equal(got.entryMembers[i], []string{"fullUrl", "resource", "search"}) {
			t.Fatalf("entry %d members %v", i, got.entryMembers[i])
		}
	}
	for _, key := range []string{server, "example.com", "fhirServer", "fhirAuthorization", "ehr-secret-token", "srv-bundle-1", "lastUpdated", `"link"`, "OperationOutcome"} {
		if bytes.Contains(sent, []byte(key)) {
			t.Fatalf("the carried request holds %q", key)
		}
	}
}

// wantOnlyTheDeclaredEdits asserts the carried request is the EHR's less
// fhirServer and fhirAuthorization (E-01), plus prefetch.coverage (E-07): with
// the carried coverage removed it is byte for byte the EHR's request with
// only the callback stripped.
func wantOnlyTheDeclaredEdits(t *testing.T, ehr, sent []byte) {
	t.Helper()
	ehrBody := relay.NewBody(ehr, relay.OriginIngressRequest)
	ed, err := relay.Doc(ehrBody)
	if err != nil {
		t.Fatal(err)
	}
	stripped, err := relay.Apply(ehrBody, "application/json", relay.EditCDSCallbackStrip,
		ed.RemoveMember(ed.Root(), "fhirServer"), ed.RemoveMember(ed.Root(), "fhirAuthorization"))
	if err != nil {
		t.Fatal(err)
	}
	sentBody := relay.NewBody(sent, relay.OriginIngressRequest)
	sd, err := relay.Doc(sentBody)
	if err != nil {
		t.Fatal(err)
	}
	pf, ok := sd.Member(sd.Root(), "prefetch")
	if !ok {
		t.Fatal("no prefetch carried")
	}
	without, err := relay.Apply(sentBody, "application/json", relay.EditCDSCoverageCarry, sd.RemoveMember(pf, "coverage"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := relay.BytesForTest(without), relay.BytesForTest(stripped); !bytes.Equal(got, want) {
		t.Fatalf("beyond E-01 and E-07 the request changed:\n got %s\nwant %s", got, want)
	}
}

// At every level, the coverage read through fhirServer is carried as a
// searchset the gateway writes: the chosen Coverage and the payor
// Organization the server included, each byte for byte as the server
// returned it, under urn:uuid addresses, with none of the server's links,
// entry addresses, Bundle id or meta, and none of its messages. The
// Organization the search included resolves the payor, so it is not read
// again. Everything else in the request is the EHR's, byte for byte.
func TestCRDIngressCarriesTheCoverageReadThroughFHIRServer(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			e := newEHRServer(t, nil)
			answer := serverAnswer(e.base)
			e.handler = ehrAnswer(200, "application/fhir+json", answer)
			env, _ := fhirServerEnv(t, e, "")
			env.originator.cfg.ConformanceEnforcement = level
			body := fhirServerRequest(e.base)
			rec := httptest.NewRecorder()
			env.originator.handleCRDIngress(rec, crdIngressPost(body))
			if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
				t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
			}
			if e.calls.Load() != 1 {
				t.Fatalf("fhirServer read %d times, want the search alone", e.calls.Load())
			}
			sent := sentRequest(t, env)
			got := carriedSearchsetOf(t, sent)
			wantGatewayWritten(t, sent, got, e.base)
			if !slices.Equal(got.modes, []string{"match", "include"}) ||
				got.matches[0] != serverResource(t, answer, "Coverage", "c9") ||
				got.includes[0] != serverResource(t, answer, "Organization", "o1") || got.total != 1 {
				t.Fatalf("carried %+v", got)
			}
			wantOnlyTheDeclaredEdits(t, body, sent)
		})
	}
}

// A Coverage naming its payor by reference alone: the Organization routing
// read is carried as an include entry, byte for byte as the server answered
// the read, at every level.
func TestCRDIngressCarriesThePayorOrganizationItRead(t *testing.T) {
	org := `{ "resourceType" : "Organization", "id" : "o1", "identifier" : [ { "system" : "urn:oid:2.16.840.1.113883.6.300", "value" : "00001" } ], "name" : "Payer é" }`
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			e := newEHRServer(t, nil)
			search := bareRefSearchset("o1")
			e.handler = ehrRoutes(search, ehrAnswer(200, "application/fhir+json", "\n"+org+"\n"))
			env, _ := fhirServerEnv(t, e, "")
			env.originator.cfg.ConformanceEnforcement = level
			body := fhirServerRequest(e.base)
			rec := httptest.NewRecorder()
			env.originator.handleCRDIngress(rec, crdIngressPost(body))
			if rec.Code != http.StatusOK || env.routeHitCount() != 1 || e.calls.Load() != 2 {
				t.Fatalf("answer %d %s; %d reads", rec.Code, rec.Body.String(), e.calls.Load())
			}
			sent := sentRequest(t, env)
			got := carriedSearchsetOf(t, sent)
			wantGatewayWritten(t, sent, got, e.base)
			if !slices.Equal(got.modes, []string{"match", "include"}) || got.matches[0] != serverResource(t, search, "Coverage", "c0") ||
				got.includes[0] != org || got.total != 1 {
				t.Fatalf("carried %+v", got)
			}
			wantOnlyTheDeclaredEdits(t, body, sent)
		})
	}
}

// The server's own links and entry addresses are on the very fhirServer the
// request named: none of them is carried, and the Coverages and Organization
// still are.
func TestCRDIngressCarriesNoAddressOnTheFHIRServer(t *testing.T) {
	e := newEHRServer(t, nil)
	// Every address in the answer is on the request's own fhirServer base.
	answer := serverAnswer(e.base)
	if strings.Count(answer, e.base) < 5 {
		t.Fatalf("the answer names its base %d times", strings.Count(answer, e.base))
	}
	e.handler = ehrAnswer(200, "application/fhir+json", answer)
	env, _ := fhirServerEnv(t, e, FHIRServerReadPrivate)
	rec := httptest.NewRecorder()
	env.originator.handleCRDIngress(rec, crdIngressPost(fhirServerRequest(e.base)))
	if rec.Code != http.StatusOK {
		t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
	}
	sent := sentRequest(t, env)
	got := carriedSearchsetOf(t, sent)
	for _, u := range got.fullURLs {
		if strings.Contains(u, e.base) || strings.Contains(u, "example.com") {
			t.Fatalf("fullUrl %q names the server", u)
		}
	}
	if bytes.Contains(sent, []byte(e.base)) || bytes.Contains(sent, []byte("/Coverage?patient=")) {
		t.Fatalf("the carried request names the server: %s", sent)
	}
	if len(got.matches) != 1 || len(got.includes) != 1 {
		t.Fatalf("carried %+v", got)
	}
}

// Only what routing used is carried: the active Coverage, never a cancelled
// one beside it, and never an Organization only the cancelled one names,
// whether the server included it or routing read the chosen one's payor.
func TestCRDIngressCarriesOnlyTheRoutingChoice(t *testing.T) {
	cancelled := `{"fullUrl":"https://example.com:8443/fhir/Coverage/old","resource":{"resourceType":"Coverage","id":"old","status":"cancelled","beneficiary":{"reference":"Patient/` + strangerMember + `"},"payor":[{"reference":"Organization/o2"}]},"search":{"mode":"match"}}`
	active := `{"fullUrl":"https://example.com:8443/fhir/Coverage/c1","resource":{"resourceType":"Coverage","id":"c1","status":"active","beneficiary":{"reference":"Patient/` + strangerMember + `"},"payor":[{"reference":"Organization/o1"}]},"search":{"mode":"match"}}`
	o1 := `{"fullUrl":"https://example.com:8443/fhir/Organization/o1","resource":` + payorOrganization("o1", "00001") + `,"search":{"mode":"include"}}`
	o2 := `{"fullUrl":"https://example.com:8443/fhir/Organization/o2","resource":` + payorOrganization("o2", "99999") + `,"search":{"mode":"include"}}`
	outcome := `{"resource":{"resourceType":"OperationOutcome","issue":[{"severity":"warning","code":"processing"}]},"search":{"mode":"outcome"}}`
	searchset := func(entries ...string) string {
		return `{"resourceType":"Bundle","type":"searchset","entry":[` + strings.Join(entries, ",") + `]}`
	}
	for name, row := range map[string]struct {
		search   string
		reads    int32
		includes []string
	}{
		"both Organizations included":                         {searchset(cancelled, o2, active, o1, outcome), 1, []string{payorOrganization("o1", "00001")}},
		"the cancelled one's included, the active one's read": {searchset(cancelled, o2, active, outcome), 2, []string{payorOrganization("o1", "00001")}},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEHRServer(t, nil)
			e.handler = ehrRoutes(row.search, ehrAnswer(200, "application/fhir+json", payorOrganization("o1", "00001")))
			env, _ := fhirServerEnv(t, e, FHIRServerReadPrivate)
			rec := httptest.NewRecorder()
			env.originator.handleCRDIngress(rec, crdIngressPost(fhirServerRequest(e.base)))
			if rec.Code != http.StatusOK || e.calls.Load() != row.reads {
				t.Fatalf("answer %d %s; %d reads, want %d", rec.Code, rec.Body.String(), e.calls.Load(), row.reads)
			}
			sent := sentRequest(t, env)
			got := carriedSearchsetOf(t, sent)
			wantGatewayWritten(t, sent, got, e.base)
			if len(got.matches) != 1 || got.matches[0] != serverResource(t, row.search, "Coverage", "c1") || !slices.Equal(got.includes, row.includes) {
				t.Fatalf("carried %+v", got)
			}
			for _, never := range []string{"cancelled", "99999", `"o2"`} {
				if bytes.Contains(sent, []byte(never)) {
					t.Fatalf("the carried request holds %q, which only the Coverage not chosen names", never)
				}
			}
		})
	}
}

// Nothing is read or filled when the read is off or no fhirServer is named:
// the request is refused as before, at every level, and nothing crosses the
// network.
func TestCRDIngressCarriesNothingWithoutTheRead(t *testing.T) {
	for _, level := range allLevels {
		for name, row := range map[string]struct {
			mode     string
			noServer bool
			msg      string
		}{
			"the read off":  {FHIRServerReadOff, false, crdNoCoverageReadOff},
			"no fhirServer": {FHIRServerReadPrivate, true, crdNoCoverageToRouteBy},
		} {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				e := newEHRServer(t, ehrAnswer(200, "application/fhir+json", serverAnswer("https://example.com:8443/fhir")))
				env, _ := fhirServerEnv(t, e, row.mode)
				env.originator.cfg.ConformanceEnforcement = level
				body := fhirServerRequest(e.base)
				if row.noServer {
					body = bytes.Replace(body, []byte(`"fhirServer" : "`+e.base+`",`), nil, 1)
				}
				rec := httptest.NewRecorder()
				env.originator.handleCRDIngress(rec, crdIngressPost(body))
				refusedBeforeTheNetwork(t, env, rec, http.StatusPreconditionFailed, row.msg)
				if e.calls.Load() != 0 {
					t.Fatalf("fhirServer read %d times", e.calls.Load())
				}
			})
		}
	}
}

// A coverage key the EHR sent is never replaced or filled, at every level:
// a Coverage is carried exactly as sent and nothing is read; a null coverage
// is not filled (the request is refused as it always was, and nothing is read
// or sent).
func TestCRDIngressNeverReplacesTheEHRsCoverage(t *testing.T) {
	sentCoverage := `{"resourceType":"Coverage","id":"ehr-c","beneficiary":{"reference":"Patient/` + strangerMember + `"},"payor":[{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}}]}`
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			e := newEHRServer(t, ehrAnswer(200, "application/fhir+json", serverAnswer("https://example.com:8443/fhir")))
			env, _ := fhirServerEnv(t, e, FHIRServerReadPrivate)
			env.originator.cfg.ConformanceEnforcement = level
			body := bytes.Replace(fhirServerRequest(e.base), []byte(`"id":"`+strangerMember+`"}`),
				[]byte(`"id":"`+strangerMember+`"},"coverage":`+sentCoverage), 1)
			rec := httptest.NewRecorder()
			env.originator.handleCRDIngress(rec, crdIngressPost(body))
			if rec.Code != http.StatusOK || e.calls.Load() != 0 {
				t.Fatalf("answer %d %s; %d reads", rec.Code, rec.Body.String(), e.calls.Load())
			}
			if got, _ := valueOf(t, sentRequest(t, env), "prefetch", "coverage"); got != sentCoverage {
				t.Fatalf("carried coverage %s, want the EHR's %s", got, sentCoverage)
			}

			e = newEHRServer(t, ehrAnswer(200, "application/fhir+json", serverAnswer("https://example.com:8443/fhir")))
			env, _ = fhirServerEnv(t, e, FHIRServerReadPrivate)
			env.originator.cfg.ConformanceEnforcement = level
			body = bytes.Replace(fhirServerRequest(e.base), []byte(`"id":"`+strangerMember+`"}`),
				[]byte(`"id":"`+strangerMember+`"},"coverage":null`), 1)
			rec = httptest.NewRecorder()
			env.originator.handleCRDIngress(rec, crdIngressPost(body))
			refusedBeforeTheNetwork(t, env, rec, http.StatusPreconditionFailed, "no coverage in request or system of record")
			if e.calls.Load() != 0 {
				t.Fatalf("a null coverage was read for: %d reads", e.calls.Load())
			}
		})
	}
}

// Another patient's Coverage is never filled: a searchset holding one beside
// the member's own is refused (502) at every level, and nothing is sent.
func TestCRDIngressNeverCarriesAnotherPatientsCoverage(t *testing.T) {
	other := strings.Replace(bareRefSearchset("o1"), `"id":"c0"`, `"id":"c-other"`, 1)
	other = strings.Replace(other, "Patient/"+strangerMember, "Patient/someone-else", 1)
	mixed := strings.TrimSuffix(coverageSearchset(strangerMember, "00001"), "]}") + "," +
		strings.TrimSuffix(strings.TrimPrefix(other, `{"resourceType":"Bundle","type":"searchset","entry":[`), "]}") + "]}"
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			e := newEHRServer(t, ehrRoutes(mixed, ehrAnswer(200, "application/fhir+json", payorOrganization("o1", "00001"))))
			env, _ := fhirServerEnv(t, e, FHIRServerReadPrivate)
			env.originator.cfg.ConformanceEnforcement = level
			rec := httptest.NewRecorder()
			env.originator.handleCRDIngress(rec, crdIngressPost(fhirServerRequest(e.base)))
			refusedBeforeTheNetwork(t, env, rec, http.StatusBadGateway, fhirServerOtherPatient)
		})
	}
}

// The fill fence (fhirServerCoverageValue): what E-07 writes is fenced as
// written, so a record that is not the bound patient's is never carried, even
// one the routing read did not refuse.
func TestFHIRServerCoverageValueFillFence(t *testing.T) {
	fence := newPatientFence(shnsdk.MemberSystem, strangerMember, nil, "", strangerMember).forPrefetch()
	own := coverageSearchset(strangerMember, "00001")
	if v, status, msg := fhirServerCoverageValue([]byte(own), nil, "https://example.com:8443/fhir", fence); status != 0 || v == nil {
		t.Fatalf("the member's own coverage: %d %s", status, msg)
	}
	other := coverageSearchset("someone-else", "00001")
	if v, status, msg := fhirServerCoverageValue([]byte(other), nil, "https://example.com:8443/fhir", fence); status != http.StatusBadGateway || msg != fillFencedOtherPatient || v != nil {
		t.Fatalf("another patient's coverage: %d %q %s", status, msg, v)
	}
	// Another patient's Coverage the routing choice did not pick is not
	// carried, so it is not what the fence refuses; one it picks is.
	mixed := strings.Replace(statusSearchset("cancelled:00001", "active:00001"), "Patient/"+strangerMember, "Patient/someone-else", 1)
	if v, status, msg := fhirServerCoverageValue([]byte(mixed), nil, "https://example.com:8443/fhir", fence); status != 0 || bytes.Contains(v, []byte("someone-else")) {
		t.Fatalf("an unchosen Coverage: %d %q %s", status, msg, v)
	}
	mixed = strings.Replace(statusSearchset("cancelled:00001", "active:00001"), "Patient/"+strangerMember, "Patient/someone-else", 2)
	mixed = strings.Replace(mixed, "Patient/someone-else", "Patient/"+strangerMember, 1)
	if v, status, msg := fhirServerCoverageValue([]byte(mixed), nil, "https://example.com:8443/fhir", fence); status != http.StatusBadGateway || msg != fillFencedOtherPatient || v != nil {
		t.Fatalf("another patient's chosen Coverage: %d %q %s", status, msg, v)
	}
}

// fhirServerCarriedEntries picks the records by routing's own rule: the
// chosen Coverages (match), the Organization entries a chosen Coverage's
// payor names by fullUrl or Organization/<id> (include), the Organization
// routing read (include), and nothing else.
func TestFHIRServerCarriedEntries(t *testing.T) {
	entry := func(fullURL, res string) string {
		if fullURL == "" {
			return `{"resource":` + res + `}`
		}
		return `{"fullUrl":"` + fullURL + `","resource":` + res + `}`
	}
	cov := func(id, status, ref string) string {
		return `{"resourceType":"Coverage","id":"` + id + `","status":"` + status + `","beneficiary":{"reference":"Patient/m"},"payor":[{"reference":"` + ref + `"}]}`
	}
	org := func(id string) string { return `{"resourceType":"Organization","id":"` + id + `"}` }
	bundle := func(entries ...string) []byte {
		return []byte(`{"resourceType":"Bundle","type":"searchset","entry":[` + strings.Join(entries, ",") + `]}`)
	}
	for name, row := range map[string]struct {
		answer []byte
		payor  []byte
		want   []string // "<mode> <resource>"
		total  int
	}{
		"a relative payor answered by type and id": {bundle(entry("", cov("c1", "active", "Organization/o1")), entry("https://s/Organization/o1", org("o1"))), nil,
			[]string{"match " + cov("c1", "active", "Organization/o1"), "include " + org("o1")}, 1},
		"an absolute payor answered by fullUrl": {bundle(entry("", cov("c1", "active", "https://s/Organization/x")), entry("https://s/Organization/x", org("o9"))), nil,
			[]string{"match " + cov("c1", "active", "https://s/Organization/x"), "include " + org("o9")}, 1},
		// A Coverage not chosen is not among what routing resolves against: a
		// cancelled Coverage whose fullUrl is the active one's absolute payor
		// reference does not answer it, so routing read o1, and o1 is carried.
		"a cancelled Coverage's fullUrl never answers the payor": {bundle(entry("https://ehr.example/fhir/Organization/o1", cov("c0", "cancelled", "Organization/o2")),
			entry("", cov("c1", "active", "https://ehr.example/fhir/Organization/o1"))), []byte(org("o1")),
			[]string{"match " + cov("c1", "active", "https://ehr.example/fhir/Organization/o1"), "include " + org("o1")}, 1},
		"an Organization no chosen Coverage names": {bundle(entry("", cov("c1", "active", "Organization/o1")), entry("", org("o2"))), nil,
			[]string{"match " + cov("c1", "active", "Organization/o1")}, 1},
		"only the stale Coverage's Organization": {bundle(entry("", cov("c0", "cancelled", "Organization/o2")), entry("", org("o2")), entry("", cov("c1", "active", "Organization/o1"))), []byte(org("o1")),
			[]string{"match " + cov("c1", "active", "Organization/o1"), "include " + org("o1")}, 1},
		"all cancelled: every Coverage and their payors": {bundle(entry("", cov("c0", "cancelled", "Organization/o1")), entry("", cov("c1", "cancelled", "Organization/o1")), entry("", org("o1"))), nil,
			[]string{"match " + cov("c0", "cancelled", "Organization/o1"), "match " + cov("c1", "cancelled", "Organization/o1"), "include " + org("o1")}, 2},
		"a message and another resource are never carried": {bundle(entry("", cov("c1", "active", "Organization/o1")),
			entry("", `{"resourceType":"OperationOutcome","id":"o1"}`), entry("", `{"resourceType":"Patient","id":"o1"}`)), []byte(org("o1")),
			[]string{"match " + cov("c1", "active", "Organization/o1"), "include " + org("o1")}, 1},
	} {
		t.Run(name, func(t *testing.T) {
			entries, total, ok := fhirServerCarriedEntries(row.answer, row.payor, "https://ehr.example/fhir")
			if !ok {
				t.Fatal("not read")
			}
			pages := [][]byte{row.answer, row.payor}
			var got []string
			for _, e := range entries {
				got = append(got, e.mode+" "+string(pages[e.res.Page][e.res.Start:e.res.End]))
			}
			if !slices.Equal(got, row.want) || total != row.total {
				t.Fatalf("carried %v (total %d), want %v (total %d)", got, total, row.want, row.total)
			}
		})
	}
	if _, _, ok := fhirServerCarriedEntries([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"Organization","id":"o1"}}]}`), nil, ""); ok {
		t.Fatal("an answer with no Coverage was carried")
	}
}

// On the payer's side, a carried coverage is a coverage like any other: a
// payer that maps its network identity to its own system's (E-03) maps the
// identifier the carried searchset holds, in the payor Organization the
// provider's gateway read or in the Coverage itself, and changes nothing
// else; its own system receives the provider's carried request otherwise
// byte for byte.
func TestCarriedCoverageMappedByThePayersIdentityMapping(t *testing.T) {
	for name, row := range map[string]struct {
		handler func(http.ResponseWriter, *http.Request)
		paths   []string
	}{
		"by the payor Organization read": {ehrRoutes(bareRefSearchset("o1"), ehrAnswer(200, "application/fhir+json", payorOrganization("o1", "00001"))),
			[]string{"prefetch.coverage.entry.1.resource.identifier.0.system", "prefetch.coverage.entry.1.resource.identifier.0.value"}},
		"by the Coverage's own identifier": {ehrAnswer(200, "application/fhir+json", statusSearchset("active:00001")),
			[]string{"prefetch.coverage.entry.0.resource.payor.0.identifier.system", "prefetch.coverage.entry.0.resource.payor.0.identifier.value"}},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEHRServer(t, row.handler)
			env, _ := fhirServerEnv(t, e, FHIRServerReadPrivate)
			rec := httptest.NewRecorder()
			// An order-sign request: the hook the payer's stub system offers.
			body := bytes.Replace(fhirServerRequest(e.base), []byte(`"hook" : "order-select"`), []byte(`"hook" : "order-sign"`), 1)
			env.originator.handleCRDIngress(rec, crdIngressPost(body))
			if rec.Code != http.StatusOK {
				t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
			}
			carried := sentRequest(t, env)
			n, p := crdResponder(t, WithPayorEdgeIdentity(shnsdk.CMSPayerIdentity, payorMapped))
			if res := handleCRD(t, n, carried); res.Status != 0 {
				t.Fatalf("status %d: %s", res.Status, res.Message)
			}
			assertSent(t, p, expectEdits(t, carried,
				tokenEdit{row.paths[0], payorMapped.System}, tokenEdit{row.paths[1], payorMapped.Value}))
		})
	}
}

// The fhirServer test seam reads only its one server, over verified TLS, and
// never outside a test binary.
func TestWithFHIRServerTrustForTest(t *testing.T) {
	e := newEHRServer(t, ehrAnswer(200, "application/fhir+json", coverageSearchset(strangerMember, "00001")))
	r := e.reader(FHIRServerReadPrivate)
	env := newInProcessExchange(t)
	env.originator.cfg.SoR = newPrefetchSoR().sor()
	WithFHIRServerTrustForTest(&env.originator.cfg, "example.com", r.roots, r.dial)
	rec := httptest.NewRecorder()
	env.originator.handleCRDIngress(rec, crdIngressPost(fhirServerRequest(e.base)))
	if rec.Code != http.StatusOK || e.calls.Load() != 1 {
		t.Fatalf("answer %d %s; %d reads", rec.Code, rec.Body.String(), e.calls.Load())
	}
	// Another name does not resolve: nothing is read.
	env = newInProcessExchange(t)
	env.originator.cfg.SoR = newPrefetchSoR().sor()
	WithFHIRServerTrustForTest(&env.originator.cfg, "example.com", r.roots, r.dial)
	rec = httptest.NewRecorder()
	env.originator.handleCRDIngress(rec, crdIngressPost(fhirServerRequest("https://other.example:8443/fhir")))
	refusedBeforeTheNetwork(t, env, rec, http.StatusPreconditionFailed, fhirServerNoAddress)
	// The public mode refuses the private address the seam resolves to.
	env.originator.cfg.FHIRServerRead = FHIRServerReadPublic
	rec = httptest.NewRecorder()
	env.originator.handleCRDIngress(rec, crdIngressPost(fhirServerRequest(e.publicBase)))
	refusedBeforeTheNetwork(t, env, rec, http.StatusPreconditionFailed, fhirServerNotPublic)
	// TLS is verified, against the roots given alone: roots that do not hold
	// the server's certificate refuse it, and the server answers nothing.
	env = newInProcessExchange(t)
	env.originator.cfg.SoR = newPrefetchSoR().sor()
	WithFHIRServerTrustForTest(&env.originator.cfg, "example.com", x509.NewCertPool(), r.dial)
	calls := e.calls.Load()
	rec = httptest.NewRecorder()
	env.originator.handleCRDIngress(rec, crdIngressPost(fhirServerRequest(e.base)))
	refusedBeforeTheNetwork(t, env, rec, http.StatusPreconditionFailed, fhirServerTLSFailed)
	if e.calls.Load() != calls {
		t.Fatal("a server the roots do not trust answered a read")
	}

	old := fhirServerTestBinary
	fhirServerTestBinary = func() bool { return false }
	defer func() { fhirServerTestBinary = old }()
	defer func() {
		if recover() == nil {
			t.Fatal("WithFHIRServerTrustForTest did not panic outside a test binary")
		}
	}()
	var cfg Config
	WithFHIRServerTrustForTest(&cfg, "example.com", r.roots, r.dial)
}

// One exception to the urn:uuid entry addresses: an included Organization a
// carried Coverage names by an absolute reference on the request's own
// fhirServer base has that reference, exactly as the Coverage writes it, as
// its fullUrl, so the reference resolves in the searchset as FHIR resolves
// one. The base is compared normalised (host case, default port, trailing
// slash); a reference on another base, and a relative one, keep a urn:uuid.
// Every other entry is a urn:uuid, and nothing else names the server.
func TestCRDIngressCarriesAnAbsolutePayorReferenceOnTheFHIRServer(t *testing.T) {
	const base = "https://example.com:8443/fhir"
	org := payorOrganization("o1", "00001")
	searchWith := func(ref, orgFullURL string) string {
		s := bareRefSearchset(ref)
		if orgFullURL == "" {
			return s
		}
		return strings.TrimSuffix(s, "]}") + `,{"fullUrl":"` + orgFullURL + `","resource":` + org + `,"search":{"mode":"include"}}]}`
	}
	for name, row := range map[string]struct {
		fhirServer string
		search     string
		reads      int32
		want       string // the include's fullUrl; "" for a urn:uuid
	}{
		"absolute on the base, read":    {base, searchWith(base+"/Organization/o1", ""), 2, base + "/Organization/o1"},
		"relative, read":                {base, searchWith("Organization/o1", ""), 2, ""},
		"fhirServer with a slash, read": {base + "/", searchWith(base+"/Organization/o1", ""), 2, base + "/Organization/o1"},
		"the host in another case, included": {base, searchWith("https://EXAMPLE.com:8443/fhir/Organization/o1", "https://EXAMPLE.com:8443/fhir/Organization/o1"), 1,
			"https://EXAMPLE.com:8443/fhir/Organization/o1"},
		"the default port left out, included": {"https://example.com:443/fhir", searchWith("https://example.com/fhir/Organization/o1", "https://example.com/fhir/Organization/o1"), 1,
			"https://example.com/fhir/Organization/o1"},
		"another base, included": {base, searchWith("https://other.example/fhir/Organization/o1", "https://other.example/fhir/Organization/o1"), 1, ""},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				e := newEHRServer(t, ehrRoutes(row.search, ehrAnswer(200, "application/fhir+json", org)))
				env, _ := fhirServerEnv(t, e, FHIRServerReadPrivate)
				env.originator.cfg.ConformanceEnforcement = level
				rec := httptest.NewRecorder()
				env.originator.handleCRDIngress(rec, crdIngressPost(fhirServerRequest(row.fhirServer)))
				if rec.Code != http.StatusOK || env.routeHitCount() != 1 || e.calls.Load() != row.reads {
					t.Fatalf("answer %d %s; %d reads, want %d", rec.Code, rec.Body.String(), e.calls.Load(), row.reads)
				}
				sent := sentRequest(t, env)
				got := carriedSearchsetOf(t, sent)
				if !slices.Equal(got.members, []string{"resourceType", "type", "total", "entry"}) || !slices.Equal(got.modes, []string{"match", "include"}) ||
					got.includes[0] != org || got.matches[0] != serverResource(t, row.search, "Coverage", "c0") {
					t.Fatalf("carried %+v", got)
				}
				if !strings.HasPrefix(got.fullURLs[0], "urn:uuid:") {
					t.Fatalf("the Coverage's fullUrl is %q", got.fullURLs[0])
				}
				if row.want == "" && !strings.HasPrefix(got.fullURLs[1], "urn:uuid:") || row.want != "" && got.fullURLs[1] != row.want {
					t.Fatalf("the Organization's fullUrl is %q, want %q", got.fullURLs[1], row.want)
				}
				for _, never := range []string{"fhirServer", "fhirAuthorization", "ehr-secret-token"} {
					if bytes.Contains(sent, []byte(never)) {
						t.Fatalf("the carried request holds %q", never)
					}
				}
			})
		}
	}
}

// The exception follows the references routing resolved: an included
// Organization's fullUrl is the absolute reference on the base that resolved
// it (a record has at most one: an entry answers only its own fullUrl, the
// read only the reference spelled on the base). Two different absolute
// strings each resolve their own record, and each record carries its own; a
// relative reference never takes the fullUrl from an absolute one (it
// resolves by type and id whatever the fullUrl).
func TestFHIRServerCarriedEntriesAbsolutePayorReference(t *testing.T) {
	const base = "https://example.com:8443/fhir"
	cov := func(id, ref string) string {
		return `{"resource":{"resourceType":"Coverage","id":"` + id + `","status":"active","beneficiary":{"reference":"Patient/m"},"payor":[{"reference":"` + ref + `"}]}}`
	}
	orgEntry := func(fullURL string) string {
		return `{"fullUrl":"` + fullURL + `","resource":{"resourceType":"Organization","id":"o1"}}`
	}
	bundle := func(entries ...string) []byte {
		return []byte(`{"resourceType":"Bundle","type":"searchset","entry":[` + strings.Join(entries, ",") + `]}`)
	}
	upper, lower := "https://EXAMPLE.com:8443/fhir/Organization/o1", base+"/Organization/o1"
	read := []byte(`{"resourceType":"Organization","id":"o1"}`)
	for name, row := range map[string]struct {
		answer []byte
		payor  []byte
		want   []string // each include's fullUrl; "" for a urn:uuid
	}{
		// Each absolute reference resolves its own record (the entry by its
		// fullUrl, the read by its id): each record has the reference that
		// resolved it.
		"two absolute strings, each its record's": {bundle(cov("c0", lower), cov("c1", upper), orgEntry(upper)), read, []string{upper, lower}},
		"relative first, then absolute":           {bundle(cov("c0", "Organization/o1"), cov("c1", lower)), read, []string{lower}},
		"absolute first, then relative":           {bundle(cov("c0", lower), cov("c1", "Organization/o1")), read, []string{lower}},
		"a cancelled Coverage's reference never":  {bundle(strings.Replace(cov("c0", upper), "active", "cancelled", 1), cov("c1", "Organization/o1")), read, []string{""}},
		"an Organization twice: the first only":   {bundle(cov("c0", lower), orgEntry(lower), orgEntry(lower)), read, []string{lower}},
		"a versioned reference never":             {bundle(cov("c0", lower+"/_history/2"), cov("c1", "Organization/o1")), read, []string{""}},
	} {
		t.Run(name, func(t *testing.T) {
			entries, _, ok := fhirServerCarriedEntries(row.answer, row.payor, base)
			if !ok {
				t.Fatal("not read")
			}
			var got []string
			for _, e := range entries {
				if e.mode == "include" {
					got = append(got, e.fullURL)
				} else if e.fullURL != "" {
					t.Fatalf("a match entry has the fullUrl %q", e.fullURL)
				}
			}
			if !slices.Equal(got, row.want) {
				t.Fatalf("include fullUrls %q, want %q", got, row.want)
			}
		})
	}
}

// The explicit fullUrl is the coverage carry's alone: a searchset assembled
// for any other path is byte for byte what it was before the exception
// (pinned), and an explicit one is written as a JSON string.
func TestAssembleSoRSearchsetFullURL(t *testing.T) {
	page := []byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"fullUrl":"https://s/Coverage/c1","resource":{"resourceType":"Coverage","id":"c1","status":"active"},"search":{"mode":"match"}},{"fullUrl":"https://s/Organization/o1","resource":{"resourceType":"Organization","id":"o1"},"search":{"mode":"include"}}]}`)
	p, err := ParseSearchPage(page, "Coverage")
	if err != nil {
		t.Fatal(err)
	}
	v, _, err := assembleSoRSearchset([][]byte{page}, []assemblyEntry{{res: p.Resources[0], mode: "match"}, {res: p.Resources[1], mode: "include"}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	const pinned = `{"resourceType":"Bundle","type":"searchset","total":1,"entry":[{"fullUrl":"urn:uuid:93e72cb4-f097-8c67-8618-45563cf82e0b","resource":{"resourceType":"Coverage","id":"c1","status":"active"},"search":{"mode":"match"}},{"fullUrl":"urn:uuid:34ddbc5c-2113-87c4-81a8-ba606643a040","resource":{"resourceType":"Organization","id":"o1"},"search":{"mode":"include"}}]}`
	if string(v) != pinned {
		t.Fatalf("assembled\n%s\nwant\n%s", v, pinned)
	}
	v, _, err = assembleSoRSearchset([][]byte{page}, []assemblyEntry{{res: p.Resources[0], mode: "match"}, {res: p.Resources[1], mode: "include", fullURL: `https://s/fhir/Organization/o1?"<x>`}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"fullUrl":"https://s/fhir/Organization/o1?\"<x>","resource":{"resourceType":"Organization"`; !bytes.Contains(v, []byte(want)) {
		t.Fatalf("assembled %s", v)
	}
}

// Only what routing resolved is included (shnsdk.ParseCoveragePayer: payor[0],
// its inline identifier first): a second payor never is, nor an Organization
// a Coverage routed by its own identifier also references.
func TestFHIRServerCarriedEntriesFollowRouting(t *testing.T) {
	org := func(id, value string) string {
		return `{"resource":{"resourceType":"Organization","id":"` + id + `","identifier":[{"system":"urn:oid:2.16.840.1.113883.6.300","value":"` + value + `"}]}}`
	}
	covWith := func(payors string) string {
		return `{"resource":{"resourceType":"Coverage","id":"c0","status":"active","beneficiary":{"reference":"Patient/m"},"payor":[` + payors + `]}}`
	}
	bundle := func(entries ...string) []byte {
		return []byte(`{"resourceType":"Bundle","type":"searchset","entry":[` + strings.Join(entries, ",") + `]}`)
	}
	for name, row := range map[string]struct {
		answer []byte
		want   []string // the included Organizations' ids
	}{
		"two payors, both in the answer: the first only": {bundle(covWith(`{"reference":"Organization/o1"},{"reference":"Organization/o2"}`), org("o1", "00001"), org("o2", "00002")), []string{"o1"}},
		"a reference beside an inline identifier: none":  {bundle(covWith(`{"reference":"Organization/o9","identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}}`), org("o9", "OTHER")), nil},
		"a contained payor: none":                        {bundle(strings.Replace(covWith(`{"reference":"#p"}`), `"payor"`, `"contained":[{"resourceType":"Organization","id":"p","identifier":[{"system":"s","value":"v"}]}],"payor"`, 1), org("p", "00001")), nil},
	} {
		t.Run(name, func(t *testing.T) {
			entries, total, ok := fhirServerCarriedEntries(row.answer, nil, "https://example.com/fhir")
			if !ok || total != 1 {
				t.Fatalf("ok %v, total %d", ok, total)
			}
			var got []string
			for _, e := range entries {
				if e.mode == "include" {
					_, id := entryHead(json.RawMessage(row.answer[e.res.Start:e.res.End]))
					got = append(got, id)
				}
			}
			if !slices.Equal(got, row.want) {
				t.Fatalf("included %v, want %v", got, row.want)
			}
		})
	}
}

// The carry's own fence, through the ingress: routing succeeds on a payor
// Organization the gateway read, but that Organization holds another
// patient's record (or opaque content), so the value the carry writes is
// refused by the fill fence, 502 at every level, and nothing is sent.
func TestCRDIngressCarryFenceRefusesTheOrganizationRead(t *testing.T) {
	ident := `"identifier":[{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}]`
	for name, row := range map[string]struct {
		org string
		msg string
	}{
		"another patient contained": {`{"resourceType":"Organization","id":"o1",` + ident + `,"contained":[{"resourceType":"Patient","id":"p","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"someone-else"}]}]}`,
			fillFencedOtherPatient},
		"a record about another patient contained": {`{"resourceType":"Organization","id":"o1",` + ident + `,"contained":[{"resourceType":"Observation","id":"x","status":"final","code":{"text":"x"},"subject":{"reference":"Patient/someone-else"}}]}`,
			fillFencedOtherPatient},
		"a Binary contained": {`{"resourceType":"Organization","id":"o1",` + ident + `,"contained":[{"resourceType":"Binary","id":"b","contentType":"text/plain","data":"eA=="}]}`,
			fillFencedBinary},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				e := newEHRServer(t, ehrRoutes(bareRefSearchset("o1"), ehrAnswer(200, "application/fhir+json", row.org)))
				env, _ := fhirServerEnv(t, e, FHIRServerReadPrivate)
				env.originator.cfg.ConformanceEnforcement = level
				rec := httptest.NewRecorder()
				env.originator.handleCRDIngress(rec, crdIngressPost(fhirServerRequest(e.base)))
				refusedBeforeTheNetwork(t, env, rec, http.StatusBadGateway, row.msg)
				if e.calls.Load() != 2 {
					t.Fatalf("fhirServer read %d times, want the search and the Organization", e.calls.Load())
				}
			})
		}
	}
}

// Through the ingress: a Coverage routed by its own payor identifier carries
// no Organization, even one its payor also references and the answer holds
// (routing never read it), at every level.
func TestCRDIngressCarriesNoOrganizationRoutingDidNotResolve(t *testing.T) {
	search := `{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"Coverage","id":"c0","status":"active","beneficiary":{"reference":"Patient/` + strangerMember + `"},` +
		`"payor":[{"reference":"Organization/o9","identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}},{"reference":"Organization/o2"}]}},` +
		`{"resource":{"resourceType":"Organization","id":"o9","identifier":[{"system":"urn:oid:2.16.840.1.113883.6.300","value":"OTHER"}]}},` +
		`{"resource":{"resourceType":"Organization","id":"o2","identifier":[{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00002"}]}}]}`
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			e := newEHRServer(t, ehrAnswer(200, "application/fhir+json", search))
			env, _ := fhirServerEnv(t, e, FHIRServerReadPrivate)
			env.originator.cfg.ConformanceEnforcement = level
			rec := httptest.NewRecorder()
			env.originator.handleCRDIngress(rec, crdIngressPost(fhirServerRequest(e.base)))
			if rec.Code != http.StatusOK || e.calls.Load() != 1 {
				t.Fatalf("answer %d %s; %d reads", rec.Code, rec.Body.String(), e.calls.Load())
			}
			sent := sentRequest(t, env)
			got := carriedSearchsetOf(t, sent)
			if len(got.matches) != 1 || len(got.includes) != 0 || bytes.Contains(sent, []byte("OTHER")) || bytes.Contains(sent, []byte("00002")) {
				t.Fatalf("carried %+v", got)
			}
		})
	}
}

// Through the ingress, at every level: a cancelled Coverage whose fullUrl is
// the active Coverage's absolute payor reference is not what that reference
// resolves to (routing drops it, then reads the Organization), so the
// Organization routing read is carried, under that reference as its fullUrl.
func TestCRDIngressCarriesThePayorAnUnchosenCoverageShadows(t *testing.T) {
	const base = "https://example.com:8443/fhir"
	ref := base + "/Organization/o1"
	search := `{"resourceType":"Bundle","type":"searchset","entry":[` +
		`{"fullUrl":"` + ref + `","resource":{"resourceType":"Coverage","id":"old","status":"cancelled","beneficiary":{"reference":"Patient/` + strangerMember + `"},"payor":[{"reference":"Organization/o2"}]}},` +
		`{"resource":{"resourceType":"Coverage","id":"c1","status":"active","beneficiary":{"reference":"Patient/` + strangerMember + `"},"payor":[{"reference":"` + ref + `"}]}}]}`
	org := payorOrganization("o1", "00001")
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			e := newEHRServer(t, ehrRoutes(search, ehrAnswer(200, "application/fhir+json", org)))
			env, _ := fhirServerEnv(t, e, FHIRServerReadPrivate)
			env.originator.cfg.ConformanceEnforcement = level
			rec := httptest.NewRecorder()
			env.originator.handleCRDIngress(rec, crdIngressPost(fhirServerRequest(base)))
			if rec.Code != http.StatusOK || e.calls.Load() != 2 {
				t.Fatalf("answer %d %s; %d reads", rec.Code, rec.Body.String(), e.calls.Load())
			}
			got := carriedSearchsetOf(t, sentRequest(t, env))
			if !slices.Equal(got.modes, []string{"match", "include"}) || got.includes[0] != org || got.fullURLs[1] != ref ||
				got.matches[0] != serverResource(t, search, "Coverage", "c1") {
				t.Fatalf("carried %+v", got)
			}
		})
	}
}
