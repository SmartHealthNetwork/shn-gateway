package engine

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// findingContextHandlers are the HTTP handler entries that own a leg and
// therefore must tag the request context before any governed check runs. A
// handler added to this package that reaches the choke point without setting
// the context would emit findings labelled "unknown": legible, but useless for
// a partner reading its own log. Add the handler here AND set the context;
// removing a row is only correct when the handler is gone.
var findingContextHandlers = []string{
	"handleInbound",
	"handleCRDIngress", "handleDTRIngress", "handlePASIngress",
	"handleScenario",
	"handleUC02", "handleUC02PayerB", "handleUC02UnknownPayer",
	"handleUC03", "handleUC03Oxygen", "handleUC03Bridge",
	"handleUC04", "handleUC05", "handleUC07HCPCS", "handleUC08",
	"handleHomeOxygen", "handleDispatch",
	"handleUC06", "handleUC07", "handleUC06Start", "handleUC07Start",
	"handleUC06Complete", "handleUC07Complete",
	"handlePatientAccessEOB", "handlePatientAccessEOBByID",
}

// engineFiles parses the non-test sources of gateway/engine. dirs lets a
// future $validate census also walk gateway/app: gateway/app has no
// production .Validate( call today, which is exactly the state such a guard
// would exist to preserve.
func engineFiles(t *testing.T, dirs ...string) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	if len(dirs) == 0 {
		dirs = []string{"."}
	}
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
			if err != nil {
				t.Fatalf("parse %s/%s: %v", dir, name, err)
			}
			// Key by dir/name so two same-named files (or two same-named
			// functions in different files) can never collide silently.
			files[filepath.Join(dir, name)] = f
		}
	}
	return fset, files
}

func TestEveryLegHandlerSetsTheFindingContext(t *testing.T) {
	_, files := engineFiles(t)
	setsContext := map[string]bool{}
	seen := map[string]bool{}
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			seen[fn.Name.Name] = true
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "withFindingContext" {
					setsContext[fn.Name.Name] = true
				}
				return true
			})
		}
	}
	var missing []string
	for _, h := range findingContextHandlers {
		if !seen[h] {
			t.Errorf("%s is listed but no longer exists in gateway/engine — remove the row or restore the handler", h)
			continue
		}
		if !setsContext[h] {
			missing = append(missing, h)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("these leg handlers must call withFindingContext before any governed check: %s", strings.Join(missing, ", "))
	}
}

// Every runtime $validate in this package is in exactly one of five
// categories. The allowlist is the truth: "a single function" is not claimed,
// and a new call must be classified deliberately, in this test, by whoever
// adds it. Categories:
//
//	choke        — validateGoverned itself, the one place a verdict decides
//	observational — a validate whose outcome never changes the answer
//	delegating   — a wrapper that forwards to another Validator
//	appendix-b   — a site whose own parse error persists at both levels (spec
//	               appendix B). No entry uses it today: appendix B's sites are
//	               validateFHIR* entries, covered by the second test below.
//	               Keep the category so a future direct-Validator site of that
//	               shape has an honest home.
//	not-fhir     — a method that merely SHARES the name Validate and checks no
//	               FHIR at all. The census matches call shape, not types (see
//	               the limits paragraph below), so a name collision needs an
//	               explicit home rather than a silent pass. Entries here must
//	               be re-read, not assumed: the excuse is per enclosing
//	               function, so a real validator call added to that same
//	               function would inherit it.
//
// Coverage: TestEveryValidateCallIsClassified walks both a function body
// (keyed by the enclosing function's name) and a package-level var/const
// initializer (keyed by the name the value is assigned to) — a `.Validate(`
// written inside a func literal assigned to a package var is just as visible
// as one inside an ordinary method.
//
// What neither test here can see, and why that's an accepted limit rather
// than an oversight: both tests match call *shape* syntactically — a
// *ast.SelectorExpr whose .Sel.Name is exactly "Validate" (first test) or
// exactly one of the choke-reaching names (second test). Neither resolves
// types or receivers. Two consequences: (1) `f := someValidator.Validate;
// f(ctx, ...)` turns the call into a bare *ast.Ident, invisible to the first
// test — a method value or a validator stashed in a function variable evades
// this guard entirely; (2) the second test's "reaches the choke point" check
// would be satisfied by any method anywhere in the walked files literally
// named validateGoverned/validateFHIRAtProfile/validateFHIRForContract,
// regardless of receiver — a same-named decoy would pass. Closing either gap
// for real needs a go/types-based (semantic) check, a materially bigger
// dependency and lift than this guard's actual job. That job is catching
// accidental drift — someone adding a validate call without thinking about
// the choke point — not defeating someone deliberately hiding one; a
// syntactic guard is the right tool for the former and relies on code review
// for the latter. Don't mistake this test's silence on a method-value or
// decoy-name case for proof there isn't one.
//
// Scope: both tests walk only engineFiles(t, ".", "../app") — the top-level
// (non-recursive) contents of gateway/engine and gateway/app. gateway/engine
// has two subpackages, engine/relay and engine/relay/internal/splice, that
// os.ReadDir never descends into and that are therefore structurally outside
// every census in this file. Both hold zero `.Validate(` sites today, so the
// boundary costs nothing now — but it is a chosen boundary, not an accident,
// and would need extending here if either subpackage ever validates FHIR
// resources directly.
var validateCallSites = map[string]string{
	"gateway.go:validateGoverned":      "choke",
	"crd_native.go:observeCRDEmbedded": "observational",
	"certify.go:collectCertification":  "observational",
	"lanes.go:Validate":                "delegating",
	"observer.go:Validate":             "delegating",
	// EOBRecord.Validate (pendledger.go) checks the ledger row's own fields —
	// an EOB id, non-empty bytes, and a subject matching the decision's — and
	// never reaches a Validator. It is the one name collision in this
	// package.
	"memstore.go:RecordDecision": "not-fhir",
}

// validateFHIREntryPoints are the wrappers that reach the choke point, keyed
// file:name like validateCallSites (so two same-named wrappers in different
// files can never be confused with each other). The census must cover these
// too, not only .Validate( calls: a new wrapper that skipped validateGoverned
// would otherwise be invisible here. Every entry must be a function whose
// body calls validateGoverned (directly or through another listed entry).
var validateFHIREntryPoints = []string{
	"gateway.go:validateFHIR", "gateway.go:validateFHIRAtProfile",
	"gateway.go:validateFHIRForContract", "gateway.go:validateFHIRPayerIngress",
	"gateway.go:validateFHIREgressOrBridged",
}

// A note for whoever next counts call sites here, so they don't redo the work this
// comment cost. A bare grep for the substring `g.validateFHIR`, with no requirement
// that it be followed by `(`, over-reports: several comments name a validateFHIR*
// wrapper only to say that site deliberately does NOT route through it. Those are
// prose, not calls; they belong in no allowlist above and no census here needs to
// see them. Count with the AST, as TestValidateFHIRCallSiteFloor below does.
//
// The figure is asserted rather than written down, because a number in a comment
// rots silently and this one already has: it read 49 until the inquiry-leg change
// both ADDED a governed check (the inquiry leg's side-effect loop) and DELETED one (the PAS
// terminal-response assembly, gone with the whole class of failure it carried).
// A count that moves in both directions in a single merge is exactly the kind you
// must re-derive from the tree rather than adjust by the size of your own diff.

// TestValidateFHIRCallSiteFloor pins the number of governed-validator call sites so
// the two censuses above cannot pass by walking a tree they have stopped seeing. It
// is a floor against a broken walk, NOT a budget: adding a call site is fine and
// this test tells you to update the number deliberately, having classified the new
// site in TestEveryValidateCallIsClassified.
func TestValidateFHIRCallSiteFloor(t *testing.T) {
	const (
		wantTotal       = 51
		wantDelegations = 2 // the wrappers' own internal delegations, both in gateway.go
	)
	_, files := engineFiles(t, ".")
	total, delegations := 0, 0
	for name, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !strings.HasPrefix(sel.Sel.Name, "validateFHIR") {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "g" {
				return true
			}
			total++
			if strings.HasSuffix(name, "gateway.go") {
				delegations++
			}
			return true
		})
	}
	if total != wantTotal {
		t.Errorf("gateway/engine holds %d non-test g.validateFHIR*( call sites, want %d. Fewer means either a site "+
			"was removed or this AST walk has stopped seeing the tree — and a walk that sees nothing makes every "+
			"classification census above pass vacuously. More means a new governed check arrived: classify it in "+
			"TestEveryValidateCallIsClassified, make sure the handler that owns its leg tags the finding context, "+
			"then update this number deliberately.", total, wantTotal)
	}
	if delegations != wantDelegations {
		t.Errorf("gateway.go holds %d internal validateFHIR* delegations, want %d — the wrapper layer changed shape, "+
			"so the classification categories above may no longer describe it", delegations, wantDelegations)
	}
}

func TestEveryValidateCallIsClassified(t *testing.T) {
	fset, files := engineFiles(t, ".", "../app")
	var unclassified []string
	inspectForValidateCalls := func(key string, node ast.Node) {
		ast.Inspect(node, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Validate" {
				return true
			}
			if _, known := validateCallSites[key]; !known {
				unclassified = append(unclassified, key+" at "+fset.Position(call.Pos()).String())
			}
			return true
		})
	}
	for name, f := range files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				// A raw .Validate( inside an ordinary function or method body.
				if d.Body != nil {
					inspectForValidateCalls(name+":"+d.Name.Name, d.Body)
				}
			case *ast.GenDecl:
				// A raw .Validate( inside a package-level var/const initializer —
				// e.g. a func literal assigned to a package var — which no
				// *ast.FuncDecl walk ever reaches. Keyed by the name the value is
				// assigned to, so the key stays stable across unrelated line
				// churn elsewhere in the file.
				if d.Tok != token.VAR && d.Tok != token.CONST {
					continue
				}
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, val := range vs.Values {
						valName := vs.Names[0].Name
						if i < len(vs.Names) {
							valName = vs.Names[i].Name
						}
						inspectForValidateCalls(name+":"+valName, val)
					}
				}
			}
		}
	}
	sort.Strings(unclassified)
	if len(unclassified) > 0 {
		t.Errorf("a runtime $validate must be classified in validateCallSites (choke / observational / delegating / appendix-b):\n  %s",
			strings.Join(unclassified, "\n  "))
	}
	for key := range validateCallSites {
		parts := strings.SplitN(key, ":", 2)
		if files[parts[0]] == nil {
			t.Errorf("validateCallSites names %s, but %s no longer exists", key, parts[0])
		}
	}
}

// Every validateFHIR* wrapper reaches the choke point. A new wrapper that
// called a Validator directly would be caught by the census above; one that
// simply forgot to go through validateGoverned is caught here.
func TestEveryValidateFHIREntryPointReachesTheChokePoint(t *testing.T) {
	_, files := engineFiles(t)
	// Keyed file:name, like validateFHIREntryPoints itself — not by bare
	// function name, which would let two same-named functions in different
	// files nondeterministically clobber each other (map iteration order over
	// `files` is not defined).
	bodies := map[string]*ast.FuncDecl{}
	for fileName, f := range files {
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
				bodies[fileName+":"+fn.Name.Name] = fn
			}
		}
	}
	for _, name := range validateFHIREntryPoints {
		fn, ok := bodies[name]
		if !ok {
			t.Errorf("validateFHIREntryPoints names %s, which no longer exists", name)
			continue
		}
		var reaches bool
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				switch sel.Sel.Name {
				case "validateGoverned", "validateFHIRAtProfile", "validateFHIRForContract":
					reaches = true
				}
			}
			return true
		})
		if !reaches {
			t.Errorf("%s does not reach validateGoverned: a governed check that skips the choke point emits no finding", name)
		}
	}
}

// bridgedEgressSites are the post-egressAdapt target-line checks that pass
// the bridged flag: the eleven PAS-bundle sites, and only those. They are the
// only call lines that changed signature; any new one must be added here
// deliberately.
//
// dtr_adaptive.go is deliberately absent and must stay absent: its leg is in
// envelopeEgressLegs, so egressAdapt returns the payload unchanged and there
// is no SHN edit to verify. A zero entry for it below is what keeps a future
// "gap-closing" patch from slipping one in.
var bridgedEgressSites = map[string]int{
	"originate.go":        7,
	"originate_resume.go": 3,
	"pas_tail.go":         1,
	"dtr_adaptive.go":     0,
}

// wantBridgedArg is the ONLY literal expression a bridged-egress call site may
// pass as its fifth (bridged) argument: the site's own route's chain length,
// read fresh at the call — never a hardcoded true/false, and never some other
// route/variable. This is the live invariant the task establishes ("bridged
// iff a chain ran"); asserting the site COUNT without asserting this would
// let `len(route.Chain) > 0` silently become `false` (or `true`) at any site
// and still show a clean 7/3/1 count.
const wantBridgedArg = "len(route.Chain) > 0"

func TestBridgedEgressSitesAreExactlyTheListedOnes(t *testing.T) {
	fset, files := engineFiles(t)
	counts := map[string]int{}
	var badArgs []string
	for name, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "validateFHIREgressOrBridged" {
				return true
			}
			counts[name]++
			pos := fset.Position(call.Pos())
			if len(call.Args) != 5 {
				badArgs = append(badArgs, fmt.Sprintf("%s: %s has %d args, want 5", name, pos, len(call.Args)))
				return true
			}
			var buf strings.Builder
			if err := printer.Fprint(&buf, fset, call.Args[4]); err != nil {
				badArgs = append(badArgs, fmt.Sprintf("%s: %s: could not render the bridged argument: %v", name, pos, err))
				return true
			}
			if got := buf.String(); got != wantBridgedArg {
				badArgs = append(badArgs, fmt.Sprintf("%s: %s passes bridged=%q, want the literal %q — the bridged flag must be the SITE'S OWN route.Chain, read fresh, not a stand-in", name, pos, got, wantBridgedArg))
			}
			return true
		})
	}
	for name, want := range bridgedEgressSites {
		if counts[name] != want {
			t.Errorf("%s has %d bridged egress checks, want %d — a new one must be added to bridgedEgressSites deliberately", name, counts[name], want)
		}
	}
	for name, got := range counts {
		if _, listed := bridgedEgressSites[name]; !listed {
			t.Errorf("%s has %d bridged egress checks but is not listed", name, got)
		}
	}
	sort.Strings(badArgs)
	for _, msg := range badArgs {
		t.Error(msg)
	}
}
