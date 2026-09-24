// conformance_findingvalues_test.go — pins the ACTUAL findingContext value a
// real handler or helper hands validateGoverned, one representative
// ingress/egress pair per leg family (CRD, DTR incl. the adaptive path, PAS,
// federated query, patient access, inbound). The AST census in
// conformance_sources_test.go can only see that a handler CALLS
// withFindingContext somewhere in its body — it cannot see ordering, value
// truth, or a helper's own internal retagging. This file is the guard two
// successive audits (fix rounds 1 and 2 of the conformance-enforcement-levels
// work) showed that census alone cannot provide.
//
// Every case below drives a real production handler or helper — never a
// hand-built findingContext passed straight to validateFHIR/validateGoverned
// — through findingSpyValidator, a Validator double that always returns a
// valid verdict (so a multi-leg flow runs to completion undisturbed) and
// records, for every call, the resourceType of what it checked and
// findingContextFrom(ctx): the exact value validateGoverned itself would have
// read for emitFinding, had the verdict been invalid instead.
package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// findingSpyCall is one $validate call findingSpyValidator observed.
type findingSpyCall struct {
	resourceType string
	fc           findingContext
}

// findingSpyValidator always returns Valid (see file doc) and records every
// call's resourceType + the findingContext actually attached to ctx.
type findingSpyValidator struct {
	calls []findingSpyCall
}

func (s *findingSpyValidator) Validate(ctx context.Context, resourceJSON []byte, _ string) (shnsdk.Result, error) {
	var probe struct {
		ResourceType string `json:"resourceType"`
	}
	_ = json.Unmarshal(resourceJSON, &probe)
	s.calls = append(s.calls, findingSpyCall{resourceType: probe.ResourceType, fc: findingContextFrom(ctx)})
	return shnsdk.Result{Valid: true}, nil
}

// nth returns the (1-indexed) nth recorded call whose resourceType equals
// want, or fails the test naming every call observed (so a broken fixture
// fails loudly, not with a silent index-out-of-range).
func (s *findingSpyValidator) nth(t *testing.T, n int, resourceType string) findingSpyCall {
	t.Helper()
	i := 0
	for _, c := range s.calls {
		if c.resourceType != resourceType {
			continue
		}
		i++
		if i == n {
			return c
		}
	}
	t.Fatalf("no call #%d for resourceType %q observed (%d total calls: %+v)", n, resourceType, len(s.calls), s.calls)
	return findingSpyCall{}
}

// wantTag pins the fields a real check's findingContext must carry. Only
// PRESENCE of a correlation id is pinned (not its value, which is
// test-run-generated) — matching this task's own "" == not minted yet
// convention for a check that runs before its leg's id exists.
type wantTag struct {
	legType, whose, seam string
	corrPresent          bool
}

func assertTag(t *testing.T, label string, got findingContext, want wantTag) {
	t.Helper()
	if got.LegType != want.legType {
		t.Errorf("%s: LegType = %q, want %q", label, got.LegType, want.legType)
	}
	if got.Whose != want.whose {
		t.Errorf("%s: Whose = %q, want %q", label, got.Whose, want.whose)
	}
	if got.Seam != want.seam {
		t.Errorf("%s: Seam = %q, want %q", label, got.Seam, want.seam)
	}
	if (got.CorrelationID != "") != want.corrPresent {
		t.Errorf("%s: CorrelationID = %q, want present=%v", label, got.CorrelationID, want.corrPresent)
	}
}

// ---- CRD family ----

// TestPinnedFindingContext_CRDIngress drives handleCRDNativeInbound directly
// (the standing pattern this package's own inbound tests already use — see
// native_crd_relay_test.go's TestHandleCRDNativeInbound_ValidatorRunsOnBoth
// family) with the request context pre-tagged EXACTLY as the real
// handleInbound dispatcher sets it (inboundSeamFor("crd-order-select"), Whose
// "peer") before the call, so the check under test is the inner handler's OWN
// use of that tag, not a hand-built substitute for it.
func TestPinnedFindingContext_CRDIngress(t *testing.T) {
	g, requester := newInboundTestGateway(t, true)
	spy := &findingSpyValidator{}
	g.cfg.Validator = spy
	// A non-2xx stop: the ingress checks under test run BEFORE the Responder is
	// ever reached, so a cheap refusal here just ends the request cleanly.
	g.cfg.Responder = pasResultResponder{result: LegResult{Status: http.StatusBadRequest, Message: "test stop"}}
	pci, _, _ := g.cfg.SoR.ResolvePatient("MBR-COVERED")

	env := shnsdk.Envelope{}
	env.Metadata.CorrelationID, env.Metadata.Sender = "corr-crd-ingress-1", requester.ID
	req := conformantCRD("MBR-COVERED", "72148")

	r := newSignedInboundRequest(t, g, requester.ID)
	r = r.WithContext(withFindingContext(r.Context(), findingContext{
		LegType: "crd-order-select", CorrelationID: env.Metadata.CorrelationID,
		Seam: inboundSeamFor("crd-order-select"), Whose: "peer",
	}))
	rec := httptest.NewRecorder()
	g.handleCRDNativeInbound(rec, r, env, shnsdk.Token{Subject: pci}, req, "")

	call := spy.nth(t, 1, "ServiceRequest")
	assertTag(t, "CRD ingress (peer's incoming order)", call.fc, wantTag{
		legType: "crd-order-select", whose: "peer", seam: "payer-native", corrPresent: true,
	})
}

// TestPinnedFindingContext_CRDEgress drives handleUC02 (originateNoPACRD) —
// this participant's own CRD order egress-validated before it is sent.
func TestPinnedFindingContext_CRDEgress(t *testing.T) {
	env := newInProcessExchange(t)
	spy := &findingSpyValidator{}
	env.originator.cfg.Validator = spy
	// The demo lane (what an unset ORIGINATION_PROFILE normalizes to in
	// gateway/app.go's loadConfig) puts UC-02 on MBR-D-UC02, the member whose
	// open order in the census stand-in IS the UC-02 hospital-bed order. On
	// the bare default lane UC-02 runs as MBR-COVERED, whose one order-bearing
	// scenario is UC-03's oxygen arm, and orderSourceContext refuses the tuple
	// mismatch 502 before any egress check runs.
	env.originator.cfg.OriginationProfile = "demo"

	rec := httptest.NewRecorder()
	env.originator.handleUC02(rec, httptest.NewRequest(http.MethodPost, "/scenario/uc02", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	call := spy.nth(t, 1, "ServiceRequest")
	assertTag(t, "CRD egress (this participant's own order)", call.fc, wantTag{
		legType: "crd-order-select", whose: "own", seam: "originate", corrPresent: false,
	})
}

// TestPinnedFindingContext_UC01EligibilityEgress drives handleScenario (UC-01)
// — this participant's own built CoverageEligibilityRequest, egress-validated
// before it is sent. This check used to bypass the choke point entirely (no
// finding was ever emitted, so nothing downstream ever read the context tag);
// now that it is routed through validateGoverned, an untagged context would
// surface as legType "unknown" on a live path, which is exactly the
// evidence-quality gap tagging exists to close. This row proves the tag
// precedes the check and is true for these bytes: legType/seam/whose are all
// populated, and correlationId is correctly ABSENT (not invented) — it is not
// minted until after this check, just before the Hub round trip.
func TestPinnedFindingContext_UC01EligibilityEgress(t *testing.T) {
	env := newInProcessExchange(t)
	spy := &findingSpyValidator{}
	env.originator.cfg.Validator = spy
	// A minimal but resourceType-valid CoverageEligibilityResponse: ParseEligibilityResponse
	// (run after the checks under test) only needs the resourceType to match — the
	// response's own content is not what this row is about.
	env.payerReturns(LegResult{Response: testResponse([]byte(`{"resourceType":"CoverageEligibilityResponse"}`))})

	rec := httptest.NewRecorder()
	env.originator.handleScenario(rec, httptest.NewRequest(http.MethodPost, "/scenario/uc01", strings.NewReader(`{"branch":"covered"}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	call := spy.nth(t, 1, "CoverageEligibilityRequest")
	assertTag(t, "UC-01 eligibility egress (this participant's own request)", call.fc, wantTag{
		legType: "coverage-eligibility", whose: "own", seam: "originate", corrPresent: false,
	})
}

// ---- Inbound family (the answer half of handleInbound's tag) ----

// TestPinnedFindingContext_InboundEgress drives handlePASNativeInbound
// directly, pre-tagged exactly as handleInbound sets it for an inbound
// pas-claim leg (Whose "peer", the REQUEST's own direction) — proving the
// direction-flip retag added after g.admit (before the payer's own
// ClaimResponse/side-effect FHIR are validated) actually reaches the check
// as Whose "own".
func TestPinnedFindingContext_InboundEgress(t *testing.T) {
	g, requester := newInboundTestGateway(t, true)
	spy := &findingSpyValidator{}
	g.cfg.Validator = spy
	// A conformant terminal PAS response Bundle, sealed as an answer this
	// responder AUTHORED (testResponse — ResponseRelayed() false), which is
	// what validatePASResult certifies; a relayedResponse would stand the
	// egress check down (R-8) and there would be no call to read the tag off.
	// ResponseSubjectForeign stands the member-fence down (the fixture's own
	// subject, SubscriberExample, is not MBR-COVERED).
	answer := pasBundleWithResponse(t, []byte(assemblyRealPending), []byte(assemblyRealTerminal))
	commits, rollbacks := 0, 0
	result := LegResult{
		Response: testResponse(answer), ResponseSubjectForeign: true,
		Commit:   func() error { commits++; return nil },
		Rollback: func() { rollbacks++ },
	}
	g.cfg.Responder = pasResultResponder{result: result}
	pci, _, _ := g.cfg.SoR.ResolvePatient("MBR-COVERED")

	env := shnsdk.Envelope{}
	env.Metadata.CorrelationID, env.Metadata.Sender = "corr-pas-inbound-1", requester.ID
	tok := shnsdk.Token{Subject: pci, CorrelationID: env.Metadata.CorrelationID}
	req := conformantPASBundleWithQR(t, "MBR-COVERED")

	r := newSignedInboundRequest(t, g, requester.ID)
	r = r.WithContext(withFindingContext(r.Context(), findingContext{
		LegType: "pas-claim", CorrelationID: env.Metadata.CorrelationID,
		Seam: inboundSeamFor("pas-claim"), Whose: "peer",
	}))
	rec := httptest.NewRecorder()
	g.handlePASNativeInbound(rec, r, env, tok, req, "pa.pas@2.0")

	if commits != 1 || rollbacks != 0 || rec.Code != http.StatusOK {
		t.Fatalf("commits=%d rollbacks=%d status=%d body=%s — fixture is not reaching the egress checks cleanly",
			commits, rollbacks, rec.Code, rec.Body.String())
	}
	call := spy.nth(t, 1, "Bundle")
	assertTag(t, "inbound egress (this payer's own authored response, after the admit direction-flip)", call.fc, wantTag{
		legType: "pas-claim", whose: "own", seam: "payer-native", corrPresent: true,
	})
}

// TestPinnedFindingContext_PayerDTREgress drives handleDTRInbound
// (payer.go) directly, pre-tagged exactly as handleInbound sets it for an
// inbound dtr-questionnaire-fetch leg — proving the direction-flip retag at
// payer.go:92-94 (after g.admit, before the payer's own $questionnaire-package
// answer is validated) actually reaches the check as Whose "own". This is
// the row that closes the gap the re-review found: this exact retag had no
// test at all.
func TestPinnedFindingContext_PayerDTREgress(t *testing.T) {
	g, requester := newInboundTestGateway(t, true)
	spy := &findingSpyValidator{}
	g.cfg.Validator = spy
	// An authored (not relayed) package: ResponseRelayed() must be false or
	// the egress check under test is skipped by the R-8 near-relay rule
	// (payer.go's own comment on this exact call site).
	pkg := []byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`)
	g.cfg.Responder = pasResultResponder{result: LegResult{Response: testResponse(pkg)}}
	pci, _, _ := g.cfg.SoR.ResolvePatient("MBR-COVERED")

	env := shnsdk.Envelope{}
	env.Metadata.CorrelationID, env.Metadata.Sender = "corr-dtr-egress-1", requester.ID
	// A $questionnaire-package input sent naming the operation, for the
	// covered member the token authorizes.
	req := dtrFramedPackageFor(t, g)

	r := newSignedInboundRequest(t, g, requester.ID)
	r = r.WithContext(withRequestFrameOperation(r.Context(), shnsdk.FrameOperationQuestionnairePackage))
	r = r.WithContext(withFindingContext(r.Context(), findingContext{
		LegType: "dtr-questionnaire-fetch", CorrelationID: env.Metadata.CorrelationID,
		Seam: inboundSeamFor("dtr-questionnaire-fetch"), Whose: "peer",
	}))
	rec := httptest.NewRecorder()
	g.handleDTRInbound(rec, r, env, shnsdk.Token{Subject: pci}, req, "pa.dtr@2.0")

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s — fixture is not reaching the egress check cleanly", rec.Code, rec.Body.String())
	}
	call := spy.nth(t, 1, "Bundle")
	assertTag(t, "payer DTR egress (this payer's own questionnaire-package answer, after the admit direction-flip)", call.fc, wantTag{
		legType: "dtr-questionnaire-fetch", whose: "own", seam: "payer-native", corrPresent: true,
	})
}

// TestPinnedFindingContext_InboundUpdateEgress drives
// handlePASUpdateNativeInbound directly, pre-tagged exactly as handleInbound
// sets it for an inbound pas-claim-update leg — proving the direction-flip
// retag at pas_native.go:413-415 actually reaches the check as Whose "own".
// The other unguarded row the re-review found; mirrors
// TestPinnedFindingContext_InboundEgress's pattern on the amendment leg —
// the check under test is validatePASResult's validateFHIRForContract call,
// the one arm it has for an answer this gateway produced.
func TestPinnedFindingContext_InboundUpdateEgress(t *testing.T) {
	g, requester := newInboundTestGateway(t, true)
	spy := &findingSpyValidator{}
	g.cfg.Validator = spy
	// A conformant terminal PAS response Bundle, sealed as AUTHORED
	// (testResponse — ResponseRelayed() false) so validatePASResult certifies
	// it rather than standing down under R-8. ResponseSubjectForeign stands
	// the member-fence down (the fixture's own subject, SubscriberExample, is
	// not MBR-COVERED) — the same posture a real RI-relayed answer carries.
	answer := pasBundleWithResponse(t, []byte(assemblyRealPending), []byte(assemblyRealTerminal))
	result := LegResult{Response: testResponse(answer), ResponseSubjectForeign: true}
	g.cfg.Responder = pasResultResponder{result: result}
	pci, _, _ := g.cfg.SoR.ResolvePatient("MBR-COVERED")

	env := shnsdk.Envelope{}
	env.Metadata.CorrelationID, env.Metadata.Sender = "corr-pas-update-inbound-1", requester.ID
	tok := shnsdk.Token{Subject: pci, CorrelationID: env.Metadata.CorrelationID}
	req := originatorBuiltConformantUpdateBundle(t)

	r := newSignedInboundRequest(t, g, requester.ID)
	r = r.WithContext(withFindingContext(r.Context(), findingContext{
		LegType: "pas-claim-update", CorrelationID: env.Metadata.CorrelationID,
		Seam: inboundSeamFor("pas-claim-update"), Whose: "peer",
	}))
	rec := httptest.NewRecorder()
	g.handlePASUpdateNativeInbound(rec, r, env, tok, req, "pa.pas@2.0")

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s — fixture is not reaching the egress check cleanly", rec.Code, rec.Body.String())
	}
	call := spy.nth(t, 1, "Bundle")
	assertTag(t, "inbound update egress (this payer's own authored amendment response, after the admit direction-flip)", call.fc, wantTag{
		legType: "pas-claim-update", whose: "own", seam: "payer-native", corrPresent: true,
	})
}

// TestPinnedFindingContext_InboundInquireEgress drives handlePASInquireInbound
// directly, pre-tagged exactly as handleInbound sets it for an inbound
// pas-claim-inquire leg — proving the direction-flip retag in inquire.go
// actually reaches the check as Whose "own". The third PAS leg, and the last
// one to get its flip: the inquiry leg arrived after the submit and amendment
// legs, so the flip was written without a row of its own and a later edit
// could have dropped it with every test still green — the exact defect class
// the census in conformance_sources_test.go cannot see.
//
// The correlation-id assertion below is the reason this row is not redundant
// with a Whose-only check. The flip must RETAG the context handleInbound
// already resolved, not build a new one: a replacement
// withFindingContext(..., findingContext{LegType: …, Whose: "own", Seam: …})
// would satisfy every other field here and silently drop the inherited
// correlation id, leaving every finding this leg emits untraceable to the
// exchange that raised it. So this row pins the id's VALUE against the
// envelope's, not merely assertTag's presence.
func TestPinnedFindingContext_InboundInquireEgress(t *testing.T) {
	g, requester := newInboundTestGateway(t, true)
	spy := &findingSpyValidator{}
	g.cfg.Validator = spy
	// The synthetic 2.0/2.1 inquiry answer (a response Bundle — the shape
	// validatePASInquiryAnswer accepts), sealed as AUTHORED (testResponse —
	// ResponseRelayed() false) so validatePASResult certifies it rather than
	// standing down under R-8. ResponseSubjectForeign stands the member fence
	// down: the fixture answers in the payer's own namespace
	// (SubscriberExample), not MBR-COVERED — the posture a real relayed RI
	// answer carries.
	answer := inquiryFixture(t, "pas-inquiry-response-2.0.json")
	g.cfg.Responder = pasResultResponder{result: LegResult{
		Response: testResponse(answer), ResponseSubjectForeign: true,
	}}
	pci, _, _ := g.cfg.SoR.ResolvePatient("MBR-COVERED")

	env := shnsdk.Envelope{}
	env.Metadata.CorrelationID, env.Metadata.Sender = "corr-pas-inquire-inbound-1", requester.ID
	tok := shnsdk.Token{Subject: pci, CorrelationID: env.Metadata.CorrelationID}
	req := inquiryBundle("MBR-COVERED", "", "TRN-1", "72148")

	r := newSignedInboundRequest(t, g, requester.ID)
	r = r.WithContext(withFindingContext(r.Context(), findingContext{
		LegType: "pas-claim-inquire", CorrelationID: env.Metadata.CorrelationID,
		Seam: inboundSeamFor("pas-claim-inquire"), Whose: "peer",
	}))
	rec := httptest.NewRecorder()
	g.handlePASInquireInbound(rec, r, env, tok, req, "pa.pas@2.0")

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s — fixture is not reaching the egress check cleanly", rec.Code, rec.Body.String())
	}
	const label = "inbound inquiry egress (this payer's own authored inquiry answer, after the direction flip)"
	call := spy.nth(t, 1, "Bundle")
	assertTag(t, label, call.fc, wantTag{
		legType: "pas-claim-inquire", whose: "own", seam: "payer-native", corrPresent: true,
	})
	if call.fc.CorrelationID != env.Metadata.CorrelationID {
		t.Errorf("%s: CorrelationID = %q, want the envelope's own %q — the flip must carry handleInbound's id, not re-derive it",
			label, call.fc.CorrelationID, env.Metadata.CorrelationID)
	}
}

// ---- Patient access family (own/egress only — a REST read has no ingress leg) ----

// TestPinnedFindingContext_PatientAccessEgress drives the real router
// (g.Handler()) into handlePatientAccessEOB -> serveEOB with a valid
// patient-access bearer token and one seeded EOB.
func TestPinnedFindingContext_PatientAccessEgress(t *testing.T) {
	authzPub, authzPriv := genED25519(t)
	payEncPub, payEncPriv := genKeyPair(t)
	_, paySignPriv := genED25519(t)
	audit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(audit.Close)

	sor := newCensusSoR()
	if err := sor.RecordEOB("p-1", "eob-1", []byte(`{"resourceType":"ExplanationOfBenefit","id":"eob-1"}`)); err != nil {
		t.Fatal(err)
	}
	spy := &findingSpyValidator{}
	g := mustNew(t, Config{
		Role:            "payer",
		HolderID:        "payer",
		Identity:        shnsdk.Identity{HolderID: "payer", SignPriv: paySignPriv, EncPub: payEncPub, EncPriv: payEncPriv},
		AuthzURL:        "http://stub.test",
		AuthzPub:        authzPub,
		HubTransportPub: authzPub,
		Reg:             shnsdk.NewRegistry(),
		Validator:       spy,
		SoR:             sor,
		Store:           sor,
		Responder:       unusedResponder{},
		Clock:           fixedClock,
		Client:          audit.Client(),
		AuditURL:        audit.URL,
	})

	tok := signTestToken(shnsdk.Token{
		Operation: "patient-access-read", Scope: "patient-access-only", Subject: "p-1",
		Frame: "patient-access", Holder: "phg", CorrelationID: "corr-pa-1",
		Expiry: fixedClock().Add(time.Hour),
	}, authzPriv)
	raw, _ := json.Marshal(tok)
	req := httptest.NewRequest(http.MethodGet, "/ExplanationOfBenefit?patient=p-1", nil)
	req.Header.Set("Authorization", "Bearer "+base64.StdEncoding.EncodeToString(raw))
	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	call := spy.nth(t, 1, "Bundle")
	assertTag(t, "patient access egress (this payer's own EOB searchset)", call.fc, wantTag{
		legType: "patient-access-read", whose: "own", seam: "originate", corrPresent: true,
	})
}

// ---- DTR family (including the adaptive path) + PAS family ----
// Both driven off handleUC04's demo lane via the pend/resume harness
// (pendstate_pin_test.go's newPendResumeFixture), which already runs the
// full crd-order-select -> dtr-questionnaire-fetch -> pas-claim (pended) ->
// pas-claim-update (approved) chain and is proven working by
// TestHandleUC04_PinsBothLegsAcrossMidRequestDrift.

func TestPinnedFindingContext_DTRAndPAS(t *testing.T) {
	gw, _ := newPendResumeFixture(t, pendFixtureOpts{
		member: "MBR-UC04", birthDate: "1982-11-03", familyName: "Chen",
		pendedItem: "operative-diagnostic-report",
	})
	spy := &findingSpyValidator{}
	gw.cfg.Validator = spy

	rec := httptest.NewRecorder()
	gw.handleUC04(rec, httptest.NewRequest(http.MethodPost, "/scenario/uc04", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	// DTR ingress: the payer's $questionnaire-package answer (a Bundle wrapping
	// the fetched Questionnaire) — the FIRST Bundle-typed call in the run.
	dtrIngress := spy.nth(t, 1, "Bundle")
	assertTag(t, "DTR ingress (payer's questionnaire package)", dtrIngress.fc, wantTag{
		legType: "dtr-questionnaire-fetch", whose: "peer", seam: "originate", corrPresent: true,
	})

	// DTR egress: the populated QuestionnaireResponse this participant built —
	// the FIRST QuestionnaireResponse-typed call (it runs before any PAS-phase
	// QR is embedded and re-checked by validatePASAttachments).
	dtrEgress := spy.nth(t, 1, "QuestionnaireResponse")
	assertTag(t, "DTR egress (this participant's own populated QR)", dtrEgress.fc, wantTag{
		legType: "dtr-questionnaire-fetch", whose: "own", seam: "originate", corrPresent: true,
	})

	// PAS: the pended (submit) answer is the Da Vinci Bundle-wrapped pended
	// shape (testPendedResponse); the approved (update) answer is a bare
	// ClaimResponse (homeOxygenApprovedClaimResponse) — so, among the
	// Bundle-typed calls, in call order: #1 DTR ingress (peer), #2 PAS submit
	// egress (own), #3 PAS submit ingress (peer), #4 PAS update egress (own);
	// the update's ingress is the run's only "ClaimResponse"-typed call.
	pasSubmitEgress := spy.nth(t, 2, "Bundle")
	assertTag(t, "PAS egress (this participant's own submit bundle)", pasSubmitEgress.fc, wantTag{
		legType: "pas-claim", whose: "own", seam: "originate", corrPresent: true,
	})
	pasSubmitIngress := spy.nth(t, 3, "Bundle")
	assertTag(t, "PAS ingress (payer's pended response)", pasSubmitIngress.fc, wantTag{
		legType: "pas-claim", whose: "peer", seam: "originate", corrPresent: true,
	})
	pasUpdateEgress := spy.nth(t, 4, "Bundle")
	assertTag(t, "PAS egress (this participant's own amendment bundle)", pasUpdateEgress.fc, wantTag{
		legType: "pas-claim-update", whose: "own", seam: "originate", corrPresent: true,
	})
	pasUpdateIngress := spy.nth(t, 1, "ClaimResponse")
	assertTag(t, "PAS ingress (payer's approved response, amendment)", pasUpdateIngress.fc, wantTag{
		legType: "pas-claim-update", whose: "peer", seam: "originate", corrPresent: true,
	})
}

// TestPinnedFindingContext_DTRAdaptiveIngress drives nextQuestionLeg directly
// — the $next-question round helper whose own retag names the payer's answer
// truthfully, regardless of which attestation path called it — over the SAME
// in-process origination harness used by the CRD family, with a
// hand-built crdDtrResult naming only what nextQuestionLeg itself reads
// (recipient/dtrLine/patientRef/pci). This is a real helper call, not a
// hand-built findingContext: the retag under test is nextQuestionLeg's own.
// It is the one case in this file where the checked resource has no
// resourceType-based ambiguity to resolve: validateFHIRPayerIngress here
// validates the OUTER Parameters wrapper (parseNextQuestionResponse extracts
// the QuestionnaireResponse only AFTER that check passes), and no other test
// in this file drives a $next-question round.
func TestPinnedFindingContext_DTRAdaptiveIngress(t *testing.T) {
	env := newInProcessExchange(t)
	spy := &findingSpyValidator{}
	env.originator.cfg.Validator = spy

	// The harness's payer entry declares no RequestFrames; nextQuestionLeg
	// refuses before ever reaching OriginateLeg unless the recipient declares
	// framed DTR operations (framedDTRRefusal) — mirrors every other in-package
	// DTR-leg harness (originate_crd_test.go, originate_homeoxygen_test.go, …).
	entry, ok := env.originator.cfg.Reg.Lookup(env.payerID)
	if !ok {
		t.Fatalf("harness payer %q not registered", env.payerID)
	}
	entry.RequestFrames = dtrOperationFrames
	env.originator.cfg.Reg.Set(env.payerID, entry)

	pci, _, _ := env.originator.cfg.SoR.ResolvePatient("MBR-COVERED")
	res := crdDtrResult{
		recipient: env.payerID, dtrLine: "2.0",
		patientRef: "Patient/MBR-COVERED", pci: pci,
	}
	const canonical = "http://example.org/Questionnaire/adaptive"
	reqQR := json.RawMessage(`{"resourceType":"QuestionnaireResponse","status":"in-progress"}`)

	// The payer's $next-question answer: a Parameters wrapper carrying the
	// grown QuestionnaireResponse, whose contained Questionnaire names the
	// delivered items and whose own subject matches the patient (the two
	// fences nextQuestionLeg runs after the ingress check).
	answer, err := json.Marshal(map[string]any{
		"resourceType": "Parameters",
		"parameter": []map[string]any{{
			"name": "questionnaire-response",
			"resource": map[string]any{
				"resourceType": "QuestionnaireResponse",
				"status":       "in-progress",
				"subject":      map[string]string{"reference": res.patientRef},
				"contained": []map[string]any{{
					"resourceType": "Questionnaire",
					"id":           "contained-questionnaire",
					"item":         []map[string]any{{"linkId": "2", "type": "group"}},
				}},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	env.payerReturns(LegResult{Response: testResponse(answer)})

	_, status, msg, lerr := env.originator.nextQuestionLeg(env.ctx, env.req, res, canonical, reqQR)
	if status != 0 {
		t.Fatalf("nextQuestionLeg refused: %d %s (%v)", status, msg, lerr)
	}

	call := spy.nth(t, 1, "Parameters")
	assertTag(t, "DTR ingress, adaptive $next-question round (payer's answer)", call.fc, wantTag{
		legType: "dtr-questionnaire-fetch", whose: "peer", seam: "originate", corrPresent: true,
	})
}

// ---- Federated query family ----

// fqFacilitySoR wraps censusSoR (Patient resolution + Store, both real) but
// forces the search path unsupported, so facilityRecordsBundle falls back to
// FacilityRecordsContext — matching censusSoR's own pre-built MBR-UC05
// fixture (FacilityRecords: an operative DiagnosticReport + its
// DocumentReference), which the census fixture's SearchPatientContext (a
// Coverage-only stub) never serves.
type fqFacilitySoR struct {
	*censusSoR
}

func (s *fqFacilitySoR) SearchPatientContext(_ context.Context, _, _ string, _ ...SearchDateRange) (SearchResult, error) {
	return SearchResult{}, &SearchError{Outcome: SearchUnsupported, Reason: "test: force the FacilityRecordsContext fallback"}
}

// consentGrantStub answers /authorize (delegating to inboundAuthzStub) and
// /check (an unconditional permit) — the two outbound calls
// handleFederatedQueryInbound's consent-gated disclosure makes.
type consentGrantStub struct {
	authz      inboundAuthzStub
	consentRef string
}

func (s *consentGrantStub) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/check") {
		b, _ := json.Marshal(shnsdk.ConsentCheckResponse{Permit: true, ConsentRef: s.consentRef})
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(b)),
			Header: http.Header{"Content-Type": []string{"application/json"}}}, nil
	}
	return s.authz.RoundTrip(req)
}

// TestPinnedFindingContext_FederatedQueryEgress drives handleFederatedQueryInbound
// directly: the facility's own disclosure (records, Patient identity
// binding, Provenance, and the sealed fulfillment) egress-validated after
// the direction flip. Pre-tagged exactly as handleInbound sets it for an
// inbound federated-query leg.
func TestPinnedFindingContext_FederatedQueryEgress(t *testing.T) {
	authzPub, authzPriv := genED25519(t)
	_, paySignPriv := genED25519(t)
	payEncPub, payEncPriv := genKeyPair(t)
	reqEncPub, _ := genKeyPair(t)
	reqSignPub, _ := genED25519(t)
	const consentRef = "consent-ref-1"

	reg := shnsdk.NewRegistry()
	reg.Set("requester", shnsdk.RegistryEntry{ID: "requester", Role: "provider", EncPub: reqEncPub, SignPub: reqSignPub, MessageFrames: shnsdk.SupportedMessageFrames()})

	spy := &findingSpyValidator{}
	g := mustNew(t, Config{
		Role:            "payer",
		HolderID:        "payer",
		Identity:        shnsdk.Identity{HolderID: "payer", SignPriv: paySignPriv, EncPub: payEncPub, EncPriv: payEncPriv},
		AuthzURL:        "http://stub.test",
		AuthzPub:        authzPub,
		HubTransportPub: authzPub,
		ConsentURL:      "http://stub.test/consent",
		Reg:             reg,
		Validator:       spy,
		SoR:             &fqFacilitySoR{censusSoR: newCensusSoR()},
		Store:           newCensusSoR(),
		Responder:       unusedResponder{},
		Clock:           fixedClock,
		Client:          &http.Client{Transport: &consentGrantStub{authz: inboundAuthzStub{authzPriv: authzPriv, clock: fixedClock}, consentRef: consentRef}},
	})

	const member = "MBR-UC05"
	pci, _, _ := g.cfg.SoR.ResolvePatient(member)
	env := shnsdk.Envelope{}
	env.Metadata.CorrelationID, env.Metadata.Sender = "corr-fq-1", "requester"
	env.Metadata.ConsentRef = consentRef
	query, err := shnsdk.BuildCDexTaskDataRequest("Patient/"+member, "DiagnosticReport", "2020-01-01", "2030-01-01",
		shnsdk.CDexTaskMeta{AuthoredOn: fixedClock(), Requester: "requester", Owner: "payer"})
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(context.Background())
	r = r.WithContext(withFindingContext(r.Context(), findingContext{
		LegType: "federated-query", CorrelationID: env.Metadata.CorrelationID,
		Seam: inboundSeamFor("federated-query"), Whose: "peer",
	}))
	rec := httptest.NewRecorder()
	g.handleFederatedQueryInbound(rec, r, env, shnsdk.Token{Subject: pci}, query, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	// facilityRecordsBundle validates the disclosed DiagnosticReport, the
	// authored Patient identity binding and the authored Provenance
	// separately (cdexrecords.go:132/141/160); handleFederatedQueryInbound
	// then validates the sealed fulfillment (a Task) once more
	// (inbound.go:469). All four must show the SAME post-direction-flip tag.
	for _, resourceType := range []string{"DiagnosticReport", "Patient", "Provenance", "Task"} {
		call := spy.nth(t, 1, resourceType)
		assertTag(t, "federated query egress ("+resourceType+")", call.fc, wantTag{
			legType: "federated-query", whose: "own", seam: "facility-inbound", corrPresent: true,
		})
	}
}

// TestPinnedFindingContext_FederatedQueryIngress reuses
// pendstate_pin_test.go's newPendResumeFixture — the same proven harness
// TestHandleUC05_FederatedQueryIngressValidatesOnDemoLane already drives
// through handleUC05's real federated-query loop — to pin the ingress check's
// findingContext: this call must never share the R-8 payer-ingress skip.
func TestPinnedFindingContext_FederatedQueryIngress(t *testing.T) {
	gw, _ := newPendResumeFixture(t, pendFixtureOpts{
		member: "MBR-D-UC05", birthDate: "1968-03-12", familyName: "Johansson-Demo",
		pendedItem: "operative-diagnostic-report",
		extraRoles: map[string]string{"facility": "metro-spine"},
	})
	gw.cfg.OriginationProfile = "demo"
	spy := &findingSpyValidator{}
	gw.cfg.Validator = spy

	rec := httptest.NewRecorder()
	gw.handleUC05(rec, httptest.NewRequest(http.MethodPost, "/scenario/uc05", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	// The ingress check validates the WHOLE CDex query result (the payer's
	// request Task, extended and returned by the facility per
	// shnsdk.BuildCDexQueryResult) — resourceType "Task", not the inner
	// records Bundle. The demo lane deliberately exercises R-8's skip
	// boundary: this same run's pas-claim/pas-claim-update peer responses ARE
	// skip-eligible (relaysReferencePayerBytes) and so emit no validator call
	// at all, which is exactly why federated-query must never share that
	// skip — a shared skip would leave this call with nothing to check
	// either.
	call := spy.nth(t, 1, "Task")
	assertTag(t, "federated query ingress (the facility's answer, as the requesting provider sees it)", call.fc, wantTag{
		legType: "federated-query", whose: "peer", seam: "originate", corrPresent: true,
	})
}
