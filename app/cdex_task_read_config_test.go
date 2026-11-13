//go:build !shn_coreonly

package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
)

// cdexReadEnv is cdexTaskEnv with the read scope set.
func cdexReadEnv(t *testing.T) map[string]string {
	t.Helper()
	e := cdexTaskEnv(t)
	e["PROVIDER_CDEX_READ_SCOPE"] = "system/Task.r"
	return e
}

// A provider that sets the read scope beside its Task endpoint loads, the
// scope read trimmed, and declares the payer's read of its Task beside the
// attachment request; without the scope, or with a blank one, it declares the
// attachment request alone.
func TestLoadConfig_ProviderCDexReadScopeTakesTheRead(t *testing.T) {
	e := cdexReadEnv(t)
	e["PROVIDER_CDEX_READ_SCOPE"] = "  system/Task.r  "
	cfg, err := loadEnv(e)
	if err != nil {
		t.Fatalf("a read scope with a Task endpoint should load: %v", err)
	}
	if cfg.ProviderCDexReadScope != "system/Task.r" {
		t.Fatalf("read scope %q, want it trimmed", cfg.ProviderCDexReadScope)
	}
	if got := capabilityDeclaration(cfg).Legs; !slices.Contains(got, "cdex-task-status") || !slices.Contains(got, "cdex-attachment-request") {
		t.Fatalf("declares %v, want the read beside the attachment request", got)
	}
	for _, scope := range []string{"", "   "} {
		e := cdexTaskEnv(t)
		e["PROVIDER_CDEX_READ_SCOPE"] = scope
		cfg, err := loadEnv(e)
		if err != nil {
			t.Fatalf("read scope %q: %v", scope, err)
		}
		if got := capabilityDeclaration(cfg).Legs; slices.Contains(got, "cdex-task-status") || !slices.Contains(got, "cdex-attachment-request") {
			t.Errorf("read scope %q: declares %v, want the attachment request alone", scope, got)
		}
	}
	// Without the read scope, the attachment request takes a Task endpoint
	// of any shape, as before.
	for _, taskURL := range []string{"https://provider.test/fhir/Task?x=1", "https://provider.test/fhir/Task/", "https://provider.test/submit"} {
		e := cdexTaskEnv(t)
		e["PROVIDER_CDEX_TASK_URL"] = taskURL
		if _, err := loadEnv(e); err != nil {
			t.Errorf("PROVIDER_CDEX_TASK_URL=%s without a read scope: %v", taskURL, err)
		}
	}
}

// The read scope's boot refusals, each naming the setting and the fix: on
// another role, without the Task endpoint, and with a Task endpoint no read
// can be made below.
func TestLoadConfig_ProviderCDexReadScopeRefusals(t *testing.T) {
	const notTask = "gateway: PROVIDER_CDEX_READ_SCOPE set requires PROVIDER_CDEX_TASK_URL to end in a Task segment, with no query or fragment (%q): set it to the provider's Task endpoint, e.g. https://ehr.example/fhir/Task"
	for name, c := range map[string]struct {
		edit func(e map[string]string)
		want string
	}{
		"on a payer": {func(e map[string]string) {
			for k := range e {
				if strings.HasPrefix(k, "PROVIDER_") || k == "INGRESS_CLIENTS_FILE" {
					delete(e, k)
				}
			}
			e["ROLE"] = "payer"
			e["PROVIDER_CDEX_READ_SCOPE"] = "system/Task.r"
		}, `gateway: PROVIDER_CDEX_READ_SCOPE is provider-only (role="payer"): only a provider takes the payer's attachment request and its read of that Task`},
		"without the Task endpoint": {func(e map[string]string) {
			for k := range e {
				if strings.HasPrefix(k, "PROVIDER_CDEX_") && k != "PROVIDER_CDEX_READ_SCOPE" {
					delete(e, k)
				}
			}
		}, "gateway: PROVIDER_CDEX_READ_SCOPE set requires PROVIDER_CDEX_TASK_URL, the provider's own Task endpoint"},
		"a query": {func(e map[string]string) {
			e["PROVIDER_CDEX_TASK_URL"] = "https://provider.test/fhir/Task?_format=json"
		}, fmt.Sprintf(notTask, "https://provider.test/fhir/Task?_format=json")},
		"a fragment":             {func(e map[string]string) { e["PROVIDER_CDEX_TASK_URL"] = "https://provider.test/fhir/Task#x" }, fmt.Sprintf(notTask, "https://provider.test/fhir/Task#x")},
		"no Task segment":        {func(e map[string]string) { e["PROVIDER_CDEX_TASK_URL"] = "https://provider.test/fhir/submit" }, fmt.Sprintf(notTask, "https://provider.test/fhir/submit")},
		"a trailing slash":       {func(e map[string]string) { e["PROVIDER_CDEX_TASK_URL"] = "https://provider.test/fhir/Task/" }, fmt.Sprintf(notTask, "https://provider.test/fhir/Task/")},
		"Task in another case":   {func(e map[string]string) { e["PROVIDER_CDEX_TASK_URL"] = "https://provider.test/fhir/TASK" }, fmt.Sprintf(notTask, "https://provider.test/fhir/TASK")},
		"a dot segment":          {func(e map[string]string) { e["PROVIDER_CDEX_TASK_URL"] = "https://provider.test/x/../Task" }, `gateway: PROVIDER_CDEX_READ_SCOPE set requires PROVIDER_CDEX_TASK_URL with no "." or ".." path segment ("https://provider.test/x/../Task"): remove the dot segments`},
		"an escaped dot segment": {func(e map[string]string) { e["PROVIDER_CDEX_TASK_URL"] = "https://provider.test/%2e/Task" }, `PROVIDER_CDEX_TASK_URL with no "." or ".." path segment`},
		"no host": {func(e map[string]string) { e["PROVIDER_CDEX_TASK_URL"] = "https:///fhir/Task" },
			`gateway: PROVIDER_CDEX_READ_SCOPE set requires PROVIDER_CDEX_TASK_URL to be an absolute http or https URL with a host ("https:///fhir/Task"): set it to the provider's Task endpoint, e.g. https://ehr.example/fhir/Task`},
	} {
		t.Run(name, func(t *testing.T) {
			e := cdexReadEnv(t)
			c.edit(e)
			if _, err := loadEnv(e); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("loadConfig: %v, want an error containing %q", err, c.want)
			}
		})
	}
}

// The read's client asks the Task forward's token endpoint, with the same
// client, for the read scope; the forward's client keeps asking for its own.
// Each holds its own token.
func TestProviderCDexReadClientAsksForTheReadScope(t *testing.T) {
	var mu sync.Mutex
	scopes := map[string]string{} // access token -> the scope it was asked for
	n := 0
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		n++
		tok := fmt.Sprintf("AT-%d", n)
		scopes[tok] = r.PostForm.Get("scope")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":%q,"token_type":"bearer","expires_in":3600}`, tok)
	}))
	defer tokenSrv.Close()
	var bearers []string
	sys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		bearers = append(bearers, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		mu.Unlock()
	}))
	defer sys.Close()
	e := cdexReadEnv(t)
	delete(e, "PROVIDER_CDEX_CLIENT_KEY")
	delete(e, "PROVIDER_CDEX_CLIENT_ALG")
	e["PROVIDER_CDEX_CLIENT_SECRET"] = "s"
	e["PROVIDER_CDEX_TOKEN_URL"] = tokenSrv.URL
	cfg, err := loadEnv(e)
	if err != nil {
		t.Fatal(err)
	}
	cfg.certs = endpointcert.New()
	read, err := providerCDexReadHTTPClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	forward, err := providerCDexHTTPClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []*http.Client{read, forward} {
		resp, err := c.Get(sys.URL + "/fhir/Task/t-1")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bearers) != 2 || scopes[bearers[0]] != "system/Task.r" || scopes[bearers[1]] != "system/Task.c" {
		t.Fatalf("bearers %v over scopes %v: want the read's asked for system/Task.r, the forward's for system/Task.c", bearers, scopes)
	}
}

// build hands engine.New the read scope and the client the Task is read with
// exactly when the read scope is set: deleting either line leaves a row red.
func TestBuildWiresTheReadScopeToGateway(t *testing.T) {
	for name, scope := range map[string]string{"set": "system/Task.r", "unset": ""} {
		t.Run(name, func(t *testing.T) {
			e := cdexReadEnv(t)
			delete(e, "PROVIDER_CDEX_CLIENT_KEY")
			delete(e, "PROVIDER_CDEX_CLIENT_ALG")
			e["PROVIDER_CDEX_CLIENT_SECRET"] = "s"
			e["PROVIDER_CDEX_READ_SCOPE"] = scope
			extra := map[string]string{"PROVIDER_DTR_POPULATE_URL": "https://populate.test/fhir/Questionnaire/$populate"}
			for k, v := range e {
				if strings.HasPrefix(k, "PROVIDER_CDEX_") || strings.HasPrefix(k, "PROVIDER_DAVINCI_INGRESS") || k == "INGRESS_CLIENTS_FILE" {
					extra[k] = v
				}
			}
			b, _, err := buildProviderForPopulate(t, extra)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if b.cdexReadScope != scope || b.cdexReadClient != (scope != "") {
				t.Fatalf("build handed the engine scope %q and a read client %v, want %q and %v", b.cdexReadScope, b.cdexReadClient, scope, scope != "")
			}
		})
	}
}
