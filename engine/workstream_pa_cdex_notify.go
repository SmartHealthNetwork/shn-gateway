//go:build !shn_coreonly

package engine

import "net/http"

// The notifier's half of a notification: the provider's own system posts each notification
// of the HRex Task topic (a handshake, a heartbeat or an id-only event) to the
// notification address its gateway wrote into the payer's Subscription (E-19),
// POST [ingress]/notify/cdex-task/{subscriber}/{kid}/{sealed}, and the gateway
// sends it on the topic's signal leg (cdex-task-signal) to the subscriber the
// address names. Every rule is a network rule, the same at every conformance
// level:
//   - the Authorization value must be the gateway's own credential for the
//     address and the X-SHN-Channel value beside it (one of each): a
//     notification without either, or whose credential does not verify
//     under the current key or the predecessor kept for a rotation's overlap,
//     is refused 401 on authentication, nothing sent;
//   - the body must be a notification of the HRex Task topic by structure
//     (subscription.ClassifyBody, then subscription.ParseNotification: a FHIR
//     JSON Bundle whose first entry is a SubscriptionStatus Parameters naming
//     the topic's canonical, with or without a |version): anything else is
//     refused 400 on routing;
//   - a notification that is not no-content by structure (the no-content test,
//     subscription.Notification.NoContent) is refused 403 on authority: the
//     topic has no content leg (N11), and content never rides a signal row;
//   - a notification outside the HRex Task topic's closed notification
//     shape (subscription.Notification.CheckShape: the closed
//     SubscriptionStatus Bundle an instance $status answer is held to, as a
//     history notification; no extension, no signature, no reference but
//     its subscription's and each focus's) is refused 403 on authority;
//   - an address into this provider's own server, at a place the payer's
//     gateway re-keys (E-17), that is not under the provider's FHIR base
//     (readNotification) is refused 422 on routing: the payer's gateway
//     could not write it at its own base;
//   - a notification-event part named additional-context is refused 403 on
//     authority (subscription.Notification.ContextDeclared): the topic's
//     notification shape declares none;
//   - a notification whose places the payer's gateway re-keys (E-17) cannot
//     be read one way only (notificationReadable: a member name repeated
//     under folding, or JSON relay cannot read) is refused 400 on the
//     fidelity of SHN's own edit, since that gateway could not make it;
//   - an http or https address at a place the payer's gateway re-keys (E-17)
//     that is not an absolute URL with a host written plainly
//     (unreadableAddress) is refused 422 on routing: the payer's gateway
//     could not re-key it, and the payer's system could follow it into the
//     provider's server;
//   - it is then sent to the subscriber as the provider's system posted it,
//     its request frame carrying the sealed endpoint the address names
//     (subscriptionEndpoint), the sealed headers (subscriptionChannel) and the
//     provider's FHIR base its Task and Subscription addresses are under
//     (statedBase), which the payer's gateway re-keys them from (E-17). The
//     leg names no patient on either side.
//
// The answer is carried back to the provider's system as the payer's gateway
// relays it: the subscriber endpoint's own answer, or a refusal. A retry is a
// new POST, sent in a new frame, so it is never refused as a replay.

// RouteCDexNotify is the notification route on a provider's gateway, as
// exchange records name it.
const RouteCDexNotify = "cdex-notify"

// The notification route's refusals (refusalReasons).
const (
	// errNotifyCredential: the gateway's credential for the address is
	// missing, repeated or does not verify, or X-SHN-Channel is.
	errNotifyCredential = msgIngressAuthentication + ": the notification does not carry this gateway's credential for its address and the X-SHN-Channel value beside it, as the Subscription's channel.header gave them (authentication); nothing was sent"
	// errNotifyNotANotification opens the refusal of a body that is not a
	// notification of the topic.
	errNotifyNotANotification = "the body is not a notification of the HRex Task topic this address is for (routing); nothing was sent: "
	// errNotifyContent: a notification carrying content on the topic's
	// signal leg.
	errNotifyContent = "the notification carries a resource, and a CDex Task notification is id-only: it names the Task and carries none (per-leg authority); nothing was sent"
	// errNotifyContext: a notification-event's additional-context, which
	// the topic's notification shape does not declare.
	errNotifyContext = "a notification-event carries an additional-context, which the HRex Task topic does not declare: a CDex Task notification names the Task alone (per-leg authority); nothing was sent"
	// errNotifyUnreadable: a notification whose places the payer's gateway
	// re-keys cannot be read one way only.
	errNotifyUnreadable = "the notification cannot be read one way only at the places the payer's gateway re-keys to its own base (JSON the network cannot read unambiguously, such as a lone surrogate in a string), so the payer's gateway could not make its edit (fidelity of SHN's own edit); nothing was sent"
	// errNotifyAddress: an address in a place the payer's gateway re-keys
	// that no URL reader reads one way only.
	errNotifyAddress = "the notification holds an http or https address, at a place the payer's gateway re-keys to its own base, that no URL reader reads one way only (it is not an absolute URL naming its server plainly), so the payer's gateway cannot re-key it (routing); nothing was sent"
	// errNotifyNotRekeyable: an address into this provider's own server, at
	// a place the payer's gateway re-keys, that is not under its FHIR base.
	errNotifyNotRekeyable = "the notification holds an address into this provider's own server, at a place the payer's gateway re-keys to its own base, that is not under the provider's FHIR base (a fragment, a dot segment, or a path outside the base, escaped or not), so the payer's gateway cannot re-key it (routing); nothing was sent"
	// errNotifyShape opens the refusal of a notification outside the topic's
	// closed notification shape.
	errNotifyShape = "the notification is outside the HRex Task topic's closed notification shape, so it cannot be shown to name no patient (per-leg authority); nothing was sent: "
)

// handleCDexNotifyIngress is the provider system's notification at a
// notification address its gateway wrote (the rules above). It is mounted
// only on a gateway that takes the payer's Subscription (cdex-task-subscribe),
// since only that gateway writes the addresses.
func (g *Gateway) handleCDexNotifyIngress(w http.ResponseWriter, r *http.Request) {
	exchangeOf(r.Context()).leg(legCDexTaskSignal)
	g.handleNotifyIngress(w, r, cdexNotifier)
}
