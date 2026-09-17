package engine

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// CompartmentError refuses a value holding a resource that is not about the
// bound patient, or whose patient cannot be determined. It never names a
// patient or an identifier.
type CompartmentError struct {
	ResourceType string
	Reason       string
}

func (e *CompartmentError) Error() string {
	if e.ResourceType == "" {
		return "resource refused: " + e.Reason
	}
	return e.ResourceType + " refused: " + e.Reason
}

// patientFence decides whether a CDS Hooks prefetch value (a resource, a
// Bundle, or null) is about one bound patient, using the FHIR R4 Patient
// compartment (patientBindingPaths, generated from the R4 core package).
//
//   - Every Patient resource carried — top level, Bundle entry or contained —
//     must be the bound patient: it carries the bound member identifier, or
//     (outside `contained`) has a bound id, and carries no other member
//     identifier. No other person's record is carried.
//   - A resource of a Patient-compartment type is bound by the references on
//     its binding paths (its `patient` search parameter, such as
//     Coverage.beneficiary or Observation.subject). At least one of them must
//     reference a Patient, and every Patient reference there must resolve to
//     the bound patient; references there to other resource types are
//     ignored, and one that cannot be resolved is refused.
//   - Other references in the resource (Coverage.subscriber, a Claim payee,
//     an Observation performer, and so on) are not binding: they may name
//     another person, and they are not resolved.
//   - A resource of any other R4 type is allowed; an unknown type is refused.
//
// A binding reference resolves as:
//   - `Patient/<id>` (optionally versioned) — to a Patient entry of the same
//     Bundle with that id, which is fenced like any other; otherwise by id;
//   - an absolute URL — only on one of the fence's base URLs, then by id;
//   - `#<id>` — to the containing resource's contained resource (`#` alone is
//     the container itself); a contained Patient must carry the member
//     identifier, since its id is local;
//   - a Bundle entry's fullUrl (such as `urn:uuid:`) — to that entry, whose
//     Patient is fenced like any other;
//   - an identifier with no reference — typed Patient, or in the member
//     identifier system, it must be the bound member identifier.
//
// Anything else that could name a Patient cannot be resolved and is refused.
// Nested Bundles and contained resources are fenced too.
type patientFence struct {
	memberSystem string
	member       string
	bases        []string
	ids          map[string]bool
	// refuseOpaque refuses every Binary resource (forPrefetch).
	refuseOpaque bool
	run          *fenceRun
}

// opaqueContentReason is the refusal of a Binary in a prefetch value.
const opaqueContentReason = "opaque content is not carried in prefetch"

// forPrefetch is the fence for CDS Hooks prefetch values: it also refuses
// every Binary resource (top level, Bundle entry or contained), whose content
// the fence cannot read for the patient.
func (f patientFence) forPrefetch() patientFence {
	f.refuseOpaque = true
	return f
}

// fenceRun is one check's memory: each resource is fenced at most once per
// document, a resource reached again while it is being fenced is a cycle,
// and the number of Provenance target resolutions is bounded.
type fenceRun struct {
	state       []fenceState
	resolutions int
}

// fenceMarkKey is the member under which a check marks each resource it
// reaches with its index in fenceRun.state. The key holds a byte that is not
// UTF-8, so no decoded document can carry it (relay.Decode refuses such
// text).
const fenceMarkKey = "\xfffence"

// mark returns res's index in the run, assigning one on first sight.
func (r *fenceRun) mark(res map[string]any) int {
	if i, ok := res[fenceMarkKey].(int); ok {
		return i
	}
	i := len(r.state)
	r.state = append(r.state, 0)
	res[fenceMarkKey] = i
	return i
}

type fenceState uint8

const (
	fenceInProgress fenceState = iota + 1
	fenceDone
)

// maxFenceResolutions bounds the Provenance target resolutions one value may
// need; a value that needs more is refused.
const maxFenceResolutions = 4096

// newPatientFence binds the patient known on the system of record by ids (its
// server id, and any other id it answers to) and to the network by the member
// identifier memberSystem|member. bases are the FHIR base URLs whose absolute
// references can be resolved.
func newPatientFence(memberSystem, member string, bases []string, ids ...string) patientFence {
	f := patientFence{memberSystem: memberSystem, member: member, ids: map[string]bool{}}
	for _, b := range bases {
		if b = strings.TrimRight(b, "/"); b != "" {
			f.bases = append(f.bases, b)
		}
	}
	for _, id := range ids {
		if id != "" {
			f.ids[id] = true
		}
	}
	return f
}

// check fences one prefetch value. Its JSON is read strictly (see
// relay.Decode): a value that repeats a member name is refused.
func (f patientFence) check(value []byte) error {
	_, err := f.checkCounting(value)
	return err
}

// checkCounting is check, also reporting the Provenance target resolutions
// it made.
func (f patientFence) checkCounting(value []byte) (int, error) {
	f.run = &fenceRun{}
	err := f.value(value)
	return f.run.resolutions, err
}

func (f patientFence) value(value []byte) error {
	var v any
	if err := relay.Decode(relay.NewBody(value, relay.OriginIngressRequest), &v); err != nil {
		return &CompartmentError{Reason: "not a readable JSON value"}
	}
	if v == nil {
		return nil
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return &CompartmentError{Reason: "not a resource"}
	}
	return f.resource(obj, nil, nil, 0)
}

// maxFenceDepth bounds how deeply nested Bundles and contained resources are
// followed; deeper nesting is refused.
const maxFenceDepth = 16

// docIndex is what a Bundle's entries resolve by: fullUrl, and Type/id
// (refused when two entries share it).
type docIndex struct {
	fullURL   map[string]map[string]any
	typeID    map[string]map[string]any
	ambiguous map[string]bool
}

func (d *docIndex) byFullURL(s string) map[string]any {
	if d == nil {
		return nil
	}
	return d.fullURL[s]
}

// scope is what `#` and fullUrl references resolve against.
type fenceScope struct {
	container map[string]any
	contained map[string]map[string]any
	entries   *docIndex
}

// resource fences res. parent is the scope of the resource that contains
// res, or nil: a contained resource's `#` references resolve against its
// container, as FHIR defines.
func (f patientFence) resource(res map[string]any, entries *docIndex, parent *fenceScope, depth int) error {
	key := f.run.mark(res)
	switch f.run.state[key] {
	case fenceDone:
		return nil
	case fenceInProgress:
		rt, _ := res["resourceType"].(string)
		return &CompartmentError{ResourceType: rt, Reason: "references form a cycle"}
	}
	f.run.state[key] = fenceInProgress
	err := f.resourceOnce(res, entries, parent, depth)
	if err == nil {
		f.run.state[key] = fenceDone
	}
	return err
}

func (f patientFence) resourceOnce(res map[string]any, entries *docIndex, parent *fenceScope, depth int) error {
	rt, _ := res["resourceType"].(string)
	if rt == "" {
		return &CompartmentError{Reason: "no resource type"}
	}
	if depth > maxFenceDepth {
		return &CompartmentError{ResourceType: rt, Reason: "references nest too deeply"}
	}
	if rt == "Bundle" {
		return f.bundle(res, depth)
	}
	if rt == "Binary" && f.refuseOpaque {
		return &CompartmentError{ResourceType: rt, Reason: opaqueContentReason}
	}
	paths, known := patientBindingPaths[rt]
	if !known {
		return &CompartmentError{ResourceType: rt, Reason: "not an R4 resource type"}
	}
	sc := fenceScope{container: res, entries: entries, contained: map[string]map[string]any{}}
	if parent != nil {
		sc = *parent
		if _, nested := res["contained"]; nested {
			return &CompartmentError{ResourceType: rt, Reason: "contained resource contains resources"}
		}
	}
	if raw, ok := res["contained"]; ok {
		list, ok := raw.([]any)
		if !ok {
			return &CompartmentError{ResourceType: rt, Reason: "contained is not a list"}
		}
		var order []map[string]any
		for _, c := range list {
			cr, ok := c.(map[string]any)
			if !ok {
				return &CompartmentError{ResourceType: rt, Reason: "contained resource is not an object"}
			}
			id, _ := cr["id"].(string)
			if id == "" || sc.contained[id] != nil {
				return &CompartmentError{ResourceType: rt, Reason: "contained resource without a unique id"}
			}
			sc.contained[id] = cr
			order = append(order, cr)
		}
		for _, cr := range order {
			if err := f.resource(cr, entries, &sc, depth+1); err != nil {
				return err
			}
		}
	}
	if rt == "Patient" {
		return f.patient(res, parent != nil)
	}
	if rt == "Provenance" {
		return f.provenance(res, sc, depth)
	}
	if len(paths) == 0 {
		return nil
	}
	patientRefs := 0
	for _, path := range paths {
		var refs []any
		collect(res, strings.Split(path, "."), &refs)
		for _, ref := range refs {
			isPatient, err := f.reference(ref, sc)
			if err != nil {
				return &CompartmentError{ResourceType: rt, Reason: err.Reason}
			}
			if isPatient {
				patientRefs++
			}
		}
	}
	if patientRefs == 0 {
		return &CompartmentError{ResourceType: rt, Reason: "no reference to the patient"}
	}
	return nil
}

func (f patientFence) bundle(b map[string]any, depth int) error {
	raw, ok := b["entry"]
	if !ok {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		return &CompartmentError{ResourceType: "Bundle", Reason: "entry is not a list"}
	}
	entries := &docIndex{fullURL: map[string]map[string]any{}, typeID: map[string]map[string]any{}, ambiguous: map[string]bool{}}
	var resources []map[string]any
	for _, e := range list {
		eo, ok := e.(map[string]any)
		if !ok {
			return &CompartmentError{ResourceType: "Bundle", Reason: "entry is not an object"}
		}
		res, ok := eo["resource"].(map[string]any)
		if !ok {
			return &CompartmentError{ResourceType: "Bundle", Reason: "entry has no resource"}
		}
		if full, _ := eo["fullUrl"].(string); full != "" {
			if entries.fullURL[full] != nil {
				return &CompartmentError{ResourceType: "Bundle", Reason: "repeated fullUrl"}
			}
			entries.fullURL[full] = res
		}
		rt, _ := res["resourceType"].(string)
		if id, _ := res["id"].(string); rt != "" && id != "" {
			key := rt + "/" + id
			if entries.typeID[key] != nil {
				entries.ambiguous[key] = true
			}
			entries.typeID[key] = res
		}
		resources = append(resources, res)
	}
	for _, res := range resources {
		if err := f.resource(res, entries, nil, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// provenance binds a Provenance through its targets: every target must resolve within the same document — a
// contained resource, or a Bundle entry by fullUrl or Type/id (relative, or
// absolute on a fence base; a version suffix is ignored) — to a resource
// that itself passes the fence, or name the bound patient. Any other target
// is refused. Each resource is fenced once per value (a cycle is refused),
// and the total number of target resolutions is bounded.
func (f patientFence) provenance(res map[string]any, sc fenceScope, depth int) error {
	refuse := func(reason string) error { return &CompartmentError{ResourceType: "Provenance", Reason: reason} }
	targets, _ := res["target"].([]any)
	if len(targets) == 0 {
		return refuse("no target")
	}
	for _, t := range targets {
		f.run.resolutions++
		if f.run.resolutions > maxFenceResolutions {
			return refuse("too many references to resolve")
		}
		ref, _ := t.(map[string]any)
		s, _ := ref["reference"].(string)
		if s == "" {
			return refuse("target cannot be resolved")
		}
		if strings.HasPrefix(s, "#") {
			if s == "#" {
				return refuse("target is the Provenance itself")
			}
			cr := sc.contained[s[1:]]
			if cr == nil {
				return refuse("target cannot be resolved")
			}
			if err := f.resource(cr, sc.entries, &sc, depth); err != nil {
				return err
			}
			continue
		}
		if entry := sc.entries.byFullURL(s); entry != nil {
			if err := f.resource(entry, sc.entries, nil, depth); err != nil {
				return err
			}
			continue
		}
		path := s
		if strings.Contains(s, ":") {
			matched := false
			for _, b := range f.bases {
				if rest, ok := strings.CutPrefix(s, b+"/"); ok {
					path, matched = rest, true
					break
				}
			}
			if !matched {
				return refuse("target cannot be resolved")
			}
		}
		m := relativeRef.FindStringSubmatch(path)
		if m == nil {
			return refuse("target cannot be resolved")
		}
		key := m[1] + "/" + m[2]
		if sc.entries != nil && sc.entries.ambiguous[key] {
			return refuse("target names several entries")
		}
		if sc.entries != nil {
			if entry := sc.entries.typeID[key]; entry != nil {
				if err := f.resource(entry, sc.entries, nil, depth); err != nil {
					return err
				}
				continue
			}
		}
		if m[1] == "Patient" && f.ids[m[2]] {
			continue
		}
		if m[1] == "Patient" {
			return refuse("target is another patient")
		}
		return refuse("target cannot be resolved")
	}
	return nil
}

// patient accepts only the bound patient. A contained Patient's id is local,
// so it must carry the member identifier.
func (f patientFence) patient(p map[string]any, isContained bool) error {
	matched := false
	if raw, ok := p["identifier"]; ok {
		list, ok := raw.([]any)
		if !ok {
			return &CompartmentError{ResourceType: "Patient", Reason: "identifier is not a list"}
		}
		for _, i := range list {
			ident, _ := i.(map[string]any)
			if sys, _ := ident["system"].(string); sys == f.memberSystem {
				if val, _ := ident["value"].(string); val != f.member {
					return &CompartmentError{ResourceType: "Patient", Reason: "another patient"}
				}
				matched = true
			}
		}
	}
	if matched {
		return nil
	}
	if id, _ := p["id"].(string); !isContained && f.ids[id] {
		return nil
	}
	return &CompartmentError{ResourceType: "Patient", Reason: "another patient"}
}

// collect appends the values at path under v, walking lists at every step.
func collect(v any, path []string, out *[]any) {
	switch t := v.(type) {
	case []any:
		for _, e := range t {
			collect(e, path, out)
		}
	case map[string]any:
		if len(path) == 0 {
			*out = append(*out, t)
			return
		}
		if next, ok := t[path[0]]; ok {
			collect(next, path[1:], out)
		}
	default:
		if len(path) == 0 {
			*out = append(*out, v) // a malformed reference; reference() refuses it
		}
	}
}

var (
	relativeRef = regexp.MustCompile(`^([A-Z][A-Za-z]+)/([A-Za-z0-9\-.]{1,64})(/_history/[A-Za-z0-9\-.]{1,64})?$`)
	typeName    = regexp.MustCompile(`^[A-Z][A-Za-z]+$`)
)

// foreignType reads the resource type of an absolute RESTful URL
// (…/Type/id or …/Type/id/_history/v).
func foreignType(s string) (string, bool) {
	u, err := url.Parse(s)
	if err != nil || !u.IsAbs() || u.Opaque != "" {
		return "", false
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if n := len(segs); n >= 4 && segs[n-2] == "_history" {
		segs = segs[:n-2]
	}
	n := len(segs)
	if n < 2 || !typeName.MatchString(segs[n-2]) || segs[n-1] == "" {
		return "", false
	}
	return segs[n-2], true
}

type refError struct{ Reason string }

// reference reports whether ref targets a Patient; a Patient target that is
// not the bound patient, or a reference that cannot be resolved, is an
// error.
func (f patientFence) reference(v any, sc fenceScope) (bool, *refError) {
	ref, ok := v.(map[string]any)
	if !ok {
		return false, &refError{"reference is not an object"}
	}
	declared := ""
	if raw, ok := ref["type"]; ok {
		if declared, ok = raw.(string); !ok {
			return false, &refError{"reference type is not a string"}
		}
	}
	rawRef, hasRef := ref["reference"]
	if !hasRef {
		rawIdent, hasIdent := ref["identifier"]
		if !hasIdent {
			return false, nil // display only: names no target
		}
		ident, _ := rawIdent.(map[string]any)
		sys, _ := ident["system"].(string)
		val, _ := ident["value"].(string)
		switch {
		case sys == f.memberSystem && (declared == "" || declared == "Patient"):
			if val != f.member {
				return false, &refError{"reference to another patient"}
			}
			return true, nil
		case declared == "Patient":
			return false, &refError{"patient identifier cannot be resolved"}
		case declared == "":
			return false, &refError{"logical reference with no type cannot be resolved"}
		}
		return false, nil
	}
	s, ok := rawRef.(string)
	if !ok || s == "" {
		return false, &refError{"reference is not a string"}
	}
	target := func(res map[string]any, isContained bool) (bool, *refError) {
		rt, _ := res["resourceType"].(string)
		if declared != "" && declared != rt {
			return false, &refError{"reference type contradicts its target"}
		}
		if rt != "Patient" {
			return false, nil
		}
		if err := f.patient(res, isContained); err != nil {
			return false, &refError{"reference to another patient"}
		}
		return true, nil
	}
	if strings.HasPrefix(s, "#") {
		if s == "#" {
			return target(sc.container, false)
		}
		res := sc.contained[s[1:]]
		if res == nil {
			return false, &refError{"contained reference cannot be resolved"}
		}
		return target(res, true)
	}
	if res := sc.entries.byFullURL(s); res != nil {
		return target(res, false)
	}
	path := s
	if strings.Contains(s, ":") {
		matched := false
		for _, b := range f.bases {
			if rest, ok := strings.CutPrefix(s, b+"/"); ok {
				path, matched = rest, true
				break
			}
		}
		if !matched {
			// Another server's resource: allowed only when it is plainly
			// not a Patient.
			rt, ok := foreignType(s)
			switch {
			case !ok:
				return false, &refError{"reference cannot be resolved"}
			case rt == "Patient" || declared == "Patient":
				return false, &refError{"patient on another server cannot be resolved"}
			case declared != "" && declared != rt:
				return false, &refError{"reference type contradicts its target"}
			}
			return false, nil
		}
	}
	m := relativeRef.FindStringSubmatch(path)
	if m == nil {
		return false, &refError{"reference cannot be resolved"}
	}
	if declared != "" && declared != m[1] {
		return false, &refError{"reference type contradicts its target"}
	}
	if m[1] != "Patient" {
		return false, nil
	}
	key := m[1] + "/" + m[2]
	if sc.entries != nil && sc.entries.ambiguous[key] {
		return false, &refError{"reference names several entries"}
	}
	if sc.entries != nil {
		if entry := sc.entries.typeID[key]; entry != nil {
			// A Patient carried in the same Bundle: the reference means that
			// Patient, which must itself be the bound patient.
			return target(entry, false)
		}
	}
	if !f.ids[m[2]] {
		return false, &refError{"reference to another patient"}
	}
	return true, nil
}

// requesterRecordsFence is the fence a requester applies to the records a
// facility disclosed for member: a record binds to the member through its
// patient reference, which names either the member itself or a Patient the
// facility carried in the same Bundle with the member identifier.
func requesterRecordsFence(member string) patientFence {
	return newPatientFence(shnsdk.MemberSystem, member, nil, member)
}
