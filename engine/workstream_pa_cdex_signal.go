//go:build !shn_coreonly

package engine

import (
	"net/http"
	"net/url"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/pa/subscription"
	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The subscriber's half of a notification (the notifications design §6.2
// step 5, §6.5, §7.4, §9): a provider's CDex Task notification the Hub
// delivers to the payer's gateway on the HRex Task topic's signal leg
// (cdex-task-signal), which the gateway delivers to the notification endpoint
// the payer's own system wrote into its Subscription, with the header values
// it wrote there. Every rule is a network rule, the same at every conformance
// level:
//   - the sealed endpoint and headers the request frame carries
//     (subscriptionEndpoint, subscriptionChannel) must open under this
//     gateway's key with this payer, the notifier the Hub authenticated and
//     the topic as their associated data (subscription.Keys.OpenDelivery):
//     missing, altered, sealed for another notifier, topic or endpoint, or
//     under a key past its rotation's overlap, it is refused 403 on
//     authority, nothing delivered;
//   - the opened endpoint is held to the delivery rule (§6.5,
//     subscription.CheckEndpoint): one that is not an absolute https URL with
//     no userinfo or fragment is refused 422 on integrity, and a notification
//     address of this network 422 on routing;
//   - the notification must be no-content by structure (§5's test): one that
//     is not, or is no notification of the topic, is refused 403 on
//     authority, since the leg carries no patient's data (§7.4), and so is
//     one with a notification-event part named additional-context, which
//     the topic's notification shape declares none of;
//   - a notification whose places E-17 re-keys cannot be read one way only
//     (a member name repeated under folding, or JSON relay cannot read) is
//     refused 502 on the fidelity of SHN's own edit, nothing delivered;
//   - a notification outside the HRex Task topic's closed notification
//     shape (subscription.Notification.CheckShape: the closed
//     SubscriptionStatus Bundle an instance $status answer is held to, as a
//     history notification; no extension, no signature, no reference but
//     its subscription's and each focus's) is refused 502 on authority,
//     nothing delivered;
//   - an address into the provider's own server, at a place E-17 re-keys,
//     that is not under the stated base (readNotification) is refused 422
//     on routing, nothing delivered: E-17 cannot write it at this gateway's
//     base;
//   - an http or https address at a place E-17 re-keys that is not an
//     absolute URL with a host written plainly (unreadableAddress) is
//     refused 422 on routing, nothing delivered: it cannot be re-keyed, and
//     the payer's system could follow it into the provider's server;
//   - the header values the payer wrote are presented as HTTP fields, each
//     "name: value": one that cannot be is refused 422, the fidelity of the
//     network's delivery of the payer's own values;
//   - the notification is then sent, with the leg's registered edits made
//     (E-17: its addresses into the provider's server re-keyed to this
//     gateway's base for the provider) and disclosed, by POST to that
//     endpoint, through the client and the checks every forward to the
//     participant's own system takes (no redirect to http, no body sent across
//     origins, the forward's deadline and the answer's size limit);
//   - the endpoint's answer, at every status, is judged by its structure
//     (subscription.CheckErrorAnswer: the leg names no patient, a lead ruling
//     applying N5): FHIR passes only as an OperationOutcome holding no
//     resource, any other FHIR is refused 502 on authority, and an answer
//     that is not FHIR, or has no body, is carried, as sent, to the
//     provider's system.

var legCDexTaskSignalInbound = inboundLeg{"payer-payment", "cdex-task-signal-response", legCDexTaskSignal}

// The payer gateway's refusals of a notification it is delivered.
const (
	// refusalSignalNoSealedValues: the request frame carries no sealed
	// endpoint or headers.
	refusalSignalNoSealedValues = "the notification carries no sealed endpoint or headers of this subscriber's own (subscriptionEndpoint, subscriptionChannel), so it has nowhere to be delivered"
	// refusalSignalContent: a signal-leg message that carries content, or is
	// not a notification of the topic.
	refusalSignalContent = "the message on this notification leg is not a notification of the HRex Task topic that discloses no patient's data (per-leg authority): a CDex Task notification is id-only"
	// refusalSignalContext: a notification-event's additional-context,
	// which the topic's notification shape does not declare.
	refusalSignalContext = "a notification-event carries an additional-context, which the HRex Task topic does not declare (per-leg authority): a CDex Task notification names the Task alone"
	// refusalSignalUnreadable: a notification whose places E-17 re-keys
	// cannot be read one way only.
	refusalSignalUnreadable = "the notification cannot be read one way only at the places this gateway re-keys to its own base (JSON the network cannot read unambiguously, such as a lone surrogate in a string), so the gateway cannot make its own registered edit (fidelity of SHN's own edit); nothing was delivered"
	// refusalSignalAddress: an address in a place E-17 re-keys that no URL
	// reader reads one way only.
	refusalSignalAddress = "the notification holds an http or https address, at a place this gateway re-keys to its own base, that no URL reader reads one way only (it is not an absolute URL naming its server plainly), so it cannot be re-keyed (routing); nothing was delivered"
	// refusalSignalNotRekeyable: an address into the provider's own server,
	// at a place E-17 re-keys, that is not under its stated base.
	refusalSignalNotRekeyable = "the notification holds an address into the provider's own server, at a place this gateway re-keys to its own base, that is not under the provider's stated FHIR base (a fragment, a dot segment, or a path outside the base, escaped or not), so it cannot be re-keyed (routing); nothing was delivered"
	// refusalSignalShape: a notification outside the closed SubscriptionStatus
	// Bundle shape. The reason it is outside follows.
	refusalSignalShape = "the notification is outside the HRex Task topic's closed notification shape, so it cannot be shown to name no patient (per-leg authority); nothing was delivered: "
	// refusalSignalHeader: a header value of the subscriber's own that
	// cannot be presented as an HTTP field. The value is never named: it is
	// the subscriber's secret, and the refusal reaches the notifier.
	refusalSignalHeader = "a channel.header value the subscriber wrote cannot be presented as an HTTP field (name: value), so the notification is not delivered"
	// refusalSignalEndpointUnreadable: the opened endpoint does not parse as
	// a URL (unreachable past the delivery rule, which parses it).
	refusalSignalEndpointUnreadable = "the notification's endpoint cannot be read as a URL, so the notification is not delivered"
	// refusalSignalNoKeys: this gateway holds no subscription keys.
	refusalSignalNoKeys = "this gateway holds no subscription keys, so it cannot open a notification's sealed endpoint; nothing was delivered"
	// refusalSignalEdit: the gateway's own edit of the notification failed
	// its verification.
	refusalSignalEdit = "the gateway cannot deliver the notification with its own registered edits verified (fidelity of SHN's own edit); nothing was delivered"
)

// handleCDexSignalInbound delivers a provider's CDex Task notification to
// the payer's own endpoint (the rules above).
func (g *Gateway) handleCDexSignalInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, body []byte, answerTok string) {
	ctx := r.Context()
	leg := legCDexTaskSignalInbound
	notifier := env.Metadata.Sender
	refuse := func(rule string, status int, msg string) {
		exchangeOf(ctx).refusing(rule)
		g.refuseInbound(w, r, leg, env, tok, answerTok, status, msg, nil)
	}
	st := g.paState()
	keys, haveKeys := g.subscriberKeys()
	if st == nil || st.notifyDelivery == nil || !haveKeys {
		exchangeOf(ctx).ownFailure()
		g.refuseInbound(w, r, leg, env, tok, answerTok, http.StatusInternalServerError, refusalSignalNoKeys, nil)
		return
	}
	frame := requestFrameSubscribe(ctx)
	if !frame.hasEndpoint || !frame.hasChannel {
		refuse(RefusalAuthority, http.StatusForbidden, refusalSignalNoSealedValues)
		return
	}
	b := subscription.Binding{Subscriber: g.cfg.HolderID, Notifier: notifier, Topic: cdexTopic.Token()}
	loop := func(raw string) bool {
		return subscription.LooksLikeNotificationAddress(raw, func(id string) bool { _, ok := g.cfg.Reg.Lookup(id); return ok }, isSubscriptionTopic)
	}
	d, rf := keys.OpenDelivery(b, frame.endpoint, frame.channel, loop)
	if rf != nil {
		refuse(rf.Primitive, rf.Status, rf.Reason)
		return
	}
	// The signal row's check, by structure (§7.4): the message is a
	// notification of the topic that discloses no patient's data. The
	// classifier's refusal is equivalent to ParseNotification's for every
	// body that reaches it (each refuses what is not one FHIR JSON object it
	// can read), so no body tells the two apart; it is kept as the shared
	// reading every check on a body starts from.
	if kind, _, err := subscription.ClassifyBody(body); err != nil || kind != subscription.BodyFHIRJSON {
		refuse(RefusalAuthority, http.StatusForbidden, refusalSignalContent)
		return
	}
	n, rf := subscription.ParseNotification(body, cdexTopic)
	if rf != nil || !n.NoContent() {
		refuse(RefusalAuthority, http.StatusForbidden, refusalSignalContent)
		return
	}
	if !n.ContextDeclared(cdexTopic.NotificationShape()) {
		refuse(RefusalAuthority, http.StatusForbidden, refusalSignalContext)
		return
	}
	base := ""
	if frame.hasStatedBase {
		base = frame.statedBase
	}
	if rf := n.CheckShape(cdexTopic.NotificationShape(), base); rf != nil {
		refuse(RefusalAuthority, http.StatusBadGateway, refusalSignalShape+rf.Reason)
		return
	}
	switch readNotification(body, relay.OriginPeerFrame, base) {
	case notificationUnreadable:
		exchangeOf(ctx).refusingFidelity(vocab.RefusalStageBuild)
		g.refuseInbound(w, r, leg, env, tok, answerTok, http.StatusBadGateway, refusalSignalUnreadable, nil)
		return
	case notificationAddressUnreadable:
		refuse(RefusalRouting, http.StatusUnprocessableEntity, refusalSignalAddress)
		return
	case notificationAddressNotRekeyable:
		refuse(RefusalRouting, http.StatusUnprocessableEntity, refusalSignalNotRekeyable)
		return
	}
	headers, ok := deliveryHeaders(d.Headers)
	if !ok {
		exchangeOf(ctx).refusingFidelity(vocab.RefusalStageBuild)
		g.refuseInbound(w, r, leg, env, tok, answerTok, http.StatusUnprocessableEntity, refusalSignalHeader, nil)
		return
	}
	target, err := url.Parse(d.Endpoint)
	if err != nil {
		// Unreachable: OpenDelivery holds the endpoint to §6.5's rule,
		// which parses it.
		refuse(RefusalIntegrity, http.StatusUnprocessableEntity, refusalSignalEndpointUnreadable)
		return
	}
	in := deliveryInput{notifier: notifier, statedBase: frame.statedBase, hasStatedBase: frame.hasStatedBase}
	p, err := g.deliveredNotification(exchangeOf(ctx), legCDexTaskSignal, body, requestMediaType(ctx), in)
	if err != nil {
		exchangeOf(ctx).refusingFidelity(vocab.RefusalStageVerify)
		g.refuseInbound(w, r, leg, env, tok, answerTok, http.StatusBadGateway, refusalSignalEdit, nil)
		return
	}
	ctx = withNativeForward(withResponderCorrelation(ctx, env.Metadata.CorrelationID))
	exchangeOf(ctx).trackForward()
	ctx = withForwardHeaders(ctx, headers)
	r = r.WithContext(ctx)
	up, result, err := st.notifyDelivery.sendVia(ctx, st.notifyDelivery.client, http.MethodPost, target.String(), target, p, legCDexTaskSignal, "notification endpoint")
	if err != nil {
		g.responderFailed(w, r, leg, env, tok, answerTok, err)
		return
	}
	// The endpoint's answer names no patient, at every status (N5).
	if rf := subscription.CheckErrorAnswer(up.raw); rf != nil {
		refuse(rf.Primitive, rf.Status, rf.Reason)
		return
	}
	if result.Status != 0 {
		g.respondLegError(w, r, leg.frame, leg.op, legCDexTaskSignal, env.Metadata.CorrelationID, result, "", notifier, "", answerTok)
		return
	}
	answer := LegResult{Response: relay.Exact(up.body, up.contentType), AnswerStatus: up.status, AnswerHeaders: up.headers}
	g.respondLeg(w, r, leg.frame, leg.op, legCDexTaskSignal, env.Metadata.CorrelationID, answer.Response, answer.stated(), "", notifier, "", answerTok)
}

// deliveryHeaders are the header values a subscriber wrote into its
// Subscription's channel.header, as the HTTP fields its gateway presents to
// its endpoint (subscription.HeaderFields). ok is false when one is not.
func deliveryHeaders(values []string) (http.Header, bool) { return subscription.HeaderFields(values) }
