package engine

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"slices"
	"sort"
	"strings"
	"testing"
)

// recordedBy is the recorder method each body a transmit sends is recorded
// by, once sent.
var recordedBy = map[transmitBody]string{sendsRequest: "sent", sendsAnswer: "answered"}

// checkBodyRecords reports every listed transmit that does not record the
// body it sends, and every function or package variable that records a body
// without being a listed transmit of one. A transmit records its body itself,
// so a writer of answers added later cannot leave an answer out of the
// exchange record.
//
// Known limits, as for checkTransmitBoundaries: the check sees a call to the
// recorder, not that it runs (a call inside a closure the transmit never
// invokes counts), and it does not follow a recorder method used as a value.
// The rows beside each writer (exchangerecord_answer_test.go and the record
// rows of each route) are what show the call runs, once the check admitted
// the answer.
func checkBodyRecords(files []*ast.File, info *types.Info, rules map[string]boundaryRule) []string {
	type scope struct {
		key  string
		node ast.Node
	}
	var scopes []scope
	for _, f := range files {
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Body != nil {
					scopes = append(scopes, scope{funcKey(d), d.Body})
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok || len(vs.Values) == 0 {
						continue
					}
					names := make([]string, len(vs.Names))
					for i, n := range vs.Names {
						names[i] = n.Name
					}
					for _, v := range vs.Values {
						scopes = append(scopes, scope{"var " + strings.Join(names, ","), v})
					}
				}
			}
		}
	}
	defined := map[string]bool{}
	records := map[string][]string{}
	for _, sc := range scopes {
		k := sc.key
		defined[k] = true
		ast.Inspect(sc.node, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			fn, ok := info.Uses[sel.Sel].(*types.Func)
			if !ok || fn.Pkg() == nil {
				return true
			}
			for _, m := range recordedBy {
				if qualifiedName(fn) == engineImportPath+".exchangeRecorder."+m && !slices.Contains(records[k], m) {
					records[k] = append(records[k], m)
				}
			}
			return true
		})
	}
	var problems []string
	for k, r := range rules {
		if r.kind != transmits || !defined[k] {
			continue
		}
		switch r.body {
		case sendsRequest, sendsAnswer:
			if m := recordedBy[r.body]; !slices.Contains(records[k], m) {
				problems = append(problems, fmt.Sprintf("%s sends a body but does not record it (exchangeRecorder.%s)", k, m))
			}
		case readsOnly:
		default:
			problems = append(problems, k+" is a transmit boundary that names no body")
		}
	}
	for k, ms := range records {
		for _, m := range ms {
			if r, ok := rules[k]; !ok || r.kind != transmits || recordedBy[r.body] != m {
				problems = append(problems, fmt.Sprintf("%s records a body (exchangeRecorder.%s) but is not a listed transmit of one", k, m))
			}
		}
	}
	sort.Strings(problems)
	return problems
}

// Every transmit records the body it sends, once sent: a request by
// exchangeRecorder.sent, an answer, every refusal included, by
// exchangeRecorder.answered. Only a listed transmit records one.
func TestEveryAnswerTransmitRecordsTheAnswer(t *testing.T) {
	fset := token.NewFileSet()
	files := parsePackageSources(t, fset, ".", nil)
	info := typedPackage(t, fset, files)
	for _, p := range checkBodyRecords(files, info, transmitBoundaries) {
		t.Error(p)
	}
}

// TestBodyRecordCheckFires adds one offending function to the real package
// and requires the check to name it.
func TestBodyRecordCheckFires(t *testing.T) {
	const imports = `import (
	"net/http"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

var _ http.ResponseWriter
`
	const writes = `func %s(w http.ResponseWriter, p relay.Payload) {
	b, err := relay.Transmit(p, nil)
	if err != nil {
		return
	}
	%s
	_, _ = w.Write(b)
}`
	rows := []struct {
		name, fn, src string
		rule          boundaryRule
		want          string
	}{
		{"an answer writer that does not record the answer", "probeAnswer", fmt.Sprintf(writes, "probeAnswer", ""),
			boundaryRule{kind: transmits, body: sendsAnswer}, "probeAnswer sends a body but does not record it (exchangeRecorder.answered)"},
		{"an answer writer that records it as a request", "probeWrongBody", fmt.Sprintf(writes, "probeWrongBody", "exchangeOfWriter(w).sent(p)"),
			boundaryRule{kind: transmits, body: sendsAnswer}, "probeWrongBody sends a body but does not record it (exchangeRecorder.answered)"},
		{"a request sender that does not record the request", "probeRequest", fmt.Sprintf(writes, "probeRequest", ""),
			boundaryRule{kind: transmits, body: sendsRequest}, "probeRequest sends a body but does not record it (exchangeRecorder.sent)"},
		{"a transmit that names no body", "probeNoBody", fmt.Sprintf(writes, "probeNoBody", "exchangeOfWriter(w).answered(p)"),
			boundaryRule{kind: transmits}, "probeNoBody is a transmit boundary that names no body"},
		{"a read that records a body", "probeRead", fmt.Sprintf(writes, "probeRead", "exchangeOfWriter(w).answered(p)"),
			boundaryRule{kind: transmits, body: readsOnly}, "probeRead records a body (exchangeRecorder.answered) but is not a listed transmit of one"},
		{"an unlisted function that records an answer", "probeUnlisted", `func probeUnlisted(x *exchangeRecorder, p relay.Payload) { x.answered(p) }`,
			boundaryRule{}, "probeUnlisted records a body (exchangeRecorder.answered) but is not a listed transmit of one"},
		{"a package variable that records an answer", "", `var probeVarRecord = func(x *exchangeRecorder, p relay.Payload) { x.answered(p) }`,
			boundaryRule{}, "var probeVarRecord records a body (exchangeRecorder.answered) but is not a listed transmit of one"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			fset := token.NewFileSet()
			files := parsePackageSources(t, fset, ".", map[string]string{"probe_extra.go": "package engine\n\n" + imports + "\n" + r.src + "\n"})
			info := typedPackage(t, fset, files)
			rules := map[string]boundaryRule{}
			for k, v := range transmitBoundaries {
				rules[k] = v
			}
			if r.rule.kind != 0 {
				rules[r.fn] = r.rule
			}
			got := checkBodyRecords(files, info, rules)
			for _, p := range got {
				if p == r.want {
					return
				}
			}
			t.Fatalf("want the problem %q, got %q", r.want, got)
		})
	}

	t.Run("clean: an answer writer that records the answer", func(t *testing.T) {
		fset := token.NewFileSet()
		files := parsePackageSources(t, fset, ".", map[string]string{"probe_extra.go": "package engine\n\n" + imports + "\n" + fmt.Sprintf(writes, "probeRecorded", "exchangeOfWriter(w).answered(p)") + "\n"})
		info := typedPackage(t, fset, files)
		rules := map[string]boundaryRule{"probeRecorded": {kind: transmits, body: sendsAnswer}}
		for k, v := range transmitBoundaries {
			rules[k] = v
		}
		if got := checkBodyRecords(files, info, rules); len(got) != 0 {
			t.Fatalf("unexpected problems: %q", got)
		}
	})
}
