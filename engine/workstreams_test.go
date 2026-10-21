package engine

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

// toyWorkstream is a workstream with the given tag, catalog and ownership
// rows, and no routes.
type toyWorkstream struct {
	tag     string
	legs    map[string]legSpec
	rows    map[relay.Key]relay.Rule
	ingress map[string]ingressHandler
	answers map[string]answerHandler
}

func (w toyWorkstream) id() string                                  { return w.tag }
func (w toyWorkstream) catalog() map[string]legSpec                 { return w.legs }
func (w toyWorkstream) routes(*Gateway, string) []mountedRoute      { return nil }
func (w toyWorkstream) ownershipRows() map[relay.Key]relay.Rule     { return w.rows }
func (w toyWorkstream) certifiedBuilds() map[certifiedBuild]string  { return nil }
func (w toyWorkstream) certification() certificationAdapter         { return nil }
func (w toyWorkstream) involvement(string) (bool, bool)             { return false, false }
func (w toyWorkstream) requestMembers(string, []byte) []namedMember { return nil }
func (w toyWorkstream) ingressRoutes() map[string]ingressHandler    { return w.ingress }
func (w toyWorkstream) canonicalContractVersion() string            { return "" }
func (w toyWorkstream) originatedAnswerCheck(string) legAnswerCheck { return nil }
func (w toyWorkstream) answer(leg string) answerHandler             { return w.answers[leg] }
func (w toyWorkstream) forward(string) forwardHandler               { return nil }
func (w toyWorkstream) contractVersions() ([]string, []string)      { return nil, nil }
func (w toyWorkstream) newMemState() memState                       { return nil }
func (w toyWorkstream) checkStore(Store) error                      { return nil }
func (w toyWorkstream) newGatewayState(*Gateway) gatewayState       { return nil }

// checkWorkstreamsUnique reports the first collision among compiled-in
// workstreams: an empty or repeated WorkstreamType, a transaction type two
// legs share, or an operation named twice, as one leg's request or response
// operation and another's (or the same leg's other). An envelope names its
// transaction type and no workstream, so the inbound side resolves its leg by
// transaction type alone; and a leg's authority is bound to its operation, not
// its transaction type, so two legs sharing an operation would let a token
// minted for one be presented on the other (AI-11), within one workstream or
// across two.
func checkWorkstreamsUnique(ws []workstream) error {
	tags := map[string]bool{}
	// The core's own route names are taken before any workstream's.
	owners := map[string]map[string]string{"transaction type": {}, "operation": {},
		"ingress route": {RouteSubstrateInbound: "core", ExchangeOther: "core"}}
	claim := func(kind, value, owner string) error {
		if other, dup := owners[kind][value]; dup {
			return fmt.Errorf("%s %q is claimed by both %s and %s", kind, value, other, owner)
		}
		owners[kind][value] = owner
		return nil
	}
	for _, w := range ws {
		if w.id() == "" || tags[w.id()] {
			return fmt.Errorf("workstream type %q is empty or compiled in twice", w.id())
		}
		tags[w.id()] = true
		for _, route := range slices.Sorted(maps.Keys(w.ingressRoutes())) {
			if err := claim("ingress route", route, w.id()); err != nil {
				return err
			}
		}
		catalog := w.catalog()
		for _, tx := range slices.Sorted(maps.Keys(catalog)) {
			spec := catalog[tx]
			leg := w.id() + "/" + tx
			for _, c := range []struct{ kind, value, side string }{
				{"transaction type", tx, ""}, {"operation", spec.Op, " request"}, {"operation", spec.RespOp, " response"},
			} {
				if err := claim(c.kind, c.value, leg+c.side); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func TestWorkstreams_LegsAreUniqueAcrossCatalogs(t *testing.T) {
	if err := checkWorkstreamsUnique(workstreams); err != nil {
		t.Fatalf("the compiled-in workstreams collide: %v", err)
	}
	pa := workstreams[0]
	toy := func(tx string, spec legSpec) workstream {
		return toyWorkstream{tag: "toy", legs: map[string]legSpec{tx: spec}}
	}
	for name, ws := range map[string][]workstream{
		"empty tag":                    {pa, toyWorkstream{tag: "", legs: map[string]legSpec{}}},
		"repeated tag":                 {pa, toyWorkstream{tag: workstreamPA, legs: map[string]legSpec{}}},
		"shared transaction type":      {pa, toy("pas-claim", legSpec{Op: "toy-op", RespOp: "toy-answer"})},
		"shared request operation":     {pa, toy("toy-leg", legSpec{Op: "pas-submit", RespOp: "toy-answer"})},
		"shared response operation":    {pa, toy("toy-leg", legSpec{Op: "toy-op", RespOp: "pas-response"})},
		"two legs share one operation": {toy("a", legSpec{Op: "x", RespOp: "y"}), toyWorkstream{tag: "toy2", legs: map[string]legSpec{"b": {Op: "x", RespOp: "z"}}}},
		"one workstream's legs share a request operation": {toyWorkstream{tag: "toy", legs: map[string]legSpec{
			"a": {Op: "x", RespOp: "y"}, "b": {Op: "x", RespOp: "z"}}}},
		"one workstream's legs share a response operation": {toyWorkstream{tag: "toy", legs: map[string]legSpec{
			"a": {Op: "x", RespOp: "y"}, "b": {Op: "w", RespOp: "y"}}}},
		"a request operation is another leg's response operation": {toyWorkstream{tag: "toy", legs: map[string]legSpec{
			"a": {Op: "x", RespOp: "y"}, "b": {Op: "y", RespOp: "z"}}}},
		"a leg's request and response operations are one": {toy("a", legSpec{Op: "x", RespOp: "x"})},
		"two workstreams declare one ingress route":       {pa, toyWorkstream{tag: "toy", legs: map[string]legSpec{}, ingress: map[string]ingressHandler{RouteCRD: {}}}},
		"a workstream declares the inbound route":         {toyWorkstream{tag: "toy", legs: map[string]legSpec{}, ingress: map[string]ingressHandler{RouteSubstrateInbound: {}}}},
		"a workstream declares the catch-all":             {toyWorkstream{tag: "toy", legs: map[string]legSpec{}, ingress: map[string]ingressHandler{ExchangeOther: {}}}},
	} {
		if err := checkWorkstreamsUnique(ws); err == nil {
			t.Errorf("%s: no collision reported", name)
		}
	}
	if err := checkWorkstreamsUnique([]workstream{pa, toy("toy-leg", legSpec{Op: "toy-op", RespOp: "toy-answer"})}); err != nil {
		t.Errorf("a second workstream with its own legs collides: %v", err)
	}
}

// catalogFor answers PA's own catalog for PA content and nothing for any other
// tag; legSpecFor answers each PA leg exactly as the catalog declares it, and
// nothing for a transaction type no catalog declares.
func TestWorkstreams_LookupsAnswerTheCatalog(t *testing.T) {
	pa, ok := catalogFor(workstreamPA)
	// Identity, not equality: the lookup answers the catalog itself, not a copy.
	if !ok || fmt.Sprintf("%p", pa) != fmt.Sprintf("%p", paCatalog) {
		t.Fatalf("catalogFor(%q) is not the PA catalog (ok %v)", workstreamPA, ok)
	}
	for _, tag := range []string{"", "x12-278", "Da-Vinci-PA"} {
		if c, ok := catalogFor(tag); ok || c != nil {
			t.Fatalf("catalogFor(%q) = %v, %v; want no catalog", tag, c, ok)
		}
	}
	for _, tx := range slices.Sorted(maps.Keys(paCatalog)) {
		spec, ok := legSpecFor(tx)
		if !ok || spec != paCatalog[tx] {
			t.Fatalf("legSpecFor(%q) = %+v, %v; want %+v", tx, spec, ok, paCatalog[tx])
		}
	}
	for _, tx := range []string{"", "unknown", "pdex-read", "CRD-ORDER-SELECT"} {
		if spec, ok := legSpecFor(tx); ok || spec != (legSpec{}) {
			t.Fatalf("legSpecFor(%q) = %+v, %v; want no leg", tx, spec, ok)
		}
	}
}

// catalogNames are the names through which a leg catalog can be read, and the
// non-test files allowed to name them (path.Match patterns): the PA adapter's
// catalog, rows and adapter value in workstream_pa.go, its methods in any of
// the PA adapter's files, and the registry and the derivation in
// workstreams.go. The taxonomy itself is the derivation's input. The FQS
// adapter (workstream_fqs.go) and the toy test profile's adapter
// (workstream_toy_test_profile.go, under its build tag) each derive their own
// catalog as PA's does.
var catalogNames = map[string][]string{
	"paCatalog":           {"workstream_pa.go"},
	"paLegs":              {"workstream_pa.go"},
	"paIngressRoutes":     {"workstream_pa.go"},
	"paWorkstream":        {"workstream_pa.go", "workstream_pa_*.go"},
	"paAdapter":           {"workstream_pa.go", "workstreams.go"},
	"fqsLegs":             {"workstream_fqs.go"},
	"fqsWorkstream":       {"workstream_fqs.go"},
	"fqsAdapter":          {"workstream_fqs.go", "workstreams.go"},
	"workstreams":         {"workstreams.go"},
	"mustCatalog":         {"workstream_pa.go", "workstream_fqs.go", "workstreams.go", "workstream_toy_test_profile.go"},
	"catalogFromTaxonomy": {"workstreams.go"},
}

const legTaxonomyImport = `"github.com/SmartHealthNetwork/shn-gateway/internal/legtaxonomy"`

// legTaxonomyImporters are the files that derive a catalog from the
// taxonomy: the PA and FQS adapters, the registry, and the toy test profile's
// adapter.
var legTaxonomyImporters = []string{"workstream_pa.go", "workstream_fqs.go", "workstreams.go", "workstream_toy_test_profile.go"}

// catalogReadFaults reports every way files (the package's non-test files, by
// name) read a leg catalog other than through the registry: a catalog name
// or the taxonomy import in a file not allowed it, and any read of paCatalog
// but the one in paAdapter's declaration.
func catalogReadFaults(fset *token.FileSet, files map[string]*ast.File) []string {
	var faults []string
	paCatalogReads := 0
	for _, name := range slices.Sorted(maps.Keys(files)) {
		f := files[name]
		for _, imp := range f.Imports {
			if imp.Path.Value == legTaxonomyImport && !slices.Contains(legTaxonomyImporters, name) {
				faults = append(faults, fmt.Sprintf("%s: imports the leg taxonomy", fset.Position(imp.Pos())))
			}
		}
		// declared are the identifiers that name a declaration, not a read.
		declared := map[*ast.Ident]bool{}
		var adapterDecl ast.Node
		ast.Inspect(f, func(n ast.Node) bool {
			switch d := n.(type) {
			case *ast.ValueSpec:
				for _, id := range d.Names {
					declared[id] = true
				}
				if len(d.Names) == 1 && d.Names[0].Name == "paAdapter" {
					adapterDecl = d
				}
			case *ast.TypeSpec:
				declared[d.Name] = true
			case *ast.FuncDecl:
				declared[d.Name] = true
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			allowed, listed := catalogNames[id.Name]
			if !listed {
				return true
			}
			if !slices.ContainsFunc(allowed, func(pattern string) bool { ok, _ := path.Match(pattern, name); return ok }) {
				faults = append(faults, fmt.Sprintf("%s: %s is named outside %v", fset.Position(id.Pos()), id.Name, allowed))
			}
			if id.Name == "paCatalog" && !declared[id] {
				inAdapter := adapterDecl != nil && id.Pos() >= adapterDecl.Pos() && id.End() <= adapterDecl.End()
				if !inAdapter {
					faults = append(faults, fmt.Sprintf("%s: paCatalog is read outside paAdapter's declaration", fset.Position(id.Pos())))
				}
				paCatalogReads++
			}
			return true
		})
	}
	if paCatalogReads != 1 {
		faults = append(faults, fmt.Sprintf("paCatalog is read %d times, want once (paAdapter's declaration)", paCatalogReads))
	}
	return faults
}

// packageSources are the package's non-test files, by name.
func packageSources(t *testing.T) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = b
	}
	return out
}

func parseSources(t *testing.T, fset *token.FileSet, srcs map[string][]byte) map[string]*ast.File {
	t.Helper()
	files := map[string]*ast.File{}
	for name, src := range srcs {
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		files[name] = f
	}
	return files
}

// The engine reads a leg catalog only through the registry: paCatalog is read
// once, into the adapter the registry compiles in, and nothing else in the
// package names the adapter, its rows, the registry, the derivation or the
// taxonomy outside the files that own them.
func TestWorkstreams_CatalogIsReadOnlyThroughTheLookups(t *testing.T) {
	srcs := packageSources(t)
	fset := token.NewFileSet()
	if faults := catalogReadFaults(fset, parseSources(t, fset, srcs)); len(faults) != 0 {
		t.Fatalf("a leg catalog is read outside the registry:\n%s", strings.Join(faults, "\n"))
	}
	probe := func(body string) []byte {
		return []byte("package engine\n\nimport \"github.com/SmartHealthNetwork/shn-gateway/internal/legtaxonomy\"\n\nvar _ = legtaxonomy.PA\n\n" + body + "\n")
	}
	for name, mutated := range map[string]map[string][]byte{
		"a core file reads paCatalog":                           {"probe.go": []byte("package engine\n\nvar _ = paCatalog\n")},
		"a core file reads the adapter's catalog":               {"probe.go": []byte("package engine\n\nvar _ = paWorkstream{}.catalog()\n")},
		"a core file whose name only starts like the adapter's": {"workstream_parse.go": []byte("package engine\n\nvar _ = paWorkstream{}.catalog()\n")},
		"a core file reads the compiled-in adapter":             {"probe.go": []byte("package engine\n\nvar _ = paAdapter.catalog()\n")},
		"a core file reads the PA ingress routes":               {"probe.go": []byte("package engine\n\nvar _ = paIngressRoutes\n")},
		"a core file reads the FQS adapter":                     {"probe.go": []byte("package engine\n\nvar _ = fqsAdapter.catalog()\n")},
		"a core file reads the FQS adapter's rows":              {"probe.go": []byte("package engine\n\nvar _ = fqsLegs\n")},
		"a core file builds an FQS adapter":                     {"probe.go": []byte("package engine\n\nvar _ = fqsWorkstream{}.catalog()\n")},
		"the PA adapter derives the FQS catalog":                {"workstream_pa_fqs.go": probe(`var _ = mustCatalog(legtaxonomy.FQS(), "fqs", fqsLegs)`)},
		"a core file reads the registry":                        {"probe.go": []byte("package engine\n\nvar _ = workstreams\n")},
		"a core file derives its own catalog":                   {"probe.go": probe(`var _ = mustCatalog(legtaxonomy.PA(), "pa", paLegs)`)},
		"a core file calls the derivation":                      {"probe.go": probe(`var _ = catalogFromTaxonomy`)},
		"a core file imports the taxonomy":                      {"probe.go": probe(``)},
		"the PA adapter reads paCatalog a second time":          {"workstream_pa.go": append(slices.Clone(srcs["workstream_pa.go"]), []byte("\nfunc paSecondRead() map[string]legSpec { return paCatalog }\n")...)},
	} {
		changed := maps.Clone(srcs)
		maps.Copy(changed, mutated)
		fset := token.NewFileSet()
		if faults := catalogReadFaults(fset, parseSources(t, fset, changed)); len(faults) == 0 {
			t.Errorf("%s: no fault reported", name)
		}
	}
}

// The engine registers the compiled-in workstreams' ownership over exactly
// their catalogs' legs, and refuses a row naming a leg its own adapter's
// catalog does not declare, or a key two adapters both declare.
func TestWorkstreams_OwnershipIsRegisteredOverTheCatalogs(t *testing.T) {
	legs, rows, err := workstreamOwnership(workstreams)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, w := range workstreams {
		want = append(want, slices.Sorted(maps.Keys(w.catalog()))...)
	}
	if len(want) < len(paCatalog) || !slices.Equal(legs, want) || !slices.Equal(relay.Legs(), slices.Sorted(slices.Values(legs))) {
		t.Fatalf("legs %v, registered %v; want the compiled-in catalogs' %v", legs, relay.Legs(), want)
	}
	registered := relay.LegOwnership()
	for k, r := range rows {
		if got, ok := registered[k]; !ok || !slices.Equal(got.Allowed, r.Allowed) || !slices.Equal(got.Edits, r.Edits) || !slices.Equal(got.Builders, r.Builders) {
			t.Errorf("%s: registered %+v, the adapter declares %+v", k, got, r)
		}
	}
	relayed := relay.Rule{Allowed: []relay.Ownership{relay.OwnershipRelayed}}
	carried := func(leg string) relay.Key {
		return relay.Key{Leg: leg, Role: relay.RoleRecipient, Direction: relay.DirectionRequest, Outcome: relay.OutcomeCarried}
	}
	toy := toyWorkstream{tag: "toy", legs: map[string]legSpec{"toy-leg": {Op: "toy-op", RespOp: "toy-answer"}}, rows: map[relay.Key]relay.Rule{carried("toy-leg"): relayed}}
	if _, _, err := workstreamOwnership([]workstream{paAdapter, toy}); err != nil {
		t.Errorf("a second workstream with its own rows is refused: %v", err)
	}
	fqCarried := relay.Key{Leg: "federated-query", Role: relay.RoleRecipient, Direction: relay.DirectionRequest, Outcome: relay.OutcomeCarried}
	if _, declared := paLegOwnership[fqCarried]; declared {
		t.Fatalf("fixture: PA declares %s; pick a key it does not", fqCarried)
	}
	for name, c := range map[string]struct {
		ws   []workstream
		want string
	}{
		"a row for another workstream's leg": {[]workstream{paAdapter, toyWorkstream{tag: "toy", legs: toy.legs, rows: map[relay.Key]relay.Rule{fqCarried: relayed}}}, "names no leg of its catalog"},
		"a row for no leg":                   {[]workstream{toyWorkstream{tag: "toy", legs: toy.legs, rows: map[relay.Key]relay.Rule{carried("other"): relayed}}}, "names no leg of its catalog"},
		"a key two adapters declare":         {[]workstream{toy, toyWorkstream{tag: "toy2", legs: toy.legs, rows: toy.rows}}, "is declared twice"},
	} {
		if _, _, err := workstreamOwnership(c.ws); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %v, want one containing %q", name, err, c.want)
		}
	}
}

// The PA adapter's ingress routes are its map itself: the core mounts exactly
// the handlers paIngressRoutes names (which the answer-line tripwire reads),
// with nothing wrapped in between.
func TestWorkstreams_PAIngressRoutesAreTheMap(t *testing.T) {
	if got := paAdapter.ingressRoutes(); fmt.Sprintf("%p", got) != fmt.Sprintf("%p", paIngressRoutes) {
		t.Fatal("the PA adapter's ingress routes are not paIngressRoutes itself")
	}
}
