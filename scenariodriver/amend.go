package scenariodriver

import (
	"encoding/json"
	"fmt"
)

// BuildAmendedRePOST builds a conformant amended re-POST bundle from the home-oxygen pended
// golden (order code E0424 unchanged from the submit, no order-shaping). The bundle carries:
//   - Claim.identifier[urn:shn:correlation] = amendCorr (the amend's own correlation)
//   - Claim.related[0].claim: both reference="Claim/<priorID>" (for br-payer's bundle-inclusion
//     validation per Da Vinci PAS) and identifier[urn:shn:correlation]=submitCorr (for SHN's
//     parseConformantPASUpdateFacts to key BeginClaimUpdate on the partner-supplied corr)
//   - The prior Claim resource (a clone of the golden Claim with id="…-prior") physically
//     included in the bundle (br-payer PAS bundle validator requires the prior Claim be included)
//   - A minimal QuestionnaireResponse (subject→Patient/member) as supplemental data (FR-32)
//   - A Provenance targeting the QR (agent→Organization/provider) (FR-32 QR-variant)
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
	const priorClaimID = "home-oxygen-therapy-claim-prior"

	// Clone the original Claim as the "prior" resource (br-payer requires the prior Claim to be
	// physically included in the amendment bundle). The clone gets a -prior id suffix so the bundle
	// contains two distinct Claim entries without id collision.
	var priorClaimResource map[string]any
	var priorClaimFullURL string
	for _, e := range entries {
		emap, _ := e.(map[string]any)
		res, _ := emap["resource"].(map[string]any)
		if res != nil && res["resourceType"] == "Claim" {
			// Deep-copy via JSON round-trip.
			raw2, _ := json.Marshal(res)
			var clone map[string]any
			_ = json.Unmarshal(raw2, &clone)
			clone["id"] = priorClaimID
			priorClaimResource = clone
			priorClaimFullURL = "http://example.org/fhir/Claim/" + priorClaimID
			break
		}
	}
	if priorClaimResource == nil {
		return nil, fmt.Errorf("golden has no Claim resource to clone as prior")
	}

	// Mutate the amend Claim:
	//   - add amendCorr as own urn:shn:correlation (SHN ingress corr-threading for the amend leg)
	//   - set related[prior] with BOTH reference (br-payer bundle-inclusion check) and identifier
	//     (SHN parseConformantPASUpdateFacts reads identifier.value for BeginClaimUpdate lookup)
	//   - add a relationship coding (Da Vinci PAS convention)
	for _, e := range entries {
		emap, _ := e.(map[string]any)
		res, _ := emap["resource"].(map[string]any)
		if res != nil && res["resourceType"] == "Claim" {
			existing, _ := res["identifier"].([]any)
			res["identifier"] = append(existing, map[string]any{
				"system": "urn:shn:correlation",
				"value":  amendCorr,
			})
			res["related"] = []any{map[string]any{
				"relationship": map[string]any{
					"coding": []any{map[string]any{
						"system": "http://terminology.hl7.org/CodeSystem/ex-relatedclaimrelationship",
						"code":   "prior",
					}},
				},
				"claim": map[string]any{
					// reference: satisfies br-payer's "prior Claim must be included in Bundle" check.
					"reference": "Claim/" + priorClaimID,
					// identifier: SHN's parseConformantPASUpdateFacts reads this value for BeginClaimUpdate.
					"identifier": map[string]any{
						"system": "urn:shn:correlation",
						"value":  submitCorr,
					},
				},
			}}
			break
		}
	}

	// Include the prior Claim resource in the bundle (required by br-payer).
	priorClaimEntry := map[string]any{
		"fullUrl":  priorClaimFullURL,
		"resource": priorClaimResource,
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
	// Inject a Provenance targeting the QR (agent→Organization/provider). FR-32 (QR-variant):
	// qrID != "" && hasDR=false → wantTarget="QuestionnaireResponse/"+qrID.
	// fullUrl required by the Da Vinci PAS Bundle spec (br-payer validates it).
	const provID = "amend-prov-1"
	provEntry := map[string]any{
		"fullUrl": "http://example.org/fhir/Provenance/" + provID,
		"resource": map[string]any{
			"resourceType": "Provenance",
			"id":           provID,
			"target":       []any{map[string]any{"reference": "QuestionnaireResponse/" + qrID}},
			"agent":        []any{map[string]any{"who": map[string]any{"reference": "Organization/provider"}}},
		},
	}
	b["entry"] = append(entries, priorClaimEntry, qrEntry, provEntry)

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

// BuildFederatedAmendedRePOST builds a conformant amended re-POST from the home-oxygen pended
// golden (order code E0424 unchanged from the submit, persona-selection-only, no order-shaping),
// carrying the CDex-middle evidence: the federated DiagnosticReport + a Provenance targeting it
// (FR-32 DR-variant). This mirrors BuildAmendedRePOST but swaps the QR-variant supplemental for
// the DiagnosticReport-variant — the federated evidence UC-05 retrieves, which the orchestration
// carries onto the ClaimUpdate. br-payer requires the prior Claim to be physically included.
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
	const priorClaimID = "home-oxygen-therapy-claim-prior"
	const drID = "dr-uc05-operative" // the facility's operative DiagnosticReport id
	const provID = "uc05-prov-1"     // Provenance targeting the federated DR (FR-32 DR-variant)

	// Clone the original Claim as the "prior" resource (br-payer requires the prior Claim to be
	// physically included in the amendment bundle), suffixing the id to avoid a collision.
	var priorClaimResource map[string]any
	var priorClaimFullURL string
	for _, e := range entries {
		res, _ := e.(map[string]any)["resource"].(map[string]any)
		if res != nil && res["resourceType"] == "Claim" {
			raw, _ := json.Marshal(res)
			var clone map[string]any
			_ = json.Unmarshal(raw, &clone)
			clone["id"] = priorClaimID
			priorClaimResource = clone
			priorClaimFullURL = "http://example.org/fhir/Claim/" + priorClaimID
			break
		}
	}
	if priorClaimResource == nil {
		return nil, fmt.Errorf("golden has no Claim resource to clone as prior")
	}

	// Mutate the amend Claim: own urn:shn:correlation=amendCorr + related[prior] carrying BOTH a
	// reference (br-payer's bundle-inclusion check) and identifier (SHN's BeginClaimUpdate lookup).
	for _, e := range entries {
		res, _ := e.(map[string]any)["resource"].(map[string]any)
		if res != nil && res["resourceType"] == "Claim" {
			existing, _ := res["identifier"].([]any)
			res["identifier"] = append(existing, map[string]any{
				"system": "urn:shn:correlation", "value": amendCorr,
			})
			res["related"] = []any{map[string]any{
				"relationship": map[string]any{
					"coding": []any{map[string]any{
						"system": "http://terminology.hl7.org/CodeSystem/ex-relatedclaimrelationship",
						"code":   "prior",
					}},
				},
				"claim": map[string]any{
					"reference":  "Claim/" + priorClaimID,
					"identifier": map[string]any{"system": "urn:shn:correlation", "value": submitCorr},
				},
			}}
			break
		}
	}

	priorClaimEntry := map[string]any{"fullUrl": priorClaimFullURL, "resource": priorClaimResource}

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
	provEntry := map[string]any{
		"fullUrl": "http://example.org/fhir/Provenance/" + provID,
		"resource": map[string]any{
			"resourceType": "Provenance",
			"id":           provID,
			"target":       []any{map[string]any{"reference": "DiagnosticReport/" + drID}},
			"agent":        []any{map[string]any{"who": map[string]any{"reference": "Organization/provider"}}},
		},
	}

	b["entry"] = append(entries, priorClaimEntry, drEntry, provEntry)

	out, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	return out, nil
}
