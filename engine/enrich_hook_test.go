//go:build !shn_coreonly

package engine

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// optInEnrichment sets g's enrichment opt-in after New and installs the hook
// (or not) in the PA workstream's state, keeping the rest of that state (the
// populator WithPopulator set). On a gateway assembled without New it builds
// the workstreams' states as New does at boot.
func optInEnrichment(g *Gateway, on bool) {
	g.cfg.EnrichNativeRequests = on
	if st := g.paState(); st != nil {
		if err := g.installEnrichment(st); err != nil {
			panic(err)
		}
		return
	}
	g.states = gatewayStates(g)
}

// gatewayStates builds the workstreams' states on a gateway assembled without
// New, as New does at boot; these gateways' configurations are never refused.
func gatewayStates(g *Gateway) map[string]gatewayState {
	states, err := workstreamGatewayStates(g)
	if err != nil {
		panic(err)
	}
	return states
}

// countingEnrichment is a hook that records every call and hands it to inner,
// or answers it with the value set for the key.
type countingEnrichment struct {
	inner                   nativeEnrichment
	prefetchKeys            []string
	coverageFilterAsks      int
	patientReads            int
	prefetchAnswer, patient []byte
}

func (c *countingEnrichment) prefetchValue(ctx context.Context, leg, key, sorID string, fence patientFence) ([]byte, SearchOutcome, int, string) {
	c.prefetchKeys = append(c.prefetchKeys, key)
	if c.prefetchAnswer != nil {
		return c.prefetchAnswer, SearchOK, 0, ""
	}
	return c.inner.prefetchValue(ctx, leg, key, sorID, fence)
}

func (c *countingEnrichment) dtrCoverageFilters() []SearchDateRange {
	c.coverageFilterAsks++
	return c.inner.dtrCoverageFilters()
}

func (c *countingEnrichment) dtrPatient(ctx context.Context, member string, fence patientFence) ([]byte, int, string) {
	c.patientReads++
	if c.patient != nil {
		return c.patient, 0, ""
	}
	return c.inner.dtrPatient(ctx, member, fence)
}

// The enrichment hook is installed at boot only under the participant's
// opt-in: with it off the core holds no hook and runs no enrichment code.
func TestEnrichmentHook_InstalledOnlyUnderTheOptIn(t *testing.T) {
	env := newInProcessExchange(t)
	if env.originator.paState().enrich != nil {
		t.Fatalf("a gateway booted without the opt-in holds the hook %T", env.originator.paState().enrich)
	}
	on, err := New(func() Config { c := env.originator.cfg; c.EnrichNativeRequests = true; return c }())
	if err != nil {
		t.Fatal(err)
	}
	if hook, ok := on.enrichment().(onBehalfEnrichment); !ok || hook.g != on {
		t.Fatalf("a gateway booted with the opt-in holds %#v, want on-behalf building's enrichment of itself", on.enrichment())
	}
	optInEnrichment(on, false)
	if on.enrichment() != nil {
		t.Fatal("turning the opt-in off leaves the hook installed")
	}
}

// countedCRDIngress posts a CDS Hooks request carrying only its coverage to a
// provider opted in to enrichment, with a counting hook over the real one.
func countedCRDIngress(t *testing.T, set func(*countingEnrichment)) (*inProcessExchange, *httptest.ResponseRecorder, *countingEnrichment) {
	t.Helper()
	env := newInProcessExchange(t)
	env.originator.cfg.SoR = newPrefetchSoR().sor()
	optInEnrichment(env.originator, true)
	hook := &countingEnrichment{inner: env.originator.paState().enrich}
	if set != nil {
		set(hook)
	}
	env.originator.paState().enrich = hook
	rec := httptest.NewRecorder()
	env.originator.handleCRDIngress(rec, crdIngressPost(ehrRequest(`"coverage":`+ehrCoverage)))
	return env, rec, hook
}

// The CDS Hooks ingress asks the hook once for each advertised key the
// request leaves out, in the advertised order, and for nothing it carries.
func TestEnrichmentHook_CRDAsksOncePerLeftOutKey(t *testing.T) {
	env, rec, hook := countedCRDIngress(t, nil)
	if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
		t.Fatalf("answer %d %s (network hits %d)", rec.Code, rec.Body.String(), env.routeHitCount())
	}
	want := slices.DeleteFunc(slices.Clone(pinnedPrefetchKeys), func(k string) bool { return k == "coverage" })
	if !slices.Equal(hook.prefetchKeys, want) {
		t.Fatalf("the hook was asked for %v, want each left-out key once: %v", hook.prefetchKeys, want)
	}
	if hook.coverageFilterAsks != 0 || hook.patientReads != 0 {
		t.Fatalf("a CDS Hooks request asked the questionnaire-package reads (%d, %d)", hook.coverageFilterAsks, hook.patientReads)
	}
}

// otherPatient is a Patient record of someone other than the bound patient.
var otherPatient = []byte(`{"resourceType":"Patient","id":"other","name":[{"family":"Other"}],"birthDate":"1960-01-01"}`)

// The fill fence is the core's: a record of another patient the hook returns
// is refused before the network, whatever the hook checked.
func TestEnrichmentHook_CoreFencesWhatTheHookReturns(t *testing.T) {
	t.Run("crd", func(t *testing.T) {
		env, rec, _ := countedCRDIngress(t, func(c *countingEnrichment) { c.prefetchAnswer = otherPatient })
		refusedBeforeTheNetwork(t, env, rec, http.StatusBadGateway, fillFencedOtherPatient)
	})
	t.Run("dtr", func(t *testing.T) {
		env, rec, _ := countedDTRIngress(t, true, func(c *countingEnrichment) { c.patient = otherPatient })
		refusedBeforeTheNetwork(t, env, rec, http.StatusBadGateway, fillFencedOtherPatient)
	})
}

// countedDTRIngress posts a questionnaire-package request, carrying its
// Coverage or not, to a provider opted in to enrichment, with a counting hook
// over the real one.
func countedDTRIngress(t *testing.T, carriesCoverage bool, set func(*countingEnrichment)) (*inProcessExchange, *httptest.ResponseRecorder, *countingEnrichment) {
	t.Helper()
	env := newInProcessExchange(t)
	sor := newPrefetchSoR()
	sor.answer(t, "Coverage", page("", "", sorEntry(sorCoverage("cov-1", shnsdk.CMSPayerIdentity.Value)), includeEntry(`{"resourceType":"Organization","id":"org-1","name":"Payer"}`)))
	env.originator.cfg.SoR = recordSoR{searchingPrefetchSoR{sor}}
	optInEnrichment(env.originator, true)
	hook := &countingEnrichment{inner: env.originator.paState().enrich}
	if set != nil {
		set(hook)
	}
	env.originator.paState().enrich = hook
	declareFramedDTR(t, env, true)
	env.payerReturns(LegResult{Response: testResponse(packageAnswer)})
	params := []string{ehrOrderParam("sr1", prefetchMember)}
	if carriesCoverage {
		params = append(params, ehrCoverageParam(prefetchMember, "00001"))
	}
	return env, postDTRIngress(env, ehrParams(append(params, dtrQuestionnaire)...)), hook
}

// The questionnaire-package ingress asks the hook for the Patient once, and
// for the coverage template's search filters once only when the request
// carries no Coverage; it never asks the CDS Hooks read.
func TestEnrichmentHook_DTRAsksOncePerRead(t *testing.T) {
	for _, carries := range []bool{true, false} {
		env, rec, hook := countedDTRIngress(t, carries, nil)
		if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
			t.Fatalf("carries coverage %v: answer %d %s (network hits %d)", carries, rec.Code, rec.Body.String(), env.routeHitCount())
		}
		wantSearches := 1
		if carries {
			wantSearches = 0
		}
		if hook.patientReads != 1 || hook.coverageFilterAsks != wantSearches || len(hook.prefetchKeys) != 0 {
			t.Fatalf("carries coverage %v: the hook was asked %d Patient reads, %d coverage search filters and prefetch %v; want 1, %d and none",
				carries, hook.patientReads, hook.coverageFilterAsks, hook.prefetchKeys, wantSearches)
		}
	}
}

// The core reaches the enrichment code only through the hook. In the
// package's non-test files (enrichmentHookFaults):
//   - Config.EnrichNativeRequests is read only by (*Gateway).installEnrichment;
//   - the hook's builder (nativeEnrichmentFor) is named only where it is
//     declared (workstream_pa_enrich.go), compared with nil and called by
//     name in installEnrichment, and set by on-behalf building's init in its own file
//     (module1_enrich_native.go), once each;
//   - onBehalfEnrichment is named only in that file, and built only by that
//     init;
//   - the reads behind the hook (obtainPrefetch, obtainDTRPatient) are named,
//     called or not, only in on-behalf building's own files: the hook's
//     (module1_enrich_native.go) and module 1's other files (module1_*.go).
func TestEnrichmentHook_TheCoreReachesEnrichmentOnlyThroughIt(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]*ast.File{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = f
	}
	for _, f := range enrichmentHookFaults(fset, files) {
		t.Error(f)
	}
}

// enrichmentHookFaults are the ways files, by base name, reach the enrichment
// code other than through the hook (TestEnrichmentHook_TheCoreReachesEnrichmentOnlyThroughIt).
func enrichmentHookFaults(fset *token.FileSet, files map[string]*ast.File) []string {
	var faults []string
	fault := func(n ast.Node, format string, args ...any) {
		faults = append(faults, fset.Position(n.Pos()).String()+": "+fmt.Sprintf(format, args...))
	}
	moduleOne := func(name string) bool {
		return strings.HasPrefix(name, "module1_")
	}
	isInstaller := func(fn *ast.FuncDecl) bool {
		if fn == nil || fn.Name.Name != "installEnrichment" || fn.Recv == nil || len(fn.Recv.List) != 1 {
			return false
		}
		star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			return false
		}
		id, ok := star.X.(*ast.Ident)
		return ok && id.Name == "Gateway"
	}
	isModuleOneInit := func(name string, fn *ast.FuncDecl) bool {
		return name == "module1_enrich_native.go" && fn != nil && fn.Name.Name == "init" && fn.Recv == nil
	}
	reads, builds, named, calls, sets, declared := 0, 0, 0, 0, 0, 0
	for _, name := range slices.Sorted(maps.Keys(files)) {
		for _, decl := range files[name].Decls {
			fn, _ := decl.(*ast.FuncDecl)
			inInstaller, inInit := isInstaller(fn), isModuleOneInit(name, fn)
			// builder are the names of nativeEnrichmentFor this file may
			// hold: its declaration, the installer's nil check and call, the
			// init's set.
			builder := map[*ast.Ident]bool{}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.ValueSpec:
					for _, id := range n.Names {
						if id.Name == "nativeEnrichmentFor" && name == "workstream_pa_enrich.go" {
							declared++
							builder[id] = true
						}
					}
				case *ast.BinaryExpr:
					id, ok := n.X.(*ast.Ident)
					if nilID, isNil := n.Y.(*ast.Ident); ok && isNil && id.Name == "nativeEnrichmentFor" && nilID.Name == "nil" && n.Op == token.EQL && inInstaller {
						builder[id] = true
					}
				case *ast.CallExpr:
					if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "nativeEnrichmentFor" && inInstaller {
						calls++
						builder[id] = true
					}
				case *ast.AssignStmt:
					for _, lhs := range n.Lhs {
						if id, ok := lhs.(*ast.Ident); ok && id.Name == "nativeEnrichmentFor" && inInit {
							sets++
							builder[id] = true
						}
					}
				case *ast.SelectorExpr:
					switch n.Sel.Name {
					case "EnrichNativeRequests":
						reads++
						if !inInstaller {
							fault(n, "Config.EnrichNativeRequests is read outside installEnrichment")
						}
					case "obtainPrefetch", "obtainDTRPatient":
						named++
						if !moduleOne(name) {
							fault(n, "%s is named outside on-behalf building's files", n.Sel.Name)
						}
					}
				case *ast.CompositeLit:
					if id, ok := n.Type.(*ast.Ident); ok && id.Name == "onBehalfEnrichment" {
						builds++
						if !inInit {
							fault(n, "onBehalfEnrichment is built outside on-behalf building's init")
						}
					}
				case *ast.Ident:
					if n.Name == "onBehalfEnrichment" && name != "module1_enrich_native.go" {
						fault(n, "onBehalfEnrichment is named outside on-behalf building's enrichment file")
					}
					if n.Name == "nativeEnrichmentFor" && !builder[n] {
						fault(n, "nativeEnrichmentFor is named other than where it is declared, called by installEnrichment or set by on-behalf building's init")
					}
				}
				return true
			})
		}
	}
	if reads != 1 || builds != 1 || named == 0 || declared != 1 || calls != 1 || sets != 1 {
		faults = append(faults, fmt.Sprintf("found %d reads of the opt-in, %d builds of the hook, %d names of its reads, and its builder declared %d, called %d and set %d times; want 1, 1, some, and 1, 1 and 1",
			reads, builds, named, declared, calls, sets))
	}
	return faults
}

// TestEnrichmentHookFaults_Rows: the hook fence accepts the hook's shape and
// refuses each way around it.
func TestEnrichmentHookFaults_Rows(t *testing.T) {
	base := map[string]string{
		"workstream_pa_enrich.go": `package engine

var nativeEnrichmentFor func(g *Gateway) nativeEnrichment

func (g *Gateway) installEnrichment(st *paGatewayState) error {
	if !g.cfg.EnrichNativeRequests {
		return nil
	}
	if nativeEnrichmentFor == nil {
		return errNoEnrichment
	}
	st.enrich = nativeEnrichmentFor(g)
	return nil
}
`,
		"module1_enrich_native.go": `package engine

func init() {
	nativeEnrichmentFor = func(g *Gateway) nativeEnrichment { return onBehalfEnrichment{g: g} }
}

func (e onBehalfEnrichment) dtrPatient() { e.g.obtainDTRPatient() }
`,
		"gateway.go": "package engine\n",
	}
	faults := func(srcs map[string]string) []string {
		t.Helper()
		fset := token.NewFileSet()
		files := map[string]*ast.File{}
		for name, src := range srcs {
			f, err := parser.ParseFile(fset, name, src, 0)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			files[name] = f
		}
		return enrichmentHookFaults(fset, files)
	}
	if f := faults(base); len(f) != 0 {
		t.Fatalf("the hook's own shape is refused: %v", f)
	}
	with := func(file, old, new string) map[string]string {
		out := maps.Clone(base)
		if !strings.Contains(out[file], old) {
			t.Fatalf("%s: the row's anchor %q drifted", file, old)
		}
		out[file] = strings.Replace(out[file], old, new, 1)
		return out
	}
	for name, row := range map[string]struct {
		srcs map[string]string
		want string
	}{
		"the core builds module 1's enrichment":           {with("workstream_pa_enrich.go", "nativeEnrichmentFor(g)", "onBehalfEnrichment{g: g}"), "onBehalfEnrichment is named outside"},
		"the installer calls the builder through a value": {with("workstream_pa_enrich.go", "st.enrich = nativeEnrichmentFor(g)", "f := nativeEnrichmentFor\n\tst.enrich = f(g)"), "nativeEnrichmentFor is named other than"},
		"another function calls the builder":              {with("gateway.go", "package engine\n", "package engine\n\nfunc other(g *Gateway) { _ = nativeEnrichmentFor(g) }\n"), "nativeEnrichmentFor is named other than"},
		"another function compares the builder":           {with("gateway.go", "package engine\n", "package engine\n\nfunc other() bool { return nativeEnrichmentFor == nil }\n"), "nativeEnrichmentFor is named other than"},
		"another function sets the builder":               {with("gateway.go", "package engine\n", "package engine\n\nfunc other() { nativeEnrichmentFor = nil }\n"), "nativeEnrichmentFor is named other than"},
		"another file declares the builder":               {with("gateway.go", "package engine\n", "package engine\n\nvar nativeEnrichmentFor func(g *Gateway) nativeEnrichment\n"), "nativeEnrichmentFor is named other than"},
		"another function reads the opt-in":               {with("gateway.go", "package engine\n", "package engine\n\nfunc other(g *Gateway) bool { return g.cfg.EnrichNativeRequests }\n"), "EnrichNativeRequests is read outside"},
		"the core names a read behind the hook":           {with("gateway.go", "package engine\n", "package engine\n\nfunc other(g *Gateway) { g.obtainPrefetch() }\n"), "obtainPrefetch is named outside"},
		"no init sets the builder":                        {with("module1_enrich_native.go", "func init() {", "func setup() {"), "nativeEnrichmentFor is named other than"},
		"the installer never calls the builder":           {with("workstream_pa_enrich.go", "st.enrich = nativeEnrichmentFor(g)", "st.enrich = nil"), "called 0"},
	} {
		got := faults(row.srcs)
		if !slices.ContainsFunc(got, func(f string) bool { return strings.Contains(f, row.want) }) {
			t.Errorf("%s: faults %v, want one containing %q", name, got, row.want)
		}
	}
}
