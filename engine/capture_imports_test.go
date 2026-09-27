package engine

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Only the engine's capture call sites import gateway/diagnostics: the hooks in
// diagnostic.go and the ingress observation in observer.go. A gate routed by
// the files that import it is then owed by a change to capture, not by every
// change to the engine's central files.
func TestDiagnosticsImportedOnlyByCaptureFiles(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			if path, _ := strconv.Unquote(imp.Path.Value); path == "github.com/SmartHealthNetwork/shn-gateway/diagnostics" {
				got = append(got, f)
			}
		}
	}
	if want := []string{"diagnostic.go", "observer.go"}; !slices.Equal(got, want) {
		t.Fatalf("files importing gateway/diagnostics: %v, want %v; put a new capture use behind a hook in diagnostic.go", got, want)
	}
}
