// continuation.go — the durable half of the prior-authorization continuation
// store (engine.ContinuationStore).
//
// This is the shape the design calls the real one: a continuation written here is
// shared by every replica of the holder's gateway and survives a restart, so a
// decision a payer pended can be continued from wherever the next request lands.
// The in-memory store is the disclosed fallback for a deployment with no
// Postgres, not a stand-in for this.
//
// AI-1 holds, and is enforced rather than asserted. Every column is scalar or a
// TEXT[] of scalars: identity, routing, the member, the identifiers the payer
// answered with, and one row per submitted line carrying its sequence, product
// code, service date and trace number. There is NO opaque column — no BYTEA, no
// JSON — so ddl_fence_test.go's exception list needs no entry for these tables,
// and TestContinuation_NoClinicalColumns says so directly.
//
// An id minted here carries the DURABLE mark: nothing written here is lost to a
// restart, so a continuation this store does not have is unknown, never lost.
package pgstore

import (
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
)

// The durable store ships the continuation store.
var _ engine.ContinuationStore = (*PgStore)(nil)

// continuationRowSQL reads one continuation by its binding.
const continuationRowSQL = `
SELECT continuation_id, payer_holder, line, correlation_id, subject_pci, member_id,
       sor_patient_id, order_ref, provider_npi, claim_identifier, claim_type, claim_priority,
       item_trace_numbers, payer_claimresponse_ids, payer_preauth_ref, last_outcome, created_at, updated_at
  FROM gw_pa_continuation
 WHERE holder_id=$1 AND continuation_id=$2`

// PutContinuation writes a continuation and its item map in ONE transaction: a
// continuation is never half-written, so an inquiry can never be built from a row
// whose lines are still the previous submission's.
//
// See engine.ContinuationStore. An empty id mints one under the durable mark.
func (s *PgStore) PutContinuation(c engine.Continuation) (engine.Continuation, error) {
	if err := engine.ValidateContinuation(c); err != nil {
		return engine.Continuation{}, err
	}
	s.maybePurgeContinuations()
	ctx, cancel := storeCtx()
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return engine.Continuation{}, fmt.Errorf("pgstore: PutContinuation: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	// Truncated to what the column holds. `timestamptz` keeps microseconds, so a
	// Go instant with nanoseconds would be stored as one value and returned to the
	// caller as another — the struct would disagree with its own row from the moment
	// it was written, and every later comparison against a read-back would fail for
	// a difference the store invented rather than a change anyone made.
	now := s.now().Truncate(time.Microsecond)
	if c.ID == "" {
		c.ID = engine.NewContinuationID(engine.ContinuationDurableMark)
		c.CreatedAt = now
	} else {
		// The row's own CREATED date is kept: an update states what the payer
		// has said since, not a new beginning. Read under the row lock so a
		// concurrent update on another replica cannot interleave between the
		// read and the write.
		var created time.Time
		err := tx.QueryRow(ctx,
			`SELECT created_at FROM gw_pa_continuation WHERE holder_id=$1 AND continuation_id=$2 FOR UPDATE`,
			c.Holder, c.ID).Scan(&created)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if c.CreatedAt.IsZero() {
				c.CreatedAt = now
			}
		case err != nil:
			return engine.Continuation{}, fmt.Errorf("pgstore: PutContinuation: %w", err)
		default:
			c.CreatedAt = created
		}
	}
	c.UpdatedAt = now
	if _, err := tx.Exec(ctx, `
INSERT INTO gw_pa_continuation (holder_id, continuation_id, payer_holder, line, correlation_id,
    subject_pci, member_id, sor_patient_id, order_ref, provider_npi, claim_identifier,
    claim_type, claim_priority, item_trace_numbers, payer_claimresponse_ids, payer_preauth_ref,
    last_outcome, created_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
ON CONFLICT (holder_id, continuation_id) DO UPDATE
  SET payer_holder = EXCLUDED.payer_holder,
      line = EXCLUDED.line,
      correlation_id = EXCLUDED.correlation_id,
      subject_pci = EXCLUDED.subject_pci,
      member_id = EXCLUDED.member_id,
      sor_patient_id = EXCLUDED.sor_patient_id,
      order_ref = EXCLUDED.order_ref,
      provider_npi = EXCLUDED.provider_npi,
      claim_identifier = EXCLUDED.claim_identifier,
      claim_type = EXCLUDED.claim_type,
      claim_priority = EXCLUDED.claim_priority,
      item_trace_numbers = EXCLUDED.item_trace_numbers,
      payer_claimresponse_ids = EXCLUDED.payer_claimresponse_ids,
      payer_preauth_ref = EXCLUDED.payer_preauth_ref,
      last_outcome = EXCLUDED.last_outcome,
      updated_at = EXCLUDED.updated_at`,
		c.Holder, c.ID, c.PayerHolder, c.Line, c.CorrID, c.SubjectPCI, c.MemberID,
		c.SoRPatientID, c.OrderRef, c.ProviderNPI, c.ClaimIdentifier, c.ClaimType, c.ClaimPriority,
		textArray(c.ItemTraceNumbers), textArray(c.PayerClaimResponseIDs),
		c.PayerPreAuthRef, c.LastOutcome, c.CreatedAt, c.UpdatedAt); err != nil {
		return engine.Continuation{}, fmt.Errorf("pgstore: PutContinuation: %w", err)
	}
	// The item map is REPLACED, not merged: it states what one submission asked
	// for, and a merge would leave a line from an earlier submission standing
	// beside the current one with nothing to say which is which.
	if _, err := tx.Exec(ctx,
		`DELETE FROM gw_pa_continuation_item WHERE holder_id=$1 AND continuation_id=$2`,
		c.Holder, c.ID); err != nil {
		return engine.Continuation{}, fmt.Errorf("pgstore: PutContinuation: items: %w", err)
	}
	for _, it := range c.Items {
		if _, err := tx.Exec(ctx, `
INSERT INTO gw_pa_continuation_item (holder_id, continuation_id, sequence, product_code, product_display,
    service_date, trace_number, authorization_number, administration_reference_number)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			c.Holder, c.ID, it.Sequence, it.ProductCode, it.ProductDisplay, it.ServiceDate, it.TraceNumber,
			it.AuthorizationNumber, it.AdministrationReferenceNumber); err != nil {
			return engine.Continuation{}, fmt.Errorf("pgstore: PutContinuation: item %d: %w", it.Sequence, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return engine.Continuation{}, fmt.Errorf("pgstore: PutContinuation: commit: %w", err)
	}
	c.Items = engine.SortedContinuationItems(c.Items)
	return c, nil
}

// ReadContinuation resolves {holder, id}. See engine.ContinuationStore.
//
// A row this store does not have is UNKNOWN. There is no "lost" verdict here by
// construction: a durable store keeps what it minted, so the only way an id names
// nothing is that this holder never minted it or its retention ran out.
func (s *PgStore) ReadContinuation(holder, id string) (engine.Continuation, engine.ContinuationLookup, error) {
	if holder == "" {
		return engine.Continuation{}, engine.ContinuationUnknown, engine.ErrContinuationHolderRequired
	}
	ctx, cancel := storeCtx()
	defer cancel()
	c := engine.Continuation{Holder: holder}
	err := s.pool.QueryRow(ctx, continuationRowSQL, holder, id).Scan(
		&c.ID, &c.PayerHolder, &c.Line, &c.CorrID, &c.SubjectPCI, &c.MemberID,
		&c.SoRPatientID, &c.OrderRef, &c.ProviderNPI, &c.ClaimIdentifier, &c.ClaimType, &c.ClaimPriority,
		&c.ItemTraceNumbers, &c.PayerClaimResponseIDs, &c.PayerPreAuthRef,
		&c.LastOutcome, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return engine.Continuation{}, engine.ContinuationUnknown, nil
	}
	if err != nil {
		return engine.Continuation{}, engine.ContinuationUnknown, fmt.Errorf("pgstore: ReadContinuation: %w", err)
	}
	rows, err := s.pool.Query(ctx, `
SELECT sequence, product_code, product_display, service_date, trace_number, authorization_number, administration_reference_number
  FROM gw_pa_continuation_item
 WHERE holder_id=$1 AND continuation_id=$2
 ORDER BY sequence`, holder, id)
	if err != nil {
		return engine.Continuation{}, engine.ContinuationUnknown, fmt.Errorf("pgstore: ReadContinuation: items: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var it engine.ContinuationItem
		if err := rows.Scan(&it.Sequence, &it.ProductCode, &it.ProductDisplay, &it.ServiceDate, &it.TraceNumber,
			&it.AuthorizationNumber, &it.AdministrationReferenceNumber); err != nil {
			return engine.Continuation{}, engine.ContinuationUnknown, fmt.Errorf("pgstore: ReadContinuation: items: %w", err)
		}
		c.Items = append(c.Items, it)
	}
	if err := rows.Err(); err != nil {
		return engine.Continuation{}, engine.ContinuationUnknown, fmt.Errorf("pgstore: ReadContinuation: items: %w", err)
	}
	return c, engine.ContinuationFound, nil
}

// Durable reports that a continuation written here survives a restart.
func (s *PgStore) Durable() bool { return true }

// textArray normalizes a nil slice to an empty one: the columns are NOT NULL, and
// a NULL array and an empty one would otherwise read back differently for the same
// "this submission carried none".
func textArray(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// maybePurgeContinuations sweeps this holder's continuations whose last update is
// older than engine.ContinuationRetention, on the same throttle and the same
// per-call bound the pend ledger's sweep uses. The item rows go with the row they
// belong to through ON DELETE CASCADE.
//
// Best-effort, like the ledger's: a failure is logged, never returned, because
// nothing a requester asked for depends on it.
func (s *PgStore) maybePurgeContinuations() {
	now := s.now()
	s.mu.Lock()
	due, cutoff := engine.DuePendPurge(now, s.lastContinuationPurge)
	if due {
		s.lastContinuationPurge = now
	}
	s.mu.Unlock()
	if !due {
		return
	}
	ctx, cancel := storeCtx()
	defer cancel()
	if _, err := s.pool.Exec(ctx, `
DELETE FROM gw_pa_continuation
 WHERE ctid IN (
   SELECT ctid FROM gw_pa_continuation
    WHERE holder_id=$1 AND updated_at <= $2
    LIMIT $3)`, s.holderID, cutoff, engine.PendPurgeMax); err != nil {
		log.Printf("pgstore: continuation purge: %v", err)
	}
}
