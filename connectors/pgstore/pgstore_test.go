package pgstore

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("SHN_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set SHN_TEST_DATABASE_URL to run Postgres integration tests")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// freshStore drops the gw_* tables and returns a store bound to holderID.
func freshStore(t *testing.T, holderID string) *PgStore {
	t.Helper()
	pool := openTestPool(t)
	ctx := context.Background()
	for _, tbl := range []string{"gw_auth_number", "gw_pended_claim", "gw_eob"} {
		if _, err := pool.Exec(ctx, "DROP TABLE IF EXISTS "+tbl+" CASCADE"); err != nil {
			t.Fatalf("drop %s: %v", tbl, err)
		}
	}
	s, err := NewPgStore(ctx, pool, holderID)
	if err != nil {
		t.Fatalf("NewPgStore: %v", err)
	}
	return s
}

func TestAuthNumber_RoundTrip(t *testing.T) {
	s := freshStore(t, "payer")
	if _, ok := s.AuthNumber("SR/1"); ok {
		t.Fatal("AuthNumber on empty store returned ok=true")
	}
	if err := s.StoreAuthNumber("SR/1", "PA-abc"); err != nil {
		t.Fatal(err)
	}
	if ref, ok := s.AuthNumber("SR/1"); !ok || ref != "PA-abc" {
		t.Fatalf("AuthNumber = %q,%v; want PA-abc,true", ref, ok)
	}
	// upsert: re-store overwrites.
	if err := s.StoreAuthNumber("SR/1", "PA-xyz"); err != nil {
		t.Fatal(err)
	}
	if ref, _ := s.AuthNumber("SR/1"); ref != "PA-xyz" {
		t.Fatalf("after re-store AuthNumber = %q; want PA-xyz", ref)
	}
}

func TestLedger_StateMachine(t *testing.T) {
	s := freshStore(t, "payer")
	// never pended → Begin false
	if ok, err := s.BeginClaimUpdate("pci1", "c1"); err != nil || ok {
		t.Fatalf("Begin on never-pended = %v,%v; want false,nil", ok, err)
	}
	if err := s.RecordPendedClaim("pci1", "c1"); err != nil {
		t.Fatal(err)
	}
	// pended → Begin true
	if ok, _ := s.BeginClaimUpdate("pci1", "c1"); !ok {
		t.Fatal("Begin on pended = false; want true")
	}
	// in-progress → Begin false (already claimed)
	if ok, _ := s.BeginClaimUpdate("pci1", "c1"); ok {
		t.Fatal("Begin on in-progress = true; want false")
	}
	// release → Begin true again
	if err := s.ReleaseClaimUpdate("pci1", "c1"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.BeginClaimUpdate("pci1", "c1"); !ok {
		t.Fatal("Begin after release = false; want true")
	}
	// finalize → Begin false (replay protection)
	if err := s.FinalizeClaimUpdate("pci1", "c1"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.BeginClaimUpdate("pci1", "c1"); ok {
		t.Fatal("Begin after finalize = true; want false (replay)")
	}
}

func TestLedger_ConcurrentBeginExactlyOne(t *testing.T) {
	s := freshStore(t, "payer")
	if err := s.RecordPendedClaim("pci2", "c2"); err != nil {
		t.Fatal(err)
	}
	var wins int32
	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, err := s.BeginClaimUpdate("pci2", "c2"); err == nil && ok {
				atomic.AddInt32(&wins, 1)
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&wins); got != 1 {
		t.Fatalf("concurrent BeginClaimUpdate winners = %d, want exactly 1 (atomic test-and-set)", got)
	}
}

func TestHolderIsolation(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	for _, tbl := range []string{"gw_auth_number", "gw_pended_claim", "gw_eob"} {
		_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS "+tbl+" CASCADE")
	}
	payer, err := NewPgStore(ctx, pool, "payer")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := NewPgStore(ctx, pool, "provider")
	if err != nil {
		t.Fatal(err)
	}
	// identical keys, different holders — no collision.
	_ = payer.StoreAuthNumber("SR/x", "PA-payer")
	_ = provider.StoreAuthNumber("SR/x", "PA-provider")
	if ref, _ := payer.AuthNumber("SR/x"); ref != "PA-payer" {
		t.Fatalf("payer sees %q; want PA-payer", ref)
	}
	if ref, _ := provider.AuthNumber("SR/x"); ref != "PA-provider" {
		t.Fatalf("provider sees %q; want PA-provider", ref)
	}
	_ = payer.RecordPendedClaim("pci", "c")
	if ok, _ := provider.BeginClaimUpdate("pci", "c"); ok {
		t.Fatal("provider claimed the payer's pended claim — holder isolation broken")
	}
}

func TestEOB_RecordReadDedupe(t *testing.T) {
	s := freshStore(t, "payer")
	if _, ok := s.EOBsForPatient("pciX"); ok {
		t.Fatal("EOBsForPatient on empty = ok=true")
	}
	if err := s.RecordEOB("pciX", "eob-1", []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordEOB("pciX", "eob-2", []byte(`{"a":2}`)); err != nil {
		t.Fatal(err)
	}
	got, ok := s.EOBsForPatient("pciX")
	if !ok || len(got) != 2 {
		t.Fatalf("EOBsForPatient = %d rows, ok=%v; want 2,true", len(got), ok)
	}
	if b, ok := s.EOBByID("eob-1"); !ok || string(b) != `{"a":1}` {
		t.Fatalf("EOBByID(eob-1) = %q,%v", b, ok)
	}
	// replayed same eob_id → still ONE row for the patient (dedupe; the spec's
	// deliberate more-correct divergence from the stub's duplicate-append).
	if err := s.RecordEOB("pciX", "eob-1", []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	got2, _ := s.EOBsForPatient("pciX")
	if len(got2) != 2 {
		t.Fatalf("after replay EOBsForPatient = %d rows; want 2 (deduped, not 3)", len(got2))
	}
}

func TestEnsureSchema_Idempotent(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	if err := EnsureSchema(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSchema(ctx, pool); err != nil { // second call must not error
		t.Fatalf("EnsureSchema not idempotent: %v", err)
	}
}

// TestEnsureSchema_Concurrent reproduces the four-gateways-share-one-DB startup race:
// concurrent EnsureSchema calls must ALL succeed. Without the advisory lock,
// CREATE TABLE IF NOT EXISTS races in pg_type and some fail with SQLSTATE 23505
// (which crash-looped the cloud/compose gateways before the lock was added).
func TestEnsureSchema_Concurrent(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	for _, tbl := range []string{"gw_auth_number", "gw_pended_claim", "gw_eob"} {
		if _, err := pool.Exec(ctx, "DROP TABLE IF EXISTS "+tbl+" CASCADE"); err != nil {
			t.Fatalf("drop %s: %v", tbl, err)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- EnsureSchema(ctx, pool) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent EnsureSchema failed (CREATE TABLE race): %v", err)
		}
	}
}
