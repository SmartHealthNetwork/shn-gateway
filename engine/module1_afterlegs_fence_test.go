//go:build !shn_coreonly

package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Every error answer module 1's exchange routes write themselves goes through
// afterLegs (or withCarriedLegs, at a leg's build), so one written after a leg
// says which legs were carried and one before any leg is left as it is. The
// one exception is relayOriginationError, which relays a leg's or the
// network's own answer as it came. Decided by structure: the error value of
// each answer a module 1 file (and lineselect.go and fhirref.go, two core
// files whose writers module 1 answers through) writes with writeJSON, and
// the message of each refuseBuilder, read from the source. Not read: an
// answer built in a variable and passed in, or keyed by a constant (none is
// written that way today), and the core's originCoverage, which module 1
// calls only before any leg.
func TestModuleOneOwnAnswersSayWhichLegsWereCarried(t *testing.T) {
	files, err := filepath.Glob("module1_*.go")
	if err != nil {
		t.Fatal(err)
	}
	// Two core files whose writers module 1 answers through.
	files = append(files, "lineselect.go", "fhirref.go")
	fset := token.NewFileSet()
	var answers int
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Name.Name == "relayOriginationError" {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				id, ok := call.Fun.(*ast.Ident)
				if !ok {
					return true
				}
				// refuseBuilder writes its msg as the answer's error.
				if id.Name == "refuseBuilder" && len(call.Args) == 2 {
					answers++
					if !saysCarriedLegs(call.Args[1]) {
						t.Errorf("%s (%s): a build refusal written without afterLegs or withCarriedLegs", fset.Position(call.Pos()), fn.Name.Name)
					}
					return true
				}
				if id.Name != "writeJSON" || len(call.Args) != 3 {
					return true
				}
				lit, ok := call.Args[2].(*ast.CompositeLit)
				if !ok {
					return true
				}
				for _, elt := range lit.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if key, ok := kv.Key.(*ast.BasicLit); !ok || key.Value != strconv.Quote("error") {
						continue
					}
					answers++
					if !saysCarriedLegs(kv.Value) {
						t.Errorf("%s (%s): an error answer written without afterLegs: a failure after a leg would not say which legs were carried", fset.Position(kv.Pos()), fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	// The scan reads the answers it is about: a rename or a move that hid
	// them would pass with nothing checked.
	if answers < 150 {
		t.Fatalf("read %d error answers in module 1's files, want at least 150", answers)
	}
}

func saysCarriedLegs(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := call.Fun.(*ast.Ident)
	return ok && (id.Name == "afterLegs" || id.Name == "withCarriedLegs")
}
