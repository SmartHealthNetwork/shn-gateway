package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

const contextQR = `{"resourceType":"QuestionnaireResponse","status":"in-progress","subject":{"reference":"Patient/synthetic","display":"Synthetic"},"questionnaire":"https://example.test/Questionnaire/synthetic","item":[{"linkId":"1","answer":[{"valueDecimal":9007199254740993.2300}]}],"extension":[{"url":"https://example.test/opaque","valueDecimal":9007199254740993.2300}]}`

func contextInputs() shnsdk.QRContext {
	return shnsdk.QRContext{PatientRef: "Patient/synthetic", CoverageRef: "Coverage/synthetic", OrderRef: "DeviceRequest/synthetic"}
}
func TestDTRContextComposition(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		for _, order := range []string{"ServiceRequest/order", "DeviceRequest/order"} {
			t.Run(line+"/"+order, func(t *testing.T) {
				qc := contextInputs()
				qc.OrderRef = order
				input := []byte(contextQR)
				before := append([]byte(nil), input...)
				out, err := composeDTRContextAtLine(input, line, qc)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(input, before) {
					t.Fatal("changed input")
				}
				var old, got map[string]json.RawMessage
				json.Unmarshal(input, &old)
				json.Unmarshal(out, &got)
				for _, key := range []string{"subject", "item", "status", "questionnaire"} {
					if !bytes.Equal(old[key], got[key]) {
						t.Fatalf("changed %s: %s", key, got[key])
					}
				}
				if !bytes.Contains(out, []byte(`9007199254740993.2300`)) {
					t.Fatal("changed numeric lexeme")
				}
				again, err := composeDTRContextAtLine(out, line, qc)
				if err != nil || !bytes.Equal(out, again) {
					t.Fatalf("not idempotent: %s %v", again, err)
				}
				m := lfDecode(t, out)
				exts := m["extension"].([]any)
				if len(exts) != 4 {
					t.Fatalf("extensions=%d", len(exts))
				}
				coverageURL := lfDTR + "qr-context"
				system := "http://hl7.org/fhir/us/davinci-crd/CodeSystem/temp"
				version := "2.0.1"
				if line == "2.1" {
					version = "2.1.0"
				}
				if line == "2.2" {
					coverageURL = lfDTR + "qr-coverage"
					system = "http://hl7.org/fhir/us/davinci-crd/CodeSystem/coverage-information-codes"
					version = "2.2.0"
				}
				for _, expect := range []struct{ url, ref string }{{coverageURL, "Coverage/synthetic"}, {lfDTR + "qr-context", order}} {
					found := false
					for _, e := range exts {
						em := e.(map[string]any)
						vr, _ := em["valueReference"].(map[string]any)
						if em["url"] == expect.url && vr["reference"] == expect.ref {
							found = true
						}
					}
					if !found {
						t.Fatalf("missing %s %s", expect.url, expect.ref)
					}
				}
				last := exts[3].(map[string]any)
				coding := last["valueCodeableConcept"].(map[string]any)["coding"].([]any)[0].(map[string]any)
				if coding["system"] != system || coding["code"] != "withpa" {
					t.Fatalf("coding=%v", coding)
				}
				if !bytes.Contains(got["meta"], []byte(lfDTR+"dtr-questionnaireresponse|"+version)) {
					t.Fatalf("profile=%s", got["meta"])
				}
			})
		}
	}
}

func TestDTRContextConflictRows(t *testing.T) {
	ext := func(url, ref string) string {
		return `{"url":"` + lfDTR + url + `","valueReference":{"reference":"` + ref + `"}}`
	}
	for _, tc := range []struct{ name, extension, meta string }{
		{"extension URL absent", `{}`, ""},
		{"extension URL wrong type", `{"url":42}`, ""},
		{"context reference wrong type", `{"url":"` + lfDTR + `qr-context","valueReference":{"reference":42,"identifier":{"value":"synthetic"}}}`, ""},
		{"context type wrong type", `{"url":"` + lfDTR + `qr-context","valueReference":{"reference":"DeviceRequest/synthetic","type":42}}`, ""},
		{"reference type disagrees", `{"url":"` + lfDTR + `qr-coverage","valueReference":{"reference":"Coverage/synthetic","type":"Patient"}}`, ""},
		{"duplicate order", ext("qr-context", "DeviceRequest/synthetic") + "," + ext("qr-context", "DeviceRequest/synthetic"), ""},
		{"mixed value arms", `{"url":"` + lfDTR + `qr-coverage","valueReference":{"reference":"Coverage/synthetic"},"valueString":"ambiguous"}`, ""},
		{"nested plus value", `{"url":"` + lfDTR + `qr-coverage","valueReference":{"reference":"Coverage/synthetic"},"extension":[]}`, ""},
		{"mixed intended systems", `{"url":"` + lfDTR + `intendedUse","valueCodeableConcept":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-crd/CodeSystem/coverage-information-codes","code":"withpa"},{"system":"http://hl7.org/fhir/us/davinci-crd/CodeSystem/temp","code":"withpa"}]}}`, ""},
		{"other coverage", ext("qr-coverage", "Coverage/other"), ""},
		{"active order type disagrees", `{"url":"` + lfDTR + `qr-context","valueReference":{"reference":"DeviceRequest/synthetic","type":"ServiceRequest"}}`, ""},
		{"order identity absent", `{"url":"` + lfDTR + `qr-context","valueReference":{"type":"DeviceRequest"}}`, ""},
		{"order identity malformed", ext("qr-context", "ServiceRequest/invalid/id"), ""},
		{"order identity empty", ext("qr-context", "DeviceRequest/"), ""},
		{"optional type disagrees", `{"url":"` + lfDTR + `qr-context","valueReference":{"reference":"ServiceRequest/other","type":"DeviceRequest"}}`, ""},
		{"duplicate coverage", ext("qr-coverage", "Coverage/synthetic") + "," + ext("qr-coverage", "Coverage/synthetic"), ""},
		{"old line coverage", ext("qr-context", "Coverage/synthetic"), ""},
		{"coverage wrong type", ext("qr-coverage", "Patient/synthetic"), ""},
		{"missing reference", `{"url":"` + lfDTR + `qr-coverage","valueString":"wrong"}`, ""},
		{"conflicting intended use", `{"url":"` + lfDTR + `intendedUse","valueCodeableConcept":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-crd/CodeSystem/coverage-information-codes","code":"withclaim"}]}}`, ""},
		{"other-line intended use", `{"url":"` + lfDTR + `intendedUse","valueCodeableConcept":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-crd/CodeSystem/temp","code":"withpa"}]}}`, ""},
		{"wrong profile line", "", `{"profile":["` + lfDTR + `dtr-questionnaireresponse|2.1.0"]}`},
		{"malformed profiles", "", `{"profile":42}`},
		{"null extension", `null`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := map[string]json.RawMessage{}
			json.Unmarshal([]byte(contextQR), &m)
			if tc.extension != "" {
				m["extension"] = json.RawMessage("[" + tc.extension + "]")
			}
			if tc.meta != "" {
				m["meta"] = json.RawMessage(tc.meta)
			}
			raw, _ := json.Marshal(m)
			if _, err := composeDTRContextAtLine(raw, "2.2", contextInputs()); err == nil {
				t.Fatal("accepted contradictory or malformed context")
			}
		})
	}
	for _, raw := range []string{`null`, `[]`, `{}`, contextQR + ` {}`, strings.Replace(contextQR, `"extension":[`, `"extension":null,"unused":[`, 1), strings.Replace(contextQR, `"QuestionnaireResponse"`, `"Patient"`, 1), strings.Replace(contextQR, `Patient/synthetic`, `Patient/other`, 1)} {
		if _, err := composeDTRContextAtLine([]byte(raw), "2.2", contextInputs()); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	for _, qc := range []shnsdk.QRContext{{}, {PatientRef: "Patient/synthetic", CoverageRef: "Patient/synthetic", OrderRef: "DeviceRequest/order"}, {PatientRef: "Patient/synthetic", CoverageRef: "Coverage/synthetic", OrderRef: "Observation/order"}} {
		if _, err := composeDTRContextAtLine([]byte(contextQR), "2.2", qc); err == nil {
			t.Fatalf("accepted input %+v", qc)
		}
	}
	if _, err := composeDTRContextAtLine([]byte(contextQR), "9.9", contextInputs()); err == nil {
		t.Fatal("accepted unknown line")
	}
}

func TestDTRContextPreservesAdditionalContext(t *testing.T) {
	raw := strings.Replace(contextQR, `"extension":[`, `"meta":{"profile":["https://example.test/other-profile"]},"extension":[{"url":"`+lfDTR+`qr-context","valueReference":{"reference":"Encounter/other"}},`, 1)
	out, err := composeDTRContextAtLine([]byte(raw), "2.2", contextInputs())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`Encounter/other`)) || !bytes.Contains(out, []byte(`https://example.test/other-profile`)) {
		t.Fatal("lost unrelated context/profile")
	}
}

func TestDTRValidationTransportAndRefusals(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		t.Run(line, func(t *testing.T) {
			expected := map[string]string{"2.0": "2.0.1", "2.1": "2.1.0", "2.2": "2.2.0"}[line]
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				raw, _ := io.ReadAll(r.Body)
				if !bytes.Equal(raw, []byte(contextQR)) {
					t.Error("validator payload changed")
				}
				if r.URL.Query().Get("profile") != lfDTR+"dtr-questionnaireresponse|"+expected {
					t.Errorf("profile=%s", r.URL.Query().Get("profile"))
				}
				w.Header().Set("Content-Type", "application/fhir+json")
				io.WriteString(w, `{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"informational"}]}`)
			}))
			defer server.Close()
			g := &Gateway{cfg: Config{Validator: failIfCalledValidator{}, ValidatorsByLine: map[string]shnsdk.Validator{line: shnsdk.NewOperationValidator(server.URL)}, ConformanceEnforcement: EnforcementStrict}}
			if status, msg := g.validateDTRQuestionnaireResponse(context.Background(), []byte(contextQR), line); status != 503 || !strings.Contains(msg, "fhir.terminology") {
				t.Fatalf("real adapter must expose unavailable terminology: %d %s", status, msg)
			}
			if calls != 1 {
				t.Fatalf("calls=%d", calls)
			}
			if status, _ := g.validateDTRQuestionnaireResponse(context.Background(), []byte(contextQR), "9.9"); status != 500 {
				t.Fatalf("unknown line status=%d", status)
			}
			g.cfg.ValidatorsByLine = map[string]shnsdk.Validator{"missing": syntheticFakeValidator()}
			if status, _ := g.validateDTRQuestionnaireResponse(context.Background(), []byte(contextQR), line); status != 503 {
				t.Fatalf("unlaned status=%d", status)
			}
			g.cfg.ValidatorsByLine = map[string]shnsdk.Validator{line: syntheticLineValidator(line)}
			if status, _ := g.validateDTRQuestionnaireResponse(context.Background(), []byte(contextQR), line); status != 422 {
				t.Fatalf("invalid final status=%d", status)
			}
			server.Close()
			g.cfg.ValidatorsByLine = map[string]shnsdk.Validator{line: shnsdk.NewOperationValidator(server.URL)}
			if status, _ := g.validateDTRQuestionnaireResponse(context.Background(), []byte(contextQR), line); status != 503 {
				t.Fatalf("outage status=%d", status)
			}
		})
	}
}

func TestDTRContextOptionalOrders(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		for _, activeType := range []string{"ServiceRequest", "DeviceRequest"} {
			for _, optionalType := range []string{"ServiceRequest", "DeviceRequest"} {
				for _, position := range []string{"absent", "before", "after"} {
					t.Run(line+"/"+activeType+"/"+optionalType+"/"+position, func(t *testing.T) {
						qc := contextInputs()
						qc.OrderRef = activeType + "/active"
						optional := `{"url":"` + lfDTR + `qr-context","valueReference":{"reference":"` + optionalType + `/other","type":"` + optionalType + `","display":"Optional order","extension":[{"url":"https://example.test/precise","valueDecimal":9007199254740993.2300}]},"id":"retained"}`
						active := `{"url":"` + lfDTR + `qr-context","valueReference":{"reference":"` + qc.OrderRef + `"}}`
						extra := optional
						if position == "before" {
							extra = active + "," + optional
						}
						if position == "after" {
							extra = optional + "," + active
						}
						raw := []byte(strings.Replace(contextQR, `"extension":[`, `"extension":[`+extra+`,`, 1))
						before := bytes.Clone(raw)
						out, err := composeDTRContextAtLine(raw, line, qc)
						if err != nil {
							t.Fatal(err)
						}
						if !bytes.Equal(raw, before) || !bytes.Contains(out, []byte(optional)) {
							t.Fatalf("changed optional context or input: %s", out)
						}
						if bytes.Count(out, []byte(`"reference":"`+qc.OrderRef+`"`)) != 1 {
							t.Fatalf("active identity not present exactly once: %s", out)
						}
						again, err := composeDTRContextAtLine(out, line, qc)
						if err != nil || !bytes.Equal(out, again) {
							t.Fatalf("not idempotent: %v", err)
						}
					})
				}
			}
		}
	}
}

func TestDTRContextActiveOrderRefusals(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		for _, kind := range []string{"ServiceRequest", "DeviceRequest"} {
			qc := contextInputs()
			qc.OrderRef = kind + "/active"
			other := "DeviceRequest"
			if kind == other {
				other = "ServiceRequest"
			}
			active := `{"url":"` + lfDTR + `qr-context","valueReference":{"reference":"` + qc.OrderRef + `"}}`
			for name, extension := range map[string]string{
				"duplicate active":            active + "," + active,
				"active type disagrees":       `{"url":"` + lfDTR + `qr-context","valueReference":{"reference":"` + qc.OrderRef + `","type":"` + other + `"}}`,
				"optional type disagrees":     `{"url":"` + lfDTR + `qr-context","valueReference":{"reference":"` + kind + `/other","type":"` + other + `"}}`,
				"malformed optional identity": `{"url":"` + lfDTR + `qr-context","valueReference":{"reference":"` + kind + `/other/invalid"}}`,
			} {
				t.Run(line+"/"+kind+"/"+name, func(t *testing.T) {
					raw := []byte(strings.Replace(contextQR, `"extension":[`, `"extension":[`+extension+`,`, 1))
					before := bytes.Clone(raw)
					if _, err := composeDTRContextAtLine(raw, line, qc); err == nil {
						t.Fatal("accepted conflicting or malformed order assertion")
					}
					if !bytes.Equal(raw, before) {
						t.Fatal("mutated rejected input")
					}
				})
			}
		}
	}
}

func TestDTRLogicalContextRefusals(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		for _, value := range malformedDTRLogicalReferences() {
			t.Run(line+"/"+value, func(t *testing.T) {
				raw := appendAuthoredTestContext([]byte(contextQR), value)
				if _, err := composeDTRContextAtLine(raw, line, contextInputs()); err == nil {
					t.Fatal("accepted malformed logical identity")
				}
			})
		}
	}
}

func malformedDTRLogicalReferences() []string {
	return []string{
		`{"display":"Missing identity"}`, `{"type":"Encounter","identifier":null}`, `{"type":"Encounter","identifier":{}}`, `{"type":"Encounter","identifier":"bad"}`, `{"type":"Encounter","identifier":[]}`,
		`{"identifier":{"value":null}}`, `{"identifier":{"value":""}}`, `{"identifier":{"value":true}}`, `{"identifier":{"system":null,"value":"v"}}`, `{"identifier":{"system":"","value":"v"}}`, `{"identifier":{"system":3,"value":"v"}}`,
		`{"reference":null,"identifier":{"value":"v"}}`, `{"reference":"","identifier":{"value":"v"}}`, `{"reference":false,"identifier":{"value":"v"}}`, `{"type":null,"identifier":{"value":"v"}}`, `{"type":"","identifier":{"value":"v"}}`,
		`{"type":"Coverage","identifier":{"value":"v"}}`, `{"type":"ServiceRequest","identifier":{"value":"v"}}`, `{"type":"DeviceRequest","identifier":{"value":"v"}}`, `{"type":"http://hl7.org/fhir/StructureDefinition/Coverage","identifier":{"value":"v"}}`, `{"type":"http://hl7.org/fhir/StructureDefinition/DeviceRequest","identifier":{"value":"v"}}`,
		`{"identifier":{"value":"v","value":"other"}}`, `{"identifier":{"value":"v"},"identifier":{"value":"other"}}`,
	}
}

func TestDTRContextRejectsDotSegmentIdentities(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		for _, id := range []string{".", "..", ".clinical", "..clinical", "a..b", "a-b"} {
			reject := id == "." || id == ".."
			for _, kind := range []string{"ServiceRequest", "DeviceRequest"} {
				t.Run(line+"/optional/"+kind+"/"+id, func(t *testing.T) {
					raw := appendAuthoredTestContext([]byte(contextQR), `{"reference":"`+kind+`/`+id+`","type":"`+kind+`"}`)
					before := bytes.Clone(raw)
					_, err := composeDTRContextAtLine(raw, line, contextInputs())
					if (err != nil) != reject {
						t.Fatalf("error=%v reject=%t", err, reject)
					}
					if !bytes.Equal(raw, before) {
						t.Fatal("source changed")
					}
				})
			}
			for _, kind := range []string{"Patient", "Coverage", "ServiceRequest", "DeviceRequest"} {
				t.Run(line+"/active/"+kind+"/"+id, func(t *testing.T) {
					qc := contextInputs()
					raw := []byte(contextQR)
					ref := kind + "/" + id
					switch kind {
					case "Patient":
						qc.PatientRef = ref
						raw = bytes.ReplaceAll(raw, []byte("Patient/synthetic"), []byte(ref))
					case "Coverage":
						qc.CoverageRef = ref
					default:
						qc.OrderRef = ref
					}
					_, err := composeDTRContextAtLine(raw, line, qc)
					if (err != nil) != reject {
						t.Fatalf("error=%v reject=%t", err, reject)
					}
				})
			}
		}
	}
}
