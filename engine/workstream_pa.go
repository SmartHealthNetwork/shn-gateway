// workstream_pa.go — the Prior Authorization workstream's adapter (design
// §4.2): its leg catalog, derived from the gateway's copy of the network's leg
// taxonomy, its participant-facing routes, its ownership rows
// (workstream_pa_ownership.go) and which legs name involved patients
// (workstream_pa_involved.go). The core reads them only through
// the workstream registry (workstreams.go). A second workstream is a sibling
// adapter with its own taxonomy; the core does not change.
package engine

import (
	"net/http"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/internal/legtaxonomy"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// paWorkstream is the Prior Authorization workstream's adapter: its catalog,
// its participant-facing routes, its ownership rows and its legs'
// involvement.
type paWorkstream struct {
	legs map[string]legSpec
}

// paAdapter is the adapter the registry compiles in (workstreams.go).
var paAdapter = paWorkstream{legs: paCatalog}

func (paWorkstream) id() string                    { return workstreamPA }
func (w paWorkstream) catalog() map[string]legSpec { return w.legs }
func (paWorkstream) routes(g *Gateway, role string) []mountedRoute {
	return g.paRoutes(role)
}
func (paWorkstream) ownershipRows() map[relay.Key]relay.Rule  { return paLegOwnership }
func (paWorkstream) ingressRoutes() map[string]ingressHandler { return paIngressRoutes }

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

// contractVersions: PA's contract lines are the SDK's, without pa.pdex@2.1
// (sdkContractVersions).
func (paWorkstream) contractVersions() (native, defaults []string) {
	return sdkContractVersions()
}

// paIngressRoutes are the PA workstream's Da Vinci ingress handlers, with the
// observer tag each route has always carried.
var paIngressRoutes = map[string]ingressHandler{
	RouteCRD:         {"crd-ingress", func(g *Gateway) http.HandlerFunc { return g.handleCRDIngress }},
	RouteDTR:         {"dtr-ingress", func(g *Gateway) http.HandlerFunc { return g.handleDTRIngress }},
	RoutePAS:         {"pas-ingress", func(g *Gateway) http.HandlerFunc { return g.handlePASIngress }},
	RoutePASInquire:  {"pas-inquire-ingress", func(g *Gateway) http.HandlerFunc { return g.handlePASInquireIngress }},
	RouteEligibility: {"eligibility-ingress", func(g *Gateway) http.HandlerFunc { return g.handleEligibilityIngress }},
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
}

// paSeamPayer is the seam a payer's inbound PA request is judged at.
const paSeamPayer = "payer-native"

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
// provider's Da Vinci ingress, which carries its participant's own requests.
// The payer serves no participant-facing PA route of its own.
func (g *Gateway) paRoutes(role string) []mountedRoute {
	var out []mountedRoute
	route := func(pattern string, h http.HandlerFunc) {
		out = append(out, mountedRoute{pattern: pattern, handler: h})
	}
	switch role {
	case "provider":
		if !g.cfg.IngressEnabled {
			return nil
		}
		// Every ingress answer carries X-Correlation-Id and X-SHN-Leg-Id
		// (correlationheader.go): the caller's trace value and the leg's id,
		// settled before the handler runs.
		route("GET /cds-services", g.withIngressCorrelation(g.handleCDSDiscovery))
		route("POST /cds-services/{id}", g.ingressRoute(RouteCRD))
		route("POST /Questionnaire/$questionnaire-package", g.ingressRoute(RouteDTR))
		route("POST /Claim/$submit", g.ingressRoute(RoutePAS))
		route("POST /Claim/$inquire", g.ingressRoute(RoutePASInquire))
		// The base R4 operation a system submits a coverage eligibility
		// request with (IHE FAIS ITI-124).
		route("POST /CoverageEligibilityRequest/$submit", g.ingressRoute(RouteEligibility))
		// FR-37: the ingress edge's own CapabilityStatement.
		route("GET /metadata", g.handleIngressMetadata)
		if g.ingressAuth != nil {
			route("POST /oauth/token", g.ingressAuth.handleToken)
			route("GET /.well-known/smart-configuration", g.ingressAuth.handleSmartConfig)
		}
		if g.cfg.IngressBaseURL != "" {
			// HRex 1.2.0 endpoint discovery:
			// version-specific endpoint codes for the Da Vinci-facing edge.
			route("GET /.well-known/davinci-configuration", g.handleDavinciConfiguration)
		}
	}
	return out
}
