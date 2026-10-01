package engine

import (
	"context"
	"encoding/json"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// The pinned PA prefetch key set. The advertised discovery MUST cover exactly these, so
// br-provider inlines the set br-payer needs.
func TestCDSDiscovery_AdvertisesPinnedPrefetchKeys(t *testing.T) {
	body, err := cdsDiscoveryJSON()
	if err != nil {
		t.Fatalf("cdsDiscoveryJSON: %v", err)
	}
	var doc struct {
		Services []struct {
			ID       string            `json:"id"`
			Hook     string            `json:"hook"`
			Prefetch map[string]string `json:"prefetch"`
		} `json:"services"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("discovery not valid JSON: %v", err)
	}
	if len(doc.Services) == 0 {
		t.Fatal("discovery advertises no services")
	}
	want := []string{"patient", "coverage", "serviceHistory", "deviceHistory", "medicationHistory", "questionnaireResponses"}
	svc := doc.Services[0]
	for _, k := range want {
		if _, ok := svc.Prefetch[k]; !ok {
			t.Errorf("discovery service %q missing pinned prefetch key %q", svc.ID, k)
		}
	}
}

// TestPinnedPrefetchKeysMatchDiscovery pins pinnedPrefetchKeys (the set the SoR-resolve path
// iterates) to EXACTLY the keys the discovery advertises. Without this the two could silently
// diverge — the gateway would resolve a key it never advertised (or advertise one it can't
// resolve), breaking self-containment at the payer, not at the ingress.
func TestPinnedPrefetchKeysMatchDiscovery(t *testing.T) {
	body, err := cdsDiscoveryJSON()
	if err != nil {
		t.Fatalf("cdsDiscoveryJSON: %v", err)
	}
	var doc struct {
		Services []struct {
			Prefetch map[string]string `json:"prefetch"`
		} `json:"services"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || len(doc.Services) == 0 {
		t.Fatalf("decode discovery: %v", err)
	}
	advertised := doc.Services[0].Prefetch
	if len(advertised) != len(pinnedPrefetchKeys) {
		t.Fatalf("advertised %d prefetch keys, pinnedPrefetchKeys has %d", len(advertised), len(pinnedPrefetchKeys))
	}
	for _, k := range pinnedPrefetchKeys {
		if _, ok := advertised[k]; !ok {
			t.Errorf("pinnedPrefetchKeys has %q but discovery does not advertise it", k)
		}
	}
}

// advertisedTemplates are the prefetch templates every advertised service carries: the
// Da Vinci reference payer's order-sign templates (deploy/two-ri), which follow CDS Hooks
// 2.0's prefetch query restrictions (a bare patient id, token narrowing, no _include).
var advertisedTemplates = map[string]string{
	"patient":                "Patient/{{context.patientId}}",
	"coverage":               "Coverage?patient={{context.patientId}}&status=active",
	"serviceHistory":         "ServiceRequest?patient={{context.patientId}}&status=active,completed",
	"deviceHistory":          "DeviceRequest?patient={{context.patientId}}&status=active,on-hold,completed",
	"medicationHistory":      "MedicationRequest?patient={{context.patientId}}&status=active,completed",
	"questionnaireResponses": "QuestionnaireResponse?patient={{context.patientId}}&status=completed",
}

// prefetchSoRQueries are the searches the gateway runs of its participant's system for the
// same values (patient pt-1), exactly as a connector sends them: the template's narrowing,
// the patient by its typed reference, and the includes the payer needs because it cannot
// fetch what a carried record references once fhirServer is removed.
var prefetchSoRQueries = map[string]string{
	"coverage":               "Coverage?patient=Patient%2Fpt-1&status=active&_include=Coverage%3Apayor",
	"serviceHistory":         "ServiceRequest?patient=Patient%2Fpt-1&status=active,completed",
	"deviceHistory":          "DeviceRequest?patient=Patient%2Fpt-1&status=active,on-hold,completed&_include=DeviceRequest%3Aperformer",
	"medicationHistory":      "MedicationRequest?patient=Patient%2Fpt-1&status=active,completed",
	"questionnaireResponses": "QuestionnaireResponse?patient=Patient%2Fpt-1&status=completed",
}

// TestPrefetchSearchMatchesTemplate pins, for every advertised key, the template and the
// search the gateway runs for that key when it obtains the value: the same resource type
// and the same filters (read from one table), the search alone carrying an include.
func TestPrefetchSearchMatchesTemplate(t *testing.T) {
	if len(advertisedTemplates) != len(pinnedPrefetchKeys) || len(prefetchSearchTypes) != len(pinnedPrefetchKeys)-1 {
		t.Fatalf("%d templates, %d search keys for %d advertised keys", len(advertisedTemplates), len(prefetchSearchTypes), len(pinnedPrefetchKeys))
	}
	for _, key := range pinnedPrefetchKeys {
		tmpl := prefetchTemplate(key)
		if tmpl != advertisedTemplates[key] {
			t.Errorf("%s: template %q, want %q", key, tmpl, advertisedTemplates[key])
		}
		if strings.Contains(tmpl, "_include") || strings.Contains(tmpl, "=Patient/") {
			t.Errorf("%s: template %q asks an EHR for an include or a typed patient reference", key, tmpl)
		}
		if key == prefetchPatientKey {
			continue
		}
		q := prefetchSearchQuery(key, "pt-1")
		if q != prefetchSoRQueries[key] {
			t.Errorf("%s: search %q, want %q", key, q, prefetchSoRQueries[key])
		}
		// The search the obtain path runs is this one.
		if s := searchSystemOfRecord(context.Background(), searchingSoR{}, prefetchSearchTypes[key], "pt-1"); s.Query != q {
			t.Errorf("%s: obtain path searched %q, want %q", key, s.Query, q)
		}
		tType, tQuery, _ := strings.Cut(tmpl, "?")
		sType, sQuery, _ := strings.Cut(q, "?")
		if tType != sType || tType != prefetchSearchTypes[key] {
			t.Errorf("%s: template searches %s, the gateway %s", key, tType, sType)
		}
		tp, err := url.ParseQuery(tQuery)
		if err != nil {
			t.Fatal(err)
		}
		sp, err := url.ParseQuery(sQuery)
		if err != nil {
			t.Fatal(err)
		}
		if tp.Get("patient") != "{{context.patientId}}" || sp.Get("patient") != "Patient/pt-1" {
			t.Errorf("%s: patient %q / %q", key, tp.Get("patient"), sp.Get("patient"))
		}
		inc := sp["_include"]
		want, hasInclude := searchIncludes[tType]
		if hasInclude != (len(inc) == 1) || (hasInclude && inc[0] != want) || len(inc) > 1 {
			t.Errorf("%s: search includes %v, want %q", key, inc, want)
		}
		delete(tp, "patient")
		delete(sp, "patient")
		delete(sp, "_include")
		if len(tp) == 0 || !reflect.DeepEqual(tp, sp) {
			t.Errorf("%s: template filters %v, search filters %v", key, tp, sp)
		}
	}
}

func TestCDSDiscovery_MatchesDispatch(t *testing.T) {
	body, err := cdsDiscoveryJSON()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Services []struct {
			ID          string            `json:"id"`
			Hook        string            `json:"hook"`
			Title       string            `json:"title"`
			Description string            `json:"description"`
			Prefetch    map[string]string `json:"prefetch"`
		} `json:"services"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	want := [][2]string{{"shn-order-sign", "order-sign"}, {"shn-order-select", "order-select"}, {"shn-order-dispatch", "order-dispatch"}}
	if len(doc.Services) != len(want) {
		t.Fatalf("services %+v", doc.Services)
	}
	for i, svc := range doc.Services {
		if svc.ID != want[i][0] || svc.Hook != want[i][1] {
			t.Errorf("service %d is %s/%s, want %s/%s", i, svc.ID, svc.Hook, want[i][0], want[i][1])
		}
		// What is advertised is what the route dispatches.
		got, ok := cdsIngressServiceByID(svc.ID)
		if !ok || got.Hook != svc.Hook || got.Leg != legForHook(svc.Hook) {
			t.Errorf("%s dispatches as %+v", svc.ID, got)
		}
		if svc.Title == "" || svc.Description == "" {
			t.Errorf("%s has no title or description", svc.ID)
		}
		for _, word := range []string{"substrate", "SHN", "shn"} {
			if strings.Contains(svc.Title+" "+svc.Description, word) {
				t.Errorf("%s copy uses %q: %q / %q", svc.ID, word, svc.Title, svc.Description)
			}
		}
		if len(svc.Prefetch) != len(pinnedPrefetchKeys) {
			t.Errorf("%s advertises %v", svc.ID, svc.Prefetch)
		}
		for _, k := range pinnedPrefetchKeys {
			if svc.Prefetch[k] != advertisedTemplates[k] {
				t.Errorf("%s prefetch %s = %q, want %q", svc.ID, k, svc.Prefetch[k], advertisedTemplates[k])
			}
		}
		if strings.Contains(string(body), "davinci-crd.version") {
			t.Error("discovery advertises a CRD version")
		}
	}
	for _, hook := range []string{"appointment-book", "encounter-start"} {
		for _, svc := range doc.Services {
			if svc.Hook == hook {
				t.Errorf("%s is advertised", hook)
			}
		}
	}
	if _, ok := cdsIngressServiceByID("order-select-crd"); ok {
		t.Error("an unadvertised service id is dispatched")
	}
}
