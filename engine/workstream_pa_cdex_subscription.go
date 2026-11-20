//go:build !shn_coreonly

package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
	"github.com/SmartHealthNetwork/shn-gateway/pa/cdexroute"
	"github.com/SmartHealthNetwork/shn-gateway/pa/subscription"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The payer's topic Subscription to its CDex Tasks' changes
// (cdex-task-subscribe; the notifications design §5, §6, §7, §9): a payer's
// system creates, reads, updates, deletes and searches a Subscription on the
// HRex Task topic at the provider's own Subscription endpoint, through its own
// gateway, the Hub and the provider's gateway, and asks its $status. The leg
// names no patient on either side (subject: none).
//
// The provider's gateway, as the notifier's, reaches the Subscription endpoint
// at the FHIR base its participant's Task endpoint is under
// (Config.ProviderCDexTaskURL, cdexroute.TaskReadBase), with a token asked for
// the subscription scope its participant sets
// (Config.ProviderCDexSubscriptionScope), and only then declares the leg
// (TakenLegs). A provider that sets none answers the leg as routing, at every
// conformance level.
//
// The interaction is the request frame's subscriptionRequest
// (shnsdk.ParseSubscriptionRequest). Every rule below is a network rule, the
// same at every conformance level:
//   - a PATCH is refused 405 on the fidelity of SHN's own edit, and an
//     instance's history or the type-level $status 403 on authority
//     (subscription.CheckInteraction, §7.2, §9); an id or version that is
//     not a FHIR id, or any other interaction outside the closed set, 400 on
//     routing; a search or page query outside the set it carries 403 on
//     authority (§7.2);
//   - a create, and an update of a Subscription that does not exist, is read
//     (415 or 400 on the fidelity of SHN's own edit when it cannot be),
//     held to the closed shape with the HRex Task topic's filter (403 on
//     authority, §7.1), and forwarded with its channel re-keyed to this
//     gateway's notification address for the payer (E-19);
//   - an update, delete or instance $status of an existing Subscription is
//     forwarded only once a read of it, on the same leg with the same token,
//     shows it is the payer's (subscription.Keys.Owns); otherwise the
//     network's 404 on authority (§7.2);
//   - a read or vread is forwarded, and a 2xx delivered only when the
//     Subscription is the payer's; otherwise the network's 404;
//   - a search is forwarded with its url re-keyed (E-21), and a page as it is;
//     a 2xx delivered only when every Subscription in it is the payer's,
//     otherwise 403 on authority;
//   - every 2xx answer is first held to the answer check
//     (subscription.CheckSubscribeAnswer: 502 on authority), and framed with
//     the provider's base as statedBase for the payer's gateway (E-20).
//
// Everything the provider's system answers otherwise, its own 4xx and 5xx
// among them, is carried as sent.

var legCDexTaskSubscribeInbound = inboundLeg{"provider-disclosure", "cdex-task-subscribe-response", legCDexTaskSubscribe}

// The provider gateway's refusals on a payer's Subscription interaction.
const (
	// refusalCDexSubscribeNotTaken: a provider gateway whose participant sets
	// no subscription scope reaches no Subscription endpoint for a payer.
	refusalCDexSubscribeNotTaken = "this provider takes no payer's Subscription to its Tasks' changes: its gateway reaches no Subscription endpoint of its own system for payers"
	// refusalCDexSubscribeNoRequest: the request frame names no interaction.
	refusalCDexSubscribeNoRequest = "the request names no Subscription interaction: its frame carries no subscriptionRequest"
	// refusalCDexSubscribeRequest opens the refusal of a subscriptionRequest
	// outside the interactions the leg carries.
	refusalCDexSubscribeRequest = "the request's subscriptionRequest is not a Subscription interaction this network carries: "
	// refusalCDexSubscribeBody: only a create and an update carry a body.
	refusalCDexSubscribeBody = "a Subscription read, vread, delete, $status, search or page carries no body"
	// refusalCDexSubscribeNoSealedEndpoint: a create, update or search the
	// payer's gateway framed no sealed endpoint for (subscriptionEndpoint).
	refusalCDexSubscribeNoSealedEndpoint = "the request carries no sealed endpoint of the subscriber's own (subscriptionEndpoint), so no notification address can be written for it"
	// refusalCDexSubscribeNoSealedHeaders: a create or update the payer's
	// gateway framed no sealed headers for (subscriptionChannel).
	refusalCDexSubscribeNoSealedHeaders = "the request carries no sealed channel headers of the subscriber's own (subscriptionChannel), so no notification address can be written for it"
	// refusalCDexSubscribeNotSealed: the framed values are not a sealed
	// endpoint and headers this gateway can write an address and credential
	// for.
	refusalCDexSubscribeNotSealed = "the request's subscriptionEndpoint and subscriptionChannel are not sealed values this gateway can write a notification address and credential for"
	// refusalCDexSubscribeSearchNotOwned: a search answer, or a page of one,
	// holds a Subscription that is not the payer's.
	refusalCDexSubscribeSearchNotOwned = "the provider's answer to the search holds a Subscription that is not the requesting payer's, so it is not delivered"
	// refusalCDexSubscribeIfMatch: an If-Match on an interaction other than
	// an update or a delete, or one an HTTP field cannot carry.
	refusalCDexSubscribeIfMatch = "an If-Match is framed only on a Subscription update or delete, as an HTTP field value"
	// refusalCDexSubscribeTarget: the interaction's address below the
	// provider's base could not be built.
	refusalCDexSubscribeTarget = "the provider's gateway cannot build the Subscription's address below its system's base; nothing was sent"
)

// cdexSubscriptionNotFound is the network's answer to an interaction on a
// Subscription that is not the requesting payer's (§7.2): an OperationOutcome
// this gateway writes, the same bytes whatever is wrong, so it says nothing of
// the Subscription or of why. FHIR R4 allows a 404 for access denied.
var cdexSubscriptionNotFound = []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"not-found","diagnostics":"Subscription not found"}]}`)

// cdexSubscriptionNotFoundMessage is cdexSubscriptionNotFound's text.
const cdexSubscriptionNotFoundMessage = "Subscription not found"

// noSubscriptionForward is the payload a refused Subscription interaction
// forwards: none.
var noSubscriptionForward relay.Payload

// cdexSubscriptionState is how a provider's gateway reaches its own
// Subscription endpoint for a payer: the client authorized for the
// subscription scope, the FHIR base the endpoint is at, and the keys its
// notification addresses' credentials are made with.
type cdexSubscriptionState struct {
	client *http.Client
	base   *url.URL
	keys   subscription.Keys
}

// newCDexSubscriptionState is a provider gateway's subscription state, nil
// when its participant sets no subscription scope. The Task endpoint must be
// one a read is made below (cdexroute.TaskReadURL), whose base is the
// Subscription endpoint's; New refuses a gateway whose endpoint is not, or
// whose identity yields no keys.
func newCDexSubscriptionState(cfg Config) (*cdexSubscriptionState, error) {
	if strings.TrimSpace(cfg.ProviderCDexSubscriptionScope) == "" || cfg.Role != "provider" {
		return nil, nil
	}
	u, err := cdexroute.TaskReadURL(strings.TrimSpace(cfg.ProviderCDexTaskURL))
	if err != nil {
		return nil, fmt.Errorf("gateway: ProviderCDexSubscriptionScope (PROVIDER_CDEX_SUBSCRIPTION_SCOPE) requires ProviderCDexTaskURL (PROVIDER_CDEX_TASK_URL) to be a Task endpoint whose FHIR base serves the Subscription endpoint: %w", err)
	}
	base, err := url.Parse(cdexroute.TaskReadBase(u))
	if err != nil {
		return nil, fmt.Errorf("gateway: ProviderCDexSubscriptionScope (PROVIDER_CDEX_SUBSCRIPTION_SCOPE): the Task endpoint's base cannot be read: %w", err)
	}
	// The keys are derived under the identity's holder, as the app derives
	// them (its bundle's), and that holder must be this gateway's: a
	// credential made under another holder's label would never verify.
	if cfg.Identity.HolderID != cfg.HolderID {
		return nil, fmt.Errorf("gateway: ProviderCDexSubscriptionScope (PROVIDER_CDEX_SUBSCRIPTION_SCOPE): the identity names holder %q, and this gateway is holder %q: the notification keys are derived under one holder: Config.Identity.HolderID and Config.HolderID must name the same holder; the gateway app sets both from its registered bundle", cfg.Identity.HolderID, cfg.HolderID)
	}
	keys, err := subscription.DeriveKeys(cfg.Identity.HolderID, cfg.Identity.SignPriv, cfg.SubscriptionPredecessorKey)
	if err != nil {
		return nil, fmt.Errorf("gateway: ProviderCDexSubscriptionScope (PROVIDER_CDEX_SUBSCRIPTION_SCOPE): the notification keys cannot be derived from this gateway's identity: %w", err)
	}
	return &cdexSubscriptionState{client: cfg.ProviderCDexSubscriptionClient, base: base, keys: keys}, nil
}

// cdexSubscriptionLocation is the subscriptionLocation the notifier's gateway
// frames on its system's 2xx answer to a create or an update: the tail of the
// answer's Location under base, the provider's own FHIR base, by the match
// E-16's taskLocation is framed by (locationUnder), as "Subscription/<id>" or
// "Subscription/<id>/_history/<vid>", read by shnsdk.ParseSubscriptionLocation.
// Anything else, a Location at another host, at a sibling path that merely
// shares base's spelling, naming another resource type, carrying a query or a
// fragment, or naming an id that is not one, is not framed: the answer is
// delivered without it.
func cdexSubscriptionLocation(up upstreamReply, base string) (string, bool) {
	remainder, ok := locationUnder(up, base)
	if !ok {
		return "", false
	}
	// The remainder is "" or begins with "/" (dtraddress.Under).
	tail := strings.TrimPrefix(remainder, "/")
	if _, _, err := shnsdk.ParseSubscriptionLocation(tail); err != nil {
		return "", false
	}
	return tail, true
}

// handleCDexSubscribeInbound answers a payer's Subscription interaction the
// Hub delivered to this provider's gateway (the rules above). With no
// subscription scope it refuses the leg as routing, before reading anything.
func (g *Gateway) handleCDexSubscribeInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, body []byte, answerTok string) {
	g.handleSubscribeInbound(w, r, env, tok, body, answerTok, cdexSubscribeRecipient)
}

// cdexSubscribeRecipient is the HRex Task topic's subscribe leg on a
// provider's gateway: its Subscription endpoint is reached through the CDex
// Task responder, under the CDex Task endpoint's certificate tag, and the
// forward names the payer and the identifiers the registry attests for it
// (RequesterHeader).
var cdexSubscribeRecipient = subscribeRecipient{
	topic:   cdexTopic,
	inbound: legCDexTaskSubscribeInbound,
	state: func(st *paGatewayState) (*nativeResponder, *cdexSubscriptionState) {
		return st.cdexTask, st.cdexSubscription
	},
	endpoint: endpointcert.CDexTask,
	subscriber: func(g *Gateway, payer string) (subscription.Holder, string) {
		ids := g.attestedPayerIDs(payer)
		return payerHolder(payer, ids), requesterHeaderValue(payer, ids)
	},
	refusals: subscribeRefusals{
		notTaken:       refusalCDexSubscribeNotTaken,
		searchNotOwned: refusalCDexSubscribeSearchNotOwned,
		target:         refusalCDexSubscribeTarget,
	},
}

// headerSealDisclosure is the EditedHeader value disclosing E-18
// (subscription-header-seal), the subscriber's gateway's edit, on a forward
// that also carries this gateway's E-19 (p): signed as E-19's is, since the
// two changed the same channel, so a signature that covers it was made over
// the channel both edits changed.
func headerSealDisclosure(p relay.Payload) string {
	v := subscription.EditHeaderSeal
	if signed := p.SignedEdited(); len(signed) > 0 {
		v += `; signed="` + strings.Join(signed, ", ") + `"`
	}
	return v
}

// readsQuery reports whether v, a subscriptionRequest whose query the SDK
// refused, is a read's or a vread's: the same value with its query left out
// parses as one (shnsdk.ParseSubscriptionRequest). A search or a page with no
// query is no interaction, so it is never one.
func readsQuery(v string) bool {
	head, _, _ := strings.Cut(v, "?")
	r, err := shnsdk.ParseSubscriptionRequest(head)
	return err == nil && (r.Interaction == shnsdk.SubscriptionRead || r.Interaction == shnsdk.SubscriptionVRead)
}

// ownsOne reports whether subs, the Subscriptions a read's or a vread's 2xx
// answer carries (subscription.AnswerSubscriptions), are exactly one the payer
// owns. An answer that carries none (an OperationOutcome, an empty body) or
// more than one (a searchset) is not a read's own Subscription.
func (s *cdexSubscriptionState) ownsOne(subs []*subscription.Subscription, ingress, payer string, topic subscription.Topic) bool {
	if len(subs) != 1 {
		return false
	}
	return s.keys.Owns(subs[0], ingress, payer, topic)
}

// owns reports whether a provider's 2xx answer to the read of a Subscription
// is one Subscription the payer owns (subscription.Keys.Owns). An answer that
// is not one readable Subscription is not the payer's.
func (s *cdexSubscriptionState) owns(up upstreamReply, ingress, payer string, topic subscription.Topic) bool {
	read, rf := subscription.ReadSubscription(up.raw)
	return rf == nil && s.keys.Owns(read, ingress, payer, topic)
}

// subscriptionTarget is the URL an interaction below the provider's base goes
// to: base's path, escaped as it was given, then "/" and path (none for a
// page, which is at the base itself), and rawQuery as the query exactly as
// given. path is one the subscriptionRequest parser admitted: FHIR ids and
// the literal segments, which need no escaping.
func subscriptionTarget(base *url.URL, path, rawQuery string, hasQuery bool) (*url.URL, error) {
	t := *base
	escaped := strings.TrimSuffix(base.EscapedPath(), "/")
	if path != "" {
		escaped += "/" + path
	}
	p, err := url.PathUnescape(escaped)
	if err != nil {
		return nil, err
	}
	t.Path, t.RawPath = p, escaped
	t.RawQuery, t.ForceQuery = rawQuery, hasQuery && rawQuery == ""
	t.Fragment, t.RawFragment = "", ""
	if t.EscapedPath() != escaped {
		return nil, errors.New("the path does not read back as built")
	}
	return &t, nil
}

// jsonValue is v as JSON with no HTML escaping, so the value written reads
// back as it was made.
func jsonValue(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
