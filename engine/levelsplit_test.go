package engine

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// levelSplitTests are the table tests that run one conformance level per
// top-level test, so that the engine's hash shards (tools/ci/test-shard.sh
// -hash, which splits by top-level test name) spread their rows rather than
// hold every level's rows in one shard. Together their levels must still be
// every level, each exactly once.
var levelSplitTests = []string{
	"TestEligibility_ContainedPatientPastTheTokenBound",
	"TestProviderData_EvidenceAboutAnotherPatientIsRefused",
	"TestProviderData_TheResumeRefusesEvidenceAboutAnotherPatient",
}

// levelSplitProblems reads funcs, the package's top-level functions by name,
// and returns what keeps each of bases from running its rows at every level
// named in levels (ConformanceLevels' names): base+"At"+Level must exist for each level and for no other name, and
// each must be the one statement helper(t, EnforcementLevel), the same helper
// for every level. The helper must read its level parameter and range over no
// level list, so each test runs its own level only. The whole test, base
// itself, must be gone, so no level runs twice.
func levelSplitProblems(funcs map[string]*ast.FuncDecl, bases []string, levels []string) []string {
	var problems []string
	for _, base := range bases {
		if _, ok := funcs[base]; ok {
			problems = append(problems, base+" runs whole beside its levels")
		}
		want := map[string]string{} // test name -> the level constant it passes
		for _, l := range levels {
			name := strings.ToUpper(l[:1]) + l[1:]
			want[base+"At"+name] = "Enforcement" + name
		}
		for name := range funcs {
			if rest, ok := strings.CutPrefix(name, base+"At"); ok && rest != "" && want[name] == "" {
				problems = append(problems, name+" is no level of "+base)
			}
		}
		helper := ""
		names := make([]string, 0, len(want))
		for name := range want {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			fd, ok := funcs[name]
			if !ok {
				problems = append(problems, name+" is missing: "+base+" does not run at "+strings.TrimPrefix(want[name], "Enforcement"))
				continue
			}
			call := onlyCall(fd)
			if call == nil {
				problems = append(problems, name+" is not one call of its helper")
				continue
			}
			fn, _ := call.Fun.(*ast.Ident)
			var args []string
			for _, a := range call.Args {
				id, _ := a.(*ast.Ident)
				if id == nil {
					args = append(args, "?")
					continue
				}
				args = append(args, id.Name)
			}
			if fn == nil || !slices.Equal(args, []string{"t", want[name]}) {
				problems = append(problems, fmt.Sprintf("%s calls %s, want helper(t, %s)", name, strings.Join(args, ", "), want[name]))
				continue
			}
			if helper == "" {
				helper = fn.Name
			} else if fn.Name != helper {
				problems = append(problems, fmt.Sprintf("%s calls %s, the other levels %s", name, fn.Name, helper))
			}
		}
		if helper != "" {
			problems = append(problems, helperProblems(funcs[helper], helper)...)
		}
	}
	return problems
}

// helperProblems reports a level-split helper that does not run exactly the
// level it is given: it must take (t, level) and read level, and range over
// no level list (allLevels or another), which would run every level in each
// test.
func helperProblems(fd *ast.FuncDecl, name string) []string {
	if fd == nil {
		return []string{name + " is not defined in the package"}
	}
	params := fd.Type.Params.List
	if len(params) != 2 || len(params[1].Names) != 1 {
		return []string{name + " does not take (t, level)"}
	}
	level := params[1].Names[0].Name
	var problems []string
	reads := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Ident:
			if n.Name == level {
				reads = true
			}
		case *ast.RangeStmt:
			if id, ok := n.X.(*ast.Ident); ok && strings.HasSuffix(id.Name, "Levels") {
				problems = append(problems, name+" ranges over "+id.Name+": each test would run every level")
			}
		}
		return true
	})
	if !reads {
		problems = append(problems, name+" never reads "+level+": every test would run one level")
	}
	return problems
}

// onlyCall is fd's body when it is exactly one call statement.
func onlyCall(fd *ast.FuncDecl) *ast.CallExpr {
	if fd.Body == nil || len(fd.Body.List) != 1 {
		return nil
	}
	es, ok := fd.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return nil
	}
	call, _ := es.X.(*ast.CallExpr)
	return call
}

// topFuncs parses each source and returns its top-level functions by name.
func topFuncs(t *testing.T, sources map[string]string) map[string]*ast.FuncDecl {
	t.Helper()
	fset := token.NewFileSet()
	funcs := map[string]*ast.FuncDecl{}
	for name, src := range sources {
		var s any
		if src != "" {
			s = src
		}
		f, err := parser.ParseFile(fset, name, s, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil {
				funcs[fd.Name.Name] = fd
			}
		}
	}
	return funcs
}

// The engine's level-split table tests run every conformance level, each once,
// through one helper. It reads the package's source, so it holds in every
// build profile.
func TestLevelSplitTestsRunEveryLevel(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*_test.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no test files: %v", err)
	}
	sources := map[string]string{}
	for _, f := range files {
		sources[f] = ""
	}
	if p := levelSplitProblems(topFuncs(t, sources), levelSplitTests, ConformanceLevels); len(p) != 0 {
		t.Fatalf("level-split tests:\n%s", strings.Join(p, "\n"))
	}
}

// REJECTION: each way a split can lose, repeat or mislabel a level is named.
func TestLevelSplitProblems(t *testing.T) {
	t.Parallel()
	levels := []string{"none", "strict"}
	const control = `package engine
func TestXAtNone(t *testing.T) { x(t, EnforcementNone) }
func TestXAtStrict(t *testing.T) { x(t, EnforcementStrict) }
func x(t *testing.T, level ConformanceEnforcement) { t.Run(level.String(), nil) }
func y(t *testing.T, level ConformanceEnforcement) { t.Run(level.String(), nil) }
`
	if p := levelSplitProblems(topFuncs(t, map[string]string{"c.go": control}), []string{"TestX"}, levels); len(p) != 0 {
		t.Fatalf("control: %v", p)
	}
	for name, c := range map[string]struct{ old, new, want string }{
		"a level missing":                 {"func TestXAtStrict(t *testing.T) { x(t, EnforcementStrict) }\n", "", "TestXAtStrict is missing: TestX does not run at Strict"},
		"another level's constant":        {"x(t, EnforcementStrict)", "x(t, EnforcementNone)", "TestXAtStrict calls t, EnforcementNone, want helper(t, EnforcementStrict)"},
		"another helper":                  {"x(t, EnforcementStrict)", "y(t, EnforcementStrict)", "TestXAtStrict calls y, the other levels x"},
		"more than the call":              {"{ x(t, EnforcementStrict) }", "{ x(t, EnforcementStrict); x(t, EnforcementNone) }", "TestXAtStrict is not one call of its helper"},
		"a computed level":                {"x(t, EnforcementStrict)", "x(t, levels[0])", "TestXAtStrict calls t, ?, want helper(t, EnforcementStrict)"},
		"a name that is no level":         {"func TestXAtNone", "func TestXAtLoose(t *testing.T) { x(t, EnforcementNone) }\nfunc TestXAtNone", "TestXAtLoose is no level of TestX"},
		"the whole test left beside":      {"func TestXAtNone", "func TestX(t *testing.T) {}\nfunc TestXAtNone", "TestX runs whole beside its levels"},
		"a helper that ignores its level": {"{ t.Run(level.String(), nil) }\nfunc y", "{ t.Run(\"one\", nil) }\nfunc y", "x never reads level: every test would run one level"},
		"a helper that runs every level":  {"{ t.Run(level.String(), nil) }\nfunc y", "{ for _, l := range allLevels { t.Run(l.String()+level.String(), nil) } }\nfunc y", "x ranges over allLevels: each test would run every level"},
		"a helper that is not defined":    {"func x(t *testing.T, level ConformanceEnforcement) { t.Run(level.String(), nil) }\n", "", "x is not defined in the package"},
	} {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(control, c.old) {
				t.Fatalf("control has no %q", c.old)
			}
			src := strings.Replace(control, c.old, c.new, 1)
			p := levelSplitProblems(topFuncs(t, map[string]string{"c.go": src}), []string{"TestX"}, levels)
			if !slices.Contains(p, c.want) {
				t.Fatalf("problems %q, want %q", p, c.want)
			}
		})
	}
}
