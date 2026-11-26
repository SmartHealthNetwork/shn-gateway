// Package cdextopic is the HRex Task topic as the network carries it: a payer's
// topic
// Subscription to the changes of the Tasks it created at a provider. Its
// subscribe leg is cdex-task-subscribe, and every notification rides
// cdex-task-signal: CDex subscriptions are id-only, so the topic has no
// content leg.
package cdextopic

import (
	"net/http"

	"github.com/SmartHealthNetwork/shn-gateway/pa/subscription"
	"github.com/SmartHealthNetwork/shn-gateway/recording"
)

const (
	// Canonical is the HRex Task topic's canonical.
	Canonical = "http://hl7.org/fhir/us/davinci-hrex/SubscriptionTopic/Task"
	// TokenValue is the topic's notification-address segment and
	// subscription.Binding.Topic.
	TokenValue = "cdex-task"
	// SubscribeLeg is the topic's subscribe leg.
	SubscribeLeg = "cdex-task-subscribe"
	// SignalLeg is the topic's signal leg, which every CDex notification
	// rides.
	SignalLeg = "cdex-task-signal"
)

// filters are the filter parameters the HRex Task topic defines, as a filter
// names them (subscription.FilterParam: name and modifier).
var filters = map[[2]string]bool{
	{"requester", "identifier"}: true,
	{"owner", "identifier"}:     true,
	{"code", ""}:                true,
	{"status", ""}:              true,
}

// Topic is the HRex Task topic, as subscription.Topic.
type Topic struct{}

var _ subscription.Topic = Topic{}

// Token is the topic's notification-address segment.
func (Topic) Token() string { return TokenValue }

// Canonical is the topic's criteria canonical.
func (Topic) Canonical() string { return Canonical }

// Contents is id-only alone: a CDex notification is not gated by the Task
// read's rule (a provider's gateway delivers the answer to a payer's read only
// for a Task whose payer-url is that gateway's own return path for the payer),
// so it carries no Task.
func (Topic) Contents() []string { return []string{"id-only"} }

// Extensions is none: HRex defines no Subscription profile of the Task
// topic's own, so only the Backport IG's allowlisted extensions are carried.
func (Topic) Extensions() []string { return nil }

// CheckFilter is the filter rule for the HRex Task topic: exactly one
// requester:identifier, every value of it a system|value pair the registry
// attests for the authenticated payer (subscription.MatchIdentifiers). A bare
// value is refused: it names no system, so it matches that value in every
// system and cannot be bound to the payer. And, as the subscriber
// wishes, at most one each of owner:identifier, code and status. Any other
// parameter or modifier, or one named twice, is refused 403, recorded
// authority. The requester filter keeps a payer from subscribing to every
// payer's Tasks; it is not the authority gate on what a notification
// discloses, which id-only and the read's payer-url rule are.
func (Topic) CheckFilter(subscriber subscription.Holder, params []subscription.FilterParam) *subscription.Refusal {
	seen := map[[2]string]bool{}
	var requester []subscription.FilterValue
	for _, p := range params {
		k := [2]string{p.Name, p.Modifier}
		if !filters[k] {
			return refusal("its filter names a parameter the HRex Task topic does not define: " + filterName(p))
		}
		if seen[k] {
			return refusal("its filter names " + filterName(p) + " twice")
		}
		seen[k] = true
		if p.Name == "requester" {
			requester = p.Values
		}
	}
	if !seen[[2]string{"requester", "identifier"}] {
		return refusal("its filter names no requester:identifier, which a Subscription to the HRex Task topic must")
	}
	for _, v := range requester {
		if !v.HasSystem {
			return refusal("its requester:identifier names " + v.Value + " with no system, which matches that value in every system; name it as system|value")
		}
	}
	return subscription.MatchIdentifiers(requester, subscriber.PayerIDs)
}

func filterName(p subscription.FilterParam) string {
	if p.Modifier == "" {
		return p.Name
	}
	return p.Name + ":" + p.Modifier
}

func refusal(reason string) *subscription.Refusal {
	return &subscription.Refusal{Status: http.StatusForbidden, Primitive: recording.RefusalAuthority,
		Reason: "the Subscription is outside the shape this network carries: " + reason}
}

// SubscribeLeg is the subscribe leg's flat transaction type.
func (Topic) SubscribeLeg() string { return SubscribeLeg }

// ContentLeg is none: CDex subscriptions are id-only.
func (Topic) ContentLeg() string { return "" }

// SignalLeg is the signal leg's flat transaction type.
func (Topic) SignalLeg() string { return SignalLeg }

// NotificationShape is the HRex Task topic's, as its pinned definitions
// declare it: the SubscriptionTopic (Basic-Task.json) in
// hl7.fhir.us.davinci-hrex 1.1.0 (sha256
// a122c031bc7d285a7aff18e18462728e499043e47f3675f43d4b359884287359) and
// 1.2.0 (sha256
// f28749c04bed6c791aa8aa594dbaffcdbabd4605d650860389dc2c2c61173c44), the
// packages gateway/deploy/validator pins, declares no notificationShape, and
// its one resourceTrigger is Task: a focus is a Task, and no
// additional-context is declared, so any is refused.
func (Topic) NotificationShape() subscription.NotificationShape {
	return subscription.NotificationShape{Topic: Canonical, Focus: []string{"Task"}}
}

// CheckContent is never called for this topic, which has no content leg
// (subscription.Leg answers "" for a content notification, refused there). A
// call is refused, never passed.
func (Topic) CheckContent(subscription.Holder, *subscription.Notification) *subscription.Refusal {
	return refusal("a CDex notification carries no content: the HRex Task topic is id-only")
}
