package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// resolveSubjectPCI binds a member id to its network patient identifier (pci) through this
// holder's OWN system of record. It is the one read the Da Vinci CRD, DTR and PAS legs make
// to bind a subject, on the ingress (origination) side and the inbound (payer) side alike.
// payload is the request the member arrived in, read only when the system of record does
// not hold the member (below); nil when the leg has none.
//
// By default a member the system of record does not hold is bound, not refused, from the
// same three facts a
// holder's record supplies — member id, birthDate and family name — read from the Patient
// the request itself carries for that member (PatientDemographics, the read this holder's
// FHIR system of record makes of its own Patient), or from the member id alone when the
// request carries no such Patient. Nothing is minted: every fact is one the partner sent.
// The payer side binds the member the same way, by its own system first
// (bindInboundSubject). A request carrying Patients for the member that disagree with
// each other binds by id alone. A participant that opts in to Config.RequireKnownMembers
// refuses such a member instead. Everything else on these legs is untouched: member-mixing
// refusals, prefetch read only from this system or the request, and the payer's own
// independent member resolution.
//
// A read failure is returned as is, with the flag set or not: an unreadable system of record
// is never mistaken for a member it does not hold.
func (g *Gateway) resolveSubjectPCI(ctx context.Context, member string, payload []byte) (pci string, found bool, readErr error) {
	pci, _, found, readErr = g.resolveSubjectBinding(ctx, member, payload)
	return pci, found, readErr
}

// resolveSubjectBinding is resolveSubjectPCI, also reporting whether the
// system of record holds the member (held) or the identifier was derived.
func (g *Gateway) resolveSubjectBinding(ctx context.Context, member string, payload []byte) (pci string, held, found bool, readErr error) {
	return g.resolveBinding(ctx, member, payload, false)
}

// resolveInvolvedPCI is resolveSubjectPCI for another patient a request names by the id of
// a Patient it carries (involved.go): that Patient is the record of the very person looked
// up, so it is read as theirs though a Coverage names it as its party (ownRecord).
func (g *Gateway) resolveInvolvedPCI(ctx context.Context, member string, payload []byte) (pci string, found bool, readErr error) {
	pci, _, found, readErr = g.resolveBinding(ctx, member, payload, true)
	return pci, found, readErr
}

func (g *Gateway) resolveBinding(ctx context.Context, member string, payload []byte, ownRecord bool) (pci string, held, found bool, readErr error) {
	pci, _, found, readErr = ReadSystemOfRecord(g.cfg.SoR).ResolvePatientContext(ctx, member)
	if readErr != nil || found || g.cfg.RequireKnownMembers {
		return pci, found, found, readErr
	}
	demo, ok := carriedDemographics(payload, member, ownRecord)
	if !ok && carriesMembersParty(payload, member, ownRecord) {
		return "", false, false, errPatientUnidentifiable
	}
	return derivedPCI(member, demo.BirthDate, demo.FamilyName), false, true, nil
}

// errPatientUnidentifiable is a request naming a member the system of record does not
// hold that carries no Patient of the member's own to derive its identifier from, and
// carries another person, a Coverage's party, under the member's own member number: the
// number is shared, so the member id alone identifies no one patient, and the request is
// refused rather than bound to an identifier another person's request also derives.
var errPatientUnidentifiable = errors.New(msgPatientUnidentifiable)

const msgPatientUnidentifiable = "cannot identify the patient: the request carries no Patient for its member with a birth date and name, and its member number is shared with another person it carries (a coverage's subscriber, policyholder or payor)"

// subjectBindingFailure is the answer to a failed bind of the member a request names:
// a patient the request cannot identify is refused 400, recorded as authority (the
// patient the exchange is for cannot be identified); any other failure is the system of
// record's (SoRFailureResponse).
func subjectBindingFailure(ctx context.Context, err error) (int, string) {
	if errors.Is(err, errPatientUnidentifiable) {
		exchangeOf(ctx).refusing(RefusalAuthority)
		return http.StatusBadRequest, msgPatientUnidentifiable
	}
	return SoRFailureResponse(err)
}

// derivedPCIDomain separates a derived identifier's hash input from the one a
// held member's identifier is made from (shnsdk.ResolvePCI), so the two can
// never be equal.
const derivedPCIDomain = "shn-derived-pci-v1\x00"

// derivedPCI is the identifier a gateway binds a member its system of record
// does not hold to, from the member id and the Patient the request carries
// (FR-G61). It is made in its own namespace: a domain-separated hash of the
// three facts exactly as sent (no case folding), so it can never equal a held
// member's identifier, which is shnsdk.ResolvePCI over the lowercased facts —
// a request naming "mbr-covered" does not alias the held MBR-COVERED — while
// two gateways deriving from the same request still agree. It has the same
// form as any network patient identifier ("pci:" and 32 hex digits).
func derivedPCI(memberID, birthDate, familyName string) string {
	sum := sha256.Sum256([]byte(derivedPCIDomain + memberID + "|" + birthDate + "|" + familyName))
	return "pci:" + hex.EncodeToString(sum[:16])
}

// PatientDemographics reads the two demographics the patient identifier is derived from
// off a FHIR Patient: birthDate and the family name of the first name, both required and
// read verbatim, by their exact member names. It is the one read of a Patient's
// demographics, for a holder's own record and for a Patient a request carries alike, so a
// subject derived from either is the same. A Patient naming one of them only in another
// case, or repeating a member name, has none: a reader taking names exactly reads it
// another way.
func PatientDemographics(patientJSON []byte) (Demo, bool) {
	var p struct {
		ResourceType string `json:"resourceType"`
		BirthDate    string `json:"birthDate"`
		Name         []struct {
			Family string `json:"family"`
		} `json:"name"`
	}
	if decodeMessage(patientJSON, &p) != nil || p.ResourceType != "Patient" || p.BirthDate == "" || len(p.Name) == 0 || p.Name[0].Family == "" {
		return Demo{}, false
	}
	return Demo{BirthDate: p.BirthDate, FamilyName: p.Name[0].Family}, true
}

// carriedPatientDemographics reads the demographics of the member's Patient wherever the
// request carries it — a prefetch value or a searchset entry, a bundle entry, a contained
// resource, an operation parameter. Zero and false when the request carries none with both
// demographics, or carries several that disagree.
func carriedPatientDemographics(payload []byte, member string) (Demo, bool) {
	return carriedDemographics(payload, member, false)
}

// carriedDemographics is carriedPatientDemographics, reading a Patient whose id is the
// member's as theirs when ownRecord (resolveInvolvedPCI).
func carriedDemographics(payload []byte, member string, ownRecord bool) (Demo, bool) {
	var demo Demo
	var found, conflict bool
	visitCarriedPatients(payload, member, ownRecord, func(patient map[string]any, party bool) {
		if party {
			return
		}
		raw, err := json.Marshal(patient)
		if err != nil {
			return
		}
		d, ok := PatientDemographics(raw)
		if !ok {
			return
		}
		if found && d != demo {
			conflict = true
		}
		demo, found = d, true
	})
	if !found || conflict {
		return Demo{}, false
	}
	return demo, true
}

// carriesPatient reports whether the request carries a Patient resource for the member
// anywhere, with demographics or not.
func carriesPatient(payload []byte, member string) bool {
	carried := false
	forEachCarriedPatient(payload, member, func(map[string]any) { carried = true })
	return carried
}

// forEachCarriedPatient calls fn for every Patient resource the request carries for the
// member, at any depth, read by its exact member names. A Patient is the member's when its
// id or its member identifier is the member id. A Coverage's party (shnsdk.CoverageParty:
// the parent a dependent's Coverage names as a contained Patient in its subscriber or
// policyHolder) is another person, never the member, whatever it carries: a Patient any
// Coverage in the request names in either slot (partyNamed) is never read as the member,
// wherever the request carries it. Contained resources named only in another case are no
// resources to a reader taking names exactly, so none is read from them, and none is read
// from the contained resources of a Coverage whose party is unclear (partyUnclear). An empty
// or unreadable request carries none, and so does one that repeats a member name exactly, or
// in another case within a FHIR resource (resourceRepeatsAName): it can be read two ways.
func forEachCarriedPatient(payload []byte, member string, fn func(patient map[string]any)) {
	visitCarriedPatients(payload, member, false, func(patient map[string]any, party bool) {
		if !party {
			fn(patient)
		}
	})
}

// carriesMembersParty reports whether the request carries a Patient bearing the member's
// id or member identifier that is a Coverage's party, or that the walk cannot decide is
// not one: another person under the member's own member number.
func carriesMembersParty(payload []byte, member string, ownRecord bool) bool {
	found := false
	visitCarriedPatients(payload, member, ownRecord, func(_ map[string]any, party bool) { found = found || party })
	return found
}

// visitCarriedPatients is forEachCarriedPatient's walk, also calling fn, with party set,
// for each Patient bearing the member's id or member identifier that it does not read as
// the member because a Coverage names it as its party or names no party one way. With
// ownRecord, a Patient whose id is the member's, outside any contained list, is the record
// of the person looked up and is never a party.
func visitCarriedPatients(payload []byte, member string, ownRecord bool, fn func(patient map[string]any, party bool)) {
	var doc any
	if len(payload) == 0 || json.Unmarshal(payload, &doc) != nil || resourceRepeatsAName(payload) {
		return
	}
	slots := partySlotsOf(doc)
	// walk visits node; fullURL is the fullUrl of the Bundle entry node is the
	// resource of, bundle that Bundle (bundleOf), and container the resource whose
	// contained list node is one of.
	var walk func(node any, fullURL string, bundle uintptr, container map[string]any)
	walk = func(node any, fullURL string, bundle uintptr, container map[string]any) {
		switch v := node.(type) {
		case map[string]any:
			if v["resourceType"] == "Patient" && patientIsMember(v, member) {
				fn(v, partyNamed(v, member, fullURL, bundle, containerOf(container), slots, ownRecord))
			}
			if forEachEntry(v, func(entryURL string, resource any) { walk(resource, entryURL, containerOf(v), nil) },
				func(other any) { walk(other, "", 0, nil) }) {
				return
			}
			for k, child := range v {
				if k != "contained" && strings.EqualFold(k, "contained") {
					continue
				}
				// A Coverage's party (shnsdk.CoverageParty, which answers for
				// a Coverage only) is skipped. A contained that is not a list
				// is read as the one resource a lenient reader reads it as.
				if list, ok := containedList(child); ok && k == "contained" {
					unclear := partyUnclear(v)
					for _, c := range list {
						if cr, ok := c.(map[string]any); ok && (unclear || shnsdk.CoverageParty(v, cr)) {
							if cr["resourceType"] == "Patient" && patientIsMember(cr, member) {
								fn(cr, true)
							}
							continue
						}
						walk(c, "", 0, v)
					}
					continue
				}
				walk(child, "", 0, nil)
			}
		case []any:
			for _, child := range v {
				walk(child, "", 0, nil)
			}
		}
	}
	walk(doc, "", 0, nil)
}

// forEachEntry visits, when v is a Bundle, the resource of each of its entries with that
// entry's fullUrl, and every other member of v and of its entries with other; it reports
// whether v is a Bundle. Only a Bundle's entries have a fullUrl a reference resolves by:
// an object elsewhere holding "fullUrl" and "resource" is no entry.
func forEachEntry(v map[string]any, entry func(fullURL string, resource any), other func(any)) bool {
	if v["resourceType"] != "Bundle" {
		return false
	}
	for k, child := range v {
		list, ok := child.([]any)
		if k != "entry" || !ok {
			other(child)
			continue
		}
		for _, raw := range list {
			e, ok := raw.(map[string]any)
			if !ok {
				other(raw)
				continue
			}
			url, _ := e["fullUrl"].(string)
			for ek, ev := range e {
				if ek == "resource" {
					entry(url, ev)
				} else {
					other(ev)
				}
			}
		}
	}
	return true
}

// partySlots are the members by which a Coverage names its party.
var partySlots = []string{"subscriber", "policyHolder", "payor"}

// referencesIn is each "reference" at any depth in node's members named members. Names
// are matched in any case: a member a reader that ignores case would read as a party
// slot or a reference is read so, so no Patient it names is taken for the member.
func referencesIn(node map[string]any, members ...string) []string {
	var refs []string
	var collect func(node any)
	collect = func(node any) {
		switch v := node.(type) {
		case map[string]any:
			for k, child := range v {
				if ref, ok := child.(string); ok && strings.EqualFold(k, "reference") {
					refs = append(refs, ref)
					continue
				}
				collect(child)
			}
		case []any:
			for _, child := range v {
				collect(child)
			}
		}
	}
	for k, child := range node {
		for _, m := range members {
			if strings.EqualFold(k, m) {
				collect(child)
			}
		}
	}
	return refs
}

// beneficiaryReferences is the reference coverage's beneficiary holds, as every reader
// reads it: the "reference" of the beneficiary itself, one Reference, by their exact names,
// never one nested deeper (an extension's, or an identifier's assigner). A beneficiary only
// a lenient reader reads, or a list of them, ties no Patient to the member.
func beneficiaryReferences(coverage map[string]any) []string {
	if r, ok := coverage["beneficiary"].(map[string]any); ok {
		if ref, ok := r["reference"].(string); ok {
			return []string{ref}
		}
	}
	return nil
}

// restfulURL is a RESTful URL of a resource: its base is the first submatch.
var restfulURL = regexp.MustCompile(`^(https?://.+/)[A-Z][A-Za-z]+/[A-Za-z0-9\-.]{1,64}(?:/_history/[A-Za-z0-9\-.]{1,64})?$`)

// restfulBase is the base of a RESTful fullUrl ("http://ehr.example/fhir/" for
// "http://ehr.example/fhir/Coverage/1"), or "" for any other (a urn:uuid, or none).
func restfulBase(fullURL string) string {
	if m := restfulURL.FindStringSubmatch(fullURL); m != nil {
		return m[1]
	}
	return ""
}

// personKey is what tells the resource r apart: for a Patient, the facts a patient
// identifier is derived from (its birth date and family name, as PatientDemographics
// reads them), each as the Patient carries it or not, so Patients that would derive one
// identity are one, versions of one person's record among them, while Patients carrying
// different facts are told apart, whichever they lack; for any other resource, all of it.
// It reads the facts as PatientDemographics reads them, by the same names through the same
// decoder: merging Patients is safe only because the two never disagree.
func personKey(r map[string]any) (string, bool) {
	raw, err := json.Marshal(r)
	if err != nil {
		return "", false
	}
	if r["resourceType"] == "Patient" {
		var p struct {
			BirthDate string `json:"birthDate"`
			Name      []struct {
				Family string `json:"family"`
			} `json:"name"`
		}
		if decodeMessage(raw, &p) == nil {
			family := ""
			if len(p.Name) > 0 {
				family = p.Name[0].Family
			}
			return "Patient\x00" + p.BirthDate + "\x00" + family, true
		}
	}
	return string(raw), true
}

// entryKey is the resource a fullUrl names, as a reader that normalizes URLs reads it:
// without its version, its scheme and host in lower case, and without a default port.
func entryKey(fullURL string) string {
	u := withoutHistory(fullURL)
	scheme, rest, ok := strings.Cut(u, "://")
	if !ok {
		return u
	}
	host, path, _ := strings.Cut(rest, "/")
	scheme, host = strings.ToLower(scheme), strings.ToLower(host)
	if (scheme == "http" && strings.HasSuffix(host, ":80")) || (scheme == "https" && strings.HasSuffix(host, ":443")) {
		host = host[:strings.LastIndex(host, ":")]
	}
	return scheme + "://" + host + "/" + path
}

// withoutHistory is the URL u names without its version.
func withoutHistory(u string) string {
	if i := strings.Index(u, "/_history/"); i >= 0 {
		return u[:i]
	}
	return u
}

// coverageLike reports whether v is a Coverage to a reader that ignores the case of member
// names.
func coverageLike(v map[string]any) bool {
	for k, rt := range v {
		if strings.EqualFold(k, "resourceType") && rt == "Coverage" {
			return true
		}
	}
	return false
}

// partySlotIndex is, for each Coverage a request carries at any depth, what it names in its
// party slots (the references, and the logical references by identifier) and in its
// beneficiary.
type partySlotIndex struct {
	coverages []coverageNaming
	// patients is, for each id a Patient has (or its entry's RESTful fullUrl names), each
	// distinct Patient carried under it, in a contained list or not: a reference by type
	// and id names no one of different people carried under one id, wherever they are.
	patients map[string]map[string]bool
	// repeated is, for each contained list, the ids more than one of its resources has.
	repeated map[uintptr]map[string]bool
	// sharedURL is each fullUrl (entryKey) Bundle entries holding different resources have,
	// in one Bundle or in several; identical copies of one entry are one.
	sharedURL map[string]bool
	// unclear is set when readers may read the request's references differently: a FHIR
	// resource it carries spells a name a reference is resolved by in another case
	// (tieNames), or a contained resource contains others. No beneficiary then ties a
	// Patient to the member.
	unclear bool
}

// tieNames are the member names by which a reader resolves a beneficiary reference to the
// Patient it names.
var tieNames = []string{"resourceType", "id", "fullUrl", "resource", "entry", "parameter", "contained", "beneficiary", "reference"}

// repeats reports whether more than one resource of the contained list scope has id: a
// "#id" then names no one of them.
func (idx partySlotIndex) repeats(scope uintptr, id string) bool {
	return idx.repeated[scope][id]
}

// ambiguous reports whether the request carries different Patients under id: a reference
// by type and id alone then names no one of them.
func (idx partySlotIndex) ambiguous(id string) bool {
	return len(idx.patients[id]) > 1
}

type coverageNaming struct {
	refs, beneficiaries []string
	idents              []slotIdentifier
	// scope is the contained list the Coverage's "#id" references resolve against: its
	// container's, when it is a contained resource, else its own (containerOf).
	scope uintptr
	// base is the RESTful base its relative references resolve against: that of the
	// fullUrl of the Bundle entry it (or its container) is the resource of, or "".
	base string
	// bundle is the Bundle (containerOf) of that entry, or 0: its references resolve to
	// that Bundle's entries alone.
	bundle uintptr
	// undecided is set when a party slot names, other than as the Coverage's beneficiary,
	// what may be a Patient (a reference that names no resource of another type, or an
	// identifier in no system or the member system): a member-bearing Patient that is not
	// this Coverage's beneficiary may be that party, so it is never read as the member.
	undecided bool
}

// containedList is a contained member's resources: its list, or, for a single object, the
// one resource a lenient reader reads it as.
func containedList(child any) ([]any, bool) {
	switch v := child.(type) {
	case []any:
		return v, true
	case map[string]any:
		return []any{v}, true
	}
	return nil, false
}

// containerOf identifies the contained list of the resource m, or 0 for none: a "#id"
// resolves only among the resources m contains.
func containerOf(m map[string]any) uintptr {
	if m == nil {
		return 0
	}
	return reflect.ValueOf(m).Pointer()
}

// slotIdentifier is an identifier a party slot names its party by. A system left out
// names the value in any system.
type slotIdentifier struct{ system, value string }

// partySlotsOf indexes the party slots of every Coverage the request carries.
func partySlotsOf(doc any) partySlotIndex {
	idx := partySlotIndex{patients: map[string]map[string]bool{}, repeated: map[uintptr]map[string]bool{}, sharedURL: map[string]bool{}}
	entries := map[string]map[string]bool{}
	idx.unclear = resourcesSpellInAnotherCase(doc, tieNames...)
	// The type of each resource a reference can name: by fullUrl within its Bundle, and by
	// "#id" within the contained list it is one of.
	byURL := map[uintptr]map[string]string{}
	byLocal := map[uintptr]map[string]string{}
	typeOf := func(r map[string]any) string {
		t, _ := memberAnyCase(r, "resourceType").(string)
		return t
	}
	// walk visits node; container is the resource whose contained list node is one of;
	// entryURL is the fullUrl of the Bundle entry node (or its container) is the resource
	// of, and bundle that Bundle.
	var walk func(node any, container map[string]any, entryURL string, bundle uintptr)
	walk = func(node any, container map[string]any, entryURL string, bundle uintptr) {
		switch v := node.(type) {
		case map[string]any:
			if typeOf(v) == "Patient" {
				id, _ := v["id"].(string)
				// Only an entry's own resource has the id its fullUrl names; a contained one
				// has none of its container's.
				if m := patientRefShape.FindStringSubmatch(entryURL); id == "" && m != nil && container == nil {
					id = m[1]
				}
				if key, ok := personKey(v); ok && id != "" {
					if idx.patients[id] == nil {
						idx.patients[id] = map[string]bool{}
					}
					idx.patients[id][key] = true
				}
			}
			if coverageLike(v) {
				scope := containerOf(v)
				if container != nil {
					scope = containerOf(container)
				}
				idx.coverages = append(idx.coverages, coverageNaming{
					refs:          referencesIn(v, partySlots...),
					beneficiaries: beneficiaryReferences(v),
					idents:        identifiersIn(v, partySlots...),
					scope:         scope,
					base:          restfulBase(entryURL),
					bundle:        bundle,
				})
			}
			if container != nil && memberAnyCase(v, "contained") != nil {
				idx.unclear = true
			}
			if container != nil {
				if id, ok := memberAnyCase(v, "id").(string); ok {
					c := containerOf(container)
					if byLocal[c] == nil {
						byLocal[c] = map[string]string{}
					}
					if _, seen := byLocal[c][id]; seen {
						if idx.repeated[c] == nil {
							idx.repeated[c] = map[string]bool{}
						}
						idx.repeated[c][id] = true
					}
					byLocal[c][id] = typeOf(v)
				}
			}
			if forEachEntry(v, func(url string, resource any) {
				b := containerOf(v)
				if r, ok := resource.(map[string]any); ok && url != "" {
					if byURL[b] == nil {
						byURL[b] = map[string]string{}
					}
					byURL[b][url] = typeOf(r)
					if key, ok := personKey(r); ok {
						u := entryKey(url)
						if entries[u] == nil {
							entries[u] = map[string]bool{}
						}
						entries[u][key] = true
						idx.sharedURL[u] = len(entries[u]) > 1
					}
				}
				walk(resource, nil, url, b)
			}, func(other any) { walk(other, nil, "", 0) }) {
				return
			}
			for k, child := range v {
				if list, ok := containedList(child); ok && k == "contained" {
					for _, c := range list {
						walk(c, v, entryURL, bundle)
					}
					continue
				}
				walk(child, nil, "", 0)
			}
		case []any:
			for _, child := range v {
				walk(child, nil, "", 0)
			}
		}
	}
	walk(doc, nil, "", 0)
	// mayBePatient reports whether ref, as a Coverage with scope resolves it, may name a
	// Patient: it names no resource the request carries as another type.
	mayBePatient := func(ref string, scope, bundle uintptr) bool {
		if m := otherTypedRef.FindStringSubmatch(ref); m != nil && m[1] != "Patient" {
			return false
		}
		if id, ok := strings.CutPrefix(ref, "#"); ok {
			if t, ok := byLocal[scope][id]; ok {
				return t == "Patient"
			}
			return true
		}
		if t, ok := byURL[bundle][ref]; ok {
			return t == "Patient"
		}
		return true
	}
	for i := range idx.coverages {
		c := &idx.coverages[i]
		for _, ref := range c.refs {
			c.undecided = c.undecided || (!slices.Contains(c.beneficiaries, ref) && mayBePatient(ref, c.scope, c.bundle))
		}
		for _, si := range c.idents {
			c.undecided = c.undecided || si.system == "" || si.system == shnsdk.MemberSystem
		}
	}
	return idx
}

// otherTypedRef is a reference naming a resource by its type and id (relative or absolute,
// optionally versioned): its type is the first submatch.
var otherTypedRef = regexp.MustCompile(`(?:^|/)([A-Z][A-Za-z]+)/[A-Za-z0-9\-.]{1,64}(?:/_history/[A-Za-z0-9\-.]{1,64})?$`)

// identifiersIn is each logical reference at any depth in node's members named members: an
// "identifier" (one, or a list of them) holding a value, and its system, if any. Names are
// matched in any case, as referencesIn matches them.
func identifiersIn(node map[string]any, members ...string) []slotIdentifier {
	var ids []slotIdentifier
	read := func(raw any) {
		var list []any
		switch v := raw.(type) {
		case map[string]any:
			list = []any{v}
		case []any:
			list = v
		}
		for _, item := range list {
			id, _ := item.(map[string]any)
			system, _ := memberAnyCase(id, "system").(string)
			value, _ := memberAnyCase(id, "value").(string)
			if value != "" {
				ids = append(ids, slotIdentifier{system: system, value: value})
			}
		}
	}
	var collect func(node any)
	collect = func(node any) {
		switch v := node.(type) {
		case map[string]any:
			for k, child := range v {
				if strings.EqualFold(k, "identifier") {
					read(child)
					continue
				}
				collect(child)
			}
		case []any:
			for _, child := range v {
				collect(child)
			}
		}
	}
	for k, child := range node {
		for _, m := range members {
			if strings.EqualFold(k, m) {
				collect(child)
			}
		}
	}
	return ids
}

// memberAnyCase is m's member named name in any case: the one named exactly, else the one
// whose name sorts first, so a resource spelling a name more than one way reads the same
// on every run.
func memberAnyCase(m map[string]any, name string) any {
	if v, ok := m[name]; ok {
		return v
	}
	var found any
	first := ""
	for k, v := range m {
		if strings.EqualFold(k, name) && (first == "" || k < first) {
			first, found = k, v
		}
	}
	return found
}

// partyNamed reports whether patient is another person than the member, or may be: by a
// Coverage whose beneficiary does not name patient, by a Patient reference to its id, by
// the fullUrl of the Bundle entry it is the resource of, by "#id" within its container, or
// by a logical reference to an identifier it carries in that identifier's system; or some
// party slot names what may be a Patient (an undecided Coverage) of which patient is not
// the beneficiary, so the walk cannot decide patient is the member. A Patient's id is read
// by its exact name (one spelling it in another case is tied to no reference); one with no
// id in an entry with a RESTful fullUrl has the id that fullUrl names. With ownRecord, a
// Patient carried outside any contained list whose id is the member's is the record of the
// person looked up: never another person. A contained resource's id is local to its
// container, never a member id. scope identifies the contained list patient is one of
// (containerOf), 0 for none.
func partyNamed(patient map[string]any, member, fullURL string, bundle, scope uintptr, slots partySlotIndex, ownRecord bool) bool {
	id, _ := patient["id"].(string)
	if m := patientRefShape.FindStringSubmatch(fullURL); id == "" && m != nil {
		id = m[1]
	}
	if ownRecord && scope == 0 && id != "" && id == member {
		return false
	}
	// A Patient in an entry whose fullUrl an entry holding another resource has, in its
	// Bundle or another, is one the walk cannot decide is the member where a Coverage may
	// name a party.
	if fullURL != "" && len(slots.coverages) > 0 && slots.sharedURL[entryKey(fullURL)] {
		return true
	}
	ids, _ := patient["identifier"].([]any)
	for _, c := range slots.coverages {
		// names reports whether ref, as c resolves it, names patient: a "#id" only among
		// the contained resources c resolves it against.
		names := func(ref string) bool {
			if fullURL != "" && ref == fullURL {
				return true
			}
			if scope != 0 && c.scope == scope && id != "" && ref == "#"+id {
				return true
			}
			m := patientRefShape.FindStringSubmatch(ref)
			return m != nil && id != "" && m[1] == id
		}
		// ties reports whether the beneficiary reference ref names patient, resolved as a
		// Bundle resolves it. A relative reference resolves against the base of the Coverage's
		// entry, when that is RESTful, so an absolute one names only the entry whose fullUrl it
		// is, and one whose fullUrl names another id than its own is named by none. A Patient
		// under a urn:uuid is named by that fullUrl, or by its type and id only through a
		// relative reference no RESTful base resolves. A contained Patient is named only by
		// "#id", within its container, and by none when another resource there has its id; any
		// other Patient with no URL of its own (a CRD prefetch Patient, or one in an entry
		// without a fullUrl) by its type and id. A reference by type and id, or by "#id", names
		// no Patient when the request carries different Patients under its id, contained or
		// not. A request readers may read differently (slots.unclear) ties none.
		// A Coverage's references resolve among its own Bundle's entries: an entry of
		// another Bundle is named by none of them, but a RESTful URL names one resource
		// wherever it is carried.
		other := fullURL != "" && c.bundle != bundle
		ties := func(ref string) bool {
			// Every way a beneficiary ties a Patient passes this guard: none does in a request
			// readers may read differently, nor when the request carries different Patients
			// under patient's id.
			if slots.unclear || slots.ambiguous(id) {
				return false
			}
			if scope != 0 {
				return strings.HasPrefix(ref, "#") && !slots.repeats(scope, id) && names(ref)
			}
			if other && (restfulBase(fullURL) == "" || strings.HasPrefix(ref, "#") || strings.HasPrefix(ref, "urn:")) {
				return false
			}
			if strings.HasPrefix(ref, "#") || strings.HasPrefix(ref, "urn:") || fullURL == "" {
				return names(ref)
			}
			if restfulBase(fullURL) == "" {
				return !strings.Contains(ref, "://") && c.base == "" && names(ref)
			}
			if m := patientRefShape.FindStringSubmatch(fullURL); m == nil || (id != "" && m[1] != id) {
				return false
			}
			abs := ref
			if !strings.Contains(ref, "://") {
				if c.base == "" {
					// resolved by its type and id, within the Coverage's Bundle, or anywhere
					// for a Coverage carried in no Bundle
					return (!other || c.bundle == 0) && names(ref)
				}
				abs = c.base + ref
			}
			return withoutHistory(abs) == withoutHistory(fullURL)
		}
		// An entry of another Bundle is named by none of its party slots' references, though
		// a slot that may name a Patient, or names an identifier it carries, still may be it.
		if slices.ContainsFunc(c.beneficiaries, ties) {
			continue // the Coverage's own member
		}
		if c.undecided {
			return true
		}
		if !other && slices.ContainsFunc(c.refs, names) {
			return true
		}
		for _, si := range c.idents {
			for _, raw := range ids {
				pi, _ := raw.(map[string]any)
				system, _ := pi["system"].(string)
				value, _ := pi["value"].(string)
				if value != "" && si.value == value && si.system == system {
					return true
				}
			}
		}
	}
	return false
}

// partyRuleNames are the member names shnsdk.CoverageParty reads, on the Coverage, its
// party slots and the party.
var partyRuleNames = []string{"resourceType", "contained", "subscriber", "policyHolder", "reference", "id", "identifier"}

// partyUnclear reports whether v is a Coverage to a reader that ignores case and spells,
// at any depth, a name the party rule reads in another case: the rule then names no party
// among its contained resources, yet a reader that ignores case would.
func partyUnclear(v map[string]any) bool {
	for k, rt := range v {
		if strings.EqualFold(k, "resourceType") && rt == "Coverage" {
			return spellsInAnotherCase(v, partyRuleNames...)
		}
	}
	return false
}

// resourcesSpellInAnotherCase reports whether a FHIR resource node carries spells, at any
// depth, one of names in another case. Members outside any resource (a CDS Hooks request's
// extension, which holds members of a vendor's own) are no FHIR names and are not read.
func resourcesSpellInAnotherCase(node any, names ...string) bool {
	switch v := node.(type) {
	case map[string]any:
		if memberAnyCase(v, "resourceType") != nil {
			return spellsInAnotherCase(v, names...)
		}
		for _, child := range v {
			if resourcesSpellInAnotherCase(child, names...) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if resourcesSpellInAnotherCase(child, names...) {
				return true
			}
		}
	}
	return false
}

// spellsInAnotherCase reports whether node, at any depth, has a member spelling one of
// names in another case.
func spellsInAnotherCase(node any, names ...string) bool {
	switch v := node.(type) {
	case map[string]any:
		for k, child := range v {
			for _, n := range names {
				if k != n && strings.EqualFold(k, n) {
					return true
				}
			}
			if spellsInAnotherCase(child, names...) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if spellsInAnotherCase(child, names...) {
				return true
			}
		}
	}
	return false
}

// patientIsMember reports whether a decoded Patient names the member by id or by member
// identifier.
func patientIsMember(patient map[string]any, member string) bool {
	if patient["id"] == member {
		return true
	}
	ids, _ := patient["identifier"].([]any)
	for _, id := range ids {
		if m, ok := id.(map[string]any); ok && m["system"] == shnsdk.MemberSystem && m["value"] == member {
			return true
		}
	}
	return false
}

// bindInboundSubject is the payer side's bind of the member its request names,
// returning the payer's own binding of that member: the PCI from its system of
// record when it holds the member, else the one derived from the member id and
// the Patient the request carries (resolveSubjectPCI). The payer handles the
// member as it would directly: it does not re-derive the leg's identity and
// refuse a difference. A request that names no member is refused, and a
// participant that requires known members (Config.RequireKnownMembers) refuses
// a member its system of record does not hold (400).
//
// Every record the payer keeps about the exchange (the pend ledger, the
// correlation it claims, an inquiry's projection) is keyed by the PCI
// returned here, never by the leg token's subject. The requester chooses that
// subject, so keying a record by it would let one patient's authority file
// another patient's decision under the first patient's identity.
func (g *Gateway) bindInboundSubject(ctx context.Context, member string, payload []byte) (pci string, status int, msg string) {
	if member == "" {
		return "", http.StatusBadRequest, "unknown member"
	}
	pci, held, found, readErr := g.resolveSubjectBinding(ctx, member, payload)
	if readErr != nil {
		status, msg := subjectBindingFailure(ctx, readErr)
		return "", status, msg
	}
	if !found || pci == "" {
		// A member not held is refused only by a participant that requires
		// known members (resolveSubjectBinding derives a subject otherwise):
		// a check it opted into, recorded as conformance. A system of record
		// that reports a member without an identifier names no rule.
		if !found && g.cfg.RequireKnownMembers {
			exchangeOf(ctx).refusing(RefusalConformance)
		}
		return "", http.StatusBadRequest, "unknown member"
	}
	involvedCollectorFrom(ctx).bound(pci, held)
	return pci, 0, ""
}

// SubjectBindingDiffersEvent is the observer event a payer leg raises when its
// own binding of the member the request names is not the patient the leg's
// token names. The request is handled as usual and filed under the payer's
// binding; the event tells the operator that the exchange is attributed, on the
// network, to a patient other than the one the payer answered about. It carries
// no identifier.
const SubjectBindingDiffersEvent = "subject.binding-differs"

// noteSubjectBinding raises SubjectBindingDiffersEvent when pci, the payer's
// binding, differs from the leg token's subject. When the Hub reads the
// involved list, that binding is also named on the answer's list (payer-held
// or payer-derived), so the exchange is recorded under it too (involved.go).
func (g *Gateway) noteSubjectBinding(ctx context.Context, leg, corrID, tokenSubject, pci string) {
	if pci != "" && pci != tokenSubject {
		g.observe(ObserverEvent{Kind: SubjectBindingDiffersEvent, Direction: "ingress", LegType: leg, CorrelationID: corrID})
		involvedCollectorFrom(ctx).differs(pci)
	}
}
