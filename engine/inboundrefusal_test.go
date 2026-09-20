package engine

import (
	"net/http"
	"testing"
)

// TestIsApplicationRefusal pins the line between a handler's answer about the
// request (framed, 200 to the Hub) and the gateway's own fault (raw): after
// authentication, every 4xx a handler writes is the participant's verdict —
// whatever its text — and every 5xx is machinery.
func TestIsApplicationRefusal(t *testing.T) {
	cases := []struct {
		status int
		msg    string
		answer bool
	}{
		// The participant's verdict about the request it was sent.
		{http.StatusBadRequest, refusalUnknownMember, true},
		{http.StatusBadRequest, "no order (ServiceRequest or DeviceRequest) in draftOrders", true},
		{http.StatusBadRequest, "parse cds request failed", true},
		{http.StatusBadRequest, "parse member failed", true},
		{http.StatusBadRequest, "inconsistent patient in order-select", true},
		{http.StatusBadRequest, "PAS bundle missing Claim.patient", true},
		{http.StatusForbidden, "token subject does not match request patient", true},
		{http.StatusForbidden, "federated query missing consent reference", true},
		{http.StatusForbidden, "is clinician-sourced", true},
		{http.StatusNotFound, "unknown questionnaire canonical", true},
		{http.StatusUnprocessableEntity, refusalIngressValidation, true},
		{http.StatusUnprocessableEntity, refusalIngressValidation + ": Bundle entry missing fullUrl; two more", true},
		{http.StatusConflict, "version conflict", true},
		// The gateway's own faults.
		{http.StatusInternalServerError, "validator unavailable", false},
		{http.StatusInternalServerError, errOwnershipFault, false},
		{http.StatusBadGateway, "holder write failed", false},
		{http.StatusServiceUnavailable, "consent service unavailable", false},
		// Not refusals at all.
		{http.StatusOK, "", false},
		{http.StatusFound, "", false},
		{0, "", false},
	}
	for _, c := range cases {
		if got := isApplicationRefusal(c.status, c.msg); got != c.answer {
			t.Errorf("isApplicationRefusal(%d, %q) = %v, want %v", c.status, c.msg, got, c.answer)
		}
	}
}
