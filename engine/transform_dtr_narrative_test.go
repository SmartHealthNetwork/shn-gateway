package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

const syntheticPackageGolden = "2.2/questionnaire-package-synthetic-fixture-availability.json"

func narrativePackage(t *testing.T) map[string]any {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal(pasGolden(t, syntheticPackageGolden), &p); err != nil {
		t.Fatal(err)
	}
	return p
}
func TestDTRStandardQuestionnaireNarrativeRefusal(t *testing.T) {
	for _, name := range []string{"captured-foreign", "valid-minus-text", "null-text", "second-questionnaire", "versioned-declaration", "bare-standard"} {
		for _, to := range []string{"2.1", "2.0"} {
			t.Run(name+"/"+to, func(t *testing.T) {
				p := narrativePackage(t)
				q := dtrCollectResources(p)["Questionnaire"][0]
				switch name {
				case "valid-minus-text", "bare-standard":
					delete(q, "text")
				case "null-text":
					q["text"] = nil
				case "second-questionnaire":
					second := narrativePackage(t)
					q2 := dtrCollectResources(second)["Questionnaire"][0]
					delete(q2, "text")
					q2["id"] = "second"
					q2["url"] = "https://shn.example/fhir/Questionnaire/second"
					p["entry"] = append(p["entry"].([]any), map[string]any{"resource": q2})
				case "versioned-declaration":
					delete(q, "text")
					q["meta"] = map[string]any{"profile": []any{"https://shn.example/unrelated", "http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/dtr-std-questionnaire|2.2.0"}}
				}
				in, _ := json.Marshal(p)
				if name == "bare-standard" {
					in, _ = json.Marshal(q)
				}
				if name == "captured-foreign" {
					// The reference payer's captured Questionnaire carries its narrative;
					// the same package minus that narrative refuses.
					var captured map[string]any
					if err := json.Unmarshal(pasGolden(t, "2.2/questionnaire-package-pa-lumbar-mri.json"), &captured); err != nil {
						t.Fatal(err)
					}
					delete(dtrCollectResources(captured)["Questionnaire"][0], "text")
					in, _ = json.Marshal(captured)
				}
				saved := bytes.Clone(in)
				out, reports, err := TransformDTRForTest("2.2", to, in, corr)
				var sc *SemanticChangeError
				if !errors.As(err, &sc) {
					t.Fatalf("want typed semantic refusal, got %v", err)
				}
				if sc.Contract != "pa.dtr" || sc.From != "2.2" || sc.To != "2.1" || sc.Direction != "down" || !reflect.DeepEqual(sc.MissingElements, []string{"Questionnaire.text"}) {
					t.Fatalf("wrong refusal: %+v", sc)
				}
				if out != nil || reports != nil {
					t.Fatalf("partial output/reports escaped refusal: %s %+v", out, reports)
				}
				if !bytes.Equal(in, saved) {
					t.Fatal("source input mutated")
				}
			})
		}
	}
}
func TestDTRStandardQuestionnaireNarrativeSpecificity(t *testing.T) {
	for _, name := range []string{"authored-narrative", "unprofiled", "base-profile", "unrelated-profile"} {
		for _, to := range []string{"2.1", "2.0"} {
			t.Run(name+"/"+to, func(t *testing.T) {
				p := narrativePackage(t)
				q := dtrCollectResources(p)["Questionnaire"][0]
				if name != "authored-narrative" {
					delete(q, "text")
					switch name {
					case "unprofiled":
						delete(q, "meta")
					case "base-profile":
						q["meta"] = map[string]any{"profile": []any{"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/dtr-base-questionnaire"}}
					case "unrelated-profile":
						q["meta"] = map[string]any{"profile": []any{"https://shn.example/fhir/StructureDefinition/other-questionnaire"}}
					}
				}
				want := dtrCollectResources(p)["Questionnaire"]
				in, _ := json.Marshal(p)
				out, _, err := TransformDTRForTest("2.2", to, in, corr)
				if err != nil {
					t.Fatal(err)
				}
				var got map[string]any
				json.Unmarshal(out, &got)
				if !reflect.DeepEqual(want, dtrCollectResources(got)["Questionnaire"]) {
					t.Fatal("Questionnaire content changed")
				}
			})
		}
	}
	for _, to := range []string{"2.1", "2.0"} {
		t.Run("captured-foreign-narrative/"+to, func(t *testing.T) {
			in := pasGolden(t, "2.2/questionnaire-package-pa-lumbar-mri.json")
			var p map[string]any
			if err := json.Unmarshal(in, &p); err != nil {
				t.Fatal(err)
			}
			want := dtrCollectResources(p)["Questionnaire"]
			if len(want) != 1 || want[0]["text"] == nil {
				t.Fatal("captured package no longer carries the payer's Questionnaire narrative")
			}
			out, _, err := TransformDTRForTest("2.2", to, in, corr)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			json.Unmarshal(out, &got)
			if !reflect.DeepEqual(want, dtrCollectResources(got)["Questionnaire"]) {
				t.Fatal("Questionnaire content changed")
			}
		})
	}
	t.Run("QR-only", func(t *testing.T) {
		in := pasGolden(t, "2.2/questionnaireresponse-autofill.json")
		if _, _, err := TransformDTRForTest("2.2", "2.1", in, corr); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("native-captured-2.2", func(t *testing.T) {
		in := pasGolden(t, "2.2/questionnaire-package-pa-lumbar-mri.json")
		out, _, err := TransformDTRForTest("2.2", "2.2", in, corr)
		if err != nil || !bytes.Equal(in, out) {
			t.Fatalf("native path changed: %v", err)
		}
	})
}
