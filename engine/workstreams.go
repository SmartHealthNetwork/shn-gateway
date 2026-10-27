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
	// ingressOperations are the operations the workstream's ingress records
	// name beside the leg (ExchangeRecord.Operation), or nil. The frame
	// operations its legs declare come from its catalog.
	ingressOperations() []string
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
	// proves it and the shn-sdk release it holds from (certifiedbuilds.go), or
	// nil.
	certifiedBuilds() map[certifiedBuild]certification
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
	// nil for a workstream that keeps none there; an error refuses the
	// gateway's configuration, and New refuses with it.
	newGatewayState(g *Gateway) (gatewayState, error)
	// newNativeState is the workstream's own forward state on a new native
	// responder n, or nil for a workstream that keeps none there. It runs
	// before the responder's options, so an option the workstream declares
	// writes into it.
	newNativeState(n *nativeResponder) nativeState
	// lineBridge is the workstream's contribution to module 2 (IG-line
	// translation): its rows of the compatibility manifest and the legs and
	// answers its chain treats specially (lineBridgeRows); the zero value for
	// a workstream that contributes none.
	lineBridge() lineBridgeRows
	// relayEdits are the registered edits on the workstream's legs
	// (relay.Edit rows); every leg each names is a leg of its catalog.
	relayEdits() []relayEdit
}

// gatewayState is one workstream's or module's own state on a gateway
// (workstream.newGatewayState, gatewayModule.newGatewayState). The workstream
// or module that made it is the one that reads it.
type gatewayState any

// nativeState is one workstream's own forward state on a native responder
// (workstream.newNativeState). The workstream that made it is the one that
// reads it.
type nativeState any

// workstreams are the workstreams compiled into this build: the zones'
// (zoneWorkstreams: Prior Authorization and Federated Query Services, none in
// the core-only profile) and a test profile's. Another workstream is a sibling
// adapter with its own taxonomy and catalog. Only the lookups in this file
// read it. Each entry is an adapter variable that holds its catalog, so Go
// initializes every catalog before the registry (a call through the interface
// would order nothing).
var workstreams = append(zoneWorkstreams(), profileWorkstreams()...)

// gatewayModule is one module compiled into this gateway that is not a
// workstream: a client of the network the participant's gateway runs for it,
// such as on-behalf building (module 1), which builds and sends a
// participant's requests for a system that cannot. The core builds the
// module's state and mounts its routes through it and names none of its code.
type gatewayModule interface {
	// id names the module's own state on a gateway (Gateway.moduleStates).
	id() string
	// newGatewayState is the module's own state on a new gateway g, built
	// after New's options, or nil for a module that keeps none there; an
	// error refuses the gateway's configuration, and New refuses with it.
	newGatewayState(g *Gateway) (gatewayState, error)
	// routes are the module's routes on g. role is the gateway's own
	// (routes() passes cfg.Role); a module may declare its routes once, for
	// that role, as New builds its state, and return what it declared.
	routes(g *Gateway, role string) []mountedRoute
}

// modules are the modules compiled into this build (zoneModules: on-behalf
// building, none in the core-only profile). Only the lookups in this file
// read it.
var modules = zoneModules()

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
	leg, _ := canonicalLeg(transactionType)
	for _, w := range workstreams {
		if s, ok := w.catalog()[leg]; ok {
			return s, true
		}
	}
	return legSpec{}, false
}

// canonicalLeg is the leg a stated transaction-type name names, in either
// form: the flat name (wire protocol 1.1.0, "pas-claim") or the prefixed one
// (2.0.0, "pa.pas-claim"). A name no leg has is returned as stated, unknown,
// and is refused where a leg is looked up, as it always was.
func canonicalLeg(stated string) (string, bool) {
	for _, w := range workstreams {
		for tx, s := range w.catalog() {
			if tx == stated || (s.WireName != "" && s.WireName == stated) {
				return tx, true
			}
		}
	}
	return stated, false
}

// wireName is the name this gateway sends for the leg txType: its prefixed
// name. A leg whose catalog row has none (a test's own row) keeps its flat
// name.
func wireName(txType string) string {
	if s, ok := legSpecFor(txType); ok && s.WireName != "" {
		return s.WireName
	}
	return txType
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
// entry. The first workstream's refusal is returned instead.
func workstreamGatewayStates(g *Gateway) (map[string]gatewayState, error) {
	out := map[string]gatewayState{}
	for _, w := range workstreams {
		st, err := w.newGatewayState(g)
		if err != nil {
			return nil, err
		}
		if st != nil {
			out[w.id()] = st
		}
	}
	return out, nil
}

// moduleGatewayStates is each compiled-in module's own state on a new gateway
// g, by module id; a module that keeps none there has no entry. The first
// module's refusal is returned instead.
func moduleGatewayStates(g *Gateway) (map[string]gatewayState, error) {
	out := map[string]gatewayState{}
	for _, m := range modules {
		st, err := m.newGatewayState(g)
		if err != nil {
			return nil, err
		}
		if st != nil {
			out[m.id()] = st
		}
	}
	return out, nil
}

// workstreamNativeStates is each compiled-in workstream's own forward state
// on a new native responder n, by workstream id; a workstream that keeps none
// there has no entry.
func workstreamNativeStates(n *nativeResponder) map[string]nativeState {
	out := map[string]nativeState{}
	for _, w := range workstreams {
		if st := w.newNativeState(n); st != nil {
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

// workstreamCompatSteps are the compiled-in workstreams' compatibility
// manifest rows, in registry order (compatManifest).
func workstreamCompatSteps() []CompatStep {
	var out []CompatStep
	for _, w := range workstreams {
		out = append(out, w.lineBridge().steps...)
	}
	return out
}

// workstreamLegSet is the set of legs rows names across the compiled-in
// workstreams' module 2 contributions (envelopeEgressLegs, verbatimChainLegs).
func workstreamLegSet(rows func(lineBridgeRows) []string) map[string]bool {
	out := map[string]bool{}
	for _, w := range workstreams {
		for _, leg := range rows(w.lineBridge()) {
			out[leg] = true
		}
	}
	return out
}

// answerBridges are the answers the compiled-in workstreams' module 2
// contributions edit, by relay key (AdaptAnswer); set at package init.
var answerBridges map[relay.Key]answerBridge

// init derives module 2's tables from the compiled-in workstreams' lineBridge
// rows. It runs after every package-level variable is set: E-08's bridge walks
// the manifest (chainFor), so the tables cannot be initializers of their own
// without an initialization cycle.
func init() {
	compatManifest = workstreamCompatSteps()
	envelopeEgressLegs = workstreamLegSet(func(r lineBridgeRows) []string { return r.envelopeEgressLegs })
	verbatimChainLegs = workstreamLegSet(func(r lineBridgeRows) []string { return r.verbatimChainLegs })
	answerBridges = map[relay.Key]answerBridge{}
	for _, w := range workstreams {
		maps.Copy(answerBridges, w.lineBridge().answers)
	}
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

// relayEdits is the closed edit registry: every compiled-in
// workstream's edits, in the relay package's id order.
var relayEdits = mustRelayEdits(workstreamRelayEdits(workstreams))

func mustRelayEdits(edits []relayEdit, err error) []relayEdit {
	if err != nil {
		panic(err)
	}
	return edits
}

// workstreamRelayEdits is the edits ws's adapters register, in the relay
// package's id order. An edit naming a leg no catalog of its own adapter
// declares is refused here; an id outside the relay package's closed set, or
// an id two edits declare, is refused by relay.ValidateEdits.
func workstreamRelayEdits(ws []workstream) ([]relayEdit, error) {
	order := map[relay.EditID]int{}
	for i, id := range relay.EditIDs() {
		order[id] = i
	}
	var out []relayEdit
	for _, w := range ws {
		catalog := w.catalog()
		for _, e := range w.relayEdits() {
			for _, leg := range e.Legs {
				if _, ok := catalog[leg]; !ok {
					return nil, fmt.Errorf("workstream %q: edit %s names %q, no leg of its catalog", w.id(), e.ID, leg)
				}
			}
			out = append(out, e)
		}
	}
	if err := relay.ValidateEdits(out); err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b relayEdit) int { return order[a.ID] - order[b.ID] })
	return out, nil
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

// workstreamOperations are the operations an exchange record names on the
// compiled-in workstreams' legs (operationsOf).
func workstreamOperations() []string { return operationsOf(workstreams) }

// operationsOf are the operations an exchange record names on ws's legs:
// each workstream's ingress operations, then the frame operations its
// catalog's legs declare, by transaction type, each value once.
func operationsOf(ws []workstream) []string {
	var out []string
	add := func(op string) {
		if !slices.Contains(out, op) {
			out = append(out, op)
		}
	}
	for _, w := range ws {
		for _, op := range w.ingressOperations() {
			add(op)
		}
		catalog := w.catalog()
		for _, leg := range slices.Sorted(maps.Keys(catalog)) {
			for _, op := range catalog[leg].FrameOperations {
				add(op)
			}
		}
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
// and what the answer itself points to (each claim a set of lines), by the
// adapter of the workstream that declares contract; none when that workstream
// certifies nothing.
func answerLineClaims(contract string, payload []byte) ([]string, []claimSet) {
	for _, w := range workstreams {
		a := w.certification()
		if a == nil || !declaresContract(w, contract) {
			continue
		}
		return a.lineClaims(payload)
	}
	return nil, nil
}

// answerCandidateGroups is the compiled-in workstream's candidate groups for a
// payer's answer of contract (certificationAdapter.candidateGroups); nil when
// no workstream restricts them.
func answerCandidateGroups(contract string, payload []byte) [][]string {
	for _, w := range workstreams {
		a := w.certification()
		if a == nil || !declaresContract(w, contract) {
			continue
		}
		return a.candidateGroups(contract, payload)
	}
	return nil
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
var certifiedBuilds = func() map[certifiedBuild]certification {
	out := map[certifiedBuild]certification{}
	for _, w := range workstreams {
		for pair, c := range w.certifiedBuilds() {
			out[pair] = c
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

// moduleRoutes are every compiled-in module's routes for role on g, in
// registry order.
func (g *Gateway) moduleRoutes(role string) (out []mountedRoute) {
	for _, m := range modules {
		out = append(out, m.routes(g, role)...)
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
	// FrameOperations are the operations the taxonomy declares the leg's
	// request frame may name as the one its body is the input of, or nil for
	// a leg whose frame names none. Which of them are served is the leg's own
	// decision.
	FrameOperations []string
	// Seam names where the leg's inbound request is judged, on the findings
	// its checks record (gatewayConformance.certify's seam strings).
	Seam string
	// WireName is the leg's name on the wire from wire protocol 2.0.0: the
	// transaction type with its workstream's prefix (the taxonomy's
	// stagedName, "pa.pas-claim"). The catalog stays keyed by the flat name.
	WireName string
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
		if want := t.Workstream + "." + l.TransactionType; l.StagedName != want {
			return nil, fmt.Errorf("workstream %q: leg %q: its prefixed name is %q, not %q", t.Workstream, l.TransactionType, l.StagedName, want)
		}
		catalog[l.TransactionType] = legSpec{
			ReqFrame: l.Request.Frame, RespFrame: l.Response.Frame,
			Op: l.Request.Operation, RespOp: l.Response.Operation, Scope: l.Scope,
			Physics: d.Physics, Contract: d.Contract,
			FrameOperations: frameOperations(l), Seam: d.Seam, WireName: l.StagedName,
		}
	}
	for _, tx := range slices.Sorted(maps.Keys(detail)) {
		if _, ok := catalog[tx]; !ok {
			return nil, fmt.Errorf("workstream %q: adapter row %q names no routable taxonomy leg", t.Workstream, tx)
		}
	}
	return catalog, nil
}

// frameOperations are the frame operations the taxonomy declares for l, or
// nil when it declares none.
func frameOperations(l legtaxonomy.Leg) []string {
	if len(l.FrameOperations) == 0 {
		return nil
	}
	return slices.Clone(l.FrameOperations)
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
