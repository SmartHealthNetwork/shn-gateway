//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"go/ast"
	"go/token"
	"go/types"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A CDS Hooks answer that repeats a member name is refused on message
// integrity wherever a CRD answer is certified, at every level and with
// module 3 absent: the payer's own answer on its native responder
// (forwardCRD), a payer's answer on the provider's Da Vinci ingress
// (crdAnswerOutcome), and the answer to a CRD leg the provider's gateway
// originates (OriginateLeg, through the PA workstream's
// originatedAnswerCheck). Each row asserts at the guard: the
// refusal the exchange records is the integrity rule, with no finding, and
// the answer goes no further.
func TestCRDAnswerRepeatedMemberRefusesOnEveryPathAtEveryLevel(t *testing.T) {
	t.Parallel()
	const member = `the JSON member "cards" in the top-level object`
	paths := []struct {
		name string
		run  func(t *testing.T, c conformanceColumn, x *exchangeRecorder) (status int, message string, relayed bool)
	}{
		{"own answer on the native responder", func(t *testing.T, c conformanceColumn, x *exchangeRecorder) (int, string, bool) {
			p := newCDSPayer(t, referencePayerServices...)
			p.respond(http.StatusOK, "application/json", []byte(`{"cards":[],"cards":[]}`))
			opts := []NativeOption{WithConformancePolicy(NewConformancePolicy(c.level))}
			if c.absent {
				opts = append(opts, withoutConformanceModule())
			}
			n := NewNativeResponder(p.srv.Client(), p.srv.URL, nil, opts...)
			ctx := context.WithValue(context.Background(), exchangeRecorderKey{}, x)
			res, err := n.Handle(ctx, "crd-order-select", "corr", "pci", cdsRequest("order-sign"))
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			// The answer exists only once the payer's system has been asked:
			// the one forward happens, and nothing comes back from it.
			if p.sentAny() != 1 {
				t.Fatalf("%d requests reached the payer's system, want 1", p.sentAny())
			}
			return res.Status, res.Message, res.Response.Ownership() != 0
		}},
		{"peer answer on the provider's ingress", func(t *testing.T, c conformanceColumn, x *exchangeRecorder) (int, string, bool) {
			answer := bytes.Replace(realCRDAnswer(t), []byte(`{`), []byte(`{"cards":[],`), 1)
			env := newInProcessExchange(t)
			c.configure(&env.originator.cfg)
			env.payerReturns(LegResult{Response: testResponse(answer)})
			req := httptest.NewRequest(http.MethodPost, "/cds-services/shn-order-select", bytes.NewReader(conformantCRDRequest("MBR-COVERED")))
			req.SetPathValue("id", "shn-order-select")
			req = req.WithContext(context.WithValue(req.Context(), exchangeRecorderKey{}, x))
			rec := httptest.NewRecorder()
			env.originator.handleCRDIngress(rec, req)
			return rec.Code, rec.Body.String(), bytes.Contains(rec.Body.Bytes(), answer)
		}},
		{"answer to an originated CRD leg", func(t *testing.T, c conformanceColumn, x *exchangeRecorder) (int, string, bool) {
			sor, _ := originSystem(t, prefetchMember)
			_, _, order := originRecords(prefetchMember)
			var answer []byte
			g, payer := originGateway(t, "provider-data", sor, nil, func(string, []byte) (int, []byte) {
				answer = []byte(`{"cards":[],` + strings.TrimPrefix(string(payerAnswerFor(t, order, shnsdk.CoveredCovered, shnsdk.PANeededNoAuth, "")), "{"))
				return 0, answer
			})
			c.configure(&g.cfg)
			req := httptest.NewRequest(http.MethodPost, "/scenario/uc02", strings.NewReader(testRouteBody(g, "/scenario/uc02", "")))
			req = req.WithContext(context.WithValue(req.Context(), exchangeRecorderKey{}, x))
			rec := httptest.NewRecorder()
			g.originateNoPACRD(rec, req, prefetchMember)
			if len(payer.sent("crd-order-select")) != 1 {
				t.Fatal("the originated CRD leg was not sent")
			}
			return rec.Code, rec.Body.String(), answer != nil && bytes.Contains(rec.Body.Bytes(), answer)
		}},
	}
	for _, path := range paths {
		for _, c := range everyLevelAndAbsent {
			t.Run(path.name+"/"+c.String(), func(t *testing.T) {
				x := &exchangeRecorder{}
				status, message, relayed := path.run(t, c, x)
				if status != http.StatusBadGateway {
					t.Fatalf("status %d, want 502: %s", status, message)
				}
				// The ingress and the scenario answer in JSON, which escapes
				// the member's quotes.
				if !strings.Contains(message, member) && !strings.Contains(message, strings.ReplaceAll(member, `"`, `\"`)) {
					t.Fatalf("the refusal does not name %s: %s", member, message)
				}
				if relayed {
					t.Fatal("the refused answer went on")
				}
				if x.guardRule != RefusalIntegrity {
					t.Fatalf("the guard recorded refusal rule %q, want %q", x.guardRule, RefusalIntegrity)
				}
				if x.rec.Findings.Count != 0 {
					t.Fatalf("a network refusal recorded %d findings", x.rec.Findings.Count)
				}
			})
		}
	}
}

// conformanceColumn is one column of a row that must come out the same
// whatever the participant chose about payload conformance: a level, or
// module 3 absent (configured strict, so the opt-out is what a column
// measures, not the level).
type conformanceColumn struct {
	level  ConformanceEnforcement
	absent bool
}

// everyLevelAndAbsent is each level, then module 3 absent.
var everyLevelAndAbsent = append(func() (cs []conformanceColumn) {
	for _, l := range allLevels {
		cs = append(cs, conformanceColumn{level: l})
	}
	return cs
}(), conformanceColumn{level: EnforcementStrict, absent: true})

func (c conformanceColumn) String() string {
	if c.absent {
		return "module-absent"
	}
	return c.level.String()
}

// configure sets cfg to the column.
func (c conformanceColumn) configure(cfg *Config) {
	cfg.ConformanceEnforcement = c.level
	if c.absent {
		disableConformanceModule(cfg)
	}
}

// crdAnswerCertifiers are the only functions that reach certifyCDSHooksAnswer,
// so the duplicate-key refusal it makes first fires on exactly the three
// paths above: a fourth caller, or a caller gone, is a change to where a
// network rule refuses and fails this list.
var crdAnswerCertifiers = []string{"Gateway.crdAnswerOutcome", "nativeResponder.forwardCRD", "paWorkstream.originatedAnswerCheck"}

// certifyCDSHooksAnswerReferrers names, by resolved object rather than by
// name, every function whose body refers to certifyCDSHooksAnswer (a call or
// a function value), and "var" for a package-level reference.
func certifyCDSHooksAnswerReferrers(t *testing.T, files []*ast.File, info *types.Info) []string {
	t.Helper()
	var target types.Object
	for _, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "certifyCDSHooksAnswer" {
				target = info.Defs[fd.Name]
			}
		}
	}
	if target == nil {
		t.Fatal("certifyCDSHooksAnswer is not declared")
	}
	var got []string
	add := func(name string) {
		if !slices.Contains(got, name) {
			got = append(got, name)
		}
	}
	for _, f := range files {
		for _, d := range f.Decls {
			name := "var"
			if fd, ok := d.(*ast.FuncDecl); ok {
				name = funcKey(fd)
			}
			ast.Inspect(d, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && info.Uses[id] == target {
					add(name)
				}
				return true
			})
		}
	}
	slices.Sort(got)
	return got
}

func TestCRDAnswerCertifiersAreTheThreePaths(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	files := parsePackageSources(t, fset, ".", nil)
	if got := certifyCDSHooksAnswerReferrers(t, files, typedPackage(t, fset, files)); !slices.Equal(got, crdAnswerCertifiers) {
		t.Fatalf("certifyCDSHooksAnswer is reached from %q, want exactly %q", got, crdAnswerCertifiers)
	}
}

// TestCRDAnswerCertifiersCheckFires adds a fourth referrer to the real package,
// as a call and as a function value, and requires the check to name it.
func TestCRDAnswerCertifiersCheckFires(t *testing.T) {
	for _, row := range []struct{ name, src, want string }{
		{"a fourth function value", "func probeFourthCertifier() { check := certifyCDSHooksAnswer; _ = check }", "probeFourthCertifier"},
		{"a package-level reference", "var probeCertifier = certifyCDSHooksAnswer", "var"},
		{"a method", "type probeHost struct{}\n\nfunc (probeHost) certify() legAnswerCheck { return certifyCDSHooksAnswer }", "probeHost.certify"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			fset := token.NewFileSet()
			files := parsePackageSources(t, fset, ".", map[string]string{"probe_extra.go": "package engine\n\n" + row.src + "\n"})
			got := certifyCDSHooksAnswerReferrers(t, files, typedPackage(t, fset, files))
			if !slices.Contains(got, row.want) || slices.Equal(got, crdAnswerCertifiers) {
				t.Fatalf("want %q named among %q", row.want, got)
			}
		})
	}
}
