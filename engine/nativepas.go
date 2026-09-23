// Native PAS operations deliver participant messages without consuming their
// clinical decisions. Local ledger and EOB readers below are separate utilities;
// native delivery does not invoke them or schedule their writes.
package engine

import (
	"context"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

// handlePASClaimUpdateNative posts the source amendment exactly once. The
// participant backend decides whether it recognizes the prior authorization;
// its application status and body are returned without a local pend lookup.
func (n *nativeResponder) handlePASClaimUpdateNative(ctx context.Context, corrID, subjectPCI string, in relay.Body, requestFHIR []byte) (LegResult, error) {
	forward, refused, err := n.payorEdgeRequest(in, payorEdgePASBundle, nativeRequestMedia(ctx, "application/fhir+json"))
	if err != nil || refused.Status != 0 {
		return refused, err
	}
	up, bad, err := n.post(ctx, n.pasBase(), "/Claim/$submit", forward, "pas-claim-update", "PAS submit")
	if err != nil || bad.Status != 0 {
		return bad, err
	}
	return LegResult{ApplicationStatus: up.status, ResponseContractVersion: up.version, ResponseVersionSource: up.versionSource, Response: relay.Exact(up.body, up.contentType), ResponseSubjectForeign: true}, nil
}

// beginClaimUpdate binds the prior authorization for one amendment, reporting WHY
// it refused. A Store with a pend ledger answers the reason itself; one without
// can only say "not pended", which is the same answer it has always given.
func (n *nativeResponder) beginClaimUpdate(subjectPCI, related string) (bool, PendRefusal, error) {
	if ledger, ok := LedgerOf(n.store); ok {
		return ledger.BeginClaimUpdateReason(subjectPCI, related)
	}
	claimed, err := n.store.BeginClaimUpdate(subjectPCI, related)
	if err != nil || claimed {
		return claimed, PendRefusalNone, err
	}
	return false, PendRefusalNotPended, nil
}

// claimUpdateRefusal states why an amendment could not bind. The reasons stay
// distinguishable: an authorization this payer has already decided is a different
// answer from one it never pended, and a requester can act on the difference.
func claimUpdateRefusal(why PendRefusal) string {
	switch why {
	case PendRefusalDecided:
		return "claim already decided; amendment of a decided authorization is not supported"
	case PendRefusalInProgress:
		return "an amendment of this authorization is already in progress"
	default:
		return "ClaimUpdate references no pending claim available for this patient"
	}
}

// pendOutcomeOf maps a parsed PAS decision to the ledger's outcome. Only approved
// and denied are decisions; anything else is recorded as approved would be this
// gateway deciding, so the pre-existing reading (anything not denied is an
// approval, the same reading the decision EOB takes) is kept in ONE place.
func pendOutcomeOf(outcome string) string {
	if outcome == "denied" {
		return PendOutcomeDenied
	}
	return PendOutcomeApproved
}

// handlePASClaimNative forwards the participant's exact submission and returns
// its backend's exact answer. Delivery does not authorize a local projection.
func (n *nativeResponder) handlePASClaimNative(ctx context.Context, corrID, subjectPCI string, in relay.Body, requestFHIR []byte) (LegResult, error) {
	forward, refused, err := n.payorEdgeRequest(in, payorEdgePASBundle, nativeRequestMedia(ctx, "application/fhir+json"))
	if err != nil || refused.Status != 0 {
		return refused, err
	}
	up, bad, err := n.post(ctx, n.pasBase(), "/Claim/$submit", forward, "pas-claim", "PAS submit")
	if err != nil || bad.Status != 0 {
		return bad, err
	}
	return LegResult{ApplicationStatus: up.status, ResponseContractVersion: up.version, ResponseVersionSource: up.versionSource, Response: relay.Exact(up.body, up.contentType), ResponseSubjectForeign: true}, nil
}
