//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The participant's system may name the member's Patient by its own record id
// rather than by the member id. The gateway re-keys that one record to the
// member id at the participant boundary (namePatientByMember). A Coverage on
// which the member is their own subscriber names the member in its subscriber
// (and policyHolder) as well as its beneficiary; the re-key names the member
// in each, so the Coverage still says the member is their own subscriber: the
// CRD prefetch carries no participant-local id, and the PAS inquiry, which
// carries the re-keyed Coverage as the participant's own bytes, names no one
// beside the member. (TestOriginatedCRD_UsesSoRPatientAndCoverage and
// TestNamePatientByMember hold the CRD and the re-key itself.)

// localPatientID is the system's own record id for the member in these rows.
const localPatientID = "pat-local-4471"

// localIDSoR is a system of record that names member's Patient by
// localPatientID: its Patient record has that id, and its Coverages name it as
// beneficiary, subscriber and policyHolder (the member their own subscriber).
// Every other read is the inner system's, by the member id.
type localIDSoR struct {
	refReadFails
	member string
	// subscriber, when set, is the reference the Coverage's subscriber and
	// policyHolder make instead of the member's local record (a dependent's
	// parent), whose record the system holds (dependentParentRecord).
	subscriber string
}

func newLocalIDSoR(inner SystemOfRecord, member string) localIDSoR {
	return localIDSoR{refReadFails: refReadFails{SystemOfRecord: inner}, member: member}
}

func (s localIDSoR) id(id string) string {
	if id == localPatientID {
		return s.member
	}
	return id
}

// localized names the member's local record wherever an inner record names
// the member's Patient by the member id.
func (s localIDSoR) localized(raw []byte) []byte {
	return bytes.ReplaceAll(raw, []byte(`"Patient/`+s.member+`"`), []byte(`"Patient/`+localPatientID+`"`))
}

func (s localIDSoR) SupplementalReportContext(ctx context.Context, member string) ([]byte, bool, error) {
	raw, found, err := s.refReadFails.SupplementalReportContext(ctx, member)
	return s.localized(raw), found, err
}

func (s localIDSoR) OpenOrder(member string) ([]byte, bool) {
	raw, found, _ := s.OpenOrderContext(context.Background(), member)
	return raw, found
}

func (s localIDSoR) OpenOrderContext(ctx context.Context, member string) ([]byte, bool, error) {
	raw, found, err := s.refReadFails.OpenOrderContext(ctx, member)
	return s.localized(raw), found, err
}

// selfSubscribed names the member's local record in a Coverage's beneficiary,
// subscriber and policyHolder.
func (s localIDSoR) selfSubscribed(raw []byte) []byte {
	var coverage map[string]any
	if err := json.Unmarshal(raw, &coverage); err != nil {
		return raw
	}
	coverage["beneficiary"] = map[string]any{"reference": "Patient/" + localPatientID}
	party := "Patient/" + localPatientID
	if s.subscriber != "" {
		party = s.subscriber
	}
	for _, slot := range []string{"subscriber", "policyHolder"} {
		coverage[slot] = map[string]any{"reference": party}
	}
	out, err := json.Marshal(coverage)
	if err != nil {
		return raw
	}
	return out
}

func (s localIDSoR) PatientFHIRRef(member string) (string, bool) {
	ref, found, _ := s.PatientFHIRRefContext(context.Background(), member)
	return ref, found
}

func (s localIDSoR) PatientFHIRRefContext(ctx context.Context, member string) (string, bool, error) {
	if member == s.member {
		return "Patient/" + localPatientID, true, nil
	}
	return s.refReadFails.PatientFHIRRefContext(ctx, member)
}

func (s localIDSoR) ResolveByReference(ref string) ([]byte, bool) {
	raw, found, _ := s.ResolveByReferenceContext(context.Background(), ref)
	return raw, found
}

func (s localIDSoR) ResolveByReferenceContext(ctx context.Context, ref string) ([]byte, bool, error) {
	if s.subscriber != "" && ref == s.subscriber {
		return dependentParentRecord(), true, nil
	}
	if ref != "Patient/"+localPatientID {
		raw, found, err := s.refReadFails.ResolveByReferenceContext(ctx, ref)
		return s.localized(raw), found, err
	}
	raw, found, err := s.refReadFails.ResolveByReferenceContext(ctx, "Patient/"+s.member)
	if err != nil || !found {
		return raw, found, err
	}
	var patient map[string]any
	if err := json.Unmarshal(raw, &patient); err != nil {
		return raw, found, nil
	}
	patient["id"] = localPatientID
	out, err := json.Marshal(patient)
	return out, true, err
}

func (s localIDSoR) OpenCoverage(member string) ([]byte, bool) {
	raw, found := s.refReadFails.SystemOfRecord.OpenCoverage(member)
	if !found || member != s.member {
		return raw, found
	}
	return s.selfSubscribed(raw), found
}

func (s localIDSoR) OpenCoverageContext(ctx context.Context, member string) ([][]byte, error) {
	covs, err := s.refReadFails.OpenCoverageContext(ctx, member)
	if member == s.member {
		for i := range covs {
			covs[i] = s.selfSubscribed(covs[i])
		}
	}
	return covs, err
}

func (s localIDSoR) SearchPatientContext(ctx context.Context, resourceType, id string, dates ...SearchDateRange) (SearchResult, error) {
	res, err := s.refReadFails.SearchPatientContext(ctx, resourceType, s.id(id), dates...)
	if err != nil || id != localPatientID {
		return res, err
	}
	if resourceType == "Coverage" {
		return coverageSearchRewritten(res, err, s.selfSubscribed)
	}
	out := SearchResult{}
	for i, page := range res.Pages {
		b := s.localized(page)
		parsed, err := ParseSearchPage(b, resourceType)
		if err != nil {
			return res, err
		}
		for _, e := range parsed.Entries {
			e.Page = i
			out.Entries = append(out.Entries, e)
		}
		out.Pages = append(out.Pages, b)
	}
	out.Total = len(out.Entries)
	return out, nil
}

// carriesTheMemberOnce fails unless a PAS request carries one Patient entry,
// the member's, under the member id, and never the system's own record id.
func carriesTheMemberOnce(t *testing.T, leg string, bundle []byte) {
	t.Helper()
	if bytes.Contains(bundle, []byte(localPatientID)) {
		t.Fatalf("the %s request carries the system's own record id %q: %s", leg, localPatientID, bundle)
	}
	if patients := patientEntries(t, bundle); patients != 1 {
		t.Fatalf("the %s request carries %d Patient entries, want the member's alone", leg, patients)
	}
}

// Unchanged by the slots' re-key, at every level: a member their own
// subscriber, whom the system names by its own record id, gets a submission
// and an amendment carrying the member once, under the member id. The PAS
// builds read the Coverage as the system holds it, whose slots all name the
// one local id, so the builder re-points each to the member's entry.
func TestOrigination_SelfSubscriberUnderALocalIDRidesOnce(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			g, stub := uc04RouteFixture(t, "2.0")
			mountOriginationRoutes(g)
			g.cfg.ConformanceEnforcement = level
			body := testRouteBody(g, "/scenario/uc04", "")
			g.cfg.SoR = newLocalIDSoR(g.cfg.SoR, "MBR-UC04")
			rec, _ := serveRecorded(t, g, "POST /scenario/uc04", body)
			if rec.Code != http.StatusOK {
				t.Fatalf("POST /scenario/uc04 = %d %s, want 200", rec.Code, rec.Body)
			}
			for _, leg := range []string{"pas-claim", "pas-claim-update"} {
				sent := stub.requests[leg]
				if len(sent) == 0 {
					t.Fatalf("no %s leg sent (legs %v)", leg, stub.legTypes)
				}
				carriesTheMemberOnce(t, leg, sent[len(sent)-1])
			}
		})
	}
}

// The inquiry that continues such a member's pend is built: its Coverage,
// carried as the participant's own bytes once re-keyed, names the member in
// each slot, so it names no one beside the member and no participant-local id.
// Re-keying the beneficiary alone leaves the subscriber on the local id, which
// the inquiry builder reads as a person whose record was not supplied.
func TestPAInquire_SelfSubscriberUnderALocalIDIsInquired(t *testing.T) {
	gw, stub := pasFollowSystem(t, "pended")
	gw.cfg.SoR = newLocalIDSoR(gw.cfg.SoR, pasFollowMember)
	req := httptest.NewRequest(http.MethodPost, "/scenario/uc03", strings.NewReader(testRouteBody(gw, "/scenario/uc03", "")))
	pend, status, msg, err := gw.submitClaimAndFollow(req.Context(), req, pasFollowSubmit(0))
	if status != 0 || err != nil {
		t.Fatalf("submit: %d %q %v", status, msg, err)
	}
	store, _ := gw.continuations()
	cont, _, err := store.ReadContinuation("provider", pend.Continuation)
	if err != nil || cont.SoRPatientID != "Patient/"+localPatientID {
		t.Fatalf("the continuation names the patient %q (%v), want the system's own record id: this row is about the re-key", cont.SoRPatientID, err)
	}
	stub.inquireAnswer = "approved"
	rec := httptest.NewRecorder()
	gw.handlePAInquire(rec, httptest.NewRequest(http.MethodPost, "/scenario/pa/inquire",
		strings.NewReader(`{"continuation":"`+pend.Continuation+`","waitSeconds":0}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("inquire = %d %s, want 200", rec.Code, rec.Body)
	}
	sent := stub.sent["pas-claim-inquire"]
	if len(sent) != 1 {
		t.Fatalf("%d inquiries sent, want 1", len(sent))
	}
	carriesTheMemberOnce(t, "pas-claim-inquire", sent[0])
	var b map[string]any
	if err := json.Unmarshal(sent[0], &b); err != nil {
		t.Fatal(err)
	}
	if parties := shnsdk.CoverageEntryParties(b); len(parties) != 0 {
		t.Fatalf("the inquiry names %v as the Coverage's party, want no one beside the member", parties)
	}
}

// A dependent whose system names them by its own record id: the re-key names
// the member in the beneficiary and leaves the subscriber and policyHolder
// naming the parent, another person. The CRD prefetch Coverage still names the
// parent, and the submission and amendment carry the parent once beside the
// member, never under the member's local id.
func TestOrigination_DependentUnderALocalIDKeepsTheParent(t *testing.T) {
	g, stub := uc04RouteFixture(t, "2.0")
	mountOriginationRoutes(g)
	body := testRouteBody(g, "/scenario/uc04", "")
	sor := newLocalIDSoR(g.cfg.SoR, "MBR-UC04")
	sor.subscriber = "Patient/" + dependentParentID
	g.cfg.SoR = sor
	rec, _ := serveRecorded(t, g, "POST /scenario/uc04", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /scenario/uc04 = %d %s, want 200", rec.Code, rec.Body)
	}
	crd := stub.requests["crd-order-select"]
	if len(crd) == 0 {
		t.Fatalf("no CRD request sent (legs %v)", stub.legTypes)
	}
	if bytes.Contains(crd[0], []byte(localPatientID)) {
		t.Fatalf("the CRD request carries the system's own record id: %s", crd[0])
	}
	if !bytes.Contains(crd[0], []byte(`"subscriber":{"reference":"Patient/`+dependentParentID+`"}`)) {
		t.Fatalf("the CRD prefetch Coverage does not name the parent as subscriber: %s", crd[0])
	}
	for _, leg := range []string{"pas-claim", "pas-claim-update"} {
		sent := stub.requests[leg]
		if len(sent) == 0 {
			t.Fatalf("no %s leg sent (legs %v)", leg, stub.legTypes)
		}
		last := sent[len(sent)-1]
		if bytes.Contains(last, []byte(localPatientID)) {
			t.Fatalf("the %s request carries the system's own record id", leg)
		}
		parentRidesAsItsOwnEntry(t, leg, last)
		if n := patientEntries(t, last); n != 2 {
			t.Fatalf("the %s request carries %d Patient entries, want the member's and the parent's", leg, n)
		}
	}
}

// patientEntries counts the Patient entries a request Bundle carries.
func patientEntries(t *testing.T, bundle []byte) int {
	t.Helper()
	var b struct {
		Entry []struct {
			Resource struct {
				ResourceType string `json:"resourceType"`
			} `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(bundle, &b); err != nil {
		t.Fatalf("decode the request: %v", err)
	}
	n := 0
	for _, e := range b.Entry {
		if e.Resource.ResourceType == "Patient" {
			n++
		}
	}
	return n
}
