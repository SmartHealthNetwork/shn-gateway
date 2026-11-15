//go:build !shn_coreonly

package engine

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/pa/subscription"
	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The subscriber's half of a topic Subscription (the notifications design §6,
// §7.2, §9): the subscriber's own system creates, reads, updates, deletes and
// searches its Subscription at its gateway's per-counterpart base for that
// counterpart, as it would at the counterpart's own base, and asks its
// $status; the gateway carries each interaction on the topic's subscribe leg,
// through the Hub, to the counterpart's gateway, which serves the notifier's
// half (workstream_pa_cdex_subscription.go for the HRex Task topic).
//
// serveSubscriptions is the half, generic over the topic. The base that mounts
// it (a workstream's per-counterpart base: workstream_pa_cdex_subscriber.go
// for CDex) admits the caller, names the counterpart and builds the base; the
// handler owns everything below it, every rule a network rule at every
// conformance level:
//   - a PATCH is refused 405, recorded as the fidelity of SHN's own edit: a
//     patch could rewrite the channel in a form the gateway cannot seal;
//   - an instance _history and a type-level $status are refused 403 on
//     authority: a past version may name another endpoint, and a type-level
//     $status answers for other subscribers;
//   - any other interaction outside the subscribe leg's closed set
//     (shnsdk.ParseSubscriptionRequest), an id or version that is not a FHIR
//     id among them, is refused 400 on routing, nothing sent;
//   - a search is read with shnsdk.ParseSubscriptionSearch: one naming more
//     than one endpoint is refused 400 on routing, as only one sealed
//     endpoint can be carried; any other query outside its set 403 on
//     authority (§7.2); its endpoint is sealed (both seals during a key
//     overlap) and its query framed without url, so the clear endpoint never
//     leaves this gateway;
//   - a page (Subscription?_getpages=…, the link E-20 writes) is framed in its
//     page form, "GET ?_getpages=…", its query as sent; one outside the page
//     set is refused 403 on authority;
//   - a create's or update's body is judged by its structure, never its
//     label, by the classifier the notifier's gateway shares (another
//     resource, or a Subscription holding one, 403 on authority; one it
//     cannot read 415 or 400 on the fidelity of SHN's own edit), its
//     channel.endpoint held to the delivery rule
//     (subscription.CheckEndpoint, §6.5: 422 on integrity or routing), each
//     channel.header value to an HTTP field the delivery can present
//     (subscription.HeaderFields: 422, the fidelity of SHN's own edit), and its
//     channel sealed (E-18): channel.endpoint and channel.header removed from
//     the body and framed sealed;
//   - only a create and an update carry a body (400 on integrity otherwise);
//   - an update's or delete's If-Match is framed as sent; the network carries
//     no Prefer on any leg.
//
// The notifier's 2xx answer is restored (E-20): each Subscription in it whose
// address ends in a notification address for this subscriber and topic, and
// whose seals open for the notifier that answered, reads back as the
// subscriber wrote it, and each fullUrl and paging link under the notifier's
// stated base is re-keyed to the per-counterpart base. A create's or update's
// 2xx gains a Location at that base when the notifier's gateway framed the
// Subscription's location (E-23). Every other answer, the notifier's own
// refusals and the Hub's among them, is carried as on every leg.

// The subscriber's own refusals of its system's Subscription interaction.
const (
	// errSubscriberPatch: a PATCH could rewrite the channel in a form the
	// gateway cannot seal (§7.2).
	errSubscriberPatch = "a Subscription is not patched through this network: its channel is sealed before it is sent, which a patch cannot be (fidelity of SHN's own edit); send the whole Subscription with PUT [base]/Subscription/[id]"
	// errSubscriberHistory: an instance _history (§7.2).
	errSubscriberHistory = "a Subscription's history is not read through this network: a past version may name another subscriber's endpoint (per-leg authority); read the Subscription with GET [base]/Subscription/[id]"
	// errSubscriberTypeStatus: a type-level $status (§7.2).
	errSubscriberTypeStatus = "a type-level $status is not carried: it answers for other subscribers' Subscriptions (per-leg authority); ask one Subscription's with GET [base]/Subscription/[id]/$status"
	// errSubscriberInteraction opens the refusal of a request outside the
	// subscribe leg's closed set.
	errSubscriberInteraction = "the request is not a Subscription interaction this network carries (routing); nothing was sent: "
	// errSubscriberSearch opens the refusal of a search or page query
	// outside its set.
	errSubscriberSearch = "the Subscription search is not one this network carries (per-leg authority): it carries exactly one url, naming the subscriber's own notification endpoint, and only status, _id, _count and _elements; a page carries what its link names; nothing was sent: "
	// errSubscriberOneEndpoint: a search naming more than one endpoint.
	errSubscriberOneEndpoint = "the Subscription search names more than one endpoint in its url (routing): only one sealed endpoint can be carried for a search; search for each endpoint on its own; nothing was sent"
	// errSubscriberBody: only a create and an update carry a body.
	errSubscriberBody = "a Subscription read, vread, delete, $status, search or page carries no body; nothing was sent"
	// errSubscriberIfMatch: an If-Match that a frame cannot carry.
	errSubscriberIfMatch = "the If-Match cannot be carried as an HTTP field value; nothing was sent"
	// errSubscriberSeal opens the refusal of a Subscription the gateway
	// cannot seal (E-18).
	errSubscriberSeal = "the Subscription's channel cannot be sealed (fidelity of SHN's own edit), so it is not sent: "
	// errSubscriberRestore: the gateway's own restore of the answer (E-20)
	// or its Location (E-23) failed its verification.
	errSubscriberRestore = "the gateway cannot write the notifier's answer back as the Subscription its system wrote (fidelity of SHN's own edit); the notifier answered: read the Subscription before sending the request again"
	// errSubscriberHeaderField: a channel.header value the gateway could never
	// present as an HTTP field. The value is never named.
	errSubscriberHeaderField = "a channel.header value is not an HTTP field (name: value) this network could present to your notification endpoint; the value is not named"
	// errSubscriberNoKeys: the gateway has no subscription keys.
	errSubscriberNoKeys = "this gateway holds no subscription keys, so it cannot seal a Subscription's channel; nothing was sent"
)

// restoreNotMade are E-20's not-made reasons as the exchange records them,
// by the library's reason (subscription.NotMade).
var restoreNotMade = map[string]string{
	subscription.NotMadeNoEndpoint:      EditNotMadeRestoreNoEndpoint,
	subscription.NotMadeForeignAddress:  EditNotMadeRestoreNotAnAddress,
	subscription.NotMadeAnotherBinding:  EditNotMadeRestoreAnotherBinding,
	subscription.NotMadeNoSealedHeaders: EditNotMadeRestoreNoSealedHeaders,
	subscription.NotMadeKeyRetired:      EditNotMadeRestoreKeyRetired,
	subscription.NotMadeSealInvalid:     EditNotMadeRestoreSealInvalid,
	subscription.NotMadeNoStatedBase:    EditNotMadeRestoreNoStatedBase,
	subscription.NotMadeUnreadable:      EditNotMadeRestoreUnreadableBundle,
}

// subscribeRequest is what a subscribe leg's request frame carries beside the
// body (Content.SubscribeFrame, its headers): the interaction (subscriptionRequest), the sealed
// endpoint (subscriptionEndpoint, two values space-separated for a search
// during a key overlap) and sealed headers (subscriptionChannel) when the
// interaction carries them, the subscriber's If-Match, and the body's media
// type.
type subscribeRequest struct {
	request, endpoint, channel, ifMatch, contentType string
}

// frameHeaders are the request-frame headers s carries (Content.SubscribeFrame):
// the interaction, and each other value it has.
func (s *subscribeRequest) frameHeaders() map[string]string {
	headers := map[string]string{shnsdk.FrameHeaderSubscriptionRequest: s.request}
	for name, v := range map[string]string{shnsdk.FrameHeaderSubscriptionEndpoint: s.endpoint, shnsdk.FrameHeaderSubscriptionChannel: s.channel,
		shnsdk.FrameHeaderIfMatch: s.ifMatch, "Content-Type": s.contentType} {
		if v != "" {
			headers[name] = v
		}
	}
	return headers
}

// subscriptionTopics are the topics a gateway serves a subscribe leg for: the
// topic tokens a notification address may name (§6.5's loop rule).
var subscriptionTopics = []subscription.Topic{cdexTopic}

// isSubscriptionTopic reports whether tok is a served topic's token.
func isSubscriptionTopic(tok string) bool {
	return slices.ContainsFunc(subscriptionTopics, func(t subscription.Topic) bool { return t.Token() == tok })
}

// subscriberKeys are the keys this gateway seals and restores its own
// participant's Subscriptions with: derived from its identity, under its
// holder, and the predecessor a rotation kept (§6.2 "Key rotation"). ok is
// false when the identity names another holder or yields no keys.
func (g *Gateway) subscriberKeys() (subscription.Keys, bool) {
	st := g.paState()
	if st == nil || st.subscriberKeys.KeyID() == "" {
		return subscription.Keys{}, false
	}
	return st.subscriberKeys, true
}

// newSubscriberKeys derives a gateway's subscriber keys (subscriberKeys), or
// zero Keys when its identity names another holder or yields none: a gateway
// that cannot seal refuses each create, update and search, never sends one
// unsealed.
func newSubscriberKeys(cfg Config) subscription.Keys {
	if cfg.Identity.HolderID == "" || cfg.Identity.HolderID != cfg.HolderID || len(cfg.Identity.SignPriv) == 0 {
		return subscription.Keys{}
	}
	k, err := subscription.DeriveKeys(cfg.Identity.HolderID, cfg.Identity.SignPriv, cfg.SubscriptionPredecessorKey)
	if err != nil {
		return subscription.Keys{}
	}
	return k
}

// serveSubscriptions serves the subscriber's system's Subscription
// interaction at its per-counterpart base for topic t: counterpart is the
// notifier's holder, base the absolute per-counterpart base its system
// addresses, and rest the path below that base: "Subscription" (a create, a
// search, or a page when its query carries _getpages),
// "Subscription/<id>", "Subscription/<id>/_history/<vid>" or
// "Subscription/<id>/$status". The base has admitted the caller; everything
// else is this handler's (the rules above).
func (g *Gateway) serveSubscriptions(w http.ResponseWriter, r *http.Request, t subscription.Topic, counterpart, base, rest string) {
	leg := t.SubscribeLeg()
	exchangeOf(r.Context()).leg(leg)
	w, scope := g.withScope(w, relay.RoleRequester)
	w = &fhirOperationWriter{w}
	refuse := func(rule string, status int, msg string, reason refusalReason) {
		exchangeOf(r.Context()).refusingFor(rule, reason)
		writeJSON(w, status, map[string]string{"error": msg})
	}
	refuseFidelity := func(status int, msg, notMade string) {
		if notMade != "" {
			exchangeOf(r.Context()).editNotMade(notMade)
		}
		exchangeOf(r.Context()).refusingFidelity(vocab.RefusalStageBuild)
		writeJSON(w, status, map[string]string{"error": msg})
	}
	if r.Method == http.MethodPatch {
		refuseFidelity(http.StatusMethodNotAllowed, errSubscriberPatch, "")
		return
	}
	segs := strings.Split(rest, "/")
	switch {
	case len(segs) == 3 && segs[0] == "Subscription" && segs[2] == "_history":
		refuse(RefusalAuthority, http.StatusForbidden, errSubscriberHistory, reasonSubscriberHistory)
		return
	case len(segs) == 2 && segs[0] == "Subscription" && segs[1] == "$status":
		refuse(RefusalAuthority, http.StatusForbidden, errSubscriberTypeStatus, reasonSubscriberTypeStatus)
		return
	}
	body, ok := readIngressBody(w, r, shnsdk.MaxRequestBytes)
	if !ok {
		return
	}
	keys, haveKeys := g.subscriberKeys()
	binding := subscription.Binding{Subscriber: g.cfg.HolderID, Notifier: counterpart, Topic: t.Token()}
	method := r.Method
	if method == http.MethodHead {
		// A HEAD is served as the GET it names, without the answer's body.
		method = http.MethodGet
	}
	frame := &subscribeRequest{}
	rawQuery, hasQuery := r.URL.RawQuery, r.URL.RawQuery != "" || r.URL.ForceQuery
	var req shnsdk.SubscriptionRequest
	switch {
	case method == http.MethodGet && rest == "Subscription" && pageQuery(rawQuery):
		// A page, at the link E-20 wrote: framed in its page form.
		var err error
		req, err = shnsdk.ParseSubscriptionRequest("GET ?" + rawQuery)
		if err != nil {
			if errors.Is(err, shnsdk.ErrSubscriptionRequestQuery) {
				refuse(RefusalAuthority, http.StatusForbidden, errSubscriberSearch+err.Error(), reasonSubscriberSearch)
				return
			}
			refuse(RefusalRouting, http.StatusBadRequest, errSubscriberInteraction+err.Error(), reasonSubscriberInteraction)
			return
		}
	case method == http.MethodGet && rest == "Subscription":
		q, err := shnsdk.ParseSubscriptionSearch(rawQuery)
		switch {
		case errors.Is(err, shnsdk.ErrSubscriptionSearchOneEndpoint):
			refuse(RefusalRouting, http.StatusBadRequest, errSubscriberOneEndpoint, reasonSubscriberOneEndpoint)
			return
		case err != nil:
			refuse(RefusalAuthority, http.StatusForbidden, errSubscriberSearch+err.Error(), reasonSubscriberSearch)
			return
		}
		// The loop rule a create's channel.endpoint is held to (§6.5): a
		// notification address is never a payer system's own endpoint, so a
		// search by one would find nothing; it is refused, never carried as
		// a search that silently answers empty.
		if subscription.LooksLikeNotificationAddress(q.Endpoint, func(id string) bool { _, ok := g.cfg.Reg.Lookup(id); return ok }, isSubscriptionTopic) {
			refuse(RefusalRouting, http.StatusUnprocessableEntity, string(reasonSubscriberSearchLoop), reasonSubscriberSearchLoop)
			return
		}
		if !haveKeys {
			g.subscriberOwnFailure(w, r)
			return
		}
		// The endpoint travels only sealed: under the current key, and the
		// predecessor's during its overlap, so a Subscription made under
		// either is found (§6.2 step 4).
		frame.endpoint = strings.Join(keys.SealEndpointForSearch(binding, q.Endpoint), " ")
		req = q.Request
	default:
		target := method + " " + rest
		if hasQuery {
			target += "?" + rawQuery
		}
		var err error
		req, err = shnsdk.ParseSubscriptionRequest(target)
		if err != nil || req.Interaction == shnsdk.SubscriptionSearch || req.Interaction == shnsdk.SubscriptionPage {
			msg := "the method and path are not one of a Subscription"
			if err != nil {
				msg = err.Error()
			}
			refuse(RefusalRouting, http.StatusBadRequest, errSubscriberInteraction+msg, reasonSubscriberInteraction)
			return
		}
	}
	frame.request = req.String()
	writes := req.Interaction == shnsdk.SubscriptionCreate || req.Interaction == shnsdk.SubscriptionUpdate
	if !writes && len(body) != 0 {
		refuse(RefusalIntegrity, http.StatusBadRequest, errSubscriberBody, reasonSubscriberBody)
		return
	}
	// An update's or a delete's If-Match is framed as the subscriber's system
	// sent it (several field lines read as one list, as HTTP reads them).
	if values := r.Header.Values("If-Match"); len(values) > 0 && (req.Interaction == shnsdk.SubscriptionUpdate || req.Interaction == shnsdk.SubscriptionDelete) {
		frame.ifMatch = strings.Join(values, ", ")
		if !shnsdk.FrameCarriesHeaderValue(frame.ifMatch) {
			refuse(RefusalIntegrity, http.StatusBadRequest, errSubscriberIfMatch, reasonSubscriberIfMatch)
			return
		}
	}
	payload := relay.Exact(relay.NewBody(nil, relay.OriginIngressRequest), "")
	if writes {
		if !haveKeys {
			g.subscriberOwnFailure(w, r)
			return
		}
		p, endpoint, channel, rf, notMade, reason := g.sealSubscription(r, keys, binding, body)
		if rf != nil {
			if notMade != "" {
				// The edit could not be made: its own fidelity.
				refuseFidelity(rf.Status, errSubscriberSeal+rf.Reason, notMade)
				return
			}
			// A network rule on its primitive; the answer is the rule's
			// fixed sentence.
			refuse(rf.Primitive, rf.Status, string(reason), reason)
			return
		}
		payload, frame.endpoint, frame.channel = p, endpoint, channel
		frame.contentType = carriedFHIRMediaType(r.Header.Get("Content-Type"))
	}
	// A module ingress client builds no Subscription interaction: no builder
	// builds this leg's request, so it is refused here.
	if _, ok := g.ingressBuilder(w, r, leg); !ok {
		return
	}
	r = r.WithContext(withFindingContext(r.Context(), findingContext{LegType: leg, Seam: "subscriber-ingress", Whose: "own"}))
	scope.leg = leg
	ex := g.exchanges.Begin(workstreamPA)
	child := g.ingressCorrelation(w, r)
	content := Content{WorkstreamType: workstreamPA, Payload: payload, Carried: true, SubscribeFrame: frame.frameHeaders()}
	var stated statedAnswer
	var answerType string
	ctx := withStatedAnswerSink(withAnswerMediaTypeSink(r.Context(), &answerType), &stated)
	// The leg names no patient on either side (its row's subject: none).
	answer, err := g.OriginateLeg(ctx, r, counterpart, leg, "", child, "", content)
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
	answerCtx := withFindingContext(r.Context(), findingContext{LegType: leg, CorrelationID: child, Seam: "subscriber-ingress", Whose: "peer"})
	if repeatsAMember(answerCtx, answer) {
		g.recordLeg(ex.ID, legProj.Project(child, "error"))
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the notifier's answer can be read two ways: it repeats a member name"})
		return
	}
	ct := writableAnswerMediaType(answerType)
	p, err := g.restoredAnswer(r, answer, ct, keys, binding, stated, base, writes)
	if err != nil {
		g.recordLeg(ex.ID, legProj.Project(child, "error"))
		exchangeOf(r.Context()).refusingFidelity(vocab.RefusalStageVerify)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": errSubscriberRestore})
		return
	}
	g.recordLeg(ex.ID, legProj.Project(child, "answered"))
	g.writeRelayedAnswer(w, stated, ct, p,
		relay.Key{Leg: leg, Role: relay.RoleRequester, Direction: relay.DirectionResponse, Outcome: relay.OutcomeAnswered})
}

// pageQuery reports whether raw, a query as sent, has a parameter whose raw
// name is exactly _getpages: the page a search's next or previous link names
// (E-20 re-keys it to the per-counterpart base's Subscription), as a HAPI FHIR
// server writes it. The name is compared as sent, never decoded and never as
// a substring of the query: a name escaped any other way (%5Fgetpages) is no
// page, falls through to the search, and is refused there as a search
// parameter outside its set (403, authority).
func pageQuery(raw string) bool {
	for _, part := range strings.Split(raw, "&") {
		name, _, _ := strings.Cut(part, "=")
		if name == "_getpages" {
			return true
		}
	}
	return false
}

// subscriberOwnFailure answers a create, update or search this gateway cannot
// seal for want of keys: its own failure, never a refusal, and nothing sent.
func (g *Gateway) subscriberOwnFailure(w http.ResponseWriter, r *http.Request) {
	exchangeOf(r.Context()).ownFailure()
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": errSubscriberNoKeys})
}

// sealSubscription is the subscriber's create or update as this gateway sends
// it. Its body is judged by its structure alone, never the media type it is
// labelled with, by the one classifier every check on a subscribe leg shares
// (subscription.ReadSubscription, over subscription.ClassifyBody), as the
// notifier's gateway judges it:
//   - FHIR JSON or FHIR XML of another resource type is refused 403, on
//     authority: the leg carries no other resource;
//   - FHIR XML, a body that is not JSON, and one that cannot be read one way
//     only are refused 415 or 400, the fidelity of SHN's own edit, which
//     cannot be made on a body it cannot read;
//   - a Subscription that holds a resource (a contained member, or any
//     object with a resourceType below it) is refused 403, on authority: the
//     leg names no patient on either side (subscription.CheckNoResource).
//
// Its channel.endpoint is then held to the delivery rule
// (subscription.CheckEndpoint, §6.5), and its channel sealed (E-18): the
// library seals the endpoint and the headers and decides the edit can be made
// (subscription.SealOutbound), and relay removes the two members and verifies
// the change, made even on signed content. A refusal with notMade set is the
// fidelity of SHN's own edit, notMade naming why E-18 was not made; one
// without is a network rule on the primitive it names, reason its fixed
// sentence.
func (g *Gateway) sealSubscription(r *http.Request, keys subscription.Keys, b subscription.Binding, body []byte) (p relay.Payload, endpoint, channel string, rf *subscription.Refusal, notMade string, reason refusalReason) {
	s, rf := subscription.ReadSubscription(body)
	switch {
	case rf != nil && rf.Primitive == RefusalAuthority:
		return noSubscriberPayload, "", "", rf, "", reasonSubscriberNotASubscription
	case rf != nil:
		return noSubscriberPayload, "", "", rf, EditNotMadeSealUnreadable, ""
	}
	if rf := subscription.CheckNoResource(s); rf != nil {
		return noSubscriberPayload, "", "", rf, "", reasonSubscriberHoldsResource
	}
	ep, _ := s.Endpoint()
	loop := func(raw string) bool {
		return subscription.LooksLikeNotificationAddress(raw, func(id string) bool { _, ok := g.cfg.Reg.Lookup(id); return ok }, isSubscriptionTopic)
	}
	if rf := subscription.CheckEndpoint(ep, loop); rf != nil {
		// Integrity, or routing for a loop.
		if rf.Primitive == RefusalRouting {
			return noSubscriberPayload, "", "", rf, "", reasonSubscriberEndpointLoop
		}
		return noSubscriberPayload, "", "", rf, "", reasonSubscriberEndpoint
	}
	// A header value the gateway could never present to the endpoint as an
	// HTTP field is refused here, before the channel is sealed, as the
	// delivery would refuse it: the fidelity of the network's own delivery
	// of the subscriber's values, never naming the value.
	if !s.PresentableHeaders() {
		return noSubscriberPayload, "", "", &subscription.Refusal{Status: http.StatusUnprocessableEntity, Reason: errSubscriberHeaderField}, EditNotMadeSealNotSealable, ""
	}
	if _, endpoint, channel, err := subscription.SealOutbound(keys, b, s); err != nil {
		return noSubscriberPayload, "", "", &subscription.Refusal{Status: http.StatusBadRequest, Reason: err.Error()}, EditNotMadeSealNotSealable, ""
	} else {
		rb := relay.NewBody(body, relay.OriginIngressRequest)
		unreadable := &subscription.Refusal{Status: http.StatusBadRequest, Reason: "the Subscription is not one JSON object this network can seal unambiguously"}
		doc, err := relay.Doc(rb)
		if err != nil {
			return noSubscriberPayload, "", "", unreadable, EditNotMadeSealUnreadable, ""
		}
		ch, ok := doc.Member(doc.Root(), "channel")
		if !ok {
			return noSubscriberPayload, "", "", unreadable, EditNotMadeSealUnreadable, ""
		}
		ops := []relay.Op{doc.RemoveMember(ch, "endpoint")}
		if _, ok := doc.Member(ch, "header"); ok {
			ops = append(ops, doc.RemoveMember(ch, "header"))
		}
		p, err := relay.Apply(rb, carriedFHIRMediaType(r.Header.Get("Content-Type")), relay.EditSubscriptionHeaderSeal, ops...)
		if err != nil {
			return noSubscriberPayload, "", "", unreadable, EditNotMadeSealUnreadable, ""
		}
		return p, endpoint, channel, nil, "", ""
	}
}

// restoredAnswer is the notifier's 2xx answer (every answer OriginateLeg
// hands back; any other is its error) as this gateway writes it to its own
// system, with media type ct: when it has a body, each Subscription in it
// restored and each
// address under the notifier's stated base re-keyed to base (E-20: the
// library decides and names each change, subscription.RestoreEdits; relay
// makes and verifies them, even on signed content); and, on a create's or an
// update's 2xx, a Location at base for the Subscription location the
// notifier's gateway framed (E-23). Why an edit was not made is recorded. err
// is the edit failing its own verification: nothing is written.
func (g *Gateway) restoredAnswer(r *http.Request, raw []byte, ct string, keys subscription.Keys, b subscription.Binding, stated statedAnswer, base string, writes bool) (relay.Payload, error) {
	x := exchangeOf(r.Context())
	p := relay.Exact(relay.NewBody(raw, relay.OriginPeerFrame), ct)
	if len(raw) != 0 {
		edits, _, notMade, err := subscription.RestoreEdits(keys, b, stated.address[shnsdk.FrameHeaderStatedBase], base, raw)
		switch {
		case err != nil:
			x.editNotMade(EditNotMadeRestoreUnreadableAnswer)
		default:
			for _, nm := range notMade {
				if why, ok := restoreNotMade[nm.Reason]; ok {
					x.editNotMade(why)
				}
			}
			if len(edits) > 0 {
				restored, err := applyRestore(raw, ct, edits)
				if err != nil {
					return noSubscriberPayload, err
				}
				p = restored
			}
		}
	}
	if !writes {
		return p, nil
	}
	tail := stated.address[shnsdk.FrameHeaderSubscriptionLocation]
	if tail == "" {
		x.editNotMade(EditNotMadeLocationNoTail)
		return p, nil
	}
	if _, _, err := shnsdk.ParseSubscriptionLocation(tail); err != nil {
		x.editNotMade(EditNotMadeLocationUnreadableTail)
		return p, nil
	}
	return headerEdited(p, relay.EditSubscriptionLocationRekey, "Location", subscriptionLocationValue(base, tail),
		relay.HeaderInputs{relay.SubscriptionLocationInputBase: base, relay.SubscriptionLocationInputTail: tail})
}

// subscriptionLocationValue is the Location E-23 writes: the per-counterpart
// base, then the Subscription location. The engine's tests replace it to make
// a value the core's check of the edit refuses.
var subscriptionLocationValue = func(base, tail string) string { return base + "/" + tail }

// applyRestore makes E-20's changes, named by the byte span of the value each
// changes (subscription.RestoreEdit), through relay over raw, the answer as
// relayed: a replace of that value, or the removal of the member whose value
// it is.
func applyRestore(raw []byte, contentType string, edits []subscription.RestoreEdit) (relay.Payload, error) {
	rb := relay.NewBody(raw, relay.OriginPeerFrame)
	doc, err := relay.Doc(rb)
	if err != nil {
		return noSubscriberPayload, err
	}
	type place struct {
		node   relay.NodeID
		parent relay.NodeID
		key    string
		member bool
	}
	at := map[[2]int]place{}
	for i := 0; i < doc.Len(); i++ {
		n := relay.NodeID(i)
		s, e := doc.Span(n)
		if _, seen := at[[2]int{s, e}]; !seen {
			at[[2]int{s, e}] = place{node: n}
		}
		if doc.Kind(n) == relay.KindObject {
			for _, m := range doc.Members(n) {
				ms, me := doc.Span(m.Value)
				at[[2]int{ms, me}] = place{node: m.Value, parent: n, key: m.Name, member: true}
			}
		}
	}
	var ops []relay.Op
	for _, e := range edits {
		pl, ok := at[[2]int{e.Start, e.End}]
		switch {
		case !ok, e.Remove && !pl.member:
			return noSubscriberPayload, errors.New("engine: a restore names a value the answer does not hold")
		case e.Remove:
			ops = append(ops, doc.RemoveMember(pl.parent, pl.key))
		default:
			ops = append(ops, doc.Replace(pl.node, e.Value))
		}
	}
	return relay.Apply(rb, contentType, relay.EditSubscriptionEndpointRestore, ops...)
}

// noSubscriberPayload is the payload a refused or failed subscriber
// interaction carries: none.
var noSubscriberPayload relay.Payload
