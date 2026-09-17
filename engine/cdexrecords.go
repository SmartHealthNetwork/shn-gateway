package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// cdexRecordsFullURL prefixes the position-based fullUrl of each entry in a
// facility's records Bundle. The facility's own server URLs are not sent.
const cdexRecordsFullURL = "urn:shn:fedquery:"

// heldRecord is one record as the system of record returned it: the bytes
// [start, end) of src (a search page, shared by that page's records, or a
// record a connector returned on its own).
type heldRecord struct {
	src        relay.Body
	b          []byte
	start, end int
}

func (r heldRecord) raw() []byte { return r.b[r.start:r.end] }

// cdexRecordsFaultHook, when set by a test, changes the assembled records
// before they are sealed, to prove the embeds are verified.
var cdexRecordsFaultHook func([]byte)

// facilityRecordsBundle builds the facility's answer to a data request for
// member:
//   - every record of each queried type the system of record holds whose
//     clinical date falls within the query's dates (all matches, in the order
//     the system returned them), each copied byte for byte as a verified
//     embed of the registered cdex-records assembly;
//   - the member's identity binding: a Patient this gateway authors with only
//     the system of record's Patient id and the member identifier (the
//     system's Patient record itself is never sent);
//   - one Provenance per record, authored by this gateway, citing consentRef.
//
// The search is narrowed to the query's dates where the type has a date
// search parameter (dateSearchPaths); the gateway still selects each record
// by its own date. Records are read through the system's search
// (SearchSystemOfRecord), or, for a connector that cannot search, through
// FacilityRecordsContext.
//
// Before anything is sent, the member must have a Patient in the system of
// record, and every record must be about that patient (the Patient
// compartment fence); a record about anyone else, or a record returned twice,
// is a 502. More records than one search may return is a 422. No record in
// range is a 404.
func (g *Gateway) facilityRecordsBundle(ctx context.Context, member string, queries []shnsdk.CDexQuery, consentRef string) (records []byte, sealed relay.Payload, status int, msg string) {
	fail := func(status int, msg string) ([]byte, relay.Payload, int, string) {
		return nil, sealed, status, msg
	}
	sorFail := func(err error) ([]byte, relay.Payload, int, string) {
		var bound *recordsBoundError
		if errors.As(err, &bound) {
			return fail(http.StatusUnprocessableEntity, "records exceed the per-answer bound for "+bound.resourceType)
		}
		status, msg := SoRFailureResponse(err)
		return fail(status, msg)
	}
	sor := ReadSystemOfRecord(g.cfg.SoR)
	ref, found, err := sor.PatientFHIRRefContext(ctx, member)
	if err != nil {
		return sorFail(err)
	}
	if !found {
		return fail(http.StatusNotFound, "the facility's system of record holds no Patient for the member")
	}
	sorID, ok := strings.CutPrefix(ref, "Patient/")
	if !ok || !fhirIDRE.MatchString(sorID) {
		return sorFail(&SoRReadError{Kind: SoRInvalidResponse})
	}
	fence := newPatientFence(shnsdk.MemberSystem, member, nil, sorID, member)
	anotherPatient := func() ([]byte, relay.Payload, int, string) {
		return fail(http.StatusBadGateway, "system of record returned another patient's resource")
	}
	// The member's own Patient record must exist and be this member; only
	// the identity binding below is sent.
	patient, hasPatient, err := sor.ResolveByReferenceContext(ctx, ref)
	if err != nil {
		return sorFail(err)
	}
	if !hasPatient {
		return fail(http.StatusBadGateway, "the facility's system of record did not return the member's Patient")
	}
	if fence.check(patient) != nil {
		return anotherPatient()
	}
	identity, err := json.Marshal(map[string]any{
		"resourceType": "Patient",
		"id":           sorID,
		"identifier":   []map[string]string{{"system": shnsdk.MemberSystem, "value": member}},
	})
	if err != nil {
		return fail(http.StatusInternalServerError, "build records bundle failed")
	}

	reader := &facilityRecordReader{g: g, sorID: sorID, member: member}
	var held []heldRecord
	seen := map[string]bool{}
	for _, q := range queries {
		recs, err := reader.records(ctx, q)
		if err != nil {
			return sorFail(err)
		}
		for _, rec := range recs {
			raw := rec.raw()
			if !q.InRange(recordClinicalDate(raw)) {
				continue // only the named records within the requested dates (FR-24)
			}
			if fence.check(raw) != nil {
				return anotherPatient()
			}
			key, ok := resourceRef(raw)
			if !ok {
				return fail(http.StatusBadGateway, "system of record returned a record without an id")
			}
			if seen[key] {
				return fail(http.StatusBadGateway, "system of record returned the same record twice")
			}
			seen[key] = true
			if status, msg := g.validateFHIR(ctx, raw, "egress", ""); status != 0 {
				return fail(status, msg)
			}
			held = append(held, rec)
		}
	}
	if len(held) == 0 {
		return fail(http.StatusNotFound, "named records not held")
	}
	if status, msg := g.validateFHIR(ctx, identity, "egress", ""); status != 0 {
		return fail(status, msg)
	}

	var provenances [][]byte
	for _, rec := range held {
		target, _ := resourceRef(rec.raw())
		// Source Provenance: targets the disclosed record, agent = this
		// facility, .policy cites the authenticated consent reference, reason
		// = TREAT (FR-32/C11).
		prov, err := buildEvidenceProvenance(target, "http://smarthealth.network/ids/holder", g.cfg.HolderID,
			consentRef, shnsdk.PurposeTreatment, g.cfg.Clock())
		if err == nil {
			// A searchset entry must have an id; the record index keeps it unique.
			prov, err = withID(prov, "record-provenance-"+strconv.Itoa(len(provenances)))
		}
		if err != nil {
			return fail(http.StatusInternalServerError, "build provenance failed")
		}
		if status, msg := g.validateFHIR(ctx, prov, "egress", ""); status != 0 {
			return fail(status, msg)
		}
		provenances = append(provenances, prov)
	}

	// The Bundle has its own id, so the fulfillment that contains it needs
	// no edit unless the request Task already uses that id.
	size := len(identity)
	for _, rec := range held {
		size += rec.end - rec.start
	}
	for _, prov := range provenances {
		size += len(prov)
	}
	const entryOverhead = 96 // fullUrl, resource and search members of one entry
	b := make([]byte, 0, size+(len(held)+len(provenances)+1)*entryOverhead+64)
	b = append(b, `{"resourceType":"Bundle","id":"results","type":"searchset","entry":[`...)
	embeds := make([]relay.Embed, 0, len(held))
	n := 0
	entry := func(res []byte, mode string) int {
		if n > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"fullUrl":"`+cdexRecordsFullURL+strconv.Itoa(n)+`","resource":`...)
		at := len(b)
		b = append(b, res...)
		b = append(b, `,"search":{"mode":"`+mode+`"}}`...)
		n++
		return at
	}
	for _, rec := range held {
		at := entry(rec.raw(), "match")
		embeds = append(embeds, relay.Embed{Source: rec.src, Start: rec.start, End: rec.end, At: at})
	}
	entry(identity, "include")
	for _, prov := range provenances {
		entry(prov, "include")
	}
	b = append(b, "]}"...)
	if cdexRecordsFaultHook != nil {
		cdexRecordsFaultHook(b)
	}
	if sealed, err = relay.Authored(relay.BuilderCDexRecords, b, fhirJSON, embeds...); err != nil {
		return fail(http.StatusInternalServerError, "build records bundle failed")
	}
	return b, sealed, 0, ""
}

// recordsBoundError: a record search exceeded a search bound.
type recordsBoundError struct{ resourceType string }

func (e *recordsBoundError) Error() string {
	return "records exceed the per-answer bound for " + e.resourceType
}

// facilityRecordReader reads one member's records of each type, preferring
// the system's search and falling back, once, to FacilityRecordsContext.
type facilityRecordReader struct {
	g             *Gateway
	sorID, member string
	legacy        map[string][]byte
	legacyRead    bool
}

func (r *facilityRecordReader) records(ctx context.Context, q shnsdk.CDexQuery) ([]heldRecord, error) {
	var dates []SearchDateRange
	if _, ok := dateSearchPaths[q.ResourceType]; ok {
		// One extra day on each side: a server compares a date bound with a
		// record's offset dateTime as instants, which could drop a record on
		// an edge day that the gateway's own selection (by the record's date)
		// keeps. The gateway's selection below stays the authority.
		dates = append(dates, SearchDateRange{Param: "date", From: shiftDate(q.Start, -1), To: shiftDate(q.End, 1)})
	}
	s := runSoRSearch(ctx, r.g.cfg.SoR, q.ResourceType, r.sorID, false, dates...)
	switch s.Outcome {
	case SearchOK:
		bodies := make([]relay.Body, len(s.pages))
		out := make([]heldRecord, 0, len(s.matches))
		for _, m := range s.matches {
			if bodies[m.page].Len() == 0 {
				bodies[m.page] = relay.NewBody(s.pages[m.page], relay.OriginUpstreamResponse)
			}
			out = append(out, heldRecord{src: bodies[m.page], b: s.pages[m.page], start: m.start, end: m.end})
		}
		return out, nil
	case SearchZero:
		return nil, nil
	case SearchUnsupported:
		if !r.legacyRead {
			held, _, err := ReadSystemOfRecord(r.g.cfg.SoR).FacilityRecordsContext(ctx, r.member)
			if err != nil {
				return nil, err
			}
			r.legacy, r.legacyRead = held, true
		}
		raw, ok := r.legacy[q.ResourceType]
		if !ok {
			return nil, nil
		}
		return []heldRecord{{src: relay.NewBody(raw, relay.OriginUpstreamResponse), b: raw, start: 0, end: len(raw)}}, nil
	case SearchUnavailable:
		return nil, &SoRReadError{Kind: SoRUnavailable}
	case SearchBound:
		return nil, &recordsBoundError{resourceType: q.ResourceType}
	}
	return nil, &SoRReadError{Kind: SoRInvalidResponse}
}

// shiftDate moves a FHIR date (YYYY-MM-DD) by days; any other value is
// returned unchanged.
func shiftDate(d string, days int) string {
	t, err := time.Parse("2006-01-02", d)
	if err != nil {
		return d
	}
	return t.AddDate(0, 0, days).Format("2006-01-02")
}

// cdexRecordsBundle returns the records Bundle a completed data-request Task
// carries: the contained Bundle its last data-query output references (the
// same one shnsdk.ExtractCDexEvidence reads), as the bytes the Task holds.
func cdexRecordsBundle(taskJSON []byte) ([]byte, error) {
	var task struct {
		Contained []json.RawMessage `json:"contained"`
		Output    []struct {
			Type struct {
				Coding []struct {
					System string `json:"system"`
					Code   string `json:"code"`
				} `json:"coding"`
			} `json:"type"`
			ValueReference *struct {
				Reference string `json:"reference"`
			} `json:"valueReference"`
		} `json:"output"`
	}
	if err := decodeMessage(taskJSON, &task); err != nil {
		return nil, fmt.Errorf("records answer is not a readable Task: %w", err)
	}
	ref := ""
	for _, o := range task.Output {
		for _, c := range o.Type.Coding {
			if c.System == "http://hl7.org/fhir/us/davinci-hrex/CodeSystem/hrex-temp" && c.Code == "data-query" && o.ValueReference != nil {
				ref = o.ValueReference.Reference
			}
		}
	}
	id, local := strings.CutPrefix(ref, "#")
	if !local || id == "" {
		return nil, errors.New("records answer has no contained data-query output")
	}
	var found []byte
	for _, c := range task.Contained {
		var head struct {
			ResourceType string `json:"resourceType"`
			ID           string `json:"id"`
		}
		if json.Unmarshal(c, &head) != nil || head.ID != id {
			continue
		}
		if found != nil || head.ResourceType != "Bundle" {
			return nil, errors.New("records answer's data-query output is not one contained Bundle")
		}
		found = c
	}
	if found == nil {
		return nil, errors.New("records answer's data-query output is not contained")
	}
	return found, nil
}

// withID returns the gateway's own resource with its id set to id.
func withID(resource []byte, id string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(resource, &m); err != nil || m == nil {
		return nil, errors.New("not a resource")
	}
	raw, err := json.Marshal(id)
	if err != nil {
		return nil, err
	}
	m["id"] = raw
	return json.Marshal(m)
}

// cdexEvidence picks the requester's supplemental evidence from a facility's
// records Bundle: the DiagnosticReport with the latest clinical date
// (recordClinicalDate, compared as instants by fhirInstant; a report without a
// readable date comes before any dated one, and of equal instants the later
// entry wins),
// and the Provenance that attributes it (targets it by Type/id). Both are the
// bytes the Bundle holds.
func cdexEvidence(bundle []byte) (report, provenance []byte, err error) {
	var b struct {
		Entry []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := decodeMessage(bundle, &b); err != nil {
		return nil, nil, fmt.Errorf("records Bundle is not readable: %w", err)
	}
	type head struct {
		ResourceType string `json:"resourceType"`
		ID           string `json:"id"`
		Target       []struct {
			Reference string `json:"reference"`
		} `json:"target"`
	}
	heads := make([]head, len(b.Entry))
	reportRef := ""
	var reportAt time.Time
	reportDated := false
	for i, e := range b.Entry {
		if json.Unmarshal(e.Resource, &heads[i]) != nil {
			return nil, nil, errors.New("records Bundle entry is not readable")
		}
		if heads[i].ResourceType == "DiagnosticReport" && heads[i].ID != "" {
			at, dated := fhirInstant(recordClinicalDate(e.Resource))
			if report == nil || (dated && (!reportDated || !at.Before(reportAt))) || (!dated && !reportDated) {
				report, reportRef, reportAt, reportDated = e.Resource, "DiagnosticReport/"+heads[i].ID, at, dated
			}
		}
	}
	if report == nil {
		return nil, nil, errors.New("records Bundle holds no report")
	}
	for i, e := range b.Entry {
		if heads[i].ResourceType != "Provenance" {
			continue
		}
		for _, t := range heads[i].Target {
			if t.Reference == reportRef {
				provenance = e.Resource
			}
		}
	}
	if provenance == nil {
		return nil, nil, errors.New("records Bundle has no Provenance for its report")
	}
	return report, provenance, nil
}

// fhirInstant reads a FHIR date or dateTime as an instant: a year, a
// year-month or a date is the start of that period in UTC; a dateTime
// carries its own offset. ok is false for anything else.
func fhirInstant(s string) (time.Time, bool) {
	for _, layout := range []string{"2006", "2006-01", "2006-01-02", time.RFC3339Nano} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// repointEvidenceSubject returns a copy of a facility's report whose subject
// names claimPatient.
//
// This is a change the requester makes to its own message, disclosed to
// partners: a prior-authorization update is the requester's own message, and
// its supplemental report must be about the Claim's patient, as the payer
// reads it. The facility's report is checked to be about that patient first
// (requesterRecordsFence). Only the value of the report's subject is
// replaced, by {"reference":<claimPatient>}; every other byte of the
// facility's report is kept, and the result is checked to be exactly that.
// The copy is never presented as the facility's bytes, which reach the
// requester unchanged in the facility's answer.
func repointEvidenceSubject(report []byte, claimPatient string) ([]byte, error) {
	doc, err := relay.Doc(relay.NewBody(report, relay.OriginPeerFrame))
	if err != nil || doc.Kind(doc.Root()) != relay.KindObject {
		return nil, errors.New("supplemental report is not a resource")
	}
	subject, ok := doc.Member(doc.Root(), "subject")
	if !ok || doc.Kind(subject) != relay.KindObject {
		return nil, errors.New("supplemental report has no subject")
	}
	value, err := json.Marshal(struct {
		Reference string `json:"reference"`
	}{claimPatient})
	if err != nil {
		return nil, err
	}
	start, end := doc.Span(subject)
	out := make([]byte, 0, len(report)-(end-start)+len(value))
	out = append(append(append(out, report[:start]...), value...), report[end:]...)
	// The copy is the report with that one value replaced.
	check, err := relay.Doc(relay.NewBody(out, relay.OriginPeerFrame))
	if err != nil {
		return nil, fmt.Errorf("repointed report: %w", err)
	}
	got, ok := check.Member(check.Root(), "subject")
	if !ok {
		return nil, errors.New("repointed report has no subject")
	}
	gs, ge := check.Span(got)
	if gs != start || !bytes.Equal(out[gs:ge], value) || !bytes.Equal(out[:gs], report[:start]) || !bytes.Equal(out[ge:], report[end:]) {
		return nil, errors.New("repointed report differs from the facility's beyond its subject")
	}
	return out, nil
}
