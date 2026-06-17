package engine

import "encoding/json"

// resourceRef extracts a "<resourceType>/<id>" reference from a FHIR resource's JSON, so a
// Provenance (or any downstream reference) targets the resource's actual server-assigned id
// rather than a hardcoded literal (wiring Flag 1). Returns ok=false when the bytes are not
// parseable JSON or lack resourceType/id.
func resourceRef(b []byte) (ref string, ok bool) {
	var probe struct {
		ResourceType string `json:"resourceType"`
		ID           string `json:"id"`
	}
	if json.Unmarshal(b, &probe) != nil || probe.ResourceType == "" || probe.ID == "" {
		return "", false
	}
	return probe.ResourceType + "/" + probe.ID, true
}
