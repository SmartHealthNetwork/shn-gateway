package engine

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The party generator's moves: each changes a request in place the way a reader may
// resolve differently, and names what it did ("" when it found nothing to change).

func genCopy(doc any) any {
	b, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	return out
}

// genSite is a place in a request: the object or list holding a value, and its key or index.
type genSite struct {
	obj   map[string]any
	list  []any
	key   string
	index int
	owner map[string]any // for a list, the object whose member key holds it
}

func (s genSite) get() any {
	if s.obj != nil {
		return s.obj[s.key]
	}
	return s.list[s.index]
}

func (s genSite) set(v any) {
	if s.obj != nil {
		s.obj[s.key] = v
		return
	}
	s.list[s.index] = v
}

// genResources are the FHIR resources doc carries, of type rt ("" for any), with where
// each is held.
func genResources(doc any, rt string) []genSite {
	var out []genSite
	var walk func(node any, site genSite)
	walk = func(node any, site genSite) {
		switch v := node.(type) {
		case map[string]any:
			if t, ok := v["resourceType"].(string); ok && (rt == "" || t == rt) && (site.obj != nil || site.list != nil) {
				out = append(out, site)
			}
			keys := genKeys(v)
			for _, k := range keys {
				walk(v[k], genSite{obj: v, key: k})
			}
		case []any:
			for i, child := range v {
				walk(child, genSite{list: v, index: i, owner: site.obj, key: site.key})
			}
		}
	}
	walk(doc, genSite{})
	return out
}

func genKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func genPick[T any](rng *rand.Rand, xs []T) (T, bool) {
	var zero T
	if len(xs) == 0 {
		return zero, false
	}
	return xs[rng.Intn(len(xs))], true
}

// genIsParent reports whether a Patient is the parent the bases carry: its family name or
// birth date is the parent's, under any spelling of the names the moves rename.
func genIsParent(p map[string]any) bool {
	for k, v := range p {
		if strings.EqualFold(k, "birthDate") && v == "1970-01-01" {
			return true
		}
		if !strings.EqualFold(k, "name") {
			continue
		}
		names, ok := v.([]any)
		if !ok {
			names = []any{v}
		}
		for _, n := range names {
			if m, ok := n.(map[string]any); ok {
				for nk, nv := range m {
					if strings.EqualFold(nk, "family") && nv == "Parentname" {
						return true
					}
				}
			}
		}
	}
	return false
}

func genPatients(doc any, parent bool) []genSite {
	var out []genSite
	for _, s := range genResources(doc, "Patient") {
		if p, ok := s.get().(map[string]any); ok && genIsParent(p) == parent {
			out = append(out, s)
		}
	}
	return out
}

// genRefForms are the forms a reference to the resource with id (and fullUrl, if an entry)
// can take.
func genRefForms(rng *rand.Rand, id, fullURL string) string {
	forms := []string{
		"Patient/" + id,
		"http://ehr.example/fhir/Patient/" + id,
		"https://ehr.example/fhir/Patient/" + id,
		"http://EHR.example:80/fhir/Patient/" + id,
		"Patient/" + id + "/_history/2",
		"#" + id,
		"urn:uuid:" + id,
		"http://other.example/fhir/Patient/" + id,
	}
	if fullURL != "" {
		forms = append(forms, fullURL, fullURL, strings.ToUpper(fullURL))
	}
	f, _ := genPick(rng, forms)
	return f
}

var genMoves = []func(*rand.Rand, any) string{
	genRenameCase, genWrap, genDuplicate, genRelocate, genRewriteRef, genSlot, genCollideID, genVendor,
}

func genMove(rng *rand.Rand, doc any) string {
	m, _ := genPick(rng, genMoves)
	return m(rng, doc)
}

// genRenameCase spells a name a reader resolves by in another case.
func genRenameCase(rng *rand.Rand, doc any) string {
	names := []string{"id", "fullUrl", "resource", "contained", "beneficiary", "subscriber", "policyHolder", "payor", "reference", "identifier", "resourceType", "entry", "birthDate", "name", "family"}
	var sites []genSite
	var walk func(node any)
	walk = func(node any) {
		switch v := node.(type) {
		case map[string]any:
			for _, k := range genKeys(v) {
				for _, n := range names {
					if k == n {
						sites = append(sites, genSite{obj: v, key: k})
					}
				}
				walk(v[k])
			}
		case []any:
			for _, c := range v {
				walk(c)
			}
		}
	}
	walk(doc)
	s, ok := genPick(rng, sites)
	if !ok {
		return ""
	}
	to, _ := genPick(rng, []string{strings.ToUpper(s.key[:1]) + s.key[1:], strings.ToUpper(s.key)})
	s.obj[to] = s.obj[s.key]
	if rng.Intn(3) > 0 {
		delete(s.obj, s.key)
	}
	return "rename " + s.key + " to " + to
}

// genWrap wraps a single Reference or contained resource in a list, or a one-element list
// in an object.
func genWrap(rng *rand.Rand, doc any) string {
	var sites []genSite
	for _, c := range genResources(doc, "Coverage") {
		cov := c.get().(map[string]any)
		for _, k := range []string{"beneficiary", "subscriber", "policyHolder", "contained", "payor"} {
			if _, ok := cov[k]; ok {
				sites = append(sites, genSite{obj: cov, key: k})
			}
		}
	}
	for _, r := range genResources(doc, "") {
		if res, ok := r.get().(map[string]any); ok {
			if _, ok := res["contained"]; ok {
				sites = append(sites, genSite{obj: res, key: "contained"})
			}
		}
	}
	s, ok := genPick(rng, sites)
	if !ok {
		return ""
	}
	switch v := s.get().(type) {
	case []any:
		if len(v) == 0 {
			return ""
		}
		s.set(v[0])
		return "unwrap " + s.key
	case map[string]any:
		s.set([]any{v, genCopy(v)})
		return "wrap " + s.key + " as a list of two"
	}
	return ""
}

// genDuplicate carries a Patient again beside itself: an identical copy, another version,
// or another person under its id.
func genDuplicate(rng *rand.Rand, doc any) string {
	s, ok := genPick(rng, genResources(doc, "Patient"))
	if !ok {
		return ""
	}
	p := s.get().(map[string]any)
	dup := genCopy(p).(map[string]any)
	how := "an identical copy"
	switch rng.Intn(3) {
	case 1:
		dup["meta"] = map[string]any{"versionId": "9"}
		dup["gender"] = "unknown"
		how = "another version"
	case 2:
		if genIsParent(p) {
			dup["name"] = []any{map[string]any{"family": "Childname"}}
			dup["birthDate"] = "2015-06-01"
			delete(dup, "identifier")
		} else {
			dup["name"] = []any{map[string]any{"family": "Parentname"}}
			dup["birthDate"] = "1970-01-01"
			dup["identifier"] = []any{map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}}
		}
		how = "another person"
	}
	if s.list != nil {
		// Appended to the list holding it: a contained list or a Bundle's entries.
		if s.owner != nil && s.key == "contained" {
			s.owner["contained"] = append(s.owner["contained"].([]any), dup)
			return "duplicate a contained Patient as " + how
		}
		return ""
	}
	if s.key == "resource" {
		for _, b := range genResources(doc, "Bundle") {
			bundle := b.get().(map[string]any)
			for _, e := range asList(bundle["entry"]) {
				if em, ok := e.(map[string]any); ok && em["resource"] != nil && fmt.Sprint(em["resource"]) == fmt.Sprint(p) {
					bundle["entry"] = append(bundle["entry"].([]any), map[string]any{"fullUrl": em["fullUrl"], "resource": dup})
					return "duplicate an entry's Patient under its fullUrl as " + how
				}
			}
		}
	}
	return ""
}

// genRelocate moves the parent into another place: a Coverage's contained list, a new
// entry of a Bundle, a Bundle of its own, or an object outside any resource holding
// "fullUrl" and "resource".
func genRelocate(rng *rand.Rand, doc any) string {
	s, ok := genPick(rng, genPatients(doc, true))
	if !ok {
		return ""
	}
	p := genCopy(s.get()).(map[string]any)
	id, _ := p["id"].(string)
	top, isMap := doc.(map[string]any)
	switch rng.Intn(4) {
	case 0:
		c, ok := genPick(rng, genResources(doc, "Coverage"))
		if !ok {
			return ""
		}
		cov := c.get().(map[string]any)
		list, _ := cov["contained"].([]any)
		cov["contained"] = append(list, p)
		return "carry the parent again in a Coverage's contained"
	case 1:
		b, ok := genPick(rng, genResources(doc, "Bundle"))
		if !ok {
			return ""
		}
		bundle := b.get().(map[string]any)
		bundle["entry"] = append(asList(bundle["entry"]), map[string]any{"fullUrl": genRefForms(rng, id, ""), "resource": p})
		return "carry the parent again as an entry"
	case 2:
		if !isMap {
			return ""
		}
		top["zzOther"] = map[string]any{"resourceType": "Bundle", "type": "collection", "entry": []any{
			map[string]any{"fullUrl": genRefForms(rng, id, ""), "resource": p}}}
		return "carry the parent again in a Bundle of its own"
	default:
		if !isMap {
			return ""
		}
		top["zzVendor"] = map[string]any{"fullUrl": genRefForms(rng, id, ""), "resource": p}
		return "carry the parent again in an object outside any resource"
	}
}

// genRewriteRef rewrites a Coverage's beneficiary or party reference into another form.
func genRewriteRef(rng *rand.Rand, doc any) string {
	var sites []map[string]any
	for _, c := range genResources(doc, "Coverage") {
		cov := c.get().(map[string]any)
		for _, k := range []string{"beneficiary", "subscriber", "policyHolder"} {
			if r, ok := cov[k].(map[string]any); ok {
				if _, ok := r["reference"].(string); ok {
					sites = append(sites, r)
				}
			}
		}
	}
	r, ok := genPick(rng, sites)
	if !ok {
		return ""
	}
	old := r["reference"].(string)
	id := old[strings.LastIndexAny(old, "/#:")+1:]
	if i := strings.Index(old, "/_history/"); i >= 0 {
		id = old[:i][strings.LastIndex(old[:i], "/")+1:]
	}
	r["reference"] = genRefForms(rng, id, "")
	return "rewrite " + old + " as " + r["reference"].(string)
}

// genSlot names the parent, or the member's own Patient, in a party slot.
func genSlot(rng *rand.Rand, doc any) string {
	c, ok := genPick(rng, genResources(doc, "Coverage"))
	if !ok {
		return ""
	}
	cov := c.get().(map[string]any)
	targets := genPatients(doc, true)
	if rng.Intn(4) == 0 {
		targets = genPatients(doc, false)
	}
	t, ok := genPick(rng, targets)
	if !ok {
		return ""
	}
	p := t.get().(map[string]any)
	id, _ := p["id"].(string)
	var slot any
	switch rng.Intn(4) {
	case 0:
		slot = map[string]any{"identifier": map[string]any{"system": shnsdk.MemberSystem, "value": strangerMember}}
	case 1:
		slot = map[string]any{"identifier": map[string]any{"value": strangerMember}}
	default:
		slot = map[string]any{"reference": genRefForms(rng, id, "")}
	}
	k, _ := genPick(rng, []string{"subscriber", "policyHolder", "payor"})
	if k == "payor" {
		cov[k] = append(asList(cov[k]), slot)
	} else {
		cov[k] = slot
	}
	return "name a Patient in " + k
}

// genCollideID gives the parent the id of the member's own Patient, or of the beneficiary,
// beside the member's own Patient (one bearing the member's id or number). A request
// carrying no such Patient that gives the parent the id its beneficiary names asserts the
// parent is the member: no reader can tell otherwise.
func genCollideID(rng *rand.Rand, doc any) string {
	own := false
	for _, c := range genPatients(doc, false) {
		p := c.get().(map[string]any)
		own = own || p["id"] == strangerMember
		for _, x := range asList(p["identifier"]) {
			if m, ok := x.(map[string]any); ok && m["system"] == shnsdk.MemberSystem && m["value"] == strangerMember {
				own = true
			}
		}
	}
	if !own {
		return ""
	}
	s, ok := genPick(rng, genPatients(doc, true))
	if !ok {
		return ""
	}
	p := s.get().(map[string]any)
	ids := []string{strangerMember, "kid", "pat"}
	for _, c := range genPatients(doc, false) {
		if id, ok := c.get().(map[string]any)["id"].(string); ok {
			ids = append(ids, id)
		}
	}
	id, _ := genPick(rng, ids)
	p["id"] = id
	return "give the parent the id " + id
}

// genVendor puts members of a vendor's own, named like FHIR names in any case, in a CDS
// Hooks request's extension.
func genVendor(rng *rand.Rand, doc any) string {
	top, ok := doc.(map[string]any)
	if !ok || top["hook"] == nil {
		return ""
	}
	ext, _ := top["extension"].(map[string]any)
	if ext == nil {
		ext = map[string]any{}
		top["extension"] = ext
	}
	k, _ := genPick(rng, []string{"Id", "ID", "Reference", "Entry", "Resource", "FullUrl", "Contained"})
	ext["com.example.vendor"] = map[string]any{k: "x"}
	return "vendor member " + k
}
