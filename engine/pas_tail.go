// pas_tail.go — the shared LEAN PAS submit→resolve tail, extracted from handleHomeOxygen
// (the provider-data order-dispatch handler) so the provider-data order-select single-shot
// lanes (UC-02/03/04, D-PD-1) reuse one path instead of forking a second.
//
// submitClaimAndResolve is the SINGLE-SHOT submit tail: build the conformant Claim Bundle →
// egress-$validate → originate the pas-claim leg → ingress-$validate → classify the resolved
// ClaimResponse. There is NO amendment leg on this tail (that is the sandbox UC-04/06 path).
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
// tail. brPayer mirrors targetsBrPayer(OriginationProfile): when true the bundle carries the
// br-payer-resolvable forms (ContainedInsurer/AbsoluteRefs/PayerOrgEntry), exactly as the existing
// HomeOxygen path built them. InfoChanged is set to !orderIsDeviceRequest(order) AND only on a
// br-payer-targeting lane:
//   - a DeviceRequest single-shot (HomeOxygen) → InfoChanged stays FALSE → the bundle is
//     byte-IDENTICAL to HomeOxygen's prior BuildConformantClaimBundle call (the order type alone
//     routes it to the payer poll); and
//   - a ServiceRequest single-shot (order-select, D-PD-1) → InfoChanged TRUE → the bundle carries
//     the infoChanged poll discriminator so the payer gate polls the timer-resolved A1.
//
// On the sandbox/managed lane (brPayer=false) InfoChanged is never set, keeping the byte-identical
// sandbox path. Pulled out as a standalone func so the byte-parity guard can unit-test it directly.
func buildPASSubmitBundle(brPayer bool, orderJSON, qrJSON []byte, patientRef, coverageRef, corr string, created time.Time, payer shnsdk.PayerIdentifier) ([]byte, error) {
	return shnsdk.BuildConformantClaimBundle(shnsdk.ConformantClaimInputs{
		QR: qrJSON, SR: orderJSON, PatientRef: patientRef, CoverageRef: coverageRef,
		Corr: corr, Created: created,
		ContainedInsurer: brPayer,
		AbsoluteRefs:     brPayer,
		PayerOrgEntry:    brPayer, // payer Org as a resolvable PAS bundle entry (br-payer findInBundle)
		// Single-shot resolve discriminator: a ServiceRequest single-shot signals "resolve to
		// terminal" via the infoChanged item extension so the payer gate polls the timer-resolved A1;
		// a DeviceRequest (HomeOxygen) stays false (its order type alone routes it to the same poll),
		// so HomeOxygen's wire bytes are unchanged. Only on a br-payer-targeting lane (the sandbox
		// lane keeps the byte-identical no-infoChanged path).
		InfoChanged: brPayer && !orderIsDeviceRequest(orderJSON),
		// The payer identity derives from the member's REAL Coverage (threaded in from the fresh
		// origination site), not a synthetic CMS literal (FR-G40).
		Payer: payer,
	})
}

// submitClaimAndResolve is the shared lean single-shot PAS tail. It builds + egress-validates the
// conformant Claim Bundle, originates the pas-claim leg, ingress-validates the response, and
// classifies the resolved ClaimResponse via the EXISTING g.classifyResolution. On approval it
// returns (parsed, respJSON, 0, ""); otherwise (PriorAuthResult{}, respJSON, status, msg) with the
// SAME statuses/messages handleHomeOxygen produced inline (so its behavior is byte-preserved). The
// caller does the FR-23 StoreAuthNumber + writes the response surface. respJSON is returned on every
// path (incl. failures) for diagnosis; it is nil only when the failure precedes the leg call.
func (g *Gateway) submitClaimAndResolve(ctx context.Context, r *http.Request, pci string, orderJSON, qrJSON []byte, patientRef, coverageRef string, payer shnsdk.PayerIdentifier, recipient string) (shnsdk.PriorAuthResult, []byte, int, string) {
	pasCorr := g.cfg.CorrelationGen()
	bundleJSON, err := buildPASSubmitBundle(targetsBrPayer(g.cfg.OriginationProfile), orderJSON, qrJSON, patientRef, coverageRef, pasCorr, g.cfg.Clock(), payer)
	if err != nil {
		return shnsdk.PriorAuthResult{}, nil, http.StatusInternalServerError, "build bundle failed"
	}
	if status, msg := g.validateFHIR(ctx, bundleJSON, "egress"); status != 0 {
		return shnsdk.PriorAuthResult{}, nil, status, msg
	}
	// recipient is the payer HOLDER resolved from the member's real Coverage at the fresh origination
	// site (FR-G40) — no default; it replaced the deleted Config.CounterpartID here.
	respJSON, err := g.OriginateLeg(ctx, r, recipient, "pas-claim", pci, pasCorr, "", Content{WorkstreamType: workstreamPA, Bytes: bundleJSON})
	if err != nil {
		return shnsdk.PriorAuthResult{}, nil, http.StatusBadGateway, err.Error()
	}
	if status, msg := g.validateFHIR(ctx, respJSON, "ingress"); status != 0 {
		return shnsdk.PriorAuthResult{}, respJSON, status, msg
	}
	// classifyResolution returns approved only for a genuine terminal A1 (the payer gate has already
	// polled br-payer's timer A4→A1 for a single-shot); a parse failure or any non-approved outcome
	// (a denial or an unresolved pend) is a genuine non-approval, never a silent pass.
	parsed, approved := g.classifyResolution(respJSON)
	if !approved {
		// Preserve handleHomeOxygen's two distinct messages: an UNPARSEABLE 2xx is "claim response
		// parse failed"; a parsed-but-not-approved response is "preauthorization not approved".
		if _, perr := shnsdk.ParseClaimResponse(respJSON); perr != nil {
			return shnsdk.PriorAuthResult{}, respJSON, http.StatusBadGateway, "claim response parse failed"
		}
		return shnsdk.PriorAuthResult{}, respJSON, http.StatusBadGateway, "preauthorization not approved"
	}
	return parsed, respJSON, 0, ""
}
