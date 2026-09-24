// eobowner.go — one decision EOB id per patient.
//
// A payer gateway files the decision EOB of an authorization under
// decisionEOBID(correlation id), and the requester chooses the correlation id.
// Two patients' exchanges under ONE correlation id would therefore name one EOB
// id, and the stores refuse the second patient's write (ErrEOBSubjectMismatch) so
// that the first patient's Patient Access list never shows the second patient's
// decision.
//
// A refusal at the write is too late to be the whole answer: by then the payer
// has acted on the claim, and its answer must still reach the requester. So the
// submit leg refuses the collision BEFORE the payer is asked (correlationTaken),
// and the write-time refusal is left for the race that check cannot close —
// another patient's decision landing between the check and the write — where
// the payer's answer relays and the decision is reported as not recorded
// (reportEOBOwnedElsewhere).
package engine

import (
	"log"
	"net/http"
)

// decisionEOBID is the id a payer decision's EOB is filed under. Every leg that
// writes a decision EOB, and the check that refuses a collision before the payer
// is asked, reads it from here, so the check names exactly the id the write would.
func decisionEOBID(corrID string) string { return "eob-" + corrID }

// EOBOwnerLookup is an OPTIONAL Store capability: which patient an EOB id is
// already filed for. A Store without it skips the pre-forward check, and its
// write-time refusal (ErrEOBSubjectMismatch) is then the only guard.
type EOBOwnerLookup interface {
	// EOBOwner reports the subject PCI eobID is filed for. found=false when no
	// EOB has that id; the error is reserved for a store failure.
	EOBOwner(eobID string) (subjectPCI string, found bool, err error)
}

// PendCorrelationLookup is an OPTIONAL Store capability: whether an authorization
// the payer has not yet decided is pended under a correlation id for a patient
// other than the one named. That authorization's decision EOB will be filed under
// the same id, so a second patient's decision under that correlation would leave
// the first patient's later decision unrecordable.
type PendCorrelationLookup interface {
	// PendedForOtherSubject reports a subject PCI other than subjectPCI with an
	// undecided (pended or in-progress) authorization under corrID. found=false
	// when there is none; the error is reserved for a store failure.
	PendedForOtherSubject(corrID, subjectPCI string) (otherPCI string, found bool, err error)
}

const (
	// refusalCorrelationTaken is the submit leg's 409 when the exchange's
	// correlation id already names another patient's authorization.
	refusalCorrelationTaken = "correlation id already names another patient's authorization"
	// refusalHolderReadFailed is the submit leg's 502 when the store cannot say
	// whether the correlation id is taken.
	refusalHolderReadFailed = "holder read failed"
	// pendDecisionNotRecordedEvent reports a payer decision that relayed to the
	// requester but was not recorded.
	pendDecisionNotRecordedEvent = "pend.decision-not-recorded"
	// reasonEOBOwnedElsewhere is why a decision went unrecorded when its EOB id
	// was filed for another patient first.
	reasonEOBOwnedElsewhere = "eob id belongs to another patient"
)

// correlationTaken is the submit leg's pre-forward check: it refuses, BEFORE the
// payer is asked, an exchange whose correlation id already names another
// patient's authorization — an EOB filed for another patient under the id this
// exchange's decision would be filed under, or another patient's authorization
// still awaiting its decision under this correlation id. Status 0 is "not taken".
//
// A store that cannot answer is an outage and is treated as the submit leg treats
// every other store failure: a 502 that is the gateway's own fault, and the
// payer is not asked. Proceeding would let the payer act on a claim whose
// decision this gateway might then be unable to record.
func (g *Gateway) correlationTaken(subjectPCI, corrID string) (int, string) {
	if l, ok := g.cfg.Store.(EOBOwnerLookup); ok {
		owner, found, err := l.EOBOwner(decisionEOBID(corrID))
		if err != nil {
			return g.correlationUnreadable("EOB owner", corrID, err)
		}
		if found && owner != subjectPCI {
			return http.StatusConflict, refusalCorrelationTaken
		}
	}
	if l, ok := g.cfg.Store.(PendCorrelationLookup); ok {
		_, found, err := l.PendedForOtherSubject(corrID, subjectPCI)
		if err != nil {
			return g.correlationUnreadable("pended authorization", corrID, err)
		}
		if found {
			return http.StatusConflict, refusalCorrelationTaken
		}
	}
	return 0, ""
}

func (g *Gateway) correlationUnreadable(what, corrID string, err error) (int, string) {
	log.Printf("gateway: pas-claim: correlation %s: %s lookup failed: %v", corrID, what, err)
	g.noteStoreError(storeErrPended)
	return http.StatusBadGateway, refusalHolderReadFailed
}

// reportEOBOwnedElsewhere records, for the operator, a payer decision that
// relayed to the requester but was not recorded because its EOB id was filed
// for another patient first. Metadata only: the leg, the exchange's correlation,
// the authorization's correlation and the reason — never the patient.
func (g *Gateway) reportEOBOwnedElsewhere(leg, op, legCorrID, authCorrID string) {
	log.Printf("gateway: %s decision not recorded: correlation %s: authorization %s: %s", leg, legCorrID, authCorrID, reasonEOBOwnedElsewhere)
	g.observe(ObserverEvent{Kind: pendDecisionNotRecordedEvent, Direction: "ingress",
		LegType: leg, CorrelationID: legCorrID, Op: op,
		Detail: "authorization " + authCorrID + ": " + reasonEOBOwnedElsewhere})
}
