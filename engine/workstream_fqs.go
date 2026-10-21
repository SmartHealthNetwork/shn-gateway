// workstream_fqs.go — the Federated Query Services workstream's adapter
// (design §4.2): its one leg, the federated query, derived from the gateway's
// copy of the network's leg taxonomy (fqs.json), its Da Vinci ingress route,
// and what each transmit of it may carry. Its other files hold the leg's
// recipient handler (workstream_fqs_inbound.go), the facility's records
// answer (workstream_fqs_records.go), the ingress handler
// (workstream_fqs_ingress.go) and the data-request Task's reading
// (workstream_fqs_task.go). PA's attachment fetch (UC-05) composes this leg;
// the core reads it only through the workstream registry (workstreams.go).
package engine

import (
	"net/http"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/internal/legtaxonomy"
)

// workstreamFQS is the WorkstreamType tag for the Federated Query Services
// workstream's content.
const workstreamFQS = "fqs"

// fqsWorkstream is the Federated Query Services workstream's adapter.
type fqsWorkstream struct {
	legs map[string]legSpec
}

// fqsAdapter is the adapter the registry compiles in (workstreams.go).
var fqsAdapter = fqsWorkstream{legs: mustCatalog(legtaxonomy.FQS(), "fqs", fqsLegs)}

// fqsLegs is what the adapter adds to its taxonomy's one leg: a read of the
// facility's records, whose payload (CDex) no declared contract line governs.
var fqsLegs = map[string]legDetail{
	"federated-query": {
		Physics:  LegPhysics{Kind: KindRequestResponse, Effect: EffectReadOnly, Timing: TimingSync, Locality: LocalitySubstrate},
		Contract: "", Seam: "facility-inbound",
	},
}

func (fqsWorkstream) id() string                    { return workstreamFQS }
func (w fqsWorkstream) catalog() map[string]legSpec { return w.legs }

// RouteFederatedQuery is the federated query's ingress route, as exchange
// records name it.
const RouteFederatedQuery = "federated-query"

// legFederatedQuery is the federated query as its recipient handler answers
// it.
var legFederatedQuery = inboundLeg{"facility-disclosure", "federated-query-response", "federated-query"}

func (fqsWorkstream) routes(g *Gateway, role string) []mountedRoute {
	return g.fqsRoutes(role)
}

// fqsRoutes are the Federated Query Services workstream's participant-facing
// routes for role: the provider's Da Vinci ingress for a consented federated
// query, the CDex task-based data request a data consumer sends a data
// source. It carries the participant's own request, so it is mounted where
// the provider's ingress is (paRoutes).
func (g *Gateway) fqsRoutes(role string) []mountedRoute {
	var out []mountedRoute
	route := func(pattern string, h http.HandlerFunc) {
		out = append(out, mountedRoute{pattern: pattern, handler: h})
	}
	switch role {
	case "provider":
		if !g.cfg.IngressEnabled {
			return nil
		}
		route("POST /Task", g.ingressRoute(RouteFederatedQuery))
	}
	return out
}

// ingressRoutes: the federated query's ingress handler, with the observer
// tag the route has always carried.
func (fqsWorkstream) ingressRoutes() map[string]ingressHandler { return fqsIngressRoutes }

var fqsIngressRoutes = map[string]ingressHandler{
	RouteFederatedQuery: {"federated-query-ingress", func(g *Gateway) http.HandlerFunc {
		return g.handleFederatedQueryIngress
	}},
}

// canonicalContractVersion and contractVersions: the federated query is
// version-neutral, so the workstream names no contract line.
func (fqsWorkstream) canonicalContractVersion() string { return "" }
func (fqsWorkstream) contractVersions() (native, defaults []string) {
	return nil, nil
}

// certifiedBuilds: with no contract line, the workstream builds no message at
// a line, so it has no builder and line pair to certify.
func (fqsWorkstream) certifiedBuilds() map[certifiedBuild]string { return nil }

// certification: the federated query's payload (CDex) is governed by no
// declared contract line, so the workstream certifies none.
func (fqsWorkstream) certification() certificationAdapter { return nil }

// originatedAnswerCheck: the originator certifies the facility's searchset
// itself (handleUC05's ingress validation), so the leg has no check here.
func (fqsWorkstream) originatedAnswerCheck(string) legAnswerCheck { return nil }

// answer: the facility's gateway answers the query from its own records, under
// the patient's consent (handleFederatedQueryInbound).
func (fqsWorkstream) answer(leg string) answerHandler {
	if leg != "federated-query" {
		return nil
	}
	return (*Gateway).handleFederatedQueryInbound
}

// forward: the native responder sends no federated query; the facility's
// gateway answers it itself (answer).
func (fqsWorkstream) forward(string) forwardHandler { return nil }

// involvement and requestMembers: a federated query names the one patient its
// consent covers, so it names no other patient for the involved list.
func (fqsWorkstream) involvement(string) (request, answer bool)   { return false, false }
func (fqsWorkstream) requestMembers(string, []byte) []namedMember { return nil }

// The federated query keeps no state in the in-memory Store, and so has
// nothing to check of the configured one.
func (fqsWorkstream) newMemState() memState  { return nil }
func (fqsWorkstream) checkStore(Store) error { return nil }

// newGatewayState: the federated query keeps no state of its own on the
// gateway.
func (fqsWorkstream) newGatewayState(*Gateway) gatewayState { return nil }

// FQSLegOperations returns a fresh copy of the Federated Query Services
// workstream catalog's operation bindings, keyed by envelope TransactionType,
// as PALegOperations returns PA's: the federated query.
func FQSLegOperations() map[string]LegOperation {
	out := make(map[string]LegOperation, len(fqsAdapter.legs))
	for legType, spec := range fqsAdapter.legs {
		out[legType] = LegOperation{
			RequestFrame:  spec.ReqFrame,
			RequestOp:     spec.Op,
			ResponseFrame: spec.RespFrame,
			ResponseOp:    spec.RespOp,
			Scope:         spec.Scope,
		}
	}
	return out
}

// ownershipRows: what each transmit of the federated query may carry
// (relay.RegisterOwnership adds the rows every leg has: the requester's
// relayed application error and the refusals).
func (fqsWorkstream) ownershipRows() map[relay.Key]relay.Rule { return fqsLegOwnership }

var fqsLegOwnership = func() map[relay.Key]relay.Rule {
	k := func(role relay.Role, dir relay.Direction, outcome relay.Outcome) relay.Key {
		return relay.Key{Leg: "federated-query", Role: role, Direction: dir, Outcome: outcome}
	}
	relayed := []relay.Ownership{relay.OwnershipRelayed}
	authored := []relay.Ownership{relay.OwnershipAuthored}
	return map[relay.Key]relay.Rule{
		// A query the participant's own system sends (a CDex data-request
		// Task) is relayed exactly; one the requester's own workflow makes
		// (PA's attachment fetch) is the gateway's own message.
		k(relay.RoleRequester, relay.DirectionRequest, relay.OutcomeCarried):    {Allowed: relayed},
		k(relay.RoleRequester, relay.DirectionRequest, relay.OutcomeOriginated): {Allowed: authored, Builders: []relay.BuilderID{relay.BuilderSDKFederatedQuery}},
		// The facility's answer reaches the requester's system exactly as it
		// arrived.
		k(relay.RoleRequester, relay.DirectionResponse, relay.OutcomeAnswered): {Allowed: relayed},
		// The facility's gateway answers with its own message, built from the
		// facility's records.
		k(relay.RoleRecipient, relay.DirectionResponse, relay.OutcomeAnswered): {Allowed: authored, Builders: []relay.BuilderID{relay.BuilderCDexFulfillment}},
	}
}()
