package relay

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay/internal/splice"
	"github.com/SmartHealthNetwork/shn-sdk/signedcontent"
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

// TestCoverageHitsAgreesWithHitsEach: on seeded random documents, for every
// effect an operation can make (a replaced value, a removed member, an
// insertion inside a container) and for random byte ranges, the indexed
// coverage reports exactly the carriers, in exactly the order, that
// comparing every effect with every region of the reading (hitsEach over
// signedcontent's regions) does.
func TestCoverageHitsAgreesWithHitsEach(t *testing.T) {
	r := rand.New(rand.NewPCG(20261114, 3))
	cases, signed := 0, 0
	for c := range 3000 {
		src := randomSignedDoc(r)
		d, err := splice.Scan([]byte(src), splice.Limits{})
		if err != nil {
			t.Fatalf("case %d: %v\n%s", c, err, src)
		}
		cov := coverage([]byte(src))
		ref := cov.regions
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

// The relay's coverage keeps each rule of the reading, indexed and not: a
// valueSignature in an extension covers its resource, a Provenance's target
// by each form covers what it names, and a duplicate region changes nothing.
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
			cov := coverage(b.bytes())
			if got, want := cov.hits(eff), hitsEach(cov.regions, eff); !slices.Equal(got, c.carrier) || !slices.Equal(want, c.carrier) {
				t.Fatalf("indexed %q, each %q, want %q", got, want, c.carrier)
			}
		})
	}
}

// coverage reads only a body the relay already scanned, under the same
// limits as signedcontent, so a body the reading refuses is a fault in the
// gateway: coverage panics rather than read it as unsigned.
func TestCoverageOfABodyTheReadingRefusesPanics(t *testing.T) {
	for name, src := range map[string]string{
		"a repeated member": `{"resourceType":"Bundle","signature":{},"signature":{}}`,
		"not JSON":          `{"resourceType":`,
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				got := recover()
				if got == nil {
					t.Fatal("coverage read a body signedcontent refuses")
				}
				// The fault names the body's size and the reading's error,
				// so it can be diagnosed.
				_, want := signedcontent.Coverage([]byte(src))
				msg, _ := got.(string)
				if want == nil || !strings.Contains(msg, fmt.Sprintf("%d bytes", len(src))) || !strings.Contains(msg, want.Error()) {
					t.Fatalf("panic %v, want one naming %d bytes and %v", got, len(src), want)
				}
			}()
			coverage([]byte(src))
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
