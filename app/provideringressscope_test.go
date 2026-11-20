//go:build !shn_coreonly

package app

import (
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine"
)

// TestLoadConfig_ProviderIngressScopes: a provider's ingress client that
// lists no scopes is registered for the umbrella, engine.ProviderUmbrellaScope,
// which permits every provider route: the explicit default, never an empty
// registration. A client registering a scope on one of the provider routes'
// resource types, the umbrella or a wildcard that SMART does not spell refuses
// the start, naming the scope; the DTR IG's own scopes, and scopes on other
// resources, start.
func TestLoadConfig_ProviderIngressScopes(t *testing.T) {
	load := func(clientsJSON string) (config, error) {
		e := baseProviderEnv()
		e["PROVIDER_DAVINCI_INGRESS_BASE_URL"] = "https://gw.test"
		e["INGRESS_CLIENTS_FILE"] = writeClientsFile(t, clientsJSON)
		return loadConfig(func(k string) string { return e[k] })
	}
	listed := testValidClientsJSON(t)
	scoped := func(scopes string) string {
		return strings.Replace(listed, `"scopes":["system/Davinci.write"]`, `"scopes":`+scopes, 1)
	}
	t.Run("a client listing no scopes is registered for the umbrella", func(t *testing.T) {
		unlisted := strings.Replace(listed, `,"scopes":["system/Davinci.write"]`, "", 1)
		if unlisted == listed {
			t.Fatal("fixture: scopes not removed")
		}
		cfg, err := load(unlisted)
		if err != nil || len(cfg.IngressClients) != 1 {
			t.Fatalf("unlisted: %v %v", cfg.IngressClients, err)
		}
		for _, c := range cfg.IngressClients {
			if len(c.Scopes) != 1 || c.Scopes[0] != engine.ProviderUmbrellaScope {
				t.Fatalf("registered %v, want [%s]", c.Scopes, engine.ProviderUmbrellaScope)
			}
			if !engine.ProviderScopesReachARoute(c.Scopes) {
				t.Fatal("the umbrella reaches no route")
			}
		}
	})
	for _, scopes := range []string{
		`["system/Claim.c","system/Claim.r"]`, `["system/*.rs","system/*.write"]`,
		`["system/questionnaire.rs","system/valueset.rs","system/library.rs"]`,
		`["system/Patient.read"]`, `["system/Subscription.*"]`,
	} {
		t.Run("boots with "+scopes, func(t *testing.T) {
			if _, err := load(scoped(scopes)); err != nil {
				t.Fatalf("%s: %v", scopes, err)
			}
		})
	}
	for _, bad := range []string{"system/claim.c", "system/Claim.wrte", "system/davinci.write", "system/*.x", "system/questionnaire.r", "system/subscription.*"} {
		t.Run("refused "+bad, func(t *testing.T) {
			_, err := load(scoped(`["system/Davinci.write","` + bad + `"]`))
			if err == nil || !strings.Contains(err.Error(), `"`+bad+`"`) {
				t.Fatalf("%s: want a boot refusal naming the scope, got %v", bad, err)
			}
		})
	}
}
