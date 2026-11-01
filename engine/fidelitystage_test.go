package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// fidelityStageWriters are the only functions that may name RefusalFidelity:
// the two recorder methods every fidelity refusal goes through, each taking
// its stage; refusalRuleFor, whose fidelity rule is returned with its stage;
// and emitExchange, which reads the settled rule to keep its stage.
var fidelityStageWriters = map[string]bool{"refusedFidelity": true, "refusingFidelity": true, "refusalRuleFor": true, "emitExchange": true}

// stageFieldWriters are the only functions that may assign the recorder's
// stage fields (guardStage, refusalStage).
var stageFieldWriters = map[string]bool{"settle": true, "refusing": true, "refusingFidelity": true, "checked": true}

// fidelityStageFaults: in files, every fidelity refusal names its stage, and
// nothing else carries one. RefusalFidelity (bare, or as a package's
// selector) is named only in its declaration and by fidelityStageWriters, and
// the string "fidelity" nowhere, so a refusal on the rule is recorded only
// through refusedFidelity or refusingFidelity (or refusalRuleFor's return);
// each such call passes RefusalStageBuild or RefusalStageVerify itself, and
// refusalRuleFor returns RefusalFidelity only beside one of them. The stage
// constants are named only there and in their declarations, settle is called
// only by decided and refusedFidelity, and the stage fields are assigned only
// by stageFieldWriters, so no refusal on another rule can be given a stage.
func fidelityStageFaults(fset *token.FileSet, files []*ast.File) []string {
	var faults []string
	fault := func(n ast.Node, msg string) {
		faults = append(faults, fset.Position(n.Pos()).String()+": "+msg)
	}
	// stageIdent is the stage constant e names, recording's (vocab.X) or
	// bare, or nil.
	stageIdent := func(e ast.Expr) *ast.Ident {
		id, ok := e.(*ast.Ident)
		if sel, isSel := e.(*ast.SelectorExpr); isSel {
			id, ok = sel.Sel, true
		}
		if ok && (id.Name == "RefusalStageBuild" || id.Name == "RefusalStageVerify") {
			return id
		}
		return nil
	}
	for _, file := range files {
		for _, decl := range file.Decls {
			if gd, ok := decl.(*ast.GenDecl); ok && gd.Tok == token.CONST {
				// A declaration naming the constants is not a use.
				continue
			}
			fn, _ := decl.(*ast.FuncDecl)
			name := ""
			if fn != nil {
				name = fn.Name.Name
			}
			// allowed are the uses this function's own shape accounts for.
			allowed := map[*ast.Ident]bool{}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					sel, ok := x.Fun.(*ast.SelectorExpr)
					if !ok {
						break
					}
					switch sel.Sel.Name {
					case "refusedFidelity", "refusingFidelity":
						id := (*ast.Ident)(nil)
						if len(x.Args) == 1 {
							id = stageIdent(x.Args[0])
						}
						if id == nil {
							fault(x, sel.Sel.Name+" is called without RefusalStageBuild or RefusalStageVerify")
							break
						}
						allowed[id] = true
					case "settle":
						if name != "decided" && name != "refusedFidelity" {
							fault(x, "settle is called other than by decided and refusedFidelity")
						}
					}
				case *ast.ReturnStmt:
					if name != "refusalRuleFor" || len(x.Results) != 2 {
						break
					}
					if id, ok := x.Results[0].(*ast.Ident); ok && id.Name == "RefusalFidelity" {
						id := stageIdent(x.Results[1])
						if id == nil {
							fault(x, "refusalRuleFor returns RefusalFidelity without its stage")
							break
						}
						allowed[id] = true
					}
				case *ast.AssignStmt:
					for _, lhs := range x.Lhs {
						if sel, ok := lhs.(*ast.SelectorExpr); ok && (sel.Sel.Name == "guardStage" || sel.Sel.Name == "refusalStage") && !stageFieldWriters[name] {
							fault(x, "the recorder's "+sel.Sel.Name+" is assigned outside settle, refusing, refusingFidelity and checked")
						}
					}
				case *ast.BasicLit:
					if v, err := strconv.Unquote(x.Value); x.Kind == token.STRING && err == nil && v == RefusalFidelity {
						fault(x, "the fidelity rule is named by its string, not RefusalFidelity")
					}
				}
				return true
			})
			ast.Inspect(decl, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				switch {
				case id.Name == "RefusalFidelity" && !fidelityStageWriters[name]:
					fault(id, "RefusalFidelity is named outside refusedFidelity, refusingFidelity, refusalRuleFor and emitExchange: record a fidelity refusal through refusedFidelity or refusingFidelity, with its stage")
				case (id.Name == "RefusalStageBuild" || id.Name == "RefusalStageVerify") && !allowed[id]:
					fault(id, id.Name+" is named other than as a fidelity refusal's stage")
				}
				return true
			})
		}
	}
	return faults
}

// Every site that records a fidelity refusal names its stage, and no site
// that records another rule names one: the package's non-test files, in
// every build profile, pass fidelityStageFaults. A source fence, not a drive
// of each site: a new site is caught the moment it is written, whether or not
// any test reaches it (a driven row proves only the sites it drives). The
// records every test produces are held to the same rule as they are read
// (wantClosed).
func TestFidelityRefusals_EveryOneNamesItsStage(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	if len(files) < 10 {
		t.Fatalf("parsed %d files; the package has more", len(files))
	}
	for _, f := range fidelityStageFaults(fset, files) {
		t.Error(f)
	}
	// The fence finds the sites it accounts for: each staged writer is called
	// somewhere, at both stages. The shared writers state the stage their name
	// says: refuseBuilder (a caller's declaration or missing statement) at
	// build, refuseUnsealed (the gateway's own seal failing) at verify.
	calls := map[string]int{}
	writers := map[string]string{}
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || (fn.Name.Name != "refuseBuilder" && fn.Name.Name != "refuseUnsealed") {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && len(call.Args) == 1 {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "refusedFidelity" {
						if id := stageName(call.Args[0]); id != "" {
							writers[fn.Name.Name] += id
						}
					}
				}
				return true
			})
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "refusedFidelity" || sel.Sel.Name == "refusingFidelity") && len(call.Args) == 1 {
					if id := stageName(call.Args[0]); id != "" {
						calls[sel.Sel.Name+" "+id]++
					}
				}
			}
			return true
		})
	}
	if writers["refuseBuilder"] != "RefusalStageBuild" || writers["refuseUnsealed"] != "RefusalStageVerify" {
		t.Errorf("refuseBuilder records %q and refuseUnsealed %q, want RefusalStageBuild and RefusalStageVerify, once each", writers["refuseBuilder"], writers["refuseUnsealed"])
	}
	for _, want := range []string{"refusedFidelity RefusalStageBuild", "refusedFidelity RefusalStageVerify", "refusingFidelity RefusalStageVerify"} {
		if calls[want] == 0 {
			t.Errorf("no %s call found: the fence reads no such site (calls %v)", want, calls)
		}
	}

	faults := func(src string) []string {
		t.Helper()
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "probe.go", "package engine\n\n"+src, 0)
		if err != nil {
			t.Fatal(err)
		}
		return fidelityStageFaults(fset, []*ast.File{f})
	}
	const staged = "func (g *Gateway) refuseA(ctx context.Context) { exchangeOf(ctx).refusedFidelity(RefusalStageBuild) }\n" +
		"func (g *Gateway) refuseB(ctx context.Context) { exchangeOf(ctx).refusingFidelity(RefusalStageVerify) }\n" +
		"func refusalRuleFor(kind CheckKind, rule string) (string, string) {\n\tif kind == KindFHIRBridged {\n\t\treturn RefusalFidelity, RefusalStageVerify\n\t}\n\treturn RefusalConformance, \"\"\n}\n"
	if f := faults(staged); len(f) != 0 {
		t.Fatalf("staged fidelity refusals are refused: %v", f)
	}
	for name, row := range map[string]struct{ src, want string }{
		"refused on the rule, unstaged":     {"func f(ctx context.Context) { exchangeOf(ctx).refused(RefusalFidelity) }\n", "RefusalFidelity is named outside"},
		"refusing on the rule, unstaged":    {"func f(ctx context.Context) { exchangeOf(ctx).refusing(RefusalFidelity) }\n", "RefusalFidelity is named outside"},
		"decided on the rule":               {"func f(x *exchangeRecorder) { x.decided(ExchangeRefused, \"\", RefusalFidelity) }\n", "RefusalFidelity is named outside"},
		"the rule from package recording":   {"func f(x *exchangeRecorder) { x.refused(vocab.RefusalFidelity) }\n", "RefusalFidelity is named outside"},
		"the rule by its string":            {"func f(x *exchangeRecorder) { x.refused(\"fidelity\") }\n", "named by its string"},
		"recording's stage on another rule": {"func f(x *exchangerecorder) { x.settle(ExchangeRefused, \"\", RefusalRouting, vocab.RefusalStageBuild) }\n", "settle is called other than"},
		"recording's stage held elsewhere":  {"var s = vocab.RefusalStageVerify\n", "named other than as a fidelity refusal's stage"},
		"a stage from a variable":           {"func f(x *exchangeRecorder, s string) { x.refusedFidelity(s) }\n", "called without RefusalStageBuild or RefusalStageVerify"},
		"a stage left empty":                {"func f(x *exchangeRecorder) { x.refusingFidelity(\"\") }\n", "called without RefusalStageBuild or RefusalStageVerify"},
		"a stage on another rule":           {"func f(x *exchangeRecorder) { x.settle(ExchangeRefused, \"\", RefusalRouting, RefusalStageBuild) }\n", "settle is called other than"},
		"a stage constant elsewhere":        {"func f(x *exchangeRecorder) { s := RefusalStageVerify; x.refusedFidelity(s) }\n", "named other than as a fidelity refusal's stage"},
		"a stage field written":             {"func f(x *exchangeRecorder) { x.guardRule, x.guardStage = RefusalRouting, \"build\" }\n", "guardStage is assigned outside"},
		"refusalRuleFor returns unstaged":   {strings.Replace(staged, "return RefusalFidelity, RefusalStageVerify", "return RefusalFidelity, \"\"", 1), "returns RefusalFidelity without its stage"},
		"a guard rule map names the rule":   {"var rules = map[string]string{\"x\": RefusalFidelity}\n", "RefusalFidelity is named outside"},
		"a settled rule compared and kept":  {"func keep(r string) bool { return r == RefusalFidelity }\n", "RefusalFidelity is named outside"},
	} {
		got := faults(row.src)
		if !slices.ContainsFunc(got, func(f string) bool { return strings.Contains(f, row.want) }) {
			t.Errorf("%s: faults %v, want one containing %q", name, got, row.want)
		}
	}
}

// stageName is the name e refers to, bare or as a package's selector.
func stageName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	}
	return ""
}
