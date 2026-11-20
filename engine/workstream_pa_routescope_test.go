//go:build !shn_coreonly

package engine

import (
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// providerScopeClient is the one ingress client a providerScopeGateway
// registers.
const providerScopeClient = "provider-ehr"

// providerScopeGateway is a provider gateway with real ingress auth whose one
// client, providerScopeClient, is registered for scopes.
func providerScopeGateway(t *testing.T, pub []byte, scopes []string, opts ...func(*Config)) *Gateway {
	t.Helper()
	_, signPriv := genED25519(t)
	sor := newCensusSoR()
	cfg := Config{
		Role:           "provider",
		HolderID:       "provider",
		PayerRouter:    payerRouterFor(t, "payer"),
		Identity:       shnsdk.Identity{HolderID: "provider", SignPriv: signPriv},
		IngressEnabled: true,
		IngressBaseURL: testIngressBaseURL,
		IngressClients: map[string]IngressClientRegistration{
			providerScopeClient: {Alg: "ES384", PublicKeyPEM: pub, Scopes: scopes},
		},
		Reg:       shnsdk.NewRegistry(),
		Validator: shnsdk.NewFakeValidator(),
		SoR:       sor,
		Store:     sor,
		Clock:     ingressFixedClock(),
		HubURL:    "http://hub.test",
	}
	for _, o := range opts {
		o(&cfg)
	}
	return mustNew(t, cfg)
}

var routeScopeJTI int

// providerDirectBearer is providerScopeClient's own signed bearer (the direct
// form br-provider sends), stating scope when it is not empty.
func providerDirectBearer(t *testing.T, priv *ecdsa.PrivateKey, scope string) string {
	t.Helper()
	now := ingressFixedClock()()
	routeScopeJTI++
	claims := jwt.MapClaims{"iss": providerScopeClient, "aud": testIngressBaseURL, "jti": fmt.Sprintf("rs-%d", routeScopeJTI),
		"iat": now.Unix(), "exp": now.Add(time.Minute).Unix()}
	if scope != "" {
		claims["scope"] = scope
	}
	return mintAssertion(t, priv, jwt.SigningMethodES384, claims)
}

// providerCall sends method path with bearer to g, with a body of {} on a
// POST.
func providerCall(g *Gateway, method, path, bearer string) *httptest.ResponseRecorder {
	var body *strings.Reader
	if method == http.MethodPost {
		body = strings.NewReader(`{}`)
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, testIngressBaseURL+path, body)
	req.Header.Set("Content-Type", "application/fhir+json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, req)
	return rec
}

// paRouteRequest is one PA provider ingress route, the scope it takes, and a
// scope of its own that permits it.
type paRouteRequest struct {
	name, method, path string
	scope              routeScope
	own                string
}

// paRouteRequests are a request to each address of every PA provider route
// that takes a scope.
var paRouteRequests = []paRouteRequest{
	{"CDS discovery", http.MethodGet, "/cds-services", scopeCDSService, ProviderUmbrellaScope},
	{"CRD hook", http.MethodPost, "/cds-services/shn-order-sign", scopeCDSService, ProviderUmbrellaScope},
	{"DTR package", http.MethodPost, "/Questionnaire/$questionnaire-package", scopeDTR, "system/Questionnaire.r"},
	{"DTR next-question", http.MethodPost, "/Questionnaire/$next-question", scopeDTR, "system/Questionnaire.read"},
	{"DTR next-question at a payer's address", http.MethodPost, "/dtr/payer/payer/Questionnaire/$next-question", scopeDTR, "system/Questionnaire.rs"},
	{"PAS submit", http.MethodPost, "/Claim/$submit", scopePASSubmit, "system/Claim.c"},
	{"PAS submit at a payer's base", http.MethodPost, "/pas/payer/payer/Claim/$submit", scopePASSubmit, "system/Claim.write"},
	{"PAS inquire", http.MethodPost, "/Claim/$inquire", scopePASInquire, "system/Claim.r"},
	{"PAS inquire at a payer's base", http.MethodPost, "/pas/payer/payer/Claim/$inquire", scopePASInquire, "system/Claim.read"},
	{"PAS payers", http.MethodGet, "/pas/payers", scopePASPayersUnsubscribed, "system/Claim.r"},
	{"eligibility", http.MethodPost, "/CoverageEligibilityRequest/$submit", scopeEligibility, "system/CoverageEligibilityRequest.c"},
	{"attachment", http.MethodPost, "/$submit-attachment", scopeAttachment, "system/DocumentReference.c"},
	{"attachment at a payer's address", http.MethodPost, "/cdex/payer/payer", scopeAttachment, "system/QuestionnaireResponse.c"},
	{"attachment at a payer's operation", http.MethodPost, "/cdex/payer/payer/$submit-attachment", scopeAttachment, "system/DocumentReference.write"},
	{"CDex payers", http.MethodGet, "/cdex/payers", scopeCDexPayers, "system/QuestionnaireResponse.write"},
}

// wantScopeRefused: rec is route's 403 insufficient_scope, with the challenge
// naming the route's own scopes, and the exchange record, where the route
// keeps one, an authority refusal giving the route's sentence.
func wantScopeRefused(t *testing.T, rec *httptest.ResponseRecorder, got *exchangeRecords, route paRouteRequest) {
	t.Helper()
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), string(route.scope.reason)) {
		t.Fatalf("answer %d %s, want 403 %q", rec.Code, rec.Body, route.scope.reason)
	}
	if want := `Bearer error="insufficient_scope", scope="` + route.scope.challenge() + `"`; rec.Header().Get("WWW-Authenticate") != want {
		t.Fatalf("challenge %q, want %q", rec.Header().Get("WWW-Authenticate"), want)
	}
	got.mu.Lock()
	n := len(got.recs)
	got.mu.Unlock()
	if n == 0 {
		return // a route that keeps no exchange record (discovery and the listings)
	}
	xr := got.only(t)
	wantRefusal(t, xr, http.StatusForbidden, RefusedByProviderGateway, RefusalAuthority)
	wantReasonInAnswer(t, xr, rec.Body.String())
}

// wantScopeAdmitted: rec is past the route's scope check: neither 401 nor 403.
func wantScopeAdmitted(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
		t.Fatalf("answer %d %s, want the route past its scope check", rec.Code, rec.Body)
	}
}

// TestPARouteScope_RefusedWithoutItsScopeAtEveryLevel: a client registered
// only for a Subscription scope (the client a system managing its PAS
// Subscriptions registers), or only for another route's scope, is refused
// 403 insufficient_scope on every PA provider ingress route, at every
// conformance level: per-leg authority, a network rule.
func TestPARouteScope_RefusedWithoutItsScopeAtEveryLevel(t *testing.T) {
	priv, pub := newTestClientKey(t)
	for _, level := range allLevels {
		for _, registered := range [][]string{{"system/Subscription.*"}, {"system/Task.c"}, {"system/Patient.read"}} {
			for _, route := range paRouteRequests {
				t.Run(fmt.Sprintf("%s/%s/%s", level, registered[0], route.name), func(t *testing.T) {
					var got exchangeRecords
					g := providerScopeGateway(t, pub, registered, func(c *Config) {
						c.ConformanceEnforcement = level
						c.ExchangeObserved = got.observe
					})
					wantScopeRefused(t, providerCall(g, route.method, route.path, providerDirectBearer(t, priv, "")), &got, route)
				})
			}
		}
	}
}

// TestPARouteScope_TheUmbrellaAndEachRoutesOwnScopeAdmit: a client registered
// for the umbrella, system/Davinci.write, the scope a client that lists none
// is registered for (app.loadIngressClients), is admitted on every PA route;
// one registered for a route's own scope is admitted on that route; and each
// is held to what it was registered for.
func TestPARouteScope_TheUmbrellaAndEachRoutesOwnScopeAdmit(t *testing.T) {
	priv, pub := newTestClientKey(t)
	umbrella := providerScopeGateway(t, pub, []string{ProviderUmbrellaScope})
	for _, route := range paRouteRequests {
		t.Run("umbrella/"+route.name, func(t *testing.T) {
			wantScopeAdmitted(t, providerCall(umbrella, route.method, route.path, providerDirectBearer(t, priv, "")))
		})
		t.Run("own/"+route.name, func(t *testing.T) {
			g := providerScopeGateway(t, pub, []string{route.own})
			wantScopeAdmitted(t, providerCall(g, route.method, route.path, providerDirectBearer(t, priv, "")))
		})
	}
}

// TestPARouteScope_Wildcards: SMART's wildcard scope permits a route for the
// permissions it grants, on whatever resource the route names, but never a
// CDS service, which names none and takes the umbrella alone. The reference
// provider registers system/*.rs, system/*.read and system/*.write.
func TestPARouteScope_Wildcards(t *testing.T) {
	priv, pub := newTestClientKey(t)
	for _, row := range []struct {
		wildcard string
		admitted []string // the route names it admits; every other PA route refuses
	}{
		{"system/*.c", []string{"PAS submit", "PAS submit at a payer's base", "PAS payers", "eligibility", "attachment", "attachment at a payer's address", "attachment at a payer's operation", "CDex payers"}},
		{"system/*.write", []string{"PAS submit", "PAS submit at a payer's base", "PAS payers", "eligibility", "attachment", "attachment at a payer's address", "attachment at a payer's operation", "CDex payers"}},
		{"system/*.rs", []string{"DTR package", "DTR next-question", "DTR next-question at a payer's address", "PAS inquire", "PAS inquire at a payer's base", "PAS payers"}},
		{"system/*.read", []string{"DTR package", "DTR next-question", "DTR next-question at a payer's address", "PAS inquire", "PAS inquire at a payer's base", "PAS payers"}},
		{"system/*.*", []string{"DTR package", "DTR next-question", "DTR next-question at a payer's address", "PAS submit", "PAS submit at a payer's base", "PAS inquire", "PAS inquire at a payer's base", "PAS payers", "eligibility", "attachment", "attachment at a payer's address", "attachment at a payer's operation", "CDex payers"}},
		{"system/*.c?patient=x", nil},
	} {
		g := providerScopeGateway(t, pub, []string{row.wildcard})
		for _, route := range paRouteRequests {
			t.Run(row.wildcard+"/"+route.name, func(t *testing.T) {
				var got exchangeRecords
				g.cfg.ExchangeObserved = got.observe
				rec := providerCall(g, route.method, route.path, providerDirectBearer(t, priv, ""))
				if slices.Contains(row.admitted, route.name) {
					wantScopeAdmitted(t, rec)
					return
				}
				wantScopeRefused(t, rec, &got, route)
			})
		}
	}
}

// TestPARouteScope_DTRsOwnScopes: a client registered for the scopes the DTR
// IG names as it writes them is admitted on the DTR routes; a scope on a
// route's resource in another case that the IG does not name is a boot error
// (CheckProviderIngressScopes).
func TestPARouteScope_DTRsOwnScopes(t *testing.T) {
	priv, pub := newTestClientKey(t)
	g := providerScopeGateway(t, pub, []string{"system/questionnaire.rs", "system/valueset.rs", "system/library.rs"})
	for _, route := range paRouteRequests {
		if route.scope.reason != scopeDTR.reason {
			continue
		}
		t.Run(route.name, func(t *testing.T) {
			wantScopeAdmitted(t, providerCall(g, route.method, route.path, providerDirectBearer(t, priv, "")))
		})
	}
	t.Run("the IG's own scopes start", func(t *testing.T) {
		if err := CheckProviderIngressScopes([]string{"system/questionnaire.rs", "system/valueset.rs", "system/library.rs"}, RouteScopeResources()); err != nil {
			t.Fatalf("the DTR IG's scopes refused at boot: %v", err)
		}
	})
	for _, scope := range []string{"system/questionnaire.r", "system/claim.c", "system/CLAIM.write", "system/documentreference.c"} {
		t.Run("refused at boot/"+scope, func(t *testing.T) {
			if err := CheckProviderIngressScopes([]string{scope}, RouteScopeResources()); err == nil {
				t.Fatalf("%s started", scope)
			}
		})
	}
}

// TestPARouteScope_ABearerNamingNoScopeIsGrantedTheRegistration: on a
// provider's ingress, a bearer that names no scope, direct or issued, is
// granted every scope its client is registered for, and the token endpoint's
// answer says so; one that names scopes keeps exactly those; and a direct
// bearer naming a scope its registration does not allow is granted none.
func TestPARouteScope_ABearerNamingNoScopeIsGrantedTheRegistration(t *testing.T) {
	priv, pub := newTestClientKey(t)
	g := providerScopeGateway(t, pub, []string{"system/Claim.c", "system/Claim.r"})
	submit, inquire := paRouteRequests[5], paRouteRequests[7]
	t.Run("a direct bearer naming none", func(t *testing.T) {
		wantScopeAdmitted(t, providerCall(g, submit.method, submit.path, providerDirectBearer(t, priv, "")))
		wantScopeAdmitted(t, providerCall(g, inquire.method, inquire.path, providerDirectBearer(t, priv, "")))
	})
	t.Run("a direct bearer naming a narrower set", func(t *testing.T) {
		wantScopeAdmitted(t, providerCall(g, submit.method, submit.path, providerDirectBearer(t, priv, "system/Claim.c")))
		var got exchangeRecords
		g.cfg.ExchangeObserved = got.observe
		wantScopeRefused(t, providerCall(g, inquire.method, inquire.path, providerDirectBearer(t, priv, "system/Claim.c")), &got, inquire)
	})
	t.Run("a direct bearer naming a scope it is not registered for", func(t *testing.T) {
		var got exchangeRecords
		g.cfg.ExchangeObserved = got.observe
		wantScopeRefused(t, providerCall(g, submit.method, submit.path, providerDirectBearer(t, priv, ProviderUmbrellaScope)), &got, submit)
	})
	t.Run("an issued bearer from a request naming none", func(t *testing.T) {
		now := ingressFixedClock()()
		claims := validClaims(providerScopeClient, g.ingressAuth.tokenURL(), now)
		routeScopeJTI++
		claims["jti"] = fmt.Sprintf("rs-token-%d", routeScopeJTI)
		rec := postToken(t, g.Handler(), mintAssertion(t, priv, jwt.SigningMethodES384, claims))
		var tr struct {
			AccessToken string `json:"access_token"`
			Scope       string `json:"scope"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &tr) != nil {
			t.Fatalf("token: %d %s", rec.Code, rec.Body)
		}
		if tr.Scope != "system/Claim.c system/Claim.r" {
			t.Fatalf("granted %q, want the registration, system/Claim.c system/Claim.r", tr.Scope)
		}
		wantScopeAdmitted(t, providerCall(g, inquire.method, inquire.path, tr.AccessToken))
	})
	t.Run("an issued bearer from a request naming one", func(t *testing.T) {
		bearer := scopedBearer(t, g, priv, providerScopeClient, "system/Claim.c")
		wantScopeAdmitted(t, providerCall(g, submit.method, submit.path, bearer))
		var got exchangeRecords
		g.cfg.ExchangeObserved = got.observe
		wantScopeRefused(t, providerCall(g, inquire.method, inquire.path, bearer), &got, inquire)
	})
	t.Run("a direct bearer whose scope claim is not a string", func(t *testing.T) {
		now := ingressFixedClock()()
		routeScopeJTI++
		bearer := mintAssertion(t, priv, jwt.SigningMethodES384, jwt.MapClaims{"iss": providerScopeClient, "aud": testIngressBaseURL,
			"jti": fmt.Sprintf("rs-%d", routeScopeJTI), "iat": now.Unix(), "exp": now.Add(time.Minute).Unix(),
			"scope": []string{"system/Claim.c"}})
		var got exchangeRecords
		g.cfg.ExchangeObserved = got.observe
		wantScopeRefused(t, providerCall(g, submit.method, submit.path, bearer), &got, submit)
	})
	t.Run("a registration holding an empty entry and one with a space", func(t *testing.T) {
		h := providerScopeGateway(t, pub, []string{"system/Claim.c", "", "system/Claim.r", "system/Task.c system/Task.r"})
		now := ingressFixedClock()()
		claims := validClaims(providerScopeClient, h.ingressAuth.tokenURL(), now)
		routeScopeJTI++
		claims["jti"] = fmt.Sprintf("rs-token-%d", routeScopeJTI)
		rec := postToken(t, h.Handler(), mintAssertion(t, priv, jwt.SigningMethodES384, claims))
		var tr struct {
			Scope string `json:"scope"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &tr) != nil || tr.Scope != "system/Claim.c system/Claim.r" {
			t.Fatalf("token: %d %s, want the registration's scopes granted", rec.Code, rec.Body)
		}
	})
	t.Run("not authenticated", func(t *testing.T) {
		if rec := providerCall(g, submit.method, submit.path, "a.b.c"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("a forged bearer: %d %s, want 401", rec.Code, rec.Body)
		}
	})
}

// TestPARouteScope_EveryMountedRouteTakesAScope: every route a provider's
// ingress mounts, but the public ones (metadata and configuration documents,
// the token endpoint) and the notification route, whose credential is the
// sealed channel, not a bearer, is in its workstream's scope table
// (routeScopes), and its handler checks that scope: a client registered for
// none of the routes' scopes is refused 403 on it, and one registered for
// exactly the scopes the table's entry names in its challenge is past the
// check. A route added without its scope check, or with another than its
// table's, fails here.
func TestPARouteScope_EveryMountedRouteTakesAScope(t *testing.T) {
	priv, pub := newTestClientKey(t)
	public := []string{
		"GET /metadata", "POST /oauth/token", "GET /.well-known/smart-configuration",
		"GET /pas/payer/{payer}/.well-known/smart-configuration", "GET /.well-known/davinci-configuration",
		"POST /notify/cdex-task/{subscriber}/{kid}/{sealed}",
	}
	tables := map[string]routeScope{}
	for _, w := range workstreams {
		for _, r := range w.routeScopes() {
			if _, twice := tables[r.pattern]; twice {
				t.Fatalf("%s is in the scope tables twice", r.pattern)
			}
			tables[r.pattern] = r.scope
		}
	}
	fill := strings.NewReplacer("{rest...}", "Questionnaire/$next-question", "{payer}", "payer", "{line}", "2.0", "{tag}", "t",
		"{id}", "shn-order-sign", "{subscriber}", "s", "{kid}", "k", "{sealed}", "x")
	// Every route a provider's ingress can mount: its own Subscription to the
	// PAS topic at a payer's base is mounted with PROVIDER_PAS_SUBSCRIPTION.
	all := takesPASNotifications(t)
	none := providerScopeGateway(t, pub, []string{"system/Patient.read"}, all)
	var mounted []mountedRoute
	for _, w := range workstreams {
		mounted = append(mounted, w.routes(none, "provider")...)
	}
	if len(mounted) == 0 {
		t.Fatal("no provider route mounted")
	}
	seen := map[string]bool{}
	for _, mr := range mounted {
		method, pattern, _ := strings.Cut(string(mr.pattern), " ")
		if slices.Contains(public, string(mr.pattern)) || strings.HasPrefix(pattern, "/substrate/") || strings.HasPrefix(pattern, "/scenario/") {
			continue
		}
		seen[string(mr.pattern)] = true
		t.Run(string(mr.pattern), func(t *testing.T) {
			scope, ok := tables[string(mr.pattern)]
			if !ok {
				t.Fatalf("%s is in no workstream's scope table", mr.pattern)
			}
			rec := providerCall(none, method, fill.Replace(pattern), providerDirectBearer(t, priv, ""))
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), string(scope.reason)) {
				t.Fatalf("no route's scope: %d %s, want 403 %q", rec.Code, rec.Body, scope.reason)
			}
			own := providerScopeGateway(t, pub, strings.Split(scope.challenge(), " "), all)
			wantScopeAdmitted(t, providerCall(own, method, fill.Replace(pattern), providerDirectBearer(t, priv, "")))
		})
	}
	for pattern := range tables {
		if !seen[pattern] {
			t.Errorf("the scope table names %s, which the provider's ingress does not mount", pattern)
		}
	}
}

// takesPASNotifications sets a provider gateway's PROVIDER_PAS_SUBSCRIPTION,
// which mounts its own Subscription to the PAS topic at each payer's base and
// takes the topic's notifications from the Hub (so needs its transport key).
func takesPASNotifications(t *testing.T) func(*Config) {
	t.Helper()
	hubPub, _ := genED25519(t)
	return func(c *Config) {
		c.ProviderPASSubscription = true
		c.HubTransportPub = hubPub
	}
}

// TestPARouteScope_AListingTakesAnyScopeOfTheRoutesUnderItsBases: a listing of
// the payers' bases takes any scope that some route under those bases takes,
// by the scope table, and no other.
func TestPARouteScope_AListingTakesAnyScopeOfTheRoutesUnderItsBases(t *testing.T) {
	priv, pub := newTestClientKey(t)
	for _, row := range []struct {
		path, names string
		admitted    []string
		refused     []string
	}{
		{"/pas/payers", "lists the payers' PAS bases", []string{"system/Claim.c", "system/Claim.read", "system/*.rs", ProviderUmbrellaScope},
			[]string{"system/Task.c", "system/DocumentReference.c", "system/Questionnaire.r"}},
		{"/cdex/payers", "lists the payers' attachment addresses", []string{"system/DocumentReference.c", "system/QuestionnaireResponse.write", "system/*.c", ProviderUmbrellaScope},
			[]string{"system/Task.c", "system/Claim.c", "system/DocumentReference.r"}},
	} {
		for _, scope := range row.admitted {
			t.Run(row.path+"/admitted/"+scope, func(t *testing.T) {
				g := providerScopeGateway(t, pub, []string{scope})
				wantScopeAdmitted(t, providerCall(g, http.MethodGet, row.path, providerDirectBearer(t, priv, "")))
			})
		}
		for _, scope := range row.refused {
			t.Run(row.path+"/refused/"+scope, func(t *testing.T) {
				g := providerScopeGateway(t, pub, []string{scope})
				if rec := providerCall(g, http.MethodGet, row.path, providerDirectBearer(t, priv, "")); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), row.names) {
					t.Fatalf("%d %s, want 403 naming %q", rec.Code, rec.Body, row.names)
				}
			})
		}
	}
	// A Subscription scope lists the PAS bases where the provider's own
	// Subscription to the PAS topic at a payer's base is mounted, since those
	// routes take one: a client that manages it discovers the bases there.
	// Each scope that permits one of those interactions is admitted, and the
	// routes under the bases admit it for that interaction. Without the
	// setting no route under the PAS bases takes one, and it is refused.
	for _, row := range []struct {
		scope, method, path string
	}{
		{"system/Subscription.*", http.MethodPost, "/pas/payer/payer/Subscription"},
		{"system/Subscription.c", http.MethodPost, "/pas/payer/payer/Subscription"},
		{"system/Subscription.read", http.MethodGet, "/pas/payer/payer/Subscription/s1"},
		{"system/Subscription.s", http.MethodGet, "/pas/payer/payer/Subscription"},
		{"system/Subscription.u", http.MethodPut, "/pas/payer/payer/Subscription/s1"},
		{"system/Subscription.d", http.MethodDelete, "/pas/payer/payer/Subscription/s1"},
	} {
		t.Run("/pas/payers/admitted/"+row.scope, func(t *testing.T) {
			g := providerScopeGateway(t, pub, []string{row.scope}, takesPASNotifications(t))
			wantScopeAdmitted(t, providerCall(g, http.MethodGet, "/pas/payers", providerDirectBearer(t, priv, "")))
			wantScopeAdmitted(t, providerCall(g, row.method, row.path, providerDirectBearer(t, priv, "")))
		})
	}
	t.Run("/pas/payers/refused/a Subscription scope, without the setting", func(t *testing.T) {
		g := providerScopeGateway(t, pub, []string{"system/Subscription.*"})
		if rec := providerCall(g, http.MethodGet, "/pas/payers", providerDirectBearer(t, priv, "")); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "lists the payers' PAS bases") {
			t.Fatalf("%d %s, want 403 naming the listing", rec.Code, rec.Body)
		}
	})
}

// TestPARouteScope_TheSMARTConfigurationAdvertisesEachRoutesScope pins the
// provider's scopes_supported: the umbrella, then each scope a route's
// challenge names, sorted.
func TestPARouteScope_TheSMARTConfigurationAdvertisesEachRoutesScope(t *testing.T) {
	_, pub := newTestClientKey(t)
	g := providerScopeGateway(t, pub, []string{ProviderUmbrellaScope})
	rec := providerCall(g, http.MethodGet, "/.well-known/smart-configuration", "")
	var doc struct {
		Scopes []string `json:"scopes_supported"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("smart-configuration: %v %s", err, rec.Body)
	}
	want := []string{
		ProviderUmbrellaScope,
		"system/Claim.read", "system/Claim.write", "system/CoverageEligibilityRequest.write",
		"system/DocumentReference.write", "system/Questionnaire.read", "system/QuestionnaireResponse.write",
		"system/Task.write",
	}
	if !slices.Equal(doc.Scopes, want) {
		t.Fatalf("scopes_supported %q, want %q", doc.Scopes, want)
	}
}

// TestPARouteScope_ThePlainBasesSubscriptionRoutesTakeASubscriptionScope: a
// Subscription at the plain base is held to a Subscription scope that permits
// the interaction, as a payer's Subscription routes are; the umbrella does not
// permit one. Past the scope, the plain base names no payer: routing.
func TestPARouteScope_ThePlainBasesSubscriptionRoutesTakeASubscriptionScope(t *testing.T) {
	priv, pub := newTestClientKey(t)
	for _, level := range allLevels {
		t.Run("the umbrella/"+level.String(), func(t *testing.T) {
			var got exchangeRecords
			g := providerScopeGateway(t, pub, []string{ProviderUmbrellaScope}, func(c *Config) {
				c.ConformanceEnforcement = level
				c.ExchangeObserved = got.observe
			})
			rec := providerCall(g, http.MethodPost, "/Subscription", providerDirectBearer(t, priv, ""))
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), string(reasonScopeSubscriptionCreate)) {
				t.Fatalf("answer %d %s, want 403 %q", rec.Code, rec.Body, reasonScopeSubscriptionCreate)
			}
			if want := `Bearer error="insufficient_scope", scope="` + SubscriptionWriteScope + `"`; rec.Header().Get("WWW-Authenticate") != want {
				t.Fatalf("challenge %q, want %q", rec.Header().Get("WWW-Authenticate"), want)
			}
			xr := got.only(t)
			wantRefusal(t, xr, http.StatusForbidden, RefusedByProviderGateway, RefusalAuthority)
		})
	}
	for _, row := range []struct {
		wildcard, method, path string
		status                 int
	}{
		{"system/*.c", http.MethodPost, "/Subscription", http.StatusUnprocessableEntity},
		{"system/*.rs", http.MethodGet, "/Subscription", http.StatusNotFound},
	} {
		t.Run("a wildcard/"+row.wildcard, func(t *testing.T) {
			g := providerScopeGateway(t, pub, []string{row.wildcard})
			if rec := providerCall(g, row.method, row.path, providerDirectBearer(t, priv, "")); rec.Code != row.status {
				t.Fatalf("answer %d %s, want the plain base's %d", rec.Code, rec.Body, row.status)
			}
		})
	}
	// Each Subscription entry in the scope table, at the plain base or a
	// payer's PAS base, needs what its handler asks for (subscriptionNeed),
	// with the same sentence.
	for _, entry := range slices.Concat(paScopedRoutes, paPASSubscriptionScopedRoutes) {
		method, path, _ := strings.Cut(entry.pattern, " ")
		path = strings.TrimPrefix(path, pasPayerBasePattern)
		if !strings.HasPrefix(path, "/Subscription") {
			continue
		}
		rest := strings.TrimPrefix(strings.NewReplacer("{rest...}", "x", "{id}", "x").Replace(path), "/")
		need, reason := subscriptionNeed(method, rest)
		if entry.scope.need != need || entry.scope.reason != reason || !entry.scope.noUmbrella {
			t.Errorf("%s: table needs %v (%q), the handler %v (%q)", entry.pattern, entry.scope.need, entry.scope.reason, need, reason)
		}
	}
	t.Run("a Subscription scope", func(t *testing.T) {
		var got exchangeRecords
		g := providerScopeGateway(t, pub, []string{"system/Subscription.*"}, func(c *Config) { c.ExchangeObserved = got.observe })
		if rec := providerCall(g, http.MethodPost, "/Subscription", providerDirectBearer(t, priv, "")); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("answer %d %s, want the plain base's 422", rec.Code, rec.Body)
		}
		wantRefusal(t, got.only(t), http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalRouting)
	})
}
