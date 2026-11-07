package dtraddress

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"strings"
	"testing"
)

func identity(t *testing.T, seed byte) ed25519.PrivateKey {
	t.Helper()
	return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, ed25519.SeedSize))
}

var addr = Address{Family: FamilyPayer, Payer: "payer-1", Provider: "provider-1", Line: "2.2", Base: "https://payer.example/fhir/dtr", Remainder: "/forms/a"}

// Two replicas of one gateway hold the same identity, so they derive the same
// key and each verifies the other's tags. A rotated identity (D125) derives a
// new key, under which a tag issued before it does not verify.
func TestDeriveKey_ReplicasAgreeAndARotationRefusesOldTags(t *testing.T) {
	a, err := DeriveKey(identity(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	b, err := DeriveKey(identity(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	tag := a.Tag(addr)
	if len(tag) != TagLen || !ValidTag(tag) || !b.Verify(addr, tag) {
		t.Fatalf("replica tag %q does not verify on the other replica", tag)
	}
	rotated, err := DeriveKey(identity(t, 2))
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Verify(addr, tag) {
		t.Fatal("a rotated key verifies a tag issued before the rotation")
	}
	if _, err := DeriveKey(nil); err == nil {
		t.Fatal("derived a key from no identity")
	}
	if (Key{}).Verify(addr, tag) {
		t.Fatal("a zero key verified a tag")
	}
}

// The key is derived, never the signing key itself, and its bytes never
// format: not as a value, a pointer, a %#v or inside a struct or an error.
func TestKey_NeverFormats(t *testing.T) {
	priv := identity(t, 7)
	k, err := DeriveKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(k.k, priv.Seed()) || bytes.Equal(k.k, priv) {
		t.Fatal("the tag key is the signing key")
	}
	wrapped := struct{ K Key }{k}
	for _, s := range []string{fmt.Sprint(k), fmt.Sprintf("%v %+v %#v %s %x %q", k, k, k, k, k, k), fmt.Sprintf("%+v %#v", wrapped, &wrapped), fmt.Errorf("e: %v", k).Error()} {
		if !strings.Contains(s, "redacted") || strings.Contains(s, fmt.Sprintf("%x", k.k)) {
			t.Fatalf("a key formatted as %q", s)
		}
	}
}

// Each field the tag binds changes it, and no two splits of one address into
// a base and a remainder share a tag.
func TestTag_BindsEveryField(t *testing.T) {
	k, _ := DeriveKey(identity(t, 1))
	base := k.Tag(addr)
	for name, a := range map[string]Address{
		"family":    {FamilyFetchServer, addr.Payer, addr.Provider, addr.Line, addr.Base, addr.Remainder},
		"payer":     {addr.Family, "payer-2", addr.Provider, addr.Line, addr.Base, addr.Remainder},
		"provider":  {addr.Family, addr.Payer, "provider-2", addr.Line, addr.Base, addr.Remainder},
		"line":      {addr.Family, addr.Payer, addr.Provider, "2.0", addr.Base, addr.Remainder},
		"base":      {addr.Family, addr.Payer, addr.Provider, addr.Line, "https://payer.example/fhir", addr.Remainder},
		"remainder": {addr.Family, addr.Payer, addr.Provider, addr.Line, addr.Base, "/forms/b"},
		"the split": {addr.Family, addr.Payer, addr.Provider, addr.Line, addr.Base + "/forms", "/a"},
	} {
		if k.Tag(a) == base || k.Verify(a, base) {
			t.Errorf("%s: the tag does not bind it", name)
		}
	}
}

func TestCleanRemainder(t *testing.T) {
	for r, want := range map[string]bool{
		"": true, "/a": true, "/a-b/c.d~e_f/G9": true,
		"/": false, "a": false, "/a/": false, "//a": false, "/.": false, "/..": false, "/a/../b": false,
		"/..;": false, "/a%2Fb": false, "/a b": false, "/a;b": false, `/a\b`: false, "/*": false, "/a=b": false,
	} {
		if got := CleanRemainder(r); got != want {
			t.Errorf("CleanRemainder(%q) = %v, want %v", r, got, want)
		}
	}
}

func TestUnder(t *testing.T) {
	base := "https://payer.example/fhir/dtr"
	for value, want := range map[string]string{
		"https://payer.example/fhir/dtr":         "",
		"https://payer.example/fhir/dtr/forms/a": "/forms/a",
		"HTTPS://PAYER.example:443/fhir/dtr/x":   "/x",
		"https://payer.example/fhir/dtrx":        "!",
		"https://payer.example/fhir":             "!",
		"https://payer.example:8443/fhir/dtr/x":  "!",
		"http://payer.example/fhir/dtr/x":        "!",
		"https://other.example/fhir/dtr/x":       "!",
		"https://u@payer.example/fhir/dtr/x":     "!",
		"https://payer.example/fhir/dtr/x?q=1":   "!",
		"https://payer.example/fhir/dtr/x#f":     "!",
		"https://payer.example/fhir/dtr/x?":      "!",
		"https://payer.example/fhir/%64tr/x":     "!",
		"/fhir/dtr/x":                            "!",
	} {
		r, ok := Under(value, base)
		if want == "!" {
			if ok {
				t.Errorf("Under(%q) = %q, want not under", value, r)
			}
			continue
		}
		if !ok || r != want {
			t.Errorf("Under(%q) = %q %v, want %q", value, r, ok, want)
		}
	}
	if r, ok := Under("https://payer.example/fhir/dtr/x", "https://payer.example/fhir/dtr/"); !ok || r != "/x" {
		t.Errorf("a base with an ending slash: %q %v", r, ok)
	}
}

// The re-key is made only where the value is spelled as the stated base, so
// the restore gives back the payer's bytes.
func TestSpelled(t *testing.T) {
	base := "https://payer.example/fhir/dtr"
	if r, ok := Spelled(base+"/forms/a", base); !ok || r != "/forms/a" {
		t.Fatalf("spelled: %q %v", r, ok)
	}
	for _, v := range []string{"https://PAYER.example/fhir/dtr/a", "https://payer.example:443/fhir/dtr/a", base + "/", base + "/a/", base + "x", base + "/a%20b"} {
		if _, ok := Spelled(v, base); ok {
			t.Errorf("Spelled(%q) passed", v)
		}
	}
}

func TestTags_FormatAndParse(t *testing.T) {
	tag := strings.Repeat("A", TagLen)
	pairs := []TagPair{{"/forms/a", tag}, {"", tag}, {BoolRemainder, tag}}
	v := FormatTags(pairs)
	if v != "/forms/a="+tag+" ="+tag+" *="+tag {
		t.Fatalf("formatted %q", v)
	}
	got, err := ParseTags(v)
	if err != nil || len(got) != 3 || got[0] != pairs[0] || got[1] != pairs[1] || got[2] != pairs[2] {
		t.Fatalf("parsed %v %v", got, err)
	}
	for _, bad := range []string{"/a", "/a=short", "/a/=" + tag, "/a=" + tag + " /a=" + strings.Repeat("B", TagLen), "x=" + tag, "**=" + tag} {
		if _, err := ParseTags(bad); err == nil {
			t.Errorf("ParseTags(%q) passed", bad)
		}
	}
	if got, err := ParseTags("/a=" + tag + " /a=" + tag); err != nil || len(got) != 1 {
		t.Errorf("the same pair twice: %v %v", got, err)
	}
}

func TestSubpath(t *testing.T) {
	tag := strings.Repeat("A", TagLen)
	for v, want := range map[string]Subpath{
		"/payer/2.2/" + tag:              {FamilyPayer, "2.2", tag, ""},
		"/payer/2.2/" + tag + "/forms/a": {FamilyPayer, "2.2", tag, "/forms/a"},
		"/fetch-server/2.0/" + tag:       {FamilyFetchServer, "2.0", tag, ""},
	} {
		s, err := ParseSubpath(v)
		if err != nil || s != want || s.String() != v {
			t.Errorf("ParseSubpath(%q) = %+v %v", v, s, err)
		}
	}
	for _, bad := range []string{"", "/", "/payer/2.2", "payer/2.2/" + tag, "/other/2.2/" + tag, "/payer/two/" + tag, "/payer/2.2/short",
		"/fetch-server/2.2/" + tag + "/x", "/payer/2.2/" + tag + "/../x", "/payer/2.2/" + tag + "/", "/payer/2.2/" + tag + "/a%2F"} {
		if _, err := ParseSubpath(bad); err == nil {
			t.Errorf("ParseSubpath(%q) passed", bad)
		}
	}
}

func TestFindIssued(t *testing.T) {
	tag := strings.Repeat("A", TagLen)
	for v, want := range map[string]Issued{
		"https://gw.example/base/dtr/payer/payer-1/2.2/" + tag + "/forms/a": {FamilyPayer, "payer-1", "2.2", tag, "/forms/a"},
		"https://gw.example/dtr/payer/p%20q/2.2/" + tag:                     {FamilyPayer, "p q", "2.2", tag, ""},
		"https://gw.example/dtr/fetch-server/payer-1/2.0/" + tag:            {FamilyFetchServer, "payer-1", "2.0", tag, ""},
	} {
		got, ok := FindIssued(v)
		if !ok || got != want {
			t.Errorf("FindIssued(%q) = %+v %v", v, got, ok)
		}
	}
	for _, bad := range []string{
		"https://gw.example/dtr/payer/payer-1/2.2/short",
		"https://gw.example/dtr/fetch-server/payer-1/2.2/" + tag + "/x",
		"https://gw.example/dtr/payer/payer-1/2.2/" + tag + "?q",
		"https://gw.example/other/payer-1/2.2/" + tag,
		"/dtr/payer/payer-1/2.2/" + tag,
	} {
		if got, ok := FindIssued(bad); ok {
			t.Errorf("FindIssued(%q) = %+v", bad, got)
		}
	}
}

func TestIssuedURL(t *testing.T) {
	tag := strings.Repeat("A", TagLen)
	got, err := IssuedURL("https://gw.example/base/", FamilyPayer, "p/q", "2.2", tag, "/forms/a")
	if err != nil || got != "https://gw.example/base/dtr/payer/p%2Fq/2.2/"+tag+"/forms/a" {
		t.Fatalf("%q %v", got, err)
	}
	if is, ok := FindIssued(got); !ok || is.Payer != "p/q" || is.Remainder != "/forms/a" {
		t.Fatalf("the written address does not read back: %+v %v", is, ok)
	}
	for _, bad := range [][6]string{
		{"ftp://gw.example", FamilyPayer, "p", "2.2", tag, ""},
		{"https://u@gw.example", FamilyPayer, "p", "2.2", tag, ""},
		{"https://gw.example?q", FamilyPayer, "p", "2.2", tag, ""},
		{"https://gw.example", FamilyPayer, "..", "2.2", tag, ""},
		{"https://gw.example", FamilyPayer, "p q", "2.2", tag, ""},
		{"https://gw.example", FamilyFetchServer, "p", "2.2", tag, "/x"},
		{"https://gw.example", FamilyPayer, "p", "2.2", "short", ""},
		{"https://gw.example", FamilyPayer, "p", "2.2", tag, "/a/"},
	} {
		if _, err := IssuedURL(bad[0], bad[1], bad[2], bad[3], bad[4], bad[5]); err == nil {
			t.Errorf("IssuedURL%q passed", bad)
		}
	}
}
