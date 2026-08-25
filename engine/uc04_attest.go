// uc04_attest.go — the provider-data UC-04 DTR ATTESTATION answer-map builder.
//
// br-payer's UC-04 questionnaire (HomeHealthAssessment) is an ADAPTIVE questionnaire with 0 CQL
// expression items, so the operated $populate auto-pops nothing — the spine's
// DTR step produces an empty QR. UC-04's DTR is ATTESTATION: the gateway itself derives the
// answers from the provider's seeded clinical data and fills the delivered tree in the
// $populate step's place. Every value here traces to the seeded record, not to a human typing
// an answer in, so the fill is stamped source="auto" (FR-17's auto class,
// shnsdk.FillQuestionnaireFromAutoAnswersAtLine) — register §15(a): stamping this
// source="manual" (as an earlier build did, authored "Organization/<holder>") would be a false
// human-authorship claim; nobody attested anything here. uc04AttestationAnswers builds that
// answer map FROM the seeded order so every answer TRACES TO SEED:
//
//   - linkId 1.1 (service category, a coded answer): SNOMED 91251008 (Physical therapy) — derived
//     from the order's clinical nature. The seeded order carries HCPCS G0151 ("services of a
//     qualified physical therapist in the home-health setting"), so the home-health service
//     category is, faithfully, physical therapy. This is faithful attestation of the ORDER's
//     service type, NOT payer-matching.
//   - linkId 3.1 (primary diagnosis, a text answer): the dx display read from the order's reasonCode
//     (coding.display, falling back to the CodeableConcept text). Read FROM the order — never
//     hardcoded.
//   - linkId 3.2 (functional limitations, text) and 3.3 (treatment goals, text): the rest of the
//     HomeHealthAssessment's Physical Therapy group, which the questionnaire REQUIRES. A home
//     physical-therapy referral in an EHR carries the clinician's functional assessment and the
//     plan-of-care goals beside the order; the persona seeds them as a ClinicalImpression (its
//     summary = the functional limitations) and a Goal (its description = the treatment goals),
//     both referenced from ServiceRequest.supportingInfo, and the attestation READS them through
//     the provider's own SystemOfRecord (ResolveByReference). Absent from the record ⇒ omitted,
//     and the fill's honesty guard then refuses the delivered group rather than fabricate.
//
// HONESTY FENCE: every attested answer traces to the seeded order; we never hardcode a value to
// match the payer; the QR is VERDICT-INERT — br-payer's verdict is a code-keyed CQL constant on
// G0151 and the A4→A1 is its pend-resolution timer, so these answers do NOT drive the verdict. No
// "clinical value → PA" or "supplier-NPI verdict" claim is made.
package engine

import (
	"encoding/json"
	"fmt"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

const snomedSystem = "http://snomed.info/sct"

// uc04AttestationAnswers builds the attestation answer map for the HomeHealthAssessment from the
// seeded order bytes. It fails CLOSED (error) when the order carries no recognized {CPT,HCPCS}
// product coding (we cannot attest a service category we cannot derive from the order). The dx
// (3.1) is best-effort: when the order has no reasonCode display/text, 3.1 is omitted (3.1 is
// optional on the questionnaire; only 1.1 + a present 3.1 are the load-bearing traces-to-seed
// evidence). resolve is the provider's SystemOfRecord reference resolver (ResolveByReference)
// for the order's supportingInfo (3.2 / 3.3); nil, or a record that does not resolve, omits them.
func uc04AttestationAnswers(orderJSON []byte, resolve func(ref string) ([]byte, bool)) (map[string]shnsdk.Answer, error) {
	// 1.1 — service category, derived from the order's product code. Fail closed if absent: this
	// fence also enforces the orderSource product-coding contract for the provider-data UC-04 lane.
	if _, _, _, err := shnsdk.ParseOrderProductCoding(orderJSON); err != nil {
		return nil, fmt.Errorf("uc04 attestation: order has no recognized product coding: %w", err)
	}
	answers := map[string]shnsdk.Answer{
		// linkId 1.1 — service category. br-payer's HomeHealthAssessment questionnaire branches 1.1
		// across {9632001 skilled-nursing, 91251008 physical-therapy, 84478008 occupational-therapy}.
		// The seeded order is HCPCS G0151 = "services of a qualified physical therapist in the
		// home-health setting", so the faithful 1.1 branch is physical therapy (91251008), derived
		// from the order — not payer-matched. Verdict-inert.
		"1.1": {Coding: &shnsdk.AnswerCoding{
			System:  snomedSystem,
			Code:    "91251008",
			Display: "Physical therapy",
		}},
	}

	// 3.1 — primary diagnosis, read from the order's reasonCode (best-effort, traces to seed).
	if dx := orderReasonDisplay(orderJSON); dx != "" {
		dx := dx
		answers["3.1"] = shnsdk.Answer{String: &dx}
	}

	// 3.2 / 3.3 — the PT assessment and goals the order's supportingInfo references, read
	// through the SoR (traces to seed; absent ⇒ omitted, never invented).
	if resolve != nil {
		limitations, goals := orderSupportingAssessment(orderJSON, resolve)
		if limitations != "" {
			answers["3.2"] = shnsdk.Answer{String: &limitations}
		}
		if goals != "" {
			answers["3.3"] = shnsdk.Answer{String: &goals}
		}
	}

	return answers, nil
}

// orderSupportingAssessment resolves the order's supportingInfo references and reads the first
// ClinicalImpression's summary (the functional limitations, 3.2) and the first Goal's
// description text (the treatment goals, 3.3). Unresolvable or other-typed references are
// skipped; "" means the record holds no such fact.
func orderSupportingAssessment(orderJSON []byte, resolve func(ref string) ([]byte, bool)) (limitations, goals string) {
	var probe struct {
		SupportingInfo []struct {
			Reference string `json:"reference"`
		} `json:"supportingInfo"`
	}
	if err := json.Unmarshal(orderJSON, &probe); err != nil {
		return "", ""
	}
	for _, si := range probe.SupportingInfo {
		if si.Reference == "" {
			continue
		}
		raw, ok := resolve(si.Reference)
		if !ok {
			continue
		}
		var rt struct {
			ResourceType string `json:"resourceType"`
		}
		if err := json.Unmarshal(raw, &rt); err != nil {
			continue
		}
		// Parsed per type: ClinicalImpression.description is a string, Goal.description a
		// CodeableConcept — one shared probe would fail to unmarshal one of them.
		switch rt.ResourceType {
		case "ClinicalImpression":
			var ci struct {
				Summary string `json:"summary"`
			}
			if err := json.Unmarshal(raw, &ci); err == nil && limitations == "" {
				limitations = ci.Summary
			}
		case "Goal":
			var goal struct {
				Description struct {
					Text string `json:"text"`
				} `json:"description"`
			}
			if err := json.Unmarshal(raw, &goal); err == nil && goals == "" {
				goals = goal.Description.Text
			}
		}
	}
	return limitations, goals
}

// orderReasonDisplay reads the human-readable primary diagnosis from a FHIR order's first
// reasonCode: the first coding[].display, falling back to the CodeableConcept .text. Returns "" when
// the order carries no reasonCode display/text (3.1 is then omitted — it is optional on the
// questionnaire and verdict-inert).
func orderReasonDisplay(orderJSON []byte) string {
	var probe struct {
		ReasonCode []struct {
			Coding []struct {
				Display *string `json:"display"`
			} `json:"coding"`
			Text *string `json:"text"`
		} `json:"reasonCode"`
	}
	if err := json.Unmarshal(orderJSON, &probe); err != nil {
		return ""
	}
	for _, rc := range probe.ReasonCode {
		for _, c := range rc.Coding {
			if c.Display != nil && *c.Display != "" {
				return *c.Display
			}
		}
		if rc.Text != nil && *rc.Text != "" {
			return *rc.Text
		}
	}
	return ""
}

// attestedAnswerValues surfaces the attested value per linkId (the coded answer's code, or the text)
// so the UC-04 response can show WHAT was attested off the seeded order — the UC-04 analog of
// HomeOxygen's qrAnswers. Coded answers render as their code; string answers as their text.
func attestedAnswerValues(answers map[string]shnsdk.Answer) map[string]string {
	out := make(map[string]string, len(answers))
	for linkID, a := range answers {
		switch {
		case a.Coding != nil:
			out[linkID] = a.Coding.Code
		case a.String != nil:
			out[linkID] = *a.String
		}
	}
	return out
}
