//go:build !shn_coreonly

package pgstore_test

import (
	"context"
	"errors"
	"strings"
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

// parityChecks runs the Store contract both impls must satisfy identically: a
// mirror that is more permissive than the oracle is hermetic green and live red.
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
}

func TestParity_Mem(t *testing.T) { parityChecks(t, engine.NewMemStore()) }
func TestParity_Pg(t *testing.T)  { parityChecks(t, pgStore(t)) }

// cdexParityChecks runs the CDex request record's contract (engine.CDexRequestStore)
// that both backends must answer identically. The rows that need the injected
// clock, the table or a second store are in cdexrequest_test.go.
func cdexParityChecks(t *testing.T, s engine.CDexRequestStore) {
	t.Helper()
	k := engine.CDexRequestKey{Counterparty: "payer-a", System: "urn:payer-a:tracking", Value: "trk-1"}
	recA := engine.CDexRequestRecord{Member: "MBR-1", PCI: "pci:one"}
	look := func(k engine.CDexRequestKey, want engine.CDexRequestLookup, wantRec engine.CDexRequestRecord) {
		t.Helper()
		rec, got, err := s.LookupCDexRequest(k)
		if err != nil || got != want || rec != wantRec {
			t.Fatalf("Lookup(%+v) = %+v, %v, %v; want %+v, %v, nil", k, rec, got, err, wantRec, want)
		}
	}
	record := func(k engine.CDexRequestKey, rec engine.CDexRequestRecord) {
		t.Helper()
		if err := s.RecordCDexRequest(k, rec); err != nil {
			t.Fatalf("Record(%+v): %v", k, err)
		}
	}
	look(k, engine.CDexRequestNone, engine.CDexRequestRecord{})
	record(k, recA)
	look(k, engine.CDexRequestFound, recA)
	record(k, recA) // the same record again changes nothing
	look(k, engine.CDexRequestFound, recA)

	other := k
	other.Counterparty = "payer-b"
	look(other, engine.CDexRequestOtherCounterparty, engine.CDexRequestRecord{})
	unrelated := k
	unrelated.Value = "trk-unrelated"
	look(unrelated, engine.CDexRequestNone, engine.CDexRequestRecord{})

	// Another member conflicts; so does the same member with another patient;
	// and a conflicted key stays conflicted whatever is recorded next.
	record(k, engine.CDexRequestRecord{Member: "MBR-2", PCI: "pci:one"})
	look(k, engine.CDexRequestConflicted, engine.CDexRequestRecord{})
	record(k, recA)
	look(k, engine.CDexRequestConflicted, engine.CDexRequestRecord{})
	k2 := k
	k2.Value = "trk-2"
	record(k2, recA)
	record(k2, engine.CDexRequestRecord{Member: "MBR-1", PCI: "pci:other"})
	look(k2, engine.CDexRequestConflicted, engine.CDexRequestRecord{})

	for name, bad := range map[string]engine.CDexRequestKey{
		"no participant":    {System: "urn:sys", Value: "t"},
		"no tracking value": {Counterparty: "payer-a", System: "urn:sys"},
		"one byte too long": {Counterparty: "payer-a", Value: strings.Repeat("v", engine.MaxCDexTrackingBytes+1)},
	} {
		if err := s.RecordCDexRequest(bad, recA); !errors.Is(err, engine.ErrCDexRequestKey) {
			t.Errorf("%s: Record err = %v, want ErrCDexRequestKey", name, err)
		}
		if _, got, err := s.LookupCDexRequest(bad); !errors.Is(err, engine.ErrCDexRequestKey) || got != engine.CDexRequestNone {
			t.Errorf("%s: Lookup = %v, %v, want none, ErrCDexRequestKey", name, got, err)
		}
	}
	atBound := engine.CDexRequestKey{Counterparty: "payer-a", Value: strings.Repeat("v", engine.MaxCDexTrackingBytes)}
	record(atBound, recA)
	look(atBound, engine.CDexRequestFound, recA)
}

func TestCDexParity_Mem(t *testing.T) { cdexParityChecks(t, engine.NewMemCDexRequests()) }
func TestCDexParity_Pg(t *testing.T) {
	cdexParityChecks(t, pgStore(t).(*pgstore.PgStore))
}
