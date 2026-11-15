package subscription

import (
	"strings"
	"testing"
)

// mutate flips one bit of s's bytes, at pos and bit taken modulo the length
// and a byte. ok is false for an empty s.
func mutate(s string, pos uint16, bit uint8) (string, bool) {
	if s == "" {
		return "", false
	}
	b := []byte(s)
	b[int(pos)%len(b)] ^= 1 << (bit % 8)
	return string(b), true
}

// FuzzOpenEndpoint: opening never panics; whatever opens is exactly the value
// SealEndpoint makes for what it opened to (a SIV value is canonical), under
// the key its key id names; and a valid value with one bit flipped anywhere
// never opens.
func FuzzOpenEndpoint(f *testing.F) {
	k := mustKeys(f, katHolder, curKey, predKey)
	pred := mustKeys(f, katHolder, predKey, nil)
	for _, s := range []string{k.SealEndpoint(katBinding, katEndpoint), pred.SealEndpoint(katBinding, katEndpoint), "", ".", "AAAAAAAAAAA.", "AAAAAAAAAAA.AAAA"} {
		f.Add(s, uint16(0), uint8(0))
	}
	f.Add(katEndpoint, uint16(13), uint8(3))
	f.Fuzz(func(t *testing.T, s string, pos uint16, bit uint8) {
		if pt, err := k.OpenEndpoint(katBinding, s); err == nil {
			if s != k.SealEndpoint(katBinding, pt) && s != pred.SealEndpoint(katBinding, pt) {
				t.Fatalf("opened %q, which no key seals %q to", s, pt)
			}
		}
		if s == "" {
			return
		}
		valid := k.SealEndpoint(katBinding, s)
		if m, ok := mutate(valid, pos, bit); ok {
			if pt, err := k.OpenEndpoint(katBinding, m); err == nil {
				t.Fatalf("a mutated value %q opened to %q", m, pt)
			}
		}
	})
}

// FuzzOpenHeaders: opening never panics; nothing opens but a value
// SealHeaders made; and a valid value with one bit flipped anywhere never
// opens.
func FuzzOpenHeaders(f *testing.F) {
	k := mustKeys(f, katHolder, curKey, predKey)
	sealed := k.SealEndpoint(katBinding, katEndpoint)
	// The seed's valid value is the same in every fuzzing process: its nonce
	// is fixed. Every seal made while fuzzing takes a fresh random nonce.
	prev := randReader
	randReader = &countingReader{next: 0x10}
	valid, err := k.SealHeaders(katBinding, sealed, katHeaders)
	randReader = prev
	if err != nil {
		f.Fatal(err)
	}
	for _, s := range []string{valid, "", ".", "AAAAAAAAAAA.", k.KeyID() + ".AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
		f.Add(s, uint16(0), uint8(0))
	}
	f.Add("a: b\nc: d", uint16(40), uint8(7))
	f.Fuzz(func(t *testing.T, s string, pos uint16, bit uint8) {
		if hs, err := k.OpenHeaders(katBinding, sealed, s); err == nil && s != valid {
			t.Fatalf("opened %q to %q, which no seal made", s, hs)
		}
		v, err := k.SealHeaders(katBinding, sealed, strings.Split(s, "\n"))
		if err != nil {
			t.Fatal(err)
		}
		if m, ok := mutate(v, pos, bit); ok {
			if hs, err := k.OpenHeaders(katBinding, sealed, m); err == nil {
				t.Fatalf("a mutated value %q opened to %q", m, hs)
			}
		}
	})
}

// FuzzVerifyCredential: verifying never panics; nothing verifies but the
// credential Credential issues for the address and sealed headers (its scheme
// in any case); and a valid credential with one bit flipped anywhere but the
// scheme's letter case never verifies.
func FuzzVerifyCredential(f *testing.F) {
	k := mustKeys(f, "payer-1", curKey, predKey)
	sub := mustKeys(f, "provider-1", altKey, nil)
	a := katAddress(sub.SealEndpoint(katBinding, katEndpoint))
	prev := randReader
	randReader = &countingReader{next: 0x30}
	sh, err := sub.SealHeaders(katBinding, a.SealedEndpoint, katHeaders)
	randReader = prev
	if err != nil {
		f.Fatal(err)
	}
	for _, s := range []string{k.Credential(a, sh), "", "Bearer ", "Bearer AAAAAAAAAAA.AAAAAAAAAAAAAAAAAAAAAA"} {
		f.Add(s, sh, uint16(0), uint8(0))
	}
	f.Fuzz(func(t *testing.T, s, sealedHeaders string, pos uint16, bit uint8) {
		valid := k.Credential(a, sealedHeaders)
		accepted := func(v string) bool {
			scheme, tok, _ := strings.Cut(v, " ")
			_, want, _ := strings.Cut(valid, " ")
			return strings.EqualFold(scheme, "Bearer") && tok == want
		}
		if err := k.VerifyCredential(a, sealedHeaders, s); err == nil && !accepted(s) {
			t.Fatalf("verified %q, which is not the issued credential %q", s, valid)
		}
		if m, ok := mutate(valid, pos, bit); ok {
			if err := k.VerifyCredential(a, sealedHeaders, m); err == nil && !accepted(m) {
				t.Fatalf("a mutated credential %q verified", m)
			}
		}
	})
}
