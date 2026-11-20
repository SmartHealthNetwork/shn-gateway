//go:build !shn_coreonly

package engine

import (
	"net/http"
	"slices"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/pa/pasroute"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The PAS per-payer base on a provider gateway's Da Vinci ingress (D132):
// [ingress base]/pas/payer/{payer} (pasroute.Base), at which the provider's
// system sends Claim/$submit and Claim/$inquire for that payer, and discovers
// this gateway's token endpoint (SMART App Launch has every FHIR base serve
// its configuration), as it would at that payer's own base in a direct
// integration, changing nothing else. A claim or an inquiry there is served by
// the plain base's handler, on the same route, leg and checks; it is routed to
// the payer the base names, not by its Coverage (N12). What the Coverage names
// is read only to compare, and the claim is carried to the base's payer as
// sent: a Coverage resolved to another registered payer is recorded
// (PASPayerDiffersEvent); one this gateway cannot resolve to a registered
// payer, whether for want of a payer directory or for what the Coverage
// carries, is recorded as unresolved (PASPayerUnresolvedEvent), since an
// unknown payer is neither the base's nor another. A base that names no
// registered payer is refused 404 on routing, before the request is read
// further, and nothing is sent.
//
// The base serves no CapabilityStatement yet: the plain ingress's declares
// operations the base does not serve, so a base's own waits for its builder,
// and GET {base}/metadata is answered as any path the gateway does not serve.
//
// A Subscription names no payer, so the plain base cannot route one: a create
// there is refused 422, and a search, read, vread, update or delete 404, each
// on routing and naming PASPayersPath, where the payers' bases are listed
// (N8). Sending it to every payer is not done: it would tell payers the
// provider never chose that it subscribes, and with what filter. The
// Subscription interactions at a payer's base are the subscribe leg's
// (legPASSubscriptionSubscribe), served by handlePASSubscriptionBase
// (workstream_pa_pas_subscriber.go) where the provider takes the PAS topic's
// notifications.

// legPASSubscriptionSubscribe is the PAS subscribe leg: the provider's
// Subscription create, read, vread, update, delete, search and instance
// $status, at a payer's base. A payer that takes it is listed at
// PASPayersPath.
const legPASSubscriptionSubscribe = "pas-subscription-subscribe"

// PASPayersPath is the provider gateway's listing of the payers' bases at
// which a Subscription can be created.
const PASPayersPath = pasroute.PayersPath

// RoutePASSubscription is the route of a Subscription at the plain base,
// which names no payer, as exchange records name it.
const RoutePASSubscription = "pas-subscription"

// PASPayerDiffersEvent is the observer event a provider gateway raises when a
// claim or an inquiry sent to a payer's base carries a Coverage it resolves to
// another registered payer. It is carried to the payer the base names; the
// event carries no payer or member identifier.
const PASPayerDiffersEvent = "pas.payer-differs"

// PASPayerUnresolvedEvent is the observer event a provider gateway raises when
// a claim or an inquiry sent to a payer's base carries a Coverage it cannot
// resolve to a registered payer: it has no payer directory, or the Coverage
// names no payer identifier it can resolve. Whether that payer is the base's
// is unknown, so it is neither the same payer nor another. It is carried to
// the payer the base names; the event carries no payer or member identifier.
const PASPayerUnresolvedEvent = "pas.payer-unresolved"

// The per-payer base's mux patterns, which handlePASIngress and
// handlePASInquireIngress dispatch on.
const (
	patternPASSubmitPayer      = "POST /pas/payer/{payer}/Claim/$submit"
	patternPASInquirePayer     = "POST /pas/payer/{payer}/Claim/$inquire"
	patternPASSmartConfigPayer = "GET /pas/payer/{payer}/.well-known/smart-configuration"
)

// The refusals the per-payer base and the plain base's Subscription routes
// make, before anything is sent.
const (
	// errPASBaseNoPayer: a per-payer base names no registered payer.
	errPASBaseNoPayer = "this base names no registered payer: a payer's base is " + pasroute.Prefix + "{payer}, under this gateway's base, with the payer's participant id"
	// errPASSubscriptionPlainBase: a Subscription created at the plain base.
	errPASSubscriptionPlainBase = "the network cannot route a Subscription at this base (routing): a Subscription names no payer; create it at the base of the payer it subscribes to, which " + PASPayersPath + " lists; nothing was sent"
	// errPASSubscriptionNotFound: a Subscription searched for, read, updated
	// or deleted at the plain base.
	errPASSubscriptionNotFound = "Subscription not found at this base: a Subscription is at the base of the payer it was created at, which " + PASPayersPath + " lists; nothing was sent"
	// errPASSubscriptionPlainBaseNotTaken and errPASSubscriptionNotFoundNotTaken
	// are those refusals on a gateway without PROVIDER_PAS_SUBSCRIPTION, which
	// takes no Subscription at any payer's base and lists none, so they name no
	// listing.
	errPASSubscriptionPlainBaseNotTaken = "the network cannot route a Subscription at this base (routing): this gateway takes no Subscription to the PAS topic at any payer's base (" + settingProviderPASSubscription + " is not set); nothing was sent"
	errPASSubscriptionNotFoundNotTaken  = "Subscription not found at this base: this gateway takes no Subscription to the PAS topic at any payer's base (" + settingProviderPASSubscription + " is not set); nothing was sent"
)

// pasPayerNote is what a request at a payer's base records of the payer its
// Coverage names: nothing when it is the base's payer (pasPayerSame),
// PASPayerDiffersEvent when it is another registered payer, and
// PASPayerUnresolvedEvent when this gateway cannot resolve it to one.
type pasPayerNote int

const (
	pasPayerSame pasPayerNote = iota
	pasPayerDiffers
	pasPayerUnresolved
)

// pasIngressRecipient is the payer a PAS submit, amendment or inquiry is
// routed to, with the payer identities the participant's tolerance of named
// payers (CONFORMANCE_TOLERATE_PEERS) is matched against, and what its
// Coverage notes. At the plain base it is the payer its Coverage names
// (pasRecipient), by the identifier it names: one naming no registered payer
// is refused, as it always was. At a payer's base it is that payer, which
// pasBaseRefused has admitted (404 otherwise). A Coverage resolved to the
// base's payer is matched by the identifier it names, and notes nothing. Any
// other answer comes from the base's payer all the same, so it is matched by
// the identifiers that payer registered, and notes PASPayerDiffersEvent when
// the Coverage resolves to another registered payer, PASPayerUnresolvedEvent
// when it resolves to none.
func (g *Gateway) pasIngressRecipient(r *http.Request, body []byte) (recipient string, payers []shnsdk.PayerIdentifier, note pasPayerNote, status int, msg string, reason refusalReason) {
	if r.Pattern != patternPASSubmitPayer && r.Pattern != patternPASInquirePayer {
		recipient, payer, status, msg, reason := g.pasRecipient(body)
		return recipient, []shnsdk.PayerIdentifier{payer}, pasPayerSame, status, msg, reason
	}
	base, e, ok := g.pasBasePayer(r)
	if !ok {
		// pasBaseRefused has already answered a base that names no
		// registered payer; refusing again here keeps the route closed if a
		// handler ever reaches this without it.
		return "", nil, pasPayerSame, http.StatusNotFound, errPASBaseNoPayer, reasonPASBaseNoPayer
	}
	named, pid, status, _, _ := g.pasRecipient(body)
	switch {
	case status == 0 && named == base:
		return base, []shnsdk.PayerIdentifier{pid}, pasPayerSame, 0, "", ""
	case status == 0:
		note = pasPayerDiffers
	default:
		note = pasPayerUnresolved
	}
	return base, append([]shnsdk.PayerIdentifier{}, e.PayerIDs...), note, 0, "", ""
}

// pasBaseRefused refuses a claim or an inquiry at a payer's base that names
// no registered payer, 404 on routing, before the request is read any
// further: the address names no one to send it to, at every level. It reports
// whether it refused.
func (g *Gateway) pasBaseRefused(w http.ResponseWriter, r *http.Request) bool {
	if r.Pattern != patternPASSubmitPayer && r.Pattern != patternPASInquirePayer {
		return false
	}
	if _, _, ok := g.pasBasePayer(r); ok {
		return false
	}
	refusePASRouting(w, r, http.StatusNotFound, errPASBaseNoPayer, reasonPASBaseNoPayer)
	return true
}

// pasBasePayer is the payer the request's base names, with its registry
// entry: a registered holder of role payer. ok is false for any other.
func (g *Gateway) pasBasePayer(r *http.Request) (string, shnsdk.RegistryEntry, bool) {
	payer := r.PathValue("payer")
	e, ok := g.cfg.Reg.Lookup(payer)
	return payer, e, payer != "" && ok && e.Role == "payer"
}

// refusePASRouting writes a PAS ingress request's routing refusal: a payer's
// base naming no registered payer is FHIR's 404 with this gateway's
// OperationOutcome; a Coverage naming no payer to route by is the answer it
// always was.
func refusePASRouting(w http.ResponseWriter, r *http.Request, status int, msg string, reason refusalReason) {
	if status == http.StatusNotFound {
		exchangeOf(r.Context()).refusedFor(RefusalRouting, reason)
		writeJSON(w, status, notFoundOutcome(msg))
		return
	}
	exchangeOf(r.Context()).routedFor(status, reason)
	writeJSON(w, status, map[string]string{"error": msg})
}

// notePASPayer raises the event note names (pasIngressRecipient), if any, for
// a claim or an inquiry carried to a payer's base, once it is sealed: a
// request refused before then is carried nowhere and raises nothing.
func (g *Gateway) notePASPayer(note pasPayerNote, leg, corrID string) {
	switch note {
	case pasPayerDiffers:
		g.observe(ObserverEvent{Kind: PASPayerDiffersEvent, Direction: "ingress", LegType: leg, CorrelationID: corrID})
	case pasPayerUnresolved:
		g.observe(ObserverEvent{Kind: PASPayerUnresolvedEvent, Direction: "ingress", LegType: leg, CorrelationID: corrID})
	}
}

// handlePASBaseSmartConfig is <payer's base>/.well-known/smart-configuration:
// SMART App Launch has every FHIR base serve its configuration, so a system
// that uses a payer's base discovers this gateway's token endpoint there. It
// is the plain base's document. A base naming no registered payer is 404.
func (g *Gateway) handlePASBaseSmartConfig(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := g.pasBasePayer(r); !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": errPASBaseNoPayer})
		return
	}
	g.ingressAuth.handleSmartConfig(w, r)
}

// handlePASSubscriptionPlain is a Subscription at the plain base, which names
// no payer, so the network cannot route it (N8): once the caller is
// authenticated and its body read as every ingress route reads one, a create
// is refused 422 (FHIR's answer to a create the server will not process), and
// a search, read, vread, update or delete 404, each with this gateway's
// OperationOutcome naming where the payers' bases are listed or, on a gateway
// without PROVIDER_PAS_SUBSCRIPTION, which lists none, that it takes no
// Subscription at any payer's base, recorded as routing. Nothing is sent.
func (g *Gateway) handlePASSubscriptionPlain(w http.ResponseWriter, r *http.Request) {
	w, _ = g.withScope(w, relay.RoleRequester)
	w = &fhirOperationWriter{w}
	// The caller is held to a Subscription scope that permits the
	// interaction, as a payer's Subscription routes hold theirs; the umbrella
	// does not permit one (paScopedRoutes).
	if g.routeScopeRefused(w, r, plainSubscriptionScope(subscriptionNeed(r.Method, subscriptionRest(r, 1)))) {
		return
	}
	exchangeOf(r.Context()).leg(legPASSubscriptionSubscribe)
	if _, ok := readIngressBody(w, r, shnsdk.MaxRequestBytes); !ok {
		return
	}
	plain, notFound := reasonPASSubscriptionPlainBase, reasonPASSubscriptionNotFound
	if !g.cfg.ProviderPASSubscription {
		plain, notFound = reasonPASSubscriptionPlainBaseNotTaken, reasonPASSubscriptionNotFoundNotTaken
	}
	if r.Method == http.MethodPost {
		exchangeOf(r.Context()).refusedFor(RefusalRouting, plain)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": string(plain)})
		return
	}
	exchangeOf(r.Context()).refusedFor(RefusalRouting, notFound)
	writeJSON(w, http.StatusNotFound, notFoundOutcome(string(notFound)))
}

// pasPayer is one entry of the provider gateway's PAS payer listing.
type pasPayer struct {
	Participant      string                   `json:"participant"`
	PayerIdentifiers []shnsdk.PayerIdentifier `json:"payerIdentifiers"`
	Base             string                   `json:"base"`
}

// pasPayers lists every registered payer that takes the PAS subscribe leg, in
// participant id order, with its base (pasroute.Base): a listed base takes a
// Subscription, and none that always refuses one is advertised. Without
// PROVIDER_PAS_SUBSCRIPTION this gateway mounts no Subscription route at any
// base, so it lists none: a true empty, never an unknown. A base that cannot
// be built is an error, never a shorter list.
func (g *Gateway) pasPayers() ([]pasPayer, error) {
	out := []pasPayer{}
	if !g.cfg.ProviderPASSubscription {
		return out, nil
	}
	for _, id := range g.cfg.Reg.IDs() {
		e, ok := g.cfg.Reg.Lookup(id)
		if !ok || e.Role != "payer" || !slices.Contains(e.Legs, legPASSubscriptionSubscribe) {
			continue
		}
		base, err := pasroute.Base(g.cfg.IngressBaseURL, id)
		if err != nil {
			return nil, err
		}
		ids := append([]shnsdk.PayerIdentifier{}, e.PayerIDs...)
		out = append(out, pasPayer{Participant: id, PayerIdentifiers: ids, Base: base})
	}
	return out, nil
}

// handlePASPayers is GET /pas/payers: the payers at whose bases a provider's
// system can create a Subscription, each with its base.
func (g *Gateway) handlePASPayers(w http.ResponseWriter, r *http.Request) {
	if g.routeScopeRefused(w, r, g.pasPayersScope()) {
		return
	}
	payers, err := g.pasPayers()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"payers": payers})
}
