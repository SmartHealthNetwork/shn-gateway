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

// cdexSubscriptionEnv is cdexTaskEnv with the subscription scope set.
func cdexSubscriptionEnv(t *testing.T) map[string]string {
	t.Helper()
	e := cdexTaskEnv(t)
	e["PROVIDER_CDEX_SUBSCRIPTION_SCOPE"] = "system/Subscription.cruds"
	return e
}

// A provider that sets the subscription scope beside its Task endpoint loads,
// the scope read trimmed, and declares the payer's Subscription to its Tasks'
// changes beside the attachment request; without the scope, or with a blank
// one, it declares the attachment request alone.
func TestLoadConfig_ProviderCDexSubscriptionScopeTakesTheLeg(t *testing.T) {
	e := cdexSubscriptionEnv(t)
	e["PROVIDER_CDEX_SUBSCRIPTION_SCOPE"] = "  system/Subscription.cruds  "
	cfg, err := loadEnv(e)
	if err != nil {
		t.Fatalf("a subscription scope with a Task endpoint should load: %v", err)
	}
	if cfg.ProviderCDexSubscriptionScope != "system/Subscription.cruds" {
		t.Fatalf("subscription scope %q, want it trimmed", cfg.ProviderCDexSubscriptionScope)
	}
	if got := capabilityDeclaration(cfg).Legs; !slices.Contains(got, "cdex-task-subscribe") || !slices.Contains(got, "cdex-attachment-request") {
		t.Fatalf("declares %v, want the subscribe leg beside the attachment request", got)
	}
	for _, scope := range []string{"", "   "} {
		e := cdexTaskEnv(t)
		e["PROVIDER_CDEX_SUBSCRIPTION_SCOPE"] = scope
		cfg, err := loadEnv(e)
		if err != nil {
			t.Fatalf("subscription scope %q: %v", scope, err)
		}
		if got := capabilityDeclaration(cfg).Legs; slices.Contains(got, "cdex-task-subscribe") || !slices.Contains(got, "cdex-attachment-request") {
			t.Errorf("subscription scope %q: declares %v, want the attachment request alone", scope, got)
		}
	}
}

// The subscription scope's boot refusals, each naming the setting and the
// fix: on another role, without the Task endpoint, without the ingress base
// the notification address is written under, and with a Task endpoint whose
// base cannot be found.
func TestLoadConfig_ProviderCDexSubscriptionScopeRefusals(t *testing.T) {
	const fix = ": set it to the provider's Task endpoint, e.g. https://ehr.example/fhir/Task, whose base (https://ehr.example/fhir) serves its Subscription endpoint"
	const notTask = "gateway: PROVIDER_CDEX_SUBSCRIPTION_SCOPE set requires PROVIDER_CDEX_TASK_URL to end in a Task segment, with no query or fragment (%q)" + fix
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
			e["PROVIDER_CDEX_SUBSCRIPTION_SCOPE"] = "system/Subscription.cruds"
		}, `gateway: PROVIDER_CDEX_SUBSCRIPTION_SCOPE is provider-only (role="payer"): only a provider takes the payer's attachment request, its read of that Task and its Subscription to that Task's changes`},
		"without the Task endpoint": {func(e map[string]string) {
			for k := range e {
				if strings.HasPrefix(k, "PROVIDER_CDEX_") && k != "PROVIDER_CDEX_SUBSCRIPTION_SCOPE" {
					delete(e, k)
				}
			}
		}, "gateway: PROVIDER_CDEX_SUBSCRIPTION_SCOPE set requires PROVIDER_CDEX_TASK_URL, the provider's own Task endpoint"},
		// The ingress itself requires its base first; with neither, the
		// subscription scope names what it needs.
		"without the ingress base": {func(e map[string]string) {
			delete(e, "PROVIDER_DAVINCI_INGRESS_BASE_URL")
			delete(e, "PROVIDER_DAVINCI_INGRESS")
		}, "gateway: PROVIDER_CDEX_SUBSCRIPTION_SCOPE set requires PROVIDER_DAVINCI_INGRESS and PROVIDER_DAVINCI_INGRESS_BASE_URL: each Subscription's notification address is written under that base"},
		"without the ingress": {func(e map[string]string) {
			delete(e, "PROVIDER_DAVINCI_INGRESS")
		}, "gateway: PROVIDER_CDEX_SUBSCRIPTION_SCOPE set requires PROVIDER_DAVINCI_INGRESS and PROVIDER_DAVINCI_INGRESS_BASE_URL"},
		"a query":          {func(e map[string]string) { e["PROVIDER_CDEX_TASK_URL"] = "https://provider.test/fhir/Task?x=1" }, fmt.Sprintf(notTask, "https://provider.test/fhir/Task?x=1")},
		"no Task segment":  {func(e map[string]string) { e["PROVIDER_CDEX_TASK_URL"] = "https://provider.test/fhir/submit" }, fmt.Sprintf(notTask, "https://provider.test/fhir/submit")},
		"a trailing slash": {func(e map[string]string) { e["PROVIDER_CDEX_TASK_URL"] = "https://provider.test/fhir/Task/" }, fmt.Sprintf(notTask, "https://provider.test/fhir/Task/")},
		"a dot segment": {func(e map[string]string) { e["PROVIDER_CDEX_TASK_URL"] = "https://provider.test/x/../Task" },
			`gateway: PROVIDER_CDEX_SUBSCRIPTION_SCOPE set requires PROVIDER_CDEX_TASK_URL with no "." or ".." path segment ("https://provider.test/x/../Task"): remove the dot segments`},
		"no host": {func(e map[string]string) { e["PROVIDER_CDEX_TASK_URL"] = "https:///fhir/Task" },
			`gateway: PROVIDER_CDEX_SUBSCRIPTION_SCOPE set requires PROVIDER_CDEX_TASK_URL to be an absolute http or https URL with a host ("https:///fhir/Task")` + fix},
	} {
		t.Run(name, func(t *testing.T) {
			e := cdexSubscriptionEnv(t)
			c.edit(e)
			if _, err := loadEnv(e); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("loadConfig: %v, want an error containing %q", err, c.want)
			}
		})
	}
}

// The subscription's client asks the Task forward's token endpoint, with the
// same client, for the subscription scope; the forward's client keeps asking
// for its own. Each holds its own token.
func TestProviderCDexSubscriptionClientAsksForItsScope(t *testing.T) {
	var mu sync.Mutex
	scopes := map[string]string{}
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
	e := cdexSubscriptionEnv(t)
	delete(e, "PROVIDER_CDEX_CLIENT_KEY")
	delete(e, "PROVIDER_CDEX_CLIENT_ALG")
	e["PROVIDER_CDEX_CLIENT_SECRET"] = "s"
	e["PROVIDER_CDEX_TOKEN_URL"] = tokenSrv.URL
	cfg, err := loadEnv(e)
	if err != nil {
		t.Fatal(err)
	}
	cfg.certs = endpointcert.New()
	sub, err := providerCDexSubscriptionHTTPClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	forward, err := providerCDexHTTPClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []*http.Client{sub, forward} {
		resp, err := c.Get(sys.URL + "/fhir/Subscription/s-1")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bearers) != 2 || scopes[bearers[0]] != "system/Subscription.cruds" || scopes[bearers[1]] != "system/Task.c" {
		t.Fatalf("bearers %v over scopes %v: want the subscription's asked for its scope, the forward's for system/Task.c", bearers, scopes)
	}
}

// build hands engine.New the subscription scope and its client exactly when
// the scope is set: deleting either line leaves a row red.
func TestBuildWiresTheSubscriptionScopeToGateway(t *testing.T) {
	for name, scope := range map[string]string{"set": "system/Subscription.cruds", "unset": ""} {
		t.Run(name, func(t *testing.T) {
			e := cdexSubscriptionEnv(t)
			delete(e, "PROVIDER_CDEX_CLIENT_KEY")
			delete(e, "PROVIDER_CDEX_CLIENT_ALG")
			e["PROVIDER_CDEX_CLIENT_SECRET"] = "s"
			e["PROVIDER_CDEX_SUBSCRIPTION_SCOPE"] = scope
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
			if b.cdexSubscriptionScope != scope || b.cdexSubscriptionClient != (scope != "") || b.subscriptionPredecessor {
				t.Fatalf("build handed the engine scope %q, a client %v and a predecessor %v; want %q, %v and none",
					b.cdexSubscriptionScope, b.cdexSubscriptionClient, b.subscriptionPredecessor, scope, scope != "")
			}
		})
	}
}
