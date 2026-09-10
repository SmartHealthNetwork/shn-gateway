package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
)

// Captured from the operated HAPI $populate path. The CQL software author is
// emitted as a fixed URI, with no accompanying Device resource (FR-32).
func TestNativePopulatorRetainsCQLSoftwareAttribution(t *testing.T) {
	raw, err := os.ReadFile("testdata/nativepopulate/oxygen-cql.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, uri string
		accept    bool
	}{
		{"known engine", "http://cqframework.org/fhir/Device/clinical-quality-language", true},
		{"unknown engine", "https://other.example/Device/clinical-quality-language", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := []byte(strings.ReplaceAll(string(raw), "http://cqframework.org/fhir/Device/clinical-quality-language", tc.uri))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/fhir+json")
				w.Write(input)
			}))
			defer server.Close()
			pkg := []byte(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Questionnaire","url":"http://example.org/fhir/Questionnaire/HomeOxygenDispatch"}}]}`)
			got, _, err := NewNativePopulator(server.Client(), server.URL).Populate(context.Background(), pkg, PopulateContext{PatientRef: "Patient/MBR-OX"})
			if err != nil {
				t.Fatal(err)
			}
			var expected, actual map[string]any
			json.Unmarshal(input, &expected)
			json.Unmarshal(got, &actual)
			count := 0
			var fix func(any)
			fix = func(v any) {
				switch x := v.(type) {
				case map[string]any:
					if x["url"] == "http://hl7.org/fhir/StructureDefinition/questionnaireresponse-author" && tc.accept {
						vr := x["valueReference"].(map[string]any)
						delete(vr, "reference")
						vr["identifier"] = map[string]any{"system": "urn:ietf:rfc:3986", "value": tc.uri}
						count++
					}
					for _, c := range x {
						fix(c)
					}
				case []any:
					for _, c := range x {
						fix(c)
					}
				}
			}
			fix(expected)
			if !reflect.DeepEqual(expected, actual) {
				t.Fatalf("changed clinical data, diagnostics, or attribution incorrectly: %s", got)
			}
			if tc.accept && count != 3 {
				t.Fatalf("author count=%d", count)
			}
			bundle := map[string]any{"resourceType": "Bundle", "type": "collection", "entry": []any{
				map[string]any{"fullUrl": "https://example.test/fhir/Patient/MBR-OX", "resource": map[string]any{"resourceType": "Patient", "id": "MBR-OX"}},
				map[string]any{"fullUrl": "https://example.test/fhir/QuestionnaireResponse/HomeOxygenDispatch-MBR-OX", "resource": actual},
			}}
			b, _ := json.Marshal(bundle)
			_, err = retainPASRequestEvidence(context.Background(), b, func(context.Context, string) ([]byte, bool, error) {
				t.Fatal("software attribution must not fetch resources")
				return nil, false, nil
			})
			if (err == nil) != tc.accept {
				t.Fatalf("closure error=%v", err)
			}
		})
	}
}

func TestCQLSoftwareAuthorCorrectionIsNarrow(t *testing.T) {
	const uri = "http://cqframework.org/fhir/Device/clinical-quality-language"
	for _, raw := range []string{
		`{"resourceType":"QuestionnaireResponse","extension":[{"url":"http://hl7.org/fhir/StructureDefinition/questionnaireresponse-author","valueReference":{"reference":"` + uri + `"}}]}`,
		`{"resourceType":"QuestionnaireResponse","item":[{"extension":[{"url":"https://other.example/author","valueReference":{"reference":"` + uri + `"}}]}]}`,
	} {
		got, err := identifyCQLSoftwareAuthor([]byte(raw))
		if err != nil || string(got) != raw {
			t.Fatalf("changed unrelated reference: %s %v", got, err)
		}
	}
	raw := []byte(`{"resourceType":"QuestionnaireResponse","item":[{"extension":[{"url":"http://hl7.org/fhir/StructureDefinition/questionnaireresponse-author","valueReference":{"reference":"` + uri + `","identifier":{"system":"urn:ietf:rfc:3986","value":"another engine"}}}]}]}`)
	if _, err := identifyCQLSoftwareAuthor(raw); err == nil {
		t.Fatal("accepted ambiguous software identity")
	}
}
