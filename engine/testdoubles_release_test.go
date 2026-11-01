package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// testDoubleFiles are the engine's test doubles and test-only seams: the
// hermetic line validator, the fhirServer read's trust seam, the ingress auth
// bypass and the module-absent opt-outs. Each exists only in a shn_testdouble
// build.
var testDoubleFiles = []string{"conformance_testdouble.go", "fhirserver_fortest.go", "ingress_testdouble.go", "workstream_pa_linefake.go"}

// The test doubles are not in a released gateway: an untagged build of this
// package, as every release is, leaves their files out and does not compile
// against what they declare; a shn_testdouble build does.
func TestTestDoublesOnlyInATestBuild(t *testing.T) {
	t.Parallel()
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal("go tool not available: the build check cannot run")
	}
	out, err := exec.Command(gobin, "list", "-f", "{{join .IgnoredGoFiles \"\\n\"}}", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	ignored := strings.Fields(string(out))
	for _, f := range testDoubleFiles {
		if !slices.Contains(ignored, f) {
			t.Errorf("an untagged build compiles %s (ignored: %q)", f, ignored)
		}
	}
	names := []string{"NewLineFakeValidator", "LineFakeValidator", "LineFakeCall", "WithFHIRServerTrustForTest", "EnableIngressForTest"}
	src := "package engine\n\nvar (\n" +
		"\t_ = NewLineFakeValidator\n\t_ *LineFakeValidator\n\t_ LineFakeCall\n" +
		"\t_ = WithFHIRServerTrustForTest\n\t_ = EnableIngressForTest\n)\n"
	probe := filepath.Join(t.TempDir(), "probe.go")
	if err := os.WriteFile(probe, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(t.TempDir(), "overlay.json")
	if err := os.WriteFile(overlay, []byte(`{"Replace":{"zz_testdouble_probe.go":"`+probe+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name     string
		tags     []string
		compiles bool
	}{
		{"untagged build", nil, false},
		{"shn_testdouble build", []string{"-tags=shn_testdouble"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			args := append(append([]string{"build", "-overlay=" + overlay}, c.tags...), "-o", os.DevNull, ".")
			out, err := exec.Command(gobin, args...).CombinedOutput()
			if (err == nil) != c.compiles {
				t.Fatalf("compiles = %v, want %v:\n%s", err == nil, c.compiles, out)
			}
			if c.compiles {
				return
			}
			for _, name := range names {
				if !strings.Contains(string(out), "undefined: "+name) {
					t.Errorf("the untagged build did not refuse %s:\n%s", name, out)
				}
			}
		})
	}
}
