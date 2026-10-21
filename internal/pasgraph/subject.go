package pasgraph

import (
	"net/url"
	"strings"
)

// ReferencePath is the JSON Pointer of member key under parent, as a
// refusal names where a reference sits ("/entry/0/resource" style, with "~"
// and "/" escaped).
func ReferencePath(parent, key string) string {
	return parent + "/" + strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}

// StripHistoryRef and SplitHistory are a copy of the engine's
// history-reference readers (stripHistoryRef and splitHistory in
// engine/fhirref.go), for the PAS graph's own readers. The engine keeps its
// copy in core, because the core's routing coverage read uses it too.
// engine/historyref_copy_test.go fences the two copies: they must read every
// reference the same way.

// StripHistoryRef drops a `/_history/<version>` suffix: a version-specific
// reference names the same resource as the version-less one.
func StripHistoryRef(ref string) string {
	base, _, _ := SplitHistory(ref)
	return base
}

// SplitHistory reads a trailing `/_history/<version>` off ref: its last
// segment pair, with a version that is one segment. A base whose path
// contains `/_history/` elsewhere is part of the reference, never its version,
// so "https://host/_history/fhir/Patient/p" is unversioned.
func SplitHistory(ref string) (base, version string, versioned bool) {
	i := strings.LastIndex(ref, "/_history/")
	if i < 0 || strings.Contains(ref[i+len("/_history/"):], "/") {
		return ref, "", false
	}
	return ref[:i], ref[i+len("/_history/"):], true
}

// SameSubject reports whether identity is the patient expected names. An
// identity is that patient when it is the same identity; and, for an answer
// whose ClaimResponse names its patient by a reference no entry resolves
// (OpenSubjects), when it is that patient's Patient/<id>.
//
// The identities bound by a Patient/<id> alone must be one identity: the first
// so bound is the patient's, and another (the same id at another base) is
// another patient.
func (g *Graph) SameSubject(identity, expected string) bool {
	if identity == expected {
		return true
	}
	if !g.OpenSubjects || !strings.HasPrefix(expected, "Patient/") || PatientTail(identity) != expected {
		return false
	}
	if g.tailIdentity == "" {
		g.tailIdentity = identity
	}
	return identity == g.tailIdentity
}

// NamesNoEntry reports whether no entry of the Bundle has a RESTful identity
// ending in tail ("Patient/<id>"): a reference by it resolves to nothing, as
// opposed to one that resolves to more than one entry, which is refused at
// every line.
func (g *Graph) NamesNoEntry(tail string) bool {
	for url := range g.ByURL {
		if strings.HasSuffix(url, "/"+tail) {
			return false
		}
	}
	return true
}

// PatientTail is the patient a literal reference names by its last
// Patient/<id>, without a trailing version: "Patient/<id>", or "" when it
// names no Patient.
func PatientTail(ref string) string {
	ref = StripHistoryRef(ref)
	i := strings.LastIndex(ref, "Patient/")
	if i < 0 || (i > 0 && ref[i-1] != '/') {
		return ""
	}
	// A relative reference names a Patient only as "Patient/<id>": in
	// "Group/Patient/x" the segment before the id is no Patient type.
	if u, err := url.Parse(ref); err == nil && !u.IsAbs() && i != 0 {
		return ""
	}
	id := ref[i+len("Patient/"):]
	if id == "" || strings.ContainsAny(id, "/#?") {
		return ""
	}
	return "Patient/" + id
}

// SubjectIdentity is the versionless identity a subject reference denotes when
// written inside owner, or why it denotes none: "#" and "#id" name the owner
// and its contained resource; a relative reference resolves against a RESTful
// owner's base (FHIR R4) or, under an owner identified by a URN, to the one
// entry whose RESTful identity ends in it (resolveUnderURN).
func (g *Graph) SubjectIdentity(owner *Entry, ref string) (string, string) {
	if ref == "#" {
		return owner.FullURL, ""
	}
	if strings.HasPrefix(ref, "#") {
		return owner.FullURL + ref, ""
	}
	u, err := url.Parse(ref)
	if err != nil {
		return "", "is not a resolvable resource identity"
	}
	if !u.IsAbs() {
		base, err := url.Parse(owner.FullURL)
		if err != nil {
			return "", "is relative, and " + owner.Label() + " has no fullUrl to resolve it against"
		}
		if base.Scheme == "urn" {
			resolved, why := g.resolveUnderURN(ref, owner)
			if why != "" {
				return "", why
			}
			ref = resolved
		} else {
			typ, _ := owner.Resource["resourceType"].(string)
			id, ok := owner.Resource["id"].(string)
			if !ok || (base.Scheme != "http" && base.Scheme != "https") {
				return "", "is relative, and " + owner.Label() + " has no RESTful fullUrl to resolve it against"
			}
			base.Path = strings.TrimSuffix(base.Path, "/"+typ+"/"+id) + "/" + u.Path
			ref = base.String()
		}
	}
	return StripHistoryRef(ref), ""
}
