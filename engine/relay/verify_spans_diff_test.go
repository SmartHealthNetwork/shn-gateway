package relay

import (
	"fmt"
	"math/rand/v2"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay/internal/splice"
)

// checkSpansEach is checkSpans with every span compared with every replaced,
// removed and anchored span (checkSpanEach): the reference the indexed check
// is held to.
func (m *model) checkSpansEach(out []byte, spans []splice.Edit, a *outAnchors) error {
	delta := 0
	for _, e := range spans {
		os := e.Start + delta
		oe := os + len(e.New)
		delta += len(e.New) - (e.End - e.Start)
		if err := m.checkSpanEach(e, out, os, oe, a); err != nil {
			return err
		}
	}
	return nil
}

// checkSpansIndexed is checkSpans on the indexed path alone, for lists it
// can index: what the differential rows hold to checkSpansEach directly, so
// a fault in checkSpans's choice of path cannot hide the indexed check.
func (m *model) checkSpansIndexed(out []byte, spans []splice.Edit, ix spanIndexes) error {
	delta := 0
	for _, e := range spans {
		os := e.Start + delta
		oe := os + len(e.New)
		delta += len(e.New) - (e.End - e.Start)
		if err := m.checkSpanIndexed(e, out, os, oe, ix); err != nil {
			return err
		}
	}
	return nil
}

// outcome names a verdict by its refusal, coordinates left out, so the rows
// can count which branches the generated cases reached.
var spanCoords = regexp.MustCompile(`\[\d+,\d+\)`)

func outcome(err error) string {
	if err == nil {
		return "pass"
	}
	return spanCoords.ReplaceAllString(err.Error(), "[_)")
}

// sameVerdict fails t unless checkSpans and checkSpansEach agree on out and
// spans exactly (both pass, or both refuse with the same text), and, for
// lists that can be indexed, the indexed check alone agrees too.
func sameVerdict(t *testing.T, name string, m *model, out []byte, spans []splice.Edit, a *outAnchors, seen map[string]int) {
	t.Helper()
	got, want := m.checkSpans(out, spans, a), m.checkSpansEach(out, spans, a)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s: checkSpans %v, checkSpansEach %v", name, got, want)
	}
	path := "each"
	if ix, ok := newSpanIndexes(m, a); ok {
		path = "indexed"
		if direct := m.checkSpansIndexed(out, spans, ix); fmt.Sprint(direct) != fmt.Sprint(want) {
			t.Fatalf("%s: the indexed check alone %v, checkSpansEach %v", name, direct, want)
		}
	}
	seen[path]++
	seen[path+": "+outcome(want)]++
}

// spanOutcomes are every verdict checkSpanEach gives, with its coordinates
// left out.
var spanOutcomes = []string{
	"pass",
	"relay: edited output failed independent verification: span [_) is not exactly the replaced value [_)",
	"relay: edited output failed independent verification: span [_) does not write exactly the replaced value",
	"relay: edited output failed independent verification: span [_) cuts through a removed member",
	"relay: edited output failed independent verification: span [_) removes bytes no operation removes",
	"relay: edited output failed independent verification: span [_) writes into a replaced value",
	"relay: edited output failed independent verification: span [_) cuts through an added value",
	"relay: edited output failed independent verification: span [_) writes bytes no operation adds",
	"relay: edited output failed independent verification: span [_) holds no declared removal or addition",
}

// wantEveryOutcome fails t unless the indexed check itself reached every
// verdict, and the fallback was reached too.
func wantEveryOutcome(t *testing.T, seen map[string]int) {
	t.Helper()
	for _, o := range spanOutcomes {
		if seen["indexed: "+o] == 0 {
			t.Errorf("no generated case reached %q on the indexed path (reached: %v)", o, seen)
		}
	}
	if seen["each"] == 0 {
		t.Errorf("no generated case reached the fallback (reached: %v)", seen)
	}
}

// randSpans is n spans within [0, size): usually non-empty and apart, in a
// shuffled order (indexed), sometimes empty or overlapping (the fallback),
// some placed on the given anchors so the exact and contained branches are
// reached.
func randSpans(r *rand.Rand, size, n int, anchors []span, messy bool) []span {
	var out []span
	if !messy {
		pos := 0
		for range n {
			s := pos + r.IntN(4)
			e := s + 1 + r.IntN(5)
			if e > size {
				break
			}
			out = append(out, span{s, e})
			pos = e
		}
	} else {
		for range n {
			s := r.IntN(size + 1)
			e := min(size, s+r.IntN(6))
			out = append(out, span{s, e})
		}
	}
	for _, a := range anchors {
		// An empty span only ever goes to the fallback: kept out of the
		// lists meant to be indexed.
		if r.IntN(3) == 0 || (!messy && a.start >= a.end) {
			continue
		}
		// Replace any span the anchor meets, keeping the list apart.
		out = slices.DeleteFunc(out, func(x span) bool { return x.start < a.end && a.start < x.end || x == a })
		if messy || !slices.ContainsFunc(out, func(x span) bool { return x.start < a.end && a.start < x.end }) {
			out = append(out, a)
		}
	}
	r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// TestCheckSpansAgreesWithCheckSpanEach: on seeded random inputs (an
// original, the model's replaced and removed spans, reported edits, and the
// output's anchors), the indexed check reaches checkSpanEach's verdict
// exactly, its refusal text included, and every verdict is reached.
func TestCheckSpansAgreesWithCheckSpanEach(t *testing.T) {
	r := rand.New(rand.NewPCG(20261114, 1))
	const layout = " ,\n\t"
	alphabet := layout + layout + `x"{}:1`
	seen := map[string]int{}
	for c := range 20000 {
		size := 20 + r.IntN(80)
		src := make([]byte, size)
		for i := range src {
			src[i] = alphabet[r.IntN(len(alphabet))]
		}
		// Reported edits: apart, in order, each with new bytes.
		var edits []splice.Edit
		var inSrc, inOut []span
		pos, delta := 0, 0
		var out []byte
		for pos < size && len(edits) < 6 {
			s := pos + r.IntN(8)
			if s > size {
				break
			}
			e := min(size, s+r.IntN(8))
			nb := make([]byte, r.IntN(8))
			for i := range nb {
				nb[i] = alphabet[r.IntN(len(alphabet))]
			}
			out = append(out, src[pos:s]...)
			edits = append(edits, splice.Edit{Start: s, End: e, New: nb})
			inSrc = append(inSrc, span{s, e})
			if e > s+1 {
				inSrc = append(inSrc, span{s + 1, e})
			}
			os := s + delta
			inOut = append(inOut, span{os, os + len(nb)})
			if len(nb) > 1 {
				inOut = append(inOut, span{os, os + len(nb) - 1})
			}
			out = append(out, nb...)
			delta += len(nb) - (e - s)
			pos = e
		}
		out = append(out, src[pos:]...)
		messy := r.IntN(5) == 0
		m := &model{
			src:           src,
			replacedSpans: randSpans(r, size, r.IntN(4), inSrc, messy && r.IntN(2) == 0),
			removedSpans:  randSpans(r, size, r.IntN(6), inSrc, messy && r.IntN(2) == 0),
		}
		a := &outAnchors{
			replaced: randSpans(r, len(out), r.IntN(4), inOut, messy && r.IntN(2) == 0),
			added:    randSpans(r, len(out), r.IntN(6), inOut, messy && r.IntN(2) == 0),
		}
		sameVerdict(t, "case "+strconv.Itoa(c), m, out, edits, a, seen)
	}
	wantEveryOutcome(t, seen)
}

// TestCheckSpansAgreesOnRealChanges: on seeded random documents and changes
// (replaced, removed, inserted and appended content), spliced, then with the
// splice's reported spans moved, split or widened by a byte, the indexed
// check reaches checkSpanEach's verdict exactly.
func TestCheckSpansAgreesOnRealChanges(t *testing.T) {
	r := rand.New(rand.NewPCG(20261114, 2))
	seen := map[string]int{}
	for c := range 3000 {
		n := 1 + r.IntN(12)
		var b strings.Builder
		b.WriteString(`{"items":[`)
		for i := range n {
			if i > 0 {
				b.WriteString([]string{",", ", ", ",\n  "}[r.IntN(3)])
			}
			fmt.Fprintf(&b, `{"a":"v%d","b":[%d], "c" : {"d":%d}}`, i, i, i)
		}
		b.WriteString(`],"tail":"t"}`)
		src := b.String()
		m, out, spans, ok := randomChange(t, r, src, n)
		if !ok {
			continue
		}
		od, err := splice.Scan(out, splice.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		var a outAnchors
		if err := m.compare(od, out, m.doc.Root(), od.Root(), "$", &a); err != nil {
			t.Fatalf("case %d: the splice's own output: %v", c, err)
		}
		sameVerdict(t, "case "+strconv.Itoa(c), m, out, spans, &a, seen)
		// The same anchors against reported spans moved off the true ones:
		// the output rebuilt from them, so only checkSpans judges them.
		moved := perturb(r, spans, len(m.src))
		sameVerdict(t, "case "+strconv.Itoa(c)+" moved", m, rebuild(m.src, moved), moved, &a, seen)
	}
	for _, o := range []string{"indexed: pass", "each"} {
		if o == "each" {
			// A real change's lists are always indexed: values and
			// members are never empty, and the splice refuses overlapping
			// operations before the check.
			if seen[o] != 0 {
				t.Errorf("a real change fell back to checkSpanEach (reached: %v)", seen)
			}
			continue
		}
		if seen[o] == 0 {
			t.Errorf("no real change reached %q (reached: %v)", o, seen)
		}
	}
}

// randomChange makes a random change to src, a document of n items, and
// returns its model, the splice's output and its spans.
func randomChange(t *testing.T, r *rand.Rand, src string, n int) (*model, []byte, []splice.Edit, bool) {
	t.Helper()
	body := NewBody([]byte(src), OriginPeerFrame)
	d := mustDoc(t, body)
	var ops []Op
	for i := range n {
		item := strconv.Itoa(i)
		switch r.IntN(6) {
		case 0:
			ops = append(ops, d.Replace(at(t, d, "items", item, "a"), []byte(`"w`+item+`"`)))
		case 1:
			ops = append(ops, d.RemoveMember(at(t, d, "items", item), "b"))
		case 2:
			ops = append(ops, d.InsertMember(at(t, d, "items", item), "e", []byte(`"n`+item+`"`)))
		case 3:
			ops = append(ops, d.AppendElement(at(t, d, "items", item, "b"), []byte(`7`)))
		case 4:
			ops = append(ops, d.Replace(at(t, d, "items", item, "c"), []byte(`{"d":0}`)))
		}
	}
	if r.IntN(4) == 0 {
		ops = append(ops, d.RemoveMember(d.Root(), "tail"))
	}
	if len(ops) == 0 {
		return nil, nil, nil, false
	}
	var raw []splice.Op
	for _, op := range ops {
		raw = append(raw, op.op)
	}
	m, err := buildModel(d.d, body.bytes(), raw)
	if err != nil {
		t.Fatal(err)
	}
	out, spans, err := d.d.Apply(raw...)
	if err != nil {
		t.Fatal(err)
	}
	return m, out, spans, true
}

// perturb returns spans with one moved, split or widened by a byte, kept in
// order and within the original.
func perturb(r *rand.Rand, spans []splice.Edit, size int) []splice.Edit {
	out := slices.Clone(spans)
	if len(out) == 0 {
		return out
	}
	i := r.IntN(len(out))
	e := out[i]
	lo := 0
	if i > 0 {
		lo = out[i-1].End
	}
	hi := size
	if i+1 < len(out) {
		hi = out[i+1].Start
	}
	switch r.IntN(4) {
	case 0:
		e.Start = max(lo, e.Start-1)
	case 1:
		e.End = min(hi, e.End+1)
	case 2:
		if e.End-e.Start >= 2 {
			mid := e.Start + (e.End-e.Start)/2
			cut := min(len(e.New), len(e.New)/2)
			first := splice.Edit{Start: e.Start, End: mid, New: e.New[:cut]}
			e = splice.Edit{Start: mid, End: e.End, New: e.New[cut:]}
			out = slices.Insert(out, i, first)
			i++
		}
	case 3:
		if len(e.New) > 0 {
			e.New = append(slices.Clone(e.New), ' ')
		}
	}
	out[i] = e
	return out
}

// rebuild is src with spans substituted.
func rebuild(src []byte, spans []splice.Edit) []byte {
	var out []byte
	pos := 0
	for _, e := range spans {
		out = append(out, src[pos:e.Start]...)
		out = append(out, e.New...)
		pos = e.End
	}
	return append(out, src[pos:]...)
}
