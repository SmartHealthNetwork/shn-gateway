package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The prefetch rows drive the provider ingress's handling of CDS Hooks
// prefetch: a value the EHR sent is kept exactly; a value it left out is read
// from the participant's own system of record, or left out when that system
// cannot provide it; nothing is ever synthesized.

// prefetchMember is the patient of the EHR requests below: the vendored
// signed request's context.patientId. The system of record names the patient
// prefetchSoRID, the same id (values are obtained only then; see
// TestPrefetch_PatientNamedDifferentlyRefused).
const (
	prefetchMember = "example"
	prefetchSoRID  = "example"
)

// searchAnswer is what a scripted search returns.
type searchAnswer struct {
	res SearchResult
	err error
}

// prefetchSoR is a participant's system of record for the prefetch rows: the
// census members, plus prefetchMember (and "other", a different patient),
// with scripted reads and searches. Every call is recorded.
type prefetchSoR struct {
	*censusSoR
	// noSearch hides the census search: prefetchSoR searches only through
	// searchingPrefetchSoR.
	noSearch
	reads    map[string][]byte
	searches map[string]searchAnswer
	search   bool   // false: the connector cannot search
	sorID    string // the system's id for prefetchMember, when not prefetchSoRID
	refErr   error  // what naming prefetchMember fails with, when set

	mu       sync.Mutex
	searched []string
	read     []string
	idReads  int // PatientFHIRRefContext calls for prefetchMember
}

func newPrefetchSoR() *prefetchSoR {
	return &prefetchSoR{
		censusSoR: newCensusSoR(),
		reads: map[string][]byte{
			"Patient/" + prefetchSoRID: []byte("{ \"resourceType\" : \"Patient\",\n  \"id\" : \"" + prefetchSoRID + "\",\n  \"name\" : [ { \"family\" : \"Test\" } ], \"birthDate\" : \"1960-01-01\" }"),
		},
		searches: map[string]searchAnswer{},
		search:   true,
	}
}

var _ ContextSystemOfRecord = (*prefetchSoR)(nil)

func (s *prefetchSoR) base() ContextSystemOfRecord { return legacySoRReader{sor: s.censusSoR} }

func (s *prefetchSoR) ResolvePatientContext(ctx context.Context, member string) (string, Demo, bool, error) {
	switch member {
	case prefetchMember:
		return "pci-example", Demo{}, true, nil
	case "other":
		return "pci-other", Demo{}, true, nil
	}
	return s.base().ResolvePatientContext(ctx, member)
}

func (s *prefetchSoR) PatientFHIRRefContext(ctx context.Context, member string) (string, bool, error) {
	if member == prefetchMember {
		s.mu.Lock()
		s.idReads++
		s.mu.Unlock()
		if s.refErr != nil {
			return "", false, s.refErr
		}
		if s.sorID != "" {
			return "Patient/" + s.sorID, true, nil
		}
		return "Patient/" + prefetchSoRID, true, nil
	}
	return s.base().PatientFHIRRefContext(ctx, member)
}

func (s *prefetchSoR) CoverageInforceContext(ctx context.Context, m string) (bool, string, error) {
	return s.base().CoverageInforceContext(ctx, m)
}

func (s *prefetchSoR) ClinicalContextContext(ctx context.Context, m string) (shnsdk.ClinicalContext, bool, error) {
	return s.base().ClinicalContextContext(ctx, m)
}

func (s *prefetchSoR) SupplementalReportContext(ctx context.Context, m string) ([]byte, bool, error) {
	return s.base().SupplementalReportContext(ctx, m)
}

func (s *prefetchSoR) FacilityRecordsContext(ctx context.Context, m string) (map[string][]byte, bool, error) {
	return s.base().FacilityRecordsContext(ctx, m)
}

func (s *prefetchSoR) OpenOrderContext(ctx context.Context, m string) ([]byte, bool, error) {
	return s.base().OpenOrderContext(ctx, m)
}

func (s *prefetchSoR) OpenCoverageContext(ctx context.Context, m string) ([][]byte, error) {
	return s.base().OpenCoverageContext(ctx, m)
}

func (s *prefetchSoR) ResolveByReferenceContext(_ context.Context, ref string) ([]byte, bool, error) {
	s.mu.Lock()
	s.read = append(s.read, ref)
	s.mu.Unlock()
	b, ok := s.reads[ref]
	return b, ok, nil
}

// noSearch has a search method at the census's depth, so neither is promoted.
type noSearch struct{}

func (noSearch) SearchPatientContext(context.Context, string, string, ...SearchDateRange) (SearchResult, error) {
	panic("not reachable: the selector is ambiguous")
}

// searchingPrefetchSoR is prefetchSoR with the optional search.
type searchingPrefetchSoR struct{ *prefetchSoR }

func (s searchingPrefetchSoR) SearchPatientContext(_ context.Context, resourceType, id string, _ ...SearchDateRange) (SearchResult, error) {
	s.mu.Lock()
	s.searched = append(s.searched, resourceType+"?patient=Patient/"+id)
	s.mu.Unlock()
	if a, ok := s.searches[resourceType]; ok {
		return a.res, a.err
	}
	p := []byte(`{"resourceType":"Bundle","type":"searchset","entry":[]}`)
	return SearchResult{Pages: [][]byte{p}}, nil
}

// sor returns the connector to configure: searching or not.
func (s *prefetchSoR) sor() SystemOfRecord {
	if s.search {
		return searchingPrefetchSoR{s}
	}
	return s
}

func (s *prefetchSoR) calls() (searched, read []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.searched), slices.Clone(s.read)
}

// answer scripts the search for resourceType with pages, as a correct
// connector reports them.
func (s *prefetchSoR) answer(t *testing.T, resourceType string, pages ...[]byte) {
	t.Helper()
	s.searches[resourceType] = searchAnswer{res: resultOf(t, resourceType, pages...)}
}

// sorCoverage is a Coverage for the system of record's patient, in the
// system's own layout, naming the payer the test router knows.
func sorCoverage(id, payerValue string) string {
	return "{\n      \"resourceType\" : \"Coverage\",\n      \"id\" : \"" + id + "\",\n      \"status\" : \"active\",\n      \"beneficiary\" : { \"reference\" : \"Patient/" + prefetchSoRID + "\" },\n      \"payor\" : [ { \"identifier\" : { \"system\" : \"" + shnsdk.CMSPayerIdentity.System + "\", \"value\" : \"" + payerValue + "\" } } ],\n      \"costToBeneficiary\" : [ { \"valueMoney\" : { \"value\" : 10.50 } } ]\n    }"
}

// sorRequest is a ServiceRequest held for subject, with a decimal and an
// escaped character the rows check survive.
func sorRequest(id, subject string) string {
	return "{ \"resourceType\" : \"ServiceRequest\", \"id\" : \"" + id + "\", \"status\" : \"completed\", \"intent\" : \"order\",\n  \"note\" : [ { \"text\" : \"a " + lt + " b\" } ], \"quantityQuantity\" : { \"value\" : 2.50 },\n  \"subject\" : { \"reference\" : \"" + subject + "\" } }"
}

// sorEntry is a match entry whose fullUrl names its resource on the system
// of record.
func sorEntry(res string) string {
	var head struct{ ResourceType, ID string }
	_ = json.Unmarshal([]byte(res), &head)
	return "{ \"fullUrl\": \"https://sor.example/fhir/" + head.ResourceType + "/" + head.ID + "\", \"resource\": " + res + ", \"search\": { \"mode\": \"match\", \"score\": 1 } }"
}

func searchPage(entries ...string) []byte {
	var e []string
	for _, r := range entries {
		e = append(e, sorEntry(r))
	}
	return page("", "", e...)
}

// ehrCoverage is the EHR's own coverage prefetch value.
const ehrCoverage = `{"resourceType":"Coverage","id":"c1","beneficiary":{"reference":"Patient/example"},"payor":[{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}}]}`

// ehrRequest is an EHR's order-select request for prefetchMember with the
// callback, in the EHR's layout, carrying the prefetch members given (a JSON
// object body without braces; "" for an empty object, "-" for no prefetch).
func ehrRequest(prefetch string) []byte {
	b := "{\n  \"hookInstance\" : \"hi-9\",\n  \"fhirServer\" : \"https://ehr.example/fhir\",\n  \"hook\" : \"order-select\",\n" +
		"  \"fhirAuthorization\" : { \"access_token\" : \"ehr-secret-token\", \"token_type\" : \"Bearer\", \"expires_in\" : 300 },\n" +
		"  \"context\" : { \"userId\" : \"Practitioner/p1\", \"patientId\" : \"example\",\n" +
		"    \"draftOrders\" : { \"resourceType\" : \"Bundle\", \"type\" : \"collection\", \"entry\" : [ { \"resource\" : { \"resourceType\" : \"ServiceRequest\", \"id\" : \"sr1\", \"status\" : \"draft\", \"intent\" : \"order\", \"subject\" : { \"reference\" : \"Patient/example\" }, \"quantityQuantity\" : { \"value\" : 1.50 } } } ] },\n" +
		"    \"selections\" : [ \"ServiceRequest/sr1\" ] }"
	if prefetch != "-" {
		b += ",\n  \"prefetch\" : {" + prefetch + "}"
	}
	return []byte(b + "\n}\n")
}

// signedEHRRequest is the vendored EHR order-sign request: its coverage
// prefetch is a signed Bundle, serviceHistory is null, deviceHistory is a
// searchset, and medicationHistory and questionnaireResponses are absent.
func signedEHRRequest(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "relayfidelity", "valid", "crd-order-sign-request.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// prepare runs the ingress preparation and returns the bytes it would send.
func prepare(t *testing.T, g *Gateway, body []byte) (crdIngressRequest, []byte, int, string) {
	t.Helper()
	p, status, msg := g.ingressEnsureSelfContainedContext(context.Background(), "crd-order-select", body, prefetchMember)
	if status != 0 {
		return p, nil, status, msg
	}
	return p, relay.BytesForTest(p.request), 0, ""
}

func mustPrepare(t *testing.T, g *Gateway, body []byte) (crdIngressRequest, []byte) {
	t.Helper()
	p, sent, status, msg := prepare(t, g, body)
	if status != 0 {
		t.Fatalf("prepare: %d %s", status, msg)
	}
	return p, sent
}

// members returns obj's members in order with their exact value bytes.
type member struct {
	name  string
	value string
}

func membersOf(t *testing.T, b []byte, path ...string) []member {
	t.Helper()
	doc, err := relay.Doc(relay.NewBody(b, relay.OriginIngressRequest))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	n := doc.Root()
	for _, p := range path {
		var ok bool
		if n, ok = doc.Member(n, p); !ok {
			return nil
		}
	}
	var out []member
	for _, m := range doc.Members(n) {
		s, e := doc.Span(m.Value)
		out = append(out, member{m.Name, string(b[s:e])})
	}
	return out
}

func valueOf(t *testing.T, b []byte, path ...string) (string, bool) {
	t.Helper()
	parent := membersOf(t, b, path[:len(path)-1]...)
	for _, m := range parent {
		if m.name == path[len(path)-1] {
			return m.value, true
		}
	}
	return "", false
}

func names(ms []member) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.name)
	}
	return out
}

// prefetchGateway is a provider gateway opted in to enrichment
// (Config.EnrichNativeRequests), the prefetch fill (E-02) these rows pin.
// carryGateway is the participant's default, which fills nothing.
func prefetchGateway(s *prefetchSoR) *Gateway {
	return &Gateway{cfg: Config{SoR: s.sor(), PayerRouter: nil, EnrichNativeRequests: true}}
}

func carryGateway(s *prefetchSoR) *Gateway {
	return &Gateway{cfg: Config{SoR: s.sor(), PayerRouter: nil}}
}

func TestPrefetch_SuppliedKeptExactly(t *testing.T) {
	s := newPrefetchSoR()
	history := "{ \"resourceType\" : \"Bundle\", \"type\" : \"searchset\", \"entry\" : [ { \"resource\" : " + sorRequest("h1", "Patient/example") + " } ] }"
	supplied := map[string]string{
		"patient":                "{ \"resourceType\" : \"Patient\", \"id\" : \"example\", \"name\" : [ { \"text\" : \"x " + lt + " y\" } ] }",
		"coverage":               ehrCoverage,
		"serviceHistory":         history,
		"deviceHistory":          `{"resourceType":"Bundle","type":"searchset","total":0}`,
		"medicationHistory":      `{"resourceType":"Bundle","type":"searchset","entry":[]}`,
		"questionnaireResponses": `{"resourceType":"Bundle","type":"searchset","total":1.0e0}`,
	}
	var parts []string
	for _, k := range []string{"questionnaireResponses", "coverage", "patient", "serviceHistory", "deviceHistory", "medicationHistory"} {
		parts = append(parts, "\n    \""+k+"\" :  "+supplied[k])
	}
	body := ehrRequest(strings.Join(parts, ","))
	p, sent := mustPrepare(t, prefetchGateway(s), body)
	if searched, read := s.calls(); len(searched) != 0 || len(read) != 0 {
		t.Fatalf("a request carrying every key read the system of record: searched %v, read %v", searched, read)
	}
	if got := p.request.Edits(); !slices.Equal(got, []relay.EditID{relay.EditCDSCallbackStrip}) {
		t.Fatalf("edits = %v, want only the callback strip", got)
	}
	got := membersOf(t, sent, "prefetch")
	want := membersOf(t, body, "prefetch")
	if !slices.Equal(got, want) {
		t.Fatalf("prefetch changed:\n got %v\nwant %v", got, want)
	}
	for k, v := range supplied {
		if string(p.values[k]) != v {
			t.Errorf("values[%s] = %s", k, p.values[k])
		}
	}
	// Everything but the callback is the EHR's.
	wantTop := membersOf(t, body)
	wantTop = slices.DeleteFunc(wantTop, func(m member) bool { return m.name == "fhirServer" || m.name == "fhirAuthorization" })
	if gotTop := membersOf(t, sent); !slices.Equal(gotTop, wantTop) {
		t.Fatalf("request members changed:\n got %v\nwant %v", names(gotTop), names(wantTop))
	}
}

func TestPrefetch_NullKeptNull(t *testing.T) {
	s := newPrefetchSoR()
	body := ehrRequest(`"patient":{"resourceType":"Patient","id":"example"},"coverage":` + ehrCoverage + `,"serviceHistory" :  null,"deviceHistory":null,"medicationHistory":null,"questionnaireResponses":null`)
	_, sent := mustPrepare(t, prefetchGateway(s), body)
	for _, k := range []string{"serviceHistory", "deviceHistory", "medicationHistory", "questionnaireResponses"} {
		if v, ok := valueOf(t, sent, "prefetch", k); !ok || v != "null" {
			t.Errorf("prefetch.%s = %q (present %v), want the EHR's null", k, v, ok)
		}
	}
	if searched, _ := s.calls(); len(searched) != 0 {
		t.Fatalf("a null the EHR sent was searched for: %v", searched)
	}
}

func TestPrefetch_MissingPatientReadFromSoR(t *testing.T) {
	s := newPrefetchSoR()
	body := ehrRequest(`"coverage":` + ehrCoverage)
	p, sent := mustPrepare(t, prefetchGateway(s), body)
	want := string(s.reads["Patient/"+prefetchSoRID])
	if v, _ := valueOf(t, sent, "prefetch", "patient"); v != want {
		t.Fatalf("prefetch.patient = %s\nwant the system of record's bytes %s", v, want)
	}
	if _, read := s.calls(); !slices.Equal(read, []string{"Patient/" + prefetchSoRID}) {
		t.Fatalf("reads = %v", read)
	}
	if !slices.Contains(p.request.Edits(), relay.EditCDSPrefetchObtain) {
		t.Fatalf("edits = %v, want the prefetch-obtain edit", p.request.Edits())
	}
	// The EHR's coverage is still first and unchanged; the obtained keys follow it.
	got := names(membersOf(t, sent, "prefetch"))
	if got[0] != "coverage" || got[1] != "patient" {
		t.Fatalf("prefetch members = %v", got)
	}
	if v, _ := valueOf(t, sent, "prefetch", "coverage"); v != ehrCoverage {
		t.Fatalf("coverage = %s", v)
	}

	t.Run("patient not in the system of record", func(t *testing.T) {
		s := newPrefetchSoR()
		delete(s.reads, "Patient/"+prefetchSoRID)
		if _, _, status, msg := prepare(t, prefetchGateway(s), body); status != http.StatusUnprocessableEntity || msg != "patient not found in system of record" {
			t.Fatalf("got %d %s", status, msg)
		}
	})
	t.Run("member unknown to the system of record", func(t *testing.T) {
		g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
		_, status, msg := g.ingressEnsureSelfContainedContext(context.Background(), "crd-order-select", []byte(`{"hook":"order-select","context":{"patientId":"MBR-UNKNOWN"},"prefetch":{}}`), "MBR-UNKNOWN")
		if status != http.StatusUnprocessableEntity || msg != "patient not found in system of record" {
			t.Fatalf("got %d %s", status, msg)
		}
	})
	t.Run("the read is not a Patient", func(t *testing.T) {
		s := newPrefetchSoR()
		s.reads["Patient/"+prefetchSoRID] = []byte(`{"resourceType":"Organization","id":"example"}`)
		if _, _, status, _ := prepare(t, prefetchGateway(s), body); status != http.StatusBadGateway {
			t.Fatalf("got %d", status)
		}
	})
	t.Run("the read is another patient", func(t *testing.T) {
		s := newPrefetchSoR()
		s.reads["Patient/"+prefetchSoRID] = []byte(`{"resourceType":"Patient","id":"example","identifier":[{"system":"urn:shn:member","value":"someone-else"}]}`)
		if _, _, status, msg := prepare(t, prefetchGateway(s), body); status != http.StatusBadGateway || msg != "system of record returned another patient's resource" {
			t.Fatalf("got %d %s", status, msg)
		}
	})
	t.Run("no prefetch object at all", func(t *testing.T) {
		s := newPrefetchSoR()
		p, sent := mustPrepare(t, prefetchGateway(s), ehrRequest("-"))
		if v, _ := valueOf(t, sent, "prefetch", "patient"); v != want {
			t.Fatalf("prefetch.patient = %s", v)
		}
		if got := p.request.Edits(); !slices.Equal(got, []relay.EditID{relay.EditCDSCallbackStrip, relay.EditCDSPrefetchObtain}) {
			t.Fatalf("edits = %v", got)
		}
	})
	t.Run("prefetch that is not an object", func(t *testing.T) {
		s := newPrefetchSoR()
		b := bytes.Replace(ehrRequest("-"), []byte("\n}\n"), []byte(",\n  \"prefetch\" : null\n}\n"), 1)
		if _, _, status, _ := prepare(t, prefetchGateway(s), b); status != http.StatusBadRequest {
			t.Fatalf("got %d", status)
		}
	})
}

func TestPrefetch_MissingCoverageSearchsetFromSoR(t *testing.T) {
	t.Run("one page is sent as the gateway's searchset of the server's records", func(t *testing.T) {
		s := newPrefetchSoR()
		pg := searchPage(sorCoverage("cov-1", "00001"))
		s.answer(t, "Coverage", pg)
		p, sent := mustPrepare(t, prefetchGateway(s), ehrRequest(`"patient":{"resourceType":"Patient","id":"example"}`))
		v, _ := valueOf(t, sent, "prefetch", "coverage")
		if v != sorAssembly(t, "Coverage", pg) {
			t.Fatalf("coverage = %s\nwant the records of %s", v, pg)
		}
		if strings.Contains(v, "sor.example") || !strings.Contains(v, sorCoverage("cov-1", "00001")) {
			t.Fatalf("coverage = %s: want the record exactly and no address of the system of record", v)
		}
		if !p.coverageFromSoR {
			t.Fatal("coverage not marked as read from the system of record")
		}
		if searched, _ := s.calls(); !slices.Contains(searched, "Coverage?patient=Patient/"+prefetchSoRID) {
			t.Fatalf("searches = %v", searched)
		}
	})
	t.Run("several pages are assembled from the exact entries", func(t *testing.T) {
		s := newPrefetchSoR()
		e1, e2 := sorEntry(sorCoverage("cov-1", "00001")), sorEntry(sorCoverage("cov-2", "00001"))
		s.answer(t, "Coverage", page("", "https://sor.example/fhir/p2", e1), page("", "", e2))
		_, sent := mustPrepare(t, prefetchGateway(s), ehrRequest(`"patient":{"resourceType":"Patient","id":"example"}`))
		want := sorAssembly(t, "Coverage", page("", "https://sor.example/fhir/p2", e1), page("", "", e2))
		if v, _ := valueOf(t, sent, "prefetch", "coverage"); v != want {
			t.Fatalf("coverage = %s\nwant %s", v, want)
		}
	})
}

// ingressRow runs the whole provider ingress for body against a system of
// record, through the in-process network, and returns the EHR's answer.
// ingressRow posts body to the provider CRD ingress of a gateway opted in to
// enrichment (the prefetch fill these rows pin); carryRow does the same at the
// participant's default, which fills nothing.
func ingressRow(t *testing.T, s *prefetchSoR, body []byte) (*inProcessExchange, *httptest.ResponseRecorder) {
	t.Helper()
	return ingressRowWith(t, s, body, true)
}

func carryRow(t *testing.T, s *prefetchSoR, body []byte) (*inProcessExchange, *httptest.ResponseRecorder) {
	t.Helper()
	return ingressRowWith(t, s, body, false)
}

func ingressRowWith(t *testing.T, s *prefetchSoR, body []byte, enrich bool) (*inProcessExchange, *httptest.ResponseRecorder) {
	t.Helper()
	env := newInProcessExchange(t)
	env.originator.cfg.SoR = s.sor()
	env.originator.cfg.EnrichNativeRequests = enrich
	rec := httptest.NewRecorder()
	env.originator.handleCRDIngress(rec, crdIngressPost(body))
	return env, rec
}

func refusedBeforeTheNetwork(t *testing.T, env *inProcessExchange, rec *httptest.ResponseRecorder, status int, msg string) {
	t.Helper()
	if rec.Code != status || !strings.Contains(rec.Body.String(), msg) {
		t.Fatalf("answer %d %s, want %d %q", rec.Code, rec.Body.String(), status, msg)
	}
	if n := env.routeHitCount(); n != 0 {
		t.Fatalf("the refused request crossed the network (%d)", n)
	}
}

var patientOnly = `"patient":{"resourceType":"Patient","id":"example"}`

func TestPrefetch_NoCoverage422(t *testing.T) {
	s := newPrefetchSoR() // the coverage search finds nothing
	g := prefetchGateway(s)
	_, sent := mustPrepare(t, g, ehrRequest(patientOnly))
	if v, _ := valueOf(t, sent, "prefetch", "coverage"); v != "null" {
		t.Fatalf("coverage = %q, want null for no match", v)
	}
	env, rec := ingressRow(t, s, ehrRequest(patientOnly))
	refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, "no coverage in request or system of record")

	t.Run("the EHR's own null", func(t *testing.T) {
		env, rec := ingressRow(t, newPrefetchSoR(), ehrRequest(patientOnly+`,"coverage":null`))
		refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, "no coverage in request or system of record")
	})
}

func TestPrefetch_AmbiguousCoverage422(t *testing.T) {
	s := newPrefetchSoR()
	s.answer(t, "Coverage", searchPage(sorCoverage("cov-1", "00001"), sorCoverage("cov-2", "00078")))
	env, rec := ingressRow(t, s, ehrRequest(patientOnly))
	refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, "ambiguous coverage for routing")

	t.Run("the EHR's own Bundle", func(t *testing.T) {
		bundle := `{"resourceType":"Bundle","type":"collection","entry":[{"resource":` + ehrCoverage + `},{"resource":` + strings.Replace(ehrCoverage, `"00001"`, `"00078"`, 1) + `}]}`
		env, rec := ingressRow(t, newPrefetchSoR(), ehrRequest(patientOnly+`,"coverage":`+bundle))
		refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, "ambiguous coverage for routing")
	})
}

func TestPrefetch_CoverageUnavailable503(t *testing.T) {
	s := newPrefetchSoR()
	s.searches["Coverage"] = searchAnswer{err: &SearchError{Outcome: SearchUnavailable, Reason: "server error"}}
	g := prefetchGateway(s)
	p, sent := mustPrepare(t, g, ehrRequest(patientOnly))
	if _, ok := valueOf(t, sent, "prefetch", "coverage"); ok {
		t.Fatal("an unavailable coverage search inserted a value")
	}
	if p.coverageStatus != http.StatusServiceUnavailable {
		t.Fatalf("coverageStatus = %d", p.coverageStatus)
	}
	env, rec := ingressRow(t, s, ehrRequest(patientOnly))
	refusedBeforeTheNetwork(t, env, rec, http.StatusServiceUnavailable, "coverage unavailable from system of record")

	t.Run("a connector that cannot search", func(t *testing.T) {
		s := newPrefetchSoR()
		s.search = false
		env, rec := ingressRow(t, s, ehrRequest(patientOnly))
		refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, "the system of record cannot search for it")
	})
}

func TestPrefetch_CoverageMalformed422(t *testing.T) {
	for name, answer := range map[string]func(t *testing.T, s *prefetchSoR){
		"not a searchset": func(t *testing.T, s *prefetchSoR) {
			s.searches["Coverage"] = searchAnswer{res: SearchResult{Pages: [][]byte{[]byte(`{"resourceType":"Bundle","type":"collection"}`)}}}
		},
		"over the bound": func(t *testing.T, s *prefetchSoR) {
			s.searches["Coverage"] = searchAnswer{res: overBound(t, "Coverage", sorCoverage("c", "00001"))}
		},
		"repeated member": func(t *testing.T, s *prefetchSoR) {
			s.searches["Coverage"] = searchAnswer{err: &SearchError{Outcome: SearchMalformed, Reason: "repeated member name"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := newPrefetchSoR()
			answer(t, s)
			env, rec := ingressRow(t, s, ehrRequest(patientOnly))
			refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, "coverage unreadable from system of record")
		})
	}
}

// overBound is a search result one page over the page bound.
func overBound(t *testing.T, resourceType, resource string) SearchResult {
	t.Helper()
	var pages [][]byte
	for i := 0; i <= SoRSearchMaxPages; i++ {
		next := ""
		if i < SoRSearchMaxPages {
			next = "https://sor.example/fhir/next"
		}
		pages = append(pages, page("", next, matchEntry(strings.Replace(resource, `"id":"d"`, `"id":"d`+strconv.Itoa(i)+`"`, 1))))
	}
	return resultOf(t, resourceType, pages...)
}

// supported is a request carrying the patient and the coverage.
var supported = patientOnly + `,"coverage":` + ehrCoverage

func TestPrefetch_HistoryZeroMatchesInsertsNull(t *testing.T) {
	s := newPrefetchSoR()
	_, sent := mustPrepare(t, prefetchGateway(s), ehrRequest(supported))
	got := membersOf(t, sent, "prefetch")
	want := []member{
		{"patient", `{"resourceType":"Patient","id":"example"}`},
		{"coverage", ehrCoverage},
		{"serviceHistory", "null"}, {"deviceHistory", "null"}, {"medicationHistory", "null"}, {"questionnaireResponses", "null"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("prefetch = %v", got)
	}
	searched, _ := s.calls()
	wantSearched := []string{
		"ServiceRequest?patient=Patient/example", "DeviceRequest?patient=Patient/example",
		"MedicationRequest?patient=Patient/example", "QuestionnaireResponse?patient=Patient/example",
	}
	if !slices.Equal(searched, wantSearched) {
		t.Fatalf("searched %v", searched)
	}

	t.Run("matches are the server's records, exactly", func(t *testing.T) {
		s := newPrefetchSoR()
		pg := searchPage(sorRequest("h1", "Patient/"+prefetchSoRID), sorRequest("h2", "Patient/example"))
		s.answer(t, "ServiceRequest", pg)
		_, sent := mustPrepare(t, prefetchGateway(s), ehrRequest(supported))
		if v, _ := valueOf(t, sent, "prefetch", "serviceHistory"); v != sorAssembly(t, "ServiceRequest", pg) {
			t.Fatalf("serviceHistory = %s", v)
		}
	})
}

func TestPrefetch_HistoryUnsupportedOmitted(t *testing.T) {
	s := newPrefetchSoR()
	s.search = false
	p, sent := mustPrepare(t, prefetchGateway(s), ehrRequest(supported))
	if got := names(membersOf(t, sent, "prefetch")); !slices.Equal(got, []string{"patient", "coverage"}) {
		t.Fatalf("prefetch keys = %v, want the unsupported histories left out", got)
	}
	if got := p.request.Edits(); !slices.Equal(got, []relay.EditID{relay.EditCDSCallbackStrip}) {
		t.Fatalf("edits = %v", got)
	}
	env, rec := ingressRow(t, s, ehrRequest(supported))
	if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
		t.Fatalf("an EHR request with its histories left out: %d %s", rec.Code, rec.Body.String())
	}

	t.Run("the server does not support the search", func(t *testing.T) {
		s := newPrefetchSoR()
		s.searches["DeviceRequest"] = searchAnswer{err: &SearchError{Outcome: SearchUnsupported, Reason: "search not supported"}}
		_, sent := mustPrepare(t, prefetchGateway(s), ehrRequest(supported))
		if _, ok := valueOf(t, sent, "prefetch", "deviceHistory"); ok {
			t.Fatal("deviceHistory inserted")
		}
		if v, _ := valueOf(t, sent, "prefetch", "medicationHistory"); v != "null" {
			t.Fatalf("medicationHistory = %q", v)
		}
	})
}

// observed collects the observer's prefetch events.
type observed struct {
	mu     sync.Mutex
	events []ObserverEvent
}

func (o *observed) observe(e ObserverEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, e)
}

func (o *observed) prefetch(t *testing.T) map[string]prefetchObtained {
	t.Helper()
	return o.prefetchOn(t, "crd-order-select")
}

// prefetchOn is prefetch for the events recorded on leg.
func (o *observed) prefetchOn(t *testing.T, leg string) map[string]prefetchObtained {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	out := map[string]prefetchObtained{}
	for _, e := range o.events {
		if e.Kind != PrefetchObtainedEvent {
			continue
		}
		var p prefetchObtained
		if err := json.Unmarshal([]byte(e.Detail), &p); err != nil {
			t.Fatalf("event detail %q: %v", e.Detail, err)
		}
		if e.Op != p.Key || e.LegType != leg || e.Direction != "sor" || len(e.Payload) != 0 {
			t.Errorf("event %+v", e)
		}
		out[p.Key] = p
	}
	return out
}

func TestPrefetch_HistoryBackendErrorOmittedAndRecorded(t *testing.T) {
	s := newPrefetchSoR()
	s.searches["ServiceRequest"] = searchAnswer{err: &SearchError{Outcome: SearchUnavailable, Reason: "server error"}}
	s.searches["MedicationRequest"] = searchAnswer{err: errors.New("connection reset")}
	obs := &observed{}
	env := newInProcessExchange(t)
	env.originator.cfg.EnrichNativeRequests = true // the prefetch fill this row pins
	env.originator.cfg.SoR = s.sor()
	env.originator.cfg.Observer = obs.observe
	rec := httptest.NewRecorder()
	env.originator.handleCRDIngress(rec, crdIngressPost(ehrRequest(supported)))
	if rec.Code != http.StatusOK {
		t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
	}
	sent := sentRequest(t, env)
	for _, k := range []string{"serviceHistory", "medicationHistory"} {
		if _, ok := valueOf(t, sent, "prefetch", k); ok {
			t.Errorf("%s was inserted after a backend failure", k)
		}
	}
	events := obs.prefetch(t)
	for _, k := range []string{"serviceHistory", "medicationHistory"} {
		if e := events[k]; e.Outcome != SearchUnavailable || e.Reason == "" {
			t.Errorf("%s recorded as %+v, want unavailable with a reason", k, e)
		}
	}
	if e := events["deviceHistory"]; e.Outcome != SearchZero {
		t.Errorf("deviceHistory recorded as %+v", e)
	}
}

func TestPrefetch_HistoryOverBoundOmitted(t *testing.T) {
	s := newPrefetchSoR()
	s.searches["DeviceRequest"] = searchAnswer{res: overBound(t, "DeviceRequest", `{"resourceType":"DeviceRequest","id":"d","status":"active","intent":"order","subject":{"reference":"Patient/example"}}`)}
	obs := &observed{}
	g := prefetchGateway(s)
	g.cfg.Observer = obs.observe
	g.cfg.Clock = fixedClock
	_, sent := mustPrepare(t, g, ehrRequest(supported))
	if _, ok := valueOf(t, sent, "prefetch", "deviceHistory"); ok {
		t.Fatal("an over-bound search was inserted")
	}
	if e := obs.prefetch(t)["deviceHistory"]; e.Outcome != SearchBound || e.Pages != SoRSearchMaxPages+1 {
		t.Fatalf("recorded %+v", e)
	}
}

func TestPrefetch_WrongSubjectSearchResult502(t *testing.T) {
	for name, key := range map[string]string{"ServiceRequest": "serviceHistory", "Coverage": "coverage"} {
		t.Run(key, func(t *testing.T) {
			s := newPrefetchSoR()
			if name == "Coverage" {
				s.answer(t, "Coverage", searchPage(strings.Replace(sorCoverage("c", "00001"), "Patient/"+prefetchSoRID, "Patient/other", 1)))
			} else {
				s.answer(t, name, searchPage(sorRequest("h1", "Patient/"+prefetchSoRID), sorRequest("h2", "Patient/other")))
			}
			body := ehrRequest(patientOnly)
			if key != "coverage" {
				body = ehrRequest(supported)
			}
			env, rec := ingressRow(t, s, body)
			refusedBeforeTheNetwork(t, env, rec, http.StatusBadGateway, "system of record returned another patient's resource")
		})
	}
}

func TestPrefetch_KeptValueForAnotherPatientRefused(t *testing.T) {
	for name, value := range map[string]string{
		"history entry":       `{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"ServiceRequest","id":"x","status":"active","intent":"order","subject":{"reference":"Patient/other"}}}]}`,
		"patient in a bundle": `{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"Patient","id":"other"}}]}`,
		"untyped identifier":  `{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"ServiceRequest","id":"x","status":"active","intent":"order","subject":{"identifier":{"system":"urn:mrn","value":"1"}}}}]}`,
		"foreign server":      `{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"ServiceRequest","id":"x","status":"active","intent":"order","subject":{"reference":"https://elsewhere.example/fhir/Patient/example"}}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			env, rec := ingressRow(t, newPrefetchSoR(), ehrRequest(supported+`,"serviceHistory":`+value))
			refusedBeforeTheNetwork(t, env, rec, http.StatusForbidden, "prefetch serviceHistory refused")
		})
	}
	t.Run("absolute reference on the EHR's own server", func(t *testing.T) {
		value := `{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"ServiceRequest","id":"x","status":"active","intent":"order","subject":{"reference":"https://ehr.example/fhir/Patient/example"}}}]}`
		if _, _, status, msg := prepare(t, prefetchGateway(newPrefetchSoR()), ehrRequest(supported+`,"serviceHistory":`+value)); status != 0 {
			t.Fatalf("got %d %s", status, msg)
		}
	})
}

// sentRequest is the CDS Hooks request the payer's side received.
func sentRequest(t *testing.T, env *inProcessExchange) []byte {
	t.Helper()
	b := env.lastRequestPayload()
	if shnsdk.IsFramed(b) {
		_, body, err := shnsdk.DecodeHTTPFrame(b)
		if err != nil {
			t.Fatalf("decode request frame: %v", err)
		}
		return body
	}
	return b
}

func TestPrefetch_FHIRServerNeverFetched(t *testing.T) {
	var hits atomic.Int32
	spy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "unexpected", http.StatusTeapot)
	}))
	defer spy.Close()
	body := bytes.Replace(ehrRequest(patientOnly), []byte("https://ehr.example/fhir"), []byte(spy.URL), 1)
	s := newPrefetchSoR()
	s.answer(t, "Coverage", searchPage(sorCoverage("cov-1", "00001")))
	env, rec := ingressRow(t, s, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the EHR's fhirServer received %d requests", n)
	}
	sent := sentRequest(t, env)
	if bytes.Contains(sent, []byte(spy.URL)) || bytes.Contains(sent, []byte(`"fhirServer"`)) {
		t.Fatalf("the payer received the EHR's server: %s", sent)
	}
}

func TestPrefetch_AuthorizationNeverForwarded(t *testing.T) {
	env, rec := ingressRow(t, newPrefetchSoR(), ehrRequest(supported))
	if rec.Code != http.StatusOK {
		t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
	}
	sent := sentRequest(t, env)
	for _, s := range []string{`"fhirAuthorization"`, "ehr-secret-token", "access_token"} {
		if bytes.Contains(sent, []byte(s)) {
			t.Fatalf("the payer received %s: %s", s, sent)
		}
	}
}

func TestPrefetchProvenanceEmitted(t *testing.T) {
	s := newPrefetchSoR()
	marker := "clinical-content-marker"
	s.answer(t, "ServiceRequest", searchPage(strings.Replace(sorRequest("h1", "Patient/"+prefetchSoRID), "\"completed\"", "\"completed\", \"id2\" : \""+marker+"\"", 1)))
	s.reads["Patient/"+prefetchSoRID] = []byte(`{"resourceType":"Patient","id":"example","name":[{"family":"` + marker + `"}]}`)
	obs := &observed{}
	g := prefetchGateway(s)
	g.cfg.Observer = obs.observe
	g.cfg.Clock = fixedClock
	mustPrepare(t, g, ehrRequest(`"coverage":`+ehrCoverage))
	events := obs.prefetch(t)
	want := map[string]prefetchObtained{
		"patient":                {Query: "Patient/example", Outcome: SearchOK, Count: 1},
		"serviceHistory":         {Query: "ServiceRequest?patient=Patient%2Fexample", Outcome: SearchOK, Count: 1, Pages: 1},
		"deviceHistory":          {Query: "DeviceRequest?patient=Patient%2Fexample&_include=DeviceRequest%3Aperformer", Outcome: SearchZero, Pages: 1},
		"medicationHistory":      {Query: "MedicationRequest?patient=Patient%2Fexample", Outcome: SearchZero, Pages: 1},
		"questionnaireResponses": {Query: "QuestionnaireResponse?patient=Patient%2Fexample", Outcome: SearchZero, Pages: 1},
	}
	if len(events) != len(want) {
		t.Fatalf("events for %d keys, want %d: %+v", len(events), len(want), events)
	}
	for k, w := range want {
		w.Key, w.Source, w.RetrievedAt = k, "system-of-record", fixedClock().UTC()
		if got := events[k]; got != w {
			t.Errorf("%s: got %+v\nwant %+v", k, got, w)
		}
	}
	for _, e := range obs.events {
		if strings.Contains(e.Detail, marker) || bytes.Contains(e.Payload, []byte(marker)) {
			t.Fatalf("an event carries clinical content: %+v", e)
		}
	}
	// The payload is unchanged: nothing about provenance is added to it.
	_, sent := mustPrepare(t, prefetchGateway(s), ehrRequest(`"coverage":`+ehrCoverage))
	if bytes.Contains(sent, []byte("system-of-record")) || bytes.Contains(sent, []byte("Provenance")) {
		t.Fatalf("provenance leaked into the request: %s", sent)
	}

	// The coverage a questionnaire-package request is routed by (when the
	// EHR sent none) is recorded the same way, naming the operation, whether
	// the search found one, found none or could not run. It is read at the
	// participant's default too, to route by, so this row runs without
	// enrichment.
	t.Run("questionnaire-package coverage", func(t *testing.T) {
		body := ehrParams(ehrOrderParam("sr1", prefetchMember), dtrQuestionnaire)
		query := "Coverage?patient=Patient%2Fexample&_include=Coverage%3Apayor"
		for _, row := range []struct {
			name   string
			answer *searchAnswer
			want   prefetchObtained
		}{
			{"found", &searchAnswer{res: resultOf(t, "Coverage", searchPage(sorCoverage("cov-1", shnsdk.CMSPayerIdentity.Value)))}, prefetchObtained{Outcome: SearchOK, Count: 1, Pages: 1}},
			{"none", nil, prefetchObtained{Outcome: SearchZero, Pages: 1}},
			{"unavailable", &searchAnswer{err: &SearchError{Outcome: SearchUnavailable, Reason: "down"}}, prefetchObtained{Outcome: SearchUnavailable, Reason: "down"}},
		} {
			t.Run(row.name, func(t *testing.T) {
				s := newPrefetchSoR()
				if row.answer != nil {
					s.searches["Coverage"] = *row.answer
				}
				obs := &observed{}
				g := carryGateway(s)
				g.cfg.Observer = obs.observe
				g.cfg.Clock = fixedClock
				_, _, _ = g.prepareDTRPackageRequest(context.Background(), body)
				got := obs.prefetchOn(t, "dtr-questionnaire-fetch")
				w := row.want
				w.Key, w.Operation, w.Source, w.Query, w.RetrievedAt = "coverage", shnsdk.FrameOperationQuestionnairePackage, "system-of-record", query, fixedClock().UTC()
				if len(got) != 1 || got["coverage"] != w {
					t.Fatalf("events %+v\nwant %+v", got, w)
				}
			})
		}
	})
}

func TestPrefetch_NoEditInsideSignedBundle(t *testing.T) {
	body := signedEHRRequest(t)
	s := newPrefetchSoR()
	pg := searchPage(`{"resourceType":"MedicationRequest","id":"m1","status":"active","intent":"order","subject":{"reference":"Patient/example"},"medicationCodeableConcept":{"text":"x"}}`)
	s.answer(t, "MedicationRequest", pg)
	p, sent := mustPrepare(t, prefetchGateway(s), body)
	if got := p.request.Edits(); !slices.Equal(got, []relay.EditID{relay.EditCDSCallbackStrip, relay.EditCDSPrefetchObtain}) {
		t.Fatalf("edits = %v", got)
	}
	orig := membersOf(t, body, "prefetch")
	got := membersOf(t, sent, "prefetch")
	// The EHR's four values — the signed coverage Bundle among them — are
	// kept exactly and in order; the two absent keys follow as siblings.
	want := append(slices.Clone(orig), member{"medicationHistory", sorAssembly(t, "MedicationRequest", pg)}, member{"questionnaireResponses", "null"})
	if !slices.Equal(got, want) {
		t.Fatalf("prefetch:\n got %v\nwant %v", names(got), names(want))
	}
	cov, _ := valueOf(t, body, "prefetch", "coverage")
	if !strings.Contains(cov, `"signature"`) || !bytes.Contains(sent, []byte(cov)) {
		t.Fatal("the signed coverage Bundle is not carried byte for byte")
	}
	// Nothing inside the signed Bundle moved: its span in the sent request
	// is the EHR's span.
	if v, _ := valueOf(t, sent, "prefetch", "coverage"); v != cov {
		t.Fatal("signed Bundle changed")
	}
}

// TestPrefetch_SignedContentRefusalSurfaces proves the ingress reports a
// refusal to edit signed content as the documented 422 rather than sending
// the request: here the whole CDS Hooks request is a signed object.
func TestPrefetch_SignedContentRefusalSurfaces(t *testing.T) {
	body := []byte(`{"hook":"order-select","context":{"patientId":"example"},"fhirServer":"https://ehr.example/fhir","prefetch":{"patient":{"resourceType":"Patient","id":"example"}},` +
		`"signature":{"type":[{"code":"1.2.840.10065.1.12.1.1"}],"when":"2026-01-01T00:00:00Z","who":{"reference":"Practitioner/p"}}}`)
	_, _, status, msg := prepare(t, prefetchGateway(newPrefetchSoR()), body)
	if status != http.StatusUnprocessableEntity || !strings.Contains(msg, "signed content cannot be edited") {
		t.Fatalf("got %d %s", status, msg)
	}
}

// namedDifferently is a system of record that names prefetchMember
// "pat-elsewhere".
func namedDifferently(t *testing.T) *prefetchSoR {
	t.Helper()
	s := newPrefetchSoR()
	s.sorID = "pat-elsewhere"
	s.reads["Patient/pat-elsewhere"] = []byte(`{"resourceType":"Patient","id":"pat-elsewhere","identifier":[{"system":"urn:shn:member","value":"example"}]}`)
	s.answer(t, "Coverage", searchPage(strings.ReplaceAll(sorCoverage("cov-1", "00001"), "Patient/example", "Patient/pat-elsewhere")))
	s.answer(t, "ServiceRequest", searchPage(sorRequest("h1", "Patient/pat-elsewhere")))
	return s
}

// TestPrefetch_PatientNamedDifferentlyRefused: when the system of record
// names the patient by an id other than context.patientId, a request whose
// patient or coverage would have to come from it is refused before anything
// is read or sent; a request carrying every key is unaffected.
func TestPrefetch_PatientNamedDifferentlyRefused(t *testing.T) {
	for name, body := range map[string][]byte{
		"patient absent":  ehrRequest(`"coverage":` + ehrCoverage),
		"coverage absent": ehrRequest(patientOnly),
		"no prefetch":     ehrRequest("-"),
	} {
		t.Run(name, func(t *testing.T) {
			s := namedDifferently(t)
			obs := &observed{}
			env := newInProcessExchange(t)
			env.originator.cfg.EnrichNativeRequests = true // the prefetch fill this row pins
			env.originator.cfg.SoR = s.sor()
			env.originator.cfg.Observer = obs.observe
			rec := httptest.NewRecorder()
			env.originator.handleCRDIngress(rec, crdIngressPost(body))
			refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, patientNamedDifferently)
			if searched, read := s.calls(); len(searched) != 0 || len(read) != 0 {
				t.Fatalf("the system of record was read: searched %v, read %v", searched, read)
			}
			if len(obs.prefetch(t)) != 0 {
				t.Fatal("a refused request recorded an obtained value")
			}
		})
	}
	t.Run("every key supplied", func(t *testing.T) {
		s := newPrefetchSoR()
		s.sorID = "pat-elsewhere"
		env, rec := ingressRow(t, s, ehrRequest(supported+`,"serviceHistory":null,"deviceHistory":null,"medicationHistory":null,"questionnaireResponses":null`))
		if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
			t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
		}
	})
}

// TestPrefetch_HistoryOmittedWhenPatientNamedDifferently: with the patient
// and coverage in the request, a system of record that names the patient
// differently is not searched; the absent history keys are left out with the
// reason recorded, and the request routes.
func TestPrefetch_HistoryOmittedWhenPatientNamedDifferently(t *testing.T) {
	s := namedDifferently(t)
	obs := &observed{}
	env := newInProcessExchange(t)
	env.originator.cfg.EnrichNativeRequests = true // the prefetch fill this row pins
	env.originator.cfg.SoR = s.sor()
	env.originator.cfg.Observer = obs.observe
	rec := httptest.NewRecorder()
	body := ehrRequest(supported + `,"deviceHistory":null`)
	env.originator.handleCRDIngress(rec, crdIngressPost(body))
	if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
		t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
	}
	if searched, read := s.calls(); len(searched) != 0 || len(read) != 0 {
		t.Fatalf("the system of record was read: searched %v, read %v", searched, read)
	}
	sent := sentRequest(t, env)
	if got := names(membersOf(t, sent, "prefetch")); !slices.Equal(got, []string{"patient", "coverage", "deviceHistory"}) {
		t.Fatalf("prefetch keys %v, want only the EHR's", got)
	}
	events := obs.prefetch(t)
	if len(events) != 3 {
		t.Fatalf("events %+v, want one per omitted history key", events)
	}
	for _, k := range []string{"serviceHistory", "medicationHistory", "questionnaireResponses"} {
		e := events[k]
		if e.Outcome != SearchNotRun || e.Reason != historyNamedDifferently || e.Count != 0 || !strings.Contains(e.Query, "Patient%2Fpat-elsewhere") {
			t.Errorf("%s recorded as %+v", k, e)
		}
	}
}

// foreignRecords is a searchset about another patient: an Observation and the
// other patient's own Patient resource.
const foreignRecords = `{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"Observation","id":"o1","status":"final","code":{"text":"x"},"subject":{"reference":"Patient/other"}}},{"resource":{"resourceType":"Patient","id":"other","name":[{"family":"Someone"}]}}]}`

// allAdvertised carries every advertised key, so nothing is obtained.
var allAdvertised = supported + `,"serviceHistory":null,"deviceHistory":null,"medicationHistory":null,"questionnaireResponses":null`

func TestPrefetch_UnadvertisedMemberFenced(t *testing.T) {
	t.Run("an unadvertised key with another patient's records", func(t *testing.T) {
		env, rec := ingressRow(t, newPrefetchSoR(), ehrRequest(allAdvertised+`,"labs":`+foreignRecords))
		refusedBeforeTheNetwork(t, env, rec, http.StatusForbidden, "prefetch labs refused")
	})
	t.Run("a case-variant key with another patient's records", func(t *testing.T) {
		body := ehrRequest(`"Patient":` + foreignRecords + `,"coverage":` + ehrCoverage + `,"serviceHistory":null,"deviceHistory":null,"medicationHistory":null,"questionnaireResponses":null`)
		if _, _, status, msg := prepare(t, prefetchGateway(newPrefetchSoR()), body); status != http.StatusForbidden || !strings.Contains(msg, "prefetch Patient refused") {
			t.Fatalf("got %d %s", status, msg)
		}
		env, rec := ingressRow(t, newPrefetchSoR(), body)
		refusedBeforeTheNetwork(t, env, rec, http.StatusForbidden, "prefetch Patient refused")
	})
	t.Run("a case-variant key beside the advertised one", func(t *testing.T) {
		// Member names that differ only in case are refused as one repeated name.
		body := ehrRequest(allAdvertised + `,"Patient":{"resourceType":"Patient","id":"example"}`)
		if _, _, status, msg := prepare(t, prefetchGateway(newPrefetchSoR()), body); status != http.StatusBadRequest || msg != "parse cds request failed" {
			t.Fatalf("got %d %s", status, msg)
		}
	})
	t.Run("an unadvertised key that is not a resource", func(t *testing.T) {
		if _, _, status, msg := prepare(t, prefetchGateway(newPrefetchSoR()), ehrRequest(allAdvertised+`,"note":"free text"`)); status != http.StatusForbidden || !strings.Contains(msg, "prefetch note refused") {
			t.Fatalf("got %d %s", status, msg)
		}
	})
	t.Run("an unadvertised key about the patient is carried exactly", func(t *testing.T) {
		labs := `{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"Observation","id":"o1","status":"final","code":{"text":"x"},"subject":{"reference":"Patient/example"},"valueQuantity":{"value":1.50}}}]}`
		env, rec := ingressRow(t, newPrefetchSoR(), ehrRequest(allAdvertised+`,"labs":`+labs+`,"none":null`))
		if rec.Code != http.StatusOK {
			t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
		}
		sent := sentRequest(t, env)
		if v, _ := valueOf(t, sent, "prefetch", "labs"); v != labs {
			t.Fatalf("labs carried as %s", v)
		}
		if v, _ := valueOf(t, sent, "prefetch", "none"); v != "null" {
			t.Fatalf("none carried as %s", v)
		}
	})
}

func TestPrefetch_KeptValuesFencedBeforeAnySearch(t *testing.T) {
	s := newPrefetchSoR()
	obs := &observed{}
	env := newInProcessExchange(t)
	env.originator.cfg.SoR = s.sor()
	env.originator.cfg.Observer = obs.observe
	rec := httptest.NewRecorder()
	// The patient and every history key are absent, and the last member the
	// EHR sent is another patient's: nothing may be read or searched first.
	body := ehrRequest(`"coverage":` + ehrCoverage + `,"labs":` + foreignRecords)
	env.originator.handleCRDIngress(rec, crdIngressPost(body))
	refusedBeforeTheNetwork(t, env, rec, http.StatusForbidden, "prefetch labs refused")
	if searched, read := s.calls(); len(searched) != 0 || len(read) != 0 {
		t.Fatalf("a refused request read the system of record: searched %v, read %v", searched, read)
	}
	if events := obs.prefetch(t); len(events) != 0 {
		t.Fatalf("a refused request recorded prefetch attempts: %+v", events)
	}
}

// An obtained coverage carries the payor Organization the search included,
// as an included record: the payer resolves the Coverage's payor from the
// request (the provider's gateway does not read it again either), and no
// address of the system of record is sent.
func TestPrefetch_ObtainedCoverageCarriesIncludedPayor(t *testing.T) {
	cov := "{ \"resourceType\" : \"Coverage\", \"id\" : \"cov-9\", \"status\" : \"active\",\n  \"beneficiary\" : { \"reference\" : \"Patient/" + prefetchSoRID + "\" },\n  \"payor\" : [ { \"reference\" : \"Organization/pay-9\" } ] }"
	org := "{ \"resourceType\" : \"Organization\", \"id\" : \"pay-9\",\n  \"identifier\" : [ { \"system\" : \"" + shnsdk.CMSPayerIdentity.System + "\", \"value\" : \"00001\" } ] }"
	include := func(res string) string {
		return `{"fullUrl":"https://sor.example/fhir/Organization/pay-9","resource":` + res + `,"search":{"mode":"include"}}`
	}
	t.Run("routed from the included payor", func(t *testing.T) {
		s := newPrefetchSoR()
		pg := page("", "", sorEntry(cov), include(org))
		s.answer(t, "Coverage", pg)
		env, rec := ingressRow(t, s, ehrRequest(`"patient":{"resourceType":"Patient","id":"example"}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
		}
		v, _ := valueOf(t, sentRequest(t, env), "prefetch", "coverage")
		if v != sorAssembly(t, "Coverage", pg) {
			t.Fatalf("coverage = %s", v)
		}
		if !strings.Contains(v, org+`,"search":{"mode":"include"}`) || strings.Contains(v, "sor.example") {
			t.Fatalf("coverage = %s: want the payor Organization exactly, as an included record, and no system of record address", v)
		}
		if _, read := s.calls(); slices.Contains(read, "Organization/pay-9") {
			t.Fatalf("the payor was read again from the system of record: %v", read)
		}
	})
	t.Run("an included record about another patient is refused", func(t *testing.T) {
		s := newPrefetchSoR()
		other := `{"resourceType":"Observation","id":"o-1","status":"final","code":{"text":"x"},"subject":{"reference":"Patient/someone-else"}}`
		s.answer(t, "Coverage", page("", "", sorEntry(cov), include(org), include(other)))
		env, rec := ingressRow(t, s, ehrRequest(`"patient":{"resourceType":"Patient","id":"example"}`))
		if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "system of record returned another patient's resource") {
			t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
		}
		if env.routeHitCount() != 0 {
			t.Fatal("a refused request reached the Hub")
		}
	})
}

// A Binary is never carried in a prefetch value: refused with 403 when the
// EHR sent it, 502 when the system of record returned it.
func TestPrefetch_BinaryRefused(t *testing.T) {
	binary := `{"resourceType":"Binary","id":"b1","contentType":"application/pdf","data":"JVBERi0="}`
	t.Run("kept bundle entry", func(t *testing.T) {
		value := `{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + binary + `}]}`
		env, rec := ingressRow(t, newPrefetchSoR(), ehrRequest(supported+`,"serviceHistory":`+value))
		refusedBeforeTheNetwork(t, env, rec, http.StatusForbidden, "prefetch serviceHistory refused: Binary refused: "+opaqueContentReason)
	})
	t.Run("kept value", func(t *testing.T) {
		env, rec := ingressRow(t, newPrefetchSoR(), ehrRequest(allAdvertised+`,"document":`+binary))
		refusedBeforeTheNetwork(t, env, rec, http.StatusForbidden, "prefetch document refused: Binary refused")
	})
	t.Run("obtained", func(t *testing.T) {
		s := newPrefetchSoR()
		s.answer(t, "ServiceRequest", page("", "", sorEntry(sorRequest("h1", "Patient/"+prefetchSoRID)), `{"resource":`+binary+`,"search":{"mode":"include"}}`))
		env, rec := ingressRow(t, s, ehrRequest(supported))
		refusedBeforeTheNetwork(t, env, rec, http.StatusBadGateway, "system of record returned a Binary resource")
	})
}

// The payor a kept coverage references is resolved from every value the
// request carries, whatever its key (a resource, or a Bundle's entries), in
// key order; null values are skipped.
func TestPrefetch_PayorResolvedFromEveryValue(t *testing.T) {
	cov := `{"resourceType":"Coverage","id":"c1","beneficiary":{"reference":"Patient/example"},"payor":[{"reference":"Organization/pay-1"}]}`
	org := `{"resourceType":"Organization","id":"pay-1","identifier":[{"system":"` + shnsdk.CMSPayerIdentity.System + `","value":"00001"}]}`
	for name, extra := range map[string]string{
		"an unadvertised resource value": `,"payer":` + org,
		"an unadvertised bundle value":   `,"organizations":{"resourceType":"Bundle","type":"collection","entry":[{"resource":` + org + `}]},"zzz":null`,
	} {
		t.Run(name, func(t *testing.T) {
			body := ehrRequest(patientOnly + `,"coverage":` + cov + `,"serviceHistory":null,"deviceHistory":null,"medicationHistory":null,"questionnaireResponses":null` + extra)
			s := newPrefetchSoR()
			_, rec := ingressRow(t, s, body)
			if rec.Code != http.StatusOK {
				t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
			}
			if _, read := s.calls(); len(read) != 0 {
				t.Fatalf("the system of record was read: %v", read)
			}
		})
	}
	t.Run("no value names the payor", func(t *testing.T) {
		body := ehrRequest(patientOnly + `,"coverage":` + cov + `,"serviceHistory":null,"deviceHistory":null,"medicationHistory":null,"questionnaireResponses":null`)
		env, rec := ingressRow(t, newPrefetchSoR(), body)
		refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, "no payer identifier on member coverage")
	})
}

// The request's patient references are bound the same way the prefetch fence
// reads them: an absolute reference on the EHR's own fhirServer is the EHR's
// patient, like a relative one; one on another server is refused.
func TestCRDIngress_SubjectAbsoluteOnEHRServer(t *testing.T) {
	onBase := func(base string) []byte {
		b := ehrRequest(allAdvertised)
		b = bytes.Replace(b, []byte(`"subject" : { "reference" : "Patient/example" }`), []byte(`"subject" : { "reference" : "`+base+`Patient/example" }`), 1)
		return bytes.Replace(b, []byte(`"beneficiary":{"reference":"Patient/example"}`), []byte(`"beneficiary":{"reference":"`+base+`Patient/example"}`), 1)
	}
	for _, base := range []string{"https://ehr.example/fhir/"} {
		t.Run("on the EHR's server", func(t *testing.T) {
			body := onBase(base)
			if !bytes.Contains(body, []byte(`"`+base+`Patient/example"`)) {
				t.Fatal("fixture not rewritten")
			}
			env, rec := ingressRow(t, newPrefetchSoR(), body)
			if rec.Code != http.StatusOK {
				t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
			}
			if env.routeHitCount() == 0 {
				t.Fatal("nothing was sent")
			}
			if pci, status, msg := prefetchGateway(newPrefetchSoR()).ingressCRDSubjectPCIContext(context.Background(), body); status != 0 || pci != "pci-example" {
				t.Fatalf("subject binding: %q %d %q", pci, status, msg)
			}
		})
	}
	for name, base := range map[string]string{
		"another server":             "https://elsewhere.example/fhir/",
		"a prefix of the EHR server": "https://ehr.example/",
		"the EHR server's host only": "https://ehr.example/fhirX/",
	} {
		t.Run(name, func(t *testing.T) {
			// The subject binding itself refuses the reference, whatever
			// else would refuse the request later.
			if _, status, msg := prefetchGateway(newPrefetchSoR()).ingressCRDSubjectPCIContext(context.Background(), onBase(base)); status != http.StatusForbidden || msg != "inconsistent patient reference in ingress payload" {
				t.Fatalf("subject binding: %d %q, want 403", status, msg)
			}
			env, rec := ingressRow(t, newPrefetchSoR(), onBase(base))
			if rec.Code != http.StatusForbidden && rec.Code != http.StatusBadRequest {
				t.Fatalf("answer %d %s, want a refusal", rec.Code, rec.Body.String())
			}
			if env.routeHitCount() != 0 {
				t.Fatal("the refused request crossed the network")
			}
		})
	}
}

// unnamedMemberSoR holds prefetchMember (it resolves) but cannot name its
// Patient: an inconsistent system of record.
type unnamedMemberSoR struct{ *prefetchSoR }

func (s unnamedMemberSoR) PatientFHIRRefContext(context.Context, string) (string, bool, error) {
	return "", false, nil
}

// A member the system of record holds but cannot name is an inconsistent
// system of record, refused, never mistaken for a member it does not hold: by
// default a history key is left out only for a member not held.
func TestPrefetch_HeldButUnnamedMemberRefused(t *testing.T) {
	for _, require := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "known members required"}[require], func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.EnrichNativeRequests = true // the prefetch fill this row pins
			env.originator.cfg.SoR = unnamedMemberSoR{newPrefetchSoR()}
			env.originator.cfg.RequireKnownMembers = require
			rec := httptest.NewRecorder()
			env.originator.handleCRDIngress(rec, crdIngressPost(ehrRequest(supported)))
			refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, "patient not found in system of record")
		})
	}
}
