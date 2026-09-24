package engine

import (
	"bytes"
	"context"
	"encoding/json"
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

// Native ingress preserves supplied CDS Hooks prefetch without enrichment.
// Source-owned construction has a separate, reachable originCRDRecords path.

// prefetchMember is the patient of the EHR requests below: the vendored
// signed request's context.patientId. The system of record names the patient
// prefetchSoRID, the same id (source-originated renaming is covered by
// TestPrefetch_OriginatedRenamedPatient).
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

// prepare exercises the retired implicit-preparation helper in isolation.
// Production ingress does not call this helper; source-owned acceptance is
// covered through originCRDRecords below.
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

func prefetchGateway(s *prefetchSoR) *Gateway {
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

// ingressRow sends a body with authenticated exchange context through native
// provider ingress. The supplied body is the producer's message; none does not
// request source enrichment.
func ingressRow(t *testing.T, s *prefetchSoR, body []byte) (*inProcessExchange, *httptest.ResponseRecorder) {
	t.Helper()
	env := newTransportExchange(t)
	env.originator.cfg.SoR = s.sor()
	rec := httptest.NewRecorder()
	req := signedFixtureIngress(t, env.originator, "/cds-services/shn-order-select", "crd-order-select", "crd-order-select", "order-select", "pci-covered", "pa.crd@2.0", "prefetch-native", body)
	req.SetPathValue("id", "shn-order-select")
	env.originator.handleCRDIngress(rec, req)
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
	observationFlush(t, g)
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

	// The coverage a questionnaire-package request is sent with (when the
	// EHR sent none) is recorded the same way, naming the operation, whether
	// the search found one, found none or could not run.
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
				g := prefetchGateway(s)
				g.cfg.Observer = obs.observe
				g.cfg.Clock = fixedClock
				_, _, _ = g.prepareDTRPackageRequest(context.Background(), body)
				observationFlush(t, g)
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

// TestPrefetch_SignedContentRefusalSurfaces exercises the isolated legacy
// source-preparation helper's refusal to edit a signed whole request. Native
// ingress boundary behavior is covered by the signed request rows above.
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

const foreignRecords = `{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"Observation","id":"o1","status":"final","code":{"text":"x"},"subject":{"reference":"Patient/other"}}},{"resource":{"resourceType":"Patient","id":"other","name":[{"family":"Someone"}]}}]}`

// allAdvertised carries every advertised key, so nothing is obtained.
var allAdvertised = supported + `,"serviceHistory":null,"deviceHistory":null,"medicationHistory":null,"questionnaireResponses":null`

// PCV-08/10: a signed native request does not ask the gateway to fill absent
// prefetch members. Missing, null, signed and opaque values remain the sender's
// exact assertion, even when the provider's optional source is unavailable.
func TestPrefetch_NativeSuppliedAbsentAndNull(t *testing.T) {
	for _, row := range []struct {
		name string
		body []byte
	}{
		{"complete", []byte(`{"hook":"order-select","context":{"patientId":"example"},"prefetch":{` + allAdvertised + `}}`)},
		{"absent", []byte(`{"hook":"order-select","context":{"patientId":"example"}}`)},
		{"empty", []byte(`{"hook":"order-select","context":{"patientId":"example"},"prefetch":{}}`)},
		{"explicit null", []byte(`{"hook":"order-select","context":{"patientId":"example"},"prefetch":{"patient":null,"coverage":null,"serviceHistory":null}}`)},
		{"foreign history", []byte(`{"hook":"order-select","context":{"patientId":"example"},"prefetch":{"serviceHistory":` + foreignRecords + `}}`)},
		{"unadvertised foreign records", []byte(`{"hook":"order-select","context":{"patientId":"example"},"prefetch":{"labs":` + foreignRecords + `}}`)},
		{"unadvertised own record", []byte(`{"hook":"order-select","context":{"patientId":"example"},"prefetch":{"labs":{"resourceType":"Observation","id":"o1","subject":{"reference":"Patient/example"},"valueQuantity":{"value":1.50}},"none":null}}`)},
		{"case variant key", []byte(`{"hook":"order-select","context":{"patientId":"example"},"prefetch":{"Patient":` + foreignRecords + `}}`)},
		{"binary history", []byte(`{"hook":"order-select","context":{"patientId":"example"},"prefetch":{"serviceHistory":{"resourceType":"Binary","id":"b1","contentType":"application/pdf","data":"JVBERi0="}}}`)},
		{"coverage without payer", []byte(`{"hook":"order-select","context":{"patientId":"example"},"prefetch":{"coverage":{"resourceType":"Coverage","id":"c1","beneficiary":{"reference":"Patient/example"}}}}`)},
		{"ambiguous supplied coverage", []byte(`{"hook":"order-select","context":{"patientId":"example"},"prefetch":{"coverage":{"resourceType":"Bundle","type":"collection","entry":[{"resource":` + ehrCoverage + `},{"resource":` + strings.Replace(ehrCoverage, `"00001"`, `"00078"`, 1) + `}]}}}`)},
		{"signed coverage", signedEHRRequest(t)},
	} {
		for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve} {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				s := newPrefetchSoR()
				s.searches["Coverage"] = searchAnswer{err: &SearchError{Outcome: SearchUnavailable, Reason: "must not search"}}
				env := newTransportExchange(t)
				env.originator.cfg.ConformanceEnforcement = level
				env.originator.cfg.SoR = s.sor()
				req := signedFixtureIngress(t, env.originator, "/cds-services/shn-order-select", "crd-order-select", "crd-order-select", "order-select", "pci-covered", "pa.crd@2.0", "prefetch-native-"+row.name, row.body)
				req.SetPathValue("id", "shn-order-select")
				rec := httptest.NewRecorder()
				env.originator.handleCRDIngress(rec, req)
				if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
					t.Fatalf("native ingress status=%d body=%s Hub=%d", rec.Code, rec.Body.String(), env.routeHitCount())
				}
				sent := sentRequest(t, env)
				if row.name == "signed coverage" {
					if got, want := membersOf(t, sent, "prefetch"), membersOf(t, row.body, "prefetch"); !slices.Equal(got, want) {
						t.Fatalf("signed prefetch changed: got=%v want=%v", got, want)
					}
				} else if !bytes.Equal(sent, row.body) {
					t.Fatalf("native body changed:\n got %s\nwant %s", sent, row.body)
				}
				if searched, read := s.calls(); len(searched) != 0 || len(read) != 0 || s.idReads != 0 {
					t.Fatalf("native ingress read source: searched=%v read=%v idReads=%d", searched, read, s.idReads)
				}
			})
		}
	}
}

// E-01 remains mandatory even at none: the callback endpoint and bearer are
// removed by the registered boundary edit, without fetching the endpoint.
func TestPrefetch_NativeCallbackAuthorityRemoved(t *testing.T) {
	var hits atomic.Int32
	spy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "unexpected", http.StatusTeapot)
	}))
	defer spy.Close()
	body := bytes.Replace(ehrRequest(allAdvertised), []byte("https://ehr.example/fhir"), []byte(spy.URL), 1)
	s := newPrefetchSoR()
	env, rec := ingressRow(t, s, body)
	if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
		t.Fatalf("status=%d body=%s Hub=%d", rec.Code, rec.Body, env.routeHitCount())
	}
	sent := sentRequest(t, env)
	for _, forbidden := range [][]byte{[]byte(spy.URL), []byte(`"fhirServer"`), []byte(`"fhirAuthorization"`), []byte("ehr-secret-token")} {
		if bytes.Contains(sent, forbidden) {
			t.Fatalf("callback authority crossed boundary: %s", sent)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("callback endpoint fetched %d times", hits.Load())
	}
	if got, want := membersOf(t, sent, "prefetch"), membersOf(t, body, "prefetch"); !slices.Equal(got, want) {
		t.Fatalf("prefetch changed: got=%v want=%v", got, want)
	}
	if searched, read := s.calls(); len(searched) != 0 || len(read) != 0 {
		t.Fatalf("native ingress read source: %v %v", searched, read)
	}
}

// The actual provider-originated construction action still reads source
// records, preserves their pagination/record bytes, and refuses unavailable or
// cross-patient mandatory inputs before any authored request exists.
func TestPrefetch_OriginatedSourceConstruction(t *testing.T) {
	t.Run("source records become authored request", func(t *testing.T) {
		s := newPrefetchSoR()
		patient := s.reads["Patient/"+prefetchSoRID]
		cov := sorCoverage("cov-1", shnsdk.CMSPayerIdentity.Value)
		p1 := page("", "https://sor.example/fhir/next", sorEntry(cov))
		p2 := page("", "", sorEntry(sorCoverage("cov-2", shnsdk.CMSPayerIdentity.Value)))
		s.answer(t, "Coverage", p1, p2)
		history := `{"resourceType":"ServiceRequest","id":"prior-order-1","status":"completed","intent":"order","subject":{"reference":"Patient/example"},"code":{"coding":[{"system":"http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets","code":"G0151"}]},"quantityQuantity":{"value":1.50}}`
		s.answer(t, "ServiceRequest", searchPage(history))
		g := prefetchGateway(s)
		g.cfg.NPI = "1234567890"
		recs, status, msg := g.originCRDRecords(context.Background(), "crd-order-select", prefetchMember)
		if status != 0 {
			t.Fatalf("origin source: %d %s", status, msg)
		}
		if !bytes.Equal(recs.patient, patient) || string(recs.coverage) != sorAssembly(t, "Coverage", p1, p2) || string(recs.history["serviceHistory"]) != sorAssembly(t, "ServiceRequest", searchPage(history)) {
			t.Fatalf("source bytes changed: patient=%s coverage=%s history=%s", recs.patient, recs.coverage, recs.history["serviceHistory"])
		}
		for _, key := range []string{"deviceHistory", "medicationHistory", "questionnaireResponses"} {
			if string(recs.history[key]) != "null" {
				t.Fatalf("%s=%s, want source zero-match null", key, recs.history[key])
			}
		}
		order := []byte(`{"resourceType":"ServiceRequest","id":"new-order","status":"draft","intent":"order","subject":{"reference":"Patient/example"}}`)
		authored, err := g.originatedOrderingRequest(hookOrderSelect, "source-correlation", recs, order)
		if err != nil {
			t.Fatal(err)
		}
		for key, want := range map[string][]byte{"patient": patient, "coverage": recs.coverage, "serviceHistory": recs.history["serviceHistory"]} {
			if got, ok := valueOf(t, authored, "prefetch", key); !ok || got != string(want) {
				t.Fatalf("authored %s=%s want=%s", key, got, want)
			}
		}
		if searched, read := s.calls(); !slices.Contains(searched, "Coverage?patient=Patient/example") || !slices.Contains(searched, "ServiceRequest?patient=Patient/example") || !slices.Contains(read, "Patient/example") {
			t.Fatalf("source not read: searched=%v read=%v", searched, read)
		}
	})
	for _, row := range []struct {
		name   string
		change func(*prefetchSoR)
		status int
		msg    string
	}{
		{"missing patient", func(s *prefetchSoR) { delete(s.reads, "Patient/example") }, http.StatusUnprocessableEntity, "patient not found"},
		{"missing coverage", func(s *prefetchSoR) {}, http.StatusUnprocessableEntity, "no coverage"},
		{"unavailable coverage", func(s *prefetchSoR) {
			s.searches["Coverage"] = searchAnswer{err: &SearchError{Outcome: SearchUnavailable, Reason: "down"}}
		}, http.StatusServiceUnavailable, "coverage unavailable"},
		{"wrong-subject coverage", func(s *prefetchSoR) {
			s.answer(t, "Coverage", searchPage(strings.Replace(sorCoverage("cov-x", "00001"), "Patient/example", "Patient/other", 1)))
		}, http.StatusBadGateway, "another patient's resource"},
		{"wrong-subject patient", func(s *prefetchSoR) {
			s.reads["Patient/example"] = []byte(`{"resourceType":"Patient","id":"other"}`)
		}, http.StatusBadGateway, "another patient's resource"},
		{"malformed coverage search", func(s *prefetchSoR) {
			s.searches["Coverage"] = searchAnswer{res: SearchResult{Pages: [][]byte{[]byte(`{"resourceType":"Bundle","type":"collection"}`)}}}
		}, http.StatusUnprocessableEntity, "coverage unreadable"},
		{"repeated-member coverage search", func(s *prefetchSoR) {
			s.searches["Coverage"] = searchAnswer{res: SearchResult{Pages: [][]byte{[]byte(`{"resourceType":"Bundle","resourceType":"Bundle","type":"searchset","entry":[]}`)}}}
		}, http.StatusUnprocessableEntity, "coverage unreadable"},
		{"over-bound coverage search", func(s *prefetchSoR) {
			s.searches["Coverage"] = searchAnswer{res: overBound(t, "Coverage", sorCoverage("cov-1", "00001"))}
		}, http.StatusUnprocessableEntity, "coverage unreadable"},
		{"binary coverage include", func(s *prefetchSoR) {
			s.answer(t, "Coverage", page("", "", sorEntry(sorCoverage("cov-1", "00001")), `{"resource":{"resourceType":"Binary","id":"b1","contentType":"application/pdf","data":"JVBERi0="},"search":{"mode":"include"}}`))
		}, http.StatusBadGateway, "Binary resource"},
	} {
		t.Run(row.name, func(t *testing.T) {
			s := newPrefetchSoR()
			row.change(s)
			_, status, msg := prefetchGateway(s).originCRDRecords(context.Background(), "crd-order-select", prefetchMember)
			if status != row.status || !strings.Contains(msg, row.msg) {
				t.Fatalf("origin source: %d %s; want %d %q", status, msg, row.status, row.msg)
			}
		})
	}
}

func TestPrefetch_OriginatedHistoryBoundaries(t *testing.T) {
	for _, row := range []struct {
		name    string
		answer  searchAnswer
		outcome SearchOutcome
		status  int
		msg     string
	}{
		{"unsupported", searchAnswer{err: &SearchError{Outcome: SearchUnsupported, Reason: "unsupported"}}, SearchUnsupported, 0, ""},
		{"unavailable", searchAnswer{err: &SearchError{Outcome: SearchUnavailable, Reason: "down"}}, SearchUnavailable, 0, ""},
		{"over bound", searchAnswer{res: overBound(t, "ServiceRequest", sorRequest("h1", "Patient/example"))}, SearchBound, 0, ""},
		{"malformed", searchAnswer{res: SearchResult{Pages: [][]byte{[]byte(`{"resourceType":"Bundle","type":"collection"}`)}}}, SearchMalformed, 0, ""},
		{"wrong subject", searchAnswer{res: resultOf(t, "ServiceRequest", searchPage(sorRequest("h1", "Patient/other")))}, SearchOK, http.StatusBadGateway, "another patient's resource"},
		{"binary include", searchAnswer{res: resultOf(t, "ServiceRequest", page("", "", sorEntry(sorRequest("h1", "Patient/example")), `{"resource":{"resourceType":"Binary","id":"b1","data":"JVBERi0="},"search":{"mode":"include"}}`))}, SearchOK, http.StatusBadGateway, "Binary resource"},
	} {
		t.Run(row.name, func(t *testing.T) {
			s := newPrefetchSoR()
			s.answer(t, "Coverage", searchPage(sorCoverage("cov-1", "00001")))
			s.searches["ServiceRequest"] = row.answer
			obs := &observed{}
			g := prefetchGateway(s)
			g.cfg.Observer = obs.observe
			g.cfg.Clock = fixedClock
			recs, status, msg := g.originCRDRecords(context.Background(), "crd-order-select", prefetchMember)
			if status != row.status || !strings.Contains(msg, row.msg) {
				t.Fatalf("origin history status=%d msg=%s, want %d %q", status, msg, row.status, row.msg)
			}
			if status == 0 {
				if value, exists := recs.history["serviceHistory"]; exists {
					t.Fatalf("unavailable history invented: %s", value)
				}
			}
			observationFlush(t, g)
			if ev, ok := obs.prefetch(t)["serviceHistory"]; !ok || ev.Outcome != row.outcome {
				t.Fatalf("source finding=%+v; want %s", ev, row.outcome)
			}
		})
	}
}

func TestPrefetch_OriginatedRenamedPatient(t *testing.T) {
	s := namedDifferently(t)
	obs := &observed{}
	g := prefetchGateway(s)
	g.cfg.Observer = obs.observe
	recs, status, msg := g.originCRDRecords(context.Background(), "crd-order-select", prefetchMember)
	if status != 0 {
		t.Fatalf("origin source: %d %s", status, msg)
	}
	if recs.sorID != "pat-elsewhere" || !bytes.Contains(recs.patient, []byte(`"id":"example"`)) || bytes.Contains(recs.coverage, []byte("Patient/pat-elsewhere")) || !bytes.Contains(recs.coverage, []byte("Patient/example")) {
		t.Fatalf("source identity edit missing: patient=%s coverage=%s", recs.patient, recs.coverage)
	}
	if len(recs.history) != 0 {
		t.Fatalf("history searched under a renamed source patient: %+v", recs.history)
	}
	if searched, read := s.calls(); len(searched) != 1 || searched[0] != "Coverage?patient=Patient/pat-elsewhere" || !slices.Equal(read, []string{"Patient/pat-elsewhere"}) {
		t.Fatalf("source calls searched=%v read=%v", searched, read)
	}
	observationFlush(t, g)
	for _, key := range []string{"serviceHistory", "deviceHistory", "medicationHistory", "questionnaireResponses"} {
		if ev := obs.prefetch(t)[key]; ev.Outcome != SearchNotRun || ev.Reason != historyNamedDifferently {
			t.Fatalf("%s omission not accounted: %+v", key, ev)
		}
	}
}

func TestPrefetch_OriginatedCoverageIncludedPayor(t *testing.T) {
	s := newPrefetchSoR()
	cov := `{"resourceType":"Coverage","id":"cov-9","status":"active","beneficiary":{"reference":"Patient/example"},"payor":[{"reference":"Organization/pay-9"}]}`
	org := `{"resourceType":"Organization","id":"pay-9","identifier":[{"system":"` + shnsdk.CMSPayerIdentity.System + `","value":"00001"}]}`
	pg := page("", "", sorEntry(cov), `{"fullUrl":"https://sor.example/fhir/Organization/pay-9","resource":`+org+`,"search":{"mode":"include"}}`)
	s.answer(t, "Coverage", pg)
	recs, status, msg := prefetchGateway(s).originCRDRecords(context.Background(), "crd-order-select", prefetchMember)
	if status != 0 {
		t.Fatalf("source Coverage: %d %s", status, msg)
	}
	if got, want := string(recs.coverage), sorAssembly(t, "Coverage", pg); got != want || !strings.Contains(got, org) || strings.Contains(got, "sor.example") {
		t.Fatalf("included payer changed or leaked source URL: got %s want %s", got, want)
	}
	if _, read := s.calls(); !slices.Equal(read, []string{"Patient/example"}) {
		t.Fatalf("included payer was fetched again: %v", read)
	}
}

// A native producer may supply absolute Patient references on its declared
// FHIR server. Strict patient-consistency checking distinguishes that valid
// control from references on a different server; none carriage is covered
// separately above and does not run this optional content rule.
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
		})
	}
}
