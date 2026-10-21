package engine

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// lineselect.go is the core's line selection: arm (1), the line both sides
// declare, and what follows when there is none (the default carry at this
// gateway's own line, or the routing refusal under REQUIRE_SHARED_LINE);
// the resume pin's own arm; and relayOriginationError, the one writer of a
// leg's refusal to an ingress or origination caller. The reach across lines
// (linereach.go) is asked only through reachPeerLine and resumeReach: arm (2),
// building natively at the peer's line, is the core's; arm (3), a transform
// chain, is module 2's (linebridge.go).

// legRoute is the resolved reachability decision for one leg:
// which contract-version TOKEN this leg is routed at (the wire-truth line —
// Content.ProfileID, the request-frame stamp, and the pend pin all read Token
// verbatim) and which line the payload is actually BUILT at before any
// adaptation (BuildLine). BuildLine == LineOf(Token) on arm (1) shared-
// declared and arm (2) native-reach (no adaptation needed — the payload is
// built NATIVELY at the target line). On arm (3) transform-chain, BuildLine
// is the chain's own source line (drawn from this build's DECLARED set)
// and Chain carries the ranked steps g.egressAdapt walks to bridge
// BuildLine -> LineOf(Token). Chain is nil on arms (1)/(2) — never
// speculatively populated (transform-iff).
//
// Pinned is set only on a resume leg carried away from its pend's pin: the
// pinned token nothing reaches any more (selectResumeRoute). The leg is then
// at this gateway's own line, and the send records the difference.
type legRoute struct {
	Token, BuildLine string
	Chain            []CompatStep
	Pinned           string
}

// selectLegLine is the SELECT-BEFORE-BUILD primitive (widened by the
// reachability arms): it resolves this leg's route
// BEFORE the payload is built, so the builder can be handed BuildLine rather
// than the line being re-derived after the bytes already exist. A
// *RouteRefusalError is the fail-closed no-bridge outcome, already observed
// on the leg.refused seam (the observation used to live inline at each
// hoisted call site).
//
// Why it moved: until payloads began to differ per line they were identical, so
// selecting inside OriginateLeg was harmless. Now the line CHOOSES THE BUILDER —
// a 2.2 Claim carries item extensions a 2.0 Claim must not — so selection has to
// precede the build or the wire bytes and the routed token disagree.
func (g *Gateway) selectLegLine(recipient, legType, corrID string, builder relay.BuilderID) (legRoute, error) {
	route, terr := g.routeFor(legType, recipient, builder)
	if terr != nil {
		var rre *RouteRefusalError
		var ri *RouteInfo
		if errors.As(terr, &rre) {
			ri = refusalRouteInfo(rre)
		}
		g.observe(ObserverEvent{
			Kind: "leg.refused", Direction: "originate", LegType: legType,
			CorrelationID: corrID, Counterpart: recipient, Detail: terr.Error(),
			Route: ri,
		})
		return legRoute{}, terr
	}
	return route, nil
}

// selectLegLineOrFail is selectLegLine for the scenario handlers, which own the
// ResponseWriter: on refusal it writes the legible 422 (relayOriginationError) or
// the 502 fallback and returns ok=false.
func (g *Gateway) selectLegLineOrFail(w http.ResponseWriter, recipient, legType, corrID string, builder relay.BuilderID) (legRoute, bool) {
	route, err := g.selectLegLine(recipient, legType, corrID, builder)
	if err != nil {
		if g.relayOriginationError(w, err) {
			return legRoute{}, false
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return legRoute{}, false
	}
	return route, true
}

// selectLegRoute is the reachability predicate: arm (1) shared declared
// line (selectContractToken, untouched — intersection-only) -> arm (2)
// native reach -> arm (3) transform chain -> refusal naming the missing
// bridge ingredient. A catalog/library mismatch (this build does not speak
// Contract at all — ownLines empty) fails closed WITHOUT attempting arms
// (2)/(3): a build that doesn't know its own contract cannot be rescued by
// reaching further.
func (g *Gateway) selectLegRoute(recipient, legType string, builder relay.BuilderID) (legRoute, error) {
	contract, err := legContract(legType)
	if err != nil {
		return legRoute{}, err
	}
	if contract == "" {
		return legRoute{}, nil // version-neutral leg
	}
	own := g.declaredContractVersions()
	var peer []string
	declaredAtAll := false
	if entry, ok := g.cfg.Reg.Lookup(recipient); ok {
		peer = entry.ContractVersions
		declaredAtAll = len(peer) > 0
	}
	ownLines := contractLineSet(own, contract)
	refusal := func(issue string) error {
		return &RouteRefusalError{
			Contract: contract, LegType: legType, Recipient: recipient,
			Own:         sortedTokens(contract, ownLines),
			Peer:        sortedTokens(contract, contractLineSet(peer, contract)),
			BridgeIssue: issue,
		}
	}
	if len(ownLines) == 0 {
		return legRoute{}, refusal("")
	}
	// Arm (1): shared declared line, or the silent-peer default — unchanged,
	// selectContractToken stays intersection-only.
	if tok, refused := selectContractToken(own, peer, declaredAtAll, contract); !refused {
		return legRoute{Token: tok, BuildLine: shnsdk.LineOf(tok), Chain: nil}, nil
	}
	// No shared declared line (a silent peer never reaches here — arm (1)
	// always succeeds for one). The reachability arms widen exactly this outcome.
	// Arms (2) and (3), the reach across lines (linereach.go): native reach,
	// then, with module 2 on, a transform chain.
	route, issue, ok := g.reachPeerLine(recipient, contract, ownLines, contractLineSet(peer, contract), builder)
	if ok {
		return route, nil
	}
	// Nothing reaches the peer's line. The participant's own setting refuses
	// the pair as routing; by default the leg is carried at this gateway's
	// own line, as if the two were integrated directly.
	if g.cfg.RequireSharedLine {
		return legRoute{}, refusal(issue)
	}
	carried, _ := carriedRoute(own, contract) // ownLines is not empty here
	return carried, nil
}

// selectResumeRoute re-runs arm selection for a PENDED-LINE PIN resume
// leg with the TARGET FIXED to the pin (never re-negotiated — AI-1: the
// amendment must answer the pend it references). The declared/lane sets may
// have GROWN since origination (the lane map is grow-only), so a line that
// needed a chain at origination time may now be natively buildable — arm 1/2
// native reach still beats a chain at resume.
// pinnedToken is trusted well-formed ("contract@line") — it was produced by
// a prior successful selectLegRoute call and stored verbatim beside the pend.
//
// When nothing reaches the pinned line any more (its lane is gone after a
// configuration change, or a default lane has not yet qualified again after a
// restart), the pin still addresses the follow-up: the same peer, the pend it
// references (the caller's recipient and state). Its IG line is a version
// check, not a network primitive: by default the follow-up is built at this
// gateway's own line and carried as sent, Pinned naming the pin so the send
// records the difference; only the participant's own REQUIRE_SHARED_LINE
// refuses it, as routing.
//
// legType names the leg for the refusal only. Both legs that resume a pend
// reach this function — the amendment and the inquiry — and a refusal that
// always said "amendment" would misname the one it refused.
func (g *Gateway) selectResumeRoute(pinnedToken, recipient, legType string, builder relay.BuilderID) (legRoute, error) {
	contract, target, ok := strings.Cut(pinnedToken, "@")
	if !ok || contract == "" || target == "" {
		return legRoute{}, fmt.Errorf("engine: selectResumeRoute: malformed pin token %q", pinnedToken)
	}
	ownLines := contractLineSet(g.declaredContractVersions(), contract)
	// Arm 1 equivalent: the pinned line is still one this gateway DECLARES, and
	// it serves it (servesLine: laned, or declared at none). Fresh selection's own arm (1) is the declared-shared rule
	// (selectLegRoute) and consults EgressNativeLines not at all — that knob
	// narrows arm (2)'s native-REACH view, never what this gateway declares.
	// Without this arm a resume refused the very line the submission had just
	// been sent at, whenever the two differed: an authorization that could be
	// created could not then be continued, which is the one thing a pin exists
	// to prevent.
	if ownLines[target] && g.servesLine(contract, target) {
		return legRoute{Token: pinnedToken, BuildLine: target, Chain: nil}, nil
	}
	// Arms 2 and 3 equivalent, the reach to the pinned line (linereach.go):
	// native, then, with module 2 on, a chain.
	if route, ok := g.resumeReach(pinnedToken, recipient, contract, target, ownLines, builder); ok {
		return route, nil
	}
	refusal := &RouteRefusalError{
		Contract: contract, LegType: legType, Recipient: recipient,
		Own:         sortedTokens(contract, ownLines),
		Peer:        []string{pinnedToken},
		BridgeIssue: "no bridge to the pinned line " + target + " remains available",
	}
	if g.cfg.RequireSharedLine {
		return legRoute{}, refusal
	}
	carried, ok := g.resumeCarry(contract, recipient, pinnedToken)
	if !ok {
		return legRoute{}, refusal
	}
	carried.Pinned = pinnedToken
	return carried, nil
}

// resumeCarry is the line a follow-up is carried at when nothing reaches its
// pin: a line this gateway and the peer both declare, as fresh selection's arm
// (1) picks it, else this gateway's own highest declared line. Either is built
// natively and sent with no chain. A carry at the pinned line itself is no
// carry (that line is declared but not served, which no gateway boots with), so
// ok is false then, and when this build declares no line for the contract.
func (g *Gateway) resumeCarry(contract, recipient, pinnedToken string) (legRoute, bool) {
	own := g.declaredContractVersions()
	var peer []string
	if entry, found := g.cfg.Reg.Lookup(recipient); found {
		peer = entry.ContractVersions
	}
	if tok, refused := selectContractToken(own, peer, len(peer) > 0, contract); !refused && tok != pinnedToken {
		return legRoute{Token: tok, BuildLine: shnsdk.LineOf(tok)}, true
	}
	carried, ok := carriedRoute(own, contract)
	if !ok || carried.Token == pinnedToken {
		return legRoute{}, false
	}
	return carried, true
}

// recipientRefusedPrefix is how the Hub names a forward the recipient's
// gateway refused at its edge (4xx); the status follows in parentheses.
const recipientRefusedPrefix = "forward to recipient failed: the recipient refused it "

// errRecipientNotYetKnowing is what the caller is told when the recipient's
// gateway refused a leg before acting on it because it has not read this
// gateway's registration yet. It speaks for that leg only: on a route that
// sends several legs, the earlier ones may have been answered.
const errRecipientNotYetKnowing = "the recipient's gateway has not read this gateway's registration yet and took no action on this leg"

// relayOriginationError surfaces a recipient's framed non-2xx application answer
// (a *RelayError from OriginateLeg, unwrapped through any %w chain) to the origination
// caller byte-identically — the same verbatim relay the Da Vinci ingress handlers do,
// for the bespoke /scenario* origination API. Returns true iff it wrote the response;
// callers keep their existing writeJSON fallback for every other (non-relay) error.
// Also writes a *RouteRefusalError (the version-routing legible refusal: no
// shared contract line ⇒ refuse legibly, never forward) as its 422 — one
// chokepoint covers every scenario and ingress
// origination site.
//
// A request payload the ownership table refused is this gateway's own fault:
// it is answered 500 here, before any other mapping.
func (g *Gateway) relayOriginationError(w http.ResponseWriter, err error) bool {
	if isOwnershipFault(err) {
		writeOwnershipFault(w)
		return true
	}
	var rre *RouteRefusalError
	if errors.As(err, &rre) {
		exchangeOfWriter(w).refused(RefusalRouting)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": rre.Error()})
		return true
	}
	// A Hub leg that produced no answer within the client's budget is a 504
	// that says so (and how long the wait was).
	if errors.Is(err, errHubTimeout) {
		writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": err.Error()})
		return true
	}
	// The Authorization Framework not answering is the network being
	// unavailable, not a verdict: this gateway's own 503, and nothing was sent.
	if errors.Is(err, errAuthzUnreachable) {
		writeUnavailable(w, errAuthzUnreachable.Error())
		return true
	}
	// An authority denial — a policy or consent refusal at the Authorization
	// Framework — is a 403, not a routing failure.
	if errors.Is(err, errAuthorizationDenied) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": errAuthorizationDenied.Error()})
		return true
	}
	// The Hub's own refusal is reported as the Hub answered it: its status and
	// reason. A refusal after the recipient was reached says so instead,
	// so the caller does not resend a request the payer may have acted on.
	var refused *hubRefusalError
	if errors.As(err, &refused) {
		switch refused.delivered {
		case "yes":
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the recipient received this request and answered, but its answer was lost on the way back (" +
				refused.reason + "); it may have acted on the request: check its outcome before resending"})
		case "unknown":
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the recipient may have received this request (" +
				refused.reason + "): check its outcome before resending"})
		default:
			// The recipient's gateway refused the leg before acting on it,
			// because it has not read this gateway's registration yet (its own
			// attestation, relayed by the Hub): nothing was done, so the caller
			// is told to resend, and when.
			if refused.notActed {
				w.Header().Set("Retry-After", strconv.Itoa(refused.retryAfter))
				writeUnavailable(w, fmt.Sprintf("%s; resend after %ds", errRecipientNotYetKnowing, refused.retryAfter))
				return true
			}
			// The recipient's gateway refused the forward at its edge: say whose
			// refusal it was, with its status and nothing of its body.
			if code, ok := strings.CutPrefix(refused.reason, recipientRefusedPrefix); ok {
				writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the recipient's gateway refused the exchange " + code})
				return true
			}
			// A Hub 409 (a replayed envelope) is reported as it is. Any other
			// Hub 4xx is about this gateway's standing with the Hub (its
			// registration, its token, its clock), not the caller's request, so
			// the caller is told 502 with the Hub's reason.
			status := refused.status
			if status < 400 || status > 599 || (status/100 == 4 && status != http.StatusConflict) {
				status = http.StatusBadGateway
			}
			writeJSON(w, status, map[string]string{"error": "hub refused the exchange: " + refused.reason})
		}
		return true
	}
	// The recipient answered, but its answer failed this gateway's checks.
	var lost *answerLostError
	if errors.As(err, &lost) {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the recipient received this request and answered, but its answer could not be accepted (" +
			lost.cause + "); it may have acted on the request: check its outcome before resending"})
		return true
	}
	var re *RelayError
	if !errors.As(err, &re) {
		return false
	}
	// A non-empty answer that states no media type is read as FHIR JSON; an
	// empty one keeps what the recipient stated, nothing included.
	ct := re.ContentType
	if ct == "" && len(re.Body) > 0 {
		ct = "application/fhir+json"
	}
	// The recipient's answer, exactly as it arrived.
	body := relay.NewBody(re.Body, relay.OriginPeerFrame)
	k := relay.Key{Leg: re.leg, Role: relay.RoleRequester, Direction: relay.DirectionResponse, Outcome: relay.OutcomeUpstreamError}
	g.writePayload(w, re.Status, ct, relay.Exact(body, ct), k)
	return true
}
