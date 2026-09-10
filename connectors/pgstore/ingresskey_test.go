package pgstore

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
)

func TestIngressKeyPg_SigningKeyPersistsAndIsSharedAcrossInstances(t *testing.T) {
	pool := testPool(t)
	now, _ := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	a := NewIngressKeyStore(pool, "holder", now)
	b := NewIngressKeyStore(pool, "holder", now)
	kid, key, err := a.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	// B verifies A's kid (miss → one reload → hit).
	pub, ok, err := b.VerificationKey(kid, now())
	if !ok || err != nil || !pub.Equal(&key.PublicKey) {
		t.Fatalf("B must resolve A's kid to A's public key (ok=%v err=%v)", ok, err)
	}
	// B signs with the SAME key (it adopts the live row rather than minting a second).
	kidB, keyB, err := b.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	if kidB != kid || !keyB.Equal(key) {
		t.Fatal("B must adopt the live signing key, not mint its own")
	}
	var _ engine.IngressKeyStore = a
}

func TestIngressKeyPg_RotatesAfterIntervalAndOldKidStillVerifies(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	s := NewIngressKeyStore(pool, "holder", now)
	kid1, _, err := s.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	advance(ingressKeyRotation - time.Second)
	k, _, err := s.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	if k != kid1 {
		t.Fatal("rotated early")
	}
	advance(time.Second)
	kid2, _, err := s.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	if kid2 == kid1 {
		t.Fatal("did not rotate at the interval")
	}
	// kid1 stays verifiable until its not_after (rotation + bearer TTL + 1 min skew).
	if _, ok, err := s.VerificationKey(kid1, now()); !ok || err != nil {
		t.Fatalf("previous key must verify right after rotation (ok=%v err=%v)", ok, err)
	}
	advance(engine.IngressBearerTTL + time.Minute)
	if _, ok, err := s.VerificationKey(kid1, now()); ok || err != nil {
		t.Fatalf("previous key past its not_after = ok:%v err:%v; want false,nil", ok, err)
	}
	// A third rotation deletes the expired row.
	advance(ingressKeyRotation)
	if _, _, err := s.SigningKey(now()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM gw_ingress_key WHERE holder_id='holder' AND kid=$1`, kid1).Scan(&n); err != nil {
		t.Fatalf("counting rows for the expired kid: %v", err)
	}
	if n != 0 {
		t.Fatal("expired key row not deleted on rotation")
	}
}

func TestIngressKeyPg_UnknownKidNegativeCachedOnlyAfterReloadRan(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	s := NewIngressKeyStore(pool, "holder", now)
	if _, _, err := s.SigningKey(now()); err != nil {
		t.Fatal(err)
	}
	advance(reloadThrottle) // SigningKey's own reload stamped lastReload; leave its window so the miss reloads
	unknown := "0123456789abcdef0123456789abcdef"
	if _, ok, err := s.VerificationKey(unknown, now()); ok || err != nil {
		t.Fatalf("unknown kid = ok:%v err:%v; want false,nil", ok, err)
	}
	if _, neg := s.negCache[unknown]; !neg {
		t.Fatal("kid unknown after a completed reload must be negatively cached")
	}
	// A second unknown kid within the same second is answered from the snapshot in hand
	// and is NOT negatively cached: no reload of this caller's own ran, and a snapshot
	// somebody else took is not evidence the kid does not exist.
	unknown2 := "fedcba9876543210fedcba9876543210"
	if _, ok, err := s.VerificationKey(unknown2, now()); ok || err != nil {
		t.Fatalf("miss inside the throttle = ok:%v err:%v; want false,nil (the last completed reload succeeded)", ok, err)
	}
	if _, neg := s.negCache[unknown2]; neg {
		t.Fatal("a miss that ran no reload of its own must NOT be negatively cached")
	}
	// Negative entries expire after negCacheTTL.
	advance(negCacheTTL + time.Second)
	s.VerificationKey("00000000000000000000000000000000", now()) // triggers a reload that prunes
	if _, neg := s.negCache[unknown]; neg {
		t.Fatal("negative cache entry outlived negCacheTTL")
	}
}

// countingDB counts pool and transaction calls, so a verification path cannot hide
// database work inside a transaction when its reload budget is exhausted.
type countingDB struct {
	keyDB
	queries int32
	reloads int32
}

func (c *countingDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	atomic.AddInt32(&c.queries, 1)
	atomic.AddInt32(&c.reloads, 1)
	return c.keyDB.Query(ctx, sql, args...)
}

func (c *countingDB) Begin(ctx context.Context) (pgx.Tx, error) {
	atomic.AddInt32(&c.queries, 1)
	tx, err := c.keyDB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &countingKeyTx{Tx: tx, counter: &c.queries}, nil
}

type countingKeyTx struct {
	pgx.Tx
	counter *int32
}

func (tx *countingKeyTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	atomic.AddInt32(tx.counter, 1)
	return tx.Tx.Query(ctx, sql, args...)
}
func (tx *countingKeyTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	atomic.AddInt32(tx.counter, 1)
	return tx.Tx.Exec(ctx, sql, args...)
}
func (tx *countingKeyTx) Commit(ctx context.Context) error {
	atomic.AddInt32(tx.counter, 1)
	return tx.Tx.Commit(ctx)
}
func (tx *countingKeyTx) Rollback(ctx context.Context) error {
	atomic.AddInt32(tx.counter, 1)
	return tx.Tx.Rollback(ctx)
}

func TestIngressKeyPg_HundredUnknownKidsOneReloadQuery(t *testing.T) {
	db := &countingDB{keyDB: testPool(t)}
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	s := NewIngressKeyStore(db, "holder", now)
	if _, _, err := s.SigningKey(now()); err != nil {
		t.Fatal(err)
	}
	// The clock is frozen, so every miss after the first lands inside the same throttle
	// window. None of them waits for it: the flood's whole cost is what this row counts.
	advance(reloadThrottle) // leave SigningKey's own reload window so the first miss reloads
	atomic.StoreInt32(&db.queries, 0)
	for i := 0; i < 100; i++ {
		kid := fmt.Sprintf("%032x", i+1)
		if _, ok, err := s.VerificationKey(kid, now()); ok || err != nil {
			t.Fatalf("unknown kid %s = ok:%v err:%v; want false,nil", kid, ok, err)
		}
	}
	if got := atomic.LoadInt32(&db.queries); got != 1 {
		t.Fatalf("100 unknown kids within one second cost %d reload queries, want exactly 1", got)
	}
	if len(s.negCache) != 1 {
		t.Fatalf("only the kid that missed AFTER a reload is negatively cached; got %d entries", len(s.negCache))
	}
}

// A miss inside the reload throttle answers IMMEDIATELY and the reload it asked for runs
// when the throttle allows. It does not park: validKID admits any 32-hex string and jwt/v5
// runs the keyfunc before it checks the signature, so an unauthenticated caller can mint
// unknown kids as fast as it likes, and any wait on this path is a goroutine that caller
// gets to hold. The price is named in DEPLOYMENT.md and pinned here: a sibling's kid minted
// inside the window is refused ONCE and verifies on the next call.
//
// WARM is the fixture, and it is load-bearing: the throttle governs the miss path of a
// replica that HAS a snapshot — one on which a reload has completed successfully, an empty
// one included (TestIngressKeyPg_EmptyFirstLoadWarmsTheReplicaAndASiblingsFirstKeyVerifiesAtTheNextSlot)
// — where deferring costs one refusal inside a second. A COLD replica has no snapshot and
// parks for a reload younger than the request instead (the cold rows below); the pair is
// the whole contract.
func TestIngressKeyPg_ThrottledMissAnswersImmediatelyAndReloadsWhenTheThrottleAllows(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	db := &countingDB{keyDB: pool}
	a := NewIngressKeyStore(pool, "holder", now)
	b := NewIngressKeyStore(db, "holder", now)

	// B mints the holder's first key, so its cache has been READ from here on.
	kidB, _, err := b.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	// Past that key's rotation interval, so A mints a SECOND kid below rather than
	// adopting B's; B's own key stays inside its not_after, so B is a warm replica missing
	// exactly one sibling kid. The mint's own reload is not what this row counts.
	advance(ingressKeyRotation)
	atomic.StoreInt32(&db.queries, 0)
	if b.cold() {
		t.Fatal("the fixture is not a warm replica")
	}

	// B spends its 1 s reload budget on an unknown kid FIRST: the throttle is now armed.
	if _, ok, err := b.VerificationKey("fedcba9876543210fedcba9876543210", now()); ok || err != nil {
		t.Fatalf("unknown kid = ok:%v err:%v; want false,nil", ok, err)
	}
	// A rotates INSIDE that window — the bearer a partner presents to B milliseconds
	// later carries this kid.
	kid, key, err := a.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	if kid == kidB {
		t.Fatal("A adopted B's key instead of rotating: the row has no sibling kid to miss")
	}
	// One refusal, immediately, with no second query: the throttle is what the replica is
	// protecting, and the caller is not made to hold a goroutine while it waits it out.
	if _, ok, err := b.VerificationKey(kid, now()); ok || err != nil {
		t.Fatalf("a sibling's fresh kid inside the throttle = ok:%v err:%v; want false,nil answered at once", ok, err)
	}
	if got := atomic.LoadInt32(&db.queries); got != 1 {
		t.Fatalf("reload queries inside the throttle window = %d, want 1 (the miss must not query around the throttle either)", got)
	}
	// It must NOT be negatively cached: no reload of B's own ran, and a snapshot taken
	// before A's INSERT is not evidence the kid does not exist. Caching it here would make
	// the refusal last negCacheTTL instead of one throttle window.
	if _, neg := b.negCache[kid]; neg {
		t.Fatal("a kid refused inside the throttle was negatively cached: the refusal now outlives the window")
	}
	// Once the throttle allows a reload, the very next call resolves it.
	advance(reloadThrottle)
	pub, ok, err := b.VerificationKey(kid, now())
	if !ok || err != nil {
		t.Fatalf("the sibling's kid on the next call = ok:%v err:%v; want it resolved", ok, err)
	}
	if !pub.Equal(&key.PublicKey) {
		t.Fatal("resolved the wrong public key")
	}
	if got := atomic.LoadInt32(&db.queries); got != 2 {
		t.Fatalf("reload queries = %d, want 2 (the first miss, then the one the next slot allowed)", got)
	}
}

// Rotation is traffic-independent. Rotation in the request path is throttled like every
// miss, so one garbage bearer per second (a distinct unknown kid each time: each miss
// reloads, re-stamping lastReload) keeps every SigningKey slow path inside the throttle
// and on the grace branch. Grace is a bridge, not the mechanism: refreshOnce (RunRefresh,
// every ingressKeyRefresh) inserts the new key when its reload finds none inside the
// rotation interval, so the flood cannot starve rotation past the grace and the token
// endpoint never answers 503 for it. Two replicas may both insert in one tick — harmless,
// both verify, newest wins on the next reload.
func TestIngressKeyPg_RefreshRotatesWhileAnUnknownKidFloodKeepsTheThrottleArmed(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	s := NewIngressKeyStore(pool, "holder", now)
	kid1, _, err := s.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	advance(ingressKeyRotation) // rotation due from here on
	refreshEvery := int(ingressKeyRefresh / time.Second)
	for sec := 1; sec <= int((engine.IngressBearerTTL+2*time.Minute)/time.Second); sec++ {
		advance(time.Second)
		if _, ok, err := s.VerificationKey(fmt.Sprintf("%032x", sec), now()); ok || err != nil {
			t.Fatalf("an unknown kid at +%ds = ok:%v err:%v; want false,nil", sec, ok, err)
		}
		if sec%refreshEvery == 0 {
			if err := s.refreshOnce(now()); err != nil {
				t.Fatalf("refreshOnce at +%ds: %v", sec, err)
			}
		}
		kid, _, err := s.SigningKey(now())
		if err != nil {
			t.Fatalf("SigningKey at +%ds under the flood: %v (rotation starved past the grace)", sec, err)
		}
		if sec < refreshEvery && kid != kid1 {
			t.Fatalf("before the first refresh the throttled slow path signs with the current key inside its slack, got %s", kid)
		}
		if sec >= refreshEvery && kid == kid1 {
			t.Fatalf("refreshOnce at +%ds did not rotate: still signing with %s", sec, kid1)
		}
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM gw_ingress_key WHERE holder_id='holder' AND created_at > $1`, now().Add(-ingressKeyRotation)).Scan(&n); err != nil {
		t.Fatalf("counting the live key rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("one process rotating once must insert exactly one live key, got %d", n)
	}
}

func TestIngressKeyPg_RefreshOnceMakesFreshKeyVisibleWithoutAMiss(t *testing.T) {
	pool := testPool(t)
	now, _ := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	a := NewIngressKeyStore(pool, "holder", now)
	b := NewIngressKeyStore(pool, "holder", now)
	kid, _, err := a.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	if err := b.refreshOnce(now()); err != nil {
		t.Fatal(err)
	}
	if _, cached := b.keys[kid]; !cached {
		t.Fatal("refreshOnce did not load the live key into the positive cache")
	}
	// RunRefresh returns promptly on ctx cancellation (no ticker leak).
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.RunRefresh(ctx, nil); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunRefresh did not return on ctx.Done")
	}
}

// A database this replica cannot read makes a kid UNRESOLVABLE, which is not the same
// thing as unknown: the store says so (the third return), and the caller answers a
// retryable 503 instead of a 401 that tells an honest partner its credential is bad.
func TestIngressKeyPg_DBErrorIsReportedAsUnavailableNotUnknown(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	db := &countingDB{keyDB: pool}
	s := NewIngressKeyStore(db, "holder", now)
	kid, _, err := s.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	// Cached kid still verifies, and reports no outage (the positive cache needs no DB)…
	if _, ok, err := s.VerificationKey(kid, now()); !ok || err != nil {
		t.Fatalf("cached kid with the pool closed = ok:%v err:%v; want true,nil", ok, err)
	}
	// …an unknown kid is rejected WITH the store error and is NOT negatively cached,
	// because the reload ERRORED (not because it was throttled — leave SigningKey's
	// reload window first, so the closed pool is really touched)…
	advance(reloadThrottle)
	atomic.StoreInt32(&db.queries, 0)
	unknown := "0123456789abcdef0123456789abcdef"
	if _, ok, err := s.VerificationKey(unknown, now()); ok || err == nil {
		t.Fatalf("unknown kid over a dead database = ok:%v err:%v; want false and a non-nil error (the caller owes a 503, not a 401)", ok, err)
	}
	if _, neg := s.negCache[unknown]; neg {
		t.Fatal("errored reload must not negatively cache")
	}
	if s.lastReload != now() {
		t.Fatal("an errored reload must still stamp lastReload (a hung DB is throttled like a healthy one)")
	}
	if got := atomic.LoadInt32(&db.queries); got != 1 {
		t.Fatalf("queries after the first miss = %d, want 1", got)
	}
	// …and a second unknown kid inside that same window issues NO query and still reports
	// the outage: a snapshot taken by a failed reload cannot tell unknown from unreadable.
	if _, ok, err := s.VerificationKey("fedcba9876543210fedcba9876543210", now()); ok || err == nil {
		t.Fatalf("second miss inside the window = ok:%v err:%v; want false and the outage error", ok, err)
	}
	if got := atomic.LoadInt32(&db.queries); got != 1 {
		t.Fatalf("queries after the second miss = %d, want 1 (the throttle still admits one reload per second)", got)
	}
	// …and a store that never loaded a key cannot sign.
	fresh := NewIngressKeyStore(pool, "holder", now)
	if _, _, err := fresh.SigningKey(now()); err == nil {
		t.Fatal("SigningKey over a closed pool must error")
	}
}

// Rotation is due but the DB is unreachable: a bearer minted now with the current key
// stays verifiable everywhere while now+IngressBearerTTL is inside that key's not_after
// (the 1-minute slack past rotation+TTL), so the store keeps signing with it instead of
// answering 503 for a blip; past the slack the error surfaces.
func TestIngressKeyPg_RotationDueOnDBErrorSignsWithCurrentKeyInsideSlack(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	s := NewIngressKeyStore(pool, "holder", now)
	kid1, _, err := s.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	advance(ingressKeyRotation) // rotation due
	pool.Close()
	kid, _, err := s.SigningKey(now())
	if err != nil || kid != kid1 {
		t.Fatalf("inside the slack the current key must still sign: kid=%q err=%v", kid, err)
	}
	advance(time.Minute) // now + IngressBearerTTL == not_after: a bearer minted now could outlive the key
	if _, _, err := s.SigningKey(now()); err == nil {
		t.Fatal("past the slack SigningKey must error (the caller answers 503)")
	}
}

// Cached-kid verification never waits on Postgres: with the DB hung (a connection that
// never answers), a live kid still verifies while a reload is parked mid-query.
func TestIngressKeyPg_CachedKidVerifiesWhileTheDBHangs(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	hung := &hungDB{keyDB: pool, blocked: make(chan struct{}), gate: make(chan struct{})}
	t.Cleanup(hung.release) // never leave the parked reload behind, even on a failed row
	s := NewIngressKeyStore(hung, "holder", now)
	kid, _, err := s.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	hung.hang.Store(true)
	advance(reloadThrottle)
	missDone := make(chan struct{})
	go func() { s.VerificationKey("0123456789abcdef0123456789abcdef", now()); close(missDone) }() // parks in the hung query
	<-hung.blocked                                                                                // the reload holds reloadMu and only release() can free it
	// No timing anywhere: if the cache lock were held across the DB round trip this call
	// could not return until release(), so a regression shows up as a blocked test (a
	// go test timeout panic naming this line), never as a threshold that flakes on a
	// slow runner.
	if _, ok, err := s.VerificationKey(kid, now()); !ok || err != nil {
		t.Fatalf("cached kid while a reload is in flight = ok:%v err:%v; want true,nil", ok, err)
	}
	select {
	case <-missDone:
		t.Fatal("the parked reload returned before it was released: the row proved nothing")
	default: // still parked, so the cached kid really did resolve mid-reload
	}
	hung.release()
	<-missDone
}

// hungDB parks Query once hang is set; ONLY release() frees it (no ctx escape), so a row
// can assert what happens while a reload is in flight without depending on any duration.
type hungDB struct {
	keyDB
	hang        atomic.Bool
	blocked     chan struct{}
	gate        chan struct{}
	onceBlocked sync.Once
	onceRelease sync.Once
}

func (h *hungDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if h.hang.Load() {
		h.onceBlocked.Do(func() { close(h.blocked) })
		<-h.gate
	}
	return h.keyDB.Query(ctx, sql, args...)
}

func (h *hungDB) release() { h.onceRelease.Do(func() { close(h.gate) }) }

// The negative cache is bounded at negCacheMax entries, evicts in FIFO order, and does so
// in O(1) BY CONSTRUCTION: the ring slot about to be written names the oldest kid still
// holding a slot, so there is no scan for an oldest entry and no timestamp comparison.
// That matters twice over — the old scan was O(negCacheMax) on every eviction, and being
// seeded from a timestamp it evicted NOTHING when the entries shared an instant (a stalled
// or injected clock), letting the map grow without bound. Neither failure is reachable
// from a shape that never reads a clock.
//
// This row drives negCacheAddLocked under the store's own lock rather than through
// VerificationKey because the public path cannot reach the bound: a reload admits at
// most one negative entry (the kid that triggered it), reloads are throttled to one per
// second, and every reload prunes entries older than negCacheTTL — so VerificationKey
// alone holds at most negCacheTTL/reloadThrottle ≈ 60 entries. negCacheMax is the
// belt-and-braces bound that keeps the map finite if that relationship ever changes.
func TestIngressKeyPg_NegCacheBounded(t *testing.T) {
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	s := NewIngressKeyStore(nil, "holder", now) // no DB call on this path
	add := func(st *IngressKeyStore, kid string) {
		st.mu.Lock()
		defer st.mu.Unlock()
		st.negCacheAddLocked(kid, now())
	}
	size := func(st *IngressKeyStore) int {
		st.mu.RLock()
		defer st.mu.RUnlock()
		return len(st.negCache)
	}
	has := func(st *IngressKeyStore, kid string) bool {
		st.mu.RLock()
		defer st.mu.RUnlock()
		_, ok := st.negCache[kid]
		return ok
	}
	// Distinct instants: the map fills to the bound and the FIRST kid is evicted.
	for i := 0; i <= negCacheMax; i++ {
		add(s, fmt.Sprintf("%032x", i))
		advance(time.Millisecond)
	}
	if got := size(s); got != negCacheMax {
		t.Fatalf("negative cache holds %d entries, want the bound %d", got, negCacheMax)
	}
	if has(s, fmt.Sprintf("%032x", 0)) {
		t.Fatal("the oldest entry must be the one evicted")
	}
	// FIFO, not "some entry": #1 is the next-oldest and must still be there, and one more
	// insertion must take exactly it.
	if !has(s, fmt.Sprintf("%032x", 1)) {
		t.Fatal("eviction took an entry that was not the oldest: the order is not FIFO")
	}
	if !has(s, fmt.Sprintf("%032x", negCacheMax)) {
		t.Fatal("the newest entry must be kept")
	}
	add(s, "ffffffffffffffffffffffffffffffff")
	if has(s, fmt.Sprintf("%032x", 1)) {
		t.Fatal("the second insertion past the bound did not evict the second-oldest entry")
	}
	if got := size(s); got != negCacheMax {
		t.Fatalf("negative cache holds %d entries after a second eviction, want %d", got, negCacheMax)
	}
	// Equal instants: every entry stamped at the same time as the eviction (what a
	// stalled or injected clock produces) must still cost exactly one eviction. FIFO
	// order never reads the clock, so this is the same code path, not a special case.
	equal := NewIngressKeyStore(nil, "holder", now)
	for i := 0; i <= negCacheMax; i++ {
		add(equal, fmt.Sprintf("%032x", i))
	}
	if got := size(equal); got != negCacheMax {
		t.Fatalf("negative cache grew past its bound when every entry shares a timestamp: %d entries, want %d", got, negCacheMax)
	}
	if has(equal, fmt.Sprintf("%032x", 0)) {
		t.Fatal("with equal timestamps the oldest entry must still be the one evicted")
	}
	// A kid added twice must not consume a second slot — it would evict a live entry for
	// nothing and let the ring drift out of step with the map.
	dup := NewIngressKeyStore(nil, "holder", now)
	for i := 0; i < 4; i++ {
		add(dup, "0123456789abcdef0123456789abcdef")
	}
	if got := size(dup); got != 1 {
		t.Fatalf("re-adding one kid produced %d entries, want 1", got)
	}
	if dup.negNext != 1 {
		t.Fatalf("re-adding one kid consumed %d ring slots, want 1", dup.negNext)
	}
}

// One unreadable key row must cost only that kid. Aborting the whole reload would
// freeze the snapshot (no sibling's key is ever learned), skip rotateIfDue and keep
// SigningKey's slow path from ever reaching insertKey — whose DELETE is the only purge —
// so the holder's token endpoint would 503 for good once the last good key aged out.
func TestIngressKeyPg_UnreadableKeyRowIsIsolated(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	s := NewIngressKeyStore(pool, "holder", now)
	good, _, err := s.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	// A row whose PEM cannot be parsed lands beside the good one (a truncated secret).
	bad := "00000000000000000000000000000bad"
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO gw_ingress_key (holder_id, kid, private_key_pem, created_at, not_after) VALUES ($1,$2,$3,$4,$5)`,
		"holder", bad, "-----BEGIN PRIVATE KEY-----\nbm90IGEga2V5\n-----END PRIVATE KEY-----\n",
		now(), now().Add(ingressKeyRotation+engine.IngressBearerTTL+time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.refreshOnce(now()); err != nil {
		t.Fatalf("one unreadable row must not fail the reload: %v", err)
	}
	advance(reloadThrottle)
	if _, ok, err := s.VerificationKey(good, now()); !ok || err != nil {
		t.Fatalf("the good key must still verify from a snapshot that carried an unreadable row (ok=%v err=%v)", ok, err)
	}
	// Unreadable, not unreachable: the kid is unknown (401), never an outage (503).
	if _, ok, err := s.VerificationKey(bad, now()); ok || err != nil {
		t.Fatalf("an unreadable key row = ok:%v err:%v; want false,nil (fail closed for that kid)", ok, err)
	}
	// Rotation still runs past the interval…
	advance(ingressKeyRotation)
	if err := s.refreshOnce(now()); err != nil {
		t.Fatalf("rotation starved by the unreadable row: %v", err)
	}
	kid2, _, err := s.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	if kid2 == good {
		t.Fatal("refreshOnce did not rotate past the unreadable row")
	}
	// …and insertKey's purge removes the unreadable row once its not_after has passed.
	advance(ingressKeyRotation)
	if err := s.refreshOnce(now()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM gw_ingress_key WHERE holder_id='holder' AND kid=$1`, bad).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("the unreadable row was never purged")
	}
}

// interleavingDB parks the first Begin (insertKey's transaction) until a reload has
// taken its snapshot, then lets that insert finish before the reload swaps its caches.
// No sleeps: every step is a channel handshake.
type interleavingDB struct {
	keyDB
	armBegin, armQuery   atomic.Bool
	beganInsert          chan struct{} // closed when the parked insert is about to open its tx
	mayCommit            chan struct{} // closed to let that insert proceed
	insertDone           chan struct{} // closed when the inserting caller has written its cache
	onceBegin, onceQuery sync.Once
}

func (d *interleavingDB) Begin(ctx context.Context) (pgx.Tx, error) {
	if d.armBegin.Load() {
		parked := false
		d.onceBegin.Do(func() { parked = true })
		if parked {
			close(d.beganInsert)
			<-d.mayCommit
		}
	}
	return d.keyDB.Begin(ctx)
}

func (d *interleavingDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	rows, err := d.keyDB.Query(ctx, sql, args...)
	if err != nil || !d.armQuery.Load() {
		return rows, err
	}
	hooked := false
	d.onceQuery.Do(func() { hooked = true })
	if !hooked {
		return rows, err
	}
	// The snapshot is taken; release the parked insert and wait for it to land before
	// the reload gets to its cache swap.
	return &interleavedRows{Rows: rows, afterScan: func() { close(d.mayCommit); <-d.insertDone }}, nil
}

// interleavedRows runs afterScan on the reload's rows.Err() call — the last thing that
// happens before reloadLocked swaps its caches.
type interleavedRows struct {
	pgx.Rows
	afterScan func()
	once      sync.Once
}

func (r *interleavedRows) Err() error {
	r.once.Do(r.afterScan)
	return r.Rows.Err()
}

// A reload that snapshots the DB before this process's own insert commits must not drop
// that key when it swaps its caches: s.signing would name an absent kid and the next
// SigningKey would answer 503 (for up to a second, once per rotation).
func TestIngressKeyPg_ReloadDoesNotDropThisProcessesFreshKey(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	db := &interleavingDB{keyDB: pool, beganInsert: make(chan struct{}), mayCommit: make(chan struct{}), insertDone: make(chan struct{})}
	s := NewIngressKeyStore(db, "holder", now)
	if _, _, err := s.SigningKey(now()); err != nil {
		t.Fatal(err)
	}
	advance(ingressKeyRotation) // rotation is due: the next SigningKey inserts
	t1 := now()
	var kidNew string
	var errNew error
	db.armBegin.Store(true)
	go func() {
		kidNew, _, errNew = s.SigningKey(t1) // parks inside insertKey's Begin
		close(db.insertDone)
	}()
	<-db.beganInsert // the fresh key is not in the DB yet
	db.armQuery.Store(true)
	advance(reloadThrottle) // leave the throttle window so the next miss really reloads
	// This miss reloads: its snapshot predates the insert, and the insert lands (and
	// writes s.keys/s.signing) before the reload swaps.
	if _, ok, err := s.VerificationKey("fedcba9876543210fedcba9876543210", now()); ok || err != nil {
		t.Fatalf("unknown kid = ok:%v err:%v; want false,nil", ok, err)
	}
	<-db.insertDone
	if errNew != nil {
		t.Fatal(errNew)
	}
	if _, ok, err := s.VerificationKey(kidNew, now()); !ok || err != nil {
		t.Fatalf("the reload dropped the key this process had just inserted (ok=%v err=%v)", ok, err)
	}
	kid, _, err := s.SigningKey(now())
	if err != nil {
		t.Fatalf("SigningKey after the reload: %v (s.signing names a kid the swap dropped)", err)
	}
	if kid != kidNew {
		t.Fatalf("SigningKey = %s, want the freshly inserted %s", kid, kidNew)
	}
}

// downDB is a keyDB whose round trips fail once down is set: a database that becomes
// unreachable to THIS replica after it has already learned keys from it.
type downDB struct {
	keyDB
	down atomic.Bool
}

var errDownDB = fmt.Errorf("downDB: unreachable")

func (d *downDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if d.down.Load() {
		return nil, errDownDB
	}
	return d.keyDB.Query(ctx, sql, args...)
}

func (d *downDB) Begin(ctx context.Context) (pgx.Tx, error) {
	if d.down.Load() {
		return nil, errDownDB
	}
	return d.keyDB.Begin(ctx)
}

// A replica whose own key has aged out of its slack, whose database is unreachable, but
// which ALREADY HOLDS a live sibling key (learned by a VerificationKey miss reload while
// the database was up) can still sign — with that key. Every replica behind the store
// verifies it, so refusing here would be a 503 the gateway did not need to answer.
// The adoption therefore has to be reached BEFORE the grace/error branch, not after it.
func TestIngressKeyPg_SignsWithASiblingsLiveKeyWhenItsOwnReloadErrors(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	db := &downDB{keyDB: pool}
	b := NewIngressKeyStore(db, "holder", now)
	kidB, _, err := b.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	// Past B's key's rotation AND past its slack, so grace-signing with it is not an
	// option (a bearer minted now would outlive the key at its siblings).
	advance(ingressKeyRotation + 10*time.Minute)

	// A, on a healthy pool, mints the holder's live key.
	a := NewIngressKeyStore(pool, "holder", now)
	kidA, _, err := a.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	if kidA == kidB {
		t.Fatal("A must have minted a NEW key (B's is past its rotation)")
	}
	// B learns it the only way a replica does: a miss on a bearer A signed.
	if _, ok, err := b.VerificationKey(kidA, now()); !ok || err != nil {
		t.Fatalf("B must learn A's key through a miss reload (ok=%v err=%v)", ok, err)
	}

	db.down.Store(true)         // B's database goes away
	advance(2 * reloadThrottle) // …and the next reload really runs (and errors)
	kid, key, err := b.SigningKey(now())
	if err != nil {
		t.Fatalf("B must sign with the sibling key it already holds, got %v", err)
	}
	if kid != kidA {
		t.Fatalf("B signed with %q, want A's live key %q", kid, kidA)
	}
	if key == nil {
		t.Fatal("nil signing key")
	}
}

// A COLD replica — one whose cache holds no key at all — must mint on its first
// SigningKey call even with the miss throttle armed. The throttle is there to stop an
// unauthenticated caller turning random kids into database traffic; sharing it with the
// signing path lets those same callers hold a fresh replica at 503 until the first
// refresh tick, because an empty cache has nothing to grace-sign with and nothing for a
// throttled call to adopt.
func TestIngressKeyPg_ColdReplicaMintsWhileTheMissThrottleIsArmed(t *testing.T) {
	pool := testPool(t)
	now, _ := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	s := NewIngressKeyStore(pool, "holder", now)
	// An unauthenticated bearer with a random kid: the reload runs against an empty key
	// table and stamps the throttle at this instant.
	if _, ok, err := s.VerificationKey("0123456789abcdef0123456789abcdef", now()); ok || err != nil {
		t.Fatalf("unknown kid = ok:%v err:%v; want false,nil", ok, err)
	}
	// Same instant, so the throttle is armed. The first honest token request must still
	// get a signing key.
	kid, key, err := s.SigningKey(now())
	if err != nil || kid == "" || key == nil {
		t.Fatalf("cold replica: SigningKey = %q,%v,%v — want a minted key (the miss throttle is holding /oauth/token at 503)", kid, key != nil, err)
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM gw_ingress_key WHERE holder_id='holder' AND kid=$1`, kid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("minted key rows = %d, want 1 (the key must be shared, not process-local)", n)
	}
}

// A miss on a WARM replica that arrives while another goroutine's reload is in flight
// answers IMMEDIATELY from the snapshot in hand, and the kid verifies on the NEXT call
// once that reload lands. Waiting here is what an anonymous caller would be handed a
// goroutine for: the in-flight reload can occupy the whole store timeout, and jwt/v5
// reaches the keyfunc before it validates a signature, so unsigned tokens with random kids
// would each park one.
//
// WARM is the whole point of the fixture: B mints a key of its OWN before the hung reload
// starts, so a reload has completed successfully and the cold path (which does park, on
// the reload it needs — see the rows below) is provably not the branch under test. A cold
// store here would answer this row's assertion by waiting, which is the opposite of what
// its name claims.
//
// Deterministic: every step is a channel handshake (hungDB parks the query; only release()
// frees it), so the "did not wait" assertion is made while the reload provably cannot have
// finished.
func TestIngressKeyPg_MissDuringAnInFlightReloadAnswersAtOnceAndResolvesNext(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	hung := &hungDB{keyDB: pool, blocked: make(chan struct{}), gate: make(chan struct{})}
	t.Cleanup(hung.release)
	b := NewIngressKeyStore(hung, "holder", now)
	parked := make(chan struct{})
	b.onColdWait = func() { close(parked) } // nothing may park in this row; see the assertion below

	// B mints the first key of the holder, so its cache is WARM from here on.
	kidB, _, err := b.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	// A rotates: past B's key's rotation interval, A mints a SECOND key that B has never
	// seen. B's own key stays live (not_after is rotation + bearer TTL + a minute), so B
	// is still a warm replica missing exactly one sibling kid.
	advance(ingressKeyRotation)
	a := NewIngressKeyStore(pool, "holder", now)
	kid, key, err := a.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	if kid == kidB {
		t.Fatal("A adopted B's key instead of rotating: the row has no sibling kid to miss")
	}
	if b.cold() {
		t.Fatal("the fixture is not a warm replica")
	}
	hung.hang.Store(true)

	firstDone := make(chan struct{})
	go func() { b.VerificationKey(kid, now()); close(firstDone) }() // parks inside the reload's query
	<-hung.blocked                                                  // a reload is now in flight and has stamped the throttle
	if b.inFlight() == nil {
		t.Fatal("no in-flight latch published while a reload is parked in its query")
	}

	// The second caller must come back NOW — the reload it would have waited on is
	// provably still parked, so a returned answer cannot have come from waiting.
	if _, ok, err := b.VerificationKey(kid, now()); ok || err != nil {
		t.Fatalf("miss during an in-flight reload = ok:%v err:%v; want false,nil answered without waiting", ok, err)
	}
	select {
	case <-firstDone:
		t.Fatal("the parked reload finished before the second caller answered: the row proved nothing")
	default: // still parked, so the second caller really did not wait for it
	}
	select {
	case <-parked:
		t.Fatal("a warm replica parked on the in-flight reload: the cold path leaked into the warm one")
	default:
	}
	// It must not be negatively cached either: the snapshot it answered from is not its own.
	if _, neg := b.negCache[kid]; neg {
		t.Fatal("a kid refused while another reload was in flight was negatively cached")
	}

	// Release the parked query; the kid resolves on the NEXT call, from the cache that
	// reload filled — no second query needed.
	hung.release()
	<-firstDone
	pub, ok, err := b.VerificationKey(kid, now())
	if !ok || err != nil {
		t.Fatalf("the kid on the next call = ok:%v err:%v; want it resolved from the completed reload", ok, err)
	}
	if !pub.Equal(&key.PublicKey) {
		t.Fatal("resolved the wrong public key")
	}
}

// lockedClock is an injected clock that may be advanced from another goroutine — the
// cold rows below advance it from inside the store's onColdWait hook, on the caller's
// goroutine, while the test goroutine and (in one row) the refresh loop read it.
type lockedClock struct {
	mu  sync.Mutex
	cur time.Time
}

func newLockedClock(t0 time.Time) *lockedClock { return &lockedClock{cur: t0} }

func (c *lockedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur
}

func (c *lockedClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cur = c.cur.Add(d)
}

// coldAnswer is one VerificationKey result handed back over a channel by a caller that
// runs on its own goroutine, so a row can assert it has NOT answered yet.
type coldAnswer struct {
	pub *ecdsa.PublicKey
	ok  bool
	err error
}

// verifyAsync runs one VerificationKey on its own goroutine and returns the channel its
// answer lands on.
func verifyAsync(s *IngressKeyStore, kid string, now time.Time) <-chan coldAnswer {
	got := make(chan coldAnswer, 1)
	go func() {
		pub, ok, err := s.VerificationKey(kid, now)
		got <- coldAnswer{pub, ok, err}
	}()
	return got
}

// The COLD replica's half of the same seam, and the one case that must NOT answer at once.
// A replica on which no reload has ever completed cannot tell an unknown kid from one it
// simply has not loaded yet, so answering 401 there refuses LIVE bearers minted by a
// healthy sibling — on every rolling deploy, which is the exact case a shared key store
// exists to make safe. The boot reload the refresh loop starts before the listener comes
// up normally holds the reload slot, and a request that arrives while it is still running
// must PARK (bounded, on a channel — not on the database) and answer from what that
// reload brings in: here the key was in the table before the reload's SELECT ran, so the
// reload lands it and the caller's re-lookup hits.
//
// Deterministic: hungDB parks the boot reload's query and only release() frees it, and the
// parked caller announces itself through onColdWait, so the "has not returned" assertion
// is made while the caller is provably parked and the reload provably unfinished.
func TestIngressKeyPg_ColdReplicaWaitsForTheBootReloadThenVerifiesASiblingsBearer(t *testing.T) {
	pool := testPool(t)
	now, _ := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	// A mints the holder's live key and issues a bearer with it.
	a := NewIngressKeyStore(pool, "holder", now)
	kid, key, err := a.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}

	// B is the replica that has just started: nothing loaded, its boot reload parked in
	// the database.
	hung := &hungDB{keyDB: pool, blocked: make(chan struct{}), gate: make(chan struct{})}
	t.Cleanup(hung.release)
	b := NewIngressKeyStore(hung, "holder", now)
	parked := make(chan struct{})
	b.onColdWait = func() { close(parked) }
	hung.hang.Store(true)
	bootDone := make(chan struct{})
	go func() { b.refreshOnce(now()); close(bootDone) }() // what RunRefresh does before the first tick
	mustSignal(t, hung.blocked, "the boot reload's query")
	if b.inFlight() == nil {
		t.Fatal("no in-flight latch published while the boot reload is parked in its query")
	}
	if !b.cold() {
		t.Fatal("the fixture is not a cold replica")
	}

	got := verifyAsync(b, kid, now())
	mustSignal(t, parked, "the cold replica's wait on the boot reload")
	select {
	case ans := <-got:
		t.Fatalf("a cold replica answered ok:%v err:%v while the boot reload was still parked — a live sibling bearer is refused on every rolling deploy", ans.ok, ans.err)
	default: // still parked, which is the whole contract
	}

	hung.release()
	<-bootDone
	ans := <-got
	if !ans.ok || ans.err != nil {
		t.Fatalf("a live sibling bearer at a cold replica = ok:%v err:%v; want it accepted once the boot reload landed", ans.ok, ans.err)
	}
	if !ans.pub.Equal(&key.PublicKey) {
		t.Fatal("resolved the wrong public key")
	}
}

// …and the wait is BOUNDED. A boot reload that never comes back (a database that accepts
// the connection and answers nothing) must not hold the cold replica's callers past one
// store timeout, and what they get at the bound is the tri-state "could not tell" — a
// retryable 503 — never the 401 that accuses a healthy sibling's bearer of being forged.
//
// The bound is injected (coldWaitBound) so the row costs milliseconds rather than the
// shipped two seconds; the assertion that it was the BOUND and not the reload that ended
// the wait is structural, not a stopwatch: the reload is still parked in its query when
// the caller comes back.
func TestIngressKeyPg_ColdReplicaWaitOnTheBootReloadIsBounded(t *testing.T) {
	pool := testPool(t)
	now, _ := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	a := NewIngressKeyStore(pool, "holder", now)
	kid, _, err := a.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	hung := &hungDB{keyDB: pool, blocked: make(chan struct{}), gate: make(chan struct{})}
	t.Cleanup(hung.release)
	b := NewIngressKeyStore(hung, "holder", now)
	b.coldWaitBound = 20 * time.Millisecond
	hung.hang.Store(true)
	go b.refreshOnce(now())
	mustSignal(t, hung.blocked, "the boot reload's query")

	pub, ok, verr := b.VerificationKey(kid, now())
	if ok || pub != nil {
		t.Fatalf("resolved a kid the parked reload never delivered: ok:%v", ok)
	}
	if verr == nil {
		t.Fatal("a cold replica whose reload never answered returned (nil,false,nil) — a 401 for a live bearer, on a cache that has never been loaded; want the store-unavailable tri-state (503)")
	}
	if !errors.Is(verr, errColdReloadWait) {
		t.Fatalf("cold wait timeout error = %v; want %v", verr, errColdReloadWait)
	}
	// The reload is STILL parked, so the bound is what ended the wait — no stopwatch needed.
	if b.inFlight() == nil {
		t.Fatal("the boot reload completed after all: the row measured nothing")
	}
}

// staleDB executes the real SELECT and then parks BEFORE handing its rows back, so a key
// minted while it is parked provably post-dates the snapshot: the reload that lands from
// it is a snapshot OLDER than that key, whatever the wall clock says.
type staleDB struct {
	keyDB
	arm         atomic.Bool
	blocked     chan struct{}
	gate        chan struct{}
	onceBlocked sync.Once
	onceRelease sync.Once
}

func (d *staleDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	rows, err := d.keyDB.Query(ctx, sql, args...)
	if err != nil || !d.arm.Load() {
		return rows, err
	}
	parked := false
	d.onceBlocked.Do(func() { parked = true })
	if parked {
		close(d.blocked)
		<-d.gate
	}
	return rows, nil
}

func (d *staleDB) release() { d.onceRelease.Do(func() { close(d.gate) }) }

// A cold caller answers only from a reload that STARTED AFTER IT ARRIVED. The boot
// reload's SELECT can run before a sibling mints the holder's key and land after the
// bearer carrying that key has arrived — a snapshot older than the request. Answering
// "unknown" from it is the same 401 for a live bearer as answering from nothing, reached
// through the other arm: the cache was loaded, but not with evidence about THIS request.
// So the caller that parked on the boot reload wakes, finds that reload predates it, and
// resolves the kid through a younger one — here its own, at the next throttle slot.
//
// Deterministic: staleDB parks the boot reload AFTER its SELECT has executed and before
// its rows are read, so the mint provably post-dates the snapshot; the parked caller
// announces itself through onColdWait, whose SECOND firing (the caller woke, found the
// boot snapshot too old and is about to park for the throttle slot) proves the snapshot
// really was empty and then advances the injected clock past the throttle, so the caller
// runs its own reload at once rather than after a real second.
func TestIngressKeyPg_ColdReplicaDoesNotAnswerFromASnapshotOlderThanTheRequest(t *testing.T) {
	pool := testPool(t)
	clk := newLockedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	stale := &staleDB{keyDB: pool, blocked: make(chan struct{}), gate: make(chan struct{})}
	t.Cleanup(stale.release)
	counted := &countingDB{keyDB: stale}
	b := NewIngressKeyStore(counted, "holder", clk.now)
	b.coldWaitBound = 30 * time.Second // the "has not answered" assertion must not rest on the shipped 2 s
	stale.arm.Store(true)
	bootDone := make(chan struct{})
	go func() { b.refreshOnce(clk.now()); close(bootDone) }() // what RunRefresh does before the first tick
	mustSignal(t, stale.blocked, "the boot reload's SELECT (executed, rows not yet returned)")

	// A mints the holder's first key AFTER that SELECT ran, and issues a bearer with it.
	a := NewIngressKeyStore(pool, "holder", clk.now)
	kid, key, err := a.SigningKey(clk.now())
	if err != nil {
		t.Fatal(err)
	}

	parked := make(chan struct{}, 4)
	var waits atomic.Int32
	b.onColdWait = func() {
		if waits.Add(1) == 2 {
			// The caller woke from the boot reload and is about to park for the
			// throttle slot: the snapshot it declined to answer from must really be
			// the stale one (no key), or the row proves nothing.
			if !b.cacheEmpty() {
				t.Error("the boot snapshot carried the key: the row is not the stale case it is named for")
			}
			if b.cold() {
				t.Error("the stale boot reload completed successfully, so the replica must be warm from here on (the caller stays on the cold path it entered)")
			}
			clk.advance(reloadThrottle)
		}
		parked <- struct{}{}
	}
	got := verifyAsync(b, kid, clk.now())
	mustSignal(t, parked, "the cold caller's wait on the boot reload")
	select {
	case ans := <-got:
		t.Fatalf("answered ok:%v err:%v while the boot reload was still parked", ans.ok, ans.err)
	default:
	}
	stale.release()
	<-bootDone
	ans := <-got
	if !ans.ok || ans.err != nil {
		t.Fatalf("a live bearer at a cold replica whose boot snapshot predates the key = ok:%v err:%v; want the key (a 401 here refuses a live bearer on the evidence of a snapshot older than the request)", ans.ok, ans.err)
	}
	if !ans.pub.Equal(&key.PublicKey) {
		t.Fatal("resolved the wrong public key")
	}
	if got := atomic.LoadInt32(&counted.reloads); got != 2 {
		t.Fatalf("reload queries = %d, want 2 (the stale boot reload, then the one younger than the request)", got)
	}
	// The refresh also adopts the sibling's committed row: Begin, isolation,
	// advisory lock, SELECT, Commit and the deferred Rollback call (already closed).
	if got := atomic.LoadInt32(&counted.queries); got != 8 {
		t.Fatalf("database calls = %d, want two reloads plus six transactional adoption calls", got)
	}
	if n := waits.Load(); n != 2 {
		t.Fatalf("the caller parked %d time(s), want 2: once on the boot reload, once for the throttle slot", n)
	}
}

// The SHIPPED shape of the cold state: RunRefresh is running, so a cold miss never runs a
// reload of its own — it kicks the wake channel and parks, and the loop's reload is what
// resolves it. One reload stream per replica, in both states. The fixture is the outage
// that outlasts start-up: the warm-up reload failed, so the replica is still cold when the
// database comes back and a sibling mints the holder's key.
//
// Deterministic: the loop's own error hook is the signal that the warm-up has failed;
// the injected clock is stepped past the throttle before the request, so the wake-driven
// reload runs at once; and the caller announces its park through onColdWait.
func TestIngressKeyPg_ColdReplicaUnderTheRefreshLoopIsResolvedByTheLoopsReloadNotItsOwn(t *testing.T) {
	pool := testPool(t)
	clk := newLockedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	down := &downDB{keyDB: pool}
	counted := &countingDB{keyDB: down}
	b := NewIngressKeyStore(counted, "holder", clk.now)
	var parked atomic.Int32
	b.onColdWait = func() { parked.Add(1) }
	down.down.Store(true)
	failed := make(chan struct{})
	var once sync.Once
	startRefresh(t, b, func(string) { once.Do(func() { close(failed) }) })
	mustSignal(t, failed, "the refresh loop's warm-up reload (failing)")
	if !b.cold() {
		t.Fatal("a failed warm-up left the replica warm: the fixture is not the outage it is named for")
	}

	// The database comes back and a sibling mints the holder's key.
	down.down.Store(false)
	a := NewIngressKeyStore(pool, "holder", clk.now)
	kid, key, err := a.SigningKey(clk.now())
	if err != nil {
		t.Fatal(err)
	}
	clk.advance(reloadThrottle) // the failed warm-up armed the throttle; open the slot

	pub, ok, verr := b.VerificationKey(kid, clk.now())
	if !ok || verr != nil {
		t.Fatalf("a live bearer at a cold replica with the refresh loop running = ok:%v err:%v; want the key", ok, verr)
	}
	if !pub.Equal(&key.PublicKey) {
		t.Fatal("resolved the wrong public key")
	}
	if got := atomic.LoadInt32(&counted.queries); got != 2 {
		t.Fatalf("reload queries = %d, want 2 (the failed warm-up and the loop's wake-driven reload) — the request path must issue none of its own while the loop runs", got)
	}
	if parked.Load() == 0 {
		t.Fatal("the cold caller never parked: it answered from something other than the loop's reload")
	}
	if b.cold() {
		t.Fatal("the loop's successful reload did not warm the replica")
	}
}

// Cold is "no reload has ever completed successfully" — and an EMPTY successful load
// counts. A replica whose boot reload found no rows is WARM from then on and pays the
// ordinary once-a-second throttle: inside the window that boot reload armed, a miss is
// refused at once with no wait and no second query, and a sibling's first key minted
// inside that same window is refused ONCE and verifies at the next slot — the same bill a
// rotation costs (TestIngressKeyPg_ThrottledMissAnswersImmediatelyAndReloadsWhenTheThrottleAllows).
// With the slot OPEN the empty cache resolves that first key on the same request, at the
// cost of one synchronous query and never a wait
// (TestIngressKeyPg_WarmReplicaWithAnEmptyCacheResolvesASiblingsFirstKeyOnItsFirstCall).
//
// That is what caps an unauthenticated flood at a replica whose holder has no key yet:
// the old cold predicate ("the cache is empty") let every such miss run its own query.
// The clock is frozen, so the throttle is provably armed for the whole row.
func TestIngressKeyPg_EmptyFirstLoadWarmsTheReplicaAndASiblingsFirstKeyVerifiesAtTheNextSlot(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	db := &countingDB{keyDB: pool}
	b := NewIngressKeyStore(db, "holder", now)
	var parked atomic.Int32
	b.onColdWait = func() { parked.Add(1) }
	// B boots first: its reload SUCCEEDS and finds no live row.
	if err := b.reload(now()); err != nil {
		t.Fatal(err)
	}
	if !b.cacheEmpty() {
		t.Fatal("the boot reload found a key: the fixture is not the empty holder")
	}
	if b.cold() {
		t.Fatal("a successful empty load left the replica cold: a holder with no key yet would pay a query per miss")
	}
	if !b.throttled(now()) {
		t.Fatal("the fixture's throttle is not armed, so the row is not the window under test")
	}

	// A miss inside the window: answered at once, no wait, no query.
	if _, ok, err := b.VerificationKey("0123456789abcdef0123456789abcdef", now()); ok || err != nil {
		t.Fatalf("unknown kid inside the throttle at a warm-but-empty replica = ok:%v err:%v; want false,nil at once", ok, err)
	}
	if got := atomic.LoadInt32(&db.queries); got != 1 {
		t.Fatalf("reload queries = %d, want 1 (the boot reload only: the throttle is in force)", got)
	}

	// A mints the holder's FIRST key inside the same window and issues a bearer with it.
	a := NewIngressKeyStore(pool, "holder", now)
	kid, key, err := a.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := b.VerificationKey(kid, now()); ok || err != nil {
		t.Fatalf("a sibling's first key inside the throttle = ok:%v err:%v; want false,nil — refused once, at once", ok, err)
	}
	if got := atomic.LoadInt32(&db.queries); got != 1 {
		t.Fatalf("reload queries = %d, want 1 (the miss must not query around the throttle)", got)
	}
	b.mu.RLock()
	_, neg := b.negCache[kid]
	b.mu.RUnlock()
	if neg {
		t.Fatal("a kid refused inside the throttle was negatively cached: the refusal now outlives the window")
	}
	if parked.Load() != 0 {
		t.Fatalf("a warm replica parked %d time(s): the cold path leaked into the warm one", parked.Load())
	}
	// The next slot resolves it.
	advance(reloadThrottle)
	pub, ok, err := b.VerificationKey(kid, now())
	if !ok || err != nil {
		t.Fatalf("the sibling's first key at the next slot = ok:%v err:%v; want it resolved", ok, err)
	}
	if !pub.Equal(&key.PublicKey) {
		t.Fatal("resolved the wrong public key")
	}
	if got := atomic.LoadInt32(&db.queries); got != 2 {
		t.Fatalf("reload queries = %d, want 2", got)
	}
}

// The WARM path's classification and its evidence come from one cache-lock section, and
// every warm refusal takes a last look under the lock before it answers. A request that
// straddles the first successful install — classified while the warm-up reload was in
// flight, answered after that reload installed its snapshot but before its completion
// stamped an outcome — would otherwise refuse, from the lookup it classified by, a kid the
// install has since put in its own cache: I-1's 401, one frame up, on every replica boot.
//
// This row pins the OUTCOME under one lock hold, not the interleaving: the state the
// scenario leaves the store in (snapshot installed and loaded, reload still in flight,
// no outcome stamped) must answer the key. No deterministic red-before is constructible
// for the gap between two adjacent reads on the caller's own goroutine without a seam of
// its own; the guard is structural — classify and missAnswer — and lives in the code.
func TestIngressKeyPg_WarmMissTakesALastLookUnderTheLockBeforeRefusing(t *testing.T) {
	pool := testPool(t)
	now, _ := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	a := NewIngressKeyStore(pool, "holder", now)
	kid, key, err := a.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	hung := &hungDB{keyDB: pool, blocked: make(chan struct{}), gate: make(chan struct{})}
	t.Cleanup(hung.release)
	b := NewIngressKeyStore(hung, "holder", now)
	var parked atomic.Int32
	b.onColdWait = func() { parked.Add(1) }
	hung.hang.Store(true)
	go b.reload(now()) // the warm-up: parked in its query, latch published, nothing stamped
	mustSignal(t, hung.blocked, "the warm-up reload's query")
	// The install lands (as reloadQuery's section does) while the reload is still in
	// flight and before its completion: loaded, the key in the cache, no outcome yet.
	a.mu.RLock()
	installed := a.keys[kid]
	a.mu.RUnlock()
	b.mu.Lock()
	b.keys[kid] = installed
	b.loaded = true
	b.mu.Unlock()
	if b.inFlight() == nil || b.cold() || b.cacheEmpty() {
		t.Fatal("the fixture is not the straddled-install state (in flight, loaded, key cached)")
	}

	pub, ok, verr := b.VerificationKey(kid, now())
	if !ok || verr != nil {
		t.Fatalf("a kid the install put in the cache, with its reload still in flight = ok:%v err:%v; want the key — a 401 here refuses a bearer the replica is holding", ok, verr)
	}
	if !pub.Equal(&key.PublicKey) {
		t.Fatal("resolved the wrong public key")
	}
	if parked.Load() != 0 {
		t.Fatal("a warm replica parked")
	}
	// …and the last look itself, in isolation: a refusal's evidence and the key are read
	// together, so a key present at that instant is answered, and an absent one answers
	// the snapshot's outcome.
	if p, ok, err := b.missAnswer(kid, now()); !ok || err != nil || !p.Equal(&key.PublicKey) {
		t.Fatalf("missAnswer with the key cached = ok:%v err:%v; want the key", ok, err)
	}
	if _, ok, err := b.missAnswer("0123456789abcdef0123456789abcdef", now()); ok || err != nil {
		t.Fatalf("missAnswer for an unknown kid under a nil outcome = ok:%v err:%v; want false,nil", ok, err)
	}
}

// The fresh-install shape, on the SHIPPED wiring: every replica boots against an EMPTY
// key table and reads it successfully (warm), the refresh loop is running, and moments
// later the holder's first token is minted at one replica while the first authenticated
// call lands at another. That replica's cache holds NO key, so a snapshot is not something
// it can answer from; it runs the one synchronous, throttled, TryLock-guarded reload the
// warm path allows (attempt) and verifies the sibling's first key on the FIRST call —
// never a wait, never a park, one query. Deferring to the out-of-band kick here is a 401
// against the first bearer a brand-new deployment ever presents, at every replica but the
// minting one, which is exactly what make smoke's two-replica ingress probe presents.
//
// Deterministic: reloadSignalDB + settleReload prove the empty warm-up has completed; the
// clock (mutex-guarded, since the loop reads it) is stepped past the throttle the warm-up
// armed, so the slot is provably open when the bearer arrives.
func TestIngressKeyPg_WarmReplicaWithAnEmptyCacheResolvesASiblingsFirstKeyOnItsFirstCall(t *testing.T) {
	pool := testPool(t)
	clk := newLockedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	db := &reloadSignalDB{keyDB: pool}
	b := NewIngressKeyStore(db, "holder", clk.now)
	var parked atomic.Int32
	b.onColdWait = func() { parked.Add(1) }
	warmed := db.afterNextReload()
	startRefresh(t, b, nil)
	mustSignal(t, warmed, "the refresh loop's warm-up reload over the empty table")
	settleReload(b)
	if b.cold() || !b.cacheEmpty() {
		t.Fatal("the fixture must be a WARM replica holding no key (a successful empty read)")
	}
	clk.advance(reloadThrottle) // the warm-up armed the throttle; the smoke's mint comes ten seconds later

	// The holder's first key is minted at a sibling…
	a := NewIngressKeyStore(pool, "holder", clk.now)
	kid, key, err := a.SigningKey(clk.now())
	if err != nil {
		t.Fatal(err)
	}
	// …and the first authenticated call lands here.
	pub, ok, verr := b.VerificationKey(kid, clk.now())
	if !ok || verr != nil {
		t.Fatalf("the holder's first key at a warm replica whose cache holds no key = ok:%v err:%v; want it verified on the FIRST call (this is the fresh-install 401 the two-replica smoke probe hits)", ok, verr)
	}
	if !pub.Equal(&key.PublicKey) {
		t.Fatal("resolved the wrong public key")
	}
	if got := atomic.LoadInt32(&db.queries); got != 2 {
		t.Fatalf("reload queries = %d, want 2 (the warm-up and the one this miss ran itself, synchronously)", got)
	}
	if parked.Load() != 0 {
		t.Fatalf("a warm replica parked %d time(s): the cold path leaked into the warm one", parked.Load())
	}
}

// …and that exception is bounded exactly as the warm path is: an empty-cache flood against
// a hung database, with the refresh loop running, costs ONE query in flight (the TryLock
// winner, parked in its own round trip and nowhere else) and every other caller answers
// at once — no caller parks (onColdWait never fires), nobody waits on the winner, nobody
// queues on reloadMu. The liveness fence's shape
// (TestIngressKeyPg_ConcurrentMissesNeverParkAgainstAHungDatabase) on a cache that holds
// no key, where the loop-running early return does NOT apply.
//
// Deterministic: reloadSignalDB + settleReload prove the empty warm-up completed; the 199
// answers are counted while the winner's query is provably still parked (hungDB, released
// only afterwards), so none of them can have waited on it.
func TestIngressKeyPg_EmptyCacheFloodCostsOneQueryAndParksNobody(t *testing.T) {
	pool := testPool(t)
	clk := newLockedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	hung := &hungDB{keyDB: pool, blocked: make(chan struct{}), gate: make(chan struct{})}
	t.Cleanup(hung.release)
	signal := &reloadSignalDB{keyDB: hung}
	counted := &countingDB{keyDB: signal}
	s := NewIngressKeyStore(counted, "holder", clk.now)
	var parked atomic.Int32
	s.onColdWait = func() { parked.Add(1) }
	warmed := signal.afterNextReload()
	startRefresh(t, s, nil)
	t.Cleanup(hung.release) // registered after startRefresh, so it runs BEFORE the loop is stopped
	mustSignal(t, warmed, "the refresh loop's warm-up reload over the empty table")
	settleReload(s)
	if s.cold() || !s.cacheEmpty() {
		t.Fatal("the fixture must be a warm replica holding no key")
	}
	clk.advance(reloadThrottle) // slot open, so exactly one caller may run a query
	atomic.StoreInt32(&counted.queries, 0)
	hung.hang.Store(true)

	const n = 200
	var bad atomic.Int32
	done := make(chan struct{}, n) // one token per answered caller
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		kid := fmt.Sprintf("%032x", i+1)
		go func() {
			<-start
			if _, ok, err := s.VerificationKey(kid, clk.now()); ok || err != nil {
				bad.Add(1)
			}
			done <- struct{}{}
		}()
	}
	close(start)
	mustSignal(t, hung.blocked, "the one reload the flood is allowed")
	// Everyone but the winner answers while the winner is provably still parked in its
	// query: drain exactly n-1 completions (mustSignal-style ceiling — a slow scheduler
	// can only make this later, and a caller that waited on the winner would never get
	// here until release()).
	for i := 0; i < n-1; i++ {
		mustSignal(t, done, fmt.Sprintf("empty-cache miss %d of %d answering with the one query parked", i+1, n-1))
	}
	if got := atomic.LoadInt32(&counted.queries); got != 1 {
		t.Fatalf("the empty-cache flood put %d queries on the database, want exactly 1 (TryLock)", got)
	}
	// The winner is a CALLER, inside its own synchronous query, so the n-th completion
	// cannot arrive until release(). Failing direction only: on the fixed code nothing
	// can deliver it inside the settle, while a request path that deferred to the loop's
	// kick-driven reload (the fresh-install 401 in this row's shape) delivers it within
	// microseconds.
	select {
	case <-done:
		t.Fatalf("all %d misses answered with the one query still parked; want exactly %d — the query must be a caller's own TryLock-guarded attempt, not the loop's kick-driven reload", n, n-1)
	case <-time.After(200 * time.Millisecond):
	}
	if parked.Load() != 0 {
		t.Fatalf("%d of %d warm empty-cache misses parked: the cold path leaked into the warm one", parked.Load(), n)
	}
	hung.release()
	mustSignal(t, done, "the winner answering once its query was released")
	if bad.Load() != 0 {
		t.Fatalf("%d of %d misses answered something other than (nil,false,nil)", bad.Load(), n)
	}
}

// The rate ceiling in the COLD state, measured. Unauthenticated callers (validKID admits
// any 32-hex string and jwt/v5 runs the keyfunc before it verifies a signature) must not
// drive the database faster while a replica is cold than while it is warm: at most one
// reload query per throttle slot, at most one in flight, however many misses arrive. What
// the cold state costs instead is one PARKED goroutine per concurrent miss — parked on a
// channel, never on the database — for at most one bound each, and the 503-class
// errColdReloadWait at that bound rather than a 401.
//
// Three bursts of 200 at one frozen instant: while the boot reload is parked (one query,
// everyone parks on it); after that reload FAILED, inside the throttle window (zero
// further queries — everyone parks until the bound); and after the injected clock steps
// past the throttle (exactly ONE more query, whose failure every waiter answers with).
// The bound is injected so the row costs milliseconds; the handshakes are the parked
// callers announcing themselves through onColdWait.
func TestIngressKeyPg_ColdFloodParksOnTheEpochAndCostsOneQueryPerThrottleSlot(t *testing.T) {
	clk := newLockedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	hf := &hangFailDB{keyDB: testPool(t), blocked: make(chan struct{}), gate: make(chan struct{})}
	t.Cleanup(hf.release)
	counted := &countingDB{keyDB: hf}
	s := NewIngressKeyStore(counted, "holder", clk.now)
	s.coldWaitBound = 50 * time.Millisecond
	const n = 200
	var parked atomic.Int32
	allParked := make(chan struct{})
	s.onColdWait = func() {
		if parked.Add(1) == n {
			close(allParked)
		}
	}
	hf.arm.Store(true)
	go s.reload(clk.now()) // the boot reload: parks in its query, then fails
	mustSignal(t, hf.blocked, "the boot reload's query")
	if !s.cold() {
		t.Fatal("the fixture is not a cold replica")
	}

	// burst runs n concurrent misses and reports how each was answered.
	burst := func() (bounded, storeErr, other int32) {
		var wg sync.WaitGroup
		var b, e, o atomic.Int32
		for i := 0; i < n; i++ {
			kid := fmt.Sprintf("%032x", i+1)
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, ok, err := s.VerificationKey(kid, clk.now())
				switch {
				case ok || err == nil:
					o.Add(1) // a key nobody minted, or a bare 401 out of an unloaded cache
				case errors.Is(err, errColdReloadWait):
					b.Add(1)
				default:
					e.Add(1) // the reload's own failure, answered by a waiter
				}
			}()
		}
		wg.Wait()
		return b.Load(), e.Load(), o.Load()
	}

	// Burst 1: everyone parks on the parked boot reload; the flood costs no query.
	first := make(chan [3]int32, 1)
	go func() { b, e, o := burst(); first <- [3]int32{b, e, o} }()
	mustSignal(t, allParked, "all 200 cold misses parking")
	if got := atomic.LoadInt32(&counted.queries); got != 1 {
		t.Fatalf("queries with %d misses parked on the boot reload = %d, want exactly 1 (one in flight, whatever the concurrency)", n, got)
	}
	hf.release() // the boot reload fails: the replica stays cold, the throttle is armed
	r := <-first
	if r[2] != 0 {
		t.Fatalf("%d of %d cold misses answered ok or (nil,false,nil) — a 401 out of a cache that has never been loaded", r[2], n)
	}
	if got := atomic.LoadInt32(&counted.queries); got != 1 {
		t.Fatalf("queries after burst 1 = %d, want 1 — a miss that woke inside the throttle window queried around it", got)
	}
	if !s.cold() {
		t.Fatal("a failed boot reload warmed the replica")
	}

	// Burst 2, same instant, inside the throttle window: zero further queries; everyone
	// parks and comes back at the bound with the 503-class answer.
	b2, e2, o2 := burst()
	if got := atomic.LoadInt32(&counted.queries); got != 1 {
		t.Fatalf("queries after a second burst of %d inside the throttle = %d, want still 1 (the cold state must not exceed the warm rate)", n, got)
	}
	if b2 != n {
		t.Fatalf("burst 2: %d bounded-wait answers, %d store errors, %d other; want all %d to be errColdReloadWait — no reload younger than any of them completed", b2, e2, o2, n)
	}

	// Burst 3, one throttle later: exactly ONE more query, and every answer is an error
	// (its failure, or the bound for a caller that arrived after it completed) — never ok,
	// never a bare 401.
	clk.advance(reloadThrottle)
	b3, e3, o3 := burst()
	if got := atomic.LoadInt32(&counted.queries); got != 2 {
		t.Fatalf("queries after the throttle opened = %d, want exactly 2 (one more, whatever the concurrency)", got)
	}
	if o3 != 0 || e3 == 0 {
		t.Fatalf("burst 3: %d bounded-wait answers, %d store errors, %d other; want no 'other' and at least one caller answering the reload's own failure", b3, e3, o3)
	}
}

// A COLD replica over a database that refuses connections: one reload attempt per throttle
// slot and ONE log line per failed attempt, however many misses arrive. The first miss in
// a slot runs the reload, logs its failure once and answers it; every later miss in the
// same slot parks (up to the bound) and answers errColdReloadWait without a query or a
// log line of its own. Before this shape the cold path skipped the throttle, and a
// refusing database cost one connection attempt and one CloudWatch line PER REQUEST.
//
// The log is captured through a test writer; the clock is frozen inside each slot and
// stepped once between them.
func TestIngressKeyPg_ColdOutageCostsOneQueryAndOneLogLinePerThrottleSlot(t *testing.T) {
	clk := newLockedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	down := &downDB{keyDB: testPool(t)}
	down.down.Store(true)
	counted := &countingDB{keyDB: down}
	s := NewIngressKeyStore(counted, "holder", clk.now)
	s.coldWaitBound = 20 * time.Millisecond
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	reloadLines := func() int { return strings.Count(buf.String(), "pgstore: ingress key reload:") }

	const perSlot = 5
	for slot := 1; slot <= 2; slot++ {
		for i := 0; i < perSlot; i++ {
			kid := fmt.Sprintf("%032x", slot*16+i+1)
			_, ok, err := s.VerificationKey(kid, clk.now())
			switch {
			case ok || err == nil:
				t.Fatalf("slot %d miss %d over a refusing database = ok:%v err:%v — a 401 out of a cache that has never been loaded", slot, i, ok, err)
			case i == 0 && !errors.Is(err, errDownDB):
				t.Fatalf("slot %d: the first miss must run the reload and answer its failure, got %v", slot, err)
			case i > 0 && !errors.Is(err, errColdReloadWait):
				t.Fatalf("slot %d miss %d must park and answer the bound, got %v", slot, i, err)
			}
		}
		if got := atomic.LoadInt32(&counted.queries); got != int32(slot) {
			t.Fatalf("after %d misses in slot %d: %d reload attempts, want %d (one per slot)", perSlot, slot, got, slot)
		}
		if got := reloadLines(); got != slot {
			t.Fatalf("after %d misses in slot %d: %d reload log lines, want %d (one per failed attempt):\n%s", perSlot, slot, got, slot, buf.String())
		}
		clk.advance(reloadThrottle)
	}
	if !s.cold() {
		t.Fatal("a failing database warmed the replica")
	}
}

// The cold ceiling is ONE deadline for the whole resolution, from entry, across every
// wait the caller takes — not one per wait. A caller that spent the bound parked on the
// boot reload and then found that reload too old to answer from must NOT park again for
// another bound: it answers errColdReloadWait at once. Twice the ceiling would double
// both the client timeout the docs say to size for and the goroutine hold an anonymous
// flood buys.
//
// Structural, not a stopwatch: the row's onColdWait hook SPENDS the budget on the
// caller's own goroutine (a sleep well past the bound — it can only overrun, which only
// makes the deadline more spent), then releases the boot reload and waits for it to fail,
// so when the caller wakes the deadline is provably behind it. The assertion is that
// onColdWait fired exactly ONCE: a fresh deadline per wait would park the caller a second
// time for the throttle slot.
func TestIngressKeyPg_ColdWaitsShareOneDeadline(t *testing.T) {
	now, _ := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	hf := &hangFailDB{keyDB: testPool(t), blocked: make(chan struct{}), gate: make(chan struct{})}
	t.Cleanup(hf.release)
	s := NewIngressKeyStore(hf, "holder", now)
	const bound = 30 * time.Millisecond
	s.coldWaitBound = bound
	hf.arm.Store(true)
	bootDone := make(chan struct{})
	go func() { s.reload(now()); close(bootDone) }()
	mustSignal(t, hf.blocked, "the boot reload's query")

	var waits atomic.Int32
	s.onColdWait = func() {
		if waits.Add(1) != 1 {
			return
		}
		time.Sleep(4 * bound) // spend the whole budget (and then some) inside the first wait
		hf.release()          // the boot reload fails: too old to answer from, and the replica stays cold
		<-bootDone
	}
	got := verifyAsync(s, "0123456789abcdef0123456789abcdef", now())
	var ans coldAnswer
	select {
	case ans = <-got:
	case <-time.After(5 * time.Second):
		// Not a threshold on anything the code does: with one deadline the answer
		// follows the hook's sleep at once, and a caller given a fresh bound per wait
		// parks for the throttle slot (a real second) again and again against this
		// frozen clock, so the row would otherwise hang instead of failing.
		t.Fatal("the cold caller did not come back within 5 s of a deadline that had already passed: each wait is being given a deadline of its own")
	}
	if ans.ok {
		t.Fatal("resolved a kid no reload ever delivered")
	}
	if !errors.Is(ans.err, errColdReloadWait) {
		t.Fatalf("a cold caller past its deadline answered %v; want %v", ans.err, errColdReloadWait)
	}
	if n := waits.Load(); n != 1 {
		t.Fatalf("the caller parked %d times; want exactly 1 — a second park after the deadline had passed means each wait was given a deadline of its own", n)
	}
	if s.inFlight() != nil {
		t.Fatal("the boot reload had not completed when the caller answered: the row measured the parked reload, not the composed deadline")
	}
}

// A reload that COMPLETES between a cold caller's read of the state and its park must
// still wake it. The caller reads the epoch channel together with the counters and parks
// on THAT channel, so a completion in between has closed the very channel it holds; a
// caller that re-read the channel after the completion would hold the next epoch and
// park until its deadline for a key that is already in the cache.
//
// Deterministic: onColdWait fires after the state read and before the park, and this row
// uses it to release the parked boot reload and WAIT for it to land — cache filled, epoch
// closed — so the completion provably falls inside that window. The bound is injected
// large, so a lost wakeup shows as the answer not arriving, not as a 503 two seconds later.
func TestIngressKeyPg_ColdReplicaWhoseReloadLandsBeforeItParksIsNotLeftWaiting(t *testing.T) {
	pool := testPool(t)
	now, _ := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	a := NewIngressKeyStore(pool, "holder", now)
	kid, key, err := a.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	hung := &hungDB{keyDB: pool, blocked: make(chan struct{}), gate: make(chan struct{})}
	t.Cleanup(hung.release)
	b := NewIngressKeyStore(hung, "holder", now)
	b.coldWaitBound = 30 * time.Second
	hung.hang.Store(true)
	bootDone := make(chan struct{})
	go func() { b.refreshOnce(now()); close(bootDone) }()
	mustSignal(t, hung.blocked, "the boot reload's query")
	var waits atomic.Int32
	b.onColdWait = func() {
		if waits.Add(1) != 1 {
			return
		}
		hung.release()
		<-bootDone
		if b.inFlight() != nil || b.cold() {
			t.Error("the boot reload had not landed inside the window: the row is not exercising the completion-before-park case")
		}
	}

	got := verifyAsync(b, kid, now())
	var ans coldAnswer
	select {
	case ans = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the cold caller did not answer within 5 s: it parked on an epoch that had already been replaced (a lost wakeup) and is waiting out its bound for a key that is in the cache")
	}
	if !ans.ok || ans.err != nil {
		t.Fatalf("a live sibling's bearer at a cold replica whose reload landed just before it parked = ok:%v err:%v; want the key", ans.ok, ans.err)
	}
	if !ans.pub.Equal(&key.PublicKey) {
		t.Fatal("resolved the wrong public key")
	}
	if n := waits.Load(); n != 1 {
		t.Fatalf("the caller parked %d times; want exactly 1", n)
	}
}

// A cold caller that finds reloadMu held but NO latch published — the instant between a
// reload taking the lock and stamping its start — parks, exactly as it does behind a
// latch, and is woken by that reload's completion. It neither answers 401 from the
// unloaded cache nor spins: every pass of the cold loop that resolves nothing parks.
//
// Deterministic: the row itself holds reloadMu (which is what a reload does in that
// instant), so the window is open for as long as the row wants it; the caller announces
// its park through onColdWait; the row then lets go and runs the reload the holder would
// have run.
func TestIngressKeyPg_ColdReplicaParksWhileAReloadHoldsTheLockBeforeItStamps(t *testing.T) {
	pool := testPool(t)
	now, _ := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	a := NewIngressKeyStore(pool, "holder", now)
	kid, key, err := a.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	b := NewIngressKeyStore(pool, "holder", now)
	b.coldWaitBound = 30 * time.Second
	parked := make(chan struct{}, 4)
	var waits atomic.Int32
	b.onColdWait = func() { waits.Add(1); parked <- struct{}{} }
	b.reloadMu.Lock() // a reload holds the lock and has not published its latch
	var unlock sync.Once
	t.Cleanup(func() { unlock.Do(b.reloadMu.Unlock) })
	if b.inFlight() != nil {
		t.Fatal("a latch is published: the fixture is not the pre-stamp window")
	}

	got := verifyAsync(b, kid, now())
	mustSignal(t, parked, "the cold caller's park behind the held lock")
	select {
	case ans := <-got:
		t.Fatalf("a cold replica answered ok:%v err:%v while a reload held reloadMu with no latch published — a live sibling bearer refused in that window on every rolling deploy", ans.ok, ans.err)
	default: // parked, which is the whole contract
	}

	unlock.Do(b.reloadMu.Unlock)
	if err := b.reload(now()); err != nil { // what the lock holder was about to do
		t.Fatal(err)
	}
	ans := <-got
	if !ans.ok || ans.err != nil {
		t.Fatalf("a live sibling bearer at a cold replica = ok:%v err:%v; want it accepted once the reload that held the lock landed", ans.ok, ans.err)
	}
	if !ans.pub.Equal(&key.PublicKey) {
		t.Fatal("resolved the wrong public key")
	}
	if n := waits.Load(); n != 1 {
		t.Fatalf("the caller parked %d times; want exactly 1 (one park, one wakeup by the completion)", n)
	}
}

// …and that park is BOUNDED too. A lock holder that never stamps, never publishes and
// never lets go must not hold a cold caller past the bound, and what it gets there is the
// 503-class "could not tell" — never the 401, and never a spin.
//
// Structural: nothing ever published a latch, so no reload could have completed, and the
// bound is the only way the caller came back.
func TestIngressKeyPg_ColdReplicaParkedBehindAHeldLockIsBounded(t *testing.T) {
	pool := testPool(t)
	now, _ := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	a := NewIngressKeyStore(pool, "holder", now)
	kid, _, err := a.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	b := NewIngressKeyStore(pool, "holder", now)
	b.coldWaitBound = 20 * time.Millisecond
	var waits atomic.Int32
	b.onColdWait = func() { waits.Add(1) }
	b.reloadMu.Lock()
	t.Cleanup(b.reloadMu.Unlock)

	pub, ok, verr := b.VerificationKey(kid, now())
	if ok || pub != nil {
		t.Fatalf("resolved a kid no reload ever delivered: ok:%v", ok)
	}
	if verr == nil {
		t.Fatal("a cold replica behind a lock nobody released returned (nil,false,nil) — a 401 for a live bearer out of a cache that has never been loaded; want the store-unavailable tri-state (503)")
	}
	if !errors.Is(verr, errColdReloadWait) {
		t.Fatalf("cold bound error = %v; want %v", verr, errColdReloadWait)
	}
	if b.inFlight() != nil {
		t.Fatal("a latch was published after all: the row measured a reload, not the bound")
	}
	if n := waits.Load(); n != 1 {
		t.Fatalf("the caller parked %d times; want exactly 1 (parked once, for the whole bound — not spinning)", n)
	}
}

// The generation and the key are read in ONE cache-lock section, counters first. A reload
// installs its snapshot and completes in two separate sections, so a caller that took its
// lookup BEFORE reading the counters could see "a reload younger than me has completed"
// while holding a miss taken before that reload installed the key — and answer a bare 401
// for a kid that is in its own cache. Read together, the key is at least as young as the
// generation the caller trusts.
//
// Deterministic: the caller parks (onColdWait) on a cold replica whose reload failed; the
// row then lands a younger reload the way reloadQuery and reloadLocked do — the key
// installed and the completion stamped — under ONE hold of the cache lock, and closes the
// epoch. The caller's next pass must answer the key from that single section. (The
// two-lock ordering this replaces could not be forced into its gap from outside without
// a seam of its own, so this row pins the outcome, not the interleaving.)
func TestIngressKeyPg_ColdCallerReadsTheGenerationAndTheKeyTogether(t *testing.T) {
	pool := testPool(t)
	now, _ := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	a := NewIngressKeyStore(pool, "holder", now)
	kid, key, err := a.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	down := &downDB{keyDB: pool}
	down.down.Store(true)
	b := NewIngressKeyStore(down, "holder", now)
	b.coldWaitBound = 30 * time.Second
	if err := b.reload(now()); err == nil { // the boot reload fails: cold, throttle armed
		t.Fatal("the fixture's boot reload did not fail")
	}
	if !b.cold() {
		t.Fatal("the fixture is not a cold replica")
	}
	parked := make(chan struct{}, 4)
	var waits atomic.Int32
	b.onColdWait = func() { waits.Add(1); parked <- struct{}{} }

	got := verifyAsync(b, kid, now())
	mustSignal(t, parked, "the cold caller's park for the throttle slot")
	// A reload younger than the request lands: the key installed and the completion
	// stamped in one hold of the lock, exactly the state the caller's next pass reads.
	a.mu.RLock()
	installed := a.keys[kid]
	a.mu.RUnlock()
	b.mu.Lock()
	b.keys[kid] = installed
	b.loaded = true
	b.reloadStarted++
	b.reloadDone++
	b.lastReloadErr = nil
	epoch := b.reloadEpoch
	b.reloadEpoch = make(chan struct{})
	b.mu.Unlock()
	close(epoch)

	ans := <-got
	if !ans.ok || ans.err != nil {
		t.Fatalf("a kid installed by a reload younger than the request = ok:%v err:%v; want the key — a bare 401 here refuses a bearer that is in the replica's own cache", ans.ok, ans.err)
	}
	if !ans.pub.Equal(&key.PublicKey) {
		t.Fatal("resolved the wrong public key")
	}
	if n := waits.Load(); n != 1 {
		t.Fatalf("the caller parked %d times; want exactly 1", n)
	}
}

// ctxHungDB parks Query until the query's OWN context expires (or release()), and records
// the deadline that context carried — so a row can prove, structurally, what ceiling the
// store gave a round trip rather than time it.
type ctxHungDB struct {
	keyDB
	hang        atomic.Bool
	blocked     chan struct{}
	gate        chan struct{}
	onceBlocked sync.Once
	onceRelease sync.Once
	mu          sync.Mutex
	entered     time.Time // wall clock when the parked query was entered
	deadline    time.Time
	byCtx       bool
}

func (d *ctxHungDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if !d.hang.Load() {
		return d.keyDB.Query(ctx, sql, args...)
	}
	dl, _ := ctx.Deadline()
	d.mu.Lock()
	d.entered, d.deadline = time.Now(), dl
	d.mu.Unlock()
	d.onceBlocked.Do(func() { close(d.blocked) })
	select {
	case <-d.gate:
		return d.keyDB.Query(ctx, sql, args...)
	case <-ctx.Done():
		d.mu.Lock()
		d.byCtx = true
		d.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (d *ctxHungDB) release() { d.onceRelease.Do(func() { close(d.gate) }) }

// The single bound covers the query a cold caller runs ITSELF, not only its waits. A
// caller that wins the throttle slot against a database that accepts the connection and
// never answers must come back at ITS bound with the 503-class errColdReloadWait — not one
// full storeTimeout later, which would put the real ceiling at the bound plus two seconds
// while the docs state one store timeout from arrival. The round trip is given what is
// left of the bound as its context.
//
// Structural, not a stopwatch: the database records the deadline the query's context
// carried and the instant the query was entered, and the row asserts the deadline is at
// most the injected bound past that instant — the context was made before the query was
// entered, so a slow scheduler can only make the comparison truer — and that the query
// ended by that context rather than by anything the row did.
func TestIngressKeyPg_ColdCallersOwnReloadRunsInsideItsBound(t *testing.T) {
	now, _ := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	db := &ctxHungDB{keyDB: testPool(t), blocked: make(chan struct{}), gate: make(chan struct{})}
	t.Cleanup(db.release)
	s := NewIngressKeyStore(db, "holder", now)
	const bound = 50 * time.Millisecond
	s.coldWaitBound = bound
	db.hang.Store(true)
	if !s.cold() || s.throttled(now()) {
		t.Fatal("the fixture must be cold with the slot open, so the caller runs the reload itself")
	}

	_, ok, verr := s.VerificationKey("0123456789abcdef0123456789abcdef", now())
	if ok {
		t.Fatal("resolved a kid no reload ever delivered")
	}
	if !errors.Is(verr, errColdReloadWait) {
		t.Fatalf("a cold caller whose own query outlived its bound answered %v; want %v", verr, errColdReloadWait)
	}
	mustSignal(t, db.blocked, "the caller's own reload query")
	db.mu.Lock()
	entered, dl, byCtx := db.entered, db.deadline, db.byCtx
	db.mu.Unlock()
	if !byCtx {
		t.Fatal("the query did not end by its own context: the row measured something else")
	}
	if dl.IsZero() || dl.Sub(entered) > bound {
		t.Fatalf("the cold caller's own query was given a context deadline %v past the moment it was entered; want at most the %v bound left — the query got a fresh storeTimeout of its own", dl.Sub(entered), bound)
	}
}

// The no-wait reload in isolation: inside the throttle window it declines with
// errReloadThrottled and issues no query; behind a reload in flight it declines with
// errReloadInFlight. Both states share it — a cold caller's business with a decline is to
// park (resolveCold), never to query around it — so there is one rule to pin.
func TestIngressKeyPg_NoWaitReloadDeclinesInsideTheThrottleAndBehindAReloadInFlight(t *testing.T) {
	pool := testPool(t)
	now, _ := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	db := &countingDB{keyDB: pool}
	s := NewIngressKeyStore(db, "holder", now)
	if err := s.reload(now()); err != nil { // arms the throttle, publishes no latch
		t.Fatal(err)
	}
	atomic.StoreInt32(&db.queries, 0)
	if !s.throttled(now()) {
		t.Fatal("the fixture's throttle is not armed")
	}
	if ran, err := s.reloadIfDueNoWait(now()); ran || !errors.Is(err, errReloadThrottled) {
		t.Fatalf("inside the throttle = ran:%v err:%v; want false,%v", ran, err, errReloadThrottled)
	}
	if got := atomic.LoadInt32(&db.queries); got != 0 {
		t.Fatalf("queried inside the throttle window (%d queries)", got)
	}

	inflight := &hungDB{keyDB: pool, blocked: make(chan struct{}), gate: make(chan struct{})}
	t.Cleanup(inflight.release)
	f := NewIngressKeyStore(inflight, "holder", now)
	inflight.hang.Store(true)
	go f.reload(now())
	mustSignal(t, inflight.blocked, "the parked reload's query")
	if ran, err := f.reloadIfDueNoWait(now()); ran || !errors.Is(err, errReloadInFlight) {
		t.Fatalf("with a reload in flight = ran:%v err:%v; want false,%v", ran, err, errReloadInFlight)
	}
}

// countingSlowDB counts SELECT round trips and advances the injected clock by `by`
// while each one runs — a query that occupies more than the throttle window.
type countingSlowDB struct {
	keyDB
	queries int32
	advance func(time.Duration)
	by      time.Duration
}

func (d *countingSlowDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	atomic.AddInt32(&d.queries, 1)
	d.advance(d.by)
	return d.keyDB.Query(ctx, sql, args...)
}

// The throttle admits one query per second per replica — counted from when the reload
// FINISHES, not from when its caller started. A reload that burned the whole storeTimeout
// leaves the next caller outside a window stamped with the start time, so a hung database
// admits one caller per second who each then wait storeTimeout of their own.
//
// WARM, for the reason the row above states: the throttle is the warm miss path's
// protection; a cold replica parks for a reload younger than the request instead.
func TestIngressKeyPg_ThrottleCountsFromTheReloadsCompletion(t *testing.T) {
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	db := &countingSlowDB{keyDB: testPool(t), advance: advance, by: 2 * reloadThrottle}
	s := NewIngressKeyStore(db, "holder", now)
	// Mint first, so the cache has been read, then step past that reload's own window:
	// what this row measures starts at the first miss.
	if _, _, err := s.SigningKey(now()); err != nil {
		t.Fatal(err)
	}
	advance(reloadThrottle)
	atomic.StoreInt32(&db.queries, 0)
	// First miss: one reload, whose query occupies twice the throttle window.
	if _, ok, err := s.VerificationKey("0123456789abcdef0123456789abcdef", now()); ok || err != nil {
		t.Fatalf("unknown kid = ok:%v err:%v; want false,nil", ok, err)
	}
	// A different kid at the instant that reload completed: inside the window, so it
	// must be rejected WITHOUT a second query.
	if _, ok, err := s.VerificationKey("fedcba9876543210fedcba9876543210", now()); ok || err != nil {
		t.Fatalf("unknown kid = ok:%v err:%v; want false,nil", ok, err)
	}
	if got := atomic.LoadInt32(&db.queries); got != 1 {
		t.Fatalf("reload queries = %d, want 1 — the throttle window is stamped from the caller's start time, so a slow query lets the next caller straight through", got)
	}
}

// hangFailDB parks the first Query once armed and then FAILS it: a database that stops
// answering mid-reload. Begin is counted (never parked), so a row can prove that a caller
// which waited on that reload did NOT go on to attempt an insert against it.
type hangFailDB struct {
	keyDB
	arm         atomic.Bool
	blocked     chan struct{}
	gate        chan struct{}
	begins      int32
	onceBlocked sync.Once
	onceRelease sync.Once
}

func (d *hangFailDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if d.arm.Load() {
		d.onceBlocked.Do(func() { close(d.blocked) })
		<-d.gate
		return nil, errDownDB
	}
	return d.keyDB.Query(ctx, sql, args...)
}

func (d *hangFailDB) Begin(ctx context.Context) (pgx.Tx, error) {
	atomic.AddInt32(&d.begins, 1)
	return d.keyDB.Begin(ctx)
}

func (d *hangFailDB) release() { d.onceRelease.Do(func() { close(d.gate) }) }

// A caller that waits on another goroutine's reload must see that reload's OUTCOME, not
// just its completion. SigningKey's slow path is the ONE path that still waits — an
// authenticated /oauth/token call owes a bearer or an honest 503, and its caller is a
// registered client rather than an anonymous flood — and it is the path that pays for the
// difference: rotation is due, the database is gone, and a waiter told only "the reload
// finished" skips both grace-sign branches and falls into insertKey against the dead
// database, so /oauth/token answers 503 inside the very slack the store exists to bridge.
//
// The wait is driven at its own seam: a latch is published carrying a failed reload's
// error, exactly as reloadLocked leaves one when its round trip comes back with the
// database gone (err written BEFORE done is closed — that ordering is the latch's whole
// contract). Nothing here sleeps, races or polls for another goroutine to park; what the
// row pins is which branch the waiter's outcome selects, which is what regressed.
func TestIngressKeyPg_SigningWaiterTakesTheFailedReloadsGraceBranch(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	db := &hangFailDB{keyDB: pool, blocked: make(chan struct{}), gate: make(chan struct{})}
	t.Cleanup(db.release)
	s := NewIngressKeyStore(db, "holder", now)
	kid1, _, err := s.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	advance(ingressKeyRotation) // rotation due, and kid1 is still inside its slack
	atomic.StoreInt32(&db.begins, 0)

	// The reload this caller waits on came back with the database gone.
	l := &reloadLatch{done: make(chan struct{}), err: errors.New("dial: connection refused")}
	close(l.done) // err is written BEFORE the close, exactly as reloadLocked does
	s.mu.Lock()
	s.inflight = l
	s.mu.Unlock()

	kid, _, err := s.SigningKey(now())
	if err != nil {
		t.Fatalf("SigningKey after waiting on a FAILED reload = %v; want the grace-sign branch the reloader itself would have taken", err)
	}
	if kid != kid1 {
		t.Fatalf("grace-signed with %q, want the current key %q", kid, kid1)
	}
	if n := atomic.LoadInt32(&db.begins); n != 0 {
		t.Fatalf("insert attempts against the failed database = %d, want 0 (the waiter treated the failed reload as a healthy one)", n)
	}
	// A latch reporting SUCCESS must take the OTHER branch: rotation is still due and the
	// snapshot carried no key inside its rotation life, so the slow path goes on to mint.
	// Without this half the row above would pass for a SigningKey that grace-signs no
	// matter what the latch said — which would stop rotation for good.
	good := &reloadLatch{done: make(chan struct{})}
	close(good.done)
	s.mu.Lock()
	s.inflight = good
	s.mu.Unlock()
	kid2, _, err := s.SigningKey(now())
	if err != nil {
		t.Fatalf("SigningKey after waiting on a SUCCESSFUL reload: %v", err)
	}
	if kid2 == kid1 {
		t.Fatal("a waiter told the reload SUCCEEDED grace-signed instead of rotating: the outcome is not being read from the latch")
	}
	if n := atomic.LoadInt32(&db.begins); n != 1 {
		t.Fatalf("insert attempts = %d, want exactly 1 (the successful-reload branch mints; the failed one must not)", n)
	}
}

// The background refresh is the one key-store failure nothing else can report: no request
// is waiting on it, so there is no error to return, and a replica with a warm cache and no
// verification misses keeps serving normally while it silently stops rotating. The loop
// counts it itself — and only it: the request-path failures are returned to the engine,
// which counts them once, and a second count here would report one outage as two.
//
// The ticker itself stays untested (as before); this drives its body.
func TestIngressKeyPg_RefreshTickCountsOnlyItsOwnFailure(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	db := &downDB{keyDB: pool}
	s := NewIngressKeyStore(db, "holder", now)
	var stores []string
	hook := func(store string) { stores = append(stores, store) }

	// A healthy tick reloads, rotates and counts nothing.
	s.refreshTick(now(), hook)
	if len(stores) != 0 {
		t.Fatalf("a healthy refresh counted %v, want nothing", stores)
	}
	kid, _, err := s.SigningKey(now())
	if err != nil || kid == "" {
		t.Fatalf("the healthy tick must have minted the holder's key: kid=%q err=%v", kid, err)
	}

	// The database goes away. The cache is warm and no request misses, so this tick is the
	// only thing that knows — and it must say so exactly once, under the same store name
	// the engine reports the request-path failures under.
	db.down.Store(true)
	advance(ingressKeyRefresh)
	s.refreshTick(now(), hook)
	if len(stores) != 1 || stores[0] != storeNameIngressKey {
		t.Fatalf("store-error hook calls after a failed refresh = %v, want exactly [%s] — a silently failing refresh loop is an outage no alarm sees", stores, storeNameIngressKey)
	}
	// A cached kid still verifies through it, which is exactly why nothing else reports it.
	if _, ok, verr := s.VerificationKey(kid, now()); !ok || verr != nil {
		t.Fatalf("cached kid during the refresh outage = ok:%v err:%v; want true,nil", ok, verr)
	}
	if len(stores) != 1 {
		t.Fatalf("a cache hit counted a store error: %v", stores)
	}
	// A gateway with no metrics emitter passes no hook: the loop must not panic on it.
	advance(ingressKeyRefresh)
	s.refreshTick(now(), nil)
}

// mustSignal waits for a background reload's signal with a ceiling well under the 30 s
// refresh tick. The ceiling is not a threshold on anything the code does — the reload it
// waits for is issued the instant the wake is drained — it is there so that a miss which
// kicks NOTHING fails this row instead of passing 30 s later on the ticker, which would
// make the row green for a mechanism it is not testing.
func mustSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not happen within 5s — for a miss-driven reload that means nothing was kicked (the 30s refresh tick is not the mechanism under test, and a row that only went green on the tick would be green for something it does not test). 5s leaves room for the throttle remainder the refresh goroutine waits out on its own", what)
	}
}

// startRefresh starts the store's refresh loop the way app.Run does, and publishes its
// wake-draining state to THIS goroutine before returning. RunRefresh sets `refreshing`
// from inside the loop, so a row that raced its own first request against the goroutine's
// start would silently exercise the no-loop fallback branch instead of the shipped path —
// a stand-in quietly standing in for something else. Setting the flag here removes the
// race without changing what the loop does (it sets the same flag on entry).
func startRefresh(t *testing.T, s *IngressKeyStore, onErr func(string)) {
	t.Helper()
	s.refreshing.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.RunRefresh(ctx, onErr); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

// settleReload returns once the reload in flight, if any, has run its completion section:
// afterNextReload's signal hangs off rows.Close, which runs after the cache merge but
// BEFORE reloadLocked's completion stamps the outcome and reads the injected clock, so a
// row that advances a plain fixedClock right after the signal races that read. Waiting
// on the latch (or observing it cleared under mu) is the happens-before edge.
func settleReload(s *IngressKeyStore) {
	if l := s.inFlight(); l != nil {
		<-l.done
	}
}

// reloadSignalDB counts SELECT round trips and closes a channel once a reload has read its
// rows AND swapped the caches, so a row can wait for a BACKGROUND reload to have taken
// effect without polling or sleeping. The signal hangs off rows.Close(), which reloadQuery
// defers BEFORE it takes the cache lock — deferred calls run last-in-first-out, so Close
// lands after the merge, never in the middle of it.
type reloadSignalDB struct {
	keyDB
	queries int32
	mu      sync.Mutex
	waiters []chan struct{}
}

// afterNextReload returns a channel closed once one more reload has taken effect.
func (d *reloadSignalDB) afterNextReload() <-chan struct{} {
	ch := make(chan struct{})
	d.mu.Lock()
	d.waiters = append(d.waiters, ch)
	d.mu.Unlock()
	return ch
}

func (d *reloadSignalDB) fire() {
	d.mu.Lock()
	waiters := d.waiters
	d.waiters = nil
	d.mu.Unlock()
	for _, ch := range waiters {
		close(ch)
	}
}

func (d *reloadSignalDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	atomic.AddInt32(&d.queries, 1)
	rows, err := d.keyDB.Query(ctx, sql, args...)
	if err != nil {
		d.fire() // nothing will be scanned or closed: this reload is already over
		return nil, err
	}
	return &signalRows{Rows: rows, onClose: d.fire}, nil
}

// signalRows fires onClose exactly once, when the reload closes its rows.
type signalRows struct {
	pgx.Rows
	onClose func()
	once    sync.Once
}

func (r *signalRows) Close() {
	r.Rows.Close()
	r.once.Do(r.onClose)
}

// The SHIPPED shape, on a replica that has already read the table: a sibling ROTATES, and
// the kid it mints cannot be in this replica's snapshot. The miss is refused ONCE —
// immediately, holding nothing, issuing no query of its own — and the kid verifies on the
// next request once the out-of-band reload lands. This is the whole cost of never parking a
// caller, and DEPLOYMENT.md states it in the same terms.
func TestIngressKeyPg_OutOfBandReloadResolvesASiblingsRotatedKidOnTheNextCall(t *testing.T) {
	pool := testPool(t)
	clk := newLockedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)) // a live loop reads it
	now, advance := clk.now, clk.advance
	sib := NewIngressKeyStore(pool, "holder", now)
	if _, _, err := sib.SigningKey(now()); err != nil {
		t.Fatal(err)
	}
	db := &reloadSignalDB{keyDB: pool}
	s := NewIngressKeyStore(db, "holder", now)
	// The refresh loop's warm-up reload puts the holder's current key in this replica's
	// cache, so it is NOT the cold replica the row below is about: the exception that
	// covers a restart does not apply, and this row measures the steady-state contract.
	warmed := db.afterNextReload()
	startRefresh(t, s, nil)
	mustSignal(t, warmed, "the refresh loop's warm-up reload")
	settleReload(s) // the signal fires on rows.Close, before the completion reads the clock this row is about to advance
	if s.cacheEmpty() {
		t.Fatal("the warm-up reload did not fill the cache: this row would measure the cold-replica exception instead")
	}

	// The sibling rotates. The new kid did not exist when this replica last read the table.
	advance(ingressKeyRotation)
	kid, key, err := sib.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	reloaded := db.afterNextReload()
	if _, ok, verr := s.VerificationKey(kid, now()); ok || verr != nil {
		t.Fatalf("a sibling's rotated kid = ok:%v err:%v; want false,nil answered at once", ok, verr)
	}
	mustSignal(t, reloaded, "the out-of-band reload") // the wake the miss kicked was drained
	pub, ok, verr := s.VerificationKey(kid, now())
	if !ok || verr != nil {
		t.Fatalf("the same kid after the out-of-band reload = ok:%v err:%v; want it resolved", ok, verr)
	}
	if !pub.Equal(&key.PublicKey) {
		t.Fatal("resolved the wrong public key")
	}
	if got := atomic.LoadInt32(&db.queries); got != 2 {
		t.Fatalf("reload queries = %d, want 2 (the loop's warm-up and the out-of-band reload) — the request path must issue none of its own", got)
	}
}

// A RESTARTED replica must accept a live bearer on its FIRST call. Its cache is empty, so
// it cannot tell an unknown kid from one it has not read yet, and answering 401 there
// refuses valid traffic on every rolling deploy — the case a shared key store exists to
// make safe. Two things close it, and this row drives both halves:
//
//   - the refresh loop reloads once BEFORE its first tick, so a replica that restarts while
//     the holder's key already exists comes up warm;
//   - and a replica on which no reload has completed resolves the kid through a reload
//     younger than the request — its own, when no loop is running — rather than deferring
//     it, which is what covers a replica whose first request beats its warm-up.
func TestIngressKeyPg_RestartedReplicaAcceptsALiveBearerOnItsFirstCall(t *testing.T) {
	pool := testPool(t)
	now, _ := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	minting := NewIngressKeyStore(pool, "holder", now)
	kid, key, err := minting.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}

	// (a) The restarted replica: a brand-new store over the same DSN, warmed by the loop.
	db := &reloadSignalDB{keyDB: pool}
	restarted := NewIngressKeyStore(db, "holder", now)
	w := db.afterNextReload()
	startRefresh(t, restarted, nil)
	mustSignal(t, w, "the refresh loop's warm-up reload")
	pub, ok, verr := restarted.VerificationKey(kid, now())
	if !ok || verr != nil {
		t.Fatalf("a live bearer at a restarted replica = ok:%v err:%v; want it accepted on the FIRST call (every rolling deploy refuses traffic otherwise)", ok, verr)
	}
	if !pub.Equal(&key.PublicKey) {
		t.Fatal("resolved the wrong public key")
	}

	// (b) The cold replica with no loop at all (and no warm-up): it must still resolve the
	// kid itself rather than answer "unknown" from a cache it has never filled.
	cold := NewIngressKeyStore(pool, "holder", now)
	if !cold.cold() {
		t.Fatal("the fixture is not a cold replica")
	}
	pub, ok, verr = cold.VerificationKey(kid, now())
	if !ok || verr != nil {
		t.Fatalf("a live bearer at a replica that has never read the table = ok:%v err:%v; want it resolved, not refused from an empty cache", ok, verr)
	}
	if !pub.Equal(&key.PublicKey) {
		t.Fatal("resolved the wrong public key")
	}
	// …and once warm, the exception is over: the steady-state contract applies again.
	if cold.cold() {
		t.Fatal("the cold replica's own reload completed but did not warm it, so the exception would apply for ever")
	}
}

// The liveness fence. 200 concurrent misses with distinct well-formed kids against a
// database that never answers must ALL come back at once: this is the unauthenticated
// path (jwt/v5 runs the keyfunc before it validates a signature, and validKID admits any
// 32-hex string), so a wait here is a goroutine an anonymous caller gets to hold, and the
// old shape held one for reloadThrottle + storeTimeout apiece.
//
// "At once" is asserted STRUCTURALLY, not on a stopwatch: the store announces every caller
// that parks for a reload through onColdWait, and the fence is that it
// fired ZERO times across the burst. A wall-clock ceiling here would be a threshold on a
// scheduler — under -race on a two-vCPU runner, 200 goroutines can take longer than any
// honest ceiling without the code having waited on anything.
//
// The store is WARM (it minted a key before the hung reload started), which is the state
// this fence is about: a cold replica — one on which no reload has ever completed
// successfully — deliberately DOES park, bounded, for a reload younger than the request
// rather than refuse a live sibling's bearer, and that exception has its own rows above.
//
// Which also means this row cannot catch a regression in the cold GUARD it is paired
// with. With RunRefresh running, its 200 callers take the warm path's `missOutOfBand ||
// refreshing` return and never reach the cold branch at all, so a cold test dropped from
// that branch would leave this row green. The row that reds on it is
// TestIngressKeyPg_MissDuringAnInFlightReloadAnswersAtOnceAndResolvesNext — warm, loop
// not running, and onColdWait must not fire — and the pair is the whole coverage: this
// one is the liveness bill, that one is the guard.
func TestIngressKeyPg_ConcurrentMissesNeverParkAgainstAHungDatabase(t *testing.T) {
	pool := testPool(t)
	now, _ := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	hung := &hungDB{keyDB: pool, blocked: make(chan struct{}), gate: make(chan struct{})}
	t.Cleanup(hung.release)
	counted := &countingDB{keyDB: hung}
	s := NewIngressKeyStore(counted, "holder", now)
	var parked atomic.Int32
	s.onColdWait = func() { parked.Add(1) }
	// Mint first, so the cache has been read and the burst below exercises the warm miss
	// path this fence is named for. That mint's own reload is not part of what the burst
	// costs, so the query counter starts again from zero here — nothing is concurrent yet.
	if _, _, err := s.SigningKey(now()); err != nil {
		t.Fatal(err)
	}
	atomic.StoreInt32(&counted.queries, 0)
	hung.hang.Store(true)

	startRefresh(t, s, nil)
	// Registered AFTER startRefresh, so it runs BEFORE it (cleanups are LIFO): the refresh
	// goroutine is parked in the hung query and only release() can free it, so waiting for
	// the loop to exit without releasing first would hang the row.
	t.Cleanup(hung.release)
	// …and the burst must not start until that warm-up reload is provably INSIDE the query,
	// so every one of the 200 finds a reload in flight — the property this row measures.
	mustSignal(t, hung.blocked, "the refresh loop's warm-up reload query")

	const n = 200
	var wg sync.WaitGroup
	start := make(chan struct{})
	var bad atomic.Int32
	for i := 0; i < n; i++ {
		kid := fmt.Sprintf("%032x", i+1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, ok, err := s.VerificationKey(kid, now()); ok || err != nil {
				bad.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatalf("%d of %d misses answered something other than (nil,false,nil)", bad.Load(), n)
	}
	if got := parked.Load(); got != 0 {
		t.Fatalf("%d of %d concurrent unknown-kid misses parked for the in-flight reload against a hung database — the miss path is holding one goroutine per anonymous request", got, n)
	}
	// …and the burst costs exactly ONE reload query — the warm-up the row started with,
	// still parked. Nothing in the burst added a second: the wake they kicked cannot be
	// drained while the refresh goroutine is held in that query, and none of them queried
	// around it.
	if got := atomic.LoadInt32(&counted.queries); got != 1 {
		t.Fatalf("reload queries for the burst = %d, want exactly 1", got)
	}
}

// The store-outage tri-state on the SHIPPED shape (RunRefresh running). A hermetic row
// that never starts the loop exercises a different branch, and a mirror that behaves
// differently from the deployment is how a hermetic green becomes a live red. Both shapes
// must end as "could not tell" — 503 — never as an unknown kid.
func TestIngressKeyPg_OutageIsReportedAsUnavailableWithTheRefreshLoopRunning(t *testing.T) {
	pool := testPool(t)
	now, _ := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	// The holder has a key and this replica has read it: the steady-state shape, where a
	// miss defers its reload rather than running one.
	minting := NewIngressKeyStore(pool, "holder", now)
	if _, _, err := minting.SigningKey(now()); err != nil {
		t.Fatal(err)
	}
	down := &downDB{keyDB: pool}
	db := &reloadSignalDB{keyDB: down}
	s := NewIngressKeyStore(db, "holder", now)
	// The loop's own error hook is the completion signal: wakeTick calls it AFTER the
	// reload has returned and stamped its outcome, which is the state the second miss below
	// must read. No polling, no sleeping.
	failed := make(chan struct{})
	var once sync.Once
	warmed := db.afterNextReload()
	startRefresh(t, s, func(string) { once.Do(func() { close(failed) }) })
	mustSignal(t, warmed, "the refresh loop's warm-up reload")
	if s.cacheEmpty() {
		t.Fatal("the warm-up reload did not fill the cache: this row would measure the cold-replica exception instead")
	}

	down.down.Store(true)
	// The first miss kicks the reload and answers from a snapshot that has not failed yet.
	if _, ok, err := s.VerificationKey("0123456789abcdef0123456789abcdef", now()); ok || err != nil {
		t.Fatalf("first miss = ok:%v err:%v; want false,nil (nothing has failed yet)", ok, err)
	}
	mustSignal(t, failed, "the out-of-band reload") // it ran, failed, and stamped its outcome
	const unknown = "fedcba9876543210fedcba9876543210"
	if _, ok, err := s.VerificationKey(unknown, now()); ok || err == nil {
		t.Fatalf("miss after a FAILED reload = ok:%v err:%v; want false and the outage error (the caller owes a 503, not a 401)", ok, err)
	}
	if _, neg := s.negCache[unknown]; neg {
		t.Fatal("a kid refused during an outage was negatively cached")
	}
	// And the COLD replica, which resolves the kid itself: an unreadable database there is
	// still an outage, reported on the very first call rather than mistaken for a bad kid.
	cold := NewIngressKeyStore(down, "holder", now)
	if _, ok, err := cold.VerificationKey(unknown, now()); ok || err == nil {
		t.Fatalf("cold replica over a dead database = ok:%v err:%v; want false and the outage error", ok, err)
	}
	if _, neg := cold.negCache[unknown]; neg {
		t.Fatal("an errored reload at a cold replica negatively cached the kid")
	}
}

// insertRawKeyRow writes a gw_ingress_key row straight through the pool — the shared table
// has more than one writer, and this is what any of them can put there.
func insertRawKeyRow(t *testing.T, pool *pgxpool.Pool, holder, kid, pemStr string, created, notAfter time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO gw_ingress_key (holder_id, kid, private_key_pem, created_at, not_after) VALUES ($1,$2,$3,$4,$5)`,
		holder, kid, pemStr, created, notAfter); err != nil {
		t.Fatalf("insert key row: %v", err)
	}
}

// A key row on the wrong curve degrades exactly its own kid. The ingress bearer is ES384,
// which can only be produced with a P-384 key, and the shared key table has more than one
// writer — so a P-256 row that happens to be the NEWEST must not become this replica's
// signing key. If it did, every /oauth/token call would fail to sign; nothing else about
// the holder is wrong. The row is isolated the same way an unparsable one is: skipped and
// logged by kid once per reload, never failing the whole snapshot.
func TestIngressKeyPg_WrongCurveRowDegradesOnlyThatKid(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	s := NewIngressKeyStore(pool, "holder", now)
	good, goodKey, err := s.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}

	// A second writer inserts a P-256 row that is NEWER than the live P-384 one.
	p256, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(p256)
	if err != nil {
		t.Fatal(err)
	}
	const badKid = "0123456789abcdef0123456789abcdef"
	advance(time.Minute) // strictly newer than the good row
	insertRawKeyRow(t, pool, "holder", badKid, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		now(), now().Add(ingressKeyRotation))

	// The background refresh reads it and must NOT fail: one bad row is not an outage.
	if err := s.refreshOnce(now()); err != nil {
		t.Fatalf("refreshOnce failed on a wrong-curve row: %v — one unusable row must not freeze the snapshot", err)
	}
	kid, key, err := s.SigningKey(now())
	if err != nil {
		t.Fatalf("SigningKey after the wrong-curve row: %v", err)
	}
	if kid != good || !key.Equal(goodKey) {
		t.Fatalf("SigningKey = %s, want the newest P-384 key %s — a P-256 row cannot produce an ES384 bearer", kid, good)
	}
	// …and the bad kid verifies as UNKNOWN, not as a store failure: this replica ran its
	// own reload and that reload could read the table fine.
	advance(reloadThrottle)
	pub, ok, err := s.VerificationKey(badKid, now())
	if pub != nil || ok || err != nil {
		t.Fatalf("VerificationKey(wrong-curve kid) = (%v, %v, %v); want (nil, false, nil)", pub, ok, err)
	}
}
