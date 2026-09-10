package pgstore

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
)

func fixedClock(t0 time.Time) (func() time.Time, func(time.Duration)) {
	cur := t0
	return func() time.Time { return cur }, func(d time.Duration) { cur = cur.Add(d) }
}

// check runs one CheckAndRecord on a HEALTHY store: every parity row below expects the
// store to be able to tell, so a non-nil err there is a test failure, not an outcome.
// The unknown-scope row asserts the error itself and calls CheckAndRecord directly.
func check(t *testing.T, s engine.ReplayStore, scope, clientID, key string, now, expiresAt time.Time) bool {
	t.Helper()
	replay, err := s.CheckAndRecord(scope, clientID, key, now, expiresAt)
	if err != nil {
		t.Fatalf("CheckAndRecord(%s, %q, %q): unexpected store error %v", scope, clientID, key, err)
	}
	return replay
}

// replayRows is the mem↔pg parity table: every row must hold on BOTH backends
// (the mirror may be neither stricter nor looser than the oracle).
//
// Both backends store the caller's expiresAt and honour it with the SAME strict rule
// (expires_at < now ⇒ the record is re-armed), so the window here is the table's own
// choice — w is the engine's ingressJTIWindow only because that is what the ingress call
// site passes. Neither store carries a window constant of its own to drift from the
// other's; the exact-boundary row is the pin on the comparison itself.
func replayRows(t *testing.T, mk func() engine.ReplayStore) {
	t0 := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	w := 5 * time.Minute // == engine ingressJTIWindow (unexported); see the doc comment
	t.Run("first use then replay", func(t *testing.T) {
		s := mk()
		if check(t, s, engine.ReplayScopeIngressJTI, "c", "j1", t0, t0.Add(w)) {
			t.Fatal("first use reported as replay")
		}
		if !check(t, s, engine.ReplayScopeIngressJTI, "c", "j1", t0.Add(time.Second), t0.Add(w)) {
			t.Fatal("second use not a replay")
		}
	})
	t.Run("a key past the length bound is refused, and says why", func(t *testing.T) {
		// The key is caller-supplied and part of the durable record's primary key, so an
		// oversized value overflows the btree index row on a HEALTHY database — a denial
		// that would read as an outage. Both backends refuse it before recording anything
		// (mirror == oracle in both directions), and a key AT the bound still records.
		s := mk()
		over := strings.Repeat("j", engine.MaxReplayKeyBytes+1)
		replay, err := s.CheckAndRecord(engine.ReplayScopeIngressJTI, "c", over, t0, t0.Add(w))
		if !replay {
			t.Fatal("an oversized key must fail closed (replay=true)")
		}
		if err == nil {
			t.Fatal("an oversized key must report WHY it was refused, not pass as a plain replay")
		}
		if check(t, s, engine.ReplayScopeIngressJTI, "c", strings.Repeat("j", engine.MaxReplayKeyBytes), t0, t0.Add(w)) {
			t.Fatal("a key AT the bound is a first use, not a replay")
		}
	})
	t.Run("scope and client are structural", func(t *testing.T) {
		s := mk()
		if check(t, s, engine.ReplayScopeHubJTI, "", "k", t0, t0.Add(w)) || check(t, s, engine.ReplayScopePatientAccess, "", "k", t0, t0.Add(w)) {
			t.Fatal("scopes share records")
		}
		if check(t, s, engine.ReplayScopeIngressJTI, "a", "j", t0, t0.Add(w)) || check(t, s, engine.ReplayScopeIngressJTI, "b", "j", t0, t0.Add(w)) {
			t.Fatal("clients share jti records")
		}
		if !check(t, s, engine.ReplayScopeIngressJTI, "a", "j", t0, t0.Add(w)) {
			t.Fatal("same (client, jti) twice is not a replay")
		}
		if check(t, s, engine.ReplayScopeIngressJTI, "ab", "c", t0, t0.Add(w)) || check(t, s, engine.ReplayScopeIngressJTI, "a", "bc", t0, t0.Add(w)) {
			t.Fatal("client/key boundary confused")
		}
	})
	t.Run("exact window boundary is still a replay", func(t *testing.T) {
		// shnsdk.ReplayGuard: now.Sub(seen) <= window ⇒ replay, so at exactly seen+window
		// the key is still spent; the next instant it is a first use. pg must match:
		// re-arm and purge use expires_at < now, never <=. The store contract is
		// MICROSECOND-granular: pgx encodes TIMESTAMPTZ at microsecond precision, so a
		// nanosecond step would round back onto the boundary on pg — step by 1µs.
		s := mk()
		check(t, s, engine.ReplayScopeIngressJTI, "c", "edge", t0, t0.Add(w))
		if !check(t, s, engine.ReplayScopeIngressJTI, "c", "edge", t0.Add(w), t0.Add(2*w)) {
			t.Fatal("at exactly now+window the key must still be a replay")
		}
		if check(t, s, engine.ReplayScopeIngressJTI, "c", "edge", t0.Add(w).Add(time.Microsecond), t0.Add(2*w)) {
			t.Fatal("one microsecond past the window the key must be a first use")
		}
	})
	t.Run("expired row re-armed without a purge", func(t *testing.T) {
		s := mk()
		before := purgeStamp(s)
		check(t, s, engine.ReplayScopeIngressJTI, "c", "j", t0, t0.Add(w))
		if check(t, s, engine.ReplayScopeIngressJTI, "c", "j", t0.Add(w).Add(time.Second), t0.Add(2*w)) {
			t.Fatal("expired key must be a first use again")
		}
		if !check(t, s, engine.ReplayScopeIngressJTI, "c", "j", t0.Add(w).Add(2*time.Second), t0.Add(2*w)) {
			t.Fatal("re-armed key must then be a replay")
		}
		// "without a purge" is the point of the row: the re-arm must come from the
		// ON CONFLICT … DO UPDATE branch, not from the sweep having deleted the row
		// first. Without this the row would pass either way.
		if got := purgeStamp(s); got != before {
			t.Fatalf("a purge ran (lastPurge %v → %v); the re-arm must be in place, not swept", before, got)
		}
	})
	t.Run("past the retired guard's cap nothing unexpired is shed", func(t *testing.T) {
		// The in-memory mirror used to be shnsdk.ReplayGuard, whose cap (4096 for the
		// ingress-jti scope) sheds an ARBITRARY UNEXPIRED entry to stay bounded. gw_replay
		// has no cap at all, so the 4097th distinct jti inside one window made the mirror
		// LOOSER than the oracle in exactly the direction that matters: a jti already spent
		// came back as a first use. Both backends now evict only what has EXPIRED.
		//
		// n is one past the retired cap and is deliberately the SAME on both backends —
		// a smaller pg half would compare unlike with unlike and prove nothing about the
		// count that used to break. Each key is one INSERT on pg; see the report for the
		// measured duration.
		const n = 4097
		s := mk()
		for i := 0; i < n; i++ {
			if check(t, s, engine.ReplayScopeIngressJTI, "c", fmt.Sprintf("j-%d", i), t0, t0.Add(w)) {
				t.Fatalf("first use of distinct key %d reported as replay", i)
			}
		}
		// (a) EVERY key re-presented is still spent. Re-presenting only the first would
		// not be a test: the retired guard shed exactly ONE arbitrary entry at its cap, so
		// a single probe caught it 1 time in 4096. Counting the whole set is deterministic.
		shed := 0
		for i := 0; i < n; i++ {
			if !check(t, s, engine.ReplayScopeIngressJTI, "c", fmt.Sprintf("j-%d", i), t0.Add(time.Second), t0.Add(w)) {
				shed++
			}
		}
		if shed != 0 {
			t.Fatalf("%d of %d unexpired keys came back as a first use: the mirror is looser than the oracle", shed, n)
		}
		// (b) a 4098th FRESH key is still a first use (the ceiling is nowhere near here).
		if check(t, s, engine.ReplayScopeIngressJTI, "c", "j-fresh", t0.Add(time.Second), t0.Add(w)) {
			t.Fatal("a fresh key past the retired cap reported as replay")
		}
		// (c) keys past their expiresAt are re-armed on both backends, so the set that
		// grew here does not grow for ever: expiry is the ONLY thing that evicts.
		past := t0.Add(w).Add(time.Microsecond)
		if check(t, s, engine.ReplayScopeIngressJTI, "c", "j-0", past, past.Add(w)) {
			t.Fatal("a key past its expiresAt must be a first use again")
		}
		if !check(t, s, engine.ReplayScopeIngressJTI, "c", "j-0", past.Add(time.Second), past.Add(w)) {
			t.Fatal("the re-armed key must then be a replay again")
		}
	})
	t.Run("unknown scope fails closed and names itself", func(t *testing.T) {
		// The mirror has one guard per scope and answers replay for anything else;
		// the oracle must not be looser, so it refuses the same string. A scope the
		// engine never emits is a PROGRAMMING error, not a replay: both backends
		// report replay=true (fail closed) AND an error naming the scope, so the
		// caller can answer "unavailable" rather than "you replayed that".
		s := mk()
		replay, err := s.CheckAndRecord("no-such-scope", "", "k", t0, t0.Add(w))
		if !replay {
			t.Fatal("unknown scope must report replay (fail closed)")
		}
		if err == nil {
			t.Fatal("unknown scope must also report an error (it is a programming error, not a replay)")
		}
		if !strings.Contains(err.Error(), "no-such-scope") {
			t.Fatalf("error %v does not name the scope", err)
		}
	})
	t.Run("racers on a fresh key: exactly one first use", func(t *testing.T) {
		s := mk()
		first := raceCheck(s, "fresh", t0, t0.Add(w))
		if first != 1 {
			t.Fatalf("first uses = %d, want 1", first)
		}
	})
	t.Run("racers on an expired row: exactly one re-arm", func(t *testing.T) {
		s := mk()
		check(t, s, engine.ReplayScopeIngressJTI, "c", "stale", t0, t0.Add(w))
		first := raceCheck(s, "stale", t0.Add(w).Add(time.Second), t0.Add(2*w))
		if first != 1 {
			t.Fatalf("re-arms = %d, want 1", first)
		}
	})
}

// purgeStamp reads the PG store's purge-throttle stamp under its own lock. The mirror
// has a sweep of its own, but it is not reachable through this interface and is pinned
// beside it (engine TestMemReplayStore_PurgeThrottledAndNeverLoadBearing); here the
// mirror reports the zero time both times, so the row's no-purge assertion is about the
// oracle's ON CONFLICT branch, which is the branch that could be faked by a sweep.
func purgeStamp(s engine.ReplayStore) time.Time {
	ps, ok := s.(*ReplayStore)
	if !ok {
		return time.Time{}
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.lastPurge
}

func raceCheck(s engine.ReplayStore, key string, now, exp time.Time) int {
	var wg sync.WaitGroup
	var mu sync.Mutex
	first := 0
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A store error counts as a replay here (fail closed), never as a first use.
			replay, err := s.CheckAndRecord(engine.ReplayScopeIngressJTI, "c", key, now, exp)
			if !replay && err == nil {
				mu.Lock()
				first++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return first
}

func TestReplayParity_Mem(t *testing.T) {
	replayRows(t, func() engine.ReplayStore { return engine.NewInMemoryReplayStore() })
}

func TestReplayParity_Pg(t *testing.T) {
	pool := testPool(t)
	n := 0
	replayRows(t, func() engine.ReplayStore {
		n++
		// a distinct holder per row keeps rows independent without re-creating the schema
		return NewReplayStore(pool, "h-"+string(rune('a'+n)), time.Now)
	})
}

// All three guards (client_assertion jti, Hub assertion jti, patient-access correlationId)
// are one-time-use ACROSS replicas: spent at A, refused at B. The engine-level version
// of the same fact for the ingress scope is gateway/app TestPgMulti_AssertionReplayedAtBIs401;
// the Hub and patient-access scopes reach the store through the engine's
// two-instance hermetic rows (replay_shared_test.go).
func TestReplayPg_SharedAcrossInstances(t *testing.T) {
	pool := testPool(t)
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	a := NewReplayStore(pool, "holder", time.Now)
	b := NewReplayStore(pool, "holder", time.Now)
	other := NewReplayStore(pool, "other", time.Now)
	for _, tc := range []struct{ scope, client string }{
		{engine.ReplayScopeIngressJTI, "br-provider"},
		{engine.ReplayScopeHubJTI, ""},
		{engine.ReplayScopePatientAccess, ""},
	} {
		if check(t, a, tc.scope, tc.client, "key-1", now, now.Add(time.Hour)) {
			t.Fatalf("%s: A first use reported as replay", tc.scope)
		}
		if !check(t, b, tc.scope, tc.client, "key-1", now, now.Add(time.Hour)) {
			t.Fatalf("%s: B must see A's record", tc.scope)
		}
		// Holder scoping: another holder's identical key is a first use.
		if check(t, other, tc.scope, tc.client, "key-1", now, now.Add(time.Hour)) {
			t.Fatalf("%s: records leaked across holders", tc.scope)
		}
	}
}

func TestReplayPg_DBErrorFailsClosed(t *testing.T) {
	pool := testPool(t)
	s := NewReplayStore(pool, "holder", time.Now)
	pool.Close()
	now := time.Now()
	replay, err := s.CheckAndRecord(engine.ReplayScopeIngressJTI, "c", "j", now, now.Add(time.Minute))
	if !replay {
		t.Fatal("closed pool must report replay (fail closed)")
	}
	// …and it must SAY it could not tell, so the token endpoint answers a retryable
	// 503 instead of a 401 that reads as a bad credential.
	if err == nil {
		t.Fatal("closed pool must report a store error alongside the replay")
	}
}

func TestReplayPg_PurgeThrottledAndNeverLoadBearing(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	s := NewReplayStore(pool, "holder", now)
	check(t, s, engine.ReplayScopeIngressJTI, "c", "old", now(), now().Add(time.Minute))
	advance(2 * time.Minute)
	check(t, s, engine.ReplayScopeIngressJTI, "c", "new", now(), now().Add(time.Minute)) // ≥1 min since construction → purge runs
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM gw_replay WHERE holder_id='holder' AND key='old'`).Scan(&n); err != nil {
		t.Fatalf("count old: %v", err) // a discarded error would leave n==0 and pass vacuously
	}
	if n != 0 {
		t.Fatal("expired row not purged")
	}
	advance(30 * time.Second)
	check(t, s, engine.ReplayScopeIngressJTI, "c", "new2", now(), now().Add(time.Minute))
	if s.lastPurge != now().Add(-30*time.Second) {
		t.Fatalf("purge ran inside the 1-minute throttle (lastPurge=%v)", s.lastPurge)
	}
}
