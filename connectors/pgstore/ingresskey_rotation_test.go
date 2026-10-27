//go:build !shn_coreonly

package pgstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
)

// Rotation falls due an hour before the slack runs out (MON-R45): the stall a
// monitor sees as the key's remaining life and age has that hour before
// /oauth/token answers 503. Validity is a day plus a bearer's TTL and a minute,
// whatever the rotation age.
func TestIngressKeyRotationLeavesAnHourBeforeTheSlackEnds(t *testing.T) {
	if margin := ingressKeyLife - ingressKeyRotation - engine.IngressBearerTTL; margin < time.Hour {
		t.Fatalf("life - rotation - bearer TTL = %s; a stalled rotation must show at least an hour before the 503s", margin)
	}
	if want := 24*time.Hour + engine.IngressBearerTTL + time.Minute; ingressKeyLife != want {
		t.Fatalf("ingressKeyLife = %s, want %s: a key's validity is unchanged by an earlier rotation", ingressKeyLife, want)
	}
	if s := NewIngressKeyStore(nil, "holder", nil); s.rotation != ingressKeyRotation {
		t.Fatalf("a new store selects by %s, want ingressKeyRotation %s", s.rotation, ingressKeyRotation)
	}
}

// signingNotAfter is the not_after of the key s signs with, as the ingress key
// remaining-life gauge reads it (KeyTimes).
func signingNotAfter(t *testing.T, s *IngressKeyStore) time.Time {
	t.Helper()
	got := s.KeyTimes()
	if !got.HasSigning {
		t.Fatal("KeyTimes has no signing key")
	}
	return got.SigningNotAfter
}

// rotationReplicas builds two HTTP replicas of one holder: A's key store reaches the
// database through downDB, so a row can take it away from A alone; B's is healthy and
// only verifies. Both share one replay store on the healthy pool, so a 503 from A's
// /oauth/token is the key store's, never the replay store's.
func rotationReplicas(t *testing.T, clk *lockedClock) (a, b *IngressKeyStore, db *downDB, mint func(jti string) (int, string), verifyAtB func(bearer string) int) {
	t.Helper()
	pool := testPool(t)
	db = &downDB{keyDB: pool}
	a = NewIngressKeyStore(db, "holder", clk.now)
	b = NewIngressKeyStore(pool, "holder", clk.now)
	clientKey, pub := ackLostClientKey(t)
	srvA := adoptionReplica(t, pool, a, pub, clk.now)
	srvB := adoptionReplica(t, pool, b, pub, clk.now)
	mint = func(jti string) (int, string) {
		return adoptionToken(t, srvA, adoptionAssertion(t, clientKey, clk.now(), jti))
	}
	verifyAtB = func(bearer string) int { return adoptionDiscovery(t, srvB, bearer) }
	return a, b, db, mint, verifyAtB
}

// The rejection row for MON-R45: the store is unreachable from the moment rotation falls
// due. The replica keeps answering /oauth/token with the current key (every bearer still
// verifies at a sibling), its refresh fails every tick, and the key's remaining life
// counts down from over an hour. /oauth/token answers 503 first at exactly
// not_after - IngressBearerTTL, at least an hour after the stall began, and never with a
// new or a stale key.
func TestIngressKeyPg_StalledRotationShowsAnHourBeforeTheTokenEndpointAnswers503(t *testing.T) {
	clk := newLockedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	a, _, db, mint, verifyAtB := rotationReplicas(t, clk)
	code, bearer := mint("before-the-stall")
	if code != http.StatusOK {
		t.Fatalf("mint before the stall = %d", code)
	}
	kid1 := a.signing
	if got := verifyAtB(bearer); got != http.StatusOK {
		t.Fatalf("B must verify A's bearer before the stall: %d", got)
	}

	clk.advance(ingressKeyRotation) // rotation is due…
	db.down.Store(true)             // …and the store is unreachable from A
	stall := clk.now()
	notAfter := signingNotAfter(t, a)
	if left := notAfter.Sub(stall); left < time.Hour+engine.IngressBearerTTL {
		t.Fatalf("remaining life at the stall = %s; want over an hour plus a bearer's TTL", left)
	}
	if err := a.refreshOnce(clk.now()); err == nil {
		t.Fatal("refresh must fail while the store is unreachable: that failure is what the loop counts")
	}
	if got := a.KeyTimes(); !got.HasNewest || stall.Sub(got.NewestCreated) < ingressKeyRotation {
		t.Fatalf("KeyTimes = %+v: the newest key's age must show the stall (past %s)", got, ingressKeyRotation)
	}

	// One refresh tick at a time, as RunRefresh drives it, until the first 503.
	var first503 time.Time
	for i := 0; first503.IsZero(); i++ {
		if i > 200 {
			t.Fatal("no 503 within 200 refresh ticks of the stall")
		}
		if err := a.refreshOnce(clk.now()); err == nil {
			t.Fatal("refresh succeeded against an unreachable store")
		}
		code, bearer := mint(fmt.Sprintf("stall-%d", i))
		switch code {
		case http.StatusOK:
			if a.signing != kid1 {
				t.Fatalf("A signed with %q during the stall, want the current key %q", a.signing, kid1)
			}
			if got := verifyAtB(bearer); got != http.StatusOK {
				t.Fatalf("a grace bearer minted at %s did not verify at B: %d", clk.now().Sub(stall), got)
			}
			clk.advance(ingressKeyRefresh)
		case http.StatusServiceUnavailable:
			first503 = clk.now()
		default:
			t.Fatalf("mint during the stall = %d, want 200 or 503", code)
		}
	}
	if want := notAfter.Add(-engine.IngressBearerTTL); !first503.Equal(want) {
		t.Fatalf("first 503 at %s after the stall, want exactly not_after - TTL (%s after)", first503.Sub(stall), want.Sub(stall))
	}
	if lead := first503.Sub(stall); lead < time.Hour {
		t.Fatalf("the stall showed only %s before the first 503; MON-R45 needs about an hour", lead)
	}
	// An alarm on remaining life under 3600 s fires 55 minutes before the 503s.
	if lead := first503.Sub(notAfter.Add(-time.Hour)); lead < 55*time.Minute {
		t.Fatalf("remaining life crosses an hour only %s before the first 503", lead)
	}
	if _, _, err := a.SigningKey(clk.now()); err == nil {
		t.Fatal("SigningKey at not_after - TTL must return the store error, not a key")
	}

	// The last instant grace may sign: a bearer minted then verifies at B for its whole TTL.
	clk.advance(-time.Nanosecond)
	code, bearer = mint("last-grace-instant")
	if code != http.StatusOK || a.signing != kid1 {
		t.Fatalf("mint at not_after - TTL - 1ns = %d with %q; want 200 with the current key", code, a.signing)
	}
	clk.advance(engine.IngressBearerTTL - time.Second)
	if got := verifyAtB(bearer); got != http.StatusOK {
		t.Fatalf("the last grace bearer must verify at B until its exp: %d", got)
	}
}

// The same moment with the store reachable: the refresh tick rotates at the due age,
// the new key carries a full validity, and a bearer minted a second before the rotation
// verifies after it on every replica — the warm sibling and a replica that starts cold
// half an hour later, which adopts the new key, never the old one. A replica that starts
// cold with the store unreachable has no key to grace-sign with and answers 503, as before.
func TestIngressKeyPg_RotationAtTheDueAgeKeepsThePreviousBearersVerifying(t *testing.T) {
	clk := newLockedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	a, b, _, mint, verifyAtB := rotationReplicas(t, clk)
	if code, _ := mint("first"); code != http.StatusOK {
		t.Fatalf("first mint = %d", code)
	}
	kid1 := a.signing
	if err := b.refreshOnce(clk.now()); err != nil {
		t.Fatal(err)
	}

	clk.advance(ingressKeyRotation - time.Second)
	code, early := mint("a-second-before-rotation")
	if code != http.StatusOK || a.signing != kid1 {
		t.Fatalf("mint a second before rotation = %d with %q; want 200 with %q", code, a.signing, kid1)
	}
	clk.advance(time.Second)
	if err := a.refreshOnce(clk.now()); err != nil {
		t.Fatal(err)
	}
	kid2 := a.signing
	if kid2 == kid1 {
		t.Fatal("the refresh tick did not rotate at the due age")
	}
	if got, want := signingNotAfter(t, a), clk.now().Add(ingressKeyLife); !got.Equal(want) {
		t.Fatalf("new key's not_after = %s, want ingressKeyLife from its creation (%s)", got, want)
	}
	if got := verifyAtB(early); got != http.StatusOK {
		t.Fatalf("B must verify the pre-rotation bearer after the rotation: %d", got)
	}

	clk.advance(30 * time.Minute)
	pool := a.pool.(*downDB).keyDB
	c := NewIngressKeyStore(pool, "holder", clk.now)
	if pub, ok, err := c.VerificationKey(kid1, clk.now()); !ok || err != nil || pub == nil {
		t.Fatalf("a replica that starts after the rotation must verify the previous key (ok=%v err=%v)", ok, err)
	}
	kid, _, err := c.SigningKey(clk.now())
	if err != nil || kid != kid2 {
		t.Fatalf("a replica that starts after the rotation signs with %q (err %v), want the new key %q", kid, err, kid2)
	}
	down := &downDB{keyDB: pool}
	down.down.Store(true)
	if _, _, err := NewIngressKeyStore(down, "holder", clk.now).SigningKey(clk.now()); err == nil {
		t.Fatal("a cold replica with the store unreachable has no key to sign with: it must answer the error")
	}
}

// The due age is exact: a key is selected one nanosecond before it, by the cached fast
// path and by the creator's own re-read under the holder lock, and neither at it.
func TestIngressKeyPg_RotationIsDueAtExactlyTheRotationAge(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	s := NewIngressKeyStore(pool, "holder", now)
	kid1, _, err := s.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	advance(ingressKeyRotation - time.Nanosecond)
	if kid, _, ok := s.currentSigning(now()); !ok || kid != kid1 {
		t.Fatalf("one nanosecond before the due age the cached key must still sign (got %q ok=%v)", kid, ok)
	}
	if kid, _, err := s.selectOrCreateKey(now()); err != nil || kid != kid1 {
		t.Fatalf("one nanosecond before the due age the creator must adopt %q, got %q (err %v)", kid1, kid, err)
	}
	advance(time.Nanosecond)
	if _, _, ok := s.currentSigning(now()); ok {
		t.Fatal("at the due age the cached key must no longer be selected")
	}
	kid2, _, err := s.SigningKey(now())
	if err != nil || kid2 == kid1 {
		t.Fatalf("at the due age SigningKey must create a new key, got %q (err %v)", kid2, err)
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM gw_ingress_key WHERE holder_id='holder'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("rows = %d, want the previous key and the new one", n)
	}
}

// Two replicas whose refresh ticks fall due together converge on ONE new key: both have
// reloaded and found no eligible key before either begins its creator transaction, and
// the holder's advisory lock makes the second adopt the first's committed row. Every
// signing call on either replica then names that key.
func TestIngressKeyPg_ConcurrentRotationAtTheDueAgeConvergesOnOneKey(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	kid1, _, err := NewIngressKeyStore(pool, "holder", now).SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	advance(ingressKeyRotation)
	barrier := &creatorBarrierDB{keyDB: pool, arrived: make(chan struct{}, 2), release: make(chan struct{})}
	a := NewIngressKeyStore(barrier, "holder", now)
	b := NewIngressKeyStore(barrier, "holder", now)
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(barrier.release) }) })
	done := make(chan error, 2)
	for _, s := range []*IngressKeyStore{a, b} {
		go func() { done <- s.refreshOnce(now()) }()
	}
	mustSignal(t, barrier.arrived, "first rotator reached its creator transaction")
	mustSignal(t, barrier.arrived, "second rotator reached its creator transaction")
	release.Do(func() { close(barrier.release) })
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a rotator did not finish")
		}
	}
	if a.signing == kid1 || a.signing != b.signing {
		t.Fatalf("rotators signing %q and %q; want one new key (previous %q)", a.signing, b.signing, kid1)
	}
	var wg sync.WaitGroup
	kids := make(chan string, 16)
	for i := range 16 {
		s := []*IngressKeyStore{a, b}[i%2]
		wg.Add(1)
		go func() {
			defer wg.Done()
			kid, _, err := s.SigningKey(now())
			if err != nil {
				t.Error(err)
			}
			kids <- kid
		}()
	}
	wg.Wait()
	close(kids)
	for kid := range kids {
		if kid != a.signing {
			t.Fatalf("a signing call named %q, want the converged key %q", kid, a.signing)
		}
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM gw_ingress_key WHERE holder_id='holder' AND created_at >= $1`, now()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("rows created at the due age = %d, want 1", n)
	}
}

// A rolling deploy runs replicas of the earlier release, which rotated at 24 h, beside
// this one. Both write the same not_after, so neither's keys outlive the other's rule.
// The new replica rotates at the due age; the old one keeps its key until its next
// refresh, then signs with the newer key (it adopts the newest eligible key) and never
// creates one of its own. At most two keys sign, each verifies on both, and both
// replicas sign with one key well before the old rule's 24 h.
func TestIngressKeyPg_MixedFleetRotationConvergesBeforeTheEarlierRulesDueAge(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	older := NewIngressKeyStore(pool, "holder", now)
	older.rotation = 24 * time.Hour // the earlier release's rule
	newer := NewIngressKeyStore(pool, "holder", now)
	signed := map[string]bool{}
	sign := func(s *IngressKeyStore) string {
		t.Helper()
		kid, _, err := s.SigningKey(now())
		if err != nil {
			t.Fatal(err)
		}
		signed[kid] = true
		return kid
	}
	kid1 := sign(older)
	if got := sign(newer); got != kid1 {
		t.Fatalf("the newer replica must adopt the older one's key, got %q want %q", got, kid1)
	}

	advance(ingressKeyRotation)
	if err := newer.refreshOnce(now()); err != nil {
		t.Fatal(err)
	}
	kid2 := sign(newer)
	if kid2 == kid1 {
		t.Fatal("the newer replica did not rotate at its due age")
	}
	if got := sign(older); got != kid1 {
		t.Fatalf("before its next refresh the older replica signs %q, want its current key %q", got, kid1)
	}
	for _, s := range []*IngressKeyStore{older, newer} {
		for _, kid := range []string{kid1, kid2} {
			if _, ok, err := s.VerificationKey(kid, now()); !ok || err != nil {
				t.Fatalf("%q must verify on every replica (ok=%v err=%v)", kid, ok, err)
			}
		}
		advance(reloadThrottle)
	}

	advance(ingressKeyRefresh)
	if err := older.refreshOnce(now()); err != nil {
		t.Fatal(err)
	}
	if got := sign(older); got != kid2 {
		t.Fatalf("after its refresh the older replica signs %q, want the newer key %q", got, kid2)
	}
	advance(time.Hour) // past the earlier rule's 24 h: nothing more to rotate
	for _, s := range []*IngressKeyStore{older, newer} {
		if err := s.refreshOnce(now()); err != nil {
			t.Fatal(err)
		}
		if got := sign(s); got != kid2 {
			t.Fatalf("at the earlier rule's due age a replica signs %q, want %q", got, kid2)
		}
	}
	if len(signed) != 2 {
		t.Fatalf("%d keys signed across the mixed fleet, want at most two", len(signed))
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM gw_ingress_key WHERE holder_id='holder'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("rows = %d, want two: the older replica must never create a key of its own", n)
	}
}

var deferredLine = regexp.MustCompile(`rotation (deferred|resumed).*\((\d+) signings deferred since the last line\)`)

// deferredLines counts the "rotation deferred" and "rotation resumed" lines in a log and
// sums the deferred signings they carry.
func deferredLines(log string) (deferred, resumed, counted int) {
	for _, m := range deferredLine.FindAllStringSubmatch(log, -1) {
		n, _ := strconv.Atoi(m[2])
		counted += n
		if m[1] == "deferred" {
			deferred++
		} else {
			resumed++
		}
	}
	return deferred, resumed, counted
}

// A stall grace-signs for up to an hour, so the "rotation deferred" line is written at
// most once per refresh tick, carrying the number of signings deferred since the last
// one: N ticks of a stall give N lines however many tokens are minted. The line written
// when rotation resumes accounts for the rest, whichever path resumes it, so every
// deferred signing is counted exactly once. A clock that steps back cannot silence the
// line, and the next stall's first deferral logs at once.
func TestIngressKeyPg_StalledRotationLogsOncePerRefreshTick(t *testing.T) {
	for _, tc := range []struct {
		name string
		// resume ends the stall on s once the store is back; sibling shares the pool.
		resume func(t *testing.T, s, sibling *IngressKeyStore, now time.Time)
	}{
		{"refresh_creates", func(t *testing.T, s, _ *IngressKeyStore, now time.Time) {
			if err := s.refreshOnce(now); err != nil {
				t.Fatal(err)
			}
		}},
		{"refresh_adopts_a_siblings_key", func(t *testing.T, s, sibling *IngressKeyStore, now time.Time) {
			if err := sibling.refreshOnce(now); err != nil {
				t.Fatal(err)
			}
			if err := s.refreshOnce(now.Add(reloadThrottle)); err != nil {
				t.Fatal(err)
			}
		}},
		{"signing_creates", func(t *testing.T, s, _ *IngressKeyStore, now time.Time) {
			if _, _, err := s.SigningKey(now.Add(reloadThrottle)); err != nil {
				t.Fatal(err)
			}
		}},
		{"signing_adopts_a_siblings_key", func(t *testing.T, s, sibling *IngressKeyStore, now time.Time) {
			if err := sibling.refreshOnce(now); err != nil {
				t.Fatal(err)
			}
			kid, _, err := sibling.SigningKey(now)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok, err := s.VerificationKey(kid, now.Add(reloadThrottle)); !ok || err != nil {
				t.Fatalf("s must learn the sibling's key (ok=%v err=%v)", ok, err)
			}
			if got, _, err := s.SigningKey(now.Add(reloadThrottle)); err != nil || got != kid {
				t.Fatalf("s signs %q (err %v), want the sibling's key %q", got, err, kid)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testPool(t)
			t0 := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
			now, advance := fixedClock(t0)
			db := &downDB{keyDB: pool}
			s := NewIngressKeyStore(db, "holder", now)
			sibling := NewIngressKeyStore(pool, "holder", now)
			if _, _, err := s.SigningKey(now()); err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			log.SetOutput(&buf)
			t.Cleanup(func() { log.SetOutput(os.Stderr) })

			advance(ingressKeyRotation)
			db.down.Store(true)
			const ticks, perSecond = 4, 3
			signings := 0
			for range ticks * int(ingressKeyRefresh/time.Second) {
				for range perSecond {
					if _, _, err := s.SigningKey(now()); err != nil {
						t.Fatalf("grace signing failed %s into the stall: %v", now().Sub(t0.Add(ingressKeyRotation)), err)
					}
					signings++
				}
				advance(time.Second)
			}
			if deferred, _, _ := deferredLines(buf.String()); deferred != ticks {
				t.Fatalf("a stall of %d refresh ticks with %d signings wrote %d deferred lines, want %d; log:\n%s", ticks, signings, deferred, ticks, buf.String())
			}
			// The clock steps back a minute, behind the last line. The refresh tick on the
			// stepped-back clock reloads (and fails) as always; the next signing a second
			// later reloads unthrottled and must still write its line rather than wait for
			// the clock to catch up with the last one.
			advance(-time.Minute)
			if err := s.refreshOnce(now()); err == nil {
				t.Fatal("refresh succeeded against an unreachable store")
			}
			advance(time.Second)
			if _, _, err := s.SigningKey(now()); err != nil {
				t.Fatal(err)
			}
			signings++
			if deferred, _, _ := deferredLines(buf.String()); deferred != ticks+1 {
				t.Fatalf("after the clock stepped back a minute the deferral wrote %d lines in all, want %d; log:\n%s", deferred, ticks+1, buf.String())
			}
			for range 2 { // inside the line's window: counted for the resumed line
				if _, _, err := s.SigningKey(now()); err != nil {
					t.Fatal(err)
				}
				signings++
			}

			db.down.Store(false)
			tc.resume(t, s, sibling, now())
			if _, resumed, counted := deferredLines(buf.String()); resumed != 1 || counted != signings {
				t.Fatalf("after the stall: %d resumed lines and %d signings counted, want 1 and %d; log:\n%s", resumed, counted, signings, buf.String())
			}

			buf.Reset()
			advance(ingressKeyRotation + 2*reloadThrottle) // the new key falls due…
			db.down.Store(true)                            // …into a second stall
			if _, _, err := s.SigningKey(now()); err != nil {
				t.Fatal(err)
			}
			if deferred, _, counted := deferredLines(buf.String()); deferred != 1 || counted != 1 {
				t.Fatalf("the next stall's first deferral wrote %d lines counting %d, want 1 line counting 1; log:\n%s", deferred, counted, buf.String())
			}
		})
	}
}

// Unauthenticated bearers with random kids keep the miss throttle armed through a stall:
// each miss runs the reload (and fails) just before a token request, so every signing is
// throttled and none runs a reload of its own. The deferred line still comes once per
// refresh tick, from the failed reload's outcome, rather than vanishing for the hour.
func TestIngressKeyPg_StalledRotationUnderAMissFloodStillLogsOncePerRefreshTick(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	db := &downDB{keyDB: pool}
	s := NewIngressKeyStore(db, "holder", now)
	if _, _, err := s.SigningKey(now()); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	advance(ingressKeyRotation)
	db.down.Store(true)
	const ticks, perSecond = 3, 3
	perTick := perSecond * int(ingressKeyRefresh/time.Second)
	signings := 0
	for i := range ticks * int(ingressKeyRefresh/time.Second) {
		kid := fmt.Sprintf("%032x", i+1)
		if _, ok, err := s.VerificationKey(kid, now()); ok || err == nil {
			t.Fatalf("a miss against the unreachable store = ok:%v err:%v; want the store error", ok, err)
		}
		if !s.throttled(now()) {
			t.Fatal("the miss did not arm the throttle: the row would not exercise the throttled branch")
		}
		for range perSecond {
			if _, _, err := s.SigningKey(now()); err != nil {
				t.Fatal(err)
			}
			signings++
		}
		advance(time.Second)
	}
	deferred, _, counted := deferredLines(buf.String())
	if deferred != ticks {
		t.Fatalf("a flooded stall of %d refresh ticks wrote %d deferred lines, want %d; log:\n%s", ticks, deferred, ticks, buf.String())
	}
	if counted > signings || counted < signings-perTick {
		t.Fatalf("the deferred lines count %d of %d signings; only the last tick's may be outstanding", counted, signings)
	}
}

// A healthy store can still grace-sign for a moment: a miss reload (an unknown kid) arms
// the throttle at the due age, and the token request inside that second signs with the
// current key until the refresh tick rotates. That is a routine bridge, not a stall: it
// writes no "rotation deferred" line, and the rotation that follows writes no "rotation
// resumed" line an operator would read as one.
func TestIngressKeyPg_HealthyThrottledGraceSigningIsNotLoggedAsAStall(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	s := NewIngressKeyStore(pool, "holder", now)
	kid1, _, err := s.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	advance(ingressKeyRotation)
	if _, ok, err := s.VerificationKey("0123456789abcdef0123456789abcdef", now()); ok || err != nil {
		t.Fatalf("an unknown kid on a healthy store = ok:%v err:%v; want false,nil", ok, err)
	}
	if !s.throttled(now()) {
		t.Fatal("the miss did not arm the throttle: the row would not exercise the throttled branch")
	}
	kid, _, err := s.SigningKey(now())
	if err != nil || kid != kid1 {
		t.Fatalf("inside the throttle at the due age SigningKey = %q (err %v); want a grace signing with %q", kid, err, kid1)
	}
	advance(reloadThrottle)
	if err := s.refreshOnce(now()); err != nil {
		t.Fatal(err)
	}
	if s.signing == kid1 {
		t.Fatal("the refresh tick did not rotate")
	}
	if strings.Contains(buf.String(), "rotation deferred") || strings.Contains(buf.String(), "rotation resumed") {
		t.Fatalf("a healthy grace signing was logged as a stall:\n%s", buf.String())
	}
}

// errClass names a store error by kind only: the log line never carries a driver error's
// text, which can quote a host, a user or a statement.
func TestIngressKeyErrClassNamesTheKindNeverTheText(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("select: %w", context.DeadlineExceeded), "timeout"},
		{fmt.Errorf("select: %w", context.Canceled), "canceled"},
		{fmt.Errorf("insert: %w", &pgconn.PgError{Code: "25006", Message: "cannot execute INSERT in a read-only transaction"}), "postgres 25006"},
		{&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused host=db.internal user=gw")}, "unreachable"},
		// A connect failure that is not a net.Error (TLS, name resolution). pgx builds
		// these unexported-field errors itself; this one is never formatted.
		{&pgconn.ConnectError{Config: &pgconn.Config{}}, "unreachable"},
		{errors.New("password authentication failed for user gw"), "other"},
	} {
		if got := errClass(tc.err); got != tc.want {
			t.Errorf("errClass(%T) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// failingCreatorKeyCount counts the holder's rows.
func failingCreatorKeyCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM gw_ingress_key WHERE holder_id='holder'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The store reads but its creator transaction cannot begin (a read-only failover, a lock
// it cannot take): the same hour of grace as an unreachable store. The replica signs with
// the key it already holds, whether its own call runs the failing transaction or is
// throttled behind the refresh tick's, records every one of those signings in the
// deferred line (by error class, never the driver's text), changes nothing in the store,
// and answers the error first at exactly not_after - TTL.
func TestIngressKeyPg_CreatorTransactionFailingAtTheDueAgeGraceSignsThen503(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	db := &adoptionFaultDB{keyDB: pool}
	s := NewIngressKeyStore(db, "holder", now)
	kid1, _, err := s.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	advance(ingressKeyRotation)
	db.fault = "begin"
	stall := now()
	notAfter := signingNotAfter(t, s)
	signings, ticks := 0, 0
	var first503 time.Time
	for first503.IsZero() {
		if ticks > 200 {
			t.Fatal("no error within 200 refresh ticks of the stall")
		}
		// Unthrottled: this call runs the failing creator transaction itself.
		kid, _, err := s.SigningKey(now())
		if err != nil {
			first503 = now()
			break
		}
		if kid != kid1 {
			t.Fatalf("signed with %q during the stall, want the held key %q", kid, kid1)
		}
		signings++
		// The refresh tick fails the same way; the calls behind it are throttled.
		if err := s.refreshOnce(now()); err == nil {
			t.Fatal("refresh succeeded with the creator transaction failing")
		}
		for range 2 {
			if kid, _, err := s.SigningKey(now()); err != nil || kid != kid1 {
				t.Fatalf("throttled signing during the stall = %q (err %v), want %q", kid, err, kid1)
			}
			signings++
		}
		ticks++
		advance(ingressKeyRefresh)
	}
	if want := notAfter.Add(-engine.IngressBearerTTL); !first503.Equal(want) {
		t.Fatalf("first error %s after the stall, want exactly not_after - TTL (%s after)", first503.Sub(stall), want.Sub(stall))
	}
	if first503.Sub(stall) < time.Hour {
		t.Fatalf("the stall showed only %s before the first 503", first503.Sub(stall))
	}
	if n := failingCreatorKeyCount(t, pool); n != 1 {
		t.Fatalf("rows = %d, want 1: a failed creator transaction changes nothing", n)
	}
	deferred, _, counted := deferredLines(buf.String())
	if deferred != ticks {
		t.Fatalf("%d refresh ticks of the stall wrote %d deferred lines, want %d; log:\n%s", ticks, deferred, ticks, buf.String())
	}
	if counted > signings || counted < signings-3 {
		t.Fatalf("the deferred lines count %d of %d signings; only the last tick's may be outstanding", counted, signings)
	}
	if strings.Contains(buf.String(), errAdoptionFault.Error()) || !strings.Contains(buf.String(), "store error: other") {
		t.Fatalf("the deferred line must name the error class, never the error text; log:\n%s", buf.String())
	}
}

// A creator transaction whose commit fails, or whose commit lands but whose answer is
// lost: the replica grace-signs with the key it holds and publishes nothing; the next
// healthy tick converges every replica on one new key (the lost commit's own row, adopted,
// when it landed). The run's failure is cleared with it, so a healthy throttle window at
// the next due age is not logged as a stall.
func TestIngressKeyPg_CreatorCommitFailingGraceSignsAndTheNextTickConverges(t *testing.T) {
	for _, fault := range []string{"commit", "ack_lost"} {
		t.Run(fault, func(t *testing.T) {
			pool := testPool(t)
			now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
			db := &adoptionFaultDB{keyDB: pool}
			s := NewIngressKeyStore(db, "holder", now)
			kid1, _, err := s.SigningKey(now())
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			log.SetOutput(&buf)
			t.Cleanup(func() { log.SetOutput(os.Stderr) })

			advance(ingressKeyRotation)
			db.fault = fault
			kid, _, err := s.SigningKey(now())
			if err != nil || kid != kid1 {
				t.Fatalf("a failed commit at the due age = %q (err %v); want a grace signing with %q", kid, err, kid1)
			}
			if s.signing != kid1 {
				t.Fatalf("the failed transaction published %q", s.signing)
			}
			wantRows := 1
			if fault == "ack_lost" {
				wantRows = 2 // the commit landed; only its answer was lost
			}
			if n := failingCreatorKeyCount(t, pool); n != wantRows {
				t.Fatalf("rows after the failed commit = %d, want %d", n, wantRows)
			}

			db.fault = ""
			advance(ingressKeyRefresh)
			if err := s.refreshOnce(now()); err != nil {
				t.Fatal(err)
			}
			kid2 := s.signing
			if kid2 == kid1 {
				t.Fatal("the next healthy tick did not rotate")
			}
			sibling := NewIngressKeyStore(pool, "holder", now)
			if got, _, err := sibling.SigningKey(now()); err != nil || got != kid2 {
				t.Fatalf("a sibling signs %q (err %v), want the converged key %q", got, err, kid2)
			}
			if n := failingCreatorKeyCount(t, pool); n != 2 {
				t.Fatalf("rows after convergence = %d, want the previous key and one new key", n)
			}
			// The stall's one grace signing was its first deferral: logged at once,
			// so nothing is left for a resumed line.
			if deferred, resumed, counted := deferredLines(buf.String()); deferred != 1 || resumed != 0 || counted != 1 {
				t.Fatalf("the grace signing must be accounted for once: %d deferred and %d resumed lines counting %d; log:\n%s", deferred, resumed, counted, buf.String())
			}

			buf.Reset()
			advance(ingressKeyRotation)
			if _, ok, err := s.VerificationKey("0123456789abcdef0123456789abcdef", now()); ok || err != nil {
				t.Fatalf("an unknown kid on a healthy store = ok:%v err:%v", ok, err)
			}
			if got, _, err := s.SigningKey(now()); err != nil || got != kid2 {
				t.Fatalf("a healthy throttled signing at the next due age = %q (err %v), want %q", got, err, kid2)
			}
			if strings.Contains(buf.String(), "rotation deferred") {
				t.Fatalf("a cleared creator failure was logged as a new stall:\n%s", buf.String())
			}
		})
	}
}

// A replica that starts with the creator transaction failing has never signed: there is
// no key it may bridge with, so it answers the error (503), even though its reload loaded
// the holder's previous key, and the store is unchanged.
func TestIngressKeyPg_ColdReplicaWithTheCreatorTransactionFailingAnswersTheError(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	if _, _, err := NewIngressKeyStore(pool, "holder", now).SigningKey(now()); err != nil {
		t.Fatal(err)
	}
	advance(ingressKeyRotation)
	db := &adoptionFaultDB{keyDB: pool, fault: "begin"}
	cold := NewIngressKeyStore(db, "holder", now)
	if kid, key, err := cold.SigningKey(now()); err == nil || kid != "" || key != nil {
		t.Fatalf("a cold replica with the creator transaction failing = %q, %v, %v; want the store error", kid, key != nil, err)
	}
	if n := failingCreatorKeyCount(t, pool); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
}

// Writes failing under a flood of unknown kids: every miss reload succeeds and arms the
// throttle, so no signing runs the creator transaction itself; only the refresh tick does,
// and fails. Its failure is what tells the throttled signings this is a stall, so the
// deferred line still comes once per tick.
func TestIngressKeyPg_CreatorFailingUnderAMissFloodStillLogsOncePerRefreshTick(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	db := &adoptionFaultDB{keyDB: pool}
	s := NewIngressKeyStore(db, "holder", now)
	if _, _, err := s.SigningKey(now()); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	advance(ingressKeyRotation)
	db.fault = "begin"
	const ticks = 3
	perTick := int(ingressKeyRefresh / time.Second)
	for i := range ticks * perTick {
		if i%perTick == 0 {
			if err := s.refreshOnce(now()); err == nil {
				t.Fatal("refresh succeeded with the creator transaction failing")
			}
			advance(reloadThrottle)
		}
		if _, ok, err := s.VerificationKey(fmt.Sprintf("%032x", i+1), now()); ok || err != nil {
			t.Fatalf("an unknown kid with reads healthy = ok:%v err:%v; want false,nil", ok, err)
		}
		if !s.throttled(now()) {
			t.Fatal("the miss did not arm the throttle")
		}
		if _, _, err := s.SigningKey(now()); err != nil {
			t.Fatal(err)
		}
		advance(time.Second)
	}
	if deferred, _, _ := deferredLines(buf.String()); deferred != ticks {
		t.Fatalf("a flooded write-failure stall of %d ticks wrote %d deferred lines, want %d; log:\n%s", ticks, deferred, ticks, buf.String())
	}
}
