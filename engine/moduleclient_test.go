package engine

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	jwt "github.com/golang-jwt/jwt/v5"
)

// The ingress clients of the module-client rows: "ehr", the participant's own
// system, and "module", a registered on-behalf module ingress client.
const (
	nativeClientID = "ehr"
	moduleClientID = "module"
)

// moduleClientEnv is the in-process exchange with real ingress
// authentication: a native client and a module client registered, the test
// bypass off, and every exchange record kept.
type moduleClientEnv struct {
	*inProcessExchange
	keys    map[string]*ecdsa.PrivateKey
	records []ExchangeRecord
}

func newModuleClientEnv(t *testing.T, level ConformanceEnforcement) *moduleClientEnv {
	t.Helper()
	return moduleClientEnvOn(t, newInProcessExchange(t), level)
}

// moduleClientEnvOn is newModuleClientEnv over an exchange a leg's own
// fixture built.
func moduleClientEnvOn(t *testing.T, ex *inProcessExchange, level ConformanceEnforcement) *moduleClientEnv {
	t.Helper()
	env := &moduleClientEnv{inProcessExchange: ex, keys: map[string]*ecdsa.PrivateKey{}}
	clients := map[string]IngressClientRegistration{}
	for id, module := range map[string]string{nativeClientID: "", moduleClientID: ModuleOnBehalf} {
		priv, pub := newTestClientKey(t)
		env.keys[id] = priv
		clients[id] = IngressClientRegistration{Alg: "ES384", PublicKeyPEM: pub, Scopes: []string{ingressScope}, Module: module}
	}
	keys, err := newEphemeralKeyStore()
	if err != nil {
		t.Fatal(err)
	}
	s, err := newIngressAuthServer(testIngressBaseURL, clients, ingressFixedClock(), keys, NewInMemoryReplayStore())
	if err != nil {
		t.Fatal(err)
	}
	g := env.originator
	g.ingressAuth = s
	g.cfg.ingressAuthBypass = false
	g.cfg.ConformanceEnforcement = level
	g.cfg.ExchangeObserved = func(x ExchangeRecord) { env.records = append(env.records, x) }
	return env
}

// as authenticates req as client, with a direct bearer, and declares builder
// in BuilderHeader when it is not "" (send set: the header is sent even when
// empty).
func (env *moduleClientEnv) as(t *testing.T, req *http.Request, client, builder string, send bool) *http.Request {
	t.Helper()
	now := ingressFixedClock()()
	tok := signJWT(t, jwt.SigningMethodES384, env.keys[client], directClaims(client, testIngressBaseURL+req.URL.Path, now))
	req.Header.Set("Authorization", "Bearer "+tok)
	if send || builder != "" {
		req.Header.Set(BuilderHeader, builder)
	}
	return req
}

// call runs req through route as the gateway mounts it.
func (env *moduleClientEnv) call(route string, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	env.originator.ingressRoute(route)(rec, req)
	return rec
}

// last is the exchange record of the last call.
func (env *moduleClientEnv) last(t *testing.T) ExchangeRecord {
	t.Helper()
	if len(env.records) == 0 {
		t.Fatal("no exchange record")
	}
	return env.records[len(env.records)-1]
}

// moduleCRDRequest is a CDS Hooks request a module builds whole: the
// fixture's request without the callback (fhirServer, fhirAuthorization)
// the gateway would otherwise remove (E-01).
func moduleCRDRequest(t *testing.T) []byte {
	t.Helper()
	body := conformantCRDRequest("MBR-COVERED")
	for _, member := range []string{
		`"fhirServer":"https://provider.example/fhir",`,
		`"fhirAuthorization":{"token_type":"Bearer","access_token":"tok"},`,
	} {
		if !bytes.Contains(body, []byte(member)) {
			t.Fatalf("fixture: no %s in the CRD request", member)
		}
		body = bytes.Replace(body, []byte(member), nil, 1)
	}
	return body
}

// moduleLegCase is one ingress leg a module client's request is driven on:
// the exchange its fixture needs, the request, and the payer's answer.
type moduleLegCase struct {
	name, route, builder string
	exchange             func(t *testing.T) *inProcessExchange
	request              func(t *testing.T) *http.Request
	answer               func(t *testing.T) []byte
}

// moduleLegCases are the six provider ingress routes.
func moduleLegCases() []moduleLegCase {
	bundle := func(*testing.T) []byte { return []byte(`{"resourceType":"Bundle"}`) }
	return []moduleLegCase{
		{"crd", RouteCRD, string(relay.BuilderSDKCRDRequest), newInProcessExchange,
			func(t *testing.T) *http.Request { return crdIngressPost(moduleCRDRequest(t)) }, bundle},
		{"dtr", RouteDTR, string(relay.BuilderSDKDTRPackage), func(t *testing.T) *inProcessExchange {
			env := newInProcessExchange(t)
			env.originator.cfg.SoR = newPrefetchSoR().sor()
			declareFramedDTR(t, env, true)
			return env
		}, func(t *testing.T) *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/Questionnaire/$questionnaire-package",
				bytes.NewReader(ehrParams(ehrOrderParam("sr1", prefetchMember), ehrCoverageParam(prefetchMember, "00001"), dtrQuestionnaire)))
			r.Header.Set("Content-Type", "application/fhir+json")
			return r
		}, func(*testing.T) []byte { return packageAnswer }},
		{"pas submit", RoutePAS, string(relay.BuilderSDKPASSubmit), newInProcessExchange, func(t *testing.T) *http.Request {
			return httptest.NewRequest(http.MethodPost, "/Claim/$submit", strings.NewReader(pasIngressBundle("00001", "")))
		}, bundle},
		{"pas inquiry", RoutePASInquire, string(relay.BuilderSDKPASInquiry), newInProcessExchange, func(t *testing.T) *http.Request {
			return httptest.NewRequest(http.MethodPost, "/Claim/$inquire", strings.NewReader(levelInquiry(t, "MBR-COVERED", "")))
		}, bundle},
		{"eligibility", RouteEligibility, string(relay.BuilderSDKEligibility), newInProcessExchange, func(t *testing.T) *http.Request {
			return eligibilityIngressPost(eligibilityRequest(t, "MBR-COVERED"))
		}, bundle},
		{"federated query", RouteFederatedQuery, string(relay.BuilderSDKFederatedQuery),
			func(t *testing.T) *inProcessExchange { return newFQExchange(t, EnforcementObserve) },
			func(t *testing.T) *http.Request { return fqIngressPost(fqTask(t, "MBR-UC05", "facility")) },
			func(t *testing.T) []byte {
				return fqAnswer(t, fqTask(t, "MBR-UC05", "facility"), fqRecords("Patient/MBR-UC05"))
			}},
	}
}

// sentBody is the request body the payer's side received: the frame's body
// for a framed request.
func sentBody(t *testing.T, env *moduleClientEnv) []byte {
	t.Helper()
	sent := env.lastRequestPayload()
	if shnsdk.IsFramed(sent) {
		_, body, err := shnsdk.DecodeHTTPFrame(sent)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	return sent
}

// A registered module ingress client's request is the gateway's own message:
// sealed Authored by the builder it declares, on the leg's originated row, and
// recorded so (ownership authored, the builder named), with the module's bytes
// on the wire exactly as it built them, at every level.
func TestModuleClient_RecordedAuthoredByItsBuilder(t *testing.T) {
	for _, c := range moduleLegCases() {
		for _, level := range allLevels {
			t.Run(c.name+"/"+level.String(), func(t *testing.T) {
				env := moduleClientEnvOn(t, c.exchange(t), level)
				env.payerReturns(LegResult{Response: testResponse(c.answer(t))})
				req := c.request(t)
				body := readBody(t, req)
				rec := env.call(c.route, env.as(t, req, moduleClientID, c.builder, false))
				if env.routeHitCount() != 1 {
					t.Fatalf("answer %d %s: network hits %d, want the request sent once", rec.Code, rec.Body, env.routeHitCount())
				}
				if sent := sentBody(t, env); !bytes.Equal(sent, body) {
					t.Fatalf("the payer's side received\n%s\nwant the module's bytes\n%s", sent, body)
				}
				x := env.last(t)
				if x.Ownership != "authored" || x.Builder != c.builder || len(x.Edits) != 0 {
					t.Fatalf("recorded ownership %q builder %q edits %v, want authored by %s", x.Ownership, x.Builder, x.Edits, c.builder)
				}
				// The request's builder is the request's: the answer is
				// recorded as whose the module received, the payer's carried
				// exactly or, where a level refuses the fixture's answer, the
				// gateway's own refusal. Where nothing judges the answer it is
				// the payer's.
				wantAnswer, wantBuilder := "relayed", ""
				if rec.Code >= http.StatusBadRequest {
					wantAnswer, wantBuilder = "authored", string(relay.BuilderGatewayRefusal)
				}
				if (level == EnforcementNone || level == EnforcementObserve) && wantAnswer != "relayed" {
					t.Fatalf("answer %d %s at %s: the payer's answer was not carried", rec.Code, rec.Body, level)
				}
				if x.AnswerOwnership != wantAnswer || x.AnswerBuilder != wantBuilder || len(x.RequestEdits)+len(x.AnswerEdits) != 0 {
					t.Fatalf("answer %d: recorded answer %q builder %q, request edits %v answer edits %v, want %s %q and no edits",
						rec.Code, x.AnswerOwnership, x.AnswerBuilder, x.RequestEdits, x.AnswerEdits, wantAnswer, wantBuilder)
				}
			})
		}
	}
}

// The participant's own system's request is unchanged: carried, recorded as
// its own (relayed, or edited with its edits), with no builder.
func TestModuleClient_NativeRequestStaysTheParticipants(t *testing.T) {
	env := newModuleClientEnv(t, EnforcementObserve)
	env.payerReturns(LegResult{Response: testResponse([]byte(`{"resourceType":"Bundle"}`))})
	req := httptest.NewRequest(http.MethodPost, "/Claim/$submit", strings.NewReader(pasIngressBundle("00001", "")))
	env.call(RoutePAS, env.as(t, req, nativeClientID, "", false))
	if x := env.last(t); x.Ownership != "relayed" || x.Builder != "" {
		t.Fatalf("native submit recorded ownership %q builder %q, want relayed with no builder", x.Ownership, x.Builder)
	}
	env.call(RouteCRD, env.as(t, env.crdIngressRequest(t), nativeClientID, "", false))
	if x := env.last(t); x.Ownership != "edited" || x.Builder != "" || !slices.Contains(x.Edits, string(relay.EditCDSCallbackStrip)) ||
		!slices.Contains(x.RequestEdits, string(relay.EditCDSCallbackStrip)) || len(x.AnswerEdits) != 0 {
		t.Fatalf("native CRD recorded ownership %q builder %q edits %v (request %v, answer %v), want edited (E-01) with no builder, E-01 the request's",
			x.Ownership, x.Builder, x.Edits, x.RequestEdits, x.AnswerEdits)
	}
}

// REJECTION: each builder declaration the gateway cannot vouch for is refused
// at every level, as the fidelity of SHN's own edits (422), and nothing is
// sent: a native client naming a builder, a module client naming none, a
// builder that is not registered, one its module does not hold, one that does
// not build this leg's request, a module request the gateway would have to
// edit, and any declaration under the test bypass, which verifies no client.
func TestModuleClient_RefusesWhatItCannotVouchFor(t *testing.T) {
	pas := func(t *testing.T) *http.Request {
		return httptest.NewRequest(http.MethodPost, "/Claim/$submit", strings.NewReader(pasIngressBundle("00001", "")))
	}
	rows := []struct {
		name, route string
		request     func(t *testing.T) *http.Request
		client      string
		builder     string
		send        bool
		bypass      bool
		want        string
	}{
		{"a native client names a builder", RoutePAS, pas, nativeClientID, string(relay.BuilderSDKPASSubmit), false, false, "a native client must not send " + BuilderHeader},
		{"a native client sends an empty header", RoutePAS, pas, nativeClientID, "", true, false, "a native client must not send " + BuilderHeader},
		{"a module client names none", RoutePAS, pas, moduleClientID, "", false, false, "must name its builder in " + BuilderHeader},
		{"an unregistered builder", RoutePAS, pas, moduleClientID, "made-up-builder", false, false, "not a builder of module " + ModuleOnBehalf},
		{"a registered builder not the module's", RoutePAS, pas, moduleClientID, string(relay.BuilderCDexRecords), false, false, "not a builder of module " + ModuleOnBehalf},
		{"a builder for another leg", RoutePAS, pas, moduleClientID, string(relay.BuilderSDKPASInquiry), false, false, "does not build a pas-claim request"},
		{"a module CDS Hooks request carrying the callback", RouteCRD, func(t *testing.T) *http.Request { return crdIngressPost(conformantCRDRequest("MBR-COVERED")) },
			moduleClientID, string(relay.BuilderSDKCRDRequest), false, false, "carries no fhirServer or fhirAuthorization, which this gateway would remove (E-01)"},
		{"a module client repeats the header", RoutePAS, func(t *testing.T) *http.Request {
			r := pas(t)
			r.Header.Add(BuilderHeader, string(relay.BuilderSDKPASSubmit))
			r.Header.Add(BuilderHeader, string(relay.BuilderSDKPASSubmit))
			return r
		}, moduleClientID, "", false, false, BuilderHeader + " is repeated"},
		{"a module declaration under the test bypass", RoutePAS, pas, moduleClientID, string(relay.BuilderSDKPASSubmit), false, true, "a native client must not send " + BuilderHeader},
	}
	for _, row := range rows {
		for _, level := range allLevels {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				env := newModuleClientEnv(t, level)
				req := env.as(t, row.request(t), row.client, row.builder, row.send)
				if row.bypass {
					env.originator.cfg.ingressAuthBypass = true
				}
				rec := env.call(row.route, req)
				if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), row.want) {
					t.Fatalf("answer %d %s, want 422 naming %q", rec.Code, rec.Body, row.want)
				}
				if env.routeHitCount() != 0 {
					t.Fatal("a refused request crossed the network")
				}
				if x := env.last(t); x.Outcome != ExchangeRefused || x.Rule != RefusalFidelity || x.Ownership != "" {
					t.Fatalf("recorded %s rule %q ownership %q, want refused on fidelity, nothing sent", x.Outcome, x.Rule, x.Ownership)
				}
			})
		}
	}
}

// REJECTION, on every ingress route: a native client naming a builder, and a
// module client naming none, are refused there, at every level, with nothing
// sent: the check sits in each route's handler, not only in the helper.
func TestModuleClient_EveryRouteChecksTheDeclaration(t *testing.T) {
	for _, c := range moduleLegCases() {
		for _, row := range []struct {
			name, client, builder, want string
		}{
			{"a native client names a builder", nativeClientID, c.builder, "a native client must not send " + BuilderHeader},
			{"a module client names none", moduleClientID, "", "must name its builder in " + BuilderHeader},
		} {
			for _, level := range allLevels {
				t.Run(c.name+"/"+row.name+"/"+level.String(), func(t *testing.T) {
					env := moduleClientEnvOn(t, c.exchange(t), level)
					rec := env.call(c.route, env.as(t, c.request(t), row.client, row.builder, false))
					if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), row.want) {
						t.Fatalf("answer %d %s, want 422 naming %q", rec.Code, rec.Body, row.want)
					}
					if env.routeHitCount() != 0 {
						t.Fatal("a refused request crossed the network")
					}
					if x := env.last(t); x.Outcome != ExchangeRefused || x.Rule != RefusalFidelity || x.Ownership != "" {
						t.Fatalf("recorded %s rule %q ownership %q, want refused on fidelity, nothing sent", x.Outcome, x.Rule, x.Ownership)
					}
				})
			}
		}
	}
}

// A module client that authenticates the SMART Backend Services way, with a
// bearer this gateway issued from its token endpoint, is identified by that
// bearer's client: its request is recorded authored, and the same request
// from the native client's issued bearer naming a builder is refused.
func TestModuleClient_IssuedBearerIdentifiesTheModule(t *testing.T) {
	env := newModuleClientEnv(t, EnforcementObserve)
	env.payerReturns(LegResult{Response: testResponse([]byte(`{"resourceType":"Bundle"}`))})
	issued := func(client string) string {
		now := ingressFixedClock()()
		form := url.Values{
			"grant_type":            {"client_credentials"},
			"client_assertion_type": {assertionType},
			"client_assertion":      {mintAssertion(t, env.keys[client], jwt.SigningMethodES384, validClaims(client, env.originator.ingressAuth.tokenURL(), now))},
		}
		req := httptest.NewRequest(http.MethodPost, testIngressBaseURL+"/oauth/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		env.originator.ingressAuth.handleToken(rec, req)
		var tr struct {
			AccessToken string `json:"access_token"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &tr) != nil || tr.AccessToken == "" {
			t.Fatalf("token for %s: %d %s", client, rec.Code, rec.Body)
		}
		return tr.AccessToken
	}
	submit := func(client string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/Claim/$submit", strings.NewReader(pasIngressBundle("00001", "")))
		req.Header.Set("Authorization", "Bearer "+issued(client))
		req.Header.Set(BuilderHeader, string(relay.BuilderSDKPASSubmit))
		return env.call(RoutePAS, req)
	}
	if rec := submit(moduleClientID); env.routeHitCount() != 1 {
		t.Fatalf("module submit with an issued bearer: %d %s", rec.Code, rec.Body)
	}
	if x := env.last(t); x.Ownership != "authored" || x.Builder != string(relay.BuilderSDKPASSubmit) {
		t.Fatalf("recorded ownership %q builder %q, want authored by %s", x.Ownership, x.Builder, relay.BuilderSDKPASSubmit)
	}
	if rec := submit(nativeClientID); rec.Code != http.StatusUnprocessableEntity || env.routeHitCount() != 1 {
		t.Fatalf("native submit naming a builder with an issued bearer: %d %s (hits %d)", rec.Code, rec.Body, env.routeHitCount())
	}
}

// REJECTION: a module request the gateway would have to edit is refused, at
// every level, with nothing sent: a questionnaire-package request without the
// coverage enrichment would obtain from the system of record (E-04).
func TestModuleClient_RefusesARequestTheGatewayWouldEdit(t *testing.T) {
	ownPatient := `{"name":"referenced","resource":{"resourceType":"Patient","id":"example"}}`
	noCoverage := ehrParams(ehrOrderParam("sr1", prefetchMember), dtrQuestionnaire, `{"name":"context","valueString":"ctx-1"}`, ownPatient)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			s := newPrefetchSoR()
			s.answer(t, "Coverage", page("", "", sorEntry(sorCoverage("cov-1", shnsdk.CMSPayerIdentity.Value)),
				includeEntry(`{"resourceType":"Organization","id":"org-1","name":"Payer"}`)))
			ex := newInProcessExchange(t)
			optInEnrichment(ex.originator, true)
			ex.originator.cfg.SoR = s.sor()
			declareFramedDTR(t, ex, true)
			env := moduleClientEnvOn(t, ex, level)
			req := httptest.NewRequest(http.MethodPost, "/Questionnaire/$questionnaire-package", bytes.NewReader(noCoverage))
			req.Header.Set("Content-Type", "application/fhir+json")
			rec := env.call(RouteDTR, env.as(t, req, moduleClientID, string(relay.BuilderSDKDTRPackage), false))
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "this gateway would have to edit it (E-04") {
				t.Fatalf("answer %d %s, want 422 naming E-04", rec.Code, rec.Body)
			}
			if env.routeHitCount() != 0 {
				t.Fatal("a refused request crossed the network")
			}
			if x := env.last(t); x.Outcome != ExchangeRefused || x.Rule != RefusalFidelity || x.Ownership != "" {
				t.Fatalf("recorded %s rule %q ownership %q", x.Outcome, x.Rule, x.Ownership)
			}
		})
	}
}

// A module CDS Hooks request carrying the callback is refused before anything
// is read through it: for a member the system of record does not hold, a
// native request with the same callback reads the coverage at its fhirServer
// (the control), and the module's reads nothing there.
func TestModuleClient_CallbackRefusedBeforeAnyRead(t *testing.T) {
	for _, client := range []string{nativeClientID, moduleClientID} {
		t.Run(client, func(t *testing.T) {
			e := newEHRServer(t, ehrAnswer(200, "application/fhir+json", coverageSearchset(strangerMember, "00001")))
			ex, _ := fhirServerEnv(t, e, "")
			env := moduleClientEnvOn(t, ex, EnforcementObserve)
			env.payerReturns(LegResult{Response: testResponse([]byte(`{"cards":[]}`))})
			builder := ""
			if client == moduleClientID {
				builder = string(relay.BuilderSDKCRDRequest)
			}
			rec := env.call(RouteCRD, env.as(t, crdIngressPost(fhirServerRequest(e.base)), client, builder, false))
			if client == nativeClientID {
				if e.calls.Load() != 1 {
					t.Fatalf("control: the native request read its fhirServer %d times, want 1 (answer %d %s)", e.calls.Load(), rec.Code, rec.Body)
				}
				return
			}
			if rec.Code != http.StatusUnprocessableEntity || e.calls.Load() != 0 || env.routeHitCount() != 0 {
				t.Fatalf("module request: answer %d %s, fhirServer reads %d, network hits %d; want 422 with nothing read or sent",
					rec.Code, rec.Body, e.calls.Load(), env.routeHitCount())
			}
		})
	}
}

// A module body that cannot be read gets the read's own refusal (400), not a
// fidelity refusal it would not deserve.
func TestModuleClient_UnreadableBodyIsTheReadsRefusal(t *testing.T) {
	env := newModuleClientEnv(t, EnforcementObserve)
	rec := env.call(RouteCRD, env.as(t, crdIngressPost([]byte(`{"hook":"order-select","hook":"order-select"}`)), moduleClientID, string(relay.BuilderSDKCRDRequest), false))
	if rec.Code != http.StatusBadRequest || env.routeHitCount() != 0 {
		t.Fatalf("answer %d %s, want the 400 read refusal", rec.Code, rec.Body)
	}
}

// The declaration check over every ingress leg: a module client may declare
// exactly the builder that builds that leg's request, and a native client
// declares nothing.
func TestModuleClient_BuilderPermittedPerLeg(t *testing.T) {
	env := newModuleClientEnv(t, EnforcementObserve)
	legs := map[string]relay.BuilderID{
		"crd-order-select":        relay.BuilderSDKCRDRequest,
		"crd-order-dispatch":      relay.BuilderSDKCRDRequest,
		"dtr-questionnaire-fetch": relay.BuilderSDKDTRPackage,
		"pas-claim":               relay.BuilderSDKPASSubmit,
		"pas-claim-update":        relay.BuilderSDKPASUpdate,
		"pas-claim-inquire":       relay.BuilderSDKPASInquiry,
		"coverage-eligibility":    relay.BuilderSDKEligibility,
		"federated-query":         relay.BuilderSDKFederatedQuery,
	}
	for leg, want := range legs {
		for _, b := range ModuleBuilders(ModuleOnBehalf) {
			req := httptest.NewRequest(http.MethodPost, "/x", nil)
			req = req.WithContext(withCaller(req, moduleClientID))
			req.Header.Set(BuilderHeader, string(b))
			got, ok := env.originator.ingressBuilder(httptest.NewRecorder(), req, leg)
			if ok != (b == want) || (ok && got != want) {
				t.Errorf("%s declaring %s: %q, %v; want permitted only for %s", leg, b, got, ok, want)
			}
		}
		req := httptest.NewRequest(http.MethodPost, "/x", nil)
		req = req.WithContext(withCaller(req, nativeClientID))
		if got, ok := env.originator.ingressBuilder(httptest.NewRecorder(), req, leg); !ok || got != "" {
			t.Errorf("%s from a native client with no header: %q, %v; want carried as the participant's", leg, got, ok)
		}
	}
}

// withCaller is req's context carrying client as the verified caller.
func withCaller(req *http.Request, client string) context.Context {
	var got context.Context
	withIngressCaller(func(_ http.ResponseWriter, r *http.Request) {
		noteIngressCaller(r.Context(), client)
		got = r.Context()
	})(httptest.NewRecorder(), req)
	return got
}

// REJECTION, at boot: a client marked as a module this build does not
// compile in refuses the ingress.
func TestModuleClient_UnknownModuleRefusesBoot(t *testing.T) {
	_, pub := newTestClientKey(t)
	keys, err := newEphemeralKeyStore()
	if err != nil {
		t.Fatal(err)
	}
	_, err = newIngressAuthServer(testIngressBaseURL, map[string]IngressClientRegistration{
		"c": {Alg: "ES384", PublicKeyPEM: pub, Scopes: []string{ingressScope}, Module: "made-up-module"},
	}, ingressFixedClock(), keys, NewInMemoryReplayStore())
	if err == nil || !strings.Contains(err.Error(), `unknown module "made-up-module"`) {
		t.Fatalf("err = %v, want the unknown-module refusal", err)
	}
}

// The closed sets the record and the declaration draw from, pinned: a value
// added to either fails here until it is reviewed.
func TestModuleClient_ClosedSetsPinned(t *testing.T) {
	if !slices.Equal(ExchangeOwnerships, []string{"relayed", "edited", "authored"}) {
		t.Fatalf("ExchangeOwnerships %v", ExchangeOwnerships)
	}
	if !slices.Equal(ModuleIDs(), []string{ModuleOnBehalf}) {
		t.Fatalf("ModuleIDs %v", ModuleIDs())
	}
	want := []relay.BuilderID{relay.BuilderSDKCRDRequest, relay.BuilderSDKDTRPackage, relay.BuilderSDKPASSubmit,
		relay.BuilderSDKPASUpdate, relay.BuilderSDKPASInquiry, relay.BuilderSDKEligibility, relay.BuilderSDKFederatedQuery}
	if got := ModuleBuilders(ModuleOnBehalf); !slices.Equal(got, want) {
		t.Fatalf("ModuleBuilders(%s) %v, want %v", ModuleOnBehalf, got, want)
	}
	for _, b := range ModuleBuilders(ModuleOnBehalf) {
		if !slices.Contains(relay.Builders(), b) {
			t.Fatalf("module builder %s is not registered", b)
		}
	}
	// A record names only an ownership of the closed list and a registered
	// builder: an unregistered builder is never written.
	x := &exchangeRecorder{}
	x.sent(relay.ForTest([]byte(`{}`), "application/json"))
	if x.rec.Builder != "" || !slices.Contains(ExchangeOwnerships, x.rec.Ownership) {
		t.Fatalf("a test-injected payload recorded ownership %q builder %q", x.rec.Ownership, x.rec.Builder)
	}
}

// readBody reads req's body and restores it.
func readBody(t *testing.T, req *http.Request) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(req.Body); err != nil {
		t.Fatal(err)
	}
	req.Body = io.NopCloser(bytes.NewReader(buf.Bytes()))
	return buf.Bytes()
}
