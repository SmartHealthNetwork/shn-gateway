package app

import (
	"encoding/json"
	"strings"
	"testing"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
)

// moduleClientsJSON is testValidClientsJSON's one client with extra set on
// its entry.
func moduleClientsJSON(t *testing.T, extra map[string]any) string {
	t.Helper()
	var arr []map[string]any
	if err := json.Unmarshal([]byte(testValidClientsJSON(t)), &arr); err != nil || len(arr) != 1 {
		t.Fatalf("fixture: %v", err)
	}
	for k, v := range extra {
		arr[0][k] = v
	}
	b, err := json.Marshal(arr)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// An INGRESS_CLIENTS_FILE entry marks a registered module ingress client by
// "module"; an unmarked entry is the participant's own native system.
func TestLoadIngressClients_ModuleMarksAClient(t *testing.T) {
	clients, err := loadIngressClients(writeClientsFile(t, moduleClientsJSON(t, map[string]any{"module": engine.ModuleOnBehalf})))
	if err != nil {
		t.Fatal(err)
	}
	for _, reg := range clients {
		if reg.Module != engine.ModuleOnBehalf {
			t.Fatalf("module %q, want %s", reg.Module, engine.ModuleOnBehalf)
		}
	}
	native, err := loadIngressClients(writeClientsFile(t, testValidClientsJSON(t)))
	if err != nil {
		t.Fatal(err)
	}
	for _, reg := range native {
		if reg.Module != "" {
			t.Fatalf("an unmarked client registered as module %q", reg.Module)
		}
	}
}

// REJECTION: the builders a module client may declare are its module's, set by
// the build, so an entry that lists builders refuses boot.
func TestLoadIngressClients_BuildersAreNotConfigurable(t *testing.T) {
	for name, extra := range map[string]map[string]any{
		"builders on a module client": {"module": engine.ModuleOnBehalf, "builders": []string{"sdk-pas-submit"}},
		"builders on a native client": {"builders": []string{}},
	} {
		_, err := loadIngressClients(writeClientsFile(t, moduleClientsJSON(t, extra)))
		if err == nil || !strings.Contains(err.Error(), "builders is not configurable") {
			t.Errorf("%s: err = %v, want the builders refusal", name, err)
		}
	}
}

// REJECTION: a module this build does not compile in refuses boot, through
// the same configuration a deployment uses; a module it does boots.
func TestBuild_ModuleClientAtBoot(t *testing.T) {
	for name, module := range map[string]string{"unknown module": "made-up-module", "on-behalf": engine.ModuleOnBehalf} {
		t.Run(name, func(t *testing.T) {
			extra := map[string]string{
				"PROVIDER_DTR_POPULATE_URL":         "https://populate.test/fhir/Questionnaire/$populate",
				"PROVIDER_DAVINCI_INGRESS":          "1",
				"PROVIDER_DAVINCI_INGRESS_BASE_URL": "https://gw.test",
				"INGRESS_CLIENTS_FILE":              writeClientsFile(t, moduleClientsJSON(t, map[string]any{"module": module})),
			}
			_, _, err := buildProviderForPopulate(t, extra)
			if module == engine.ModuleOnBehalf {
				if err != nil {
					t.Fatalf("build: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), `unknown module "made-up-module"`) {
				t.Fatalf("err = %v, want the unknown-module refusal", err)
			}
		})
	}
}
