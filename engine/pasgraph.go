// pasgraph.go — the PAS response-graph REFERENCE-CLOSURE RULE (FR-G28).
//
// A payer's answer must CARRY EVERY RESOURCE IT NAMES. This reads a PAS response
// Bundle as a graph — one ClaimResponse, every entry identified by an absolute
// fullUrl, every Reference resolving to an entry, a contained resource of its own
// owner, or a version the graph actually holds — and refuses anything else. It
// changes not one byte: the rule decides whether a payer's bytes are relayed or
// refused, never what they say.
//
// Identity is the entry's fullUrl. Under a RESTful fullUrl the resource carries the
// id the address ends in and its relative references resolve against that base
// (FHIR R4). Under a URN (a resource with no id is placed under urn:uuid) it may
// carry no id, and FHIR gives its relative references no base; the rule reads a
// "[type]/[id]" there as the ONE entry whose RESTful identity ends in it, and
// refuses when none or more than one does — the answer must still carry, once,
// every record it names.
//
// It is a PRODUCT GUARD, not a helper of the terminal-response assembly that used
// to live beside it. That assembly is gone (a relayed decision is the payer's own
// message); the rule stayed, because a receiver that accepts an answer naming
// records it does not carry has accepted a message it cannot read. It is held to
// answers a REAL reference payer emitted, unpatched and corrected, by
// test/adversarial/payer_graph_capture_test.go — every raw capture refused, every
// corrected one accepted byte for byte.
//
// The bounded decoder below is part of the rule: a duplicate JSON member, a
// number reshaped by float conversion or a trailing document would each make
// "what this answer says" ambiguous before any reference was walked.
package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

const pasGraphMaxBytes = 8 << 20
const pasGraphMaxResources = 256
const pasGraphMaxReferences = 4096
const pasGraphMaxDepth = 64

func pasGraphError() error { return errors.New("engine: invalid or incomplete PAS response graph") }

// pasGraphRefusal is the reason a response graph was refused, stated so that the
// refusal can be read: which entry held the reference, where in it, the reference
// as the payer wrote it, the type it names and why the Bundle does not resolve it.
// The gateway never repairs the answer; this is what it says instead.
type pasGraphRefusal struct {
	Owner     string `json:"owner"`     // "entry 6 (ServiceRequest/1810)", or "Bundle" for Bundle/entry metadata
	Path      string `json:"path"`      // the element within the owner, e.g. "/subject"
	Target    string `json:"target"`    // the resource type the reference names, or "contained resource"
	Reference string `json:"reference"` // the reference string, exactly as the payer wrote it
	Why       string `json:"why"`
}

func (r *pasGraphRefusal) Error() string {
	return fmt.Sprintf("engine: PAS response graph: %s %s references %s %q, which %s", r.Owner, r.Path, r.Target, r.Reference, r.Why)
}

// pasGraphRefusalOf returns the named refusal behind err, or nil when the graph
// was refused for a structural reason that names no reference.
func pasGraphRefusalOf(err error) *pasGraphRefusal {
	var r *pasGraphRefusal
	if errors.As(err, &r) {
		return r
	}
	return nil
}

// pasGraphStructural is a structural refusal (an entry without an absolute
// fullUrl, two ClaimResponses, ...) stated with what was seen.
func pasGraphStructural(format string, args ...any) error {
	return fmt.Errorf("engine: PAS response graph: "+format, args...)
}

// pasReferenceTarget reads the resource type a reference names: "Patient" from
// "Patient/p", ".../Patient/p" or "Patient/p/_history/2"; "contained resource"
// for a local "#id".
func pasReferenceTarget(ref string) string {
	if strings.HasPrefix(ref, "#") {
		return "contained resource"
	}
	trimmed := ref
	if pos := strings.Index(trimmed, "/_history/"); pos >= 0 {
		trimmed = trimmed[:pos]
	}
	parts := strings.Split(strings.TrimSuffix(trimmed, "/"), "/")
	if len(parts) >= 2 && parts[len(parts)-2] != "" && !strings.Contains(parts[len(parts)-2], ":") {
		return parts[len(parts)-2]
	}
	return "resource"
}

func (e *pasGraphEntry) label() string {
	if e == nil {
		return "Bundle"
	}
	typ, _ := e.resource["resourceType"].(string)
	id, ok := e.resource["id"].(string)
	if !ok {
		// An id-less resource under a URN fullUrl is known by that fullUrl.
		return fmt.Sprintf("entry %d (%s %s)", e.index, typ, e.fullURL)
	}
	return fmt.Sprintf("entry %d (%s/%s)", e.index, typ, id)
}

func validatePASBundleGraph(raw []byte) error {
	g, err := readPASGraph(raw)
	if err != nil {
		return err
	}
	return g.validate()
}

type pasGraphEntry struct {
	fullURL  string
	resource map[string]any
	index    int
}
type pasGraph struct {
	bundle          map[string]any
	entries         []any
	byURL           map[string]*pasGraphEntry
	response        *pasGraphEntry
	resources, refs int
}

func readPASGraph(raw []byte) (*pasGraph, error) {
	if len(raw) > pasGraphMaxBytes {
		return nil, pasGraphError()
	}
	g := &pasGraph{byURL: make(map[string]*pasGraphEntry)}
	if decodePASObject(raw, &g.bundle) != nil || g.bundle["resourceType"] != "Bundle" || g.bundle["type"] != "collection" {
		return nil, pasGraphError()
	}
	var ok bool
	g.entries, ok = g.bundle["entry"].([]any)
	if !ok || len(g.entries) == 0 {
		return nil, pasGraphStructural("the Bundle has no entries")
	}
	if len(g.entries) > pasGraphMaxResources {
		return nil, pasGraphStructural("the Bundle has %d entries, more than the %d this rule reads", len(g.entries), pasGraphMaxResources)
	}
	for i, v := range g.entries {
		e, ok := v.(map[string]any)
		if !ok {
			return nil, pasGraphStructural("entry %d is not an object", i)
		}
		full, ok := e["fullUrl"].(string)
		if !ok || full == "" {
			return nil, pasGraphStructural("entry %d has no fullUrl", i)
		}
		u, err := url.Parse(full)
		if err != nil || !u.IsAbs() || u.Fragment != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(full, "/_history/") {
			return nil, pasGraphStructural("entry %d fullUrl %q is not an absolute, versionless identity", i, full)
		}
		r, ok := e["resource"].(map[string]any)
		if !ok {
			return nil, pasGraphStructural("entry %d (%s) carries no resource", i, full)
		}
		// An entry's identity is its fullUrl. Under a RESTful (http/https) fullUrl
		// the resource carries the id that address ends in; under a URN (FHIR R4
		// Bundle: a resource with no id is placed under urn:uuid) the resource may
		// carry no id at all, and nothing is minted for it. An id the payer did
		// state must be usable under either.
		typ, tok := r["resourceType"].(string)
		idValue, stated := r["id"]
		id, iok := idValue.(string)
		if !tok || !pasResourceTypeName(typ) || (stated && (!iok || !pasSafeResourceID(id))) {
			return nil, pasGraphStructural("entry %d (%s) carries a resource without a usable resourceType and id", i, full)
		}
		if u.Scheme == "http" || u.Scheme == "https" {
			if !stated {
				return nil, pasGraphStructural("entry %d (%s) carries a resource without a usable resourceType and id", i, full)
			}
			if u.Host == "" || !strings.HasSuffix(u.Path, "/"+typ+"/"+id) {
				return nil, pasGraphStructural("entry %d fullUrl %q does not end in its resource's identity %s/%s", i, full, typ, id)
			}
		} else if !stated && u.Scheme != "urn" {
			return nil, pasGraphStructural("entry %d (%s) carries a resource without an id under a fullUrl that is neither RESTful nor a URN", i, full)
		}
		if _, exists := g.byURL[full]; exists {
			return nil, pasGraphStructural("entry %d repeats the identity %s", i, full)
		}
		entry := &pasGraphEntry{full, r, i}
		g.byURL[full] = entry
		if typ == "ClaimResponse" {
			if g.response != nil {
				return nil, pasGraphStructural("the Bundle carries more than one ClaimResponse (%s and %s)", g.response.fullURL, full)
			}
			g.response = entry
		}
	}
	if g.response == nil {
		return nil, pasGraphStructural("the Bundle carries no ClaimResponse")
	}
	return g, nil
}

func (g *pasGraph) validate() error { return g.validateWithReferencePolicy(nil) }

func (g *pasGraph) validateWithReferencePolicy(policy *authoredPASReferencePolicy) error {
	if !policy.matches(g) {
		return pasGraphError()
	}
	// Bundle and entry metadata have no RESTful containing resource fullUrl.
	// Their references must therefore be explicit absolute identities.
	for _, key := range pasSortedKeys(g.bundle) {
		value := g.bundle[key]
		if key != "entry" && key != "resourceType" {
			if err := g.walkWithReferencePolicy(value, nil, nil, 0, false, pasReferencePath("", key), policy); err != nil {
				return err
			}
		}
	}
	for i, entry := range g.entries {
		fields := entry.(map[string]any)
		for _, key := range pasSortedKeys(fields) {
			value := fields[key]
			if key != "resource" {
				if err := g.walkWithReferencePolicy(value, nil, nil, 0, false, pasReferencePath(pasReferencePath("/entry", strconv.Itoa(i)), key), policy); err != nil {
					return err
				}
			}
		}
	}
	// Each identity is traversed once, in Bundle order, including disconnected
	// retained siblings. Reference cycles therefore do not recurse through the
	// referenced resource, and the reference a refusal names is the first one in
	// the Bundle's own order, the same on every run.
	for _, e := range g.ordered() {
		contained := make(map[string]map[string]any)
		if list, exists := e.resource["contained"]; exists {
			arr, ok := list.([]any)
			if !ok {
				return pasGraphStructural("%s contained is not an array", e.label())
			}
			for _, v := range arr {
				r, ok := v.(map[string]any)
				if !ok {
					return pasGraphStructural("%s contains a value that is not a resource", e.label())
				}
				typ, tok := r["resourceType"].(string)
				id, ok := r["id"].(string)
				if !tok || !pasResourceTypeName(typ) || !ok || !pasSafeResourceID(id) || contained[id] != nil {
					return pasGraphStructural("%s contains a resource without a unique, usable resourceType and id", e.label())
				}
				contained[id] = r
			}
		}
		if err := g.walkWithReferencePolicy(e.resource, e, contained, 0, false, "", policy); err != nil {
			return err
		}
	}
	return nil
}

func (g *pasGraph) walkWithReferencePolicy(v any, owner *pasGraphEntry, contained map[string]map[string]any, depth int, inContained bool, path string, policy *authoredPASReferencePolicy) error {
	if depth > pasGraphMaxDepth {
		return pasGraphStructural("%s nests deeper than %d levels", owner.label(), pasGraphMaxDepth)
	}
	switch x := v.(type) {
	case map[string]any:
		if typ, ok := x["resourceType"].(string); ok {
			g.resources++
			if g.resources > pasGraphMaxResources {
				return pasGraphStructural("the Bundle carries more than %d resources", pasGraphMaxResources)
			}
			if typ == "Bundle" || typ == "Parameters" {
				return pasGraphStructural("%s carries a %s at %s", owner.label(), typ, path)
			}
			if depth > 0 {
				inContained = true
				if _, exists := x["contained"]; exists {
					return pasGraphStructural("%s carries a contained resource that itself contains resources at %s", owner.label(), path)
				}
			}
		}
		if ref, exists := x["reference"]; exists {
			s, ok := ref.(string)
			g.refs++
			if !ok || s == "" {
				return pasGraphStructural("%s carries an empty reference at %s", owner.label(), path)
			}
			if g.refs > pasGraphMaxReferences {
				return pasGraphStructural("the Bundle carries more than %d references", pasGraphMaxReferences)
			}
			if why := g.resolve(s, owner, contained, inContained); why != "" && !policy.allows(owner, path, x) {
				return &pasGraphRefusal{Owner: owner.label(), Path: path, Target: pasReferenceTarget(s), Reference: s, Why: why}
			}
		}
		for _, key := range pasSortedKeys(x) {
			if err := g.walkWithReferencePolicy(x[key], owner, contained, depth+1, inContained, pasReferencePath(path, key), policy); err != nil {
				return err
			}
		}
	case []any:
		for index, child := range x {
			if err := g.walkWithReferencePolicy(child, owner, contained, depth+1, inContained, pasReferencePath(path, strconv.Itoa(index)), policy); err != nil {
				return err
			}
		}
	}
	return nil
}

// resolve reports why ref, written inside owner, does not resolve to something
// the Bundle carries — "" when it does. The rules are FHIR R4's: a "#id" names a
// resource contained in its owner, a relative reference resolves only against a
// RESTful owner fullUrl, an absolute one must be an entry's identity, and a
// versioned one must name the version the entry actually holds.
func (g *pasGraph) resolve(ref string, owner *pasGraphEntry, contained map[string]map[string]any, inContained bool) string {
	if strings.HasPrefix(ref, "#") {
		if owner == nil {
			return "names a contained resource, and Bundle metadata has no containing resource"
		}
		if ref == "#" {
			if inContained {
				return ""
			}
			return "names its containing resource from outside any contained resource"
		}
		if contained[strings.TrimPrefix(ref, "#")] != nil {
			return ""
		}
		return "names no resource contained in " + owner.label()
	}
	u, err := url.Parse(ref)
	if err != nil || u.Fragment != "" || u.RawQuery != "" || u.ForceQuery {
		return "is not a resolvable resource identity"
	}
	full := ref
	if !u.IsAbs() {
		if owner == nil {
			return "is relative, and Bundle metadata has no base to resolve it against"
		}
		if u.RawPath != "" {
			return "is not a resolvable resource identity"
		}
		// FHIR R4 defines relative resolution only for a RESTful owner fullUrl. An
		// owner identified by a URN has no base; its [type]/[id] reference is read
		// as the one entry whose RESTful identity ends in it (resolveUnderURN).
		base, err := url.Parse(owner.fullURL)
		if err != nil || base.RawPath != "" || strings.HasPrefix(ref, "/") {
			return "is relative, and " + owner.label() + " has no RESTful fullUrl to resolve it against"
		}
		if base.Scheme == "urn" {
			resolved, why := g.resolveUnderURN(ref, owner)
			if why != "" {
				return why
			}
			full = resolved
		} else {
			if base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
				return "is relative, and " + owner.label() + " has no RESTful fullUrl to resolve it against"
			}
			typ := owner.resource["resourceType"].(string)
			id, ok := owner.resource["id"].(string)
			suffix := "/" + typ + "/" + id
			if !ok || !strings.HasSuffix(base.Path, suffix) {
				return "is relative, and " + owner.label() + " has no RESTful fullUrl to resolve it against"
			}
			base.Path = strings.TrimSuffix(base.Path, suffix) + "/" + u.Path
			full = base.String()
		}
	}
	version := ""
	if pos := strings.Index(full, "/_history/"); pos >= 0 {
		version = full[pos+10:]
		full = full[:pos]
		if version == "" || strings.Contains(version, "/") {
			return "names no usable version"
		}
	}
	target := g.byURL[full]
	if target == nil {
		if full != ref {
			return fmt.Sprintf("resolves to %s, which is no entry of the Bundle", full)
		}
		return "is no entry of the Bundle"
	}
	if version != "" {
		meta, ok := target.resource["meta"].(map[string]any)
		if !ok || meta["versionId"] != version {
			return fmt.Sprintf("names version %s, which %s does not hold", version, target.label())
		}
	}
	return ""
}

// resolveUnderURN reads a relative reference written inside an entry identified
// by a URN. FHIR gives such a reference no base to resolve against, so the rule
// reads it by identity: a "[type]/[id]" (optionally "/_history/[vid]") names the
// ONE entry whose RESTful fullUrl ends in "/[type]/[id]". It returns that entry's
// fullUrl (with the version kept for the caller's version check), or why the
// reference resolves to nothing: not of that form, no such entry, or more than
// one — which is named with every candidate, so a payer can read which of its
// own records collided. Bytes are never changed; this is how they are read.
func (g *pasGraph) resolveUnderURN(ref string, owner *pasGraphEntry) (string, string) {
	where := "is relative under " + owner.label() + ", which is identified by a URN, and "
	trimmed, version := ref, ""
	if pos := strings.Index(ref, "/_history/"); pos >= 0 {
		trimmed, version = ref[:pos], ref[pos:]
	}
	parts := strings.Split(trimmed, "/")
	if len(parts) != 2 || parts[0] == "" || strings.Contains(parts[0], ":") || !pasSafeResourceID(parts[1]) {
		return "", where + "is not of the form [type]/[id]"
	}
	// Whole path segments: "/Patient/12" is not the end of ".../Patient/112", and
	// "/Person/p" is not the end of ".../RelatedPerson/p". The match reads the
	// decoded path while identities are kept as the payer's raw strings; a
	// percent-encoded id can therefore match here and be named raw, never the
	// other way round, and a collision is refused as ambiguous.
	suffix := "/" + trimmed
	var matches []string
	for _, e := range g.ordered() {
		u, err := url.Parse(e.fullURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			continue
		}
		if strings.HasSuffix(u.Path, suffix) {
			matches = append(matches, e.fullURL)
		}
	}
	switch len(matches) {
	case 0:
		return "", where + "no entry of the Bundle has a RESTful identity ending in " + trimmed
	case 1:
		return matches[0] + version, ""
	default:
		return "", fmt.Sprintf("%s%d entries have a RESTful identity ending in %s (%s)", where, len(matches), trimmed, pasNamedCandidates(matches))
	}
}

// pasRefusalCandidateLimit bounds how many colliding identities a refusal names;
// the count beside it stays exact, so a payer whose answer repeats an identity
// hundreds of times is told so without each refusal record growing with it.
const pasRefusalCandidateLimit = 16

func pasNamedCandidates(matches []string) string {
	if len(matches) <= pasRefusalCandidateLimit {
		return strings.Join(matches, ", ")
	}
	return fmt.Sprintf("%s, +%d more", strings.Join(matches[:pasRefusalCandidateLimit], ", "), len(matches)-pasRefusalCandidateLimit)
}

// pasResourceTypeName is whether typ is shaped like a FHIR resource type name:
// letters only, so that no "Foo/Patient" can make an identity end in a segment
// it does not have.
func pasResourceTypeName(typ string) bool {
	if typ == "" {
		return false
	}
	for _, c := range typ {
		if c > unicode.MaxASCII || !unicode.IsLetter(c) {
			return false
		}
	}
	return true
}

// Decode without float conversion or last-key-wins ambiguity. The same bounded
// parser protects both the retained response and the polled replacement.
func decodePASObject(raw []byte, dst *map[string]any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	v, err := decodePASValue(d, 0)
	if err != nil {
		return pasGraphError()
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return pasGraphError()
	}
	if _, err = d.Token(); err != io.EOF {
		return pasGraphError()
	}
	*dst = obj
	return nil
}
func decodePASValue(d *json.Decoder, depth int) (any, error) {
	if depth > pasGraphMaxDepth {
		return nil, pasGraphError()
	}
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch t {
	case json.Delim('{'):
		obj := make(map[string]any)
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, err
			}
			s, ok := key.(string)
			if !ok {
				return nil, pasGraphError()
			}
			if _, exists := obj[s]; exists {
				return nil, pasGraphError()
			}
			child, err := decodePASValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			obj[s] = child
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return nil, pasGraphError()
		}
		return obj, nil
	case json.Delim('['):
		arr := []any{}
		for d.More() {
			child, err := decodePASValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			arr = append(arr, child)
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return nil, pasGraphError()
		}
		return arr, nil
	default:
		if _, ok := t.(json.Delim); ok {
			return nil, pasGraphError()
		}
		return t, nil
	}
}

// ordered is every identity the graph holds, in Bundle order (then by fullUrl,
// for a graph built without indexes). Derived from byURL rather than kept beside
// it, so a graph assembled by any constructor is walked whole.
func (g *pasGraph) ordered() []*pasGraphEntry {
	out := make([]*pasGraphEntry, 0, len(g.byURL))
	for _, e := range g.byURL {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].index != out[j].index {
			return out[i].index < out[j].index
		}
		return out[i].fullURL < out[j].fullURL
	})
	return out
}

// pasSortedKeys is the object's member names in a fixed order: the walk visits
// them so, and the first refusal it reports is therefore the same for the same
// bytes on every run.
func pasSortedKeys(x map[string]any) []string {
	keys := make([]string, 0, len(x))
	for k := range x {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func pasSafeResourceID(id string) bool {
	if len(id) == 0 || len(id) > 64 || id == "." || id == ".." {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}
