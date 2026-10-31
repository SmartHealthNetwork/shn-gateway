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

// TestContractSets_OnlyThroughTheGatewaysOwn: no gateway code outside this
// file's subject reads the SDK's native set, default or parser directly, so
// every read of what the gateway builds, declares and lanes goes through the
// gateway's own sets: the union of its compiled-in workstreams', and its own
// refusal of the retired PDex token.
func TestContractSets_OnlyThroughTheGatewaysOwn(t *testing.T) {
	sdkSets := map[string]bool{"NativeContractVersions": true, "SupportedContractVersions": true, "ParseDeclaredContractVersions": true}
	var found []string
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || filepath.Clean(path) == filepath.Clean("../engine/contractset.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		alias := ""
		for _, imp := range f.Imports {
			if imp.Path.Value == `"github.com/SmartHealthNetwork/shn-sdk"` {
				alias = "shnsdk"
				if imp.Name != nil {
					alias = imp.Name.Name
				}
			}
		}
		if alias == "" {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == alias && sdkSets[sel.Sel.Name] {
					found = append(found, path+": "+alias+"."+sel.Sel.Name)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) > 0 {
		t.Errorf("gateway code reads the SDK's contract sets directly; read engine.NativeContractVersions, DefaultContractVersions or ParseDeclaredContractVersions:\n%s", strings.Join(found, "\n"))
	}
}
