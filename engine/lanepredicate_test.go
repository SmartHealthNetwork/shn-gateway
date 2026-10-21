package engine

import (
	"go/ast"
	"path/filepath"
	"testing"
)

// Payload conformance checks consume validator lanes at every level but none.
// gateway/app pins the same table for its own copy of the predicate.
func TestPayloadChecksConsumeLanes(t *testing.T) {
	for level, want := range map[ConformanceEnforcement]bool{
		EnforcementNone: false, EnforcementObserve: true, EnforcementStructural: true, EnforcementStrict: true,
	} {
		if got := payloadChecksConsumeLanes(level); got != want {
			t.Errorf("payloadChecksConsumeLanes(%s) = %v, want %v", level, got, want)
		}
	}
}

// Outside the level's own table (conformance.go), no engine or boot code names
// EnforcementNone except payloadChecksConsumeLanes, and each of the lane
// decisions that depend on the level asks that predicate, so the lane registry
// can be separated from conformance by changing that one predicate.
func TestLaneDecisionsAskWhetherPayloadChecksConsumeLanes(t *testing.T) {
	fset, files := engineFiles(t, ".", "../app")
	// asks are the lane decisions that depend on the level, by package and
	// function: each must call the predicate.
	asks := map[string]bool{
		"engine.servesLine": false, "engine.carryDTRAnswer": false, "engine.startCertification": false,
		"app.build": false, "app.validatorLanesForDeclared": false, "app.selectValidator": false, "app.discoverValidatorLanes": false,
	}
	found := 0
	for name, f := range files {
		if filepath.Base(filepath.Dir(name)) != "app" && filepath.Base(name) == "conformance.go" {
			continue
		}
		pkg := f.Name.Name
		for _, d := range f.Decls {
			fn, isFunc := d.(*ast.FuncDecl)
			if isFunc && fn.Body != nil {
				if _, listed := asks[pkg+"."+fn.Name.Name]; listed {
					ast.Inspect(fn.Body, func(n ast.Node) bool {
						if call, ok := n.(*ast.CallExpr); ok {
							if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "payloadChecksConsumeLanes" {
								asks[pkg+"."+fn.Name.Name] = true
							}
						}
						return true
					})
				}
			}
			ast.Inspect(d, func(n ast.Node) bool {
				var id *ast.Ident
				switch x := n.(type) {
				case *ast.SelectorExpr:
					id = x.Sel
				case *ast.Ident:
					id = x
				}
				if id == nil || id.Name != "EnforcementNone" {
					return true
				}
				if isFunc && fn.Name.Name == "payloadChecksConsumeLanes" {
					found++
					return false
				}
				t.Errorf("%s: reads EnforcementNone outside payloadChecksConsumeLanes: a lane decision asks whether payload checks consume lanes", fset.Position(id.Pos()))
				return false
			})
		}
	}
	if found != 2 {
		t.Fatalf("found payloadChecksConsumeLanes reading the level %d times, want 2 (engine and gateway/app)", found)
	}
	for fn, asked := range asks {
		if !asked {
			t.Errorf("%s decides about a lane by the level without asking payloadChecksConsumeLanes", fn)
		}
	}
}
