package engine

// continuation_test.go — the in-memory continuation store's own rows.
//
// The pair that matters most is TestContinuation_UnknownID404 and
// TestContinuation_LostAfterRestart410: they are the two answers one store gives
// to the same kind of request, and a store that could not tell them apart would
// either blame the caller for state it dropped itself, or tell a caller with a
// made-up id that the gateway once had it.

import (
	"strings"
	"testing"
	"time"
)

// sampleContinuation is a complete, consistent record: two submitted lines, both
// with trace numbers, listed in the order they were submitted.
func sampleContinuation(holder string) Continuation {
	return Continuation{
		Holder:          holder,
		PayerHolder:     "payer-a",
		Line:            "2.0",
		CorrID:          "corr-1",
		SubjectPCI:      "pci:one",
		MemberID:        "MBR-D-UC04",
		SoRPatientID:    "Patient/p1",
		OrderRef:        "ServiceRequest/sr-1",
		ProviderNPI:     "1234567893",
		ClaimIdentifier: "urn:shn:claim|claim-1",
		ItemTraceNumbers: []string{
			"urn:shn:trace|corr-1-1",
			"urn:shn:trace|corr-1-2",
		},
		Items: []ContinuationItem{
			{Sequence: 1, ProductCode: "https://bluebutton.cms.gov/resources/codesystem/hcpcs|E0250", ServiceDate: "2027-04-01", TraceNumber: "urn:shn:trace|corr-1-1"},
			{Sequence: 2, ProductCode: "https://bluebutton.cms.gov/resources/codesystem/hcpcs|L8000", ServiceDate: "2027-04-02", TraceNumber: "urn:shn:trace|corr-1-2"},
		},
		PayerClaimResponseIDs: []string{"urn:payer:cr|cr-1"},
		PayerPreAuthRef:       "PA-0001",
		LastOutcome:           ContinuationOutcomePended,
	}
}

func TestContinuation_RoundTripsEveryStoredFact(t *testing.T) {
	cs := NewMemContinuations()
	in := sampleContinuation("provider-a")
	out, err := cs.PutContinuation(in)
	if err != nil {
		t.Fatalf("PutContinuation: %v", err)
	}
	if out.ID == "" {
		t.Fatal("an empty id must be minted")
	}
	got, look, err := cs.ReadContinuation("provider-a", out.ID)
	if err != nil || look != ContinuationFound {
		t.Fatalf("ReadContinuation = %v, %v", look, err)
	}
	if got.PayerHolder != "payer-a" || got.Line != "2.0" || got.CorrID != "corr-1" ||
		got.SubjectPCI != "pci:one" || got.MemberID != "MBR-D-UC04" || got.SoRPatientID != "Patient/p1" ||
		got.OrderRef != "ServiceRequest/sr-1" || got.ProviderNPI != "1234567893" ||
		got.ClaimIdentifier != "urn:shn:claim|claim-1" || got.PayerPreAuthRef != "PA-0001" ||
		got.LastOutcome != "pended" {
		t.Fatalf("a stored fact did not round-trip: %+v", got)
	}
	if len(got.Items) != 2 || got.Items[1].ProductCode != "https://bluebutton.cms.gov/resources/codesystem/hcpcs|L8000" ||
		got.Items[1].ServiceDate != "2027-04-02" {
		t.Fatalf("item map did not round-trip: %+v", got.Items)
	}
	// The record the caller handed in must not be reachable through the store.
	out.Items[0].ProductCode = "mutated"
	out.ItemTraceNumbers[0] = "mutated"
	again, _, _ := cs.ReadContinuation("provider-a", out.ID)
	if again.Items[0].ProductCode == "mutated" || again.ItemTraceNumbers[0] == "mutated" {
		t.Fatal("a returned record shares its slices with the stored one")
	}
}

// TestContinuation_UnknownID404: an id this store never minted is UNKNOWN, and
// the surface answers 404 — never 403, which would confirm that the id names
// something somewhere.
func TestContinuation_UnknownID404(t *testing.T) {
	cs := NewMemContinuations()
	stored, err := cs.PutContinuation(sampleContinuation("provider-a"))
	if err != nil {
		t.Fatal(err)
	}
	// A guess with THIS store's own mint mark: the shape is right, the record
	// does not exist. This is the case a mark-based rule could most easily get
	// wrong by reporting the disclosed loss instead.
	guess := NewContinuationID(cs.mark)
	if guess == stored.ID {
		t.Fatal("two mints collided")
	}
	for _, id := range []string{guess, "not-an-id", "", stored.ID + "0"} {
		_, look, err := cs.ReadContinuation("provider-a", id)
		if err != nil {
			t.Fatalf("%q: %v", id, err)
		}
		if look != ContinuationUnknown {
			t.Fatalf("%q: lookup = %v, want unknown", id, look)
		}
		if status, msg := ContinuationRefusal(look); status != 404 || msg != "unknown continuation" {
			t.Fatalf("%q: refusal = %d %q", id, status, msg)
		}
	}
}

// TestContinuation_OtherHolderID404: the continuation is bound to
// {holder, continuationID}. Another holder presenting a real id is told nothing
// exists, not that it is somebody else's.
func TestContinuation_OtherHolderID404(t *testing.T) {
	cs := NewMemContinuations()
	stored, err := cs.PutContinuation(sampleContinuation("provider-a"))
	if err != nil {
		t.Fatal(err)
	}
	_, look, err := cs.ReadContinuation("provider-b", stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if look != ContinuationUnknown {
		t.Fatalf("lookup = %v, want unknown (never a distinguishable refusal)", look)
	}
	if status, _ := ContinuationRefusal(look); status != 404 {
		t.Fatalf("status = %d, want 404", status)
	}
}

// TestContinuation_LostAfterRestart410: the disclosed posture. A gateway with no
// durable store loses its continuations when it restarts, and says so — with the
// cause and both ways forward — instead of answering "unknown", which would put
// the fault on the caller.
func TestContinuation_LostAfterRestart410(t *testing.T) {
	before := NewMemContinuations()
	stored, err := before.PutContinuation(sampleContinuation("provider-a"))
	if err != nil {
		t.Fatal(err)
	}
	// The restart: a new store, same holder, same id in the caller's hand.
	after := NewMemContinuations()
	_, look, err := after.ReadContinuation("provider-a", stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if look != ContinuationLost {
		t.Fatalf("lookup = %v, want lost", look)
	}
	status, msg := ContinuationRefusal(look)
	if status != 410 {
		t.Fatalf("status = %d, want 410", status)
	}
	// The sentence is pinned as a LITERAL, not against the constant the code
	// reads: a comparison with ContinuationLostMessage would agree with whatever
	// the code said.
	if msg != "continuation lost (gateway restarted; submit again or inquire from your own system)" {
		t.Fatalf("message = %q", msg)
	}
	if DurableContinuations(after) {
		t.Fatal("the in-memory store must report itself as non-durable")
	}
}

// TestContinuation_ResetIsTheOperatorsOwnRestart: a demo reset discards the same
// state a restart does, so an id from before it gets the same honest answer.
func TestContinuation_ResetIsTheOperatorsOwnRestart(t *testing.T) {
	cs := NewMemContinuations()
	stored, err := cs.PutContinuation(sampleContinuation("provider-a"))
	if err != nil {
		t.Fatal(err)
	}
	cs.ResetContinuations()
	if _, look, _ := cs.ReadContinuation("provider-a", stored.ID); look != ContinuationLost {
		t.Fatalf("lookup after reset = %v, want lost", look)
	}
	// And the store still works afterwards.
	fresh, err := cs.PutContinuation(sampleContinuation("provider-a"))
	if err != nil {
		t.Fatal(err)
	}
	if _, look, _ := cs.ReadContinuation("provider-a", fresh.ID); look != ContinuationFound {
		t.Fatalf("a continuation minted after the reset must be found (%v)", look)
	}
}

// TestContinuation_DurableMarkIsNeverReadAsLost: an id a DURABLE store minted,
// presented to an in-memory one, names nothing here — but nothing was lost, so
// the answer is 404. Without this row the mark rule would report the disclosed
// loss for any id it did not recognize.
func TestContinuation_DurableMarkIsNeverReadAsLost(t *testing.T) {
	cs := NewMemContinuations()
	if _, look, _ := cs.ReadContinuation("provider-a", NewContinuationID(ContinuationDurableMark)); look != ContinuationUnknown {
		t.Fatalf("a durable id read by an in-memory store = %v, want unknown", look)
	}
}

// TestContinuation_IDCarriesItsCapability: the id is 128 bits of randomness plus
// a mint mark. The literal widths are pinned here, in one place, so a narrower id
// cannot arrive as a refactor.
func TestContinuation_IDCarriesItsCapability(t *testing.T) {
	id := NewContinuationID("d")
	mark, rand, ok := strings.Cut(id, "-")
	if !ok || mark != "d" {
		t.Fatalf("id %q does not carry its mint mark", id)
	}
	if len(rand) != 32 { // 16 bytes, hex — 128 bits
		t.Fatalf("id %q carries %d hex characters, want 32 (128 bits)", id, len(rand))
	}
	if NewContinuationID("d") == id {
		t.Fatal("two mints produced the same capability")
	}
}

// TestContinuation_ItemListAndItemMapMustAgree: the two views of one submission
// are written together. A caller that built them separately would leave an
// inquiry naming lines the submission did not have, so the store refuses the
// write rather than storing the disagreement.
func TestContinuation_ItemListAndItemMapMustAgree(t *testing.T) {
	cs := NewMemContinuations()
	c := sampleContinuation("provider-a")
	c.ItemTraceNumbers = c.ItemTraceNumbers[:1]
	if _, err := cs.PutContinuation(c); err == nil {
		t.Fatal("a trace-number list shorter than the item map must be refused")
	}
	c = sampleContinuation("provider-a")
	c.ItemTraceNumbers[1] = "urn:shn:trace|someone-elses"
	if _, err := cs.PutContinuation(c); err == nil {
		t.Fatal("a trace number the item map does not carry must be refused")
	}
	// A submission whose lines carry NO trace numbers is legitimate and stores.
	c = sampleContinuation("provider-a")
	c.ItemTraceNumbers = nil
	for i := range c.Items {
		c.Items[i].TraceNumber = ""
	}
	if _, err := cs.PutContinuation(c); err != nil {
		t.Fatalf("a submission with no trace numbers must store: %v", err)
	}
}

func TestContinuation_HolderIsRequired(t *testing.T) {
	cs := NewMemContinuations()
	c := sampleContinuation("")
	if _, err := cs.PutContinuation(c); err == nil {
		t.Fatal("a continuation with no holder is bound to nothing and must be refused")
	}
	if _, _, err := cs.ReadContinuation("", "anything"); err == nil {
		t.Fatal("a read with no holder must be refused")
	}
}

// TestContinuation_UpdateKeepsCreatedAndMovesUpdated: continuing an
// authorization records what the payer has said since; it does not restart the
// record's life, and retention counts from the last thing that happened.
func TestContinuation_UpdateKeepsCreatedAndMovesUpdated(t *testing.T) {
	now := time.Date(2027, 4, 1, 12, 0, 0, 0, time.UTC)
	cs := NewMemContinuations()
	cs.now = func() time.Time { return now }
	first, err := cs.PutContinuation(sampleContinuation("provider-a"))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(48 * time.Hour)
	first.LastOutcome = ContinuationOutcomeApproved
	second, err := cs.PutContinuation(first)
	if err != nil {
		t.Fatal(err)
	}
	if !second.CreatedAt.Equal(time.Date(2027, 4, 1, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("CreatedAt moved on update: %v", second.CreatedAt)
	}
	if !second.UpdatedAt.Equal(now) {
		t.Fatalf("UpdatedAt = %v, want %v", second.UpdatedAt, now)
	}
	got, _, _ := cs.ReadContinuation("provider-a", first.ID)
	if got.LastOutcome != "approved" {
		t.Fatalf("LastOutcome = %q", got.LastOutcome)
	}
}

// TestContinuation_RetentionIsTheLedgersSixMonths: a continuation exists to ask
// the ledger's own authorization about itself, so the two periods are one period.
// The literal is pinned here and bound to both constants in this one row.
func TestContinuation_RetentionIsTheLedgersSixMonths(t *testing.T) {
	const wantDays = 184 // the LONGEST six-month span; never a shorter approximation
	if ContinuationRetention != wantDays*24*time.Hour {
		t.Fatalf("ContinuationRetention = %v, want %d days", ContinuationRetention, wantDays)
	}
	if ContinuationRetention != PendLedgerRetention {
		t.Fatalf("the continuation (%v) and the ledger (%v) must keep the same authorization for the same time",
			ContinuationRetention, PendLedgerRetention)
	}
}

// TestContinuation_RetentionPurgeIsBoundedPerCall: the in-memory sweep drops what
// is past its retention, at most PendPurgeMax per call, and never a record inside
// the window — the same rule the durable sweep runs.
func TestContinuation_RetentionPurgeIsBoundedPerCall(t *testing.T) {
	now := time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC)
	cs := NewMemContinuations()
	cs.now = func() time.Time { return now }

	stale := now.Add(-ContinuationRetention - 24*time.Hour)
	now = stale
	var ids []string
	for i := 0; i < PendPurgeMax+50; i++ {
		c, err := cs.PutContinuation(sampleContinuation("provider-a"))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.ID)
		// Keep the sweep from running between writes.
		cs.lastPurge = now
	}
	now = stale.Add(ContinuationRetention + 24*time.Hour)
	fresh, err := cs.PutContinuation(sampleContinuation("provider-a"))
	if err != nil {
		t.Fatal(err)
	}
	remaining := 0
	for _, id := range ids {
		if _, look, _ := cs.ReadContinuation("provider-a", id); look == ContinuationFound {
			remaining++
		}
	}
	if want := len(ids) - PendPurgeMax; remaining != want {
		t.Fatalf("after one sweep %d stale records remain, want %d (at most %d purged per call)", remaining, want, PendPurgeMax)
	}
	if _, look, _ := cs.ReadContinuation("provider-a", fresh.ID); look != ContinuationFound {
		t.Fatal("a record inside the retention window was purged")
	}
}

// TestContinuation_PayerKeysAreOnlyWhatThePayerAnswered: the keys a later inquiry
// names the authorization by come from the submission and the payer's own pend.
// A fresh inquiry's own Claim.identifier is not among them, and cannot be: the
// projection has nowhere to put it.
func TestContinuation_PayerKeysAreOnlyWhatThePayerAnswered(t *testing.T) {
	k := sampleContinuation("provider-a").PayerKeys("provider-a")
	if k.RequesterHolder != "provider-a" {
		t.Fatalf("RequesterHolder = %q", k.RequesterHolder)
	}
	if len(k.RequestIDs) != 1 || k.RequestIDs[0] != "urn:shn:claim|claim-1" {
		t.Fatalf("RequestIDs = %v", k.RequestIDs)
	}
	if len(k.ClaimResponseIDs) != 1 || k.ClaimResponseIDs[0] != "urn:payer:cr|cr-1" {
		t.Fatalf("ClaimResponseIDs = %v", k.ClaimResponseIDs)
	}
	if k.PreAuthRef != "PA-0001" || len(k.ItemTraceNumbers) != 2 {
		t.Fatalf("PreAuthRef = %q, ItemTraceNumbers = %v", k.PreAuthRef, k.ItemTraceNumbers)
	}
}

// TestContinuation_MemStoreShipsTheInMemoryStore: the default Store carries a
// continuation store, so a deployment with no durable store still has one — and
// reports itself as non-durable, which is what the Kit's notice reads.
func TestContinuation_MemStoreShipsTheInMemoryStore(t *testing.T) {
	s := NewMemStore()
	cs, ok := ContinuationsOf(s)
	if !ok {
		t.Fatal("the in-memory Store must ship a ContinuationStore")
	}
	if DurableContinuations(cs) {
		t.Fatal("the in-memory Store must report its continuations as non-durable")
	}
	stored, err := cs.PutContinuation(sampleContinuation("provider-a"))
	if err != nil {
		t.Fatal(err)
	}
	s.Reset()
	if _, look, _ := cs.ReadContinuation("provider-a", stored.ID); look != ContinuationLost {
		t.Fatalf("Reset must discard continuations honestly (%v)", look)
	}
}

func TestSortedContinuationItems(t *testing.T) {
	in := []ContinuationItem{{Sequence: 3}, {Sequence: 1}, {Sequence: 2}}
	got := SortedContinuationItems(in)
	for i, want := range []int{1, 2, 3} {
		if got[i].Sequence != want {
			t.Fatalf("sorted = %+v", got)
		}
	}
	if in[0].Sequence != 3 {
		t.Fatal("the caller's slice was reordered in place")
	}
}
