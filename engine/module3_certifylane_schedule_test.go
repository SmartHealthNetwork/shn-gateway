//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"errors"
	"log"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/internal/lanequalify"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// recordedWaits replaces a client's clock: each wait the loop asks for is sent on
// waits, and the wait ends at once.
func recordedWaits(v *GatedCertificationValidator) <-chan time.Duration {
	waits := make(chan time.Duration, 64)
	v.after = func(d time.Duration) <-chan time.Time {
		select {
		case waits <- d:
		default:
		}
		c := make(chan time.Time, 1)
		c <- time.Time{}
		return c
	}
	return waits
}

func nextWait(t *testing.T, waits <-chan time.Duration) time.Duration {
	t.Helper()
	select {
	case d := <-waits:
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("the client asked for no further wait")
		return 0
	}
}

func failingQualifier(calls *int32) LaneQualifier {
	return func(context.Context, string, string) error {
		atomic.AddInt32(calls, 1)
		return errors.New("qualification corpus did not pass")
	}
}

// Clients a deploy starts together start apart and retry apart: each first
// attempt waits the delay plus a random part of the retry base, and each retry
// waits on the shared backoff (15 s doubling to 5 min, drawn from the upper half
// of its step), so eight clients failing in lockstep do not retry in lockstep.
func TestCertificationClientsStartApartAndBackOff(t *testing.T) {
	const clients = 8
	firsts := map[time.Duration]bool{}
	seconds := map[time.Duration]bool{}
	for i := range clients {
		lane := NewDiscoveredLane("2.2", "http://lane.test/fhir", nil)
		var calls int32
		v := newGatedCertification(lane, NewCertificationOperationValidator(lane.Base()), failingQualifier(&calls), "not configured")
		v.backoff = DefaultLaneBackoff(rand.New(rand.NewPCG(uint64(i+1), uint64(i+1))).Float64)
		waits := recordedWaits(v)
		v.Start()
		first := nextWait(t, waits)
		if first < certificationRequalifyDelay || first >= certificationRequalifyDelay+laneRetryBase {
			t.Fatalf("client %d starts after %v, want the delay plus less than one base", i, first)
		}
		firsts[first] = true
		steps := []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
		for k, step := range steps {
			got := nextWait(t, waits)
			if got < step/2 || got > step {
				t.Fatalf("client %d, retry %d waits %v, want within [%v, %v]", i, k+1, got, step/2, step)
			}
			if k == 0 {
				seconds[got] = true
			}
		}
		v.Close()
		if got := atomic.LoadInt32(&calls); got < int32(len(steps)) {
			t.Fatalf("client %d made %d attempts over %d waits", i, got, len(steps))
		}
	}
	if len(firsts) < clients || len(seconds) < clients {
		t.Fatalf("%d clients started at %d distinct waits and retried at %d: they still move together", clients, len(firsts), len(seconds))
	}
}

// A client configured by address has no routing lane to defer to: it starts at
// once and backs off on its own.
func TestQualifiedCertificationBacksOffOnItsOwn(t *testing.T) {
	var calls int32
	v := NewQualifiedCertificationValidator("2.2", NewCertificationOperationValidator("http://lane.test/fhir"), failingQualifier(&calls))
	v.backoff = LaneBackoff{Base: 15 * time.Second, Max: 5 * time.Minute} // whole steps
	waits := recordedWaits(v)
	v.Start()
	defer v.Close()
	if got := nextWait(t, waits); got != 0 {
		t.Fatalf("an address's client first waits %v, want 0", got)
	}
	for _, want := range []time.Duration{15 * time.Second, 30 * time.Second, time.Minute} {
		if got := nextWait(t, waits); got != want {
			t.Fatalf("retry waits %v, want %v", got, want)
		}
	}
}

// While routing's lane is retrying its own qualification, a default lane's
// client posts no corpus of its own; once routing's lane passes the same
// qualification, the client certifies, having run none.
func TestGatedCertificationDefersToRoutingWhileItRetries(t *testing.T) {
	upstream, _ := gatedTestUpstream(t, "2.2", new(int32))
	lane := NewDiscoveredLane("2.2", upstream.URL+"/fhir", shnsdk.NewOperationValidator(upstream.URL+"/fhir"))
	release := make(chan struct{})
	var calls int32
	q := func(ctx context.Context, _, _ string) error {
		atomic.AddInt32(&calls, 1)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	routed := make(chan error, 1)
	go func() { routed <- lane.KeepQualifying(ctx, q, LaneSchedule{}) }()
	for atomic.LoadInt32(&calls) == 0 {
		time.Sleep(time.Millisecond)
	}
	v := newGatedCertification(lane, NewCertificationOperationValidator(lane.Base()), q, "not configured")
	waits := recordedWaits(v)
	v.Start()
	defer v.Close()
	for range 5 {
		nextWait(t, waits) // the loop wakes, sees routing retrying, and waits again
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("%d qualifications ran while routing retried, want routing's one", got)
	}
	if _, err := v.Validate(context.Background(), literalClaim, "profile"); err == nil {
		t.Fatal("the client certified before any qualification passed")
	}
	close(release)
	if err := <-routed; err != nil {
		t.Fatal(err)
	}
	if !v.Ready() {
		t.Fatal("routing's identical pass did not qualify the client")
	}
	loopStopped(t, v)
	v.mu.Lock()
	own := v.ready
	v.mu.Unlock()
	if got := atomic.LoadInt32(&calls); got != 1 || own {
		t.Fatalf("%d qualifications ran, own ready %v: routing's pass is the client's, recorded as routing's", got, own)
	}
}

// Routing's pass is the client's own only when it ran the same qualification:
// on the endpoint the client dials, for its line, with its qualifier, within its
// budget. Each difference sends the client to its own corpus, and until that
// passes the client certifies nothing and dials nothing for the exchange.
func TestGatedCertificationTakesRoutingsPassOnlyWhenItIsTheSame(t *testing.T) {
	var hits int32
	upstream, _ := gatedTestUpstream(t, "2.2", &hits)
	base := upstream.URL + "/fhir"
	for _, row := range []struct {
		name         string
		otherRouting bool // routing ran a qualifier of its own
		slow         bool // routing's pass outlasted the budget the row sets
		setup        func(v *GatedCertificationValidator)
		takes        bool
	}{
		{name: "the same qualification", takes: true},
		{name: "another qualifier", otherRouting: true},
		{name: "a pass slower than the client's budget", slow: true, setup: func(v *GatedCertificationValidator) { v.budget = time.Millisecond }},
		{name: "another endpoint", setup: func(v *GatedCertificationValidator) { v.client = NewCertificationOperationValidator(base + "/other") }},
		{name: "another line", setup: func(v *GatedCertificationValidator) { v.line = "2.1" }},
	} {
		t.Run(row.name, func(t *testing.T) {
			atomic.StoreInt32(&hits, 0)
			var own, routed int32
			// One qualifier for routing and the client: routing's call passes
			// (slowly when the row says so); the client's own calls fail.
			shared := LaneQualifier(func(ctx context.Context, b, l string) error {
				if atomic.CompareAndSwapInt32(&routed, 0, 1) {
					if row.slow {
						time.Sleep(5 * time.Millisecond)
					}
					return nil
				}
				atomic.AddInt32(&own, 1)
				return errors.New("own corpus")
			})
			routing := shared
			if row.otherRouting {
				routing = func(context.Context, string, string) error { return nil }
				atomic.StoreInt32(&routed, 1)
			}
			lane := NewDiscoveredLane("2.2", base, shnsdk.NewOperationValidator(base))
			if err := lane.Qualify(context.Background(), routing); err != nil {
				t.Fatal(err)
			}
			v := newGatedCertification(lane, NewCertificationOperationValidator(base), shared, "not configured")
			v.delay, v.backoff = time.Millisecond, LaneBackoff{Base: time.Millisecond, Max: time.Millisecond}
			if row.setup != nil {
				row.setup(v)
			}
			if got := v.handOff(); got != row.takes {
				t.Fatalf("handOff = %v, want %v", got, row.takes)
			}
			v.Start()
			defer v.Close()
			if row.takes {
				loopStopped(t, v)
				if atomic.LoadInt32(&own) != 0 || !v.Ready() {
					t.Fatalf("own attempts %d, ready %v: routing's identical pass is the client's", own, v.Ready())
				}
				return
			}
			deadline := time.Now().Add(5 * time.Second)
			for atomic.LoadInt32(&own) == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if atomic.LoadInt32(&own) == 0 || v.Ready() {
				t.Fatalf("own attempts %d, ready %v: a different pass must not qualify the client", own, v.Ready())
			}
			_, err := v.Validate(context.Background(), literalClaim, "profile")
			var unavailable *CertificationLaneUnavailable
			if !errors.As(err, &unavailable) || unavailable.Called || atomic.LoadInt32(&hits) != 0 {
				t.Fatalf("certification with a different pass: %v, hits %d; want unavailable, nothing dialed", err, hits)
			}
		})
	}
}

// A re-qualification after a qualified lane stops answering starts after a
// random part of the retry base too, so the clients a validator's restart sends
// back to qualifying do not all start at once.
func TestCertificationRequalificationStartsSpread(t *testing.T) {
	var calls int32
	q := func(context.Context, string, string) error { atomic.AddInt32(&calls, 1); return nil }
	v := NewQualifiedCertificationValidator("2.2", NewCertificationOperationValidator("http://lane.test/fhir"), q)
	v.backoff = LaneBackoff{Base: 15 * time.Second, Max: 5 * time.Minute, Jitter: func() float64 { return 0.5 }}
	waits := recordedWaits(v)
	v.Start()
	defer v.Close()
	if got := nextWait(t, waits); got != 0 {
		t.Fatalf("an address's first attempt waits %v, want 0", got)
	}
	waitQualified(t, v)
	loopStopped(t, v)
	v.requalify()
	if got := nextWait(t, waits); got != 7500*time.Millisecond {
		t.Fatalf("the re-qualification waits %v, want 7.5s (half the base)", got)
	}
}

// The client logs its state changes, never each attempt: the first failure and a
// change of its reason, with the attempts so far and the wait before the next,
// and its success.
func TestCertificationQualificationLogsStateChangesWithAttempts(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	var calls int32
	q := func(context.Context, string, string) error {
		switch atomic.AddInt32(&calls, 1) {
		case 1, 2:
			return errors.New("qualification corpus did not pass")
		case 3:
			return context.DeadlineExceeded
		}
		return nil
	}
	v := NewQualifiedCertificationValidator("2.2", NewCertificationOperationValidator("http://lane.test/fhir"), q)
	v.backoff = LaneBackoff{Base: time.Millisecond, Max: time.Millisecond}
	v.Start()
	waitQualified(t, v)
	v.Close()
	var lines []string
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, "certification_lane_qualification") {
			lines = append(lines, l)
		}
	}
	if len(lines) != 3 {
		t.Fatalf("logged %d state changes over 4 attempts, want 3 (first failure, a new reason, ready):\n%s", len(lines), strings.Join(lines, "\n"))
	}
	for i, want := range []string{`"state":"failed"`, `"state":"failed"`, `"state":"ready"`} {
		if !strings.Contains(lines[i], want) {
			t.Fatalf("line %d: %s, want %s", i, lines[i], want)
		}
	}
	for i, want := range []string{`"attempts":1,`, `"attempts":3,`, `"attempts":4,`} {
		if !strings.Contains(lines[i], want) {
			t.Fatalf("line %d: %s, want %s", i, lines[i], want)
		}
	}
	if !strings.Contains(lines[0], `"next_retry_s":`) || strings.Contains(lines[2], `"next_retry_s":`) {
		t.Fatalf("a failure names the next retry and success does not: %s / %s", lines[0], lines[2])
	}
}

// A default lane's client deferring to routing names routing's state: pending
// until routing's first attempt fails, then failed, retrying, as the evidence
// read when it ran attempts of its own.
func TestGatedCertificationDeferringNamesRoutingsFailure(t *testing.T) {
	lane := NewDiscoveredLane("2.2", "http://lane.test/fhir", nil)
	var turn int32
	first := make(chan struct{})
	q := func(ctx context.Context, _, _ string) error {
		if atomic.AddInt32(&turn, 1) == 1 {
			<-first // routing's first attempt, failing once the row has read the pending text
			return lanequalify.ErrCorpus
		}
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = lane.KeepQualifying(ctx, q, LaneSchedule{Backoff: LaneBackoff{Base: time.Millisecond, Max: time.Millisecond}})
	}()
	v := NewGatedCertificationValidator(lane, q, "not configured")
	v.Start()
	defer v.Close()
	for atomic.LoadInt32(&turn) == 0 {
		time.Sleep(time.Millisecond)
	}
	_, err := v.Validate(context.Background(), literalClaim, "profile")
	if want := "certification validator unavailable: not configured; default lane qualification pending (routing qualifying)"; err == nil || err.Error() != want {
		t.Fatalf("deferring before routing's first attempt fails: %v, want %q", err, want)
	}
	close(first)
	deadline := time.Now().Add(5 * time.Second)
	for lane.State().Failures == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	_, err = v.Validate(context.Background(), literalClaim, "profile")
	if want := "certification validator unavailable: not configured; default lane qualification failed, retrying (routing attempt 1: qualification corpus did not pass)"; err == nil || err.Error() != want {
		t.Fatalf("deferring after routing's failure: %v, want %q", err, want)
	}
}

// The schedule a gateway runs is the shared backoff with jitter, and a 120 s
// attempt: the constructors' own, with the package's random source.
func TestCertificationClientsRunTheSharedSchedule(t *testing.T) {
	prev := certificationJitter
	certificationJitter = rand.Float64
	t.Cleanup(func() { certificationJitter = prev })
	lane := NewDiscoveredLane("2.2", "http://lane.test/fhir", nil)
	for name, v := range map[string]*GatedCertificationValidator{
		"default lane": NewGatedCertificationValidator(lane, failingQualifier(new(int32)), "not configured"),
		"address":      NewQualifiedCertificationValidator("2.2", NewCertificationOperationValidator("http://lane.test/fhir"), failingQualifier(new(int32))),
	} {
		if v.backoff.Jitter == nil || v.backoff.Base != 15*time.Second || v.backoff.Max != 5*time.Minute {
			t.Fatalf("%s: backoff %+v, want 15 s doubling to 5 min with jitter", name, v.backoff)
		}
		if v.budget != 120*time.Second {
			t.Fatalf("%s: an attempt's budget is %v, want 120 s (routing's)", name, v.budget)
		}
	}
	if v := NewGatedCertificationValidator(lane, nil, "x"); v.delay != 15*time.Second {
		t.Fatalf("a default lane's client first waits %v, want 15 s", v.delay)
	}
	if v := NewQualifiedCertificationValidator("2.2", NewCertificationOperationValidator("http://lane.test/fhir"), nil); v.delay != 0 {
		t.Fatalf("an address's client first waits %v, want 0", v.delay)
	}
}

// When routing stops retrying without a pass (its manager closed), a deferring
// client no longer waits on it: it runs its own corpus.
func TestGatedCertificationStopsDeferringWhenRoutingStops(t *testing.T) {
	lane := NewDiscoveredLane("2.2", "http://lane.test/fhir", nil)
	var calls int32
	q := failingQualifier(&calls)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = lane.KeepQualifying(ctx, q, LaneSchedule{Backoff: LaneBackoff{Base: time.Hour, Max: time.Hour}})
	}()
	for atomic.LoadInt32(&calls) == 0 {
		time.Sleep(time.Millisecond)
	}
	v := newGatedCertification(lane, NewCertificationOperationValidator(lane.Base()), q, "not configured")
	v.delay, v.backoff = time.Millisecond, LaneBackoff{Base: time.Millisecond, Max: time.Millisecond}
	v.Start()
	defer v.Close()
	time.Sleep(20 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("%d qualifications while routing retried, want routing's one", got)
	}
	cancel()
	<-done
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&calls) < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := atomic.LoadInt32(&calls); got < 2 {
		t.Fatalf("routing stopped and the client ran no corpus of its own (%d qualifications)", got)
	}
}

// MON-R24: a qualified lane that does not answer its call is unavailable and
// says it was called (CertificationLaneUnavailable.Called), so the call is
// timed; a lane never started says it was not.
func TestCertificationUnreachableLaneWasCalled(t *testing.T) {
	gone := httptest.NewServer(http.NotFoundHandler())
	base := gone.URL + "/fhir"
	gone.Close() // connection refused from here on
	v := NewQualifiedCertificationValidator("2.2", NewCertificationOperationValidator(base), func(context.Context, string, string) error { return nil })
	v.backoff = LaneBackoff{Base: time.Hour, Max: time.Hour}
	v.Start()
	defer v.Close()
	waitQualified(t, v)
	loopStopped(t, v)
	_, err := v.Validate(context.Background(), literalClaim, "profile")
	var unavailable *CertificationLaneUnavailable
	if !errors.As(err, &unavailable) || !unavailable.Called {
		t.Fatalf("a qualified lane that refused the connection: %v; want unavailable, called", err)
	}
	unstarted := NewQualifiedCertificationValidator("2.2", NewCertificationOperationValidator(base), func(context.Context, string, string) error { return nil })
	if _, err := unstarted.Validate(context.Background(), literalClaim, "profile"); !errors.As(err, &unavailable) || unavailable.Called {
		t.Fatalf("a lane never started: %v; want unavailable, not called", err)
	}
}
