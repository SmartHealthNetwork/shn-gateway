package engine

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"slices"
	"testing"
)

// conformancePolicyReaders are the only files that read a ConformancePolicy
// other than through conformanceModule: the policy's own file, which is module
// 3's table, and the native responder's options, which hold the policy the
// app gives it.
var conformancePolicyReaders = []string{"conformance.go", "native_conformance.go"}

// conformanceModuleBypasses names each use of the level that goes around
// module 3 outside conformancePolicyReaders: a method of ConformancePolicy
// called on the concrete policy, the native responder's policy field read, a
// policy built (by either constructor), or the ConformancePolicy type named
// at all (a literal, a variable, an embedding), the responder's field
// declaration aside. It decides by resolved object, never by name.
func conformanceModuleBypasses(t *testing.T, fset *token.FileSet, files []*ast.File, info *types.Info) []string {
	t.Helper()
	var policy *types.TypeName
	var field *types.Var
	var fieldType ast.Expr
	builds := map[types.Object]bool{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.TypeSpec:
				switch n.Name.Name {
				case "ConformancePolicy":
					policy, _ = info.Defs[n.Name].(*types.TypeName)
				case "nativeResponder":
					st, _ := n.Type.(*ast.StructType)
					for _, fl := range st.Fields.List {
						for _, name := range fl.Names {
							if name.Name == "conformance" {
								field, _ = info.Defs[name].(*types.Var)
								fieldType = fl.Type
							}
						}
					}
				}
			case *ast.FuncDecl:
				if n.Recv == nil && (n.Name.Name == "NewConformancePolicy" || n.Name.Name == "newConformancePolicy") {
					builds[info.Defs[n.Name]] = true
				}
			}
			return true
		})
	}
	if policy == nil || field == nil || len(builds) != 2 {
		t.Fatalf("ConformancePolicy %v, nativeResponder.conformance %v, constructors %d: one is not declared", policy, field, len(builds))
	}
	isPolicy := func(typ types.Type) bool {
		if p, ok := typ.(*types.Pointer); ok {
			typ = p.Elem()
		}
		n, ok := typ.(*types.Named)
		return ok && n.Obj() == policy
	}
	var got []string
	for _, f := range files {
		file := filepath.Base(fset.Position(f.Pos()).Filename)
		if slices.Contains(conformancePolicyReaders, file) {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				sel := info.Selections[n]
				switch {
				case sel == nil:
				case sel.Kind() != types.FieldVal && isPolicy(sel.Recv()):
					got = append(got, fmt.Sprintf("%s calls ConformancePolicy.%s on the policy itself", file, n.Sel.Name))
				case sel.Kind() == types.FieldVal && sel.Obj() == field:
					got = append(got, file+" reads nativeResponder.conformance")
				}
			case *ast.Ident:
				switch obj := info.Uses[n]; {
				case builds[obj]:
					got = append(got, file+" builds a ConformancePolicy")
				case obj == policy && ast.Expr(n) != fieldType:
					got = append(got, file+" names ConformancePolicy")
				}
			}
			return true
		})
	}
	return got
}

// Every payload conformance check reaches the level through module 3
// (conformanceModule): the gateway's conformance, the native responder's
// module. None reads a ConformancePolicy around it.
func TestPayloadChecksAskModuleThree(t *testing.T) {
	fset := token.NewFileSet()
	files := parsePackageSources(t, fset, ".", nil)
	if got := conformanceModuleBypasses(t, fset, files, typedPackage(t, fset, files)); len(got) != 0 {
		t.Fatalf("the level is read around module 3:\n%q", got)
	}
}

// TestPayloadChecksAskModuleThreeCheckFires adds a bypass to the real package
// and requires the check to name it; a call through the module is clean.
func TestPayloadChecksAskModuleThreeCheckFires(t *testing.T) {
	for _, row := range []struct{ name, src, want string }{
		{"a policy method on the policy", "func probeRuns(p ConformancePolicy) bool { return p.Runs(KindContent, RulePatientMixed) }", "probe_extra.go calls ConformancePolicy.Runs on the policy itself"},
		{"a policy method value", "func probeDecide(p ConformancePolicy) any { return p.Decide }", "probe_extra.go calls ConformancePolicy.Decide on the policy itself"},
		{"the responder's policy read", "func probeLevel(n *nativeResponder) ConformanceEnforcement { return n.conformance.Level() }", "probe_extra.go reads nativeResponder.conformance"},
		{"a policy built", "func probeBuild() conformanceModule { return NewConformancePolicy(EnforcementStrict) }", "probe_extra.go builds a ConformancePolicy"},
		{"a policy built unexported", "func probeBuildTable() conformanceModule { return newConformancePolicy(EnforcementStrict, false) }", "probe_extra.go builds a ConformancePolicy"},
		{"a policy literal", "func probeLiteral() conformanceModule { return ConformancePolicy{} }", "probe_extra.go names ConformancePolicy"},
		{"a policy variable", "func probeVar() bool { var p ConformancePolicy; return guardDefect(nil, p, nil, KindContent, RulePatientMixed, VerdictInvalid, nil) }", "probe_extra.go names ConformancePolicy"},
		{"a policy embedded", "type probeEmbed struct{ ConformancePolicy }\n\nfunc probeEmbedded(e probeEmbed) bool { return e.Runs(KindContent, RulePatientMixed) }", "probe_extra.go names ConformancePolicy"},
		{"through the module", "func probeModule(m conformanceModule) bool { return m.Runs(KindContent, RulePatientMixed) }", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			fset := token.NewFileSet()
			files := parsePackageSources(t, fset, ".", map[string]string{"probe_extra.go": "package engine\n\n" + row.src + "\n"})
			got := conformanceModuleBypasses(t, fset, files, typedPackage(t, fset, files))
			if row.want == "" {
				if len(got) != 0 {
					t.Fatalf("a call through the module was named: %q", got)
				}
				return
			}
			if !slices.Contains(got, row.want) {
				t.Fatalf("want %q, got %q", row.want, got)
			}
		})
	}
}

// The gateway's module 3 is the configured level, and a gateway built with no
// level is strict: the zero Config never means no module.
func TestGatewayConformanceIsTheConfiguredLevel(t *testing.T) {
	for _, level := range allLevels {
		if got := (&Gateway{cfg: Config{ConformanceEnforcement: level}}).conformance().Level(); got != level {
			t.Errorf("configured %s, module at %s", level, got)
		}
	}
	if got := (&Gateway{}).conformance().Level(); got != EnforcementStrict {
		t.Errorf("zero Config: module at %s, want strict", got)
	}
	if got := NewNativeResponder(nil, "", "", nil, nil).module().Level(); got != EnforcementStrict {
		t.Errorf("responder with no policy: module at %s, want strict", got)
	}
}
