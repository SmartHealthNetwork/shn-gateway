package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// goListFiles is the non-test Go files the go command compiles in dir under
// this test binary's build profile tags; env is added to the command's
// environment.
func goListFiles(t *testing.T, dir string, env ...string) []string {
	t.Helper()
	args := []string{"list", "-f", `{{join .GoFiles " "}}`}
	if len(thisBuildTags) > 0 {
		args = append(args, "-tags", strings.Join(thisBuildTags, ","))
	}
	cmd := exec.Command("go", append(args, ".")...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list in %s: %v", dir, err)
	}
	files := strings.Fields(string(out))
	slices.Sort(files)
	return files
}

// inThisBuildFiles is the non-test Go files of dir inThisBuild selects.
func inThisBuildFiles(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if !e.IsDir() && strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") && inThisBuild(t, dir, n) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// TestInThisBuild_AgreesWithTheGoCommand: the files inThisBuild selects are
// the files the go command compiles under the same tags, for this package
// and for a probe package whose files combine the build profile tag with
// operating system tags, negation, conjunction and disjunction.
func TestInThisBuild_AgreesWithTheGoCommand(t *testing.T) {
	if got, want := inThisBuildFiles(t, "."), goListFiles(t, "."); !slices.Equal(got, want) || len(want) < 50 {
		t.Errorf("this package: inThisBuild selects %v, the go command compiles %v", got, want)
	}
	dir := t.TempDir()
	probe := map[string]string{
		"none.go":       "package probe\n",
		"notoy_os.go":   "//go:build !shn_toyworkstream && linux\n\npackage probe\n",
		"toy_or.go":     "//go:build shn_toyworkstream || darwin\n\npackage probe\n",
		"neither.go":    "//go:build !linux && !darwin && !windows\n\npackage probe\n",
		"toy_notwin.go": "//go:build shn_toyworkstream && !windows\n\npackage probe\n",
		"unix_only.go":  "//go:build unix && !shn_toyworkstream\n\npackage probe\n",
	}
	for name, src := range probe {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The probe is a package outside any module, which the go command lists
	// in GOPATH mode. Temporary seam: it would be a temporary module, but a
	// go.mod literal here would read as reaching the repository root to the
	// CI router (tracked in the SHN platform repository: "gateroute: a
	// temp-module literal in a test is not root reach"). When that is fixed,
	// the probe writes its own go.mod and lists in module mode; either way
	// the go command decides the same files.
	if got, want := inThisBuildFiles(t, dir), goListFiles(t, dir, "GO111MODULE=off"); !slices.Equal(got, want) || len(want) < 2 {
		t.Errorf("probe package: inThisBuild selects %v, the go command compiles %v", got, want)
	}
}
