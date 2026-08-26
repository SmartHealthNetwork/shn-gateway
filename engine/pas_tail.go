// pas_tail.go — the shared LEAN PAS submit→resolve tail, extracted from handleHomeOxygen
// (the provider-data order-dispatch handler) so the provider-data order-select single-shot
// lanes (UC-02/03/04, D-PD-1) reuse one path instead of forking a second.
//
// submitClaimAndResolve is the SINGLE-SHOT submit tail: build the conformant Claim Bundle →
// egress-$validate → originate the pas-claim leg → ingress-$validate → classify the resolved
// ClaimResponse. There is NO amendment leg on this tail (that is the UC-04/06 path).
// A single-shot ServiceRequest sets the Da Vinci PAS infoChanged item extension so the payer
// gate POLLS the timer-resolved terminal A1 (handlePASClaimNative); a single-shot DeviceRequest
// (HomeOxygen) does NOT — its order type alone routes it to the same poll. infoChanged is the
// payer-side poll DISCRIMINATOR, NOT a verdict input (the verdict is br-payer's code-keyed CQL
// constant; the A4→A1 is br-payer's own timer).
package engine

import (
	"context"
	"net/http"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// buildPASSubmitBundle assembles the single-shot conformant $submit Claim Bundle for the lean PAS
// tail. brPayer mirrors relaysReferencePayerBytes(OriginationProfile) (NOT the narrower
// targetsBrPayer — both provider-data over a live HTTP dial to br-payer AND demo through the
// in-process mirror of the SAME reference-payer bytes need the br-payer-resolvable wire shape;
// see relaysReferencePayerBytes's doc, gateway/engine/originate.go): when true the bundle carries the
// br-payer-resolvable forms (ContainedInsurer/AbsoluteRefs/PayerOrgEntry), exactly as the existing
// HomeOxygen path built them. InfoChanged is set to !orderIsDeviceRequest(order) AND only on a
// reference-payer-relaying lane:
//   - a DeviceRequest single-shot (HomeOxygen) → InfoChanged stays FALSE → the bundle is
//     byte-IDENTICAL to HomeOxygen's prior BuildConformantClaimBundle call (the order type alone
//     routes it to the payer poll); and
//   - a ServiceRequest single-shot (order-select, D-PD-1) → InfoChanged TRUE → the bundle carries
//     the infoChanged poll discriminator so the payer gate polls the timer-resolved A1.
//
// On a lane that does NOT relay reference-payer bytes (brPayer=false) InfoChanged is never set,
// keeping the byte-identical pre-existing path. Pulled out as a standalone func so the
// byte-parity guard can unit-test it directly.
// line is the routed contract line the bundle is BUILT at (select before build) —
// resolved by submitClaimAndResolve before this call, so the wire bytes and the
// routed token cannot disagree.
func buildPASSubmitBundle(line string, brPayer bool, orderJSON, qrJSON []byte, patientRef, coverageRef, member, corr string, created time.Time, payer shnsdk.PayerIdentifier) ([]byte, error) {
	return shnsdk.BuildConformantClaimBundleAtLine(line, shnsdk.ConformantClaimInputs{
		QR: qrJSON, SR: orderJSON, PatientRef: patientRef, CoverageRef: coverageRef, MemberID: member,
		Corr: corr, Created: created,
		ContainedInsurer: brPayer,
		AbsoluteRefs:     brPayer,
		PayerOrgEntry:    brPayer, // payer Org as a resolvable PAS bundle entry (br-payer findInBundle)
		// Single-shot resolve discriminator: a ServiceRequest single-shot signals "resolve to
		// terminal" via the infoChanged item extension so the payer gate polls the timer-resolved A1;
		// a DeviceRequest (HomeOxygen) stays false (its order type alone routes it to the same poll),
		// so HomeOxygen's wire bytes are unchanged. Only on a reference-payer-relaying lane (a lane
		// that does not relay reference-payer bytes keeps the byte-identical no-infoChanged path).
		InfoChanged: brPayer && !orderIsDeviceRequest(orderJSON),
		// The payer identity derives from the member's REAL Coverage (threaded in from the fresh
		// origination site), not a synthetic CMS literal (FR-G40).
		Payer: payer,
	})
}

// submitClaimAndResolve is the shared lean single-shot PAS tail. It builds + egress-validates the
// conformant Claim Bundle, originates the pas-claim leg, ingress-validates the response, and
// classifies the resolved ClaimResponse via the EXISTING g.classifyResolution. On approval it
// returns (parsed, respJSON, 0, "", nil); otherwise (PriorAuthResult{}, respJSON, status, msg, err)
// with the SAME statuses/messages handleHomeOxygen produced inline (so its behavior is
// byte-preserved). The trailing err carries the RAW OriginateLeg error (with its %w chain intact) on
// the leg-failure path and is nil on every other path — it exists SOLELY so the caller can attempt
// relayOriginationError before the writeJSON(status,msg) fallback; a bare (status,msg) return would
// re-synthesize the error to a string and DROP the *RelayError sentinel (the %w audit). The caller
// does the FR-23 StoreAuthNumber + writes the response surface. respJSON is returned on every path
// (incl. failures) for diagnosis; it is nil only when the failure precedes the leg call.
func (g *Gateway) submitClaimAndResolve(ctx context.Context, r *http.Request, pci string, orderJSON, qrJSON []byte, patientRef, coverageRef, member string, payer shnsdk.PayerIdentifier, recipient string) (shnsdk.PriorAuthResult, []byte, int, string, error) {
	pasCorr := g.cfg.CorrelationGen()
	// Select-before-build: this tail used to let OriginateLeg select
	// INTERNALLY off an empty Content.ProfileID, which put the choice AFTER the bundle
	// was already built. The routed line now chooses the builder, so it is hoisted here
	// — a refusal returns the *RouteRefusalError through the existing err channel, which
	// the caller already relays as the legible 422.
	route, terr := g.selectLegLine(recipient, "pas-claim", pasCorr)
	if terr != nil {
		return shnsdk.PriorAuthResult{}, nil, http.StatusBadGateway, terr.Error(), terr
	}
	targetLine := shnsdk.LineOf(route.Token)
	bundleJSON, err := buildPASSubmitBundle(route.BuildLine, relaysReferencePayerBytes(g.cfg.OriginationProfile), orderJSON, qrJSON, patientRef, coverageRef, member, pasCorr, g.cfg.Clock(), payer)
	if err != nil {
		return shnsdk.PriorAuthResult{}, nil, http.StatusInternalServerError, "build bundle failed", nil
	}
	bundleJSON, _, aerr := g.egressAdapt(route, bundleJSON, ExchangeIdentity{CorrelationID: pasCorr, LegType: "pas-claim", Counterpart: recipient})
	if aerr != nil {
		return shnsdk.PriorAuthResult{}, nil, http.StatusBadGateway, aerr.Error(), aerr
	}
	if status, msg := g.validateFHIR(ctx, bundleJSON, "egress", targetLine); status != 0 {
		return shnsdk.PriorAuthResult{}, nil, status, msg, nil
	}
	// recipient is the payer HOLDER resolved from the member's real Coverage at the fresh origination
	// site (FR-G40) — no default; it replaced the deleted Config.CounterpartID here.
	respJSON, err := g.OriginateLeg(ctx, r, recipient, "pas-claim", pci, pasCorr, "",
		Content{WorkstreamType: workstreamPA, ProfileID: route.Token, Route: routeInfoFor(route), Bytes: bundleJSON})
	if err != nil {
		// Return the RAW err (not just err.Error()) so the caller can relayOriginationError a framed
		// *RelayError verbatim; msg stays for the non-relay writeJSON fallback (byte-identical).
		return shnsdk.PriorAuthResult{}, nil, http.StatusBadGateway, err.Error(), err
	}
	if status, msg := g.validateFHIRPayerIngress(ctx, respJSON, targetLine); status != 0 {
		return shnsdk.PriorAuthResult{}, respJSON, status, msg, nil
	}
	// classifyResolution returns approved only for a genuine terminal A1 (the payer gate has already
	// polled br-payer's timer A4→A1 for a single-shot); a parse failure or any non-approved outcome
	// (a denial or an unresolved pend) is a genuine non-approval, never a silent pass.
	parsed, approved := g.classifyResolution(respJSON)
	if !approved {
		// Preserve handleHomeOxygen's two distinct messages: an UNPARSEABLE 2xx is "claim response
		// parse failed"; a parsed-but-not-approved response is "preauthorization not approved".
		if _, perr := shnsdk.ParseClaimResponse(respJSON); perr != nil {
			return shnsdk.PriorAuthResult{}, respJSON, http.StatusBadGateway, "claim response parse failed", nil
		}
		return shnsdk.PriorAuthResult{}, respJSON, http.StatusBadGateway, "preauthorization not approved", nil
	}
	return parsed, respJSON, 0, "", nil
}
