// observer_test.go — hermetic tests for the observer seam (see STABILITY.md).
// The seam is ADDITIVE instrumentation: nil Observer = zero emission (the
// default, and the published-gateway posture); a configured Observer receives
// structured events with edge payload snapshots. Neutrality (on/off responses
// byte-identical) is asserted in TestObserver_ConformanceNeutral.
package engine

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// TestObserve_NilObserverIsNoop: a gateway with no Observer must not panic and
// must not emit — the flag-off half of the rejection row.
func TestObserve_NilObserverIsNoop(t *testing.T) {
	clock := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	_, provSignPriv := genED25519(t)
	provEncPub, provEncPriv := genKeyPair(t)

	sor := NewStubHolderData()

	gw := New(Config{
		Role:        "provider",
		HolderID:    "provider",
		PayerRouter: payerRouterFor(t, "payer"),
		Identity: shnsdk.Identity{
			HolderID: "provider",
			SignPriv: provSignPriv,
			EncPub:   provEncPub,
			EncPriv:  provEncPriv,
		},
		Validator: shnsdk.NewFakeValidator(),
		SoR:       sor,
		Store:     sor,
		Clock:     clock,
	})
	// Must not panic.
	gw.observe(ObserverEvent{Kind: "leg.originated"})
}

// TestObserve_StampsClockTime: events carry the gateway clock's time, not the
// caller's — determinism for the inspector timeline.
func TestObserve_StampsClockTime(t *testing.T) {
	fixed := time.Unix(1700000000, 0).UTC()
	clock := func() time.Time { return fixed }
	_, provSignPriv := genED25519(t)
	provEncPub, provEncPriv := genKeyPair(t)

	sor := NewStubHolderData()

	var got []ObserverEvent
	gw := New(Config{
		Role:        "provider",
		HolderID:    "provider",
		PayerRouter: payerRouterFor(t, "payer"),
		Identity: shnsdk.Identity{
			HolderID: "provider",
			SignPriv: provSignPriv,
			EncPub:   provEncPub,
			EncPriv:  provEncPriv,
		},
		Validator: shnsdk.NewFakeValidator(),
		SoR:       sor,
		Store:     sor,
		Clock:     clock,
		Observer:  func(e ObserverEvent) { got = append(got, e) },
	})
	gw.observe(ObserverEvent{Kind: "leg.originated", LegType: "crd-order-select"})
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	if !got[0].Time.Equal(fixed) {
		t.Fatalf("event time = %v, want clock time %v", got[0].Time, fixed)
	}
	if got[0].Kind != "leg.originated" || got[0].LegType != "crd-order-select" {
		t.Fatalf("event fields not preserved: %+v", got[0])
	}
}

// TestObserver_OriginationLegEvents drives UC-03 through the hermetic
// stubSubstrate harness (originate_test.go) with a capturing Observer and
// asserts the origination emissions: the CRD leg emits originated+response;
// the DTR leg (which the stub errors) emits originated+failed. This covers
// every origination leg in the engine — roundTrip is the single choke point.
func TestObserver_OriginationLegEvents(t *testing.T) {
	gw, _, _ := crdTestSystem(t, shnsdk.CardCoverage{Covered: shnsdk.CoveredCovered, PANeeded: shnsdk.PANeededAuthNeeded, Questionnaires: []string{"http://example.org/q"}})
	var events []ObserverEvent
	gw.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }

	callUC03(t, gw) // HTTP outcome is irrelevant here; the stub errors leg 1 by design

	var kinds []string
	for _, e := range events {
		if e.Direction == "originate" {
			kinds = append(kinds, e.Kind+"/"+e.LegType)
		}
	}
	want := []string{
		"leg.originated/crd-order-select",
		"leg.response/crd-order-select",
		"leg.originated/dtr-questionnaire-fetch",
		"leg.failed/dtr-questionnaire-fetch",
	}
	if len(kinds) < len(want) {
		t.Fatalf("origination events = %v, want at least %v", kinds, want)
	}
	for i, w := range want {
		if kinds[i] != w {
			t.Fatalf("event %d = %q, want %q (all: %v)", i, kinds[i], w, kinds)
		}
	}

	// The CRD pair carries the correlation id, counterpart, and payloads.
	var orig, resp *ObserverEvent
	for i := range events {
		e := &events[i]
		if e.LegType == "crd-order-select" && e.Kind == "leg.originated" {
			orig = e
		}
		if e.LegType == "crd-order-select" && e.Kind == "leg.response" {
			resp = e
		}
	}
	if orig == nil || resp == nil {
		t.Fatal("missing CRD originated/response events")
	}
	if orig.CorrelationID == "" || orig.CorrelationID != resp.CorrelationID {
		t.Fatalf("correlation ids: originated=%q response=%q", orig.CorrelationID, resp.CorrelationID)
	}
	if orig.Counterpart != "payer" {
		t.Fatalf("counterpart = %q, want payer", orig.Counterpart)
	}
	if len(orig.Payload) == 0 || len(resp.Payload) == 0 {
		t.Fatalf("payload snapshots missing: originated=%dB response=%dB", len(orig.Payload), len(resp.Payload))
	}
	if orig.AuthorityFrame != "provider-tpo" || resp.AuthorityFrame != "payer-coverage" {
		t.Fatalf("frames: originated=%q response=%q", orig.AuthorityFrame, resp.AuthorityFrame)
	}
}

// TestObserver_ValidateEvents: every Validator.Validate call the engine makes
// emits validate.result — decorating the validator (not each call site) means
// no call site can be missed.
func TestObserver_ValidateEvents(t *testing.T) {
	gw, _, _ := crdTestSystem(t, shnsdk.CardCoverage{Covered: shnsdk.CoveredCovered, PANeeded: shnsdk.PANeededAuthNeeded, Questionnaires: []string{"http://example.org/q"}})
	var validates []ObserverEvent
	gw.cfg.Observer = func(e ObserverEvent) {
		if e.Kind == "validate.result" {
			validates = append(validates, e)
		}
	}
	// Re-decorate: crdTestSystem constructed via New with Observer nil, so the
	// validator wasn't wrapped. Rebuilding through New with the observer set is
	// what production does; mirror it.
	cfg := gw.cfg
	gw2 := New(cfg)

	callUC03(t, gw2)

	if len(validates) == 0 {
		t.Fatal("no validate.result events observed for a UC-03 run")
	}
	for _, e := range validates {
		if e.Detail != "valid" && e.Detail != "invalid" && e.Detail != "validator unavailable" {
			t.Fatalf("unexpected validate detail %q", e.Detail)
		}
		if e.Direction != "validate" {
			t.Fatalf("validate event direction = %q, want validate", e.Direction)
		}
	}
}

// TestObserver_IngressEvents: a Da Vinci ingress call emits received+responded
// with request/response payload snapshots — the conformant lane's origin
// events in the Kit inspector. The scenario outcome is NOT asserted; the
// middleware fires around the handler whatever the handler decides.
func TestObserver_IngressEvents(t *testing.T) {
	gw, _, _ := crdTestSystem(t, shnsdk.CardCoverage{Covered: shnsdk.CoveredCovered, PANeeded: shnsdk.PANeededAuthNeeded, Questionnaires: []string{"http://example.org/q"}})
	cfg := gw.cfg
	EnableIngressForTest(&cfg) // bypassed auth ⇒ IngressBaseURL/IngressClients not required
	var events []ObserverEvent
	cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
	gw2 := New(cfg)

	ref := "Patient/MBR-COVERED"
	req := httptest.NewRequest(http.MethodPost, "/cds-services/order-select-crd",
		bytes.NewReader(crdReqJSON("MBR-COVERED", ref, ref)))
	rec := httptest.NewRecorder()
	gw2.Handler().ServeHTTP(rec, req)

	var recv, resp *ObserverEvent
	for i := range events {
		switch events[i].Kind {
		case "ingress.received":
			recv = &events[i]
		case "ingress.responded":
			resp = &events[i]
		}
	}
	if recv == nil || resp == nil {
		t.Fatalf("want ingress.received + ingress.responded, got %+v", events)
	}
	if recv.LegType != "crd-ingress" || resp.LegType != "crd-ingress" {
		t.Fatalf("route tags: recv=%q resp=%q, want crd-ingress", recv.LegType, resp.LegType)
	}
	if len(recv.Payload) == 0 || len(resp.Payload) == 0 {
		t.Fatalf("payload snapshots missing: recv=%dB resp=%dB", len(recv.Payload), len(resp.Payload))
	}
	if resp.Detail == "" {
		t.Fatal("ingress.responded must carry the HTTP status in Detail")
	}
}

// TestObserver_ConformanceNeutral: the same UC-03 drive with observer OFF and
// ON must produce byte-identical HTTP responses — the spec's "additive
// instrumentation, no behavior change" promise as a permanent gate. Both
// gateways get a pinned CorrelationGen and clock so the comparison is
// deterministic.
func TestObserver_ConformanceNeutral(t *testing.T) {
	run := func(withObserver bool) (int, string) {
		gw, _, _ := crdTestSystem(t, shnsdk.CardCoverage{Covered: shnsdk.CoveredCovered, PANeeded: shnsdk.PANeededAuthNeeded, Questionnaires: []string{"http://example.org/q"}})
		gw.cfg.CorrelationGen = func() string { return "corr-fixed-0001" }
		if withObserver {
			gw.cfg.Observer = func(ObserverEvent) {}
			gw = New(gw.cfg) // re-run New so the validator decoration path is active
			// Re-pin AFTER New — New re-applies defaults (CorrelationGen would
			// revert to crypto-random). Order-sensitive; do not "simplify".
			gw.cfg.CorrelationGen = func() string { return "corr-fixed-0001" }
		}
		rec := callUC03(t, gw)
		return rec.Code, rec.Body.String()
	}
	offCode, offBody := run(false)
	onCode, onBody := run(true)
	if offCode != onCode || offBody != onBody {
		t.Fatalf("observer changed behavior:\noff: %d %s\non:  %d %s", offCode, offBody, onCode, onBody)
	}
}

// TestObserver_IngressConformanceNeutral: the same CRD ingress call with
// observer OFF and ON must produce byte-identical HTTP status + body. Task
// 4's review flagged the ingress tee (recordingWriter) as the highest-risk
// code for the on/off byte-identity constraint — TestObserver_ConformanceNeutral
// only drives the origination path (roundTrip + validator decorator), so this
// test gives the ingress middleware its own gate. Each run builds its own
// crdTestSystem so stubSubstrate.legCount cannot leak between the off/on
// drives. CorrelationGen is intentionally left unpinned (crypto-random): the
// ingress response body (a relayed cards envelope) does not embed the
// correlation id, so an unpinned generator does not make the comparison flaky.
func TestObserver_IngressConformanceNeutral(t *testing.T) {
	ref := "Patient/MBR-COVERED"
	run := func(withObserver bool) (int, string) {
		gw, _, _ := crdTestSystem(t, shnsdk.CardCoverage{Covered: shnsdk.CoveredCovered, PANeeded: shnsdk.PANeededAuthNeeded, Questionnaires: []string{"http://example.org/q"}})
		cfg := gw.cfg
		EnableIngressForTest(&cfg) // bypassed auth ⇒ IngressBaseURL/IngressClients not required
		if withObserver {
			cfg.Observer = func(ObserverEvent) {}
		}
		gw2 := New(cfg)

		req := httptest.NewRequest(http.MethodPost, "/cds-services/order-select-crd",
			bytes.NewReader(crdReqJSON("MBR-COVERED", ref, ref)))
		rec := httptest.NewRecorder()
		gw2.Handler().ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	offCode, offBody := run(false)
	onCode, onBody := run(true)
	if offCode != onCode || offBody != onBody {
		t.Fatalf("observer changed ingress behavior:\noff: %d %s\non:  %d %s", offCode, offBody, onCode, onBody)
	}
}

// TestRecordingWriter_Unwrap: the ingress tee must expose the underlying
// ResponseWriter so http.ResponseController verbs (Flush, deadlines) pass
// through on the observed path.
func TestRecordingWriter_Unwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &recordingWriter{ResponseWriter: rec, status: http.StatusOK}
	if err := http.NewResponseController(rw).Flush(); err != nil {
		t.Fatalf("ResponseController.Flush through the tee: %v", err)
	}
	if !rec.Flushed {
		t.Fatal("flush did not reach the underlying writer")
	}
}

// TestObservingSoR_EmitsPerMethod: every SystemOfRecord method emits exactly
// one sor.read event with Op = the method name, and results pass through
// untouched (compared against a bare stub).
func TestObservingSoR_EmitsPerMethod(t *testing.T) {
	fixed := time.Unix(1700000000, 0).UTC()
	var events []ObserverEvent
	bare := NewStubHolderData()
	sor := observingSoR{
		inner:    NewStubHolderData(),
		observer: func(e ObserverEvent) { events = append(events, e) },
		clock:    func() time.Time { return fixed },
	}

	const member = "MBR-COVERED" // stub persona, present in every read seam it supports

	pci, demo, found := sor.ResolvePatient(member)
	wantPCI, wantDemo, wantFound := bare.ResolvePatient(member)
	if pci != wantPCI || demo != wantDemo || found != wantFound {
		t.Fatalf("ResolvePatient passthrough: got (%q,%+v,%v) want (%q,%+v,%v)", pci, demo, found, wantPCI, wantDemo, wantFound)
	}
	sor.PatientFHIRRef(member)
	sor.CoverageInforce(member)
	sor.ClinicalContext(member)
	sor.SupplementalReport(member)
	sor.FacilityRecords(member)
	orderJSON, orderFound := sor.OpenOrder(member)
	wantOrder, wantOrderFound := bare.OpenOrder(member)
	if !bytes.Equal(orderJSON, wantOrder) || orderFound != wantOrderFound {
		t.Fatal("OpenOrder passthrough mismatch")
	}
	sor.OpenCoverage(member)
	sor.ResolveByReference("Organization/dme-1")

	wantOps := []string{
		"ResolvePatient", "PatientFHIRRef", "CoverageInforce", "ClinicalContext",
		"SupplementalReport", "FacilityRecords", "OpenOrder", "OpenCoverage", "ResolveByReference",
	}
	if len(events) != len(wantOps) {
		t.Fatalf("got %d events, want %d: %+v", len(events), len(wantOps), events)
	}
	for i, e := range events {
		if e.Kind != "sor.read" || e.Direction != "sor" {
			t.Fatalf("event %d: kind=%q direction=%q, want sor.read/sor", i, e.Kind, e.Direction)
		}
		if e.Op != wantOps[i] {
			t.Fatalf("event %d op = %q, want %q", i, e.Op, wantOps[i])
		}
		if !e.Time.Equal(fixed) {
			t.Fatalf("event %d time = %v, want clock time", i, e.Time)
		}
		if e.Detail == "" {
			t.Fatalf("event %d (%s) has empty Detail", i, e.Op)
		}
	}
	// Payload present exactly for the byte-returning methods that found data.
	for i, e := range events {
		byteReturning := e.Op == "OpenOrder" || e.Op == "OpenCoverage" ||
			e.Op == "ResolveByReference" || e.Op == "SupplementalReport"
		if byteReturning && e.Detail == "found" && len(e.Payload) == 0 {
			t.Fatalf("event %d (%s): found but no payload snapshot", i, e.Op)
		}
		if !byteReturning && len(e.Payload) != 0 {
			t.Fatalf("event %d (%s): unexpected payload on a non-byte method", i, e.Op)
		}
	}
}

// TestObservingSoR_CoverageInforceDetail: the not-inforce branches of
// CoverageInforce's Detail mapping — a reason from the SoR passes through
// verbatim, and an empty reason (unknown member) falls back to "not inforce"
// (three-way Detail mapping).
func TestObservingSoR_CoverageInforceDetail(t *testing.T) {
	var events []ObserverEvent
	sor := observingSoR{
		inner:    NewStubHolderData(),
		observer: func(e ObserverEvent) { events = append(events, e) },
		clock:    time.Now,
	}
	sor.CoverageInforce("MBR-NOTCOVERED")
	sor.CoverageInforce("MBR-NO-SUCH-MEMBER")
	if len(events) != 2 {
		t.Fatalf("want 2 events, got %d", len(events))
	}
	if events[0].Op != "CoverageInforce" || events[0].Detail != "coverage-terminated" {
		t.Fatalf("MBR-NOTCOVERED: op=%q detail=%q, want CoverageInforce/coverage-terminated", events[0].Op, events[0].Detail)
	}
	if len(events[0].Payload) != 0 {
		t.Fatalf("MBR-NOTCOVERED: unexpected payload")
	}
	if events[1].Op != "CoverageInforce" || events[1].Detail != "not inforce" {
		t.Fatalf("MBR-NO-SUCH-MEMBER: op=%q detail=%q, want CoverageInforce/not inforce", events[1].Op, events[1].Detail)
	}
	if len(events[1].Payload) != 0 {
		t.Fatalf("MBR-NO-SUCH-MEMBER: unexpected payload")
	}
}

// TestObserver_SoRDecorationPrecedesDerivations pins the spec's review
// finding: the derived sandbox Responder and managed Populator must hold the
// DECORATED SoR. If the decoration moves below the gateway.go:240/251
// derivations, this fails.
func TestObserver_SoRDecorationPrecedesDerivations(t *testing.T) {
	gw, _, _ := crdTestSystem(t, shnsdk.CardCoverage{Covered: shnsdk.CoveredCovered, PANeeded: shnsdk.PANeededAuthNeeded, Questionnaires: []string{"http://example.org/q"}})
	cfg := gw.cfg
	// The first New() (observer off) already derived Responder/Populator over
	// the RAW SoR — nil them so this New() re-derives, as a fresh production
	// construction with an Observer would.
	cfg.Responder = nil
	cfg.Populator = nil
	cfg.Adjudicator = NewSandboxAdjudicator(cfg.SoR, cfg.Clock)
	cfg.Observer = func(ObserverEvent) {}
	g2 := New(cfg)

	if _, ok := g2.cfg.SoR.(observingSoR); !ok {
		t.Fatalf("cfg.SoR not decorated: %T", g2.cfg.SoR)
	}
	mp, ok := g2.cfg.Populator.(*managedPopulator)
	if !ok {
		t.Fatalf("unexpected populator type %T", g2.cfg.Populator)
	}
	if _, ok := mp.sor.(observingSoR); !ok {
		t.Fatal("managed Populator captured the raw SoR — decoration must precede the gateway.go:251 derivation")
	}
	sr, ok := g2.cfg.Responder.(*sandboxResponder)
	if !ok {
		t.Fatalf("unexpected responder type %T", g2.cfg.Responder)
	}
	if _, ok := sr.sor.(observingSoR); !ok {
		t.Fatal("sandbox Responder captured the raw SoR — decoration must precede the gateway.go:240 derivation")
	}
}

// TestObserver_SoRDecorationIdempotent: an already-decorated SoR is not
// wrapped again (double emission) — insurance for any future app-layer
// pre-decoration.
func TestObserver_SoRDecorationIdempotent(t *testing.T) {
	gw, _, _ := crdTestSystem(t, shnsdk.CardCoverage{Covered: shnsdk.CoveredCovered, PANeeded: shnsdk.PANeededAuthNeeded, Questionnaires: []string{"http://example.org/q"}})
	cfg := gw.cfg
	cfg.Observer = func(ObserverEvent) {}
	g2 := New(cfg) // decorates
	cfg2 := g2.cfg
	g3 := New(cfg2) // must NOT re-wrap
	inner, ok := g3.cfg.SoR.(observingSoR)
	if !ok {
		t.Fatalf("cfg.SoR not decorated: %T", g3.cfg.SoR)
	}
	if _, doubled := inner.inner.(observingSoR); doubled {
		t.Fatal("SoR decorated twice — New() must guard on the decorator type")
	}
	// Same guard-on-decorator-type pattern for the validator: before this
	// change gateway.go:263-264 double-wrapped on a re-New, double-emitting
	// validate.result.
	v, ok := g3.cfg.Validator.(observingValidator)
	if !ok {
		t.Fatalf("cfg.Validator not decorated: %T", g3.cfg.Validator)
	}
	if _, doubled := v.inner.(observingValidator); doubled {
		t.Fatal("validator decorated twice — extend the type guard to the observingValidator install site")
	}
}

// TestObserver_SoREventsInUC03 drives UC-03 hermetically with the observer on
// and asserts the reads the run actually makes emit. callUC03's stub errors
// the DTR leg by design (observer_test.go:95), which short-circuits before
// runCRDThenDTROrder reaches the Populator — so the UC-03 drive itself is
// asserted only for the sor.read ops it genuinely makes (ResolvePatient et
// al., originate.go:352). The non-negotiable half of the capture-site pin —
// a populator-driven read emits through the decorated SoR — is then asserted
// directly: g2.cfg.Populator is in-package, so its Populate entry point is
// invoked with a stub persona + the sandbox $questionnaire-package, and the
// resulting ClinicalContext sor.read is required.
func TestObserver_SoREventsInUC03(t *testing.T) {
	gw, _, _ := crdTestSystem(t, shnsdk.CardCoverage{Covered: shnsdk.CoveredCovered, PANeeded: shnsdk.PANeededAuthNeeded, Questionnaires: []string{"http://example.org/q"}})
	cfg := gw.cfg
	cfg.Responder = nil
	cfg.Populator = nil
	var events []ObserverEvent
	cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
	g2 := New(cfg)

	callUC03(t, g2)

	ops := map[string]bool{}
	for _, e := range events {
		if e.Kind == "sor.read" {
			ops[e.Op] = true
			if e.Direction != "sor" {
				t.Fatalf("sor.read with direction %q", e.Direction)
			}
		}
	}
	if len(ops) == 0 {
		t.Fatal("UC-03 drive emitted no sor.read events")
	}
	if !ops["ResolvePatient"] {
		t.Fatalf("no ResolvePatient sor.read from the UC-03 drive (ops seen: %v)", ops)
	}

	// Non-negotiable half: the managed Populator (holding the decorated SoR —
	// TestObserver_SoRDecorationPrecedesDerivations already pins that capture)
	// emits ClinicalContext when it actually populates.
	mp, ok := g2.cfg.Populator.(*managedPopulator)
	if !ok {
		t.Fatalf("unexpected populator type %T", g2.cfg.Populator)
	}
	const member = "MBR-COVERED"
	_, _, err := mp.Populate(context.Background(), wrapSandboxPackage(t), PopulateContext{
		Member:      member,
		PatientRef:  "Patient/" + member,
		CoverageRef: "Coverage/" + member,
		OrderRef:    "ServiceRequest/sr-" + member,
		Authored:    time.Unix(1700000000, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("managed populate: %v", err)
	}

	ops = map[string]bool{}
	for _, e := range events {
		if e.Kind == "sor.read" {
			ops[e.Op] = true
		}
	}
	if !ops["ClinicalContext"] {
		t.Fatalf("no ClinicalContext sor.read — the managed Populator is reading an undecorated SoR (ops seen: %v)", ops)
	}
}

// TestObservingSoR_NotFoundDetail: a miss emits Detail "not found" (and, for
// CoverageInforce, the not-inforce reason) — a not-found read is a normal
// branch, not an error.
func TestObservingSoR_NotFoundDetail(t *testing.T) {
	var events []ObserverEvent
	sor := observingSoR{
		inner:    NewStubHolderData(),
		observer: func(e ObserverEvent) { events = append(events, e) },
		clock:    time.Now,
	}
	sor.ResolvePatient("MBR-NO-SUCH-MEMBER")
	sor.OpenOrder("MBR-NO-SUCH-MEMBER")
	if len(events) != 2 {
		t.Fatalf("want 2 events, got %d", len(events))
	}
	for _, e := range events {
		if e.Detail != "not found" {
			t.Fatalf("%s Detail = %q, want \"not found\"", e.Op, e.Detail)
		}
		if len(e.Payload) != 0 {
			t.Fatalf("%s: payload on a miss", e.Op)
		}
	}
}
