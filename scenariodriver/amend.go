package scenariodriver

import (
	"encoding/json"
	"fmt"
)

// BuildAmendedRePOST builds a conformant PAS 2.0.1 amended re-POST bundle from the home-oxygen
// pended golden (order code E0424 unchanged from the submit, no order-shaping). The bundle
// carries one Claim, the update (asPAS201Update):
//   - Claim.identifier[urn:shn:correlation] = amendCorr (the amend's own correlation)
//   - Claim.related[0].claim.identifier[urn:shn:correlation] = submitCorr: the prior, named by
//     its identifier alone. PAS 2.0.1's request Bundle allows one Claim (entry:Claim 1..1), so
//     the prior is not carried; the reference payer looks it up in its own records
//   - meta.profile profile-claim-update, the profile a PAS 2.0.1 update Claim is
//   - A minimal QuestionnaireResponse (subject→Patient/member) as supplemental data (FR-32)
//   - A Provenance targeting the QR (agent identified by provider holder identity) (FR-32 QR-variant)
//
// The home-oxygen golden has NO QR and NO DiagnosticReport. We inject a minimal QR because
// parseConformantPASSubjects tolerates a QR-less conformant bundle (R-5: optional), but the
// FR-32 gate on the payer side requires a Provenance whose target matches the supplemental QR
// or DR. Using a QR (not DR) so the QR-variant path fires.
func BuildAmendedRePOST(member, submitCorr, amendCorr string) ([]byte, error) {
	// Load and rebind the home-oxygen golden (same order as the submit — no order-shaping).
	raw := PASPendGolden()
	bundle, err := RebindPASPatient(raw, member)
	if err != nil {
		return nil, fmt.Errorf("rebind patient: %w", err)
	}

	var b map[string]any
	if err := json.Unmarshal(bundle, &b); err != nil {
		return nil, fmt.Errorf("parse bundle: %w", err)
	}
	entries, _ := b["entry"].([]any)
	const qrID = "amend-qr-1"
	if err := asPAS201Update(entries, submitCorr, amendCorr); err != nil {
		return nil, err
	}

	// Inject a minimal QuestionnaireResponse (subject→member) as the supplemental resource.
	// FR-32: the Provenance must target it by id. The home-oxygen golden has no QR entry.
	// fullUrl is required by the Da Vinci PAS Bundle spec (br-payer validates it).
	qrEntry := map[string]any{
		"fullUrl": "http://example.org/fhir/QuestionnaireResponse/" + qrID,
		"resource": map[string]any{
			"resourceType": "QuestionnaireResponse",
			"id":           qrID,
			"status":       "completed",
			"subject":      map[string]any{"reference": "Patient/" + member},
		},
	}
	// Inject a Provenance targeting the QR (agent identified by provider holder identity). FR-32 (QR-variant):
	// qrID != "" && hasDR=false → wantTarget="QuestionnaireResponse/"+qrID.
	// fullUrl required by the Da Vinci PAS Bundle spec (br-payer validates it).
	const provID = "amend-prov-1"
	recorded, err := bundleTimestamp(b)
	if err != nil {
		return nil, err
	}
	provEntry := map[string]any{
		"fullUrl": "http://example.org/fhir/Provenance/" + provID,
		"resource": map[string]any{
			"resourceType": "Provenance",
			"id":           provID,
			"target":       []any{map[string]any{"reference": "QuestionnaireResponse/" + qrID}},
			"recorded":     recorded,
			"agent":        []any{map[string]any{"who": map[string]any{"identifier": map[string]any{"system": "http://smarthealth.network/ids/holder", "value": "provider"}}}},
		},
	}
	b["entry"] = append(entries, qrEntry, provEntry)

	out, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	// Make the amend bundle's Coverage routable (inline CMSPayerIdentity|00001) exactly like the
	// submit path (BuildPASBundle) — the PAS ingress now derives the payer HOLDER from the
	// inbound bundle's Coverage (FR-G40; the PAYER_DIRECTORY maps 00001 → the payer), so an
	// amend bundle whose Coverage carries no parseable payer would fail closed 422 before relay.
	return AddRoutablePayor(out)
}

// BuildFederatedAmendedRePOST builds a conformant PAS 2.0.1 amended re-POST from the home-oxygen
// pended golden (order code E0424 unchanged from the submit, persona-selection-only, no
// order-shaping), carrying the CDex-middle evidence: the federated DiagnosticReport + a
// Provenance targeting it (FR-32 DR-variant). This mirrors BuildAmendedRePOST, one Claim naming
// its prior by identifier (asPAS201Update), but swaps the QR-variant supplemental for the
// DiagnosticReport-variant — the federated evidence UC-05 retrieves, which the orchestration
// carries onto the ClaimUpdate.
func BuildFederatedAmendedRePOST(member, submitCorr, amendCorr string, drJSON []byte) ([]byte, error) {
	// Start from the pend golden: rebind + routable payor FIRST (preserving pasBundleFromGolden order).
	rebound, err := RebindPASPatient(PASPendGolden(), member)
	if err != nil {
		return nil, fmt.Errorf("rebind patient: %w", err)
	}
	bundle, err := AddRoutablePayor(rebound)
	if err != nil {
		return nil, fmt.Errorf("add routable payor: %w", err)
	}

	var b map[string]any
	if err := json.Unmarshal(bundle, &b); err != nil {
		return nil, fmt.Errorf("parse bundle: %w", err)
	}
	entries, _ := b["entry"].([]any)
	const drID = "dr-uc05-operative" // the facility's operative DiagnosticReport id
	const provID = "uc05-prov-1"     // Provenance targeting the federated DR (FR-32 DR-variant)
	if err := asPAS201Update(entries, submitCorr, amendCorr); err != nil {
		return nil, err
	}

	// The federated DiagnosticReport (rebound onto the submit's member so the FR-32 subject-bind
	// holds — br-payer keys on the order code, the bundle's Patient is member). Its id is
	// preserved so the Provenance can target DiagnosticReport/<drID> (the FR-32 DR-variant gate).
	var drMap map[string]any
	if err := json.Unmarshal(drJSON, &drMap); err != nil {
		return nil, fmt.Errorf("parse federated DiagnosticReport: %w", err)
	}
	drMap["id"] = drID
	drMap["subject"] = map[string]any{"reference": "Patient/" + member}
	drEntry := map[string]any{
		"fullUrl":  "http://example.org/fhir/DiagnosticReport/" + drID,
		"resource": drMap,
	}

	// Provenance targeting the federated DiagnosticReport, agent → the provider (FR-32 DR-variant:
	// hasDR=true → wantTarget = "DiagnosticReport/"+drID).
	recorded, err := bundleTimestamp(b)
	if err != nil {
		return nil, err
	}
	provEntry := map[string]any{
		"fullUrl": "http://example.org/fhir/Provenance/" + provID,
		"resource": map[string]any{
			"resourceType": "Provenance",
			"id":           provID,
			"target":       []any{map[string]any{"reference": "DiagnosticReport/" + drID}},
			"recorded":     recorded,
			"agent":        []any{map[string]any{"who": map[string]any{"identifier": map[string]any{"system": "http://smarthealth.network/ids/holder", "value": "provider"}}}},
		},
	}

	b["entry"] = append(entries, drEntry, provEntry)

	out, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	return out, nil
}

// pasProfileClaimUpdate is the profile a PAS 2.0.1 update Claim declares. The golden's Claim
// declares profile-claim, the submission's profile.
const pasProfileClaimUpdate = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim-update"

// asPAS201Update turns the golden's one Claim into the PAS 2.0.1 update of the submission
// submitCorr names: its one identifier is amendCorr (a PAS 2.0.1 Claim carries exactly one,
// replacing the golden's); it declares profile-claim-update; and related[0] names the prior by
// identifier alone (relationship prior), the identifier the submission carried
// (InjectShnCorrelation) and the one parseConformantPASUpdateFacts keys the update on. The prior
// itself is not added: PAS 2.0.1's request Bundle allows one Claim.
func asPAS201Update(entries []any, submitCorr, amendCorr string) error {
	for _, e := range entries {
		res, _ := e.(map[string]any)["resource"].(map[string]any)
		if res == nil || res["resourceType"] != "Claim" {
			continue
		}
		res["identifier"] = []any{map[string]any{"system": "urn:shn:correlation", "value": amendCorr}}
		res["meta"] = map[string]any{"profile": []any{pasProfileClaimUpdate}}
		res["related"] = []any{map[string]any{
			"relationship": map[string]any{
				"coding": []any{map[string]any{
					"system": "http://terminology.hl7.org/CodeSystem/ex-relatedclaimrelationship",
					"code":   "prior",
				}},
			},
			"claim": map[string]any{
				"identifier": map[string]any{"system": "urn:shn:correlation", "value": submitCorr},
			},
		}}
		return nil
	}
	return fmt.Errorf("golden has no Claim resource to make the update")
}

// bundleTimestamp returns the Bundle's own timestamp, the instant an amendment's
// Provenance records (Provenance.recorded is 1..1 in base R4): read from the golden
// the Bundle is built from, so the driver's Bundles stay deterministic.
func bundleTimestamp(b map[string]any) (string, error) {
	ts, _ := b["timestamp"].(string)
	if ts == "" {
		return "", fmt.Errorf("the golden Bundle carries no timestamp for its Provenance to record")
	}
	return ts, nil
}
