package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestPASRequestEvidenceClosure(t *testing.T) {
	body := []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"fullUrl":"https://example.test/fhir/Patient/p","resource":{"resourceType":"Patient","id":"p"}},{"fullUrl":"https://example.test/fhir/ServiceRequest/s","resource":{"resourceType":"ServiceRequest","id":"s","subject":{"reference":"Patient/p"},"supportingInfo":[{"reference":"ClinicalImpression/c"}]}}]}`)
	good := []byte(`{"resourceType":"ClinicalImpression","id":"c","subject":{"reference":"Patient/p"},"problem":[{"reference":"Condition/d"}]}`)
	for _, tc := range []struct {
		name     string
		clinical []byte
		found    bool
		wantErr  bool
	}{
		{"selected transitive evidence", good, true, false},
		{"missing", nil, false, true},
		{"wrong patient", []byte(`{"resourceType":"ClinicalImpression","id":"c","subject":{"reference":"Patient/other"}}`), true, true},
		{"wrong identity", []byte(`{"resourceType":"ClinicalImpression","id":"other","subject":{"reference":"Patient/p"}}`), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			got, err := retainPASRequestEvidence(context.Background(), body, func(_ context.Context, ref string) ([]byte, bool, error) {
				calls = append(calls, ref)
				if ref == "ClinicalImpression/c" {
					return tc.clinical, tc.found, nil
				}
				if ref == "Condition/d" {
					return []byte(`{"resourceType":"Condition","id":"d","subject":{"reference":"Patient/p"},"evidence":[{"detail":[{"reference":"ClinicalImpression/c"}]}]}`), true, nil
				}
				return nil, false, fmt.Errorf("unexpected read")
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v", err)
			}
			if err == nil {
				var b struct {
					Entry []json.RawMessage `json:"entry"`
				}
				json.Unmarshal(got, &b)
				if len(b.Entry) != 4 || len(calls) != 2 {
					t.Fatalf("entries=%d reads=%v", len(b.Entry), calls)
				}
			}
		})
	}
}

func TestPASRequestEvidenceRefusals(t *testing.T) {
	makeBody := func(ref string) []byte {
		return []byte(fmt.Sprintf(`{"resourceType":"Bundle","type":"collection","entry":[{"fullUrl":"https://example.test/fhir/Patient/p","resource":{"resourceType":"Patient","id":"p"}},{"fullUrl":"https://example.test/fhir/ServiceRequest/s","resource":{"resourceType":"ServiceRequest","id":"s","subject":{"reference":"Patient/p"},"supportingInfo":[{"reference":%q}]}}]}`, ref))
	}
	for _, ref := range []string{"https://other.test/Condition/c", "Condition/c?x=1", "Condition%2Fc", "Condition/c/_history/1", "Organization/o", "Condition/../c"} {
		t.Run(ref, func(t *testing.T) {
			calls := 0
			_, err := retainPASRequestEvidence(context.Background(), makeBody(ref), func(context.Context, string) ([]byte, bool, error) { calls++; return nil, false, nil })
			if err == nil || calls != 0 {
				t.Fatalf("error=%v reads=%d", err, calls)
			}
		})
	}
	t.Run("read failure is sanitized", func(t *testing.T) {
		_, err := retainPASRequestEvidence(context.Background(), makeBody("Condition/c"), func(context.Context, string) ([]byte, bool, error) {
			return nil, false, fmt.Errorf("private backend credentials")
		})
		if err == nil || strings.Contains(err.Error(), "private") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("resource budget", func(t *testing.T) {
		calls := 0
		_, err := retainPASRequestEvidence(context.Background(), makeBody("Condition/c0"), func(_ context.Context, ref string) ([]byte, bool, error) {
			calls++
			return []byte(fmt.Sprintf(`{"resourceType":"Condition","id":%q,"subject":{"reference":"Patient/p"},"evidence":[{"detail":[{"reference":"Condition/c%d"}]}]}`, strings.TrimPrefix(ref, "Condition/"), calls)), true, nil
		})
		if err == nil || calls != pasGraphMaxResources-2 {
			t.Fatalf("error=%v reads=%d", err, calls)
		}
	})
	t.Run("byte budget", func(t *testing.T) {
		_, err := retainPASRequestEvidence(context.Background(), makeBody("Condition/c"), func(context.Context, string) ([]byte, bool, error) {
			return []byte(strings.Repeat(" ", pasGraphMaxBytes)), true, nil
		})
		if err == nil {
			t.Fatal("accepted oversized evidence")
		}
	})
	t.Run("reference budget", func(t *testing.T) {
		var b map[string]any
		json.Unmarshal(makeBody("Condition/c"), &b)
		sr := b["entry"].([]any)[1].(map[string]any)["resource"].(map[string]any)
		refs := make([]any, pasGraphMaxReferences+1)
		for i := range refs {
			refs[i] = map[string]any{"reference": "Patient/p"}
		}
		sr["supportingInfo"] = refs
		raw, _ := json.Marshal(b)
		_, err := retainPASRequestEvidence(context.Background(), raw, func(context.Context, string) ([]byte, bool, error) {
			t.Fatal("unexpected read")
			return nil, false, nil
		})
		if err == nil {
			t.Fatal("accepted reference overflow")
		}
	})
	t.Run("depth budget", func(t *testing.T) {
		raw := []byte(`{"resourceType":"Bundle","nested":` + strings.Repeat(`[`, pasGraphMaxDepth+1) + `0` + strings.Repeat(`]`, pasGraphMaxDepth+1) + `}`)
		_, err := retainPASRequestEvidence(context.Background(), raw, nil)
		if err == nil {
			t.Fatal("accepted depth overflow")
		}
	})
}

func TestPASRequestRejectsOtherPatientContainedEvidence(t *testing.T) {
	body := []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"fullUrl":"https://example.test/fhir/Claim/q","resource":{"resourceType":"Claim","id":"q","patient":{"reference":"Patient/p"}}},{"fullUrl":"https://example.test/fhir/Patient/p","resource":{"resourceType":"Patient","id":"p"}},{"fullUrl":"https://example.test/fhir/Coverage/v","resource":{"resourceType":"Coverage","id":"v","beneficiary":{"reference":"Patient/p"}}},{"fullUrl":"https://example.test/fhir/ServiceRequest/s","resource":{"resourceType":"ServiceRequest","id":"s","subject":{"reference":"Patient/p"},"supportingInfo":[{"reference":"Condition/c"}]}}]}`)
	evidence := []byte(`{"resourceType":"Condition","id":"c","subject":{"reference":"Patient/p"},"contained":[{"resourceType":"Patient","id":"other"},{"resourceType":"Observation","id":"o","status":"final","code":{"text":"Other patient's result"},"subject":{"reference":"#other"},"valueString":"Other patient's clinical data"}],"evidence":[{"detail":[{"reference":"#o"}]}]}`)
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprintf("foreign=%v", foreign), func(t *testing.T) {
			selected := evidence
			if !foreign {
				var resource map[string]any
				if err := json.Unmarshal(evidence, &resource); err != nil {
					t.Fatal(err)
				}
				observation := resource["contained"].([]any)[1].(map[string]any)
				observation["subject"] = map[string]any{"reference": "Patient/p"}
				resource["contained"] = []any{observation}
				selected, _ = json.Marshal(resource)
			}
			got, err := retainPASRequestEvidence(context.Background(), body, func(context.Context, string) ([]byte, bool, error) { return selected, true, nil })
			if (err != nil) != foreign {
				_, status, msg := parseConformantPASSubjects(got)
				t.Fatalf("foreign=%v error=%v; subsequent request guard status=%d msg=%q", foreign, err, status, msg)
			}
		})
	}
}
