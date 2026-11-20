//go:build !shn_coreonly

package engine

import (
	"context"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/pa/subscription"
)

// A topic Subscription's notification travels two halves (the notifications
// design §6.2 step 5, §7.3, §7.4): the notifier's gateway takes it from its
// own system at a notification address it wrote (handleNotifyIngress), and the
// subscriber's gateway delivers it to the endpoint its own system wrote
// (handleSignalDelivery). Both halves hold every notification to its topic's
// rules alone; what differs between topics is named here, once per topic, so
// each half is one handler whichever topic it carries.

// notifierTopic is what the notifier's half takes from a topic: the topic,
// the notifier's subscription state on this gateway, the reasons it refuses
// with, and the check of the places the subscriber's gateway re-keys, when
// that gateway re-keys any (rekey; nil when it re-keys none).
type notifierTopic struct {
	topic   subscription.Topic
	state   func(*paGatewayState) *cdexSubscriptionState
	reasons notifierReasons
	rekey   *notifierRekey
	content *notifierContent
}

// notifierContent is what the notifier's half takes from a topic that has a
// content leg (nil when it has none, and a notification with content is
// refused): the holder the topic's content check holds a notification to
// (subscriber), the reason it is refused with (notSubscribers, the check's
// reason following), and the binding of its patient, the content leg's token
// subject (bind, given the notifier's FHIR base its gateway states), or why
// it is refused, and the check of a notification the subscriber's gateway
// edits on the content leg (readable), refused on the fidelity of SHN's own
// edit with unreadable where that gateway could not make its edit.
type notifierContent struct {
	subscriber     func(g *Gateway, subscriber string) subscription.Holder
	notSubscribers refusalReason
	bind           func(g *Gateway, ctx context.Context, n *subscription.Notification, body []byte, base string) (string, *notifierRefusal)
	readable       func(body []byte, origin relay.Origin, base string) notificationRead
	unreadable     string
}

// notifierRefusal is a content notification's refusal by the notifier's
// half: its status and text, and the rule and reason it is recorded under,
// or recorded when a check that refused has recorded it already.
type notifierRefusal struct {
	rule     string
	status   int
	msg      string
	reason   refusalReason
	recorded bool
}

// notifierReasons are the notifier's half's refusals that name its topic,
// each the refusal's text and its reason (refusalReasons). notANotification
// opens the text, and the reason it is not one follows; so does shape's.
// content refuses a notification with content, read only for a topic with no
// content leg (notifierTopic.content nil).
type notifierReasons struct {
	notANotification, content, context, shape refusalReason
}

// notifierRekey is the notifier's check of the places the subscriber's
// gateway re-keys (read), refused where that gateway could not make its edit:
// unreadable (the fidelity of SHN's own edit, the text alone), and an address
// it cannot read or re-key (routing, each the text and its reason).
type notifierRekey struct {
	read                  func(body []byte, origin relay.Origin, base string) notificationRead
	unreadable            string
	address, notRekeyable refusalReason
}

// deliveryTopic is what the subscriber's half takes from a topic: the topic,
// the leg it delivers (inbound, the signal leg, or the content leg when
// content is set) as its inbound dispatcher answers it, the refusals that name
// the topic, the check of the places this gateway re-keys, when it re-keys
// any (rekey; nil when it re-keys none), and whether this gateway takes the
// leg, when its participant decides that (taken; nil when every gateway of
// the role takes it), with the refusal of a leg it does not take
// (refusals.notTaken).
type deliveryTopic struct {
	topic    subscription.Topic
	inbound  inboundLeg
	content  bool
	refusals deliveryRefusals
	rekey    *deliveryRekey
	taken    func(g *Gateway) bool
}

// deliveryRekey is the subscriber's check of the places this gateway re-keys
// (read), with its refusals: unreadable (the fidelity of SHN's own edit),
// and an address it cannot read or re-key (routing).
type deliveryRekey struct {
	read                              func(body []byte, origin relay.Origin, base string) notificationRead
	unreadable, address, notRekeyable string
}

// deliveryRefusals are the subscriber's half's refusals that name its topic.
// shape opens its text, and the reason the notification is outside the shape
// follows; notTaken refuses the leg on a gateway that does not take it.
type deliveryRefusals struct {
	content, context, shape, notTaken string
}

// cdexNotifier is the HRex Task topic's notifier half, on a provider's
// gateway: its state is the provider's subscription state, and the payer's
// gateway re-keys addresses into the provider's server (E-17).
var cdexNotifier = notifierTopic{
	topic: cdexTopic,
	state: func(st *paGatewayState) *cdexSubscriptionState { return st.cdexSubscription },
	reasons: notifierReasons{
		notANotification: reasonNotifyNotANotification,
		content:          reasonNotifyContent,
		context:          reasonNotifyContext,
		shape:            reasonNotifyShape,
	},
	rekey: &notifierRekey{
		read:         readNotification,
		unreadable:   errNotifyUnreadable,
		address:      reasonNotifyAddress,
		notRekeyable: reasonNotifyNotRekeyable,
	},
}

// cdexDelivery is the HRex Task topic's subscriber half, on a payer's
// gateway, which re-keys the notification's addresses into the provider's
// server to its own base (E-17).
var cdexDelivery = deliveryTopic{
	topic:   cdexTopic,
	inbound: legCDexTaskSignalInbound,
	refusals: deliveryRefusals{
		content: refusalSignalContent,
		context: refusalSignalContext,
		shape:   refusalSignalShape,
	},
	rekey: &deliveryRekey{
		read:         readNotification,
		unreadable:   refusalSignalUnreadable,
		address:      refusalSignalAddress,
		notRekeyable: refusalSignalNotRekeyable,
	},
}
