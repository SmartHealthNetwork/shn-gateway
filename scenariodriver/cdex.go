package scenariodriver

import (
	"bytes"
	"fmt"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// FacilityCDexEvidence builds the facility CDex Data Source pipeline using the same shnsdk
// calls (BuildDiagnosticReport / BuildDocumentReference / BuildProvenanceWithPolicy →
// BuildRecordsBundle → BuildCDexQueryResult → ExtractCDexEvidence) to produce the federated
// evidence that UC-05 retrieves and the orchestration carries onto the amended ClaimUpdate
// (CXL-D11: CDex middle bracketed by SHN gateways, not real external CDex actors).
// Returns the extracted DiagnosticReport + Provenance with the given now timestamp.
func FacilityCDexEvidence(member string, now time.Time) (drJSON, provJSON []byte, err error) {
	patientRef := "Patient/" + member

	// The two named records the facility holds for the member (mirrors StubHolderData.FacilityRecords).
	dr, err := shnsdk.BuildDiagnosticReport("dr-uc05-operative", patientRef, "72148",
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
	// cites a consent ref, reason = TREAT — exactly the inbound.go's BuildProvenanceWithPolicy call.
	prov, err := shnsdk.BuildProvenanceWithPolicy("DiagnosticReport/dr-uc05-operative",
		"Organization/metro-spine", "Consent/uc05-treat", shnsdk.PurposeTreatment, now)
	if err != nil {
		return nil, nil, fmt.Errorf("build facility Provenance: %w", err)
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
