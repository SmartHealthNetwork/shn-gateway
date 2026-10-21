package engine

// pas_correlation_reuse_test.go — a correlation id is the requester's own
// trace id, and two patients' authorizations may carry the same one. The
// payer's records are keyed by its own binding of the member each request
// names, so nothing is shared between the two: each patient's submission is
// carried to the payer and answered, each binds its own ledger row, and
// neither patient's later exchange reads, amends or decides the other's
// authorization.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// reuseGateway is a counting payer gateway whose payer decides each claim and
// records the decision under the subject it is handed: approved for the member
// approve names, denied for anyone else, so a write that reached the wrong
// patient's row would change that row's outcome.
func reuseGateway(t *testing.T, approve string) (*Gateway, inboundTestRequester, *censusSoR, *int) {
	t.Helper()
	g, requester, store, calls := countingPayerGateway(t)
	approved := memberPCI(t, g, approve)
	g.cfg.Responder = countingPASResponder{
		inner: approvingPASResponder{clock: g.cfg.Clock}, calls: calls,
		commit: func(_ context.Context, corrID, subjectPCI string) func() error {
			outcome := PendOutcomeDenied
			if subjectPCI == approved {
				outcome = PendOutcomeApproved
			}
			return func() error {
				_, err := testLedger(store).RecordDecision(subjectPCI, corrID, outcome, fixedClock(), PendKeys{RequesterHolder: requester.ID})
				return err
			}
		},
	}
	return g, requester, store, calls
}

// reusedPendRow reads (pci, corr) from the fixture ledger.
func reusedPendRow(t *testing.T, store *censusSoR, pci, corr string) (PendRecord, bool) {
	t.Helper()
	rec, found, err := testLedger(store).PendRecordOf(pci, corr)
	if err != nil {
		t.Fatalf("read ledger (%s, %s): %v", pci, corr, err)
	}
	return rec, found
}

// TestPASSubmit_ReusedCorrelationAnotherPatientIsCarried: patient A's claim is
// decided under a correlation id, and patient B's claim then arrives under the
// same id. B's claim is carried to the payer and answered like any other: the
// payer is asked twice, B's decision is filed under B's own binding, and A's
// recorded decision is unchanged.
func TestPASSubmit_ReusedCorrelationAnotherPatientIsCarried(t *testing.T) {
	const corr = "corr-reused"
	g, requester, store, calls := reuseGateway(t, "MBR-COVERED")
	pciA, pciB := memberPCI(t, g, "MBR-COVERED"), memberPCI(t, g, "MBR-UC04")

	if status, body := submitAs(t, g, requester, "MBR-COVERED", corr); status != http.StatusOK || *calls != 1 {
		t.Fatalf("A's submission: status=%d calls=%d body=%s", status, *calls, body)
	}
	before, found := reusedPendRow(t, store, pciA, corr)
	if !found || before.State != PendStateDecided || before.Outcome != PendOutcomeApproved {
		t.Fatalf("control: A's authorization = %+v (found=%v), want decided/approved", before, found)
	}

	status, body := submitAs(t, g, requester, "MBR-UC04", corr)
	if status != http.StatusOK {
		t.Fatalf("B's submission under A's correlation id: status=%d body=%s, want the payer's answer", status, body)
	}
	if parsed, err := shnsdk.ParseClaimResponse(body); err != nil || parsed.Outcome != "approved" {
		t.Fatalf("B's answer is not the payer's: %+v err=%v", parsed, err)
	}
	if *calls != 2 {
		t.Fatalf("the payer was asked %d times, want twice: B's claim must reach it", *calls)
	}
	if got, found := reusedPendRow(t, store, pciB, corr); !found || got.State != PendStateDecided || got.Outcome != PendOutcomeDenied {
		t.Fatalf("B's authorization = %+v (found=%v), want B's own decision under B", got, found)
	}
	after, _ := reusedPendRow(t, store, pciA, corr)
	wantSameDecision(t, after, before)
}

// TestPASCorrelationCollision_InquiryNotStuck: patient B's authorization is
// pended under a correlation id, patient A then submits under the SAME id, and
// B later inquires. A's submission reaches the payer and is answered; B's stays
// pended through it. B's inquiry records B's decision on B's row, and A's
// recorded decision is untouched.
func TestPASCorrelationCollision_InquiryNotStuck(t *testing.T) {
	const corr = "corr-shared"
	g, requester, store, calls := reuseGateway(t, "MBR-UC04")
	pciA, pciB := memberPCI(t, g, "MBR-COVERED"), memberPCI(t, g, "MBR-UC04")
	if _, err := testLedger(store).RecordPendedKeyed(pciB, corr, fixedClock(), PendKeys{RequesterHolder: requester.ID, ClaimResponseIDs: []string{inquiryCRKey}, ItemTraceNumbers: []string{inquiryTraceKey}}); err != nil {
		t.Fatalf("B's pend: %v", err)
	}

	if status, body := submitAs(t, g, requester, "MBR-COVERED", corr); status != http.StatusOK || *calls != 1 {
		t.Fatalf("A's submission under B's correlation id: status=%d calls=%d body=%s, want the payer's answer", status, *calls, body)
	}
	aDecided, found := reusedPendRow(t, store, pciA, corr)
	if !found || aDecided.State != PendStateDecided || aDecided.Outcome != PendOutcomeDenied {
		t.Fatalf("A's authorization = %+v (found=%v), want A's own decision under A", aDecided, found)
	}
	if got, _ := reusedPendRow(t, store, pciB, corr); got.State != PendStatePended || got.Outcome != "" {
		t.Fatalf("A's submission changed B's authorization: %+v", got)
	}

	facts, fstatus, msg := parsePASInquiryFacts(inquiryBundle("MBR-UC04", "", "TRN-1", "72148"))
	if fstatus != 0 {
		t.Fatalf("B's inquiry refused: %d %s", fstatus, msg)
	}
	commit, _ := g.inquiryLedgerEffect(requester.ID, pciB, "corr-inquiry-leg", facts, decidedAnswer(t))
	if commit == nil {
		t.Fatal("B's inquiry produced no ledger write")
	}
	if err := commit(); err != nil {
		t.Fatalf("B's decision: %v", err)
	}
	if got, found := reusedPendRow(t, store, pciB, corr); !found || got.State != PendStateDecided || got.Outcome != PendOutcomeApproved {
		t.Fatalf("B's authorization = %+v (found=%v), want decided/approved", got, found)
	}
	got, _ := reusedPendRow(t, store, pciA, corr)
	wantSameDecision(t, got, aDecided)
}

// TestPASInquire_ReusedCorrelationAnotherPatientsPayerKeyRecordsNothing: A's
// and B's authorizations are both pended under one correlation id, each with
// the ClaimResponse identifier its payer issued. An inquiry bound to B whose
// answer names A's payer-issued identifier is relayed and records nothing, on
// either row: the inquiry leg's authority is its own patient, the payer's
// binding of the member the inquiry names, and the ledger re-checks that
// patient on every match (per-leg authority, here the patient binding, applied
// to what the payer gateway records; the answer still reaches the requester). The
// correlation id plays no part in the match. The control, A's own inquiry,
// records A's decision and leaves B's pended, so the row can fail.
func TestPASInquire_ReusedCorrelationAnotherPatientsPayerKeyRecordsNothing(t *testing.T) {
	const corr = "corr-shared-inquiry"
	for _, tc := range []struct {
		name, member string
		decidesA     bool
	}{
		{"control: A's own inquiry", "MBR-COVERED", true},
		{"B's inquiry naming A's payer-issued key", "MBR-UC04", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, requester := newInboundTestGateway(t, true)
			var events []ObserverEvent
			g.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
			store := g.cfg.Store.(*censusSoR)
			answer := decidedAnswer(t) // A's ClaimResponse identifier, echoing TRN-1
			g.cfg.Responder = pasResultResponder{result: LegResult{Response: testResponse(answer), ResponseSubjectForeign: true}}
			pciA, pciB := memberPCI(t, g, "MBR-COVERED"), memberPCI(t, g, "MBR-UC04")
			if _, err := testLedger(store).RecordPendedKeyed(pciA, corr, fixedClock(), PendKeys{RequesterHolder: requester.ID, ClaimResponseIDs: []string{inquiryCRKey}, ItemTraceNumbers: []string{inquiryTraceKey}}); err != nil {
				t.Fatalf("A's pend: %v", err)
			}
			if _, err := testLedger(store).RecordPendedKeyed(pciB, corr, fixedClock(), PendKeys{RequesterHolder: requester.ID, ClaimResponseIDs: []string{crClaimB}, ItemTraceNumbers: []string{inquiryTraceKey}}); err != nil {
				t.Fatalf("B's pend: %v", err)
			}

			env := shnsdk.Envelope{}
			env.Metadata.CorrelationID, env.Metadata.Sender = "corr-inquiry-leg", requester.ID
			rec := httptest.NewRecorder()
			g.handlePASInquireInbound(rec, newSignedInboundRequest(t, g, requester.ID), env,
				shnsdk.Token{Subject: memberPCI(t, g, tc.member), CorrelationID: env.Metadata.CorrelationID}, inquiryBundle(tc.member, "", "TRN-1", "72148"), "pa.pas@2.0")
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d %s, want the payer's answer relayed", rec.Code, rec.Body)
			}
			if _, body, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, rec.Body.Bytes())); err != nil || !bytes.Equal(body, answer) {
				t.Fatalf("the payer's answer was not relayed as sent (err=%v)", err)
			}
			a, _ := reusedPendRow(t, store, pciA, corr)
			b, _ := reusedPendRow(t, store, pciB, corr)
			if b.State != PendStatePended || b.Outcome != "" {
				t.Fatalf("B's authorization changed: %+v", b)
			}
			if tc.decidesA {
				if a.State != PendStateDecided || a.Outcome != PendOutcomeApproved {
					t.Fatalf("control: A's authorization %+v, want decided/approved", a)
				}
				return
			}
			if a.State != PendStatePended || a.Outcome != "" || !a.DecidedAt.IsZero() {
				t.Fatalf("B's inquiry decided A's authorization: %+v", a)
			}
			if n := countEvents(events, PendOtherSubjectEvent); n != 1 {
				t.Fatalf("events = %+v, want one %s", events, PendOtherSubjectEvent)
			}
		})
	}
}
