package relay

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay/internal/splice"
)

// signedProvenanceBundle is a Bundle of n resources and one signed Provenance with
// n targets, each naming one of them.
func signedProvenanceBundle(n int) string {
	var b strings.Builder
	b.WriteString(`{"resourceType":"Bundle","type":"collection","id":"b","entry":[{"resource":{"resourceType":"Provenance","signature":[{"type":[],"when":"x","who":{}}],"target":[`)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"reference":"Basic/%d"}`, i)
	}
	b.WriteString(`]}}`)
	for i := range n {
		fmt.Fprintf(&b, `,{"fullUrl":"https://x.example/fhir/Basic/%d","resource":{"resourceType":"Basic","id":"%d"}}`, i, i)
	}
	b.WriteString(`]}`)
	return b.String()
}

// Finding what a signed Provenance covers costs time near linear in its
// targets and the document's resources, not their product.
func TestCoverageCostIsNearLinearInProvenanceTargets(t *testing.T) {
	wantLinearCost(t, "coverage", 32768, func(n int) func() {
		src := signedProvenanceBundle(n)
		d, err := splice.Scan([]byte(src), splice.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		if c := coverage(d); len(c.hits(effect{start: 0, end: len(src)})) == 0 {
			t.Fatal("the Provenance covers nothing")
		}
		return func() { _ = coverage(d) }
	})
}

// signedSearchset is a search answer of e paging links and g Subscriptions,
// each holding a Signature (dup: one Subscription holding g Signatures).
func signedSearchset(e, g int, dup bool) string {
	var b strings.Builder
	b.WriteString(`{"resourceType":"Bundle","type":"searchset","link":[`)
	for i := range e {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"relation":"next","url":"https://p.example/fhir/Subscription?i=%d"}`, i)
	}
	b.WriteString(`],"entry":[`)
	sig := `{"type":[],"when":"x","who":{}}`
	if dup {
		b.WriteString(`{"resource":{"resourceType":"Subscription","s":[` + strings.TrimSuffix(strings.Repeat(sig+",", g), ",") + `]}}`)
	} else {
		for i := range g {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"resource":{"resourceType":"Subscription","s":` + sig + `}}`)
		}
	}
	b.WriteString(`]}`)
	return b.String()
}

// Deciding which signatures a change's effects touch costs time near linear
// in the effects and the covered regions, not their product, whether the
// regions are distinct or one region many times.
func TestCoverageHitsCostIsNearLinearInEditsAndRegions(t *testing.T) {
	for _, dup := range []bool{false, true} {
		wantLinearCost(t, fmt.Sprintf("hits (one Subscription holding every Signature: %v)", dup), 32768, func(n int) func() {
			b := NewBody([]byte(signedSearchset(n, n, dup)), OriginPeerFrame)
			doc := mustDoc(t, b)
			var raw []splice.Op
			for i, u := range eachAt(t, doc, []string{"link"}, "url") {
				raw = append(raw, doc.Replace(u, []byte(`"https://g.example/p?i=`+strconv.Itoa(i)+`"`)).op)
			}
			m, err := buildModel(doc.d, b.bytes(), raw)
			if err != nil {
				t.Fatal(err)
			}
			cov := coverage(doc.d)
			return func() {
				for _, eff := range m.effects {
					_ = cov.hits(*eff)
				}
			}
		})
	}
}

// Apply on a body holding a signed Provenance, whatever the edit, costs time
// near linear in the body: the coverage it computes is the part a peer's
// body sets.
func TestApplyCostIsNearLinearInSignedProvenance(t *testing.T) {
	wantLinearCost(t, "Apply", 32768, func(n int) func() {
		b := NewBody([]byte(signedProvenanceBundle(n)), OriginPeerFrame)
		doc := mustDoc(t, b)
		op := doc.Replace(at(t, doc, "id"), []byte(`"c"`))
		if _, err := Apply(b, "application/fhir+json", EditCDSCallbackStrip, op); err != nil {
			t.Fatal(err)
		}
		return func() { _, _ = Apply(b, "application/fhir+json", EditCDSCallbackStrip, op) }
	})
}

// typeName is the i-th of distinct names a carrier repeats ("Signature in
// <type>"): a capital, then letters.
func typeName(i int) string {
	b := []byte{'A'}
	for {
		b = append(b, byte('a'+i%26))
		i /= 26
		if i == 0 {
			return string(b)
		}
	}
}

// One effect touching many regions, each its own carrier (a signed resource
// of its own type inside the value an edit changes), costs time near linear
// in them, not their number times the carriers already found.
func TestCoverageHitsCostIsNearLinearInDistinctCarriers(t *testing.T) {
	wantLinearCost(t, "hits (distinct carriers)", 49152, func(n int) func() {
		var b strings.Builder
		b.WriteString(`{"resourceType":"Bundle","x":[`)
		for i := range n {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"resourceType":"` + typeName(i) + `","s":{"type":[],"when":"x","who":{}}}`)
		}
		b.WriteString(`],"v":1}`)
		d, err := splice.Scan([]byte(b.String()), splice.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		x, _ := d.Member(d.Root(), "x")
		s, e := d.Span(x)
		eff := effect{start: s, end: e}
		cov := coverage(d)
		if got := len(cov.hits(eff)); got != n {
			t.Fatalf("%d carriers, want %d", got, n)
		}
		return func() { _ = cov.hits(eff) }
	})
}
