//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/fhirseed"
)

const testStatedReportType = `{"system":"https://codesystem.x12.org/005010/755","code":"PY","display":"Physician's Report"}`

func pasRoute(build, target string) legRoute {
	return legRoute{Token: "pa.pas@" + target, BuildLine: build}
}

// An amendment carries the report type its request states only where it can:
// built at the line it reaches, on a line with a place for it, and with a pend
// tying the answer to an item. Otherwise it goes on without it, and a statement
// it does not carry is on the record. With no statement there is nothing to
// record, and nothing links at PAS 2.2.
func TestAmendment_ReportTypeIsCarriedOrRecorded(t *testing.T) {
	t.Parallel()
	const note = "11504-8"
	for _, tc := range []struct {
		name          string
		build, target string
		stated, code  string
		carried       bool
		links         bool
		recorded      bool
	}{
		{name: "stated at 2.2", build: "2.2", target: "2.2", stated: testStatedReportType, code: note, carried: true, links: true},
		{name: "stated, the pend asks another attachment", build: "2.2", target: "2.2", stated: testStatedReportType, code: "28570-0", recorded: true},
		{name: "none at 2.2", build: "2.2", target: "2.2", code: note},
		{name: "null at 2.2", build: "2.2", target: "2.2", stated: "null", code: note},
		{name: "stated below 2.2, no place for it", build: "2.1", target: "2.1", stated: testStatedReportType, code: note, links: true, recorded: true},
		{name: "none below 2.2", build: "2.1", target: "2.1", code: note, links: true},
		{name: "stated, 2.1 bridged up to 2.2", build: "2.1", target: "2.2", stated: testStatedReportType, code: note, recorded: true},
		{name: "none, 2.1 bridged up to 2.2", build: "2.1", target: "2.2", code: note},
		{name: "stated, 2.2 bridged down to 2.1", build: "2.2", target: "2.1", stated: testStatedReportType, code: note, recorded: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := attachmentUpdateInputs(tc.build, true, true)
			in.DiagnosticReport = []byte(`{"resourceType":"DiagnosticReport","id":"source-dr","status":"final","code":{"coding":[{"system":"http://loinc.org","code":"11504-8"}]}}`)
			sub, err := buildAuthoredPASSubmit(tc.build, attachmentInputs("ServiceRequest", tc.build, true))
			if err != nil {
				t.Fatal(err)
			}
			rec, err := recordPASSubmission(sub)
			if err != nil {
				t.Fatal(err)
			}
			rec.pend = amendmentLinkPend(t, tc.build, 1, tc.code)
			route := pasRoute(tc.build, tc.target)
			stated := json.RawMessage(tc.stated)
			in.PriorClaim = rec.prior
			in.Pend, in.ReportType = rec.pendFor(route), reportTypeFor(route, stated)
			body, err := buildAuthoredPASUpdate(tc.build, in)
			if err != nil {
				t.Fatal(err)
			}
			if got := amendmentCarriesReportType(body); got != tc.carried {
				t.Fatalf("carries a report type: %v, want %v", got, tc.carried)
			}
			refs, _, _ := amendmentLinks(t, body)
			if got := len(refs) != 0; got != tc.links {
				t.Fatalf("linked %v, want links=%v", refs, tc.links)
			}
			x := &exchangeRecorder{}
			recordReportTypeNotCarried(context.WithValue(context.Background(), exchangeRecorderKey{}, x), stated, body)
			if got := slices.Contains(x.rec.EditsNotMade, EditNotMadeReportTypeNotCarried); got != tc.recorded {
				t.Fatalf("recorded %v, want %v", x.rec.EditsNotMade, tc.recorded)
			}
		})
	}
}

// The prior Claim rides in the amendment as sent: a report type it states is
// the submission's, and says nothing of the amending Claim.
func TestAmendmentCarriesReportType_ReadsTheAmendingClaim(t *testing.T) {
	t.Parallel()
	ext := `{"resource":{"resourceType":"Claim","supportingInfo":[{"extension":[{"url":"` + pasExtDocumentInformation + `"}]}]}}`
	plain := `{"resource":{"resourceType":"Claim"}}`
	for body, want := range map[string]bool{
		`{"entry":[` + ext + `]}`:               true,
		`{"entry":[` + plain + `,` + ext + `]}`: false,
		`{"entry":[]}`:                          false,
		`not json`:                              false,
	} {
		if got := amendmentCarriesReportType([]byte(body)); got != want {
			t.Errorf("%s: %v, want %v", body, got, want)
		}
	}
}

func TestPendFor_AtEachRoute(t *testing.T) {
	t.Parallel()
	s := pasSubmissionRecord{pend: []byte("pend")}
	for _, tc := range []struct {
		build, target string
		pend          bool
	}{
		{"2.0", "2.0", true}, {"2.1", "2.1", true}, {"2.2", "2.2", true},
		{"2.0", "2.1", true}, {"2.2", "2.1", true},
		{"2.1", "2.2", false}, {"2.0", "2.2", false},
		{"9.9", "9.9", false},
	} {
		if got := s.pendFor(pasRoute(tc.build, tc.target)) != nil; got != tc.pend {
			t.Errorf("%s to %s: pend %v, want %v", tc.build, tc.target, got, tc.pend)
		}
	}
}

// amendmentSites are the routes that build an amendment, one for each build
// site. A route's /complete shares its site with the single call.
var amendmentSites = []struct {
	name, path string
	opts       pendFixtureOpts
	serve      func(*Gateway, http.ResponseWriter, *http.Request)
}{
	{"uc04", "/scenario/uc04", pendFixtureOpts{member: "MBR-UC04", birthDate: "1982-11-03", familyName: "Chen", pendedItem: "operative-diagnostic-report"}, (*Gateway).handleUC04},
	{"uc05", "/scenario/uc05", pendFixtureOpts{member: "MBR-UC05", birthDate: "1968-03-12", familyName: "Johansson", pendedItem: "operative-diagnostic-report",
		extraRoles: map[string]string{"facility": "metro-spine"}}, (*Gateway).handleUC05},
	{"uc06", "/scenario/uc06", pendFixtureOpts{member: "MBR-UC06", birthDate: "1969-07-21", familyName: "Reyes", pendedItem: "functional-status"}, (*Gateway).handleUC06},
	{"uc07", "/scenario/uc07", pendFixtureOpts{member: "MBR-UC07", birthDate: "1990-08-25", familyName: "Haddad", pendedItem: "patient-reported-functional-status",
		extraRoles: map[string]string{"phg": "phg"}}, (*Gateway).handleUC07},
}

func serveAmendment(t *testing.T, gw *Gateway, path string, serve func(*Gateway, http.ResponseWriter, *http.Request), edit func(map[string]json.RawMessage)) (*httptest.ResponseRecorder, *exchangeRecorder) {
	t.Helper()
	x := &exchangeRecorder{}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(editBody(t, testRouteBody(gw, path, ""), edit)))
	rec := httptest.NewRecorder()
	serve(gw, rec, withOriginationClaimInput(withLegsTracked(req.WithContext(context.WithValue(req.Context(), exchangeRecorderKey{}, x)))))
	return rec, x
}

func stateReportType(stated bool) func(map[string]json.RawMessage) {
	return func(m map[string]json.RawMessage) {
		delete(m, "reportType")
		if stated {
			m["reportType"] = json.RawMessage(testStatedReportType)
		}
	}
}

// A route takes the statement as it takes the certificationType, at every site
// that builds an amendment. The stub payer's pend ties the amended answer to no
// item, so the amendment is sent without it and the exchange records that; with
// no statement there is nothing to record.
func TestAmendmentRoute_AStatementNotCarriedIsRecorded(t *testing.T) {
	for _, site := range amendmentSites {
		for _, stated := range []bool{true, false} {
			t.Run(site.name, func(t *testing.T) {
				gw, stub := routeFixtureAtLine(t, site.opts, "2.2")
				rec, x := serveAmendment(t, gw, site.path, site.serve, stateReportType(stated))
				if rec.Code != 200 || !legAttempted(stub.legTypes, "pas-claim-update") {
					t.Fatalf("%d %s legs=%v", rec.Code, rec.Body.String(), stub.legTypes)
				}
				if got := slices.Contains(x.rec.EditsNotMade, EditNotMadeReportTypeNotCarried); got != stated {
					t.Fatalf("recorded %v with a statement %v", x.rec.EditsNotMade, stated)
				}
			})
		}
	}
	for _, stated := range []bool{true, false} {
		t.Run("uc06 complete", func(t *testing.T) {
			gw, stub := uc06RouteFixture(t, "2.2")
			var started startResp
			if start := startUC06(gw, testRouteBody(gw, "/scenario/uc06/start", "")); start.Code != 200 || json.Unmarshal(start.Body.Bytes(), &started) != nil {
				t.Fatalf("start %d %s", start.Code, start.Body.String())
			}
			x := &exchangeRecorder{}
			body := editBody(t, testRouteBody(gw, "/scenario/uc06/complete", `"resumeToken":"`+started.ResumeToken+`"`), stateReportType(stated))
			req := httptest.NewRequest(http.MethodPost, "/scenario/uc06/complete", strings.NewReader(body))
			rec := httptest.NewRecorder()
			gw.handleUC06Complete(rec, withOriginationClaimInput(req.WithContext(context.WithValue(req.Context(), exchangeRecorderKey{}, x))))
			if rec.Code != 200 || !legAttempted(stub.legTypes, "pas-claim-update") {
				t.Fatalf("%d %s legs=%v", rec.Code, rec.Body.String(), stub.legTypes)
			}
			if got := slices.Contains(x.rec.EditsNotMade, EditNotMadeReportTypeNotCarried); got != stated {
				t.Fatalf("recorded %v with a statement %v", x.rec.EditsNotMade, stated)
			}
		})
	}
}

// With a pend that asks for what each route's amendment answers (UC-04's and
// UC-05's report, UC-06's and UC-07's questionnaire), the statement reaches the
// wire at every build site, linked, and nothing is recorded. Unstated, the same
// pend links nothing.
func TestAmendmentRoute_AStatementIsCarriedAtEverySite(t *testing.T) {
	for _, site := range amendmentSites {
		for _, stated := range []bool{true, false} {
			t.Run(site.name, func(t *testing.T) {
				gw, stub := routeFixtureAtLine(t, site.opts, "2.2")
				stub.overrideResponse = func(leg string, payload []byte) []byte {
					if leg != "pas-claim" {
						return payload
					}
					if site.name == "uc04" || site.name == "uc05" {
						return bytes.ReplaceAll(amendmentLinkPend(t, "2.2", 1, "72148"), []byte("http://loinc.org"), []byte("http://www.ama-assn.org/go/cpt"))
					}
					return pendAsksForQuestionnaire(t, payload)
				}
				rec, x := serveAmendment(t, gw, site.path, site.serve, stateReportType(stated))
				if rec.Code != 200 || !legAttempted(stub.legTypes, "pas-claim-update") {
					t.Fatalf("%d %s legs=%v", rec.Code, rec.Body.String(), stub.legTypes)
				}
				sent := stub.requests["pas-claim-update"]
				if got := amendmentCarriesReportType(sent[len(sent)-1]); got != stated {
					t.Fatalf("carried %v with a statement %v", got, stated)
				}
				if refs, _, _ := amendmentLinks(t, sent[len(sent)-1]); (len(refs) != 0) != stated {
					t.Fatalf("linked %v with a statement %v", refs, stated)
				}
				if slices.Contains(x.rec.EditsNotMade, EditNotMadeReportTypeNotCarried) {
					t.Fatalf("recorded %v", x.rec.EditsNotMade)
				}
			})
		}
	}
}

// pendAsksForQuestionnaire adds to a payer's pend the CommunicationRequest asking
// item 1 for the questionnaire the routes' amendments answer.
func pendAsksForQuestionnaire(t *testing.T, pend []byte) []byte {
	t.Helper()
	var b map[string]any
	if err := json.Unmarshal(pend, &b); err != nil {
		t.Fatal(err)
	}
	b["entry"] = append(b["entry"].([]any), map[string]any{"resource": map[string]any{
		"resourceType": "CommunicationRequest", "status": "active",
		"extension": []any{
			map[string]any{"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-serviceLineNumber", "valuePositiveInt": 1},
			map[string]any{"url": "http://example.org/fhir/us/davinci-pas/StructureDefinition/extension-requestedQuestionnaire", "valueCanonical": fhirseed.LumbarMRIQuestionnaireCanonical},
		},
	}})
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A malformed statement is refused at the amendment's build, the leg that
// consumes it, at every conformance level and at every line, like the facts
// the amendment is built from: at /complete before the amendment (after
// UC-07's patient-DTR exchange, which needs none of it), and on the single call
// after the submission and every leg before the amendment, which were carried
// and stand, as the refusal says.
func TestAmendmentRoute_MalformedReportTypeRefusedAtTheAmendment(t *testing.T) {
	bad := func(m map[string]json.RawMessage) {
		m["reportType"] = json.RawMessage(`{"system":"http://loinc.org","code":"PY"}`)
	}
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		t.Run(line+"/complete", func(t *testing.T) {
			rec, stub := pendThenComplete(t, line, "", bad)
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "ReportType") {
				t.Fatalf("%d %s, want 422 naming ReportType", rec.Code, rec.Body.String())
			}
			if legAttempted(stub.legTypes, "pas-claim-update") {
				t.Fatal("an amendment with a malformed report type was sent")
			}
			wantCarried(t, rec.Body.String())
		})
	}
	for _, level := range allLevels {
		for _, resume := range []struct {
			name            string
			fixture         func(*testing.T, string) (*Gateway, *pendResumeSubstrate)
			start, complete func(*Gateway, http.ResponseWriter, *http.Request)
			before          []string // the legs /complete sends before its amendment
		}{
			{"uc06", uc06RouteFixture, (*Gateway).handleUC06Start, (*Gateway).handleUC06Complete, nil},
			{"uc07", uc07RouteFixture, (*Gateway).handleUC07Start, (*Gateway).handleUC07Complete, []string{"patient-dtr"}},
		} {
			t.Run(level.String()+"/"+resume.name+" complete", func(t *testing.T) {
				gw, stub := resume.fixture(t, "2.2")
				gw.cfg.ConformanceEnforcement = level
				path := "/scenario/" + resume.name
				start := httptest.NewRecorder()
				resume.start(gw, start, withOriginationClaimInput(httptest.NewRequest(http.MethodPost, path+"/start", strings.NewReader(testRouteBody(gw, path+"/start", "")))))
				var started startResp
				if start.Code != 200 || json.Unmarshal(start.Body.Bytes(), &started) != nil || started.ResumeToken == "" {
					t.Fatalf("start %d %s", start.Code, start.Body.String())
				}
				before := len(stub.legTypes)
				body := editBody(t, testRouteBody(gw, path+"/complete", `"resumeToken":"`+started.ResumeToken+`"`), bad)
				rec := httptest.NewRecorder()
				resume.complete(gw, rec, withOriginationClaimInput(withLegsTracked(httptest.NewRequest(http.MethodPost, path+"/complete", strings.NewReader(body)))))
				if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "ReportType") {
					t.Fatalf("%d %s, want 422 naming ReportType", rec.Code, rec.Body.String())
				}
				if sent := stub.legTypes[before:]; !slices.Equal(sent, resume.before) {
					t.Fatalf("legs sent before the refusal: %v, want %v", sent, resume.before)
				}
				wantCarried(t, rec.Body.String(), resume.before...)
			})
		}
		for _, site := range amendmentSites {
			t.Run(level.String()+"/"+site.name, func(t *testing.T) {
				gw, stub := routeFixtureAtLine(t, site.opts, "2.2")
				gw.cfg.ConformanceEnforcement = level
				rec, _ := serveAmendment(t, gw, site.path, site.serve, bad)
				if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "ReportType") {
					t.Fatalf("%d %s, want 422 naming ReportType", rec.Code, rec.Body.String())
				}
				if !legAttempted(stub.legTypes, "pas-claim") || legAttempted(stub.legTypes, "pas-claim-update") {
					t.Fatalf("legs %v: want the submission sent and no amendment", stub.legTypes)
				}
				wantCarried(t, rec.Body.String(), stub.legTypes...)
			})
		}
	}
}
