//go:build !shn_coreonly

package engine

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// parentMarker is what only the parent's own record carries.
const parentMarker = "PARENT-MRN-77"

// subscribedBy is cov with its subscriber naming Patient/<parent>.
func subscribedBy(cov, parent string) string {
	return strings.Replace(cov, `"status" : "active",`, `"status" : "active", "subscriber" : { "reference" : "Patient/`+parent+`" }, "relationship" : { "coding" : [ { "code" : "child" } ] },`, 1)
}

// patientInclude is an include entry of a Patient on the system of record.
func patientInclude(id, identifier string) string {
	return `{ "fullUrl": "https://sor.example/fhir/Patient/` + id + `", "resource": { "resourceType" : "Patient", "id" : "` + id + `", "identifier" : [ { "system" : "urn:oid:1.2.3.4.5", "value" : "` + identifier + `" } ], "name" : [ { "family" : "Parent" } ] }, "search": { "mode": "include" } }`
}

// noPatientEntry fails when the sent request's coverage prefetch, if any,
// holds a Patient entry, or the request carries the parent's record. The rows
// using it include no Patient a Coverage's beneficiary names.
func noPatientEntry(t *testing.T, sent []byte) {
	t.Helper()
	if strings.Contains(string(sent), parentMarker) {
		t.Fatal("the parent's record was carried")
	}
	var req struct {
		Prefetch map[string]json.RawMessage `json:"prefetch"`
	}
	if err := json.Unmarshal(sent, &req); err != nil {
		t.Fatal(err)
	}
	raw, ok := req.Prefetch["coverage"]
	if !ok {
		return
	}
	var b struct {
		Entry []struct {
			Resource struct {
				ResourceType string `json:"resourceType"`
			} `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	for _, e := range b.Entry {
		if e.Resource.ResourceType == "Patient" {
			t.Fatal("the coverage the gateway filled carries a Patient entry")
		}
	}
}

// A Patient the system of record includes in a Coverage search, such as the
// parent a dependent's Coverage names as its subscriber, is the gateway's own
// read and never carried: it is dropped before the fence and the carry, not
// refused. Each Coverage is still bound by its beneficiary. So it holds at
// every level, with the opt-in and without it, on the CRD routing read and
// coverage fill and on the questionnaire-package routing read.
func TestCoverageSearch_IncludedPatientIsDroppedNotRefused(t *testing.T) {
	t.Parallel()
	crdRows := map[string]func(t *testing.T) *prefetchSoR{
		"the parent the subscriber names": func(t *testing.T) *prefetchSoR {
			s := newPrefetchSoR()
			s.answer(t, "Coverage", page("", "", sorEntry(subscribedBy(sorCoverage("cov-1", "00001"), "parent")), patientInclude("parent", parentMarker)))
			return s
		},
		"an unrelated Patient": func(t *testing.T) *prefetchSoR {
			s := newPrefetchSoR()
			s.answer(t, "Coverage", page("", "", sorEntry(sorCoverage("cov-1", "00001")), patientInclude("stranger", parentMarker)))
			return s
		},
		// The parent only a Coverage that is not chosen names: dropped too.
		"a parent only a dropped Coverage names": func(t *testing.T) *prefetchSoR {
			s := newPrefetchSoR()
			dropped := strings.Replace(subscribedBy(sorCoverage("cov-2", "00001"), "parent"), `"status" : "active"`, `"status" : "cancelled"`, 1)
			s.answer(t, "Coverage", page("", "", sorEntry(sorCoverage("cov-1", "00001")), sorEntry(dropped), patientInclude("parent", parentMarker)))
			return s
		},
	}
	for name, sor := range crdRows {
		for _, enrich := range []bool{false, true} {
			for _, level := range allLevels {
				t.Run("cds-hooks/"+name+"/"+level.String()+optInLabel(enrich), func(t *testing.T) {
					env, rec, _ := levelIngressRowWith(t, sor(t), level, ehrRequest(patientOnly), enrich)
					if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
						t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
					}
					noPatientEntry(t, sentRequest(t, env))
				})
			}
		}
	}
	for _, enrich := range []bool{false, true} {
		for _, level := range allLevels {
			t.Run("questionnaire-package/the parent the subscriber names/"+level.String()+optInLabel(enrich), func(t *testing.T) {
				s := newPrefetchSoR()
				s.answer(t, "Coverage", page("", "", sorEntry(subscribedBy(sorCoverage("cov-1", shnsdk.CMSPayerIdentity.Value), "parent")), patientInclude("parent", parentMarker)))
				env, rec := dtrIngressRowWith(t, s, ehrParams(ehrOrderParam("sr1", prefetchMember), dtrQuestionnaire), level, enrich)
				if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
					t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
				}
				if strings.Contains(string(sentRequest(t, env)), parentMarker) {
					t.Fatal("the parent's record was carried")
				}
			})
		}
	}
}

// Dropping the Patients a search included loosens nothing else: a Coverage
// whose beneficiary is another patient is still refused, at every level and
// on both reads, with the parent's record beside it or not.
func TestCoverageSearch_DroppingIncludedPatientsKeepsTheBeneficiaryBinding(t *testing.T) {
	t.Parallel()
	other := func(cov string) string {
		return strings.ReplaceAll(cov, "Patient/"+prefetchSoRID, "Patient/someone-else")
	}
	for _, enrich := range []bool{false, true} {
		for _, level := range allLevels {
			t.Run("cds-hooks/"+level.String()+optInLabel(enrich), func(t *testing.T) {
				s := newPrefetchSoR()
				s.answer(t, "Coverage", page("", "", sorEntry(other(subscribedBy(sorCoverage("cov-1", "00001"), "parent"))), patientInclude("parent", parentMarker)))
				env, rec, _ := levelIngressRowWith(t, s, level, ehrRequest(patientOnly), enrich)
				refusedBeforeTheNetwork(t, env, rec, http.StatusBadGateway, fillFencedOtherPatient)
			})
			t.Run("questionnaire-package/"+level.String()+optInLabel(enrich), func(t *testing.T) {
				s := newPrefetchSoR()
				s.answer(t, "Coverage", page("", "", sorEntry(other(subscribedBy(sorCoverage("cov-1", shnsdk.CMSPayerIdentity.Value), "parent"))), patientInclude("parent", parentMarker)))
				env, rec := dtrIngressRowWith(t, s, ehrParams(ehrOrderParam("sr1", prefetchMember), dtrQuestionnaire), level, enrich)
				refusedBeforeTheNetwork(t, env, rec, http.StatusBadGateway, fillFencedOtherPatient)
			})
		}
	}
}

// sorTestFence is a fence for the system of record's patient, resolving
// absolute references on its base.
var sorTestFence = newPatientFence(shnsdk.MemberSystem, prefetchMember, []string{"https://sor.example/fhir"}, prefetchSoRID)

// withoutUnboundPatientIncludes drops exactly the included Patients no
// matched Coverage's beneficiary names: matches, other includes and the
// Patient a beneficiary names stay, in the server's order, across pages; a
// search with none is unchanged byte for byte.
func TestSearchset_WithoutUnboundPatientIncludes(t *testing.T) {
	org := `{"fullUrl":"https://sor.example/fhir/Organization/pay-9","resource":{"resourceType":"Organization","id":"pay-9"},"search":{"mode":"include"}}`
	cov := sorCoverage("cov-1", "00001")
	member := patientInclude(prefetchSoRID, "MBR")
	assembled := func(pages ...[]byte) sorSearchset {
		return classifySearch("q", "Coverage", resultOf(t, "Coverage", pages...), true)
	}
	got, err := assembled(page("", "", sorEntry(cov), patientInclude("parent", parentMarker), member, org)).withoutUnboundPatientIncludes(sorTestFence)
	if want := assembled(page("", "", sorEntry(cov), member, org)); err != nil || string(got.Value) != string(want.Value) || len(got.includes) != 2 {
		t.Fatalf("%v\ngot %s\nwant %s", err, got.Value, want.Value)
	}
	// Across pages, with the Patient on the first.
	got, err = assembled(page("", "https://sor.example/fhir/next", sorEntry(cov), patientInclude("parent", parentMarker)), page("", "", sorEntry(sorCoverage("cov-2", "00001")), org)).withoutUnboundPatientIncludes(sorTestFence)
	want := assembled(page("", "https://sor.example/fhir/next", sorEntry(cov)), page("", "", sorEntry(sorCoverage("cov-2", "00001")), org))
	if err != nil || string(got.Value) != string(want.Value) {
		t.Fatalf("two pages: %v\ngot %s\nwant %s", err, got.Value, want.Value)
	}
	none := assembled(page("", "", sorEntry(cov), member, org))
	if got, err := none.withoutUnboundPatientIncludes(sorTestFence); err != nil || string(got.Value) != string(none.Value) {
		t.Fatalf("a search with no unbound Patient include changed: %v", err)
	}
}

// A Patient the search included that a Coverage's beneficiary names, under an
// id that is not the system's id for the patient (a linked record), binds that
// Coverage as before: it is fenced and must be the patient, and the coverage
// the opt-in adds carries it. One that is another member is refused.
func TestCoverageSearch_ABeneficiaryTheSearchIncludedStillBinds(t *testing.T) {
	t.Parallel()
	linked := func(identifierValue, ref string) func(t *testing.T) *prefetchSoR {
		return func(t *testing.T) *prefetchSoR {
			s := newPrefetchSoR()
			cov := strings.ReplaceAll(sorCoverage("cov-1", "00001"), "Patient/"+prefetchSoRID, ref)
			p := `{ "fullUrl": "https://sor.example/fhir/Patient/linked-7", "resource": { "resourceType" : "Patient", "id" : "linked-7", "identifier" : [ { "system" : "` + shnsdk.MemberSystem + `", "value" : "` + identifierValue + `" } ], "name" : [ { "family" : "Linked" } ] }, "search": { "mode": "include" } }`
			s.answer(t, "Coverage", page("", "", sorEntry(cov), p))
			return s
		}
	}
	// The beneficiary names the linked Patient relative or versioned, as the
	// fence reads it (namesPatientEntry).
	refs := map[string]string{"relative": "Patient/linked-7", "versioned": "Patient/linked-7/_history/2"}
	for form, ref := range refs {
		for _, enrich := range []bool{false, true} {
			for _, level := range allLevels {
				t.Run(form+"/the member/"+level.String()+optInLabel(enrich), func(t *testing.T) {
					env, rec, _ := levelIngressRowWith(t, linked(prefetchMember, ref)(t), level, ehrRequest(patientOnly), enrich)
					if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
						t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
					}
					// The coverage the opt-in adds carries the Patient its
					// beneficiary names, as before.
					if enrich && !strings.Contains(string(sentRequest(t, env)), `"id" : "linked-7"`) {
						t.Fatal("the Patient the beneficiary names was not carried")
					}
				})
				t.Run(form+"/another member/"+level.String()+optInLabel(enrich), func(t *testing.T) {
					env, rec, _ := levelIngressRowWith(t, linked("MBR-OTHER", ref)(t), level, ehrRequest(patientOnly), enrich)
					refusedBeforeTheNetwork(t, env, rec, http.StatusBadGateway, fillFencedOtherPatient)
				})
			}
		}
	}
}

// Only an included Patient is dropped: another included resource about
// another patient is still fenced and refused, at every level.
func TestCoverageSearch_AnotherIncludedRecordIsStillFenced(t *testing.T) {
	t.Parallel()
	rp := `{ "fullUrl": "https://sor.example/fhir/RelatedPerson/rp-1", "resource": { "resourceType" : "RelatedPerson", "id" : "rp-1", "patient" : { "reference" : "Patient/someone-else" } }, "search": { "mode": "include" } }`
	for _, enrich := range []bool{false, true} {
		for _, level := range allLevels {
			t.Run(level.String()+optInLabel(enrich), func(t *testing.T) {
				s := newPrefetchSoR()
				s.answer(t, "Coverage", page("", "", sorEntry(subscribedBy(sorCoverage("cov-1", "00001"), "parent")), patientInclude("parent", parentMarker), rp))
				env, rec, _ := levelIngressRowWith(t, s, level, ehrRequest(patientOnly), enrich)
				refusedBeforeTheNetwork(t, env, rec, http.StatusBadGateway, fillFencedOtherPatient)
			})
		}
	}
}

// ehrEntryPartyCoverage is an EHR's coverage prefetch Bundle carrying the
// dependent's Coverage, the dependent's own Patient entry and the parent the
// Coverage's subscriber names as an entry of its own; extra entries are
// appended.
func ehrEntryPartyCoverage(extra ...string) string {
	b := `{"resourceType":"Bundle","type":"searchset","entry":[` +
		`{"fullUrl":"https://ehr.example/fhir/Coverage/c1","resource":{"resourceType":"Coverage","id":"c1","beneficiary":{"reference":"Patient/example"},"subscriber":{"reference":"Patient/parent"},"payor":[{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}}]}},` +
		`{"fullUrl":"https://ehr.example/fhir/Patient/example","resource":{"resourceType":"Patient","id":"example"}},` +
		`{"fullUrl":"https://ehr.example/fhir/Patient/parent","resource":{"resourceType":"Patient","id":"parent","identifier":[{"system":"urn:oid:1.2.3.4.5","value":"MRN-9"}]}}`
	for _, e := range extra {
		b += "," + e
	}
	return b + `]}`
}

// The EHR's own coverage prefetch is checked for one patient as a content
// rule (patient.mixed): the parent its Coverage names as a Patient entry of
// its own, named by shnsdk.CoverageEntryParties, is no other patient at any
// level, and the value is carried exactly; a second Patient entry no Coverage
// names is another patient, recorded or refused as the level says.
func TestLevelCRDIngress_EHRCoverageWithAnEntryParty(t *testing.T) {
	t.Parallel()
	for _, level := range allLevels {
		t.Run("the parent/"+level.String(), func(t *testing.T) {
			value := ehrEntryPartyCoverage()
			env, rec, findings := levelIngressRow(t, newPrefetchSoR(), level, ehrRequest(patientOnly+`,"coverage":`+value))
			if rec.Code != http.StatusOK || env.routeHitCount() != 1 || len(findings) != 0 {
				t.Fatalf("answer %d %s, findings %+v", rec.Code, rec.Body.String(), findings)
			}
			if got, _ := valueOf(t, sentRequest(t, env), "prefetch", "coverage"); got != value {
				t.Fatalf("the EHR's value must be carried exactly, got %s", got)
			}
		})
		t.Run("a second, unrelated Patient/"+level.String(), func(t *testing.T) {
			value := ehrEntryPartyCoverage(`{"fullUrl":"https://ehr.example/fhir/Patient/other","resource":{"resourceType":"Patient","id":"other","identifier":[{"system":"urn:oid:1.2.3.4.5","value":"MRN-10"}]}}`)
			env, rec, findings := levelIngressRow(t, newPrefetchSoR(), level, ehrRequest(patientOnly+`,"coverage":`+value))
			wantLevelOutcome(t, level, env, rec, findings, RulePatientMixed, http.StatusForbidden, "prefetch coverage refused")
		})
	}
}
