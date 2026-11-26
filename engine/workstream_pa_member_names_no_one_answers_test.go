//go:build !shn_coreonly

package engine

import (
	"net/http"
	"testing"
)

// namesNoOneAnswer is a route's answer to a patient reference naming no one:
// an empty field is the route's own missing patient (its own text), and any
// other reference that names no one ("Patient/", "Patient/ ",
// "Patient/Patient/") is refused 400 msgMemberNamesNoOne, but where the
// route's own reading answers first: eligibility's own text on its legs. A PAS
// bundle's Claim.patient that names no one is refused so at every level,
// before the patient-mixed check. An order whose subject
// names no one is the patient-mixed rule's (carried below strict). A zero
// status is a request carried.
type namesNoOneAnswer struct {
	status int
	msg    string
}

func namesNoOneWant(route, ref string, level ConformanceEnforcement) (namesNoOneAnswer, bool) {
	strict := level == EnforcementStrict
	none := namesNoOneAnswer{http.StatusBadRequest, msgMemberNamesNoOne}
	switch route {
	case "provider eligibility", "payer eligibility/records", "payer eligibility/forward":
		return namesNoOneAnswer{http.StatusBadRequest, errEligibilityNamesNoOne}, true
	case "CRD ingress, patientId":
		return none, ref != ""
	case "CRD dispatch, patientId":
		if ref == "" {
			return namesNoOneAnswer{http.StatusBadRequest, "missing context.patientId"}, true
		}
		return none, true
	case "CRD ingress, an order's subject", "CRD dispatch, an order's subject":
		if !strict {
			return namesNoOneAnswer{}, true
		}
		switch {
		case route == "CRD ingress, an order's subject" && ref == "":
			return namesNoOneAnswer{http.StatusForbidden, "draft order missing patient subject"}, true
		case route == "CRD ingress, an order's subject":
			return namesNoOneAnswer{http.StatusForbidden, "inconsistent patient reference in ingress payload"}, true
		case ref == "":
			return namesNoOneAnswer{http.StatusForbidden, "dispatched order missing patient subject"}, true
		default:
			return namesNoOneAnswer{http.StatusForbidden, "inconsistent patient in order-dispatch"}, true
		}
	case "PAS ingress":
		switch {
		case ref == "":
			return namesNoOneAnswer{http.StatusBadRequest, "PAS bundle missing Claim.patient"}, true
		}
		return none, true
	case "FQS ingress":
		if ref == "" {
			return namesNoOneAnswer{http.StatusBadRequest, "federated query rejected: cdex: Task.for missing/invalid"}, true
		}
		return none, true
	case "FQS inbound":
		if ref == "" {
			return namesNoOneAnswer{http.StatusForbidden, "query rejected: cdex: Task.for missing/invalid"}, true
		}
		return none, true
	case "patient-DTR":
		if ref == "" {
			return namesNoOneAnswer{http.StatusBadRequest, refusalPatientDTRNoPatient}, true
		}
		return none, true
	}
	return namesNoOneAnswer{}, false
}

// wantNamesNoOneAnswer holds a route's answer (its status, and its text: a
// bare message, or the "error" or OperationOutcome diagnostics of a body) to
// the one namesNoOneWant names, at every level.
func wantNamesNoOneAnswer(t *testing.T, route, ref string, level ConformanceEnforcement, status int, body string) {
	t.Helper()
	want, ok := namesNoOneWant(route, ref, level)
	if !ok {
		t.Fatalf("no answer named for route %q, reference %q", route, ref)
	}
	if got := answerText(body); status != want.status || got != want.msg {
		t.Fatalf("answer %d %q; want %d %q", status, got, want.status, want.msg)
	}
}
