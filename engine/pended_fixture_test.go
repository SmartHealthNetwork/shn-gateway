package engine

import (
	"encoding/json"
	"time"
)

// testPendedResponse is a payer's pended PAS 2.0 response for patientRef: a
// ClaimResponse whose item carries the A4 (pended) review action, and the
// payer's profiled Task asking for one questionnaire, identified by item.
// ParsePendedResponse reads item back as the needed item's code.
func testPendedResponse(patientRef, corr, item string, created time.Time) ([]byte, error) {
	ts := created.UTC().Format(time.RFC3339)
	pas := "http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes"
	coding := func(system, code string) map[string]any {
		return map[string]any{"coding": []any{map[string]any{"system": system, "code": code}}}
	}
	payer := map[string]any{"identifier": map[string]any{"system": "http://hl7.org/fhir/sid/us-npi", "value": "1234567893"}}
	bundle := map[string]any{
		"resourceType": "Bundle", "type": "collection", "timestamp": ts,
		"entry": []any{
			map[string]any{"fullUrl": "https://payer.example/fhir/ClaimResponse/cr-" + corr, "resource": map[string]any{
				"resourceType": "ClaimResponse", "id": "cr-" + corr, "status": "active", "use": "preauthorization",
				"type":    coding("http://terminology.hl7.org/CodeSystem/claim-type", "professional"),
				"patient": map[string]any{"reference": patientRef}, "created": ts,
				"insurer":    map[string]any{"reference": "Organization/payer"},
				"outcome":    "queued",
				"identifier": []any{map[string]any{"system": "urn:shn:correlation", "value": corr}},
				"item": []any{map[string]any{"itemSequence": 1, "adjudication": []any{map[string]any{
					"category": coding("http://terminology.hl7.org/CodeSystem/adjudication", "submitted"),
					"extension": []any{map[string]any{
						"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewAction",
						"extension": []any{map[string]any{
							"url":                  "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewActionCode",
							"valueCodeableConcept": coding("https://codesystem.x12.org/005010/306", "A4"),
						}},
					}},
				}}}},
			}},
			map[string]any{"fullUrl": "https://payer.example/fhir/Task/task-" + corr, "resource": map[string]any{
				"resourceType": "Task", "id": "task-" + corr,
				"meta":       map[string]any{"profile": []any{"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-task|2.0.1"}},
				"identifier": []any{map[string]any{"system": "https://payer.example/pa-request", "value": corr}},
				"status":     "requested", "intent": "order",
				"code":            coding(pas, "attachment-request-questionnaire"),
				"for":             map[string]any{"reference": patientRef},
				"requester":       payer,
				"owner":           payer,
				"reasonCode":      coding(pas, "priorAuthorization"),
				"reasonReference": map[string]any{"reference": "https://provider.example/fhir/Claim/claim-" + corr},
				"input": []any{
					map[string]any{"type": coding(pas, "payer-url"), "valueUrl": "https://payer.example/fhir"},
					map[string]any{
						"extension":       []any{map[string]any{"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-paLineNumber", "valueInteger": 1}},
						"type":            coding(pas, "questionnaires-needed"),
						"valueIdentifier": map[string]any{"system": "https://payer.example/questionnaire", "value": item},
					},
				},
			}},
		},
	}
	return json.Marshal(bundle)
}
