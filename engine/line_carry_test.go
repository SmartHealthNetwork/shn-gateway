package engine

// line_carry_test.go — the network's default across IG lines (linecarry.go).
// By default every site carries the message as sent, whatever the two
// participants' lines, and records it, at every level. Under the
// participant's own setting (RequireSharedLine) each refuses the pair as
// routing, at every level.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// lineEvents collects the LineCarriedEvent events g raises, keeping any
// observer already set.
type lineEvents struct {
	mu  sync.Mutex
	evs []ObserverEvent
}

func captureLineEvents(g *Gateway) *lineEvents {
	got := &lineEvents{}
	prev := g.cfg.Observer
	g.cfg.Observer = func(e ObserverEvent) {
		if e.Kind == LineCarriedEvent {
			got.mu.Lock()
			got.evs = append(got.evs, e)
			got.mu.Unlock()
		}
		if prev != nil {
			prev(e)
		}
	}
	return got
}

// one returns the one event raised, failing on none or more.
func (l *lineEvents) one(t *testing.T) ObserverEvent {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.evs) != 1 {
		t.Fatalf("want one %s event, got %d: %+v", LineCarriedEvent, len(l.evs), l.evs)
	}
	return l.evs[0]
}

func (l *lineEvents) none(t *testing.T) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.evs) != 0 {
		t.Fatalf("want no %s event, got %+v", LineCarriedEvent, l.evs)
	}
}

// frameClaim is the contract line a request payload's frame claims ("" bare
// or unclaimed).
func frameClaim(t *testing.T, payload []byte) string {
	t.Helper()
	if !shnsdk.IsFramed(payload) {
		return ""
	}
	hdr, _, err := shnsdk.DecodeHTTPFrame(payload)
	if err != nil {
		t.Fatal(err)
	}
	return hdr.Headers[shnsdk.FrameHeaderContractVersion]
}

// requesterIngress is a provider ingress whose recipient's declaration
// decides the leg's line.
type requesterIngress struct {
	route    string
	contract string
	answer   func(*testing.T) []byte
	setup    func(*testing.T, *inProcessExchange)
	request  func(*testing.T, *inProcessExchange) *http.Request
}

func requesterIngresses() map[string]requesterIngress {
	dtrBody := ehrParams(ehrOrderParam("sr1", prefetchMember), ehrCoverageParam(prefetchMember, "00001"), dtrQuestionnaire)
	return map[string]requesterIngress{
		// The CDS Hooks ingress selects the route before it builds.
		"CRD": {RouteCRD, "pa.crd", realCRDAnswer, nil,
			func(t *testing.T, env *inProcessExchange) *http.Request { return env.crdIngressRequest(t) }},
		// The questionnaire-package ingress selects the route before it sends.
		"DTR": {RouteDTR, "pa.dtr", func(*testing.T) []byte { return packageAnswer }, func(t *testing.T, env *inProcessExchange) {
			env.originator.cfg.SoR = newPrefetchSoR().sor()
			declareFramedDTR(t, env, true)
		}, func(t *testing.T, env *inProcessExchange) *http.Request {
			return httptest.NewRequest(http.MethodPost, "/Questionnaire/$questionnaire-package", bytes.NewReader(dtrBody))
		}},
		// The PAS ingress carries the EHR's Bundle as sent; OriginateLeg
		// fills in its line.
		"PAS": {RoutePAS, "pa.pas", func(*testing.T) []byte { return levelPASPayerAnswer }, nil,
			func(t *testing.T, env *inProcessExchange) *http.Request {
				return httptest.NewRequest(http.MethodPost, "/Claim/$submit", strings.NewReader(pasIngressBundle("00001", "")))
			}},
	}
}

// runRequesterIngress drives one ingress with the recipient declaring tokens
// and returns the exchange, the answer and the one exchange record.
func runRequesterIngress(t *testing.T, row requesterIngress, level ConformanceEnforcement, require bool, tokens []string) (*inProcessExchange, *httptest.ResponseRecorder, ExchangeRecord, *lineEvents) {
	t.Helper()
	env := newInProcessExchange(t)
	env.originator.cfg.ConformanceEnforcement = level
	env.originator.cfg.RequireSharedLine = require
	declareRecipientVersions(t, env, tokens)
	declareRecipientRequestFrames(t, env)
	if row.setup != nil {
		row.setup(t, env)
	}
	env.payerReturns(LegResult{Response: testResponse(row.answer(t))})
	var got exchangeRecords
	env.originator.cfg.ExchangeObserved = got.observe
	ev := captureLineEvents(env.originator)
	rec := httptest.NewRecorder()
	env.originator.ingressRoute(row.route)(rec, row.request(t, env))
	return env, rec, got.only(t), ev
}

// A pair that shares no declared line, and that no native build or
// translation reaches, is carried at the requester's own line by default:
// the payer's side receives exactly what it would from a pair that shares
// that line, the answer is relayed, and the carry is recorded.
func TestLineCarry_RequesterCarriesAPairWithNoSharedLine(t *testing.T) {
	for name, row := range requesterIngresses() {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				control, crec, _, cev := runRequesterIngress(t, row, level, false, []string{row.contract + "@2.0"})
				if crec.Code != http.StatusOK {
					t.Fatalf("shared-line control %d %s", crec.Code, crec.Body)
				}
				cev.none(t)
				shared := control.lastRequestPayload()

				env, rec, x, ev := runRequesterIngress(t, row, level, false, []string{row.contract + "@9.9"})
				if rec.Code != http.StatusOK || rec.Body.String() != crec.Body.String() {
					t.Fatalf("answer %d %s, want the payer's answer relayed as with a shared line (%d %s)", rec.Code, rec.Body, crec.Code, crec.Body)
				}
				if env.routeHitCount() != 1 {
					t.Fatalf("network hits %d, want the request carried once", env.routeHitCount())
				}
				if sent := env.lastRequestPayload(); !bytes.Equal(sent, shared) {
					t.Fatalf("the payer's side received\n%s\nwant exactly what a shared-line pair sends\n%s", sent, shared)
				}
				if claim := frameClaim(t, env.lastRequestPayload()); claim != row.contract+"@2.0" {
					t.Fatalf("frame claims %q, want this gateway's own line %s@2.0", claim, row.contract)
				}
				if x.Outcome != ExchangeAnswered || !slices.Equal(x.LineCarried, []string{LineCarriedRequest}) {
					t.Fatalf("record %s, carried %v; want answered, carried [request]", x.Outcome, x.LineCarried)
				}
				e := ev.one(t)
				if e.Counterpart != env.payerID || !strings.Contains(e.Detail, row.contract+"@2.0") || !strings.Contains(e.Detail, row.contract+"@9.9") {
					t.Fatalf("event %+v, want the payer and both lines named", e)
				}
			})
		}
	}
}

// Under the participant's own setting the same pair is refused as routing
// before anything is sent, at every level.
func TestLineCarry_RequesterRefusesAPairWithNoSharedLineWhenRequired(t *testing.T) {
	for name, row := range requesterIngresses() {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env, rec, x, ev := runRequesterIngress(t, row, level, true, []string{row.contract + "@9.9"})
				if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "no shared contract line for "+row.contract) {
					t.Fatalf("answer %d %s, want 422 no shared contract line", rec.Code, rec.Body)
				}
				if env.routeHitCount() != 0 {
					t.Fatalf("a refused request crossed the network (%d)", env.routeHitCount())
				}
				wantRefusal(t, x, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalRouting)
				ev.none(t)
			})
		}
	}
}

// The setting never changes a pair that shares a line: it routes exactly as
// without it.
func TestLineCarry_SettingLeavesASharedLineAlone(t *testing.T) {
	for name, row := range requesterIngresses() {
		t.Run(name, func(t *testing.T) {
			off, offRec, _, _ := runRequesterIngress(t, row, EnforcementObserve, false, []string{row.contract + "@2.0"})
			on, onRec, x, ev := runRequesterIngress(t, row, EnforcementObserve, true, []string{row.contract + "@2.0"})
			if onRec.Code != http.StatusOK || onRec.Body.String() != offRec.Body.String() || !bytes.Equal(on.lastRequestPayload(), off.lastRequestPayload()) {
				t.Fatalf("with the setting: %d %s, without: %d %s", onRec.Code, onRec.Body, offRec.Code, offRec.Body)
			}
			if len(x.LineCarried) != 0 {
				t.Fatalf("a shared line is not a carry: %v", x.LineCarried)
			}
			ev.none(t)
		})
	}
}

// ---- a walk to the payer's line that would change the bytes ----

// swapStep makes contract's 2.0->2.1 up-step change the bytes it walks, for
// the length of the test.
func swapStep(t *testing.T, contract string) *int {
	t.Helper()
	idx := -1
	for i, s := range compatManifest {
		if s.Contract == contract && s.From == "2.0" && s.To == "2.1" {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("no %s 2.0->2.1 row", contract)
	}
	saved := compatManifest[idx]
	t.Cleanup(func() { compatManifest[idx] = saved })
	var walked int
	// The stand-in changes bytes but is not gated: module 2 plans a walk up
	// through it (a gated row taken upward is never planned, gatedUpward).
	compatManifest[idx].Class = StepFull
	compatManifest[idx].Up = func(p []byte, x ExchangeIdentity) ([]byte, LossReport, error) {
		walked++
		return append(bytes.Clone(p), '\n'), LossReport{Module: contract + " 2.0->2.1", Source: "2.0", Target: "2.1"}, nil
	}
	return &walked
}

// The CDS Hooks and questionnaire-package requests are the EHR's own bytes,
// and no certified translation of them exists: a walk that would change one is
// not applied. By default the request is carried as sent, at the line it is
// in, exactly as a pair sharing that line receives it; the setting refuses the
// pair as routing. The questionnaire-package walk never sees the bytes in
// production (envelopeEgressLegs); the row lets it, to reach the guard.
func TestLineCarry_NativeRequestAWalkWouldChangeIsCarriedAsSent(t *testing.T) {
	for name, row := range requesterIngresses() {
		if name == "PAS" {
			continue // the PAS ingress never walks the EHR's Bundle
		}
		for _, level := range allLevels {
			for _, require := range []bool{false, true} {
				t.Run(name+"/"+level.String()+"/"+map[bool]string{false: "default", true: "required"}[require], func(t *testing.T) {
					control, crec, _, _ := runRequesterIngress(t, row, level, false, []string{row.contract + "@2.0"})
					if crec.Code != http.StatusOK {
						t.Fatalf("shared-line control %d %s", crec.Code, crec.Body)
					}
					if name == "DTR" {
						walkSeesBytes(t, envelopeEgressLegs, "dtr-questionnaire-fetch")
					}
					walked := swapStep(t, row.contract)
					fake := shnsdk.NewFakeValidator()
					forceWalk := func(t *testing.T, env *inProcessExchange) {
						if row.setup != nil {
							row.setup(t, env)
						}
						env.originator.cfg.ValidatorsByContractLine = contractLanes(map[string]shnsdk.Validator{"2.0": fake, "2.1": fake})
						env.originator.cfg.EgressNativeLines = []string{"2.0"} // the 2.1 route walks the swapped step
						env.originator.cfg.TranslateIGLines = true
					}
					var transformed int
					walking := row
					walking.setup = func(t *testing.T, env *inProcessExchange) {
						forceWalk(t, env)
						prev := env.originator.cfg.Observer
						env.originator.cfg.Observer = func(e ObserverEvent) {
							if e.Kind == "leg.transformed" {
								transformed++
							}
							if prev != nil {
								prev(e)
							}
						}
					}
					env, rec, x, ev := runRequesterIngress(t, walking, level, require, []string{row.contract + "@2.1"})
					if *walked != 1 {
						t.Fatalf("the swapped step ran %d times, want 1", *walked)
					}
					if require {
						if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "cannot be carried to the payer's line unchanged") || env.routeHitCount() != 0 {
							t.Fatalf("answer %d %s (network hits %d), want 422 before the network", rec.Code, rec.Body, env.routeHitCount())
						}
						wantRefusal(t, x, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalRouting)
						return
					}
					if rec.Code != http.StatusOK {
						t.Fatalf("answer %d %s, want carried", rec.Code, rec.Body)
					}
					if sent := env.lastRequestPayload(); !bytes.Equal(sent, control.lastRequestPayload()) {
						t.Fatalf("the payer's side received\n%s\nwant the request as sent\n%s", sent, control.lastRequestPayload())
					}
					if !slices.Equal(x.LineCarried, []string{LineCarriedRequest}) {
						t.Fatalf("carried %v, want [request]", x.LineCarried)
					}
					if transformed != 0 {
						t.Fatalf("%d leg.transformed events for a walk that was not applied, want none", transformed)
					}
					ev.one(t)
				})
			}
		}
	}
}

// walkSeesBytes lets leg's walk run its steps over the bytes for the length
// of the test: by construction the questionnaire-package fetch and the PAS
// inquiry walks never see them (envelopeEgressLegs, verbatimChainLegs), so a
// walk on them cannot change a byte otherwise.
func walkSeesBytes(t *testing.T, exempt map[string]bool, leg string) {
	t.Helper()
	if !exempt[leg] {
		t.Fatalf("%s is not exempt; the row would not need this", leg)
	}
	delete(exempt, leg)
	t.Cleanup(func() { exempt[leg] = true })
}

// countTransformed counts the leg.transformed events g raises.
func countTransformed(g *Gateway) *int {
	n := new(int)
	prev := g.cfg.Observer
	g.cfg.Observer = func(e ObserverEvent) {
		if e.Kind == "leg.transformed" {
			*n++
		}
		if prev != nil {
			prev(e)
		}
	}
	return n
}

// A questionnaire request this gateway built (carryUnchanged): a walk to the
// payer's line that would change it is not applied and not recorded; by
// default the request goes at its build line, and the setting refuses it as
// routing. A walk that changes nothing is applied and recorded.
func TestLineCarry_BuiltQuestionnaireRequestAWalkWouldChange(t *testing.T) {
	fake := shnsdk.NewFakeValidator()
	route := legRoute{Token: "pa.dtr@2.1", BuildLine: "2.0", Chain: chainFor("pa.dtr", "2.0", "2.1")}
	if len(route.Chain) == 0 {
		t.Fatal("no pa.dtr 2.0->2.1 chain")
	}
	body := []byte(`{"resourceType":"Parameters"}`)
	newGateway := func(require bool) (*Gateway, *int) {
		g := &Gateway{cfg: Config{TranslateIGLines: true, HolderID: "provider", Clock: func() time.Time { return time.Unix(1700000000, 0).UTC() },
			ValidatorsByContractLine: contractLanes(map[string]shnsdk.Validator{"2.0": fake, "2.1": fake}), RequireSharedLine: require}}
		return g, countTransformed(g)
	}
	walkSeesBytes(t, envelopeEgressLegs, "dtr-questionnaire-fetch")

	g, transformed := newGateway(false)
	if sent, status, msg := g.carryUnchanged(route, body, "corr-1", "payer"); status != 0 || !reflect.DeepEqual(sent, route) || *transformed != 1 {
		t.Fatalf("an unchanged walk = %+v %d %q (%d transformed), want the route, recorded once", sent, status, msg, *transformed)
	}

	walked := swapStep(t, "pa.dtr")
	route.Chain = chainFor("pa.dtr", "2.0", "2.1") // the steps a chain holds are copied when it is built
	g, transformed = newGateway(false)
	sent, status, msg := g.carryUnchanged(route, body, "corr-1", "payer")
	if *walked != 1 || status != 0 || sent.Token != "pa.dtr@2.0" || sent.BuildLine != "2.0" || sent.Chain != nil || *transformed != 0 {
		t.Fatalf("default = %+v %d %q (walked %d, %d transformed), want carried at pa.dtr@2.0, nothing recorded", sent, status, msg, *walked, *transformed)
	}
	g, transformed = newGateway(true)
	if _, status, msg := g.carryUnchanged(route, body, "corr-1", "payer"); status != http.StatusUnprocessableEntity || !strings.Contains(msg, "cannot be carried to the payer's line unchanged") || *transformed != 0 {
		t.Fatalf("required = %d %q (%d transformed), want the 422 and nothing recorded", status, msg, *transformed)
	}
}

// The same through a workflow that builds and sends the questionnaire request
// (the CRD dispatch): the leg goes out at the build line, with no chain, and
// the carry is recorded, by default; under the setting the workflow is refused
// before the questionnaire request is sent.
func TestLineCarry_DispatchQuestionnaireAWalkWouldChange(t *testing.T) {
	const performerRef = "Organization/org-dme-ox"
	orderJSON, err := buildHomeOxygenDeviceRequest("dr-ox", "Patient/MBR-OX", performerRef)
	if err != nil {
		t.Fatal(err)
	}
	supplierJSON, err := buildHomeOxygenSupplier("org-dme-ox")
	if err != nil {
		t.Fatal(err)
	}
	demo := Demo{BirthDate: "1958-07-14", FamilyName: "Okafor-Oxygen"}
	fake := shnsdk.NewFakeValidator()
	for _, require := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "required"}[require], func(t *testing.T) {
			walkSeesBytes(t, envelopeEgressLegs, "dtr-questionnaire-fetch")
			walked := swapStep(t, "pa.dtr")
			var events []ObserverEvent
			fix := newDispatchFixtureWith(t, "MBR-OX", demo, orderJSON, performerRef, supplierJSON, func(cfg *Config) {
				cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
				cfg.OriginationProfile = ""
				cfg.RequireSharedLine = require
				cfg.DeclaredContractVersions = []string{shnsdk.ContractPACRD20, shnsdk.ContractPADTR20, shnsdk.ContractPAPAS20}
				cfg.ValidatorsByContractLine = contractLanes(map[string]shnsdk.Validator{"2.0": fake, "2.1": fake})
				cfg.EgressNativeLines = []string{"2.0"} // the payer's 2.1 is reached through the swapped step
				cfg.TranslateIGLines = true
				entry, ok := cfg.Reg.Lookup("payer")
				if !ok {
					t.Fatal("fixture registry has no payer entry")
				}
				entry.ContractVersions = []string{shnsdk.ContractPACRD20, "pa.dtr@2.1", shnsdk.ContractPAPAS20}
				cfg.Reg.Set("payer", entry)
			})
			rec := httptest.NewRecorder()
			fix.gw.handleDispatch(rec, httptest.NewRequest(http.MethodPost, "/scenario/dispatch", bytes.NewBufferString(testRouteBody(fix.gw, "/scenario/dispatch", `"member":"MBR-OX"`))))
			var sent []ObserverEvent
			carried := 0
			for _, e := range events {
				if e.LegType != "dtr-questionnaire-fetch" {
					continue
				}
				switch e.Kind {
				case "leg.originated":
					sent = append(sent, e)
				case LineCarriedEvent:
					carried++
				case "leg.transformed":
					t.Fatalf("a walk that was not applied was recorded: %+v", e)
				}
			}
			if *walked != 1 {
				t.Fatalf("the swapped step ran %d times, want 1 (%d %s)", *walked, rec.Code, rec.Body)
			}
			if require {
				if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "cannot be carried to the payer's line unchanged") || len(sent) != 0 {
					t.Fatalf("required = %d %s (%d questionnaire requests sent), want 422 before it is sent", rec.Code, rec.Body, len(sent))
				}
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("default = %d %s, want answered", rec.Code, rec.Body)
			}
			if len(sent) != 1 || sent[0].Route == nil || sent[0].Route.Token != "pa.dtr@2.0" || len(sent[0].Route.Chain) != 0 {
				t.Fatalf("questionnaire requests %+v, want one at its build line pa.dtr@2.0 with no chain", sent)
			}
			if carried != 1 {
				t.Fatalf("%d %s events for the questionnaire leg, want 1", carried, LineCarriedEvent)
			}
		})
	}
}

// The same through the CRD-then-DTR order workflow the use cases run.
func TestLineCarry_OrderQuestionnaireAWalkWouldChange(t *testing.T) {
	fake := shnsdk.NewFakeValidator()
	for _, require := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "required"}[require], func(t *testing.T) {
			walkSeesBytes(t, envelopeEgressLegs, "dtr-questionnaire-fetch")
			walked := swapStep(t, "pa.dtr")
			gw, stub, _ := crdTestSystem(t, shnsdk.CardCoverage{Covered: shnsdk.CoveredCovered, PANeeded: shnsdk.PANeededAuthNeeded, Questionnaires: []string{"http://example.org/q"}})
			var events []ObserverEvent
			gw.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
			gw.cfg.RequireSharedLine = require
			gw.cfg.DeclaredContractVersions = []string{shnsdk.ContractPACRD20, shnsdk.ContractPADTR20, shnsdk.ContractPAPAS20}
			gw.cfg.ValidatorsByContractLine = contractLanes(map[string]shnsdk.Validator{"2.0": fake, "2.1": fake})
			gw.cfg.EgressNativeLines = []string{"2.0"}
			gw.cfg.TranslateIGLines = true
			entry, ok := gw.cfg.Reg.Lookup("payer")
			if !ok {
				t.Fatal("payer not registered")
			}
			entry.ContractVersions = []string{shnsdk.ContractPACRD20, "pa.dtr@2.1", shnsdk.ContractPAPAS20}
			gw.cfg.Reg.Set("payer", entry)
			rec := callRunCRDThenDTROrder(t, gw, stub)
			var sent []ObserverEvent
			carried := 0
			for _, e := range events {
				if e.LegType != "dtr-questionnaire-fetch" {
					continue
				}
				switch e.Kind {
				case "leg.originated":
					sent = append(sent, e)
				case LineCarriedEvent:
					carried++
				case "leg.transformed":
					t.Fatalf("a walk that was not applied was recorded: %+v", e)
				}
			}
			if *walked != 1 {
				t.Fatalf("the swapped step ran %d times, want 1 (%d %s)", *walked, rec.Code, rec.Body)
			}
			if require {
				if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "cannot be carried to the payer's line unchanged") || len(sent) != 0 {
					t.Fatalf("required = %d %s (%d questionnaire requests sent), want 422 before it is sent", rec.Code, rec.Body, len(sent))
				}
				return
			}
			if len(sent) != 1 || sent[0].Route == nil || sent[0].Route.Token != "pa.dtr@2.0" || len(sent[0].Route.Chain) != 0 || carried != 1 {
				t.Fatalf("questionnaire requests %+v (%d carry events; answer %d %s), want one at its build line pa.dtr@2.0 with no chain, recorded", sent, carried, rec.Code, rec.Body)
			}
		})
	}
}

// The prior-authorization inquiry this gateway builds: a walk to the pend's
// line that would change it is not applied; by default the inquiry is sent as
// built, and the setting refuses it as routing before anything is sent.
func TestLineCarry_BuiltInquiryAWalkWouldChange(t *testing.T) {
	walkSeesBytes(t, verbatimChainLegs, "pas-claim-inquire")
	for _, require := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "required"}[require], func(t *testing.T) {
			walked := swapStep(t, "pa.pas")
			gw, stub := pendingPersonaSystem(t)
			fake := shnsdk.NewFakeValidator()
			gw.cfg.ValidatorsByContractLine = contractLanes(map[string]shnsdk.Validator{"2.0": fake, "2.1": fake})
			gw.cfg.EgressNativeLines = []string{"2.0"} // the 2.1 pin walks the swapped step
			gw.cfg.TranslateIGLines = true
			gw.cfg.RequireSharedLine = require
			// The payer reads request frames, so the line the inquiry claims is on the wire.
			entry, ok := gw.cfg.Reg.Lookup("payer")
			if !ok {
				t.Fatal("payer not registered")
			}
			entry.RequestFrames = []string{shnsdk.RequestFrameV1}
			gw.cfg.Reg.Set("payer", entry)
			transformed := countTransformed(gw)
			cont := pendingPersonaContinuation()
			cont.Line = "2.1"
			req := httptest.NewRequest(http.MethodPost, "/scenario/pa/inquire", nil)
			_, status, msg, _ := gw.inquireContinuation(req.Context(), req, cont)
			if *walked != 1 || *transformed != 0 {
				t.Fatalf("walked %d, %d transformed (%d %q), want one walk, not recorded", *walked, *transformed, status, msg)
			}
			if require {
				if status != http.StatusUnprocessableEntity || !strings.Contains(msg, "cannot be carried to the payer's line unchanged") || stub.sawInquiry {
					t.Fatalf("required = %d %q (sent %v), want 422 before anything is sent", status, msg, stub.sawInquiry)
				}
				return
			}
			if !stub.sawInquiry {
				t.Fatalf("default = %d %q, want the inquiry sent as built", status, msg)
			}
			sent := stub.sent["pas-claim-inquire"]
			if len(sent) != 1 || frameClaim(t, sent[0]) != "pa.pas@2.0" {
				t.Fatalf("the inquiry went %d times, claiming %q; want once, at its build line pa.pas@2.0", len(sent), frameClaim(t, sent[len(sent)-1]))
			}
		})
	}
}

// ---- the answer's line stamp ----

// An answer stamped with another line than the leg routed is relayed by
// default, at every level and on every provider ingress, and recorded.
func TestLevelProviderIngress_AnswerStampedWithAnotherLineIsRelayedAtEveryLevel(t *testing.T) {
	for name, row := range providerIngressStampRows() {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env := newInProcessExchange(t)
				env.originator.cfg.ConformanceEnforcement = level
				if row.setup != nil {
					row.setup(t, env)
				}
				env.payerReturns(LegResult{Response: testResponse(stampedAnswer(t, routedToken(t, env, row.leg), row.answer(t)))})
				ev := captureLineEvents(env.originator)
				rec := row.send(t, env)
				if rec.Code != http.StatusOK {
					t.Fatalf("answer %d %s, want relayed", rec.Code, rec.Body)
				}
				if strings.Contains(rec.Body.String(), stampMismatch) {
					t.Fatalf("the answer was refused: %s", rec.Body)
				}
				if e := ev.one(t); !strings.Contains(e.Detail, "answer stamped") {
					t.Fatalf("event %+v, want the answer's stamp named", e)
				}
			})
		}
	}
}

// This gateway's own workflow reads an answer stamped with another line by
// default too: it is returned, and recorded.
func TestLevelOriginate_AnswerStampedWithAnotherLineIsReturnedAtEveryLevel(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.ConformanceEnforcement = level
			ev := captureLineEvents(env.originator)
			sealBare(env, stampedAnswer(t, "pa.crd@2.0", []byte(`{"cards":[]}`)))
			got, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "crd-order-select", "pci-1", "corr-1", "",
				Content{WorkstreamType: workstreamPA, ProfileID: "pa.crd@2.0", Payload: testRequest(env.crdReq)})
			if err != nil || string(got) != `{"cards":[]}` {
				t.Fatalf("got %s %v, want the answer returned", got, err)
			}
			ev.one(t)
		})
	}
}

// The exchange record: the answer's stamp is recorded as a carry by default,
// and as a routing refusal under the setting.
func TestExchangeRecord_AnswerStampedWithAnotherLine(t *testing.T) {
	for _, require := range []bool{false, true} {
		env := newInProcessExchange(t)
		env.originator.cfg.RequireSharedLine = require
		var got exchangeRecords
		env.originator.cfg.ExchangeObserved = got.observe
		env.payerReturns(LegResult{Response: testResponse(stampedAnswer(t, "pa.crd@2.0", realCRDAnswer(t)))})
		rec := httptest.NewRecorder()
		env.originator.ingressRoute(RouteCRD)(rec, env.crdIngressRequest(t))
		x := got.only(t)
		if require {
			wantRefusal(t, x, http.StatusBadGateway, RefusedByProviderGateway, RefusalRouting)
			continue
		}
		if x.Outcome != ExchangeAnswered || !slices.Equal(x.LineCarried, []string{LineCarriedAnswer}) {
			t.Fatalf("record %s carried %v, want answered, carried [answer]", x.Outcome, x.LineCarried)
		}
	}
}

// ---- the responder: a request claiming a line it cannot answer at ----

// A framed request claiming a line the payer's gateway cannot build or
// validate at is carried to its participant by default, exactly as a bare
// request, at every level, and recorded; the setting refuses it as routing.
func TestLineCarry_ResponderCarriesAClaimItCannotAnswerAt(t *testing.T) {
	body := conformantCRD("MBR-COVERED", "72148")
	for _, level := range allLevels {
		for _, require := range []bool{false, true} {
			t.Run(level.String()+"/"+map[bool]string{false: "default", true: "required"}[require], func(t *testing.T) {
				p, got := newRecordingLevelPayer(t, level)
				p.g.cfg.RequireSharedLine = require
				ev := captureLineEvents(p.g)
				bare := p.sendFramed(t, "crd-order-select", nil, body, p.pci)
				control := p.partner.lastBody
				_ = got.only(t)
				ans := p.sendFramed(t, "crd-order-select", map[string]string{shnsdk.FrameHeaderContractVersion: "pa.crd@9.9"}, body, p.pci)
				x := got.only(t)
				if require {
					p.wantRefused(t, ans, http.StatusUnprocessableEntity, "pa.crd@9.9")
					wantRefusal(t, x, http.StatusUnprocessableEntity, RefusedByPayerGateway, RefusalRouting)
					ev.none(t)
					return
				}
				if ans.status != bare.status || !bytes.Equal(ans.body, bare.body) {
					t.Fatalf("answer %d %s, want the bare request's %d %s", ans.status, ans.body, bare.status, bare.body)
				}
				if !bytes.Equal(p.partner.lastBody, control) {
					t.Fatalf("the participant received\n%s\nwant what a bare request carries\n%s", p.partner.lastBody, control)
				}
				if !slices.Equal(x.LineCarried, []string{LineCarriedRequest}) || x.Outcome != ExchangeAnswered {
					t.Fatalf("record %s carried %v, want answered, carried [request]", x.Outcome, x.LineCarried)
				}
				if e := ev.one(t); !strings.Contains(e.Detail, "pa.crd@9.9") {
					t.Fatalf("event %+v, want the claim named", e)
				}
			})
		}
	}
}

// Each claim the responder cannot answer at — a line no build speaks, another
// contract's token, a line with only a certification client, a native line
// with no lane — is carried by default: answered at the line a bare request
// gets, with the claim named for the record. The same claims refused under
// the setting are lineselect_test.go's rows.
func TestLineCarry_UnanswerableClaimsAreCarriedByDefault(t *testing.T) {
	fake := shnsdk.NewFakeValidator()
	for name, row := range map[string]struct {
		lanes map[string]shnsdk.Validator
		claim string
	}{
		"unknown line":    {nil, "pa.pas@9.9"},
		"other contract":  {nil, "pa.crd@2.0"},
		"native, unlaned": {map[string]shnsdk.Validator{"2.0": fake}, "pa.pas@2.2"},
	} {
		t.Run(name, func(t *testing.T) {
			g := d9Gateway(row.lanes, nil)
			body := []byte(`{"resourceType":"Bundle"}`)
			got, tok, carried, status, msg := g.unframeRequestFrom("requester", "pas-claim", framedRequest(t, row.claim, body))
			if status != 0 || tok != "pa.pas@2.0" || carried != row.claim || !bytes.Equal(got, body) {
				t.Fatalf("(%s, %q, carried %q, %d, %q), want the body answered at pa.pas@2.0 with the claim carried", got, tok, carried, status, msg)
			}
		})
	}
	t.Run("certification client only", func(t *testing.T) {
		g := d9Gateway(map[string]shnsdk.Validator{"2.0": fake}, nil)
		g.cfg.CertificationValidatorsByLine = map[string]shnsdk.Validator{"2.0": fake, "2.2": fake}
		if _, tok, carried, status, _ := g.unframeRequestFrom("requester", "pas-claim", framedRequest(t, "pa.pas@2.2", []byte(`{}`))); status != 0 || tok != "pa.pas@2.0" || carried != "pa.pas@2.2" {
			t.Fatalf("(%q, carried %q, %d), want answered at pa.pas@2.0 with the claim carried", tok, carried, status)
		}
	})
}

// A claim on a version-neutral leg names no line the leg has: it is ignored,
// with or without the setting, as an answer's stamp is.
func TestLineCarry_ClaimOnAVersionNeutralLegIsIgnored(t *testing.T) {
	for _, require := range []bool{false, true} {
		g := d9Gateway(nil, nil)
		g.cfg.RequireSharedLine = require
		body := []byte(`{"resourceType":"Task"}`)
		got, tok, carried, status, msg := g.unframeRequestFrom("requester", "federated-query", framedRequest(t, "pa.pas@2.0", body))
		if status != 0 || tok != "" || carried != "" || !bytes.Equal(got, body) {
			t.Fatalf("require=%v: (%s, %q, %d, %q), want the body with no line", require, got, tok, status, msg)
		}
	}
}

// A leg carried between lines in two places on one side — a claim the payer's
// gateway cannot answer at, then its forward to a partner sharing no line —
// names "request" once in its record, and raises both events.
func TestLineCarry_RecordNamesEachPlaceOnce(t *testing.T) {
	p := newLevelPayer(t, EnforcementObserve, WithDeclaredContractVersions([]string{"pa.crd@2.2"}))
	var got exchangeRecords
	p.g.cfg.ExchangeObserved = got.observe
	ev := captureLineEvents(p.g)
	ans := p.sendFramed(t, "crd-order-select", map[string]string{shnsdk.FrameHeaderContractVersion: "pa.crd@9.9"}, conformantCRD("MBR-COVERED", "72148"), p.pci)
	if ans.status != http.StatusOK {
		t.Fatalf("answer %d %s, want carried and answered", ans.status, ans.body)
	}
	if x := got.only(t); !slices.Equal(x.LineCarried, []string{LineCarriedRequest}) {
		t.Fatalf("carried %v, want [request] once", x.LineCarried)
	}
	ev.mu.Lock()
	n := len(ev.evs)
	ev.mu.Unlock()
	if n != 2 {
		t.Fatalf("%d %s events, want one for the claim and one for the forward", n, LineCarriedEvent)
	}
}

// ---- the payer's forward to a partner declaring other lines ----

// A payer gateway forwarding to a Da Vinci partner whose declared lines share
// none with its own forwards the request as sent by default, at every level,
// exactly as to a partner that declares nothing, and records it.
func TestLineCarry_NativeForwardCarriesAPartnerWithNoSharedLine(t *testing.T) {
	body := conformantCRD("MBR-COVERED", "72148")
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			silent := newLevelPayer(t, level)
			want := silent.send(t, "crd-order-select", "", body)
			forwarded := silent.partner.lastBody

			p := newLevelPayer(t, level, WithDeclaredContractVersions([]string{"pa.crd@2.2"}))
			var got exchangeRecords
			p.g.cfg.ExchangeObserved = got.observe
			ev := captureLineEvents(p.g)
			ans := p.send(t, "crd-order-select", "", body)
			if ans.status != want.status || !bytes.Equal(ans.body, want.body) {
				t.Fatalf("answer %d %s, want %d %s", ans.status, ans.body, want.status, want.body)
			}
			if !bytes.Equal(p.partner.lastBody, forwarded) {
				t.Fatalf("the partner received\n%s\nwant\n%s", p.partner.lastBody, forwarded)
			}
			if x := got.only(t); !slices.Equal(x.LineCarried, []string{LineCarriedRequest}) {
				t.Fatalf("carried %v, want [request]", x.LineCarried)
			}
			if e := ev.one(t); !strings.Contains(e.Detail, "pa.crd@2.2") {
				t.Fatalf("event %+v, want the partner's line named", e)
			}
		})
	}
}

// A line a peer's frame states is named in records only in a token's shape.
func TestLineCarry_PeerStatedLineIsBounded(t *testing.T) {
	for in, want := range map[string]string{
		"pa.pas@2.2":                      "pa.pas@2.2",
		"pa.pas@9.9.9":                    "pa.pas@9.9.9",
		"pa.pas@2.2; injected text":       "sha256:" + sha256hex([]byte("pa.pas@2.2; injected text")),
		strings.Repeat("a", 200) + "@2.0": "sha256:" + sha256hex([]byte(strings.Repeat("a", 200)+"@2.0")),
		"":                                "sha256:" + sha256hex(nil),
	} {
		if got := boundedLine(in); got != want {
			t.Errorf("boundedLine(%q) = %q, want %q", in, got, want)
		}
	}
}
