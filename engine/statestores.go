package engine

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// IngressKeyStore holds the key that signs issued ingress bearers and resolves a
// bearer's kid to the public key that verifies it. Behind a shared store every
// replica of a holder signs and verifies with the same material; the in-memory
// default is one process-local key, which is only correct at one replica.
// SigningKey may rotate; VerificationKey must answer for every key still inside
// its verification life. Implementations fail closed: an unknown kid, a store
// error or a timeout is (nil, false) / a non-nil err, never a guess.
//
// VerificationKey's three shapes are distinct and load-bearing:
//
//	(pub, true, nil)  — kid resolved
//	(nil, false, nil) — unknown or expired kid: the caller answers its ordinary 401
//	(nil, false, err) — the store could NOT TELL (its backing store errored or timed
//	                    out): the caller answers a retryable 503 and counts the
//	                    outage, because a 401 would report a database failure as a
//	                    bad credential
//
// A store that merely throttles its own lookups must NOT report that as err — the honest
// answer is (nil, false, nil), and the store loads the kid out of band for the next call.
// It must also not make the caller WAIT for that: this seam is reached from the ingress
// bearer header, which is read before the bearer's signature is checked, so an
// implementation that blocks on a store round trip hands an unauthenticated caller a
// held goroutine per request.
type IngressKeyStore interface {
	SigningKey(now time.Time) (kid string, key *ecdsa.PrivateKey, err error)
	VerificationKey(kid string, now time.Time) (pub *ecdsa.PublicKey, ok bool, err error)
}

// ReplayStore is the one-time-use record behind every replay guard. CheckAndRecord
// records (scope, clientID, key) and reports whether it was already recorded and
// unexpired. clientID is the issuer the key is unique under (RFC 7523 §3) and is ""
// for scopes with a single issuer.
//
// err is non-nil when the store could not tell (a store error or timeout), and
// implementations MUST answer replay=true alongside it. Callers MUST treat err != nil
// as a rejection (fail closed); the error exists so a caller can distinguish an
// outage from a genuine replay when it chooses the status it answers with — the
// token endpoint owes a retryable 503, not a 401 that reads as a bad credential.
type ReplayStore interface {
	CheckAndRecord(scope, clientID, key string, now, expiresAt time.Time) (replay bool, err error)
}

// MaxReplayKeyBytes bounds the one-time-use key (a caller-supplied jti or correlationId).
// The key is part of the record's primary key, so a multi-kilobyte value overflows the
// btree index row on a perfectly HEALTHY database — which surfaces as a 503 and a counted
// store error, i.e. an oversized credential field reading as an outage of the platform.
// Every caller therefore rejects a longer key with its own ordinary refusal BEFORE the
// store is consulted; both stores refuse one too (defence in depth, and the mirror must
// not be looser than the oracle), and gw_replay carries the same bound as a CHECK.
//
// 512 bytes is far above any real jti: RFC 7519 leaves the value opaque, and the
// SMART Backend Services clients this ingress serves send a UUID or a random 32-byte
// token. Nothing legitimate is being cut off here.
const MaxReplayKeyBytes = 512

// Replay scopes. The window is the CALLER's on both backends — every call site passes
// expiresAt = now + its own window, unchanged from the single-process guards these
// replace — so neither the mirror nor the oracle carries a window of its own to drift.
const (
	ReplayScopeIngressJTI    = "ingress-jti"    // client_assertion jti, per client_id, ingressJTIWindow
	ReplayScopeHubJTI        = "hub-jti"        // X-Hub-Assertion jti, shnsdk.MaxAssertionTTL
	ReplayScopePatientAccess = "patient-access" // patient-access correlationId, paReplayWindow
)

// kidHexLen is the wire shape of a bearer kid: 32 lowercase hex characters (16
// random bytes). verifyBearer rejects any other shape before consulting the store,
// so a forged header cannot drive store lookups.
const kidHexLen = 32

func newKID() (string, error) {
	var b [kidHexLen / 2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("ingress kid: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func validKID(kid string) bool {
	if len(kid) != kidHexLen {
		return false
	}
	for i := 0; i < len(kid); i++ {
		c := kid[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// ephemeralKeyStore is the no-DSN default: one P-384 key generated at
// construction, one kid in the wire shape, never rotates. This is exactly the
// pre-store behavior plus the kid header.
type ephemeralKeyStore struct {
	kid string
	key *ecdsa.PrivateKey
}

func newEphemeralKeyStore() (*ephemeralKeyStore, error) {
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("ingress bearer key: %w", err)
	}
	kid, err := newKID()
	if err != nil {
		return nil, err
	}
	return &ephemeralKeyStore{kid: kid, key: key}, nil
}

func (s *ephemeralKeyStore) SigningKey(time.Time) (string, *ecdsa.PrivateKey, error) {
	return s.kid, s.key, nil
}

// VerificationKey never returns an error: the one key is in this process's memory, so
// there is no store that could fail to answer.
func (s *ephemeralKeyStore) VerificationKey(kid string, _ time.Time) (*ecdsa.PublicKey, bool, error) {
	if kid != s.kid {
		return nil, false, nil
	}
	return &s.key.PublicKey, true, nil
}

// memReplayPurgeInterval throttles the in-memory sweep of expired records. It matches
// the Postgres store's own purge throttle (pgstore.replayPurgeInterval): the sweep is
// hygiene on both sides — the lookup below applies the same strict expiry rule whether or
// not an expired entry has been swept — so the two backends agree without the constants
// being shared across the module boundary.
const memReplayPurgeInterval = time.Minute

// memReplayMaxEntries is the per-scope safety ceiling of the in-memory mirror.
//
// The mirror evicts ONLY expired entries, because the table it stands in for (gw_replay)
// has no cap at all: a bounded set that sheds an UNEXPIRED entry to stay bounded is
// looser than the oracle in the one direction that matters — a key already spent comes
// back as a first use. So the set is allowed to grow with the traffic inside a window,
// and this is the bound that keeps it finite.
//
// 1<<20 per scope is far above anything a deployment reaches: the widest window is the
// patient-access hour, and a million distinct correlationIds an hour at ONE replica is
// orders of magnitude past this product's traffic. Reaching it means something is wrong.
// At the ceiling the store fails CLOSED — (replay=true, err) naming the scope — because a
// store that cannot record cannot tell a first use from a replay, and that pair is exactly
// the documented "could not tell" shape (see the ReplayStore contract above): the caller
// answers a retryable 503 rather than accusing an honest client of replaying.
const memReplayMaxEntries = 1 << 20

// memReplayScope is one scope's one-time-use record: key → expiresAt, evicted by EXPIRY
// alone. It mirrors one holder+scope slice of gw_replay, including the strict
// `expires_at < now` re-arm rule, so the two answer the same thing in both directions.
type memReplayScope struct {
	mu        sync.Mutex
	max       int
	seen      map[string]time.Time // key → the expiresAt the caller recorded it with
	lastPurge time.Time            // zero until the first call; see maybePurgeLocked
}

// memReplayStore is the no-DSN default: one record set per scope, process-local and
// therefore correct only at a single replica. It stores the caller's expiresAt exactly as
// the table does and honours it with the same strict rule (`expires_at < now` ⇒ re-armed),
// so a scope's window is entirely the caller's (every caller passes now+window) and the
// mirror carries no window constant of its own to drift from the oracle's.
type memReplayStore struct {
	scopes map[string]*memReplayScope
}

// NewInMemoryReplayStore returns the process-local ReplayStore: correct only at a
// single replica. The interface, not the concrete type, is the exported shape — the
// struct behind it stays an implementation detail.
func NewInMemoryReplayStore() ReplayStore {
	return newMemReplayStore(memReplayMaxEntries)
}

// newMemReplayStore builds the mirror at a given per-scope ceiling. The ceiling is a
// parameter so the fail-closed row can drive the ceiling branch without materialising
// 1<<20 records per scope; every shipped construction goes through NewInMemoryReplayStore.
func newMemReplayStore(max int) *memReplayStore {
	s := &memReplayStore{scopes: map[string]*memReplayScope{}}
	for _, scope := range []string{ReplayScopeIngressJTI, ReplayScopeHubJTI, ReplayScopePatientAccess} {
		s.scopes[scope] = &memReplayScope{max: max, seen: map[string]time.Time{}}
	}
	return s
}

// CheckAndRecord keys the record on (len(clientID), clientID, key) so the client
// boundary is structural: ("ab","c") and ("a","bc") are distinct identities.
func (s *memReplayStore) CheckAndRecord(scope, clientID, key string, now, expiresAt time.Time) (bool, error) {
	if len(key) > MaxReplayKeyBytes {
		// Defence in depth: every caller rejects an oversized key before it gets here, and
		// the Postgres oracle refuses it too (pgstore.ReplayStore) — the mirror must be
		// neither stricter nor looser than the store it stands in for.
		return true, fmt.Errorf("replay: key exceeds %d bytes", MaxReplayKeyBytes)
	}
	sc, ok := s.scopes[scope]
	if !ok {
		// Fail closed AND say why: a scope outside the engine's constants is a
		// programming error, not a replay, and the caller must be able to answer
		// "unavailable" rather than accuse the client of replaying. The Postgres
		// oracle answers the same shape (pgstore.ReplayStore).
		return true, fmt.Errorf("replay: unknown scope %q", scope)
	}
	return sc.checkAndRecord(scope, strconv.Itoa(len(clientID))+":"+clientID+key, now, expiresAt)
}

func (sc *memReplayScope) checkAndRecord(scope, k string, now, expiresAt time.Time) (bool, error) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.maybePurgeLocked(now)
	exp, known := sc.seen[k]
	if known && !exp.Before(now) {
		// Spent. gw_replay re-arms only on `expires_at < now` (strict), so AT exactly
		// expiresAt the key is still spent; the mirror uses the identical comparison.
		return true, nil
	}
	if !known && len(sc.seen) >= sc.max {
		// Never let the purge THROTTLE be what turns a full set into a refusal: sweep
		// first, then decide. Only unexpired records can hold the ceiling.
		sc.purgeLocked(now)
		if len(sc.seen) >= sc.max {
			return true, fmt.Errorf("replay %s: in-memory one-time-use record is full at %d entries (cannot record)", scope, sc.max)
		}
	}
	sc.seen[k] = expiresAt
	return false, nil
}

// maybePurgeLocked sweeps expired records at most once per memReplayPurgeInterval. The
// first call stamps the throttle without sweeping — the pg store stamps it at
// construction, and the mirror has no clock until a caller brings one. Correctness never
// depends on the sweep: checkAndRecord applies the expiry rule to whatever it finds.
func (sc *memReplayScope) maybePurgeLocked(now time.Time) {
	if sc.lastPurge.IsZero() {
		sc.lastPurge = now
		return
	}
	if now.Sub(sc.lastPurge) < memReplayPurgeInterval {
		return
	}
	sc.purgeLocked(now)
}

// purgeLocked drops every EXPIRED record and nothing else. There is no flood-shed step:
// shedding a live record is precisely how a bounded mirror becomes looser than the
// unbounded table it stands in for.
func (sc *memReplayScope) purgeLocked(now time.Time) {
	sc.lastPurge = now
	for k, exp := range sc.seen {
		if exp.Before(now) {
			delete(sc.seen, k)
		}
	}
}
