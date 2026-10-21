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
			rec := serve(g, "POST /scenario/uc03", testRouteBody(g, "/scenario/uc03", ""))
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
	"POST /CoverageEligibilityRequest/$submit",
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
//     Federated Query Services adapter's fqsRoutes) and originationRoutes,
//     each called with the role, and mountedRoute is named nowhere but its
//     type, routes, the declarations coreRoutes, paRoutes, fqsRoutes and
//     originationRoutes, and the signatures of workstreamRoutes, the workstream
//     interface's routes method and its adapters' routes methods; and the
//     bodies of workstreamRoutes and of each adapter's routes method are
//     exactly workstreamRoutesBody and adapterRoutesBody, so the registry
//     decides nothing about which routes mount;
//   - requireOriginationProfile is called once in the package, by routes, on
//     the routes declared as originating, and requireScenarioCaller once, by
//     routes, on the routes declared as the operator surface;
//   - in a declaration every route pattern is a string literal, given to a
//     local closure or as a mountedRoute's pattern, and it is declared
//     originating (through a closure that builds originating routes, or
//     originates: true) exactly when it is classified as originating, and
//     declared the operator surface (operator: true) exactly when on-behalf
//     building declares it; a declaration names no other string but a role
//     and "", and reads no gateway state but the conditions routeConditions
//     drives (routeGates).
//
// By running every declaration for every role, under every condition that
// gates a route: every originating route is declared originating and no other
// route is, every /scenario route and no other is mounted behind
// requireScenarioCaller, a route a role mounts is declared once, each piece
// declares only its own routes (the core its inbound, on-behalf building
// /scenario/*, the workstream adapters the rest), and every pattern a
// piece's source names is mounted by that piece under some driven condition,
// so a route under a condition this test does not drive fails it. A new route, however
// it is written, cannot join any of these sets silently.
func TestScenarioRoutes_EveryRouteIsClassified(t *testing.T) {
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

	declarations := map[string]string{"coreRoutes": "core", "paRoutes": "pa", "fqsRoutes": "fqs", "originationRoutes": "origination"}
	// mountable are what routes mounts: the core's and on-behalf building's
	// declarations, and the workstream registry's routes.
	mountable := map[string]bool{"coreRoutes": true, "workstreamRoutes": true, "originationRoutes": true}
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
	var literals []declaredRoute
	routesFuncs, handlers, muxRegistrations, mountLists, registryBodies, adapterBodies := 0, 0, 0, 0, 0, 0
	guards := map[string]int{}
	mounted := map[string]bool{}
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
			// signature is where mountedRoute may be named as a type alone: the
			// registry's routes and the workstream adapters' routes methods,
			// whose bodies are pinned.
			var signature ast.Node
			adapterRoutes := fn != nil && fn.Name.Name == "routes" && fn.Recv != nil && len(fn.Recv.List) == 1 && receiverType(fn) != "Gateway"
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
					if x.Name.Name == "workstream" {
						// The adapter interface may name mountedRoute in its
						// routes method's signature alone.
						if it, ok := x.Type.(*ast.InterfaceType); ok {
							for _, m := range it.Methods.List {
								if len(m.Names) == 1 && m.Names[0].Name == "routes" {
									continue
								}
								ast.Inspect(m.Type, func(n ast.Node) bool {
									if id, ok := n.(*ast.Ident); ok && id.Name == "mountedRoute" {
										t.Errorf("%s: mountedRoute is named in a workstream method other than routes", fset.Position(id.Pos()))
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
					if x.Name == "mountedRoute" && method != "routes" && declarations[method] == "" && !inSignature {
						t.Errorf("%s: mountedRoute is named outside its type, routes and the route declarations", fset.Position(x.Pos()))
					}
				case *ast.SelectorExpr:
					if x.Sel.Name == "requireOriginationProfile" || x.Sel.Name == "requireScenarioCaller" {
						if call, isCall := stack[len(stack)-2].(*ast.CallExpr); !isCall || call.Fun != x || method != "routes" {
							t.Errorf("%s: %s is used outside routes", fset.Position(x.Pos()), x.Sel.Name)
						}
						guards[x.Sel.Name]++
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
	if registryBodies != 1 || adapterBodies != len(workstreams) {
		t.Errorf("found %d workstreamRoutes and %d adapter routes methods, want 1 and %d (one per compiled-in workstream)", registryBodies, adapterBodies, len(workstreams))
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
	for _, g := range []string{"requireOriginationProfile", "requireScenarioCaller"} {
		if guards[g] != 1 {
			t.Errorf("%s is called %d times in the package, want once (by routes)", g, guards[g])
		}
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

	want := map[string]bool{} // "role pattern" -> originates
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
	produced := map[string]bool{} // "piece pattern"
	for _, role := range []string{"provider", "payer", "facility", "phg"} {
		for _, c := range everyRouteCondition() {
			g := routeGateway(role, c)
			// The registry mounts exactly the adapters' declarations, in
			// registry order: PA's, then Federated Query Services'.
			if reg, adapters := g.workstreamRoutes(role), slices.Concat(g.paRoutes(role), g.fqsRoutes(role)); !sameRoutes(reg, adapters) {
				t.Errorf("%s under %s: the workstream registry mounts %v, the adapters declare %v", role, c, routePatterns(reg), routePatterns(adapters))
			}
			seen := map[string]string{}
			for piece, declared := range map[string][]mountedRoute{
				"core": g.coreRoutes(role), "pa": g.paRoutes(role), "fqs": g.fqsRoutes(role), "origination": g.originationRoutes(role),
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
					if prev, ok := got[key]; ok && prev != rt.originates {
						t.Errorf("%s is originating under one condition and not under another", key)
					}
					got[key] = rt.originates
				}
			}
		}
	}
	for key, originates := range got {
		w, listed := want[key]
		switch {
		case !listed:
			t.Errorf("%s is declared but not classified (add it to a route list here)", key)
		case w && !originates:
			t.Errorf("%s originates but is not declared originating (mounted behind requireOriginationProfile)", key)
		case !w && originates:
			t.Errorf("%s does not originate but is declared originating", key)
		}
	}
	for key := range want {
		if _, ok := got[key]; !ok {
			t.Errorf("%s is classified but no declaration mounts it", key)
		}
	}
	for _, l := range literals {
		if !produced[l.piece+" "+l.pattern] {
			t.Errorf("%s: %s declares %s under a role or condition this test does not drive, or this test does not run the %s declaration", l.pos, l.piece, l.pattern, l.piece)
		}
	}
}

// sameRoutes reports whether a and b declare the same patterns, in order, the
// same ones originating and the same ones the operator surface.
func sameRoutes(a, b []mountedRoute) bool {
	return slices.EqualFunc(a, b, func(x, y mountedRoute) bool {
		return x.pattern == y.pattern && x.originates == y.originates && x.operator == y.operator && (x.handler == nil) == (y.handler == nil)
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
	routeGates      = map[string]bool{"cfg.IngressEnabled": true, "cfg.IngressBaseURL": true, "ingressAuth": true}
	routePatternLit = regexp.MustCompile(`^(GET|POST|PUT|PATCH|DELETE) /`)
)

// readRouteDeclaration reads the route patterns a declaration's source names,
// failing t on anything the classifier cannot read: a pattern that is not a
// string literal, an originates or operator value that is not a literal, a
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
	// routeLiteral reads one mountedRoute literal: its originates and operator
	// values, and its pattern, which is param inside a closure and a literal elsewhere.
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
			case key.Name == "originates" || key.Name == "operator":
				id, ok := kv.Value.(*ast.Ident)
				if !ok || (id.Name != "true" && id.Name != "false") {
					t.Errorf("%s: %s declares %s other than as a literal", at(kv.Value), fn.Name.Name, key.Name)
				}
				if key.Name == "originates" {
					flags.originates = ok && id.Name == "true"
				} else {
					flags.operator = ok && id.Name == "true"
				}
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
		"the adapter returns another declaration": adapter(`{ return g.originationRoutes(role) }`),
		"the adapter passes another role":         adapter(`{ return g.paRoutes("provider") }`),
	} {
		if err == nil {
			t.Errorf("%s: the fence accepts it", name)
		}
	}
}
