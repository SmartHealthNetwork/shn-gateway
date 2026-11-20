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

// pasSubscriptionEnv is a payer that forwards to its own Da Vinci endpoint,
// authenticated, with its Da Vinci ingress, and the PAS subscription scope
// set, then extra on top.
func pasSubscriptionEnv(t *testing.T, extra map[string]string) map[string]string {
	t.Helper()
	unlisted := strings.Replace(testValidClientsJSON(t), `,"scopes":["system/Davinci.write"]`, "", 1)
	e := payerEnv(map[string]string{
		"PAYER_DAVINCI_INGRESS": "1", "PAYER_DAVINCI_INGRESS_BASE_URL": "https://payer-gw.test", "INGRESS_CLIENTS_FILE": writeClientsFile(t, unlisted),
		"PAYER_DAVINCI_TOKEN_URL": "https://payer.example/token", "PAYER_DAVINCI_CLIENT_ID": "gw", "PAYER_DAVINCI_CLIENT_SECRET": "s",
		"PAYER_DAVINCI_SCOPE": "system/*.read", "PAYER_DAVINCI_SUBSCRIPTION_SCOPE": "system/Subscription.cruds",
	})
	for k, v := range extra {
		e[k] = v
	}
	return e
}

// A payer that sets the PAS subscription scope loads, the scope read trimmed,
// and declares a provider's Subscription to the PAS topic from that setting
// alone; with it unset or blank, it declares none.
func TestLoadConfig_PayerSubscriptionScopeTakesTheLeg(t *testing.T) {
	e := pasSubscriptionEnv(t, map[string]string{"PAYER_DAVINCI_SUBSCRIPTION_SCOPE": "  system/Subscription.cruds  "})
	cfg, err := loadEnv(e)
	if err != nil {
		t.Fatalf("the subscription scope should load: %v", err)
	}
	if cfg.PayerDavinciSubscriptionScope != "system/Subscription.cruds" {
		t.Fatalf("subscription scope %q, want it trimmed", cfg.PayerDavinciSubscriptionScope)
	}
	if got := capabilityDeclaration(cfg).Legs; !slices.Contains(got, "pas-subscription-subscribe") {
		t.Fatalf("declares %v, want pas-subscription-subscribe among them", got)
	}
	for _, scope := range []string{"", "   "} {
		cfg, err := loadEnv(pasSubscriptionEnv(t, map[string]string{"PAYER_DAVINCI_SUBSCRIPTION_SCOPE": scope}))
		if err != nil {
			t.Fatalf("subscription scope %q: %v", scope, err)
		}
		if got := capabilityDeclaration(cfg).Legs; slices.Contains(got, "pas-subscription-subscribe") {
			t.Errorf("subscription scope %q: declares %v", scope, got)
		}
	}
}

// The subscription scope's boot refusals, each naming the setting and what it
// needs: on another role, without the ingress or its base, without a token
// endpoint, and with a base its Subscription endpoint cannot be at (named by
// the setting that gives it: the PAS base, or the Da Vinci base when that is
// unset).
func TestLoadConfig_PayerSubscriptionScopeRefusals(t *testing.T) {
	for name, c := range map[string]struct {
		env  map[string]string
		drop []string
		want string
	}{
		"another role": {env: map[string]string{"ROLE": "provider"}, want: "PAYER_DAVINCI_SUBSCRIPTION_SCOPE is payer-only"},
		"no ingress":   {drop: []string{"PAYER_DAVINCI_INGRESS", "PAYER_DAVINCI_INGRESS_BASE_URL", "INGRESS_CLIENTS_FILE"}, want: "requires PAYER_DAVINCI_INGRESS and PAYER_DAVINCI_INGRESS_BASE_URL"},
		"no token endpoint": {drop: []string{"PAYER_DAVINCI_TOKEN_URL", "PAYER_DAVINCI_CLIENT_ID", "PAYER_DAVINCI_CLIENT_SECRET", "PAYER_DAVINCI_SCOPE"},
			want: "requires PAYER_DAVINCI_TOKEN_URL"},
		"a PAS base with a query": {env: map[string]string{"PAYER_DAVINCI_PAS_BASE_URL": "https://payer.example/pas?x=1"},
			want: "requires PAYER_DAVINCI_PAS_BASE_URL, the base the payer's Subscription endpoint is at, to be an absolute http or https URL"},
		"a Da Vinci base with a fragment": {env: map[string]string{"PAYER_DAVINCI_BASE_URL": "https://payer.example/fhir#f"},
			want: "requires PAYER_DAVINCI_BASE_URL, the base the payer's Subscription endpoint is at"},
	} {
		t.Run(name, func(t *testing.T) {
			e := pasSubscriptionEnv(t, c.env)
			for _, k := range c.drop {
				delete(e, k)
			}
			_, err := loadEnv(e)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err %v, want one naming %q", err, c.want)
			}
		})
	}
}

// The Subscription endpoint's client asks the payer's token endpoint for the
// subscription scope, with the Da Vinci client: its own token, never the
// forward's.
func TestPayerSubscriptionClientAsksForItsScope(t *testing.T) {
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
	cfg, err := loadEnv(pasSubscriptionEnv(t, map[string]string{"PAYER_DAVINCI_TOKEN_URL": tokenSrv.URL}))
	if err != nil {
		t.Fatal(err)
	}
	cfg.certs = endpointcert.New()
	sub, err := payerSubscriptionHTTPClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	forward, err := payerDavinciHTTPClient(cfg)
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
	if len(bearers) != 2 || scopes[bearers[0]] != "system/Subscription.cruds" || scopes[bearers[1]] != "system/*.read" {
		t.Fatalf("bearers %v over scopes %v: want the subscription's asked for its scope, the forward's for the Da Vinci scope", bearers, scopes)
	}
}

// build hands engine.New the subscription scope, its client, the base (the
// PAS base, or the Da Vinci base when that is unset) and the payer's backend
// options exactly when the scope is set, and reads the payer system's
// CapabilityStatement once at startup: one that declares no Subscription is
// logged as a warning, and the leg is declared all the same.
func TestBuildWiresThePayerSubscriptionScopeToGateway(t *testing.T) {
	var mu sync.Mutex
	var metadata []string
	sys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"access_token":"AT","token_type":"bearer","expires_in":3600}`)
		case strings.HasSuffix(r.URL.Path, "/metadata"):
			mu.Lock()
			metadata = append(metadata, r.URL.Path+" "+r.Header.Get("X-Backend"))
			mu.Unlock()
			w.Header().Set("Content-Type", "application/fhir+json")
			fmt.Fprint(w, `{"resourceType":"CapabilityStatement","rest":[{"mode":"server","resource":[{"type":"Claim"}]}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer sys.Close()
	for name, c := range map[string]struct {
		scope, pasBase, wantBase string
	}{
		"set, the Da Vinci base":  {"system/Subscription.cruds", "", sys.URL + "/fhir"},
		"set, the PAS base first": {"system/Subscription.cruds", sys.URL + "/pas", sys.URL + "/pas"},
		"unset":                   {"", "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			mu.Lock()
			metadata = nil
			mu.Unlock()
			env := pasSubscriptionEnv(t, map[string]string{
				"PAYER_DAVINCI_SUBSCRIPTION_SCOPE": c.scope, "PAYER_DAVINCI_TOKEN_URL": sys.URL + "/token",
				"PAYER_DAVINCI_BACKEND_HEADERS": "X-Backend: edge-key",
			})
			if c.pasBase != "" {
				env["PAYER_DAVINCI_PAS_BASE_URL"] = c.pasBase
			}
			extra := map[string]string{}
			for k, v := range env {
				if (strings.HasPrefix(k, "PAYER_") && k != "PAYER_DAVINCI_BASE_URL") || k == "INGRESS_CLIENTS_FILE" {
					extra[k] = v
				}
			}
			b, _ := buildPayerAt(t, sys.URL+"/fhir", extra)
			if b.payerSubscriptionScope != c.scope || b.payerSubscriptionClient != (c.scope != "") || b.payerSubscriptionBase != c.wantBase || (b.payerSubscriptionOptions > 0) != (c.scope != "") {
				t.Fatalf("build handed the engine scope %q, a client %v, base %q and %d options; want %q, %v, %q and options exactly when set",
					b.payerSubscriptionScope, b.payerSubscriptionClient, b.payerSubscriptionBase, b.payerSubscriptionOptions, c.scope, c.scope != "", c.wantBase)
			}
			mu.Lock()
			defer mu.Unlock()
			if c.scope == "" {
				if len(metadata) != 0 {
					t.Fatalf("a payer without the scope read %v", metadata)
				}
				return
			}
			if want := []string{strings.TrimPrefix(c.wantBase, sys.URL) + "/metadata edge-key"}; !slices.Equal(metadata, want) {
				t.Fatalf("the startup read %v, want %v: once, at the Subscription base, with the backend headers", metadata, want)
			}
		})
	}
}
