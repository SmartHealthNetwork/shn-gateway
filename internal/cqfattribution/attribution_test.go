package cqfattribution

import (
	"strings"
	"testing"
)

func TestNormalizeRejectsAmbiguousOrUnboundedJSON(t *testing.T) {
	for name, raw := range map[string]string{
		"duplicate key":  `{"resourceType":"QuestionnaireResponse","item":[],"item":[]}`,
		"trailing value": `{"resourceType":"QuestionnaireResponse"}{}`,
		"not object":     `[]`,
		"depth":          `{"resourceType":"QuestionnaireResponse","nested":` + strings.Repeat(`[`, 65) + `0` + strings.Repeat(`]`, 65) + `}`,
		"size":           strings.Repeat(" ", MaxBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Normalize([]byte(raw)); err == nil {
				t.Fatal("accepted invalid response")
			}
		})
	}
}

func TestNormalizePreservesNumberPrecision(t *testing.T) {
	raw := []byte(`{"resourceType":"QuestionnaireResponse","item":[{"linkId":"a","extension":[{"url":"http://hl7.org/fhir/StructureDefinition/questionnaireresponse-author","valueReference":{"reference":"http://cqframework.org/fhir/Device/clinical-quality-language"}}],"answer":[{"valueDecimal":12345678901234567890.123456789}]}]}`)
	out, err := Normalize(raw)
	if err != nil || !strings.Contains(string(out), "12345678901234567890.123456789") {
		t.Fatalf("number changed: %s %v", out, err)
	}
}
