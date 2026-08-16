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
	"testing"

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
	h := composeObserverHandler(hub.Handler())

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
}

func TestDemoTransformEndpoint_RefusalSemanticChange(t *testing.T) {
	hub := observer.NewHub()
	h := composeObserverHandler(hub.Handler())

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
		Refusal        string `json:"refusal"`
		SemanticChange bool   `json:"semanticChange"`
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
}

// TestDemoTransformEndpoint_UnknownChainRefusal proves an unrecognized
// contract/line pair (chainFor's nil-hole path) refuses with 422 and
// semanticChange:false — a plain error, not a typed *SemanticChangeError —
// naming the chain that has no manifest row.
func TestDemoTransformEndpoint_UnknownChainRefusal(t *testing.T) {
	hub := observer.NewHub()
	h := composeObserverHandler(hub.Handler())

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
	h := composeObserverHandler(hub.Handler())

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
	h := composeObserverHandler(hub.Handler())

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
	h := composeObserverHandler(hub.Handler())

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
