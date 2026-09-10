package engine

import (
	"encoding/json"
	"net/http"
)

// fhirOperationWriter selects the envelope for locally generated JSON answers.
// Raw writes remain untouched: a framed upstream answer is relayed verbatim.
// Only the FHIR operation handlers opt in, leaving CDS Hooks and OAuth alone.
type fhirOperationWriter struct{ http.ResponseWriter }

func (w *fhirOperationWriter) writeJSON(status int, value any) {
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
			value = map[string]any{"resourceType": "OperationOutcome", "issue": []map[string]string{{"severity": "error", "code": code, "diagnostics": message}}}
		}
	}
	w.Header().Set("Content-Type", "application/fhir+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
