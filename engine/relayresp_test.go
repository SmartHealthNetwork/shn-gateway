// gateway/engine/relayresp_test.go
package engine

import (
	"encoding/json"
	"testing"
)

func TestRelayWrap_JSONBody_RoundTrips(t *testing.T) {
	oo := []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing"}]}`)
	w := wrapRelayResponse(502, oo)
	// wrapper must be valid JSON carrying the sentinel key
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(w, &probe); err != nil {
		t.Fatalf("wrapper not valid JSON: %v", err)
	}
	if _, ok := probe["__shnStatus"]; !ok {
		t.Fatal("wrapper missing __shnStatus")
	}
	status, body, wrapped := unwrapRelayResponse(w)
	if !wrapped || status != 502 {
		t.Fatalf("unwrap: wrapped=%v status=%d, want true/502", wrapped, status)
	}
	if string(body) != string(oo) {
		t.Fatalf("body not byte-verbatim:\n got %s\nwant %s", body, oo)
	}
}

func TestRelayWrap_NonJSONBody_Base64Fallback(t *testing.T) {
	raw := []byte("<html>Bad Gateway</html>")
	status, body, wrapped := unwrapRelayResponse(wrapRelayResponse(500, raw))
	if !wrapped || status != 500 || string(body) != string(raw) {
		t.Fatalf("non-JSON round-trip failed: wrapped=%v status=%d body=%q", wrapped, status, body)
	}
}

func TestRelayUnwrap_BareFHIR_NotDetectedAsWrapped(t *testing.T) {
	// A bare FHIR body with a primitive-extension sibling _status must NOT be read as a wrapper.
	fhir := []byte(`{"resourceType":"ClaimResponse","status":"active","_status":{"id":"x"}}`)
	status, body, wrapped := unwrapRelayResponse(fhir)
	if wrapped {
		t.Fatal("bare FHIR misdetected as wrapped")
	}
	if status != 200 || string(body) != string(fhir) {
		t.Fatalf("bare payload: status=%d body=%s, want 200 + verbatim", status, body)
	}
}

func TestRelayError_Error(t *testing.T) {
	e := &RelayError{Status: 502, Body: []byte(`{"x":1}`)}
	if e.Error() == "" {
		t.Fatal("RelayError.Error() empty")
	}
}
