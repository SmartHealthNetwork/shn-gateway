package engine

import (
	"encoding/json"
	"net/url"
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

// TestPrefetchSearchMatchesTemplate pins that the search the gateway runs for
// an absent prefetch key is exactly the template it advertises, with the
// patient filled in.
func TestPrefetchSearchMatchesTemplate(t *testing.T) {
	for _, key := range pinnedPrefetchKeys {
		tmpl := prefetchTemplate(key)
		if key == prefetchPatientKey {
			if tmpl != "Patient/{{context.patientId}}" {
				t.Errorf("patient template %q", tmpl)
			}
			continue
		}
		q, err := SoRSearchQuery(prefetchSearchTypes[key], "pt-1")
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		decoded, err := url.QueryUnescape(q)
		if err != nil {
			t.Fatal(err)
		}
		if want := strings.ReplaceAll(tmpl, "{{context.patientId}}", "pt-1"); decoded != want {
			t.Errorf("%s: search %q, template %q", key, decoded, want)
		}
	}
	if len(prefetchSearchTypes) != len(pinnedPrefetchKeys)-1 {
		t.Fatalf("%d search keys for %d advertised keys", len(prefetchSearchTypes), len(pinnedPrefetchKeys))
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
			if svc.Prefetch[k] != prefetchTemplate(k) {
				t.Errorf("%s prefetch %s = %q", svc.ID, k, svc.Prefetch[k])
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
