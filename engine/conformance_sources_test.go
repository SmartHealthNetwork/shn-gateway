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

// Every runtime $validate in this package is in exactly one of six
// categories. The allowlist is the truth: "a single function" is not claimed,
// and a new call must be classified deliberately, in this test, by whoever
// adds it. Categories:
//
//	choke        — judgeLines (behind validateGovernedLines), the one place a verdict decides
//	               (validateGoverned is its single-line form)
//	observational — a validate whose outcome never changes the answer
//	delegating   — a wrapper that forwards to another Validator
//	appendix-b   — a site whose own parse error persists at every level (spec
//	               appendix B). No entry uses it today: appendix B's sites are
//	               validateFHIR* entries, covered by the second test below.
//	               Keep the category so a future direct-Validator site of that
//	               shape has an honest home.
//	edit-evidence — a registered edit's own precondition: its verdict decides only
//	               whether SHN makes the edit (an answer it does not evidence is
//	               carried as sent), never refuses and records no finding; every
//	               refusal the edit makes goes through the choke point
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
	"gateway.go:judgeLines":                            "choke",
	"workstream_pa_crd_inbound.go:validateCRDEmbedded": "observational",
	"certify.go:collectCertification":                  "observational",
	// The boot warm-up (gateway/app) discards every verdict: it only reaches
	// the certification endpoint before a real certification has to.
	"../app/certifywarm.go:warmCertificationEndpoint": "observational",
	"lanes.go:Validate":       "delegating",
	"certifylane.go:Validate": "delegating",
	"observer.go:Validate":    "delegating",
	"lanehealth.go:Validate":  "delegating",
	// E-08's source evidence: an answer valid at its own DTR line is renamed,
	// any other is carried as sent; the renamed answer's check and a carried
	// answer's report both go through judgeLines.
	"dtr_answer_bridge.go:dtrAnswerValidAt": "edit-evidence",
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
	"gateway.go:validatePayerAnswerAtCandidates",
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
		wantTotal       = 37
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
		t.Errorf("a runtime $validate must be classified in validateCallSites (choke / observational / delegating / appendix-b / edit-evidence / not-fhir):\n  %s",
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
				case "validateGoverned", "validateGovernedLines", "judgeLines", "validateFHIRAtProfile", "validateFHIRForContract", "validatePayerAnswerAtCandidates":
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

// moduleBuildSites are the module 1 build sites that take a request to its leg
// through the LegClient (sendLeg with a Build), by file, and how many of them
// name a Contract, so that the request is checked at the leg's line (bridged
// when module 2 walked it): the eleven PAS bundle sites. A new one must be
// added here deliberately.
//
// module1_dtr_adaptive.go's next-question site names none and must keep naming none:
// its leg is an envelope leg (envelopeEgressLegs), so a walk changes nothing
// there, and a check that cannot see an SHN edit has nothing to verify.
var moduleBuildSites = map[string]struct{ sites, checked int }{
	"module1_originate.go":            {9, 7},
	"module1_originate_resume.go":     {3, 3},
	"module1_pas_tail.go":             {1, 1},
	"module1_originate_homeoxygen.go": {1, 0},
	"module1_dtr_adaptive.go":         {1, 0},
}

// The bridged check, SHN's own IG-line edit refused at every level, is module
// 2's Certify and runs nowhere else:
//   - a governed check is bridged only in chainBridge.Certify (the literal
//     true), and the choke point's own layers only pass the flag on;
//   - Certify is asked only by egressBuild, for a request module 2 walked, and
//     by E-08 (carryDTRAnswer), for an answer it renamed;
//   - egressBuild decides a request is bridged from its own route's chain,
//     never from an argument, and only it calls egressAdapt;
//   - only the LegClient (sendLeg) calls egressBuild, and the module 1 sites
//     reach their legs through it as listed.
func TestBridgedCheckIsModuleTwosCertify(t *testing.T) {
	fset, files := engineFiles(t)
	passOn := map[string]bool{"validateGoverned": true, "validateGovernedLines": true}
	certifyCallers := map[string]int{}
	adaptCallers := map[string]int{}
	buildCallers := map[string]int{}
	builds := map[string]struct{ sites, checked int }{}
	sites, assigned := 0, 0
	for name, f := range files {
		base := filepath.Base(name)
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pos := fset.Position(call.Pos())
				switch sel.Sel.Name {
				case "validateGoverned", "validateGovernedLines", "judgeLines":
					// judgeLines is also given module 3, ahead of the
					// finding context.
					at := 7
					if sel.Sel.Name == "judgeLines" {
						at = 8
					}
					if len(call.Args) <= at {
						t.Errorf("%s: %s has %d args", pos, sel.Sel.Name, len(call.Args))
						return true
					}
					arg := call.Args[at]
					switch id, _ := arg.(*ast.Ident); {
					case id != nil && id.Name == "false":
					case id != nil && id.Name == "true" && fn.Name.Name == "Certify" && base == "linebridge.go":
					case id != nil && id.Name == "bridged" && passOn[fn.Name.Name] && base == "gateway.go":
					default:
						var buf strings.Builder
						_ = printer.Fprint(&buf, fset, arg)
						t.Errorf("%s: %s in %s passes bridged=%s: only module 2's Certify runs the bridged check", pos, sel.Sel.Name, fn.Name.Name, buf.String())
					}
				case "Certify":
					certifyCallers[base+":"+fn.Name.Name]++
				case "egressAdapt":
					adaptCallers[base+":"+fn.Name.Name]++
				case "egressBuild":
					sites++
					buildCallers[base+":"+fn.Name.Name]++
				case "sendLeg":
					lit, ok := call.Args[len(call.Args)-1].(*ast.CompositeLit)
					if !ok {
						t.Errorf("%s: sendLeg is given other than a legSend literal: its fields are read here", pos)
						return true
					}
					build := keyed(lit, "Build")
					if build == nil {
						return true // a sealed request: nothing is built or walked
					}
					b := builds[base]
					b.sites++
					if u, ok := build.(*ast.UnaryExpr); ok {
						if m, ok := u.X.(*ast.CompositeLit); ok {
							if keyed(m, "Contract") != nil {
								b.checked++
							}
							builds[base] = b
							return true
						}
					}
					t.Errorf("%s: sendLeg's Build is other than a moduleBuild literal: its fields are read here", pos)
				}
				return true
			})
			// Every site acts on egressBuild's status: a check's refusal, and the
			// registered transform's (sealTransformed), which any walk that
			// changes bytes can raise. It is never discarded.
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok || len(as.Rhs) != 1 || len(as.Lhs) != 4 {
					return true
				}
				call, ok := as.Rhs[0].(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); !ok || sel.Sel.Name != "egressBuild" || len(call.Args) != 4 {
					return true
				}
				assigned++
				if id, ok := as.Lhs[1].(*ast.Ident); ok && id.Name == "_" {
					t.Errorf("%s: an egressBuild discards its status", fset.Position(as.Pos()))
				}
				return true
			})
		}
	}
	if assigned != sites {
		t.Errorf("%d of %d egressBuild calls assign their four results: a call that drops them acts on nothing it checked", assigned, sites)
	}
	wantCertify := map[string]int{"modulebuild.go:egressBuild": 1, "dtr_answer_bridge.go:carryDTRAnswer": 1}
	if fmt.Sprint(certifyCallers) != fmt.Sprint(wantCertify) {
		t.Errorf("Certify is asked by %v, want %v", certifyCallers, wantCertify)
	}
	if want := map[string]int{"modulebuild.go:egressBuild": 1}; fmt.Sprint(adaptCallers) != fmt.Sprint(want) {
		t.Errorf("egressAdapt is called by %v, want only egressBuild", adaptCallers)
	}
	if want := map[string]int{"legclient.go:sendLeg": 1}; fmt.Sprint(buildCallers) != fmt.Sprint(want) {
		t.Errorf("egressBuild is called by %v, want only the LegClient's sendLeg", buildCallers)
	}
	if fmt.Sprint(builds) != fmt.Sprint(moduleBuildSites) {
		t.Errorf("module 1 build sites (file: {sites checked}) = %v, want %v: a new one must be added to moduleBuildSites deliberately", builds, moduleBuildSites)
	}
	// egressBuild reads bridged-ness from its route, and checks a walked request
	// only through Certify.
	src, err := os.ReadFile("modulebuild.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"if len(route.Chain) > 0 {", "g.bridge().Certify(ctx, m.Contract, line, \"\", out)"} {
		if !strings.Contains(string(src), want) {
			t.Errorf("egressBuild no longer contains %q: a walked request's check must be module 2's Certify, chosen by the route's own chain", want)
		}
	}
}

// payerAnswerIngressSites are the call lines of validateFHIRPayerIngress: the
// ingress check of a payer's answer (DTR package, next-question, PAS
// ClaimResponse) on a leg this gateway originated. Each passes the payer identity
// the member's Coverage named, because whether the check is skipped depends on
// whose answer it is. A new site must be added here deliberately.
var payerAnswerIngressSites = map[string]int{
	"module1_originate.go":            8,
	"module1_originate_homeoxygen.go": 1,
	"module1_dtr_adaptive.go":         1,
	"module1_pas_tail.go":             1,
	"module1_originate_resume.go":     3,
}

// TestPayerAnswerUnskippedCallers pins who may call validatePayerAnswerAtCandidates,
// the payer-answer check without the reference-payer skip: validateFHIRPayerIngress,
// which adds the skip for the legs this gateway originates, and carriedAnswerRefused,
// the provider ingress's check of the answers it carries for its own participant's
// system, which judges every payer's answer. A new caller would judge, or skip, a
// payer's answer by a rule no one chose, so it must be added here deliberately.
func TestPayerAnswerUnskippedCallers(t *testing.T) {
	fset, files := engineFiles(t)
	want := map[string]bool{"gateway.go:validateFHIRPayerIngress": true, "workstream_pa_ingress.go:carriedAnswerRefused": true}
	got := map[string]bool{}
	for name, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "validatePayerAnswerAtCandidates" {
					key := filepath.Base(name) + ":" + fn.Name.Name
					got[key] = true
					if !want[key] {
						t.Errorf("%s: %s calls validatePayerAnswerAtCandidates; only %v may", fset.Position(call.Pos()), key, want)
					}
				}
				return true
			})
		}
	}
	for key := range want {
		if !got[key] {
			t.Errorf("%s no longer calls validatePayerAnswerAtCandidates", key)
		}
	}
}

// TestPayerAnswerIngressNamesTheRoutedPayer pins that every
// validateFHIRPayerIngress call passes, as its payer, the flow's own routed payer
// identity — a variable or field named payer, carried from the member's Coverage —
// never a literal identity or a package-level value such as
// shnsdk.CMSPayerIdentity, so the reference-payer skip is decided by the payer the
// leg was actually routed by.
func TestPayerAnswerIngressNamesTheRoutedPayer(t *testing.T) {
	fset, files := engineFiles(t)
	counts := map[string]int{}
	for name, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "validateFHIRPayerIngress" {
				return true
			}
			counts[filepath.Base(name)]++
			pos := fset.Position(call.Pos())
			if len(call.Args) != 5 {
				t.Errorf("%s: validateFHIRPayerIngress has %d args, want 5", pos, len(call.Args))
				return true
			}
			switch arg := call.Args[4].(type) {
			case *ast.Ident:
				if arg.Name == "payer" {
					return true
				}
			case *ast.SelectorExpr:
				if x, ok := arg.X.(*ast.Ident); ok && arg.Sel.Name == "payer" && x.Name != "shnsdk" {
					return true
				}
			}
			var buf strings.Builder
			_ = printer.Fprint(&buf, fset, call.Args[4])
			t.Errorf("%s: validateFHIRPayerIngress names payer %s, want the flow's routed payer identity (payer, res.payer, st.payer)", pos, buf.String())
			return true
		})
	}
	for name, want := range payerAnswerIngressSites {
		if counts[name] != want {
			t.Errorf("%s has %d payer-answer ingress checks, want %d — a new one must be added to payerAnswerIngressSites deliberately", name, counts[name], want)
		}
	}
	for name, got := range counts {
		if _, listed := payerAnswerIngressSites[name]; !listed {
			t.Errorf("%s has %d payer-answer ingress checks but is not listed", name, got)
		}
	}
}

// keyed is the value of lit's field key, or nil when lit does not set it.
func keyed(lit *ast.CompositeLit, key string) ast.Expr {
	for _, el := range lit.Elts {
		if kv, ok := el.(*ast.KeyValueExpr); ok {
			if k, ok := kv.Key.(*ast.Ident); ok && k.Name == key {
				return kv.Value
			}
		}
	}
	return nil
}
