// ingress_discovery.go — the static CDS Hooks /cds-services discovery the provider ingress
// advertises so br-provider's resolvePrefetch knows what to inline. The prefetch templates are
// the pinned PA set, hand-authored from the two-oracle conformant shape (br-payer's +
// br-provider's reverse-engineered prefetch needs). DEFERRED: capturing br-payer's LIVE
// /cds-services as a fixture and a hermetic assertion that pinnedPrefetchKeys ⊇ that captured
// set — needs br-payer running; until then the superset relationship is asserted by the pinned
// conformant shape, not by a live-captured fixture.
package engine

import "encoding/json"

// crdIngressServiceID is the CDS service id the ingress route {id} matches and the leg
// forwards to. Pinned from br-payer's order-select service.
const crdIngressServiceID = "order-select-crd"

// pinnedPrefetchKeys is the PA-standard prefetch set the ingress advertises and (when a key
// is not inlined by the caller) resolves from the SoR.
var pinnedPrefetchKeys = []string{
	"patient", "coverage", "serviceHistory", "deviceHistory", "medicationHistory", "questionnaireResponses",
}

func cdsDiscoveryJSON() ([]byte, error) {
	prefetch := map[string]string{
		"patient":                "Patient/{{context.patientId}}",
		"coverage":               "Coverage?patient=Patient/{{context.patientId}}",
		"serviceHistory":         "ServiceRequest?patient=Patient/{{context.patientId}}",
		"deviceHistory":          "DeviceRequest?patient=Patient/{{context.patientId}}",
		"medicationHistory":      "MedicationRequest?patient=Patient/{{context.patientId}}",
		"questionnaireResponses": "QuestionnaireResponse?patient=Patient/{{context.patientId}}",
	}
	doc := map[string]any{
		"services": []map[string]any{{
			"hook":        "order-select",
			"id":          crdIngressServiceID,
			"title":       "SHN Smart Gateway — Coverage Requirements Discovery (ingress)",
			"description": "Provider-side Da Vinci ingress; forwards order-select to the payer via the SHN substrate.",
			"prefetch":    prefetch,
		}},
	}
	return json.Marshal(doc)
}
