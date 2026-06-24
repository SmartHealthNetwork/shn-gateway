package engine

import (
	"context"
	"net/http"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// adjTestClock is the fixed clock used by sandbox-adjudicator tests.
var adjTestClock = time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC)

// adjTestCPT is the CPT code in the UC-03/approved fixture.
const adjTestCPT = "72148"

// newSandboxResponderForTest builds a sandboxResponder backed by a fresh
// StubHolderData (satisfies both SystemOfRecord and Store) and a fixed clock.
func newSandboxResponderForTest(t *testing.T) LegResponder {
	t.Helper()
	data := NewStubHolderData()
	clock := func() time.Time { return adjTestClock }
	adj := NewSandboxAdjudicator(data, clock)
	return NewSandboxResponder(adj, data, data, clock)
}

// TestSandbox_PASClaimUpdateNative_Finalizes is CANARY #1 of the four-cell relocation: the
// CONFORMANT sandbox update case (case "pas-claim-update") completes the in-process
// pend→approve resolution (FinalizeClaimUpdate) — UC-04's distinctive capability now on the
// conformant leg. It mirrors the minimized case "pas-claim-update" (adjudicator.go:375-425) but
// reads the prior-claim key via parseConformantPASUpdateFacts and the QR/member/hasDR via
// parseConformantPASSubjects (not the strict ParseClaimBundle). No EOB on the update leg.
func TestSandbox_PASClaimUpdateNative_Finalizes(t *testing.T) {
	bundle := originatorBuiltConformantUpdateBundle(t) // related[prior]=convergence-pas-submit-0001, hasDR=true
	const origCorr = "convergence-pas-submit-0001"
	const pci = "pci:conf-update"

	newSeeded := func(t *testing.T) (LegResponder, *StubHolderData) {
		t.Helper()
		data := NewStubHolderData()
		clock := func() time.Time { return adjTestClock }
		r := NewSandboxResponder(NewSandboxAdjudicator(data, clock), data, data, clock)
		return r, data
	}

	t.Run("prior pend -> approved -> Commit FinalizeClaimUpdate, Rollback armed, no EOB", func(t *testing.T) {
		r, data := newSeeded(t)
		_ = data.RecordPendedClaim(pci, origCorr)
		res, err := r.Handle(context.Background(), "pas-claim-update", "corr-upd", pci, bundle)
		if err != nil || res.Status != 0 {
			t.Fatalf("conformant sandbox update: err=%v status=%d msg=%s", err, res.Status, res.Message)
		}
		if res.Commit == nil || res.Rollback == nil {
			t.Fatalf("approved update must carry FinalizeClaimUpdate Commit + armed Rollback")
		}
		if len(res.SideEffectFHIR) != 0 {
			t.Fatalf("update leg must emit NO EOB; got %d", len(res.SideEffectFHIR))
		}
		// The decision is approved (a parseable ClaimResponse with a preAuthRef).
		parsed, perr := shnsdk.ParseClaimResponse(res.ResponseFHIR)
		if perr != nil || parsed.Outcome != "approved" || parsed.PreAuthRef == "" {
			t.Fatalf("want approved ClaimResponse + preAuthRef, got %+v err=%v", parsed, perr)
		}
		// Commit completes the pend→approve transition: a replayed update finds nothing (409).
		if err := res.Commit(); err != nil {
			t.Fatalf("Commit (FinalizeClaimUpdate): %v", err)
		}
		replay, _ := r.Handle(context.Background(), "pas-claim-update", "corr-upd2", pci, bundle)
		if replay.Status != http.StatusConflict {
			t.Fatalf("after Finalize, a replayed update must 409 (claim gone), got %d", replay.Status)
		}
	})

	t.Run("no prior pend -> 409 (derived-ledger fail-safe)", func(t *testing.T) {
		r, _ := newSeeded(t) // NOT seeded
		res, _ := r.Handle(context.Background(), "pas-claim-update", "corr-upd", pci, bundle)
		if res.Status != http.StatusConflict {
			t.Fatalf("no prior pend must be 409, got %d", res.Status)
		}
	})
}
