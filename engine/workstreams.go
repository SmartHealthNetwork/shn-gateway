package engine

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/internal/legtaxonomy"
)

// workstream is one workstream compiled into this gateway: its adapter
// (design §4.2). The core reads a workstream's legs and routes through it and
// names none of them itself.
type workstream interface {
	// id is the WorkstreamType the workstream's content is tagged with.
	id() string
	// catalog is the workstream's leg catalog, keyed by envelope
	// TransactionType.
	catalog() map[string]legSpec
	// routes are the workstream's participant-facing routes for role on g.
	routes(g *Gateway, role string) []mountedRoute
	// ingressRoutes are the workstream's ingress handlers, by the route each
	// exchange record names (ExchangeRoutes).
	ingressRoutes() map[string]ingressHandler
	// canonicalContractVersion is the contract-version token the deployment's
	// canonical validator (Config.Validator, FHIR_VALIDATE_URL) resolves for the
	// workstream, or "" when the workstream names none.
	canonicalContractVersion() string
	// originatedAnswerCheck is the check an answer to one of the workstream's
	// legs this gateway originates must pass before anything reads it, or nil
	// for a leg whose originated answer has none.
	originatedAnswerCheck(leg string) legAnswerCheck
	// answer is the handler that answers one of the workstream's legs at the
	// recipient, once the core's checks have admitted it; every leg of its
	// catalog has one (TestInbound_EveryLegHasAnAnswerHandler).
	answer(leg string) answerHandler
	// forward is the native responder's send of one of the workstream's legs
	// to the participant's own system, once the core's version rule has admitted
	// it, or nil for a leg the native responder does not send.
	forward(leg string) forwardHandler
	// contractVersions are the contract-version tokens the workstream builds
	// natively and those a deployment declares by default
	// (NativeContractVersions, DefaultContractVersions).
	contractVersions() (native, defaults []string)
	// ownershipRows are what each transmit of each of the workstream's legs
	// may carry (relay.RegisterOwnership); every row names a leg of its
	// catalog.
	ownershipRows() map[relay.Key]relay.Rule
	// certifiedBuilds are the workstream's builder and line pairs that
	// build-time certification proves, each with the make validate test that
	// proves it (certifiedbuilds.go), or nil.
	certifiedBuilds() map[certifiedBuild]string
	// certification is the workstream's part in certifying its legs'
	// payloads (certificationAdapter, species.go), or nil when it certifies
	// none.
	certification() certificationAdapter
	// involvement reports, for one of the workstream's legs, whether its
	// request names the other patients it carries and whether its answer
	// names the responder's own binding of the member, for the network's
	// involved list.
	involvement(leg string) (request, answer bool)
	// requestMembers is every member a request on one of the workstream's
	// legs names, each read as that leg binds its subject, with the bytes its
	// patient is bound from.
	requestMembers(leg string, payload []byte) []namedMember
	// newMemState is the workstream's own state in a new in-memory Store
	// (memstore.go), or nil for a workstream that keeps none there.
	newMemState() memState
	// checkStore reports why the workstream cannot use the configured Store
	// as given, or nil. New refuses to construct a gateway on any.
	checkStore(s Store) error
	// newGatewayState is the workstream's own state on a new gateway g, or
	// nil for a workstream that keeps none there.
	newGatewayState(g *Gateway) gatewayState
}

// gatewayState is one workstream's own state on a gateway
// (workstream.newGatewayState). The workstream that made it is the one that
// reads it.
type gatewayState any

// workstreams are the workstreams compiled into this build: Prior
// Authorization (workstream_pa.go) and Federated Query Services
// (workstream_fqs.go). Another workstream is a sibling adapter with its own
// taxonomy and catalog. Only the lookups in this file read it. Each entry is
// an adapter variable that holds its catalog, so Go initializes every catalog
// before the registry (a call through the interface would order nothing).
var workstreams = append([]workstream{paAdapter, fqsAdapter}, profileWorkstreams()...)

// catalogFor is the leg catalog of the compiled-in workstream whose content is
// tagged workstreamType; ok is false when this build serves no such
// workstream.
func catalogFor(workstreamType string) (catalog map[string]legSpec, ok bool) {
	for _, w := range workstreams {
		if w.id() == workstreamType {
			return w.catalog(), true
		}
	}
	return nil, false
}

// legSpecFor is the leg transactionType names, in whichever compiled-in
// workstream declares it; ok is false when none does. An envelope names its
// transaction type and no workstream, so the inbound side resolves the leg by
// transaction type alone: transaction types are unique across the compiled-in
// catalogs, and so are the request and response operations a leg's authority
// is bound to (TestWorkstreams_LegsAreUniqueAcrossCatalogs).
func legSpecFor(transactionType string) (legSpec, bool) {
	for _, w := range workstreams {
		if s, ok := w.catalog()[transactionType]; ok {
			return s, true
		}
	}
	return legSpec{}, false
}

// legAnswerCheck certifies an answer at the routed line and this gateway's own
// level, recording its findings; a non-zero Status refuses the answer.
type legAnswerCheck func(ctx context.Context, mod conformanceModule, emit func(ConformanceFinding), body []byte, line, whose string) LegResult

// originatedAnswerCheckFor is the check the compiled-in workstream declaring
// leg puts on an answer to it this gateway originates, or nil.
func originatedAnswerCheckFor(leg string) legAnswerCheck {
	if w, ok := workstreamFor(leg); ok {
		return w.originatedAnswerCheck(leg)
	}
	return nil
}

// forwardFor is the forward the compiled-in workstream declaring leg sends it
// to the participant's own system with, or nil when none declares it or it has
// none.
func forwardFor(leg string) forwardHandler {
	if w, ok := workstreamFor(leg); ok {
		return w.forward(leg)
	}
	return nil
}

// answerFor is the handler the compiled-in workstream declaring leg answers it
// with, or nil when none declares it or it has none.
func answerFor(leg string) answerHandler {
	for _, w := range workstreams {
		if _, ok := w.catalog()[leg]; ok {
			return w.answer(leg)
		}
	}
	return nil
}

// workstreamCatalogs are the compiled-in workstreams' catalogs, in registry
// order.
func workstreamCatalogs() []map[string]legSpec {
	var out []map[string]legSpec
	for _, w := range workstreams {
		out = append(out, w.catalog())
	}
	return out
}

// workstreamMemStates is each compiled-in workstream's own state for a new
// in-memory Store (memstore.go), by workstream id; a workstream that keeps none
// there has no entry.
func workstreamMemStates() map[string]memState {
	out := map[string]memState{}
	for _, w := range workstreams {
		if st := w.newMemState(); st != nil {
			out[w.id()] = st
		}
	}
	return out
}

// workstreamGatewayStates is each compiled-in workstream's own state on a new
// gateway g, by workstream id; a workstream that keeps none there has no
// entry.
func workstreamGatewayStates(g *Gateway) map[string]gatewayState {
	out := map[string]gatewayState{}
	for _, w := range workstreams {
		if st := w.newGatewayState(g); st != nil {
			out[w.id()] = st
		}
	}
	return out
}

// workstreamStoreFault is the first compiled-in workstream's reason it
// cannot use s as given (checkStore), or nil.
func workstreamStoreFault(s Store) error {
	for _, w := range workstreams {
		if err := w.checkStore(s); err != nil {
			return err
		}
	}
	return nil
}

// workstreamFor is the compiled-in workstream whose catalog declares leg.
func workstreamFor(leg string) (workstream, bool) {
	for _, w := range workstreams {
		if _, ok := w.catalog()[leg]; ok {
			return w, true
		}
	}
	return nil, false
}

// legInvolvement is the involvement of leg as the workstream that declares
// it states it (workstream.involvement); neither for a leg no compiled-in
// workstream declares.
func legInvolvement(leg string) (request, answer bool) {
	w, ok := workstreamFor(leg)
	if !ok {
		return false, false
	}
	return w.involvement(leg)
}

// legPhysics is the physics of the leg transactionType names, or the zero
// physics when no compiled-in catalog declares it.
func legPhysics(transactionType string) LegPhysics {
	spec, _ := legSpecFor(transactionType)
	return spec.Physics
}

// init registers the compiled-in workstreams' ownership rows, over their
// legs, before any transmit can read the table (relay.RegisterOwnership is
// write-once and refuses a registration after the first read).
func init() {
	legs, rows, err := workstreamOwnership(workstreams)
	if err != nil {
		panic(err)
	}
	relay.RegisterOwnership(legs, rows)
}

// workstreamOwnership is the legs of ws's catalogs and the ownership rows
// their adapters declare. A row naming a leg no catalog of its own adapter
// declares, or a key two adapters both declare, is refused.
func workstreamOwnership(ws []workstream) (legs []string, rows map[relay.Key]relay.Rule, err error) {
	rows = map[relay.Key]relay.Rule{}
	for _, w := range ws {
		catalog := w.catalog()
		legs = append(legs, slices.Sorted(maps.Keys(catalog))...)
		for k, r := range w.ownershipRows() {
			if _, ok := catalog[k.Leg]; !ok {
				return nil, nil, fmt.Errorf("workstream %q: ownership row %s names no leg of its catalog", w.id(), k)
			}
			if _, dup := rows[k]; dup {
				return nil, nil, fmt.Errorf("workstream %q: ownership row %s is declared twice", w.id(), k)
			}
			rows[k] = r
		}
	}
	return legs, rows, nil
}

// ingressHandler is one ingress route a workstream declares: the observer
// tag the route carries, and its handler on a gateway.
type ingressHandler struct {
	tag     string
	handler func(*Gateway) http.HandlerFunc
}

// workstreamIngressRoute is the ingress route route of whichever compiled-in
// workstream declares it; ok is false when none does. Route names are unique
// across the compiled-in workstreams (TestWorkstreams_LegsAreUniqueAcrossCatalogs).
func workstreamIngressRoute(route string) (ingressHandler, bool) {
	for _, w := range workstreams {
		if h, ok := w.ingressRoutes()[route]; ok {
			return h, true
		}
	}
	return ingressHandler{}, false
}

// workstreamIngressRouteNames are the ingress routes the compiled-in
// workstreams declare, each workstream's sorted, in registry order.
func workstreamIngressRouteNames() []string {
	var out []string
	for _, w := range workstreams {
		out = append(out, slices.Sorted(maps.Keys(w.ingressRoutes()))...)
	}
	return out
}

// certificationAdapters are the compiled-in workstreams' certification
// adapters, in registry order.
func certificationAdapters() []certificationAdapter {
	var out []certificationAdapter
	for _, w := range workstreams {
		if a := w.certification(); a != nil {
			out = append(out, a)
		}
	}
	return out
}

// detectSpecies is the species a compiled-in workstream recognises payload
// as, or "" when none does.
func detectSpecies(payload []byte) string {
	for _, a := range certificationAdapters() {
		if s := a.species(payload); s != "" {
			return s
		}
	}
	return ""
}

// certificationAdapterFor is the adapter of the compiled-in workstream that
// certifies species, or nil.
func certificationAdapterFor(species string) certificationAdapter {
	for _, a := range certificationAdapters() {
		if a.certifies(species) {
			return a
		}
	}
	return nil
}

// profileFor is the profile a payload of species is certified against at
// line on leg ("" for any leg), by the workstream that certifies species.
func profileFor(species, line, leg string) (string, bool) {
	if a := certificationAdapterFor(species); a != nil {
		return a.profile(species, line, leg)
	}
	return "", false
}

// answerLineClaims is the order of lines to judge an answer on contract at
// and the lines the answer itself points to, by the adapter of the
// workstream that declares contract; none when that workstream certifies
// nothing.
func answerLineClaims(contract string, payload []byte) ([]string, map[string]bool) {
	for _, w := range workstreams {
		a := w.certification()
		if a == nil || !declaresContract(w, contract) {
			continue
		}
		return a.lineClaims(payload)
	}
	return nil, map[string]bool{}
}

// declaresContract reports whether w builds a line of contract natively.
func declaresContract(w workstream, contract string) bool {
	native, _ := w.contractVersions()
	for _, tok := range native {
		if c, _, ok := strings.Cut(tok, "@"); ok && c == contract {
			return true
		}
	}
	return false
}

// certificationExcuse lets each compiled-in workstream mark the issues of a
// reading that are a known consequence of how the validator checks its
// profiles (certificationAdapter.excuse).
func certificationExcuse(errs []fhirIssue, body []byte, line, profile string) {
	for _, a := range certificationAdapters() {
		a.excuse(errs, body, line, profile)
	}
}

// certifiedBuilds is the union of the compiled-in workstreams' certified
// builder and line pairs (workstream.certifiedBuilds, certifiedbuilds.go).
var certifiedBuilds = func() map[certifiedBuild]string {
	out := map[certifiedBuild]string{}
	for _, w := range workstreams {
		for pair, test := range w.certifiedBuilds() {
			out[pair] = test
		}
	}
	return out
}()

// workstreamContractVersions is every compiled-in workstream's tokens, as pick
// reads them from each, in registry order and each workstream's own order,
// each once.
func workstreamContractVersions(pick func(native, defaults []string) []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, w := range workstreams {
		for _, tok := range pick(w.contractVersions()) {
			if !seen[tok] {
				seen[tok] = true
				out = append(out, tok)
			}
		}
	}
	return out
}

// CanonicalContractVersion is the contract-version token the canonical
// validator (Config.Validator, FHIR_VALIDATE_URL) resolves: the first a
// compiled-in workstream names, or "" in a build whose workstreams name none.
func CanonicalContractVersion() string {
	for _, w := range workstreams {
		if tok := w.canonicalContractVersion(); tok != "" {
			return tok
		}
	}
	return ""
}

// workstreamKinds are the legs of the compiled-in workstreams' catalogs, each
// workstream's sorted, in registry order.
func workstreamKinds() []string {
	var out []string
	for _, w := range workstreams {
		out = append(out, slices.Sorted(maps.Keys(w.catalog()))...)
	}
	return out
}

// workstreamRoutes are the participant-facing routes every compiled-in
// workstream declares for role.
func (g *Gateway) workstreamRoutes(role string) (out []mountedRoute) {
	for _, w := range workstreams {
		out = append(out, w.routes(g, role)...)
	}
	return out
}

// legSpec is the per-leg-type contract the origination and inbound sides both
// read. ReqFrame is the originator's authority frame ("provider-tpo" for every
// PA leg today); RespFrame is the responder's. Op/RespOp
// are the request/response authority operations. Scope is the policy-derived
// min-necessary scope label (documentary; authz derives the real scope, and the
// network's lockstep conformance check pins this label to it). Physics is
// the Layer-2 leg classification — recorded now, load-bearing later
// (notification/async are headroom; no PA scenario exercises them).
type legSpec struct {
	ReqFrame  string
	RespFrame string
	Op        string
	RespOp    string
	Scope     string
	Physics   LegPhysics
	// Contract is the contract-version family this legType's payload belongs to
	// ("pa.pas"), consumed by the OriginateLeg version
	// filter and the response-frame stamp. "" = version-neutral: the leg's
	// payload is not governed by any declared contract line (base-R4 CER, CDex,
	// bespoke patient-dtr JSON) and is never filtered or stamped. Every adapter
	// row states it explicitly (pinned for PA by TestPACatalog_Contracts).
	Contract string
	// OperationHeader reports that the leg's request frame may name the
	// operation its body is the input of: the taxonomy declares frame
	// operations for it. Which operation values are served is the leg's own
	// decision.
	OperationHeader bool
	// Seam names where the leg's inbound request is judged, on the findings
	// its checks record (gatewayConformance.certify's seam strings).
	Seam string
}

// legDetail is what a workstream adapter adds to one routable leg of its
// taxonomy: the leg's physics in full, and the contract family its payload
// belongs to (legSpec.Contract), and the seam its inbound request is judged
// at (legSpec.Seam). The frames, operations and scope are the taxonomy's.
type legDetail struct {
	Physics  LegPhysics
	Contract string
	Seam     string
}

// catalogFromTaxonomy derives a workstream's leg catalog from its leg
// taxonomy and its adapter's detail, joined on the transaction type. The
// taxonomy must be the named workstream's, every routable taxonomy leg needs a
// detail row, every detail row needs a routable taxonomy leg, and a row's
// physics must be complete and of the taxonomy's kind; anything else is
// refused, so the catalog can drift from neither.
func catalogFromTaxonomy(t legtaxonomy.Taxonomy, name string, detail map[string]legDetail) (map[string]legSpec, error) {
	if t.Workstream != name {
		return nil, fmt.Errorf("the taxonomy is workstream %q's, not %q's", t.Workstream, name)
	}
	catalog := map[string]legSpec{}
	for _, l := range t.Legs {
		if !l.Routable() {
			continue
		}
		d, ok := detail[l.TransactionType]
		if !ok {
			return nil, fmt.Errorf("workstream %q: taxonomy leg %q has no adapter row", t.Workstream, l.TransactionType)
		}
		if d.Physics.Kind != KindRequestResponse {
			return nil, fmt.Errorf("workstream %q: leg %q is %s in the taxonomy, its adapter row says %v", t.Workstream, l.TransactionType, l.Physics, d.Physics.Kind)
		}
		if d.Physics.Effect == "" || d.Physics.Timing == "" || d.Physics.Locality == "" {
			return nil, fmt.Errorf("workstream %q: leg %q: the adapter row's physics %+v is incomplete", t.Workstream, l.TransactionType, d.Physics)
		}
		if d.Seam == "" {
			return nil, fmt.Errorf("workstream %q: leg %q: the adapter row names no seam", t.Workstream, l.TransactionType)
		}
		catalog[l.TransactionType] = legSpec{
			ReqFrame: l.Request.Frame, RespFrame: l.Response.Frame,
			Op: l.Request.Operation, RespOp: l.Response.Operation, Scope: l.Scope,
			Physics: d.Physics, Contract: d.Contract,
			OperationHeader: len(l.FrameOperations) > 0, Seam: d.Seam,
		}
	}
	for _, tx := range slices.Sorted(maps.Keys(detail)) {
		if _, ok := catalog[tx]; !ok {
			return nil, fmt.Errorf("workstream %q: adapter row %q names no routable taxonomy leg", t.Workstream, tx)
		}
	}
	return catalog, nil
}

// mustCatalog is catalogFromTaxonomy for a compiled-in workstream: a catalog
// that cannot be derived stops the binary when the package loads.
func mustCatalog(t legtaxonomy.Taxonomy, name string, detail map[string]legDetail) map[string]legSpec {
	c, err := catalogFromTaxonomy(t, name, detail)
	if err != nil {
		panic(err)
	}
	return c
}
