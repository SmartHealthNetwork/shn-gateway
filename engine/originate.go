// originate.go — the provider-side scenario drivers (UC-01…08): originate a PA
// exchange, run it through the Hub, and surface the result. Part of package gateway
// (the Smart Gateway runs every holder role; this file is the provider-origination
// surface). Behavior-preserving split of gateway.go (finding C); no logic change.
// See gateway.go for the package doc.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// targetsBrPayer reports whether the origination profile talks to a real Da Vinci PAS payer
// (br-payer) DIRECTLY over HTTP, which needs the contained-insurer / absolute-refs /
// payer-org-entry / DTR-coverage handling every br-payer-shaped request/response requires.
// provider-data is the sole origination lane that dials br-payer's own HTTP surface — it must
// not regress the contained-payor → uniform-A3 bug. This predicate does NOT decide the R-8
// ingress-$validate skip — see relaysReferencePayerBytes below: that concern is about WHOSE
// bytes are being relayed, not about which lane makes the HTTP call. The demo lane relays the
// SAME reference-payer bytes
// (via the in-process mirror, internal/brpayermirror) without ever dialing br-payer itself, and
// needs the skip too — a distinction this predicate alone can no longer express.
func targetsBrPayer(profile string) bool { return profile == "provider-data" }

// isDemoProfile reports whether the origination profile is the family-keyed demo lane
// (§4.3): br-payer-mirrored code families (E0250/L8000/G0151/J3490 — originationCodes)
// driven off the MBR-D-UC0N persona roster, in-process against internal/brpayermirror
// rather than the live br-payer HTTP surface provider-data targets. Distinct from
// targetsBrPayer in HOW it reaches the reference payer (in-process mirror vs. a live HTTP
// dial) — but NOT distinct on WHOSE bytes come back: since the in-process payer stub retired the
// mirror relays the reference payer's OWN pinned bytes verbatim
// (internal/brpayermirror/loopback.go), so this lane gets the SAME R-8 ingress-$validate skip
// provider-data gets, for its payer-directed legs — see relaysReferencePayerBytes and
// validateFHIRPayerIngress. The superseded claim that demo "does NOT get the skip" was true
// only while the demo lane's payer content was still SHN's own in-process stub; it
// no longer is.
func isDemoProfile(profile string) bool { return profile == "demo" }

// relaysReferencePayerBytes reports whether THIS lane's payer counterparty answers with the
// reference payer's own bytes, relayed VERBATIM — never SHN-produced content — which is what
// R-8 (FR-36) actually protects: SHN $validates only what it PRODUCES and hosts US Core
// profiles only, so a real Da Vinci payer's own DTR $questionnaire-package / PAS ClaimResponse
// bytes fail a US-Core-only validator by construction (foreign profiles, e.g.
// dtr-std-questionnaire, are never fetchable — HAPI-0992 on a relayed Parameters wrapper is the
// SAME class of failure, not a different one). Two origination lanes reach the reference payer
// today: provider-data over a live HTTP dial to br-payer (targetsBrPayer), and demo through the
// in-process mirror of it (isDemoProfile). NO profile == "" special case here any more:
// gateway/app.go's loadConfig normalizes an unset ORIGINATION_PROFILE to "demo" ONCE, at the
// config boundary, before this predicate — or any other reader of cfg.OriginationProfile —
// ever sees it, so isDemoProfile alone is now sufficient. Adding profile == "" back here would
// re-establish the exact "every predicate special-cases the unset default separately"
// pattern that was retired: the empty-string trap independently bit two call sites (the
// ingress-$validate skip, then the UC-08 not-covered→deny gate) before the fix moved to the
// config boundary. This predicate answers only "does the LANE relay reference-payer bytes at
// all" — the counterparty half (is THIS leg actually one of the payer-directed leg types) is
// enforced by which validate function a call site uses; see validateFHIR's doc comment.
// Egress (always SHN-produced, on every lane, at every call site) is UNAFFECTED — it keeps
// validating unconditionally; a lane whose counterparty is genuinely SHN-produced (none exist
// among provider-data/demo today, but a future one could) would keep validating ingress too.
func relaysReferencePayerBytes(profile string) bool {
	return targetsBrPayer(profile) || isDemoProfile(profile)
}

// attestsOnHHA reports whether this deployment's UC-04/05/06/07 order is the G0151
// home-health family — and therefore whether an attested QR item must name the
// HomeHealthAssessment questionnaire's own linkId rather than the lumbar Oswestry one.
// True for the provider-data lane (a seeded G0151 ServiceRequest) and for the demo lane
// (§4.3's G0151 family tuple) — i.e. for every lane a provider gateway can be configured
// into today; false only for a lane whose order rides the lumbar questionnaire.
func (g *Gateway) attestsOnHHA() bool {
	return g.cfg.OriginationProfile == "provider-data" || isDemoProfile(g.cfg.OriginationProfile)
}

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
type legRoute struct {
	Token, BuildLine string
	Chain            []CompatStep
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
func (g *Gateway) selectLegLine(recipient, legType, corrID string) (legRoute, error) {
	route, terr := g.selectLegRoute(recipient, legType)
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
func (g *Gateway) selectLegLineOrFail(w http.ResponseWriter, recipient, legType, corrID string) (legRoute, bool) {
	route, err := g.selectLegLine(recipient, legType, corrID)
	if err != nil {
		if g.relayOriginationError(w, err) {
			return legRoute{}, false
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return legRoute{}, false
	}
	return route, true
}

// bridgeRefusalText extracts the reshapable refusal text from err, if err's cause is one
// of the TWO shapes the kit-bridging-visualization demo's designed refusal can legitimately
// take (fix-round finding, 2026-08-26 second live run):
//
//   - *RouteRefusalError — the SELECTION-time refusal: arm(1)/(2)/(3) all fail closed
//     before any bundle is ever built (e.g. this build has no $validate lane at all for the
//     peer's declared line, so neither native reach nor a transform chain can even be
//     attempted). This was the ONLY shape the refuse row's matcher pinned pre-fix-round —
//     it was also, unknowingly, the ONLY shape reachable at the time, because provider-gw
//     had no lane for line 2.2 at all (the FIRST live run: both bridge rows dead at
//     selection).
//   - *SemanticChangeError — the APPLY-time refusal: arm(3)'s chain IS selected (a real
//     bridging chain exists and gets picked), but a gated step inside it refuses to
//     fabricate a field it has no honest byte-level source for (transform_pas.go — pa.pas's
//     2.0->2.1 up-step is gated for every Claim payload). This is the shape the SECOND live
//     run's fix (setting SHN_DEMO_EGRESS_NATIVE_LINES=2.0 on the deployment) produces:
//     narrowing arm(2)'s native reach is what forces arm(3)'s chain to be the path taken at
//     all — without the knob, this build's native pa.pas@2.2 reach wins first and the
//     exchange is silently APPROVED (the run-2 regression this fix closes).
//
// Route callers match this with errors.As (transform_pas.go's own doc comment for
// *SemanticChangeError says exactly this) to distinguish either designed-refusal shape
// from any other applyChain/selectLegLine failure (a plain I/O/parse error, a
// chainDisconnectedError, an unknown legType) — those are genuine faults and must NOT be
// reshaped into "designed" refusal, or a real bug would silently read as the exhibit.
func bridgeRefusalText(err error) (string, bool) {
	var rre *RouteRefusalError
	if errors.As(err, &rre) {
		return rre.Error(), true
	}
	var sce *SemanticChangeError
	if errors.As(err, &sce) {
		return sce.Error(), true
	}
	return "", false
}

// writeBridgeRefusal writes the demo lane's structured 200 refusal body (task2 brief A3a):
// {"refused":true,"refusedAt":legType,"refusal":"<the designed refusal's own error text>"}
// instead of the ordinary non-2xx relayOriginationError/502 path. This is deliberately NOT
// how any other surface answers: internal/scenariodrive's Client.Post/RunCheck treats ANY
// non-2xx status as a transport failure and never hands the response body to a Check's
// Want matcher, so the only way the canary/cloudsmoke UC03-bridge-refuse row can pin WHERE
// and WHY the refusal happened is a 200 body carrying the refusal text verbatim. This is
// the demo lane's own surface, not an external contract — the published Kit pins its own
// gateway child via kit/runner's independent bridge-demo driver (rows_conformant.go), so
// nothing outside this lane is affected.
func writeBridgeRefusal(w http.ResponseWriter, legType, refusal string) {
	writeJSON(w, http.StatusOK, map[string]any{
		"refused":   true,
		"refusedAt": legType,
		"refusal":   refusal,
	})
}

// selectLegLineOrBridgeRefuse is selectLegLineOrFail specialized for the
// kit-bridging-visualization demo's own /scenario/uc03 surface (handleUC03Bridge, task2
// brief A3a): on the SELECTION-time designed refusal (see bridgeRefusalText) it writes
// writeBridgeRefusal's 200 body instead of relayOriginationError's ordinary 422. Any error
// OTHER than a reshapable one (a genuine transport/relay fault) still goes through the
// ordinary relayOriginationError/502 path unchanged — this function reshapes only the
// designed outcome, never masks a real fault as "designed". The APPLY-time sibling
// (a *SemanticChangeError from egressAdapt) is reshaped at handleUC03Bridge's own
// egressAdapt call site instead — selectLegLine itself never returns that error type.
func (g *Gateway) selectLegLineOrBridgeRefuse(w http.ResponseWriter, recipient, legType, corrID string) (legRoute, bool) {
	route, err := g.selectLegLine(recipient, legType, corrID)
	if err == nil {
		return route, true
	}
	if text, ok := bridgeRefusalText(err); ok {
		writeBridgeRefusal(w, legType, text)
		return legRoute{}, false
	}
	if g.relayOriginationError(w, err) {
		return legRoute{}, false
	}
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
	return legRoute{}, false
}

// selectLegRoute is the reachability predicate: arm (1) shared declared
// line (selectContractToken, untouched — intersection-only) -> arm (2)
// native reach -> arm (3) transform chain -> refusal naming the missing
// bridge ingredient. A catalog/library mismatch (this build does not speak
// Contract at all — ownLines empty) fails closed WITHOUT attempting arms
// (2)/(3): a build that doesn't know its own contract cannot be rescued by
// reaching further.
func (g *Gateway) selectLegRoute(recipient, legType string) (legRoute, error) {
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
	peerLines := contractLineSet(peer, contract)
	if route, ok := g.selectNativeReachRoute(contract, peerLines); ok {
		return route, nil
	}
	strict := g.strictPeer(recipient)
	laned := func(line string) bool { return g.validatorForContractLine(contract, line) != nil }
	route, issue, ok := selectChainRoute(contract, ownLines, peerLines, strict, laned)
	if ok {
		return route, nil
	}
	return legRoute{}, refusal(issue)
}

// selectNativeReachRoute is arm (2): some peer-declared line t native to
// this build's binary AND laned wins, highest t first. Deliberately
// independent of own's DECLARED set — native reach is the
// sanctioned exception to "declared is the egress statement", gated only by
// the lane map and, when narrowed (tests or SHN_DEMO_EGRESS_NATIVE_LINES),
// EgressNativeLines.
func (g *Gateway) selectNativeReachRoute(contract string, peerLines map[string]bool) (legRoute, bool) {
	best := ""
	for _, t := range g.nativeLinesView(contract) {
		if !peerLines[t] || g.validatorForContractLine(contract, t) == nil {
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

// nativeLinesView is arm (2)'s (and selectResumeRoute's) view of
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
// configured, no step refused for this peer (strict — FR-G52). Several
// bridgeable targets: highest target wins; several chains to one target:
// chain ranking (full beats carry beats gated, by the chain's WORST step;
// fewer steps beat more; ties broken by highest source line). Returns
// (route, "", true) on success or (zero, issue, false) on refusal — issue
// names the HIGHEST-declared target's specific missing ingredient (the
// RouteRefusalError bridge-ingredient grammar), using bare LINES (never a full
// "contract@line" token) so it cannot duplicate a token already named
// elsewhere in the refusal message (regression fence:
// TestSelectLegToken_RefusalIsLegible's duplicate-collapse assertion).
func selectChainRoute(contract string, ownLines, peerLines map[string]bool, strict bool, laned func(string) bool) (legRoute, string, bool) {
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
		if !laned(t) {
			if issue == "" {
				issue = "no configured validator lane for line " + t
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
			candidates = append(candidates, legRoute{Token: contract + "@" + t, BuildLine: s, Chain: steps})
		}
		if len(candidates) > 0 {
			return bestChain(candidates), "", true
		}
		if issue != "" {
			continue
		}
		if strict && anyChainExists(contract, sources, t) {
			issue = "chain to line " + t + " refused for this peer (gated overlay: chain contains a lossy step)"
		} else {
			issue = "no transform chain bridges to line " + t
		}
	}
	return legRoute{}, issue, false
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
	laned := func(l string) bool { return lanedSet[l] }
	route, issue, ok := selectChainRoute(contract, toSet(own), toSet(peerDeclared), strict, laned)
	if !ok {
		return "", "", 0, false, issue
	}
	return route.Token, route.BuildLine, len(route.Chain), true, ""
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
// legType names the leg for the refusal only. Both legs that resume a pend
// reach this function — the amendment and the inquiry — and a refusal that
// always said "amendment" would misname the one it refused.
func (g *Gateway) selectResumeRoute(pinnedToken, recipient, legType string) (legRoute, error) {
	contract, target, ok := strings.Cut(pinnedToken, "@")
	if !ok || contract == "" || target == "" {
		return legRoute{}, fmt.Errorf("engine: selectResumeRoute: malformed pin token %q", pinnedToken)
	}
	ownLines := contractLineSet(g.declaredContractVersions(), contract)
	// Arm 1 equivalent: the pinned line is still one this gateway DECLARES, and
	// it is laned. Fresh selection's own arm (1) is the declared-shared rule
	// (selectLegRoute) and consults EgressNativeLines not at all — that knob
	// narrows arm (2)'s native-REACH view, never what this gateway declares.
	// Without this arm a resume refused the very line the submission had just
	// been sent at, whenever the two differed: an authorization that could be
	// created could not then be continued, which is the one thing a pin exists
	// to prevent.
	if ownLines[target] && g.validatorForContractLine(contract, target) != nil {
		return legRoute{Token: pinnedToken, BuildLine: target, Chain: nil}, nil
	}
	// Arm 2 equivalent: is the pinned target line buildable natively RIGHT
	// NOW (own declared may have grown to include it, or it may simply be
	// laned)? Either way, native beats a chain at resume too.
	for _, t := range g.nativeLinesView(contract) {
		if t == target && g.validatorForContractLine(contract, t) != nil {
			return legRoute{Token: pinnedToken, BuildLine: target, Chain: nil}, nil
		}
	}
	// Arm 3: re-derive a chain from a CURRENT own-declared source to the
	// pinned target.
	if g.validatorForContractLine(contract, target) != nil {
		strict := g.strictPeer(recipient)
		var candidates []legRoute
		for s := range ownLines {
			if s == target {
				continue
			}
			steps := chainFor(contract, s, target)
			if len(steps) == 0 {
				continue
			}
			if strict && chainWorstClass(steps) != StepFull {
				continue
			}
			candidates = append(candidates, legRoute{Token: pinnedToken, BuildLine: s, Chain: steps})
		}
		if len(candidates) > 0 {
			return bestChain(candidates), nil
		}
	}
	return legRoute{}, &RouteRefusalError{
		Contract: contract, LegType: legType, Recipient: recipient,
		Own:         sortedTokens(contract, ownLines),
		Peer:        []string{pinnedToken},
		BridgeIssue: "no bridge to the pinned line " + target + " remains available",
	}
}

// recipientRefusedPrefix is how the Hub names a forward the recipient's
// gateway refused at its edge (4xx); the status follows in parentheses.
const recipientRefusedPrefix = "forward to recipient failed: the recipient refused it "

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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": errOwnershipFault})
		return true
	}
	var rre *RouteRefusalError
	if errors.As(err, &rre) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": rre.Error()})
		return true
	}
	// A Hub leg that produced no answer within the client's budget is a 504
	// that says so (and how long the wait was).
	if errors.Is(err, errHubTimeout) {
		writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": err.Error()})
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
	ct := re.ContentType
	if ct == "" {
		ct = "application/fhir+json"
	}
	// The recipient's answer, exactly as it arrived.
	body := relay.NewBody(re.Body, relay.OriginPeerFrame)
	k := relay.Key{Leg: re.leg, Role: relay.RoleRequester, Direction: relay.DirectionResponse, Outcome: relay.OutcomeUpstreamError}
	g.writePayload(w, re.Status, ct, relay.Exact(body, ct), k)
	return true
}

// FilledItem is the gateway-engine-LOCAL attribution surface for a DTR auto-filled
// QR item (console response, QRItems field). The SDK's FillQuestionnaire drops the
// FilledItem summary (UI-only); the gateway reconstructs it via fillSummary
// from the ClinicalContext. JSON tags are byte-for-byte compatible with the original
// dtr.FilledItem shape so the console response format is unchanged.
type FilledItem struct {
	LinkID    string `json:"linkId"`
	Answer    string `json:"answer"`
	Origin    string `json:"origin"`
	SourceRef string `json:"sourceRef,omitempty"`
}

// fillSummary reconstructs the []FilledItem summary from the ClinicalContext —
// the same leaves shnsdk.FillQuestionnaire answers, in the same order (the fill
// carries no per-item summary, so the console surface is rebuilt here). Items with
// a negative/absent flag (prior-surgery=false, etc.) are omitted, matching the
// fill. functional-status-oswestry is intentionally absent (no local source).
func fillSummary(cc shnsdk.ClinicalContext) []FilledItem {
	var out []FilledItem
	out = append(out, FilledItem{
		LinkID:    "conservative-therapy-weeks",
		Answer:    itoa(cc.ConservativeTherapyWeeks),
		Origin:    "auto",
		SourceRef: cc.ConservativeTherapyRef,
	})
	out = append(out, FilledItem{
		LinkID:    "neuro-deficit",
		Answer:    boolStr(cc.NeuroDeficit),
		Origin:    "auto",
		SourceRef: cc.NeuroDeficitRef,
	})
	out = append(out, FilledItem{
		LinkID:    "prior-imaging",
		Answer:    boolStr(cc.PriorImaging),
		Origin:    "auto",
		SourceRef: cc.PriorImagingRef,
	})
	if cc.PriorSurgery {
		out = append(out, FilledItem{
			LinkID:    "prior-surgery",
			Answer:    "true",
			Origin:    "auto",
			SourceRef: cc.PriorSurgeryRef,
		})
	}
	if cc.HighDisability {
		out = append(out, FilledItem{
			LinkID:    "high-disability",
			Answer:    "true",
			Origin:    "auto",
			SourceRef: cc.HighDisabilityRef,
		})
	}
	if cc.PatientReported {
		out = append(out, FilledItem{
			LinkID: "patient-reported-required",
			Answer: "true",
			Origin: "auto",
		})
	}
	return out
}

func itoa(n int) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = digits[n%10]
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// ---- Provider role ----

type scenarioReq struct {
	Branch string `json:"branch"`
}

type scenarioResp struct {
	Covered bool   `json:"covered"`
	Reason  string `json:"reason"`
}

func (g *Gateway) handleScenario(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req scenarioReq
	if tooLarge, err := shnsdk.DecodeJSONBody(w, r, &req); err != nil {
		if tooLarge {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
		} else {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		}
		return
	}

	// A participant-facing gateway rejects unknown branches rather than silently
	// treating anything non-"covered" as not-covered.
	// provider-data lane uses distinct UC-01 personas (MBR-PD-UC01/MBR-PD-UC01-NC) so
	// each scenario reads its OWN seeded Coverage; the demo lane uses its own distinct
	// pair (MBR-D-UC01/MBR-D-UC01-NC, §4.3) for the same reason; any other lane
	// keeps the shared MBR-COVERED/MBR-NOTCOVERED conformant-roster defaults
	// (sceneMember returns the default literal for neither provider-data nor demo).
	var memberID string
	var ok bool
	switch req.Branch {
	case "covered":
		memberID, ok = g.scenarioMember(w, r, "MBR-COVERED", "MBR-PD-UC01", "MBR-D-UC01")
	case "notcovered":
		memberID, ok = g.scenarioMember(w, r, "MBR-NOTCOVERED", "MBR-PD-UC01-NC", "MBR-D-UC01-NC")
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown branch"})
		return
	}
	if !ok {
		return
	}

	pci, _, found, readErr := ReadSystemOfRecord(g.cfg.SoR).ResolvePatientContext(ctx, memberID)
	if writeSoRFailure(w, readErr) {
		return
	}
	if !found {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown member"})
		return
	}

	// Read-ADD (FR-G40): UC-01 reads the member's OWN open Coverage as the routing/identity SOURCE
	// (the eligibility leg's recipient is resolved from realCov). The eligibility REQUEST itself
	// stays bare-insurer (BuildEligibilityRequest unchanged — insurer-coherence deferred this slice),
	// so this read does not change the payload bytes; it only fails closed when the member has no
	// coverage / no parseable payer on file (AI-G11 / OWD-G10).
	realCov, hasCov, status, msg := g.memberCoverage(ctx, memberID)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	if !hasCov {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "no coverage on file for member"})
		return
	}
	// Route the eligibility leg to the payer HOLDER resolved from the member's own Coverage
	// (FR-G40): no default — a miss (no parseable payer / no directory mapping) fails closed HERE
	// before any leg (AI-G11 / OWD-G10). The eligibility REQUEST stays bare-insurer (unchanged), so
	// UC-01 discards the parsed payer identity — it only routes.
	recipient, _, status, msg := g.recipientForSoR(ctx, realCov)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	cerJSON, err := g.eligibilityRequest(memberID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build request failed"})
		return
	}

	// Tag the leg BEFORE the egress validate below: that check now feeds the
	// choke point's finding (this is the site the migration to validateGoverned
	// routed through the choke point for the first time), so an untagged
	// context would surface as legType "unknown" on a live production path —
	// exactly the evidence-quality gap tagging exists to close. correlationID
	// is not minted until just before the Hub round trip further down, so it
	// is carried empty here rather than invented; the later tag below adds it
	// once real.
	ctx = withFindingContext(ctx, findingContext{
		LegType: "coverage-eligibility", Seam: "originate", Whose: "own",
	})

	// Egress validation is load-bearing: an invalid resource must never reach the
	// substrate. Empty profile = base-R4 + meta.profile pinning (see roundTrip).
	// F7: lane-selected per line like every other validate, but deliberately
	// NOT g.validateFHIR — this site echoes the choke point's bounded
	// govResult.Issues in its 422 body, which validateFHIR's (status,msg)
	// contract cannot carry. coverage-eligibility is
	// version-neutral, so the line is "" (the canonical lane).
	// Unconditional on purpose, not an oversight — relaysReferencePayerBytes does not apply
	// here. cerJSON is a request THIS
	// engine builds (shnsdk.BuildEligibilityRequest), never a relay of anyone else's bytes,
	// on every origination lane. Only the ingress/egress legs a LegResponder (the payer
	// content occupant — CRD/DTR/PAS) actually touches can carry reference-payer bytes;
	// eligibility never reaches that occupant's Handle (R11 — see gateway/engine/native.go's "NO
	// coverage-eligibility arm" comment).
	cerValidator := g.validatorForLine("")
	// Routed through the choke point (validateGoverned) so the level decides
	// whether the check runs and an invalid verdict emits its conformance
	// finding. This site's own status/message contract is preserved explicitly
	// below, including its own missing-lane text (gr.NoLane), never relayed from
	// the choke point's generic text.
	fc := findingContextFrom(ctx)
	fc.Whose = "own"
	if gr := g.validateGoverned(ctx, fc, cerValidator, cerJSON, "egress", "", "", false); gr.Status != 0 {
		if gr.NoLane {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "no FHIR validator lane configured (FR-36/FR-G29)"})
			return
		}
		if gr.Status == http.StatusInternalServerError {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "validator unavailable"})
			return
		}
		// The issues echo is now BOUNDED (findingIssuesShown + "and N more"),
		// where it was unbounded before the migration — see the PR body.
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "egress validation failed",
			"issues": gr.Issues,
		})
		return
	}

	// Generate the correlationID BEFORE authorizing so the token is bound to the
	// exact envelope it will ride in (C2): token.CorrelationID == envelope CID.
	correlationID := g.cfg.CorrelationGen()
	ctx = withFindingContext(ctx, findingContext{
		LegType: "coverage-eligibility", CorrelationID: correlationID, Seam: "originate", Whose: "own",
	})

	// UC-01 uses the SAME authorized-sealed-leg helper as UC-02/03 (OriginateLeg →
	// roundTrip): authorize(eligibility-inquiry) → seal → Hub /route → verify the response leg
	// (respOp eligibility-response, bound to this correlationID, Sender=="payer",
	// subject==pci) → decrypt. Folding UC-01 onto the shared helper keeps the
	// trust-critical response-leg verification in ONE place — no duplicated copy to
	// drift.
	crrJSON, err := g.OriginateLeg(ctx, r, recipient, "coverage-eligibility", pci, correlationID, "", Content{WorkstreamType: workstreamPA, Payload: sealRequest(relay.BuilderSDKEligibility, cerJSON, "application/fhir+json")})
	if err != nil {
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	// Ingress-validate the decrypted response (load-bearing). A payer returning an
	// invalid CRR is an UPSTREAM failure → 502 (preserves the UC-01 contract; only
	// the response-leg token verification was folded into roundTrip (via OriginateLeg),
	// not the validation-status semantics).
	// F7: lane-selected, still NOT g.validateFHIR — an invalid payer answer is a 502
	// here (an UPSTREAM failure), not validateFHIR's 500/422. Version-neutral leg ⇒
	// line "" ⇒ the canonical lane; a missing lane keeps THIS site's 502.
	// Unconditional on purpose here too. crrJSON is the CoverageEligibilityResponse the
	// payer's gateway built from its SoR's Coverage read (R11), or, for a payer that
	// declares its own eligibility endpoint, the payer system's own answer relayed; either
	// way it is a CoverageEligibilityResponse this requester validates as it would a direct
	// answer.
	crrValidator := g.validatorForLine("")
	// Routed through the choke point so an invalid payer answer still emits its
	// conformance finding. This site answers the scenario caller, which reads a
	// payer-side failure as a bad gateway — every outcome here is 502
	// (unchanged from before the migration); the choke point's own status
	// (500/422) is never relayed, only its message, which already matches this
	// site's literals byte-for-byte in both cases.
	fc = findingContextFrom(ctx)
	fc.Whose = "peer"
	if gr := g.validateGoverned(ctx, fc, crrValidator, crrJSON, "ingress", "", "", false); gr.Status != 0 {
		msg := gr.Msg
		if gr.NoLane {
			msg = "no FHIR validator lane configured (FR-36/FR-G29)"
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": msg})
		return
	}
	covered, reason, err := shnsdk.ParseEligibilityResponse(crrJSON)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "response parse failed"})
		return
	}

	writeJSON(w, http.StatusOK, scenarioResp{Covered: covered, Reason: reason})
}

type uc02Resp struct {
	Covered     string `json:"covered"`
	PARequired  bool   `json:"paRequired"`
	NeedsDTR    bool   `json:"needsDTR"`
	CardSummary string `json:"cardSummary"`
}

type uc03Resp struct {
	PARequired bool   `json:"paRequired"`
	AuthNumber string `json:"authNumber,omitempty"`
	ValidUntil string `json:"validUntil,omitempty"`
	// AuthNumber/ValidUntil/QRItems are omitempty, and an approval always carries a
	// non-empty AuthNumber. A PENDED response is now a real answer on this surface
	// (Decision/Continuation below): the payer has not decided yet, and the caller
	// continues the decision rather than being told the request failed.
	QRItems       []FilledItem `json:"qrItems,omitempty"`
	PendedItems   []string     `json:"pendedItems,omitempty"`
	AmendmentCorr string       `json:"amendmentCorr,omitempty"` // UC-04/06: the pas-claim-update corrId — proves the amendment leg ran (C4)
	Attested      bool         `json:"attested,omitempty"`      // UC-06/07: clinician/patient attestation applied (C4 UC-06 distinctive)
	// QRAnswers surfaces answer values keyed by the questionnaire linkId across the provider-data
	// scenarios, with provenance that DIFFERS by family — never conflate them:
	//   - HomeOxygen: operated-$populate COMPUTED values (the native populator drops FilledItem
	//     attribution, so they are read straight off the returned QR). This is the crux evidence the
	//     $populate ran br-payer's real prepop CQL against the seeded observations (2.2=86 O₂-sat /
	//     2.3=54 PaO₂), not an answer book. Empty when no quantity answers populated (e.g. aged-out obs).
	//   - UC-04 / UC-06: ORG-ATTESTED-FROM-SEED base values (1.1 service category / 3.1 dx, attested
	//     from the seeded order — the adaptive HomeHealthAssessment is 0-CQL, so $populate auto-pops
	//     nothing). These are traces-to-seed evidence, NOT CQL-computed; the QR is verdict-INERT
	//     (br-payer's A4→A1 is its pend-resolution timer).
	QRAnswers map[string]string `json:"qrAnswers,omitempty"`

	// The payer's own determination, and the capability that continues it.
	//
	// Decision is approved, denied or pended — the three answers a payer gives,
	// reported as what they are rather than as "approved, or an error". Denied and
	// Rationale carry the payer's own refusal in the vocabulary the denial surface
	// already uses. Pended with a Continuation means the payer has not decided
	// yet and this authorization can be continued; the caller resumes it with
	// POST /scenario/pa/inquire.
	//
	// ContinuationDurable is FALSE when this deployment keeps continuations in
	// memory only, so a restart loses them. It is a disclosure the surfaces show,
	// not a warning about a fault.
	// ContinuationDurable is a POINTER on purpose. Absent means "there is no
	// continuation to say anything about"; present-and-false is the disclosure
	// that this deployment keeps continuations in memory only, and the surfaces
	// show a notice on it. A plain bool with omitempty would make the disclosure
	// indistinguishable from its own absence — and it is the non-durable shapes,
	// where false is the truth, that most need to say so.
	Decision            string `json:"decision,omitempty"`
	Denied              bool   `json:"denied,omitempty"`
	Rationale           string `json:"rationale,omitempty"`
	Pended              bool   `json:"pended,omitempty"`
	Continuation        string `json:"continuation,omitempty"`
	ContinuationDurable *bool  `json:"continuationDurable,omitempty"`
}

// crdDtrResult carries the outputs of the CRD+DTR prefix shared by UC-03/04/06.
type crdDtrResult struct {
	qrSource       *dtrBuildSource
	qrJSON, srJSON []byte
	// questionnaireJSON is the bare Questionnaire extracted from the fetched $questionnaire-package
	// (nil on the no-DTR paths). The provider-data UC-04 lane re-fills it by attestation when the
	// operated $populate auto-pops nothing (the adaptive HomeHealthAssessment has 0 CQL items).
	questionnaireJSON       []byte
	patientRef, coverageRef string
	// coverage is the member's OWN Coverage record, read once from the system of
	// record at the fresh origination site — the same read that resolves the payer
	// identity and the route. It rides the PAS request as the resolvable entry the
	// Claim names, because the payer locates the policy from it and matches a later
	// inquiry against the coverage it stored; a coverage the SDK minted would be a
	// record no inquiry built from this participant's own system could name again.
	coverage []byte
	// insurer is the payer's OWN Organization record — the one that coverage names
	// as payor, read from the same system. The PAS request carries it as the entry
	// its Claim.insurer and its Coverage.payor both name, because the payer scopes
	// a later inquiry's search by the insurer and never re-homes an organization
	// carrying a plan identifier: a submission naming a minted payer organization
	// is one this participant's own inquiry can never match.
	insurer []byte
	// member is the BARE member id this exchange originated for — the value the
	// Patient carries as its member identifier and the one a payer matches an
	// inquiry on. It rides beside coverageRef, which stays the Reference-shaped
	// value the QR-context / native-lane roles need.
	member string
	// memberSystem is the namespace the participant's own system names that
	// member under, carried from the ONE reading of their Patient this flow
	// made. The PAS builders need it: a payer matches a prior authorization on
	// the member id.
	memberSystem string
	pci          string
	filled       []FilledItem
	// payer is the member's REAL payer identity, parsed from the member's open Coverage
	// (OpenCoverage → ParsePayerIdentifier) at the fresh origination site (FR-G40). It threads
	// to the payer-org-emitting PAS builders so the payload's payer derives from the patient's
	// real Coverage, not a synthetic CMS literal.
	payer shnsdk.PayerIdentifier
	// recipient is the payer HOLDER id every leg of this exchange routes to, resolved from the
	// member's real Coverage via recipientFor at the fresh origination site (FR-G40). There is NO
	// default — a miss fails closed before any leg (AI-G11 / OWD-G10). It replaced the deleted
	// Config.CounterpartID at the PAS-tail / resume sites (res.recipient).
	recipient string
	// crdOrder is the order the payer returned with its coverage information
	// (the CRD answer's update action), the order the questionnaire step works
	// from; nil when the payer returned none. crdAssertionID is the payer's
	// coverage-assertion-id for it ("" when none).
	crdOrder       []byte
	crdAssertionID string
	// dtrLine is the contract-version line selected at the DTR-fetch leg
	// (select-before-build, F7) — "" on the no-DTR path. Threaded to every authored-QR build
	// site downstream (the managed Populator call, and the provider-data attestation refills
	// at handleUC04 / scenarioToPend) so the QR they build matches the line the fetch already
	// committed to (closes an earlier KNOWN GAP where every authored QR was frozen at
	// the 2.0 shape regardless of the selected line). The sibling contract-version TOKEN
	// (route.Token) selected at the same leg is not separately retained — no downstream
	// consumer reads it; re-derive it from dtrLine
	// (shnsdk contract "pa.dtr@"+dtrLine) if a future caller needs the full token.
	dtrLine string
}

// orderSource returns the origination order bytes: the member's OPEN ORDER, read
// from the participant's own system of record. EVERY lane reads it there.
//
// It used to build the order from the per-UC tuple on every lane but
// provider-data. That order was held in no participant's system, so the
// Claim/$inquire that continues a pended authorization — which re-reads the
// order before it asks the payer anything — found nothing and refused its own
// inquiry: an authorization that pended could never resolve. A record no
// participant's system holds is minted, and nothing downstream can make it real
// later, so the order is read rather than authored.
//
// The tuple is still the scenario's own statement of WHICH product this
// origination is about, and it is checked against the order the system holds
// (wantCode ""(empty) states none and skips the check — the provider-data lanes
// take the order entirely from the data). A disagreement means the request is
// about an order this participant does not have, which is refused rather than
// originated as something else.
//
// Returns (orderJSON, httpStatus, msg); status 0 == ok.
func (g *Gateway) orderSourceContext(ctx context.Context, member, wantSystem, wantCode string) ([]byte, int, string) {
	order, ok, readErr := ReadSystemOfRecord(g.cfg.SoR).OpenOrderContext(ctx, member)
	if readErr != nil {
		status, msg := SoRFailureResponse(readErr)
		return nil, status, msg
	}
	if !ok {
		return nil, http.StatusBadGateway, "no open order for member in SoR"
	}
	// The product coding comes from the DATA (ServiceRequest.code / DeviceRequest
	// codeCodeableConcept), never a literal — fail closed if it carries no {CPT,HCPCS} coding.
	system, code, _, err := shnsdk.ParseOrderProductCoding(order)
	if err != nil {
		return nil, http.StatusBadGateway, "open order has no recognized product coding"
	}
	if wantCode != "" && (system != wantSystem || code != wantCode) {
		return nil, http.StatusBadGateway, fmt.Sprintf(
			"this origination is about %s|%s and the member's open order is %s|%s", wantSystem, wantCode, system, code)
	}
	return order, 0, ""
}

// OrderingProviderRef is the participant's OWN requesting-provider Organization
// in its system of record — the party an order this gateway AUTHORS is requested
// by, and therefore the party the prior authorization for it names.
//
// It is a reference into the participant's own system, resolved there like any
// other; this gateway states it, it does not carry the record. The value is
// stated here because this module cannot import the repository that seeds it —
// the same reason the Kit restates the reference payer's questionnaire
// canonicals — and exactly one row binds the two (the platform's
// TestOrderingProviderRefMatchesSeed).
const OrderingProviderRef = "Organization/org-ordering-provider"

// nameOrderPerformer states an authored order's performer. ServiceRequest.performer
// is a LIST (a DeviceRequest's is a single reference), and this only ever writes
// the list form because this is the only site that authors a ServiceRequest.
func nameOrderPerformer(order []byte, ref string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(order, &m); err != nil {
		return nil, err
	}
	performer, err := json.Marshal([]map[string]string{{"reference": ref}})
	if err != nil {
		return nil, err
	}
	m["performer"] = performer
	return json.Marshal(m)
}

// sceneMember returns the distinct provider-data persona member for a scenario under
// provider-data, the distinct demo persona (MBR-D-UC0N, §4.3) under demo, or the
// conformant-roster default otherwise. Distinct members are REQUIRED in the provider-data
// lane: the order is read via OpenOrder(member) (keyed on member ONLY), so two scenarios
// sharing a member would read the same order. The demo lane does not read SoR orders (it
// builds from the originationCodes tuple), but still needs its OWN member so each scenario
// resolves its own seeded Coverage and doesn't collide with another demo scenario's canary
// twin. Any other lane keeps the default member. A caller for which demo carries no
// distinct persona (edge-case routing proofs, the visualization-only bridge persona)
// passes the same literal for all three.
func (g *Gateway) sceneMember(defaultMember, providerDataMember, demoMember string) string {
	switch {
	case g.cfg.OriginationProfile == "provider-data":
		return providerDataMember
	case isDemoProfile(g.cfg.OriginationProfile):
		return demoMember
	default:
		return defaultMember
	}
}

// scenarioMember resolves the effective member for a /scenario/* request: the
// lane's sceneMember default, remapped to its dedicated canary twin when the
// request carries ?personaSet=canary (observability Phase 3, settled decision
// #1 — the monitor canary must never drive the shared demo personas). Fails
// closed 400 on an unknown personaSet value or a member with no twin: a typo
// silently running the demo personas is exactly the collision the twins exist
// to prevent. ok=false means the response is already written.
func (g *Gateway) scenarioMember(w http.ResponseWriter, r *http.Request, defaultMember, providerDataMember, demoMember string) (string, bool) {
	member := g.sceneMember(defaultMember, providerDataMember, demoMember)
	switch ps := r.URL.Query().Get("personaSet"); ps {
	case "":
		return member, true
	case "canary":
		twin, ok := CanaryTwins[member]
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no canary twin for member " + member})
			return "", false
		}
		return twin, true
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown personaSet " + ps})
		return "", false
	}
}

// runCRDThenDTROrder is the generalized CRD order-select + DTR fetch + auto-fill prefix.
// The order's {system, code, display, dx} are explicit so a HCPCS scenario can
// originate an L8000 order; existing callers delegate with the CPT lumbar order (byte-unchanged).
// On any failure it writes the HTTP error and returns ok=false.
// proceedOnNotCovered (the handleUC08 caller): when true, a not-covered CRD verdict does NOT
// terminally stop — the order is returned so the caller can carry it to PAS for br-payer's
// formal A2 "Not Certified" ClaimResponse (D-S2-2). handleUC08 passes targetsBrPayer(profile)
// || isDemoProfile(profile) — true for provider-data (the live br-payer lane) AND demo (the
// in-process mirror of the SAME br-payer family), since both target br-payer's J3490
// NOT-COVERED family. The generic FR-G25/AI-1 STOP (false) is the DEFAULT for every other
// caller, and — after gateway/app.go's loadConfig normalization — for every profile that
// is neither provider-data nor demo (an unset
// ORIGINATION_PROFILE is normalized to "demo" before the engine ever sees it, so it no longer
// reaches this STOP path in a real deployment). The opt-in never yields an auth on a denial:
// handleUC08 asserts the PAS result is DENIED (its approved→502 guard), so a not-covered order
// routed to PAS can only deny, never approve.
func (g *Gateway) runCRDThenDTROrder(w http.ResponseWriter, r *http.Request, member, system, code, display, dx string, proceedOnNotCovered bool) (crdDtrResult, bool) {
	ctx := r.Context()

	pci, _, found, readErr := ReadSystemOfRecord(g.cfg.SoR).ResolvePatientContext(ctx, member)
	if writeSoRFailure(w, readErr) {
		return crdDtrResult{}, false
	}
	if !found {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown member"})
		return crdDtrResult{}, false
	}
	patientRef := "Patient/" + member
	coverageRef := "Coverage/" + member

	// realCov is the member's OWN open Coverage — the routing/identity SOURCE (FR-G40). Its parsed
	// payer identity feeds the egress builders so the payload's payer derives from the patient's
	// real Coverage, not a synthetic CMS literal. realCov stays a LOCAL (never an egress payload,
	// never stored on crdDtrResult); the recipient is resolved from it inline at this site.
	realCov, found, status, msg := g.memberCoverage(ctx, member)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return crdDtrResult{}, false
	}
	if !found {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "no coverage on file for member"})
		return crdDtrResult{}, false
	}
	// Resolve the payer HOLDER every leg of this exchange routes to AND the parsed payer identity in
	// ONE parse of the member's own Coverage (FR-G40): no default — a miss fails closed HERE before
	// any leg (AI-G11 / OWD-G10). `payer` (the parsed identity) threads to the PAS builders below, so
	// routed-payer and payload-payer cannot diverge (one payer fact, read once).
	recipient, payer, status, msg := g.recipientForSoR(ctx, realCov)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return crdDtrResult{}, false
	}
	// And that payer's own Organization record, from the same system: the
	// submission names it, and so does every inquiry about the submission.
	realPayerOrg, status, msg := g.memberPayerOrganization(ctx, realCov)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return crdDtrResult{}, false
	}

	srJSON, status, msg := g.orderSourceContext(ctx, member, system, code)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return crdDtrResult{}, false
	}
	// The order retains the identity and the bytes the participant's own system
	// supplied. Nothing gives it a local one here any more: an id this gateway
	// assigned would name a record the system does not hold under that name, and
	// the inquiry that continues a pended authorization re-reads the order by the
	// reference it was submitted under.
	//
	// The CRD request carries the participant's own Patient and Coverage search
	// result (originate_crd.go); an order read from the system of record names the
	// patient the same way. The Coverage is read twice — above for routing
	// (memberCoverage), here as the search result the request carries — so each
	// read keeps its own refusal rules.
	recs, status, msg := g.originCRDRecords(ctx, "crd-order-select", member)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return crdDtrResult{}, false
	}
	// Name the patient in the order the way the request names them — every lane,
	// because every lane's order now comes from the system of record.
	named, err := originOrder(recs, srJSON)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "name the patient in the open order: " + err.Error()})
		return crdDtrResult{}, false
	}
	srJSON = named
	// This helper owns the crd-order-select leg (and, below, dtr-questionnaire-fetch)
	// regardless of which caller's headline leg dispatched here — retag rather than
	// inherit, so a finding from this check never borrows the caller's leg name.
	ctx = withFindingContext(ctx, findingContext{
		LegType: "crd-order-select", Seam: "originate", Whose: "own",
	})
	if status, msg := g.validateFHIR(ctx, srJSON, "egress", ""); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return crdDtrResult{}, false
	}

	// --- CRD round-trip: must come back PA-required with a canonical. ---
	// Select-before-build. The CRD builder is LINE-INERT — pa.crd
	// is identity across 2.0/2.1/2.2 (compat.go's live-derived rows), so
	// route.BuildLine changes not one byte below — but the ROUTING axis is real
	// and was never adjudicated: sitting on OriginateLeg's arm-1-only
	// empty-ProfileID backfill meant a pa.crd@2.2-only peer (legitimate:
	// new-from-birth or foreign holders) was REFUSED by a binary that serves it
	// byte-identically. Selecting here puts the leg on the same reachability
	// arms (native reach, then transform chain) every other contract-mapped leg
	// already uses. crdCorr is hoisted so selection, adaptation and the leg
	// itself all share one correlation.
	crdCorr := g.cfg.CorrelationGen()
	crdRoute, ok := g.selectLegLineOrFail(w, recipient, "crd-order-select", crdCorr)
	if !ok {
		return crdDtrResult{}, false
	}
	// The order is signed and headed for prior authorization: order-sign.
	crdReq, err := g.originatedOrderingRequest(hookOrderSign, crdCorr, recs, srJSON)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "build order-sign request failed: " + err.Error()})
		return crdDtrResult{}, false
	}
	// Validation posture is UNCHANGED by the promotion (D-7 is routing-only):
	// there is deliberately NO validateFHIR enforcement point after egressAdapt
	// on this leg. crdReq is CDS Hooks JSON — a transport ENVELOPE, not itself a
	// FHIR resource the pa.crd compat-manifest rows model. The order it carries
	// is $validated above; the Patient and Coverage are the participant's own
	// records, relayed as its system holds them and fenced to the member.
	adaptedCRDReq, _, err := g.egressAdapt(crdRoute, crdReq, ExchangeIdentity{CorrelationID: crdCorr, LegType: "crd-order-select", Counterpart: recipient})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return crdDtrResult{}, false
	}
	crdRespJSON, err := g.OriginateLeg(ctx, r, recipient, "crd-order-select", pci, crdCorr, "",
		Content{WorkstreamType: workstreamPA, ProfileID: crdRoute.Token, Route: routeInfoFor(crdRoute), Payload: sealRequest(relay.BuilderSDKCRDRequest, adaptedCRDReq, "application/json")})
	if err != nil {
		if g.relayOriginationError(w, err) {
			return crdDtrResult{}, false
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return crdDtrResult{}, false
	}
	answer, err := readOriginatedAnswer(crdRespJSON, srJSON)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "card parse failed"})
		return crdDtrResult{}, false
	}
	cov := answer.coverage
	// Generic verdict-driven switch on the CRD card result (FR-G25). A
	// config-only gateway must handle ANY conformant CRD verdict it observes, not just
	// one fixed shape (always covered + PA + DTR). The PA decision keys on the
	// pa-needed axis ONLY (PARequired); whether a DTR questionnaire is fetched is decided
	// separately by the doc-needed axis (NeedsDTR), below. Every non-proceeding value is
	// handled fail-closed with no silent fall-through. AI-1: a coverage denial STOPS,
	// never proceeds silently.
	switch {
	case cov.Covered == shnsdk.CoveredNotCovered:
		if proceedOnNotCovered {
			// provider-data UC-08 (D-S2-2): a not-covered CRD verdict IS the denial
			// scenario; carry the order to PAS for br-payer's formal A2 "Not Certified"
			// ClaimResponse + rationale (not-covered → A2). This does NOT weaken FR-G25/AI-1:
			// handleUC08 asserts the PAS result is DENIED (502 on any approval), so a
			// not-covered order can never yield an auth. Not-covered carries no questionnaire
			// (NeedsDTR=false) → return the built order straight for the PAS submit.
			return crdDtrResult{srJSON: srJSON, patientRef: patientRef, coverageRef: coverageRef, coverage: realCov, insurer: realPayerOrg, member: member, memberSystem: recs.memberSystem, pci: pci, payer: payer, recipient: recipient}, true
		}
		// AI-1: a coverage denial STOPS — never routes DTR/PAS. (adversarial Row 1)
		// Explicit terminal stop; patient-facing denial UX is deferred.
		writeJSON(w, http.StatusOK, map[string]any{"paRequired": false, "covered": false, "outcome": "not-covered"})
		return crdDtrResult{}, false
	case cov.PANeeded == shnsdk.PANeededSatisfied:
		// TYPE-ready (SatisfiedPaID carried); the proceed-with-existing-auth
		// short-circuit is a new terminal-success path, deferred fail-closed this
		// slice. Distinct message — a real conformant payer is most likely to hit
		// this branch.
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "PA already satisfied — short-circuit not yet implemented"})
		return crdDtrResult{}, false
	case !cov.PARequired():
		// Generic: PA decision keys on the pa-needed axis. No PA (incl.
		// pa-needed:conditional, which is NOT auth-needed/performpa) ⇒ this prefix
		// (UC-03+) has nothing to submit; the no-PA path is UC-02's handleUC02, not here.
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "expected PA-required card"})
		return crdDtrResult{}, false
	}
	// PA is required (covered OR conditional coverage both proceed). Whether a
	// DTR questionnaire is fetched is decided by the doc-needed axis (NeedsDTR) —
	// whether the CARD advertised a questionnaire — independently of the PA axis.
	// Both are live: the reference payer advertises one on the L8000 and G0151
	// families (so both route DTR) and none on E0424, which is PA-decided off the
	// request alone and goes straight to PAS.
	res := crdDtrResult{srJSON: srJSON, patientRef: patientRef, coverageRef: coverageRef, coverage: realCov, insurer: realPayerOrg, member: member, memberSystem: recs.memberSystem, pci: pci, payer: payer, recipient: recipient,
		crdOrder: answer.updatedOrder, crdAssertionID: answer.assertionID}
	if cov.NeedsDTR() {
		canonical := shnsdk.StripCanonicalVersion(cov.Questionnaires[0])

		// SELECT-BEFORE-BUILD: the token/line is resolved
		// BEFORE the DTR fetch REQUEST is built, not just before the PACKAGE that comes
		// back — a DTR line whose $questionnaire-package input profile makes `coverage`
		// 1..1 (2.2 — DTRDef.QuestionnairePackageCoverageRequired, verified live
		// 2026-08-12) needs the line to gate whether Coverage is attached below, so the
		// request is no longer fully line-invariant the way it once was (only
		// the br-payer-targeting profile varied it). This is still the line the answer
		// is $validate-checked against (F7).
		dtrCorr := g.cfg.CorrelationGen()
		route, ok := g.selectLegLineOrFail(w, recipient, "dtr-questionnaire-fetch", dtrCorr)
		if !ok {
			return crdDtrResult{}, false
		}
		res.dtrLine = shnsdk.LineOf(route.Token)

		// --- DTR round-trip: fetch Questionnaire, validate, auto-fill locally. ---
		// The request is the $questionnaire-package operation's own input
		// (originatedPackageRequest): the system of record's Coverage search
		// result, the order as the payer returned it, the questionnaire canonical
		// as the payer stated it and the payer's coverage-assertion-id as context.
		// It is sent in a request frame naming the operation, and only to a payer
		// that declares framed DTR operations. The questionnaire request is the
		// same at every line apart from the input profile's own cardinalities,
		// which the builder checks at the selected line; the walk to the payer's
		// line changes no byte of it (envelopeEgressLegs).
		if status, msg := g.framedDTRRefusal(recipient); status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return crdDtrResult{}, false
		}
		dtrReq, dtrPayload, err := originatedPackageRequest(res.dtrLine, recs, res.dtrOrder(), cov.Questionnaires[0], res.crdAssertionID)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "build questionnaire-package request failed: " + err.Error()})
			return crdDtrResult{}, false
		}
		if status, msg := g.carryUnchanged(route, dtrReq, dtrCorr, recipient); status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return crdDtrResult{}, false
		}
		packageJSON, err := g.OriginateLeg(ctx, r, recipient, "dtr-questionnaire-fetch", pci, dtrCorr, "",
			Content{WorkstreamType: workstreamPA, ProfileID: route.Token, Route: routeInfoFor(route), Payload: dtrPayload,
				Operation: shnsdk.FrameOperationQuestionnairePackage})
		if err != nil {
			if g.relayOriginationError(w, err) {
				return crdDtrResult{}, false
			}
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return crdDtrResult{}, false
		}
		ctx = withFindingContext(ctx, findingContext{
			LegType: "dtr-questionnaire-fetch", CorrelationID: dtrCorr, Seam: "originate", Whose: "peer",
		})
		if status, msg := g.validateFHIRPayerIngress(ctx, packageJSON, res.dtrLine, "pa.dtr"); status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return crdDtrResult{}, false
		}

		// The DTR-fetch leg carries the full $questionnaire-package collection
		// Bundle (its dependent Libraries/ValueSets survive the wire for Step 3, which
		// will read them from packageJSON here). Extract the bare Questionnaire for the
		// F5 canonical check + auto-fill. A package with no Questionnaire is a partner
		// fault → 502 (the guard relocated from native.go's producer-side extract).
		questionnaireJSON, err := extractQuestionnaireFromPackage(packageJSON)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "fetched questionnaire package has no Questionnaire"})
			return crdDtrResult{}, false
		}

		// F5: verify the fetched Questionnaire's url matches the canonical the payer
		// advertised in the CRD card. A mismatch means the payer returned a different
		// questionnaire than the card claimed — reject to prevent canonical substitution.
		fetchedURL, err := shnsdk.ParseQuestionnaireURL(questionnaireJSON)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "fetched questionnaire url parse failed"})
			return crdDtrResult{}, false
		}
		if fetchedURL != canonical {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "fetched questionnaire does not match advertised canonical"})
			return crdDtrResult{}, false
		}

		// The operated $populate engine reads the FHIR store directly, so its subject must be the
		// store-resolvable Patient ref (a scoped id), NOT the logical SHN ref: the system of
		// record's Patient reference, read once for the CRD request.
		subjectFHIRRef := "Patient/" + recs.sorID
		// The questionnaire works from the order as the payer returned it (with its
		// coverage information) when the payer returned one, else the order sent.
		orderRef, ok := resourceRef(res.dtrOrder())
		if !ok {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "order missing id"})
			return crdDtrResult{}, false
		}
		authored := g.cfg.Clock()
		qrJSON, fill, err := g.cfg.Populator.Populate(ctx, packageJSON, PopulateContext{
			Member:         member,
			PatientRef:     patientRef,
			SubjectFHIRRef: subjectFHIRRef,
			CoverageRef:    coverageRef,
			OrderRef:       orderRef,
			Order:          res.dtrOrder(),
			Authored:       authored,
			Line:           res.dtrLine,
		})
		if err != nil {
			writeJSON(w, statusForPopulateErr(err), map[string]string{"error": messageForPopulateErr(err)})
			return crdDtrResult{}, false
		}
		// QR-SUBJECT FENCE — uniform across backends, compared against the LOGICAL PatientRef. Managed
		// fills with PatientRef directly. The native backend reads the FHIR store by the (possibly
		// scoped) SubjectFHIRRef, verifies the returned QR is about THAT patient, and normalizes
		// QR.subject → PatientRef before returning — so by here both backends present the logical ref.
		// (Comparing against the scoped SubjectFHIRRef here would wrongly reject the managed+real-SoR
		// combination, where managed sets the logical ref but the SoR resolves a scoped id.) 502.
		if subj, serr := questionnaireResponseSubject(qrJSON); serr != nil || subj != patientRef {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "populated QR subject does not match patient"})
			return crdDtrResult{}, false
		}
		// QR-QUESTIONNAIRE FENCE — uniform across backends. F5 (above) checks the FETCHED
		// Questionnaire's url before the seam; it never sees the returned QR's self-declared
		// `questionnaire`. A native engine can return a QR for a DIFFERENT questionnaire. Reject
		// any QR whose questionnaire (url-part) ≠ canonical. 502.
		if qq, qerr := questionnaireResponseCanonical(qrJSON); qerr != nil || qq != canonical {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "populated QR questionnaire does not match canonical"})
			return crdDtrResult{}, false
		}

		ctx = withFindingContext(ctx, findingContext{
			LegType: "dtr-questionnaire-fetch", CorrelationID: dtrCorr, Seam: "originate", Whose: "own",
		})
		if status, msg := g.validateFHIRForContract(ctx, qrJSON, "egress", "pa.dtr", res.dtrLine, baseQRProfile); status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return crdDtrResult{}, false
		}

		res.qrSource = newRawDTRBuildSource(qrJSON, questionnaireJSON, shnsdk.QRContext{PatientRef: patientRef, CoverageRef: coverageRef, OrderRef: orderRef, Authored: authored})
		qrJSON, status, msg := g.completeDTRContext(ctx, qrJSON, res.dtrLine, shnsdk.QRContext{PatientRef: patientRef, CoverageRef: coverageRef, OrderRef: orderRef})
		if status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return crdDtrResult{}, false
		}
		res.qrJSON = qrJSON
		res.filled = fill
		// Surface the fetched bare Questionnaire so a caller (provider-data UC-04) can re-fill it
		// by attestation when $populate auto-pops nothing (it stays nil on the no-DTR paths).
		res.questionnaireJSON = questionnaireJSON
	}

	return res, true
}

func isSoRPopulateError(err error) bool {
	var readErr *SoRReadError
	return errors.As(err, &readErr) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func messageForPopulateErr(err error) string {
	if isSoRPopulateError(err) {
		_, msg := SoRFailureResponse(err)
		return msg
	}
	return err.Error()
}

// statusForPopulateErr maps a Populator error to an HTTP status. A managed
// FillQuestionnaire marshal/unsupported error is the gateway's own fault → 500
// (behavior-preserving: the inline path returned 500 here, and this never trips on the
// 8 scenarios). errNoClinicalContext (a data fault) and errPopulateUpstream
// (a native $populate fault) are partner/data faults → 502.
func statusForPopulateErr(err error) int {
	if isSoRPopulateError(err) {
		status, _ := SoRFailureResponse(err)
		return status
	}
	switch {
	case errors.Is(err, errNoClinicalContext):
		return http.StatusBadGateway
	case errors.Is(err, errPopulateUpstream):
		return http.StatusBadGateway
	case errors.Is(err, errPopulateForeignSubject):
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

// questionnaireResponseSubject returns the QR's subject.reference.
func questionnaireResponseSubject(qrJSON []byte) (string, error) {
	var probe struct {
		Subject struct {
			Reference string `json:"reference"`
		} `json:"subject"`
	}
	if err := json.Unmarshal(qrJSON, &probe); err != nil {
		return "", err
	}
	return probe.Subject.Reference, nil
}

// setQuestionnaireResponseSubject rewrites the QR's subject.reference (JSON-level, preserving every
// other field/order). Used to normalize the operated engine's store-resolvable subject (a scoped
// FHIR id) back to the logical SHN ref after the QR-subject fence has verified it. On a parse
// failure it returns the input unchanged (the egress validate that follows then rejects it).
func setQuestionnaireResponseSubject(qrJSON []byte, ref string) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(qrJSON, &m); err != nil {
		return qrJSON
	}
	subject, err := dtrObject(m["subject"])
	if err != nil {
		return qrJSON
	}
	subject["reference"] = dtrRaw(ref)
	subj, err := json.Marshal(subject)
	if err != nil {
		return qrJSON
	}
	m["subject"] = subj
	out, err := json.Marshal(m)
	if err != nil {
		return qrJSON
	}
	return out
}

// questionnaireResponseNumericAnswers walks the QR's nested items and returns, for every item that
// carries a numeric answer, a {linkId → value} map (the value rendered as a string, e.g. "86").
// Used only by the provider-data HomeOxygen handler to surface the operated-$populate computed O₂
// values (linkIds 2.2/2.3) as the C1 crux evidence — the native populator drops per-item FilledItem
// attribution, so the values are read straight off the QR. It reads BOTH answer shapes: the operated
// HAPI CR engine emits a `valueDecimal` for a `quantity`-type item (verified live against the
// HomeOxygen questionnaire), while the managed/hermetic populator may emit a `valueQuantity`; either
// is accepted. An empty/aged-out QR (no numeric answers) yields an empty map.
func questionnaireResponseNumericAnswers(qrJSON []byte) map[string]string {
	var qr struct {
		Item []qrItemNode `json:"item"`
	}
	if err := json.Unmarshal(qrJSON, &qr); err != nil {
		return nil
	}
	out := map[string]string{}
	var walk func(items []qrItemNode)
	walk = func(items []qrItemNode) {
		for _, it := range items {
			for _, a := range it.Answer {
				switch {
				case a.ValueDecimal != nil:
					out[it.LinkID] = strconv.FormatFloat(*a.ValueDecimal, 'f', -1, 64)
				case a.ValueQuantity != nil && a.ValueQuantity.Value != nil:
					out[it.LinkID] = strconv.FormatFloat(*a.ValueQuantity.Value, 'f', -1, 64)
				}
				walk(a.Item)
			}
			walk(it.Item)
		}
	}
	walk(qr.Item)
	if len(out) == 0 {
		return nil
	}
	return out
}

// qrItemNode is the recursive QR item shape questionnaireResponseNumericAnswers reads (only the
// fields it needs: linkId, numeric answers — decimal or quantity — and nested items).
//
// Items nest on BOTH of FHIR's axes: under item.item and under item.answer.item,
// each contentReferencing back to QuestionnaireResponse.item. Answer.Item is
// what makes the second axis reachable; without it those items are discarded at
// unmarshal and the walk below cannot see them however it recurses.
type qrItemNode struct {
	LinkID string `json:"linkId"`
	Answer []struct {
		ValueDecimal  *float64 `json:"valueDecimal"`
		ValueQuantity *struct {
			Value *float64 `json:"value"`
		} `json:"valueQuantity"`
		Item []qrItemNode `json:"item"`
	} `json:"answer"`
	Item []qrItemNode `json:"item"`
}

// questionnaireResponseCanonical returns the URL-PART of the QR's `questionnaire`
// canonical (version stripped). The managed QR sets a VERSIONED canonical
// (e.g. ".../pa-lumbar-mri|1.0.0") while the F5 `canonical` is the bare url — so the
// fence compares url-parts, not the raw versioned string.
func questionnaireResponseCanonical(qrJSON []byte) (string, error) {
	var probe struct {
		Questionnaire string `json:"questionnaire"`
	}
	if err := json.Unmarshal(qrJSON, &probe); err != nil {
		return "", err
	}
	if i := strings.IndexByte(probe.Questionnaire, '|'); i >= 0 {
		return probe.Questionnaire[:i], nil
	}
	return probe.Questionnaire, nil
}

// handleUC02 runs the no-PA CRD round-trip: a covered member's order is CRD-checked and comes
// back covered with no prior-auth required. provider-data originates a seeded E0250 hospital-bed
// DeviceRequest (HospitalBeds → covered/no-PA/no-DTR); every other lane originates the per-UC
// tuple. Surfaces the covered/no-PA/no-DTR triple (NeedsDTR is the
// reasonCode discriminator, asserted by the live gate).
func (g *Gateway) handleUC02(w http.ResponseWriter, r *http.Request) {
	// UC-02 (no-PA) originates the seeded E0250 hospital-bed DeviceRequest off provider data
	// (the MBR-PD-UC02 persona) — D-PD-2 is dropped. The order is read via orderSource → OpenOrder
	// in the provider-data lane (it traces to the provider's seeded SoR, never a literal); a
	// mis-seeded member with no open order fails closed at OpenOrder. Every other lane originates
	// the no-PA order off the per-UC tuple (byte-unchanged).
	member, ok := g.scenarioMember(w, r, "MBR-COVERED", "MBR-PD-UC02", "MBR-D-UC02")
	if !ok {
		return
	}
	r = r.WithContext(withFindingContext(r.Context(), findingContext{
		LegType: "crd-order-select", Seam: "originate", Whose: "own",
	}))
	g.originateNoPACRD(w, r, member)
}

// handleUC02PayerB drives the SECOND-PAYER routing lane (FR-G41): the MBR-PD-UC02-PB persona's own
// Coverage names a DISTINCT payer identity (urn:oid:…300|00078) rather than the reference payer's
// 00001, so the provider gateway's FeedPayerRouter must resolve it off the /holders feed with no
// static per-provider directory — the many-to-many drop-in property. MBR-PD-UC02-PB is seeded into
// the provider tenant by cmd/fhirseed (shnsdk provider-data persona "uc02-payerb").
//
// WHAT IT PROVES DEPENDS ON THE DEPLOYMENT, and right now that is less than the name suggests: the
// second payer holder that claimed 00078 retired alongside the payer it was the counterpart to, so
// unless a deployment publishes a holder claiming 00078, this lane resolves nothing and fails closed
// 422 — the same answer handleUC02UnknownPayer asserts. It is kept because the persona, its seeded
// Coverage and the resolution path are all real: point 00078 at a live payer holder and the lane is a
// routing proof again, with no code change. It was never a proof of a distinct VERDICT — differential
// adjudication is a separate property.
func (g *Gateway) handleUC02PayerB(w http.ResponseWriter, r *http.Request) {
	member, ok := g.scenarioMember(w, r, "MBR-PD-UC02-PB", "MBR-PD-UC02-PB", "MBR-PD-UC02-PB")
	if !ok {
		return
	}
	r = r.WithContext(withFindingContext(r.Context(), findingContext{
		LegType: "crd-order-select", Seam: "originate", Whose: "own",
	}))
	g.originateNoPACRD(w, r, member)
}

// handleUC02UnknownPayer is the LIVE FeedPayerRouter fail-closed proof (AI-G11/AI-G12): the
// MBR-UNKNOWN-PAYER persona's Coverage names a payer identity (urn:oid:…300|00099) that NO holder
// claims in the feed, so the coverage-derived resolution fails closed with a legible 422 ("no
// registered payer for identifier …|00099") — never a default payer. Seeded into the provider tenant
// by cmd/fhirseed (a smoke-only negative persona, not an SDK partner-onboarding fixture).
func (g *Gateway) handleUC02UnknownPayer(w http.ResponseWriter, r *http.Request) {
	member, ok := g.scenarioMember(w, r, "MBR-UNKNOWN-PAYER", "MBR-UNKNOWN-PAYER", "MBR-UNKNOWN-PAYER")
	if !ok {
		return
	}
	r = r.WithContext(withFindingContext(r.Context(), findingContext{
		LegType: "crd-order-select", Seam: "originate", Whose: "own",
	}))
	g.originateNoPACRD(w, r, member)
}

// originateNoPACRD runs the no-PA CRD round-trip for member: read the member's OWN Coverage as the
// routing/identity source (FR-G40/G41 — the payer holder is resolved off Coverage.payor, no default),
// originate a crd-order-select leg, and surface the covered/no-PA/no-DTR triple. Shared by handleUC02
// (persona-A) and the second-payer / unknown-payer routing proofs.
func (g *Gateway) originateNoPACRD(w http.ResponseWriter, r *http.Request, member string) {
	ctx := r.Context()

	pci, _, found, readErr := ReadSystemOfRecord(g.cfg.SoR).ResolvePatientContext(ctx, member)
	if writeSoRFailure(w, readErr) {
		return
	}
	if !found {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown member"})
		return
	}
	o := originationCodes().uc02
	var srJSON []byte
	var status int
	var msg string
	if g.cfg.OriginationProfile == "provider-data" {
		// The coverage check is about an order still being chosen: the
		// member's draft order, as the system of record holds it.
		srJSON, status, msg = g.draftOrderContext(ctx, member)
	} else {
		// Every other lane reads the member's OPEN order from the same system.
		srJSON, status, msg = g.orderSourceContext(ctx, member, o.system, o.code)
	}
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	// Read the member's OWN open Coverage as the routing/identity SOURCE (FR-G40).
	// realCov stays a LOCAL (the recipient is resolved from it).
	realCov, hasCov, status, msg := g.memberCoverage(ctx, member)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	if !hasCov {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "no coverage on file for member"})
		return
	}
	// Resolve the CRD leg's payer HOLDER AND the parsed payer identity in ONE parse of the member's
	// own Coverage (FR-G40): no default — a miss fails closed HERE before the leg (AI-G11 / OWD-G10).
	// `payer` threads to the CRD coverage builder, so routed-payer and payload-payer cannot diverge.
	recipient, _, status, msg := g.recipientForSoR(ctx, realCov)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	// The order keeps the identity the participant's own system supplied, on
	// every lane (orderSource's comment).
	//
	// The CRD request carries the participant's own Patient and Coverage search
	// result (originate_crd.go); an order read from the system of record names the
	// patient the same way. The Coverage is read twice — above for routing
	// (memberCoverage), here as the search result the request carries — so each
	// read keeps its own refusal rules.
	recs, status, msg := g.originCRDRecords(ctx, "crd-order-select", member)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	named, err := originOrder(recs, srJSON)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "name the patient in the open order: " + err.Error()})
		return
	}
	srJSON = named
	if status, msg := g.validateFHIR(ctx, srJSON, "egress", ""); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	// Select-before-build — see runCRDThenDTROrder's site for the
	// full rationale: the CRD builder is line-inert, but the routing axis is
	// real, and the arm-1-only backfill refused peers this build serves
	// byte-identically. correlationID was already a named var; its assignment
	// moved up above the builder so selection, adaptation and the leg all share
	// the one correlation.
	correlationID := g.cfg.CorrelationGen()
	crdRoute, ok := g.selectLegLineOrFail(w, recipient, "crd-order-select", correlationID)
	if !ok {
		return
	}
	// A coverage check while the order is chosen, with no authorization to follow:
	// order-select.
	reqJSON, err := g.originatedOrderingRequest(hookOrderSelect, correlationID, recs, srJSON)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "build order-select request failed: " + err.Error()})
		return
	}
	// Validation posture UNCHANGED (routing-only promotion): reqJSON is a CDS
	// Hooks envelope, not a FHIR resource; the order it carries is $validated
	// above, the Patient and Coverage are the participant's own records. No
	// enforcement point is added or removed after egressAdapt.
	adaptedReqJSON, _, err := g.egressAdapt(crdRoute, reqJSON, ExchangeIdentity{CorrelationID: correlationID, LegType: "crd-order-select", Counterpart: recipient})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	respJSON, err := g.OriginateLeg(ctx, r, recipient, "crd-order-select", pci, correlationID, "",
		Content{WorkstreamType: workstreamPA, ProfileID: crdRoute.Token, Route: routeInfoFor(crdRoute), Payload: sealRequest(relay.BuilderSDKCRDRequest, adaptedReqJSON, "application/json")})
	if err != nil {
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	cov, err := crdCoverage(respJSON)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "card parse failed"})
		return
	}

	// The coverage view carries no card Summary; do a small inline parse of the
	// raw cards JSON to extract the first card's summary for the console response.
	var rawCards struct {
		Cards []struct {
			Summary string `json:"summary"`
		} `json:"cards"`
	}
	cardSummary := ""
	if json.Unmarshal(respJSON, &rawCards) == nil && len(rawCards.Cards) > 0 {
		cardSummary = rawCards.Cards[0].Summary
	}

	// Surface the CRD coverage-info TRIPLE — the live two-RI gate asserts UC-02 off
	// covered/no-PA/no-DTR (Covered=="covered" + PARequired()==false + NeedsDTR()==false),
	// NOT a card-summary string. The visible card is HospitalBeds' always-on "Document
	// medical necessity for hospital bed" guidance card, surfaced for logging only.
	// NeedsDTR()==false is the reasonCode-discriminator guard: the seeded E0250 carries a
	// reasonCode (Documentation Required=false) so br-payer returns no questionnaire — that
	// discriminator is held by the live two-RI gate's NeedsDTR==false hard assertion.
	writeJSON(w, http.StatusOK, uc02Resp{
		Covered:     cov.Covered,
		PARequired:  cov.PARequired(),
		NeedsDTR:    cov.NeedsDTR(),
		CardSummary: cardSummary,
	})
}

// handleUC03 runs the full PA-required path off the HomeOxygen family — CRD
// (order-dispatch, advisory card) → DTR fetch + operated $populate → the item-6.1
// requester attestation → PAS submit → approval. On approval the provider stores the
// auth number for the order reference (FR-23) and answers paRequired=true. Off
// provider-data, the request body selects the member the same way handleScenario's uc01
// does (scenarioReq.Branch): "" (or an absent body) is the demo-lane oxygen origination
// (§4.3/register §11 ruling (b)); "bridge-demo" and "bridge-refuse" are the
// kit-bridging-visualization demo's two arms (task2 brief A3a), sharing one body
// (handleUC03Bridge) — their OWN literal L8000/order-select exhibit, decoupled from this
// arm. "bridge-demo" drives MBR-BRIDGE-DEMO for the full bridged SUCCESS (CRD/DTR cross
// the version boundary via the transform chains, PAS lands on the shared line, a real
// payer-issued authNumber); "bridge-refuse" drives MBR-BRIDGE-REFUSE for the DESIGNED
// refusal (the peer's skewed PAS declaration refuses the pas-claim leg's contract-version
// line-select before any bundle is ever built) — see selectLegLineOrBridgeRefuse for how
// that refusal is surfaced; any other value 400s.
func (g *Gateway) handleUC03(w http.ResponseWriter, r *http.Request) {
	if g.cfg.OriginationProfile == "provider-data" {
		// UC-03 off provider data = the HomeOxygenDispatch path (the only br-payer family that PA-requires
		// + launches a DTR + GENUINELY auto-fills it via operated $populate). E1390 vs HomeOxygen's E0431;
		// the approval is br-payer's pend-resolution timer (D-2RI-3), and the genuine auto-fill (the $populate
		// runs the real prepop CQL against the seeded O2 obs) is UC-03's distinctive. The implicit fail-closed
		// at OpenOrder is preserved (a mis-seeded MBR-PD-UC03 with no E1390 order fails closed there).
		member, ok := g.scenarioMember(w, r, "MBR-PD-UC03", "MBR-PD-UC03", "MBR-D-UC03")
		if !ok {
			return
		}
		r = r.WithContext(withFindingContext(r.Context(), findingContext{
			LegType: "crd-order-dispatch", Seam: "originate", Whose: "own",
		}))
		g.originateDispatch(w, r, member)
		return
	}

	// The UC-03 branch selection rides a dedicated req.Branch switch rather
	// than the persona set (personaSet=canary cannot carry it), mirroring
	// handleScenario's UC-01 shape and decoded/validated the same way (an empty
	// or absent body decodes to the zero Branch "", handleScenario's own
	// idiom — DecodeJSONBody on nil/empty body returns io.EOF, tolerated
	// below so every existing nil-body uc03 caller in this package's test
	// suite, and any live client that never sent a body, keeps behaving
	// byte-identically).
	var req scenarioReq
	if tooLarge, err := shnsdk.DecodeJSONBody(w, r, &req); err != nil && !errors.Is(err, io.EOF) {
		if tooLarge {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
		} else {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		}
		return
	}

	switch req.Branch {
	case "":
		g.handleUC03Oxygen(w, r)
	case "bridge-demo":
		g.handleUC03Bridge(w, r, "MBR-BRIDGE-DEMO")
	case "bridge-refuse":
		g.handleUC03Bridge(w, r, "MBR-BRIDGE-REFUSE")
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown branch"})
	}
}

// handleUC03Oxygen is UC-03's re-keyed non-provider-data arm (register §11 ruling (b)):
// the oxygen family (E1390) via order-DISPATCH — the CRD hook shape genuinely proven live
// against the reference payer for this family (originate_homeoxygen.go's DIVERGENCE 2/3),
// not order-select's PA-required assertion, which no live pin covers for this family. Rides
// runCRDDispatch (shared with originateDispatch) so the CRD/DTR/populate plumbing is ONE
// implementation. UC-03's distinctive over originateDispatch's own callers: it must
// ANSWER the tree's one required item (6.1) before submitting — buildOxygenNecessityItem
// merges that answer into the populated QR without disturbing the genuine 2.2/2.3 auto
// answers — and it surfaces the hermetic FR-17 source=auto attribution
// (homeOxygenAutoFillEvidence) that register §9 row 4 asked for.
func (g *Gateway) handleUC03Oxygen(w http.ResponseWriter, r *http.Request) {
	member, ok := g.scenarioMember(w, r, "MBR-COVERED", "MBR-COVERED", "MBR-D-UC03")
	if !ok {
		return
	}
	r = r.WithContext(withFindingContext(r.Context(), findingContext{
		LegType: "crd-order-dispatch", Seam: "originate", Whose: "own",
	}))
	o := originationCodes().uc03
	// The order is the member's OWN open order, read from the participant's
	// system like every other lane's — it used to be built here and held
	// nowhere, so the authorization it produced named an order that could never
	// be re-read. The scenario still states which product it is about, and a
	// system holding a different order is refused rather than originated.
	order, ok := g.dispatchOrderOfRecord(w, r, member)
	if !ok {
		return
	}
	if system, code, _, err := shnsdk.ParseOrderProductCoding(order.orderJSON); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "open order has no recognized product coding"})
		return
	} else if system != o.system || code != o.code {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf(
			"this origination is about %s|%s and the member's open order is %s|%s", o.system, o.code, system, code)})
		return
	}
	res, ok := g.runCRDDispatch(w, r, member, order)
	if !ok {
		return
	}

	// The hermetic FR-17 source=auto proof (register §9 row 4 / §11): independently
	// cross-checked against the member's OWN seeded Observation, never trusting the
	// populate engine's own claim (the anti-pattern this slice exists to rule out).
	filled, readErr := g.homeOxygenAutoFillEvidenceContext(r.Context(), member, res.qrJSON)
	if writeSoRFailure(w, readErr) {
		return
	}

	// §3 ruling: 6.1 (the one required item) is answered honestly through the requester's
	// OWN attestation — source="manual", never "auto", never fabricated as clinical fact —
	// merged into the ALREADY-populated QR so the genuine auto answers survive.
	npi, status, msg := g.attestingNPI(r.Context(), res.orderJSON, "")
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	when := g.cfg.Clock().Format("2006-01-02")
	itemJSON, err := buildOxygenNecessityItem(npi, o.display, o.dx, when)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	res.qrSource = res.qrSource.withAmendment(itemJSON, "amend qr with item 6.1")
	attestedQR, err := res.qrSource.buildAtLine(res.dtrLine)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// This attested QR is the same dtr-questionnaire-fetch artifact runCRDDispatch's
	// own checks already name that way — match it, rather than carry this handler's
	// entry tag (crd-order-dispatch) onto a DTR-leg check.
	dtrCtx := withFindingContext(r.Context(), findingContext{
		LegType: "dtr-questionnaire-fetch", Seam: "originate", Whose: "own",
	})
	if status, msg := g.validateDTRQuestionnaireResponse(dtrCtx, attestedQR, res.dtrLine); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	res.qrJSON = attestedQR

	// --- PAS — the shared lean single-shot tail (submitClaimAndFollow). The genuine
	// outcome is conditional-coverage A4-pended → A1 (D-2RI-3), and the A1 comes from the
	// payer's answer to the follow-up inquiry. A payer that pends and does not resolve is
	// answered with the pend and its continuation. ---
	wait, waitOK := pasWaitOf(r)
	if !waitOK {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "wait must be a whole number of seconds"})
		return
	}
	decision, status, msg, err := g.submitClaimAndFollow(r.Context(), r, pasFollowInputs{
		pci: res.pci, patientRef: res.patientRef, coverageRef: res.coverageRef, coverage: res.coverage, insurer: res.insurer, member: res.member, memberSystem: res.memberSystem,
		recipient: res.recipient, orderRef: res.orderRef, orderJSON: res.orderJSON,
		supplierJSON: res.supplierJSON, source: res.qrSource, payer: res.payer, wait: wait,
	})
	if status != 0 {
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	// FR-23: persist the payer-issued auth number against the order reference. Only an
	// approval has one.
	if decision.Decision == PASDecisionApproved {
		if err := g.cfg.Store.StoreAuthNumber(res.orderRef, decision.Parsed.PreAuthRef); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "holder write failed (auth number)"})
			return
		}
	}

	writeJSON(w, http.StatusOK, decision.applyTo(uc03Resp{
		PARequired: true,
		QRItems:    filled,
		QRAnswers:  res.qrAnswers,
		// Attested is computed by INDEPENDENTLY inspecting the actual attestedQR bytes just
		// submitted (questionnaireResponseAnswered), never self-reported from "did the
		// attestation code path run" — a self-reported flag would stay true even if a future
		// edit accidentally submitted the pre-attestation shell (register §11 ruling,
		// criterion 2; see uc02_uc03_test.go's mutation evidence).
		Attested: questionnaireResponseAnswered(attestedQR, "6.1"),
	}))
}

// uc03BridgeCode is the kit-bridging-visualization demo's OWN literal order tuple —
// decoupled from originationCodes().uc03 (R3): the bridge-refuse arm demonstrates a
// bridging peer's REFUSAL, which (per TestHandleUC03Bridge_SelectsMember) fires at the
// ROUTING gate before any leg is ever attempted, so the family/code carried has never
// mattered to that arm's outcome; the bridge-demo arm reuses the SAME literal (task2
// brief) since both arms share one body. Kept literally UNCHANGED (L8000, order-select)
// rather than following UC-03's oxygen re-key, so this demo-only exhibit stays
// byte-identical to before R3.
var uc03BridgeCode = orderTuple{systemHCPCSBuild, "L8000", DemoDisplayL8000, DemoDxL8000}

// handleUC03Bridge is the kit-bridging-visualization demo's shared body for both
// "bridge-demo" (MBR-BRIDGE-DEMO, engine.BridgeDemoPayerID — the full bridged SUCCESS:
// CRD/DTR cross the version boundary via the transform chains, PAS lands on the shared
// line, approval with a payer-issued authNumber) and "bridge-refuse" (MBR-BRIDGE-REFUSE,
// engine.BridgeRefusePayerID — the DESIGNED refusal: the peer's PAS declaration is
// skewed against this build's own, so selectLegLineOrBridgeRefuse's pas-claim
// contract-version line-select refuses BEFORE any bundle is ever built). The two arms
// differ ONLY in which member/persona they drive — which coverage-derived routing
// (FeedPayerRouter) sends to which deployed bridge-peer holder — everything else,
// including the CRD+DTR order-select flow and the PAS submit attempt, is byte-identical.
// UNCHANGED by R3 (it is not the scenario register §11 ruled on — see handleUC03's doc
// comment): this is the kit-bridging-visualization demo's OWN literal L8000/order-select
// exhibit, decoupled from handleUC03Oxygen's oxygen re-key.
func (g *Gateway) handleUC03Bridge(w http.ResponseWriter, r *http.Request, member string) {
	r = r.WithContext(withFindingContext(r.Context(), findingContext{
		LegType: "pas-claim", Seam: "originate", Whose: "own",
	}))
	ctx := r.Context()

	// Both scenarioMember args are the bridge persona (this path never runs under
	// OriginationProfile=="provider-data" — that profile early-returns above — so
	// providerDataMember is dead here). ?personaSet=canary is NOT special-cased:
	// scenarioMember's CanaryTwins lookup naturally 400s ("no canary twin for member
	// ...") since no twin is registered — deliberately not adding one (reviewer ruling:
	// a canary twin for a demo-only persona would be semantic abuse of the canary
	// mechanism, which exists for observability's shared scenario members, not
	// visualization fixtures).
	m, ok := g.scenarioMember(w, r, member, member, member)
	if !ok {
		return
	}
	o := uc03BridgeCode
	res, ok := g.runCRDThenDTROrder(w, r, m, o.system, o.code, o.display, o.dx, false)
	if !ok {
		return
	}

	// The order this origination is about, as the participant's own system names
	// it: what the authorization is filed against and what an inquiry re-reads.
	srRef, ok := orderRefOrFail(w, res.srJSON)
	if !ok {
		return
	}

	// --- PAS round-trip: submit the preauth bundle, expect an approval — UNLESS the
	// peer's PAS declaration is skewed against this build's own (bridge-refuse), in
	// which case selectLegLineOrBridgeRefuse writes the demo lane's structured 200
	// refusal itself and we return here without ever building a bundle. ---
	pasCorr := g.cfg.CorrelationGen()
	ctx = withFindingContext(ctx, findingContext{
		LegType: "pas-claim", CorrelationID: pasCorr, Seam: "originate", Whose: "own",
	})
	// Select-before-build: the routed line CHOOSES the builder, so it is
	// resolved BEFORE the bundle exists. Also the pended-line pin for any
	// pas-claim-update leg downstream — pas-claim and pas-claim-update share the
	// pa.pas contract, so one selection is contract-correct for both.
	route, ok := g.selectLegLineOrBridgeRefuse(w, res.recipient, "pas-claim", pasCorr)
	if !ok {
		return
	}
	targetLine := shnsdk.LineOf(route.Token)
	pasQR, err := buildPASAttachment(res.qrSource, targetLine)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	pasProviderJSON, providerOK := g.pasProviderOrFail(w, r, res.srJSON)
	if !providerOK {
		return
	}
	pasMemberSystem, memberOK := g.pasMemberSystemOrFail(w, res.memberSystem, res.member)
	if !memberOK {
		return
	}
	bundleJSON, err := buildAuthoredPASSubmit(route.BuildLine, shnsdk.ConformantClaimInputs{
		QR: pasQR, SR: res.srJSON, Provider: pasProviderJSON, Coverage: res.coverage, Insurer: res.insurer, PatientRef: res.patientRef, CoverageRef: res.coverageRef, MemberID: res.member, MemberIDSystem: pasMemberSystem,
		Corr: pasCorr, Created: g.cfg.Clock(),
		ContainedInsurer: relaysReferencePayerBytes(g.cfg.OriginationProfile),
		AbsoluteRefs:     relaysReferencePayerBytes(g.cfg.OriginationProfile),
		PayerOrgEntry:    relaysReferencePayerBytes(g.cfg.OriginationProfile), // payer Org as a resolvable PAS bundle entry (br-payer findInBundle)
		Payer:            res.payer,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build bundle failed: " + err.Error()})
		return
	}
	// APPLY-time designed refusal (fix-round finding, second live run): with
	// SHN_DEMO_EGRESS_NATIVE_LINES narrowing arm(2), the bridge-refuse arm's own chain IS
	// selected above (a real bridging chain to the peer's declared line exists) but refuses
	// HERE, inside the chain's gated up-step, as a *SemanticChangeError — see
	// bridgeRefusalText's doc comment for why this and the selection-time
	// *RouteRefusalError are the ONLY two shapes reshaped; every other egressAdapt error
	// (a genuine fault) still falls through to the ordinary 502 below unchanged.
	bundleJSON, err = g.completeAuthoredPASRequest(ctx, bundleJSON, pasQR, res.srJSON, res.coverageRef, relaysReferencePayerBytes(g.cfg.OriginationProfile))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "PAS evidence linkage failed"})
		return
	}
	bundleJSON, _, err = g.egressAdapt(route, bundleJSON, ExchangeIdentity{CorrelationID: pasCorr, LegType: "pas-claim", Counterpart: res.recipient})
	if err != nil {
		if text, ok := bridgeRefusalText(err); ok {
			writeBridgeRefusal(w, "pas-claim", text)
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if status, msg := g.validatePASAttachments(ctx, bundleJSON, targetLine, res.qrSource != nil); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	if status, msg := g.validateFHIREgressOrBridged(ctx, bundleJSON, "pa.pas", targetLine, len(route.Chain) > 0); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	claimRespJSON, err := g.OriginateLeg(ctx, r, res.recipient, "pas-claim", res.pci, pasCorr, "",
		Content{WorkstreamType: workstreamPA, ProfileID: route.Token, Route: routeInfoFor(route), Payload: sealRequest(relay.BuilderSDKPASSubmit, bundleJSON, "application/fhir+json")})
	if err != nil {
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	ctx = withFindingContext(ctx, findingContext{
		LegType: "pas-claim", CorrelationID: pasCorr, Seam: "originate", Whose: "peer",
	})
	if status, msg := g.validateFHIRPayerIngress(ctx, claimRespJSON, targetLine, "pa.pas"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	// The payer's answer is reported as the payer gave it: approved, denied, or
	// pended with the continuation that carries on.
	wait, waitOK := pasWaitOf(r)
	if !waitOK {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "wait must be a whole number of seconds"})
		return
	}
	decision, status, msg, err := g.followDecision(ctx, r,
		pasSubmission{corr: pasCorr, route: route, bundleJSON: bundleJSON, respJSON: claimRespJSON},
		pasFollowInputs{pci: res.pci, patientRef: res.patientRef, member: res.member,
			recipient: res.recipient, orderRef: srRef, orderJSON: res.srJSON, memberSystem: res.memberSystem, wait: wait})
	if status != 0 {
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	// FR-23: persist the payer-issued auth number against the SR reference. Only an
	// approval has one.
	if decision.Decision == PASDecisionApproved {
		if err := g.cfg.Store.StoreAuthNumber(srRef, decision.Parsed.PreAuthRef); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "holder write failed (auth number)"})
			return
		}
	}

	writeJSON(w, http.StatusOK, decision.applyTo(uc03Resp{
		PARequired: true,
		QRItems:    res.filled,
	}))
}

// handleUC07HCPCS runs the HCPCS (L8000) DV-approve path — the in-process mirror of the
// two-RI L8000 approve: CRD (PA-required) → DTR auto-fill → PAS approve on first
// submit → the payer projects a HCPCS-system PDex PA EOB into the Patient-Access Store
// (FR-28; system flows from the L8000 order). NOT patient-authorship.
func (g *Gateway) handleUC07HCPCS(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(withFindingContext(r.Context(), findingContext{
		LegType: "pas-claim", Seam: "originate", Whose: "own",
	}))
	ctx := r.Context()

	member, ok := g.scenarioMember(w, r, "MBR-UC07HCPCS", "MBR-UC07HCPCS", "MBR-UC07HCPCS")
	if !ok {
		return
	}
	o := originationCodes().uc07hcpcs
	res, ok := g.runCRDThenDTROrder(w, r, member, o.system, o.code, o.display, o.dx, false)
	if !ok {
		return
	}

	// The order this origination is about, as the participant's own system names
	// it: what the authorization is filed against and what an inquiry re-reads.
	srRef, ok := orderRefOrFail(w, res.srJSON)
	if !ok {
		return
	}

	// --- PAS round-trip: submit the preauth bundle, expect an approval. ---
	pasCorr := g.cfg.CorrelationGen()
	ctx = withFindingContext(ctx, findingContext{
		LegType: "pas-claim", CorrelationID: pasCorr, Seam: "originate", Whose: "own",
	})
	// Select-before-build: the routed line CHOOSES the builder, so it is
	// resolved BEFORE the bundle exists. Also the pended-line pin for any
	// pas-claim-update leg downstream — pas-claim and pas-claim-update share the
	// pa.pas contract, so one selection is contract-correct for both.
	route, ok := g.selectLegLineOrFail(w, res.recipient, "pas-claim", pasCorr)
	if !ok {
		return
	}
	targetLine := shnsdk.LineOf(route.Token)
	pasQR, err := buildPASAttachment(res.qrSource, targetLine)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	pasProviderJSON, providerOK := g.pasProviderOrFail(w, r, res.srJSON)
	if !providerOK {
		return
	}
	pasMemberSystem, memberOK := g.pasMemberSystemOrFail(w, res.memberSystem, res.member)
	if !memberOK {
		return
	}
	bundleJSON, err := buildAuthoredPASSubmit(route.BuildLine, shnsdk.ConformantClaimInputs{
		QR: pasQR, SR: res.srJSON, Provider: pasProviderJSON, Coverage: res.coverage, Insurer: res.insurer, PatientRef: res.patientRef, CoverageRef: res.coverageRef, MemberID: res.member, MemberIDSystem: pasMemberSystem,
		Corr: pasCorr, Created: g.cfg.Clock(),
		ContainedInsurer: relaysReferencePayerBytes(g.cfg.OriginationProfile),
		AbsoluteRefs:     relaysReferencePayerBytes(g.cfg.OriginationProfile),
		PayerOrgEntry:    relaysReferencePayerBytes(g.cfg.OriginationProfile), // payer Org as a resolvable PAS bundle entry (br-payer findInBundle)
		Payer:            res.payer,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build bundle failed: " + err.Error()})
		return
	}
	bundleJSON, err = g.completeAuthoredPASRequest(ctx, bundleJSON, pasQR, res.srJSON, res.coverageRef, relaysReferencePayerBytes(g.cfg.OriginationProfile))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "PAS evidence linkage failed"})
		return
	}
	bundleJSON, _, err = g.egressAdapt(route, bundleJSON, ExchangeIdentity{CorrelationID: pasCorr, LegType: "pas-claim", Counterpart: res.recipient})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if status, msg := g.validatePASAttachments(ctx, bundleJSON, targetLine, res.qrSource != nil); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	if status, msg := g.validateFHIREgressOrBridged(ctx, bundleJSON, "pa.pas", targetLine, len(route.Chain) > 0); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	claimRespJSON, err := g.OriginateLeg(ctx, r, res.recipient, "pas-claim", res.pci, pasCorr, "",
		Content{WorkstreamType: workstreamPA, ProfileID: route.Token, Route: routeInfoFor(route), Payload: sealRequest(relay.BuilderSDKPASSubmit, bundleJSON, "application/fhir+json")})
	if err != nil {
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	ctx = withFindingContext(ctx, findingContext{
		LegType: "pas-claim", CorrelationID: pasCorr, Seam: "originate", Whose: "peer",
	})
	if status, msg := g.validateFHIRPayerIngress(ctx, claimRespJSON, targetLine, "pa.pas"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	// The payer's answer is reported as the payer gave it: approved, denied, or
	// pended with the continuation that carries on.
	wait, waitOK := pasWaitOf(r)
	if !waitOK {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "wait must be a whole number of seconds"})
		return
	}
	decision, status, msg, err := g.followDecision(ctx, r,
		pasSubmission{corr: pasCorr, route: route, bundleJSON: bundleJSON, respJSON: claimRespJSON},
		pasFollowInputs{pci: res.pci, patientRef: res.patientRef, member: res.member,
			recipient: res.recipient, orderRef: srRef, orderJSON: res.srJSON, memberSystem: res.memberSystem, wait: wait})
	if status != 0 {
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	// FR-23: persist the payer-issued auth number against the SR reference. Only an
	// approval has one.
	if decision.Decision == PASDecisionApproved {
		if err := g.cfg.Store.StoreAuthNumber(srRef, decision.Parsed.PreAuthRef); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "holder write failed (auth number)"})
			return
		}
	}

	writeJSON(w, http.StatusOK, decision.applyTo(uc03Resp{
		PARequired: true,
		QRItems:    res.filled,
	}))
}

// handleUC04 runs the pended-then-approved PA path. Two profile lanes share the CRD+DTR prefix:
//   - demo (and any non-provider-data lane): CRD+DTR → PAS submit → PENDED (no operative DiagnosticReport yet) →
//     ClaimUpdate with the provider-LOCAL operative report + Provenance → approved (FR-20/21).
//   - provider-data (L1): ATTEST the adaptive HomeHealthAssessment off the seeded order (the
//     operated $populate auto-pops nothing), then the lean single-shot PAS tail with NO
//     amendment (D-PD-1 defers the operative-DiagnosticReport amendment). The attested QR is
//     verdict-INERT — br-payer's A4→A1 is its pend-resolution timer, not a QR-driven verdict.
func (g *Gateway) handleUC04(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(withFindingContext(r.Context(), findingContext{
		LegType: "pas-claim", Seam: "originate", Whose: "own",
	}))
	ctx := r.Context()

	o := originationCodes().uc04
	member, ok := g.scenarioMember(w, r, "MBR-UC04", "MBR-PD-UC04", "MBR-D-UC04")
	if !ok {
		return
	}
	res, ok := g.runCRDThenDTROrder(w, r, member, o.system, o.code, o.display, o.dx, false)
	if !ok {
		return
	}

	// The order this origination is about, as the participant's own system names
	// it: what the authorization is filed against and what an inquiry re-reads.
	srRef, ok := orderRefOrFail(w, res.srJSON)
	if !ok {
		return
	}

	if g.cfg.OriginationProfile == "provider-data" {
		// provider-data (L1): ATTEST the adaptive HomeHealthAssessment questionnaire off the seeded
		// order ($populate auto-pops nothing), then the lean single-shot tail (D-PD-1: no
		// amendment). Every attested answer traces to the seeded order (res.srJSON); the attested QR
		// is verdict-INERT — br-payer's A4→A1 is its pend-resolution timer, not a QR-driven verdict.
		resolve, readErr := sorReferenceCallback(ctx, g.cfg.SoR)
		answers, err := uc04AttestationAnswers(res.srJSON, resolve)
		if writeSoRFailure(w, *readErr) {
			return
		}
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		orderRef := srRef
		// Attest at the selected DTR line (closes an earlier KNOWN GAP: this QR
		// used to be built at the frozen 2.0 shape regardless of the selected line — the
		// 2.2 two-RI run showed wrong-line QR bytes could pass SILENTLY, the UC-03 auto-fill
		// site this closes). The HomeHealthAssessment is SDC-ADAPTIVE: the attestation
		// drives $next-question so the group the attested answers belong to is DELIVERED
		// before the fill (dtr_adaptive.go) — a fill over the package's group-1 tree alone
		// drops every group-3 answer and puts 1.1 alone on the wire.
		qc := shnsdk.QRContext{PatientRef: res.patientRef, CoverageRef: res.coverageRef, OrderRef: orderRef, Authored: g.cfg.Clock()}
		attestedQR, deliveredTree, status, msg, err := g.attestAdaptiveQuestionnaire(ctx, r, res, answers, qc)
		if status != 0 {
			if g.relayOriginationError(w, err) {
				return
			}
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
		res.qrSource = newAutomaticDTRBuildSource(deliveredTree, answers, qc)
		res.questionnaireJSON = deliveredTree
		attestedQR, status, msg = g.completeDTRContext(ctx, attestedQR, res.dtrLine, shnsdk.QRContext{PatientRef: res.patientRef, CoverageRef: res.coverageRef, OrderRef: orderRef})
		if status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
		wait, waitOK := pasWaitOf(r)
		if !waitOK {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "wait must be a whole number of seconds"})
			return
		}
		decision, status, msg, err := g.submitClaimAndFollow(ctx, r, pasFollowInputs{
			pci: res.pci, patientRef: res.patientRef, coverageRef: res.coverageRef, coverage: res.coverage, insurer: res.insurer, member: res.member, memberSystem: res.memberSystem,
			recipient: res.recipient, orderRef: orderRef, orderJSON: res.srJSON,
			source: res.qrSource, payer: res.payer, wait: wait,
		})
		if status != 0 {
			if g.relayOriginationError(w, err) {
				return
			}
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
		if decision.Decision == PASDecisionApproved {
			if err := g.cfg.Store.StoreAuthNumber(orderRef, decision.Parsed.PreAuthRef); err != nil {
				writeJSON(w, http.StatusBadGateway, map[string]string{"error": "holder write failed (auth number)"})
				return
			}
		}
		// Surface the attested answer VALUES (the traces-to-seed evidence, the UC-04 analog of
		// HomeOxygen's qrAnswers).
		writeJSON(w, http.StatusOK, decision.applyTo(uc03Resp{PARequired: true, QRAnswers: attestedAnswerValues(answers)}))
		return
	}

	// demo (and any non-provider-data lane): the operative-DiagnosticReport amendment tail.
	// PAS submit — expect PENDED (no operative DiagnosticReport yet).
	pasCorr := g.cfg.CorrelationGen()
	ctx = withFindingContext(ctx, findingContext{
		LegType: "pas-claim", CorrelationID: pasCorr, Seam: "originate", Whose: "own",
	})
	// Select-before-build: the routed line CHOOSES the builder, so it is
	// resolved BEFORE the bundle exists. Also the pended-line pin for any
	// pas-claim-update leg downstream — pas-claim and pas-claim-update share the
	// pa.pas contract, so one selection is contract-correct for both.
	route, ok := g.selectLegLineOrFail(w, res.recipient, "pas-claim", pasCorr)
	if !ok {
		return
	}
	targetLine := shnsdk.LineOf(route.Token)
	pasQR, err := buildPASAttachment(res.qrSource, targetLine)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	pasProviderJSON, providerOK := g.pasProviderOrFail(w, r, res.srJSON)
	if !providerOK {
		return
	}
	pasMemberSystem, memberOK := g.pasMemberSystemOrFail(w, res.memberSystem, res.member)
	if !memberOK {
		return
	}
	bundleJSON, err := buildAuthoredPASSubmit(route.BuildLine, shnsdk.ConformantClaimInputs{
		QR: pasQR, SR: res.srJSON, Provider: pasProviderJSON, Coverage: res.coverage, Insurer: res.insurer, PatientRef: res.patientRef, CoverageRef: res.coverageRef, MemberID: res.member, MemberIDSystem: pasMemberSystem,
		Corr: pasCorr, Created: g.cfg.Clock(),
		ContainedInsurer: relaysReferencePayerBytes(g.cfg.OriginationProfile),
		AbsoluteRefs:     relaysReferencePayerBytes(g.cfg.OriginationProfile),
		PayerOrgEntry:    relaysReferencePayerBytes(g.cfg.OriginationProfile), // payer Org as a resolvable PAS bundle entry (br-payer findInBundle)
		Payer:            res.payer,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build bundle failed: " + err.Error()})
		return
	}
	bundleJSON, err = g.completeAuthoredPASRequest(ctx, bundleJSON, pasQR, res.srJSON, res.coverageRef, relaysReferencePayerBytes(g.cfg.OriginationProfile))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "PAS evidence linkage failed"})
		return
	}
	bundleJSON, _, err = g.egressAdapt(route, bundleJSON, ExchangeIdentity{CorrelationID: pasCorr, LegType: "pas-claim", Counterpart: res.recipient})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if status, msg := g.validatePASAttachments(ctx, bundleJSON, targetLine, res.qrSource != nil); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	if status, msg := g.validateFHIREgressOrBridged(ctx, bundleJSON, "pa.pas", targetLine, len(route.Chain) > 0); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	pendedResp, err := g.OriginateLeg(ctx, r, res.recipient, "pas-claim", res.pci, pasCorr, "",
		Content{WorkstreamType: workstreamPA, ProfileID: route.Token, Route: routeInfoFor(route), Payload: sealRequest(relay.BuilderSDKPASSubmit, bundleJSON, "application/fhir+json")})
	if err != nil {
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	ctx = withFindingContext(ctx, findingContext{
		LegType: "pas-claim", CorrelationID: pasCorr, Seam: "originate", Whose: "peer",
	})
	if status, msg := g.validateFHIRPayerIngress(ctx, pendedResp, targetLine, "pa.pas"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	pended, neededItems, err := shnsdk.ParsePendedResponse(pendedResp)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "parse pended response failed"})
		return
	}
	if !pended {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "expected pended response"})
		return
	}
	// Map []NeededItem → []string using .Code (the Task.input valueString, matching
	// what the internal ParsePendedOrApproved returned as a plain []string).
	needed := neededItemCodes(neededItems)

	// Back to this participant's own bytes for the amendment build (pas-claim-update):
	// the pended-response check above is the only peer-answer read in this stretch.
	ctx = withFindingContext(ctx, findingContext{
		LegType: "pas-claim-update", Seam: "originate", Whose: "own",
	})

	// Amend: attach the provider-LOCAL operative DiagnosticReport + Provenance.
	drJSON, drOK, readErr := ReadSystemOfRecord(g.cfg.SoR).SupplementalReportContext(ctx, member)
	if writeSoRFailure(w, readErr) {
		return
	}
	if !drOK {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "no supplemental report"})
		return
	}
	if status, msg := g.validateFHIR(ctx, drJSON, "egress", ""); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	drRef, ok := resourceRef(drJSON)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "supplemental report missing id"})
		return
	}
	provJSON, err := buildEvidenceProvenance(drRef, "http://smarthealth.network/ids/holder", g.cfg.HolderID, "", "", g.cfg.Clock())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build provenance failed"})
		return
	}
	if status, msg := g.validateFHIR(ctx, provJSON, "egress", ""); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	updateCorr := g.cfg.CorrelationGen()
	ctx = withFindingContext(ctx, findingContext{
		LegType: "pas-claim-update", CorrelationID: updateCorr, Seam: "originate", Whose: "own",
	})
	// Built at the PINNED route (never re-selected: the amendment must answer
	// the pend it references, and the pended-pin rule says a resume leg never
	// re-negotiates) — route.BuildLine/route.Token are the SAME captured route
	// the initial pas-claim submit selected above.
	updateBundle, err := buildAuthoredPASUpdate(route.BuildLine, shnsdk.ConformantClaimUpdateInputs{
		QR: pasQR, SR: res.srJSON, Provider: pasProviderJSON, Coverage: res.coverage, Insurer: res.insurer, PatientRef: res.patientRef, CoverageRef: res.coverageRef, MemberID: res.member, MemberIDSystem: pasMemberSystem,
		Provenance: provJSON, DiagnosticReport: drJSON, Corr: updateCorr, OriginalCorr: pasCorr, Created: g.cfg.Clock(),
		ContainedInsurer: relaysReferencePayerBytes(g.cfg.OriginationProfile),
		AbsoluteRefs:     relaysReferencePayerBytes(g.cfg.OriginationProfile),
		PayerOrgEntry:    relaysReferencePayerBytes(g.cfg.OriginationProfile), // payer Org as a resolvable PAS bundle entry (br-payer findInBundle)
		Payer:            res.payer,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build update bundle failed"})
		return
	}
	updateBundle, err = g.completeAuthoredPASRequest(ctx, updateBundle, pasQR, res.srJSON, res.coverageRef, relaysReferencePayerBytes(g.cfg.OriginationProfile))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "PAS evidence linkage failed"})
		return
	}
	updateBundle, _, err = g.egressAdapt(route, updateBundle, ExchangeIdentity{CorrelationID: updateCorr, LegType: "pas-claim-update", Counterpart: res.recipient})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if status, msg := g.validatePASAttachments(ctx, updateBundle, targetLine, res.qrSource != nil); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	if status, msg := g.validateFHIREgressOrBridged(ctx, updateBundle, "pa.pas", targetLine, len(route.Chain) > 0); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	// ClaimUpdate exchange — expect APPROVED.
	updateResp, err := g.OriginateLeg(ctx, r, res.recipient, "pas-claim-update", res.pci, updateCorr, "",
		Content{WorkstreamType: workstreamPA, ProfileID: route.Token, Route: routeInfoFor(route), Payload: sealRequest(relay.BuilderSDKPASUpdate, updateBundle, "application/fhir+json")})
	if err != nil {
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	ctx = withFindingContext(ctx, findingContext{
		LegType: "pas-claim-update", CorrelationID: updateCorr, Seam: "originate", Whose: "peer",
	})
	if status, msg := g.validateFHIRPayerIngress(ctx, updateResp, targetLine, "pa.pas"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	// The payer's answer to the amendment is reported as the payer gave it. UC-04 is a
	// DiagnosticReport amendment, NOT an attestation → no Attested. AmendmentCorr is the
	// evidence the amendment leg ran (the decision was reached THROUGH the amendment, not a
	// bare approve); AuthNumber is the payer's own authorization number when it approved.
	// A re-pend is answered with its continuation, which the caller continues.
	wait, waitOK := pasWaitOf(r)
	if !waitOK {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "wait must be a whole number of seconds"})
		return
	}
	decision, status, msg, err := g.followDecision(ctx, r,
		pasSubmission{corr: updateCorr, route: route, bundleJSON: updateBundle, respJSON: updateResp},
		pasFollowInputs{pci: res.pci, patientRef: res.patientRef, member: res.member,
			recipient: res.recipient, orderRef: srRef, orderJSON: res.srJSON, memberSystem: res.memberSystem, wait: wait})
	if status != 0 {
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	if decision.Decision == PASDecisionApproved {
		if err := g.cfg.Store.StoreAuthNumber(srRef, decision.Parsed.PreAuthRef); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "holder write failed (auth number)"})
			return
		}
	}
	writeJSON(w, http.StatusOK, decision.applyTo(uc03Resp{
		PARequired: true, AmendmentCorr: updateCorr, QRItems: res.filled, PendedItems: needed,
	}))
}

// uc05Resp is the UC-05 result. ConsentDenied/Pended mark the negative branch
// (federated query refused, PA stays pended); the positive branch carries the
// approval + the facility the evidence came from (source attribution).
type uc05Resp struct {
	PARequired    bool         `json:"paRequired"`
	AuthNumber    string       `json:"authNumber,omitempty"`
	ValidUntil    string       `json:"validUntil,omitempty"`
	QRItems       []FilledItem `json:"qrItems,omitempty"`
	PendedItems   []string     `json:"pendedItems,omitempty"`
	FacilityID    string       `json:"facilityId,omitempty"`
	Pended        bool         `json:"pended,omitempty"`
	ConsentDenied bool         `json:"consentDenied,omitempty"`

	// The payer's own determination and the capability that continues it, in
	// exactly the vocabulary uc03Resp uses. TWO branches set Pended above and
	// they mean different things: the consent-denied branch never asked the payer
	// again, so it carries no Decision and no Continuation; the payer's own pend
	// carries both.
	Decision            string `json:"decision,omitempty"`
	Denied              bool   `json:"denied,omitempty"`
	Rationale           string `json:"rationale,omitempty"`
	Continuation        string `json:"continuation,omitempty"`
	ContinuationDurable *bool  `json:"continuationDurable,omitempty"`
}

// handleUC05 runs the federated EXTERNAL-retrieval PA path (the non-aggregation
// showcase): CRD+DTR → PAS submit → PENDED (operative report not local) →
// consent-gated federated query to the external facility (provider→Hub→facility),
// which returns ONLY the named operative DiagnosticReport + a source Provenance
// citing the consent → ClaimUpdate with those → APPROVED. Branch "noconsent" uses
// Linda's no-consent twin: the federated query is denied and the PA stays pended.
func (g *Gateway) handleUC05(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(withFindingContext(r.Context(), findingContext{
		LegType: "pas-claim", Seam: "originate", Whose: "own",
	}))
	ctx := r.Context()

	var req struct {
		Branch string `json:"branch"`
	}
	// The body selects the branch (default consent when absent). An EMPTY body is
	// allowed (io.EOF ⇒ default), but a MALFORMED body is REJECTED rather than
	// silently falling through to the happy path — a caller sending bad JSON gets a
	// clear 400, not an unintended consented run.
	if tooLarge, err := shnsdk.DecodeJSONBody(w, r, &req); err != nil && !errors.Is(err, io.EOF) {
		if tooLarge {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
		} else {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		}
		return
	}
	// Reject unknown branch values (defense-in-depth, mirroring UC-01): only the
	// empty default, "consent", and "noconsent" are valid. An unrecognized branch
	// must NOT silently run the consented path.
	switch req.Branch {
	case "", "consent", "noconsent":
		// valid
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown branch"})
		return
	}
	member, ok := g.scenarioMember(w, r, "MBR-UC05", "MBR-PD-UC05", "MBR-D-UC05")
	if !ok {
		return
	}
	if req.Branch == "noconsent" {
		member, ok = g.scenarioMember(w, r, "MBR-UC05-NOCONSENT", "MBR-PD-UC05-NC", "MBR-D-UC05-NC")
		if !ok {
			return
		}
	}

	o := originationCodes().uc05
	res, ok := g.runCRDThenDTROrder(w, r, member, o.system, o.code, o.display, o.dx, false)
	if !ok {
		return
	}

	// The authorization is filed against the order the participant's own system
	// holds, on every lane. The noconsent branch never reaches StoreAuthNumber
	// (it returns consentDenied earlier), so the reference is moot there.
	srRef, ok := orderRefOrFail(w, res.srJSON)
	if !ok {
		return
	}

	if g.cfg.OriginationProfile == "provider-data" {
		// UC-05 carries the SAME seeded G0151 order (and so the SAME adaptive
		// HomeHealthAssessment) as UC-04: ATTEST it exactly as handleUC04 does — the
		// operated $populate auto-pops nothing on the 0-CQL HHA, so the fill must drive
		// $next-question until group 3 is delivered before it can answer 3.1/3.2/3.3.
		// Every attested answer traces to the seeded order (res.srJSON), same as UC-04.
		// Unlike UC-04/06/07, UC-05 never re-attests on the amendment: the
		// pas-claim-update leg below carries the federated-query DiagnosticReport
		// evidence, not a new/superseding QR — it reuses res.qrJSON UNCHANGED, so
		// overwriting res.qrJSON here (before either leg is built) is enough; there is
		// no pend-then-amend attestation lane for UC-05 (no scenarioToPend call site),
		// so the grown tree never needs to be carried forward past this point.
		resolve, readErr := sorReferenceCallback(ctx, g.cfg.SoR)
		answers, err := uc04AttestationAnswers(res.srJSON, resolve)
		if writeSoRFailure(w, *readErr) {
			return
		}
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		qc := shnsdk.QRContext{PatientRef: res.patientRef, CoverageRef: res.coverageRef, OrderRef: srRef, Authored: g.cfg.Clock()}
		attestedQR, deliveredTree, status, msg, err := g.attestAdaptiveQuestionnaire(ctx, r, res, answers, qc)
		if status != 0 {
			if g.relayOriginationError(w, err) {
				return
			}
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
		res.qrSource = newAutomaticDTRBuildSource(deliveredTree, answers, qc)
		res.questionnaireJSON = deliveredTree
		attestedQR, status, msg = g.completeDTRContext(ctx, attestedQR, res.dtrLine, shnsdk.QRContext{PatientRef: res.patientRef, CoverageRef: res.coverageRef, OrderRef: srRef})
		if status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
		res.qrJSON = attestedQR
	}

	// PAS submit — expect PENDED (no operative DiagnosticReport yet).
	pasCorr := g.cfg.CorrelationGen()
	ctx = withFindingContext(ctx, findingContext{
		LegType: "pas-claim", CorrelationID: pasCorr, Seam: "originate", Whose: "own",
	})
	// Select-before-build: the routed line CHOOSES the builder, so it is
	// resolved BEFORE the bundle exists. Also the pended-line pin for any
	// pas-claim-update leg downstream — pas-claim and pas-claim-update share the
	// pa.pas contract, so one selection is contract-correct for both.
	route, ok := g.selectLegLineOrFail(w, res.recipient, "pas-claim", pasCorr)
	if !ok {
		return
	}
	targetLine := shnsdk.LineOf(route.Token)
	pasQR, err := buildPASAttachment(res.qrSource, targetLine)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	pasProviderJSON, providerOK := g.pasProviderOrFail(w, r, res.srJSON)
	if !providerOK {
		return
	}
	pasMemberSystem, memberOK := g.pasMemberSystemOrFail(w, res.memberSystem, res.member)
	if !memberOK {
		return
	}
	bundleJSON, err := buildAuthoredPASSubmit(route.BuildLine, shnsdk.ConformantClaimInputs{
		QR: pasQR, SR: res.srJSON, Provider: pasProviderJSON, Coverage: res.coverage, Insurer: res.insurer, PatientRef: res.patientRef, CoverageRef: res.coverageRef, MemberID: res.member, MemberIDSystem: pasMemberSystem,
		Corr: pasCorr, Created: g.cfg.Clock(),
		ContainedInsurer: relaysReferencePayerBytes(g.cfg.OriginationProfile),
		AbsoluteRefs:     relaysReferencePayerBytes(g.cfg.OriginationProfile),
		PayerOrgEntry:    relaysReferencePayerBytes(g.cfg.OriginationProfile), // payer Org as a resolvable PAS bundle entry (br-payer findInBundle)
		Payer:            res.payer,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build bundle failed: " + err.Error()})
		return
	}
	bundleJSON, err = g.completeAuthoredPASRequest(ctx, bundleJSON, pasQR, res.srJSON, res.coverageRef, relaysReferencePayerBytes(g.cfg.OriginationProfile))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "PAS evidence linkage failed"})
		return
	}
	bundleJSON, _, err = g.egressAdapt(route, bundleJSON, ExchangeIdentity{CorrelationID: pasCorr, LegType: "pas-claim", Counterpart: res.recipient})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if status, msg := g.validatePASAttachments(ctx, bundleJSON, targetLine, res.qrSource != nil); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	if status, msg := g.validateFHIREgressOrBridged(ctx, bundleJSON, "pa.pas", targetLine, len(route.Chain) > 0); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	pendedResp, err := g.OriginateLeg(ctx, r, res.recipient, "pas-claim", res.pci, pasCorr, "",
		Content{WorkstreamType: workstreamPA, ProfileID: route.Token, Route: routeInfoFor(route), Payload: sealRequest(relay.BuilderSDKPASSubmit, bundleJSON, "application/fhir+json")})
	if err != nil {
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	ctx = withFindingContext(ctx, findingContext{
		LegType: "pas-claim", CorrelationID: pasCorr, Seam: "originate", Whose: "peer",
	})
	if status, msg := g.validateFHIRPayerIngress(ctx, pendedResp, targetLine, "pa.pas"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	pended, neededItems, err := shnsdk.ParsePendedResponse(pendedResp)
	if err != nil || !pended {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "expected pended response"})
		return
	}
	needed := neededItemCodes(neededItems)

	// --- Federated query to the external facility (consent-gated). ---
	facility, fok := g.cfg.Reg.LookupByRole("facility")
	if !fok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "no facility registered"})
		return
	}
	// FR-24 names two document types; cdex-9 mandates exactly one data-query per CDex Task,
	// so the substrate federates one consent-gated leg per named type (FR-25 per-leg consent,
	// FR-26 only-matching-records-traverse). The DiagnosticReport leg yields the ClaimUpdate
	// evidence; the DocumentReference leg's named records traverse + are audited but are not
	// adjudication evidence (ExtractCDexEvidence pulls DiagnosticReport + Provenance).
	var drJSON, provJSON []byte
	for _, docType := range []string{"DiagnosticReport", "DocumentReference"} {
		reqMeta := shnsdk.CDexTaskMeta{AuthoredOn: g.cfg.Clock(), Requester: g.cfg.HolderID, Owner: facility.ID}
		queryJSON, err := shnsdk.BuildCDexTaskDataRequest(res.patientRef, docType,
			"2024-01-01", g.cfg.Clock().UTC().Format("2006-01-02"), reqMeta)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build query failed"})
			return
		}
		fqCorr := g.cfg.CorrelationGen()
		// custodian = facility.ID so the Authorization Framework gates consent for THIS source per leg (FR-25).
		recordsJSON, err := g.OriginateLeg(ctx, r, facility.ID, "federated-query", res.pci, fqCorr, facility.ID, Content{WorkstreamType: workstreamPA, Payload: sealRequest(relay.BuilderSDKFederatedQuery, queryJSON, "application/fhir+json")})
		if err != nil {
			// Distinguish a genuine consent DENIAL from an infrastructure/integrity
			// failure. ONLY an authorization denial (the no-consent branch) leaves the PA
			// validly pended; a facility outage, a tampered response, or a transport error
			// is a real failure and must NOT be misreported to the operator as "consent
			// denied". The denial is this flow's business outcome, so it is read before
			// relayOriginationError, which answers a denial as a 403.
			if errors.Is(err, errAuthorizationDenied) {
				writeJSON(w, http.StatusOK, uc05Resp{PARequired: true, Pended: true, ConsentDenied: true, PendedItems: needed})
				return
			}
			if g.relayOriginationError(w, err) {
				return
			}
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "federated query failed: " + err.Error()})
			return
		}
		// Ingress-validate the facility's searchset BEFORE trusting/extracting its
		// resources (defense in depth — every resource crossing the substrate is validated).
		// MUST stay plain validateFHIR, never validateFHIRPayerIngress: the facility is not
		// the reference payer, so this leg's bytes are never eligible for the R-8 skip
		// regardless of lane — this exact call site is the one that was once wrongly
		// exempted, sharing the payer-directed skip with every other ingress leg on the
		// same lane.
		fqCtx := withFindingContext(ctx, findingContext{
			LegType: "federated-query", CorrelationID: fqCorr, Seam: "originate", Whose: "peer",
		})
		if status, msg := g.validateFHIR(fqCtx, recordsJSON, "ingress", ""); status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
		// The facility's records arrive exactly as its system of record holds them. Fence
		// them before any use: every record must be about this member, directly or through
		// the Patient the facility carried with the member identifier.
		records, err := cdexRecordsBundle(recordsJSON)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "federated response parse failed: " + err.Error()})
			return
		}
		if err := requesterRecordsFence(res.member).check(records); err != nil {
			// The fence's reason names the class of failure, never a patient.
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "federated response refused: " + err.Error()})
			return
		}
		// Only the DiagnosticReport leg yields adjudication evidence; the DocRef leg's records
		// were ingress-validated, fenced + audited above but are not PAS evidence (see the loop comment).
		if docType == "DiagnosticReport" {
			drJSON, provJSON, err = cdexEvidence(records)
			if err != nil {
				writeJSON(w, http.StatusBadGateway, map[string]string{"error": "federated response parse failed: " + err.Error()})
				return
			}
			// Requester-side change, disclosed to partners: the ClaimUpdate is this
			// provider's own message, and its supplemental report must name the
			// Claim's patient. The facility's report (fenced above) keeps its own
			// subject in the facility's answer; this copy is re-pointed at
			// res.patientRef and re-encoded.
			drJSON, err = repointEvidenceSubject(drJSON, res.patientRef)
			if err != nil {
				writeJSON(w, http.StatusBadGateway, map[string]string{"error": "federated response parse failed: " + err.Error()})
				return
			}
		}
	}

	// --- ClaimUpdate with the externally-retrieved DiagnosticReport + Provenance. ---
	updateCorr := g.cfg.CorrelationGen()
	ctx = withFindingContext(ctx, findingContext{
		LegType: "pas-claim-update", CorrelationID: updateCorr, Seam: "originate", Whose: "own",
	})
	// Built at the PINNED route (never re-selected: the amendment must answer
	// the pend it references, and the pended-pin rule says a resume leg never
	// re-negotiates) — route.BuildLine/route.Token are the SAME captured route
	// the initial pas-claim submit selected above.
	updateBundle, err := buildAuthoredPASUpdate(route.BuildLine, shnsdk.ConformantClaimUpdateInputs{
		QR: pasQR, SR: res.srJSON, Provider: pasProviderJSON, Coverage: res.coverage, Insurer: res.insurer, PatientRef: res.patientRef, CoverageRef: res.coverageRef, MemberID: res.member, MemberIDSystem: pasMemberSystem,
		Provenance: provJSON, DiagnosticReport: drJSON, Corr: updateCorr, OriginalCorr: pasCorr, Created: g.cfg.Clock(),
		ContainedInsurer: relaysReferencePayerBytes(g.cfg.OriginationProfile),
		AbsoluteRefs:     relaysReferencePayerBytes(g.cfg.OriginationProfile),
		PayerOrgEntry:    relaysReferencePayerBytes(g.cfg.OriginationProfile), // payer Org as a resolvable PAS bundle entry (br-payer findInBundle)
		Payer:            res.payer,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build update bundle failed"})
		return
	}
	updateBundle, err = g.completeAuthoredPASRequest(ctx, updateBundle, pasQR, res.srJSON, res.coverageRef, relaysReferencePayerBytes(g.cfg.OriginationProfile))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "PAS evidence linkage failed"})
		return
	}
	updateBundle, _, err = g.egressAdapt(route, updateBundle, ExchangeIdentity{CorrelationID: updateCorr, LegType: "pas-claim-update", Counterpart: res.recipient})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if status, msg := g.validatePASAttachments(ctx, updateBundle, targetLine, res.qrSource != nil); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	if status, msg := g.validateFHIREgressOrBridged(ctx, updateBundle, "pa.pas", targetLine, len(route.Chain) > 0); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	updateResp, err := g.OriginateLeg(ctx, r, res.recipient, "pas-claim-update", res.pci, updateCorr, "",
		Content{WorkstreamType: workstreamPA, ProfileID: route.Token, Route: routeInfoFor(route), Payload: sealRequest(relay.BuilderSDKPASUpdate, updateBundle, "application/fhir+json")})
	if err != nil {
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	ctx = withFindingContext(ctx, findingContext{
		LegType: "pas-claim-update", CorrelationID: updateCorr, Seam: "originate", Whose: "peer",
	})
	if status, msg := g.validateFHIRPayerIngress(ctx, updateResp, targetLine, "pa.pas"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	// The payer's answer to the amendment carrying the federated evidence, reported
	// as the payer gave it.
	wait, waitOK := pasWaitOf(r)
	if !waitOK {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "wait must be a whole number of seconds"})
		return
	}
	decision, status, msg, err := g.followDecision(ctx, r,
		pasSubmission{corr: updateCorr, route: route, bundleJSON: updateBundle, respJSON: updateResp},
		pasFollowInputs{pci: res.pci, patientRef: res.patientRef, member: res.member,
			recipient: res.recipient, orderRef: srRef, orderJSON: res.srJSON, memberSystem: res.memberSystem, wait: wait})
	if status != 0 {
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	if decision.Decision == PASDecisionApproved {
		if err := g.cfg.Store.StoreAuthNumber(srRef, decision.Parsed.PreAuthRef); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "holder write failed (auth number)"})
			return
		}
	}
	writeJSON(w, http.StatusOK, decision.applyToUC05(uc05Resp{
		PARequired: true, QRItems: res.filled, PendedItems: needed, FacilityID: facility.ID,
	}))
}

// uc08Resp is the provider-side result of the UC-08 denial scenario.
// PARequired is always true (a PA was needed); Denied is true when the payer
// issued a denial (no auth number). Rationale is the ClaimResponse disposition.
// PatientDenialReason is the payer's own reason code from the PHG denial view
// (the patient surface reads the payer's PDex PA EOB; this field surfaces it for
// the operator console demo — the PHG call stands in for the patient app).
type uc08Resp struct {
	PARequired          bool   `json:"paRequired"`
	Denied              bool   `json:"denied"`
	AuthNumber          string `json:"authNumber,omitempty"`
	Rationale           string `json:"rationale,omitempty"`
	PatientDenialReason string `json:"patientDenialReason,omitempty"`
	// PatientDenialReasonDisplay is the payer's own text for that code, and
	// PatientDenialReasonSystem the code system it belongs to, both as the PHG
	// read them from the EOB. A payer that supplied no reason code states its
	// decision as a review decision instead, so the surface has to name the
	// code set it is showing rather than assume one.
	PatientDenialReasonDisplay string `json:"patientDenialReasonDisplay,omitempty"`
	PatientDenialReasonSystem  string `json:"patientDenialReasonSystem,omitempty"`
	// PatientAppeal is the appeal-window text the PHG read FROM the EOB.processNote
	// (FR-28: data-driven from the FHIR resource, not a UI string).
	PatientAppeal string `json:"patientAppeal,omitempty"`
}

// handleUC08 runs the PA-denied path (UC-08, FR-22): CRD+DTR → PAS submit →
// denied ClaimResponse (reviewAction A3, no preAuthRef) → the provider queries
// the PHG denial view (which reads the payer's PDex PA EOB) to obtain the
// patient-rendered reason: the payer's own decision code and the code system it
// stated it in — here its X12 306 review decision A3 "Not Certified", because
// the reference payer supplies no claim adjustment reason code of its own.
// The denial is TERMINAL — it does NOT pend.
func (g *Gateway) handleUC08(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(withFindingContext(r.Context(), findingContext{
		LegType: "pas-claim", Seam: "originate", Whose: "own",
	}))
	ctx := r.Context()

	o := originationCodes().uc08
	// provider-data (Mode A) and demo (§4.3, the in-process br-payer mirror) both target
	// br-payer's J3490 family, whose CRD verdict is NOT-COVERED — opt in to carry the
	// order past the FR-G25 stop to PAS → the formal A2 "Not Certified" ClaimResponse
	// (D-S2-2). Any other lane keeps the covered+PA→PAS-deny path (proceedOnNotCovered stays false).
	member, ok := g.scenarioMember(w, r, "MBR-UC08", "MBR-PD-UC08", "MBR-D-UC08")
	if !ok {
		return
	}
	proceedOnNotCovered := targetsBrPayer(g.cfg.OriginationProfile) || isDemoProfile(g.cfg.OriginationProfile)
	res, ok := g.runCRDThenDTROrder(w, r, member, o.system, o.code, o.display, o.dx, proceedOnNotCovered)
	if !ok {
		return
	}

	// PAS submit — expect DENIED (4 weeks conservative therapy < 6, no prior surgery,
	// not high-disability → Adjudicate returns Denied).
	pasCorr := g.cfg.CorrelationGen()
	ctx = withFindingContext(ctx, findingContext{
		LegType: "pas-claim", CorrelationID: pasCorr, Seam: "originate", Whose: "own",
	})
	// Select-before-build: the routed line CHOOSES the builder, so it is
	// resolved BEFORE the bundle exists. Also the pended-line pin for any
	// pas-claim-update leg downstream — pas-claim and pas-claim-update share the
	// pa.pas contract, so one selection is contract-correct for both.
	route, ok := g.selectLegLineOrFail(w, res.recipient, "pas-claim", pasCorr)
	if !ok {
		return
	}
	targetLine := shnsdk.LineOf(route.Token)
	pasQR, err := buildPASAttachment(res.qrSource, targetLine)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	pasProviderJSON, providerOK := g.pasProviderOrFail(w, r, res.srJSON)
	if !providerOK {
		return
	}
	pasMemberSystem, memberOK := g.pasMemberSystemOrFail(w, res.memberSystem, res.member)
	if !memberOK {
		return
	}
	bundleJSON, err := buildAuthoredPASSubmit(route.BuildLine, shnsdk.ConformantClaimInputs{
		QR: pasQR, SR: res.srJSON, Provider: pasProviderJSON, Coverage: res.coverage, Insurer: res.insurer, PatientRef: res.patientRef, CoverageRef: res.coverageRef, MemberID: res.member, MemberIDSystem: pasMemberSystem,
		Corr: pasCorr, Created: g.cfg.Clock(),
		ContainedInsurer: relaysReferencePayerBytes(g.cfg.OriginationProfile),
		AbsoluteRefs:     relaysReferencePayerBytes(g.cfg.OriginationProfile),
		PayerOrgEntry:    relaysReferencePayerBytes(g.cfg.OriginationProfile), // payer Org as a resolvable PAS bundle entry (br-payer findInBundle)
		Payer:            res.payer,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build bundle failed: " + err.Error()})
		return
	}
	bundleJSON, err = g.completeAuthoredPASRequest(ctx, bundleJSON, pasQR, res.srJSON, res.coverageRef, relaysReferencePayerBytes(g.cfg.OriginationProfile))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "PAS evidence linkage failed"})
		return
	}
	bundleJSON, _, err = g.egressAdapt(route, bundleJSON, ExchangeIdentity{CorrelationID: pasCorr, LegType: "pas-claim", Counterpart: res.recipient})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if status, msg := g.validatePASAttachments(ctx, bundleJSON, targetLine, res.qrSource != nil); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	if status, msg := g.validateFHIREgressOrBridged(ctx, bundleJSON, "pa.pas", targetLine, len(route.Chain) > 0); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	claimRespJSON, err := g.OriginateLeg(ctx, r, res.recipient, "pas-claim", res.pci, pasCorr, "",
		Content{WorkstreamType: workstreamPA, ProfileID: route.Token, Route: routeInfoFor(route), Payload: sealRequest(relay.BuilderSDKPASSubmit, bundleJSON, "application/fhir+json")})
	if err != nil {
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	ctx = withFindingContext(ctx, findingContext{
		LegType: "pas-claim", CorrelationID: pasCorr, Seam: "originate", Whose: "peer",
	})
	if status, msg := g.validateFHIRPayerIngress(ctx, claimRespJSON, targetLine, "pa.pas"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	// A denial is TERMINAL (does NOT pend) — use ParseClaimResponse directly and
	// expect Outcome == "denied". ParsePendedResponse is not used here.
	parsed, err := shnsdk.ParseClaimResponse(claimRespJSON)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "claim response parse failed"})
		return
	}
	if parsed.Outcome == "approved" {
		// Unexpected approval — surface the auth number for diagnostics.
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "expected denial but got approval: " + parsed.PreAuthRef})
		return
	}

	// Extract the human-readable rationale from the denied ClaimResponse via
	// res.Denial.Rationale (replaces pas.ParseDeniedRationale).
	rationale := ""
	if parsed.Denial != nil {
		rationale = parsed.Denial.Rationale
	}

	// Demo orchestration ONLY: the provider scenario queries the PHG denial view so
	// the console can show the patient view in one click. This stands in for the
	// patient app (the patient would query the PHG directly). It is INTENTIONALLY
	// fail-open — a PHG hiccup must not fail the real denial decision, which already
	// succeeded on the substrate. The patient-surfacing requirement is proven
	// INDEPENDENTLY of this convenience path by TestUC08_PatientSurfacingDirect
	// (which queries the PHG directly and fails if surfacing is skipped), so this
	// fail-open cannot silently hide a broken patient surface.
	var patientDenialReason, patientDenialReasonDisplay, patientDenialReasonSystem, patientAppeal string
	if g.cfg.PHGURL != "" {
		phgURL := g.cfg.PHGURL + "/denial?pci=" + res.pci
		phgReq, err2 := http.NewRequestWithContext(ctx, http.MethodGet, phgURL, nil)
		if err2 == nil {
			phgResp, err2 := g.cfg.Client.Do(phgReq)
			if err2 == nil {
				defer phgResp.Body.Close()
				var views []struct {
					ReasonCode    string `json:"reasonCode"`
					ReasonDisplay string `json:"reasonDisplay"`
					ReasonSystem  string `json:"reasonSystem"`
					Appeal        string `json:"appeal"`
				}
				if json.NewDecoder(io.LimitReader(phgResp.Body, shnsdk.MaxResponseBytes)).Decode(&views) == nil && len(views) > 0 {
					patientDenialReason = views[0].ReasonCode
					patientDenialReasonDisplay = views[0].ReasonDisplay
					patientDenialReasonSystem = views[0].ReasonSystem
					patientAppeal = views[0].Appeal
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, uc08Resp{
		PARequired:                 true,
		Denied:                     true,
		AuthNumber:                 "",
		Rationale:                  rationale,
		PatientAppeal:              patientAppeal,
		PatientDenialReason:        patientDenialReason,
		PatientDenialReasonDisplay: patientDenialReasonDisplay,
		PatientDenialReasonSystem:  patientDenialReasonSystem,
	})
}

// neededItemCodes maps []shnsdk.NeededItem → []string using .Code, matching what
// the internal ParsePendedOrApproved returned as a plain []string (Task.input
// valueString). The console/operator surface reads these as opaque codes.
func neededItemCodes(items []shnsdk.NeededItem) []string {
	if len(items) == 0 {
		return nil
	}
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Code
	}
	return out
}

// dtrOrder is the order the questionnaire step works from: the payer's
// updated order when the payer returned one, else the order sent.
func (r crdDtrResult) dtrOrder() []byte {
	if len(r.crdOrder) > 0 {
		return r.crdOrder
	}
	return r.srJSON
}

// crdCoverage reads the payer's coverage answer off its CDS Hooks response,
// wherever the payer put the coverage information (an update system action, a
// card suggestion's action, or the older card extension), without changing a
// byte: the first order's first coverage information.
func crdCoverage(body []byte) (shnsdk.CardCoverage, error) {
	obs, err := shnsdk.ParseCRDResponse(body)
	if err != nil {
		return shnsdk.CardCoverage{}, err
	}
	cov, ok := obs.Primary()
	if !ok {
		return shnsdk.CardCoverage{}, fmt.Errorf("engine: CRD response carries no coverage information")
	}
	return cov, nil
}

// eligibilityRequest builds the eligibility request for memberID. It names the
// provider (Practitioner/<NPI>) only when an NPI is configured; otherwise the
// request names no provider (CoverageEligibilityRequest.provider is optional)
// rather than a stand-in.
func (g *Gateway) eligibilityRequest(memberID string) ([]byte, error) {
	b, err := shnsdk.BuildEligibilityRequest(memberID, g.cfg.NPI, g.cfg.Clock())
	if err != nil || g.cfg.NPI != "" {
		return b, err
	}
	var cer map[string]json.RawMessage
	if err := json.Unmarshal(b, &cer); err != nil {
		return nil, err
	}
	delete(cer, "provider")
	return json.Marshal(cer)
}
