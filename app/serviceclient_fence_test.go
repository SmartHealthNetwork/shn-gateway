//go:build !shn_coreonly

package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/checks"
	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
	"github.com/SmartHealthNetwork/shn-gateway/internal/serviceclient"
)

// serviceKinds are the endpoint kinds of the network's own services and the
// OTLP collector: every client or transport built for one is under the
// network-service redirect policy (serviceclient), which follows no redirect.
var serviceKinds = []string{"Hub", "Authorization", "Consent", "Registrar", "Audit", "OTLP", "Discovery", "AuthorizationKey", "HubKey"}

// unpolicedValidators are the functions that make a validator client with
// shnsdk.NewOperationValidator and do not themselves call TagValidatorClient,
// which puts it under the policy, with where it is put under it.
var unpolicedValidators = map[string]string{
	"app/app.go selectValidator":                                   "build calls TagValidatorClient on the validator it returns",
	"engine/module3_certify.go NewCertificationOperationValidator": "its transport is certificationTransport, whose RoundTrip applies serviceclient.Check (checked below)",
}

// TestNetworkServiceClientsFollowNoRedirect fails on a client or transport
// built for a network service's endpoint kind (kindClient, a Recorder's
// Client or Wrap) outside serviceclient.ServiceClient or Transport and not
// over a transport one of them made, on a
// validator client made where TagValidatorClient is not called (or a row
// above argues it), and on a certification transport that does not apply
// serviceclient.Check: a new such site fails until it follows no redirect.
func TestNetworkServiceClientsFollowNoRedirect(t *testing.T) {
	fset := token.NewFileSet()
	var problems []string
	kinds := map[string]bool{}
	validators := map[string]bool{}
	certCheck := false
	for _, dir := range outboundDirs {
		err := filepath.WalkDir(filepath.Join(gatewayModule, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == "testdata" {
				return filepath.SkipDir
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(gatewayModule, path)
			if err != nil {
				return err
			}
			r := serviceSitesIn(filepath.ToSlash(rel), f)
			problems = append(problems, r.problems...)
			for _, k := range r.kinds {
				kinds[k] = true
			}
			for _, v := range r.validators {
				validators[v] = true
			}
			certCheck = certCheck || r.certCheck
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range problems {
		t.Error(p)
	}
	for _, k := range serviceKinds {
		if !kinds[k] {
			t.Errorf("no client is built for endpointcert.%s: the fence reads nothing for it (moved, or renamed?)", k)
		}
	}
	for _, site := range slices.Sorted(maps.Keys(unpolicedValidators)) {
		if !validators[site] {
			t.Errorf("unpolicedValidators lists %q, which makes no validator now: remove the row", site)
		}
	}
	if !certCheck {
		t.Error("engine/module3_certify.go: certificationTransport.RoundTrip does not apply serviceclient.Check")
	}
}

// serviceSites is what serviceSitesIn found in one file.
type serviceSites struct {
	problems   []string
	kinds      []string // service kinds built under the policy
	validators []string // unpoliced validator sites the table argues
	certCheck  bool
}

// serviceSitesIn reads f, at rel, for the fence above.
func serviceSitesIn(rel string, f *ast.File) serviceSites {
	ecName, scName, sdkName := "", "", ""
	for _, imp := range f.Imports {
		name := ""
		if imp.Name != nil {
			name = imp.Name.Name
		}
		switch strings.Trim(imp.Path.Value, `"`) {
		case "github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert":
			ecName = orName(name, "endpointcert")
		case "github.com/SmartHealthNetwork/shn-gateway/internal/serviceclient":
			scName = orName(name, "serviceclient")
		case "github.com/SmartHealthNetwork/shn-sdk":
			sdkName = orName(name, "shnsdk")
		}
	}
	var out serviceSites
	isSel := func(e ast.Expr, pkg string, names ...string) bool {
		s, ok := e.(*ast.SelectorExpr)
		if !ok || pkg == "" {
			return false
		}
		id, ok := s.X.(*ast.Ident)
		return ok && id.Name == pkg && slices.Contains(names, s.Sel.Name)
	}
	// serviceKindOf is the service kind expression e names, directly
	// (endpointcert.Hub) or as a tag (endpointcert.Of(endpointcert.Hub)).
	serviceKindOf := func(e ast.Expr) string {
		if c, ok := e.(*ast.CallExpr); ok && isSel(c.Fun, ecName, "Of") && len(c.Args) == 1 {
			e = c.Args[0]
		}
		if s, ok := e.(*ast.SelectorExpr); ok && isSel(s, ecName, serviceKinds...) {
			return s.Sel.Name
		}
		return ""
	}
	// builds reports the service kind a call builds a client or transport
	// for: kindClient(kind), or a Recorder's Client, Wrap or WrapRouted.
	builds := func(c *ast.CallExpr) string {
		builder := false
		switch fn := c.Fun.(type) {
		case *ast.Ident:
			// kindClient, or any local builder (resolveDiscovery's boot)
			// handed a service kind.
			builder = true
		case *ast.SelectorExpr:
			builder = slices.Contains([]string{"Client", "Wrap", "WrapRouted"}, fn.Sel.Name)
		}
		if !builder || len(c.Args) == 0 {
			return ""
		}
		return serviceKindOf(c.Args[0])
	}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		scope := rel + " " + fd.Name.Name
		tags, makesValidator := false, false
		var stack []ast.Node
		ast.Inspect(fd, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return false
			}
			stack = append(stack, n)
			if cl, ok := n.(*ast.CompositeLit); ok && isSel(cl.Type, sdkName, "OperationValidator") {
				makesValidator = true
			}
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := c.Fun.(type) {
			case *ast.Ident:
				tags = tags || fn.Name == "TagValidatorClient"
			case *ast.SelectorExpr:
				tags = tags || fn.Sel.Name == "TagValidatorClient"
			}
			if isSel(c.Fun, sdkName, "NewOperationValidator") {
				makesValidator = true
			}
			if fd.Recv != nil && fd.Name.Name == "RoundTrip" && isSel(c.Fun, scName, "Check") && recvName(fd) == "certificationTransport" {
				out.certCheck = true
			}
			kind := builds(c)
			if kind == "" {
				return true
			}
			// Under the policy: inside ServiceClient or Transport, or built
			// over a transport one of them made.
			policed := false
			for _, a := range c.Args[1:] {
				if ac, ok := a.(*ast.CallExpr); ok && isSel(ac.Fun, scName, "ServiceClient", "Transport") {
					policed = true
				}
			}
			for _, a := range stack[:len(stack)-1] {
				if ac, ok := a.(*ast.CallExpr); ok && isSel(ac.Fun, scName, "ServiceClient", "Transport") {
					policed = true
				}
			}
			if !policed {
				out.problems = append(out.problems, scope+": a client for endpointcert."+kind+" is built outside serviceclient.ServiceClient or Transport: a network service's client follows no redirect")
			} else {
				out.kinds = append(out.kinds, kind)
			}
			return true
		})
		if makesValidator && !tags {
			if _, ok := unpolicedValidators[scope]; ok {
				out.validators = append(out.validators, scope)
			} else {
				out.problems = append(out.problems, scope+": makes a validator client and does not call TagValidatorClient, which puts it under the network-service redirect policy (or argue it in unpolicedValidators)")
			}
		}
	}
	return out
}

// recvName is the type name of fd's receiver.
func recvName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return ""
	}
	t := fd.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// Every kind the fence above reads is one tagCheckTargets probes under the
// policy too (noRedirectKinds), and every client tagCheckTargets makes for a
// kind of noRedirectKinds follows no redirect: a check of a network service
// or sidecar is no laxer than its calls.
func TestCheckProbesOfNetworkServicesFollowNoRedirect(t *testing.T) {
	for _, k := range serviceKinds {
		if !slices.ContainsFunc(noRedirectKinds, func(n endpointcert.Kind) bool { return n.String() == kindName(k) }) {
			t.Errorf("endpointcert.%s is fenced as a network service but not in noRedirectKinds", k)
		}
	}
	rec := endpointcert.New()
	var targets []checks.Target
	for id := range settingTags {
		targets = append(targets, checks.Target{ID: id})
	}
	targets = append(targets, checks.Target{ID: "AUDIT_ENDPOINT"}, checks.Target{ID: "validator-lane-2.1"})
	for _, tg := range tagCheckTargets(rec, targets, &http.Client{}) {
		tag, _ := checkTag(tg.ID)
		if got := serviceclient.Refuses(tg.Client.Transport); got != followsNoRedirect(tag.Kind()) {
			t.Errorf("%s (%v): its probe client follows no redirect %v, want %v", tg.ID, tag.Kind(), got, followsNoRedirect(tag.Kind()))
		}
	}
}

// kindName is the endpoint kind's name for a Go identifier of endpointcert.
func kindName(ident string) string {
	for _, k := range endpointcert.Kinds() {
		if strings.EqualFold(strings.ReplaceAll(k.String(), "-", ""), ident) {
			return k.String()
		}
	}
	return ""
}

// The fence's own rows: a service client built bare fails it, wrapped
// passes; a validator made with no TagValidatorClient fails it.
func TestNetworkServiceClientsFence_Rows(t *testing.T) {
	const head = `package x
import (
	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
	"github.com/SmartHealthNetwork/shn-gateway/internal/serviceclient"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)
`
	for _, row := range []struct {
		name, src string
		fails     bool
	}{
		{"a bare Hub client", `func f() { _ = kindClient(endpointcert.Hub) }`, true},
		{"a bare audit client by tag", `func f() { _ = rec.Client(endpointcert.Of(endpointcert.Audit), nil) }`, true},
		{"a bare OTLP transport", `func f() { _ = rec.Wrap(endpointcert.Of(endpointcert.OTLP), nil) }`, true},
		{"a wrapped Hub client", `func f() { _ = serviceclient.ServiceClient(kindClient(endpointcert.Hub)) }`, false},
		{"an OTLP transport over the policy", `func f() { _ = rt{next: rec.Wrap(endpointcert.Of(endpointcert.OTLP), serviceclient.Transport(nil))} }`, false},
		{"an OTLP transport over another transport", `func f() { _ = rt{next: rec.Wrap(endpointcert.Of(endpointcert.OTLP), other(nil))} }`, true},
		{"an OTLP transport under the policy", `func f() { _ = serviceclient.Transport(rec.Wrap(endpointcert.Of(endpointcert.OTLP), nil)) }`, false},
		{"a participant's client", `func f() { _ = kindClient(endpointcert.FHIR) }`, false},
		{"a validator never tagged", `func g() { _ = shnsdk.NewOperationValidator("u") }`, true},
		{"a validator tagged", `func g() { v := shnsdk.NewOperationValidator("u"); TagValidatorClient(v, nil, t) }`, false},
		{"a validator literal never tagged", `func g() { _ = &shnsdk.OperationValidator{BaseURL: "u"} }`, true},
		{"a bare discovery boot client", `func f() { _ = boot(endpointcert.Discovery) }`, true},
		{"a discovery boot client under the policy", `func f() { _ = serviceclient.ServiceClient(boot(endpointcert.HubKey)) }`, false},
	} {
		f, err := parser.ParseFile(token.NewFileSet(), "x.go", head+row.src, 0)
		if err != nil {
			t.Fatalf("%s: %v", row.name, err)
		}
		if got := len(serviceSitesIn("x/x.go", f).problems) != 0; got != row.fails {
			t.Errorf("%s: fails %v, want %v", row.name, got, row.fails)
		}
	}
}
