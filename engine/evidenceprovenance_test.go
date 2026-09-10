package engine

import (
	"encoding/json"
	"testing"
	"time"
)

func TestEvidenceProvenanceUsesBusinessIdentity(t *testing.T) {
	for _, tc := range []struct{ system, value string }{{"http://hl7.org/fhir/sid/us-npi", "1234567890"}, {"http://smarthealth.network/ids/holder", "metro-spine"}} {
		raw, err := buildEvidenceProvenance("DiagnosticReport/d", tc.system, tc.value, "Consent/c", "TREAT", time.Unix(0, 0))
		if err != nil {
			t.Fatal(err)
		}
		var p struct {
			Agent []struct {
				Who struct {
					Reference  string                         `json:"reference"`
					Identifier struct{ System, Value string } `json:"identifier"`
				}
			}
			Policy []string
			Target []struct{ Reference string }
		}
		if err = json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		if len(p.Agent) != 1 || p.Agent[0].Who.Reference != "" || p.Agent[0].Who.Identifier.System != tc.system || p.Agent[0].Who.Identifier.Value != tc.value || len(p.Policy) != 1 || p.Policy[0] != "Consent/c" || p.Target[0].Reference != "DiagnosticReport/d" {
			t.Fatalf("wrong identity/provenance: %s", raw)
		}
	}
	for _, value := range []string{"", " "} {
		if _, err := buildEvidenceProvenance("DiagnosticReport/d", "http://hl7.org/fhir/sid/us-npi", value, "", "", time.Unix(0, 0)); err == nil {
			t.Fatal("accepted absent identity")
		}
	}
}

func TestUpdateEvidenceRecognizesLogicalAgents(t *testing.T) {
	for _, tc := range []struct {
		system, value string
		want          bool
	}{
		{"http://hl7.org/fhir/sid/us-npi", "1234567890", true},
		{"http://smarthealth.network/ids/holder", "metro-spine", true},
		{"http://hl7.org/fhir/sid/us-npi", "", false},
		{"http://hl7.org/fhir/sid/us-npi", " ", false},
		{"", "1234567890", false},
		{"https://unrecognized.example/identity", "1234567890", false},
	} {
		raw, _ := json.Marshal(map[string]any{"resourceType": "Bundle", "entry": []any{map[string]any{"resource": map[string]any{"resourceType": "Provenance", "agent": []any{map[string]any{"who": map[string]any{"identifier": map[string]string{"system": tc.system, "value": tc.value}}}}}}}})
		facts, status, msg := parseConformantPASUpdateFacts(raw)
		if status != 0 {
			t.Fatalf("%d %s", status, msg)
		}
		if (len(facts.provenanceAgents) > 0) != tc.want {
			t.Errorf("system=%q value=%q agents=%v", tc.system, tc.value, facts.provenanceAgents)
		}
	}
}

func TestUpdateEvidenceRejectsMalformedLogicalAgent(t *testing.T) {
	for _, who := range []string{`{"identifier":"not-an-Identifier"}`, `{"identifier":{"system":9,"value":"123"}}`, `{"identifier":{"system":"http://hl7.org/fhir/sid/us-npi","value":9}}`} {
		raw := []byte(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Provenance","agent":[{"who":` + who + `}]}}]}`)
		_, status, _ := parseConformantPASUpdateFacts(raw)
		if status != 400 {
			t.Errorf("who=%s status=%d", who, status)
		}
	}
}
