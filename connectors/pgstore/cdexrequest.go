//go:build !shn_coreonly

// cdexrequest.go — the durable half of the CDex request record
// (engine.CDexRequestStore): what pairs a CDex attachment submission with the
// attachment request it answers.
//
// When a gateway carries an attachment request it records the request's
// tracking identifier, scoped to the other participant, with the member and the
// patient identifier it bound the request to; a submission from that
// participant naming the same tracking identifier binds by that record. Written
// here, the record is shared by every replica of the holder's gateway and
// survives a restart, so a submission landing anywhere still pairs. The
// in-memory record is the fallback for a deployment with no Postgres.
//
// It answers exactly as engine.MemCDexRequests does: a key recorded again with
// the same record changes nothing (its retention is not refreshed); with a
// different one it is marked conflicted, keeping its first record, never
// overwritten; a record past engine.CDexRequestRetention is never found and is
// recorded afresh. The record-or-conflict decision is ONE statement (INSERT …
// ON CONFLICT … DO UPDATE … WHERE), so two replicas racing on one key cannot
// overwrite each other: the second sees the first's row under its lock.
//
// AI-1 holds: opaque identifiers only (the other participant, the tracking
// identifier, the member and the patient identifier), a flag and the store's own
// time. No clinical content, and no JSON or BYTEA column, which is what keeps
// ddl_fence_test.go's exception list empty.
package pgstore

import (
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
)

// The durable store ships the CDex request record.
var _ engine.CDexRequestStore = (*PgStore)(nil)

// recordCDexRequestSQL writes k → rec in one statement. $7 is now, $8 the
// retention cutoff (now - engine.CDexRequestRetention).
//
// The DO UPDATE runs only when the existing row is past retention (it is then
// replaced, as a fresh record) or is live, not yet conflicted, and holds a
// different member or patient (it is then marked conflicted at now, its member
// and patient kept). A live conflicted row, or one holding the same record, is
// left as it is, time included.
const recordCDexRequestSQL = `
INSERT INTO gw_cdex_request (holder_id, counterparty, tracking_system, tracking_value,
    member_id, subject_pci, conflicted, changed_at)
VALUES ($1, $2, $3, $4, $5, $6, false, $7)
ON CONFLICT (holder_id, counterparty, tracking_system, tracking_value) DO UPDATE
  SET member_id   = CASE WHEN gw_cdex_request.changed_at <= $8
                         THEN EXCLUDED.member_id ELSE gw_cdex_request.member_id END,
      subject_pci = CASE WHEN gw_cdex_request.changed_at <= $8
                         THEN EXCLUDED.subject_pci ELSE gw_cdex_request.subject_pci END,
      conflicted  = gw_cdex_request.changed_at > $8,
      changed_at  = EXCLUDED.changed_at
  WHERE gw_cdex_request.changed_at <= $8
     OR (NOT gw_cdex_request.conflicted
         AND (gw_cdex_request.member_id <> EXCLUDED.member_id
              OR gw_cdex_request.subject_pci <> EXCLUDED.subject_pci))`

// RecordCDexRequest records k → rec. See engine.CDexRequestStore.
func (s *PgStore) RecordCDexRequest(k engine.CDexRequestKey, rec engine.CDexRequestRecord) error {
	if !engine.ValidCDexRequestKey(k) {
		return engine.ErrCDexRequestKey
	}
	s.maybePurgeCDexRequests()
	ctx, cancel := storeCtx()
	defer cancel()
	// Truncated to what the column holds (see PutContinuation), so retention is
	// cut against the instant the row actually stores.
	now := s.now().Truncate(time.Microsecond)
	if _, err := s.pool.Exec(ctx, recordCDexRequestSQL,
		s.holderID, k.Counterparty, k.System, k.Value, rec.Member, rec.PCI,
		now, now.Add(-engine.CDexRequestRetention)); err != nil {
		return fmt.Errorf("pgstore: RecordCDexRequest: %w", err)
	}
	return nil
}

// LookupCDexRequest resolves k. See engine.CDexRequestStore. Only a row changed
// inside retention counts; a store failure is returned, never read as none.
func (s *PgStore) LookupCDexRequest(k engine.CDexRequestKey) (engine.CDexRequestRecord, engine.CDexRequestLookup, error) {
	if !engine.ValidCDexRequestKey(k) {
		return engine.CDexRequestRecord{}, engine.CDexRequestNone, engine.ErrCDexRequestKey
	}
	s.maybePurgeCDexRequests()
	ctx, cancel := storeCtx()
	defer cancel()
	cutoff := s.now().Truncate(time.Microsecond).Add(-engine.CDexRequestRetention)
	var (
		rec        engine.CDexRequestRecord
		conflicted bool
	)
	err := s.pool.QueryRow(ctx, `
SELECT member_id, subject_pci, conflicted FROM gw_cdex_request
 WHERE holder_id=$1 AND counterparty=$2 AND tracking_system=$3 AND tracking_value=$4
   AND changed_at > $5`,
		s.holderID, k.Counterparty, k.System, k.Value, cutoff).Scan(&rec.Member, &rec.PCI, &conflicted)
	switch {
	case err == nil && conflicted:
		return engine.CDexRequestRecord{}, engine.CDexRequestConflicted, nil
	case err == nil:
		return rec, engine.CDexRequestFound, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return engine.CDexRequestRecord{}, engine.CDexRequestNone, fmt.Errorf("pgstore: LookupCDexRequest: %w", err)
	}
	var other bool
	if err := s.pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM gw_cdex_request
                WHERE holder_id=$1 AND tracking_system=$2 AND tracking_value=$3
                  AND changed_at > $4)`,
		s.holderID, k.System, k.Value, cutoff).Scan(&other); err != nil {
		return engine.CDexRequestRecord{}, engine.CDexRequestNone, fmt.Errorf("pgstore: LookupCDexRequest: %w", err)
	}
	if other {
		return engine.CDexRequestRecord{}, engine.CDexRequestOtherCounterparty, nil
	}
	return engine.CDexRequestRecord{}, engine.CDexRequestNone, nil
}

// maybePurgeCDexRequests sweeps this holder's request records last changed
// longer than engine.CDexRequestRetention ago, on the same throttle and the same
// per-call bound the pend ledger's and the continuation store's sweeps use, so
// the two backends' observable retention is one rule.
//
// Best-effort, like theirs: a failure is logged, never returned. A row the
// sweep has not reached yet is still never found: both methods cut on
// retention themselves.
func (s *PgStore) maybePurgeCDexRequests() {
	now := s.now()
	s.mu.Lock()
	due, _ := engine.DuePendPurge(now, s.lastCDexRequestPurge)
	if due {
		s.lastCDexRequestPurge = now
	}
	s.mu.Unlock()
	if !due {
		return
	}
	ctx, cancel := storeCtx()
	defer cancel()
	if _, err := s.pool.Exec(ctx, `
DELETE FROM gw_cdex_request
 WHERE ctid IN (
   SELECT ctid FROM gw_cdex_request
    WHERE holder_id=$1 AND changed_at <= $2
    LIMIT $3)`, s.holderID, now.Add(-engine.CDexRequestRetention), engine.PendPurgeMax); err != nil {
		log.Printf("pgstore: cdex request purge: %v", err)
	}
}
