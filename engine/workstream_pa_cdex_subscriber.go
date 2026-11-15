//go:build !shn_coreonly

package engine

import (
	"bytes"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/pa/cdexroute"
	"github.com/SmartHealthNetwork/shn-gateway/pa/cdextopic"
	"github.com/SmartHealthNetwork/shn-gateway/pa/subscription"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The payer gateway's side of the payer's topic Subscription to its CDex
// Tasks' changes (cdex-task-subscribe; the notifications design §6.1, §7.2,
// §10): the payer's own system manages its Subscription at a provider's base
// on its gateway (POST, GET, PUT and DELETE <base>/Subscription[/…], with
// <base> /cdex/provider/{provider}, the base its Tasks are created and read
// at), as it would at the provider's own base, and the network carries each
// interaction to that provider (serveSubscriptions, the topic the HRex Task
// topic). This file is the base: it admits the caller, names the provider and
// builds the base.
//   - a bearer whose scopes permit the interaction on Subscription (create,
//     read for a read, vread or $status, update, delete, search for a search
//     or a page) is admitted; any other is refused 403 insufficient_scope, on
//     authority, before anything is read;
//   - a base that names no registered provider, or one no base can be built
//     for, is refused on routing: 422 for a create, 404 otherwise;
//   - at the plain base, which names no provider, a create is refused 422
//     naming the providers' bases, and a read or search 404, each on routing
//     (D132 N8's mirror, as D129 Q5 answers the plain Task read): sending it
//     to every provider would tell providers the payer never chose that it
//     subscribes, and with what filter.
//
// A provider whose gateway does not take the leg is refused by the Hub, which
// records it, and its refusal is carried as on every leg. Each provider's base
// declares Subscription in its CapabilityStatement only when that provider's
// gateway takes the leg.

// cdexTopic is the HRex Task topic a payer subscribes to at a provider.
var cdexTopic subscription.Topic = cdextopic.Topic{}

// RouteCDexSubscriptionBase is the Subscription route at a provider's base
// on this gateway, as exchange records name it.
const RouteCDexSubscriptionBase = "cdex-subscription-base"

// RouteCDexSubscription is the Subscription route at this gateway's plain
// base, which serves none, as exchange records name it.
const RouteCDexSubscription = "cdex-subscription"

// The refusals at the plain base, and of a base that names no provider.
const (
	// errCDexSubscriptionPlainCreate: a create at the plain base names no
	// provider (N8).
	errCDexSubscriptionPlainCreate = "a Subscription names no provider, so it is not created at this base (routing): create it at the base of the provider whose Tasks it follows (POST [base]/Subscription), which " + CDexProvidersPath + " lists"
	// errCDexSubscriptionPlainRead: a read or search at the plain base names
	// no provider (N8).
	errCDexSubscriptionPlainRead = "Subscription not found at this base: a Subscription is read and searched at the base of the provider it was created for, which " + CDexProvidersPath + " lists"
)

// subscriptionNeed is the Subscription permission the request takes: create
// for a POST, update for a PUT or a PATCH, delete for a DELETE, and for a GET
// or HEAD search at the type (a search or a page) and read below it (a read,
// a vread or an instance $status). rest is the path below the base.
func subscriptionNeed(method, rest string) (taskPermission, refusalReason) {
	switch method {
	case http.MethodPost:
		return taskCreate, reasonScopeSubscriptionCreate
	case http.MethodPut, http.MethodPatch:
		return taskUpdate, reasonScopeSubscriptionUpdate
	case http.MethodDelete:
		return taskDelete, reasonScopeSubscriptionDelete
	}
	if rest == "Subscription" {
		return taskSearch, reasonScopeSubscriptionSearch
	}
	return taskRead, reasonScopeSubscriptionRead
}

// subscriptionRest is the path below a base, as the request sent it,
// escaped: the segments from the Subscription segment, which is segment at
// of the path.
func subscriptionRest(r *http.Request, at int) string {
	segs := strings.Split(r.URL.EscapedPath(), "/")
	if len(segs) <= at {
		return ""
	}
	return strings.Join(segs[at:], "/")
}

// handleCDexSubscriptionIngress is the payer system's Subscription
// interaction at a provider's base: admitted, routed to the provider the base
// names, and served by serveSubscriptions with the HRex Task topic.
func (g *Gateway) handleCDexSubscriptionIngress(w http.ResponseWriter, r *http.Request) {
	w, _ = g.withScope(w, relay.RoleRequester)
	w = &fhirOperationWriter{w}
	rest := subscriptionRest(r, 4)
	if need, reason := subscriptionNeed(r.Method, rest); g.subscriptionScopeRefused(w, r, need, reason) {
		return
	}
	exchangeOf(r.Context()).leg(legCDexTaskSubscribe)
	// The body is read as every ingress route reads one, before the base is
	// routed, so a coding or a size it cannot take is refused as there; the
	// handler reads it again as read.
	body, ok := readIngressBody(w, r, shnsdk.MaxRequestBytes)
	if !ok {
		return
	}
	r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	provider := r.PathValue("provider")
	recipient, status, msg, reason := g.cdexRequestRecipient(r)
	var base string
	if status == 0 {
		b, err := cdexroute.ProviderBase(g.cfg.IngressBaseURL, provider)
		if err != nil {
			status, msg, reason = http.StatusUnprocessableEntity, errCDexRequestNoProvider, reasonCDexRequestNoProvider
		}
		base = b
	}
	if status != 0 {
		// A create's own answer for an address that names no one is FHIR's
		// 422; any other interaction's, 404.
		exchangeOf(r.Context()).refusedFor(RefusalRouting, reason)
		if r.Method == http.MethodPost {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": msg})
			return
		}
		writeJSON(w, http.StatusNotFound, notFoundOutcome(msg))
		return
	}
	g.serveSubscriptions(w, r, cdexTopic, recipient, base, rest)
}

// handleCDexSubscriptionPlain is the payer system's Subscription interaction
// at the plain base, which names no provider (N8, as D129 Q5 answers the
// plain Task read): once the bearer is admitted, a create is answered 422 and
// a read or search 404, each this gateway's own OperationOutcome naming where
// a Subscription is made, recorded as routing. Sending it to every provider
// is not done.
func (g *Gateway) handleCDexSubscriptionPlain(w http.ResponseWriter, r *http.Request) {
	w, _ = g.withScope(w, relay.RoleRequester)
	w = &fhirOperationWriter{w}
	rest := subscriptionRest(r, 1)
	if need, reason := subscriptionNeed(r.Method, rest); g.subscriptionScopeRefused(w, r, need, reason) {
		return
	}
	exchangeOf(r.Context()).leg(legCDexTaskSubscribe)
	if r.Method == http.MethodPost {
		// The body is read as every ingress route reads one, so a coding or
		// a size it cannot take is refused as there, then the create.
		if _, ok := readIngressBody(w, r, shnsdk.MaxRequestBytes); !ok {
			return
		}
		exchangeOf(r.Context()).refusedFor(RefusalRouting, reasonCDexSubscriptionPlainCreate)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": errCDexSubscriptionPlainCreate})
		return
	}
	// A read or search carries no body (message integrity, 400), as at the
	// plain Task endpoint.
	if !readNoBody(w, r) {
		return
	}
	exchangeOf(r.Context()).refusedFor(RefusalRouting, reasonCDexSubscriptionPlainRead)
	writeJSON(w, http.StatusNotFound, notFoundOutcome(errCDexSubscriptionPlainRead))
}

// cdexSubscribeSearchParams are the search parameters a provider's base
// serves for a Subscription search (§7.2, §10; N7 with the owner's
// addition): exactly url, status and _id.
var cdexSubscribeSearchParams = []shnsdk.CapabilitySearchParam{
	{Name: "url", Definition: "http://hl7.org/fhir/SearchParameter/Subscription-url", Type: "uri"},
	{Name: "status", Definition: "http://hl7.org/fhir/SearchParameter/Subscription-status", Type: "token"},
	{Name: "_id", Definition: "http://hl7.org/fhir/SearchParameter/Resource-id", Type: "token"},
}

// withCDexSubscription is a provider's base's CapabilityStatement stmt with
// Subscription declared (shnsdk.WithSubscription): create, read, vread,
// update, delete, search-type with exactly url, status and _id, and the
// instance $status. It names no SubscriptionTopic: the HRex Task topic is
// named where the provider's own statement advertises it (§10), which this
// gateway cannot learn from the network today, so it is owed with PRV-7 and
// never guessed.
func withCDexSubscription(stmt []byte) ([]byte, error) {
	return shnsdk.WithSubscription(stmt, shnsdk.SubscriptionCapability{SearchParams: cdexSubscribeSearchParams, StatusOperation: true})
}

// takesCDexSubscribe reports whether the provider's registry entry declares
// the subscribe leg, so its base declares Subscription.
func takesCDexSubscribe(legs []string) bool { return slices.Contains(legs, legCDexTaskSubscribe) }
