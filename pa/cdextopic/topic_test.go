package cdextopic

import (
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/pa/subscription"
	"github.com/SmartHealthNetwork/shn-gateway/recording"
)

// The HRex Task topic's filter rule: exactly one
// requester:identifier naming the payer's attested identifiers, each as
// system|value, and at most one each of owner:identifier, code and status. A
// payer holding identifiers in two systems names each with its own system,
// and is refused naming one with the other's; a bare value, which names no
// system and so matches that value in every system, is refused even when it
// is the payer's own.
func TestTopic_CheckFilter(t *testing.T) {
	const sysA, sysB = "http://example.org/payer-ids/a", "http://example.org/payer-ids/b"
	holder := subscription.Holder{ID: "payer-x", PayerIDs: []subscription.Identifier{{System: sysA, Value: "A1"}, {System: sysB, Value: "B1"}}}
	v := func(sys, val string) subscription.FilterValue {
		return subscription.FilterValue{System: sys, HasSystem: sys != "-", Value: val}
	}
	req := func(vals ...subscription.FilterValue) subscription.FilterParam {
		return subscription.FilterParam{Name: "requester", Modifier: "identifier", Values: vals}
	}
	top := Topic{}
	for name, params := range map[string][]subscription.FilterParam{
		"system A's value under A": {req(v(sysA, "A1"))},
		"system B's value under B": {req(v(sysB, "B1"))},
		"both, each its own":       {req(v(sysA, "A1"), v(sysB, "B1"))},
		"with owner, code, status": {req(v(sysA, "A1")), {Name: "owner", Modifier: "identifier", Values: []subscription.FilterValue{{Value: "NPI1"}}}, {Name: "code", Values: []subscription.FilterValue{{Value: "attachment-request-code"}}}, {Name: "status", Values: []subscription.FilterValue{{Value: "completed"}}}},
	} {
		if rf := top.CheckFilter(holder, params); rf != nil {
			t.Errorf("%s: refused %+v", name, rf)
		}
	}
	for name, params := range map[string][]subscription.FilterParam{
		"system A's value under B":    {req(v(sysB, "A1"))},
		"system B's value under A":    {req(v(sysA, "B1"))},
		"one good, one crossed":       {req(v(sysA, "A1"), v(sysA, "B1"))},
		"the empty system":            {req(v("", "A1"))},
		"an unattested value":         {req(subscription.FilterValue{Value: "Z9"})},
		"a bare value of the payer's": {req(subscription.FilterValue{Value: "B1"})},
		"a bare value beside a pair":  {req(v(sysA, "A1"), subscription.FilterValue{Value: "B1"})},
		"no requester":                {{Name: "status", Values: []subscription.FilterValue{{Value: "completed"}}}},
		"requester twice":             {req(v(sysA, "A1")), req(v(sysB, "B1"))},
		"requester with no modifier":  {{Name: "requester", Values: []subscription.FilterValue{v(sysA, "A1")}}},
		"status twice":                {req(v(sysA, "A1")), {Name: "status", Values: []subscription.FilterValue{{Value: "a"}}}, {Name: "status", Values: []subscription.FilterValue{{Value: "b"}}}},
		"a modifier on status":        {req(v(sysA, "A1")), {Name: "status", Modifier: "not", Values: []subscription.FilterValue{{Value: "a"}}}},
		"another parameter":           {req(v(sysA, "A1")), {Name: "patient", Values: []subscription.FilterValue{{Value: "Patient/1"}}}},
		"owner by reference":          {req(v(sysA, "A1")), {Name: "owner", Values: []subscription.FilterValue{{Value: "Organization/1"}}}},
	} {
		rf := top.CheckFilter(holder, params)
		if rf == nil || rf.Status != http.StatusForbidden || rf.Primitive != recording.RefusalAuthority {
			t.Errorf("%s: %+v, want 403 authority", name, rf)
		}
	}
	// The missing requester filter is refused for what it is, not as a
	// filter naming no identifier.
	if rf := top.CheckFilter(holder, []subscription.FilterParam{{Name: "status", Values: []subscription.FilterValue{{Value: "completed"}}}}); rf == nil || !strings.Contains(rf.Reason, "names no requester:identifier") {
		t.Errorf("no requester: %+v, want the refusal naming the missing requester:identifier", rf)
	}
	if rf := top.CheckFilter(holder, []subscription.FilterParam{req(subscription.FilterValue{Value: "A1"})}); rf == nil || !strings.Contains(rf.Reason, "no system") {
		t.Errorf("a bare value: %+v, want the refusal naming the missing system", rf)
	}
	if rf := top.CheckContent(holder, nil); rf == nil || rf.Status != http.StatusForbidden {
		t.Errorf("CheckContent: %+v, want a refusal: the topic has no content leg", rf)
	}
	if top.Token() != "cdex-task" || top.Canonical() != Canonical || !slices.Equal(top.Contents(), []string{"id-only"}) || top.Extensions() != nil ||
		top.SubscribeLeg() != "cdex-task-subscribe" || top.ContentLeg() != "" || top.SignalLeg() != "cdex-task-signal" {
		t.Fatal("the topic's constants")
	}
}

// The notification shape is the pinned HRex Task topic's (hl7.fhir.us.davinci-
// hrex 1.1.0, sha256 a122c031bc7d285a7aff18e18462728e499043e47f3675f43d4b359884287359,
// and 1.2.0, sha256 f28749c04bed6c791aa8aa594dbaffcdbabd4605d650860389dc2c2c61173c44,
// as gateway/deploy/validator pins them; Basic-Task.json): no notificationShape,
// and one resourceTrigger, on Task. So a focus is a Task, and no
// additional-context is declared. A tripwire: a change to either list, or to
// the pins, is a change to what an instance $status answer may carry.
func TestTopic_NotificationShapeIsThePinnedTopics(t *testing.T) {
	got := Topic{}.NotificationShape()
	if got.Topic != Canonical || !slices.Equal(got.Focus, []string{"Task"}) || len(got.AdditionalContext) != 0 {
		t.Fatalf("notification shape %+v, want the topic's canonical, focus [Task] and no additional-context", got)
	}
	pins, err := os.ReadFile("../../deploy/validator/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, pin := range []string{
		"a122c031bc7d285a7aff18e18462728e499043e47f3675f43d4b359884287359  hl7.fhir.us.davinci-hrex-1.1.0.tgz",
		"f28749c04bed6c791aa8aa594dbaffcdbabd4605d650860389dc2c2c61173c44  hl7.fhir.us.davinci-hrex-1.2.0.tgz",
	} {
		if !strings.Contains(string(pins), pin) {
			t.Errorf("the validator no longer pins %q: re-read the topic's notification shape from the package it pins now", pin)
		}
	}
}
