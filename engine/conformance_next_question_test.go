package engine

import (
	"bytes"
	"context"
	"encoding/json"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"testing"
)

// PCV-04: select published profiles from the independently declared DTR line.
// The spy proves target selection and byte ownership, not live IG validity.
func TestDeepNextQuestionPublishedTargets(t *testing.T) {
	for _, row := range []struct{ line, version, base, in, out, qr string }{
		{"2.0", "3.0.0", "http://hl7.org/fhir/uv/sdc/StructureDefinition/", "parameters-questionnaire-next-question-in", "parameters-questionnaire-next-question-out", "sdc-questionnaireresponse-adapt"},
		{"2.1", "2.1.0", "http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/", "dtr-next-question-input-parameters", "dtr-next-question-output-parameters", "dtr-questionnaireresponse"},
		{"2.2", "2.2.0", "http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/", "dtr-next-question-input-parameters", "dtr-next-question-output-parameters", "dtr-questionnaireresponse-adapt"},
	} {
		for _, direction := range []string{"request", "response"} {
			for _, wrapped := range []bool{false, true} {
				t.Run(row.line+"/"+direction+"/"+map[bool]string{false: "bare", true: "wrapped"}[wrapped], func(t *testing.T) {
					body := ` {"resourceType":"QuestionnaireResponse", "status":"in-progress"} `
					profile := row.qr
					if wrapped {
						name := "questionnaire-response"
						profile = row.in
						if direction == "response" {
							name = "return"
							profile = row.out
						}
						body = ` {"resourceType":"Parameters", "parameter":[{"name":"` + name + `","resource":` + body + `}]} `
					}
					in := deepInput(body)
					in.Direction = direction
					in.Status = 200
					in.Exchange.legType = "dtr-questionnaire-fetch"
					in.Exchange.operation = shnsdk.FrameOperationNextQuestion
					in.DeclaredVersion = "pa.dtr@" + row.line
					targets, contract, line, ok := deepValidationTargets(in)
					want := row.base + profile + "|" + row.version
					if !ok || contract != "pa.dtr" || line != row.line || len(targets) != 1 {
						t.Fatalf("no target: %v %s %s %+v", ok, contract, line, targets)
					}
					if targets[0].profile != want || !bytes.Equal(targets[0].raw, []byte(body)) {
						t.Fatalf("wrong profile or altered bytes: %+v want %s", targets[0], want)
					}
					calls := 0
					g := &Gateway{cfg: Config{Validator: observationValidator(func(_ context.Context, b []byte, p string) (shnsdk.ValidationEvidence, error) {
						calls++
						if p != want || !bytes.Equal(b, []byte(body)) {
							t.Fatalf("checker target %s %s", p, b)
						}
						return *syntheticEvidence(), nil
					})}}
					if got := deepRule(t, g, "fhir.profile").Check(context.Background(), in); got.State != CheckValid || calls != 1 {
						t.Fatalf("check %+v calls=%d", got, calls)
					}
				})
			}
		}
	}
}

func TestNextQuestionDirectionalEnvelope(t *testing.T) {
	for _, direction := range []string{"request", "response"} {
		for _, name := range []string{"questionnaire-response", "return"} {
			body := `{"resourceType":"Parameters","parameter":[{"name":"` + name + `","resource":{"resourceType":"QuestionnaireResponse"}}]}`
			in := structuralInput("dtr-questionnaire-fetch", shnsdk.FrameOperationNextQuestion, direction, body)
			want := CheckInvalid
			if (direction == "request" && name == "questionnaire-response") || (direction == "response" && name == "return") {
				want = CheckValid
			}
			for _, rule := range StructuralRules() {
				if rule.ID == "dtr.next-question."+direction {
					if got := rule.Check(context.Background(), in); got.State != want {
						t.Fatalf("%s/%s: %+v want %s", direction, name, got, want)
					}
				}
			}
		}
	}
}

func TestNextQuestionPublishedResponseConsumption(t *testing.T) {
	body := bytes.Replace(nextQuestionAnswer(t, "Patient/a", rawItems(t, adaptiveTree(t, "1"))), []byte(`"name":"questionnaire-response"`), []byte(`"name":"return"`), 1)
	qr, items, err := parseNextQuestionResponse(body)
	if err != nil || len(qr) == 0 || len(items) == 0 {
		t.Fatalf("published response unreadable: %s %v", qr, err)
	}
}

func TestNextQuestionProfileEnforcementModes(t *testing.T) {
	for _, direction := range []string{"request", "response"} {
		for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementBasic, EnforcementStrict} {
			for _, state := range []shnsdk.ValidationState{shnsdk.ValidationValid, shnsdk.ValidationInvalid, shnsdk.ValidationUnavailable} {
				in := deepInput(`{"resourceType":"QuestionnaireResponse"}`)
				in.Direction = direction
				in.Status = 200
				in.DeclaredVersion = "pa.dtr@2.2"
				in.Exchange.legType = "dtr-questionnaire-fetch"
				in.Exchange.operation = shnsdk.FrameOperationNextQuestion
				in.Exchange.policy = NewConformancePolicy(level)
				calls := 0
				g := &Gateway{cfg: Config{Validator: observationValidator(func(context.Context, []byte, string) (shnsdk.ValidationEvidence, error) {
					calls++
					ev := *syntheticEvidence()
					ev.Profile.State = state
					return ev, nil
				})}}
				err := g.enforceRules(context.Background(), in, []ConformanceRule{deepRule(t, g, "fhir.profile")})
				if level != EnforcementStrict {
					if err != nil || calls != 0 {
						t.Fatalf("optional synchronous check: %v calls=%d", err, calls)
					}
					continue
				}
				if calls != 1 {
					t.Fatalf("strict executions=%d", calls)
				}
				if state == shnsdk.ValidationValid {
					if err != nil {
						t.Fatal(err)
					}
					continue
				}
				code := 503
				if state == shnsdk.ValidationInvalid {
					code = 422
					if direction == "response" {
						code = 502
					}
				}
				wantStructuralError(t, err, code, "fhir.profile")
			}
		}
	}
}

func TestNextQuestionProfileUnsupported(t *testing.T) {
	for _, row := range []struct{ version, direction, operation, body string }{
		{"", "request", shnsdk.FrameOperationNextQuestion, `{"resourceType":"QuestionnaireResponse"}`},
		{"pa.dtr@9.0", "request", shnsdk.FrameOperationNextQuestion, `{"resourceType":"QuestionnaireResponse"}`},
		{"pa.dtr@2.0", "unknown", shnsdk.FrameOperationNextQuestion, `{"resourceType":"QuestionnaireResponse"}`},
		{"pa.dtr@2.0", "request", "unknown", `{"resourceType":"QuestionnaireResponse"}`},
		{"pa.dtr@2.0", "response", shnsdk.FrameOperationNextQuestion, `{"resourceType":"Questionnaire"}`},
	} {
		in := deepInput(row.body)
		in.Direction = row.direction
		in.DeclaredVersion = row.version
		in.Exchange.legType = "dtr-questionnaire-fetch"
		in.Exchange.operation = row.operation
		if targets, _, _, ok := deepValidationTargets(in); ok {
			t.Fatalf("invented target for %+v: %+v", row, targets)
		}
	}
}

func TestNextQuestionResponseAmbiguousOutputRefused(t *testing.T) {
	// A legacy alias and the published return cannot select two competing results.
	body := nextQuestionAnswer(t, "Patient/a", rawItems(t, adaptiveTree(t, "1")))
	var top struct {
		ResourceType string            `json:"resourceType"`
		Parameter    []json.RawMessage `json:"parameter"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatal(err)
	}
	top.Parameter = append(top.Parameter, bytes.Replace(top.Parameter[0], []byte(`"name":"questionnaire-response"`), []byte(`"name":"return"`), 1))
	body, err := json.Marshal(top)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := parseNextQuestionResponse(body); err == nil {
		t.Fatal("ambiguous next-question response consumed")
	}
}
