//go:build !shn_coreonly

package engine

import (
	"reflect"
	"slices"
	"sort"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// paRequestInvolvedLegs are the PA legs whose request names the other
// patients it carries and whose answer names the payer's own binding of the
// member when it differs from the leg token's patient: the
// prior-authorization legs, and coverage eligibility, whose batch names a
// patient for each request.
var paRequestInvolvedLegs = map[string]bool{
	"coverage-eligibility":    true,
	"crd-order-select":        true,
	"crd-order-dispatch":      true,
	"dtr-questionnaire-fetch": true,
	"pas-claim":               true,
	"pas-claim-update":        true,
	"pas-claim-inquire":       true,
}

// involvement reports whether a request on PA leg names the other patients
// it carries, and whether its answer names the payer's own binding: both on
// paRequestInvolvedLegs, neither on the others.
func (paWorkstream) involvement(leg string) (request, answer bool) {
	in := paRequestInvolvedLegs[leg]
	return in, in
}

// requestMembers is every member a request on PA leg names, each read exactly as
// that leg's subject binding reads it. A coverage-eligibility input names the
// member of each request it submits, read as its binding reads it
// (eligibilitySubmissions, eligibilitySubmissionMember) and bound from the
// resources carried with that request; every other leg names carriedMembers,
// bound from the whole request.
func (paWorkstream) requestMembers(leg string, payload []byte) []namedMember {
	var out []namedMember
	if leg == "coverage-eligibility" {
		subs, err := eligibilitySubmissions(payload)
		if err != nil {
			return nil
		}
		for _, sub := range subs {
			if m, err := eligibilitySubmissionMember(sub); err == nil && m != "" {
				out = append(out, namedMember{member: m, scope: sub.scope, ownScope: true})
			}
		}
		return out
	}
	pas := leg == "pas-claim" || leg == "pas-claim-update"
	var subject, shared map[string]bool
	if pas {
		subject, shared = pasSubjectNames(payload)
	}
	members := carriedMembers(payload, leg)
	// On CRD the leg's own patient is context.patientId, by the request's
	// structure, as the leg's subject binding reads it. When no resource the
	// request carries references it, it is still the name subjectCarriedNames
	// counts its carried Patient's other names under: a member that is the
	// subject, with no read, never named or left out. Referenced, it is a
	// member as any other, read as its binding reads it, and still one of the
	// subject's names subjectCarriedNames counts under (legName).
	var legName string
	if crdSubjectLegs[leg] {
		legName = crdPatientID(payload)
		if legName != "" && !slices.Contains(members, legName) {
			out = append(out, namedMember{member: legName, scope: payload, subject: true})
		}
	}
	for _, m := range members {
		out = append(out, namedMember{member: m, scope: payload, pas: pas, subject: subject[m] && !shared[m], shared: shared[m], legName: legName != "" && m == legName})
	}
	return out
}

// crdSubjectLegs are the CRD legs whose own patient is context.patientId.
var crdSubjectLegs = map[string]bool{"crd-order-select": true, "crd-order-dispatch": true}

// crdPatientID is a CDS Hooks request's context.patientId as the leg's subject
// binding reads it (a "Patient/" prefix taken off, as memberForPCI does), "" when
// it names none or names it by a value that is not a string (which the
// binding refuses).
func crdPatientID(payload []byte) string {
	var req struct {
		Context map[string]any `json:"context"`
	}
	if decodeMessage(payload, &req) != nil {
		return ""
	}
	id, _ := req.Context["patientId"].(string)
	return strings.TrimPrefix(id, "Patient/")
}

// pasSubjectNames are the names a PAS submit or update request gives its own
// patient, decided by the request's structure alone, never by what any name
// binds to: the name the operative Claim's patient reference reads as (the id of a
// Patient/<id> reference, else the reference itself, as carriedMembers names a
// Patient by its fullUrl or #id), and, when that reference resolves to exactly
// one Patient entry (shnsdk.BundleReferenceURL to the entry's fullUrl, or a
// relative Patient/<id> to the entry's id), the names that entry is read under:
// its fullUrl, its id and its member identifiers (shnsdk.MemberSystem). shared
// are the entry's names another Patient the request carries is read under too,
// at any depth: those are no name of the subject alone, so a member under one
// is unknown, never the subject (two Patients are never one person), and so is
// Claim.patient's own name when two carried Patients go by it. Only the
// operative Claim, the first, decides, as the PAS subject binding reads it.
func pasSubjectNames(payload []byte) (subject, shared map[string]bool) {
	var doc any
	if decodeMessage(payload, &doc) != nil {
		return nil, nil
	}
	bundle, _ := doc.(map[string]any)
	entries, _ := bundle["entry"].([]any)
	readName := func(ref string) string {
		if strings.Contains(ref, "Patient/") {
			return pasMemberFromRef(ref)
		}
		return ref
	}
	// patientNames are the names a Patient is read under; fullURL is its
	// entry's, when it is an entry's resource.
	patientNames := func(r map[string]any, fullURL string) []string {
		var out []string
		if fullURL != "" {
			out = append(out, readName(fullURL))
		}
		if id, ok := r["id"].(string); ok && id != "" {
			out = append(out, id)
		}
		ids, _ := r["identifier"].([]any)
		for _, i := range ids {
			im, _ := i.(map[string]any)
			if v, ok := im["value"].(string); ok && v != "" && im["system"] == shnsdk.MemberSystem {
				out = append(out, v)
			}
		}
		return out
	}
	// count is, for each name, the Patients the request carries under it, at
	// any depth (an entry's, a contained one, one in a nested Bundle). A
	// contained Patient is counted under "#<id>" too, the name carriedMembers
	// gives it on these legs.
	count := map[string]int{}
	var walk func(node any, fullURL string, contained bool)
	walk = func(node any, fullURL string, contained bool) {
		switch v := node.(type) {
		case map[string]any:
			if v["resourceType"] == "Patient" {
				names := patientNames(v, fullURL)
				if id, ok := v["id"].(string); ok && id != "" && contained {
					names = append(names, "#"+id)
				}
				seen := map[string]bool{}
				for _, n := range names {
					if !seen[n] {
						seen[n] = true
						count[n]++
					}
				}
			}
			entryURL, _ := v["fullUrl"].(string)
			for k, child := range v {
				switch {
				case k == "resource" && entryURL != "":
					walk(child, entryURL, false)
				case k == "contained":
					walk(child, "", true)
				default:
					walk(child, "", false)
				}
			}
		case []any:
			for _, child := range v {
				walk(child, "", contained)
			}
		}
	}
	walk(doc, "", false)
	// The operative Claim, the first Claim entry, as the PAS subject binding
	// reads it (readConformantPASSubjects): a prior Claim an update carries
	// decides nothing here.
	var claimEntry map[string]any
	var ref string
	for _, e := range entries {
		em, _ := e.(map[string]any)
		claim, _ := em["resource"].(map[string]any)
		if claim == nil || claim["resourceType"] != "Claim" {
			continue
		}
		slot, _ := claim["patient"].(map[string]any)
		ref, _ = slot["reference"].(string)
		claimEntry = em
		break
	}
	if ref == "" {
		return nil, nil
	}
	subject, shared = map[string]bool{}, map[string]bool{}
	own := readName(ref)
	// Claim.patient's own name is the subject's, unless two Patients the
	// request carries go by it: then it names no one alone, and is counted.
	if count[own] > 1 {
		shared[own] = true
	} else {
		subject[own] = true
	}
	resolved := shnsdk.BundleReferenceURL(claimEntry, ref)
	relativeID := ""
	if strings.HasPrefix(ref, "Patient/") {
		relativeID = pasMemberFromRef(ref)
	}
	var match []map[string]any
	var matchURL string
	for _, pe := range entries {
		pm, _ := pe.(map[string]any)
		p, _ := pm["resource"].(map[string]any)
		if p == nil || p["resourceType"] != "Patient" {
			continue
		}
		fullURL, _ := pm["fullUrl"].(string)
		id, _ := p["id"].(string)
		if (fullURL != "" && fullURL == resolved) || (relativeID != "" && id == relativeID) {
			match = append(match, p)
			matchURL = fullURL
		}
	}
	if len(match) == 1 {
		for _, n := range patientNames(match[0], matchURL) {
			if count[n] > 1 {
				shared[n] = true
				continue
			}
			subject[n] = true
		}
	}
	return subject, shared
}

// carriedMembers is every member a request on leg names, sorted and each
// once, each read exactly as that leg's subject binding reads the member a
// reference names, so every reference to the leg's own patient reads as the
// subject does:
//   - a reference with "Patient/" names what the leg's reader returns, the id
//     without a version suffix: on the DTR package the id after it
//     (patientMember); on every other leg the id after the last "Patient/"
//     (pasMemberFromRef, the PAS and inquiry readers agreeing). A versioned
//     reference names the patient it versions, so a version suffix neither
//     hides another member nor names the leg's own patient a second time;
//   - a carried Patient names what the leg binds it by: on PAS submit and
//     update, which bind a Patient only through the reference to it, its
//     entry's fullUrl read as a reference, "#<id>" when contained, else its
//     id; on every other leg (CRD, the DTR package, the inquiry), its id;
//   - a urn:uuid: or #contained reference names a Patient only when the
//     request carries it, and then names what that Patient names; a
//     Coverage's party (a contained Patient its subscriber or policyHolder
//     names, shnsdk.CoverageParty, or a Patient entry of the request Bundle
//     that only the subscriber or policyHolder of its Coverage entries name,
//     shnsdk.CoverageEntryParties) is no member, and neither it nor the slot's
//     reference to it names one; but a patient the party names is;
//   - a Coverage's subscriber or policyHolder element names no member on any
//     leg, whatever its reference names: a bare reference to a party the
//     request does not carry (a dependent's parent, as origination sends it on
//     CRD and DTR) names no one either.
//
// A Patient's member identifier is never read: the subject is bound by the id
// its request uses. An unreadable request names none. The request is read as
// the leg's deciding readers read it (decodeMessage): one that repeats a
// member name, exactly or in another case, can be read two ways, so it too
// names none, and the Hub never records an exchange under a patient those
// readers did not see.
func carriedMembers(payload []byte, leg string) []string {
	var doc any
	if len(payload) == 0 || decodeMessage(payload, &doc) != nil {
		return nil
	}
	read := func(ref string) (string, bool) {
		if !strings.Contains(ref, "Patient/") {
			return "", false
		}
		return pasMemberFromRef(ref), true
	}
	if leg == "dtr-questionnaire-fetch" {
		read = patientMember
	}
	// Only the PAS binding names a carried Patient by the reference to it; the
	// others (CRD, the DTR package, the inquiry) name it by its id.
	byReference := leg == "pas-claim" || leg == "pas-claim-update"
	partyEntries, partySlots := entryPartyReads(doc)
	if partySlots == nil {
		partySlots = map[uintptr]bool{}
	}
	set := map[string]bool{}
	pointers := map[string]string{} // a carried Patient's fullUrl or #id → what it names
	var refs []string
	var walk func(node any, fullURL string, contained bool)
	walk = func(node any, fullURL string, contained bool) {
		switch v := node.(type) {
		case map[string]any:
			if partyEntries[reflect.ValueOf(v).Pointer()] {
				// A Coverage's party carried as an entry of its own: only its
				// own identity is passed over. Its elements are walked as any
				// resource's are, so a patient it names is involved, as for a
				// contained party.
				for _, member := range v {
					walk(member, "", false)
				}
				return
			}
			if v["resourceType"] == "Coverage" {
				// A Coverage's party, the person its subscriber or policyHolder
				// element names, is no member, whether the request carries that
				// person or names them by a bare reference: the element's
				// reference names no one. Decided by the element, by FHIR's
				// exact (case-sensitive) names, never by whether the party is
				// carried.
				for _, k := range []string{"subscriber", "policyHolder"} {
					if slot, ok := v[k].(map[string]any); ok {
						partySlots[reflect.ValueOf(slot).Pointer()] = true
					}
				}
			}
			if v["resourceType"] == "Patient" {
				id, _ := v["id"].(string)
				names := id
				switch {
				case byReference && fullURL != "":
					names = fullURL
					if m, ok := read(fullURL); ok {
						names = m
					}
				case byReference && contained && id != "":
					names = "#" + id
				}
				if fullURL != "" {
					pointers[fullURL] = names
				}
				if contained && id != "" {
					pointers["#"+id] = names
				}
				if names != "" {
					set[names] = true
				}
			}
			if ref, ok := v["reference"].(string); ok && !partySlots[reflect.ValueOf(v).Pointer()] {
				refs = append(refs, ref)
			}
			entryURL, _ := v["fullUrl"].(string)
			for k, child := range v {
				switch {
				case k == "resource" && entryURL != "":
					walk(child, entryURL, false)
				case k == "contained":
					// A Coverage's party (shnsdk.CoverageParty, which answers
					// for a Coverage only) is another person the Coverage
					// names, never a member of the request: neither it nor a
					// reference to it names one. Only its own identity is
					// passed over: its elements are walked as any resource's
					// are, so what the party names (another patient it links
					// to, say) is read like the rest of the request, and that
					// patient is involved too. A contained that is not a list
					// is walked whole.
					list, ok := child.([]any)
					if !ok {
						walk(child, "", true)
						continue
					}
					for _, c := range list {
						if cr, ok := c.(map[string]any); ok && shnsdk.CoverageParty(v, cr) {
							for _, member := range cr {
								walk(member, "", false)
							}
							continue
						}
						walk(c, "", true)
					}
				default:
					walk(child, "", false)
				}
			}
		case []any:
			for _, child := range v {
				walk(child, "", contained)
			}
		}
	}
	walk(doc, "", false)
	for _, ref := range refs {
		if m, ok := read(ref); ok {
			if m != "" {
				set[m] = true
			}
			continue
		}
		if m, ok := pointers[ref]; ok {
			set[m] = true
		}
	}
	out := make([]string, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// entryPartyReads are, when doc is a Bundle, the resources of its Patient
// entries that are a Coverage's party (shnsdk.CoverageEntryParties, read once
// for the Bundle), and the subscriber and policyHolder of its Coverage entries
// whose reference resolves to one from the Coverage's entry
// (shnsdk.BundleReferenceURL), each by its identity in doc.
func entryPartyReads(doc any) (parties, slots map[uintptr]bool) {
	bundle, _ := doc.(map[string]any)
	set := shnsdk.CoverageEntryParties(bundle)
	if len(set) == 0 {
		return nil, nil
	}
	parties, slots = map[uintptr]bool{}, map[uintptr]bool{}
	entries, _ := bundle["entry"].([]any)
	for _, e := range entries {
		em, _ := e.(map[string]any)
		res, ok := em["resource"].(map[string]any)
		if !ok {
			continue
		}
		if fullURL, _ := em["fullUrl"].(string); set[fullURL] {
			parties[reflect.ValueOf(res).Pointer()] = true
		}
		if res["resourceType"] != "Coverage" {
			continue
		}
		for _, k := range []string{"subscriber", "policyHolder"} {
			slot, ok := res[k].(map[string]any)
			if ref, _ := slot["reference"].(string); ok && ref != "" && set[shnsdk.BundleReferenceURL(em, ref)] {
				slots[reflect.ValueOf(slot).Pointer()] = true
			}
		}
	}
	return parties, slots
}
