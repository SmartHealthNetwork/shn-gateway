package engine

import (
	"regexp"
	"strings"
)

// strictRead is the party generator's oracle: a reader of FHIR references written from the
// FHIR R4 resolution rules (Bundle.entry.fullUrl, contained "#id", relative and absolute
// references), sharing no helper with the gateway's subject derivation. It reads only a
// request every reader resolves alike, and reports ok false for any other:
//   - every FHIR resource (an object with "resourceType") spells the names this reader reads
//     in their exact case;
//   - no contained resource contains others, and every contained is a list;
//   - a Coverage's beneficiary is one Reference, and its party slots are References or
//     logical references by one Identifier (subscriber, policyHolder: one; payor: a list);
//   - a Patient's identifier and name are lists;
//   - ids are unique within each contained list; no two Bundle entries share a fullUrl, nor
//     two Patients an id (contained or not: some readers resolve a reference by type and id
//     to a contained resource too).
//
// On such a request it applies the published rule: where a Coverage's party slot may name a
// Patient (a reference, other than the Coverage's own beneficiary reference itself, that names
// no resource of another type, or a logical reference in no system or the member system), a Patient is
// read as the member only when that Coverage's beneficiary names it; a slot's logical
// reference in another system names the Patients carrying that identifier. The member's own
// Patients are the others bearing the member's id or number. It returns the facts each of
// those Patients derives an identity from ("<birthDate>|<family>"), and refused when none
// derives one and a party bears the number.
func strictRead(doc any, member string) (srReading, bool) {
	r := &strictReader{member: member, scopes: map[int]map[string]*srResource{}}
	if !r.wellFormed(doc, false, false) {
		return srReading{}, false
	}
	r.index(doc, 0, "", "", 0)
	if !r.unique() {
		return srReading{}, false
	}
	return r.outcome(), true
}

// srReading is what the strict reader reads: the facts ("<birthDate>|<family>") of each of
// the member's own Patients carrying both, and whether the member cannot be identified.
type srReading struct {
	own     map[string]bool
	refused bool
}

type srResource struct {
	res       map[string]any
	fullURL   string // the fullUrl of the Bundle entry it is the resource of
	base      string // the fullUrl its relative references resolve against: its entry's, or its container's
	bundle    int    // the Bundle of that entry, 0 for none
	container int    // the resource whose contained list it is one of, 0 for none
	self      int    // the scope of its own contained list
}

type strictReader struct {
	member    string
	patients  []*srResource
	coverages []*srResource
	all       []*srResource
	scopes    map[int]map[string]*srResource // contained resources, by container and id
	next      int
}

// srNames are the member names the reader resolves a request by.
var srNames = []string{"resourceType", "id", "fullUrl", "resource", "entry", "parameter", "contained",
	"beneficiary", "subscriber", "policyHolder", "payor", "reference", "identifier", "system", "value",
	"birthDate", "name", "family"}

// wellFormed checks the shape rules above; inResource is whether node is within a FHIR
// resource, contained whether it is a contained resource.
func (r *strictReader) wellFormed(node any, inResource, contained bool) bool {
	switch v := node.(type) {
	case map[string]any:
		isResource := false
		for k := range v {
			isResource = isResource || strings.EqualFold(k, "resourceType")
		}
		inResource = inResource || isResource
		if inResource {
			for k := range v {
				for _, n := range srNames {
					if k != n && strings.EqualFold(k, n) {
						return false
					}
				}
			}
		}
		if isResource {
			if c, ok := v["contained"]; ok {
				list, isList := c.([]any)
				if !isList || contained {
					return false
				}
				for _, x := range list {
					if !r.wellFormed(x, true, true) {
						return false
					}
				}
			}
			switch v["resourceType"] {
			case "Coverage":
				if !srCoverageSlotsWellFormed(v) {
					return false
				}
			case "Patient":
				for _, k := range []string{"identifier", "name"} {
					if x, ok := v[k]; ok {
						if _, isList := x.([]any); !isList {
							return false
						}
					}
				}
			}
		}
		for k, child := range v {
			if k == "contained" && isResource {
				continue
			}
			if !r.wellFormed(child, inResource, false) {
				return false
			}
		}
	case []any:
		for _, child := range v {
			if !r.wellFormed(child, inResource, false) {
				return false
			}
		}
	}
	return true
}

func srCoverageSlotsWellFormed(c map[string]any) bool {
	slot := func(x any) bool {
		m, ok := x.(map[string]any)
		if !ok {
			return false
		}
		if id, ok := m["identifier"]; ok {
			if _, isObject := id.(map[string]any); !isObject {
				return false
			}
		}
		return true
	}
	for _, k := range []string{"beneficiary", "subscriber", "policyHolder"} {
		if x, ok := c[k]; ok && !slot(x) {
			return false
		}
	}
	if b, ok := c["beneficiary"].(map[string]any); ok {
		if _, ok := b["reference"].(string); !ok {
			return false
		}
	}
	if p, ok := c["payor"]; ok {
		list, isList := p.([]any)
		if !isList {
			return false
		}
		for _, x := range list {
			if !slot(x) {
				return false
			}
		}
	}
	return true
}

// index records every resource the request carries: Bundle entries with their fullUrl and
// Bundle, contained resources with their container, and others with neither.
func (r *strictReader) index(node any, bundle int, fullURL, base string, container int) {
	switch v := node.(type) {
	case map[string]any:
		rt, isResource := v["resourceType"].(string)
		if isResource {
			r.next++
			res := &srResource{res: v, fullURL: fullURL, base: base, bundle: bundle, container: container, self: r.next}
			r.all = append(r.all, res)
			switch rt {
			case "Patient":
				r.patients = append(r.patients, res)
			case "Coverage":
				r.coverages = append(r.coverages, res)
			}
			if list, ok := v["contained"].([]any); ok {
				r.scopes[res.self] = map[string]*srResource{}
				for _, x := range list {
					if m, ok := x.(map[string]any); ok {
						r.index(m, bundle, "", base, res.self)
						if id, ok := m["id"].(string); ok {
							r.scopes[res.self][id] = r.all[len(r.all)-1]
						}
					}
				}
			}
			if rt == "Bundle" {
				r.next++
				b := r.next
				for _, e := range asList(v["entry"]) {
					em, ok := e.(map[string]any)
					if !ok {
						continue
					}
					url, _ := em["fullUrl"].(string)
					for k, x := range em {
						if k == "resource" {
							r.index(x, b, url, url, 0)
						} else {
							r.index(x, 0, "", "", 0)
						}
					}
				}
			}
		}
		for k, child := range v {
			if isResource && (k == "contained" || (rt == "Bundle" && k == "entry")) {
				continue
			}
			r.index(child, 0, "", "", 0)
		}
	case []any:
		for _, child := range v {
			r.index(child, 0, "", "", 0)
		}
	}
}

func asList(x any) []any {
	l, _ := x.([]any)
	return l
}

// unique reports whether contained ids, entry fullUrls and the ids of Patients outside
// contained lists are each carried once.
func (r *strictReader) unique() bool {
	urls := map[string]bool{}
	ids := map[string]bool{}
	for _, res := range r.all {
		if res.fullURL != "" {
			u := srWithoutVersion(res.fullURL)
			if urls[u] {
				return false
			}
			urls[u] = true
		}
		// Some readers resolve a reference by type and id to a contained resource as well:
		// Patients sharing an id, contained or not, are read two ways.
		if res.res["resourceType"] == "Patient" {
			if id := srID(res); id != "" {
				if ids[id] {
					return false
				}
				ids[id] = true
			}
		}
		seen := map[string]bool{}
		for _, x := range asList(res.res["contained"]) {
			if m, ok := x.(map[string]any); ok {
				if id, ok := m["id"].(string); ok {
					if seen[id] {
						return false
					}
					seen[id] = true
				}
			}
		}
	}
	return true
}

// srRestful is a RESTful URL of a resource: its base, type and id.
var srRestful = regexp.MustCompile(`^(https?://.+/)([A-Z][A-Za-z]+)/([A-Za-z0-9\-.]{1,64})(?:/_history/[A-Za-z0-9\-.]{1,64})?$`)

// srRelative is a relative reference: its type and id.
var srRelative = regexp.MustCompile(`^([A-Z][A-Za-z]+)/([A-Za-z0-9\-.]{1,64})(?:/_history/[A-Za-z0-9\-.]{1,64})?$`)

func srWithoutVersion(u string) string {
	if i := strings.Index(u, "/_history/"); i >= 0 {
		return u[:i]
	}
	return u
}

// srID is a resource's id, or, for an entry with none, the id its RESTful fullUrl names.
func srID(res *srResource) string {
	if id, ok := res.res["id"].(string); ok {
		return id
	}
	if m := srRestful.FindStringSubmatch(res.fullURL); m != nil {
		return m[3]
	}
	return ""
}

// resolve is the resource ref names, read from the Coverage from (FHIR R4, Bundle:
// resolving references): a "#id" among the resources contained with it; an absolute
// reference or urn the entry whose fullUrl it is, in from's Bundle, or, for a RESTful URL,
// a copy of that server resource in another, else the resource of its type and id that has
// no URL of its own (a server resource the request carries); a relative one made absolute against from's
// RESTful base (else a resource of its type and id with no URL of its own), or, from no
// RESTful base, the resource of its type and id among from's Bundle's entries, else
// one carried outside any Bundle (as a prefetched server resource), else, for a Coverage
// in no Bundle, any carried outside a contained list. ok is false when it names no
// resource the request carries.
func (r *strictReader) resolve(ref string, from *srResource) (*srResource, bool) {
	if id, ok := strings.CutPrefix(ref, "#"); ok {
		scope := from.container
		if scope == 0 {
			scope = from.self
		}
		res, ok := r.scopes[scope][id]
		return res, ok
	}
	byURL := func(u string) (*srResource, bool) {
		u = srWithoutVersion(u)
		var other *srResource
		for _, res := range r.all {
			if res.fullURL == "" || srWithoutVersion(res.fullURL) != u {
				continue
			}
			if res.bundle == from.bundle {
				return res, true
			}
			if !strings.HasPrefix(u, "urn:") {
				other = res
			}
		}
		return other, other != nil
	}
	if strings.HasPrefix(ref, "urn:") || strings.Contains(ref, "://") {
		if res, ok := byURL(ref); ok || strings.HasPrefix(ref, "urn:") {
			return res, ok
		}
		// A Patient with no URL of its own (carried outside any Bundle, or in an entry with no
		// fullUrl) is a server resource the request carries, named by the type and id of an
		// absolute reference.
		m := srRestful.FindStringSubmatch(ref)
		if m == nil {
			return nil, false
		}
		for _, res := range r.all {
			if res.fullURL == "" && res.container == 0 && res.res["resourceType"] == m[2] && srID(res) == m[3] {
				return res, true
			}
		}
		return nil, false
	}
	m := srRelative.FindStringSubmatch(ref)
	if m == nil {
		return nil, false
	}
	match := func(res *srResource) bool {
		return res.container == 0 && res.res["resourceType"] == m[1] && srID(res) == m[2]
	}
	if b := srRestful.FindStringSubmatch(from.base); b != nil {
		// FHIR R4: made absolute against a RESTful base, the reference names the entry
		// whose fullUrl it is; else it names a server resource, which the request carries
		// only as a resource with no URL of its own.
		if res, ok := byURL(b[1] + ref); ok {
			return res, true
		}
		for _, res := range r.all {
			if res.fullURL == "" && match(res) {
				return res, true
			}
		}
		return nil, false
	}
	// A relative reference from a resource with no RESTful base has no meaning FHIR defines;
	// the documented choice reads it by type and id among the Bundle's entries.
	if from.bundle != 0 {
		for _, res := range r.all {
			if res.bundle == from.bundle && match(res) {
				return res, true
			}
		}
	}
	for _, res := range r.all {
		if res.bundle == 0 && match(res) {
			return res, true
		}
	}
	if from.bundle == 0 {
		for _, res := range r.all {
			if match(res) {
				return res, true
			}
		}
	}
	return nil, false
}

// outcome applies the published rule to the resolved request.
func (r *strictReader) outcome() srReading {
	bearsMember := func(p *srResource) bool {
		if srID(p) == r.member {
			return true
		}
		return srCarries(p, "urn:shn:member", r.member)
	}
	party := map[*srResource]bool{}
	for _, c := range r.coverages {
		var beneficiary *srResource
		beneficiaryRef := ""
		if b, ok := c.res["beneficiary"].(map[string]any); ok {
			beneficiaryRef = b["reference"].(string)
			beneficiary, _ = r.resolve(beneficiaryRef, c)
		}
		var slots []map[string]any
		for _, k := range []string{"subscriber", "policyHolder"} {
			if s, ok := c.res[k].(map[string]any); ok {
				slots = append(slots, s)
			}
		}
		for _, x := range asList(c.res["payor"]) {
			slots = append(slots, x.(map[string]any))
		}
		undecided := false
		for _, s := range slots {
			if ref, ok := s["reference"].(string); ok {
				// A slot naming the beneficiary in the beneficiary reference's own words names
				// the Coverage's member; one naming it in other words may still name another.
				res, found := r.resolve(ref, c)
				switch {
				case ref == beneficiaryRef:
				case found && res.res["resourceType"] == "Patient":
					undecided = true
				case found:
				default:
					// A reference to no carried resource may name a Patient, unless it names
					// a resource of another type.
					rel, rest := srRelative.FindStringSubmatch(ref), srRestful.FindStringSubmatch(ref)
					if (rel == nil || rel[1] == "Patient") && (rest == nil || rest[2] == "Patient") {
						undecided = true
					}
				}
			}
			if id, ok := s["identifier"].(map[string]any); ok {
				system, _ := id["system"].(string)
				value, _ := id["value"].(string)
				if system == "" || system == "urn:shn:member" {
					undecided = true
				} else {
					for _, p := range r.patients {
						if p != beneficiary && srCarries(p, system, value) {
							party[p] = true
						}
					}
				}
			}
		}
		if undecided {
			for _, p := range r.patients {
				if p != beneficiary {
					party[p] = true
				}
			}
		}
	}
	facts := map[string]bool{}
	partyBears := false
	for _, p := range r.patients {
		if !bearsMember(p) {
			continue
		}
		if party[p] {
			partyBears = true
			continue
		}
		bd, _ := p.res["birthDate"].(string)
		family := ""
		if names := asList(p.res["name"]); len(names) > 0 {
			if n, ok := names[0].(map[string]any); ok {
				family, _ = n["family"].(string)
			}
		}
		if bd != "" && family != "" {
			facts[bd+"|"+family] = true
		}
	}
	return srReading{own: facts, refused: len(facts) == 0 && partyBears}
}

// srCarries reports whether a Patient carries the identifier system|value.
func srCarries(p *srResource, system, value string) bool {
	for _, x := range asList(p.res["identifier"]) {
		if m, ok := x.(map[string]any); ok && m["system"] == system && m["value"] == value {
			return true
		}
	}
	return false
}
