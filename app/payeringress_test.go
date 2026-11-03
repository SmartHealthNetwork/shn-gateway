//go:build !shn_coreonly

package app

import (
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine"
	"github.com/SmartHealthNetwork/shn-gateway/pa/cdexroute"
)

// The payer's Da Vinci ingress (the attachment-request Task) boots by the
// provider's all-or-nothing rule (FR-G13): payer-only, with a base URL every
// per-provider base can be built under, and at least one registered client.
func TestLoadConfig_PayerIngress(t *testing.T) {
	// A client listing no scopes, so it is registered for the Task scope.
	unlisted := strings.Replace(testValidClientsJSON(t), `,"scopes":["system/Davinci.write"]`, "", 1)
	clients := writeClientsFile(t, unlisted)
	davinciOnly := writeClientsFile(t, testValidClientsJSON(t))
	load := func(extra map[string]string) (config, error) {
		e := payerEnv(extra)
		return loadConfig(func(k string) string { return e[k] })
	}
	t.Run("valid", func(t *testing.T) {
		cfg, err := load(map[string]string{"PAYER_DAVINCI_INGRESS": "1", "PAYER_DAVINCI_INGRESS_BASE_URL": "https://payer-gw.test", "INGRESS_CLIENTS_FILE": clients})
		if err != nil {
			t.Fatalf("valid payer ingress: %v", err)
		}
		if !cfg.PayerDavinciIngress || cfg.IngressBaseURL != "https://payer-gw.test" || len(cfg.IngressClients) != 1 {
			t.Fatalf("config %+v", cfg)
		}
	})
	for name, row := range map[string]struct {
		env  map[string]string
		want string
	}{
		"no base":    {map[string]string{"PAYER_DAVINCI_INGRESS": "1", "INGRESS_CLIENTS_FILE": clients}, "requires PAYER_DAVINCI_INGRESS_BASE_URL"},
		"no clients": {map[string]string{"PAYER_DAVINCI_INGRESS": "1", "PAYER_DAVINCI_INGRESS_BASE_URL": "https://payer-gw.test"}, "INGRESS_CLIENTS_FILE"},
		"an empty client list": {map[string]string{"PAYER_DAVINCI_INGRESS": "1", "PAYER_DAVINCI_INGRESS_BASE_URL": "https://payer-gw.test",
			"INGRESS_CLIENTS_FILE": writeClientsFile(t, "[]")}, "INGRESS_CLIENTS_FILE"},
		// Every payer route takes only the Task scope: a client that can
		// never be granted it is a registration error.
		"a client without the Task scope": {map[string]string{"PAYER_DAVINCI_INGRESS": "1", "PAYER_DAVINCI_INGRESS_BASE_URL": "https://payer-gw.test",
			"INGRESS_CLIENTS_FILE": davinciOnly}, "lists no system/Task.write"},
		"a bad client": {map[string]string{"PAYER_DAVINCI_INGRESS": "1", "PAYER_DAVINCI_INGRESS_BASE_URL": "https://payer-gw.test",
			"INGRESS_CLIENTS_FILE": writeClientsFile(t, `[{"client_id":"x","alg":"HS256","public_key_pem":"irrelevant"}]`)}, "ingress clients"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := load(row.env); err == nil || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("want a boot refusal naming %q, got %v", row.want, err)
			}
		})
	}
	for _, base := range []string{"https://", "https://gw.test/fhir?tenant=1", "https://gw.test/fhir#x", "https://gw.test/fhir?", "https://u:p@gw.test"} {
		t.Run("refused "+base, func(t *testing.T) {
			if _, err := cdexroute.ProviderBase(base, "provider"); err == nil {
				t.Fatalf("fixture: %q builds a provider's base", base)
			}
			_, err := load(map[string]string{"PAYER_DAVINCI_INGRESS": "1", "PAYER_DAVINCI_INGRESS_BASE_URL": base, "INGRESS_CLIENTS_FILE": clients})
			if err == nil || !strings.Contains(err.Error(), "PAYER_DAVINCI_INGRESS_BASE_URL") {
				t.Fatalf("base %q: want a boot refusal naming the setting, got %v", base, err)
			}
		})
	}
	t.Run("payer-only", func(t *testing.T) {
		e := baseProviderEnv()
		delete(e, "PROVIDER_DAVINCI_INGRESS")
		e["PAYER_DAVINCI_INGRESS"] = "1"
		e["PAYER_DAVINCI_INGRESS_BASE_URL"] = "https://gw.test"
		e["INGRESS_CLIENTS_FILE"] = clients
		if _, err := loadConfig(func(k string) string { return e[k] }); err == nil || !strings.Contains(err.Error(), "PAYER_DAVINCI_INGRESS is payer-only") {
			t.Fatalf("payer ingress on a provider: want refused, got %v", err)
		}
	})
	t.Run("off by default", func(t *testing.T) {
		cfg, err := load(nil)
		if err != nil || cfg.PayerDavinciIngress || cfg.IngressBaseURL != "" || cfg.IngressClients != nil {
			t.Fatalf("config %+v, %v; want no ingress", cfg, err)
		}
	})
}

// A client registered with no scopes takes the one scope its ingress's
// routes check: the attachment-request Task routes' on a payer, the Da Vinci
// routes' on a provider. Listed scopes are kept as written.
func TestLoadIngressClientsScoped(t *testing.T) {
	listed := testValidClientsJSON(t)
	unlisted := strings.Replace(listed, `,"scopes":["system/Davinci.write"]`, "", 1)
	if unlisted == listed {
		t.Fatal("fixture: scopes not removed")
	}
	for name, row := range map[string]struct {
		body, def, want string
	}{
		"unlisted on a payer":    {unlisted, engine.TaskWriteScope, engine.TaskWriteScope},
		"unlisted on a provider": {unlisted, "system/Davinci.write", "system/Davinci.write"},
		"listed on a payer":      {listed, engine.TaskWriteScope, "system/Davinci.write"},
	} {
		clients, err := loadIngressClientsScoped(writeClientsFile(t, row.body), row.def)
		if err != nil || len(clients) != 1 {
			t.Fatalf("%s: %v %v", name, clients, err)
		}
		for _, c := range clients {
			if len(c.Scopes) != 1 || c.Scopes[0] != row.want {
				t.Errorf("%s: scopes %v, want [%s]", name, c.Scopes, row.want)
			}
		}
	}
}
