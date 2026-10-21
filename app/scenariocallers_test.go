package app

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/scenarioauth"
)

// A provider gateway reads the callers its /scenario routes admit from
// SCENARIO_CALLERS_FILE: each label with its public key. An empty
// list admits nothing; a malformed file, an empty or repeated label, or a key
// that is not an Ed25519 public key refuses the boot, never admits less or
// more than the file says.
func TestLoadConfig_ScenarioCallersFile(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString(pub)
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	load := func(role, path string) (config, error) {
		env := map[string]string{
			"ROLE":                      role,
			"SHN_SECRETS":               "/etc/shn/bundles/" + role,
			"SHN_DISCOVERY_URL":         "http://accounts:8088/discovery",
			"PROVIDER_DTR_POPULATE_URL": "https://populate.test/fhir/Questionnaire/$populate",
			"SCENARIO_CALLERS_FILE":     path,
		}
		return loadConfig(func(k string) string { return env[k] })
	}

	cfg, err := load("provider", write("good.json", `[{"label":"monitor","public_key":"`+key+`"}]`))
	if err != nil {
		t.Fatalf("a good file: %v", err)
	}
	if len(cfg.ScenarioCallers) != 1 || !cfg.ScenarioCallers["monitor"].Equal(pub) {
		t.Fatalf("callers = %v, want exactly monitor with its key", cfg.ScenarioCallers)
	}

	cfg, err = load("provider", write("empty.json", `[]`))
	if err != nil {
		t.Fatalf("an empty list: %v", err)
	}
	if cfg.ScenarioCallers == nil || len(cfg.ScenarioCallers) != 0 {
		t.Fatalf("an empty list loaded as %#v, want an empty, non-nil set (admits nothing)", cfg.ScenarioCallers)
	}

	cfg, err = load("provider", "")
	if err != nil {
		t.Fatalf("unset: %v", err)
	}
	if cfg.ScenarioCallers != nil {
		t.Fatalf("unset loaded as %#v, want nil", cfg.ScenarioCallers)
	}

	for name, body := range map[string]string{
		"malformed":   `{"label":"monitor"}`,
		"empty-label": `[{"label":"","public_key":"` + key + `"}]`,
		"duplicate":   `[{"label":"monitor","public_key":"` + key + `"},{"label":"monitor","public_key":"` + key + `"}]`,
		"bad-base64":  `[{"label":"monitor","public_key":"%%%"}]`,
		"short-key":   `[{"label":"monitor","public_key":"` + base64.StdEncoding.EncodeToString(pub[:16]) + `"}]`,
		"no-key":      `[{"label":"monitor"}]`,
	} {
		if _, err := load("provider", write(name+".json", body)); err == nil {
			t.Errorf("%s: the boot was not refused", name)
		}
	}
	if _, err := load("provider", filepath.Join(dir, "missing.json")); err == nil {
		t.Error("a missing file: the boot was not refused")
	}
	// No other role serves /scenario: naming callers there is a mistake.
	if _, err := load("payer", write("payer.json", `[]`)); err == nil || !strings.Contains(err.Error(), "provider-only") {
		t.Errorf("a payer with SCENARIO_CALLERS_FILE: err = %v, want refused as provider-only", err)
	}
}

// The boot line says whom a provider gateway's /scenario routes admit, or
// warns that they do not authenticate their caller.
func TestScenarioCallersLine(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	if got := scenarioCallersLine(nil); !strings.Contains(got, "WARNING") || !strings.Contains(got, "do not authenticate") {
		t.Errorf("unset: %q, want a warning", got)
	}
	if got := scenarioCallersLine(map[string]ed25519.PublicKey{}); !strings.Contains(got, "no caller") {
		t.Errorf("empty: %q", got)
	}
	got := scenarioCallersLine(map[string]ed25519.PublicKey{"phg": pub, "monitor": pub})
	if !strings.HasSuffix(got, "monitor, phg") {
		t.Errorf("two callers: %q, want them listed in order", got)
	}
}

// A provider gateway built from SCENARIO_CALLERS_FILE refuses, at its own
// /scenario routes, a request that is not its named caller's, or is its
// caller's for another gateway, and serves the caller's for its own holder id;
// its boot line says whom it admits. Built without the file it
// serves the routes unauthenticated, as before, and its boot line warns.
func TestBuild_ScenarioCallersReachTheRoutes(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "scenario-callers.json")
	if err := os.WriteFile(path, []byte(`[{"label":"monitor","public_key":"`+base64.StdEncoding.EncodeToString(pub)+`"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	// gateway is the holder id the request is signed for; "" sends it unsigned.
	reset := func(h http.Handler, gateway string) int {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/scenario/reset", nil)
		if gateway != "" {
			if err := scenarioauth.Sign(r, "monitor", gateway, priv, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	// Swaps the process-global logger: gateway/app has no t.Parallel() test.
	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(prev)

	b, _, err := buildProviderForPopulate(t, map[string]string{
		"PROVIDER_DTR_POPULATE_URL": "https://populate.test/fhir/Questionnaire/$populate",
		"SCENARIO_CALLERS_FILE":     path,
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := reset(b.handler, ""); got != http.StatusUnauthorized {
		t.Errorf("an unsigned reset: %d, want 401", got)
	}
	// The gateway's holder id is its bundle's (buildProviderForPopulate's
	// h-test), not its role.
	if got := reset(b.handler, "provider"); got != http.StatusUnauthorized {
		t.Errorf("the named caller's reset for another gateway: %d, want 401", got)
	}
	if got := reset(b.handler, "h-test"); got != http.StatusOK {
		t.Errorf("the named caller's reset for this gateway: %d, want 200", got)
	}
	if !strings.Contains(logs.String(), "gateway: /scenario admits only its callers: monitor\n") {
		t.Errorf("boot log does not name the admitted caller:\n%s", logs.String())
	}

	logs.Reset()
	b, _, err = buildProviderForPopulate(t, map[string]string{
		"PROVIDER_DTR_POPULATE_URL": "https://populate.test/fhir/Questionnaire/$populate",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := reset(b.handler, ""); got != http.StatusOK {
		t.Errorf("without the file, an unsigned reset: %d, want 200 (unchanged)", got)
	}
	if !strings.Contains(logs.String(), "gateway: WARNING: SCENARIO_CALLERS_FILE unset") {
		t.Errorf("boot log does not warn that the routes are unauthenticated:\n%s", logs.String())
	}
}
