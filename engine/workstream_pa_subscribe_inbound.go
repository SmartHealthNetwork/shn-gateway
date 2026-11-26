//go:build !shn_coreonly

package engine

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
	"github.com/SmartHealthNetwork/shn-gateway/internal/jsonmember"
	"github.com/SmartHealthNetwork/shn-gateway/pa/subscription"
	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A topic Subscription's subscribe leg at the notifier's gateway: a
// subscriber's Subscription interaction the Hub delivered, answered by the
// notifier's own Subscription endpoint. Its rules are written out where each
// topic's subscribe leg is (workstream_pa_cdex_subscription.go for the HRex
// Task topic), each held here to the topic it is given (subscribeRecipient).
// The refusals that name no role are shared by every topic, under the names
// the HRex Task topic gave them (refusalCDexSubscribe*).

// subscribeRecipient is what the notifier's subscribe leg takes from a topic:
// the topic, the leg as the inbound dispatcher answers it, the responder and
// the subscription state that reach the notifier's Subscription endpoint, the
// endpoint's certificate tag, how the subscriber is held (its holder for the
// topic's filter, and the RequesterHeader value its forward carries, "" for
// none), and the refusals that name the roles.
type subscribeRecipient struct {
	topic      subscription.Topic
	inbound    inboundLeg
	state      func(*paGatewayState) (*nativeResponder, *cdexSubscriptionState)
	endpoint   endpointcert.Kind
	subscriber func(g *Gateway, id string) (holder subscription.Holder, requester string)
	refusals   subscribeRefusals
}

// subscribeRefusals are the subscribe leg's refusals that name the roles: the
// notifier takes no Subscription; a search answer holds one that is not the
// subscriber's; an interaction's address below the notifier's base cannot be
// built.
type subscribeRefusals struct {
	notTaken, searchNotOwned, target string
}

// handleSubscribeInbound answers a subscriber's Subscription interaction on
// sr's topic that the Hub delivered to this notifier's gateway, held to the
// rules its topic's subscribe leg writes out. With no subscription state it
// refuses the leg as routing, before reading anything.
func (g *Gateway) handleSubscribeInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, body []byte, answerTok string, sr subscribeRecipient) {
	ctx := r.Context()
	leg := sr.inbound
	refuse := func(rule string, status int, msg string) {
		exchangeOf(ctx).refusing(rule)
		g.refuseInbound(w, r, leg, env, tok, answerTok, status, msg, nil)
	}
	// A library refusal names its primitive; one of the fidelity of SHN's
	// own edit (an unreadable Subscription, which E-19 cannot be made on)
	// is recorded through refusingFidelity, at the edit's build.
	refuseLib := func(rf *subscription.Refusal) {
		exchangeOf(ctx).refusing(rf.Primitive)
		g.refuseInbound(w, r, leg, env, tok, answerTok, rf.Status, rf.Reason, nil)
	}
	refuseUnreadable := func(rf *subscription.Refusal) {
		exchangeOf(ctx).refusingFidelity(vocab.RefusalStageBuild)
		g.refuseInbound(w, r, leg, env, tok, answerTok, rf.Status, rf.Reason, nil)
	}
	var responder *nativeResponder
	var sub *cdexSubscriptionState
	if st := g.paState(); st != nil {
		responder, sub = sr.state(st)
	}
	if responder == nil || sub == nil {
		refuse(RefusalRouting, http.StatusUnprocessableEntity, sr.refusals.notTaken)
		return
	}
	frame := requestFrameSubscribe(ctx)
	if !frame.hasRequest {
		refuse(RefusalRouting, http.StatusBadRequest, refusalCDexSubscribeNoRequest)
		return
	}
	// An interaction the leg never carries is refused by its own rule
	// first, from the method and path alone: a PATCH 405 on fidelity, an
	// instance's history and the type-level $status 403 on authority.
	if rf := subscription.CheckInteraction(frame.request); rf != nil {
		if rf.Unreadable() {
			refuseUnreadable(rf)
			return
		}
		refuseLib(rf)
		return
	}
	req, err := shnsdk.ParseSubscriptionRequest(frame.request)
	if err != nil {
		// A search's or a page's query outside its set is the leg's
		// authority; every other value outside the grammar, a read's
		// or a vread's query among them, is routing, as an id is.
		if errors.Is(err, shnsdk.ErrSubscriptionRequestQuery) && !readsQuery(frame.request) {
			refuse(RefusalAuthority, http.StatusForbidden, refusalCDexSubscribeRequest+err.Error())
			return
		}
		refuse(RefusalRouting, http.StatusBadRequest, refusalCDexSubscribeRequest+err.Error())
		return
	}
	for _, id := range []string{req.ID, req.VersionID} {
		if id == "" {
			continue
		}
		if rf := subscription.CheckID(id); rf != nil {
			refuseLib(rf)
			return
		}
	}
	writes := req.Interaction == shnsdk.SubscriptionCreate || req.Interaction == shnsdk.SubscriptionUpdate
	if !writes && len(body) != 0 {
		refuse(RefusalIntegrity, http.StatusBadRequest, refusalCDexSubscribeBody)
		return
	}
	// An If-Match is framed on an update or a delete alone, and carried to
	// the notifier's system as sent, as an HTTP field it can carry.
	conditional := req.Interaction == shnsdk.SubscriptionUpdate || req.Interaction == shnsdk.SubscriptionDelete
	if frame.hasIfMatch && (!conditional || frame.ifMatch == "" || !shnsdk.FrameCarriesHeaderValue(frame.ifMatch)) {
		refuse(RefusalIntegrity, http.StatusBadRequest, refusalCDexSubscribeIfMatch)
		return
	}
	// A Prefer is the subscriber's one return preference, carried to the
	// notifier's system as sent on the interaction's own forward; anything
	// else framed as one is refused, so no other preference crosses.
	if frame.hasPrefer && (!subscription.IsReturnPreference(frame.prefer) || !shnsdk.FrameCarriesHeaderValue(frame.prefer)) {
		refuse(RefusalIntegrity, http.StatusBadRequest, refusalCDexSubscribePrefer)
		return
	}
	subscriber := env.Metadata.Sender
	holder, requester := sr.subscriber(g, subscriber)
	topic := sr.topic
	ingress := g.cfg.IngressBaseURL

	// Every call to the notifier's system is the leg's, tracked and named
	// as the read's is.
	ctx = withNativeForward(withResponderCorrelation(ctx, env.Metadata.CorrelationID))
	exchangeOf(ctx).trackForward()
	// The forward names the subscriber only where its topic registers a
	// header for it (requester; "" names none).
	forwardHeaders := func() http.Header {
		if requester == "" {
			return http.Header{}
		}
		return http.Header{RequesterHeader: {requester}}
	}
	ctx = withForwardHeaders(ctx, forwardHeaders())
	ctx = endpointcert.WithTag(ctx, endpointcert.Of(sr.endpoint))
	r = r.WithContext(ctx)
	empty := relay.Exact(relay.NewBody(nil, relay.OriginPeerFrame), "")
	// send makes one call to the notifier's system, forwarding p. own is the
	// interaction's own forward, never the gateway's own read before it: it
	// alone carries the subscriber's If-Match (framed on an update or a
	// delete alone) and its return preference, Prefer, as sent.
	// disclosed are EditedHeader values of edits the subscriber's gateway made to
	// p's body, disclosed before this gateway's own.
	send := func(method string, target *url.URL, p relay.Payload, own bool, disclosed ...string) (upstreamReply, LegResult, bool) {
		callCtx := ctx
		if (own && (frame.hasIfMatch || frame.hasPrefer)) || len(disclosed) > 0 {
			h := forwardHeaders()
			if own && frame.hasIfMatch {
				h[shnsdk.FrameHeaderIfMatch] = []string{frame.ifMatch}
			}
			if own && frame.hasPrefer {
				h[shnsdk.FrameHeaderPrefer] = []string{frame.prefer}
			}
			for _, v := range disclosed {
				h.Add(EditedHeader, v)
			}
			callCtx = withForwardHeaders(ctx, h)
		}
		up, bad, err := responder.sendVia(callCtx, sub.client, method, target.String(), target, p, leg.tx, "Subscription "+string(req.Interaction))
		if err != nil {
			g.responderFailed(w, r, leg, env, tok, answerTok, err)
			return upstreamReply{}, LegResult{}, false
		}
		return up, bad, true
	}
	carry := func(up upstreamReply, bad LegResult) {
		// The notifier's own answer that is not a 2xx is carried as sent,
		// under no patient, once it holds no resource but an
		// OperationOutcome's own when it is FHIR JSON (the leg names no
		// patient, N5).
		if rf := subscription.CheckErrorAnswer(up.raw); rf != nil {
			refuseLib(rf)
			return
		}
		g.respondLegError(w, r, leg.frame, leg.op, leg.tx, env.Metadata.CorrelationID, bad, "", subscriber, "", answerTok)
	}
	target := func(path, rawQuery string, hasQuery bool) (*url.URL, bool) {
		t, err := subscriptionTarget(sub.base, path, rawQuery, hasQuery)
		if err != nil {
			exchangeOf(ctx).ownFailure()
			g.refuseInbound(w, r, leg, env, tok, answerTok, http.StatusInternalServerError, sr.refusals.target, nil)
			return nil, false
		}
		return t, true
	}

	// A create's or an update's body is checked and re-keyed first (its
	// shape, the topic's filter, E-19), so a body the network refuses sends
	// nothing to the notifier's system, not even the gateway's own read.
	p := empty
	if req.Interaction == shnsdk.SubscriptionCreate || req.Interaction == shnsdk.SubscriptionUpdate {
		var rf *subscription.Refusal
		var unreadable bool
		p, rf, unreadable = g.subscriptionRekeyed(ctx, sub, body, frame, subscriber, holder, topic)
		switch {
		case unreadable:
			refuseUnreadable(rf)
			return
		case rf != nil:
			refuseLib(rf)
			return
		}
	}

	// An update, a delete and a $status act or answer without carrying the
	// Subscription back: the Subscription is read first, on the same leg
	// with the same token, and the request is forwarded only when it is the
	// subscriber's. An update of one that does not exist (its body already
	// checked above) is forwarded as an update that creates it.
	switch req.Interaction {
	case shnsdk.SubscriptionUpdate, shnsdk.SubscriptionDelete, shnsdk.SubscriptionStatus:
		readAt, ok := target("Subscription/"+req.ID, "", false)
		if !ok {
			return
		}
		up, bad, ok := send(http.MethodGet, readAt, empty, false)
		if !ok {
			return
		}
		switch {
		case bad.Status != 0 && req.Interaction == shnsdk.SubscriptionUpdate && (bad.Status == http.StatusNotFound || bad.Status == http.StatusGone):
			// An update as create: its body was checked above.
		case bad.Status != 0:
			carry(up, bad)
			return
		case !sub.owns(up, ingress, subscriber, topic):
			g.refuseSubscriptionNotFound(w, r, env, tok, answerTok, leg)
			return
		}
	}

	var (
		method   = http.MethodGet
		at       *url.URL
		ok       bool
		checkOwn = false
		allOwned = false
		sealed   []string
	)
	switch req.Interaction {
	case shnsdk.SubscriptionCreate, shnsdk.SubscriptionUpdate:
		method = http.MethodPost
		path := "Subscription"
		if req.Interaction == shnsdk.SubscriptionUpdate {
			method, path = http.MethodPut, "Subscription/"+req.ID
		}
		if at, ok = target(path, "", false); !ok {
			return
		}
		// The subscriber's gateway declares E-18 by the published frame contract:
		// a create's or an update's subscriptionEndpoint and
		// subscriptionChannel are E-18's output, the channel it sealed and
		// removed, and a forward reaches here only with both (one missing
		// is refused above). Decided from the frame, never the body.
		if frame.hasEndpoint && frame.hasChannel {
			sealed = []string{headerSealDisclosure(p)}
		}
	case shnsdk.SubscriptionRead, shnsdk.SubscriptionVRead:
		// The query (_elements, _summary) is carried as sent: the ownership
		// test decides what an answer that hides the channel gets.
		if at, ok = target(req.Path(), req.RawQuery, req.RawQuery != ""); !ok {
			return
		}
		checkOwn = true
	case shnsdk.SubscriptionDelete:
		method = http.MethodDelete
		if at, ok = target(req.Path(), "", false); !ok {
			return
		}
	case shnsdk.SubscriptionStatus:
		if at, ok = target(req.Path(), "", false); !ok {
			return
		}
	case shnsdk.SubscriptionSearch:
		var rf *subscription.Refusal
		p, rf = g.searchRekeyed(ctx, empty, req, frame, subscriber, topic)
		if rf != nil {
			refuseLib(rf)
			return
		}
		q, _ := p.EditedQuery()
		if at, ok = target("Subscription", q, true); !ok {
			return
		}
		allOwned = true
	case shnsdk.SubscriptionPage:
		if rf := subscription.CheckPageQuery(req.Query()); rf != nil {
			refuseLib(rf)
			return
		}
		if at, ok = target("", req.RawQuery, true); !ok {
			return
		}
		allOwned = true
	}
	up, bad, ok := send(method, at, p, true, sealed...)
	if !ok {
		return
	}
	if bad.Status != 0 {
		carry(up, bad)
		return
	}
	// The answer check comes first: nothing but shapes that name no patient
	// is delivered on this leg, whatever the interaction. Then whose each
	// Subscription the answer carries is: a read's is exactly one, the
	// subscriber's; a create's, an update's, a delete's or a $status's every
	// one is the subscriber's, or the answer is the network's 404 (a create's
	// or an update's at most one); a search's or a page's every one, or 403.
	subs, rf := subscription.AnswerSubscriptions(up.raw, req.Interaction, req.ID, topic.NotificationShape())
	if rf != nil {
		refuseLib(rf)
		return
	}
	createsOrUpdates := req.Interaction == shnsdk.SubscriptionCreate || req.Interaction == shnsdk.SubscriptionUpdate
	deletesOrStatus := req.Interaction == shnsdk.SubscriptionDelete || req.Interaction == shnsdk.SubscriptionStatus
	switch {
	case checkOwn && !sub.ownsOne(subs, ingress, subscriber, topic):
		g.refuseSubscriptionNotFound(w, r, env, tok, answerTok, leg)
		return
	case createsOrUpdates && len(subs) > 0 && !sub.ownsOne(subs, ingress, subscriber, topic):
		// A create's or an update's answer that carries a Subscription
		// carries the subscriber's own, or it is the network's 404.
		g.refuseSubscriptionNotFound(w, r, env, tok, answerTok, leg)
		return
	case deletesOrStatus && !sub.ownsAll(subs, ingress, subscriber, topic):
		// A delete's or a $status's answer that carries a Subscription
		// carries only the subscriber's own, or it is the network's 404.
		g.refuseSubscriptionNotFound(w, r, env, tok, answerTok, leg)
		return
	case allOwned:
		for _, s := range subs {
			if !sub.keys.Owns(s, ingress, subscriber, topic) {
				refuse(RefusalAuthority, http.StatusForbidden, sr.refusals.searchNotOwned)
				return
			}
		}
	}
	address := map[string]string{shnsdk.FrameHeaderStatedBase: sub.base.String()}
	if createsOrUpdates {
		if tail, ok := cdexSubscriptionLocation(up, sub.base.String()); ok {
			address[shnsdk.FrameHeaderSubscriptionLocation] = tail
		} else if noLocation(up) {
			exchangeOf(ctx).editNotMade(EditNotMadeSubscriptionLocationNoLocation)
		} else {
			exchangeOf(ctx).editNotMade(EditNotMadeSubscriptionLocationNotUnderBase)
		}
	}
	result := LegResult{Response: relay.Exact(up.body, up.contentType), AnswerStatus: up.status, AnswerHeaders: up.headers,
		AddressHeaders: address}
	g.respondLeg(w, r, leg.frame, leg.op, leg.tx, env.Metadata.CorrelationID,
		result.Response, result.stated(), "", subscriber, "", answerTok)
}

// refuseSubscriptionNotFound writes the network's 404
// (cdexSubscriptionNotFound) on leg for a Subscription that is not the
// requesting subscriber's, recorded as the leg's authority and as this
// gateway's own refusal, under no patient.
func (g *Gateway) refuseSubscriptionNotFound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, answerTok string, leg inboundLeg) {
	exchangeOf(r.Context()).refusing(RefusalAuthority)
	p, err := relay.Authored(relay.BuilderGatewayRefusal, cdexSubscriptionNotFound, "application/fhir+json")
	if err != nil {
		writeOwnershipFault(w)
		return
	}
	g.respondLegError(w, r, leg.frame, leg.op, leg.tx, env.Metadata.CorrelationID,
		LegResult{Status: http.StatusNotFound, Message: cdexSubscriptionNotFoundMessage, Response: p}, "", env.Metadata.Sender, "", answerTok)
}

// subscriptionRekeyed is a subscriber's Subscription create or update as this
// gateway forwards it to its own system: read (subscription.ReadSubscription),
// held to the closed shape with its topic's filter for the subscriber
// (subscription.CheckShape, holder), and its channel re-keyed by E-19 to this
// gateway's notification address for the subscriber and the sealed endpoint
// its gateway framed, with the gateway's credential for that address and the
// subscriber's sealed headers (subscription.RekeyForNotifier decides whether
// the edit can be made; the change is made and verified by relay). A refusal
// names its status and primitive; why E-19 was not made is recorded.
func (g *Gateway) subscriptionRekeyed(ctx context.Context, sub *cdexSubscriptionState, body []byte, frame subscribeFrame, subscriber string, holder subscription.Holder, topic subscription.Topic) (relay.Payload, *subscription.Refusal, bool) {
	x := exchangeOf(ctx)
	ct := requestMediaType(ctx)
	// The body is read by its structure alone (subscription.ClassifyBody),
	// never its label: another resource is the leg's authority, a body this
	// gateway cannot read the fidelity of its own edit.
	s, rf := subscription.ReadSubscription(body)
	switch {
	case rf.Unreadable():
		x.editNotMade(EditNotMadeSubscriptionUnreadable)
		return noSubscriptionForward, rf, true
	case rf != nil:
		return noSubscriptionForward, rf, false
	}
	if rf := subscription.CheckShape(s, topic, holder); rf != nil {
		return noSubscriptionForward, rf, false
	}
	// The leg names no patient on either side: the Subscription holds no
	// resource (a network rule).
	if rf := subscription.CheckNoResource(s); rf != nil {
		return noSubscriptionForward, rf, false
	}
	notSealed := func(why, reason string) (relay.Payload, *subscription.Refusal, bool) {
		x.editNotMade(why)
		return noSubscriptionForward, &subscription.Refusal{Status: http.StatusForbidden, Primitive: vocab.RefusalAuthority, Reason: reason}, false
	}
	switch {
	case !frame.hasEndpoint || frame.endpoint == "" || strings.Contains(frame.endpoint, " "):
		return notSealed(EditNotMadeSubscriptionNoSealedEndpoint, refusalCDexSubscribeNoSealedEndpoint)
	case !frame.hasChannel || frame.channel == "":
		return notSealed(EditNotMadeSubscriptionNoSealedHeaders, refusalCDexSubscribeNoSealedHeaders)
	}
	// The channel is read first, structurally: the subscriber's gateway seals
	// it (E-18), so channel.endpoint may be absent, and E-19 then writes it;
	// one that is present is a string, overwritten. A channel member a lenient
	// reader would take for endpoint or header (another case of the name)
	// cannot be re-keyed one way only.
	rb := relay.NewBody(body, relay.OriginPeerFrame)
	unreadable := func() (relay.Payload, *subscription.Refusal, bool) {
		x.editNotMade(EditNotMadeSubscriptionUnreadable)
		return noSubscriptionForward, &subscription.Refusal{Status: http.StatusBadRequest,
			Reason: "the Subscription is not one JSON object this network can re-key unambiguously"}, true
	}
	doc, err := relay.Doc(rb)
	if err != nil {
		return unreadable()
	}
	channel, ok := doc.Member(doc.Root(), "channel")
	if !ok || doc.Kind(channel) != relay.KindObject {
		return unreadable()
	}
	for _, m := range doc.Members(channel) {
		f := jsonmember.FoldedName(m.Name)
		if (f == jsonmember.FoldedName("endpoint") && m.Name != "endpoint") || (f == jsonmember.FoldedName("header") && m.Name != "header") {
			return unreadable()
		}
	}
	ep, hasEndpoint := doc.Member(channel, "endpoint")
	if hasEndpoint && doc.Kind(ep) != relay.KindString {
		return unreadable()
	}
	addr := subscription.Address{Ingress: g.cfg.IngressBaseURL, Topic: topic.Token(), Subscriber: subscriber, SealedEndpoint: frame.endpoint}
	if _, err := subscription.RekeyForNotifier(sub.keys, addr, frame.channel, s); err != nil {
		return notSealed(EditNotMadeSubscriptionNotSealedValues, refusalCDexSubscribeNotSealed)
	}
	headers, err := jsonValue([]string{
		subscription.AuthorizationHeader + ": " + sub.keys.Credential(addr, frame.channel),
		subscription.ChannelHeader + ": " + frame.channel,
	})
	if err != nil {
		return notSealed(EditNotMadeSubscriptionNotSealedValues, refusalCDexSubscribeNotSealed)
	}
	endpoint, err := jsonValue(addr.String())
	if err != nil {
		return notSealed(EditNotMadeSubscriptionNotSealedValues, refusalCDexSubscribeNotSealed)
	}
	var ops []relay.Op
	if hasEndpoint {
		ops = append(ops, doc.Replace(ep, endpoint))
	} else {
		ops = append(ops, doc.InsertMember(channel, "endpoint", endpoint))
	}
	h, hasHeader := doc.Member(channel, "header")
	if hasHeader {
		ops = append(ops, doc.Replace(h, headers))
	} else {
		ops = append(ops, doc.InsertMember(channel, "header", headers))
	}
	p, err := relay.Apply(rb, ct, relay.EditSubscriptionEndpointRekey, ops...)
	if err != nil {
		return unreadable()
	}
	return p, nil, false
}

// searchRekeyed is a subscriber's Subscription search on topic as this
// gateway forwards it to its own system. The search as framed names no url:
// the endpoint travels only sealed, in subscriptionEndpoint (two values during
// the subscriber's key overlap, space-separated). E-21 writes url first, the
// notification address E-19 would write for the subscriber and each sealed
// value (subscription.RekeySearchURL), before every other parameter byte for
// byte as framed, and relay verifies it (relay.QueryEdited). The search with
// its url is then held to the parameters it may carry (subscription.CheckSearch:
// an _elements that leaves out channel among them).
func (g *Gateway) searchRekeyed(ctx context.Context, empty relay.Payload, req shnsdk.SubscriptionRequest, frame subscribeFrame, subscriber string, topic subscription.Topic) (relay.Payload, *subscription.Refusal) {
	x := exchangeOf(ctx)
	sealed := strings.Split(frame.endpoint, " ")
	if !frame.hasEndpoint || frame.endpoint == "" || len(sealed) > 2 {
		x.editNotMade(EditNotMadeSearchNoSealedEndpoint)
		return noSubscriptionForward, &subscription.Refusal{Status: http.StatusForbidden, Primitive: vocab.RefusalAuthority, Reason: refusalCDexSubscribeNoSealedEndpoint}
	}
	b := subscription.Binding{Subscriber: subscriber, Notifier: g.cfg.HolderID, Topic: topic.Token()}
	q, rf := subscription.RekeySearchURL(g.cfg.IngressBaseURL, b, sealed, req.Query())
	if rf != nil {
		x.editNotMade(EditNotMadeSearchNotSealedValue)
		return noSubscriptionForward, rf
	}
	if rf := subscription.CheckSearch(q); rf != nil {
		return noSubscriptionForward, rf
	}
	to := "url=" + url.QueryEscape(q.Get("url"))
	if req.RawQuery != "" {
		to += "&" + req.RawQuery
	}
	p, err := relay.QueryEdited(empty, relay.EditSubscriptionURLSearchRekey, req.RawQuery, to, relay.QueryInputs{
		relay.QueryInputIngress: g.cfg.IngressBaseURL, relay.QueryInputTopic: b.Topic, relay.QueryInputSubscriber: subscriber,
	})
	if err != nil {
		x.editNotMade(EditNotMadeSearchNotSealedValue)
		return noSubscriptionForward, &subscription.Refusal{Status: http.StatusForbidden, Primitive: vocab.RefusalAuthority, Reason: refusalCDexSubscribeNotSealed}
	}
	return p, nil
}
