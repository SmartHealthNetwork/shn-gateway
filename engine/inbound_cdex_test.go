package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// cdexAnswerTask is a payer's data request Task with a layout, an unknown
// member and an existing output the fulfillment must keep.
const cdexAnswerTask = `{"resourceType":"Task","id":"cdex-req-1",
  "status":"requested", "intent":"order",
  "code":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-hrex/CodeSystem/hrex-temp","code":"data-request"}]},
  "for":{"reference":"Patient/MBR-UC05"},
  "input":[{"type":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-hrex/CodeSystem/hrex-temp","code":"data-query"}]},"valueString":"DiagnosticReport?patient=MBR-UC05&category=LAB"}],
  "x-unknown":{"score":1.50}}`

// cdexAnswerRecords is a facility records Bundle carrying number lexemes a
// decode and re-encode would change.
const cdexAnswerRecords = `{"resourceType":"Bundle","type":"searchset","entry":[{"fullUrl":"urn:shn:fedquery:0","resource":` +
	`{"resourceType":"Observation","id":"obs-1","status":"final","code":{"text":"x"},"valueQuantity":{"value":1.50},` +
	`"extension":[{"url":"http://example.org/fhir/StructureDefinition/relay-fidelity-unknown","extension":[` +
	`{"url":"a","valueDecimal":9007199254740993},{"url":"b","valueDecimal":1e2}]}]}}]}`

// TestFederatedQueryAnswer_AuthoredFulfillmentWithVerifiedEmbeds: the
// facility's answer is the registered cdex-fulfillment, the only answer the
// ownership table admits on that transmit; its bytes are the SDK's exact
// fulfillment (the payer's Task with the declared edits, the records
// embedded as sent); and every span it copies is declared and verified, so a
// copy that differs from its source is refused when sealed.
func TestFederatedQueryAnswer_AuthoredFulfillmentWithVerifiedEmbeds(t *testing.T) {
	task, records := []byte(cdexAnswerTask), []byte(cdexAnswerRecords)
	answer, fulfilled, err := sealCDexFulfillment(task, records)
	if err != nil {
		t.Fatal(err)
	}
	if answer.Ownership() != relay.OwnershipAuthored || answer.Builder() != relay.BuilderCDexFulfillment {
		t.Fatalf("answer %v, want authored by the CDex fulfillment builder", answer)
	}
	want, err := shnsdk.BuildCDexQueryResult(task, records)
	if err != nil {
		t.Fatal(err)
	}
	got := relay.BytesForTest(answer)
	if !bytes.Equal(got, want) || !bytes.Equal(fulfilled, want) {
		t.Fatal("the sealed answer is not the SDK's fulfillment")
	}
	for _, lexeme := range []string{`"value":1.50`, `9007199254740993`, `1e2`, `"x-unknown":{"score":1.50}`, `"intent":"order"`} {
		if !bytes.Contains(got, []byte(lexeme)) {
			t.Errorf("the answer lost %s", lexeme)
		}
	}

	key := relay.Key{Leg: "federated-query", Role: relay.RoleRecipient, Direction: relay.DirectionResponse, Outcome: relay.OutcomeAnswered}
	if _, err := relay.Transmit(answer, relay.Check(key)); err != nil {
		t.Fatalf("the table refused the fulfillment: %v", err)
	}
	if rule := relay.LegOwnership()[key]; !slices.Equal(rule.Builders, []relay.BuilderID{relay.BuilderCDexFulfillment}) {
		t.Fatalf("the federated-query answer admits %v, want only the CDex fulfillment", rule.Builders)
	}
	rebuilt, err := relay.Authored(relay.BuilderSDKFederatedQuery, want, "application/fhir+json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := relay.Transmit(rebuilt, relay.Check(key)); !errors.Is(err, relay.ErrOwnershipRefused) {
		t.Fatalf("an answer from another builder must be refused, got %v", err)
	}

	f, err := shnsdk.BuildCDexFulfillment(task, records)
	if err != nil {
		t.Fatal(err)
	}
	var fromTask, fromRecords int
	for _, c := range f.Copied {
		if c.FromRecords {
			fromRecords++
		} else {
			fromTask++
		}
	}
	if fromTask == 0 || fromRecords == 0 {
		t.Fatalf("the fulfillment declares %d task and %d record copies; both must be declared", fromTask, fromRecords)
	}
	t.Run("a copy that differs from its source is refused", func(t *testing.T) {
		src := relay.NewBody(records, relay.OriginUpstreamResponse)
		for _, c := range f.Copied {
			if !c.FromRecords {
				continue
			}
			tampered := bytes.Clone(f.Task)
			i := c.At + (c.End-c.Start)/2
			tampered[i] ^= 0x01
			_, err := relay.Authored(relay.BuilderCDexFulfillment, tampered, "application/fhir+json",
				relay.Embed{Source: src, Start: c.Start, End: c.End, At: c.At})
			if !errors.Is(err, relay.ErrEmbedMismatch) {
				t.Fatalf("a tampered copy at %d was sealed: %v", i, err)
			}
			return
		}
	})
	t.Run("a signed request Task is refused", func(t *testing.T) {
		signed := bytes.Replace(task, []byte(`"intent":"order",`), []byte(`"intent":"order","extension":[{"url":"http://example.org/sig","valueSignature":`+
			`{"type":[{"system":"urn:iso-astm:E1762-95:2013","code":"1.2.840.10065.1.12.1.1"}],"when":"2026-06-19T21:53:54+00:00","who":{"reference":"Organization/payer"}}}],`), 1)
		_, _, err := sealCDexFulfillment(signed, records)
		assertCDexRefused(t, err, shnsdk.ErrCDexSignedContent)
	})
	t.Run("a request Task in a status that does not allow fulfillment is refused", func(t *testing.T) {
		done := bytes.Replace(task, []byte(`"status":"requested"`), []byte(`"status":"completed"`), 1)
		_, _, err := sealCDexFulfillment(done, records)
		assertCDexRefused(t, err, shnsdk.ErrCDexTaskStatus)
	})
	t.Run("a request that is not one well-formed Task is refused", func(t *testing.T) {
		for name, bad := range map[string]string{
			"duplicate member": `{"resourceType":"Task","status":"requested","status":"completed"}`,
			"not a Task":       `{"resourceType":"Bundle","type":"collection"}`,
			"output not array": `{"resourceType":"Task","status":"requested","output":{}}`,
		} {
			_, _, err := sealCDexFulfillment([]byte(bad), records)
			var refused *cdexRequestRefused
			if !errors.As(err, &refused) {
				t.Errorf("%s: want a request refusal, got %v", name, err)
			}
		}
	})
	t.Run("a fault in the facility's own records stays a build fault", func(t *testing.T) {
		_, _, err := sealCDexFulfillment(task, []byte(`{"resourceType":"Bundle","id":7}`))
		var refused *cdexRequestRefused
		if err == nil || errors.As(err, &refused) {
			t.Fatalf("want a build fault, got %v", err)
		}
	})
}

// assertCDexRefused checks err is a request refusal (answered 422) wrapping
// sentinel, whose reason does not echo the request.
func assertCDexRefused(t *testing.T, err, sentinel error) {
	t.Helper()
	var refused *cdexRequestRefused
	if !errors.As(err, &refused) || !errors.Is(err, sentinel) {
		t.Fatalf("want a request refusal wrapping %v, got %v", sentinel, err)
	}
	if strings.Contains(refused.reason, "{") || !strings.HasPrefix(refused.reason, "federated query refused: ") {
		t.Fatalf("refusal reason %q", refused.reason)
	}
}

// facilitySoR is a facility's system of record: patient fac-77 (member
// MBR-UC05) and a searchable record store whose pages are served byte for
// byte.
type facilitySoR struct {
	*censusSoR
	ContextSystemOfRecord
	patient   []byte
	pages     map[string][][]byte // resource type → pages
	searched  []string
	noSearch  bool
	searchErr error
	legacy    map[string][]byte
}

func (s *facilitySoR) PatientFHIRRefContext(_ context.Context, member string) (string, bool, error) {
	if member != "MBR-UC05" {
		return "", false, nil
	}
	return "Patient/fac-77", true, nil
}

func (s *facilitySoR) ResolveByReferenceContext(_ context.Context, ref string) ([]byte, bool, error) {
	if ref == "Patient/fac-77" && s.patient != nil {
		return s.patient, true, nil
	}
	return nil, false, nil
}

func (s *facilitySoR) FacilityRecordsContext(context.Context, string) (map[string][]byte, bool, error) {
	return s.legacy, len(s.legacy) > 0, nil
}

func (s *facilitySoR) SearchPatientContext(_ context.Context, rt, id string, dates ...SearchDateRange) (SearchResult, error) {
	if s.noSearch {
		return SearchResult{}, &SearchError{Outcome: SearchUnsupported, Reason: "no search"}
	}
	if s.searchErr != nil {
		return SearchResult{}, s.searchErr
	}
	q, err := SoRSearchQuery(rt, id, dates...)
	if err != nil {
		return SearchResult{}, err
	}
	s.searched = append(s.searched, q)
	var res SearchResult
	for i, p := range s.pages[rt] {
		parsed, err := ParseSearchPage(p, rt)
		if err != nil {
			return SearchResult{}, err
		}
		for _, e := range parsed.Entries {
			e.Page = i
			res.Entries = append(res.Entries, e)
		}
		res.Pages = append(res.Pages, p)
	}
	if len(res.Pages) == 0 {
		res.Pages = [][]byte{[]byte(`{"resourceType":"Bundle","type":"searchset"}`)}
	}
	res.Total = len(res.Entries)
	return res, nil
}

// Facility records as the system of record holds them: pretty-printed, with
// markup characters and number lexemes a re-encode would change, and the
// facility's own patient id as subject.
var (
	facPatient = []byte("{\n  \"resourceType\": \"Patient\",\n  \"id\": \"fac-77\",\n  \"identifier\": [ { \"system\": \"urn:shn:member\", \"value\": \"MBR-UC05\" }, { \"system\": \"urn:mrn\", \"value\": \"MRN-SECRET-4411\" } ],\n  \"name\": [ { \"family\": \"Hiddenfamily\" } ],\n  \"birthDate\": \"1961-02-03\"\n}")
	// facIdentity is the identity binding the facility gateway authors.
	facIdentity = `{"id":"fac-77","identifier":[{"system":"urn:shn:member","value":"MBR-UC05"}],"resourceType":"Patient"}`
	facDR1      = "{\n    \"resourceType\": \"DiagnosticReport\",\n    \"id\": \"dr-1\",\n    \"status\": \"final\",\n    \"code\": { \"text\": \"MRI <lumbar> & spine\" },\n    \"subject\": { \"reference\": \"Patient/fac-77\" },\n    \"effectiveDateTime\": \"2025-03-01\",\n    \"extension\": [ { \"url\": \"http://example.org/x\", \"valueDecimal\": 1.50 }, { \"url\": \"http://example.org/y\", \"valueDecimal\": 9007199254740993 }, { \"url\": \"http://example.org/z\", \"valueDecimal\": 1e2 } ]\n  }"
	facDR2      = "{\"resourceType\":\"DiagnosticReport\",\"id\":\"dr-2\",\"status\":\"final\",\"code\":{\"text\":\"x-ray\"},\"subject\":{\"reference\":\"Patient/fac-77\"},\"effectiveDateTime\":\"2025-06-01T10:00:00Z\"}"
	facDROld    = "{\"resourceType\":\"DiagnosticReport\",\"id\":\"dr-old\",\"status\":\"final\",\"code\":{\"text\":\"old\"},\"subject\":{\"reference\":\"Patient/fac-77\"},\"effectiveDateTime\":\"2019-01-01\"}"
	facDOC      = "{\"resourceType\":\"DocumentReference\",\"id\":\"doc-1\",\"status\":\"current\",\"subject\":{\"reference\":\"Patient/fac-77\"},\"date\":\"2025-04-01T00:00:00Z\",\"content\":[{\"attachment\":{\"contentType\":\"text/plain\",\"data\":\"PD4m\"}}]}"
	facDROther  = "{\"resourceType\":\"DiagnosticReport\",\"id\":\"dr-x\",\"status\":\"final\",\"code\":{\"text\":\"x\"},\"subject\":{\"reference\":\"Patient/someone-else\"},\"effectiveDateTime\":\"2025-03-01\"}"
)

func facPage(next string, resources ...string) []byte {
	b := "{\n  \"resourceType\" : \"Bundle\",\n  \"type\" : \"searchset\",\n  \"total\" : 99"
	if next != "" {
		b += ",\n  \"link\" : [ { \"relation\" : \"next\", \"url\" : \"" + next + "\" } ]"
	}
	b += ",\n  \"entry\" : [ "
	for i, r := range resources {
		if i > 0 {
			b += ", "
		}
		b += "{ \"fullUrl\" : \"http://sor.internal/fhir/facility/x/" + strconv.Itoa(i) + "\", \"resource\" : " + r + ", \"search\" : { \"mode\" : \"match\" } }"
	}
	return []byte(b + " ]\n}")
}

func newFacilityGateway(sor *facilitySoR) *Gateway {
	ok := certificationValidatorFunc(func(context.Context, []byte, string) (shnsdk.Result, error) { return shnsdk.Result{Valid: true}, nil })
	return &Gateway{cfg: Config{SoR: sor, HolderID: "facility-1", Validator: ok, Clock: func() time.Time { return time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC) }}}
}

func facilityQueries(types ...string) []shnsdk.CDexQuery {
	var qs []shnsdk.CDexQuery
	for _, rt := range types {
		qs = append(qs, shnsdk.CDexQuery{ResourceType: rt, PatientRef: "Patient/MBR-UC05", Start: "2024-01-01", End: "2026-09-17"})
	}
	return qs
}

// TestFederatedQuery_RecordsKeepSystemOfRecordBytes: the facility's records
// reach the requester exactly as its system of record holds them: every
// record the query names within its dates is carried (all matches, across
// pages), each as a verified copy of
// the server's bytes, with the facility's Patient (carrying the member
// identifier) and one gateway Provenance per record; nothing is re-encoded,
// and the payer's Task then embeds that Bundle exactly.
func TestFederatedQuery_RecordsKeepSystemOfRecordBytes(t *testing.T) {
	sor := &facilitySoR{censusSoR: newCensusSoR(), patient: facPatient, pages: map[string][][]byte{
		"DiagnosticReport":  {facPage("http://sor.internal/fhir/facility?page=2", facDR1, facDROld), facPage("", facDR2)},
		"DocumentReference": {facPage("", facDOC)},
	}}
	sor.ContextSystemOfRecord = ReadSystemOfRecord(sor.censusSoR)
	g := newFacilityGateway(sor)
	records, payload, status, msg := g.facilityRecordsBundle(context.Background(), "MBR-UC05", facilityQueries("DiagnosticReport", "DocumentReference"), "Consent/c-1")
	if status != 0 {
		t.Fatalf("status %d: %s", status, msg)
	}
	if payload.Ownership() != relay.OwnershipAuthored || payload.Builder() != relay.BuilderCDexRecords || !bytes.Equal(relay.BytesForTest(payload), records) {
		t.Fatalf("payload = %v", payload)
	}
	// Each search is narrowed to the requested dates, widened by a day on
	// each side, by the type's date search parameter.
	if want := []string{
		"DiagnosticReport?date=ge2023-12-31&date=le2026-09-18&patient=Patient%2Ffac-77",
		"DocumentReference?date=ge2023-12-31&date=le2026-09-18&patient=Patient%2Ffac-77",
	}; !slices.Equal(sor.searched, want) {
		t.Fatalf("searches = %v, want %v", sor.searched, want)
	}
	for _, want := range []string{facDR1, facDR2, facDOC, facIdentity} {
		if n := bytes.Count(records, []byte(want)); n != 1 {
			t.Errorf("records carry %d exact copies of %.40q, want 1", n, want)
		}
	}
	// Only the identity binding crosses: none of the facility's Patient
	// record does (minimum necessary).
	for _, private := range []string{"Hiddenfamily", "1961-02-03", "MRN-SECRET-4411", "urn:mrn"} {
		if bytes.Contains(records, []byte(private)) {
			t.Errorf("the facility's Patient record leaked %q", private)
		}
	}
	if bytes.Contains(records, []byte(`"dr-old"`)) {
		t.Error("a record outside the requested dates was carried")
	}
	if bytes.Contains(records, []byte("sor.internal")) {
		t.Error("the system of record's own URLs reached the answer")
	}
	for _, lexeme := range []string{"1.50", "9007199254740993", "1e2", "<lumbar> & spine"} {
		if !bytes.Contains(records, []byte(lexeme)) {
			t.Errorf("records lost %q", lexeme)
		}
	}
	// One Provenance per record, in record order, each attributing its record.
	var parsed struct {
		Entry []struct {
			Resource struct {
				ResourceType string `json:"resourceType"`
				ID           string `json:"id"`
				Target       []struct {
					Reference string `json:"reference"`
				} `json:"target"`
			} `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(records, &parsed); err != nil {
		t.Fatal(err)
	}
	var order, targets []string
	for _, e := range parsed.Entry {
		order = append(order, e.Resource.ResourceType)
		if e.Resource.ResourceType == "Provenance" {
			if len(e.Resource.Target) != 1 {
				t.Fatalf("provenance targets %v", e.Resource.Target)
			}
			targets = append(targets, e.Resource.Target[0].Reference)
		}
	}
	if want := []string{"DiagnosticReport", "DiagnosticReport", "DocumentReference", "Patient", "Provenance", "Provenance", "Provenance"}; !slices.Equal(order, want) {
		t.Fatalf("entries = %v, want %v", order, want)
	}
	if want := []string{"DiagnosticReport/dr-1", "DiagnosticReport/dr-2", "DocumentReference/doc-1"}; !slices.Equal(targets, want) {
		t.Fatalf("provenance targets = %v", targets)
	}
	// A searchset's entries must each have an id (FHIR validation refuses a
	// search result without one), and the ids must be distinct.
	ids := map[string]bool{}
	for _, e := range parsed.Entry {
		key := e.Resource.ResourceType + "/" + e.Resource.ID
		if e.Resource.ID == "" || ids[key] {
			t.Fatalf("entry %s has no unique id", key)
		}
		ids[key] = true
	}
	// The requester's fence accepts the answer; its evidence is the last
	// report and the Provenance that attributes it.
	if err := requesterRecordsFence("MBR-UC05").check(records); err != nil {
		t.Fatalf("requester fence: %v", err)
	}
	answer, fulfilled, err := sealCDexFulfillment([]byte(cdexAnswerTask), records)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(relay.BytesForTest(answer), records) || !bytes.Contains(fulfilled, []byte(facDR1)) {
		t.Fatal("the fulfillment does not embed the records exactly")
	}
	carried, err := cdexRecordsBundle(fulfilled)
	if err != nil || !bytes.Equal(carried, records) {
		t.Fatalf("records read back = %v", err)
	}
	dr, prov, err := cdexEvidence(carried)
	if err != nil {
		t.Fatal(err)
	}
	if string(dr) != facDR2 || !bytes.Contains(prov, []byte(`"DiagnosticReport/dr-2"`)) {
		t.Fatalf("evidence = %s / %s", dr, prov)
	}
}

func TestCDexEvidence_LatestReportByDate(t *testing.T) {
	report := func(id, dates string) string {
		return `{"resourceType":"DiagnosticReport","id":"` + id + `",` + dates + `}`
	}
	prov := func(id string) string {
		return `{"resourceType":"Provenance","target":[{"reference":"DiagnosticReport/` + id + `"}]}`
	}
	for name, tc := range map[string]struct {
		reports []string
		want    string
	}{
		"latest effectiveDateTime first": {[]string{report("a", `"effectiveDateTime":"2025-06-01"`), report("b", `"effectiveDateTime":"2025-01-01"`)}, "a"},
		"period end":                     {[]string{report("a", `"effectiveDateTime":"2025-06-01"`), report("b", `"effectivePeriod":{"start":"2025-01-01","end":"2025-07-01"}`)}, "b"},
		"period start only":              {[]string{report("a", `"effectivePeriod":{"start":"2025-08-01"}`), report("b", `"effectiveDateTime":"2025-07-01"`)}, "a"},
		"undated comes first":            {[]string{report("a", `"status":"final"`), report("b", `"effectiveDateTime":"2020-01-01"`), report("c", `"status":"final"`)}, "b"},
		"equal dates, later entry":       {[]string{report("a", `"effectiveDateTime":"2025-06-01"`), report("b", `"effectiveDateTime":"2025-06-01"`)}, "b"},
		// Instants, not text: 08:00-05:00 is 13:00Z, after 10:00Z.
		"offsets compared as instants": {[]string{report("b", `"effectiveDateTime":"2025-06-01T08:00:00-05:00"`), report("a", `"effectiveDateTime":"2025-06-01T10:00:00Z"`)}, "b"},
		// A date alone is the start of its day, UTC: 23:00-05:00 on May 31
		// is 04:00Z on June 1, after "2025-06-01".
		"date is start of day UTC":   {[]string{report("b", `"effectiveDateTime":"2025-05-31T23:00:00-05:00"`), report("a", `"effectiveDateTime":"2025-06-01"`)}, "b"},
		"year and month precision":   {[]string{report("a", `"effectiveDateTime":"2025-07"`), report("b", `"effectiveDateTime":"2025"`)}, "a"},
		"fractional seconds":         {[]string{report("b", `"effectiveDateTime":"2025-06-01T10:00:00.5Z"`), report("a", `"effectiveDateTime":"2025-06-01T10:00:00Z"`)}, "b"},
		"unreadable date is undated": {[]string{report("a", `"effectiveDateTime":"2025-13-99"`), report("b", `"effectiveDateTime":"2001-01-01"`)}, "b"},
	} {
		t.Run(name, func(t *testing.T) {
			entries := append([]string{}, tc.reports...)
			for _, id := range []string{"a", "b", "c"} {
				entries = append(entries, prov(id))
			}
			b := []byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + strings.Join(entries, `},{"resource":`) + `}]}`)
			dr, p, err := cdexEvidence(b)
			if err != nil || !bytes.Contains(dr, []byte(`"id":"`+tc.want+`"`)) || !bytes.Contains(p, []byte("DiagnosticReport/"+tc.want)) {
				t.Fatalf("evidence = %s / %s (%v)", dr, p, err)
			}
		})
	}
}

func TestCDexEvidence_ReportAndItsProvenance(t *testing.T) {
	prov := func(target string) string {
		return `{"resourceType":"Provenance","target":[{"reference":"` + target + `"}]}`
	}
	bundle := func(entries ...string) []byte {
		return []byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + strings.Join(entries, `},{"resource":`) + `}]}`)
	}
	if _, _, err := cdexEvidence(bundle(facDR1, prov("DiagnosticReport/dr-x"))); err == nil {
		t.Error("a report without its own Provenance was used")
	}
	if _, _, err := cdexEvidence(bundle(facDOC, prov("DocumentReference/doc-1"))); err == nil {
		t.Error("an answer without a report yielded evidence")
	}
	dr, p, err := cdexEvidence(bundle(facDR2, prov("DiagnosticReport/dr-1"), facDR1, prov("DiagnosticReport/dr-2"), prov("DiagnosticReport/dr-1")))
	if err != nil || string(dr) != facDR2 || !bytes.Contains(p, []byte("dr-2")) {
		t.Fatalf("evidence = %s / %s (%v)", dr, p, err)
	}
}

func TestCDexRecordsBundle_Refusals(t *testing.T) {
	for name, task := range map[string]string{
		"not JSON":          `{`,
		"no output":         `{"resourceType":"Task"}`,
		"external output":   `{"resourceType":"Task","output":[{"type":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-hrex/CodeSystem/hrex-temp","code":"data-query"}]},"valueReference":{"reference":"Bundle/x"}}]}`,
		"missing contained": `{"resourceType":"Task","output":[{"type":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-hrex/CodeSystem/hrex-temp","code":"data-query"}]},"valueReference":{"reference":"#r"}}]}`,
		"not a Bundle":      `{"resourceType":"Task","contained":[{"resourceType":"Basic","id":"r"}],"output":[{"type":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-hrex/CodeSystem/hrex-temp","code":"data-query"}]},"valueReference":{"reference":"#r"}}]}`,
		"two with the id":   `{"resourceType":"Task","contained":[{"resourceType":"Bundle","id":"r"},{"resourceType":"Bundle","id":"r"}],"output":[{"type":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-hrex/CodeSystem/hrex-temp","code":"data-query"}]},"valueReference":{"reference":"#r"}}]}`,
	} {
		if _, err := cdexRecordsBundle([]byte(task)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A connector that cannot search answers with its own record bytes, also
// carried exactly.
func TestFederatedQuery_RecordsFromConnectorWithoutSearch(t *testing.T) {
	sor := &facilitySoR{censusSoR: newCensusSoR(), patient: facPatient, noSearch: true, legacy: map[string][]byte{"DiagnosticReport": []byte(facDR1)}}
	sor.ContextSystemOfRecord = ReadSystemOfRecord(sor.censusSoR)
	records, _, status, msg := newFacilityGateway(sor).facilityRecordsBundle(context.Background(), "MBR-UC05", facilityQueries("DiagnosticReport", "DocumentReference"), "Consent/c-1")
	if status != 0 {
		t.Fatalf("status %d: %s", status, msg)
	}
	if !bytes.Contains(records, []byte(facDR1)) || !bytes.Contains(records, []byte(facIdentity)) || bytes.Contains(records, []byte("Hiddenfamily")) {
		t.Fatalf("records = %s", records)
	}
}

func TestFederatedQuery_RecordsRefusals(t *testing.T) {
	otherPatient := bytes.Replace(facPatient, []byte("MBR-UC05"), []byte("MBR-UC99"), 1)
	cases := map[string]struct {
		sor    *facilitySoR
		status int
	}{
		"another patient's record":  {&facilitySoR{patient: facPatient, pages: map[string][][]byte{"DiagnosticReport": {facPage("", facDR1, facDROther)}}}, http.StatusBadGateway},
		"another patient's Patient": {&facilitySoR{patient: otherPatient, pages: map[string][][]byte{"DiagnosticReport": {facPage("", facDR1)}}}, http.StatusBadGateway},
		"no Patient record":         {&facilitySoR{pages: map[string][][]byte{"DiagnosticReport": {facPage("", facDR1)}}}, http.StatusBadGateway},
		"same record twice":         {&facilitySoR{patient: facPatient, pages: map[string][][]byte{"DiagnosticReport": {facPage("n", facDR1), facPage("", facDR1)}}}, http.StatusBadGateway},
		"over a bound":              {&facilitySoR{patient: facPatient, searchErr: &SearchError{Outcome: SearchBound, Reason: "entry bound"}}, http.StatusUnprocessableEntity},
		"unavailable":               {&facilitySoR{patient: facPatient, searchErr: &SearchError{Outcome: SearchUnavailable, Reason: "time bound"}}, http.StatusServiceUnavailable},
		"nothing in range":          {&facilitySoR{patient: facPatient, pages: map[string][][]byte{"DiagnosticReport": {facPage("", facDROld)}}}, http.StatusNotFound},
		"nothing held":              {&facilitySoR{patient: facPatient}, http.StatusNotFound},
		"malformed page":            {&facilitySoR{patient: facPatient, pages: map[string][][]byte{"DiagnosticReport": {[]byte(`{"resourceType":"Bundle","type":"searchset","type":"x"}`)}}}, http.StatusBadGateway},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tc.sor.censusSoR = newCensusSoR()
			tc.sor.ContextSystemOfRecord = ReadSystemOfRecord(tc.sor.censusSoR)
			records, _, status, msg := newFacilityGateway(tc.sor).facilityRecordsBundle(context.Background(), "MBR-UC05", facilityQueries("DiagnosticReport"), "Consent/c-1")
			if status != tc.status || records != nil {
				t.Fatalf("status = %d (%s), records = %d bytes; want %d", status, msg, len(records), tc.status)
			}
			if want := map[string]string{
				"no Patient record": "the facility's system of record did not return the member's Patient",
				"same record twice": "system of record returned the same record twice",
				"over a bound":      "records exceed the per-answer bound for DiagnosticReport",
			}[name]; want != "" && msg != want {
				t.Fatalf("message = %q, want %q", msg, want)
			}
		})
	}
	t.Run("unknown member", func(t *testing.T) {
		sor := &facilitySoR{censusSoR: newCensusSoR(), patient: facPatient}
		sor.ContextSystemOfRecord = ReadSystemOfRecord(sor.censusSoR)
		_, _, status, msg := newFacilityGateway(sor).facilityRecordsBundle(context.Background(), "MBR-OTHER", facilityQueries("DiagnosticReport"), "c")
		if status != http.StatusNotFound || msg != "the facility's system of record holds no Patient for the member" {
			t.Fatalf("status = %d (%s)", status, msg)
		}
	})
	t.Run("a copy that differs from its page is refused", func(t *testing.T) {
		sor := &facilitySoR{censusSoR: newCensusSoR(), patient: facPatient, pages: map[string][][]byte{"DiagnosticReport": {facPage("", facDR1)}}}
		sor.ContextSystemOfRecord = ReadSystemOfRecord(sor.censusSoR)
		cdexRecordsFaultHook = func(b []byte) {
			i := bytes.Index(b, []byte("<lumbar>"))
			b[i+1] = 'X'
		}
		t.Cleanup(func() { cdexRecordsFaultHook = nil })
		records, _, status, _ := newFacilityGateway(sor).facilityRecordsBundle(context.Background(), "MBR-UC05", facilityQueries("DiagnosticReport"), "c")
		if status != http.StatusInternalServerError || records != nil {
			t.Fatalf("a changed record copy was sealed: status %d", status)
		}
	})
}

// At the search bounds, the facility shares one sealed body per page among
// that page's records: the bytes it allocates stay a small multiple of what
// the system returned, instead of growing with records × page size.
func TestFederatedQuery_RecordsAllocationBounded(t *testing.T) {
	var pages [][]byte
	pad := strings.Repeat("x", SoRSearchMaxBytes/SoRSearchMaxEntries-400)
	for p := 0; p < SoRSearchMaxPages; p++ {
		var recs []string
		for r := 0; r < SoRSearchMaxEntries/SoRSearchMaxPages; r++ {
			recs = append(recs, fmt.Sprintf(`{"resourceType":"DiagnosticReport","id":"dr-%d-%d","status":"final","code":{"text":"%s"},"subject":{"reference":"Patient/fac-77"},"effectiveDateTime":"2025-03-01"}`, p, r, pad))
		}
		next := "n"
		if p == SoRSearchMaxPages-1 {
			next = ""
		}
		pages = append(pages, facPage(next, recs...))
	}
	input := 0
	for _, p := range pages {
		input += len(p)
	}
	if input > SoRSearchMaxBytes {
		t.Fatalf("fixture is %d bytes, over the size bound", input)
	}
	sor := &facilitySoR{censusSoR: newCensusSoR(), patient: facPatient, pages: map[string][][]byte{"DiagnosticReport": pages}}
	sor.ContextSystemOfRecord = ReadSystemOfRecord(sor.censusSoR)
	g := newFacilityGateway(sor)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	records, _, status, msg := g.facilityRecordsBundle(context.Background(), "MBR-UC05", facilityQueries("DiagnosticReport"), "c")
	runtime.ReadMemStats(&after)
	if status != 0 {
		t.Fatalf("status %d: %s", status, msg)
	}
	if n := bytes.Count(records, []byte(`"resourceType":"DiagnosticReport"`)); n != SoRSearchMaxEntries {
		t.Fatalf("records = %d, want %d", n, SoRSearchMaxEntries)
	}
	allocated := int(after.TotalAlloc - before.TotalAlloc)
	// Measured: about 12.5x with one body per page (including the test
	// system's own page parsing); a body per record measures about 35x.
	if limit := 18 * input; allocated > limit {
		t.Fatalf("allocated %d bytes for %d bytes of pages (limit %d)", allocated, input, limit)
	}
}

// The requester fences the facility's records before using them: every
// record must be about the requested member, through the Patient the facility
// carried.
func TestCDexRecords_RequesterFence(t *testing.T) {
	bundle := func(entries ...string) []byte {
		return []byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + strings.Join(entries, `},{"resource":`) + `}]}`)
	}
	prov := `{"resourceType":"Provenance","id":"p","target":[{"reference":"DiagnosticReport/dr-1"}],"recorded":"2026-09-17T00:00:00Z","agent":[{"who":{"identifier":{"system":"http://smarthealth.network/ids/holder","value":"f"}}}]}`
	fence := requesterRecordsFence("MBR-UC05")
	if err := fence.check(bundle(facDR1, facIdentity, prov)); err != nil {
		t.Fatalf("a consistent answer was refused: %v", err)
	}
	for name, b := range map[string][]byte{
		"no carried patient":       bundle(facDR1, prov),
		"record for another":       bundle(facDR1, facDROther, facIdentity, prov),
		"patient of another":       bundle(facDR1, strings.Replace(facIdentity, "MBR-UC05", "MBR-UC99", 1), prov),
		"provenance of nothing":    bundle(facIdentity, prov),
		"two patients with one id": bundle(facDR1, facIdentity, facIdentity, prov),
	} {
		if err := fence.check(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The requester's ClaimUpdate carries the facility's report pointed at the
// Claim's patient: a disclosed change the requester makes to its own message.
// Only the subject's value changes; every other byte of the facility's report
// is kept.
func TestCDexEvidence_RepointedToClaimPatient(t *testing.T) {
	got, err := repointEvidenceSubject([]byte(facDR1), "Patient/MBR-UC05")
	if err != nil {
		t.Fatal(err)
	}
	old := `{ "reference": "Patient/fac-77" }`
	want := strings.Replace(facDR1, old, `{"reference":"Patient/MBR-UC05"}`, 1)
	if string(got) != want {
		t.Fatalf("repointed:\n got %s\nwant %s", got, want)
	}
	for name, report := range map[string]string{
		"not a resource":        `[1]`,
		"no subject":            `{"resourceType":"DiagnosticReport","id":"d"}`,
		"subject not an object": `{"resourceType":"DiagnosticReport","id":"d","subject":"Patient/x"}`,
		"repeated subject":      `{"resourceType":"DiagnosticReport","subject":{"reference":"Patient/a"},"subject":{"reference":"Patient/b"}}`,
		"trailing content":      `{"resourceType":"DiagnosticReport","subject":{"reference":"Patient/a"}} {}`,
	} {
		if _, err := repointEvidenceSubject([]byte(report), "Patient/MBR-UC05"); err == nil {
			t.Errorf("%s: repointed", name)
		}
	}
	t.Run("a reference that needs escaping", func(t *testing.T) {
		got, err := repointEvidenceSubject([]byte(facDR2), "Patient/a\"b")
		if err != nil {
			t.Fatal(err)
		}
		var dr struct {
			Subject struct{ Reference string } `json:"subject"`
		}
		if err := json.Unmarshal(got, &dr); err != nil || dr.Subject.Reference != "Patient/a\"b" {
			t.Fatalf("repointed = %s (%v)", got, err)
		}
	})
}

func TestShiftDate(t *testing.T) {
	for in, want := range map[[2]string]string{
		{"2024-01-01", "-1"}: "2023-12-31",
		{"2024-02-28", "1"}:  "2024-02-29",
		{"2026-12-31", "1"}:  "2027-01-01",
		{"", "1"}:            "",
		{"2024-01", "1"}:     "2024-01",
	} {
		days, _ := strconv.Atoi(in[1])
		if got := shiftDate(in[0], days); got != want {
			t.Errorf("shiftDate(%q, %d) = %q, want %q", in[0], days, got, want)
		}
	}
}

// A record on the first requested day, written with an offset that puts its
// instant on the day before, is still carried: the server search is widened
// and the gateway selects by the record's own date.
func TestFederatedQuery_EdgeDayRecordKept(t *testing.T) {
	edge := strings.Replace(facDR2, `"2025-06-01T10:00:00Z"`, `"2024-01-01T00:30:00+05:00"`, 1)
	early := strings.Replace(strings.Replace(facDR2, `"dr-2"`, `"dr-early"`, 1), `"2025-06-01T10:00:00Z"`, `"2023-12-31T23:30:00-05:00"`, 1)
	sor := &facilitySoR{censusSoR: newCensusSoR(), patient: facPatient, pages: map[string][][]byte{"DiagnosticReport": {facPage("", edge, early)}}}
	sor.ContextSystemOfRecord = ReadSystemOfRecord(sor.censusSoR)
	records, _, status, msg := newFacilityGateway(sor).facilityRecordsBundle(context.Background(), "MBR-UC05", facilityQueries("DiagnosticReport"), "c")
	if status != 0 {
		t.Fatalf("status %d: %s", status, msg)
	}
	if !bytes.Contains(records, []byte(edge)) || bytes.Contains(records, []byte("dr-early")) {
		t.Fatalf("records = %s", records)
	}
}
