//go:build !shn_coreonly

package engine

import "github.com/SmartHealthNetwork/shn-gateway/engine/relay"

// lineBridge is the PA workstream's contribution to module 2: its rows of the
// compatibility manifest, the legs the chain walks without rewriting, and the
// one answer module 2 edits (E-08, the DTR answer's line-specific names).
func (paWorkstream) lineBridge() lineBridgeRows {
	return lineBridgeRows{
		steps:              paCompatSteps,
		envelopeEgressLegs: paEnvelopeEgressLegs,
		verbatimChainLegs:  paVerbatimChainLegs,
		answers:            map[relay.Key]answerBridge{dtrAnswerKey: chainBridge.carryDTRAnswer},
	}
}

// paEnvelopeEgressLegs are the PA workstream's envelopeEgressLegs: the
// DTR-fetch-ONLY non-FHIR carve-out (the multi-version spec's recorded
// DTR-fetch known-gap obligation, discharged).
// Membership means egressAdapt walks
// route.Chain for the routing/observer story but never hands the bytes to a
// step function — safe only because dtr-questionnaire-fetch's payload is
// the operation's own input, carried to the payer's line unchanged: a walk
// that would change a byte of it is never applied (handleDTRIngress carries
// the participant's request as sent; carryUnchanged sends the gateway's own
// as built, at the payer's line it was built at, with no walk), so no pa.dtr
// compat-manifest step ever rewrites it.
//
// CRD legs (crd-order-select, crd-order-dispatch) must NEVER join this set:
// their arm-3 byte-identity rests on the identity chain genuinely RUNNING
// (TestD7CRDArm3IdentityChainIsBytePreserving) — a CDS Hooks payload IS FHIR
// content the pa.crd manifest models, so carving it out here would misname
// it as an envelope, would be redundant (applyChain's nil-func identity path
// is already a byte-pure pass-through with identical reports, transform.go's
// applyChain), and would silently bypass any FUTURE real CRD module instead
// of making it run or refuse honestly. TestCRDLegsNeverJoinEnvelopeCarveOut
// pins the set's exact membership.
//
// Terminology note: the commit that discharged it calls this "proven
// safe by byte-identity guard" — there is no RUNTIME guard (no equality
// check anywhere in AdaptRequest (transform.go); out is never a copy, so one would be
// structurally unreachable). "Guard" there means
// TestEnvelopeLegChainIsByteIdenticalPassThrough, the test-time pin that
// enforces byte-identity by construction, not a production code path.
var paEnvelopeEgressLegs = []string{"dtr-questionnaire-fetch"}

// paVerbatimChainLegs are the PA workstream's verbatimChainLegs: legs whose
// payload the compat chain WALKS — so the routing and observer story stays
// honest — and never rewrites.
//
// pas-claim-inquire is one, for two reasons that both have to hold.
//
// FIRST, the chain has nothing to do to it. Every pa.pas step models a
// difference between the SUBMIT/AMENDMENT profiles (profile-claim,
// profile-claim-update) or the response profile: the Claim.item line detail and
// the related-claim relationship, and ClaimResponse.request. An inquiry's Claim
// is profile-claim-inquiry, a third profile that states none of them — the same
// distinction the certification layer draws (workstream_pa_conformance_species.go)
// and the validator lane scopes (workstream_pa_linefake.go). Measured: running the chain over an inquiry changed not
// one fact, only the key order of the re-marshalled JSON.
//
// SECOND, that re-marshal is exactly what an inquiry must not suffer. Its
// payload CARRIES the participant's own Patient, Coverage, provider and payer
// records as verbatim spans, sealed as embeds — that is what makes the bytes on
// the wire provably the participant's records rather than a rendering of them.
// A step that re-serialized them would reorder their keys and leave the seal
// describing a message that no longer exists.
//
// inquireContinuation keeps the runtime byte-equality check, so a future
// inquiry-aware step that DID change the bytes is never sent under a broken
// seal: the inquiry is sent as built, at its build line, or refused under the
// participant's REQUIRE_SHARED_LINE.
var paVerbatimChainLegs = []string{"pas-claim-inquire"}
