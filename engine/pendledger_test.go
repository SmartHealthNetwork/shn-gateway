package engine

// pendledger_test.go holds the ledger rows that need in-package access: the
// injected clock behind the bounded retention purge, the structural pin on
// PendKeys, and the ledger-LESS fallback (a Store that deliberately does not
// implement PendLedger). The backend-agnostic state-machine and lookup table lives
// in gateway/connectors/pgstore/pendledger_test.go, where it runs against MemStore
// AND PgStore from one definition.

import (
	"errors"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"
)

// pendedRows counts the ledger's rows. In-package, under the store's own lock, so
// the retention rows assert what the sweep actually left behind.
func pendedRows(d *MemStore) int {
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
func (s storeWithoutLedger) RecordEOB(pci, id string, b []byte) error {
	return s.inner.RecordEOB(pci, id, b)
}
func (s storeWithoutLedger) EOBsForPatient(pci string) ([][]byte, bool) {
	return s.inner.EOBsForPatient(pci)
}
func (s storeWithoutLedger) EOBByID(id string) ([]byte, bool) { return s.inner.EOBByID(id) }

var _ Store = storeWithoutLedger{}

// TestPendKeys_FieldSetIsPinned: the ledger never matches a pended claim on the
// INQUIRY's own Claim.identifier — and matches
// nothing that is not a lookup key at all. The ledger cannot match on a fact it has
// no field for, so the field set is the enforcement: every field here is a key, a
// new field is a deliberate reviewed change, and a NON-key input to a ledger write
// (the payer's ClaimResponse.created, for one) is a parameter of the call rather
// than a field of this struct.
func TestPendKeys_FieldSetIsPinned(t *testing.T) {
	want := []string{"ClaimResponseIDs", "ItemTraceNumbers", "PreAuthRef", "RequestIDs", "RequesterHolder"}
	rt := reflect.TypeOf(PendKeys{})
	var got []string
	for i := 0; i < rt.NumField(); i++ {
		got = append(got, rt.Field(i).Name)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PendKeys fields = %v, want %v — a fact the ledger must never match on "+
			"(the inquiry's own Claim.identifier) or a non-key input to a write would arrive as a new field", got, want)
	}
}

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
	d := NewMemStore()
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

// TestPendLedger_RetentionIsSixMonths pins the disclosed period: PAS keeps a pended
// authorization answerable for at least six months, so the ledger must not purge
// earlier than the LONGEST six-month span.
func TestPendLedger_RetentionIsSixMonths(t *testing.T) {
	from := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	longest := from.AddDate(0, 6, 0).Sub(from) // Mar 1 → Sep 1 = 184 days
	if PendLedgerRetention < longest {
		t.Fatalf("PendLedgerRetention = %s, shorter than the longest six-month span (%s)", PendLedgerRetention, longest)
	}
}

// TestPendLedger_KeyedLookupIgnoresAnOversizedProbe: an over-long key can never
// have been recorded (RecordPendedKeyed refuses it), so a lookup carrying one
// matches nothing rather than erroring — the mirror of the record-side guard.
func TestPendLedger_KeyedLookupIgnoresAnOversizedProbe(t *testing.T) {
	d := NewMemStore()
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
	_, _, found, ambiguous, err := d.LookupPended("provider-a", PendKeys{
		RequesterHolder: "provider-a",
		RequestIDs:      []string{string(long)},
	})
	if err != nil || found || ambiguous {
		t.Fatalf("lookup with an oversized probe = %v,%v,%v", found, ambiguous, err)
	}
}

// TestPendLedger_ResetClearsTheLedger: the demo reset contract covers the new
// ledger state too (an unreset index would leak keys across a reset).
func TestPendLedger_ResetClearsTheLedger(t *testing.T) {
	d := NewMemStore()
	if _, err := d.RecordPendedKeyed("PCI-A", "corr-A", time.Time{}, PendKeys{RequesterHolder: "provider-a", PreAuthRef: "PA-1"}); err != nil {
		t.Fatal(err)
	}
	d.Reset()
	if _, ok, err := d.PendRecordOf("PCI-A", "corr-A"); ok || err != nil {
		t.Fatalf("PendRecordOf after Reset = %v,%v", ok, err)
	}
	_, _, found, _, err := d.LookupPended("provider-a", PendKeys{RequesterHolder: "provider-a", PreAuthRef: "PA-1"})
	if err != nil || found {
		t.Fatalf("lookup after Reset = %v,%v", found, err)
	}
}

// TestPendStateMachine_HelpersAreTotal: the three backends share these pure
// helpers, so the transition table is asserted once, here, including the states a
// backend must never invent.
func TestPendStateMachine_HelpersAreTotal(t *testing.T) {
	for _, row := range []struct {
		name  string
		cur   PendState
		found bool
		next  PendState
		ok    bool
		why   PendRefusal
	}{
		{"absent", "", false, "", false, PendRefusalNotPended},
		{"pended", PendStatePended, true, PendStateInProgress, true, PendRefusalNone},
		{"in progress", PendStateInProgress, true, PendStateInProgress, false, PendRefusalInProgress},
		{"decided", PendStateDecided, true, PendStateDecided, false, PendRefusalDecided},
	} {
		now := time.Unix(5000, 0).UTC()
		next, ok, why := PendBegin(PendRecord{State: row.cur, LastTransition: now}, row.found, now)
		if next != row.next || ok != row.ok || why != row.why {
			t.Errorf("%s: PendBegin = %q,%v,%q — want %q,%v,%q", row.name, next, ok, why, row.next, row.ok, row.why)
		}
	}

	// An amendment's hold lapses after PendInProgressStale: a stranded row (its
	// gateway stopped before releasing it) binds again and is re-pended again.
	// Inside the bound it does neither, and a re-pend leaves its time alone.
	held := time.Unix(5000, 0).UTC()
	hold := PendRecord{State: PendStateInProgress, LastTransition: held}
	inside, lapsed := held.Add(PendInProgressStale-time.Second), held.Add(PendInProgressStale)
	if _, ok, why := PendBegin(hold, true, inside); ok || why != PendRefusalInProgress {
		t.Errorf("a live hold was bound again: %v %q", ok, why)
	}
	if next, ok, _ := PendBegin(hold, true, lapsed); !ok || next != PendStateInProgress {
		t.Errorf("a lapsed hold was not bound: %v %q", ok, next)
	}
	if next, tr := PendRePend(hold, true, inside, inside); next.State != PendStateInProgress || !next.LastTransition.Equal(held) || tr.Changed {
		t.Errorf("a re-pend moved or refreshed a live hold: %+v %+v", next, tr)
	}
	if next, tr := PendRePend(hold, true, lapsed, lapsed); next.State != PendStatePended || !next.LastTransition.Equal(lapsed) || !tr.Changed {
		t.Errorf("a re-pend left a lapsed hold in place: %+v %+v", next, tr)
	}

	// ONE meaning for "undated", across BOTH timestamp rules: not later. An
	// answer whose date is absent does not displace what the ledger already
	// holds — it neither reopens a decision nor replaces one.
	decided := PendRecord{State: PendStateDecided, Outcome: PendOutcomeApproved, DecidedAt: time.Unix(1000, 0).UTC()}
	for _, row := range []struct {
		name string
		when time.Time
	}{
		{"undated", time.Time{}},
		{"the same instant", decided.DecidedAt},
		{"earlier", decided.DecidedAt.Add(-time.Hour)},
	} {
		next, tr := PendRePend(decided, true, row.when, time.Unix(9000, 0).UTC())
		if next.State != PendStateDecided || tr.Event != DecisionStaleAnswerEvent || tr.Changed {
			t.Errorf("a %s re-pend of a decided claim = %+v,%+v — want the decision to stand", row.name, next, tr)
		}
		if next.Outcome != PendOutcomeApproved || !next.DecidedAt.Equal(decided.DecidedAt) {
			t.Errorf("a %s re-pend disturbed the decision: %+v", row.name, next)
		}

		dec, dtr := PendDecide(decided, true, PendOutcomeDenied, row.when)
		wantChanged := row.name == "the same instant" // an exact tie falls to arrival order
		if dtr.Event != DecisionConflictEvent || dtr.Changed != wantChanged {
			t.Errorf("a %s conflicting decision = %+v", row.name, dtr)
		}
		if !wantChanged && (dec.Outcome != PendOutcomeApproved || !dec.DecidedAt.Equal(decided.DecidedAt)) {
			t.Errorf("a %s conflicting decision displaced the recorded one: %+v", row.name, dec)
		}
	}
	// A LATER re-pend is the one case that reopens a decision.
	if next, tr := PendRePend(decided, true, decided.DecidedAt.Add(time.Hour), time.Unix(9000, 0).UTC()); next.State != PendStatePended || tr.Event != DecisionSupersededEvent || !tr.Changed {
		t.Errorf("a later re-pend of a decided claim = %+v,%+v", next, tr)
	}
}

// TestEOBRecord_ValidateRejections is the isolated rejection row for the guard the
// atomic write depends on.
func TestEOBRecord_ValidateRejections(t *testing.T) {
	good := &EOBRecord{SubjectPCI: "PCI-A", EOBID: "eob-1", JSON: []byte(`{}`)}
	if err := good.Validate("PCI-A"); err != nil {
		t.Fatalf("a well-formed EOB was refused: %v", err)
	}
	for _, bad := range []struct {
		name string
		rec  *EOBRecord
	}{
		{"no id", &EOBRecord{SubjectPCI: "PCI-A", JSON: []byte(`{}`)}},
		{"no bytes", &EOBRecord{SubjectPCI: "PCI-A", EOBID: "eob-1"}},
		{"another subject", &EOBRecord{SubjectPCI: "PCI-B", EOBID: "eob-1", JSON: []byte(`{}`)}},
		{"no subject", &EOBRecord{EOBID: "eob-1", JSON: []byte(`{}`)}},
	} {
		if err := bad.rec.Validate("PCI-A"); !errors.Is(err, ErrPendEOBInvalid) {
			t.Errorf("%s: err = %v, want ErrPendEOBInvalid", bad.name, err)
		}
	}
	// A nil EOB is "no EOB with this decision", not an invalid one.
	var none *EOBRecord
	if err := none.Validate("PCI-A"); err != nil {
		t.Errorf("a nil EOB was refused: %v", err)
	}
}

// TestPendLedger_AStrandedHoldLapses: an amendment hold whose gateway never
// released it lapses after PendInProgressStale. Before then a keyed or keyless
// re-pend records without moving it; after, the re-pend moves it and a new
// amendment binds.
func TestPendLedger_AStrandedHoldLapses(t *testing.T) {
	d := NewMemStore()
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
