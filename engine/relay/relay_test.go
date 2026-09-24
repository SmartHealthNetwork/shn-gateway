package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay/internal/splice"
)

// fixtureDir is the vendored relay fixture corpus kept beside the splice
// package (see its README). This package reads the same copies.
const fixtureDir = "internal/splice/testdata/relayfidelity"

const fhirJSON = "application/fhir+json"

func readFixture(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureDir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func validFixtures(t *testing.T) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(fixtureDir, "valid"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, "valid/"+e.Name())
	}
	if len(out) == 0 {
		t.Fatal("no valid fixtures")
	}
	return out
}

func mustDoc(t *testing.T, b Body) *Document {
	t.Helper()
	d, err := Doc(b)
	if err != nil {
		t.Fatalf("Doc: %v", err)
	}
	return d
}

// at resolves a path of member names and array indices ("3") from the root.
func at(t *testing.T, d *Document, path ...string) NodeID {
	t.Helper()
	n := d.Root()
	for _, p := range path {
		switch d.Kind(n) {
		case KindObject:
			m, ok := d.Member(n, p)
			if !ok {
				t.Fatalf("path %v: no member %q", path, p)
			}
			n = m
		case KindArray:
			var i int
			if _, err := fmt.Sscanf(p, "%d", &i); err != nil {
				t.Fatalf("path %v: %q is not an index", path, p)
			}
			el := d.Elems(n)
			if i < 0 || i >= len(el) {
				t.Fatalf("path %v: index %d out of range", path, i)
			}
			n = el[i]
		default:
			t.Fatalf("path %v: %q under a scalar", path, p)
		}
	}
	return n
}

func mustTransmit(t *testing.T, p Payload) []byte {
	t.Helper()
	b, err := Transmit(p, func(Payload) error { return nil })
	if err != nil {
		t.Fatalf("Transmit: %v", err)
	}
	return b
}

func TestDecodeRefusesDuplicateKeys(t *testing.T) {
	for file, key := range map[string]string{
		"invalid/duplicate-key.json":          "beneficiary",
		"invalid/escaped-duplicate-key.json":  "resourceType",
		"invalid/casefold-duplicate-key.json": "Patient",
	} {
		t.Run(file, func(t *testing.T) {
			var v map[string]any
			err := Decode(NewBody(readFixture(t, file), OriginIngressRequest), &v)
			var dk *DuplicateKeyError
			if !errors.As(err, &dk) || !errors.Is(err, ErrDuplicateKey) {
				t.Fatalf("want *DuplicateKeyError, got %v", err)
			}
			if dk.Key != key {
				t.Fatalf("duplicate key %q, want %q", dk.Key, key)
			}
			if errors.Is(err, ErrInvalidJSON) {
				t.Fatal("a duplicate must not also read as invalid JSON")
			}
			if v != nil {
				t.Fatal("nothing may be decoded from a refused body")
			}
			var sk *splice.DuplicateKeyError
			if !errors.As(err, &sk) {
				t.Fatal("the scanner's error must stay reachable")
			}
		})
	}
}

func TestDecodeRefusesInvalidJSON(t *testing.T) {
	rows := map[string]Body{
		"trailing":                        NewBody(readFixture(t, "invalid/trailing-document.json"), OriginPeerFrame),
		"bad utf8":                        NewBody(readFixture(t, "invalid/bad-utf8.bin"), OriginPeerFrame),
		"surrogate":                       NewBody(readFixture(t, "invalid/lone-surrogate.json"), OriginPeerFrame),
		"empty":                           NewBody(nil, OriginPeerFrame),
		"zero Body":                       {},
		"trailing value after whitespace": NewBody([]byte("{} \n {}"), OriginPeerFrame),
	}
	for name, b := range rows {
		t.Run(name, func(t *testing.T) {
			var v any
			err := Decode(b, &v)
			var ij *InvalidJSONError
			if !errors.As(err, &ij) || !errors.Is(err, ErrInvalidJSON) {
				t.Fatalf("want *InvalidJSONError, got %v", err)
			}
			if errors.Is(err, ErrDuplicateKey) {
				t.Fatal("not a duplicate")
			}
			if ij.Reason == "" || !strings.Contains(err.Error(), "offset") {
				t.Fatalf("the error must say where and why: %v", err)
			}
		})
	}
}

// TestDecodeRefusesCaseFoldedNames: a case-insensitive decoder would read
// both spellings into one field, so the body is refused whichever comes
// first.
func TestDecodeRefusesCaseFoldedNames(t *testing.T) {
	kelvin := string(rune(0x212A))
	for name, src := range map[string]string{
		"lower first":   `{"resourceType":"Claim","patient":{"reference":"Patient/a"},"Patient":{"reference":"Patient/b"}}`,
		"upper first":   `{"Patient":{"reference":"Patient/b"},"patient":{"reference":"Patient/a"}}`,
		"escaped":       `{"patient":{},"` + string(rune(92)) + `u0050atient":{}}`,
		"Kelvin sign":   `{"kind":1,"` + kelvin + `IND":2}`,
		"deeply nested": `{"entry":[{"resource":{"contained":[{"id":"x","ID":"y"}]}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			var typed struct {
				Patient struct{ Reference string }
			}
			err := Decode(NewBody([]byte(src), OriginPeerFrame), &typed)
			if !errors.Is(err, ErrDuplicateKey) {
				t.Fatalf("want ErrDuplicateKey, got %v", err)
			}
			if typed.Patient.Reference != "" {
				t.Fatal("nothing may be decoded")
			}
		})
	}
}

func TestDecodeRefusesOversizedBodies(t *testing.T) {
	big := make([]byte, 16<<20+1)
	for i := range big {
		big[i] = ' '
	}
	big[0] = '0'
	tokens := []byte("[" + strings.Repeat("0,", 1_000_000) + "0]")
	for name, raw := range map[string][]byte{
		"depth":  readFixture(t, "invalid/deep-nesting.json"),
		"size":   big,
		"tokens": tokens,
	} {
		t.Run(name, func(t *testing.T) {
			var v any
			err := Decode(NewBody(raw, OriginPeerFrame), &v)
			var le *LimitError
			if !errors.As(err, &le) || !errors.Is(err, ErrTooLarge) || errors.Is(err, ErrInvalidJSON) {
				t.Fatalf("want only *LimitError, got %v", err)
			}
			if le.Reason == "" || !strings.Contains(err.Error(), "too large") {
				t.Fatalf("message %v", err)
			}
			if _, err := Doc(NewBody(raw, OriginPeerFrame)); !errors.Is(err, ErrTooLarge) {
				t.Fatalf("Doc: %v", err)
			}
		})
	}
}

func TestDecodeUsesNumbersAndReportsTypeErrors(t *testing.T) {
	b := NewBody([]byte(`{"a":9007199254740993,"b":1.50,"c":"x"}`), OriginUpstreamResponse)
	var m map[string]any
	if err := Decode(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["a"] != json.Number("9007199254740993") || m["b"] != json.Number("1.50") {
		t.Fatalf("numbers must decode as their lexemes: %#v", m)
	}
	var typed struct {
		C int `json:"c"`
	}
	err := Decode(b, &typed)
	if err == nil || errors.Is(err, ErrInvalidJSON) || errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("a type mismatch is its own error, got %v", err)
	}
	var ute *json.UnmarshalTypeError
	if !errors.As(err, &ute) {
		t.Fatalf("the decoder's error must stay reachable: %v", err)
	}
}

func TestDecodeValidFixtures(t *testing.T) {
	for _, f := range validFixtures(t) {
		var v map[string]any
		if err := Decode(NewBody(readFixture(t, f), OriginPeerFrame), &v); err != nil || len(v) == 0 {
			t.Errorf("%s: %v", f, err)
		}
	}
}

func TestNewBodyCopies(t *testing.T) {
	src := []byte(`{"a":1}`)
	b := NewBody(src, OriginIngressRequest)
	src[5] = '2'
	if got := mustTransmit(t, Exact(b, fhirJSON)); string(got) != `{"a":1}` {
		t.Fatalf("NewBody must copy: %s", got)
	}
	if b.Origin() != OriginIngressRequest || b.Len() != 7 {
		t.Fatalf("Origin/Len = %v/%d", b.Origin(), b.Len())
	}
	if (Body{}).Len() != 0 || (Body{}).Origin() != 0 {
		t.Fatal("zero Body")
	}
	for o, want := range map[Origin]string{OriginIngressRequest: "ingress-request",
		OriginUpstreamResponse: "upstream-response", OriginPeerFrame: "peer-frame", 0: "invalid"} {
		if o.String() != want {
			t.Errorf("Origin(%d) = %q, want %q", o, o.String(), want)
		}
	}
}

func TestExactOfTheZeroBody(t *testing.T) {
	p := Exact(Body{}, "application/json")
	if p.Ownership() != OwnershipRelayed || p.Len() != 0 {
		t.Fatalf("payload %v", p)
	}
	if b, err := Transmit(p, func(Payload) error { return nil }); err != nil || len(b) != 0 {
		t.Fatalf("Transmit = %q, %v", b, err)
	}
}

func TestExactRoundTrip(t *testing.T) {
	for _, f := range validFixtures(t) {
		raw := readFixture(t, f)
		p := Exact(NewBody(raw, OriginPeerFrame), fhirJSON)
		if got := mustTransmit(t, p); !bytes.Equal(got, raw) {
			t.Errorf("%s: Exact changed the bytes", f)
		}
		if p.Ownership() != OwnershipRelayed || p.Edits() != nil || p.Builder() != "" ||
			p.ContentType() != fhirJSON || p.Len() != len(raw) {
			t.Errorf("%s: %v", f, p)
		}
	}
}

func TestDocNavigation(t *testing.T) {
	bs := "\\" // built at run time so the escape survives editing
	b := NewBody([]byte(`{"s":"a`+bs+`u00e9","n":[1,{"k":true}]}`), OriginPeerFrame)
	d := mustDoc(t, b)
	if d.Len() != 6 || d.Kind(d.Root()) != KindObject {
		t.Fatalf("Len/Kind = %d/%v", d.Len(), d.Kind(d.Root()))
	}
	ms := d.Members(d.Root())
	if len(ms) != 2 || ms[0].Name != "s" || ms[1].Name != "n" {
		t.Fatalf("Members = %+v", ms)
	}
	if s, err := d.StringValue(at(t, d, "s")); err != nil || s != "aé" {
		t.Fatalf("StringValue = %q, %v", s, err)
	}
	if _, err := d.StringValue(at(t, d, "n")); err == nil {
		t.Fatal("StringValue on an array must fail")
	}
	k := at(t, d, "n", "1", "k")
	if d.Kind(k) != KindBool {
		t.Fatalf("Kind = %v", d.Kind(k))
	}
	if s, e := d.Span(at(t, d, "n")); s != 19 || e != 33 {
		t.Fatalf("Span = %d,%d", s, e)
	}
	var v []any
	if err := d.DecodeValue(at(t, d, "n"), &v); err != nil || len(v) != 2 || v[0] != json.Number("1") {
		t.Fatalf("DecodeValue = %#v, %v", v, err)
	}
	if err := d.DecodeValue(NodeID(99), &v); err == nil {
		t.Fatal("DecodeValue on a missing node must fail")
	}
	if _, err := Doc(NewBody(readFixture(t, "invalid/duplicate-key.json"), OriginPeerFrame)); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("Doc must refuse duplicates: %v", err)
	}
	if _, err := Doc(Body{}); !errors.Is(err, ErrInvalidJSON) {
		t.Fatalf("Doc must refuse an empty body: %v", err)
	}
}

func TestApplyEdit(t *testing.T) {
	b := NewBody([]byte(`{"hook":"order-sign", "fhirServer":"https://ehr.example","prefetch":{"a":1.50}}`), OriginIngressRequest)
	d := mustDoc(t, b)
	p, err := Apply(b, "application/json", EditCDSCallbackStrip, d.RemoveMember(d.Root(), "fhirServer"))
	if err != nil {
		t.Fatal(err)
	}
	if got := mustTransmit(t, p); string(got) != `{"hook":"order-sign", "prefetch":{"a":1.50}}` {
		t.Fatalf("Apply output %s", got)
	}
	if p.Ownership() != OwnershipEdited || !slices.Equal(p.Edits(), []EditID{EditCDSCallbackStrip}) ||
		p.ContentType() != "application/json" || p.Builder() != "" {
		t.Fatalf("payload %v", p)
	}
	e := p.Edits()
	e[0] = "X"
	if p.Edits()[0] != EditCDSCallbackStrip {
		t.Fatal("Edits must return a copy")
	}
}

func TestApplyChangesOnAFixture(t *testing.T) {
	raw := readFixture(t, "valid/crd-order-sign-request.json")
	b := NewBody(raw, OriginIngressRequest)
	d := mustDoc(t, b)
	root := d.Root()
	pf := at(t, d, "prefetch")
	p, err := ApplyChanges(b, "application/json",
		Change{Edit: EditCDSCallbackStrip, Ops: []Op{d.RemoveMember(root, "fhirServer"), d.RemoveMember(root, "fhirAuthorization")}},
		Change{Edit: EditCDSPrefetchObtain, Ops: []Op{d.InsertMember(pf, "encounter", []byte(`null`))}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.Edits(), []EditID{EditCDSCallbackStrip, EditCDSPrefetchObtain}) || p.Ownership() != OwnershipEdited {
		t.Fatalf("payload %v", p)
	}
	out := mustTransmit(t, p)
	var got map[string]any
	if err := Decode(NewBody(out, OriginPeerFrame), &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["fhirServer"]; ok {
		t.Fatal("fhirServer kept")
	}
	if _, ok := got["fhirAuthorization"]; ok {
		t.Fatal("fhirAuthorization kept")
	}
	if v, ok := got["prefetch"].(map[string]any)["encounter"]; !ok || v != nil {
		t.Fatal("prefetch.encounter not inserted as null")
	}
	// The signed prefetch Bundle and every number lexeme are carried as-is.
	cs, ce := d.Span(at(t, d, "prefetch", "coverage"))
	if !bytes.Contains(out, raw[cs:ce]) || !bytes.Contains(out, []byte("[ 1.50, 9007199254740993, 1e2, -0.0 ]")) {
		t.Fatal("untouched content changed")
	}

	// Ensure creates the prefetch object when it is absent.
	b2 := NewBody([]byte(`{"hook":"order-sign"}`), OriginIngressRequest)
	ens, h := mustDoc(t, b2).EnsureObjectMember(mustDoc(t, b2).Root(), "prefetch")
	p2, err := Apply(b2, "application/json", EditCDSPrefetchObtain, ens, mustDoc(t, b2).InsertMember(NodeID(h), "patient", []byte(`{"resourceType":"Patient"}`)))
	if err != nil {
		t.Fatal(err)
	}
	if got := mustTransmit(t, p2); string(got) != `{"hook":"order-sign","prefetch":{"patient":{"resourceType":"Patient"}}}` {
		t.Fatalf("ensure output %s", got)
	}

	// Append into a DTR Parameters array.
	b3 := NewBody(readFixture(t, "valid/dtr-package-params-2.2.json"), OriginIngressRequest)
	d3 := mustDoc(t, b3)
	p3, err := Apply(b3, fhirJSON, EditDTRCoverageObtain, d3.AppendElement(at(t, d3, "parameter"), []byte(`{"name":"coverage","resource":{"resourceType":"Coverage","id":"c9"}}`)))
	if err != nil {
		t.Fatal(err)
	}
	var params struct {
		Parameter []struct {
			Name string `json:"name"`
		} `json:"parameter"`
	}
	if err := Decode(NewBody(mustTransmit(t, p3), OriginPeerFrame), &params); err != nil {
		t.Fatal(err)
	}
	if n := len(params.Parameter); n != len(d3.Elems(at(t, d3, "parameter")))+1 || params.Parameter[n-1].Name != "coverage" {
		t.Fatalf("append result %+v", params)
	}
}

func TestApplyNoOpIsRelayed(t *testing.T) {
	raw := readFixture(t, "valid/pas-submit-2.0.json")
	b := NewBody(raw, OriginPeerFrame)
	d := mustDoc(t, b)
	// A same-value replace inside the signed Bundle is not an edit.
	n := at(t, d, "entry", "0", "fullUrl")
	s, e := d.Span(n)
	same := append([]byte{}, raw[s:e]...)
	rows := map[string][]Op{
		"same-value replace": {d.Replace(n, same)},
		"no operations":      nil,
	}
	ens, _ := d.EnsureObjectMember(d.Root(), "meta")
	rows["ensure an existing object"] = []Op{ens}
	for name, ops := range rows {
		t.Run(name, func(t *testing.T) {
			p, err := Apply(b, fhirJSON, EditPayorEdgeRestamp, ops...)
			if err != nil {
				t.Fatal(err)
			}
			if p.Ownership() != OwnershipRelayed || p.Edits() != nil {
				t.Fatalf("a no-op must relay exactly: %v", p)
			}
			if !bytes.Equal(mustTransmit(t, p), raw) {
				t.Fatal("bytes changed")
			}
		})
	}
}

func TestApplyRefusals(t *testing.T) {
	b := NewBody([]byte(`{"a":1,"b":[]}`), OriginPeerFrame)
	d := mustDoc(t, b)
	if _, err := Apply(b, "", EditID("E-99"), d.Replace(at(t, d, "a"), []byte(`2`))); !errors.Is(err, ErrUnknownEdit) {
		t.Fatalf("unknown edit id: %v", err)
	}
	if _, err := Apply(b, "", "", d.Replace(at(t, d, "a"), []byte(`2`))); !errors.Is(err, ErrUnknownEdit) {
		t.Fatalf("empty edit id: %v", err)
	}
	if _, err := ApplyChanges(b, ""); !errors.Is(err, ErrUnknownEdit) {
		t.Fatalf("no changes: %v", err)
	}
	for name, op := range map[string]Op{
		"missing member": d.RemoveMember(d.Root(), "zz"),
		"bad value":      d.Replace(at(t, d, "a"), []byte(`{`)),
		"wrong kind":     d.AppendElement(at(t, d, "a"), []byte(`1`)),
		"existing key":   d.InsertMember(d.Root(), "a", []byte(`1`)),
		"zero op":        {},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Apply(b, "", EditPayorEdgeRestamp, op)
			if !errors.Is(err, ErrInvalidEdit) || errors.Is(err, ErrDuplicateKey) {
				t.Fatalf("want ErrInvalidEdit (and not an input duplicate), got %v", err)
			}
		})
	}
	t.Run("overlapping changes", func(t *testing.T) {
		_, err := ApplyChanges(b, "",
			Change{Edit: EditPayorEdgeRestamp, Ops: []Op{d.Replace(at(t, d, "a"), []byte(`2`))}},
			Change{Edit: EditCDSCallbackStrip, Ops: []Op{d.RemoveMember(d.Root(), "a")}})
		if !errors.Is(err, ErrInvalidEdit) {
			t.Fatalf("want ErrInvalidEdit, got %v", err)
		}
	})
	dup := NewBody(readFixture(t, "invalid/duplicate-key.json"), OriginPeerFrame)
	if _, err := Apply(dup, "", EditPayorEdgeRestamp); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("duplicate input: %v", err)
	}
	if _, err := Apply(Body{}, "", EditPayorEdgeRestamp); !errors.Is(err, ErrInvalidJSON) {
		t.Fatalf("empty input: %v", err)
	}
}

// withSpliceHook swaps the splice for one test. Tests using it do not run in
// parallel.
func withSpliceHook(t *testing.T, h func(*splice.Doc, ...splice.Op) ([]byte, []splice.Edit, error)) {
	t.Helper()
	old := spliceHook
	spliceHook = h
	t.Cleanup(func() { spliceHook = old })
}

func TestApplyVerifyCatchesFaultySplice(t *testing.T) {
	src := []byte(`{"a":"x","b":"y","c":{"d":1}}`)
	b := NewBody(src, OriginPeerFrame)
	d := mustDoc(t, b)
	replaceA := d.Replace(at(t, d, "a"), []byte(`"z"`))
	rows := []struct {
		name string
		ops  []Op
		hook func(out []byte, spans []splice.Edit) ([]byte, []splice.Edit)
	}{
		{"byte changed outside the span", []Op{replaceA}, func(out []byte, spans []splice.Edit) ([]byte, []splice.Edit) {
			out[len(out)-3] = '2'
			return out, spans
		}},
		{"span widened to cover a changed byte", []Op{replaceA}, func(out []byte, spans []splice.Edit) ([]byte, []splice.Edit) {
			// "a":"z","b":"q" — the span now reaches into b and says so.
			out = []byte(`{"a":"z","b":"q","c":{"d":1}}`)
			return out, []splice.Edit{{Start: 5, End: 16, New: []byte(`"z","b":"q"`)}}
		}},
		{"a different value than declared", []Op{replaceA}, func(out []byte, spans []splice.Edit) ([]byte, []splice.Edit) {
			return []byte(`{"a":"w","b":"y","c":{"d":1}}`), []splice.Edit{{Start: 5, End: 8, New: []byte(`"w"`)}}
		}},
		{"another member dropped inside the object", []Op{d.RemoveMember(d.Root(), "a")}, func(out []byte, spans []splice.Edit) ([]byte, []splice.Edit) {
			return []byte(`{"c":{"d":1}}`), []splice.Edit{{Start: 1, End: 17, New: []byte{}}}
		}},
		{"insert written somewhere else", []Op{d.InsertMember(at(t, d, "c"), "e", []byte(`2`))}, func(out []byte, spans []splice.Edit) ([]byte, []splice.Edit) {
			return []byte(`{"a":"x","b":"y","e":2,"c":{"d":1}}`), []splice.Edit{{Start: 17, End: 17, New: []byte(`"e":2,`)}}
		}},
		{"append of the wrong element", []Op{}, nil},
		{"spans out of range", []Op{replaceA}, func(out []byte, spans []splice.Edit) ([]byte, []splice.Edit) {
			return out, []splice.Edit{{Start: 5, End: 999, New: []byte(`"z"`)}}
		}},
		{"spans out of order", []Op{replaceA, d.RemoveMember(at(t, d, "c"), "d")}, func(out []byte, spans []splice.Edit) ([]byte, []splice.Edit) {
			slices.Reverse(spans)
			return out, spans
		}},
		{"output that does not scan", []Op{replaceA}, func(out []byte, spans []splice.Edit) ([]byte, []splice.Edit) {
			// Consistent spans, exact target, but the new value is broken.
			return []byte(`{"a":"z,"b":"y","c":{"d":1}}`), []splice.Edit{{Start: 5, End: 8, New: []byte(`"z`)}}
		}},
		{"change reported for a no-op", []Op{d.Replace(at(t, d, "a"), []byte(`"x"`))}, func(out []byte, spans []splice.Edit) ([]byte, []splice.Edit) {
			return []byte(`{"a":"x","b":"y","c":{"d":3}}`), spans
		}},
	}
	arrBody := NewBody([]byte(`{"p":[1]}`), OriginPeerFrame)
	arrDoc := mustDoc(t, arrBody)
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			body, ops, hook := b, r.ops, r.hook
			if r.hook == nil { // append of the wrong element
				body = arrBody
				ops = []Op{arrDoc.AppendElement(at(t, arrDoc, "p"), []byte(`2`))}
				hook = func(out []byte, spans []splice.Edit) ([]byte, []splice.Edit) {
					return []byte(`{"p":[1,3]}`), []splice.Edit{{Start: 7, End: 7, New: []byte(`,3`)}}
				}
			}
			withSpliceHook(t, func(sd *splice.Doc, sops ...splice.Op) ([]byte, []splice.Edit, error) {
				out, spans, err := sd.Apply(sops...)
				if err != nil {
					return nil, nil, err
				}
				out, spans = hook(out, spans)
				return out, spans, nil
			})
			p, err := Apply(body, "", EditPayorEdgeRestamp, ops...)
			if !errors.Is(err, ErrVerifyMismatch) {
				t.Fatalf("want ErrVerifyMismatch, got %v (%v)", err, p)
			}
			t.Log(err)
			if p.Ownership() != 0 {
				t.Fatal("a refused Apply returns the zero Payload")
			}
		})
	}
	t.Run("unfaulted splice passes", func(t *testing.T) {
		withSpliceHook(t, func(sd *splice.Doc, sops ...splice.Op) ([]byte, []splice.Edit, error) { return sd.Apply(sops...) })
		if _, err := Apply(b, "", EditPayorEdgeRestamp, replaceA); err != nil {
			t.Fatal(err)
		}
	})
}

// provenanceBundle is an unsigned Bundle whose Provenance signs the Claim
// (by relative reference), the Patient (by fullUrl) and a contained
// Organization, but not the Coverage. The ServiceRequest carries a nested
// Signature element.
const provenanceBundle = `{
  "resourceType": "Bundle",
  "type": "collection",
  "entry": [
    {"fullUrl": "http://example.org/fhir/Claim/c1",
     "resource": {"resourceType": "Claim", "id": "c1", "insurer": {"reference": "Organization/o1"}}},
    {"fullUrl": "urn:uuid:5b1f4c9e-0000-4000-8000-000000000001",
     "resource": {"resourceType": "Patient", "id": "p1", "name": [{"family": "SMITH"}]}},
    {"fullUrl": "http://example.org/fhir/Coverage/cov1",
     "resource": {"resourceType": "Coverage", "id": "cov1", "status": "active"}},
    {"fullUrl": "http://example.org/fhir/Organization/o1",
     "resource": {"resourceType": "Organization", "id": "o1", "name": "Payer",
       "contained": [{"resourceType": "Organization", "id": "part", "name": "Part"}]}},
    {"fullUrl": "http://example.org/fhir/ServiceRequest/sr1",
     "resource": {"resourceType": "ServiceRequest", "id": "sr1", "status": "active",
       "extension": [{"url": "http://example.org/sig", "valueSignature": {
         "type": [{"system": "urn:iso-astm:E1762-95:2013", "code": "1.2.840.10065.1.12.1.1"}],
         "when": "2026-06-19T21:54:38+00:00", "who": {"reference": "Practitioner/pr1"}}}]}},
    {"fullUrl": "http://example.org/fhir/Provenance/pv1",
     "resource": {"resourceType": "Provenance", "id": "pv1",
       "target": [{"reference": "Claim/c1/_history/2"},
                  {"reference": "urn:uuid:5b1f4c9e-0000-4000-8000-000000000001"},
                  {"reference": "#part"},
                  {"identifier": {"value": "not a literal reference"}},
                  {"reference": "Encounter/not-in-this-document"}],
       "recorded": "2026-06-19T21:54:38+00:00",
       "signature": [{"type": [{"code": "1.2.840.10065.1.12.1.1"}],
         "when": "2026-06-19T21:54:38+00:00", "who": {"reference": "Practitioner/pr1"}}]}}
  ]
}`

func TestApplySignedContent(t *testing.T) {
	pas := NewBody(readFixture(t, "valid/pas-submit-2.0.json"), OriginPeerFrame)
	pasDoc := mustDoc(t, pas)
	crd := NewBody(readFixture(t, "valid/crd-order-sign-request.json"), OriginIngressRequest)
	crdDoc := mustDoc(t, crd)
	prov := NewBody([]byte(provenanceBundle), OriginPeerFrame)
	provDoc := mustDoc(t, prov)
	inq := NewBody(readFixture(t, "valid/pas-inquiry-response-2.2.json"), OriginPeerFrame)
	inqDoc := mustDoc(t, inq)

	str := []byte(`"changed"`)
	type row struct {
		name    string
		body    Body
		edit    EditID
		ops     func() []Op
		carrier string // "" = must be edited
	}
	rows := []row{
		{"Bundle.signature: a token inside an entry", pas, EditPayorEdgeRestamp, func() []Op {
			return []Op{pasDoc.Replace(at(t, pasDoc, "entry", "0", "fullUrl"), str)}
		}, "Bundle.signature"},
		{"Bundle.signature: a member added to the Bundle", pas, EditPayorEdgeRestamp, func() []Op {
			return []Op{pasDoc.InsertMember(pasDoc.Root(), "x", []byte(`1`))}
		}, "Bundle.signature"},
		{"Bundle.signature: the signature itself removed", pas, EditPayorEdgeRestamp, func() []Op {
			return []Op{pasDoc.RemoveMember(pasDoc.Root(), "signature")}
		}, "Bundle.signature"},
		{"Bundle.signature: an entry appended", pas, EditPayorEdgeRestamp, func() []Op {
			return []Op{pasDoc.AppendElement(at(t, pasDoc, "entry"), []byte(`{}`))}
		}, "Bundle.signature"},
		{"signed prefetch Bundle: a token inside it", crd, EditPayorEdgeRestamp, func() []Op {
			return []Op{crdDoc.Replace(at(t, crdDoc, "prefetch", "coverage", "entry", "0", "resource", "id"), str)}
		}, "Bundle.signature"},
		{"signed prefetch Bundle: the whole Bundle replaced", crd, EditPayorEdgeRestamp, func() []Op {
			return []Op{crdDoc.Replace(at(t, crdDoc, "prefetch", "coverage"), []byte(`null`))}
		}, "Bundle.signature"},
		{"signed prefetch Bundle: removed from prefetch", crd, EditCDSPrefetchObtain, func() []Op {
			return []Op{crdDoc.RemoveMember(at(t, crdDoc, "prefetch"), "coverage")}
		}, "Bundle.signature"},
		{"signed prefetch Bundle: a sibling key inserted", crd, EditCDSPrefetchObtain, func() []Op {
			return []Op{crdDoc.InsertMember(at(t, crdDoc, "prefetch"), "encounter", []byte(`null`))}
		}, ""},
		{"signed prefetch Bundle: the member after it removed", crd, EditCDSPrefetchObtain, func() []Op {
			return []Op{crdDoc.RemoveMember(at(t, crdDoc, "prefetch"), "serviceHistory")}
		}, ""},
		{"signed prefetch Bundle: an unsigned sibling Bundle edited", crd, EditPayorEdgeRestamp, func() []Op {
			return []Op{crdDoc.Replace(at(t, crdDoc, "prefetch", "deviceHistory", "entry", "0", "resource", "id"), str)}
		}, ""},
		{"Provenance.signature: target by relative reference", prov, EditPayorEdgeRestamp, func() []Op {
			return []Op{provDoc.Replace(at(t, provDoc, "entry", "0", "resource", "insurer", "reference"), str)}
		}, "Provenance.signature"},
		{"Provenance.signature: target by fullUrl", prov, EditPayorEdgeRestamp, func() []Op {
			return []Op{provDoc.InsertMember(at(t, provDoc, "entry", "1", "resource"), "gender", []byte(`"male"`))}
		}, "Provenance.signature"},
		{"Provenance.signature: contained target", prov, EditPayorEdgeRestamp, func() []Op {
			return []Op{provDoc.Replace(at(t, provDoc, "entry", "3", "resource", "contained", "0", "name"), str)}
		}, "Provenance.signature"},
		{"Provenance.signature: the Provenance itself", prov, EditPayorEdgeRestamp, func() []Op {
			return []Op{provDoc.Replace(at(t, provDoc, "entry", "5", "resource", "recorded"), str)}
		}, "Provenance.signature"},
		{"Provenance.signature: an untargeted resource", prov, EditPayorEdgeRestamp, func() []Op {
			return []Op{provDoc.Replace(at(t, provDoc, "entry", "2", "resource", "status"), str)}
		}, ""},
		{"Provenance.signature: the containing resource outside its contained target", prov, EditPayorEdgeRestamp, func() []Op {
			return []Op{provDoc.Replace(at(t, provDoc, "entry", "3", "resource", "name"), str)}
		}, ""},
		{"Provenance.signature: entry metadata outside the target", prov, EditPayorEdgeRestamp, func() []Op {
			return []Op{provDoc.Replace(at(t, provDoc, "entry", "0", "fullUrl"), str)}
		}, ""},
		{"Signature element: its enclosing resource", prov, EditPayorEdgeRestamp, func() []Op {
			return []Op{provDoc.Replace(at(t, provDoc, "entry", "4", "resource", "status"), str)}
		}, "Signature in ServiceRequest"},
		{"Signature element: nested Bundle signature in a response", inq, EditPayorEdgeRestamp, func() []Op {
			return []Op{inqDoc.Replace(at(t, inqDoc, "parameter", "0", "resource", "entry", "0", "fullUrl"), str)}
		}, "Bundle.signature"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			p, err := Apply(r.body, fhirJSON, r.edit, r.ops()...)
			if r.carrier == "" {
				if err != nil {
					t.Fatalf("uncovered edit refused: %v", err)
				}
				if p.Ownership() != OwnershipEdited {
					t.Fatalf("want Edited, got %v", p)
				}
				return
			}
			var se *SignedContentError
			if !errors.As(err, &se) || !errors.Is(err, ErrSignedContent) {
				t.Fatalf("want *SignedContentError, got %v", err)
			}
			if se.Edit != r.edit || se.Carrier != r.carrier {
				t.Fatalf("got (%s, %s), want (%s, %s)", se.Edit, se.Carrier, r.edit, r.carrier)
			}
			want := fmt.Sprintf("signed content cannot be edited (%s, %s)", r.edit, r.carrier)
			if err.Error() != want {
				t.Fatalf("message %q, want %q", err.Error(), want)
			}
		})
	}
	t.Run("the refusal names the change that touched signed content", func(t *testing.T) {
		_, err := ApplyChanges(crd, "application/json",
			Change{Edit: EditCDSCallbackStrip, Ops: []Op{crdDoc.RemoveMember(crdDoc.Root(), "fhirServer")}},
			Change{Edit: EditPayorEdgeRestamp, Ops: []Op{crdDoc.Replace(at(t, crdDoc, "prefetch", "coverage", "type"), str)}})
		var se *SignedContentError
		if !errors.As(err, &se) || se.Edit != EditPayorEdgeRestamp {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a signature element outside any resource covers the document", func(t *testing.T) {
		b := NewBody([]byte(`{"a":1,"s":{"type":[],"when":"x","who":{}}}`), OriginPeerFrame)
		_, err := Apply(b, "", EditPayorEdgeRestamp, mustDoc(t, b).Replace(at(t, mustDoc(t, b), "a"), []byte(`2`)))
		var se *SignedContentError
		if !errors.As(err, &se) || se.Carrier != "Signature" {
			t.Fatalf("got %v", err)
		}
	})
}

func TestAuthored(t *testing.T) {
	src := []byte(`{"resourceType":"OperationOutcome"}`)
	p, err := Authored(BuilderGatewayRefusal, src, fhirJSON)
	if err != nil {
		t.Fatal(err)
	}
	src[2] = 'X'
	if p.Ownership() != OwnershipAuthored || p.Builder() != BuilderGatewayRefusal || p.Edits() != nil ||
		string(mustTransmit(t, p)) != `{"resourceType":"OperationOutcome"}` {
		t.Fatalf("payload %v", p)
	}
	if _, err := Authored("", nil, "application/json"); !errors.Is(err, ErrUnknownBuilder) {
		t.Fatalf("empty id: %v", err)
	}
	if _, err := Authored("reencode-helper", []byte(`{}`), "application/json"); !errors.Is(err, ErrUnknownBuilder) {
		t.Fatalf("unknown id: %v", err)
	}
	_, err = Authored(BuilderID("test-injected"), []byte(`{}`), "application/json")
	if !errors.Is(err, ErrReservedBuilder) || errors.Is(err, ErrUnknownBuilder) {
		t.Fatalf("reserved id: %v", err)
	}
	if _, err := Authored(BuilderGatewayRefusal, []byte(`{"a":1`), "application/json"); !errors.Is(err, ErrInvalidJSON) {
		t.Fatalf("invalid JSON: %v", err)
	}
	if _, err := Authored(BuilderGatewayRefusal, []byte(`{"a":1,"a":2}`), "application/problem+json; charset=utf-8"); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := Authored(BuilderGatewayRefusal, []byte("not json"), "text/plain; charset=utf-8"); err != nil {
		t.Fatalf("a non-JSON media type is not scanned: %v", err)
	}
	if _, err := Authored(BuilderGatewayRefusal, nil, "application/json"); err != nil {
		t.Fatalf("an empty body is allowed: %v", err)
	}
	for _, ct := range []string{"", "not a media type;;"} {
		if _, err := Authored(BuilderGatewayRefusal, []byte(`{}`), ct); !errors.Is(err, ErrContentType) {
			t.Fatalf("content type %q: %v", ct, err)
		}
	}
	for _, id := range append(slices.Clone(registeredBuilders), interimBuilders...) {
		if _, err := Authored(id, []byte(`{}`), "application/json"); err != nil {
			t.Errorf("%s: %v", id, err)
		}
	}
}

func TestAuthoredEmbeds(t *testing.T) {
	raw := readFixture(t, "valid/pas-submit-2.0.json")
	srcBody := NewBody(raw, OriginPeerFrame)
	d := mustDoc(t, srcBody)
	s, e := d.Span(at(t, d, "entry", "0", "resource"))
	prefix := `{"resourceType":"Bundle","entry":[{"resource":`
	b := append(append([]byte(prefix), raw[s:e]...), "}]}"...)
	good := Embed{Source: srcBody, Start: s, End: e, At: len(prefix)}

	p, err := Authored(BuilderCDexFulfillment, b, fhirJSON, good)
	if err != nil {
		t.Fatalf("valid embed: %v", err)
	}
	if !bytes.Equal(mustTransmit(t, p), b) {
		t.Fatal("bytes")
	}

	forged := append([]byte(prefix), bytes.Replace(raw[s:e], []byte(`"careTeam"`), []byte(`"careTeaM"`), 1)...)
	forged = append(forged, "}]}"...)
	sub, subEnd := d.Span(at(t, d, "entry", "0", "resource", "created"))
	seq, seqEnd := d.Span(at(t, d, "entry", "0", "resource", "careTeam", "0", "sequence"))
	rows := map[string]struct {
		b []byte
		e Embed
	}{
		"bytes differ":                     {forged, good},
		"destination off by one":           {b, Embed{Source: srcBody, Start: s, End: e, At: len(prefix) + 1}},
		"destination past the end":         {b, Embed{Source: srcBody, Start: s, End: e, At: len(b)}},
		"negative destination":             {b, Embed{Source: srcBody, Start: s, End: e, At: -1}},
		"source past the end":              {b, Embed{Source: srcBody, Start: s, End: len(raw) + 1, At: len(prefix)}},
		"empty source span":                {b, Embed{Source: srcBody, Start: s, End: s, At: len(prefix)}},
		"source span is not a whole value": {b, Embed{Source: srcBody, Start: s + 1, End: e, At: len(prefix) + 1}},
		"source is not JSON":               {b, Embed{Source: NewBody([]byte("{"), OriginPeerFrame), Start: 0, End: 1, At: 0}},
		"zero source":                      {b, Embed{Start: 0, End: 1, At: 0}},
		// A whole value in the source copied so that it is only part of a
		// value in the authored body.
		"destination is not a whole value": {[]byte(`{"x":[11]}`),
			Embed{Source: srcBody, Start: seq, End: seqEnd, At: 6}},
	}
	for name, r := range rows {
		t.Run(name, func(t *testing.T) {
			_, err := Authored(BuilderCDexFulfillment, r.b, fhirJSON, r.e)
			if !errors.Is(err, ErrEmbedMismatch) {
				t.Fatalf("want ErrEmbedMismatch, got %v", err)
			}
			if name == "source is not JSON" && !strings.Contains(err.Error(), "source: relay: invalid JSON") {
				t.Fatalf("want the source's own refusal, got %v", err)
			}
		})
	}
	t.Run("a scalar embed in a non-JSON body", func(t *testing.T) {
		body := []byte("id=" + string(raw[sub:subEnd]))
		if _, err := Authored(BuilderGatewayRefusal, body, "text/plain", Embed{Source: srcBody, Start: sub, End: subEnd, At: 3}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestTransmit(t *testing.T) {
	called := false
	check := func(Payload) error { called = true; return nil }
	if b, err := Transmit(Payload{}, check); !errors.Is(err, ErrUnsetOwnership) || b != nil || called {
		t.Fatalf("zero payload: %v %v called=%v", b, err, called)
	}
	p := Exact(NewBody([]byte(`{"a":1}`), OriginPeerFrame), "application/json")
	if _, err := Transmit(p, nil); !errors.Is(err, ErrNoOwnershipCheck) {
		t.Fatalf("nil check: %v", err)
	}
	refuse := errors.New("not on this leg")
	var seen Payload
	b, err := Transmit(p, func(q Payload) error { seen = q; return refuse })
	if !errors.Is(err, refuse) || b != nil || seen.Ownership() != OwnershipRelayed {
		t.Fatalf("refusing check: %v %v", b, err)
	}
	out := mustTransmit(t, p)
	out[0] = '['
	if again := mustTransmit(t, p); string(again) != `{"a":1}` {
		t.Fatalf("Transmit must return a copy: %s", again)
	}
}

func TestBodyAndPayloadDoNotLeakBytes(t *testing.T) {
	const secret = "SECRETMEMBERID"
	raw := []byte(`{"id":"` + secret + `"}`)
	body := NewBody(raw, OriginPeerFrame)
	pay := Exact(body, "application/json")
	doc := mustDoc(t, body)
	type holder struct {
		b Body
		p Payload
		d *Document
		B Body
		P Payload
	}
	values := map[string]any{
		"body": body, "payload": pay, "doc": doc, "body ptr": &body, "payload ptr": &pay,
		"holder": holder{body, pay, doc, body, pay}, "holder ptr": &holder{body, pay, doc, body, pay},
		"slice": []Payload{pay}, "map": map[string]Body{"k": body},
	}
	hexSecret := fmt.Sprintf("%x", secret)
	for name, v := range values {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%T"} {
			got := fmt.Sprintf(verb, v)
			if strings.Contains(got, secret) || strings.Contains(strings.ToLower(got), hexSecret) ||
				strings.Contains(got, "123 34 105 100") { // the leading bytes printed as numbers
				t.Errorf("%s %s leaks bytes: %s", name, verb, got)
			}
		}
		j, err := json.Marshal(v)
		if err != nil {
			t.Errorf("%s: json: %v", name, err)
		}
		if strings.Contains(string(j), secret) || strings.Contains(string(j), "eyJpZCI6") { // base64 of the bytes
			t.Errorf("%s: json leaks bytes: %s", name, j)
		}
	}
	if j, _ := json.Marshal(body); string(j) != `{}` {
		t.Fatalf("json.Marshal(Body) = %s", j)
	}
	if j, _ := json.Marshal(pay); string(j) != `{}` {
		t.Fatalf("json.Marshal(Payload) = %s", j)
	}
	if got := fmt.Sprint(pay); got != "relay.Payload{relayed application/json 23 bytes}" {
		t.Fatalf("Payload prints %q", got)
	}
	if got := fmt.Sprint(body); got != "relay.Body{peer-frame 23 bytes}" {
		t.Fatalf("Body prints %q", got)
	}
}

func TestOwnershipAndIdentifiers(t *testing.T) {
	for o, want := range map[Ownership]string{0: "unset", OwnershipRelayed: "relayed",
		OwnershipEdited: "edited", OwnershipAuthored: "authored", 9: "Ownership(9)"} {
		if o.String() != want {
			t.Errorf("Ownership(%d) = %q, want %q", o, o.String(), want)
		}
	}
	if got := EditIDs(); !slices.Equal(got, []EditID{"E-01", "E-02", "E-03", "E-04", "E-05"}) {
		t.Fatalf("EditIDs = %v", got)
	}
	ids := EditIDs()
	ids[0] = "X"
	if EditIDs()[0] != "E-01" {
		t.Fatal("EditIDs must return a copy")
	}
	var z Payload
	if z.Ownership() != 0 || z.Edits() != nil || z.ContentType() != "" || z.Builder() != "" || z.Len() != 0 {
		t.Fatal("zero Payload accessors")
	}
}

func TestBuilderSetIsClosed(t *testing.T) {
	want := []BuilderID{"gateway-refusal", "cdex-fulfillment", "cdex-records", "sor-searchset", "sdk-crd-request",
		"sdk-dtr-package", "sdk-pas-submit", "sdk-pas-update", "sdk-pas-inquiry", "sdk-eligibility",
		"sdk-federated-query", "sdk-patient-dtr", "dtr-next-question"}
	if !slices.Equal(registeredBuilders, want) {
		t.Fatalf("registeredBuilders = %v", registeredBuilders)
	}
	wantInterim := []BuilderID{"defect-empty-error-substitution"}
	if !slices.Equal(interimBuilders, wantInterim) {
		t.Fatalf("interimBuilders = %v", interimBuilders)
	}
	if len(authoredBuilders) != len(want)+len(wantInterim) {
		t.Fatalf("authoredBuilders has %d ids", len(authoredBuilders))
	}
	if _, ok := authoredBuilders[builderTestInjected]; ok {
		t.Fatal("the reserved id must not be registered")
	}
	if got := Builders(); !slices.Equal(got, append(slices.Clone(want), wantInterim...)) {
		t.Fatalf("Builders = %v", got)
	}
}

func TestApplyVerifyCreatedObjects(t *testing.T) {
	src := []byte(`{"a":1}`)
	b := NewBody(src, OriginPeerFrame)
	d := mustDoc(t, b)
	ops := func() []Op {
		ens, h := d.EnsureObjectMember(d.Root(), "pf")
		inner, h2 := d.EnsureObjectMember(NodeID(h), "q")
		again, _ := d.EnsureObjectMember(d.Root(), "pf")
		return []Op{ens, d.InsertMember(NodeID(h), "k", []byte(`1`)), inner, d.InsertMember(NodeID(h2), "z", []byte(`true`)), again}
	}
	p, err := Apply(b, "", EditCDSPrefetchObtain, ops()...)
	if err != nil {
		t.Fatal(err)
	}
	if got := mustTransmit(t, p); string(got) != `{"a":1,"pf":{"k":1,"q":{"z":true}}}` {
		t.Fatalf("output %s", got)
	}
	for name, written := range map[string]string{
		"wrong inserted value":    `,"pf":{"k":2,"q":{"z":true}}`,
		"created member renamed":  `,"pg":{"k":1,"q":{"z":true}}`,
		"created object replaced": `,"pf":[]`,
		"nested member missing":   `,"pf":{"k":1,"q":{}}`,
		"nested member renamed":   `,"pf":{"k":1,"r":{"z":true}}`,
		"extra created member":    `,"pf":{"k":1,"q":{"z":true},"x":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			withSpliceHook(t, func(sd *splice.Doc, sops ...splice.Op) ([]byte, []splice.Edit, error) {
				out := append(append([]byte{}, src[:6]...), written...)
				out = append(out, '}')
				return out, []splice.Edit{{Start: 6, End: 6, New: []byte(written)}}, nil
			})
			_, err := Apply(b, "", EditCDSPrefetchObtain, ops()...)
			if !errors.Is(err, ErrVerifyMismatch) {
				t.Fatalf("want ErrVerifyMismatch, got %v", err)
			}
			t.Log(err)
		})
	}
}

func TestApplyRefusesMalformedOperations(t *testing.T) {
	b := NewBody([]byte(`{"a":1,"o":{"x":1},"arr":[]}`), OriginPeerFrame)
	d := mustDoc(t, b)
	ens, h := d.EnsureObjectMember(d.Root(), "new")
	ensO, hO := d.EnsureObjectMember(d.Root(), "o")
	ensA, _ := d.EnsureObjectMember(d.Root(), "a")
	stray, hs := d.EnsureObjectMember(d.Root(), "stray")
	_ = stray
	rows := map[string][]Op{
		"replace a missing value":                   {d.Replace(NodeID(999), []byte(`1`))},
		"replace a handle":                          {ens, d.Replace(NodeID(h), []byte(`1`))},
		"replace one value twice":                   {d.Replace(at(t, d, "a"), []byte(`2`)), d.Replace(at(t, d, "a"), []byte(`3`))},
		"remove from a created object":              {ens, d.RemoveMember(NodeID(h), "x")},
		"remove from an array":                      {d.RemoveMember(at(t, d, "arr"), "x")},
		"remove from a missing value":               {d.RemoveMember(NodeID(999), "x")},
		"insert through an unknown handle":          {d.InsertMember(NodeID(hs), "k", []byte(`1`))},
		"insert into a number":                      {d.InsertMember(at(t, d, "a"), "k", []byte(`1`))},
		"insert one name twice":                     {d.InsertMember(d.Root(), "k", []byte(`1`)), d.InsertMember(d.Root(), "k", []byte(`2`))},
		"insert twice into a created one":           {ens, d.InsertMember(NodeID(h), "k", []byte(`1`)), d.InsertMember(NodeID(h), "k", []byte(`2`))},
		"ensure over a number":                      {ensA},
		"ensure under an array":                     {func() Op { op, _ := d.EnsureObjectMember(at(t, d, "arr"), "k"); return op }()},
		"ensure over an inserted value":             {d.InsertMember(d.Root(), "new", []byte(`1`)), ens},
		"ensure under an unknown handle":            {func() Op { op, _ := d.EnsureObjectMember(NodeID(hs), "k"); return op }()},
		"append to an object":                       {d.AppendElement(d.Root(), []byte(`1`))},
		"append to a handle":                        {ens, d.AppendElement(NodeID(h), []byte(`1`))},
		"ensure existing then insert a name it has": {ensO, d.InsertMember(NodeID(hO), "x", []byte(`2`))},
	}
	for name, ops := range rows {
		t.Run(name, func(t *testing.T) {
			if _, err := Apply(b, "", EditCDSPrefetchObtain, ops...); !errors.Is(err, ErrInvalidEdit) {
				t.Fatalf("want ErrInvalidEdit, got %v", err)
			}
		})
	}
	t.Run("ensure existing then insert", func(t *testing.T) {
		p, err := Apply(b, "", EditCDSPrefetchObtain, ensO, d.InsertMember(NodeID(hO), "y", []byte(`2`)))
		if err != nil {
			t.Fatal(err)
		}
		if got := mustTransmit(t, p); string(got) != `{"a":1,"o":{"x":1,"y":2},"arr":[]}` {
			t.Fatalf("output %s", got)
		}
	})
	t.Run("remove and re-insert a name", func(t *testing.T) {
		p, err := Apply(b, "", EditCDSPrefetchObtain, d.RemoveMember(d.Root(), "a"), d.InsertMember(d.Root(), "a", []byte(`5`)))
		if err != nil {
			t.Fatal(err)
		}
		if got := mustTransmit(t, p); string(got) != `{"o":{"x":1},"arr":[],"a":5}` {
			t.Fatalf("output %s", got)
		}
	})
}

func TestErrorAndSummaryText(t *testing.T) {
	err := Decode(NewBody([]byte(`{"k":1,"k":2}`), OriginPeerFrame), new(any))
	if err == nil || err.Error() != `relay: duplicate member name "k" at offset 7` {
		t.Fatalf("duplicate message %v", err)
	}
	var nilDoc *Document
	if got := fmt.Sprint(nilDoc); got != "relay.Document{}" {
		t.Fatalf("nil Document prints %q", got)
	}
	if got := fmt.Sprint(mustDoc(t, NewBody([]byte(`[1,2]`), OriginPeerFrame))); got != "relay.Document{3 values}" {
		t.Fatalf("Document prints %q", got)
	}
	if got := fmt.Sprint(Payload{}); got != "relay.Payload{unset}" {
		t.Fatalf("zero Payload prints %q", got)
	}
	b := NewBody([]byte(`{"a":1}`), OriginPeerFrame)
	p, err := Apply(b, "", EditPayorEdgeRestamp, mustDoc(t, b).Replace(at(t, mustDoc(t, b), "a"), []byte(`2`)))
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(p); got != "relay.Payload{edited [E-03] 7 bytes}" {
		t.Fatalf("edited Payload prints %q", got)
	}
	a, err := Authored(BuilderGatewayRefusal, nil, "application/json")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(a); got != "relay.Payload{authored gateway-refusal application/json 0 bytes}" {
		t.Fatalf("authored Payload prints %q", got)
	}
}
