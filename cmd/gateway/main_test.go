package main

import (
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
