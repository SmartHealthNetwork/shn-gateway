// conformance_findingvalues_test.go pins source attribution on real handler
// paths. Optional checks run after the handler returns; finding tests wait for
// the bounded completion barrier before inspecting their metadata.
// The synthetic validator records the resource and immutable context seen by
// each actual check; the observer captures findings for checks with no
// independently declared validator target.
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
	"sync"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func TestRetainedDTRCorrelation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply *ApplicationReplyView
		want  string
		ok    bool
	}{
		{"absent", nil, "", false},
		{"prior CRD leg", &ApplicationReplyView{Leg: "crd-order-dispatch", CorrelationID: "crd-correlation"}, "", false},
		{"empty DTR correlation", &ApplicationReplyView{Leg: "dtr-questionnaire-fetch"}, "", false},
		{"received DTR leg", &ApplicationReplyView{Leg: "dtr-questionnaire-fetch", CorrelationID: "dtr-correlation"}, "dtr-correlation", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := retainedDTRCorrelation(ConsumptionAttempt{ApplicationReply: tc.reply})
			if got != tc.want || ok != tc.ok {
				t.Fatalf("retained DTR correlation = %q/%v, want %q/%v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// PCV-02/05/11: the optional worker must retain the authenticated leg's
// metadata for both directions after the handler has returned. The fixture is
// synthetic and checks only the gateway's metadata plumbing.
func TestObservationFindingMetadataNativeInbound(t *testing.T) {
	g, requester := newInboundTestGatewayWithPolicy(t, true, EnforcementObserve)
	g.cfg.Validator = syntheticFakeValidator()
	answer := realCRDAnswer(t)
	g.cfg.Responder = pasResultResponder{result: LegResult{Response: testResponse(answer)}}
	var mu sync.Mutex
	var findings []ConformanceFinding
	g.cfg.Observer = func(e ObserverEvent) {
		if e.Kind != ConformanceObservedEvent {
			return
		}
		var f ConformanceFinding
		if json.Unmarshal([]byte(e.Detail), &f) != nil {
			return
		}
		mu.Lock()
		findings = append(findings, f)
		mu.Unlock()
	}
	pci, _, _ := g.cfg.SoR.ResolvePatient("MBR-COVERED")
	const correlation = "corr-observation-native-crd"
	req := conformantCRD("MBR-COVERED", "72148")
	ex := ExchangeContext{holder: requester.ID, recipient: g.cfg.HolderID,
		legType: "crd-order-select", subjectPCI: pci, correlationID: correlation,
		contractVersion: "pa.crd@2.0", policy: g.policy()}
	env := shnsdk.Envelope{}
	env.Metadata.CorrelationID, env.Metadata.Sender = correlation, requester.ID
	r := newSignedInboundRequest(t, g, requester.ID)
	r = r.WithContext(context.WithValue(r.Context(), nativeExchangeKey{}, ex))
	r = r.WithContext(withFindingContext(r.Context(), findingContext{
		LegType: ex.legType, CorrelationID: correlation,
		Seam: inboundSeamFor(ex.legType), Whose: "peer",
	}))
	rec := httptest.NewRecorder()
	g.handleCRDNativeInbound(rec, r, env, shnsdk.Token{Subject: pci}, req, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("native answer status %d: %s", rec.Code, rec.Body.String())
	}
	hdr, gotBody, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, rec.Body.Bytes()))
	if err != nil || hdr.Status != http.StatusOK || !bytes.Equal(gotBody, answer) {
		t.Fatalf("native answer changed: %v %d %s", err, hdr.Status, gotBody)
	}
	observationFlush(t, g)
	mu.Lock()
	defer mu.Unlock()
	for _, row := range []struct {
		direction, whose, digest string
		state                    CheckState
	}{
		{"request", "peer", sha256hex(req), CheckValid},
		// The native responder did not declare a response line. An unavailable
		// profile finding must not become an invented passing certificate.
		{"response", "own", sha256hex(answer), CheckUnavailable},
	} {
		found := false
		for _, f := range findings {
			if f.Direction != row.direction || f.Rule != "fhir.profile" {
				continue
			}
			found = true
			if f.Gateway != g.cfg.HolderID || f.LegType != ex.legType || f.CorrelationID != correlation ||
				f.Seam != "payer-native" || f.Whose != row.whose || f.Level != "observe" ||
				f.Action != "not_enforced" || f.State != row.state || f.PayloadSHA256 != row.digest {
				t.Errorf("%s finding metadata: %+v", row.direction, f)
			}
		}
		if !found {
			t.Errorf("missing %s fhir.profile finding: %+v", row.direction, findings)
		}
	}
}

func TestObservationFindingMetadataNativeInbound_None(t *testing.T) {
	g, requester := newInboundTestGatewayWithPolicy(t, true, EnforcementNone)
	g.cfg.Validator = failIfCalledValidator{t: t}
	if g.certification != nil {
		t.Fatal("none started an optional certification worker")
	}
	answer := realCRDAnswer(t)
	g.cfg.Responder = pasResultResponder{result: LegResult{Response: testResponse(answer)}}
	var mu sync.Mutex
	findings := 0
	g.cfg.Observer = func(e ObserverEvent) {
		if e.Kind == ConformanceObservedEvent {
			mu.Lock()
			findings++
			mu.Unlock()
		}
	}
	pci, _, _ := g.cfg.SoR.ResolvePatient("MBR-COVERED")
	const correlation = "corr-none-native-crd"
	ex := ExchangeContext{holder: requester.ID, recipient: g.cfg.HolderID,
		legType: "crd-order-select", subjectPCI: pci, correlationID: correlation,
		contractVersion: "pa.crd@2.0", policy: g.policy()}
	env := shnsdk.Envelope{}
	env.Metadata.CorrelationID, env.Metadata.Sender = correlation, requester.ID
	r := newSignedInboundRequest(t, g, requester.ID)
	r = r.WithContext(context.WithValue(r.Context(), nativeExchangeKey{}, ex))
	rec := httptest.NewRecorder()
	g.handleCRDNativeInbound(rec, r, env, shnsdk.Token{Subject: pci}, conformantCRD("MBR-COVERED", "72148"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("native answer status %d: %s", rec.Code, rec.Body.String())
	}
	hdr, gotBody, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, rec.Body.Bytes()))
	if err != nil || hdr.Status != http.StatusOK || !bytes.Equal(gotBody, answer) {
		t.Fatalf("none changed answer: %v %d %s", err, hdr.Status, gotBody)
	}
	observationFlush(t, g)
	mu.Lock()
	defer mu.Unlock()
	if findings != 0 {
		t.Fatalf("none emitted %d optional conformance findings", findings)
	}
}

// findingSpyCall is one $validate call findingSpyValidator observed.
type findingSpyCall struct {
	resourceType string
	fc           findingContext
}

// findingSpyValidator always returns Valid (see file doc) and records every
// call's resourceType + the findingContext actually attached to ctx.
type findingSpyValidator struct {
	mu       sync.Mutex
	calls    []findingSpyCall
	findings []ConformanceFinding
	flush    func()
}

func (s *findingSpyValidator) observe(e ObserverEvent) {
	if e.Kind != ConformanceObservedEvent {
		return
	}
	var f ConformanceFinding
	if json.Unmarshal([]byte(e.Detail), &f) != nil {
		return
	}
	s.mu.Lock()
	s.findings = append(s.findings, f)
	s.mu.Unlock()
}

func (s *findingSpyValidator) finding(t *testing.T, leg, direction, rule string) ConformanceFinding {
	t.Helper()
	if s.flush != nil {
		s.flush()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.findings {
		if f.LegType == leg && f.Direction == direction && f.Rule == rule {
			return f
		}
	}
	t.Fatalf("no %s/%s/%s finding (got %+v)", leg, direction, rule, s.findings)
	return ConformanceFinding{}
}

func findingTag(f ConformanceFinding) findingContext {
	return findingContext{LegType: f.LegType, CorrelationID: f.CorrelationID, Seam: f.Seam, Whose: f.Whose}
}

func (s *findingSpyValidator) Validate(ctx context.Context, resourceJSON []byte, _ string) (shnsdk.Result, error) {
	var probe struct {
		ResourceType string `json:"resourceType"`
	}
	_ = json.Unmarshal(resourceJSON, &probe)
	s.mu.Lock()
	s.calls = append(s.calls, findingSpyCall{resourceType: probe.ResourceType, fc: findingContextFrom(ctx)})
	s.mu.Unlock()
	return shnsdk.Result{Valid: true}, nil
}

// nth returns the (1-indexed) nth recorded call whose resourceType equals
// want, or fails the test naming every call observed (so a broken fixture
// fails loudly, not with a silent index-out-of-range).
func (s *findingSpyValidator) nth(t *testing.T, n int, resourceType string) findingSpyCall {
	t.Helper()
	if s.flush != nil {
		s.flush()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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

// wantTag pins source attribution and correlation presence on the actual leg.
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
	g.cfg.ConformanceEnforcement = EnforcementObserve
	g.startCertification()
	spy.flush = func() { observationFlush(t, g) }
	g.cfg.Observer = spy.observe
	// A non-2xx stop: the ingress checks under test run BEFORE the Responder is
	// ever reached, so a cheap refusal here just ends the request cleanly.
	g.cfg.Responder = pasResultResponder{result: LegResult{Status: http.StatusBadRequest, Message: "test stop"}}
	pci, _, _ := g.cfg.SoR.ResolvePatient("MBR-COVERED")

	env := shnsdk.Envelope{}
	env.Metadata.CorrelationID, env.Metadata.Sender = "corr-crd-ingress-1", requester.ID
	req := conformantCRD("MBR-COVERED", "72148")

	r := newSignedInboundRequest(t, g, requester.ID)
	r = r.WithContext(context.WithValue(r.Context(), nativeExchangeKey{}, ExchangeContext{
		holder: requester.ID, recipient: g.cfg.HolderID, legType: "crd-order-select",
		subjectPCI: pci, correlationID: env.Metadata.CorrelationID,
		contractVersion: "pa.crd@2.0", policy: g.policy(),
	}))
	r = r.WithContext(withFindingContext(r.Context(), findingContext{
		LegType: "crd-order-select", CorrelationID: env.Metadata.CorrelationID,
		Seam: inboundSeamFor("crd-order-select"), Whose: "peer",
	}))
	rec := httptest.NewRecorder()
	g.handleCRDNativeInbound(rec, r, env, shnsdk.Token{Subject: pci}, req, "")

	f := spy.finding(t, "crd-order-select", "request", "fhir.profile")
	assertTag(t, "CRD ingress (peer's incoming order)", findingTag(f), wantTag{
		legType: "crd-order-select", whose: "peer", seam: "payer-native", corrPresent: true,
	})
}

// TestPinnedFindingContext_CRDEgress drives handleUC02 (originateNoPACRD) —
// this participant's own CRD order egress-validated before it is sent.
func TestPinnedFindingContext_CRDEgress(t *testing.T) {
	env := newInProcessExchange(t)
	spy := &findingSpyValidator{}
	env.originator.cfg.Validator = spy
	env.originator.cfg.ConformanceEnforcement = EnforcementObserve
	env.originator.startCertification()
	spy.flush = func() { observationFlush(t, env.originator) }
	env.originator.cfg.Observer = spy.observe
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
	f := spy.finding(t, "crd-order-select", "request", "fhir.profile")
	assertTag(t, "CRD egress (this participant's own order)", findingTag(f), wantTag{
		legType: "crd-order-select", whose: "own", seam: "originate", corrPresent: true,
	})
}

// TestPinnedFindingContext_UC01EligibilityEgress pins the provider's own
// built eligibility request after the leg acquires its correlation ID.
func TestPinnedFindingContext_UC01EligibilityEgress(t *testing.T) {
	env := newInProcessExchange(t)
	spy := &findingSpyValidator{}
	env.originator.cfg.Validator = spy
	env.originator.cfg.ConformanceEnforcement = EnforcementObserve
	env.originator.startCertification()
	spy.flush = func() { observationFlush(t, env.originator) }
	env.originator.cfg.Observer = spy.observe
	correlationCalls := 0
	env.originator.cfg.CorrelationGen = func() string {
		correlationCalls++
		return "uc01-eligibility-leg"
	}
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
	if correlationCalls != 1 || call.fc.CorrelationID != "uc01-eligibility-leg" {
		t.Fatalf("eligibility correlation generated %d times; authored check=%q", correlationCalls, call.fc.CorrelationID)
	}
	env.substrate.mu.Lock()
	routedCorrelation := env.substrate.lastMetadata.CorrelationID
	env.substrate.mu.Unlock()
	if routedCorrelation != call.fc.CorrelationID {
		t.Fatalf("authored check correlation=%q, routed leg=%q", call.fc.CorrelationID, routedCorrelation)
	}
	assertTag(t, "UC-01 eligibility egress (this participant's own request)", call.fc, wantTag{
		legType: "coverage-eligibility", whose: "own", seam: "originate", corrPresent: true,
	})
}

// ---- Inbound family (the answer half of handleInbound's tag) ----

// TestPinnedFindingContext_InboundEgress drives handlePASNativeInbound
// directly, pre-tagged exactly as handleInbound sets it for an inbound
// pas-claim leg (Whose "peer", the REQUEST's own direction) — proving the
// direction-specific source metadata retained by the observation worker.
func TestPinnedFindingContext_InboundEgress(t *testing.T) {
	g, requester := newInboundTestGateway(t, true)
	spy := &findingSpyValidator{}
	g.cfg.Validator = spy
	g.cfg.ConformanceEnforcement = EnforcementObserve
	g.startCertification()
	spy.flush = func() { observationFlush(t, g) }
	g.cfg.Observer = spy.observe
	// A conformant terminal PAS response Bundle, sealed as an answer this
	// responder AUTHORED (testResponse — ResponseRelayed() false), which is
	// The answer is authored by this participant. Its clinical closure must not
	// commit as a prerequisite for native delivery; rollback releases it.
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

	if commits != 0 || rollbacks != 1 || rec.Code != http.StatusOK {
		t.Fatalf("commits=%d rollbacks=%d status=%d body=%s — fixture is not reaching the egress checks cleanly",
			commits, rollbacks, rec.Code, rec.Body.String())
	}
	call := spy.nth(t, 1, "Bundle")
	assertTag(t, "inbound egress (this payer's own authored response, after the admit direction-flip)", call.fc, wantTag{
		legType: "pas-claim", whose: "own", seam: "payer-native", corrPresent: true,
	})
}

// TestPinnedFindingContext_PayerDTREgress drives handleDTRInbound
// (payer.go) directly, with the inbound DTR leg's request context. The
// validator sees the payer's own authored questionnaire-package answer.
func TestPinnedFindingContext_PayerDTREgress(t *testing.T) {
	g, requester := newInboundTestGateway(t, true)
	spy := &findingSpyValidator{}
	g.cfg.Validator = spy
	g.cfg.ConformanceEnforcement = EnforcementObserve
	g.startCertification()
	spy.flush = func() { observationFlush(t, g) }
	g.cfg.Observer = spy.observe
	// An authored package keeps this participant's ownership on the answer.
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
	f := spy.finding(t, "dtr-questionnaire-fetch", "response", "fhir.profile")
	def, _ := shnsdk.DTRLineDef("2.0")
	if f.CheckClass != CheckDeep || f.Operation != shnsdk.FrameOperationQuestionnairePackage || f.Profile != certificationDTR+"DTR-QPackageBundle|"+def.PackageVersion || f.State != CheckValid {
		t.Fatalf("DTR profile classification: %+v", f)
	}
}

// TestPinnedFindingContext_InboundUpdateEgress drives
// handlePASUpdateNativeInbound directly, pre-tagged exactly as handleInbound
// sets it for an inbound pas-claim-update leg. It pins the authored response's
// ownership and the inherited correlation ID at the observer worker.
func TestPinnedFindingContext_InboundUpdateEgress(t *testing.T) {
	g, requester := newInboundTestGateway(t, true)
	spy := &findingSpyValidator{}
	g.cfg.Validator = spy
	g.cfg.ConformanceEnforcement = EnforcementObserve
	g.startCertification()
	spy.flush = func() { observationFlush(t, g) }
	g.cfg.Observer = spy.observe
	// A conformant terminal PAS response Bundle, sealed as AUTHORED
	// (testResponse — ResponseRelayed() false). ResponseSubjectForeign stands
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
	g.cfg.ConformanceEnforcement = EnforcementObserve
	g.startCertification()
	spy.flush = func() { observationFlush(t, g) }
	g.cfg.Observer = spy.observe
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
		ConformanceEnforcement: EnforcementStrict,
		Role:                   "payer",
		HolderID:               "payer",
		Identity:               shnsdk.Identity{HolderID: "payer", SignPriv: paySignPriv, EncPub: payEncPub, EncPriv: payEncPriv},
		AuthzURL:               "http://stub.test",
		AuthzPub:               authzPub,
		HubTransportPub:        authzPub,
		Reg:                    shnsdk.NewRegistry(),
		Validator:              spy,
		SoR:                    sor,
		Store:                  sor,
		Responder:              unusedResponder{},
		Clock:                  fixedClock,
		Client:                 audit.Client(),
		AuditURL:               audit.URL,
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
	gw.cfg.ConformanceEnforcement = EnforcementObserve
	gw.startCertification()
	spy.flush = func() { observationFlush(t, gw) }
	gw.cfg.Observer = spy.observe

	rec := httptest.NewRecorder()
	gw.handleUC04(rec, httptest.NewRequest(http.MethodPost, "/scenario/uc04", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	// The worker reports the whole carried message for each leg. A missing
	// producer declaration may yield unavailable, never a fabricated pass.
	dtrIngress := spy.finding(t, "dtr-questionnaire-fetch", "response", "fhir.profile")
	assertTag(t, "DTR ingress (payer's questionnaire package)", findingTag(dtrIngress), wantTag{
		legType: "dtr-questionnaire-fetch", whose: "peer", seam: "originate", corrPresent: true,
	})
	dtrEgress := spy.finding(t, "dtr-questionnaire-fetch", "request", "fhir.profile")
	assertTag(t, "DTR egress (this participant's own populated QR)", findingTag(dtrEgress), wantTag{
		legType: "dtr-questionnaire-fetch", whose: "own", seam: "originate", corrPresent: true,
	})
	pasSubmitEgress := spy.finding(t, "pas-claim", "request", "fhir.profile")
	pasDef, _ := shnsdk.PASLineDef("2.0")
	if pasSubmitEgress.CheckClass != CheckDeep || pasSubmitEgress.Profile != certificationPAS+"profile-pas-request-bundle|"+pasDef.PackageVersion || pasSubmitEgress.Operation != "" {
		t.Fatalf("PAS profile classification: %+v", pasSubmitEgress)
	}
	assertTag(t, "PAS egress (this participant's own submit bundle)", findingTag(pasSubmitEgress), wantTag{
		legType: "pas-claim", whose: "own", seam: "originate", corrPresent: true,
	})
	pasSubmitIngress := spy.finding(t, "pas-claim", "response", "fhir.profile")
	assertTag(t, "PAS ingress (payer's pended response)", findingTag(pasSubmitIngress), wantTag{
		legType: "pas-claim", whose: "peer", seam: "originate", corrPresent: true,
	})
	pasUpdateEgress := spy.finding(t, "pas-claim-update", "request", "fhir.profile")
	assertTag(t, "PAS egress (this participant's own amendment bundle)", findingTag(pasUpdateEgress), wantTag{
		legType: "pas-claim-update", whose: "own", seam: "originate", corrPresent: true,
	})
	pasUpdateIngress := spy.finding(t, "pas-claim-update", "response", "fhir.profile")
	assertTag(t, "PAS ingress (payer's approved response, amendment)", findingTag(pasUpdateIngress), wantTag{
		legType: "pas-claim-update", whose: "peer", seam: "originate", corrPresent: true,
	})
}

// TestPinnedFindingContext_DTRAdaptiveIngress drives the actual $next-question
// round through OriginateLegMessage. Its undeclared response still produces an
// unavailable profile observation with the peer's source metadata; it is not
// represented as a passing validation of the Parameters wrapper.
func TestPinnedFindingContext_DTRAdaptiveIngress(t *testing.T) {
	env := newInProcessExchange(t)
	spy := &findingSpyValidator{}
	env.originator.cfg.Validator = spy
	env.originator.cfg.ConformanceEnforcement = EnforcementObserve
	env.originator.startCertification()
	spy.flush = func() { observationFlush(t, env.originator) }
	env.originator.cfg.Observer = spy.observe

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

	_, status, msg, lerr := env.originator.nextQuestionLeg(env.ctx, env.req, &res, canonical, reqQR)
	if status != 0 {
		t.Fatalf("nextQuestionLeg refused: %d %s (%v)", status, msg, lerr)
	}

	f := spy.finding(t, "dtr-questionnaire-fetch", "response", "fhir.profile")
	if f.State != CheckUnavailable || f.Operation != shnsdk.FrameOperationNextQuestion || f.Profile != "" || len(f.Profiles) != 0 {
		t.Errorf("undeclared adaptive answer was certified: %+v", f)
	}
	assertTag(t, "DTR ingress, adaptive $next-question round (payer's answer)", findingTag(f), wantTag{
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
		ConformanceEnforcement: EnforcementStrict,
		Role:                   "payer",
		HolderID:               "payer",
		Identity:               shnsdk.Identity{HolderID: "payer", SignPriv: paySignPriv, EncPub: payEncPub, EncPriv: payEncPriv},
		AuthzURL:               "http://stub.test",
		AuthzPub:               authzPub,
		HubTransportPub:        authzPub,
		ConsentURL:             "http://stub.test/consent",
		Reg:                    reg,
		Validator:              spy,
		SoR:                    &fqFacilitySoR{censusSoR: newCensusSoR()},
		Store:                  newCensusSoR(),
		Responder:              unusedResponder{},
		Clock:                  fixedClock,
		Client:                 &http.Client{Transport: &consentGrantStub{authz: inboundAuthzStub{authzPriv: authzPriv, clock: fixedClock}, consentRef: consentRef}},
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
// through handleUC05's real federated-query loop to pin the facility answer's
// source metadata independently from the preceding PAS legs.
func TestPinnedFindingContext_FederatedQueryIngress(t *testing.T) {
	gw, _ := newPendResumeFixture(t, pendFixtureOpts{
		member: "MBR-D-UC05", birthDate: "1968-03-12", familyName: "Johansson-Demo",
		pendedItem: "operative-diagnostic-report",
		extraRoles: map[string]string{"facility": "metro-spine"},
	})
	gw.cfg.OriginationProfile = "demo"
	spy := &findingSpyValidator{}
	gw.cfg.Validator = spy
	gw.cfg.ConformanceEnforcement = EnforcementObserve
	gw.startCertification()
	spy.flush = func() { observationFlush(t, gw) }
	gw.cfg.Observer = spy.observe

	rec := httptest.NewRecorder()
	gw.handleUC05(rec, httptest.NewRequest(http.MethodPost, "/scenario/uc05", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	// The finding belongs to the facility's returned CDex query result, not
	// to a provider-authored Task or a PAS response earlier in this run.
	f := spy.finding(t, "federated-query", "response", "fhir.profile")
	assertTag(t, "federated query ingress (the facility's answer, as the requesting provider sees it)", findingTag(f), wantTag{
		legType: "federated-query", whose: "peer", seam: "originate", corrPresent: true,
	})
}
