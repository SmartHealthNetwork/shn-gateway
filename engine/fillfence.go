package engine

import (
	"errors"
	"net/http"
)

// The fill fence's refusals: this gateway never inserts, into a request it
// carries, a record it read that is not the bound patient's own. That is SHN's
// own edit to the message, so it refuses at every level.
const (
	fillFencedBinary       = "system of record returned a Binary resource"
	fillFencedOtherPatient = "system of record returned another patient's resource"
)

// fillFenceRefusal is the refusal for a record the fill fence refused.
func fillFenceRefusal(err error) (int, string) {
	var ce *CompartmentError
	if errors.As(err, &ce) && ce.Reason == opaqueContentReason {
		return http.StatusBadGateway, fillFencedBinary
	}
	return http.StatusBadGateway, fillFencedOtherPatient
}
