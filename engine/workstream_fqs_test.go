package engine

import (
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

// TestFQSAdapter_AnswersTheFederatedQuery: the FQS adapter is the federated
// query's workstream at every lookup the core makes: its answer is the
// facility's own (handleFederatedQueryInbound), it has no native forward and
// no originated-answer check, its request names no other patient, its
// ownership rows are the ones PA declared for it, and it declares and mounts
// the leg's Da Vinci ingress (POST /Task), which PA no longer does.
func TestFQSAdapter_AnswersTheFederatedQuery(t *testing.T) {
	const leg = "federated-query"
	if got := answerFor(leg); got == nil || fmt.Sprintf("%p", got) != fmt.Sprintf("%p", (*Gateway).handleFederatedQueryInbound) {
		t.Error("the federated query's answer is not handleFederatedQueryInbound")
	}
	if paAdapter.answer(leg) != nil {
		t.Error("the PA adapter still answers the federated query")
	}
	if forwardFor(leg) != nil || originatedAnswerCheckFor(leg) != nil {
		t.Error("the federated query has a native forward or an originated-answer check")
	}
	if request, answer := legInvolvement(leg); request || answer {
		t.Errorf("involvement (%v, %v), want neither", request, answer)
	}
	if got := namedMembers([]byte(`{"resourceType":"Task","for":{"reference":"Patient/m1"}}`), leg); got != nil {
		t.Errorf("members %v, want none", got)
	}
	k := func(role relay.Role, dir relay.Direction, outcome relay.Outcome) relay.Key {
		return relay.Key{Leg: leg, Role: role, Direction: dir, Outcome: outcome}
	}
	relayed := []relay.Ownership{relay.OwnershipRelayed}
	authored := []relay.Ownership{relay.OwnershipAuthored}
	want := map[relay.Key]relay.Rule{
		k(relay.RoleRequester, relay.DirectionRequest, relay.OutcomeCarried):    {Allowed: relayed},
		k(relay.RoleRequester, relay.DirectionRequest, relay.OutcomeOriginated): {Allowed: authored, Builders: []relay.BuilderID{relay.BuilderSDKFederatedQuery}},
		k(relay.RoleRequester, relay.DirectionResponse, relay.OutcomeAnswered):  {Allowed: relayed},
		k(relay.RoleRecipient, relay.DirectionResponse, relay.OutcomeAnswered):  {Allowed: authored, Builders: []relay.BuilderID{relay.BuilderCDexFulfillment}},
	}
	sameRule := func(a, b relay.Rule) bool {
		return slices.Equal(a.Allowed, b.Allowed) && slices.Equal(a.Edits, b.Edits) && slices.Equal(a.Builders, b.Builders)
	}
	if got := fqsAdapter.ownershipRows(); !maps.EqualFunc(got, want, sameRule) {
		t.Errorf("the FQS ownership rows = %+v, want %+v", got, want)
	}
	for key := range paLegOwnership {
		if key.Leg == leg {
			t.Errorf("PA still declares %s", key)
		}
	}
	if h, ok := workstreamIngressRoute(RouteFederatedQuery); !ok || h.tag != "federated-query-ingress" || fmt.Sprintf("%p", h.handler(&Gateway{})) != fmt.Sprintf("%p", (&Gateway{}).handleFederatedQueryIngress) {
		t.Errorf("the federated query's ingress route is %+v, %v; want the FQS adapter's handleFederatedQueryIngress, tagged federated-query-ingress", h, ok)
	}
	if _, ok := paIngressRoutes[RouteFederatedQuery]; ok {
		t.Error("the PA adapter still declares the federated query's ingress route")
	}
	if got := slices.Collect(maps.Keys(fqsAdapter.ingressRoutes())); !slices.Equal(got, []string{RouteFederatedQuery}) {
		t.Errorf("the FQS adapter's ingress routes are %v, want only %s", got, RouteFederatedQuery)
	}
	g := &Gateway{cfg: Config{IngressEnabled: true}}
	if got := routePatterns(g.fqsRoutes("provider")); !slices.Equal(got, []string{"POST /Task"}) {
		t.Errorf("the FQS adapter mounts %v for a provider with its ingress on, want POST /Task", got)
	}
	if slices.Contains(routePatterns(g.paRoutes("provider")), "POST /Task") {
		t.Error("the PA adapter still mounts POST /Task")
	}
	for _, role := range []string{"payer", "facility", "phg"} {
		if got := g.fqsRoutes(role); got != nil {
			t.Errorf("the FQS adapter mounts %v for %s", routePatterns(got), role)
		}
	}
	if got := (&Gateway{}).fqsRoutes("provider"); got != nil {
		t.Errorf("the FQS adapter mounts %v for a provider with its ingress off", routePatterns(got))
	}
}

// TestFQSLegOperations_IsTheFederatedQuery: the FQS accessor returns the
// federated query's bindings, which PA's no longer lists, in a fresh copy.
func TestFQSLegOperations_IsTheFederatedQuery(t *testing.T) {
	ops := FQSLegOperations()
	want := LegOperation{RequestFrame: "provider-tpo", RequestOp: "federated-query-submit", ResponseFrame: "facility-disclosure", ResponseOp: "federated-query-response", Scope: "named-docs-only"}
	if len(ops) != 1 || ops["federated-query"] != want {
		t.Fatalf("FQSLegOperations() = %+v, want only federated-query %+v", ops, want)
	}
	if _, ok := PALegOperations()["federated-query"]; ok {
		t.Fatal("PALegOperations still lists the federated query")
	}
	ops["federated-query"] = LegOperation{RequestOp: "tampered"}
	if FQSLegOperations()["federated-query"] != want {
		t.Fatal("mutation through a returned map reached a later call")
	}
}
