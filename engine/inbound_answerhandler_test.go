package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A leg the compiled-in workstreams declare, which passes every check
// handleInbound makes and has no answer handler, is answered with this
// gateway's own failure, at every level: 500, recorded as no refusal on any
// rule. It was answered 200 with nothing written, and recorded answered.
//
// The test adds its leg after the ownership table was registered
// (relay.RegisterOwnership, at init), so the refusal naming the leg has no
// row and its transmit is refused in turn: the answer is a bare 500. An
// adapter compiled in registers its legs' refusal rows, and its answer names
// the leg (errNoAnswerHandler).
func TestInbound_LegWithoutAnswerHandlerIsOwnFailure(t *testing.T) {
	saved := workstreams
	t.Cleanup(func() { workstreams = saved })
	// Its names are its own, so a build that compiles in the toy test profile
	// (whose adapter answers toy-echo) runs it unchanged.
	workstreams = append(slices.Clone(saved), toyWorkstream{tag: "unanswered", legs: map[string]legSpec{"unanswered-echo": {
		ReqFrame: "provider-tpo", RespFrame: "unanswered-answer", Op: "unanswered-ask", RespOp: "unanswered-answer", Scope: "unanswered-only", Physics: paReadOnly,
	}}})
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p, got := newRecordingLevelPayer(t, level)
			w := httptest.NewRecorder()
			p.g.inboundRoute()(w, inboundRequestWith(t, p, "unanswered-echo", []byte(`{"echo":"x"}`), nil, nil))
			rec := got.only(t)
			if w.Code != http.StatusInternalServerError || rec.Outcome != ExchangeOther || rec.RefusedBy != "" || rec.Rule != "" || rec.Status != http.StatusInternalServerError {
				t.Fatalf("answer %d; record = %s %s/%s status %d, want 500 and this gateway's own failure (%s, no party or rule)", w.Code, rec.Outcome, rec.RefusedBy, rec.Rule, rec.Status, ExchangeOther)
			}
		})
	}
}

// TestInbound_EveryLegHasAnAnswerHandler: every leg the compiled-in
// workstreams declare is answered by its workstream's adapter (answer), and
// handleInbound dispatches through it alone: no switch on the transaction type
// remains in the core's dispatch, so the fail-closed default above is never a
// leg's answer and a second workstream's legs need no core edit.
func TestInbound_EveryLegHasAnAnswerHandler(t *testing.T) {
	legs := 0
	for _, w := range workstreams {
		legs += len(w.catalog())
	}
	if legs < 9 {
		t.Fatalf("%d legs compiled in — the fence would be vacuous", legs)
	}
	for _, f := range answerFaults(workstreams) {
		t.Error(f)
	}
	src, err := os.ReadFile("inbound.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range dispatchSwitchFaults(src) {
		t.Error(f)
	}

	// Rejection rows: a workstream leg with no handler, a transaction-type
	// switch back in handleInbound, and a dispatch the fence cannot find, are
	// each reported.
	probe := toyWorkstream{tag: "probe", legs: map[string]legSpec{"probe-echo": {Op: "probe-ask", RespOp: "probe-answer"}}}
	if got := strings.Join(answerFaults([]workstream{probe}), "\n"); !strings.Contains(got, `leg "probe-echo" of workstream "probe" has no answer handler`) {
		t.Errorf("a leg with no handler: %q", got)
	}
	switched := strings.Replace(string(src), "\tif answer := answerFor(env.Metadata.TransactionType); answer != nil {",
		"\tswitch env.Metadata.TransactionType {\n\tcase \"probe-echo\":\n\t}\n\tif answer := answerFor(env.Metadata.TransactionType); answer != nil {", 1)
	if got := strings.Join(dispatchSwitchFaults([]byte(switched)), "\n"); !strings.Contains(got, "switches on the transaction type") {
		t.Errorf("a switch back in the dispatch: %q", got)
	}
	ifChained := strings.Replace(string(src), "\tif answer := answerFor(env.Metadata.TransactionType); answer != nil {",
		"\tif env.Metadata.TransactionType == \"probe-echo\" {\n\t\treturn\n\t}\n\tif answer := answerFor(env.Metadata.TransactionType); answer != nil {", 1)
	if got := strings.Join(dispatchSwitchFaults([]byte(ifChained)), "\n"); !strings.Contains(got, "compares the transaction type") {
		t.Errorf("an if chain on the transaction type: %q", got)
	}
	tagless := strings.Replace(string(src), "\tif answer := answerFor(env.Metadata.TransactionType); answer != nil {",
		"\tswitch {\n\tcase \"probe-echo\" == env.Metadata.TransactionType:\n\t}\n\tif answer := answerFor(env.Metadata.TransactionType); answer != nil {", 1)
	if got := strings.Join(dispatchSwitchFaults([]byte(tagless)), "\n"); !strings.Contains(got, "compares the transaction type") {
		t.Errorf("a tagless switch on the transaction type: %q", got)
	}
	gone := strings.Replace(string(src), "func (g *Gateway) handleInbound(", "func (g *Gateway) handleInboundGone(", 1)
	if got := strings.Join(dispatchSwitchFaults([]byte(gone)), "\n"); !strings.Contains(got, "no handleInbound") {
		t.Errorf("no dispatch: %q", got)
	}
	unrouted := strings.Replace(string(src), "answerFor(env.Metadata.TransactionType)", "answerFor(env.Metadata.Sender)", 1)
	if got := strings.Join(dispatchSwitchFaults([]byte(unrouted)), "\n"); !strings.Contains(got, "does not dispatch through answerFor") {
		t.Errorf("a dispatch not through answerFor: %q", got)
	}
}

// answerFaults reports every leg of ws's catalogs whose workstream declares
// no answer handler for it.
func answerFaults(ws []workstream) []string {
	var out []string
	for _, w := range ws {
		for _, leg := range slices.Sorted(maps.Keys(w.catalog())) {
			if w.answer(leg) == nil {
				out = append(out, "leg "+strconv.Quote(leg)+" of workstream "+strconv.Quote(w.id())+" has no answer handler")
			}
		}
	}
	return out
}

// dispatchSwitchFaults reports a handleInbound that switches on the
// transaction type, or that does not dispatch through answerFor on it.
func dispatchSwitchFaults(src []byte) []string {
	f, err := parser.ParseFile(token.NewFileSet(), "inbound.go", src, parser.SkipObjectResolution)
	if err != nil {
		return []string{"inbound.go does not parse: " + err.Error()}
	}
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if x, ok := d.(*ast.FuncDecl); ok && x.Name.Name == "handleInbound" && x.Recv != nil {
			fn = x
		}
	}
	if fn == nil {
		return []string{"no handleInbound in inbound.go"}
	}
	var out []string
	routed := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SwitchStmt:
			if isEnvTransactionType(x.Tag) {
				out = append(out, "handleInbound switches on the transaction type: answer a leg through its workstream (answerFor)")
			}
		case *ast.BinaryExpr:
			// A comparison of the transaction type with a leg's name, in an if
			// chain or a tagless switch's case, is a switch by another name.
			if (x.Op == token.EQL || x.Op == token.NEQ) && (isEnvTransactionType(x.X) && isStringLit(x.Y) || isEnvTransactionType(x.Y) && isStringLit(x.X)) {
				out = append(out, "handleInbound compares the transaction type with a leg's name: answer a leg through its workstream (answerFor)")
			}
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "answerFor" && len(x.Args) == 1 && isEnvTransactionType(x.Args[0]) {
				routed = true
			}
		}
		return true
	})
	if !routed {
		out = append(out, "handleInbound does not dispatch through answerFor(env.Metadata.TransactionType)")
	}
	return out
}

func isStringLit(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	return ok && lit.Kind == token.STRING
}

func isEnvTransactionType(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "TransactionType" {
		return false
	}
	inner, ok := sel.X.(*ast.SelectorExpr)
	if !ok || inner.Sel.Name != "Metadata" {
		return false
	}
	id, ok := inner.X.(*ast.Ident)
	return ok && id.Name == "env"
}

// TestPA_AnswerCasesAreTheCatalogLegs: the PA adapter's answer names exactly
// its catalog's legs, so a case left behind when a leg leaves the catalog
// fails here rather than sitting unreachable.
func TestPA_AnswerCasesAreTheCatalogLegs(t *testing.T) {
	src, err := os.ReadFile("workstream_pa_answer.go")
	if err != nil {
		t.Fatal(err)
	}
	legs := slices.Sorted(maps.Keys(paCatalog))
	for _, f := range paAnswerCaseFaults(src, legs) {
		t.Error(f)
	}
	// Rejection rows: a case for no leg, and a leg with no case.
	if got := strings.Join(paAnswerCaseFaults([]byte(strings.Replace(string(src), `case "patient-dtr":`, `case "patient-dtr", "probe-gone":`, 1)), legs), "\n"); !strings.Contains(got, `case "probe-gone" names no PA leg`) {
		t.Errorf("a case for no leg: %q", got)
	}
	if got := strings.Join(paAnswerCaseFaults(src, append(slices.Clone(legs), "probe-new")), "\n"); !strings.Contains(got, `leg "probe-new" has no case`) {
		t.Errorf("a leg with no case: %q", got)
	}
}

// paAnswerCaseFaults compares the string cases of paWorkstream.answer with legs.
func paAnswerCaseFaults(src []byte, legs []string) []string {
	f, err := parser.ParseFile(token.NewFileSet(), "workstream_pa_answer.go", src, parser.SkipObjectResolution)
	if err != nil {
		return []string{"workstream_pa_answer.go does not parse: " + err.Error()}
	}
	cases, found := map[string]bool{}, false
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "answer" || fn.Recv == nil {
			continue
		}
		found = true
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if cc, ok := n.(*ast.CaseClause); ok {
				for _, e := range cc.List {
					if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if v, err := strconv.Unquote(lit.Value); err == nil {
							cases[v] = true
						}
					}
				}
			}
			return true
		})
	}
	if !found {
		return []string{"no paWorkstream.answer in workstream_pa_answer.go"}
	}
	var out []string
	want := map[string]bool{}
	for _, l := range legs {
		want[l] = true
		if !cases[l] {
			out = append(out, "leg "+strconv.Quote(l)+" has no case in paWorkstream.answer")
		}
	}
	for _, c := range slices.Sorted(maps.Keys(cases)) {
		if !want[c] {
			out = append(out, "paWorkstream.answer's case "+strconv.Quote(c)+" names no PA leg")
		}
	}
	return out
}
