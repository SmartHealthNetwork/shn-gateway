package engine

import (
	"bytes"
	"fmt"
	"go/ast"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
)

// A leaf at scale is one whose input is a record or a body at the full scale
// of one of the gateway's carrying limits: a million JSON tokens, or the
// request's size bound. Nearly all of its time is one goroutine's scan of that
// input. Each row that builds such an input checks the input's own size
// (atScaleAt), so whether a leaf is at scale is decided by what it sends, not
// by its name. Under -race the detector instruments every byte of the scan, and the
// handler paths the scan runs on are race-checked by the package's other rows
// on the same paths, so a leaf at scale skips in a race build and make
// gateway-at-scale runs it without -race, at every level its test runs.
//
// The two hold each other: a leaf at scale fails, in every build, unless its
// top-level test is in atScaleTestsFile, and gateway-at-scale (which
// runs exactly that list, with SHN_ENGINE_AT_SCALE=1) fails unless every test
// in the list ran a leaf at scale in it.

// raceBuild is whether the test binary is a race build (go test -race):
// atscale_race_test.go sets it.
var raceBuild bool

// atScaleBytes is the size from which an input is at scale. The inputs at
// scale are 2 MB and more; every other row's input is a few kilobytes.
const atScaleBytes = 1 << 20

// atScaleTestsFile lists the top-level tests that hold leaves at scale, one
// name per line, sorted: gateway-at-scale runs exactly these.
const atScaleTestsFile = "testdata/at-scale-tests.txt"

// atScaleEnv is set to 1 by gateway-at-scale alone: TestMain then
// requires every listed test to have run a leaf at scale.
const atScaleEnv = "SHN_ENGINE_AT_SCALE"

var atScaleList = sync.OnceValues(func() ([]string, error) {
	b, err := os.ReadFile(atScaleTestsFile)
	if err != nil {
		return nil, err
	}
	return parseAtScaleTests(b)
})

// atScaleRan is the listed tests that ran a leaf at scale in this binary.
var atScaleRan struct {
	mu   sync.Mutex
	tops map[string]bool
}

var testName = regexp.MustCompile(`^Test[A-Za-z0-9_]*$`)

// parseAtScaleTests reads the list: a name per line, each a top-level test
// name, sorted with no repeats, and at least one. Unknown is not empty: a
// blank line or a list with no name is refused, never read as no tests.
func parseAtScaleTests(b []byte) ([]string, error) {
	if len(b) == 0 || b[len(b)-1] != '\n' {
		return nil, fmt.Errorf("%s: want one name per line, each ending in a newline", atScaleTestsFile)
	}
	var tops []string
	for i, line := range strings.Split(string(bytes.TrimSuffix(b, []byte("\n"))), "\n") {
		switch {
		case !testName.MatchString(line):
			return nil, fmt.Errorf("%s line %d: %q is not a top-level test name", atScaleTestsFile, i+1, line)
		case len(tops) > 0 && line <= tops[len(tops)-1]:
			return nil, fmt.Errorf("%s line %d: %s is not after %s: the list is sorted with no repeats", atScaleTestsFile, i+1, line, tops[len(tops)-1])
		}
		tops = append(tops, line)
	}
	return tops, nil
}

// atScaleUnlisted is the failure of a leaf at scale whose top-level test is
// not listed: a race build would skip it and gateway-at-scale would not
// run it, so no build would.
func atScaleUnlisted(tops []string, top string) error {
	if slices.Contains(tops, top) {
		return nil
	}
	return fmt.Errorf("%s runs a leaf at scale but is not in %s: a race build skips it and gateway-at-scale does not run it", top, atScaleTestsFile)
}

// atScaleRunProblems is what keeps a run of gateway-at-scale from
// having run every listed test's leaves at scale: a race build, which skips
// them all, or a listed test that ran none (renamed, removed, or with no
// input at scale left).
func atScaleRunProblems(tops []string, ran map[string]bool, race bool) []string {
	if race {
		return []string{atScaleEnv + "=1 in a race build: every leaf at scale skips there"}
	}
	var problems []string
	for _, top := range tops {
		if !ran[top] {
			problems = append(problems, top+" is in "+atScaleTestsFile+" but ran no leaf at scale")
		}
	}
	return problems
}

// atScale marks t as a leaf at scale: it fails unless its top-level test is
// listed, skips in a race build, and otherwise records that the test ran one.
func atScale(t *testing.T) {
	t.Helper()
	tops, err := atScaleList()
	if err != nil {
		t.Fatal(err)
	}
	top, _, _ := strings.Cut(t.Name(), "/")
	if err := atScaleUnlisted(tops, top); err != nil {
		t.Fatal(err)
	}
	if raceBuild {
		t.Skip("at scale: make gateway-at-scale runs this leaf without -race")
	}
	atScaleRan.mu.Lock()
	defer atScaleRan.mu.Unlock()
	if atScaleRan.tops == nil {
		atScaleRan.tops = map[string]bool{}
	}
	atScaleRan.tops[top] = true
}

// atScaleAt marks t as a leaf at scale when its input of n bytes is.
func atScaleAt(t *testing.T, n int) {
	t.Helper()
	if n >= atScaleBytes {
		atScale(t)
	}
}

// atScaleRunCheck is TestMain's check after the run: under atScaleEnv, the
// problems with the run as gateway-at-scale's.
func atScaleRunCheck() []string {
	if os.Getenv(atScaleEnv) != "1" {
		return nil
	}
	tops, err := atScaleList()
	if err != nil {
		return []string{err.Error()}
	}
	atScaleRan.mu.Lock()
	defer atScaleRan.mu.Unlock()
	return atScaleRunProblems(tops, atScaleRan.tops, raceBuild)
}

// The list as checked in reads, and names only top-level tests the package
// defines: one it names that is not defined would never run. It reads the
// package's source, so it holds in every build, race builds included.
func TestAtScale_TheListNamesThePackagesTests(t *testing.T) {
	t.Parallel()
	tops, err := atScaleList()
	if err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob("*_test.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no test files: %v", err)
	}
	sources := map[string]string{}
	for _, f := range files {
		sources[f] = ""
	}
	for _, p := range atScaleUndefined(tops, topFuncs(t, sources)) {
		t.Error(p)
	}
}

// atScaleUndefined is each listed name the package's top-level functions do
// not define: a test the list names that is not defined never runs.
func atScaleUndefined(tops []string, funcs map[string]*ast.FuncDecl) []string {
	var problems []string
	for _, top := range tops {
		if _, ok := funcs[top]; !ok {
			problems = append(problems, atScaleTestsFile+" names "+top+", which the package does not define")
		}
	}
	return problems
}

// REJECTION: each way the list, a leaf at scale or a run of
// gateway-at-scale can fail to hold the other is named.
func TestAtScaleProblems(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ list, want string }{
		"an empty list":       {"", "want one name per line"},
		"no final newline":    {"TestA", "want one name per line"},
		"a blank line":        {"TestA\n\nTestB\n", `line 2: "" is not a top-level test name`},
		"not a test":          {"TestA\nExampleB\n", `line 2: "ExampleB" is not a top-level test name`},
		"a subtest":           {"TestA/x\n", `line 1: "TestA/x" is not a top-level test name`},
		"out of order":        {"TestB\nTestA\n", "line 2: TestA is not after TestB"},
		"a name twice":        {"TestA\nTestA\n", "line 2: TestA is not after TestA"},
		"spaces around names": {" TestA\n", `line 1: " TestA" is not a top-level test name`},
	} {
		t.Run("list/"+name, func(t *testing.T) {
			if _, err := parseAtScaleTests([]byte(c.list)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("parse %q = %v, want %q", c.list, err, c.want)
			}
		})
	}
	if tops, err := parseAtScaleTests([]byte("TestA\nTestB\n")); err != nil || !slices.Equal(tops, []string{"TestA", "TestB"}) {
		t.Fatalf("control list = %v, %v", tops, err)
	}
	tops := []string{"TestA", "TestB"}
	if err := atScaleUnlisted(tops, "TestA"); err != nil {
		t.Fatalf("listed: %v", err)
	}
	if err := atScaleUnlisted(tops, "TestC"); err == nil || !strings.Contains(err.Error(), "TestC runs a leaf at scale but is not in") {
		t.Fatalf("unlisted: %v", err)
	}
	defined := map[string]*ast.FuncDecl{"TestA": nil, "TestB": nil}
	if p := atScaleUndefined(tops, defined); len(p) != 0 {
		t.Fatalf("control names: %v", p)
	}
	if p := atScaleUndefined(tops, map[string]*ast.FuncDecl{"TestA": nil}); !slices.Equal(p, []string{atScaleTestsFile + " names TestB, which the package does not define"}) {
		t.Fatalf("a listed name the package does not define: %v", p)
	}
	all := map[string]bool{"TestA": true, "TestB": true}
	if p := atScaleRunProblems(tops, all, false); len(p) != 0 {
		t.Fatalf("control run: %v", p)
	}
	for name, c := range map[string]struct {
		ran  map[string]bool
		race bool
		want string
	}{
		"a listed test that ran none": {map[string]bool{"TestA": true}, false, "TestB is in " + atScaleTestsFile + " but ran no leaf at scale"},
		"no listed test ran":          {nil, false, "TestA is in " + atScaleTestsFile + " but ran no leaf at scale"},
		"a race build":                {all, true, atScaleEnv + "=1 in a race build: every leaf at scale skips there"},
	} {
		t.Run("run/"+name, func(t *testing.T) {
			if p := atScaleRunProblems(tops, c.ran, c.race); !slices.Contains(p, c.want) {
				t.Fatalf("problems %q, want %q", p, c.want)
			}
		})
	}
}
