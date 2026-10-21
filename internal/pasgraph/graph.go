// Package pasgraph is the PAS response-graph REFERENCE-CLOSURE RULE (FR-G28),
// shared by the gateway engine and the Prior Authorization workstream's
// readers of a native PAS answer (package pasanswer), so both read an answer
// by one definition.
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
package pasgraph

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

// The bounds the rule reads within: bytes, resources, references and depth.
const MaxBytes = 8 << 20
const MaxResources = 256
const MaxReferences = 4096
const MaxDepth = 64

// The causes a refusal of a response graph is named by: a closed set, each
// naming why the Bundle was not read, never a value it carries. Package
// pasanswer documents each (PASRefusal*).
const (
	RefusalUndecodable      = "undecodable"
	RefusalNotCollection    = "not-collection"
	RefusalNoEntries        = "no-entries"
	RefusalEntry            = "entry"
	RefusalFullURL          = "fullurl"
	RefusalIdentity         = "identity"
	RefusalRepeatedIdentity = "repeated-identity"
	RefusalNoClaimResponse  = "no-claimresponse"
	RefusalClaimResponses   = "claimresponses"
	RefusalRequest          = "request"
	RefusalContained        = "contained"
	RefusalNested           = "nested"
	RefusalEmptyReference   = "empty-reference"
	RefusalReference        = "reference"
	RefusalBound            = "bound"
	RefusalPolicy           = "policy"
	RefusalNoDecision       = "no-decision"
	RefusalDecision         = "decision"
)

// ReferencePolicy names the exact references a verified holder-authored
// attachment may leave unresolved (the engine's authored attachment policy).
// A nil ReferencePolicy matches every graph and allows no reference.
type ReferencePolicy interface {
	// Matches reports whether the policy was made for g.
	Matches(g *Graph) bool
	// Allows reports whether node, at path inside owner, may stay unresolved.
	Allows(owner *Entry, path string, node map[string]any) bool
}

// Refuse is a refusal of a response graph this rule could not read at
// all, with its cause (the Refusal* constants).
func Refuse(cause string) error { return &CauseError{cause: cause} }

// CauseError is a refusal that names no reference, with its cause
// from the closed set of the Refusal* constants: a reader of retained evidence
// names it by that cause alone, never by its text, which can quote the
// payer's values.
type CauseError struct {
	cause string
	msg   string // "" for an answer the rule could not read at all
}

// Cause is the refusal's cause, one of the Refusal* constants.
func (e *CauseError) Cause() string { return e.cause }

func (e *CauseError) Error() string {
	if e.msg == "" {
		return "engine: invalid or incomplete PAS response graph"
	}
	return "engine: PAS response graph: " + e.msg
}

// Refusal is the reason a response graph was refused, stated so that the
// refusal can be read: which entry held the reference, where in it, the reference
// as the payer wrote it, the type it names and why the Bundle does not resolve it.
// The gateway never repairs the answer; this is what it says instead.
type Refusal struct {
	Owner     string `json:"owner"`     // "entry 6 (ServiceRequest/1810)", or "Bundle" for Bundle/entry metadata
	Path      string `json:"path"`      // the element within the owner, e.g. "/subject"
	Target    string `json:"target"`    // the resource type the reference names, or "contained resource"
	Reference string `json:"reference"` // the reference string, exactly as the payer wrote it
	Why       string `json:"why"`
	// holder is the resource type of the Owner ("Bundle" for Bundle or entry
	// metadata), carried so a reader need not parse it back from the label.
	holder string
}

// Holder is the resource type of the entry holding the reference ("Bundle"
// for Bundle or entry metadata).
func (r *Refusal) Holder() string { return r.holder }

func (r *Refusal) Error() string {
	return fmt.Sprintf("engine: PAS response graph: %s %s references %s %q, which %s", r.Owner, r.Path, r.Target, r.Reference, r.Why)
}

// RefusalOf returns the named refusal behind err, or nil when the graph
// was refused for a structural reason that names no reference.
func RefusalOf(err error) *Refusal {
	var r *Refusal
	if errors.As(err, &r) {
		return r
	}
	return nil
}

// Structural is a structural refusal (an entry without an absolute
// fullUrl, two ClaimResponses, ...) stated with what was seen.
func Structural(cause, format string, args ...any) error {
	return &CauseError{cause: cause, msg: fmt.Sprintf(format, args...)}
}

// ReferenceTarget reads the resource type a reference names: "Patient" from
// "Patient/p", ".../Patient/p" or "Patient/p/_history/2"; "contained resource"
// for a local "#id"; "resource" when the segment before the id is no type name
// (a malformed "Patient/p/_history/2/x").
func ReferenceTarget(ref string) string {
	if strings.HasPrefix(ref, "#") {
		return "contained resource"
	}
	trimmed := StripHistoryRef(ref)
	parts := strings.Split(strings.TrimSuffix(trimmed, "/"), "/")
	if len(parts) >= 2 && ResourceTypeName(parts[len(parts)-2]) {
		return parts[len(parts)-2]
	}
	return "resource"
}

// ResourceType is the entry's resource type, "Bundle" for Bundle or entry
// metadata (a nil entry), as label names it.
func (e *Entry) ResourceType() string {
	if e == nil {
		return "Bundle"
	}
	typ, _ := e.Resource["resourceType"].(string)
	return typ
}

// Label names the entry as a refusal does: "entry 6 (ServiceRequest/1810)",
// or "Bundle" for Bundle or entry metadata (a nil entry).
func (e *Entry) Label() string {
	if e == nil {
		return "Bundle"
	}
	typ, _ := e.Resource["resourceType"].(string)
	id, ok := e.Resource["id"].(string)
	if !ok {
		// An id-less resource under a URN fullUrl is known by that fullUrl.
		return fmt.Sprintf("entry %d (%s %s)", e.Index, typ, e.FullURL)
	}
	return fmt.Sprintf("entry %d (%s/%s)", e.Index, typ, id)
}

// ValidateBundle reads raw as a response graph (Read) and applies the rule.
func ValidateBundle(raw []byte) error {
	g, err := Read(raw)
	if err != nil {
		return err
	}
	return g.Validate()
}

// Entry is one Bundle entry of a graph: its fullUrl (its identity), its
// resource and its position in the Bundle.
type Entry struct {
	FullURL  string
	Resource map[string]any
	Index    int
}

// Graph is a PAS Bundle read as a graph of identities (Read, ReadOf).
type Graph struct {
	Bundle          map[string]any
	Entries         []any
	ByURL           map[string]*Entry
	Response        *Entry
	resources, refs int
	// unresolvedAllowed, when set, names the references an answer may leave
	// unresolved at its IG line (ValidateAnswerAt); nil requires every one to
	// resolve.
	unresolvedAllowed func(owner *Entry, path, ref string) bool
	// OpenSubjects is set for an answer at an IG line that lets it return a
	// resource as a reference only (2.1.0): a subject that no entry resolves
	// is read by the patient its Patient/<id> names (the engine's pasGraphSubjectMismatch).
	OpenSubjects bool
	// tailIdentity is the one identity an open-subjects walk has bound by a
	// Patient/<id> alone (SameSubject): every other it binds so must be it.
	tailIdentity string
}

// Read reads a response graph that must carry its one ClaimResponse.
func Read(raw []byte) (*Graph, error) { return ReadOf(raw, true) }

// ReadOf reads a response graph, requiring its one ClaimResponse only
// when needResponse is set.
func ReadOf(raw []byte, needResponse bool) (*Graph, error) {
	if len(raw) > MaxBytes {
		return nil, Refuse(RefusalBound)
	}
	g := &Graph{ByURL: make(map[string]*Entry)}
	if err := DecodeObject(raw, &g.Bundle); err != nil {
		return nil, err
	}
	if g.Bundle["resourceType"] != "Bundle" || g.Bundle["type"] != "collection" {
		return nil, Refuse(RefusalNotCollection)
	}
	var ok bool
	g.Entries, ok = g.Bundle["entry"].([]any)
	if !ok || len(g.Entries) == 0 {
		return nil, Structural(RefusalNoEntries, "the Bundle has no entries")
	}
	if len(g.Entries) > MaxResources {
		return nil, Structural(RefusalBound, "the Bundle has %d entries, more than the %d this rule reads", len(g.Entries), MaxResources)
	}
	for i, v := range g.Entries {
		e, ok := v.(map[string]any)
		if !ok {
			return nil, Structural(RefusalEntry, "entry %d is not an object", i)
		}
		full, ok := e["fullUrl"].(string)
		if !ok || full == "" {
			return nil, Structural(RefusalEntry, "entry %d has no fullUrl", i)
		}
		u, err := url.Parse(full)
		if err != nil || !u.IsAbs() || u.Fragment != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(full, "/_history/") {
			return nil, Structural(RefusalFullURL, "entry %d fullUrl %q is not an absolute, versionless identity", i, full)
		}
		r, ok := e["resource"].(map[string]any)
		if !ok {
			return nil, Structural(RefusalEntry, "entry %d (%s) carries no resource", i, full)
		}
		// An entry's identity is its fullUrl. Under a RESTful (http/https) fullUrl
		// the resource carries the id that address ends in; under a URN (FHIR R4
		// Bundle: a resource with no id is placed under urn:uuid) the resource may
		// carry no id at all, and nothing is minted for it. An id the payer did
		// state must be usable under either.
		typ, tok := r["resourceType"].(string)
		idValue, stated := r["id"]
		id, iok := idValue.(string)
		if !tok || !ResourceTypeName(typ) || (stated && (!iok || !SafeResourceID(id))) {
			return nil, Structural(RefusalIdentity, "entry %d (%s) carries a resource without a usable resourceType and id", i, full)
		}
		if u.Scheme == "http" || u.Scheme == "https" {
			if !stated {
				return nil, Structural(RefusalIdentity, "entry %d (%s) carries a resource without a usable resourceType and id", i, full)
			}
			if u.Host == "" || !strings.HasSuffix(u.Path, "/"+typ+"/"+id) {
				return nil, Structural(RefusalFullURL, "entry %d fullUrl %q does not end in its resource's identity %s/%s", i, full, typ, id)
			}
		} else if !stated && u.Scheme != "urn" {
			return nil, Structural(RefusalFullURL, "entry %d (%s) carries a resource without an id under a fullUrl that is neither RESTful nor a URN", i, full)
		}
		if _, exists := g.ByURL[full]; exists {
			return nil, Structural(RefusalRepeatedIdentity, "entry %d repeats the identity %s", i, full)
		}
		entry := &Entry{full, r, i}
		g.ByURL[full] = entry
		if typ == "ClaimResponse" {
			if g.Response != nil {
				return nil, Structural(RefusalClaimResponses, "the Bundle carries more than one ClaimResponse (%s and %s)", g.Response.FullURL, full)
			}
			g.Response = entry
		}
	}
	if g.Response == nil && needResponse {
		return nil, Structural(RefusalNoClaimResponse, "the Bundle carries no ClaimResponse")
	}
	return g, nil
}

// Validate applies the rule with every reference required to resolve.
func (g *Graph) Validate() error { return g.ValidateWithReferencePolicy(nil) }

// ValidateBundleAt is ValidateBundle for a PAS $submit or
// amendment answer exchanged at IG line (ValidateAnswerAt).
func ValidateBundleAt(raw []byte, line string) error {
	g, err := Read(raw)
	if err != nil {
		return err
	}
	return g.ValidateAnswerAt(line)
}

// ValidateAnswerAt is Validate for a PAS $submit or amendment answer at the IG
// line it was exchanged at (FR-G28), each line as its own specification says:
//   - "2.1": an answer need carry only its ClaimResponse and what the decision
//     made or changed; anything the request sent may be returned as a
//     reference only. Its ClaimResponse.request must name the request Claim,
//     by a reference to a Claim or by an identifier with a value
//     (ClaimResponseRequestNamesClaim), and no reference need resolve (the
//     rest is the profile's, $validate's).
//   - "2.2": the request Claim need not be in the Bundle, but
//     ClaimResponse.request must name it the same way; a reference to that
//     Claim, the request's own or any other, need not resolve, and every
//     other reference must.
//   - any other line, or none: every reference must resolve, the request
//     Claim's included (2.0.1).
//
// The graph's structural rules (one ClaimResponse, absolute fullUrls, no
// empty reference, no nested contained, the bounds) hold at every line.
func (g *Graph) ValidateAnswerAt(line string) error {
	switch line {
	case "2.1", "2.2":
		if err := g.requireRequestNamed(); err != nil {
			return err
		}
		claim := ""
		if ref, _ := g.Response.Resource["request"].(map[string]any)["reference"].(string); ref != "" && referenceNamesClaim(ref, g.responseEntryType) {
			claim = g.referenceIdentity(ref, g.Response)
		}
		all := line == "2.1"
		g.unresolvedAllowed = func(owner *Entry, _ string, ref string) bool {
			return all || claim != "" && g.referenceIdentity(ref, owner) == claim
		}
		defer func() { g.unresolvedAllowed = nil }()
	}
	return g.Validate()
}

// requireRequestNamed refuses an answer whose ClaimResponse.request names the
// request Claim neither by a reference to a Claim nor by an identifier with a
// value (ClaimResponseRequestNamesClaim).
func (g *Graph) requireRequestNamed() error {
	if ClaimResponseRequestNamesClaim(g.Response.Resource, g.responseEntryType) {
		return nil
	}
	return Structural(RefusalRequest, "%s /request names the request Claim neither by a reference to a Claim nor by identifier", g.Response.Label())
}

// ClaimResponseRequestNamesClaim reports whether a ClaimResponse's request
// names the Claim it answers: by a reference to a Claim (referenceNamesClaim),
// or by an identifier with a value. entryType reads the resource type of the
// Bundle entry a reference names.
func ClaimResponseRequestNamesClaim(cr map[string]any, entryType func(ref string) string) bool {
	request, _ := cr["request"].(map[string]any)
	if ref, _ := request["reference"].(string); ref != "" && referenceNamesClaim(ref, entryType) {
		return true
	}
	if id, _ := request["identifier"].(map[string]any); id != nil {
		if value, _ := id["value"].(string); strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

// referenceNamesClaim reports whether a literal reference names a Claim: its
// type segment says Claim, or it names a Bundle entry that is one (a
// urn:uuid reference carries no type). A reference to anything else names no
// Claim.
func referenceNamesClaim(ref string, entryType func(ref string) string) bool {
	return ReferenceTarget(ref) == "Claim" || entryType != nil && entryType(ref) == "Claim"
}

// responseEntryType is the resource type of the entry a reference written in
// the ClaimResponse names, "" when it names no entry.
func (g *Graph) responseEntryType(ref string) string {
	if e := g.ByURL[g.referenceIdentity(ref, g.Response)]; e != nil {
		rt, _ := e.Resource["resourceType"].(string)
		return rt
	}
	return ""
}

// referenceIdentity is the identity a literal reference written inside owner
// names, whether or not the Bundle carries it: an absolute reference as
// written, a relative one against a RESTful owner's base, without a trailing
// version. A relative reference under a URN owner is its own text, so two
// such references compare by what they say. "" when it names no identity.
func (g *Graph) referenceIdentity(ref string, owner *Entry) string {
	ref = StripHistoryRef(ref)
	u, err := url.Parse(ref)
	if err != nil || strings.HasPrefix(ref, "#") || u.Fragment != "" || u.RawQuery != "" {
		return ""
	}
	if u.IsAbs() {
		return ref
	}
	if owner == nil {
		return ""
	}
	base, err := url.Parse(owner.FullURL)
	if err != nil || strings.HasPrefix(ref, "/") {
		return ""
	}
	if base.Scheme == "urn" {
		return "relative:" + ref
	}
	typ, _ := owner.Resource["resourceType"].(string)
	id, _ := owner.Resource["id"].(string)
	suffix := "/" + typ + "/" + id
	if base.Host == "" || id == "" || !strings.HasSuffix(base.Path, suffix) {
		return ""
	}
	base.Path = strings.TrimSuffix(base.Path, suffix) + "/" + u.Path
	return base.String()
}

// ValidateWithReferencePolicy is Validate with the references policy allows
// left unresolved; a policy made for another graph is refused (RefusalPolicy).
func (g *Graph) ValidateWithReferencePolicy(policy ReferencePolicy) error {
	if policy != nil && !policy.Matches(g) {
		return Refuse(RefusalPolicy)
	}
	// Bundle and entry metadata have no RESTful containing resource fullUrl.
	// Their references must therefore be explicit absolute identities.
	for _, key := range SortedKeys(g.Bundle) {
		value := g.Bundle[key]
		if key != "entry" && key != "resourceType" {
			if err := g.walkWithReferencePolicy(value, nil, nil, 0, false, ReferencePath("", key), policy); err != nil {
				return err
			}
		}
	}
	for i, entry := range g.Entries {
		fields := entry.(map[string]any)
		for _, key := range SortedKeys(fields) {
			value := fields[key]
			if key != "resource" {
				if err := g.walkWithReferencePolicy(value, nil, nil, 0, false, ReferencePath(ReferencePath("/entry", strconv.Itoa(i)), key), policy); err != nil {
					return err
				}
			}
		}
	}
	// Each identity is traversed once, in Bundle order, including disconnected
	// retained siblings. Reference cycles therefore do not recurse through the
	// referenced resource, and the reference a refusal names is the first one in
	// the Bundle's own order, the same on every run.
	for _, e := range g.Ordered() {
		contained := make(map[string]map[string]any)
		if list, exists := e.Resource["contained"]; exists {
			arr, ok := list.([]any)
			if !ok {
				return Structural(RefusalContained, "%s contained is not an array", e.Label())
			}
			for _, v := range arr {
				r, ok := v.(map[string]any)
				if !ok {
					return Structural(RefusalContained, "%s contains a value that is not a resource", e.Label())
				}
				typ, tok := r["resourceType"].(string)
				id, ok := r["id"].(string)
				if !tok || !ResourceTypeName(typ) || !ok || !SafeResourceID(id) || contained[id] != nil {
					return Structural(RefusalContained, "%s contains a resource without a unique, usable resourceType and id", e.Label())
				}
				contained[id] = r
			}
		}
		if err := g.walkWithReferencePolicy(e.Resource, e, contained, 0, false, "", policy); err != nil {
			return err
		}
	}
	return nil
}

func (g *Graph) walkWithReferencePolicy(v any, owner *Entry, contained map[string]map[string]any, depth int, inContained bool, path string, policy ReferencePolicy) error {
	if depth > MaxDepth {
		return Structural(RefusalBound, "%s nests deeper than %d levels", owner.Label(), MaxDepth)
	}
	switch x := v.(type) {
	case map[string]any:
		if typ, ok := x["resourceType"].(string); ok {
			g.resources++
			if g.resources > MaxResources {
				return Structural(RefusalBound, "the Bundle carries more than %d resources", MaxResources)
			}
			if typ == "Bundle" || typ == "Parameters" {
				return Structural(RefusalNested, "%s carries a %s at %s", owner.Label(), typ, path)
			}
			if depth > 0 {
				inContained = true
				if _, exists := x["contained"]; exists {
					return Structural(RefusalContained, "%s carries a contained resource that itself contains resources at %s", owner.Label(), path)
				}
			}
		}
		if ref, exists := x["reference"]; exists {
			s, ok := ref.(string)
			g.refs++
			if !ok || s == "" {
				return Structural(RefusalEmptyReference, "%s carries an empty reference at %s", owner.Label(), path)
			}
			if g.refs > MaxReferences {
				return Structural(RefusalBound, "the Bundle carries more than %d references", MaxReferences)
			}
			if why := g.Resolve(s, owner, contained, inContained); why != "" && !(policy != nil && policy.Allows(owner, path, x)) && (g.unresolvedAllowed == nil || !g.unresolvedAllowed(owner, path, s)) {
				return &Refusal{Owner: owner.Label(), Path: path, Target: ReferenceTarget(s), Reference: s, Why: why, holder: owner.ResourceType()}
			}
		}
		for _, key := range SortedKeys(x) {
			if err := g.walkWithReferencePolicy(x[key], owner, contained, depth+1, inContained, ReferencePath(path, key), policy); err != nil {
				return err
			}
		}
	case []any:
		for index, child := range x {
			if err := g.walkWithReferencePolicy(child, owner, contained, depth+1, inContained, ReferencePath(path, strconv.Itoa(index)), policy); err != nil {
				return err
			}
		}
	}
	return nil
}

// Resolve reports why ref, written inside owner, does not resolve to something
// the Bundle carries — "" when it does. The rules are FHIR R4's: a "#id" names a
// resource contained in its owner, a relative reference resolves only against a
// RESTful owner fullUrl, an absolute one must be an entry's identity, and a
// versioned one must name the version the entry actually holds.
func (g *Graph) Resolve(ref string, owner *Entry, contained map[string]map[string]any, inContained bool) string {
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
		return "names no resource contained in " + owner.Label()
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
		base, err := url.Parse(owner.FullURL)
		if err != nil || base.RawPath != "" || strings.HasPrefix(ref, "/") {
			return "is relative, and " + owner.Label() + " has no RESTful fullUrl to resolve it against"
		}
		if base.Scheme == "urn" {
			resolved, why := g.resolveUnderURN(ref, owner)
			if why != "" {
				return why
			}
			full = resolved
		} else {
			if base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
				return "is relative, and " + owner.Label() + " has no RESTful fullUrl to resolve it against"
			}
			typ := owner.Resource["resourceType"].(string)
			id, ok := owner.Resource["id"].(string)
			suffix := "/" + typ + "/" + id
			if !ok || !strings.HasSuffix(base.Path, suffix) {
				return "is relative, and " + owner.Label() + " has no RESTful fullUrl to resolve it against"
			}
			base.Path = strings.TrimSuffix(base.Path, suffix) + "/" + u.Path
			full = base.String()
		}
	}
	version := ""
	if base, v, versioned := SplitHistory(full); versioned {
		full, version = base, v
		if version == "" {
			return "names no usable version"
		}
	}
	target := g.ByURL[full]
	if target == nil {
		if full != ref {
			return fmt.Sprintf("resolves to %s, which is no entry of the Bundle", full)
		}
		return "is no entry of the Bundle"
	}
	if version != "" {
		meta, ok := target.Resource["meta"].(map[string]any)
		if !ok || meta["versionId"] != version {
			return fmt.Sprintf("names version %s, which %s does not hold", version, target.Label())
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
func (g *Graph) resolveUnderURN(ref string, owner *Entry) (string, string) {
	where := "is relative under " + owner.Label() + ", which is identified by a URN, and "
	trimmed, version := ref, ""
	if base, v, versioned := SplitHistory(ref); versioned {
		trimmed, version = base, "/_history/"+v
	}
	parts := strings.Split(trimmed, "/")
	if len(parts) != 2 || parts[0] == "" || strings.Contains(parts[0], ":") || !SafeResourceID(parts[1]) {
		return "", where + "is not of the form [type]/[id]"
	}
	// Whole path segments: "/Patient/12" is not the end of ".../Patient/112", and
	// "/Person/p" is not the end of ".../RelatedPerson/p". The match reads the
	// decoded path while identities are kept as the payer's raw strings; a
	// percent-encoded id can therefore match here and be named raw, never the
	// other way round, and a collision is refused as ambiguous.
	suffix := "/" + trimmed
	var matches []string
	for _, e := range g.Ordered() {
		u, err := url.Parse(e.FullURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			continue
		}
		if strings.HasSuffix(u.Path, suffix) {
			matches = append(matches, e.FullURL)
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

// ResourceTypeName is whether typ is shaped like a FHIR resource type name:
// letters only, so that no "Foo/Patient" can make an identity end in a segment
// it does not have.
func ResourceTypeName(typ string) bool {
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

// DecodeObject decodes without float conversion or last-key-wins ambiguity. The same bounded
// parser protects both the retained response and the polled replacement.
func DecodeObject(raw []byte, dst *map[string]any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	v, err := decodePASValue(d, 0)
	var c *CauseError
	if errors.As(err, &c) {
		return err
	}
	if err != nil {
		return Refuse(RefusalUndecodable)
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return Refuse(RefusalUndecodable)
	}
	if _, err = d.Token(); err != io.EOF {
		return Refuse(RefusalUndecodable)
	}
	*dst = obj
	return nil
}
func decodePASValue(d *json.Decoder, depth int) (any, error) {
	if depth > MaxDepth {
		return nil, Refuse(RefusalBound)
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
				return nil, Refuse(RefusalUndecodable)
			}
			if _, exists := obj[s]; exists {
				return nil, Refuse(RefusalUndecodable)
			}
			child, err := decodePASValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			obj[s] = child
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return nil, Refuse(RefusalUndecodable)
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
			return nil, Refuse(RefusalUndecodable)
		}
		return arr, nil
	default:
		if _, ok := t.(json.Delim); ok {
			return nil, Refuse(RefusalUndecodable)
		}
		return t, nil
	}
}

// Ordered is every identity the graph holds, in Bundle order (then by fullUrl,
// for a graph built without indexes). Derived from byURL rather than kept beside
// it, so a graph assembled by any constructor is walked whole.
func (g *Graph) Ordered() []*Entry {
	out := make([]*Entry, 0, len(g.ByURL))
	for _, e := range g.ByURL {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Index != out[j].Index {
			return out[i].Index < out[j].Index
		}
		return out[i].FullURL < out[j].FullURL
	})
	return out
}

// SortedKeys is the object's member names in a fixed order: the walk visits
// them so, and the first refusal it reports is therefore the same for the same
// bytes on every run.
func SortedKeys(x map[string]any) []string {
	keys := make([]string, 0, len(x))
	for k := range x {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// SafeResourceID is whether id is a usable FHIR resource id: 1 to 64 of
// [A-Za-z0-9-.], and neither "." nor "..".
func SafeResourceID(id string) bool {
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
