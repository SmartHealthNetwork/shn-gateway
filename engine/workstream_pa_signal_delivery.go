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

// The subscriber's half of a topic Subscription's notification, on its
// signal leg or, for a topic that has one, its content leg
// (deliveryTopic.content). Its rules are written out where each topic's
// delivery is (workstream_pa_cdex_signal.go for the HRex Task topic,
// workstream_pa_pas_signal.go and workstream_pa_pas_notify.go for the PAS
// topic), each held here to the topic it is given (deliveryTopic).

// handleSignalDelivery delivers a notification of dt's topic, on its signal
// leg or its content leg (dt.content), to the endpoint this subscriber's own
// system wrote, held to the rules its topic's delivery writes out.
func (g *Gateway) handleSignalDelivery(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, body []byte, answerTok string, dt deliveryTopic) {
	ctx := r.Context()
	leg := dt.inbound
	topic := dt.topic
	notifier := env.Metadata.Sender
	refuse := func(rule string, status int, msg string) {
		exchangeOf(ctx).refusing(rule)
		g.refuseInbound(w, r, leg, env, tok, answerTok, status, msg, nil)
	}
	// A gateway whose participant has not asked for the topic's
	// notifications takes no such leg: refused on routing, as every leg it
	// does not take, never read as a failure of its own.
	if dt.taken != nil && !dt.taken(g) {
		refuse(RefusalRouting, http.StatusUnprocessableEntity, dt.refusals.notTaken)
		return
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
	b := subscription.Binding{Subscriber: g.cfg.HolderID, Notifier: notifier, Topic: topic.Token()}
	loop := func(raw string) bool {
		return subscription.LooksLikeNotificationAddress(raw, func(id string) bool { _, ok := g.cfg.Reg.Lookup(id); return ok }, isSubscriptionTopic)
	}
	d, rf := keys.OpenDelivery(b, frame.endpoint, frame.channel, loop)
	if rf != nil {
		refuse(rf.Primitive, rf.Status, rf.Reason)
		return
	}
	// The signal row's check, by structure: the message is a
	// notification of the topic that discloses no patient's data. The
	// classifier's refusal is equivalent to ParseNotification's for every
	// body that reaches it (each refuses what is not one FHIR JSON object it
	// can read), so no body tells the two apart; it is kept as the shared
	// reading every check on a body starts from.
	if kind, _, err := subscription.ClassifyBody(body); err != nil || kind != subscription.BodyFHIRJSON {
		refuse(RefusalAuthority, http.StatusForbidden, dt.refusals.content)
		return
	}
	// The row is checked against the content by structure: a signal
	// leg carries a notification that discloses no patient's data, and a
	// content leg one that does (the notifier's gateway picks the leg by the
	// same test), so a message on either that fails its row's test is
	// refused.
	n, rf := subscription.ParseNotification(body, topic)
	if rf != nil || (subscription.Leg(topic, n) == topic.ContentLeg()) != dt.content {
		refuse(RefusalAuthority, http.StatusForbidden, dt.refusals.content)
		return
	}
	if !n.ContextDeclared(topic.NotificationShape()) {
		refuse(RefusalAuthority, http.StatusForbidden, dt.refusals.context)
		return
	}
	base := ""
	if frame.hasStatedBase {
		base = frame.statedBase
	}
	// A signal leg's notification is held to the closed SubscriptionStatus
	// shape that shows it names no patient; a content leg's names one, and
	// is carried to its own system as the notifier's system sent it.
	if !dt.content {
		if rf := n.CheckShape(topic.NotificationShape(), base); rf != nil {
			refuse(RefusalAuthority, http.StatusForbidden, dt.refusals.shape+rf.Reason)
			return
		}
	}
	if rk := dt.rekey; rk != nil {
		switch rk.read(body, relay.OriginPeerFrame, base) {
		case notificationUnreadable:
			exchangeOf(ctx).refusingFidelity(vocab.RefusalStageBuild)
			g.refuseInbound(w, r, leg, env, tok, answerTok, http.StatusBadRequest, rk.unreadable, nil)
			return
		case notificationAddressUnreadable:
			refuse(RefusalRouting, http.StatusUnprocessableEntity, rk.address)
			return
		case notificationAddressNotRekeyable:
			refuse(RefusalRouting, http.StatusUnprocessableEntity, rk.notRekeyable)
			return
		}
	}
	headers, ok := deliveryHeaders(d.Headers)
	if !ok {
		exchangeOf(ctx).refusingFidelity(vocab.RefusalStageBuild)
		g.refuseInbound(w, r, leg, env, tok, answerTok, http.StatusUnprocessableEntity, refusalSignalHeader, nil)
		return
	}
	target, err := url.Parse(d.Endpoint)
	if err != nil {
		// Unreachable: OpenDelivery holds the endpoint to the endpoint rule,
		// which parses it.
		refuse(RefusalIntegrity, http.StatusUnprocessableEntity, refusalSignalEndpointUnreadable)
		return
	}
	in := deliveryInput{notifier: notifier, statedBase: frame.statedBase, hasStatedBase: frame.hasStatedBase}
	p, err := g.deliveredNotification(exchangeOf(ctx), leg.tx, body, requestMediaType(ctx), in)
	if err != nil {
		exchangeOf(ctx).refusingFidelity(vocab.RefusalStageVerify)
		g.refuseInbound(w, r, leg, env, tok, answerTok, http.StatusBadGateway, refusalSignalEdit, nil)
		return
	}
	ctx = withNativeForward(withResponderCorrelation(ctx, env.Metadata.CorrelationID))
	exchangeOf(ctx).trackForward()
	ctx = withForwardHeaders(ctx, headers)
	r = r.WithContext(ctx)
	up, result, err := st.notifyDelivery.sendVia(ctx, st.notifyDelivery.client, http.MethodPost, target.String(), target, p, leg.tx, "notification endpoint")
	if err != nil {
		g.responderFailed(w, r, leg, env, tok, answerTok, err)
		return
	}
	// A signal leg's answer names no patient, at every status (N5): it holds
	// no resource (subscription.CheckSignalAnswer), a FHIR server's
	// acknowledgement Bundle included. A content leg's names the patient the
	// leg's token names, and the endpoint's own answer is carried as it
	// answered.
	subject := ""
	if dt.content {
		subject = tok.Subject
	} else if rf := subscription.CheckSignalAnswer(up.raw); rf != nil {
		refuse(rf.Primitive, rf.Status, rf.Reason)
		return
	}
	if result.Status != 0 {
		g.respondLegError(w, r, leg.frame, leg.op, leg.tx, env.Metadata.CorrelationID, result, subject, notifier, "", answerTok)
		return
	}
	answer := LegResult{Response: relay.Exact(up.body, up.contentType), AnswerStatus: up.status, AnswerHeaders: up.headers}
	g.respondLeg(w, r, leg.frame, leg.op, leg.tx, env.Metadata.CorrelationID, answer.Response, answer.stated(), subject, notifier, "", answerTok)
}
