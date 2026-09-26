package pgstore

// pendledger_pg_test.go holds the ledger rows that only the DURABLE backend can
// answer, and that need in-package access: the injected clock behind the bounded
// retention sweep, and a genuine mid-transaction EOB failure — the one thing an
// in-memory store cannot produce, and the whole point of "the EOB is written in the
// same transaction as the decision".
//
// Every row here is pg-gated (testPool skips without SHN_TEST_DATABASE_URL) and
// runs in the serial `pg` job.

import (
	"context"
	"strconv"
	"testing"
	"time"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
)

func ledgerStore(t *testing.T, clock func() time.Time) *PgStore {
	t.Helper()
	pool := testPool(t)
	s, err := NewPgStore(context.Background(), pool, "payer")
	if err != nil {
		t.Fatalf("NewPgStore: %v", err)
	}
	s.now = clock
	return s
}

func pendRowCount(t *testing.T, s *PgStore) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM gw_pended_claim WHERE holder_id=$1`, s.holderID).Scan(&n); err != nil {
		t.Fatalf("count gw_pended_claim: %v", err)
	}
	return n
}

// TestPgPendLedger_RetentionPurgeIsBoundedPerCall: the durable sweep drops rows
// whose last transition is older than the retention period, at most
// engine.PendPurgeMax per call, and never a row inside the window.
func TestPgPendLedger_RetentionPurgeIsBoundedPerCall(t *testing.T) {
	now := time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC)
	s := ledgerStore(t, func() time.Time { return now })

	old := now.Add(-engine.PendLedgerRetention - 24*time.Hour)
	stale := now
	now = old
	const staleRows = 150
	for i := 0; i < staleRows; i++ {
		if err := s.RecordPendedClaim("PCI-A", "corr-"+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	now = stale
	if _, err := s.RecordPendedKeyed("PCI-A", "corr-fresh", now, engine.PendKeys{RequesterHolder: "provider-a"}); err != nil {
		t.Fatal(err)
	}
	left := pendRowCount(t, s)
	if want := staleRows + 1 - engine.PendPurgeMax; left != want {
		t.Fatalf("after one sweep %d rows remain, want %d (a sweep bounded to %d)", left, want, engine.PendPurgeMax)
	}

	// Rate-limited: a second write inside the same interval sweeps nothing.
	if _, err := s.RecordPendedKeyed("PCI-A", "corr-fresh-2", now, engine.PendKeys{RequesterHolder: "provider-a"}); err != nil {
		t.Fatal(err)
	}
	if got := pendRowCount(t, s); got != left+1 {
		t.Fatalf("a second write in the same interval swept rows: %d, want %d", got, left+1)
	}

	// Once the interval elapses the sweep resumes and stops at the fresh rows.
	for i := 0; i < 3; i++ {
		now = now.Add(engine.PendPurgeInterval + time.Second)
		if _, err := s.RecordPendedKeyed("PCI-A", "corr-fresh", now, engine.PendKeys{RequesterHolder: "provider-a"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := pendRowCount(t, s); got != 2 {
		t.Fatalf("%d rows remain, want the 2 inside the retention window", got)
	}
	if _, ok, err := s.PendRecordOf("PCI-A", "corr-fresh"); !ok || err != nil {
		t.Fatalf("the fresh row was purged: %v,%v", ok, err)
	}
}

// TestPgPendLedger_IndexHoldsExactlyTheDeclaredKeys: the durable index must hold
// ONE row per engine.PendKeys.Refs() entry and nothing else. Refs is the only
// function that turns a PendKeys field into a stored key, so this count is what
// stops a field that is NOT a lookup key from quietly becoming one — a non-key
// input to the write (the payer's ClaimResponse.created, say) indexed by accident
// would silently widen what a follow-up can match on.
func TestPgPendLedger_IndexHoldsExactlyTheDeclaredKeys(t *testing.T) {
	at := time.Date(2026, 9, 17, 11, 0, 0, 0, time.UTC)
	s := ledgerStore(t, func() time.Time { return at })
	k := engine.PendKeys{
		RequesterHolder:  "provider-a",
		RequestIDs:       []string{"urn:shn:claim|CLM-1", "urn:shn:claim|CLM-2"},
		ClaimResponseIDs: []string{"urn:payer:claimresponse|CR-1"},
		PreAuthRef:       "PA-1",
		ItemTraceNumbers: []string{"urn:shn:trace|TR-1-1", "urn:shn:trace|TR-1-2"},
	}
	refs, err := k.Refs()
	if err != nil {
		t.Fatal(err)
	}
	tr, err := s.RecordPendedKeyed("PCI-A", "corr-A", at, k)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Keys != len(refs) {
		t.Fatalf("the pend reported %d keys, want %d", tr.Keys, len(refs))
	}

	var stored int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM gw_pended_claim_key WHERE holder_id=$1 AND subject_pci=$2 AND correlation_id=$3`,
		s.holderID, "PCI-A", "corr-A").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != len(refs) {
		t.Fatalf("%d index rows for %d declared keys — the index holds a fact Refs did not produce", stored, len(refs))
	}

	// And every stored row IS one of the declared keys, kind and value.
	want := map[engine.PendKeyRef]bool{}
	for _, ref := range refs {
		want[ref] = true
	}
	rows, err := s.pool.Query(context.Background(),
		`SELECT kind, key FROM gw_pended_claim_key WHERE holder_id=$1 AND subject_pci=$2 AND correlation_id=$3`,
		s.holderID, "PCI-A", "corr-A")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var got engine.PendKeyRef
		if err := rows.Scan(&got.Kind, &got.Key); err != nil {
			t.Fatal(err)
		}
		if !want[got] {
			t.Errorf("the index holds %+v, which PendKeys.Refs never declared", got)
		}
		delete(want, got)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for ref := range want {
		t.Errorf("declared key %+v was not indexed", ref)
	}
}

// TestPgPendLedger_PurgeTakesTheKeysWithTheRow: a purged authorization must leave
// no lookup entry behind, or a later follow-up would resolve to a claim that is
// gone. ON DELETE CASCADE is what guarantees it; this is the row that proves it.
func TestPgPendLedger_PurgeTakesTheKeysWithTheRow(t *testing.T) {
	now := time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC)
	s := ledgerStore(t, func() time.Time { return now })
	if _, err := s.RecordPendedKeyed("PCI-A", "corr-A", now, engine.PendKeys{
		RequesterHolder: "provider-a",
		PreAuthRef:      "PA-1",
	}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(engine.PendLedgerRetention + engine.PendPurgeInterval + time.Hour)
	if _, err := s.RecordPendedKeyed("PCI-B", "corr-B", now, engine.PendKeys{RequesterHolder: "provider-a"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.PendRecordOf("PCI-A", "corr-A"); ok || err != nil {
		t.Fatalf("the expired row survived: %v,%v", ok, err)
	}
	var keys int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM gw_pended_claim_key WHERE holder_id=$1 AND subject_pci=$2`, s.holderID, "PCI-A").Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if keys != 0 {
		t.Fatalf("%d lookup keys outlived their purged claim", keys)
	}
	_, _, found, _, err := s.LookupPended("provider-a", engine.PendKeys{RequesterHolder: "provider-a", PreAuthRef: "PA-1"})
	if err != nil || found {
		t.Fatalf("a purged claim is still findable: %v,%v", found, err)
	}
}

// TestPgPendLedger_DecisionAndEOBAreOneTransaction injects a REAL failure into the
// EOB write — a trigger that raises inside the transaction, after the decision row
// has already been written — and asserts the decision rolled back with it. This is
// the atomicity claim itself: an in-memory store's EOB write cannot fail, so only
// the durable backend can answer this row.
func TestPgPendLedger_DecisionAndEOBAreOneTransaction(t *testing.T) {
	at := time.Date(2026, 9, 17, 11, 0, 0, 0, time.UTC)
	s := ledgerStore(t, func() time.Time { return at })
	ctx := context.Background()
	if _, err := s.RecordPendedKeyed("PCI-A", "corr-A", at, engine.PendKeys{
		RequesterHolder: "provider-a",
		PreAuthRef:      "PA-1",
	}); err != nil {
		t.Fatal(err)
	}

	for _, stmt := range []string{
		`CREATE OR REPLACE FUNCTION gw_eob_injected_failure() RETURNS trigger AS $$
BEGIN RAISE EXCEPTION 'injected EOB write failure'; END; $$ LANGUAGE plpgsql`,
		`CREATE TRIGGER gw_eob_injected_failure BEFORE INSERT ON gw_eob
FOR EACH ROW EXECUTE FUNCTION gw_eob_injected_failure()`,
	} {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("arm the injected failure: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = s.pool.Exec(ctx, `DROP TRIGGER IF EXISTS gw_eob_injected_failure ON gw_eob`)
		_, _ = s.pool.Exec(ctx, `DROP FUNCTION IF EXISTS gw_eob_injected_failure()`)
	})

	_, err := s.RecordDecision("PCI-A", "corr-A", engine.PendOutcomeApproved, at,
		&engine.EOBRecord{SubjectPCI: "PCI-A", EOBID: "eob-1", JSON: []byte(`{"resourceType":"ExplanationOfBenefit"}`)})
	if err == nil {
		t.Fatal("RecordDecision returned nil with the EOB write failing")
	}

	// No decision: the claim is still pended, and an amendment can still bind it.
	rec, ok, recErr := s.PendRecordOf("PCI-A", "corr-A")
	if recErr != nil || !ok {
		t.Fatalf("PendRecordOf = %v,%v", ok, recErr)
	}
	if rec.State != engine.PendStatePended || rec.Outcome != "" || !rec.DecidedAt.IsZero() {
		t.Fatalf("a failed EOB write left a decision behind: %+v", rec)
	}
	if got, found := s.EOBsForPatient("PCI-A"); found || len(got) != 0 {
		t.Fatalf("%d EOBs were written by the failed transaction", len(got))
	}

	// With the failure removed the same call decides cleanly — the rollback left
	// nothing wedged.
	if _, err := s.pool.Exec(ctx, `DROP TRIGGER gw_eob_injected_failure ON gw_eob`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordDecision("PCI-A", "corr-A", engine.PendOutcomeApproved, at,
		&engine.EOBRecord{SubjectPCI: "PCI-A", EOBID: "eob-1", JSON: []byte(`{"resourceType":"ExplanationOfBenefit"}`)}); err != nil {
		t.Fatalf("RecordDecision after the failure was removed: %v", err)
	}
	rec, _, _ = s.PendRecordOf("PCI-A", "corr-A")
	if rec.State != engine.PendStateDecided || rec.Outcome != engine.PendOutcomeApproved {
		t.Fatalf("the retried decision = %+v", rec)
	}
	if got, found := s.EOBsForPatient("PCI-A"); !found || len(got) != 1 {
		t.Fatalf("EOBsForPatient = %d,%v, want one EOB", len(got), found)
	}
}

// TestPgPendLedger_ConcurrentBeginBindsOnce: the row lock is the mutual exclusion
// across connections (two replicas amending the same authorization). Exactly one
// caller binds; the other is refused as in-progress, never with a store error.
func TestPgPendLedger_ConcurrentBeginBindsOnce(t *testing.T) {
	at := time.Date(2026, 9, 17, 11, 0, 0, 0, time.UTC)
	s := ledgerStore(t, func() time.Time { return at })
	if _, err := s.RecordPendedKeyed("PCI-A", "corr-A", at, engine.PendKeys{RequesterHolder: "provider-a"}); err != nil {
		t.Fatal(err)
	}
	const racers = 8
	type outcome struct {
		ok  bool
		why engine.PendRefusal
		err error
	}
	results := make(chan outcome, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		go func() {
			<-start
			ok, why, err := s.BeginClaimUpdateReason("PCI-A", "corr-A")
			results <- outcome{ok, why, err}
		}()
	}
	close(start)
	bound, refused := 0, 0
	for i := 0; i < racers; i++ {
		r := <-results
		switch {
		case r.err != nil:
			t.Errorf("begin returned a store error: %v", r.err)
		case r.ok:
			bound++
		case r.why == engine.PendRefusalInProgress:
			refused++
		default:
			t.Errorf("begin refused with %q, want %q", r.why, engine.PendRefusalInProgress)
		}
	}
	if bound != 1 || refused != racers-1 {
		t.Fatalf("%d bound and %d refused, want exactly 1 and %d", bound, refused, racers-1)
	}
}

// TestPgPendLedger_KeyBoundIsEnforcedByTheDatabase: the CHECK is the backstop for
// the store-level guard. If a future caller reached the insert with an oversized
// key, the database refuses it rather than corrupting the index.
func TestPgPendLedger_KeyBoundIsEnforcedByTheDatabase(t *testing.T) {
	at := time.Date(2026, 9, 17, 11, 0, 0, 0, time.UTC)
	s := ledgerStore(t, func() time.Time { return at })
	if _, err := s.RecordPendedKeyed("PCI-A", "corr-A", at, engine.PendKeys{RequesterHolder: "provider-a"}); err != nil {
		t.Fatal(err)
	}
	long := make([]byte, engine.MaxPendKeyBytes+1)
	for i := range long {
		long[i] = 'x'
	}
	_, err := s.pool.Exec(context.Background(), `
INSERT INTO gw_pended_claim_key (holder_id, requester_holder, kind, key, subject_pci, correlation_id)
VALUES ($1, $2, $3, $4, $5, $6)`,
		s.holderID, "provider-a", engine.PendKeyRequestIdentifier, string(long), "PCI-A", "corr-A")
	if err == nil {
		t.Fatalf("the database accepted a %d-byte key (max %d)", len(long), engine.MaxPendKeyBytes)
	}
}

// TestPgPendLedger_AStrandedHoldLapses is the Postgres row for
// engine.TestPendLedger_AStrandedHoldLapses, the keyless SQL included.
func TestPgPendLedger_AStrandedHoldLapses(t *testing.T) {
	now := time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC)
	s := ledgerStore(t, func() time.Time { return now })
	keys := engine.PendKeys{RequesterHolder: "provider-a", PreAuthRef: "PA-1"}
	if _, err := s.RecordPendedKeyed("PCI-A", "corr-A", now, keys); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := s.BeginClaimUpdateReason("PCI-A", "corr-A"); err != nil || !ok {
		t.Fatalf("begin: %v %v", ok, err)
	}
	now = now.Add(engine.PendInProgressStale - time.Second)
	if _, err := s.RecordPendedKeyed("PCI-A", "corr-A", now, keys); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordPendedClaim("PCI-A", "corr-A"); err != nil {
		t.Fatal(err)
	}
	if rec, _, _ := s.PendRecordOf("PCI-A", "corr-A"); rec.State != engine.PendStateInProgress {
		t.Fatalf("a re-pend moved a live hold: %+v", rec)
	}
	now = now.Add(2 * time.Second)
	if err := s.RecordPendedClaim("PCI-A", "corr-A"); err != nil {
		t.Fatal(err)
	}
	if rec, _, _ := s.PendRecordOf("PCI-A", "corr-A"); rec.State != engine.PendStatePended {
		t.Fatalf("a lapsed hold was not re-pended: %+v", rec)
	}
	if ok, _, err := s.BeginClaimUpdateReason("PCI-A", "corr-A"); err != nil || !ok {
		t.Fatalf("a new amendment could not bind after the hold lapsed: %v %v", ok, err)
	}
}

// OpenPends lists only this holder's undecided pends: a decided one (dated or
// not), and one filed by another holder, are left out.
func TestPgPendLedger_OpenPendsListsOnlyUndecided(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	s := ledgerStore(t, func() time.Time { return now })
	for _, corr := range []string{"open", "done", "undated"} {
		if _, err := s.RecordPendedKeyed("PCI-"+corr, "corr-"+corr, now, engine.PendKeys{RequesterHolder: "provider-a"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.RecordDecision("PCI-done", "corr-done", "approved", now, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordDecision("PCI-undated", "corr-undated", "denied", time.Time{}, nil); err != nil {
		t.Fatal(err)
	}
	var decidedAt *time.Time
	if err := s.pool.QueryRow(context.Background(),
		`SELECT decided_at FROM gw_pended_claim WHERE holder_id=$1 AND correlation_id='corr-undated'`, s.holderID).Scan(&decidedAt); err != nil || decidedAt != nil {
		t.Fatalf("the undated decision must be stored without a date to exercise the state check: %v %v", decidedAt, err)
	}
	other, err := NewPgStore(context.Background(), s.pool, "another-payer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.RecordPendedKeyed("PCI-other", "corr-other", now, engine.PendKeys{RequesterHolder: "provider-a"}); err != nil {
		t.Fatal(err)
	}
	open, err := OpenPends(context.Background(), s.pool, s.holderID)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].SubjectPCI != "PCI-open" || open[0].CorrelationID != "corr-open" || open[0].RequesterHolder != "provider-a" {
		t.Fatalf("open pends %+v, want only PCI-open/corr-open", open)
	}
}
