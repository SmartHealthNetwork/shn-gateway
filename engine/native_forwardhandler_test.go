package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// paForwardedLegs are the PA legs the native responder sends to the payer's
// own system. Eligibility, the federated query and patient-authored DTR are
// answered by engine handlers and never reach it.
var paForwardedLegs = []string{
	"crd-order-dispatch", "crd-order-select", "dtr-questionnaire-fetch",
	"pas-claim", "pas-claim-inquire", "pas-claim-update",
}

// TestNativeResponder_HandleForwardsThroughItsWorkstream: the native
// responder's Handle keeps the core's version rule and sends every leg through
// the workstream that declares it (forwardFor), with no switch or comparison
// on the leg's name.
func TestNativeResponder_HandleForwardsThroughItsWorkstream(t *testing.T) {
	src, err := os.ReadFile("native.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range handleForwardFaults(src) {
		t.Error(f)
	}
	for _, leg := range paForwardedLegs {
		if forwardFor(leg) == nil {
			t.Errorf("forwardFor(%q) is nil: the native responder cannot send it", leg)
		}
	}
	for _, leg := range []string{"coverage-eligibility", "federated-query", "patient-dtr", "no-such-leg"} {
		if forwardFor(leg) != nil {
			t.Errorf("forwardFor(%q) is a forward: the native responder never sends it", leg)
		}
	}

	// Rejection rows: each way a leg switch could come back into Handle.
	const anchor = "\tin := relay.NewBody(requestFHIR, relay.OriginPeerFrame)\n"
	if !strings.Contains(string(src), anchor) {
		t.Fatalf("native.go's Handle no longer builds its request as %q: re-anchor the rows", anchor)
	}
	put := func(stmt string) []byte {
		return []byte(strings.Replace(string(src), anchor, stmt+anchor, 1))
	}
	for name, c := range map[string]struct {
		src  []byte
		want string
	}{
		"a switch put back":       {put("\tswitch leg {\n\tcase \"pas-claim\":\n\t}\n"), "switches on the leg"},
		"an if chain":             {put("\tif leg == \"pas-claim\" {\n\t\treturn LegResult{}, nil\n\t}\n"), "compares the leg"},
		"a tagless switch":        {put("\tswitch {\n\tcase \"crd-order-select\" == leg:\n\t}\n"), "compares the leg"},
		"not through forwardFor":  {[]byte(strings.Replace(string(src), "forwardFor(leg)", "forwardFor(\"pas-claim\")", 1)), "does not send through forwardFor(leg)"},
		"no Handle":               {[]byte(strings.Replace(string(src), "func (n *nativeResponder) Handle(", "func (n *nativeResponder) handleGone(", 1)), "no nativeResponder.Handle"},
		"a switch on another tag": {put("\tswitch contract {\n\tcase \"pa.pas\":\n\t}\n"), ""},
	} {
		got := strings.Join(handleForwardFaults(c.src), "\n")
		if c.want == "" {
			if got != "" {
				t.Errorf("%s: want no fault, got %q", name, got)
			}
			continue
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: want a fault containing %q, got %q", name, c.want, got)
		}
	}
}

func handleForwardFaults(src []byte) []string {
	f, err := parser.ParseFile(token.NewFileSet(), "native.go", src, parser.SkipObjectResolution)
	if err != nil {
		return []string{"native.go does not parse: " + err.Error()}
	}
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if x, ok := d.(*ast.FuncDecl); ok && x.Name.Name == "Handle" && x.Recv != nil {
			fn = x
		}
	}
	if fn == nil {
		return []string{"no nativeResponder.Handle in native.go"}
	}
	isLeg := func(e ast.Expr) bool {
		id, ok := e.(*ast.Ident)
		return ok && id.Name == "leg"
	}
	var out []string
	routed := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SwitchStmt:
			if isLeg(x.Tag) {
				out = append(out, "Handle switches on the leg: send a leg through its workstream (forwardFor)")
			}
		case *ast.BinaryExpr:
			if (x.Op == token.EQL || x.Op == token.NEQ) && (isLeg(x.X) && isStringLit(x.Y) || isLeg(x.Y) && isStringLit(x.X)) {
				out = append(out, "Handle compares the leg with a leg's name: send a leg through its workstream (forwardFor)")
			}
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "forwardFor" && len(x.Args) == 1 && isLeg(x.Args[0]) {
				routed = true
			}
		}
		return true
	})
	if !routed {
		out = append(out, "Handle does not send through forwardFor(leg)")
	}
	return out
}

// TestPA_ForwardCasesAreTheForwardedLegs: the PA adapter's forward names
// exactly the legs the native responder sends, each a leg of the PA catalog.
func TestPA_ForwardCasesAreTheForwardedLegs(t *testing.T) {
	src, err := os.ReadFile("workstream_pa_forward.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, leg := range paForwardedLegs {
		if _, ok := paCatalog[leg]; !ok {
			t.Errorf("paForwardedLegs names %q, which is no PA leg", leg)
		}
	}
	for _, f := range paForwardCaseFaults(src, paForwardedLegs) {
		t.Error(f)
	}
	// Rejection rows: a case for no forwarded leg, and a forwarded leg with no case.
	if got := strings.Join(paForwardCaseFaults([]byte(strings.Replace(string(src), `case "pas-claim":`, `case "pas-claim", "probe-gone":`, 1)), paForwardedLegs), "\n"); !strings.Contains(got, `case "probe-gone" is no forwarded leg`) {
		t.Errorf("a case for no leg: %q", got)
	}
	if got := strings.Join(paForwardCaseFaults(src, append(slices.Clone(paForwardedLegs), "probe-new")), "\n"); !strings.Contains(got, `leg "probe-new" has no case`) {
		t.Errorf("a leg with no case: %q", got)
	}
}

func paForwardCaseFaults(src []byte, legs []string) []string {
	f, err := parser.ParseFile(token.NewFileSet(), "workstream_pa_forward.go", src, parser.SkipObjectResolution)
	if err != nil {
		return []string{"workstream_pa_forward.go does not parse: " + err.Error()}
	}
	cases, found := map[string]bool{}, false
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "forward" || fn.Recv == nil {
			continue
		}
		found = true
		// Only the outer switch's cases name legs; a forward's own switch
		// (the DTR operation) is not walked into.
		for _, st := range fn.Body.List {
			sw, ok := st.(*ast.SwitchStmt)
			if !ok {
				continue
			}
			for _, s := range sw.Body.List {
				for _, e := range s.(*ast.CaseClause).List {
					if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if v, err := strconv.Unquote(lit.Value); err == nil {
							cases[v] = true
						}
					}
				}
			}
		}
	}
	if !found {
		return []string{"no paWorkstream.forward in workstream_pa_forward.go"}
	}
	var out []string
	want := map[string]bool{}
	for _, l := range legs {
		want[l] = true
		if !cases[l] {
			out = append(out, "leg "+strconv.Quote(l)+" has no case in paWorkstream.forward")
		}
	}
	for _, c := range slices.Sorted(maps.Keys(cases)) {
		if !want[c] {
			out = append(out, "paWorkstream.forward's case "+strconv.Quote(c)+" is no forwarded leg")
		}
	}
	return out
}
