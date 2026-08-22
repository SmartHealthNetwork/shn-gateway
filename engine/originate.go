// originate.go — the provider-side scenario drivers (UC-01…08): originate a PA
// exchange, run it through the Hub, and surface the result. Part of package gateway
// (the Smart Gateway runs every holder role; this file is the provider-origination
// surface). Behavior-preserving split of gateway.go (finding C); no logic change.
// See gateway.go for the package doc.
package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// targetsBrPayer reports whether the origination profile targets a real Da Vinci PAS payer
// (br-payer), which needs the contained-insurer / absolute-refs / payer-org-entry / DTR-coverage
// handling AND the R-8 ingress-$validate skip for br-payer's relayed foreign bytes. provider-data
// is the sole br-payer-targeting origination lane — it must not regress the contained-payor →
// uniform-A3 bug OR $validate foreign relayed bytes (R-8). The sandbox lane is SHN-produced.
func targetsBrPayer(profile string) bool { return profile == "provider-data" }

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
	laned := func(line string) bool { return g.validatorForLine(line) != nil }
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
		if !peerLines[t] || g.validatorForLine(t) == nil {
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
// a prior successful selectLegRoute call and stored verbatim in pendState.
func (g *Gateway) selectResumeRoute(pinnedToken, recipient string) (legRoute, error) {
	contract, target, ok := strings.Cut(pinnedToken, "@")
	if !ok || contract == "" || target == "" {
		return legRoute{}, fmt.Errorf("engine: selectResumeRoute: malformed pin token %q", pinnedToken)
	}
	// Arm 1/2 equivalent: is the pinned target line buildable natively RIGHT
	// NOW (own declared may have grown to include it, or it may simply be
	// laned)? Either way, native beats a chain at resume too.
	for _, t := range g.nativeLinesView(contract) {
		if t == target && g.validatorForLine(t) != nil {
			return legRoute{Token: pinnedToken, BuildLine: target, Chain: nil}, nil
		}
	}
	// Arm 3: re-derive a chain from a CURRENT own-declared source to the
	// pinned target.
	ownLines := contractLineSet(g.declaredContractVersions(), contract)
	if g.validatorForLine(target) != nil {
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
		Contract: contract, LegType: "pas-claim-update", Recipient: recipient,
		Own:         sortedTokens(contract, ownLines),
		Peer:        []string{pinnedToken},
		BridgeIssue: "no bridge to the pinned line " + target + " remains available",
	}
}

// relayOriginationError surfaces a recipient's framed non-2xx application answer
// (a *RelayError from OriginateLeg, unwrapped through any %w chain) to the origination
// caller byte-identically — the same verbatim relay the Da Vinci ingress handlers do,
// for the bespoke /scenario* origination API. Returns true iff it wrote the response;
// callers keep their existing writeJSON fallback for every other (non-relay) error.
// Also writes a *RouteRefusalError (the version-routing legible refusal: no
// shared contract line ⇒ refuse legibly, never forward) as its 422 — one
// chokepoint covers every scenario and ingress
// origination site.
func (g *Gateway) relayOriginationError(w http.ResponseWriter, err error) bool {
	var rre *RouteRefusalError
	if errors.As(err, &rre) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": rre.Error()})
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
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(re.Status)
	_, _ = w.Write(re.Body)
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
	// each scenario reads its OWN seeded Coverage; the sandbox lane keeps the shared
	// MBR-COVERED/MBR-NOTCOVERED defaults (byte-identical — sceneMember returns the
	// default literal for non-provider-data OriginationProfile).
	var memberID string
	var ok bool
	switch req.Branch {
	case "covered":
		memberID, ok = g.scenarioMember(w, r, "MBR-COVERED", "MBR-PD-UC01")
	case "notcovered":
		memberID, ok = g.scenarioMember(w, r, "MBR-NOTCOVERED", "MBR-PD-UC01-NC")
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown branch"})
		return
	}
	if !ok {
		return
	}

	pci, _, found := g.cfg.SoR.ResolvePatient(memberID)
	if !found {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown member"})
		return
	}

	// Read-ADD (FR-G40): UC-01 reads the member's OWN open Coverage as the routing/identity SOURCE
	// (the eligibility leg's recipient is resolved from realCov). The eligibility REQUEST itself
	// stays bare-insurer (BuildEligibilityRequest unchanged — insurer-coherence deferred this slice),
	// so this read does not change the payload bytes; it only fails closed when the member has no
	// coverage / no parseable payer on file (AI-G11 / OWD-G10).
	realCov, hasCov := g.cfg.SoR.OpenCoverage(memberID)
	if !hasCov {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "no coverage on file for member"})
		return
	}
	// Route the eligibility leg to the payer HOLDER resolved from the member's own Coverage
	// (FR-G40): no default — a miss (no parseable payer / no directory mapping) fails closed HERE
	// before any leg (AI-G11 / OWD-G10). The eligibility REQUEST stays bare-insurer (unchanged), so
	// UC-01 discards the parsed payer identity — it only routes.
	recipient, _, status, msg := g.recipientForWith(realCov, g.cfg.SoR.ResolveByReference)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	cerJSON, err := shnsdk.BuildEligibilityRequest(memberID, g.cfg.NPI, g.cfg.Clock())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build request failed"})
		return
	}

	// Egress validation is load-bearing: an invalid resource must never reach the
	// substrate. Empty profile = base-R4 + meta.profile pinning (see roundTrip).
	// F7: lane-selected per line like every other validate, but deliberately
	// NOT g.validateFHIR — this site echoes res.Issues in its 422 body, which
	// validateFHIR's (status,msg) contract cannot carry. coverage-eligibility is
	// version-neutral, so the line is "" (the canonical lane).
	cerValidator := g.validatorForLine("")
	if cerValidator == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "no FHIR validator lane configured (FR-36/FR-G29)"})
		return
	}
	res, err := cerValidator.Validate(ctx, cerJSON, "")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "validator unavailable"})
		return
	}
	if !res.Valid {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "egress validation failed",
			"issues": res.Issues,
		})
		return
	}

	// Generate the correlationID BEFORE authorizing so the token is bound to the
	// exact envelope it will ride in (C2): token.CorrelationID == envelope CID.
	correlationID := g.cfg.CorrelationGen()

	// UC-01 uses the SAME authorized-sealed-leg helper as UC-02/03 (OriginateLeg →
	// roundTrip): authorize(eligibility-inquiry) → seal → Hub /route → verify the response leg
	// (respOp eligibility-response, bound to this correlationID, Sender=="payer",
	// subject==pci) → decrypt. Folding UC-01 onto the shared helper keeps the
	// trust-critical response-leg verification in ONE place — no duplicated copy to
	// drift.
	crrJSON, err := g.OriginateLeg(ctx, r, recipient, "coverage-eligibility", pci, correlationID, "", Content{WorkstreamType: workstreamPA, Bytes: cerJSON})
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
	crrValidator := g.validatorForLine("")
	if crrValidator == nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "no FHIR validator lane configured (FR-36/FR-G29)"})
		return
	}
	ingress, err := crrValidator.Validate(ctx, crrJSON, "")
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "validator unavailable"})
		return
	}
	if !ingress.Valid {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "ingress validation failed"})
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
	// AuthNumber/ValidUntil/QRItems are omitempty. The UC-04/06 amendment tail resolves to a
	// genuine terminal A1, so the approve path always carries a non-empty AuthNumber — there
	// is no terminal-pend response.
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
}

// crdDtrResult carries the outputs of the CRD+DTR prefix shared by UC-03/04/06.
type crdDtrResult struct {
	qrJSON, srJSON []byte
	// questionnaireJSON is the bare Questionnaire extracted from the fetched $questionnaire-package
	// (nil on the no-DTR paths). The provider-data UC-04 lane re-fills it by attestation when the
	// operated $populate auto-pops nothing (the adaptive HomeHealthAssessment has 0 CQL items).
	questionnaireJSON       []byte
	patientRef, coverageRef string
	// member is the BARE member id this exchange originated for — the value every PAS-lane
	// producer stamps as the Coverage's urn:shn:coverage MB identifier (that identifier is a
	// member number, not a reference) AND as the value of the conformant Claim's
	// insurance[0].coverage LOGICAL reference — the same bare value in both roles. It rides
	// beside coverageRef, which stays the Reference-shaped value the QR-context / native-lane
	// roles need.
	member string
	pci    string
	filled []FilledItem
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

// orderSource returns the origination order bytes for the active profile. Under
// provider-data it reads the member's open order from the SoR (the order code/dx
// trace to the provider's seeded data, never a literal); otherwise it builds the order
// from the per-UC tuple (the self-contained sandbox demo). The else branch
// keeps the exact BuildServiceRequestCoded call verbatim so sandbox stays byte-identical.
// Returns (orderJSON, httpStatus, msg); status 0 == ok.
func (g *Gateway) orderSource(member, patientRef, system, code, display, dx string) ([]byte, int, string) {
	if g.cfg.OriginationProfile == "provider-data" {
		order, ok := g.cfg.SoR.OpenOrder(member)
		if !ok {
			return nil, http.StatusBadGateway, "no open order for member in SoR"
		}
		// The product coding comes from the DATA (ServiceRequest.code / DeviceRequest
		// codeCodeableConcept), never a literal — fail closed if it carries no {CPT,HCPCS} coding.
		if _, _, _, err := shnsdk.ParseOrderProductCoding(order); err != nil {
			return nil, http.StatusBadGateway, "open order has no recognized product coding"
		}
		return order, 0, ""
	}
	sr, err := BuildServiceRequestCoded(system, code, display, dx, patientRef)
	if err != nil {
		return nil, http.StatusInternalServerError, "build request failed"
	}
	return sr, 0, ""
}

// sceneMember returns the distinct provider-data persona member for a scenario, or the
// sandbox default otherwise. Distinct members are REQUIRED in the provider-data lane:
// the order is read via OpenOrder(member) (keyed on member ONLY), so two scenarios sharing a
// member would read the same order. The sandbox lane keeps its default member (the order is
// built from the per-UC tuple there, so a shared member is harmless and byte-identical).
func (g *Gateway) sceneMember(defaultMember, providerDataMember string) string {
	if g.cfg.OriginationProfile == "provider-data" {
		return providerDataMember
	}
	return defaultMember
}

// scenarioMember resolves the effective member for a /scenario/* request: the
// lane's sceneMember default, remapped to its dedicated canary twin when the
// request carries ?personaSet=canary (observability Phase 3, settled decision
// #1 — the monitor canary must never drive the shared demo personas). Fails
// closed 400 on an unknown personaSet value or a member with no twin: a typo
// silently running the demo personas is exactly the collision the twins exist
// to prevent. ok=false means the response is already written.
func (g *Gateway) scenarioMember(w http.ResponseWriter, r *http.Request, defaultMember, providerDataMember string) (string, bool) {
	member := g.sceneMember(defaultMember, providerDataMember)
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
// proceedOnNotCovered (the provider-data handleUC08 caller): when true, a not-covered CRD
// verdict does NOT terminally stop — the order is returned so the caller can carry it to PAS for
// br-payer's formal A2 "Not Certified" ClaimResponse (D-S2-2). handleUC08 passes
// targetsBrPayer(profile), which is true only for provider-data (the live br-payer lane); the
// sandbox lane keeps the generic FR-G25/AI-1 STOP (false). The generic STOP is the DEFAULT
// (false) for every other caller and is unchanged. The opt-in never yields an auth on a denial:
// handleUC08 asserts the PAS result is DENIED (its approved→502 guard), so a not-covered order
// routed to PAS can only deny, never approve.
func (g *Gateway) runCRDThenDTROrder(w http.ResponseWriter, r *http.Request, member, system, code, display, dx string, proceedOnNotCovered bool) (crdDtrResult, bool) {
	ctx := r.Context()

	pci, _, found := g.cfg.SoR.ResolvePatient(member)
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
	realCov, found := g.cfg.SoR.OpenCoverage(member)
	if !found {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "no coverage on file for member"})
		return crdDtrResult{}, false
	}
	// Resolve the payer HOLDER every leg of this exchange routes to AND the parsed payer identity in
	// ONE parse of the member's own Coverage (FR-G40): no default — a miss fails closed HERE before
	// any leg (AI-G11 / OWD-G10). `payer` (the parsed identity) threads to the PAS builders below, so
	// routed-payer and payload-payer cannot diverge (one payer fact, read once).
	recipient, payer, status, msg := g.recipientForWith(realCov, g.cfg.SoR.ResolveByReference)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return crdDtrResult{}, false
	}

	srJSON, status, msg := g.orderSource(member, patientRef, system, code, display, dx)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return crdDtrResult{}, false
	}
	if status, msg := g.validateFHIR(ctx, srJSON, "egress", ""); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return crdDtrResult{}, false
	}

	// crd-order-select keeps the CONTAINED-payor egress form (BuildCoverageWithPayer); only the
	// payer IDENTITY now derives from realCov (no synthetic CMS payer). The urn:shn:coverage MB
	// identifier carries the BARE member id (a member number, not a reference) — coverageRef
	// stays the reference-shaped value the QRContext / native-lane roles carried on
	// crdDtrResult need (the conformant PAS
	// Claim's insurance[0].coverage is a logical reference built from `member`, not from it).
	coverageJSON, err := shnsdk.BuildCoverageWithPayer(patientRef, member, payer)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build coverage failed"})
		return crdDtrResult{}, false
	}
	if status, msg := g.validateFHIR(ctx, coverageJSON, "egress", ""); status != 0 {
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
	crdReq, err := shnsdk.BuildConformantOrderSelectRequest(srJSON, coverageJSON, patientRef)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build order-select failed"})
		return crdDtrResult{}, false
	}
	// Validation posture is UNCHANGED by the promotion (D-7 is routing-only):
	// there is deliberately NO validateFHIR enforcement point after egressAdapt
	// on this leg. crdReq is CDS Hooks JSON — a transport ENVELOPE, not itself a
	// FHIR resource the pa.crd compat-manifest rows model. The FHIR resources it
	// carries (srJSON, coverageJSON) are each $validated above, before assembly,
	// exactly as before. Same rationale as the DTR-fetch sites below.
	adaptedCRDReq, _, err := g.egressAdapt(crdRoute, crdReq, ExchangeIdentity{CorrelationID: crdCorr, LegType: "crd-order-select", Counterpart: recipient})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return crdDtrResult{}, false
	}
	crdRespJSON, err := g.OriginateLeg(ctx, r, recipient, "crd-order-select", pci, crdCorr, "",
		Content{WorkstreamType: workstreamPA, ProfileID: crdRoute.Token, Route: routeInfoFor(crdRoute), Bytes: adaptedCRDReq})
	if err != nil {
		if g.relayOriginationError(w, err) {
			return crdDtrResult{}, false
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return crdDtrResult{}, false
	}
	cov, err := shnsdk.ParseCards(crdRespJSON)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "card parse failed"})
		return crdDtrResult{}, false
	}
	// Generic verdict-driven switch on the CRD card result (FR-G25). A
	// config-only gateway must handle ANY conformant CRD verdict it observes, not just
	// the sandbox shape (always covered + PA + DTR). The PA decision keys on the
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
			return crdDtrResult{srJSON: srJSON, patientRef: patientRef, coverageRef: coverageRef, member: member, pci: pci, payer: payer, recipient: recipient}, true
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
	// DTR questionnaire is fetched is decided by the doc-needed axis (NeedsDTR),
	// independently: no-doc (br-payer L8000) skips DTR straight to PAS; clinical
	// (br-payer G0151) routes DTR.
	res := crdDtrResult{srJSON: srJSON, patientRef: patientRef, coverageRef: coverageRef, member: member, pci: pci, payer: payer, recipient: recipient}
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
		// In the br-payer-targeting lane (provider-data), carry the Coverage so the
		// native-forward re-emits the payer-required coverage param on
		// $questionnaire-package — a real Da Vinci payer (br-payer) 400s without it (the
		// v0.11.0 QuestionnaireFetchRequest.Coverage seam). At 2.0/2.1 the sandbox
		// responder doesn't need it, so gate on the profile to keep that leg
		// byte-identical (C1 discipline) — but at a line whose DTRDef requires it
		// (2.2), Coverage is ALWAYS attached, honest (the SAME SoR-derived resource
		// built above), regardless of profile: the sandbox responder's own 2.2 QR
		// shell (adjudicator.go) reads it too, to close the DTR-at-2.2 sandbox gap
		// honestly rather than fabricating a subject/coverage reference.
		fetch := shnsdk.QuestionnaireFetchRequest{Canonical: canonical}
		coverageRequired := false
		if def, ok := shnsdk.DTRLineDef(res.dtrLine); ok {
			coverageRequired = def.QuestionnairePackageCoverageRequired
		}
		if targetsBrPayer(g.cfg.OriginationProfile) || coverageRequired {
			fetch.Coverage = coverageJSON
		}
		dtrReq, err := json.Marshal(fetch)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build dtr request failed"})
			return crdDtrResult{}, false
		}
		// egressAdapt runs here per the select-before-build pipeline, but there is
		// deliberately NO validateFHIR enforcement point after it on THIS leg: dtrReq
		// is QuestionnaireFetchRequest JSON, a transport ENVELOPE (canonical +
		// optional Coverage), not itself a FHIR resource the pa.dtr compat-manifest
		// rows model — every other select-before-build site validates its ADAPTED
		// bytes at the target lane immediately after this call; this one cannot,
		// because there is no FHIR shape to validate. OBLIGATION DISCHARGED (the
		// multi-version spec's recorded DTR-fetch known-gap
		// entry): egressAdapt's envelopeEgressLegs carve-out routes
		// "dtr-questionnaire-fetch" through a byte-identical pass-through instead of
		// applyChain, safe BY CONSTRUCTION (the step funcs never see the envelope
		// bytes, so they can never re-marshal them) — proven by
		// TestEnvelopeLegChainIsByteIdenticalPassThrough, the enforcement fence
		// standing in for the impossible envelope $validate.
		adaptedDTRReq, _, err := g.egressAdapt(route, dtrReq, ExchangeIdentity{CorrelationID: dtrCorr, LegType: "dtr-questionnaire-fetch", Counterpart: recipient})
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return crdDtrResult{}, false
		}
		packageJSON, err := g.OriginateLeg(ctx, r, recipient, "dtr-questionnaire-fetch", pci, dtrCorr, "",
			Content{WorkstreamType: workstreamPA, ProfileID: route.Token, Route: routeInfoFor(route), Bytes: adaptedDTRReq})
		if err != nil {
			if g.relayOriginationError(w, err) {
				return crdDtrResult{}, false
			}
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return crdDtrResult{}, false
		}
		if status, msg := g.validateFHIR(ctx, packageJSON, "ingress", res.dtrLine); status != 0 {
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
		// store-resolvable Patient ref (a scoped id), NOT the logical SHN ref. Resolve it via the SoR
		// (the stub returns the logical ref, so the managed/hermetic path is unchanged). Falls back to
		// the logical ref when the SoR can't resolve it.
		subjectFHIRRef := patientRef
		if ref, ok := g.cfg.SoR.PatientFHIRRef(member); ok && ref != "" {
			subjectFHIRRef = ref
		}
		qrJSON, fill, err := g.cfg.Populator.Populate(ctx, packageJSON, PopulateContext{
			Member:         member,
			PatientRef:     patientRef,
			SubjectFHIRRef: subjectFHIRRef,
			CoverageRef:    coverageRef,
			OrderRef:       "ServiceRequest/sr-" + member,
			Authored:       g.cfg.Clock(),
			Line:           res.dtrLine,
		})
		if err != nil {
			writeJSON(w, statusForPopulateErr(err), map[string]string{"error": err.Error()})
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

		if status, msg := g.validateFHIR(ctx, qrJSON, "egress", res.dtrLine); status != 0 {
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

// statusForPopulateErr maps a Populator error to an HTTP status. A managed
// FillQuestionnaire marshal/unsupported error is the gateway's own fault → 500
// (behavior-preserving: the inline path returned 500 here, and this never trips on the
// 8 sandbox scenarios). errNoClinicalContext (a data fault) and errPopulateUpstream
// (a native $populate fault) are partner/data faults → 502.
func statusForPopulateErr(err error) int {
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
	subj, err := json.Marshal(map[string]string{"reference": ref})
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
// DeviceRequest (HospitalBeds → covered/no-PA/no-DTR); the sandbox lane originates the per-UC
// tuple (an X-ray order). Surfaces the covered/no-PA/no-DTR triple (NeedsDTR is the
// reasonCode discriminator, asserted by the live gate).
func (g *Gateway) handleUC02(w http.ResponseWriter, r *http.Request) {
	// UC-02 (no-PA) originates the seeded E0250 hospital-bed DeviceRequest off provider data
	// (the MBR-PD-UC02 persona) — D-PD-2 is dropped. The order is read via orderSource → OpenOrder
	// in the provider-data lane (it traces to the provider's seeded SoR, never a literal); a
	// mis-seeded member with no open order fails closed at OpenOrder. The sandbox lane originates
	// the no-PA order off the per-UC tuple (byte-unchanged).
	member, ok := g.scenarioMember(w, r, "MBR-COVERED", "MBR-PD-UC02")
	if !ok {
		return
	}
	g.originateNoPACRD(w, r, member)
}

// handleUC02PayerB is the LIVE second-payer self-discovery proof (FR-G41): the MBR-PD-UC02-PB
// persona's own Coverage names a DISTINCT payer identity (urn:oid:…300|00078) which the provider
// gateway's FeedPayerRouter resolves to holder `payer-b` off the /holders feed — while the persona-A
// scenarios (MBR-COVERED / 00001) resolve to `payer`. Both are self-discovered off the SAME feed with
// no static per-provider directory (the many-to-many drop-in property). MBR-PD-UC02-PB is seeded into
// the provider tenant by cmd/fhirseed (shnsdk provider-data persona "uc02-payerb"). This proves
// ROUTING (both members seal to DIFFERENT holders), not a distinct verdict — differential adjudication
// is a Connectathon-day property.
func (g *Gateway) handleUC02PayerB(w http.ResponseWriter, r *http.Request) {
	member, ok := g.scenarioMember(w, r, "MBR-PD-UC02-PB", "MBR-PD-UC02-PB")
	if !ok {
		return
	}
	g.originateNoPACRD(w, r, member)
}

// handleUC02UnknownPayer is the LIVE FeedPayerRouter fail-closed proof (AI-G11/AI-G12): the
// MBR-UNKNOWN-PAYER persona's Coverage names a payer identity (urn:oid:…300|00099) that NO holder
// claims in the feed, so the coverage-derived resolution fails closed with a legible 422 ("no
// registered payer for identifier …|00099") — never a default payer. Seeded into the provider tenant
// by cmd/fhirseed (a smoke-only negative persona, not an SDK partner-onboarding fixture).
func (g *Gateway) handleUC02UnknownPayer(w http.ResponseWriter, r *http.Request) {
	member, ok := g.scenarioMember(w, r, "MBR-UNKNOWN-PAYER", "MBR-UNKNOWN-PAYER")
	if !ok {
		return
	}
	g.originateNoPACRD(w, r, member)
}

// originateNoPACRD runs the no-PA CRD round-trip for member: read the member's OWN Coverage as the
// routing/identity source (FR-G40/G41 — the payer holder is resolved off Coverage.payor, no default),
// originate a crd-order-select leg, and surface the covered/no-PA/no-DTR triple. Shared by handleUC02
// (persona-A) and the second-payer / unknown-payer routing proofs.
func (g *Gateway) originateNoPACRD(w http.ResponseWriter, r *http.Request, member string) {
	ctx := r.Context()

	pci, _, found := g.cfg.SoR.ResolvePatient(member)
	if !found {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown member"})
		return
	}
	patientRef := "Patient/" + member

	o := originationCodes(g.cfg.OriginationProfile).uc02
	srJSON, status, msg := g.orderSource(member, patientRef, o.system, o.code, o.display, o.dx)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	if status, msg := g.validateFHIR(ctx, srJSON, "egress", ""); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	// Read the member's OWN open Coverage as the routing/identity SOURCE (FR-G40); the egress
	// CRD coverage keeps the contained-payor form, now carrying the REAL payer identity (no
	// synthetic CMS literal). realCov stays a LOCAL (the recipient is resolved from it).
	realCov, hasCov := g.cfg.SoR.OpenCoverage(member)
	if !hasCov {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "no coverage on file for member"})
		return
	}
	// Resolve the CRD leg's payer HOLDER AND the parsed payer identity in ONE parse of the member's
	// own Coverage (FR-G40): no default — a miss fails closed HERE before the leg (AI-G11 / OWD-G10).
	// `payer` threads to the CRD coverage builder, so routed-payer and payload-payer cannot diverge.
	recipient, payer, status, msg := g.recipientForWith(realCov, g.cfg.SoR.ResolveByReference)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	// urn:shn:coverage carries the BARE member id (a member number, not a reference).
	coverageJSON, err := shnsdk.BuildCoverageWithPayer(patientRef, member, payer)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build coverage failed"})
		return
	}
	// Every FHIR resource crossing the substrate is validated — the Coverage in
	// the CRD prefetch included.
	if status, msg := g.validateFHIR(ctx, coverageJSON, "egress", ""); status != 0 {
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
	reqJSON, err := shnsdk.BuildConformantOrderSelectRequest(srJSON, coverageJSON, patientRef)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build order-select failed"})
		return
	}
	// Validation posture UNCHANGED (routing-only promotion): reqJSON is a CDS
	// Hooks envelope, not a FHIR resource; its carried resources are $validated
	// above. No enforcement point is added or removed after egressAdapt.
	adaptedReqJSON, _, err := g.egressAdapt(crdRoute, reqJSON, ExchangeIdentity{CorrelationID: correlationID, LegType: "crd-order-select", Counterpart: recipient})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	respJSON, err := g.OriginateLeg(ctx, r, recipient, "crd-order-select", pci, correlationID, "",
		Content{WorkstreamType: workstreamPA, ProfileID: crdRoute.Token, Route: routeInfoFor(crdRoute), Bytes: adaptedReqJSON})
	if err != nil {
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	cov, err := shnsdk.ParseCards(respJSON)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "card parse failed"})
		return
	}

	// The SDK's ParseCards drops the card Summary; do a small inline parse of the
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

// handleUC03 runs the full PA-required path: CRD (must require PA) → DTR fetch +
// local auto-fill → PAS submit → approval. On approval the provider stores the
// auth number for the SR (FR-23) and answers paRequired=true. Off provider-data,
// the request body selects the member the same way handleScenario's uc01 does
// (scenarioReq.Branch): "" (or an absent body) is MBR-COVERED, byte-identical
// to before this switch existed; "bridge-refuse" is the kit-bridging-visualization
// demo persona MBR-BRIDGE-REFUSE; any other value 400s.
func (g *Gateway) handleUC03(w http.ResponseWriter, r *http.Request) {
	if g.cfg.OriginationProfile == "provider-data" {
		// UC-03 off provider data = the HomeOxygenDispatch path (the only br-payer family that PA-requires
		// + launches a DTR + GENUINELY auto-fills it via operated $populate). E1390 vs HomeOxygen's E0431;
		// the approval is br-payer's pend-resolution timer (D-2RI-3), and the genuine auto-fill (the $populate
		// runs the real prepop CQL against the seeded O2 obs) is UC-03's distinctive. The implicit fail-closed
		// at OpenOrder is preserved (a mis-seeded MBR-PD-UC03 with no E1390 order fails closed there).
		member, ok := g.scenarioMember(w, r, "MBR-PD-UC03", "MBR-PD-UC03")
		if !ok {
			return
		}
		g.originateDispatch(w, r, member)
		return
	}

	ctx := r.Context()
	const srRef = "ServiceRequest/sr-uc03"

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

	var member string
	var ok bool
	switch req.Branch {
	case "":
		member, ok = g.scenarioMember(w, r, "MBR-COVERED", "MBR-COVERED")
	case "bridge-refuse":
		// Both scenarioMember args are the demo refuse persona (mirrors the
		// ""-branch literal above: this path never runs under
		// OriginationProfile=="provider-data" — that profile early-returns
		// above — so providerDataMember is dead here exactly like it was for
		// the literal it replaces). ?personaSet=canary is NOT special-cased:
		// scenarioMember's CanaryTwins lookup naturally 400s ("no canary
		// twin for member MBR-BRIDGE-REFUSE") since no twin is registered —
		// deliberately not adding one (reviewer ruling: a canary twin for a
		// demo-only persona would be semantic abuse of the canary
		// mechanism, which exists for observability's shared scenario
		// members, not visualization fixtures).
		member, ok = g.scenarioMember(w, r, "MBR-BRIDGE-REFUSE", "MBR-BRIDGE-REFUSE")
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown branch"})
		return
	}
	if !ok {
		return
	}
	o := originationCodes(g.cfg.OriginationProfile).uc03
	res, ok := g.runCRDThenDTROrder(w, r, member, o.system, o.code, o.display, o.dx, false)
	if !ok {
		return
	}

	// --- PAS round-trip: submit the preauth bundle, expect an approval. ---
	pasCorr := g.cfg.CorrelationGen()
	// Select-before-build: the routed line CHOOSES the builder, so it is
	// resolved BEFORE the bundle exists. Also the pended-line pin for any
	// pas-claim-update leg downstream — pas-claim and pas-claim-update share the
	// pa.pas contract, so one selection is contract-correct for both.
	route, ok := g.selectLegLineOrFail(w, res.recipient, "pas-claim", pasCorr)
	if !ok {
		return
	}
	targetLine := shnsdk.LineOf(route.Token)
	bundleJSON, err := shnsdk.BuildConformantClaimBundleAtLine(route.BuildLine, shnsdk.ConformantClaimInputs{
		QR: res.qrJSON, SR: res.srJSON, PatientRef: res.patientRef, CoverageRef: res.coverageRef, MemberID: res.member,
		Corr: pasCorr, Created: g.cfg.Clock(),
		ContainedInsurer: targetsBrPayer(g.cfg.OriginationProfile),
		AbsoluteRefs:     targetsBrPayer(g.cfg.OriginationProfile),
		PayerOrgEntry:    targetsBrPayer(g.cfg.OriginationProfile), // payer Org as a resolvable PAS bundle entry (br-payer findInBundle)
		Payer:            res.payer,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build bundle failed"})
		return
	}
	bundleJSON, _, err = g.egressAdapt(route, bundleJSON, ExchangeIdentity{CorrelationID: pasCorr, LegType: "pas-claim", Counterpart: res.recipient})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if status, msg := g.validateFHIR(ctx, bundleJSON, "egress", targetLine); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	claimRespJSON, err := g.OriginateLeg(ctx, r, res.recipient, "pas-claim", res.pci, pasCorr, "",
		Content{WorkstreamType: workstreamPA, ProfileID: route.Token, Route: routeInfoFor(route), Bytes: bundleJSON})
	if err != nil {
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if status, msg := g.validateFHIR(ctx, claimRespJSON, "ingress", targetLine); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	parsed, err := shnsdk.ParseClaimResponse(claimRespJSON)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "claim response parse failed"})
		return
	}
	if parsed.Outcome != "approved" {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "preauthorization not approved"})
		return
	}

	// FR-23: persist the payer-issued auth number against the SR reference.
	if err := g.cfg.Store.StoreAuthNumber(srRef, parsed.PreAuthRef); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "holder write failed (auth number)"})
		return
	}

	writeJSON(w, http.StatusOK, uc03Resp{
		PARequired: true,
		AuthNumber: parsed.PreAuthRef,
		ValidUntil: parsed.ValidUntil,
		QRItems:    res.filled,
	})
}

// handleUC07HCPCS runs the HCPCS (L8000) DV-approve path — the in-process mirror of the
// two-RI L8000 approve: CRD (PA-required) → DTR auto-fill → PAS approve on first
// submit → the payer projects a HCPCS-system PDex PA EOB into the Patient-Access Store
// (FR-28; system flows from the L8000 order). NOT patient-authorship.
func (g *Gateway) handleUC07HCPCS(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	const srRef = "ServiceRequest/sr-uc07hcpcs"

	member, ok := g.scenarioMember(w, r, "MBR-UC07HCPCS", "MBR-UC07HCPCS")
	if !ok {
		return
	}
	o := originationCodes(g.cfg.OriginationProfile).uc07hcpcs
	res, ok := g.runCRDThenDTROrder(w, r, member, o.system, o.code, o.display, o.dx, false)
	if !ok {
		return
	}

	// --- PAS round-trip: submit the preauth bundle, expect an approval. ---
	pasCorr := g.cfg.CorrelationGen()
	// Select-before-build: the routed line CHOOSES the builder, so it is
	// resolved BEFORE the bundle exists. Also the pended-line pin for any
	// pas-claim-update leg downstream — pas-claim and pas-claim-update share the
	// pa.pas contract, so one selection is contract-correct for both.
	route, ok := g.selectLegLineOrFail(w, res.recipient, "pas-claim", pasCorr)
	if !ok {
		return
	}
	targetLine := shnsdk.LineOf(route.Token)
	bundleJSON, err := shnsdk.BuildConformantClaimBundleAtLine(route.BuildLine, shnsdk.ConformantClaimInputs{
		QR: res.qrJSON, SR: res.srJSON, PatientRef: res.patientRef, CoverageRef: res.coverageRef, MemberID: res.member,
		Corr: pasCorr, Created: g.cfg.Clock(),
		ContainedInsurer: targetsBrPayer(g.cfg.OriginationProfile),
		AbsoluteRefs:     targetsBrPayer(g.cfg.OriginationProfile),
		PayerOrgEntry:    targetsBrPayer(g.cfg.OriginationProfile), // payer Org as a resolvable PAS bundle entry (br-payer findInBundle)
		Payer:            res.payer,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build bundle failed"})
		return
	}
	bundleJSON, _, err = g.egressAdapt(route, bundleJSON, ExchangeIdentity{CorrelationID: pasCorr, LegType: "pas-claim", Counterpart: res.recipient})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if status, msg := g.validateFHIR(ctx, bundleJSON, "egress", targetLine); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	claimRespJSON, err := g.OriginateLeg(ctx, r, res.recipient, "pas-claim", res.pci, pasCorr, "",
		Content{WorkstreamType: workstreamPA, ProfileID: route.Token, Route: routeInfoFor(route), Bytes: bundleJSON})
	if err != nil {
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if status, msg := g.validateFHIR(ctx, claimRespJSON, "ingress", targetLine); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	parsed, err := shnsdk.ParseClaimResponse(claimRespJSON)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "claim response parse failed"})
		return
	}
	if parsed.Outcome != "approved" {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "preauthorization not approved"})
		return
	}

	// FR-23: persist the payer-issued auth number against the SR reference.
	if err := g.cfg.Store.StoreAuthNumber(srRef, parsed.PreAuthRef); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "holder write failed (auth number)"})
		return
	}

	writeJSON(w, http.StatusOK, uc03Resp{
		PARequired: true,
		AuthNumber: parsed.PreAuthRef,
		ValidUntil: parsed.ValidUntil,
		QRItems:    res.filled,
	})
}

// classifyResolution classifies a PAS (update) ClaimResponse at a resolution site as approved or
// not. The amendment resolves to a genuine terminal A1 — the payer-gw responder polls br-payer's
// timer-driven A4→A1 and returns the resolved A1 (or 422→OriginateLeg err on non-resolution), so a
// resolution site sees ONLY approved | denied | error here, never a live pend. Anything not
// approved → caller 502s (a denial or an unresolved pend is a genuine non-approval, never a silent
// pass — C1).
func (g *Gateway) classifyResolution(respJSON []byte) (parsed shnsdk.PriorAuthResult, approved bool) {
	p, err := shnsdk.ParseClaimResponse(respJSON)
	if err == nil && p.Outcome == "approved" {
		return p, true
	}
	return shnsdk.PriorAuthResult{}, false
}

// handleUC04 runs the pended-then-approved PA path. Two profile lanes share the CRD+DTR prefix:
//   - sandbox: CRD+DTR → PAS submit → PENDED (no operative DiagnosticReport yet) →
//     ClaimUpdate with the provider-LOCAL operative report + Provenance → approved (FR-20/21).
//   - provider-data (L1): ATTEST the adaptive HomeHealthAssessment off the seeded order (the
//     operated $populate auto-pops nothing), then the lean single-shot PAS tail with NO
//     amendment (D-PD-1 defers the operative-DiagnosticReport amendment). The attested QR is
//     verdict-INERT — br-payer's A4→A1 is its pend-resolution timer, not a QR-driven verdict.
func (g *Gateway) handleUC04(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	const srRef = "ServiceRequest/sr-uc04"

	o := originationCodes(g.cfg.OriginationProfile).uc04
	member, ok := g.scenarioMember(w, r, "MBR-UC04", "MBR-PD-UC04")
	if !ok {
		return
	}
	res, ok := g.runCRDThenDTROrder(w, r, member, o.system, o.code, o.display, o.dx, false)
	if !ok {
		return
	}

	if g.cfg.OriginationProfile == "provider-data" {
		// provider-data (L1): ATTEST the adaptive HomeHealthAssessment questionnaire off the seeded
		// order ($populate auto-pops nothing), then the lean single-shot tail (D-PD-1: no
		// amendment). Every attested answer traces to the seeded order (res.srJSON); the attested QR
		// is verdict-INERT — br-payer's A4→A1 is its pend-resolution timer, not a QR-driven verdict.
		answers, err := uc04AttestationAnswers(res.srJSON, g.cfg.SoR.ResolveByReference)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		orderRef, ok := resourceRef(res.srJSON) // Bug-2: persist against the REAL seeded order ref, not the sandbox literal.
		if !ok {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "order missing id"})
			return
		}
		// Attest at the selected DTR line (closes an earlier KNOWN GAP: this QR
		// used to be built at the frozen 2.0 shape regardless of the selected line — the
		// 2.2 two-RI run showed wrong-line QR bytes could pass SILENTLY, the UC-03 auto-fill
		// site this closes). The HomeHealthAssessment is SDC-ADAPTIVE: the attestation
		// drives $next-question so the group the attested answers belong to is DELIVERED
		// before the fill (dtr_adaptive.go) — a fill over the package's group-1 tree alone
		// drops every group-3 answer and puts 1.1 alone on the wire.
		attestedQR, _, status, msg, err := g.attestAdaptiveQuestionnaire(ctx, r, res, answers,
			"Organization/"+g.cfg.HolderID,
			shnsdk.QRContext{PatientRef: res.patientRef, CoverageRef: res.coverageRef, OrderRef: orderRef, Authored: g.cfg.Clock()})
		if status != 0 {
			if g.relayOriginationError(w, err) {
				return
			}
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
		if status, msg := g.validateFHIR(ctx, attestedQR, "egress", res.dtrLine); status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
		parsed, _, status, msg, err := g.submitClaimAndResolve(ctx, r, res.pci, res.srJSON, attestedQR, res.patientRef, res.coverageRef, res.member, res.payer, res.recipient)
		if status != 0 {
			if g.relayOriginationError(w, err) {
				return
			}
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
		if err := g.cfg.Store.StoreAuthNumber(orderRef, parsed.PreAuthRef); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "holder write failed (auth number)"})
			return
		}
		// Surface the attested answer VALUES (the traces-to-seed evidence, the UC-04 analog of
		// HomeOxygen's qrAnswers).
		writeJSON(w, http.StatusOK, uc03Resp{PARequired: true, AuthNumber: parsed.PreAuthRef, ValidUntil: parsed.ValidUntil, QRAnswers: attestedAnswerValues(answers)})
		return
	}

	// sandbox: the operative-DiagnosticReport amendment tail (UNCHANGED below).
	// PAS submit — expect PENDED (no operative DiagnosticReport yet).
	pasCorr := g.cfg.CorrelationGen()
	// Select-before-build: the routed line CHOOSES the builder, so it is
	// resolved BEFORE the bundle exists. Also the pended-line pin for any
	// pas-claim-update leg downstream — pas-claim and pas-claim-update share the
	// pa.pas contract, so one selection is contract-correct for both.
	route, ok := g.selectLegLineOrFail(w, res.recipient, "pas-claim", pasCorr)
	if !ok {
		return
	}
	targetLine := shnsdk.LineOf(route.Token)
	bundleJSON, err := shnsdk.BuildConformantClaimBundleAtLine(route.BuildLine, shnsdk.ConformantClaimInputs{
		QR: res.qrJSON, SR: res.srJSON, PatientRef: res.patientRef, CoverageRef: res.coverageRef, MemberID: res.member,
		Corr: pasCorr, Created: g.cfg.Clock(),
		ContainedInsurer: targetsBrPayer(g.cfg.OriginationProfile),
		AbsoluteRefs:     targetsBrPayer(g.cfg.OriginationProfile),
		PayerOrgEntry:    targetsBrPayer(g.cfg.OriginationProfile), // payer Org as a resolvable PAS bundle entry (br-payer findInBundle)
		Payer:            res.payer,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build bundle failed"})
		return
	}
	bundleJSON, _, err = g.egressAdapt(route, bundleJSON, ExchangeIdentity{CorrelationID: pasCorr, LegType: "pas-claim", Counterpart: res.recipient})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if status, msg := g.validateFHIR(ctx, bundleJSON, "egress", targetLine); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	pendedResp, err := g.OriginateLeg(ctx, r, res.recipient, "pas-claim", res.pci, pasCorr, "",
		Content{WorkstreamType: workstreamPA, ProfileID: route.Token, Route: routeInfoFor(route), Bytes: bundleJSON})
	if err != nil {
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if status, msg := g.validateFHIR(ctx, pendedResp, "ingress", targetLine); status != 0 {
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

	// Amend: attach the provider-LOCAL operative DiagnosticReport + Provenance.
	drJSON, drOK := g.cfg.SoR.SupplementalReport(member)
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
	provJSON, err := shnsdk.BuildProvenance(drRef, "Organization/"+g.cfg.HolderID, g.cfg.Clock())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build provenance failed"})
		return
	}
	if status, msg := g.validateFHIR(ctx, provJSON, "egress", ""); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	updateCorr := g.cfg.CorrelationGen()
	// Built at the PINNED route (never re-selected: the amendment must answer
	// the pend it references, and the pended-pin rule says a resume leg never
	// re-negotiates) — route.BuildLine/route.Token are the SAME captured route
	// the initial pas-claim submit selected above.
	updateBundle, err := shnsdk.BuildConformantClaimUpdateBundleAtLine(route.BuildLine, shnsdk.ConformantClaimUpdateInputs{
		QR: res.qrJSON, SR: res.srJSON, PatientRef: res.patientRef, CoverageRef: res.coverageRef, MemberID: res.member,
		Provenance: provJSON, DiagnosticReport: drJSON, Corr: updateCorr, OriginalCorr: pasCorr, Created: g.cfg.Clock(),
		ContainedInsurer: targetsBrPayer(g.cfg.OriginationProfile),
		AbsoluteRefs:     targetsBrPayer(g.cfg.OriginationProfile),
		PayerOrgEntry:    targetsBrPayer(g.cfg.OriginationProfile), // payer Org as a resolvable PAS bundle entry (br-payer findInBundle)
		Payer:            res.payer,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build update bundle failed"})
		return
	}
	updateBundle, _, err = g.egressAdapt(route, updateBundle, ExchangeIdentity{CorrelationID: updateCorr, LegType: "pas-claim-update", Counterpart: res.recipient})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if status, msg := g.validateFHIR(ctx, updateBundle, "egress", targetLine); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	// ClaimUpdate exchange — expect APPROVED.
	updateResp, err := g.OriginateLeg(ctx, r, res.recipient, "pas-claim-update", res.pci, updateCorr, "",
		Content{WorkstreamType: workstreamPA, ProfileID: route.Token, Route: routeInfoFor(route), Bytes: updateBundle})
	if err != nil {
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if status, msg := g.validateFHIR(ctx, updateResp, "ingress", targetLine); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	// The amendment resolves to a genuine terminal A1 (the payer-gw polled
	// br-payer's timer A4→A1). UC-04 is a DiagnosticReport amendment, NOT an attestation → no
	// Attested. AmendmentCorr is the evidence the amendment leg ran (the A1 was reached
	// THROUGH the amendment, not a bare approve); AuthNumber is br-payer's real AUTH-NNNN.
	parsedUpd, approved := g.classifyResolution(updateResp)
	if !approved {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "preauthorization not approved after amendment"})
		return
	}
	if err := g.cfg.Store.StoreAuthNumber(srRef, parsedUpd.PreAuthRef); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "holder write failed (auth number)"})
		return
	}
	writeJSON(w, http.StatusOK, uc03Resp{PARequired: true, AuthNumber: parsedUpd.PreAuthRef, ValidUntil: parsedUpd.ValidUntil, AmendmentCorr: updateCorr, QRItems: res.filled, PendedItems: needed})
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
}

// handleUC05 runs the federated EXTERNAL-retrieval PA path (the non-aggregation
// showcase): CRD+DTR → PAS submit → PENDED (operative report not local) →
// consent-gated federated query to the external facility (provider→Hub→facility),
// which returns ONLY the named operative DiagnosticReport + a source Provenance
// citing the consent → ClaimUpdate with those → APPROVED. Branch "noconsent" uses
// Linda's no-consent twin: the federated query is denied and the PA stays pended.
func (g *Gateway) handleUC05(w http.ResponseWriter, r *http.Request) {
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
	member, ok := g.scenarioMember(w, r, "MBR-UC05", "MBR-PD-UC05")
	if !ok {
		return
	}
	srRef := "ServiceRequest/sr-uc05"
	if req.Branch == "noconsent" {
		member, ok = g.scenarioMember(w, r, "MBR-UC05-NOCONSENT", "MBR-PD-UC05-NC")
		if !ok {
			return
		}
		srRef = "ServiceRequest/sr-uc05-noconsent"
	}

	o := originationCodes(g.cfg.OriginationProfile).uc05
	res, ok := g.runCRDThenDTROrder(w, r, member, o.system, o.code, o.display, o.dx, false)
	if !ok {
		return
	}

	// provider-data (L1): persist the auth against the REAL seeded order ref, not the
	// sandbox literal — Bug-2 pattern from handleUC04. The noconsent branch never
	// reaches StoreAuthNumber (returns consentDenied earlier), so the re-assigned srRef
	// is moot there but harmless.
	if g.cfg.OriginationProfile == "provider-data" {
		ref, ok := resourceRef(res.srJSON)
		if !ok {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "order missing id"})
			return
		}
		srRef = ref
	}

	// PAS submit — expect PENDED (no operative DiagnosticReport yet).
	pasCorr := g.cfg.CorrelationGen()
	// Select-before-build: the routed line CHOOSES the builder, so it is
	// resolved BEFORE the bundle exists. Also the pended-line pin for any
	// pas-claim-update leg downstream — pas-claim and pas-claim-update share the
	// pa.pas contract, so one selection is contract-correct for both.
	route, ok := g.selectLegLineOrFail(w, res.recipient, "pas-claim", pasCorr)
	if !ok {
		return
	}
	targetLine := shnsdk.LineOf(route.Token)
	bundleJSON, err := shnsdk.BuildConformantClaimBundleAtLine(route.BuildLine, shnsdk.ConformantClaimInputs{
		QR: res.qrJSON, SR: res.srJSON, PatientRef: res.patientRef, CoverageRef: res.coverageRef, MemberID: res.member,
		Corr: pasCorr, Created: g.cfg.Clock(),
		ContainedInsurer: targetsBrPayer(g.cfg.OriginationProfile),
		AbsoluteRefs:     targetsBrPayer(g.cfg.OriginationProfile),
		PayerOrgEntry:    targetsBrPayer(g.cfg.OriginationProfile), // payer Org as a resolvable PAS bundle entry (br-payer findInBundle)
		Payer:            res.payer,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build bundle failed"})
		return
	}
	bundleJSON, _, err = g.egressAdapt(route, bundleJSON, ExchangeIdentity{CorrelationID: pasCorr, LegType: "pas-claim", Counterpart: res.recipient})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if status, msg := g.validateFHIR(ctx, bundleJSON, "egress", targetLine); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	pendedResp, err := g.OriginateLeg(ctx, r, res.recipient, "pas-claim", res.pci, pasCorr, "",
		Content{WorkstreamType: workstreamPA, ProfileID: route.Token, Route: routeInfoFor(route), Bytes: bundleJSON})
	if err != nil {
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if status, msg := g.validateFHIR(ctx, pendedResp, "ingress", targetLine); status != 0 {
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
		recordsJSON, err := g.OriginateLeg(ctx, r, facility.ID, "federated-query", res.pci, fqCorr, facility.ID, Content{WorkstreamType: workstreamPA, Bytes: queryJSON})
		if err != nil {
			if g.relayOriginationError(w, err) {
				return
			}
			// Distinguish a genuine consent DENIAL from an infrastructure/integrity
			// failure. ONLY an authorization denial (the no-consent branch) leaves the PA
			// validly pended; a facility outage, a tampered response, or a transport error
			// is a real 502 and must NOT be misreported to the operator as "consent denied".
			if errors.Is(err, errAuthorizationDenied) {
				writeJSON(w, http.StatusOK, uc05Resp{PARequired: true, Pended: true, ConsentDenied: true, PendedItems: needed})
				return
			}
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "federated query failed: " + err.Error()})
			return
		}
		// Ingress-validate the facility's searchset BEFORE trusting/extracting its
		// resources (defense in depth — every resource crossing the substrate is validated).
		if status, msg := g.validateFHIR(ctx, recordsJSON, "ingress", ""); status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
		// Only the DiagnosticReport leg yields adjudication evidence; the DocRef leg's records
		// were ingress-validated + audited above but are not PAS evidence (see the loop comment).
		if docType == "DiagnosticReport" {
			drJSON, provJSON, err = shnsdk.ExtractCDexEvidence(recordsJSON)
			if err != nil {
				writeJSON(w, http.StatusBadGateway, map[string]string{"error": "federated response parse failed: " + err.Error()})
				return
			}
		}
	}

	// --- ClaimUpdate with the externally-retrieved DiagnosticReport + Provenance. ---
	updateCorr := g.cfg.CorrelationGen()
	// Built at the PINNED route (never re-selected: the amendment must answer
	// the pend it references, and the pended-pin rule says a resume leg never
	// re-negotiates) — route.BuildLine/route.Token are the SAME captured route
	// the initial pas-claim submit selected above.
	updateBundle, err := shnsdk.BuildConformantClaimUpdateBundleAtLine(route.BuildLine, shnsdk.ConformantClaimUpdateInputs{
		QR: res.qrJSON, SR: res.srJSON, PatientRef: res.patientRef, CoverageRef: res.coverageRef, MemberID: res.member,
		Provenance: provJSON, DiagnosticReport: drJSON, Corr: updateCorr, OriginalCorr: pasCorr, Created: g.cfg.Clock(),
		ContainedInsurer: targetsBrPayer(g.cfg.OriginationProfile),
		AbsoluteRefs:     targetsBrPayer(g.cfg.OriginationProfile),
		PayerOrgEntry:    targetsBrPayer(g.cfg.OriginationProfile), // payer Org as a resolvable PAS bundle entry (br-payer findInBundle)
		Payer:            res.payer,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build update bundle failed"})
		return
	}
	updateBundle, _, err = g.egressAdapt(route, updateBundle, ExchangeIdentity{CorrelationID: updateCorr, LegType: "pas-claim-update", Counterpart: res.recipient})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if status, msg := g.validateFHIR(ctx, updateBundle, "egress", targetLine); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	updateResp, err := g.OriginateLeg(ctx, r, res.recipient, "pas-claim-update", res.pci, updateCorr, "",
		Content{WorkstreamType: workstreamPA, ProfileID: route.Token, Route: routeInfoFor(route), Bytes: updateBundle})
	if err != nil {
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if status, msg := g.validateFHIR(ctx, updateResp, "ingress", targetLine); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	parsedUpd, err := shnsdk.ParseClaimResponse(updateResp)
	if err != nil || parsedUpd.Outcome != "approved" {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "preauthorization not approved after federated retrieval"})
		return
	}
	if err := g.cfg.Store.StoreAuthNumber(srRef, parsedUpd.PreAuthRef); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "holder write failed (auth number)"})
		return
	}
	writeJSON(w, http.StatusOK, uc05Resp{PARequired: true, AuthNumber: parsedUpd.PreAuthRef, ValidUntil: parsedUpd.ValidUntil, QRItems: res.filled, PendedItems: needed, FacilityID: facility.ID})
}

// uc08Resp is the provider-side result of the UC-08 denial scenario.
// PARequired is always true (a PA was needed); Denied is true when the payer
// issued a denial (no auth number). Rationale is the ClaimResponse disposition.
// PatientDenialReason is the CARC reason code from the PHG denial view (the
// patient surface reads the payer's PDex PA EOB; this field surfaces it for the
// operator console demo — the PHG call stands in for the patient app).
type uc08Resp struct {
	PARequired          bool   `json:"paRequired"`
	Denied              bool   `json:"denied"`
	AuthNumber          string `json:"authNumber,omitempty"`
	Rationale           string `json:"rationale,omitempty"`
	PatientDenialReason string `json:"patientDenialReason,omitempty"`
	// PatientAppeal is the appeal-window text the PHG read FROM the EOB.processNote
	// (FR-28: data-driven from the FHIR resource, not a UI string).
	PatientAppeal string `json:"patientAppeal,omitempty"`
}

// handleUC08 runs the PA-denied path (UC-08, FR-22): CRD+DTR → PAS submit →
// denied ClaimResponse (reviewAction A3, no preAuthRef) → the provider queries
// the PHG denial view (which reads the payer's PDex PA EOB) to obtain the
// patient-rendered reason (CARC 50). The denial is TERMINAL — it does NOT pend.
func (g *Gateway) handleUC08(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	o := originationCodes(g.cfg.OriginationProfile).uc08
	// provider-data (Mode A): br-payer's J3490 CRD verdict is NOT-COVERED, so opt in to carry the
	// order past the FR-G25 stop to PAS → the formal A2 "Not Certified" ClaimResponse
	// (D-S2-2). Sandbox keeps the covered+PA→PAS-deny path (proceedOnNotCovered stays false).
	member, ok := g.scenarioMember(w, r, "MBR-UC08", "MBR-PD-UC08")
	if !ok {
		return
	}
	res, ok := g.runCRDThenDTROrder(w, r, member, o.system, o.code, o.display, o.dx, targetsBrPayer(g.cfg.OriginationProfile))
	if !ok {
		return
	}

	// PAS submit — expect DENIED (4 weeks conservative therapy < 6, no prior surgery,
	// not high-disability → Adjudicate returns Denied).
	pasCorr := g.cfg.CorrelationGen()
	// Select-before-build: the routed line CHOOSES the builder, so it is
	// resolved BEFORE the bundle exists. Also the pended-line pin for any
	// pas-claim-update leg downstream — pas-claim and pas-claim-update share the
	// pa.pas contract, so one selection is contract-correct for both.
	route, ok := g.selectLegLineOrFail(w, res.recipient, "pas-claim", pasCorr)
	if !ok {
		return
	}
	targetLine := shnsdk.LineOf(route.Token)
	bundleJSON, err := shnsdk.BuildConformantClaimBundleAtLine(route.BuildLine, shnsdk.ConformantClaimInputs{
		QR: res.qrJSON, SR: res.srJSON, PatientRef: res.patientRef, CoverageRef: res.coverageRef, MemberID: res.member,
		Corr: pasCorr, Created: g.cfg.Clock(),
		ContainedInsurer: targetsBrPayer(g.cfg.OriginationProfile),
		AbsoluteRefs:     targetsBrPayer(g.cfg.OriginationProfile),
		PayerOrgEntry:    targetsBrPayer(g.cfg.OriginationProfile), // payer Org as a resolvable PAS bundle entry (br-payer findInBundle)
		Payer:            res.payer,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build bundle failed"})
		return
	}
	bundleJSON, _, err = g.egressAdapt(route, bundleJSON, ExchangeIdentity{CorrelationID: pasCorr, LegType: "pas-claim", Counterpart: res.recipient})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if status, msg := g.validateFHIR(ctx, bundleJSON, "egress", targetLine); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	claimRespJSON, err := g.OriginateLeg(ctx, r, res.recipient, "pas-claim", res.pci, pasCorr, "",
		Content{WorkstreamType: workstreamPA, ProfileID: route.Token, Route: routeInfoFor(route), Bytes: bundleJSON})
	if err != nil {
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if status, msg := g.validateFHIR(ctx, claimRespJSON, "ingress", targetLine); status != 0 {
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
	var patientDenialReason, patientAppeal string
	if g.cfg.PHGURL != "" {
		phgURL := g.cfg.PHGURL + "/denial?pci=" + res.pci
		phgReq, err2 := http.NewRequestWithContext(ctx, http.MethodGet, phgURL, nil)
		if err2 == nil {
			phgResp, err2 := g.cfg.Client.Do(phgReq)
			if err2 == nil {
				defer phgResp.Body.Close()
				var views []struct {
					ReasonCode string `json:"reasonCode"`
					Appeal     string `json:"appeal"`
				}
				if json.NewDecoder(io.LimitReader(phgResp.Body, shnsdk.MaxResponseBytes)).Decode(&views) == nil && len(views) > 0 {
					patientDenialReason = views[0].ReasonCode
					patientAppeal = views[0].Appeal
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, uc08Resp{
		PARequired:          true,
		Denied:              true,
		AuthNumber:          "",
		Rationale:           rationale,
		PatientAppeal:       patientAppeal,
		PatientDenialReason: patientDenialReason,
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
