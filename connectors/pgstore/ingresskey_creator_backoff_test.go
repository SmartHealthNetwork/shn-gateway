//go:build !shn_coreonly

package pgstore

import (
	"bytes"
	"context"
	"log"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
)

// hangingWriteDB is a store whose reads answer and whose creator transaction hangs: once
// hang is set, Begin blocks until its context's deadline (the store timeout) and fails.
// It counts every Begin and signals each one as it starts hanging.
type hangingWriteDB struct {
	keyDB
	hang    atomic.Bool
	begins  atomic.Int32
	hanging chan struct{}
}

func (d *hangingWriteDB) Begin(ctx context.Context) (pgx.Tx, error) {
	d.begins.Add(1)
	if !d.hang.Load() {
		return d.keyDB.Begin(ctx)
	}
	select {
	case d.hanging <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// hangBound is how long any token request may take while key writes hang: the one
// creator attempt it can find in flight (a store timeout) plus a margin for the rest.
const hangBound = storeTimeout + time.Second

// With key writes hanging at the due age, token requests do not queue behind one creator
// attempt each. The first request's attempt hangs for one store timeout; every concurrent
// request waits behind it at most that long, then grace-signs at once. After that the
// refresh tick alone retries, once per tick, and a request that arrives while the tick's
// attempt hangs still returns within one store timeout plus the margin.
func TestIngressKeyPg_HangingKeyWritesCostOneAttemptPerTickAndNoQueue(t *testing.T) {
	pool := testPool(t)
	clk := newLockedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	db := &hangingWriteDB{keyDB: pool, hanging: make(chan struct{}, 1)}
	s := NewIngressKeyStore(db, "holder", clk.now)
	s.refreshing.Store(true) // the refresh loop runs; the row drives its ticks by hand
	kid1, _, err := s.SigningKey(clk.now())
	if err != nil {
		t.Fatal(err)
	}

	clk.advance(ingressKeyRotation)
	db.hang.Store(true)
	db.begins.Store(0)
	// The first request starts the creator transaction, which hangs. The others arrive a
	// second apart while it hangs, each past the reload throttle, so each reaches the
	// creator decision itself: with one attempt per request they would queue a store
	// timeout each.
	const callers = 8
	type answer struct {
		kid  string
		err  error
		took time.Duration
	}
	answers := make(chan answer, callers)
	call := func(at time.Time) {
		start := time.Now()
		kid, _, err := s.SigningKey(at)
		answers <- answer{kid, err, time.Since(start)}
	}
	go call(clk.now())
	mustSignal(t, db.hanging, "the first request's creator attempt is hanging")
	for range callers - 1 {
		clk.advance(reloadThrottle)
		go call(clk.now())
	}
	for range callers {
		a := <-answers
		if a.err != nil || a.kid != kid1 || a.took > hangBound {
			t.Fatalf("a token request with key writes hanging = %q (err %v) after %s; want a grace signing with %q within %s", a.kid, a.err, a.took, kid1, hangBound)
		}
	}
	if n := db.begins.Load(); n != 1 {
		t.Fatalf("%d token requests started %d creator transactions, want 1", callers, n)
	}

	for tick := range 2 {
		select { // a hang signalled before this tick is not the tick's own
		case <-db.hanging:
		default:
		}
		clk.advance(ingressKeyRefresh)
		refreshed := make(chan error, 1)
		go func() { refreshed <- s.refreshOnce(clk.now()) }()
		mustSignal(t, db.hanging, "the tick's creator attempt is hanging")
		// A second after the tick's reload, so this request reloads itself and reaches
		// the creator decision once the tick's attempt lets go of the signing lock.
		clk.advance(reloadThrottle)
		start := time.Now()
		kid, _, err := s.SigningKey(clk.now())
		if took := time.Since(start); err != nil || kid != kid1 || took > hangBound {
			t.Fatalf("tick %d: a token request during the tick's hanging attempt = %q (err %v) after %s; want %q within %s", tick, kid, err, took, kid1, hangBound)
		}
		if err := <-refreshed; err == nil {
			t.Fatal("the tick's creator attempt succeeded with writes hanging")
		}
		for range 3 {
			// A second apart, so each request reloads (healthy) and reaches the creator
			// decision rather than the reload throttle: it must not start an attempt.
			clk.advance(reloadThrottle)
			start := time.Now()
			if kid, _, err := s.SigningKey(clk.now()); err != nil || kid != kid1 || time.Since(start) > hangBound {
				t.Fatalf("tick %d: a token request after the tick = %q (err %v) after %s", tick, kid, err, time.Since(start))
			}
		}
		if n := db.begins.Load(); n != int32(tick+2) {
			t.Fatalf("after tick %d: %d creator transactions, want one per tick plus the first request's (%d)", tick, n, tick+2)
		}
	}
}

// Without the refresh loop, the request path itself retries, once per refresh interval:
// a failed creator transaction starts no new one inside that window, whether or not the
// slack remains (past it the stored error is the answer, at once), and a replica that
// has never signed answers the error without a second attempt.
func TestIngressKeyPg_FailedKeyWriteIsRetriedOncePerWindowWithoutTheRefreshLoop(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	db := &adoptionFaultDB{keyDB: pool}
	s := NewIngressKeyStore(db, "holder", now)
	kid1, _, err := s.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	notAfter := signingNotAfter(t, s)
	begins := func() int { return db.calls["begin"] }
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	graceSignings := 0
	sign := func(what string) {
		t.Helper()
		if kid, _, err := s.SigningKey(now()); err != nil || kid != kid1 {
			t.Fatalf("%s: SigningKey = %q (err %v), want a grace signing with %q", what, kid, err, kid1)
		}
		graceSignings++
	}

	advance(ingressKeyRotation)
	db.fault = "begin"
	base := begins()
	sign("the first failed write")
	if begins() != base+1 {
		t.Fatalf("creator transactions = %d, want %d", begins(), base+1)
	}
	advance(ingressKeyRefresh - reloadThrottle)
	for range 3 {
		sign("inside the window")
		advance(reloadThrottle / 4)
	}
	if begins() != base+1 {
		t.Fatalf("inside the window %d creator transactions were started, want none", begins()-base-1)
	}
	advance(reloadThrottle)
	sign("past the window")
	if begins() != base+2 {
		t.Fatalf("past the window: %d creator transactions, want %d", begins(), base+2)
	}

	// A retry five seconds before the slack ends fails and grace-signs; five seconds on,
	// still inside its window, the slack is spent: the stored error, with no new attempt.
	advance(notAfter.Add(-engine.IngressBearerTTL - 5*time.Second).Sub(now()))
	sign("five seconds before the slack ends")
	at := begins()
	advance(5 * time.Second)
	if _, _, err := s.SigningKey(now()); err == nil {
		t.Fatal("at not_after - TTL the stored error must be the answer")
	}
	if begins() != at {
		t.Fatal("past the slack, inside the window, a new creator transaction was started")
	}

	cold := NewIngressKeyStore(&adoptionFaultDB{keyDB: pool, fault: "begin"}, "holder", now)
	coldDB := cold.pool.(*adoptionFaultDB)
	for range 2 {
		if _, _, err := cold.SigningKey(now()); err == nil {
			t.Fatal("a replica that has never signed must answer the error")
		}
		advance(reloadThrottle) // past the reload throttle: the next call reaches the creator decision
	}
	if n := coldDB.calls["begin"]; n != 1 {
		t.Fatalf("a cold replica started %d creator transactions inside one window, want 1", n)
	}

	// Once writes heal, the next attempt rotates, and every grace signing above is
	// accounted for exactly once in the deferred and resumed lines.
	db.fault = ""
	advance(ingressKeyRefresh)
	if kid, _, err := s.SigningKey(now()); err != nil || kid == kid1 {
		t.Fatalf("once writes heal the next attempt must rotate: %q (err %v)", kid, err)
	}
	if _, _, counted := deferredLines(buf.String()); counted != graceSignings {
		t.Fatalf("the deferred and resumed lines count %d grace signings, want %d; log:\n%s", counted, graceSignings, buf.String())
	}
}

// The tick does not run a creator transaction straight after a token request's failed
// one (a request could otherwise wait out two in a row), but its own failure never
// suppresses the next tick's retry, even with the ticker running a little early, and a
// clock that steps back ends the window.
func TestIngressKeyPg_TheTickSkipsOnlyARetryStraightAfterAFailedWrite(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	db := &adoptionFaultDB{keyDB: pool}
	s := NewIngressKeyStore(db, "holder", now)
	if _, _, err := s.SigningKey(now()); err != nil {
		t.Fatal(err)
	}
	advance(ingressKeyRotation)
	db.fault = "begin"
	base := db.calls["begin"]
	if _, _, err := s.SigningKey(now()); err != nil {
		t.Fatal(err)
	}
	advance(time.Second)
	if err := s.rotateIfDue(now()); err == nil {
		t.Fatal("the skipped tick must still report the stall")
	}
	if n := db.calls["begin"] - base; n != 1 {
		t.Fatalf("a tick a second after a request's failed write started a second transaction (%d in all)", n)
	}
	advance(ingressKeyRefresh / 2)
	if err := s.rotateIfDue(now()); err == nil {
		t.Fatal("the tick's creator attempt succeeded with Begin failing")
	}
	if n := db.calls["begin"] - base; n != 2 {
		t.Fatalf("the next tick did not retry (%d transactions in all, want 2)", n)
	}
	advance(ingressKeyRefresh - 100*time.Millisecond) // the ticker fires a little early
	if err := s.rotateIfDue(now()); err == nil {
		t.Fatal("the tick's creator attempt succeeded with Begin failing")
	}
	if n := db.calls["begin"] - base; n != 3 {
		t.Fatalf("an early tick did not retry after the previous tick's own failure (%d transactions in all, want 3)", n)
	}
	// A clock that steps back after the tick's own failure: the next tick still retries,
	// so a store that has recovered is not left unused until the slack runs out. (Ten
	// minutes into the stall, so a minute back is still past the due age.)
	advance(10 * time.Minute)
	if err := s.rotateIfDue(now()); err == nil {
		t.Fatal("the tick's creator attempt succeeded with Begin failing")
	}
	advance(-time.Minute)
	if err := s.rotateIfDue(now()); err == nil {
		t.Fatal("the tick's creator attempt succeeded with Begin failing")
	}
	if n := db.calls["begin"] - base; n != 5 {
		t.Fatalf("after the clock stepped back the tick did not retry (%d transactions in all, want 5)", n)
	}
	// A token request's failure, then the clock steps back behind it: the tick retries
	// rather than skipping as if the failure were moments ago.
	advance(ingressKeyRefresh)
	if _, _, err := s.SigningKey(now()); err != nil {
		t.Fatal(err)
	}
	if n := db.calls["begin"] - base; n != 6 {
		t.Fatalf("a request past the window did not retry (%d transactions in all, want 6)", n)
	}
	advance(-time.Minute)
	if err := s.rotateIfDue(now()); err == nil {
		t.Fatal("the tick's creator attempt succeeded with Begin failing")
	}
	if n := db.calls["begin"] - base; n != 7 {
		t.Fatalf("after the clock stepped back behind a request's failure the tick did not retry (%d transactions in all, want 7)", n)
	}
}

// The race at a window's end: the tick reads the clock and starts its reload, and a token
// request takes the signing lock during that reload, runs its own creator attempt (the
// window has just expired) and fails, stamping a time later than the tick's reading. The
// tick's reload can wait a store timeout on reloadMu behind another reload and then take
// its own, so that stamp can be more than a store timeout after the reading, past the time
// window; the failure count is what catches it. The tick must still see that attempt as
// moments ago and not start a second one straight after it, or the requests queued behind
// the first would wait out two store timeouts.
func TestIngressKeyPg_TheTickSkipsARequestsAttemptMadeWhileItReloaded(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	db := &adoptionFaultDB{keyDB: pool}
	s := NewIngressKeyStore(db, "holder", now)
	if _, _, err := s.SigningKey(now()); err != nil {
		t.Fatal(err)
	}
	advance(ingressKeyRotation)
	db.fault = "begin"
	base := db.calls["begin"]

	tickAt, failures := now(), s.requestFailureCount() // the tick reads both, then reloads
	advance(storeTimeout + 500*time.Millisecond)
	if _, _, err := s.SigningKey(now()); err != nil { // the request's own attempt fails
		t.Fatal(err)
	}
	if err := s.rotateIfDueSince(tickAt, failures); err == nil {
		t.Fatal("the skipped tick must still report the stall")
	}
	if n := db.calls["begin"] - base; n != 1 {
		t.Fatalf("the tick started a creator transaction straight after the request's (%d in all, want 1)", n)
	}
}

// The same race with the request taking the signing lock between the tick's clock reading
// and its count read: the count read waits out the request's attempt, so the count has not
// moved, and only the time says the attempt was moments ago, stamped after the tick's
// reading. A failure stamped up to a store timeout after the reading counts as moments
// ago; one stamped a store timeout or more after it is a clock that stepped back, and the
// tick retries.
func TestIngressKeyPg_TheTickSkipsARequestsAttemptStampedJustAfterItsClockReading(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	db := &adoptionFaultDB{keyDB: pool}
	s := NewIngressKeyStore(db, "holder", now)
	if _, _, err := s.SigningKey(now()); err != nil {
		t.Fatal(err)
	}
	advance(ingressKeyRotation)
	db.fault = "begin"
	base := db.calls["begin"]

	tickAt := now() // the tick reads the clock
	advance(time.Millisecond)
	if _, _, err := s.SigningKey(now()); err != nil { // a request takes signMu first and fails
		t.Fatal(err)
	}
	if err := s.rotateIfDueSince(tickAt, s.requestFailureCount()); err == nil {
		t.Fatal("the skipped tick must still report the stall")
	}
	if n := db.calls["begin"] - base; n != 1 {
		t.Fatalf("the tick started a creator transaction straight after the request's (%d in all, want 1)", n)
	}

	advance(ingressKeyRefresh)
	if _, _, err := s.SigningKey(now()); err != nil {
		t.Fatal(err)
	}
	if n := db.calls["begin"] - base; n != 2 {
		t.Fatalf("a request past the window did not retry (%d transactions in all, want 2)", n)
	}
	if err := s.rotateIfDueSince(now().Add(-storeTimeout), s.requestFailureCount()); err == nil {
		t.Fatal("the tick's creator attempt succeeded with Begin failing")
	}
	if n := db.calls["begin"] - base; n != 3 {
		t.Fatalf("a tick a store timeout behind a request's failure did not retry (%d transactions in all, want 3)", n)
	}
}
