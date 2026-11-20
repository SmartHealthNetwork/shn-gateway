//go:build !shn_coreonly

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The parent a dependent's Coverage names in its subscriber and policyHolder,
// carrying the member's member identifier, and the member's own Patient.
const (
	exactParent = `{"resourceType":"Patient","id":"parent","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"` + strangerMember + `"}],"name":[{"family":"Parentname"}],"birthDate":"1970-01-01"}`
	exactChild  = `{"resourceType":"Patient","id":"` + strangerMember + `","name":[{"family":"Childname"}],"birthDate":"2015-06-01"}`
)

// partyCoverage is the dependent's Coverage with its contained resources
// under the member name contained.
func partyCoverage(contained string) string {
	return `{"resourceType":"Coverage","id":"c",` + contained +
		`,"subscriber":{"reference":"#parent"},"policyHolder":{"reference":"#parent"},"beneficiary":{"reference":"Patient/` + strangerMember + `"}}`
}

// unclearPartyCoverages are dependent Coverages carrying the parent exactly,
// each spelling a name the party rule reads in another case. The binding reads
// a FHIR resource's names as FHIR spells them (bindingView), so where the name
// is one the view spells (a slot's reference, the Coverage's type) the rule
// names the parent as the exact spelling does; where it is not (a slot only a
// lenient reader reads beside the exact one, a parent containing others) the
// rule names no party, though a reader that ignores case finds the parent a
// party.
var unclearPartyCoverages = map[string]string{
	"a slot named in another case beside the exact one": `{"resourceType":"Coverage","id":"c","contained":[` + exactParent + `],` +
		`"policyHolder":{"reference":"#parent"},"Subscriber":{"display":"x"},"beneficiary":{"reference":"Patient/` + strangerMember + `"}}`,
	"the parent containing a list named in another case": partyCoverage(`"contained":[` + strings.Replace(exactParent, `"id":"parent"`, `"id":"parent","Contained":[]`, 1) + `]`),
	"a slot naming its reference in another case":        strings.Replace(partyCoverage(`"contained":[`+exactParent+`]`), `"policyHolder":{"reference":"#parent"}`, `"policyHolder":{"Reference":"#parent"}`, 1),
	"the Coverage naming its type in another case":       strings.Replace(partyCoverage(`"contained":[`+exactParent+`]`), `"resourceType":"Coverage"`, `"ResourceType":"Coverage"`, 1),
}

// spellsTieName are the unclearPartyCoverages that spell, in another case, a
// name a reference is resolved by that the binding's view does not spell as
// FHIR does: the parent's "Contained" is read as contained, so the parent
// contains resources, which readers may resolve differently. The others spell
// a name the view spells as FHIR does, so they read as the exact spelling does.
var spellsTieName = map[string]bool{
	"the parent containing a list named in another case": true,
}

// wantUnreadable is wantNoParent's want for a request the binding cannot decode at
// all: refused (errRequestPatientsUnreadable), never bound by the id alone.
const wantUnreadable = "unreadable"

// The subject derivation reads a carried Patient's demographics through the
// binding's view of the request (bindingView), in which each name the readers
// read, and each type they compare, takes its FHIR spelling within a FHIR
// resource: a Coverage's contained resources named in another case are its
// contained resources, so the parent there is its party, as when they are
// named exactly, and is never read as the member. The demographics themselves
// are read by their exact names (PatientDemographics): a Patient naming its
// birth date or name only in another case yields none. The contained resources
// of a Coverage whose party the party rule cannot name, because it spells in
// another case a name the view does not spell, yield none. A request repeating
// a member name in a resource, exactly or in another case, can be read two ways:
// the binding cannot decode it, and refuses it rather than bind by the id alone. Every leg that derives a patient identifier reads it through
// this derivation. A request spelling a name a reference is resolved by in
// another case is one readers may resolve differently: no beneficiary ties the
// member's own Patient there, so beside a party slot it is not decided to be
// the member, and the request is refused rather than read.
func TestSubjectDerivation_ReadsCarriedPatientsByExactNames(t *testing.T) {
	none := derivedPCI(strangerMember, "", "")
	childs := derivedPCI(strangerMember, "2015-06-01", "Childname")
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	for name, row := range map[string]struct {
		payload string
		want    string
	}{
		"the party under Contained":                       {`{"prefetch":{"coverage":` + partyCoverage(`"Contained":[`+exactParent+`]`) + `}}`, ""},
		"the party under Contained, with the child":       {`{"prefetch":{"patient":` + exactChild + `,"coverage":` + partyCoverage(`"Contained":[`+exactParent+`]`) + `}}`, childs},
		"the party under CONTAINED, in a searchset":       {`{"prefetch":{"coverage":{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + partyCoverage(`"CONTAINED":[`+exactParent+`]`) + `}]}}}`, ""},
		"contained repeated":                              {`{"prefetch":{"patient":` + exactChild + `,"coverage":` + partyCoverage(`"contained":[`+exactParent+`],"contained":[]`) + `}}`, wantUnreadable},
		"contained repeated in another case":              {`{"prefetch":{"patient":` + exactChild + `,"coverage":` + partyCoverage(`"contained":[],"Contained":[`+exactParent+`]`) + `}}`, wantUnreadable},
		"the child naming its birth date in another case": {`{"prefetch":{"patient":` + strings.Replace(exactChild, `"birthDate"`, `"BirthDate"`, 1) + `}}`, none},
		"the child naming its name in another case":       {`{"prefetch":{"patient":` + strings.Replace(exactChild, `"name"`, `"Name"`, 1) + `}}`, none},
		"the child naming its family in another case":     {`{"prefetch":{"patient":` + strings.Replace(exactChild, `"family"`, `"Family"`, 1) + `}}`, none},
		"the child, read exactly":                         {`{"prefetch":{"patient":` + exactChild + `,"coverage":` + partyCoverage(`"contained":[`+exactParent+`]`) + `}}`, childs},
	} {
		t.Run(name, func(t *testing.T) {
			wantNoParent(t, g, row.payload, row.want)
		})
	}
	// The parent alone carries the member's number with no Patient of the
	// member's own: the member cannot be identified ("" is refused). With the
	// member's own Patient, it is read, unless the request spells a name a
	// reference is resolved by in another case.
	for name, coverage := range unclearPartyCoverages {
		withChild := childs
		if spellsTieName[name] {
			withChild = ""
		}
		t.Run(name, func(t *testing.T) {
			wantNoParent(t, g, `{"prefetch":{"coverage":`+coverage+`}}`, "")
			wantNoParent(t, g, `{"prefetch":{"patient":`+exactChild+`,"coverage":`+coverage+`}}`, withChild)
		})
	}
}

// wantNoParent checks that the derivation reads no parent's demographics from
// payload, and derives want, or, with want "", refuses: the member cannot be
// identified, or, with wantUnreadable, refuses: the request cannot be read.
func wantNoParent(t *testing.T, g *Gateway, payload, want string) {
	t.Helper()
	if d, ok := carriedPatientDemographics([]byte(payload), strangerMember); ok && d.FamilyName == "Parentname" {
		t.Fatalf("the parent's demographics were read: %+v", d)
	}
	pci, found, err := g.resolveSubjectPCI(context.Background(), strangerMember, []byte(payload))
	if want == wantUnreadable {
		if !errors.Is(err, errRequestPatientsUnreadable) || pci != "" {
			t.Fatalf("pci %q (found %v, err %v), want the request refused as unreadable", pci, found, err)
		}
		return
	}
	if want == "" {
		if !errors.Is(err, errPatientUnidentifiable) || pci != "" {
			t.Fatalf("pci %q (found %v, err %v), want the patient refused as unidentifiable", pci, found, err)
		}
		return
	}
	if err != nil || !found || pci != want {
		t.Fatalf("pci %q (found %v, err %v), want %q", pci, found, err, want)
	}
}

// PatientDemographics is the one read of a Patient's demographics, a holder's
// own record's included: by exact member names, with no member repeated.
func TestPatientDemographics_ExactNames(t *testing.T) {
	if d, ok := PatientDemographics([]byte(exactChild)); !ok || d != (Demo{BirthDate: "2015-06-01", FamilyName: "Childname"}) {
		t.Fatalf("an exactly named Patient: %+v %v", d, ok)
	}
	for name, patient := range map[string]string{
		"resourceType in another case":       strings.Replace(exactChild, `"resourceType"`, `"ResourceType"`, 1),
		"birthDate in another case":          strings.Replace(exactChild, `"birthDate"`, `"BIRTHDATE"`, 1),
		"name in another case":               strings.Replace(exactChild, `"name"`, `"Name"`, 1),
		"family in another case":             strings.Replace(exactChild, `"family"`, `"Family"`, 1),
		"birthDate repeated":                 strings.Replace(exactChild, `"birthDate":"2015-06-01"`, `"birthDate":"2015-06-01","birthDate":"1970-01-01"`, 1),
		"birthDate repeated in another case": strings.Replace(exactChild, `"birthDate":"2015-06-01"`, `"birthDate":"2015-06-01","BirthDate":"1970-01-01"`, 1),
	} {
		if d, ok := PatientDemographics([]byte(patient)); ok {
			t.Errorf("%s: demographics %+v, want none", name, d)
		}
	}
}

// On a leg that derives a patient identifier, a dependent's Coverage carrying
// its parent under contained resources named in another case, or with a
// party the rule cannot name, never binds the parent. Contained in the
// eligibility request beside the member's own Patient, the Coverage nests
// contained resources, which readers may resolve differently: the member's
// own Patient is not decided to be the member, and the request is refused as
// a patient the gateway cannot identify, at the provider's coverage
// eligibility ingress and on the payer's forward path, at every level.
func TestEligibility_PartyReadTwoWaysIsNeverTheMember(t *testing.T) {
	t.Parallel()
	coverages := map[string]string{"the party under Contained": partyCoverage(`"Contained":[` + exactParent + `]`)}
	for name, coverage := range unclearPartyCoverages {
		coverages[name] = coverage
	}
	childs := derivedPCI(strangerMember, "2015-06-01", "Childname")
	for name, coverage := range coverages {
		request := func(t *testing.T) []byte {
			var doc map[string]any
			if err := json.Unmarshal(eligibilityRequest(t, strangerMember), &doc); err != nil {
				t.Fatal(err)
			}
			var cov, child map[string]any
			// Contained beside the member's own Patient, the Coverage names it
			// as a contained resource is named: by "#id".
			beneficiary := `"beneficiary":{"reference":"Patient/` + strangerMember + `"}`
			if !strings.Contains(coverage, beneficiary) {
				t.Fatal("fixture: the Coverage names no beneficiary by type and id")
			}
			local := strings.Replace(coverage, beneficiary, `"beneficiary":{"reference":"#`+strangerMember+`"}`, 1)
			if err := json.Unmarshal([]byte(local), &cov); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(exactChild), &child); err != nil {
				t.Fatal(err)
			}
			doc["contained"] = []any{child, cov}
			return mustJSON(t, doc)
		}
		for _, level := range eligibilityLevels {
			t.Run(name+"/provider/"+level.String(), func(t *testing.T) {
				env, rec, _ := runEligibilityIngress(t, level, request(t), payersEligibilityAnswer("Patient/"+strangerMember))
				if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), msgPatientUnidentifiable) {
					t.Fatalf("answer %d %s, want 400 %q", rec.Code, rec.Body, msgPatientUnidentifiable)
				}
				if got := env.substrate.authorizedSubjects; len(got) != 0 {
					t.Fatalf("leg authorized for %v, want none", got)
				}
			})
			t.Run(name+"/payer forward/"+level.String(), func(t *testing.T) {
				p := eligibilityPayer(t, level, true)
				p.partner.respByPath[eligibilityPath] = payersEligibilityAnswer("Patient/" + strangerMember)
				run := runObserved(t, p, "coverage-eligibility", request(t), childs)
				if run.answer.status != http.StatusBadRequest || !strings.Contains(string(run.answer.body), msgPatientUnidentifiable) {
					t.Fatalf("answer %d %s, want 400 %q", run.answer.status, run.answer.body, msgPatientUnidentifiable)
				}
			})
		}
	}
}
