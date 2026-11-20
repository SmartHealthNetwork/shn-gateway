// Package pastopic is the PAS topic as the network carries it (the
// notifications design §5, §7.1): a provider's topic Subscription, at a
// payer's base, to the results of the prior authorizations its system sent.
// Its subscribe leg is pas-subscription-subscribe; a full-resource
// notification rides pas-subscription-notify, and every other notification
// (a handshake, a heartbeat, an id-only event) rides pas-subscription-signal.
package pastopic

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/internal/jsonmember"
	"github.com/SmartHealthNetwork/shn-gateway/pa/subscription"
	"github.com/SmartHealthNetwork/shn-gateway/recording"
)

const (
	// Canonical is the PAS topic's canonical, the same at every PAS line the
	// network carries (2.0.1, 2.1.0 and 2.2.1).
	Canonical = "http://hl7.org/fhir/us/davinci-pas/SubscriptionTopic/PASSubscriptionTopic"
	// TokenValue is the topic's notification-address segment and
	// subscription.Binding.Topic.
	TokenValue = "pas"
	// SubscribeLeg is the topic's subscribe leg.
	SubscribeLeg = "pas-subscription-subscribe"
	// ContentLeg is the topic's notify leg, which a full-resource
	// notification rides.
	ContentLeg = "pas-subscription-notify"
	// SignalLeg is the topic's signal leg, which every no-content
	// notification rides.
	SignalLeg = "pas-subscription-signal"
	// FilterParameter is the one filter parameter a PAS Subscription names:
	// the PAS specification's org-identifier (spec-58), as the reference
	// payer reads it.
	FilterParameter = "org-identifier"
)

// Topic is the PAS topic, as subscription.Topic.
type Topic struct{}

var _ subscription.Topic = Topic{}

// Token is the topic's notification-address segment.
func (Topic) Token() string { return TokenValue }

// Canonical is the topic's criteria canonical.
func (Topic) Canonical() string { return Canonical }

// Contents are full-resource and id-only: the network carries every line, and
// refusing a 2.0.1 pair's id-only for 2.2.1's full-resource rule would refuse
// a pair for its line (§7.1 rule 2). Which content a pair uses is the
// notifier's to accept or refuse by its line.
func (Topic) Contents() []string { return []string{"full-resource", "id-only"} }

// Extensions is none: the PAS Subscription profile (2.1.0 and 2.2.1; 2.0.1
// has none) constrains only the Backport IG's filter-criteria and
// payload-content extensions, so only the Backport IG's allowlisted
// extensions are carried.
func (Topic) Extensions() []string { return nil }

// CheckFilter is §7.1 rule 3 for the PAS topic: exactly one org-identifier
// parameter, with no modifier, and no other parameter, each of its values an
// identifier the registry attests for the authenticated provider
// (subscription.Holder.ProviderIDs). A system|value pair must equal an
// attested pair (subscription.MatchIdentifiers). A value with no system, as
// the PAS specification names the filter's values (spec-58: the sending
// system's identifier), must equal the value of an identifier attested for
// the provider, in whatever system it is attested. In a direct connection the
// payer enforces spec-59 because it knows its caller; through the network
// only the network knows who the caller is, so it holds the value to the
// caller's attested identifiers. Today the registry attests NPIs only, so a
// value with no system is carried only when it is one of the provider's
// attested NPIs. Anything else is refused 403, recorded authority. A value is
// never rewritten: a request that passes is carried as sent.
func (Topic) CheckFilter(subscriber subscription.Holder, params []subscription.FilterParam) *subscription.Refusal {
	for _, p := range params {
		if p.Name != FilterParameter || p.Modifier != "" {
			return refusal("its filter names a parameter the PAS topic does not define: " + filterName(p))
		}
	}
	switch len(params) {
	case 0:
		return refusal("its filter names no " + FilterParameter + ", which a Subscription to the PAS topic must")
	case 1:
	default:
		return refusal("its filter names " + FilterParameter + " more than once")
	}
	var pairs []subscription.FilterValue
	for _, v := range params[0].Values {
		if v.HasSystem {
			pairs = append(pairs, v)
			continue
		}
		if r := checkBare(subscriber, v); r != nil {
			return r
		}
	}
	if len(pairs) == 0 && len(params[0].Values) > 0 {
		return nil
	}
	return subscription.MatchIdentifiers(pairs, subscriber.ProviderIDs)
}

// checkBare is CheckFilter's rule on one value with no system: it is carried
// when it equals the value of an identifier attested for the subscriber, and
// refused 403, recorded authority, otherwise.
func checkBare(subscriber subscription.Holder, v subscription.FilterValue) *subscription.Refusal {
	if slices.ContainsFunc(subscriber.ProviderIDs, func(a subscription.Identifier) bool {
		return a.Value == v.Value
	}) {
		return nil
	}
	return &subscription.Refusal{Status: http.StatusForbidden, Primitive: recording.RefusalAuthority,
		Reason: "the Subscription's " + FilterParameter + " names " + v.Value + " with no system, which is not an identifier attested for the subscriber; name one of its attested identifiers, with or without its system"}
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

// ContentLeg is the notify leg's flat transaction type, which a full-resource
// notification rides.
func (Topic) ContentLeg() string { return ContentLeg }

// SignalLeg is the signal leg's flat transaction type.
func (Topic) SignalLeg() string { return SignalLeg }

// NotificationShape is the PAS topic's, as its pinned definitions declare
// it (Basic-PASSubscriptionTopic.json in hl7.fhir.us.davinci-pas 2.0.1, 2.1.0
// and 2.2.1, the packages gateway/deploy/validator pins): 2.1.0 and 2.2.1
// declare a notificationShape on Bundle (the PAS Response Bundle), and 2.0.1
// declares none and no resourceTrigger, its one eventTrigger naming
// ClaimResponse. A focus is either; no additional-context is declared, so any
// is refused.
func (Topic) NotificationShape() subscription.NotificationShape {
	return subscription.NotificationShape{Topic: Canonical, Focus: []string{"Bundle", "ClaimResponse"}}
}

// CheckContent is §7.3's N6 check on a content notification, a network rule
// at every conformance level: every ClaimResponse the notification carries,
// wherever it sits (subscription.Notification.Resources: an entry's resource,
// an entry of a PAS Response Bundle at any depth, a parameter's resource, an
// entry's response.outcome, a contained resource, any other member; its
// resourceType and every member name compared under case folding), names
// its receiver in its TransmissionIdentifiers extension's
// applicationReceiverCode, and each code names an identifier the registry
// attests for the subscriber, by CheckFilter's rule: a value with no system
// equals the value of an attested identifier, and a system|value pair equals
// an attested pair. The code is the provider's own sending-system code
// turned round, the value its Subscription's org-identifier named, so a
// notification it does not name is not the subscriber's: a code that names
// another identifier, a receiver code with no string value or with a value[x]
// beside its valueString, a ClaimResponse with no code, a notification that
// carries no ClaimResponse, and one whose resources cannot be read are each
// refused 403, recorded authority, and
// nothing reaches the subscriber. It reads that one field to decide
// authority; it judges nothing else about the payload.
func (Topic) CheckContent(subscriber subscription.Holder, n *subscription.Notification) *subscription.Refusal {
	resources, rf := n.Resources()
	if rf != nil {
		return contentRefusal(rf.Reason)
	}
	responses := 0
	for _, res := range resources {
		if !res.TypeIs("ClaimResponse") {
			continue
		}
		responses++
		var r map[string]any
		if json.Unmarshal(res.Raw, &r) != nil {
			return contentRefusal("a ClaimResponse it carries cannot be read, so it cannot be shown to be the subscriber's")
		}
		codes, ok := receiverCodes(r)
		if !ok {
			return contentRefusal("a ClaimResponse names its " + ReceiverCode + " by no string value alone (valueString, with no other value[x]), so it cannot be shown to be the subscriber's")
		}
		if len(codes) == 0 {
			return contentRefusal("a ClaimResponse names no " + ReceiverCode + " in its " + TransmissionIdentifiersURL + " extension, so it cannot be shown to be the subscriber's")
		}
		for _, c := range codes {
			if !attested(subscriber, c) {
				return contentRefusal("a ClaimResponse's " + ReceiverCode + " " + c + " is not an identifier attested for the subscriber")
			}
		}
	}
	if responses == 0 {
		return contentRefusal("it carries no ClaimResponse, so it names no receiver the network can hold to the subscriber")
	}
	return nil
}

// TransmissionIdentifiersURL is the PAS extension a ClaimResponse names its
// sender and receiver codes in.
const TransmissionIdentifiersURL = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-TransmissionIdentifiers"

// ReceiverCode is the TransmissionIdentifiers sub-extension that names the
// response's receiver.
const ReceiverCode = "applicationReceiverCode"

// receiverCodes are the applicationReceiverCode values r names, each the
// valueString of the sub-extension of its TransmissionIdentifiers extension,
// every member name compared under case folding, as a lenient reader takes
// them. ok is false when a receiver sub-extension names its value by no
// non-empty string, or carries a value[x] beside valueString (otherValue): a
// receiver the check cannot read, which fails closed.
func receiverCodes(r map[string]any) (codes []string, ok bool) {
	exts, _ := folded(r, "extension").([]any)
	for _, e := range exts {
		em, _ := e.(map[string]any)
		if folded(em, "url") != TransmissionIdentifiersURL {
			continue
		}
		subs, _ := folded(em, "extension").([]any)
		for _, s := range subs {
			sm, _ := s.(map[string]any)
			if folded(sm, "url") != ReceiverCode {
				continue
			}
			v, isString := folded(sm, "valueString").(string)
			if !isString || v == "" || otherValue(sm) {
				return nil, false
			}
			codes = append(codes, v)
		}
	}
	return codes, true
}

// otherValue reports whether sub-extension m carries a value[x] other than
// valueString (its member names folded): a receiver a reader could take
// from that value instead, which the check cannot read.
func otherValue(m map[string]any) bool {
	value, valueString := jsonmember.FoldedName("value"), jsonmember.FoldedName("valueString")
	for k := range m {
		if f := jsonmember.FoldedName(k); strings.HasPrefix(f, value) && f != valueString {
			return true
		}
	}
	return false
}

// folded is m's member whose name equals name under case folding
// (jsonmember.FoldedName), nil when it has none. The notification names no
// member twice under folding (subscription.ParseNotification refuses one
// that does), so there is at most one.
func folded(m map[string]any, name string) any {
	want := jsonmember.FoldedName(name)
	for k, v := range m {
		if jsonmember.FoldedName(k) == want {
			return v
		}
	}
	return nil
}

// attested reports whether code names an identifier attested for the
// subscriber, by CheckFilter's rule: system|value equals an attested pair,
// a value with no system the value of one.
func attested(subscriber subscription.Holder, code string) bool {
	system, value, pair := strings.Cut(code, "|")
	return slices.ContainsFunc(subscriber.ProviderIDs, func(a subscription.Identifier) bool {
		if pair {
			return a.System == system && a.Value == value
		}
		return a.Value == code
	})
}

func contentRefusal(reason string) *subscription.Refusal {
	return &subscription.Refusal{Status: http.StatusForbidden, Primitive: recording.RefusalAuthority,
		Reason: "the notification is not shown to be the subscriber's: " + reason}
}
