package buildversion

import (
	"strings"
	"testing"
)

// Each source in its order: a valid stamp wins, an invalid one is never
// guessed at (not even from the module version), and a build with neither
// reads as Unstamped.
func TestResolve(t *testing.T) {
	for _, row := range []struct {
		name, stamp, module string
		want                string
		src                 Source
	}{
		{"a release stamp", "v1.0.0", "(devel)", "v1.0.0", FromStamp},
		{"a main build's pseudo-version", "v1.0.1-0.20261024041500-120bc4c14e22", "(devel)", "v1.0.1-0.20261024041500-120bc4c14e22", FromStamp},
		{"the stamp beats the module version", "v1.0.0", "v0.63.0", "v1.0.0", FromStamp},
		{"no stamp, go install at a tag", "", "v1.0.0", "v1.0.0", FromModule},
		{"no stamp, a build without version control", "", "(devel)", Unstamped, FromNone},
		{"no stamp, no build info", "", "", Unstamped, FromNone},
		{"a stamp that is not a version", "1.0.0", "v1.0.0", Unstamped, FromBadStamp},
		{"a stamp naming a commit", "120bc4c14e22", "", Unstamped, FromBadStamp},
	} {
		got, src := resolve(row.stamp, row.module)
		if got != row.want || src != row.src {
			t.Errorf("%s: resolve(%q, %q) = %q, %q; want %q, %q", row.name, row.stamp, row.module, got, src, row.want, row.src)
		}
	}
}

// The bound is the gateway-start record's on gateway.version, and Unstamped
// meets it, so a build that states none still writes a valid record.
func TestValid(t *testing.T) {
	for v, want := range map[string]bool{
		"v1.0.0": true, "v0.63.0": true, Unstamped: true, "v1.0.1-0.20261024041500-120bc4c14e22": true, "v1.0.0+meta": true,
		"1.0.0": false, "v1.0": false, "(devel)": false, "": false, "v1.0.0-": false, "v1.0.0 ": false,
		"v1.0.0-" + strings.Repeat("a", 64): true, "v1.0.0-" + strings.Repeat("a", 65): false,
	} {
		if Valid(v) != want {
			t.Errorf("Valid(%q) = %v, want %v", v, !want, want)
		}
	}
}
