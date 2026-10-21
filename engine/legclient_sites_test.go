package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// legFault is a Hub that refuses one leg. A POST to /route whose envelope is
// that leg's is answered as the Hub's route handler answers a replayed
// envelope (409, not delivered); every other call reaches the fixture's own
// Hub. The leg is chosen by the envelope's transaction type, so a run fails
// the same leg every time. It records each refusal it made and each leg.failed
// event the gateway observed for the leg.
type legFault struct {
	leg  string
	next http.RoundTripper

	mu     sync.Mutex
	hits   int
	failed []ObserverEvent
}

const legFaultReason = "replay detected"

// failLeg installs a legFault for leg on gw, over its Hub and its observer.
func failLeg(gw *Gateway, leg string) *legFault {
	f := &legFault{leg: leg, next: http.DefaultTransport}
	client := http.Client{}
	if gw.cfg.Client != nil {
		client = *gw.cfg.Client
	}
	if client.Transport != nil {
		f.next = client.Transport
	}
	client.Transport = f
	gw.cfg.Client = &client
	observe := gw.cfg.Observer
	gw.cfg.Observer = func(e ObserverEvent) {
		if e.Kind == "leg.failed" && e.LegType == leg {
			f.mu.Lock()
			f.failed = append(f.failed, e)
			f.mu.Unlock()
		}
		if observe != nil {
			observe(e)
		}
	}
	return f
}

func (f *legFault) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/route") && req.Body != nil {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		if env, err := shnsdk.DecodeEnvelope(body); err == nil && env.Metadata.TransactionType == f.leg {
			f.mu.Lock()
			f.hits++
			f.mu.Unlock()
			return &http.Response{
				StatusCode: http.StatusConflict,
				Header:     http.Header{"Content-Type": []string{"application/json"}, HubDeliveredHeader: []string{"no"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":"` + legFaultReason + `"}`)),
				Request:    req,
			}, nil
		}
	}
	return f.next.RoundTrip(req)
}

// post is a POST to path carrying gw's route body.
func post(gw *Gateway, path, extra string) *http.Request {
	return httptest.NewRequest(http.MethodPost, path, strings.NewReader(testRouteBody(gw, path, extra)))
}

// legSiteRow is one module 1 send site with its leg refused by the Hub: how to
// reach it, and what the participant's caller is then told.
type legSiteRow struct {
	site string // file:function, as legclient_test.go lists it
	leg  string
	// run drives the flow to the site and answers its caller on rec. fail
	// installs the fault on the gateway the flow sends from, at the point the
	// row's leg is next.
	run func(t *testing.T, rec *httptest.ResponseRecorder, fail func(*Gateway) *legFault) *legFault
}

// legSiteRows are module 1's sends, one row per site (moduleOneSends).
var legSiteRows = []legSiteRow{
	{"module1_originate.go:handleScenario", "coverage-eligibility", func(t *testing.T, rec *httptest.ResponseRecorder, fail func(*Gateway) *legFault) *legFault {
		env := newInProcessExchange(t)
		f := fail(env.originator)
		env.originator.handleScenario(rec, post(env.originator, "/scenario/uc01", `"branch":"covered"`))
		return f
	}},
	{"module1_originate.go:handleUC03Bridge", "pas-claim", func(t *testing.T, rec *httptest.ResponseRecorder, fail func(*Gateway) *legFault) *legFault {
		gw, _ := newPendResumeFixture(t, pendFixtureOpts{member: "MBR-BRIDGE-DEMO", birthDate: "1983-03-11", familyName: "Solberg-BridgeDemo", pendedItem: "operative-diagnostic-report"})
		routeIdentityTo(t, &gw.cfg, BridgeDemoPayerID)
		f := fail(gw)
		gw.handleUC03(rec, post(gw, "/scenario/uc03", `"branch":"bridge-demo"`))
		return f
	}},
	uc04Row("module1_originate.go:runCRDThenDTROrder", "crd-order-select"),
	uc04Row("module1_originate.go:runCRDThenDTROrder", "dtr-questionnaire-fetch"),
	uc04Row("module1_originate.go:handleUC04", "pas-claim"),
	uc04Row("module1_originate.go:handleUC04", "pas-claim-update"),
	uc05Row("federated-query"),
	uc05Row("pas-claim"),
	uc05Row("pas-claim-update"),
	{"module1_originate.go:handleUC07HCPCS", "pas-claim", func(t *testing.T, rec *httptest.ResponseRecorder, fail func(*Gateway) *legFault) *legFault {
		gw, _ := newPendResumeFixture(t, pendFixtureOpts{member: "MBR-UC07HCPCS", birthDate: "1977-01-30", familyName: "Nakamura", pendedItem: "operative-diagnostic-report"})
		f := fail(gw)
		gw.handleUC07HCPCS(rec, post(gw, "/scenario/uc07hcpcs", ""))
		return f
	}},
	{"module1_originate.go:handleUC08", "pas-claim", func(t *testing.T, rec *httptest.ResponseRecorder, fail func(*Gateway) *legFault) *legFault {
		gw, stub, _ := crdTestSystem(t, shnsdk.CardCoverage{Covered: shnsdk.CoveredNotCovered, PANeeded: shnsdk.PANeededNoAuth})
		gw.cfg.OriginationProfile = "demo"
		stub.pci = shnsdk.ResolvePCI("MBR-D-UC08", "1968-12-21", "Adeyemi")
		stub.pasDenialRationale = "not medically necessary"
		stub.pasLine = highestLine(contractLineSet(gw.declaredContractVersions(), "pa.pas"))
		f := fail(gw)
		gw.handleUC08(rec, post(gw, "/scenario/uc08", ""))
		return f
	}},
	{"module1_originate.go:originateNoPACRD", "crd-order-select", func(t *testing.T, rec *httptest.ResponseRecorder, fail func(*Gateway) *legFault) *legFault {
		env := newInProcessExchange(t)
		env.originator.cfg.OriginationProfile = "demo"
		f := fail(env.originator)
		env.originator.handleUC02(rec, post(env.originator, "/scenario/uc02", ""))
		return f
	}},
	homeOxygenRow("module1_originate_homeoxygen.go:runCRDDispatch", "crd-order-dispatch"),
	homeOxygenRow("module1_originate_homeoxygen.go:runCRDDispatch", "dtr-questionnaire-fetch"),
	homeOxygenRow("module1_pas_tail.go:submitPASClaim", "pas-claim"),
	{"module1_originate_resume.go:scenarioToPend", "pas-claim", func(t *testing.T, rec *httptest.ResponseRecorder, fail func(*Gateway) *legFault) *legFault {
		gw, _ := uc06Fixture(t)
		f := fail(gw)
		if _, ok := gw.scenarioToPend(rec, post(gw, "/scenario/uc06", ""), "uc06", "MBR-UC06"); ok {
			t.Fatal("the pend completed with its pas-claim leg refused")
		}
		return f
	}},
	{"module1_originate_resume.go:completeClinician", "pas-claim-update", func(t *testing.T, rec *httptest.ResponseRecorder, fail func(*Gateway) *legFault) *legFault {
		gw, _ := uc06Fixture(t)
		st := pendFor(t, gw, "uc06", "MBR-UC06")
		f := fail(gw)
		if gw.completeClinician(rec, post(gw, "/scenario/uc06/complete", ""), st, "", "") {
			t.Fatal("the resume completed with its pas-claim-update leg refused")
		}
		return f
	}},
	uc07Row("patient-dtr"),
	uc07Row("pas-claim-update"),
	{"module1_originate_wait.go:inquireContinuation", "pas-claim-inquire", func(t *testing.T, rec *httptest.ResponseRecorder, fail func(*Gateway) *legFault) *legFault {
		gw, _ := pasFollowSystem(t, "pended")
		req := httptest.NewRequest(http.MethodPost, "/scenario/uc03", strings.NewReader(testOrderBody(pasFollowSubmit(0).orderJSON, "")))
		pend, status, msg, err := gw.submitClaimAndFollow(req.Context(), req, pasFollowSubmit(0))
		if status != 0 || err != nil || pend.Continuation == "" {
			t.Fatalf("submit: %d %q %v %+v", status, msg, err, pend)
		}
		f := fail(gw)
		gw.handlePAInquire(rec, httptest.NewRequest(http.MethodPost, "/scenario/pa/inquire",
			strings.NewReader(`{"continuation":"`+pend.Continuation+`","waitSeconds":0}`)))
		return f
	}},
	{"module1_dtr_adaptive.go:nextQuestionLeg", "dtr-questionnaire-fetch", func(t *testing.T, rec *httptest.ResponseRecorder, fail func(*Gateway) *legFault) *legFault {
		const subject = "Patient/MBR-COVERED"
		env := newInProcessExchange(t)
		declareFramedDTR(t, env, true)
		route, err := env.originator.selectLegLine(env.payerID, "dtr-questionnaire-fetch", "corr-0", "")
		if err != nil {
			t.Fatal(err)
		}
		f := fail(env.originator)
		res := crdDtrResult{recipient: env.payerID, pci: "pci-covered", patientRef: subject, dtrLine: shnsdk.LineOf(route.Token)}
		reqQR := []byte(`{"resourceType":"QuestionnaireResponse","status":"in-progress","subject":{"reference":"` + subject + `"}}`)
		_, status, msg, err := env.originator.nextQuestionLeg(context.Background(), env.req, res, testAdaptiveCanonical, reqQR)
		// Its callers, through attestAdaptiveQuestionnaire (handleUC04,
		// handleUC05, scenarioToPend), answer with relayOriginationError when
		// it returns the leg's error, else with its status and message.
		var refused *hubRefusalError
		if status != http.StatusBadGateway || !errors.As(err, &refused) || msg != err.Error() {
			t.Fatalf("nextQuestionLeg = %d %q %v: want the leg's own failure", status, msg, err)
		}
		if !env.originator.relayOriginationError(rec, err) {
			writeJSON(rec, status, map[string]string{"error": msg})
		}
		return f
	}},
}

// The rows a flow shares: UC-04 (CRD, DTR, PAS submit, PAS update), UC-05
// (PAS submit, the facility's records, PAS update), UC-07's resume and the
// home-oxygen dispatch (CRD, DTR, the PAS tail).
func uc04Row(site, leg string) legSiteRow {
	return legSiteRow{site, leg, func(t *testing.T, rec *httptest.ResponseRecorder, fail func(*Gateway) *legFault) *legFault {
		gw, _ := newPendResumeFixture(t, pendFixtureOpts{member: "MBR-UC04", birthDate: "1982-11-03", familyName: "Chen", pendedItem: "operative-diagnostic-report"})
		f := fail(gw)
		gw.handleUC04(rec, post(gw, "/scenario/uc04", ""))
		return f
	}}
}

func uc05Row(leg string) legSiteRow {
	return legSiteRow{"module1_originate.go:handleUC05", leg, func(t *testing.T, rec *httptest.ResponseRecorder, fail func(*Gateway) *legFault) *legFault {
		gw, _ := newPendResumeFixture(t, pendFixtureOpts{member: "MBR-UC05", birthDate: "1968-03-12", familyName: "Johansson",
			pendedItem: "operative-diagnostic-report", extraRoles: map[string]string{"facility": "metro-spine"}})
		f := fail(gw)
		gw.handleUC05(rec, post(gw, "/scenario/uc05", ""))
		return f
	}}
}

func uc07Row(leg string) legSiteRow {
	return legSiteRow{"module1_originate_resume.go:completePatient", leg, func(t *testing.T, rec *httptest.ResponseRecorder, fail func(*Gateway) *legFault) *legFault {
		gw, _ := newPendResumeFixture(t, pendFixtureOpts{member: "MBR-UC07", birthDate: "1990-08-25", familyName: "Haddad",
			pendedItem: "patient-reported-functional-status", extraRoles: map[string]string{"phg": "phg"}})
		st := pendFor(t, gw, "uc07", "MBR-UC07")
		f := fail(gw)
		if gw.completePatient(rec, post(gw, "/scenario/uc07/complete", ""), st, "") {
			t.Fatalf("the resume completed with its %s leg refused", leg)
		}
		return f
	}}
}

func homeOxygenRow(site, leg string) legSiteRow {
	return legSiteRow{site, leg, func(t *testing.T, rec *httptest.ResponseRecorder, fail func(*Gateway) *legFault) *legFault {
		fix := newMBROXDispatchFixture(t)
		f := fail(fix.gw)
		fix.gw.handleHomeOxygen(rec, post(fix.gw, "/scenario/homeoxygen", ""))
		return f
	}}
}

// pendFor runs scenario to its pend, with no leg refused.
func pendFor(t *testing.T, gw *Gateway, scenario, member string) pendState {
	t.Helper()
	rec := httptest.NewRecorder()
	st, ok := gw.scenarioToPend(rec, post(gw, "/scenario/"+scenario, ""), scenario, member)
	if !ok {
		t.Fatalf("scenarioToPend(%s): %d %s", scenario, rec.Code, rec.Body)
	}
	return st
}

// Every module 1 send site answers a leg the Hub refused as the Hub answered
// it, and the failure is recorded on the leg: the participant's caller reads
// the Hub's 409 and its reason (relayOriginationError), never a generic 502,
// and the gateway observes leg.failed for that leg with the refusal.
func TestModuleOneSendSitesAnswerALegTheHubRefused(t *testing.T) {
	sites := map[string]int{}
	for _, row := range legSiteRows {
		sites[row.site+":"+strconv.Quote(row.leg)]++
		t.Run(row.site+" "+row.leg, func(t *testing.T) {
			rec := httptest.NewRecorder()
			f := row.run(t, rec, func(gw *Gateway) *legFault { return failLeg(gw, row.leg) })
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.hits != 1 {
				t.Fatalf("the Hub refused the %s leg %d times, want once: the flow did not reach the site (%d %s)", row.leg, f.hits, rec.Code, rec.Body)
			}
			const want = `"error":"hub refused the exchange: ` + legFaultReason + `"`
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), want) {
				t.Fatalf("the caller was told %d %s, want 409 with %s", rec.Code, rec.Body, want)
			}
			if len(f.failed) != 1 || f.failed[0].Detail != "hub refused the exchange (409): "+legFaultReason || f.failed[0].Direction != "originate" {
				t.Fatalf("leg.failed for %s = %+v, want one, naming the Hub's refusal", row.leg, f.failed)
			}
		})
	}
	if fmt.Sprint(sites) != fmt.Sprint(moduleOneSends) {
		t.Fatalf("rows by site = %v, want one row for each of module 1's sends %v", sites, moduleOneSends)
	}
}
