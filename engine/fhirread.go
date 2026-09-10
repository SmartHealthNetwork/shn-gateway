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
	"net/url"
	"strings"
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
// bare polling ClaimResponse carries one; a native response Bundle carries a
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
// response is now a package Bundle; the bare-resource questionnaireHasSubject
// would probe the Bundle wrapper (which has no subject) and silently pass, so the
// (C) subject fence must walk the package's Questionnaire entries. A partner could
// include several Questionnaires, so it checks every one. Unexported: only
// fenceResponseSubject uses it.
//
// unwrapQuestionnairePackage is called first so that br-payer's Parameters wrapper
// (dtr-qpackage-output-parameters) is normalised to its inner Bundle before the walk.
// Without this, the fence would see no entries in the Parameters wrapper and return
// false — silently becoming vacuous against br-payer's real response shape.
func packageQuestionnaireHasSubject(b []byte) bool {
	b = unwrapQuestionnairePackage(b)
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

// consistentPASResponseSubjects checks the entire retained graph against its own
// ClaimResponse patient, independently of the authorized request's namespace.
func consistentPASResponseSubjects(raw []byte) bool {
	g, err := readPASGraph(raw)
	if err != nil || g.validate() != nil {
		return false
	}
	patient, ok := g.response.resource["patient"].(map[string]any)
	if !ok {
		return false
	}
	ref, ok := patient["reference"].(string)
	if !ok {
		return false
	}
	expected := pasSubjectIdentity(g.response, ref)
	if expected == "" {
		return false
	}
	return consistentPASGraphSubjects(g, expected)
}

// consistentPASGraphSubjects binds every primary subject in a validated graph,
// including contained evidence, before either request or response exchange.
func consistentPASGraphSubjects(g *pasGraph, expected string) bool {
	if expected == "" {
		return false
	}
	// R4 primary subject references use these field names across resources:
	// patient/subject, Coverage.beneficiary, Task.for, and subject[x]'s Reference choice. ResearchSubject.individual
	// and EnrollmentRequest.candidate are resource-specific roles; an Encounter
	// participant.individual may instead identify a clinician. Apply the
	// rule to every object, including contained resources and nested elements;
	// an unfamiliar resource type cannot silently exempt an explicit subject.
	subjectFields := map[string]bool{"patient": true, "subject": true, "beneficiary": true, "for": true, "subjectReference": true, "patientReference": true}
	var boundReference func(any, *pasGraphEntry) bool
	boundReference = func(value any, owner *pasGraphEntry) bool {
		if refs, ok := value.([]any); ok {
			if len(refs) == 0 {
				return false
			}
			for _, ref := range refs {
				if !boundReference(ref, owner) {
					return false
				}
			}
			return true
		}
		ref, ok := value.(map[string]any)
		if !ok {
			return false
		}
		literal, ok := ref["reference"].(string)
		// Identifier-only subjects have no exact graph identity to bind.
		return ok && literal != "" && pasSubjectIdentity(owner, literal) == expected
	}

	found := false
	var visit func(any, *pasGraphEntry, int) bool
	visit = func(v any, owner *pasGraphEntry, depth int) bool {
		switch x := v.(type) {
		case map[string]any:
			if typ, ok := x["resourceType"].(string); ok {
				if typ == "Patient" {
					identity := owner.fullURL
					if depth > 0 {
						id, ok := x["id"].(string)
						if !ok {
							return false
						}
						identity += "#" + id
					}
					if identity != expected {
						return false
					}
					found = true
				}

			}
			for field, value := range x {
				subjectField := subjectFields[field] || (field == "individual" && x["resourceType"] == "ResearchSubject") || (field == "candidate" && x["resourceType"] == "EnrollmentRequest")
				if subjectField && !boundReference(value, owner) {
					return false
				}
			}
			// A typed Patient Reference is also subject-bearing in polymorphic
			// paths (e.g. actor or extension.valueReference). Do not let an
			// identifier-only form bypass binding simply because its role differs.
			if typ, ok := x["type"].(string); ok && (typ == "Patient" || typ == "http://hl7.org/fhir/StructureDefinition/Patient") {
				if !boundReference(x, owner) {
					return false
				}
			}
			for _, child := range x {
				if !visit(child, owner, depth+1) {
					return false
				}
			}
		case []any:
			for _, child := range x {
				if !visit(child, owner, depth+1) {
					return false
				}
			}
		}
		return true
	}
	for _, entry := range g.byURL {
		if !visit(entry.resource, entry, 0) {
			return false
		}
	}
	return found
}

func pasSubjectIdentity(owner *pasGraphEntry, ref string) string {
	if ref == "#" {
		return owner.fullURL
	}
	if strings.HasPrefix(ref, "#") {
		return owner.fullURL + ref
	}
	u, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	if !u.IsAbs() {
		base, err := url.Parse(owner.fullURL)
		if err != nil || (base.Scheme != "http" && base.Scheme != "https") {
			return ""
		}
		typ := owner.resource["resourceType"].(string)
		id := owner.resource["id"].(string)
		base.Path = strings.TrimSuffix(base.Path, "/"+typ+"/"+id) + "/" + u.Path
		ref = base.String()
	}
	if pos := strings.Index(ref, "/_history/"); pos >= 0 {
		ref = ref[:pos]
	}
	return ref
}
