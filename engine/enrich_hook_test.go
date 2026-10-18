package engine

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// optInEnrichment sets g's enrichment opt-in after New, and installs the hook
// as New does at boot.
func optInEnrichment(g *Gateway, on bool) {
	g.cfg.EnrichNativeRequests = on
	g.installEnrichment()
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
	if env.originator.enrich != nil {
		t.Fatalf("a gateway booted without the opt-in holds the hook %T", env.originator.enrich)
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
	hook := &countingEnrichment{inner: env.originator.enrich}
	if set != nil {
		set(hook)
	}
	env.originator.enrich = hook
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
	hook := &countingEnrichment{inner: env.originator.enrich}
	if set != nil {
		set(hook)
	}
	env.originator.enrich = hook
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
// package's non-test files:
//   - Config.EnrichNativeRequests is read only by (*Gateway).installEnrichment;
//   - onBehalfEnrichment is named only in the hook's two files, and built
//     only by installEnrichment;
//   - the reads behind the hook (obtainPrefetch, obtainDTRPatient) are named,
//     called or not, only in on-behalf building's own files: the hook's
//     (enrich_native.go) and its origination (originate*.go).
func TestEnrichmentHook_TheCoreReachesEnrichmentOnlyThroughIt(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	moduleOne := func(name string) bool {
		return name == "enrich_native.go" || strings.HasPrefix(name, "originate")
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
	reads, builds, named := 0, 0, 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, _ := decl.(*ast.FuncDecl)
			inInstaller := isInstaller(fn)
			ast.Inspect(decl, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.SelectorExpr:
					switch n.Sel.Name {
					case "EnrichNativeRequests":
						reads++
						if !inInstaller {
							t.Errorf("%s: Config.EnrichNativeRequests is read outside installEnrichment", fset.Position(n.Pos()))
						}
					case "obtainPrefetch", "obtainDTRPatient":
						named++
						if !moduleOne(name) {
							t.Errorf("%s: %s is named outside on-behalf building's files", fset.Position(n.Pos()), n.Sel.Name)
						}
					}
				case *ast.CompositeLit:
					if id, ok := n.Type.(*ast.Ident); ok && id.Name == "onBehalfEnrichment" {
						builds++
						if !inInstaller {
							t.Errorf("%s: onBehalfEnrichment is built outside installEnrichment", fset.Position(n.Pos()))
						}
					}
				case *ast.Ident:
					if n.Name == "onBehalfEnrichment" && name != "enrich.go" && name != "enrich_native.go" {
						t.Errorf("%s: onBehalfEnrichment is named outside the hook's files", fset.Position(n.Pos()))
					}
				}
				return true
			})
		}
	}
	if reads != 1 || builds != 1 || named == 0 {
		t.Fatalf("found %d reads of the opt-in, %d builds of the hook and %d names of its reads, want 1, 1 and some", reads, builds, named)
	}
}
