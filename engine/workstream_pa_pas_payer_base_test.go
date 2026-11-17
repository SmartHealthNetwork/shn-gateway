//go:build !shn_coreonly

package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/pa/pasroute"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// otherPayerID is a second registered payer, which the payer router maps the
// payer identifier otherPayerValue to.
const (
	otherPayerID    = "payer-b"
	otherPayerValue = "00002"
)

// pasBaseExchange is the in-process exchange at level with a second
// registered payer the router resolves, its exchange records and its
// observer events.
func pasBaseExchange(t *testing.T, level ConformanceEnforcement) (*inProcessExchange, *exchangeRecords, *pasBaseEvents) {
	t.Helper()
	env := newInProcessExchange(t)
	env.originator.cfg.ConformanceEnforcement = level
	reg := env.originator.cfg.Reg
	payer, _ := reg.Lookup(env.payerID)
	// The base's payer registers the payer identifier the fixtures' Coverage
	// names, by which tolerance matches its answers.
	payer.PayerIDs = []shnsdk.PayerIdentifier{shnsdk.CMSPayerIdentity}
	reg.Set(env.payerID, payer)
	reg.Set(otherPayerID, shnsdk.RegistryEntry{ID: otherPayerID, Role: "payer", EncPub: payer.EncPub, SignPub: payer.SignPub,
		Legs: AnsweredLegs("payer"), MessageFrames: shnsdk.SupportedMessageFrames()})
	router, err := NewConfigPayerRouter([]PayerDirectoryEntry{
		{System: shnsdk.CMSPayerIdentity.System, Value: shnsdk.CMSPayerIdentity.Value, HolderID: env.payerID},
		{System: shnsdk.CMSPayerIdentity.System, Value: otherPayerValue, HolderID: otherPayerID},
	})
	if err != nil {
		t.Fatal(err)
	}
	env.originator.cfg.PayerRouter = router
	var got exchangeRecords
	env.originator.cfg.ExchangeObserved = got.observe
	events := &pasBaseEvents{}
	env.originator.cfg.Observer = events.observe
	return env, &got, events
}

// pasBaseEvents collects observer events.
type pasBaseEvents struct {
	mu     sync.Mutex
	events []ObserverEvent
}

func (e *pasBaseEvents) observe(ev ObserverEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, ev)
}

// of are the events of kind.
func (e *pasBaseEvents) of(kind string) []ObserverEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []ObserverEvent
	for _, ev := range e.events {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

// withPayorValue is body with its Coverage's payor identifier value, the
// fixture's "00001", replaced by value.
func withPayorValue(t *testing.T, body, value string) string {
	t.Helper()
	out := strings.ReplaceAll(body, `"00001"`, `"`+value+`"`)
	if out == body {
		t.Fatal("fixture: no payor identifier value replaced")
	}
	return out
}

// withPayorSystem is body with its Coverage's payor identifier system
// replaced by system.
func withPayorSystem(t *testing.T, body, system string) string {
	t.Helper()
	out := strings.ReplaceAll(body, `"urn:oid:2.16.840.1.113883.6.300"`, `"`+system+`"`)
	if out == body {
		t.Fatal("fixture: no payor identifier system replaced")
	}
	return out
}

// pasBasePath is the leg's operation at payer's base.
func pasBasePath(leg carriedAnswerLeg, payer string) string {
	return pasroute.Prefix + payer + leg.path
}

// servePASBase posts body to path through the gateway's mounted routes.
func servePASBase(env *inProcessExchange, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/fhir+json")
	env.originator.Handler().ServeHTTP(rec, req)
	env.originator.drainObserveChecks()
	return rec
}

// wantPASPayerEventOnly fails unless e is the event kind carrying the leg, its
// direction and the leg's correlation, and nothing else: no identifier,
// counterpart or payload. Every field is compared, so a field added to the
// event is caught too.
func wantPASPayerEventOnly(t *testing.T, e ObserverEvent, kind, leg, corrID string) {
	t.Helper()
	if corrID == "" {
		t.Fatal("the record carries no correlation id")
	}
	e.Time = time.Time{}
	got, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(ObserverEvent{Kind: kind, Direction: "ingress", LegType: leg, CorrelationID: corrID})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("event %s, want %s", got, want)
	}
}

// pasPayerEvents are the events a request at a payer's base may note, by
// the note that raises each.
var pasPayerEvents = map[pasPayerNote]string{pasPayerDiffers: PASPayerDiffersEvent, pasPayerUnresolved: PASPayerUnresolvedEvent}

// wantPASPayerNote fails unless events hold exactly the event note raises
// (none for pasPayerSame), for leg and corrID, and no other of
// pasPayerEvents.
func wantPASPayerNote(t *testing.T, events *pasBaseEvents, note pasPayerNote, leg, corrID string) {
	t.Helper()
	for n, kind := range pasPayerEvents {
		raised := events.of(kind)
		want := 0
		if n == note {
			want = 1
		}
		if len(raised) != want {
			t.Fatalf("%s raised %d times, want %d (note %d)", kind, len(raised), want, note)
		}
		if want == 1 {
			wantPASPayerEventOnly(t, raised[0], kind, leg, corrID)
		}
	}
}

// A submit, an amendment or an inquiry at a payer's base goes to the payer
// the base names, carried as sent, whatever its Coverage names (N12), at
// every level, and notes what the Coverage names, naming nothing beyond the
// leg. A Coverage the payer directory resolves to the base's own payer notes
// nothing; one it resolves to another registered payer notes
// pas.payer-differs. One this gateway cannot resolve notes
// pas.payer-unresolved, never pas.payer-differs, since an unknown payer is
// neither: a payer identifier the directory does not list, a payor with no
// identifier system, and any Coverage at a gateway with no payer directory.
// At the plain base the unresolved ones are refused 422 (the control).
func TestPASPayerBase_RoutesByTheAddressAndNotesTheCoveragesPayer(t *testing.T) {
	cases := []struct {
		name        string
		edit        func(t *testing.T, body string) string
		noDirectory bool
		note        pasPayerNote
		// plain is the plain base's status for the same body.
		plain int
	}{
		{"the base's own payer", func(_ *testing.T, body string) string { return body }, false, pasPayerSame, http.StatusOK},
		{"another registered payer", func(t *testing.T, body string) string { return withPayorValue(t, body, otherPayerValue) }, false, pasPayerDiffers, 0},
		{"a payer identifier no one registers", func(t *testing.T, body string) string { return withPayorValue(t, body, "99999") }, false, pasPayerUnresolved, http.StatusUnprocessableEntity},
		{"a payor with no identifier system", func(t *testing.T, body string) string { return withPayorSystem(t, body, "") }, false, pasPayerUnresolved, http.StatusUnprocessableEntity},
		{"the base's own payer, with no payer directory", func(_ *testing.T, body string) string { return body }, true, pasPayerUnresolved, http.StatusUnprocessableEntity},
		{"another registered payer, with no payer directory", func(t *testing.T, body string) string { return withPayorValue(t, body, otherPayerValue) }, true, pasPayerUnresolved, http.StatusUnprocessableEntity},
	}
	for _, leg := range carriedAnswerLegs() {
		for _, c := range cases {
			for _, level := range allLevels {
				t.Run(leg.leg+"/"+c.name+"/"+level.String(), func(t *testing.T) {
					exchange := func() (*inProcessExchange, *exchangeRecords, *pasBaseEvents) {
						env, got, events := pasBaseExchange(t, level)
						if c.noDirectory {
							env.originator.cfg.PayerRouter = nil
						}
						env.payerReturns(LegResult{Response: testResponse(leg.answer(t))})
						return env, got, events
					}
					env, got, events := exchange()
					body := c.edit(t, leg.body(t))
					rec := servePASBase(env, pasBasePath(leg, env.payerID), body)
					x := got.only(t)
					if rec.Code != http.StatusOK || x.Recipient != env.payerID || x.Route != leg.route {
						t.Fatalf("answer %d %s (record to %s on %s), want the claim carried to the base's payer on %s", rec.Code, rec.Body, x.Recipient, x.Route, leg.route)
					}
					if env.routeHitCount() != 1 {
						t.Fatalf("network hits %d, want the claim carried once", env.routeHitCount())
					}
					if sent := env.lastRequestPayload(); !bytes.Equal(sent, []byte(body)) {
						t.Fatalf("the claim crossed the network as %s, want it as sent", sent)
					}
					wantPASPayerNote(t, events, c.note, leg.leg, x.CorrelationID)
					if c.plain == 0 {
						return
					}
					// The control: the same body at the plain base is routed by
					// its Coverage, and notes nothing.
					env, got, events = exchange()
					rec = servePASBase(env, leg.path, body)
					x = got.only(t)
					if rec.Code != c.plain {
						t.Fatalf("plain base: answer %d %s, want %d", rec.Code, rec.Body, c.plain)
					}
					wantPASPayerNote(t, events, pasPayerSame, leg.leg, x.CorrelationID)
					if c.plain == http.StatusUnprocessableEntity {
						wantRefusal(t, x, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalRouting)
					}
				})
			}
		}
	}
}

// A base that names no registered payer, a participant that is no payer
// among them, is refused 404 with an OperationOutcome naming no payer, on
// routing, at every level, before the request's content is checked: a body
// the subject binding, or the inquiry's reading, would refuse is refused for
// its address all the same. Nothing is sent or noted.
func TestPASPayerBase_ABaseNamingNoPayerIs404(t *testing.T) {
	for _, leg := range carriedAnswerLegs() {
		for _, base := range []string{"nobody", "provider"} {
			for _, body := range []struct{ name, body string }{{"a claim", leg.body(t)}, {"no Bundle", `{}`}} {
				for _, level := range allLevels {
					t.Run(leg.leg+"/"+base+"/"+body.name+"/"+level.String(), func(t *testing.T) {
						env, got, events := pasBaseExchange(t, level)
						rec := servePASBase(env, pasBasePath(leg, base), body.body)
						refusedBeforeTheNetwork(t, env, rec, http.StatusNotFound, errPASBaseNoPayer)
						wantNotFoundOutcome(t, rec, errPASBaseNoPayer)
						x := got.only(t)
						wantRefusal(t, x, http.StatusNotFound, RefusedByProviderGateway, RefusalRouting)
						if x.RefusalReason != string(reasonPASBaseNoPayer) || x.Route != leg.route {
							t.Fatalf("record reason %q on %s, want %q on %s", x.RefusalReason, x.Route, reasonPASBaseNoPayer, leg.route)
						}
						wantPASPayerNote(t, events, pasPayerSame, leg.leg, "-")
					})
				}
			}
			// The control: the same unreadable body at the base's own payer
			// is refused by the checks the address no longer pre-empts, not
			// for its address.
			env, got, _ := pasBaseExchange(t, EnforcementNone)
			rec := servePASBase(env, pasBasePath(leg, env.payerID), `{}`)
			if x := got.only(t); rec.Code == http.StatusOK || rec.Code == http.StatusNotFound || x.RefusalReason == string(reasonPASBaseNoPayer) {
				t.Fatalf("control: %d %s (reason %q), want a refusal other than the base's 404", rec.Code, rec.Body, x.RefusalReason)
			}
		}
	}
}

// A claim at a payer's base refused after it is routed and before it is
// sealed raises no pas.payer-differs, though its Coverage names another
// payer: the event records a claim the gateway carries, and this one is not.
// A clinician-sourced item without its attestation is refused there at
// strict.
func TestPASPayerBase_AClaimRefusedBeforeSealingRaisesNoEvent(t *testing.T) {
	item, err := shnsdk.BuildManualAttestedItem("functional-status-oswestry", "42", shnsdk.Attestation{NPI: "1999999999", Text: "I attest these are my clinical findings.", When: "2026-06-04"})
	if err != nil {
		t.Fatal(err)
	}
	body := withPayorValue(t, levelPASBundleWithEntry(`{"resource":{"resourceType":"QuestionnaireResponse","subject":{"reference":"Patient/MBR-COVERED"},"item":[`+string(stripItemExtension(t, item))+`]}}`), otherPayerValue)
	env, got, events := pasBaseExchange(t, EnforcementStrict)
	rec := servePASBase(env, pasroute.Prefix+env.payerID+"/Claim/$submit", body)
	refusedBeforeTheNetwork(t, env, rec, http.StatusForbidden, "is clinician-sourced")
	got.only(t)
	wantPASPayerNote(t, events, pasPayerSame, "pas-claim", "-")
	// The control: below strict the same claim is carried, and recorded.
	env, got, events = pasBaseExchange(t, EnforcementObserve)
	env.payerReturns(LegResult{Response: testResponse(levelPASPayerAnswer)})
	rec = servePASBase(env, pasroute.Prefix+env.payerID+"/Claim/$submit", body)
	if x := got.only(t); rec.Code != http.StatusOK || len(events.of(PASPayerDiffersEvent)) != 1 {
		t.Fatalf("control at observe: %d %s, %d events (record %s); want carried and recorded once", rec.Code, rec.Body, len(events.of(PASPayerDiffersEvent)), x.Outcome)
	}
}

// wantNotFoundOutcome fails unless rec is this gateway's own 404: an
// application/fhir+json OperationOutcome of one error issue of code
// not-found whose diagnostics is msg.
func wantNotFoundOutcome(t *testing.T, rec *httptest.ResponseRecorder, msg string) {
	t.Helper()
	wantOutcome(t, rec, http.StatusNotFound, "not-found", msg)
}

// wantOutcome fails unless rec is status with an application/fhir+json
// OperationOutcome of one error issue of code whose diagnostics is msg.
func wantOutcome(t *testing.T, rec *httptest.ResponseRecorder, status int, code, msg string) {
	t.Helper()
	var o struct {
		ResourceType string
		Issue        []struct{ Severity, Code, Diagnostics string }
	}
	if rec.Code != status || rec.Header().Get("Content-Type") != "application/fhir+json" || json.Unmarshal(rec.Body.Bytes(), &o) != nil ||
		o.ResourceType != "OperationOutcome" || len(o.Issue) != 1 || o.Issue[0].Severity != "error" || o.Issue[0].Code != code || o.Issue[0].Diagnostics != msg {
		t.Fatalf("answer %d %q %s, want %d, an OperationOutcome of one %s issue %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body, status, code, msg)
	}
}

// A payer's base serves no CapabilityStatement yet: the plain ingress's
// declares operations the base does not serve, so GET {base}/metadata is
// answered 404 as any path the gateway does not serve, never with the plain
// base's statement. The plain base's own is served (the control).
func TestPASPayerBase_ServesNoCapabilityStatementYet(t *testing.T) {
	env, _, _ := pasBaseExchange(t, EnforcementStrict)
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		env.originator.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	if plain := get("/metadata"); plain.Code != http.StatusOK {
		t.Fatalf("control: GET /metadata %d %s", plain.Code, plain.Body)
	}
	for _, payer := range []string{env.payerID, otherPayerID, "nobody"} {
		if rec := get(pasroute.Prefix + payer + "/metadata"); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "CapabilityStatement") {
			t.Fatalf("GET %s/metadata: %d %s, want 404 and no CapabilityStatement", payer, rec.Code, rec.Body)
		}
	}
}

// SMART App Launch has every FHIR base serve its configuration: at a payer's
// base it is the plain base's document, so a system that uses the base
// discovers this gateway's token endpoint there. A base naming no registered
// payer is 404. A gateway whose ingress authenticates no caller of its own
// mounts neither.
func TestPASPayerBase_ServesTheSmartConfiguration(t *testing.T) {
	env, _, _ := pasBaseExchange(t, EnforcementStrict)
	keys, err := newEphemeralKeyStore()
	if err != nil {
		t.Fatal(err)
	}
	_, pub := newTestClientKey(t)
	clients := map[string]IngressClientRegistration{"ehr": {Alg: "ES384", PublicKeyPEM: pub, Scopes: []string{ingressScope}}}
	s, err := newIngressAuthServer(testIngressBaseURL, clients, ingressFixedClock(), keys, NewInMemoryReplayStore())
	if err != nil {
		t.Fatal(err)
	}
	env.originator.ingressAuth = s
	env.originator.cfg.ingressAuthBypass = false
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		env.originator.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	plain := get("/.well-known/smart-configuration")
	if plain.Code != http.StatusOK || !strings.Contains(plain.Body.String(), "token_endpoint") {
		t.Fatalf("control: the plain base's SMART configuration %d %s", plain.Code, plain.Body)
	}
	for _, payer := range []string{env.payerID, otherPayerID} {
		if rec := get(pasroute.Prefix + payer + "/.well-known/smart-configuration"); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), plain.Body.Bytes()) {
			t.Fatalf("%s's base: %d %s, want the plain base's document", payer, rec.Code, rec.Body)
		}
	}
	for _, base := range []string{"nobody", "provider"} {
		if rec := get(pasroute.Prefix + base + "/.well-known/smart-configuration"); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), errPASBaseNoPayer) {
			t.Fatalf("base %s: %d %s, want 404 naming no payer", base, rec.Code, rec.Body)
		}
	}
	env.originator.ingressAuth = nil
	if slices.Contains(env.originator.RoutePatternsForTest(), patternPASSmartConfigPayer) {
		t.Fatal("a gateway without its own ingress auth mounts the base's SMART configuration")
	}
}

// pasPayersOf lists the payers through handlePASPayers.
func pasPayersOf(t *testing.T, g *Gateway) (int, []pasPayer) {
	t.Helper()
	rec := httptest.NewRecorder()
	g.handlePASPayers(rec, httptest.NewRequest(http.MethodGet, PASPayersPath, nil))
	var doc struct {
		Payers []pasPayer `json:"payers"`
	}
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("listing %s: %v", rec.Body, err)
		}
		if doc.Payers == nil {
			t.Fatalf("listing %s, want a payers list", rec.Body)
		}
	}
	return rec.Code, doc.Payers
}

// GET /pas/payers lists every registered payer that takes the PAS subscribe
// leg, in participant id order, with its payer identifiers and its base: the
// address pasroute.Base builds, byte for byte. A participant that is no payer
// is not listed, whatever legs it declares, nor is a payer that does not take
// the leg; a registry with none lists none (an empty list, not an error). A
// base that cannot be built is an error, never a shorter list.
func TestPASPayers_ListsEachPayersBase(t *testing.T) {
	env := newInProcessExchange(t)
	g := env.originator
	g.cfg.IngressBaseURL = "https://provider.example.test/fhir"
	reg := g.cfg.Reg
	// Today's registrar: no payer can declare the leg, so none is listed.
	if code, payers := pasPayersOf(t, g); code != http.StatusOK || len(payers) != 0 {
		t.Fatalf("listing %d %+v, want none", code, payers)
	}
	payer, _ := reg.Lookup(env.payerID)
	payer.PayerIDs = []shnsdk.PayerIdentifier{shnsdk.CMSPayerIdentity}
	payer.Legs = append(slices.Clone(payer.Legs), legPASSubscriptionSubscribe)
	reg.Set(env.payerID, payer)
	// Registered after it and before it in id order, both taking the leg.
	reg.Set("a-payer", shnsdk.RegistryEntry{ID: "a-payer", Role: "payer", Legs: []string{legPASSubscriptionSubscribe}})
	reg.Set("z-payer", shnsdk.RegistryEntry{ID: "z-payer", Role: "payer", Legs: []string{"pas-claim", legPASSubscriptionSubscribe}})
	// A holder that is no payer is left out, whatever legs it declares.
	provider, _ := reg.Lookup("provider")
	provider.Legs = []string{legPASSubscriptionSubscribe}
	reg.Set("provider", provider)
	code, payers := pasPayersOf(t, g)
	var ids []string
	for _, p := range payers {
		ids = append(ids, p.Participant)
		want, err := pasroute.Base(g.cfg.IngressBaseURL, p.Participant)
		if err != nil || p.Base != want {
			t.Fatalf("%s listed at %q, want %q (%v)", p.Participant, p.Base, want, err)
		}
	}
	if code != http.StatusOK || !slices.Equal(ids, []string{"a-payer", env.payerID, "z-payer"}) {
		t.Fatalf("listing %d %v, want the three payers in id order", code, ids)
	}
	if got := payers[1].PayerIdentifiers; len(got) != 1 || got[0] != shnsdk.CMSPayerIdentity {
		t.Fatalf("%s listed with %+v, want its registered identifier", env.payerID, got)
	}
	if got := payers[0].PayerIdentifiers; got == nil || len(got) != 0 {
		t.Fatalf("a payer registering none is listed with %#v, want an empty list", got)
	}
	// A payer that does not take the leg is left out.
	for name, legs := range map[string][]string{"no list": nil, "an empty list": {}, "a list without the leg": {"pas-claim"}} {
		notTaking := payer
		notTaking.Legs = legs
		reg.Set(env.payerID, notTaking)
		if code, payers := pasPayersOf(t, g); code != http.StatusOK || len(payers) != 2 || slices.ContainsFunc(payers, func(p pasPayer) bool { return p.Participant == env.payerID }) {
			t.Fatalf("%s: listing %d %+v, want it left out", name, code, payers)
		}
	}
	// A base that cannot be built is an error, never a shorter list.
	reg.Set(env.payerID, payer)
	for _, base := range []string{"", "https://provider.example.test/./fhir"} {
		g.cfg.IngressBaseURL = base
		if code, _ := pasPayersOf(t, g); code != http.StatusServiceUnavailable {
			t.Fatalf("listing under ingress base %q: %d, want 503", base, code)
		}
	}
}

// GET /pas/payers takes the ingress's own authentication, as every route its
// system calls does: a caller with no bearer is refused 401.
func TestPASPayers_RefusesAnUnauthenticatedCaller(t *testing.T) {
	_, pub := newTestClientKey(t)
	g := gatewayWithAuth(t, "br-provider", pub)
	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, testIngressBaseURL+PASPayersPath, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET %s with no bearer: %d %s, want 401", PASPayersPath, rec.Code, rec.Body)
	}
}

// A Subscription names no payer, so the plain base cannot route one (N8): a
// create is refused 422, and a search, read, vread, history, instance
// $status, update, patch or delete 404,
// each with an OperationOutcome naming where the payers' bases are listed,
// recorded as the provider gateway's routing refusal on its own route, at
// every level, and nothing is sent. A claim at the plain base is the control:
// carried as before.
func TestPASSubscription_ThePlainBaseNamesNoPayer(t *testing.T) {
	calls := []struct {
		method, path string
		status       int
		code, msg    string
	}{
		{http.MethodPost, "/Subscription", http.StatusUnprocessableEntity, "not-supported", errPASSubscriptionPlainBase},
		{http.MethodGet, "/Subscription", http.StatusNotFound, "not-found", errPASSubscriptionNotFound},
		{http.MethodGet, "/Subscription?url=https://ehr.example/notify", http.StatusNotFound, "not-found", errPASSubscriptionNotFound},
		{http.MethodGet, "/Subscription/sub-1", http.StatusNotFound, "not-found", errPASSubscriptionNotFound},
		{http.MethodGet, "/Subscription/sub-1/_history/2", http.StatusNotFound, "not-found", errPASSubscriptionNotFound},
		{http.MethodGet, "/Subscription/sub-1/_history", http.StatusNotFound, "not-found", errPASSubscriptionNotFound},
		{http.MethodGet, "/Subscription/sub-1/$status", http.StatusNotFound, "not-found", errPASSubscriptionNotFound},
		{http.MethodGet, "/Subscription/$status", http.StatusNotFound, "not-found", errPASSubscriptionNotFound},
		{http.MethodPatch, "/Subscription/sub-1", http.StatusNotFound, "not-found", errPASSubscriptionNotFound},
		{http.MethodPut, "/Subscription/sub-1", http.StatusNotFound, "not-found", errPASSubscriptionNotFound},
		{http.MethodDelete, "/Subscription/sub-1", http.StatusNotFound, "not-found", errPASSubscriptionNotFound},
	}
	subscription := `{"resourceType":"Subscription","status":"requested","channel":{"type":"rest-hook","endpoint":"https://ehr.example/notify"}}`
	for _, c := range calls {
		for _, level := range allLevels {
			t.Run(c.method+" "+c.path+"/"+level.String(), func(t *testing.T) {
				env, got, _ := pasBaseExchange(t, level)
				var body string
				if c.method == http.MethodPost || c.method == http.MethodPut || c.method == http.MethodPatch {
					body = subscription
				}
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(c.method, c.path, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/fhir+json")
				env.originator.Handler().ServeHTTP(rec, req)
				refusedBeforeTheNetwork(t, env, rec, c.status, PASPayersPath)
				wantOutcome(t, rec, c.status, c.code, c.msg)
				x := got.only(t)
				wantRefusal(t, x, c.status, RefusedByProviderGateway, RefusalRouting)
				wantReasonInAnswer(t, x, rec.Body.String())
				if x.Route != RoutePASSubscription {
					t.Fatalf("recorded on route %q, want %q", x.Route, RoutePASSubscription)
				}
			})
		}
	}
	// The control: a claim at the plain base is routed by its Coverage and
	// carried.
	env, got, _ := pasBaseExchange(t, EnforcementStrict)
	env.payerReturns(LegResult{Response: testResponse(levelPASPayerAnswer)})
	rec := servePASBase(env, "/Claim/$submit", pasIngressBundle("00001", ""))
	if x := got.only(t); rec.Code != http.StatusOK || x.Recipient != env.payerID || env.routeHitCount() != 1 {
		t.Fatalf("control: plain claim %d %s to %s, want it carried to its Coverage's payer", rec.Code, rec.Body, x.Recipient)
	}
}

// The per-payer base's routes, its listing and the plain base's Subscription
// refusals are mounted on a provider gateway whose Da Vinci ingress is
// enabled, and on no other.
func TestPASPayerBase_MountedOnTheProviderIngressOnly(t *testing.T) {
	want := []string{patternPASSubmitPayer, patternPASInquirePayer, "GET " + PASPayersPath,
		"POST /Subscription", "GET /Subscription", "GET /Subscription/{rest...}",
		"PUT /Subscription/{id}", "PATCH /Subscription/{id}", "DELETE /Subscription/{id}"}
	env := newInProcessExchange(t)
	mounted := env.originator.RoutePatternsForTest()
	for _, p := range want {
		if !slices.Contains(mounted, p) {
			t.Errorf("the provider ingress does not mount %q", p)
		}
	}
	env.originator.cfg.IngressEnabled = false
	off := env.originator.RoutePatternsForTest()
	env.originator.cfg.IngressEnabled = true
	payer, _ := payerIngressExchange(t, EnforcementStrict)
	if slices.Contains(mounted, "GET /pas/payer/{payer}/metadata") {
		t.Error("the provider ingress mounts a payer's base's CapabilityStatement, which it does not build")
	}
	for i, p := range want {
		if slices.Contains(off, p) {
			t.Errorf("a provider gateway without its ingress mounts %q", p)
		}
		// A payer's plain-base Subscription routes are its own CDex
		// subscription ingress (cdex-subscription-ingress), not these: only
		// the PAS base's routes and listing (want[:3]) are held off it.
		if i < 3 && slices.Contains(payer.originator.RoutePatternsForTest(), p) {
			t.Errorf("a payer gateway mounts %q", p)
		}
	}
}

// The participant's tolerance of named payers (CONFORMANCE_TOLERATE_PEERS)
// applies at a payer's base to the payer that answers, the base's: a Coverage
// naming the base's own payer is matched by the identifier it names, as at
// the plain base; any other is matched by the identifiers the base's payer
// registered. So at strict a tolerated base payer's defective answer is
// carried whatever payer the Coverage names, and refused 502 when only the
// Coverage's payer is tolerated (the control).
func TestPASPayerBase_ToleranceAppliesToTheBasesPayer(t *testing.T) {
	basePayer := []shnsdk.PayerIdentifier{shnsdk.CMSPayerIdentity}
	coveragePayer := []shnsdk.PayerIdentifier{{System: shnsdk.CMSPayerIdentity.System, Value: otherPayerValue}}
	// A second identifier the base's payer registers, which the fixtures'
	// Coverage does not name.
	secondID := shnsdk.PayerIdentifier{System: shnsdk.CMSPayerIdentity.System, Value: "00003"}
	toB := func(t *testing.T, body string) string { return withPayorValue(t, body, otherPayerValue) }
	for _, leg := range carriedAnswerLegs() {
		for _, c := range []struct {
			name      string
			edit      func(t *testing.T, body string) string
			registers []shnsdk.PayerIdentifier
			tolerated []shnsdk.PayerIdentifier
			want      int
		}{
			// The base's payer registers two identifiers: at its own base,
			// the one the Coverage names decides, as at the plain base.
			{"the base's own payer by two identifiers, the named one tolerated", func(_ *testing.T, body string) string { return body },
				[]shnsdk.PayerIdentifier{shnsdk.CMSPayerIdentity, secondID}, basePayer, http.StatusOK},
			{"the base's own payer by two identifiers, only the other tolerated", func(_ *testing.T, body string) string { return body },
				[]shnsdk.PayerIdentifier{shnsdk.CMSPayerIdentity, secondID}, []shnsdk.PayerIdentifier{secondID}, http.StatusBadGateway},
			{"the base's own payer, tolerated", func(_ *testing.T, body string) string { return body }, nil, basePayer, http.StatusOK},
			{"another registered payer, the base's tolerated", toB, nil, basePayer, http.StatusOK},
			{"another registered payer, only it tolerated", toB, nil, coveragePayer, http.StatusBadGateway},
			{"an unresolved payer, the base's tolerated", func(t *testing.T, body string) string { return withPayorValue(t, body, "99999") }, nil, basePayer, http.StatusOK},
			{"the base's own payer, none tolerated", func(_ *testing.T, body string) string { return body }, nil, nil, http.StatusBadGateway},
		} {
			t.Run(leg.leg+"/"+c.name, func(t *testing.T) {
				env, got, _ := pasBaseExchange(t, EnforcementStrict)
				if c.registers != nil {
					reg := env.originator.cfg.Reg
					payer, _ := reg.Lookup(env.payerID)
					payer.PayerIDs = c.registers
					reg.Set(env.payerID, payer)
				}
				declareOriginationProfile(env.originator, "demo")
				env.originator.cfg.Validator = &shnsdk.FakeValidator{RejectIfContains: `"ClaimResponse"`}
				env.originator.cfg.ConformanceToleratePeers = c.tolerated
				env.payerReturns(LegResult{Response: testResponse(leg.answer(t))})
				rec := servePASBase(env, pasBasePath(leg, env.payerID), c.edit(t, leg.body(t)))
				got.only(t)
				if rec.Code != c.want || env.routeHitCount() != 1 {
					t.Fatalf("answer %d %s, %d network hits; want %d after carrying the claim once", rec.Code, rec.Body, env.routeHitCount(), c.want)
				}
			})
		}
	}
}
