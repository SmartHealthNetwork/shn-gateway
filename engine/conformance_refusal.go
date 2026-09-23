package engine

import (
	"encoding/json"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

// safeRefusal carries only closed rule metadata and a bounded configured holder
// label. Validator diagnostics and payload values never enter this carrier.
func (e *conformanceError) safeRefusal() *conformanceError {
	out := *e
	if len(out.Gateway) > 256 {
		out.Gateway = "sha256:" + sha256hex([]byte(out.Gateway))
	}
	return &out
}

// operationOutcome preserves machine-readable refusal metadata in a FHIR
// BackboneElement extension. The issue code describes invalidity separately
// from an enforced check whose evidence is unavailable.
func (e *conformanceError) operationOutcome() any {
	safe := e.safeRefusal()
	code := "invalid"
	if safe.Category == "conformance_unavailable" {
		code = "transient"
	}
	fields := []map[string]string{
		{"url": "category", "valueString": safe.Category},
		{"url": "rule", "valueString": safe.Rule},
		{"url": "gateway", "valueString": safe.Gateway},
		{"url": "level", "valueString": safe.Level},
		{"url": "direction", "valueString": safe.Direction},
	}
	return map[string]any{"resourceType": "OperationOutcome", "issue": []any{map[string]any{
		"severity": "error", "code": code, "extension": []any{map[string]any{"url": "urn:shn:conformance-refusal", "extension": fields}},
	}}}
}

func (e *conformanceError) refusalPayload(leg string) (relay.Payload, error) {
	var value any = e.safeRefusal()
	media := "application/json"
	if !strings.HasPrefix(leg, "crd-") {
		value = e.operationOutcome()
		media = "application/fhir+json"
	}
	body, err := json.Marshal(value)
	if err != nil {
		var empty relay.Payload
		return empty, err
	}
	return relay.Authored(relay.BuilderGatewayRefusal, body, media)
}
