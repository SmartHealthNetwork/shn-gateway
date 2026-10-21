package engine

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// TestContractSets_DeclareNoPDex: the gateway's native set, its default and
// every declared set it parses leave out pa.pdex@2.1, including a value that
// names no token (empty, or only commas and spaces), which the SDK answers
// with its own default; a set naming the line is refused.
func TestContractSets_DeclareNoPDex(t *testing.T) {
	for name, set := range map[string][]string{"native": NativeContractVersions(), "default": DefaultContractVersions()} {
		if slices.Contains(set, shnsdk.ContractPAPDex21) || len(set) == 0 {
			t.Errorf("%s set = %v, want a non-empty set without %s", name, set, shnsdk.ContractPAPDex21)
		}
	}
	for _, csv := range []string{"", " ", ",", " , ", ",,", "pa.crd@2.0,,pa.pas@2.0"} {
		got, err := ParseDeclaredContractVersions(csv)
		if err != nil {
			t.Fatalf("ParseDeclaredContractVersions(%q): %v", csv, err)
		}
		if slices.Contains(got, shnsdk.ContractPAPDex21) || len(got) == 0 {
			t.Errorf("ParseDeclaredContractVersions(%q) = %v, want a set without %s", csv, got, shnsdk.ContractPAPDex21)
		}
	}
	if got, _ := ParseDeclaredContractVersions(" , "); !slices.Equal(got, DefaultContractVersions()) {
		t.Errorf("a value naming no token parses to %v, want the default %v", got, DefaultContractVersions())
	}
	for _, csv := range []string{"pa.pdex@2.1", " pa.pdex@2.1 ", "pa.crd@2.0, pa.pdex@2.1", ",pa.pdex@2.1,"} {
		if got, err := ParseDeclaredContractVersions(csv); !errors.Is(err, ErrPDexUndeclared) {
			t.Errorf("ParseDeclaredContractVersions(%q) = %v, %v; want ErrPDexUndeclared", csv, got, err)
		}
	}
}

// TestContractSets_OnlyThroughTheGatewaysOwn: no gateway code outside this
// file's subject reads the SDK's native set, default or parser directly, so
// what the gateway builds, declares and lanes never follows the linked SDK's
// list (which named pa.pdex@2.1 through shn-sdk v0.62.0).
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

// TestContractSets_PDexRefusalText pins the refusal of a declared pa.pdex@2.1:
// it is ErrPDexUndeclared, with this text, wherever the token stands and
// whatever else the set names (a token this build cannot build included),
// because the line is checked before the SDK's parser reads the set.
func TestContractSets_PDexRefusalText(t *testing.T) {
	const want = `contract-version token "pa.pdex@2.1" is no longer declared by any gateway: a gateway serves no Patient Access API and builds no PDex resource, so a declaration must not claim it: remove it from the declared set (SHN_CONTRACT_VERSIONS)`
	if ErrPDexUndeclared.Error() != want {
		t.Fatalf("ErrPDexUndeclared = %q, want %q", ErrPDexUndeclared, want)
	}
	for _, csv := range []string{"pa.pdex@2.1", "pa.crd@2.0, pa.dtr@2.0, pa.pas@2.0, pa.pdex@2.1", "pa.pas@9.9, pa.pdex@2.1", "not-a-token,pa.pdex@2.1"} {
		if _, err := ParseDeclaredContractVersions(csv); err == nil || err.Error() != want {
			t.Errorf("ParseDeclaredContractVersions(%q) = %v, want %q", csv, err, want)
		}
	}
}
