//go:build !shn_coreonly

package engine

import (
	"net/http"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/pa/subscription"
	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The notifier's half of a topic Subscription's notification. Its rules are
// written out where each topic's notifier is (workstream_pa_cdex_notify.go
// for the HRex Task topic), each held here to the topic it is given
// (notifierTopic).

// handleNotifyIngress is the notifier's system's notification of nt's topic
// at a notification address this gateway wrote, held to the rules its
// topic's notifier writes out: the topic's signal leg carries a notification
// with no content, and its content leg one with content (nt.content), which
// a topic with no content leg refuses. The route's own handler records the
// signal leg first, as its route's constant (the stability fence reads it
// there), and a notification with content is recorded on the content leg.
func (g *Gateway) handleNotifyIngress(w http.ResponseWriter, r *http.Request, nt notifierTopic) {
	topic := nt.topic
	leg := topic.SignalLeg()
	w, scope := g.withScope(w, relay.RoleRequester)
	w = &fhirOperationWriter{w}
	refuse := func(rule string, status int, msg string, reason refusalReason) {
		exchangeOf(r.Context()).refusingFor(rule, reason)
		writeJSON(w, status, map[string]string{"error": msg})
	}
	// The address the notifier's system posted to, as this gateway wrote
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
	var sub *cdexSubscriptionState
	if st != nil {
		sub = nt.state(st)
	}
	if sub == nil || !isAddress || len(auths) != 1 || len(channels) != 1 ||
		sub.keys.VerifyCredential(addr, channels[0], auths[0]) != nil {
		refuse(RefusalAuthentication, http.StatusUnauthorized, errNotifyCredential, reasonIngressAuthentication)
		return
	}
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
		refuse(RefusalRouting, http.StatusBadRequest, string(nt.reasons.notANotification)+why, nt.reasons.notANotification)
		return
	}
	n, rf := subscription.ParseNotification(body, topic)
	if rf != nil {
		refuse(RefusalRouting, http.StatusBadRequest, string(nt.reasons.notANotification)+rf.Reason, nt.reasons.notANotification)
		return
	}
	// The leg by structure (the no-content test): no content rides the
	// signal leg; content rides the content leg of a topic that has one,
	// and is refused on a topic that has none.
	subject := ""
	if subscription.Leg(topic, n) != leg {
		if nt.content == nil {
			refuse(RefusalAuthority, http.StatusForbidden, string(nt.reasons.content), nt.reasons.content)
			return
		}
		leg = topic.ContentLeg()
		exchangeOf(r.Context()).leg(leg)
	}
	if !n.ContextDeclared(topic.NotificationShape()) {
		refuse(RefusalAuthority, http.StatusForbidden, string(nt.reasons.context), nt.reasons.context)
		return
	}
	if leg == topic.SignalLeg() {
		if rf := n.CheckShape(topic.NotificationShape(), sub.base.String()); rf != nil {
			refuse(RefusalAuthority, http.StatusForbidden, string(nt.reasons.shape)+rf.Reason, nt.reasons.shape)
			return
		}
	} else {
		if rf := topic.CheckContent(nt.content.subscriber(g, addr.Subscriber), n); rf != nil {
			refuse(rf.Primitive, rf.Status, string(nt.content.notSubscribers)+rf.Reason, nt.content.notSubscribers)
			return
		}
		// The content checks' findings name the leg they judge.
		r = r.WithContext(withFindingContext(r.Context(), findingContext{LegType: leg, Seam: "notifier-ingress", Whose: "own"}))
		pci, nr := nt.content.bind(g, r.Context(), n, body, sub.base.String())
		if nr != nil {
			switch {
			case nr.recorded:
			case nr.reason == "":
				exchangeOf(r.Context()).refusing(nr.rule)
			default:
				exchangeOf(r.Context()).refusingFor(nr.rule, nr.reason)
			}
			writeJSON(w, nr.status, map[string]string{"error": nr.msg})
			return
		}
		subject = pci
		if rd := nt.content.readable; rd != nil && rd(n.Body(), relay.OriginIngressRequest, sub.base.String()) == notificationUnreadable {
			exchangeOf(r.Context()).refusingFidelity(vocab.RefusalStageBuild)
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": nt.content.unreadable})
			return
		}
	}
	if rk := nt.rekey; rk != nil {
		switch rk.read(n.Body(), relay.OriginIngressRequest, sub.base.String()) {
		case notificationUnreadable:
			exchangeOf(r.Context()).refusingFidelity(vocab.RefusalStageBuild)
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": rk.unreadable})
			return
		case notificationAddressUnreadable:
			refuse(RefusalRouting, http.StatusUnprocessableEntity, string(rk.address), rk.address)
			return
		case notificationAddressNotRekeyable:
			refuse(RefusalRouting, http.StatusUnprocessableEntity, string(rk.notRekeyable), rk.notRekeyable)
			return
		}
	}
	// A module ingress client builds no notification; the route admits the
	// notifier's own system alone.
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
	// The content leg's involved list reads the notification at the base its
	// binding read it at.
	ctx = withNotifiedBase(ctx, sub.base.String())
	// The signal leg names no patient on either side (its row's subject:
	// none); the content leg names the bound patient.
	answer, err := g.OriginateLeg(ctx, r, addr.Subscriber, leg, subject, child, "", content)
	spec, _ := legSpecFor(leg)
	legProj := Leg{Type: leg, Physics: spec.Physics, Content: content}
	if subject != "" {
		legProj.Subjects = []string{subject}
	}
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
