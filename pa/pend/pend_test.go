package pend

import (
	"reflect"
	"sort"
	"testing"
	"time"
)

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

// TestResolvePendMatch_Verdicts pins the shared verdict every backend returns,
// as one table, so a third-party PendLedger that calls it gets the same
// answers the built-in stores do.
func TestResolvePendMatch_Verdicts(t *testing.T) {
	strong := PendKeys{RequesterHolder: "provider-a", ClaimResponseIDs: []string{"urn:payer:cr|CR-1"}, ItemTraceNumbers: []string{"urn:shn:trace|TRACE-1"}}
	one := []PendCandidate{{SubjectPCI: "PCI-A", CorrelationID: "corr-A"}}
	two := append(one, PendCandidate{SubjectPCI: "PCI-A", CorrelationID: "corr-B"})
	held := []PendKeyRef{{Kind: PendKeyClaimResponseIdentifier, Key: "urn:payer:cr|CR-1"}, {Kind: PendKeyItemTraceNumber, Key: "urn:shn:trace|TRACE-1"}}
	for _, tc := range []struct {
		name       string
		stated     PendKeys
		candidates []PendCandidate
		held       []PendKeyRef
		want       PendMatch
	}{
		{"no strong key, whatever was found", PendKeys{RequesterHolder: "provider-a", RequestIDs: []string{"urn:shn:claim|C-1"}, ItemTraceNumbers: []string{"urn:shn:trace|TRACE-1"}}, one, held,
			PendMatch{Verdict: PendMatchNoStrongKey}},
		{"nothing found", strong, nil, nil, PendMatch{Verdict: PendMatchNone}},
		{"two found", strong, two, held, PendMatch{Verdict: PendMatchAmbiguous}},
		{"one found, agreeing", strong, one, held, PendMatch{SubjectPCI: "PCI-A", CorrelationID: "corr-A", Verdict: PendMatchFound}},
		{"one found, a stated kind it holds none of", PendKeys{RequesterHolder: "provider-a", ClaimResponseIDs: []string{"urn:payer:cr|CR-1"}, PreAuthRef: "PA-9"}, one, held,
			PendMatch{SubjectPCI: "PCI-A", CorrelationID: "corr-A", Verdict: PendMatchFound}},
		{"one found, disagreeing", PendKeys{RequesterHolder: "provider-a", ClaimResponseIDs: []string{"urn:payer:cr|CR-1"}, ItemTraceNumbers: []string{"urn:shn:trace|TRACE-9"}}, one, held,
			PendMatch{Verdict: PendMatchDisagrees, Kind: PendKeyItemTraceNumber}},
	} {
		if got := ResolvePendMatch(tc.stated, tc.candidates, tc.held, false); got != tc.want {
			t.Errorf("%s: ResolvePendMatch = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// PendDecisionKept is what each store asks before it indexes an answer's
// authorization number: only the decision the ledger kept indexes one. Through a
// ledger an answer that does not lose on its date is the one kept, so the
// outcome half of the rule is pinned here, on the function itself: an answer
// whose outcome is not the kept one never indexes its number, whatever its date.
func TestPendDecisionKept(t *testing.T) {
	at := time.Date(2026, 9, 17, 11, 0, 0, 0, time.UTC)
	kept := PendRecord{State: PendStateDecided, Outcome: PendOutcomeApproved, DecidedAt: at}
	for _, tc := range []struct {
		name    string
		next    PendRecord
		outcome string
		date    time.Time
		want    bool
	}{
		{"the kept decision, same date", kept, PendOutcomeApproved, at, true},
		{"the kept decision, restated later", kept, PendOutcomeApproved, at.Add(time.Hour), true},
		{"the kept decision, restated earlier", kept, PendOutcomeApproved, at.Add(-time.Hour), false},
		{"another outcome, same date", kept, PendOutcomeDenied, at, false},
		{"another outcome, later", kept, PendOutcomeDenied, at.Add(time.Hour), false},
		{"a row that is not decided", PendRecord{State: PendStatePended}, PendOutcomeApproved, at, false},
	} {
		if got := PendDecisionKept(tc.next, tc.outcome, tc.date); got != tc.want {
			t.Errorf("%s: PendDecisionKept = %v, want %v", tc.name, got, tc.want)
		}
	}
}
