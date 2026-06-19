// fhirread.go — self-contained json-level FHIR subject readers for the (C)
// outbound fence (fenceResponseSubject). These were originally added to the
// LOCAL sdk/ module but the gateway's standalone Docker build resolves
// shn-sdk from the published v0.7.0 proxy (no go.work), which does not
// carry them. Relocating them here makes the gateway closure complete
// against shn-sdk v0.7.0 (Ruling: no SDK release this slice). They promote
// to shn-sdk with the LegResponder promotion.
package engine

import (
	"encoding/json"
	"fmt"
)

// ParseCoverageEligibilityResponsePatient extracts patient.reference from a
// CoverageEligibilityResponse JSON (e.g. "Patient/MBR-COVERED"). It errors if
// the resourceType is not CoverageEligibilityResponse or the patient reference
// is absent. Used by the (C) outbound fence (coverage-eligibility leg): the
// engine compares the returned ref to the inbound member-namespace ref so a
// connector cannot swap the patient between the request it was handed and the
// response it returned. Exported because test/adversarial uses it directly.
func ParseCoverageEligibilityResponsePatient(data []byte) (string, error) {
	var probe struct {
		ResourceType string `json:"resourceType"`
		Patient      struct {
			Reference string `json:"reference"`
		} `json:"patient"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return "", err
	}
	if probe.ResourceType != "CoverageEligibilityResponse" {
		return "", fmt.Errorf("engine: expected CoverageEligibilityResponse, got %q", probe.ResourceType)
	}
	if probe.Patient.Reference == "" {
		return "", fmt.Errorf("engine: CoverageEligibilityResponse missing patient.reference")
	}
	return probe.Patient.Reference, nil
}

// ParsePASResponsePatients returns every patient reference in a PAS response: a
// bare ClaimResponse (approved/denied) carries one; a Bundle (pended) carries a
// ClaimResponse (+ possibly other resources) — collect every ClaimResponse's
// .patient.reference. Errors if none found. json-level, no FHIR lib. Used by
// the (C) outbound fence (pas-claim/pas-claim-update legs) and by
// test/adversarial. Exported because test/adversarial uses it directly.
func ParsePASResponsePatients(b []byte) ([]string, error) {
	var probe struct {
		ResourceType string `json:"resourceType"`
		Patient      struct {
			Reference string `json:"reference"`
		} `json:"patient"`
		Entry []struct {
			Resource struct {
				ResourceType string `json:"resourceType"`
				Patient      struct {
					Reference string `json:"reference"`
				} `json:"patient"`
			} `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return nil, fmt.Errorf("engine: parse PAS response: %w", err)
	}
	var refs []string
	switch probe.ResourceType {
	case "ClaimResponse":
		if probe.Patient.Reference != "" {
			refs = append(refs, probe.Patient.Reference)
		}
	case "Bundle":
		for _, e := range probe.Entry {
			if e.Resource.ResourceType == "ClaimResponse" && e.Resource.Patient.Reference != "" {
				refs = append(refs, e.Resource.Patient.Reference)
			}
		}
	}
	if len(refs) == 0 {
		return nil, fmt.Errorf("engine: PAS response has no ClaimResponse patient reference (resourceType %q)", probe.ResourceType)
	}
	return refs, nil
}

// questionnaireHasSubject reports whether a Questionnaire JSON carries any
// subject element. A DTR Questionnaire is patient-agnostic (the FHIR
// Questionnaire resource has no subject); the gateway (C) fence rejects one
// that unexpectedly names a subject. Unexported: only fenceResponseSubject uses
// it (fence_test.go drives it indirectly via fenceResponseSubject).
func questionnaireHasSubject(b []byte) bool {
	var probe struct {
		Subject *json.RawMessage `json:"subject"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return false // malformed → no valid subject; egress-$validate catches shape
	}
	return probe.Subject != nil
}

// packageQuestionnaireHasSubject reports whether ANY Questionnaire entry in a
// $questionnaire-package collection Bundle carries a subject. The DTR-fetch leg
// response is now a package Bundle (§6.2); the bare-resource questionnaireHasSubject
// would probe the Bundle wrapper (which has no subject) and silently pass, so the
// (C) subject fence must walk the package's Questionnaire entries. A partner could
// include several Questionnaires, so it checks every one. Unexported: only
// fenceResponseSubject uses it.
func packageQuestionnaireHasSubject(b []byte) bool {
	var bundle struct {
		Entry []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(b, &bundle); err != nil {
		return false // malformed → no valid subject; egress-$validate catches shape
	}
	for _, e := range bundle.Entry {
		var probe struct {
			ResourceType string `json:"resourceType"`
		}
		if err := json.Unmarshal(e.Resource, &probe); err != nil {
			continue
		}
		if probe.ResourceType == "Questionnaire" && questionnaireHasSubject(e.Resource) {
			return true
		}
	}
	return false
}

// parseEOBPatient returns an ExplanationOfBenefit's .patient.reference. It
// errors if the resourceType is not ExplanationOfBenefit or the patient
// reference is absent. json-level, no FHIR lib. Unexported: only
// fenceResponseSubject uses it.
func parseEOBPatient(b []byte) (string, error) {
	var probe struct {
		ResourceType string `json:"resourceType"`
		Patient      struct {
			Reference string `json:"reference"`
		} `json:"patient"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return "", fmt.Errorf("engine: parse ExplanationOfBenefit: %w", err)
	}
	if probe.ResourceType != "ExplanationOfBenefit" {
		return "", fmt.Errorf("engine: expected ExplanationOfBenefit, got %q", probe.ResourceType)
	}
	if probe.Patient.Reference == "" {
		return "", fmt.Errorf("engine: ExplanationOfBenefit missing patient.reference")
	}
	return probe.Patient.Reference, nil
}
