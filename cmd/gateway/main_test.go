//go:build !shn_coreonly

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The binary a release ships (an untagged go build, as the published image,
// go install and the release's reproducible build all are) refuses to boot on the
// validator test double; only a build tagged shn_testdouble boots on it.
func TestOnlyATestDoubleBuildBootsOnTheValidatorTestDouble(t *testing.T) {
	const refusal = "a released gateway has no validator test double"
	for _, c := range []struct {
		name    string
		tags    []string
		refuses bool
	}{
		{"untagged build", nil, true},
		{"shn_testdouble build", []string{"-tags", "shn_testdouble"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "gateway")
			build := exec.Command("go", append(append([]string{"build"}, c.tags...), "-o", bin, ".")...)
			if out, err := build.CombinedOutput(); err != nil {
				t.Fatalf("go build: %v\n%s", err, out)
			}
			run := exec.Command(bin)
			run.Env = []string{
				"SHN_FAKE_VALIDATOR=1",
				"ROLE=provider",
				"SHN_SECRETS=" + t.TempDir(),
				"SHN_DISCOVERY_URL=http://127.0.0.1:9/discovery",
				"FHIR_VALIDATE_URL=http://127.0.0.1:9/fhir",
			}
			out, err := run.CombinedOutput()
			if err == nil {
				t.Fatalf("the gateway booted with an empty bundle:\n%s", out)
			}
			if got := strings.Contains(string(out), refusal); got != c.refuses {
				t.Fatalf("refused on the test double = %v, want %v:\n%s", got, c.refuses, out)
			}
		})
	}
}

// `gateway -version` alone prints the build's version and its source; any
// other arguments start the gateway as before.
func TestPrintVersion(t *testing.T) {
	var b strings.Builder
	if !printVersion([]string{"-version"}, &b) {
		t.Fatal("-version did not answer")
	}
	if got := b.String(); got != "v0.0.0-unstamped unstamped\n" {
		t.Fatalf("an unstamped test build printed %q", got)
	}
	for _, args := range [][]string{nil, {}, {"-version", "x"}, {"--version"}, {"version"}, {"-v"}} {
		b.Reset()
		if printVersion(args, &b) || b.Len() != 0 {
			t.Errorf("%q answered %q", args, b.String())
		}
	}
}

// The built binary answers -version with an empty environment, from an empty
// directory, before it reads any setting, file or network: stamped by -X as
// the image builds it, and unstamped without it.
func TestVersionAnswersWithNothingConfigured(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the gateway binary")
	}
	const sym = "github.com/SmartHealthNetwork/shn-gateway/internal/buildversion.stamp"
	for _, row := range []struct{ ldflags, want string }{
		{"-s -w -X " + sym + "=v1.2.3", "v1.2.3 stamp\n"},
		{"-s -w", "v0.0.0-unstamped unstamped\n"},
	} {
		bin := filepath.Join(t.TempDir(), "gateway")
		build := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-ldflags="+row.ldflags, "-o", bin, ".")
		build.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build: %v\n%s", err, out)
		}
		run := exec.Command(bin, "-version")
		run.Env = []string{}
		run.Dir = t.TempDir()
		out, err := run.Output()
		if err != nil || string(out) != row.want {
			t.Fatalf("-version with %q: %q, %v; want %q and exit 0", row.ldflags, out, err, row.want)
		}
	}
}
