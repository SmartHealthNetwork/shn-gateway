package engine

import (
	"crypto/ecdsa"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var kidShape = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestNewKID_ShapeAndUniqueness(t *testing.T) {
	a, err := newKID()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := newKID()
	if !kidShape.MatchString(a) || !kidShape.MatchString(b) {
		t.Fatalf("kid shape: %q %q", a, b)
	}
	if a == b {
		t.Fatal("two kids collided")
	}
}

func TestValidKID_Rejections(t *testing.T) {
	ok, _ := newKID()
	if !validKID(ok) {
		t.Fatalf("fresh kid rejected: %q", ok)
	}
	for _, bad := range []string{"", "ephemeral", ok[:31], ok + "0", "ABCDEF0123456789ABCDEF0123456789", "zz" + ok[2:]} {
		if validKID(bad) {
			t.Errorf("validKID(%q) = true", bad)
		}
	}
}

func TestEphemeralKeyStore_OwnKidVerifies(t *testing.T) {
	s, err := newEphemeralKeyStore()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	kid, key, err := s.SigningKey(now)
	if err != nil || key == nil || !validKID(kid) {
		t.Fatalf("SigningKey: kid=%q key=%v err=%v", kid, key != nil, err)
	}
	kid2, key2, _ := s.SigningKey(now.Add(48 * time.Hour))
	if kid2 != kid || key2 != key {
		t.Fatal("ephemeral store must never rotate")
	}
	pub, ok, err := s.VerificationKey(kid, now)
	if !ok || err != nil || !pub.Equal(&key.PublicKey) {
		t.Fatalf("own kid must verify with own public key (ok=%v err=%v)", ok, err)
	}
	// The in-process store can always tell: an unknown kid is (nil, false, nil), never
	// an outage — nothing here can fail to answer.
	other, _ := newKID()
	if _, ok, err := s.VerificationKey(other, now); ok || err != nil {
		t.Fatalf("unknown kid = ok:%v err:%v; want false,nil", ok, err)
	}
	var _ IngressKeyStore = s
	var _ *ecdsa.PublicKey = pub
}

// memCheck runs one CheckAndRecord on the in-memory store, which can always tell:
// a non-nil err on any row below (bar the unknown-scope row, which asserts it) is a
// failure, not an outcome.
func memCheck(t *testing.T, s ReplayStore, scope, clientID, key string, now, expiresAt time.Time) bool {
	t.Helper()
	replay, err := s.CheckAndRecord(scope, clientID, key, now, expiresAt)
	if err != nil {
		t.Fatalf("CheckAndRecord(%s, %q, %q): unexpected store error %v", scope, clientID, key, err)
	}
	return replay
}

func TestMemReplayStore_FirstUseThenReplay(t *testing.T) {
	s := NewInMemoryReplayStore()
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	exp := now.Add(5 * time.Minute)
	if memCheck(t, s, ReplayScopeIngressJTI, "client-a", "j1", now, exp) {
		t.Fatal("first use reported as replay")
	}
	if !memCheck(t, s, ReplayScopeIngressJTI, "client-a", "j1", now.Add(time.Second), exp) {
		t.Fatal("second use not reported as replay")
	}
}

func TestMemReplayStore_ScopesAndClientsAreStructural(t *testing.T) {
	s := NewInMemoryReplayStore()
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	exp := now.Add(5 * time.Minute)
	// same key, different scope → two first uses
	if memCheck(t, s, ReplayScopeHubJTI, "", "k", now, exp) || memCheck(t, s, ReplayScopePatientAccess, "", "k", now, exp) {
		t.Fatal("scopes must not share records")
	}
	// same jti, two clients → two first uses (RFC 7523 §3)
	if memCheck(t, s, ReplayScopeIngressJTI, "client-a", "j", now, exp) || memCheck(t, s, ReplayScopeIngressJTI, "client-b", "j", now, exp) {
		t.Fatal("clients must not share jti records")
	}
	// boundary shuffle: ("ab","c") vs ("a","bc") are distinct identities
	if memCheck(t, s, ReplayScopeIngressJTI, "ab", "c", now, exp) || memCheck(t, s, ReplayScopeIngressJTI, "a", "bc", now, exp) {
		t.Fatal("clientID/key boundary confused")
	}
}

func TestMemReplayStore_ExpiredKeyIsReArmed(t *testing.T) {
	s := NewInMemoryReplayStore()
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	memCheck(t, s, ReplayScopeIngressJTI, "c", "j", now, now.Add(ingressJTIWindow))
	if !memCheck(t, s, ReplayScopeIngressJTI, "c", "j", now.Add(ingressJTIWindow), now.Add(2*ingressJTIWindow)) {
		t.Fatal("at exactly the recorded expiresAt the key is still spent (the table re-arms on expires_at < now, strict)")
	}
	if memCheck(t, s, ReplayScopeIngressJTI, "c", "j", now.Add(ingressJTIWindow+time.Nanosecond), now.Add(2*ingressJTIWindow)) {
		t.Fatal("expired key must be a first use again")
	}
}

// The mirror evicts ONLY expired records, so a flood of distinct unexpired keys grows
// the set; memReplayMaxEntries is the safety ceiling that keeps it finite. At the ceiling
// the store cannot record, and a store that cannot record cannot tell a first use from a
// replay — so it answers the documented "could not tell" pair: replay=true AND an error
// naming the scope, which the caller turns into a retryable 503 rather than an accusation.
//
// The row drives a store built at a SMALL ceiling. The shipped one is 1<<20 per scope, and
// materialising a million records per scope under -race would cost hundreds of megabytes
// to exercise a branch that is identical at any size; the shipped value is pinned by the
// row below instead.
func TestMemReplayStore_CeilingFailsClosedAndNamesTheScope(t *testing.T) {
	const max = 4
	s := newMemReplayStore(max)
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	exp := now.Add(time.Hour)
	for i := 0; i < max; i++ {
		if memCheck(t, s, ReplayScopeIngressJTI, "c", fmt.Sprintf("j-%d", i), now, exp) {
			t.Fatalf("first use of key %d reported as replay below the ceiling", i)
		}
	}
	replay, err := s.CheckAndRecord(ReplayScopeIngressJTI, "c", "one-too-many", now, exp)
	if !replay {
		t.Fatal("a store that cannot record must fail closed (replay=true)")
	}
	if err == nil || !strings.Contains(err.Error(), ReplayScopeIngressJTI) {
		t.Fatalf("the ceiling must report an error naming the scope, got %v", err)
	}
	// A key already recorded is still answered at the ceiling: the refusal is about
	// RECORDING, and refusing to answer a plain replay would be a second, invented failure.
	if replay, err := s.CheckAndRecord(ReplayScopeIngressJTI, "c", "j-0", now, exp); !replay || err != nil {
		t.Fatalf("an already-recorded key at the ceiling = replay:%v err:%v; want true,nil", replay, err)
	}
	// The ceiling is per SCOPE: a full ingress-jti set does not refuse a hub-jti record.
	if memCheck(t, s, ReplayScopeHubJTI, "", "h-0", now, exp) {
		t.Fatal("scopes share a ceiling")
	}
	// …and EXPIRY is what makes room again: past every record's expiresAt a fresh key
	// records, without the purge throttle standing in the way.
	past := exp.Add(time.Microsecond)
	if memCheck(t, s, ReplayScopeIngressJTI, "c", "after-expiry", past, past.Add(time.Hour)) {
		t.Fatal("a fresh key past every record's expiresAt must be a first use, not a ceiling refusal")
	}
}

// The shipped mirror carries the safety ceiling, at every scope the engine emits. A
// constructor that quietly built a small one would make the ceiling row above a test of
// nothing the product runs.
func TestMemReplayStore_ShippedCeilingAndScopes(t *testing.T) {
	s, ok := NewInMemoryReplayStore().(*memReplayStore)
	if !ok {
		t.Fatalf("NewInMemoryReplayStore returned %T, not the in-memory mirror", NewInMemoryReplayStore())
	}
	for _, scope := range []string{ReplayScopeIngressJTI, ReplayScopeHubJTI, ReplayScopePatientAccess} {
		sc, ok := s.scopes[scope]
		if !ok {
			t.Fatalf("scope %q has no record set", scope)
		}
		if sc.max != memReplayMaxEntries {
			t.Fatalf("scope %q ceiling = %d, want memReplayMaxEntries (%d)", scope, sc.max, memReplayMaxEntries)
		}
	}
	if len(s.scopes) != 3 {
		t.Fatalf("record sets = %d, want exactly the 3 engine scopes", len(s.scopes))
	}
}

// The sweep is hygiene, never load-bearing: it is throttled to once a minute, and the
// expiry rule is applied on every lookup whether or not a sweep has run. This row pins
// both halves — an expired record is a first use again INSIDE the throttle window (so the
// sweep is not what re-arms it), and the sweep really does reclaim the entry once the
// window passes (so the set does not grow for ever).
func TestMemReplayStore_PurgeThrottledAndNeverLoadBearing(t *testing.T) {
	s := newMemReplayStore(memReplayMaxEntries)
	sc := s.scopes[ReplayScopeIngressJTI]
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	memCheck(t, s, ReplayScopeIngressJTI, "c", "j", now, now.Add(time.Second))
	// Two seconds later the record is expired but the sweep is still throttled…
	if memCheck(t, s, ReplayScopeIngressJTI, "c", "j", now.Add(2*time.Second), now.Add(time.Minute)) {
		t.Fatal("an expired record must be a first use again without waiting for a sweep")
	}
	if sc.lastPurge != now {
		t.Fatalf("a sweep ran inside the throttle window (lastPurge=%v, want %v)", sc.lastPurge, now)
	}
	// …and past the throttle the sweep reclaims what has expired.
	memCheck(t, s, ReplayScopeIngressJTI, "c", "other", now.Add(2*memReplayPurgeInterval), now.Add(3*memReplayPurgeInterval))
	if _, still := sc.seen["1:cj"]; still {
		t.Fatal("the sweep did not reclaim the expired record")
	}
	if len(sc.seen) != 1 {
		t.Fatalf("records after the sweep = %d, want 1 (only the unexpired one)", len(sc.seen))
	}
}

func TestMemReplayStore_UnknownScopeFailsClosed(t *testing.T) {
	s := NewInMemoryReplayStore()
	now := time.Now()
	// A scope this store holds no guard for is a PROGRAMMING error, not a replay:
	// replay=true (fail closed) AND an error naming the scope, so the caller answers
	// "the record is unavailable" rather than "you replayed that". The pg oracle
	// answers the same shape (connectors/pgstore replay parity table).
	replay, err := s.CheckAndRecord("not-a-scope", "", "k", now, now.Add(time.Minute))
	if !replay {
		t.Fatal("unknown scope must report replay (fail closed)")
	}
	if err == nil || !strings.Contains(err.Error(), "not-a-scope") {
		t.Fatalf("unknown scope must report an error naming the scope, got %v", err)
	}
}

func TestMemReplayStore_RacersExactlyOneFirstUse(t *testing.T) {
	s := NewInMemoryReplayStore()
	now := time.Now()
	var wg sync.WaitGroup
	var mu sync.Mutex
	first := 0
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A store error counts as a replay here (fail closed), never a first use.
			replay, err := s.CheckAndRecord(ReplayScopeIngressJTI, "c", "race", now, now.Add(time.Minute))
			if !replay && err == nil {
				mu.Lock()
				first++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if first != 1 {
		t.Fatalf("first uses = %d, want 1", first)
	}
}

// stubReplayStore is the ReplayStore seam stand-in for the caller-side rows: it answers
// a fixed (replay, err) pair. (false, err) is the adversarial shape the interface's
// fail-closed rule exists for — a third-party store that could not tell but did not
// claim a replay; every caller must reject on err whatever the bool says. Safe without
// a lock: no test in gateway/engine calls t.Parallel().
type stubReplayStore struct {
	replay bool
	err    error
	calls  int
}

func (s *stubReplayStore) CheckAndRecord(string, string, string, time.Time, time.Time) (bool, error) {
	s.calls++
	return s.replay, s.err
}

var _ ReplayStore = (*stubReplayStore)(nil)
