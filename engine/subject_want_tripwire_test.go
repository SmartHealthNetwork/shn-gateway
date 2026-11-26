package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// Every token this gateway verifies as a leg's own states its subject and
// every binding (verifyBoundWant, through shnsdk.VerifyBoundWant): no file of
// the gateway's code, this module's whole tree read from the parsed code,
// uses the SDK's deprecated VerifyBound or VerifyBoundNoPayload, whose empty
// expected subject or binding reads as "do not check". A new verify site
// cannot pass an empty one. A use is a selector naming either function on the
// file's own import of the SDK, under whatever name the file gives it; a
// comment naming them is no use.
func TestEveryVerifyStatesItsSubject(t *testing.T) {
	const sdkPath = "github.com/SmartHealthNetwork/shn-sdk"
	deprecated := map[string]bool{"VerifyBound": true, "VerifyBoundNoPayload": true}
	root := ".." // the gateway module
	files := 0
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != root && (d.Name() == "testdata" || d.Name() == "node_modules" || strings.HasPrefix(d.Name(), ".")) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		files++
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		locals := map[string]bool{}
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, "\"`") != sdkPath {
				continue
			}
			switch {
			case imp.Name == nil:
				locals["shnsdk"] = true // the SDK's package name
			case imp.Name.Name == ".":
				t.Errorf("%s: dot-imports the SDK; import it by name, so its verifies read as the SDK's", fset.Position(imp.Pos()))
			case imp.Name.Name != "_":
				locals[imp.Name.Name] = true
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && locals[pkg.Name] && deprecated[sel.Sel.Name] {
				t.Errorf("%s: uses shnsdk.%s; state the token's subject and every binding through verifyBoundWant", fset.Position(sel.Pos()), sel.Sel.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 200 {
		t.Fatalf("walked %d files; want the gateway module's code", files)
	}
}
