package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// A dependent's PAS answer carries the Coverage the payer answered for, and
// that Coverage names the parent as its subscriber or policyHolder: a Patient
// contained in the Coverage, carrying only an MRN. The answer's subject rules
// (the $submit response's subjectMismatch, the inquiry answer's subjects)
// read that parent as the Coverage's party, another person, and still fence
// it as a contained resource; every other contained Patient, and every other
// reference to the parent, binds as before.

// answerParent is the parent a dependent's Coverage contains.
func answerParent() map[string]any {
	return map[string]any{"resourceType": "Patient", "id": "parent",
		"identifier": []any{map[string]any{"system": "urn:oid:1.2.3.4.5", "value": "MRN-9"}},
		"name":       []any{map[string]any{"family": "Parent"}}}
}

// answerEntries returns the entries of an answer Bundle, or of every Bundle a
// Parameters answer returns.
func answerEntries(doc map[string]any) []map[string]any {
	var out []map[string]any
	if doc["resourceType"] == "Parameters" {
		params, _ := doc["parameter"].([]any)
		for _, p := range params {
			if res, _ := p.(map[string]any)["resource"].(map[string]any); res != nil {
				out = append(out, answerEntries(res)...)
			}
		}
		return out
	}
	entries, _ := doc["entry"].([]any)
	for _, e := range entries {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// answerEntryOf returns the first entry resource of resourceType.
func answerEntryOf(t *testing.T, doc map[string]any, resourceType string) map[string]any {
	t.Helper()
	for _, e := range answerEntries(doc) {
		if res, _ := e["resource"].(map[string]any); res != nil && res["resourceType"] == resourceType {
			return res
		}
	}
	t.Fatalf("fixture: the answer carries no %s entry", resourceType)
	return nil
}

// dependentAnswerOf returns answer with each of its entry-level Coverages
// made a dependent's (the parent contained, named by the subscriber, the
// relationship child), then edit applied to the whole answer.
func dependentAnswerOf(t *testing.T, answer []byte, edit func(t *testing.T, doc map[string]any)) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(answer, &doc); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range answerEntries(doc) {
		cov, _ := e["resource"].(map[string]any)
		if cov == nil || cov["resourceType"] != "Coverage" {
			continue
		}
		n++
		cov["contained"] = []any{answerParent()}
		cov["subscriber"] = map[string]any{"reference": "#parent"}
		cov["relationship"] = map[string]any{"coding": []any{map[string]any{"system": "http://terminology.hl7.org/CodeSystem/subscriber-relationship", "code": "child"}}}
	}
	if n == 0 {
		t.Fatal("fixture: the answer carries no Coverage entry")
	}
	if edit != nil {
		edit(t, doc)
	}
	return mustJSON(t, doc)
}

// coverageEdit applies fn to the answer's first Coverage entry.
func coverageEdit(fn func(cov map[string]any)) func(*testing.T, map[string]any) {
	return func(t *testing.T, doc map[string]any) { fn(answerEntryOf(t, doc, "Coverage")) }
}

// partyEdit applies fn to the parent the first Coverage contains.
func partyEdit(fn func(parent map[string]any)) func(*testing.T, map[string]any) {
	return coverageEdit(func(cov map[string]any) { fn(cov["contained"].([]any)[0].(map[string]any)) })
}

// dependentAnswerAccepts are the edits a dependent's answer may carry: each
// keeps the parent the Coverage's party.
var dependentAnswerAccepts = map[string]func(*testing.T, map[string]any){
	"subscriber": nil,
	"policyHolder": coverageEdit(func(cov map[string]any) {
		delete(cov, "subscriber")
		cov["policyHolder"] = map[string]any{"reference": "#parent"}
	}),
	"subscriber and policyHolder": coverageEdit(func(cov map[string]any) {
		cov["policyHolder"] = map[string]any{"reference": "#parent"}
	}),
	// The slot typed Patient: the one typed Patient reference that names
	// another person.
	"the slots typed Patient": coverageEdit(func(cov map[string]any) {
		cov["subscriber"] = map[string]any{"reference": "#parent", "type": "Patient", "display": "Parent"}
		cov["policyHolder"] = map[string]any{"reference": "#parent", "type": "http://hl7.org/fhir/StructureDefinition/Patient"}
	}),
	"a parent with no identifier": partyEdit(func(p map[string]any) { delete(p, "identifier") }),
	"a parent carrying another member's identifier": partyEdit(func(p map[string]any) {
		p["identifier"] = []any{map[string]any{"system": "urn:shn:member", "value": "MBR-2"}}
	}),
}

// dependentAnswerRefusals are the edits that make a dependent's answer bind
// to more than one patient (or to a party that is not fenced), each one
// mutation from an accepted answer. shape marks the ones the answer's graph
// closure refuses before the subject rule reads it.
var dependentAnswerRefusals = map[string]struct {
	edit  func(*testing.T, map[string]any)
	shape bool
}{
	"another Patient contained, no slot naming it": {edit: coverageEdit(func(cov map[string]any) {
		cov["contained"] = append(cov["contained"].([]any), map[string]any{"resourceType": "Patient", "id": "other", "identifier": []any{map[string]any{"system": "urn:shn:member", "value": "MBR-2"}}})
	})},
	"another Patient contained, named in the slot's extension": {edit: coverageEdit(func(cov map[string]any) {
		cov["contained"] = append(cov["contained"].([]any), map[string]any{"resourceType": "Patient", "id": "other"})
		cov["subscriber"] = map[string]any{"reference": "#parent", "extension": []any{map[string]any{"url": "http://example.org/x", "valueReference": map[string]any{"reference": "#other"}}}}
	})},
	"the party also named in an extension": {edit: coverageEdit(func(cov map[string]any) {
		cov["extension"] = []any{map[string]any{"url": "http://example.org/x", "valueReference": map[string]any{"reference": "#parent"}}}
	})},
	"the party also named in its slot's extension": {edit: coverageEdit(func(cov map[string]any) {
		cov["subscriber"] = map[string]any{"reference": "#parent", "extension": []any{map[string]any{"url": "http://example.org/x", "valueReference": map[string]any{"reference": "#parent"}}}}
	})},
	"the party also the beneficiary": {edit: coverageEdit(func(cov map[string]any) {
		cov["beneficiary"] = map[string]any{"reference": "#parent"}
	})},
	"the party also the payor": {edit: coverageEdit(func(cov map[string]any) {
		cov["payor"] = append(cov["payor"].([]any), map[string]any{"reference": "#parent"})
	})},
	// Only the slot naming the party is exempt: the other slot, typed
	// Patient, names another member.
	"the other slot a typed Patient naming another member": {edit: coverageEdit(func(cov map[string]any) {
		cov["policyHolder"] = map[string]any{"type": "Patient", "identifier": map[string]any{"system": "urn:shn:member", "value": "MBR-2"}}
	})},
	"the party's identifier not a list": {edit: partyEdit(func(p map[string]any) {
		p["identifier"] = map[string]any{"system": "urn:oid:1.2.3.4.5", "value": "MRN-9"}
	})},
	// A member the rule reads spelled in another case makes no party.
	"the party spelling identifier in another case": {edit: partyEdit(func(p map[string]any) {
		p["Identifier"] = p["identifier"]
		delete(p, "identifier")
	})},
	"the party containing a resource": {shape: true, edit: partyEdit(func(p map[string]any) {
		p["contained"] = []any{map[string]any{"resourceType": "Observation", "id": "o", "status": "final", "code": map[string]any{"text": "x"}}}
	})},
	"two contained resources sharing the party's id": {shape: true, edit: coverageEdit(func(cov map[string]any) {
		cov["contained"] = append(cov["contained"].([]any), answerParent())
	})},
	// "#parent" names one resource only when no other contained resource has
	// its id (shnsdk.CoverageParty): neither copy is then the party.
	"another contained resource sharing the party's id": {shape: true, edit: coverageEdit(func(cov map[string]any) {
		cov["contained"] = append(cov["contained"].([]any), map[string]any{"resourceType": "Organization", "id": "parent", "name": "Employer"})
	})},
	"a nested Coverage containing its parent": {shape: true, edit: nestedCoverageWithItsParent},
	// The parent as an entry of its own: another patient's record, as before.
	"a standalone parent entry": {edit: func(t *testing.T, doc map[string]any) {
		cov := answerEntryOf(t, doc, "Coverage")
		delete(cov, "contained")
		cov["subscriber"] = map[string]any{"reference": "Patient/Parent"}
		b := answerBundleHolding(t, doc, cov)
		b["entry"] = append(b["entry"].([]any), map[string]any{"fullUrl": answerBase(t, doc) + "/Patient/Parent", "resource": map[string]any{"resourceType": "Patient", "id": "Parent"}})
	}},
	// A Coverage that is not itself an entry has no party: the Claim's
	// contained Coverage and the parent beside it.
	"a nested Coverage's parent": {edit: func(t *testing.T, doc map[string]any) {
		cov := answerEntryOf(t, doc, "Coverage")
		delete(cov, "contained")
		delete(cov, "subscriber")
		holder := answerEntryOf(t, doc, "ClaimResponse")
		patient := holder["patient"]
		holder["contained"] = []any{
			map[string]any{"resourceType": "Coverage", "id": "nested", "status": "active", "beneficiary": patient, "subscriber": map[string]any{"reference": "#parent"}, "payor": []any{map[string]any{"display": "Payer"}}},
			answerParent(),
		}
		holder["insurance"] = []any{map[string]any{"sequence": 1, "focal": true, "coverage": map[string]any{"reference": "#nested"}}}
	}},
}

// nestedCoverageWithItsParent makes the answer's ClaimResponse contain a
// Coverage that itself contains the parent its subscriber names: a Coverage
// that is no entry has no party, whatever it contains.
func nestedCoverageWithItsParent(t *testing.T, doc map[string]any) {
	cov := answerEntryOf(t, doc, "Coverage")
	delete(cov, "contained")
	delete(cov, "subscriber")
	holder := answerEntryOf(t, doc, "ClaimResponse")
	holder["contained"] = []any{map[string]any{"resourceType": "Coverage", "id": "nested", "status": "active", "beneficiary": holder["patient"],
		"contained": []any{answerParent()}, "subscriber": map[string]any{"reference": "#parent"}, "payor": []any{map[string]any{"display": "Payer"}}}}
	holder["insurance"] = []any{map[string]any{"sequence": 1, "focal": true, "coverage": map[string]any{"reference": "#nested"}}}
}

// answerBundleHolding returns the answer Bundle res is an entry of: the
// answer, or one of the Bundles a Parameters answer returns.
func answerBundleHolding(t *testing.T, doc, res map[string]any) map[string]any {
	t.Helper()
	bundles := []map[string]any{doc}
	if doc["resourceType"] == "Parameters" {
		bundles = nil
		params, _ := doc["parameter"].([]any)
		for _, p := range params {
			if b, _ := p.(map[string]any)["resource"].(map[string]any); b != nil {
				bundles = append(bundles, b)
			}
		}
	}
	for _, b := range bundles {
		entries, _ := b["entry"].([]any)
		for _, e := range entries {
			if r, _ := e.(map[string]any)["resource"].(map[string]any); r != nil && fmt.Sprintf("%p", r) == fmt.Sprintf("%p", res) {
				return b
			}
		}
	}
	t.Fatal("fixture: the resource is no entry of the answer")
	return nil
}

// answerBase is the base of the answer's ClaimResponse entry's fullUrl.
func answerBase(t *testing.T, doc map[string]any) string {
	t.Helper()
	for _, e := range answerEntries(doc) {
		if res, _ := e["resource"].(map[string]any); res != nil && res["resourceType"] == "ClaimResponse" {
			full, _ := e["fullUrl"].(string)
			if i := strings.Index(full, "/ClaimResponse/"); i > 0 {
				return full[:i]
			}
		}
	}
	t.Fatal("fixture: no RESTful ClaimResponse entry")
	return ""
}

func TestDependentSubmitAnswer_PartyIsNoSecondPatient(t *testing.T) {
	if pasResponseSubjectMismatch([]byte(assemblyRealPending)) != nil {
		t.Fatal("fixture: the base answer must bind")
	}
	for name, edit := range dependentAnswerAccepts {
		t.Run("accepts/"+name, func(t *testing.T) {
			answer := dependentAnswerOf(t, []byte(assemblyRealPending), edit)
			if _, bad := validateNativePASResponse(answer); bad.Status != 0 {
				t.Fatalf("the answer must be readable: %+v", bad)
			}
			if r := pasResponseSubjectMismatch(answer); r != nil {
				t.Fatalf("refused: %s", r.Why)
			}
		})
	}
	for name, row := range dependentAnswerRefusals {
		t.Run("refuses/"+name, func(t *testing.T) {
			answer := dependentAnswerOf(t, []byte(assemblyRealPending), row.edit)
			_, bad := validateNativePASResponse(answer)
			if row.shape {
				if bad.Status == 0 {
					t.Fatal("the closure must refuse this graph")
				}
				return
			}
			if bad.Status != 0 {
				t.Fatalf("the graph must close, so the subject rule decides: %+v", bad)
			}
			if pasResponseSubjectMismatch(answer) == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// Only the party's own id must be unique among the Coverage's contained
// resources, as shnsdk.CoverageParty and the sdk's PAS response check read
// it: another contained resource without an id, or two others sharing an id
// of their own, leave "#parent" naming one resource, so the parent is still
// the party. The inquiry answer, which no graph closure reads, carries such
// a Coverage; a $submit answer's graph closure refuses it whether or not the
// Coverage has a party.
func TestDependentAnswer_OnlyThePartysIDMustBeUnique(t *testing.T) {
	for name, extra := range map[string][]any{
		"another contained resource without an id": {map[string]any{"resourceType": "Organization", "name": "Employer"}},
		"two other contained resources sharing an id": {
			map[string]any{"resourceType": "Organization", "id": "o", "name": "Employer"},
			map[string]any{"resourceType": "Organization", "id": "o", "name": "Employer 2"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			edit := coverageEdit(func(cov map[string]any) { cov["contained"] = append(cov["contained"].([]any), extra...) })
			for _, fixture := range []string{"pas-inquiry-response-2.0.json", "pas-inquiry-response-2.2.json"} {
				answer := dependentAnswerOf(t, inquiryFixture(t, fixture), edit)
				if validatePASInquiryAnswer(answer).Status != 0 || !consistentPASInquiryAnswerSubjects(answer) {
					t.Fatalf("%s: the parent was not read as the party", fixture)
				}
				var doc map[string]any
				if err := json.Unmarshal(answer, &doc); err != nil {
					t.Fatal(err)
				}
				if p := entryCoverageParties(answerEntryOf(t, doc, "Coverage")); !p.any() || p.roleOf(0) != roleCoverageParty {
					t.Fatalf("%s: the Coverage has no party", fixture)
				}
			}
			if _, bad := validateNativePASResponse(dependentAnswerOf(t, []byte(assemblyRealPending), edit)); bad.Status == 0 {
				t.Fatal("the $submit answer's graph closure must refuse this Coverage")
			}
			noParty := dependentAnswerOf(t, []byte(assemblyRealPending), coverageEdit(func(cov map[string]any) {
				delete(cov, "subscriber")
				cov["contained"] = extra
			}))
			if _, bad := validateNativePASResponse(noParty); bad.Status == 0 {
				t.Fatal("the closure must refuse this Coverage without a party too")
			}
		})
	}
}

// A contained Patient that does not hold as a contained resource is no
// party (shnsdk.CoverageParty), so the subject rule itself reads it as a
// second patient, not only the graph closure that runs before it: one that
// contains a resource, one whose identifier is not a list, one that spells a
// member the rule reads in another case, and the parent a Coverage that is no
// entry contains.
func TestDependentSubmitAnswer_RuleFencesTheParty(t *testing.T) {
	const asPatient = "carries a Patient (http://localhost:8081/fhir/Coverage/InsuranceExample#parent) at /contained/0 that is not the ClaimResponse's patient"
	for name, row := range map[string]struct {
		edit func(*testing.T, map[string]any)
		want string
	}{
		"containing a resource":            {dependentAnswerRefusals["the party containing a resource"].edit, asPatient},
		"identifier not a list":            {dependentAnswerRefusals["the party's identifier not a list"].edit, asPatient},
		"identifier named in another case": {dependentAnswerRefusals["the party spelling identifier in another case"].edit, asPatient},
		"a nested Coverage's":              {nestedCoverageWithItsParent, "carries a Patient (http://localhost:8081/fhir/ClaimResponse/1770#parent) at /contained/0/contained/0"},
	} {
		t.Run(name, func(t *testing.T) {
			g, err := readPASGraph(dependentAnswerOf(t, []byte(assemblyRealPending), row.edit))
			if err != nil {
				t.Fatal(err)
			}
			expected, why := g.subjectIdentity(g.response, "Patient/SubscriberExample")
			if why != "" {
				t.Fatal(why)
			}
			if r := g.subjectMismatch(expected); r == nil || !strings.Contains(r.Why, row.want) {
				t.Fatalf("got %+v, want %q", r, row.want)
			}
		})
	}
	g, err := readPASGraph(dependentAnswerOf(t, []byte(assemblyRealPending), nil))
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := g.subjectIdentity(g.response, "Patient/SubscriberExample")
	if r := g.subjectMismatch(expected); r != nil {
		t.Fatalf("the control is refused: %s", r.Why)
	}
}

// The refusal names the parent the rule read as a second patient.
func TestDependentSubmitAnswer_RefusalNamesTheContainedPatient(t *testing.T) {
	answer := dependentAnswerOf(t, []byte(assemblyRealPending), dependentAnswerRefusals["the party also named in an extension"].edit)
	r := pasResponseSubjectMismatch(answer)
	want := "carries a Patient (http://localhost:8081/fhir/Coverage/InsuranceExample#parent) at /contained/0 that is not the ClaimResponse's patient http://localhost:8081/fhir/Patient/SubscriberExample"
	if r == nil || !strings.Contains(r.Why, want) {
		t.Fatalf("got %+v, want %q", r, want)
	}
}

func TestDependentInquiryAnswer_PartyIsNoSecondPatient(t *testing.T) {
	for _, fixture := range []string{"pas-inquiry-response-2.0.json", "pas-inquiry-response-2.2.json"} {
		base := inquiryFixture(t, fixture)
		if validatePASInquiryAnswer(base).Status != 0 || !consistentPASInquiryAnswerSubjects(base) {
			t.Fatalf("fixture %s: the base answer must be readable and bind", fixture)
		}
		for name, edit := range dependentAnswerAccepts {
			t.Run(fixture+"/accepts/"+name, func(t *testing.T) {
				answer := dependentAnswerOf(t, base, edit)
				if validatePASInquiryAnswer(answer).Status != 0 || !consistentPASInquiryAnswerSubjects(answer) {
					t.Fatal("refused")
				}
			})
		}
		for name, row := range dependentAnswerRefusals {
			t.Run(fixture+"/refuses/"+name, func(t *testing.T) {
				answer := dependentAnswerOf(t, base, row.edit)
				if validatePASInquiryAnswer(answer).Status != 0 {
					t.Fatal("the answer must be readable, so the subject rule decides")
				}
				if consistentPASInquiryAnswerSubjects(answer) {
					t.Fatal("accepted")
				}
			})
		}
	}
}

// A PAS request graph this gateway completes from its system of record is
// bound by the same rule: a dependent's Coverage entry carries its party, and
// the parent named anywhere else too is a second patient.
func TestDependentRequestGraph_PartyIsNoSecondPatient(t *testing.T) {
	const coverage = `{"resourceType":"Coverage","id":"cov","status":"active","contained":[{"resourceType":"Patient","id":"parent","identifier":[{"system":"urn:oid:1.2.3.4.5","value":"MRN-9"}]}],"subscriber":{"reference":"#parent"},"beneficiary":{"reference":"Patient/p"},"payor":[{"display":"Payer"}]%s}`
	body := func(extra string) []byte {
		return []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"fullUrl":"https://example.test/fhir/Patient/p","resource":{"resourceType":"Patient","id":"p"}},` +
			`{"fullUrl":"https://example.test/fhir/Coverage/cov","resource":` + fmt.Sprintf(coverage, extra) + `}]}`)
	}
	read := func(context.Context, string) ([]byte, bool, error) { return nil, false, nil }
	if _, err := retainPASRequestEvidence(context.Background(), body(""), read); err != nil {
		t.Fatalf("a dependent's request graph was refused: %v", err)
	}
	if _, err := retainPASRequestEvidence(context.Background(), body(`,"extension":[{"url":"http://example.org/x","valueReference":{"reference":"#parent"}}]`), read); err == nil {
		t.Fatal("a parent also named in an extension was carried as a party")
	}
}
