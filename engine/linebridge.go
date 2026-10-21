package engine

import (
	"context"
	"errors"
	"sort"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

// linebridge.go is module 2, the IG-line bridge (core design §3.3): the
// reach across lines by a transform chain (arm (3) of line selection), the
// walk of a request along that chain, the carry of an answer into the
// requester's line (E-08), and the bridged check that certifies what the
// bridge changed. The core asks it only through lineBridge, and only when
// g.bridge() returns one: with module 2 off no chain is selected and no
// answer is renamed, and a leg is built natively at a line it reaches or
// carried as sent.
//
// Building natively at a peer's line (arm (2), linereach.go) is not module
// 2's: it is module 1 choosing its build line, not translation.

// lineBridge is the core's four calls into module 2.
type lineBridge interface {
	// Reach is arm (3): the best transform chain from one of own's lines to
	// one of peer's, for a leg to recipient. It answers (route, "", true), or
	// (zero, the missing ingredient, false) when no chain reaches.
	Reach(recipient, contract string, own, peer map[string]bool) (legRoute, string, bool)
	// AdaptRequest walks payload along route's chain, which is not empty, and
	// returns the walked bytes, the chain's loss reports and record, which
	// emits leg.transformed and the transform Provenance for the walk. A caller
	// that does not send the walked bytes does not call record. record is
	// never nil.
	AdaptRequest(route legRoute, payload []byte, x ExchangeIdentity) ([]byte, []LossReport, func() error, error)
	// AdaptAnswer is the payload the requester's gateway gives its
	// participant's system for answer, the answer on key to a leg routed at
	// the routed token, with declared the tokens this gateway declares: the
	// answer carried into this gateway's line where the bridge carries that
	// answer across, else the answer exactly as it arrived. wrote is the
	// token the answer is written in (routed when carried as sent, "" on a
	// refusal); status is non-zero for a refusal, with its message.
	AdaptAnswer(ctx context.Context, key relay.Key, answer []byte, contentType, routed string, declared []string, x ExchangeIdentity) (p relay.Payload, wrote string, status int, msg string)
	// Certify is the bridged check of out, bytes the bridge changed, at
	// contract's line, against profile ("" for the line's own): SHN's own
	// edit, refused at every level when out is not valid there or the check
	// cannot run, and never left to the observe worker.
	Certify(ctx context.Context, contract, line, profile string, out []byte) govResult
}

// bridge is module 2 when the participant has turned it on
// (Config.TranslateIGLines), and nil when it is off, the default.
func (g *Gateway) bridge() lineBridge {
	if !g.cfg.TranslateIGLines {
		return nil
	}
	return chainBridge{g: g}
}

// errBridgeOff is a route across lines reaching a gateway whose module 2 is
// off. Selection never makes one (only module 2's Reach puts a chain on a
// route), so this is a fault, never sent.
var errBridgeOff = errors.New("engine: a transform chain on a route while module 2, the IG-line bridge, is off")

// chainBridge is module 2 as this build compiles it: the compat manifest's
// transform chains (compat.go), walked by applyChain. It reads the lanes, and
// runs its checks, through the gateway it bridges for.
type chainBridge struct{ g *Gateway }

// Reach is arm (3) (selectChainRoute), with the lanes as the gateway has them
// and the peer's gated overlay (strictPeer).
func (b chainBridge) Reach(recipient, contract string, own, peer map[string]bool) (legRoute, string, bool) {
	laned := func(line string) string {
		v := b.g.validatorForContractLine(contract, line)
		switch {
		case v == nil:
			return "no configured validator lane for line " + line
		case !laneReachable(v):
			return "no certified translation to line " + line + " (its validator lane cannot be reached)"
		}
		return ""
	}
	return selectChainRoute(contract, own, peer, b.g.strictPeer(recipient), laned)
}

// Certify runs the governed check of out as a bridged one: kind fhir-bridged,
// whose network, decided by bridgedDecision rather than the participant's
// conformance level.
func (b chainBridge) Certify(ctx context.Context, contract, line, profile string, out []byte) govResult {
	return b.g.validateGoverned(ctx, findingContextFrom(ctx), b.g.validatorForContractLine(contract, line), out, "egress", line, profile, true)
}

// strictPeer is the per-peer gated-overlay input to arm (3)'s chain
// evaluation (FR-G52). Production-dormant BY DESIGN: this consult always
// answers false for every real
// call site — SHN-registry peers get NO overlay field today ("SHN builds
// tolerate unknown extensions by construction"), and PAYER_DAVINCI_STRICT_EXTENSIONS
// (env → config → WithStrictExtensions → nativeResponder.strictExtensions,
// gateway/app/app.go + native.go) stays dormant plumbing on the native
// responder ONLY — it is not read here. Two future extension points, both
// explicitly deferred, not implemented now: (1) a per-registry-entry
// overlay field on shnsdk.RegistryEntry, live the day a non-SHN build
// registers; (2) reading the SAME PAYER_DAVINCI_STRICT_EXTENSIONS flag here
// too, going live together with transform-at-the-native-forward-edge (the
// deferral native.go's strictExtensions field comment records) — until that
// ships, native.go's arm-1-only pin (TestNativeForwardStaysArm1) means the
// one peer the flag is meant for could never reach this consult anyway.
// StrictPeerForTest (Config, the EgressNativeLines seam's shape) is the SELECTION-level test
// override — never set outside a test.
func (g *Gateway) strictPeer(recipient string) bool { return g.cfg.StrictPeerForTest }

// selectChainRoute is arm (3): some peer-declared target t with a
// manifest-complete chain from an own-declared source s, target lane
// configured and, for an explicit lane, reachable (laned names why a line is
// not, "" when it is), no step refused for this peer (strict — FR-G52), and no gated
// row taken upward (gatedUpward: a chain that could only refuse). Several
// bridgeable targets: highest target wins; several chains to one target:
// chain ranking (full beats carry beats gated, by the chain's WORST step;
// fewer steps beat more; ties broken by highest source line). Returns
// (route, "", true) on success or (zero, issue, false) on refusal — issue
// names the HIGHEST-declared target's specific missing ingredient (the
// RouteRefusalError bridge-ingredient grammar), using bare LINES (never a full
// "contract@line" token) so it cannot duplicate a token already named
// elsewhere in the refusal message (regression fence:
// TestSelectLegToken_RefusalIsLegible's duplicate-collapse assertion).
func selectChainRoute(contract string, ownLines, peerLines map[string]bool, strict bool, laned func(string) string) (legRoute, string, bool) {
	var targets []string
	for l := range peerLines {
		targets = append(targets, l)
	}
	sort.Slice(targets, func(i, j int) bool { return compareLines(targets[i], targets[j]) > 0 })

	var sources []string
	for l := range ownLines {
		sources = append(sources, l)
	}

	issue := ""
	for _, t := range targets {
		var gated *CompatStep
		if why := laned(t); why != "" {
			if issue == "" {
				issue = why
			}
			continue
		}
		var candidates []legRoute
		for _, s := range sources {
			if s == t {
				continue
			}
			steps := chainFor(contract, s, t)
			if len(steps) == 0 {
				continue
			}
			if strict && chainWorstClass(steps) != StepFull {
				continue
			}
			if step, ok := gatedUpward(steps, s); ok {
				gated = &step
				continue
			}
			candidates = append(candidates, legRoute{Token: contract + "@" + t, BuildLine: s, Chain: steps})
		}
		if len(candidates) > 0 {
			return bestChain(candidates), "", true
		}
		if issue != "" {
			continue
		}
		switch {
		case strict && anyChainExists(contract, sources, t):
			issue = "chain to line " + t + " refused for this peer (gated overlay: chain contains a lossy step)"
		case gated != nil:
			issue = "no certified translation to line " + t + " (its chain needs the gated " + gated.Contract + " " + gated.From + " to " + gated.To + " step)"
		default:
			issue = "no transform chain bridges to line " + t
		}
	}
	return legRoute{}, issue, false
}

// gatedUpward reports the gated row, if any, that walking steps from line from
// takes upward: a step with no honest byte-level source for an element the newer
// line requires, which refuses every message it is given. Such a chain can never
// make a certified translation, so module 2 does not plan it, and the leg is
// carried as sent instead (or refused as routing under REQUIRE_SHARED_LINE).
// A gated row taken downward drops into carried content and is planned.
func gatedUpward(steps []CompatStep, from string) (CompatStep, bool) {
	cur := from
	for _, s := range steps {
		up := cur == s.From
		if up && s.Class == StepGated {
			return s, true
		}
		if up {
			cur = s.To
		} else {
			cur = s.From
		}
	}
	return CompatStep{}, false
}

// anyChainExists reports whether ANY manifest-complete chain bridges some
// own-declared source to t, ignoring strictness — used only to distinguish
// selectChainRoute's two "no candidates" refusal messages (a chain exists
// but every candidate was strict-excluded, vs. no chain exists at all).
func anyChainExists(contract string, sources []string, t string) bool {
	for _, s := range sources {
		if s == t {
			continue
		}
		if len(chainFor(contract, s, t)) > 0 {
			return true
		}
	}
	return false
}

// classRank orders StepClass best-to-worst for D1b ranking: full(0) <
// carry(1) < gated(2).
func classRank(c StepClass) int {
	switch c {
	case StepFull:
		return 0
	case StepCarry:
		return 1
	default:
		return 2 // StepGated
	}
}

// chainWorstClass is the worst (highest-rank) StepClass among a chain's
// steps — D1b ranks a candidate chain by its worst step, not its best.
func chainWorstClass(steps []CompatStep) StepClass {
	worst := StepFull
	for _, s := range steps {
		if classRank(s.Class) > classRank(worst) {
			worst = s.Class
		}
	}
	return worst
}

// bestChain picks the D1b-best candidate among several chains to the SAME
// target line. candidates must be non-empty.
func bestChain(candidates []legRoute) legRoute {
	best := candidates[0]
	for _, c := range candidates[1:] {
		if chainBetter(c, best) {
			best = c
		}
	}
	return best
}

// chainBetter reports whether a ranks ABOVE b per D1b: full beats carry
// beats gated (by worst step), fewer steps beat more, ties broken by
// highest source line.
func chainBetter(a, b legRoute) bool {
	ra, rb := classRank(chainWorstClass(a.Chain)), classRank(chainWorstClass(b.Chain))
	if ra != rb {
		return ra < rb
	}
	if len(a.Chain) != len(b.Chain) {
		return len(a.Chain) < len(b.Chain)
	}
	return compareLines(a.BuildLine, b.BuildLine) > 0
}

// SelectChainRouteForTest is a thin exported wrapper around arm (3)'s
// transform-chain evaluation (chain ranking + the strict-peer input) for
// package adversarial and other cross-module tests, which cannot see
// unexported engine symbols — the same cross-module-boundary rationale as
// TransformPASForTest/TransformDTRForTest. own/peerDeclared are this
// contract's declared LINE lists (not tokens); lanedLines are the lines
// treated as having a configured validator lane.
func SelectChainRouteForTest(contract string, own, peerDeclared, lanedLines []string, strict bool) (token, buildLine string, chainLen int, ok bool, refusalDetail string) {
	toSet := func(lines []string) map[string]bool {
		out := map[string]bool{}
		for _, l := range lines {
			out[l] = true
		}
		return out
	}
	lanedSet := toSet(lanedLines)
	laned := func(l string) string {
		if lanedSet[l] {
			return ""
		}
		return "no configured validator lane for line " + l
	}
	route, issue, ok := selectChainRoute(contract, toSet(own), toSet(peerDeclared), strict, laned)
	if !ok {
		return "", "", 0, false, issue
	}
	return route.Token, route.BuildLine, len(route.Chain), true, ""
}

// bridgedDecision is what the bridged check decides about its verdict: bytes
// module 2 changed are SHN's own edit (AGENTS.md, relay fidelity), so anything
// but a valid verdict refuses, at every level and with no conformance level
// stated at all. It is module 2's, not ConformancePolicy's.
func bridgedDecision(v Verdict) Decision {
	if v == VerdictValid {
		return Record
	}
	return Refuse
}
