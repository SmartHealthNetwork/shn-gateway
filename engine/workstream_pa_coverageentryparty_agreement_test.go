//go:build !shn_coreonly

package engine

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"

	"github.com/SmartHealthNetwork/shn-gateway/internal/pasgraph"
)

// The engine decides a Coverage's party carried as a Patient entry of its own
// by shnsdk.CoverageEntryParties wherever it asks, never by a copy of the
// rule: the request walk (pasCarriesAnotherPatient), the inquiry's members
// bind (readPASInquiryFacts), the involved list (carriedMembers), and the
// answer's subject rules (pasGraphSubjectMismatch, the inquiry answer's
// pasgraph.InquiryScopeSubjects). These rows run each over every entry-parent
// request and answer row, and hold its reading of the parent's entry to the
// sdk's: the parent is read as a patient exactly when the rule names it no
// party.

// withProbeOnParent is body with the parent's Patient entry linking to a
// probe patient, Patient/zz-probe. The link is the parent's own reference, so
// it leaves the sdk's party decision unchanged.
func withProbeOnParent(t *testing.T, body []byte) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	before := shnsdk.CoverageEntryParties(doc)
	found := false
	for _, e := range doc["entry"].([]any) {
		res := e.(map[string]any)["resource"].(map[string]any)
		if res["id"] == "MBR-PARENT" {
			links, _ := res["link"].([]any)
			res["link"] = append(links, map[string]any{"type": "seealso", "other": map[string]any{"reference": "Patient/zz-probe"}})
			found = true
		}
	}
	if !found {
		t.Fatal("fixture: no parent entry")
	}
	if after := shnsdk.CoverageEntryParties(doc); !maps.Equal(before, after) {
		t.Fatalf("fixture: the probe changed the parties: %v, %v", before, after)
	}
	return mustJSON(t, doc)
}

// entryPartiesOf is the sdk's rule over bundle.
func entryPartiesOf(t *testing.T, bundle map[string]any) map[string]bool {
	t.Helper()
	return shnsdk.CoverageEntryParties(bundle)
}

func TestCoverageEntryParty_RequestWalkersAgree(t *testing.T) {
	t.Parallel()
	strict := func(string) bool { return true }
	reached := map[bool]int{}
	for fixture, base := range entryParentFixtures(t) {
		for _, rows := range []map[string]entryParent{entryParentAccepts, entryParentRefusals} {
			for name, p := range rows {
				body, ok := withEntryParent(t, base, p)
				if !ok {
					continue
				}
				t.Run(fixture+"/"+name, func(t *testing.T) {
					var doc map[string]any
					if err := json.Unmarshal(body, &doc); err != nil {
						t.Fatal(err)
					}
					parentURL := ""
					for _, e := range doc["entry"].([]any) {
						em := e.(map[string]any)
						if em["resource"].(map[string]any)["id"] == "MBR-PARENT" {
							parentURL, _ = em["fullUrl"].(string)
						}
					}
					party := entryPartiesOf(t, doc)[parentURL]
					reached[party]++
					// The involved list's own party decision, held to the rule:
					// it names the parent (by its id, or on the PAS submit legs
					// by its fullUrl when that names no Patient/<id>) exactly
					// when the rule names it no party, and, since it passes over
					// only the parent's own identity, it reaches a patient the
					// parent's entry names (the probe) whether or not the parent
					// is a party.
					probed := withProbeOnParent(t, body)
					for _, leg := range []string{"crd-order-select", "crd-order-dispatch", "dtr-questionnaire-fetch", "pas-claim", "pas-claim-update", "pas-claim-inquire"} {
						members := carriedMembers(probed, leg)
						if !slices.Contains(members, "zz-probe") {
							t.Fatalf("%s: the involved list did not reach the patient the parent's entry names (party %v): %v", leg, party, members)
						}
						if listed := slices.Contains(members, "MBR-PARENT") || slices.Contains(members, parentURL); listed == party {
							t.Fatalf("%s: the involved list lists the parent %v, the sdk names it a party %v", leg, listed, party)
						}
					}
					// The walk and the inquiry bind read the parent as another
					// patient whenever it is no party.
					if !party && !pasCarriesAnotherPatient(body, "MBR-COVERED") {
						t.Fatal("the walk read the parent's entry, no party, as no patient")
					}
					if fixture == "inquiry" && !party {
						if _, status, _ := readPASInquiryFacts(body, strict); status == 0 {
							t.Fatal("the inquiry bind read the parent's entry, no party, as no member")
						}
					}
				})
			}
		}
	}
	if reached[true] == 0 || reached[false] == 0 {
		t.Fatalf("the rows reach a party %d times and no party %d times: both must be reached", reached[true], reached[false])
	}
}

func TestCoverageEntryParty_AnswerWalkersAgree(t *testing.T) {
	t.Parallel()
	edits := map[string]func(*testing.T, map[string]any){}
	for name, edit := range entryParentAnswerAccepts {
		edits[name] = edit
	}
	// shape marks the rows a $submit answer's graph closure refuses before
	// the subject rule reads them: the only rows the $submit comparison skips.
	shape := map[string]bool{}
	for name, row := range entryParentAnswerRefusals {
		edits[name] = row.edit
		shape[name] = row.shape
	}
	// reached counts the inquiry walk's comparisons, and compared the $submit
	// rule's, each by whether the sdk names the parent a party.
	reached, compared := map[bool]int{}, map[bool]int{}
	for _, fixture := range []string{"pas-inquiry-response-2.0.json", "pas-inquiry-response-2.2.json", "submit"} {
		for name, edit := range edits {
			t.Run(fixture+"/"+name, func(t *testing.T) {
				var answer []byte
				if fixture == "submit" {
					answer = dependentAnswerOf(t, []byte(assemblyRealPending), edit)
				} else {
					answer = dependentAnswerOf(t, inquiryFixture(t, fixture), edit)
				}
				var doc any
				if err := json.Unmarshal(answer, &doc); err != nil {
					t.Fatal(err)
				}
				carried := false
				for _, bundle := range pasgraph.InquiryAnswerScopes(doc) {
					parentURL := ""
					for _, e := range answerEntries(bundle) {
						if res, _ := e["resource"].(map[string]any); res["resourceType"] == "Patient" && res["id"] == "Parent" {
							parentURL, _ = e["fullUrl"].(string)
						}
					}
					if parentURL == "" {
						continue
					}
					carried = true
					party := entryPartiesOf(t, bundle)[parentURL]
					reached[party]++
					members, _ := pasgraph.InquiryScopeSubjects(bundle)
					if members[parentURL] == party {
						t.Fatalf("the inquiry answer's walk reads the parent as a patient %v, the sdk names it a party %v", members[parentURL], party)
					}
					if fixture != "submit" {
						continue
					}
					g, err := readPASGraph(answer)
					if err != nil {
						t.Fatal(err)
					}
					if err := g.ValidateAnswerAt(""); err != nil {
						if !shape[name] {
							t.Fatalf("the graph closure refused a row the subject rule must decide: %v", err)
						}
						continue // the graph closure decides first
					}
					expected, _ := g.SubjectIdentity(g.Response, "Patient/SubscriberExample")
					r := pasGraphSubjectMismatch(g, expected)
					if read := r != nil && r.Identity == parentURL; read == party {
						t.Fatalf("the $submit answer's rule reads the parent as a patient %v, the sdk names it a party %v (%+v)", read, party, r)
					}
					compared[party]++
				}
				if !carried {
					t.Fatal("fixture: no scope of the answer carries the parent's entry")
				}
			})
		}
	}
	if reached[true] == 0 || reached[false] == 0 {
		t.Fatalf("the rows reach a party %d times and no party %d times: both must be reached", reached[true], reached[false])
	}
	// Every $submit row but the closure's is compared, a party and no party
	// among them.
	want := 0
	for name := range edits {
		if !shape[name] {
			want++
		}
	}
	if compared[true] == 0 || compared[false] == 0 || compared[true]+compared[false] != want {
		t.Fatalf("the $submit rule was compared on %d party rows and %d no-party rows, want %d rows with both kinds", compared[true], compared[false], want)
	}
}
