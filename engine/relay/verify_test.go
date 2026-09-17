package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay/internal/splice"
)

// fixedSplice makes the splice return out and spans as given.
func fixedSplice(t *testing.T, out string, spans ...splice.Edit) {
	t.Helper()
	withSpliceHook(t, func(*splice.Doc, ...splice.Op) ([]byte, []splice.Edit, error) {
		return []byte(out), spans, nil
	})
}

func wantMismatch(t *testing.T, err error, fragment string) {
	t.Helper()
	if !errors.Is(err, ErrVerifyMismatch) {
		t.Fatalf("want ErrVerifyMismatch, got %v", err)
	}
	if !strings.Contains(err.Error(), fragment) {
		t.Fatalf("refused by the wrong check: %v (want %q)", err, fragment)
	}
}

// Each row below is refused through Apply by exactly one of the three
// checks; the others pass it.
func TestVerifyRowsCaughtByOneCheck(t *testing.T) {
	const src = `{"a": "x","b":"y", "c":"z"}`
	rows := []struct {
		name     string
		ops      func(d *Document) []Op
		out      string
		spans    []splice.Edit
		fragment string
	}{
		{"rebuild: a layout byte changed outside every span",
			func(d *Document) []Op { return []Op{d.Replace(at(t, d, "a"), []byte(`"q"`))} },
			`{"a": "q","b":"y","c":"z"}`, []splice.Edit{{Start: 6, End: 9, New: []byte(`"q"`)}},
			"outside the reported spans"},
		{"spans: a replace span that also takes the following comma",
			func(d *Document) []Op { return []Op{d.Replace(at(t, d, "a"), []byte(`"q"`))} },
			`{"a": "q","b":"y", "c":"z"}`, []splice.Edit{{Start: 6, End: 10, New: []byte(`"q",`)}},
			"not exactly the replaced value"},
		{"spans: a replace span whose new bytes carry extra layout",
			func(d *Document) []Op { return []Op{d.Replace(at(t, d, "a"), []byte(`"q"`))} },
			`{"a": "q" ,"b":"y", "c":"z"}`, []splice.Edit{{Start: 6, End: 9, New: []byte(`"q" `)}},
			"does not write exactly the replaced value"},
		{"spans: whitespace collapsed next to an untouched member",
			func(d *Document) []Op { return []Op{d.Replace(at(t, d, "b"), []byte(`"q"`))} },
			`{"a":"x","b":"q", "c":"z"}`, []splice.Edit{{Start: 5, End: 6, New: []byte{}}, {Start: 14, End: 17, New: []byte(`"q"`)}},
			"holds no declared removal or addition"},
		{"compare: a sibling name token re-escaped",
			func(d *Document) []Op { return []Op{d.Replace(at(t, d, "a"), []byte(`"q"`))} },
			`{"` + string(rune(92)) + `u0061": "q","b":"y", "c":"z"}`,
			[]splice.Edit{{Start: 1, End: 4, New: []byte(`"` + string(rune(92)) + `u0061"`)}, {Start: 6, End: 9, New: []byte(`"q"`)}},
			"name token"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			b := NewBody([]byte(src), OriginPeerFrame)
			d := mustDoc(t, b)
			ops := r.ops(d)
			fixedSplice(t, r.out, r.spans...)
			_, err := Apply(b, "", EditPayorEdgeRestamp, ops...)
			wantMismatch(t, err, r.fragment)
		})
	}
	t.Run("layout collapsed around a removal is the edit's own seam", func(t *testing.T) {
		// Documented residual: the layout between a removed member and its
		// neighbours is not pinned, only that it is layout.
		b := NewBody([]byte(src), OriginPeerFrame)
		d := mustDoc(t, b)
		fixedSplice(t, "{\"a\": \"x\",\n\"c\":\"z\"}", splice.Edit{Start: 10, End: 19, New: []byte("\n")})
		if _, err := Apply(b, "", EditCDSCallbackStrip, d.RemoveMember(d.Root(), "b")); err != nil {
			t.Fatal(err)
		}
	})
}

// modelFor builds the verification model for ops on src.
func modelFor(t *testing.T, src string, ops func(d *Document) []Op) *model {
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
	return m
}

// TestVerifyChecksStandAlone drives each check directly with input that only
// that check's branch refuses, so no check relies on another to catch its
// case.
func TestVerifyChecksStandAlone(t *testing.T) {
	const src = `{"a":"x","b":"y","c":"z"}`
	removeB := func(d *Document) []Op { return []Op{d.RemoveMember(d.Root(), "b")} }
	legit := `{"a":"x","c":"z"}`

	t.Run("rebuild", func(t *testing.T) {
		m := modelFor(t, src, removeB)
		spans := []splice.Edit{{Start: 9, End: 17, New: []byte{}}}
		if err := m.checkRebuild([]byte(legit), spans); err != nil {
			t.Fatal(err)
		}
		wantMismatch(t, m.checkRebuild([]byte(`{"a":"X","c":"z"}`), spans), "outside the reported spans")
		wantMismatch(t, m.checkRebuild([]byte(legit), []splice.Edit{{Start: 9, End: 99}}), "out of order or range")
	})

	t.Run("spans", func(t *testing.T) {
		m := modelFor(t, src, removeB)
		ok := []splice.Edit{{Start: 9, End: 17, New: []byte{}}}
		if err := m.checkSpans([]byte(legit), ok, &outAnchors{}); err != nil {
			t.Fatal(err)
		}
		// Removes the surviving member a together with b.
		wantMismatch(t, m.checkSpans([]byte(`{"c":"z"}`), []splice.Edit{{Start: 1, End: 17, New: []byte{}}}, &outAnchors{}),
			"removes bytes no operation removes")
		// Writes a token no operation adds.
		wantMismatch(t, m.checkSpans([]byte(`{"a":"x",1"c":"z"}`), []splice.Edit{{Start: 9, End: 17, New: []byte(`1`)}}, &outAnchors{}),
			"writes bytes no operation adds")
		// Cuts through the removed member.
		wantMismatch(t, m.checkSpans([]byte(legit), []splice.Edit{{Start: 10, End: 17, New: []byte{}}}, &outAnchors{}),
			"cuts through a removed member")
		// Writes into a replaced value of the output.
		wantMismatch(t, m.checkSpans([]byte(`{"a":"x"X"c":"z"}`), []splice.Edit{{Start: 8, End: 17, New: []byte(`X`)}},
			&outAnchors{replaced: []span{{5, 9}}}), "writes into a replaced value")
		// Holds nothing declared.
		wantMismatch(t, m.checkSpans([]byte(src), []splice.Edit{{Start: 8, End: 9, New: []byte(`,`)}}, &outAnchors{}),
			"holds no declared removal or addition")

		ins := modelFor(t, `{"a":1}`, func(d *Document) []Op { return []Op{d.InsertMember(d.Root(), "k", []byte(`2`))} })
		out := []byte(`{"a":1,"k":2}`)
		added := &outAnchors{added: []span{{7, 12}}}
		if err := ins.checkSpans(out, []splice.Edit{{Start: 6, End: 6, New: []byte(`,"k":2`)}}, added); err != nil {
			t.Fatal(err)
		}
		wantMismatch(t, ins.checkSpans(out, []splice.Edit{{Start: 6, End: 6, New: []byte(`,"k"`)}}, added),
			"cuts through an added value")

		// Removes a token made only of quotes.
		app := modelFor(t, `[""]`, func(d *Document) []Op { return []Op{d.AppendElement(d.Root(), []byte(`2`))} })
		appOut := []byte(`["",2]`)
		appAdded := &outAnchors{added: []span{{4, 5}}}
		if err := app.checkSpans(appOut, []splice.Edit{{Start: 3, End: 3, New: []byte(`,2`)}}, appAdded); err != nil {
			t.Fatal(err)
		}
		wantMismatch(t, app.checkSpans(appOut, []splice.Edit{{Start: 1, End: 3, New: []byte(`"",2`)}}, appAdded),
			"removes bytes no operation removes")

		rep := modelFor(t, src, func(d *Document) []Op { return []Op{d.Replace(at(t, d, "a"), []byte(`"q"`))} })
		repOut := []byte(`{"a":"q","b":"y","c":"z"}`)
		if err := rep.checkSpans(repOut, []splice.Edit{{Start: 5, End: 8, New: []byte(`"q"`)}}, &outAnchors{replaced: []span{{5, 8}}}); err != nil {
			t.Fatal(err)
		}
		wantMismatch(t, rep.checkSpans(repOut, []splice.Edit{{Start: 5, End: 8, New: []byte(`"q"`)}}, &outAnchors{replaced: []span{{5, 7}}}),
			"does not write exactly the replaced value")
		wantMismatch(t, rep.checkSpans(repOut, []splice.Edit{{Start: 4, End: 8, New: []byte(`:"q"`)}}, &outAnchors{replaced: []span{{5, 8}}}),
			"not exactly the replaced value")
	})

	t.Run("compare", func(t *testing.T) {
		m := modelFor(t, src, removeB)
		cmp := func(out string) error {
			od, err := splice.Scan([]byte(out), splice.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			return m.compare(od, []byte(out), m.doc.Root(), od.Root(), "$", &outAnchors{})
		}
		if err := cmp(legit); err != nil {
			t.Fatal(err)
		}
		wantMismatch(t, cmp(`{"a":"X","c":"z"}`), "changed but no operation addresses it")
		wantMismatch(t, cmp(`{"a":"x","c":"z","zz":1}`), "has 3 members, want 2")
		wantMismatch(t, cmp(`{"a":"x","d":"z"}`), "name token")
		wantMismatch(t, cmp(`{"`+string(rune(92))+`u0061":"x","c":"z"}`), "name token")
		wantMismatch(t, cmp(`["a"]`), "changed kind")

		rep := modelFor(t, src, func(d *Document) []Op { return []Op{d.Replace(at(t, d, "a"), []byte(`"q"`))} })
		od, _ := splice.Scan([]byte(src), splice.Limits{})
		wantMismatch(t, rep.compare(od, []byte(src), rep.doc.Root(), od.Root(), "$", &outAnchors{}), "does not hold the declared value")

		arr := modelFor(t, `[1]`, func(d *Document) []Op { return []Op{d.AppendElement(d.Root(), []byte(`2`))} })
		for out, frag := range map[string]string{`[1]`: "has 1 elements, want 2", `[1,2,3]`: "has 3 elements, want 2",
			`[1,3]`: "does not hold the appended value", `[0,2]`: "changed but no operation"} {
			ad, _ := splice.Scan([]byte(out), splice.Limits{})
			wantMismatch(t, arr.compare(ad, []byte(out), 0, 0, "$", &outAnchors{}), frag)
		}
	})
}

func TestLayoutBytes(t *testing.T) {
	for c := 0; c < 256; c++ {
		want := c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ','
		if isLayout(byte(c)) != want {
			t.Errorf("isLayout(%q) = %v", rune(c), !want)
		}
	}
}

func TestApplyRefusesOperationsFromAnotherBody(t *testing.T) {
	raw := []byte(`{"a":1}`)
	b1 := NewBody(raw, OriginPeerFrame)
	b2 := NewBody(raw, OriginPeerFrame) // same bytes, different body
	d1 := mustDoc(t, b1)
	op := d1.Replace(at(t, d1, "a"), []byte(`2`))
	if _, err := Apply(b2, "", EditPayorEdgeRestamp, op); !errors.Is(err, ErrInvalidEdit) || !strings.Contains(err.Error(), "another body") {
		t.Fatalf("want a foreign-operation refusal, got %v", err)
	}
	// A copy of the same Body is the same body.
	b1copy := b1
	if _, err := Apply(b1copy, "", EditPayorEdgeRestamp, op); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(b1, "", EditPayorEdgeRestamp, Op{}); !errors.Is(err, ErrInvalidEdit) {
		t.Fatalf("zero Op: %v", err)
	}
}

func TestOpAndChangeDoNotPrintValues(t *testing.T) {
	const secret = "SECRETVALUE"
	b := NewBody([]byte(`{"id":"`+secret+`"}`), OriginPeerFrame)
	d := mustDoc(t, b)
	op := d.Replace(at(t, d, "id"), []byte(`"`+secret+`2"`))
	ins := d.InsertMember(d.Root(), "SECRETNAME", []byte(`1`))
	c := Change{Edit: EditPayorEdgeRestamp, Ops: []Op{op, ins}}
	for _, v := range []any{op, c, &c, []Change{c}, Embed{Source: b, Start: 6, End: 19}} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%x", "%d", "%q"} {
			got := fmt.Sprintf(verb, v)
			if strings.Contains(got, "SECRET") || strings.Contains(strings.ToLower(got), fmt.Sprintf("%x", "SECRET")) {
				t.Errorf("%s leaks: %s", verb, got)
			}
		}
		j, _ := json.Marshal(v)
		if bytes.Contains(j, []byte("SECRET")) || bytes.Contains(j, []byte("U0VDUkVU")) {
			t.Errorf("json leaks: %s", j)
		}
	}
	if got := fmt.Sprint(op); got != "relay.Op{replace node 1, 14 value bytes}" {
		t.Fatalf("Op prints %q", got)
	}
	if got := fmt.Sprint(Op{}); got != "relay.Op{}" {
		t.Fatalf("zero Op prints %q", got)
	}
}

func TestSignatureCoverageEdges(t *testing.T) {
	str := []byte(`"changed"`)
	rows := []struct {
		name    string
		doc     string
		path    []string
		carrier string
	}{
		{"relative target matches a resource with no id by its fullUrl",
			`{"resourceType":"Bundle","entry":[
			  {"fullUrl":"http://a.example/fhir/Claim/c1","resource":{"resourceType":"Claim","status":"active"}},
			  {"resource":{"resourceType":"Provenance","target":[{"reference":"http://b.example/fhir/Claim/c1"}],"signature":[{"type":[],"when":"x","who":{}}]}}]}`,
			[]string{"entry", "0", "resource", "status"}, "Provenance.signature"},
		{"a fullUrl ending in another type does not match",
			`{"resourceType":"Bundle","entry":[
			  {"fullUrl":"http://a.example/fhir/Coverage/c1","resource":{"resourceType":"Claim","status":"active"}},
			  {"resource":{"resourceType":"Provenance","target":[{"reference":"Claim/c1"}],"signature":[{"type":[],"when":"x","who":{}}]}}]}`,
			[]string{"entry", "0", "resource", "status"}, ""},
		{"Signature element with the extension form of when",
			`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ServiceRequest","status":"active","extension":[{"url":"u","valueSignature":{"type":[],"_when":{"extension":[]},"who":{}}}]}}]}`,
			[]string{"entry", "0", "resource", "status"}, "Signature in ServiceRequest"},
		{"an object with type and who but no when is not a Signature",
			`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ServiceRequest","status":"active","performer":[{"type":"x","who":{}}]}}]}`,
			[]string{"entry", "0", "resource", "status"}, ""},
		{"target inside a nested Bundle",
			`{"resourceType":"Parameters","parameter":[
			  {"name":"p","resource":{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim","id":"c1","status":"active"}}]}},
			  {"name":"q","resource":{"resourceType":"Provenance","target":[{"reference":"Claim/c1"}],"signature":[{"type":[],"when":"x","who":{}}]}}]}`,
			[]string{"parameter", "0", "resource", "entry", "0", "resource", "status"}, "Provenance.signature"},
		{"inner Bundle signed, outer sibling entry edited",
			`{"resourceType":"Bundle","entry":[
			  {"resource":{"resourceType":"Bundle","signature":{"type":[],"when":"x","who":{}},"entry":[]}},
			  {"resource":{"resourceType":"Claim","id":"c2","status":"active"}}]}`,
			[]string{"entry", "1", "resource", "status"}, ""},
		{"a null Bundle.signature still counts",
			`{"resourceType":"Bundle","signature":null,"entry":[{"resource":{"resourceType":"Claim","id":"c2","status":"active"}}]}`,
			[]string{"entry", "0", "resource", "status"}, "Bundle.signature"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			b := NewBody([]byte(r.doc), OriginPeerFrame)
			d := mustDoc(t, b)
			_, err := Apply(b, "", EditPayorEdgeRestamp, d.Replace(at(t, d, r.path...), str))
			if r.carrier == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			var se *SignedContentError
			if !errors.As(err, &se) || se.Carrier != r.carrier {
				t.Fatalf("want carrier %q, got %v", r.carrier, err)
			}
		})
	}
}

// embedCase builds a searchset that embeds n entries of one source.
func embedCase(tb testing.TB, n int) ([]byte, []Embed) {
	tb.Helper()
	var sb strings.Builder
	sb.WriteString(`{"resourceType":"Bundle","entry":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `{"resource":{"resourceType":"Observation","id":"o%d","status":"final"}}`, i)
	}
	sb.WriteString(`]}`)
	src := NewBody([]byte(sb.String()), OriginUpstreamResponse)
	d, err := Doc(src)
	if err != nil {
		tb.Fatal(err)
	}
	raw := src.bytes()
	var out bytes.Buffer
	out.WriteString(`{"resourceType":"Bundle","type":"searchset","entry":[`)
	var embeds []Embed
	entry, _ := d.Member(d.Root(), "entry")
	for i, e := range d.Elems(entry) {
		s, en := d.Span(e)
		if i > 0 {
			out.WriteByte(',')
		}
		embeds = append(embeds, Embed{Source: src, Start: s, End: en, At: out.Len()})
		out.Write(raw[s:en])
	}
	out.WriteString(`]}`)
	return out.Bytes(), embeds
}

// TestAuthoredScansEachSourceOnce pins the cost shape of embed checks: one
// scan of the authored body and one per distinct source, however many
// embeds there are.
func TestAuthoredScansEachSourceOnce(t *testing.T) {
	for _, n := range []int{1, 50, 5000} {
		b, embeds := embedCase(t, n)
		before := scanCount.Load()
		if _, err := Authored(BuilderSoRSearchset, b, fhirJSON, embeds...); err != nil {
			t.Fatalf("%d embeds: %v", n, err)
		}
		if got := scanCount.Load() - before; got != 2 {
			t.Fatalf("%d embeds: %d scans, want 2", n, got)
		}
	}
	// A second source adds one scan.
	b, embeds := embedCase(t, 3)
	other := NewBody([]byte(`{"x":1}`), OriginPeerFrame)
	body := append(append([]byte{}, b[:len(b)-2]...), `,{"x":1}]}`...)
	embeds = append(embeds, Embed{Source: other, Start: 0, End: 7, At: len(b) - 1})
	before := scanCount.Load()
	if _, err := Authored(BuilderSoRSearchset, body, fhirJSON, embeds...); err != nil {
		t.Fatal(err)
	}
	if got := scanCount.Load() - before; got != 3 {
		t.Fatalf("%d scans, want 3", got)
	}
}

func BenchmarkAuthoredEmbeds(b *testing.B) {
	for _, n := range []int{100, 1000, 5000} {
		body, embeds := embedCase(b, n)
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			for b.Loop() {
				if _, err := Authored(BuilderSoRSearchset, body, fhirJSON, embeds...); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestEmbedSourceMustBeAWholeValue(t *testing.T) {
	src := NewBody([]byte(`{"a":123}`), OriginPeerFrame)
	// Bytes match and the destination is a whole value, but the source span
	// is the tail of a number.
	_, err := Authored(BuilderCDexFulfillment, []byte(`{"x":23}`), "application/json", Embed{Source: src, Start: 6, End: 8, At: 5})
	if !errors.Is(err, ErrEmbedMismatch) || !strings.Contains(err.Error(), "source span") {
		t.Fatalf("want a source whole-value refusal, got %v", err)
	}
	// The same bytes from a whole source value pass.
	src2 := NewBody([]byte(`{"a":23}`), OriginPeerFrame)
	if _, err := Authored(BuilderCDexFulfillment, []byte(`{"x":23}`), "application/json", Embed{Source: src2, Start: 5, End: 7, At: 5}); err != nil {
		t.Fatal(err)
	}
}
