package engine

// workstream_pa_pendledger_test.go holds the ledger rows that need the engine's memory store:
// the injected clock behind the bounded retention purge and the ledger-LESS
// fallback (a Store that deliberately does not implement PendLedger). The
// ledger's own rules (the structural pin on PendKeys among them) are tested in
// package pend. The backend-agnostic state-machine and lookup table lives
// in gateway/connectors/pgstore/pendledger_test.go, where it runs against MemStore
// AND PgStore from one definition.

import (
	"strconv"
	"testing"
	"time"
)

// pendedRows counts the ledger's rows. In-package, under the ledger's own lock, so
// the retention rows assert what the sweep actually left behind.
func pendedRows(d *memPendLedger) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.pendedClaims)
}

// storeWithoutLedger is a Store that deliberately does NOT implement PendLedger:
// the fallback shape (a partner's own Store, or any implementation written before
// the ledger existed). It delegates the Store surface to a MemStore and declares
// nothing else, so the type assertion in LedgerOf must fail on it.
type storeWithoutLedger struct{ inner *MemStore }

func (s storeWithoutLedger) StoreAuthNumber(ref, pre string) error {
	return s.inner.StoreAuthNumber(ref, pre)
}
func (s storeWithoutLedger) AuthNumber(ref string) (string, bool) { return s.inner.AuthNumber(ref) }
func (s storeWithoutLedger) RecordPendedClaim(pci, corr string) error {
	return s.inner.RecordPendedClaim(pci, corr)
}
func (s storeWithoutLedger) BeginClaimUpdate(pci, corr string) (bool, error) {
	return s.inner.BeginClaimUpdate(pci, corr)
}
func (s storeWithoutLedger) ReleaseClaimUpdate(pci, corr string) error {
	return s.inner.ReleaseClaimUpdate(pci, corr)
}
func (s storeWithoutLedger) FinalizeClaimUpdate(pci, corr string) error {
	return s.inner.FinalizeClaimUpdate(pci, corr)
}

var _ Store = storeWithoutLedger{}

// TestPendLedger_LedgerOfRefusesAStoreWithoutOne: the assertion the legs branch on.
func TestPendLedger_LedgerOfRefusesAStoreWithoutOne(t *testing.T) {
	if _, ok := LedgerOf(NewMemStore()); !ok {
		t.Fatal("LedgerOf(MemStore) = false — MemStore ships the ledger")
	}
	if l, ok := LedgerOf(storeWithoutLedger{inner: NewMemStore()}); ok || l != nil {
		t.Fatalf("LedgerOf(a Store without the ledger) = %v,%v", l, ok)
	}
}

// TestPendLedger_FallbackTransitionsWithoutALedger: with no PendLedger, today's
// transitions apply — and a DENIAL finalizes too, so a denied claim is never
// re-pended. An inquiry writes nothing at all (there is no ledger to write to), so
// the claim's state is exactly what the submit and update legs left.
func TestPendLedger_FallbackTransitionsWithoutALedger(t *testing.T) {
	s := storeWithoutLedger{inner: NewMemStore()}

	// A pend, then an amendment that binds it.
	if err := s.RecordPendedClaim("PCI-A", "corr-A"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.BeginClaimUpdate("PCI-A", "corr-A"); !ok || err != nil {
		t.Fatalf("begin = %v,%v", ok, err)
	}

	// The payer DENIES. FallbackDecision is what the leg calls on a store with no
	// ledger, for a denial exactly as for an approval.
	if err := FallbackDecision(s, "PCI-A", "corr-A"); err != nil {
		t.Fatal(err)
	}
	// A denied claim is never re-pended: nothing can bind it again.
	if ok, err := s.BeginClaimUpdate("PCI-A", "corr-A"); ok || err != nil {
		t.Fatalf("begin after a denial = %v,%v", ok, err)
	}

	// An inquiry changes no ledger state: there is no ledger to look up in, so the
	// leg has nothing to write, and a pend that is still open stays open.
	if err := s.RecordPendedClaim("PCI-B", "corr-B"); err != nil {
		t.Fatal(err)
	}
	if _, ok := LedgerOf(s); ok {
		t.Fatal("LedgerOf = true on the fallback store")
	}
	if ok, err := s.BeginClaimUpdate("PCI-B", "corr-B"); !ok || err != nil {
		t.Fatalf("the open pend was disturbed: %v,%v", ok, err)
	}
}

// TestPendLedger_RetentionPurgeIsBoundedPerCall: the lazy sweep drops rows whose
// last transition is older than PendLedgerRetention, AT MOST PendPurgeMax of them
// per call, and never touches a row inside the window.
func TestPendLedger_RetentionPurgeIsBoundedPerCall(t *testing.T) {
	d := newMemPendLedger()
	now := time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC)

	// 150 rows recorded one full retention period + a day ago, and one recorded now.
	old := now.Add(-PendLedgerRetention - 24*time.Hour)
	d.now = func() time.Time { return old }
	const stale = 150
	for i := 0; i < stale; i++ {
		if err := d.RecordPendedClaim("PCI-A", "corr-"+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	d.now = func() time.Time { return now }
	if _, err := d.RecordPendedKeyed("PCI-A", "corr-fresh", now, PendKeys{RequesterHolder: "provider-a"}); err != nil {
		t.Fatal(err)
	}
	// That write armed the sweep; it ran at most PendPurgeMax rows.
	left := pendedRows(d)
	if want := stale + 1 - PendPurgeMax; left != want {
		t.Fatalf("after one sweep %d rows remain, want %d (a sweep bounded to %d)", left, want, PendPurgeMax)
	}

	// Sweeps are rate-limited: another write in the same interval sweeps nothing.
	if _, err := d.RecordPendedKeyed("PCI-A", "corr-fresh-2", now, PendKeys{RequesterHolder: "provider-a"}); err != nil {
		t.Fatal(err)
	}
	if got := pendedRows(d); got != left+1 {
		t.Fatalf("a second write in the same interval swept rows: %d, want %d", got, left+1)
	}

	// Once the interval elapses the sweep resumes, and it stops at the fresh rows.
	for i := 0; i < 3; i++ {
		now = now.Add(PendPurgeInterval + time.Second)
		if _, err := d.RecordPendedKeyed("PCI-A", "corr-fresh", now, PendKeys{RequesterHolder: "provider-a"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := pendedRows(d); got != 2 {
		t.Fatalf("%d rows remain, want the 2 rows inside the retention window", got)
	}
	if _, ok, err := d.PendRecordOf("PCI-A", "corr-fresh"); !ok || err != nil {
		t.Fatalf("the fresh row was purged: %v,%v", ok, err)
	}
}

// TestPendLedger_KeyedLookupIgnoresAnOversizedProbe: an over-long key can never
// have been recorded (RecordPendedKeyed refuses it), so a lookup carrying one
// matches nothing rather than erroring — the mirror of the record-side guard.
func TestPendLedger_KeyedLookupIgnoresAnOversizedProbe(t *testing.T) {
	d := newMemPendLedger()
	if _, err := d.RecordPendedKeyed("PCI-A", "corr-A", time.Time{}, PendKeys{
		RequesterHolder: "provider-a",
		PreAuthRef:      "PA-1",
	}); err != nil {
		t.Fatal(err)
	}
	long := make([]byte, MaxPendKeyBytes+1)
	for i := range long {
		long[i] = 'x'
	}
	// An oversized strong key is dropped from the probe, so it searches by
	// nothing; an oversized weak key beside a real strong key confirms nothing,
	// and the claim is found by the strong key.
	m, err := d.LookupPended("provider-a", PendKeys{
		RequesterHolder:  "provider-a",
		ClaimResponseIDs: []string{string(long)},
	})
	if err != nil || m.Verdict != PendMatchNoStrongKey {
		t.Fatalf("lookup with an oversized strong probe = %+v,%v", m, err)
	}
	m, err = d.LookupPended("provider-a", PendKeys{
		RequesterHolder: "provider-a",
		PreAuthRef:      "PA-1",
		RequestIDs:      []string{string(long)},
	})
	if err != nil || m.Verdict != PendMatchFound {
		t.Fatalf("lookup with an oversized weak probe = %+v,%v", m, err)
	}
}

// TestPendLedger_ResetClearsTheLedger: the demo reset contract covers the new
// ledger state too (an unreset index would leak keys across a reset).
func TestPendLedger_ResetClearsTheLedger(t *testing.T) {
	d := NewMemStore()
	if _, err := testLedger(d).RecordPendedKeyed("PCI-A", "corr-A", time.Time{}, PendKeys{RequesterHolder: "provider-a", PreAuthRef: "PA-1"}); err != nil {
		t.Fatal(err)
	}
	d.Reset()
	if _, ok, err := testLedger(d).PendRecordOf("PCI-A", "corr-A"); ok || err != nil {
		t.Fatalf("PendRecordOf after Reset = %v,%v", ok, err)
	}
	m, err := testLedger(d).LookupPended("provider-a", PendKeys{RequesterHolder: "provider-a", PreAuthRef: "PA-1"})
	if err != nil || m.Verdict != PendMatchNone {
		t.Fatalf("lookup after Reset = %+v,%v", m, err)
	}
}

// TestPendLedger_AStrandedHoldLapses: an amendment hold whose gateway never
// released it lapses after PendInProgressStale. Before then a keyed or keyless
// re-pend records without moving it; after, the re-pend moves it and a new
// amendment binds.
func TestPendLedger_AStrandedHoldLapses(t *testing.T) {
	d := newMemPendLedger()
	now := time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return now }
	keys := PendKeys{RequesterHolder: "provider-a", PreAuthRef: "PA-1"}
	if _, err := d.RecordPendedKeyed("PCI-A", "corr-A", now, keys); err != nil {
		t.Fatal(err)
	}
	if ok, _, _ := d.BeginClaimUpdateReason("PCI-A", "corr-A"); !ok {
		t.Fatal("begin")
	}
	now = now.Add(PendInProgressStale - time.Second)
	if _, err := d.RecordPendedKeyed("PCI-A", "corr-A", now, keys); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordPendedClaim("PCI-A", "corr-A"); err != nil {
		t.Fatal(err)
	}
	if rec, _, _ := d.PendRecordOf("PCI-A", "corr-A"); rec.State != PendStateInProgress {
		t.Fatalf("a re-pend moved a live hold: %+v", rec)
	}
	now = now.Add(2 * time.Second) // past the bound from the Begin, not from the re-pends
	if err := d.RecordPendedClaim("PCI-A", "corr-A"); err != nil {
		t.Fatal(err)
	}
	if rec, _, _ := d.PendRecordOf("PCI-A", "corr-A"); rec.State != PendStatePended {
		t.Fatalf("a lapsed hold was not re-pended: %+v", rec)
	}
	if ok, _, _ := d.BeginClaimUpdateReason("PCI-A", "corr-A"); !ok {
		t.Fatal("a new amendment could not bind after the hold lapsed")
	}
}
