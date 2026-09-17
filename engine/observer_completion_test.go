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
	g := certificationGateway(t, &shnsdk.FakeValidator{}, func(ObserverEvent) { close(entered); <-release })
	var once sync.Once
	defer once.Do(func() { close(release) })
	certificationSubmit(g, "callback")
	<-entered
	if len(g.CertificationEvidenceForTest()) != 1 {
		t.Fatal("verdict not stored before callback")
	}
	finished, _ := startCompletionWait(t, g.WaitObserverCompletion)
	once.Do(func() { close(release) })
	requireCompletion(t, finished)
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
	g := &Gateway{cfg: Config{Role: "provider", Clock: time.Now}}
	EnableIngressForTest(&g.cfg)
	g.cfg.Observer = func(ObserverEvent) {
		g.operations.mu.Lock()
		pending := len(g.operations.pending)
		g.operations.mu.Unlock()
		if pending != 1 {
			t.Errorf("handler has %d operation tickets", pending)
		}
		panic("intentional observer panic")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("handler did not panic")
			}
		}()
		g.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/Claim/$submit", strings.NewReader(`{}`)))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := g.WaitObserverCompletion(ctx); err != nil {
		t.Fatalf("orphan after panic: %v", err)
	}
}
func TestObserverCompletionAcceptedNotices(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	noticeEntered, noticeRelease := make(chan struct{}), make(chan struct{})
	var validationOnce, releaseOnce, noticeOnce sync.Once
	g := certificationGateway(t, certificationValidatorFunc(func(ctx context.Context, _ []byte, _ string) (shnsdk.Result, error) {
		validationOnce.Do(func() { close(entered) })
		select {
		case <-release:
			return shnsdk.Result{Valid: true}, nil
		case <-ctx.Done():
			return shnsdk.Result{}, ctx.Err()
		}
	}), func(e ObserverEvent) {
		if e.CorrelationID == "overflow" {
			close(noticeEntered)
			<-noticeRelease
		}
	})
	defer releaseOnce.Do(func() { close(release) })
	defer noticeOnce.Do(func() { close(noticeRelease) })
	certificationSubmit(g, "active")
	<-entered
	for i := 0; i < certificationQueueCapacity; i++ {
		certificationSubmit(g, fmt.Sprint(i))
	}
	certificationSubmit(g, "overflow")
	finished, _ := startCompletionWait(t, g.WaitObserverCompletion)
	releaseOnce.Do(func() { close(release) })
	<-noticeEntered
	assertCompletionBlocked(t, finished)
	noticeOnce.Do(func() { close(noticeRelease) })
	requireCompletion(t, finished)
	g.certification.mu.Lock()
	defer g.certification.mu.Unlock()
	if g.certification.accepted != 34 || g.certification.completed != 34 {
		t.Fatalf("accepted/completed = %d/%d", g.certification.accepted, g.certification.completed)
	}
}
