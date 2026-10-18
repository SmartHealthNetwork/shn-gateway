package engine

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The order-dispatch routes (HomeOxygen, /scenario/dispatch, UC-03 on provider
// data, and UC-03's demo default arm) build the PAS claim after their CRD and
// DTR legs, so a claim fact the build at the routed line would refuse is
// refused before the first of them, exactly as the order-select routes refuse
// it (claimFactsBeforeAnyLeg): 422 naming the element, and no leg sent.

// uc03ProviderDataFixture is the MBR-PD-UC03 / E1390 order-dispatch persona
// UC-03 originates for on provider data.
func uc03ProviderDataFixture(t *testing.T) *dispatchFixture {
	t.Helper()
	const member = "MBR-PD-UC03"
	const performerRef = "Organization/org-dme-uc03"
	orderJSON, err := buildDispatchDeviceRequest("dr-uc03", "Patient/"+member, performerRef, "E1390", "Stationary oxygen concentrator", "J96.10")
	if err != nil {
		t.Fatalf("build DeviceRequest: %v", err)
	}
	supplierJSON, err := buildHomeOxygenSupplier("org-dme-uc03")
	if err != nil {
		t.Fatalf("build supplier: %v", err)
	}
	return newDispatchFixture(t, member, Demo{BirthDate: "1962-03-02", FamilyName: "Delgado-Dispatch"}, orderJSON, performerRef, supplierJSON)
}

func TestDispatchRoutes_RefuseUnstatedFactsBeforeAnyLeg(t *testing.T) {
	type route struct {
		name string
		// drive runs the route with body edited by edit and returns the
		// response and every leg the route sent.
		drive func(t *testing.T, edit func(map[string]json.RawMessage)) (*httptest.ResponseRecorder, []string)
	}
	dispatchDrive := func(fixture func(*testing.T) *dispatchFixture, path, extra string, run func(*Gateway, http.ResponseWriter, *http.Request)) func(*testing.T, func(map[string]json.RawMessage)) (*httptest.ResponseRecorder, []string) {
		return func(t *testing.T, edit func(map[string]json.RawMessage)) (*httptest.ResponseRecorder, []string) {
			fix := fixture(t)
			body := testRouteBody(fix.gw, path, extra)
			if edit != nil {
				body = editBody(t, body, edit)
			}
			rec := httptest.NewRecorder()
			run(fix.gw, rec, withOriginationClaimInput(httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))))
			return rec, fix.stub.legTypes
		}
	}
	routes := []route{
		{"homeoxygen", dispatchDrive(newMBROXDispatchFixture, "/scenario/homeoxygen", "",
			func(g *Gateway, w http.ResponseWriter, r *http.Request) { g.handleHomeOxygen(w, r) })},
		{"dispatch", dispatchDrive(newMBROXDispatchFixture, "/scenario/dispatch", `"member":"MBR-OX"`,
			func(g *Gateway, w http.ResponseWriter, r *http.Request) { g.handleDispatch(w, r) })},
		{"uc03 provider-data", dispatchDrive(uc03ProviderDataFixture, "/scenario/uc03", "",
			func(g *Gateway, w http.ResponseWriter, r *http.Request) { g.handleUC03(w, r) })},
		{"uc03 demo default arm", func(t *testing.T, edit func(map[string]json.RawMessage)) (*httptest.ResponseRecorder, []string) {
			gw, stub, _ := uc03HandlerSystem(t, shnsdk.CardCoverage{Covered: shnsdk.CoveredCovered, PANeeded: shnsdk.PANeededAuthNeeded})
			body := testRouteBody(gw, "/scenario/uc03", `"branch":""`)
			if edit != nil {
				body = editBody(t, body, edit)
			}
			rec := httptest.NewRecorder()
			gw.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/scenario/uc03", strings.NewReader(body)))
			return rec, stub.legTypes
		}},
	}
	for _, rt := range routes {
		// The control: the full statement reaches the order-dispatch leg.
		t.Run(rt.name+"/control", func(t *testing.T) {
			rec, legs := rt.drive(t, nil)
			if !legAttempted(legs, "crd-order-dispatch") {
				t.Fatalf("control sent no order-dispatch leg: %d %s legs=%v", rec.Code, rec.Body.String(), legs)
			}
		})
		// The facts every PAS line requires (Claim profile snapshot, 1..1 at
		// 2.0.1, 2.1.0 and 2.2.1), so the row holds at whatever line the
		// fixture routes to.
		for fact, element := range map[string]string{
			"claimType": "Claim.type", "priority": "Claim.priority", "serviceCategory": "Claim.item.category",
		} {
			t.Run(rt.name+"/"+fact, func(t *testing.T) {
				rec, legs := rt.drive(t, dropFact(t, fact))
				if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), element) {
					t.Fatalf("%d %s, want 422 naming %s", rec.Code, rec.Body.String(), element)
				}
				if len(legs) != 0 {
					t.Fatalf("legs sent before the refusal: %v", legs)
				}
			})
		}
	}
}

// TestPASOriginationsCheckFactsBeforeAnyLeg is the static guard on the rule
// above, over every non-test file of this package:
//
//  1. each origination prefix (runCRDThenDTROrder, runCRDDispatch) calls
//     pasOriginationBeforeAnyLeg, and calls it before any call that sends a
//     leg (OriginateLeg, or a function of this package that reaches it);
//  2. every other function that builds a PAS claim or amendment, or sends a
//     pas-claim or pas-claim-update leg, calls a prefix (or, for a resume,
//     resumeAmendmentOrFail) before any call that sends a leg — unless it is
//     one of the PAS tail helpers, whose callers this rule checks in turn.
//
// A new route that builds a claim without passing the check fails here.
func TestPASOriginationsCheckFactsBeforeAnyLeg(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	funcs := map[string]*ast.FuncDecl{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
				funcs[fn.Name.Name] = fn
			}
		}
	}
	calleeName := func(call *ast.CallExpr) string {
		switch f := call.Fun.(type) {
		case *ast.Ident:
			return f.Name
		case *ast.SelectorExpr:
			return f.Sel.Name
		}
		return ""
	}
	type site struct {
		name string
		call *ast.CallExpr
	}
	callsOf := func(fn *ast.FuncDecl) []site {
		var out []site
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				out = append(out, site{calleeName(call), call})
			}
			return true
		})
		return out
	}
	// sends: the functions of this package that reach OriginateLeg.
	sends := map[string]bool{"OriginateLeg": true}
	for grew := true; grew; {
		grew = false
		for name, fn := range funcs {
			if sends[name] {
				continue
			}
			for _, c := range callsOf(fn) {
				if sends[c.name] {
					sends[name], grew = true, true
					break
				}
			}
		}
	}
	firstSend := func(fn *ast.FuncDecl, except map[string]bool) token.Pos {
		for _, c := range callsOf(fn) {
			if sends[c.name] && !except[c.name] {
				return c.call.Pos()
			}
		}
		return token.NoPos
	}
	firstCall := func(fn *ast.FuncDecl, names map[string]bool) token.Pos {
		for _, c := range callsOf(fn) {
			if names[c.name] {
				return c.call.Pos()
			}
		}
		return token.NoPos
	}

	prefixes := map[string]bool{"runCRDThenDTROrder": true, "runCRDDispatch": true}
	for name := range prefixes {
		fn, ok := funcs[name]
		if !ok {
			t.Fatalf("the prefix %s is gone; the guard no longer reads the origination paths", name)
		}
		check := firstCall(fn, map[string]bool{"pasOriginationBeforeAnyLeg": true})
		if check == token.NoPos {
			t.Errorf("%s does not call pasOriginationBeforeAnyLeg", name)
			continue
		}
		if send := firstSend(fn, nil); send != token.NoPos && send < check {
			t.Errorf("%s: a leg is sent at %s before pasOriginationBeforeAnyLeg at %s", name, fset.Position(send), fset.Position(check))
		}
	}

	builds := map[string]bool{
		"buildAuthoredPASSubmit": true, "buildAuthoredPASUpdate": true, "buildPASSubmitBundle": true,
		"submitPASClaim": true, "submitClaimAndFollow": true,
	}
	tails := map[string]bool{
		"buildAuthoredPASSubmit": true, "buildAuthoredPASUpdate": true, "buildPASSubmitBundle": true,
		"submitPASClaim": true, "submitClaimAndFollow": true,
	}
	entries := map[string]bool{"runCRDThenDTROrder": true, "runCRDDispatch": true, "resumeAmendmentOrFail": true}
	var checked []string
	for name, fn := range funcs {
		if prefixes[name] || tails[name] {
			continue
		}
		originates := false
		for _, c := range callsOf(fn) {
			if builds[c.name] {
				originates = true
			}
			if c.name == "OriginateLeg" && len(c.call.Args) > 3 {
				if lit, ok := c.call.Args[3].(*ast.BasicLit); ok && (lit.Value == `"pas-claim"` || lit.Value == `"pas-claim-update"`) {
					originates = true
				}
			}
		}
		if !originates {
			continue
		}
		checked = append(checked, name)
		entry := firstCall(fn, entries)
		if entry == token.NoPos {
			t.Errorf("%s (%s) builds or sends a PAS claim without passing a prefix or resumeAmendmentOrFail", name, fset.Position(fn.Pos()))
			continue
		}
		if send := firstSend(fn, entries); send != token.NoPos && send < entry {
			t.Errorf("%s: a leg is sent at %s before the facts check at %s", name, fset.Position(send), fset.Position(entry))
		}
	}
	if len(checked) < 8 {
		t.Fatalf("the guard checked %d PAS-originating functions (%v); it no longer reads the origination paths", len(checked), checked)
	}
	slices.Sort(checked)
	t.Logf("PAS-originating functions checked: %v", checked)
}

// TestSingleCallAmendmentLine_IsTheSubmissionsBuildLine pins what
// claimFactsBeforeAnyLeg relies on for a single call that also amends (UC-04's
// in-flow amendment, UC-05, UC-06/UC-07): it checks the amendment's facts at
// the line the submission's selection builds at (selectLegRoute), and the
// amendment is built at the line the resume's selection picks for the
// submission's pin (selectResumeRoute). Within one request both read the same
// declared, lane and narrowing state, so over every such state the two build
// lines are the same line, or the resume refuses the route outright (never a
// facts refusal at some other line).
func TestSingleCallAmendmentLine_IsTheSubmissionsBuildLine(t *testing.T) {
	lines := []string{"2.0", "2.1", "2.2"}
	var subsets [][]string
	for mask := 0; mask < 1<<len(lines); mask++ {
		var s []string
		for i, l := range lines {
			if mask&(1<<i) != 0 {
				s = append(s, l)
			}
		}
		subsets = append(subsets, s)
	}
	tokens := func(ls []string) []string {
		var out []string
		for _, l := range ls {
			out = append(out, "pa.pas@"+l)
		}
		return out
	}
	fake := shnsdk.NewFakeValidator()
	compared, chained, nativeReach := 0, 0, 0
	for _, own := range subsets[1:] {
		for _, peer := range subsets {
			for _, laned := range subsets[1:] {
				for _, narrow := range append([][]string{nil}, subsets...) {
					for _, strict := range []bool{false, true} {
						byLine := map[string]shnsdk.Validator{}
						for _, l := range laned {
							byLine[l] = fake
						}
						reg := shnsdk.NewRegistry()
						reg.Set("payer", shnsdk.RegistryEntry{ID: "payer", Role: "payer", ContractVersions: tokens(peer)})
						g := &Gateway{cfg: Config{
							DeclaredContractVersions: tokens(own),
							Reg:                      reg,
							ValidatorsByLine:         byLine,
							EgressNativeLines:        narrow,
							StrictPeerForTest:        strict,
						}}
						route, err := g.selectLegRoute("payer", "pas-claim")
						if err != nil {
							continue
						}
						resumed, err := g.selectResumeRoute(route.Token, "payer", "pas-claim-update")
						if err != nil {
							continue
						}
						compared++
						if len(route.Chain) > 0 {
							chained++
						} else if !slices.Contains(own, route.BuildLine) {
							nativeReach++
						}
						if resumed.BuildLine != route.BuildLine {
							t.Errorf("own %v peer %v laned %v narrow %v strict %v: the submission builds at %s (%s) and the amendment at %s",
								own, peer, laned, narrow, strict, route.BuildLine, route.Token, resumed.BuildLine)
						}
					}
				}
			}
		}
	}
	// The matrix reaches every arm: a shared or default line, native reach
	// and a chain.
	if compared == 0 || chained == 0 || nativeReach == 0 {
		t.Fatalf("compared %d states (%d chained, %d native reach): the matrix no longer reaches every selection arm", compared, chained, nativeReach)
	}
}
