//go:build !shn_coreonly

package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// memberWalkRows are documents the duplicate-member check must judge as the
// token walk does: each names a member twice under case folding (dup) or not.
var memberWalkRows = []struct {
	name string
	doc  string
	dup  bool
}{
	{"one object, distinct names", `{"a":1,"b":2}`, false},
	{"a name twice", `{"a":1,"a":2}`, true},
	{"a name in another ASCII case", `{"id":"x","Id":"y"}`, true},
	{"the escaped Kelvin sign is k", `{"k":1,"\u212a":2}`, true},
	{"the raw Kelvin sign is K", "{\"K\":1,\"\u212a\":2}", true},
	{"the raw long s is s", "{\"subject\":1,\"\u017fubject\":2}", true},
	{"the escaped long s is s", `{"subject":1,"\u017fubject":2}`, true},
	{"an escaped ASCII letter is that letter", `{"\u0061":1,"A":2}`, true},
	{"an escaped quote in a name", `{"a\"b":1,"A\"B":2}`, true},
	{"an escaped backslash in a name", `{"a\\b":1,"a\\c":2}`, false},
	{"a backslash name and its folded twin", `{"x\\y":1,"X\\Y":2}`, true},
	{"a surrogate pair and the raw rune", "{\"\\ud83d\\ude00\":1,\"\U0001F600\":2}", true},
	{"two names that differ past ASCII", "{\"\u00e9\":1,\"\u00ea\":2}", false},
	{"\u00e9 and \u00c9", "{\"\u00e9\":1,\"\u00c9\":2}", true},
	{"the empty name twice", `{"":1,"":2}`, true},
	{"the same name in sibling objects", `{"a":{"x":1},"b":{"x":2}}`, false},
	{"the same name in parent and child", `{"x":{"x":{"x":1}}}`, false},
	{"a twin after a nested object", `{"a":{"b":1,"c":[{"d":1}]},"A":2}`, true},
	{"a twin inside an array's object", `{"a":[1,{"x":1,"y":2,"X":3}]}`, true},
	{"objects in an array, each its own", `{"a":[{"x":1},{"x":2},{"X":3}]}`, false},
	{"a value that looks like a name", `{"a":"a","b":"A"}`, false},
	{"a value holding braces, commas and quotes", `{"a":"{\"a\":1,\"a\":2}","b":"]},"}`, false},
	{"names after a string value with a brace", `{"a":"}","A":1}`, true},
	{"white space everywhere", " { \"a\" : [ 1 , { \"b\" : null } ] ,\n\t\"B\" : true , \"A\" : false } ", true},
	{"numbers and literals", `{"a":-1.5e+10,"b":true,"c":false,"d":null,"e":[0,1,2]}`, false},
	{"a top-level array of objects", `[{"a":1},{"a":1,"a":2}]`, true},
	{"a top-level scalar", `"a"`, false},
	{"an empty object and array", `{"a":{},"b":[],"c":[{}]}`, false},
	{"a wide object with a twin past the linear bound", wideObject(40, 33), true},
	{"a wide object without a twin", wideObject(40, -1), false},
	{"a wide object, a twin at the bound", wideObject(memberLinearMax+1, memberLinearMax), true},
	{"a wide object's child reuses its names", `{"o":` + wideObject(30, -1) + `,"p":` + wideObject(30, -1) + `}`, false},
	{"a twin of a name read after the set began", namedObject(40, "M25"), true},
	{"a sibling's name the set held before", `{"o":` + namedObject(30, "") + `,"p":{` + yNames(17) + `,"m29":1}}`, false},
}

// wideObject is an object of n members m0..m(n-1); the member at twinAt
// (when >= 0) is named M0, m0's twin in another case.
func wideObject(n, twinAt int) string {
	var b strings.Builder
	b.WriteByte('{')
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		name := fmt.Sprintf("m%d", i)
		if i == twinAt {
			name = "M0"
		}
		fmt.Fprintf(&b, `%q:%d`, name, i)
	}
	b.WriteByte('}')
	return b.String()
}

// namedObject is an object of n members m0..m(n-1), then one named extra
// when it is not "".
func namedObject(n int, extra string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"m%d":%d`, i, i)
	}
	if extra != "" {
		fmt.Fprintf(&b, `,%q:0`, extra)
	}
	b.WriteByte('}')
	return b.String()
}

// yNames is n members y0..y(n-1), without braces.
func yNames(n int) string {
	parts := make([]string, n)
	for i := range n {
		parts[i] = fmt.Sprintf(`"y%d":%d`, i, i)
	}
	return strings.Join(parts, ",")
}

// TestOneSpellingPerMember_AgreesWithTheTokenWalk: the one-pass walk refuses
// exactly the documents the token walk refuses, with the same reason.
func TestOneSpellingPerMember_AgreesWithTheTokenWalk(t *testing.T) {
	for _, row := range memberWalkRows {
		t.Run(row.name, func(t *testing.T) {
			doc := []byte(row.doc)
			if !json.Valid(doc) {
				t.Fatalf("row is not JSON: %s", doc)
			}
			got, want := oneSpellingPerMember(doc), oneSpellingPerMemberByTokens(doc)
			if (want != nil) != row.dup {
				t.Fatalf("the token walk says %v; the row says dup=%v", want, row.dup)
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("one-pass walk: %v; token walk: %v", got, want)
			}
		})
	}
}

// TestOneSpellingPerMember_NearTheTokenBound: a record of about the relay's
// million tokens is judged as the token walk judges it, with and without a
// twin at its end, and the walk allocates per object at most, never per token.
func TestOneSpellingPerMember_NearTheTokenBound(t *testing.T) {
	clean := benchRecord(1_000_000/8*56, true)
	if n := tokens(clean); n < 900_000 || n > 1_000_100 {
		t.Fatalf("the record holds %d tokens, want about a million", n)
	}
	twin := append(bytes.TrimSuffix(clean, []byte(`}`)), []byte(`,"ID":"o2"}`)...)
	for _, doc := range [][]byte{clean, twin} {
		got, want := oneSpellingPerMember(doc), oneSpellingPerMemberByTokens(doc)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("one-pass walk: %v; token walk: %v", got, want)
		}
	}
	if oneSpellingPerMember(twin) == nil {
		t.Fatal("the twin at the end of a million tokens was not refused")
	}
	// The token walk allocates about 5.5 per token here; the one-pass walk
	// grows a few buffers. Objects past memberLinearMax names, each kept in a
	// set, share one set per depth: thousands of them allocate no more than
	// one does.
	wide := wideRecord(1<<20, memberLinearMax+1)
	for name, doc := range map[string][]byte{"small objects": clean, "objects past the linear bound": wide} {
		if err := oneSpellingPerMember(doc); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if allocs := testing.AllocsPerRun(1, func() { _ = oneSpellingPerMember(doc) }); allocs > 100 {
			t.Fatalf("%s: the walk allocated %v times", name, allocs)
		}
	}
	wideTwin := append(bytes.TrimSuffix(wide, []byte(`}]}`)), []byte(`,"M3":0}]}`)...)
	if got, want := oneSpellingPerMember(wideTwin), oneSpellingPerMemberByTokens(wideTwin); fmt.Sprint(got) != fmt.Sprint(want) || got == nil {
		t.Fatalf("a twin in the last wide object: one-pass walk %v; token walk %v", got, want)
	}
}

// TestOneSpellingPerMember_AgreesWhenNamesCollide: with a name hash of four
// values, nearly every name in an object past the linear bound shares its
// hash with others, so each lookup walks a chain: the walk still judges
// every row as the token walk does.
func TestOneSpellingPerMember_AgreesWhenNamesCollide(t *testing.T) {
	weak := func(b []byte) uint64 { return uint64(len(b)) % 4 }
	docs := []string{wideObject(40, 33), wideObject(40, -1), namedObject(40, "M25"), namedObject(40, "M39"), namedObject(40, "m0x"),
		`{"o":` + namedObject(30, "") + `,"p":{` + yNames(17) + `,"m29":1}}`, `{"a":[` + namedObject(20, "") + `,` + namedObject(20, "M19") + `]}`}
	for _, row := range memberWalkRows {
		docs = append(docs, row.doc)
	}
	for _, doc := range docs {
		w := memberWalk{hash: weak}
		got, want := w.walk([]byte(doc)), oneSpellingPerMemberByTokens([]byte(doc))
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%.80s: one-pass walk %v; token walk %v", doc, got, want)
		}
	}
}

// FuzzOneSpellingPerMember: on any JSON document the one-pass walk and the
// token walk agree.
func FuzzOneSpellingPerMember(f *testing.F) {
	for _, row := range memberWalkRows {
		f.Add([]byte(row.doc))
	}
	f.Fuzz(func(t *testing.T, doc []byte) {
		if !json.Valid(doc) {
			return
		}
		got, want := oneSpellingPerMember(doc), oneSpellingPerMemberByTokens(doc)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%q: one-pass walk: %v; token walk: %v", doc, got, want)
		}
	})
}
