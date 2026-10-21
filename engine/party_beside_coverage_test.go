package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The parent of a dependent the system of record does not hold: another
// person, under the member's own (family-level) member number.
const partyParentID = "parent-1"

func partyParent() map[string]any {
	return map[string]any{"resourceType": "Patient", "id": partyParentID,
		"identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}},
		"name":       []any{map[string]any{"family": "Parentname"}}, "birthDate": "1970-01-01"}
}

// parentIdentity is the identifier the parent's own request derives: the
// member number with the parent's birth date and name.
func parentIdentity() string { return derivedPCI(strangerMember, "1970-01-01", "Parentname") }

// namingParty sets the party slots of every Coverage in node to ref, in the
// form slot gives (a Reference, or a list of them), and reports how many it set.
func namingParty(node any, ref string, slot func(ref string) any) int {
	n := 0
	switch v := node.(type) {
	case map[string]any:
		if v["resourceType"] == "Coverage" {
			v["subscriber"], v["policyHolder"] = slot(ref), slot(ref)
			n++
		}
		for _, child := range v {
			n += namingParty(child, ref, slot)
		}
	case []any:
		for _, child := range v {
			n += namingParty(child, ref, slot)
		}
	}
	return n
}

// cerNaming is the member's eligibility request naming its patient by ref,
// with contained carried as its contained resources.
func cerNaming(t *testing.T, ref string, contained ...any) map[string]any {
	t.Helper()
	doc := decodeDoc(t, eligibilityRequest(t, strangerMember))
	doc["patient"] = map[string]any{"reference": ref}
	doc["contained"] = contained
	return doc
}

// renamedID is r with the id id.
func renamedID(r map[string]any, id string) map[string]any {
	r["id"] = id
	return r
}

// childPatient is the dependent's own Patient, under the member's number.
func childPatient() map[string]any {
	return map[string]any{"resourceType": "Patient", "id": strangerMember,
		"identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}},
		"name":       []any{map[string]any{"family": "Childname"}}, "birthDate": "2015-06-01"}
}

// withPersonID is r with an identifier in a system of its own beside its
// member number.
func withPersonID(r map[string]any) map[string]any {
	ids, _ := r["identifier"].([]any)
	r["identifier"] = append(ids, map[string]any{"system": "urn:example:person-ids", "value": "P-1"})
	return r
}

// laterVersion is r at version 2, adding an identifier and a given name.
func laterVersion(r map[string]any) map[string]any {
	r = withMeta(r, "2")
	ids, _ := r["identifier"].([]any)
	r["identifier"] = append(ids, map[string]any{"system": "http://hl7.org/fhir/sid/us-ssn", "value": "999-00-0001"})
	r["name"] = []any{map[string]any{"family": "Childname", "given": []any{"Kid"}}}
	return r
}

// withoutBirthDate is r with no birth date.
func withoutBirthDate(r map[string]any) map[string]any {
	delete(r, "birthDate")
	return r
}

// withMeta is r at version versionID.
func withMeta(r map[string]any, versionID string) map[string]any {
	r["meta"] = map[string]any{"versionId": versionID}
	return r
}

// withoutIdentifier is r with no identifier.
func withoutIdentifier(r map[string]any) map[string]any {
	delete(r, "identifier")
	return r
}

// withoutID is r with no id.
func withoutID(r map[string]any) map[string]any {
	delete(r, "id")
	return r
}

// renamed is r with its member from renamed to.
func renamed(r map[string]any, from, to string) map[string]any {
	r[to] = r[from]
	delete(r, from)
	return r
}

func reference(ref string) any     { return map[string]any{"reference": ref} }
func referenceList(ref string) any { return []any{map[string]any{"reference": ref}} }
func decodeDoc(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// withParentParty is body with every Coverage naming the parent in its party
// slots by ref, and the parent added by add.
func withParentParty(t *testing.T, body []byte, ref string, slot func(string) any, add func(doc map[string]any, parent map[string]any)) []byte {
	t.Helper()
	doc := decodeDoc(t, body)
	if namingParty(doc, ref, slot) == 0 {
		t.Fatal("fixture: the request carries no Coverage")
	}
	add(doc, partyParent())
	return mustJSON(t, doc)
}

// partyForm names the parent in a Coverage's party slots, given how the leg
// refers to the parent (ref).
type partyForm func(coverage map[string]any, ref string)

// partyForms are the ways a Coverage names its party.
var partyForms = map[string]partyForm{
	"subscriber and policyholder by reference": func(c map[string]any, ref string) {
		c["subscriber"], c["policyHolder"] = reference(ref), reference(ref)
	},
	// Appended: a Coverage's payor list keeps the payers it names.
	"payor by reference": func(c map[string]any, ref string) {
		list, _ := c["payor"].([]any)
		c["payor"] = append(list, reference(ref))
	},
	"subscriber by identifier": func(c map[string]any, _ string) {
		c["subscriber"] = map[string]any{"identifier": map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}}
	},
	// A reference nested in the beneficiary names no beneficiary.
	"subscriber, and an extension of the beneficiary, by reference": func(c map[string]any, ref string) {
		c["subscriber"] = reference(ref)
		b, _ := c["beneficiary"].(map[string]any)
		if b == nil {
			b = map[string]any{}
			c["beneficiary"] = b
		}
		b["extension"] = []any{map[string]any{"url": "http://example.org/x", "valueReference": reference(ref)}}
	},
}

// withParentNamedAs is body with every Coverage naming the parent in form,
// and the parent added by add.
func withParentNamedAs(t *testing.T, body []byte, form partyForm, ref string, add func(doc map[string]any, parent map[string]any)) []byte {
	t.Helper()
	doc := decodeDoc(t, body)
	n := 0
	var walk func(node any)
	walk = func(node any) {
		switch v := node.(type) {
		case map[string]any:
			if v["resourceType"] == "Coverage" {
				form(v, ref)
				n++
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(doc)
	if n == 0 {
		t.Fatal("fixture: the request carries no Coverage")
	}
	add(doc, partyParent())
	return mustJSON(t, doc)
}

// asContainedSibling carries the parent in the resource's own contained list,
// beside the Coverage that names it.
func asContainedSibling(doc map[string]any, parent map[string]any) {
	list, _ := doc["contained"].([]any)
	doc["contained"] = append(list, parent)
}

// asBundleEntry carries the parent as an entry of the Bundle.
func asBundleEntry(fullURL string) func(map[string]any, map[string]any) {
	return func(doc map[string]any, parent map[string]any) {
		list, _ := doc["entry"].([]any)
		doc["entry"] = append(list, map[string]any{"fullUrl": fullURL, "resource": parent})
	}
}

// asParameter carries the parent as a parameter of its own.
func asParameter(doc map[string]any, parent map[string]any) {
	list, _ := doc["parameter"].([]any)
	doc["parameter"] = append(list, map[string]any{"name": "parent", "resource": parent})
}

// asPrefetch carries the parent as a prefetch value of its own.
func asPrefetch(doc map[string]any, parent map[string]any) {
	prefetch, _ := doc["prefetch"].(map[string]any)
	prefetch["parent"] = parent
}

// wantPartyBinding checks a leg's binding of a request carrying the parent as
// a party: never the parent's identity; the member's own (want), or, with
// want "", refused as a patient the request cannot identify.
func wantPartyBinding(t *testing.T, side string, pci string, status int, msg, want string) {
	t.Helper()
	if pci == parentIdentity() {
		t.Fatalf("%s: bound to the parent's identity", side)
	}
	if want == "" {
		if status != http.StatusBadRequest || msg != msgPatientUnidentifiable {
			t.Fatalf("%s: status %d %q (pci %q), want 400 %q", side, status, msg, pci, msgPatientUnidentifiable)
		}
		return
	}
	if status != 0 || pci != want {
		t.Fatalf("%s: status %d %q, pci %q, want bound to the member's own %s", side, status, msg, pci, want)
	}
}

// The shapes a request carries a Coverage's party in, read as a FHIR reader
// resolves the Coverage's reference: never the member. A request carrying the
// member's own Patient derives from it; one carrying none, whose member number
// a party carries, cannot identify the member and is refused. A party that is
// the Coverage's own beneficiary is the member; a request naming no party, and
// a member the system of record holds, are bound as before.
func TestPartyBesideCoverage_Derivation(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	childs := derivedPCI(strangerMember, "2015-06-01", "Childname")
	child := map[string]any{"resourceType": "Patient", "id": strangerMember, "name": []any{map[string]any{"family": "Childname"}}, "birthDate": "2015-06-01"}
	cer := func(t *testing.T, member string, contained ...any) map[string]any {
		doc := decodeDoc(t, eligibilityRequest(t, member))
		doc["contained"] = contained
		return doc
	}
	coverage := func(beneficiary string, slot func(string) any, ref string) map[string]any {
		return map[string]any{"resourceType": "Coverage", "id": "cov", "status": "active",
			"subscriber": slot(ref), "policyHolder": slot(ref), "beneficiary": reference(beneficiary)}
	}
	parentContaining := partyParent()
	parentContaining["contained"] = []any{}
	parentContaining["id"] = "parent"
	inline := partyParent()
	inline["id"] = "parent"
	bundle := func(entries ...any) map[string]any {
		return map[string]any{"resourceType": "Bundle", "type": "collection", "entry": entries}
	}
	entry := func(fullURL string, r any) any { return map[string]any{"fullUrl": fullURL, "resource": r} }
	for name, row := range map[string]struct {
		payload any
		want    string
	}{
		"beside its Coverage":                    {cer(t, strangerMember, coverage("Patient/"+strangerMember, reference, "#parent"), inline), ""},
		"beside its Coverage, with the child":    {cer(t, strangerMember, coverage("#"+strangerMember, reference, "#parent"), inline, child), childs},
		"beside its Coverage, a slot as a list":  {cer(t, strangerMember, coverage("Patient/"+strangerMember, referenceList, "#parent"), inline), ""},
		"beside its Coverage, containing a list": {cer(t, strangerMember, coverage("Patient/"+strangerMember, reference, "#parent"), parentContaining), ""},
		"a Bundle entry by fullUrl": {bundle(entry("urn:uuid:cov", coverage("Patient/"+strangerMember, reference, "urn:uuid:parent")),
			entry("urn:uuid:parent", partyParent())), ""},
		"a Bundle entry by its id": {bundle(entry("urn:uuid:cov", coverage("Patient/"+strangerMember, reference, "Patient/"+partyParentID)),
			entry("http://ehr.example/fhir/Patient/"+partyParentID, partyParent())), ""},
		"by an absolute reference, with the child": {bundle(entry("urn:uuid:cov", coverage("Patient/"+strangerMember, reference, "http://ehr.example/fhir/Patient/"+partyParentID)),
			entry("urn:uuid:p", partyParent()), entry("urn:uuid:c", child)), childs},
		"a Bundle entry with no id, by its RESTful fullUrl, versioned": {bundle(entry("urn:uuid:cov", coverage("Patient/"+strangerMember, reference, "Patient/"+partyParentID+"/_history/3")),
			entry("http://ehr.example/fhir/Patient/"+partyParentID, withoutID(partyParent()))), ""},
		"the slots named in another case": {bundle(entry("urn:uuid:cov", renamed(renamed(coverage("Patient/"+strangerMember, reference, "Patient/"+partyParentID), "subscriber", "Subscriber"), "policyHolder", "PolicyHolder")),
			entry("urn:uuid:parent", partyParent())), ""},
		"a slot's reference named in another case": {bundle(entry("urn:uuid:cov", coverage("Patient/"+strangerMember, func(ref string) any { return map[string]any{"Reference": ref} }, "Patient/"+partyParentID)),
			entry("urn:uuid:parent", partyParent())), ""},
		"the Coverage naming its type in another case": {bundle(entry("urn:uuid:cov", renamed(coverage("Patient/"+strangerMember, reference, "Patient/"+partyParentID), "resourceType", "ResourceType")),
			entry("urn:uuid:parent", partyParent())), ""},
		"a self-paying parent named as payor": {bundle(entry("urn:uuid:cov", map[string]any{"resourceType": "Coverage", "id": "cov", "status": "active",
			"beneficiary": reference("Patient/" + strangerMember), "payor": []any{reference("Patient/" + partyParentID)}}),
			entry("urn:uuid:parent", partyParent())), ""},
		"a subscriber by identifier": {bundle(entry("urn:uuid:cov", coverage("Patient/"+strangerMember, func(string) any {
			return map[string]any{"identifier": map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}}
		}, "")), entry("urn:uuid:parent", partyParent())), ""},
		// The member's own Patient under another id, tied to the member by the
		// Coverage's beneficiary, is the member though a slot names its identifier.
		"a subscriber by identifier, the member's own Patient its beneficiary names": {bundle(
			entry("urn:uuid:cov", coverage("Patient/kid", func(string) any {
				return map[string]any{"identifier": map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}}
			}, "")), entry("urn:uuid:parent", partyParent()),
			entry("urn:uuid:kid", map[string]any{"resourceType": "Patient", "id": "kid",
				"identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}},
				"name":       []any{map[string]any{"family": "Childname"}}, "birthDate": "2015-06-01"})), childs},
		// A Patient tied to the member by nothing but an id equal to the member
		// id is not the member's own when a slot names its identifier: the member
		// may have been read from an identifier, not that id (a Coverage's
		// beneficiary, which FHIR requires, is what ties the member's own).
		"a subscriber by identifier, the member's own Patient tied by its id alone": {bundle(
			entry("urn:uuid:cov", map[string]any{"resourceType": "Coverage", "id": "cov", "status": "active",
				"subscriber": map[string]any{"identifier": map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}}}),
			entry("urn:uuid:parent", partyParent()),
			entry("urn:uuid:kid", map[string]any{"resourceType": "Patient", "id": strangerMember,
				"identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}},
				"name":       []any{map[string]any{"family": "Childname"}}, "birthDate": "2015-06-01"})), ""},
		// An identifier is a system and a value: one in another system names
		// another identifier, so the member's own Patient is no party.
		"a subscriber by an identifier in another system": {bundle(
			entry("urn:uuid:cov", map[string]any{"resourceType": "Coverage", "id": "cov", "status": "active",
				"subscriber": map[string]any{"identifier": map[string]any{"system": "urn:example:subscriber-ids", "value": strangerMember}}}),
			entry("urn:uuid:kid", map[string]any{"resourceType": "Patient", "id": "kid",
				"identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}},
				"name":       []any{map[string]any{"family": "Childname"}}, "birthDate": "2015-06-01"})), childs},
		"a subscriber by an identifier with no system": {bundle(entry("urn:uuid:cov", coverage("Patient/"+strangerMember, func(string) any {
			return map[string]any{"identifier": map[string]any{"value": strangerMember}}
		}, "")), entry("urn:uuid:parent", partyParent())), ""},
		"a subscriber by a list of identifiers, named in another case": {bundle(entry("urn:uuid:cov", coverage("Patient/"+strangerMember, func(string) any {
			return map[string]any{"Identifier": []any{map[string]any{"System": shnsdk.MemberSystem, "Value": strangerMember}}}
		}, "")), entry("urn:uuid:parent", partyParent())), ""},
		// Only the beneficiary of the Coverage whose slot names the identifier
		// ties a Patient to the member: the parent's own Coverage is no tie.
		"a subscriber by identifier, the parent the beneficiary of another Coverage": {bundle(
			entry("urn:uuid:own", map[string]any{"resourceType": "Coverage", "id": "own", "beneficiary": reference("Patient/" + partyParentID), "subscriber": reference("Patient/" + partyParentID)}),
			entry("urn:uuid:cov", coverage("Patient/dep", func(string) any {
				return map[string]any{"identifier": map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}}
			}, "")), entry("urn:uuid:parent", partyParent())), ""},
		// A self-subscriber written in two forms: the member's own record, by
		// its id, is the member.
		"a self-subscriber by fullUrl and by id": {bundle(entry("urn:uuid:cov", coverage("Patient/"+strangerMember, reference, "urn:uuid:self")),
			entry("urn:uuid:self", map[string]any{"resourceType": "Patient", "id": strangerMember,
				"name": []any{map[string]any{"family": "Childname"}}, "birthDate": "2015-06-01"})), childs},
		// The ordinary dependent: the parent carries no member number.
		"a dependent's Coverage naming a parent of no member number": {bundle(entry("urn:uuid:cov", coverage("Patient/"+strangerMember, reference, "Patient/"+partyParentID)),
			entry("urn:uuid:parent", withoutIdentifier(partyParent())), entry("urn:uuid:c", child)), childs},
		// A contained resource's id is local, never a member id: a parent
		// contained beside the Coverage under the member's id is still its
		// party, as one contained in the Coverage is.
		"beside its Coverage, under the member's id": {cer(t, strangerMember, coverage("Patient/"+strangerMember, reference, "#"+strangerMember),
			renamedID(partyParent(), strangerMember)), ""},
		// The request names its patient "#pt", whose member identifier is the
		// member's: the member is read from that identifier, so a parent entry
		// whose id equals the member is no record the request names, and the
		// subscriber reference names it as the party.
		"a contained patient, the parent an entry under the member's id": {bundle(
			entry("urn:uuid:cer", cerNaming(t, "#pt",
				map[string]any{"resourceType": "Patient", "id": "pt", "identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}}},
				coverage("#pt", reference, "Patient/"+strangerMember))),
			entry("http://ehr.example/fhir/Patient/"+strangerMember, renamedID(partyParent(), strangerMember))), ""},
		// A contained self-subscriber: the Coverage's beneficiary "#pt" names the
		// member's own Patient, so its subscriber identifier makes no party of it.
		"a contained self-subscriber by identifier": {cerNaming(t, "#pt",
			map[string]any{"resourceType": "Patient", "id": "pt", "identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}},
				"name": []any{map[string]any{"family": "Childname"}}, "birthDate": "2015-06-01"},
			coverage("#pt", func(string) any {
				return map[string]any{"identifier": map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}}
			}, "")), childs},
		// A "#id" resolves only among the resources contained with the Coverage:
		// a beneficiary "#x" naming the child it contains is no tie for another
		// resource's contained "#x", the parent.
		"a contained beneficiary sharing its local id with a parent contained elsewhere": {bundle(
			entry("urn:uuid:cov", map[string]any{"resourceType": "Coverage", "id": "cov",
				"contained":   []any{map[string]any{"resourceType": "Patient", "id": "x"}},
				"beneficiary": reference("#x"),
				"subscriber":  map[string]any{"identifier": map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}}}),
			entry("urn:uuid:claim", map[string]any{"resourceType": "Claim", "id": "claim", "patient": reference("Patient/" + strangerMember),
				"contained": []any{renamedID(partyParent(), "x")}})), ""},
		// A contained that is a single object is read as the one resource a
		// lenient reader reads it as: the parent there is the Coverage's party.
		"contained as a single object": {map[string]any{"resourceType": "Coverage", "id": "cov", "status": "active",
			"contained":   renamedID(partyParent(), "parent"),
			"subscriber":  reference("#parent"),
			"beneficiary": reference("Patient/" + strangerMember)}, ""},
		// A Patient spelling its id in another case is tied to no reference.
		"beside its Coverage, the parent's id spelled Id": {cer(t, strangerMember, coverage("Patient/"+strangerMember, reference, "#parent"),
			renamed(renamedID(partyParent(), "parent"), "id", "Id")), ""},
		"a Bundle entry by its id spelled Id": {bundle(entry("urn:uuid:cov", coverage("Patient/"+strangerMember, reference, "Patient/"+partyParentID)),
			entry("urn:uuid:p", renamed(partyParent(), "id", "Id"))), ""},
		// A member-bearing Patient the walk cannot place (here, in a list within
		// a contained list) is not decided to be the member while a party slot
		// may name it, and no beneficiary names it.
		"in a list within a contained list": {map[string]any{"resourceType": "Coverage", "id": "cov", "status": "active",
			"contained": []any{[]any{renamedID(partyParent(), "parent")}}, "subscriber": reference("#parent"),
			"beneficiary": reference("Patient/" + strangerMember)}, ""},
		"the member's own Patient no beneficiary names, a party slot naming a Patient": {bundle(
			entry("urn:uuid:cov", coverage("Patient/someone-else", reference, "Patient/x")),
			entry("urn:uuid:c", child)), ""},
		// Each Coverage decides for itself: another Coverage naming the parent
		// as its beneficiary is no tie for the Coverage whose party slot may
		// name it, whether the walk cannot read the parent's id or the slot's
		// reference is one no Patient reference shape reads.
		"the parent's id spelled Id, another Coverage's beneficiary naming its fullUrl": {bundle(
			entry("urn:uuid:cov", coverage("Patient/"+strangerMember, reference, "Patient/parent")),
			entry("urn:uuid:own", map[string]any{"resourceType": "Coverage", "id": "own", "beneficiary": reference("urn:uuid:ppp")}),
			entry("urn:uuid:ppp", renamed(renamedID(partyParent(), "parent"), "id", "Id"))), ""},
		"a slot naming an unreadable host, another Coverage's beneficiary naming the parent's fullUrl": {bundle(
			entry("urn:uuid:cov", coverage("Patient/"+strangerMember, reference, "http://ehr_host/fhir/Patient/"+partyParentID)),
			entry("urn:uuid:own", map[string]any{"resourceType": "Coverage", "id": "own", "beneficiary": reference("urn:uuid:ppp")}),
			entry("urn:uuid:ppp", partyParent())), ""},
		// A slot naming the Coverage's own beneficiary leaves the Coverage
		// decided: the parent's own Coverage, carried beside the dependent's,
		// names no party for the dependent's own Patient.
		"the parent's own Coverage beside the dependent's": {bundle(
			entry("urn:uuid:cov", coverage("Patient/kid", reference, "Patient/"+partyParentID)),
			entry("urn:uuid:own", coverage("Patient/"+partyParentID, reference, "Patient/"+partyParentID)),
			entry("urn:uuid:parent", withoutIdentifier(partyParent())),
			entry("urn:uuid:k", renamedID(childPatient(), "kid"))), childs},
		// A slot naming a fullUrl two entries share, the parent's and an
		// Organization's, names the parent though the later entry is no Patient.
		"an Organization's entry sharing the parent's fullUrl": {bundle(
			entry("urn:uuid:cov", coverage("Patient/"+strangerMember, reference, "urn:uuid:parent")),
			entry("urn:uuid:parent", partyParent()),
			entry("urn:uuid:parent", map[string]any{"resourceType": "Organization", "id": "org"})), ""},
		// An identifier in the parent's own system names the parent alone.
		"a subscriber by an identifier in the parent's own system": {bundle(
			entry("urn:uuid:cov", map[string]any{"resourceType": "Coverage", "id": "cov", "status": "active",
				"beneficiary": reference("Patient/" + strangerMember),
				"subscriber":  map[string]any{"identifier": map[string]any{"system": "urn:example:person-ids", "value": "P-1"}}}),
			entry("urn:uuid:parent", withPersonID(partyParent()))), ""},
		// The member's own Patient tied to the Coverage by its fullUrl alone.
		"the member's own Patient its beneficiary names by fullUrl, a parent the subscriber": {bundle(
			entry("urn:uuid:cov", coverage("urn:uuid:k", reference, "Patient/"+partyParentID)),
			entry("urn:uuid:parent", partyParent()),
			entry("urn:uuid:k", renamedID(childPatient(), "kid"))), childs},
		// The beneficiary is its own reference, as every reader reads it: a
		// reference nested in it (an extension's, an identifier's assigner) ties
		// nothing, and one spelled in another case ties nothing either.
		"a beneficiary's extension naming the parent": {bundle(
			entry("urn:uuid:cov", map[string]any{"resourceType": "Coverage", "id": "cov", "status": "active",
				"beneficiary": map[string]any{"reference": "Patient/" + strangerMember,
					"extension": []any{map[string]any{"url": "http://example.org/x", "valueReference": reference("Patient/" + partyParentID)}}},
				"subscriber": reference("Patient/" + partyParentID)}),
			entry("urn:uuid:parent", partyParent())), ""},
		"a beneficiary identifier's assigner naming the parent": {bundle(
			entry("urn:uuid:cov", map[string]any{"resourceType": "Coverage", "id": "cov", "status": "active",
				"beneficiary": map[string]any{"reference": "Patient/" + strangerMember,
					"identifier": map[string]any{"value": "B-1", "assigner": reference("Patient/" + partyParentID)}},
				"subscriber": reference("Patient/" + partyParentID)}),
			entry("urn:uuid:parent", partyParent())), ""},
		"a beneficiary's reference spelled in another case": {bundle(
			entry("urn:uuid:cov", map[string]any{"resourceType": "Coverage", "id": "cov", "status": "active",
				"beneficiary": map[string]any{"Reference": "Patient/kid"}, "subscriber": reference("Patient/" + partyParentID)}),
			entry("urn:uuid:k", renamedID(childPatient(), "kid"))), ""},
		// A beneficiary resolves as a Bundle resolves it: an absolute reference
		// names only the entry whose fullUrl it is, and a relative one resolves
		// against the RESTful base of the Coverage's entry.
		"the parent under the beneficiary's id on another server": {bundle(
			entry("urn:uuid:cov", coverage("http://a.example/fhir/Patient/child", reference, "http://b.example/fhir/Patient/child")),
			entry("http://b.example/fhir/Patient/child", renamedID(partyParent(), "child"))), ""},
		"the parent under the beneficiary's id, beside a Coverage on another server": {bundle(
			entry("http://a.example/fhir/Coverage/cov", coverage("Patient/child", reference, "http://b.example/fhir/Patient/child")),
			entry("http://b.example/fhir/Patient/child", renamedID(partyParent(), "child"))), ""},
		"the member's own Patient on the Coverage's server, by a relative beneficiary": {bundle(
			entry("http://ehr.example/fhir/Coverage/cov", coverage("Patient/kid", reference, "Patient/"+partyParentID)),
			entry("http://ehr.example/fhir/Patient/"+partyParentID, partyParent()),
			entry("http://ehr.example/fhir/Patient/kid", renamedID(childPatient(), "kid"))), childs},
		"the member's own Patient by an absolute, versioned beneficiary": {bundle(
			entry("urn:uuid:cov", coverage("http://ehr.example/fhir/Patient/kid/_history/2", reference, "Patient/"+partyParentID)),
			entry("urn:uuid:parent", partyParent()),
			entry("http://ehr.example/fhir/Patient/kid", renamedID(childPatient(), "kid"))), childs},
		// A Patient carried outside an entry is named by its type and id, whatever
		// the Coverage's entry; one whose RESTful fullUrl names another id than
		// its own is named by none.
		"the member's own Patient outside an entry, its Coverage in a RESTful entry, a parent not carried": {map[string]any{
			"patient":  renamedID(childPatient(), "kid"),
			"coverage": bundle(entry("http://ehr.example/fhir/Coverage/cov", coverage("Patient/kid", reference, "Patient/"+partyParentID)))}, childs},
		"the member's own Patient outside an entry, a self-subscriber in a RESTful entry": {map[string]any{
			"patient":  renamedID(childPatient(), "kid"),
			"coverage": bundle(entry("http://ehr.example/fhir/Coverage/cov", coverage("Patient/kid", reference, "Patient/kid")))}, childs},
		"the member's own Patient outside an entry, its parent in a RESTful entry": {map[string]any{
			"patient": renamedID(childPatient(), "kid"),
			"coverage": bundle(entry("http://ehr.example/fhir/Coverage/cov", coverage("Patient/kid", reference, "Patient/"+partyParentID)),
				entry("http://ehr.example/fhir/Patient/"+partyParentID, partyParent()))}, childs},
		// A Patient under a urn:uuid is named by that fullUrl, or by its type and
		// id only through a relative reference no RESTful base resolves: a
		// RESTful Coverage's "Patient/kid" names another resource, so the walk
		// cannot decide the Patient is the member.
		"the member's own Patient under a urn:uuid, a self-subscriber in a RESTful entry": {bundle(
			entry("http://ehr.example/fhir/Coverage/cov", coverage("Patient/kid", reference, "Patient/kid")),
			entry("urn:uuid:k", renamedID(childPatient(), "kid"))), ""},
		"the member's own Patient under a urn:uuid, by a relative beneficiary": {bundle(
			entry("urn:uuid:cov", coverage("Patient/kid", reference, "Patient/"+partyParentID)),
			entry("urn:uuid:parent", partyParent()),
			entry("urn:uuid:k", renamedID(childPatient(), "kid"))), childs},
		// Different people under one id: no reference names either, however exactly, so
		// beside the parent's slot the member cannot be identified.
		"a parent under a urn:uuid with the beneficiary's id, beside a RESTful Coverage": {bundle(
			entry("http://ehr.example/fhir/Coverage/cov", coverage("Patient/kid", reference, "urn:uuid:x")),
			entry("http://ehr.example/fhir/Patient/kid", renamedID(childPatient(), "kid")),
			entry("urn:uuid:x", renamedID(partyParent(), "kid"))), ""},
		"a parent under a urn:uuid with the beneficiary's id, the member not carried": {bundle(
			entry("http://ehr.example/fhir/Coverage/cov", coverage("Patient/kid", reference, "urn:uuid:x")),
			entry("urn:uuid:x", renamedID(partyParent(), "kid"))), ""},
		// A contained Patient is named only by "#id", within its container: a
		// parent contained under the beneficiary's id is no beneficiary.
		"a parent contained under the beneficiary's id": {bundle(
			entry("http://ehr.example/fhir/Coverage/cov", coverage("Patient/kid", reference, "Patient/"+partyParentID)),
			entry("http://ehr.example/fhir/Patient/kid", withoutIdentifier(renamedID(childPatient(), "kid"))),
			entry("http://ehr.example/fhir/Claim/c1", map[string]any{"resourceType": "Claim", "id": "c1", "patient": reference("Patient/kid"),
				"contained": []any{renamedID(partyParent(), "kid")}})), ""},
		"a parent contained under the beneficiary's id, the subscriber by identifier": {bundle(
			entry("urn:uuid:cov", coverage("Patient/kid", func(string) any {
				return map[string]any{"identifier": map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}}
			}, "")),
			entry("urn:uuid:claim", map[string]any{"resourceType": "Claim", "id": "c1", "patient": reference("Patient/kid"),
				"contained": []any{renamedID(partyParent(), "kid")}})), ""},
		"a parent contained beside its Coverage under the beneficiary's id": {map[string]any{"resourceType": "Claim", "id": "c1",
			"patient":   reference("Patient/kid"),
			"contained": []any{coverage("Patient/kid", reference, "Patient/"+partyParentID), renamedID(partyParent(), "kid")}}, ""},
		"the member's own Patient contained, its beneficiary by type and id": {cer(t, strangerMember,
			coverage("Patient/"+strangerMember, reference, "Patient/"+partyParentID), child), ""},
		"the member's own Patient contained, its beneficiary by #id": {cer(t, strangerMember,
			coverage("#"+strangerMember, reference, "Patient/"+partyParentID), child), childs},
		// Only a Bundle's entries have a fullUrl a reference resolves by, and a
		// Coverage's references resolve among its own Bundle's entries alone: an
		// object elsewhere holding "fullUrl" and "resource", or an entry of
		// another Bundle, is named by none.
		"the parent in a wrapper outside any Bundle under the beneficiary's urn": {map[string]any{
			"patient":  renamedID(childPatient(), "kid"),
			"coverage": bundle(entry("http://ehr.example/fhir/Coverage/cov", coverage("urn:uuid:p", reference, "Patient/"+partyParentID))),
			"vendor":   map[string]any{"fullUrl": "urn:uuid:p", "resource": partyParent()}}, ""},
		"the parent, id-less, in a wrapper outside any Bundle under the beneficiary's RESTful fullUrl": {map[string]any{
			"coverage": bundle(entry("http://ehr.example/fhir/Coverage/cov", coverage("Patient/kid", reference, "Patient/"+partyParentID))),
			"vendor":   map[string]any{"fullUrl": "http://ehr.example/fhir/Patient/kid", "resource": withoutID(partyParent())}}, ""},
		"the parent an entry of another Bundle under the beneficiary's urn": {map[string]any{
			"coverage":    bundle(entry("urn:uuid:cov", coverage("urn:uuid:p", reference, "Patient/"+partyParentID))),
			"draftOrders": bundle(entry("urn:uuid:p", partyParent()))}, ""},
		"the member's own Patient an entry of the Coverage's own Bundle under the beneficiary's urn": {map[string]any{
			"coverage":    bundle(entry("urn:uuid:cov", coverage("urn:uuid:k", reference, "Patient/"+partyParentID)), entry("urn:uuid:k", renamedID(childPatient(), "kid"))),
			"draftOrders": bundle(entry("urn:uuid:p", partyParent()))}, childs},
		"two Bundles, each the member's self-subscribed Coverage and identical copies of the member's entry": {map[string]any{
			"first": bundle(entry("http://ehr.example/fhir/Coverage/cov", coverage("http://ehr.example/fhir/Patient/kid", reference, "http://ehr.example/fhir/Patient/kid")),
				entry("http://ehr.example/fhir/Patient/kid", renamedID(childPatient(), "kid"))),
			"second": bundle(entry("http://ehr.example/fhir/Coverage/cov", coverage("http://ehr.example/fhir/Patient/kid", reference, "http://ehr.example/fhir/Patient/kid")),
				entry("http://ehr.example/fhir/Patient/kid", renamedID(childPatient(), "kid")))}, childs},
		"the parent under the member's own fullUrl in another Bundle": {map[string]any{
			"coverage":    bundle(entry("http://ehr.example/fhir/Coverage/cov", coverage("Patient/kid", reference, "Patient/kid")), entry("http://ehr.example/fhir/Patient/kid", withoutIdentifier(renamedID(childPatient(), "kid")))),
			"draftOrders": bundle(entry("http://ehr.example/fhir/Patient/kid", renamedID(partyParent(), "kid")))}, ""},
		// Versions of one person's record under one fullUrl are one Patient.
		"two versions of the member's own record under one fullUrl, no Coverage": {map[string]any{"resourceType": "Bundle", "type": "history", "entry": []any{
			entry("http://ehr.example/fhir/Patient/kid", withMeta(renamedID(childPatient(), "kid"), "2")),
			entry("http://ehr.example/fhir/Patient/kid", withMeta(renamedID(childPatient(), "kid"), "1"))}}, childs},
		"two versions of the member's own record under one fullUrl, a parent the subscriber": {map[string]any{
			"patient": map[string]any{"resourceType": "Bundle", "type": "history", "entry": []any{
				entry("http://ehr.example/fhir/Patient/kid", withMeta(renamedID(childPatient(), "kid"), "2")),
				entry("http://ehr.example/fhir/Patient/kid", withMeta(renamedID(childPatient(), "kid"), "1"))}},
			"coverage": bundle(entry("http://ehr.example/fhir/Coverage/cov", coverage("Patient/kid", reference, "Patient/"+partyParentID)))}, childs},
		"two versions of the member's own record, the later adding an identifier and a given name, a parent the subscriber": {map[string]any{
			"patient": map[string]any{"resourceType": "Bundle", "type": "history", "entry": []any{
				entry("http://ehr.example/fhir/Patient/kid", laterVersion(renamedID(childPatient(), "kid"))),
				entry("http://ehr.example/fhir/Patient/kid", withMeta(renamedID(childPatient(), "kid"), "1"))}},
			"coverage": bundle(entry("http://ehr.example/fhir/Coverage/cov", coverage("Patient/kid", reference, "Patient/"+partyParentID)))}, childs},
		// Only Patients carrying both facts are told apart by them: one lacking a
		// birth date or family name is itself, so beside it the parent stays
		// another person.
		"a Patient with no demographics under the parent's id, the parent the subscriber": {bundle(
			entry("urn:uuid:a", map[string]any{"resourceType": "Patient", "id": "x", "gender": "female"}),
			entry("urn:uuid:b", map[string]any{"resourceType": "Patient", "id": "x", "birthDate": "1980-02-02",
				"identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}}}),
			entry("urn:uuid:c", coverage("Patient/x", reference, "urn:uuid:b"))), ""},
		"the member's own Patient and the parent each with a birth date alone, under one id": {bundle(
			entry("urn:uuid:a", map[string]any{"resourceType": "Patient", "id": "x", "birthDate": "2015-06-01"}),
			entry("urn:uuid:b", map[string]any{"resourceType": "Patient", "id": "x", "birthDate": "1980-02-02",
				"identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}}}),
			entry("urn:uuid:c", coverage("Patient/x", reference, "urn:uuid:b"))), ""},
		"two versions of the member's own record with no birth date, a parent the subscriber": {map[string]any{
			"patient": map[string]any{"resourceType": "Bundle", "type": "history", "entry": []any{
				entry("http://ehr.example/fhir/Patient/kid", withMeta(withoutBirthDate(renamedID(childPatient(), "kid")), "2")),
				entry("http://ehr.example/fhir/Patient/kid", withMeta(withoutBirthDate(renamedID(childPatient(), "kid")), "1"))}},
			"coverage": bundle(entry("http://ehr.example/fhir/Coverage/cov", coverage("Patient/kid", reference, "Patient/"+partyParentID)))}, derivedPCI(strangerMember, "", "")},
		"the member's own Patient and the parent sharing a birth date, under one id": {bundle(
			entry("urn:uuid:a", map[string]any{"resourceType": "Patient", "id": "x", "birthDate": "2015-06-01", "name": []any{map[string]any{"family": "Childname"}}}),
			entry("urn:uuid:b", map[string]any{"resourceType": "Patient", "id": "x", "birthDate": "2015-06-01", "name": []any{map[string]any{"family": "Parentname"}},
				"identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}}}),
			entry("urn:uuid:c", coverage("Patient/x", reference, "urn:uuid:b"))), ""},
		// Patients carrying neither fact derive one identity, the member id alone: they are
		// one record, so the beneficiary naming them ties both and the request binds by the
		// member id, the identity either derives.
		"two Patients under one id carrying neither fact, the subscriber by the member's identifier": {bundle(
			entry("urn:uuid:a", map[string]any{"resourceType": "Patient", "id": "x", "gender": "female"}),
			entry("urn:uuid:b", map[string]any{"resourceType": "Patient", "id": "x", "gender": "male",
				"identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}}}),
			entry("urn:uuid:c", coverage("Patient/x", func(string) any {
				return map[string]any{"identifier": map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}}
			}, ""))), derivedPCI(strangerMember, "", "")},
		// With no Coverage, nothing names a party: different people under one
		// fullUrl are read as before, their demographics disagreeing.
		"different people under one fullUrl, no Coverage": {bundle(
			entry("http://ehr.example/fhir/Patient/kid", renamedID(childPatient(), "kid")),
			entry("http://ehr.example/fhir/Patient/kid", renamedID(partyParent(), "kid"))), derivedPCI(strangerMember, "", "")},
		// A RESTful URL names one resource wherever it is carried: the member's
		// own entry of another Bundle is the beneficiary its URL resolves to,
		// while no different Patient is carried under its id.
		"the member's own Patient an entry of another Bundle at the beneficiary's RESTful URL": {map[string]any{
			"patient":  bundle(entry("http://ehr.example/fhir/Patient/kid", renamedID(childPatient(), "kid"))),
			"coverage": bundle(entry("http://ehr.example/fhir/Coverage/cov", coverage("Patient/kid", reference, "Patient/"+partyParentID)))}, childs},
		"a Coverage in no Bundle naming the member's own entry by type and id": {map[string]any{
			"patient":  bundle(entry("http://ehr.example/fhir/Patient/kid", renamedID(childPatient(), "kid"))),
			"coverage": coverage("Patient/kid", reference, "Patient/"+partyParentID)}, childs},
		"the parent, id-less, an entry of another Bundle at the beneficiary's RESTful URL, the member's own Patient outside any entry": {map[string]any{
			"patient":  withoutIdentifier(renamedID(childPatient(), "kid")),
			"coverage": bundle(entry("http://ehr.example/fhir/Coverage/cov", coverage("Patient/kid", reference, "Patient/"+partyParentID))),
			"vendor":   bundle(entry("http://ehr.example/fhir/Patient/kid", withoutID(partyParent())))}, ""},
		// A beneficiary is one Reference: a list of them ties no Patient.
		"a beneficiary list naming the member's own Patient and the parent": {bundle(
			entry("http://ehr.example/fhir/Coverage/cov", map[string]any{"resourceType": "Coverage", "id": "cov", "status": "active",
				"beneficiary": []any{reference("Patient/kid"), reference("Patient/" + partyParentID)},
				"subscriber":  map[string]any{"identifier": map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}}}),
			entry("http://ehr.example/fhir/Patient/kid", withoutIdentifier(renamedID(childPatient(), "kid"))),
			entry("http://ehr.example/fhir/Patient/"+partyParentID, partyParent())), ""},
		// A request spelling a name a reference is resolved by in another case,
		// or nesting contained resources, is one readers may resolve
		// differently: no beneficiary ties a Patient there.
		"the member's own Patient spelling its id Id, the parent under that id": {map[string]any{"resourceType": "Parameters", "parameter": []any{
			map[string]any{"name": "coverage", "resource": coverage("Patient/kid", reference, "Patient/"+partyParentID)},
			map[string]any{"name": "patient", "resource": renamed(withoutIdentifier(renamedID(childPatient(), "kid")), "id", "Id")},
			map[string]any{"name": "patient", "resource": renamedID(partyParent(), "kid")}}}, ""},
		"an entry spelling fullUrl FullUrl, the parent under that fullUrl": {bundle(
			entry("urn:uuid:cov", coverage("urn:uuid:p", reference, "Patient/"+partyParentID)),
			map[string]any{"FullUrl": "urn:uuid:p", "resource": withoutIdentifier(renamedID(childPatient(), "kid"))},
			entry("urn:uuid:p", renamedID(partyParent(), "q"))), ""},
		"an entry spelling resource Resource, the parent under its fullUrl": {bundle(
			entry("urn:uuid:cov", coverage("urn:uuid:p", reference, "Patient/"+partyParentID)),
			map[string]any{"fullUrl": "urn:uuid:p", "Resource": withoutIdentifier(renamedID(childPatient(), "kid"))},
			entry("urn:uuid:p", renamedID(partyParent(), "q"))), ""},
		"a contained Coverage containing the member's own Patient, the parent beside it": {map[string]any{"resourceType": "Claim", "id": "c1",
			"patient": reference("#pat"),
			"contained": []any{renamedID(partyParent(), "pat"), map[string]any{"resourceType": "Coverage", "id": "cov", "status": "active",
				"beneficiary": reference("#pat"), "subscriber": map[string]any{"identifier": map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}},
				"contained": []any{withoutIdentifier(renamedID(childPatient(), "pat"))}}}}, ""},
		// A fullUrl two entries share (versions aside) names no one of them.
		"the member's own Patient and the parent under one RESTful fullUrl": {bundle(
			entry("http://ehr.example/fhir/Coverage/cov", coverage("Patient/kid", func(string) any {
				return map[string]any{"identifier": map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}}
			}, "")),
			entry("http://ehr.example/fhir/Patient/kid", withoutIdentifier(renamedID(childPatient(), "kid"))),
			entry("http://ehr.example/fhir/Patient/kid", renamedID(partyParent(), "kid"))), ""},
		"the member's own Patient and the parent under one urn:uuid": {bundle(
			entry("urn:uuid:cov", coverage("urn:uuid:p", reference, "Patient/"+partyParentID)),
			entry("urn:uuid:p", withoutIdentifier(renamedID(childPatient(), "kid"))),
			entry("urn:uuid:p", renamedID(partyParent(), "q"))), ""},
		"the parent under another version of the member's own fullUrl": {bundle(
			entry("urn:uuid:cov", coverage("http://ehr.example/fhir/Patient/kid", reference, "Patient/"+partyParentID)),
			entry("http://ehr.example/fhir/Patient/kid/_history/1", withoutIdentifier(renamedID(childPatient(), "kid"))),
			entry("http://ehr.example/fhir/Patient/kid/_history/2", renamedID(partyParent(), "kid"))), ""},
		"the member's own Patient and the parent under one fullUrl spelled with its default port": {bundle(
			entry("http://ehr.example/fhir/Coverage/cov", coverage("Patient/kid", func(string) any {
				return map[string]any{"identifier": map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}}
			}, "")),
			entry("HTTP://EHR.example:80/fhir/Patient/kid", withoutIdentifier(renamedID(childPatient(), "kid"))),
			entry("http://ehr.example/fhir/Patient/kid", renamedID(partyParent(), "kid"))), ""},
		"the member's own entry repeated as identical copies, a parent the subscriber": {bundle(
			entry("urn:uuid:cov", coverage("http://ehr.example/fhir/Patient/kid", reference, "Patient/"+partyParentID)),
			entry("http://ehr.example/fhir/Patient/kid", renamedID(childPatient(), "kid")),
			entry("urn:uuid:inner", bundle(entry("http://ehr.example/fhir/Patient/kid", renamedID(childPatient(), "kid"))))), childs},
		// A "#id" two contained resources share names no one of them.
		"the member's own Patient and the parent contained under one id, the subscriber by identifier": {cerNaming(t, "#pat",
			withoutIdentifier(renamedID(childPatient(), "pat")), renamedID(partyParent(), "pat"),
			coverage("#pat", func(string) any {
				return map[string]any{"identifier": map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}}
			}, "")), ""},
		"the parent and the member's own Patient contained under one id, the subscriber by reference": {cerNaming(t, "#pat",
			renamedID(partyParent(), "pat"), withoutIdentifier(renamedID(childPatient(), "pat")),
			coverage("#pat", reference, "#par")), ""},
		// Different people under one id, one contained and one not, are read two ways:
		// some readers resolve a reference by type and id to a contained resource too.
		"the member's own Patient contained, the parent outside it under the same id, an absolute beneficiary": {func() map[string]any {
			doc := cer(t, strangerMember, coverage("http://other.example/fhir/Patient/"+strangerMember, reference, "#parent"),
				renamedID(partyParent(), "parent"), child)
			doc["vendor"] = map[string]any{"fullUrl": "urn:uuid:p", "resource": renamedID(partyParent(), strangerMember)}
			return doc
		}(), ""},
		"the member's own Patient and another person contained under one id, the parent in a Bundle under it, a versioned beneficiary": {func() map[string]any {
			doc := cer(t, strangerMember, coverage("Patient/"+strangerMember+"/_history/2", reference, "#parent"),
				child, renamedID(partyParent(), "parent"), renamedID(partyParent(), strangerMember))
			doc["other"] = bundle(entry("https://ehr.example/fhir/Patient/"+strangerMember, renamedID(partyParent(), strangerMember)))
			return doc
		}(), ""},
		// A contained Patient with no id has none of its container's: beside the member's
		// own entry it is no Patient under the member's id.
		"the member's own entry containing a Patient with no id": {bundle(
			entry("http://ehr.example/fhir/Coverage/cov", coverage("Patient/kid", reference, "Patient/"+partyParentID)),
			entry("http://ehr.example/fhir/Patient/kid", func() map[string]any {
				p := renamedID(childPatient(), "kid")
				p["contained"] = []any{map[string]any{"resourceType": "Patient", "gender": "female",
					"link": []any{map[string]any{"other": reference("#"), "type": "seealso"}}}}
				return p
			}()),
			entry("urn:uuid:p", partyParent())), childs},
		// A "#id" two contained resources share, a Patient and another, names neither.
		"the member's own Patient and an Organization contained under one id": {cerNaming(t, "#x",
			coverage("#x", reference, "#parent"), renamedID(childPatient(), "x"),
			map[string]any{"resourceType": "Organization", "id": "x"}, renamedID(partyParent(), "parent")), ""},
		// A "#id" resolves only among the resources contained with the Coverage: a
		// parent contained in another resource under that id is no beneficiary.
		"the parent contained in another resource under the beneficiary's #id": {bundle(
			entry("urn:uuid:cov", map[string]any{"resourceType": "Coverage", "id": "cov", "status": "active",
				"beneficiary": reference("#x"), "subscriber": reference("Patient/" + partyParentID)}),
			entry("urn:uuid:claim", map[string]any{"resourceType": "Claim", "id": "claim",
				"contained": []any{renamedID(partyParent(), "x")}})), ""},
		// A fullUrl is compared as a reader that normalizes URLs reads it: the parent
		// at the member's own URL, spelled with its default port, is another resource
		// at that URL.
		"the parent at the member's own fullUrl spelled with its default port, under another id": {bundle(
			entry("http://ehr.example/fhir/Coverage/cov", coverage("Patient/kid", reference, "Patient/"+partyParentID)),
			entry("http://ehr.example/fhir/Patient/kid", renamedID(childPatient(), "kid")),
			entry("http://ehr.example:80/fhir/Patient/kid", renamedID(partyParent(), "p2"))), ""},
		// A reference by type and id names no one of different Patients carried
		// under that id; identical copies of one Patient are one.
		"a parent sharing the beneficiary's id with another Patient, the subscriber by fullUrl": {bundle(
			entry("urn:uuid:cov", coverage("Patient/kid", reference, "urn:uuid:x")),
			entry("urn:uuid:c", withoutIdentifier(renamedID(childPatient(), "kid"))),
			entry("urn:uuid:x", renamedID(partyParent(), "kid"))), ""},
		"the member's own Patient carried twice, a parent the subscriber": {map[string]any{
			"patient": renamedID(childPatient(), "kid"),
			"coverage": bundle(entry("http://ehr.example/fhir/Coverage/cov", coverage("Patient/kid", reference, "Patient/"+partyParentID)),
				entry("urn:uuid:k", renamedID(childPatient(), "kid")))}, childs},
		"a parent under a urn:uuid with an absolute beneficiary's id": {bundle(
			entry("urn:uuid:cov", coverage("http://ehr.example/fhir/Patient/kid", reference, "urn:uuid:x")),
			entry("urn:uuid:x", renamedID(partyParent(), "kid"))), ""},
		"the parent in an entry whose fullUrl names the beneficiary's id": {bundle(
			entry("urn:uuid:cov", coverage("http://ehr.example/fhir/Patient/child", reference, "Patient/"+partyParentID)),
			entry("http://ehr.example/fhir/Patient/child", partyParent())), ""},
		// Controls: a payor naming an Organization, by reference or by a payer
		// identifier, may name no Patient: the member's own Patient is read.
		"a payor naming an Organization by reference": {bundle(entry("urn:uuid:cov", map[string]any{"resourceType": "Coverage", "id": "cov", "status": "active",
			"beneficiary": reference("Patient/kid"), "payor": []any{reference("Organization/payer")}}),
			entry("urn:uuid:c", child)), childs},
		"a payor naming a contained Organization": {map[string]any{"resourceType": "Coverage", "id": "cov", "status": "active",
			"contained":   []any{map[string]any{"resourceType": "Organization", "id": "org"}, child},
			"beneficiary": reference("Patient/kid"), "payor": []any{reference("#org")}}, childs},
		// The beneficiary ties the member's own Patient in every reference form.
		"a self-subscriber tied by fullUrl": {bundle(entry("urn:uuid:cov", coverage("urn:uuid:self", reference, "urn:uuid:self")),
			entry("urn:uuid:self", renamedID(childPatient(), "kid"))), childs},
		"an id-less Patient tied through its RESTful fullUrl": {bundle(entry("urn:uuid:cov", coverage("Patient/kid", reference, "Patient/x")),
			entry("http://ehr.example/fhir/Patient/kid", withoutID(childPatient()))), childs},
		"a contained single-object Patient tied by its #id": {map[string]any{"resourceType": "Coverage", "id": "cov", "status": "active",
			"contained": renamedID(childPatient(), "pt"), "beneficiary": reference("#pt"), "subscriber": reference("Patient/x")}, childs},
		// The parent is the beneficiary of its own Coverage, and the dependent's
		// Coverage names it as its party: it is still the party there.
		"the parent its own Coverage's beneficiary, the dependent's party by reference": {bundle(
			entry("urn:uuid:own", map[string]any{"resourceType": "Coverage", "id": "own", "beneficiary": reference("Patient/" + partyParentID)}),
			entry("urn:uuid:cov", coverage("Patient/dep", reference, "Patient/"+partyParentID)),
			entry("urn:uuid:parent", partyParent())), ""},
		"the parent its own Coverage's beneficiary, the dependent's party by an identifier with no system": {bundle(
			entry("urn:uuid:own", map[string]any{"resourceType": "Coverage", "id": "own", "beneficiary": reference("Patient/" + partyParentID)}),
			entry("urn:uuid:cov", coverage("Patient/dep", func(string) any {
				return map[string]any{"identifier": map[string]any{"value": strangerMember}}
			}, "")), entry("urn:uuid:parent", partyParent())), ""},
		"a payor naming an Organization by a payer identifier": {bundle(entry("urn:uuid:cov", map[string]any{"resourceType": "Coverage", "id": "cov", "status": "active",
			"beneficiary": reference("Patient/kid"), "payor": []any{map[string]any{"identifier": map[string]any{"system": "urn:oid:2.16.840.1.113883.6.300", "value": "00001"}}}}),
			entry("urn:uuid:c", child)), childs},
		// The parent's own request: its Coverage's subscriber is its beneficiary.
		"the parent's own request": {cer(t, strangerMember, coverage("#parent", reference, "#parent"), inline), parentIdentity()},
		// Control: no party and no Patient binds by the member id, as before.
		"no party, no Patient": {cer(t, strangerMember), derivedPCI(strangerMember, "", "")},
	} {
		t.Run(name, func(t *testing.T) {
			payload := mustJSON(t, row.payload)
			pci, found, err := g.resolveSubjectPCI(context.Background(), strangerMember, payload)
			if pci == parentIdentity() && row.want != parentIdentity() {
				t.Fatal("bound to the parent's identity")
			}
			if row.want == "" {
				if !errors.Is(err, errPatientUnidentifiable) || pci != "" {
					t.Fatalf("pci %q (found %v, err %v), want refused: the member cannot be identified", pci, found, err)
				}
				return
			}
			if err != nil || !found || pci != row.want {
				t.Fatalf("pci %q (found %v, err %v), want %q", pci, found, err, row.want)
			}
		})
	}
	// A member the system of record holds binds through it, whatever the
	// request carries.
	held, _, _ := newCensusSoR().ResolvePatient("MBR-COVERED")
	payload := mustJSON(t, cer(t, "MBR-COVERED", coverage("Patient/MBR-COVERED", reference, "#parent"), inline))
	if pci, found, err := g.resolveSubjectPCI(context.Background(), "MBR-COVERED", payload); err != nil || !found || pci != held {
		t.Fatalf("held member: pci %q (found %v, err %v), want its record's %q", pci, found, err, held)
	}
}

// The identities the rows pin: the dependent's own, from its Patient (member
// eXYZ123, born 2015-06-01, family Childname), and the parent's (born
// 1970-01-01, family Parentname), which the parent's own requests derive.
const (
	pinnedChildIdentity  = "pci:bdbda6f02d661da9535f72f0c80513e6"
	pinnedParentIdentity = "pci:b98d6aeb66c46d4a6bdcc7e66c73826f"
)

// withMembersOwnPatient is body with the member's own Patients given a birth
// date and family name, or, carrying none, with one added by add.
func withMembersOwnPatient(t *testing.T, body []byte, add func(map[string]any, map[string]any)) []byte {
	t.Helper()
	doc := decodeDoc(t, body)
	found := 0
	var walk func(node any)
	walk = func(node any) {
		switch v := node.(type) {
		case map[string]any:
			if v["resourceType"] == "Patient" && patientIsMember(v, strangerMember) {
				v["birthDate"], v["name"] = "2015-06-01", []any{map[string]any{"family": "Childname"}}
				found++
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(doc)
	if found == 0 {
		add(doc, map[string]any{"resourceType": "Patient", "id": strangerMember, "birthDate": "2015-06-01", "name": []any{map[string]any{"family": "Childname"}}})
		// Added as a contained resource, it is named as one is: by "#id".
		contained, _ := doc["contained"].([]any)
		if slices.ContainsFunc(contained, func(c any) bool {
			r, _ := c.(map[string]any)
			return r["resourceType"] == "Patient" && r["id"] == strangerMember
		}) {
			var local func(node any)
			local = func(node any) {
				switch v := node.(type) {
				case map[string]any:
					if v["reference"] == "Patient/"+strangerMember {
						v["reference"] = "#" + strangerMember
					}
					for _, child := range v {
						local(child)
					}
				case []any:
					for _, child := range v {
						local(child)
					}
				}
			}
			local(doc)
		}
	}
	return mustJSON(t, doc)
}

// On every leg that derives an identifier, at every level, the provider
// gateway's bind and the payer gateway's bind of the same request carrying the
// parent as a Coverage's party agree: never the parent's identity. Without the
// member's own Patient carrying a birth date and name, the request is refused;
// with it, both bind the member's own identity. (At strict a provider gateway's
// own patient-consistency check, which it opted into, may refuse a request
// naming a second patient first.)
func TestPartyBesideCoverage_EveryLegBothGateways(t *testing.T) {
	ctx := context.Background()
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	if derivedPCI(strangerMember, "2015-06-01", "Childname") != pinnedChildIdentity || parentIdentity() != pinnedParentIdentity {
		t.Fatal("the pinned identities are not the derivation's")
	}
	ref := "Patient/" + strangerMember
	type leg struct {
		original []byte
		partyRef string
		slot     func(string) any
		add      func(map[string]any, map[string]any)
		provider func([]byte) (string, int, string)
	}
	elig := decodeDoc(t, eligibilityRequest(t, strangerMember))
	elig["contained"] = []any{map[string]any{"resourceType": "Coverage", "id": "cov", "status": "active", "beneficiary": reference(ref)}}
	legs := map[string]leg{
		"CRD": {crdReqJSON(strangerMember, ref, ref), "Patient/" + partyParentID, reference, asPrefetch,
			func(b []byte) (string, int, string) { return g.ingressCRDSubjectPCIContext(ctx, b) }},
		"DTR": {dtrPackageFor(strangerMember), "Patient/" + partyParentID, reference, asParameter,
			func(b []byte) (string, int, string) {
				out, status, msg := g.prepareDTRPackageRequest(ctx, b)
				return out.pci, status, msg
			}},
		"PAS": {conformantPASBundleWithQR(t, strangerMember), "urn:uuid:parent", reference, asBundleEntry("urn:uuid:parent"),
			func(b []byte) (string, int, string) { return g.ingressPASNativeSubjectPCIContext(ctx, b) }},
		"inquiry": {inquiryBundle(strangerMember, "", "TRN-1", "72148"), "Patient/" + partyParentID, referenceList,
			asBundleEntry("http://ehr.example/fhir/Patient/" + partyParentID), nil},
		"eligibility": {mustJSON(t, elig), "#" + partyParentID, reference, asContainedSibling, nil},
	}
	for name, l := range legs {
		rows := map[string]struct {
			request []byte
			want    string
		}{
			"the leg's own form/no Patient of the member's own with demographics": {withParentParty(t, l.original, l.partyRef, l.slot, l.add), ""},
			"the leg's own form/the member's own Patient":                         {withParentParty(t, withMembersOwnPatient(t, l.original, l.add), l.partyRef, l.slot, l.add), pinnedChildIdentity},
		}
		for form, f := range partyForms {
			rows[form+"/no Patient of the member's own with demographics"] = struct {
				request []byte
				want    string
			}{withParentNamedAs(t, l.original, f, l.partyRef, l.add), ""}
			rows[form+"/the member's own Patient"] = struct {
				request []byte
				want    string
			}{withParentNamedAs(t, withMembersOwnPatient(t, l.original, l.add), f, l.partyRef, l.add), pinnedChildIdentity}
		}
		for variant, row := range rows {
			for _, level := range allLevels {
				t.Run(name+"/"+variant+"/"+level.String(), func(t *testing.T) {
					g.cfg.ConformanceEnforcement = level
					if l.provider != nil {
						pci, status, msg := l.provider(row.request)
						if !(level == EnforcementStrict && status == http.StatusForbidden && strings.Contains(msg, "inconsistent patient")) {
							wantPartyBinding(t, "provider", pci, status, msg, row.want)
						}
					}
					pci, status, msg := g.bindInboundSubject(ctx, strangerMember, row.request)
					wantPartyBinding(t, "payer", pci, status, msg, row.want)
				})
			}
		}
	}
}

// The inquiry, eligibility and federated query ingress routes, end to end on
// the provider's gateway at every level: a request carrying the parent as a
// Coverage's party and no Patient of the member's own is refused as a patient
// it cannot identify, recorded as authority, before anything is sent. (The
// binds of the other legs run before any level's check, on both gateways:
// TestPartyBesideCoverage_EveryLegBothGateways.)
func TestPartyBesideCoverage_IngressRefusesAtEveryLevel(t *testing.T) {
	inquiry := withParentParty(t, inquiryBundle(strangerMember, "", "TRN-1", "72148"), "Patient/"+partyParentID, reference,
		asBundleEntry("http://ehr.example/fhir/Patient/"+partyParentID))
	elig := decodeDoc(t, eligibilityRequest(t, strangerMember))
	elig["contained"] = []any{map[string]any{"resourceType": "Coverage", "id": "cov", "status": "active", "beneficiary": reference("Patient/" + strangerMember)}}
	eligibility := withParentParty(t, mustJSON(t, elig), "#"+partyParentID, reference, asContainedSibling)
	task := decodeDoc(t, cdexTaskFor(t, strangerMember))
	task["contained"] = []any{map[string]any{"resourceType": "Coverage", "id": "cov", "status": "active", "beneficiary": reference("Patient/" + strangerMember)}}
	federated := withParentParty(t, mustJSON(t, task), "#"+partyParentID, reference, asContainedSibling)
	for _, level := range allLevels {
		t.Run("inquiry/"+level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.ConformanceEnforcement = level
			var got exchangeRecords
			env.originator.cfg.ExchangeObserved = got.observe
			w := httptest.NewRecorder()
			env.originator.ingressRoute(RoutePASInquire)(w, httptest.NewRequest(http.MethodPost, "/Claim/$inquire", bytes.NewReader(inquiry)))
			if n := env.routeHitCount(); n != 0 {
				t.Fatalf("the refused inquiry crossed the network (%d)", n)
			}
			// At strict the participant's own patient-consistency check, which
			// it opted into, refuses the request first.
			if level == EnforcementStrict && w.Code == http.StatusForbidden && strings.Contains(w.Body.String(), "inconsistent patient") {
				return
			}
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "cannot identify the patient") {
				t.Fatalf("answer %d %s", w.Code, w.Body)
			}
			wantRefusal(t, got.only(t), http.StatusBadRequest, RefusedByProviderGateway, RefusalAuthority)
		})
		t.Run("eligibility/"+level.String(), func(t *testing.T) {
			env, rec, x := runEligibilityIngress(t, level, eligibility, nil)
			refusedBeforeTheNetwork(t, env, rec, http.StatusBadRequest, "cannot identify the patient")
			wantRefusal(t, x, http.StatusBadRequest, RefusedByProviderGateway, RefusalAuthority)
		})
		t.Run("federated query/"+level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.ConformanceEnforcement = level
			var got exchangeRecords
			env.originator.cfg.ExchangeObserved = got.observe
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteFederatedQuery)(rec, httptest.NewRequest(http.MethodPost, "/Task", bytes.NewReader(federated)))
			refusedBeforeTheNetwork(t, env, rec, http.StatusBadRequest, "cannot identify the patient")
			wantRefusal(t, got.only(t), http.StatusBadRequest, RefusedByProviderGateway, RefusalAuthority)
		})
	}
}

// A CRD request carries its patient as a bare prefetch Patient and its
// Coverage in a searchset of RESTful entries: a self-covered member, a
// dependent whose parent is not carried, and a self-paying member are each
// bound from the member's own Patient, by the provider's CRD ingress and the
// payer's gateway, at every level. Members of a vendor's own in the CDS Hooks
// extension are no FHIR names: whatever their case, the request is read alike.
func TestPartyBesideCoverage_CRDPrefetchBindsTheMember(t *testing.T) {
	ctx := context.Background()
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	want := derivedPCI("example", "1960-01-01", "Test")
	for name, slot := range map[string][2]string{
		"no party":                      {"", ""},
		"self-subscriber":               {"subscriber", "Patient/example"},
		"a parent not carried":          {"subscriber", "Patient/parent"},
		"a self-paying member as payor": {"payor", "Patient/example"},
	} {
		for vendor, member := range map[string]map[string]any{
			"":                                 nil,
			", a vendor extension member Id":   {"Id": "abc"},
			", vendor members named Reference": {"Reference": "x", "Entry": []any{}, "Resource": map[string]any{}},
		} {
			b, err := os.ReadFile(filepath.Join("testdata", "relayfidelity", "valid", "crd-order-sign-request.json"))
			if err != nil {
				t.Fatal(err)
			}
			doc := decodeDoc(t, b)
			prefetch, _ := doc["prefetch"].(map[string]any)
			coverages, _ := prefetch["coverage"].(map[string]any)
			entries, _ := coverages["entry"].([]any)
			first, _ := entries[0].(map[string]any)
			if !strings.HasPrefix(first["fullUrl"].(string), "http") {
				t.Fatal("fixture: the Coverage's entry has no RESTful fullUrl")
			}
			cov, _ := first["resource"].(map[string]any)
			if slot[0] == "payor" {
				list, _ := cov["payor"].([]any)
				cov["payor"] = append(list, reference(slot[1]))
			} else if slot[0] != "" {
				cov[slot[0]] = reference(slot[1])
			}
			if member != nil {
				ext, ok := doc["extension"].(map[string]any)
				if !ok {
					t.Fatal("fixture: the request carries no CDS Hooks extension")
				}
				ext["com.example.vendor"] = member
			}
			body := mustJSON(t, doc)
			for _, level := range allLevels {
				t.Run(name+vendor+"/"+level.String(), func(t *testing.T) {
					g.cfg.ConformanceEnforcement = level
					if pci, status, msg := g.ingressCRDSubjectPCIContext(ctx, body); status != 0 || pci != want {
						t.Fatalf("provider: status %d %q, pci %q, want bound to the member's own %q", status, msg, pci, want)
					}
					if pci, status, msg := g.bindInboundSubject(ctx, "example", body); status != 0 || pci != want {
						t.Fatalf("payer: status %d %q, pci %q, want bound to the member's own %q", status, msg, pci, want)
					}
				})
			}
		}
	}
}

// Well-formed requests that name a party yet carry the member's own Patient,
// kept as fixtures (testdata/wellformed-member) so that any later tightening of
// the subject derivation is checked against them: each binds the member's own
// identity on the provider's ingress and the payer's gateway, at every level.
// A CDS Hooks request's extension holds members of a vendor's own, in any
// case, and is no FHIR resource.
func TestPartyBesideCoverage_WellFormedFixturesBindTheMember(t *testing.T) {
	ctx := context.Background()
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	for file, row := range map[string]struct {
		member, want string
		provider     func([]byte) (string, int, string)
	}{
		"crd-dependent-vendor-extension.json": {"example", derivedPCI("example", "1960-01-01", "Test"),
			func(b []byte) (string, int, string) { return g.ingressCRDSubjectPCIContext(ctx, b) }},
	} {
		body, err := os.ReadFile(filepath.Join("testdata", "wellformed-member", file))
		if err != nil {
			t.Fatal(err)
		}
		for _, level := range allLevels {
			t.Run(file+"/"+level.String(), func(t *testing.T) {
				g.cfg.ConformanceEnforcement = level
				if pci, status, msg := row.provider(body); status != 0 || pci != row.want {
					t.Fatalf("provider: status %d %q, pci %q, want %q", status, msg, pci, row.want)
				}
				if pci, status, msg := g.bindInboundSubject(ctx, row.member, body); status != 0 || pci != row.want {
					t.Fatalf("payer: status %d %q, pci %q, want %q", status, msg, pci, row.want)
				}
			})
		}
	}
	entries, err := os.ReadDir(filepath.Join("testdata", "wellformed-member"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("testdata/wellformed-member holds %d files; give each a row above", len(entries))
	}
}

// A vendor's own object outside any FHIR resource that repeats a name in another case is
// no FHIR resource: the subject derivation reads the member's own Patient beside it, and
// the payer's gateway binds it at every level. (The provider's CDS Hooks parser refuses
// such a body before any subject is bound, as a body read two ways.)
func TestPartyBesideCoverage_VendorObjectRepeatingANameBindsTheMember(t *testing.T) {
	ctx := context.Background()
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	want := derivedPCI("example", "1960-01-01", "Test")
	raw, err := os.ReadFile(filepath.Join("testdata", "wellformed-member", "crd-dependent-vendor-extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	doc := decodeDoc(t, raw)
	doc["extension"].(map[string]any)["com.example.vendor"] = map[string]any{"thing": map[string]any{"fullUrl": "x", "FULLURL": "y"}}
	body := mustJSON(t, doc)
	if pci, _, err := g.resolveSubjectPCI(ctx, "example", body); err != nil || pci != want {
		t.Fatalf("derivation: pci %q (err %v), want the member's own %q", pci, err, want)
	}
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			g.cfg.ConformanceEnforcement = level
			if pci, status, msg := g.bindInboundSubject(ctx, "example", body); status != 0 || pci != want {
				t.Fatalf("payer: status %d %q, pci %q, want the member's own %q", status, msg, pci, want)
			}
			if _, status, msg := g.ingressCRDSubjectPCIContext(ctx, body); status != http.StatusBadRequest || msg != "parse cds request failed" {
				t.Fatalf("provider: status %d %q, want the CDS Hooks parser's refusal", status, msg)
			}
		})
	}
}

// The member's own Patient repeating a member name, exactly or in another case, is read
// two ways: no demographics are read from the request, and the payer's gateway binds it by
// the member id alone at every level. (The provider's CDS Hooks parser refuses such a body
// before any subject is bound.)
func TestPartyBesideCoverage_ResourceRepeatingANameIsNotRead(t *testing.T) {
	ctx := context.Background()
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	want := derivedPCI("example", "", "")
	raw, err := os.ReadFile(filepath.Join("testdata", "relayfidelity", "valid", "crd-order-sign-request.json"))
	if err != nil {
		t.Fatal(err)
	}
	for name, repeat := range map[string][2]string{
		"birthDate repeated in another case": {`"birthDate"`, `"BirthDate":"1999-01-01","birthDate"`},
		"birthDate repeated exactly":         {`"birthDate"`, `"birthDate":"1999-01-01","birthDate"`},
	} {
		doc := decodeDoc(t, raw)
		prefetch := doc["prefetch"].(map[string]any)
		patient, err := json.Marshal(prefetch["patient"])
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(patient, []byte(repeat[0])) {
			t.Fatal("fixture: the prefetch Patient carries no birthDate")
		}
		prefetch["patient"] = json.RawMessage(bytes.Replace(patient, []byte(repeat[0]), []byte(repeat[1]), 1))
		body := mustJSON(t, doc)
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				g.cfg.ConformanceEnforcement = level
				if pci, status, msg := g.bindInboundSubject(ctx, "example", body); status != 0 || pci != want {
					t.Fatalf("payer: status %d %q, pci %q, want the member id alone %q", status, msg, pci, want)
				}
				if _, status, msg := g.ingressCRDSubjectPCIContext(ctx, body); status != http.StatusBadRequest || msg != "parse cds request failed" {
					t.Fatalf("provider: status %d %q, want the CDS Hooks parser's refusal", status, msg)
				}
			})
		}
	}
}

// resourceRepeatsAName reads repeats within FHIR resources only.
func TestResourceRepeatsAName(t *testing.T) {
	for body, want := range map[string]bool{
		`{"resourceType":"Patient","id":"a","id":"b"}`:                                                       true,
		`{"resourceType":"Patient","id":"a","ID":"b"}`:                                                       true,
		`{"resourceType":"Patient","name":[{"family":"A","Family":"B"}]}`:                                    true,
		`{"ResourceType":"Patient","id":"a","Id":"b"}`:                                                       true,
		`{"id":"a","Id":"b","resourceType":"Patient"}`:                                                       true,
		`{"hook":"order-sign","extension":{"v":{"fullUrl":"x","FULLURL":"y"}}}`:                              false,
		`{"hook":"order-sign","prefetch":{"patient":{"resourceType":"Patient","id":"a"}},"x":{"a":1,"A":2}}`: false,
		`{"hook":"order-sign","prefetch":{"patient":{"resourceType":"Patient","id":"a","Id":"b"}}}`:          true,
		`{"resourceType":"Bundle","entry":[{"fullUrl":"x","FullUrl":"y"}]}`:                                  true,
		`{"resourceType":"Patient","id":"a"}`:                                                                false,
		`{"hook":"order-sign","prefetch":{},"prefetch":{}}`:                                                  true,
		`{"hook":"order-sign","extension":{"v":{"a":1,"a":2}}}`:                                              true,
		`{"resourceType":"Patient","a":{"b":1}`:                                                              true,
	} {
		if got := resourceRepeatsAName([]byte(body)); got != want {
			t.Errorf("%s: %v, want %v", body, got, want)
		}
	}
}

// Every way a Coverage's beneficiary ties the member's own Patient yields to one rule:
// beside a different Patient carried under the same id, contained or not, no form ties it,
// and beside the parent's slot the member cannot be identified. Each form's control, with
// no such Patient, binds the member's own.
func TestPartyBesideCoverage_EveryTiePathYieldsToAnotherUnderTheID(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	childs := derivedPCI(strangerMember, "2015-06-01", "Childname")
	kid := func() map[string]any { return renamedID(childPatient(), "kid") }
	other := func() map[string]any {
		return map[string]any{"resourceType": "Patient", "id": "kid", "name": []any{map[string]any{"family": "Othername"}}, "birthDate": "1999-09-09"}
	}
	slot := "Patient/" + partyParentID
	coverage := func(beneficiary string) map[string]any {
		return map[string]any{"resourceType": "Coverage", "id": "cov", "status": "active",
			"beneficiary": reference(beneficiary), "subscriber": reference(slot)}
	}
	bundle := func(entries ...any) map[string]any {
		return map[string]any{"resourceType": "Bundle", "type": "collection", "entry": entries}
	}
	entry := func(fullURL string, r any) any { return map[string]any{"fullUrl": fullURL, "resource": r} }
	for name, form := range map[string]func() map[string]any{
		"by #id among the contained": func() map[string]any {
			doc := cerNaming(t, "#kid", coverage("#kid"), kid())
			doc["zzParent"] = partyParent()
			return doc
		},
		"by type and id, a Patient with no URL": func() map[string]any {
			return map[string]any{"patient": kid(), "parent": partyParent(),
				"coverage": bundle(entry("http://ehr.example/fhir/Coverage/cov", coverage("Patient/kid")))}
		},
		"by urn": func() map[string]any {
			return bundle(entry("urn:uuid:cov", coverage("urn:uuid:k")), entry("urn:uuid:k", kid()), entry("urn:uuid:p", partyParent()))
		},
		"by an absolute URL": func() map[string]any {
			return bundle(entry("urn:uuid:cov", coverage("http://ehr.example/fhir/Patient/kid")),
				entry("http://ehr.example/fhir/Patient/kid", kid()), entry("urn:uuid:p", partyParent()))
		},
		"relative against the Coverage's RESTful base": func() map[string]any {
			return bundle(entry("http://ehr.example/fhir/Coverage/cov", coverage("Patient/kid")),
				entry("http://ehr.example/fhir/Patient/kid", kid()), entry("urn:uuid:p", partyParent()))
		},
		"an entry of another Bundle, by its RESTful URL": func() map[string]any {
			return map[string]any{"patient": bundle(entry("http://ehr.example/fhir/Patient/kid", kid())),
				"coverage": bundle(entry("http://ehr.example/fhir/Coverage/cov", coverage("Patient/kid")), entry("urn:uuid:p", partyParent()))}
		},
		"a Patient under a fullUrl that is no RESTful URL": func() map[string]any {
			return bundle(entry("urn:uuid:cov", coverage("Patient/kid")), entry("ftp://ehr.example/kid", kid()), entry("urn:uuid:p", partyParent()))
		},
		"from a Coverage in no Bundle": func() map[string]any {
			return map[string]any{"patient": bundle(entry("http://ehr.example/fhir/Patient/kid", kid())),
				"coverage": coverage("Patient/kid"), "parent": partyParent()}
		},
	} {
		t.Run(name, func(t *testing.T) {
			pci, _, err := g.resolveSubjectPCI(context.Background(), strangerMember, mustJSON(t, form()))
			if err != nil || pci != childs {
				t.Fatalf("control: pci %q (err %v), want the member's own %q", pci, err, childs)
			}
			for where, add := range map[string]func(map[string]any){
				"contained elsewhere": func(doc map[string]any) {
					doc["zzOther"] = map[string]any{"resourceType": "Claim", "id": "c", "contained": []any{other()}}
				},
				"outside any contained list": func(doc map[string]any) { doc["zzOther"] = other() },
			} {
				doc := form()
				add(doc)
				pci, _, err := g.resolveSubjectPCI(context.Background(), strangerMember, mustJSON(t, doc))
				if !errors.Is(err, errPatientUnidentifiable) || pci != "" {
					t.Fatalf("another under the id, %s: pci %q (err %v), want refused as unidentifiable", where, pci, err)
				}
			}
		})
	}
}
