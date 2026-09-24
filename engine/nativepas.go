// Native PAS operations relay participant messages exactly. A successful submit
// may also schedule the payer's pre-existing local pend/EOB projection. The
// gateway executes that effect only after the response is written and flushed,
// so local parsing or storage can never change the peer's answer.
package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
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
// its backend's exact answer. Its Commit parses and records the existing
// participant-local projection after response delivery.
func (n *nativeResponder) handlePASClaimNative(ctx context.Context, corrID, subjectPCI string, in relay.Body, requestFHIR []byte) (LegResult, error) {
	forward, refused, err := n.payorEdgeRequest(in, payorEdgePASBundle, nativeRequestMedia(ctx, "application/fhir+json"))
	if err != nil || refused.Status != 0 {
		return refused, err
	}
	up, bad, err := n.post(ctx, n.pasBase(), "/Claim/$submit", forward, "pas-claim", "PAS submit")
	if err != nil || bad.Status != 0 {
		return bad, err
	}
	return LegResult{
		ApplicationStatus:       up.status,
		ResponseContractVersion: up.version,
		ResponseVersionSource:   up.versionSource,
		Response:                relay.Exact(up.body, up.contentType),
		ResponseSubjectForeign:  true,
		Commit:                  n.projectPASSubmit(ctx, corrID, subjectPCI, requestFHIR, up.raw),
	}, nil
}

// projectPASSubmit returns the payer-local projection of an already-delivered
// submit answer. Every clinical read and every Store write stays inside the
// closure. Failure is diagnostic only at the native delivery boundary.
func (n *nativeResponder) projectPASSubmit(ctx context.Context, corrID, subjectPCI string, requestFHIR, responseFHIR []byte) func() error {
	requestFHIR = append([]byte(nil), requestFHIR...)
	responseFHIR = append([]byte(nil), responseFHIR...)
	return func() error {
		s, status, msg := parseConformantPASSubjects(requestFHIR)
		if status != 0 {
			return fmt.Errorf("parse PAS submit for local projection: %s", msg)
		}
		response, lr := validateRelayedPASResponse(corrID, "pas-claim", responseFHIR)
		if lr.Status != 0 {
			return fmt.Errorf("parse PAS answer for local projection: %s", lr.Message)
		}
		pended, _, err := shnsdk.ParsePendedResponse(response)
		if err != nil {
			return fmt.Errorf("parse pended PAS answer for local projection: %w", err)
		}
		requester := requesterHolderOf(ctx)
		answerKeys, created := pasAnswerKeys(requester, response)
		if pended {
			return recordPASPend(ctx, n.store, subjectPCI, corrID,
				mergePendKeys(requester, answerKeys, pasRequestKeys(requestFHIR)), created)()
		}
		parsed, err := shnsdk.ParseClaimResponse(response)
		if err != nil {
			return fmt.Errorf("parse PAS decision for local projection: %w", err)
		}
		if parsed.Outcome == "denied" && payerStatedAuthNumber(response) != "" {
			return fmt.Errorf("payer decision states both a denial and an authorization number")
		}
		procSystem, code, display, _ := shnsdk.ParseOrderProductCoding(s.srJSON)
		var eob *EOBRecord
		if code != "" {
			eobJSON, err := n.projectDecisionEOB(corrID, "Patient/"+s.member, procSystem, code, display, parsed)
			if err != nil {
				return fmt.Errorf("project payer decision EOB: %w", err)
			}
			eob = &EOBRecord{SubjectPCI: subjectPCI, EOBID: "eob-" + corrID, JSON: eobJSON}
		}
		return recordPASDecision(ctx, n.store, subjectPCI, corrID, pendOutcomeOf(parsed.Outcome), created, eob)()
	}
}

func (n *nativeResponder) projectDecisionEOB(corrID, patientRef, procSystem, code, display string, parsed shnsdk.PriorAuthResult) ([]byte, error) {
	return decisionEOB(n.clock, corrID, patientRef, procSystem, code, display, parsed)
}

// decisionEOB is the existing payer-local PDex projection shared with inquiry.
// It states only facts carried by the participant's request and answer.
func decisionEOB(clock func() time.Time, corrID, patientRef, procSystem, code, display string, parsed shnsdk.PriorAuthResult) ([]byte, error) {
	decision, authNumber := shnsdk.PADecisionApproved, parsed.PreAuthRef
	notes := parsed.ProcessNotes
	if notes == nil {
		notes = []shnsdk.PASProcessNote{}
	}
	var denialReasons []shnsdk.PASCoding
	if parsed.Outcome == "denied" {
		decision, authNumber = shnsdk.PADecisionDenied, ""
		denialReasons = parsed.DenialReasons
		if denialReasons == nil {
			denialReasons = []shnsdk.PASCoding{}
		}
	} else if len(parsed.DenialReasons) > 0 {
		return nil, fmt.Errorf("reason codes on a decision that is not a denial")
	}
	return shnsdk.BuildPADecisionEOB(shnsdk.PADecisionEOBParams{
		ID:              "eob-" + corrID,
		PatientRef:      patientRef,
		CoverageRef:     "Coverage/" + strings.TrimPrefix(patientRef, "Patient/"),
		CPTCode:         code,
		CPTDisplay:      display,
		ProcedureSystem: procSystem,
		Decision:        decision,
		AuthNumber:      authNumber,
		Created:         clock(),
		ProcessNotes:    notes,
		ReviewAction:    parsed.ReviewAction,
		DenialReasons:   denialReasons,
	})
}

func payerStatedAuthNumber(body []byte) string {
	g, err := readPASGraph(body)
	if err != nil {
		return ""
	}
	ref, _ := g.response.resource["preAuthRef"].(string)
	return ref
}
