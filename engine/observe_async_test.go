package engine

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/diagnostics"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// blockingValidator holds every call until release is closed, then answers
// result: a validator lane that is slow or overloaded.
type blockingValidator struct {
	release chan struct{}
	result  shnsdk.Result
	mu      sync.Mutex
	calls   int
}

func newBlockingValidator(result shnsdk.Result) *blockingValidator {
	return &blockingValidator{release: make(chan struct{}), result: result}
}

func (b *blockingValidator) Validate(ctx context.Context, _ []byte, _ string) (shnsdk.Result, error) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	select {
	case <-b.release:
		return b.result, nil
	case <-ctx.Done():
		return shnsdk.Result{}, ctx.Err()
	}
}

func (b *blockingValidator) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

var invalidResult = shnsdk.Result{Valid: false, Issues: []string{"error: Coverage.status: minimum required = 1, but only found 0"}}

func observeGateway(t *testing.T, level ConformanceEnforcement, v shnsdk.Validator) (*Gateway, *[]ObserverEvent) {
	t.Helper()
	g, events, _ := findingGateway(t, v)
	g.cfg.ConformanceEnforcement = level
	var mu sync.Mutex
	g.cfg.Observer = func(e ObserverEvent) { mu.Lock(); *events = append(*events, e); mu.Unlock() }
	t.Cleanup(func() { _ = g.Close() })
	return g, events
}

func observedFindings(events *[]ObserverEvent, corr string) []string {
	var out []string
	for _, e := range *events {
		if e.Kind == ConformanceObservedEvent && strings.Contains(e.Detail, `"correlationId":"`+corr+`"`) {
			out = append(out, e.Detail)
		}
	}
	return out
}

func ingressCtx(corr string) context.Context {
	return withFindingContext(context.Background(), findingContext{
		LegType: "crd-order-select", CorrelationID: corr, Seam: "payer-inbound", Whose: "peer",
	})
}

const coverageJSON = `{"resourceType":"Coverage","id":"c1"}`

// within runs f and fails the test by name if it has not returned within d: a
// check that blocks on the validator where it must not would otherwise hang the
// package.
func within(t *testing.T, d time.Duration, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %s", what, d)
	}
}

// At observe a check that can only record runs off the request path: the leg
// does not wait on a slow validator, and the finding still lands once the
// validator answers.
func TestObserveCheckDoesNotWaitOnASlowValidator(t *testing.T) {
	v := newBlockingValidator(invalidResult)
	g, events := observeGateway(t, EnforcementObserve, v)

	var status int
	var msg string
	t.Cleanup(func() {
		select {
		case <-v.release:
		default:
			close(v.release)
		}
	})
	within(t, time.Second, "an observe check on a slow validator", func() {
		status, msg = g.validateFHIR(ingressCtx("corr-slow"), []byte(coverageJSON), "ingress", "")
	})
	if status != 0 {
		t.Fatalf("an observe check never refuses, got %d %q", status, msg)
	}
	if got := observedFindings(events, "corr-slow"); len(got) != 0 {
		t.Fatalf("no finding can exist before the validator answers, got %v", got)
	}

	close(v.release)
	g.drainObserveChecks()

	got := observedFindings(events, "corr-slow")
	if len(got) != 1 {
		t.Fatalf("the finding must land once the validator answers, got %d: %v", len(got), got)
	}
	for _, want := range []string{`"kind":"fhir-ingress"`, `"legType":"crd-order-select"`, `"seam":"payer-inbound"`, `"decision":"relayed"`, `"level":"observe"`} {
		if !strings.Contains(got[0], want) {
			t.Fatalf("finding missing %s: %s", want, got[0])
		}
	}
}

// A validator outage at observe is recorded as unavailable, off the request
// path, exactly as the synchronous check recorded it.
func TestObserveCheckRecordsAnOutageOffThePath(t *testing.T) {
	g, events := observeGateway(t, EnforcementObserve, failingValidator{})
	if status, msg := g.validateFHIR(ingressCtx("corr-down"), []byte(coverageJSON), "ingress", ""); status != 0 {
		t.Fatalf("an outage at observe is relayed, got %d %q", status, msg)
	}
	g.drainObserveChecks()
	got := observedFindings(events, "corr-down")
	if len(got) != 1 || !strings.Contains(got[0], `"verdict":"unavailable"`) {
		t.Fatalf("an outage at observe records one unavailable finding, got %v", got)
	}
}

// At strict and structural a check refuses, so it stays on the request path:
// the leg waits for the verdict and refuses on it.
func TestRefusingLevelsStillWaitForTheVerdict(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementStrict, EnforcementStructural} {
		t.Run(level.String(), func(t *testing.T) {
			v := newBlockingValidator(invalidResult)
			g, _ := observeGateway(t, level, v)
			done := make(chan int, 1)
			go func() {
				status, _ := g.validateFHIR(ingressCtx("corr-strict"), []byte(coverageJSON), "ingress", "")
				done <- status
			}()
			select {
			case status := <-done:
				t.Fatalf("a refusing level returned %d before the validator answered", status)
			case <-time.After(100 * time.Millisecond):
			}
			close(v.release)
			if status := <-done; status != http.StatusUnprocessableEntity {
				t.Fatalf("an invalid verdict at %s refuses 422, got %d", level, status)
			}
		})
	}
}

// A bridged payload is SHN's own edit and refuses at every level, observe
// included, so its check never leaves the request path.
func TestBridgedCheckStaysSynchronousAtObserve(t *testing.T) {
	v := newBlockingValidator(invalidResult)
	g, _ := observeGateway(t, EnforcementObserve, v)
	done := make(chan int, 1)
	go func() {
		status, _ := g.validateFHIREgressOrBridged(ingressCtx("corr-bridged"), []byte(coverageJSON), "", "", true)
		done <- status
	}()
	select {
	case status := <-done:
		t.Fatalf("a bridged check returned %d before the validator answered", status)
	case <-time.After(100 * time.Millisecond):
	}
	close(v.release)
	if status := <-done; status == 0 {
		t.Fatal("an invalid bridged payload refuses at observe")
	}
}

// The decision EOB a payer builds stays on the request path, because whether
// it is written depends on its verdict; at observe its wait is bounded by
// decisionEOBObserveDeadline, and a validator that misses it is unavailable:
// the answer is relayed and the decision written (as for any validator outage).
func TestDecisionEOBCheckIsBoundedAtObserve(t *testing.T) {
	saved := decisionEOBObserveDeadline
	decisionEOBObserveDeadline = 150 * time.Millisecond
	t.Cleanup(func() { decisionEOBObserveDeadline = saved })

	v := newBlockingValidator(invalidResult)
	t.Cleanup(func() { close(v.release) })
	g, events := observeGateway(t, EnforcementObserve, v)
	ctx := withFindingContext(context.Background(), findingContext{LegType: "pas-claim", CorrelationID: "corr-eob", Seam: "payer-native", Whose: "self"})

	var status int
	var msg string
	var invalid bool
	start := time.Now()
	within(t, 5*time.Second, "the decision-EOB check at observe", func() {
		status, msg, invalid = g.validateDecisionEOBs(ctx, [][]byte{[]byte(`{"resourceType":"ExplanationOfBenefit"}`)})
	})
	took := time.Since(start)
	if took < decisionEOBObserveDeadline || took > decisionEOBObserveDeadline+time.Second {
		t.Fatalf("the decision-EOB check waits for its deadline and no longer, took %s", took)
	}
	if status != 0 || invalid {
		t.Fatalf("a missed deadline is unavailable: relayed and written, got status %d %q invalid=%v", status, msg, invalid)
	}
	got := observedFindings(events, "corr-eob")
	if len(got) != 1 || !strings.Contains(got[0], `"verdict":"unavailable"`) {
		t.Fatalf("a missed deadline records one unavailable finding at once, got %v", got)
	}
}

// One budget covers all of a leg's decision EOBs at observe: an inquiry answer
// carrying hundreds of decisions, with the validator stuck, adds no more than the
// budget once, and every EOB past it is recorded unavailable, never skipped.
func TestDecisionEOBBudgetIsPerLegAtObserve(t *testing.T) {
	saved := decisionEOBObserveDeadline
	decisionEOBObserveDeadline = 150 * time.Millisecond
	t.Cleanup(func() { decisionEOBObserveDeadline = saved })
	v := newBlockingValidator(invalidResult)
	t.Cleanup(func() { close(v.release) })
	g, events := observeGateway(t, EnforcementObserve, v)
	ctx := withFindingContext(context.Background(), findingContext{LegType: "pas-claim-inquire", CorrelationID: "corr-many", Seam: "payer-native", Whose: "self"})
	eobs := make([][]byte, 300)
	for i := range eobs {
		eobs[i] = []byte(`{"resourceType":"ExplanationOfBenefit"}`)
	}
	var status int
	var invalid bool
	start := time.Now()
	within(t, 5*time.Second, "300 decision-EOB checks at observe", func() { status, _, invalid = g.validateDecisionEOBs(ctx, eobs) })
	if took := time.Since(start); took > decisionEOBObserveDeadline+time.Second {
		t.Fatalf("300 decision EOBs took %s; the leg's budget is %s once", took, decisionEOBObserveDeadline)
	}
	if status != 0 || invalid {
		t.Fatalf("decisions past the budget are relayed and written, got status %d invalid=%v", status, invalid)
	}
	got := observedFindings(events, "corr-many")
	if len(got) != len(eobs) {
		t.Fatalf("recorded %d findings, want one per decision EOB (%d)", len(got), len(eobs))
	}
	for _, f := range got {
		if !strings.Contains(f, `"verdict":"unavailable"`) {
			t.Fatalf("a decision past the budget is recorded unavailable, got %s", f)
		}
	}
}

// The budget is observe's alone: at strict and structural the decision EOB still
// waits for the validator and refuses on an invalid verdict, however long it takes.
func TestDecisionEOBIsUnboundedWhereItRefuses(t *testing.T) {
	saved := decisionEOBObserveDeadline
	decisionEOBObserveDeadline = 50 * time.Millisecond
	t.Cleanup(func() { decisionEOBObserveDeadline = saved })
	for _, level := range []ConformanceEnforcement{EnforcementStrict, EnforcementStructural} {
		t.Run(level.String(), func(t *testing.T) {
			// It honours its context, as the real client does, so a deadline
			// applied here would cut it short.
			slow := ctxValidator(func(ctx context.Context) (shnsdk.Result, error) {
				select {
				case <-time.After(250 * time.Millisecond):
					return shnsdk.Result{Valid: false, Issues: []string{"error: ExplanationOfBenefit.status: minimum required = 1, but only found 0"}}, nil
				case <-ctx.Done():
					return shnsdk.Result{}, ctx.Err()
				}
			})
			g, _ := observeGateway(t, level, slow)
			ctx := withFindingContext(context.Background(), findingContext{LegType: "pas-claim", CorrelationID: "corr-strict-eob", Seam: "payer-native", Whose: "self"})
			status, _, _ := g.validateDecisionEOBs(ctx, [][]byte{[]byte(`{"resourceType":"ExplanationOfBenefit"}`)})
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("at %s a slow validator's invalid verdict refuses 422, not a deadline's unavailable; got %d", level, status)
			}
		})
	}
}

// The observer completion barrier waits for the checks an operation queued: their
// events belong to it even though the operation itself has ended.
func TestObserverCompletionWaitsForQueuedChecks(t *testing.T) {
	v := newBlockingValidator(invalidResult)
	released := false
	t.Cleanup(func() {
		if !released {
			close(v.release)
		}
	})
	g, _ := observeGateway(t, EnforcementObserve, v)
	g.validateFHIR(ingressCtx("corr-barrier"), []byte(coverageJSON), "ingress", "")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := g.WaitObserverCompletion(ctx); err == nil {
		t.Fatal("the barrier returned while a queued check was still running")
	}
	close(v.release)
	released = true
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if err := g.WaitObserverCompletion(ctx2); err != nil {
		t.Fatalf("the barrier must return once the queued check has run: %v", err)
	}
}

// At observe an invalid decision EOB the validator does judge in time is still
// recorded and not written (the decision-EOB rule), synchronously.
func TestDecisionEOBInvalidInTimeIsNotWrittenAtObserve(t *testing.T) {
	g, events := observeGateway(t, EnforcementObserve, &shnsdk.FakeValidator{RejectIfContains: "ExplanationOfBenefit"})
	ctx := withFindingContext(context.Background(), findingContext{LegType: "pas-claim", CorrelationID: "corr-eob2", Seam: "payer-native", Whose: "self"})
	status, _, recorded := g.validateFHIRDecisionEOB(ctx, []byte(`{"resourceType":"ExplanationOfBenefit"}`))
	if status != 0 || !recorded {
		t.Fatalf("an invalid decision EOB at observe is relayed and marked recorded (not written), got status %d recorded=%v", status, recorded)
	}
	if got := observedFindings(events, "corr-eob2"); len(got) != 1 {
		t.Fatalf("its finding is emitted before the answer, got %v", got)
	}
}

// A full queue drops the newest check and counts it; it never blocks the leg
// and never refuses.
func TestObserveCheckQueueDropsAndCountsWhenFull(t *testing.T) {
	v := newBlockingValidator(invalidResult)
	g, _ := observeGateway(t, EnforcementObserve, v)
	t.Cleanup(func() { close(v.release) }) // runs before the gateway's Close
	total := observeCheckQueueCapacity + 5
	start := time.Now()
	for i := 0; i < total; i++ {
		if status, _ := g.validateFHIR(ingressCtx("corr-full"), []byte(coverageJSON), "ingress", ""); status != 0 {
			t.Fatalf("a full queue never refuses, got %d", status)
		}
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("enqueueing %d checks took %s; a full queue must not block", total, took)
	}
	// Up to observeCheckWorkers checks are held by the workers, the queue holds its
	// capacity, and the rest drop.
	if dropped := g.observeChecksDropped(); dropped < uint64(5-observeCheckWorkers) || dropped > 5 {
		t.Fatalf("dropped = %d, want the overflow counted (%d to 5)", dropped, 5-observeCheckWorkers)
	}
}

// Close does not wait forever on a stuck validator: past observeFlushTimeout it
// cancels the calls still running, and each is recorded as unavailable.
func TestCloseIsBoundedByAStuckValidator(t *testing.T) {
	saved := observeFlushTimeout
	observeFlushTimeout = 100 * time.Millisecond
	t.Cleanup(func() { observeFlushTimeout = saved })
	v := newBlockingValidator(invalidResult)
	g, events := observeGateway(t, EnforcementObserve, v)
	g.validateFHIR(ingressCtx("corr-stuck"), []byte(coverageJSON), "ingress", "")
	within(t, time.Second, "Close on a stuck validator", func() { _ = g.Close() })
	got := observedFindings(events, "corr-stuck")
	if len(got) != 1 || !strings.Contains(got[0], `"verdict":"unavailable"`) {
		t.Fatalf("a check cancelled at Close is recorded as unavailable, got %v", got)
	}
}

// Close waits up to 10 seconds for queued checks, the bound the gateway's
// documentation promises: a shorter one turns the findings of a slow shutdown
// into "unavailable".
func TestCloseWaitsTheDocumentedFlushBound(t *testing.T) {
	if observeFlushTimeout != 10*time.Second {
		t.Fatalf("observeFlushTimeout = %s, documented as 10 seconds", observeFlushTimeout)
	}
}

// Close flushes the queued checks, so a gateway shutting down still records
// what its legs asked to be checked.
func TestCloseFlushesQueuedObserveChecks(t *testing.T) {
	v := newBlockingValidator(invalidResult)
	g, events := observeGateway(t, EnforcementObserve, v)
	for i := 0; i < 3; i++ {
		g.validateFHIR(ingressCtx("corr-close"), []byte(coverageJSON), "ingress", "")
	}
	// Close begins while the validator still holds the checks; it must wait for
	// their verdicts, not cancel them into "unavailable".
	closed := make(chan struct{})
	go func() { _ = g.Close(); close(closed) }()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		q := g.observeChecks.Load()
		q.mu.Lock()
		closing := q.closed
		q.mu.Unlock()
		if closing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Close never closed the queue")
		}
	}
	close(v.release)
	within(t, 5*time.Second, "Close after the validator answered", func() { <-closed })
	got := observedFindings(events, "corr-close")
	if len(got) != 3 {
		t.Fatalf("Close must flush every queued check, got %d findings", len(got))
	}
	for _, f := range got {
		if strings.Contains(f, `"verdict":"unavailable"`) || !strings.Contains(f, `"issues"`) {
			t.Fatalf("a flushed check must carry the validator's verdict, got %s", f)
		}
	}
}

// At none nothing runs: no validator call, no queue, no finding.
func TestNoneRunsNoObserveCheck(t *testing.T) {
	v := newBlockingValidator(invalidResult)
	close(v.release)
	g, events := observeGateway(t, EnforcementNone, v)
	g.validateFHIR(ingressCtx("corr-none"), []byte(coverageJSON), "ingress", "")
	g.drainObserveChecks()
	if v.callCount() != 0 || len(observedFindings(events, "corr-none")) != 0 {
		t.Fatalf("none calls no validator and records nothing, got %d calls", v.callCount())
	}
}

// CRD's embedded-resource validation only records, so at observe the answer
// does not wait for it either: with the validator held, the leg answers at
// once, and the embedded resource's outcome is recorded when it answers.
func TestObserveCRDEmbeddedValidationDoesNotHoldTheAnswer(t *testing.T) {
	const covInfo = "ext-coverage-information"
	g, requester := newInboundTestGateway(t, true)
	g.cfg.ConformanceEnforcement = EnforcementObserve
	p := newCDSPayer(t, referencePayerServices...)
	g.cfg.Responder = NewNativeResponder(p.srv.Client(), p.srv.URL, "", nil, nil)
	release := make(chan struct{})
	g.cfg.Validator = validatorFunc(func(b []byte) (shnsdk.Result, error) {
		if bytes.Contains(b, []byte(covInfo)) {
			<-release
		}
		return shnsdk.Result{Valid: true}, nil
	})
	var mu sync.Mutex
	var events []ObserverEvent
	g.cfg.Observer = func(e ObserverEvent) { mu.Lock(); events = append(events, e); mu.Unlock() }
	t.Cleanup(func() { _ = g.Close() })
	pci, _, _ := g.cfg.SoR.ResolvePatient("MBR-COVERED")
	env := shnsdk.Envelope{}
	env.Metadata.CorrelationID, env.Metadata.Sender = "corr-emb", requester.ID
	rec := httptest.NewRecorder()
	req := newSignedInboundRequest(t, g, requester.ID)
	done := make(chan struct{})
	go func() {
		defer close(done)
		embeddedValidationTails[0].handle(g, rec, req, env, shnsdk.Token{Subject: pci})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("the answer waited for the embedded validation")
	}
	if rec.Code != http.StatusOK {
		close(release)
		t.Fatalf("status %d", rec.Code)
	}
	embeddedEvents := func() int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, e := range events {
			if e.Kind == CRDEmbeddedValidatedEvent && e.CorrelationID == "corr-emb" {
				n++
			}
		}
		return n
	}
	if n := embeddedEvents(); n != 0 {
		close(release)
		t.Fatalf("an embedded outcome was recorded before the validator answered: %d", n)
	}
	close(release)
	g.drainObserveChecks()
	if n := embeddedEvents(); n != 1 {
		t.Fatalf("recorded %d embedded outcomes once the validator answered, want 1", n)
	}
}

// ctxValidator answers from f, which sees the call's context.
type ctxValidator func(context.Context) (shnsdk.Result, error)

func (f ctxValidator) Validate(ctx context.Context, _ []byte, _ string) (shnsdk.Result, error) {
	return f(ctx)
}

// findingsTo sends the correlation id of every finding g records to the
// returned channel, in the order recorded.
func findingsTo(g *Gateway) <-chan string {
	ch := make(chan string, 16)
	g.cfg.Observer = func(e ObserverEvent) {
		if e.Kind != ConformanceObservedEvent {
			return
		}
		for _, corr := range []string{"a", "b", "c"} {
			if strings.Contains(e.Detail, `"correlationId":"`+corr+`"`) {
				ch <- corr
			}
		}
	}
	return ch
}

// The barrier waits for every check queued before it, even when the workers
// finish out of order: a later check finishing first does not stand in for an
// earlier one still running.
func TestObserverCompletionWaitsForEarlierChecksOutOfOrder(t *testing.T) {
	release := make(chan struct{})
	released := false
	g, _ := observeGateway(t, EnforcementObserve, validatorFunc(func(b []byte) (shnsdk.Result, error) {
		if bytes.Contains(b, []byte("slow")) {
			<-release
		}
		return shnsdk.Result{Valid: false, Issues: []string{"error: synthetic"}}, nil
	}))
	t.Cleanup(func() { // runs before the gateway's Close
		if !released {
			close(release)
		}
	})
	found := findingsTo(g)
	await := func(want string) {
		t.Helper()
		select {
		case got := <-found:
			if got != want {
				t.Fatalf("finding for %s recorded, want %s", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no finding for %s", want)
		}
	}
	g.validateFHIR(ingressCtx("a"), []byte(`{"resourceType":"Coverage","id":"slow"}`), "ingress", "")
	g.validateFHIR(ingressCtx("b"), []byte(coverageJSON), "ingress", "")
	await("b")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	barrier := make(chan error, 1)
	go func() { barrier <- g.WaitObserverCompletion(ctx) }()
	// Wait until the barrier has taken its cutoff, then queue a fast check
	// after it and let that finish while a is still held.
	for {
		q := g.observeChecks.Load()
		q.mu.Lock()
		waiting := len(q.pending) == 1
		q.mu.Unlock()
		if waiting {
			break
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	g.validateFHIR(ingressCtx("c"), []byte(coverageJSON), "ingress", "")
	await("c")
	select {
	case err := <-barrier:
		t.Fatalf("the barrier returned (%v) while a check queued before it was still running", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	released = true
	await("a")
	if err := <-barrier; err != nil {
		t.Fatalf("the barrier must return once the earlier check has run: %v", err)
	}
}

// A check dropped by a full queue leaves its leg's result incomplete: the
// result never reads as a clean count for a leg whose check was not run.
func TestADroppedCheckMakesItsLegsResultIncomplete(t *testing.T) {
	v := newBlockingValidator(invalidResult)
	g, _ := observeGateway(t, EnforcementObserve, v)
	t.Cleanup(func() { close(v.release) }) // runs before the gateway's Close
	sink := &captureSink{}
	g.cfg.Diagnostic = sink.emit
	// Hold every worker, then fill the queue, so the next check is dropped.
	for i := 0; i < observeCheckWorkers; i++ {
		g.validateFHIR(ingressCtx("corr-fill"), []byte(coverageJSON), "ingress", "")
	}
	for deadline := time.Now().Add(5 * time.Second); v.callCount() < observeCheckWorkers; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d workers took a check, want %d", v.callCount(), observeCheckWorkers)
		}
	}
	for i := 0; i < observeCheckQueueCapacity; i++ {
		g.validateFHIR(ingressCtx("corr-fill"), []byte(coverageJSON), "ingress", "")
	}
	tl := &legTally{}
	g.validateFHIR(withLegTally(ingressCtx("leg-dropped"), tl), []byte(coverageJSON), "ingress", "")
	if g.observeChecksDropped() != 1 {
		t.Fatalf("dropped = %d, want the one check past a full queue", g.observeChecksDropped())
	}
	tl.end(g, ExchangeRecord{CorrelationID: "leg-dropped"})
	r := sink.kind(diagnostics.KindConformanceResult)
	if len(r) != 1 {
		t.Fatalf("want one result for the leg, got %d", len(r))
	}
	if n, _, incomplete := resultDetail(t, r[0]); n != 0 || !incomplete {
		t.Fatalf("result = %d incomplete %v, want 0 and incomplete", n, incomplete)
	}
}

// A panic in a check (a validator or an Observer callback) ends that check
// only: the gateway and its workers keep running, and the leg's result says
// incomplete.
func TestAPanickingCheckEndsOnlyItself(t *testing.T) {
	g, events := observeGateway(t, EnforcementObserve, validatorFunc(func(b []byte) (shnsdk.Result, error) {
		if bytes.Contains(b, []byte("boom")) {
			panic("synthetic validator failure")
		}
		return invalidResult, nil
	}))
	sink := &captureSink{}
	g.cfg.Diagnostic = sink.emit
	tl := &legTally{}
	for i := 0; i < 2*observeCheckWorkers; i++ {
		g.validateFHIR(withLegTally(ingressCtx("leg-boom"), tl), []byte(`{"resourceType":"Coverage","id":"boom"}`), "ingress", "")
	}
	g.validateFHIR(ingressCtx("corr-after"), []byte(coverageJSON), "ingress", "")
	within(t, 5*time.Second, "the checks after a panicking one", g.drainObserveChecks)
	if got := observedFindings(events, "corr-after"); len(got) != 1 {
		t.Fatalf("a check after the panics recorded %d findings, want 1", len(got))
	}
	tl.end(g, ExchangeRecord{CorrelationID: "leg-boom"})
	r := sink.kind(diagnostics.KindConformanceResult)
	if len(r) != 1 {
		t.Fatalf("want one result for the leg, got %d", len(r))
	}
	if _, _, incomplete := resultDetail(t, r[0]); !incomplete {
		t.Fatal("a leg whose checks failed while running must read incomplete")
	}
}

// A check that arrives at a gateway closed before its first check is dropped
// and counted, never run.
func TestAGatewayClosedBeforeItsFirstCheckDropsIt(t *testing.T) {
	v := newBlockingValidator(invalidResult)
	close(v.release)
	g, events := observeGateway(t, EnforcementObserve, v)
	within(t, time.Second, "Close of an unused gateway", func() { _ = g.Close() })
	if status, _ := g.validateFHIR(ingressCtx("corr-late"), []byte(coverageJSON), "ingress", ""); status != 0 {
		t.Fatalf("a check after Close never refuses, got %d", status)
	}
	g.drainObserveChecks()
	if g.observeChecksDropped() != 1 || v.callCount() != 0 || len(observedFindings(events, "corr-late")) != 0 {
		t.Fatalf("dropped %d, validator calls %d: a check after Close is dropped, never run", g.observeChecksDropped(), v.callCount())
	}
}
