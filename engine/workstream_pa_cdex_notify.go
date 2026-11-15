//go:build !shn_coreonly

package engine

import (
	"net/http"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/pa/cdextopic"
	"github.com/SmartHealthNetwork/shn-gateway/pa/subscription"
	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The notifier's half of a notification (the notifications design §6.2 step
// 5, §6.3, §7.3, §7.4, §9): the provider's own system posts each notification
// of the HRex Task topic (a handshake, a heartbeat or an id-only event) to the
// notification address its gateway wrote into the payer's Subscription (E-19),
// POST [ingress]/notify/cdex-task/{subscriber}/{kid}/{sealed}, and the gateway
// sends it on the topic's signal leg (cdex-task-signal) to the subscriber the
// address names. Every rule is a network rule, the same at every conformance
// level:
//   - the Authorization value must be the gateway's own credential for the
//     address and the X-SHN-Channel value beside it (§6.3; one of each): a
//     notification without either, or whose credential does not verify
//     under the current key or the predecessor kept for a rotation's overlap,
//     is refused 401 on authentication, nothing sent;
//   - the body must be a notification of the HRex Task topic by structure
//     (subscription.ClassifyBody, then subscription.ParseNotification: a FHIR
//     JSON Bundle whose first entry is a SubscriptionStatus Parameters naming
//     the topic's canonical, with or without a |version): anything else is
//     refused 400 on routing (§7.3);
//   - a notification that is not no-content by structure (§5's test,
//     subscription.Notification.NoContent) is refused 403 on authority: the
//     topic has no content leg (N11), and content never rides a signal row
//     (§7.4);
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
// new POST, sent in a new frame, so it is never refused as a replay (§9).

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
	leg := legCDexTaskSignal
	exchangeOf(r.Context()).leg(leg)
	w, scope := g.withScope(w, relay.RoleRequester)
	w = &fhirOperationWriter{w}
	refuse := func(rule string, status int, msg string, reason refusalReason) {
		exchangeOf(r.Context()).refusingFor(rule, reason)
		writeJSON(w, status, map[string]string{"error": msg})
	}
	topic := cdextopic.Topic{}
	// The address the provider's system posted to, as this gateway wrote
	// it: its ingress, then the path as sent, escaped.
	raw := strings.TrimRight(g.cfg.IngressBaseURL, "/") + r.URL.EscapedPath()
	addr, isAddress := subscription.ParseAddress(raw, g.cfg.IngressBaseURL)
	// The credential first, before the body is read: it is carried in the
	// address and the headers alone (one Authorization and one
	// X-SHN-Channel, verified together over the address), so a sender that
	// holds none cannot make this gateway read a body up to its limit. An
	// address this gateway never writes (another topic, a query, a value
	// that is no sealed endpoint's form), or a gateway that writes none, has
	// no credential that verifies.
	isAddress = isAddress && addr.Topic == topic.Token() && r.URL.RawQuery == "" && !r.URL.ForceQuery
	st := g.paState()
	auths, channels := r.Header.Values(subscription.AuthorizationHeader), r.Header.Values(subscription.ChannelHeader)
	if st == nil || st.cdexSubscription == nil || !isAddress || len(auths) != 1 || len(channels) != 1 ||
		st.cdexSubscription.keys.VerifyCredential(addr, channels[0], auths[0]) != nil {
		refuse(RefusalAuthentication, http.StatusUnauthorized, errNotifyCredential, reasonIngressAuthentication)
		return
	}
	sub := st.cdexSubscription
	// Then the body, read as every ingress route reads one, so a coding or a
	// size it cannot take is refused as there.
	body, ok := readIngressBody(w, r, shnsdk.MaxRequestBytes)
	if !ok {
		return
	}
	// The body by its structure alone, never its label (the one shared
	// classifier), then the route's structural minimum. The classifier's
	// refusal is equivalent to ParseNotification's for every body that
	// reaches it (each refuses what is not one FHIR JSON object it can read),
	// so no body tells the two apart; it is kept as the shared reading every
	// check on a body starts from.
	if kind, _, err := subscription.ClassifyBody(body); err != nil || kind != subscription.BodyFHIRJSON {
		why := "it is not one FHIR JSON resource this network can read"
		refuse(RefusalRouting, http.StatusBadRequest, errNotifyNotANotification+why, reasonNotifyNotANotification)
		return
	}
	n, rf := subscription.ParseNotification(body, topic)
	if rf != nil {
		refuse(RefusalRouting, http.StatusBadRequest, errNotifyNotANotification+rf.Reason, reasonNotifyNotANotification)
		return
	}
	if subscription.Leg(topic, n) != leg {
		refuse(RefusalAuthority, http.StatusForbidden, errNotifyContent, reasonNotifyContent)
		return
	}
	if !n.ContextDeclared(topic.NotificationShape()) {
		refuse(RefusalAuthority, http.StatusForbidden, errNotifyContext, reasonNotifyContext)
		return
	}
	if rf := n.CheckShape(topic.NotificationShape(), sub.base.String()); rf != nil {
		refuse(RefusalAuthority, http.StatusForbidden, errNotifyShape+rf.Reason, reasonNotifyShape)
		return
	}
	switch readNotification(n.Body(), relay.OriginIngressRequest, sub.base.String()) {
	case notificationUnreadable:
		exchangeOf(r.Context()).refusingFidelity(vocab.RefusalStageBuild)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errNotifyUnreadable})
		return
	case notificationAddressUnreadable:
		refuse(RefusalRouting, http.StatusUnprocessableEntity, errNotifyAddress, reasonNotifyAddress)
		return
	case notificationAddressNotRekeyable:
		refuse(RefusalRouting, http.StatusUnprocessableEntity, errNotifyNotRekeyable, reasonNotifyNotRekeyable)
		return
	}
	// A module ingress client builds no notification; the route admits the
	// provider's own system alone.
	if _, ok := g.ingressBuilder(w, r, leg); !ok {
		return
	}
	ct := carriedFHIRMediaType(r.Header.Get("Content-Type"))
	frame := map[string]string{
		shnsdk.FrameHeaderSubscriptionEndpoint: addr.SealedEndpoint,
		shnsdk.FrameHeaderSubscriptionChannel:  channels[0],
		shnsdk.FrameHeaderStatedBase:           sub.base.String(),
		"Content-Type":                         ct,
	}
	r = r.WithContext(withFindingContext(r.Context(), findingContext{LegType: leg, Seam: "notifier-ingress", Whose: "own"}))
	scope.leg = leg
	ex := g.exchanges.Begin(workstreamPA)
	child := g.ingressCorrelation(w, r)
	content := Content{WorkstreamType: workstreamPA, Payload: relay.Exact(relay.NewBody(n.Body(), relay.OriginIngressRequest), ct), Carried: true, SubscribeFrame: frame}
	var stated statedAnswer
	var answerType string
	ctx := withStatedAnswerSink(withAnswerMediaTypeSink(r.Context(), &answerType), &stated)
	// The leg names no patient on either side (its row's subject: none).
	answer, err := g.OriginateLeg(ctx, r, addr.Subscriber, leg, "", child, "", content)
	spec, _ := legSpecFor(leg)
	legProj := Leg{Type: leg, Physics: spec.Physics, Content: content}
	if err != nil {
		g.recordLeg(ex.ID, legProj.Project(child, "error"))
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	g.recordLeg(ex.ID, legProj.Project(child, "answered"))
	g.writeRelayedAnswer(w, stated, writableAnswerMediaType(answerType), relay.Exact(relay.NewBody(answer, relay.OriginPeerFrame), writableAnswerMediaType(answerType)),
		relay.Key{Leg: leg, Role: relay.RoleRequester, Direction: relay.DirectionResponse, Outcome: relay.OutcomeAnswered})
}
