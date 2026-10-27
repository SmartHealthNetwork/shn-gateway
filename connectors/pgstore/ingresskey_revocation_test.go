//go:build !shn_coreonly

package pgstore

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// deleteIngressKey retires kid the way the deployment guide tells an operator to: by
// deleting its row.
func deleteIngressKey(t *testing.T, pool *pgxpool.Pool, kid string) {
	t.Helper()
	tag, err := pool.Exec(context.Background(), `DELETE FROM gw_ingress_key WHERE holder_id='holder' AND kid=$1`, kid)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("deleting %s removed %d rows, want 1", kid, tag.RowsAffected())
	}
}

// A key whose row is deleted leaves the cache at the replica's next reload: it no longer
// verifies, and the replica stops signing with it and adopts a new key. Before that
// reload the cached key still verifies; that window is the one refresh interval the
// deployment guide states.
func TestIngressKeyPg_DeletedKeyLeavesTheCacheAtTheNextReload(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	s := NewIngressKeyStore(pool, "holder", now)
	kid, _, err := s.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	advance(reloadThrottle)
	if err := s.refreshOnce(now()); err != nil { // the key is now one a snapshot carried
		t.Fatal(err)
	}

	deleteIngressKey(t, pool, kid)
	if _, ok, err := s.VerificationKey(kid, now()); !ok || err != nil {
		t.Fatalf("before the next reload the cached key = ok:%v err:%v; want it still cached", ok, err)
	}
	advance(ingressKeyRefresh)
	if err := s.refreshOnce(now()); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.VerificationKey(kid, now()); ok || err != nil {
		t.Fatalf("after the next reload the deleted key = ok:%v err:%v; want refused (false, nil)", ok, err)
	}
	next, _, err := s.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	if next == kid {
		t.Fatalf("SigningKey still signs with the deleted key %s", kid)
	}
}

// A key this replica adopted and never saw in a snapshot is dropped too once a later
// reload's SELECT runs after it and does not find it: only a reload that may predate the
// key's commit keeps it.
func TestIngressKeyPg_OwnAdoptedKeyDeletedBeforeAnySnapshotLeavesAtTheNextReload(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	s := NewIngressKeyStore(pool, "holder", now)
	if _, _, err := s.SigningKey(now()); err != nil { // the boot reload, then the first key
		t.Fatal(err)
	}
	advance(ingressKeyRotation)
	kid, _, err := s.SigningKey(now()) // adopted here, cached by insert, in no snapshot yet
	if err != nil {
		t.Fatal(err)
	}
	if s.keys[kid].gen == 0 {
		t.Fatalf("the adopted key %s carries no cache generation", kid)
	}
	deleteIngressKey(t, pool, kid)
	advance(reloadThrottle)
	if err := s.reload(now()); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.VerificationKey(kid, now()); ok || err != nil {
		t.Fatalf("after a reload that ran past the adoption the deleted key = ok:%v err:%v; want refused", ok, err)
	}
}

// Another replica's new key is learned at the next reload, and the key deletion rule does
// not drop it once it is cached.
func TestIngressKeyPg_AnotherReplicasNewKeyIsLearnedAndKept(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	a := NewIngressKeyStore(pool, "holder", now)
	b := NewIngressKeyStore(pool, "holder", now)
	if _, _, err := a.SigningKey(now()); err != nil {
		t.Fatal(err)
	}
	advance(ingressKeyRotation)
	kidB, _, err := b.SigningKey(now()) // B rotates; A has not seen the new key
	if err != nil {
		t.Fatal(err)
	}
	if err := a.refreshOnce(now()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // learned at the first reload, kept at the next
		if _, ok, err := a.VerificationKey(kidB, now()); !ok || err != nil {
			t.Fatalf("reload %d: A does not verify B's new key %s (ok:%v err:%v)", i+1, kidB, ok, err)
		}
		advance(ingressKeyRefresh)
		if err := a.refreshOnce(now()); err != nil {
			t.Fatal(err)
		}
	}
}

// The deployment guide's revocation, end to end: a bearer A minted verifies at A and at B;
// once its key's row is deleted, one refresh tick at each replica and the same bearer is
// refused at both, and A's next token is signed with a new key that both verify.
func TestIngressKeyPg_RevokedKeyStopsVerifyingWithinOneRefreshOnEveryReplica(t *testing.T) {
	pool := testPool(t)
	clk := newLockedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	a := NewIngressKeyStore(pool, "holder", clk.now)
	b := NewIngressKeyStore(pool, "holder", clk.now)
	clientKey, pub := ackLostClientKey(t)
	srvA := adoptionReplica(t, pool, a, pub, clk.now)
	srvB := adoptionReplica(t, pool, b, pub, clk.now)
	mint := func(jti string) (int, string) {
		return adoptionToken(t, srvA, adoptionAssertion(t, clientKey, clk.now(), jti))
	}

	code, bearer := mint("before-the-revocation")
	if code != http.StatusOK {
		t.Fatalf("mint = %d", code)
	}
	revoked := a.signing
	replicas := map[string]*httptest.Server{"A": srvA, "B": srvB}
	for name, srv := range replicas {
		if got := adoptionDiscovery(t, srv, bearer); got != http.StatusOK {
			t.Fatalf("before the revocation %s answers %d to the bearer; want 200", name, got)
		}
	}

	deleteIngressKey(t, pool, revoked)
	clk.advance(ingressKeyRefresh)
	for name, s := range map[string]*IngressKeyStore{"A": a, "B": b} {
		if err := s.refreshOnce(clk.now()); err != nil {
			t.Fatalf("refresh at %s: %v", name, err)
		}
	}
	for name, srv := range replicas {
		if got := adoptionDiscovery(t, srv, bearer); got != http.StatusUnauthorized {
			t.Fatalf("one refresh after the revocation %s answers %d to the revoked key's bearer; want 401", name, got)
		}
	}

	code, fresh := mint("after-the-revocation")
	if code != http.StatusOK {
		t.Fatalf("mint after the revocation = %d", code)
	}
	if a.signing == revoked {
		t.Fatalf("A still signs with the revoked key %s", revoked)
	}
	for name, srv := range replicas {
		if got := adoptionDiscovery(t, srv, fresh); got != http.StatusOK {
			t.Fatalf("%s answers %d to a bearer signed with the new key; want 200", name, got)
		}
	}
}

// A reload outside the refresh tick (a verification miss, an out-of-band wake) drops the
// signing key while an older key, past its signing life, stays cached. The next token
// request is inside that reload's throttle window and has nothing to grace-sign with;
// the reload succeeded, so it adopts or creates a key at once rather than answering 503
// until the window closes.
func TestIngressKeyPg_DeletedSigningKeyInsideTheThrottleWindowSignsWithANewKey(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	s := NewIngressKeyStore(pool, "holder", now)
	old, _, err := s.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	advance(ingressKeyRotation)
	revoked, _, err := s.SigningKey(now()) // rotation: the old key stays live, no longer signable
	if err != nil || revoked == old {
		t.Fatalf("rotation = %q, %v; want a new key beside %s", revoked, err, old)
	}
	deleteIngressKey(t, pool, revoked)
	advance(reloadThrottle)
	if err := s.reload(now()); err != nil { // the miss's reload: it stamps the throttle
		t.Fatal(err)
	}
	if _, ok := s.keys[old]; !ok {
		t.Fatalf("the older live key %s left the cache; the row is still there", old)
	}
	kid, _, err := s.SigningKey(now())
	if err != nil {
		t.Fatalf("SigningKey inside the throttle window after the signing key was deleted: %v; want a new key", err)
	}
	if kid == revoked || kid == old {
		t.Fatalf("SigningKey = %s; want neither the deleted %s nor the unsignable %s", kid, revoked, old)
	}
}

// beginCountingDownDB is downDB that also counts creator transactions started.
type beginCountingDownDB struct {
	downDB
	begins atomic.Int32
}

func (d *beginCountingDownDB) Begin(ctx context.Context) (pgx.Tx, error) {
	d.begins.Add(1)
	return d.downDB.Begin(ctx)
}

// The rejection row for the throttled branch: with the store unreachable, a token request
// inside the throttle window of that failed reload, past the grace ceiling, answers 503
// and starts no creator transaction. Only a reload that succeeded lets it adopt or create.
func TestIngressKeyPg_ThrottledSigningPastTheSlackWithTheStoreDownStartsNoTransaction(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	db := &beginCountingDownDB{downDB: downDB{keyDB: pool}}
	s := NewIngressKeyStore(db, "holder", now)
	if _, _, err := s.SigningKey(now()); err != nil {
		t.Fatal(err)
	}
	db.down.Store(true)
	advance(ingressKeyLife) // past the slack: nothing to grace-sign with
	base := db.begins.Load()
	if _, _, err := s.SigningKey(now()); err == nil { // runs the reload, which fails
		t.Fatal("SigningKey past the slack with the store down succeeded")
	}
	if _, _, err := s.SigningKey(now()); err == nil { // inside that reload's throttle window
		t.Fatal("throttled SigningKey past the slack with the store down succeeded")
	}
	if n := db.begins.Load() - base; n != 0 {
		t.Fatalf("with the store down, %d creator transactions were started; want none", n)
	}
}
