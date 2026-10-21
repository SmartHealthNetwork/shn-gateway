package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// nativeArm is arm (2) of line selection, building natively at a peer-declared
// line this build speaks and has a lane for, and the config field only it
// reads. It is the core's (linereach.go): module 1 choosing its build line,
// which translates nothing (ruled 2026-10-19).
var nativeArm = map[string]bool{
	"selectNativeReachRoute": true, "nativeLinesView": true,
	"EgressNativeLines": true,
}

// chainArm is arm (3), a transform chain to the peer's line, and the helpers
// and config field only it uses: module 2's (linebridge.go).
var chainArm = map[string]bool{
	"strictPeer": true, "selectChainRoute": true, "anyChainExists": true,
	"classRank": true, "chainWorstClass": true, "bestChain": true,
	"chainBetter": true, "SelectChainRouteForTest": true,
	"StrictPeerForTest": true,
}

// reachEntries are the two ways the core's selection asks for a reach across
// lines.
var reachEntries = map[string]bool{"reachPeerLine": true, "resumeReach": true}

// moduleTwoFiles hold module 2's code: the bridge and its arm (3), its request
// walk (transform.go) and E-08 (dtr_answer_bridge.go).
var moduleTwoFiles = map[string]bool{"linebridge.go": true, "transform.go": true, "dtr_answer_bridge.go": true}

// bridgeCalls are where the core asks module 2, by method: each of its four
// calls from one place, through the gateway's bridge.
var bridgeCalls = map[string]map[string]bool{
	"Reach":        {"linereach.go": true},
	"AdaptRequest": {"gateway.go": true},
	"AdaptAnswer":  {"gateway.go": true},
	"Certify":      {"modulebuild.go": true, "dtr_answer_bridge.go": true},
}

// chainRunners are the files that may run a transform chain (applyChain):
// module 2's own, the cross-module test wrappers and the demonstration
// endpoint's chain (RunTransformChain, never a leg).
var chainRunners = map[string]bool{
	"transform.go": true, "dtr_answer_bridge.go": true,
	"transform_pas.go": true, "transform_dtr.go": true, "transform_run.go": true,
}

// Line selection is split at arm (2). In the package's non-test files:
//   - arm (2) and the config field only it reads (EgressNativeLines) are named
//     in linereach.go alone, apart from the field's declaration in Config;
//   - arm (3), its helpers and the config field only they read
//     (StrictPeerForTest) are named in linebridge.go alone, apart from the
//     field's declaration;
//   - the core reaches across lines only through reachPeerLine and
//     resumeReach, which only the core's selection (lineselect.go) calls, and
//     reaches module 2 only through the gateway's bridge (bridge()), each of
//     whose calls is asked from one place (bridgeCalls); chainBridge, module 2
//     as this build compiles it, is named only in module 2's files;
//   - a transform chain runs only in module 2's files (chainRunners);
//   - lineselect.go declares arm (1), the carry or refusal after it, the
//     resume pin's own arm and relayOriginationError.
func TestLineSelection_ArmsOneAndTwoAreCoreTheChainIsModuleTwo(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]string{}
	asked := map[string][]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		configField := map[token.Pos]bool{}
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil || ok && recvIsGateway(fn) {
				declared[fn.Name.Name] = name
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if ts, ok := n.(*ast.TypeSpec); ok && ts.Name.Name == "Config" {
				if st, ok := ts.Type.(*ast.StructType); ok {
					for _, fld := range st.Fields.List {
						for _, id := range fld.Names {
							configField[id.Pos()] = true
						}
					}
				}
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				sel, ok := n.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if _, ok := bridgeCalls[sel.Sel.Name]; ok {
					asked[sel.Sel.Name] = append(asked[sel.Sel.Name], name)
				}
				if sel.Sel.Name == "bridge" && name != "linereach.go" && name != "gateway.go" && name != "modulebuild.go" {
					t.Errorf("%s: module 2 is asked for outside the core's reach, walk, answer and build", fset.Position(n.Pos()))
				}
			case *ast.Ident:
				id := n
				switch {
				case (nativeArm[id.Name] || chainArm[id.Name]) && name == "gateway.go" && configField[id.Pos()]:
					// The config fields' own declarations.
				case nativeArm[id.Name] && name != "linereach.go":
					t.Errorf("%s: %s, arm (2), is named outside linereach.go", fset.Position(id.Pos()), id.Name)
				case chainArm[id.Name] && name != "linebridge.go":
					t.Errorf("%s: %s, module 2's arm (3), is named outside linebridge.go", fset.Position(id.Pos()), id.Name)
				case id.Name == "chainBridge" && !moduleTwoFiles[name]:
					t.Errorf("%s: chainBridge, module 2, is named outside module 2's files", fset.Position(id.Pos()))
				case id.Name == "applyChain" && !chainRunners[name]:
					t.Errorf("%s: a transform chain runs outside module 2", fset.Position(id.Pos()))
				case reachEntries[id.Name] && name != "linereach.go" && name != "lineselect.go":
					t.Errorf("%s: %s is asked outside the core's line selection", fset.Position(id.Pos()), id.Name)
				}
			}
			return true
		})
	}
	for fn := range nativeArm {
		if fn != "EgressNativeLines" && declared[fn] != "linereach.go" {
			t.Errorf("%s is declared in %q, want linereach.go", fn, declared[fn])
		}
	}
	for fn := range chainArm {
		if fn != "StrictPeerForTest" && declared[fn] != "linebridge.go" {
			t.Errorf("%s is declared in %q, want linebridge.go", fn, declared[fn])
		}
	}
	for fn := range reachEntries {
		if declared[fn] != "linereach.go" {
			t.Errorf("%s is declared in %q, want linereach.go", fn, declared[fn])
		}
	}
	if declared["bridge"] != "linebridge.go" {
		t.Errorf("bridge is declared in %q, want linebridge.go", declared["bridge"])
	}
	for call, want := range bridgeCalls {
		got := map[string]bool{}
		for _, f := range asked[call] {
			got[f] = true
		}
		var gotFiles, wantFiles []string
		for f := range got {
			gotFiles = append(gotFiles, f)
		}
		for f := range want {
			wantFiles = append(wantFiles, f)
		}
		sort.Strings(gotFiles)
		sort.Strings(wantFiles)
		if strings.Join(gotFiles, ",") != strings.Join(wantFiles, ",") {
			t.Errorf("module 2's %s is asked from %v, want %v", call, gotFiles, wantFiles)
		}
	}
	for _, fn := range []string{"selectLegLine", "selectLegLineOrFail", "selectLegRoute", "selectResumeRoute", "relayOriginationError"} {
		if declared[fn] != "lineselect.go" {
			t.Errorf("%s is declared in %q, want lineselect.go", fn, declared[fn])
		}
	}
}

// recvIsGateway reports whether fn is a method of *Gateway.
func recvIsGateway(fn *ast.FuncDecl) bool {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return false
	}
	star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	id, ok := star.X.(*ast.Ident)
	return ok && id.Name == "Gateway"
}
