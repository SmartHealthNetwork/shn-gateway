package engine

import (
	"encoding/json"
	"net/http"
)

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

// orderRefOrFail names the order an origination is about, as the participant's
// OWN system holds it — the reference the stored authorization number is filed
// under and the one a continuation re-reads the order by, months later.
//
// It replaced a per-scenario literal at each origination site. Those literals
// named an order the gateway had authored and no system held, so every
// authorization they filed, and every inquiry that tried to continue one, was
// keyed on a reference that resolved to nothing. An order with no identity is
// refused here rather than filed under a name nothing resolves.
func orderRefOrFail(w http.ResponseWriter, order []byte) (string, bool) {
	ref, ok := resourceRef(order)
	if !ok {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the member's open order carries no identity in the system of record"})
		return "", false
	}
	return ref, true
}
