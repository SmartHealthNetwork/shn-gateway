//go:build !shn_coreonly

package engine

import (
	"context"
	"errors"
	"net/http"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// refuseOriginRecord answers a read of the participant's records an
// origination's leg carries (the patient, the coverage, the order and its
// history), made before that leg is built, that did not supply them, and
// records it by what it is, told apart by err, never by the message:
//   - the fill fence refused a record that is not the bound patient's own
//     (err a *CompartmentError): the fidelity of SHN's own edit, at the verify
//     stage, 422;
//   - the system did not answer (any other err): a backend failure answered
//     with its own status (a 502 or 503) and recorded as one (failOriginRead);
//   - the system holds no such record, or SHN's build of the leg from it
//     cannot be made faithfully, as an open order about another product than
//     the route asks about (422, no err): the fidelity of SHN's own build, at
//     the build stage (refuseOwnBuild);
//   - anything else is answered with its status, unrecorded.
//
// Every answer says which legs the request already sent were carried.
func (g *Gateway) refuseOriginRecord(ctx context.Context, w http.ResponseWriter, recipient, leg string, status int, msg string, err error) {
	var fenced *CompartmentError
	switch {
	case errors.As(err, &fenced):
		g.refuseOwnVerify(ctx, w, recipient, leg, msg)
	case failOriginRead(ctx, w, status, msg, err):
	case status == http.StatusUnprocessableEntity:
		g.refuseOwnBuild(ctx, w, recipient, leg, msg)
	default:
		writeJSON(w, status, map[string]string{"error": withCarriedLegs(ctx, msg)})
	}
}

// refuseOwnVerify answers a record or a fill this gateway was to carry on leg
// that failed its own check of it, as one about another patient than the
// bound one: the fidelity of SHN's own edit, at the verify stage, 422,
// recorded as leg not sent, saying which legs were carried.
func (g *Gateway) refuseOwnVerify(ctx context.Context, w http.ResponseWriter, recipient, leg, msg string) {
	g.observeOwnBuildRefused(recipient, leg, msg)
	exchangeOfWriter(w).refusedFidelity(vocab.RefusalStageVerify)
	writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": withCarriedLegs(ctx, msg)})
}

// refuseOwnFill answers a questionnaire fill, made after the coverage check
// and questionnaire legs, whose QuestionnaireResponse is about another
// patient than the bound one or for another questionnaire than the one
// fetched (msg says which): SHN's own fill failing its own check
// (refuseOwnVerify), at the PAS claim that would carry it.
func (g *Gateway) refuseOwnFill(ctx context.Context, w http.ResponseWriter, recipient, msg string) {
	g.refuseOwnVerify(ctx, w, recipient, "pas-claim", msg)
}

// refuseOriginRouting answers an origination whose leg cannot be addressed —
// a member the system of record does not know, no patient health gateway to
// carry the patient's leg, a payer that cannot take the leg's frame — as a
// routing refusal, with status (400 for a member the request names that the
// system does not hold, else 422), saying which legs were carried.
func refuseOriginRouting(ctx context.Context, w http.ResponseWriter, status int, msg string, reason refusalReason) {
	exchangeOfWriter(w).refusedFor(RefusalRouting, reason)
	writeJSON(w, status, map[string]string{"error": withCarriedLegs(ctx, msg)})
}

// failOriginRead answers, at the build of a leg that needs the record, a read
// of the participant's system that failed to answer (err non-nil): a backend
// failure with its own status, recorded as one (sorFailure), never a refusal
// and never a record left out, saying which legs were carried. It reports
// whether it answered; a read that answered (err nil) is the caller's.
func failOriginRead(ctx context.Context, w http.ResponseWriter, status int, msg string, err error) bool {
	if err == nil {
		return false
	}
	// A member that names no one is the request's, refused on authority,
	// never recorded as the system of record's failure.
	if errors.Is(err, errMemberNamesNoOne) {
		exchangeOf(ctx).refusingFor(RefusalAuthority, reasonMemberNamesNoOne)
	} else {
		exchangeOf(ctx).sorFailure(err)
	}
	writeJSON(w, status, map[string]string{"error": withCarriedLegs(ctx, msg)})
	return true
}

// failOriginReadErr answers a failed read of the participant's system (err
// non-nil) as failOriginRead does, with the status SoRFailureResponse gives
// it: every read an origination makes, before any leg or after some, is
// answered this way. It reports whether it answered.
func failOriginReadErr(ctx context.Context, w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	status, msg := SoRFailureResponse(err)
	return failOriginRead(ctx, w, status, msg, err)
}

// answerFollowFailure answers a step after legs were sent that failed (an
// adaptive questionnaire round, or following a submission or an amendment to
// a decision): a leg's or the network's
// own failure as relayOriginationError answers it; a read of the
// participant's system that failed to answer (a *SoRReadError, as the pend's
// patient read) as the backend failure it is (failOriginRead); a request this
// gateway's PAS builder refused on the fidelity of its own build (an inquiry
// whose Coverage names a person whose record cannot ride it,
// isPASBuildFidelity) as that refusal, at the build stage, with
// buildFidelityStatus's status; anything else with its status. Each says which
// legs were carried.
func (g *Gateway) answerFollowFailure(ctx context.Context, w http.ResponseWriter, status int, msg string, err error) {
	if g.relayOriginationError(w, err) {
		return
	}
	if isPASBuildFidelity(err) {
		exchangeOf(ctx).refusedFidelity(vocab.RefusalStageBuild)
		writeJSON(w, buildFidelityStatus(err), map[string]string{"error": withCarriedLegs(ctx, msg)})
		return
	}
	var readErr *SoRReadError
	if errors.As(err, &readErr) && failOriginRead(ctx, w, status, msg, err) {
		return
	}
	writeJSON(w, status, map[string]string{"error": withCarriedLegs(ctx, msg)})
}

// failPopulate answers a questionnaire fill that failed, after the coverage
// check and questionnaire legs: a read of the participant's system, or its
// operated $populate engine, that failed to answer is a backend failure
// (failOriginRead); a fill the operated engine made about another patient is
// SHN's own fill failing its check (refuseOwnFill); any other failure is
// answered with its status. Each says which legs were carried.
func (g *Gateway) failPopulate(ctx context.Context, w http.ResponseWriter, recipient string, err error) {
	// A read for a member that names no one is the request's (failOriginReadErr).
	if errors.Is(err, errMemberNamesNoOne) {
		failOriginReadErr(ctx, w, err)
		return
	}
	status, msg := statusForPopulateErr(err), messageForPopulateErr(err)
	if errors.Is(err, errPopulateForeignSubject) {
		g.refuseOwnFill(ctx, w, recipient, msg)
		return
	}
	if (isSoRPopulateError(err) || errors.Is(err, errPopulateUpstream)) && failOriginRead(ctx, w, status, msg, err) {
		return
	}
	writeJSON(w, status, map[string]string{"error": withCarriedLegs(ctx, msg)})
}

// buildFidelityStatus is the status of a refusal on the fidelity of the
// gateway's own build, which follows whose content it is (gateway/STABILITY.md,
// "What the gateway cannot carry or edit"); the refusal's rule and stage are
// its own. It is 502 for what the gateway read from its own participant's
// system and cannot carry: a record the system of record answered that the
// build cannot carry as read (errRecordNotCarried, the built request's
// spelling checks among them), or that E-06 would re-point where a signature
// covers it. Every other such refusal is 422:
//   - a record the request names that the system does not hold
//     (evidenceNotHeldError): the request named it;
//   - any other signed-content refusal, and the SDK builders' own refusals of
//     the request's facts and carried records.
//
// Each is told apart by its type, never by its message.
func buildFidelityStatus(err error) int {
	var notHeld *evidenceNotHeldError
	if errors.As(err, &notHeld) {
		return http.StatusUnprocessableEntity
	}
	var signed *relay.SignedContentError
	if errors.As(err, &signed) {
		if signed.Edit == relay.EditEvidenceSubjectRekey {
			return http.StatusBadGateway
		}
		return http.StatusUnprocessableEntity
	}
	if errors.Is(err, errRecordNotCarried) {
		return http.StatusBadGateway
	}
	return http.StatusUnprocessableEntity
}

// evidenceFidelityRefusal is the answer to evidence the gateway's own edit
// cannot carry, recorded as the fidelity of SHN's own edit at the build, with
// buildFidelityStatus's status: the signed-content refusal (D113) for evidence
// E-06 would re-point and a signature covers, a record read from the
// participant's system that the build cannot carry as read, and a record the
// request references that the system does not hold, each saying so
// (errRecordNotCarried). A record read that is past the gateway's own carrying
// limits (evidencePastLimitError) is 502 naming the record and the limit. ok is
// false for any other error.
func evidenceFidelityRefusal(err error) (int, string, bool) {
	var past *evidencePastLimitError
	if errors.As(err, &past) {
		return http.StatusBadGateway, past.Error(), true
	}
	var signed *relay.SignedContentError
	if errors.As(err, &signed) {
		return buildFidelityStatus(err), signed.Error(), true
	}
	if errors.Is(err, errRecordNotCarried) {
		return buildFidelityStatus(err), err.Error(), true
	}
	return 0, "", false
}

// failEvidenceLinkage answers a PAS request whose evidence could not be
// linked (completeAuthoredPASRequest), after the legs before the claim:
// evidence the gateway's own edit cannot carry is refused with
// buildFidelityStatus's status (502 for a record read from the system of
// record, 422 for one the request names that it does not hold), and a record
// read past the gateway's own carrying limits 502, each recorded as the
// fidelity of the build (evidenceFidelityRefusal), a record the gateway could not prepare is its own
// 500, a record the system answered with something other than that resource
// is the system's invalid answer, 502 naming the record, and a read of the
// participant's system that failed to answer is a backend failure, each
// recorded as failOriginRead does, and anything else the gateway's own 502,
// refused under no named rule: a request graph the completion will not read
// that no record read takes past a limit (its own bounds and shape rules), an
// authored request that does not match its source, or no system of record
// configured (cfg.SoR nil). A record the system does
// not hold is refused 422 as evidence the build cannot fill
// (evidenceFidelityRefusal). Each says which legs were carried.
func failEvidenceLinkage(ctx context.Context, w http.ResponseWriter, err error) {
	const msg = "PAS evidence linkage failed"
	if status, why, ok := evidenceFidelityRefusal(err); ok {
		exchangeOf(ctx).refusedFidelity(vocab.RefusalStageBuild)
		writeJSON(w, status, map[string]string{"error": withCarriedLegs(ctx, why)})
		return
	}
	if evidencePrepareFailed(err) {
		// This gateway could not prepare a record it read: its own failure.
		exchangeOf(ctx).ownFailure()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": withCarriedLegs(ctx, err.Error())})
		return
	}
	var unreadable *evidenceUnreadableError
	if errors.As(err, &unreadable) {
		failOriginRead(ctx, w, http.StatusBadGateway, msg+": "+unreadable.Error(), err)
		return
	}
	var readErr *SoRReadError
	if errors.As(err, &readErr) || errors.Is(err, errMemberNamesNoOne) {
		status, why := SoRFailureResponse(err)
		failOriginRead(ctx, w, status, msg+": "+why, err)
		return
	}
	// The gateway's own refusal of the request it built, under no named rule,
	// whatever legs came before it: decided here, never left to them, as the
	// PAS tail's answer is recorded.
	exchangeOf(ctx).refused(ExchangeOther)
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": withCarriedLegs(ctx, msg)})
}

// msgNoPHG is the refusal of a call to originate to a patient's PHG when the
// network registers none.
const msgNoPHG = "no PHG registered"

// readRecordNotCarried is err, an amendment build's failure, with the SDK's
// refusal of a record it carries as written (shnsdk.ErrPASRecordNotCarried)
// answered as a record read from the participant's own system that the build
// cannot carry as read (errRecordNotCarried, 502): for a build whose every
// record carried so is the system's or SHN's own, decided by the build's
// inputs at its call site, never by the message. Any other error is returned
// as it is.
func readRecordNotCarried(err error) error {
	if errors.Is(err, shnsdk.ErrPASRecordNotCarried) {
		return &readRecordNotCarriedError{err: err}
	}
	return err
}

// readRecordNotCarriedError keeps the SDK's refusal (its message, and
// errors.Is reporting shnsdk.ErrPASRecordNotCarried) and reports
// errRecordNotCarried too, so buildFidelityStatus answers it 502.
type readRecordNotCarriedError struct{ err error }

func (e *readRecordNotCarriedError) Error() string { return e.err.Error() }

func (e *readRecordNotCarriedError) Unwrap() error { return e.err }

func (e *readRecordNotCarriedError) Is(target error) bool { return target == errRecordNotCarried }
