package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/SmartHealthNetwork/shn-gateway/connectors/exchangecontext"
	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"github.com/golang-jwt/jwt/v5"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNoneNeverRunsValidation(t *testing.T) {
	g := &Gateway{cfg: Config{ConformanceEnforcement: EnforcementNone}}
	g.startCertification()
	defer g.Close()
	if g.certification != nil {
		t.Fatal("none started a certification worker")
	}
	in := CheckInput{Body: []byte("{bad"), Exchange: ExchangeContext{policy: NewConformancePolicy(EnforcementNone)}}
	if g.observeContent(in) {
		t.Fatal("none enqueued a payload")
	}
}

func TestObservationCallbackDoesNotBlock(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	g := &Gateway{cfg: Config{Clock: time.Now, Observer: func(ObserverEvent) { close(entered); <-release }}}
	done := make(chan struct{})
	go func() { g.observe(ObserverEvent{Kind: "boundary.prepared"}); close(done) }()
	<-entered
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("callback blocked emitter")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if g.WaitObserverCompletion(ctx) == nil {
		t.Fatal("canceled barrier succeeded")
	}
}

type observationValidator func(context.Context, []byte, string) (shnsdk.ValidationEvidence, error)

func (f observationValidator) Validate(context.Context, []byte, string) (shnsdk.Result, error) {
	panic("legacy validation unexpectedly called")
}
func (f observationValidator) ValidateEvidence(c context.Context, b []byte, p string) (shnsdk.ValidationEvidence, error) {
	return f(c, b, p)
}
func observationInput(level ConformanceEnforcement) CheckInput {
	return CheckInput{Body: []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/p"}}}]}`), Direction: "request", DeclaredVersion: "pa.pas@2.0", Exchange: ExchangeContext{policy: NewConformancePolicy(level), legType: "pas-claim", contractVersion: "pa.pas@2.0", subjectPCI: "pci", holder: "provider"}}
}
func newObservationGateway(t *testing.T, level ConformanceEnforcement, v shnsdk.Validator) *Gateway {
	t.Helper()
	g := &Gateway{cfg: Config{Clock: time.Now, HolderID: "provider", ConformanceEnforcement: level, Validator: v}}
	g.startCertification()
	t.Cleanup(func() { g.Close() })
	return g
}
func observationFlush(t *testing.T, g *Gateway) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := g.WaitObserverCompletion(ctx); err != nil {
		t.Fatal(err)
	}
}
func TestObservationRegistryImmutableAndNonblocking(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	g := newObservationGateway(t, EnforcementObserve, observationValidator(func(ctx context.Context, b []byte, p string) (shnsdk.ValidationEvidence, error) {
		once.Do(func() { close(entered) })
		select {
		case <-release:
		case <-ctx.Done():
			return shnsdk.ValidationEvidence{}, ctx.Err()
		}
		return *syntheticEvidence(), nil
	}))
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	in := observationInput(EnforcementObserve)
	digest := sha256hex(in.Body)
	if !g.observeContent(in) {
		t.Fatal("job dropped")
	}
	<-entered
	copy(in.Body, bytes.Repeat([]byte("x"), len(in.Body)))
	close(release)
	observationFlush(t, g)
	w := g.certification
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.findings) == 0 {
		t.Fatal("no registry observations")
	}
	for _, f := range w.findings {
		if f.PayloadSHA256 != digest || f.Action != "not_enforced" || f.RuleSet != ConformanceRuleSet || f.Gateway != "provider" || f.Decision != "" {
			t.Fatalf("finding=%+v", f)
		}
	}
	if g.observationMemory.bytes != 0 {
		t.Fatal("body reservation leaked")
	}
}
func TestObservationBasicOnlyDeepAndPanicRecovery(t *testing.T) {
	var calls atomic.Int32
	g := newObservationGateway(t, EnforcementBasic, observationValidator(func(context.Context, []byte, string) (shnsdk.ValidationEvidence, error) {
		if calls.Add(1) == 1 {
			panic("FOREIGN-RESOURCE")
		}
		return *syntheticEvidence(), nil
	}))
	if !g.observeContent(observationInput(EnforcementBasic)) {
		t.Fatal("dropped")
	}
	observationFlush(t, g)
	if !g.observeContent(observationInput(EnforcementBasic)) {
		t.Fatal("dropped")
	}
	observationFlush(t, g)
	w := g.certification
	w.mu.Lock()
	defer w.mu.Unlock()
	unavailable, valid := false, false
	for _, f := range w.findings {
		if f.Rule == "fhir.profile" {
			unavailable = unavailable || f.State == CheckUnavailable
			valid = valid || f.State == CheckValid
		}
		for _, r := range StructuralRules() {
			if f.Rule == r.ID {
				t.Fatal("basic observed structural rule")
			}
		}
	}
	if !unavailable || !valid {
		t.Fatal("panic was not unavailable followed by valid recovery")
	}
}
func TestObservationNotificationOwnershipAndPanic(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	received := make(chan ObserverEvent, 2)
	var calls atomic.Int32
	g := newObservationGateway(t, EnforcementNone, nil)
	g.cfg.Observer = func(e ObserverEvent) {
		switch calls.Add(1) {
		case 1:
			close(entered)
			<-release
			panic("FOREIGN-RESOURCE")
		default:
			received <- e
		}
	}
	g.observe(ObserverEvent{Kind: "test.block"})
	<-entered
	raw := []byte(`original`)
	route := &RouteInfo{Own: []string{"first"}}
	g.observe(ObserverEvent{Kind: "leg.originated", Payload: raw, Route: route})
	copy(raw, []byte(`mutated!`))
	route.Own[0] = "changed"
	close(release)
	select {
	case e := <-received:
		if string(e.Payload) != "original" || e.Route.Own[0] != "first" {
			t.Fatalf("borrowed memory changed: %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not recover")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if g.WaitObserverCompletion(ctx) == nil {
		t.Fatal("panic reported successful observer coverage")
	}
	if g.certification != nil {
		t.Fatal("none inspection started conformance")
	}
}
func TestObservationSharedBudgetAndBoundedClose(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	g := newObservationGateway(t, EnforcementObserve, observationValidator(func(ctx context.Context, _ []byte, _ string) (shnsdk.ValidationEvidence, error) {
		once.Do(func() { close(entered) })
		<-release
		return *syntheticEvidence(), nil
	}))
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	g.observeContent(observationInput(EnforcementObserve))
	<-entered
	g.cfg.Observer = func(ObserverEvent) {}
	large := make([]byte, observationMessageLimit)
	for i := 0; i < 3; i++ {
		in := observationInput(EnforcementObserve)
		in.Body = large
		if !g.observeContent(in) {
			t.Fatal("budget prematurely exhausted")
		}
	}
	in := observationInput(EnforcementObserve)
	in.Body = large
	if g.observeContent(in) {
		t.Fatal("budget exceeded")
	}
	g.observe(ObserverEvent{Kind: "leg.originated", Payload: large})
	g.observerDispatch.mu.Lock()
	drops := g.observerDispatch.dropped
	g.observerDispatch.mu.Unlock()
	if drops == 0 {
		t.Fatal("inspection bypassed shared budget")
	}
	g.observationMemory.mu.Lock()
	used := g.observationMemory.bytes
	g.observationMemory.mu.Unlock()
	if used > observationBodyBudget {
		t.Fatal("budget exceeded")
	}
	releaseOnce.Do(func() { close(release) })
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	g.observationMemory.mu.Lock()
	defer g.observationMemory.mu.Unlock()
	if g.observationMemory.bytes != 0 {
		t.Fatal("close leaked body reservations")
	}
}
func TestObservationBlockedCallbackCloseAndSaturation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	g := newObservationGateway(t, EnforcementNone, nil)
	g.cfg.Observer = func(ObserverEvent) { once.Do(func() { close(entered) }); <-release }
	defer close(release)
	g.observe(ObserverEvent{Kind: "blocked"})
	<-entered
	for i := 0; i < 1000; i++ {
		g.observe(ObserverEvent{Kind: "queued"})
	}
	d := &g.observerDispatch
	d.mu.Lock()
	n, dropped := len(d.queue), d.dropped
	d.mu.Unlock()
	if n != 32 || dropped != 968 {
		t.Fatalf("queue=%d dropped=%d", n, dropped)
	}
	done := make(chan error, 1)
	go func() { done <- g.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("close waited for arbitrary callback")
	}
}

// Mounted ingress with real originator crypto; Hub/authz/recipient are the
// explicit relaySubstrate test doubles. The root pair companion uses real Hub.
func TestObservationMountedNativeNoninterference(t *testing.T) {
	for _, mode := range []string{"none-disabled", "none-inspection", "observe-blocked-validator", "observe-blocked-callback", "observe-saturated"} {
		t.Run(mode, func(t *testing.T) {
			pair := newInProcessExchange(t)
			g := pair.originator
			level := EnforcementObserve
			if strings.HasPrefix(mode, "none") {
				level = EnforcementNone
			}
			g.cfg.ConformanceEnforcement = level
			entered, release := make(chan struct{}), make(chan struct{})
			var once, releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			var validations atomic.Int32
			g.cfg.Validator = observationValidator(func(ctx context.Context, _ []byte, _ string) (shnsdk.ValidationEvidence, error) {
				validations.Add(1)
				if level == EnforcementNone {
					panic("none validator")
				}
				once.Do(func() { close(entered) })
				select {
				case <-release:
				case <-ctx.Done():
					return shnsdk.ValidationEvidence{}, ctx.Err()
				}
				return *syntheticEvidence(), nil
			})
			g.cfg.CertificationValidatorsByLine = map[string]shnsdk.Validator{"2.0": certificationValidatorFunc(func(context.Context, []byte, string) (shnsdk.Result, error) { panic("passive certifier") })}
			g.cfg.SubjectReferenceResolver = subjectResolverFunc(func(context.Context, PatientReference) (string, bool, error) { panic("patient resolver") })
			g.cfg.SoR = nativeReadPanicSoR{}
			if mode == "none-inspection" {
				g.cfg.Observer = func(ObserverEvent) { panic("optional observer") }
			}
			if mode == "observe-blocked-callback" {
				g.cfg.Observer = func(ObserverEvent) { once.Do(func() { close(entered) }); <-release }
			}
			g.startCertification()
			defer g.Close()
			if mode == "observe-saturated" {
				g.observeContent(observationInput(level))
				<-entered
				for i := 0; i < 32; i++ {
					g.observeContent(observationInput(level))
				}
			}
			g.cfg.ingressAuthBypass = false
			key, pub := newTestClientKey(t)
			g.ingressAuth = newTestAuthServer(t, "native-source", pub, "ES384")
			reg := g.ingressAuth.clients["native-source"]
			reg.ContextOperations = []string{"pas-submit"}
			g.ingressAuth.clients["native-source"] = reg
			entry, _ := g.cfg.Reg.Lookup("payer")
			entry.RequestFrames = shnsdk.SupportedRequestFrames()
			g.cfg.Reg.Set("payer", entry)
			body := observationInput(level).Body
			r := httptest.NewRequest("POST", "/Claim/$submit", bytes.NewReader(body))
			r.Header.Set("Content-Type", "application/fhir+json")
			now := g.ingressAuth.now()
			c := exchangecontext.Claims{RegisteredClaims: jwt.RegisteredClaims{Issuer: "native-source", Subject: "native-source", Audience: jwt.ClaimStrings{testIngressBaseURL + r.URL.Path}, ID: "observation-once", IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute))}, Holder: "provider", Recipient: "payer", Leg: "pas-claim", Operation: "pas-submit", SubjectPCI: "externally-established-pci", CorrelationID: "observation-corr", ContentType: r.Header.Get("Content-Type"), ContractVersion: "pa.pas@2.0"}
			putContext(t, r, c, body, key)
			r.Header.Set("Authorization", "Bearer "+signJWT(t, jwt.SigningMethodES384, key, directClaims(c.Issuer, testIngressBaseURL+r.URL.Path, now)))
			answer := []byte("opaque recipient answer")
			framed, err := shnsdk.EncodeHTTPFrame(201, "application/custom", answer)
			if err != nil {
				t.Fatal(err)
			}
			pair.payerReturns(LegResult{Response: relay.Exact(relay.NewBody(framed, relay.OriginPeerFrame), "application/json")})
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { g.Handler().ServeHTTP(w, r); close(done) }()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("optional work blocked native response")
			}
			if w.Code != 201 || !bytes.Equal(w.Body.Bytes(), answer) || pair.routeHitCount() != 1 {
				t.Fatalf("native response %d %s", w.Code, w.Body.String())
			}
			_, sent, err := shnsdk.DecodeHTTPFrame(pair.lastRequestPayload())
			if err != nil || !bytes.Equal(sent, body) {
				t.Fatal("request changed")
			}
			if level == EnforcementNone {
				if validations.Load() != 0 || g.certification != nil {
					t.Fatal("none ran checks")
				}
			} else {
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("no real optional work reached")
				}
			}
			releaseOnce.Do(func() { close(release) })
		})
	}
}
func TestObservationUncooperativeCheckerCloseBound(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var targetBytes atomic.Int64
	g := newObservationGateway(t, EnforcementObserve, observationValidator(func(_ context.Context, target []byte, _ string) (shnsdk.ValidationEvidence, error) {
		targetBytes.Store(int64(len(target)))
		once.Do(func() { close(entered) })
		<-release
		return *syntheticEvidence(), nil
	}))
	g.observeContent(observationInput(EnforcementObserve))
	<-entered
	for i := 0; i < 1000; i++ {
		g.observeContent(observationInput(EnforcementObserve))
	}
	start := time.Now()
	err := g.Close()
	if err != context.DeadlineExceeded || time.Since(start) > 7*time.Second {
		t.Fatalf("unbounded close: %v %v", err, time.Since(start))
	}
	g.observationMemory.mu.Lock()
	held := g.observationMemory.bytes
	g.observationMemory.mu.Unlock()
	if held != len(observationInput(EnforcementObserve).Body)+int(targetBytes.Load()) {
		t.Fatalf("stranded copy budget=%d", held)
	}
	close(release)
	<-g.certification.done
	g.observationMemory.mu.Lock()
	defer g.observationMemory.mu.Unlock()
	if g.observationMemory.bytes != 0 {
		t.Fatal("stranded job failed to release after returning")
	}
}

func TestObservationLimitsExpiryAndRing(t *testing.T) {
	var calls atomic.Int32
	g := newObservationGateway(t, EnforcementObserve, observationValidator(func(context.Context, []byte, string) (shnsdk.ValidationEvidence, error) {
		calls.Add(1)
		return *syntheticEvidence(), nil
	}))
	in := observationInput(EnforcementObserve)
	if !g.enqueueObservation(certificationJob{input: &in, payload: in.Body, queued: time.Now().Add(-31 * time.Second)}) {
		t.Fatal("expired job not admitted")
	}
	observationFlush(t, g)
	records, _ := g.ConformanceObservationsForTest()
	if calls.Load() != 0 || len(records) == 0 {
		t.Fatal("expired job executed validators")
	}
	for _, f := range records {
		if f.State != CheckUnavailable {
			t.Fatal("expired job misclassified", f)
		}
	}
	for i := 0; i < 50; i++ {
		in.Exchange.correlationID = fmt.Sprint(i)
		g.observeContent(in)
		observationFlush(t, g)
	}
	records, _ = g.ConformanceObservationsForTest()
	if len(records) != 256 || records[len(records)-1].CorrelationID != "49" {
		t.Fatal("ring not bounded/recent")
	}
	records[0].Rule = "caller mutation"
	again, _ := g.ConformanceObservationsForTest()
	if again[0].Rule == "caller mutation" {
		t.Fatal("ring borrowed")
	}
	in.Body = make([]byte, observationMessageLimit+1)
	if g.observeContent(in) {
		t.Fatal("oversize job copied")
	}
	_, drops := g.ConformanceObservationsForTest()
	if drops != 1 {
		t.Fatal("oversize drop not counted")
	}
	if g.observationMemory.bytes != 0 {
		t.Fatal("retained raw body")
	}
}
func TestObservationCandidateTimeout(t *testing.T) {
	g := newObservationGateway(t, EnforcementObserve, observationValidator(func(ctx context.Context, _ []byte, _ string) (shnsdk.ValidationEvidence, error) {
		<-ctx.Done()
		return shnsdk.ValidationEvidence{}, ctx.Err()
	}))
	start := time.Now()
	g.observeContent(observationInput(EnforcementObserve))
	observationFlush(t, g)
	if d := time.Since(start); d < certificationCandidateTimeout || d > 3*time.Second {
		t.Fatalf("candidate timeout %v", d)
	}
	records, _ := g.ConformanceObservationsForTest()
	for _, f := range records {
		if f.Rule == "fhir.profile" && f.State != CheckUnavailable {
			t.Fatal("timeout became invalid or valid")
		}
	}
}
func TestObservationMetadataOnlyHasNoRawCopy(t *testing.T) {
	g := newObservationGateway(t, EnforcementNone, nil)
	g.observe(ObserverEvent{Payload: make([]byte, observationMessageLimit)})
	if g.observationMemory.bytes != 0 {
		t.Fatal("disabled observer copied")
	}
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	g.cfg.Observer = func(e ObserverEvent) {
		if len(e.Payload) != 0 {
			t.Error("metadata gained payload")
		}
		close(entered)
		<-release
	}
	g.observe(ObserverEvent{Kind: "boundary.prepared", Detail: `{"edit":"E01"}`})
	<-entered
	if g.observationMemory.bytes > 1024 || g.certification != nil {
		t.Fatal("metadata notification retained clinical bytes or started checker")
	}
}
func TestObservationSafeFindingDiagnostics(t *testing.T) {
	const phi = "FOREIGN-PATIENT-SECRET"
	f := safeFinding(ConformanceFinding{Issues: []string{phi, phi}, Path: "Patient/" + phi, Decision: "relayed", CheckIssues: []CheckIssue{{Severity: phi, Code: phi}}, CheckClass: CheckClass(phi), Operation: phi, ResultSeverity: phi, ClosedReason: phi, Profile: phi, Profiles: []string{phi}})
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(phi)) || bytes.Contains(b, []byte("sha256")) || f.Decision != "" || f.Path != "" || f.Action != "not_enforced" || len(f.Issues) != 1 || f.Issues[0] != "validator_issues count=2" || f.CheckClass != "" || f.Operation != "" || f.ResultSeverity != "" || f.ClosedReason != "" || f.Profile != "" || len(f.Profiles) != 0 {
		t.Fatalf("unsafe finding %s", b)
	}
}

// A dispatcher that has already dequeued when Close wins must account for the
// suppressed callback as a drop, just like Close's queue drain.
func TestObservationClosedDequeuedNotificationIsDropped(t *testing.T) {
	g := &Gateway{cfg: Config{Observer: func(ObserverEvent) { t.Error("closed callback ran") }}}
	d := &g.observerDispatch
	d.queue = make(chan observerNotification, 1)
	d.stop = make(chan struct{})
	d.changed = make(chan struct{})
	d.closed = true
	d.accepted = 1
	d.queue <- observerNotification{sequence: 1}
	go g.runObserver(d)
	d.mu.Lock()
	for d.completed == 0 {
		changed := d.changed
		d.mu.Unlock()
		<-changed
		d.mu.Lock()
	}
	dropped := d.dropped
	d.mu.Unlock()
	close(d.stop)
	if dropped != 1 {
		t.Fatalf("suppressed notification drops=%d, want 1", dropped)
	}
}

func TestObservationTargetSerializationPressureUnavailable(t *testing.T) {
	var calls atomic.Int32
	g := newObservationGateway(t, EnforcementObserve, observationValidator(func(context.Context, []byte, string) (shnsdk.ValidationEvidence, error) {
		calls.Add(1)
		return *syntheticEvidence(), nil
	}))
	in := observationInput(EnforcementObserve)
	pressure := observationBodyBudget - len(in.Body)
	if !g.observationMemory.reserve(pressure) {
		t.Fatal("pressure")
	}
	defer g.observationMemory.release(pressure)
	if !g.observeContent(in) {
		t.Fatal("job admission must fit")
	}
	observationFlush(t, g)
	if calls.Load() != 0 {
		t.Fatalf("validator invoked with uncharged target: %d", calls.Load())
	}
	rows, _ := g.ConformanceObservationsForTest()
	found := false
	for _, r := range rows {
		if r.Rule == "fhir.profile" {
			found = true
			if r.State != CheckUnavailable || r.Action != "not_enforced" {
				t.Fatalf("budget result=%+v", r)
			}
		}
	}
	if !found {
		t.Fatal("profile coverage missing")
	}
}

func TestObservationTargetsOwnOneSerializedResourceAtATime(t *testing.T) {
	var calls atomic.Int32
	var g *Gateway
	in := observationInput(EnforcementObserve)
	in.Exchange.legType = "crd-order-select"
	in.Exchange.contractVersion = "pa.crd@2.0"
	in.DeclaredVersion = "pa.crd@2.0"
	in.Body = []byte(`{"hook":"order-select","hookInstance":"synthetic","context":{"patientId":"p"},"prefetch":{"one":{"resourceType":"Patient","id":"one"},"two":{"resourceType":"Patient","id":"two"}}}`)
	target := []byte(`{"id":"one","resourceType":"Patient"}`)
	pressure := observationBodyBudget - len(in.Body) - len(target)
	g = newObservationGateway(t, EnforcementObserve, observationValidator(func(_ context.Context, raw []byte, _ string) (shnsdk.ValidationEvidence, error) {
		calls.Add(1)
		g.observationMemory.mu.Lock()
		held := g.observationMemory.bytes
		g.observationMemory.mu.Unlock()
		if held != pressure+len(in.Body)+len(raw) {
			t.Errorf("target ownership=%d", held)
		}
		return *syntheticEvidence(), nil
	}))
	if !g.observationMemory.reserve(pressure) {
		t.Fatal("pressure")
	}
	defer g.observationMemory.release(pressure)
	if !g.observeContent(in) {
		t.Fatal("admission")
	}
	observationFlush(t, g)
	if calls.Load() != 2 {
		t.Fatalf("target calls=%d want 2", calls.Load())
	}
}
