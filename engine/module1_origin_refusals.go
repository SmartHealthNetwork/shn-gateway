//go:build !shn_coreonly

package engine

import (
	"context"
	"errors"
	"net/http"

	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
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
func refuseOriginRouting(ctx context.Context, w http.ResponseWriter, status int, msg string) {
	exchangeOfWriter(w).refused(RefusalRouting)
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
	exchangeOf(ctx).sorFailure(err)
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
// isPASBuildFidelity) as that refusal, at the build stage; anything else with
// its status. Each says which legs were carried.
func (g *Gateway) answerFollowFailure(ctx context.Context, w http.ResponseWriter, status int, msg string, err error) {
	if g.relayOriginationError(w, err) {
		return
	}
	if isPASBuildFidelity(err) {
		exchangeOf(ctx).refusedFidelity(vocab.RefusalStageBuild)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": withCarriedLegs(ctx, msg)})
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

// failEvidenceLinkage answers a PAS request whose evidence could not be
// linked (completeAuthoredPASRequest), after the legs before the claim: a
// read of the participant's system that failed to answer is a backend
// failure (failOriginRead), anything else the gateway's own 502. Each says
// which legs were carried.
func failEvidenceLinkage(ctx context.Context, w http.ResponseWriter, err error) {
	const msg = "PAS evidence linkage failed"
	var readErr *SoRReadError
	if errors.As(err, &readErr) {
		status, why := SoRFailureResponse(err)
		failOriginRead(ctx, w, status, msg+": "+why, err)
		return
	}
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": withCarriedLegs(ctx, msg)})
}
