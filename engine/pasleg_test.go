package engine

// pasleg_test.go — the submit and update legs report every ledger write that
// removed a decision EOB, so an EOB leaving Patient Access is never silent.

import (
	"context"
	"testing"
	"time"
)

func TestPASLeg_ReportsARemovedEOB(t *testing.T) {
	const pci, corr = "PCI-A", "corr-A"
	at := time.Unix(1_800_000_000, 0).UTC()
	keys := PendKeys{RequesterHolder: "provider-a", RequestIDs: []string{"urn:shn:claim|CLM-A"}}
	// denied is a store holding corr's denial with its EOB.
	denied := func(t *testing.T) *MemStore {
		t.Helper()
		s := NewMemStore()
		if _, err := s.RecordPendedKeyed(pci, corr, at, keys); err != nil {
			t.Fatal(err)
		}
		eob := &EOBRecord{SubjectPCI: pci, EOBID: DecisionEOBID(corr), JSON: []byte(`{"resourceType":"ExplanationOfBenefit","id":"denial"}`)}
		if _, err := s.RecordDecision(pci, corr, PendOutcomeDenied, at, keys, eob); err != nil {
			t.Fatal(err)
		}
		if _, ok := s.EOBByID(DecisionEOBID(corr)); !ok {
			t.Fatal("premise: the denial wrote no EOB")
		}
		return s
	}
	// removedNotes counts the leg's pend.eob-removed notes; eobKept reports
	// whether the denial's EOB is still filed.
	removedNotes := func(leg *pasLeg) int {
		n := 0
		for _, k := range leg.notes {
			if k == PendEOBRemovedEvent {
				n++
			}
		}
		return n
	}
	eobKept := func(s *MemStore) bool {
		_, ok := s.EOBByID(DecisionEOBID(corr))
		return ok
	}

	for _, tc := range []struct {
		name    string
		write   func(ctx context.Context, s *MemStore) func() error
		removed bool
	}{
		{"a later decision with another outcome and no EOB of its own", func(ctx context.Context, s *MemStore) func() error {
			return recordPASDecision(ctx, s, pci, corr, PendOutcomeApproved, at.Add(time.Hour), keys, nil)
		}, true},
		{"a later re-pend", func(ctx context.Context, s *MemStore) func() error {
			return recordPASPend(ctx, s, pci, corr, keys, at.Add(time.Hour))
		}, true},
		{"control: the same outcome restated later", func(ctx context.Context, s *MemStore) func() error {
			return recordPASDecision(ctx, s, pci, corr, PendOutcomeDenied, at.Add(time.Hour), keys, nil)
		}, false},
		{"control: an older re-pend", func(ctx context.Context, s *MemStore) func() error {
			return recordPASPend(ctx, s, pci, corr, keys, at.Add(-time.Hour))
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := denied(t)
			ctx, leg := withPASLeg(context.Background(), keys.RequesterHolder)
			if err := tc.write(ctx, s)(); err != nil {
				t.Fatalf("write: %v", err)
			}
			if eobKept(s) == tc.removed {
				t.Fatalf("premise: the denial's EOB kept=%v, want removed=%v", eobKept(s), tc.removed)
			}
			want := 0
			if tc.removed {
				want = 1
			}
			if got := removedNotes(leg); got != want {
				t.Fatalf("%s notes = %d (all notes %v), want %d", PendEOBRemovedEvent, got, leg.notes, want)
			}
		})
	}
}
