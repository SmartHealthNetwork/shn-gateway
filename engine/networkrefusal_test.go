package engine

import (
	"context"
	"go/ast"
	"go/token"
	"sort"
	"strconv"
	"testing"
)

// The network rules are the core's: networkRefusal decides them with no policy
// argument, so a network rule refuses whatever level the participant states, or
// none at all. Through the guard, every level refuses a network defect and
// records no finding, and a valid verdict refuses nothing.
func TestNetworkRefusalConsultsNoPolicy(t *testing.T) {
	for _, rule := range []string{RuleSubjectPCI, RuleSubjectToken, RuleDuplicateKey} {
		if !networkRefusal(rule) {
			t.Errorf("%s must be a network refusal", rule)
		}
	}
	for rule := range contentRules {
		if networkRefusal(rule) {
			t.Errorf("content rule %s must not be a network refusal", rule)
		}
	}
	policies := map[string]ConformancePolicy{}
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict} {
		policies[level.String()] = NewConformancePolicy(level)
		policies[level.String()+"+unreadable"] = newConformancePolicy(level, true)
	}
	for name, p := range policies {
		for rule := range networkRules {
			for _, kind := range []CheckKind{KindNetwork, KindContent} {
				for _, v := range []Verdict{VerdictInvalid, VerdictUnavailable, VerdictDeeper} {
					var findings []ConformanceFinding
					if !guardDefect(context.Background(), p, func(f ConformanceFinding) { findings = append(findings, f) }, kind, rule, v, []byte(`{}`)) {
						t.Errorf("%s: %s %s (%v) must refuse", name, kind, rule, v)
					}
					if len(findings) != 0 {
						t.Errorf("%s: %s %s records no finding, got %+v", name, kind, rule, findings)
					}
				}
				if guardDefect(context.Background(), p, nil, kind, rule, VerdictValid, nil) {
					t.Errorf("%s: a valid %s %s must not refuse", name, kind, rule)
				}
			}
		}
	}
}

// SHN's own bridged edit is module 2's: bridgedDecision refuses anything but a
// valid verdict, with no policy argument, so the bridged check refuses at every
// level, none included (TestValidateGovernedBridgedRunsAtNone runs it through
// the choke point).
func TestBridgedDecisionIsModuleTwos(t *testing.T) {
	for _, tc := range []struct {
		v    Verdict
		want Decision
	}{
		{VerdictValid, Record},
		{VerdictInvalid, Refuse},
		{VerdictDeeper, Refuse},
		{VerdictUnavailable, Refuse},
	} {
		if got := bridgedDecision(tc.v); got != tc.want {
			t.Errorf("bridgedDecision(%v) = %v, want %v", tc.v, got, tc.want)
		}
	}
}

// ConformancePolicy is payload conformance alone: no method of it, and no
// package function it reaches, names a network rule or SHN's own bridged edit,
// by identifier or by value. Those refuse outside it, so module 3 can leave the
// core without taking them along.
func TestConformancePolicyNamesNoNetworkRuleOrBridgedEdit(t *testing.T) {
	fset, files := engineFiles(t)
	forbidden := map[string]bool{
		"networkRules": true, "networkRefusal": true, "KindNetwork": true,
		"KindFHIRBridged": true, "bridgedDecision": true,
	}
	forbiddenValues := map[string]bool{string(KindNetwork): true, string(KindFHIRBridged): true}
	for rule := range networkRules {
		forbiddenValues[rule] = true
	}
	// values is each package-level const and var with a string value, aliases
	// resolved, so a network rule or the bridged kind named by any constant is
	// caught as surely as by its literal.
	values, alias := map[string]string{}, map[string]string{}
	funcs := map[string]*ast.FuncDecl{}
	var roots []*ast.FuncDecl
	for _, f := range files {
		for _, d := range f.Decls {
			if gd, ok := d.(*ast.GenDecl); ok && (gd.Tok == token.CONST || gd.Tok == token.VAR) {
				for _, sp := range gd.Specs {
					vs := sp.(*ast.ValueSpec)
					for i, n := range vs.Names {
						if i >= len(vs.Values) {
							continue
						}
						val := vs.Values[i]
						if call, ok := val.(*ast.CallExpr); ok && len(call.Args) == 1 {
							val = call.Args[0]
						}
						switch v := val.(type) {
						case *ast.BasicLit:
							if str, err := strconv.Unquote(v.Value); v.Kind == token.STRING && err == nil {
								values[n.Name] = str
							}
						case *ast.Ident:
							alias[n.Name] = v.Name
						}
					}
				}
			}
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if fn.Recv == nil {
				funcs[fn.Name.Name] = fn
				continue
			}
			typ := fn.Recv.List[0].Type
			if star, ok := typ.(*ast.StarExpr); ok {
				typ = star.X
			}
			if id, ok := typ.(*ast.Ident); ok && id.Name == "ConformancePolicy" {
				roots = append(roots, fn)
			}
		}
	}
	for range alias {
		for name, to := range alias {
			if v, ok := values[to]; ok {
				values[name] = v
			}
		}
	}
	if values["RuleDuplicateKey"] != RuleDuplicateKey || values["KindFHIRBridged"] != string(KindFHIRBridged) {
		t.Fatalf("the fence cannot read the package's constants: %q %q", values["RuleDuplicateKey"], values["KindFHIRBridged"])
	}
	seen := map[string]bool{}
	var walk func(fn *ast.FuncDecl)
	walk = func(fn *ast.FuncDecl) {
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Ident:
				if forbidden[x.Name] || forbiddenValues[values[x.Name]] {
					t.Errorf("%s: %s names %s: network rules and the bridged edit are decided outside ConformancePolicy", fset.Position(x.Pos()), fn.Name.Name, x.Name)
				}
			case *ast.BasicLit:
				if v, err := strconv.Unquote(x.Value); x.Kind == token.STRING && err == nil && forbiddenValues[v] {
					t.Errorf("%s: %s names %q: network rules and the bridged edit are decided outside ConformancePolicy", fset.Position(x.Pos()), fn.Name.Name, v)
				}
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok && funcs[id.Name] != nil && !seen[id.Name] {
					seen[id.Name] = true
					walk(funcs[id.Name])
				}
			}
			return true
		})
	}
	var names []string
	for _, fn := range roots {
		seen[fn.Name.Name] = true
		walk(fn)
	}
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, want := range []string{"Runs", "RunsKind", "Decide", "Level", "decideStructural"} {
		if !seen[want] {
			t.Errorf("the fence did not reach %s (reached %v)", want, names)
		}
	}
}
