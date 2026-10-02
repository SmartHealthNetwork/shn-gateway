package engine

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A dependent's Coverage names the parent in its subscriber or policyHolder,
// often as a contained Patient that carries only an MRN. The patient fence
// carries that party as part of the Coverage (shnsdk.CoverageParty), wherever a
// Coverage is fenced: the routing read through fhirServer and its carry, the
// system of record's Coverage read with and without enrichment, a Coverage
// the EHR sent in prefetch.coverage, and the $questionnaire-package coverage,
// sent or obtained. The beneficiary still binds the Coverage.

// dependentParent is the parent, contained, carrying only an MRN.
const dependentParent = `{"resourceType":"Patient","id":"parent","identifier":[{"system":"urn:oid:1.2.3.4.5","value":"MRN-9"}],"name":[{"family":"Parent"}]}`

// dependentSlots are the party slot sets a dependent's Coverage may use.
var dependentSlots = map[string][]string{
	"subscriber":            {"subscriber"},
	"policyHolder":          {"policyHolder"},
	"subscriber and holder": {"subscriber", "policyHolder"},
}

// dependentMembers is the members a dependent's Coverage adds before its
// beneficiary: the contained parent and the slots naming it.
func dependentMembers(slots []string) string {
	m := `"contained":[` + dependentParent + `],"relationship":{"coding":[{"code":"child"}]},`
	for _, s := range slots {
		m += `"` + s + `":{"reference":"#parent"},`
	}
	return m
}

// beneficiaryOf returns how v writes Coverage.beneficiary naming patient:
// compactly, or in a server's spaced layout.
func beneficiaryOf(t *testing.T, v, patient string) string {
	t.Helper()
	for _, b := range []string{
		`"beneficiary":{"reference":"Patient/` + patient + `"}`,
		`"beneficiary" : { "reference" : "Patient/` + patient + `" }`,
	} {
		if strings.Contains(v, b) {
			return b
		}
	}
	t.Fatalf("fixture names no beneficiary Patient/%s", patient)
	return ""
}

// dependentOf turns the Coverage in v whose beneficiary is Patient/<patient>
// into a dependent's, the parent in slots.
func dependentOf(t *testing.T, v, patient string, slots []string) string {
	t.Helper()
	b := beneficiaryOf(t, v, patient)
	return strings.Replace(v, b, dependentMembers(slots)+b, 1)
}

// alsoFromPayor makes the parent referenced from the payor too: no longer a
// party only.
func alsoFromPayor(v string) string {
	return strings.Replace(v, `"relationship":`, `"insurer":{"reference":"#parent"},"relationship":`, 1)
}

// dependentCoverage is a Coverage for patient naming its payer inline.
func dependentCoverage(t *testing.T, id, patient string, slots []string) string {
	t.Helper()
	return dependentOf(t, `{"resourceType":"Coverage","id":"`+id+`","status":"active","beneficiary":{"reference":"Patient/`+patient+`"},"payor":[{"identifier":{"system":"`+shnsdk.CMSPayerIdentity.System+`","value":"00001"}}]}`, patient, slots)
}

// The read through the request's fhirServer routes a dependent's Coverage
// and carries it, the contained parent byte for byte, at every level.
func TestFHIRServerReadRoutesAndCarriesADependentsCoverage(t *testing.T) {
	for name, slots := range dependentSlots {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				e := newEHRServer(t, nil)
				answer := dependentOf(t, serverAnswer(e.base), strangerMember, slots)
				e.handler = ehrAnswer(200, "application/fhir+json", answer)
				env, _ := fhirServerEnv(t, e, "")
				env.originator.cfg.ConformanceEnforcement = level
				rec := httptest.NewRecorder()
				env.originator.handleCRDIngress(rec, crdIngressPost(fhirServerRequest(e.base)))
				if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
					t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
				}
				got := carriedSearchsetOf(t, sentRequest(t, env))
				if len(got.matches) != 1 || got.matches[0] != serverResource(t, answer, "Coverage", "c9") || !strings.Contains(got.matches[0], dependentParent) {
					t.Fatalf("carried %+v", got)
				}
			})
		}
	}
}

// The read through fhirServer still refuses: a parent referenced from
// anywhere but the party slots, a standalone entry for another Patient (412),
// and a beneficiary naming another patient (502), at every level.
func TestFHIRServerReadRefusesWhatIsNotADependentsCoverage(t *testing.T) {
	rows := map[string]struct {
		answer func(t *testing.T, base string) string
		status int
		msg    string
	}{
		"the parent referenced from another element": {func(t *testing.T, base string) string {
			return alsoFromPayor(dependentOf(t, serverAnswer(base), strangerMember, []string{"subscriber"}))
		}, fhirServerRefused, fhirServerNotSearchset},
		"a standalone entry for the parent": {func(t *testing.T, base string) string {
			a := serverAnswer(base)
			b := beneficiaryOf(t, a, strangerMember)
			a = strings.Replace(a, b, `"subscriber":{"reference":"Patient/parent"},`+b, 1)
			i := strings.LastIndex(a, "]}")
			return a[:i] + `,{"resource":` + dependentParent + `,"search":{"mode":"include"}}` + a[i:]
		}, fhirServerRefused, fhirServerNotSearchset},
		"the beneficiary naming another patient": {func(t *testing.T, base string) string {
			a := dependentOf(t, serverAnswer(base), strangerMember, []string{"subscriber", "policyHolder"})
			return strings.Replace(a, beneficiaryOf(t, a, strangerMember), `"beneficiary":{"reference":"Patient/someone-else"}`, 1)
		}, http.StatusBadGateway, fhirServerOtherPatient},
	}
	for name, row := range rows {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				e := newEHRServer(t, nil)
				e.handler = ehrAnswer(200, "application/fhir+json", row.answer(t, e.base))
				env, _ := fhirServerEnv(t, e, "")
				env.originator.cfg.ConformanceEnforcement = level
				rec := httptest.NewRecorder()
				env.originator.handleCRDIngress(rec, crdIngressPost(fhirServerRequest(e.base)))
				refusedBeforeTheNetwork(t, env, rec, row.status, row.msg)
			})
		}
	}
}

// The system of record's Coverage read routes a dependent's Coverage, and
// under enrichment carries it with the parent byte for byte; a parent
// referenced from another element is refused (502) at every level.
func TestSystemOfRecordRoutesADependentsCoverage(t *testing.T) {
	for name, slots := range dependentSlots {
		for _, enrich := range []bool{false, true} {
			for _, level := range allLevels {
				t.Run(name+"/"+level.String()+map[bool]string{false: "", true: "/enrich"}[enrich], func(t *testing.T) {
					cov := dependentCoverage(t, "cov-1", prefetchSoRID, slots)
					s := newPrefetchSoR()
					s.answer(t, "Coverage", page("", "", sorEntry(cov)))
					env, rec, _ := levelIngressRowWith(t, s, level, ehrRequest(patientOnly), enrich)
					if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
						t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
					}
					if carried := bytes.Contains(sentRequest(t, env), []byte(cov)); carried != enrich {
						t.Fatalf("the Coverage carried byte for byte: %t, want %t", carried, enrich)
					}
				})
			}
		}
	}
	for _, enrich := range []bool{false, true} {
		for _, level := range allLevels {
			t.Run("the parent referenced from another element/"+level.String()+map[bool]string{false: "", true: "/enrich"}[enrich], func(t *testing.T) {
				s := newPrefetchSoR()
				s.answer(t, "Coverage", page("", "", sorEntry(alsoFromPayor(dependentCoverage(t, "cov-1", prefetchSoRID, []string{"subscriber"})))))
				env, rec, _ := levelIngressRowWith(t, s, level, ehrRequest(patientOnly), enrich)
				refusedBeforeTheNetwork(t, env, rec, http.StatusBadGateway, fillFencedOtherPatient)
			})
		}
	}
}

// A dependent's Coverage the EHR sent in prefetch.coverage is carried at
// every level, strict included; one whose parent is referenced from another
// element is refused at strict (the request's own consistency) and carried as
// sent below it.
func TestSentPrefetchCoverageOfADependent(t *testing.T) {
	for name, slots := range dependentSlots {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				body := ehrRequest(patientOnly + `,"coverage":` + dependentCoverage(t, "c1", "example", slots))
				env, rec, _ := levelIngressRowWith(t, newPrefetchSoR(), level, body, false)
				if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
					t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
				}
			})
		}
	}
	for _, level := range allLevels {
		t.Run("the parent referenced from another element/"+level.String(), func(t *testing.T) {
			body := ehrRequest(patientOnly + `,"coverage":` + alsoFromPayor(dependentCoverage(t, "c1", "example", []string{"subscriber"})))
			env, rec, _ := levelIngressRowWith(t, newPrefetchSoR(), level, body, false)
			if level == EnforcementStrict {
				refusedBeforeTheNetwork(t, env, rec, http.StatusForbidden, "prefetch coverage refused: Patient refused: another patient")
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("below strict the request is carried as sent: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// The $questionnaire-package ingress routes a dependent's Coverage, sent as
// the coverage parameter or obtained from the system of record, at every
// level; an obtained one whose parent is referenced from another element is
// refused (502).
func TestDTRIngressRoutesADependentsCoverage(t *testing.T) {
	for name, slots := range dependentSlots {
		for _, level := range allLevels {
			t.Run("sent/"+name+"/"+level.String(), func(t *testing.T) {
				body := ehrParams(ehrOrderParam("sr1", prefetchMember), `{"name":"coverage","resource":`+dependentCoverage(t, "c1", prefetchMember, slots)+`}`, dtrQuestionnaire)
				env, rec := dtrIngressRowAt(t, newPrefetchSoR(), body, level)
				if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
					t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
				}
			})
			t.Run("obtained/"+name+"/"+level.String(), func(t *testing.T) {
				s := newPrefetchSoR()
				s.answer(t, "Coverage", page("", "", sorEntry(dependentCoverage(t, "cov-1", prefetchSoRID, slots))))
				env, rec := dtrIngressRowAt(t, s, ehrParams(ehrOrderParam("sr1", prefetchMember), dtrQuestionnaire), level)
				if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
					t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
				}
			})
		}
	}
	for _, level := range allLevels {
		t.Run("obtained, the parent referenced from another element/"+level.String(), func(t *testing.T) {
			s := newPrefetchSoR()
			s.answer(t, "Coverage", page("", "", sorEntry(alsoFromPayor(dependentCoverage(t, "cov-1", prefetchSoRID, []string{"subscriber"})))))
			env, rec := dtrIngressRowAt(t, s, ehrParams(ehrOrderParam("sr1", prefetchMember), dtrQuestionnaire), level)
			refusedBeforeTheNetwork(t, env, rec, http.StatusBadGateway, fillFencedOtherPatient)
		})
	}
}

// A Coverage's party is another person: a payer (or a provider) that does not
// hold the member derives the member's identity from the member's own carried
// Patient, never from the parent, even one carrying the member's id or member
// identifier (a family-level member number).
func TestSubjectDerivationNeverReadsACoveragesParty(t *testing.T) {
	parent := `{"resourceType":"Patient","id":"` + strangerMember + `","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"` + strangerMember + `"}],"name":[{"family":"Parentname"}],"birthDate":"1970-01-01"}`
	child := `{"resourceType":"Patient","id":"` + strangerMember + `","name":[{"family":"Childname"}],"birthDate":"2015-06-01"}`
	coverage := `{"resourceType":"Coverage","id":"c","contained":[` + strings.Replace(parent, `"id":"`+strangerMember+`"`, `"id":"parent"`, 1) + `],` +
		`"subscriber":{"reference":"#parent"},"policyHolder":{"reference":"#parent"},"beneficiary":{"reference":"Patient/` + strangerMember + `"}}`
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	for name, row := range map[string]struct {
		payload string
		want    string
	}{
		"the parent alone":         {`{"prefetch":{"coverage":` + coverage + `}}`, derivedPCI(strangerMember, "", "")},
		"the parent and the child": {`{"prefetch":{"patient":` + child + `,"coverage":` + coverage + `}}`, derivedPCI(strangerMember, "2015-06-01", "Childname")},
		"in a searchset":           {`{"prefetch":{"coverage":{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + coverage + `}]}}}`, derivedPCI(strangerMember, "", "")},
	} {
		t.Run(name, func(t *testing.T) {
			if d, ok := carriedPatientDemographics([]byte(row.payload), strangerMember); ok && d.FamilyName == "Parentname" {
				t.Fatalf("the parent's demographics were read: %+v", d)
			}
			pci, found, err := g.resolveSubjectPCI(context.Background(), strangerMember, []byte(row.payload))
			if err != nil || !found || pci != row.want {
				t.Fatalf("pci %q (found %v, err %v), want %q", pci, found, err, row.want)
			}
		})
	}
	// The same contained Patient named from anywhere but the party slots is
	// read as before: it is no party.
	notParty := strings.Replace(coverage, `"policyHolder":{"reference":"#parent"},`, `"payor":[{"reference":"#parent"}],`, 1)
	if d, ok := carriedPatientDemographics([]byte(`{"prefetch":{"coverage":`+notParty+`}}`), strangerMember); !ok || d.FamilyName != "Parentname" {
		t.Fatalf("a contained Patient that is no party: %+v %v", d, ok)
	}
}

// A Coverage's party is no member of the request: the involved list names the
// patient the Coverage binds, never the parent its subscriber or policyHolder
// names (whose contained id is local), on every leg. A contained Patient that
// is no party is still read as before.
func TestCarriedMembersSkipACoveragesParty(t *testing.T) {
	coverage := `{"resourceType":"Coverage","id":"c","contained":[{"resourceType":"Patient","id":"parent","name":[{"family":"Parent"}]}],` +
		`"subscriber":{"reference":"#parent"},"policyHolder":{"reference":"#parent"},"beneficiary":{"reference":"Patient/m1"}}`
	payload := `{"resourceType":"Bundle","type":"collection","entry":[{"fullUrl":"urn:uuid:c1","resource":` + coverage + `}]}`
	for _, leg := range []string{"crd-order-select", "dtr-questionnaire-fetch", "pas-claim", "pas-claim-inquire"} {
		if got := carriedMembers([]byte(payload), leg); !slices.Equal(got, []string{"m1"}) {
			t.Fatalf("%s: members %v, want only the beneficiary", leg, got)
		}
		// A contained that is not a list is walked as before.
		odd := `{"resourceType":"Coverage","id":"c","contained":{"resourceType":"Patient","id":"x"},"subscriber":{"reference":"#x"},"beneficiary":{"reference":"Patient/m1"}}`
		if got := carriedMembers([]byte(odd), leg); !slices.Contains(got, "x") && !slices.Contains(got, "#x") {
			t.Fatalf("%s: an object-shaped contained Patient: members %v", leg, got)
		}
		notParty := strings.Replace(payload, `"policyHolder":{"reference":"#parent"},`, `"payor":[{"reference":"#parent"}],`, 1)
		if got := carriedMembers([]byte(notParty), leg); len(got) < 2 {
			t.Fatalf("%s: a contained Patient that is no party: members %v", leg, got)
		}
	}
}

// When the system of record names the patient by an id other than the
// request's, its Coverage read only routes, fenced to the system's own id
// alone, with the opt-in on or off. A dependent's Coverage there still
// carries its party, so a dependent member is routed (CDS Hooks and
// $questionnaire-package) and the parent is carried nowhere, while a
// dependent's Coverage whose beneficiary is the request's id (another patient
// in that system) is still refused, at every level.
func TestDependentCoverageRoutedWhenTheSystemNamesThePatientDifferently(t *testing.T) {
	for name, slots := range dependentSlots {
		for _, enrich := range []bool{false, true} {
			for _, level := range allLevels {
				t.Run("cds-hooks/"+name+"/"+level.String()+optInLabel(enrich), func(t *testing.T) {
					s := namedDifferently(t)
					cov := strings.ReplaceAll(sorCoverage("cov-1", "00001"), "Patient/example", "Patient/pat-elsewhere")
					s.answer(t, "Coverage", searchPage(dependentOf(t, cov, "pat-elsewhere", slots)))
					env, rec, _ := levelIngressRowWith(t, s, level, ehrRequest(patientOnly), enrich)
					if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
						t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
					}
					if strings.Contains(string(sentRequest(t, env)), "MRN-9") {
						t.Fatal("the parent was carried in the request")
					}
				})
				t.Run("cds-hooks-request-id/"+name+"/"+level.String()+optInLabel(enrich), func(t *testing.T) {
					s := namedDifferently(t)
					s.answer(t, "Coverage", searchPage(dependentOf(t, sorCoverage("cov-1", "00001"), "example", slots)))
					env, rec, _ := levelIngressRowWith(t, s, level, ehrRequest(patientOnly), enrich)
					refusedBeforeTheNetwork(t, env, rec, http.StatusBadGateway, fillFencedOtherPatient)
				})
				t.Run("questionnaire-package/"+name+"/"+level.String()+optInLabel(enrich), func(t *testing.T) {
					s := newPrefetchSoR()
					s.sorID = "sor-9"
					cov := strings.ReplaceAll(sorCoverage("cov-1", shnsdk.CMSPayerIdentity.Value), "Patient/"+prefetchSoRID, "Patient/sor-9")
					s.answer(t, "Coverage", searchPage(dependentOf(t, cov, "sor-9", slots)))
					env, rec := dtrIngressRowWith(t, s, ehrParams(ehrOrderParam("sr1", prefetchMember), dtrQuestionnaire), level, enrich)
					if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
						t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
					}
					if strings.Contains(string(sentRequest(t, env)), "MRN-9") {
						t.Fatal("the parent was carried in the request")
					}
				})
				t.Run("questionnaire-package-request-id/"+name+"/"+level.String()+optInLabel(enrich), func(t *testing.T) {
					s := newPrefetchSoR()
					s.sorID = "sor-9"
					cov := strings.ReplaceAll(sorCoverage("cov-1", shnsdk.CMSPayerIdentity.Value), "Patient/"+prefetchSoRID, "Patient/"+prefetchMember)
					s.answer(t, "Coverage", searchPage(dependentOf(t, cov, prefetchMember, slots)))
					env, rec := dtrIngressRowWith(t, s, ehrParams(ehrOrderParam("sr1", prefetchMember), dtrQuestionnaire), level, enrich)
					refusedBeforeTheNetwork(t, env, rec, http.StatusBadGateway, fillFencedOtherPatient)
				})
			}
		}
	}
}
