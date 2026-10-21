package engine

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
)

// amendAttempt builds, checks and sends one pas-claim-update under corr, as
// its origination site always has. handled reports that it already answered
// its caller, because a build, a guard or a check stopped the amendment before
// it was sent; otherwise err is OriginateLeg's.
type amendAttempt func(corr string) (bundle, resp []byte, handled bool, err error)

// LegResentEvent is the observer event an origination site emits when it
// re-sends an amendment the payer refused with 409: its CorrelationID is the
// re-send's, and its Detail names the refused attempt's correlation id.
const LegResentEvent = "leg.resent"

// sendAmendment sends a pas-claim-update this gateway originates for its
// participant, playing the requester. The payer may answer the first attempt
// 409 Conflict, FHIR's version conflict: its store refused the amendment
// because the record it updates moved in the meantime (the Da Vinci reference
// payer's pend-resolution timer writing the same ClaimResponse in the same
// instant), and it persisted nothing. Then the amendment is built and sent
// once more under a new correlation id, and the payer's answer to that one is
// what the caller relays; a second 409 is relayed as it came.
//
// Each attempt is its own leg, with its own correlation id and its own Hub
// records; both carry the amendment's evidence Provenance. The re-send's
// LegResentEvent and log line name the refused attempt's correlation id, which
// links the two; the Audit Plane record carries no such link. Only origination
// re-sends: a gateway relaying a requester's amendment never re-sends on the
// requester's behalf.
func (g *Gateway) sendAmendment(recipient string, attempt amendAttempt) (corr string, bundle, resp []byte, handled bool, err error) {
	corr = g.cfg.CorrelationGen()
	bundle, resp, handled, err = attempt(corr)
	var re *RelayError
	if handled || !errors.As(err, &re) || re.Status != http.StatusConflict {
		return corr, bundle, resp, handled, err
	}
	refused := corr
	corr = g.cfg.CorrelationGen()
	log.Printf("gateway: pas-claim-update %s: the payer answered 409 (a version conflict); re-sending the amendment once as %s", refused, corr)
	detail, _ := json.Marshal(struct {
		Refused string `json:"refusedCorrelationId"`
		Status  int    `json:"status"`
	}{refused, re.Status})
	g.observe(ObserverEvent{Kind: LegResentEvent, LegType: "pas-claim-update", Direction: "originate", CorrelationID: corr, Counterpart: recipient, Status: re.Status, Detail: string(detail)})
	bundle, resp, handled, err = attempt(corr)
	return corr, bundle, resp, handled, err
}
