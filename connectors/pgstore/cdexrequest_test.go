//go:build !shn_coreonly

package pgstore

// cdexrequest_test.go — the CDex request record's durable rows, plus the
// hermetic rows that fence its table and its key check.
//
// TestCDexRequestDDL_* and TestCDexRequest_InvalidKeyNeverReachesTheDatabase run
// with no database. The rest are pg-gated (testPool skips without
// SHN_TEST_DATABASE_URL) and run in the serial `pg` job. The contract rows both
// backends answer alike are in parity_test.go (cdexParityChecks); the rows here
// need the injected clock, the table itself, or a second store.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
)

// TestCDexRequestDDL_KeyBoundMatchesTheEngineConstant: gw_cdex_request's CHECK
// is the backstop for engine.MaxCDexTrackingBytes, and a const string cannot
// interpolate the constant, so the literal is pinned here, as gw_replay's is.
func TestCDexRequestDDL_KeyBoundMatchesTheEngineConstant(t *testing.T) {
	want := fmt.Sprintf("CHECK (octet_length(tracking_system) + octet_length(tracking_value) <= %d)", engine.MaxCDexTrackingBytes)
	if body := ddlTableBody(t, "gw_cdex_request"); !strings.Contains(body, want) {
		t.Fatalf("gw_cdex_request does not declare %q: the schema and engine.MaxCDexTrackingBytes have drifted.\n%s", want, body)
	}
}

// cdexRequestColumns is gw_cdex_request's whole column set: opaque identifiers,
// a flag and a time (AI-1). Pinned so no column carrying a value arrives in the
// table quietly, whatever its type.
var cdexRequestColumns = map[string]string{
	"holder_id": "TEXT", "counterparty": "TEXT", "tracking_system": "TEXT", "tracking_value": "TEXT",
	"member_id": "TEXT", "subject_pci": "TEXT", "conflicted": "BOOLEAN", "changed_at": "TIMESTAMPTZ",
}

// TestCDexRequestDDL_NoClinicalColumns: the record is metadata. The generic
// fence already walks every gw_* table; this row names gw_cdex_request
// directly, because "the fence is green" would also be true of a schema with no
// such table, and pins its columns.
func TestCDexRequestDDL_NoClinicalColumns(t *testing.T) {
	cols := parseDDLTables(t, ddl)["gw_cdex_request"]
	if len(cols) == 0 {
		t.Fatal("ddl declares no gw_cdex_request")
	}
	seen := map[string]bool{}
	for _, c := range cols {
		want, ok := cdexRequestColumns[c.name]
		switch {
		case !ok:
			t.Errorf("gw_cdex_request.%s (%s) is not one of the record's pinned columns: it holds opaque identifiers, a flag and a time only (AI-1)", c.name, c.typ)
		case normalizeSQLType(c.typ) != want:
			t.Errorf("gw_cdex_request.%s is %s, pinned as %s", c.name, c.typ, want)
		}
		seen[c.name] = true
	}
	for _, name := range sortedKeys(mapKeys(cdexRequestColumns)) {
		if !seen[name] {
			t.Errorf("gw_cdex_request declares no %s", name)
		}
	}
}

// TestCDexRequest_InvalidKeyNeverReachesTheDatabase: a key with no participant
// or tracking value, or over engine.MaxCDexTrackingBytes, is refused before the
// store touches its pool (this store has none: a query would panic).
func TestCDexRequest_InvalidKeyNeverReachesTheDatabase(t *testing.T) {
	s := &PgStore{holderID: "payer", now: time.Now}
	for name, k := range invalidCDexKeys() {
		if err := s.RecordCDexRequest(k, engine.CDexRequestRecord{Member: "M1", PCI: "pci:1"}); !errors.Is(err, engine.ErrCDexRequestKey) {
			t.Errorf("%s: Record err = %v, want ErrCDexRequestKey", name, err)
		}
		rec, look, err := s.LookupCDexRequest(k)
		if !errors.Is(err, engine.ErrCDexRequestKey) || look != engine.CDexRequestNone || rec != (engine.CDexRequestRecord{}) {
			t.Errorf("%s: Lookup = %+v, %v, %v, want zero, none, ErrCDexRequestKey", name, rec, look, err)
		}
	}
}

func invalidCDexKeys() map[string]engine.CDexRequestKey {
	return map[string]engine.CDexRequestKey{
		"no participant":    {System: "urn:sys", Value: "t-1"},
		"no tracking value": {Counterparty: "payer-a", System: "urn:sys"},
		"one byte too long": {Counterparty: "payer-a", System: "urn:sys", Value: strings.Repeat("v", engine.MaxCDexTrackingBytes-len("urn:sys")+1)},
	}
}

// --- pg-gated rows ---

func cdexStore(t *testing.T, holder string, clock func() time.Time) *PgStore {
	t.Helper()
	pool := testPool(t)
	s, err := NewPgStore(context.Background(), pool, holder)
	if err != nil {
		t.Fatalf("NewPgStore: %v", err)
	}
	if clock != nil {
		s.now = clock
	}
	return s
}

// cdexRowCount counts the rows the table holds for s's holder, live or not.
func cdexRowCount(t *testing.T, s *PgStore) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM gw_cdex_request WHERE holder_id=$1`, s.holderID).Scan(&n); err != nil {
		t.Fatalf("count gw_cdex_request: %v", err)
	}
	return n
}

// cdexRow reads one row as stored, whatever its age.
func cdexRow(t *testing.T, s *PgStore, k engine.CDexRequestKey) (engine.CDexRequestRecord, bool, time.Time) {
	t.Helper()
	var (
		rec        engine.CDexRequestRecord
		conflicted bool
		changed    time.Time
	)
	if err := s.pool.QueryRow(context.Background(), `
SELECT member_id, subject_pci, conflicted, changed_at FROM gw_cdex_request
 WHERE holder_id=$1 AND counterparty=$2 AND tracking_system=$3 AND tracking_value=$4`,
		s.holderID, k.Counterparty, k.System, k.Value).Scan(&rec.Member, &rec.PCI, &conflicted, &changed); err != nil {
		t.Fatalf("read gw_cdex_request row: %v", err)
	}
	return rec, conflicted, changed
}

func mustRecordCDex(t *testing.T, s *PgStore, k engine.CDexRequestKey, rec engine.CDexRequestRecord) {
	t.Helper()
	if err := s.RecordCDexRequest(k, rec); err != nil {
		t.Fatalf("RecordCDexRequest(%+v): %v", k, err)
	}
}

func wantCDexLookup(t *testing.T, s *PgStore, k engine.CDexRequestKey, want engine.CDexRequestLookup, wantRec engine.CDexRequestRecord) {
	t.Helper()
	rec, look, err := s.LookupCDexRequest(k)
	if err != nil {
		t.Fatalf("LookupCDexRequest(%+v): %v", k, err)
	}
	if look != want || rec != wantRec {
		t.Fatalf("LookupCDexRequest(%+v) = %+v, %v; want %+v, %v", k, rec, look, wantRec, want)
	}
}

var (
	cdexT0   = time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC)
	cdexKey  = engine.CDexRequestKey{Counterparty: "payer-a", System: "urn:payer-a:tracking", Value: "trk-1"}
	cdexRecA = engine.CDexRequestRecord{Member: "MBR-1", PCI: "pci:one"}
	cdexDay  = 24 * time.Hour
)

// TestCDexRequestPg_RecordThenFound: a recorded request is found by its key,
// with the member and patient it was recorded with.
func TestCDexRequestPg_RecordThenFound(t *testing.T) {
	now := cdexT0
	s := cdexStore(t, "payer", func() time.Time { return now })
	wantCDexLookup(t, s, cdexKey, engine.CDexRequestNone, engine.CDexRequestRecord{})
	mustRecordCDex(t, s, cdexKey, cdexRecA)
	wantCDexLookup(t, s, cdexKey, engine.CDexRequestFound, cdexRecA)
}

// TestCDexRequestPg_SameRecordAgainIsANoOp: the same record sent again changes
// nothing, its time included: re-sent at T+100d it still expires at T+184d,
// where a refreshed row would live until T+284d.
func TestCDexRequestPg_SameRecordAgainIsANoOp(t *testing.T) {
	now := cdexT0
	s := cdexStore(t, "payer", func() time.Time { return now })
	mustRecordCDex(t, s, cdexKey, cdexRecA)
	now = cdexT0.Add(100 * cdexDay)
	mustRecordCDex(t, s, cdexKey, cdexRecA)
	if _, conflicted, changed := cdexRow(t, s, cdexKey); conflicted || !changed.Equal(cdexT0) {
		t.Fatalf("after the same record again: conflicted=%v changed=%v, want false, %v", conflicted, changed, cdexT0)
	}
	now = cdexT0.Add(engine.CDexRequestRetention - time.Second)
	wantCDexLookup(t, s, cdexKey, engine.CDexRequestFound, cdexRecA)
	now = cdexT0.Add(engine.CDexRequestRetention + time.Second)
	wantCDexLookup(t, s, cdexKey, engine.CDexRequestNone, engine.CDexRequestRecord{})
}

// TestCDexRequestPg_DifferentRecordConflicts: a key recorded again for another
// member, or for the same member with another patient, is conflicted, and its
// first record is never returned or overwritten. A third record on a conflicted
// key leaves it conflicted, its time unmoved; retention counts from the
// conflict.
func TestCDexRequestPg_DifferentRecordConflicts(t *testing.T) {
	now := cdexT0
	s := cdexStore(t, "payer", func() time.Time { return now })

	mustRecordCDex(t, s, cdexKey, cdexRecA)
	conflictAt := cdexT0.Add(time.Hour)
	now = conflictAt
	mustRecordCDex(t, s, cdexKey, engine.CDexRequestRecord{Member: "MBR-2", PCI: "pci:one"})
	wantCDexLookup(t, s, cdexKey, engine.CDexRequestConflicted, engine.CDexRequestRecord{})
	if rec, conflicted, changed := cdexRow(t, s, cdexKey); rec != cdexRecA || !conflicted || !changed.Equal(conflictAt) {
		t.Fatalf("conflicted row = %+v conflicted=%v changed=%v; want the first record kept, conflicted at %v", rec, conflicted, changed, conflictAt)
	}

	// A third record, the original or a new one, leaves it conflicted and its
	// time where the conflict put it.
	now = cdexT0.Add(2 * time.Hour)
	mustRecordCDex(t, s, cdexKey, cdexRecA)
	mustRecordCDex(t, s, cdexKey, engine.CDexRequestRecord{Member: "MBR-3", PCI: "pci:three"})
	wantCDexLookup(t, s, cdexKey, engine.CDexRequestConflicted, engine.CDexRequestRecord{})
	if rec, conflicted, changed := cdexRow(t, s, cdexKey); rec != cdexRecA || !conflicted || !changed.Equal(conflictAt) {
		t.Fatalf("after a third record: %+v conflicted=%v changed=%v; want the first record, conflicted at %v", rec, conflicted, changed, conflictAt)
	}

	// Same member, another patient: conflicted too.
	k2 := engine.CDexRequestKey{Counterparty: "payer-a", System: "urn:payer-a:tracking", Value: "trk-2"}
	mustRecordCDex(t, s, k2, cdexRecA)
	mustRecordCDex(t, s, k2, engine.CDexRequestRecord{Member: cdexRecA.Member, PCI: "pci:other"})
	wantCDexLookup(t, s, k2, engine.CDexRequestConflicted, engine.CDexRequestRecord{})

	// Retention counts from the conflict, not the first record.
	now = cdexT0.Add(engine.CDexRequestRetention + time.Second)
	wantCDexLookup(t, s, cdexKey, engine.CDexRequestConflicted, engine.CDexRequestRecord{})
	now = conflictAt.Add(engine.CDexRequestRetention + time.Second)
	wantCDexLookup(t, s, cdexKey, engine.CDexRequestNone, engine.CDexRequestRecord{})
}

// TestCDexRequestPg_OtherCounterparty: no record for the key, but a live one of
// its tracking identifier with another participant, is OtherCounterparty; a
// record past retention under the other participant is not.
func TestCDexRequestPg_OtherCounterparty(t *testing.T) {
	now := cdexT0
	s := cdexStore(t, "payer", func() time.Time { return now })
	mustRecordCDex(t, s, cdexKey, cdexRecA)
	other := cdexKey
	other.Counterparty = "payer-b"
	wantCDexLookup(t, s, other, engine.CDexRequestOtherCounterparty, engine.CDexRequestRecord{})
	unrelated := other
	unrelated.Value = "trk-unrelated"
	wantCDexLookup(t, s, unrelated, engine.CDexRequestNone, engine.CDexRequestRecord{})
	otherSystem := other
	otherSystem.System = "urn:payer-b:tracking"
	wantCDexLookup(t, s, otherSystem, engine.CDexRequestNone, engine.CDexRequestRecord{})
	now = cdexT0.Add(engine.CDexRequestRetention + time.Second)
	wantCDexLookup(t, s, other, engine.CDexRequestNone, engine.CDexRequestRecord{})
}

// TestCDexRequestPg_RetentionIsEnforced: a row past retention is never found,
// even before the sweep removes it, and a record on its key is written afresh;
// the sweep then removes the table's stale rows, and a fresh record on a swept
// key is found.
func TestCDexRequestPg_RetentionIsEnforced(t *testing.T) {
	now := cdexT0
	s := cdexStore(t, "payer", func() time.Time { return now })
	mustRecordCDex(t, s, cdexKey, cdexRecA)
	conflictedKey := engine.CDexRequestKey{Counterparty: "payer-a", System: "urn:payer-a:tracking", Value: "trk-conflicted"}
	mustRecordCDex(t, s, conflictedKey, cdexRecA)
	mustRecordCDex(t, s, conflictedKey, engine.CDexRequestRecord{Member: "MBR-2", PCI: "pci:two"})

	// Past retention, with the sweep held off: not found, though still stored.
	now = cdexT0.Add(engine.CDexRequestRetention + time.Hour)
	holdSweep := func() {
		s.mu.Lock()
		s.lastCDexRequestPurge = now
		s.mu.Unlock()
	}
	holdSweep()
	wantCDexLookup(t, s, cdexKey, engine.CDexRequestNone, engine.CDexRequestRecord{})
	wantCDexLookup(t, s, conflictedKey, engine.CDexRequestNone, engine.CDexRequestRecord{})
	if n := cdexRowCount(t, s); n != 2 {
		t.Fatalf("rows with the sweep held off = %d, want 2", n)
	}
	// A record on an expired key is fresh, not a conflict, even for another
	// member, and an expired conflict is cleared.
	fresh := engine.CDexRequestRecord{Member: "MBR-9", PCI: "pci:nine"}
	mustRecordCDex(t, s, conflictedKey, fresh)
	wantCDexLookup(t, s, conflictedKey, engine.CDexRequestFound, fresh)
	if _, conflicted, changed := cdexRow(t, s, conflictedKey); conflicted || !changed.Equal(now) {
		t.Fatalf("a record on an expired key: conflicted=%v changed=%v, want false, %v", conflicted, changed, now)
	}

	// The sweep runs once the throttle allows: the stale row is gone.
	now = now.Add(engine.PendPurgeInterval + time.Second)
	wantCDexLookup(t, s, cdexKey, engine.CDexRequestNone, engine.CDexRequestRecord{})
	if n := cdexRowCount(t, s); n != 1 {
		t.Fatalf("rows after the sweep = %d, want 1 (only the fresh record)", n)
	}
	mustRecordCDex(t, s, cdexKey, cdexRecA)
	wantCDexLookup(t, s, cdexKey, engine.CDexRequestFound, cdexRecA)
}

// TestCDexRequestPg_RetentionPurgeIsBoundedPerCall: one sweep removes at most
// engine.PendPurgeMax stale rows, and never a live one.
func TestCDexRequestPg_RetentionPurgeIsBoundedPerCall(t *testing.T) {
	now := cdexT0
	s := cdexStore(t, "payer", func() time.Time { return now })
	const stale = engine.PendPurgeMax + 20
	for i := 0; i < stale; i++ {
		k := cdexKey
		k.Value = fmt.Sprintf("trk-stale-%d", i)
		mustRecordCDex(t, s, k, cdexRecA)
	}
	now = cdexT0.Add(engine.CDexRequestRetention + time.Hour)
	s.mu.Lock()
	s.lastCDexRequestPurge = now
	s.mu.Unlock()
	live := cdexKey
	live.Value = "trk-live"
	mustRecordCDex(t, s, live, cdexRecA)
	now = now.Add(engine.PendPurgeInterval + time.Second)
	wantCDexLookup(t, s, live, engine.CDexRequestFound, cdexRecA)
	if n, want := cdexRowCount(t, s), stale-engine.PendPurgeMax+1; n != want {
		t.Fatalf("rows after one sweep = %d, want %d", n, want)
	}
}

// TestCDexRequestPg_HolderScoped: another holder sharing the database sees
// nothing of this holder's record (not Found, not OtherCounterparty, no
// conflict), and this holder's sweep leaves its rows alone.
func TestCDexRequestPg_HolderScoped(t *testing.T) {
	now := cdexT0
	a := cdexStore(t, "payer", func() time.Time { return now })
	b, err := NewPgStore(context.Background(), a.pool, "payer-two")
	if err != nil {
		t.Fatal(err)
	}
	b.now = a.now
	mustRecordCDex(t, b, cdexKey, cdexRecA)
	wantCDexLookup(t, a, cdexKey, engine.CDexRequestNone, engine.CDexRequestRecord{})
	other := cdexKey
	other.Counterparty = "payer-b"
	wantCDexLookup(t, a, other, engine.CDexRequestNone, engine.CDexRequestRecord{})

	recA2 := engine.CDexRequestRecord{Member: "MBR-2", PCI: "pci:two"}
	mustRecordCDex(t, a, cdexKey, recA2)
	wantCDexLookup(t, a, cdexKey, engine.CDexRequestFound, recA2)
	wantCDexLookup(t, b, cdexKey, engine.CDexRequestFound, cdexRecA)

	now = cdexT0.Add(engine.CDexRequestRetention + time.Hour)
	wantCDexLookup(t, a, cdexKey, engine.CDexRequestNone, engine.CDexRequestRecord{})
	if n := cdexRowCount(t, a); n != 0 {
		t.Fatalf("holder a's rows after its sweep = %d, want 0", n)
	}
	if n := cdexRowCount(t, b); n != 1 {
		t.Fatalf("holder b's rows after holder a's sweep = %d, want 1", n)
	}
}

// TestCDexRequestPg_InvalidKey: an invalid key is refused by both methods with
// ErrCDexRequestKey and writes nothing; a key at the bound is recorded; and the
// table's CHECK refuses an oversized key written past the store.
func TestCDexRequestPg_InvalidKey(t *testing.T) {
	s := cdexStore(t, "payer", nil)
	for name, k := range invalidCDexKeys() {
		if err := s.RecordCDexRequest(k, cdexRecA); !errors.Is(err, engine.ErrCDexRequestKey) {
			t.Errorf("%s: Record err = %v, want ErrCDexRequestKey", name, err)
		}
		if _, look, err := s.LookupCDexRequest(k); !errors.Is(err, engine.ErrCDexRequestKey) || look != engine.CDexRequestNone {
			t.Errorf("%s: Lookup = %v, %v, want none, ErrCDexRequestKey", name, look, err)
		}
	}
	if n := cdexRowCount(t, s); n != 0 {
		t.Fatalf("invalid keys wrote %d rows", n)
	}
	atBound := engine.CDexRequestKey{Counterparty: "payer-a", System: "urn:sys",
		Value: strings.Repeat("v", engine.MaxCDexTrackingBytes-len("urn:sys"))}
	mustRecordCDex(t, s, atBound, cdexRecA)
	wantCDexLookup(t, s, atBound, engine.CDexRequestFound, cdexRecA)

	_, err := s.pool.Exec(context.Background(), `
INSERT INTO gw_cdex_request (holder_id, counterparty, tracking_system, tracking_value, member_id, subject_pci, changed_at)
VALUES ($1, 'payer-a', 'urn:sys', $2, 'MBR-1', 'pci:one', now())`,
		s.holderID, strings.Repeat("v", engine.MaxCDexTrackingBytes-len("urn:sys")+1))
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("an oversized key written past the store: err = %v, want a check_violation (23514)", err)
	}
}

// TestCDexRequestPg_SurvivesReopen: the record is durable. A store opened on a
// new pool over the same database, after the first is closed, still finds it.
func TestCDexRequestPg_SurvivesReopen(t *testing.T) {
	before := cdexStore(t, "payer", nil)
	mustRecordCDex(t, before, cdexKey, cdexRecA)
	before.pool.Close()

	pool, err := pgxpool.New(context.Background(), os.Getenv("SHN_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	after, err := NewPgStore(context.Background(), pool, "payer")
	if err != nil {
		t.Fatal(err)
	}
	wantCDexLookup(t, after, cdexKey, engine.CDexRequestFound, cdexRecA)
}

// TestCDexRequestPg_StoreErrorIsReturned: a store that cannot be read says so;
// "unavailable" never reads as "none".
func TestCDexRequestPg_StoreErrorIsReturned(t *testing.T) {
	s := cdexStore(t, "payer", nil)
	s.pool.Close()
	if err := s.RecordCDexRequest(cdexKey, cdexRecA); err == nil {
		t.Fatal("Record over a closed pool returned no error")
	}
	if _, _, err := s.LookupCDexRequest(cdexKey); err == nil {
		t.Fatal("Lookup over a closed pool returned no error")
	}
}

// TestCDexRequestPg_RacingReplicasCannotOverwrite: replicas of one holder's
// gateway recording one key for different patients at once leave it
// conflicted, never one replica's record standing as found; replicas recording
// the same record leave it found.
func TestCDexRequestPg_RacingReplicasCannotOverwrite(t *testing.T) {
	first := cdexStore(t, "payer", nil)
	const replicas = 8
	stores := []*PgStore{first}
	for i := 1; i < replicas; i++ {
		r, err := NewPgStore(context.Background(), first.pool, "payer")
		if err != nil {
			t.Fatal(err)
		}
		stores = append(stores, r)
	}
	race := func(k engine.CDexRequestKey, recOf func(i int) engine.CDexRequestRecord) {
		var wg sync.WaitGroup
		errs := make(chan error, replicas)
		for i, r := range stores {
			wg.Add(1)
			go func() { defer wg.Done(); errs <- r.RecordCDexRequest(k, recOf(i)) }()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("racing record: %v", err)
			}
		}
	}
	race(cdexKey, func(i int) engine.CDexRequestRecord {
		return engine.CDexRequestRecord{Member: fmt.Sprintf("MBR-%d", i), PCI: fmt.Sprintf("pci:%d", i)}
	})
	wantCDexLookup(t, first, cdexKey, engine.CDexRequestConflicted, engine.CDexRequestRecord{})

	same := cdexKey
	same.Value = "trk-same"
	race(same, func(int) engine.CDexRequestRecord { return cdexRecA })
	wantCDexLookup(t, first, same, engine.CDexRequestFound, cdexRecA)
}
