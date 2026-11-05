//go:build !shn_coreonly

// workstream_pa.go — the Prior Authorization workstream's adapter (design
// §4.2): its leg catalog, derived from the gateway's copy of the network's leg
// taxonomy, its participant-facing routes, its ownership rows
// (workstream_pa_ownership.go) and which legs name involved patients
// (workstream_pa_involved.go). The core reads them only through
// the workstream registry (workstreams.go). A second workstream is a sibling
// adapter with its own taxonomy; the core does not change.
package engine

import (
	"fmt"
	"net/http"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/internal/legtaxonomy"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// paWorkstream is the Prior Authorization workstream's adapter: its catalog,
// its participant-facing routes, its ownership rows and its legs'
// involvement.
type paWorkstream struct {
	legs  map[string]legSpec
	edits []relayEdit
}

// paAdapter is the adapter the registry compiles in (workstreams_zones.go). It
// holds its edits as it holds its catalog, so Go initializes both before the
// registry reads them.
var paAdapter = paWorkstream{legs: paCatalog, edits: paRelayEdits}

func (paWorkstream) id() string                    { return workstreamPA }
func (w paWorkstream) catalog() map[string]legSpec { return w.legs }
func (w paWorkstream) relayEdits() []relayEdit     { return w.edits }
func (paWorkstream) routes(g *Gateway, role string) []mountedRoute {
	return g.paRoutes(role)
}
func (paWorkstream) ownershipRows() map[relay.Key]relay.Rule  { return paLegOwnership }
func (paWorkstream) ingressRoutes() map[string]ingressHandler { return paIngressRoutes }

// refusalReasons: the PA adapter's (workstream_pa_refusalreason.go).
func (paWorkstream) refusalReasons() []refusalReason { return paRefusalReasons() }

// ingressOperations: the CRD ingress records the hook of the CDS service it
// answers (cdsIngressServices), in discovery order.
func (paWorkstream) ingressOperations() []string {
	var out []string
	for _, s := range cdsIngressServices {
		out = append(out, s.Hook)
	}
	return out
}

// canonicalContractVersion: the canonical validator resolves PAS at 2.0, the
// line every deployment's FHIR_VALIDATE_URL serves.
func (paWorkstream) canonicalContractVersion() string { return shnsdk.ContractPAPAS20 }

// The PA workstream's ingress routes, as exchange records name them.
const (
	RouteCRD         = "crd"
	RouteDTR         = "dtr"
	RoutePAS         = "pas"
	RoutePASInquire  = "pas-inquire"
	RouteEligibility = "eligibility"
)

// contractVersions: PA's contract lines are the SDK's native set and default
// (sdkContractVersions), which carry no pa.pdex token: a test holds the SDK the
// gateway requires to that, and ParseDeclaredContractVersions refuses the token.
func (paWorkstream) contractVersions() (native, defaults []string) {
	return sdkContractVersions()
}

// retiredPDexToken is the PDex 2.1 contract-version token, which no gateway
// declares: a gateway serves no Patient Access API and builds no PDex
// resource. The gateway owns the literal: the SDK retires its own name for it,
// and the refusal names it to the operator, so its bytes never change.
const retiredPDexToken = "pa.pdex@2.1"

// ErrPDexUndeclared refuses a declared set naming pa.pdex@2.1.
var ErrPDexUndeclared = fmt.Errorf("contract-version token %q is no longer declared by any gateway: a gateway serves no Patient Access API and builds no PDex resource, so a declaration must not claim it: remove it from the declared set (SHN_CONTRACT_VERSIONS)", retiredPDexToken)

// retiredContractVersion: PA retired pa.pdex@2.1 (contractRetirer).
func (paWorkstream) retiredContractVersion(tok string) error {
	if tok == retiredPDexToken {
		return ErrPDexUndeclared
	}
	return nil
}

// paIngressRoutes are the PA workstream's Da Vinci ingress handlers, with the
// observer tag each route has always carried.
var paIngressRoutes = map[string]ingressHandler{
	RouteCRD:         {"crd-ingress", func(g *Gateway) http.HandlerFunc { return g.handleCRDIngress }},
	RouteDTR:         {"dtr-ingress", func(g *Gateway) http.HandlerFunc { return g.handleDTRIngress }},
	RoutePAS:         {"pas-ingress", func(g *Gateway) http.HandlerFunc { return g.handlePASIngress }},
	RoutePASInquire:  {"pas-inquire-ingress", func(g *Gateway) http.HandlerFunc { return g.handlePASInquireIngress }},
	RouteEligibility: {"eligibility-ingress", func(g *Gateway) http.HandlerFunc { return g.handleEligibilityIngress }},
	RouteCDexSubmit:  {"cdex-ingress", func(g *Gateway) http.HandlerFunc { return g.handleCDexSubmitIngress }},
	// A submission to a payer's return path: the same handler, routed by the
	// path (cdexroute).
	RouteCDexReturnSubmit: {"cdex-ingress", func(g *Gateway) http.HandlerFunc { return g.handleCDexSubmitIngress }},
	// The payer's attachment-request Task, at a provider's base on the payer's
	// gateway (cdexroute.ProviderBase).
	RouteCDexRequestBase: {"cdex-request-ingress", func(g *Gateway) http.HandlerFunc { return g.handleCDexRequestIngress }},
	// The same Task at the payer's plain Task endpoint, routed by the
	// organization its Task.owner names.
	RouteCDexRequest: {"cdex-request-ingress", func(g *Gateway) http.HandlerFunc { return g.handleCDexRequestByOwner }},
}

// paCatalog is the PA workstream's leg catalog, keyed by legType (= envelope
// TransactionType), derived from the gateway's copy of the network's leg
// taxonomy (legtaxonomy.PA) and paLegs. The recipient is NOT a catalog
// property: payer legs derive the recipient from the patient's Coverage via
// recipientFor (no default), facility/phg legs target a registry LookupByRole
// result, so the recipient stays a call-time argument.
var paCatalog = mustCatalog(legtaxonomy.PA(), "pa", paLegs)

var (
	paReadOnly = LegPhysics{Kind: KindRequestResponse, Effect: EffectReadOnly, Timing: TimingSync, Locality: LocalitySubstrate}
	paMutating = LegPhysics{Kind: KindRequestResponse, Effect: EffectMutating, Timing: TimingSync, Locality: LocalitySubstrate}
)

// paLegs is what the PA adapter adds to each routable leg of its taxonomy:
// the leg's physics and its contract family. Every row states its contract
// explicitly; "" is a leg whose payload no declared contract line governs
// (base-R4 CER, bespoke patient-dtr JSON). The frames, operations and
// scopes are the taxonomy's, which keeps every operation distinct (a token
// bound to one leg's operation cannot be lifted onto another's, AI-11) and
// gives the amended re-POST its own scope (pas-update-bundle beside
// pas-bundle, FR-34).
var paLegs = map[string]legDetail{
	"coverage-eligibility":    {Physics: paReadOnly, Contract: "", Seam: paSeamPayer},
	"dtr-questionnaire-fetch": {Physics: paReadOnly, Contract: "pa.dtr", Seam: paSeamPayer},
	"patient-dtr":             {Physics: paReadOnly, Contract: "", Seam: "phg-inbound"},
	"crd-order-select":        {Physics: paReadOnly, Contract: "pa.crd", Seam: paSeamPayer},
	"crd-order-dispatch":      {Physics: paReadOnly, Contract: "pa.crd", Seam: paSeamPayer},
	// The submit and the amended re-POST (F-B1) change the payer's record.
	"pas-claim":        {Physics: paMutating, Contract: "pa.pas", Seam: paSeamPayer},
	"pas-claim-update": {Physics: paMutating, Contract: "pa.pas", Seam: paSeamPayer},
	// The inquiry (Claim/$inquire) reads the payer's own record: the inquiry
	// itself decides nothing, though the answer may report a decision the payer
	// already made.
	"pas-claim-inquire": {Physics: paReadOnly, Contract: "pa.pas", Seam: paSeamPayer},
	// A CDex $submit-attachment adds documentation to the payer's record of a
	// prior authorization. It is carried as sent whatever the two sides' PA
	// lines: no declared contract line governs it until a second CDex line
	// gives the network lines to route between.
	"cdex-attachment-submit": {Physics: paMutating, Contract: "", Seam: paSeamPayer},
	// A payer's CDex attachment-request Task asks the provider's own system to
	// create a Task. It is carried whatever the two sides' PA lines, as the
	// submission is.
	// The provider takes it only once its gateway is configured with its own
	// system's Task endpoint, so no gateway declares it until then.
	"cdex-attachment-request": {Physics: paMutating, Contract: "", Seam: paSeamProvider, TakenWhenSet: settingProviderCDexTaskURL},
}

// paSeamPayer is the seam a payer's inbound PA request is judged at.
const paSeamPayer = "payer-native"

// paSeamProvider is the seam a provider's inbound PA request is judged at: a
// payer's request to the provider's own system.
const paSeamProvider = "provider-native"

// originatedAnswerCheck: an answer to a CRD leg this gateway originates is
// certified against the CDS Hooks response rules, as a relayed one is
// (crdAnswerOutcome). A carried leg (the Da Vinci ingress) certifies the
// answer itself.
func (paWorkstream) originatedAnswerCheck(leg string) legAnswerCheck {
	switch leg {
	case "crd-order-select", "crd-order-dispatch":
		return certifyCDSHooksAnswer
	}
	return nil
}

// paRoutes are the PA workstream's participant-facing routes for role: the
// provider's Da Vinci ingress, which carries its participant's own requests,
// and the payer's, which carries its participant's attachment-request Tasks.
func (g *Gateway) paRoutes(role string) []mountedRoute {
	var out []mountedRoute
	route := func(pattern string, credential routeCredential, h http.HandlerFunc) {
		out = append(out, mountedRoute{pattern: pattern, handler: h, credential: credential})
	}
	// A documentation attachment submission is read to this gateway's ceiling,
	// coded or not.
	attachment := func(pattern string, h http.HandlerFunc) {
		out = append(out, mountedRoute{pattern: pattern, handler: h, credential: noCredential, bodyLimit: g.attachmentCeiling()})
	}
	switch role {
	case "provider":
		if !g.cfg.IngressEnabled {
			return nil
		}
		// Every ingress answer carries X-Correlation-Id and X-SHN-Leg-Id
		// (correlationheader.go): the caller's trace value and the leg's id,
		// settled before the handler runs.
		route("GET /cds-services", noCredential, g.withIngressCorrelation(g.handleCDSDiscovery))
		route("POST /cds-services/{id}", noCredential, g.ingressRoute(RouteCRD))
		route("POST /Questionnaire/$questionnaire-package", noCredential, g.ingressRoute(RouteDTR))
		route("POST /Claim/$submit", noCredential, g.ingressRoute(RoutePAS))
		route("POST /Claim/$inquire", noCredential, g.ingressRoute(RoutePASInquire))
		// The base R4 operation a system submits a coverage eligibility
		// request with (IHE FAIS ITI-124).
		route("POST /CoverageEligibilityRequest/$submit", noCredential, g.ingressRoute(RouteEligibility))
		// CDex $submit-attachment, at the plain endpoint and at each payer's
		// return path (cdexroute.Prefix), with or without the operation's name;
		// and the payers a submission can be addressed to (CDexPayersPath).
		attachment("POST /$submit-attachment", g.ingressRoute(RouteCDexSubmit))
		attachment("POST /cdex/payer/{payer}", g.ingressRoute(RouteCDexReturnSubmit))
		attachment("POST /cdex/payer/{payer}/$submit-attachment", g.ingressRoute(RouteCDexReturnSubmit))
		route("GET /cdex/payers", noCredential, g.withIngressCorrelation(g.handleCDexPayers))
		// FR-37: the ingress edge's own CapabilityStatement.
		route("GET /metadata", noCredential, g.handleIngressMetadata)
		if g.ingressAuth != nil {
			route("POST /oauth/token", carriesCredential, g.ingressAuth.handleToken)
			route("GET /.well-known/smart-configuration", noCredential, g.ingressAuth.handleSmartConfig)
		}
		if g.cfg.IngressBaseURL != "" {
			// HRex 1.2.0 endpoint discovery:
			// version-specific endpoint codes for the Da Vinci-facing edge.
			route("GET /.well-known/davinci-configuration", noCredential, g.handleDavinciConfiguration)
		}
	case "payer":
		if !g.cfg.IngressEnabled {
			return nil
		}
		// The payer's own system creates an attachment-request Task at a
		// provider's base (cdexroute.ProviderPrefix), listed at
		// CDexProvidersPath, or at the plain Task endpoint, which routes it by
		// the organization its Task.owner names.
		route("POST /cdex/provider/{provider}/Task", noCredential, g.ingressRoute(RouteCDexRequestBase))
		route("POST /Task", noCredential, g.ingressRoute(RouteCDexRequest))
		route("GET /cdex/providers", noCredential, g.withIngressCorrelation(g.handleCDexProviders))
		if g.ingressAuth != nil {
			route("POST /oauth/token", carriesCredential, g.ingressAuth.handleToken)
			route("GET /.well-known/smart-configuration", noCredential, g.ingressAuth.handleSmartConfig)
		}
	}
	return out
}
