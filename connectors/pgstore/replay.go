package pgstore

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
)

var _ engine.ReplayStore = (*ReplayStore)(nil)

// replayPurgeInterval throttles the sweep of expired rows; the sweep is hygiene,
// never load-bearing (the DO UPDATE … WHERE branch re-arms expired rows in place).
const replayPurgeInterval = time.Minute

// replayScopes is the closed set of scopes this store records. The in-memory mirror
// (engine.NewInMemoryReplayStore) holds one record set per scope and fails closed on any
// other scope; the shared oracle must be neither stricter nor looser than the mirror,
// so an unknown scope is a replay here too — decided before any statement runs, so a
// scope string the engine never emits cannot drive database work.
var replayScopes = map[string]bool{
	engine.ReplayScopeIngressJTI:    true,
	engine.ReplayScopeHubJTI:        true,
	engine.ReplayScopePatientAccess: true,
}

// ReplayStore is the Postgres one-time-use record shared by every replica of a
// holder. One statement decides: RowsAffected()==1 is a first use (fresh insert or
// an expired row re-armed), 0 is a replay. Any error is a replay (fail closed) AND is
// returned, so the caller can answer "unavailable" rather than "replayed".
// A row expires when expires_at < now (strict): at exactly expires_at the key is still
// spent. The in-memory mirror stores the same caller-supplied expires_at and applies the
// identical comparison, and — like this table — evicts ONLY what has expired, so neither
// side sheds a live record the other still holds.
// A scope outside replayScopes is a replay without touching the database, and so is a key
// longer than engine.MaxReplayKeyBytes (which the gw_replay CHECK would refuse anyway, and
// whose index row a multi-kilobyte value would overflow on a healthy database).
type ReplayStore struct {
	pool      *pgxpool.Pool
	holderID  string
	now       func() time.Time
	mu        sync.Mutex
	lastPurge time.Time
}

func NewReplayStore(pool *pgxpool.Pool, holderID string, clock func() time.Time) *ReplayStore {
	if clock == nil {
		clock = time.Now
	}
	return &ReplayStore{pool: pool, holderID: holderID, now: clock, lastPurge: clock()}
}

func (s *ReplayStore) CheckAndRecord(scope, clientID, key string, now, expiresAt time.Time) (bool, error) {
	if !replayScopes[scope] {
		log.Printf("pgstore: replay: unknown scope %q (rejecting as replay)", scope)
		return true, fmt.Errorf("pgstore: replay: unknown scope %q", scope)
	}
	if len(key) > engine.MaxReplayKeyBytes {
		// Decided before any statement runs: an oversized key is a caller-supplied value,
		// and letting it reach the index would turn it into a 503 on a healthy database.
		// The in-memory mirror refuses it the same way.
		log.Printf("pgstore: replay %s: key of %d bytes exceeds %d (rejecting as replay)", scope, len(key), engine.MaxReplayKeyBytes)
		return true, fmt.Errorf("pgstore: replay %s: key exceeds %d bytes", scope, engine.MaxReplayKeyBytes)
	}
	s.maybePurge(now)
	ctx, cancel := storeCtx()
	defer cancel()
	tag, err := s.pool.Exec(ctx, `
INSERT INTO gw_replay (holder_id, scope, client_id, key, expires_at) VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (holder_id, scope, client_id, key) DO UPDATE SET expires_at = EXCLUDED.expires_at
    WHERE gw_replay.expires_at < $6`,
		s.holderID, scope, clientID, key, expiresAt, now)
	if err != nil {
		log.Printf("pgstore: replay %s: %v (rejecting as replay)", scope, err)
		// Fail closed, and hand the error up: the caller rejects either way, but the
		// token endpoint owes a retryable 503 for an outage and a 401 only for a real
		// replay. Losing the distinction here is what made a database outage read as a
		// bad credential.
		return true, fmt.Errorf("pgstore: replay %s: %w", scope, err)
	}
	return tag.RowsAffected() != 1, nil
}

// maybePurge deletes the holder's expired rows at most once per replayPurgeInterval.
// Errors are logged; correctness never depends on the sweep.
func (s *ReplayStore) maybePurge(now time.Time) {
	s.mu.Lock()
	if now.Sub(s.lastPurge) < replayPurgeInterval {
		s.mu.Unlock()
		return
	}
	s.lastPurge = now
	s.mu.Unlock()
	if err := s.purgeExpired(now); err != nil {
		log.Printf("pgstore: replay purge: %v", err)
	}
}

func (s *ReplayStore) purgeExpired(now time.Time) error {
	ctx, cancel := storeCtx()
	defer cancel()
	_, err := s.pool.Exec(ctx, `DELETE FROM gw_replay WHERE holder_id=$1 AND expires_at < $2`, s.holderID, now)
	return err
}
