package splice

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// bs is a backslash, spelled so no escape sequence appears in this source.
var bs = string(rune(92))

// inlineSeeds keep the fuzz targets seeded even without the fixture files.
var inlineSeeds = []string{
	`{"a":"x","b":[1.50,9007199254740993,1e2,-0.0],"c":{"d":"y"}}`,
	"{\r\n\t\"resourceType\": \"Bundle\",\r\n\t\"entry\": [\r\n\t\t{\"resource\": {\"id\": \"o1\", \"name\": \"<>&\"}}\r\n\t]\r\n}",
	`["` + bs + `u00e9", "` + bs + `ud83d` + bs + `ude00", "` + bs + `"` + bs + bs + `"]`,
	`{"k` + bs + `u0061":"v","ka":"w"}`,
	`[]`,
	`"only"`,
	`{"a":1,"a":2}`,
	`[1] [2]`,
	`["` + bs + `ud800"]`,
	"[\"\xff\"]",
}

func addFixtureSeeds(f *testing.F, subs ...string) {
	f.Helper()
	for _, sub := range subs {
		ents, err := os.ReadDir(filepath.Join(fixtureDir, sub))
		if err != nil {
			f.Fatalf("read %s: %v", sub, err)
		}
		for _, e := range ents {
			raw, err := os.ReadFile(filepath.Join(fixtureDir, sub, e.Name()))
			if err != nil {
				f.Fatal(err)
			}
			f.Add(raw)
		}
	}
}

// FuzzSplicePreservesUntouchedSpans replaces one fuzzer-chosen string token
// with a fuzzer-chosen string and asserts that the output is the input with
// only that span substituted, that it re-scans to the same structure with
// every other span shifted exactly, and that the new token decodes to the
// intended text.
func FuzzSplicePreservesUntouchedSpans(f *testing.F) {
	for i, s := range inlineSeeds {
		f.Add([]byte(s), uint32(i), "replacement "+s)
	}
	for _, sub := range []string{"valid"} {
		ents, err := os.ReadDir(filepath.Join(fixtureDir, sub))
		if err != nil {
			f.Fatalf("read %s: %v", sub, err)
		}
		for i, e := range ents {
			raw, err := os.ReadFile(filepath.Join(fixtureDir, sub, e.Name()))
			if err != nil {
				f.Fatal(err)
			}
			f.Add(raw, uint32(i*7919), "PAYER-2 <>& é\r\n\t"+bs+"\"")
		}
	}
	f.Fuzz(func(t *testing.T, src []byte, pick uint32, repl string) {
		d, err := Scan(src, Limits{})
		if err != nil {
			return
		}
		var stringsIDs []NodeID
		for i := range d.nodes {
			if d.nodes[i].kind == KindString {
				stringsIDs = append(stringsIDs, NodeID(i))
			}
		}
		if len(stringsIDs) == 0 {
			return
		}
		target := stringsIDs[int(pick%uint32(len(stringsIDs)))]

		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(pick&1 == 0)
		if err := enc.Encode(repl); err != nil {
			t.Fatal(err)
		}
		value := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))

		out, edits, err := d.Apply(Replace(target, value))
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		s, e := d.Span(target)
		want := append(append(append([]byte{}, src[:s]...), value...), src[e:]...)
		if !bytes.Equal(out, want) {
			t.Fatalf("output differs outside the edited span")
		}
		if len(edits) != 1 || edits[0].Start != s || edits[0].End != e || !bytes.Equal(edits[0].New, value) {
			t.Fatalf("edits %+v, want one [%d,%d)", edits, s, e)
		}

		d2, err := Scan(out, Limits{})
		if err != nil {
			t.Fatalf("output does not re-scan: %v", err)
		}
		if len(d2.nodes) != len(d.nodes) {
			t.Fatalf("node count %d, want %d", len(d2.nodes), len(d.nodes))
		}
		delta := len(value) - (e - s)
		shift := func(off int) int {
			if off >= e {
				return off + delta
			}
			return off
		}
		for i := range d.nodes {
			a, b := &d.nodes[i], &d2.nodes[i]
			if a.kind != b.kind || len(a.members) != len(b.members) || len(a.elems) != len(b.elems) {
				t.Fatalf("node %d changed shape", i)
			}
			wantStart, wantEnd := shift(a.start), shift(a.end)
			if NodeID(i) == target {
				wantEnd = s + len(value)
			}
			if b.start != wantStart || b.end != wantEnd {
				t.Fatalf("node %d span [%d,%d), want [%d,%d)", i, b.start, b.end, wantStart, wantEnd)
			}
			for j := range a.members {
				if a.members[j].name != b.members[j].name || shift(a.members[j].keyStart) != b.members[j].keyStart {
					t.Fatalf("node %d member %d moved or renamed", i, j)
				}
			}
		}
		var decoded string
		if err := json.Unmarshal(value, &decoded); err != nil {
			t.Fatal(err)
		}
		got, err := d2.StringValue(target)
		if err != nil || got != decoded {
			t.Fatalf("new token decodes to %q (%v), want %q", got, err, decoded)
		}
	})
}

// FuzzScanAgreesWithEncodingJSON checks the scanner against the standard
// library: whatever Scan accepts, encoding/json accepts and decodes to the
// same strings; whatever Scan refuses as malformed, encoding/json refuses
// too. Scan is stricter only by policy (UTF-8, lone surrogates, duplicate
// names, limits).
func FuzzScanAgreesWithEncodingJSON(f *testing.F) {
	for _, s := range inlineSeeds {
		f.Add([]byte(s))
	}
	addFixtureSeeds(f, "valid", "invalid")
	f.Fuzz(func(t *testing.T, src []byte) {
		d, err := Scan(src, Limits{})
		valid := json.Valid(src)
		if err == nil {
			if !valid {
				t.Fatalf("Scan accepted what encoding/json refuses: %q", src)
			}
			dec := json.NewDecoder(bytes.NewReader(src))
			dec.UseNumber()
			var v any
			if err := dec.Decode(&v); err != nil {
				t.Fatalf("encoding/json cannot decode an accepted document: %v", err)
			}
			for i := range d.nodes {
				n := NodeID(i)
				s, e := d.Span(n)
				if !json.Valid(src[s:e]) {
					t.Fatalf("node %d span %q is not one JSON value", i, src[s:e])
				}
				if d.Kind(n) != KindString {
					continue
				}
				var want string
				if err := json.Unmarshal(src[s:e], &want); err != nil {
					t.Fatal(err)
				}
				got, err := d.StringValue(n)
				if err != nil || got != want {
					t.Fatalf("StringValue %q, encoding/json %q", got, want)
				}
				if !utf8.ValidString(got) {
					t.Fatalf("decoded string is not valid UTF-8: %q", got)
				}
			}
			for i := range d.nodes {
				for _, m := range d.nodes[i].members {
					var want string
					if err := json.Unmarshal(src[m.keyStart:m.keyEnd], &want); err != nil || want != m.name {
						t.Fatalf("member name %q, encoding/json %q (%v)", m.name, want, err)
					}
				}
			}
			return
		}
		var se *ScanError
		var de *DuplicateKeyError
		switch {
		case errors.As(err, &de):
			if !strings.Contains(string(src), `"`) {
				t.Fatalf("duplicate reported in a document with no strings")
			}
		case !errors.As(err, &se):
			t.Fatalf("unexpected error type %T: %v", err, err)
		case errors.Is(err, ErrSyntax), errors.Is(err, ErrTrailingData):
			if valid {
				t.Fatalf("Scan refused valid JSON as malformed (%v): %q", err, src)
			}
		case errors.Is(err, ErrInvalidUTF8):
			if utf8.Valid(src) {
				t.Fatalf("invalid UTF-8 reported for valid UTF-8: %q", src)
			}
		case errors.Is(err, ErrLoneSurrogate):
			if !strings.Contains(string(src), bs+"u") {
				t.Fatalf("lone surrogate reported without a unicode escape")
			}
		case errors.Is(err, ErrDepthLimit), errors.Is(err, ErrTokenLimit), errors.Is(err, ErrSizeLimit):
		default:
			t.Fatalf("unclassified error %v", err)
		}
		if d != nil {
			t.Fatal("a refused scan returned a document")
		}
	})
}
