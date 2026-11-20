package fhirid

import (
	"strings"
	"testing"
)

// A FHIR id the gateway writes into a path or a search is 1 to 64 of
// [A-Za-z0-9-.], and never the dot segment "." or "..": a server or proxy that
// cleans a request's path reads those as a step in it, and could answer for
// another resource or a history instead.
func TestValid(t *testing.T) {
	for _, id := range []string{"a", "MBR-001", "p.1", "...", "a..", "..a", "A-Z.0-9", strings.Repeat("x", 64)} {
		if !Valid(id) {
			t.Errorf("Valid(%q) = false; want true", id)
		}
	}
	for _, id := range []string{"", ".", "..", strings.Repeat("x", 65), "a/b", "a b", "a,b", "a|b", "a_b", "a?b", "a#b", "a%2eb", "é", "a\x00"} {
		if Valid(id) {
			t.Errorf("Valid(%q) = true; want false", id)
		}
	}
}
