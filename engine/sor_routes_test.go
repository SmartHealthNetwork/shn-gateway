package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

type routeFailureSoR struct {
	SystemOfRecord
	ContextSystemOfRecord
	t    *testing.T
	fail string
}

func (s routeFailureSoR) ResolvePatient(string) (string, Demo, bool) {
	s.t.Error("legacy patient method selected")
	return "", Demo{}, false
}
func (s routeFailureSoR) OpenCoverage(string) ([]byte, bool) {
	s.t.Error("legacy coverage method selected")
	return nil, false
}
func (s routeFailureSoR) ResolvePatientContext(ctx context.Context, m string) (string, Demo, bool, error) {
	if s.fail == "patient" {
		return "", Demo{}, false, errors.New("private-upstream-sentinel")
	}
	return "pci", Demo{}, true, nil
}
func (s routeFailureSoR) OpenCoverageContext(ctx context.Context, m string) ([]byte, bool, error) {
	return nil, false, &SoRReadError{Kind: SoRUnavailable}
}
func TestRouteSoRFailure(t *testing.T) {
	for _, observed := range []bool{false, true} {
		for _, fail := range []string{"patient", "coverage"} {
			t.Run(fail+map[bool]string{false: "/bare", true: "/observed"}[observed], func(t *testing.T) {
				s := routeFailureSoR{SystemOfRecord: newCensusSoR(), ContextSystemOfRecord: ReadSystemOfRecord(newCensusSoR()), t: t, fail: fail}
				var sor SystemOfRecord = s
				var events []ObserverEvent
				if observed {
					sor = observingSoR{inner: s, clock: time.Now, observer: func(e ObserverEvent) { events = append(events, e) }}
				}
				g := &Gateway{cfg: Config{SoR: sor}}
				w := httptest.NewRecorder()
				r := httptest.NewRequest("POST", "/scenario", strings.NewReader(`{"branch":"covered"}`))
				g.handleScenario(w, r)
				want := 502
				if fail == "coverage" {
					want = 503
				}
				if w.Code != want || strings.Contains(w.Body.String(), "sentinel") {
					t.Fatalf("status/body: %d %s", w.Code, w.Body.String())
				}
				if observed {
					if len(events) == 0 || strings.Contains(events[len(events)-1].Detail, "not found") {
						t.Fatalf("failure reported as absence: %+v", events)
					}
				}
			})
		}
	}
}

// scriptedReadSoR fails a selected read while all other reads keep the census controls.
// Legacy entrypoints report an error, so a route cannot pass by erasing the capability.
type scriptedReadSoR struct {
	t      *testing.T
	base   ContextSystemOfRecord
	before func(context.Context, string, string) error
}

func (s scriptedReadSoR) ResolvePatient(key string) (string, Demo, bool) {
	s.t.Error("legacy ResolvePatient selected")
	return "", Demo{}, false
}
func (s scriptedReadSoR) ResolvePatientContext(ctx context.Context, key string) (string, Demo, bool, error) {
	if err := s.before(ctx, "ResolvePatient", key); err != nil {
		return "", Demo{}, false, err
	}
	return s.base.ResolvePatientContext(ctx, key)
}
func (s scriptedReadSoR) PatientFHIRRef(key string) (string, bool) {
	s.t.Error("legacy PatientFHIRRef selected")
	return "", false
}
func (s scriptedReadSoR) PatientFHIRRefContext(ctx context.Context, key string) (string, bool, error) {
	if err := s.before(ctx, "PatientFHIRRef", key); err != nil {
		return "", false, err
	}
	return s.base.PatientFHIRRefContext(ctx, key)
}
func (s scriptedReadSoR) CoverageInforce(key string) (bool, string) {
	s.t.Error("legacy CoverageInforce selected")
	return false, ""
}
func (s scriptedReadSoR) CoverageInforceContext(ctx context.Context, key string) (bool, string, error) {
	if err := s.before(ctx, "CoverageInforce", key); err != nil {
		return false, "", err
	}
	return s.base.CoverageInforceContext(ctx, key)
}
func (s scriptedReadSoR) ClinicalContext(key string) (shnsdk.ClinicalContext, bool) {
	s.t.Error("legacy ClinicalContext selected")
	return shnsdk.ClinicalContext{}, false
}
func (s scriptedReadSoR) ClinicalContextContext(ctx context.Context, key string) (shnsdk.ClinicalContext, bool, error) {
	if err := s.before(ctx, "ClinicalContext", key); err != nil {
		return shnsdk.ClinicalContext{}, false, err
	}
	return s.base.ClinicalContextContext(ctx, key)
}
func (s scriptedReadSoR) SupplementalReport(key string) ([]byte, bool) {
	s.t.Error("legacy SupplementalReport selected")
	return nil, false
}
func (s scriptedReadSoR) SupplementalReportContext(ctx context.Context, key string) ([]byte, bool, error) {
	if err := s.before(ctx, "SupplementalReport", key); err != nil {
		return nil, false, err
	}
	return s.base.SupplementalReportContext(ctx, key)
}
func (s scriptedReadSoR) FacilityRecords(key string) (map[string][]byte, bool) {
	s.t.Error("legacy FacilityRecords selected")
	return nil, false
}
func (s scriptedReadSoR) FacilityRecordsContext(ctx context.Context, key string) (map[string][]byte, bool, error) {
	if err := s.before(ctx, "FacilityRecords", key); err != nil {
		return nil, false, err
	}
	return s.base.FacilityRecordsContext(ctx, key)
}
func (s scriptedReadSoR) OpenOrder(key string) ([]byte, bool) {
	s.t.Error("legacy OpenOrder selected")
	return nil, false
}
func (s scriptedReadSoR) OpenOrderContext(ctx context.Context, key string) ([]byte, bool, error) {
	if err := s.before(ctx, "OpenOrder", key); err != nil {
		return nil, false, err
	}
	return s.base.OpenOrderContext(ctx, key)
}
func (s scriptedReadSoR) OpenCoverage(key string) ([]byte, bool) {
	s.t.Error("legacy OpenCoverage selected")
	return nil, false
}
func (s scriptedReadSoR) OpenCoverageContext(ctx context.Context, key string) ([]byte, bool, error) {
	if err := s.before(ctx, "OpenCoverage", key); err != nil {
		return nil, false, err
	}
	return s.base.OpenCoverageContext(ctx, key)
}
func (s scriptedReadSoR) ResolveByReference(key string) ([]byte, bool) {
	s.t.Error("legacy ResolveByReference selected")
	return nil, false
}
func (s scriptedReadSoR) ResolveByReferenceContext(ctx context.Context, key string) ([]byte, bool, error) {
	if err := s.before(ctx, "ResolveByReference", key); err != nil {
		return nil, false, err
	}
	return s.base.ResolveByReferenceContext(ctx, key)
}

func TestSoRSubjectRouteFamilies(t *testing.T) {
	ctx := context.WithValue(context.Background(), struct{}{}, "request")
	pas := loadPASGolden(t, "MBR-COVERED")
	crd := conformantCRD("MBR-COVERED", "72148")
	dispatch := []byte(`{"hook":"order-dispatch","context":{"patientId":"MBR-COVERED","dispatchedOrders":["DeviceRequest/dr1"],"performer":"Organization/dme1"},"prefetch":{"deviceHistory":{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"DeviceRequest","id":"dr1","subject":{"reference":"Patient/MBR-COVERED"}}}]}}}`)
	for _, observed := range []bool{false, true} {
		for _, route := range []string{"crd-ingress", "crd-native", "dispatch-native", "pas-ingress", "pas-native", "pas-update", "next-question", "kept-bundle"} {
			t.Run(route+map[bool]string{false: "/bare", true: "/observed"}[observed], func(t *testing.T) {
				s := scriptedReadSoR{t: t, base: ReadSystemOfRecord(newCensusSoR()), before: func(got context.Context, op, key string) error {
					if got != ctx {
						t.Error("request context lost")
					}
					return &SoRReadError{Kind: SoRUnavailable}
				}}
				var sor SystemOfRecord = s
				if observed {
					sor = observingSoR{inner: s, clock: time.Now, observer: func(e ObserverEvent) {
						if e.Detail != "unavailable" {
							t.Errorf("unsafe observation %+v", e)
						}
					}}
				}
				g := &Gateway{cfg: Config{SoR: sor}}
				var status int
				var msg string
				switch route {
				case "crd-ingress":
					_, status, msg = g.ingressCRDSubjectPCIContext(ctx, crd)
				case "crd-native":
					_, _, status, msg = g.conformantCRDBindContext(ctx, crd, "pci")
				case "dispatch-native":
					_, _, status, msg = g.conformantCRDDispatchBindContext(ctx, dispatch, "pci")
				case "pas-ingress":
					_, status, msg = g.ingressPASNativeSubjectPCIContext(ctx, pas)
				case "pas-native":
					_, status, msg = g.conformantPASBindContext(ctx, pas, "pci")
				case "pas-update":
					_, status, msg = g.conformantPASUpdateBindContext(ctx, pas, "pci")
				case "next-question":
					status, msg = g.bindNextQuestionSubjectContext(ctx, "Patient/MBR-COVERED", "pci")
				case "kept-bundle":
					status, msg = g.fenceKeptBundleContext(ctx, []byte(`{"resourceType":"Bundle","entry":[{"resource":{"subject":{"reference":"Patient/MBR-COVERED"}}}]}`), "pci")
				}
				if status != 503 || strings.Contains(msg, "sentinel") {
					t.Fatalf("status/message %d %s", status, msg)
				}
			})
		}
	}
}

func TestSoRLateBindingFailureAndConcurrentIsolation(t *testing.T) {
	type failKey struct{}
	s := scriptedReadSoR{t: t, base: ReadSystemOfRecord(newCensusSoR())}
	// Use the established member for first lookup and a repeated-read counter local to each request.
	type countKey struct{}
	s.before = func(ctx context.Context, op, key string) error {
		n := ctx.Value(countKey{}).(*int)
		*n++
		if ctx.Value(failKey{}) == true && *n == 2 {
			return errors.New("private-upstream-sentinel")
		}
		return nil
	}
	body := crdReqJSON("MBR-COVERED", "Patient/MBR-COVERED", "Patient/MBR-COVERED")
	g := &Gateway{cfg: Config{SoR: s}}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(fail bool) {
			defer wg.Done()
			n := 0
			ctx := context.WithValue(context.Background(), countKey{}, &n)
			ctx = context.WithValue(ctx, failKey{}, fail)
			_, status, msg := g.ingressCRDSubjectPCIContext(ctx, body)
			want := 0
			if fail {
				want = 502
			}
			if status != want {
				t.Errorf("status/message %d %s want %d", status, msg, want)
			}
			if fail && n != 2 {
				t.Errorf("read after failure: %d", n)
			}
		}(i%2 == 0)
	}
	wg.Wait()
}

func TestSoRManagedAndEvidenceFailures(t *testing.T) {
	for _, err := range []error{errors.New("private-upstream-sentinel"), &SoRReadError{Kind: SoRUnavailable}, context.DeadlineExceeded} {
		s := scriptedReadSoR{t: t, base: ReadSystemOfRecord(newCensusSoR()), before: func(context.Context, string, string) error { return err }}
		qr, fill, got := newManagedPopulator(s).Populate(context.Background(), wrapDemoLumbarPackage(t), PopulateContext{Member: "MBR-COVERED"})
		want, _ := SoRFailureResponse(err)
		if got == nil || statusForPopulateErr(got) != want || strings.Contains(got.Error(), "sentinel") || qr != nil || fill != nil {
			t.Fatalf("populate masked backend failure: %v %s %+v", got, qr, fill)
		}
		g := &Gateway{cfg: Config{SoR: s}}
		evidence, got := g.homeOxygenAutoFillEvidenceContext(context.Background(), "MBR-COVERED", nil)
		if got == nil || evidence != nil {
			t.Fatalf("evidence failure became absence: %v %+v", got, evidence)
		}
		_, _, got = g.resolvePrefetchFromSoRContext(context.Background(), "patient", "MBR-COVERED", "Patient/logical")
		if got == nil {
			t.Fatal("patient reference failure used logical fallback")
		}
	}
}

func TestSoRReferenceCallbackStopsAndSanitizes(t *testing.T) {
	calls := 0
	s := scriptedReadSoR{t: t, base: ReadSystemOfRecord(newCensusSoR()), before: func(context.Context, string, string) error { calls++; return errors.New("private-upstream-sentinel") }}
	resolve, err := sorReferenceCallback(context.Background(), s)
	for i := 0; i < 3; i++ {
		if b, ok := resolve("Organization/payer"); ok || b != nil {
			t.Fatal("failed read returned data")
		}
	}
	if calls != 1 || *err == nil || strings.Contains((*err).Error(), "sentinel") {
		t.Fatalf("callback calls/error %d %v", calls, *err)
	}
	g := &Gateway{cfg: Config{SoR: s, PayerRouter: payerRouterFor(t, "payer")}}
	_, _, status, msg := g.recipientForSoR(context.Background(), []byte(`{"resourceType":"Coverage","payor":[{"reference":"Organization/payer"}]}`))
	if status != 502 || strings.Contains(msg, "sentinel") {
		t.Fatalf("recipient failure masked: %d %s", status, msg)
	}
}

func TestSoRNativeHandlersStopBeforeResponder(t *testing.T) {
	s := scriptedReadSoR{t: t, base: ReadSystemOfRecord(newCensusSoR()), before: func(context.Context, string, string) error { return &SoRReadError{Kind: SoRAuthenticationFailed} }}
	g := &Gateway{cfg: Config{SoR: s}} // A responder call would panic: failure must stop first.
	for _, route := range []string{"crd", "pas", "patient-dtr", "eligibility", "records"} {
		t.Run(route, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/substrate/inbound", nil)
			switch route {
			case "crd":
				g.handleCRDNativeInbound(w, r, shnsdk.Envelope{}, shnsdk.Token{}, conformantCRD("MBR-COVERED", "72148"), "")
			case "pas":
				g.handlePASNativeInbound(w, r, shnsdk.Envelope{}, shnsdk.Token{}, loadPASGolden(t, "MBR-COVERED"), "")
			case "eligibility":
				b, e := shnsdk.BuildEligibilityRequest("MBR-COVERED", "1234567890", time.Now())
				if e != nil {
					t.Fatal(e)
				}
				g.handleEligibilityInbound(w, r, shnsdk.Envelope{}, shnsdk.Token{}, b, "")
			case "records":
				b, e := shnsdk.BuildCDexTaskDataRequest("Patient/MBR-COVERED", "DocumentReference", "2023-01-01", "2023-12-31", shnsdk.CDexTaskMeta{AuthoredOn: time.Unix(1700000000, 0), Requester: "provider", Owner: "facility"})
				if e != nil {
					t.Fatal(e)
				}
				env := shnsdk.Envelope{}
				env.Metadata.ConsentRef = "consent"
				g.handleFederatedQueryInbound(w, r, env, shnsdk.Token{}, b, "")
			case "patient-dtr":
				g.handlePatientDTRInbound(w, r, shnsdk.Envelope{}, shnsdk.Token{}, []byte(`{"patientRef":"Patient/MBR-COVERED"}`), "")
			}
			if w.Code != 502 {
				t.Fatalf("status %d body %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestSoRLateDispatchRefFailureNoPopulateOrPAS(t *testing.T) {
	for _, observed := range []bool{false, true} {
		t.Run(map[bool]string{false: "bare", true: "observed"}[observed], func(t *testing.T) {
			fixture := newMBROXDispatchFixture(t)
			s := scriptedReadSoR{t: t, base: ReadSystemOfRecord(fixture.sor), before: func(ctx context.Context, op, key string) error {
				if op == "PatientFHIRRef" {
					return &SoRReadError{Kind: SoRUnavailable}
				}
				return nil
			}}
			var sor SystemOfRecord = s
			if observed {
				sor = observingSoR{inner: s, clock: fixture.gw.cfg.Clock, observer: func(e ObserverEvent) {
					if e.Op == "PatientFHIRRef" && e.Detail != "unavailable" {
						t.Errorf("failure misreported %+v", e)
					}
				}}
			}
			fixture.gw.cfg.SoR = sor
			fixture.gw.cfg.Populator = nil // Any population after the failed ref lookup is a test failure.
			w := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/scenario/dispatch", strings.NewReader(`{"member":"MBR-OX"}`))
			fixture.gw.handleDispatch(w, r)
			if w.Code != 503 {
				t.Fatalf("late read status %d %s", w.Code, w.Body.String())
			}
			if !legAttempted(fixture.stub.legTypes, "dtr-questionnaire-fetch") || legAttempted(fixture.stub.legTypes, "pas-claim") {
				t.Fatalf("unexpected legs %+v", fixture.stub.legTypes)
			}
		})
	}
}

func TestSoRFHIROperationEnvelope(t *testing.T) {
	for _, kind := range []SoRFailureKind{SoRAuthenticationFailed, SoRUnavailable} {
		s := scriptedReadSoR{t: t, base: ReadSystemOfRecord(newCensusSoR()), before: func(context.Context, string, string) error { return &SoRReadError{Kind: kind} }}
		g := &Gateway{cfg: Config{SoR: s}}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/fhir/Claim/$submit", nil)
		g.handlePASNativeInbound(&fhirOperationWriter{w}, r, shnsdk.Envelope{}, shnsdk.Token{}, loadPASGolden(t, "MBR-COVERED"), "")
		want, _ := SoRFailureResponse(&SoRReadError{Kind: kind})
		assertFHIRIngressError(t, w, want)
		code := "processing"
		if want == 503 {
			code = "transient"
		}
		if !strings.Contains(w.Body.String(), `"code":"`+code+`"`) {
			t.Fatalf("wrong issue category %s", w.Body.String())
		}
	}
}

func TestSoRObserverEveryReadFailure(t *testing.T) {
	s := scriptedReadSoR{t: t, base: ReadSystemOfRecord(newCensusSoR()), before: func(context.Context, string, string) error { return errors.New("private-upstream-sentinel") }}
	var events []ObserverEvent
	reader := ReadSystemOfRecord(observingSoR{inner: s, clock: func() time.Time { return time.Unix(1700000000, 0) }, observer: func(e ObserverEvent) { events = append(events, e) }})
	ctx := context.Background()
	reads := []func() error{
		func() error { _, _, _, e := reader.ResolvePatientContext(ctx, "member"); return e },
		func() error { _, _, e := reader.PatientFHIRRefContext(ctx, "member"); return e },
		func() error { _, _, e := reader.CoverageInforceContext(ctx, "member"); return e },
		func() error { _, _, e := reader.ClinicalContextContext(ctx, "member"); return e },
		func() error { _, _, e := reader.SupplementalReportContext(ctx, "member"); return e },
		func() error { _, _, e := reader.FacilityRecordsContext(ctx, "member"); return e },
		func() error { _, _, e := reader.OpenOrderContext(ctx, "member"); return e },
		func() error { _, _, e := reader.OpenCoverageContext(ctx, "member"); return e },
		func() error { _, _, e := reader.ResolveByReferenceContext(ctx, "ref"); return e },
	}
	for _, read := range reads {
		if err := read(); err == nil {
			t.Fatal("observer erased error")
		}
	}
	if len(events) != 9 {
		t.Fatalf("events %d", len(events))
	}
	for _, e := range events {
		if e.Detail != "invalid_response" || e.Payload != nil {
			t.Fatalf("unsafe observation %+v", e)
		}
	}
}
