// transform_run_test.go — RunTransformChain: the exported,
// contract-parameterized sibling of TransformPASForTest/TransformDTRForTest.
// Exercises the SAME real fixtures and idioms transform_dtr_test.go and
// test/conformance/dtr_transform_live_test.go already establish for pa.dtr
// 2.1<->2.2, through the new exported entry point rather than the *ForTest
// wrapper or chainFor/applyChain directly — proving the demo/diagnostic seam
// runs the identical chain a real leg would.
package engine

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// TestRunTransformChain_DTR2122AutofillGoldenBothDirections proves the exported
// entry point reproduces the real 2.1<->2.2 autofill golden pair exactly —
// same assertion idiom as TestDTRStep2122Up_AutofillGolden/
// TestDTRStep2122Down_AutofillGolden, just driven through RunTransformChain.
func TestRunTransformChain_DTR2122AutofillGoldenBothDirections(t *testing.T) {
	in21 := pasGolden(t, "2.1/questionnaireresponse-autofill.json")
	want22 := pasGolden(t, "2.2/questionnaireresponse-autofill.json")

	up, upReports, err := RunTransformChain("pa.dtr", "2.1", "2.2", in21, corr)
	if err != nil {
		t.Fatalf("2.1->2.2: unexpected error: %v", err)
	}
	assertJSONEqual(t, up, want22, "2.1->2.2 autofill QR via RunTransformChain")
	if len(upReports) != 1 {
		t.Fatalf("want exactly 1 step report, got %+v", upReports)
	}

	down, downReports, err := RunTransformChain("pa.dtr", "2.2", "2.1", want22, corr)
	if err != nil {
		t.Fatalf("2.2->2.1: unexpected error: %v", err)
	}
	assertJSONEqual(t, down, in21, "2.2->2.1 autofill QR via RunTransformChain")
	if len(downReports) != 1 || len(downReports[0].Carried) != 0 {
		t.Fatalf("no itemWeight in this fixture, want 1 report with no Carried entries, got %+v", downReports)
	}
}

// TestRunTransformChain_DTRItemWeightCarryDown pins the loss-policy carry
// mechanism through the exported entry point: a real 2.2 autofill golden with
// a synthetic-but-realistic item.answer.value.extension:itemWeight injected (the
// exact idiom test/conformance/dtr_transform_live_test.go's
// injectAnswerExtension establishes — SHN's own builder never stamps
// itemWeight, so no real golden carries one) downcasts to 2.1 with a Carried
// LossEntry naming item.answer.value.extension:itemWeight, never silently dropped.
func TestRunTransformChain_DTRItemWeightCarryDown(t *testing.T) {
	in22 := pasGolden(t, "2.2/questionnaireresponse-autofill.json")
	withWeight := injectItemWeight(t, in22, "conservative-therapy-weeks")

	_, reports, err := RunTransformChain("pa.dtr", "2.2", "2.1", withWeight, corr)
	if err != nil {
		t.Fatalf("2.2->2.1 with itemWeight: unexpected error: %v", err)
	}
	if !anyCarried(reports, dtrItemWeightLocus) {
		t.Fatalf("want a Carried LossEntry naming %s, got %+v", dtrItemWeightLocus, reports)
	}
}

// TestRunTransformChain_IdentityFromEqualsTo proves the from==to edge
// (chainFor returns []CompatStep{}, applyChain's loop never runs) returns the
// payload byte-unchanged with a nil (zero-length) reports slice and no
// error — the endpoint layer (gateway/app/demo_endpoint.go) is responsible
// for normalizing that nil to a wire-visible "[]", not this func.
func TestRunTransformChain_IdentityFromEqualsTo(t *testing.T) {
	in := pasGolden(t, "2.1/questionnaireresponse-autofill.json")
	out, reports, err := RunTransformChain("pa.dtr", "2.1", "2.1", in, corr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(in) {
		t.Fatalf("from==to must return the payload byte-unchanged:\ngot:  %s\nwant: %s", out, in)
	}
	if len(reports) != 0 {
		t.Fatalf("want 0 reports for an empty chain, got %+v", reports)
	}
}

// TestRunTransformChain_UnknownChain proves an unrecognized contract/line
// pair errors rather than silently no-op'ing (chainFor's nil-hole path).
func TestRunTransformChain_UnknownChain(t *testing.T) {
	if _, _, err := RunTransformChain("pa.dtr", "1.0", "9.9", []byte(`{}`), corr); err == nil {
		t.Fatalf("want an error for an unknown chain, got success")
	}
	if _, _, err := RunTransformChain("no.such.contract", "2.1", "2.2", []byte(`{}`), corr); err == nil {
		t.Fatalf("want an error for an unknown contract, got success")
	}
}

// TestRunTransformChain_MultiCoverageSemanticChange proves the exported entry
// point surfaces the SAME typed *SemanticChangeError (errors.As) a caller
// driving dtrStep2122Up directly would see — the spec's canonical
// semantic-change refusal, TestDTRStep2122Up_MultiCoverageGated's fixture.
func TestRunTransformChain_MultiCoverageSemanticChange(t *testing.T) {
	in := []byte(`{
		"resourceType":"QuestionnaireResponse","id":"qr-multi","status":"completed",
		"questionnaire":"http://smarthealth.network/fhir/Questionnaire/pa-lumbar-mri|1.0.0",
		"subject":{"reference":"Patient/MBR-COVERED"},
		"authored":"2026-06-04T00:00:00Z",
		"extension":[
			{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-context","valueReference":{"reference":"Coverage/MBR-PRIMARY"}},
			{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-context","valueReference":{"reference":"Coverage/MBR-SECONDARY"}},
			{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-context","valueReference":{"reference":"ServiceRequest/sr-MBR-COVERED"}},
			{"url":"http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/intendedUse","valueCodeableConcept":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-crd/CodeSystem/temp","code":"withpa","display":"Information needed for a prior authorization"}]}}
		],
		"item":[]
	}`)

	out, reports, err := RunTransformChain("pa.dtr", "2.1", "2.2", in, corr)
	if err == nil {
		t.Fatalf("want a semantic-change refusal, got success (out=%q reports=%+v)", out, reports)
	}
	var scErr *SemanticChangeError
	if !errors.As(err, &scErr) {
		t.Fatalf("want *SemanticChangeError (errors.As), got %T: %v", err, err)
	}
	if scErr.Contract != "pa.dtr" || scErr.From != "2.1" || scErr.To != "2.2" || scErr.Direction != "up" {
		t.Fatalf("unexpected error fields: %+v", scErr)
	}
	if out != nil || reports != nil {
		t.Fatalf("a refused chain must return nil output and nil reports, got out=%q reports=%+v", out, reports)
	}
}

// injectItemWeight returns qrJSON (a bare QuestionnaireResponse) with a
// dtrItemWeightExt extension appended to the first answer of the item
// matching linkID — mirrors test/conformance/dtr_transform_live_test.go's
// injectAnswerExtension (same rationale: a synthetic-but-realistic fixture
// for the live-lane carry check without touching any checked-in golden).
func injectItemWeight(t *testing.T, qrJSON []byte, linkID string) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(qrJSON, &doc); err != nil {
		t.Fatalf("decode QR: %v", err)
	}
	items, _ := doc["item"].([]any)
	found := false
	for _, it := range items {
		im, ok := it.(map[string]any)
		if !ok || im["linkId"] != linkID {
			continue
		}
		answers, _ := im["answer"].([]any)
		if len(answers) == 0 {
			t.Fatalf("item %q has no answer to attach itemWeight to", linkID)
		}
		am := answers[0].(map[string]any)
		// Inject at the answer's VALUE, the only locus the itemWeight SD's
		// context permits. The autofill golden's answers are primitive
		// (valueInteger), so the extension goes on the sibling "_valueInteger"
		// object, created here when absent — this helper is an injector, not the
		// engine's read-side walker.
		under, ok := am["_valueInteger"].(map[string]any)
		if !ok {
			under = map[string]any{}
			am["_valueInteger"] = under
		}
		exts, _ := under["extension"].([]any)
		under["extension"] = append(exts, map[string]any{
			"url":          dtrItemWeightExt,
			"valueDecimal": 0.5,
		})
		found = true
		break
	}
	if !found {
		t.Fatalf("no item with linkId %q found", linkID)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return out
}

// ChainSteps mirrors the chain the demonstration endpoint runs so its
// callers can report the hop list from the engine that executed it —
// never a hardcoded copy that drifts when the compatibility manifest
// gains an intermediate line.
func TestChainSteps(t *testing.T) {
	got := ChainSteps("pa.dtr", "2.1", "2.2")
	want := []ChainStep{{Module: "pa.dtr 2.1->2.2", From: "2.1", To: "2.2", Class: "carry"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ChainSteps(pa.dtr,2.1,2.2) = %+v, want %+v", got, want)
	}
	// Down direction: chainFor returns canonical manifest steps either way;
	// the walk must direction-swap, never emit a degenerate 2.2->2.2 hop.
	down := ChainSteps("pa.dtr", "2.2", "2.1")
	wantDown := []ChainStep{{Module: "pa.dtr 2.2->2.1", From: "2.2", To: "2.1", Class: "carry"}}
	if !reflect.DeepEqual(down, wantDown) {
		t.Fatalf("ChainSteps(pa.dtr,2.2,2.1) = %+v, want %+v", down, wantDown)
	}
	if ChainSteps("pa.dtr", "2.1", "9.9") != nil {
		t.Fatalf("unknown target line must yield nil, not a guess")
	}
}

// anyCarried reports whether any LossReport in reports carries a LossEntry
// whose Path is path — mirrors test/conformance/pas_transform_live_test.go's
// anyCarried.
func anyCarried(reports []LossReport, path string) bool {
	for _, r := range reports {
		for _, c := range r.Carried {
			if c.Path == path {
				return true
			}
		}
	}
	return false
}
