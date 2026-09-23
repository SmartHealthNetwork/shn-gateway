package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func TestLocalConsumptionInquiryPreservesApplicationReply(t *testing.T) {
	for _, kind := range []string{"approved", "opaque", "malformed FHIR", "unknown decision", "other authorization", "selected valid with unrelated patient", "backend error"} {
		t.Run(kind, func(t *testing.T) {
			g, stub := pasFollowSystem(t, "pended")
			req := httptest.NewRequest("POST", "/scenario/uc03", nil)
			pend, status, msg, err := g.submitClaimAndFollow(req.Context(), req, pasFollowSubmit(0))
			if status != 0 || err != nil {
				t.Fatalf("initial submit: %d %s %v", status, msg, err)
			}
			answer, err := stub.pasAnswer("approved", stub.submitCorr)
			if err != nil {
				t.Fatal(err)
			}
			answer = inquiryResponseBundle(answer, stub.clock())
			media := "application/fhir+json"
			appStatus, wantStatus := 202, 200
			if kind == "opaque" {
				answer = []byte("opaque\x00answer\xff\n")
				media = "application/octet-stream"
				wantStatus = 502
			}
			if kind == "malformed FHIR" {
				answer = []byte(`{"resourceType":"Bundle","entry":[`)
				wantStatus = 502
			}
			if kind == "unknown decision" {
				answer = bytes.ReplaceAll(answer, []byte(`"code":"A1"`), []byte(`"code":"ZZ"`))
				wantStatus = 502
			}
			if kind == "other authorization" {
				answer = bytes.ReplaceAll(answer, []byte(stub.submitCorr), []byte("unrelated-authorization"))
				wantStatus = 502
			}
			if kind == "selected valid with unrelated patient" {
				foreign, e := stub.pasAnswer("approved", "unrelated-authorization")
				if e != nil {
					t.Fatal(e)
				}
				foreign = bytes.ReplaceAll(foreign, []byte("Patient/"+pasFollowMember), []byte("Patient/foreign"))
				answer, _, _ = mixedInquiryFacts(t, answer, inquiryResponseBundle(foreign, stub.clock()))
			}
			if kind == "backend error" {
				answer = []byte("backend refused\x00\n")
				media = "application/problem+json"
				appStatus = 422
				wantStatus = 422
			}
			stub.inquiryWire, err = shnsdk.EncodeHTTPFrameHeaders(appStatus, map[string]string{"Content-Type": media, shnsdk.FrameHeaderContractVersion: "pa.pas@2.0"}, answer)
			if err != nil {
				t.Fatal(err)
			}
			peer, _ := g.cfg.Reg.Lookup("payer")
			peer.MessageFrames = shnsdk.SupportedMessageFrames()
			g.cfg.Reg.Set("payer", peer)
			w := httptest.NewRecorder()
			g.handlePAInquire(w, httptest.NewRequest("POST", "/scenario/pa/inquire", strings.NewReader(`{"continuation":"`+pend.Continuation+`","waitSeconds":0}`)))
			if w.Code != wantStatus {
				t.Fatalf("status=%d want=%d body=%s", w.Code, wantStatus, w.Body.String())
			}
			if !stub.sawInquiry {
				t.Fatal("actual inquiry did not run")
			}
			if kind == "backend error" {
				if !bytes.Equal(w.Body.Bytes(), answer) || w.Header().Get("Content-Type") != media {
					t.Fatal("backend error changed")
				}
				return
			}
			var out struct {
				Decision         string `json:"decision"`
				ApplicationReply struct {
					Status      int    `json:"status"`
					Media       string `json:"contentType"`
					Version     string `json:"declaredVersion"`
					Source      string `json:"versionSource"`
					Body        string `json:"bodyBase64"`
					Leg         string `json:"leg"`
					Correlation string `json:"correlationId"`
				} `json:"applicationReply"`
				Consumption struct{ State, Code string } `json:"consumption"`
			}
			if json.Unmarshal(w.Body.Bytes(), &out) != nil {
				t.Fatalf("not JSON: %s", w.Body.String())
			}
			raw, err := base64.StdEncoding.DecodeString(out.ApplicationReply.Body)
			if err != nil || !bytes.Equal(raw, answer) || out.ApplicationReply.Status != appStatus || out.ApplicationReply.Media != media || out.ApplicationReply.Version != "pa.pas@2.0" || out.ApplicationReply.Source != "producer" || out.ApplicationReply.Leg != "pas-claim-inquire" || out.ApplicationReply.Correlation == "" {
				t.Fatalf("received reply lost: %s", w.Body.String())
			}
			wantState := "available"
			if kind != "approved" && kind != "selected valid with unrelated patient" {
				wantState = "unavailable"
				if out.Decision != "" {
					t.Fatal("unreadable answer became decision")
				}
			}
			if kind != "approved" && kind != "selected valid with unrelated patient" {
				store, _ := g.continuations()
				still, _, err := store.ReadContinuation("provider", pend.Continuation)
				if err != nil || still.LastOutcome != ContinuationOutcomePended {
					t.Fatal("unavailable consumption changed local authorization state")
				}
			}
			if kind == "selected valid with unrelated patient" {
				store, _ := g.continuations()
				saved, _, e := store.ReadContinuation("provider", pend.Continuation)
				if e != nil {
					t.Fatal(e)
				}
				assertSelectedInquiryFacts(t, saved)
			}
			if out.Consumption.State != wantState {
				t.Fatalf("consumption=%+v want=%s", out.Consumption, wantState)
			}
		})
	}
}

// A payer may store the Claim and Patient under its own REST ids. The built-in
// scenario classifies the authenticated queued and final answers by the
// authorization's identifiers, while retaining the received reply unchanged.
func TestBuiltInPASWorkflow_PayerLocalReferencesReachFinalDecision(t *testing.T) {
	g, payer := pasFollowSystem(t, "pended")
	payer.payerLocalPASRefs = true
	r := httptest.NewRequest("POST", "/scenario/uc03", nil)
	pend, status, msg, err := g.submitClaimAndFollow(r.Context(), r, pasFollowSubmit(0))
	if status != 0 || err != nil || pend.Decision != PASDecisionPended || pend.Continuation == "" || pend.ReplyView == nil {
		t.Fatalf("queued payer answer refused: status=%d message=%q error=%v decision=%q continuation=%q", status, msg, err, pend.Decision, pend.Continuation)
	}
	payer.inquireAnswer = "approved"
	w := httptest.NewRecorder()
	g.handlePAInquire(w, httptest.NewRequest("POST", "/scenario/pa/inquire", strings.NewReader(`{"continuation":"`+pend.Continuation+`","waitSeconds":0}`)))
	var final struct {
		Decision         string
		ApplicationReply *ApplicationReplyView
		Consumption      ConsumptionOutcome
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &final) != nil || final.Decision != PASDecisionApproved || final.ApplicationReply == nil || final.Consumption.State != "available" || !payer.sawInquiry {
		t.Fatalf("final payer answer refused: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestLocalConsumptionSubmitRetainsReply(t *testing.T) {
	for _, kind := range []string{"approved", "opaque", "unknown decision", "backend error"} {
		t.Run(kind, func(t *testing.T) {
			g, stub := pasFollowSystem(t, "approved")
			answer, err := stub.pasAnswer("approved", "fixture-correlation")
			if err != nil {
				t.Fatal(err)
			}
			status := 201
			if kind == "opaque" {
				answer = []byte("unreadable\x00\xff\n")
			}
			if kind == "unknown decision" {
				answer = bytes.ReplaceAll(answer, []byte(`"code":"A1"`), []byte(`"code":"ZZ"`))
			}
			if kind == "backend error" {
				status = 422
				answer = []byte("backend error\x00\n")
			}
			stub.submitWire, err = shnsdk.EncodeHTTPFrameHeaders(status, map[string]string{"Content-Type": "application/octet-stream", shnsdk.FrameHeaderContractVersion: "pa.pas@2.0"}, answer)
			if err != nil {
				t.Fatal(err)
			}
			peer, _ := g.cfg.Reg.Lookup("payer")
			peer.MessageFrames = shnsdk.SupportedMessageFrames()
			g.cfg.Reg.Set("payer", peer)
			r := httptest.NewRequest("POST", "/scenario/uc03", nil)
			out, localStatus, msg, err := g.submitClaimAndFollow(r.Context(), r, pasFollowSubmit(0))
			if kind == "backend error" {
				var re *RelayError
				if !errors.As(err, &re) || re.Status != status || !bytes.Equal(re.Body, answer) || re.ContentType != "application/octet-stream" {
					t.Fatalf("backend answer changed %v", err)
				}
				return
			}
			if kind == "approved" {
				if localStatus != 0 || out.Decision != PASDecisionApproved {
					t.Fatalf("approved failed: %d %s %v", localStatus, msg, err)
				}
			} else if localStatus != 502 || out.Decision != "" {
				t.Fatalf("invalid result consumed: %d %s", localStatus, out.Decision)
			}
			if out.ReplyView == nil || out.ApplicationReply.Status != status {
				t.Fatal("received application reply lost")
			}
			raw, err := base64.StdEncoding.DecodeString(out.ReplyView.BodyBase64)
			if err != nil || !bytes.Equal(raw, answer) || out.ReplyView.Leg != "pas-claim" || out.ReplyView.DeclaredVersion != "pa.pas@2.0" || out.ReplyView.CorrelationID == "" {
				t.Fatal("reply changed")
			}
			want := "available"
			if kind != "approved" {
				want = "unavailable"
			}
			if out.Consumption.State != want {
				t.Fatalf("consumption=%+v", out.Consumption)
			}
			if kind == "approved" {
				wire, _ := json.Marshal(out.applyTo(uc03Resp{}))
				if !bytes.Contains(wire, []byte(`"applicationReply"`)) {
					t.Fatal("successful HTTP result loses reply")
				}
			}
		})
	}
}

func TestLocalConsumptionWaitRetainsFailedInquiry(t *testing.T) {
	for _, enforce := range []bool{false, true} {
		name := "parser"
		if enforce {
			name = "response enforcement"
		}
		t.Run(name, func(t *testing.T) { localConsumptionWaitRetainsFailedInquiry(t, enforce) })
	}
}

func localConsumptionWaitRetainsFailedInquiry(t *testing.T, enforce bool) {
	t.Helper()
	g, stub := pasFollowSystem(t, "pended")
	r := httptest.NewRequest("POST", "/scenario/uc03", nil)
	pend, status, msg, err := g.submitClaimAndFollow(r.Context(), r, pasFollowSubmit(0))
	if status != 0 || err != nil {
		t.Fatalf("submit: %d %s %v", status, msg, err)
	}
	prior, err := stub.pasAnswer("pended", stub.submitCorr)
	if err != nil {
		t.Fatal(err)
	}
	prior = inquiryResponseBundle(prior, stub.clock())
	if enforce {
		g.cfg.ConformanceEnforcement = EnforcementBasic
	}
	opaque := []byte("later inquiry\x00unreadable\xff\n")
	for _, body := range [][]byte{prior, opaque} {
		wire, err := shnsdk.EncodeHTTPFrameHeaders(202, map[string]string{"Content-Type": "application/fhir+json", shnsdk.FrameHeaderContractVersion: "pa.pas@2.0"}, body)
		if err != nil {
			t.Fatal(err)
		}
		stub.inquiryWireQueue = append(stub.inquiryWireQueue, wire)
	}
	peer, _ := g.cfg.Reg.Lookup("payer")
	peer.MessageFrames = shnsdk.SupportedMessageFrames()
	g.cfg.Reg.Set("payer", peer)
	w := httptest.NewRecorder()
	g.handlePAInquire(w, httptest.NewRequest("POST", "/scenario/pa/inquire", strings.NewReader(`{"continuation":"`+pend.Continuation+`","waitSeconds":1}`)))
	var out struct {
		Decision         string
		ApplicationReply *ApplicationReplyView
		Consumption      ConsumptionOutcome
		LastInquiry      *struct {
			ApplicationReply *ApplicationReplyView
			Consumption      struct {
				State   string
				Refusal *struct {
					Status                                    int
					Category, Rule, Gateway, Level, Direction string
				}
			}
		}
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if out.Decision != "pended" || out.Consumption.State != "available" || out.LastInquiry == nil || out.LastInquiry.ApplicationReply == nil || out.LastInquiry.Consumption.State != "unavailable" {
		t.Fatalf("lost attempt: %s", w.Body.String())
	}
	refusal := out.LastInquiry.Consumption.Refusal
	if enforce && (refusal == nil || refusal.Status != 502 || refusal.Category != "conformance_invalid" || refusal.Rule != "json.syntax" || refusal.Gateway != "provider" || refusal.Level != "basic" || refusal.Direction != "response") {
		t.Fatalf("lost local refusal: %s", w.Body.String())
	}
	view := out.LastInquiry.ApplicationReply
	if view.Status != 202 || view.ContentType != "application/fhir+json" || view.DeclaredVersion != "pa.pas@2.0" || view.VersionSource != "producer" || view.Leg != "pas-claim-inquire" || view.CorrelationID == "" {
		t.Fatalf("lost reply metadata: %+v", view)
	}
	store, _ := g.continuations()
	still, _, err := store.ReadContinuation("provider", pend.Continuation)
	if err != nil || still.LastOutcome != ContinuationOutcomePended {
		t.Fatal("failed inquiry changed terminal state")
	}
	got, _ := base64.StdEncoding.DecodeString(out.LastInquiry.ApplicationReply.BodyBase64)
	before, _ := base64.StdEncoding.DecodeString(out.ApplicationReply.BodyBase64)
	if !bytes.Equal(got, opaque) || !bytes.Equal(before, prior) || out.LastInquiry.ApplicationReply.CorrelationID == out.ApplicationReply.CorrelationID {
		t.Fatal("decision and latest unreadable reply conflated")
	}
}

func TestLocalConsumptionReplyViewBoundsAndOwnership(t *testing.T) {
	for _, size := range []int{0, shnsdk.MaxResponseBytes, shnsdk.MaxResponseBytes + 1} {
		raw := bytes.Repeat([]byte{'x'}, size)
		reply := ApplicationReply{Status: 202, Payload: relay.Exact(relay.NewBody(raw, relay.OriginPeerFrame), "application/octet-stream")}
		view, err := reply.view("pas-claim", "corr")
		if size > shnsdk.MaxResponseBytes {
			if err == nil || view != nil {
				t.Fatal("unbounded response view")
			}
			continue
		}
		if err != nil || view == nil {
			t.Fatalf("bounded reply unavailable: %v", err)
		}
		b, err := base64.StdEncoding.DecodeString(view.BodyBase64)
		if err != nil || !bytes.Equal(b, raw) {
			t.Fatal("view changed bytes")
		}
	}
	bad, err := relay.Authored(relay.BuilderSDKPASSubmit, []byte(`{"resourceType":"Bundle"}`), "application/json")
	if err != nil {
		t.Fatal(err)
	}
	if view, err := (ApplicationReply{Status: 200, Payload: bad}).view("pas-claim", "corr"); err == nil || view != nil {
		t.Fatal("view bypassed ownership")
	}
}

func TestLocalConsumptionInquiryEnforcementRetainsReply(t *testing.T) {
	for _, kind := range []string{"basic invalid", "strict invalid", "strict unavailable", "preexchange"} {
		t.Run(kind, func(t *testing.T) {
			g, stub := pasFollowSystem(t, "pended")
			r := httptest.NewRequest("POST", "/scenario/uc03", nil)
			pend, status, msg, err := g.submitClaimAndFollow(r.Context(), r, pasFollowSubmit(0))
			if status != 0 || err != nil {
				t.Fatalf("submit: %d %s %v", status, msg, err)
			}
			answer, err := stub.pasAnswer("approved", stub.submitCorr)
			if err != nil {
				t.Fatal(err)
			}
			answer = inquiryResponseBundle(answer, stub.clock())
			g.cfg.ConformanceEnforcement = EnforcementStrict
			g.cfg.SubjectReferenceResolver = subjectResolverFunc(func(_ context.Context, ref PatientReference) (string, bool, error) {
				// This fixture owns the member identifier and relative Patient linkage.
				known := ref.Holder == "provider" && ((ref.System == shnsdk.MemberSystem && ref.Value == pasFollowMember) || (ref.System == "fhir-relative" && ref.Value == "Patient/"+pasFollowMember))
				return stub.pci, known, nil
			})
			wantStatus, category, rule, level := 502, "conformance_invalid", "fhir.profile", "strict"
			if kind == "basic invalid" {
				g.cfg.ConformanceEnforcement = EnforcementBasic
				answer = []byte("invalid response\x00\xff")
				rule, level = "json.syntax", "basic"
			} else {
				v := syntheticEvidenceValidatorFunc(func(b []byte) (shnsdk.Result, error) {
					if kind == "preexchange" || bytes.Contains(b, []byte(`"resourceType":"ClaimResponse"`)) {
						if kind == "strict unavailable" {
							return shnsdk.Result{}, errors.New("private validator diagnostic")
						}
						return shnsdk.Result{Valid: false}, nil
					}
					return shnsdk.Result{Valid: true}, nil
				})
				g.cfg.Validator = v
				g.cfg.ValidatorsByLine = map[string]shnsdk.Validator{"2.0": v}
				if kind == "strict unavailable" {
					wantStatus, category = 503, "conformance_unavailable"
				}
			}
			stub.inquiryWire, err = shnsdk.EncodeHTTPFrameHeaders(202, map[string]string{"Content-Type": "application/fhir+json", shnsdk.FrameHeaderContractVersion: "pa.pas@2.0"}, answer)
			if err != nil {
				t.Fatal(err)
			}
			peer, _ := g.cfg.Reg.Lookup("payer")
			peer.MessageFrames = shnsdk.SupportedMessageFrames()
			g.cfg.Reg.Set("payer", peer)
			w := httptest.NewRecorder()
			g.handlePAInquire(w, httptest.NewRequest("POST", "/scenario/pa/inquire", strings.NewReader(`{"continuation":"`+pend.Continuation+`","waitSeconds":0}`)))
			var out struct {
				Decision         string
				ApplicationReply *ApplicationReplyView
				Consumption      struct {
					State   string
					Refusal *struct {
						Status                                    int
						Category, Rule, Gateway, Level, Direction string
					}
				}
			}
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if kind == "preexchange" {
				if w.Code < 400 || stub.sawInquiry || out.ApplicationReply != nil {
					t.Fatalf("fabricated reply: %d %s", w.Code, w.Body.String())
				}
				return
			}
			if !stub.sawInquiry {
				t.Fatalf("did not reach response guard: %d %s", w.Code, w.Body.String())
			}
			if w.Code != wantStatus || out.ApplicationReply == nil || out.Consumption.State != "unavailable" || out.Consumption.Refusal == nil {
				t.Fatalf("lost received refusal: %d %s", w.Code, w.Body.String())
			}
			got, err := base64.StdEncoding.DecodeString(out.ApplicationReply.BodyBase64)
			view, refusal := out.ApplicationReply, out.Consumption.Refusal
			if err != nil || !bytes.Equal(got, answer) || view.Status != 202 || view.ContentType != "application/fhir+json" || view.DeclaredVersion != "pa.pas@2.0" || view.VersionSource != "producer" || view.Leg != "pas-claim-inquire" || view.CorrelationID == "" {
				t.Fatalf("changed reply: %+v", view)
			}
			if refusal.Status != wantStatus || refusal.Category != category || refusal.Rule != rule || refusal.Gateway != "provider" || refusal.Level != level || refusal.Direction != "response" || bytes.Contains(w.Body.Bytes(), []byte("private validator diagnostic")) {
				t.Fatalf("unsafe/wrong refusal: %s", w.Body.String())
			}
			store, _ := g.continuations()
			still, _, err := store.ReadContinuation("provider", pend.Continuation)
			if err != nil || still.LastOutcome != ContinuationOutcomePended || out.Decision != "" {
				t.Fatal("response refusal changed terminal state")
			}
			retained, localStatus, _, originalErr := g.inquireContinuation(r.Context(), r, still)
			var ce *conformanceError
			if localStatus == 0 || !errors.As(originalErr, &ce) || ce.status != wantStatus || ce.Direction != "response" || retained.ReplyView == nil {
				t.Fatalf("original enforcement error or reply lost: %d %v", localStatus, originalErr)
			}

		})
	}
}

func mixedInquiryFacts(t *testing.T, selectedBody, foreignBody []byte) (mixed, selectedOnly, foreignOnly []byte) {
	t.Helper()
	var selected, foreign map[string]any
	if json.Unmarshal(selectedBody, &selected) != nil || json.Unmarshal(foreignBody, &foreign) != nil {
		t.Fatal("inquiry fixtures unreadable")
	}
	for label, bundle := range map[string]map[string]any{"selected": selected, "foreign": foreign} {
		for _, entry := range bundle["entry"].([]any) {
			r := entry.(map[string]any)["resource"].(map[string]any)
			if r["resourceType"] != "ClaimResponse" {
				continue
			}
			ids, _ := r["identifier"].([]any)
			r["identifier"] = append(ids, map[string]any{"system": "urn:fixture:response", "value": label})
			r["preAuthRef"] = label + "-preauth"
			for _, item := range r["item"].([]any) {
				it := item.(map[string]any)
				exts, _ := it["extension"].([]any)
				for _, number := range []string{"authorizationNumber", "administrationReferenceNumber"} {
					exts = append(exts, map[string]any{"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-" + number, "valueString": label + "-" + number})
				}
				it["extension"] = exts
			}
		}
	}
	selectedOnly, _ = json.Marshal(selected)
	foreignOnly, _ = json.Marshal(foreign)
	selected["entry"] = append(selected["entry"].([]any), foreign["entry"].([]any)...)
	selected["entry"] = append(selected["entry"].([]any), map[string]any{"fullUrl": "https://payer.example/fhir/Patient/foreign", "resource": map[string]any{"resourceType": "Patient", "id": "foreign", "identifier": []any{map[string]any{"system": "urn:unrelated", "value": "foreign"}}}})
	mixed, _ = json.Marshal(selected)
	return
}

func assertSelectedInquiryFacts(t *testing.T, c Continuation) {
	t.Helper()
	if c.PayerPreAuthRef != "selected-preauth" || !slices.Contains(c.PayerClaimResponseIDs, "urn:fixture:response|selected") || slices.Contains(c.PayerClaimResponseIDs, "urn:fixture:response|foreign") {
		t.Fatalf("unselected inquiry facts persisted: preauth=%s identifiers=%v", c.PayerPreAuthRef, c.PayerClaimResponseIDs)
	}
	if len(c.Items) == 0 {
		t.Fatal("fixture has no continuation items")
	}
	if c.Items[0].AuthorizationNumber != "selected-authorizationNumber" || c.Items[0].AdministrationReferenceNumber != "selected-administrationReferenceNumber" {
		t.Fatalf("wrong selected item facts: %+v", c.Items[0])
	}
}

func TestLocalConsumptionInquirySelectedFactsRemainIsolated(t *testing.T) {
	g, stub := pasFollowSystem(t, "pended")
	r := httptest.NewRequest("POST", "/scenario/uc03", nil)
	pend, status, msg, err := g.submitClaimAndFollow(r.Context(), r, pasFollowSubmit(0))
	if status != 0 || err != nil {
		t.Fatalf("initial submit: %d %s %v", status, msg, err)
	}
	selected, err := stub.pasAnswer("approved", stub.submitCorr)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := stub.pasAnswer("approved", "unrelated-authorization")
	if err != nil {
		t.Fatal(err)
	}
	mixed, selected, foreign := mixedInquiryFacts(t, inquiryResponseBundle(selected, stub.clock()), inquiryResponseBundle(foreign, stub.clock()))
	peer, _ := g.cfg.Reg.Lookup("payer")
	peer.MessageFrames = shnsdk.SupportedMessageFrames()
	g.cfg.Reg.Set("payer", peer)
	store, _ := g.continuations()
	mutations := &consumptionMutationStore{Store: g.cfg.Store, ContinuationStore: store}
	g.cfg.Store = mutations
	call := func(answer []byte, want int) {
		t.Helper()
		stub.inquiryWire, err = shnsdk.EncodeHTTPFrameHeaders(202, map[string]string{"Content-Type": "application/fhir+json", shnsdk.FrameHeaderContractVersion: "pa.pas@2.0"}, answer)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		g.handlePAInquire(w, httptest.NewRequest("POST", "/scenario/pa/inquire", strings.NewReader(`{"continuation":"`+pend.Continuation+`","waitSeconds":0}`)))
		var out struct {
			ApplicationReply *ApplicationReplyView
			Consumption      ConsumptionOutcome
			Decision         string
		}
		if json.Unmarshal(w.Body.Bytes(), &out) != nil || w.Code != want || out.ApplicationReply == nil {
			t.Fatalf("inquiry status/evidence: %d %s", w.Code, w.Body.String())
		}
		v := out.ApplicationReply
		raw, _ := base64.StdEncoding.DecodeString(v.BodyBase64)
		if !bytes.Equal(raw, answer) || v.Status != 202 || v.ContentType != "application/fhir+json" || v.DeclaredVersion != "pa.pas@2.0" || v.Leg != "pas-claim-inquire" || v.CorrelationID == "" {
			t.Fatalf("reply changed: %+v", v)
		}
		if want != 200 && (out.Consumption.State != "unavailable" || out.Decision != "") {
			t.Fatal("unrelated inquiry became decision")
		}
	}
	call(mixed, 200)
	before, _, err := store.ReadContinuation("provider", pend.Continuation)
	if err != nil {
		t.Fatal(err)
	}
	assertSelectedInquiryFacts(t, before)
	puts := mutations.puts
	call(foreign, 502)
	after, _, err := store.ReadContinuation("provider", pend.Continuation)
	if err != nil || !sameContinuation(before, after) || mutations.puts != puts || mutations.writes != 0 {
		t.Fatal("unrelated answer mutated continuation or wrote authorization")
	}
	call(selected, 200)
	after, _, err = store.ReadContinuation("provider", pend.Continuation)
	if err != nil || after.LastOutcome != ContinuationOutcomeApproved {
		t.Fatal("selected authorization lost")
	}
	assertSelectedInquiryFacts(t, after)
}

// sameContinuation compares every persisted field without inspecting sealed payloads.
func sameContinuation(a, b Continuation) bool {
	return a.ID == b.ID && a.Holder == b.Holder && a.PayerHolder == b.PayerHolder &&
		a.Line == b.Line && a.CorrID == b.CorrID && a.SubjectPCI == b.SubjectPCI &&
		a.MemberID == b.MemberID && a.SoRPatientID == b.SoRPatientID && a.OrderRef == b.OrderRef &&
		a.ProviderNPI == b.ProviderNPI && a.ClaimIdentifier == b.ClaimIdentifier &&
		a.ClaimType == b.ClaimType && a.ClaimPriority == b.ClaimPriority &&
		(a.ItemTraceNumbers == nil) == (b.ItemTraceNumbers == nil) && slices.Equal(a.ItemTraceNumbers, b.ItemTraceNumbers) &&
		(a.Items == nil) == (b.Items == nil) && slices.Equal(a.Items, b.Items) &&
		(a.PayerClaimResponseIDs == nil) == (b.PayerClaimResponseIDs == nil) && slices.Equal(a.PayerClaimResponseIDs, b.PayerClaimResponseIDs) &&
		a.PayerPreAuthRef == b.PayerPreAuthRef && a.LastOutcome == b.LastOutcome &&
		a.CreatedAt == b.CreatedAt && a.UpdatedAt == b.UpdatedAt
}
