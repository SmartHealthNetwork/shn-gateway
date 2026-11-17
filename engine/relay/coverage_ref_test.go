package relay

import (
	"slices"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay/internal/splice"
)

// coverageEach, hitsEach and refersTo are the signed-content coverage as it
// was before it was indexed: every Provenance target compared with every
// resource (refersTo), and every effect with every covered region. They are
// the reference the indexed coverage is held to, kept verbatim but for how a
// Signature element is found, which both read from signatureElements (the
// rule it replaced, by members' names, is shapeSignatures), and how a
// Bundle's or a Provenance's own signature is found, which both read from
// holdsOwnSignature, and a contained Provenance's target "#", which names
// the value whose contained array holds it, the resource enclosing that,
// and so on up while that resource is itself contained, and the names of
// the members that place a resource and resolve a target, which both read
// in any case (memberAnyCase), each object's members scanned here, and a
// resourceType, which both read both ways (byNode exactly, placed in any
// case).

func coverageEach(d *splice.Doc) []coveredRegion {
	parent := parents(d)
	byNode := map[splice.NodeID]*resource{}
	placed := map[splice.NodeID]*resource{}
	var all []*resource
	for i := 0; i < d.Len(); i++ {
		n := splice.NodeID(i)
		if d.Kind(n) != splice.KindObject {
			continue
		}
		typ, exact := stringMember(d, n, "resourceType"), true
		if typ == "" {
			typ, exact = stringMemberAnyCase(d, n, "resourceType"), false
		}
		if typ == "" {
			continue
		}
		r := &resource{node: n, typ: typ, id: stringMemberAnyCase(d, n, "id")}
		if p := parent[n]; p >= 0 {
			if v, ok := memberAnyCase(d, p, "resource"); ok && v == n {
				r.fullURL = stringMemberAnyCase(d, p, "fullUrl")
			}
			if d.Kind(p) == splice.KindArray {
				if pp := parent[p]; pp >= 0 {
					if v, ok := memberAnyCase(d, pp, "contained"); ok && v == p {
						r.contained = true
					}
				}
			}
		}
		if exact {
			byNode[n] = r
		}
		placed[n] = r
		all = append(all, r)
	}
	var c []coveredRegion
	add := func(n splice.NodeID, carrier string) {
		s, e := d.Span(n)
		c = append(c, coveredRegion{start: s, end: e, carrier: carrier})
	}
	signature := signatureElements(d, byNode)
	for i := 0; i < d.Len(); i++ {
		n := splice.NodeID(i)
		if d.Kind(n) != splice.KindObject {
			continue
		}
		if r := placed[n]; r != nil && holdsOwnSignature(d, n, r.typ) {
			switch r.typ {
			case "Bundle":
				add(n, "Bundle.signature")
			case "Provenance":
				add(n, "Provenance.signature")
				targets, _ := memberAnyCase(d, n, "target")
				for _, t := range d.Elems(targets) {
					ref := stringMemberAnyCase(d, t, "reference")
					if ref == "" {
						continue
					}
					if ref == "#" {
						// The holder of each contained array up from the
						// Provenance, and the resource read exactly
						// enclosing it, while the resource reached, or one
						// read either way on the way to it, is itself
						// contained.
						seen := map[splice.NodeID]bool{}
						for work := []splice.NodeID{n}; len(work) > 0; {
							at := work[len(work)-1]
							work = work[:len(work)-1]
							if seen[at] {
								continue
							}
							seen[at] = true
							p := parent[at]
							if p < 0 || d.Kind(p) != splice.KindArray || parent[p] < 0 {
								continue
							}
							if v, ok := memberAnyCase(d, parent[p], "contained"); !ok || v != p {
								continue
							}
							add(parent[p], "Provenance.signature")
							for enc := parent[p]; enc >= 0; enc = parent[enc] {
								if placed[enc] != nil {
									work = append(work, enc)
								}
								if byNode[enc] != nil {
									add(enc, "Provenance.signature")
									break
								}
							}
						}
						continue
					}
					for _, cand := range all {
						if refersTo(ref, cand) {
							add(cand.node, "Provenance.signature")
						}
					}
				}
			}
		}
		if signature[n] {
			enc := parent[n]
			for enc >= 0 && byNode[enc] == nil {
				enc = parent[enc]
			}
			if enc >= 0 && r4ResourceTypes[byNode[enc].typ] {
				add(enc, "Signature in "+byNode[enc].typ)
			} else if enc >= 0 {
				// A resource type that is no FHIR R4 resource type is
				// the payload's own text: the carrier does not repeat it.
				add(enc, "Signature")
			} else {
				add(d.Root(), "Signature")
			}
		}
	}
	return c
}

func hitsEach(c []coveredRegion, e effect) []string {
	var out []string
	for _, r := range c {
		touched := e.start < r.end && r.start < e.end
		if e.inside {
			touched = r.start <= e.start && e.end <= r.end
		}
		if touched && !slices.Contains(out, r.carrier) {
			out = append(out, r.carrier)
		}
	}
	return out
}

// refersTo reports whether a reference may name r. It errs toward a match:
// a wider match only refuses more edits.
func refersTo(ref string, r *resource) bool {
	if strings.HasPrefix(ref, "#") {
		return r.contained && len(ref) > 1 && r.id == ref[1:]
	}
	ref = stripHistory(ref)
	if r.fullURL != "" && stripHistory(r.fullURL) == ref {
		return true
	}
	segs := strings.Split(ref, "/")
	if len(segs) < 2 {
		return false
	}
	typ, id := segs[len(segs)-2], segs[len(segs)-1]
	if r.typ != typ || id == "" {
		return false
	}
	// By type and id: the resource's own id, or, for a resource that
	// carries none, the tail of its fullUrl.
	return r.id == id || strings.HasSuffix(stripHistory(r.fullURL), "/"+typ+"/"+id)
}

// shapeSignatures is how the signed-content check found a Signature element
// before it read element types: any object with type, when (or _when) and
// who, at any depth, but a Bundle's or a Provenance's own signature. It is
// the reference signatureElements is held to: on real resources both find
// the same Signatures, and only an object merely shaped as one, at a place
// whose type is not Signature, drops out.
func shapeSignatures(d *splice.Doc, byNode map[splice.NodeID]*resource) []bool {
	parent := parents(d)
	sig := make([]bool, d.Len())
	for i := 0; i < d.Len(); i++ {
		n := splice.NodeID(i)
		if d.Kind(n) == splice.KindObject && hasMember(d, n, "type") && (hasMember(d, n, "when") || hasMember(d, n, "_when")) && hasMember(d, n, "who") && !ownSignature(d, parent, byNode, n) {
			sig[n] = true
		}
	}
	return sig
}

// ownSignature reports whether the Signature element n is the signature
// member of a Bundle or a Provenance (directly, or as an element of it).
func ownSignature(d *splice.Doc, parent []splice.NodeID, byNode map[splice.NodeID]*resource, n splice.NodeID) bool {
	holder := parent[n]
	if holder >= 0 && d.Kind(holder) == splice.KindArray {
		holder = parent[holder]
	}
	r := byNode[holder]
	if holder < 0 || r == nil || (r.typ != "Bundle" && r.typ != "Provenance") {
		return false
	}
	sig, ok := d.Member(holder, "signature")
	return ok && (sig == n || sig == parent[n])
}
