//go:build !shn_coreonly

package engine

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/scenarioauth"
)

// The verified caller reaches the route: scenario caller authentication puts
// the caller it admitted into the request context, so a statement the request
// makes about its own clinician can rank by who verifiably made it. A gateway
// with no callers runs its routes unauthenticated and carries no caller; with
// callers, an unsigned request is refused by authentication and never reaches
// the route.
func TestScenarioCaller_CarriesTheVerifiedCaller(t *testing.T) {
	pub, priv := callerKey(t)
	route := func(seen *[]scenarioauth.Caller, reached *bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			*reached = true
			if c, ok := scenarioauth.CallerFrom(r.Context()); ok {
				*seen = append(*seen, c)
			}
			w.WriteHeader(http.StatusOK)
		}
	}

	g, _, _ := scenarioCallerProvider(t, map[string]ed25519.PublicKey{"provider-app": pub})
	var seen []scenarioauth.Caller
	var reached bool
	rec := httptest.NewRecorder()
	g.requireScenarioCaller(route(&seen, &reached))(rec, signedScenario(t, "POST /scenario/uc06", `{}`, "provider-app", priv, g.cfg.Clock()))
	if rec.Code != http.StatusOK || len(seen) != 1 || seen[0].Label != "provider-app" {
		t.Fatalf("verified: %d, callers seen %+v, want the one verified caller", rec.Code, seen)
	}

	seen, reached = nil, false
	rec = httptest.NewRecorder()
	g.requireScenarioCaller(route(&seen, &reached))(rec, httptest.NewRequest(http.MethodPost, "/scenario/uc06", strings.NewReader(`{"npi":"1111111112"}`)))
	wantUnauthorized(t, "unsigned", rec)
	if reached {
		t.Fatal("an unsigned request reached the route")
	}

	open, _, _ := scenarioCallerProvider(t, nil)
	seen, reached = nil, false
	rec = httptest.NewRecorder()
	open.requireScenarioCaller(route(&seen, &reached))(rec, httptest.NewRequest(http.MethodPost, "/scenario/uc06", strings.NewReader(`{}`)))
	if !reached || len(seen) != 0 {
		t.Fatalf("no callers configured: reached %v, callers %+v, want the route reached with no verified caller", reached, seen)
	}
}

// REJECTION, at every level: an origination whose attesting clinician no
// source names is refused at the build of the leg that carries the
// attestation, 422, recorded as that leg failing, after the legs before it,
// which were carried and stand. UC-03's oxygen order attests item 6.1 of its
// PAS claim, after the order-dispatch and questionnaire legs. A request no
// source names even the ordering clinician for is refused at the first leg
// that names one, the CRD request itself, with no leg sent (UC-06's single
// call and UC-02's order-select). UC-06's amendment refusing for its attesting
// clinician is TestUC06TwoPhase_TheClinicianIsResolvedAtComplete's.
func TestOrigination_NoClinicianRefusedAtTheLegThatNamesIt(t *testing.T) {
	for _, level := range allLevels {
		t.Run("uc03 oxygen attestation/"+level.String(), func(t *testing.T) {
			fix := uc03DemoOxygenFixture(t)
			gw, stub := fix.gw, fix.stub
			gw.cfg.NPI, gw.cfg.ConformanceEnforcement = "", level
			var events []ObserverEvent
			gw.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
			rec := httptest.NewRecorder()
			gw.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/scenario/uc03", strings.NewReader(testRouteBody(gw, "/scenario/uc03", `"branch":""`))))
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), msgNoAttestingClinician) {
				t.Fatalf("%d %s, want 422 %q", rec.Code, rec.Body, msgNoAttestingClinician)
			}
			if !legAttempted(stub.legTypes, "crd-order-dispatch") || legAttempted(stub.legTypes, "pas-claim") {
				t.Fatalf("legs %v: want the order-dispatch leg sent and no claim", stub.legTypes)
			}
			wantCarried(t, rec.Body.String(), stub.legTypes...)
			wantOwnBuildRefusal(t, events, msgNoAttestingClinician)
		})
		t.Run("uc06 single call/"+level.String(), func(t *testing.T) {
			gw, stub := uc06RouteFixture(t, "2.0")
			gw.cfg.NPI, gw.cfg.ConformanceEnforcement = "", level
			var events []ObserverEvent
			gw.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
			rec := httptest.NewRecorder()
			gw.handleUC06(mounted(rec, withOriginationClaimInput(withLegsTracked(httptest.NewRequest(http.MethodPost, "/scenario/uc06", strings.NewReader(testRouteBody(gw, "/scenario/uc06", "")))))))
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), errNoOrderingClinician.Error()) {
				t.Fatalf("%d %s, want 422 %q", rec.Code, rec.Body, errNoOrderingClinician)
			}
			if len(stub.requests) != 0 {
				t.Fatalf("legs sent before the refusal: %v", stub.requests)
			}
			wantCarried(t, rec.Body.String())
			wantOwnBuildRefusal(t, events, errNoOrderingClinician.Error())
		})
		t.Run("uc02 order-select/"+level.String(), func(t *testing.T) {
			sor, _ := originSystem(t, prefetchMember)
			g, payer := originGateway(t, "provider-data", sor, nil, func(string, []byte) (int, []byte) {
				t.Fatal("a leg was sent")
				return 0, nil
			})
			g.cfg.NPI, g.cfg.ConformanceEnforcement = "", level
			var events []ObserverEvent
			g.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
			rec := httptest.NewRecorder()
			g.originateNoPACRD(rec, httptest.NewRequest(http.MethodPost, "/scenario/uc02", strings.NewReader(testRouteBody(g, "/scenario/uc02", ""))), prefetchMember)
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "build order-select request failed: order ") ||
				!strings.Contains(rec.Body.String(), errNoOrderingClinician.Error()) {
				t.Fatalf("%d %s, want 422 naming the missing ordering clinician", rec.Code, rec.Body)
			}
			if n := len(payer.sent("crd-order-select")); n != 0 {
				t.Fatalf("%d requests sent", n)
			}
			wantOwnBuildRefusal(t, events, errNoOrderingClinician.Error())
		})
		t.Run("uc04 order-sign/"+level.String(), func(t *testing.T) {
			gw, stub := newPendResumeFixture(t, pendFixtureOpts{member: "MBR-UC04", birthDate: "1982-11-03", familyName: "Chen", pendedItem: "operative-diagnostic-report"})
			gw.cfg.NPI, gw.cfg.ConformanceEnforcement = "", level
			var events []ObserverEvent
			gw.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
			rec := httptest.NewRecorder()
			gw.handleUC04(rec, withOriginationClaimInput(httptest.NewRequest(http.MethodPost, "/scenario/uc04", strings.NewReader(testRouteBody(gw, "/scenario/uc04", "")))))
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "build order-sign request failed: order ServiceRequest/") ||
				!strings.Contains(rec.Body.String(), errNoOrderingClinician.Error()) {
				t.Fatalf("%d %s, want 422 naming the missing ordering clinician", rec.Code, rec.Body)
			}
			if len(stub.requests) != 0 {
				t.Fatalf("legs sent before the refusal: %v", stub.requests)
			}
			wantOwnBuildRefusal(t, events, errNoOrderingClinician.Error())
		})
	}
}

// wantOwnBuildRefusal asserts a leg.failed event naming msg.
func wantOwnBuildRefusal(t *testing.T, events []ObserverEvent, msg string) {
	t.Helper()
	for _, e := range events {
		if e.Kind == "leg.failed" && strings.Contains(e.Detail, msg) {
			return
		}
	}
	t.Fatalf("no leg.failed naming %q in %+v", msg, events)
}

// UC-06 two-phase: /start reads no npi and never refuses for the attesting
// clinician, since a later /complete may state one (its order-sign request
// still needs an ordering clinician, here the record's). A /complete no
// source names an attesting NPI for is refused before its amendment leg and
// leaves the pend parked; a corrected /complete resumes it, with the clinician
// it states.
func TestUC06TwoPhase_TheClinicianIsResolvedAtComplete(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) { testUC06TwoPhase(t, level) })
	}
}

func testUC06TwoPhase(t *testing.T, level ConformanceEnforcement) {
	gw, stub := uc06RouteFixture(t, "2.0")
	gw.cfg.NPI, gw.cfg.ConformanceEnforcement = "", level
	var events []ObserverEvent
	gw.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
	// The order names its clinician, whose record carries no NPI: the
	// order-sign request names it, and the attestation has no NPI to use yet.
	gw.cfg.SoR = &requesterCensusSoR{censusSoR: gw.cfg.SoR.(*censusSoR), reads: map[string]int{}, requester: "Practitioner/dr-nonpi",
		records: map[string][]byte{"Practitioner/dr-nonpi": []byte(`{"resourceType":"Practitioner","id":"dr-nonpi","name":[{"family":"NoNPI"}]}`)}}
	start := startUC06(gw, testRouteBody(gw, "/scenario/uc06/start", ""))
	var started startResp
	if start.Code != http.StatusOK || json.Unmarshal(start.Body.Bytes(), &started) != nil || started.ResumeToken == "" {
		t.Fatalf("start %d %s, want a pend with no clinician yet", start.Code, start.Body)
	}
	complete := func(extra string) *httptest.ResponseRecorder {
		body := testRouteBody(gw, "/scenario/uc06/complete", `"resumeToken":"`+started.ResumeToken+`"`+extra)
		rec := httptest.NewRecorder()
		gw.handleUC06Complete(rec, withOriginationClaimInput(httptest.NewRequest(http.MethodPost, "/scenario/uc06/complete", strings.NewReader(body))))
		return rec
	}
	if rec := complete(""); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), msgNoAttestingClinician) {
		t.Fatalf("complete with no clinician: %d %s, want 422", rec.Code, rec.Body)
	}
	wantOwnBuildRefusal(t, events, msgNoAttestingClinician)
	if n := len(stub.requests["pas-claim-update"]); n != 0 {
		t.Fatalf("%d amendments sent after the refusal", n)
	}
	if _, ok := gw.loadPending(started.ResumeToken); !ok {
		t.Fatal("the refused /complete dropped the pend")
	}
	if rec := complete(`,"npi":"1111111112"`); rec.Code != http.StatusOK {
		t.Fatalf("corrected complete %d %s", rec.Code, rec.Body)
	}
	if n := len(stub.requests["pas-claim-update"]); n != 1 || !strings.Contains(string(stub.requests["pas-claim-update"][0]), "1111111112") {
		t.Fatalf("%d amendments; want one attested by the stated clinician", n)
	}
}

// REJECTION: an attestation route whose request states no answer is refused
// at the leg that carries the answer — UC-06's amendment, UC-07's patient-DTR
// exchange — after the legs before it, which were carried and stand (on the
// single calls, the coverage check, the questionnaire and the submission the
// payer pended), and the pend stays parked: the answer is the participant's,
// and the gateway supplies none.
func TestAttestation_AnswerIsRequired(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) { testAnswerIsRequired(t, level) })
	}
}

func testAnswerIsRequired(t *testing.T, level ConformanceEnforcement) {
	const want = "answer is required: the request states no answer for questionnaire item "
	observe := func(gw *Gateway) *[]ObserverEvent {
		var events []ObserverEvent
		gw.cfg.ConformanceEnforcement = level
		gw.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
		return &events
	}
	t.Run("uc06 single call", func(t *testing.T) {
		gw, stub := uc06RouteFixture(t, "2.0")
		events := observe(gw)
		rec := httptest.NewRecorder()
		gw.handleUC06(mounted(rec, withOriginationClaimInput(withLegsTracked(httptest.NewRequest(http.MethodPost, "/scenario/uc06", strings.NewReader(testRouteBody(gw, "/scenario/uc06", testNoAnswer)))))))
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("%d %s, want 422", rec.Code, rec.Body)
		}
		if !legAttempted(stub.legTypes, "pas-claim") || legAttempted(stub.legTypes, "pas-claim-update") {
			t.Fatalf("legs %v: want the submission sent and no amendment", stub.legTypes)
		}
		wantCarried(t, rec.Body.String(), stub.legTypes...)
		wantOwnBuildRefusal(t, *events, want)
	})
	t.Run("uc07 single call", func(t *testing.T) {
		gw, stub := newPendResumeFixture(t, pendFixtureOpts{member: "MBR-UC07", birthDate: "1990-08-25", familyName: "Haddad", pendedItem: "patient-reported-functional-status",
			extraRoles: map[string]string{"phg": "phg"}})
		events := observe(gw)
		rec := httptest.NewRecorder()
		gw.handleUC07(mounted(rec, withOriginationClaimInput(withLegsTracked(httptest.NewRequest(http.MethodPost, "/scenario/uc07", strings.NewReader(testRouteBody(gw, "/scenario/uc07", testNoAnswer)))))))
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("%d %s, want 422", rec.Code, rec.Body)
		}
		if !legAttempted(stub.legTypes, "pas-claim") || legAttempted(stub.legTypes, "patient-dtr") || legAttempted(stub.legTypes, "pas-claim-update") {
			t.Fatalf("legs %v: want the submission sent and no patient-DTR exchange or amendment", stub.legTypes)
		}
		wantCarried(t, rec.Body.String(), stub.legTypes...)
		wantOwnBuildRefusal(t, *events, want)
	})
	t.Run("uc06 complete", func(t *testing.T) {
		gw, stub := uc06RouteFixture(t, "2.0")
		events := observe(gw)
		start := startUC06(gw, testRouteBody(gw, "/scenario/uc06/start", ""))
		var started startResp
		if start.Code != http.StatusOK || json.Unmarshal(start.Body.Bytes(), &started) != nil {
			t.Fatalf("start %d %s", start.Code, start.Body)
		}
		rec := httptest.NewRecorder()
		gw.handleUC06Complete(rec, withOriginationClaimInput(httptest.NewRequest(http.MethodPost, "/scenario/uc06/complete",
			strings.NewReader(testRouteBody(gw, "/scenario/uc06/complete", `"resumeToken":"`+started.ResumeToken+`",`+testNoAnswer)))))
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), want) || len(stub.requests["pas-claim-update"]) != 0 {
			t.Fatalf("%d %s, want 422 and no amendment", rec.Code, rec.Body)
		}
		if _, ok := gw.loadPending(started.ResumeToken); !ok {
			t.Fatal("the refused /complete dropped the pend")
		}
		wantOwnBuildRefusal(t, *events, want)
	})
}

// The npi the single call states reaches both the order-sign request's userId
// and the attestation, ranked by who verifiably stated it: first from a
// caller scenario authentication verified, below the configured NPI from an
// unverified one. An npi that is not an NPI is refused before anything is
// sent.
func TestUC06SingleCall_TheStatedClinician(t *testing.T) {
	const sent, configured = "1111111112", "1234567893"
	drive := func(t *testing.T, verified bool, npi string) (*httptest.ResponseRecorder, *pendResumeSubstrate) {
		gw, stub := uc06RouteFixture(t, "2.0")
		gw.cfg.NPI = configured
		body := testRouteBody(gw, "/scenario/uc06", `"npi":"`+npi+`"`)
		route := func(w http.ResponseWriter, r *http.Request) { gw.handleUC06(w, withOriginationClaimInput(r)) }
		var req *http.Request
		if verified {
			pub, priv := callerKey(t)
			gw.cfg.ScenarioCallers = map[string]ed25519.PublicKey{"provider-app": pub}
			req = signedScenarioFor(t, "provider", "POST /scenario/uc06", body, "provider-app", priv, gw.cfg.Clock())
		} else {
			req = httptest.NewRequest(http.MethodPost, "/scenario/uc06", strings.NewReader(body))
		}
		rec := httptest.NewRecorder()
		gw.requireScenarioCaller(route)(rec, req)
		return rec, stub
	}
	for _, c := range []struct {
		name     string
		verified bool
		want     string
	}{
		{"a verified caller's npi ranks first", true, sent},
		{"an unverified caller's npi ranks below the configured NPI", false, configured},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec, stub := drive(t, c.verified, sent)
			if rec.Code != http.StatusOK {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
			crd := stub.requests["crd-order-select"]
			if len(crd) != 1 || !strings.Contains(string(crd[0]), `"userId":"Practitioner/`+c.want+`"`) {
				t.Fatalf("order-sign userId: want Practitioner/%s in %d requests", c.want, len(crd))
			}
			amend := stub.requests["pas-claim-update"]
			if len(amend) != 1 || !strings.Contains(string(amend[0]), c.want) {
				t.Fatalf("amendment: want the attestation to name %s", c.want)
			}
			if other := map[string]string{sent: configured, configured: sent}[c.want]; strings.Contains(string(amend[0]), other) {
				t.Fatalf("amendment names %s as well", other)
			}
		})
	}
	t.Run("an npi that is not an NPI, at /complete", func(t *testing.T) {
		gw, stub := uc06RouteFixture(t, "2.0")
		gw.cfg.NPI = configured
		start := startUC06(gw, testRouteBody(gw, "/scenario/uc06/start", ""))
		var started startResp
		if start.Code != http.StatusOK || json.Unmarshal(start.Body.Bytes(), &started) != nil {
			t.Fatalf("start %d %s", start.Code, start.Body)
		}
		pub, priv := callerKey(t)
		gw.cfg.ScenarioCallers = map[string]ed25519.PublicKey{"provider-app": pub}
		body := testRouteBody(gw, "/scenario/uc06/complete", `"resumeToken":"`+started.ResumeToken+`","npi":"1111111111"`)
		req := signedScenarioFor(t, "provider", "POST /scenario/uc06/complete", body, "provider-app", priv, gw.cfg.Clock())
		rec := httptest.NewRecorder()
		gw.requireScenarioCaller(func(w http.ResponseWriter, r *http.Request) { gw.handleUC06Complete(w, withOriginationClaimInput(r)) })(rec, req)
		// Refused, not skipped for the next source: the configured NPI would
		// otherwise have attested in its place.
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), msgSentNPIInvalid) {
			t.Fatalf("%d %s, want 400 %q", rec.Code, rec.Body, msgSentNPIInvalid)
		}
		if n := len(stub.requests["pas-claim-update"]); n != 0 {
			t.Fatalf("%d amendments sent", n)
		}
		if _, ok := gw.loadPending(started.ResumeToken); !ok {
			t.Fatal("the refused /complete dropped the pend")
		}
	})
	t.Run("an npi that is not an NPI", func(t *testing.T) {
		rec, stub := drive(t, true, "1111111111")
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), msgSentNPIInvalid) || len(stub.requests) != 0 {
			t.Fatalf("%d %s with %d legs, want 400 and none", rec.Code, rec.Body, len(stub.requests))
		}
	})
}

// uc03DemoOxygenFixture is UC-03's demo persona (MBR-D-UC03) with its oxygen
// order (E1390) dispatched to a supplier, on the order-dispatch fixture, so
// /scenario/uc03 carries its order-dispatch and questionnaire legs.
func uc03DemoOxygenFixture(t *testing.T) *dispatchFixture {
	t.Helper()
	const member = "MBR-D-UC03"
	const performerRef = "Organization/org-dme-uc03"
	orderJSON, err := buildDispatchDeviceRequest("dr-uc03", "Patient/"+member, performerRef, "E1390", DemoDisplayE1390UC03, DemoDxE1390UC03)
	if err != nil {
		t.Fatal(err)
	}
	supplierJSON, err := buildHomeOxygenSupplier("org-dme-uc03")
	if err != nil {
		t.Fatal(err)
	}
	fix := newDispatchFixture(t, member, Demo{BirthDate: "1979-09-02", FamilyName: "Whitfield"}, orderJSON, performerRef, supplierJSON)
	declareOriginationProfile(fix.gw, "demo")
	return fix
}
