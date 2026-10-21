package engine

import (
	"strings"
	"testing"
)

// testLedger is s's pend ledger: every store these tests build keeps one.
func testLedger(s Store) PendLedger {
	l, ok := LedgerOf(s)
	if !ok {
		panic("the test's store keeps no pend ledger")
	}
	return l
}

// testContinuations is s's continuation store: every store these tests build
// keeps one.
func testContinuations(s Store) ContinuationStore {
	c, ok := ContinuationsOf(s)
	if !ok {
		panic("the test's store keeps no continuation store")
	}
	return c
}

// TestMemStore_ServesThePAState: the in-memory Store, and a type embedding one
// (the engine's census fixture, as internal/fixturesor's SoR does), answer
// LedgerOf and ContinuationsOf with the PA workstream's own state, every time the
// same: a pend the Store seam records is the ledger's row, and a continuation one
// lookup writes the next reads. A store that answered false here would send the
// PAS legs to FallbackDecision and the originator to the gateway's fallback
// continuation store without a word, so this row is what fails instead.
func TestMemStore_ServesThePAState(t *testing.T) {
	for name, s := range map[string]Store{"MemStore": NewMemStore(), "embedded": newCensusSoR()} {
		t.Run(name, func(t *testing.T) {
			if _, ok := LedgerOf(s); !ok {
				t.Fatal("LedgerOf = false: the in-memory Store must keep the PA pend ledger")
			}
			cs, ok := ContinuationsOf(s)
			if !ok {
				t.Fatal("ContinuationsOf = false: the in-memory Store must keep the PA continuation store")
			}
			if DurableContinuations(cs) {
				t.Error("the in-memory continuation store reports itself durable")
			}
			if err := s.RecordPendedClaim("PCI-A", "corr-A"); err != nil {
				t.Fatal(err)
			}
			if rec, found, err := testLedger(s).PendRecordOf("PCI-A", "corr-A"); err != nil || !found || rec.State != PendStatePended {
				t.Fatalf("the Store seam's pend is not the ledger's row: %+v found=%v err=%v", rec, found, err)
			}
			put, err := cs.PutContinuation(sampleContinuation("provider-a"))
			if err != nil {
				t.Fatal(err)
			}
			if _, lookup, err := testContinuations(s).ReadContinuation("provider-a", put.ID); err != nil || lookup != ContinuationFound {
				t.Fatalf("a second lookup reads another continuation store: %v, %v", lookup, err)
			}
		})
	}
}

// TestMemStore_WithoutThePAStateServesNone is the rejection row for the one
// above: a memory store that has lost the PA workstream's state answers false
// from both lookups, so that row cannot pass on it.
func TestMemStore_WithoutThePAStateServesNone(t *testing.T) {
	d := NewMemStore()
	delete(d.states, workstreamPA)
	if _, ok := LedgerOf(d); ok {
		t.Error("LedgerOf = true on a memory store without the PA state")
	}
	if _, ok := ContinuationsOf(d); ok {
		t.Error("ContinuationsOf = true on a memory store without the PA state")
	}
}

// TestMemStore_ResetClearsThePAState: Reset clears the auth numbers, the ledger
// and the continuations together (the demo reset contract), and a continuation
// minted before it reads as lost.
func TestMemStore_ResetClearsThePAState(t *testing.T) {
	d := NewMemStore()
	if err := d.StoreAuthNumber("ServiceRequest/sr-1", "AUTH-1"); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordPendedClaim("PCI-A", "corr-A"); err != nil {
		t.Fatal(err)
	}
	put, err := testContinuations(d).PutContinuation(sampleContinuation("provider-a"))
	if err != nil {
		t.Fatal(err)
	}
	d.Reset()
	if _, ok := d.AuthNumber("ServiceRequest/sr-1"); ok {
		t.Error("an auth number survived Reset")
	}
	if _, found, err := testLedger(d).PendRecordOf("PCI-A", "corr-A"); err != nil || found {
		t.Errorf("a ledger row survived Reset: found=%v err=%v", found, err)
	}
	if _, lookup, err := testContinuations(d).ReadContinuation("provider-a", put.ID); err != nil || lookup != ContinuationLost {
		t.Errorf("a continuation minted before Reset reads %v, %v; want lost", lookup, err)
	}
}

// TestGateway_FallbackContinuationsAreOneStore: a configured Store that keeps no
// continuation store leaves the gateway its own in-memory one, built once: an id
// one request mints, the next reads, and the gateway says it is not durable.
func TestGateway_FallbackContinuationsAreOneStore(t *testing.T) {
	gw := &Gateway{cfg: Config{Store: storeWithoutLedger{inner: NewMemStore()}}}
	if _, ok := ContinuationsOf(gw.cfg.Store); ok {
		t.Fatal("the row's Store keeps a continuation store; it must keep none")
	}
	first, ok := gw.continuations()
	if !ok {
		t.Fatal("continuations() = none")
	}
	put, err := first.PutContinuation(sampleContinuation("provider-a"))
	if err != nil {
		t.Fatal(err)
	}
	second, _ := gw.continuations()
	if _, lookup, err := second.ReadContinuation("provider-a", put.ID); err != nil || lookup != ContinuationFound {
		t.Fatalf("the fallback store changed between requests: %v, %v", lookup, err)
	}
	if gw.continuationsAreDurable() {
		t.Error("the fallback continuation store reports itself durable")
	}
}

// partialLedger embeds an in-memory Store and declares one ledger method of
// its own: not the whole PendLedger.
type partialLedger struct{ *MemStore }

func (partialLedger) LookupPended(string, PendKeys) (PendMatch, error) { return PendMatch{}, nil }

// partialContinuations embeds an in-memory Store and declares one continuation
// method of its own: not the whole ContinuationStore.
type partialContinuations struct{ *MemStore }

func (partialContinuations) Durable() bool { return true }

// TestNew_RefusesAPartialOverride: a Store that embeds the in-memory Store and
// declares only part of the ledger or the continuation store would have its
// own methods skipped for the embedded state's. The gateway refuses to start
// with it, naming the type and the method, rather than meet it on a request.
func TestNew_RefusesAPartialOverride(t *testing.T) {
	for name, row := range map[string]struct {
		store    Store
		typ, why string
	}{
		"ledger":        {partialLedger{NewMemStore()}, "engine.partialLedger", "declares LookupPended but not the whole PendLedger"},
		"continuations": {partialContinuations{NewMemStore()}, "engine.partialContinuations", "declares Durable but not the whole ContinuationStore"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := New(Config{SoR: newCensusSoR(), Store: row.store})
			if err == nil || !strings.Contains(err.Error(), row.typ) || !strings.Contains(err.Error(), row.why) {
				t.Fatalf("New = %v, want a refusal naming %s and %q", err, row.typ, row.why)
			}
		})
	}
}

// TestMemStore_APartialOverrideNeverPanics: on the request path, a Store New
// would refuse (a Gateway assembled without New, say) is answered with no
// ledger or continuation store rather than the embedded one in its place, and
// never with a panic. The other lookup, whose interface it does not touch,
// answers it as before.
func TestMemStore_APartialOverrideNeverPanics(t *testing.T) {
	if l, ok := LedgerOf(partialLedger{NewMemStore()}); ok || l != nil {
		t.Errorf("LedgerOf(partial ledger) = %v, %v; want none", l, ok)
	}
	if c, ok := ContinuationsOf(partialContinuations{NewMemStore()}); ok || c != nil {
		t.Errorf("ContinuationsOf(partial continuations) = %v, %v; want none", c, ok)
	}
	if _, ok := ContinuationsOf(partialLedger{NewMemStore()}); !ok {
		t.Error("ContinuationsOf refused a store whose override is a ledger method")
	}
	if _, ok := LedgerOf(partialContinuations{NewMemStore()}); !ok {
		t.Error("LedgerOf refused a store whose override is a continuation method")
	}
}

// TestNew_AcceptsTheShippedStores: the in-memory Store and a type embedding
// it whole (the census fixture) start a gateway; the check refuses only a
// partial override.
func TestNew_AcceptsTheShippedStores(t *testing.T) {
	for name, s := range map[string]Store{"MemStore": NewMemStore(), "embedded": newCensusSoR(), "whole ledger override": newUnreadableLedger()} {
		if err := workstreamStoreFault(s); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
