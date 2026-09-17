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
			}
			return map[string]any{"resourceType": "OperationOutcome", "issue": []map[string]string{{"severity": "error", "code": code, "diagnostics": message}}}
		}
	}
	return value
}
