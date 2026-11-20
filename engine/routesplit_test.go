package engine

import (
	"fmt"
	"go/ast"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// pinnedSetRoles are the roles the routes table holds to their pinned sets:
// the four a gateway serves, none, and one no gateway knows.
var pinnedSetRoles = []string{"provider", "payer", "facility", "phg", "", "unknown"}

// pinnedSetRoleTests are the routes table's per-role tests, by the role each
// runs, so that the engine's hash shards (tools/ci/test-shard.sh -hash) spread
// the table's gateways rather than hold every role's in one shard. Together
// they must run every one of pinnedSetRoles, each exactly once.
var pinnedSetRoleTests = map[string]string{
	"TestRoutes_TheProviderMountsItsPinnedSet":   "provider",
	"TestRoutes_ThePayerMountsItsPinnedSet":      "payer",
	"TestRoutes_TheFacilityMountsItsPinnedSet":   "facility",
	"TestRoutes_ThePHGMountsItsPinnedSet":        "phg",
	"TestRoutes_NoRoleMountsItsPinnedSet":        "",
	"TestRoutes_AnUnknownRoleMountsItsPinnedSet": "unknown",
}

// pinnedSetWholeTest is the table test the per-role tests replace.
const pinnedSetWholeTest = "TestRoutes_EachRoleMountsItsPinnedSet"

// pinnedSetSplitProblems reads funcs, the package's top-level functions by
// name, and returns what keeps tests (test name -> role) from running every
// one of roles exactly once:
//   - each test must be the one statement helper(t, "<its role>"), the same
//     helper for every test;
//   - no other top-level test may end in MountsItsPinnedSet;
//   - the whole test must be gone, so no role runs twice;
//   - the roles the tests name must be roles, each once;
//   - the helper must take (t, role), read role, and range over no role list,
//     so each test runs its own role only.
func pinnedSetSplitProblems(funcs map[string]*ast.FuncDecl, tests map[string]string, roles []string) []string {
	var problems []string
	if _, ok := funcs[pinnedSetWholeTest]; ok {
		problems = append(problems, pinnedSetWholeTest+" runs every role beside the per-role tests")
	}
	for name := range funcs {
		if _, ok := tests[name]; !ok && strings.HasPrefix(name, "Test") && strings.HasSuffix(name, "MountsItsPinnedSet") && name != pinnedSetWholeTest {
			problems = append(problems, name+" is no per-role test of the routes table")
		}
	}
	ran := map[string]int{}
	for _, role := range tests {
		ran[role]++
	}
	for _, role := range roles {
		switch ran[role] {
		case 0:
			problems = append(problems, fmt.Sprintf("no per-role test runs role %q", role))
		case 1:
		default:
			problems = append(problems, fmt.Sprintf("%d per-role tests run role %q", ran[role], role))
		}
	}
	for role := range ran {
		if !slices.Contains(roles, role) {
			problems = append(problems, fmt.Sprintf("role %q is no role of the routes table", role))
		}
	}
	names := make([]string, 0, len(tests))
	for name := range tests {
		names = append(names, name)
	}
	slices.Sort(names)
	helper := ""
	for _, name := range names {
		fd, ok := funcs[name]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s is missing: role %q does not run", name, tests[name]))
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
			switch a := a.(type) {
			case *ast.Ident:
				args = append(args, a.Name)
			case *ast.BasicLit:
				args = append(args, a.Value)
			default:
				args = append(args, "?")
			}
		}
		if fn == nil {
			problems = append(problems, name+" calls no helper by name")
			continue
		}
		if want := []string{"t", strconv.Quote(tests[name])}; !slices.Equal(args, want) {
			problems = append(problems, fmt.Sprintf("%s calls %s, want helper(t, %s)", name, strings.Join(args, ", "), want[1]))
			continue
		}
		if helper == "" {
			helper = fn.Name
		} else if fn.Name != helper {
			problems = append(problems, fmt.Sprintf("%s calls %s, the other roles %s", name, fn.Name, helper))
		}
	}
	if helper != "" {
		problems = append(problems, roleHelperProblems(funcs[helper], helper)...)
	}
	return problems
}

// roleHelperProblems reports a per-role helper that does not run exactly the
// role it is given: it must take (t, role) and read role, and range over no
// role list, which would run every role in each test.
func roleHelperProblems(fd *ast.FuncDecl, name string) []string {
	if fd == nil {
		return []string{name + " is not defined in the package"}
	}
	params := fd.Type.Params.List
	if len(params) != 2 || len(params[1].Names) != 1 {
		return []string{name + " does not take (t, role)"}
	}
	role := params[1].Names[0].Name
	var problems []string
	reads := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Ident:
			if n.Name == role {
				reads = true
			}
		case *ast.RangeStmt:
			switch x := n.X.(type) {
			case *ast.Ident:
				if strings.HasSuffix(x.Name, "Roles") {
					problems = append(problems, name+" ranges over "+x.Name+": each test would run every role")
				}
			case *ast.CompositeLit:
				problems = append(problems, name+" ranges over a list it builds: each test would run more than its role")
			}
		}
		return true
	})
	if !reads {
		problems = append(problems, name+" never reads "+role+": every test would run one role")
	}
	return problems
}

// The routes table's per-role tests run every role, each once, through one
// helper. It reads the package's source, so it holds in every build profile.
// It reads the test files from disk, so a go test -overlay of one does not
// reach it: a mutation of the split is proven on a copy of the module.
func TestRoutes_PinnedSetSplitRunsEveryRole(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*_test.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no test files: %v", err)
	}
	sources := map[string]string{}
	for _, f := range files {
		sources[f] = ""
	}
	if p := pinnedSetSplitProblems(topFuncs(t, sources), pinnedSetRoleTests, pinnedSetRoles); len(p) != 0 {
		t.Fatalf("per-role routes tests:\n%s", strings.Join(p, "\n"))
	}
}

// REJECTION: each way the split can lose, repeat or mislabel a role is named.
func TestPinnedSetSplitProblems(t *testing.T) {
	t.Parallel()
	roles := []string{"provider", ""}
	tests := map[string]string{"TestRoutes_PMountsItsPinnedSet": "provider", "TestRoutes_NMountsItsPinnedSet": ""}
	const control = `package engine
func TestRoutes_PMountsItsPinnedSet(t *testing.T) { x(t, "provider") }
func TestRoutes_NMountsItsPinnedSet(t *testing.T) { x(t, "") }
func x(t *testing.T, role string) { t.Run(role, nil) }
func y(t *testing.T, role string) { t.Run(role, nil) }
`
	if p := pinnedSetSplitProblems(topFuncs(t, map[string]string{"c.go": control}), tests, roles); len(p) != 0 {
		t.Fatalf("control: %v", p)
	}
	for name, c := range map[string]struct{ old, new, want string }{
		"a role's test missing":          {"func TestRoutes_NMountsItsPinnedSet(t *testing.T) { x(t, \"\") }\n", "", `TestRoutes_NMountsItsPinnedSet is missing: role "" does not run`},
		"another role's literal":         {`x(t, "")`, `x(t, "provider")`, `TestRoutes_NMountsItsPinnedSet calls t, "provider", want helper(t, "")`},
		"another helper":                 {`x(t, "")`, `y(t, "")`, "TestRoutes_PMountsItsPinnedSet calls x, the other roles y"},
		"more than the call":             {`{ x(t, "") }`, `{ x(t, ""); x(t, "provider") }`, "TestRoutes_NMountsItsPinnedSet is not one call of its helper"},
		"a computed role":                {`x(t, "")`, `x(t, roles[1])`, `TestRoutes_NMountsItsPinnedSet calls t, ?, want helper(t, "")`},
		"a test that is no role's":       {"func TestRoutes_PMountsItsPinnedSet", "func TestRoutes_QMountsItsPinnedSet(t *testing.T) { x(t, \"payer\") }\nfunc TestRoutes_PMountsItsPinnedSet", "TestRoutes_QMountsItsPinnedSet is no per-role test of the routes table"},
		"the whole test left beside":     {"func TestRoutes_PMountsItsPinnedSet", "func " + pinnedSetWholeTest + "(t *testing.T) {}\nfunc TestRoutes_PMountsItsPinnedSet", pinnedSetWholeTest + " runs every role beside the per-role tests"},
		"a helper that ignores its role": {"{ t.Run(role, nil) }\nfunc y", "{ t.Run(\"one\", nil) }\nfunc y", "x never reads role: every test would run one role"},
		"a helper that runs every role":  {"{ t.Run(role, nil) }\nfunc y", "{ for _, r := range pinnedSetRoles { t.Run(r+role, nil) } }\nfunc y", "x ranges over pinnedSetRoles: each test would run every role"},
		"a helper that runs a list":      {"{ t.Run(role, nil) }\nfunc y", "{ for _, r := range []string{role, \"payer\"} { t.Run(r, nil) } }\nfunc y", "x ranges over a list it builds: each test would run more than its role"},
		"a helper that is not defined":   {"func x(t *testing.T, role string) { t.Run(role, nil) }\n", "", "x is not defined in the package"},
		"a helper without its role":      {"func x(t *testing.T, role string) { t.Run(role, nil) }", "func x(t *testing.T) { t.Run(\"\", nil) }", "x does not take (t, role)"},
		"a call not by name":             {`x(t, "")`, `h.x(t, "")`, "TestRoutes_NMountsItsPinnedSet calls no helper by name"},
	} {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(control, c.old) {
				t.Fatalf("control has no %q", c.old)
			}
			src := strings.Replace(control, c.old, c.new, 1)
			p := pinnedSetSplitProblems(topFuncs(t, map[string]string{"c.go": src}), tests, roles)
			if !slices.Contains(p, c.want) {
				t.Fatalf("problems %q, want %q", p, c.want)
			}
		})
	}
	// The tests' map itself: a role run twice, a role left out, and a role
	// that is no role of the table.
	for name, c := range map[string]struct {
		tests map[string]string
		want  string
	}{
		"a role run twice": {map[string]string{"TestRoutes_PMountsItsPinnedSet": "provider", "TestRoutes_NMountsItsPinnedSet": "provider"}, `2 per-role tests run role "provider"`},
		"a role left out":  {map[string]string{"TestRoutes_PMountsItsPinnedSet": "provider"}, `no per-role test runs role ""`},
		"no role of it":    {map[string]string{"TestRoutes_PMountsItsPinnedSet": "provider", "TestRoutes_NMountsItsPinnedSet": "admin"}, `role "admin" is no role of the routes table`},
	} {
		t.Run(name, func(t *testing.T) {
			p := pinnedSetSplitProblems(topFuncs(t, map[string]string{"c.go": control}), c.tests, roles)
			if !slices.Contains(p, c.want) {
				t.Fatalf("problems %q, want %q", p, c.want)
			}
		})
	}
}
