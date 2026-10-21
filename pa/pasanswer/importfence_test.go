package pasanswer

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// enginePackage is the gateway engine package itself. Package pasanswer is a
// leaf of the Prior Authorization workstream: it reads an answer through
// internal/pasgraph and must not depend on the engine it moved from, directly
// or through any package it imports. The engine's own subpackages
// (engine/relay and the like) are core primitives and are allowed.
const enginePackage = "github.com/SmartHealthNetwork/shn-gateway/engine"

// engineDependencies names each entry of a `go list -deps` listing that is
// the engine package, a test variant of it ("<path> [<pkg>.test]") included.
func engineDependencies(deps []string) []string {
	var found []string
	for _, dep := range deps {
		path, _, _ := strings.Cut(dep, " ")
		if path == enginePackage {
			found = append(found, dep)
		}
	}
	return found
}

// The package, its tests included, depends on no engine package, at any depth.
func TestImportFenceNoEngineDependency(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "-test", "-f", "{{.ImportPath}}", ".")
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOPROXY=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	deps := strings.Split(strings.TrimSpace(string(out)), "\n")
	// The listing is this package's: it names the package and the graph rule
	// it reads by, so an empty or foreign listing cannot pass.
	for _, want := range []string{
		"github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer",
		"github.com/SmartHealthNetwork/shn-gateway/internal/pasgraph",
	} {
		if !slices.Contains(deps, want) {
			t.Fatalf("go list did not list %s; the listing is not this package's:\n%s", want, out)
		}
	}
	if got := engineDependencies(deps); len(got) != 0 {
		t.Fatalf("package pasanswer depends on the engine: %v", got)
	}
}

// The check refuses a listing naming the engine package, plain or as a test
// variant, and allows the engine's subpackages and a path that only begins
// with the engine's.
func TestImportFenceCheckRows(t *testing.T) {
	for name, row := range map[string]struct {
		deps    []string
		refused bool
	}{
		"the engine":              {[]string{"errors", enginePackage, enginePackage + "/relay"}, true},
		"the engine test variant": {[]string{enginePackage + " [github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer.test]"}, true},
		"relay is allowed":        {[]string{"errors", enginePackage + "/relay", enginePackage + "/relay/internal/splice"}, false},
		"a longer path":           {[]string{enginePackage + "x"}, false},
	} {
		if got := engineDependencies(row.deps); (len(got) != 0) != row.refused {
			t.Errorf("%s: the check found %v, refused want %v", name, got, row.refused)
		}
	}
}
