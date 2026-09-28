package engine

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// originationProfileConfig is a Config New accepts for role, apart from its
// OriginationProfile: the refusal rows below then fail only on that field.
func originationProfileConfig(t *testing.T, role, profile string) Config {
	t.Helper()
	hubPub, _ := genED25519(t)
	_, signPriv := genED25519(t)
	sor := newCensusSoR()
	cfg := Config{
		Role:               role,
		HolderID:           role,
		Identity:           shnsdk.Identity{HolderID: role, SignPriv: signPriv},
		HubTransportPub:    hubPub,
		SoR:                sor,
		Store:              sor,
		OriginationProfile: profile,
	}
	if role == "payer" {
		cfg.Responder = unusedResponder{}
	}
	return cfg
}

var originationRoles = []string{"provider", "payer", "facility", "phg"}

// An OriginationProfile that names no lane is refused by New on every role, whatever
// the case or whitespace: the origination paths compare it exactly, so it would reach
// none of them and build a PAS request with a payer Organization no system of record
// supplied.
func TestNew_RefusesUnknownOriginationProfile(t *testing.T) {
	unknown := []string{
		"relay-only", "unknown-lane", "payer-data", "provider_data", "provider",
		"Demo", "DEMO", "Provider-Data", " demo", "demo ", "\tprovider-data\n", " ",
	}
	for _, role := range originationRoles {
		for _, v := range unknown {
			t.Run(role+"/"+v, func(t *testing.T) {
				g, err := New(originationProfileConfig(t, role, v))
				if g != nil {
					t.Cleanup(func() { _ = g.Close() })
				}
				want := fmt.Sprintf("gateway: Config.OriginationProfile %q is not an origination profile (must be demo|provider-data)", v)
				if err == nil || err.Error() != want {
					t.Fatalf("OriginationProfile=%q on role %s: err = %v, want %q", v, role, err, want)
				}
			})
		}
	}
}

// Every accepted OriginationProfile builds on every role, and so does unset: the
// published binary sets demo for an unset provider before New sees it, and the other
// roles originate nothing.
func TestNew_AcceptsOriginationProfiles(t *testing.T) {
	for _, role := range originationRoles {
		for _, v := range append(OriginationProfiles(), "") {
			t.Run(role+"/"+v, func(t *testing.T) {
				mustNew(t, originationProfileConfig(t, role, v))
			})
		}
	}
}

// The accepted list is exactly the lanes, and every lane builds its PAS request with
// the payer Organization as a bundle entry (PayerOrgEntry), never the contained one a
// payer's system did not supply.
func TestOriginationProfiles_AreTheLanes(t *testing.T) {
	got := OriginationProfiles()
	if fmt.Sprint(got) != "[demo provider-data]" {
		t.Fatalf("OriginationProfiles() = %q, want [demo provider-data]", got)
	}
	for _, p := range got {
		if !isOriginationProfile(p) || !relaysReferencePayerBytes(p) {
			t.Fatalf("%q: isOriginationProfile=%v relaysReferencePayerBytes=%v, want both true", p, isOriginationProfile(p), relaysReferencePayerBytes(p))
		}
	}
	got[0] = "mutated"
	if OriginationProfiles()[0] != "demo" {
		t.Fatal("OriginationProfiles must return a copy")
	}
}

// originatingRoutes are the provider routes that build and send a request of this
// gateway's own. TestScenarioRoutes_EveryRouteIsClassified keeps this list and
// Handler's wiring the same.
var originatingRoutes = []string{
	"POST /scenario/uc01",
	"POST /scenario/uc02",
	"POST /scenario/uc02-payerb",
	"POST /scenario/uc02-unknownpayer",
	"POST /scenario/uc03",
	"POST /scenario/uc04",
	"POST /scenario/uc05",
	"POST /scenario/uc06",
	"POST /scenario/uc07",
	"POST /scenario/uc07hcpcs",
	"POST /scenario/uc08",
	"POST /scenario/homeoxygen",
	"POST /scenario/dispatch",
	"POST /scenario/uc06/start",
	"POST /scenario/uc06/complete",
	"POST /scenario/uc07/start",
	"POST /scenario/uc07/complete",
	"POST /scenario/pa/inquire",
}

// localScenarioRoutes only read or drop this gateway's own pended state; they send
// nothing, so they serve whatever the origination profile.
var localScenarioRoutes = []string{
	"POST /scenario/uc06/cancel",
	"POST /scenario/uc07/cancel",
	"GET /scenario/uc07/pending",
	"POST /scenario/reset",
}

// countingTransport counts every outbound request before handing it on.
type countingTransport struct {
	inner http.RoundTripper
	n     int
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.n++
	return c.inner.RoundTrip(req)
}

// unsetProfileProvider is crdTestSystem's provider with no origination profile, every
// outbound request counted and every observer event recorded.
func unsetProfileProvider(t *testing.T, ingress bool) (*Gateway, *stubSubstrate, *countingTransport, *[]ObserverEvent) {
	t.Helper()
	gw, stub, _ := crdTestSystem(t, shnsdk.CardCoverage{Covered: shnsdk.CoveredCovered, PANeeded: shnsdk.PANeededAuthNeeded, Questionnaires: []string{"http://example.org/q"}})
	cfg := gw.cfg
	cfg.OriginationProfile = ""
	cfg.Populator = nil
	counter := &countingTransport{inner: stub}
	cfg.Client = &http.Client{Transport: counter}
	var events []ObserverEvent
	cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
	if ingress {
		EnableIngressForTest(&cfg)
	}
	return mustNew(t, cfg), stub, counter, &events
}

func serve(g *Gateway, route, body string) *httptest.ResponseRecorder {
	method, path, _ := strings.Cut(route, " ")
	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

// On a provider with no origination profile every originating route refuses before it
// reads, builds or sends anything: 503, uncacheable, the named error. No request leaves
// the gateway (no authorization, Hub or payer call) and nothing is read from the system
// of record.
func TestOriginatingRoutes_RefuseWithoutProfile(t *testing.T) {
	wantBody := `{"error":"origination profile not set: set ORIGINATION_PROFILE (demo|provider-data)"}`
	for _, route := range originatingRoutes {
		t.Run(route, func(t *testing.T) {
			g, stub, counter, events := unsetProfileProvider(t, false)
			rec := serve(g, route, `{"continuation":"c1","resumeToken":"r1","answers":{}}`)
			if rec.Code != http.StatusServiceUnavailable || strings.TrimSpace(rec.Body.String()) != wantBody {
				t.Fatalf("%s = %d %s, want 503 %s", route, rec.Code, rec.Body.String(), wantBody)
			}
			if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", cc)
			}
			if counter.n != 0 || len(stub.legTypes) != 0 {
				t.Fatalf("%d outbound requests, legs %v; want none", counter.n, stub.legTypes)
			}
			if len(*events) != 0 {
				t.Fatalf("events = %+v, want none (nothing read, built or sent)", *events)
			}
		})
	}
}

// The routes that only touch this gateway's own pended state serve as before.
func TestLocalScenarioRoutes_ServeWithoutProfile(t *testing.T) {
	for _, route := range localScenarioRoutes {
		t.Run(route, func(t *testing.T) {
			g, _, counter, _ := unsetProfileProvider(t, false)
			rec := serve(g, route, `{"resumeToken":"absent"}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s = %d %s, want 200", route, rec.Code, rec.Body.String())
			}
			if counter.n != 0 {
				t.Fatalf("%s sent %d requests, want none", route, counter.n)
			}
		})
	}
}

// A provider with no origination profile still serves its ingress: a Da Vinci request
// from the participant's own system is relayed to the payer through the Hub, and the
// ingress discovery document is served.
func TestIngress_RelaysWithoutProfile(t *testing.T) {
	g, stub, counter, _ := unsetProfileProvider(t, true)
	rec := serve(g, "POST /cds-services/shn-order-select", string(routableCRDReqJSON()))
	if rec.Code != http.StatusOK {
		t.Fatalf("CRD ingress = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if counter.n == 0 || !legAttempted(stub.legTypes, "crd-order-select") {
		t.Fatalf("CRD ingress relayed nothing: %d requests, legs %v", counter.n, stub.legTypes)
	}
	if rec := serve(g, "GET /cds-services", ""); rec.Code != http.StatusOK {
		t.Fatalf("CDS discovery = %d %s, want 200", rec.Code, rec.Body.String())
	}
}

// On either lane the originating routes pass the guard and originate: the system of
// record is read and, on the demo lane driven to the stub payer, a leg is sent.
func TestOriginatingRoutes_RunOnEveryLane(t *testing.T) {
	for _, profile := range OriginationProfiles() {
		t.Run(profile, func(t *testing.T) {
			g, stub, counter, events := unsetProfileProvider(t, false)
			g.cfg.OriginationProfile = profile
			aimStubAt(t, stub, "MBR-D-UC03")
			rec := serve(g, "POST /scenario/uc03", "")
			if strings.Contains(rec.Body.String(), "origination profile not set") {
				t.Fatalf("%s: uc03 refused as unset: %d %s", profile, rec.Code, rec.Body.String())
			}
			read := false
			for _, e := range *events {
				read = read || e.Kind == "sor.read"
			}
			if !read {
				t.Fatalf("%s: uc03 read nothing from the system of record (%d %s)", profile, rec.Code, rec.Body.String())
			}
			if profile == "demo" && (counter.n == 0 || !legAttempted(stub.legTypes, "crd-order-dispatch")) {
				t.Fatalf("demo: uc03 sent no CRD leg: %d requests, legs %v (%d %s)", counter.n, stub.legTypes, rec.Code, rec.Body.String())
			}
		})
	}
}

// providerOtherRoutes are the provider's routes outside /scenario/: its Da Vinci
// ingress, which relays the participant's own requests and never originates.
var providerOtherRoutes = []string{
	"GET /cds-services",
	"POST /cds-services/{id}",
	"POST /Questionnaire/$questionnaire-package",
	"POST /Claim/$submit",
	"POST /Claim/$inquire",
	"GET /metadata",
	"POST /oauth/token",
	"GET /.well-known/smart-configuration",
	"GET /.well-known/davinci-configuration",
}

// otherRoleRoutes are the routes of the roles that originate nothing.
var otherRoleRoutes = map[string][]string{
	"payer":    {"POST /substrate/inbound", "GET /metadata", "GET /ExplanationOfBenefit", "GET /ExplanationOfBenefit/{id}"},
	"facility": {"POST /substrate/inbound"},
	"phg":      {"POST /substrate/inbound"},
}

// Every route this package registers is registered in (*Gateway).Handler, under one
// role, with a literal pattern, and is classified here. The whole package is parsed
// (every non-test file): a Handle or HandleFunc call whose first argument is a string
// literal anywhere outside Handler fails, and inside Handler the ServeMux may be passed
// to Handle and HandleFunc only, so no helper can register routes on it. A provider
// route counts as guarded only when its handler argument is a call to originates
// (bound once, to requireOriginationProfile) or to g.requireOriginationProfile itself.
// Every originating route is guarded; no other route is. A new route, however it is
// written (Handle or HandleFunc, on one line or several, in any file), cannot join
// either set silently.
func TestScenarioRoutes_EveryRouteIsClassified(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	if len(files) < 2 {
		t.Fatalf("parsed %d files; the package has more", len(files))
	}

	want := map[string]bool{} // "role pattern" -> guarded
	for _, r := range originatingRoutes {
		want["provider "+r] = true
	}
	for _, r := range append(append([]string{}, localScenarioRoutes...), providerOtherRoutes...) {
		want["provider "+r] = false
	}
	for role, routes := range otherRoleRoutes {
		for _, r := range routes {
			want[role+" "+r] = false
		}
	}

	isGuard := func(e ast.Expr) bool {
		call, ok := e.(*ast.CallExpr)
		if !ok {
			return false
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			return fn.Name == "originates"
		case *ast.SelectorExpr:
			return fn.Sel.Name == "requireOriginationProfile"
		}
		return false
	}
	isRoleSwitch := func(sw *ast.SwitchStmt) bool {
		sel, ok := sw.Tag.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "Role"
	}
	isGatewayHandler := func(fn *ast.FuncDecl) bool {
		if fn == nil || fn.Name.Name != "Handler" || fn.Recv == nil || len(fn.Recv.List) != 1 {
			return false
		}
		star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			return false
		}
		id, ok := star.X.(*ast.Ident)
		return ok && id.Name == "Gateway"
	}
	isRegistration := func(call *ast.CallExpr) (string, bool) {
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
			return "", false
		}
		return sel.Sel.Name, true
	}
	isStringLit := func(e ast.Expr) bool {
		lit, ok := e.(*ast.BasicLit)
		return ok && lit.Kind == token.STRING
	}

	got := map[string]bool{}
	bindings, handlers := 0, 0
	for _, file := range files {
		var stack []ast.Node
		ast.Inspect(file, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			var fn *ast.FuncDecl
			for i := len(stack) - 1; i >= 0 && fn == nil; i-- {
				fn, _ = stack[i].(*ast.FuncDecl)
			}
			inHandler := isGatewayHandler(fn)
			if d, ok := n.(*ast.FuncDecl); ok && isGatewayHandler(d) {
				handlers++
			}
			if as, ok := n.(*ast.AssignStmt); ok && inHandler {
				for i, lhs := range as.Lhs {
					if id, ok := lhs.(*ast.Ident); ok && id.Name == "originates" {
						sel, ok := as.Rhs[i].(*ast.SelectorExpr)
						if !ok || sel.Sel.Name != "requireOriginationProfile" {
							t.Errorf("%s: originates is bound to something other than requireOriginationProfile", fset.Position(as.Pos()))
						}
						bindings++
					}
				}
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			pos := fset.Position(call.Pos())
			method, registers := isRegistration(call)
			if !inHandler {
				if registers && len(call.Args) > 0 && isStringLit(call.Args[0]) {
					t.Errorf("%s: a route is registered outside (*Gateway).Handler", pos)
				}
				return true
			}
			if !registers {
				for _, a := range call.Args {
					if id, ok := a.(*ast.Ident); ok && id.Name == "mux" {
						t.Errorf("%s: Handler passes its ServeMux to a call other than Handle or HandleFunc", pos)
					}
				}
				return true
			}
			var roles []string
			for i := len(stack) - 2; i > 1; i-- {
				cc, ok := stack[i].(*ast.CaseClause)
				if !ok {
					continue
				}
				if sw, ok := stack[i-2].(*ast.SwitchStmt); ok && isRoleSwitch(sw) {
					for _, e := range cc.List {
						if lit, ok := e.(*ast.BasicLit); ok {
							if v, err := strconv.Unquote(lit.Value); err == nil {
								roles = append(roles, v)
							}
						}
					}
					break
				}
			}
			if len(roles) == 0 {
				t.Errorf("%s: a route is registered outside the role switch", pos)
				return true
			}
			if len(call.Args) != 2 {
				t.Errorf("%s: %s with %d arguments", pos, method, len(call.Args))
				return true
			}
			if !isStringLit(call.Args[0]) {
				t.Errorf("%s: route pattern is not a string literal", pos)
				return true
			}
			pattern, _ := strconv.Unquote(call.Args[0].(*ast.BasicLit).Value)
			for _, role := range roles {
				key := role + " " + pattern
				if _, dup := got[key]; dup {
					t.Errorf("%s: %s registered twice", pos, key)
				}
				got[key] = isGuard(call.Args[1])
			}
			return true
		})
	}
	if handlers != 1 {
		t.Errorf("found %d (*Gateway).Handler methods, want 1", handlers)
	}
	if bindings != 1 {
		t.Errorf("originates is bound %d times in Handler, want once", bindings)
	}
	for key, guarded := range got {
		w, listed := want[key]
		switch {
		case !listed:
			t.Errorf("%s is registered but not classified (add it to a route list here)", key)
		case w && !guarded:
			t.Errorf("%s originates but is not wired through requireOriginationProfile", key)
		case !w && guarded:
			t.Errorf("%s does not originate but is wired through requireOriginationProfile", key)
		}
	}
	for key := range want {
		if _, ok := got[key]; !ok {
			t.Errorf("%s is classified but Handler does not register it", key)
		}
	}
}
