package relay

import (
	"slices"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay/internal/splice"
)

// coverageEach, hitsEach and refersTo are the signed-content coverage as it
// was before it was indexed: every Provenance target compared with every
// resource (refersTo), and every effect with every covered region. They are
// the reference the indexed coverage is held to, kept verbatim.

func coverageEach(d *splice.Doc) []coveredRegion {
	parent := parents(d)
	byNode := map[splice.NodeID]*resource{}
	var all []*resource
	for i := 0; i < d.Len(); i++ {
		n := splice.NodeID(i)
		if d.Kind(n) != splice.KindObject {
			continue
		}
		typ := stringMember(d, n, "resourceType")
		if typ == "" {
			continue
		}
		r := &resource{node: n, typ: typ, id: stringMember(d, n, "id")}
		if p := parent[n]; p >= 0 {
			if v, ok := d.Member(p, "resource"); ok && v == n {
				r.fullURL = stringMember(d, p, "fullUrl")
			}
			if d.Kind(p) == splice.KindArray {
				if pp := parent[p]; pp >= 0 {
					if v, ok := d.Member(pp, "contained"); ok && v == p {
						r.contained = true
					}
				}
			}
		}
		byNode[n] = r
		all = append(all, r)
	}
	var c []coveredRegion
	add := func(n splice.NodeID, carrier string) {
		s, e := d.Span(n)
		c = append(c, coveredRegion{start: s, end: e, carrier: carrier})
	}
	for i := 0; i < d.Len(); i++ {
		n := splice.NodeID(i)
		if d.Kind(n) != splice.KindObject {
			continue
		}
		if r := byNode[n]; r != nil && hasMember(d, n, "signature") {
			switch r.typ {
			case "Bundle":
				add(n, "Bundle.signature")
			case "Provenance":
				add(n, "Provenance.signature")
				targets, _ := d.Member(n, "target")
				for _, t := range d.Elems(targets) {
					ref := stringMember(d, t, "reference")
					if ref == "" {
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
		if hasMember(d, n, "type") && (hasMember(d, n, "when") || hasMember(d, n, "_when")) && hasMember(d, n, "who") && !ownSignature(d, parent, byNode, n) {
			enc := parent[n]
			for enc >= 0 && byNode[enc] == nil {
				enc = parent[enc]
			}
			if enc >= 0 && fhirTypeName(byNode[enc].typ) {
				add(enc, "Signature in "+byNode[enc].typ)
			} else if enc >= 0 {
				// A resource type that is no FHIR type name is the
				// payload's own text: the carrier does not repeat it.
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
