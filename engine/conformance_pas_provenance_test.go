package engine

import (
	"context"
	"strings"
	"testing"
)

const pasProvenanceRuleValid = `{"resourceType":"Bundle","type":"collection","entry":[{"fullUrl":"https://source.example/fhir/Claim/amended","resource":{"resourceType":"Claim","id":"amended"}},{"fullUrl":"https://source.example/fhir/Claim/original","resource":{"resourceType":"Claim","id":"original"}},{"fullUrl":"https://source.example/fhir/QuestionnaireResponse/supplement","resource":{"resourceType":"QuestionnaireResponse","id":"supplement"}},{"fullUrl":"https://source.example/fhir/Provenance/proof","resource":{"resourceType":"Provenance","id":"proof","target":[{"reference":"QuestionnaireResponse/supplement"}],"agent":[{"who":{"reference":"Practitioner/author"}}]}}]}`

// PCV-04/FR-32: source-amendment provenance must identify a real supplemental
// entry within the producer's own Bundle namespace and a usable agent.
func TestDeepPASProvenanceSourceAndNamespace(t *testing.T) {
	valid := pasProvenanceRuleValid
	const foreign = `https://foreign.example/fhir/QuestionnaireResponse/supplement`
	for _, tc := range []struct {
		name string
		body string
		want CheckState
	}{
		{"valid", valid, CheckValid},
		{"urn exact fullUrl", strings.Replace(strings.Replace(valid, `https://source.example/fhir/Provenance/proof`, `urn:uuid:ab12`, 1), `"reference":"QuestionnaireResponse/supplement"`, `"reference":"https://source.example/fhir/QuestionnaireResponse/supplement"`, 1), CheckValid},
		{"urn exact fullUrl without optional resource id", strings.Replace(strings.Replace(strings.Replace(valid, `https://source.example/fhir/Provenance/proof`, `urn:uuid:ab12`, 1), `"reference":"QuestionnaireResponse/supplement"`, `"reference":"https://source.example/fhir/QuestionnaireResponse/supplement"`, 1), `"id":"proof",`, "", 1), CheckValid},
		{"duplicate supplemental fullUrl", strings.Replace(valid, `,{"fullUrl":"https://source.example/fhir/Provenance/proof"`, `,{"fullUrl":"https://source.example/fhir/QuestionnaireResponse/supplement","resource":{"resourceType":"Organization","id":"collision"}},{"fullUrl":"https://source.example/fhir/Provenance/proof"`, 1), CheckUnavailable},
		{"relative target from foreign owner base", strings.Replace(valid, `https://source.example/fhir/Provenance/proof`, `https://foreign.example/fhir/Provenance/proof`, 1), CheckInvalid},
		{"missing provenance", strings.Replace(valid, `,{"fullUrl":"https://source.example/fhir/Provenance/proof","resource":{"resourceType":"Provenance","id":"proof","target":[{"reference":"QuestionnaireResponse/supplement"}],"agent":[{"who":{"reference":"Practitioner/author"}}]}}`, "", 1), CheckInvalid},
		{"no agent", strings.Replace(valid, `"agent":[{"who":{"reference":"Practitioner/author"}}]`, `"agent":[]`, 1), CheckInvalid},
		{"wrong target", strings.Replace(valid, `"reference":"QuestionnaireResponse/supplement"`, `"reference":"QuestionnaireResponse/other"`, 1), CheckInvalid},
		{"foreign namespace same suffix", strings.Replace(valid, `"reference":"QuestionnaireResponse/supplement"`, `"reference":"`+foreign+`"`, 1), CheckInvalid},
		{"unreadable target", strings.Replace(valid, `"target":[{"reference":"QuestionnaireResponse/supplement"}]`, `"target":"bad"`, 1), CheckUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := deepInput(tc.body)
			in.Exchange.legType = "pas-claim-update"
			in.DeclaredVersion = "pa.pas@2.2"
			rule := deepRule(t, &Gateway{}, "pas.provenance")
			if !rule.Applies(in) {
				t.Fatal("amendment provenance rule did not apply")
			}
			if got := rule.Check(context.Background(), in); got.State != tc.want {
				t.Fatalf("state = %s, want %s: %+v", got.State, tc.want, got)
			}
		})
	}
}
