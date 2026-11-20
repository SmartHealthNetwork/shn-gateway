//go:build !shn_coreonly

package app

import (
	"slices"
	"strings"
	"testing"
)

// providerPASEnv is a provider gateway's minimal environment with
// PROVIDER_PAS_SUBSCRIPTION set to value.
func providerPASEnv(t *testing.T, value string) map[string]string {
	t.Helper()
	return map[string]string{
		"ROLE": "provider", "SHN_SECRETS": "/x", "SHN_DISCOVERY_URL": "https://d",
		"PROVIDER_DTR_POPULATE_URL": "https://populate.test/fhir/Questionnaire/$populate",
		"PROVIDER_DAVINCI_INGRESS":  "1", "PROVIDER_DAVINCI_INGRESS_BASE_URL": "https://provider-gw.test",
		"INGRESS_CLIENTS_FILE":      writeClientsFile(t, testValidClientsJSON(t)),
		"PROVIDER_PAS_SUBSCRIPTION": value,
	}
}

// A provider that sets PROVIDER_PAS_SUBSCRIPTION (1 or true) loads and
// declares the PAS topic's signal leg from that setting alone; unset, 0 or
// false, it declares none. Any other value refuses the start, naming the
// setting and the value: never "any value means on". A payer that sets it
// refuses to start, naming it.
func TestLoadConfig_ProviderPASSubscriptionTakesTheSignalLeg(t *testing.T) {
	for _, value := range []string{" 1 ", "   ", "yes", "on", "TRUE", "True", "2", "-1", "enabled"} {
		if _, err := loadEnv(providerPASEnv(t, value)); err == nil || !strings.Contains(err.Error(), "PROVIDER_PAS_SUBSCRIPTION must be 1 or true") || !strings.Contains(err.Error(), `"`+value+`"`) {
			t.Errorf("PROVIDER_PAS_SUBSCRIPTION %q: %v, want a refusal naming the setting and the value", value, err)
		}
	}
	for value, takes := range map[string]bool{"1": true, "true": true, "": false, "0": false, "false": false} {
		cfg, err := loadEnv(providerPASEnv(t, value))
		if err != nil {
			t.Fatalf("PROVIDER_PAS_SUBSCRIPTION %q: %v", value, err)
		}
		if cfg.ProviderPASSubscription != takes {
			t.Errorf("PROVIDER_PAS_SUBSCRIPTION %q: read %v, want %v", value, cfg.ProviderPASSubscription, takes)
		}
		if got := capabilityDeclaration(cfg).Legs; slices.Contains(got, "pas-subscription-signal") != takes {
			t.Errorf("PROVIDER_PAS_SUBSCRIPTION %q: declares %v, want pas-subscription-signal %v", value, got, takes)
		}
	}
	_, err := loadEnv(payerEnv(map[string]string{"PROVIDER_PAS_SUBSCRIPTION": "1"}))
	if err == nil || !strings.Contains(err.Error(), "PROVIDER_PAS_SUBSCRIPTION is provider-only") {
		t.Fatalf("a payer with the setting: %v, want a refusal naming it", err)
	}
	// Its documentation return path (E-22) is under the ingress base: the
	// setting without the ingress refuses to start, naming the setting's own
	// requirement; without the ingress's base alone, the ingress's own
	// refusal comes first (the setting's base clause, as checkProviderCDex's,
	// is reached by no configuration the ingress admits).
	for _, row := range []struct {
		drop []string
		want string
	}{
		{[]string{"PROVIDER_DAVINCI_INGRESS", "PROVIDER_DAVINCI_INGRESS_BASE_URL", "INGRESS_CLIENTS_FILE"}, "PROVIDER_PAS_SUBSCRIPTION set requires PROVIDER_DAVINCI_INGRESS and PROVIDER_DAVINCI_INGRESS_BASE_URL"},
		{[]string{"PROVIDER_DAVINCI_INGRESS_BASE_URL"}, "PROVIDER_DAVINCI_INGRESS requires PROVIDER_DAVINCI_INGRESS_BASE_URL"},
	} {
		e := providerPASEnv(t, "1")
		for _, k := range row.drop {
			delete(e, k)
		}
		if _, err := loadEnv(e); err == nil || !strings.Contains(err.Error(), row.want) {
			t.Errorf("without %v: %v, want a refusal naming %q", row.drop, err, row.want)
		}
	}
	// A payer that leaves the setting off (0 or false) starts: only a set
	// value is the provider's.
	for _, off := range []string{"0", "false"} {
		if _, err := loadEnv(payerEnv(map[string]string{"PROVIDER_PAS_SUBSCRIPTION": off})); err != nil {
			t.Errorf("a payer with PROVIDER_PAS_SUBSCRIPTION=%s: %v, want it to start", off, err)
		}
	}
}

// build hands engine.New the setting exactly when it is set.
func TestBuildWiresProviderPASSubscriptionToGateway(t *testing.T) {
	for raw, want := range map[string]bool{"": false, "0": false, "false": false, "1": true, "true": true} {
		b, _, err := buildProviderForPopulate(t, map[string]string{"PROVIDER_DTR_POPULATE_URL": "https://populate.test/fhir/Questionnaire/$populate", "PROVIDER_PAS_SUBSCRIPTION": raw,
			"PROVIDER_DAVINCI_INGRESS": "1", "PROVIDER_DAVINCI_INGRESS_BASE_URL": "https://provider-gw.test", "INGRESS_CLIENTS_FILE": writeClientsFile(t, testValidClientsJSON(t))})
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		if b.providerPASSubscription != want {
			t.Fatalf("PROVIDER_PAS_SUBSCRIPTION %q: build handed the engine %v, want %v", raw, b.providerPASSubscription, want)
		}
	}
}
