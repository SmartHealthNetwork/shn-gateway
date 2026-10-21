package engine

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// buildsAt: a laned line is buildable by every builder at every level; an
// unlaned one only where no payload check consumes lanes, and only for a
// builder certified at that line. A participant's own carried request
// (carriedRequest) and an uncertified builder always need the lane.
func TestBuildsAt(t *testing.T) {
	laned := &Gateway{cfg: Config{ValidatorsByContractLine: contractLanes(map[string]shnsdk.Validator{"2.2": shnsdk.NewFakeValidator()})}}
	unlaned := &Gateway{cfg: Config{ValidatorsByContractLine: contractLanes(map[string]shnsdk.Validator{"2.0": shnsdk.NewFakeValidator()})}}
	uncertified := []relay.BuilderID{carriedRequest, relay.BuilderSDKPASSubmit, relay.BuilderSDKPASUpdate, relay.BuilderSDKCRDRequest, relay.BuilderSDKDTRPackage, relay.BuilderDTRNextQuestion}
	for _, level := range allLevels {
		laned.cfg.ConformanceEnforcement, unlaned.cfg.ConformanceEnforcement = level, level
		for _, b := range append([]relay.BuilderID{relay.BuilderSDKPASInquiry}, uncertified...) {
			if !laned.buildsAt(b, "pa.pas", "2.2") {
				t.Errorf("%s: %q must build at a laned line", level, b)
			}
		}
		for _, line := range []string{"2.1", "2.2"} {
			if got, want := unlaned.buildsAt(relay.BuilderSDKPASInquiry, "pa.pas", line), !payloadChecksConsumeLanes(level); got != want {
				t.Errorf("%s: the certified inquiry at unlaned %s = %v, want %v", level, line, got, want)
			}
			for _, b := range uncertified {
				if unlaned.buildsAt(b, "pa.pas", line) {
					t.Errorf("%s: uncertified %q built at unlaned %s", level, b, line)
				}
			}
		}
		if unlaned.buildsAt(relay.BuilderSDKPASInquiry, "pa.dtr", "2.2") {
			t.Errorf("%s: a builder certified on pa.pas was taken as certified on pa.dtr", level)
		}
	}
}

// Fresh selection's arm 2 reads the same predicate. With the payer declaring
// only pa.pas@2.2 and no 2.2 lane, at none the certified inquiry is built
// natively at 2.2, while every uncertified builder and a carried request are
// carried at this gateway's own 2.0; at every level that checks, all are
// carried.
func TestNativeReachBuildsUnlanedOnlyForACertifiedBuilderAtNone(t *testing.T) {
	for _, level := range allLevels {
		for _, b := range []relay.BuilderID{relay.BuilderSDKPASInquiry, relay.BuilderSDKPASSubmit, carriedRequest} {
			g := &Gateway{cfg: Config{
				DeclaredContractVersions: []string{shnsdk.ContractPAPAS20},
				ValidatorsByContractLine: contractLanes(map[string]shnsdk.Validator{"2.0": shnsdk.NewFakeValidator()}),
				ConformanceEnforcement:   level,
				Reg:                      resumeRegistry("pa.pas@2.2"),
			}}
			route, err := g.selectLegRoute("payer", "pas-claim", b)
			want := shnsdk.ContractPAPAS20
			if b == relay.BuilderSDKPASInquiry && !payloadChecksConsumeLanes(level) {
				want = "pa.pas@2.2"
			}
			if err != nil || route.Token != want || route.BuildLine != shnsdk.LineOf(want) || route.Chain != nil {
				t.Errorf("%s %q: %+v, %v, want built at %s", level, b, route, err, want)
			}
		}
	}
}

// Every certified pair names the make validate test that certifies it
// (test/conformance checks that its line's run list selects it), a line its
// contract builds natively, and a builder the ownership table lets this
// gateway originate on one of that contract's legs.
func TestCertifiedBuildsNameTheirTest(t *testing.T) {
	if len(certifiedBuilds) == 0 {
		t.Fatal("certifiedBuilds is empty: the PAS inquiry is certified at every line")
	}
	for pair, test := range certifiedBuilds {
		if test == "" || pair.Builder == carriedRequest {
			t.Errorf("%+v: a certified pair names its builder and its test", pair)
		}
		if !slices.Contains(nativeLinesForContract(pair.Contract), pair.Line) {
			t.Errorf("%+v: %s does not build line %s natively", pair, pair.Contract, pair.Line)
		}
		originates := false
		for leg := range originatedBuilders() {
			if c, _ := legContract(leg); c == pair.Contract && slices.Contains(originatedBuilders()[leg], pair.Builder) {
				originates = true
			}
		}
		if !originates {
			t.Errorf("%+v: no %s leg is originated by %s", pair, pair.Contract, pair.Builder)
		}
	}
}

// originatedBuilders is each leg's builders a requester may originate it with,
// from every workstream's ownership table.
func originatedBuilders() map[string][]relay.BuilderID {
	out := map[string][]relay.BuilderID{}
	for _, w := range workstreams {
		for k, rule := range w.ownershipRows() {
			if k.Role == relay.RoleRequester && k.Direction == relay.DirectionRequest && k.Outcome == relay.OutcomeOriginated {
				out[k.Leg] = append(out[k.Leg], rule.Builders...)
			}
		}
	}
	return out
}

// Every production line selection names its builder: a module 1 site names a
// builder the ownership table lets it originate that leg with (so a certified
// builder cannot stand in for the one the site builds with), a wrapper passes
// its own builder argument on, and native ingress, which carries the
// participant's own request, names carriedRequest, which nothing else names. A
// site that named the wrong builder could build an uncertified message at a
// line it has no lane for; native ingress naming a builder would build-reach on
// bytes it did not build.
func TestEveryLineSelectionNamesItsBuilder(t *testing.T) {
	const nativeIngress = "workstream_pa_ingress.go"
	fset, files := engineFiles(t)
	// leg is the position of each selection's leg-type argument (-1: it takes a
	// contract, and only the wrappers call it).
	leg := map[string]int{
		"selectLegLine": 1, "selectLegLineOrFail": 2, "selectLegLineOrBridgeRefuse": 2, "selectLegRoute": 1,
		"selectResumeRoute": 2, "routeFor": 0, "reachPeerLine": -1, "resumeReach": -1, "selectNativeReachRoute": -1,
	}
	consts := relayBuilderConsts(t)
	allowed := originatedBuilders()
	takes := map[string]int{
		"selectLegLine": 3, "selectLegLineOrFail": 4, "selectLegLineOrBridgeRefuse": 4, "selectLegRoute": 2,
		"selectResumeRoute": 3, "routeFor": 2, "reachPeerLine": 4, "resumeReach": 5, "selectNativeReachRoute": 2,
	}
	calls := 0
	for name, f := range files {
		native := filepath.Base(name) == nativeIngress
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			at, ok := takes[sel.Sel.Name]
			if !ok {
				return true
			}
			calls++
			if len(call.Args) != at+1 {
				t.Errorf("%s: %s has %d args, want %d", fset.Position(call.Pos()), sel.Sel.Name, len(call.Args), at+1)
				return true
			}
			switch a := call.Args[at].(type) {
			case *ast.SelectorExpr:
				if id, ok := a.X.(*ast.Ident); ok && id.Name == "relay" && !native {
					b, known := consts[a.Sel.Name]
					lit, isLit := call.Args[max(leg[sel.Sel.Name], 0)].(*ast.BasicLit)
					if !known || leg[sel.Sel.Name] < 0 || !isLit {
						t.Errorf("%s: %s names relay.%s where its leg cannot be read", fset.Position(call.Pos()), sel.Sel.Name, a.Sel.Name)
						return true
					}
					legType, _ := strconv.Unquote(lit.Value)
					if !slices.Contains(allowed[legType], b) {
						t.Errorf("%s: %s names relay.%s for %s, which the ownership table does not let it originate (%v)", fset.Position(call.Pos()), sel.Sel.Name, a.Sel.Name, legType, allowed[legType])
					}
					return true
				}
			case *ast.Ident:
				if a.Name == "builder" || a.Name == "carriedRequest" && native {
					return true
				}
			}
			t.Errorf("%s: %s is not given its builder: a relay.Builder* at a site that builds, the wrapper's builder, or carriedRequest on native ingress (%s) alone", fset.Position(call.Pos()), sel.Sel.Name, nativeIngress)
			return true
		})
	}
	if calls < 20 {
		t.Fatalf("found %d line selections, want at least 20: the fence read nothing", calls)
	}
}

// relayBuilderConsts is each relay.Builder* constant's value, read from the
// relay package's source.
func relayBuilderConsts(t *testing.T) map[string]relay.BuilderID {
	t.Helper()
	_, files := engineFiles(t, "relay")
	out := map[string]relay.BuilderID{}
	for _, f := range files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, sp := range gd.Specs {
				vs := sp.(*ast.ValueSpec)
				for i, n := range vs.Names {
					if i < len(vs.Values) {
						if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							v, _ := strconv.Unquote(lit.Value)
							out[n.Name] = relay.BuilderID(v)
						}
					}
				}
			}
		}
	}
	if out["BuilderSDKPASInquiry"] != relay.BuilderSDKPASInquiry {
		t.Fatalf("could not read the relay builder constants: %v", out)
	}
	return out
}
