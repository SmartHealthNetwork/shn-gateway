//go:build !shn_coreonly

package engine

import (
	"bytes"
	"io"
	"net/http"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/pa/pasroute"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The provider gateway's side of a provider's topic Subscription to the PAS
// topic (pas-subscription-subscribe; the notifications design §6.1, §7.2,
// §10): the provider's own system manages its Subscription at a payer's base
// on its gateway (POST, GET, PUT, PATCH and DELETE <base>/Subscription[/…],
// with <base> /pas/payer/{payer}, the base it sends that payer's claims to),
// as it would at the payer's own base, and the network carries each
// interaction to that payer (serveSubscriptions, the topic the PAS topic,
// with E-18, E-20 and E-23 as for the HRex Task topic at a provider's base).
// This file is the base: it admits the caller, names the payer and builds the
// base.
//   - a bearer whose scopes permit the interaction on Subscription (create,
//     read for a read, vread or $status, update, delete, search for a search
//     or a page) is admitted; any other is refused 403 insufficient_scope, on
//     authority, before anything is read;
//   - a base that names no registered payer, or one no base can be built
//     for, is refused on routing: 422 for a create, 404 otherwise;
//   - at the plain base, which names no payer, a Subscription is refused as
//     before (handlePASSubscriptionPlain).
//
// The routes are mounted where the provider takes the PAS topic's
// notifications (PROVIDER_PAS_SUBSCRIPTION, with the Da Vinci ingress): a
// provider that cannot be delivered a notification makes no Subscription
// through the network. A payer whose gateway does not take the leg is refused
// by the Hub, which records it, and its refusal is carried as on every leg.

// RoutePASSubscriptionBase is the Subscription route at a payer's base on
// this gateway, as exchange records name it.
const RoutePASSubscriptionBase = "pas-subscription-base"

// The refusal of a base that names no payer a base can be built for.
const (
	// errPASSubscriptionNoPayer: a base naming no registered payer.
	errPASSubscriptionNoPayer = "this base names no registered payer to subscribe at: a payer's base is " + pasroute.Prefix + "{payer}, which " + PASPayersPath + " lists; nothing was sent"
)

// handlePASSubscriptionBase is the provider system's Subscription interaction
// at a payer's base: admitted, routed to the payer the base names, and served
// by serveSubscriptions with the PAS topic.
func (g *Gateway) handlePASSubscriptionBase(w http.ResponseWriter, r *http.Request) {
	w, _ = g.withScope(w, relay.RoleRequester)
	w = &fhirOperationWriter{w}
	rest := subscriptionRest(r, 4)
	// The caller is held to a Subscription scope that permits the
	// interaction, as at the plain base; the umbrella does not permit one
	// (paScopedRoutes).
	if g.routeScopeRefused(w, r, plainSubscriptionScope(subscriptionNeed(r.Method, rest))) {
		return
	}
	exchangeOf(r.Context()).leg(legPASSubscriptionSubscribe)
	// The body is read as every ingress route reads one, before the base is
	// routed, so a coding or a size it cannot take is refused as there; the
	// handler reads it again as read.
	body, ok := readIngressBody(w, r, shnsdk.MaxRequestBytes)
	if !ok {
		return
	}
	r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	payer, _, registered := g.pasBasePayer(r)
	var base string
	if registered {
		b, err := pasroute.Base(g.cfg.IngressBaseURL, payer)
		registered, base = err == nil, b
	}
	if !registered {
		// A create's own answer for an address that names no one is FHIR's
		// 422; any other interaction's, 404.
		exchangeOf(r.Context()).refusedFor(RefusalRouting, reasonPASSubscriptionNoPayer)
		if r.Method == http.MethodPost {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": errPASSubscriptionNoPayer})
			return
		}
		writeJSON(w, http.StatusNotFound, notFoundOutcome(errPASSubscriptionNoPayer))
		return
	}
	g.serveSubscriptions(w, r, pasTopic, payer, base, rest)
}
