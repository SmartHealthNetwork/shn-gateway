package cqfattribution

import (
	"encoding/json"
	"errors"
)

// MaxBytes bounds native population responses before attribution processing.
const MaxBytes = 8 << 20
const maxDepth = 64

func invalidResponse() error { return errors.New("invalid CQL attribution response") }

// Normalize preserves CQL software attribution using its exact URI identity.
// CQFramework's ItemProcessor.addAuthorExtension emits CQL_ENGINE_DEVICE as
// an absolute Reference without a Device resource. Preserve this known software
// attribution as its URI identifier (FR-32), rather than asserting a retrievable
// Device. Only CQF-populator item author extensions receive this correction;
// other references still have to satisfy ordinary PAS graph closure.
// Source: cqframework/clinical-reasoning, ExtensionBuilders and Constants.
func Normalize(raw []byte) ([]byte, error) {
	const authorURL = "http://hl7.org/fhir/StructureDefinition/questionnaireresponse-author"
	const cqlURI = "http://cqframework.org/fhir/Device/clinical-quality-language"
	var qr map[string]any
	if len(raw) > MaxBytes || decodeObject(raw, &qr) != nil {
		return nil, invalidResponse()
	}
	changed := false
	var items func(any, int) error
	items = func(v any, depth int) error {
		if depth > maxDepth {
			return invalidResponse()
		}
		list, ok := v.([]any)
		if !ok {
			return nil
		}
		for _, v := range list {
			item, ok := v.(map[string]any)
			if !ok {
				return invalidResponse()
			}
			if exts, ok := item["extension"].([]any); ok {
				for _, v := range exts {
					ext, ok := v.(map[string]any)
					if !ok || ext["url"] != authorURL {
						continue
					}
					ref, ok := ext["valueReference"].(map[string]any)
					if !ok || ref["reference"] != cqlURI {
						continue
					}
					if _, exists := ref["identifier"]; exists {
						return invalidResponse()
					}
					delete(ref, "reference")
					ref["identifier"] = map[string]any{"system": "urn:ietf:rfc:3986", "value": cqlURI}
					changed = true
				}
			}
			if err := items(item["item"], depth+1); err != nil {
				return err
			}
			if answers, ok := item["answer"].([]any); ok {
				for _, v := range answers {
					if answer, ok := v.(map[string]any); ok {
						if err := items(answer["item"], depth+1); err != nil {
							return err
						}
					}
				}
			}
		}
		return nil
	}
	if err := items(qr["item"], 0); err != nil {
		return nil, err
	}
	if !changed {
		return raw, nil
	}
	return json.Marshal(qr)
}
