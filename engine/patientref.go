package engine

// patientref.go — a patient reference read the same way in every form FHIR
// allows. A relative ("Patient/<id>") or absolute ("<base>/Patient/<id>", with
// or without "/_history/<version>") reference names the member by its Patient
// id; a contained one ("#<id>") names the contained Patient, whose member is
// its member identifier (its id is local to the resource carrying it). Only
// a well-formed reference names a patient: anything else (a trailing path, an
// operation, an escaped character, a reference that resolves to no patient or
// to more than one) names none, and the caller refuses it as it refused it
// before.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A FHIR id: letters, digits, "-" and ".", at most 64.
const fhirID = `[A-Za-z0-9\-.]{1,64}`

// patientRefShape is a well-formed relative or absolute Patient reference: an
// optional http(s) base of plain path segments, then Patient/<id>, then an
// optional /_history/<version>, and nothing else.
var patientRefShape = regexp.MustCompile(`^(?:https?://[A-Za-z0-9.\-]+(?::[0-9]{1,5})?(?:/[A-Za-z0-9._~\-]+)*/)?Patient/(` + fhirID + `)(?:/_history/` + fhirID + `)?$`)

// localRefShape is a well-formed contained reference.
var localRefShape = regexp.MustCompile(`^#(` + fhirID + `)$`)

// fhirIDOnly is a FHIR id alone.
var fhirIDOnly = regexp.MustCompile(`^` + fhirID + `$`)

// readPatientReference reads a literal reference by FHIR's reference grammar,
// by its structure, never by finding "Patient/" in its text. It names
// a Patient when it is the relative "Patient/<id>", or an http or https URL
// (a host, no user, query or fragment, no empty path segment) whose path ends
// in "Patient/<id>", either with an optional trailing "/_history/<version>".
// ok is false for anything else: another type, another number of segments, an
// empty id or version, an operation ("$<name>") where the id goes, a URN or
// another scheme, a "#" (a contained reference, which its caller resolves), a
// query, an escaped character in the id or version (a "%" escape can hide a
// "/", so the reference would read two ways; a base's own segments may carry
// one), or a URL that does not parse. Such a reference names no Patient.
//
// id is the Patient id as written. conforms reports whether it, and the
// version when there is one, are FHIR ids: an id that names one Patient by
// structure but is not a FHIR id (an underscore, a space, more than 64
// characters) still names that Patient, and whether to refuse it is the
// participant's conformance choice, never the network's.
func readPatientReference(ref string) (id string, conforms, ok bool) {
	typ, id, conforms, ok := readTypedReference(ref)
	if !ok || typ != "Patient" {
		return "", false, false
	}
	return id, conforms, true
}

// resourceTypeName is a FHIR resource type's name, as a reference's type
// segment spells it.
var resourceTypeName = regexp.MustCompile(`^[A-Z][A-Za-z]{0,63}$`)

// readTypedReference reads a literal reference to a resource of any type by
// the grammar readPatientReference reads a Patient's: typ is its type segment
// (a resource type's name), id its id as written, and conforms and ok as there.
func readTypedReference(ref string) (typ, id string, conforms, ok bool) {
	if ref == "" || strings.ContainsAny(ref, "#?") {
		return "", "", false, false
	}
	path := ref
	absolute := false
	if i := strings.IndexByte(ref, ':'); i >= 0 && !strings.Contains(ref[:i], "/") {
		// A scheme: only an http or https URL is read.
		u, err := url.Parse(ref)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" {
			return "", "", false, false
		}
		// The path as sent, taken from the reference's own text after its
		// authority, never a path rebuilt from the decoded one, where an
		// escaped "/" would read as a separator. A base may carry escapes.
		rest, found := strings.CutPrefix(ref[i+1:], "//")
		j := strings.IndexByte(rest, '/')
		if !found || j < 0 {
			return "", "", false, false
		}
		path, absolute = rest[j+1:], true
	}
	seg := strings.Split(path, "/")
	version := ""
	if n := len(seg); n >= 4 && seg[n-2] == "_history" {
		version, seg = seg[n-1], seg[:n-2]
		if version == "" {
			return "", "", false, false
		}
	}
	n := len(seg)
	if n < 2 || (!absolute && n != 2) || !resourceTypeName.MatchString(seg[n-2]) || seg[n-1] == "" || strings.HasPrefix(seg[n-1], "$") {
		return "", "", false, false
	}
	for _, s := range seg {
		if s == "" {
			return "", "", false, false
		}
	}
	// The segments that name the Patient carry no escape: one can hide a "/",
	// so the reference would read two ways. A base's own segments may.
	if strings.Contains(seg[n-1], "%") || strings.Contains(version, "%") {
		return "", "", false, false
	}
	id = seg[n-1]
	conforms = fhirIDOnly.MatchString(id) && (version == "" || fhirIDOnly.MatchString(version))
	return seg[n-2], id, conforms, true
}

// patientRefCompared is how a patient-consistency check reads a literal
// reference: the Patient id it names by FHIR's reference grammar
// (readPatientReference), else, for a reference to a patient that names no
// Patient so (patientLike), the reference as written, which is no member the
// request binds. names is false for any other reference (another type, a URN,
// a "#<id>"), which the check resolves or leaves to its own type.
func patientRefCompared(ref string) (member string, names bool) {
	if id, _, ok := readPatientReference(ref); ok {
		return id, true
	}
	if patientLike(ref) {
		return ref, true
	}
	return "", false
}

// patientLike reports whether a reference carries a "Patient" segment, its
// segments split at "/", ":" and "#" and read without surrounding white
// space, as written or with its escapes decoded: a reference to a patient,
// whether or not it names one by FHIR's reference grammar. A "#<id>" and a
// well-formed reference to another type (readTypedReference) are not.
func patientLike(ref string) bool {
	if contained := strings.TrimSpace(ref); strings.HasPrefix(contained, "#") && len(contained) > 1 && !strings.ContainsAny(contained[1:], "/:#") {
		// A "#<id>" names a contained resource by its id, which its caller
		// resolves: never a reference to a patient by its text.
		return false
	}
	if typ, _, _, ok := readTypedReference(ref); ok && typ != "Patient" {
		// A well-formed reference to another type names that type, whatever
		// its base or id spells.
		return false
	}
	if patientSegment(ref) {
		return true
	}
	decoded, err := url.PathUnescape(ref)
	return err == nil && decoded != ref && patientSegment(decoded)
}

// patientSegment reports a "Patient" segment of ref (patientLike).
func patientSegment(ref string) bool {
	for _, s := range strings.FieldsFunc(ref, func(r rune) bool { return r == '/' || r == ':' || r == '#' }) {
		if strings.TrimSpace(s) == "Patient" {
			return true
		}
	}
	return false
}

// patientRefMember returns the member a patient reference names, among the
// resource's contained resources; ok is false when it names none. A "#<id>"
// names the one contained Patient with that id, by its member identifier;
// any other reference is read by FHIR's reference grammar
// (readPatientReference), and names the Patient id it writes, whether or not
// that id is a FHIR id (whether to refuse one that is not is the
// participant's conformance choice, never the network's).
func patientRefMember(ref string, contained []map[string]any) (string, bool) {
	if m := localRefShape.FindStringSubmatch(ref); m != nil {
		id := m[1]
		var found map[string]any
		for _, c := range contained {
			if cid, _ := c["id"].(string); cid != id {
				continue
			}
			if found != nil {
				return "", false // one id naming two contained resources
			}
			found = c
		}
		if found == nil || found["resourceType"] != "Patient" {
			return "", false
		}
		return containedPatientMember(found)
	}
	if id, _, ok := readPatientReference(ref); ok {
		return id, true
	}
	return "", false
}

// containedPatientMember is a contained Patient's member: its member
// identifier, which it must carry exactly once (a contained resource's id is
// local to the resource that carries it, never a member id). The value is an
// identifier's, not limited to a FHIR id's shape.
func containedPatientMember(patient map[string]any) (string, bool) {
	member := ""
	ids, _ := patient["identifier"].([]any)
	for _, id := range ids {
		m, ok := id.(map[string]any)
		if !ok || m["system"] != shnsdk.MemberSystem {
			continue
		}
		v, _ := m["value"].(string)
		if v == "" {
			continue // a blank value names no one
		}
		if member != "" && v != member {
			return "", false // two members: more than one patient
		}
		member = v
	}
	return member, member != ""
}

// containedObjects is a contained list's resources: its elements that are
// JSON objects. Anything else in it names no resource, and reading the
// request does not depend on it.
func containedObjects(list []any) []map[string]any {
	var out []map[string]any
	for _, c := range list {
		if m, ok := c.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// idShape is a FHIR id's characters only: its uses build an id or a relative
// reference for a reading the gateway makes in memory, never a path or search it
// sends, which take fhirid.Valid.
var idShape = regexp.MustCompile(`^` + fhirID + `$`)

// eligibilityRequestMember is the member a CoverageEligibilityRequest's
// patient names, in any reference form. The SDK's reader reads the request
// first, so a request it refuses is refused as before; the request is then
// read by its exact member names, and its patient reference resolved in its
// well-formed forms. A reference that is neither well-formed nor contained
// names no one (errMemberNamesNoOne), never a member read from its text. A request the strict reader refuses for its own
// limits (not well-formed UTF-8 or a lone surrogate escape, or past its depth
// or token bound) is read so too when it reads one way only: it repeats no
// member name and names its patient's reference, and the contained resources
// a "#id" reference names, exactly. An error
// is a request the SDK's reader refuses, one this gateway cannot read one way
// only (a repeated member name, or a member named only in another case: a
// reader taking names exactly finds another patient, or none), or a contained
// reference that names no contained Patient or more than one (contained
// resources of another JSON type hold none), or, in a body past the strict
// reader's limits, a member value read from a byte or escape a reader replaces
// (errReplacedCharacter).
func eligibilityRequestMember(cer []byte) (string, error) {
	// A patient reference that names no one is refused before the SDK's
	// reader reads it, whatever that reader answers for it.
	if blankPatientReference(cer) {
		return "", errMemberNamesNoOne
	}
	literal, err := shnsdk.ParseEligibilityRequestMember(cer)
	// From shn-sdk v1.15.0 the SDK's reader refuses a member that names no one
	// (its only refusal, an *EligibilityRequestRefusal). A reference read so by
	// names in any case may name its patient only in another case: that is
	// read below, by exact names, and refused for it first, as before. Any
	// other blank member is refused after (eligibilitySubmissionMember).
	var namesNoOne *shnsdk.EligibilityRequestRefusal
	blankRead := errors.As(err, &namesNoOne)
	if blankRead {
		literal, err = "", nil
	}
	if err != nil {
		return "", err
	}
	var req struct {
		Patient struct {
			Reference string `json:"reference"`
		} `json:"patient"`
	}
	if err := decodeMessage(cer, &req); err != nil {
		if readsTwoWays(err) || repeatsAName(cer) {
			return "", fmt.Errorf("the request's patient cannot be read one way only: %w", err)
		}
		ref, ok := exactPatientReference(cer)
		// Read so by names in any case, the member was blank: the exact
		// reference reads as the SDK's reading did when it names no one too
		// (whatever whitespace it carries), as a reader that returns the
		// blank member reads it.
		if blankRead && ok && memberNamesNoOne(strings.TrimPrefix(ref, "Patient/")) {
			literal = strings.TrimPrefix(ref, "Patient/")
		}
		if !ok || strings.TrimPrefix(ref, "Patient/") != literal {
			return "", fmt.Errorf("the request's patient: %w", errNamedInAnotherCase)
		}
		// A body that repeats no member name reads one way by its exact
		// member names, so a "#id" reference resolves through the contained
		// resources so named, as it does in a body the strict reader reads.
		contained, err := exactContainedFor(cer, ref)
		if errors.Is(err, errNamedInAnotherCase) {
			return "", fmt.Errorf("the request's patient: %w", err)
		}
		member, err := containedRequestMember(ref, contained)
		// This reader replaces each byte of not-well-formed UTF-8, and each lone surrogate
		// escape, with U+FFFD: a member value read from such a byte or escape may have been
		// sent as any of several values another reader keeps apart. One carrying U+FFFD as
		// sent is read one way, wherever else the body is not well-formed.
		if err == nil && strings.ContainsRune(member, utf8.RuneError) && memberValueReplaced(cer, ref) {
			return "", fmt.Errorf("the request's patient: %w", errReplacedCharacter)
		}
		return member, err
	}
	ref := req.Patient.Reference
	contained, err := eligibilityContainedFor(cer, ref)
	if err != nil && readsTwoWays(err) {
		return "", fmt.Errorf("the request's patient cannot be read one way only: %w", err)
	}
	// Contained resources of another JSON type hold no contained Patient.
	return containedRequestMember(ref, contained)
}

// blankPatientReference reports whether a CoverageEligibilityRequest, read by
// its exact member names, names its patient by a reference that names no one
// (memberNamesNoOne). A body that does not read so is left to the readers that
// follow.
func blankPatientReference(cer []byte) bool {
	var req struct {
		ResourceType string `json:"resourceType"`
		Patient      *struct {
			Reference *string `json:"reference"`
		} `json:"patient"`
	}
	if decodeMessage(cer, &req) != nil || req.ResourceType != "CoverageEligibilityRequest" || req.Patient == nil || req.Patient.Reference == nil {
		return false
	}
	// One "Patient/" taken off, as the SDK's reader takes it, then the member
	// that leaves read as the engine reads every member.
	return memberNamesNoOne(strings.TrimPrefix(*req.Patient.Reference, "Patient/"))
}

// errReplacedCharacter is a request whose member value carries U+FFFD in a body that is not
// well-formed UTF-8 or carries a lone surrogate escape: the reader that reads such a body
// replaces each such byte or escape with U+FFFD, so values that differ on the wire would
// bind as one member, and another reader keeps them apart.
var errReplacedCharacter = errors.New("a member value carries a replaced character")

// memberValueReplaced reports whether the bytes a request's member value is read from carry
// a character a reader replaces, read as the member is read, by exact member names: its
// patient reference ref's own, or, for a "#id" reference, the non-blank member identifier
// values of the contained Patient it names. Anything the member is not read from (another
// field, a value of another JSON type) is not looked at.
func memberValueReplaced(cer []byte, ref string) bool {
	object := func(raw json.RawMessage) map[string]json.RawMessage {
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil {
			return nil
		}
		return m
	}
	list := func(raw json.RawMessage) []json.RawMessage {
		var l []json.RawMessage
		if json.Unmarshal(raw, &l) != nil {
			return nil
		}
		return l
	}
	str := func(raw json.RawMessage) (string, bool) {
		var v string
		return v, len(raw) > 0 && raw[0] == '"' && json.Unmarshal(raw, &v) == nil
	}
	req := object(cer)
	id, local := strings.CutPrefix(ref, "#")
	if !local {
		return replacesCharacters(object(req["patient"])["reference"])
	}
	for _, raw := range list(req["contained"]) {
		patient := object(raw)
		if rt, _ := str(patient["resourceType"]); rt != "Patient" {
			continue
		}
		if pid, _ := str(patient["id"]); pid != id {
			continue
		}
		for _, x := range list(patient["identifier"]) {
			identifier := object(x)
			if system, _ := str(identifier["system"]); system != shnsdk.MemberSystem {
				continue
			}
			if v, ok := str(identifier["value"]); ok && v != "" && replacesCharacters(identifier["value"]) {
				return true
			}
		}
	}
	return false
}

// replacesCharacters reports whether body is one a reader replaces characters of: not
// well-formed UTF-8, or carrying a \u escape of a lone surrogate.
func replacesCharacters(body []byte) bool {
	if !utf8.Valid(body) {
		return true
	}
	surrogate := func(i int) (rune, bool) {
		if i+6 > len(body) || body[i] != '\\' || body[i+1] != 'u' {
			return 0, false
		}
		v, err := strconv.ParseUint(string(body[i+2:i+6]), 16, 16)
		return rune(v), err == nil
	}
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' {
			continue
		}
		r, ok := surrogate(i)
		if !ok {
			i++ // another escape: skip its character, so "\\u…" is read as written
			continue
		}
		switch {
		case r >= 0xD800 && r <= 0xDBFF:
			if lo, ok := surrogate(i + 6); ok && lo >= 0xDC00 && lo <= 0xDFFF {
				i += 11
				continue
			}
			return true
		case r >= 0xDC00 && r <= 0xDFFF:
			return true
		}
		i += 5
	}
	return false
}

// containedRequestMember is the member a request's patient reference ref
// names, with contained its contained resources read by their exact member
// names.
func containedRequestMember(ref string, contained []any) (string, error) {
	if member, ok := patientRefMember(ref, containedObjects(contained)); ok {
		return member, nil
	}
	// A contained reference that names no contained Patient, or more than
	// one, names no patient. Any other reference names no Patient by FHIR's
	// reference grammar ("urn:Patient/<id>", "Group/1", "Patient/1/x", a bare
	// id): it names no one, and the binding refuses it on authority, never a
	// member read from its text.
	if strings.HasPrefix(ref, "#") {
		return "", fmt.Errorf("patient reference %q names no patient", ref)
	}
	return "", errMemberNamesNoOne
}

// eligibilityContainedFor is a request's contained resources, read by their
// exact member name, when its patient reference ref is a "#id" one: only that
// reference is read through them. Any other reference reads none, so how the
// request names its contained resources does not change which patient it
// names.
func eligibilityContainedFor(request []byte, ref string) ([]any, error) {
	if !strings.HasPrefix(ref, "#") {
		return nil, nil
	}
	var c struct {
		Contained []any `json:"contained"`
	}
	if err := decodeMessage(request, &c); err != nil {
		return nil, err
	}
	return c.Contained, nil
}

// exactContainedFor is eligibilityContainedFor for a body the strict reader
// refuses for its own limits: the contained resources read by their exact
// member names, as encoding/json reads the body into maps. The caller has
// found no repeated member name, so the reading is the body's one reading.
// Contained resources named only in another case are errNamedInAnotherCase,
// as the strict reader refuses them; ones of another JSON type are none.
func exactContainedFor(request []byte, ref string) ([]any, error) {
	if !strings.HasPrefix(ref, "#") {
		return nil, nil
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(request, &top) != nil {
		return nil, nil
	}
	raw, ok := top["contained"]
	if !ok {
		for name := range top {
			if strings.EqualFold(name, "contained") {
				return nil, errNamedInAnotherCase
			}
		}
		return nil, nil
	}
	var contained []any
	if json.Unmarshal(raw, &contained) != nil {
		return nil, nil
	}
	return contained, nil
}

// exactPatientReference is a request's patient reference read by its exact
// member names, as encoding/json reads the body. ok is false when the body
// names none so.
func exactPatientReference(cer []byte) (ref string, ok bool) {
	var top, patient map[string]json.RawMessage
	if json.Unmarshal(cer, &top) != nil || json.Unmarshal(top["patient"], &patient) != nil || json.Unmarshal(patient["reference"], &ref) != nil {
		return "", false
	}
	return ref, true
}
