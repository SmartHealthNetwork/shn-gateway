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
	"strings"
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

// lookup runs LookupPended and fails the row on a store error.
func lookup(t *testing.T, l ledgerUnderTest, requesterHolder string, k engine.PendKeys) engine.PendMatch {
	t.Helper()
	m, err := l.LookupPended(requesterHolder, k, nil)
	if err != nil {
		t.Fatalf("LookupPended: %v", err)
	}
	return m
}

// wantFound asserts the lookup named exactly this authorization.
func wantFound(t *testing.T, m engine.PendMatch, subject, corr string) {
	t.Helper()
	if m.Verdict != engine.PendMatchFound || m.SubjectPCI != subject || m.CorrelationID != corr {
		t.Fatalf("lookup = %+v, want found %s/%s", m, subject, corr)
	}
}

// wantVerdict asserts a lookup that names no authorization, and why.
func wantVerdict(t *testing.T, m engine.PendMatch, want engine.PendMatchVerdict) {
	t.Helper()
	if m.Verdict != want {
		t.Fatalf("lookup = %+v, want %s", m, want)
	}
	if m.SubjectPCI != "" || m.CorrelationID != "" {
		t.Fatalf("a %s lookup named an authorization: %+v", m.Verdict, m)
	}
}

// wantEOBGone asserts a removed EOB is gone from every read: by id, from the
// patient's list, and as an owned id.
func wantEOBGone(t *testing.T, l ledgerUnderTest, eobID string) {
	t.Helper()
	if got, ok := l.EOBsForPatient(pciA); ok && len(got) > 0 {
		t.Fatalf("the removed EOB is still in the patient's list: %d EOB(s)", len(got))
	}
	if owner, ok := l.(engine.EOBOwnerLookup); ok {
		if who, found, err := owner.EOBOwner(eobID); err != nil || found {
			t.Fatalf("the removed EOB id is still owned: %q,%v,%v", who, found, err)
		}
	}
}

// decideN is decide with the decision's authorization number.
func decideN(t *testing.T, l ledgerUnderTest, subject, corr, outcome string, at time.Time, authorizationNumber string, e *engine.EOBRecord) engine.PendTransition {
	t.Helper()
	tr, err := l.RecordDecision(subject, corr, outcome, at, engine.PendKeys{RequesterHolder: requester, PreAuthRef: authorizationNumber}, e)
	if err != nil {
		t.Fatalf("RecordDecision(%s,%s,%s): %v", subject, corr, outcome, err)
	}
	return tr
}

func decide(t *testing.T, l ledgerUnderTest, subject, corr, outcome string, at time.Time, e *engine.EOBRecord) engine.PendTransition {
	t.Helper()
	tr, err := l.RecordDecision(subject, corr, outcome, at, engine.PendKeys{}, e)
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

	// A re-pend never reopens an amendment in progress. The payer's later word
	// about the authorization (a resent submission it pended again, or another
	// amendment it pended) is recorded — its keys join the authorization's — but
	// only the in-progress amendment's own outcome moves the row. Otherwise a
	// second amendment could bind while the first is still with the payer.
	t.Run("a re-pend leaves an amendment in progress", func(t *testing.T) {
		l := newLedger(t)
		pend(t, l, pciA, corrA, tPend, keys(requester))
		if ok, _, err := l.BeginClaimUpdateReason(pciA, corrA); err != nil || !ok {
			t.Fatalf("begin = %v,%v", ok, err)
		}
		later := keys(requester)
		later.ClaimResponseIDs = []string{"urn:payer:claimresponse|CR-2"}
		tr := pend(t, l, pciA, corrA, tLater, later)
		if tr.From != engine.PendStateInProgress || tr.To != engine.PendStateInProgress || tr.Changed || tr.Event != "" {
			t.Fatalf("re-pend during an amendment = %+v", tr)
		}
		wantState(t, l, pciA, corrA, engine.PendStateInProgress)
		if ok, why, _ := l.BeginClaimUpdateReason(pciA, corrA); ok || why != engine.PendRefusalInProgress {
			t.Fatalf("a second amendment bound during the first: %v,%q", ok, why)
		}
		// The re-pend's key was still recorded.
		probe := engine.PendKeys{RequesterHolder: requester, ClaimResponseIDs: []string{"urn:payer:claimresponse|CR-2"}}
		wantFound(t, lookup(t, l, requester, probe), pciA, corrA)
		// The keyless seam holds the same rule.
		if err := l.RecordPendedClaim(pciA, corrA); err != nil {
			t.Fatal(err)
		}
		wantState(t, l, pciA, corrA, engine.PendStateInProgress)
		// The amendment's own outcome still moves it.
		if err := l.ReleaseClaimUpdate(pciA, corrA); err != nil {
			t.Fatal(err)
		}
		wantState(t, l, pciA, corrA, engine.PendStatePended)
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

	// Lookup SEARCHES by the strong keys only: the payer's own
	// ClaimResponse identifier, or its preAuthRef, finds the claim alone. A weak
	// key alone (the requester's request identifier, or an item trace number,
	// which reused example bodies share across claims) finds nothing: it can only
	// confirm what a strong key found.
	t.Run("lookup searches by the strong keys only", func(t *testing.T) {
		l := newLedger(t)
		pend(t, l, pciA, corrA, tPend, keys(requester))
		full := keys(requester)
		for _, probe := range []struct {
			kind string
			k    engine.PendKeys
		}{
			{engine.PendKeyClaimResponseIdentifier, engine.PendKeys{RequesterHolder: requester, ClaimResponseIDs: full.ClaimResponseIDs}},
			{engine.PendKeyPreAuthRef, engine.PendKeys{RequesterHolder: requester, PreAuthRef: full.PreAuthRef}},
		} {
			wantFound(t, lookup(t, l, requester, probe.k), pciA, corrA)
		}
		for _, probe := range []struct {
			kind string
			k    engine.PendKeys
		}{
			{engine.PendKeyRequestIdentifier, engine.PendKeys{RequesterHolder: requester, RequestIDs: full.RequestIDs}},
			{engine.PendKeyItemTraceNumber, engine.PendKeys{RequesterHolder: requester, ItemTraceNumbers: full.ItemTraceNumbers}},
			{"both weak kinds", engine.PendKeys{RequesterHolder: requester, RequestIDs: full.RequestIDs, ItemTraceNumbers: full.ItemTraceNumbers}},
		} {
			wantVerdict(t, lookup(t, l, requester, probe.k), engine.PendMatchNoStrongKey)
		}
	})

	// A decided claim stays findable: an inquiry about it must resolve to the
	// decision rather than to "no such authorization".
	t.Run("lookup finds a decided claim", func(t *testing.T) {
		l := newLedger(t)
		pend(t, l, pciA, corrA, tPend, keys(requester))
		decide(t, l, pciA, corrA, engine.PendOutcomeApproved, tDecided, eob("eob-1"))
		wantFound(t, lookup(t, l, requester, keys(requester)), pciA, corrA)
	})

	// requesterHolder namespaces every key: another requester's identical keys
	// find nothing.
	t.Run("lookup is namespaced by requester", func(t *testing.T) {
		l := newLedger(t)
		pend(t, l, pciA, corrA, tPend, keys(requester))
		wantVerdict(t, lookup(t, l, other, keys(other)), engine.PendMatchNone)
	})

	// Two pends sharing a STRONG key the payer issued are AMBIGUOUS, and the
	// lookup changes nothing: the ledger will not guess which one the answer is
	// about. (An echo of the requester's own claim identifier is not a strong
	// key; see "an echoed claim identifier never widens the probe".)
	t.Run("an ambiguous strong key makes no ledger change", func(t *testing.T) {
		l := newLedger(t)
		shared := engine.PendKeys{RequesterHolder: requester, ClaimResponseIDs: []string{"http://example.org/PATIENT_EVENT_TRACE_NUMBER|111099"}}
		pend(t, l, pciA, corrA, tPend, shared)
		pend(t, l, pciA, corrB, tPend, shared)
		wantVerdict(t, lookup(t, l, requester, shared), engine.PendMatchAmbiguous)
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
		wantVerdict(t, lookup(t, l, requester, unrelated), engine.PendMatchNone)
	})

	// The one authorization a strong key finds must agree with every key the
	// answer states: a key of a kind it holds that is not one of its keys
	// makes the answer about another claim. A kind it holds none of is not a
	// disagreement — a payer states its preAuthRef on the decision, not on the
	// pend it answered first.
	t.Run("the strong key's claim must agree with every stated key", func(t *testing.T) {
		l := newLedger(t)
		pend(t, l, pciA, corrA, tPend, engine.PendKeys{RequesterHolder: requester,
			ClaimResponseIDs: []string{"urn:payer:claimresponse|CR-1"}, RequestIDs: []string{"urn:shn:claim|CLM-1"},
			ItemTraceNumbers: []string{"urn:shn:trace|TR-1-1", "urn:shn:trace|TR-1-2"}})
		strong := []string{"urn:payer:claimresponse|CR-1"}
		for _, tc := range []struct {
			name string
			k    engine.PendKeys
			want engine.PendMatch
		}{
			{"every stated key held", engine.PendKeys{RequesterHolder: requester, ClaimResponseIDs: strong,
				RequestIDs: []string{"urn:shn:claim|CLM-1"}, ItemTraceNumbers: []string{"urn:shn:trace|TR-1-2"}},
				engine.PendMatch{SubjectPCI: pciA, CorrelationID: corrA, Verdict: engine.PendMatchFound}},
			{"a preAuthRef the pend never held", engine.PendKeys{RequesterHolder: requester, ClaimResponseIDs: strong, PreAuthRef: "PA-NEW"},
				engine.PendMatch{SubjectPCI: pciA, CorrelationID: corrA, Verdict: engine.PendMatchFound}},
			{"another claim's trace number", engine.PendKeys{RequesterHolder: requester, ClaimResponseIDs: strong,
				ItemTraceNumbers: []string{"urn:shn:trace|TR-1-1", "urn:shn:trace|TR-9-1"}},
				engine.PendMatch{Verdict: engine.PendMatchDisagrees, Kind: engine.PendKeyItemTraceNumber}},
			{"another claim's request identifier", engine.PendKeys{RequesterHolder: requester, ClaimResponseIDs: strong,
				RequestIDs: []string{"urn:shn:claim|CLM-9"}},
				engine.PendMatch{Verdict: engine.PendMatchDisagrees, Kind: engine.PendKeyRequestIdentifier}},
			{"a second ClaimResponse identifier it does not hold", engine.PendKeys{RequesterHolder: requester,
				ClaimResponseIDs: []string{"urn:payer:claimresponse|CR-1", "urn:payer:claimresponse|CR-9"}},
				engine.PendMatch{Verdict: engine.PendMatchDisagrees, Kind: engine.PendKeyClaimResponseIdentifier}},
		} {
			if got := lookup(t, l, requester, tc.k); got != tc.want {
				t.Fatalf("%s: lookup = %+v, want %+v", tc.name, got, tc.want)
			}
		}
	})

	// A weak key shared by two claims confirms the one a strong key found and
	// never makes it ambiguous: reused example bodies share item trace
	// numbers, and the claim is still pinned by its own strong key.
	t.Run("a shared weak key does not make a strong match ambiguous", func(t *testing.T) {
		l := newLedger(t)
		const shared = "http://example.org/ITEM_TRACE_NUMBER|1"
		pend(t, l, pciA, corrA, tPend, engine.PendKeys{RequesterHolder: requester,
			ClaimResponseIDs: []string{"urn:payer:claimresponse|CR-A"}, ItemTraceNumbers: []string{shared}})
		pend(t, l, pciA, corrB, tPend, engine.PendKeys{RequesterHolder: requester,
			ClaimResponseIDs: []string{"urn:payer:claimresponse|CR-B"}, ItemTraceNumbers: []string{shared}})
		wantFound(t, lookup(t, l, requester, engine.PendKeys{RequesterHolder: requester,
			ClaimResponseIDs: []string{"urn:payer:claimresponse|CR-B"}, ItemTraceNumbers: []string{shared}}), pciA, corrB)
		wantVerdict(t, lookup(t, l, requester, engine.PendKeys{RequesterHolder: requester, ItemTraceNumbers: []string{shared}}),
			engine.PendMatchNoStrongKey)
	})

	// The EOB states the decision the ledger keeps, so it is written only when
	// the decision is. The real legs always reuse one EOB id per
	// authorization (eob-<corr>), so a losing or repeated decision writing its EOB
	// would leave the ledger saying one thing and Patient Access serving another.
	t.Run("a losing or repeated decision keeps the recorded decision's EOB", func(t *testing.T) {
		l := newLedger(t)
		pend(t, l, pciA, corrA, tPend, keys(requester))
		eobOf := func(body string) *engine.EOBRecord {
			return &engine.EOBRecord{SubjectPCI: pciA, EOBID: "eob-" + corrA, JSON: []byte(body)}
		}
		const approved = `{"resourceType":"ExplanationOfBenefit","id":"eob-corr-A","outcome":"approved"}`
		decide(t, l, pciA, corrA, engine.PendOutcomeApproved, tLater, eobOf(approved))
		for _, tc := range []struct {
			name, outcome string
			at            time.Time
		}{
			{"an earlier-dated denial", engine.PendOutcomeDenied, tEarly},
			{"the same approval again", engine.PendOutcomeApproved, tDecided},
		} {
			tr := decide(t, l, pciA, corrA, tc.outcome, tc.at, eobOf(`{"resourceType":"ExplanationOfBenefit","id":"eob-corr-A","outcome":"`+tc.name+`"}`))
			if tr.Changed {
				t.Fatalf("%s changed the decision: %+v", tc.name, tr)
			}
			if got, ok := l.EOBByID("eob-" + corrA); !ok || string(got) != approved {
				t.Fatalf("%s replaced the kept decision's EOB: %s", tc.name, got)
			}
		}
		wantDecision(t, wantState(t, l, pciA, corrA, engine.PendStateDecided), engine.PendOutcomeApproved, tLater)
		// A decision that DOES change it writes its EOB with it.
		decide(t, l, pciA, corrA, engine.PendOutcomeDenied, tLater.Add(time.Hour), eobOf(`{"resourceType":"ExplanationOfBenefit","id":"eob-corr-A","outcome":"denied"}`))
		if got, _ := l.EOBByID("eob-" + corrA); !strings.Contains(string(got), `"denied"`) {
			t.Fatalf("the winning decision's EOB was not written: %s", got)
		}
	})

	// A follow-up's own keys (its lines' trace numbers and authorization numbers)
	// never search; they only say whether the match is the authorization the
	// follow-up was about: it holds every one of them, and no other authorization
	// in the requester's namespace holds any.
	t.Run("a follow-up's own keys say whether it is about the match", func(t *testing.T) {
		l := newLedger(t)
		const traceA, traceB, shared = "urn:shn:trace|A-1", "urn:shn:trace|B-1", "urn:shn:trace|SHARED"
		pend(t, l, pciA, corrA, tPend, engine.PendKeys{RequesterHolder: requester,
			ClaimResponseIDs: []string{"urn:payer:claimresponse|CR-A"}, ItemTraceNumbers: []string{traceA, shared}})
		pend(t, l, pciA, corrB, tPend, engine.PendKeys{RequesterHolder: requester,
			ClaimResponseIDs: []string{"urn:payer:claimresponse|CR-B"}, ItemTraceNumbers: []string{traceB, shared}})
		answerA := engine.PendKeys{RequesterHolder: requester, ClaimResponseIDs: []string{"urn:payer:claimresponse|CR-A"}}
		about := func(traces ...string) []engine.PendKeyRef { return engine.PendAboutKeys(traces, nil) }
		for _, tc := range []struct {
			name  string
			about []engine.PendKeyRef
			want  bool
		}{
			{"none stated", nil, false},
			{"held by the match alone", about(traceA), true},
			{"an authorization number the match holds alone", engine.PendAboutKeys(nil, []string{"PA-A"}), false},
			{"held by the match and another", about(shared), false},
			{"one held by the match, one by another", about(traceA, traceB), false},
			{"one the match does not hold", about(traceA, "urn:shn:trace|NOWHERE"), false},
			{"held only by another", about(traceB), false},
		} {
			m, err := l.LookupPended(requester, answerA, tc.about)
			if err != nil {
				t.Fatal(err)
			}
			wantFound(t, m, pciA, corrA)
			if m.About != tc.want {
				t.Fatalf("%s: About = %v, want %v", tc.name, m.About, tc.want)
			}
		}
		// An authorization number is a preAuthRef key: one the match holds alone
		// names it.
		pend(t, l, pciA, corrA, tLater, engine.PendKeys{RequesterHolder: requester, PreAuthRef: "PA-A"})
		if m := lookup(t, l, requester, answerA); m.About {
			t.Fatalf("no follow-up keys, yet About: %+v", m)
		}
		m, err := l.LookupPended(requester, answerA, engine.PendAboutKeys(nil, []string{"PA-A"}))
		if err != nil || !m.About {
			t.Fatalf("an authorization number the match holds alone: %+v, %v", m, err)
		}
	})

	// A decision first recorded without its EOB gets it from a later restatement
	// of the kept decision; an older restatement never replaces the kept EOB.
	t.Run("a later restatement of the kept decision supplies its EOB", func(t *testing.T) {
		l := newLedger(t)
		pend(t, l, pciA, corrA, tPend, keys(requester))
		eobOf := func(body string) *engine.EOBRecord {
			return &engine.EOBRecord{SubjectPCI: pciA, EOBID: "eob-" + corrA, JSON: []byte(body)}
		}
		decide(t, l, pciA, corrA, engine.PendOutcomeApproved, tDecided, nil)
		if _, ok := l.EOBByID("eob-" + corrA); ok {
			t.Fatal("a decision recorded without an EOB has one")
		}
		const supplied = `{"resourceType":"ExplanationOfBenefit","id":"eob-corr-A","n":"supplied"}`
		if tr := decide(t, l, pciA, corrA, engine.PendOutcomeApproved, tDecided, eobOf(supplied)); tr.Changed {
			t.Fatalf("restating the kept decision changed it: %+v", tr)
		}
		if got, ok := l.EOBByID("eob-" + corrA); !ok || string(got) != supplied {
			t.Fatalf("the restatement did not supply the kept decision's EOB: %s", got)
		}
		decide(t, l, pciA, corrA, engine.PendOutcomeApproved, tEarly, eobOf(`{"resourceType":"ExplanationOfBenefit","id":"eob-corr-A","n":"older"}`))
		if got, _ := l.EOBByID("eob-" + corrA); string(got) != supplied {
			t.Fatalf("an older restatement replaced the kept EOB: %s", got)
		}
		wantDecision(t, wantState(t, l, pciA, corrA, engine.PendStateDecided), engine.PendOutcomeApproved, tDecided)
	})

	// Patient Access never serves an EOB that contradicts the ledger: when the
	// kept outcome changes and the new decision brings no EOB of its own, the
	// old decision's EOB is removed in the same write, and the write says so.
	t.Run("an outcome that changes without its EOB removes the old one", func(t *testing.T) {
		l := newLedger(t)
		pend(t, l, pciA, corrA, tPend, keys(requester))
		denied := &engine.EOBRecord{SubjectPCI: pciA, EOBID: engine.DecisionEOBID(corrA), JSON: []byte(`{"resourceType":"ExplanationOfBenefit","outcome":"denied"}`)}
		decideN(t, l, pciA, corrA, engine.PendOutcomeDenied, tDecided, "", denied)
		tr := decideN(t, l, pciA, corrA, engine.PendOutcomeApproved, tLater, "", nil)
		if !tr.Changed || !tr.EOBRemoved {
			t.Fatalf("a later approval with no EOB = %+v, want the outcome changed and the denial's EOB removed", tr)
		}
		if got, ok := l.EOBByID(engine.DecisionEOBID(corrA)); ok {
			t.Fatalf("the denial's EOB still stands after the approval: %s", got)
		}
		wantEOBGone(t, l, engine.DecisionEOBID(corrA))
		wantDecision(t, wantState(t, l, pciA, corrA, engine.PendStateDecided), engine.PendOutcomeApproved, tLater)
		// The approval's own EOB, when it arrives, is written.
		approved := &engine.EOBRecord{SubjectPCI: pciA, EOBID: engine.DecisionEOBID(corrA), JSON: []byte(`{"resourceType":"ExplanationOfBenefit","outcome":"approved"}`)}
		if tr := decideN(t, l, pciA, corrA, engine.PendOutcomeApproved, tLater, "", approved); tr.EOBRemoved {
			t.Fatalf("restating the kept decision removed an EOB: %+v", tr)
		}
		if got, ok := l.EOBByID(engine.DecisionEOBID(corrA)); !ok || string(got) != string(approved.JSON) {
			t.Fatalf("the kept decision's EOB = %s,%v", got, ok)
		}
		// A losing answer removes nothing.
		if tr := decideN(t, l, pciA, corrA, engine.PendOutcomeDenied, tEarly, "", nil); tr.EOBRemoved || tr.Changed {
			t.Fatalf("an earlier denial = %+v, want no change and nothing removed", tr)
		}
		if _, ok := l.EOBByID(engine.DecisionEOBID(corrA)); !ok {
			t.Fatal("a losing answer removed the kept decision's EOB")
		}
	})

	// A later-dated re-pend reopens a decided authorization; the decision's EOB
	// no longer states what the ledger keeps, so it is removed with it. A stale
	// re-pend leaves the decision, and its EOB, as they are.
	t.Run("a superseding re-pend removes the decision's EOB", func(t *testing.T) {
		l := newLedger(t)
		pend(t, l, pciA, corrA, tPend, keys(requester))
		decideN(t, l, pciA, corrA, engine.PendOutcomeApproved, tDecided, "", &engine.EOBRecord{SubjectPCI: pciA, EOBID: engine.DecisionEOBID(corrA), JSON: []byte(`{"resourceType":"ExplanationOfBenefit"}`)})
		if tr := pend(t, l, pciA, corrA, tEarly, keys(requester)); tr.EOBRemoved {
			t.Fatalf("a stale re-pend removed the EOB: %+v", tr)
		}
		if _, ok := l.EOBByID(engine.DecisionEOBID(corrA)); !ok {
			t.Fatal("a stale re-pend removed the decision's EOB")
		}
		if tr := pend(t, l, pciA, corrA, tLater, keys(requester)); tr.Event != engine.DecisionSupersededEvent || !tr.EOBRemoved {
			t.Fatalf("a superseding re-pend = %+v, want %s with the EOB removed", tr, engine.DecisionSupersededEvent)
		}
		if got, ok := l.EOBByID(engine.DecisionEOBID(corrA)); ok {
			t.Fatalf("the superseded decision's EOB still stands: %s", got)
		}
		wantEOBGone(t, l, engine.DecisionEOBID(corrA))
	})

	// A ClaimResponse identifier the authorization also holds as one of its own
	// request identifiers is the requester's claim identifier, echoed by the
	// payer, and claims built from one body share it: it cannot make a match on
	// its own. The payer's preAuthRef can.
	t.Run("an echoed claim identifier alone names no claim", func(t *testing.T) {
		l := newLedger(t)
		const echoed = "http://example.org/PATIENT_EVENT_TRACE_NUMBER|111099"
		pend(t, l, pciA, corrA, tPend, engine.PendKeys{RequesterHolder: requester,
			ClaimResponseIDs: []string{echoed}, RequestIDs: []string{echoed}})
		answer := engine.PendKeys{RequesterHolder: requester, ClaimResponseIDs: []string{echoed}, RequestIDs: []string{echoed}}
		wantVerdict(t, lookup(t, l, requester, answer), engine.PendMatchRequesterKeyOnly)
		decideN(t, l, pciA, corrA, engine.PendOutcomeApproved, tDecided, "AUTH-A", nil)
		answer.PreAuthRef = "AUTH-A"
		wantFound(t, lookup(t, l, requester, answer), pciA, corrA)
	})

	// An echo is the requester's identifier whatever system the payer states it
	// under, and it never widens the probe: two claims sharing it are told apart
	// by the payer's preAuthRef rather than made ambiguous.
	t.Run("an echoed claim identifier never widens the probe", func(t *testing.T) {
		l := newLedger(t)
		const reqID = "http://example.org/PATIENT_EVENT_TRACE_NUMBER|111099"
		const echoedOwnSystem = "urn:payer:claimresponse|111099"
		for _, corr := range []string{corrA, corrB} {
			pend(t, l, pciA, corr, tPend, engine.PendKeys{RequesterHolder: requester,
				ClaimResponseIDs: []string{echoedOwnSystem}, RequestIDs: []string{reqID}})
		}
		// The value echoed under the payer's own system is still the requester's.
		wantVerdict(t, lookup(t, l, requester, engine.PendKeys{RequesterHolder: requester, ClaimResponseIDs: []string{echoedOwnSystem}}),
			engine.PendMatchRequesterKeyOnly)
		// B's authorization number names B alone, the shared echo notwithstanding.
		decideN(t, l, pciA, corrB, engine.PendOutcomeApproved, tDecided, "AUTH-B", nil)
		wantFound(t, lookup(t, l, requester, engine.PendKeys{RequesterHolder: requester,
			ClaimResponseIDs: []string{echoedOwnSystem}, PreAuthRef: "AUTH-B"}), pciA, corrB)
	})

	// A decision with no ledger row before it (the payer decided at submit) is
	// filed under its requester with every key of the submission, so a later
	// follow-up finds it and judges whether it is about it like any other claim.
	t.Run("a decision with no prior row is filed under its keys", func(t *testing.T) {
		l := newLedger(t)
		const trace = "urn:shn:trace|SUBMIT-1"
		tr, err := l.RecordDecision(pciA, corrA, engine.PendOutcomeDenied, tDecided, engine.PendKeys{RequesterHolder: requester,
			ClaimResponseIDs: []string{"urn:payer:claimresponse|CR-D"}, ItemTraceNumbers: []string{trace}, PreAuthRef: "AUTH-D"}, nil)
		if err != nil || !tr.Changed {
			t.Fatalf("RecordDecision = %+v,%v", tr, err)
		}
		if rec := mustRecord(t, l, pciA, corrA); rec.RequesterHolder != requester {
			t.Fatalf("the decided row has requester %q, want %q", rec.RequesterHolder, requester)
		}
		m, err := l.LookupPended(requester, engine.PendKeys{RequesterHolder: requester, ClaimResponseIDs: []string{"urn:payer:claimresponse|CR-D"}},
			engine.PendAboutKeys([]string{trace}, nil))
		if err != nil || m.Verdict != engine.PendMatchFound || m.CorrelationID != corrA || !m.About {
			t.Fatalf("lookup of the decided-at-submit claim = %+v,%v, want found and About", m, err)
		}
		wantFound(t, lookup(t, l, requester, engine.PendKeys{RequesterHolder: requester, PreAuthRef: "AUTH-D"}), pciA, corrA)
		// Another requester's namespace holds nothing of it.
		wantVerdict(t, lookup(t, l, other, engine.PendKeys{RequesterHolder: other, PreAuthRef: "AUTH-D"}), engine.PendMatchNone)
	})

	// When the payer's authorization number names the authorization, a
	// ClaimResponse identifier the answer states need not be one it holds: a
	// payer may issue a new ClaimResponse for its decision. Two authorizations
	// holding one authorization number stay ambiguous.
	t.Run("a match by preAuthRef needs no ClaimResponse identifier agreement", func(t *testing.T) {
		l := newLedger(t)
		pend(t, l, pciA, corrA, tPend, engine.PendKeys{RequesterHolder: requester,
			ClaimResponseIDs: []string{"urn:payer:claimresponse|CR-1"}, PreAuthRef: "PA-1", ItemTraceNumbers: []string{"urn:shn:trace|TRACE-1"}})
		newResponse := engine.PendKeys{RequesterHolder: requester, ClaimResponseIDs: []string{"urn:payer:claimresponse|CR-2"}, PreAuthRef: "PA-1"}
		wantFound(t, lookup(t, l, requester, newResponse), pciA, corrA)
		// The other kinds still have to agree.
		withTrace := newResponse
		withTrace.ItemTraceNumbers = []string{"urn:shn:trace|TRACE-9"}
		if got := lookup(t, l, requester, withTrace); got.Verdict != engine.PendMatchDisagrees || got.Kind != engine.PendKeyItemTraceNumber {
			t.Fatalf("lookup = %+v, want disagrees on %s", got, engine.PendKeyItemTraceNumber)
		}
		// A new ClaimResponse identifier alone finds nothing.
		wantVerdict(t, lookup(t, l, requester, engine.PendKeys{RequesterHolder: requester, ClaimResponseIDs: newResponse.ClaimResponseIDs}), engine.PendMatchNone)
		// Two authorizations holding one authorization number: ambiguous.
		pend(t, l, pciA, corrB, tPend, engine.PendKeys{RequesterHolder: requester, PreAuthRef: "PA-1"})
		wantVerdict(t, lookup(t, l, requester, newResponse), engine.PendMatchAmbiguous)
	})

	// The kept decision's authorization number names the authorization from
	// then on, so a later follow-up that states it finds the authorization, and
	// can be about it. A losing decision's number is not indexed.
	t.Run("the kept decision's authorization number is indexed", func(t *testing.T) {
		l := newLedger(t)
		pend(t, l, pciA, corrA, tPend, engine.PendKeys{RequesterHolder: requester, ClaimResponseIDs: []string{"urn:payer:claimresponse|CR-1"}})
		decideN(t, l, pciA, corrA, engine.PendOutcomeApproved, tLater, "AUTH-0001", nil)
		byNumber := engine.PendKeys{RequesterHolder: requester, PreAuthRef: "AUTH-0001"}
		wantFound(t, lookup(t, l, requester, byNumber), pciA, corrA)
		m, err := l.LookupPended(requester, byNumber, engine.PendAboutKeys(nil, []string{"AUTH-0001"}))
		if err != nil || !m.About {
			t.Fatalf("a follow-up naming the authorization number = %+v,%v, want About", m, err)
		}
		decideN(t, l, pciA, corrA, engine.PendOutcomeDenied, tEarly, "AUTH-LOSING", nil)
		wantVerdict(t, lookup(t, l, requester, engine.PendKeys{RequesterHolder: requester, PreAuthRef: "AUTH-LOSING"}), engine.PendMatchNone)
		// Another authorization decided under the same number makes it ambiguous.
		pend(t, l, pciA, corrB, tPend, engine.PendKeys{RequesterHolder: requester, ClaimResponseIDs: []string{"urn:payer:claimresponse|CR-B"}})
		decideN(t, l, pciA, corrB, engine.PendOutcomeApproved, tLater, "AUTH-0001", nil)
		wantVerdict(t, lookup(t, l, requester, byNumber), engine.PendMatchAmbiguous)
	})

	// A later-dated restatement of the kept decision advances the date it is
	// kept by, so an older restatement can never be its latest word.
	t.Run("a later restatement advances the kept decision's date", func(t *testing.T) {
		l := newLedger(t)
		pend(t, l, pciA, corrA, tPend, keys(requester))
		decideN(t, l, pciA, corrA, engine.PendOutcomeApproved, tEarly, "", nil)
		decideN(t, l, pciA, corrA, engine.PendOutcomeApproved, tLater, "", &engine.EOBRecord{SubjectPCI: pciA, EOBID: engine.DecisionEOBID(corrA), JSON: []byte(`{"n":"later"}`)})
		wantDecision(t, wantState(t, l, pciA, corrA, engine.PendStateDecided), engine.PendOutcomeApproved, tLater)
		decideN(t, l, pciA, corrA, engine.PendOutcomeApproved, tDecided, "", &engine.EOBRecord{SubjectPCI: pciA, EOBID: engine.DecisionEOBID(corrA), JSON: []byte(`{"n":"between"}`)})
		if got, _ := l.EOBByID(engine.DecisionEOBID(corrA)); string(got) != `{"n":"later"}` {
			t.Fatalf("a restatement dated between replaced the later one's EOB: %s", got)
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
			_, err := l.RecordDecision(pciA, corrA, engine.PendOutcomeApproved, tDecided, engine.PendKeys{}, bad.eob)
			if !errors.Is(err, engine.ErrPendEOBInvalid) {
				t.Fatalf("%s: err = %v, want ErrPendEOBInvalid", bad.name, err)
			}
			wantState(t, l, pciA, corrA, engine.PendStatePended)
			if got, ok := l.EOBsForPatient(pciA); ok || len(got) != 0 {
				t.Fatalf("%s: %d EOBs were written by a refused decision", bad.name, len(got))
			}
		}
	})

	// An EOB id is the payer's "eob-" + the exchange's correlation id, and a
	// requester can choose its correlation id. A second patient's exchange that
	// reuses the first's correlation id — the SAME correlation id, under two
	// patients, which is the collision a requester can actually cause — must not
	// move its EOB into the first patient's Patient Access list, and the refused
	// EOB leaves its decision pended.
	t.Run("an EOB id owned by another patient is refused on decision", func(t *testing.T) {
		l := newLedger(t)
		const (
			shared = "corr-shared"
			eobID  = "eob-" + shared
			pciB   = "PCI-B"
		)
		first := []byte(`{"resourceType":"ExplanationOfBenefit","id":"shared","patient":{"reference":"Patient/A"}}`)
		pend(t, l, pciA, shared, tPend, keys(requester))
		decide(t, l, pciA, shared, engine.PendOutcomeApproved, tDecided, &engine.EOBRecord{SubjectPCI: pciA, EOBID: eobID, JSON: first})
		pend(t, l, pciB, shared, tPend, engine.PendKeys{RequesterHolder: requester, RequestIDs: []string{"urn:shn:claim|CLM-B"}})
		_, err := l.RecordDecision(pciB, shared, engine.PendOutcomeApproved, tDecided, engine.PendKeys{},
			&engine.EOBRecord{SubjectPCI: pciB, EOBID: eobID, JSON: []byte(`{"resourceType":"ExplanationOfBenefit","id":"shared","patient":{"reference":"Patient/B"}}`)})
		if !errors.Is(err, engine.ErrEOBSubjectMismatch) {
			t.Fatalf("err = %v, want ErrEOBSubjectMismatch", err)
		}
		wantState(t, l, pciB, shared, engine.PendStatePended)
		wantDecision(t, wantState(t, l, pciA, shared, engine.PendStateDecided), engine.PendOutcomeApproved, tDecided)
		if got, ok := l.EOBsForPatient(pciA); !ok || len(got) != 1 || string(got[0]) != string(first) {
			t.Fatalf("the first patient's EOB changed: %s, found=%v", got, ok)
		}
		if got, ok := l.EOBsForPatient(pciB); ok || len(got) != 0 {
			t.Fatalf("the refused EOB became visible to the second patient: %s, found=%v", got, ok)
		}
	})

	// The owner of an EOB id re-recording it through a decision replaces its
	// bytes and stays ONE EOB: the guard is about the patient, not the id.
	t.Run("the owning patient re-records its EOB id on decision", func(t *testing.T) {
		l := newLedger(t)
		pend(t, l, pciA, corrA, tPend, keys(requester))
		decide(t, l, pciA, corrA, engine.PendOutcomeApproved, tDecided, eob("eob-"+corrA))
		again := &engine.EOBRecord{SubjectPCI: pciA, EOBID: "eob-" + corrA, JSON: []byte(`{"resourceType":"ExplanationOfBenefit","id":"again"}`)}
		decide(t, l, pciA, corrA, engine.PendOutcomeDenied, tLater, again)
		wantDecision(t, wantState(t, l, pciA, corrA, engine.PendStateDecided), engine.PendOutcomeDenied, tLater)
		if got, ok := l.EOBsForPatient(pciA); !ok || len(got) != 1 || string(got[0]) != string(again.JSON) {
			t.Fatalf("re-recorded EOB = %s, found=%v, want the one re-recorded", got, ok)
		}
	})

	// The pre-forward check's first question (engine.EOBOwnerLookup): which
	// patient an EOB id is filed for — and "none" is not an error.
	t.Run("EOB owner lookup", func(t *testing.T) {
		l := newLedger(t)
		lookup, ok := l.(engine.EOBOwnerLookup)
		if !ok {
			t.Fatal("the backend does not answer EOBOwner")
		}
		if owner, found, err := lookup.EOBOwner("eob-" + corrA); err != nil || found || owner != "" {
			t.Fatalf("an unfiled id = %q,%v,%v, want not found", owner, found, err)
		}
		if err := l.RecordEOB(pciA, "eob-"+corrA, []byte(`{"resourceType":"ExplanationOfBenefit"}`)); err != nil {
			t.Fatal(err)
		}
		if owner, found, err := lookup.EOBOwner("eob-" + corrA); err != nil || !found || owner != pciA {
			t.Fatalf("a filed id = %q,%v,%v, want %q", owner, found, err, pciA)
		}
		// The same id refused for another patient leaves the owner as it was.
		_ = l.RecordEOB("PCI-B", "eob-"+corrA, []byte(`{"resourceType":"ExplanationOfBenefit"}`))
		if owner, _, _ := lookup.EOBOwner("eob-" + corrA); owner != pciA {
			t.Fatalf("owner after a refused write = %q, want %q", owner, pciA)
		}
	})

	// The pre-forward check's second question (engine.PendCorrelationLookup):
	// whether another patient's authorization is still awaiting its decision
	// under a correlation id. The asking patient's own row, a decided row and a
	// row under another correlation id are not.
	t.Run("pended-for-another-patient lookup", func(t *testing.T) {
		l := newLedger(t)
		lookup, ok := l.(engine.PendCorrelationLookup)
		if !ok {
			t.Fatal("the backend does not answer PendedForOtherSubject")
		}
		const pciB, pciC = "PCI-B", "PCI-C"
		want := func(t *testing.T, corr, asking, wantPCI string, wantFound bool) {
			t.Helper()
			other, found, err := lookup.PendedForOtherSubject(corr, asking)
			if err != nil || found != wantFound || other != wantPCI {
				t.Fatalf("PendedForOtherSubject(%s,%s) = %q,%v,%v, want %q,%v", corr, asking, other, found, err, wantPCI, wantFound)
			}
		}
		want(t, corrA, pciA, "", false)
		pend(t, l, pciA, corrA, tPend, keys(requester))
		want(t, corrA, pciA, "", false) // one's own pend is not another patient's
		want(t, corrA, pciB, pciA, true)
		want(t, corrB, pciB, "", false) // another correlation id
		// An amendment in progress is still awaiting its decision.
		if claimed, _, err := l.BeginClaimUpdateReason(pciA, corrA); err != nil || !claimed {
			t.Fatalf("begin = %v,%v", claimed, err)
		}
		want(t, corrA, pciB, pciA, true)
		// A decided authorization still holds its correlation id, with or
		// without its decision EOB.
		decide(t, l, pciA, corrA, engine.PendOutcomeApproved, tDecided, nil)
		want(t, corrA, pciB, pciA, true)
		// With two others under the id, the least PCI answers, on every backend.
		pend(t, l, pciC, corrA, tPend, engine.PendKeys{RequesterHolder: requester, RequestIDs: []string{"urn:shn:claim|CLM-C"}})
		pend(t, l, pciB, corrA, tPend, engine.PendKeys{RequesterHolder: requester, RequestIDs: []string{"urn:shn:claim|CLM-B"}})
		want(t, corrA, pciA, pciB, true)
		want(t, corrA, pciB, pciA, true)
		want(t, corrA, pciC, pciA, true)
	})

	// The same guard on the EOB write that has no ledger decision.
	t.Run("an EOB id owned by another patient is refused on record", func(t *testing.T) {
		l := newLedger(t)
		first := []byte(`{"resourceType":"ExplanationOfBenefit","id":"a"}`)
		if err := l.RecordEOB(pciA, "eob-corr-shared", first); err != nil {
			t.Fatal(err)
		}
		if err := l.RecordEOB("PCI-B", "eob-corr-shared", []byte(`{"resourceType":"ExplanationOfBenefit","id":"b"}`)); !errors.Is(err, engine.ErrEOBSubjectMismatch) {
			t.Fatalf("err = %v, want ErrEOBSubjectMismatch", err)
		}
		if got, ok := l.EOBsForPatient(pciA); !ok || len(got) != 1 || string(got[0]) != string(first) {
			t.Fatalf("the first patient's EOB changed: %s, found=%v", got, ok)
		}
		if got, ok := l.EOBsForPatient("PCI-B"); ok || len(got) != 0 {
			t.Fatalf("the refused EOB became visible: %s, found=%v", got, ok)
		}
		// The same patient re-recording its own EOB id replaces it, as before.
		again := []byte(`{"resourceType":"ExplanationOfBenefit","id":"a","status":"active"}`)
		if err := l.RecordEOB(pciA, "eob-corr-shared", again); err != nil {
			t.Fatalf("re-recording one's own EOB id: %v", err)
		}
		if got, _ := l.EOBsForPatient(pciA); len(got) != 1 || string(got[0]) != string(again) {
			t.Fatalf("re-recorded EOB = %s", got)
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
		if _, err := l.LookupPended("", keys(requester), nil); !errors.Is(err, engine.ErrPendRequesterRequired) {
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
			if _, err := l.RecordDecision(pciA, corrA, outcome, tDecided, engine.PendKeys{}, nil); !errors.Is(err, engine.ErrPendOutcomeInvalid) {
				t.Fatalf("outcome %q: err = %v, want ErrPendOutcomeInvalid", outcome, err)
			}
			wantState(t, l, pciA, corrA, engine.PendStatePended)
		}
	})

	// A decision for an authorization another requester submitted is refused, as
	// a re-pend is: it would land on that requester's authorization, and its
	// authorization number would name it there.
	t.Run("a decision from another requester is refused", func(t *testing.T) {
		l := newLedger(t)
		pend(t, l, pciA, corrA, tPend, keys(requester))
		denied := &engine.EOBRecord{SubjectPCI: pciA, EOBID: engine.DecisionEOBID(corrA), JSON: []byte(`{"resourceType":"ExplanationOfBenefit"}`)}
		if _, err := l.RecordDecision(pciA, corrA, engine.PendOutcomeDenied, tDecided, engine.PendKeys{RequesterHolder: other, PreAuthRef: "AUTH-OTHER"}, denied); !errors.Is(err, engine.ErrPendRequesterMismatch) {
			t.Fatalf("err = %v, want ErrPendRequesterMismatch", err)
		}
		wantState(t, l, pciA, corrA, engine.PendStatePended)
		if _, ok := l.EOBByID(engine.DecisionEOBID(corrA)); ok {
			t.Fatal("the refused decision wrote its EOB")
		}
		wantVerdict(t, lookup(t, l, requester, engine.PendKeys{RequesterHolder: requester, PreAuthRef: "AUTH-OTHER"}), engine.PendMatchNone)
		// The authorization's own requester still decides it.
		decideN(t, l, pciA, corrA, engine.PendOutcomeApproved, tDecided, "AUTH-OWN", nil)
		wantState(t, l, pciA, corrA, engine.PendStateDecided)
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
		wantVerdict(t, lookup(t, l, requester, engine.PendKeys{RequesterHolder: requester}), engine.PendMatchNoStrongKey)
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
		claimKeys := engine.PendKeys{RequesterHolder: requester, RequestIDs: []string{"urn:shn:claim|CLM-1"}, ClaimResponseIDs: []string{"urn:payer:claimresponse|CR-1"}}
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
		wantFound(t, lookup(t, l, requester, claimKeys), pciA, corrA)

		// Finalize removes the row AND its index entries.
		if err := l.FinalizeClaimUpdate(pciA, corrA); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := l.PendRecordOf(pciA, corrA); ok || err != nil {
			t.Fatalf("the finalized row survived: %v,%v", ok, err)
		}
		wantVerdict(t, lookup(t, l, requester, claimKeys), engine.PendMatchNone)

		// A fresh authorization reusing the same identifier is found, and is NOT
		// ambiguous — which it would be if the finalized row's entry had leaked.
		pend(t, l, pciA, corrB, tLater, claimKeys)
		wantFound(t, lookup(t, l, requester, claimKeys), pciA, corrB)
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
		wantFound(t, lookup(t, l, requester, keys(requester)), pciA, corrA)

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
		pend(t, l, pciA, corrA, tPend, engine.PendKeys{RequesterHolder: requester, RequestIDs: []string{"urn:shn:claim|CLM-1"}, ClaimResponseIDs: []string{"urn:payer:claimresponse|CR-1"}})
		pend(t, l, pciA, corrA, tLater, engine.PendKeys{RequesterHolder: requester, PreAuthRef: "PA-2"})
		for _, k := range []engine.PendKeys{
			// The first response's strong key, confirmed by its request identifier.
			{RequesterHolder: requester, RequestIDs: []string{"urn:shn:claim|CLM-1"}, ClaimResponseIDs: []string{"urn:payer:claimresponse|CR-1"}},
			{RequesterHolder: requester, PreAuthRef: "PA-2"},
		} {
			wantFound(t, lookup(t, l, requester, k), pciA, corrA)
		}
	})
}
