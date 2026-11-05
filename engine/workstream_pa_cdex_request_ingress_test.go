//go:build !shn_coreonly

package engine

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/pa/cdexroute"
	"github.com/SmartHealthNetwork/shn-gateway/pa/cdextask"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// cdexRequestTask is the CDex 2.1.0 IG's own Task Attachment Request by code
// (Task-cdex-task-example19.json), byte for byte: a contained Patient with a
// member number, a contained PractitionerRole owner with NPIs, a requester
// identifier and a payer-url.
func cdexRequestTask(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../pa/cdextask/testdata/cdex-task-example19.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// cdexRequestEdit is example 19 with one replacement, which must apply once.
func cdexRequestEdit(t *testing.T, old, new string) []byte {
	t.Helper()
	b := cdexRequestTask(t)
	if bytes.Count(b, []byte(old)) != 1 {
		t.Fatalf("fixture: %s appears %d times, want once", old, bytes.Count(b, []byte(old)))
	}
	return bytes.Replace(b, []byte(old), []byte(new), 1)
}

// cdexRequestMutate is example 19 decoded, edited and encoded again.
func cdexRequestMutate(t *testing.T, edit func(task map[string]any)) []byte {
	t.Helper()
	var task map[string]any
	if err := json.Unmarshal(cdexRequestTask(t), &task); err != nil {
		t.Fatal(err)
	}
	edit(task)
	b, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The leg takes a Task coded, by system and code, as a request for attachments
// by code or for a questionnaire, and no other: not the CDex data requests,
// not a taken code under another system, not a Task with no code.
func TestCDexRequestCodeTaken(t *testing.T) {
	for name, row := range map[string]struct {
		codings []cdextask.Coding
		taken   bool
	}{
		"by code":                          {[]cdextask.Coding{{System: cdextask.PASTempCodes, Code: cdextask.CodeAttachmentRequest}}, true},
		"by questionnaire":                 {[]cdextask.Coding{{System: cdextask.PASTempCodes, Code: cdextask.CodeAttachmentRequestQuestionnaire}}, true},
		"a data request by code":           {[]cdextask.Coding{{System: cdexTempCodes, Code: "data-request-code"}}, false},
		"a data request by query":          {[]cdextask.Coding{{System: cdexTempCodes, Code: "data-request-query"}}, false},
		"a data request by questionnaire":  {[]cdextask.Coding{{System: cdexTempCodes, Code: "data-request-questionnaire"}}, false},
		"a taken code, another system":     {[]cdextask.Coding{{System: cdexTempCodes, Code: cdextask.CodeAttachmentRequestQuestionnaire}}, false},
		"the by-code code, another system": {[]cdextask.Coding{{System: cdexTempCodes, Code: cdextask.CodeAttachmentRequest}}, false},
		"another PAS code":                 {[]cdextask.Coding{{System: cdextask.PASTempCodes, Code: "data-request-code"}}, false},
		"no code":                          {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			task := cdextask.Task{}
			if row.codings != nil {
				task.Code = cdextask.Codes{State: cdextask.Present, Codings: row.codings}
			}
			if got := cdexRequestCodeTaken(task); got != row.taken {
				t.Fatalf("taken %v, want %v", got, row.taken)
			}
		})
	}
}

// cdexTempCodes is the CDex code system the IG's data-request Tasks are coded in.
const cdexTempCodes = "http://hl7.org/fhir/us/davinci-cdex/CodeSystem/cdex-temp"

// cdexDataRequest is example 19 recoded as system#code, as a CDex data
// request is: Task.code replaced, everything else as the IG wrote it.
func cdexDataRequest(system, code string) func(t *testing.T) []byte {
	return func(t *testing.T) []byte {
		return cdexRequestMutate(t, func(m map[string]any) {
			m["code"] = map[string]any{"coding": []any{map[string]any{"system": system, "code": code}}}
		})
	}
}

// cdexQuestionnaireTasks are the CDex 2.1.0 IG's own questionnaire-request
// Tasks, byte for byte as published (D128):
//   - Task-cdex-task-inline-example22.json, the Task the IG's questionnaire
//     page has the payer POST (a static questionnaire;
//     https://hl7.org/fhir/us/davinci-cdex/STU2.1/Task-cdex-task-inline-example22.json);
//   - Task-cdex-task-example23.json, an adaptive questionnaire with a
//     signature-flag input
//     (https://hl7.org/fhir/us/davinci-cdex/STU2.1/Task-cdex-task-example23.json).
var cdexQuestionnaireTasks = []string{"cdex-task-inline-example22.json", "cdex-task-example23.json"}

// cdexFixture is one of the IG's Tasks in gateway/pa/cdextask/testdata, as published.
func cdexFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("../pa/cdextask/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// cdexRequestPost is the payer system's POST of body to provider's base.
func cdexRequestPost(body []byte, provider string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, cdexroute.ProviderPrefix+provider+"/Task", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/fhir+json")
	req.SetPathValue("provider", provider)
	return req
}

// payerIngressExchange is the in-process exchange with its originating
// gateway acting as a payer's: what its own ingress refuses is the payer
// gateway's.
func payerIngressExchange(t *testing.T, level ConformanceEnforcement) (*inProcessExchange, *exchangeRecords) {
	t.Helper()
	env := newInProcessExchange(t)
	env.originator.cfg.Role = "payer"
	env.originator.cfg.ConformanceEnforcement = level
	var got exchangeRecords
	env.originator.cfg.ExchangeObserved = got.observe
	return env, &got
}

// The payer's attachment-request Task is refused only on the network's own
// primitives, at every level, by a payer's gateway behind its own token
// endpoint, and each refusal is the payer gateway's: a body that reads two
// ways (integrity), a Task outside the leg's scope (authority, 403), a base
// that names no registered provider (routing, 422), and a Task it cannot bind
// to a patient (authority, 422). Nothing is sent.
func TestCDexRequestIngress_RefusesOnlyOnTheNetworksPrimitives(t *testing.T) {
	priv, pub := newTestClientKey(t)
	for name, row := range map[string]struct {
		body     func(t *testing.T) []byte
		provider string
		status   int
		msg      string
		rule     string
	}{
		"a base naming no participant": {cdexRequestTask, "nobody", http.StatusUnprocessableEntity, errCDexRequestNoProvider, RefusalRouting},
		// payer-2 is registered, but not as a provider.
		"a base naming a payer": {cdexRequestTask, "payer-2", http.StatusUnprocessableEntity, errCDexRequestNoProvider, RefusalRouting},
		"a repeated member name": {func(t *testing.T) []byte {
			return cdexRequestEdit(t, `"status" : "requested"`, `"status" : "requested", "status" : "draft"`)
		}, "provider-1", http.StatusBadRequest, errCDexRequestTwoWays, RefusalIntegrity},
		"a member named in another case": {func(t *testing.T) []byte {
			return cdexRequestEdit(t, `"valueUrl" :`, `"ValueUrl" :`)
		}, "provider-1", http.StatusBadRequest, errCDexRequestTwoWays, RefusalIntegrity},
		"not JSON":   {func(*testing.T) []byte { return []byte("not json") }, "provider-1", http.StatusForbidden, errCDexRequestNotTask, RefusalAuthority},
		"not a Task": {func(*testing.T) []byte { return []byte(`{"resourceType":"Parameters"}`) }, "provider-1", http.StatusForbidden, errCDexRequestNotTask, RefusalAuthority},
		"no code":    {func(t *testing.T) []byte { return cdexRequestEdit(t, `"code" : {`, `"codeX" : {`) }, "provider-1", http.StatusForbidden, errCDexRequestCode, RefusalAuthority},
		// The CDex data-request Tasks (the IG's cdex-temp codes) are outside
		// the leg's authority (the design, §12), whatever else the Task says.
		"a data request by code":          {cdexDataRequest(cdexTempCodes, "data-request-code"), "provider-1", http.StatusForbidden, errCDexRequestCode, RefusalAuthority},
		"a data request by query":         {cdexDataRequest(cdexTempCodes, "data-request-query"), "provider-1", http.StatusForbidden, errCDexRequestCode, RefusalAuthority},
		"a data request by questionnaire": {cdexDataRequest(cdexTempCodes, "data-request-questionnaire"), "provider-1", http.StatusForbidden, errCDexRequestCode, RefusalAuthority},
		// A PAS code the leg does not take: the system alone is not enough.
		"another PAS code": {cdexDataRequest(cdextask.PASTempCodes, "data-request-code"), "provider-1", http.StatusForbidden, errCDexRequestCode, RefusalAuthority},
		// A taken code under another system is not that code.
		"the questionnaire code under another system": {cdexDataRequest(cdexTempCodes, cdextask.CodeAttachmentRequestQuestionnaire), "provider-1", http.StatusForbidden, errCDexRequestCode, RefusalAuthority},
		"the code under another system": {func(t *testing.T) []byte {
			return cdexRequestMutate(t, func(m map[string]any) {
				m["code"] = map[string]any{"coding": []any{map[string]any{"system": "http://hl7.org/fhir/us/davinci-cdex/CodeSystem/cdex-temp", "code": cdextask.CodeAttachmentRequest}}}
			})
		}, "provider-1", http.StatusForbidden, errCDexRequestCode, RefusalAuthority},
		"a patient that is not contained": {func(t *testing.T) []byte {
			return cdexRequestEdit(t, `"reference" : "#patient"`, `"reference" : "Patient/123"`)
		}, "provider-1", http.StatusUnprocessableEntity, errCDexRequestNoPatient, RefusalAuthority},
		"a contained patient with no member number": {func(t *testing.T) []byte {
			return cdexRequestEdit(t, `"code" : "MB"`, `"code" : "MR"`)
		}, "provider-1", http.StatusUnprocessableEntity, errCDexRequestNoPatient, RefusalAuthority},
		"two member numbers that differ": {func(t *testing.T) []byte {
			return cdexRequestMutate(t, func(m map[string]any) {
				for _, c := range m["contained"].([]any) {
					if p := c.(map[string]any); p["resourceType"] == "Patient" {
						p["identifier"] = append(p["identifier"].([]any), map[string]any{
							"type":  map[string]any{"coding": []any{map[string]any{"system": v20203MB.System, "code": "MB"}}},
							"value": "Member999",
						})
					}
				}
			})
		}, "provider-1", http.StatusUnprocessableEntity, errCDexRequestNoPatient, RefusalAuthority},
		"a member number on the reference that differs from the contained one": {func(t *testing.T) []byte {
			return cdexRequestMutate(t, func(m map[string]any) {
				m["for"] = map[string]any{"reference": "#patient", "identifier": map[string]any{
					"type": map[string]any{"coding": []any{map[string]any{"system": v20203MB.System, "code": "MB"}}}, "value": "Member999"}}
			})
		}, "provider-1", http.StatusUnprocessableEntity, errCDexRequestNoPatient, RefusalAuthority},
		// The member number names no Patient of the member's own, only
		// another person a contained Coverage names as its subscriber: it is
		// shared, so it identifies no one patient.
		"a member number shared with a coverage's subscriber": {func(t *testing.T) []byte {
			return cdexRequestMutate(t, func(m map[string]any) {
				m["for"] = map[string]any{"identifier": map[string]any{
					"type": map[string]any{"coding": []any{map[string]any{"system": v20203MB.System, "code": "MB"}}}, "value": "Member123"}}
				var keep []any
				for _, c := range m["contained"].([]any) {
					if c.(map[string]any)["resourceType"] != "Patient" {
						keep = append(keep, c)
					}
				}
				m["contained"] = append(keep, map[string]any{"resourceType": "Coverage", "id": "cov", "status": "active",
					"subscriber": map[string]any{"reference": "#sub"}, "beneficiary": map[string]any{"reference": "Patient/other"},
					"contained": []any{map[string]any{"resourceType": "Patient", "id": "sub",
						"identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": "Member123"}}}}})
			})
		}, "provider-1", http.StatusUnprocessableEntity, msgPatientUnidentifiable, RefusalAuthority},
		// The reference's identifier cannot be read (its system is not a
		// string): it could name another member, so the Task names no one.
		"an unreadable identifier on the reference": {func(t *testing.T) []byte {
			return cdexRequestMutate(t, func(m map[string]any) {
				m["for"] = map[string]any{"reference": "#patient", "identifier": map[string]any{
					"type": map[string]any{"coding": []any{map[string]any{"system": v20203MB.System, "code": "MB"}}}, "value": "M-OTHER", "system": 42}}
			})
		}, "provider-1", http.StatusUnprocessableEntity, errCDexRequestNoPatient, RefusalAuthority},
		"no patient": {func(t *testing.T) []byte { return cdexRequestEdit(t, `"for" : {`, `"forX" : {`) }, "provider-1", http.StatusUnprocessableEntity, errCDexRequestNoPatient, RefusalAuthority},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				var got exchangeRecords
				hub, hits := hubHits(t)
				g := payerGatewayWithAuth(t, "payer-system", pub, func(c *Config) {
					c.ConformanceEnforcement = level
					c.ExchangeObserved = got.observe
					c.HubURL = hub
					c.AuthzURL = hub
				})
				rec := payerPost(g, http.MethodPost, providerBasePath(row.provider), scopedBearer(t, g, priv, "payer-system", TaskWriteScope), row.body(t))
				if rec.Code != row.status || !strings.Contains(rec.Body.String(), row.msg) {
					t.Fatalf("answer %d %s, want %d %q", rec.Code, rec.Body, row.status, row.msg)
				}
				if n := hits.Load(); n != 0 {
					t.Fatalf("the refused Task reached the network (%d calls)", n)
				}
				x := got.only(t)
				wantRefusal(t, x, row.status, RefusedByPayerGateway, row.rule)
				wantReasonInAnswer(t, x, rec.Body.String())
				if x.Exchange != legCDexRequest {
					t.Fatalf("refusal recorded as exchange %q, want %s", x.Exchange, legCDexRequest)
				}
				if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "fhir+json") || !strings.Contains(rec.Body.String(), "OperationOutcome") {
					t.Fatalf("refusal %s %s, want an OperationOutcome", ct, rec.Body)
				}
			})
		}
	}
}

// The patient is bound by the contained Patient's member number: one MB
// identifier, or several that agree; any other identifier is not read as the
// member.
func TestCDexTaskMember(t *testing.T) {
	mb := []cdextask.Coding{v20203MB}
	for name, row := range map[string]struct {
		patient cdextask.Patient
		member  string
		ok      bool
	}{
		"one member number": {cdextask.Patient{State: cdextask.Present, Contained: true, Identifiers: []cdextask.Identifier{{Value: "M1", Type: mb}}}, "M1", true},
		"beside another identifier": {cdextask.Patient{State: cdextask.Present, Contained: true, Identifiers: []cdextask.Identifier{
			{Value: "MRN-9", Type: []cdextask.Coding{{System: v20203MB.System, Code: "MR"}}}, {Value: "M1", Type: mb}}}, "M1", true},
		"two that agree":  {cdextask.Patient{State: cdextask.Present, Contained: true, Identifiers: []cdextask.Identifier{{Value: "M1", Type: mb}, {Value: "M1", Type: mb}}}, "M1", true},
		"two that differ": {cdextask.Patient{State: cdextask.Present, Contained: true, Identifiers: []cdextask.Identifier{{Value: "M1", Type: mb}, {Value: "M2", Type: mb}}}, "", false},
		"no type":         {cdextask.Patient{State: cdextask.Present, Contained: true, Identifiers: []cdextask.Identifier{{Value: "M1"}}}, "", false},
		"MB under another system": {cdextask.Patient{State: cdextask.Present, Contained: true, Identifiers: []cdextask.Identifier{
			{Value: "M1", Type: []cdextask.Coding{{System: "urn:x", Code: "MB"}}}}}, "", false},
		"not contained": {cdextask.Patient{State: cdextask.Present, Reference: "Patient/1"}, "", false},
		"on the reference alone": {cdextask.Patient{State: cdextask.Present, Reference: "Patient/1",
			Identifier: cdextask.IdentifierField{State: cdextask.Present, Identifier: cdextask.Identifier{Value: "M1", Type: mb}}}, "M1", true},
		"an unreadable identifier on the reference": {cdextask.Patient{State: cdextask.Present, Contained: true,
			Identifiers: []cdextask.Identifier{{Value: "M1", Type: mb}}, Identifier: cdextask.IdentifierField{State: cdextask.Unreadable}}, "", false},
		"unreadable": {cdextask.Patient{State: cdextask.Unreadable}, "", false},
		"absent":     {cdextask.Patient{State: cdextask.Absent}, "", false},
	} {
		if member, ok := cdexTaskMember(cdextask.Task{Patient: row.patient}); member != row.member || ok != row.ok {
			t.Errorf("%s: %q %v, want %q %v", name, member, ok, row.member, row.ok)
		}
	}
}

// payerGatewayWithAuth is a payer's gateway with its ingress on, behind its
// own token endpoint, and clientID registered for both ingress scopes. Its
// registry holds a provider, provider-1, and another payer, payer-2.
func payerGatewayWithAuth(t *testing.T, clientID string, pubPEM []byte, opts ...func(*Config)) *Gateway {
	t.Helper()
	_, signPriv := genED25519(t)
	hubPub, _ := genED25519(t)
	sor := newCensusSoR()
	reg := shnsdk.NewRegistry()
	providerEnc, _ := genKeyPair(t)
	reg.Set("provider-1", shnsdk.RegistryEntry{ID: "provider-1", Role: "provider", EncPub: providerEnc, Legs: []string{legCDexRequest}})
	reg.Set("payer-2", shnsdk.RegistryEntry{ID: "payer-2", Role: "payer"})
	cfg := Config{
		Role:           "payer",
		HolderID:       "payer",
		Identity:       shnsdk.Identity{HolderID: "payer", SignPriv: signPriv},
		IngressEnabled: true,
		IngressBaseURL: testIngressBaseURL,
		IngressClients: map[string]IngressClientRegistration{
			clientID: {Alg: "ES384", PublicKeyPEM: pubPEM, Scopes: []string{ingressScope, TaskWriteScope}},
		},
		Reg:             reg,
		Validator:       shnsdk.NewFakeValidator(),
		SoR:             sor,
		Store:           sor,
		Clock:           ingressFixedClock(),
		HubURL:          "http://hub.test",
		HubTransportPub: hubPub,
		// The payer's own system, never reached: every row here ends at
		// this gateway's ingress or before the Hub.
		Responder: NewNativeResponder(http.DefaultClient, "http://payer.invalid", fixedClock),
	}
	for _, o := range opts {
		o(&cfg)
	}
	return mustNew(t, cfg)
}

// payerPost serves one request to g's handler: method and path under the
// ingress base, with bearer when set.
func payerPost(g *Gateway, method, path, bearer string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, testIngressBaseURL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/fhir+json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, req)
	return rec
}

func providerBasePath(provider string) string { return cdexroute.ProviderPrefix + provider + "/Task" }

// cdexRequestJTI numbers each assertion scopedBearer mints: the fixed clock
// would otherwise repeat its jti, which the token endpoint refuses.
var cdexRequestJTI int

// scopedBearer is a bearer the gateway's token endpoint issues clientID for
// scope ("" asks for none).
func scopedBearer(t *testing.T, g *Gateway, priv *ecdsa.PrivateKey, clientID, scope string) string {
	t.Helper()
	now := ingressFixedClock()()
	claims := validClaims(clientID, g.ingressAuth.tokenURL(), now)
	cdexRequestJTI++
	claims["jti"] = fmt.Sprintf("jti-%d", cdexRequestJTI)
	assertion := mintAssertion(t, priv, jwt.SigningMethodES384, claims)
	var rec *httptest.ResponseRecorder
	if scope == "" {
		rec = postToken(t, g.Handler(), assertion)
	} else {
		rec = postTokenScope(t, g.Handler(), assertion, scope)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("token for %q: %d %s", scope, rec.Code, rec.Body)
	}
	var tr struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &tr); err != nil || tr.AccessToken == "" {
		t.Fatalf("token for %q: %s (%v)", scope, rec.Body, err)
	}
	return tr.AccessToken
}

// The payer's Task routes take only a bearer granted system/Task.write: one
// granted none, or the provider ingress's scope, is refused 403
// insufficient_scope, recorded as the payer gateway's authority refusal,
// before the Task is read and before the exchange is the leg's, at a
// provider's base and at the plain Task endpoint alike. No bearer is 401. The
// SMART configuration advertises the one scope.
func TestCDexRequestIngress_TakesOnlyTheTaskWriteScope(t *testing.T) {
	priv, pub := newTestClientKey(t)
	g := payerGatewayWithAuth(t, "payer-system", pub)
	var got exchangeRecords
	g.cfg.ExchangeObserved = got.observe
	send := func(method, path, bearer string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, testIngressBaseURL+path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/fhir+json")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		g.Handler().ServeHTTP(rec, req)
		return rec
	}
	task := cdexRequestTask(t)
	for _, path := range []string{cdexroute.ProviderPrefix + "provider-1/Task", "/Task"} {
		for name, scope := range map[string]string{"no scope": "", "the provider ingress's scope": ingressScope} {
			t.Run(path+"/"+name, func(t *testing.T) {
				rec := send(http.MethodPost, path, scopedBearer(t, g, priv, "payer-system", scope), task)
				if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "insufficient_scope") ||
					!strings.Contains(rec.Header().Get("WWW-Authenticate"), `error="insufficient_scope"`) {
					t.Fatalf("answer %d %s (%q), want 403 insufficient_scope", rec.Code, rec.Body, rec.Header().Get("WWW-Authenticate"))
				}
				x := got.only(t)
				wantRefusal(t, x, http.StatusForbidden, RefusedByPayerGateway, RefusalAuthority)
				if x.Exchange != ExchangeOther {
					t.Fatalf("recorded as %s, want %s: the scope is checked before the leg is known", x.Exchange, ExchangeOther)
				}
				wantReasonInAnswer(t, x, rec.Body.String())
			})
		}
		t.Run(path+"/no bearer", func(t *testing.T) {
			if rec := send(http.MethodPost, path, "", task); rec.Code != http.StatusUnauthorized {
				t.Fatalf("answer %d %s, want 401", rec.Code, rec.Body)
			}
			x := got.only(t)
			wantRefusal(t, x, http.StatusUnauthorized, RefusedByPayerGateway, RefusalAuthentication)
			if x.Exchange != ExchangeOther {
				t.Fatalf("recorded as %s, want %s", x.Exchange, ExchangeOther)
			}
		})
	}
	// Granted the scope, the Task is read and routed: a base naming no
	// registered provider refuses it as routing, after the scope check.
	t.Run("granted the scope", func(t *testing.T) {
		rec := send(http.MethodPost, cdexroute.ProviderPrefix+"nobody/Task", scopedBearer(t, g, priv, "payer-system", TaskWriteScope), task)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "names no registered provider") {
			t.Fatalf("answer %d %s, want the routing refusal", rec.Code, rec.Body)
		}
		xr := got.only(t)
		wantRefusal(t, xr, http.StatusUnprocessableEntity, RefusedByPayerGateway, RefusalRouting)
		wantReasonInAnswer(t, xr, rec.Body.String())
	})
	t.Run("the listing", func(t *testing.T) {
		if rec := send(http.MethodGet, CDexProvidersPath, scopedBearer(t, g, priv, "payer-system", ingressScope), nil); rec.Code != http.StatusForbidden {
			t.Fatalf("listing with another scope: %d %s, want 403", rec.Code, rec.Body)
		}
		rec := send(http.MethodGet, CDexProvidersPath, scopedBearer(t, g, priv, "payer-system", TaskWriteScope), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("listing: %d %s", rec.Code, rec.Body)
		}
	})
	t.Run("the SMART configuration", func(t *testing.T) {
		rec := send(http.MethodGet, "/.well-known/smart-configuration", "", nil)
		var doc struct {
			Scopes []string `json:"scopes_supported"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil || !slices.Equal(doc.Scopes, []string{TaskWriteScope}) {
			t.Fatalf("scopes_supported %v (%s), want [%s]", doc.Scopes, rec.Body, TaskWriteScope)
		}
	})
}

// A registered client cannot grant itself the scope with a direct bearer its
// registration does not allow; one that its registration allows is taken.
func TestCDexRequestIngress_DirectBearerScope(t *testing.T) {
	priv, pub := newTestClientKey(t)
	g := payerGatewayWithAuth(t, "payer-system", pub)
	direct := func(scope string) string {
		now := ingressFixedClock()()
		claims := jwt.MapClaims{"iss": "payer-system", "aud": testIngressBaseURL + "/Task", "jti": "d-" + scope,
			"iat": now.Unix(), "exp": now.Add(time.Minute).Unix()}
		if scope != "" {
			claims["scope"] = scope
		}
		return mintAssertion(t, priv, jwt.SigningMethodES384, claims)
	}
	req := func(bearer string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, testIngressBaseURL+"/Task", nil)
		r.Header.Set("Authorization", "Bearer "+bearer)
		return r
	}
	for scope, want := range map[string]string{"": "", TaskWriteScope: TaskWriteScope, "system/*.write": ""} {
		_, granted, ok, _ := g.ingressAuthGrant(req(direct(scope)))
		if !ok || granted != want {
			t.Errorf("direct bearer stating %q: granted %q (ok %v), want %q", scope, granted, ok, want)
		}
	}
}

// GET /cdex/providers lists every registered provider that declares the
// attachment request, each with its base on this gateway; one that declares
// no list, another list, or that is no provider, is left out.
func TestCDexProviders_ListsEachProvidersBase(t *testing.T) {
	env, _ := payerIngressExchange(t, EnforcementObserve)
	env.originator.cfg.IngressBaseURL = "https://payer.example.test/fhir"
	list := func() (int, []cdexProvider) {
		rec := httptest.NewRecorder()
		env.originator.handleCDexProviders(rec, httptest.NewRequest(http.MethodGet, CDexProvidersPath, nil))
		var doc struct {
			Providers []cdexProvider `json:"providers"`
		}
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
				t.Fatalf("listing %s: %v", rec.Body, err)
			}
		}
		return rec.Code, doc.Providers
	}
	provider, ok := env.originator.cfg.Reg.Lookup("provider")
	if !ok {
		t.Fatal("fixture: no provider in the registry")
	}
	// A payer is left out whatever it declares.
	payer, _ := env.originator.cfg.Reg.Lookup(env.payerID)
	payer.Legs = []string{legCDexRequest}
	env.originator.cfg.Reg.Set(env.payerID, payer)
	for name, legs := range map[string][]string{"no list": nil, "an empty list": {}, "a list without the leg": {legCDexSubmit}} {
		notTaking := provider
		notTaking.Legs = legs
		env.originator.cfg.Reg.Set("provider", notTaking)
		if code, providers := list(); code != http.StatusOK || len(providers) != 0 {
			t.Fatalf("%s: listing %d %+v, want none", name, code, providers)
		}
	}
	provider.Legs = []string{legCDexRequest}
	env.originator.cfg.Reg.Set("provider", provider)
	want, err := cdexroute.ProviderBase(env.originator.cfg.IngressBaseURL, "provider")
	if err != nil {
		t.Fatal(err)
	}
	if code, providers := list(); code != http.StatusOK || len(providers) != 1 || !sameCDexProvider(providers[0], cdexProvider{Participant: "provider", Base: want}) {
		t.Fatalf("listing %d %+v, want the one provider at %s", code, providers, want)
	}
	// The organization identifiers the network attests for a provider are
	// listed with its base, as the public feed serves them.
	attested := []shnsdk.ProviderIdentifier{{System: providerIDSystem, Value: "1234567893"}}
	provider.ProviderIDs = attested
	env.originator.cfg.Reg.Set("provider", provider)
	if code, providers := list(); code != http.StatusOK || len(providers) != 1 || !sameCDexProvider(providers[0], cdexProvider{Participant: "provider", Base: want, Identifiers: attested}) {
		t.Fatalf("listing %d %+v, want the provider with its identifiers", code, providers)
	}
	// A provider whose base cannot be built makes the listing unavailable,
	// never a shorter list.
	env.originator.cfg.Reg.Set("provider two", shnsdk.RegistryEntry{ID: "provider two", Role: "provider", Legs: []string{legCDexRequest}})
	if code, providers := list(); code != http.StatusServiceUnavailable || providers != nil {
		t.Fatalf("listing %d %+v, want 503 and no list", code, providers)
	}
}

// The payer's ingress routes are mounted on a payer whose ingress is on, and
// nowhere else.
func TestCDexRequestIngress_MountedOnlyOnAPayerWithItsIngress(t *testing.T) {
	g := &Gateway{cfg: Config{Role: "payer"}}
	if routes := g.paRoutes("payer"); len(routes) != 0 {
		t.Fatalf("ingress off: %d routes mounted", len(routes))
	}
	g.cfg.IngressEnabled = true
	var patterns []string
	for _, r := range g.paRoutes("payer") {
		patterns = append(patterns, r.pattern)
	}
	for _, want := range []string{"POST " + cdexroute.ProviderPrefix + "{provider}/Task", "POST /Task", "GET " + CDexProvidersPath} {
		if !slices.Contains(patterns, want) {
			t.Errorf("payer routes %v, want %q", patterns, want)
		}
	}
	for _, p := range patterns {
		if strings.Contains(p, "cds-services") || strings.Contains(p, "$submit") {
			t.Errorf("payer mounts a provider route %q", p)
		}
	}
}

// hubHits is a stand-in for the network's services (the Hub and the
// Authorization Framework) that answers every call 503 and counts them: a
// refused Task never reaches it.
func hubHits(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &hits
}

// cdexRequestRecipientID is the registered provider the in-process exchange
// carries a Task to: it declares the leg, and the stand-in Hub opens what is
// sealed for it.
const cdexRequestRecipientID = "cdex-provider"

// runCDexRequestIngress sends body through the payer's ingress to
// cdexRequestRecipientID's base at level, with the provider answering answer, and
// returns the exchange, the payer system's answer and the one exchange record.
// The stand-in Hub (relaySubstrate) carries the leg as the network would; the
// Hub's own checks of the leg are the end-to-end rows'.
func runCDexRequestIngress(t *testing.T, level ConformanceEnforcement, body []byte, answer LegResult) (*inProcessExchange, *httptest.ResponseRecorder, ExchangeRecord) {
	t.Helper()
	env, got := payerIngressExchange(t, level)
	env.originator.cfg.Reg.Set(cdexRequestRecipientID, shnsdk.RegistryEntry{ID: cdexRequestRecipientID, Role: "provider",
		EncPub: env.substrate.recipientEncPub, Legs: []string{legCDexRequest}, MessageFrames: shnsdk.SupportedMessageFrames()})
	env.payerReturns(answer)
	rec := httptest.NewRecorder()
	env.originator.ingressRoute(RouteCDexRequestBase)(rec, cdexRequestPost(body, cdexRequestRecipientID))
	return env, rec, got.only(t)
}

// cdexRequestAnswer is a provider's answer to the Task's create: the Task as
// its system holds it.
var cdexRequestAnswer = []byte(`{"resourceType":"Task","id":"t-1","status":"requested","intent":"order"}`)

// A Task the network can bind to a patient, coded as a request for
// attachments by code or (D128) for a questionnaire, is carried to the
// provider its base names exactly as sent, and the provider's answer relayed exactly, with
// its status and no Location, at every level, whichever form names the member
// number: the Patient it contains as Task.for (the IG's own example),
// Task.for.identifier alone, or both, agreeing; and the IG's own
// questionnaire-request Tasks, byte for byte. The leg is authorized for one
// patient, and nothing is edited.
func TestCDexRequestIngress_CarriesTheTaskAsSent(t *testing.T) {
	t.Parallel()
	mb := map[string]any{"coding": []any{map[string]any{"system": v20203MB.System, "code": "MB"}}}
	bodies := map[string]func(t *testing.T) []byte{
		"the contained Patient": cdexRequestTask,
		"Task.for.identifier alone": func(t *testing.T) []byte {
			return cdexRequestMutate(t, func(m map[string]any) {
				m["for"] = map[string]any{"identifier": map[string]any{"type": mb, "value": "Member123"}}
			})
		},
		"both, agreeing": func(t *testing.T) []byte {
			return cdexRequestMutate(t, func(m map[string]any) {
				m["for"] = map[string]any{"reference": "#patient", "identifier": map[string]any{"type": mb, "value": "Member123"}}
			})
		},
	}
	// The IG's questionnaire-request Tasks (D128), as published.
	for _, f := range cdexQuestionnaireTasks {
		bodies["the IG's "+f] = func(t *testing.T) []byte { return cdexFixture(t, f) }
	}
	for name, body := range bodies {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				task := body(t)
				env, rec, x := runCDexRequestIngress(t, level, task,
					LegResult{Response: relayedResponse(cdexRequestAnswer), AnswerStatus: http.StatusCreated})
				if rec.Code != http.StatusCreated || !bytes.Equal(rec.Body.Bytes(), cdexRequestAnswer) {
					t.Fatalf("answer %d %s, want the provider's 201 relayed exactly", rec.Code, rec.Body)
				}
				if loc := rec.Header().Get("Location"); loc != "" {
					t.Fatalf("answer carries Location %q, want none", loc)
				}
				if sent := env.lastRequestPayload(); !bytes.Equal(sent, task) {
					t.Fatalf("the provider's side received\n%s\nwant the Task as sent\n%s", sent, task)
				}
				env.substrate.mu.Lock()
				names, subjects := slices.Clone(env.substrate.sentNames), slices.Clone(env.substrate.authorizedSubjects)
				env.substrate.mu.Unlock()
				if want := wireName(legCDexRequest); !slices.Equal(names, []string{want}) {
					t.Fatalf("sent as %v, want the one %s leg", names, want)
				}
				if len(subjects) != 1 || subjects[0] == "" {
					t.Fatalf("authorized for %q, want one patient", subjects)
				}
				if x.Outcome != ExchangeAnswered || x.Route != RouteCDexRequestBase || x.Exchange != legCDexRequest ||
					x.Recipient != cdexRequestRecipientID || len(x.Edits) != 0 {
					t.Fatalf("record %+v, want an answered %s exchange with %s on route %s, carried unedited", x, legCDexRequest, cdexRequestRecipientID, RouteCDexRequestBase)
				}
			})
		}
	}
}

// Today's bind of a payer-system member number, pinned so that a change to it
// is made on purpose: the shared rule reads a carried Patient's demographics
// only when its id is the member number or it carries the number under
// shnsdk.MemberSystem, so a Task naming its member under the payer's own
// identifier system, as the CDex IG's example does, derives the patient's
// identifier from the number alone, as the provider's CDex submission keyed by
// the same number does. A Coverage's subscriber carrying that same payer-system
// number is not read as sharing it, so the Task is carried, at every level.
func TestCDexRequestIngress_APayerSystemMemberNumberDerivesFromTheNumberAlone(t *testing.T) {
	t.Parallel()
	mb := map[string]any{"coding": []any{map[string]any{"system": v20203MB.System, "code": "MB"}}}
	payerNumber := map[string]any{"type": mb, "system": "http://example.org/cdex/payer/member-ids", "value": "Member123"}
	want := derivedPCI("Member123", "", "")
	for name, body := range map[string]func(t *testing.T) []byte{
		// Its contained Patient has a birth date and a family name.
		"the IG's example as sent": cdexRequestTask,
		"a subscriber carrying the same payer-system number": func(t *testing.T) []byte {
			return cdexRequestMutate(t, func(m map[string]any) {
				m["for"] = map[string]any{"identifier": payerNumber}
				var keep []any
				for _, c := range m["contained"].([]any) {
					if c.(map[string]any)["resourceType"] != "Patient" {
						keep = append(keep, c)
					}
				}
				m["contained"] = append(keep, map[string]any{"resourceType": "Coverage", "id": "cov", "status": "active",
					"subscriber": map[string]any{"reference": "#sub"}, "beneficiary": map[string]any{"reference": "Patient/other"},
					"contained": []any{map[string]any{"resourceType": "Patient", "id": "sub", "identifier": []any{payerNumber},
						"birthDate": "1960-01-01", "name": []any{map[string]any{"family": "Other"}}}}})
			})
		},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env, rec, x := runCDexRequestIngress(t, level, body(t),
					LegResult{Response: relayedResponse(cdexRequestAnswer), AnswerStatus: http.StatusCreated})
				if rec.Code != http.StatusCreated || x.Outcome != ExchangeAnswered {
					t.Fatalf("answer %d %s (record %s), want the Task carried and answered", rec.Code, rec.Body, x.Outcome)
				}
				env.substrate.mu.Lock()
				subjects := slices.Clone(env.substrate.authorizedSubjects)
				env.substrate.mu.Unlock()
				if len(subjects) != 1 || subjects[0] != want {
					t.Fatalf("authorized for %q, want the identifier derived from the member number alone %s", subjects, want)
				}
			})
		}
	}
}

// The provider's answer is its own: an error relayed exactly as the provider's
// side sent it (a provider's gateway that takes no Task refuses it 422), its
// media type kept, and one that repeats a member name refused on message
// integrity by the payer's gateway, at every level. That refusal reads
// structure only: an object naming one member twice, which readers resolve
// differently, so it cannot be carried as one message. Content is never read:
// an answer repeating an identifier value, as two identifiers with the same
// value, is relayed exactly.
func TestCDexRequestIngress_RelaysTheProvidersAnswer(t *testing.T) {
	t.Parallel()
	oo := []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"not-supported","diagnostics":"no Task endpoint"}]}`)
	repeated := []byte(`{"resourceType":"Task","resourceType":"Bundle","status":"requested"}`)
	sameValueTwice := []byte(`{"resourceType":"Task","id":"t-1","status":"requested","intent":"order",` +
		`"identifier":[{"system":"urn:x","value":"M1"},{"system":"urn:x","value":"M1"}]}`)
	for _, level := range allLevels {
		t.Run("error/"+level.String(), func(t *testing.T) {
			_, rec, _ := runCDexRequestIngress(t, level, cdexRequestTask(t), LegResult{Status: http.StatusUnprocessableEntity, Response: testResponse(oo)})
			if rec.Code != http.StatusUnprocessableEntity || !bytes.Equal(rec.Body.Bytes(), oo) {
				t.Fatalf("answer %d %s, want the provider's 422 relayed exactly", rec.Code, rec.Body)
			}
		})
		t.Run("media type/"+level.String(), func(t *testing.T) {
			_, rec, _ := runCDexRequestIngress(t, level, cdexRequestTask(t),
				LegResult{Response: relay.ForTest(cdexRequestAnswer, "application/json"), AnswerStatus: http.StatusCreated})
			if rec.Code != http.StatusCreated || rec.Header().Get("Content-Type") != "application/json" || !bytes.Equal(rec.Body.Bytes(), cdexRequestAnswer) {
				t.Fatalf("answer %d %q %s, want the provider's answer with its own media type", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
			}
		})
		t.Run("repeated member/"+level.String(), func(t *testing.T) {
			_, rec, x := runCDexRequestIngress(t, level, cdexRequestTask(t), LegResult{Response: testResponse(repeated)})
			if rec.Code != http.StatusBadGateway || bytes.Contains(rec.Body.Bytes(), []byte("Bundle")) {
				t.Fatalf("answer %d %s, want 502 and the provider's bytes not relayed", rec.Code, rec.Body)
			}
			wantRefusal(t, x, http.StatusBadGateway, RefusedByPayerGateway, RefusalIntegrity)
		})
		t.Run("repeated identifier value/"+level.String(), func(t *testing.T) {
			_, rec, x := runCDexRequestIngress(t, level, cdexRequestTask(t),
				LegResult{Response: relayedResponse(sameValueTwice), AnswerStatus: http.StatusCreated})
			if rec.Code != http.StatusCreated || !bytes.Equal(rec.Body.Bytes(), sameValueTwice) || x.Outcome != ExchangeAnswered {
				t.Fatalf("answer %d %s (record %s), want the provider's answer relayed exactly", rec.Code, rec.Body, x.Outcome)
			}
		})
	}
}

// A Task the gateway cannot settle is refused before anything is sent, at
// every level: one bound while the payer's own system of record fails (that
// system's failure, never an empty or guessed member), and a native client's
// that names a builder (only a module ingress client builds for another).
func TestCDexRequestIngress_RefusedBeforeTheNetwork(t *testing.T) {
	t.Parallel()
	sorErr := &SoRReadError{Kind: SoRUnavailable}
	sorStatus, sorMsg := SoRFailureResponse(sorErr)
	for _, level := range allLevels {
		t.Run("system of record fails/"+level.String(), func(t *testing.T) {
			env, got := payerIngressExchange(t, level)
			env.originator.cfg.SoR = failingSoR{SystemOfRecord: env.originator.cfg.SoR, err: sorErr}
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteCDexRequestBase)(rec, cdexRequestPost(cdexRequestTask(t), "provider"))
			refusedBeforeTheNetwork(t, env, rec, sorStatus, sorMsg)
			if x := got.only(t); x.Exchange != legCDexRequest || x.Outcome == ExchangeAnswered {
				t.Fatalf("record %+v, want the %s leg not answered", x, legCDexRequest)
			}
		})
		t.Run("a native client names a builder/"+level.String(), func(t *testing.T) {
			env, got := payerIngressExchange(t, level)
			req := cdexRequestPost(cdexRequestTask(t), "provider")
			req.Header.Set(BuilderHeader, "some-builder")
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteCDexRequestBase)(rec, req)
			refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, errBuilderNative)
			wantRefusal(t, got.only(t), http.StatusUnprocessableEntity, RefusedByPayerGateway, RefusalFidelity)
		})
	}
}

// A body over the request ceiling is refused 413 before anything is sent,
// never cut short and read as another message, recorded as routing, at every
// level.
func TestCDexRequestIngress_OversizeTaskIsRefused413(t *testing.T) {
	t.Parallel()
	big := cdexRequestEdit(t, `"status" : "requested"`, `"status" : "requested", "text" : {"div" : "`+strings.Repeat("a", int(shnsdk.MaxRequestBytes))+`"}`)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env, got := payerIngressExchange(t, level)
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteCDexRequestBase)(rec, cdexRequestPost(big, "provider"))
			refusedBeforeTheNetwork(t, env, rec, http.StatusRequestEntityTooLarge, "request body too large")
			x := got.only(t)
			wantRefusal(t, x, http.StatusRequestEntityTooLarge, RefusedByPayerGateway, RefusalRouting)
			if x.Exchange != legCDexRequest {
				t.Fatalf("refusal recorded as exchange %q, want %s", x.Exchange, legCDexRequest)
			}
		})
	}
}

// A payer that opts in to checking members refuses a member its own system
// does not hold: a check it opted into, recorded as conformance, at every
// level. Without the opt-in the same Task is carried (above).
func TestCDexRequestIngress_UnknownMemberRefusedOnlyWhenOptedIn(t *testing.T) {
	priv, pub := newTestClientKey(t)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			var got exchangeRecords
			g := payerGatewayWithAuth(t, "payer-system", pub, func(c *Config) {
				c.ConformanceEnforcement = level
				c.ExchangeObserved = got.observe
				c.RequireKnownMembers = true
			})
			rec := payerPost(g, http.MethodPost, providerBasePath("provider-1"), scopedBearer(t, g, priv, "payer-system", TaskWriteScope), cdexRequestTask(t))
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unknown member") {
				t.Fatalf("answer %d %s, want 400 unknown member", rec.Code, rec.Body)
			}
			wantRefusal(t, got.only(t), http.StatusBadRequest, RefusedByPayerGateway, RefusalConformance)
		})
	}
}

// The scope is checked whatever the level, and through every way a caller
// presents a credential: a direct bearer (a registered client's own signed
// token) is taken only with a scope its registration allows; a forged or
// tampered bearer is 401; a key-store outage is 503, never a refusal of an
// honest caller.
func TestCDexRequestIngress_ScopeAtEveryLevelAndEveryCredential(t *testing.T) {
	priv, pub := newTestClientKey(t)
	for _, level := range allLevels {
		t.Run("another scope/"+level.String(), func(t *testing.T) {
			var got exchangeRecords
			g := payerGatewayWithAuth(t, "payer-system", pub, func(c *Config) {
				c.ConformanceEnforcement = level
				c.ExchangeObserved = got.observe
			})
			rec := payerPost(g, http.MethodPost, providerBasePath("provider-1"), scopedBearer(t, g, priv, "payer-system", ingressScope), cdexRequestTask(t))
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "insufficient_scope") {
				t.Fatalf("answer %d %s, want 403 insufficient_scope", rec.Code, rec.Body)
			}
			xr := got.only(t)
			wantRefusal(t, xr, http.StatusForbidden, RefusedByPayerGateway, RefusalAuthority)
			wantReasonInAnswer(t, xr, rec.Body.String())
		})
	}
	direct := func(scope string) string {
		now := ingressFixedClock()()
		cdexRequestJTI++
		claims := jwt.MapClaims{"iss": "payer-system", "aud": testIngressBaseURL + providerBasePath("nobody"), "jti": fmt.Sprintf("d-%d", cdexRequestJTI),
			"iat": now.Unix(), "exp": now.Add(time.Minute).Unix()}
		if scope != "" {
			claims["scope"] = scope
		}
		return mintAssertion(t, priv, jwt.SigningMethodES384, claims)
	}
	for name, row := range map[string]struct {
		bearer func(g *Gateway) string
		status int
		rule   string
	}{
		// Past the scope, the base names no provider: routing.
		"a direct bearer stating the scope":     {func(*Gateway) string { return direct(TaskWriteScope) }, http.StatusUnprocessableEntity, RefusalRouting},
		"a direct bearer stating none":          {func(*Gateway) string { return direct("") }, http.StatusForbidden, RefusalAuthority},
		"a direct bearer stating another scope": {func(*Gateway) string { return direct(ingressScope) }, http.StatusForbidden, RefusalAuthority},
		"a forged bearer":                       {func(*Gateway) string { return "a.b.c" }, http.StatusUnauthorized, RefusalAuthentication},
		"a tampered bearer": {func(g *Gateway) string {
			// The signature's first character, moved to the next one of the
			// base64url alphabet: an ES384 signature fills its text exactly,
			// so the signed bytes always differ.
			const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
			b := []byte(scopedBearer(t, g, priv, "payer-system", TaskWriteScope))
			i := bytes.LastIndexByte(b, '.') + 1
			b[i] = alphabet[(strings.IndexByte(alphabet, b[i])+1)%len(alphabet)]
			return string(b)
		}, http.StatusUnauthorized, RefusalAuthentication},
	} {
		t.Run(name, func(t *testing.T) {
			var got exchangeRecords
			g := payerGatewayWithAuth(t, "payer-system", pub, func(c *Config) { c.ExchangeObserved = got.observe })
			rec := payerPost(g, http.MethodPost, providerBasePath("nobody"), row.bearer(g), cdexRequestTask(t))
			if rec.Code != row.status {
				t.Fatalf("answer %d %s, want %d", rec.Code, rec.Body, row.status)
			}
			xr := got.only(t)
			wantRefusal(t, xr, row.status, RefusedByPayerGateway, row.rule)
			wantReasonInAnswer(t, xr, rec.Body.String())
			// The listing is held to the same credential.
			if listed := payerPost(g, http.MethodGet, CDexProvidersPath, row.bearer(g), nil); row.status != http.StatusUnprocessableEntity && listed.Code != row.status {
				t.Fatalf("listing %d %s, want %d", listed.Code, listed.Body, row.status)
			}
		})
	}
	t.Run("a key-store outage", func(t *testing.T) {
		eph, err := newEphemeralKeyStore()
		if err != nil {
			t.Fatal(err)
		}
		kid, err := newKID()
		if err != nil {
			t.Fatal(err)
		}
		now := ingressFixedClock()()
		bearer := mintAssertionKID(t, priv, kid, jwt.MapClaims{"client_id": "payer-system", "aud": testIngressBaseURL, "scope": TaskWriteScope,
			"iat": now.Unix(), "exp": now.Add(time.Minute).Unix()})
		g := payerGatewayWithAuth(t, "payer-system", pub, func(c *Config) { c.IngressKeys = &unavailableKeyStore{IngressKeyStore: eph} })
		for _, path := range []string{providerBasePath("provider-1"), CDexProvidersPath} {
			method := http.MethodPost
			if path == CDexProvidersPath {
				method = http.MethodGet
			}
			if rec := payerPost(g, method, path, bearer, cdexRequestTask(t)); rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("%s during a key-store outage: %d %s, want 503", path, rec.Code, rec.Body)
			}
		}
	})
}

// sameCDexProvider reports whether a and b are one listing entry: the same
// participant, base and identifiers, an omitted identifier list apart from
// an empty one.
func sameCDexProvider(a, b cdexProvider) bool {
	return a.Participant == b.Participant && a.Base == b.Base &&
		(a.Identifiers == nil) == (b.Identifiers == nil) && slices.Equal(a.Identifiers, b.Identifiers)
}
