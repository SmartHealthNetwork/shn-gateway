// Package pgstore is the durable (Postgres) implementation of the gateway's
// Store seam — the holder's OWN business state: issued authorization numbers, the
// pended-claim ledger, and PA-decision EOBs (AI-1: metadata/decision only, never a
// cross-holder clinical record). It mirrors internal/auditstore (NewPgStore +
// EnsureSchema over pgxpool) and satisfies engine.Store (the public seam); the
// compile-time conformance assertion lives in-package (var _ engine.Store below).
//
// Bound to one holder: the Store interface carries no holder id, so (like
// holdersim.NewClient(url, holderID)) NewPgStore captures holderID and every query
// is scoped WHERE holder_id = $holderID — partitioning the gateways that share one
// DB, harmless-constant in the single-tenant partner case.
//
// Read-error seam: gateway.Store's read methods return (zero, bool) with no error
// channel, so a read here collapses any failure to "not found". A genuine DB outage
// would otherwise be indistinguishable from absence (e.g. the Patient Access API
// reporting "no EOBs" during a Postgres blip), so non-pgx.ErrNoRows read errors are
// LOGGED (notFound) to stay observable. The writes (which DO return error) are the
// durability-critical path. A future gateway.Store interface that carries read errors
// would close this seam (return the error instead of logging); out of scope for E-mid.
package pgstore

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
)

// PgStore is the public reference durable Store impl. The
// compile-time conformance assertion lives in-package now that pgstore is in the
// gateway module (connectors→engine is the only import direction; engine never
// imports connectors, so this is cycle-free).
var _ engine.Store = (*PgStore)(nil)

// notFound collapses a read error to false. A non-ErrNoRows error (e.g. a DB outage)
// is logged first so it is observable rather than indistinguishable from genuine
// absence (see the package doc's read-error seam note).
func notFound(method string, err error) bool {
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		log.Printf("pgstore: %s: %v", method, err)
	}
	return false
}

// PgStore is the Postgres-backed gateway.Store, bound to one holder.
type PgStore struct {
	pool     *pgxpool.Pool
	holderID string
}

// NewPgStore runs EnsureSchema (the fail-fast: pgxpool.New is lazy and does not
// connect, so this CREATE TABLE is what forces the connection) and returns the
// store bound to holderID.
func NewPgStore(ctx context.Context, pool *pgxpool.Pool, holderID string) (*PgStore, error) {
	if err := EnsureSchema(ctx, pool); err != nil {
		return nil, fmt.Errorf("pgstore: EnsureSchema: %w", err)
	}
	return &PgStore{pool: pool, holderID: holderID}, nil
}

// schemaLockKey serializes concurrent EnsureSchema calls (see below). Postgres
// advisory locks are CLUSTER-GLOBAL (not per-database), so a DISTINCT key is what
// keeps this from contending with another service's lock (e.g. auditstore's) — and
// even an accidental collision would only harmlessly serialize two unrelated
// schema-inits, never corrupt anything.
const schemaLockKey int64 = 0x676174657761795F // arbitrary distinct key ("gateway_" bytes)

// EnsureSchema creates the three tables if absent (idempotent; plain DDL, no
// CREATE ROLE — least-privilege-friendly). Safe to call repeatedly AND concurrently:
// `CREATE TABLE/INDEX IF NOT EXISTS` is NOT concurrency-safe on its own (concurrent
// callers race in pg_type/pg_class and one fails with SQLSTATE 23505), and the four
// gateways share one shn_gateway DB and all run EnsureSchema at startup — so the DDL
// runs inside a transaction holding a transaction-scoped advisory lock (the
// auditstore pattern). The first holder creates the tables; the rest then find them
// already present (no-op).
func EnsureSchema(ctx context.Context, pool *pgxpool.Pool) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS gw_auth_number (
    holder_id           TEXT NOT NULL,
    service_request_ref TEXT NOT NULL,
    pre_auth_ref        TEXT NOT NULL,
    PRIMARY KEY (holder_id, service_request_ref)
);
CREATE TABLE IF NOT EXISTS gw_pended_claim (
    holder_id      TEXT NOT NULL,
    subject_pci    TEXT NOT NULL,
    correlation_id TEXT NOT NULL,
    state          TEXT NOT NULL,
    PRIMARY KEY (holder_id, subject_pci, correlation_id)
);
CREATE TABLE IF NOT EXISTS gw_eob (
    holder_id   TEXT NOT NULL,
    eob_id      TEXT NOT NULL,
    subject_pci TEXT NOT NULL,
    eob_json    BYTEA NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (holder_id, eob_id)
);
CREATE INDEX IF NOT EXISTS gw_eob_by_patient ON gw_eob (holder_id, subject_pci, created_at);
`
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pgstore: begin schema tx: %w", err)
	}
	defer tx.Rollback(ctx) // no-op after a successful Commit
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, schemaLockKey); err != nil {
		return fmt.Errorf("pgstore: schema advisory lock: %w", err)
	}
	if _, err := tx.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("pgstore: create schema: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("pgstore: commit schema: %w", err)
	}
	return nil
}

// --- auth numbers (provider-side custody) ---

func (s *PgStore) StoreAuthNumber(serviceRequestRef, preAuthRef string) error {
	_, err := s.pool.Exec(context.Background(), `
INSERT INTO gw_auth_number (holder_id, service_request_ref, pre_auth_ref)
VALUES ($1, $2, $3)
ON CONFLICT (holder_id, service_request_ref) DO UPDATE SET pre_auth_ref = EXCLUDED.pre_auth_ref`,
		s.holderID, serviceRequestRef, preAuthRef)
	if err != nil {
		return fmt.Errorf("pgstore: StoreAuthNumber: %w", err)
	}
	return nil
}

func (s *PgStore) AuthNumber(serviceRequestRef string) (string, bool) {
	var ref string
	err := s.pool.QueryRow(context.Background(),
		`SELECT pre_auth_ref FROM gw_auth_number WHERE holder_id=$1 AND service_request_ref=$2`,
		s.holderID, serviceRequestRef).Scan(&ref)
	if err != nil {
		return "", notFound("AuthNumber", err)
	}
	return ref, true
}

// --- pended-claim ledger (payer-side state machine) ---

func (s *PgStore) RecordPendedClaim(subjectPCI, correlationID string) error {
	_, err := s.pool.Exec(context.Background(), `
INSERT INTO gw_pended_claim (holder_id, subject_pci, correlation_id, state)
VALUES ($1, $2, $3, 'pended')
ON CONFLICT (holder_id, subject_pci, correlation_id) DO UPDATE SET state = 'pended'`,
		s.holderID, subjectPCI, correlationID)
	if err != nil {
		return fmt.Errorf("pgstore: RecordPendedClaim: %w", err)
	}
	return nil
}

// BeginClaimUpdate is the ATOMIC test-and-set: a single conditional UPDATE. Under
// READ COMMITTED, concurrent UPDATEs on the same row serialize on the row lock and
// the loser re-evaluates its WHERE against the post-commit row (now 'in_progress')
// → 0 rows → false. Exactly one returns true — no app-level lock, correct across
// connections/replicas. Mirrors the stub's mutex test-and-set.
func (s *PgStore) BeginClaimUpdate(subjectPCI, correlationID string) (bool, error) {
	tag, err := s.pool.Exec(context.Background(), `
UPDATE gw_pended_claim SET state = 'in_progress'
WHERE holder_id=$1 AND subject_pci=$2 AND correlation_id=$3 AND state='pended'`,
		s.holderID, subjectPCI, correlationID)
	if err != nil {
		return false, fmt.Errorf("pgstore: BeginClaimUpdate: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *PgStore) ReleaseClaimUpdate(subjectPCI, correlationID string) error {
	_, err := s.pool.Exec(context.Background(), `
UPDATE gw_pended_claim SET state = 'pended'
WHERE holder_id=$1 AND subject_pci=$2 AND correlation_id=$3 AND state='in_progress'`,
		s.holderID, subjectPCI, correlationID)
	if err != nil {
		return fmt.Errorf("pgstore: ReleaseClaimUpdate: %w", err)
	}
	return nil
}

func (s *PgStore) FinalizeClaimUpdate(subjectPCI, correlationID string) error {
	_, err := s.pool.Exec(context.Background(),
		`DELETE FROM gw_pended_claim WHERE holder_id=$1 AND subject_pci=$2 AND correlation_id=$3`,
		s.holderID, subjectPCI, correlationID)
	if err != nil {
		return fmt.Errorf("pgstore: FinalizeClaimUpdate: %w", err)
	}
	return nil
}

// --- EOBs (Patient Access API surface) ---

func (s *PgStore) RecordEOB(subjectPCI, eobID string, eobJSON []byte) error {
	_, err := s.pool.Exec(context.Background(), `
INSERT INTO gw_eob (holder_id, eob_id, subject_pci, eob_json)
VALUES ($1, $2, $3, $4)
ON CONFLICT (holder_id, eob_id) DO UPDATE SET eob_json = EXCLUDED.eob_json`,
		s.holderID, eobID, subjectPCI, eobJSON)
	if err != nil {
		return fmt.Errorf("pgstore: RecordEOB: %w", err)
	}
	return nil
}

func (s *PgStore) EOBsForPatient(subjectPCI string) ([][]byte, bool) {
	// Ordered by created_at, eob_id — chronological, not the stub's slice-insertion order
	// (a second, benign divergence from StubHolderData: no caller asserts EOB order; the
	// Patient Access response is a searchset and the _id filter is order-independent).
	// eob_id is the deterministic tiebreaker when two EOBs share a created_at (same-
	// transaction inserts tie on now()).
	rows, err := s.pool.Query(context.Background(),
		`SELECT eob_json FROM gw_eob WHERE holder_id=$1 AND subject_pci=$2 ORDER BY created_at, eob_id`,
		s.holderID, subjectPCI)
	if err != nil {
		return nil, notFound("EOBsForPatient", err)
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, notFound("EOBsForPatient", err)
		}
		out = append(out, b) // pgx allocates a fresh slice per Scan → naturally a defensive copy
	}
	if err := rows.Err(); err != nil {
		return nil, notFound("EOBsForPatient", err)
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

func (s *PgStore) EOBByID(eobID string) ([]byte, bool) {
	var b []byte
	err := s.pool.QueryRow(context.Background(),
		`SELECT eob_json FROM gw_eob WHERE holder_id=$1 AND eob_id=$2`,
		s.holderID, eobID).Scan(&b)
	if err != nil {
		return nil, notFound("EOBByID", err)
	}
	return b, true
}
