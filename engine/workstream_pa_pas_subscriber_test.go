//go:build !shn_coreonly

package engine

// workstream_pa_pas_subscriber_test.go — the provider gateway's side of the
// provider system's Subscription to the PAS topic at a payer's base
// (pas-subscription-subscribe; workstream_pa_pas_subscriber.go): the scope
// each interaction takes, a base that names no payer, the routes mounted only
// where the provider takes the topic's notifications, and the scopes the
// provider's SMART configuration advertises. The interactions themselves run
// through both gateways and the real Hub in test/adversarial
// (pas_subscription_signal_test.go, pas_subscription_notify_test.go).

import (
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

const pasSubscriberBody = `{"resourceType":"Subscription","status":"requested","channel":{"type":"rest-hook","endpoint":"https://ehr.example/notify"}}`

// pasSubscriberCalls are the Subscription interactions at a payer's base,
// with the permission each takes.
var pasSubscriberCalls = []struct {
	name, method, rest, perm string
}{
	{"create", http.MethodPost, "/Subscription", "c"},
	{"search", http.MethodGet, "/Subscription?url=https://ehr.example/notify", "s"},
	{"read", http.MethodGet, "/Subscription/sub-1", "r"},
	{"vread", http.MethodGet, "/Subscription/sub-1/_history/2", "r"},
	{"$status", http.MethodGet, "/Subscription/sub-1/$status", "r"},
	{"update", http.MethodPut, "/Subscription/sub-1", "u"},
	{"patch", http.MethodPatch, "/Subscription/sub-1", "u"},
	{"delete", http.MethodDelete, "/Subscription/sub-1", "d"},
}

// pasSubscriberRequest is the provider system's call at target.
func pasSubscriberRequest(method, target string) *http.Request {
	var body string
	if method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch {
		body = pasSubscriberBody
	}
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/fhir+json")
	}
	return req
}

// pasSubscriberGateway is the in-process exchange's provider gateway rebuilt
// to take the PAS topic's notifications, behind real ingress auth with one
// native client registered for scopes, at level.
func pasSubscriberGateway(t *testing.T, level ConformanceEnforcement, scopes []string) (*inProcessExchange, *exchangeRecords, *ecdsa.PrivateKey) {
	t.Helper()
	return pasProviderGateway(t, level, scopes, true)
}

// pasProviderGateway is pasSubscriberGateway with the setting as takes.
func pasProviderGateway(t *testing.T, level ConformanceEnforcement, scopes []string, takes bool) (*inProcessExchange, *exchangeRecords, *ecdsa.PrivateKey) {
	t.Helper()
	env, got, _ := pasBaseExchange(t, level)
	priv, pub := newTestClientKey(t)
	cfg := env.originator.cfg
	cfg.ProviderPASSubscription = takes
	cfg.ingressAuthBypass = false
	cfg.IngressBaseURL = testIngressBaseURL
	cfg.IngressClients = map[string]IngressClientRegistration{"provider-system": {Alg: "ES384", PublicKeyPEM: pub, Scopes: scopes}}
	cfg.Clock = ingressFixedClock()
	env.substrate.clock = cfg.Clock
	env.originator = mustNew(t, cfg)
	return env, got, priv
}

// Each Subscription interaction at a payer's base takes a bearer whose scopes
// permit it on Subscription, at every level: one granted only the Da Vinci
// scope, or every Subscription permission but the one the interaction takes,
// as a Subscription scope or a wildcard, is refused 403 insufficient_scope
// naming the scope to ask for, recorded as the provider gateway's authority
// refusal, with nothing sent; no bearer is 401 on authentication. The
// control: the same client granted every permission, by a Subscription scope
// or a wildcard, is admitted, and its call crosses the network.
func TestPASSubscriptionBase_TakesAScopeThatPermitsTheInteraction(t *testing.T) {
	registered := []string{ingressScope, "system/Subscription.cruds", "system/Subscription.ruds", "system/Subscription.cuds",
		"system/Subscription.crds", "system/Subscription.crus", "system/Subscription.crud",
		"system/*.cruds", "system/*.ruds", "system/*.cuds", "system/*.crds", "system/*.crus", "system/*.crud"}
	for _, c := range pasSubscriberCalls {
		for _, level := range allLevels {
			for _, row := range []struct {
				name, scope string
				status      int
			}{
				{"no bearer", "", http.StatusUnauthorized},
				{"the Da Vinci scope only", ingressScope, http.StatusForbidden},
				{"every permission but " + c.perm, "system/Subscription." + strings.Replace("cruds", c.perm, "", 1), http.StatusForbidden},
				{"every permission", "system/Subscription.cruds", 0},
				{"a wildcard with every permission but " + c.perm, "system/*." + strings.Replace("cruds", c.perm, "", 1), http.StatusForbidden},
				{"a wildcard with every permission", "system/*.cruds", 0},
			} {
				t.Run(fmt.Sprintf("%s/%s/%s", c.name, row.name, level), func(t *testing.T) {
					env, got, key := pasSubscriberGateway(t, level, registered)
					req := pasSubscriberRequest(c.method, testIngressBaseURL+"/pas/payer/"+env.payerID+c.rest)
					if row.scope != "" {
						req.Header.Set("Authorization", "Bearer "+scopedBearer(t, env.originator, key, "provider-system", row.scope))
					}
					rec := httptest.NewRecorder()
					env.originator.Handler().ServeHTTP(rec, req)
					x := got.only(t)
					if row.status == 0 {
						// Admitted: what follows is the leg's, not the door's.
						if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden || x.Rule == RefusalAuthentication || x.Rule == RefusalAuthority {
							t.Fatalf("admitted client refused: %d %s (%s)", rec.Code, rec.Body, x.Rule)
						}
						wantCarriedOrPatchRefused(t, env, c.method, rec)
						return
					}
					refusedBeforeTheNetwork(t, env, rec, row.status, "")
					if x.Route != RoutePASSubscriptionBase {
						t.Fatalf("recorded on route %q, want %q", x.Route, RoutePASSubscriptionBase)
					}
					if row.status == http.StatusUnauthorized {
						if x.Rule != RefusalAuthentication {
							t.Fatalf("record %+v, want authentication", x)
						}
						return
					}
					need, reason := subscriptionNeed(c.method, strings.SplitN(strings.TrimPrefix(c.rest, "/"), "?", 2)[0])
					if ch := rec.Header().Get("WWW-Authenticate"); !strings.Contains(ch, `insufficient_scope`) || !strings.Contains(ch, `scope="`+subscriptionChallengeScope(need)+`"`) {
						t.Fatalf("challenge %q, want insufficient_scope naming %s", ch, subscriptionChallengeScope(need))
					}
					if x.Rule != RefusalAuthority || x.RefusedBy != RefusedByProviderGateway || x.RefusalReason != string(reason) {
						t.Fatalf("record %+v, want the provider gateway's authority refusal %q", x, reason)
					}
				})
			}
		}
	}
}

// A base that names no registered payer (no holder, or a holder that is not a
// payer) is refused on routing before anything is sent, at every level: a
// create 422, any other interaction 404 with FHIR's OperationOutcome, each
// naming where the payers' bases are listed. The control: the same call at the
// payer's own base crosses the network.
func TestPASSubscriptionBase_ABaseNamingNoPayerIsRefusedOnRouting(t *testing.T) {
	for _, c := range pasSubscriberCalls {
		for _, base := range []string{"nobody", "provider"} {
			for _, level := range allLevels {
				t.Run(c.name+"/"+base+"/"+level.String(), func(t *testing.T) {
					env, got, _ := pasBaseExchange(t, level)
					env.originator.cfg.ProviderPASSubscription = true
					rec := httptest.NewRecorder()
					env.originator.Handler().ServeHTTP(rec, pasSubscriberRequest(c.method, "/pas/payer/"+base+c.rest))
					want := http.StatusNotFound
					if c.method == http.MethodPost {
						want = http.StatusUnprocessableEntity
					}
					refusedBeforeTheNetwork(t, env, rec, want, PASPayersPath)
					if want == http.StatusNotFound {
						wantNotFoundOutcome(t, rec, errPASSubscriptionNoPayer)
					}
					x := got.only(t)
					wantRefusal(t, x, want, RefusedByProviderGateway, RefusalRouting)
					if x.RefusalReason != string(reasonPASSubscriptionNoPayer) || x.Route != RoutePASSubscriptionBase {
						t.Fatalf("record reason %q on %s, want %q on %s", x.RefusalReason, x.Route, reasonPASSubscriptionNoPayer, RoutePASSubscriptionBase)
					}
					wantReasonInAnswer(t, x, rec.Body.String())
				})
			}
		}
		env, _, _ := pasBaseExchange(t, EnforcementStrict)
		env.originator.cfg.ProviderPASSubscription = true
		rec := httptest.NewRecorder()
		env.originator.Handler().ServeHTTP(rec, pasSubscriberRequest(c.method, "/pas/payer/"+env.payerID+c.rest))
		wantCarriedOrPatchRefused(t, env, c.method, rec)
	}
}

// wantCarriedOrPatchRefused fails unless the call crossed the network once,
// or, a PATCH, was refused 405 with nothing sent: a Subscription's channel is
// sealed before it is sent, which a patch cannot be (serveSubscriptions).
func wantCarriedOrPatchRefused(t *testing.T, env *inProcessExchange, method string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if method == http.MethodPatch {
		refusedBeforeTheNetwork(t, env, rec, http.StatusMethodNotAllowed, "PUT")
		return
	}
	if env.routeHitCount() != 1 {
		t.Fatalf("%d %s: crossed the network %d times, want once", rec.Code, rec.Body, env.routeHitCount())
	}
}

// The routes are mounted only where the provider takes the PAS topic's
// notifications: without PROVIDER_PAS_SUBSCRIPTION each call at a payer's base
// is answered as any path the gateway does not serve, nothing sent.
func TestPASSubscriptionBase_MountedOnlyWhereTheProviderTakesTheNotifications(t *testing.T) {
	for _, c := range pasSubscriberCalls {
		env, _, _ := pasBaseExchange(t, EnforcementNone)
		rec := httptest.NewRecorder()
		env.originator.Handler().ServeHTTP(rec, pasSubscriberRequest(c.method, "/pas/payer/"+env.payerID+c.rest))
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed || env.routeHitCount() != 0 {
			t.Fatalf("%s without the setting: %d %s, crossed %d times", c.name, rec.Code, rec.Body, env.routeHitCount())
		}
	}
}

// A provider whose own system manages its Subscription to the PAS topic, its
// one client registered for a wildcard that grants every permission alone,
// is admitted on a create at a payer's PAS base, while its SMART
// configuration lists the Da Vinci routes' scopes alone: the configuration
// names scopes sufficient for the routes, not every scope they admit.
func TestPASSubscriptionBase_AWildcardIsAdmittedNotAdvertised(t *testing.T) {
	const wildcard = "system/*.cruds"
	env, got, key := pasProviderGateway(t, EnforcementNone, []string{wildcard}, true)
	rec := httptest.NewRecorder()
	env.originator.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, testIngressBaseURL+"/.well-known/smart-configuration", nil))
	var conf struct {
		Scopes []string `json:"scopes_supported"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &conf); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("smart-configuration: %d %s", rec.Code, rec.Body)
	}
	if want := providerScopes(compiledRouteScopes()); !slices.Equal(conf.Scopes, want) {
		t.Fatalf("scopes_supported %q, want the Da Vinci routes' scopes alone %q", conf.Scopes, want)
	}
	req := pasSubscriberRequest(http.MethodPost, testIngressBaseURL+"/pas/payer/"+env.payerID+"/Subscription")
	req.Header.Set("Authorization", "Bearer "+scopedBearer(t, env.originator, key, "provider-system", wildcard))
	created := httptest.NewRecorder()
	env.originator.Handler().ServeHTTP(created, req)
	if x := got.only(t); created.Code == http.StatusUnauthorized || created.Code == http.StatusForbidden || x.Rule == RefusalAuthentication || x.Rule == RefusalAuthority {
		t.Fatalf("a client granted %s refused: %d %s (%s)", wildcard, created.Code, created.Body, x.Rule)
	}
	wantCarriedOrPatchRefused(t, env, http.MethodPost, created)
}

// A provider's SMART configuration advertises, after the scopes its Da Vinci
// routes take (providerScopes), each Subscription scope with no query a
// native client is registered for, once it takes the PAS topic's
// notifications; never one with a query, a module client's, a wildcard, or
// any at all without the setting. It names scopes sufficient for its routes,
// not every scope they admit: a route admits a wildcard that grants what it
// needs (TestPASSubscriptionBase_AWildcardIsAdmittedNotAdvertised), as it
// admits another spelling of its own scope, and the configuration names
// neither. The boot rule refuses a Subscription scope SMART does
// not spell and requires none.
func TestPASSubscriptionBase_TheProvidersScopes(t *testing.T) {
	clients := map[string]IngressClientRegistration{
		"a": {Scopes: []string{ingressScope, "system/Subscription.cruds", "system/Subscription.r?x=1"}},
		"b": {Scopes: []string{"system/Subscription.read"}},
		"m": {Module: "x", Scopes: []string{"system/Subscription.*"}},
		"w": {Scopes: []string{"system/*.rs", "system/*.cruds"}},
	}
	routes := providerScopes(compiledRouteScopes())
	if slices.ContainsFunc(routes, namesSubscription) {
		t.Fatalf("the Da Vinci routes' scopes %q name Subscription", routes)
	}
	if got, want := ingressScopes("provider", clients, true), slices.Concat(routes, []string{"system/Subscription.cruds", "system/Subscription.read"}); !slices.Equal(got, want) {
		t.Fatalf("advertised %q, want %q", got, want)
	}
	if got := ingressScopes("provider", clients, false); !slices.Equal(got, routes) {
		t.Fatalf("advertised %q without the setting, want the Da Vinci routes' scopes alone %q", got, routes)
	}
	for scopes, ok := range map[string]bool{
		"":           true,
		ingressScope: true,
		ingressScope + " system/Subscription.cruds": true,
		"system/Subscription.read":                  true,
		"system/Subscription.xyz":                   false,
		"system/Subscription.":                      false,
		"system/Subscription.rc":                    false,
	} {
		if err := CheckProviderIngressScopes(strings.Fields(scopes), RouteScopeResources()); (err == nil) != ok {
			t.Errorf("%q: %v, want ok %v", scopes, err, ok)
		}
	}
}

// The SMART configuration a provider gateway serves, at the plain base and at
// a payer's PAS base, advertises each Subscription scope with no query its
// native client is registered for once it takes the PAS topic's
// notifications, after the Da Vinci routes' scopes; without the setting, those
// alone. Read from the served document, so a gateway built without the
// setting reaching ingressScopes fails it.
func TestPASSubscriptionBase_TheSMARTConfigurationAdvertisesTheScopes(t *testing.T) {
	registered := []string{ingressScope, "system/Subscription.cruds", "system/Subscription.r?x=1"}
	routes := providerScopes(compiledRouteScopes())
	for takes, want := range map[bool][]string{
		true:  slices.Concat(routes, []string{"system/Subscription.cruds"}),
		false: routes,
	} {
		env, _, _ := pasProviderGateway(t, EnforcementNone, registered, takes)
		for _, path := range []string{"/.well-known/smart-configuration", "/pas/payer/" + env.payerID + "/.well-known/smart-configuration"} {
			rec := httptest.NewRecorder()
			env.originator.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, testIngressBaseURL+path, nil))
			var conf struct {
				Scopes []string `json:"scopes_supported"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &conf); err != nil || rec.Code != http.StatusOK {
				t.Fatalf("takes %v, %s: %d %s", takes, path, rec.Code, rec.Body)
			}
			if !slices.Equal(conf.Scopes, want) {
				t.Errorf("takes %v, %s: scopes_supported %q, want %q", takes, path, conf.Scopes, want)
			}
		}
	}
}

// A provider's Subscription scope lists the payers' PAS bases exactly where
// its SMART configuration advertises it: with PROVIDER_PAS_SUBSCRIPTION, where
// the routes under those bases take it, both; without, neither, so a scope is
// never admitted for more than it can be used for, nor advertised for less.
func TestPASSubscriptionBase_TheListingAdmitsWhatIsAdvertised(t *testing.T) {
	const scope = "system/Subscription.cruds"
	for _, takes := range []bool{true, false} {
		t.Run(fmt.Sprintf("setting %v", takes), func(t *testing.T) {
			env, _, key := pasProviderGateway(t, EnforcementNone, []string{scope}, takes)
			rec := httptest.NewRecorder()
			env.originator.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, testIngressBaseURL+"/.well-known/smart-configuration", nil))
			var conf struct {
				Scopes []string `json:"scopes_supported"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &conf); err != nil || rec.Code != http.StatusOK {
				t.Fatalf("smart-configuration: %d %s", rec.Code, rec.Body)
			}
			req := httptest.NewRequest(http.MethodGet, testIngressBaseURL+"/pas/payers", nil)
			req.Header.Set("Authorization", "Bearer "+scopedBearer(t, env.originator, key, "provider-system", scope))
			listed := httptest.NewRecorder()
			env.originator.Handler().ServeHTTP(listed, req)
			advertised := slices.Contains(conf.Scopes, scope)
			admitted := listed.Code != http.StatusUnauthorized && listed.Code != http.StatusForbidden
			if advertised != takes || admitted != takes {
				t.Fatalf("advertised %v (%q), listing %d %s; want both %v", advertised, conf.Scopes, listed.Code, listed.Body, takes)
			}
			if !takes && listed.Code != http.StatusForbidden {
				t.Fatalf("listing %d %s, want 403 insufficient_scope", listed.Code, listed.Body)
			}
		})
	}
}

// A module ingress client never makes a Subscription: one registered for a
// wildcard, which the scope check admits, is refused by the route's builder
// check, 422, at every level, with nothing sent: with no builder named, and
// with each builder its module has named, since none builds a Subscription
// interaction. The boot rule refuses a module client's Subscription scope
// because of this: a wildcard is the only scope that reaches here.
func TestPASSubscriptionBase_AModuleClientMakesNoSubscription(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env, _, _ := pasBaseExchange(t, level)
			priv, pub := newTestClientKey(t)
			cfg := env.originator.cfg
			cfg.ProviderPASSubscription = true
			cfg.ingressAuthBypass = false
			cfg.IngressBaseURL = testIngressBaseURL
			cfg.IngressClients = map[string]IngressClientRegistration{"module-1": {Alg: "ES384", PublicKeyPEM: pub, Scopes: []string{"system/*.cruds"}, Module: ModuleOnBehalf}}
			cfg.Clock = ingressFixedClock()
			env.substrate.clock = cfg.Clock
			env.originator = mustNew(t, cfg)
			post := func(builder string) *httptest.ResponseRecorder {
				req := pasSubscriberRequest(http.MethodPost, testIngressBaseURL+"/pas/payer/"+env.payerID+"/Subscription")
				req.Header.Set("Authorization", "Bearer "+scopedBearer(t, env.originator, priv, "module-1", "system/*.cruds"))
				if builder != "" {
					req.Header.Set(BuilderHeader, builder)
				}
				rec := httptest.NewRecorder()
				env.originator.Handler().ServeHTTP(rec, req)
				return rec
			}
			refusedBeforeTheNetwork(t, env, post(""), http.StatusUnprocessableEntity, errBuilderUndeclared)
			builders := ModuleBuilders(ModuleOnBehalf)
			if len(builders) == 0 {
				t.Fatal("control: the module declares no builder")
			}
			for _, b := range builders {
				refusedBeforeTheNetwork(t, env, post(string(b)), http.StatusUnprocessableEntity, "does not build a "+legPASSubscriptionSubscribe+" request")
			}
		})
	}
}
