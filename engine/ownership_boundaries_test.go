package engine

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The transmit boundaries of this package. Every function that sends bytes
// — through an *http.Client, an http.RoundTripper or an
// http.ResponseWriter, or by handing a client or a writer to another
// package — is listed here, and a new one fails the test until it is listed.
// A listed transmit calls relay.Transmit itself, so what it sends has been
// checked against the leg ownership table; the runtime check is the real
// guard, and this test keeps every send routed through it.
//
// Known limits: the check follows static calls only. It does not see a send
// made through a writer converted to io.Writer (or stored in an interface or
// struct field), through a method value, or through a func-typed parameter.
// Such code fails the runtime check only if it sends a payload the table does
// not permit; it cannot send one without relay.Transmit.

type boundaryKind int

const (
	// transmits: the function calls relay.Transmit.
	transmits boundaryKind = iota + 1
	// sealsVia: the function writes an envelope it got from via, a listed
	// transmit.
	sealsVia
	// excluded: the function sends something that is not an exchange
	// message; reason says what.
	excluded
)

type boundaryRule struct {
	kind   boundaryKind
	via    string
	reason string
	// callers, when set, are the only functions that may call this one; a
	// call to it counts as a send in the caller.
	callers []string
	// noSink marks a listed transmit that sends nothing itself (it builds
	// what its callers send).
	noSink bool
}

const engineImportPath = "github.com/SmartHealthNetwork/shn-gateway/engine"

var transmitBoundaries = map[string]boundaryRule{
	// Requests to the network, and the envelope that carries them.
	"Gateway.roundTripInner": {kind: transmits, noSink: true},
	"Gateway.postEnvelope": {kind: excluded, callers: []string{"Gateway.roundTripInner"},
		reason: "posts the sealed envelope roundTripInner built from its checked request"},
	// Requests to the participant's own system.
	"nativeResponder.post": {kind: transmits},
	// Answers to the network.
	"Gateway.buildResponseLeg": {kind: transmits, noSink: true},
	"writeLeg": {kind: excluded,
		callers: []string{"Gateway.respondLeg", "Gateway.respondLegError"},
		reason:  "writes the sealed envelope buildResponseLeg built from its checked answer"},
	"Gateway.respondLeg":      {kind: sealsVia, via: "Gateway.buildResponseLeg"},
	"Gateway.respondLegError": {kind: sealsVia, via: "Gateway.buildResponseLeg"},
	// Answers to the participant's own system (the ingress writers, the
	// relayed application errors) and every refusal.
	"Gateway.writePayload": {kind: transmits},
	"writeJSON":            {kind: transmits},
	// The engine's own reads of a payload before its transmit.
	"Gateway.admit": {kind: transmits, noSink: true},

	// Sends that are not exchange messages.
	"FetchConformanceStatus": {kind: excluded,
		reason: "a bodyless operational health read of the authorized participant gateway; only bounded allowlisted metadata is returned"},
	"Gateway.observeIngress": {kind: excluded, reason: "transparent HTTP observation delegates to the original ingress handler; no payload is authored or transmitted here"},
	"Gateway.observeInbound": {kind: excluded, reason: "transparent HTTP observation delegates to the independently checked inbound handler; no payload is authored or transmitted here"},
	"writeLocalJSON": {kind: excluded,
		reason: "a successful local API answer (a scenario or console summary built from decoded values)"},
	"Gateway.authorize": {kind: excluded,
		reason: "the authorization request: frame, operation, subject and payload hash, never the payload"},
	"Gateway.auditPatientAccess": {kind: excluded,
		reason: "the Patient Access audit record: metadata only"},
	"Gateway.consentBackstop": {kind: excluded,
		reason: "the consent check request: subject, purpose, custodian and recipient, never the payload"},
	"DiscoverCDSServices": {kind: excluded,
		reason: "a CDS Services discovery read that sends no body"},
	"nativePopulator.post": {kind: excluded,
		reason: "a call to the participant's own pre-population service with a request this gateway builds; no peer is on the wire"},
	"Gateway.handleUC08": {kind: excluded,
		reason: "the scenario's read of the patient view, which sends no body"},
	"Gateway.handleIngressMetadata": {kind: excluded,
		reason: "this gateway's own CapabilityStatement"},
	"Gateway.handlePatientAccessMetadata": {kind: excluded,
		reason: "this gateway's own Patient Access CapabilityStatement"},
	"Gateway.handleCDSDiscovery": {kind: excluded,
		reason: "this gateway's own CDS Services discovery document"},
	"Gateway.handleDavinciConfiguration": {kind: excluded,
		reason: "this gateway's own Da Vinci configuration document"},
	"oauthErr": {kind: excluded,
		reason: "an error from the ingress authorization server"},
	"ingressAuthServer.handleToken": {kind: excluded,
		reason: "the ingress authorization server's token response"},
	"ingressAuthServer.handleSmartConfig": {kind: excluded,
		reason: "the ingress authorization server's SMART configuration"},
	"Gateway.serveEOB": {kind: excluded,
		reason: "the payer's own records to a patient app on the token-gated Patient Access API; no network leg"},
}

// writerUsersThatSendNothing are functions of other packages that take a
// response writer without writing to it.
var writerUsersThatSendNothing = map[string]string{
	"net/http.ServeMux.ServeHTTP":                          "the router dispatches to this package's own handlers, which are checked themselves",
	"github.com/SmartHealthNetwork/shn-sdk.DecodeJSONBody": "reads the request body; the writer only caps its size",
}

// unestablishedLegScopes are the only functions that name the refusal row a
// transmit uses before a leg is known.
var unestablishedLegScopes = []string{"Gateway.withScope", "refusalKey"}

// typedPackage type-checks the given sources against this module's
// compiled dependencies.
func typedPackage(t *testing.T, fset *token.FileSet, files []*ast.File) *types.Info {
	t.Helper()
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal("go tool not available: the boundary check cannot type the package")
	}
	out, err := exec.Command(gobin, "list", "-export", "-deps", "-f", "{{.ImportPath}}={{.Export}}", ".").Output()
	if err != nil {
		t.Fatalf("go list -export: %v", err)
	}
	exports := map[string]string{}
	for _, l := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok && v != "" {
			exports[k] = v
		}
	}
	lookup := func(path string) (io.ReadCloser, error) {
		f, ok := exports[path]
		if !ok {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(f)
	}
	info := &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Uses:       map[*ast.Ident]types.Object{},
		Defs:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	conf := types.Config{Importer: importer.ForCompiler(fset, "gc", lookup)}
	if _, err := conf.Check(engineImportPath, fset, files, info); err != nil {
		t.Fatalf("type-check: %v", err)
	}
	return info
}

// qualifiedName names fn as path.Func or path.Type.Method.
func qualifiedName(fn *types.Func) string {
	name := fn.Name()
	if sig, ok := fn.Type().(*types.Signature); ok && sig.Recv() != nil {
		rt := sig.Recv().Type()
		if p, ok := rt.(*types.Pointer); ok {
			rt = p.Elem()
		}
		if n, ok := rt.(*types.Named); ok {
			name = n.Obj().Name() + "." + name
		}
	}
	return fn.Pkg().Path() + "." + name
}

func funcKey(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	typ := fd.Recv.List[0].Type
	if s, ok := typ.(*ast.StarExpr); ok {
		typ = s.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return "?." + fd.Name.Name
}

// namedLookup finds an exported type from an imported package.
func namedLookup(pkgs []*types.Package, path, name string) types.Type {
	for _, p := range pkgs {
		if p.Path() == path {
			if o := p.Scope().Lookup(name); o != nil {
				return o.Type()
			}
		}
	}
	return nil
}

func importedPackages(info *types.Info) []*types.Package {
	seen := map[*types.Package]bool{}
	var out []*types.Package
	var add func(*types.Package)
	add = func(p *types.Package) {
		if p == nil || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
		for _, q := range p.Imports() {
			add(q)
		}
	}
	for _, o := range info.Uses {
		if o != nil {
			add(o.Pkg())
		}
	}
	return out
}

// checkTransmitBoundaries reports every way the package's functions depart
// from rules.
func checkTransmitBoundaries(fset *token.FileSet, files []*ast.File, info *types.Info, rules map[string]boundaryRule, unestablished []string) []string {
	pkgs := importedPackages(info)
	var writerIface, tripperIface *types.Interface
	if t := namedLookup(pkgs, "net/http", "ResponseWriter"); t != nil {
		writerIface, _ = t.Underlying().(*types.Interface)
	}
	if t := namedLookup(pkgs, "net/http", "RoundTripper"); t != nil {
		tripperIface, _ = t.Underlying().(*types.Interface)
	}
	isWriter := func(t types.Type) bool {
		return t != nil && writerIface != nil && (types.Implements(t, writerIface) || types.Implements(types.NewPointer(t), writerIface))
	}
	isClient := func(t types.Type) bool {
		if p, ok := t.(*types.Pointer); ok {
			t = p.Elem()
		}
		n, ok := t.(*types.Named)
		return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == "net/http" && n.Obj().Name() == "Client"
	}

	var problems []string
	sinks := map[string][]string{}
	transmitCalls := map[string]bool{}
	calls := map[string]map[string]bool{}
	var unestablishedUse []string
	defined := map[string]bool{}

	// Every scope that can hold code is scanned: function bodies, and the
	// initializers of package-level declarations (a function literal assigned
	// to a package variable, or any call made while initializing one).
	type scope struct {
		key  string
		node ast.Node
	}
	var scopes []scope
	for _, f := range files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Body != nil {
					scopes = append(scopes, scope{funcKey(d), d.Body})
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok || len(vs.Values) == 0 {
						continue
					}
					names := make([]string, len(vs.Names))
					for i, n := range vs.Names {
						names[i] = n.Name
					}
					for _, v := range vs.Values {
						scopes = append(scopes, scope{"var " + strings.Join(names, ","), v})
					}
				}
			}
		}
	}
	{
		for _, sc := range scopes {
			key := sc.key
			defined[key] = true
			if calls[key] == nil {
				calls[key] = map[string]bool{}
			}
			sink := func(n ast.Node, what string) {
				sinks[key] = append(sinks[key], fmt.Sprintf("%s (%s)", what, fset.Position(n.Pos())))
			}
			ast.Inspect(sc.node, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					if o, ok := info.Uses[sel.Sel].(*types.Const); ok && o.Pkg() != nil && strings.HasSuffix(o.Pkg().Path(), "/engine/relay") && o.Name() == "LegUnestablished" {
						unestablishedUse = append(unestablishedUse, key)
					}
				}
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var obj types.Object
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					if s := info.Selections[fun]; s != nil {
						obj = s.Obj()
						recv := s.Recv()
						name := obj.Name()
						switch {
						case isClient(recv) && (name == "Do" || name == "Get" || name == "Post" || name == "PostForm" || name == "Head"):
							sink(call, "http client "+name)
						case name == "RoundTrip" && tripperIface != nil && types.Implements(recv, tripperIface):
							sink(call, "round trip")
						case (name == "Write" || name == "WriteString" || name == "ReadFrom") && isWriter(recv):
							sink(call, "response "+name)
						}
					} else {
						obj = info.Uses[fun.Sel]
					}
				case *ast.Ident:
					obj = info.Uses[fun]
				}
				fn, ok := obj.(*types.Func)
				if !ok || fn.Pkg() == nil {
					return true
				}
				if sig, ok := fn.Type().(*types.Signature); ok && sig.Recv() == nil && fn.Pkg().Path() == "net/http" {
					switch fn.Name() {
					case "Get", "Post", "PostForm", "Head":
						sink(call, "http."+fn.Name())
					}
				}
				if strings.HasSuffix(fn.Pkg().Path(), "/engine/relay") && fn.Name() == "Transmit" {
					transmitCalls[key] = true
				}
				if fn.Pkg().Path() == engineImportPath {
					callee := strings.TrimPrefix(qualifiedName(fn), engineImportPath+".")
					calls[key][callee] = true
					if r, ok := rules[callee]; ok && len(r.callers) > 0 {
						sink(call, "call to "+callee)
					}
					return true
				}
				// Another package handed a client or a writer sends on our
				// behalf.
				for _, a := range call.Args {
					at := info.TypeOf(a)
					switch {
					case at != nil && isClient(at):
						sink(call, "client handed to "+fn.Pkg().Name()+"."+fn.Name())
					case at != nil && isWriter(at):
						if _, ok := writerUsersThatSendNothing[qualifiedName(fn)]; !ok {
							sink(call, "writer handed to "+fn.Pkg().Name()+"."+fn.Name())
						}
					}
				}
				return true
			})
		}
	}

	keys := make([]string, 0, len(sinks))
	for k := range sinks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, ok := rules[k]; !ok {
			problems = append(problems, fmt.Sprintf("%s sends bytes outside the listed transmit boundaries: %s", k, strings.Join(sinks[k], "; ")))
		}
	}
	ruleKeys := make([]string, 0, len(rules))
	for k := range rules {
		ruleKeys = append(ruleKeys, k)
	}
	sort.Strings(ruleKeys)
	for _, k := range ruleKeys {
		r := rules[k]
		if !defined[k] {
			problems = append(problems, fmt.Sprintf("%s is listed but not defined", k))
			continue
		}
		if len(sinks[k]) == 0 && !r.noSink {
			problems = append(problems, fmt.Sprintf("%s is listed but sends nothing; remove it", k))
		}
		switch r.kind {
		case transmits:
			if !transmitCalls[k] {
				problems = append(problems, fmt.Sprintf("%s is a transmit boundary but does not call relay.Transmit", k))
			}
		case sealsVia:
			if v, ok := rules[r.via]; !ok || v.kind != transmits {
				problems = append(problems, fmt.Sprintf("%s seals via %s, which is not a listed transmit", k, r.via))
			} else if !calls[k][r.via] {
				problems = append(problems, fmt.Sprintf("%s writes an envelope but does not call %s", k, r.via))
			}
		case excluded:
			if r.reason == "" {
				problems = append(problems, fmt.Sprintf("%s is excluded without a reason", k))
			}
		default:
			problems = append(problems, fmt.Sprintf("%s has no kind", k))
		}
		if len(r.callers) > 0 {
			for caller, called := range calls {
				if !called[k] {
					continue
				}
				allowed := false
				for _, c := range r.callers {
					allowed = allowed || c == caller
				}
				if !allowed {
					problems = append(problems, fmt.Sprintf("%s calls %s, which only %v may call", caller, k, r.callers))
				}
			}
			for _, c := range r.callers {
				if cr, ok := rules[c]; !ok || (cr.kind != transmits && cr.kind != sealsVia) {
					problems = append(problems, fmt.Sprintf("%s may be called by %s, which is not a listed transmit", k, c))
				}
			}
		}
	}
	sort.Strings(unestablishedUse)
	want := append([]string(nil), unestablished...)
	sort.Strings(want)
	if strings.Join(dedupe(unestablishedUse), ",") != strings.Join(want, ",") {
		problems = append(problems, fmt.Sprintf("relay.LegUnestablished is named by %v, want exactly %v", dedupe(unestablishedUse), want))
	}
	sort.Strings(problems)
	return problems
}

func dedupe(s []string) []string {
	var out []string
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

func parsePackageSources(t *testing.T, fset *token.FileSet, dir string, extra map[string]string) []*ast.File {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	names := make([]string, 0, len(extra))
	for n := range extra {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		f, err := parser.ParseFile(fset, n, extra[n], parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		files = append(files, f)
	}
	return files
}

func TestEveryTransmitBoundaryChecksOwnership(t *testing.T) {
	fset := token.NewFileSet()
	files := parsePackageSources(t, fset, ".", nil)
	if len(files) < 50 {
		t.Fatalf("parsed only %d files", len(files))
	}
	info := typedPackage(t, fset, files)
	for _, p := range checkTransmitBoundaries(fset, files, info, transmitBoundaries, unestablishedLegScopes) {
		t.Error(p)
	}
}

// TestTransmitBoundaryCheckFires adds one offending function to the real
// package and requires the check to name it.
func TestTransmitBoundaryCheckFires(t *testing.T) {
	const relayImport = `import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

var (
	_ = json.Marshal
	_ = fmt.Sprint
	_ = io.Copy
	_ = strings.Contains
	_ = relay.Transmit
	_ http.ResponseWriter
)
`
	rows := []struct {
		name  string
		src   string
		rules func(map[string]boundaryRule)
		want  string
	}{
		{"a new response write", `func probeWrite(w http.ResponseWriter, b []byte) { _, _ = w.Write(b) }`, nil,
			"probeWrite sends bytes outside the listed transmit boundaries"},
		{"a write through io.Copy", `func probeCopy(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(w, r.Body) }`, nil,
			"probeCopy sends bytes outside"},
		{"a write through an encoder", `func probeEncode(w http.ResponseWriter) { _ = json.NewEncoder(w).Encode(1) }`, nil,
			"probeEncode sends bytes outside"},
		{"a write through fmt", `func probeFprint(w http.ResponseWriter) { fmt.Fprint(w, "x") }`, nil,
			"probeFprint sends bytes outside"},
		{"a write through a wrapper", `type probeWrapper struct{ http.ResponseWriter }
func probeWrapped(w probeWrapper) { _, _ = w.Write([]byte("x")) }`, nil,
			"probeWrapped sends bytes outside"},
		{"a new client send", `func probeDo(c *http.Client, r *http.Request) { _, _ = c.Do(r) }`, nil,
			"probeDo sends bytes outside"},
		{"a client handed to another package", `func probeHand(c *http.Client) { _ = fmt.Sprint(c) }`, nil,
			"probeHand sends bytes outside"},
		{"a writer handed to another handler", `func probeServe(w http.ResponseWriter, r *http.Request) { http.NotFoundHandler().ServeHTTP(w, r) }`, nil,
			"probeServe sends bytes outside"},
		{"a package-level http.Post", `func probePost(u string) { _, _ = http.Post(u, "application/json", strings.NewReader("{}")) }`, nil,
			"probePost sends bytes outside"},
		{"a package-level http.Get", `func probeGet(u string) { _, _ = http.Get(u) }`, nil,
			"probeGet sends bytes outside"},
		{"a function literal in a package variable", `var probeVarFunc = func(w http.ResponseWriter) { _, _ = w.Write([]byte("x")) }`, nil,
			"var probeVarFunc sends bytes outside"},
		{"a send while initializing a package variable", `var probeInit, _ = http.Head("http://example.invalid")`, nil,
			"var probeInit,_ sends bytes outside"},
		{"a default client send", `func probeDefault(r *http.Request) { _, _ = http.DefaultClient.Do(r) }`, nil,
			"probeDefault sends bytes outside"},
		{"a new envelope writer", `func probeEnvelope(w http.ResponseWriter) { writeLeg(w, nil) }`, nil,
			"probeEnvelope sends bytes outside"},
		{"a listed transmit without the check", `func probeUnchecked(w http.ResponseWriter, b []byte) { _, _ = w.Write(b) }`,
			func(r map[string]boundaryRule) { r["probeUnchecked"] = boundaryRule{kind: transmits} },
			"probeUnchecked is a transmit boundary but does not call relay.Transmit"},
		{"an envelope writer that seals nothing", `func probeSeal(w http.ResponseWriter) { writeLeg(w, nil) }`,
			func(r map[string]boundaryRule) {
				r["probeSeal"] = boundaryRule{kind: sealsVia, via: "Gateway.buildResponseLeg"}
				wl := r["writeLeg"]
				wl.callers = append(append([]string(nil), wl.callers...), "probeSeal")
				r["writeLeg"] = wl
			},
			"probeSeal writes an envelope but does not call Gateway.buildResponseLeg"},
		{"a listed caller of an envelope writer that is not a transmit", `func probeCaller(w http.ResponseWriter) { writeLeg(w, nil) }`,
			func(r map[string]boundaryRule) {
				r["probeCaller"] = boundaryRule{kind: excluded, reason: "x"}
				wl := r["writeLeg"]
				wl.callers = append(append([]string(nil), wl.callers...), "probeCaller")
				r["writeLeg"] = wl
			},
			"writeLeg may be called by probeCaller, which is not a listed transmit"},
		{"an exclusion without a reason", `func probeBare(w http.ResponseWriter) { _, _ = w.Write(nil) }`,
			func(r map[string]boundaryRule) { r["probeBare"] = boundaryRule{kind: excluded} },
			"probeBare is excluded without a reason"},
		{"a stale entry", `func probeQuiet() {}`,
			func(r map[string]boundaryRule) { r["probeQuiet"] = boundaryRule{kind: excluded, reason: "x"} },
			"probeQuiet is listed but sends nothing"},
		{"a missing entry", ``,
			func(r map[string]boundaryRule) { r["probeGone"] = boundaryRule{kind: transmits} },
			"probeGone is listed but not defined"},
		{"a new pre-leg refusal row", `func probeUnestablished() string { return relay.LegUnestablished }`, nil,
			"relay.LegUnestablished is named by"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			fset := token.NewFileSet()
			files := parsePackageSources(t, fset, ".", map[string]string{"probe_extra.go": "package engine\n\n" + relayImport + "\n" + r.src + "\n"})
			info := typedPackage(t, fset, files)
			rules := map[string]boundaryRule{}
			for k, v := range transmitBoundaries {
				rules[k] = v
			}
			if r.rules != nil {
				r.rules(rules)
			}
			got := checkTransmitBoundaries(fset, files, info, rules, unestablishedLegScopes)
			for _, p := range got {
				if strings.Contains(p, r.want) {
					return
				}
			}
			t.Fatalf("want a problem containing %q, got %q", r.want, got)
		})
	}

	t.Run("clean: a listed transmit that checks", func(t *testing.T) {
		src := `func probeChecked(w http.ResponseWriter, p relay.Payload) {
	b, err := relay.Transmit(p, nil)
	if err == nil {
		_, _ = w.Write(b)
	}
}`
		fset := token.NewFileSet()
		files := parsePackageSources(t, fset, ".", map[string]string{"probe_extra.go": "package engine\n\n" + relayImport + "\n" + src + "\n"})
		info := typedPackage(t, fset, files)
		rules := map[string]boundaryRule{"probeChecked": {kind: transmits}}
		for k, v := range transmitBoundaries {
			rules[k] = v
		}
		if got := checkTransmitBoundaries(fset, files, info, rules, unestablishedLegScopes); len(got) != 0 {
			t.Fatalf("unexpected problems: %q", got)
		}
	})
}
