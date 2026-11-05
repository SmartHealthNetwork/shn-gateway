package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// refusalReasonFiles are the files that declare the registered reasons.
var refusalReasonFiles = []string{"refusalreason.go", "workstream_pa_refusalreason.go", "workstream_fqs_refusalreason.go", "module1_refusalreason.go"}

// refusalReasonFaults: a refusalReason is only ever one of the registered
// constants. Outside refusalReasonFiles no file converts a value to
// refusalReason or declares a const or var of it, no call to a recorder that
// takes one passes a string literal or an expression built from strings, and
// no function returning one returns a literal. Inside them every const is
// listed in exactly one registry, once. So a value read from a request can
// reach a record's reason only by a conversion this fence refuses, and
// emitExchange records anything unregistered as other.
func refusalReasonFaults(fset *token.FileSet, files map[string]*ast.File) []string {
	var faults []string
	fault := func(n ast.Node, msg string) {
		faults = append(faults, fset.Position(n.Pos()).String()+": "+msg)
	}
	isReasonType := func(e ast.Expr) bool {
		for {
			p, ok := e.(*ast.ParenExpr)
			if !ok {
				break
			}
			e = p.X
		}
		id, ok := e.(*ast.Ident)
		return ok && id.Name == "refusalReason"
	}
	literal := func(e ast.Expr) bool {
		switch x := e.(type) {
		case *ast.BasicLit:
			return x.Kind == token.STRING && x.Value != `""`
		case *ast.BinaryExpr:
			return true
		}
		return false
	}
	declared := map[string]bool{}
	listed := map[string]int{}
	for name, f := range files {
		registry := slices.Contains(refusalReasonFiles, name)
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.GenDecl:
				for _, spec := range x.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok || vs.Type == nil || !isReasonType(vs.Type) {
						continue
					}
					// A zero-valued variable holds no sentence; a constant or an
					// initialized variable does.
					if x.Tok == token.CONST || len(vs.Values) > 0 {
						if !registry {
							fault(vs, "a refusalReason is declared outside the registry files")
						}
					}
					if x.Tok == token.CONST && registry {
						for _, id := range vs.Names {
							declared[id.Name] = true
						}
					}
				}
			case *ast.CallExpr:
				if isReasonType(x.Fun) && !registry {
					fault(x, "a value is converted to refusalReason outside the registry files")
				}
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
					switch sel.Sel.Name {
					case "refusedFor", "refusingFor", "routedFor":
						if len(x.Args) == 2 && literal(x.Args[1]) {
							fault(x, sel.Sel.Name+" is passed a string, not a registered reason")
						}
					}
				}
				if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "refuseOriginRouting" && len(x.Args) == 5 && literal(x.Args[4]) {
					fault(x, "refuseOriginRouting is passed a string, not a registered reason")
				}
			case *ast.FuncDecl:
				if x.Type.Results == nil || x.Body == nil {
					break
				}
				var at []int
				i := 0
				for _, r := range x.Type.Results.List {
					n := max(1, len(r.Names))
					if isReasonType(r.Type) {
						for k := range n {
							at = append(at, i+k)
						}
					}
					i += n
				}
				if len(at) == 0 {
					break
				}
				ast.Inspect(x.Body, func(n ast.Node) bool {
					if _, ok := n.(*ast.FuncLit); ok {
						return false
					}
					if ret, ok := n.(*ast.ReturnStmt); ok {
						for _, k := range at {
							if k < len(ret.Results) && literal(ret.Results[k]) {
								fault(ret, x.Name.Name+" returns a string as its refusalReason")
							}
						}
					}
					return true
				})
			case *ast.CompositeLit:
				if arr, ok := x.Type.(*ast.ArrayType); ok && isReasonType(arr.Elt) && registry {
					for _, e := range x.Elts {
						if id, ok := e.(*ast.Ident); ok {
							listed[id.Name]++
						} else {
							fault(e, "a registry lists something other than a registered constant")
						}
					}
				}
			}
			return true
		})
	}
	for name := range declared {
		if listed[name] != 1 {
			faults = append(faults, name+" is listed in the registries "+strconv.Itoa(listed[name])+" times, want once")
		}
	}
	for name := range listed {
		if !declared[name] {
			faults = append(faults, name+" is listed but not a refusalReason constant")
		}
	}
	return faults
}

func parseEngineFiles(t *testing.T, fset *token.FileSet) map[string]*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]*ast.File{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files[e.Name()] = f
	}
	return files
}

func TestRefusalReasons_OnlyRegisteredConstants(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	files := parseEngineFiles(t, fset)
	for _, name := range refusalReasonFiles {
		if files[name] == nil {
			t.Fatalf("no %s: the fence reads no registry", name)
		}
	}
	for _, f := range refusalReasonFaults(fset, files) {
		t.Error(f)
	}
	// The fence reads the sites it accounts for: each recorder call is made.
	calls := map[string]int{}
	for name, f := range files {
		if slices.Contains(refusalReasonFiles, name) {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					calls[sel.Sel.Name]++
				}
			}
			return true
		})
	}
	for _, want := range []string{"refusedFor", "refusingFor", "routedFor"} {
		if calls[want] == 0 {
			t.Errorf("no %s call found: the fence reads no such site", want)
		}
	}
}

// The fence refuses each way a value could be made a reason outside the
// registry.
func TestRefusalReasons_FenceRefuses(t *testing.T) {
	t.Parallel()
	registry := "package engine\ntype refusalReason string\nconst reasonA refusalReason = \"a\"\nfunc coreRefusalReasons() []refusalReason { return []refusalReason{reasonA} }\n"
	for name, row := range map[string]struct{ src, want string }{
		"a conversion":               {"package engine\nfunc f(s string) { x.refusedFor(\"r\", refusalReason(s)) }\n", "converted to refusalReason"},
		"a parenthesized conversion": {"package engine\nfunc f(s string) { x.refusedFor(\"r\", (refusalReason)(s)) }\n", "converted to refusalReason"},
		"a literal argument":         {"package engine\nfunc f() { x.refusingFor(\"r\", \"from the body\") }\n", "is passed a string"},
		"a built argument":           {"package engine\nfunc f(s string) { x.routedFor(422, \"a\"+\"b\") }\n", "is passed a string"},
		"a const elsewhere":          {"package engine\nconst reasonB refusalReason = \"b\"\n", "declared outside the registry"},
		"a literal returned":         {"package engine\nfunc f() (int, refusalReason) { return 422, \"from the body\" }\n", "returns a string as its refusalReason"},
		"an origin literal":          {"package engine\nfunc f() { refuseOriginRouting(ctx, w, 422, msg, \"x\") }\n", "refuseOriginRouting is passed a string"},
		"an unlisted constant":       {"", "listed in the registries 0 times"},
		"a constant listed two":      {"", "listed in the registries 2 times"},
	} {
		t.Run(name, func(t *testing.T) {
			fset := token.NewFileSet()
			reg := registry
			switch name {
			case "an unlisted constant":
				reg += "const reasonC refusalReason = \"c\"\n"
			case "a constant listed two":
				reg = strings.Replace(reg, "{reasonA}", "{reasonA, reasonA}", 1)
			}
			files := map[string]*ast.File{}
			f, err := parser.ParseFile(fset, "refusalreason.go", reg, 0)
			if err != nil {
				t.Fatal(err)
			}
			files["refusalreason.go"] = f
			if row.src != "" {
				g, err := parser.ParseFile(fset, "site.go", row.src, 0)
				if err != nil {
					t.Fatal(err)
				}
				files["site.go"] = g
			}
			faults := refusalReasonFaults(fset, files)
			if !slices.ContainsFunc(faults, func(f string) bool { return strings.Contains(f, row.want) }) {
				t.Fatalf("faults %v, want one saying %q", faults, row.want)
			}
		})
	}
}

// closedReason keeps a registered sentence and records anything else as other.
func TestClosedReason(t *testing.T) {
	t.Parallel()
	if got := closedReason(reasonIngressAuthentication); got != string(reasonIngressAuthentication) {
		t.Fatalf("closedReason(registered) = %q", got)
	}
	for _, v := range []string{"Member123", "no registered payer for identifier urn:x|77441", string(reasonIngressAuthentication) + " "} {
		if got := closedReason(refusalReason(v)); got != ExchangeOther {
			t.Fatalf("closedReason(%q) = %q, want %q", v, got, ExchangeOther)
		}
	}
}
