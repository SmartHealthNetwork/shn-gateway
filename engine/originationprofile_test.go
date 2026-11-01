//go:build !shn_coreonly

package engine

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"slices"
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
// the route declarations the same.
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
// nothing. Like every /scenario route, they are mounted only with an origination
// profile.
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

// On a provider with no origination profile module 1 is off: no /scenario
// route is mounted, so every one of them, a path no route names, and every
// method on them is the mux's own 404, the answer of a path the gateway does
// not serve. No module 1 handler runs: nothing is read, built or sent, and
// nothing is recorded.
func TestScenarioRoutes_NotMountedWithoutProfile(t *testing.T) {
	t.Parallel()
	notFound := httptest.NewRecorder()
	http.NotFound(notFound, httptest.NewRequest(http.MethodGet, "/", nil))
	paths := []string{"GET /scenario/uc06", "POST /scenario/unknown", "POST /scenario/uc07/pending", "GET /scenario"}
	for _, route := range slices.Concat(originatingRoutes, localScenarioRoutes, paths) {
		t.Run(route, func(t *testing.T) {
			g, stub, counter, events := unsetProfileProvider(t, true)
			rec := serve(g, route, `{"continuation":"c1","resumeToken":"r1","answers":{}}`)
			if rec.Code != http.StatusNotFound || rec.Body.String() != notFound.Body.String() {
				t.Fatalf("%s = %d %q, want the mux's own 404 %q", route, rec.Code, rec.Body.String(), notFound.Body.String())
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

// On either lane the originating routes are mounted and originate: the system of
// record is read and, on the demo lane driven to the stub payer, a leg is sent.
func TestOriginatingRoutes_RunOnEveryLane(t *testing.T) {
	for _, profile := range OriginationProfiles() {
		t.Run(profile, func(t *testing.T) {
			g, stub, counter, events := unsetProfileProvider(t, false)
			declareOriginationProfile(g, profile)
			aimStubAt(t, stub, "MBR-D-UC03")
			rec := serve(g, "POST /scenario/uc03", testRouteBody(g, "/scenario/uc03", ""))
			if rec.Code == http.StatusNotFound {
				t.Fatalf("%s: uc03 is not mounted: %d %s", profile, rec.Code, rec.Body.String())
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
	"POST /CoverageEligibilityRequest/$submit",
	"POST /$submit-attachment",
	"POST /cdex/payer/{payer}",
	"POST /cdex/payer/{payer}/$submit-attachment",
	"GET /cdex/payers",
	"POST /Task",
	"GET /metadata",
	"POST /oauth/token",
	"GET /.well-known/smart-configuration",
	"GET /.well-known/davinci-configuration",
}

// otherRoleRoutes are the routes of the roles that originate nothing.
var otherRoleRoutes = map[string][]string{
	"payer":    {"POST /substrate/inbound"},
	"facility": {"POST /substrate/inbound"},
	"phg":      {"POST /substrate/inbound"},
}

// Every route this package mounts is declared by one piece of the gateway for
// a role (AI-G10) and is classified here, both by reading the source and by
// running the declarations.
//
// By source (every non-test file of the package):
//   - a Handle or HandleFunc call with a pattern and a handler fails anywhere
//     outside (*Gateway).routes, and inside it the ServeMux is passed to
//     HandleFunc once and to nothing else, so no helper mounts a route on it;
//     Handler is exactly handlerBody, the routes mux served under the
//     operation count, so it answers no path of its own;
//   - routes mounts exactly coreRoutes, workstreamRoutes (every compiled-in
//     workstream adapter's routes: the PA adapter's are paRoutes, the
//     Federated Query Services adapter's fqsRoutes) and moduleRoutes (every
//     compiled-in module's routes: on-behalf building's are originationRoutes
//     as its state kept them, in moduleOneState's origination field, which
//     module 1's newGatewayState alone sets, from originationRoutes called
//     with the configured role: originationKeptFaults), each called with the
//     role, in that order; mountedRoute is named nowhere but its type,
//     routes, the declarations coreRoutes, paRoutes, fqsRoutes and
//     originationRoutes, moduleOneState's origination field, and the
//     signatures of workstreamRoutes and moduleRoutes, the workstream and
//     gatewayModule interfaces' routes methods, and their adapters' and
//     modules' routes methods; and the bodies of workstreamRoutes, moduleRoutes, each
//     adapter's routes method and each module's are exactly
//     workstreamRoutesBody, moduleRoutesBody, adapterRoutesBody and
//     moduleRoutesMethodBodies, so neither registry decides anything about
//     which routes mount;
//   - readsClaimFacts is called once in the package, by originationRoutes,
//     on the routes it declares originating, requireScenarioCaller once, by
//     routes, on the routes declared as the operator surface, and
//     recordExchange only by ingressRoute, inboundRoute and routes (on the
//     routes declared with an exchange route);
//   - on-behalf building's routes are its static table, originationRouteTable
//     (originationTableFaults): each row a pattern literal, an originates
//     literal and a method of the gateway; originationRoutes, which mounts it,
//     and moduleOne.exchangeRoutes, which names its exchange routes, are
//     exactly originationRoutesBody and moduleOneExchangeRoutesBody, so an
//     originating row, and only one, reads the Claim facts and records its
//     exchange; the table is named nowhere else;
//   - in a declaration every route pattern is a string literal, given to a
//     local closure or as a mountedRoute's pattern, and it is declared
//     originating (its handler is readsClaimFacts over its own, directly or
//     through a closure that builds originating routes) exactly when it is
//     classified as originating, and
//     declared the operator surface (operator: true) exactly when on-behalf
//     building declares it; a declaration names no other string but a role
//     and "", and reads no gateway state but the conditions routeConditions
//     drives (routeGates).
//
// By running every declaration for every role, under every condition that
// gates a route: every /scenario route and no other is mounted behind
// requireScenarioCaller, and only with a declared origination profile; a
// route is declared with an exchange route exactly when it is an originating
// /scenario route, and that name is its originationExchangeRoute, one of
// ExchangeRoutes, named by no other route; a route a role mounts is declared once, each piece
// declares only its own routes (the core its inbound, on-behalf building
// /scenario/*, the workstream adapters the rest), and every pattern a
// piece's source names is mounted by that piece under some driven condition,
// so a route under a condition this test does not drive fails it. A new route, however
// it is written, cannot join any of these sets silently.
func TestScenarioRoutes_EveryRouteIsClassified(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || !inThisBuild(t, ".", name) {
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

	declarations := map[string]string{"coreRoutes": "core", "paRoutes": "pa", "fqsRoutes": "fqs"}
	// pinnedDeclarations are declarations whose whole body is pinned: on-behalf
	// building's mounts its static table, read by originationTableFaults.
	pinnedDeclarations := map[string]string{"originationRoutes": originationRoutesBody}
	// mountable are what routes mounts: the core's declaration, and the
	// workstream and module registries' routes.
	mountable := map[string]bool{"coreRoutes": true, "workstreamRoutes": true, "moduleRoutes": true}
	originating := map[string]bool{}
	for _, r := range originatingRoutes {
		originating[r] = true
	}
	classified := map[string]bool{}
	for _, r := range slices.Concat(originatingRoutes, localScenarioRoutes, providerOtherRoutes) {
		classified[r] = true
	}
	for _, routes := range otherRoleRoutes {
		for _, r := range routes {
			classified[r] = true
		}
	}
	gatewayFields := map[string]bool{}
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			if ts, ok := n.(*ast.TypeSpec); ok && ts.Name.Name == "Gateway" {
				if st, ok := ts.Type.(*ast.StructType); ok {
					for _, f := range st.Fields.List {
						for _, id := range f.Names {
							gatewayFields[id.Name] = true
						}
					}
				}
			}
			return true
		})
	}
	if !gatewayFields["cfg"] || !gatewayFields["ingressAuth"] {
		t.Fatalf("the Gateway struct's fields were not found: %v", gatewayFields)
	}

	isRegistration := func(call *ast.CallExpr) bool {
		sel, ok := call.Fun.(*ast.SelectorExpr)
		return ok && (sel.Sel.Name == "Handle" || sel.Sel.Name == "HandleFunc") && len(call.Args) == 2
	}
	literals, tableFaults := originationTableFaults(fset, files)
	for _, f := range tableFaults {
		t.Error(f)
	}
	pinnedBodies := map[string]int{}
	routesFuncs, handlers, muxRegistrations, mountLists, registryBodies, adapterBodies := 0, 0, 0, 0, 0, 0
	moduleRegistryBodies, moduleBodies := 0, 0
	moduleTypes := map[string]bool{}
	for _, m := range modules {
		moduleTypes[reflect.TypeOf(m).Name()] = true
	}
	guards := map[string]int{}
	mounted := map[string]bool{}
	var mountOrder []string
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, _ := decl.(*ast.FuncDecl)
			method, recv := gatewayMethod(fn)
			if method == "routes" {
				routesFuncs++
			}
			if method == "Handler" {
				handlers++
				var body strings.Builder
				if err := printer.Fprint(&body, fset, fn.Body); err != nil {
					t.Fatal(err)
				}
				// Comments are not parsed; the blank lines they leave are dropped.
				printed := body.String()
				for strings.Contains(printed, "\n\n") {
					printed = strings.ReplaceAll(printed, "\n\n", "\n")
				}
				if want := strings.ReplaceAll(handlerBody, "g.", recv+"."); printed != want {
					t.Errorf("%s: Handler is not exactly the routes mux served under the operation count; declare a route instead of answering in it:\n%s", fset.Position(fn.Pos()), body.String())
				}
			}
			if piece, ok := declarations[method]; ok {
				literals = append(literals, readRouteDeclaration(t, fset, fn, recv, piece, gatewayFields)...)
			}
			if want, ok := pinnedDeclarations[method]; ok {
				pinnedBodies[method]++
				if err := bodyIs(fset, fn, strings.ReplaceAll(want, "g.", recv+".")); err != nil {
					t.Errorf("%s: %s %v", fset.Position(fn.Pos()), method, err)
				}
			}
			// signature is where mountedRoute may be named as a type alone: the
			// registries' routes and the workstream adapters' and modules'
			// routes methods, whose bodies are pinned.
			var signature ast.Node
			otherRoutes := fn != nil && fn.Name.Name == "routes" && fn.Recv != nil && len(fn.Recv.List) == 1 && receiverType(fn) != "Gateway"
			moduleRoutesMethod := otherRoutes && moduleTypes[receiverType(fn)]
			adapterRoutes := otherRoutes && !moduleRoutesMethod
			if method == "moduleRoutes" {
				signature = fn.Type
				moduleRegistryBodies++
				if err := bodyIs(fset, fn, strings.ReplaceAll(moduleRoutesBody, "g,", recv+",")); err != nil {
					t.Errorf("%s: moduleRoutes %v", fset.Position(fn.Pos()), err)
				}
			}
			if moduleRoutesMethod {
				signature = fn.Type
				moduleBodies++
				if err := moduleRoutesMethodIs(fset, fn); err != nil {
					t.Errorf("%s: a module's routes %v", fset.Position(fn.Pos()), err)
				}
			}
			if method == "workstreamRoutes" {
				signature = fn.Type
				registryBodies++
				if err := bodyIs(fset, fn, strings.ReplaceAll(workstreamRoutesBody, "g,", recv+",")); err != nil {
					t.Errorf("%s: workstreamRoutes %v", fset.Position(fn.Pos()), err)
				}
			}
			if adapterRoutes {
				signature = fn.Type
				adapterBodies++
				if err := adapterRoutesIs(fset, fn); err != nil {
					t.Errorf("%s: a workstream adapter's routes %v", fset.Position(fn.Pos()), err)
				}
			}
			var stack []ast.Node
			ast.Inspect(decl, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return true
				}
				stack = append(stack, n)
				switch x := n.(type) {
				case *ast.TypeSpec:
					if x.Name.Name == "workstream" || x.Name.Name == "gatewayModule" {
						// The adapter and module interfaces may name
						// mountedRoute in their routes method's signature
						// alone.
						if it, ok := x.Type.(*ast.InterfaceType); ok {
							for _, m := range it.Methods.List {
								if len(m.Names) == 1 && m.Names[0].Name == "routes" {
									continue
								}
								ast.Inspect(m.Type, func(n ast.Node) bool {
									if id, ok := n.(*ast.Ident); ok && id.Name == "mountedRoute" {
										t.Errorf("%s: mountedRoute is named in a %s method other than routes", fset.Position(id.Pos()), x.Name.Name)
									}
									return true
								})
							}
							return false
						}
					}
					if x.Name.Name == "mountedRoute" {
						// Its own name is not a use.
						ast.Inspect(x.Type, func(m ast.Node) bool {
							if id, ok := m.(*ast.Ident); ok && id.Name == "mountedRoute" {
								t.Errorf("%s: mountedRoute names itself", fset.Position(id.Pos()))
							}
							return true
						})
						return false
					}
				case *ast.Ident:
					inSignature := signature != nil && x.Pos() >= signature.Pos() && x.End() <= signature.End()
					if x.Name == "mountedRoute" && method != "routes" && declarations[method] == "" && pinnedDeclarations[method] == "" && !inSignature && !inOriginationField(stack) {
						t.Errorf("%s: mountedRoute is named outside its type, routes and the route declarations", fset.Position(x.Pos()))
					}
				case *ast.SelectorExpr:
					if wantIn, guard := map[string]string{"readsClaimFacts": "originationRoutes", "requireScenarioCaller": "routes"}[x.Sel.Name]; guard {
						if call, isCall := stack[len(stack)-2].(*ast.CallExpr); !isCall || call.Fun != x || method != wantIn {
							t.Errorf("%s: %s is used outside %s", fset.Position(x.Pos()), x.Sel.Name, wantIn)
						}
						guards[x.Sel.Name]++
					}
					if x.Sel.Name == "recordExchange" {
						if call, isCall := stack[len(stack)-2].(*ast.CallExpr); !isCall || call.Fun != x || !map[string]bool{"ingressRoute": true, "inboundRoute": true, "routes": true}[method] {
							t.Errorf("%s: recordExchange is used outside ingressRoute, inboundRoute and routes", fset.Position(x.Pos()))
						}
					}
				case *ast.CompositeLit:
					if method != "routes" {
						break
					}
					outer, ok := x.Type.(*ast.ArrayType)
					if !ok {
						break
					}
					inner, ok := outer.Elt.(*ast.ArrayType)
					if !ok {
						break
					}
					if id, ok := inner.Elt.(*ast.Ident); !ok || id.Name != "mountedRoute" {
						break
					}
					mountLists++
					for _, elt := range x.Elts {
						name, ok := declarationCall(elt, recv)
						if !ok {
							t.Errorf("%s: routes mounts something other than a declaration called with the role", fset.Position(elt.Pos()))
							continue
						}
						mounted[name] = true
						mountOrder = append(mountOrder, name)
					}
				case *ast.CallExpr:
					pos := fset.Position(x.Pos())
					if method != "routes" {
						if isRegistration(x) {
							t.Errorf("%s: a route is mounted outside (*Gateway).routes", pos)
						}
						break
					}
					if isRegistration(x) {
						muxRegistrations++
						break
					}
					for _, a := range x.Args {
						if id, ok := a.(*ast.Ident); ok && id.Name == "mux" {
							t.Errorf("%s: routes passes its ServeMux to a call other than Handle or HandleFunc", pos)
						}
					}
				}
				return true
			})
		}
	}
	for _, f := range originationKeptFaults(fset, files) {
		t.Error(f)
	}
	if registryBodies != 1 || adapterBodies != len(workstreams) {
		t.Errorf("found %d workstreamRoutes and %d adapter routes methods, want 1 and %d (one per compiled-in workstream)", registryBodies, adapterBodies, len(workstreams))
	}
	if moduleRegistryBodies != 1 || moduleBodies != len(modules) {
		t.Errorf("found %d moduleRoutes and %d module routes methods, want 1 and %d (one per compiled-in module)", moduleRegistryBodies, moduleBodies, len(modules))
	}
	if routesFuncs != 1 || handlers != 1 {
		t.Errorf("found %d (*Gateway).routes and %d (*Gateway).Handler methods, want 1 of each", routesFuncs, handlers)
	}
	if muxRegistrations != 1 {
		t.Errorf("routes mounts through %d Handle/HandleFunc calls, want the one loop's", muxRegistrations)
	}
	if mountLists != 1 {
		t.Errorf("routes builds %d lists of declarations, want one", mountLists)
	}
	for name := range pinnedDeclarations {
		if pinnedBodies[name] != 1 {
			t.Errorf("found %d %s, want 1", pinnedBodies[name], name)
		}
	}
	for g, in := range map[string]string{"readsClaimFacts": "originationRoutes", "requireScenarioCaller": "routes"} {
		if guards[g] != 1 {
			t.Errorf("%s is called %d times in the package, want once (by %s)", g, guards[g], in)
		}
	}
	if want := []string{"coreRoutes", "workstreamRoutes", "moduleRoutes"}; !slices.Equal(mountOrder, want) {
		t.Errorf("routes mounts %v in that order, want %v: the core's, then the workstreams', then the modules'", mountOrder, want)
	}
	for name := range mountable {
		if !mounted[name] {
			t.Errorf("routes does not mount %s", name)
		}
	}
	for name := range mounted {
		if !mountable[name] {
			t.Errorf("routes mounts %s, which is not a classified declaration", name)
		}
	}
	if len(literals) == 0 {
		t.Fatal("no route pattern was read from the declarations")
	}
	for _, l := range literals {
		switch {
		case !classified[l.pattern]:
			t.Errorf("%s: %s is declared but not classified (add it to a route list here)", l.pos, l.pattern)
		case l.originates != originating[l.pattern]:
			t.Errorf("%s: %s is declared originating=%v but classified originating=%v", l.pos, l.pattern, l.originates, originating[l.pattern])
		case l.operator != (l.piece == "origination"):
			t.Errorf("%s: %s is declared operator=%v, but every /scenario route, and only those, is the operator surface", l.pos, l.pattern, l.operator)
		}
	}

	want := map[string]bool{} // "role pattern" -> originates (read from the source above)
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
	got := map[string]bool{}
	produced := map[string]bool{}        // "piece pattern"
	exchangeNames := map[string]string{} // exchange route -> pattern
	for _, role := range []string{"provider", "payer", "facility", "phg"} {
		for _, c := range everyRouteCondition() {
			g := routeGateway(role, c)
			// The registry mounts exactly the adapters' declarations, in
			// registry order: PA's, then Federated Query Services'.
			if reg, adapters := g.workstreamRoutes(role), slices.Concat(g.paRoutes(role), g.fqsRoutes(role)); !sameRoutes(reg, adapters) {
				t.Errorf("%s under %s: the workstream registry mounts %v, the adapters declare %v", role, c, routePatterns(reg), routePatterns(adapters))
			}
			// The module registry mounts exactly what on-behalf building
			// declares for the role, as its state kept it.
			declared, err := g.originationRoutes(role)
			if err != nil {
				t.Fatalf("%s under %s: %v", role, c, err)
			}
			if reg := g.moduleRoutes(role); !sameRoutes(reg, declared) {
				t.Errorf("%s under %s: the module registry mounts %v, on-behalf building declares %v", role, c, routePatterns(reg), routePatterns(declared))
			}
			seen := map[string]string{}
			for piece, declared := range map[string][]mountedRoute{
				"core": g.coreRoutes(role), "pa": g.paRoutes(role), "fqs": g.fqsRoutes(role), "origination": g.moduleRoutes(role),
			} {
				for _, rt := range declared {
					key := role + " " + rt.pattern
					if other, dup := seen[rt.pattern]; dup {
						t.Errorf("%s under %s is declared by %s and %s", key, c, other, piece)
					}
					seen[rt.pattern] = piece
					produced[piece+" "+rt.pattern] = true
					scenario := strings.Contains(rt.pattern, "/scenario")
					inbound := rt.pattern == "POST /substrate/inbound"
					if (piece == "origination") != scenario || (piece == "core") != inbound {
						t.Errorf("%s is declared by %s, which does not own it", key, piece)
					}
					if rt.handler == nil {
						t.Errorf("%s is declared with no handler", key)
					}
					if rt.operator != (piece == "origination") {
						t.Errorf("%s under %s is mounted operator=%v, want it behind requireScenarioCaller exactly when it is /scenario", key, c, rt.operator)
					}
					// An originating /scenario route, and only one, records its
					// exchange, under its own closed route name.
					if records := piece == "origination" && originating[rt.pattern]; (rt.exchangeRoute != "") != records {
						t.Errorf("%s under %s is declared with exchange route %q, want one exactly when it originates", key, c, rt.exchangeRoute)
					} else if records {
						if rt.exchangeRoute != originationExchangeRoute(rt.pattern) || !slices.Contains(ExchangeRoutes, rt.exchangeRoute) || rt.exchangeRoute == ExchangeOther {
							t.Errorf("%s records route %q, want its originationExchangeRoute %q, one of ExchangeRoutes", key, rt.exchangeRoute, originationExchangeRoute(rt.pattern))
						}
						if other, dup := exchangeNames[rt.exchangeRoute]; dup && other != rt.pattern {
							t.Errorf("%s and %s record the same route %q", rt.pattern, other, rt.exchangeRoute)
						}
						exchangeNames[rt.exchangeRoute] = rt.pattern
					}
					got[key] = true
				}
			}
		}
	}
	for key := range got {
		if _, listed := want[key]; !listed {
			t.Errorf("%s is declared but not classified (add it to a route list here)", key)
		}
	}
	for key := range want {
		if _, ok := got[key]; !ok {
			t.Errorf("%s is classified but no declaration mounts it", key)
		}
	}
	// Every exchange route a module names is recorded by a route it mounts.
	for _, name := range moduleExchangeRouteNames() {
		if _, ok := exchangeNames[name]; !ok {
			t.Errorf("module exchange route %q is recorded by no mounted route", name)
		}
	}
	for _, l := range literals {
		if !produced[l.piece+" "+l.pattern] {
			t.Errorf("%s: %s declares %s under a role or condition this test does not drive, or this test does not run the %s declaration", l.pos, l.piece, l.pattern, l.piece)
		}
	}
}

// sameRoutes reports whether a and b declare the same patterns, in order, and
// the same ones the operator surface.
func sameRoutes(a, b []mountedRoute) bool {
	return slices.EqualFunc(a, b, func(x, y mountedRoute) bool {
		return x.pattern == y.pattern && x.operator == y.operator && x.exchangeRoute == y.exchangeRoute && (x.handler == nil) == (y.handler == nil)
	})
}

func routePatterns(rs []mountedRoute) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.pattern)
	}
	return out
}

// routeFlags are what a mountedRoute literal declares about its route: that it
// originates, and that it is the operator surface.
type routeFlags struct{ originates, operator bool }

// declaredRoute is one route pattern a declaration's source names: the piece
// that declares it, where, and whether it is declared originating and the
// operator surface.
type declaredRoute struct {
	piece, pattern, pos string
	originates          bool
	operator            bool
}

// routeRoles are the roles a route declaration may name; routeGates the
// gateway state it may read (its own fields, and its config's), which are
// the conditions routeGateway sets.
var (
	routeRoles      = map[string]bool{"provider": true, "payer": true, "facility": true, "phg": true}
	routeGates      = map[string]bool{"cfg.IngressEnabled": true, "cfg.IngressBaseURL": true, "cfg.OriginationProfile": true, "ingressAuth": true}
	routePatternLit = regexp.MustCompile(`^(GET|POST|PUT|PATCH|DELETE) /`)
)

// readRouteDeclaration reads the route patterns a declaration's source names,
// failing t on anything the classifier cannot read: a pattern that is not a
// string literal, an operator value that is not a literal, a
// string that is neither a pattern, a role nor "", or gateway state other than
// the gates.
func readRouteDeclaration(t *testing.T, fset *token.FileSet, fn *ast.FuncDecl, recv, piece string, gatewayFields map[string]bool) []declaredRoute {
	t.Helper()
	at := func(n ast.Node) string { return fset.Position(n.Pos()).String() }
	// closures are the declaration's local route builders, by name: whether
	// the routes each builds are originating and the operator surface.
	closures := map[string]routeFlags{}
	// consumed are the pattern literals read as a closure's argument or a
	// mountedRoute's pattern.
	consumed := map[*ast.BasicLit]bool{}
	var out []declaredRoute
	// routeLiteral reads one mountedRoute literal: whether it originates (its
	// handler is readsClaimFacts over the route's own), its operator value, and
	// its pattern, which is param inside a closure and a literal elsewhere.
	routeLiteral := func(lit *ast.CompositeLit, param string) (flags routeFlags) {
		for _, e := range lit.Elts {
			kv, ok := e.(*ast.KeyValueExpr)
			if !ok {
				t.Errorf("%s: a mountedRoute in %s is not written with field names", at(e), fn.Name.Name)
				continue
			}
			key, _ := kv.Key.(*ast.Ident)
			switch {
			case key == nil:
			case key.Name == "handler":
				if call, ok := kv.Value.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "readsClaimFacts" {
						flags.originates = true
					}
				}
			case key.Name == "operator":
				id, ok := kv.Value.(*ast.Ident)
				if !ok || (id.Name != "true" && id.Name != "false") {
					t.Errorf("%s: %s declares %s other than as a literal", at(kv.Value), fn.Name.Name, key.Name)
				}
				flags.operator = ok && id.Name == "true"
			case key.Name == "pattern":
				if param != "" {
					if id, ok := kv.Value.(*ast.Ident); !ok || id.Name != param {
						t.Errorf("%s: a route closure in %s builds a pattern other than its parameter", at(kv.Value), fn.Name.Name)
					}
					continue
				}
				bl, ok := kv.Value.(*ast.BasicLit)
				if !ok || bl.Kind != token.STRING {
					t.Errorf("%s: %s declares a route pattern that is not a string literal", at(kv.Value), fn.Name.Name)
					continue
				}
				consumed[bl] = true
			}
		}
		return flags
	}
	isRouteLit := func(n ast.Node) (*ast.CompositeLit, bool) {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return nil, false
		}
		id, ok := lit.Type.(*ast.Ident)
		return lit, ok && id.Name == "mountedRoute"
	}
	// The closures first, so a call to one is read wherever it appears.
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		name, okName := as.Lhs[0].(*ast.Ident)
		fl, okFn := as.Rhs[0].(*ast.FuncLit)
		if !okName || !okFn {
			return true
		}
		param := ""
		if ps := fl.Type.Params.List; len(ps) > 0 && len(ps[0].Names) > 0 {
			param = ps[0].Names[0].Name
		}
		builds := 0
		ast.Inspect(fl.Body, func(m ast.Node) bool {
			if lit, ok := isRouteLit(m); ok {
				builds++
				closures[name.Name] = routeLiteral(lit, param)
			}
			return true
		})
		if builds > 1 {
			t.Errorf("%s: route closure %s builds %d routes, want one", at(as), name.Name, builds)
		}
		return true
	})
	var stack []ast.Node
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		switch x := n.(type) {
		case *ast.FuncLit:
			// A closure's body was read above.
			if as, ok := stack[len(stack)-2].(*ast.AssignStmt); ok && len(as.Rhs) == 1 && as.Rhs[0] == x {
				if id, ok := as.Lhs[0].(*ast.Ident); ok {
					if _, isClosure := closures[id.Name]; isClosure {
						stack = stack[:len(stack)-1]
						return false
					}
				}
			}
		case *ast.CompositeLit:
			// A mountedRoute literal, written with its type or as an element
			// of a []mountedRoute.
			lits := []*ast.CompositeLit{}
			if lit, ok := isRouteLit(x); ok {
				lits = append(lits, lit)
			}
			if arr, ok := x.Type.(*ast.ArrayType); ok {
				if id, ok := arr.Elt.(*ast.Ident); ok && id.Name == "mountedRoute" {
					for _, e := range x.Elts {
						if el, ok := e.(*ast.CompositeLit); ok && el.Type == nil {
							lits = append(lits, el)
						}
					}
				}
			}
			for _, lit := range lits {
				flags := routeLiteral(lit, "")
				for _, e := range lit.Elts {
					if kv, ok := e.(*ast.KeyValueExpr); ok {
						if key, _ := kv.Key.(*ast.Ident); key != nil && key.Name == "pattern" {
							if bl, ok := kv.Value.(*ast.BasicLit); ok {
								v, _ := strconv.Unquote(bl.Value)
								out = append(out, declaredRoute{piece: piece, pattern: v, pos: at(bl), originates: flags.originates, operator: flags.operator})
							}
						}
					}
				}
			}
		case *ast.CallExpr:
			id, ok := x.Fun.(*ast.Ident)
			if !ok {
				break
			}
			flags, isClosure := closures[id.Name]
			if !isClosure {
				break
			}
			if len(x.Args) == 0 {
				t.Errorf("%s: %s is called with no pattern", at(x), id.Name)
				break
			}
			bl, ok := x.Args[0].(*ast.BasicLit)
			if !ok || bl.Kind != token.STRING {
				t.Errorf("%s: %s declares a route pattern that is not a string literal", at(x.Args[0]), fn.Name.Name)
				break
			}
			consumed[bl] = true
			v, _ := strconv.Unquote(bl.Value)
			out = append(out, declaredRoute{piece: piece, pattern: v, pos: at(bl), originates: flags.originates, operator: flags.operator})
		case *ast.BasicLit:
			if x.Kind != token.STRING {
				break
			}
			v, _ := strconv.Unquote(x.Value)
			switch {
			case routePatternLit.MatchString(v):
				// Checked below: a pattern must be consumed.
			case v != "" && !routeRoles[v]:
				t.Errorf("%s: %s names %q, which is neither a route pattern, a role nor \"\"", at(x), fn.Name.Name, v)
			}
		case *ast.SelectorExpr:
			// <recv>.<field> and <recv>.cfg.<field>: only the gates are read.
			if r, ok := x.X.(*ast.Ident); ok && r.Name == recv && gatewayFields[x.Sel.Name] {
				parent, _ := stack[len(stack)-2].(*ast.SelectorExpr)
				switch {
				case x.Sel.Name == "cfg" && (parent == nil || parent.X != x):
					t.Errorf("%s: %s reads %s.cfg whole", at(x), fn.Name.Name, recv)
				case x.Sel.Name != "cfg" && !routeGates[x.Sel.Name]:
					t.Errorf("%s: %s reads %s.%s, which routeConditions does not drive", at(x), fn.Name.Name, recv, x.Sel.Name)
				}
			}
			if inner, ok := x.X.(*ast.SelectorExpr); ok {
				if r, ok := inner.X.(*ast.Ident); ok && r.Name == recv && inner.Sel.Name == "cfg" && !routeGates["cfg."+x.Sel.Name] {
					t.Errorf("%s: %s reads %s.cfg.%s, which routeConditions does not drive", at(x), fn.Name.Name, recv, x.Sel.Name)
				}
			}
		}
		return true
	})
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.STRING {
			if v, _ := strconv.Unquote(bl.Value); routePatternLit.MatchString(v) && !consumed[bl] {
				t.Errorf("%s: %s names the pattern %s other than as a route closure's argument or a mountedRoute's pattern", at(bl), fn.Name.Name, v)
			}
		}
		return true
	})
	return out
}

// originationRoutesBody is (*Gateway).originationRoutes' whole body: the
// profile check, the role and profile gate, and the table mounted, each row the
// operator surface over its own handler, an originating row reading the Claim
// facts and recording its exchange under its exchange route, any other doing
// neither.
const originationRoutesBody = `{
	if err := checkOriginationProfile(g.cfg.OriginationProfile); err != nil {
		return nil, err
	}
	if role != "provider" || g.cfg.OriginationProfile == "" {
		return nil, nil
	}
	var out []mountedRoute
	for _, rt := range originationRouteTable {
		h := func(w http.ResponseWriter, r *http.Request) { rt.handler(g, w, r) }
		if !rt.originates {
			out = append(out, mountedRoute{pattern: rt.pattern, handler: h, operator: true, credential: carriesCredential})
			continue
		}
		out = append(out, mountedRoute{pattern: rt.pattern, handler: g.readsClaimFacts(h), operator: true, exchangeRoute: originationExchangeRoute(rt.pattern), credential: carriesCredential})
	}
	return out, nil
}`

// moduleOneExchangeRoutesBody is moduleOne.exchangeRoutes' whole body: the
// exchange route of every originating row of the table, and no other.
const moduleOneExchangeRoutesBody = `{
	var out []string
	for _, rt := range originationRouteTable {
		if rt.originates {
			out = append(out, originationExchangeRoute(rt.pattern))
		}
	}
	return out
}`

// originationTableFaults reads on-behalf building's route table from files:
// the one originationRouteTable, a []originationRoute literal whose every row
// names exactly a pattern (a route pattern literal), originates (true or
// false) and a handler ((*Gateway).<method>), each with its field name. It
// returns the routes the table declares, each the operator surface (as
// originationRoutesBody mounts it), and the faults: a row it cannot read, a
// moduleOne.exchangeRoutes other than moduleOneExchangeRoutesBody, and the
// table named anywhere but its declaration, originationRoutes and
// moduleOne.exchangeRoutes.
func originationTableFaults(fset *token.FileSet, files []*ast.File) ([]declaredRoute, []string) {
	var out []declaredRoute
	var faults []string
	fault := func(n ast.Node, msg string) {
		faults = append(faults, fset.Position(n.Pos()).String()+": "+msg)
	}
	tables, exchangeRoutes := 0, 0
	for _, file := range files {
		for _, decl := range file.Decls {
			if gd, ok := decl.(*ast.GenDecl); ok && gd.Tok == token.VAR {
				for _, spec := range gd.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "originationRouteTable" {
						continue
					}
					tables++
					lit := originationTableLiteral(vs)
					if lit == nil {
						fault(vs, "originationRouteTable is not a []originationRoute literal")
						continue
					}
					for _, e := range lit.Elts {
						row, ok := e.(*ast.CompositeLit)
						if !ok || row.Type != nil {
							fault(e, "an originationRouteTable row is not an originationRoute literal")
							continue
						}
						var pattern string
						var originates, hasPattern, hasOriginates, hasHandler bool
						for _, f := range row.Elts {
							kv, _ := f.(*ast.KeyValueExpr)
							var key *ast.Ident
							if kv != nil {
								key, _ = kv.Key.(*ast.Ident)
							}
							if key == nil {
								fault(f, "an originationRouteTable row is not written with field names")
								continue
							}
							switch key.Name {
							case "pattern":
								bl, ok := kv.Value.(*ast.BasicLit)
								v := ""
								if ok && bl.Kind == token.STRING {
									v, _ = strconv.Unquote(bl.Value)
								}
								if !routePatternLit.MatchString(v) {
									fault(kv.Value, "an originationRouteTable pattern is not a route pattern literal")
									continue
								}
								pattern, hasPattern = v, true
							case "originates":
								id, ok := kv.Value.(*ast.Ident)
								if !ok || (id.Name != "true" && id.Name != "false") {
									fault(kv.Value, "an originationRouteTable row declares originates other than as a literal")
									continue
								}
								originates, hasOriginates = id.Name == "true", true
							case "handler":
								if !isGatewayMethodExpr(kv.Value) {
									fault(kv.Value, "an originationRouteTable handler is not a method of the gateway, (*Gateway).<method>")
									continue
								}
								hasHandler = true
							default:
								fault(kv, "an originationRouteTable row names "+key.Name+", which is not pattern, originates or handler")
							}
						}
						if !hasPattern || !hasOriginates || !hasHandler {
							fault(row, "an originationRouteTable row does not name its pattern, originates and handler")
							continue
						}
						out = append(out, declaredRoute{piece: "origination", pattern: pattern, pos: fset.Position(row.Pos()).String(), originates: originates, operator: true})
					}
				}
				// The table's own declaration is not a use.
				continue
			}
			fn, _ := decl.(*ast.FuncDecl)
			method, _ := gatewayMethod(fn)
			isExchangeRoutes := fn != nil && fn.Recv != nil && len(fn.Recv.List) == 1 && receiverType(fn) == "moduleOne" && fn.Name.Name == "exchangeRoutes"
			if isExchangeRoutes {
				exchangeRoutes++
				if err := bodyIs(fset, fn, moduleOneExchangeRoutesBody); err != nil {
					fault(fn, "moduleOne.exchangeRoutes "+err.Error())
				}
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && id.Name == "originationRouteTable" && method != "originationRoutes" && !isExchangeRoutes {
					fault(id, "originationRouteTable is named other than by originationRoutes and moduleOne.exchangeRoutes")
				}
				return true
			})
		}
	}
	if tables != 1 {
		faults = append(faults, fmt.Sprintf("found %d originationRouteTable declarations, want 1", tables))
	}
	if exchangeRoutes != 1 {
		faults = append(faults, fmt.Sprintf("found %d moduleOne.exchangeRoutes methods, want 1", exchangeRoutes))
	}
	return out, faults
}

// originationTableLiteral is vs's value when it is one []originationRoute
// literal, else nil.
func originationTableLiteral(vs *ast.ValueSpec) *ast.CompositeLit {
	if len(vs.Values) != 1 {
		return nil
	}
	lit, ok := vs.Values[0].(*ast.CompositeLit)
	if !ok {
		return nil
	}
	arr, ok := lit.Type.(*ast.ArrayType)
	if !ok || arr.Len != nil {
		return nil
	}
	if elt, ok := arr.Elt.(*ast.Ident); !ok || elt.Name != "originationRoute" {
		return nil
	}
	return lit
}

// isGatewayMethodExpr reports whether e is a method expression of *Gateway,
// (*Gateway).<method>.
func isGatewayMethodExpr(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	paren, ok := sel.X.(*ast.ParenExpr)
	if !ok {
		return false
	}
	star, ok := paren.X.(*ast.StarExpr)
	if !ok {
		return false
	}
	id, ok := star.X.(*ast.Ident)
	return ok && id.Name == "Gateway"
}

// gatewayMethod is fn's name and its receiver's name, when fn is a method of
// *Gateway.
func gatewayMethod(fn *ast.FuncDecl) (name, recv string) {
	if fn == nil || fn.Recv == nil || len(fn.Recv.List) != 1 {
		return "", ""
	}
	star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return "", ""
	}
	if id, ok := star.X.(*ast.Ident); !ok || id.Name != "Gateway" {
		return "", ""
	}
	if names := fn.Recv.List[0].Names; len(names) == 1 {
		recv = names[0].Name
	}
	return fn.Name.Name, recv
}

// inOriginationField reports whether the innermost node of stack is in the
// type of module 1's state's origination field.
func inOriginationField(stack []ast.Node) bool {
	inState := false
	for _, n := range stack {
		switch x := n.(type) {
		case *ast.TypeSpec:
			inState = x.Name.Name == "moduleOneState"
		case *ast.Field:
			if inState && len(x.Names) == 1 && x.Names[0].Name == "origination" {
				return true
			}
		}
	}
	return false
}

// originationKeptFaults: on-behalf building's routes are declared once, as
// New builds module 1's state, for the configured role, and kept. In files,
// moduleOneState's origination field is assigned exactly once, in module 1's
// newGatewayState (the one New runs for every module, after its options), as
// the first of the two values of originationRoutes called with the gateway's
// cfg.Role, the second (the refusal) held, not dropped; it is otherwise read
// only as module 1's routes method returns it, so nothing sets it in a
// literal, writes into it or hands it on; and nothing else calls
// originationRoutes.
func originationKeptFaults(fset *token.FileSet, files []*ast.File) []string {
	var faults []string
	fault := func(n ast.Node, msg string) {
		faults = append(faults, fset.Position(n.Pos()).String()+": "+msg)
	}
	sets := 0
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, _ := decl.(*ast.FuncDecl)
			moduleOneMethod := ""
			if fn != nil && fn.Recv != nil && len(fn.Recv.List) == 1 && receiverType(fn) == "moduleOne" {
				moduleOneMethod = fn.Name.Name
			}
			var kept *ast.CallExpr
			allowed := map[*ast.SelectorExpr]bool{}
			var stack []ast.Node
			ast.Inspect(decl, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return true
				}
				stack = append(stack, n)
				switch x := n.(type) {
				case *ast.AssignStmt:
					for i, lhs := range x.Lhs {
						sel, ok := lhs.(*ast.SelectorExpr)
						if !ok || sel.Sel.Name != "origination" {
							continue
						}
						sets++
						call, _ := x.Rhs[0].(*ast.CallExpr)
						if moduleOneMethod != "newGatewayState" || i != 0 || len(x.Lhs) != 2 || len(x.Rhs) != 1 || call == nil || !originationCallForRole(call) {
							fault(x, "module 1's origination is set other than by its newGatewayState, from originationRoutes(<gateway>.cfg.Role)")
							continue
						}
						if refusal, ok := x.Lhs[1].(*ast.Ident); !ok || refusal.Name == "_" {
							fault(x, "module 1's newGatewayState drops originationRoutes' refusal")
						}
						allowed[sel] = true
						kept = call
					}
				case *ast.SelectorExpr:
					if x.Sel.Name != "origination" || allowed[x] {
						break
					}
					if ret, isReturn := stack[len(stack)-2].(*ast.ReturnStmt); !isReturn || len(ret.Results) != 1 || moduleOneMethod != "routes" {
						fault(x, "module 1's origination is used other than set by its newGatewayState and returned by its routes")
					}
				case *ast.KeyValueExpr:
					if id, ok := x.Key.(*ast.Ident); ok && id.Name == "origination" {
						fault(x, "module 1's origination is set in a literal")
					}
				case *ast.CallExpr:
					if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "originationRoutes" && x != kept {
						fault(x, "originationRoutes is called other than to set module 1's origination in its newGatewayState")
					}
				}
				return true
			})
		}
	}
	if sets != 1 {
		faults = append(faults, fmt.Sprintf("module 1's origination is set %d times, want once (by its newGatewayState)", sets))
	}
	return faults
}

// originationCallForRole reports whether call is X.originationRoutes(Y.cfg.Role).
func originationCallForRole(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "originationRoutes" || len(call.Args) != 1 {
		return false
	}
	arg, ok := call.Args[0].(*ast.SelectorExpr)
	if !ok || arg.Sel.Name != "Role" {
		return false
	}
	cfg, ok := arg.X.(*ast.SelectorExpr)
	if !ok || cfg.Sel.Name != "cfg" {
		return false
	}
	_, ok = cfg.X.(*ast.Ident)
	return ok
}

// declarationCall is the name of the declaration e calls, when e is
// <recv>.<name>(role).
func declarationCall(e ast.Expr, recv string) (string, bool) {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	r, ok := sel.X.(*ast.Ident)
	arg, okArg := call.Args[0].(*ast.Ident)
	if !ok || r.Name != recv || !okArg || arg.Name != "role" {
		return "", false
	}
	return sel.Sel.Name, true
}

// workstreamRoutesBody is (*Gateway).workstreamRoutes' whole body: every
// compiled-in adapter's routes, in registry order, and nothing else.
const workstreamRoutesBody = `{
	for _, w := range workstreams {
		out = append(out, w.routes(g, role)...)
	}
	return out
}`

// moduleRoutesBody is (*Gateway).moduleRoutes' whole body: every compiled-in
// module's routes, in registry order, and nothing else.
const moduleRoutesBody = `{
	for _, m := range modules {
		out = append(out, m.routes(g, role)...)
	}
	return out
}`

// moduleRoutesMethodBodies are each module's whole routes method, by its
// type: the routes its state kept, and none on a gateway assembled without
// New. %s is the method's gateway parameter.
var moduleRoutesMethodBodies = map[string]string{
	"moduleOne": `{
	if st := %s.moduleOneState(); st != nil {
		return st.origination
	}
	return nil
}`,
}

// moduleRoutesMethodIs reports whether fn, a module's routes method, is
// exactly its pinned body.
func moduleRoutesMethodIs(fset *token.FileSet, fn *ast.FuncDecl) error {
	want, ok := moduleRoutesMethodBodies[receiverType(fn)]
	if !ok {
		return fmt.Errorf("of %s has no pinned body: pin it in moduleRoutesMethodBodies", receiverType(fn))
	}
	params := fn.Type.Params.List
	if len(params) != 2 || len(params[0].Names) != 1 {
		return fmt.Errorf("takes %d parameters, want the named gateway and the role", len(params))
	}
	return bodyIs(fset, fn, fmt.Sprintf(want, params[0].Names[0].Name))
}

// adapterRoutesBody is a workstream adapter's whole routes method: its
// classified declaration, called with the role. %s are the method's gateway
// and role parameters and the declaration's name.
const adapterRoutesBody = `{
	return %s.%s(%s)
}`

// adapterDeclarations are the classified declarations an adapter's routes
// method may return.
var adapterDeclarations = []string{"paRoutes", "fqsRoutes"}

// adapterNoRoutesBody is the routes body of a workstream with no
// participant-facing route (the toy test profile).
const adapterNoRoutesBody = `{
	return nil
}`

// receiverType is the base type name of fn's receiver.
func receiverType(fn *ast.FuncDecl) string {
	t := fn.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// bodyIs reports whether fn's body is exactly want, token for token (spacing
// and comments aside).
func bodyIs(fset *token.FileSet, fn *ast.FuncDecl, want string) error {
	var body strings.Builder
	if err := printer.Fprint(&body, fset, fn.Body); err != nil {
		return err
	}
	if strings.Join(strings.Fields(body.String()), " ") != strings.Join(strings.Fields(want), " ") {
		return fmt.Errorf("is not exactly:\n%s\ngot:\n%s", want, body.String())
	}
	return nil
}

// adapterRoutesIs reports whether fn, a workstream adapter's routes method,
// returns one classified declaration called with its role parameter and does
// nothing else.
func adapterRoutesIs(fset *token.FileSet, fn *ast.FuncDecl) error {
	var params []string
	for _, f := range fn.Type.Params.List {
		for _, id := range f.Names {
			params = append(params, id.Name)
		}
	}
	if len(params) != 2 {
		return fmt.Errorf("takes %d named parameters, want the gateway and the role", len(params))
	}
	// A workstream with no participant-facing routes mounts none.
	if bodyIs(fset, fn, adapterNoRoutesBody) == nil {
		return nil
	}
	var errs []error
	for _, decl := range adapterDeclarations {
		err := bodyIs(fset, fn, fmt.Sprintf(adapterRoutesBody, params[0], decl, params[1]))
		if err == nil {
			return nil
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// handlerBody is (*Gateway).Handler's whole body: it takes the routes mux and
// serves every request through it, counting the operation. Anything else it
// did would answer a path no declaration classifies.
const handlerBody = `{
	mux := g.routes()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		done := g.operations.begin()
		defer done()
		mux.ServeHTTP(w, r)
	})
}`

// moduleStatesAfterOptionsFaults: New builds every module's state once, after
// its options. In files, New's body calls moduleGatewayStates exactly once,
// in a statement of its own that comes after the loop over its options (a
// range over opts), never before it or inside it, so an option that sets
// what a module reads (its role, its origination profile) is seen.
func moduleStatesAfterOptionsFaults(fset *token.FileSet, files []*ast.File) []string {
	var faults []string
	news := 0
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, _ := decl.(*ast.FuncDecl)
			if fn == nil || fn.Recv != nil || fn.Name.Name != "New" || fn.Body == nil {
				continue
			}
			news++
			optionsAt, calls := -1, 0
			for i, stmt := range fn.Body.List {
				if rs, ok := stmt.(*ast.RangeStmt); ok {
					if id, ok := rs.X.(*ast.Ident); ok && id.Name == "opts" {
						optionsAt = i
					}
				}
				ast.Inspect(stmt, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "moduleGatewayStates" {
						calls++
						if optionsAt < 0 || i <= optionsAt {
							faults = append(faults, fset.Position(call.Pos()).String()+": New builds module state before or inside its options loop")
						}
					}
					return true
				})
			}
			if optionsAt < 0 {
				faults = append(faults, fset.Position(fn.Pos()).String()+": New has no loop over its options")
			}
			if calls != 1 {
				faults = append(faults, fmt.Sprintf("%s: New calls moduleGatewayStates %d times, want once, after its options", fset.Position(fn.Pos()), calls))
			}
		}
	}
	if news != 1 {
		faults = append(faults, fmt.Sprintf("found %d New functions, want 1", news))
	}
	return faults
}

// TestNew_BuildsModuleStateAfterOptions: the package's New builds module
// state after its options, and the fence refuses every other placement.
func TestNew_BuildsModuleStateAfterOptions(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || !inThisBuild(t, ".", name) {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	for _, f := range moduleStatesAfterOptionsFaults(fset, files) {
		t.Error(f)
	}

	faults := func(src string) []string {
		t.Helper()
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "probe.go", "package engine\n\n"+src, 0)
		if err != nil {
			t.Fatal(err)
		}
		return moduleStatesAfterOptionsFaults(fset, []*ast.File{f})
	}
	const options = "\tfor _, opt := range opts {\n\t\topt(g)\n\t}\n"
	const build = "\tif g.moduleStates, err = moduleGatewayStates(g); err != nil {\n\t\treturn nil, err\n\t}\n"
	newWith := func(body string) string {
		return "func New(cfg Config, opts ...Option) (*Gateway, error) {\n\tg := &Gateway{cfg: cfg}\n\tvar err error\n" + body + "\treturn g, nil\n}\n"
	}
	if f := faults(newWith(options + build)); len(f) != 0 {
		t.Fatalf("New building module state after its options is refused: %v", f)
	}
	for name, row := range map[string]struct{ src, want string }{
		"before the options":      {newWith(build + options), "before or inside its options loop"},
		"inside the options loop": {newWith("\tfor _, opt := range opts {\n\t\topt(g)\n\t\tg.moduleStates, err = moduleGatewayStates(g)\n\t}\n"), "before or inside its options loop"},
		"no options loop":         {newWith(build), "no loop over its options"},
		"never":                   {newWith(options), "calls moduleGatewayStates 0 times"},
		"twice":                   {newWith(build + options + build), "calls moduleGatewayStates 2 times"},
	} {
		got := faults(row.src)
		if !slices.ContainsFunc(got, func(f string) bool { return strings.Contains(f, row.want) }) {
			t.Errorf("%s: faults %v, want one containing %q", name, got, row.want)
		}
	}
}

// TestRouteFence_OriginationIsKeptOnlyByNew: originationKeptFaults accepts
// module 1's newGatewayState, which New runs once, keeping on-behalf
// building's routes for the configured role and its routes method returning
// them, and refuses every other way of setting or declaring them.
func TestRouteFence_OriginationIsKeptOnlyByNew(t *testing.T) {
	faults := func(src string) []string {
		t.Helper()
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "probe.go", "package engine\n\n"+src, 0)
		if err != nil {
			t.Fatal(err)
		}
		return originationKeptFaults(fset, []*ast.File{f})
	}
	const keptByState = "func (moduleOne) newGatewayState(g *Gateway) (gatewayState, error) {\n\tst := &moduleOneState{}\n\tvar err error\n\tif st.origination, err = g.originationRoutes(g.cfg.Role); err != nil {\n\t\treturn nil, err\n\t}\n\treturn st, nil\n}\n"
	if f := faults(keptByState); len(f) != 0 {
		t.Fatalf("module 1's state keeping the routes for the configured role is refused: %v", f)
	}
	const routes = "func (moduleOne) routes(g *Gateway, _ string) []mountedRoute {\n\tif st := g.moduleOneState(); st != nil {\n\t\treturn st.origination\n\t}\n\treturn nil\n}\n"
	if f := faults(keptByState + routes); len(f) != 0 {
		t.Fatalf("module 1's routes returning the kept routes is refused: %v", f)
	}
	const keep = "if st.origination, err = g.originationRoutes(g.cfg.Role); err != nil {\n\t\treturn nil, err\n\t}"
	for name, row := range map[string]struct{ src, want string }{
		"the state keeps none":                {"func (moduleOne) newGatewayState(g *Gateway) (gatewayState, error) { return &moduleOneState{}, nil }\n", "set 0 times"},
		"the state keeps another role's":      {strings.Replace(keptByState, "g.originationRoutes(g.cfg.Role)", `g.originationRoutes("provider")`, 1), "set other than by its newGatewayState"},
		"the state keeps another declaration": {strings.Replace(keptByState, "g.originationRoutes(g.cfg.Role)", "g.coreRoutes(g.cfg.Role)", 1), "set other than by its newGatewayState"},
		"the state keeps them twice":          {strings.Replace(keptByState, "\treturn st, nil", "\tst.origination, err = g.originationRoutes(g.cfg.Role)\n\treturn st, nil", 1), "set 2 times"},
		"the state drops the refusal":         {strings.Replace(keptByState, keep, "st.origination, _ = g.originationRoutes(g.cfg.Role)", 1), "drops originationRoutes' refusal"},
		"the state sets them from one value":  {strings.Replace(keptByState, keep, "st.origination = nil", 1), "set other than by its newGatewayState"},
		"another type's state keeps them":     {strings.Replace(keptByState, "func (moduleOne)", "func (paWorkstream)", 1), "set other than by its newGatewayState"},
		"a method resets them":                {keptByState + "func (g *Gateway) reset() { g.moduleOneState().origination, _ = g.originationRoutes(g.cfg.Role) }\n", "set other than by its newGatewayState"},
		"a method clears them":                {keptByState + "func (g *Gateway) clear() { g.moduleOneState().origination = nil }\n", "set other than by its newGatewayState"},
		"a method edits one":                  {keptByState + "func (g *Gateway) open() { g.moduleOneState().origination[0].operator = false }\n", "used other than set by its newGatewayState and returned by its routes"},
		"a method hands them on":              {keptByState + "func (g *Gateway) more() []mountedRoute { return g.moduleOneState().origination }\n", "used other than set by its newGatewayState and returned by its routes"},
		"routes edits one":                    {keptByState + "func (moduleOne) routes(g *Gateway, _ string) []mountedRoute {\n\tst := g.moduleOneState()\n\tst.origination[0].handler = nil\n\treturn st.origination\n}\n", "used other than set by its newGatewayState and returned by its routes"},
		"a literal sets them":                 {keptByState + "func other() *moduleOneState { return &moduleOneState{origination: nil} }\n", "set in a literal"},
		"routes declares them again":          {keptByState + "func (moduleOne) routes(g *Gateway, role string) []mountedRoute { r, _ := g.originationRoutes(role); return r }\n", "called other than to set"},
	} {
		got := faults(row.src)
		if !slices.ContainsFunc(got, func(f string) bool { return strings.Contains(f, row.want) }) {
			t.Errorf("%s: faults %v, want one containing %q", name, got, row.want)
		}
	}
}

// TestRouteFence_RegistryBodiesArePinned: the registry and an adapter's routes
// method pass the fence only exactly as pinned. A body that gates the routes
// on gateway state, rewrites a pattern or swaps a handler is refused, whatever
// conditions the behavioural walk drives.
func TestRouteFence_RegistryBodiesArePinned(t *testing.T) {
	parse := func(src string) *ast.FuncDecl {
		t.Helper()
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "probe.go", "package engine\n\n"+src, 0)
		if err != nil {
			t.Fatal(err)
		}
		return f.Decls[0].(*ast.FuncDecl)
	}
	fset := token.NewFileSet()
	registry := func(body string) error {
		return bodyIs(fset, parse("func (g *Gateway) workstreamRoutes(role string) (out []mountedRoute) "+body), workstreamRoutesBody)
	}
	adapter := func(body string) error {
		return adapterRoutesIs(fset, parse("func (paWorkstream) routes(g *Gateway, role string) []mountedRoute "+body))
	}
	moduleRegistry := func(body string) error {
		return bodyIs(fset, parse("func (g *Gateway) moduleRoutes(role string) (out []mountedRoute) "+body), moduleRoutesBody)
	}
	module := func(recv, body string) error {
		return moduleRoutesMethodIs(fset, parse("func ("+recv+") routes(gw *Gateway, _ string) []mountedRoute "+body))
	}
	if err := moduleRegistry("{\n\t// every module\n\tfor _, m := range modules { out = append(out, m.routes(g, role)...) }\n\treturn out\n}"); err != nil {
		t.Errorf("the pinned module registry body, respaced and commented, is refused: %v", err)
	}
	if err := module("moduleOne", "{ if st := gw.moduleOneState(); st != nil { return st.origination }; return nil }"); err != nil {
		t.Errorf("module 1's pinned routes body is refused: %v", err)
	}
	if err := registry("{\n\t// every adapter\n\tfor _, w := range workstreams { out = append(out, w.routes(g, role)...) }\n\treturn out\n}"); err != nil {
		t.Errorf("the pinned registry body, respaced and commented, is refused: %v", err)
	}
	if err := adapter("{ return g.paRoutes(role) }"); err != nil {
		t.Errorf("the pinned adapter body is refused: %v", err)
	}
	for name, err := range map[string]error{
		"the registry swaps a handler": registry(`{
	for _, w := range workstreams { out = append(out, w.routes(g, role)...) }
	for i := range out { out[i].handler = g.handleCDSDiscovery }
	return out
}`),
		"the registry gates on gateway state": registry(`{
	if g.cfg.HubAcceptsInvolved { return nil }
	for _, w := range workstreams { out = append(out, w.routes(g, role)...) }
	return out
}`),
		"the registry skips an adapter": registry(`{
	for _, w := range workstreams[1:] { out = append(out, w.routes(g, role)...) }
	return out
}`),
		"the adapter gates on gateway state": adapter(`{
	if g.cfg.HubAcceptsInvolved { return nil }
	return g.paRoutes(role)
}`),
		"the adapter rewrites a pattern": adapter(`{
	r := g.paRoutes(role)
	if g.cfg.HubAcceptsInvolved { r[0].pattern = "GET /evil" }
	return r
}`),
		"the adapter returns another declaration": adapter(`{ r, _ := g.originationRoutes(role); return r }`),
		"the adapter passes another role":         adapter(`{ return g.paRoutes("provider") }`),
		"the module registry swaps a handler": moduleRegistry(`{
	for _, m := range modules { out = append(out, m.routes(g, role)...) }
	for i := range out { out[i].handler = g.handleCDSDiscovery }
	return out
}`),
		"the module registry gates on gateway state": moduleRegistry(`{
	if g.cfg.HubAcceptsInvolved { return nil }
	for _, m := range modules { out = append(out, m.routes(g, role)...) }
	return out
}`),
		"the module registry skips a module": moduleRegistry(`{
	for _, m := range modules[1:] { out = append(out, m.routes(g, role)...) }
	return out
}`),
		"module 1 gates on gateway state": module("moduleOne", `{
	if st := gw.moduleOneState(); st != nil && !gw.cfg.HubAcceptsInvolved { return st.origination }
	return nil
}`),
		"module 1 declares its routes again": module("moduleOne", `{ r, _ := gw.originationRoutes(gw.cfg.Role); return r }`),
		"module 1 drops the operator surface": module("moduleOne", `{
	if st := gw.moduleOneState(); st != nil { for i := range st.origination { st.origination[i].operator = false }; return st.origination }
	return nil
}`),
		"a module with no pinned body": module("moduleTwo", "{ return nil }"),
	} {
		if err == nil {
			t.Errorf("%s: the fence accepts it", name)
		}
	}
}

// TestRouteFence_OriginationTableIsRead: originationTableFaults reads module
// 1's static route table and its exchangeRoutes method exactly as pinned, and
// refuses every way of declaring a row it cannot classify, of naming an
// exchange route other than an originating row's, or of using the table
// elsewhere; originationRoutes passes the fence only as originationRoutesBody,
// so a local row cannot record its exchange and an originating row cannot skip
// it.
func TestRouteFence_OriginationTableIsRead(t *testing.T) {
	read := func(src string) ([]declaredRoute, []string) {
		t.Helper()
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "probe.go", "package engine\n\n"+src, 0)
		if err != nil {
			t.Fatal(err)
		}
		return originationTableFaults(fset, []*ast.File{f})
	}
	const table = "var originationRouteTable = []originationRoute{\n" +
		"\t{pattern: \"POST /scenario/uc01\", originates: true, handler: (*Gateway).handleScenario},\n" +
		"\t{pattern: \"POST /scenario/reset\", originates: false, handler: (*Gateway).handleScenarioReset},\n" +
		"}\n"
	const exchangeRoutes = "func (moduleOne) exchangeRoutes() []string " + moduleOneExchangeRoutesBody + "\n"
	const mount = "func (g *Gateway) originationRoutes(role string) ([]mountedRoute, error) " + originationRoutesBody + "\n"
	got, faults := read(table + exchangeRoutes + mount)
	if len(faults) != 0 {
		t.Fatalf("the pinned table, mount and exchangeRoutes are refused: %v", faults)
	}
	if len(got) != 2 || got[0].pattern != "POST /scenario/uc01" || !got[0].originates || !got[0].operator || got[1].originates || !got[1].operator {
		t.Fatalf("read %+v, want uc01 originating and reset local, both the operator surface", got)
	}
	row := func(old, new string) string { return strings.Replace(table, old, new, 1) + exchangeRoutes }
	for name, r := range map[string]struct{ src, want string }{
		"a pattern from a constant":      {row(`pattern: "POST /scenario/uc01"`, "pattern: uc01Pattern"), "not a route pattern literal"},
		"a string that is not a pattern": {row(`"POST /scenario/uc01"`, `"scenario-uc01"`), "not a route pattern literal"},
		"originates from a variable":     {row("originates: true", "originates: recorded"), "originates other than as a literal"},
		"a handler closure":              {row("handler: (*Gateway).handleScenario", "handler: func(g *Gateway, w http.ResponseWriter, r *http.Request) {}"), "not a method of the gateway"},
		"another type's method":          {row("(*Gateway).handleScenario", "(*nativeResponder).handleScenario"), "not a method of the gateway"},
		"a row with another field":       {row("originates: false,", "originates: false, records: true,"), "names records"},
		"a row without originates":       {row("originates: true, ", ""), "does not name its pattern, originates and handler"},
		"a row without field names":      {row(`{pattern: "POST /scenario/uc01", originates: true, handler: (*Gateway).handleScenario}`, `{"POST /scenario/uc01", true, (*Gateway).handleScenario}`), "not written with field names"},
		"a table of another type":        {strings.Replace(table, "[]originationRoute{", "[]mountedRoute{", 1) + exchangeRoutes, "not a []originationRoute literal"},
		"no table":                       {exchangeRoutes, "found 0 originationRouteTable"},
		"two tables":                     {table + table + exchangeRoutes, "found 2 originationRouteTable"},
		"a method edits a row":           {table + exchangeRoutes + "func (g *Gateway) open() { originationRouteTable[1].originates = true }\n", "named other than by originationRoutes"},
		"a function appends a row":       {table + exchangeRoutes + "func init() { originationRouteTable = append(originationRouteTable, originationRoute{}) }\n", "named other than by originationRoutes"},
		"no exchangeRoutes":              {table, "found 0 moduleOne.exchangeRoutes"},
		"exchangeRoutes names every row": {table + "func (moduleOne) exchangeRoutes() (out []string) {\n\tfor _, rt := range originationRouteTable {\n\t\tout = append(out, originationExchangeRoute(rt.pattern))\n\t}\n\treturn out\n}\n", "moduleOne.exchangeRoutes is not exactly"},
	} {
		_, faults := read(r.src)
		if !slices.ContainsFunc(faults, func(f string) bool { return strings.Contains(f, r.want) }) {
			t.Errorf("%s: faults %v, want one containing %q", name, faults, r.want)
		}
	}

	parse := func(body string) *ast.FuncDecl {
		t.Helper()
		f, err := parser.ParseFile(token.NewFileSet(), "probe.go", "package engine\n\nfunc (g *Gateway) originationRoutes(role string) ([]mountedRoute, error) "+body, 0)
		if err != nil {
			t.Fatal(err)
		}
		return f.Decls[0].(*ast.FuncDecl)
	}
	fset := token.NewFileSet()
	if err := bodyIs(fset, parse(originationRoutesBody), originationRoutesBody); err != nil {
		t.Fatalf("the pinned originationRoutes body is refused: %v", err)
	}
	const recorded = "out = append(out, mountedRoute{pattern: rt.pattern, handler: g.readsClaimFacts(h), operator: true, exchangeRoute: originationExchangeRoute(rt.pattern), credential: carriesCredential})"
	const local = "out = append(out, mountedRoute{pattern: rt.pattern, handler: h, operator: true, credential: carriesCredential})"
	for name, body := range map[string]string{
		"a local row records its exchange":   strings.Replace(originationRoutesBody, local, "out = append(out, mountedRoute{pattern: rt.pattern, handler: h, operator: true, exchangeRoute: originationExchangeRoute(rt.pattern), credential: carriesCredential})", 1),
		"an originating row records nothing": strings.Replace(originationRoutesBody, recorded, "out = append(out, mountedRoute{pattern: rt.pattern, handler: g.readsClaimFacts(h), operator: true, credential: carriesCredential})", 1),
		"an originating row names its own":   strings.Replace(originationRoutesBody, "exchangeRoute: originationExchangeRoute(rt.pattern)", `exchangeRoute: "other"`, 1),
		"an originating row reads no facts":  strings.Replace(originationRoutesBody, "handler: g.readsClaimFacts(h)", "handler: h", 1),
		"a row leaves the operator surface":  strings.Replace(originationRoutesBody, "operator: true, credential: carriesCredential})\n\t\t\tcontinue", "operator: false, credential: carriesCredential})\n\t\t\tcontinue", 1),
		"the mount gates on gateway state":   strings.Replace(originationRoutesBody, `role != "provider" ||`, `role != "provider" || g.cfg.HubAcceptsInvolved ||`, 1),
		"the mount skips the profile check":  strings.Replace(originationRoutesBody, "if err := checkOriginationProfile(g.cfg.OriginationProfile); err != nil {\n\t\treturn nil, err\n\t}\n", "", 1),
	} {
		if body == originationRoutesBody {
			t.Fatalf("%s: the probe does not change the pinned body", name)
		}
		if err := bodyIs(fset, parse(body), originationRoutesBody); err == nil {
			t.Errorf("%s: the fence accepts it", name)
		}
	}
}
