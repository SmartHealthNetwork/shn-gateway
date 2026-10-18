package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The payer's PAS review action as a ClaimResponse adjudication states it,
// and as the EOB carries it (only the extension URLs change).
func payerRA(code, number string) string {
	subs := `{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewActionCode","valueCodeableConcept":{"coding":[{"system":"https://codesystem.x12.org/005010/306","code":"` + code + `"}]}}`
	if number != "" {
		subs += `,{"url":"number","valueString":"` + number + `"}`
	}
	return `{"extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewAction","extension":[` + subs + `]}],` +
		`"category":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/adjudication","code":"submitted"}]}}`
}

func eobRA(code, number string) string {
	subs := `{"url":"http://hl7.org/fhir/us/davinci-pdex/StructureDefinition/extension-reviewActionCode","valueCodeableConcept":{"coding":[{"system":"https://codesystem.x12.org/005010/306","code":"` + code + `"}]}}`
	if number != "" {
		subs += `,{"url":"number","valueString":"` + number + `"}`
	}
	return `{"extension":[{"url":"http://hl7.org/fhir/us/davinci-pdex/StructureDefinition/extension-reviewAction","extension":[` + subs + `]}],` +
		`"category":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/adjudication","code":"submitted"}]}}`
}

func eobRawList(t *testing.T, eob []byte, key string) []json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(eob, &m); err != nil {
		t.Fatal(err)
	}
	var out []json.RawMessage
	if v, ok := m[key]; ok {
		if err := json.Unmarshal(v, &out); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// TestDecisionEOB_SubmitPairsOnlyTheItemCodingTheOrder: on submit the
// EOB item is the request item that codes the order, its productOrService
// carried as that item wrote it, and only the payer's adjudication of that
// item's sequence. The order's code is never paired with another item: a
// request whose items do not code the order writes no EOB, and the finding
// says so.
func TestDecisionEOB_SubmitPairsOnlyTheItemCodingTheOrder(t *testing.T) {
	cr := eobRowClaimResponse(t, func(cr map[string]any) {
		cr["item"] = []any{
			map[string]any{"itemSequence": 2, "adjudication": []any{json.RawMessage(payerRA("A3", ""))}},
			map[string]any{"itemSequence": 3, "adjudication": []any{json.RawMessage(payerRA("A1", "AUTH-3"))}},
		}
	})
	eob, err := eobRowBuild(t, eobRowRequest(t, nil), cr, shnsdk.PriorAuthResult{Outcome: "approved"})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		PreAuthRef []string `json:"preAuthRef"`
		Item       []struct {
			Sequence         int               `json:"sequence"`
			ProductOrService json.RawMessage   `json:"productOrService"`
			Adjudication     []json.RawMessage `json:"adjudication"`
		} `json:"item"`
	}
	if err := json.Unmarshal(eob, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Item) != 1 || got.Item[0].Sequence != 3 || string(got.Item[0].ProductOrService) != `{"coding":[{"code":"72148","system":"`+eobRowCPT+`"}]}` ||
		len(got.Item[0].Adjudication) != 1 || string(got.Item[0].Adjudication[0]) != eobRA("A1", "AUTH-3") {
		t.Fatalf("EOB item = %+v, want sequence 3 with its own product and only its A1", got.Item)
	}
	if len(got.PreAuthRef) != 1 || got.PreAuthRef[0] != "AUTH-3" {
		t.Fatalf("preAuthRef = %v, want only item 3's number", got.PreAuthRef)
	}

	// One request item that does not code the order: no EOB, never the
	// order's code over that item's adjudication.
	one := eobRowRequest(t, func(c map[string]any) {
		c["item"] = []any{map[string]any{"sequence": 1, "productOrService": map[string]any{"coding": []any{map[string]any{"system": eobRowCPT, "code": "99999"}}}}}
	})
	eob, err = eobRowBuild(t, one, eobRowClaimResponse(t, func(cr map[string]any) {
		cr["item"] = []any{map[string]any{"itemSequence": 1, "adjudication": []any{json.RawMessage(payerRA("A1", ""))}}}
	}), shnsdk.PriorAuthResult{Outcome: "approved"})
	var nw *eobNotWritten
	if !errors.As(err, &nw) || eob != nil || err.Error() != "decision EOB not written: no item of the request Claim states the decision's product coding" {
		t.Fatalf("got EOB=%s err=%v, want no EOB naming the unmatched item", eob, err)
	}
}

// TestDecisionEOB_ProductSystemNeverMinted: a request item coding its
// product with no system is carried with no system; the EOB never adds one.
func TestDecisionEOB_ProductSystemNeverMinted(t *testing.T) {
	request := eobRowRequest(t, func(c map[string]any) {
		c["item"] = []any{map[string]any{"sequence": 3, "productOrService": map[string]any{"coding": []any{map[string]any{"code": "72148"}}}}}
	})
	eob, err := decisionEOB("corr-row", "Patient/MBR-COVERED", "", "72148", "", shnsdk.PriorAuthResult{Outcome: "approved"},
		decisionEOBSource{request: request, itemIndex: -1, claimResponse: eobRowClaimResponse(t, nil)})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(eob, []byte(`"productOrService":{"coding":[{"code":"72148"}]}`)) || bytes.Contains(eob, []byte("ama-assn")) {
		t.Fatalf("productOrService minted a system:\n%s", eob)
	}
}

// TestDecisionEOB_InquiryPairsByTraceNumber: on an inquiry the
// ClaimResponse item carried is the one bearing the trace number that picked
// the inquiry line, never one picked by sequence: two payer items whose
// sequences cross the inquiry's lines leak nothing into each other, and the
// review action numbers in preAuthRef follow the same item. A line picked
// without a trace number pairs with no payer item.
func TestDecisionEOB_InquiryPairsByTraceNumber(t *testing.T) {
	traceExt := func(v string) []any {
		return []any{map[string]any{"url": pasExtItemTraceNumberURL, "valueIdentifier": map[string]any{"system": "http://example.org/ITEM_TRACE_NUMBER", "value": v}}}
	}
	request := eobRowRequest(t, func(c map[string]any) {
		c["item"] = []any{
			map[string]any{"sequence": 1, "extension": traceExt("TRN-A"), "productOrService": map[string]any{"coding": []any{map[string]any{"system": eobRowCPT, "code": "72148"}}}},
			map[string]any{"sequence": 2, "extension": traceExt("TRN-B"), "productOrService": map[string]any{"coding": []any{map[string]any{"system": eobRowCPT, "code": "72149"}}}},
		}
	})
	// The payer's items cross the inquiry's sequences: sequence 2 answers
	// TRN-A, sequence 1 answers TRN-B.
	cr := eobRowClaimResponse(t, func(cr map[string]any) {
		cr["item"] = []any{
			map[string]any{"itemSequence": 2, "extension": traceExt("TRN-A"), "adjudication": []any{json.RawMessage(payerRA("A3", "N-1"))}},
			map[string]any{"itemSequence": 1, "extension": traceExt("TRN-B"), "adjudication": []any{json.RawMessage(payerRA("A1", "N-2"))}},
		}
	})
	eob, err := decisionEOB("corr-row", "Patient/MBR-COVERED", eobRowCPT, "72149", "", shnsdk.PriorAuthResult{Outcome: "approved"},
		decisionEOBSource{request: request, itemIndex: 1, itemTrace: "http://example.org/ITEM_TRACE_NUMBER|TRN-B", inquiry: true, claimResponse: cr})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(eob, []byte(eobRA("A1", "N-2"))) || bytes.Contains(eob, []byte(`"A3"`)) || bytes.Contains(eob, []byte("N-1")) ||
		!bytes.Contains(eob, []byte(`"preAuthRef":["N-2"]`)) || !bytes.Contains(eob, []byte(`"sequence":2`)) {
		t.Fatalf("the inquiry EOB must carry only the TRN-B item's adjudication and number:\n%s", eob)
	}

	// A line picked without a trace number pairs with no payer item, so the
	// payer's decision is on no adjudication the EOB could carry: no EOB is
	// written (an EOB with no item decision would read as an approval), and
	// the finding names the position only.
	eob, err = decisionEOB("corr-row", "Patient/MBR-COVERED", eobRowCPT, "72148", "", shnsdk.PriorAuthResult{Outcome: "denied"},
		decisionEOBSource{request: request, itemIndex: 0, inquiry: true, claimResponse: cr})
	var nw *eobNotWritten
	if !errors.As(err, &nw) || eob != nil || err.Error() != "decision EOB not written: "+eobNoInquiryItemAnswered {
		t.Fatalf("got EOB=%s err=%v, want no EOB for a line picked without a trace number", eob, err)
	}
}

// TestDecisionEOB_InquiryWithoutTraceNumberWritesNoEOB: an inquiry whose
// line states no item trace number, answered as the reference payer answers
// (its own trace numbers on the item, its decision on the item's review
// action; testdata/brpayer-current known-member), pairs with no payer item.
// The decision is on no adjudication the EOB could carry, so no EOB is
// written: never an EOB that carries the request item without the payer's
// decision on it.
func TestDecisionEOB_InquiryWithoutTraceNumberWritesNoEOB(t *testing.T) {
	request := eobRowRequest(t, func(c map[string]any) {
		c["item"] = []any{map[string]any{"sequence": 1, "productOrService": map[string]any{"coding": []any{map[string]any{"system": eobRowCPT, "code": "72148"}}}}}
	})
	cr := eobRowClaimResponse(t, func(cr map[string]any) {
		cr["item"] = []any{json.RawMessage(`{"extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemTraceNumber","valueIdentifier":{"system":"urn:trnorg:PASPAYER","value":"AUTH-TRN0001"}}],` +
			`"itemSequence":1,"adjudication":[{"extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewAction","extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewActionCode","valueCodeableConcept":{"coding":[{"system":"https://codesystem.x12.org/005010/306","code":"A3","display":"Not Required"}]}}]}],` +
			`"category":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/adjudication","code":"submitted","display":"Submitted Amount"}]}}]}`)}
	})
	item, trace := inquiryItemFor(pasInquiryFacts{items: []pasInquiryItemFact{{index: 0, system: eobRowCPT, code: "72148"}}}, []string{"urn:trnorg:PASPAYER|AUTH-TRN0001"})
	if trace != "" {
		t.Fatalf("a line with no trace number was matched by trace %q", trace)
	}
	eob, err := decisionEOB("corr-row", "Patient/MBR-COVERED", item.system, item.code, "", shnsdk.PriorAuthResult{Outcome: "denied"},
		decisionEOBSource{request: request, itemIndex: item.index, itemTrace: trace, inquiry: true, claimResponse: cr})
	var nw *eobNotWritten
	if !errors.As(err, &nw) || eob != nil || err.Error() != "decision EOB not written: "+eobNoInquiryItemAnswered {
		t.Fatalf("got EOB=%s err=%v, want no EOB", eob, err)
	}
}

// TestDecisionEOB_SubmitNoPayerItemAnswersWritesNoEOB: on submit, a payer
// whose items answer none of the sequence of the request item that codes the
// order states its decision on no adjudication the EOB could carry: no EOB is
// written, and the finding names the position only.
func TestDecisionEOB_SubmitNoPayerItemAnswersWritesNoEOB(t *testing.T) {
	cr := eobRowClaimResponse(t, func(cr map[string]any) {
		cr["item"] = []any{map[string]any{"itemSequence": 2, "adjudication": []any{json.RawMessage(payerRA("A3", "X-2"))}}}
	})
	eob, err := eobRowBuild(t, eobRowRequest(t, nil), cr, shnsdk.PriorAuthResult{Outcome: "denied"})
	var nw *eobNotWritten
	if !errors.As(err, &nw) || eob != nil || err.Error() != "decision EOB not written: "+eobNoSubmitItemAnswered {
		t.Fatalf("got EOB=%s err=%v, want no EOB", eob, err)
	}
	// No payer items at all: the same.
	cr = eobRowClaimResponse(t, func(cr map[string]any) { delete(cr, "item") })
	eob, err = eobRowBuild(t, eobRowRequest(t, nil), cr, shnsdk.PriorAuthResult{Outcome: "approved"})
	if !errors.As(err, &nw) || eob != nil || err.Error() != "decision EOB not written: "+eobNoSubmitItemAnswered {
		t.Fatalf("got EOB=%s err=%v, want no EOB", eob, err)
	}
}

// TestDecisionEOB_HeaderAndAddItemAdjudication: a payer that states
// its review action on the ClaimResponse header adjudication or on an addItem
// for the answered item has it carried, transcribed as the item's, with its
// number in preAuthRef. An addItem for another item is not carried. A review
// action that cannot be carried there writes no EOB, named by position.
func TestDecisionEOB_HeaderAndAddItemAdjudication(t *testing.T) {
	addFor := func(seq int, code string) map[string]any {
		return map[string]any{"itemSequence": []any{seq}, "productOrService": map[string]any{"coding": []any{map[string]any{"code": code}}},
			"adjudication": []any{json.RawMessage(payerRA("A1", ""))}}
	}
	cr := eobRowClaimResponse(t, func(cr map[string]any) {
		cr["adjudication"] = []any{json.RawMessage(payerRA("A1", "H-1"))}
		cr["addItem"] = []any{addFor(3, "E0431"), addFor(2, "E9999")}
	})
	eob, err := eobRowBuild(t, eobRowRequest(t, nil), cr, shnsdk.PriorAuthResult{Outcome: "approved"})
	if err != nil {
		t.Fatal(err)
	}
	if h := eobRawList(t, eob, "adjudication"); len(h) != 1 || string(h[0]) != eobRA("A1", "H-1") {
		t.Fatalf("header adjudication = %s", h)
	}
	add := eobRawList(t, eob, "addItem")
	if len(add) != 1 || !bytes.Contains(add[0], []byte("E0431")) || !bytes.Contains(add[0], []byte(eobRA("A1", ""))) {
		t.Fatalf("addItem = %s, want only item 3's, transcribed", add)
	}
	if !bytes.Contains(eob, []byte(`"preAuthRef":["H-1"]`)) {
		t.Fatalf("the header's number is not stated:\n%s", eob)
	}

	cr = eobRowClaimResponse(t, func(cr map[string]any) {
		cr["adjudication"] = []any{json.RawMessage(`{"extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewAction","extension":[{"url":"number","valueString":"x"},{"url":"http://payer.example/y"}]}]}`)}
	})
	eob, err = eobRowBuild(t, eobRowRequest(t, nil), cr, shnsdk.PriorAuthResult{Outcome: "approved"})
	if eob != nil || err == nil || err.Error() != "decision EOB not written: review action sub-extension 2 has no counterpart in the EOB review action" {
		t.Fatalf("got EOB=%s err=%v, want no EOB naming the header's sub-extension by position", eob, err)
	}
}

// TestDecisionEOB_InquiryAddItemSelectedByTheRequestItem: on an inquiry the
// payer's item answering the line (by trace number) may number its own
// sequence differently from the request line. An addItem tied to the request
// line's sequence is carried; one tied only to the payer item's own sequence
// cannot be carried as written (the EOB's one item is the request line's
// sequence), so no EOB is written and the finding names it by position —
// never dropped silently.
func TestDecisionEOB_InquiryAddItemSelectedByTheRequestItem(t *testing.T) {
	trace := []any{map[string]any{"url": pasExtItemTraceNumberURL, "valueIdentifier": map[string]any{"system": "http://example.org/T", "value": "L1"}}}
	request := eobRowRequest(t, func(c map[string]any) {
		c["item"] = []any{map[string]any{"sequence": 4, "extension": trace, "productOrService": map[string]any{"coding": []any{map[string]any{"system": eobRowCPT, "code": "72148"}}}}}
	})
	addFor := func(seq int, code string) map[string]any {
		return map[string]any{"itemSequence": []any{seq}, "productOrService": map[string]any{"coding": []any{map[string]any{"code": code}}},
			"adjudication": []any{json.RawMessage(payerRA("A1", ""))}}
	}
	build := func(addItems ...any) ([]byte, error) {
		cr := eobRowClaimResponse(t, func(cr map[string]any) {
			cr["item"] = []any{map[string]any{"itemSequence": 1, "extension": trace, "adjudication": []any{json.RawMessage(payerRA("A1", "N-1"))}}}
			cr["addItem"] = addItems
		})
		return decisionEOB("corr-row", "Patient/MBR-COVERED", eobRowCPT, "72148", "", shnsdk.PriorAuthResult{Outcome: "approved"},
			decisionEOBSource{request: request, itemIndex: 0, itemTrace: "http://example.org/T|L1", inquiry: true, claimResponse: cr})
	}
	eob, err := build(addFor(4, "E0431"), addFor(9, "E9999"))
	if err != nil {
		t.Fatal(err)
	}
	if add := eobRawList(t, eob, "addItem"); len(add) != 1 || !bytes.Contains(add[0], []byte("E0431")) {
		t.Fatalf("addItem = %s, want only the one tied to the request line's sequence", add)
	}
	eob, err = build(addFor(1, "E0431"))
	var nw *eobNotWritten
	if !errors.As(err, &nw) || eob != nil || err.Error() != "decision EOB not written: addItem 1 of the payer's ClaimResponse names an item sequence the EOB does not carry" {
		t.Fatalf("got EOB=%s err=%v, want no EOB naming the addItem by position", eob, err)
	}
}

// TestDecisionEOB_AddItemNamingAnotherItemTooIsNotCarried: an addItem tied to
// the answered item and to another item cannot be carried as written onto an
// EOB that carries only the one item: no EOB, the finding names it.
func TestDecisionEOB_AddItemNamingAnotherItemTooIsNotCarried(t *testing.T) {
	cr := eobRowClaimResponse(t, func(cr map[string]any) {
		cr["addItem"] = []any{map[string]any{"itemSequence": []any{3, 2}, "productOrService": map[string]any{"coding": []any{map[string]any{"code": "E0431"}}}}}
	})
	eob, err := eobRowBuild(t, eobRowRequest(t, nil), cr, shnsdk.PriorAuthResult{Outcome: "approved"})
	if eob != nil || err == nil || err.Error() != "decision EOB not written: addItem 1 of the payer's ClaimResponse names an item sequence the EOB does not carry" {
		t.Fatalf("got EOB=%s err=%v", eob, err)
	}
}
