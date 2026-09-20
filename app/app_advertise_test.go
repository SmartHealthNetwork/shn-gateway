package app

import (
	"slices"
	"strings"
	"testing"
)

// TestLoadConfigAdvertisedCDSHooks: unset advertises everything (nil), a
// list narrows in the order given, and a hook the network does not carry
// refuses to boot naming the hooks it does.
func TestLoadConfigAdvertisedCDSHooks(t *testing.T) {
	base := map[string]string{
		"ROLE":                      "provider",
		"SHN_SECRETS":               "/etc/shn/bundles/provider",
		"SHN_DISCOVERY_URL":         "http://accounts:8088/discovery",
		"PROVIDER_DTR_POPULATE_URL": "https://populate.test/fhir/Questionnaire/$populate",
	}
	load := func(t *testing.T, raw string) (config, error) {
		t.Helper()
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		if raw != "" {
			m["CDS_ADVERTISE_HOOKS"] = raw
		}
		return loadConfig(env(m))
	}
	cfg, err := load(t, "")
	if err != nil || cfg.AdvertisedCDSHooks != nil {
		t.Fatalf("unset: hooks=%q err=%v, want nil (every hook)", cfg.AdvertisedCDSHooks, err)
	}
	cfg, err = load(t, "order-sign, order-select")
	if err != nil || !slices.Equal(cfg.AdvertisedCDSHooks, []string{"order-sign", "order-select"}) {
		t.Fatalf("narrowed: hooks=%q err=%v", cfg.AdvertisedCDSHooks, err)
	}
	if _, err := load(t, "order-sign,order-dispach"); err == nil || !strings.Contains(err.Error(), `"order-dispach"`) || !strings.Contains(err.Error(), "order-dispatch") {
		t.Fatalf("a hook the network does not carry must refuse to boot naming the carried hooks, got %v", err)
	}
}
