// ingress_discovery.go — the CDS Hooks /cds-services discovery the provider ingress
// advertises, and the services its POST /cds-services/{id} route dispatches: one per
// hook the network carries. What is advertised is exactly what is dispatched
// (TestCDSDiscovery_MatchesDispatch). The prefetch templates are the pinned PA set;
// for a key the EHR leaves out, the ingress obtains the value from the participant's
// own system of record with a search narrowed the same way, which keeps the includes
// the template leaves out (ingress_crd.go, sorsearch.go).
package engine

import (
	"encoding/json"
	"fmt"
	"strings"
)

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

// ParseAdvertisedCDSHooks parses the CDS_ADVERTISE_HOOKS value: a comma-separated
// list of hooks the network carries (order-sign, order-select, order-dispatch),
// each at most once, at least one. Anything else is a configuration error the
// gateway refuses to boot on — an advertisement is a promise to the EHR, so a
// typo must not silently advertise nothing or everything.
func ParseAdvertisedCDSHooks(raw string) ([]string, error) {
	var hooks []string
	for _, part := range strings.Split(raw, ",") {
		h := strings.TrimSpace(part)
		if h == "" {
			continue
		}
		known := false
		for _, s := range cdsIngressServices {
			known = known || s.Hook == h
		}
		if !known {
			return nil, fmt.Errorf("CDS_ADVERTISE_HOOKS: unknown hook %q (the network carries %s)", h, strings.Join(cdsHookNames(), ", "))
		}
		for _, seen := range hooks {
			if seen == h {
				return nil, fmt.Errorf("CDS_ADVERTISE_HOOKS: hook %q listed twice", h)
			}
		}
		hooks = append(hooks, h)
	}
	if len(hooks) == 0 {
		return nil, fmt.Errorf("CDS_ADVERTISE_HOOKS: no hooks named (unset it to advertise every hook the network carries)")
	}
	return hooks, nil
}

// cdsHookNames lists every hook the network carries, in discovery order.
func cdsHookNames() []string {
	names := make([]string, 0, len(cdsIngressServices))
	for _, s := range cdsIngressServices {
		names = append(names, s.Hook)
	}
	return names
}

// advertisedCDSServices are the services this gateway advertises and
// dispatches: every service the network carries, narrowed to the configured
// hooks when Config.AdvertisedCDSHooks is set, in discovery order.
func (g *Gateway) advertisedCDSServices() []cdsIngressService {
	if g.cfg.AdvertisedCDSHooks == nil {
		return cdsIngressServices
	}
	var out []cdsIngressService
	for _, s := range cdsIngressServices {
		for _, h := range g.cfg.AdvertisedCDSHooks {
			if s.Hook == h {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

// advertisedCDSServiceByID returns the advertised service with id, and the ids
// this gateway does advertise, so a refusal can name what is offered.
func (g *Gateway) advertisedCDSServiceByID(id string) (cdsIngressService, []string, bool) {
	var offered []string
	var found cdsIngressService
	ok := false
	for _, s := range g.advertisedCDSServices() {
		offered = append(offered, s.ID)
		if s.ID == id {
			found, ok = s, true
		}
	}
	return found, offered, ok
}

// cdsDiscoveryJSON is this gateway's /cds-services listing.
func (g *Gateway) cdsDiscoveryJSON() ([]byte, error) {
	return cdsDiscoveryJSONFor(g.advertisedCDSServices())
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
// searches: `<type>?patient={{context.patientId}}&status=<codes>`.
var prefetchSearchTypes = map[string]string{
	"coverage":               "Coverage",
	"serviceHistory":         "ServiceRequest",
	"deviceHistory":          "DeviceRequest",
	"medicationHistory":      "MedicationRequest",
	"questionnaireResponses": "QuestionnaireResponse",
}

// prefetchStatus is the status each advertised search narrows to, by resource type: the
// Da Vinci reference payer's order-sign templates (CRD 2.2.1's example). It is the one
// table both the advertised template (prefetchTemplate) and the gateway's own search of
// its participant's system for the same value (prefetchSearchFilters) read, so what an
// EHR is asked for and what the gateway reads for it select the same records. The coverage
// the gateway routes by is not a filled value (it is read only to route, or carried by the
// gateway's own originated request): it asks for every Coverage and routes on the active
// ones first (obtainRoutingCoverage, routingCoverageChoice).
var prefetchStatus = map[string][]string{
	"Coverage":              {"active"},
	"ServiceRequest":        {"active", "completed"},
	"DeviceRequest":         {"active", "on-hold", "completed"},
	"MedicationRequest":     {"active", "completed"},
	"QuestionnaireResponse": {"completed"},
}

// prefetchSearchFilters is the narrowing of the gateway's search for a prefetch value of
// resourceType: the status its advertised template names (none for a type no template
// searches).
func prefetchSearchFilters(resourceType string) []SearchDateRange {
	codes, ok := prefetchStatus[resourceType]
	if !ok {
		return nil
	}
	return []SearchDateRange{{Param: "status", AnyOf: codes}}
}

// prefetchSearchQuery is the search the gateway runs for the advertised search key, for
// the system of record's patient sorID ("" when an input is invalid).
func prefetchSearchQuery(key, sorID string) string {
	rt := prefetchSearchTypes[key]
	q, _ := SoRSearchQuery(rt, sorID, prefetchSearchFilters(rt)...)
	return q
}

// prefetchTemplate is the advertised template for key. A search template names the patient
// by the bare id and narrows by the status prefetchStatus names, and carries no _include:
// CDS Hooks 2.0 does not list _include among the query features a client supports, so an
// EHR is not asked for one. The gateway's own search for the same value (SoRSearchQuery,
// prefetchSearchFilters) narrows the same way but keeps its include (searchIncludes): a
// value it obtains is carried to the payer, which cannot fetch the payor Organization or a
// device order's performer itself once fhirServer is removed.
func prefetchTemplate(key string) string {
	if key == prefetchPatientKey {
		return "Patient/{{context.patientId}}"
	}
	rt := prefetchSearchTypes[key]
	t := rt + "?patient={{context.patientId}}"
	for _, f := range prefetchSearchFilters(rt) {
		t += "&" + f.Param + "=" + strings.Join(f.AnyOf, ",")
	}
	return t
}

// cdsDiscoveryJSON is the listing of every service the network carries — what
// a gateway with no narrowing advertises.
func cdsDiscoveryJSON() ([]byte, error) {
	return cdsDiscoveryJSONFor(cdsIngressServices)
}

// cdsDiscoveryJSONFor renders the /cds-services listing for services, each
// carrying the pinned prefetch templates.
func cdsDiscoveryJSONFor(advertised []cdsIngressService) ([]byte, error) {
	prefetch := map[string]string{}
	for _, key := range pinnedPrefetchKeys {
		prefetch[key] = prefetchTemplate(key)
	}
	services := make([]map[string]any, 0, len(advertised))
	for _, s := range advertised {
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
