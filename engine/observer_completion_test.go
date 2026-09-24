package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// Done is evaluated only after the wait has captured its cutoff and released
// its accounting lock. This synchronizes assertions without a scheduler delay.
type completionContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *completionContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}
func startCompletionWait(t *testing.T, wait func(context.Context) error) (<-chan error, context.CancelFunc) {
	t.Helper()
	base, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	ctx := &completionContext{Context: base, waiting: make(chan struct{})}
	finished := make(chan error, 1)
	go func() { finished <- wait(ctx) }()
	select {
	case <-ctx.waiting:
	case err := <-finished:
		t.Fatalf("completed before source/callback closure: %v", err)
	case <-base.Done():
		t.Fatal("wait did not capture cutoff")
	}
	assertCompletionBlocked(t, finished)
	return finished, cancel
}
func assertCompletionBlocked(t *testing.T, finished <-chan error) {
	t.Helper()
	select {
	case err := <-finished:
		t.Fatalf("completed before source/callback closure: %v", err)
	default:
	}
}
func requireCompletion(t *testing.T, finished <-chan error) {
	t.Helper()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("completion did not return")
	}
}
func TestObserverCompletionWaitsForDeferredEnqueue(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	g := certificationGateway(t, certificationValidatorFunc(func(ctx context.Context, _ []byte, _ string) (shnsdk.Result, error) {
		once.Do(func() { close(entered) })
		select {
		case <-release:
			return shnsdk.Result{Valid: true}, nil
		case <-ctx.Done():
			return shnsdk.Result{}, ctx.Err()
		}
	}), nil)
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	done := g.operations.begin()
	finished, _ := startCompletionWait(t, g.WaitObserverCompletion)
	request, cancel := context.WithCancel(context.Background())
	cancel()
	<-request.Done() // client cancellation precedes the server's deferred enqueue
	certificationSubmit(g, "canceled-request")
	done()
	<-entered
	assertCompletionBlocked(t, finished)
	releaseOnce.Do(func() { close(release) })
	requireCompletion(t, finished)
}
func TestObserverCompletionWaitsForCallback(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	g := certificationGateway(t, &shnsdk.FakeValidator{}, func(e ObserverEvent) {
		if e.Kind == ConformanceObservedEvent {
			select {
			case <-entered:
			default:
				close(entered)
			}
			<-release
		}
	})
	var once sync.Once
	defer once.Do(func() { close(release) })
	certificationSubmit(g, "callback")
	<-entered
	findings, _ := g.ConformanceObservationsForTest()
	if len(findings) == 0 {
		t.Fatal("verdict not stored before callback")
	}
	finished, _ := startCompletionWait(t, g.WaitObserverCompletion)
	once.Do(func() { close(release) })
	requireCompletion(t, finished)
}

func TestObserverCompletionReportsDefaultQueueLoss(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	g := &Gateway{cfg: Config{Observer: func(ObserverEvent) {
		enteredOnce.Do(func() { close(entered) })
		<-release
	}}}
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); _ = g.Close() })
	g.observe(ObserverEvent{Kind: "held"})
	<-entered
	for range observationNotificationCapacity + 1 {
		g.observe(ObserverEvent{Kind: "queued"})
	}
	d := &g.observerDispatch
	d.mu.Lock()
	queued, accepted, dropped := len(d.queue), d.accepted, d.dropped
	d.mu.Unlock()
	if queued != observationNotificationCapacity || accepted != observationNotificationCapacity+1 || dropped != 1 {
		t.Fatalf("default queue=%d accepted=%d dropped=%d", queued, accepted, dropped)
	}
	releaseOnce.Do(func() { close(release) })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := g.WaitObserverCompletion(ctx); err == nil || err.Error() != "observer delivery incomplete" {
		t.Fatalf("loss claimed complete delivery: %v", err)
	}
	d.mu.Lock()
	completed, panics := d.completed, d.panics
	d.mu.Unlock()
	if completed != accepted || panics != 0 {
		t.Fatalf("incomplete accepted work: accepted=%d completed=%d panics=%d", accepted, completed, panics)
	}
}

func TestObserverCompletionProvisionedProofQueue(t *testing.T) {
	const capacity = 384
	for _, tc := range []struct {
		name     string
		cfg      *Config
		capacity int
	}{
		{"missing configuration", nil, capacity},
		{"missing observer", &Config{}, capacity},
		{"zero capacity", &Config{Observer: func(ObserverEvent) {}}, 0},
		{"default capacity", &Config{Observer: func(ObserverEvent) {}}, observationNotificationCapacity},
		{"unbounded capacity", &Config{Observer: func(ObserverEvent) {}}, capacity + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := SetObserverQueueCapacityForTest(tc.cfg, tc.capacity); err == nil {
				t.Fatal("unsupported observer proof capacity accepted")
			}
		})
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	cfg := Config{Observer: func(ObserverEvent) {
		enteredOnce.Do(func() { close(entered) })
		<-release
	}}
	if err := SetObserverQueueCapacityForTest(&cfg, capacity); err != nil {
		t.Fatal(err)
	}
	g := &Gateway{cfg: cfg}
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); _ = g.Close() })
	g.observe(ObserverEvent{Kind: "held"})
	<-entered
	for range 333 {
		g.observe(ObserverEvent{Kind: "queued"})
	}
	d := &g.observerDispatch
	d.mu.Lock()
	queued, accepted, dropped := len(d.queue), d.accepted, d.dropped
	d.mu.Unlock()
	if queued != 333 || accepted != 334 || dropped != 0 {
		t.Fatalf("proof queue=%d accepted=%d dropped=%d", queued, accepted, dropped)
	}
	releaseOnce.Do(func() { close(release) })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := g.WaitObserverCompletion(ctx); err != nil {
		t.Fatalf("provisioned proof lost observation: %v", err)
	}
}
func TestObserverCompletionOperationCutoff(t *testing.T) {
	var g Gateway
	first, second := g.operations.begin(), g.operations.begin()
	finished, _ := startCompletionWait(t, g.WaitObserverCompletion)
	g.operations.mu.Lock()
	next, pending := g.operations.next, len(g.operations.pending)
	g.operations.mu.Unlock()
	if next != 2 || pending != 2 {
		t.Fatal("unexpected captured operation state")
	}
	third := g.operations.begin()
	defer third()
	second()
	assertCompletionBlocked(t, finished)
	first()
	first() // completion is idempotent
	requireCompletion(t, finished)
	g.operations.mu.Lock()
	defer g.operations.mu.Unlock()
	if len(g.operations.pending) != 1 {
		t.Fatal("later operation was removed")
	}
}
func TestObserverCompletionCancellation(t *testing.T) {
	var g Gateway
	done := g.operations.begin()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.WaitObserverCompletion(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled: %v", err)
	}
	finished, cancel := startCompletionWait(t, g.WaitObserverCompletion)
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-wait: %v", err)
	}
	g.operations.mu.Lock()
	pending := len(g.operations.pending)
	g.operations.mu.Unlock()
	if pending != 1 {
		t.Fatal("canceled waiter removed live ticket")
	}
	done()
	if err := g.WaitObserverCompletion(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestObserverCompletionHandlerPanicClosesTicket(t *testing.T) {
	g := &Gateway{cfg: Config{Role: "provider", Clock: time.Now, Observer: func(ObserverEvent) { panic("optional callback") }}}
	EnableIngressForTest(&g.cfg)
	defer g.Close()
	g.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/Claim/$submit", strings.NewReader(`{}`)))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := g.WaitObserverCompletion(ctx); err == nil {
		t.Fatal("panic claimed complete delivery")
	}
	g.operations.mu.Lock()
	defer g.operations.mu.Unlock()
	if len(g.operations.pending) != 0 {
		t.Fatal("orphan operation ticket")
	}
}
func TestObserverCompletionAcceptedNotices(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	g := certificationGateway(t, certificationValidatorFunc(func(ctx context.Context, _ []byte, _ string) (shnsdk.Result, error) {
		once.Do(func() { close(entered) })
		select {
		case <-release:
		case <-ctx.Done():
			return shnsdk.Result{}, ctx.Err()
		}
		return shnsdk.Result{Valid: true}, nil
	}), nil)
	certificationSubmit(g, "active")
	<-entered
	for i := 0; i < certificationQueueCapacity; i++ {
		certificationSubmit(g, fmt.Sprint(i))
	}
	certificationSubmit(g, "overflow")
	finished, _ := startCompletionWait(t, g.WaitObserverCompletion)
	close(release)
	if err := <-finished; err == nil {
		t.Fatal("dropped observation reported full coverage")
	}
	g.certification.mu.Lock()
	defer g.certification.mu.Unlock()
	if g.certification.accepted != 33 || g.certification.completed != 33 || g.certification.dropped != 1 {
		t.Fatalf("accepted/completed/dropped=%d/%d/%d", g.certification.accepted, g.certification.completed, g.certification.dropped)
	}
}
