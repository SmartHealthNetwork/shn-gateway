package engine

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestInspectNativePASResponsePreservesIngressContract(t *testing.T) {
	approved := pasBundleWithResponse(t, []byte(assemblyRealPending), []byte(assemblyRealTerminal))
	for name, b := range map[string][]byte{"pending": []byte(assemblyRealPending), "approved": approved} {
		facts, err := InspectNativePASResponse(b)
		if err != nil || !facts.ReferencesComplete || facts.ClaimResponseID == "" || facts.Decision == "" {
			t.Fatalf("%s: %+v %v", name, facts, err)
		}
		got, result := validateNativePASResponse(b)
		if result.Status != 0 || !bytes.Equal(got, b) {
			t.Fatal(result)
		}
	}
	// A ClaimResponse the payer placed under a urn:uuid fullUrl without an id is
	// identified by that fullUrl; the facts carry the payer's own identifier and
	// the fullUrl, and no id is minted for it.
	b := assemblySmallGraph()
	e := b["entry"].([]any)[0].(map[string]any)
	e["fullUrl"] = "urn:uuid:10000000-0000-4000-8000-000000000001"
	cr := e["resource"].(map[string]any)
	delete(cr, "id")
	cr["identifier"] = []any{map[string]any{"system": "https://payer.test/pa", "value": "PA-7781"}}
	cr["outcome"] = "queued"
	cr["patient"] = map[string]any{"reference": "https://payer.test/fhir/Patient/p"}
	cr["request"] = map[string]any{"reference": "https://payer.test/fhir/Claim/c"}
	cr["item"] = []any{map[string]any{"itemSequence": 1, "adjudication": []any{map[string]any{
		"category":  map[string]any{"coding": []any{map[string]any{"system": "http://terminology.hl7.org/CodeSystem/adjudication", "code": "submitted"}}},
		"extension": []any{map[string]any{"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewAction", "extension": []any{map[string]any{"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewActionCode", "valueCodeableConcept": map[string]any{"coding": []any{map[string]any{"system": "https://codesystem.x12.org/005010/306", "code": "A4"}}}}}}},
	}}}}
	idless, _ := json.Marshal(b)
	facts, err := InspectNativePASResponse(idless)
	if err != nil {
		t.Fatalf("id-less ClaimResponse under urn:uuid refused: %v", err)
	}
	if facts.Decision != "pending" || facts.ClaimResponseID != "" || facts.ClaimResponseIdentity != "urn:uuid:10000000-0000-4000-8000-000000000001" || facts.ClaimResponseIdentifier != "https://payer.test/pa|PA-7781" || !facts.ReferencesComplete {
		t.Fatalf("facts do not carry the payer's own identity: %+v", facts)
	}
	if got, result := validateNativePASResponse(idless); result.Status != 0 || !bytes.Equal(got, idless) {
		t.Fatalf("id-less answer not relayed unchanged: %+v", result)
	}
	withID := pasBundleWithResponse(t, []byte(assemblyRealPending), []byte(assemblyRealTerminal))
	facts, err = InspectNativePASResponse(withID)
	if err != nil || facts.ClaimResponseID != "1770" || facts.ClaimResponseIdentity != "http://localhost:8081/fhir/ClaimResponse/1770" || facts.ClaimResponseIdentifier != "http://example.org/PATIENT_EVENT_TRACE_NUMBER|2ca2e10c-e924-4e12-9d69-5b71e4e91c31" {
		t.Fatalf("facts for an id-bearing ClaimResponse: %+v %v", facts, err)
	}
	for _, b := range [][]byte{[]byte(`{}`), []byte(`{"resourceType":"ClaimResponse","id":"bare","outcome":"queued"}`)} {
		if _, err := InspectNativePASResponse(b); err == nil {
			t.Fatal("accepted invalid graph")
		}
		if _, r := validateNativePASResponse(b); r.Status != 502 {
			t.Fatal(r)
		}
	}
}
