package engine

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestTransformPreservesNumericLiterals verifies that transform output retains
// the spelling of each numeric JSON literal.
func TestTransformPreservesNumericLiterals(t *testing.T) {
	literals := []string{`0.50`, `1e2`, `12345678901234567890`, `1.10`}
	claim := []byte(`{"resourceType":"Claim","id":"c1","total":{"value":1.10,"currency":"USD"},"item":[{"sequence":1,"quantity":{"value":0.50},"unitPrice":{"value":1e2,"currency":"USD"},"net":{"value":12345678901234567890}}]}`)
	qr := []byte(`{"resourceType":"QuestionnaireResponse","status":"completed","extension":[{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-context","valueReference":{"reference":"Coverage/c"}},{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-context","valueReference":{"reference":"ServiceRequest/s"}}],"item":[{"linkId":"1","answer":[{"valueDecimal":0.50}]},{"linkId":"2","answer":[{"valueDecimal":1e2}]},{"linkId":"3","answer":[{"valueDecimal":12345678901234567890}]},{"linkId":"4","answer":[{"valueDecimal":1.10}]}]}`)
	steps := []struct {
		name string
		in   []byte
		fn   func([]byte, ExchangeIdentity) ([]byte, LossReport, error)
	}{
		{"pas", claim, pasStep2122Down},
		{"pas", claim, pasStep2021Down},
		{"pas", claim, pasStep2122Up},
		{"dtr", qr, dtrStep2122Up},
	}
	for _, s := range steps {
		out, _, err := s.fn(s.in, corr)
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		for _, lit := range literals {
			if got := numericField(t, out, numericPaths(s.name)[lit]); !bytes.Equal(got, []byte(lit)) {
				t.Errorf("%s: literal %s became %s", s.name, lit, got)
			}
		}
	}

	up, _, err := dtrStep2122Up(qr, corr)
	if err != nil {
		t.Fatal(err)
	}
	down, _, err := dtrStep2122Down(up, corr)
	if err != nil {
		t.Fatal(err)
	}
	for _, lit := range literals {
		if got := numericField(t, down, numericPaths("dtr")[lit]); !bytes.Equal(got, []byte(lit)) {
			t.Errorf("dtr 2.2->2.1: literal %s became %s", lit, got)
		}
	}
}

func TestTransformParsersRequireOneDocument(t *testing.T) {
	parsers := []struct {
		name  string
		parse func([]byte) (map[string]any, error)
		step  func([]byte, ExchangeIdentity) ([]byte, LossReport, error)
	}{
		{"pas", pasParseTop, pasStep2122Down},
		{"dtr", dtrParseTop, dtrStep2122Down},
	}
	for _, p := range parsers {
		t.Run(p.name, func(t *testing.T) {
			for _, suffix := range []string{`{}`, `null`, `1`, `trailing garbage`} {
				input := []byte(`{"resourceType":"QuestionnaireResponse"}` + suffix)
				if _, err := p.parse(input); err == nil {
					t.Errorf("parser accepted extra input %q", suffix)
				}
				if _, _, err := p.step(input, corr); err == nil {
					t.Errorf("transform accepted extra input %q", suffix)
				}
			}
			if _, err := p.parse([]byte("{\"resourceType\":\"QuestionnaireResponse\"} \n\t")); err != nil {
				t.Fatalf("parser rejected trailing whitespace: %v", err)
			}
		})
	}
}

func TestTransformCarryRestoresNumericLiterals(t *testing.T) {
	t.Run("pas transmission identifiers", func(t *testing.T) {
		in := []byte(`{"resourceType":"Claim","id":"c1","extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-TransmissionIdentifiers","extension":[{"url":"applicationSenderCode","valueString":"SND-0001"},{"url":"https://example.org/fhir/StructureDefinition/numeric-probe","valueDecimal":0.50}]}]}`)
		probeURL, ok := numericFieldAt(in, []any{"extension", 0, "extension", 1, "url"})
		if !ok || !bytes.Equal(probeURL, []byte(`"https://example.org/fhir/StructureDefinition/numeric-probe"`)) {
			t.Fatalf("numeric probe URL = %s, want the synthetic extension URL", probeURL)
		}
		down, report, err := pasStep2122Down(in, corr)
		if err != nil {
			t.Fatal(err)
		}
		if len(report.Carried) != 1 || report.Carried[0].Path != "Claim.extension:transmissionIdentifiers" {
			t.Fatalf("unexpected carry report: %+v", report.Carried)
		}
		probePath := []any{"extension", 0, "extension", 1, "valueDecimal"}
		if _, ok := numericFieldAt(down, probePath); ok {
			t.Fatal("carried probe is still present in the delivered representation")
		}
		up, _, err := pasStep2122Up(down, corr)
		if err != nil {
			t.Fatal(err)
		}
		if got := numericField(t, up, probePath); !bytes.Equal(got, []byte(`0.50`)) {
			t.Fatalf("restored probe became %s", got)
		}
	})

	t.Run("dtr coding item weight", func(t *testing.T) {
		in := []byte(`{"resourceType":"QuestionnaireResponse","status":"completed","extension":[{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-coverage","valueReference":{"reference":"Coverage/c"}},{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-context","valueReference":{"reference":"ServiceRequest/s"}}],"item":[{"linkId":"1","answer":[{"valueCoding":{"system":"http://terminology.hl7.org/CodeSystem/v2-0136","code":"N","extension":[{"url":"http://hl7.org/fhir/StructureDefinition/itemWeight","valueDecimal":1.10}]}}]}]}`)
		down, report, err := dtrStep2122Down(in, corr)
		if err != nil {
			t.Fatal(err)
		}
		if len(report.Carried) != 1 || report.Carried[0].Path != dtrItemWeightLocus {
			t.Fatalf("unexpected carry report: %+v", report.Carried)
		}
		weightPath := []any{"item", 0, "answer", 0, "valueCoding", "extension", 0, "valueDecimal"}
		if _, ok := numericFieldAt(down, weightPath); ok {
			t.Fatal("carried item weight is still present in the delivered representation")
		}
		up, _, err := dtrStep2122Up(down, corr)
		if err != nil {
			t.Fatal(err)
		}
		if got := numericField(t, up, weightPath); !bytes.Equal(got, []byte(`1.10`)) {
			t.Fatalf("restored item weight became %s", got)
		}
	})
}

func numericPaths(name string) map[string][]any {
	if name == "pas" {
		return map[string][]any{
			`0.50`:                 {"item", 0, "quantity", "value"},
			`1e2`:                  {"item", 0, "unitPrice", "value"},
			`12345678901234567890`: {"item", 0, "net", "value"},
			`1.10`:                 {"total", "value"},
		}
	}
	return map[string][]any{
		`0.50`:                 {"item", 0, "answer", 0, "valueDecimal"},
		`1e2`:                  {"item", 1, "answer", 0, "valueDecimal"},
		`12345678901234567890`: {"item", 2, "answer", 0, "valueDecimal"},
		`1.10`:                 {"item", 3, "answer", 0, "valueDecimal"},
	}
}

func numericField(t *testing.T, raw []byte, path []any) json.RawMessage {
	t.Helper()
	field, ok := numericFieldAt(raw, path)
	if !ok {
		t.Fatalf("missing numeric field at %v in %s", path, raw)
	}
	return field
}

func numericFieldAt(raw []byte, path []any) (json.RawMessage, bool) {
	current := json.RawMessage(raw)
	for _, part := range path {
		switch p := part.(type) {
		case string:
			var object map[string]json.RawMessage
			if err := json.Unmarshal(current, &object); err != nil {
				return nil, false
			}
			var ok bool
			current, ok = object[p]
			if !ok {
				return nil, false
			}
		case int:
			var array []json.RawMessage
			if err := json.Unmarshal(current, &array); err != nil || p < 0 || p >= len(array) {
				return nil, false
			}
			current = array[p]
		default:
			return nil, false
		}
	}
	return current, true
}

func TestTransformParsersRejectMalformedDocumentSuffixes(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func([]byte, ExchangeIdentity) ([]byte, LossReport, error)
	}{
		{"pas", pasStep2122Down},
		{"dtr", dtrStep2122Down},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := tc.fn([]byte(`{"resourceType":"Bundle"} trailing`), corr)
			if err == nil || !strings.Contains(err.Error(), "invalid JSON") {
				t.Fatalf("want invalid JSON error, got %v", err)
			}
		})
	}
}
