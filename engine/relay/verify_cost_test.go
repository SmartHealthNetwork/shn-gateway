package relay

import (
	"strconv"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay/internal/splice"
)

// linksBody is a search answer of n paging links, each a member url in an
// object of a link array: the shape whose every url an edit may re-key.
func linksBody(n int) string {
	var b strings.Builder
	b.WriteString(`{"resourceType":"Bundle","type":"searchset","link":[`)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"relation":"next","url":"https://p.example/fhir/Subscription?_getpages=x&i=` + strconv.Itoa(i) + `"}`)
	}
	b.WriteString(`]}`)
	return b.String()
}

// rekeyLinks is one Replace of every link's url.
func rekeyLinks(t *testing.T, n int) func(d *Document) []Op {
	return func(d *Document) []Op {
		ops := make([]Op, 0, n)
		for i, u := range eachAt(t, d, []string{"link"}, "url") {
			ops = append(ops, d.Replace(u, []byte(`"https://g.example/p/Subscription?_getpages=x&i=`+strconv.Itoa(i)+`"`)))
		}
		return ops
	}
}

// splicedModel is the verification model for ops on src, with the splice's
// output and spans for them.
func splicedModel(t *testing.T, src string, ops func(d *Document) []Op) (*model, []byte, []splice.Edit) {
	t.Helper()
	b := NewBody([]byte(src), OriginPeerFrame)
	d := mustDoc(t, b)
	var raw []splice.Op
	for _, op := range ops(d) {
		raw = append(raw, op.op)
	}
	m, err := buildModel(d.d, b.bytes(), raw)
	if err != nil {
		t.Fatal(err)
	}
	out, spans, err := d.d.Apply(raw...)
	if err != nil {
		t.Fatal(err)
	}
	return m, out, spans
}

// checkSpans costs time near linear in the number of edits, not the product
// of the edits and the spans it compares them with (wantLinearCost).
func TestCheckSpansCostIsNearLinearInEdits(t *testing.T) {
	wantLinearCost(t, "checkSpans", 98304, func(n int) func() {
		m, out, spans := splicedModel(t, linksBody(n), rekeyLinks(t, n))
		od, err := splice.Scan(out, splice.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		var a outAnchors
		if err := m.compare(od, out, m.doc.Root(), od.Root(), "$", &a); err != nil {
			t.Fatal(err)
		}
		if err := m.checkSpans(out, spans, &a); err != nil {
			t.Fatal(err)
		}
		return func() { _ = m.checkSpans(out, spans, &a) }
	})
}

// Apply, which scans, splices and checks a change, costs time near linear
// in the number of edits a peer's message sets: a search answer whose every
// paging link is re-keyed (wantLinearCost). The answer is as large as the
// body limits comfortably admit: scanning and splicing are linear, and a
// check comparing every edit with every span overtakes them only at many
// edits.
func TestApplyCostIsNearLinearInEdits(t *testing.T) {
	wantLinearCost(t, "Apply", 131072, func(n int) func() {
		b := NewBody([]byte(linksBody(n)), OriginPeerFrame)
		ops := rekeyLinks(t, n)(mustDoc(t, b))
		if _, err := Apply(b, "application/fhir+json", EditCDSCallbackStrip, ops...); err != nil {
			t.Fatal(err)
		}
		return func() { _, _ = Apply(b, "application/fhir+json", EditCDSCallbackStrip, ops...) }
	})
}
