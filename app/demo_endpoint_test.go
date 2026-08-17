// demo_endpoint_test.go — POST /demo/transform on the observer listener's mux
// httptest against the composed observer handler exactly as build()
// wires it (composeObserverHandler wrapping observer.NewHub().Handler()),
// never against a live network listener.
package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine"
	"github.com/SmartHealthNetwork/shn-gateway/observer"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// demoGolden reads a real per-line golden fixture vendored into this module
// at gateway/app/testdata/golden/ (see that directory's README.md) — a copy
// of the repo-root testdata/golden/ fixture of the same relPath, kept in
// sync by the root module's gateway_vendored_golden_drift_test.go. Vendored
// (rather than reached for above the module root) so this test still passes
// in the gateway module published standalone, which does not carry the repo
// root's testdata/ alongside it.
func demoGolden(t *testing.T, relPath string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "golden", relPath))
	if err != nil {
		t.Fatalf("read golden %s: %v", relPath, err)
	}
	return raw
}

func TestDemoTransformEndpoint_HappyPath(t *testing.T) {
	hub := observer.NewHub()
	h := composeObserverHandler(hub.Handler(), &engine.Gateway{}, false)

	in21 := demoGolden(t, "2.1/questionnaireresponse-autofill.json")
	want22 := demoGolden(t, "2.2/questionnaireresponse-autofill.json")

	reqBody, err := json.Marshal(map[string]any{
		"contract": "pa.dtr",
		"from":     "2.1",
		"to":       "2.2",
		"payload":  json.RawMessage(in21),
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/demo/transform", bytes.NewReader(reqBody))
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Output      json.RawMessage     `json:"output"`
		LossReports []engine.LossReport `json:"lossReports"`
		Chain       []engine.ChainStep  `json:"chain"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rr.Body.String())
	}

	var got, want any
	if err := json.Unmarshal(resp.Output, &got); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if err := json.Unmarshal(want22, &want); err != nil {
		t.Fatalf("decode want22: %v", err)
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("output mismatch:\ngot:  %s\nwant: %s", gotJSON, wantJSON)
	}

	if len(resp.LossReports) != 1 {
		t.Fatalf("lossReports = %+v, want exactly 1 step report round-tripped", resp.LossReports)
	}
	if resp.LossReports[0].Module != "pa.dtr 2.1->2.2" {
		t.Fatalf("lossReports[0].Module = %q, want %q", resp.LossReports[0].Module, "pa.dtr 2.1->2.2")
	}

	wantChain := []engine.ChainStep{{Module: "pa.dtr 2.1->2.2", From: "2.1", To: "2.2", Class: "carry"}}
	if !reflect.DeepEqual(resp.Chain, wantChain) {
		t.Fatalf("chain = %+v, want %+v", resp.Chain, wantChain)
	}
}

func TestDemoTransformEndpoint_RefusalSemanticChange(t *testing.T) {
	hub := observer.NewHub()
	h := composeObserverHandler(hub.Handler(), &engine.Gateway{}, false)

	multiCoverageQR := []byte(`{
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

	reqBody, err := json.Marshal(map[string]any{
		"contract": "pa.dtr",
		"from":     "2.1",
		"to":       "2.2",
		"payload":  json.RawMessage(multiCoverageQR),
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/demo/transform", bytes.NewReader(reqBody))
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Refusal        string             `json:"refusal"`
		SemanticChange bool               `json:"semanticChange"`
		Chain          []engine.ChainStep `json:"chain"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rr.Body.String())
	}
	if !resp.SemanticChange {
		t.Fatalf("semanticChange = false, want true (a *SemanticChangeError refusal)")
	}
	if resp.Refusal == "" {
		t.Fatalf("refusal is empty, want the error text")
	}

	wantChain := []engine.ChainStep{{Module: "pa.dtr 2.1->2.2", From: "2.1", To: "2.2", Class: "carry"}}
	if !reflect.DeepEqual(resp.Chain, wantChain) {
		t.Fatalf("chain = %+v, want %+v (the attempted chain, even though the run refused)", resp.Chain, wantChain)
	}
}

// TestDemoTransformEndpoint_UnknownChainRefusal proves an unrecognized
// contract/line pair (chainFor's nil-hole path) refuses with 422 and
// semanticChange:false — a plain error, not a typed *SemanticChangeError —
// naming the chain that has no manifest row.
func TestDemoTransformEndpoint_UnknownChainRefusal(t *testing.T) {
	hub := observer.NewHub()
	h := composeObserverHandler(hub.Handler(), &engine.Gateway{}, false)

	reqBody, err := json.Marshal(map[string]any{
		"contract": "pa.dtr",
		"from":     "1.0",
		"to":       "9.9",
		"payload":  json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/demo/transform", bytes.NewReader(reqBody))
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Refusal        string `json:"refusal"`
		SemanticChange bool   `json:"semanticChange"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rr.Body.String())
	}
	if resp.SemanticChange {
		t.Fatalf("semanticChange = true, want false (an unknown-chain error, not a typed SemanticChangeError)")
	}
	if resp.Refusal == "" {
		t.Fatalf("refusal is empty, want the error text naming the chain")
	}
}

// TestDemoTransformEndpoint_OverLimitBodyRejected proves the request body is
// bounded exactly like every other inbound handler in this codebase
// (shnsdk.MaxRequestBytes, 8 MiB) — SHN Kit proxies partner-facing traffic
// here, so an unbounded Decode would be a memory-exhaustion vector fed by
// attacker-controlled bytes. Same over-limit construction idiom as
// sdk/transport_test.go's TestDecodeJSONBody_TooLarge: a valid JSON object
// whose string value alone exceeds MaxRequestBytes.
func TestDemoTransformEndpoint_OverLimitBodyRejected(t *testing.T) {
	hub := observer.NewHub()
	h := composeObserverHandler(hub.Handler(), &engine.Gateway{}, false)

	val := bytes.Repeat([]byte("a"), shnsdk.MaxRequestBytes+1)
	body := append([]byte(`{"contract":"pa.dtr","from":"2.1","to":"2.2","payload":{},"pad":"`), val...)
	body = append(body, []byte(`"}`)...)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/demo/transform", bytes.NewReader(body))
	h.ServeHTTP(rr, req)

	if rr.Code == http.StatusOK {
		t.Fatalf("status = 200, want a rejection for an over-limit body (no chain should run); body=%s", rr.Body.String())
	}
	var resp struct {
		Output json.RawMessage `json:"output"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Output != nil {
		t.Fatalf("an over-limit request must not run the chain, got output=%s", resp.Output)
	}
}

func TestDemoTransformEndpoint_MalformedJSON(t *testing.T) {
	hub := observer.NewHub()
	h := composeObserverHandler(hub.Handler(), &engine.Gateway{}, false)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/demo/transform", bytes.NewReader([]byte(`{not valid json`)))
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

// TestDemoTransformEndpoint_IdentityFromEqualsTo proves the from==to edge
// (an empty chain — chainFor returns []CompatStep{}, applyChain's loop never
// runs) round-trips the payload unchanged and reports an EMPTY array, never
// null, on the wire — SHN Kit's exhibit proxy relies on "lossReports is always
// an array". Asserted against the RAW response
// body, not a decoded struct, so a nil-vs-[] slice distinction that Go's json
// decoder would silently absorb can't hide a regression.
func TestDemoTransformEndpoint_IdentityFromEqualsTo(t *testing.T) {
	hub := observer.NewHub()
	h := composeObserverHandler(hub.Handler(), &engine.Gateway{}, false)

	in21 := demoGolden(t, "2.1/questionnaireresponse-autofill.json")
	reqBody, err := json.Marshal(map[string]any{
		"contract": "pa.dtr",
		"from":     "2.1",
		"to":       "2.1",
		"payload":  json.RawMessage(in21),
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/demo/transform", bytes.NewReader(reqBody))
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte(`"lossReports":[]`)) {
		t.Fatalf("raw response body does not contain %q (want an empty array, not null): %s", `"lossReports":[]`, rr.Body.String())
	}

	var resp struct {
		Output json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var got, want any
	if err := json.Unmarshal(resp.Output, &got); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if err := json.Unmarshal(in21, &want); err != nil {
		t.Fatalf("decode want: %v", err)
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("identity from==to must round-trip the payload unchanged:\ngot:  %s\nwant: %s", gotJSON, wantJSON)
	}
}

// TestDemoTransformEndpoint_AbsentWhenUnregistered proves /demo/transform
// exists ONLY through composeObserverHandler's explicit wiring — the raw hub
// handler (what obsHandler would serve if OBSERVER_ADDR's build() branch
// never wrapped it, i.e. the ObserverAddr=="" posture where obsHandler stays
// nil and the endpoint is never reachable at all) 404s.
func TestDemoTransformEndpoint_AbsentWhenUnregistered(t *testing.T) {
	hub := observer.NewHub()
	rawHubHandler := hub.Handler() // NOT composeObserverHandler-wrapped

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/demo/transform", bytes.NewReader([]byte(`{}`)))
	rawHubHandler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (endpoint absent without composeObserverHandler)", rr.Code)
	}
}

// TestDemoCaptureEndpoint_FlagOff404 proves the endpoint 404s with the
// distinct "edge capture is not enabled" body whenever edgeCaptureEnabled is
// false — the flag-off case, regardless of what (if anything) the store
// holds.
func TestDemoCaptureEndpoint_FlagOff404(t *testing.T) {
	hub := observer.NewHub()
	h := composeObserverHandler(hub.Handler(), &engine.Gateway{}, false)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/demo/capture/demo-1", nil)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rr.Body.String())
	}
	if resp.Error != "edge capture is not enabled" {
		t.Fatalf("error = %q, want %q", resp.Error, "edge capture is not enabled")
	}
}

// TestDemoCaptureEndpoint_FlagOnMissingID404 proves the endpoint 404s with
// the distinct "no capture for this leg" body when the flag is on but no
// leg has ever recorded under the requested id — a different body from the
// flag-off case, so a caller can tell the two apart.
func TestDemoCaptureEndpoint_FlagOnMissingID404(t *testing.T) {
	hub := observer.NewHub()
	h := composeObserverHandler(hub.Handler(), &engine.Gateway{}, true)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/demo/capture/never-recorded", nil)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rr.Body.String())
	}
	if resp.Error != "no capture for this leg" {
		t.Fatalf("error = %q, want %q", resp.Error, "no capture for this leg")
	}
}

// TestDemoCaptureEndpoint_FlagOnPresentID200 drives a transformed leg
// through the SAME fixture path TestDemoTransformEndpoint_HappyPath uses
// (engine.RunTransformChain over the vendored 2.1/2.2 golden pair), seeds
// the gateway's edge-capture store with the result via
// RecordEdgeCaptureForTest, then proves every field round-trips through the
// real HTTP endpoint.
func TestDemoCaptureEndpoint_FlagOnPresentID200(t *testing.T) {
	hub := observer.NewHub()
	gw := &engine.Gateway{}

	in21 := demoGolden(t, "2.1/questionnaireresponse-autofill.json")
	want22 := demoGolden(t, "2.2/questionnaireresponse-autofill.json")

	const correlationID = "demo-capture-test-1"
	const legType = "pa.dtr:2.1->2.2"
	x := engine.ExchangeIdentity{CorrelationID: correlationID, LegType: legType}
	out, reports, err := engine.RunTransformChain("pa.dtr", "2.1", "2.2", in21, x)
	if err != nil {
		t.Fatalf("RunTransformChain: %v", err)
	}
	chain := engine.ChainSteps("pa.dtr", "2.1", "2.2")
	capturedAt := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	gw.RecordEdgeCaptureForTest(engine.EdgeCapture{
		CorrelationID: correlationID,
		LegType:       legType,
		Contract:      "pa.dtr",
		From:          "2.1",
		To:            "2.2",
		Chain:         chain,
		LossReports:   reports,
		Before:        in21,
		After:         out,
		CapturedAt:    capturedAt,
	})

	h := composeObserverHandler(hub.Handler(), gw, true)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/demo/capture/"+correlationID, nil)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		CorrelationID string              `json:"correlationId"`
		LegType       string              `json:"legType"`
		Contract      string              `json:"contract"`
		From          string              `json:"from"`
		To            string              `json:"to"`
		Chain         []engine.ChainStep  `json:"chain"`
		LossReports   []shnsdk.LossReport `json:"lossReports"`
		Before        json.RawMessage     `json:"before"`
		After         json.RawMessage     `json:"after"`
		CapturedAt    time.Time           `json:"capturedAt"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rr.Body.String())
	}

	if resp.CorrelationID != correlationID {
		t.Fatalf("correlationId = %q, want %q", resp.CorrelationID, correlationID)
	}
	if resp.LegType != legType {
		t.Fatalf("legType = %q, want %q", resp.LegType, legType)
	}
	if resp.Contract != "pa.dtr" || resp.From != "2.1" || resp.To != "2.2" {
		t.Fatalf("contract/from/to = %q/%q/%q, want pa.dtr/2.1/2.2", resp.Contract, resp.From, resp.To)
	}
	wantChain := []engine.ChainStep{{Module: "pa.dtr 2.1->2.2", From: "2.1", To: "2.2", Class: "carry"}}
	if !reflect.DeepEqual(resp.Chain, wantChain) {
		t.Fatalf("chain = %+v, want %+v", resp.Chain, wantChain)
	}
	if len(resp.LossReports) != 1 || resp.LossReports[0].Module != "pa.dtr 2.1->2.2" {
		t.Fatalf("lossReports = %+v, want exactly 1 round-tripped report for pa.dtr 2.1->2.2", resp.LossReports)
	}
	if !resp.CapturedAt.Equal(capturedAt) {
		t.Fatalf("capturedAt = %v, want %v", resp.CapturedAt, capturedAt)
	}

	var gotBefore, wantBefore any
	if err := json.Unmarshal(resp.Before, &gotBefore); err != nil {
		t.Fatalf("decode before: %v", err)
	}
	if err := json.Unmarshal(in21, &wantBefore); err != nil {
		t.Fatalf("decode want before: %v", err)
	}
	gotBeforeJSON, _ := json.Marshal(gotBefore)
	wantBeforeJSON, _ := json.Marshal(wantBefore)
	if string(gotBeforeJSON) != string(wantBeforeJSON) {
		t.Fatalf("before mismatch:\ngot:  %s\nwant: %s", gotBeforeJSON, wantBeforeJSON)
	}

	var gotAfter, wantAfter any
	if err := json.Unmarshal(resp.After, &gotAfter); err != nil {
		t.Fatalf("decode after: %v", err)
	}
	if err := json.Unmarshal(want22, &wantAfter); err != nil {
		t.Fatalf("decode want after: %v", err)
	}
	gotAfterJSON, _ := json.Marshal(gotAfter)
	wantAfterJSON, _ := json.Marshal(wantAfter)
	if string(gotAfterJSON) != string(wantAfterJSON) {
		t.Fatalf("after mismatch:\ngot:  %s\nwant: %s", gotAfterJSON, wantAfterJSON)
	}
}

// TestDemoCaptureEndpoint_NilLossReportsAndChainNormalizeToEmptyArray proves
// a captured leg with nil LossReports AND a nil Chain (a real, if uncommon,
// shape — e.g. a carve-out leg the store still recorded) serializes BOTH as
// `[]`, never JSON `null`: the wire contract's `lossReports` and `chain`
// fields are always arrays a caller can range over without a nil check
// (`chain` rides the same normalization `lossReports` already had, and the
// ui's BridgingCapture type consumes both).
func TestDemoCaptureEndpoint_NilLossReportsAndChainNormalizeToEmptyArray(t *testing.T) {
	hub := observer.NewHub()
	gw := &engine.Gateway{}

	const correlationID = "demo-capture-nil-loss"
	gw.RecordEdgeCaptureForTest(engine.EdgeCapture{
		CorrelationID: correlationID,
		LegType:       "dtr-questionnaire-fetch",
		Contract:      "pa.dtr",
		From:          "2.0",
		To:            "2.0",
		Chain:         nil,
		LossReports:   nil,
		Before:        []byte(`{"a":1}`),
		After:         []byte(`{"a":1}`),
		CapturedAt:    time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
	})

	h := composeObserverHandler(hub.Handler(), gw, true)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/demo/capture/"+correlationID, nil)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rr.Body.String())
	}
	if got := string(raw["lossReports"]); got != "[]" {
		t.Fatalf("lossReports = %s, want the empty array literal `[]`, never `null`", got)
	}
	if got := string(raw["chain"]); got != "[]" {
		t.Fatalf("chain = %s, want the empty array literal `[]`, never `null`", got)
	}
}

// TestDemoCaptureEndpoint_NilPayloadsMarshalAsNull proves a captured
// leg with a nil Before/After — a real captured shape, not just a
// theoretical one — round-trips through the wire endpoint as a well-formed
// body with `before`/`after` as the JSON literal `null`, never a broken or
// truncated response. Before this fix, edgeCaptureStore.Record's deep copy
// turned a nil json.RawMessage into a non-nil EMPTY one on the way in, which
// encoding/json refuses to marshal — and since writeDemoJSON has already
// written the 200 status header by the time Encode fails, the client got a
// 200 with an empty/truncated body instead of an error.
func TestDemoCaptureEndpoint_NilPayloadsMarshalAsNull(t *testing.T) {
	hub := observer.NewHub()
	gw := &engine.Gateway{}

	const correlationID = "demo-capture-nil-payload"
	gw.RecordEdgeCaptureForTest(engine.EdgeCapture{
		CorrelationID: correlationID,
		LegType:       "dtr-questionnaire-fetch",
		Contract:      "pa.dtr",
		From:          "2.0",
		To:            "2.0",
		Before:        nil,
		After:         nil,
		CapturedAt:    time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
	})

	h := composeObserverHandler(hub.Handler(), gw, true)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/demo/capture/"+correlationID, nil)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("response body is not valid JSON (want a well-formed body even for nil payloads): %v (body=%s)", err, rr.Body.String())
	}
	if got := string(raw["before"]); got != "null" {
		t.Fatalf(`before = %s, want the JSON literal "null"`, got)
	}
	if got := string(raw["after"]); got != "null" {
		t.Fatalf(`after = %s, want the JSON literal "null"`, got)
	}
}

// TestDemoCaptureEndpoint_POSTMethodNotAllowed proves POST against
// /demo/capture/{correlationId} 405s. The mux pattern is registered
// WITHOUT a method (composeObserverHandler's own doc comment explains why:
// a method-restricted "GET ..." pattern here would lose to the mux's
// method-less "/" catch-all for a POST request — "/" still matches, so
// ServeMux never reaches its own 405 logic, and the request would 404
// through the hub instead). The 405 this test asserts comes ENTIRELY from
// handleDemoCapture's own explicit r.Method guard — there is no mux-level
// safety net here. This test is that guard's only regression coverage:
// deleting the guard silently turns POST from 405 into 404, and nothing
// else in this suite would catch it.
func TestDemoCaptureEndpoint_POSTMethodNotAllowed(t *testing.T) {
	hub := observer.NewHub()
	h := composeObserverHandler(hub.Handler(), &engine.Gateway{}, true)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/demo/capture/demo-1", nil)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405; body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Allow"); got != http.MethodGet {
		t.Fatalf("Allow header = %q, want %q", got, http.MethodGet)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "method not allowed") {
		t.Fatalf("body = %q, want it to mention %q", body, "method not allowed")
	}
}

// TestDemoCaptureEndpoint_EmptyID404 proves a request with no correlation id
// segment at all 404s (the registered pattern requires a path segment
// there, so ServeMux falls through to the catch-all "/" — never reaching
// the handler with an empty id).
func TestDemoCaptureEndpoint_EmptyID404(t *testing.T) {
	hub := observer.NewHub()
	h := composeObserverHandler(hub.Handler(), &engine.Gateway{}, true)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/demo/capture/", nil)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}
