package engine

import "strings"

// definitionOnlyDTRResponse proves a narrow no-patient-assertion response subset.
// DTR 2.0/2.1 permit Questionnaire packages without a QuestionnaireResponse;
// 2.2 requires one. Profile, terminology and envelope checks remain independent.
func definitionOnlyDTRResponse(in CheckInput) bool {
	if in.Exchange.legType != "dtr-questionnaire-fetch" || in.Exchange.operation != "questionnaire-package" || in.Direction != "response" || (in.DeclaredVersion != "pa.dtr@2.0" && in.DeclaredVersion != "pa.dtr@2.1") {
		return false
	}
	root, ok := deepDocument(in)
	if !ok {
		return false
	}
	bundles := []any{root}
	if resourceIs(root, "Parameters") {
		params, ok := parameterShapes(root, true)
		if !ok || !dtrPackageOutputParameters(params, strings.TrimPrefix(in.DeclaredVersion, "pa.dtr@")) {
			return false
		}
		bundles = nil
		for _, raw := range params {
			p := raw.(map[string]any)
			if p["name"] != "PackageBundle" && !(in.DeclaredVersion == "pa.dtr@2.0" && p["name"] == "return") {
				return false
			}
			bundle, ok := p["resource"].(map[string]any)
			if !ok || !resourceIs(bundle, "Bundle") {
				return false
			}
			bundles = append(bundles, bundle)
		}
	}
	for _, raw := range bundles {
		b, ok := raw.(map[string]any)
		if !ok || b["type"] != "collection" {
			return false
		}
		entries, ok := bundleEntries(b, true)
		if !ok || len(entries) == 0 {
			return false
		}
		questionnaires := 0
		seen := map[string]bool{}
		for _, raw := range entries {
			e, ok := raw.(map[string]any)
			if !ok {
				return false
			}
			url, ok := e["fullUrl"].(string)
			if !ok || url == "" || seen[url] {
				return false
			}
			seen[url] = true
			r, ok := e["resource"].(map[string]any)
			if !ok {
				return false
			}
			switch r["resourceType"] {
			case "Questionnaire":
				questionnaires++
			case "Library", "ValueSet":
			default:
				return false
			}
		}
		if questionnaires != 1 {
			return false
		}
	}
	// Decline the exception for any patient/clinical assertion, even one nested
	// inside an otherwise definition-only entry. No local identity is inferred.
	return definitionDataOnly(root, true)
}

func definitionDataOnly(v any, outer bool) bool {
	switch x := v.(type) {
	case map[string]any:
		// FHIR Reference.identifier is an object; ordinary artifact identifiers
		// are arrays. A Reference need not carry a literal reference URL or type.
		if _, referenceIdentifier := x["identifier"].(map[string]any); referenceIdentifier {
			return false
		}
		if rt, ok := x["resourceType"]; ok {
			switch rt {
			case "Parameters", "Bundle":
				if !outer {
					return false
				}
			case "Questionnaire", "Library", "ValueSet":
			default:
				return false
			}
		}
		for k, child := range x {
			// Covers choice-valued References such as Extension.valueReference,
			// including identifier-only and otherwise empty/malformed forms.
			if strings.HasSuffix(k, "Reference") {
				return false
			}
			switch k {
			case "contained", "reference", "subject", "subjectReference", "subjectCodeableConcept", "patient", "beneficiary":
				return false
			}
			// Only the outer operation and its package Bundle may be containers.
			allowContainer := outer && (k == "parameter" || k == "resource")
			if !definitionDataOnly(child, allowContainer) {
				return false
			}
		}
	case []any:
		for _, child := range x {
			if !definitionDataOnly(child, outer) {
				return false
			}
		}
	}
	return true
}
