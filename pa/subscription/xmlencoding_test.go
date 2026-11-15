package subscription

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/text/encoding/htmlindex"
)

// Each label the registry reads names, by the WHATWG Encoding Standard (as
// golang.org/x/text implements it), the encoding the registry says, and no
// label names one the registry refuses: ISO-2022-JP, the replacement
// encoding (ISO-2022-KR, ISO-2022-CN, HZ), UTF-16 and x-user-defined are
// absent, as is every label no registry names (UTF-7, UTF-32, EBCDIC).
func TestXMLEncodingLabelsAreWHATWGs(t *testing.T) {
	if len(xmlEncodingLabels) == 0 {
		t.Fatal("the registry is empty")
	}
	for label, name := range xmlEncodingLabels {
		if label != strings.ToLower(label) {
			t.Errorf("%q: a label is held in lower case", label)
		}
		enc, err := htmlindex.Get(label)
		if err != nil {
			t.Errorf("%q: not a WHATWG label: %v", label, err)
			continue
		}
		if got, _ := htmlindex.Name(enc); got != name {
			t.Errorf("%q: WHATWG names %q, the registry %q", label, got, name)
		}
	}
	for _, refused := range []string{"iso-2022-jp", "csiso2022jp", "iso-2022-kr", "iso-2022-cn", "hz-gb-2312", "replacement",
		"utf-16", "utf-16le", "utf-16be", "unicode", "ucs-2", "x-user-defined", "utf-7", "utf-32", "ibm037", "cp037", "ebcdic-cp-us"} {
		if _, ok := xmlEncodingLabels[refused]; ok {
			t.Errorf("%q: the registry reads an encoding that does not read ASCII as ASCII", refused)
		}
	}
	// Every label WHATWG gives each encoding the registry reads is in it.
	names := map[string]bool{}
	for _, n := range xmlEncodingLabels {
		names[n] = true
	}
	for _, label := range whatwgLabelsOf(t, names) {
		if _, ok := xmlEncodingLabels[label]; !ok {
			t.Errorf("%q: a WHATWG label of an encoding the registry reads is missing from it", label)
		}
	}
}

// whatwgLabelsOf lists, from the labels the registry knows and their common
// spellings, every one WHATWG maps to an encoding in names: a check that
// the registry was not trimmed by hand.
func whatwgLabelsOf(t *testing.T, names map[string]bool) []string {
	t.Helper()
	var out []string
	for _, l := range []string{"latin1", "ascii", "us-ascii", "iso-8859-1", "windows-1252", "cp1252", "sjis", "ms932", "windows-31j",
		"shift_jis", "euc-jp", "euc-kr", "ks_c_5601-1987", "gb2312", "gbk", "gb18030", "big5", "big5-hkscs", "koi8-r", "koi8-u",
		"macintosh", "x-mac-cyrillic", "ibm866", "windows-874", "tis-620", "iso-8859-8-i", "iso-8859-16", "utf8", "unicode-1-1-utf-8"} {
		enc, err := htmlindex.Get(l)
		if err != nil {
			t.Fatalf("%q: %v", l, err)
		}
		if n, _ := htmlindex.Name(enc); names[n] {
			out = append(out, l)
		}
	}
	return out
}

// xmlMarkupBytes are the ASCII bytes XML's markup is made of: where one
// stands, a reader of the declared encoding and readXML, which reads the
// bytes, must see the same character.
const xmlMarkupBytes = "<>\"'=&/?!-\t\n\r "

// Every encoding the registry reads reads ASCII as ASCII: each byte below
// 0x80 alone decodes as that character, no byte from 0x80 alone decodes as
// an ASCII character, and no character's multibyte sequence holds a byte of
// XML's markup. So the markup sits where the bytes show it, whatever the
// characters around it, and a byte a multibyte character swallows (a
// Shift_JIS or GBK trail byte in 0x40-0x7E, a GB18030 digit) is never one a
// tag, a value's quotes or a reference is made of.
func TestXMLEncodingsKeepMarkupWhereTheBytesShowIt(t *testing.T) {
	names := map[string]bool{}
	for _, n := range xmlEncodingLabels {
		names[n] = true
	}
	for name := range names {
		enc, err := htmlindex.Get(name)
		if err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		for b := 0; b < 0x100; b++ {
			got, err := enc.NewDecoder().Bytes([]byte{byte(b)})
			switch {
			case b < 0x80 && (err != nil || !bytes.Equal(got, []byte{byte(b)})):
				t.Errorf("%s: byte %#x decodes as %q, %v", name, b, got, err)
			case b >= 0x80 && err == nil && slices.ContainsFunc([]rune(string(got)), func(r rune) bool { return r < 0x80 }):
				t.Errorf("%s: byte %#x alone decodes as ASCII %q", name, b, got)
			}
		}
		e := enc.NewEncoder()
		multibyte, asciiTrail := 0, 0
		for r := rune(0x80); r <= utf8.MaxRune; r++ {
			if r >= 0xD800 && r <= 0xDFFF {
				continue
			}
			if name != "gb18030" && r > 0xFFFF {
				// Only GB18030 encodes beyond the BMP.
				break
			}
			seq, err := e.Bytes([]byte(string(r)))
			if err != nil || len(seq) < 2 {
				continue
			}
			multibyte++
			if bytes.ContainsFunc(seq[1:], func(r rune) bool { return r < 0x80 }) {
				asciiTrail++
			}
			if i := bytes.IndexAny(seq, xmlMarkupBytes); i >= 0 {
				t.Errorf("%s: U+%04X is %x, holding the markup byte %q", name, r, seq, seq[i])
			}
		}
		// The check is not vacuous: each multibyte encoding was read
		// through, and those whose trail bytes reach into ASCII were seen to.
		if want, ok := multibyteEncodings[name]; ok && (multibyte < 1000 || (asciiTrail > 0) != want) {
			t.Errorf("%s: %d multibyte characters, %d with a trail byte below 0x80; want one below 0x80: %v", name, multibyte, asciiTrail, want)
		}
	}
	for name := range multibyteEncodings {
		if !names[name] {
			t.Errorf("%s: a multibyte encoding the registry does not read", name)
		}
	}
}

// multibyteEncodings are the registry's multibyte encodings, each with
// whether a trail byte of it may lie below 0x80 (Shift_JIS's, GBK's and
// Big5's in 0x40-0x7E, EUC-KR's (Windows-949's) letters, GB18030's digits);
// EUC-JP's never does.
var multibyteEncodings = map[string]bool{
	"shift_jis": true, "gbk": true, "gb18030": true, "big5": true, "euc-kr": true, "euc-jp": false,
}
