// memstore_test.go — the Store-half rows, re-homed verbatim from the retired
// holderdata_test.go when StubHolderData split (§4.1). Same assertions, same
// concurrency shapes; only the constructor changed (NewStubHolderData → NewMemStore),
// because the auth-number store and the pended-claim ledger never had anything to
// do with the deleted persona census. The pended-claim ledger's rows are PA's
// claim keeping and sit in memstore_claimkeeper_test.go.
package engine

import (
	"sync"
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
