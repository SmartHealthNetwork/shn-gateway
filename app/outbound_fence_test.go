//go:build !shn_coreonly

package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// outboundSites is every place in the packages build() links that makes an
// HTTP client or transport, or reaches for net/http's defaults, keyed by
// "file func what", with how its client carries an endpoint kind (MON-R25)
// or why it needs none. TestMONR25EveryOutboundClientIsTagged fails on a
// site missing here, so a new client is tagged, or argued, before it ships;
// and on a row with no site left, so the table stays true.
var outboundSites = map[string]outboundSite{
	// app: the template, and the clients made from it.
	"app/app.go build shnsdk.NewClient":                                {1, "the template every kindClient is a tagged copy of; never used untagged"},
	"app/app.go build http.DefaultClient":                              {1, "FHIR_DATA_URL with no SMART block: tagged fhir by the cfg.certs.Client it is passed to"},
	"app/app.go build http.Client{}":                                   {1, "the payer's unauthenticated forward: payerTransport, the payer's tagged Router"},
	"app/app.go var:requesterLegBudget shnsdk.NewClient":               {1, "reads the template's timeout; no client is kept"},
	"app/app.go probeAuthClient smartauth.NewHTTPClient":               {1, "built from a SMART config tagSmart tagged"},
	"app/app.go probeAuthClient http.Client{}":                         {1, "a credential that does not load: every request fails before dialing"},
	"app/app.go selectValidator shnsdk.NewOperationValidator":          {1, "tagged validator-<canonical line> by build right after (TagValidatorClient)"},
	"app/app.go fhirHTTPClient smartauth.NewHTTPClient":                {1, "fhirSmartConfig tags it fhir / fhir-token"},
	"app/app.go payerDavinciHTTPClient smartauth.NewHTTPClient":        {1, "payerDavinciSmartConfig tags it with the payer Router / payer-token"},
	"app/app.go providerDTRPopulateHTTPClient smartauth.NewHTTPClient": {1, "providerDTRPopulateSmartBase tags it populate / populate-token"},
	"app/attachmentinflight.go taskMetadataClient http.Client{}":       {1, "the task's own ECS metadata endpoint, read once at boot: the platform's, no peer and no endpoint kind"},
	"app/attachmentinflight.go taskMetadataClient http.Transport{}":    {1, "that read's transport: no proxy, since the endpoint is on the task's own link"},
	"app/bootfetch.go bootClient http.DefaultTransport":                {1, "a nil transport's meaning; resolveDiscovery passes tagged clients"},
	"app/diagnostic.go newDiagnosticSource http.Client{}":              {1, "the sink's publisher client: tagged diagnostic by build right after"},
	"app/diagnostic.go smart http.Client{}":                            {1, "the token client's copy: its transport is the tagged one tagSmart set"},
	"app/outbound.go tagSmart http.Client{}":                           {1, "the tagged token client itself"},
	"app/outbound.go laneQualifier http.Client{}":                      {1, "Wrapped by rec.Client in the same expression with the attempt's lane; an unmarked attempt gets no tag and records nothing"},
	"app/outbound.go taggedValidator shnsdk.NewOperationValidator":     {1, "tagged on the next line"},

	"app/app.go var:certificationClient engine.NewCertificationOperationValidator": {1, "the constructor's seam for tests; every caller goes through certifyClient, which tags the client"},
	"app/app.go certificationValidators engine.NewGatedCertificationValidator":     {1, "builds its client inside engine; tagged on the next line through CertificationClient"},
	"app/lanes.go qualifyDefaultLane engine.QualifyValidatorLane":                  {1, "laneQualifier's form with no recorder; with one, the qualification runs QualifyValidatorLaneWith over a tagged client"},
	"engine/certtags.go TagValidatorClient shnsdk.NewClient":                       {1, "the default a validator with no client takes, Wrapped on the next lines"},
	"internal/endpointcert/endpointcert.go Client var http.Client":                 {1, "the copy Client returns, its transport Wrapped"},

	// Nil defaults for library callers: build() always passes a client.
	"checks/checks.go NewRunner http.DefaultClient":                         {1, "nil default; build passes the template, and every target its tagged copy"},
	"connectors/fhirsor/search.go searchClient http.DefaultClient":          {1, "nil default; build passes a tagged fhir client"},
	"connectors/smartauth/tokensource.go fetch http.Client{}":               {1, "nil default; tagSmart sets HTTPClient"},
	"connectors/smartauth/transport.go NewHTTPClient http.DefaultTransport": {1, "nil default; tagSmart sets Transport"},
	"connectors/smartauth/transport.go NewHTTPClient http.Client{}":         {1, "wraps the transport it is given"},
	"diagnostics/http.go ObserveTransport http.DefaultTransport":            {1, "nil default; build passes tagged transports"},
	"diagnostics/publisher.go RunPublisher http.Client{}":                   {1, "nil default; build passes the tagged diagnostic client"},
	"engine/gateway.go New http.DefaultClient":                              {1, "nil default; build passes the hub client"},
	"engine/gateway.go authorizeOnce http.DefaultClient":                    {1, "nil default; build passes the authorization client"},
	"internal/auditappend/appender.go New http.DefaultClient":               {1, "nil default; build passes the audit client"},
	"internal/fhirclient/client.go New http.DefaultClient":                  {1, "nil default; its callers pass a tagged client"},

	// Clients the engine builds itself.
	"engine/fhirserver_read.go client http.Transport{}":                             {1, "the read's own guarded transport, unchanged; Wrapped crd-fhir-server through CertRecorder"},
	"engine/fhirserver_read.go client http.Client{}":                                {1, "wraps the tagged transport"},
	"engine/lanehealth.go ownConnectionClient http.Transport{}":                     {1, "only when http.DefaultTransport is not an *http.Transport (an embedder's fixture), as before"},
	"engine/lanehealth.go ownConnectionClient http.DefaultTransport":                {1, "the lane check's own clone, as before; Rewrapped with the lane client's kind (laneTransport)"},
	"engine/lanehealth.go ownConnectionClient http.Client{}":                        {1, "wraps that clone, Rewrapped with the lane's kind"},
	"engine/lanes.go QualifyValidatorLane http.Client{}":                            {1, "the untagged form for embedders; build uses QualifyValidatorLaneWith (laneQualifier)"},
	"engine/module3_certify.go NewCertificationOperationValidator http.Transport{}": {1, "tagged certify-<line> by TagValidatorClient (certifyClient, CertificationClient)"},
	"engine/module3_certify.go NewCertificationOperationValidator http.Client{}":    {1, "wraps that transport"},
	"internal/lanequalify/probe.go httpClient http.Client{}":                        {1, "Warm's corpus posts (a Kit twin, byte for byte): they follow a metadata read of the same base through the lane's tagged client in the same qualification, which records the endpoint's certificate"},

	// A caller's side of a gateway's /scenario routes (tools and the Kit):
	// build() uses only scenarioauth.ParseCallers, which sends nothing.
	"scenarioauth/scenarioauth.go Client http.Client{}":            {1, "a caller's signing client, never built by a gateway"},
	"scenarioauth/scenarioauth.go ClientFromEnv http.Client{}":     {1, "a caller's signing client, never built by a gateway"},
	"scenarioauth/scenarioauth.go RoundTrip http.DefaultTransport": {1, "a caller's signing transport's nil base, never built by a gateway"},

	// The OTLP exporter's own client. Its gRPC path records otlp from each
	// call's peer TLS state (peerCertificate), which builds no client.
	"telemetry/otelsetup/setup.go build http.Client{}":         {1, "the OTLP HTTP exporter's client: its transport Wrapped otlp in the same expression"},
	"telemetry/otelsetup/setup.go build http.DefaultTransport": {1, "the OTLP HTTP exporter's own clone, Wrapped otlp where the client is built"},

	// The recorder.
	"internal/endpointcert/endpointcert.go transport http.DefaultTransport": {1, "a wrapper of a nil transport reads http.DefaultTransport at each request, as the client did"},
}

// outboundSite is how many times a site occurs in its function, and why it
// carries its kind or needs none: a second client in a function that already
// has one is a new site.
type outboundSite struct {
	n   int
	why string
}

// outboundDirs are the gateway module's packages the fence checks: every
// one but the tools and harnesses a deployed gateway never runs (fhirseed,
// scenariodriver, deploy).
var outboundDirs = []string{"app", "checks", "cmd", "connectors", "diagnostics", "engine", "internal", "observer", "pa", "recording", "scenarioauth", "seed", "telemetry", "validatorissue"}

// gatewayModule is the gateway module's directory, from this package's.
const gatewayModule = ".."

func TestMONR25EveryOutboundClientIsTagged(t *testing.T) {
	found := map[string]int{}
	fset := token.NewFileSet()
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
			rel = filepath.ToSlash(rel)
			for _, site := range outboundSitesIn(f) {
				found[rel+" "+site]++
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, site := range slices.Sorted(maps.Keys(found)) {
		want, ok := outboundSites[site]
		switch {
		case !ok:
			t.Errorf("untagged outbound client at %q: give it its endpoint kind (app/outbound.go, endpointcert) or add it to outboundSites with why it needs none", site)
		case want.n != found[site]:
			t.Errorf("%q occurs %d times, outboundSites says %d: a new client in that function needs its kind, or the row its count and reason", site, found[site], want.n)
		}
	}
	for _, site := range slices.Sorted(maps.Keys(outboundSites)) {
		if found[site] == 0 {
			t.Errorf("outboundSites lists %q, which no longer exists: remove the row", site)
		}
	}
}

// outboundSitesIn is each "func what" in f, once per occurrence: a struct
// literal, new() or zero-value declaration of http.Client or
// http.Transport, a use of http.DefaultClient or http.DefaultTransport, a
// call of net/http's default-client helpers (http.Get, Head, Post,
// PostForm), a call of a constructor that makes a client (shnsdk.NewClient,
// shnsdk.NewOperationValidator, smartauth.NewHTTPClient), or a use outside
// the engine of the engine's untagged client constructors
// (engine.NewCertificationOperationValidator, NewGatedCertificationValidator,
// QualifyValidatorLane).
func outboundSitesIn(f *ast.File) []string {
	httpName, sdkName, smartName, engineName := "", "", "", ""
	for _, imp := range f.Imports {
		p := strings.Trim(imp.Path.Value, `"`)
		name := ""
		if imp.Name != nil {
			name = imp.Name.Name
		}
		switch p {
		case "net/http":
			httpName = orName(name, "http")
		case "github.com/SmartHealthNetwork/shn-sdk":
			sdkName = orName(name, "shnsdk")
		case "github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth":
			smartName = orName(name, "smartauth")
		case "github.com/SmartHealthNetwork/shn-gateway/engine":
			engineName = orName(name, "engine")
		}
	}
	var out []string
	sel := func(e ast.Expr, pkg string, names ...string) (string, bool) {
		s, ok := e.(*ast.SelectorExpr)
		if !ok || pkg == "" {
			return "", false
		}
		id, ok := s.X.(*ast.Ident)
		if !ok || id.Name != pkg || !slices.Contains(names, s.Sel.Name) {
			return "", false
		}
		return s.Sel.Name, true
	}
	visit := func(scope string, n ast.Node) {
		ast.Inspect(n, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CompositeLit:
				if name, ok := sel(x.Type, httpName, "Client", "Transport"); ok {
					out = append(out, scope+" http."+name+"{}")
				}
			case *ast.SelectorExpr:
				if name, ok := sel(x, httpName, "DefaultClient", "DefaultTransport"); ok {
					out = append(out, scope+" http."+name)
				}
				if name, ok := sel(x, engineName, "NewCertificationOperationValidator", "NewGatedCertificationValidator", "QualifyValidatorLane"); ok {
					out = append(out, scope+" engine."+name)
				}
			case *ast.ValueSpec:
				if x.Type != nil {
					if name, ok := sel(x.Type, httpName, "Client", "Transport"); ok {
						out = append(out, scope+" var http."+name)
					}
				}
			case *ast.CallExpr:
				if name, ok := sel(x.Fun, httpName, "Get", "Head", "Post", "PostForm"); ok {
					out = append(out, scope+" http."+name+"()")
				}
				if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "new" && len(x.Args) == 1 {
					if name, ok := sel(x.Args[0], httpName, "Client", "Transport"); ok {
						out = append(out, scope+" new(http."+name+")")
					}
				}
				if name, ok := sel(x.Fun, sdkName, "NewClient", "NewOperationValidator"); ok {
					out = append(out, scope+" shnsdk."+name)
				}
				if name, ok := sel(x.Fun, smartName, "NewHTTPClient"); ok {
					out = append(out, scope+" smartauth."+name)
				}
			}
			return true
		})
	}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			visit(d.Name.Name, d)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				if vs, ok := spec.(*ast.ValueSpec); ok {
					for _, n := range vs.Names {
						visit("var:"+n.Name, vs)
					}
				}
			}
		}
	}
	return out
}

func orName(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
