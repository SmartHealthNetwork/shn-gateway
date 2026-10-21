package engine

// pas_no_decision_eob_test.go — a payer gateway records the payer's decision
// on its pend ledger and builds no ExplanationOfBenefit from it. The store
// has no EOB API to write one through (TestStoreAndLedgerHaveNoEOBAPI).

import (
	"bytes"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The observer kinds a payer gateway raised while it still built a decision
// EOB. Literal here: the constants are gone, and no leg may raise them.
var retiredDecisionEOBEvents = []string{"pa.decision-eob-not-written", "pend.inquiry-eob-withheld", "pend.eob-removed"}

// recordingEvents wraps p's observer so every event the legs raise is kept.
func recordingEvents(p *levelPayer) func() []ObserverEvent {
	var mu sync.Mutex
	var all []ObserverEvent
	prev := p.g.cfg.Observer
	p.g.cfg.Observer = func(e ObserverEvent) {
		mu.Lock()
		all = append(all, e)
		mu.Unlock()
		if prev != nil {
			prev(e)
		}
	}
	return func() []ObserverEvent {
		mu.Lock()
		defer mu.Unlock()
		return append([]ObserverEvent(nil), all...)
	}
}

// wantNoRetiredEvent fails when any event is of a retired decision EOB kind.
func wantNoRetiredEvent(t *testing.T, events []ObserverEvent) {
	t.Helper()
	for _, e := range events {
		for _, kind := range retiredDecisionEOBEvents {
			if e.Kind == kind {
				t.Fatalf("a leg raised %s: %+v", kind, e)
			}
		}
	}
}

// hcpcsSubmitBundle is a conformant $submit whose order is coded in HCPCS
// (a home oxygen system), built by the SDK as an originator builds it.
func hcpcsSubmitBundle(t *testing.T) []byte {
	t.Helper()
	sr := []byte(`{"resourceType":"ServiceRequest","id":"sr-hcpcs","status":"active","intent":"order","subject":{"reference":"Patient/MBR-COVERED"},"code":{"coding":[{"system":"http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets","code":"E0424","display":"Stationary Oxygen System"}]}}`)
	b, err := shnsdk.BuildConformantClaimBundleExact(shnsdk.ConformantClaimInputs{PASClaimFacts: testClaimFacts(), Coverage: testMemberCoverage("MBR-COVERED"), Insurer: testPayerOrganization(shnsdk.CMSPayerIdentity),
		Provider:       testRequestingProvider(),
		MemberIDSystem: shnsdk.MemberSystem,
		SR:             sr, PatientRef: "Patient/MBR-COVERED", CoverageRef: "Coverage/MBR-COVERED", MemberID: "MBR-COVERED",
		Corr: "corr-hcpcs-submit", Created: fixedClock(),
		Payer: shnsdk.CMSPayerIdentity,
	})
	if err != nil {
		t.Fatalf("hcpcsSubmitBundle: %v", err)
	}
	return b
}

// TestPASDecision_RecordedWithNoEOB: a decided submission and a decided
// inquiry, at every level, relay the payer's answer exactly and record the
// decision on the ledger (decided, with the payer's outcome), raising no
// event about an ExplanationOfBenefit.
func TestPASDecision_RecordedWithNoEOB(t *testing.T) {
	approved := fixturePASResponse(t, approvedClaimResponse(nil), true)
	denied := fixturePASResponse(t, deniedClaimResponse(reviewActionExt("A3", "Not Certified", "", ""), nil), true)
	for _, row := range []struct {
		name    string
		request []byte
		answer  []byte
		outcome string
	}{
		{"submit approved", originatorBuiltConformantBundle(t, "MBR-COVERED"), approved, PendOutcomeApproved},
		{"submit denied", originatorBuiltConformantBundle(t, "MBR-COVERED"), denied, PendOutcomeDenied},
		{"submit, HCPCS order", hcpcsSubmitBundle(t), approved, PendOutcomeApproved},
	} {
		for _, level := range allLevels {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				p := newLevelPayer(t, level)
				events := recordingEvents(p)
				p.partner.respByPath[pasSubmitPath] = row.answer
				got := p.send(t, "pas-claim", "", row.request)
				if got.status != http.StatusOK || !bytes.Equal(got.body, row.answer) {
					t.Fatalf("the payer's answer must be relayed exactly: %d %s", got.status, got.body)
				}
				if rec, found := p.pendOf(t, got.corr); !found || rec.State != PendStateDecided || rec.Outcome != row.outcome {
					t.Fatalf("the decision must be recorded: found=%v %+v, want decided/%s", found, rec, row.outcome)
				}
				wantNoRetiredEvent(t, events())
			})
		}
	}
	for _, level := range allLevels {
		t.Run("inquiry by trace number/"+level.String(), func(t *testing.T) {
			p := newLevelPayer(t, level)
			events := recordingEvents(p)
			p.seedInquiryPend(t, "corr-submit-1")
			got := p.send(t, "pas-claim-inquire", "", inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"))
			if got.status != http.StatusOK || !bytes.Equal(got.body, decidedAnswer(t)) {
				t.Fatalf("the payer's answer must be relayed exactly: %d %s", got.status, got.body)
			}
			if rec, _ := p.pendOf(t, "corr-submit-1"); rec.State != PendStateDecided || rec.Outcome != PendOutcomeApproved {
				t.Fatalf("the inquiry must record the decision: %+v", rec)
			}
			wantNoRetiredEvent(t, events())
		})
	}
}

// TestPASDecision_NoteOutsideNoteTypeIsRecorded: a decision whose process
// note is typed outside NoteType is relayed exactly and recorded like any
// other, and no leg reports a record it could not write.
func TestPASDecision_NoteOutsideNoteTypeIsRecorded(t *testing.T) {
	answer := fixturePASResponse(t, withNote(deniedClaimResponse(reviewActionExt("A3", "Not Certified", "", ""), nil), "email", "Reply by email."), true)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := newLevelPayer(t, level)
			events := recordingEvents(p)
			p.partner.respByPath[pasSubmitPath] = answer
			got := p.send(t, "pas-claim", "", originatorBuiltConformantBundle(t, "MBR-COVERED"))
			if got.status != http.StatusOK || !bytes.Equal(got.body, answer) {
				t.Fatalf("the payer's decision must be relayed exactly, got %d %s", got.status, got.body)
			}
			if rec, found := p.pendOf(t, got.corr); !found || rec.State != PendStateDecided || rec.Outcome != PendOutcomeDenied {
				t.Fatalf("the decision must be recorded: found=%v %+v", found, rec)
			}
			if len(p.sideRecord) != 0 || len(p.skipped) != 0 {
				t.Fatalf("a recorded decision reports nothing unwritten: %+v %+v", p.sideRecord, p.skipped)
			}
			wantNoRetiredEvent(t, events())
		})
	}
}

// TestPASInquire_UnattributableDecisionsRaiseNoEOBEvent: a decision learned
// from an inquiry about another claim is recorded on its own authorization,
// and a later-dated one flips it, with no event about an ExplanationOfBenefit
// withheld or removed (there is none).
func TestPASInquire_UnattributableDecisionsRaiseNoEOBEvent(t *testing.T) {
	f := newInquiryLedgerFixture(t, PendKeys{})
	const traceB = "http://provider.example/trn|TRN-B"
	f.pend(t, "corr-claim-D", PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{"http://example.org/PATIENT_EVENT_TRACE_NUMBER|D-1"}, ItemTraceNumbers: []string{inquiryTraceKey}})
	f.pend(t, corrClaimB, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{crClaimB}, ItemTraceNumbers: []string{traceB}})

	// A denial for B, in an answer to an inquiry about B, then a later-dated
	// approval for B in an answer to the inquiry about D.
	aboutB := f.withFacts(t, "TRN-B")
	wantNoRetiredEvent(t, aboutB.applyEvents(t, inquiryRequester, inquiryAnswerOf(t, inquiryCR{id: crClaimB, traces: []string{traceB}, code: "A3", created: "2026-09-17T00:00:00Z"})))
	wantRecordedDecision(t, f.stateOf(t, corrClaimB), PendOutcomeDenied)
	wantNoRetiredEvent(t, f.applyEvents(t, inquiryRequester, inquiryAnswerOf(t, inquiryCR{id: crClaimB, preAuthRef: "AUTH-B", traces: []string{traceB}, code: "A1", created: "2026-09-20T00:00:00Z"})))
	wantRecordedDecision(t, f.stateOf(t, corrClaimB), PendOutcomeApproved)
	if got := f.stateOf(t, "corr-claim-D").State; got != PendStatePended {
		t.Fatalf("claim D was decided: %q", got)
	}
}

// TestPASSubmit_ReusedCorrelationAnotherPatientsDecisionIsRecorded: patient
// A's claim is decided under a correlation id, then patient B's claim is
// decided under the same id, through the native forward to the payer's own
// system. B's decision is relayed and recorded on B's own row, with B's
// outcome, and A's recorded decision is unchanged: two patients'
// authorizations share no record.
func TestPASSubmit_ReusedCorrelationAnotherPatientsDecisionIsRecorded(t *testing.T) {
	approved := fixturePASResponse(t, approvedClaimResponse(nil), true)
	denied := fixturePASResponse(t, deniedClaimResponse(reviewActionExt("A3", "Not Certified", "", ""), nil), true)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := newLevelPayer(t, level)
			p.corr = "corr-reused-decided"
			events := recordingEvents(p)
			pciB, _, ok := p.store.ResolvePatient("MBR-UC04")
			if !ok {
				t.Fatal("MBR-UC04 is not in the test system of record")
			}

			p.partner.respByPath[pasSubmitPath] = approved
			if got := p.send(t, "pas-claim", "", originatorBuiltConformantBundle(t, "MBR-COVERED")); got.status != http.StatusOK || !bytes.Equal(got.body, approved) {
				t.Fatalf("A's submission: %d %s", got.status, got.body)
			}
			before, found := p.pendOf(t, p.corr)
			if !found || before.State != PendStateDecided || before.Outcome != PendOutcomeApproved {
				t.Fatalf("control: A's authorization = %+v (found=%v), want decided/approved", before, found)
			}

			p.partner.respByPath[pasSubmitPath] = denied
			got := p.sendAs(t, "pas-claim", "", originatorBuiltConformantBundle(t, "MBR-UC04"), pciB)
			if got.status != http.StatusOK || !bytes.Equal(got.body, denied) {
				t.Fatalf("B's submission under A's correlation id: %d %s, want the payer's answer relayed", got.status, got.body)
			}
			rec, found, err := testLedger(p.store).PendRecordOf(pciB, p.corr)
			if err != nil || !found || rec.State != PendStateDecided || rec.Outcome != PendOutcomeDenied {
				t.Fatalf("B's decision must be recorded on B's own row: %+v (found=%v err=%v)", rec, found, err)
			}
			after, _ := p.pendOf(t, p.corr)
			wantSameDecision(t, after, before)
			if n := countEvents(events(), pendDecisionNotRecordedEvent); n != 0 || len(p.skipped) != 0 {
				t.Fatalf("B's decision went unrecorded: %d %s, skipped %+v", n, pendDecisionNotRecordedEvent, p.skipped)
			}
		})
	}
}

// TestStoreAndLedgerHaveNoEOBAPI: neither the Store seam nor the pend ledger
// has a method that names an ExplanationOfBenefit, and RecordDecision takes
// the decision and its keys alone, so no engine code and no backend can write
// a decision EOB through them.
func TestStoreAndLedgerHaveNoEOBAPI(t *testing.T) {
	checked := 0
	for _, iface := range []reflect.Type{reflect.TypeOf((*Store)(nil)).Elem(), reflect.TypeOf((*PendLedger)(nil)).Elem()} {
		for i := 0; i < iface.NumMethod(); i++ {
			m := iface.Method(i)
			checked++
			if strings.Contains(strings.ToUpper(m.Name), "EOB") {
				t.Errorf("%s.%s: the store carries an EOB method", iface.Name(), m.Name)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no Store or PendLedger method was checked")
	}
	rd, ok := reflect.TypeOf((*PendLedger)(nil)).Elem().MethodByName("RecordDecision")
	if !ok {
		t.Fatal("PendLedger has no RecordDecision")
	}
	if n := rd.Type.NumIn(); n != 5 {
		t.Errorf("RecordDecision takes %d arguments, want 5 (subject, correlation, outcome, date, keys)", n)
	}
}
