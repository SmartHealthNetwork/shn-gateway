package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"

	"github.com/SmartHealthNetwork/shn-gateway/internal/jsonmember"
)

// The readers that name the patients a request carries (the involved list,
// carriedMembers), bind its member from a Patient it carries
// (visitCarriedPatients and the readers built on it) and decide a Coverage's
// party (partySlotsOf, partyNamed, shnsdk.CoverageParty,
// shnsdk.CoverageEntryParties, shnsdk.BundleReferenceURL) read member names
// and resource types by their exact FHIR spellings. A lenient FHIR reader
// folds them: a Patient under "Contained", with "resourceType":"patient", or
// named by {"Reference":"Patient/p2"} is the same Patient to it. So each of
// those readers reads one canonical view of the request (canonicalNames), in
// which every name they read takes its FHIR spelling whatever the case it was
// sent in, and the readers' exact-name walks then read what a lenient reader
// reads.
//
// The view is for reading only: the involved list, the binding and the party
// rule. It is never encoded into anything sent; the bytes a gateway carries
// stay exactly as the participant sent them.

// viewNames are the member names the view spells as FHIR does: every name the
// readers above read, the party rule's own (partyRuleNames) among them. A name
// missing here is one a lenient reader reads that the involved list and the
// binding would not; TestCanonicalNames_EveryRowReadsTheSameRecased fails for
// it. The demographics a patient identifier is derived from (birthDate, name,
// family) are not here: they are the identity rule's facts, read as
// PatientDemographics reads them, and that rule is not changed by the view.
//
// It is derived from the readers' own lists, so it cannot drift from them: the
// party rule's names (partyRuleNames), the names a beneficiary reference is
// resolved by (tieNames: a Bundle's entry, fullUrl and resource, a Parameters'
// parameter, the beneficiary), and walkNames, the names the walks read that no
// list of a reader's own holds. A Coverage's payor is read in any case already
// (partySlotsOf), so it is not listed.
var viewNames = func() []string {
	names := slices.Concat(partyRuleNames, tieNames, walkNames)
	slices.Sort(names)
	return slices.Compact(names)
}()

// walkNames are the names the readers read with no list of their own: an
// identifier's system and value (patientIsMember, partyNamed,
// shnsdk.CoverageEntryParties), and a versioned reference's target, its meta and
// versionId (shnsdk.CoverageEntryParties).
var walkNames = []string{"system", "value", "meta", "versionId"}

// viewTypes are the resource types the readers compare a resourceType with:
// in the view, a resourceType whose value is one of them in another case is
// spelled as FHIR does.
var viewTypes = []string{"Patient", "Coverage", "Bundle"}

// viewNameSpelling and viewTypeSpelling map the folded form of each of viewNames, and of viewTypes,
// to its FHIR spelling.
var viewNameSpelling, viewTypeSpelling = foldedSpellings(viewNames), foldedSpellings(viewTypes)

func foldedSpellings(names []string) map[string]string {
	out := make(map[string]string, len(names))
	for _, n := range names {
		out[jsonmember.FoldedName(n)] = n
	}
	return out
}

// errRequestPatientsUnreadable is a non-empty message whose patients cannot be read: on a
// leg that names its patients for the involved list, the strict decode the leg's deciding
// readers make refuses it (strictView); in a binding, the binding's own decode does
// (bindingView). It is never read as carrying no patient. The involved list records the
// omission and sends the request with no list (requestNamedPatients); a binding from the
// message's own bytes refuses it 422, recorded as authority, rather than derive the
// member's identifier from the member id alone (resolveBinding). Every leg's own readers
// refuse such a message first.
var errRequestPatientsUnreadable = errors.New(msgRequestPatientsUnreadable)

const msgRequestPatientsUnreadable = "cannot read the request's patients: it is not one JSON document that names each member of a FHIR resource once, in any case"

// canonicalView is a request's canonical view, as strictView, bindingView and viewOf make
// it: the one value the shared readers of a request's patients read (carriedMembersIn,
// pasSubjectNames, entryPartyReads, visitCarriedPatients and the readers built on it).
// Its zero value carries no patient. It is a named type, so a reader cannot be handed the
// request's bytes or any other value in its place; only those three makers build one.
//
// It is not comparable: two views compared with == or != fail to compile. doc holds a
// decoded request, a map, and comparing interfaces that hold maps panics at run time, so
// a view is never compared; its zero value already reads as no patient everywhere
// (TestCanonicalView_IsNotComparable).
type canonicalView struct {
	_   [0]func() // first, so the zero-size field adds no padding
	doc any
}

// strictView is the request's value as the involved list reads it: decoded as the leg's
// deciding readers decode it (decodeMessage: relay.Decode, numbers kept as json.Number, so
// a decimal past float64's range such as 1e400 reads, and a member name repeated in any
// object, exactly or in another case, refused), then canonicalised (canonicalNames).
func strictView(payload []byte) (canonicalView, error) {
	var doc any
	if err := decodeMessage(payload, &doc); err != nil {
		return canonicalView{}, errRequestPatientsUnreadable
	}
	return canonicalView{doc: canonicalNames(doc)}, nil
}

// bindingView is the request's value as the binding reads it: decoded by encoding/json
// (so a body past the relay's own limits, deeper than its nesting bound or carrying a
// byte or escape it does not read, is read as before), with numbers kept as json.Number,
// and refused exactly where the binding refused before: a body that is not one JSON
// document, or that repeats a member name exactly in any object, or in another case
// within a FHIR resource (resourceRepeatsAName). A name spelled two ways in a vendor's own
// object outside any resource is read, and not renamed (canonicalNames).
func bindingView(payload []byte) (canonicalView, error) {
	if resourceRepeatsAName(payload) {
		return canonicalView{}, errRequestPatientsUnreadable
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var doc any
	if dec.Decode(&doc) != nil {
		return canonicalView{}, errRequestPatientsUnreadable
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return canonicalView{}, errRequestPatientsUnreadable
	}
	return canonicalView{doc: canonicalNames(doc)}, nil
}

// canonicalNames is doc, a decoded request, with each member of a FHIR resource whose
// name folds equal (jsonmember.FoldedName) to one of viewNames named as FHIR spells it,
// and each resourceType whose value folds equal to one of viewTypes spelled as FHIR does.
// A FHIR resource is an object carrying a resourceType member in any case; its members
// are renamed down to the next resource nested in it, which is a resource of its own.
// Nothing outside every resource (a CDS Hooks request's own members, a vendor's
// extension object) is renamed. Both views it is applied to refuse a name repeated in
// another case within a resource, so no two members of an object take one name. Every
// map in the result is new: identities (reflect.ValueOf(m).Pointer()) are taken and
// compared within one result only. The result is for reading only, never encoded into
// anything sent.
func canonicalNames(doc any) any { return canonicalValue(doc, false) }

// canonicalValue is v renamed as canonicalNames names it; inResource is whether v is
// within a FHIR resource.
func canonicalValue(v any, inResource bool) any {
	switch t := v.(type) {
	case map[string]any:
		resource := inResource || isResource(t)
		out := make(map[string]any, len(t))
		for k, child := range t {
			if n, ok := viewNameSpelling[jsonmember.FoldedName(k)]; ok && resource {
				k = n
			}
			if s, ok := child.(string); ok && resource && k == "resourceType" {
				if typ, ok := viewTypeSpelling[jsonmember.FoldedName(s)]; ok {
					child = typ
				}
			}
			out[k] = canonicalValue(child, resource)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, child := range t {
			out[i] = canonicalValue(child, inResource)
		}
		return out
	}
	return v
}

// isResource reports whether m carries a resourceType member, in any case.
func isResource(m map[string]any) bool {
	for k := range m {
		if jsonmember.FoldedName(k) == foldedResourceType {
			return true
		}
	}
	return false
}

var foldedResourceType = jsonmember.FoldedName("resourceType")
