package engine

import (
	"encoding/json"
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
