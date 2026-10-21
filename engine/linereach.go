package engine

import "github.com/SmartHealthNetwork/shn-gateway/engine/relay"

// linereach.go is the reach across lines when arm (1) finds no line both sides
// declare, or, at resume, when the pinned line is not one this gateway declares
// and serves (servesLine). Arm (2), building natively at a peer-declared line
// this build speaks and can build at (buildsAt: a lane serves it, or no payload
// check consumes lanes and the builder is certified there), is the core's: module 1 choosing the
// line it builds at, which translates nothing, so it runs whether or not module
// 2 is on. Arm (3), a transform chain to the peer's line, is module 2's
// (linebridge.go), asked only through the gateway's bridge. The core's
// selection (lineselect.go) asks this file only through reachPeerLine and
// resumeReach.

// reachPeerLine is arms (2) and (3) for a fresh leg: native reach, else, with
// module 2 on, the best transform chain. It answers (route, "", true), or
// (zero, the missing ingredient, false) when nothing reaches the peer's line
// (no ingredient is named when module 2 is off).
func (g *Gateway) reachPeerLine(recipient, contract string, ownLines, peerLines map[string]bool, builder relay.BuilderID) (legRoute, string, bool) {
	if route, ok := g.selectNativeReachRoute(contract, peerLines, builder); ok {
		return route, "", true
	}
	b := g.bridge()
	if b == nil {
		return legRoute{}, "", false
	}
	return b.Reach(recipient, contract, ownLines, peerLines)
}

// resumeReach is arms 2 and 3 at resume: the pinned target line built
// natively, else, with module 2 on, a chain from a current own-declared source
// to it.
func (g *Gateway) resumeReach(pinnedToken, recipient, contract, target string, ownLines map[string]bool, builder relay.BuilderID) (legRoute, bool) {
	// Arm 2 equivalent: is the pinned target line buildable natively RIGHT
	// NOW (own declared may have grown to include it, or buildsAt may hold:
	// laned, or certified for builder where no payload check consumes lanes)?
	// Either way, native beats a chain at resume too.
	for _, t := range g.nativeLinesView(contract) {
		if t == target && g.buildsAt(builder, contract, t) {
			return legRoute{Token: pinnedToken, BuildLine: target, Chain: nil}, true
		}
	}
	// Arm 3: re-derive a chain from a CURRENT own-declared source to the
	// pinned target, the one line the peer is asked at.
	b := g.bridge()
	if b == nil {
		return legRoute{}, false
	}
	route, _, ok := b.Reach(recipient, contract, ownLines, map[string]bool{target: true})
	if !ok {
		return legRoute{}, false
	}
	route.Token = pinnedToken
	return route, true
}

// selectNativeReachRoute is arm (2): some peer-declared line t native to
// this build's binary AND buildable by builder (buildsAt) wins, highest t
// first. Deliberately independent of own's DECLARED set — native reach is the
// sanctioned exception to "declared is the egress statement", gated only by
// buildsAt (the lane map, or a certified builder where no payload check
// consumes lanes) and, when narrowed (tests or SHN_DEMO_EGRESS_NATIVE_LINES),
// EgressNativeLines.
func (g *Gateway) selectNativeReachRoute(contract string, peerLines map[string]bool, builder relay.BuilderID) (legRoute, bool) {
	best := ""
	for _, t := range g.nativeLinesView(contract) {
		if !peerLines[t] || !g.buildsAt(builder, contract, t) {
			continue
		}
		if best == "" || compareLines(t, best) > 0 {
			best = t
		}
	}
	if best == "" {
		return legRoute{}, false
	}
	return legRoute{Token: contract + "@" + best, BuildLine: best, Chain: nil}, true
}

// nativeLinesView is arm (2)'s (and resumeReach's) view of
// NativeContractVersions() for contract: the real set in production,
// RESTRICTED to Config.EgressNativeLines when set (nil is the
// PRODUCTION default; non-nil comes from the cross-line pair test suite or
// the loudly-named SHN_DEMO_EGRESS_NATIVE_LINES demo env, gateway/app) so
// arm (3)'s transform chain fires even though the target line IS native.
func (g *Gateway) nativeLinesView(contract string) []string {
	lines := nativeLinesForContract(contract)
	if g.cfg.EgressNativeLines == nil {
		return lines
	}
	allowed := map[string]bool{}
	for _, l := range g.cfg.EgressNativeLines {
		allowed[l] = true
	}
	var out []string
	for _, l := range lines {
		if allowed[l] {
			out = append(out, l)
		}
	}
	return out
}
