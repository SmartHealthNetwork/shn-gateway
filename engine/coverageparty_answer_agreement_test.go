package engine

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The PAS answer checks decide a Coverage's party by shnsdk.CoverageParty,
// for the contained list of a Coverage that is an entry of the answer: the
// $submit answer's subject rule (subjectMismatch), the inquiry answer's
// (inquiryPatientScope.collect) and the request graph a provider gateway
// completes from its system of record (retainPASRequestEvidence). These rows
// run each of them over every row of the shared table
// (testdata/coverageparty/rows.json), the row's Coverage made the answer's
// Coverage entry, as TestCoverageParty_FenceAgrees and
// TestCoverageParty_WalkersAgree run the fence and the walks.

// answerRowCoverage is the row's Coverage as the answer's own Coverage entry
// cov: the row's members replace the entry's, its id stays the entry's (the
// answer's other resources reference it), and the row's references to its
// patient and its payer name the answer's.
func answerRowCoverage(t *testing.T, r coveragePartyRow, cov map[string]any) {
	t.Helper()
	patient, _ := cov["beneficiary"].(map[string]any)["reference"].(string)
	payors, _ := cov["payor"].([]any)
	var payer string
	if len(payors) > 0 {
		payer, _ = payors[0].(map[string]any)["reference"].(string)
	}
	if patient == "" || payer == "" {
		t.Fatal("fixture: the answer's Coverage names no beneficiary or payor by reference")
	}
	row, _ := r.decode(t)
	renamed := renameRowReferences(row, map[string]string{"Patient/p1": patient, "Organization/payer": payer}).(map[string]any)
	for _, k := range []string{"subscriber", "policyHolder", "beneficiary", "payor", "relationship", "contained"} {
		delete(cov, k)
	}
	for k, v := range renamed {
		if k != "id" {
			cov[k] = v
		}
	}
}

// renameRowReferences returns v with every string value that is a key of
// names replaced by its value.
func renameRowReferences(v any, names map[string]string) any {
	switch x := v.(type) {
	case map[string]any:
		for k, c := range x {
			x[k] = renameRowReferences(c, names)
		}
	case []any:
		for i, c := range x {
			x[i] = renameRowReferences(c, names)
		}
	case string:
		if n, ok := names[x]; ok {
			return n
		}
	}
	return v
}

// answerRowOf returns answer with each of its Coverage entries made the row's
// Coverage.
func answerRowOf(t *testing.T, answer []byte, r coveragePartyRow) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(answer, &doc); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range answerEntries(doc) {
		if cov, _ := e["resource"].(map[string]any); cov != nil && cov["resourceType"] == "Coverage" {
			answerRowCoverage(t, r, cov)
			n++
		}
	}
	if n == 0 {
		t.Fatal("fixture: the answer carries no Coverage entry")
	}
	return mustJSON(t, doc)
}

// answerRowKind is what a row asks the answer checks.
type answerRowKind int

const (
	// rowOther: the resource asked about is no Patient (exactly so
	// spelled), so it is never read as a second patient.
	rowOther answerRowKind = iota
	// rowParty: the resource asked about is the party, and nothing else in
	// the Coverage names another patient, so the answer is accepted.
	rowParty
	// rowPartyBeside: the resource asked about is the party, beside a
	// contained Patient that is not (or a beneficiary naming one): the answer
	// is refused, but not for the party.
	rowPartyBeside
	// rowSecondPatient: a Patient that is no party, so the answer is
	// refused (by the subject rule or a guard before it).
	rowSecondPatient
)

// answerRowKindOf classifies a row by the rule and the row's own Coverage.
func answerRowKindOf(t *testing.T, r coveragePartyRow) answerRowKind {
	t.Helper()
	cov, list := r.decode(t)
	asked, _ := list[r.Contained].(map[string]any)
	if !r.Party {
		if asked["resourceType"] == "Patient" {
			return rowSecondPatient
		}
		return rowOther
	}
	if ben, _ := cov["beneficiary"].(map[string]any); ben["reference"] != "Patient/p1" {
		return rowPartyBeside
	}
	for j, c := range list {
		if o, _ := c.(map[string]any); j != r.Contained && o["resourceType"] == "Patient" && !shnsdk.CoverageParty(cov, o) {
			return rowPartyBeside
		}
	}
	return rowParty
}

// askedPath is the answer path of the resource the row asks about, under its
// Coverage entry.
func (r coveragePartyRow) askedPath() string { return "/contained/" + strconv.Itoa(r.Contained) }

// askedIdentity is the identity the inquiry walk gives the resource the row
// asks about, under its Coverage entry at owner, when it reads it as a
// patient.
func askedIdentity(t *testing.T, r coveragePartyRow, owner string) string {
	t.Helper()
	_, list := r.decode(t)
	asked, _ := list[r.Contained].(map[string]any)
	id, _ := asked["id"].(string)
	return owner + "#" + id
}

func TestCoverageParty_SubmitAnswerAgrees(t *testing.T) {
	kinds := map[answerRowKind]int{}
	for _, r := range sdkCoveragePartyRows(t) {
		t.Run(r.Name, func(t *testing.T) {
			kind := answerRowKindOf(t, r)
			kinds[kind]++
			got := pasResponseSubjectMismatch(answerRowOf(t, []byte(assemblyRealPending), r))
			switch kind {
			case rowParty:
				if got != nil {
					t.Fatalf("the party was refused: %s", got.Why)
				}
			case rowPartyBeside:
				if got == nil {
					t.Fatal("the Coverage names another patient beside the party, and was accepted")
				}
				if got.Path == r.askedPath() {
					t.Fatalf("the party was refused as a second patient: %s", got.Why)
				}
			case rowSecondPatient:
				if got == nil {
					t.Fatal("a contained Patient that is no party was accepted")
				}
			}
		})
	}
	requireAnswerRowKinds(t, kinds)
}

func TestCoverageParty_InquiryAnswerAgrees(t *testing.T) {
	for _, fixture := range []string{"pas-inquiry-response-2.0.json", "pas-inquiry-response-2.2.json"} {
		base := inquiryFixture(t, fixture)
		kinds := map[answerRowKind]int{}
		for _, r := range sdkCoveragePartyRows(t) {
			t.Run(fixture+"/"+r.Name, func(t *testing.T) {
				kind := answerRowKindOf(t, r)
				kinds[kind]++
				answer := answerRowOf(t, base, r)
				accepted := validatePASInquiryAnswer(answer).Status == 0 && consistentPASInquiryAnswerSubjects(answer)
				switch kind {
				case rowParty:
					if !accepted {
						t.Fatal("the party was refused")
					}
				case rowPartyBeside, rowSecondPatient:
					if accepted {
						t.Fatal("the Coverage names a patient that is no party, and was accepted")
					}
				}
				// The walk itself, in each scope: it reads the resource asked
				// about as a patient exactly when it is an exactly spelled
				// Patient that is no party.
				var doc any
				if err := json.Unmarshal(answer, &doc); err != nil {
					t.Fatal(err)
				}
				scopes := 0
				for _, bundle := range inquiryAnswerScopes(doc) {
					for _, e := range answerEntries(bundle) {
						res, _ := e["resource"].(map[string]any)
						if id, _ := res["id"].(string); id != "InsuranceExample" {
							continue
						}
						scopes++
						owner, _ := e["fullUrl"].(string)
						members := map[string]bool{}
						newInquiryPatientScope(bundle).collect(bundle, members)
						if read, want := members[askedIdentity(t, r, owner)], kind == rowSecondPatient; read != want {
							t.Fatalf("the walk read the resource asked about as a patient: %v, want %v (%v)", read, want, members)
						}
					}
				}
				if scopes == 0 {
					t.Fatal("fixture: no scope carries the row's Coverage")
				}
			})
		}
		requireAnswerRowKinds(t, kinds)
	}
}

func TestCoverageParty_RequestGraphAgrees(t *testing.T) {
	read := func(context.Context, string) ([]byte, bool, error) { return nil, false, nil }
	kinds := map[answerRowKind]int{}
	for _, r := range sdkCoveragePartyRows(t) {
		t.Run(r.Name, func(t *testing.T) {
			kind := answerRowKindOf(t, r)
			kinds[kind]++
			cov := map[string]any{"resourceType": "Coverage", "id": "cov", "status": "active",
				"beneficiary": map[string]any{"reference": "Patient/p"}, "payor": []any{map[string]any{"reference": "Organization/payer"}}}
			answerRowCoverage(t, r, cov)
			body := mustJSON(t, map[string]any{"resourceType": "Bundle", "type": "collection", "entry": []any{
				map[string]any{"fullUrl": "https://example.test/fhir/Patient/p", "resource": map[string]any{"resourceType": "Patient", "id": "p"}},
				map[string]any{"fullUrl": "https://example.test/fhir/Organization/payer", "resource": map[string]any{"resourceType": "Organization", "id": "payer", "name": "Payer"}},
				map[string]any{"fullUrl": "https://example.test/fhir/Coverage/cov", "resource": cov},
			}})
			_, err := retainPASRequestEvidence(context.Background(), body, read)
			switch kind {
			case rowParty:
				if err != nil {
					t.Fatalf("the party was refused: %v", err)
				}
			case rowPartyBeside, rowSecondPatient:
				if err == nil {
					t.Fatal("the Coverage names a patient that is no party, and was accepted")
				}
			}
		})
	}
	requireAnswerRowKinds(t, kinds)
}

// requireAnswerRowKinds fails unless the table reaches each kind of row: the
// agreement is not vacuous.
func requireAnswerRowKinds(t *testing.T, kinds map[answerRowKind]int) {
	t.Helper()
	for kind, least := range map[answerRowKind]int{rowParty: 6, rowPartyBeside: 2, rowSecondPatient: 30, rowOther: 3} {
		if kinds[kind] < least {
			t.Errorf("the table has %d rows of kind %d; want at least %d", kinds[kind], kind, least)
		}
	}
}

// Only a Coverage that is itself an entry of the answer has a party: an
// inquiry answer that is a lone Coverage, or a Parameters return that is one,
// is no Bundle, so the Coverage is no entry and the parent it contains is read
// as a patient. (The answer's shape check refuses both documents first; this
// is the walk's own reading.)
func TestCoverageParty_InquiryLoneCoverageHasNoParty(t *testing.T) {
	for _, r := range sdkCoveragePartyRows(t) {
		if r.Name != "subscriber only" {
			continue
		}
		cov, _ := r.decode(t)
		for name, doc := range map[string]any{
			"a lone Coverage":                        cov,
			"a Parameters return that is a Coverage": map[string]any{"resourceType": "Parameters", "parameter": []any{map[string]any{"name": "return", "resource": cov}}},
		} {
			t.Run(name, func(t *testing.T) {
				members, ok := pasInquiryAnswerSubjects(mustJSON(t, doc))
				if !ok {
					t.Fatal("the walk could not read the document")
				}
				if !members["Coverage/c#parent"] {
					t.Fatalf("the parent of a Coverage that is no entry was not read as a patient: %v", members)
				}
			})
		}
		// Control: the same Coverage as an entry has its party.
		members, ok := pasInquiryAnswerSubjects(mustJSON(t, map[string]any{"resourceType": "Bundle", "type": "collection",
			"entry": []any{map[string]any{"fullUrl": "https://example.test/fhir/Coverage/c", "resource": cov}}}))
		if !ok || members["https://example.test/fhir/Coverage/c#parent"] || len(members) != 1 {
			t.Fatalf("the control: the entry's party was read as a patient: %v", members)
		}
		return
	}
	t.Fatal("the table lost the row \"subscriber only\"")
}
