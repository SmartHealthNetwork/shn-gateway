package engine

import (
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// G0151 ServiceRequest with an ICD-10 reasonCode (display) → the attestation answer map
// has 1.1 = SNOMED 91251008 (Physical therapy, derived from the order's G0151 product
// code — faithful attestation of the order's service type) and 3.1 = the order's dx
// display read FROM the order (never hardcoded). The QR is verdict-inert (this only
// proves the answers trace to the seeded order).
func TestUC04AttestationAnswers_FromSeededOrder(t *testing.T) {
	order := []byte(`{
		"resourceType":"ServiceRequest","id":"sr-pd-uc04","status":"draft","intent":"order",
		"code":{"coding":[{"system":"http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets","code":"G0151","display":"Services performed by a qualified physical therapist in the home health setting"}]},
		"reasonCode":[{"coding":[{"system":"http://hl7.org/fhir/sid/icd-10-cm","code":"I63.9","display":"Cerebral infarction, unspecified"}]}],
		"subject":{"reference":"Patient/MBR-PD-UC04"}
	}`)
	answers, err := uc04AttestationAnswers(order)
	if err != nil {
		t.Fatalf("uc04AttestationAnswers: unexpected error: %v", err)
	}

	// 1.1 — service category, a coded answer derived from the order's G0151 (PT).
	a11, ok := answers["1.1"]
	if !ok || a11.Coding == nil {
		t.Fatalf("answers[1.1] missing or not a coding: %+v", a11)
	}
	if a11.Coding.Code != "91251008" {
		t.Fatalf("answers[1.1] code = %q, want 91251008 (Physical therapy)", a11.Coding.Code)
	}
	if a11.Coding.System != "http://snomed.info/sct" {
		t.Fatalf("answers[1.1] system = %q, want SNOMED", a11.Coding.System)
	}

	// 3.1 — primary diagnosis, a text answer read FROM the order's reasonCode (not hardcoded).
	a31, ok := answers["3.1"]
	if !ok || a31.String == nil {
		t.Fatalf("answers[3.1] missing or not a string: %+v", a31)
	}
	if *a31.String != "Cerebral infarction, unspecified" {
		t.Fatalf("answers[3.1] = %q, want the order's dx display", *a31.String)
	}
}

// A reasonCode that carries only text (no coding.display) is still attested from the order.
func TestUC04AttestationAnswers_DxFromReasonText(t *testing.T) {
	order := []byte(`{
		"resourceType":"ServiceRequest","id":"sr-pd-uc04","status":"draft","intent":"order",
		"code":{"coding":[{"system":"http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets","code":"G0151"}]},
		"reasonCode":[{"text":"Cerebral infarction"}],
		"subject":{"reference":"Patient/MBR-PD-UC04"}
	}`)
	answers, err := uc04AttestationAnswers(order)
	if err != nil {
		t.Fatalf("uc04AttestationAnswers: unexpected error: %v", err)
	}
	a31, ok := answers["3.1"]
	if !ok || a31.String == nil || *a31.String != "Cerebral infarction" {
		t.Fatalf("answers[3.1] = %+v, want text 'Cerebral infarction' from reasonCode.text", a31)
	}
}

// An order with no recognized product coding fails closed (the honesty fence: never
// attest a service category we cannot derive from the order).
func TestUC04AttestationAnswers_NoProductCoding_Errors(t *testing.T) {
	order := []byte(`{
		"resourceType":"ServiceRequest","id":"sr-x","status":"draft","intent":"order",
		"code":{"coding":[{"system":"http://example.org/other","code":"ZZZ"}]},
		"reasonCode":[{"coding":[{"display":"X"}]}],
		"subject":{"reference":"Patient/MBR-PD-UC04"}
	}`)
	if _, err := uc04AttestationAnswers(order); err == nil {
		t.Fatalf("expected an error for an order with no {CPT,HCPCS} product coding, got nil")
	}
}

// attestedAnswerValues surfaces the coded value (or the text) per linkId so the response
// can show what was attested (the traces-to-seed evidence, the UC-04 analog of HomeOxygen's
// qrAnswers).
func TestAttestedAnswerValues(t *testing.T) {
	answers := map[string]shnsdk.Answer{
		"1.1": {Coding: &shnsdk.AnswerCoding{System: "http://snomed.info/sct", Code: "91251008", Display: "Physical therapy"}},
		"3.1": {String: strPtr("Cerebral infarction, unspecified")},
	}
	vals := attestedAnswerValues(answers)
	if vals["1.1"] != "91251008" {
		t.Fatalf("attestedAnswerValues[1.1] = %q, want 91251008", vals["1.1"])
	}
	if vals["3.1"] != "Cerebral infarction, unspecified" {
		t.Fatalf("attestedAnswerValues[3.1] = %q, want the dx text", vals["3.1"])
	}
}
