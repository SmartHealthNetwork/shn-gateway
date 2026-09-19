package pgstore_test

// pendledger_test.go is the ONE conformance table for the payer pend ledger
// (engine.PendLedger), run against BOTH backends that ship it — the
// in-memory engine.MemStore and this package's PgStore — exactly as parity_test.go
// runs the Store contract against both. A rule that holds on one backend and not the
// other is the "stand-in more permissive than the real thing" bug, so every row here
// runs twice.
//
// The third implementation (internal/holdersim's delegating Client) runs the same
// rules over its HTTP routes in internal/holdersim/pendledger_test.go; it cannot run
// from here because holdersim lives in the platform module, which imports this one.
//
// Retention (the bounded lazy purge) and the ledger-less fallback are backend-local
// rows: they need the store's injected clock and an in-package type, so they live in
// gateway/engine/pendledger_test.go and pendledger_pg_test.go.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/pgstore"
	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
)

// ledgerUnderTest is the contract a ledger backend satisfies: the unchanged Store
// seam plus the additive ledger.
type ledgerUnderTest interface {
	engine.Store
	engine.PendLedger
}

// Compile-time proof that both backends are in the table's contract.
var (
	_ ledgerUnderTest = (*engine.MemStore)(nil)
	_ ledgerUnderTest = (*pgstore.PgStore)(nil)
)

// The ledger's clock values. Fixed, whole-microsecond UTC instants: TIMESTAMPTZ
// keeps microseconds, so a nanosecond in a fixture would make the pg row fail on
// round-trip for a reason that has nothing to do with the rule under test.
var (
	tPend    = time.Date(2026, 9, 17, 9, 0, 0, 0, time.UTC)
	tEarly   = time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	tDecided = time.Date(2026, 9, 17, 11, 0, 0, 0, time.UTC)
	tLater   = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
)

const (
	requester = "provider-a"
	other     = "provider-b"
	pciA      = "PCI-A"
	corrA     = "corr-A"
	corrB     = "corr-B"
)

// keys is the synthetic PendKeys of one submitted claim: one key of every kind.
func keys(requesterHolder string) engine.PendKeys {
	return engine.PendKeys{
		RequesterHolder:  requesterHolder,
		RequestIDs:       []string{"urn:shn:claim|CLM-1"},
		ClaimResponseIDs: []string{"urn:payer:claimresponse|CR-1"},
		PreAuthRef:       "PA-1",
		ItemTraceNumbers: []string{"urn:shn:trace|TR-1-1"},
	}
}

// eob is a synthetic PA-decision EOB record for pciA.
func eob(id string) *engine.EOBRecord {
	return &engine.EOBRecord{SubjectPCI: pciA, EOBID: id, JSON: []byte(`{"resourceType":"ExplanationOfBenefit","id":"` + id + `"}`)}
}

func ledgerPg(t *testing.T) ledgerUnderTest {
	t.Helper()
	pool := pgstore.TestPool(t)
	s, err := pgstore.NewPgStore(context.Background(), pool, "payer")
	if err != nil {
		t.Fatalf("NewPgStore: %v", err)
	}
	return s
}

func TestPendLedgerParity_Mem(t *testing.T) {
	pendLedgerChecks(t, func(t *testing.T) ledgerUnderTest { t.Helper(); return engine.NewMemStore() })
}

func TestPendLedgerParity_Pg(t *testing.T) {
	pendLedgerChecks(t, func(t *testing.T) ledgerUnderTest { t.Helper(); return ledgerPg(t) })
}

// --- assertion helpers ---

func mustRecord(t *testing.T, l ledgerUnderTest, subject, corr string) engine.PendRecord {
	t.Helper()
	rec, ok, err := l.PendRecordOf(subject, corr)
	if err != nil {
		t.Fatalf("PendRecordOf(%s,%s): %v", subject, corr, err)
	}
	if !ok {
		t.Fatalf("PendRecordOf(%s,%s): no ledger row", subject, corr)
	}
	return rec
}

func wantState(t *testing.T, l ledgerUnderTest, subject, corr string, want engine.PendState) engine.PendRecord {
	t.Helper()
	rec := mustRecord(t, l, subject, corr)
	if rec.State != want {
		t.Fatalf("state = %q, want %q", rec.State, want)
	}
	return rec
}

func wantDecision(t *testing.T, rec engine.PendRecord, outcome string, at time.Time) {
	t.Helper()
	if rec.Outcome != outcome {
		t.Fatalf("outcome = %q, want %q", rec.Outcome, outcome)
	}
	if !rec.DecidedAt.Equal(at) {
		t.Fatalf("decidedAt = %s, want %s", rec.DecidedAt, at)
	}
}

func pend(t *testing.T, l ledgerUnderTest, subject, corr string, created time.Time, k engine.PendKeys) engine.PendTransition {
	t.Helper()
	tr, err := l.RecordPendedKeyed(subject, corr, created, k)
	if err != nil {
		t.Fatalf("RecordPendedKeyed(%s,%s): %v", subject, corr, err)
	}
	return tr
}

func decide(t *testing.T, l ledgerUnderTest, subject, corr, outcome string, at time.Time, e *engine.EOBRecord) engine.PendTransition {
	t.Helper()
	tr, err := l.RecordDecision(subject, corr, outcome, at, e)
	if err != nil {
		t.Fatalf("RecordDecision(%s,%s,%s): %v", subject, corr, outcome, err)
	}
	return tr
}

// --- the table ---

func pendLedgerChecks(t *testing.T, newLedger func(*testing.T) ledgerUnderTest) {
	t.Helper()

	// The transitions: pended → in_progress → pended → in_progress → decided.
	t.Run("transitions", func(t *testing.T) {
		l := newLedger(t)
		tr := pend(t, l, pciA, corrA, tPend, keys(requester))
		if tr.From != "" || tr.To != engine.PendStatePended || !tr.Changed || tr.Event != "" {
			t.Fatalf("first pend transition = %+v", tr)
		}
		rec := wantState(t, l, pciA, corrA, engine.PendStatePended)
		if rec.RequesterHolder != requester {
			t.Fatalf("requesterHolder = %q, want %q", rec.RequesterHolder, requester)
		}

		ok, why, err := l.BeginClaimUpdateReason(pciA, corrA)
		if err != nil || !ok || why != engine.PendRefusalNone {
			t.Fatalf("begin on pended = %v,%q,%v", ok, why, err)
		}
		wantState(t, l, pciA, corrA, engine.PendStateInProgress)

		// A second concurrent update cannot bind the same claim.
		if ok, why, _ := l.BeginClaimUpdateReason(pciA, corrA); ok || why != engine.PendRefusalInProgress {
			t.Fatalf("begin on in_progress = %v,%q", ok, why)
		}

		if err := l.ReleaseClaimUpdate(pciA, corrA); err != nil {
			t.Fatal(err)
		}
		wantState(t, l, pciA, corrA, engine.PendStatePended)

		if ok, _, _ := l.BeginClaimUpdateReason(pciA, corrA); !ok {
			t.Fatal("begin after release = false")
		}
		tr = decide(t, l, pciA, corrA, engine.PendOutcomeApproved, tDecided, eob("eob-1"))
		if tr.From != engine.PendStateInProgress || tr.To != engine.PendStateDecided || tr.Event != "" || !tr.Changed {
			t.Fatalf("decision transition = %+v", tr)
		}
		wantDecision(t, wantState(t, l, pciA, corrA, engine.PendStateDecided), engine.PendOutcomeApproved, tDecided)
	})

	// A claim the ledger never pended (an immediately approved submit) is still
	// recorded as decided, so a later amendment is refused as decided and not as
	// "never pended".
	t.Run("decision without a prior pend", func(t *testing.T) {
		l := newLedger(t)
		tr := decide(t, l, pciA, corrA, engine.PendOutcomeDenied, tDecided, eob("eob-1"))
		if tr.From != "" || tr.To != engine.PendStateDecided {
			t.Fatalf("transition = %+v", tr)
		}
		wantDecision(t, wantState(t, l, pciA, corrA, engine.PendStateDecided), engine.PendOutcomeDenied, tDecided)
	})

	// decided is ABSORBING: no Store transition moves it.
	t.Run("decided is absorbing", func(t *testing.T) {
		l := newLedger(t)
		pend(t, l, pciA, corrA, tPend, keys(requester))
		decide(t, l, pciA, corrA, engine.PendOutcomeApproved, tDecided, eob("eob-1"))

		if ok, err := l.BeginClaimUpdate(pciA, corrA); ok || err != nil {
			t.Fatalf("Store.BeginClaimUpdate on decided = %v,%v", ok, err)
		}
		if err := l.ReleaseClaimUpdate(pciA, corrA); err != nil {
			t.Fatal(err)
		}
		if err := l.FinalizeClaimUpdate(pciA, corrA); err != nil {
			t.Fatal(err)
		}
		if err := l.RecordPendedClaim(pciA, corrA); err != nil {
			t.Fatal(err)
		}
		wantDecision(t, wantState(t, l, pciA, corrA, engine.PendStateDecided), engine.PendOutcomeApproved, tDecided)
	})

	// Release and Finalize on a decided claim are no-ops that report "already
	// decided" — the row, its outcome and its date all survive.
	t.Run("release and finalize on decided are no-ops", func(t *testing.T) {
		l := newLedger(t)
		pend(t, l, pciA, corrA, tPend, keys(requester))
		decide(t, l, pciA, corrA, engine.PendOutcomeDenied, tDecided, eob("eob-1"))
		for _, step := range []struct {
			name string
			call func() error
		}{
			{"release", func() error { return l.ReleaseClaimUpdate(pciA, corrA) }},
			{"finalize", func() error { return l.FinalizeClaimUpdate(pciA, corrA) }},
		} {
			if err := step.call(); err != nil {
				t.Fatalf("%s: %v", step.name, err)
			}
			rec := wantState(t, l, pciA, corrA, engine.PendStateDecided)
			wantDecision(t, rec, engine.PendOutcomeDenied, tDecided)
			if _, why, _ := l.BeginClaimUpdateReason(pciA, corrA); why != engine.PendRefusalDecided {
				t.Fatalf("after %s the refusal reason is %q, want %q", step.name, why, engine.PendRefusalDecided)
			}
		}
	})

	// Begin on a decided claim refuses with a reason DISTINGUISHABLE from
	// "not pended" — the leg turns it into the 409 that says so.
	t.Run("begin on decided refuses with a distinct reason", func(t *testing.T) {
		l := newLedger(t)
		if ok, why, err := l.BeginClaimUpdateReason(pciA, "corr-never"); ok || err != nil || why != engine.PendRefusalNotPended {
			t.Fatalf("begin on a never-pended claim = %v,%q,%v", ok, why, err)
		}
		pend(t, l, pciA, corrA, tPend, keys(requester))
		decide(t, l, pciA, corrA, engine.PendOutcomeApproved, tDecided, eob("eob-1"))
		ok, why, err := l.BeginClaimUpdateReason(pciA, corrA)
		if ok || err != nil {
			t.Fatalf("begin on decided = %v,%v", ok, err)
		}
		if why != engine.PendRefusalDecided {
			t.Fatalf("refusal = %q, want %q", why, engine.PendRefusalDecided)
		}
		if why == engine.PendRefusalNotPended {
			t.Fatal("the decided refusal is not distinguishable from not-pended")
		}
	})

	// The same outcome twice is idempotent: one decision, one EOB, no event.
	t.Run("record decision is idempotent", func(t *testing.T) {
		l := newLedger(t)
		pend(t, l, pciA, corrA, tPend, keys(requester))
		decide(t, l, pciA, corrA, engine.PendOutcomeApproved, tDecided, eob("eob-1"))
		tr := decide(t, l, pciA, corrA, engine.PendOutcomeApproved, tDecided, eob("eob-1"))
		if tr.Changed || tr.Event != "" {
			t.Fatalf("repeat decision transition = %+v", tr)
		}
		wantDecision(t, wantState(t, l, pciA, corrA, engine.PendStateDecided), engine.PendOutcomeApproved, tDecided)
	})

	// Duplicate terminal inquiries (the same answer relayed twice) write ONE EOB.
	t.Run("duplicate terminal decisions write one EOB", func(t *testing.T) {
		l := newLedger(t)
		pend(t, l, pciA, corrA, tPend, keys(requester))
		decide(t, l, pciA, corrA, engine.PendOutcomeApproved, tDecided, eob("eob-1"))
		decide(t, l, pciA, corrA, engine.PendOutcomeApproved, tDecided, eob("eob-1"))
		got, ok := l.EOBsForPatient(pciA)
		if !ok || len(got) != 1 {
			t.Fatalf("EOBsForPatient = %d,%v — want exactly one EOB", len(got), ok)
		}
	})

	// A conflicting outcome keeps the decision the payer dated LATER, and says so.
	t.Run("conflicting outcome keeps the later-dated decision", func(t *testing.T) {
		l := newLedger(t)
		pend(t, l, pciA, corrA, tPend, keys(requester))
		decide(t, l, pciA, corrA, engine.PendOutcomeApproved, tDecided, eob("eob-1"))

		tr := decide(t, l, pciA, corrA, engine.PendOutcomeDenied, tLater, eob("eob-2"))
		if tr.Event != engine.DecisionConflictEvent || !tr.Changed {
			t.Fatalf("later conflicting decision = %+v, want %s", tr, engine.DecisionConflictEvent)
		}
		wantDecision(t, wantState(t, l, pciA, corrA, engine.PendStateDecided), engine.PendOutcomeDenied, tLater)

		tr = decide(t, l, pciA, corrA, engine.PendOutcomeApproved, tEarly, eob("eob-3"))
		if tr.Event != engine.DecisionConflictEvent {
			t.Fatalf("earlier conflicting decision = %+v, want %s", tr, engine.DecisionConflictEvent)
		}
		if tr.Changed {
			t.Fatal("an earlier-dated conflicting decision replaced the later one")
		}
		wantDecision(t, wantState(t, l, pciA, corrA, engine.PendStateDecided), engine.PendOutcomeDenied, tLater)

		// An UNDATED conflicting decision is "not later" as well — the same
		// meaning the re-pend rule gives an absent date — so it does not displace
		// the recorded one either.
		tr = decide(t, l, pciA, corrA, engine.PendOutcomeApproved, time.Time{}, eob("eob-4"))
		if tr.Event != engine.DecisionConflictEvent || tr.Changed {
			t.Fatalf("undated conflicting decision = %+v", tr)
		}
		wantDecision(t, wantState(t, l, pciA, corrA, engine.PendStateDecided), engine.PendOutcomeDenied, tLater)
	})

	// A re-pend of a decided claim supersedes ONLY when the payer dated it later.
	t.Run("re-pend after decided supersedes only when later", func(t *testing.T) {
		l := newLedger(t)
		pend(t, l, pciA, corrA, tPend, keys(requester))
		decide(t, l, pciA, corrA, engine.PendOutcomeApproved, tDecided, eob("eob-1"))

		// Fixture 1: the re-pend is dated BEFORE the decision — a stale answer.
		tr := pend(t, l, pciA, corrA, tEarly, keys(requester))
		if tr.Event != engine.DecisionStaleAnswerEvent || tr.To != engine.PendStateDecided || tr.Changed {
			t.Fatalf("stale re-pend = %+v, want %s", tr, engine.DecisionStaleAnswerEvent)
		}
		wantDecision(t, wantState(t, l, pciA, corrA, engine.PendStateDecided), engine.PendOutcomeApproved, tDecided)

		// Fixture 2: the re-pend is dated at the SAME instant as the decision.
		// "Not later" covers equal, so the decision stands.
		tr = pend(t, l, pciA, corrA, tDecided, keys(requester))
		if tr.Event != engine.DecisionStaleAnswerEvent || tr.To != engine.PendStateDecided || tr.Changed {
			t.Fatalf("same-instant re-pend = %+v, want %s", tr, engine.DecisionStaleAnswerEvent)
		}
		wantDecision(t, wantState(t, l, pciA, corrA, engine.PendStateDecided), engine.PendOutcomeApproved, tDecided)

		// Fixture 3: the re-pend is UNDATED. "Not later" covers that too, so it
		// does not reopen the decision — reopening on an absent date would let a
		// malformed response (ClaimResponse.created is 1..1) undo a decision the
		// payer actually made.
		tr = pend(t, l, pciA, corrA, time.Time{}, keys(requester))
		if tr.Event != engine.DecisionStaleAnswerEvent || tr.To != engine.PendStateDecided || tr.Changed {
			t.Fatalf("undated re-pend = %+v, want %s", tr, engine.DecisionStaleAnswerEvent)
		}
		wantDecision(t, wantState(t, l, pciA, corrA, engine.PendStateDecided), engine.PendOutcomeApproved, tDecided)

		// Fixture 4: the re-pend is dated AFTER the decision — it supersedes it,
		// and a later amendment then binds normally.
		tr = pend(t, l, pciA, corrA, tLater, keys(requester))
		if tr.Event != engine.DecisionSupersededEvent || tr.To != engine.PendStatePended || !tr.Changed {
			t.Fatalf("superseding re-pend = %+v, want %s", tr, engine.DecisionSupersededEvent)
		}
		rec := wantState(t, l, pciA, corrA, engine.PendStatePended)
		if rec.Outcome != "" || !rec.DecidedAt.IsZero() {
			t.Fatalf("a superseded decision survived: %+v", rec)
		}
		if ok, why, _ := l.BeginClaimUpdateReason(pciA, corrA); !ok || why != engine.PendRefusalNone {
			t.Fatalf("amendment after a superseding re-pend = %v,%q", ok, why)
		}
	})

	// A rollback (the leg's ReleaseClaimUpdate hook) after the decision does not
	// revert it.
	t.Run("rollback after a decision does not revert it", func(t *testing.T) {
		l := newLedger(t)
		pend(t, l, pciA, corrA, tPend, keys(requester))
		if ok, _, _ := l.BeginClaimUpdateReason(pciA, corrA); !ok {
			t.Fatal("begin = false")
		}
		decide(t, l, pciA, corrA, engine.PendOutcomeDenied, tDecided, eob("eob-1"))
		if err := l.ReleaseClaimUpdate(pciA, corrA); err != nil { // the deferred Rollback
			t.Fatal(err)
		}
		wantDecision(t, wantState(t, l, pciA, corrA, engine.PendStateDecided), engine.PendOutcomeDenied, tDecided)
		if ok, why, _ := l.BeginClaimUpdateReason(pciA, corrA); ok || why != engine.PendRefusalDecided {
			t.Fatalf("begin after the rollback = %v,%q", ok, why)
		}
	})

	// Lookup matches on EVERY key kind, one at a time.
	t.Run("lookup by each key kind", func(t *testing.T) {
		l := newLedger(t)
		pend(t, l, pciA, corrA, tPend, keys(requester))
		full := keys(requester)
		for _, probe := range []struct {
			kind string
			k    engine.PendKeys
		}{
			{engine.PendKeyRequestIdentifier, engine.PendKeys{RequesterHolder: requester, RequestIDs: full.RequestIDs}},
			{engine.PendKeyClaimResponseIdentifier, engine.PendKeys{RequesterHolder: requester, ClaimResponseIDs: full.ClaimResponseIDs}},
			{engine.PendKeyPreAuthRef, engine.PendKeys{RequesterHolder: requester, PreAuthRef: full.PreAuthRef}},
			{engine.PendKeyItemTraceNumber, engine.PendKeys{RequesterHolder: requester, ItemTraceNumbers: full.ItemTraceNumbers}},
		} {
			subject, corr, found, ambiguous, err := l.LookupPended(requester, probe.k)
			if err != nil || !found || ambiguous {
				t.Fatalf("%s: lookup = %v,%v,%v", probe.kind, found, ambiguous, err)
			}
			if subject != pciA || corr != corrA {
				t.Fatalf("%s: lookup = %q,%q", probe.kind, subject, corr)
			}
		}
	})

	// A decided claim stays findable: an inquiry about it must resolve to the
	// decision rather than to "no such authorization".
	t.Run("lookup finds a decided claim", func(t *testing.T) {
		l := newLedger(t)
		pend(t, l, pciA, corrA, tPend, keys(requester))
		decide(t, l, pciA, corrA, engine.PendOutcomeApproved, tDecided, eob("eob-1"))
		subject, corr, found, ambiguous, err := l.LookupPended(requester, keys(requester))
		if err != nil || !found || ambiguous || subject != pciA || corr != corrA {
			t.Fatalf("lookup of a decided claim = %q,%q,%v,%v,%v", subject, corr, found, ambiguous, err)
		}
	})

	// requesterHolder namespaces every key: another requester's identical keys
	// find nothing.
	t.Run("lookup is namespaced by requester", func(t *testing.T) {
		l := newLedger(t)
		pend(t, l, pciA, corrA, tPend, keys(requester))
		subject, corr, found, ambiguous, err := l.LookupPended(other, keys(other))
		if err != nil {
			t.Fatal(err)
		}
		if found || ambiguous || subject != "" || corr != "" {
			t.Fatalf("another requester's lookup = %q,%q,%v,%v", subject, corr, found, ambiguous)
		}
	})

	// Two pends sharing an identifier are AMBIGUOUS, and the lookup changes nothing.
	t.Run("an ambiguous key makes no ledger change", func(t *testing.T) {
		l := newLedger(t)
		shared := engine.PendKeys{RequesterHolder: requester, RequestIDs: []string{"urn:shn:claim|CLM-SHARED"}}
		pend(t, l, pciA, corrA, tPend, shared)
		pend(t, l, pciA, corrB, tPend, shared)
		subject, corr, found, ambiguous, err := l.LookupPended(requester, engine.PendKeys{RequesterHolder: requester, RequestIDs: shared.RequestIDs})
		if err != nil {
			t.Fatal(err)
		}
		if !ambiguous {
			t.Fatalf("lookup = %q,%q,%v,%v — want ambiguous", subject, corr, found, ambiguous)
		}
		if found || subject != "" || corr != "" {
			t.Fatalf("an ambiguous lookup named a claim: %q,%q,%v", subject, corr, found)
		}
		wantState(t, l, pciA, corrA, engine.PendStatePended)
		wantState(t, l, pciA, corrB, engine.PendStatePended)
	})

	// An answer whose identifiers belong to an unrelated patient's claim matches
	// nothing at all.
	t.Run("an unrelated answer does not match", func(t *testing.T) {
		l := newLedger(t)
		pend(t, l, pciA, corrA, tPend, keys(requester))
		unrelated := engine.PendKeys{
			RequesterHolder:  requester,
			RequestIDs:       []string{"urn:shn:claim|CLM-OTHER"},
			ClaimResponseIDs: []string{"urn:payer:claimresponse|CR-OTHER"},
			PreAuthRef:       "PA-OTHER",
			ItemTraceNumbers: []string{"urn:shn:trace|TR-OTHER"},
		}
		subject, corr, found, ambiguous, err := l.LookupPended(requester, unrelated)
		if err != nil {
			t.Fatal(err)
		}
		if found || ambiguous || subject != "" || corr != "" {
			t.Fatalf("unrelated lookup = %q,%q,%v,%v", subject, corr, found, ambiguous)
		}
	})

	// The decision and its EOB are ONE write: a refused EOB leaves no decision.
	t.Run("a refused EOB leaves no decision", func(t *testing.T) {
		l := newLedger(t)
		pend(t, l, pciA, corrA, tPend, keys(requester))
		for _, bad := range []struct {
			name string
			eob  *engine.EOBRecord
		}{
			{"no id", &engine.EOBRecord{SubjectPCI: pciA, JSON: []byte(`{"resourceType":"ExplanationOfBenefit"}`)}},
			{"no bytes", &engine.EOBRecord{SubjectPCI: pciA, EOBID: "eob-1"}},
			{"another subject", &engine.EOBRecord{SubjectPCI: "PCI-OTHER", EOBID: "eob-1", JSON: []byte(`{}`)}},
		} {
			_, err := l.RecordDecision(pciA, corrA, engine.PendOutcomeApproved, tDecided, bad.eob)
			if !errors.Is(err, engine.ErrPendEOBInvalid) {
				t.Fatalf("%s: err = %v, want ErrPendEOBInvalid", bad.name, err)
			}
			wantState(t, l, pciA, corrA, engine.PendStatePended)
			if got, ok := l.EOBsForPatient(pciA); ok || len(got) != 0 {
				t.Fatalf("%s: %d EOBs were written by a refused decision", bad.name, len(got))
			}
		}
	})

	// A decision needs no EOB (the update leg records one without) — a nil EOB is
	// the decision alone.
	t.Run("a decision without an EOB", func(t *testing.T) {
		l := newLedger(t)
		pend(t, l, pciA, corrA, tPend, keys(requester))
		decide(t, l, pciA, corrA, engine.PendOutcomeApproved, tDecided, nil)
		wantDecision(t, wantState(t, l, pciA, corrA, engine.PendStateDecided), engine.PendOutcomeApproved, tDecided)
		if got, ok := l.EOBsForPatient(pciA); ok || len(got) != 0 {
			t.Fatalf("%d EOBs from an EOB-less decision", len(got))
		}
	})

	// --- the guards, each with its rejection row ---

	t.Run("a pend with no requester holder is refused", func(t *testing.T) {
		l := newLedger(t)
		k := keys("")
		if _, err := l.RecordPendedKeyed(pciA, corrA, tPend, k); !errors.Is(err, engine.ErrPendRequesterRequired) {
			t.Fatalf("err = %v, want ErrPendRequesterRequired", err)
		}
		if _, ok, err := l.PendRecordOf(pciA, corrA); ok || err != nil {
			t.Fatalf("a refused pend wrote a row: %v,%v", ok, err)
		}
		if _, _, _, _, err := l.LookupPended("", keys(requester)); !errors.Is(err, engine.ErrPendRequesterRequired) {
			t.Fatalf("lookup err = %v, want ErrPendRequesterRequired", err)
		}
	})

	t.Run("an oversized key is refused", func(t *testing.T) {
		l := newLedger(t)
		k := keys(requester)
		long := make([]byte, engine.MaxPendKeyBytes+1)
		for i := range long {
			long[i] = 'x'
		}
		k.RequestIDs = []string{string(long)}
		if _, err := l.RecordPendedKeyed(pciA, corrA, tPend, k); !errors.Is(err, engine.ErrPendKeyTooLong) {
			t.Fatalf("err = %v, want ErrPendKeyTooLong", err)
		}
		if _, ok, err := l.PendRecordOf(pciA, corrA); ok || err != nil {
			t.Fatalf("a refused pend wrote a row: %v,%v", ok, err)
		}
	})

	t.Run("an outcome that is not a decision is refused", func(t *testing.T) {
		l := newLedger(t)
		pend(t, l, pciA, corrA, tPend, keys(requester))
		for _, outcome := range []string{"", "pended", "PENDED", string(engine.PendStateInProgress)} {
			if _, err := l.RecordDecision(pciA, corrA, outcome, tDecided, nil); !errors.Is(err, engine.ErrPendOutcomeInvalid) {
				t.Fatalf("outcome %q: err = %v, want ErrPendOutcomeInvalid", outcome, err)
			}
			wantState(t, l, pciA, corrA, engine.PendStatePended)
		}
	})

	t.Run("a re-pend from another requester is refused", func(t *testing.T) {
		l := newLedger(t)
		pend(t, l, pciA, corrA, tPend, keys(requester))
		if _, err := l.RecordPendedKeyed(pciA, corrA, tLater, keys(other)); !errors.Is(err, engine.ErrPendRequesterMismatch) {
			t.Fatalf("err = %v, want ErrPendRequesterMismatch", err)
		}
		rec := wantState(t, l, pciA, corrA, engine.PendStatePended)
		if rec.RequesterHolder != requester {
			t.Fatalf("requesterHolder = %q, want %q", rec.RequesterHolder, requester)
		}
	})

	// A payer response that carries no identifier at all still pends — the bytes
	// are relayed either way — but nothing can look it up. Recorded, not hidden:
	// the transition REPORTS that it indexed no key, so the leg can raise the
	// event instead of leaving an operator to discover it at the next inquiry.
	t.Run("a pend with no keys is recorded and reported", func(t *testing.T) {
		l := newLedger(t)
		tr := pend(t, l, pciA, corrA, tPend, engine.PendKeys{RequesterHolder: requester})
		if tr.Keys != 0 {
			t.Fatalf("keyless pend reported %d keys", tr.Keys)
		}
		wantState(t, l, pciA, corrA, engine.PendStatePended)
		_, _, found, ambiguous, err := l.LookupPended(requester, engine.PendKeys{RequesterHolder: requester})
		if err != nil || found || ambiguous {
			t.Fatalf("keyless lookup = %v,%v,%v", found, ambiguous, err)
		}
		// An amendment on the same correlation still binds: only discoverability
		// by inquiry is limited, and nothing about the pend was lost.
		if ok, why, _ := l.BeginClaimUpdateReason(pciA, corrA); !ok || why != engine.PendRefusalNone {
			t.Fatalf("amendment on a keyless pend = %v,%q", ok, why)
		}
	})

	// The Store seam's KEYLESS pend sets the state and nothing else. It must not
	// replace the record: dropping the requester the claim was submitted under
	// would hand the authorization to whoever re-pends it next, and would strand
	// its lookup keys under a namespace that no longer matches — a stale index
	// entry that answers a later, unrelated lookup with ambiguous=true.
	//
	// The whole sequence runs as one row because each step is what exposes the
	// next: a keyless pend, then another requester's attempt, then a Finalize that
	// must take the keys with the row, then a fresh pend that must be findable and
	// unambiguous.
	t.Run("a keyless pend keeps the claim's requester and keys", func(t *testing.T) {
		l := newLedger(t)
		claimKeys := engine.PendKeys{RequesterHolder: requester, RequestIDs: []string{"urn:shn:claim|CLM-1"}}
		pend(t, l, pciA, corrA, tPend, claimKeys)

		// The keyless pend.
		if err := l.RecordPendedClaim(pciA, corrA); err != nil {
			t.Fatal(err)
		}
		rec := wantState(t, l, pciA, corrA, engine.PendStatePended)
		if rec.RequesterHolder != requester {
			t.Fatalf("the keyless pend dropped the requester: %q, want %q", rec.RequesterHolder, requester)
		}

		// So a DIFFERENT requester still cannot take the authorization over.
		if _, err := l.RecordPendedKeyed(pciA, corrA, tLater,
			engine.PendKeys{RequesterHolder: other, RequestIDs: []string{"urn:shn:claim|CLM-1"}}); !errors.Is(err, engine.ErrPendRequesterMismatch) {
			t.Fatalf("another requester re-pended a keyless-pended claim: err = %v, want ErrPendRequesterMismatch", err)
		}

		// And the keys are still the original requester's, still resolving.
		subject, corr, found, ambiguous, err := l.LookupPended(requester, claimKeys)
		if err != nil || !found || ambiguous || subject != pciA || corr != corrA {
			t.Fatalf("lookup after the keyless pend = %q,%q,%v,%v,%v", subject, corr, found, ambiguous, err)
		}

		// Finalize removes the row AND its index entries.
		if err := l.FinalizeClaimUpdate(pciA, corrA); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := l.PendRecordOf(pciA, corrA); ok || err != nil {
			t.Fatalf("the finalized row survived: %v,%v", ok, err)
		}
		if _, _, found, ambiguous, err := l.LookupPended(requester, claimKeys); err != nil || found || ambiguous {
			t.Fatalf("a finalized claim is still indexed: %v,%v,%v", found, ambiguous, err)
		}

		// A fresh authorization reusing the same identifier is found, and is NOT
		// ambiguous — which it would be if the finalized row's entry had leaked.
		pend(t, l, pciA, corrB, tLater, claimKeys)
		subject, corr, found, ambiguous, err = l.LookupPended(requester, claimKeys)
		if err != nil || !found || ambiguous {
			t.Fatalf("lookup after re-pending the same identifier = %q,%q,%v,%v,%v", subject, corr, found, ambiguous, err)
		}
		if subject != pciA || corr != corrB {
			t.Fatalf("lookup resolved to %q,%q, want %q,%q", subject, corr, pciA, corrB)
		}
	})

	// A pend that DID carry identifiers reports how many it indexed, so the leg
	// can tell "nothing to match on" from "four ways to match".
	t.Run("a keyed pend reports its key count", func(t *testing.T) {
		l := newLedger(t)
		want, err := keys(requester).Refs()
		if err != nil {
			t.Fatal(err)
		}
		tr := pend(t, l, pciA, corrA, tPend, keys(requester))
		if tr.Keys != len(want) {
			t.Fatalf("pend reported %d keys, want %d", tr.Keys, len(want))
		}
	})

	// The reported count is the UNION the authorization has, not this response's
	// contribution. A re-pend whose response carries no identifier leaves a claim
	// that is still findable by the keys the first response gave, so reporting zero
	// for it would make the leg raise "nothing to match on" about an authorization
	// with four ways to match — a false alarm on every such re-pend.
	t.Run("a keyless re-pend reports the keys the claim already has", func(t *testing.T) {
		l := newLedger(t)
		want, err := keys(requester).Refs()
		if err != nil {
			t.Fatal(err)
		}
		pend(t, l, pciA, corrA, tPend, keys(requester))

		tr := pend(t, l, pciA, corrA, tLater, engine.PendKeys{RequesterHolder: requester})
		if tr.Keys != len(want) {
			t.Fatalf("the keyless re-pend reported %d keys, want the %d the claim already has", tr.Keys, len(want))
		}
		// And it is findable, which is the fact the count is supposed to describe.
		subject, corr, found, ambiguous, err := l.LookupPended(requester, keys(requester))
		if err != nil || !found || ambiguous || subject != pciA || corr != corrA {
			t.Fatalf("lookup after the keyless re-pend = %q,%q,%v,%v,%v", subject, corr, found, ambiguous, err)
		}

		// A re-pend that DOES add a key reports the widened union.
		tr = pend(t, l, pciA, corrA, tLater, engine.PendKeys{RequesterHolder: requester, PreAuthRef: "PA-2"})
		if tr.Keys != len(want)+1 {
			t.Fatalf("the widening re-pend reported %d keys, want %d", tr.Keys, len(want)+1)
		}
	})

	// A re-pend UNIONS the new response's keys with the ones already recorded: the
	// original submit identifiers keep identifying the authorization.
	t.Run("a re-pend keeps the earlier keys", func(t *testing.T) {
		l := newLedger(t)
		pend(t, l, pciA, corrA, tPend, engine.PendKeys{RequesterHolder: requester, RequestIDs: []string{"urn:shn:claim|CLM-1"}})
		pend(t, l, pciA, corrA, tLater, engine.PendKeys{RequesterHolder: requester, PreAuthRef: "PA-2"})
		for _, k := range []engine.PendKeys{
			{RequesterHolder: requester, RequestIDs: []string{"urn:shn:claim|CLM-1"}},
			{RequesterHolder: requester, PreAuthRef: "PA-2"},
		} {
			subject, corr, found, _, err := l.LookupPended(requester, k)
			if err != nil || !found || subject != pciA || corr != corrA {
				t.Fatalf("lookup %+v = %q,%q,%v,%v", k, subject, corr, found, err)
			}
		}
	})
}
