//go:build !shn_coreonly

package engine

import (
	"context"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// conformancePolicyReaders are the only files that read a ConformancePolicy
// other than through conformanceModule: the policy's own file, which is module
// 3's table, and the native responder's options, which hold the policy the
// app gives it.
var conformancePolicyReaders = []string{"conformance.go", "native_conformance.go"}

// conformanceModuleBypasses names each use of the level that goes around
// module 3 outside conformancePolicyReaders: a method of ConformancePolicy
// called on the concrete policy, the native responder's policy field read,
// the gateway's configured level (Config.ConformanceEnforcement) read, a
// policy built (by either constructor), or the ConformancePolicy type named
// at all (a literal, a variable, an embedding), the responder's field
// declaration aside. It decides by resolved object, never by name.
func conformanceModuleBypasses(t *testing.T, fset *token.FileSet, files []*ast.File, info *types.Info) []string {
	t.Helper()
	var policy *types.TypeName
	var field, configLevel *types.Var
	var fieldType ast.Expr
	builds := map[types.Object]bool{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.TypeSpec:
				switch n.Name.Name {
				case "ConformancePolicy":
					policy, _ = info.Defs[n.Name].(*types.TypeName)
				case "Config":
					st, _ := n.Type.(*ast.StructType)
					for _, fl := range st.Fields.List {
						for _, name := range fl.Names {
							if name.Name == "ConformanceEnforcement" {
								configLevel, _ = info.Defs[name].(*types.Var)
							}
						}
					}
				case "nativeResponder":
					st, _ := n.Type.(*ast.StructType)
					for _, fl := range st.Fields.List {
						for _, name := range fl.Names {
							if name.Name == "conformance" {
								field, _ = info.Defs[name].(*types.Var)
								// Exempt only a line declaring that one field.
								if len(fl.Names) == 1 {
									fieldType = fl.Type
								}
							}
						}
					}
				}
			case *ast.FuncDecl:
				if n.Recv == nil && (n.Name.Name == "NewConformancePolicy" || n.Name.Name == "newConformancePolicy") {
					builds[info.Defs[n.Name]] = true
				}
			}
			return true
		})
	}
	if policy == nil || field == nil || configLevel == nil || len(builds) != 2 {
		t.Fatalf("ConformancePolicy %v, nativeResponder.conformance %v, Config.ConformanceEnforcement %v, constructors %d: one is not declared", policy, field, configLevel, len(builds))
	}
	isPolicy := func(typ types.Type) bool {
		typ = types.Unalias(typ)
		if p, ok := typ.(*types.Pointer); ok {
			typ = types.Unalias(p.Elem())
		}
		n, ok := typ.(*types.Named)
		return ok && n.Obj() == policy
	}
	var got []string
	for _, f := range files {
		file := filepath.Base(fset.Position(f.Pos()).Filename)
		if slices.Contains(conformancePolicyReaders, file) {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				sel := info.Selections[n]
				switch {
				case sel == nil:
				case sel.Kind() != types.FieldVal && isPolicy(sel.Recv()):
					got = append(got, fmt.Sprintf("%s calls ConformancePolicy.%s on the policy itself", file, n.Sel.Name))
				case sel.Kind() == types.FieldVal && sel.Obj() == field:
					got = append(got, file+" reads nativeResponder.conformance")
				case sel.Kind() == types.FieldVal && sel.Obj() == configLevel:
					got = append(got, file+" reads Config.ConformanceEnforcement")
				}
			case *ast.Ident:
				switch obj := info.Uses[n]; {
				case builds[obj]:
					got = append(got, file+" builds a ConformancePolicy")
				case obj == policy && ast.Expr(n) != fieldType:
					got = append(got, file+" names ConformancePolicy")
				}
			}
			return true
		})
	}
	return got
}

// Every payload conformance check reaches the level through module 3
// (conformanceModule): the gateway's conformance, the native responder's
// module. None reads a ConformancePolicy around it.
func TestPayloadChecksAskModuleThree(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	files := parsePackageSources(t, fset, ".", nil)
	if got := conformanceModuleBypasses(t, fset, files, typedPackage(t, fset, files)); len(got) != 0 {
		t.Fatalf("the level is read around module 3:\n%q", got)
	}
}

// TestPayloadChecksAskModuleThreeCheckFires adds a bypass to the real package
// and requires the check to name it; a call through the module is clean.
func TestPayloadChecksAskModuleThreeCheckFires(t *testing.T) {
	for _, row := range []struct{ name, src, want string }{
		{"a policy method on the policy", "func probeRuns(p ConformancePolicy) bool { return p.Runs(KindContent, RulePatientMixed) }", "probe_extra.go calls ConformancePolicy.Runs on the policy itself"},
		{"a policy method value", "func probeDecide(p ConformancePolicy) any { return p.Decide }", "probe_extra.go calls ConformancePolicy.Decide on the policy itself"},
		{"the responder's policy read", "func probeLevel(n *nativeResponder) ConformanceEnforcement { return n.conformance.Level() }", "probe_extra.go reads nativeResponder.conformance"},
		{"a policy built", "func probeBuild() conformanceModule { return NewConformancePolicy(EnforcementStrict) }", "probe_extra.go builds a ConformancePolicy"},
		{"a policy built unexported", "func probeBuildTable() conformanceModule { return newConformancePolicy(EnforcementStrict, false) }", "probe_extra.go builds a ConformancePolicy"},
		{"a policy literal", "func probeLiteral() conformanceModule { return ConformancePolicy{} }", "probe_extra.go names ConformancePolicy"},
		{"a policy variable", "func probeVar() bool { var p ConformancePolicy; return guardDefect(nil, p, nil, KindContent, RulePatientMixed, VerdictInvalid, nil) }", "probe_extra.go names ConformancePolicy"},
		{"a policy embedded", "type probeEmbed struct{ ConformancePolicy }\n\nfunc probeEmbedded(e probeEmbed) bool { return e.Runs(KindContent, RulePatientMixed) }", "probe_extra.go names ConformancePolicy"},
		{"the configured level read", "func probeConfigured(g *Gateway) bool { return payloadChecksConsumeLanes(g.cfg.ConformanceEnforcement) }", "probe_extra.go reads Config.ConformanceEnforcement"},
		{"through the module", "func probeModule(m conformanceModule) bool { return m.Runs(KindContent, RulePatientMixed) }", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			fset := token.NewFileSet()
			files := parsePackageSources(t, fset, ".", map[string]string{"probe_extra.go": "package engine\n\n" + row.src + "\n"})
			got := conformanceModuleBypasses(t, fset, files, typedPackage(t, fset, files))
			if row.want == "" {
				if len(got) != 0 {
					t.Fatalf("a call through the module was named: %q", got)
				}
				return
			}
			if !slices.Contains(got, row.want) {
				t.Fatalf("want %q, got %q", row.want, got)
			}
		})
	}
}

// disableConformanceModule and withoutConformanceModule are this package's
// own module-absent opt-outs: the exported ones exist only in a test build
// (conformance_testdouble.go), and the engine's rows run untagged.
func disableConformanceModule(c *Config) { c.conformanceModuleAbsent = true }

func withoutConformanceModule() NativeOption {
	return func(n *nativeResponder) { n.moduleAbsent = true }
}

// The opt-outs are not in a released gateway's API: an untagged build of
// this package does not compile against them, and a shn_testdouble build
// does.
func TestConformanceModuleOptOutsOnlyInATestBuild(t *testing.T) {
	t.Parallel()
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal("go tool not available: the build check cannot run")
	}
	probe := filepath.Join(t.TempDir(), "probe.go")
	if err := os.WriteFile(probe, []byte("package engine\n\nvar _, _ = DisableConformanceModuleForTest, WithoutConformanceModuleForTest\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(t.TempDir(), "overlay.json")
	if err := os.WriteFile(overlay, []byte(`{"Replace":{"zz_optout_probe.go":"`+probe+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name     string
		tags     []string
		compiles bool
	}{
		{"untagged build", nil, false},
		{"shn_testdouble build", []string{"-tags=shn_testdouble"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			args := append(append([]string{"build", "-overlay=" + overlay}, c.tags...), "-o", os.DevNull, ".")
			out, err := exec.Command(gobin, args...).CombinedOutput()
			if (err == nil) != c.compiles {
				t.Fatalf("compiles = %v, want %v:\n%s", err == nil, c.compiles, out)
			}
			if !c.compiles && !strings.Contains(string(out), "undefined: DisableConformanceModuleForTest") {
				t.Fatalf("failed for another reason than the missing opt-out:\n%s", out)
			}
		})
	}
}

// Module 3 absent answers exactly as the module at none, for every kind, rule
// and verdict: no check runs, no defect refuses, and a guard site's content
// decision records nothing. It is so by construction (absentModule embeds the
// module at none); this row holds the construction to it.
func TestAbsentModuleIsTheModuleAtNone(t *testing.T) {
	none := NewConformancePolicy(EnforcementNone)
	var absent conformanceModule = newAbsentModule()
	if absent.Level() != none.Level() {
		t.Fatalf("absent is at %s, none at %s", absent.Level(), none.Level())
	}
	rules := []string{"", "response.json", "response.object", "line", "card.indicator", "card.summary.length", RuleDuplicateKey}
	for rule := range contentRules {
		rules = append(rules, rule)
	}
	for _, kind := range []CheckKind{KindFHIRIngress, KindFHIREgress, KindFHIRBridged, KindCDSEnvelope, KindNetwork, KindContent} {
		if absent.RunsKind(kind) != none.RunsKind(kind) {
			t.Errorf("RunsKind(%s): absent %v, none %v", kind, absent.RunsKind(kind), none.RunsKind(kind))
		}
		for _, rule := range rules {
			if absent.Runs(kind, rule) != none.Runs(kind, rule) {
				t.Errorf("Runs(%s, %q): absent %v, none %v", kind, rule, absent.Runs(kind, rule), none.Runs(kind, rule))
			}
			for _, v := range []Verdict{VerdictValid, VerdictInvalid, VerdictDeeper, VerdictUnavailable} {
				if absent.Decide(kind, rule, v) != none.Decide(kind, rule, v) {
					t.Errorf("Decide(%s, %q, %v): absent %s, none %s", kind, rule, v, absent.Decide(kind, rule, v), none.Decide(kind, rule, v))
				}
				var emitted []ConformanceFinding
				emit := func(f ConformanceFinding) { emitted = append(emitted, f) }
				xa, xn := &exchangeRecorder{}, &exchangeRecorder{}
				ra := absent.content(context.WithValue(context.Background(), exchangeRecorderKey{}, xa), emit, kind, rule, v, []byte("{}"))
				rn := none.content(context.WithValue(context.Background(), exchangeRecorderKey{}, xn), emit, kind, rule, v, []byte("{}"))
				if ra != rn || len(emitted) != 0 || xa.rec.Findings.Count != xn.rec.Findings.Count || xa.guardRule != xn.guardRule {
					t.Errorf("content(%s, %q, %v): absent %v, none %v, findings %d, recorded %d/%d, refusal %q/%q", kind, rule, v, ra, rn, len(emitted), xa.rec.Findings.Count, xn.rec.Findings.Count, xa.guardRule, xn.guardRule)
				}
			}
		}
	}
}

// With module 3 absent, the lane decisions are none's at every configured
// level: a declared line is served without a lane, and a certified builder
// builds at it.
func TestAbsentModuleDecidesLanesAsNone(t *testing.T) {
	gateway := func(level ConformanceEnforcement, absent bool) *Gateway {
		cfg := Config{ConformanceEnforcement: level, DeclaredContractVersions: []string{"pa.pas@2.0", "pa.pas@2.2"},
			ValidatorsByContractLine: contractLanes(map[string]shnsdk.Validator{"2.0": shnsdk.NewFakeValidator()})}
		if absent {
			disableConformanceModule(&cfg)
		}
		return &Gateway{cfg: cfg}
	}
	none := gateway(EnforcementNone, false)
	for _, level := range allLevels {
		absent := gateway(level, true)
		if got, want := absent.servesLine("pa.pas", "2.2"), none.servesLine("pa.pas", "2.2"); got != want || !got {
			t.Errorf("absent at %s: servesLine(pa.pas, 2.2) = %v, none %v", level, got, want)
		}
		if got, want := absent.buildsAt(relay.BuilderSDKPASInquiry, "pa.pas", "2.2"), none.buildsAt(relay.BuilderSDKPASInquiry, "pa.pas", "2.2"); got != want || !got {
			t.Errorf("absent at %s: buildsAt(inquiry, pa.pas, 2.2) = %v, none %v", level, got, want)
		}
	}
}

// The gateway's module 3 is the configured level, and a gateway built with no
// level is strict: the zero Config never means no module. Only the explicit
// opt-out leaves it absent, whatever the level, and an absent module starts
// no certification.
func TestGatewayConformanceIsTheConfiguredLevel(t *testing.T) {
	for _, level := range allLevels {
		cfg := Config{ConformanceEnforcement: level}
		disableConformanceModule(&cfg)
		g := &Gateway{cfg: cfg}
		if _, ok := g.conformance().conformanceModule.(absentModule); !ok {
			t.Errorf("opted out at %s: module %T, want absent", level, g.conformance().conformanceModule)
		}
		g.startCertification()
		if g.certification != nil {
			t.Errorf("opted out at %s: certification started", level)
		}
		if _, ok := NewNativeResponder(nil, "", nil, WithConformancePolicy(NewConformancePolicy(level)), withoutConformanceModule()).module().(absentModule); !ok {
			t.Errorf("responder opted out at %s: module is not absent", level)
		}
	}
	for _, level := range allLevels {
		if got := (&Gateway{cfg: Config{ConformanceEnforcement: level}}).conformance().Level(); got != level {
			t.Errorf("configured %s, module at %s", level, got)
		}
	}
	if got := (&Gateway{}).conformance().Level(); got != EnforcementStrict {
		t.Errorf("zero Config: module at %s, want strict", got)
	}
	if got := NewNativeResponder(nil, "", nil).module().Level(); got != EnforcementStrict {
		t.Errorf("responder with no policy: module at %s, want strict", got)
	}
}
