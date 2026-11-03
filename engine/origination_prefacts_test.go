//go:build !shn_coreonly

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

// The order-dispatch routes (HomeOxygen, /scenario/dispatch and UC-03 on
// provider data) build the PAS claim in the shared tail (submitPASClaim), after
// their order-dispatch and questionnaire legs, so a claim fact the build at the
// routed line would refuse is refused there, as the order-select routes refuse
// it: 422 naming the element, with no claim sent, and the legs before it, which
// were carried and stand, named in the answer. UC-03's demo default arm builds
// its claim in the same tail after attesting item 6.1; its own check before
// that attestation is TestOrigination_NoClinicianRefusedAtTheLegThatNamesIt's.

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

func TestDispatchRoutes_RefuseUnstatedFactsAtThePASBuild(t *testing.T) {
	t.Parallel()
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
			run(fix.gw, rec, withOriginationClaimInput(withLegsTracked(httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))))
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
				if !legAttempted(legs, "crd-order-dispatch") || legAttempted(legs, "pas-claim") {
					t.Fatalf("legs %v: want the order-dispatch leg sent and no claim", legs)
				}
				wantCarried(t, rec.Body.String(), legs...)
			})
		}
	}
}

// TestPASOriginationsCheckSourcesAtTheClaimsBuild is the static guard on
// where a PAS origination checks what its claim consumes (a refusal sits at
// the build of the first leg that cannot succeed), over every
// non-test file of this package:
//
//  1. each origination prefix (runCRDThenDTROrder, runCRDDispatch) checks only
//     the request's shape before its first leg (pasOriginationRequestOrFail,
//     before any call that sends a leg: OriginateLeg, or a function of this
//     package that reaches it), and never what the claim consumes
//     (pasClaimSourcesAt, pasClaimInputOrRefuse), which a not-covered CRD
//     answer would never need;
//  2. every function that builds a PAS claim or amendment checks what that
//     build consumes (pasClaimInputOrRefuse, pasClaimSourcesAt or, for an
//     amendment, resumeAmendmentOrFail) before the build — the PAS builders
//     themselves excepted, and the shared tail's caller-checked path
//     (sourcesRead), whose callers this rule checks in turn.
//
// A new route that builds a claim without the check, or hoists it before the
// first leg, fails here.
func TestPASOriginationsCheckSourcesAtTheClaimsBuild(t *testing.T) {
	t.Parallel()
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

	sourceChecks := map[string]bool{"pasClaimSourcesAt": true, "pasClaimInputOrRefuse": true}
	prefixes := map[string]bool{"runCRDThenDTROrder": true, "runCRDDispatch": true}
	for name := range prefixes {
		fn, ok := funcs[name]
		if !ok {
			t.Fatalf("the prefix %s is gone; the guard no longer reads the origination paths", name)
		}
		check := firstCall(fn, map[string]bool{"pasOriginationRequestOrFail": true})
		if check == token.NoPos {
			t.Errorf("%s does not call pasOriginationRequestOrFail", name)
			continue
		}
		if send := firstSend(fn, nil); send != token.NoPos && send < check {
			t.Errorf("%s: a leg is sent at %s before pasOriginationRequestOrFail at %s", name, fset.Position(send), fset.Position(check))
		}
		if hoisted := firstCall(fn, sourceChecks); hoisted != token.NoPos {
			t.Errorf("%s checks what the claim consumes at %s, before its first leg", name, fset.Position(hoisted))
		}
	}

	builds := map[string]bool{
		"buildModuleOnePASSubmit": true, "buildModuleOnePASUpdate": true, "buildPASSubmitBundle": true,
	}
	builders := map[string]bool{"buildModuleOnePASSubmit": true, "buildModuleOnePASUpdate": true, "buildPASSubmitBundle": true}
	checks := map[string]bool{"pasClaimSourcesAt": true, "pasClaimInputOrRefuse": true, "resumeAmendmentOrFail": true}
	var checked []string
	for name, fn := range funcs {
		if builders[name] {
			continue
		}
		build := firstCall(fn, builds)
		if build == token.NoPos {
			continue
		}
		checked = append(checked, name)
		check := firstCall(fn, checks)
		// An amendment built in the same request as its submission rides
		// the submission's check (UC-04's in-flow amendment, UC-05): the
		// function's first build is the submission's, after the check.
		if check == token.NoPos || check > build {
			t.Errorf("%s (%s) builds a PAS request at %s without first checking what it consumes", name, fset.Position(fn.Pos()), fset.Position(build))
		}
	}
	if len(checked) < 8 {
		t.Fatalf("the guard checked %d PAS-building functions (%v); it no longer reads the origination paths", len(checked), checked)
	}
	// The shared tail checks what the claim consumes for its caller unless
	// the caller already did: every call hands it the route's member to check
	// the request's against (statedMember), or says it checked (sourcesRead).
	tailCalls := 0
	for name, fn := range funcs {
		for _, c := range callsOf(fn) {
			if c.name != "submitClaimAndFollow" {
				continue
			}
			for _, arg := range c.call.Args {
				lit, ok := arg.(*ast.CompositeLit)
				if !ok {
					continue
				}
				if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "pasFollowInputs" {
					continue
				}
				tailCalls++
				states := false
				for _, el := range lit.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok {
						if k, ok := kv.Key.(*ast.Ident); ok && (k.Name == "statedMember" || k.Name == "sourcesRead") {
							states = true
						}
					}
				}
				if !states {
					t.Errorf("%s (%s) calls the shared tail without statedMember or sourcesRead", name, fset.Position(c.call.Pos()))
				}
			}
		}
	}
	if tailCalls < 3 {
		t.Fatalf("the guard read %d calls of the shared tail; it no longer reads them", tailCalls)
	}
	slices.Sort(checked)
	t.Logf("PAS-building functions checked: %v", checked)
}

// TestSingleCallAmendmentLine_IsTheSubmissionsBuildLine pins, for a single
// call that also amends (UC-04's in-flow amendment, UC-05, UC-06/UC-07), that
// the amendment is built at the line its submission was: the submission at
// the line its selection builds at (selectLegRoute), the amendment at the line
// the resume's selection picks for the submission's pin (selectResumeRoute),
// where the amendment's facts are checked. Within one request both read the same
// declared, lane and narrowing state, so over every state a gateway can boot
// in (gateway/app refuses a declared line with no lane at every level that
// checks) the two build lines are the same line, or the resume refuses the
// route outright (never a facts refusal at some other line), and the resume is
// never carried away from the pin it was just given.
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
						if slices.ContainsFunc(own, func(l string) bool { return !slices.Contains(laned, l) }) {
							continue // a declared line with no lane: this gateway does not boot
						}
						byLine := map[string]shnsdk.Validator{}
						for _, l := range laned {
							byLine[l] = fake
						}
						reg := shnsdk.NewRegistry()
						reg.Set("payer", shnsdk.RegistryEntry{ID: "payer", Role: "payer", ContractVersions: tokens(peer)})
						g := &Gateway{cfg: Config{
							DeclaredContractVersions: tokens(own),
							Reg:                      reg,
							ValidatorsByContractLine: contractLanes(byLine),
							TranslateIGLines:         true,
							EgressNativeLines:        narrow,
							StrictPeerForTest:        strict,
						}}
						route, err := g.selectLegRoute("payer", "pas-claim", "")
						if err != nil {
							continue
						}
						resumed, err := g.selectResumeRoute(route.Token, "payer", "pas-claim-update", "")
						if err != nil {
							continue
						}
						compared++
						if len(route.Chain) > 0 {
							chained++
						} else if !slices.Contains(own, route.BuildLine) {
							nativeReach++
						}
						if resumed.Pinned != "" {
							t.Errorf("own %v peer %v laned %v narrow %v strict %v: the resume was carried away from the pin %s it was just given",
								own, peer, laned, narrow, strict, resumed.Pinned)
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
