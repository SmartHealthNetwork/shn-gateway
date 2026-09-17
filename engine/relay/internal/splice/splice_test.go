package splice

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// fixtureDir holds this package's copies of the relay fixture corpus: the
// valid hostile-but-legal documents and the documents whose syntax must be
// refused. See testdata/relayfidelity/README.md.
const fixtureDir = "testdata/relayfidelity"

func mustScan(t *testing.T, src string) *Doc {
	t.Helper()
	d, err := Scan([]byte(src), Limits{})
	if err != nil {
		t.Fatalf("Scan(%q): %v", src, err)
	}
	return d
}

func mustMember(t *testing.T, d *Doc, obj NodeID, key string) NodeID {
	t.Helper()
	n, ok := d.Member(obj, key)
	if !ok {
		t.Fatalf("member %q not found", key)
	}
	return n
}

// applyExact applies ops and asserts the exact output bytes, that the
// returned edits reproduce the output from the original bytes, and that the
// output re-scans.
func applyExact(t *testing.T, d *Doc, want string, ops ...Op) {
	t.Helper()
	out, edits, err := d.Apply(ops...)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if string(out) != want {
		t.Fatalf("Apply output\n got: %q\nwant: %q", out, want)
	}
	if got := rebuild(d.src, edits); !bytes.Equal(got, out) {
		t.Fatalf("edits do not reproduce the output\nedits: %+v\n  got: %q", edits, got)
	}
	if _, err := Scan(out, Limits{}); err != nil {
		t.Fatalf("output does not re-scan: %v", err)
	}
}

// rebuild is the independent reconstruction: original bytes outside the
// declared spans, new bytes inside them, in offset order.
func rebuild(src []byte, edits []Edit) []byte {
	var b []byte
	pos := 0
	for _, e := range edits {
		b = append(b, src[pos:e.Start]...)
		b = append(b, e.New...)
		pos = e.End
	}
	return append(b, src[pos:]...)
}

// TestScanRefusesCaseFoldedMemberNames: names equal under simple case
// folding collide, because a case-insensitive decoder (encoding/json's
// struct matching, for one) reads both into one field.
func TestScanRefusesCaseFoldedMemberNames(t *testing.T) {
	kelvin := string(rune(0x212A))
	longS := string(rune(0x017F))
	cases := []struct {
		name, src, key string
		depth          int
	}{
		{"lower then upper", `{"patient":1,"Patient":2}`, "Patient", 0},
		{"upper then lower", `{"Patient":1,"patient":2}`, "patient", 0},
		{"mixed case", `{"resourceType":"Claim","RESOURCETYPE":"Patient"}`, "RESOURCETYPE", 0},
		{"escaped fold", `{"patient":1,"` + bs + `u0050atient":2}`, "Patient", 0},
		{"Kelvin sign against k", `{"kind":1,"` + kelvin + `ind":2}`, kelvin + "ind", 0},
		{"escaped Kelvin sign against K", `{"Kind":1,"` + bs + `u212Aind":2}`, kelvin + "ind", 0},
		{"long s against S", `{"Status":1,"` + longS + `tatus":2}`, longS + "tatus", 0},
		{"deeply nested", `{"entry":[{"resource":{"a":{"b":[{"beneficiary":1,"BENEFICIARY":2}]}}}]}`, "BENEFICIARY", 6},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Scan([]byte(c.src), Limits{})
			var d *DuplicateKeyError
			if !errors.As(err, &d) || !errors.Is(err, ErrDuplicateKey) {
				t.Fatalf("want *DuplicateKeyError, got %v", err)
			}
			if d.Key != c.key || d.Depth != c.depth {
				t.Fatalf("got key %q depth %d, want %q depth %d", d.Key, d.Depth, c.key, c.depth)
			}
		})
	}
	if !strings.Contains(cases[3].src, bs+"u0050") || !strings.Contains(cases[5].src, bs+"u212A") {
		t.Fatal("the escaped rows lost their unicode escapes")
	}
	// The indexed path folds too.
	var b strings.Builder
	b.WriteString("{")
	for i := 0; i < 40; i++ {
		b.WriteString(`"k` + string(rune('a'+i%26)) + string(rune('a'+i/26)) + `":1,`)
	}
	b.WriteString(`"` + kelvin + `AA":2}`)
	_, err := Scan([]byte(b.String()), Limits{})
	var d *DuplicateKeyError
	if !errors.As(err, &d) || d.Key != kelvin+"AA" {
		t.Fatalf("large object: want a folded duplicate, got %v", err)
	}
	// Names that merely look alike are not folded together.
	mustScan(t, `{"a":1,"`+string(rune(0x0430))+`":2,"i":3,"`+string(rune(0x0131))+`":4}`)
}

func TestScanRefusesDuplicateMemberNames(t *testing.T) {
	cases := []struct {
		name  string
		src   string
		key   string
		depth int
	}{
		{"plain at depth 0", `{"a":1,"a":2}`, "a", 0},
		{"escaped at depth 0", `{"a":1,"\u0061":2}`, "a", 0},
		{"escaped both spellings", `{"\u0061":1,"a":2}`, "a", 0},
		{"surrogate-pair spelling", `{"😀":1,"\ud83d\ude00":2}`, "😀", 0},
		{"escaped at depth 5", `{"a":[{"b":{"c":[{"x":1,"\u0078":2}]}}]}`, "x", 5},
		{"after an unrelated sibling", `{"z":{"k":1},"y":{"k":2,"k":3}}`, "k", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Scan([]byte(c.src), Limits{})
			if !errors.Is(err, ErrDuplicateKey) {
				t.Fatalf("want ErrDuplicateKey, got %v", err)
			}
			var d *DuplicateKeyError
			if !errors.As(err, &d) {
				t.Fatalf("want *DuplicateKeyError, got %T", err)
			}
			if d.Key != c.key || d.Depth != c.depth {
				t.Fatalf("got key %q depth %d, want %q depth %d", d.Key, d.Depth, c.key, c.depth)
			}
			second := strings.LastIndex(c.src, `"`+strings.SplitN(c.src[d.Offset:], `"`, 3)[1]+`"`)
			if d.Offset != second {
				t.Fatalf("offset %d does not point at the second spelling (%d)", d.Offset, second)
			}
			// The escaped rows only test unescaping while the two colliding
			// spellings differ in raw bytes.
			if c.name != "plain at depth 0" && c.name != "after an unrelated sibling" {
				assertRawEscape(t, c.src, c.key)
			}
		})
	}
	// The same name in sibling objects is not a duplicate.
	mustScan(t, `{"x":[{"a":1},{"a":2}],"a":3}`)
	// Distinct names that differ only after unescaping are not duplicates.
	distinct := `{"a":1,"\u0062":2,"C":3,"\u212B":4}`
	if !strings.Contains(distinct, bs+"u0062") {
		t.Fatal("the distinct-names row lost its unicode escape")
	}
	mustScan(t, distinct)
	// A large object takes the indexed path and still refuses.
	var b strings.Builder
	b.WriteString("{")
	for i := 0; i < 40; i++ {
		b.WriteString(`"k` + string(rune('a'+i%26)) + string(rune('a'+i/26)) + `":1,`)
	}
	b.WriteString(`"k\u0061a":2}`)
	assertRawEscape(t, b.String(), "kaa")
	if _, err := Scan([]byte(b.String()), Limits{}); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("large object: want ErrDuplicateKey, got %v", err)
	}
}

// assertRawEscape fails unless src spells a name with a unicode escape and
// carries the plain spelling of key exactly once, so a duplicate refusal can
// only come from comparing names after unescaping.
func assertRawEscape(t *testing.T, src, key string) {
	t.Helper()
	if !strings.Contains(src, bs+"u") {
		t.Fatalf("row carries no unicode escape: %q", src)
	}
	if n := strings.Count(src, `"`+key+`"`); n != 1 {
		t.Fatalf("plain spelling %q occurs %d times; the colliding names must differ in raw bytes", key, n)
	}
}

func TestScanRefusesMalformedDocuments(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want error
	}{
		{"trailing document", `[1] [2]`, ErrTrailingData},
		{"trailing scalar", `{} 1`, ErrTrailingData},
		{"trailing garbage", "\"a\"\n,", ErrTrailingData},
		{"invalid UTF-8 in a string", "[\"a\xffb\"]", ErrInvalidUTF8},
		{"invalid UTF-8 in a key", "{\"\xc3\":1}", ErrInvalidUTF8},
		{"surrogate encoded as UTF-8", "[\"\xed\xa0\x80\"]", ErrInvalidUTF8},
		{"overlong UTF-8", "[\"\xc0\xaf\"]", ErrInvalidUTF8},
		{"invalid UTF-8 outside a string", "[1,\xff]", ErrSyntax},
		{"lone high surrogate", `["\ud800"]`, ErrLoneSurrogate},
		{"lone high surrogate then text", `["\ud800x"]`, ErrLoneSurrogate},
		{"lone low surrogate", `["\udc00"]`, ErrLoneSurrogate},
		{"high then non-low escape", `["\ud800\u0041"]`, ErrLoneSurrogate},
		{"high then high", `["\ud800\ud800"]`, ErrLoneSurrogate},
		{"lone surrogate in a key", `{"\udfff":1}`, ErrLoneSurrogate},
		{"empty input", ``, ErrSyntax},
		{"whitespace only", " \n", ErrSyntax},
		{"unclosed object", `{`, ErrSyntax},
		{"unclosed array", `[1`, ErrSyntax},
		{"member without value", `{"a"}`, ErrSyntax},
		{"member without colon", `{"a" 1}`, ErrSyntax},
		{"non-string key", `{1:1}`, ErrSyntax},
		{"trailing comma in array", `[1,]`, ErrSyntax},
		{"trailing comma in object", `{"a":1,}`, ErrSyntax},
		{"leading comma", `[,1]`, ErrSyntax},
		{"missing comma", `[1 2]`, ErrSyntax},
		{"mismatched close", `[1}`, ErrSyntax},
		{"leading zero at top level", `01`, ErrTrailingData},
		{"leading zero in an array", `[01]`, ErrSyntax},
		{"negative leading zero", `[-01]`, ErrSyntax},
		{"bare minus", `-`, ErrSyntax},
		{"dangling fraction", `1.`, ErrSyntax},
		{"dangling exponent", `1e`, ErrSyntax},
		{"signed dangling exponent", `1e+`, ErrSyntax},
		{"plus sign", `+1`, ErrSyntax},
		{"leading dot", `.5`, ErrSyntax},
		{"NaN", `NaN`, ErrSyntax},
		{"truncated literal", `tru`, ErrSyntax},
		{"wrong literal", `nul1`, ErrSyntax},
		{"unterminated string", `"abc`, ErrSyntax},
		{"raw control character", "\"a\x01\"", ErrSyntax},
		{"raw newline in string", "\"a\nb\"", ErrSyntax},
		{"bad escape", `"\q"`, ErrSyntax},
		{"short unicode escape", `"\u12"`, ErrSyntax},
		{"non-hex unicode escape", `"\u12g4"`, ErrSyntax},
		{"escape at end", `"\`, ErrSyntax},
		{"single quotes", `['a']`, ErrSyntax},
		{"comment", `[1 /* x */]`, ErrSyntax},
		{"form feed is not whitespace", "\f[1]", ErrSyntax},
		{"byte order mark", "\xef\xbb\xbf[1]", ErrSyntax},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := Scan([]byte(c.src), Limits{})
			if !errors.Is(err, c.want) {
				t.Fatalf("want %v, got %v (doc %v)", c.want, err, d)
			}
			var se *ScanError
			if !errors.As(err, &se) {
				t.Fatalf("want *ScanError, got %T", err)
			}
			if se.Offset < 0 || se.Offset > len(c.src) {
				t.Fatalf("offset %d outside the input", se.Offset)
			}
		})
	}
}

func TestScanAcceptsLegalDocuments(t *testing.T) {
	for _, src := range []string{
		`0`, `-0`, `-0.0`, `1.50`, `9007199254740993`, `1e2`, `1E+2`, `-1.5e-10`,
		`true`, `false`, `null`, `""`, `"a"`, `[]`, `{}`, " \t\r\n[ ] \t\r\n",
		`["\ud83d\ude00"]`, `["\uD83D\uDE00"]`, `["😀é"]`, `["\"\\\/\b\f\n\r\t"]`,
		`{"a":{"b":[1,{"c":null}]}}`, `["\\ud800"]`, `["\u0000"]`, `["` + "\x7f" + `"]`,
	} {
		if _, err := Scan([]byte(src), Limits{}); err != nil {
			t.Errorf("Scan(%q): %v", src, err)
		}
	}
}

func nested(depth int) string {
	return strings.Repeat("[", depth) + strings.Repeat("]", depth)
}

func TestScanLimits(t *testing.T) {
	if got := DefaultLimits(); got != (Limits{MaxDepth: 64, MaxTokens: 1_000_000, MaxBytes: 16 << 20}) {
		t.Fatalf("DefaultLimits = %+v", got)
	}

	// Depth: 64 nested containers are accepted, 65 are refused, for arrays,
	// objects and a mix.
	if _, err := Scan([]byte(nested(64)), Limits{}); err != nil {
		t.Fatalf("depth 64: %v", err)
	}
	if _, err := Scan([]byte(nested(65)), Limits{}); !errors.Is(err, ErrDepthLimit) {
		t.Fatalf("depth 65: want ErrDepthLimit, got %v", err)
	}
	obj := func(depth int) string {
		return strings.Repeat(`{"a":`, depth-1) + `{}` + strings.Repeat(`}`, depth-1)
	}
	if _, err := Scan([]byte(obj(64)), Limits{}); err != nil {
		t.Fatalf("object depth 64: %v", err)
	}
	if _, err := Scan([]byte(obj(65)), Limits{}); !errors.Is(err, ErrDepthLimit) {
		t.Fatalf("object depth 65: want ErrDepthLimit, got %v", err)
	}
	if _, err := Scan([]byte(nested(3)), Limits{MaxDepth: 2}); !errors.Is(err, ErrDepthLimit) {
		t.Fatalf("custom depth: want ErrDepthLimit, got %v", err)
	}
	// Deep input far past the limit is refused without exhausting the stack.
	if _, err := Scan([]byte(strings.Repeat("[", 1<<20)), Limits{}); !errors.Is(err, ErrDepthLimit) {
		t.Fatalf("very deep: want ErrDepthLimit, got %v", err)
	}

	// Tokens: every value and every member name is one token.
	if _, err := Scan([]byte(`[1,2,3]`), Limits{MaxTokens: 4}); err != nil {
		t.Fatalf("4 tokens under limit 4: %v", err)
	}
	if _, err := Scan([]byte(`[1,2,3]`), Limits{MaxTokens: 3}); !errors.Is(err, ErrTokenLimit) {
		t.Fatalf("4 tokens under limit 3: want ErrTokenLimit, got %v", err)
	}
	if _, err := Scan([]byte(`{"a":1}`), Limits{MaxTokens: 3}); err != nil {
		t.Fatalf("3 tokens under limit 3: %v", err)
	}
	if _, err := Scan([]byte(`{"a":1}`), Limits{MaxTokens: 2}); !errors.Is(err, ErrTokenLimit) {
		t.Fatalf("3 tokens under limit 2: want ErrTokenLimit, got %v", err)
	}
	if _, err := Scan([]byte("["+strings.Repeat("0,", 1_000_000)+"0]"), Limits{}); !errors.Is(err, ErrTokenLimit) {
		t.Fatalf("default token limit: want ErrTokenLimit, got %v", err)
	}

	// Bytes.
	if _, err := Scan([]byte(`[1,2]`), Limits{MaxBytes: 5}); err != nil {
		t.Fatalf("5 bytes under limit 5: %v", err)
	}
	if _, err := Scan([]byte(`[1,2] `), Limits{MaxBytes: 5}); !errors.Is(err, ErrSizeLimit) {
		t.Fatalf("6 bytes under limit 5: want ErrSizeLimit, got %v", err)
	}
	big := make([]byte, 16<<20+1)
	for i := range big {
		big[i] = ' '
	}
	big[0] = '0'
	if _, err := Scan(big, Limits{}); !errors.Is(err, ErrSizeLimit) {
		t.Fatalf("default size limit: want ErrSizeLimit, got %v", err)
	}
	if _, err := Scan(big[:16<<20], Limits{}); err != nil {
		t.Fatalf("exactly 16 MiB: %v", err)
	}
}

func TestNavigation(t *testing.T) {
	src := `{"res\u006furce":{"n":1.50},"list":[true,false,null,"s",-0.0],"e":{}}`
	d := mustScan(t, src)
	root := d.Root()
	if d.Kind(root) != KindObject {
		t.Fatalf("root kind %v", d.Kind(root))
	}
	if s, e := d.Span(root); s != 0 || e != len(src) {
		t.Fatalf("root span %d,%d", s, e)
	}
	// Member compares decoded names, so the escaped spelling is found by its
	// plain name and not by its raw spelling.
	res := mustMember(t, d, root, "resource")
	if _, ok := d.Member(root, `res\u006furce`); ok {
		t.Fatal("Member matched a raw escaped spelling")
	}
	if _, ok := d.Member(root, "missing"); ok {
		t.Fatal("Member found a missing key")
	}
	n := mustMember(t, d, res, "n")
	if s, e := d.Span(n); src[s:e] != "1.50" || d.Kind(n) != KindNumber {
		t.Fatalf("number span %q kind %v", src[s:e], d.Kind(n))
	}
	list := mustMember(t, d, root, "list")
	elems := d.Elems(list)
	wantKinds := []Kind{KindBool, KindBool, KindNull, KindString, KindNumber}
	if len(elems) != len(wantKinds) {
		t.Fatalf("elems %v", elems)
	}
	for i, el := range elems {
		if d.Kind(el) != wantKinds[i] {
			t.Errorf("elem %d kind %v want %v", i, d.Kind(el), wantKinds[i])
		}
	}
	if s, e := d.Span(elems[4]); src[s:e] != "-0.0" {
		t.Fatalf("elem 4 span %q", src[s:e])
	}
	if d.Elems(root) != nil || d.Elems(NodeID(-1)) != nil || d.Elems(NodeID(1000)) != nil {
		t.Fatal("Elems on a non-array must be nil")
	}
	if _, ok := d.Member(list, "x"); ok {
		t.Fatal("Member on an array")
	}
	if _, ok := d.Member(NodeID(-3), "x"); ok {
		t.Fatal("Member on an invalid node")
	}
	if k := d.Kind(NodeID(1000)); k != 0 {
		t.Fatalf("Kind of an invalid node = %v", k)
	}
	if s, e := d.Span(NodeID(1000)); s != -1 || e != -1 {
		t.Fatalf("Span of an invalid node = %d,%d", s, e)
	}
	empty := mustMember(t, d, root, "e")
	if d.Kind(empty) != KindObject {
		t.Fatal("empty object kind")
	}
	if len(d.Elems(mustMember(t, mustScan(t, `{"a":[]}`), 0, "a"))) != 0 {
		t.Fatal("empty array has elements")
	}
	for k, want := range map[Kind]string{KindObject: "object", KindArray: "array", KindString: "string", KindNumber: "number", KindBool: "bool", KindNull: "null", 0: "invalid"} {
		if k.String() != want {
			t.Errorf("Kind(%d).String() = %q", k, k.String())
		}
	}
}

func TestStringValue(t *testing.T) {
	cases := []struct{ src, want string }{
		{`"plain"`, "plain"},
		{`""`, ""},
		{`"a\u00e9\ud83d\ude00\n\"\\\/\b\f\r\t"`, "aé\U0001F600\n\"\\/\b\f\r\t"},
		{`"\uD83D\uDE00x"`, "\U0001F600x"},
		{`"é😀<>&"`, "é😀<>&"},
		{`"\u0000"`, "\x00"},
		{`"\\ud800"`, `\ud800`},
	}
	for _, c := range cases {
		d := mustScan(t, c.src)
		got, err := d.StringValue(d.Root())
		if err != nil || got != c.want {
			t.Errorf("StringValue(%s) = %q, %v; want %q", c.src, got, err, c.want)
		}
	}
	d := mustScan(t, `[1]`)
	if _, err := d.StringValue(d.Root()); !errors.Is(err, ErrInvalidOp) {
		t.Fatalf("StringValue on an array: want ErrInvalidOp, got %v", err)
	}
	if _, err := d.StringValue(NodeID(9)); !errors.Is(err, ErrInvalidOp) {
		t.Fatalf("StringValue on an invalid node: want ErrInvalidOp, got %v", err)
	}
}

func TestRemoveMember(t *testing.T) {
	type row struct {
		name, src, key, want string
	}
	styles := []struct {
		name string
		src  string // three members a, b, c
		noA  string
		noB  string
		noC  string
		only string // a single member a
	}{
		{
			name: "compact",
			src:  `{"a":1,"b":2,"c":3}`,
			noA:  `{"b":2,"c":3}`,
			noB:  `{"a":1,"c":3}`,
			noC:  `{"a":1,"b":2}`,
			only: `{"a":1}`,
		},
		{
			name: "single-line spaced",
			src:  `{ "a" : 1 , "b" : 2 , "c" : 3 }`,
			noA:  `{ "b" : 2 , "c" : 3 }`,
			noB:  `{ "a" : 1 , "c" : 3 }`,
			noC:  `{ "a" : 1 , "b" : 2 }`,
			only: `{ "a" : 1 }`,
		},
		{
			name: "multi-line",
			src:  "{\n  \"a\": 1,\n  \"b\": 2,\n  \"c\": 3\n}",
			noA:  "{\n  \"b\": 2,\n  \"c\": 3\n}",
			noB:  "{\n  \"a\": 1,\n  \"c\": 3\n}",
			noC:  "{\n  \"a\": 1,\n  \"b\": 2\n}",
			only: "{\n  \"a\": 1\n}",
		},
		{
			name: "CRLF and tab",
			src:  "{\r\n\t\"a\": 1,\r\n\t\"b\": 2,\r\n\t\"c\": 3\r\n}",
			noA:  "{\r\n\t\"b\": 2,\r\n\t\"c\": 3\r\n}",
			noB:  "{\r\n\t\"a\": 1,\r\n\t\"c\": 3\r\n}",
			noC:  "{\r\n\t\"a\": 1,\r\n\t\"b\": 2\r\n}",
			only: "{\r\n\t\"a\": 1\r\n}",
		},
		{
			name: "tab separated",
			src:  "{\t\"a\":\t1,\t\"b\":\t2,\t\"c\":\t3}",
			noA:  "{\t\"b\":\t2,\t\"c\":\t3}",
			noB:  "{\t\"a\":\t1,\t\"c\":\t3}",
			noC:  "{\t\"a\":\t1,\t\"b\":\t2}",
			only: "{\t\"a\":\t1}",
		},
		{
			name: "container values",
			src:  "{\n  \"a\": {\"x\": [1]},\n  \"b\": [\n    {}\n  ],\n  \"c\": {\n    \"y\": null\n  }\n}",
			noA:  "{\n  \"b\": [\n    {}\n  ],\n  \"c\": {\n    \"y\": null\n  }\n}",
			noB:  "{\n  \"a\": {\"x\": [1]},\n  \"c\": {\n    \"y\": null\n  }\n}",
			noC:  "{\n  \"a\": {\"x\": [1]},\n  \"b\": [\n    {}\n  ]\n}",
			only: "{\n  \"a\": {\"x\": [1]}\n}",
		},
	}
	var rows []row
	for _, s := range styles {
		rows = append(rows,
			row{s.name + "/first", s.src, "a", s.noA},
			row{s.name + "/middle", s.src, "b", s.noB},
			row{s.name + "/last", s.src, "c", s.noC},
			row{s.name + "/only", s.only, "a", "{}"},
		)
	}
	// An escaped member name is removed by its decoded name.
	rows = append(rows, row{"escaped name", `{"\u0061":1,"b":2}`, "a", `{"b":2}`})
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			if r.name == "escaped name" && (!strings.Contains(r.src, bs+"u0061") || strings.Contains(r.src, `"a"`)) {
				t.Fatalf("the escaped-name row lost its unicode escape: %q", r.src)
			}
			d := mustScan(t, r.src)
			applyExact(t, d, r.want, RemoveMember(d.Root(), r.key))
		})
	}

	t.Run("several in one object", func(t *testing.T) {
		src := "{\n  \"a\": 1,\n  \"b\": 2,\n  \"c\": 3,\n  \"d\": 4\n}"
		for _, c := range []struct {
			keys []string
			want string
		}{
			{[]string{"a", "b"}, "{\n  \"c\": 3,\n  \"d\": 4\n}"},
			{[]string{"b", "c"}, "{\n  \"a\": 1,\n  \"d\": 4\n}"},
			{[]string{"c", "d"}, "{\n  \"a\": 1,\n  \"b\": 2\n}"},
			{[]string{"a", "d"}, "{\n  \"b\": 2,\n  \"c\": 3\n}"},
			{[]string{"a", "c"}, "{\n  \"b\": 2,\n  \"d\": 4\n}"},
			{[]string{"b", "d"}, "{\n  \"a\": 1,\n  \"c\": 3\n}"},
			{[]string{"d", "a", "c"}, "{\n  \"b\": 2\n}"},
			{[]string{"a", "b", "c", "d"}, "{}"},
		} {
			d := mustScan(t, src)
			var ops []Op
			for _, k := range c.keys {
				ops = append(ops, RemoveMember(d.Root(), k))
			}
			applyExact(t, d, c.want, ops...)
		}
	})

	t.Run("nested object", func(t *testing.T) {
		src := `{"outer":{"a":1,"b":2},"z":0}`
		d := mustScan(t, src)
		outer := mustMember(t, d, d.Root(), "outer")
		applyExact(t, d, `{"outer":{"a":1},"z":0}`, RemoveMember(outer, "b"))
	})

	t.Run("refusals", func(t *testing.T) {
		d := mustScan(t, `{"a":{"b":1},"l":[1]}`)
		a := mustMember(t, d, d.Root(), "a")
		l := mustMember(t, d, d.Root(), "l")
		for _, c := range []struct {
			name string
			ops  []Op
			want error
		}{
			{"missing key", []Op{RemoveMember(d.Root(), "zz")}, ErrNoMember},
			{"not an object", []Op{RemoveMember(l, "a")}, ErrInvalidOp},
			{"invalid node", []Op{RemoveMember(NodeID(99), "a")}, ErrInvalidOp},
			{"removed twice", []Op{RemoveMember(d.Root(), "a"), RemoveMember(d.Root(), "a")}, ErrOverlap},
			{"edit inside a removed member", []Op{RemoveMember(d.Root(), "a"), RemoveMember(a, "b")}, ErrOverlap},
			{"replace inside a removed member", []Op{RemoveMember(d.Root(), "a"), Replace(mustMember(t, d, a, "b"), []byte(`2`))}, ErrOverlap},
		} {
			if _, _, err := d.Apply(c.ops...); !errors.Is(err, c.want) {
				t.Errorf("%s: want %v, got %v", c.name, c.want, err)
			}
		}
	})
}

func TestInsertMember(t *testing.T) {
	cases := []struct {
		name, src, want string
		kvs             [][2]string
	}{
		{"empty object", `{}`, `{"k":"v"}`, [][2]string{{"k", `"v"`}}},
		{"empty object, two members", `{}`, `{"k":"v","n":1}`, [][2]string{{"k", `"v"`}, {"n", `1`}}},
		{"empty object with whitespace", "{\n}", "{\n\"k\":\"v\"}", [][2]string{{"k", `"v"`}}},
		{"compact", `{"a":1}`, `{"a":1,"k":{"x":[1.50]}}`, [][2]string{{"k", `{"x":[1.50]}`}}},
		{"single-line spaced is written compact", `{ "a": 1 }`, `{ "a": 1,"k":true }`, [][2]string{{"k", `true`}}},
		{
			"multi-line",
			"{\n  \"a\": 1,\n  \"b\": 2\n}",
			"{\n  \"a\": 1,\n  \"b\": 2,\n  \"k\": \"v\",\n  \"m\": null\n}",
			[][2]string{{"k", `"v"`}, {"m", `null`}},
		},
		{
			"multi-line CRLF, tab and spaced colon",
			"{\r\n\t\"a\" : 1\r\n}",
			"{\r\n\t\"a\" : 1,\r\n\t\"k\" : \"v\"\r\n}",
			[][2]string{{"k", `"v"`}},
		},
		{
			"multi-line with a container last value",
			"{\n  \"a\": {\n    \"x\": 1\n  }\n}",
			"{\n  \"a\": {\n    \"x\": 1\n  },\n  \"k\": []\n}",
			[][2]string{{"k", `[]`}},
		},
		{"key escaping", `{}`, `{"q\"\\\n\u0001é":0}`, [][2]string{{"q\"\\\n\x01é", `0`}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := mustScan(t, c.src)
			var ops []Op
			for _, kv := range c.kvs {
				ops = append(ops, InsertMember(d.Root(), kv[0], []byte(kv[1])))
			}
			applyExact(t, d, c.want, ops...)
		})
	}

	t.Run("inserted key round-trips through Member", func(t *testing.T) {
		d := mustScan(t, `{}`)
		out, _, err := d.Apply(InsertMember(d.Root(), "q\"\\\n\x01é", []byte(`0`)))
		if err != nil {
			t.Fatal(err)
		}
		mustMember(t, mustScan(t, string(out)), 0, "q\"\\\n\x01é")
	})

	t.Run("refusals", func(t *testing.T) {
		d := mustScan(t, `{"a":1,"l":[]}`)
		l := mustMember(t, d, d.Root(), "l")
		for _, c := range []struct {
			name string
			ops  []Op
			want error
		}{
			{"existing key", []Op{InsertMember(d.Root(), "a", []byte(`2`))}, ErrDuplicateKey},
			{"same key twice", []Op{InsertMember(d.Root(), "k", []byte(`2`)), InsertMember(d.Root(), "k", []byte(`3`))}, ErrDuplicateKey},
			{"not an object", []Op{InsertMember(l, "k", []byte(`2`))}, ErrInvalidOp},
			{"invalid node", []Op{InsertMember(NodeID(40), "k", []byte(`2`))}, ErrInvalidOp},
			{"undefined handle", []Op{InsertMember(NodeID(-7), "k", []byte(`2`))}, ErrInvalidOp},
			{"invalid UTF-8 key", []Op{InsertMember(d.Root(), "\xff", []byte(`2`))}, ErrInvalidUTF8},
			{"invalid value", []Op{InsertMember(d.Root(), "k", []byte(`2 3`))}, ErrInvalidValue},
			{"empty value", []Op{InsertMember(d.Root(), "k", nil)}, ErrInvalidValue},
			{"insert into a replaced object", []Op{Replace(d.Root(), []byte(`{}`)), InsertMember(d.Root(), "k", []byte(`1`))}, ErrOverlap},
		} {
			if _, _, err := d.Apply(c.ops...); !errors.Is(err, c.want) {
				t.Errorf("%s: want %v, got %v", c.name, c.want, err)
			}
		}
	})
}

func TestRemoveAndInsertInOneObject(t *testing.T) {
	cases := []struct {
		name, src, want string
		ops             func(d *Doc) []Op
	}{
		{
			"remove last, insert",
			"{\n  \"a\": 1,\n  \"b\": 2\n}",
			"{\n  \"a\": 1,\n  \"c\": 3\n}",
			func(d *Doc) []Op {
				return []Op{InsertMember(d.Root(), "c", []byte(`3`)), RemoveMember(d.Root(), "b")}
			},
		},
		{
			"remove first, insert",
			"{\n  \"a\": 1,\n  \"b\": 2\n}",
			"{\n  \"b\": 2,\n  \"c\": 3\n}",
			func(d *Doc) []Op {
				return []Op{RemoveMember(d.Root(), "a"), InsertMember(d.Root(), "c", []byte(`3`))}
			},
		},
		{
			"remove and re-insert the same name moves it last",
			`{"a":1,"b":2}`,
			`{"b":2,"a":9}`,
			func(d *Doc) []Op {
				return []Op{InsertMember(d.Root(), "a", []byte(`9`)), RemoveMember(d.Root(), "a")}
			},
		},
		{
			"remove the only member, insert (multi-line)",
			"{\n  \"a\": 1\n}",
			"{\n  \"b\": 2,\n  \"c\": 3\n}",
			func(d *Doc) []Op {
				return []Op{RemoveMember(d.Root(), "a"), InsertMember(d.Root(), "b", []byte(`2`)), InsertMember(d.Root(), "c", []byte(`3`))}
			},
		},
		{
			"remove the only member, insert (compact)",
			`{"a":1}`,
			`{"b":2}`,
			func(d *Doc) []Op {
				return []Op{RemoveMember(d.Root(), "a"), InsertMember(d.Root(), "b", []byte(`2`))}
			},
		},
		{
			"remove all, ensure",
			"{\n\t\"a\": 1,\n\t\"b\": 2\n}",
			"{\n\t\"p\": {\"x\":1}\n}",
			func(d *Doc) []Op {
				e, h := EnsureObjectMember(d.Root(), "p")
				return []Op{RemoveMember(d.Root(), "a"), RemoveMember(d.Root(), "b"), e, InsertMember(NodeID(h), "x", []byte(`1`))}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := mustScan(t, c.src)
			applyExact(t, d, c.want, c.ops(d)...)
		})
	}
}

func TestEnsureObjectMember(t *testing.T) {
	t.Run("absent parent composed with inserts", func(t *testing.T) {
		src := "{\n  \"hook\": \"order-sign\",\n  \"context\": {}\n}"
		d := mustScan(t, src)
		ens, h := EnsureObjectMember(d.Root(), "prefetch")
		applyExact(t, d,
			"{\n  \"hook\": \"order-sign\",\n  \"context\": {},\n  \"prefetch\": {\"patient\":{\"resourceType\":\"Patient\"},\"coverage\":null}\n}",
			ens,
			InsertMember(NodeID(h), "patient", []byte(`{"resourceType":"Patient"}`)),
			InsertMember(NodeID(h), "coverage", []byte(`null`)),
		)
	})
	t.Run("absent parent alone", func(t *testing.T) {
		d := mustScan(t, `{"hook":"x"}`)
		ens, _ := EnsureObjectMember(d.Root(), "prefetch")
		applyExact(t, d, `{"hook":"x","prefetch":{}}`, ens)
	})
	t.Run("absent parent in an empty object", func(t *testing.T) {
		d := mustScan(t, `{}`)
		ens, h := EnsureObjectMember(d.Root(), "p")
		applyExact(t, d, `{"p":{"k":"v"}}`, ens, InsertMember(NodeID(h), "k", []byte(`"v"`)))
	})
	t.Run("nested absent parents", func(t *testing.T) {
		d := mustScan(t, `{"a":1}`)
		e1, h1 := EnsureObjectMember(d.Root(), "p")
		e2, h2 := EnsureObjectMember(NodeID(h1), "q")
		applyExact(t, d, `{"a":1,"p":{"q":{"k":true},"z":0}}`,
			e1, e2,
			InsertMember(NodeID(h2), "k", []byte(`true`)),
			InsertMember(NodeID(h1), "z", []byte(`0`)),
		)
	})
	t.Run("ensured twice resolves to one member", func(t *testing.T) {
		d := mustScan(t, `{}`)
		e1, h1 := EnsureObjectMember(d.Root(), "p")
		e2, h2 := EnsureObjectMember(d.Root(), "p")
		if h1 == h2 {
			t.Fatal("handles must be distinct")
		}
		applyExact(t, d, `{"p":{"a":1,"b":2}}`,
			e1, e2,
			InsertMember(NodeID(h1), "a", []byte(`1`)),
			InsertMember(NodeID(h2), "b", []byte(`2`)),
		)
	})
	t.Run("existing parent resolves to the existing node", func(t *testing.T) {
		src := "{\n  \"pre\\u0066etch\": {\n    \"a\": 1\n  }\n}"
		d := mustScan(t, src)
		ens, h := EnsureObjectMember(d.Root(), "prefetch")
		applyExact(t, d,
			"{\n  \"pre\\u0066etch\": {\n    \"a\": 1,\n    \"b\": 2\n  }\n}",
			ens, InsertMember(NodeID(h), "b", []byte(`2`)),
		)
		// Alone, an existing parent is no edit at all.
		out, edits, err := d.Apply(ens)
		if err != nil || string(out) != src || len(edits) != 0 {
			t.Fatalf("ensure of an existing member: %q %v %v", out, edits, err)
		}
		// The resolved handle also works for removal.
		applyExact(t, d, "{\n  \"pre\\u0066etch\": {}\n}", ens, RemoveMember(NodeID(h), "a"))
	})
	t.Run("refusals", func(t *testing.T) {
		d := mustScan(t, `{"s":"x","o":{"k":1},"l":[]}`)
		o := mustMember(t, d, d.Root(), "o")
		eS, _ := EnsureObjectMember(d.Root(), "s")
		eL, hL := EnsureObjectMember(mustMember(t, d, d.Root(), "l"), "p")
		eO, hO := EnsureObjectMember(d.Root(), "o")
		eN, hN := EnsureObjectMember(d.Root(), "new")
		eBad, _ := EnsureObjectMember(NodeID(77), "p")
		_, hLate := EnsureObjectMember(d.Root(), "late")
		for _, c := range []struct {
			name string
			ops  []Op
			want error
		}{
			{"existing non-object", []Op{eS}, ErrInvalidOp},
			{"parent not an object", []Op{eL, InsertMember(NodeID(hL), "k", []byte(`1`))}, ErrInvalidOp},
			{"invalid parent", []Op{eBad}, ErrInvalidOp},
			{"handle used before its ensure", []Op{InsertMember(NodeID(hN), "k", []byte(`1`)), eN}, ErrInvalidOp},
			{"handle from another op list", []Op{InsertMember(NodeID(hLate), "k", []byte(`1`))}, ErrInvalidOp},
			{"duplicate insert into a created member", []Op{eN, InsertMember(NodeID(hN), "k", []byte(`1`)), InsertMember(NodeID(hN), "k", []byte(`2`))}, ErrDuplicateKey},
			{"ensure collides with an insert", []Op{InsertMember(d.Root(), "new", []byte(`1`)), eN}, ErrDuplicateKey},
			{"insert collides with an ensure", []Op{eN, InsertMember(d.Root(), "new", []byte(`1`))}, ErrDuplicateKey},
			{"replace a created member", []Op{eN, Replace(NodeID(hN), []byte(`1`))}, ErrInvalidOp},
			{"append to a created member", []Op{eN, AppendElement(NodeID(hN), []byte(`1`))}, ErrInvalidOp},
			{"remove from a created member", []Op{eN, RemoveMember(NodeID(hN), "k")}, ErrInvalidOp},
			{"ensure a member that is removed", []Op{RemoveMember(d.Root(), "o"), eO}, ErrOverlap},
			{"insert into an existing member that is removed", []Op{eO, InsertMember(NodeID(hO), "z", []byte(`1`)), RemoveMember(d.Root(), "o")}, ErrOverlap},
			{"insert into an existing member that is replaced", []Op{eO, InsertMember(NodeID(hO), "z", []byte(`1`)), Replace(o, []byte(`{}`))}, ErrOverlap},
		} {
			if _, _, err := d.Apply(c.ops...); !errors.Is(err, c.want) {
				t.Errorf("%s: want %v, got %v", c.name, c.want, err)
			}
		}
	})
}

func TestEnsureResolvedNodeIsClaimed(t *testing.T) {
	d := mustScan(t, `{"o":{"k":"v"},"s":1}`)
	o := mustMember(t, d, d.Root(), "o")
	k := mustMember(t, d, o, "k")
	eO, hO := EnsureObjectMember(d.Root(), "o")
	eO2, hO2 := EnsureObjectMember(d.Root(), "o")

	// Edits inside or beside the resolved node are fine, and two ensures of
	// the same existing member do not collide with each other.
	applyExact(t, d, `{"o":{"k":"w","a":1,"b":2},"s":2}`,
		eO, eO2,
		Replace(k, []byte(`"w"`)),
		InsertMember(NodeID(hO), "a", []byte(`1`)),
		InsertMember(NodeID(hO2), "b", []byte(`2`)),
		Replace(mustMember(t, d, d.Root(), "s"), []byte(`2`)),
	)

	nested := mustScan(t, `{"a":{"x":{}},"b":2}`)
	a := mustMember(t, nested, nested.Root(), "a")
	eX, hX := EnsureObjectMember(a, "x")
	for _, c := range []struct {
		name string
		doc  *Doc
		ops  []Op
	}{
		{"replace the resolved node", d, []Op{eO, Replace(o, []byte(`1`))}},
		{"replace the resolved node before the ensure", d, []Op{Replace(o, []byte(`1`)), eO}},
		{"replace an enclosing value", d, []Op{eO, Replace(d.Root(), []byte(`{}`))}},
		{"remove the resolved member", d, []Op{eO, RemoveMember(d.Root(), "o")}},
		{"remove an ancestor member", nested, []Op{RemoveMember(nested.Root(), "a"), eX}},
		{"remove an ancestor member, with an insert", nested, []Op{eX, InsertMember(NodeID(hX), "k", []byte(`1`)), RemoveMember(nested.Root(), "a")}},
		{"remove every member of the parent", nested, []Op{RemoveMember(nested.Root(), "a"), RemoveMember(nested.Root(), "b"), eX}},
		{"replace an ancestor", nested, []Op{eX, Replace(a, []byte(`{}`))}},
	} {
		if _, _, err := c.doc.Apply(c.ops...); !errors.Is(err, ErrOverlap) {
			t.Errorf("%s: want ErrOverlap, got %v", c.name, err)
		}
	}
	// Alone, the claim emits nothing.
	out, edits, err := nested.Apply(eX)
	if err != nil || string(out) != `{"a":{"x":{}},"b":2}` || len(edits) != 0 {
		t.Fatalf("claim alone: %q %v %v", out, edits, err)
	}
}

func TestEnumeration(t *testing.T) {
	src := `{"b":[1,{"c":"d"}],"a":{},"e":null}`
	d := mustScan(t, src)
	if d.Len() != 7 {
		t.Fatalf("Len = %d, want 7", d.Len())
	}
	// Dense ids in document order.
	wantStarts := []int{0, 5, 6, 8, 13, 23, 30}
	for i := 0; i < d.Len(); i++ {
		s, e := d.Span(NodeID(i))
		if s != wantStarts[i] {
			t.Errorf("node %d starts at %d, want %d", i, s, wantStarts[i])
		}
		if !(s < e) {
			t.Errorf("node %d empty span", i)
		}
	}
	if d.Kind(NodeID(d.Len())) != 0 || d.Kind(NodeID(d.Len()-1)) != KindNull {
		t.Fatal("ids beyond Len must be invalid")
	}

	ms := d.Members(d.Root())
	wantNames := []string{"b", "a", "e"}
	if len(ms) != 3 {
		t.Fatalf("Members = %+v", ms)
	}
	for i, m := range ms {
		if m.Name != wantNames[i] {
			t.Errorf("member %d name %q, want %q", i, m.Name, wantNames[i])
		}
		if src[m.KeyStart:m.KeyEnd] != `"`+wantNames[i]+`"` {
			t.Errorf("member %d key span %q", i, src[m.KeyStart:m.KeyEnd])
		}
		if v, _ := d.Member(d.Root(), m.Name); v != m.Value {
			t.Errorf("member %d value %d, Member says %d", i, m.Value, v)
		}
	}
	ms[0].Name = "changed"
	if d.Members(d.Root())[0].Name != "b" {
		t.Fatal("Members must return a copy")
	}
	esc := mustScan(t, `{"x`+bs+`u0079":1}`)
	if m := esc.Members(esc.Root()); len(m) != 1 || m[0].Name != "xy" || m[0].KeyEnd-m[0].KeyStart != 9 {
		t.Fatalf("escaped member %+v", m)
	}
	if got := d.Members(mustMember(t, d, d.Root(), "a")); got == nil || len(got) != 0 {
		t.Fatalf("empty object members %v", got)
	}
	if d.Members(mustMember(t, d, d.Root(), "b")) != nil || d.Members(NodeID(-1)) != nil || d.Members(NodeID(99)) != nil {
		t.Fatal("Members on a non-object must be nil")
	}
}

func TestAppendElement(t *testing.T) {
	cases := []struct {
		name, src, want string
		vals            []string
	}{
		{"empty array", `[]`, `[{"a":1}]`, []string{`{"a":1}`}},
		{"empty array, two values", `[]`, `[1,"x"]`, []string{`1`, `"x"`}},
		{"empty array with whitespace", "[\n]", "[\n1]", []string{`1`}},
		{"compact", `[1,2]`, `[1,2,3]`, []string{`3`}},
		{"single-line spaced is written compact", `[1, 2]`, `[1, 2,3]`, []string{`3`}},
		{"multi-line", "[\n  1,\n  2\n]", "[\n  1,\n  2,\n  3,\n  4\n]", []string{`3`, `4`}},
		{"multi-line CRLF tab", "[\r\n\t{\r\n\t\t\"a\": 1\r\n\t}\r\n]", "[\r\n\t{\r\n\t\t\"a\": 1\r\n\t},\r\n\t{\"b\":2}\r\n]", []string{`{"b":2}`}},
		{"single element on its own line", "[\n    \"x\"\n  ]", "[\n    \"x\",\n    \"y\"\n  ]", []string{`"y"`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := mustScan(t, c.src)
			var ops []Op
			for _, v := range c.vals {
				ops = append(ops, AppendElement(d.Root(), []byte(v)))
			}
			applyExact(t, d, c.want, ops...)
		})
	}
	t.Run("nested array alongside an edit to its last element", func(t *testing.T) {
		d := mustScan(t, `{"l":["a","b"]}`)
		l := mustMember(t, d, d.Root(), "l")
		applyExact(t, d, `{"l":["a","B","c"]}`, AppendElement(l, []byte(`"c"`)), Replace(d.Elems(l)[1], []byte(`"B"`)))
	})
	t.Run("refusals", func(t *testing.T) {
		d := mustScan(t, `{"l":[[1]]}`)
		l := mustMember(t, d, d.Root(), "l")
		for _, c := range []struct {
			name string
			ops  []Op
			want error
		}{
			{"not an array", []Op{AppendElement(d.Root(), []byte(`1`))}, ErrInvalidOp},
			{"invalid node", []Op{AppendElement(NodeID(50), []byte(`1`))}, ErrInvalidOp},
			{"invalid value", []Op{AppendElement(l, []byte(`[1,]`))}, ErrInvalidValue},
			{"append inside a replaced array", []Op{Replace(l, []byte(`[]`)), AppendElement(d.Elems(l)[0], []byte(`2`))}, ErrOverlap},
			{"append to a replaced array", []Op{Replace(l, []byte(`[]`)), AppendElement(l, []byte(`2`))}, ErrOverlap},
		} {
			if _, _, err := d.Apply(c.ops...); !errors.Is(err, c.want) {
				t.Errorf("%s: want %v, got %v", c.name, c.want, err)
			}
		}
	})
}

// hostileBundle carries the number lexemes and whitespace styles the relay
// fixtures carry; every edit below must leave them byte for byte.
const hostileBundle = "{\r\n\t\"resourceType\": \"Bundle\",\r\n\t\"entry\": [\r\n\t\t{\r\n\t\t\t\"fullUrl\": \"urn:uuid:1\",\r\n\t\t\t\"resource\": {\r\n\t\t\t\t\"resourceType\": \"Organization\",\r\n\t\t\t\t\"identifier\": [ { \"system\": \"http://example.org/payer\", \"value\" : \"PAYER-1\" } ],\r\n\t\t\t\t\"extension\": [{\"url\":\"http://example.org/x\",\"valueDecimal\":1.50}]\r\n\t\t\t}\r\n\t\t},\r\n\t\t{\"resource\":{\"resourceType\":\"Basic\",\"n\":[9007199254740993,1e2,-0.0],\"t\":\"<>&\\u00e9\"}}\r\n\t]\r\n}"

func TestNestedReplaceInsideAnEntry(t *testing.T) {
	d := mustScan(t, hostileBundle)
	entry0 := d.Elems(mustMember(t, d, d.Root(), "entry"))[0]
	org := mustMember(t, d, entry0, "resource")
	ident := d.Elems(mustMember(t, d, org, "identifier"))[0]
	val := mustMember(t, d, ident, "value")
	if d.Kind(val) != KindString {
		t.Fatalf("kind %v", d.Kind(val))
	}
	want := strings.Replace(hostileBundle, `"value" : "PAYER-1"`, `"value" : "PAYER-\u00e9/2"`, 1)
	applyExact(t, d, want, Replace(val, []byte(`"PAYER-\u00e9/2"`)))

	out, edits, err := d.Apply(Replace(val, []byte(`"PAYER-2"`)))
	if err != nil {
		t.Fatal(err)
	}
	s, e := d.Span(val)
	if len(edits) != 1 || edits[0].Start != s || edits[0].End != e || string(edits[0].New) != `"PAYER-2"` {
		t.Fatalf("edits %+v, span %d,%d", edits, s, e)
	}
	// Everything outside the span is untouched, including every number lexeme.
	if !bytes.Equal(out[:s], []byte(hostileBundle[:s])) || !bytes.Equal(out[s+len(`"PAYER-2"`):], []byte(hostileBundle[e:])) {
		t.Fatal("bytes outside the edited span changed")
	}
	for _, lex := range []string{"1.50", "9007199254740993", "1e2", "-0.0", `"<>&\u00e9"`, "\r\n\t\t\t\t"} {
		if bytes.Count(out, []byte(lex)) != strings.Count(hostileBundle, lex) {
			t.Errorf("lexeme %q count changed", lex)
		}
	}
}

func TestNumbersUntouchedOutsideEditedSpans(t *testing.T) {
	src := `{"a":1.50,"b":9007199254740993,"c":1e2,"d":-0.0,"s":"x","l":[1.50,-0.0]}`
	d := mustScan(t, src)
	s := mustMember(t, d, d.Root(), "s")
	l := mustMember(t, d, d.Root(), "l")
	applyExact(t, d,
		`{"a":1.50,"b":9007199254740993,"d":-0.0,"s":"y","l":[1.50,-0.0,1e2],"e":0.10}`,
		Replace(s, []byte(`"y"`)),
		RemoveMember(d.Root(), "c"),
		AppendElement(l, []byte(`1e2`)),
		InsertMember(d.Root(), "e", []byte(`0.10`)),
	)
	// A number replaced by an equal-valued different lexeme is a real edit.
	b := mustMember(t, d, d.Root(), "b")
	applyExact(t, d, `{"a":1.50,"b":9.007199254740993e15,"c":1e2,"d":-0.0,"s":"x","l":[1.50,-0.0]}`,
		Replace(b, []byte(`9.007199254740993e15`)))
}

func TestReplace(t *testing.T) {
	src := `{"a":[1,{"b":"c"}],"d":true}`
	d := mustScan(t, src)
	a := mustMember(t, d, d.Root(), "a")
	inner := d.Elems(a)[1]
	b := mustMember(t, d, inner, "b")
	dd := mustMember(t, d, d.Root(), "d")

	applyExact(t, d, `{"a":[1,{"b":"c"}],"d":{"x":[]}}`, Replace(dd, []byte(`{"x":[]}`)))
	applyExact(t, d, `null`, Replace(d.Root(), []byte(`null`)))
	applyExact(t, d, `{"a":"gone","d":false}`, Replace(dd, []byte(`false`)), Replace(a, []byte(`"gone"`)))
	// A same-bytes replacement is still reported as a declared edit.
	out, edits, err := d.Apply(Replace(b, []byte(`"c"`)))
	if err != nil || string(out) != src || len(edits) != 1 {
		t.Fatalf("same-bytes replace: %q %+v %v", out, edits, err)
	}
	// No operations: the original bytes and no edits.
	out, edits, err = d.Apply()
	if err != nil || string(out) != src || len(edits) != 0 {
		t.Fatalf("empty apply: %q %+v %v", out, edits, err)
	}
	// The caller's value slice is copied.
	v := []byte(`"z"`)
	op := Replace(b, v)
	v[1] = 'q'
	applyExact(t, d, `{"a":[1,{"b":"z"}],"d":true}`, op)

	for _, c := range []struct {
		name string
		ops  []Op
		want error
	}{
		{"parent and child", []Op{Replace(a, []byte(`1`)), Replace(b, []byte(`1`))}, ErrOverlap},
		{"child and parent", []Op{Replace(b, []byte(`1`)), Replace(d.Root(), []byte(`1`))}, ErrOverlap},
		{"same node twice", []Op{Replace(b, []byte(`1`)), Replace(b, []byte(`2`))}, ErrOverlap},
		{"invalid node", []Op{Replace(NodeID(100), []byte(`1`))}, ErrInvalidOp},
		{"negative node", []Op{Replace(NodeID(-1), []byte(`1`))}, ErrInvalidOp},
		{"nil op", []Op{nil}, ErrInvalidOp},
		{"two values", []Op{Replace(b, []byte(`1 2`))}, ErrInvalidValue},
		{"leading whitespace", []Op{Replace(b, []byte(` 1`))}, ErrInvalidValue},
		{"trailing whitespace", []Op{Replace(b, []byte("1\n"))}, ErrInvalidValue},
		{"empty", []Op{Replace(b, []byte(``))}, ErrInvalidValue},
		{"duplicate keys", []Op{Replace(b, []byte(`{"k":1,"k":2}`))}, ErrInvalidValue},
		{"duplicate keys (sentinel)", []Op{Replace(b, []byte(`{"k":1,"k":2}`))}, ErrDuplicateKey},
		{"lone surrogate", []Op{Replace(b, []byte(`"\udc00"`))}, ErrLoneSurrogate},
		{"invalid UTF-8", []Op{Replace(b, []byte("\"\xff\""))}, ErrInvalidUTF8},
		{"syntax", []Op{Replace(b, []byte(`{`))}, ErrSyntax},
	} {
		if _, _, err := d.Apply(c.ops...); !errors.Is(err, c.want) {
			t.Errorf("%s: want %v, got %v", c.name, c.want, err)
		}
	}
}

func TestApplyEditsAreInOriginalOffsetOrder(t *testing.T) {
	src := `{"a":"x","b":[1],"c":{"k":1},"d":"y"}`
	d := mustScan(t, src)
	dn := mustMember(t, d, d.Root(), "d")
	an := mustMember(t, d, d.Root(), "a")
	out, edits, err := d.Apply(
		Replace(dn, []byte(`"Y"`)),
		InsertMember(mustMember(t, d, d.Root(), "c"), "z", []byte(`2`)),
		AppendElement(mustMember(t, d, d.Root(), "b"), []byte(`2`)),
		Replace(an, []byte(`"X"`)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"a":"X","b":[1,2],"c":{"k":1,"z":2},"d":"Y"}`; string(out) != want {
		t.Fatalf("got %s", out)
	}
	if !sort.SliceIsSorted(edits, func(i, j int) bool { return edits[i].Start < edits[j].Start }) || len(edits) != 4 {
		t.Fatalf("edits %+v", edits)
	}
	want := []Edit{
		{5, 8, []byte(`"X"`)},
		{15, 15, []byte(`,2`)},
		{27, 27, []byte(`,"z":2`)},
		{33, 36, []byte(`"Y"`)},
	}
	for i, e := range edits {
		if e.Start != want[i].Start || e.End != want[i].End || !bytes.Equal(e.New, want[i].New) {
			t.Fatalf("edit %d = {%d %d %q}, want {%d %d %q}", i, e.Start, e.End, e.New, want[i].Start, want[i].End, want[i].New)
		}
	}
}

func TestApplyPostconditionRescans(t *testing.T) {
	// The output must satisfy the document's own limits: an insertion that
	// pushes it past the size or depth bound is refused, not emitted.
	d, err := Scan([]byte(`{"a":1}`), Limits{MaxBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.Apply(InsertMember(d.Root(), "b", []byte(`2`))); !errors.Is(err, ErrSizeLimit) || !errors.Is(err, ErrPostcondition) {
		t.Fatalf("size: want ErrPostcondition wrapping ErrSizeLimit, got %v", err)
	}
	d, err = Scan([]byte(`[[]]`), Limits{MaxDepth: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.Apply(AppendElement(d.Elems(d.Root())[0], []byte(`[]`))); !errors.Is(err, ErrDepthLimit) || !errors.Is(err, ErrPostcondition) {
		t.Fatalf("depth: want ErrPostcondition wrapping ErrDepthLimit, got %v", err)
	}
}

// syntaxFixtures maps each invalid-syntax fixture to the refusal it must
// produce.
var syntaxFixtures = map[string]error{
	"casefold-duplicate-key.json": ErrDuplicateKey,
	"duplicate-key.json":          ErrDuplicateKey,
	"escaped-duplicate-key.json":  ErrDuplicateKey,
	"trailing-document.json":      ErrTrailingData,
	"bad-utf8.bin":                ErrInvalidUTF8,
	"lone-surrogate.json":         ErrLoneSurrogate,
	"deep-nesting.json":           ErrDepthLimit,
}

func fixtureNames(t testing.TB, sub string) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(fixtureDir, sub))
	if err != nil {
		t.Fatalf("read %s: %v", sub, err)
	}
	var names []string
	for _, e := range ents {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}

func TestFixtureCorpus(t *testing.T) {
	valid := fixtureNames(t, "valid")
	if len(valid) == 0 {
		t.Fatal("no valid fixtures")
	}
	for _, name := range valid {
		t.Run("valid/"+name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(fixtureDir, "valid", name))
			if err != nil {
				t.Fatal(err)
			}
			d, err := Scan(raw, Limits{})
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			// Replacing every string token with itself reproduces the input.
			var ops []Op
			for i := range d.nodes {
				if d.Kind(NodeID(i)) == KindString {
					s, e := d.Span(NodeID(i))
					ops = append(ops, Replace(NodeID(i), raw[s:e]))
				}
			}
			out, edits, err := d.Apply(ops...)
			if err != nil || !bytes.Equal(out, raw) || len(edits) != len(ops) {
				t.Fatalf("identity splice: %v (%d edits for %d ops)", err, len(edits), len(ops))
			}
		})
	}
	got := fixtureNames(t, "invalid")
	if len(got) != len(syntaxFixtures) {
		t.Fatalf("invalid/ holds %v, want exactly the %d syntax fixtures", got, len(syntaxFixtures))
	}
	for _, name := range got {
		want, ok := syntaxFixtures[name]
		if !ok {
			t.Fatalf("unexpected invalid fixture %s", name)
		}
		t.Run("invalid/"+name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(fixtureDir, "invalid", name))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Scan(raw, Limits{}); !errors.Is(err, want) {
				t.Fatalf("want %v, got %v", want, err)
			}
		})
	}
	t.Run("duplicate-key detail", func(t *testing.T) {
		raw, err := os.ReadFile(filepath.Join(fixtureDir, "invalid", "duplicate-key.json"))
		if err != nil {
			t.Fatal(err)
		}
		_, err = Scan(raw, Limits{})
		var dk *DuplicateKeyError
		if !errors.As(err, &dk) || dk.Key != "beneficiary" || dk.Depth < 1 {
			t.Fatalf("want a nested duplicate beneficiary, got %v", err)
		}
	})
	t.Run("escaped-duplicate-key detail", func(t *testing.T) {
		raw, err := os.ReadFile(filepath.Join(fixtureDir, "invalid", "escaped-duplicate-key.json"))
		if err != nil {
			t.Fatal(err)
		}
		_, err = Scan(raw, Limits{})
		var dk *DuplicateKeyError
		if !errors.As(err, &dk) || dk.Key != "resourceType" || dk.Depth != 0 {
			t.Fatalf("want a top-level duplicate resourceType, got %v", err)
		}
		if !bytes.Contains(raw, []byte(bs+"u0054")) || bytes.Count(raw, []byte(`"resourceType"`)) != 1 {
			t.Fatal("the fixture must spell the second resourceType with an escape")
		}
	})
}

func TestErrorMessages(t *testing.T) {
	_, err := Scan([]byte(`{"a":1,"a":2}`), Limits{})
	if msg := err.Error(); !strings.Contains(msg, `"a"`) || !strings.Contains(msg, "offset 7") {
		t.Fatalf("duplicate message %q", msg)
	}
	_, err = Scan([]byte(`[1,]`), Limits{})
	if msg := err.Error(); !strings.Contains(msg, "offset 3") {
		t.Fatalf("syntax message %q", msg)
	}
}

// TestDescribe pins the read-only description of each operation, which the
// caller's independent verification uses to recompute the expected result.
func TestDescribe(t *testing.T) {
	val := []byte(`"v"`)
	ensure, h := EnsureObjectMember(3, "k")
	rows := []struct {
		name string
		op   Op
		want OpInfo
	}{
		{"replace", Replace(2, val), OpInfo{Kind: OpReplace, Target: 2, Value: val}},
		{"remove", RemoveMember(1, "a"), OpInfo{Kind: OpRemoveMember, Target: 1, Key: "a"}},
		{"insert", InsertMember(NodeID(h), "b", val), OpInfo{Kind: OpInsertMember, Target: NodeID(h), Key: "b", Value: val}},
		{"ensure", ensure, OpInfo{Kind: OpEnsureObjectMember, Target: 3, Key: "k", Handle: h}},
		{"append", AppendElement(4, val), OpInfo{Kind: OpAppendElement, Target: 4, Value: val}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			got := Describe(r.op)
			if got.Kind != r.want.Kind || got.Target != r.want.Target || got.Key != r.want.Key ||
				got.Handle != r.want.Handle || !bytes.Equal(got.Value, r.want.Value) {
				t.Fatalf("Describe = %+v, want %+v", got, r.want)
			}
			if got.Value != nil {
				got.Value[0] = 'x'
				if again := Describe(r.op); again.Value[0] != r.want.Value[0] {
					t.Fatal("Describe must return a copy of the value bytes")
				}
			}
		})
	}
	if got := Describe(nil); got.Kind != 0 {
		t.Fatalf("Describe(nil) = %+v, want the zero OpInfo", got)
	}
	for k, want := range map[OpKind]string{OpReplace: "replace", OpRemoveMember: "remove-member",
		OpInsertMember: "insert-member", OpEnsureObjectMember: "ensure-object-member",
		OpAppendElement: "append-element", 0: "invalid"} {
		if k.String() != want {
			t.Errorf("OpKind(%d).String() = %q, want %q", k, k.String(), want)
		}
	}
}
