//go:build !shn_coreonly

package engine

import (
	"net/http"
	"slices"

	"github.com/SmartHealthNetwork/shn-gateway/pa/pastopic"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The PAS topic's no-content notifications (pas-subscription-signal; the
// notifications design §5, §6.2 step 5, §6.3, §6.5, §7.3, §7.4, §9): a payer's
// own system posts each notification of a provider's Subscription to the PAS
// topic (a handshake, a heartbeat or an id-only event) to the notification
// address its gateway wrote into that Subscription (E-19),
// POST [ingress]/notify/pas/{subscriber}/{kid}/{sealed}; the payer's gateway
// sends it on the topic's signal leg to the provider the address names, and
// the provider's gateway delivers it to the endpoint the provider's own system
// wrote, with the header values it wrote there. The leg names no patient on
// either side (subject: none).
//
// The notifier's half, on the payer's gateway, is the HRex Task topic's
// (handleNotifyIngress, its rules written out at
// workstream_pa_cdex_notify.go), held to the PAS topic, with one difference:
// neither gateway re-keys an address in a PAS notification, so no address in
// one is refused for where it points. Every rule is a network rule, the same
// at every conformance level:
//   - the gateway's own credential for the address and the X-SHN-Channel
//     value beside it, or the notification is refused 401 on authentication;
//   - a notification of the PAS topic by structure, or it is refused 400 on
//     routing;
//   - no content by structure (§5's test): a full-resource notification
//     rides the topic's content leg instead (pas-subscription-notify, held to
//     that leg's rules, workstream_pa_pas_notify.go), never this one;
//   - no notification-event part named additional-context (the PAS topic's
//     notification shape declares none), and inside the topic's closed
//     notification shape, or it is refused 403 on authority;
//   - it is then sent to the subscriber as the payer's system posted it, its
//     request frame carrying the sealed endpoint and headers and the payer's
//     PAS base (statedBase).
//
// The subscriber's half, on the provider's gateway, is the HRex Task topic's
// (handleSignalDelivery, its rules written out at
// workstream_pa_cdex_signal.go), held to the PAS topic, with no edit: the
// notification reaches the provider's endpoint byte for byte as the payer's
// system sent it. A provider takes the leg only when its participant sets
// PROVIDER_PAS_SUBSCRIPTION; one that does not refuses a notification that
// reaches it anyway 422 on routing, as every leg it does not take.

// legPASSubscriptionSignal is the PAS topic's signal leg.
const legPASSubscriptionSignal = pastopic.SignalLeg

// RoutePASNotify is the PAS topic's notification route on a payer's gateway,
// as exchange records name it.
const RoutePASNotify = "pas-notify"

// The PAS notification route's refusals that name the topic
// (refusalReasons); the credential's is the HRex Task topic's route's
// (errNotifyCredential), which names none.
const (
	// errPASNotifyNotANotification opens the refusal of a body that is not a
	// notification of the PAS topic.
	errPASNotifyNotANotification = "the body is not a notification of the PAS topic this address is for (routing); nothing was sent: "
	// errPASNotifyContext: a notification-event's additional-context, which
	// the PAS topic's notification shape does not declare.
	errPASNotifyContext = "a notification-event carries an additional-context, which the PAS topic does not declare (per-leg authority); nothing was sent"
	// errPASNotifyShape opens the refusal of a notification outside the PAS
	// topic's closed notification shape.
	errPASNotifyShape = "the notification is outside the PAS topic's closed notification shape, so it cannot be shown to name no patient (per-leg authority); nothing was sent: "
)

// pasNotifier is the PAS topic's notifier half, on a payer's gateway: its
// state is the payer's subscription state, and the provider's gateway
// re-keys no address. It names no refusal of a notification with content:
// the topic has a content leg (content), so one is never refused for it.
var pasNotifier = notifierTopic{
	topic: pasTopic,
	state: func(st *paGatewayState) *cdexSubscriptionState { return st.pasSubscription },
	reasons: notifierReasons{
		notANotification: reasonPASNotifyNotANotification,
		context:          reasonPASNotifyContext,
		shape:            reasonPASNotifyShape,
	},
	content: pasNotifierContent,
}

// handlePASNotifyIngress is the payer system's notification at a PAS
// notification address its gateway wrote (the rules above). It is mounted
// only on a gateway that takes the provider's Subscription
// (pas-subscription-subscribe), since only that gateway writes the
// addresses.
func (g *Gateway) handlePASNotifyIngress(w http.ResponseWriter, r *http.Request) {
	exchangeOf(r.Context()).leg(legPASSubscriptionSignal)
	g.handleNotifyIngress(w, r, pasNotifier)
}

var legPASSubscriptionSignalInbound = inboundLeg{"provider-tpo", "pas-signal-response", legPASSubscriptionSignal}

// The provider gateway's refusals of a PAS notification it is delivered that
// name the topic.
const (
	// refusalPASSignalContent: a signal-leg message that carries content, or
	// is not a notification of the PAS topic.
	refusalPASSignalContent = "the message on this notification leg is not a notification of the PAS topic that discloses no patient's data (per-leg authority): this leg carries a handshake, a heartbeat or an id-only event"
	// refusalPASSignalContext: a notification-event's additional-context,
	// which the PAS topic's notification shape does not declare.
	refusalPASSignalContext = "a notification-event carries an additional-context, which the PAS topic does not declare (per-leg authority)"
	// refusalPASSignalNotTaken: a provider whose participant has not asked
	// its gateway to deliver the PAS topic's notifications.
	refusalPASSignalNotTaken = "this provider takes no payer's PAS notification: its participant has not asked its gateway to deliver them"
	// refusalPASSignalShape: a notification outside the PAS topic's closed
	// notification shape. The reason it is outside follows.
	refusalPASSignalShape = "the notification is outside the PAS topic's closed notification shape, so it cannot be shown to name no patient (per-leg authority); nothing was delivered: "
)

// pasDelivery is the PAS topic's subscriber half, on a provider's gateway
// whose participant asks for the topic's notifications, which re-keys no
// address.
var pasDelivery = deliveryTopic{
	topic:   pasTopic,
	inbound: legPASSubscriptionSignalInbound,
	refusals: deliveryRefusals{
		content:  refusalPASSignalContent,
		context:  refusalPASSignalContext,
		shape:    refusalPASSignalShape,
		notTaken: refusalPASSignalNotTaken,
	},
	// Taken as the leg's row says (TakenLegs: the role and the setting it
	// is taken on), so the check follows the row.
	taken: func(g *Gateway) bool { return slices.Contains(g.cfg.TakenLegs(), legPASSubscriptionSignal) },
}

// handlePASSignalInbound delivers a payer's PAS notification to the
// provider's own endpoint (the rules above).
func (g *Gateway) handlePASSignalInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, body []byte, answerTok string) {
	g.handleSignalDelivery(w, r, env, tok, body, answerTok, pasDelivery)
}
