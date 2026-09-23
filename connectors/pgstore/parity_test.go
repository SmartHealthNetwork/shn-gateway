package pgstore_test

import (
	"context"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/pgstore"
	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
)

// storeUnderTest is the engine.Store contract both impls satisfy.
type storeUnderTest = engine.Store

func pgStore(t *testing.T) storeUnderTest {
	t.Helper()
	// Skips when SHN_TEST_DATABASE_URL is unset; drops every gw_* table and
	// recreates the schema (one shared table list — see pgstore.gwTables).
	pool := pgstore.TestPool(t)
	s, err := pgstore.NewPgStore(context.Background(), pool, "payer")
	if err != nil {
		t.Fatalf("NewPgStore: %v", err)
	}
	return s
}

// parityChecks runs the id contract both impls must satisfy identically —
// including a REPLAYED eob id, which the in-memory store used to double-count while
// PgStore deduped. That divergence is closed (MemStore holds the EOB ids in record
// order and the bytes by id, exactly as the durable store's primary key does), so
// the replay is exercised here rather than avoided: a mirror that is more
// permissive than the oracle is hermetic green and live red.
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
	// EOB
	_ = s.RecordEOB("pX", "eob-a", []byte(`{"n":1}`))
	_ = s.RecordEOB("pX", "eob-b", []byte(`{"n":2}`))
	if got, ok := s.EOBsForPatient("pX"); !ok || len(got) != 2 {
		t.Fatalf("EOBsForPatient = %d,%v", len(got), ok)
	}
	if b, ok := s.EOBByID("eob-a"); !ok || string(b) != `{"n":1}` {
		t.Fatalf("EOBByID = %q,%v", b, ok)
	}
	// A replayed id is ONE EOB on both backends, carrying the newest bytes.
	_ = s.RecordEOB("pX", "eob-a", []byte(`{"n":3}`))
	if got, ok := s.EOBsForPatient("pX"); !ok || len(got) != 2 {
		t.Fatalf("EOBsForPatient after a replay = %d,%v, want 2", len(got), ok)
	}
	if b, ok := s.EOBByID("eob-a"); !ok || string(b) != `{"n":3}` {
		t.Fatalf("EOBByID after a replay = %q,%v", b, ok)
	}
}

func TestParity_Mem(t *testing.T) { parityChecks(t, engine.NewMemStore()) }
func TestParity_Pg(t *testing.T)  { parityChecks(t, pgStore(t)) }

func eobSubjectOwnershipChecks(t *testing.T, s storeUnderTest) {
	t.Helper()
	first := []byte(`{"resourceType":"ExplanationOfBenefit","id":"shared","patient":{"reference":"Patient/A"}}`)
	foreign := []byte(`{"resourceType":"ExplanationOfBenefit","id":"shared","patient":{"reference":"Patient/B"}}`)
	if err := s.RecordEOB("pci:A", "shared", first); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordEOB("pci:B", "shared", foreign); err == nil {
		t.Fatal("a second patient replaced an EOB ID already owned by the first")
	}
	if got, ok := s.EOBsForPatient("pci:A"); !ok || len(got) != 1 || string(got[0]) != string(first) {
		t.Fatalf("first patient's EOB changed after refused replacement: %s, found=%v", got, ok)
	}
	if got, ok := s.EOBsForPatient("pci:B"); ok || len(got) != 0 {
		t.Fatalf("second patient's refused EOB became visible: %s, found=%v", got, ok)
	}
	if got, ok := s.EOBByID("shared"); !ok || string(got) != string(first) {
		t.Fatalf("instance read changed after refused replacement: %s, found=%v", got, ok)
	}
}

func TestEOBSubjectOwnership_Mem(t *testing.T) { eobSubjectOwnershipChecks(t, engine.NewMemStore()) }
func TestEOBSubjectOwnership_Pg(t *testing.T)  { eobSubjectOwnershipChecks(t, pgStore(t)) }
