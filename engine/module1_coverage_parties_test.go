//go:build !shn_coreonly

package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A dependent's Coverage names the parent, its subscriber, by a reference to a
// Patient record of its own. The PAS request module 1 builds carries the
// participant's own record of the parent as a Patient entry of its own, read
// from its system at the PAS build, the leg that consumes it. A parent the
// system does not hold is refused there as the fidelity of SHN's own build;
// a read the system fails to answer is a backend failure.

const dependentParentID = "pat-dependent-parent"

// dependentParentRecord is the participant's own record of the parent.
func dependentParentRecord() []byte {
	return []byte(`{"resourceType":"Patient","id":"` + dependentParentID + `","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"MBR-UC04-PARENT"}],"name":[{"family":"Chen","given":["Lin"]}],"birthDate":"1955-02-14","gender":"female"}`)
}

// parentHeld is how the system answers a read of the parent's record.
type parentHeld int

const (
	parentFound parentHeld = iota
	parentNotHeld
	parentReadFails
	// parentAnotherRecord answers the read with a Patient other than the
	// one asked for: an invalid response.
	parentAnotherRecord
)

// dependentCoverageSoR is a system of record whose member Coverage names the
// parent as its subscriber by Patient/<id>, the relationship child, and which
// answers a read of the parent as held says.
type dependentCoverageSoR struct {
	refReadFails
	held parentHeld
	// subscriber is the reference the Coverage's subscriber slot makes, and
	// the read held answers: the parent's, unless a row names another.
	subscriber string
}

func newDependentCoverageSoR(inner SystemOfRecord, held parentHeld) dependentCoverageSoR {
	return dependentCoverageSoR{refReadFails: refReadFails{SystemOfRecord: inner}, held: held, subscriber: "Patient/" + dependentParentID}
}

func (s dependentCoverageSoR) dependent(raw []byte) []byte {
	var coverage map[string]any
	if err := json.Unmarshal(raw, &coverage); err != nil {
		return raw
	}
	coverage["subscriber"] = map[string]any{"reference": s.subscriber}
	coverage["relationship"] = map[string]any{"coding": []any{map[string]any{
		"system": "http://terminology.hl7.org/CodeSystem/subscriber-relationship", "code": "child"}}}
	out, err := json.Marshal(coverage)
	if err != nil {
		return raw
	}
	return out
}

func (s dependentCoverageSoR) OpenCoverage(member string) ([]byte, bool) {
	raw, found := s.refReadFails.SystemOfRecord.OpenCoverage(member)
	if !found {
		return raw, found
	}
	return s.dependent(raw), found
}

func (s dependentCoverageSoR) OpenCoverageContext(ctx context.Context, member string) ([][]byte, error) {
	covs, err := ReadSystemOfRecord(s.refReadFails.SystemOfRecord).OpenCoverageContext(ctx, member)
	for i := range covs {
		covs[i] = s.dependent(covs[i])
	}
	return covs, err
}

func (s dependentCoverageSoR) SearchPatientContext(ctx context.Context, resourceType, id string, dates ...SearchDateRange) (SearchResult, error) {
	res, err := s.refReadFails.SearchPatientContext(ctx, resourceType, id, dates...)
	if resourceType != "Coverage" {
		return res, err
	}
	return coverageSearchRewritten(res, err, s.dependent)
}

func (s dependentCoverageSoR) ResolveByReferenceContext(ctx context.Context, ref string) ([]byte, bool, error) {
	if ref != literalPatientKey(s.subscriber) {
		return s.refReadFails.ResolveByReferenceContext(ctx, ref)
	}
	switch s.held {
	case parentNotHeld:
		return nil, false, nil
	case parentReadFails:
		return nil, false, &SoRReadError{Kind: SoRUnavailable}
	case parentAnotherRecord:
		return []byte(`{"resourceType":"Patient","id":"someone-else"}`), true, nil
	}
	return dependentParentRecord(), true, nil
}

func (s dependentCoverageSoR) ResolveByReference(ref string) ([]byte, bool) {
	raw, found, _ := s.ResolveByReferenceContext(context.Background(), ref)
	return raw, found
}

// parentRidesAsItsOwnEntry fails unless the PAS request bundle carries the
// parent's record as a Patient entry the Coverage names as its party
// (shnsdk.CoverageEntryParties), with the record's identity.
func parentRidesAsItsOwnEntry(t *testing.T, leg string, bundle []byte) {
	t.Helper()
	var b map[string]any
	if err := json.Unmarshal(bundle, &b); err != nil {
		t.Fatalf("%s request: %v", leg, err)
	}
	parties := shnsdk.CoverageEntryParties(b)
	entries, _ := b["entry"].([]any)
	for _, e := range entries {
		entry, _ := e.(map[string]any)
		r, _ := entry["resource"].(map[string]any)
		full, _ := entry["fullUrl"].(string)
		if r["resourceType"] == "Patient" && r["id"] == dependentParentID {
			if !parties[full] {
				t.Fatalf("%s request carries the parent at %s, but no Coverage names it as its party: %v", leg, full, parties)
			}
			return
		}
	}
	t.Fatalf("%s request carries no Patient entry for the parent", leg)
}

func TestOrigination_DependentParentRidesThePASRequest(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			g, stub := uc04RouteFixture(t, "2.0")
			mountOriginationRoutes(g)
			g.cfg.ConformanceEnforcement = level
			body := testRouteBody(g, "/scenario/uc04", "")
			g.cfg.SoR = newDependentCoverageSoR(g.cfg.SoR, parentFound)
			rec, _ := serveRecorded(t, g, "POST /scenario/uc04", body)
			if rec.Code != http.StatusOK {
				t.Fatalf("POST /scenario/uc04 = %d %s, want 200", rec.Code, rec.Body)
			}
			for _, leg := range []string{"pas-claim", "pas-claim-update"} {
				sent := stub.requests[leg]
				if len(sent) == 0 {
					t.Fatalf("no %s leg sent (legs %v)", leg, stub.legTypes)
				}
				parentRidesAsItsOwnEntry(t, leg, sent[len(sent)-1])
			}
		})
	}
}

// REJECTION, at every level: a parent the system does not hold is refused at
// the PAS claim's build, 422, the fidelity of SHN's own build, after the CRD
// and questionnaire legs, which stand; a read the system fails to answer is a
// backend failure there. No PAS claim is sent either way.
func TestOrigination_DependentParentUnavailableAtThePASBuild(t *testing.T) {
	for _, row := range []struct {
		name   string
		held   parentHeld
		status int
		check  func(*testing.T, ExchangeRecord)
	}{
		{"not held", parentNotHeld, http.StatusUnprocessableEntity, func(t *testing.T, r ExchangeRecord) {
			wantRefusal(t, r, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalFidelity)
			if r.RefusalStage != vocab.RefusalStageBuild {
				t.Fatalf("stage %q, want %s", r.RefusalStage, vocab.RefusalStageBuild)
			}
		}},
		{"read fails", parentReadFails, http.StatusServiceUnavailable, upstreamError},
	} {
		for _, level := range allLevels {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				g, stub := uc04RouteFixture(t, "2.0")
				mountOriginationRoutes(g)
				g.cfg.ConformanceEnforcement = level
				body := testRouteBody(g, "/scenario/uc04", "")
				g.cfg.SoR = newDependentCoverageSoR(g.cfg.SoR, row.held)
				rec, recs := serveRecorded(t, g, "POST /scenario/uc04", body)
				if rec.Code != row.status {
					t.Fatalf("POST /scenario/uc04 = %d %s, want %d", rec.Code, rec.Body, row.status)
				}
				if legAttempted(stub.legTypes, "pas-claim") || !legAttempted(stub.legTypes, "dtr-questionnaire-fetch") {
					t.Fatalf("legs %v: want the questionnaire fetched and no PAS claim", stub.legTypes)
				}
				wantCarried(t, rec.Body.String(), stub.legTypes...)
				row.check(t, onlyRecord(t, recs))
			})
		}
	}
}

// The provider-data lane completes the PAS request from the participant's own
// system (completeAuthoredPASRequest): the dependent's parent, an entry the
// Coverage names as its party, is accepted there and carried.
func TestProviderData_DependentParentRidesTheCompletedPASRequest(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			const member = "MBR-PD-UC04"
			uc04 := DemoOrderCodes().UC04
			order := buildAuthoredQROrderWithID(t, "sr-uc04-dependent", "Patient/"+member, uc04.System, uc04.Code, uc04.Display, uc04.Dx)
			g, stub, sent := newAuthoredQRSingleShotFixture(t, member, Demo{BirthDate: "1982-11-03", FamilyName: "Chen-ProviderData"}, order)
			mountOriginationRoutes(g)
			g.cfg.ConformanceEnforcement = level
			g.cfg.SoR = newDependentCoverageSoR(g.cfg.SoR, parentFound)
			rec, _ := serveRecorded(t, g, "POST /scenario/uc04", testOrderBody(order, `"member":"`+member+`"`))
			if rec.Code != http.StatusOK || !legAttempted(stub.legTypes, "pas-claim") {
				t.Fatalf("POST /scenario/uc04 = %d %s legs %v, want 200 with the PAS claim sent", rec.Code, rec.Body, stub.legTypes)
			}
			claims := sent.get("pas-claim")
			if len(claims) == 0 {
				t.Fatal("no PAS claim captured")
			}
			parentRidesAsItsOwnEntry(t, "pas-claim", claims[len(claims)-1])
		})
	}
}

// The completion accepts a second Patient entry exactly when the member's
// Coverage names it as its party (shnsdk.CoverageEntryParties), and refuses
// every other second Patient: one nothing names, one another element names
// too, and two members.
func TestCompletedPASRequest_SecondPatientOnlyAsTheCoverageParty(t *testing.T) {
	const base = "https://example.test/fhir/"
	entry := func(full, resource string) string {
		return `{"fullUrl":"` + base + full + `","resource":` + resource + `}`
	}
	member := entry("Patient/p", `{"resourceType":"Patient","id":"p"}`)
	parent := entry("Patient/parent", `{"resourceType":"Patient","id":"parent","identifier":[{"system":"urn:oid:1.2.3.4.5","value":"MRN-9"}]}`)
	coverage := func(subscriber, extra string) string {
		return entry("Coverage/cov", `{"resourceType":"Coverage","id":"cov","status":"active",`+subscriber+`"beneficiary":{"reference":"Patient/p"},"payor":[{"display":"Payer"}]`+extra+`}`)
	}
	bundle := func(entries ...string) []byte {
		return []byte(`{"resourceType":"Bundle","type":"collection","entry":[` + strings.Join(entries, ",") + `]}`)
	}
	read := func(context.Context, string) ([]byte, bool, error) { return nil, false, nil }
	named := `"subscriber":{"reference":"Patient/parent"},`
	for _, row := range []struct {
		name   string
		body   []byte
		accept bool
	}{
		{"the Coverage's subscriber, an entry of its own", bundle(member, coverage(named, ""), parent), true},
		{"the parent before the member", bundle(parent, coverage(named, ""), member), true},
		{"the member their own subscriber, no second Patient", bundle(member, coverage(`"subscriber":{"reference":"Patient/p"},`, "")), true},
		{"a second Patient nothing names", bundle(member, coverage("", ""), parent), false},
		{"the parent named by an extension too", bundle(member, coverage(named, `,"extension":[{"url":"http://example.org/x","valueReference":{"reference":"Patient/parent"}}]`), parent), false},
		{"a second Patient the Coverage does not name", bundle(member, coverage(named, ""), parent, entry("Patient/other", `{"resourceType":"Patient","id":"other"}`)), false},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, err := retainPASRequestEvidence(context.Background(), row.body, read)
			if row.accept && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !row.accept && err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// The PAS tail (module1_pas_tail.go, submitPASClaim), which the provider-data
// single-shot, the order dispatch and UC-03's oxygen arm build through,
// answers a parent the system cannot supply as the order-select route does:
// 422 fidelity at stage build, or a backend failure, with no claim sent.
func TestProviderData_DependentParentUnavailableAtThePASBuild(t *testing.T) {
	for _, row := range []struct {
		name   string
		held   parentHeld
		status int
		check  func(*testing.T, ExchangeRecord)
	}{
		{"not held", parentNotHeld, http.StatusUnprocessableEntity, func(t *testing.T, r ExchangeRecord) {
			wantRefusal(t, r, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalFidelity)
			if r.RefusalStage != vocab.RefusalStageBuild {
				t.Fatalf("stage %q, want %s", r.RefusalStage, vocab.RefusalStageBuild)
			}
		}},
		{"read fails", parentReadFails, http.StatusServiceUnavailable, upstreamError},
	} {
		for _, level := range allLevels {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				const member = "MBR-PD-UC04"
				uc04 := DemoOrderCodes().UC04
				order := buildAuthoredQROrderWithID(t, "sr-uc04-dependent", "Patient/"+member, uc04.System, uc04.Code, uc04.Display, uc04.Dx)
				g, stub, _ := newAuthoredQRSingleShotFixture(t, member, Demo{BirthDate: "1982-11-03", FamilyName: "Chen-ProviderData"}, order)
				mountOriginationRoutes(g)
				g.cfg.ConformanceEnforcement = level
				g.cfg.SoR = newDependentCoverageSoR(g.cfg.SoR, row.held)
				rec, recs := serveRecorded(t, g, "POST /scenario/uc04", testOrderBody(order, `"member":"`+member+`"`))
				if rec.Code != row.status {
					t.Fatalf("POST /scenario/uc04 = %d %s, want %d", rec.Code, rec.Body, row.status)
				}
				if legAttempted(stub.legTypes, "pas-claim") || !legAttempted(stub.legTypes, "dtr-questionnaire-fetch") {
					t.Fatalf("legs %v: want the questionnaire fetched and no PAS claim", stub.legTypes)
				}
				wantCarried(t, rec.Body.String(), "crd-order-select", "dtr-questionnaire-fetch")
				row.check(t, onlyRecord(t, recs))
			})
		}
	}
}

// A two-phase resume amends under the same Coverage, so its amendment carries
// the parent the pended submission read.
func TestResume_AmendmentCarriesTheDependentParent(t *testing.T) {
	gw, stub := uc06Fixture(t)
	gw.cfg.SoR = newDependentCoverageSoR(gw.cfg.SoR, parentFound)
	st := pendFor(t, gw, "uc06", "MBR-UC06")
	if len(st.coverageParties) != 1 {
		t.Fatalf("the pend kept %d parent records, want 1", len(st.coverageParties))
	}
	rec := httptest.NewRecorder()
	mw, mr := markedPost(rec, gw, "/scenario/uc06/complete", "")
	if !gw.completeClinician(mw, mr, st, "42") {
		t.Fatalf("completeClinician = %d %s", rec.Code, rec.Body)
	}
	for _, leg := range []string{"pas-claim", "pas-claim-update"} {
		sent := stub.requests[leg]
		if len(sent) == 0 {
			t.Fatalf("no %s leg sent (legs %v)", leg, stub.legTypes)
		}
		parentRidesAsItsOwnEntry(t, leg, sent[len(sent)-1])
	}
}

// literalPatientKey is the SDK PAS builder's rule for a Coverage slot's
// Patient (shn-sdk patientKey), so the gateway reads exactly the record the
// builder will look for.
func TestLiteralPatientKey(t *testing.T) {
	for ref, want := range map[string]string{
		"Patient/p":                         "Patient/p",
		"https://h.example/fhir/Patient/p":  "Patient/p",
		"Patient/p/_history/3":              "Patient/p",
		"https://h.example/fhir/Patient/p/": "Patient/p",
		"Patient/p/x":                       "Patient/p",
		"#parent":                           "",
		"urn:uuid:5b1f1c3e-0000-4000-8000-000000000001": "",
		"RelatedPerson/r": "",
		"XPatient/p":      "",
		"Patient/":        "",
		"":                "",
	} {
		if got := literalPatientKey(ref); got != want {
			t.Errorf("literalPatientKey(%q) = %q, want %q", ref, got, want)
		}
	}
}

// coveragePartyRecords reads each person the Coverage names beside a literal
// beneficiary once, and nothing else: no read for the member as their own
// subscriber, for a beneficiary that names no literal Patient, or for an id no
// record can carry; a record that is not the Patient asked for is an invalid
// response, a backend failure.
func TestCoveragePartyRecords(t *testing.T) {
	parent := dependentParentRecord()
	for _, row := range []struct {
		name     string
		coverage string
		answer   []byte
		reads    []string
		records  int
		status   int
	}{
		{"subscriber and policyHolder name the parent", `{"beneficiary":{"reference":"Patient/m"},"subscriber":{"reference":"Patient/` + dependentParentID + `"},"policyHolder":{"reference":"https://h.example/fhir/Patient/` + dependentParentID + `/_history/2"}}`, parent, []string{"Patient/" + dependentParentID}, 1, 0},
		{"the member their own subscriber", `{"beneficiary":{"reference":"Patient/m"},"subscriber":{"reference":"https://h.example/fhir/Patient/m"}}`, parent, nil, 0, 0},
		{"a contained parent", `{"beneficiary":{"reference":"Patient/m"},"subscriber":{"reference":"#parent"}}`, parent, nil, 0, 0},
		{"a beneficiary naming no literal Patient", `{"beneficiary":{"identifier":{"value":"m"}},"subscriber":{"reference":"Patient/` + dependentParentID + `"}}`, parent, nil, 0, 0},
		{"a slot spelled in another case", `{"beneficiary":{"reference":"Patient/m"},"Subscriber":{"reference":"Patient/` + dependentParentID + `"},"policyHolder":{"Reference":"Patient/` + dependentParentID + `"}}`, parent, nil, 0, 0},
		{"an id no record can carry", `{"beneficiary":{"reference":"Patient/m"},"subscriber":{"reference":"Patient/a b"}}`, parent, nil, 0, 0},
		{"another record answered", `{"beneficiary":{"reference":"Patient/m"},"subscriber":{"reference":"Patient/` + dependentParentID + `"}}`, []byte(`{"resourceType":"Patient","id":"someone-else"}`), []string{"Patient/" + dependentParentID}, 0, http.StatusBadGateway},
	} {
		t.Run(row.name, func(t *testing.T) {
			var reads []string
			g := &Gateway{cfg: Config{SoR: refAnswer{answer: func(ref string) ([]byte, bool, error) {
				reads = append(reads, ref)
				return row.answer, true, nil
			}}}}
			records, refusal := g.coveragePartyRecords(context.Background(), []byte(row.coverage))
			if strings.Join(reads, ",") != strings.Join(row.reads, ",") {
				t.Fatalf("reads %v, want %v", reads, row.reads)
			}
			if len(records) != row.records {
				t.Fatalf("%d records, want %d", len(records), row.records)
			}
			switch {
			case row.status == 0 && refusal != nil:
				t.Fatalf("refused %+v", refusal)
			case row.status != 0 && (refusal == nil || refusal.status != row.status || refusal.readErr == nil):
				t.Fatalf("refusal %+v, want %d with the read's error", refusal, row.status)
			}
		})
	}
}

// refAnswer is a system of record that answers a read by reference with
// answer; any other read is not expected of it (the embedded nil system).
type refAnswer struct {
	SystemOfRecord
	answer func(ref string) ([]byte, bool, error)
}

func (s refAnswer) ResolveByReference(ref string) ([]byte, bool) {
	b, ok, _ := s.answer(ref)
	return b, ok
}
func (s refAnswer) ResolveByReferenceContext(_ context.Context, ref string) ([]byte, bool, error) {
	return s.answer(ref)
}

// Every module 1 site that builds a PAS submission or amendment (legSiteRows)
// carries the dependent's parent: with the parent held, each builds and sends
// its leg. A site that dropped the parent would not: the SDK builder refuses a
// Coverage naming a person whose record it was not handed
// (shnsdk.ErrPASCoveragePartyNotCarried).
func TestEveryPASSite_CarriesTheDependentParent(t *testing.T) {
	var sites int
	for _, row := range legSiteRows {
		// The resume sites' drivers expect their leg refused; they have rows
		// of their own below (TestResume_*).
		if row.leg != "pas-claim" && row.leg != "pas-claim-update" || strings.HasPrefix(row.site, "module1_originate_resume.go:") {
			continue
		}
		sites++
		t.Run(row.site+" "+row.leg, func(t *testing.T) {
			rec := httptest.NewRecorder()
			var answered *answeredLegs
			row.run(t, rec, func(gw *Gateway) *legFault {
				gw.cfg.SoR = newDependentCoverageSoR(gw.cfg.SoR, parentFound)
				answered = recordAnsweredLegs(gw)
				return failLeg(gw, "no-such-leg")
			})
			if !legAttempted(answered.list(), row.leg) {
				t.Fatalf("the %s leg was not sent with the parent held (legs %v): %d %s", row.leg, answered.list(), rec.Code, rec.Body)
			}
		})
	}
	if sites < 8 {
		t.Fatalf("%d PAS sites, want at least 8", sites)
	}
}

// UC-07's resume amends after the patient's own questionnaire exchange; its
// amendment carries the parent the pended submission read.
func TestResume_PatientAmendmentCarriesTheDependentParent(t *testing.T) {
	gw, stub := newPendResumeFixture(t, pendFixtureOpts{member: "MBR-UC07", birthDate: "1990-08-25", familyName: "Haddad",
		pendedItem: "patient-reported-functional-status", extraRoles: map[string]string{"phg": "phg"}})
	gw.cfg.SoR = newDependentCoverageSoR(gw.cfg.SoR, parentFound)
	st := pendFor(t, gw, "uc07", "MBR-UC07")
	rec := httptest.NewRecorder()
	mw, mr := markedPost(rec, gw, "/scenario/uc07/complete", "")
	if !gw.completePatient(mw, mr, st, "42") {
		t.Fatalf("completePatient = %d %s", rec.Code, rec.Body)
	}
	for _, leg := range []string{"pas-claim", "pas-claim-update"} {
		sent := stub.requests[leg]
		if len(sent) == 0 {
			t.Fatalf("no %s leg sent (legs %v)", leg, stub.legTypes)
		}
		parentRidesAsItsOwnEntry(t, leg, sent[len(sent)-1])
	}
}
