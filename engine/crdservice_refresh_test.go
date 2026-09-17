package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// listingPayer is a payer listing whose reads can be held open and whose
// answer can be broken.
type listingPayer struct {
	srv *httptest.Server

	mu     sync.Mutex
	reads  int
	broken bool
	hold   chan struct{} // non-nil: a listing read waits for it to close
	held   chan struct{} // receives once per listing read that is waiting
}

func newListingPayer(t *testing.T) *listingPayer {
	t.Helper()
	p := &listingPayer{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(realCRDAnswer(t))
			return
		}
		p.mu.Lock()
		p.reads++
		hold, held, broken := p.hold, p.held, p.broken
		p.mu.Unlock()
		if hold != nil {
			held <- struct{}{}
			<-hold
		}
		if broken {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"services": referencePayerServices})
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *listingPayer) set(broken bool, hold bool) (release func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.broken = broken
	p.hold, p.held = nil, nil
	if !hold {
		return func() {}
	}
	h := make(chan struct{})
	p.hold, p.held = h, make(chan struct{}, 16)
	var once sync.Once
	return func() { once.Do(func() { close(h) }) }
}

func (p *listingPayer) readCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reads
}

// testClock is a clock tests move forward while requests run.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func signRequest(t *testing.T, n *nativeResponder) LegResult {
	t.Helper()
	res, err := n.Handle(context.Background(), "crd-order-select", "c", "pci", cdsRequest("order-sign"))
	if err != nil {
		t.Error(err)
	}
	return res
}

// TestCDSListing_ConcurrentRequestsReadOnce: requests that find no usable
// listing while a read is in progress wait for that read; the listing is read
// once and every request is served.
func TestCDSListing_ConcurrentRequestsReadOnce(t *testing.T) {
	p := newListingPayer(t)
	release := p.set(false, true)
	defer release()
	clock := &testClock{now: time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)}
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, "", nil, clock.Now)
	const callers = 6
	joined := make(chan struct{}, callers)
	n.cds.mu.Lock()
	n.cds.joined = func() { joined <- struct{}{} }
	n.cds.mu.Unlock()

	results := make(chan LegResult, callers)
	for range callers {
		go func() { results <- signRequest(t, n) }()
	}
	<-p.held // the first read is open
	for range callers - 1 {
		select {
		case <-joined:
		case <-time.After(10 * time.Second):
			t.Fatal("the other requests did not wait for the read in progress")
		}
	}
	release()
	for range callers {
		if res := <-results; res.Status != 0 {
			t.Fatalf("request refused: %+v", res)
		}
	}
	if got := p.readCount(); got != 1 {
		t.Fatalf("listing read %d times, want 1", got)
	}
}

// TestCDSListing_RefreshDoesNotHoldRequests: while an expired listing is read
// again, requests are served from the listing already held instead of waiting.
func TestCDSListing_RefreshDoesNotHoldRequests(t *testing.T) {
	p := newListingPayer(t)
	clock := &testClock{now: time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)}
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, "", nil, clock.Now)
	if res := signRequest(t, n); res.Status != 0 {
		t.Fatalf("first request: %+v", res)
	}
	clock.Add(cdsServiceListingTTL)
	release := p.set(false, true)
	defer release()
	first := make(chan LegResult, 1)
	go func() { first <- signRequest(t, n) }()
	<-p.held
	done := make(chan LegResult, 1)
	go func() { done <- signRequest(t, n) }()
	select {
	case res := <-done:
		if res.Status != 0 {
			t.Fatalf("request during the refresh: %+v", res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a request waited for the listing refresh although a listing was held")
	}
	release()
	if res := <-first; res.Status != 0 {
		t.Fatalf("refreshing request: %+v", res)
	}
	if got := p.readCount(); got != 2 {
		t.Fatalf("listing read %d times, want 2", got)
	}
}

// TestCDSListing_RefreshFailureKeepsLastGoodListing: a listing that cannot be
// read again is replaced by nothing — requests are served from the last
// listing read, until that listing is older than cdsServiceListingMaxAge;
// after that they are refused (502). A failed refresh is remembered for the
// retry window, so it is not read again on every request.
func TestCDSListing_RefreshFailureKeepsLastGoodListing(t *testing.T) {
	p := newListingPayer(t)
	clock := &testClock{now: time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)}
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, "", nil, clock.Now)
	if res := signRequest(t, n); res.Status != 0 {
		t.Fatalf("first request: %+v", res)
	}
	p.set(true, false)
	clock.Add(cdsServiceListingTTL)
	if res := signRequest(t, n); res.Status != 0 || p.readCount() != 2 {
		t.Fatalf("refresh failure: %+v after %d reads, want the last listing used after one more read", res, p.readCount())
	}
	if res := signRequest(t, n); res.Status != 0 || p.readCount() != 2 {
		t.Fatalf("within the retry window: %+v after %d reads, want the last listing without another read", res, p.readCount())
	}
	clock.Add(cdsServiceListingRetryAfter)
	if res := signRequest(t, n); res.Status != 0 || p.readCount() != 3 {
		t.Fatalf("after the retry window: %+v after %d reads, want another read and the last listing used", res, p.readCount())
	}

	t.Run("beyond the maximum age the request is refused", func(t *testing.T) {
		clock.Add(cdsServiceListingMaxAge - cdsServiceListingTTL - cdsServiceListingRetryAfter)
		res := signRequest(t, n)
		if res.Status != http.StatusBadGateway || res.Message != "payer CDS service listing unavailable" {
			t.Fatalf("listing read %s ago: %+v, want 502", cdsServiceListingMaxAge, res)
		}
		if p.readCount() != 4 {
			t.Fatalf("listing read %d times, want 4", p.readCount())
		}
		// Within the retry window the failed read is not repeated, and the
		// over-age listing is still not used.
		res = signRequest(t, n)
		if res.Status != http.StatusBadGateway || res.Message != "payer CDS service listing unavailable" || p.readCount() != 4 {
			t.Fatalf("again within the retry window: %+v after %d reads, want 502 without another read", res, p.readCount())
		}
	})
	t.Run("a later successful read is used", func(t *testing.T) {
		p.set(false, false)
		clock.Add(cdsServiceListingRetryAfter)
		if res := signRequest(t, n); res.Status != 0 || p.readCount() != 5 {
			t.Fatalf("%+v after %d reads", res, p.readCount())
		}
	})
}

// TestCDSListing_OverAgeListingWaitsForTheRead: a listing older than
// cdsServiceListingMaxAge is not used while another request's read is in
// progress: the request waits for that read.
func TestCDSListing_OverAgeListingWaitsForTheRead(t *testing.T) {
	p := newListingPayer(t)
	clock := &testClock{now: time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)}
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, "", nil, clock.Now)
	if res := signRequest(t, n); res.Status != 0 {
		t.Fatalf("first request: %+v", res)
	}
	clock.Add(cdsServiceListingMaxAge)
	release := p.set(false, true)
	defer release()
	joined := make(chan struct{}, 1)
	n.cds.mu.Lock()
	n.cds.joined = func() { joined <- struct{}{} }
	n.cds.mu.Unlock()
	first := make(chan LegResult, 1)
	go func() { first <- signRequest(t, n) }()
	<-p.held // the read is open
	second := make(chan LegResult, 1)
	go func() { second <- signRequest(t, n) }()
	select {
	case <-joined:
	case res := <-second:
		t.Fatalf("a request used the over-age listing instead of waiting: %+v", res)
	case <-time.After(10 * time.Second):
		t.Fatal("the request neither waited nor answered")
	}
	release()
	for _, c := range []chan LegResult{first, second} {
		if res := <-c; res.Status != 0 {
			t.Fatalf("%+v", res)
		}
	}
	if got := p.readCount(); got != 2 {
		t.Fatalf("listing read %d times, want 2", got)
	}
}

// TestCDSListing_WaiterLeavesWhenItsRequestEnds: a request waiting for a
// read in progress stops waiting when its own context ends; the read goes on
// for the others.
func TestCDSListing_WaiterLeavesWhenItsRequestEnds(t *testing.T) {
	p := newListingPayer(t)
	release := p.set(false, true)
	defer release()
	clock := &testClock{now: time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)}
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, "", nil, clock.Now)
	joined := make(chan struct{}, 1)
	n.cds.mu.Lock()
	n.cds.joined = func() { joined <- struct{}{} }
	n.cds.mu.Unlock()
	first := make(chan LegResult, 1)
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	go func() {
		res, _ := n.Handle(firstCtx, "crd-order-select", "c", "pci", cdsRequest("order-sign"))
		first <- res
	}()
	<-p.held
	ctx, cancel := context.WithCancel(context.Background())
	waiter := make(chan LegResult, 1)
	go func() {
		res, _ := n.Handle(ctx, "crd-order-select", "c", "pci", cdsRequest("order-sign"))
		waiter <- res
	}()
	<-joined
	cancel()
	cancelFirst() // the request that started the read ends too; the read is not cut short
	if res := <-waiter; res.Status != http.StatusBadGateway {
		t.Fatalf("waiter: %+v, want 502", res)
	}
	release()
	<-first
	if res := signRequest(t, n); res.Status != 0 || p.readCount() != 1 {
		t.Fatalf("%+v after %d reads, want the listing read by the first request kept", res, p.readCount())
	}
}
