package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// eobSourceClaim is a request Claim carrying what a decision EOB reads from
// it: type, provider, insurance and an item coding the order.
func eobSourceClaim(member, system, code string) string {
	return `{"resourceType":"Claim","use":"preauthorization","patient":{"reference":"Patient/` + member + `"},` +
		`"type":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/claim-type","code":"professional"}]},` +
		`"provider":{"reference":"Organization/prov-1"},` +
		`"insurance":[{"sequence":1,"focal":true,"coverage":{"reference":"Coverage/cov-1"}}],` +
		`"item":[{"sequence":1,"productOrService":{"coding":[{"system":"` + system + `","code":"` + code + `"}]}}]}`
}

const (
	eobRowCPT     = "http://www.ama-assn.org/go/cpt"
	eobRowType    = `{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/claim-type","code":"institutional","display":"Institutional"}],"text":"as the provider wrote it"}`
	eobRowCreated = "2026-09-30T08:15:00-05:00"
)

// eobRowRequest is a request Bundle whose Claim states every EOB source,
// edited by mutate (nil keeps it whole).
func eobRowRequest(t *testing.T, mutate func(claim map[string]any)) []byte {
	t.Helper()
	claim := map[string]any{
		"resourceType": "Claim", "use": "preauthorization",
		"patient":  map[string]any{"reference": "Patient/MBR-COVERED"},
		"type":     json.RawMessage(eobRowType),
		"provider": map[string]any{"reference": "Practitioner/pr-7", "display": "Dr. Rivera"},
		"insurance": []any{
			map[string]any{"sequence": 1, "focal": true, "coverage": map[string]any{"reference": "Coverage/cov-7"}},
			map[string]any{"sequence": 2, "focal": false, "coverage": map[string]any{"reference": "Coverage/cov-8"}},
		},
		"item": []any{
			map[string]any{"sequence": 2, "productOrService": map[string]any{"coding": []any{map[string]any{"system": eobRowCPT, "code": "99999"}}}},
			map[string]any{"sequence": 3, "net": json.RawMessage(`{"value":412.30,"currency":"USD"}`),
				"productOrService": map[string]any{"coding": []any{map[string]any{"system": eobRowCPT, "code": "72148"}}}},
		},
	}
	if mutate != nil {
		mutate(claim)
	}
	b, err := json.Marshal(map[string]any{"resourceType": "Bundle", "type": "collection", "entry": []any{
		map[string]any{"fullUrl": "urn:uuid:claim", "resource": claim},
		map[string]any{"fullUrl": "https://provider.test/fhir/Practitioner/pr-9", "resource": map[string]any{"resourceType": "Practitioner", "id": "pr-9",
			"identifier": []any{map[string]any{"system": "http://provider.test/staff", "value": "S-9"}, map[string]any{"system": "http://hl7.org/fhir/sid/us-npi", "value": "1234567893"}}}},
		map[string]any{"fullUrl": "urn:uuid:cov-mb", "resource": map[string]any{"resourceType": "Coverage",
			"identifier": []any{map[string]any{"system": "http://payer.test/plan", "value": "PLAN-1"},
				map[string]any{"type": map[string]any{"coding": []any{map[string]any{"system": "http://terminology.hl7.org/CodeSystem/v2-0203", "code": "MB"}}}, "system": "http://payer.test/member", "value": "M-77"}}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// eobRowClaimResponse is the payer's ClaimResponse stating every EOB source,
// edited by mutate (nil keeps it whole).
func eobRowClaimResponse(t *testing.T, mutate func(cr map[string]any)) []byte {
	t.Helper()
	cr := map[string]any{
		"resourceType": "ClaimResponse", "outcome": "complete", "created": eobRowCreated,
		"insurer": map[string]any{"reference": "Organization/ins-1"},
		"item":    []any{map[string]any{"itemSequence": 3, "adjudication": []any{map[string]any{"category": map[string]any{"coding": []any{map[string]any{"code": "submitted"}}}}}}},
	}
	if mutate != nil {
		mutate(cr)
	}
	b, err := json.Marshal(cr)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func eobRowBuild(t *testing.T, request, claimResponse []byte, parsed shnsdk.PriorAuthResult) ([]byte, error) {
	t.Helper()
	return decisionEOB("corr-row", "Patient/MBR-COVERED", eobRowCPT, "72148", "MRI", parsed,
		decisionEOBSource{request: request, itemIndex: -1, claimResponse: claimResponse})
}

func eobField(t *testing.T, eob []byte, path ...string) json.RawMessage {
	t.Helper()
	cur := json.RawMessage(eob)
	for _, p := range path {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(cur, &m); err != nil {
			var arr []json.RawMessage
			if json.Unmarshal(cur, &arr) != nil || len(arr) == 0 {
				return nil
			}
			if err := json.Unmarshal(arr[0], &m); err != nil {
				return nil
			}
		}
		cur = m[p]
	}
	return cur
}

// TestDecisionEOB_CarriesEachSourceAsWritten: every element is read from its
// source and carried exactly: the request Claim's type, provider, insurance,
// item sequence and net; the ClaimResponse's created (its offset kept, never
// normalized to UTC or replaced by a clock), insurer and outcome.
func TestDecisionEOB_CarriesEachSourceAsWritten(t *testing.T) {
	eob, err := eobRowBuild(t, eobRowRequest(t, nil), eobRowClaimResponse(t, func(cr map[string]any) {
		// The payer states no submitted entry, so the request's net is the
		// submitted amount.
		cr["outcome"], cr["preAuthRef"], cr["item"] = "partial", "PA-7", []any{map[string]any{"itemSequence": 3}}
	}),
		shnsdk.PriorAuthResult{Outcome: "approved", PreAuthRef: "PA-7"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"type":` + eobRowType,
		`"created":"` + eobRowCreated + `"`,
		`"insurer":{"reference":"Organization/ins-1"}`,
		`"provider":{"reference":"Practitioner/pr-7","display":"Dr. Rivera"}`,
		`"insurance":[{"focal":true,"coverage":{"reference":"Coverage/cov-7"}},{"focal":false,"coverage":{"reference":"Coverage/cov-8"}}]`,
		`"outcome":"partial"`,
		`"sequence":3`,
		`"amount":{"value":412.30,"currency":"USD"}`,
		`"status":"active"`, `"use":"preauthorization"`, `"preAuthRef":["PA-7"]`,
	} {
		if !bytes.Contains(eob, []byte(want)) {
			t.Errorf("EOB does not carry %s:\n%s", want, eob)
		}
	}
}

// TestDecisionEOB_RequiredElementAbsentWritesNoEOB: an element the EOB
// requires that its source does not state is never filled in: no EOB, and the
// error names the element and its source (the finding text).
func TestDecisionEOB_RequiredElementAbsentWritesNoEOB(t *testing.T) {
	for _, tc := range []struct {
		name    string
		claim   func(map[string]any)
		cr      func(map[string]any)
		request []byte
		want    string
	}{
		{name: "no request Claim", request: []byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`),
			want: "decision EOB not written: Claim absent at the request Bundle"},
		{name: "unreadable request Claim", claim: func(c map[string]any) { c["item"] = "not a list" },
			want: "decision EOB not written: Claim unreadable at the request Bundle"},
		{name: "type", claim: func(c map[string]any) { delete(c, "type") }, want: "decision EOB not written: type absent at the request Claim"},
		{name: "provider", claim: func(c map[string]any) { delete(c, "provider") }, want: "decision EOB not written: provider absent at the request Claim"},
		{name: "insurance", claim: func(c map[string]any) { delete(c, "insurance") }, want: "decision EOB not written: insurance absent at the request Claim"},
		{name: "insurance.focal", claim: func(c map[string]any) {
			c["insurance"] = []any{map[string]any{"sequence": 1, "coverage": map[string]any{"reference": "Coverage/cov-7"}}}
		}, want: "decision EOB not written: insurance.focal absent at the request Claim"},
		{name: "insurance.coverage", claim: func(c map[string]any) {
			c["insurance"] = []any{map[string]any{"sequence": 1, "focal": true}}
		}, want: "decision EOB not written: insurance.coverage absent at the request Claim"},
		{name: "item.sequence", claim: func(c map[string]any) {
			c["item"] = []any{map[string]any{"productOrService": map[string]any{"coding": []any{map[string]any{"system": eobRowCPT, "code": "72148"}}}}}
		}, want: "decision EOB not written: item.sequence absent at the request Claim item"},
		{name: "no item answers the order", claim: func(c map[string]any) {
			c["item"] = []any{
				map[string]any{"sequence": 1, "productOrService": map[string]any{"coding": []any{map[string]any{"system": eobRowCPT, "code": "1"}}}},
				map[string]any{"sequence": 2, "productOrService": map[string]any{"coding": []any{map[string]any{"system": eobRowCPT, "code": "2"}}}},
			}
		}, want: "decision EOB not written: no item of the request Claim states the decision's product coding"},
		{name: "created", cr: func(cr map[string]any) { delete(cr, "created") }, want: "decision EOB not written: created absent at the payer's ClaimResponse"},
		{name: "insurer", cr: func(cr map[string]any) { delete(cr, "insurer") }, want: "decision EOB not written: insurer absent at the payer's ClaimResponse"},
		{name: "insurer with neither reference nor identifier", cr: func(cr map[string]any) { cr["insurer"] = map[string]any{"display": "A payer"} },
			want: "decision EOB not written: insurer absent at the payer's ClaimResponse"},
		{name: "outcome", cr: func(cr map[string]any) { delete(cr, "outcome") }, want: "decision EOB not written: outcome absent at the payer's ClaimResponse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := tc.request
			if request == nil {
				request = eobRowRequest(t, tc.claim)
			}
			eob, err := eobRowBuild(t, request, eobRowClaimResponse(t, tc.cr), shnsdk.PriorAuthResult{Outcome: "approved"})
			var absent *eobNotWritten
			if !errors.As(err, &absent) || eob != nil {
				t.Fatalf("got EOB=%s err=%v, want no EOB and an absent element", eob, err)
			}
			if err.Error() != tc.want {
				t.Fatalf("finding = %q, want %q", err.Error(), tc.want)
			}
		})
	}
}

// TestDecisionEOB_ReferenceByIdentifier: a reference whose source carries an
// identifier, or names a resource in the source Bundle that has one, is stated
// by that identifier (an NPI first, then a member identifier, else the first),
// exactly as written; otherwise by the literal reference. No local id is made.
func TestDecisionEOB_ReferenceByIdentifier(t *testing.T) {
	for _, tc := range []struct {
		name  string
		claim func(map[string]any)
		cr    func(map[string]any)
		want  []string
	}{
		{name: "provider reference carries an identifier", claim: func(c map[string]any) {
			c["provider"] = json.RawMessage(`{"identifier":{"system":"http://hl7.org/fhir/sid/us-npi","value":"1215930367"},"display":"Clinic"}`)
		}, want: []string{`"provider":{"identifier":{"system":"http://hl7.org/fhir/sid/us-npi","value":"1215930367"},"display":"Clinic"}`}},
		{name: "provider names a Bundle entry with an NPI", claim: func(c map[string]any) {
			c["provider"] = map[string]any{"reference": "Practitioner/pr-9"}
		}, want: []string{`"provider":{"identifier":{"system":"http://hl7.org/fhir/sid/us-npi","value":"1234567893"}}`}},
		{name: "coverage names a Bundle entry with a member identifier", claim: func(c map[string]any) {
			c["insurance"] = []any{map[string]any{"sequence": 1, "focal": true, "coverage": map[string]any{"reference": "urn:uuid:cov-mb"}}}
		}, want: []string{`"coverage":{"identifier":{"system":"http://payer.test/member","type":{"coding":[{"code":"MB","system":"http://terminology.hl7.org/CodeSystem/v2-0203"}]},"value":"M-77"}}`}},
		{name: "no identifier anywhere: the literal reference", want: []string{
			`"provider":{"reference":"Practitioner/pr-7","display":"Dr. Rivera"}`, `"coverage":{"reference":"Coverage/cov-7"}`, `"insurer":{"reference":"Organization/ins-1"}`}},
		{name: "insurer carries an identifier", cr: func(cr map[string]any) {
			cr["insurer"] = json.RawMessage(`{"identifier":{"system":"http://hl7.org/fhir/sid/us-npi","value":"1972532007"}}`)
		}, want: []string{`"insurer":{"identifier":{"system":"http://hl7.org/fhir/sid/us-npi","value":"1972532007"}}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eob, err := eobRowBuild(t, eobRowRequest(t, tc.claim), eobRowClaimResponse(t, tc.cr), shnsdk.PriorAuthResult{Outcome: "approved"})
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.want {
				if !bytes.Contains(eob, []byte(w)) {
					t.Errorf("EOB does not state %s:\n%s", w, eob)
				}
			}
		})
	}
	// The insurer names an entry of the payer's own answer Bundle.
	answer := []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"fullUrl":"https://payer.test/fhir/Organization/ins-1","resource":{"resourceType":"Organization","id":"ins-1","identifier":[{"system":"http://hl7.org/fhir/sid/us-npi","value":"1972532007"}]}}]}`)
	eob, err := decisionEOB("corr-row", "Patient/MBR-COVERED", eobRowCPT, "72148", "MRI", shnsdk.PriorAuthResult{Outcome: "approved"},
		decisionEOBSource{request: eobRowRequest(t, nil), itemIndex: -1, claimResponse: eobRowClaimResponse(t, nil), answer: answer})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(eob, []byte(`"insurer":{"identifier":{"system":"http://hl7.org/fhir/sid/us-npi","value":"1972532007"}}`)) {
		t.Fatalf("insurer not stated by the identifier its answer entry carries:\n%s", eob)
	}
}

// eobRowRealPayerA3 is the reference payer's own adjudication on a denial:
// an amountless "submitted" entry carrying its A3 under the PAS extension
// (testdata/brpayer-current/known-member/first-submission.response.json).
const eobRowRealPayerA3 = `{"extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewAction","extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewActionCode","valueCodeableConcept":{"coding":[{"system":"https://codesystem.x12.org/005010/306","code":"A3","display":"Not Required"}]}}]}],"category":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/adjudication","code":"submitted","display":"Submitted Amount"}]}}`

const eobRowPayerA1 = `{"category":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/adjudication","code":"submitted"}]},"extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewAction","extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewActionCode","valueCodeableConcept":{"coding":[{"system":"https://codesystem.x12.org/005010/306","code":"A1"}]}}]}]}`

const eobRowPayerReason = `{"category":{"coding":[{"code":"denialreason"}]},"reason":{"coding":[{"system":"https://x12.org/codes/claim-adjustment-reason-codes","code":"197"}]}}`

// eobRowWithAdjudication is a ClaimResponse whose item 3 states exactly the
// given adjudication entries, as written.
func eobRowWithAdjudication(entries ...string) func(map[string]any) {
	return func(cr map[string]any) {
		adj := []any{}
		for _, e := range entries {
			adj = append(adj, json.RawMessage(e))
		}
		cr["item"] = []any{map[string]any{"itemSequence": 3, "adjudication": adj},
			map[string]any{"itemSequence": 9, "adjudication": []any{json.RawMessage(`{"category":{"text":"another item"},"amount":{"value":1,"currency":"USD"}}`)}}}
	}
}

// TestDecisionEOB_Adjudication: the payer's own adjudication for the item is
// carried byte for byte — the reference payer's amountless submitted entry
// with its A3, an approval's A1, a denial's reason codes — and no amount,
// review action or reason code is ever added. The request's net is a
// "submitted" adjudication only where the payer stated no submitted entry;
// with neither, the item carries no adjudication.
func TestDecisionEOB_Adjudication(t *testing.T) {
	noNet := func(c map[string]any) {
		c["item"] = []any{map[string]any{"sequence": 3, "productOrService": map[string]any{"coding": []any{map[string]any{"system": eobRowCPT, "code": "72148"}}}}}
	}
	netSubmitted := `{"category":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/adjudication","code":"submitted","display":"Submitted Amount"}]},"amount":{"value":412.30,"currency":"USD"}}`
	for _, tc := range []struct {
		name    string
		claim   func(map[string]any)
		cr      func(map[string]any)
		outcome string
		want    string
	}{
		{name: "real payer denial: amountless submitted with A3, net not added", cr: eobRowWithAdjudication(eobRowRealPayerA3), outcome: "denied",
			want: `"adjudication":[` + eobRowRealPayerA3 + `]`},
		{name: "approval with A1", claim: noNet, cr: eobRowWithAdjudication(eobRowPayerA1), outcome: "approved",
			want: `"adjudication":[` + eobRowPayerA1 + `]`},
		{name: "denial with reasons, the request's net as submitted", cr: eobRowWithAdjudication(eobRowPayerReason), outcome: "denied",
			want: `"adjudication":[` + netSubmitted + `,` + eobRowPayerReason + `]`},
		{name: "denial with A3 and reasons", claim: noNet, cr: eobRowWithAdjudication(eobRowRealPayerA3, eobRowPayerReason), outcome: "denied",
			want: `"adjudication":[` + eobRowRealPayerA3 + `,` + eobRowPayerReason + `]`},
		{name: "no payer adjudication, no net: none", claim: noNet, cr: eobRowWithAdjudication(), outcome: "approved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eob, err := eobRowBuild(t, eobRowRequest(t, tc.claim), eobRowClaimResponse(t, tc.cr), shnsdk.PriorAuthResult{Outcome: tc.outcome})
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if bytes.Contains(eob, []byte(`"adjudication"`)) {
					t.Fatalf("EOB states an adjudication no source stated:\n%s", eob)
				}
				return
			}
			// The review action is re-expressed in the EOB's own extension,
			// every value unchanged; everything else is byte for byte.
			want := strings.ReplaceAll(tc.want, "davinci-pas/StructureDefinition/extension-reviewAction", "davinci-pdex/StructureDefinition/extension-reviewAction")
			if !bytes.Contains(eob, []byte(want+`}]`)) {
				t.Fatalf("EOB does not carry exactly %s:\n%s", want, eob)
			}
			if bytes.Contains(eob, []byte("another item")) {
				t.Fatalf("EOB carries another item's adjudication:\n%s", eob)
			}
		})
	}
}

// TestDecisionEOB_PayerDetailCarried: an A6
// (modified) approval's review action, a denial reason in the payer's own
// code system, and reason codes on an approval are carried as the payer
// stated them; the EOB's preAuthRef is the payer's own numbers as stated
// (its ClaimResponse.preAuthRef, then the review action number), whatever the
// decision, and none when it stated none.
func TestDecisionEOB_PayerDetailCarried(t *testing.T) {
	a6 := `{"category":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/adjudication","code":"submitted"}]},"extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewAction","extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewActionCode","valueCodeableConcept":{"coding":[{"system":"https://codesystem.x12.org/005010/306","code":"A6","display":"Modified"}]}},{"url":"number","valueString":"AUTH-A6"}]}]}`
	proprietary := `{"category":{"coding":[{"code":"denialreason"}]},"reason":{"coding":[{"system":"http://payer.example/reasons","code":"P-17","display":"Plan rule 17"}]}}`
	for _, tc := range []struct {
		name, outcome, preAuth string
		entries                []string
		want                   []string
		wantNot                []string
	}{
		{name: "A6 approval with its number", outcome: "approved", entries: []string{a6},
			want: []string{`"code":"A6","display":"Modified"`, `{"url":"number","valueString":"AUTH-A6"}`, `"preAuthRef":["AUTH-A6"]`}},
		{name: "top-level preAuthRef and the review action number, each once", outcome: "approved", preAuth: "AUTH-TOP", entries: []string{a6},
			want: []string{`"preAuthRef":["AUTH-TOP","AUTH-A6"]`}},
		{name: "denial reason in the payer's own system", outcome: "denied", entries: []string{eobRowRealPayerA3, proprietary},
			want: []string{proprietary}},
		{name: "reason codes on an approval", outcome: "approved", entries: []string{eobRowPayerA1, eobRowPayerReason},
			want: []string{eobRowPayerReason}},
		{name: "no number stated: no preAuthRef", outcome: "approved", entries: []string{eobRowPayerA1},
			wantNot: []string{"preAuthRef"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eob, err := eobRowBuild(t, eobRowRequest(t, nil), eobRowClaimResponse(t, func(cr map[string]any) {
				eobRowWithAdjudication(tc.entries...)(cr)
				if tc.preAuth != "" {
					cr["preAuthRef"] = tc.preAuth
				}
			}), shnsdk.PriorAuthResult{Outcome: tc.outcome})
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.want {
				if !bytes.Contains(eob, []byte(w)) {
					t.Errorf("EOB does not carry %s:\n%s", w, eob)
				}
			}
			for _, w := range tc.wantNot {
				if bytes.Contains(eob, []byte(w)) {
					t.Errorf("EOB carries %s:\n%s", w, eob)
				}
			}
		})
	}
	// A payer review action sub-extension with no counterpart writes no EOB
	// and is named; it is never dropped.
	unmapped := strings.Replace(eobRowRealPayerA3, `"display":"Not Required"}]}}]`, `"display":"Not Required"}]}},{"url":"http://payer.example/x","valueString":"y"}]`, 1)
	eob, err := eobRowBuild(t, eobRowRequest(t, nil), eobRowClaimResponse(t, eobRowWithAdjudication(unmapped)), shnsdk.PriorAuthResult{Outcome: "denied"})
	if want := "decision EOB not written: review action sub-extension 2 has no counterpart in the EOB review action"; eob != nil || err == nil || err.Error() != want {
		t.Fatalf("got EOB=%s err=%v, want no EOB and %q", eob, err, want)
	}
}

// TestDecisionEOB_NoMintedContent is the hermetic tripwire over the gateway's
// projection: across every decision shape, no EOB states the reviewer
// stand-in, an amount the payer did not state, Organization/payer as its
// insurer, the claim-type constant the request did not send, an A3 the payer
// did not state, or a PDex profile.
func TestDecisionEOB_NoMintedContent(t *testing.T) {
	a2 := &shnsdk.PASReviewAction{Code: shnsdk.PASCoding{System: shnsdk.X12ReviewDecisionSystem, Code: "A2"}}
	for _, parsed := range []shnsdk.PriorAuthResult{
		{Outcome: "approved"}, {Outcome: "approved", PreAuthRef: "PA-1"}, {Outcome: "denied"},
		{Outcome: "denied", ReviewAction: a2}, {Outcome: "denied", DenialReasons: []shnsdk.PASCoding{{System: shnsdk.RARCSystem, Code: "N130"}}},
	} {
		for _, net := range []bool{true, false} {
			claim := func(c map[string]any) {}
			if !net {
				claim = func(c map[string]any) {
					c["item"] = []any{map[string]any{"sequence": 3, "productOrService": map[string]any{"coding": []any{map[string]any{"system": eobRowCPT, "code": "72148"}}}}}
				}
			}
			eob, err := eobRowBuild(t, eobRowRequest(t, claim), eobRowClaimResponse(t, nil), parsed)
			if err != nil {
				t.Fatalf("%+v: %v", parsed, err)
			}
			minted := []string{"reviewer-uc08", `"value":0,`, `"Organization/payer"`, `"code":"A3"`, `"code":"A2"`, `"code":"professional"`, `"code":"50"`, "Appeal window", "pdex", `"meta"`}
			if !net {
				minted = append(minted, `"amount"`)
			}
			for _, m := range minted {
				if strings.Contains(string(eob), m) {
					t.Errorf("%+v net=%v: EOB carries %s, which no source stated:\n%s", parsed, net, m, eob)
				}
			}
		}
	}
}

// TestNativeSubmit_EOBNotWrittenStillRelaysAndRecords: a payer answer whose
// ClaimResponse states no insurer still relays exactly and its decision is
// recorded, at every level, with no EOB; the leg notes which element was
// absent at which source.
func TestNativeSubmit_EOBNotWrittenStillRelaysAndRecords(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict} {
		t.Run(level.String(), func(t *testing.T) {
			decision := []byte(`{"resourceType":"ClaimResponse","outcome":"complete","created":"2026-06-03T00:00:00Z","preAuthRef":"P-1","preAuthPeriod":{"end":"2030-01-01"}}`)
			body := fixturePASResponse(t, decision, true)
			// Strip the insurer the fixture helper states.
			var b map[string]any
			if err := json.Unmarshal(body, &b); err != nil {
				t.Fatal(err)
			}
			entries := b["entry"].([]any)
			delete(entries[0].(map[string]any)["resource"].(map[string]any), "insurer")
			var kept []any
			for _, e := range entries {
				if !strings.HasSuffix(e.(map[string]any)["fullUrl"].(string), "/Organization/insurer") {
					kept = append(kept, e)
				}
			}
			b["entry"] = kept
			body, _ = json.Marshal(b)
			srv := stubPartnerSrv(t, http.StatusOK, body)
			n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", newCensusSoR(), fixedClock, WithConformancePolicy(NewConformancePolicy(level)))
			ctx, leg := withPASLeg(context.Background(), "provider")
			res, err := n.Handle(ctx, "pas-claim", "corr-absent", "PCI-1", originatorBuiltConformantBundle(t, "MBR-COVERED"))
			if err != nil || res.Status != 0 {
				t.Fatalf("the payer's decision must relay: err=%v status=%d msg=%s", err, res.Status, res.Message)
			}
			if !bytes.Equal(responseBytes(res), body) {
				t.Fatal("the payer's answer must reach the requester unchanged")
			}
			if len(res.SideEffectFHIR) != 0 {
				t.Fatalf("an EOB was written with its insurer absent: %s", res.SideEffectFHIR[0])
			}
			if res.Commit == nil {
				t.Fatal("the decision must still be recorded")
			}
			if len(leg.detailed) != 1 || leg.detailed[0].kind != DecisionEOBNotWrittenEvent ||
				leg.detailed[0].detail != "decision EOB not written: insurer absent at the payer's ClaimResponse" {
				t.Fatalf("notes = %+v, want one %s naming the insurer and its source", leg.detailed, DecisionEOBNotWrittenEvent)
			}
		})
	}
}

// TestPASInquire_DecisionEOBReadsItsOwnClaimResponse: on the inquiry leg the
// EOB's created is the created of the ClaimResponse that carried the decision,
// exactly as written (its offset kept), never this gateway's clock; its
// provider is the inquiry Claim's (stated by the NPI its entry carries) and
// its insurer the ClaimResponse's.
func TestPASInquire_DecisionEOBReadsItsOwnClaimResponse(t *testing.T) {
	f := newInquiryLedgerFixture(t, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{inquiryCRKey}, ItemTraceNumbers: []string{inquiryTraceKey}})
	result := f.apply(t, inquiryRequester, decidedAnswer(t))
	if len(result.SideEffectFHIR) != 1 {
		t.Fatalf("want one decision EOB, got %d", len(result.SideEffectFHIR))
	}
	eob := result.SideEffectFHIR[0]
	for _, want := range []string{
		`"created":"2019-07-20T11:01:00+05:00"`,
		`"provider":{"identifier":{"system":"http://hl7.org/fhir/sid/us-npi","value":"1215930367"}}`,
		`"insurer":{"identifier":{"system":"http://hl7.org/fhir/sid/us-npi","value":"1234567893"}}`,
		`"insurance":[{"focal":true,"coverage":{"reference":"urn:uuid:coverage"}}]`,
	} {
		if !bytes.Contains(eob, []byte(want)) {
			t.Errorf("inquiry EOB does not carry %s:\n%s", want, eob)
		}
	}
	if bytes.Contains(eob, []byte(fixedClock().UTC().Format("2006-01-02T15:04:05Z07:00"))) {
		t.Errorf("inquiry EOB carries this gateway's clock:\n%s", eob)
	}
}

// TestPASInquire_EOBNotWrittenStillRecordsTheDecision: a decision whose
// ClaimResponse states no created is recorded without an EOB, and the inquiry
// raises DecisionEOBNotWrittenEvent naming the element and its source.
func TestPASInquire_EOBNotWrittenStillRecordsTheDecision(t *testing.T) {
	f := newInquiryLedgerFixture(t, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{inquiryCRKey}, ItemTraceNumbers: []string{inquiryTraceKey}})
	answer := bytes.Replace(decidedAnswer(t), []byte(`"created"`), []byte(`"createdNot"`), 1)
	if bytes.Equal(answer, decidedAnswer(t)) {
		t.Fatal("the fixture states no created to remove")
	}
	events := f.applyEvents(t, inquiryRequester, answer)
	if rec := f.state(t); rec.State != PendStateDecided {
		t.Fatalf("the decision was not recorded: %+v", rec)
	}
	if _, ok := f.store.EOBByID(decisionEOBID(f.corr)); ok {
		t.Fatal("an EOB was written with its created absent")
	}
	var found bool
	for _, e := range events {
		if e.Kind == DecisionEOBNotWrittenEvent {
			found = true
			if want := "authorization " + f.corr + ": decision EOB not written: created absent at the payer's ClaimResponse"; e.Detail != want {
				t.Errorf("detail = %q, want %q", e.Detail, want)
			}
		}
	}
	if !found {
		t.Fatalf("no %s event: %+v", DecisionEOBNotWrittenEvent, events)
	}
}

// TestPASInquire_AnswerWithoutTheLinesTraceWritesNoEOB: an inquiry answer
// whose ClaimResponse item echoes no trace number (the published 2.0 answer
// as written) decides the authorization, but its decision is on no item the
// EOB could pair with the inquiry's line: the decision is recorded, no EOB is
// written, and the finding names the position only.
func TestPASInquire_AnswerWithoutTheLinesTraceWritesNoEOB(t *testing.T) {
	f := newInquiryLedgerFixture(t, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{inquiryCRKey}, ItemTraceNumbers: []string{inquiryTraceKey}})
	events := f.applyEvents(t, inquiryRequester, inquiryFixture(t, "pas-inquiry-response-2.0.json"))
	if rec := f.state(t); rec.State != PendStateDecided || rec.Outcome != PendOutcomeApproved {
		t.Fatalf("the decision was not recorded: %+v", rec)
	}
	if _, ok := f.store.EOBByID(decisionEOBID(f.corr)); ok {
		t.Fatal("an EOB was written that carries none of the payer's decision on the line")
	}
	want := "authorization " + f.corr + ": decision EOB not written: " + eobNoInquiryItemAnswered
	for _, e := range events {
		if e.Kind == DecisionEOBNotWrittenEvent && e.Detail == want {
			return
		}
	}
	t.Fatalf("no %s event %q: %+v", DecisionEOBNotWrittenEvent, want, events)
}
