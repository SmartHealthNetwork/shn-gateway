// module1_originate.go — the provider-side scenario drivers (UC-01…08): originate a PA
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
// not regress the contained-payor → uniform-A3 bug. This predicate does NOT decide the
// payer-answer ingress skip — see relaysReferencePayerBytes and validateFHIRPayerIngress:
// that concern is about WHOSE bytes are being relayed, not about which lane makes the HTTP
// call. The demo lane relays the SAME reference-payer bytes (via the in-process mirror,
// internal/brpayermirror) without ever dialing br-payer itself.
func targetsBrPayer(profile string) bool { return profile == "provider-data" }

// isDemoProfile reports whether the origination profile is the family-keyed demo lane
// (§4.3): br-payer-mirrored code families (E0250/L8000/G0151/J3490 — originationCodes)
// driven off the MBR-D-UC0N persona roster, in-process against internal/brpayermirror
// rather than the live br-payer HTTP surface provider-data targets. Distinct from
// targetsBrPayer in HOW it reaches the reference payer (in-process mirror vs. a live HTTP
// dial) — but NOT distinct on WHOSE bytes come back: since the in-process payer stub retired the
// mirror relays the reference payer's OWN pinned bytes verbatim
// (internal/brpayermirror/loopback.go), so this lane is treated as provider-data is: its
// answers from a reference payer identity are not certified, and a partner payer's are — see
// relaysReferencePayerBytes and validateFHIRPayerIngress.
func isDemoProfile(profile string) bool { return profile == "demo" }

// relaysReferencePayerBytes reports whether THIS lane's payer counterparty is the
// network's reference payer, answering with the reference implementation's own bytes
// relayed verbatim. Two origination lanes reach it today: provider-data over a live
// HTTP dial (targetsBrPayer), and demo through the in-process mirror of it
// (isDemoProfile). An unset ORIGINATION_PROFILE is normalized to "demo" once, at the
// config boundary (gateway/app.go loadConfig), so no profile == "" case belongs here.
//
// The lane decides how the PAS request is shaped for that counterparty (the contained
// insurer, absolute references and payer Organization entry the reference payer
// resolves). It is only HALF of the ingress-check skip: validateFHIRPayerIngress skips
// a payer's answer only when this is true AND the payer identity the leg was routed by
// is one of the network's reference payers (isReferencePayer). A partner payer reached
// from either lane is governed at the gateway's conformance level like any other peer.
// Egress is unaffected on every lane: SHN-built bytes are always validated.
func relaysReferencePayerBytes(profile string) bool {
	return targetsBrPayer(profile) || isDemoProfile(profile)
}

// originationProfiles lists the origination lanes, one per lane predicate above:
// isDemoProfile and targetsBrPayer. A value is a lane only if one of them names it
// (isOriginationProfile), so this list cannot admit a value no lane recognises.
var originationProfiles = []string{"demo", "provider-data"}

// isOriginationProfile reports whether profile names an origination lane.
func isOriginationProfile(profile string) bool {
	return isDemoProfile(profile) || targetsBrPayer(profile)
}

// OriginationProfiles returns the values Config.OriginationProfile accepts (the
// origination lanes), in a fixed order. It is a copy: changing it changes nothing here.
func OriginationProfiles() []string {
	return append([]string(nil), originationProfiles...)
}

// checkOriginationProfile is New's check of Config.OriginationProfile. A value that
// names no lane is refused on every role: the origination paths compare it exactly, so
// it would match none of them and build a PAS request carrying a payer Organization no
// system of record supplied. Unset is not refused here: a provider with no profile
// still relays, and its originating routes refuse instead (requireOriginationProfile).
// The published binary reads an unset ORIGINATION_PROFILE as demo on the provider role
// before it builds a Config.
func checkOriginationProfile(profile string) error {
	if profile != "" && !isOriginationProfile(profile) {
		return fmt.Errorf("gateway: Config.OriginationProfile %q is not an origination profile (must be %s)", profile, strings.Join(originationProfiles, "|"))
	}
	return nil
}

// referencePayerSystem is the payer-identifier namespace the reference payers claim
// their identities under.
const referencePayerSystem = "urn:oid:2.16.840.1.113883.6.300"

// referencePayerIdentities is the network's published, closed set of reference payer
// identities: 00001, the Da Vinci reference payer (shnsdk.CMSPayerIdentity); 00300,
// the hosted conformance payer in front of it; and 00301, the hosted 2.2 reference
// payer. A leg is routed by the payer identity the member's own Coverage names, so the
// identity — not the holder that happens to answer for it — says whose answer it is.
// Their DTR and PAS answers are the reference implementation's own bytes, which do not
// yet conform (the 2.0 reference payer's DTR package fails DTR 2.0.1); a provider
// gateway on a lane that relays them does not certify them (validateFHIRPayerIngress).
// BridgeDemoPayerID and BridgeRefusePayerID, the two SHN-operated bridging-demo payers,
// are in the set only because they relay the reference payer's content: each fronts
// 00001 as its backend, so their answers are the same reference bytes under the demo
// payer's own identity. They leave the set with the rest of it once the reference
// content conforms.
// The set changes only by a release of this rule, never by configuration.
var referencePayerIdentities = map[shnsdk.PayerIdentifier]bool{
	{System: referencePayerSystem, Value: "00001"}: true,
	{System: referencePayerSystem, Value: "00300"}: true,
	{System: referencePayerSystem, Value: "00301"}: true,
	BridgeDemoPayerID:   true,
	BridgeRefusePayerID: true,
}

// isReferencePayer reports whether payer is one of the network's reference payer
// identities (referencePayerIdentities).
func isReferencePayer(payer shnsdk.PayerIdentifier) bool { return referencePayerIdentities[payer] }

// ReferencePayerIdentities returns the network's reference payer identities, sorted by
// system then value — the identities whose answers a provider gateway relaying
// reference-payer bytes does not certify. It is a copy: changing it changes nothing here.
func ReferencePayerIdentities() []shnsdk.PayerIdentifier {
	out := make([]shnsdk.PayerIdentifier, 0, len(referencePayerIdentities))
	for p := range referencePayerIdentities {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].System != out[j].System {
			return out[i].System < out[j].System
		}
		return out[i].Value < out[j].Value
	})
	return out
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
// Want matcher, so the only way a canary/cloudsmoke row can pin WHERE
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
func (g *Gateway) selectLegLineOrBridgeRefuse(w http.ResponseWriter, recipient, legType, corrID string, builder relay.BuilderID) (legRoute, bool) {
	route, err := g.selectLegLine(recipient, legType, corrID, builder)
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

// FilledItem is the gateway-engine-LOCAL attribution surface for a DTR auto-filled
// QR item (the scenario route's answer, QRItems field). The SDK's FillQuestionnaire drops the
// FilledItem summary (UI-only); the gateway reconstructs it via fillSummary
// from the ClinicalContext. JSON tags are byte-for-byte compatible with the original
// dtr.FilledItem shape so the scenario response format is unchanged.
type FilledItem struct {
	LinkID    string `json:"linkId"`
	Answer    string `json:"answer"`
	Origin    string `json:"origin"`
	SourceRef string `json:"sourceRef,omitempty"`
}

// fillSummary reconstructs the []FilledItem summary from the ClinicalContext —
// the same leaves shnsdk.FillQuestionnaire answers, in the same order (the fill
// carries no per-item summary, so the scenario answer's summary is rebuilt here). Items with
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
	r = ensureOriginationClaimInput(r)
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
		memberID, ok = g.scenarioMember(w, r, "uc01")
	case "notcovered":
		memberID, ok = g.scenarioMember(w, r, "uc01-notcovered")
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
	// (the eligibility leg's recipient is resolved from realCov), and the request's insurer names
	// the payer that Coverage names. It fails closed when the member has no coverage / no
	// parseable payer on file (AI-G11 / OWD-G10).
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
	// before any leg (AI-G11 / OWD-G10). The same payer identity is the request's insurer.
	recipient, payerIdentity, status, msg := g.recipientForSoR(ctx, realCov)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	cerJSON, err := g.eligibilityRequest(memberID, payerIdentity)
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
	// engine builds (shnsdk.BuildEligibilityRequestExact), never a relay of anyone else's bytes,
	// on every origination lane. Only the ingress/egress legs a LegResponder (the payer
	// content occupant — CRD/DTR/PAS) actually touches can carry reference-payer bytes;
	// eligibility never reaches that occupant's Handle (R11 — see gateway/engine/workstream_pa_forward.go's "NO
	// coverage-eligibility case" comment).
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
	sent, failed := g.sendLeg(ctx, r, legSend{
		Workstream: workstreamPA, Leg: "coverage-eligibility", Recipient: recipient, PCI: pci, Corr: correlationID,
		Sealed: sealRequest(relay.BuilderSDKEligibility, cerJSON, "application/fhir+json"),
	})
	if failed != nil {
		g.writeLegFailure(w, failed)
		return
	}
	crrJSON := sent.Answer

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
	// PASRoute is the bridge exhibit's PAS leg route (handleUC03Bridge only):
	// the line it was sent at, the line it was built at, and the chain that
	// carried it across, if any. A token at the build line to a payer that
	// declares another line is a carry.
	PASRoute *RouteInfo `json:"pasRoute,omitempty"`

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
	// sorID is the id the participant's own system holds the member's Patient
	// under, from the same one reading: the supplemental report's subject is
	// checked against it (E-06), never against a second reading.
	sorID  string
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
	realCov, found, status, msg := g.memberRoutingCoverage(ctx, member)
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
	// The request states its claim facts for the order the member holds, and
	// every fact the PAS build at the routed line requires (the amendment's
	// too, on a route that sends one), or it is refused here, before any leg,
	// exactly as the build would refuse it (pasOriginationBeforeAnyLeg).
	if !g.pasOriginationBeforeAnyLeg(w, r, recipient, srJSON) {
		return crdDtrResult{}, false
	}
	// The order retains the identity and the bytes the participant's own system
	// supplied. Nothing gives it a local one here any more: an id this gateway
	// assigned would name a record the system does not hold under that name, and
	// the inquiry that continues a pended authorization re-reads the order by the
	// reference it was submitted under.
	//
	// The CRD request carries the participant's own Patient and Coverage search
	// result (module1_originate_crd.go); an order read from the system of record names the
	// patient the same way. The Coverage is read twice — above for routing
	// (memberRoutingCoverage), here as the search result the request carries — so each
	// read keeps its own refusal rules; both choose the same coverage (routingCoverageChoice).
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
	crdRoute, ok := g.selectLegLineOrFail(w, recipient, "crd-order-select", crdCorr, relay.BuilderSDKCRDRequest)
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
	sent, failed := g.sendLeg(ctx, r, legSend{
		Workstream: workstreamPA, Leg: "crd-order-select", Recipient: recipient, PCI: pci, Corr: crdCorr, Route: &crdRoute,
		Build: &moduleBuild{
			Builder: relay.BuilderSDKCRDRequest, ContentType: "application/json", Body: crdReq,
		},
	})
	if failed != nil {
		g.writeLegFailure(w, failed)
		return crdDtrResult{}, false
	}
	crdRespJSON := sent.Answer
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
			return crdDtrResult{srJSON: srJSON, patientRef: patientRef, coverageRef: coverageRef, coverage: realCov, insurer: realPayerOrg, member: member, memberSystem: recs.memberSystem, sorID: recs.sorID, pci: pci, payer: payer, recipient: recipient}, true
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
	res := crdDtrResult{srJSON: srJSON, patientRef: patientRef, coverageRef: coverageRef, coverage: realCov, insurer: realPayerOrg, member: member, memberSystem: recs.memberSystem, sorID: recs.sorID, pci: pci, payer: payer, recipient: recipient,
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
		route, ok := g.selectLegLineOrFail(w, recipient, "dtr-questionnaire-fetch", dtrCorr, relay.BuilderSDKDTRPackage)
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
		sendRoute, carryStatus, carryMsg := g.carryUnchanged(route, dtrReq, dtrCorr, recipient)
		if carryStatus != 0 {
			writeJSON(w, carryStatus, map[string]string{"error": carryMsg})
			return crdDtrResult{}, false
		}
		route = sendRoute
		sent, failed := g.sendLeg(ctx, r, legSend{
			Workstream: workstreamPA, Leg: "dtr-questionnaire-fetch", Recipient: recipient, PCI: pci, Corr: dtrCorr, Route: &route,
			Sealed: dtrPayload, Operation: shnsdk.FrameOperationQuestionnairePackage,
		})
		if failed != nil {
			g.writeLegFailure(w, failed)
			return crdDtrResult{}, false
		}
		packageJSON := sent.Answer
		ctx = withFindingContext(ctx, findingContext{
			LegType: "dtr-questionnaire-fetch", CorrelationID: dtrCorr, Seam: "originate", Whose: "peer",
		})
		if status, msg := g.validateFHIRPayerIngress(ctx, packageJSON, res.dtrLine, "pa.dtr", res.payer); status != 0 {
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
	member, ok := g.scenarioMember(w, r, "uc02")
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
	member, ok := g.scenarioMember(w, r, "uc02-payerb")
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
	member, ok := g.scenarioMember(w, r, "uc02-unknownpayer")
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
	realCov, hasCov, status, msg := g.memberRoutingCoverage(ctx, member)
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
	// result (module1_originate_crd.go); an order read from the system of record names the
	// patient the same way. The Coverage is read twice — above for routing
	// (memberRoutingCoverage), here as the search result the request carries — so each
	// read keeps its own refusal rules; both choose the same coverage (routingCoverageChoice).
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
	crdRoute, ok := g.selectLegLineOrFail(w, recipient, "crd-order-select", correlationID, relay.BuilderSDKCRDRequest)
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
	sent, failed := g.sendLeg(ctx, r, legSend{
		Workstream: workstreamPA, Leg: "crd-order-select", Recipient: recipient, PCI: pci, Corr: correlationID, Route: &crdRoute,
		Build: &moduleBuild{
			Builder: relay.BuilderSDKCRDRequest, ContentType: "application/json", Body: reqJSON,
		},
	})
	if failed != nil {
		g.writeLegFailure(w, failed)
		return
	}
	respJSON := sent.Answer

	cov, err := crdCoverage(respJSON)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "card parse failed"})
		return
	}

	// The coverage view carries no card Summary; do a small inline parse of the
	// raw cards JSON to extract the first card's summary for the scenario response.
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
// (§4.3/register §11 ruling (b)); "bridge-demo" and "line-carry" are the
// kit-bridging-visualization demo's two arms (task2 brief A3a), sharing one body
// (handleUC03Bridge) — their OWN literal L8000/order-select exhibit, decoupled from this
// arm. "bridge-demo" drives MBR-BRIDGE-DEMO for the full bridged SUCCESS (CRD/DTR cross
// the version boundary, through the transform chains when the gateway translates, PAS
// lands on the shared line, a real payer-issued authNumber); "line-carry" drives
// MBR-BRIDGE-REFUSE, whose payer declares only a PAS line this build has no certified
// translation to: the submission is carried at its build line and the carry recorded, and
// the payer answers as its own system does. Only the participant's REQUIRE_SHARED_LINE
// refuses it, as routing — see selectLegLineOrBridgeRefuse for how that refusal is
// surfaced; any other value 400s.
func (g *Gateway) handleUC03(w http.ResponseWriter, r *http.Request) {
	if g.cfg.OriginationProfile == "provider-data" {
		// UC-03 off provider data = the HomeOxygenDispatch path (the only br-payer family that PA-requires
		// + launches a DTR + GENUINELY auto-fills it via operated $populate). E1390 vs HomeOxygen's E0431;
		// the approval is br-payer's pend-resolution timer (D-2RI-3), and the genuine auto-fill (the $populate
		// runs the real prepop CQL against the seeded O2 obs) is UC-03's distinctive. The implicit fail-closed
		// at OpenOrder is preserved (a mis-seeded MBR-PD-UC03 with no E1390 order fails closed there).
		member, ok := g.scenarioMember(w, r, "uc03")
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
	r = ensureOriginationClaimInput(r)
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
		g.handleUC03Bridge(w, r, "uc03-bridge-demo")
	case "line-carry":
		g.handleUC03Bridge(w, r, "uc03-line-carry")
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown branch"})
	}
}

// handleUC03Oxygen is UC-03's re-keyed non-provider-data arm (register §11 ruling (b)):
// the oxygen family (E1390) via order-DISPATCH — the CRD hook shape genuinely proven live
// against the reference payer for this family (module1_originate_homeoxygen.go's DIVERGENCE 2/3),
// not order-select's PA-required assertion, which no live pin covers for this family. Rides
// runCRDDispatch (shared with originateDispatch) so the CRD/DTR/populate plumbing is ONE
// implementation. UC-03's distinctive over originateDispatch's own callers: it must
// ANSWER the tree's one required item (6.1) before submitting — buildOxygenNecessityItem
// merges that answer into the populated QR without disturbing the genuine 2.2/2.3 auto
// answers — and it surfaces the hermetic FR-17 source=auto attribution
// (homeOxygenAutoFillEvidence) that register §9 row 4 asked for.
func (g *Gateway) handleUC03Oxygen(w http.ResponseWriter, r *http.Request) {
	member, ok := g.scenarioMember(w, r, "uc03")
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
// decoupled from originationCodes().uc03 (R3): the line-carry arm demonstrates a
// pair with no certified translation being carried, which does not depend on the
// family/code carried; the bridge-demo arm reuses the SAME literal (task2
// brief) since both arms share one body. Kept literally UNCHANGED (L8000, order-select)
// rather than following UC-03's oxygen re-key, so this demo-only exhibit stays
// byte-identical to before R3.
var uc03BridgeCode = orderTuple{systemHCPCSBuild, "L8000", DemoDisplayL8000, DemoDxL8000}

// handleUC03Bridge is the kit-bridging-visualization demo's shared body for both
// "bridge-demo" (MBR-BRIDGE-DEMO, engine.BridgeDemoPayerID — the full bridged SUCCESS:
// CRD/DTR cross the version boundary via the transform chains, PAS lands on the shared
// line, approval with a payer-issued authNumber) and "line-carry" (MBR-BRIDGE-REFUSE,
// engine.BridgeRefusePayerID — the peer declares only a PAS line no certified
// translation reaches, so the submission is carried at its build line and the carry
// recorded; under REQUIRE_SHARED_LINE, selectLegLineOrBridgeRefuse's pas-claim
// contract-version line-select refuses it as routing BEFORE any bundle is built).
// Both arms answer with pasRoute, the PAS leg's route. The two arms
// differ ONLY in which member/persona they drive — which coverage-derived routing
// (FeedPayerRouter) sends to which deployed bridge-peer holder — everything else,
// including the CRD+DTR order-select flow and the PAS submit attempt, is byte-identical.
// UNCHANGED by R3 (it is not the scenario register §11 ruled on — see handleUC03's doc
// comment): this is the kit-bridging-visualization demo's OWN literal L8000/order-select
// exhibit, decoupled from handleUC03Oxygen's oxygen re-key.
func (g *Gateway) handleUC03Bridge(w http.ResponseWriter, r *http.Request, scenario string) {
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
	m, ok := g.scenarioMember(w, r, scenario)
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

	// --- PAS round-trip: submit the preauth bundle and report the payer's decision.
	// When the peer's PAS declaration shares no line with this build's and nothing
	// reaches it (line-carry), the submission is carried at its build line; only
	// under REQUIRE_SHARED_LINE does selectLegLineOrBridgeRefuse write the demo
	// lane's structured 200 refusal itself, and we return here without ever
	// building a bundle. ---
	pasCorr := g.cfg.CorrelationGen()
	ctx = withFindingContext(ctx, findingContext{
		LegType: "pas-claim", CorrelationID: pasCorr, Seam: "originate", Whose: "own",
	})
	// Select-before-build: the routed line CHOOSES the builder, so it is
	// resolved BEFORE the bundle exists. Also the pended-line pin for any
	// pas-claim-update leg downstream — pas-claim and pas-claim-update share the
	// pa.pas contract, so one selection is contract-correct for both.
	route, ok := g.selectLegLineOrBridgeRefuse(w, res.recipient, "pas-claim", pasCorr, relay.BuilderSDKPASSubmit)
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
	// The request's own facts, exactly as the request to this route states
	// them; the builder refuses a required one it does not state.
	claimIn, inOK := claimInputForOrder(w, r, res.srJSON)
	if !inOK {
		return
	}
	claimFacts := claimIn.facts()
	bundleJSON, err := buildAuthoredPASSubmit(route.BuildLine, shnsdk.ConformantClaimInputs{
		QR: pasQR, SR: res.srJSON, Provider: pasProviderJSON, Coverage: res.coverage, Insurer: res.insurer, PatientRef: res.patientRef, CoverageRef: res.coverageRef, MemberID: res.member, MemberIDSystem: pasMemberSystem,
		Corr: pasCorr, Created: g.cfg.Clock(),
		ContainedInsurer: relaysReferencePayerBytes(g.cfg.OriginationProfile),
		AbsoluteRefs:     relaysReferencePayerBytes(g.cfg.OriginationProfile),
		PayerOrgEntry:    relaysReferencePayerBytes(g.cfg.OriginationProfile), // payer Org as a resolvable PAS bundle entry (br-payer findInBundle)
		Payer:            res.payer,
		PASClaimFacts:    claimFacts,
	})
	if err != nil {
		pasBuildFailure(w, err, "build bundle failed: "+err.Error())
		return
	}
	// An APPLY-time refusal (a *SemanticChangeError) is reshaped like the selection-time
	// *RouteRefusalError (bridgeRefusalText); module 2 no longer plans the chain whose
	// gated up-step used to refuse here (gatedUpward), so the line-carry arm is carried
	// instead. Every other walk error (a genuine fault) still falls through to
	// the ordinary 502 below unchanged.
	bundleJSON, err = g.completeAuthoredPASRequest(ctx, bundleJSON, pasQR, res.srJSON, res.coverageRef, relaysReferencePayerBytes(g.cfg.OriginationProfile))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "PAS evidence linkage failed"})
		return
	}
	sent, failed := g.sendLeg(ctx, r, legSend{
		Workstream: workstreamPA, Leg: "pas-claim", Recipient: res.recipient, PCI: res.pci, Corr: pasCorr, Route: &route,
		Build: &moduleBuild{
			Builder: relay.BuilderSDKPASSubmit, ContentType: "application/fhir+json", Body: bundleJSON, Contract: "pa.pas",
			Check: func(line string, body []byte) (int, string) {
				return g.validatePASAttachments(ctx, body, line, res.qrSource != nil)
			},
		},
	})
	if failed != nil {
		// The bridging demo lane answers a walk the chain refused with its
		// structured refusal.
		if text, ok := bridgeRefusalText(failed.Err); ok && !failed.FromLeg {
			writeBridgeRefusal(w, "pas-claim", text)
			return
		}
		g.writeLegFailure(w, failed)
		return
	}
	bundleJSON = sent.Bytes
	claimRespJSON := sent.Answer
	// What follows reads the route the claim was sent on.
	route, targetLine = sent.Route, shnsdk.LineOf(sent.Route.Token)
	ctx = withFindingContext(ctx, findingContext{
		LegType: "pas-claim", CorrelationID: pasCorr, Seam: "originate", Whose: "peer",
	})
	if status, msg := g.validateFHIRPayerIngress(ctx, claimRespJSON, targetLine, "pa.pas", res.payer); status != 0 {
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
		PASRoute:   routeInfoFor(route),
	}))
}

// handleUC07HCPCS runs the HCPCS (L8000) DV-approve path — the in-process mirror of the
// two-RI L8000 approve: CRD (PA-required) → DTR auto-fill → PAS approve on first
// submit → the payer records the decision on its pend ledger. NOT
// patient-authorship.
func (g *Gateway) handleUC07HCPCS(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(withFindingContext(r.Context(), findingContext{
		LegType: "pas-claim", Seam: "originate", Whose: "own",
	}))
	ctx := r.Context()

	member, ok := g.scenarioMember(w, r, "uc07hcpcs")
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
	route, ok := g.selectLegLineOrFail(w, res.recipient, "pas-claim", pasCorr, relay.BuilderSDKPASSubmit)
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
	// The request's own facts, exactly as the request to this route states
	// them; the builder refuses a required one it does not state.
	claimIn, inOK := claimInputForOrder(w, r, res.srJSON)
	if !inOK {
		return
	}
	claimFacts := claimIn.facts()
	bundleJSON, err := buildAuthoredPASSubmit(route.BuildLine, shnsdk.ConformantClaimInputs{
		QR: pasQR, SR: res.srJSON, Provider: pasProviderJSON, Coverage: res.coverage, Insurer: res.insurer, PatientRef: res.patientRef, CoverageRef: res.coverageRef, MemberID: res.member, MemberIDSystem: pasMemberSystem,
		Corr: pasCorr, Created: g.cfg.Clock(),
		ContainedInsurer: relaysReferencePayerBytes(g.cfg.OriginationProfile),
		AbsoluteRefs:     relaysReferencePayerBytes(g.cfg.OriginationProfile),
		PayerOrgEntry:    relaysReferencePayerBytes(g.cfg.OriginationProfile), // payer Org as a resolvable PAS bundle entry (br-payer findInBundle)
		Payer:            res.payer,
		PASClaimFacts:    claimFacts,
	})
	if err != nil {
		pasBuildFailure(w, err, "build bundle failed: "+err.Error())
		return
	}
	bundleJSON, err = g.completeAuthoredPASRequest(ctx, bundleJSON, pasQR, res.srJSON, res.coverageRef, relaysReferencePayerBytes(g.cfg.OriginationProfile))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "PAS evidence linkage failed"})
		return
	}
	sent, failed := g.sendLeg(ctx, r, legSend{
		Workstream: workstreamPA, Leg: "pas-claim", Recipient: res.recipient, PCI: res.pci, Corr: pasCorr, Route: &route,
		Build: &moduleBuild{
			Builder: relay.BuilderSDKPASSubmit, ContentType: "application/fhir+json", Body: bundleJSON, Contract: "pa.pas",
			Check: func(line string, body []byte) (int, string) {
				return g.validatePASAttachments(ctx, body, line, res.qrSource != nil)
			},
		},
	})
	if failed != nil {
		g.writeLegFailure(w, failed)
		return
	}
	bundleJSON = sent.Bytes
	claimRespJSON := sent.Answer
	// What follows reads the route the claim was sent on.
	route, targetLine = sent.Route, shnsdk.LineOf(sent.Route.Token)
	ctx = withFindingContext(ctx, findingContext{
		LegType: "pas-claim", CorrelationID: pasCorr, Seam: "originate", Whose: "peer",
	})
	if status, msg := g.validateFHIRPayerIngress(ctx, claimRespJSON, targetLine, "pa.pas", res.payer); status != 0 {
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
	member, ok := g.scenarioMember(w, r, "uc04")
	if !ok {
		return
	}
	if g.cfg.OriginationProfile != "provider-data" {
		// The demo lane's in-flow amendment follows the submission in this
		// request (provider-data's single-shot tail sends none).
		r = routeSendsAmendment(r)
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
		// before the fill (module1_dtr_adaptive.go) — a fill over the package's group-1 tree alone
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
	route, ok := g.selectLegLineOrFail(w, res.recipient, "pas-claim", pasCorr, relay.BuilderSDKPASSubmit)
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
	// The request's own facts, exactly as the request to this route states
	// them; the builder refuses a required one it does not state.
	claimIn, inOK := claimInputForOrder(w, r, res.srJSON)
	if !inOK {
		return
	}
	claimFacts := claimIn.facts()
	bundleJSON, err := buildAuthoredPASSubmit(route.BuildLine, shnsdk.ConformantClaimInputs{
		QR: pasQR, SR: res.srJSON, Provider: pasProviderJSON, Coverage: res.coverage, Insurer: res.insurer, PatientRef: res.patientRef, CoverageRef: res.coverageRef, MemberID: res.member, MemberIDSystem: pasMemberSystem,
		Corr: pasCorr, Created: g.cfg.Clock(),
		ContainedInsurer: relaysReferencePayerBytes(g.cfg.OriginationProfile),
		AbsoluteRefs:     relaysReferencePayerBytes(g.cfg.OriginationProfile),
		PayerOrgEntry:    relaysReferencePayerBytes(g.cfg.OriginationProfile), // payer Org as a resolvable PAS bundle entry (br-payer findInBundle)
		Payer:            res.payer,
		PASClaimFacts:    claimFacts,
	})
	if err != nil {
		pasBuildFailure(w, err, "build bundle failed: "+err.Error())
		return
	}
	// The submission an amendment of this request restates, recorded off the
	// bytes it built.
	submitted, err := recordPASSubmission(bundleJSON)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	bundleJSON, err = g.completeAuthoredPASRequest(ctx, bundleJSON, pasQR, res.srJSON, res.coverageRef, relaysReferencePayerBytes(g.cfg.OriginationProfile))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "PAS evidence linkage failed"})
		return
	}
	sent, failed := g.sendLeg(ctx, r, legSend{
		Workstream: workstreamPA, Leg: "pas-claim", Recipient: res.recipient, PCI: res.pci, Corr: pasCorr, Route: &route,
		Build: &moduleBuild{
			Builder: relay.BuilderSDKPASSubmit, ContentType: "application/fhir+json", Body: bundleJSON, Contract: "pa.pas",
			Check: func(line string, body []byte) (int, string) {
				return g.validatePASAttachments(ctx, body, line, res.qrSource != nil)
			},
		},
	})
	if failed != nil {
		g.writeLegFailure(w, failed)
		return
	}
	bundleJSON = sent.Bytes
	pendedResp := sent.Answer
	// What follows reads the route the claim was sent on.
	route, targetLine = sent.Route, shnsdk.LineOf(sent.Route.Token)
	ctx = withFindingContext(ctx, findingContext{
		LegType: "pas-claim", CorrelationID: pasCorr, Seam: "originate", Whose: "peer",
	})
	if status, msg := g.validateFHIRPayerIngress(ctx, pendedResp, targetLine, "pa.pas", res.payer); status != 0 {
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
	submitted.pend = pendedResp
	// Map []NeededItem → []string using .Code (the Task.input valueString, matching
	// what the internal ParsePendedOrApproved returned as a plain []string).
	needed := neededItemCodes(neededItems)

	// Back to this participant's own bytes for the amendment build (pas-claim-update):
	// the pended-response check above is the only peer-answer read in this stretch.
	ctx = withFindingContext(ctx, findingContext{
		LegType: "pas-claim-update", Seam: "originate", Whose: "own",
	})

	// Amend: attach the provider-LOCAL operative DiagnosticReport + Provenance,
	// its subject naming the member's network patient (E-06).
	drJSON, status, msg := g.supplementalReport(ctx, member, res.sorID)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
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
	// Built at the PINNED route (never re-selected: the amendment must answer
	// the pend it references, and the pended-pin rule says a resume leg never
	// re-negotiates) — route.BuildLine/route.Token are the SAME captured route
	// the initial pas-claim submit selected above.
	// The route the amendment was sent on, which what follows reads.
	sentRoute := route
	updateCorr, updateBundle, updateResp, handled, err := g.sendAmendment(res.recipient, func(updateCorr string) ([]byte, []byte, bool, error) {
		ctx := withFindingContext(ctx, findingContext{
			LegType: "pas-claim-update", CorrelationID: updateCorr, Seam: "originate", Whose: "own",
		})
		updateBundle, err := buildAuthoredPASUpdate(route.BuildLine, shnsdk.ConformantClaimUpdateInputs{
			QR: pasQR, SR: res.srJSON, Provider: pasProviderJSON, Coverage: res.coverage, Insurer: res.insurer, PatientRef: res.patientRef, CoverageRef: res.coverageRef, MemberID: res.member, MemberIDSystem: pasMemberSystem,
			Provenance: provJSON, DiagnosticReport: drJSON, Corr: updateCorr, OriginalCorr: pasCorr, Created: g.cfg.Clock(),
			ContainedInsurer: relaysReferencePayerBytes(g.cfg.OriginationProfile),
			AbsoluteRefs:     relaysReferencePayerBytes(g.cfg.OriginationProfile),
			PayerOrgEntry:    relaysReferencePayerBytes(g.cfg.OriginationProfile), // payer Org as a resolvable PAS bundle entry (br-payer findInBundle)
			Payer:            res.payer,
			// The amendment states the facts and the certificationType the
			// request to this route states (the builder refuses a required one
			// it does not state); the prior Claim it restates is the recorded
			// submission's, read off the bytes the submission built — never a
			// constant; the infoChanged marker is derived from it by the
			// builder (an item sent before exactly as now carries none).
			PASClaimFacts:     claimFacts,
			CertificationType: claimIn.CertificationType,
			PriorClaim:        submitted.prior,
			// The payer's own pend: the attached report is linked to the
			// items it asks the report for, and to no other.
			Pend: submitted.pendFor(targetLine),
		})
		if err != nil {
			pasBuildFailure(w, err, "build update bundle failed")
			return nil, nil, true, nil
		}
		updateBundle, err = g.completeAuthoredPASRequest(ctx, updateBundle, pasQR, res.srJSON, res.coverageRef, relaysReferencePayerBytes(g.cfg.OriginationProfile))
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "PAS evidence linkage failed"})
			return nil, nil, true, nil
		}
		sent, failed := g.sendLeg(ctx, r, legSend{
			Workstream: workstreamPA, Leg: "pas-claim-update", Recipient: res.recipient, PCI: res.pci, Corr: updateCorr, Route: &route,
			Build: &moduleBuild{
				Builder: relay.BuilderSDKPASUpdate, ContentType: "application/fhir+json", Body: updateBundle, Contract: "pa.pas",
				Check: func(line string, body []byte) (int, string) {
					return g.validatePASAttachments(ctx, body, line, res.qrSource != nil)
				},
			},
		})
		if failed != nil && !failed.FromLeg {
			writeJSON(w, failed.Status, map[string]string{"error": failed.Msg})
			return nil, nil, true, nil
		}
		updateBundle = sent.Bytes

		// ClaimUpdate exchange — expect APPROVED.
		if failed != nil {
			return updateBundle, nil, false, failed.Err
		}
		sentRoute = sent.Route
		return updateBundle, sent.Answer, false, nil
	})
	if handled {
		return
	}
	if err != nil {
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	route, targetLine = sentRoute, shnsdk.LineOf(sentRoute.Token)
	ctx = withFindingContext(ctx, findingContext{
		LegType: "pas-claim-update", CorrelationID: updateCorr, Seam: "originate", Whose: "peer",
	})
	if status, msg := g.validateFHIRPayerIngress(ctx, updateResp, targetLine, "pa.pas", res.payer); status != 0 {
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
	r = ensureOriginationClaimInput(r)
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
	// The branch selects the scenario persona first, so the member the request
	// names is checked against that branch's member, not the default one.
	scenario := "uc05"
	if req.Branch == "noconsent" {
		scenario = "uc05-noconsent"
	}
	member, ok := g.scenarioMember(w, r, scenario)
	if !ok {
		return
	}

	if req.Branch != "noconsent" {
		// The consented branch amends with the federated evidence in this
		// request; the noconsent branch stops before any amendment.
		r = routeSendsAmendment(r)
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
	route, ok := g.selectLegLineOrFail(w, res.recipient, "pas-claim", pasCorr, relay.BuilderSDKPASSubmit)
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
	// The request's own facts, exactly as the request to this route states
	// them; the builder refuses a required one it does not state.
	claimIn, inOK := claimInputForOrder(w, r, res.srJSON)
	if !inOK {
		return
	}
	claimFacts := claimIn.facts()
	bundleJSON, err := buildAuthoredPASSubmit(route.BuildLine, shnsdk.ConformantClaimInputs{
		QR: pasQR, SR: res.srJSON, Provider: pasProviderJSON, Coverage: res.coverage, Insurer: res.insurer, PatientRef: res.patientRef, CoverageRef: res.coverageRef, MemberID: res.member, MemberIDSystem: pasMemberSystem,
		Corr: pasCorr, Created: g.cfg.Clock(),
		ContainedInsurer: relaysReferencePayerBytes(g.cfg.OriginationProfile),
		AbsoluteRefs:     relaysReferencePayerBytes(g.cfg.OriginationProfile),
		PayerOrgEntry:    relaysReferencePayerBytes(g.cfg.OriginationProfile), // payer Org as a resolvable PAS bundle entry (br-payer findInBundle)
		Payer:            res.payer,
		PASClaimFacts:    claimFacts,
	})
	if err != nil {
		pasBuildFailure(w, err, "build bundle failed: "+err.Error())
		return
	}
	// The submission an amendment of this request restates, recorded off the
	// bytes it built.
	submitted, err := recordPASSubmission(bundleJSON)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	bundleJSON, err = g.completeAuthoredPASRequest(ctx, bundleJSON, pasQR, res.srJSON, res.coverageRef, relaysReferencePayerBytes(g.cfg.OriginationProfile))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "PAS evidence linkage failed"})
		return
	}
	sent, failed := g.sendLeg(ctx, r, legSend{
		Workstream: workstreamPA, Leg: "pas-claim", Recipient: res.recipient, PCI: res.pci, Corr: pasCorr, Route: &route,
		Build: &moduleBuild{
			Builder: relay.BuilderSDKPASSubmit, ContentType: "application/fhir+json", Body: bundleJSON, Contract: "pa.pas",
			Check: func(line string, body []byte) (int, string) {
				return g.validatePASAttachments(ctx, body, line, res.qrSource != nil)
			},
		},
	})
	if failed != nil {
		g.writeLegFailure(w, failed)
		return
	}
	bundleJSON = sent.Bytes
	pendedResp := sent.Answer
	// What follows reads the route the claim was sent on.
	route, targetLine = sent.Route, shnsdk.LineOf(sent.Route.Token)
	ctx = withFindingContext(ctx, findingContext{
		LegType: "pas-claim", CorrelationID: pasCorr, Seam: "originate", Whose: "peer",
	})
	if status, msg := g.validateFHIRPayerIngress(ctx, pendedResp, targetLine, "pa.pas", res.payer); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	pended, neededItems, err := shnsdk.ParsePendedResponse(pendedResp)
	if err != nil || !pended {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "expected pended response"})
		return
	}
	submitted.pend = pendedResp
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
		sent, failed := g.sendLeg(ctx, r, legSend{
			Workstream: workstreamFQS, Leg: "federated-query", Recipient: facility.ID, PCI: res.pci, Corr: fqCorr, Custodian: facility.ID,
			Sealed: sealRequest(relay.BuilderSDKFederatedQuery, queryJSON, "application/fhir+json"),
		})
		recordsJSON := sent.Answer
		if failed != nil {
			err := failed.Err
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
		// a payer, so this leg's bytes are never eligible for the reference-payer skip
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
	// Built at the PINNED route (never re-selected: the amendment must answer
	// the pend it references, and the pended-pin rule says a resume leg never
	// re-negotiates) — route.BuildLine/route.Token are the SAME captured route
	// the initial pas-claim submit selected above.
	// The route the amendment was sent on, which what follows reads.
	sentRoute := route
	updateCorr, updateBundle, updateResp, handled, err := g.sendAmendment(res.recipient, func(updateCorr string) ([]byte, []byte, bool, error) {
		ctx := withFindingContext(ctx, findingContext{
			LegType: "pas-claim-update", CorrelationID: updateCorr, Seam: "originate", Whose: "own",
		})
		updateBundle, err := buildAuthoredPASUpdate(route.BuildLine, shnsdk.ConformantClaimUpdateInputs{
			QR: pasQR, SR: res.srJSON, Provider: pasProviderJSON, Coverage: res.coverage, Insurer: res.insurer, PatientRef: res.patientRef, CoverageRef: res.coverageRef, MemberID: res.member, MemberIDSystem: pasMemberSystem,
			Provenance: provJSON, DiagnosticReport: drJSON, Corr: updateCorr, OriginalCorr: pasCorr, Created: g.cfg.Clock(),
			ContainedInsurer: relaysReferencePayerBytes(g.cfg.OriginationProfile),
			AbsoluteRefs:     relaysReferencePayerBytes(g.cfg.OriginationProfile),
			PayerOrgEntry:    relaysReferencePayerBytes(g.cfg.OriginationProfile), // payer Org as a resolvable PAS bundle entry (br-payer findInBundle)
			Payer:            res.payer,
			// The amendment states the facts and the certificationType the
			// request to this route states (the builder refuses a required one
			// it does not state); the prior Claim it restates is the recorded
			// submission's, read off the bytes the submission built — never a
			// constant; the infoChanged marker is derived from it by the
			// builder (an item sent before exactly as now carries none).
			PASClaimFacts:     claimFacts,
			CertificationType: claimIn.CertificationType,
			PriorClaim:        submitted.prior,
			// The payer's own pend: the attached report is linked to the
			// items it asks the report for, and to no other.
			Pend: submitted.pendFor(targetLine),
		})
		if err != nil {
			pasBuildFailure(w, err, "build update bundle failed")
			return nil, nil, true, nil
		}
		updateBundle, err = g.completeAuthoredPASRequest(ctx, updateBundle, pasQR, res.srJSON, res.coverageRef, relaysReferencePayerBytes(g.cfg.OriginationProfile))
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "PAS evidence linkage failed"})
			return nil, nil, true, nil
		}
		sent, failed := g.sendLeg(ctx, r, legSend{
			Workstream: workstreamPA, Leg: "pas-claim-update", Recipient: res.recipient, PCI: res.pci, Corr: updateCorr, Route: &route,
			Build: &moduleBuild{
				Builder: relay.BuilderSDKPASUpdate, ContentType: "application/fhir+json", Body: updateBundle, Contract: "pa.pas",
				Check: func(line string, body []byte) (int, string) {
					return g.validatePASAttachments(ctx, body, line, res.qrSource != nil)
				},
			},
		})
		if failed != nil && !failed.FromLeg {
			writeJSON(w, failed.Status, map[string]string{"error": failed.Msg})
			return nil, nil, true, nil
		}
		updateBundle = sent.Bytes
		if failed != nil {
			return updateBundle, nil, false, failed.Err
		}
		sentRoute = sent.Route
		return updateBundle, sent.Answer, false, nil
	})
	if handled {
		return
	}
	if err != nil {
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	route, targetLine = sentRoute, shnsdk.LineOf(sentRoute.Token)
	ctx = withFindingContext(ctx, findingContext{
		LegType: "pas-claim-update", CorrelationID: updateCorr, Seam: "originate", Whose: "peer",
	})
	if status, msg := g.validateFHIRPayerIngress(ctx, updateResp, targetLine, "pa.pas", res.payer); status != 0 {
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
type uc08Resp struct {
	PARequired bool   `json:"paRequired"`
	Denied     bool   `json:"denied"`
	AuthNumber string `json:"authNumber,omitempty"`
	Rationale  string `json:"rationale,omitempty"`
}

// handleUC08 runs the PA-denied path (UC-08, FR-22): CRD+DTR → PAS submit →
// denied ClaimResponse (reviewAction A3, no preAuthRef) → the provider answers
// denied with the ClaimResponse's own rationale. The denial is TERMINAL — it
// does NOT pend.
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
	member, ok := g.scenarioMember(w, r, "uc08")
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
	route, ok := g.selectLegLineOrFail(w, res.recipient, "pas-claim", pasCorr, relay.BuilderSDKPASSubmit)
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
	// The request's own facts, exactly as the request to this route states
	// them; the builder refuses a required one it does not state.
	claimIn, inOK := claimInputForOrder(w, r, res.srJSON)
	if !inOK {
		return
	}
	claimFacts := claimIn.facts()
	bundleJSON, err := buildAuthoredPASSubmit(route.BuildLine, shnsdk.ConformantClaimInputs{
		QR: pasQR, SR: res.srJSON, Provider: pasProviderJSON, Coverage: res.coverage, Insurer: res.insurer, PatientRef: res.patientRef, CoverageRef: res.coverageRef, MemberID: res.member, MemberIDSystem: pasMemberSystem,
		Corr: pasCorr, Created: g.cfg.Clock(),
		ContainedInsurer: relaysReferencePayerBytes(g.cfg.OriginationProfile),
		AbsoluteRefs:     relaysReferencePayerBytes(g.cfg.OriginationProfile),
		PayerOrgEntry:    relaysReferencePayerBytes(g.cfg.OriginationProfile), // payer Org as a resolvable PAS bundle entry (br-payer findInBundle)
		Payer:            res.payer,
		PASClaimFacts:    claimFacts,
	})
	if err != nil {
		pasBuildFailure(w, err, "build bundle failed: "+err.Error())
		return
	}
	bundleJSON, err = g.completeAuthoredPASRequest(ctx, bundleJSON, pasQR, res.srJSON, res.coverageRef, relaysReferencePayerBytes(g.cfg.OriginationProfile))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "PAS evidence linkage failed"})
		return
	}
	sent, failed := g.sendLeg(ctx, r, legSend{
		Workstream: workstreamPA, Leg: "pas-claim", Recipient: res.recipient, PCI: res.pci, Corr: pasCorr, Route: &route,
		Build: &moduleBuild{
			Builder: relay.BuilderSDKPASSubmit, ContentType: "application/fhir+json", Body: bundleJSON, Contract: "pa.pas",
			Check: func(line string, body []byte) (int, string) {
				return g.validatePASAttachments(ctx, body, line, res.qrSource != nil)
			},
		},
	})
	if failed != nil {
		g.writeLegFailure(w, failed)
		return
	}
	bundleJSON = sent.Bytes

	claimRespJSON := sent.Answer
	// What follows reads the route the claim was sent on.
	route, targetLine = sent.Route, shnsdk.LineOf(sent.Route.Token)
	ctx = withFindingContext(ctx, findingContext{
		LegType: "pas-claim", CorrelationID: pasCorr, Seam: "originate", Whose: "peer",
	})
	if status, msg := g.validateFHIRPayerIngress(ctx, claimRespJSON, targetLine, "pa.pas", res.payer); status != 0 {
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

	writeJSON(w, http.StatusOK, uc08Resp{
		PARequired: true,
		Denied:     true,
		AuthNumber: "",
		Rationale:  rationale,
	})
}

// neededItemCodes maps []shnsdk.NeededItem → []string using .Code, matching what
// the internal ParsePendedOrApproved returned as a plain []string (Task.input
// valueString). The scenario route's callers read these as opaque codes.
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

// eligibilityRequest builds the eligibility request for memberID, naming payer
// (the payer the member's own Coverage names) as its insurer. It names the
// provider (Practitioner/<NPI>) only when an NPI is configured; otherwise the
// request names no provider (CoverageEligibilityRequest.provider is optional)
// rather than a stand-in.
func (g *Gateway) eligibilityRequest(memberID string, payer shnsdk.PayerIdentifier) ([]byte, error) {
	b, err := shnsdk.BuildEligibilityRequestExact(memberID, g.cfg.NPI, payer, g.cfg.Clock())
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

// writeLegFailure answers a scenario caller whose request sendLeg did not get
// answered: a framed relay error from the leg as relayOriginationError writes
// it, anything else with the failure's status and message.
func (g *Gateway) writeLegFailure(w http.ResponseWriter, f *legFailure) {
	if f.FromLeg && g.relayOriginationError(w, f.Err) {
		return
	}
	writeJSON(w, f.Status, map[string]string{"error": f.Msg})
}
