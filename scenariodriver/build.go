package scenariodriver

import (
	"bytes"
	"encoding/json"
	"fmt"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

const SystemHCPCS = "http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets"

// ScenarioOrder is the HCPCS code and display for a Prior Authorization scenario (UC-01…08).
type ScenarioOrder struct {
	Code    string
	Display string
}

// PersonaOrders maps each scenario key (noPA|approve|deny|pend) to its persona-selected
// HCPCS code + display. Transcribed from br-provider's order-templates.ts; SELECTION only,
// never invented. br-payer matches its PlanDefinition by code regardless of order type.
var PersonaOrders = map[string]ScenarioOrder{
	"noPA":    {Code: "E0250", Display: "Hospital Bed with Side Rails"},
	"approve": {Code: "L8000", Display: "Breast prosthesis, mastectomy bra"},
	"deny":    {Code: "J3490", Display: "Unclassified drugs"},
	"pend":    {Code: "E0424", Display: "Stationary Oxygen System"},
}

// BuildCRDRequest builds a conformant CDS Hooks order-sign request for the given code on the
// given provider-seeded member. Coverage carries the cms-payer Org (urn:oid:2.16.840.1.113883.6.300|00001)
// br-payer adjudicates against. The ServiceRequest carrier enables SHN's payer-side subject-bind
// extraction; br-payer matches its PlanDefinition by code regardless of order type. hookInstance
// is the fixed opaque token "shn-scenariodriver" (not per-request identity, so runs stay
// deterministic and comparable against goldens); display is omitted from the coding when empty.
func BuildCRDRequest(member, system, code, display string) ([]byte, error) {
	ref := "Patient/" + member
	coding := map[string]any{"system": system, "code": code}
	if display != "" {
		coding["display"] = display
	}
	body := map[string]any{
		"hook":         "order-sign",
		"hookInstance": "shn-scenariodriver",
		"fhirServer":   "https://provider.example/fhir",
		"context": map[string]any{
			"userId":    "Practitioner/p1",
			"patientId": member,
			"draftOrders": map[string]any{
				"resourceType": "Bundle", "type": "collection",
				"entry": []any{map[string]any{
					"fullUrl": "urn:uuid:sr1",
					"resource": map[string]any{
						"resourceType": "ServiceRequest", "id": "sr1", "status": "draft",
						"intent":    "order",
						"code":      map[string]any{"coding": []any{coding}},
						"subject":   map[string]any{"reference": ref},
						"insurance": []any{map[string]any{"reference": "Coverage/c1"}},
					},
				}},
			},
		},
		"prefetch": map[string]any{
			"patient": map[string]any{"resourceType": "Patient", "id": member},
			"coverage": map[string]any{
				"resourceType": "Coverage", "id": "c1", "status": "active",
				"beneficiary": map[string]any{"reference": ref},
				"payor":       []any{map[string]any{"reference": "#cms-payer"}},
				"contained": []any{map[string]any{
					"resourceType": "Organization", "id": "cms-payer",
					"identifier": []any{map[string]any{"system": shnsdk.CMSPayerIdentity.System, "value": shnsdk.CMSPayerIdentity.Value}},
					"name":       "Centers for Medicare and Medicaid Services",
				}},
			},
		},
	}
	return json.Marshal(body)
}

// RebindPASPatient sets the Patient.id STRUCTURALLY (the golden is pretty-printed `"id": "…"` with a
// space, so a raw string-replace would no-op), then string-replaces every Patient/<oldID> reference
// on the freshly-marshaled (spacing-normalized) JSON. This is the deterministic rebind of a committed
// br-payer golden onto a provider-seeded member. br-payer keys its decision on the order code, not
// the patient. Returns error on unparseable JSON or if no Patient resource is found.
func RebindPASPatient(bundleJSON []byte, newID string) ([]byte, error) {
	var b map[string]any
	if err := json.Unmarshal(bundleJSON, &b); err != nil {
		return nil, fmt.Errorf("parse bundle: %w", err)
	}
	entries, _ := b["entry"].([]any)
	oldID := ""
	for _, e := range entries {
		r, _ := e.(map[string]any)["resource"].(map[string]any)
		if r != nil && r["resourceType"] == "Patient" {
			oldID, _ = r["id"].(string)
			r["id"] = newID
		}
	}
	if oldID == "" {
		return nil, fmt.Errorf("bundle has no Patient resource to rebind")
	}
	out, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("marshal rebind: %w", err)
	}
	return bytes.ReplaceAll(out, []byte("Patient/"+oldID), []byte("Patient/"+newID)), nil
}

// AddRoutablePayor adds an inline payor identifier (CMSPayerIdentity, urn:oid:…300|00001) to the
// PAS bundle's Coverage.payor[0] so the PAS ingress can route it off the bundle's Coverage (FR-G40).
// The existing payor REFERENCE (Organization/InsurerExample) is PRESERVED — the identifier is purely
// ADDITIVE and the PAS ingress's ParsePayerIdentifier reads the inline form first. Returns error on
// unparseable JSON or if the bundle has no Coverage entry.
func AddRoutablePayor(bundleJSON []byte) ([]byte, error) {
	var b map[string]any
	if err := json.Unmarshal(bundleJSON, &b); err != nil {
		return nil, fmt.Errorf("parse bundle: %w", err)
	}
	entries, _ := b["entry"].([]any)
	found := false
	for _, e := range entries {
		res, _ := e.(map[string]any)["resource"].(map[string]any)
		if res == nil || res["resourceType"] != "Coverage" {
			continue
		}
		payors, _ := res["payor"].([]any)
		if len(payors) == 0 {
			payors = []any{map[string]any{}}
		}
		p0, _ := payors[0].(map[string]any)
		if p0 == nil {
			p0 = map[string]any{}
		}
		p0["identifier"] = map[string]any{"system": shnsdk.CMSPayerIdentity.System, "value": shnsdk.CMSPayerIdentity.Value}
		payors[0] = p0
		res["payor"] = payors
		found = true
	}
	if !found {
		return nil, fmt.Errorf("bundle has no Coverage to make routable")
	}
	out, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	return out, nil
}

// BuildPASBundle loads a committed br-payer $submit golden, rebinds it onto member, and makes its
// Coverage routable (AddRoutablePayor) so the PAS ingress can derive the payer holder from the
// bundle's Coverage (FR-G40).
func BuildPASBundle(golden []byte, member string) ([]byte, error) {
	rebound, err := RebindPASPatient(golden, member)
	if err != nil {
		return nil, err
	}
	return AddRoutablePayor(rebound)
}

// InjectShnCorrelation surgically adds a Claim.identifier entry {system:"urn:shn:correlation",
// value:corr} to a PAS Bundle JSON via one-pass map edit. Deterministic. Enables the submit→amend
// correlation handoff: SHN keys the pend on the partner-supplied corr so the follow-up amended
// re-POST can reference it via Claim.related[prior].
func InjectShnCorrelation(bundleJSON []byte, corr string) ([]byte, error) {
	var b map[string]any
	if err := json.Unmarshal(bundleJSON, &b); err != nil {
		return nil, fmt.Errorf("parse bundle: %w", err)
	}
	entries, _ := b["entry"].([]any)
	for _, e := range entries {
		res, _ := e.(map[string]any)["resource"].(map[string]any)
		if res != nil && res["resourceType"] == "Claim" {
			existing, _ := res["identifier"].([]any)
			res["identifier"] = append(existing, map[string]any{
				"system": "urn:shn:correlation",
				"value":  corr,
			})
			break
		}
	}
	out, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	return out, nil
}

// BuildQuestionnairePackageRequest builds a conformant DTR $questionnaire-package input Parameters
// with the contained-payor Coverage + valueCanonical. The Parameters is consumed by the DTR ingress
// to fetch a CRD card's questionnaire from the native payer endpoint.
func BuildQuestionnairePackageRequest(canonical, member string) ([]byte, error) {
	params := map[string]any{
		"resourceType": "Parameters",
		"meta": map[string]any{
			"profile": []any{"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/dtr-qpackage-input-parameters"},
		},
		"parameter": []any{
			map[string]any{
				"name": "coverage",
				"resource": map[string]any{
					"resourceType": "Coverage", "id": "coverage-1", "status": "active",
					"beneficiary": map[string]any{"reference": "Patient/" + member},
					"payor":       []any{map[string]any{"reference": "#payor-org"}},
					"contained": []any{map[string]any{
						"resourceType": "Organization", "id": "payor-org", "active": true,
						"identifier": []any{map[string]any{"system": shnsdk.CMSPayerIdentity.System, "value": shnsdk.CMSPayerIdentity.Value}},
						"name":       "Centers for Medicare and Medicaid Services",
					}},
				},
			},
			map[string]any{"name": "questionnaire", "valueCanonical": canonical},
		},
	}
	return json.Marshal(params)
}
