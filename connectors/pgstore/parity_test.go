package pgstore_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/pgstore"
	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
)

// storeUnderTest is the engine.Store contract both impls satisfy.
type storeUnderTest = engine.Store

func pgStore(t *testing.T) storeUnderTest {
	t.Helper()
	url := os.Getenv("SHN_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set SHN_TEST_DATABASE_URL to run Postgres parity tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	for _, tbl := range []string{"gw_auth_number", "gw_pended_claim", "gw_eob"} {
		_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS "+tbl+" CASCADE")
	}
	s, err := pgstore.NewPgStore(ctx, pool, "payer")
	if err != nil {
		t.Fatalf("NewPgStore: %v", err)
	}
	return s
}

// parityChecks runs the unique-id contract both impls must satisfy identically.
// NOTE: uses only DISTINCT eob ids — the stub appends duplicates on replay while
// PgStore dedupes (a deliberate divergence), so replay is intentionally NOT exercised.
func parityChecks(t *testing.T, s storeUnderTest) {
	t.Helper()
	// auth number
	if _, ok := s.AuthNumber("SR/1"); ok {
		t.Fatal("AuthNumber empty = ok")
	}
	if err := s.StoreAuthNumber("SR/1", "PA-1"); err != nil {
		t.Fatal(err)
	}
	if ref, ok := s.AuthNumber("SR/1"); !ok || ref != "PA-1" {
		t.Fatalf("AuthNumber = %q,%v", ref, ok)
	}
	// ledger
	if ok, _ := s.BeginClaimUpdate("p", "c"); ok {
		t.Fatal("Begin never-pended = true")
	}
	if err := s.RecordPendedClaim("p", "c"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.BeginClaimUpdate("p", "c"); !ok {
		t.Fatal("Begin pended = false")
	}
	if ok, _ := s.BeginClaimUpdate("p", "c"); ok {
		t.Fatal("Begin in-progress = true")
	}
	if err := s.FinalizeClaimUpdate("p", "c"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.BeginClaimUpdate("p", "c"); ok {
		t.Fatal("Begin after finalize = true")
	}
	// EOB (unique ids only — no replay, per the deliberate stub/pg dedupe divergence)
	_ = s.RecordEOB("pX", "eob-a", []byte(`{"n":1}`))
	_ = s.RecordEOB("pX", "eob-b", []byte(`{"n":2}`))
	if got, ok := s.EOBsForPatient("pX"); !ok || len(got) != 2 {
		t.Fatalf("EOBsForPatient = %d,%v", len(got), ok)
	}
	if b, ok := s.EOBByID("eob-a"); !ok || string(b) != `{"n":1}` {
		t.Fatalf("EOBByID = %q,%v", b, ok)
	}
}

func TestParity_Stub(t *testing.T) { parityChecks(t, engine.NewStubHolderData()) }
func TestParity_Pg(t *testing.T)   { parityChecks(t, pgStore(t)) }
