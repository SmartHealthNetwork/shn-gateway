//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// serveRecorded serves route ("METHOD /path") on g's Handler, as mounted,
// with body, and returns the answer and every exchange record the call
// produced.
func serveRecorded(t *testing.T, g *Gateway, route, body string) (*httptest.ResponseRecorder, []ExchangeRecord) {
	t.Helper()
	var got exchangeRecords
	g.cfg.ExchangeObserved = got.observe
	defer func() { g.cfg.ExchangeObserved = nil }()
	method, path, _ := strings.Cut(route, " ")
	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	got.mu.Lock()
	defer got.mu.Unlock()
	for _, r := range got.recs {
		wantClosed(t, r)
	}
	return rec, got.recs
}

// mountOriginationRoutes mounts module 1's routes, as its state keeps them,
// on a fixture gateway that drives the hermetic personas with no origination
// profile (the profile picks the member a route resolves: sceneMember). The
// routes are declared for a lane, as New declares them, and the fixture's own
// profile is then put back, so each route serves exactly as its handler did
// when the fixture called it directly.
func mountOriginationRoutes(g *Gateway) {
	profile := g.cfg.OriginationProfile
	declareOriginationProfile(g, "demo")
	g.cfg.OriginationProfile = profile
}

// onlyRecord is the one record a call produced.
func onlyRecord(t *testing.T, recs []ExchangeRecord) ExchangeRecord {
	t.Helper()
	if len(recs) != 1 {
		t.Fatalf("want exactly one exchange record, got %d: %+v", len(recs), recs)
	}
	return recs[0]
}

// wantOrigination asserts the record is an origination route's own: direction
// ingress, the route's closed name, this gateway the sender.
func wantOrigination(t *testing.T, r ExchangeRecord, pattern string, status int) {
	t.Helper()
	if r.Direction != DirectionIngress || r.Route != originationExchangeRoute(pattern) || r.Sender != "provider" || r.Status != status {
		t.Fatalf("record direction %q route %q sender %q status %d, want ingress %q from provider, status %d",
			r.Direction, r.Route, r.Sender, r.Status, originationExchangeRoute(pattern), status)
	}
}

// A refusal on an origination route records its real rule: a member whose
// Coverage names a payer no participant registers is a routing refusal by
// this gateway, before any leg. The route is uc02, whose no-PA CRD body
// (originateNoPACRD) uc02-unknownpayer shares: that route's persona,
// MBR-UNKNOWN-PAYER, is a seeded persona the hermetic system of record does
// not hold, so the payer directory here registers no payer for the member's
// own Coverage instead (00001; it registers only 00078).
func TestOriginationRecord_UnregisteredPayerIsRouting(t *testing.T) {
	g, stub, _ := crdTestSystem(t, uc03Coverage())
	declareOriginationProfile(g, "demo")
	router, err := NewConfigPayerRouter([]PayerDirectoryEntry{{System: shnsdk.CMSPayerIdentity.System, Value: "00078", HolderID: "payer"}})
	if err != nil {
		t.Fatal(err)
	}
	g.cfg.PayerRouter = router
	const route = "POST /scenario/uc02"
	rec, recs := serveRecorded(t, g, route, testRouteBody(g, "/scenario/uc02", ""))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "no registered payer") {
		t.Fatalf("%s = %d %s, want 422 naming no registered payer", route, rec.Code, rec.Body.String())
	}
	if len(stub.legTypes) != 0 {
		t.Fatalf("legs %v sent before the routing refusal", stub.legTypes)
	}
	r := onlyRecord(t, recs)
	wantOrigination(t, r, route, http.StatusUnprocessableEntity)
	wantRefusal(t, r, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalRouting)
	if r.Exchange != ExchangeOther || r.Recipient != "" {
		t.Fatalf("exchange %q recipient %q, want none: no leg was sent", r.Exchange, r.Recipient)
	}
}

// A successful origination records answered, with the first leg it sent and
// its two holders: uc02, the no-PA coverage check, answered covered.
func TestOriginationRecord_SuccessRecordsTheLeg(t *testing.T) {
	g, stub, _ := crdTestSystem(t, shnsdk.CardCoverage{Covered: shnsdk.CoveredCovered, PANeeded: shnsdk.PANeededNoAuth})
	declareOriginationProfile(g, "demo")
	aimStubAt(t, stub, "MBR-D-UC02")
	const route = "POST /scenario/uc02"
	rec, recs := serveRecorded(t, g, route, testRouteBody(g, "/scenario/uc02", ""))
	if rec.Code != http.StatusOK || len(stub.legTypes) == 0 {
		t.Fatalf("%s = %d %s (legs %v), want 200 after a leg", route, rec.Code, rec.Body.String(), stub.legTypes)
	}
	r := onlyRecord(t, recs)
	wantOrigination(t, r, route, http.StatusOK)
	if r.Outcome != ExchangeAnswered || r.Exchange != stub.legTypes[0] || r.Exchange != "crd-order-select" || r.Recipient != "payer" {
		t.Fatalf("record %s exchange %q recipient %q, want answered on the first leg sent (%v) to payer", r.Outcome, r.Exchange, r.Recipient, stub.legTypes)
	}
	if !sha256Hex.MatchString(r.RequestCiphertextHash) || r.ContractLine == "" {
		t.Fatalf("request hash %q line %q, want the sealed leg's", r.RequestCiphertextHash, r.ContractLine)
	}
}

// advancingHub advances the gateway's injected clock by step on each request
// to the Hub's route, so a leg's answer takes step with no real wait.
type advancingHub struct {
	next http.RoundTripper
	now  *time.Time
	step time.Duration
}

func (a advancingHub) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/route") {
		*a.now = a.now.Add(a.step)
	}
	return a.next.RoundTrip(req)
}

// An origination whose leg's answer takes 30 s (the clock injected, nothing
// slept) records that latency, which its access line carries as latencyMs,
// and the first leg's correlation id (the answer names no leg id of its
// own). The metrics leave that latency out of the exchange and stage
// durations (app TestExchangeMetricHook_OriginationLatencyIsNotADuration).
func TestOriginationRecord_LatencyAndCorrelationAreTheFirstLegs(t *testing.T) {
	g, stub, _ := crdTestSystem(t, shnsdk.CardCoverage{Covered: shnsdk.CoveredCovered, PANeeded: shnsdk.PANeededNoAuth})
	declareOriginationProfile(g, "demo")
	aimStubAt(t, stub, "MBR-D-UC02")
	now := g.cfg.Clock()
	g.cfg.Clock = func() time.Time { return now }
	g.cfg.Client = &http.Client{Transport: advancingHub{next: stub, now: &now, step: 30 * time.Second}}
	var minted []string
	g.cfg.CorrelationGen = func() string {
		id := fmt.Sprintf("leg-corr-%d", len(minted)+1)
		minted = append(minted, id)
		return id
	}
	rec, recs := serveRecorded(t, g, "POST /scenario/uc02", testRouteBody(g, "/scenario/uc02", ""))
	if rec.Code != http.StatusOK || len(stub.legTypes) != 1 {
		t.Fatalf("uc02 = %d %s (legs %v), want 200 after one leg", rec.Code, rec.Body.String(), stub.legTypes)
	}
	r := onlyRecord(t, recs)
	if r.Latency != 30*time.Second {
		t.Fatalf("latency %v, want the injected 30s", r.Latency)
	}
	if len(minted) == 0 || r.CorrelationID != minted[0] {
		t.Fatalf("correlation %q, want the first leg's (%v)", r.CorrelationID, minted)
	}
}

// The PHG's UC-07 completion, /scenario/uc07/complete, records its exchange:
// the patient-DTR exchange it sends first, answered.
func TestOriginationRecord_UC07CompletionRecords(t *testing.T) {
	t.Parallel()
	g, stub := uc07RouteFixture(t, "2.2")
	mountOriginationRoutes(g)
	start, recs := serveRecorded(t, g, "POST /scenario/uc07/start", testRouteBody(g, "/scenario/uc07/start", ""))
	var started startResp
	if start.Code != http.StatusOK || json.Unmarshal(start.Body.Bytes(), &started) != nil || started.ResumeToken == "" {
		t.Fatalf("start %d %s", start.Code, start.Body.String())
	}
	wantOrigination(t, onlyRecord(t, recs), "POST /scenario/uc07/start", http.StatusOK)
	before := len(stub.legTypes)
	const route = "POST /scenario/uc07/complete"
	rec, recs := serveRecorded(t, g, route, testRouteBody(g, "/scenario/uc07/complete", `"resumeToken":"`+started.ResumeToken+`"`))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s = %d %s, want 200", route, rec.Code, rec.Body.String())
	}
	sent := stub.legTypes[before:]
	r := onlyRecord(t, recs)
	wantOrigination(t, r, route, http.StatusOK)
	if r.Outcome != ExchangeAnswered || len(sent) == 0 || r.Exchange != sent[0] || r.Exchange != "patient-dtr" {
		t.Fatalf("record %s exchange %q, want answered on the first leg the completion sent (%v)", r.Outcome, r.Exchange, sent)
	}
}

// The Hub refusing an origination's leg on its audit rule is recorded as the
// Hub's refusal on audit, never as a failure or as unreachable.
func TestOriginationRecord_HubAuditRefusal(t *testing.T) {
	g, stub, _ := uc03HandlerSystem(t, uc03Coverage())
	g.cfg.Client = &http.Client{Transport: hubAnswers{next: stub, status: http.StatusBadGateway, reason: "audit append failed", delivered: "no"}}
	const route = "POST /scenario/uc03"
	rec, recs := serveRecorded(t, g, route, testRouteBody(g, "/scenario/uc03", ""))
	if rec.Code/100 == 2 {
		t.Fatalf("%s = %d %s, want the Hub's refusal answered", route, rec.Code, rec.Body.String())
	}
	r := onlyRecord(t, recs)
	wantOrigination(t, r, route, rec.Code)
	wantRefusal(t, r, rec.Code, RefusedByHub, RefusalAudit)
	if r.Exchange != "crd-order-dispatch" || r.HubDelivered != "no" {
		t.Fatalf("exchange %q hubDelivered %q, want the refused leg and the Hub's word", r.Exchange, r.HubDelivered)
	}
}

// A request stating claim facts for an order the member does not hold is
// refused before anything is built: a fidelity refusal at the build stage.
func TestOriginationRecord_StatedOrderMismatchIsBuildStage(t *testing.T) {
	g, stub := uc07RouteFixture(t, "2.0")
	mountOriginationRoutes(g)
	start, _ := serveRecorded(t, g, "POST /scenario/uc07/start", testRouteBody(g, "/scenario/uc07/start", ""))
	var started startResp
	if start.Code != http.StatusOK || json.Unmarshal(start.Body.Bytes(), &started) != nil || started.ResumeToken == "" {
		t.Fatalf("start %d %s", start.Code, start.Body.String())
	}
	before := len(stub.legTypes)
	body := editBody(t, testRouteBody(g, "/scenario/uc07/complete", `"resumeToken":"`+started.ResumeToken+`"`), func(m map[string]json.RawMessage) {
		m["order"] = json.RawMessage(`{"system":"http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets","code":"J3490"}`)
	})
	const route = "POST /scenario/uc07/complete"
	rec, recs := serveRecorded(t, g, route, body)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "fidelity of SHN's own build") {
		t.Fatalf("%s = %d %s, want the 422 fidelity refusal", route, rec.Code, rec.Body.String())
	}
	// The amendment is the leg that consumes the stated order; UC-07's
	// patient-DTR exchange, which needs none of it, is sent first and stands.
	if sent := stub.legTypes[before:]; !slices.Equal(sent, []string{"patient-dtr"}) {
		t.Fatalf("legs %v sent before the refusal, want the patient-DTR exchange alone", sent)
	}
	wantCarried(t, rec.Body.String(), "patient-dtr")
	r := onlyRecord(t, recs)
	wantOrigination(t, r, route, http.StatusUnprocessableEntity)
	wantRefusal(t, r, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalFidelity)
	if r.RefusalStage != vocab.RefusalStageBuild {
		t.Fatalf("stage %q, want %s: nothing was built", r.RefusalStage, vocab.RefusalStageBuild)
	}

	// A request naming no order states facts for none: the amendment's build
	// cannot carry them either, the same fidelity refusal at the same point.
	before = len(stub.legTypes)
	body = editBody(t, testRouteBody(g, "/scenario/uc07/complete", `"resumeToken":"`+started.ResumeToken+`"`), func(m map[string]json.RawMessage) { delete(m, "order") })
	rec, recs = serveRecorded(t, g, route, body)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `the request names no \"order\"`) {
		t.Fatalf("no order: %d %s, want 422 naming the absent order", rec.Code, rec.Body.String())
	}
	if sent := stub.legTypes[before:]; !slices.Equal(sent, []string{"patient-dtr"}) {
		t.Fatalf("no order: legs %v sent before the refusal, want the patient-DTR exchange alone", sent)
	}
	r = onlyRecord(t, recs)
	wantRefusal(t, r, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalFidelity)
	if r.RefusalStage != vocab.RefusalStageBuild {
		t.Fatalf("no order: stage %q, want %s", r.RefusalStage, vocab.RefusalStageBuild)
	}
}

// A bridged message failing its $validate is an edit this gateway built
// failing its own verification: a fidelity refusal at the verify stage.
func TestExchangeRecord_BridgedRefusalIsVerifyStage(t *testing.T) {
	g, _ := observeGateway(t, EnforcementStrict, nil)
	var got exchangeRecords
	g.cfg.ExchangeObserved = got.observe
	g.recordExchange(RouteDTR, DirectionIngress, func(w http.ResponseWriter, r *http.Request) {
		g.emitFindingIn(r.Context(), ConformanceFinding{Kind: string(KindFHIRBridged), Decision: Refuse.String()})
		w.WriteHeader(http.StatusUnprocessableEntity)
	})(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))
	r := got.only(t)
	wantRefusal(t, r, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalFidelity)
	if r.RefusalStage != vocab.RefusalStageVerify {
		t.Fatalf("stage %q, want %s", r.RefusalStage, vocab.RefusalStageVerify)
	}
}

// The fence over the mount table: every route on-behalf building's table
// declares originating is recorded (its exchange route is one of
// ExchangeRoutes, and serving it produces one record under that name, an
// unauthenticated caller's refusal included), and no route it declares local
// is. Both sets are read from the table itself.
func TestOriginationRecord_EveryOriginatingRouteAndNoLocalOne(t *testing.T) {
	t.Parallel()
	var originating, local []string
	for _, rt := range originationRouteTable {
		if rt.originates {
			originating = append(originating, rt.pattern)
		} else {
			local = append(local, rt.pattern)
		}
	}
	if len(originating) == 0 || len(local) == 0 {
		t.Fatalf("the table declares %d originating and %d local routes; the fence needs both", len(originating), len(local))
	}
	if want := append(append([]string(nil), originatingRoutes...), localScenarioRoutes...); !slices.Equal(slices.Sorted(slices.Values(append(append([]string(nil), originating...), local...))), slices.Sorted(slices.Values(want))) {
		t.Fatalf("the table declares %v and %v, the classified routes are %v", originating, local, want)
	}
	for _, p := range originating {
		if !slices.Contains(ExchangeRoutes, originationExchangeRoute(p)) {
			t.Errorf("%s: its exchange route %q is not one of ExchangeRoutes", p, originationExchangeRoute(p))
		}
	}
	for _, p := range local {
		if slices.Contains(ExchangeRoutes, originationExchangeRoute(p)) {
			t.Errorf("%s is local, but ExchangeRoutes names %q", p, originationExchangeRoute(p))
		}
	}
	for _, authenticated := range []bool{false, true} {
		for _, p := range append(append([]string(nil), originating...), local...) {
			name := p
			if authenticated {
				name += " (scenario callers configured)"
			}
			t.Run(name, func(t *testing.T) {
				g, _, _ := crdTestSystem(t, uc03Coverage())
				declareOriginationProfile(g, "demo")
				if authenticated {
					pub, _ := genED25519(t)
					g.cfg.ScenarioCallers = map[string]ed25519.PublicKey{"operator": pub}
				}
				rec, recs := serveRecorded(t, g, p, `{}`)
				if !slices.Contains(originating, p) {
					if len(recs) != 0 {
						t.Fatalf("local route %s produced records %+v", p, recs)
					}
					return
				}
				r := onlyRecord(t, recs)
				wantOrigination(t, r, p, rec.Code)
				if authenticated {
					// No assertion: refused before the route runs, on
					// authentication.
					wantRefusal(t, r, http.StatusUnauthorized, RefusedByProviderGateway, RefusalAuthentication)
				}
			})
		}
	}
}

// A fidelity refusal's stage says whose fault it is: a caller's declaration
// the gateway does not honour (a native client naming a builder) is the
// build stage, the caller's to fix; the gateway's own seal of a module
// client's request failing (the builder table refusing the body) is the
// verify stage, SHN's to fix, which the refused-fidelity alarm counts.
func TestFidelityStage_DeclarationIsBuildAndAFailedSealIsVerify(t *testing.T) {
	var got exchangeRecords
	g := &Gateway{cfg: Config{ExchangeObserved: got.observe, HolderID: "provider"}}
	declared := httptest.NewRequest(http.MethodPost, "/cds-services/shn-order-select", strings.NewReader("{}"))
	declared.Header.Set(BuilderHeader, string(relay.BuilderSDKCRDRequest))
	w := httptest.NewRecorder()
	g.recordExchange(RouteCRD, DirectionIngress, func(w http.ResponseWriter, r *http.Request) {
		if _, ok := g.ingressBuilder(w, r, "crd-order-select"); ok {
			t.Fatal("a native client naming a builder was admitted")
		}
	})(w, declared)
	r := got.only(t)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), errBuilderNative) {
		t.Fatalf("declaration: %d %s", w.Code, w.Body.String())
	}
	wantRefusal(t, r, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalFidelity)
	if r.RefusalStage != vocab.RefusalStageBuild {
		t.Fatalf("declaration refused at %q, want %s", r.RefusalStage, vocab.RefusalStageBuild)
	}

	body := []byte("not one JSON document")
	w = httptest.NewRecorder()
	g.recordExchange(RouteCRD, DirectionIngress, func(w http.ResponseWriter, _ *http.Request) {
		if _, ok := sealModuleRequest(w, relay.BuilderSDKCRDRequest, relay.Exact(relay.NewBody(body, relay.OriginPeerFrame), "application/json"), body); ok {
			t.Fatal("a body the builder table refuses was sealed")
		}
	})(w, httptest.NewRequest(http.MethodPost, "/cds-services/shn-order-select", nil))
	r = got.only(t)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "could not be sealed as") {
		t.Fatalf("seal: %d %s", w.Code, w.Body.String())
	}
	wantRefusal(t, r, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalFidelity)
	if r.RefusalStage != vocab.RefusalStageVerify {
		t.Fatalf("failed seal refused at %q, want %s", r.RefusalStage, vocab.RefusalStageVerify)
	}
}

// coverageLessSoR is a participant system of record that holds the member but
// no Coverage for them.
type coverageLessSoR struct {
	SystemOfRecord
	ContextSystemOfRecord
}

func (coverageLessSoR) OpenCoverageContext(context.Context, string) ([][]byte, error) {
	return nil, nil
}

// A member the participant's own system holds no coverage for cannot be
// routed: no payer can be addressed, so every origination route refuses on
// routing before any leg, at every conformance level (a network rule), with
// its answer unchanged: 422 "no coverage on file for member". The routes
// below reach each read that answers it: the eligibility read (uc01), the
// no-prior-authorization CRD (uc02), the CRD-then-DTR order (uc04) and the
// CRD dispatch (uc03 and dispatch).
func TestOriginationRecord_NoCoverageOnFileIsRouting(t *testing.T) {
	crd := func(t *testing.T) (*Gateway, func() []string) {
		g, stub, _ := crdTestSystem(t, uc03Coverage())
		declareOriginationProfile(g, "demo")
		return g, func() []string { return stub.legTypes }
	}
	dispatch := func(t *testing.T) (*Gateway, func() []string) {
		fix := newMBROXDispatchFixture(t)
		mountOriginationRoutes(fix.gw)
		return fix.gw, func() []string { return fix.stub.legTypes }
	}
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict} {
		for _, row := range []struct {
			route, extra string
			fixture      func(*testing.T) (*Gateway, func() []string)
		}{
			{"POST /scenario/uc01", `"branch":"covered"`, crd},
			{"POST /scenario/uc02", "", crd},
			{"POST /scenario/uc03", "", crd},
			{"POST /scenario/uc04", "", crd},
			{"POST /scenario/dispatch", `"member":"MBR-OX"`, dispatch},
		} {
			t.Run(level.String()+"/"+row.route, func(t *testing.T) {
				g, legs := row.fixture(t)
				g.cfg.ConformanceEnforcement = level
				_, path, _ := strings.Cut(row.route, " ")
				body := testRouteBody(g, path, row.extra)
				g.cfg.SoR = coverageLessSoR{SystemOfRecord: g.cfg.SoR, ContextSystemOfRecord: ReadSystemOfRecord(g.cfg.SoR)}
				rec, recs := serveRecorded(t, g, row.route, body)
				if rec.Code != http.StatusUnprocessableEntity || strings.TrimSpace(rec.Body.String()) != `{"error":"no coverage on file for member"}` {
					t.Fatalf("%s = %d %s, want 422 no coverage on file for member", row.route, rec.Code, rec.Body.String())
				}
				if sent := legs(); len(sent) != 0 {
					t.Fatalf("legs %v sent before the routing refusal", sent)
				}
				r := onlyRecord(t, recs)
				wantOrigination(t, r, row.route, http.StatusUnprocessableEntity)
				wantRefusal(t, r, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalRouting)
			})
		}
	}
}

// A member whose Coverage names a payer identity two payer participants
// claim cannot be routed to one recipient: the router fails closed, and the
// origination is refused on routing before any leg, at every conformance
// level, as an unregistered payer is.
func TestOriginationRecord_AmbiguousPayerIsRouting(t *testing.T) {
	t.Parallel()
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict} {
		t.Run(level.String(), func(t *testing.T) {
			g, stub, _ := crdTestSystem(t, uc03Coverage())
			declareOriginationProfile(g, "demo")
			g.cfg.ConformanceEnforcement = level
			claim := []shnsdk.PayerIdentifier{{System: shnsdk.CMSPayerIdentity.System, Value: "00001"}}
			for _, id := range []string{"payer", "payer-b"} {
				e, _ := g.cfg.Reg.Lookup(id)
				e.ID, e.Role, e.PayerIDs = id, "payer", claim
				g.cfg.Reg.Set(id, e)
			}
			g.cfg.PayerRouter = NewFeedPayerRouter(g.cfg.Reg)
			const route = "POST /scenario/uc02"
			rec, recs := serveRecorded(t, g, route, testRouteBody(g, "/scenario/uc02", ""))
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "no registered payer") {
				t.Fatalf("%s = %d %s, want 422 naming no registered payer", route, rec.Code, rec.Body.String())
			}
			if len(stub.legTypes) != 0 {
				t.Fatalf("legs %v sent before the routing refusal", stub.legTypes)
			}
			r := onlyRecord(t, recs)
			wantOrigination(t, r, route, http.StatusUnprocessableEntity)
			wantRefusal(t, r, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalRouting)

			// One claimant: the same member's leg is routed and sent, so the
			// refusal above is the ambiguity's. (What the stub answers it is
			// not this row's subject.)
			e, _ := g.cfg.Reg.Lookup("payer-b")
			e.PayerIDs = nil
			g.cfg.Reg.Set("payer-b", e)
			if rec, _ = serveRecorded(t, g, route, testRouteBody(g, "/scenario/uc02", "")); len(stub.legTypes) == 0 {
				t.Fatalf("one claimant: %s = %d %s, no leg sent; want the leg routed", route, rec.Code, rec.Body.String())
			}
		})
	}
}

// withoutClaimFact removes one claim fact from a route body.
func withoutClaimFact(t *testing.T, body, fact string) string {
	t.Helper()
	return editBody(t, body, func(m map[string]json.RawMessage) {
		var facts map[string]json.RawMessage
		if err := json.Unmarshal(m["claimFacts"], &facts); err != nil {
			t.Fatal(err)
		}
		delete(facts, fact)
		b, err := json.Marshal(facts)
		if err != nil {
			t.Fatal(err)
		}
		m["claimFacts"] = b
	})
}

// A prior-authorization request the caller states without a claim fact the
// line requires is SHN refusing to build what it cannot build faithfully: a
// fidelity refusal at the build stage, at every conformance level, with the
// answer unchanged (422 naming the element). Each route refuses it at the PAS
// claim's build (pasBuildFailure, or the shared tail's own build), after the
// CRD and questionnaire legs, which were carried and stand, as the answer
// says.
func TestOriginationRecord_UnstatedClaimFactIsBuildStage(t *testing.T) {
	t.Parallel()
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict} {
		for _, row := range []struct{ route, extra string }{
			{"POST /scenario/uc03", ""},
			{"POST /scenario/uc04", ""},
			{"POST /scenario/uc07/start", ""},
			{"POST /scenario/dispatch", `"member":"MBR-OX"`},
		} {
			route := row.route
			t.Run(level.String()+"/"+route, func(t *testing.T) {
				var g *Gateway
				switch route {
				case "POST /scenario/dispatch":
					g = newMBROXDispatchFixture(t).gw
				case "POST /scenario/uc03":
					g = uc03ProviderDataFixture(t).gw
				case "POST /scenario/uc04":
					g, _ = newPendResumeFixture(t, pendFixtureOpts{member: "MBR-UC04", birthDate: "1982-11-03", familyName: "Chen", pendedItem: "operative-diagnostic-report"})
				default:
					g, _ = uc07RouteFixture(t, "2.0")
				}
				profile := g.cfg.OriginationProfile
				mountOriginationRoutes(g)
				g.cfg.OriginationProfile = profile
				g.cfg.ConformanceEnforcement = level
				answered := recordAnsweredLegs(g)
				_, path, _ := strings.Cut(route, " ")
				rec, recs := serveRecorded(t, g, route, withoutClaimFact(t, testRouteBody(g, path, row.extra), "claimType"))
				if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Claim.type") {
					t.Fatalf("%s = %d %s, want 422 naming Claim.type", route, rec.Code, rec.Body.String())
				}
				legs := answered.list()
				if len(legs) == 0 || slices.Contains(legs, "pas-claim") {
					t.Fatalf("%s: legs %v answered, want the legs before the claim and no claim", route, legs)
				}
				wantCarried(t, rec.Body.String(), legs...)
				r := onlyRecord(t, recs)
				wantOrigination(t, r, route, http.StatusUnprocessableEntity)
				wantRefusal(t, r, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalFidelity)
				if r.RefusalStage != vocab.RefusalStageBuild {
					t.Fatalf("stage %q, want %s: the caller stated no fact to build from", r.RefusalStage, vocab.RefusalStageBuild)
				}
			})
		}
	}
}

// An amendment whose recorded submission cannot be carried as the requester
// submitted it is not built: the fidelity of SHN's own edit, at the build
// stage, at every conformance level, answered 422 in the SDK's own words, with
// no amendment sent. Each row breaks the record of a real pend, so the real
// builder refuses it through the real route; the control completes.
func TestOriginationRecord_APriorClaimThatCannotBeCarriedIsBuildStage(t *testing.T) {
	t.Parallel()
	editClaim := func(t *testing.T, claim []byte, edit func(map[string]json.RawMessage)) []byte {
		t.Helper()
		var m map[string]json.RawMessage
		if err := json.Unmarshal(claim, &m); err != nil {
			t.Fatal(err)
		}
		edit(m)
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict} {
		for _, line := range []string{"2.0", "2.2"} {
			for _, row := range []struct {
				name string
				edit func(*testing.T, *shnsdk.PASPriorClaim)
				want string
			}{
				{"a record with no Claim", func(_ *testing.T, p *shnsdk.PASPriorClaim) { p.Claim = nil }, "resubmit the request instead of amending it"},
				{"a Claim with no items", func(t *testing.T, p *shnsdk.PASPriorClaim) {
					p.Claim = editClaim(t, p.Claim, func(m map[string]json.RawMessage) { delete(m, "item") })
				}, "no items"},
				{"a Claim with no id", func(t *testing.T, p *shnsdk.PASPriorClaim) {
					p.Claim = editClaim(t, p.Claim, func(m map[string]json.RawMessage) { delete(m, "id") })
				}, "no id"},
				{"control", nil, ""},
			} {
				t.Run(level.String()+"/"+line+"/"+row.name, func(t *testing.T) {
					g, stub := uc07RouteFixture(t, line)
					mountOriginationRoutes(g)
					g.cfg.ConformanceEnforcement = level
					start, _ := serveRecorded(t, g, "POST /scenario/uc07/start", testRouteBody(g, "/scenario/uc07/start", ""))
					var started startResp
					if start.Code != http.StatusOK || json.Unmarshal(start.Body.Bytes(), &started) != nil || started.ResumeToken == "" {
						t.Fatalf("start %d %s", start.Code, start.Body.String())
					}
					if row.edit != nil {
						pending := &g.moduleOneState().pending
						pending.mu.Lock()
						st := pending.byToken[started.ResumeToken]
						row.edit(t, &st.submitted.prior)
						pending.byToken[started.ResumeToken] = st
						pending.mu.Unlock()
					}
					before := len(stub.requests["pas-claim-update"])
					rec, recs := serveRecorded(t, g, "POST /scenario/uc07/complete",
						testRouteBody(g, "/scenario/uc07/complete", `"resumeToken":"`+started.ResumeToken+`"`))
					sent := len(stub.requests["pas-claim-update"]) - before
					r := onlyRecord(t, recs)
					if row.edit == nil {
						if rec.Code != http.StatusOK || sent != 1 {
							t.Fatalf("control: %d %s, %d amendments, want 200 and one", rec.Code, rec.Body.String(), sent)
						}
						return
					}
					if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), row.want) ||
						!strings.Contains(rec.Body.String(), shnsdk.ErrPASPriorClaimNotCarried.Error()[:40]) {
						t.Fatalf("%d %s, want 422 in the SDK's words naming %q", rec.Code, rec.Body.String(), row.want)
					}
					if sent != 0 {
						t.Fatalf("%d amendments sent after the refusal", sent)
					}
					wantRefusal(t, r, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalFidelity)
					if r.RefusalStage != vocab.RefusalStageBuild {
						t.Fatalf("stage %q, want %s", r.RefusalStage, vocab.RefusalStageBuild)
					}
				})
			}
		}
	}
}

// From PAS 2.1 the prior Claim is carried whole, so a reference the requester
// itself stated in a claim fact must be one the amendment can carry as sent.
// On the demo lane the submission carried it as stated, so the caller's own
// statement is what cannot be carried: stage build, at every level, no
// amendment sent, and the pend is kept.
func TestOriginationRecord_AStatedReferenceTheAmendmentCannotCarryIsBuildStage(t *testing.T) {
	t.Parallel()
	stated := func(m map[string]json.RawMessage) {
		var f map[string]json.RawMessage
		_ = json.Unmarshal(m["claimFacts"], &f)
		var coding map[string]json.RawMessage
		_ = json.Unmarshal(f["requestType"], &coding)
		coding["extension"] = json.RawMessage(`[{"url":"http://example.org/stated","valueReference":{"reference":"Observation/stated"}}]`)
		f["requestType"], _ = json.Marshal(coding)
		m["claimFacts"], _ = json.Marshal(f)
	}
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict} {
		for _, line := range []string{"2.1", "2.2"} {
			t.Run(level.String()+"/"+line, func(t *testing.T) {
				opts := pendFixtureOpts{member: "MBR-D-UC07", birthDate: "1985-10-23", familyName: "Kowalczyk", pendedItem: "patient-reported-functional-status",
					extraRoles: map[string]string{"phg": "phg"}, declared: []string{shnsdk.ContractPACRD21, shnsdk.ContractPADTR21, shnsdk.ContractPAPAS21}}
				if line == "2.2" {
					opts.declared = []string{shnsdk.ContractPACRD22, shnsdk.ContractPADTR22, shnsdk.ContractPAPAS22}
				}
				g, stub := newPendResumeFixture(t, opts)
				declareOriginationProfile(g, "demo")
				g.cfg.Validator = NewLineFakeValidator("2.0")
				g.cfg.ValidatorsByContractLine = contractLanes(map[string]shnsdk.Validator{"2.0": NewLineFakeValidator("2.0"), line: NewLineFakeValidator(line)})
				g.cfg.ConformanceEnforcement = level
				start, _ := serveRecorded(t, g, "POST /scenario/uc07/start", editBody(t, testRouteBody(g, "/scenario/uc07/start", ""), stated))
				var started startResp
				if start.Code != http.StatusOK || json.Unmarshal(start.Body.Bytes(), &started) != nil || started.ResumeToken == "" {
					t.Fatalf("start %d %s", start.Code, start.Body.String())
				}
				before := len(stub.requests["pas-claim-update"])
				rec, recs := serveRecorded(t, g, "POST /scenario/uc07/complete",
					testRouteBody(g, "/scenario/uc07/complete", `"resumeToken":"`+started.ResumeToken+`"`))
				if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "references what the amendment's bundle does not carry: Observation/stated") {
					t.Fatalf("%d %s, want 422 naming the reference", rec.Code, rec.Body.String())
				}
				if sent := len(stub.requests["pas-claim-update"]) - before; sent != 0 {
					t.Fatalf("%d amendments sent after the refusal", sent)
				}
				r := onlyRecord(t, recs)
				wantRefusal(t, r, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalFidelity)
				if r.RefusalStage != vocab.RefusalStageBuild {
					t.Fatalf("stage %q, want %s: the caller stated it", r.RefusalStage, vocab.RefusalStageBuild)
				}
				if _, ok := g.loadPending(started.ResumeToken); !ok {
					t.Fatal("the refusal dropped the pend, which the payer still holds")
				}
			})
		}
	}
}

// claimEntries is every Claim a Bundle's entries carry, as its bytes hold it.
func claimEntries(t *testing.T, bundle []byte) []json.RawMessage {
	t.Helper()
	var b struct {
		Entry []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(bundle, &b); err != nil {
		t.Fatal(err)
	}
	var claims []json.RawMessage
	for _, e := range b.Entry {
		var r struct {
			ResourceType string `json:"resourceType"`
		}
		if err := json.Unmarshal(e.Resource, &r); err != nil {
			t.Fatal(err)
		}
		if r.ResourceType == "Claim" {
			claims = append(claims, e.Resource)
		}
	}
	return claims
}

// From PAS 2.1 an amendment's prior Claim is the requester's own: what the
// payer receives as the prior-Claim entry is byte-identical to the Claim the
// submission's leg sent it, never a restatement, on the demo lane a deployed
// gateway runs as well as with no profile, at every conformance level, for a
// fact stated with characters JSON can encode more than one way.
func TestAmendment_ThePriorClaimOnTheWireIsTheSubmittedClaim(t *testing.T) {
	t.Parallel()
	stated := func(m map[string]json.RawMessage) {
		var f map[string]json.RawMessage
		_ = json.Unmarshal(m["claimFacts"], &f)
		var coding map[string]json.RawMessage
		_ = json.Unmarshal(f["requestType"], &coding)
		coding["display"] = json.RawMessage(`"a < b & caf\u00e9 \ud83d\ude00 > c"`)
		f["requestType"], _ = json.Marshal(coding)
		m["claimFacts"], _ = json.Marshal(f)
	}
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict} {
		for _, lane := range []string{"no profile", "demo"} {
			for _, line := range []string{"2.1", "2.2"} {
				t.Run(level.String()+"/"+lane+"/"+line, func(t *testing.T) {
					opts := pendFixtureOpts{member: "MBR-UC07", birthDate: "1990-08-25", familyName: "Haddad", pendedItem: "patient-reported-functional-status",
						extraRoles: map[string]string{"phg": "phg"}, declared: []string{shnsdk.ContractPACRD21, shnsdk.ContractPADTR21, shnsdk.ContractPAPAS21}}
					if lane == "demo" {
						opts.member, opts.birthDate, opts.familyName = "MBR-D-UC07", "1985-10-23", "Kowalczyk"
					}
					if line == "2.2" {
						opts.declared = []string{shnsdk.ContractPACRD22, shnsdk.ContractPADTR22, shnsdk.ContractPAPAS22}
					}
					g, stub := newPendResumeFixture(t, opts)
					if lane == "demo" {
						declareOriginationProfile(g, "demo")
					} else {
						mountOriginationRoutes(g)
					}
					g.cfg.Validator = NewLineFakeValidator("2.0")
					g.cfg.ValidatorsByContractLine = contractLanes(map[string]shnsdk.Validator{"2.0": NewLineFakeValidator("2.0"), line: NewLineFakeValidator(line)})
					g.cfg.ConformanceEnforcement = level
					start, _ := serveRecorded(t, g, "POST /scenario/uc07/start", editBody(t, testRouteBody(g, "/scenario/uc07/start", ""), stated))
					var started startResp
					if start.Code != http.StatusOK || json.Unmarshal(start.Body.Bytes(), &started) != nil || started.ResumeToken == "" {
						t.Fatalf("start %d %s", start.Code, start.Body.String())
					}
					rec, _ := serveRecorded(t, g, "POST /scenario/uc07/complete",
						editBody(t, testRouteBody(g, "/scenario/uc07/complete", `"resumeToken":"`+started.ResumeToken+`"`), stated))
					if rec.Code != http.StatusOK {
						t.Fatalf("complete %d %s", rec.Code, rec.Body.String())
					}
					submissions, amendments := stub.requests["pas-claim"], stub.requests["pas-claim-update"]
					if len(submissions) != 1 || len(amendments) != 1 {
						t.Fatalf("%d submissions and %d amendments on the wire, want one of each", len(submissions), len(amendments))
					}
					sent, amended := claimEntries(t, submissions[0]), claimEntries(t, amendments[0])
					if len(sent) != 1 || len(amended) != 2 {
						t.Fatalf("%d Claims in the submission and %d in the amendment, want 1 and 2", len(sent), len(amended))
					}
					if !bytes.Equal(amended[1], sent[0]) {
						t.Fatalf("the prior Claim on the wire is not the submitted Claim:\n%s\n%s", amended[1], sent[0])
					}
				})
			}
		}
	}
}

// twoCoverageSoR is a participant system of record that holds, beside the
// member's own Coverage, a second active one: extra.
type twoCoverageSoR struct {
	SystemOfRecord
	ContextSystemOfRecord
	extra []byte
}

func (s twoCoverageSoR) OpenCoverageContext(ctx context.Context, member string) ([][]byte, error) {
	covs, err := s.ContextSystemOfRecord.OpenCoverageContext(ctx, member)
	if err != nil {
		return nil, err
	}
	return append(covs, s.extra), nil
}

// A member whose own Coverage records do not name one payer (two that name
// different payers, or one whose payer cannot be read) cannot be routed to
// one recipient: every origination route refuses on routing before any leg,
// at every conformance level, with its answer unchanged. The routes reach
// each origination read: eligibility (uc01), the no-prior-authorization CRD
// (uc02), the CRD dispatch (uc03) and the CRD-then-DTR order (uc04).
func TestOriginationRecord_AmbiguousCoverageIsRouting(t *testing.T) {
	t.Parallel()
	const ambiguous = `{"error":"ambiguous coverage for routing: the member's Coverage records do not name one payer"}`
	for _, level := range allLevels {
		for _, extra := range []struct{ name, coverage string }{
			{"another payer", `{"resourceType":"Coverage","id":"cov-other","status":"active","beneficiary":{"reference":"Patient/MBR-COVERED"},"payor":[{"identifier":{"system":"` + shnsdk.CMSPayerIdentity.System + `","value":"99999"}}]}`},
			{"no payer", `{"resourceType":"Coverage","id":"cov-none","status":"active","beneficiary":{"reference":"Patient/MBR-COVERED"}}`},
		} {
			for _, route := range []string{"POST /scenario/uc01", "POST /scenario/uc02", "POST /scenario/uc03", "POST /scenario/uc04"} {
				t.Run(level.String()+"/"+extra.name+"/"+route, func(t *testing.T) {
					g, stub, _ := crdTestSystem(t, uc03Coverage())
					declareOriginationProfile(g, "demo")
					g.cfg.ConformanceEnforcement = level
					_, path, _ := strings.Cut(route, " ")
					extraBody := ""
					if route == "POST /scenario/uc01" {
						extraBody = `"branch":"covered"`
					}
					body := testRouteBody(g, path, extraBody)
					g.cfg.SoR = twoCoverageSoR{SystemOfRecord: g.cfg.SoR, ContextSystemOfRecord: ReadSystemOfRecord(g.cfg.SoR), extra: []byte(extra.coverage)}
					rec, recs := serveRecorded(t, g, route, body)
					if rec.Code != http.StatusUnprocessableEntity || strings.TrimSpace(rec.Body.String()) != ambiguous {
						t.Fatalf("%s = %d %s, want 422 %s", route, rec.Code, rec.Body.String(), ambiguous)
					}
					if len(stub.legTypes) != 0 {
						t.Fatalf("legs %v sent before the routing refusal", stub.legTypes)
					}
					r := onlyRecord(t, recs)
					wantOrigination(t, r, route, http.StatusUnprocessableEntity)
					wantRefusal(t, r, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalRouting)
				})
			}
		}
	}
}
