package pgstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
)

var _ engine.ExchangeStore = (*ExchangeStore)(nil)

// exchangePurgeInterval throttles the lazy sweep of expired exchanges on Begin. The
// interval matches the in-memory mirror's (one minute), but the two arm it
// differently: the mirror sweeps on its own first Begin, while this store seeds
// lastPurge at construction — invisible through the ExchangeStore interface,
// since both backends filter on expiry at read regardless of when the sweep runs.
const exchangePurgeInterval = time.Minute

// defaultExchangeTTL mirrors the engine's unexported defaultExchangeTTL constant
// (engine/exchangestore.go): 168 hours, applied when ttl is non-positive. The
// engine constant can't be referenced directly (it's unexported), so the value is
// duplicated here; a non-positive ttl must default the same way on both backends
// (mirror == oracle at the constructor boundary), never expire on arrival.
const defaultExchangeTTL = 168 * time.Hour

// exchangeBreakerOpen is how long the exchange seam stops touching the database after a
// round trip failed. Every CRD/DTR/PAS request makes two synchronous calls through this
// seam (Begin's insert, AppendLeg's transaction) plus a once-a-minute purge, each bounded
// only by storeTimeout — so an unreachable database adds that bound to every request on a
// path that does not otherwise need the database at all (a direct-bearer CRD relay). The
// seam is best-effort by contract, so skipping it costs nothing an outage was not already
// costing: 5 s is short enough that a blip re-arms the correlation records within a
// handful of requests and long enough that a sustained outage stops dominating latency.
//
// ONLY this seam gets a breaker. The replay record and the ingress key store are
// correctness-bearing (they decide whether a caller is admitted), so they keep their full
// storeTimeout and answer 503 rather than skipping.
const exchangeBreakerOpen = 5 * time.Second

// errExchangeStoreUnavailable is what a skipped AppendLeg returns. It is deliberately NOT
// the "unknown exchange" caller error: the gateway counts this as a store failure, exactly
// as it counts the failure this skip stands in for.
var errExchangeStoreUnavailable = errors.New("exchange store unavailable (recent store failure)")

// exchangeDB is the narrow pool surface this store uses. *pgxpool.Pool satisfies it;
// tests wrap or replace it to count round trips (the breaker's whole claim is about round
// trips NOT made, which nothing else can observe).
type exchangeDB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

// randRead is crypto/rand.Read, indirected so a test can drive Begin's failure branch.
// Never reassigned outside tests.
var randRead = rand.Read

// ExchangeStore is the durable correlation seam (metadata only, one row per leg,
// LegPhysics flattened to four columns so the schema is reviewable against
// LegRecord). Best-effort by contract: Begin returns an Exchange whenever it returns at
// all (the id is minted here, the insert is attempted and its error logged and counted);
// AppendLeg returns the error for the gateway to log and count; Get reads the unexpired
// exchange and its legs in seq order into a fresh value. Every call is bounded by
// storeTimeout. The ONE thing Begin does not survive is a failed random source, which it
// refuses exactly as the in-memory mirror does (see Begin).
//
// A store failure opens a fail-fast breaker for exchangeBreakerOpen: while it is open the
// seam makes NO database calls and returns the same best-effort outcomes it would have
// returned on a failure (Begin still hands back its Exchange, AppendLeg returns a store
// error, Get reports absent), and when the window elapses exactly ONE call goes through as
// a half-open probe — success closes the breaker, failure re-opens it. A skipped call
// produces the SAME accounting as the failed call it stands in for (Begin counts through
// the error hook, AppendLeg's error is counted by the gateway), so StoreError still
// reports one outage per request; what it does not produce is a log line per request —
// the breaker logs once when it opens and once when it closes.
//
// The in-memory mirror (engine's inMemoryExchangeStore) has no database and therefore no
// breaker: there is nothing to fail and nothing to skip, and the observable contract is
// unchanged on both backends (a best-effort seam that may lose records), so this needs no
// parity row.
type ExchangeStore struct {
	pool      exchangeDB
	holderID  string
	ttl       time.Duration
	now       func() time.Time
	mu        sync.Mutex
	lastPurge time.Time
	// breakerUntil is when the fail-fast window ends; the zero value means CLOSED (the
	// database is trusted). probing marks the single half-open call in flight, so a burst
	// arriving at the instant a window elapses still costs one round trip, not one each.
	breakerUntil time.Time
	probing      bool
	// errHook counts a store failure this seam absorbs rather than returns (the
	// best-effort Begin insert), so a database outage is visible to an operator and
	// not only in the log. Optional; set once via SetErrorHook before the store is
	// shared with request-serving goroutines. nil ⇒ no counting.
	errHook func(store string)
}

// SetErrorHook wires the store-error counter (the application layer passes the same
// EMF hook the engine's Config.StoreErrorMetric gets). Call it before the store serves
// requests; nil-safe at every call site.
func (s *ExchangeStore) SetErrorHook(h func(store string)) { s.errHook = h }

func (s *ExchangeStore) noteErr() {
	if s.errHook != nil {
		s.errHook(storeNameExchange)
	}
}

// breakerAllows reports whether this call may touch the database. While the window is
// open it says no; when the window has elapsed it says yes to exactly one caller (the
// half-open probe) and no to the rest until that probe reports.
func (s *ExchangeStore) breakerAllows(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.breakerUntil.IsZero() {
		return true // closed: the database is trusted
	}
	if now.Before(s.breakerUntil) || s.probing {
		return false
	}
	s.probing = true // this call is the half-open probe
	return true
}

// breakerReport records what a round trip that WAS allowed found. A store error opens (or
// re-opens) the window; a success closes it. The open/close pair is the only logging the
// breaker does — the per-request line is exactly what it exists to stop.
//
// started is the clock read the caller took BEFORE its round trip; the window is stamped
// from the clock read HERE, at completion. Every call on this seam is bounded by
// storeTimeout and an unreachable database is exactly what burns it, so a window measured
// from the start would be exchangeBreakerOpen minus that timeout — three seconds instead of
// five — and the seam would resume putting a two-second round trip in front of every
// request that much early. (The ingress key store stamps its own throttle the same way, and
// for the same reason.) A clock that stepped backwards is clamped to started, so the window
// is never shorter than the caller's own read.
func (s *ExchangeStore) breakerReport(started time.Time, err error) {
	now := s.now()
	if now.Before(started) {
		now = started
	}
	s.mu.Lock()
	wasOpen := !s.breakerUntil.IsZero()
	s.probing = false
	if err != nil {
		s.breakerUntil = now.Add(exchangeBreakerOpen)
	} else {
		s.breakerUntil = time.Time{}
	}
	s.mu.Unlock()
	switch {
	case err != nil && !wasOpen:
		log.Printf("pgstore: exchange store unreachable; skipping its calls for %v: %v", exchangeBreakerOpen, err)
	case err == nil && wasOpen:
		log.Printf("pgstore: exchange store reachable again; resuming its calls")
	}
}

// reportPanic releases the half-open slot when a guarded round trip panics. Without it a
// panic would leave probing == true with the window already elapsed, and breakerAllows
// would refuse every later call forever — one panic would take the correlation seam out
// for the life of the process. A panicking call is not evidence of health, so the window
// re-opens; the panic itself is re-raised unchanged, since it is the caller's to handle.
//
// started is the caller's pre-call clock read, and breakerReport stamps the window from the
// completion it reads for itself: a panic can come back just as late as an error can.
func (s *ExchangeStore) reportPanic(started time.Time) {
	if r := recover(); r != nil {
		s.breakerReport(started, fmt.Errorf("exchange store call panicked: %v", r))
		panic(r)
	}
}

// NewExchangeStore returns the Postgres-backed ExchangeStore bound to holderID.
func NewExchangeStore(pool exchangeDB, holderID string, ttl time.Duration, clock func() time.Time) *ExchangeStore {
	if ttl <= 0 {
		ttl = defaultExchangeTTL
	}
	if clock == nil {
		clock = time.Now
	}
	return &ExchangeStore{pool: pool, holderID: holderID, ttl: ttl, now: clock, lastPurge: clock()}
}

func (s *ExchangeStore) Begin(workstream string) *engine.Exchange {
	now := s.now()
	s.maybePurge(now)
	// 16 random bytes as 32 hex chars. The in-memory store uses the engine's
	// newCorrelationID() instead; ids are opaque to every caller, so the shapes may differ.
	//
	// A failed random source is unrecoverable and is refused here exactly as the
	// in-memory mirror refuses it (engine.newCorrelationID panics): continuing would
	// mint an all-zero id for every affected call, whose primary-key collision this
	// best-effort insert would then swallow — aliasing unrelated exchanges' legs onto
	// one row. The mirror must never be stricter than the store it stands in for.
	var b [16]byte
	if _, err := randRead(b[:]); err != nil {
		panic(fmt.Sprintf("pgstore: crypto/rand failed generating exchange id: %v", err))
	}
	id := hex.EncodeToString(b[:])
	if !s.breakerAllows(now) {
		// Skipped, not hidden: the accounting is exactly what the failed insert this
		// stands in for produced (counted here — nothing else reports Begin), minus the
		// per-request log line the breaker's open/close pair replaces.
		s.noteErr()
		return &engine.Exchange{ID: id, Workstream: workstream}
	}
	defer s.reportPanic(now)
	ctx, cancel := storeCtx()
	defer cancel()
	_, err := s.pool.Exec(ctx, `INSERT INTO gw_exchange (holder_id, exchange_id, workstream, created_at, expires_at) VALUES ($1,$2,$3,$4,$5)`,
		s.holderID, id, workstream, now, now.Add(s.ttl))
	s.breakerReport(now, err)
	if err != nil {
		// Best-effort: the id is already minted, so the caller gets its Exchange either
		// way and the follow-up AppendLeg is what the gateway logs and counts. The
		// insert failure itself is counted here — nothing else reports it.
		log.Printf("pgstore: exchange begin %s: %v", id, err)
		s.noteErr()
	}
	return &engine.Exchange{ID: id, Workstream: workstream}
}

func (s *ExchangeStore) AppendLeg(exchangeID string, rec engine.LegRecord) error {
	now := s.now()
	if !s.breakerAllows(now) {
		// The gateway counts this exactly as it counts the failure it stands in for.
		return fmt.Errorf("ExchangeStore: append leg to %q: %w", exchangeID, errExchangeStoreUnavailable)
	}
	defer s.reportPanic(now)
	err, storeErr := s.appendLeg(exchangeID, rec, now)
	s.breakerReport(now, storeErr)
	return err
}

// appendLeg is AppendLeg's round trip. It returns the error the caller sees and, second,
// that error again ONLY when it was the database's — a caller error (appending to an
// exchange this holder does not have) says nothing about reachability and must never open
// the breaker.
func (s *ExchangeStore) appendLeg(exchangeID string, rec engine.LegRecord, now time.Time) (err, storeErr error) {
	ctx, cancel := storeCtx()
	defer cancel()
	tx, berr := s.pool.Begin(ctx)
	if berr != nil {
		e := fmt.Errorf("ExchangeStore: begin tx: %w", berr)
		return e, e
	}
	defer tx.Rollback(ctx) // no-op after a successful Commit; covers every error path below
	var one int
	err = tx.QueryRow(ctx, `SELECT 1 FROM gw_exchange WHERE holder_id=$1 AND exchange_id=$2 AND expires_at > $3 FOR UPDATE`,
		s.holderID, exchangeID, now).Scan(&one)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("ExchangeStore: append to unknown exchange %q", exchangeID), nil // caller error (same text as the in-memory store)
	case err != nil:
		e := fmt.Errorf("ExchangeStore: lookup exchange %q: %w", exchangeID, err) // DB outage: counted as a store error, never as a caller error
		return e, e
	}
	// pgx encodes a nil []string as SQL NULL; a patient-agnostic leg (DTR
	// $questionnaire-package carries Subjects == nil) must land as the empty array, exactly
	// as the in-memory store accepts it (mirror == oracle in both directions).
	subjects := rec.Subjects
	if subjects == nil {
		subjects = []string{}
	}
	// The row lock above serializes concurrent appends to the same exchange, so
	// MAX(seq)+1 is deterministic (and the leg primary key would reject a duplicate).
	if _, err := tx.Exec(ctx, `
INSERT INTO gw_exchange_leg (holder_id, exchange_id, seq, leg_type, correlation_id, subjects, kind, effect, timing, locality, outcome)
SELECT $1, $2, COALESCE(MAX(seq),0)+1, $3, $4, $5, $6, $7, $8, $9, $10
  FROM gw_exchange_leg WHERE holder_id=$1 AND exchange_id=$2`,
		s.holderID, exchangeID, rec.Type, rec.CorrelationID, subjects,
		rec.Physics.Kind, rec.Physics.Effect, rec.Physics.Timing, rec.Physics.Locality, rec.Outcome); err != nil {
		e := fmt.Errorf("ExchangeStore: append leg: %w", err)
		return e, e
	}
	if err := tx.Commit(ctx); err != nil {
		return err, err
	}
	return nil, nil
}

func (s *ExchangeStore) Get(exchangeID string) (*engine.Exchange, bool) {
	now := s.now()
	if !s.breakerAllows(now) {
		return nil, false // absent: exactly what a failed read reports (notFound), without the round trip
	}
	defer s.reportPanic(now)
	ex, ok, storeErr := s.get(exchangeID, now)
	s.breakerReport(now, storeErr)
	return ex, ok
}

// get is Get's round trip. Its third return is the error ONLY when the database failed —
// a genuinely absent exchange (pgx.ErrNoRows) is an answer, not an outage.
func (s *ExchangeStore) get(exchangeID string, now time.Time) (*engine.Exchange, bool, error) {
	ctx, cancel := storeCtx()
	defer cancel()
	ex := &engine.Exchange{ID: exchangeID, Legs: []engine.LegRecord{}} // non-nil, like the in-memory snapshot
	if err := s.pool.QueryRow(ctx, `SELECT workstream FROM gw_exchange WHERE holder_id=$1 AND exchange_id=$2 AND expires_at > $3`,
		s.holderID, exchangeID, now).Scan(&ex.Workstream); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}
		return nil, notFound("exchange get", err), err
	}
	rows, err := s.pool.Query(ctx, `SELECT leg_type, correlation_id, subjects, kind, effect, timing, locality, outcome
  FROM gw_exchange_leg WHERE holder_id=$1 AND exchange_id=$2 ORDER BY seq`, s.holderID, exchangeID)
	if err != nil {
		return nil, notFound("exchange legs", err), err
	}
	defer rows.Close()
	for rows.Next() {
		var l engine.LegRecord
		if err := rows.Scan(&l.Type, &l.CorrelationID, &l.Subjects, &l.Physics.Kind, &l.Physics.Effect, &l.Physics.Timing, &l.Physics.Locality, &l.Outcome); err != nil {
			return nil, notFound("exchange leg scan", err), err
		}
		if len(l.Subjects) == 0 {
			l.Subjects = nil // '{}' scans as an empty slice; the in-memory clone hands back nil — same shape both ways
		}
		ex.Legs = append(ex.Legs, l)
	}
	if err := rows.Err(); err != nil {
		return nil, notFound("exchange legs", err), err
	}
	return ex, true, nil
}

// Reset deletes every exchange (and, by cascade, every leg) of this holder. It is an
// ADMIN path (Gateway.Reset), not a request path, so the breaker neither gates it nor
// learns from it: the caller asked for this one call and gets its real error.
func (s *ExchangeStore) Reset() error {
	ctx, cancel := storeCtx()
	defer cancel()
	_, err := s.pool.Exec(ctx, `DELETE FROM gw_exchange WHERE holder_id=$1`, s.holderID)
	return err
}

// maybePurge sweeps this holder's expired exchanges at most once per
// exchangePurgeInterval. The sweep is an optimization only: Get and AppendLeg filter
// on expires_at themselves, so an unswept row is never readable.
func (s *ExchangeStore) maybePurge(now time.Time) {
	s.mu.Lock()
	due := now.Sub(s.lastPurge) >= exchangePurgeInterval
	s.mu.Unlock()
	if !due || !s.breakerAllows(now) {
		// Not due, or the database is being skipped: leave lastPurge alone so the sweep is
		// simply attempted again on the next Begin. The sweep is an optimization only.
		return
	}
	s.mu.Lock()
	s.lastPurge = now
	s.mu.Unlock()
	defer s.reportPanic(now)
	ctx, cancel := storeCtx()
	defer cancel()
	_, err := s.pool.Exec(ctx, `DELETE FROM gw_exchange WHERE holder_id=$1 AND expires_at <= $2`, s.holderID, now)
	s.breakerReport(now, err)
	if err != nil {
		log.Printf("pgstore: exchange purge: %v", err)
	}
}
