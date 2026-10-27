package engine

import (
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"strings"
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

// Outside the level's own table (conformance.go) and the default's one home
// (gateway/app's loadConfig, which maps an absent CONFORMANCE_ENFORCEMENT to
// none, a level and not a lane decision), no engine or boot code names
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
	msgs, found, defaults := scanLaneLevelReads(fset, files, asks)
	for _, m := range msgs {
		t.Error(m)
	}
	if defaults != 1 {
		t.Fatalf("found loadConfig naming EnforcementNone %d times, want 1 (the unset default)", defaults)
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

// scanLaneLevelReads reports each read of EnforcementNone outside
// payloadChecksConsumeLanes and app.loadConfig, counts the reads in each of
// those two, and marks in asks each listed function that calls the predicate.
func scanLaneLevelReads(fset *token.FileSet, files map[string]*ast.File, asks map[string]bool) (msgs []string, found, defaults int) {
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
				if isFunc && pkg == "app" && fn.Name.Name == "loadConfig" {
					defaults++
					return false
				}
				msgs = append(msgs, fmt.Sprintf("%s: reads EnforcementNone outside payloadChecksConsumeLanes: a lane decision asks whether payload checks consume lanes", fset.Position(id.Pos())))
				return false
			})
		}
	}
	return msgs, found, defaults
}

// Every guard ships its rejection rows: a read of EnforcementNone in any other
// app function is refused, loadConfig's reads are counted (so a second one
// fails the count of one), and a listed lane decision that never calls the
// predicate stays unmarked.
func TestLaneDecisionsAskWhetherPayloadChecksConsumeLanesRejects(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := `package app

func loadConfig() { _ = EnforcementNone; _ = EnforcementNone }

func build() { _ = EnforcementNone }
`
	if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	fset, files := engineFiles(t, dir)
	asks := map[string]bool{"app.build": false}
	msgs, found, defaults := scanLaneLevelReads(fset, files, asks)
	if len(msgs) != 1 || !strings.Contains(msgs[0], "reads EnforcementNone outside payloadChecksConsumeLanes") {
		t.Errorf("build's read of EnforcementNone: msgs = %v, want it refused", msgs)
	}
	if defaults != 2 || found != 0 {
		t.Errorf("defaults = %d, found = %d: want loadConfig's two reads counted (the real test wants exactly one)", defaults, found)
	}
	if asks["app.build"] {
		t.Error("build never calls payloadChecksConsumeLanes, but is marked as asking it")
	}
}
