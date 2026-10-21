// memstore_test.go — the Store-half rows, re-homed verbatim from the retired
// holderdata_test.go when StubHolderData split (§4.1). Same assertions, same
// concurrency shapes; only the constructor changed (NewStubHolderData → NewMemStore),
// because the auth-number store and the pended-claim ledger never had anything to
// do with the deleted persona census.
package engine

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestMemStore_AuthNumber proves the auth-number store round-trips and
// that an absent serviceRequestRef yields found=false.
func TestMemStore_AuthNumber(t *testing.T) {
	d := NewMemStore()

	t.Run("store then read round-trip", func(t *testing.T) {
		if err := d.StoreAuthNumber("ServiceRequest/sr-1", "PA-12345"); err != nil {
			t.Fatalf("StoreAuthNumber: %v", err)
		}
		got, found := d.AuthNumber("ServiceRequest/sr-1")
		if !found {
			t.Fatal("expected found=true after StoreAuthNumber")
		}
		if got != "PA-12345" {
			t.Errorf("AuthNumber = %q, want PA-12345", got)
		}
	})

	t.Run("absent ref not found", func(t *testing.T) {
		if _, found := d.AuthNumber("ServiceRequest/absent"); found {
			t.Error("expected found=false for absent ref")
		}
	})
}

// TestMemStore_AuthNumberConcurrent is a -race smoke test for the
// auth-number store under concurrent Store/AuthNumber access.
func TestMemStore_AuthNumberConcurrent(t *testing.T) {
	d := NewMemStore()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		ref := "ServiceRequest/sr-" + strconvI(i)
		go func() {
			defer wg.Done()
			_ = d.StoreAuthNumber(ref, "PA-"+strconvI(i))
		}()
		go func() {
			defer wg.Done()
			d.AuthNumber(ref)
		}()
	}
	wg.Wait()
}

func strconvI(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{digits[i%10]}, b...)
		i /= 10
	}
	return string(b)
}

// TestPendedClaimLedger verifies the state machine: record(pended) → begin(claim) →
// finalize(gone) and the release path (FR-21/FR-6).
func TestPendedClaimLedger(t *testing.T) {
	d := NewMemStore()
	// No record yet → cannot begin.
	ok, err := d.BeginClaimUpdate("pci:1", "corr-1")
	if err != nil {
		t.Fatalf("BeginClaimUpdate error: %v", err)
	}
	if ok {
		t.Fatal("BeginClaimUpdate must be false with no prior pend")
	}
	if err := d.RecordPendedClaim("pci:1", "corr-1"); err != nil {
		t.Fatalf("RecordPendedClaim error: %v", err)
	}
	// Mismatched key must not be claimable.
	ok1, _ := d.BeginClaimUpdate("pci:2", "corr-1")
	ok2, _ := d.BeginClaimUpdate("pci:1", "corr-2")
	if ok1 || ok2 {
		t.Fatal("ledger is keyed on (pci, correlation) — mismatched key must not begin")
	}
	// Begin claims it; a SECOND begin (concurrent/replay) fails while in-progress.
	ok, err = d.BeginClaimUpdate("pci:1", "corr-1")
	if err != nil {
		t.Fatalf("BeginClaimUpdate error: %v", err)
	}
	if !ok {
		t.Fatal("first BeginClaimUpdate on a pended claim must succeed")
	}
	ok, _ = d.BeginClaimUpdate("pci:1", "corr-1")
	if ok {
		t.Fatal("second BeginClaimUpdate while in-progress must fail")
	}
	// Release returns it to pended → claimable again (retry after insufficient).
	if err := d.ReleaseClaimUpdate("pci:1", "corr-1"); err != nil {
		t.Fatalf("ReleaseClaimUpdate error: %v", err)
	}
	ok, err = d.BeginClaimUpdate("pci:1", "corr-1")
	if err != nil {
		t.Fatalf("BeginClaimUpdate error: %v", err)
	}
	if !ok {
		t.Fatal("after release, the claim must be claimable again")
	}
	// Finalize removes it → not claimable (replay protection).
	if err := d.FinalizeClaimUpdate("pci:1", "corr-1"); err != nil {
		t.Fatalf("FinalizeClaimUpdate error: %v", err)
	}
	ok, _ = d.BeginClaimUpdate("pci:1", "corr-1")
	if ok {
		t.Fatal("finalized claim must not be claimable (replay protection)")
	}
}

// TestPendedClaim_AtomicUnderConcurrency (race regression, #3): with N goroutines
// racing to claim the SAME pended claim, EXACTLY ONE wins. Run under -race.
func TestPendedClaim_AtomicUnderConcurrency(t *testing.T) {
	d := NewMemStore()
	if err := d.RecordPendedClaim("pci:1", "corr-1"); err != nil {
		t.Fatalf("RecordPendedClaim: %v", err)
	}

	const n = 64
	var wins int64
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			ok, _ := d.BeginClaimUpdate("pci:1", "corr-1")
			if ok {
				atomic.AddInt64(&wins, 1)
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("concurrent BeginClaimUpdate winners = %d, want exactly 1", wins)
	}
}

// TestMemStore_Reset (#4): Reset clears mutable state — auth numbers and the
// pended-claim ledger — so the demo returns to clean synthetic state.
func TestMemStore_Reset(t *testing.T) {
	d := NewMemStore()
	if err := d.StoreAuthNumber("ServiceRequest/sr-1", "PA-abc"); err != nil {
		t.Fatalf("StoreAuthNumber: %v", err)
	}
	if err := d.RecordPendedClaim("pci:1", "corr-1"); err != nil {
		t.Fatalf("RecordPendedClaim: %v", err)
	}

	d.Reset()

	if _, found := d.AuthNumber("ServiceRequest/sr-1"); found {
		t.Error("Reset must clear the auth-number store")
	}
	ok, _ := d.BeginClaimUpdate("pci:1", "corr-1")
	if ok {
		t.Error("Reset must clear the pended-claim ledger")
	}
	// Reset is idempotent and leaves a usable store: a fresh record after it works.
	if err := d.RecordPendedClaim("pci:1", "corr-2"); err != nil {
		t.Fatalf("RecordPendedClaim after Reset: %v", err)
	}
	if ok, _ := d.BeginClaimUpdate("pci:1", "corr-2"); !ok {
		t.Error("the store must still accept new state after Reset")
	}
}
