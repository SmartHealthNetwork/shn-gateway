package pgstore

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
)

var _ engine.IngressKeyStore = (*IngressKeyStore)(nil)

const (
	ingressKeyRotation = 24 * time.Hour   // a new signing key this often
	ingressKeyRefresh  = 30 * time.Second // background reload cadence (RunRefresh)
	reloadThrottle     = time.Second      // at most one miss-driven reload per second
	negCacheTTL        = time.Minute
	negCacheMax        = 1024
)

type cachedKey struct {
	key      *ecdsa.PrivateKey
	created  time.Time
	notAfter time.Time
}

// keyDB is the narrow pool surface the key store uses. *pgxpool.Pool satisfies it; tests
// wrap it to count round-trips or park them.
type keyDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Begin(ctx context.Context) (pgx.Tx, error)
}

var (
	errReloadThrottled = errors.New("ingress key reload throttled")
	// errReloadInFlight is the no-wait reload declining because another goroutine already
	// holds the round trip. It is not a store failure and never reaches a caller: attempt
	// treats it exactly as it treats the throttle.
	errReloadInFlight = errors.New("ingress key reload already in flight")
	// errColdReloadWait is a COLD replica's bounded wait expiring: no reload that started
	// after the request arrived completed within the store timeout. It is a store failure
	// by construction — the cache has never been loaded, so the replica genuinely cannot
	// tell an unknown kid from an unloaded one — and it reaches the caller, which answers
	// a retryable 503 rather than a 401 against a live bearer.
	errColdReloadWait = errors.New("ingress key store cold: no reload completed within the store timeout")
)

// IngressKeyStore is the Postgres-backed signing/verification key store shared by
// every replica of a holder. Signing: the newest live row (created within
// ingressKeyRotation) is adopted; when none exists a P-384 key is generated and
// inserted under a holder-scoped transaction lock. Concurrent creators adopt the
// same committed readable winner before their operations succeed.
// Verification: a per-process positive cache kid → key; a kid still unknown after a
// reload this caller RAN is negatively cached for a minute. An errored miss is rejected
// but never cached, and it is rejected WITH the error, so the caller answers a 503
// ("could not tell") rather than a 401 that reads as a bad credential. RunRefresh reloads
// every 30 s regardless of traffic and counts its OWN failures through the loop-only hook
// it is passed (refreshTick): nothing else can report them, because no request is waiting
// on a background reload.
//
// # Two states: cold until the first successful load, warm from then on
//
// A replica is COLD from its start until the first reload that COMPLETES SUCCESSFULLY,
// and WARM for the rest of the process's life. A successful reload that found no rows
// at all warms it: "I read the table and it was empty" is a snapshot, and a holder with
// no key yet pays the same THROTTLE as everyone else from then on. What keeps a replica
// cold is only a database it cannot read — an outage that outlasts start-up keeps it cold
// for as long as the outage lasts. A warm replica whose cache holds no key is a special
// case of warm, not of cold: it never parks, but it does not answer from the empty
// snapshot either — when the throttle allows it runs the one synchronous no-wait reload
// (attempt) and resolves a sibling's first key on the same request, at the cost of one
// query; inside an armed throttle window it answers at once like any warm miss.
//
// # The WARM miss path never waits
//
// A verification miss on a warm replica answers from the freshest snapshot this replica
// has and returns — it does not sleep out a throttle window, does not wait on another
// goroutine's in-flight reload, and never queues behind reloadMu. validKID admits any
// 32-hex string and jwt/v5 runs the keyfunc BEFORE it validates the signature, so an
// unauthenticated caller can present unsigned tokens carrying fresh random kids as fast
// as it likes; every request that could park a goroutine on the way to its 401 is a
// goroutine an anonymous caller gets to hold. So none of them park.
//
// The reload a miss would have waited for is instead kicked OUT OF BAND through a
// buffered wake channel of size 1:
//
//   - While RunRefresh is running (every deployment with a DSN — app.Run starts it), its
//     goroutine drains the wake channel: at most one extra reload per reloadThrottle,
//     never concurrent with itself, and it stops with RunRefresh's ctx. A burst of a
//     million unknown kids costs one reload per second and not one parked goroutine.
//   - When RunRefresh was never started (hermetic rows, and any embedder that does not
//     run the loop) nothing drains the channel, so the miss path makes at most ONE
//     non-blocking throttled reload attempt of its own — reloadIfDueNoWait: it declines
//     the moment a reload is in flight or reloadMu is held, and never waits on either.
//
// After concurrent initial creation/adoption completes, each creator has the same
// signing key cached and accepts its bearers immediately. A lagging warm replica
// can still refuse a sibling's later rotation until a successful reload learns it;
// the one-second throttle is a query budget, not an acceptance deadline. Writers
// from older versions do not participate in the shared creator decision.
//
// # The COLD miss path waits for a reload younger than the request
//
// A cold replica has no snapshot: its cache is not evidence of anything, so a 401 from it
// would refuse LIVE bearers on every rolling deploy (RunRefresh starts loading before the
// listener comes up, but concurrently — a request can arrive while that first query is
// still running). A cold miss therefore never answers "unknown" from the unloaded cache.
// It answers from a reload that STARTED AFTER THE REQUEST ARRIVED — the only snapshot that
// can be trusted to carry a key minted before the request — and waits for one, bounded.
//
// The mechanism is a pair of generation counters under mu (reloadStarted, incremented as
// a reload is about to issue its SELECT; reloadDone, incremented as it completes, success
// or failure) and a broadcast channel (reloadEpoch) that every completion closes and
// replaces. Reloads are serialized by reloadMu, so reloadDone completes in order. A cold
// miss records arrival := reloadStarted and loops, bounded by ONE deadline of storeTimeout
// from entry (resolveCold):
//
//   - a cache HIT answers at once (a cached key is always real evidence);
//   - reloadDone > arrival — a reload younger than the request has completed — answers
//     from that snapshot: a miss is a true unknown, (nil, false, nil), or, when that
//     reload failed, "could not tell" with its error. Nothing is negatively cached here;
//     the negative cache stays a warm-path device;
//   - otherwise the caller tries one reload of its own under exactly the WARM path's rules
//     (reloadIfDueNoWait: the throttle, then reloadMu's TryLock; never when RunRefresh is
//     draining the wake channel, so the shipped wiring has ONE reload stream), and when
//     that declines it kicks the wake channel and PARKS — on the epoch channel (any reload
//     completing wakes it), on a timer for the throttle slot when no reload is in flight,
//     or on the deadline, whichever comes first — and goes round again;
//   - at the deadline it answers errColdReloadWait, which the caller reports as a
//     retryable 503. Never a 401.
//
// What this costs, stated plainly because it is the one place a request on the
// unauthenticated path waits: in BOTH states the MISS PATH puts at most one reload query
// per reloadThrottle per replica on the database, at most one in flight, and a failed
// reload is logged once (by the goroutine that ran it) — the 30 s refresh tick and the
// signing-key mint path (SigningKey's slow path, rotateIfDue) add their own loads on top,
// unthrottled and traffic-independent. What the cold state adds is one PARKED goroutine
// per concurrent miss, for at most one store timeout each, the query it may run itself
// included — parked on a channel, never on the database. It lasts until the first
// successful load, which is normally milliseconds; a
// database outage that outlasts start-up keeps the replica cold, and while it does each
// miss parks up to one store timeout while the single reload stream retries once per
// second and logs once per failure, then answers 503. Size client timeouts for that.
//
// A miss ends in exactly one of three outcomes:
//
//   - RESOLVED — a live cached kid, or one a reload brought in while the caller waited.
//   - UNKNOWN — the snapshot the caller may answer from does not carry the kid: rejected.
//     Negatively cached for negCacheTTL only on the warm path and only when the reload
//     that failed to find it was THIS caller's; a snapshot taken before a sibling's
//     INSERT is not evidence the kid does not exist.
//   - COULD NOT TELL — the reload the caller answers from failed (or the cold deadline
//     passed with no reload younger than the request completed): rejected WITH the error,
//     so the caller answers 503 rather than a 401.
//
// Locking: the cache lock (mu) is never held across a DB round trip, so a cached kid
// verifies immediately while a reload is in flight or the DB is hung; reloads are
// serialized by reloadMu, and lastReload is stamped BOTH when an attempt starts and when
// it completes (errors included), so a failing or hung DB costs at most one ≤storeTimeout
// query per second per replica. Lock order: signMu → reloadMu → mu.
type IngressKeyStore struct {
	pool     keyDB
	holderID string
	now      func() time.Time

	mu         sync.RWMutex         // guards the fields down to reloadEpoch; never held across a DB call
	keys       map[string]cachedKey // kid → key, live rows
	signing    string               // kid currently used to sign ("" until loaded)
	negCache   map[string]time.Time // kid → when it was found absent
	lastReload time.Time            // last reload attempt: stamped at its start AND at its completion (errors count)
	// lastReloadErr is the outcome of the reload lastReload was stamped by (nil when it
	// succeeded). A miss the throttle keeps from running its own reload answers from it:
	// inside a window stamped by a FAILED reload the honest answer is "the store could
	// not tell" (503), not "unknown kid" (401).
	lastReloadErr error
	// inflight is non-nil exactly while a reload is in flight; its done channel is closed
	// when that reload finishes and its err carries the outcome the reloader saw, so a
	// waiter takes the same branch the reloader took. SigningKey's slow path waits on it;
	// the miss path reads it only to decide (lookup, reloadIfDueNoWait). Never held while
	// waiting.
	inflight *reloadLatch
	// loaded is set when the first successful reload installs its snapshot (reloadQuery)
	// and never cleared: the cold/warm line (see the type doc). Read once per request,
	// in the same mu section as the lookup (classify): a request that read it before the
	// install is on the cold path and answers by generation (a hit, or a reload younger
	// than itself); one that reads it after is warm and answers from that snapshot. A
	// warm request never refuses from the read it classified by, though — every warm
	// refusal derived from the snapshot takes a last look under mu (missAnswer), so a key
	// the install put in the cache after the classification is answered, not refused.
	// (The two negative-cache refusals need no such look: a kid a completed reload
	// negatively cached was absent from a snapshot younger than any install.)
	loaded bool
	// reloadStarted counts reloads that have begun (stamped in the same mu section that
	// publishes the latch, just before the SELECT is issued); reloadDone counts reloads
	// that have completed, success or failure (stamped in the same section that records
	// the outcome). Serialized by reloadMu, so reloadDone advances in start order. A cold
	// miss trusts a snapshot only when reloadDone has passed the reloadStarted it arrived
	// at: that reload's SELECT ran after the request did.
	reloadStarted, reloadDone uint64
	// reloadEpoch is closed and replaced by every reload completion, under mu, in the same
	// section that advances reloadDone. A cold miss reads it together with the counters
	// and parks on it, so a completion that lands between its read and its park still
	// wakes it (the channel it holds is the one that was closed).
	reloadEpoch chan struct{}

	// negRing is the negative cache's insertion order: a fixed ring of negCacheMax slots
	// whose next write evicts whatever that slot last held. Eviction is therefore O(1) by
	// construction — no scan for an oldest entry — and the map can never exceed the ring,
	// because every insertion consumes exactly one slot. A kid the TTL prune already
	// dropped leaves a stale slot behind; evicting it later is a delete of an absent key.
	negRing []string
	negNext int

	reloadMu sync.Mutex // serializes reloads (the DB round trip)
	signMu   sync.Mutex // serializes the SigningKey slow path (reload → adopt → insert)

	// wake carries a miss's request for an out-of-band reload to RunRefresh's goroutine.
	// Buffered at 1 and sent to without blocking: a flood collapses into one pending
	// wake, and a miss never waits for the reload it asked for.
	wake chan struct{}
	// refreshing reports whether RunRefresh's loop is draining wake. It is the ONLY thing
	// the miss path branches on: with the loop running the miss never touches the
	// database itself; without it the miss makes one non-blocking throttled attempt.
	refreshing atomic.Bool

	// coldWaitBound is the ceiling on a COLD miss's whole resolution; zero — the shipped
	// value — means storeTimeout. onColdWait, likewise nil in every shipped wiring, fires
	// once per wait iteration of resolveCold, just before the caller parks. Both are set
	// only by tests, and only before the store serves requests: the bound lets a row prove
	// the wait is bounded without spending two seconds, and the hook lets a row assert
	// STRUCTURALLY that a caller parked (or that a warm caller never did), rather than on
	// a wall clock that flakes under -race.
	coldWaitBound time.Duration
	onColdWait    func()
}

// reloadLatch is one in-flight reload's completion signal AND its result: err is
// written before done is closed, so every waiter reads exactly the outcome the reloader
// saw. A waiter that read only "finished" would take the healthy-database branch during
// an outage — SigningKey's waiter would skip its grace-sign and try to INSERT against
// the dead database, turning a bridged blip into a 503.
type reloadLatch struct {
	done chan struct{}
	err  error // valid only after done is closed
}

func NewIngressKeyStore(pool keyDB, holderID string, clock func() time.Time) *IngressKeyStore {
	if clock == nil {
		clock = time.Now
	}
	return &IngressKeyStore{
		pool: pool, holderID: holderID, now: clock,
		keys: map[string]cachedKey{}, negCache: map[string]time.Time{},
		negRing:     make([]string, negCacheMax),
		wake:        make(chan struct{}, 1),
		reloadEpoch: make(chan struct{}),
	}
}

// SigningKey returns the key to sign with at now, rotating when the current key
// is older than ingressKeyRotation. Errors surface (the caller answers 503) — except
// when the cache already holds a key inside its signing life (this process's own or a
// sibling's, learned by a VerificationKey miss reload), which is adopted, or inside
// the grace described below.
func (s *IngressKeyStore) SigningKey(now time.Time) (string, *ecdsa.PrivateKey, error) {
	if kid, key, ok := s.currentSigning(now); ok {
		return kid, key, nil
	}
	s.signMu.Lock()
	defer s.signMu.Unlock()
	if kid, key, ok := s.currentSigning(now); ok { // another caller finished the slow path
		return kid, key, nil
	}
	// Throttled like every miss: with the DB down inside the grace window, rotation is
	// due on EVERY call, and an unthrottled reload here would queue each /oauth/token
	// request behind its own storeTimeout under signMu. On a healthy DB the first call
	// after rotation inserts the new key before releasing signMu, so the next call is
	// the fast path again; a throttled call after a reload that found no live key signs
	// with the current key inside its slack. Rotation itself does not depend on this
	// path: refreshOnce rotates every ingressKeyRefresh (see rotateIfDue), so under a
	// flood that keeps the throttle armed the grace is a bridge of at most that interval.
	// A cache holding NO key at all is the cold-replica case: there is nothing to
	// grace-sign with and nothing a throttled call could adopt, so sharing the
	// miss throttle here lets unauthenticated bearers with random kids (which stamp
	// it) hold a fresh replica's /oauth/token at 503 until the first refresh tick.
	// Reload unthrottled in that one state; every later call takes the throttled
	// path above. signMu still admits one such reload at a time.
	var err error
	if s.cacheEmpty() {
		err = s.reload(now)
	} else {
		_, err = s.reloadIfDue(now)
	}
	throttled := errors.Is(err, errReloadThrottled)
	// Adopt any key the cache already holds that is inside its signing life FIRST —
	// before grace-signing and before surfacing a reload error. That key may be a
	// SIBLING's, learned by a VerificationKey miss reload while the database was still
	// reachable; it is eligible for signing, so a replica holding one
	// has no reason to answer 503 just because its own reload has started failing.
	s.mu.Lock()
	if kid := s.newestLiveLocked(now); kid != "" {
		s.signing = kid
		key := s.keys[kid].key
		s.mu.Unlock()
		return kid, key, nil
	}
	s.mu.Unlock()
	if err != nil && !throttled {
		if kid, key, ok := s.graceSign(now); ok {
			log.Printf("pgstore: ingress key rotation deferred (reload: %v); signing with %s inside its slack", err, kid)
			return kid, key, nil
		}
		return "", nil, err
	}
	if throttled {
		if kid, key, ok := s.graceSign(now); ok {
			return kid, key, nil
		}
		return "", nil, fmt.Errorf("ingress key rotation due and the current key's slack is exhausted (refresh has not rotated it): %w", err)
	}
	kid, c, err := s.selectOrCreateKey(now) // DB round trip: no cache lock held
	if err != nil {
		return "", nil, err
	}
	s.mu.Lock()
	s.keys[kid] = c
	s.signing = kid
	s.mu.Unlock()
	return kid, c.key, nil
}

// graceSign is the current signing key while a bearer minted now with it stays
// inside its verification lifetime: now+IngressBearerTTL before not_after (the 1-minute
// slack past rotation+TTL). Used when rotation is due but the store cannot be reached
// (or was reached within the throttle), so a blip does not become a 503; past the slack
// the store error surfaces.
func (s *IngressKeyStore) graceSign(now time.Time) (string, *ecdsa.PrivateKey, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	kid := s.signing
	c, ok := s.keys[kid]
	if !ok || !now.Add(engine.IngressBearerTTL).Before(c.notAfter) {
		return "", nil, false
	}
	return kid, c.key, true
}

// currentSigning is the fast path: the cached signing key while it is inside its
// rotation interval.
func (s *IngressKeyStore) currentSigning(now time.Time) (string, *ecdsa.PrivateKey, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.keys[s.signing]
	if !ok || !now.Before(c.created.Add(ingressKeyRotation)) {
		return "", nil, false
	}
	return s.signing, c.key, true
}

// VerificationKey resolves kid at now: (key, true, nil) for a kid inside its
// verification life, (nil, false, nil) for one the snapshot this replica may answer from
// does not have, and (nil, false, err) when the store could not TELL — the reload that
// snapshot came from failed, or (cold only) no reload younger than the request completed
// within the store timeout. The third shape is what lets the caller answer a retryable
// 503 instead of a 401 that accuses an honest client of a bad credential.
//
// Four branches, in order: a cache hit; a negatively cached kid (warm-path evidence, from a
// reload the refusing caller itself ran); a COLD replica — no reload has ever completed
// successfully — which resolves the miss through resolveCold and never answers "unknown"
// from its unloaded cache; and the WARM replica, which never waits: not on a throttle
// window, not on another goroutine's reload, not on reloadMu. See the type doc for both
// states and what each costs.
func (s *IngressKeyStore) VerificationKey(kid string, now time.Time) (*ecdsa.PublicKey, bool, error) {
	pub, ok, action, cold, empty := s.classify(kid, now)
	if ok {
		return pub, true, nil
	}
	if action == missReject {
		return nil, false, nil // negatively cached by a reload that ran: rejected, NOT a store failure
	}
	if cold {
		return s.resolveCold(kid, now)
	}
	// A WARM replica whose cache holds NO key at all — a successful read of an empty
	// table before any creator commits — has no snapshot worth answering from, so it
	// does not defer to the out-of-band kick: it runs the one synchronous, throttled, TryLock-guarded attempt
	// below instead (never a wait, never a park, one query in flight, the throttle
	// honoured) and verifies a sibling's first key on the first call. With any key
	// cached, the snapshot in hand is the answer and the reload is kicked out of band.
	if !empty && (action == missOutOfBand || s.refreshing.Load()) {
		// Either a reload is already in flight / the throttle is armed, or the refresh
		// loop is the one that reloads here. Ask for a reload and answer NOW from the
		// snapshot in hand: the kid verifies on the next request once that reload lands.
		s.kickReload()
		return s.missAnswer(kid, now)
	}
	pub, ok, ran, err := s.attempt(kid, now)
	if err != nil {
		return nil, false, err
	}
	if ok {
		return pub, true, nil
	}
	if !ran {
		// A reload started under this caller's feet, so the snapshot that answered is not
		// its own and may PREDATE the row for this kid. Reject without caching — the
		// negative cache is only ever seeded by a reload this caller drove — and if that
		// snapshot's own reload FAILED, say the store could not tell. Ask for one more
		// reload on the way out, so a later caller can read a fresher snapshot.
		s.kickReload()
		return s.missAnswer(kid, now)
	}
	s.mu.Lock()
	s.negCacheAddLocked(kid, now)
	s.mu.Unlock()
	return nil, false, nil
}

// classify is VerificationKey's ONE read of the cache before it chooses a path: the kid's
// presence (a live cached key), the action a miss owes (lookupLocked), and the two states
// the miss path branches on — cold (no reload has ever installed a snapshot) and empty (a
// snapshot with no key in it). All from one hold of mu, so the classification and the
// evidence it acts on are the same instant: read across two sections, a reload's install
// could land between them and a caller would refuse, from the first read, a kid the second
// read already holds.
func (s *IngressKeyStore) classify(kid string, now time.Time) (pub *ecdsa.PublicKey, ok bool, action missAction, cold, empty bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	pub, ok, action = s.lookupLocked(kid, now)
	return pub, ok, action, !s.loaded, len(s.keys) == 0
}

// missAnswer is the warm path's LAST look before it refuses, taken under mu together with
// the outcome it would refuse with: a reload that installed the kid since this request was
// classified answers with the key; otherwise the freshest snapshot's outcome — nil for
// "unknown", the last completed reload's error for "could not tell" (503). It is what
// keeps a request that straddles an install from answering 401 for a kid in its own cache.
func (s *IngressKeyStore) missAnswer(kid string, now time.Time) (*ecdsa.PublicKey, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if c, ok := s.keys[kid]; ok && now.Before(c.notAfter) {
		return &c.key.PublicKey, true, nil
	}
	return nil, false, s.lastReloadErr
}

// resolveCold is the COLD replica's miss: no reload has ever completed successfully, so
// the cache is not evidence and a 401 from it would refuse live bearers on every rolling
// deploy. The caller answers only from a reload that STARTED after it arrived
// (reloadDone > arrival — see the type doc for why that is the only trustworthy snapshot),
// or from a cache hit, which is real evidence whenever it appears. It is bounded by ONE
// deadline, coldWaitBound (storeTimeout by default) from entry, across every wait it
// takes and the one query it may run itself (that query's context gets what is left of
// the bound), and at that deadline it answers errColdReloadWait: a 503 the client
// retries, never the 401 that would accuse a healthy sibling's live bearer of being forged.
//
// Each pass: a hit answers; a younger completed reload answers (its miss is a true
// unknown, not negatively cached; its failure is "could not tell"); past the deadline the
// answer is the bound's. Otherwise the caller tries ONE reload of its own under exactly
// the warm path's rules — reloadIfDueNoWait, throttle then TryLock, and not at all while
// RunRefresh is draining the wake channel, so the shipped wiring has one reload stream —
// and, when that declines, kicks the wake channel and parks: on the epoch channel it read
// together with the counters (so a completion between the read and the park still wakes
// it), on a timer for the throttle slot when no reload is in flight (the slot is computed
// from the injected clock; the timer itself is real time), or on the deadline. A caller
// therefore never spins: every pass that resolves nothing either ran a reload or parked.
//
// The deadline is wall time, not the injected clock: it bounds a race against other
// goroutines, and a row with a frozen clock must still terminate. The injected clock stays
// the authority for everything the store DECIDES (key liveness, the throttle).
func (s *IngressKeyStore) resolveCold(kid string, now time.Time) (*ecdsa.PublicKey, bool, error) {
	bound := s.coldWaitBound
	if bound <= 0 {
		bound = storeTimeout
	}
	deadline := time.Now().Add(bound)
	expiry := time.NewTimer(bound)
	defer expiry.Stop()
	s.mu.RLock()
	arrival := s.reloadStarted
	s.mu.RUnlock()
	for {
		snap := s.coldSnapshot(kid, now)
		if snap.pub != nil {
			return snap.pub, true, nil
		}
		if snap.done > arrival {
			return nil, false, snap.lastErr
		}
		epoch, inflight := snap.epoch, snap.inflight
		remaining := time.Until(deadline)
		if remaining <= 0 {
			s.kickReload() // a later caller can find a fresher snapshot
			return nil, false, errColdReloadWait
		}
		if !s.refreshing.Load() {
			tick := s.now()
			// The caller's own round trip gets what is LEFT of its bound, never a fresh
			// storeTimeout: the deadline is one for the whole call, the query included.
			ran, err := s.reloadIfDueNoWaitWithin(tick, min(remaining, storeTimeout))
			if ran {
				if err != nil {
					// Logged once, by the goroutine that ran it; every waiter that
					// answers from this outcome reads it from lastReloadErr in silence.
					log.Printf("pgstore: ingress key reload: %v", err)
					if !time.Now().Before(deadline) {
						return nil, false, errColdReloadWait // the bound cut the query short
					}
				}
				continue // the counters advanced: the next pass answers
			}
			if errors.Is(err, errReloadInFlight) {
				inflight = true // a latch, or reloadMu held before its latch is published
			}
		}
		if s.onColdWait != nil {
			s.onColdWait()
		}
		s.kickReload()
		var slot <-chan time.Time
		var slotTimer *time.Timer
		if !inflight {
			d := s.throttleRemaining(s.now())
			if d <= 0 && !s.refreshing.Load() {
				continue // the slot opened while this pass was deciding: try again
			}
			if d > 0 {
				slotTimer = time.NewTimer(d)
				slot = slotTimer.C
			}
		}
		select {
		case <-epoch:
		case <-slot:
		case <-expiry.C:
		}
		if slotTimer != nil {
			slotTimer.Stop()
		}
	}
}

// coldPass is one cold pass's read of the store: the generation counters, the epoch to
// park on, whether a reload is in flight, the freshest outcome, and the kid's live key if
// the cache holds one.
type coldPass struct {
	done     uint64
	epoch    chan struct{}
	inflight bool
	lastErr  error
	pub      *ecdsa.PublicKey
}

// coldSnapshot is ONE cache-lock section for the generation and the key, counters first.
// A reload installs its snapshot and completes in two separate sections, so a key read
// taken before the counters could miss a key that the completion the counters then report
// has already installed — a bare 401 for a kid in the cache. Read together, the key is at
// least as young as the generation, which is the invariant resolveCold answers by; it
// lives here, in one method, so a later edit cannot quietly split it again.
func (s *IngressKeyStore) coldSnapshot(kid string, now time.Time) coldPass {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p := coldPass{done: s.reloadDone, epoch: s.reloadEpoch, inflight: s.inflight != nil, lastErr: s.lastReloadErr}
	if c, ok := s.keys[kid]; ok && now.Before(c.notAfter) {
		p.pub = &c.key.PublicKey
	}
	return p
}

// kickReload asks for one out-of-band reload without blocking. A full buffer means one is
// already pending, which is exactly what this caller wanted; a store whose RunRefresh is
// not running has no drainer, and the pending wake is consumed by the next loop that
// starts (or never, which costs one buffered token).
func (s *IngressKeyStore) kickReload() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// attempt is one non-blocking reload for kid followed by a re-read of the cache. ran
// reports whether THIS caller executed the reload: only then is the snapshot its own, and
// only then may the negative cache be seeded from it. A store error is logged and returned
// — the caller answers 503 and counts the outage, never a 401 that reports a database
// failure as a bad credential — and an errored miss is never cached.
func (s *IngressKeyStore) attempt(kid string, now time.Time) (*ecdsa.PublicKey, bool, bool, error) {
	ran, err := s.reloadIfDueNoWait(now)
	if err != nil && !errors.Is(err, errReloadThrottled) && !errors.Is(err, errReloadInFlight) {
		log.Printf("pgstore: ingress key reload: %v", err)
		return nil, false, ran, err
	}
	pub, ok, _ := s.lookup(kid, now)
	return pub, ok, ran, nil
}

// missAction is what a cache miss must do next.
type missAction int

const (
	missReload    missAction = iota // this caller may run one reload of its own, then re-check
	missReject                      // negatively cached by a completed reload: reject without a reload
	missOutOfBand                   // a reload is in flight, or the throttle is armed: kick and answer now
)

// lookup is the lock-only path: (key, true, _) on a live cached kid, else the action the
// miss owes.
func (s *IngressKeyStore) lookup(kid string, now time.Time) (*ecdsa.PublicKey, bool, missAction) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lookupLocked(kid, now)
}

// lookupLocked is lookup with mu held (classify reads it together with the miss path's
// two states).
func (s *IngressKeyStore) lookupLocked(kid string, now time.Time) (*ecdsa.PublicKey, bool, missAction) {
	if c, ok := s.keys[kid]; ok && now.Before(c.notAfter) {
		return &c.key.PublicKey, true, missReload
	}
	if seen, ok := s.negCache[kid]; ok && now.Sub(seen) < negCacheTTL {
		return nil, false, missReject
	}
	if s.inflight != nil || now.Sub(s.lastReload) < reloadThrottle {
		return nil, false, missOutOfBand
	}
	return nil, false, missReload
}

// cacheEmpty reports whether no key at all is cached. SigningKey's slow path reloads
// unthrottled in that state (there is nothing to grace-sign with), and VerificationKey's
// warm path runs its one synchronous attempt rather than deferring to the out-of-band
// kick (an empty snapshot is not an answer); it is NOT the cold predicate — see cold.
func (s *IngressKeyStore) cacheEmpty() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.keys) == 0
}

// cold reports whether no reload has ever completed successfully: the state in which the
// cache is not evidence of anything and VerificationKey resolves a miss through
// resolveCold. A successful reload that found no rows warms the replica.
func (s *IngressKeyStore) cold() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.loaded
}

// inFlight returns the latch a reload in flight will close, or nil when none is.
func (s *IngressKeyStore) inFlight() *reloadLatch {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.inflight
}

// RunRefresh reloads live rows every ingressKeyRefresh until ctx is done, and drains the
// wake channel a verification miss kicks. The constructor starts nothing; app.Run starts
// this beside the registrar poller.
//
// This goroutine is the ONLY drainer, so a wake-driven reload is never concurrent with
// itself or with a tick, and it stops with ctx — nothing is started lazily and nothing
// outlives the loop. While it runs, a request path holding any key never issues a reload
// of its own (the refreshing flag), so an unauthenticated flood of unknown kids costs at
// most one reload per reloadThrottle here and not one parked request goroutine anywhere.
// The one exception is a warm replica whose cache holds NO key: there the request path
// still runs its one throttled, TryLock-guarded reload itself (VerificationKey), holding
// that one request inside its query, so a sibling's first key is verified on the first
// call rather than after the loop's next reload.
//
// onErr (nil-safe) is the LOOP's own store-error counter — see refreshTick for why it is
// the one place this store counts anything. A wake-driven reload is one of the loop's own:
// no request is waiting on it either.
func (s *IngressKeyStore) RunRefresh(ctx context.Context, onErr func(store string)) {
	s.refreshing.Store(true)
	defer s.refreshing.Store(false)
	// Warm the cache BEFORE the first tick. A replica that starts serving with an empty
	// cache cannot resolve any kid, so every live bearer presented to it is refused until
	// something loads the table — which would make a routine restart, and therefore every
	// rolling deploy, refuse traffic. Run starts this before the listener. Rotation stays
	// on the tick: this is a read, so a fleet restarting together mints nothing.
	if err := s.reload(s.now()); err != nil {
		log.Printf("pgstore: ingress key warm-up reload: %v", err)
		if onErr != nil {
			onErr(storeNameIngressKey)
		}
	}
	t := time.NewTicker(ingressKeyRefresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.refreshTick(s.now(), onErr)
		case <-s.wake:
			s.wakeTick(ctx, onErr)
		}
	}
}

// wakeTick services one out-of-band reload request. It waits out the remainder of the
// throttle window HERE — on the background goroutine, with no caller parked behind it —
// so a kid a miss asked about is loaded within reloadThrottle rather than at the next
// 30-second tick. A wake that arrives while the window is armed therefore costs a timer,
// never a spin.
func (s *IngressKeyStore) wakeTick(ctx context.Context, onErr func(store string)) {
	if d := s.throttleRemaining(s.now()); d > 0 {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
	if err := s.reload(s.now()); err != nil {
		log.Printf("pgstore: ingress key reload (out of band): %v", err)
		if onErr != nil {
			onErr(storeNameIngressKey)
		}
	}
}

// throttleRemaining is how long until the next reload slot opens; <= 0 means now. A clock
// that stepped backwards is clamped to the window, so a wake can never park the loop for
// longer than one throttle.
func (s *IngressKeyStore) throttleRemaining(now time.Time) time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d := reloadThrottle - now.Sub(s.lastReload)
	if d > reloadThrottle {
		return reloadThrottle
	}
	return d
}

// refreshTick is one turn of the RunRefresh loop, split out so the body is testable while
// the ticker itself stays untested.
//
// The background reload is the ONE key-store failure nothing else can report: no request
// is waiting on it, so there is no error to return, and a replica with a warm cache and no
// verification misses keeps serving normally while it silently stops rotating. The loop
// therefore counts it here — and ONLY here. Every request-path failure is RETURNED
// (VerificationKey's third value, SigningKey's error) and counted once by the engine;
// counting those here as well would report one outage as two.
func (s *IngressKeyStore) refreshTick(now time.Time, onErr func(store string)) {
	if err := s.refreshOnce(now); err != nil {
		log.Printf("pgstore: ingress key refresh: %v", err)
		if onErr != nil {
			onErr(storeNameIngressKey)
		}
	}
}

// refreshOnce is the RunRefresh loop body: one unthrottled reload, then rotation if the
// reload found no key inside its rotation interval. Rotation lives HERE, independent of
// request traffic: the request path's rotation is throttled like every miss, and a flood
// of unknown-kid bearers (one per second re-stamps lastReload) would otherwise keep every
// token request on the grace branch until the slack ran out. With this, the request
// path's grace is a bridge of at most ingressKeyRefresh.
func (s *IngressKeyStore) refreshOnce(now time.Time) error {
	if err := s.reload(now); err != nil {
		return err
	}
	return s.rotateIfDue(now)
}

// rotateIfDue adopts or creates a committed signing key when no cached key is
// eligible. signMu serializes this process; the transaction lock coordinates holders'
// concurrent creators across processes.
func (s *IngressKeyStore) rotateIfDue(now time.Time) error {
	s.signMu.Lock()
	defer s.signMu.Unlock()
	s.mu.Lock()
	if kid := s.newestLiveLocked(now); kid != "" {
		s.signing = kid
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	kid, c, err := s.selectOrCreateKey(now) // DB round trip: no cache lock held
	if err != nil {
		return fmt.Errorf("rotate: %w", err)
	}
	s.mu.Lock()
	s.keys[kid] = c
	s.signing = kid
	s.mu.Unlock()
	log.Printf("pgstore: ingress signing key adopted by refresh: %s", kid)
	return nil
}

// reload runs one reload unconditionally (the refresh loop only: refreshOnce and
// wakeTick). SigningKey's slow path goes through reloadIfDue; the verification miss path
// goes through reloadIfDueNoWait.
func (s *IngressKeyStore) reload(now time.Time) error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	return s.reloadLocked(now)
}

// reloadIfDue is SIGNING's reload — the only path that still WAITS. /oauth/token is an
// authenticated call that must come back with a bearer or an honest 503, and its caller
// is one registered client, not an anonymous flood: waiting out one round trip there is
// worth resolving an eligible signing key. The verification miss path uses
// reloadIfDueNoWait instead; see the type doc.
//
// ran is true only when THIS caller executed the reload, so its caller knows whether the
// snapshot it is about to read is its own or one it merely waited on.
//
// A reload already in flight is waited on rather than queried around: its result is what
// this caller needs (errors included — see the reloadLatch), and the throttle it stamped
// at its own start would otherwise refuse a key the reload is about to bring in. The wait
// is bounded by that reload's storeTimeout; no lock is held across it.
func (s *IngressKeyStore) reloadIfDue(now time.Time) (bool, error) {
	if l := s.inFlight(); l != nil {
		<-l.done
		// The waiter takes the branch the RELOADER took: a reload that failed leaves this
		// caller as unable to reach the database as if it had queried itself, and a
		// SigningKey waiter that read only "finished" would skip its grace-sign and try to
		// insert against the dead database.
		return false, l.err
	}
	if s.throttled(now) {
		return false, errReloadThrottled
	}
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	// Under reloadMu no other reload can be in flight (a reload clears the latch before
	// releasing this lock), so the throttle is the only thing left to re-check: a caller
	// that queued behind one finds lastReload freshly stamped, and its own caller
	// re-reads the cache that reload just filled.
	if s.throttled(now) {
		return false, errReloadThrottled
	}
	return true, s.reloadLocked(now)
}

// reloadIfDueNoWait is the VERIFICATION miss path's reload. It runs one reload when the
// throttle allows and nothing else is reloading, and otherwise DECLINES immediately:
// never waiting on the in-flight latch, and never blocking on reloadMu (TryLock, not
// Lock — a reload parked in a hung query holds reloadMu for its whole storeTimeout, and
// Lock here would hand an anonymous caller a parked goroutine per request, which is the
// thing this path exists not to do). The cold path uses it under the same rules; what a
// cold caller does with a decline is park (resolveCold), never query around it.
//
// ran is true only when THIS caller executed the reload, so its caller knows whether the
// snapshot it is about to read is its own and may seed the negative cache from it.
func (s *IngressKeyStore) reloadIfDueNoWait(now time.Time) (bool, error) {
	return s.reloadIfDueNoWaitWithin(now, storeTimeout)
}

// reloadIfDueNoWaitWithin is reloadIfDueNoWait with the round trip's own ceiling: the
// warm path passes storeTimeout; a cold caller passes what is left of its bound, so its
// one deadline covers the query it runs as well as the waits it takes.
func (s *IngressKeyStore) reloadIfDueNoWaitWithin(now time.Time, timeout time.Duration) (bool, error) {
	if s.inFlight() != nil {
		return false, errReloadInFlight
	}
	if s.throttled(now) {
		return false, errReloadThrottled
	}
	if !s.reloadMu.TryLock() {
		// Held by a reload that has not published its latch yet (or by refreshOnce).
		return false, errReloadInFlight
	}
	defer s.reloadMu.Unlock()
	// Under reloadMu no other reload can be in flight, so the throttle is the only thing
	// left to re-check: a caller that arrived just behind one finds lastReload freshly
	// stamped, and its own caller re-reads the cache that reload just filled.
	if s.throttled(now) {
		return false, errReloadThrottled
	}
	return true, s.reloadLockedWithin(now, timeout)
}

func (s *IngressKeyStore) throttled(now time.Time) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return now.Sub(s.lastReload) < reloadThrottle
}

// reloadLocked requires reloadMu. It takes mu only to stamp the attempt, publish the
// in-flight latch, advance reloadStarted and swap the caches; the DB round trip runs with
// no cache lock held. The completion — the latch's result and close, lastReloadErr,
// reloadDone and the epoch broadcast — runs under defer, so a panicking round trip can
// never leave a waiter parked forever or the throttle pinned open.
func (s *IngressKeyStore) reloadLocked(now time.Time) error {
	return s.reloadLockedWithin(now, storeTimeout)
}

// reloadLockedWithin is reloadLocked with the round trip's ceiling supplied (see
// reloadIfDueNoWaitWithin); every other reload passes storeTimeout.
func (s *IngressKeyStore) reloadLockedWithin(now time.Time, timeout time.Duration) (err error) {
	l := &reloadLatch{done: make(chan struct{})}
	s.mu.Lock()
	s.lastReload = now // the ATTEMPT, so a failing DB is throttled like a healthy one
	s.inflight = l
	s.reloadStarted++ // the SELECT is about to be issued: a request that reads this
	// count from here on cannot be satisfied by this reload's snapshot
	s.mu.Unlock()
	defer func() {
		// The result is written BEFORE the close: closing the channel is the
		// happens-before edge every waiter reads l.err through.
		l.err = err
		s.mu.Lock()
		if end := s.now(); end.After(s.lastReload) {
			// …and again on COMPLETION: a query that burned the whole storeTimeout must
			// not leave the next caller outside a window measured from its start.
			s.lastReload = end
		}
		s.lastReloadErr = err // what a throttled miss inside this window must answer from
		s.inflight = nil
		s.reloadDone++
		epoch := s.reloadEpoch
		s.reloadEpoch = make(chan struct{})
		s.mu.Unlock()
		close(epoch)
		close(l.done)
	}()
	return s.reloadQuery(now, timeout)
}

// reloadQuery is reloadLocked's round trip + cache swap (split out so the stamping and
// latch bookkeeping above read as one block), bounded by timeout. Requires reloadMu.
func (s *IngressKeyStore) reloadQuery(now time.Time, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	rows, err := s.pool.Query(ctx, `SELECT kid, private_key_pem, created_at, not_after FROM gw_ingress_key WHERE holder_id=$1 AND not_after > $2`, s.holderID, now)
	if err != nil {
		return err
	}
	defer rows.Close()
	fresh := map[string]cachedKey{}
	unreadable, firstUnreadable := 0, ""
	for rows.Next() {
		var kid, pemStr string
		var created, notAfter time.Time
		if err := rows.Scan(&kid, &pemStr, &created, &notAfter); err != nil {
			return err
		}
		key, err := parsePKCS8EC(pemStr)
		if err != nil {
			// Isolate the row: a key material blob this build cannot read OR cannot sign
			// with (see parsePKCS8EC — wrong curve included) makes THAT kid unverifiable
			// and unadoptable, never the whole snapshot. Failing the reload here would
			// freeze s.keys (no sibling's key is ever learned), return before
			// rotateIfDue and keep SigningKey's slow path from ever reaching selectOrCreateKey —
			// whose DELETE is the only purge — so the bad row would never leave and the
			// token endpoint would 503 for good once the last good key aged out.
			unreadable++
			if firstUnreadable == "" {
				firstUnreadable = kid
			}
			continue
		}
		fresh[kid] = cachedKey{key: key, created: created, notAfter: notAfter}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if unreadable > 0 {
		// The kid only: never the PEM, and never the parse error, which can quote the
		// bytes it choked on.
		log.Printf("pgstore: ingress key rows unreadable, skipped %d (first kid %s)", unreadable, firstUnreadable)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Merge, do not replace. A reload that took its snapshot before this process's own
	// adoption committed does not carry that key, and a wholesale swap landing after
	// SigningKey wrote it into the cache would drop it: s.signing would name an absent
	// kid and the next SigningKey would answer 503 until the following reload. Keep any
	// cached key the snapshot missed that is still inside its not_after — the same
	// liveness the query itself filters on, so an expired key is never resurrected.
	for kid, c := range s.keys {
		if _, ok := fresh[kid]; !ok && now.Before(c.notAfter) {
			fresh[kid] = c
		}
	}
	s.keys = fresh
	// The cold/warm line is crossed HERE, in the section that installs the snapshot — the
	// moment this reload's success is decided — not in reloadLocked's completion section
	// a few instructions later: a miss that lands between the two answers from this
	// snapshot on the warm path rather than parking for the next one.
	s.loaded = true
	for kid, seen := range s.negCache { // prune: expired entries and kids now known
		if _, known := fresh[kid]; known || now.Sub(seen) >= negCacheTTL {
			delete(s.negCache, kid)
		}
	}
	return nil
}

func (s *IngressKeyStore) newestLiveLocked(now time.Time) string {
	best := ""
	for kid, c := range s.keys {
		if !now.Before(c.created.Add(ingressKeyRotation)) || !now.Before(c.notAfter) {
			continue // past its signing or verification life
		}
		if best == "" || c.created.After(s.keys[best].created) || (c.created.Equal(s.keys[best].created) && kid > best) {
			best = kid
		}
	}
	return best
}

// negCacheAddLocked records kid as absent, evicting in FIFO order. O(1) BY CONSTRUCTION:
// the ring slot about to be written names the oldest kid still holding a slot, so there is
// no scan for an oldest entry and no timestamp comparison — which also means a stalled or
// backwards-stepping clock cannot stop eviction. Every insertion consumes exactly one of
// negCacheMax slots, so the map can never exceed the bound. Requires mu.
func (s *IngressKeyStore) negCacheAddLocked(kid string, now time.Time) {
	if _, dup := s.negCache[kid]; dup {
		s.negCache[kid] = now // refresh the stamp; the kid already owns a slot
		return
	}
	if old := s.negRing[s.negNext]; old != "" {
		delete(s.negCache, old) // a no-op if the TTL prune already dropped it
	}
	s.negRing[s.negNext] = kid
	s.negNext = (s.negNext + 1) % len(s.negRing)
	s.negCache[kid] = now
}

// parsePKCS8EC reads one stored key row. It rejects anything the ingress bearer cannot be
// signed with — including an EC key on the wrong CURVE: issued bearers are ES384, which
// only a P-384 key can produce, and this table has more than one writer. A P-256 row that
// happened to be the newest would otherwise be adopted as the signing key and turn every
// /oauth/token call into a failure to sign. Rejecting it here folds it into reloadQuery's
// unreadable-row isolation, so it degrades exactly that kid. The error names the curve and
// never the key bytes.
func parsePKCS8EC(pemStr string) (*ecdsa.PrivateKey, error) {
	blk, _ := pem.Decode([]byte(pemStr))
	if blk == nil {
		return nil, fmt.Errorf("no PEM block")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	ec, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not an EC key")
	}
	if ec.Curve != elliptic.P384() {
		return nil, fmt.Errorf("unsupported curve %s (ingress bearers are ES384: P-384 only)", ec.Curve.Params().Name)
	}
	return ec, nil
}
