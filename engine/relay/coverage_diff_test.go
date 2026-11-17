package relay

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay/internal/splice"
)

// randomSignedDoc writes a seeded random document in which signatures cover
// what the coverage rules cover: Bundles (signed or not) of resources with
// ids, fullUrls (absolute, relative, with or without a version) and
// contained resources; Provenances (signed or not) whose targets name
// resources by fullUrl, by type and id (with or without a version), by #id,
// by "#" (their container, when they are contained), or nothing in the
// document; Signatures in extensions and Task inputs; and
// Signature-shaped objects at any depth, in members R4 does not define (read
// as Signatures), inside a resource of an R4 type or another, or outside all
// of them.
func randomSignedDoc(r *rand.Rand) string {
	types := []string{"Patient", "Basic", "Task", "Subscription", "Provenance", "x-type", "Bundle"}
	ids := []string{"1", "2", "3", "a", ""}
	sig := func() string {
		when := `"when"`
		if r.IntN(3) == 0 {
			when = `"_when"`
		}
		return `{"type":[{"code":"1"}],` + when + `:"2026-11-14","who":{"reference":"Organization/o"}}`
	}
	ref := func() string {
		typ, id := types[r.IntN(len(types))], ids[r.IntN(len(ids))]
		switch r.IntN(6) {
		case 0:
			return "#" + id
		case 1:
			return "https://x.example/fhir/" + typ + "/" + id
		case 2:
			return typ + "/" + id + "/_history/2"
		case 3:
			return "urn:uuid:" + id
		case 4:
			return "nothing"
		}
		return typ + "/" + id
	}
	var res func(depth int) string
	res = func(depth int) string {
		typ := types[r.IntN(len(types))]
		var m []string
		m = append(m, `"resourceType":"`+typ+`"`)
		if id := ids[r.IntN(len(ids))]; id != "" {
			m = append(m, `"id":"`+id+`"`)
		}
		if typ == "Provenance" || r.IntN(4) == 0 {
			var ts []string
			for range r.IntN(4) {
				ts = append(ts, `{"reference":"`+ref()+`"}`)
			}
			m = append(m, `"target":[`+strings.Join(ts, ",")+`]`)
		}
		if r.IntN(3) == 0 {
			m = append(m, `"signature":[`+sig()+`]`)
		}
		if r.IntN(4) == 0 {
			m = append(m, `"x":`+sig())
		}
		if r.IntN(5) == 0 {
			// An extension or a modifier extension holds a Signature by its
			// type; a member merely named like one, by its shape.
			name := []string{"extension", "modifierExtension", "ext"}[r.IntN(3)]
			m = append(m, `"`+name+`":[{"url":"u","valueSignature":`+sig()+`}]`)
		}
		if typ == "Task" && r.IntN(3) == 0 {
			m = append(m, `"input":[{"type":{},"valueSignature":`+sig()+`}]`)
		}
		if depth < 2 && r.IntN(3) == 0 {
			var cs []string
			for range 1 + r.IntN(2) {
				cs = append(cs, res(depth+1))
			}
			m = append(m, `"contained":[`+strings.Join(cs, ",")+`]`)
		}
		if depth < 2 && typ == "Bundle" {
			var es []string
			for range r.IntN(4) {
				e := `{"resource":` + res(depth+1)
				if r.IntN(2) == 0 {
					e += `,"fullUrl":"https://x.example/fhir/` + types[r.IntN(len(types))] + "/" + ids[r.IntN(len(ids))]
					if r.IntN(3) == 0 {
						e += "/_history/1"
					}
					e += `"`
				}
				es = append(es, e+"}")
			}
			m = append(m, `"entry":[`+strings.Join(es, ",")+`]`)
		}
		r.Shuffle(len(m), func(i, j int) { m[i], m[j] = m[j], m[i] })
		return "{" + strings.Join(m, ",") + "}"
	}
	if r.IntN(6) == 0 {
		return `{"data":[` + res(0) + `,` + sig() + `]}`
	}
	return res(0)
}

// TestCoverageAgreesWithCoverageEach: on seeded random documents, for every
// effect an operation can make (a replaced value, a removed member, an
// insertion inside a container) and for random byte ranges, the indexed
// coverage reports exactly the carriers, in exactly the order, that
// comparing every effect with every region of the unindexed coverage does.
func TestCoverageAgreesWithCoverageEach(t *testing.T) {
	r := rand.New(rand.NewPCG(20261114, 3))
	cases, signed := 0, 0
	for c := range 3000 {
		src := randomSignedDoc(r)
		d, err := splice.Scan([]byte(src), splice.Limits{})
		if err != nil {
			t.Fatalf("case %d: %v\n%s", c, err, src)
		}
		ref := coverageEach(d)
		cov := coverage(d)
		if len(ref) > 0 {
			signed++
		}
		var effects []effect
		for i := 0; i < d.Len(); i++ {
			n := splice.NodeID(i)
			s, e := d.Span(n)
			effects = append(effects, effect{start: s, end: e})
			if k := d.Kind(n); k == splice.KindObject || k == splice.KindArray {
				effects = append(effects, effect{start: s, end: e, inside: true})
			}
			if d.Kind(n) == splice.KindObject {
				for _, mem := range d.Members(n) {
					_, ve := d.Span(mem.Value)
					effects = append(effects, effect{start: mem.KeyStart, end: ve})
				}
			}
		}
		for range 20 {
			s := r.IntN(len(src) + 1)
			e := min(len(src), s+r.IntN(40))
			effects = append(effects, effect{start: s, end: e, inside: r.IntN(2) == 0})
		}
		for _, e := range effects {
			got, want := cov.hits(e), hitsEach(ref, e)
			if !slices.Equal(got, want) {
				t.Fatalf("case %d, effect %+v: indexed %q, each %q\n%s", c, e, got, want, src)
			}
			cases++
		}
	}
	if signed < 1000 {
		t.Fatalf("only %d of the documents hold a signature", signed)
	}
	t.Logf("%d effects compared", cases)
}

// The reference itself is exercised on the shapes the indexed coverage must
// keep: a valueSignature in an extension covers its resource, a
// Provenance's target by each form covers what it names, and a duplicate
// region changes nothing.
func TestCoverageKeepsEachRule(t *testing.T) {
	for name, c := range map[string]struct {
		src     string
		path    []string
		carrier []string
	}{
		"valueSignature in an extension":                     {`{"resourceType":"Patient","extension":[{"url":"u","valueSignature":{"type":[],"when":"x","who":{}}}],"name":"n"}`, []string{"name"}, []string{"Signature in Patient"}},
		"a Provenance target by fullUrl with a version":      {`{"resourceType":"Bundle","entry":[{"fullUrl":"https://x/P/1","resource":{"resourceType":"P","v":1}},{"resource":{"resourceType":"Provenance","signature":[{}],"target":[{"reference":"https://x/P/1/_history/3"}]}}]}`, []string{"entry", "0", "resource", "v"}, []string{"Provenance.signature"}},
		"a Provenance target by type and the fullUrl's tail": {`{"resourceType":"Bundle","entry":[{"fullUrl":"https://x/P/9","resource":{"resourceType":"P","v":1}},{"resource":{"resourceType":"Provenance","signature":[{}],"target":[{"reference":"P/9"}]}}]}`, []string{"entry", "0", "resource", "v"}, []string{"Provenance.signature"}},
		"a Provenance target by #id":                         {`{"resourceType":"Provenance","signature":[{}],"contained":[{"resourceType":"P","id":"c","v":1}],"target":[{"reference":"#c"}]}`, []string{"contained", "0", "v"}, []string{"Provenance.signature"}},
		"many Signatures in one resource":                    {`{"resourceType":"Task","input":[` + strings.Repeat(`{"valueSignature":{"type":0,"when":0,"who":0}},`, 5) + `{"valueSignature":{"type":0,"when":0,"who":0}}],"v":1}`, []string{"v"}, []string{"Signature in Task"}},
		"a signed Bundle and a Signature inside it":          {`{"resourceType":"Bundle","signature":{"type":0,"when":0,"who":0},"entry":[{"resource":{"resourceType":"Task","output":[{"valueSignature":{"type":0,"when":0,"who":0}}],"v":1}}]}`, []string{"entry", "0", "resource", "v"}, []string{"Bundle.signature", "Signature in Task"}},
		"only Signature-shaped, at a place of another type":  {`{"resourceType":"Task","input":[{"type":{"type":0,"when":0,"who":0}}],"v":1}`, []string{"v"}, nil},
		"Signature-shaped, in a member R4 does not define":   {`{"resourceType":"Task","a":[{"type":0,"when":0,"who":0}],"v":1}`, []string{"v"}, []string{"Signature in Task"}},
	} {
		t.Run(name, func(t *testing.T) {
			b := NewBody([]byte(c.src), OriginPeerFrame)
			doc := mustDoc(t, b)
			n := at(t, doc, c.path...)
			s, e := doc.d.Span(n)
			eff := effect{start: s, end: e}
			if got, want := coverage(doc.d).hits(eff), hitsEach(coverageEach(doc.d), eff); !slices.Equal(got, c.carrier) || !slices.Equal(want, c.carrier) {
				t.Fatalf("indexed %q, each %q, want %q", got, want, c.carrier)
			}
		})
	}
}

// randomNested is n regions within [0, size) that nest or lie apart (as
// values' spans do), with repeats of a span under other carriers, in a
// shuffled coverage order.
func randomNested(r *rand.Rand, size, n int) []coveredRegion {
	carriers := []string{"Bundle.signature", "Provenance.signature", "Signature in Task", "Signature"}
	var out []coveredRegion
	var grow func(s, e, depth int)
	grow = func(s, e, depth int) {
		for pos := s; pos < e && len(out) < n; {
			a := pos + r.IntN(4)
			b := min(e, a+1+r.IntN(max(1, (e-s)/2)))
			if a >= b {
				break
			}
			out = append(out, coveredRegion{a, b, carriers[r.IntN(len(carriers))]})
			if r.IntN(4) == 0 {
				out = append(out, coveredRegion{a, b, carriers[r.IntN(len(carriers))]})
			}
			if depth < 6 && b-a > 2 && r.IntN(2) == 0 {
				grow(a+1, b-1, depth+1)
			}
			pos = b
		}
	}
	grow(0, size, 0)
	r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// TestCoverageMapHitsAgreesWithHitsEach: on seeded random region sets that
// nest (the index) and that do not (the fallback), and random effects, the
// indexed hits reports exactly hitsEach's carriers in hitsEach's order.
func TestCoverageMapHitsAgreesWithHitsEach(t *testing.T) {
	r := rand.New(rand.NewPCG(20261114, 4))
	paths := map[bool]int{}
	for c := range 5000 {
		size := 10 + r.IntN(200)
		regions := randomNested(r, size, 1+r.IntN(30))
		if r.IntN(5) == 0 {
			// A region crossing another's edge: no longer nested.
			s := r.IntN(size)
			regions = append(regions, coveredRegion{s, min(size, s+1+r.IntN(20)), "Signature"})
		}
		cov := newCoverageMap(regions)
		paths[cov.nested]++
		for range 30 {
			s := r.IntN(size + 1)
			e := min(size, s+r.IntN(30))
			eff := effect{start: s, end: e, inside: r.IntN(3) == 0}
			if got, want := cov.hits(eff), hitsEach(regions, eff); !slices.Equal(got, want) {
				t.Fatalf("case %d (nested %v), effect %+v: indexed %q, each %q, regions %v", c, cov.nested, eff, got, want, regions)
			}
		}
	}
	if paths[true] < 1000 || paths[false] < 100 {
		t.Fatalf("both ways must be reached: %v", paths)
	}
}

// TestTargetIndexResolvesAsRefersTo: on seeded random resources and
// references built from the pieces refersTo reads (types with and without a
// slash, ids, fullUrls absolute, relative, versioned, with a /_history/ that
// is not a version, or none; contained resources; refs by #id, type and id,
// fullUrl, one segment, an empty last segment), resolve names exactly the
// resources refersTo matches, in their order; and over a run of references
// sharing one index, each names only resources refersTo matches, and once
// each is resolved every resource refersTo matches for it is resolved.
func TestTargetIndexResolvesAsRefersTo(t *testing.T) {
	r := rand.New(rand.NewPCG(20261114, 5))
	types := []string{"Patient", "Task", "A/B", "Basic"}
	ids := []string{"1", "2", "a", ""}
	pick := func(s []string) string { return s[r.IntN(len(s))] }
	fullURL := func() string {
		switch r.IntN(8) {
		case 0:
			return ""
		case 1:
			return "https://x.example/fhir/" + pick(types) + "/" + pick(ids)
		case 2:
			return "https://x.example/fhir/" + pick(types) + "/" + pick(ids) + "/_history/" + pick(ids)
		case 3:
			return pick(types) + "/" + pick(ids)
		case 4:
			return "/" + pick(types) + "/" + pick(ids)
		case 5:
			return "x/_history/1/" + pick(types) + "/" + pick(ids)
		case 6:
			return "urn:uuid:" + pick(ids)
		}
		return "/_history/1"
	}
	ref := func() string {
		switch r.IntN(9) {
		case 0:
			return "#" + pick(ids)
		case 1:
			return "#"
		case 2:
			return pick(types) + "/" + pick(ids) + "/_history/1"
		case 3:
			return pick(ids)
		case 4:
			return pick(types) + "/"
		case 5:
			return "/" + pick(types) + "/" + pick(ids)
		case 6, 7:
			return fullURL()
		}
		return pick(types) + "/" + pick(ids)
	}
	matched, total := 0, 0
	for c := range 20000 {
		var all []*resource
		for i := range 1 + r.IntN(8) {
			all = append(all, &resource{node: splice.NodeID(i), typ: pick(types), id: pick(ids), fullURL: fullURL(), contained: r.IntN(3) == 0})
		}
		want := func(ref string) []int {
			var out []int
			for i, res := range all {
				if refersTo(ref, res) {
					out = append(out, i)
				}
			}
			return out
		}
		// One reference against a fresh index: exactly refersTo's.
		one := ref()
		if got, w := newTargetIndex(all).resolve(one), want(one); !slices.Equal(got, w) {
			t.Fatalf("case %d: %q resolves to %v, refersTo matches %v (resources %+v)", c, one, got, w, all)
		}
		// A run of references over one index.
		ix := newTargetIndex(all)
		gotAll, wantAll := map[int]bool{}, map[int]bool{}
		for range 1 + r.IntN(5) {
			ref := ref()
			w := want(ref)
			for _, i := range ix.resolve(ref) {
				if !slices.Contains(w, i) {
					t.Fatalf("case %d: %q resolves to %d, which refersTo does not match", c, ref, i)
				}
				gotAll[i] = true
			}
			// Order: once a reference is resolved, every resource it may
			// name is resolved, by it or an earlier one, so no region is
			// added later than refersTo would add it.
			for _, i := range w {
				if !gotAll[i] {
					t.Fatalf("case %d: after %q, resource %d it names is not yet resolved", c, ref, i)
				}
				wantAll[i] = true
			}
			total++
			if len(w) > 0 {
				matched++
			}
		}
		for i := range wantAll {
			if !gotAll[i] {
				t.Fatalf("case %d: resource %d is matched by a reference but never resolved", c, i)
			}
		}
	}
	if matched < 5000 {
		t.Fatalf("only %d of %d references match a resource", matched, total)
	}
}

// A fullUrl ends in a type and an id only when the type is the resource's
// own: a Patient whose fullUrl ends in Task/1 is not named by Patient/1.
func TestTargetIndexTailIsTheResourcesOwnType(t *testing.T) {
	all := []*resource{{typ: "Patient", fullURL: "https://x/Task/1"}, {node: 1, typ: "Task", fullURL: "https://x/Task/1"}}
	for ref, want := range map[string][]int{"Patient/1": nil, "Task/1": {1}, "https://x/Task/1": {0, 1}} {
		if got := newTargetIndex(all).resolve(ref); !slices.Equal(got, want) {
			t.Errorf("%q: %v, want %v", ref, got, want)
		}
	}
}
