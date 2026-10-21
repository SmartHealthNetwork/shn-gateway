package engine

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/pa/pend"
)

// TestPendForwardersAreThePendPackage: each deprecated engine name of the pend
// ledger is the pend package's own, so code naming either reads the same. The
// types are aliases (a store implementing one implements the other), the
// errors are the same values (errors.Is holds across them), and every
// forwarder returns exactly what the pend function returns, over records and
// keys that reach each state and verdict.
func TestPendForwardersAreThePendPackage(t *testing.T) {
	var _ pend.PendLedger = PendLedger(nil)
	var _ PendLedger = (*memPendLedger)(nil)
	var _ pend.PendLedger = (*memPendLedger)(nil)
	for name, pair := range map[string][2]error{
		"requester required": {ErrPendRequesterRequired, pend.ErrPendRequesterRequired},
		"requester mismatch": {ErrPendRequesterMismatch, pend.ErrPendRequesterMismatch},
		"key too long":       {ErrPendKeyTooLong, pend.ErrPendKeyTooLong},
		"outcome invalid":    {ErrPendOutcomeInvalid, pend.ErrPendOutcomeInvalid},
	} {
		if pair[0] != pair[1] || !errors.Is(pair[0], pair[1]) {
			t.Errorf("%s: the engine's error is not the pend package's", name)
		}
	}

	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	keys := []PendKeys{
		{},
		{RequesterHolder: "req-a", RequestIDs: []string{"sys|claim-1"}},
		{RequesterHolder: "req-a", ClaimResponseIDs: []string{"sys|cr-1"}, PreAuthRef: "PA-1", ItemTraceNumbers: []string{"sys|t-1"}},
		{RequesterHolder: "req-b", PreAuthRef: string(make([]byte, MaxPendKeyBytes+1))},
		{RequesterHolder: "req-a", RequestIDs: []string{"sys|claim-2"}, ClaimResponseIDs: []string{"sys|cr-2"}, PreAuthRef: "PA-2"},
	}
	records := []PendRecord{
		{},
		{State: PendStatePended, RequesterHolder: "req-a", LastTransition: now.Add(-time.Hour)},
		{State: PendStateInProgress, RequesterHolder: "req-a", LastTransition: now.Add(-time.Minute)},
		{State: PendStateInProgress, RequesterHolder: "req-a", LastTransition: now.Add(-PendInProgressStale - time.Minute)},
		{State: PendStateDecided, RequesterHolder: "req-a", Outcome: PendOutcomeApproved, DecidedAt: now.Add(-time.Minute)},
	}
	same := func(what string, a, b any) {
		t.Helper()
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%s: the engine forwarder returned %#v, the pend package %#v", what, a, b)
		}
	}
	// Every verdict, both agreements and both echo answers must be reached, so
	// a forwarder returning a constant or dropping an argument cannot pass.
	verdicts := map[PendMatchVerdict]bool{}
	agreed, echoes := map[bool]bool{}, map[bool]bool{}
	candidateSets := [][]PendCandidate{nil, {{SubjectPCI: "pci-1", CorrelationID: "corr-1"}}, {{}, {}}}
	for _, k := range keys {
		same("ProbePendKeys", ProbePendKeys(k), pend.ProbePendKeys(k))
		same("StrongPendProbe", StrongPendProbe(k), pend.StrongPendProbe(k))
		r1, e1 := ValidatePendKeys(k)
		r2, e2 := pend.ValidatePendKeys(k)
		same("ValidatePendKeys", []any{r1, e1}, []any{r2, e2})
		for _, found := range []bool{false, true} {
			same("DecisionPendKeys", DecisionPendKeys(found, !found, "req-a", k), pend.DecisionPendKeys(found, !found, "req-a", k))
		}
		for _, hk := range keys {
			held := pend.ProbePendKeys(hk)
			a1, w1 := PendKeysAgree(held, k)
			a2, w2 := pend.PendKeysAgree(held, k)
			same("PendKeysAgree", []any{a1, w1}, []any{a2, w2})
			agreed[a2] = true
			for _, candidates := range candidateSets {
				for _, echoed := range []bool{false, true} {
					m := pend.ResolvePendMatch(k, candidates, held, echoed)
					same("ResolvePendMatch", ResolvePendMatch(k, candidates, held, echoed), m)
					verdicts[m.Verdict] = true
				}
			}
			for _, crKey := range []string{"other|claim-1", "sys|cr-1", ""} {
				e := pend.EchoedClaimIdentifier(crKey, held)
				same("EchoedClaimIdentifier", EchoedClaimIdentifier(crKey, held), e)
				echoes[e] = true
			}
		}
	}
	for _, v := range []PendMatchVerdict{PendMatchFound, PendMatchNoStrongKey, PendMatchNone, PendMatchAmbiguous, PendMatchDisagrees, PendMatchRequesterKeyOnly} {
		if !verdicts[v] {
			t.Errorf("the inputs never reach the verdict %q: widen them", v)
		}
	}
	if !agreed[true] || !agreed[false] || !echoes[true] || !echoes[false] {
		t.Errorf("the inputs reach agreements %v and echoes %v: widen them to both answers", agreed, echoes)
	}
	for _, r := range records {
		for _, found := range []bool{false, true} {
			s1, ok1, why1 := PendBegin(r, found, now)
			s2, ok2, why2 := pend.PendBegin(r, found, now)
			same("PendBegin", []any{s1, ok1, why1}, []any{s2, ok2, why2})
			n1, t1 := PendRePend(r, found, now.Add(-time.Second), now)
			n2, t2 := pend.PendRePend(r, found, now.Add(-time.Second), now)
			same("PendRePend", []any{n1, t1}, []any{n2, t2})
			for _, outcome := range []string{PendOutcomeApproved, PendOutcomeDenied, "maybe"} {
				d1, dt1 := PendDecide(r, found, outcome, now)
				d2, dt2 := pend.PendDecide(r, found, outcome, now)
				same("PendDecide", []any{d1, dt1}, []any{d2, dt2})
				same("PendDecisionKept", PendDecisionKept(r, outcome, now), pend.PendDecisionKept(r, outcome, now))
			}
		}
	}
	for _, outcome := range []string{PendOutcomeApproved, PendOutcomeDenied, "", "maybe"} {
		same("ValidatePendDecision", ValidatePendDecision(outcome), pend.ValidatePendDecision(outcome))
	}
	for _, last := range []time.Time{{}, now.Add(-time.Second), now.Add(-PendPurgeInterval - time.Second)} {
		d1, c1 := DuePendPurge(now, last)
		d2, c2 := pend.DuePendPurge(now, last)
		same("DuePendPurge", []any{d1, c1}, []any{d2, c2})
	}
}
