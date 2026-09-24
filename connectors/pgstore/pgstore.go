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
	"sync"
	"time"

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
	// now is the store's own clock: it stamps every ledger transition and cuts the
	// retention window. Injected so the retention rows are deterministic; never
	// reassigned outside tests.
	now func() time.Time
	// mu guards lastPurge only (the lazy retention sweep's throttle). Every other
	// field is read-only after construction, and the ledger's mutual exclusion is
	// the database's row lock, not this mutex.
	mu        sync.Mutex
	lastPurge time.Time
	// lastContinuationPurge throttles the continuation store's own lazy sweep.
	// It is separate from lastPurge because the two stores are swept by
	// different call paths: one throttle would let a busy ledger starve the
	// continuation sweep, and vice versa.
	lastContinuationPurge time.Time
}

// NewPgStore runs EnsureSchema (the fail-fast: pgxpool.New is lazy and does not
// connect, so this CREATE TABLE is what forces the connection) and returns the
// store bound to holderID.
func NewPgStore(ctx context.Context, pool *pgxpool.Pool, holderID string) (*PgStore, error) {
	if err := EnsureSchema(ctx, pool); err != nil {
		return nil, fmt.Errorf("pgstore: EnsureSchema: %w", err)
	}
	return &PgStore{pool: pool, holderID: holderID, now: time.Now}, nil
}

// schemaLockKey serializes concurrent EnsureSchema calls (see below). Postgres
// advisory locks are CLUSTER-GLOBAL (not per-database), so a DISTINCT key is what
// keeps this from contending with another service's lock (e.g. auditstore's) — and
// even an accidental collision would only harmlessly serialize two unrelated
// schema-inits, never corrupt anything.
const schemaLockKey int64 = 0x676174657761795F // arbitrary distinct key ("gateway_" bytes)

// ddl is the gateway schema, executed verbatim by EnsureSchema. It lives at
// package scope so the hermetic DDL fence (ddl_fence_test.go) parses the very
// text that runs against Postgres, not a copy of it.
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
-- The pend ledger's additive columns (engine.PendLedger). This package has
-- no migration framework: EnsureSchema runs CREATE … IF NOT EXISTS in one
-- transaction, so the ledger arrives as ALTER … ADD COLUMN IF NOT EXISTS, which is
-- idempotent and leaves an already-upgraded database untouched. The three ledger
-- facts are nullable: a row written before the upgrade (or by the Store's keyless
-- pend) simply has no decision and no requester yet.
--
-- last_transition_at is the store's OWN clock, and retention counts from it, so a
-- payer's ClaimResponse.created can neither shorten nor extend how long a row
-- survives. It is NOT NULL with a DEFAULT so the ALTER backfills the rows already
-- in the table with the upgrade instant — an in-flight pend then keeps its full
-- retention period from the upgrade rather than being purged or kept forever.
ALTER TABLE gw_pended_claim ADD COLUMN IF NOT EXISTS outcome TEXT;
ALTER TABLE gw_pended_claim ADD COLUMN IF NOT EXISTS decided_at TIMESTAMPTZ;
ALTER TABLE gw_pended_claim ADD COLUMN IF NOT EXISTS requester_holder TEXT;
ALTER TABLE gw_pended_claim ADD COLUMN IF NOT EXISTS last_transition_at TIMESTAMPTZ NOT NULL DEFAULT now();
CREATE INDEX IF NOT EXISTS gw_pended_claim_retention ON gw_pended_claim (holder_id, last_transition_at);
-- gw_pended_claim_key is the ledger's lookup index: one row per (kind, key) a
-- follow-up can name an authorization by, in the namespace of the requester that
-- submitted it. Scalar columns only — NO JSON (AI-1 and the ddl_fence): a key is a
-- kind and a value, and nothing about a claim is stored here that is not one of
-- those. ON DELETE CASCADE ties the index's lifetime to the row it indexes, so the
-- retention purge cannot leave an entry pointing at a claim that is gone.
--
-- The CHECK is the backstop for engine.MaxPendKeyBytes: the key is payer-supplied
-- and part of the primary key, so an oversized value would overflow the btree index
-- row. Callers and every store refuse it first (see gw_replay for the same pairing).
CREATE TABLE IF NOT EXISTS gw_pended_claim_key (
    holder_id        TEXT NOT NULL,
    requester_holder TEXT NOT NULL,
    kind             TEXT NOT NULL,
    key              TEXT NOT NULL,
    subject_pci      TEXT NOT NULL,
    correlation_id   TEXT NOT NULL,
    PRIMARY KEY (holder_id, requester_holder, kind, key, subject_pci, correlation_id),
    FOREIGN KEY (holder_id, subject_pci, correlation_id)
        REFERENCES gw_pended_claim (holder_id, subject_pci, correlation_id) ON DELETE CASCADE,
    CHECK (octet_length(key) <= 512)
);
-- gw_pa_continuation is the server-held prior-authorization continuation
-- (engine.ContinuationStore): the metadata a provider gateway's own originator
-- flows need to inquire again about an authorization a payer pended. Shared
-- across the holder's replicas, and durable, which is what makes a continuation
-- answerable from whichever replica the next request lands on.
--
-- SCALARS AND TEXT[] ONLY — NO OPAQUE COLUMN (AI-1, ddl_fence). No order bytes
-- and no QuestionnaireResponse bytes are stored: the order is re-read from the
-- participant's own system at inquiry time, and the item table below is what
-- says whether it still matches what was submitted. The two arrays hold
-- identifier strings ("system|value"), which is what an answer is matched back
-- by; a JSON column "just for the keys" is exactly what the fence exists to stop.
--
-- Retention counts from updated_at, the store's OWN clock, for the same six
-- months the pend ledger keeps its authorizations: a capability outliving the
-- authorization it names, or the reverse, is a continuation that resolves to
-- nothing.
CREATE TABLE IF NOT EXISTS gw_pa_continuation (
    holder_id               TEXT NOT NULL,
    continuation_id         TEXT NOT NULL,
    payer_holder            TEXT NOT NULL,
    line                    TEXT NOT NULL,
    correlation_id          TEXT NOT NULL,
    subject_pci             TEXT NOT NULL,
    member_id               TEXT NOT NULL,
    sor_patient_id          TEXT NOT NULL,
    order_ref               TEXT NOT NULL,
    provider_npi            TEXT NOT NULL,
    claim_identifier        TEXT NOT NULL,
    claim_type              TEXT NOT NULL,
    claim_priority          TEXT NOT NULL,
    item_trace_numbers      TEXT[] NOT NULL DEFAULT '{}',
    payer_claimresponse_ids TEXT[] NOT NULL DEFAULT '{}',
    payer_preauth_ref       TEXT NOT NULL,
    last_outcome            TEXT NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL,
    updated_at              TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (holder_id, continuation_id)
);
CREATE INDEX IF NOT EXISTS gw_pa_continuation_retention ON gw_pa_continuation (holder_id, updated_at);
-- gw_pa_continuation_item is the item map: one row per line the submission
-- carried, with the sequence it was submitted under, the product it asked for,
-- its date of service and the trace number the payer echoes. It is what a later
-- inquiry's lines are built from, and what an order changed since submission is
-- detected against. ON DELETE CASCADE ties its lifetime to the continuation's,
-- so the retention sweep cannot leave a line behind with nothing to belong to.
CREATE TABLE IF NOT EXISTS gw_pa_continuation_item (
    holder_id       TEXT NOT NULL,
    continuation_id TEXT NOT NULL,
    sequence        INTEGER NOT NULL,
    product_code    TEXT NOT NULL,
    -- The coding's human-readable description, as the participant's own order
    -- stated it. It identifies nothing; the decision resource renders it.
    product_display TEXT NOT NULL DEFAULT '',
    service_date    TEXT NOT NULL,
    trace_number    TEXT NOT NULL,
    -- REF-BB and REF-NT: what the payer gave this line, when it gave them. An
    -- inquiry carries them when they are held, and a payer matches on them.
    authorization_number            TEXT NOT NULL,
    administration_reference_number TEXT NOT NULL,
    PRIMARY KEY (holder_id, continuation_id, sequence),
    FOREIGN KEY (holder_id, continuation_id)
        REFERENCES gw_pa_continuation (holder_id, continuation_id) ON DELETE CASCADE
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
CREATE TABLE IF NOT EXISTS gw_ingress_key (
    holder_id       TEXT NOT NULL,
    kid             TEXT NOT NULL,
    private_key_pem TEXT NOT NULL,      -- PKCS#8 PEM, P-384
    created_at      TIMESTAMPTZ NOT NULL,
    not_after       TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (holder_id, kid)
);
CREATE TABLE IF NOT EXISTS gw_replay (
    holder_id  TEXT NOT NULL,
    scope      TEXT NOT NULL,
    client_id  TEXT NOT NULL,             -- '' for single-issuer scopes
    key        TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (holder_id, scope, client_id, key),
    -- engine.MaxReplayKeyBytes: the key is caller-supplied (a jti / correlationId) and part
    -- of the primary key, so an oversized value overflows the btree index row on a healthy
    -- database. Callers and both stores reject it first; this is the backstop. No migration:
    -- this table has never shipped in a release, so the constraint arrives with the table.
    CHECK (octet_length(key) <= 512)
);
CREATE INDEX IF NOT EXISTS gw_replay_expiry ON gw_replay (holder_id, expires_at);
CREATE TABLE IF NOT EXISTS gw_exchange (
    holder_id   TEXT NOT NULL,
    exchange_id TEXT NOT NULL,
    workstream  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (holder_id, exchange_id)
);
CREATE INDEX IF NOT EXISTS gw_exchange_expiry ON gw_exchange (holder_id, expires_at);
CREATE TABLE IF NOT EXISTS gw_exchange_leg (
    holder_id      TEXT NOT NULL,
    exchange_id    TEXT NOT NULL,
    seq            INTEGER NOT NULL,
    leg_type       TEXT NOT NULL,
    correlation_id TEXT NOT NULL,
    subjects       TEXT[] NOT NULL DEFAULT '{}', -- a patient-agnostic leg has no subjects; never NULL (see AppendLeg)
    kind           TEXT NOT NULL,
    effect         TEXT NOT NULL,
    timing         TEXT NOT NULL,
    locality       TEXT NOT NULL,
    outcome        TEXT NOT NULL,
    PRIMARY KEY (holder_id, exchange_id, seq),
    FOREIGN KEY (holder_id, exchange_id) REFERENCES gw_exchange (holder_id, exchange_id)
        ON DELETE CASCADE
);
`

// EnsureSchema creates the gateway tables (business Store: gw_auth_number,
// gw_pended_claim with the pend ledger's gw_pended_claim_key, the continuation
// store's gw_pa_continuation with gw_pa_continuation_item, gw_eob; shared
// replica state: gw_ingress_key, gw_replay,
// gw_exchange, gw_exchange_leg) if absent (idempotent; plain DDL, no
// CREATE ROLE — least-privilege-friendly). Safe to call repeatedly AND concurrently:
// `CREATE TABLE/INDEX IF NOT EXISTS` is NOT concurrency-safe on its own (concurrent
// callers race in pg_type/pg_class and one fails with SQLSTATE 23505), and the four
// gateways share one shn_gateway DB and all run EnsureSchema at startup — so the DDL
// runs inside a transaction holding a transaction-scoped advisory lock (the
// auditstore pattern). The first holder creates the tables; the rest then find them
// already present (no-op).
func EnsureSchema(ctx context.Context, pool *pgxpool.Pool) error {
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

// RecordPendedClaim records a pended claim with no lookup keys — the Store seam's
// keyless pend. The conditional DO UPDATE is what makes `decided` absorbing here:
// a claim the ledger already decided is left alone, and only a dated, keyed re-pend
// (RecordPendedKeyed) can supersede a decision.
func (s *PgStore) RecordPendedClaim(subjectPCI, correlationID string) error {
	s.maybePurge()
	ctx, cancel := storeCtx()
	defer cancel()
	_, err := s.pool.Exec(ctx, `
INSERT INTO gw_pended_claim (holder_id, subject_pci, correlation_id, state, last_transition_at)
VALUES ($1, $2, $3, 'pended', $4)
ON CONFLICT (holder_id, subject_pci, correlation_id) DO UPDATE
  SET state = 'pended', last_transition_at = $4
  WHERE gw_pended_claim.state <> 'decided'`,
		s.holderID, subjectPCI, correlationID, s.now())
	if err != nil {
		return fmt.Errorf("pgstore: RecordPendedClaim: %w", err)
	}
	return nil
}

// BeginClaimUpdate is the ATOMIC test-and-set. It is BeginClaimUpdateReason with
// the refusal reason dropped, so the two can never disagree about which states may
// be claimed.
func (s *PgStore) BeginClaimUpdate(subjectPCI, correlationID string) (bool, error) {
	ok, _, err := s.BeginClaimUpdateReason(subjectPCI, correlationID)
	return ok, err
}

// ReleaseClaimUpdate returns an in-progress claim to pended. Its WHERE is also the
// "already decided" no-op: a decided row is not in_progress, so a rollback after
// the decision cannot revert it.
func (s *PgStore) ReleaseClaimUpdate(subjectPCI, correlationID string) error {
	ctx, cancel := storeCtx()
	defer cancel()
	_, err := s.pool.Exec(ctx, `
UPDATE gw_pended_claim SET state = 'pended', last_transition_at = $4
WHERE holder_id=$1 AND subject_pci=$2 AND correlation_id=$3 AND state='in_progress'`,
		s.holderID, subjectPCI, correlationID, s.now())
	if err != nil {
		return fmt.Errorf("pgstore: ReleaseClaimUpdate: %w", err)
	}
	return nil
}

// FinalizeClaimUpdate completes the pended→approved transition on the Store-only
// path by removing the claim (replay protection). A DECIDED row is exempt: the
// ledger keeps it for its retention period so a follow-up still resolves to the
// decision.
func (s *PgStore) FinalizeClaimUpdate(subjectPCI, correlationID string) error {
	ctx, cancel := storeCtx()
	defer cancel()
	_, err := s.pool.Exec(ctx,
		`DELETE FROM gw_pended_claim
WHERE holder_id=$1 AND subject_pci=$2 AND correlation_id=$3 AND state <> 'decided'`,
		s.holderID, subjectPCI, correlationID)
	if err != nil {
		return fmt.Errorf("pgstore: FinalizeClaimUpdate: %w", err)
	}
	return nil
}

// --- EOBs (Patient Access API surface) ---

func (s *PgStore) RecordEOB(subjectPCI, eobID string, eobJSON []byte) error {
	var recordedID string
	err := s.pool.QueryRow(context.Background(), `
INSERT INTO gw_eob (holder_id, eob_id, subject_pci, eob_json)
VALUES ($1, $2, $3, $4)
ON CONFLICT (holder_id, eob_id) DO UPDATE SET eob_json = EXCLUDED.eob_json
WHERE gw_eob.subject_pci = EXCLUDED.subject_pci
RETURNING eob_id`,
		s.holderID, eobID, subjectPCI, eobJSON).Scan(&recordedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return engine.ErrEOBSubjectMismatch
	}
	if err != nil {
		return fmt.Errorf("pgstore: RecordEOB: %w", err)
	}
	return nil
}

func (s *PgStore) EOBsForPatient(subjectPCI string) ([][]byte, bool) {
	// Ordered by created_at, eob_id — chronological, not the stub's slice-insertion order
	// (a second, benign divergence from MemStore: no caller asserts EOB order; the
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
