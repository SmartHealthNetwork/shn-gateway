// davincimap.go — pure translators between SHN's internal leg shapes and the real
// Da Vinci wire ops. No shnsdk symbols (standalone-build safe). See the design spec
// §3/§4: SHN's CRD hook omits the CDS-Hooks-required hookInstance; the DTR leg's
// ResponseFHIR is the full $questionnaire-package collection Bundle (§6.2) —
// buildQuestionnairePackage wraps the sandbox Questionnaire, native.go forwards a real
// partner's package VERBATIM, and extractQuestionnaireFromPackage (consumer-side, called
// from originate.go) extracts the bare Questionnaire for F5/auto-fill. Deps survive the
// wire.
package engine

import (
	"encoding/json"
	"fmt"
)

// augmentCRDHook adds the CDS-Hooks-required hookInstance to SHN's minimized
// order-select hook, preserving every other field. SHN's OrderSelectRequest omits
// hookInstance (sdk/crd.go); a real /cds-services endpoint requires it.
func augmentCRDHook(hookJSON []byte, hookInstance string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(hookJSON, &m); err != nil {
		return nil, fmt.Errorf("engine: parse CRD hook: %w", err)
	}
	hi, err := json.Marshal(hookInstance)
	if err != nil {
		return nil, fmt.Errorf("engine: marshal hookInstance: %w", err)
	}
	m["hookInstance"] = hi
	return json.Marshal(m)
}

// buildQuestionnairePackageRequest translates SHN's {canonical} DTR fetch into a
// Da Vinci $questionnaire-package Parameters request.
func buildQuestionnairePackageRequest(canonical string) ([]byte, error) {
	params := map[string]any{
		"resourceType": "Parameters",
		"parameter": []map[string]any{
			{"name": "questionnaire", "valueCanonical": canonical},
		},
	}
	return json.Marshal(params)
}

// extractQuestionnaireFromPackage pulls the bare Questionnaire entry out of a
// $questionnaire-package collection Bundle, returning its bytes VERBATIM. Called by the
// consumer (originate.go) after the full package has crossed the wire — the package's
// dependent Libraries/ValueSets survive the wire intact inside the Bundle; this extractor
// returns the bare Questionnaire that originate.go feeds to ParseQuestionnaireURL (F5)
// and FillQuestionnaire (auto-fill). A package with no Questionnaire entry returns an
// error (→ 502 at the consumer: partner fault).
func extractQuestionnaireFromPackage(packageBundle []byte) ([]byte, error) {
	var bundle struct {
		Entry []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(packageBundle, &bundle); err != nil {
		return nil, fmt.Errorf("engine: parse $questionnaire-package bundle: %w", err)
	}
	for _, e := range bundle.Entry {
		var probe struct {
			ResourceType string `json:"resourceType"`
		}
		if err := json.Unmarshal(e.Resource, &probe); err != nil {
			continue
		}
		if probe.ResourceType == "Questionnaire" {
			return e.Resource, nil
		}
	}
	return nil, fmt.Errorf("engine: $questionnaire-package response contains no Questionnaire")
}

// buildQuestionnairePackage wraps a bare Questionnaire into a one-entry
// $questionnaire-package collection Bundle — the uniform DTR-fetch leg response
// shape (§6.2). The sandbox payer has no dependent Libraries/ValueSets, so this
// wrapper is honestly deps-free; a real partner's package (forwarded VERBATIM by
// native.go) carries them. shnsdk-free (standalone-build safe). The byte shape
// (json.Marshal of this map) is load-bearing: the test loopback's default wrap
// must match it exactly for the DTR-fetch leg to stay byte-parity in
// test/responderparity (the wrapper is engine-value-free — no corrID/clock).
func buildQuestionnairePackage(questionnaire []byte) ([]byte, error) {
	// A FHIR collection Bundle requires every entry to carry a fullUrl (IG-HAPI
	// $validate enforces this — caught by make validate, not the hermetic gate).
	// Use the Questionnaire's canonical url as the entry identity (deterministic;
	// keeps the wrap byte-identical across the gateway/sdk/substrate/loopback
	// producers for test/responderparity + test/sdkparity byte-parity).
	var probe struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(questionnaire, &probe); err != nil {
		return nil, fmt.Errorf("engine: buildQuestionnairePackage: questionnaire is not valid json: %w", err)
	}
	if probe.URL == "" {
		return nil, fmt.Errorf("engine: buildQuestionnairePackage: questionnaire has no url for entry fullUrl")
	}
	pkg := map[string]any{
		"resourceType": "Bundle",
		"type":         "collection",
		"entry": []map[string]any{
			{"fullUrl": probe.URL, "resource": json.RawMessage(questionnaire)},
		},
	}
	return json.Marshal(pkg)
}

// jsonUnmarshalStrictCanonical extracts a non-empty "canonical" string from the DTR
// fetch request body, erroring on malformed JSON or a missing/empty canonical.
func jsonUnmarshalStrictCanonical(data []byte, out *string) error {
	var probe struct {
		Canonical string `json:"canonical"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	if probe.Canonical == "" {
		return fmt.Errorf("engine: DTR fetch missing canonical")
	}
	*out = probe.Canonical
	return nil
}
