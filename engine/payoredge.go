// payoredge.go — the payer-edge identity mapping: a payer-role gateway translating
// its network payer identity into the identifier its own payer system expects, on the
// request it sends that system (nativeResponder).
//
// The mapping is an ownership assertion, not a rewrite of the request:
//   - It maps only a request whose Coverages name an identity this gateway OWNS. A payer
//     publishes its identities itself, on the network feed, and the routing directory is
//     many-to-many: a request routed here on any one of them is a request this payer owns.
//     So "own" is the set of identities this holder publishes on the feed, union the
//     configured identity (ownPayerIdentities). A Coverage naming another payer —
//     including an identity published by a DIFFERENT holder — or no readable payer, is
//     refused with a legible 400-class LegResult, never a bare error (which the engine
//     would answer with an opaque 500); Coverages naming different payers, and a
//     Coverage payor reference that resolves to no resource or to several, are refused
//     with a 422. A prior-authorization Claim's insurer reference that resolves to no
//     resource or to several is the request's own content (RuleInsurer): not checked
//     at none, recorded at observe and left as sent, refused at strict with the same 422.
//   - It changes only the payer identifier's system and value strings (inline on the
//     Coverage's routing payor, payor[0], or on the Organization it references) and, for
//     a prior-authorization request, the Claim insurer's when it names this payer (an
//     insurer naming another identity is left as sent). The
//     edit is the registered payer-identity edit (relayedit.go), made by relay.Apply:
//     every other byte of the request is sent exactly as it arrived, and an edit that
//     would change signed content is refused (422) rather than made.
//   - A mapping to the identity the request already carries makes no edit: the request
//     is sent exactly.
//
// Coverage carriage spans the legs this mapping covers: both CRD legs,
// crd-order-select and crd-order-dispatch (prefetch.coverage, a bare Coverage or a
// Bundle), dtr-questionnaire-fetch (the request's coverage), and
// pas-claim/pas-claim-update/pas-claim-inquire (every Coverage entry of the
// request Bundle, and each Claim.insurer — an inquiry Bundle carries both, so it
// maps on the same carrier as a submission).
package engine

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// WithPayorEdgePublishedIdentities supplies the feed half of "own": the payer
// identities holderID itself publishes on the network feed, read live off reg so a
// newly published identity is owned without a restart. It is the network's own fact
// about this participant, never a value this gateway mints, and it never replaces the
// configured identity — ownPayerIdentities unions the two.
//
// A gateway that cannot see its own entry among the holders reg has converged — no
// registrar configured, a registration that has not propagated yet, an entry skipped
// for an undecodable key — falls back to the configured identity ALONE. That is
// fail-closed: it narrows what this gateway answers for, never widens it. It says so
// once through note; note may be nil (the default: the standard log).
func WithPayorEdgePublishedIdentities(reg shnsdk.Registry, holderID string, note func(string)) NativeOption {
	if note == nil {
		note = func(s string) { log.Print(s) }
	}
	var once sync.Once
	return func(n *nativeResponder) {
		n.payorEdgePublished = func() []shnsdk.PayerIdentifier {
			e, ok := reg.Lookup(holderID)
			if !ok || len(e.PayerIDs) == 0 {
				once.Do(func() {
					note(fmt.Sprintf("payer backend identity mapping: this gateway cannot see its own payer identities on the network feed "+
						"(holder %q); the configured identity alone decides which requests it owns", holderID))
				})
				return nil
			}
			return e.PayerIDs
		}
	}
}

// ownPayerIdentities is the set of payer identities this gateway owns: the configured
// one first (so a single-identity deployment's refusals read exactly as they always
// have), then every identity this holder publishes on the network feed that the
// configured one does not already cover. Never empty while the seam is on.
func (n *nativeResponder) ownPayerIdentities() []shnsdk.PayerIdentifier {
	own := []shnsdk.PayerIdentifier{*n.payorEdgeOwn}
	if n.payorEdgePublished == nil {
		return own
	}
	for _, p := range n.payorEdgePublished() {
		if p.System == "" || p.Value == "" || slices.Contains(own, p) {
			continue
		}
		own = append(own, p)
	}
	return own
}

// mapsPayerIdentity reports whether the payer identity mapping is configured: the
// request this responder sends its payer's system is then addressed by the payer its
// Coverages name.
func (n *nativeResponder) mapsPayerIdentity() bool { return n.payorEdgeOwn != nil }

// payerIdentityMapper is implemented by a responder that can map the payer identity
// (nativeResponder).
type payerIdentityMapper interface {
	mapsPayerIdentity() bool
}

// mapsPayerIdentity reports whether this gateway's responder maps the payer identity of
// the requests it sends its payer's system.
func (g *Gateway) mapsPayerIdentity() bool {
	m, ok := g.cfg.Responder.(payerIdentityMapper)
	return ok && m.mapsPayerIdentity()
}

// OwnPayerIdentitiesForTest exposes the identities this responder answers for —
// test-only introspection (the ConformanceLevelForTest pattern) proving the feed half
// of "own" actually reached this responder, not just whatever options a caller
// assembled. nil when the mapping seam is off.
func (n *nativeResponder) OwnPayerIdentitiesForTest() []shnsdk.PayerIdentifier {
	if n.payorEdgeOwn == nil {
		return nil
	}
	return n.ownPayerIdentities()
}

// payorEdgeOwnListMax bounds how many owned identities a refusal names: enough to
// diagnose which identity the requester should have used, short enough that a payer
// publishing dozens does not turn one refusal into a directory dump.
const payorEdgeOwnListMax = 5

// payorEdgeOwnList renders the owned identities for a refusal, bounded.
func payorEdgeOwnList(own []shnsdk.PayerIdentifier) string {
	shown := own
	if len(shown) > payorEdgeOwnListMax {
		shown = shown[:payorEdgeOwnListMax]
	}
	parts := make([]string, 0, len(shown))
	for _, p := range shown {
		parts = append(parts, p.System+"|"+p.Value)
	}
	s := strings.Join(parts, ", ")
	if rest := len(own) - len(shown); rest > 0 {
		s += fmt.Sprintf(" (+%d more)", rest)
	}
	return s
}

// payorEdgeRefusal builds the fail-closed LegResult for a request that names no
// readable payer, or another payer: a 400-class, legible refusal naming the mismatch
// and the identities this gateway owns (so the requester can see which one it should
// have named), never a bare error (which the engine maps to an opaque "hub routing
// failed" 500 — this refusal must be visible to the caller as a policy decision, not a
// transport fault).
func payorEdgeRefusal(own []shnsdk.PayerIdentifier, got shnsdk.PayerIdentifier, gotOK bool) LegResult {
	is, identity := "is", "identity"
	if len(own) > 1 {
		is, identity = "are", "identities"
	}
	if !gotOK {
		return LegResult{
			Status: http.StatusBadRequest,
			Message: fmt.Sprintf(
				"payer backend identity mapping: inbound Coverage carries no resolvable payor identifier (this gateway's own payer %s %s %s); refusing rather than adjudicating for an unrecognized payer",
				identity, is, payorEdgeOwnList(own)),
		}
	}
	match := "does not match this gateway's own payer identity"
	if len(own) > 1 {
		match = "does not match any of this gateway's own payer identities"
	}
	return LegResult{
		Status: http.StatusBadRequest,
		Message: fmt.Sprintf(
			"payer backend identity mapping: inbound Coverage payor %s|%s %s %s; refusing rather than adjudicating as a different payer",
			got.System, got.Value, match, payorEdgeOwnList(own)),
	}
}

// payorEdgeRequest returns the request to send the payer's own system: the network's
// request in exactly, or, when the mapping is configured and changes something, in with
// the payer-identity edit applied. A refusal is returned as a LegResult (Status set); a
// gateway fault as an error. refusesInsurer decides a Claim insurer reference that does
// not resolve (locatePayorEdge); nil refuses it, which is what a carrier with no Claim
// passes.
func (n *nativeResponder) payorEdgeRequest(in relay.Body, carrier payorEdgeCarrier, contentType string, refusesInsurer func() bool) (relay.Payload, LegResult, error) {
	var none relay.Payload
	if n.payorEdgeOwn == nil {
		return relay.Exact(in, contentType), LegResult{}, nil
	}
	doc, err := relay.Doc(in)
	if err != nil {
		return none, LegResult{Status: http.StatusBadRequest, Message: "payer backend identity mapping: request is not one well-formed JSON document"}, nil
	}
	ops, err := locatePayorEdge(doc, carrier, n.ownPayerIdentities(), *n.payorEdgeBackend, refusesInsurer)
	var refused *payorEdgeRefused
	if errors.As(err, &refused) {
		return none, refused.legResult(), nil
	}
	if err != nil {
		return none, LegResult{}, fmt.Errorf("engine: payor edge: %w", err)
	}
	if len(ops) == 0 {
		return relay.Exact(in, contentType), LegResult{}, nil
	}
	p, err := relay.Apply(in, contentType, relay.EditPayorEdgeRestamp, ops...)
	var signed *relay.SignedContentError
	if errors.As(err, &signed) {
		return none, LegResult{Status: http.StatusUnprocessableEntity, Message: signed.Error()}, nil
	}
	if err != nil {
		return none, LegResult{}, fmt.Errorf("engine: payor edge: %w", err)
	}
	return p, LegResult{}, nil
}
