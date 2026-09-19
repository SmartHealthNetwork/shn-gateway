// pendledger.go — the durable half of the payer pend ledger (engine.PendLedger):
// the keyed lookup, the absorbing decided state, the atomic decision-plus-EOB write
// and the bounded six-month retention sweep.
//
// It runs the SAME pure state-machine helpers the in-memory store runs
// (engine.PendBegin / PendRePend / PendDecide), so the two backends cannot drift:
// what differs here is only WHERE the row is read and written — inside a
// transaction, behind a row lock (SELECT … FOR UPDATE), which is how two gateway
// replicas sharing one database serialize on the same authorization.
//
// AI-1 holds: state, outcome, dates, the requester holder and identifier keys. No
// clinical content, and no JSON column (the keys live in gw_pended_claim_key as
// scalar kind/value rows, which is what keeps ddl_fence_test.go's exception list
// empty).
package pgstore

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
)

// The durable store ships the optional ledger.
var _ engine.PendLedger = (*PgStore)(nil)

// pendRowSQL reads one ledger row for update. The nullable ledger columns are the
// pre-upgrade / keyless-pend case (see the DDL), so they are read through
// null-tolerant destinations.
const pendRowSQL = `
SELECT state, COALESCE(requester_holder, ''), COALESCE(outcome, ''), decided_at
  FROM gw_pended_claim
 WHERE holder_id=$1 AND subject_pci=$2 AND correlation_id=$3`

// readPendRow reads the row inside tx, taking its row lock so a concurrent leg
// working the same authorization waits rather than racing. found=false when there
// is no such row.
func readPendRow(ctx context.Context, tx pgx.Tx, holderID, subjectPCI, corrID string) (engine.PendRecord, bool, error) {
	var (
		rec       engine.PendRecord
		state     string
		decidedAt *time.Time
	)
	err := tx.QueryRow(ctx, pendRowSQL+` FOR UPDATE`, holderID, subjectPCI, corrID).
		Scan(&state, &rec.RequesterHolder, &rec.Outcome, &decidedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return engine.PendRecord{}, false, nil
	}
	if err != nil {
		return engine.PendRecord{}, false, err
	}
	rec.State = engine.PendState(state)
	if decidedAt != nil {
		rec.DecidedAt = *decidedAt
	}
	return rec, true, nil
}

// writePendRow upserts the row the state machine produced.
func writePendRow(ctx context.Context, tx pgx.Tx, holderID, subjectPCI, corrID string, rec engine.PendRecord) error {
	var decidedAt *time.Time
	if !rec.DecidedAt.IsZero() {
		at := rec.DecidedAt
		decidedAt = &at
	}
	var outcome, requester *string
	if rec.Outcome != "" {
		o := rec.Outcome
		outcome = &o
	}
	if rec.RequesterHolder != "" {
		r := rec.RequesterHolder
		requester = &r
	}
	_, err := tx.Exec(ctx, `
INSERT INTO gw_pended_claim (holder_id, subject_pci, correlation_id, state, outcome, decided_at, requester_holder, last_transition_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (holder_id, subject_pci, correlation_id) DO UPDATE
  SET state = EXCLUDED.state,
      outcome = EXCLUDED.outcome,
      decided_at = EXCLUDED.decided_at,
      requester_holder = EXCLUDED.requester_holder,
      last_transition_at = EXCLUDED.last_transition_at`,
		holderID, subjectPCI, corrID, string(rec.State), outcome, decidedAt, requester, rec.LastTransition)
	return err
}

// BeginClaimUpdateReason is the ledger-aware test-and-set: it takes the row lock,
// asks the shared state machine whether the claim may be claimed, and reports why
// not when it may not. Concurrent callers serialize on the row lock and the loser
// re-reads the committed row (now 'in_progress') → refused with
// PendRefusalInProgress. Exactly one wins, across connections and replicas.
func (s *PgStore) BeginClaimUpdateReason(subjectPCI, corrID string) (bool, engine.PendRefusal, error) {
	ctx, cancel := storeCtx()
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, engine.PendRefusalNone, fmt.Errorf("pgstore: BeginClaimUpdate: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	cur, found, err := readPendRow(ctx, tx, s.holderID, subjectPCI, corrID)
	if err != nil {
		return false, engine.PendRefusalNone, fmt.Errorf("pgstore: BeginClaimUpdate: %w", err)
	}
	next, ok, why := engine.PendBegin(cur.State, found)
	if !ok {
		return false, why, nil
	}
	cur.State = next
	cur.LastTransition = s.now()
	if err := writePendRow(ctx, tx, s.holderID, subjectPCI, corrID, cur); err != nil {
		return false, engine.PendRefusalNone, fmt.Errorf("pgstore: BeginClaimUpdate: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, engine.PendRefusalNone, fmt.Errorf("pgstore: BeginClaimUpdate: commit: %w", err)
	}
	return true, engine.PendRefusalNone, nil
}

// RecordPendedKeyed records (or re-pends) an authorization and indexes its lookup
// keys. See engine.PendLedger. The row and its keys are written in ONE transaction:
// a pend is never half-indexed.
func (s *PgStore) RecordPendedKeyed(subjectPCI, corrID string, created time.Time, k engine.PendKeys) (engine.PendTransition, error) {
	refs, err := engine.ValidatePendKeys(k)
	if err != nil {
		return engine.PendTransition{}, err
	}
	s.maybePurge()
	ctx, cancel := storeCtx()
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return engine.PendTransition{}, fmt.Errorf("pgstore: RecordPendedKeyed: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	cur, found, err := readPendRow(ctx, tx, s.holderID, subjectPCI, corrID)
	if err != nil {
		return engine.PendTransition{}, fmt.Errorf("pgstore: RecordPendedKeyed: %w", err)
	}
	if found && cur.RequesterHolder != "" && cur.RequesterHolder != k.RequesterHolder {
		return engine.PendTransition{}, engine.ErrPendRequesterMismatch
	}
	next, tr := engine.PendRePend(cur, found, created)
	next.RequesterHolder = k.RequesterHolder
	next.LastTransition = s.now()
	if err := writePendRow(ctx, tx, s.holderID, subjectPCI, corrID, next); err != nil {
		return engine.PendTransition{}, fmt.Errorf("pgstore: RecordPendedKeyed: %w", err)
	}
	for _, ref := range refs {
		// DO NOTHING is the union: a key this response repeats keeps identifying
		// the same authorization, and a re-pend never drops the earlier keys.
		if _, err := tx.Exec(ctx, `
INSERT INTO gw_pended_claim_key (holder_id, requester_holder, kind, key, subject_pci, correlation_id)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT DO NOTHING`,
			s.holderID, k.RequesterHolder, ref.Kind, ref.Key, subjectPCI, corrID); err != nil {
			return engine.PendTransition{}, fmt.Errorf("pgstore: RecordPendedKeyed: key %s: %w", ref.Kind, err)
		}
	}
	// The UNION the authorization now has, not this response's contribution: a
	// re-pend whose response carries no identifier leaves an authorization that is
	// still perfectly findable by the keys an earlier response gave, and reporting
	// zero for it would raise "nothing to match on" about a claim that has plenty.
	// Counted INSIDE the transaction, so the number is the one that landed and not
	// whatever a concurrent leg had left behind by the time anybody re-read it.
	if err := tx.QueryRow(ctx, `
SELECT count(*) FROM gw_pended_claim_key
 WHERE holder_id=$1 AND subject_pci=$2 AND correlation_id=$3`,
		s.holderID, subjectPCI, corrID).Scan(&tr.Keys); err != nil {
		return engine.PendTransition{}, fmt.Errorf("pgstore: RecordPendedKeyed: key count: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return engine.PendTransition{}, fmt.Errorf("pgstore: RecordPendedKeyed: commit: %w", err)
	}
	return tr, nil
}

// LookupPended resolves a follow-up's keys within one requester's namespace. See
// engine.PendLedger. The probe is capped at two rows: one is the answer, two is
// ambiguous, and nothing beyond that changes either verdict.
func (s *PgStore) LookupPended(requesterHolder string, k engine.PendKeys) (string, string, bool, bool, error) {
	if requesterHolder == "" {
		return "", "", false, false, engine.ErrPendRequesterRequired
	}
	probes := pendProbes(k)
	if len(probes.kinds) == 0 {
		return "", "", false, false, nil
	}
	ctx, cancel := storeCtx()
	defer cancel()
	rows, err := s.pool.Query(ctx, `
SELECT DISTINCT subject_pci, correlation_id
  FROM gw_pended_claim_key
 WHERE holder_id=$1 AND requester_holder=$2
   AND (kind, key) IN (SELECT * FROM unnest($3::text[], $4::text[]))
 LIMIT 2`, s.holderID, requesterHolder, probes.kinds, probes.keys)
	if err != nil {
		return "", "", false, false, fmt.Errorf("pgstore: LookupPended: %w", err)
	}
	defer rows.Close()
	var found [][2]string
	for rows.Next() {
		var subject, corr string
		if err := rows.Scan(&subject, &corr); err != nil {
			return "", "", false, false, fmt.Errorf("pgstore: LookupPended: %w", err)
		}
		found = append(found, [2]string{subject, corr})
	}
	if err := rows.Err(); err != nil {
		return "", "", false, false, fmt.Errorf("pgstore: LookupPended: %w", err)
	}
	switch len(found) {
	case 0:
		return "", "", false, false, nil
	case 1:
		return found[0][0], found[0][1], true, false, nil
	default:
		return "", "", false, true, nil
	}
}

// pendProbes flattens the probe keys into the two parallel arrays the lookup's
// unnest takes.
func pendProbes(k engine.PendKeys) struct{ kinds, keys []string } {
	var out struct{ kinds, keys []string }
	// A key the record side would have refused (oversized) can never be stored, so
	// LookupPended drops it rather than failing the follow-up — engine.PendKeys
	// does that filtering for both backends.
	for _, ref := range engine.ProbePendKeys(k) {
		out.kinds = append(out.kinds, ref.Kind)
		out.keys = append(out.keys, ref.Key)
	}
	return out
}

// RecordDecision records the payer's terminal answer and its EOB as one write. See
// engine.PendLedger.
//
// The decision and the EOB are ONE transaction: if the EOB write fails, the
// decision rolls back with it, so the ledger never says "decided" about an
// authorization whose EOB is missing.
func (s *PgStore) RecordDecision(subjectPCI, corrID, outcome string, decidedAt time.Time, eob *engine.EOBRecord) (engine.PendTransition, error) {
	if err := engine.ValidatePendDecision(outcome); err != nil {
		return engine.PendTransition{}, err
	}
	if err := eob.Validate(subjectPCI); err != nil {
		return engine.PendTransition{}, err
	}
	ctx, cancel := storeCtx()
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return engine.PendTransition{}, fmt.Errorf("pgstore: RecordDecision: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	cur, found, err := readPendRow(ctx, tx, s.holderID, subjectPCI, corrID)
	if err != nil {
		return engine.PendTransition{}, fmt.Errorf("pgstore: RecordDecision: %w", err)
	}
	next, tr := engine.PendDecide(cur, found, outcome, decidedAt)
	next.RequesterHolder = cur.RequesterHolder
	next.LastTransition = s.now()
	if err := writePendRow(ctx, tx, s.holderID, subjectPCI, corrID, next); err != nil {
		return engine.PendTransition{}, fmt.Errorf("pgstore: RecordDecision: %w", err)
	}
	if eob != nil {
		if _, err := tx.Exec(ctx, `
INSERT INTO gw_eob (holder_id, eob_id, subject_pci, eob_json)
VALUES ($1, $2, $3, $4)
ON CONFLICT (holder_id, eob_id) DO UPDATE SET eob_json = EXCLUDED.eob_json`,
			s.holderID, eob.EOBID, eob.SubjectPCI, eob.JSON); err != nil {
			return engine.PendTransition{}, fmt.Errorf("pgstore: RecordDecision: EOB: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return engine.PendTransition{}, fmt.Errorf("pgstore: RecordDecision: commit: %w", err)
	}
	return tr, nil
}

// PendRecordOf reads one ledger row. See engine.PendLedger. Unlike Store's
// read methods it RETURNS the error: "no such authorization" and "the database is
// unavailable" answer a follow-up differently, so they must stay distinguishable.
func (s *PgStore) PendRecordOf(subjectPCI, corrID string) (engine.PendRecord, bool, error) {
	ctx, cancel := storeCtx()
	defer cancel()
	var (
		rec        engine.PendRecord
		state      string
		decidedAt  *time.Time
		lastChange time.Time
	)
	err := s.pool.QueryRow(ctx, `
SELECT state, COALESCE(requester_holder, ''), COALESCE(outcome, ''), decided_at, last_transition_at
  FROM gw_pended_claim
 WHERE holder_id=$1 AND subject_pci=$2 AND correlation_id=$3`,
		s.holderID, subjectPCI, corrID).
		Scan(&state, &rec.RequesterHolder, &rec.Outcome, &decidedAt, &lastChange)
	if errors.Is(err, pgx.ErrNoRows) {
		return engine.PendRecord{}, false, nil
	}
	if err != nil {
		return engine.PendRecord{}, false, fmt.Errorf("pgstore: PendRecordOf: %w", err)
	}
	rec.State = engine.PendState(state)
	if decidedAt != nil {
		rec.DecidedAt = *decidedAt
	}
	rec.LastTransition = lastChange
	return rec, true, nil
}

// maybePurge sweeps this holder's ledger rows whose last transition is older than
// engine.PendLedgerRetention, at most once per engine.PendPurgeInterval and at most
// engine.PendPurgeMax rows per call. The ctid subquery is what bounds it: an
// unbounded DELETE on a large backlog would hold locks for as long as the backlog
// is big, on a request path. A backlog is worked off over many calls instead.
//
// Like the exchange store's sweep this is best-effort: a failure is logged, never
// returned, because nothing a requester asked for depends on it.
func (s *PgStore) maybePurge() {
	now := s.now()
	s.mu.Lock()
	due, cutoff := engine.DuePendPurge(now, s.lastPurge)
	if due {
		s.lastPurge = now
	}
	s.mu.Unlock()
	if !due {
		return
	}
	ctx, cancel := storeCtx()
	defer cancel()
	if _, err := s.pool.Exec(ctx, `
DELETE FROM gw_pended_claim
 WHERE ctid IN (
   SELECT ctid FROM gw_pended_claim
    WHERE holder_id=$1 AND last_transition_at <= $2
    LIMIT $3)`, s.holderID, cutoff, engine.PendPurgeMax); err != nil {
		log.Printf("pgstore: pend ledger purge: %v", err)
	}
}
