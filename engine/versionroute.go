// versionroute.go — version-aware routing: pure
// line-set/selection helpers over contract-version tokens ("<contract>@<line>").
// Selection happens at the ORIGINATING gateway (OriginateLeg); the Hub is
// untouched. The functions are pure so the same filter serves substrate
// recipients (registry-declared tokens) and the foreign native-forward peer
// (operator-declared PAYER_DAVINCI_CONTRACT_VERSIONS).
package engine

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// contractLineSet collapses a declared token list to the SET of lines for one
// contract. A set, not a list: the registrar admits duplicate tokens by design
// (messageFrames precedent), so consumers must tolerate them. Malformed and
// other-contract tokens are skipped — admission already validated shape; this
// is a filter, not a validator.
func contractLineSet(tokens []string, contract string) map[string]bool {
	out := map[string]bool{}
	for _, tok := range tokens {
		c, line, ok := strings.Cut(tok, "@")
		if !ok || c != contract || line == "" {
			continue
		}
		out[line] = true
	}
	return out
}

// compareLines orders two "<major>.<minor>[.<…>]" lines numerically per dot
// segment (2.10 > 2.9); a missing segment is 0 (2 == 2.0). Returns -1/0/1.
func compareLines(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		var av, bv int
		if i < len(as) {
			av, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			bv, _ = strconv.Atoi(bs[i])
		}
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	return 0
}

// highestLine returns the numerically highest line in a set ("" for empty).
func highestLine(lines map[string]bool) string {
	best := ""
	for l := range lines {
		if best == "" || compareLines(l, best) > 0 {
			best = l
		}
	}
	return best
}

// sortedTokens renders a contract's line set as sorted full tokens for legible
// refusal text ("pa.pas@2.0,pa.pas@2.2").
func sortedTokens(contract string, lines map[string]bool) []string {
	out := make([]string, 0, len(lines))
	for l := range lines {
		out = append(out, contract+"@"+l)
	}
	sort.Strings(out)
	return out
}

// selectContractToken applies the routing rule for one contract:
//   - contract == ""            → version-neutral leg: no token, never refused.
//   - !declaredAtAll            → pre-contract peer (declared NOTHING): route at
//     this build's own highest line. Silence is not incompatibility — every
//     pre-v0.35.0 holder and every fixture registry is silent (rollout safety).
//   - declared, shared line     → highest common line, deterministically.
//   - declared, no shared line  → this function stops HERE, refused=true
//     (INTERSECTION-ONLY, deliberately). The
//     reachability arms (native-reach, then a transform chain) live ONE
//     LAYER UP, in the route layer (legRoute / selectLegRoute, originate.go),
//     never inside this function — two verified callers (OriginateLeg's
//     empty-ProfileID fallback, gateway.go, and native.go's foreign-forward
//     filter) must keep intersection-only semantics (the caller×arm
//     matrix), so widening THIS function would silently change their
//     behavior too. selectLegLine is the arms' home; this stays the pure,
//     narrow intersection predicate every caller can still rely on.
//   - declared, no shared line OR contract absent from the non-empty
//     declaration → refused=true. Deliberately stricter than the checks drift
//     rule ("silence is not disagreement"): drift compares two descriptions of
//     the SAME endpoint, routing compares two parties' capabilities — a
//     non-empty declaration is exhaustive for routing.
func selectContractToken(own, peer []string, declaredAtAll bool, contract string) (string, bool) {
	if contract == "" {
		return "", false
	}
	ownLines := contractLineSet(own, contract)
	if len(ownLines) == 0 {
		// This build does not speak the contract it is trying to exercise —
		// a catalog/library mismatch, not a peer problem. Fail closed.
		return "", true
	}
	if !declaredAtAll {
		return contract + "@" + highestLine(ownLines), false
	}
	peerLines := contractLineSet(peer, contract)
	common := map[string]bool{}
	for l := range ownLines {
		if peerLines[l] {
			common[l] = true
		}
	}
	if len(common) == 0 {
		return "", true
	}
	return contract + "@" + highestLine(common), false
}

// RouteRefusalError is the legible version-routing refusal (the AI-G11
// 422 grammar sibling of "no registered payer for identifier …"):
// no shared contract line and no bridge. It names the failing contract, the
// leg, and both parties' declared tokens so the refusal is actionable without
// log access. relayOriginationError writes it as the HTTP 422.
type RouteRefusalError struct {
	Contract  string
	LegType   string
	Recipient string
	Own       []string // this build's tokens for Contract (sorted, deduped)
	Peer      []string // recipient's declared tokens for Contract (sorted, deduped; empty = contract absent from a non-empty declaration)
	// BridgeIssue names the specific reachability-arm ingredient that was
	// missing (arm (4)): empty when arm (1) never even attempted
	// arms (2)/(3) (a catalog/library mismatch — this build does not speak
	// Contract at all); otherwise a short legible phrase naming why arms
	// (2)/(3) also failed ("no configured validator lane for line …", "no
	// transform chain bridges to line …", "chain to line … refused for this
	// peer (gated overlay …)"). Appended parenthetically so the base message
	// (and every existing exact-substring assertion against it) stays
	// unchanged when BridgeIssue is empty.
	BridgeIssue string
}

func (e *RouteRefusalError) Error() string {
	peer := strings.Join(e.Peer, ",")
	if peer == "" {
		peer = "(contract not declared)"
	}
	msg := fmt.Sprintf("no shared contract line for %s (leg %s): this gateway speaks %s; recipient %q declares %s — no bridge available",
		e.Contract, e.LegType, strings.Join(e.Own, ","), e.Recipient, peer)
	if e.BridgeIssue != "" {
		msg += " (" + e.BridgeIssue + ")"
	}
	return msg
}

// selectLegToken resolves the contract-version token for one leg to one
// recipient off the live registry. "" with nil error =
// version-neutral leg or the token for a silent (pre-contract) peer per
// selectContractToken's rules; a *RouteRefusalError is the fail-closed no-
// shared-line outcome. An unregistered recipient is NOT this function's
// concern (roundTripInner already fails it) — treated as silent here.
func (g *Gateway) selectLegToken(recipient, legType string) (string, error) {
	contract, err := legContract(legType)
	if err != nil {
		return "", err
	}
	if contract == "" {
		return "", nil
	}
	own := g.declaredContractVersions()
	var peer []string
	if entry, ok := g.cfg.Reg.Lookup(recipient); ok {
		peer = entry.ContractVersions
	}
	tok, refused := selectContractToken(own, peer, len(peer) > 0, contract)
	if refused {
		return "", &RouteRefusalError{
			Contract:  contract,
			LegType:   legType,
			Recipient: recipient,
			Own:       sortedTokens(contract, contractLineSet(own, contract)),
			Peer:      sortedTokens(contract, contractLineSet(peer, contract)),
		}
	}
	return tok, nil
}

// legContract is the ok-guarded read of a legType's catalog contract. An unknown
// legType is a CALLER BUG (every call site passes a catalog legType), and the
// unchecked map index it replaces returned the zero legSpec — whose empty
// Contract is indistinguishable from a genuinely version-neutral leg. That
// fail-OPEN seed would have silently skipped the version filter and the frame
// stamp for a typo'd or future legType; it now closes loudly (the
// catalog ok-guards). "" with nil error = a genuinely version-neutral leg.
func legContract(legType string) (string, error) {
	spec, ok := paCatalog[legType]
	if !ok {
		return "", fmt.Errorf("engine: unknown legType %q (not in the PA catalog) — cannot resolve its contract", legType)
	}
	return spec.Contract, nil
}

// declaredContractVersions is the SINGLE accessor for this deployment's declared
// exchange-contract token set. Config's
// DeclaredContractVersions carries the operator's SHN_CONTRACT_VERSIONS override
// (boot-validated in gateway/app: grammar + ⊆ NativeContractVersions); empty ⇒
// this build's default declaration.
//
// Every consumer of "what do we speak" reads THIS — leg selection, the
// CapabilityStatement / davinci-configuration builders, the responder's
// symmetric recomputation, and (through internal/provision) the registry entry
// peers select against — so a deployment cannot declare one set locally and
// another to its peers.
func (g *Gateway) declaredContractVersions() []string {
	if len(g.cfg.DeclaredContractVersions) > 0 {
		return g.cfg.DeclaredContractVersions
	}
	return shnsdk.SupportedContractVersions()
}

// contractTokenForLeg is the responder-side frame stamp for a legType:
// "" for a version-neutral leg, else the contract-version token
// the answer's payload was BUILT at. Content-descriptive, not a negotiation echo.
//
// builtToken is the token the responder actually built at — the honored/recomputed
// answer line. It is returned verbatim when set, so the stamp cannot drift
// from the bytes. Empty builtToken (a legacy caller, or a build that made no
// per-line choice) falls back to this build's own highest DECLARED line for the
// contract, which is the pre-request-framing behavior.
func (g *Gateway) contractTokenForLeg(legType, builtToken string) (string, error) {
	contract, err := legContract(legType)
	if err != nil {
		return "", err
	}
	if contract == "" {
		return "", nil // version-neutral legs are never stamped, whatever the caller passes
	}
	if builtToken != "" {
		return builtToken, nil
	}
	lines := contractLineSet(g.declaredContractVersions(), contract)
	if len(lines) == 0 {
		return "", nil
	}
	return contract + "@" + highestLine(lines), nil
}
