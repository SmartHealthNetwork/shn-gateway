package engine

import (
	"net/http"
)

// fhirOperationWriter selects the envelope for locally generated JSON answers.
// Raw writes remain untouched: a framed upstream answer is relayed verbatim.
// Only the FHIR operation handlers opt in, leaving CDS Hooks and OAuth alone.
type fhirOperationWriter struct{ http.ResponseWriter }

// Unwrap exposes the underlying writer (http.ResponseController, scopeOf).
func (w *fhirOperationWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// fhirOperationValue is the value a FHIR operation route answers for a
// locally generated JSON answer: an {"error": …} refusal becomes an
// OperationOutcome; anything else is unchanged.
func fhirOperationValue(status int, value any) any {
	if failure, ok := value.(map[string]string); ok && status >= 400 {
		if message, exists := failure["error"]; exists {
			return refusalOutcome(status, message, nil)
		}
	}
	return value
}

// refusalOutcome is this gateway's own refusal as an OperationOutcome: one
// error issue whose code follows the status and whose diagnostics is the
// refusal's message, then one invalid issue for each validator issue the
// refusal echoes, in order. The same body answers a client on a FHIR
// operation route (fhirOperationValue) and a requester on a leg whose answer
// envelope is FHIR (respondLegError), so a client reads one shape whichever
// end refused.
func refusalOutcome(status int, message string, echoed []string) map[string]any {
	code := "processing"
	switch status {
	case http.StatusBadRequest:
		code = "invalid"
	case http.StatusUnauthorized:
		code = "login"
	case http.StatusForbidden:
		code = "forbidden"
	case http.StatusUnprocessableEntity:
		code = "not-supported"
	case http.StatusServiceUnavailable:
		code = "transient"
	case http.StatusGatewayTimeout:
		code = "timeout"
	}
	issues := []map[string]string{{"severity": "error", "code": code, "diagnostics": message}}
	for _, e := range echoed {
		issues = append(issues, map[string]string{"severity": "error", "code": "invalid", "diagnostics": e})
	}
	return map[string]any{"resourceType": "OperationOutcome", "issue": issues}
}
