package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

type failedAuthNumberStore struct {
	Store
	calls int
}

func (s *failedAuthNumberStore) StoreAuthNumber(string, string) error {
	s.calls++
	return errors.New("synthetic write failure")
}

func TestLocalConsumptionDispatchPASReply(t *testing.T) {
	for _, route := range []string{"dispatch", "homeoxygen"} {
		for _, kind := range []string{"approved", "opaque", "unknown decision", "basic refusal", "backend error", "local write failure"} {
			t.Run(route+"/"+kind, func(t *testing.T) {
				f := newMBROXDispatchFixture(t)
				body := homeOxygenApprovedClaimResponse()
				appStatus, wantStatus := 201, 200
				switch kind {
				case "opaque", "basic refusal":
					body = []byte("opaque\x00answer\xff")
					wantStatus = 502
				case "unknown decision":
					body = bytes.ReplaceAll(body, []byte(`"code":"A1"`), []byte(`"code":"ZZ"`))
					wantStatus = 502
				case "backend error":
					body = []byte("backend refused\x00")
					appStatus = 422
					wantStatus = 422
				case "local write failure":
					wantStatus = 502
				}
				if kind == "basic refusal" {
					f.gw.cfg.ConformanceEnforcement = EnforcementBasic
				}
				writes := &failedAuthNumberStore{Store: f.gw.cfg.Store}
				if kind == "local write failure" {
					f.gw.cfg.Store = writes
				}
				f.stub.packageParameters = true
				f.stub.frameErrLeg = "pas-claim"
				f.stub.frameErrStatus = appStatus
				f.stub.frameErrBody = body
				f.stub.frameErrCT = "application/fhir+json"
				peer, _ := f.gw.cfg.Reg.Lookup("payer")
				peer.MessageFrames = shnsdk.SupportedMessageFrames()
				f.gw.cfg.Reg.Set("payer", peer)
				w := httptest.NewRecorder()
				r := httptest.NewRequest("POST", "/scenario/"+route+"?wait=0", strings.NewReader(`{"member":"MBR-OX"}`))
				if route == "dispatch" {
					f.gw.handleDispatch(w, r)
				} else {
					f.gw.handleHomeOxygen(w, r)
				}
				if w.Code != wantStatus {
					t.Fatalf("status=%d want=%d body=%s legs=%v", w.Code, wantStatus, w.Body.String(), f.stub.legTypes)
				}
				if kind == "backend error" {
					if !bytes.Equal(w.Body.Bytes(), body) || w.Header().Get("Content-Type") != "application/fhir+json" {
						t.Fatal("backend reply changed")
					}
					return
				}
				var out struct {
					ApplicationReply *ApplicationReplyView
					Consumption      ConsumptionOutcome
					Decision         string
				}
				if json.Unmarshal(w.Body.Bytes(), &out) != nil || out.ApplicationReply == nil {
					t.Fatalf("received reply lost: %s", w.Body.String())
				}
				raw, err := base64.StdEncoding.DecodeString(out.ApplicationReply.BodyBase64)
				if err != nil || !bytes.Equal(raw, body) || out.ApplicationReply.Status != appStatus || out.ApplicationReply.ContentType != "application/fhir+json" || out.ApplicationReply.Leg != "pas-claim" || out.ApplicationReply.CorrelationID == "" {
					t.Fatalf("reply changed: %+v", out.ApplicationReply)
				}
				wantState := "unavailable"
				if kind == "approved" {
					wantState = "available"
				}
				if out.Consumption.State != wantState {
					t.Fatalf("consumption=%+v", out.Consumption)
				}
				if kind == "basic refusal" && (out.Consumption.Refusal == nil || out.Consumption.Refusal.Direction != "response") {
					t.Fatal("response refusal lost")
				}
				if kind != "approved" && out.Decision != "" {
					t.Fatal("failed local consumption asserted decision")
				}
				if kind == "local write failure" && writes.calls != 1 {
					t.Fatal("actual authorized write not reached")
				}
			})
		}
	}
}

func TestLocalConsumptionDirectPASSubmit(t *testing.T) {
	for _, scenario := range []string{"uc04", "uc05", "uc06-start", "uc07-start", "uc07-hcpcs", "uc08"} {
		t.Run(scenario, func(t *testing.T) {
			members := map[string]string{"uc04": "MBR-UC04", "uc05": "MBR-UC05", "uc06-start": "MBR-UC06", "uc07-start": "MBR-UC07", "uc07-hcpcs": "MBR-UC07HCPCS", "uc08": "MBR-UC08"}
			member := members[scenario]
			_, demo, found := newCensusSoR().ResolvePatient(member)
			if !found {
				t.Fatal("missing fixture persona")
			}
			g, stub := newPendResumeFixture(t, pendFixtureOpts{member: member, birthDate: demo.BirthDate, familyName: demo.FamilyName, pendedItem: "functional-status"})
			// Native none delivers the answer; the explicit local consumer must still refuse it.
			g.cfg.ConformanceEnforcement = EnforcementNone
			answer := []byte("unreadable PAS\x00\xff")
			stub.overrideResponse = func(leg string, b []byte) []byte {
				if leg != "pas-claim" {
					return b
				}
				wire, err := shnsdk.EncodeHTTPFrameHeaders(202, map[string]string{"Content-Type": "application/octet-stream", shnsdk.FrameHeaderContractVersion: "pa.pas@2.0"}, answer)
				if err != nil {
					t.Fatal(err)
				}
				return wire
			}
			peer, _ := g.cfg.Reg.Lookup("payer")
			peer.MessageFrames = shnsdk.SupportedMessageFrames()
			g.cfg.Reg.Set("payer", peer)
			w := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/scenario/"+scenario+"?wait=0", nil)
			switch scenario {
			case "uc04":
				g.handleUC04(w, r)
			case "uc05":
				g.handleUC05(w, r)
			case "uc06-start":
				g.handleUC06Start(w, r)
			case "uc07-start":
				g.handleUC07Start(w, r)
			case "uc07-hcpcs":
				g.handleUC07HCPCS(w, r)
			case "uc08":
				g.handleUC08(w, r)
			}
			if !legAttempted(stub.legTypes, "pas-claim") {
				t.Fatalf("PAS not reached: %d %s legs=%v", w.Code, w.Body.String(), stub.legTypes)
			}
			var out struct {
				ApplicationReply *ApplicationReplyView
				Consumption      ConsumptionOutcome
				Decision         string
				Denied           bool
			}
			if w.Code != 502 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.ApplicationReply == nil {
				t.Fatalf("reply lost: %d %s", w.Code, w.Body.String())
			}
			raw, err := base64.StdEncoding.DecodeString(out.ApplicationReply.BodyBase64)
			if err != nil || !bytes.Equal(raw, answer) || out.ApplicationReply.Status != 202 || out.ApplicationReply.ContentType != "application/octet-stream" || out.ApplicationReply.DeclaredVersion != "pa.pas@2.0" || out.ApplicationReply.Leg != "pas-claim" || out.ApplicationReply.CorrelationID == "" || out.Consumption.State != "unavailable" || out.Decision != "" || out.Denied {
				t.Fatalf("invalid local result: %+v", out)
			}
		})
	}
}

func TestLocalConsumptionPASResume(t *testing.T) {
	for _, scenario := range []string{"uc06", "uc07"} {
		for _, kind := range []string{"approved", "opaque", "unknown decision", "backend error", "local write failure"} {
			t.Run(scenario+"/"+kind, func(t *testing.T) {
				member := "MBR-UC06"
				if scenario == "uc07" {
					member = "MBR-UC07"
				}
				_, demo, _ := newCensusSoR().ResolvePatient(member)
				g, stub := newPendResumeFixture(t, pendFixtureOpts{member: member, birthDate: demo.BirthDate, familyName: demo.FamilyName, pendedItem: "functional-status", extraRoles: map[string]string{"phg": "phg"}})
				g.cfg.ConformanceEnforcement = EnforcementNone
				started := httptest.NewRecorder()
				r := httptest.NewRequest("POST", "/scenario/"+scenario+"/start", nil)
				if scenario == "uc06" {
					g.handleUC06Start(started, r)
				} else {
					g.handleUC07Start(started, r)
				}
				var start startResp
				if started.Code != 200 || json.Unmarshal(started.Body.Bytes(), &start) != nil || start.ResumeToken == "" || start.ApplicationReply == nil || start.ApplicationReply.Leg != "pas-claim" || start.Consumption.State != "available" {
					t.Fatalf("start reply lost: %d %s", started.Code, started.Body.String())
				}
				answer := bytes.ReplaceAll(homeOxygenApprovedClaimResponse(), []byte("Patient/MBR-OX"), []byte("Patient/"+member))
				appStatus, wantStatus := 201, 200
				switch kind {
				case "opaque":
					answer = []byte("unreadable update\x00\xff")
					wantStatus = 502
				case "unknown decision":
					answer = bytes.ReplaceAll(answer, []byte(`"code":"A1"`), []byte(`"code":"ZZ"`))
					wantStatus = 502
				case "backend error":
					answer = []byte("backend update error\x00")
					appStatus = 409
					wantStatus = 409
				case "local write failure":
					wantStatus = 502
					g.cfg.Store = &failedAuthNumberStore{Store: g.cfg.Store}
				}
				stub.overrideResponse = func(leg string, b []byte) []byte {
					if leg != "pas-claim-update" {
						return b
					}
					wire, err := shnsdk.EncodeHTTPFrameHeaders(appStatus, map[string]string{"Content-Type": "application/octet-stream", shnsdk.FrameHeaderContractVersion: "pa.pas@2.0"}, answer)
					if err != nil {
						t.Fatal(err)
					}
					return wire
				}
				peer, _ := g.cfg.Reg.Lookup("payer")
				peer.MessageFrames = shnsdk.SupportedMessageFrames()
				g.cfg.Reg.Set("payer", peer)
				w := httptest.NewRecorder()
				r = httptest.NewRequest("POST", "/scenario/"+scenario+"/complete?wait=0", strings.NewReader(`{"resumeToken":"`+start.ResumeToken+`"}`))
				if scenario == "uc06" {
					g.handleUC06Complete(w, r)
				} else {
					g.handleUC07Complete(w, r)
				}
				if !legAttempted(stub.legTypes, "pas-claim-update") {
					t.Fatalf("update not reached: %d %s legs=%v", w.Code, w.Body.String(), stub.legTypes)
				}
				if w.Code != wantStatus {
					t.Fatalf("status=%d want=%d body=%s", w.Code, wantStatus, w.Body.String())
				}
				_, stillPending := g.loadPending(start.ResumeToken)
				if stillPending != (kind != "approved") {
					t.Fatal("failed consumer advanced/deleted pending workflow")
				}
				if kind == "backend error" {
					if !bytes.Equal(w.Body.Bytes(), answer) || w.Header().Get("Content-Type") != "application/octet-stream" {
						t.Fatal("backend reply changed")
					}
					return
				}
				var out struct {
					ApplicationReply *ApplicationReplyView
					Consumption      ConsumptionOutcome
					Decision         string
				}
				if json.Unmarshal(w.Body.Bytes(), &out) != nil || out.ApplicationReply == nil {
					t.Fatalf("update reply lost: %s", w.Body.String())
				}
				view := out.ApplicationReply
				raw, err := base64.StdEncoding.DecodeString(view.BodyBase64)
				if err != nil || !bytes.Equal(raw, answer) || view.Status != appStatus || view.ContentType != "application/octet-stream" || view.DeclaredVersion != "pa.pas@2.0" || view.Leg != "pas-claim-update" || view.CorrelationID == "" || view.CorrelationID == start.ApplicationReply.CorrelationID {
					t.Fatalf("wrong update evidence: %+v", view)
				}
				expected := "unavailable"
				if kind == "approved" {
					expected = "available"
				}
				if out.Consumption.State != expected {
					t.Fatalf("outcome=%+v", out.Consumption)
				}
				if kind != "approved" && out.Decision != "" {
					t.Fatal("failed update consumption asserted decision")
				}
			})
		}
	}
}

func TestLocalConsumptionPASPreUpdateReadFailureRetainsPend(t *testing.T) {
	g, stub := newPendResumeFixture(t, pendFixtureOpts{member: "MBR-UC04", birthDate: "1982-11-03", familyName: "Chen", pendedItem: "operative-diagnostic-report"})
	g.cfg.ConformanceEnforcement = EnforcementNone
	base := g.cfg.SoR
	g.cfg.SoR = searchingScriptedSoR{scriptedReadSoR{t: t, base: ReadSystemOfRecord(base), search: base.(SearchSystemOfRecord), before: func(_ context.Context, op, key string) error {
		if op == "SupplementalReport" {
			return &SoRReadError{Kind: SoRUnavailable}
		}
		return nil
	}}}
	var answer []byte
	stub.overrideResponse = func(leg string, b []byte) []byte {
		if leg == "pas-claim" {
			answer = append([]byte(nil), b...)
		}
		return b
	}
	w := httptest.NewRecorder()
	g.handleUC04(w, httptest.NewRequest("POST", "/scenario/uc04", nil))
	var out struct {
		ApplicationReply *ApplicationReplyView
		Consumption      ConsumptionOutcome
	}
	if w.Code != 503 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.ApplicationReply == nil {
		t.Fatalf("prior reply lost: %d %s legs=%v", w.Code, w.Body.String(), stub.legTypes)
	}
	raw, err := base64.StdEncoding.DecodeString(out.ApplicationReply.BodyBase64)
	if err != nil || !bytes.Equal(raw, answer) || out.ApplicationReply.Leg != "pas-claim" || out.Consumption.State != "unavailable" || legAttempted(stub.legTypes, "pas-claim-update") {
		t.Fatal("local read failure lost prior evidence or dispatched update")
	}
}

func TestLocalConsumptionPASUpdatePreexchangeRetainsPriorReply(t *testing.T) {
	g, stub := newPendResumeFixture(t, pendFixtureOpts{member: "MBR-UC04", birthDate: "1982-11-03", familyName: "Chen", pendedItem: "operative-diagnostic-report"})
	g.cfg.ConformanceEnforcement = EnforcementNone
	var prior []byte
	stub.overrideResponse = func(leg string, b []byte) []byte {
		if leg == "pas-claim" {
			prior = append([]byte(nil), b...)
		}
		return b
	}
	refused := false
	g.cfg.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		if strings.HasSuffix(r.URL.Path, "/authorize") && bytes.Contains(body, []byte(`"operation":"pas-update-submit"`)) {
			refused = true
			return errResp("synthetic authorization outage"), nil
		}
		return stub.RoundTrip(r)
	})}
	w := httptest.NewRecorder()
	g.handleUC04(w, httptest.NewRequest("POST", "/scenario/uc04", nil))
	var out struct {
		ApplicationReply *ApplicationReplyView
		Consumption      ConsumptionOutcome
	}
	if !refused || w.Code != 502 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.ApplicationReply == nil {
		t.Fatalf("prior reply lost: %d %s legs=%v", w.Code, w.Body.String(), stub.legTypes)
	}
	got, err := base64.StdEncoding.DecodeString(out.ApplicationReply.BodyBase64)
	if err != nil || !bytes.Equal(got, prior) || out.ApplicationReply.Leg != "pas-claim" || out.Consumption.State != "unavailable" || legAttempted(stub.legTypes, "pas-claim-update") {
		t.Fatal("invented update reply or lost earlier reply")
	}
}

type unavailableWorkflowPopulator struct{}

func (unavailableWorkflowPopulator) Populate(context.Context, []byte, PopulateContext) ([]byte, []FilledItem, error) {
	return nil, nil, errors.New("synthetic populate failure")
}

func TestLocalConsumptionDispatchPrefixRetainsReply(t *testing.T) {
	for _, leg := range []string{"crd-order-dispatch", "dtr-questionnaire-fetch"} {
		for _, kind := range []string{"opaque", "backend error", "populate failure"} {
			if kind == "populate failure" && leg != "dtr-questionnaire-fetch" {
				continue
			}
			t.Run(leg+"/"+kind, func(t *testing.T) {
				f := newMBROXDispatchFixture(t)
				body := []byte("unreadable prefix\x00\xff")
				appStatus, wantStatus := 202, 502
				if kind == "backend error" {
					appStatus, wantStatus = 409, 409
				}
				if kind == "populate failure" {
					wantStatus = http.StatusInternalServerError
					pkg, err := testQuestionnairePackage(homeOxygenQuestionnaire(f.canonical))
					if err != nil {
						t.Fatal(err)
					}
					body = pkg
					f.gw.cfg.Populator = unavailableWorkflowPopulator{}
				}
				f.stub.packageParameters = true
				f.stub.frameErrLeg = leg
				f.stub.frameErrStatus = appStatus
				f.stub.frameErrBody = body
				f.stub.frameErrCT = "application/octet-stream"
				peer, _ := f.gw.cfg.Reg.Lookup("payer")
				peer.MessageFrames = shnsdk.SupportedMessageFrames()
				f.gw.cfg.Reg.Set("payer", peer)
				w := httptest.NewRecorder()
				f.gw.handleDispatch(w, httptest.NewRequest("POST", "/scenario/dispatch?wait=0", strings.NewReader(`{"member":"MBR-OX"}`)))
				if w.Code != wantStatus {
					t.Fatalf("status=%d want=%d body=%s", w.Code, wantStatus, w.Body.String())
				}
				if kind == "backend error" {
					if !bytes.Equal(w.Body.Bytes(), body) || w.Header().Get("Content-Type") != "application/octet-stream" {
						t.Fatal("backend changed")
					}
					return
				}
				var out struct {
					ApplicationReply *ApplicationReplyView
					Consumption      ConsumptionOutcome
				}
				if json.Unmarshal(w.Body.Bytes(), &out) != nil || out.ApplicationReply == nil {
					t.Fatalf("reply lost: %s", w.Body.String())
				}
				got, err := base64.StdEncoding.DecodeString(out.ApplicationReply.BodyBase64)
				view := out.ApplicationReply
				if err != nil || !bytes.Equal(got, body) || view.Status != appStatus || view.ContentType != "application/octet-stream" || view.Leg != leg || view.CorrelationID == "" || out.Consumption.State != "unavailable" {
					t.Fatalf("wrong result: %+v", out)
				}
				if legAttempted(f.stub.legTypes, "pas-claim") {
					t.Fatal("failed prefix consumption allowed PAS construction")
				}
			})
		}
	}
}

func TestLocalConsumptionOrderingPrefixRetainsReply(t *testing.T) {
	for _, route := range []string{"uc02", "uc06-start"} {
		for _, leg := range []string{"crd-order-select", "dtr-questionnaire-fetch"} {
			if route == "uc02" && leg != "crd-order-select" {
				continue
			}
			t.Run(route+"/"+leg, func(t *testing.T) {
				g, stub := uc06Fixture(t)
				if route == "uc02" {
					g, stub = newPendResumeFixture(t, pendFixtureOpts{member: "MBR-D-UC02", birthDate: "1965-06-11", familyName: "Fontaine"})
					g.cfg.OriginationProfile = "demo"
				}
				g.cfg.ConformanceEnforcement = EnforcementNone
				body := []byte("unreadable prefix\x00\xff")
				stub.overrideResponse = func(got string, b []byte) []byte {
					if got != leg {
						return b
					}
					framed, err := shnsdk.EncodeHTTPFrame(202, "application/octet-stream", body)
					if err != nil {
						t.Fatal(err)
					}
					return framed
				}
				peer, _ := g.cfg.Reg.Lookup("payer")
				peer.MessageFrames = shnsdk.SupportedMessageFrames()
				g.cfg.Reg.Set("payer", peer)
				w := httptest.NewRecorder()
				r := httptest.NewRequest("POST", "/scenario/"+route+"?wait=0", nil)
				if route == "uc02" {
					g.handleUC02(w, r)
				} else {
					g.handleUC06Start(w, r)
				}
				var out ConsumptionAttempt
				if w.Code != 502 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.ApplicationReply == nil {
					t.Fatalf("reply lost: %d %s legs=%v", w.Code, w.Body.String(), stub.legTypes)
				}
				raw, err := base64.StdEncoding.DecodeString(out.ApplicationReply.BodyBase64)
				if err != nil || !bytes.Equal(raw, body) || out.ApplicationReply.Leg != leg || out.ApplicationReply.Status != 202 || out.Consumption.State != "unavailable" {
					t.Fatalf("wrong attempt: %+v", out)
				}
				if legAttempted(stub.legTypes, "pas-claim") {
					t.Fatal("unreadable prefix reached PAS")
				}
			})
		}
	}
}

type workflowAuthWrites struct {
	Store
	calls int
}

func (s *workflowAuthWrites) StoreAuthNumber(order, auth string) error {
	s.calls++
	return s.Store.StoreAuthNumber(order, auth)
}

func TestLocalConsumptionDisclosureReplies(t *testing.T) {
	for _, leg := range []string{"patient-dtr", "federated-query"} {
		for _, kind := range []string{"opaque", "backend error"} {
			t.Run(leg+"/"+kind, func(t *testing.T) {
				member := "MBR-UC07"
				if leg == "federated-query" {
					member = "MBR-UC05"
				}
				_, demo, _ := newCensusSoR().ResolvePatient(member)
				g, stub := newPendResumeFixture(t, pendFixtureOpts{member: member, birthDate: demo.BirthDate, familyName: demo.FamilyName, pendedItem: "functional-status", extraRoles: map[string]string{"phg": "phg", "facility": "facility"}})
				g.cfg.ConformanceEnforcement = EnforcementNone
				var start startResp
				if leg == "patient-dtr" {
					w := httptest.NewRecorder()
					g.handleUC07Start(w, httptest.NewRequest("POST", "/scenario/uc07/start", nil))
					if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &start) != nil {
						t.Fatalf("start: %d %s", w.Code, w.Body.String())
					}
				}
				body := []byte("unreadable disclosure\x00\xff")
				appStatus, wantStatus := 202, 502
				if kind == "backend error" {
					appStatus, wantStatus = 409, 409
				}
				stub.overrideResponse = func(got string, b []byte) []byte {
					if got != leg {
						return b
					}
					v, e := shnsdk.EncodeHTTPFrame(appStatus, "application/octet-stream", body)
					if e != nil {
						t.Fatal(e)
					}
					return v
				}
				holder := "phg"
				if leg == "federated-query" {
					holder = "facility"
				}
				peer, _ := g.cfg.Reg.Lookup(holder)
				peer.MessageFrames = shnsdk.SupportedMessageFrames()
				g.cfg.Reg.Set(holder, peer)
				w := httptest.NewRecorder()
				if leg == "federated-query" {
					g.handleUC05(w, httptest.NewRequest("POST", "/scenario/uc05?wait=0", nil))
				} else {
					g.handleUC07Complete(w, httptest.NewRequest("POST", "/scenario/uc07/complete?wait=0", strings.NewReader(`{"resumeToken":"`+start.ResumeToken+`"}`)))
				}
				if w.Code != wantStatus {
					t.Fatalf("status=%d body=%s legs=%v", w.Code, w.Body.String(), stub.legTypes)
				}
				if kind == "backend error" {
					if !bytes.Equal(w.Body.Bytes(), body) {
						t.Fatal("backend changed")
					}
					return
				}
				var out ConsumptionAttempt
				if json.Unmarshal(w.Body.Bytes(), &out) != nil || out.ApplicationReply == nil {
					t.Fatalf("reply lost: %s", w.Body.String())
				}
				raw, _ := base64.StdEncoding.DecodeString(out.ApplicationReply.BodyBase64)
				if !bytes.Equal(raw, body) || out.ApplicationReply.Leg != leg || out.ApplicationReply.Status != 202 || out.Consumption.State != "unavailable" {
					t.Fatalf("wrong attempt: %+v", out)
				}
				if legAttempted(stub.legTypes, "pas-claim-update") {
					t.Fatal("unreadable source permitted update")
				}
			})
		}
	}
}

func TestLocalConsumptionEligibilityReply(t *testing.T) {
	for _, kind := range []string{"valid", "opaque", "backend error"} {
		t.Run(kind, func(t *testing.T) {
			member := "MBR-COVERED"
			_, demo, _ := newCensusSoR().ResolvePatient(member)
			g, stub := newPendResumeFixture(t, pendFixtureOpts{member: member, birthDate: demo.BirthDate, familyName: demo.FamilyName})
			g.cfg.ConformanceEnforcement = EnforcementNone
			var body []byte
			appStatus, wantStatus := 202, 200
			if kind == "opaque" {
				wantStatus = 502
			}
			if kind == "backend error" {
				appStatus, wantStatus = 409, 409
			}
			stub.overrideResponse = func(leg string, b []byte) []byte {
				if leg != "coverage-eligibility" {
					return b
				}
				body = b
				if kind != "valid" {
					body = []byte("opaque eligibility\x00\xff")
				}
				frame, e := shnsdk.EncodeHTTPFrame(appStatus, "application/fhir+json", body)
				if e != nil {
					t.Fatal(e)
				}
				return frame
			}
			peer, _ := g.cfg.Reg.Lookup("payer")
			peer.MessageFrames = shnsdk.SupportedMessageFrames()
			g.cfg.Reg.Set("payer", peer)
			w := httptest.NewRecorder()
			g.handleScenario(w, httptest.NewRequest("POST", "/scenario/uc01", strings.NewReader(`{"branch":"covered"}`)))
			if w.Code != wantStatus {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if kind == "backend error" {
				if !bytes.Equal(w.Body.Bytes(), body) {
					t.Fatal("backend changed")
				}
				return
			}
			var out struct {
				ApplicationReply *ApplicationReplyView
				Consumption      ConsumptionOutcome
				Covered          bool
			}
			if json.Unmarshal(w.Body.Bytes(), &out) != nil || out.ApplicationReply == nil {
				t.Fatalf("reply lost: %s", w.Body.String())
			}
			raw, _ := base64.StdEncoding.DecodeString(out.ApplicationReply.BodyBase64)
			wantState := "unavailable"
			if kind == "valid" {
				wantState = "available"
			}
			if !bytes.Equal(raw, body) || out.ApplicationReply.Leg != "coverage-eligibility" || out.Consumption.State != wantState || out.Covered != (kind == "valid") {
				t.Fatalf("wrong result: %+v", out)
			}
		})
	}
}

func TestLocalConsumptionNextQuestionReply(t *testing.T) {
	env := newInProcessExchange(t)
	env.originator.cfg.ConformanceEnforcement = EnforcementNone
	declareFramedDTR(t, env, true)
	body := []byte("opaque adaptive\x00\xff")
	env.payerReturns(LegResult{Response: testResponse(body)})
	route, err := env.originator.selectLegLine(env.payerID, "dtr-questionnaire-fetch", "corr-0")
	if err != nil {
		t.Fatal(err)
	}
	res := crdDtrResult{recipient: env.payerID, pci: "pci-covered", patientRef: "Patient/MBR-COVERED", dtrLine: shnsdk.LineOf(route.Token)}
	_, status, _, _ := env.originator.nextQuestionLeg(context.Background(), env.req, &res, testAdaptiveCanonical, []byte(`{"resourceType":"QuestionnaireResponse","status":"in-progress","subject":{"reference":"Patient/MBR-COVERED"}}`))
	if status == 0 || res.attempt.ApplicationReply == nil {
		t.Fatalf("failed next-question lost reply: %d %+v", status, res.attempt)
	}
	raw, _ := base64.StdEncoding.DecodeString(res.attempt.ApplicationReply.BodyBase64)
	if !bytes.Equal(raw, body) || res.attempt.Consumption.State != "unavailable" {
		t.Fatal("adaptive reply changed")
	}
}

func TestLocalConsumptionLatestReplySurvivesNextDispatchFailure(t *testing.T) {
	for _, leg := range []string{"dtr-questionnaire-fetch", "federated-query", "patient-dtr"} {
		t.Run(leg, func(t *testing.T) {
			var g *Gateway
			var transport http.RoundTripper
			var answer []byte
			var start startResp
			if leg == "dtr-questionnaire-fetch" {
				f := newMBROXDispatchFixture(t)
				g = f.gw
				transport = f.stub
				var err error
				answer, err = testQuestionnairePackage(homeOxygenQuestionnaire(f.canonical))
				if err != nil {
					t.Fatal(err)
				}
				f.stub.frameErrLeg, f.stub.frameErrStatus, f.stub.frameErrBody, f.stub.frameErrCT = leg, 201, answer, "application/fhir+json"
				peer, _ := g.cfg.Reg.Lookup("payer")
				peer.MessageFrames = shnsdk.SupportedMessageFrames()
				g.cfg.Reg.Set("payer", peer)
			} else {
				member := "MBR-UC05"
				if leg == "patient-dtr" {
					member = "MBR-UC07"
				}
				_, demo, _ := newCensusSoR().ResolvePatient(member)
				var stub *pendResumeSubstrate
				g, stub = newPendResumeFixture(t, pendFixtureOpts{member: member, birthDate: demo.BirthDate, familyName: demo.FamilyName, pendedItem: "functional-status", extraRoles: map[string]string{"phg": "phg", "facility": "facility"}})
				transport = stub
				g.cfg.ConformanceEnforcement = EnforcementNone
				stub.overrideResponse = func(got string, b []byte) []byte {
					if got == leg {
						answer = append([]byte(nil), b...)
					}
					return b
				}
				if leg == "patient-dtr" {
					w := httptest.NewRecorder()
					g.handleUC07Start(w, httptest.NewRequest("POST", "/scenario/uc07/start", nil))
					if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &start) != nil {
						t.Fatalf("start: %d %s", w.Code, w.Body.String())
					}
				}
			}
			refused := false
			op := "pas-update-submit"
			if leg == "dtr-questionnaire-fetch" {
				op = "pas-submit"
			}
			g.cfg.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				b, e := io.ReadAll(r.Body)
				if e != nil {
					return nil, e
				}
				r.Body = io.NopCloser(bytes.NewReader(b))
				if strings.HasSuffix(r.URL.Path, "/authorize") && bytes.Contains(b, []byte(`"operation":"`+op+`"`)) {
					refused = true
					return errResp("synthetic authorize outage"), nil
				}
				return transport.RoundTrip(r)
			})}
			w := httptest.NewRecorder()
			switch leg {
			case "dtr-questionnaire-fetch":
				g.handleDispatch(w, httptest.NewRequest("POST", "/scenario/dispatch?wait=0", strings.NewReader(`{"member":"MBR-OX"}`)))
			case "federated-query":
				g.handleUC05(w, httptest.NewRequest("POST", "/scenario/uc05?wait=0", nil))
			case "patient-dtr":
				g.handleUC07Complete(w, httptest.NewRequest("POST", "/scenario/uc07/complete?wait=0", strings.NewReader(`{"resumeToken":"`+start.ResumeToken+`"}`)))
			}
			var out ConsumptionAttempt
			if !refused || w.Code != 502 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.ApplicationReply == nil {
				t.Fatalf("next dispatch failure lost reply: refused=%v status=%d %s", refused, w.Code, w.Body.String())
			}
			raw, _ := base64.StdEncoding.DecodeString(out.ApplicationReply.BodyBase64)
			if !bytes.Equal(raw, answer) || out.ApplicationReply.Leg != leg || out.Consumption.State != "unavailable" {
				t.Fatalf("wrong last reply: %+v", out)
			}
		})
	}
}

type consumptionMutationStore struct {
	Store
	ContinuationStore
	writes, puts int
}

func (s *consumptionMutationStore) StoreAuthNumber(order, auth string) error {
	s.writes++
	return s.Store.StoreAuthNumber(order, auth)
}
func (s *consumptionMutationStore) PutContinuation(c Continuation) (Continuation, error) {
	s.puts++
	return s.ContinuationStore.PutContinuation(c)
}

func TestLocalConsumptionFQTimeoutRetainsLastReply(t *testing.T) {
	for _, failedLeg := range []int{1, 2} {
		t.Run(fmt.Sprintf("FQ leg %d", failedLeg), func(t *testing.T) {
			member := "MBR-UC05"
			_, demo, _ := newCensusSoR().ResolvePatient(member)
			g, stub := newPendResumeFixture(t, pendFixtureOpts{member: member, birthDate: demo.BirthDate, familyName: demo.FamilyName, pendedItem: "functional-status", extraRoles: map[string]string{"facility": "facility"}})
			g.cfg.ConformanceEnforcement = EnforcementNone
			cs, _ := g.continuations()
			mutations := &consumptionMutationStore{Store: g.cfg.Store, ContinuationStore: cs}
			g.cfg.Store = mutations
			for _, holder := range []string{"payer", "facility"} {
				p, _ := g.cfg.Reg.Lookup(holder)
				p.MessageFrames = shnsdk.SupportedMessageFrames()
				g.cfg.Reg.Set(holder, p)
			}
			var lastBody []byte
			var lastLeg, lastCorr, activeCorr, lastVersion string
			fq := 0
			stub.overrideResponse = func(leg string, b []byte) []byte {
				if leg != "pas-claim" && leg != "federated-query" {
					return b
				}
				lastBody = append([]byte(nil), b...)
				lastLeg, lastCorr = leg, activeCorr
				lastVersion = ""
				headers := map[string]string{"Content-Type": "application/fhir+json"}
				if leg == "pas-claim" {
					lastVersion = "pa.pas@2.0"
					headers[shnsdk.FrameHeaderContractVersion] = lastVersion
				}
				frame, err := shnsdk.EncodeHTTPFrameHeaders(202, headers, b)
				if err != nil {
					t.Fatal(err)
				}
				return frame
			}
			var failedCorr string
			g.cfg.Client = &http.Client{Timeout: 100 * time.Millisecond, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				b, err := io.ReadAll(r.Body)
				if err != nil {
					return nil, err
				}
				r.Body = io.NopCloser(bytes.NewReader(b))
				if strings.HasSuffix(r.URL.Path, "/route") {
					env, err := shnsdk.DecodeEnvelope(b)
					if err != nil {
						return nil, err
					}
					activeCorr = env.Metadata.CorrelationID
					if env.Metadata.TransactionType == "federated-query" {
						fq++
						if fq == failedLeg {
							failedCorr = activeCorr
							<-r.Context().Done()
							return nil, r.Context().Err()
						}
					}
				}
				return stub.RoundTrip(r)
			})}
			w := httptest.NewRecorder()
			g.handleUC05(w, httptest.NewRequest("POST", "/scenario/uc05?wait=0", nil))
			var out struct {
				ApplicationReply *ApplicationReplyView
				Consumption      ConsumptionOutcome
				Decision         string
			}
			if w.Code != 504 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.ApplicationReply == nil {
				t.Fatalf("timeout lost prior reply: %d %s", w.Code, w.Body.String())
			}
			v := out.ApplicationReply
			raw, _ := base64.StdEncoding.DecodeString(v.BodyBase64)
			if !bytes.Equal(raw, lastBody) || v.Leg != lastLeg || v.CorrelationID != lastCorr || v.CorrelationID == failedCorr || v.Status != 202 || v.ContentType != "application/fhir+json" || v.DeclaredVersion != lastVersion || out.Consumption.State != "unavailable" || out.Decision != "" {
				t.Fatalf("wrong prior reply: %+v outcome=%+v", v, out.Consumption)
			}
			if fq != failedLeg || legAttempted(stub.legTypes, "pas-claim-update") || mutations.writes != 0 {
				t.Fatalf("timeout advanced workflow: fq=%d legs=%v writes=%d", fq, stub.legTypes, mutations.writes)
			}
			wantLeg := "pas-claim"
			if failedLeg == 2 {
				wantLeg = "federated-query"
			}
			if lastLeg != wantLeg {
				t.Fatalf("did not reach intended prior reply: %s", lastLeg)
			}
		})
	}
}

func TestLocalConsumptionFQConsentDenialRetainsPend(t *testing.T) {
	member := "MBR-UC05"
	_, demo, _ := newCensusSoR().ResolvePatient(member)
	g, stub := newPendResumeFixture(t, pendFixtureOpts{member: member, birthDate: demo.BirthDate, familyName: demo.FamilyName, pendedItem: "functional-status", extraRoles: map[string]string{"facility": "facility"}})
	g.cfg.ConformanceEnforcement = EnforcementNone
	var pend []byte
	stub.overrideResponse = func(leg string, b []byte) []byte {
		if leg == "pas-claim" {
			pend = append([]byte(nil), b...)
		}
		return b
	}
	denied := false
	g.cfg.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		r.Body = io.NopCloser(bytes.NewReader(b))
		if strings.HasSuffix(r.URL.Path, "/authorize") && bytes.Contains(b, []byte(`"operation":"federated-query-submit"`)) {
			denied = true
			return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader(`{"error":"consent denied"}`)), Header: http.Header{"Content-Type": []string{"application/json"}}}, nil
		}
		return stub.RoundTrip(r)
	})}
	writes := &workflowAuthWrites{Store: g.cfg.Store}
	g.cfg.Store = writes
	w := httptest.NewRecorder()
	g.handleUC05(w, httptest.NewRequest("POST", "/scenario/uc05?wait=0", nil))
	var out uc05Resp
	if !denied || w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || !out.ConsentDenied || !out.Pended || out.ApplicationReply == nil || out.Consumption.State != "available" {
		t.Fatalf("consent denial changed: %d %s", w.Code, w.Body.String())
	}
	raw, _ := base64.StdEncoding.DecodeString(out.ApplicationReply.BodyBase64)
	if !bytes.Equal(raw, pend) || out.ApplicationReply.Leg != "pas-claim" || writes.calls != 0 || legAttempted(stub.legTypes, "federated-query") || legAttempted(stub.legTypes, "pas-claim-update") {
		t.Fatal("denial lost pend or advanced workflow")
	}
}
