package engine

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The engine decides a Coverage's party by shnsdk.CoverageParty wherever it
// asks: the patient fence (resourceOnce), the involved list's walk
// (carriedMembers) and the subject derivation's walk (forEachCarriedPatient).
// These rows run each of those call sites over every row of the sdk's shared
// table. The module carries its own copy of the table
// (testdata/coverageparty/rows.json), as a published module must carry its
// own testdata; test/twinfence pins it byte-identical to the sdk's
// (sdk/testdata/coverageparty/rows.json), so a row added there is pinned here
// too.

// coveragePartyRow is one row of the sdk's table: the Coverage, the index of
// the contained resource asked about, and whether it is the party.
type coveragePartyRow struct {
	Name      string          `json:"name"`
	Coverage  json.RawMessage `json:"coverage"`
	Contained int             `json:"contained"`
	Party     bool            `json:"party"`
}

// sdkCoveragePartyRows reads this module's copy of the sdk's table.
func sdkCoveragePartyRows(t *testing.T) []coveragePartyRow {
	t.Helper()
	raw, err := os.ReadFile("testdata/coverageparty/rows.json")
	if err != nil {
		t.Fatalf("read the coverage party rows: %v", err)
	}
	var rows []coveragePartyRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("decode the sdk's coverage party rows: %v", err)
	}
	if len(rows) < 30 {
		t.Fatalf("the sdk's table has %d rows; it is not the table", len(rows))
	}
	seen := map[string]bool{}
	for _, r := range rows {
		if r.Name == "" || seen[r.Name] {
			t.Fatalf("row %q: rows are named, each once", r.Name)
		}
		seen[r.Name] = true
	}
	// The rows that pin where the rule differs from the engine's former copy
	// must be in the table.
	for _, name := range []string{"no id, the slot naming the Coverage itself", "a slot-named member on a resource that is not a Coverage",
		"a duplicate id: the copy that contains a resource", "a duplicate id: the other copy", "an id another contained resource also has"} {
		if !seen[name] {
			t.Fatalf("the sdk's table lost the row %q", name)
		}
	}
	return rows
}

// decodeRow decodes a fresh copy of the row's Coverage and returns it with
// its contained list.
func (r coveragePartyRow) decode(t *testing.T) (map[string]any, []any) {
	t.Helper()
	var cov map[string]any
	if err := json.Unmarshal(r.Coverage, &cov); err != nil {
		t.Fatalf("%s: %v", r.Name, err)
	}
	list, _ := cov["contained"].([]any)
	if r.Contained < 0 || r.Contained >= len(list) {
		t.Fatalf("%s: no contained resource %d", r.Name, r.Contained)
	}
	return cov, list
}

// addMark puts the fence's own mark on every object in v, as a fence run
// does on each resource it reaches.
func addMark(v any) {
	switch x := v.(type) {
	case map[string]any:
		for _, c := range x {
			addMark(c)
		}
		x[fenceMarkKey] = 0
	case []any:
		for _, c := range x {
			addMark(c)
		}
	}
}

// The rule itself, as the engine hands it its maps: a Coverage a fence run
// has marked (the mark is a member no JSON document can carry, its value an
// int) is decided as the unmarked one.
func TestCoverageParty_FenceMarkIsIgnored(t *testing.T) {
	for _, r := range sdkCoveragePartyRows(t) {
		t.Run(r.Name, func(t *testing.T) {
			cov, list := r.decode(t)
			cr, _ := list[r.Contained].(map[string]any)
			if got := shnsdk.CoverageParty(cov, cr); got != r.Party {
				t.Fatalf("party %v, want %v", got, r.Party)
			}
			addMark(cov)
			if _, ok := cov[fenceMarkKey].(int); !ok {
				t.Fatal("fixture: the Coverage carries no mark")
			}
			if got := shnsdk.CoverageParty(cov, cr); got != r.Party {
				t.Fatalf("marked: party %v, want %v", got, r.Party)
			}
		})
	}
}

// agreementMember is the bound member the fence rows bind; agreementOther
// is another member's identifier the asked-about Patient is given, so the
// fence refuses it unless it is the party.
const (
	agreementMember = "MBR-1"
	agreementOther  = "MBR-OTHER"
)

// fenceVariant is the row's Coverage with every other contained Patient
// made the bound patient (its member identifier the bound member's), and the
// Patient asked about also carrying another member's: the fence refuses it
// exactly when it checks its identity, so whether the fence took it for the
// party is what the two fences below tell apart. An identifier is only
// appended to a list or added where none is: a Patient whose identifier is
// not a list, or that spells identifier in another case, is left as it is
// (an identifier written over it, or beside it, would change what the rule
// and the fence read). A party's
// identity is not read by the rule, so no change moves a row's answer
// (TestCoverageParty_FenceVariantKeepsTheAnswer).
func fenceVariant(t *testing.T, r coveragePartyRow) []byte {
	t.Helper()
	cov, list := r.decode(t)
	for j, c := range list {
		cr, ok := c.(map[string]any)
		if !ok || cr["resourceType"] != "Patient" {
			continue
		}
		if j == r.Contained {
			appendIdentifier(cr, map[string]any{"system": shnsdk.MemberSystem, "value": agreementOther}, false)
			continue
		}
		appendIdentifier(cr, map[string]any{"system": shnsdk.MemberSystem, "value": agreementMember}, true)
	}
	b, err := json.Marshal(cov)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// appendIdentifier appends ident to p's identifier list, or adds the list
// when p has none, and leaves p as it is when its identifier is not a list or
// a member names identifier in another case. With replace, the list's member
// identifiers (shnsdk.MemberSystem) are dropped first.
func appendIdentifier(p, ident map[string]any, replace bool) {
	for k := range p {
		if k != "identifier" && strings.EqualFold(k, "identifier") {
			return
		}
	}
	raw, present := p["identifier"]
	ids, isList := raw.([]any)
	if present && !isList {
		return
	}
	var out []any
	for _, i := range ids {
		if m, _ := i.(map[string]any); replace && m["system"] == shnsdk.MemberSystem {
			continue
		}
		out = append(out, i)
	}
	p["identifier"] = append(out, ident)
}

// walkerVariant is the row's Coverage with a probe Patient nested inside the
// resource asked about: a walker that walks that resource reaches the probe,
// one that skips it as the party does not.
func walkerVariant(t *testing.T, r coveragePartyRow) map[string]any {
	t.Helper()
	cov, list := r.decode(t)
	cr, ok := list[r.Contained].(map[string]any)
	if !ok {
		t.Fatalf("%s: contained %d is not an object", r.Name, r.Contained)
	}
	cr["_probe"] = map[string]any{"resourceType": "Patient", "id": "zz-probe"}
	return cov
}

func TestCoverageParty_FenceVariantKeepsTheAnswer(t *testing.T) {
	for _, r := range sdkCoveragePartyRows(t) {
		t.Run(r.Name, func(t *testing.T) {
			for name, raw := range map[string][]byte{"fence": fenceVariant(t, r), "walker": mustJSON(t, walkerVariant(t, r))} {
				v := coveragePartyRow{Name: r.Name, Coverage: raw, Contained: r.Contained}
				cov, list := v.decode(t)
				cr, _ := list[r.Contained].(map[string]any)
				if got := shnsdk.CoverageParty(cov, cr); got != r.Party {
					t.Fatalf("%s variant: party %v, want %v", name, got, r.Party)
				}
			}
		})
	}
}

// sameRefusal reports whether two fence results are the same: both nil, or
// the same refusal.
func sameRefusal(a, b error) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	var ca, cb *CompartmentError
	if !errors.As(a, &ca) || !errors.As(b, &cb) {
		return a.Error() == b.Error()
	}
	return *ca == *cb
}

// The patient fence carries exactly the rows the rule names a party, and
// refuses every other row whose resource asked about is a Patient, by
// whichever of its guards refuses first: for every row that is no party the
// fence decides as the fence that carries no party does (withoutParties).
// Where the guard is fixed by the row, the reason is too (fenceNoPartyReason).
func TestCoverageParty_FenceAgrees(t *testing.T) {
	reasons := map[string]int{}
	for _, r := range sdkCoveragePartyRows(t) {
		t.Run(r.Name, func(t *testing.T) {
			value := fenceVariant(t, r)
			fence := newPatientFence(shnsdk.MemberSystem, agreementMember, nil, "p1")
			withParty := fence.check(value)
			withoutParty := fence.withoutParties().check(value)
			anotherPatient := &CompartmentError{ResourceType: "Patient", Reason: "another patient"}
			if r.Party {
				if withParty != nil {
					t.Fatalf("the party was refused: %v", withParty)
				}
				// Not vacuous: checked as a patient, the party is refused.
				if !sameRefusal(withoutParty, anotherPatient) {
					t.Fatalf("without the party rule: %v, want %v", withoutParty, anotherPatient)
				}
				return
			}
			if !sameRefusal(withParty, withoutParty) {
				t.Fatalf("the fence took a resource that is no party for one: %v, without the rule %v", withParty, withoutParty)
			}
			_, list := r.decode(t)
			cr, _ := list[r.Contained].(map[string]any)
			if rt, _ := cr["resourceType"].(string); strings.EqualFold(rt, "Patient") && withParty == nil {
				t.Fatal("the fence carried a contained Patient the rule names no party")
			}
			if want := fenceNoPartyReason(t, value, r.Contained); want != "" {
				var ce *CompartmentError
				if !errors.As(withParty, &ce) || ce.Reason != want {
					t.Fatalf("the fence: %v, want the refusal %q", withParty, want)
				}
				reasons[want]++
			}
		})
	}
	// Each guard the rows reach is reached: the reasons are not vacuous.
	for _, want := range []string{"not a readable JSON value", "contained resource without a unique id", "contained resource contains resources", "identifier is not a list", "another patient"} {
		if reasons[want] == 0 {
			t.Errorf("no row was refused %q", want)
		}
	}
}

// fenceNoPartyReason is the refusal the patient fence gives value, a fence
// variant of a row whose resource asked about (contained[i]) is no party,
// where the row fixes it, or "" where it does not (a resource that is no
// Patient, or one an earlier resource's refusal may precede). In the fence's
// order: a document that repeats a member name in any object, exactly or in
// another case, is unreadable; a contained list whose members do not each
// carry a unique id is refused; then the Patient asked about is refused for
// containing resources, for an identifier that is not a list, or as another
// patient.
func fenceNoPartyReason(t *testing.T, value []byte, i int) string {
	t.Helper()
	var cov map[string]any
	if err := json.Unmarshal(value, &cov); err != nil {
		t.Fatal(err)
	}
	if foldedDuplicateMember(cov) {
		return "not a readable JSON value"
	}
	list, _ := cov["contained"].([]any)
	seen := map[string]bool{}
	for _, c := range list {
		o, _ := c.(map[string]any)
		id, _ := o["id"].(string)
		if id == "" || seen[id] {
			return "contained resource without a unique id"
		}
		seen[id] = true
	}
	cr, _ := list[i].(map[string]any)
	if cr["resourceType"] != "Patient" {
		return ""
	}
	// An earlier contained resource is fenced first; the rows' others are
	// the bound patient or bound by it, so only the one asked about refuses.
	if _, nested := cr["contained"]; nested {
		return "contained resource contains resources"
	}
	if raw, ok := cr["identifier"]; ok {
		if _, ok := raw.([]any); !ok {
			return "identifier is not a list"
		}
	}
	return "another patient"
}

// foldedDuplicateMember reports whether any object in v has two members
// whose names are equal under case folding.
func foldedDuplicateMember(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		seen := map[string]bool{}
		for k, c := range x {
			if seen[strings.ToLower(k)] {
				return true
			}
			seen[strings.ToLower(k)] = true
			if foldedDuplicateMember(c) {
				return true
			}
		}
	case []any:
		for _, c := range x {
			if foldedDuplicateMember(c) {
				return true
			}
		}
	}
	return false
}

// The two walks that skip a party (the involved list's and the subject
// derivation's) skip exactly the resource the rule names, the Coverage alone
// or as a Bundle entry, on every leg: they reach the probe inside every
// other resource. A resource the rule names no party is walked, which can
// only add to what the walks find.
func TestCoverageParty_WalkersAgree(t *testing.T) {
	for _, r := range sdkCoveragePartyRows(t) {
		t.Run(r.Name, func(t *testing.T) {
			cov := walkerVariant(t, r)
			for shape, payload := range map[string][]byte{
				"resource": mustJSON(t, cov),
				"entry":    mustJSON(t, map[string]any{"resourceType": "Bundle", "type": "collection", "entry": []any{map[string]any{"fullUrl": "urn:uuid:c1", "resource": cov}}}),
			} {
				wantWalked := !r.Party
				for _, leg := range []string{"crd-order-select", "dtr-questionnaire-fetch", "pas-claim", "pas-claim-update", "pas-claim-inquire"} {
					walked := false
					for _, m := range carriedMembers(payload, leg) {
						walked = walked || m == "zz-probe"
					}
					if walked != wantWalked {
						t.Fatalf("%s, carriedMembers on %s: walked %v, want %v", shape, leg, walked, wantWalked)
					}
				}
				walked := false
				forEachCarriedPatient(payload, "zz-probe", func(p map[string]any) { walked = walked || p["id"] == "zz-probe" })
				if walked != wantWalked {
					t.Fatalf("%s, forEachCarriedPatient: walked %v, want %v", shape, walked, wantWalked)
				}
			}
		})
	}
}

// A contained that is not a list is walked whole, as before the rule: the
// rule answers only for a resource in a Coverage's contained list. The
// involved list names the object-shaped Patient, and the subject derivation
// reads it.
func TestCoverageParty_WalkersWalkAContainedThatIsNotAList(t *testing.T) {
	for name, coverage := range map[string]string{
		"an object":               `{"resourceType":"Coverage","id":"c","contained":{"resourceType":"Patient","id":"parent","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"zz-probe"}]},"subscriber":{"reference":"#parent"},"beneficiary":{"reference":"Patient/p1"}}`,
		"an object, both slots":   `{"resourceType":"Coverage","id":"c","contained":{"resourceType":"Patient","id":"parent","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"zz-probe"}]},"subscriber":{"reference":"#parent"},"policyHolder":{"reference":"#parent"},"beneficiary":{"reference":"Patient/p1"}}`,
		"a list inside an object": `{"resourceType":"Coverage","id":"c","contained":{"x":[{"resourceType":"Patient","id":"parent","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"zz-probe"}]}]},"subscriber":{"reference":"#parent"},"beneficiary":{"reference":"Patient/p1"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			for _, leg := range []string{"crd-order-select", "pas-claim-inquire"} {
				got := carriedMembers([]byte(coverage), leg)
				found := false
				for _, m := range got {
					found = found || m == "parent"
				}
				if !found {
					t.Fatalf("%s: carriedMembers %v does not name the walked Patient", leg, got)
				}
			}
			if !carriesPatient([]byte(coverage), "zz-probe") {
				t.Fatal("forEachCarriedPatient skipped an object-shaped contained Patient")
			}
		})
	}
}

// A row whose asked-about resource shares its id with another contained
// resource names no party (shnsdk.CoverageParty: "#<id>" cannot say which it
// names), so the engine reads it as it read every contained resource before
// the rule: the patient fence refuses the Coverage for its duplicate id, and
// the involved list's and subject derivation's walks walk both copies, which
// can only add to what they find. The rows include a copy that contains
// another member's Patient: a walk that took either copy for the party would
// miss it.
func TestCoverageParty_DuplicateIDRows(t *testing.T) {
	n := 0
	for _, r := range sdkCoveragePartyRows(t) {
		cov, list := r.decode(t)
		cr, _ := list[r.Contained].(map[string]any)
		id, ok := cr["id"].(string)
		if !ok || id == "" {
			continue
		}
		same := 0
		for _, c := range list {
			if o, ok := c.(map[string]any); ok && o["id"] == id {
				same++
			}
		}
		if same < 2 {
			continue
		}
		n++
		t.Run(r.Name, func(t *testing.T) {
			if r.Party {
				t.Fatal("the table names a resource sharing its id the party")
			}
			if shnsdk.CoverageParty(cov, cr) {
				t.Fatal("shnsdk.CoverageParty took a resource sharing its id for the party")
			}
			fence := newPatientFence(shnsdk.MemberSystem, agreementMember, nil, "p1")
			var ce *CompartmentError
			if err := fence.check(fenceVariant(t, r)); !errors.As(err, &ce) || ce.Reason != "contained resource without a unique id" {
				t.Fatalf("the fence: %v, want a refusal for the duplicate id", err)
			}
			payload := mustJSON(t, walkerVariant(t, r))
			walked := false
			for _, m := range carriedMembers(payload, "pas-claim") {
				walked = walked || m == "zz-probe"
			}
			if !walked {
				t.Fatal("carriedMembers skipped a resource sharing its id")
			}
			walked = false
			forEachCarriedPatient(payload, "zz-probe", func(p map[string]any) { walked = walked || p["id"] == "zz-probe" })
			if !walked {
				t.Fatal("forEachCarriedPatient skipped a resource sharing its id")
			}
			if cr["resourceType"] == "Patient" {
				// Both copies are walked: a Patient either contains is named
				// (as a contained Patient, "#<id>").
				members := carriedMembers(mustJSON(t, cov), "pas-claim")
				for _, c := range list {
					o, _ := c.(map[string]any)
					if o["resourceType"] != "Patient" {
						continue
					}
					for _, inner := range anyList(o["contained"]) {
						im, _ := inner.(map[string]any)
						if im["resourceType"] != "Patient" {
							continue
						}
						found := false
						for _, m := range members {
							found = found || m == "#"+im["id"].(string)
						}
						if !found {
							t.Fatalf("carriedMembers %v does not name the Patient %v a copy contains", members, im["id"])
						}
					}
				}
			}
		})
	}
	if n < 3 {
		t.Fatalf("the sdk's table has %d rows asking about a resource that shares its id; want at least 3", n)
	}
}

// anyList is v as a list, or nil.
func anyList(v any) []any {
	l, _ := v.([]any)
	return l
}
