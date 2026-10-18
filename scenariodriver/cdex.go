package scenariodriver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// FacilityCDexEvidence builds the facility CDex Data Source pipeline using the same shnsdk
// calls (BuildDocumentReference / BuildProvenanceWithIdentifier →
// BuildRecordsBundle → BuildCDexQueryResult → ExtractCDexEvidence) to produce the federated
// evidence that UC-05 retrieves and the orchestration carries onto the amended ClaimUpdate
// (the CDex middle is bracketed by SHN gateways, not real external CDex actors).
// Returns the extracted DiagnosticReport + Provenance with the given now timestamp.
func FacilityCDexEvidence(member string, now time.Time) (drJSON, provJSON []byte, err error) {
	patientRef := "Patient/" + member

	// The two named records the facility holds for the member.
	dr, err := facilityOperativeReport("dr-uc05-operative", patientRef, "72148",
		"Operative report — lumbar microdiscectomy")
	if err != nil {
		return nil, nil, fmt.Errorf("build facility DiagnosticReport: %w", err)
	}
	docref, err := shnsdk.BuildDocumentReference("docref-uc05-operative", patientRef,
		"DiagnosticReport/dr-uc05-operative")
	if err != nil {
		return nil, nil, fmt.Errorf("build facility DocumentReference: %w", err)
	}

	// Build the CDex request Task the originator would send for the DiagnosticReport leg, so the
	// completed-Task wrapper transitions a real request (BuildCDexQueryResult requires a request Task).
	reqMeta := shnsdk.CDexTaskMeta{AuthoredOn: now, Requester: "provider", Owner: "metro-spine"}
	queryJSON, err := shnsdk.BuildCDexTaskDataRequest(patientRef, "DiagnosticReport",
		"2024-01-01", now.Format("2006-01-02"), reqMeta)
	if err != nil {
		return nil, nil, fmt.Errorf("build CDex Task Data Request: %w", err)
	}

	// Source Provenance: targets the disclosed DiagnosticReport, agent = the facility, .policy
	// cites a consent ref, reason = TREAT. The known holder identity is logical,
	// matching the facility responder; no Organization resource is invented.
	prov, err := shnsdk.BuildProvenanceWithIdentifier("DiagnosticReport/dr-uc05-operative",
		shnsdk.ProvenanceIdentifier{System: "http://smarthealth.network/ids/holder", Value: "metro-spine"}, now)
	if err != nil {
		return nil, nil, fmt.Errorf("build facility Provenance: %w", err)
	}

	// Policy and purpose remain the original CDex disclosure context (FR-32).
	var source map[string]json.RawMessage
	if err = json.Unmarshal(prov, &source); err != nil {
		return nil, nil, err
	}
	source["policy"], _ = json.Marshal([]string{"Consent/uc05-treat"})
	source["reason"], _ = json.Marshal([]any{map[string]any{"coding": []any{map[string]string{
		"system": "http://terminology.hl7.org/CodeSystem/v3-ActReason", "code": shnsdk.PurposeTreatment,
	}}}})
	prov, err = json.Marshal(source)
	if err != nil {
		return nil, nil, err
	}

	inner, err := shnsdk.BuildRecordsBundle([][]byte{dr, docref, prov})
	if err != nil {
		return nil, nil, fmt.Errorf("build records bundle: %w", err)
	}
	completedTask, err := shnsdk.BuildCDexQueryResult(queryJSON, inner)
	if err != nil {
		return nil, nil, fmt.Errorf("build CDex query result: %w", err)
	}

	// Pull the evidence back out the way the originating engine does (engine/originate.go).
	drOut, provOut, err := shnsdk.ExtractCDexEvidence(completedTask)
	if err != nil {
		return nil, nil, fmt.Errorf("extract CDex evidence: %w", err)
	}
	if !bytes.Contains(drOut, []byte("dr-uc05-operative")) {
		return nil, nil, fmt.Errorf("extracted DiagnosticReport is not the facility's operative report")
	}
	if !bytes.Contains(provOut, []byte("DiagnosticReport/dr-uc05-operative")) {
		return nil, nil, fmt.Errorf("extracted Provenance does not target the operative DiagnosticReport")
	}
	return drOut, provOut, nil
}

// facilityOperativeReport is the operative report the simulated facility holds for patientRef, the record its CDex Data Source discloses: its date, category and code are that synthetic record's own, stated
// here (the SDK builds no report). Its bytes are the US Core DiagnosticReport Note form the
// network's other synthetic records take: final, Radiology (v2-0074 RAD), coded with the
// order's CPT code, dated 2026-05-15.
func facilityOperativeReport(id, patientRef, cpt, display string) ([]byte, error) {
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
