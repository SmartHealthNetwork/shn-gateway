//go:build !shn_coreonly

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
	"github.com/SmartHealthNetwork/shn-gateway/pa/pastopic"
	"github.com/SmartHealthNetwork/shn-gateway/pa/subscription"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A provider's topic Subscription to the PAS topic (pas-subscription-subscribe;
// the notifications design §5, §6, §7, §9): a provider's system creates,
// reads, updates, deletes and searches a Subscription on the PAS topic at the
// payer's own Subscription endpoint, through its own gateway, the Hub and the
// payer's gateway, and asks its $status. The leg names no patient on either
// side (subject: none).
//
// The payer's gateway, as the notifier's, reaches the Subscription endpoint at
// its system's PAS base (Config.PayerSubscriptionBaseURL), with a token asked
// for the subscription scope its participant sets
// (Config.PayerSubscriptionScope), and declares the leg from that setting
// alone (TakenLegs), as the registry, provisioning and a hosted tenant credit
// it. A payer that sets none answers the leg as routing, at every conformance
// level. A payer whose system does not serve Subscription has its own answer
// carried back as sent: the network adds no refusal for it.
//
// The notification address E-19 writes (/notify/pas/…) is served by the
// PAS topic's notification route (workstream_pa_pas_signal.go), mounted with
// this leg: it sends the topic's no-content notifications on its signal leg,
// and refuses a full-resource one until the topic's notify leg is carried.
//
// Every rule is the subscribe leg's (workstream_pa_subscribe_inbound.go,
// written out at workstream_pa_cdex_subscription.go), held to the PAS topic:
// its filter names the provider's own attested identifiers
// (pastopic.Topic.CheckFilter), and the forward names no subscriber in a
// header of the network's (the PAS topic registers none; the filter carries
// the subscriber).

var legPASSubscriptionSubscribeInbound = inboundLeg{"payer-coverage", "pas-subscribe-response", pastopic.SubscribeLeg}

// The payer gateway's refusals on a provider's Subscription interaction that
// name the roles.
const (
	// refusalPASSubscribeNotTaken: a payer gateway whose participant sets no
	// subscription scope reaches no Subscription endpoint for a provider.
	refusalPASSubscribeNotTaken = "this payer takes no provider's Subscription to the PAS topic: its gateway reaches no Subscription endpoint of its own system for providers"
	// refusalPASSubscribeSearchNotOwned: a search answer, or a page of one,
	// holds a Subscription that is not the provider's.
	refusalPASSubscribeSearchNotOwned = "the payer's answer to the search holds a Subscription that is not the requesting provider's, so it is not delivered"
	// refusalPASSubscribeTarget: the interaction's address below the payer's
	// base could not be built.
	refusalPASSubscribeTarget = "the payer's gateway cannot build the Subscription's address below its system's base; nothing was sent"
)

// pasTopic is the PAS topic a provider subscribes to at a payer.
var pasTopic subscription.Topic = pastopic.Topic{}

// pasSubscribeRecipient is the PAS topic's subscribe leg on a payer's
// gateway: its Subscription endpoint is reached through the payer's
// Subscription responder, under the PAS endpoint's certificate tag, and the
// provider is held by the identifiers the registry attests for it, with no
// header of the network's on the forward.
var pasSubscribeRecipient = subscribeRecipient{
	topic:   pasTopic,
	inbound: legPASSubscriptionSubscribeInbound,
	state: func(st *paGatewayState) (*nativeResponder, *cdexSubscriptionState) {
		return st.pasSubscribe, st.pasSubscription
	},
	endpoint: endpointcert.PayerPAS,
	subscriber: func(g *Gateway, provider string) (subscription.Holder, string) {
		return providerHolder(provider, g.attestedProviderIDs(provider)), ""
	},
	refusals: subscribeRefusals{
		notTaken:       refusalPASSubscribeNotTaken,
		searchNotOwned: refusalPASSubscribeSearchNotOwned,
		target:         refusalPASSubscribeTarget,
	},
}

// attestedProviderIDs are the organization identifiers the registry attests
// for provider, none when it is not registered as a provider.
func (g *Gateway) attestedProviderIDs(provider string) []shnsdk.ProviderIdentifier {
	e, ok := g.cfg.Reg.Lookup(provider)
	if !ok || e.Role != "provider" {
		return nil
	}
	return append([]shnsdk.ProviderIdentifier(nil), e.ProviderIDs...)
}

// providerHolder is provider as a topic's filter holds it: its participant id
// and the organization identifiers the registry attests for it.
func providerHolder(provider string, ids []shnsdk.ProviderIdentifier) subscription.Holder {
	h := subscription.Holder{ID: provider}
	for _, id := range ids {
		h.ProviderIDs = append(h.ProviderIDs, subscription.Identifier{System: id.System, Value: id.Value})
	}
	return h
}

// handlePASSubscribeInbound answers a provider's Subscription interaction on
// the PAS topic the Hub delivered to this payer's gateway.
func (g *Gateway) handlePASSubscribeInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, body []byte, answerTok string) {
	g.handleSubscribeInbound(w, r, env, tok, body, answerTok, pasSubscribeRecipient)
}

// newPASSubscriptionState is a payer gateway's subscription state and the
// responder its Subscription endpoint is reached through, both nil when its
// participant sets no subscription scope. New refuses a gateway whose base is
// not an absolute http or https URL with a host and no query or fragment, or
// whose identity yields no keys.
func newPASSubscriptionState(cfg Config) (*nativeResponder, *cdexSubscriptionState, error) {
	if strings.TrimSpace(cfg.PayerSubscriptionScope) == "" || cfg.Role != "payer" {
		return nil, nil, nil
	}
	raw := strings.TrimSpace(cfg.PayerSubscriptionBaseURL)
	base, err := url.Parse(raw)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.ForceQuery || base.Fragment != "" {
		return nil, nil, fmt.Errorf("gateway: PayerSubscriptionScope (PAYER_DAVINCI_SUBSCRIPTION_SCOPE) requires PayerSubscriptionBaseURL (PAYER_DAVINCI_PAS_BASE_URL, or PAYER_DAVINCI_BASE_URL when that is unset) to be an absolute http or https URL with a host and no query or fragment (%q)", raw)
	}
	// The base is stated to the subscriber's gateway (statedBase), which
	// takes one with no ending slash: a base written with one names the same
	// base.
	base.Path, base.RawPath = strings.TrimRight(base.Path, "/"), strings.TrimRight(base.RawPath, "/")
	if cfg.Identity.HolderID != cfg.HolderID {
		return nil, nil, fmt.Errorf("gateway: PayerSubscriptionScope (PAYER_DAVINCI_SUBSCRIPTION_SCOPE): the identity names holder %q, and this gateway is holder %q: the notification keys are derived under one holder: Config.Identity.HolderID and Config.HolderID must name the same holder; the gateway app sets both from its registered bundle", cfg.Identity.HolderID, cfg.HolderID)
	}
	keys, err := subscription.DeriveKeys(cfg.Identity.HolderID, cfg.Identity.SignPriv, cfg.SubscriptionPredecessorKey)
	if err != nil {
		return nil, nil, fmt.Errorf("gateway: PayerSubscriptionScope (PAYER_DAVINCI_SUBSCRIPTION_SCOPE): the notification keys cannot be derived from this gateway's identity: %w", err)
	}
	opts := append([]NativeOption{WithUpstreamParty("payer"), WithBackendDeadline(cdexTaskDeadline)}, cfg.PayerSubscriptionOptions...)
	responder := NewNativeResponder(cfg.PayerSubscriptionClient, base.String(), cfg.Clock, opts...)
	return responder, &cdexSubscriptionState{client: cfg.PayerSubscriptionClient, base: base, keys: keys}, nil
}

// PASSubscriptionUnsupportedEvent is the observer event a payer's gateway
// raises at startup when it takes a provider's Subscription to the PAS topic
// (PayerSubscriptionScope) and its system's CapabilityStatement does not show
// Subscription support, or cannot be read; its Detail is the reason. It is a
// signal for the operator only: the gateway declares the leg from its setting
// whatever the answer, and a Subscription its system does not serve is carried
// to it and answered as that system answers.
const PASSubscriptionUnsupportedEvent = "pas.subscription-unsupported"

// pasMetadataLimit bounds the CapabilityStatement the startup check reads.
const pasMetadataLimit = 4 << 20

// CheckPASSubscriptionSupport reads the payer system's CapabilityStatement at
// its Subscription endpoint's base ([base]/metadata), with the subscription
// client, and reports whether it declares the Subscription resource. Where it
// does not, or cannot be read, it logs a warning naming why and raises
// PASSubscriptionUnsupportedEvent. It changes nothing the gateway declares or
// how any request is carried. A gateway that takes no such Subscription reads
// nothing and reports true.
func (g *Gateway) CheckPASSubscriptionSupport(ctx context.Context) (supported bool, reason string) {
	st := g.paState()
	if st == nil || st.pasSubscription == nil {
		return true, ""
	}
	reason = pasSubscriptionSupport(ctx, st.pasSubscribe, st.pasSubscription)
	if reason == "" {
		return true, ""
	}
	log.Printf("gateway: WARNING PAYER_DAVINCI_SUBSCRIPTION_SCOPE is set, and %s; the gateway takes a provider's Subscription to the PAS topic all the same, and carries each one to your system as sent", reason)
	g.observe(ObserverEvent{Kind: PASSubscriptionUnsupportedEvent, LegType: pastopic.SubscribeLeg, Detail: reason})
	return false, reason
}

// pasSubscriptionSupport is "" when the CapabilityStatement at sub's base
// declares the Subscription resource on a server rest, and otherwise why not.
func pasSubscriptionSupport(ctx context.Context, responder *nativeResponder, sub *cdexSubscriptionState) string {
	at, err := subscriptionTarget(sub.base, "metadata", "", false)
	if err != nil {
		return "the payer system's CapabilityStatement address cannot be built under its base"
	}
	req, err := http.NewRequestWithContext(endpointcert.WithTag(ctx, endpointcert.Of(endpointcert.PayerPAS)), http.MethodGet, at.String(), nil)
	if err != nil {
		return "the payer system's CapabilityStatement cannot be asked for"
	}
	req.Header.Set("Accept", "application/fhir+json")
	// As every call to the payer's own system: its fixed backend headers.
	responder.applyBackendHeaders(req)
	if responder.backendDeadline > 0 {
		dctx, cancel := context.WithTimeout(req.Context(), responder.backendDeadline)
		defer cancel()
		req = req.WithContext(dctx)
	}
	resp, err := sub.client.Do(req)
	if err != nil {
		// The client's error is not named: it can carry the token endpoint's
		// or the base's whole address.
		if errors.Is(err, context.DeadlineExceeded) {
			return "the payer system's CapabilityStatement cannot be read: the request timed out"
		}
		return "the payer system's CapabilityStatement cannot be read: the request failed"
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Sprintf("the payer system's CapabilityStatement cannot be read: it answered %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, pasMetadataLimit+1))
	if err != nil || len(body) > pasMetadataLimit {
		return "the payer system's CapabilityStatement cannot be read whole"
	}
	var cs struct {
		ResourceType string `json:"resourceType"`
		Rest         []struct {
			Mode     string `json:"mode"`
			Resource []struct {
				Type string `json:"type"`
			} `json:"resource"`
		} `json:"rest"`
	}
	if err := json.Unmarshal(body, &cs); err != nil || cs.ResourceType != "CapabilityStatement" {
		return "the payer system's metadata is not a CapabilityStatement this gateway can read"
	}
	for _, r := range cs.Rest {
		if r.Mode != "server" {
			continue
		}
		for _, res := range r.Resource {
			if res.Type == "Subscription" {
				return ""
			}
		}
	}
	return "the payer system's CapabilityStatement declares no Subscription resource"
}
