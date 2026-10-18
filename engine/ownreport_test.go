package engine

import "encoding/json"

// testOwnReport is a DiagnosticReport as a test participant's own synthetic system holds it: its date, category and code are that synthetic record's own, stated
// here (the SDK builds no report). Its bytes are the US Core DiagnosticReport Note form the
// network's other synthetic records take: final, Radiology (v2-0074 RAD), coded with the
// order's CPT code, dated 2026-05-15.
func testOwnReport(id, patientRef, cpt, display string) ([]byte, error) {
	type coding struct {
		System  string `json:"system"`
		Code    string `json:"code"`
		Display string `json:"display"`
	}
	type concept struct {
		Coding []coding `json:"coding"`
	}
	return json.Marshal(struct {
		ID   string `json:"id"`
		Meta struct {
			Profile []string `json:"profile"`
		} `json:"meta"`
		Status   string    `json:"status"`
		Category []concept `json:"category"`
		Code     concept   `json:"code"`
		Subject  struct {
			Reference string `json:"reference"`
		} `json:"subject"`
		EffectiveDateTime string `json:"effectiveDateTime"`
		ResourceType      string `json:"resourceType"`
	}{
		ResourceType: "DiagnosticReport",
		ID:           id,
		Meta: struct {
			Profile []string `json:"profile"`
		}{Profile: []string{"http://hl7.org/fhir/us/core/StructureDefinition/us-core-diagnosticreport-note"}},
		Status:   "final",
		Category: []concept{{Coding: []coding{{System: "http://terminology.hl7.org/CodeSystem/v2-0074", Code: "RAD", Display: "Radiology"}}}},
		Code:     concept{Coding: []coding{{System: "http://www.ama-assn.org/go/cpt", Code: cpt, Display: display}}},
		Subject: struct {
			Reference string `json:"reference"`
		}{Reference: patientRef},
		EffectiveDateTime: "2026-05-15",
	})
}
