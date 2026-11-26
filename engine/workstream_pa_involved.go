//go:build !shn_coreonly

package engine

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"sort"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// paRequestInvolvedLegs are the PA legs whose request names the other
// patients it carries and, but on the PAS content notification, whose answer
// names the payer's own binding of the member when it differs from the leg
// token's patient: the prior-authorization legs, coverage eligibility, whose
// batch names a patient for each request, and the PAS topic's content
// notification, whose decision may name more than one: an exchange is
// recorded under every patient involved.
var paRequestInvolvedLegs = map[string]bool{
	"pas-subscription-notify": true,
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
	// The PAS content notification's answer is the provider's endpoint's,
	// carried as it answered: no payer binding to name.
	return in, in && leg != legPASSubscriptionNotify
}

// requestMembers is every member a request on PA leg names, each read exactly as
// that leg's subject binding reads it. A coverage-eligibility input names the
// member of each request it submits, read as its binding reads it
// (eligibilitySubmissions, eligibilitySubmissionMember) and bound from the
// resources carried with that request; every other leg names carriedMembers,
// bound from the whole request. Off coverage eligibility the request is read from its
// strict view (strictView), once for every reader here; a non-empty request no
// view can be made of is errRequestPatientsUnreadable, never read as naming no one.
// On coverage eligibility, a body eligibilitySubmissions cannot read, empty or
// absent included, is errRequestPatientsUnreadable too, and each request it
// submits whose own member cannot be read is an unreadable member, never left
// out as naming no one.
func (w paWorkstream) requestMembers(leg string, payload []byte) ([]namedMember, error) {
	return w.requestMembersIn(context.Background(), leg, payload)
}

// requestMembersIn is requestMembers, the PAS content notification's read
// at the payer's FHIR base its gateway states, which the notification's
// context carries (withNotifiedBase), as its binding reads it.
func (paWorkstream) requestMembersIn(ctx context.Context, leg string, payload []byte) ([]namedMember, error) {
	if leg == legPASSubscriptionNotify {
		return pasNotifiedMembers(payload, notifiedBaseFrom(ctx))
	}
	var out []namedMember
	if leg == "coverage-eligibility" {
		// An eligibility request always names its member, so a body whose
		// requests cannot be read, an empty or absent one included, is one
		// whose patients are unknown: errRequestPatientsUnreadable, counted as
		// an omission and never read as naming no one. So is each request in
		// it whose own member cannot be read: an unreadable member, counted
		// once, beside every member the other requests name; a request whose
		// reference names no one (errMemberNamesNoOne) is such a member.
		// Which routes reach each case: the leg's ingress route
		// reads the body with eligibilitySubmissions and each request's member
		// with eligibilitySubmissionMember, and refuses either error 400 first
		// ("parse member failed" for the member, or its own text for one that
		// names no one), so only an embedder's direct
		// OriginateLeg sends a body or a request this cannot read. Module 1
		// builds its own request with the SDK's builder for a member its system
		// of record holds, so it never sends any of these.
		subs, err := eligibilitySubmissions(payload)
		if err != nil {
			return nil, errRequestPatientsUnreadable
		}
		for _, sub := range subs {
			// A request whose patient reference names no one is a read error
			// (errMemberNamesNoOne), never a blank member: it is counted as
			// unreadable, and the system of record is never read for it.
			m, err := eligibilitySubmissionMember(sub)
			if err != nil {
				out = append(out, namedMember{unreadable: true})
				continue
			}
			out = append(out, namedMember{member: m, scope: sub.scope, ownScope: true, subjectResource: memberSubjectResource(eligibilitySubjectResource(sub), m)})
		}
		return out, nil
	}
	if len(payload) == 0 {
		return nil, nil
	}
	view, err := strictView(payload)
	if err != nil {
		return nil, err
	}
	pas := leg == "pas-claim" || leg == "pas-claim-update"
	var subject, shared map[string]bool
	if pas {
		subject, shared = pasSubjectNames(view)
	}
	members, unreadable := carriedMembersIn(view, leg)
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
		out = append(out, namedMember{member: m, scope: payload, pas: pas, subject: subject[m] && !shared[m], shared: shared[m], legName: legName != "" && m == legName,
			subjectResource: legSubjectResource(leg, payload, m)})
	}
	// A reference to a patient that names none the leg can read (carriedMembersIn)
	// leaves the request's patients unknown, never fewer: one unreadable member,
	// recorded once as an omission beside the members it does name.
	if unreadable {
		out = append(out, namedMember{unreadable: true})
	}
	return out, nil
}

// crdSubjectLegs are the CRD legs whose own patient is context.patientId.
var crdSubjectLegs = map[string]bool{"crd-order-select": true, "crd-order-dispatch": true}

// crdPatientID is a CDS Hooks request's context.patientId as the leg's subject
// binding reads it (crdContextPatientID), "" when it names none, names no
// Patient by its structure, or names it by a value that is not a string (which
// the binding refuses).
func crdPatientID(payload []byte) string {
	var req struct {
		Context map[string]any `json:"context"`
	}
	if decodeMessage(payload, &req) != nil {
		return ""
	}
	id, _ := req.Context["patientId"].(string)
	member, _, noPatient := crdContextPatientID(id)
	if noPatient {
		return ""
	}
	return member
}

// pasSubjectNames are the names a PAS submit or update request gives its own
// patient, decided by the request's structure alone, never by what any name
// binds to: the name the operative Claim's patient reference reads as (the id of a
// Patient/<id> reference, else the reference itself, as carriedMembers names a
// Patient by its fullUrl or #id); the member the PAS binding binds a "#<id>" or
// URN to (pasPatientRefMember), never a contained target's local id; and, when
// that reference resolves to exactly one Patient entry
// (shnsdk.BundleReferenceURL to the entry's fullUrl, or a relative Patient/<id>
// to the entry's id), the names that entry is read under: its fullUrl, its id
// and its member identifiers (shnsdk.MemberSystem). shared
// are the entry's names another Patient the request carries is read under too,
// at any depth: those are no name of the subject alone, so a member under one
// is unknown, never the subject (two Patients are never one person), and so is
// Claim.patient's own name when two carried Patients go by it. Only the
// operative Claim, the first, decides, as the PAS subject binding reads it.
// view is the request's strict view (strictView), the one carriedMembers
// reads, so the Patients it counts are the ones carriedMembers names. The
// Claim's "patient" and its type "Claim" are read exactly, not through the
// view's names: the PAS readers before it (readConformantPASSubjects, on the
// requester's ingress and the payer's) refuse a "patient" named in another case
// (a member that matches a field only by case, relay.Decode) and find no Claim
// in a resource typed otherwise than exactly "Claim", so a request read here
// names its Claim as they read it.
func pasSubjectNames(view canonicalView) (subject, shared map[string]bool) {
	doc := view.doc
	bundle, _ := doc.(map[string]any)
	entries, _ := bundle["entry"].([]any)
	readName := pasSubjectName
	patientNames := pasPatientNames
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
	// A "#<id>" or URN names the Patient it resolves to, by the request's
	// structure: the member the PAS binding binds it to (pasPatientRefMember)
	// is the subject's name too, under the same rule (a name another carried
	// Patient goes by is shared, never the subject's alone). A contained
	// Patient's own id is local to the Claim, never a member: a reference
	// elsewhere naming Patient/<that id> names someone else.
	claim, _ := claimEntry["resource"].(map[string]any)
	if m, _, err := pasPatientRefMember(bundle, claim, ref); err == nil && m != "" && m != own {
		if count[m] > 1 {
			shared[m] = true
		} else {
			subject[m] = true
		}
	}
	if p, matchURL, ok := pasSubjectEntry(entries, claimEntry, ref); ok {
		for _, n := range patientNames(p, matchURL) {
			if count[n] > 1 {
				shared[n] = true
				continue
			}
			subject[n] = true
		}
	}
	return subject, shared
}

// pasSubjectEntry is the one Patient entry of a PAS Bundle that the operative Claim's
// patient reference (ref, in claimEntry) names: the entry whose fullUrl the reference
// resolves to (shnsdk.BundleReferenceURL), or, for a relative "Patient/<id>", the Patient
// entry with that id. ok is false for none or more than one.
func pasSubjectEntry(entries []any, claimEntry map[string]any, ref string) (patient map[string]any, fullURL string, ok bool) {
	resolved := shnsdk.BundleReferenceURL(claimEntry, ref)
	relativeID := ""
	if id, _, ok := readPatientReference(ref); ok && strings.HasPrefix(ref, "Patient/") {
		relativeID = id
	}
	var match []map[string]any
	var matchURL string
	for _, pe := range entries {
		pm, _ := pe.(map[string]any)
		p, _ := pm["resource"].(map[string]any)
		if p == nil || p["resourceType"] != "Patient" {
			continue
		}
		u, _ := pm["fullUrl"].(string)
		id, _ := p["id"].(string)
		if (u != "" && u == resolved) || (relativeID != "" && id == relativeID) {
			match = append(match, p)
			matchURL = u
		}
	}
	if len(match) != 1 {
		return nil, "", false
	}
	return match[0], matchURL, true
}

// pasSubjectResource is a PAS submission's, update's or inquiry's subject resource
// (resolveSubjectPCIFrom): the one Patient its operative Claim's patient reference
// resolves to. A "#<id>" or URN reference resolves as claimPatientTarget reads it, any
// other as pasSubjectEntry does. nil when it resolves to none, or the Bundle cannot be
// read as a JSON object.
func pasSubjectResource(bundleJSON []byte) []byte {
	var doc map[string]any
	if json.Unmarshal(bundleJSON, &doc) != nil {
		return nil
	}
	entries, _ := doc["entry"].([]any)
	for _, e := range entries {
		em, _ := e.(map[string]any)
		claim, _ := em["resource"].(map[string]any)
		if claim == nil || claim["resourceType"] != "Claim" {
			continue
		}
		slot, _ := claim["patient"].(map[string]any)
		ref, _ := slot["reference"].(string)
		if ref == "" {
			return nil
		}
		var p map[string]any
		if strings.HasPrefix(ref, "#") || strings.HasPrefix(ref, "urn:") {
			p, _ = claimPatientTarget(doc, claim, ref)
		} else {
			p, _, _ = pasSubjectEntry(entries, em, ref)
		}
		if p == nil {
			return nil
		}
		raw, err := json.Marshal(p)
		if err != nil {
			return nil
		}
		return raw
	}
	return nil
}

// pasSubjectName is the name a reference or a fullUrl reads as: the id of a
// Patient reference by FHIR's grammar, else the reference itself
// (pasSubjectNames, pasMemberFromRef).
func pasSubjectName(ref string) string {
	return pasMemberFromRef(ref)
}

// pasPatientNames are the names a Patient is read under (pasSubjectNames):
// its entry's fullUrl read as a reference (fullURL, "" when it is no entry's
// resource), its id and its member identifiers (shnsdk.MemberSystem).
func pasPatientNames(r map[string]any, fullURL string) []string {
	var out []string
	if fullURL != "" {
		out = append(out, pasSubjectName(fullURL))
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

// carriedMembers is every member a request on leg names, sorted and each
// once, each read exactly as that leg's subject binding reads the member a
// reference names, so every reference to the leg's own patient reads as the
// subject does:
//   - a reference with "Patient/" names what the leg's reader returns, the id
//     without a version suffix: on the DTR package the id after it
//     (patientMember); on every other leg the id a Patient reference names
//     by FHIR's grammar (readPatientReference, the PAS and inquiry readers
//     agreeing). A reference to a patient (patientLike) that names no Patient
//     so, and resolves to none the request carries, names no member: its
//     patient is unknown, and the involved pass records the omission
//     (carriedMembersIn's unreadable). A versioned
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
// its request uses. The request is read from its canonical view
// (strictView): decoded as the leg's deciding readers decode it, with each
// name these rules read, and each resource type they compare, as FHIR spells
// it within each FHIR resource whatever the case it was sent in, so a Patient under "Contained", a
// "resourceType":"patient" or a {"Reference":"Patient/p2"} is named as a
// lenient reader reads it. A request no view can be made of (one that is not
// well-formed JSON, or repeats a member name, exactly or in another case, so
// that it can be read two ways) names none here; requestMembers reports it
// (errRequestPatientsUnreadable), and the involved pass records the omission
// rather than read it as naming no one.
func carriedMembers(payload []byte, leg string) []string {
	if len(payload) == 0 {
		return nil
	}
	view, err := strictView(payload)
	if err != nil {
		return nil
	}
	members, _ := carriedMembersIn(view, leg)
	return members
}

// carriedMembersIn is carriedMembers over a request's canonical view. Every
// identity it takes and compares (the party entries and slots) is of that view.
// unreadable reports a reference to a patient (patientLike) that names no
// Patient by the leg's reader and resolves to none the request carries: its
// patient is unknown, and the involved pass records the omission, never a
// request naming fewer patients, on every leg.
func carriedMembersIn(view canonicalView, leg string) (members []string, unreadable bool) {
	doc := view.doc
	read := func(ref string) (string, bool) {
		id, _, ok := readPatientReference(ref)
		return id, ok
	}
	// Only the PAS binding names a carried Patient by the reference to it; the
	// others (CRD, the DTR package, the inquiry) name it by its id.
	byReference := leg == "pas-claim" || leg == "pas-claim-update"
	partyEntries, partySlots := entryPartyReads(view)
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
				// reference names no one. Decided by the element, by its name in
				// the canonical view, never by whether the party is carried.
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
			continue
		}
		if patientLike(ref) {
			unreadable = true
		}
	}
	out := make([]string, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	sort.Strings(out)
	return out, unreadable
}

// entryPartyReads are, when the view is a Bundle, the resources of its Patient
// entries that are a Coverage's party (shnsdk.CoverageEntryParties, read once
// for the Bundle), and the subscriber and policyHolder of its Coverage entries
// whose reference resolves to one from the Coverage's entry
// (shnsdk.BundleReferenceURL), each by its identity in the view. The view is
// the request's (strictView, or bindingView in the binding), the one its caller walks,
// so the SDK's exact-name rules read the names a lenient reader reads.
func entryPartyReads(view canonicalView) (parties, slots map[uintptr]bool) {
	bundle, _ := view.doc.(map[string]any)
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
