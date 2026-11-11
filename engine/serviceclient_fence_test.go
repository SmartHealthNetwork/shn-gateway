package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// networkServiceClientReaders are the only functions that may read the
// Hub's, the Authorization Framework's and the consent check's clients from
// the configuration: each returns it under the network-service redirect
// policy (serviceclient). New only defaults Client when it is nil.
var networkServiceClientReaders = map[string]bool{"hubClient": true, "authzClient": true, "consentClient": true, "New": true}

// TestNetworkServiceClientsAreReadThroughThePolicy fails on a read of
// Config.Client, AuthzClient or ConsentClient (g.cfg.X, or cfg.X) outside
// networkServiceClientReaders: a new call site that took the field itself
// would send without the policy.
func TestNetworkServiceClientsAreReadThroughThePolicy(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	read := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range configClientReads(t, name, src) {
			if p.ok {
				read++
				continue
			}
			t.Errorf("%s: %s reads Config.%s outside hubClient, authzClient and consentClient: a network service's client is read under the redirect policy", name, p.fn, p.field)
		}
	}
	if read == 0 {
		t.Fatal("no read of the network services' clients found: the fence reads nothing")
	}
}

type configClientRead struct {
	fn, field string
	ok        bool
}

// configClientReads finds every selector .Client, .AuthzClient or
// .ConsentClient on g.cfg or cfg in src, with the function it is in.
func configClientReads(t *testing.T, name string, src []byte) []configClientRead {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), name, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []configClientRead
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		ast.Inspect(fd, func(n ast.Node) bool {
			s, ok := n.(*ast.SelectorExpr)
			if !ok || !slices.Contains([]string{"Client", "AuthzClient", "ConsentClient"}, s.Sel.Name) {
				return true
			}
			onCfg := false
			switch x := s.X.(type) {
			case *ast.Ident:
				onCfg = x.Name == "cfg"
			case *ast.SelectorExpr:
				onCfg = x.Sel.Name == "cfg"
			}
			if onCfg {
				out = append(out, configClientRead{fn: fd.Name.Name, field: s.Sel.Name, ok: networkServiceClientReaders[fd.Name.Name]})
			}
			return true
		})
	}
	return out
}

// The fence's own rows: a read in another function fails it; one in the
// three readers passes.
func TestNetworkServiceClientsFence_Rows(t *testing.T) {
	for _, row := range []struct {
		src  string
		fail bool
	}{
		{`package engine; func (g *Gateway) post() { _ = g.cfg.Client }`, true},
		{`package engine; func (g *Gateway) post() { _ = g.cfg.ConsentClient }`, true},
		{`package engine; func build(cfg Config) { _ = cfg.AuthzClient }`, true},
		{`package engine; func (g *Gateway) hubClient() { _ = g.cfg.Client }`, false},
		{`package engine; func (g *Gateway) post(v *V) { _ = v.Client }`, false},
	} {
		fails := slices.ContainsFunc(configClientReads(t, "x.go", []byte(row.src)), func(r configClientRead) bool { return !r.ok })
		if fails != row.fail {
			t.Errorf("%s: fails %v, want %v", row.src, fails, row.fail)
		}
	}
}
