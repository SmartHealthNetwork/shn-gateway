// ingress_discovery.go — the CDS Hooks /cds-services discovery the provider ingress
// advertises, and the services its POST /cds-services/{id} route dispatches: one per
// hook the network carries. What is advertised is exactly what is dispatched
// (TestCDSDiscovery_MatchesDispatch). The prefetch templates are the pinned PA set;
// for a key the EHR leaves out, the ingress obtains the value from the participant's
// own system of record with the same search (ingress_crd.go).
package engine

import "encoding/json"

// cdsIngressService is one CDS service the provider ingress offers: its id,
// the hook it answers, and the exchange leg that carries it.
type cdsIngressService struct {
	ID, Hook, Leg      string
	Title, Description string
}

// cdsIngressServices are the advertised services, in discovery order.
// appointment-book is not offered.
var cdsIngressServices = []cdsIngressService{
	{
		ID: "shn-order-sign", Hook: "order-sign", Leg: "crd-order-select",
		Title:       "Coverage Requirements Discovery (order sign)",
		Description: "Sends a signed order's coverage requirements request to the patient's payer over the Smart Health Network and returns the payer's answer as the payer sent it.",
	},
	{
		ID: "shn-order-select", Hook: "order-select", Leg: "crd-order-select",
		Title:       "Coverage Requirements Discovery (order select)",
		Description: "Sends a selected order's coverage requirements request to the patient's payer over the Smart Health Network and returns the payer's answer as the payer sent it.",
	},
	{
		ID: "shn-order-dispatch", Hook: "order-dispatch", Leg: "crd-order-dispatch",
		Title:       "Coverage Requirements Discovery (order dispatch)",
		Description: "Sends a dispatched order's coverage requirements request to the patient's payer over the Smart Health Network and returns the payer's answer as the payer sent it.",
	},
}

// cdsIngressServiceByID returns the advertised service with id (ids are
// matched exactly).
func cdsIngressServiceByID(id string) (cdsIngressService, bool) {
	for _, s := range cdsIngressServices {
		if s.ID == id {
			return s, true
		}
	}
	return cdsIngressService{}, false
}

// legForHook is the exchange leg that carries a CDS Hooks request with hook
// ("" for a hook the network does not carry).
func legForHook(hook string) string {
	for leg, hooks := range crdLegHooks {
		for _, h := range hooks {
			if h == hook {
				return leg
			}
		}
	}
	return ""
}

// pinnedPrefetchKeys is the PA-standard prefetch set the ingress advertises and, for a key
// the request leaves out, obtains from the participant's own system of record, in this order.
var pinnedPrefetchKeys = []string{
	"patient", "coverage", "serviceHistory", "deviceHistory", "medicationHistory", "questionnaireResponses",
}

// prefetchPatientKey is the one advertised key that is a read of the patient; every other
// advertised key is a patient search of the resource type prefetchSearchTypes names.
const prefetchPatientKey = "patient"

// prefetchSearchTypes maps each advertised search key to the resource type its template
// searches: `<type>?patient=Patient/{{context.patientId}}`.
var prefetchSearchTypes = map[string]string{
	"coverage":               "Coverage",
	"serviceHistory":         "ServiceRequest",
	"deviceHistory":          "DeviceRequest",
	"medicationHistory":      "MedicationRequest",
	"questionnaireResponses": "QuestionnaireResponse",
}

// prefetchTemplate is the advertised template for key.
func prefetchTemplate(key string) string {
	if key == prefetchPatientKey {
		return "Patient/{{context.patientId}}"
	}
	t := prefetchSearchTypes[key] + "?patient=Patient/{{context.patientId}}"
	if inc, ok := searchIncludes[prefetchSearchTypes[key]]; ok {
		t += "&_include=" + inc
	}
	return t
}

func cdsDiscoveryJSON() ([]byte, error) {
	prefetch := map[string]string{}
	for _, key := range pinnedPrefetchKeys {
		prefetch[key] = prefetchTemplate(key)
	}
	services := make([]map[string]any, 0, len(cdsIngressServices))
	for _, s := range cdsIngressServices {
		services = append(services, map[string]any{
			"hook":        s.Hook,
			"id":          s.ID,
			"title":       s.Title,
			"description": s.Description,
			"prefetch":    prefetch,
		})
	}
	return json.Marshal(map[string]any{"services": services})
}
