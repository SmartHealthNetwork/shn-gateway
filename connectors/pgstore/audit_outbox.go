//go:build !shn_coreonly

package pgstore

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SmartHealthNetwork/shn-gateway/internal/auditappend"
)

// AuditOutbox is the durable audit outbox (gw_audit_outbox), one holder's
// rows in a store database several gateways may share: its queued records
// survive a restart, and replicas sharing it never send one row at once (a
// claim holds the row for its lease).
type AuditOutbox struct {
	pool     *pgxpool.Pool
	holderID string
}

var _ auditappend.Store = (*AuditOutbox)(nil)

// NewAuditOutbox returns holderID's outbox over pool, whose schema
// EnsureSchema (NewPgStore) created.
func NewAuditOutbox(pool *pgxpool.Pool, holderID string) *AuditOutbox {
	return &AuditOutbox{pool: pool, holderID: holderID}
}

// Enqueue inserts the record and deletes this holder's oldest rows beyond
// bound in one transaction. A record already queued under its recordId is
// not queued twice.
func (o *AuditOutbox) Enqueue(ctx context.Context, e auditappend.Entry, now time.Time, bound int) (int, error) {
	if len(e.Body) > auditappend.MaxBody {
		return 0, errors.New("pgstore: audit record over its bound")
	}
	tx, err := o.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) // no-op after a successful Commit
	if _, err := tx.Exec(ctx, `
INSERT INTO gw_audit_outbox (holder_id, record_id, body, enqueued_at, next_attempt_at)
VALUES ($1, $2, $3, $4, $4)
ON CONFLICT (holder_id, record_id) DO NOTHING`,
		o.holderID, e.RecordID, e.Body, now); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `
DELETE FROM gw_audit_outbox
WHERE holder_id = $1 AND seq IN (
    SELECT seq FROM gw_audit_outbox WHERE holder_id = $1 ORDER BY seq DESC OFFSET $2)`,
		o.holderID, bound)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// Claim takes this holder's row due earliest (the oldest among equals) and
// holds it until now+lease.
// SKIP LOCKED keeps two replicas from claiming one row together.
func (o *AuditOutbox) Claim(ctx context.Context, now time.Time, lease time.Duration) (auditappend.Entry, bool, error) {
	var e auditappend.Entry
	err := o.pool.QueryRow(ctx, `
UPDATE gw_audit_outbox SET next_attempt_at = $3
WHERE holder_id = $1 AND seq = (
    SELECT seq FROM gw_audit_outbox
    WHERE holder_id = $1 AND next_attempt_at <= $2
    ORDER BY next_attempt_at, seq LIMIT 1 FOR UPDATE SKIP LOCKED)
RETURNING seq, record_id, body, attempts`,
		o.holderID, now, now.Add(lease)).Scan(&e.Seq, &e.RecordID, &e.Body, &e.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return auditappend.Entry{}, false, nil
	}
	if err != nil {
		return auditappend.Entry{}, false, err
	}
	return e, true, nil
}

// Delete removes a row, or answers auditappend.ErrGone when it is no longer
// there (overflow took it while it was being sent).
func (o *AuditOutbox) Delete(ctx context.Context, seq int64) error {
	tag, err := o.pool.Exec(ctx, `DELETE FROM gw_audit_outbox WHERE holder_id = $1 AND seq = $2`, o.holderID, seq)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return auditappend.ErrGone
	}
	return nil
}

// Defer makes a row due at next, counting the attempt and its reason.
func (o *AuditOutbox) Defer(ctx context.Context, seq int64, next time.Time, status string) error {
	_, err := o.pool.Exec(ctx, `
UPDATE gw_audit_outbox SET next_attempt_at = $3, attempts = attempts + 1, last_status = left($4, 32)
WHERE holder_id = $1 AND seq = $2`, o.holderID, seq, next, status)
	return err
}

// Depth counts this holder's queued rows.
func (o *AuditOutbox) Depth(ctx context.Context) (int, error) {
	var n int
	err := o.pool.QueryRow(ctx, `SELECT count(*) FROM gw_audit_outbox WHERE holder_id = $1`, o.holderID).Scan(&n)
	return n, err
}

// Durable is true: the rows survive a restart.
func (o *AuditOutbox) Durable() bool { return true }
