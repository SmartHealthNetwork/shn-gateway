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
		src := []byte(signedProvenanceBundle(n))
		if c := coverage(src); len(c.hits(effect{start: 0, end: len(src)})) == 0 {
			t.Fatal("the Provenance covers nothing")
		}
		return func() { _ = coverage(src) }
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
	sig := `{"url":"u","valueSignature":{"type":[],"when":"x","who":{}}}`
	if dup {
		b.WriteString(`{"resource":{"resourceType":"Subscription","extension":[` + strings.TrimSuffix(strings.Repeat(sig+",", g), ",") + `]}}`)
	} else {
		for i := range g {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"resource":{"resourceType":"Subscription","extension":[` + sig + `]}}`)
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
			cov := coverage(b.bytes())
			// Each Subscription is covered by its own Signatures, kept once
			// a region: n regions, or one.
			if want := map[bool]int{false: n, true: 1}[dup]; len(cov.regions) != want {
				t.Fatalf("%d covered regions, want %d", len(cov.regions), want)
			}
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

// typeName is the i-th of distinct names: a capital, then letters.
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

// One effect touching many regions, each its own carrier, costs time near
// linear in them, not their number times the carriers already found. A
// document's carriers are bounded (one per R4 resource type, and three
// more; signedcontent's TestCoverageCarriersAreBounded), far below this row's sizes, so it
// builds the regions itself, as a document's resources lie (side by side,
// each signed under a carrier of its own), to hold hits to it for any
// carrier set.
func TestCoverageHitsCostIsNearLinearInDistinctCarriers(t *testing.T) {
	wantLinearCost(t, "hits (distinct carriers)", 49152, func(n int) func() {
		regions := make([]coveredRegion, n)
		for i := range n {
			regions[i] = coveredRegion{start: 2 * i, end: 2*i + 1, carrier: "Signature in " + typeName(i)}
		}
		cov := newCoverageMap(regions)
		if !cov.nested {
			t.Fatal("side-by-side regions read as not nested")
		}
		eff := effect{start: 0, end: 2 * n}
		if got := len(cov.hits(eff)); got != n {
			t.Fatalf("%d carriers, want %d", got, n)
		}
		return func() { _ = cov.hits(eff) }
	})
}
