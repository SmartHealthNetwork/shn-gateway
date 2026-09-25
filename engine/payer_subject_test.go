package engine

// payer_subject_test.go — what the payer records about an exchange is keyed by
// its own binding of the member the request names (bindInboundSubject), never
// by the leg token's subject. The requester chooses that subject: a token
// minted for patient A, carrying patient B's request bytes and bound to their
// hash, reaches the payer, which answers about B, relays its answer, and files
// nothing under A.

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// subjectCapture records the subject each payer leg hands its content occupant
// and delegates the answer.
type subjectCapture struct {
	inner LegResponder
	got   *[]string
}

func (c subjectCapture) Handle(ctx context.Context, leg, corrID, subjectPCI string, requestFHIR []byte) (LegResult, error) {
	*c.got = append(*c.got, subjectPCI)
	return c.inner.Handle(ctx, leg, corrID, subjectPCI, requestFHIR)
}

// TestPASSubmit_AnotherPatientsTokenFilesNothingUnderIt: A's token with B's
// claim. The payer is asked about B, under B's binding; the decision it files
// (as the native responder files one, under the subject it is handed) is B's,
// and A's Patient Access read returns nothing.
func TestPASSubmit_AnotherPatientsTokenFilesNothingUnderIt(t *testing.T) {
	g, requester, store, _ := eobOwnerGateway(t)
	pciA, pciB := memberPCI(t, g, "MBR-COVERED"), memberPCI(t, g, "MBR-UC04")
	var subjects []string
	var events []ObserverEvent
	g.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
	g.cfg.Responder = subjectCapture{got: &subjects, inner: countingPASResponder{
		inner: approvingPASResponder{clock: g.cfg.Clock}, calls: new(int),
		commit: func(_ context.Context, corrID, subjectPCI string) func() error {
			return func() error {
				return store.RecordEOB(subjectPCI, decisionEOBID(corrID), []byte(`{"resourceType":"ExplanationOfBenefit","id":"decision"}`))
			}
		},
	}}

	const corr = "corr-token-for-a"
	bundle := conformantPASBundleWithQR(t, "MBR-UC04")
	env, err := shnsdk.Seal(shnsdk.Metadata{
		Sender: requester.ID, Recipient: "payer", TransactionType: "pas-claim", AuthorityFrame: "payer-coverage",
		Timestamp: g.cfg.Clock().Format(time.RFC3339), CorrelationID: corr,
	}, bundle, g.cfg.Identity.EncPub)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	tok := shnsdk.Token{Operation: "pas-submit", Subject: pciA, CorrelationID: corr}
	rec := httptest.NewRecorder()
	g.handlePASNativeInbound(rec, newSignedInboundRequest(t, g, requester.ID), env, tok, bundle, "pa.pas@2.0")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d %s, want the payer's answer relayed", rec.Code, rec.Body)
	}
	if hdr, _, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, rec.Body.Bytes())); err != nil || hdr.Status != http.StatusOK {
		t.Fatalf("framed answer: status=%d err=%v, want the payer's 200", hdr.Status, err)
	}
	if len(subjects) != 1 || subjects[0] != pciB {
		t.Fatalf("the payer was handed %q, want B's own binding %q", subjects, pciB)
	}
	if countEvents(events, SubjectBindingDiffersEvent) != 1 {
		t.Fatalf("events = %+v, want one %s", events, SubjectBindingDiffersEvent)
	}
	if got, found := store.EOBsForPatient(pciA); found || len(got) != 0 {
		t.Fatalf("A's Patient Access read returns %d EOBs from B's claim", len(got))
	}
	if got, found := store.EOBsForPatient(pciB); !found || len(got) != 1 {
		t.Fatalf("B's decision filed %d EOBs under B (found=%v), want 1", len(got), found)
	}
}

// TestPASSubmit_CorrelationCheckedAgainstTheClaimsOwnPatient: the correlation
// check asks whether the id names another patient than the one the claim is
// for. A's token does not make B's claim A's: B's claim under a correlation id
// A's pended authorization holds is refused before the payer is asked.
func TestPASSubmit_CorrelationCheckedAgainstTheClaimsOwnPatient(t *testing.T) {
	const corr = "corr-a-pended"
	g, requester, store, calls := eobOwnerGateway(t)
	pciA := memberPCI(t, g, "MBR-COVERED")
	if _, err := store.RecordPendedKeyed(pciA, corr, fixedClock(), PendKeys{RequesterHolder: requester.ID, RequestIDs: []string{"urn:shn:claim|CLM-A"}}); err != nil {
		t.Fatalf("A's pend: %v", err)
	}
	bundle := conformantPASBundleWithQR(t, "MBR-UC04")
	env, err := shnsdk.Seal(shnsdk.Metadata{
		Sender: requester.ID, Recipient: "payer", TransactionType: "pas-claim", AuthorityFrame: "payer-coverage",
		Timestamp: g.cfg.Clock().Format(time.RFC3339), CorrelationID: corr,
	}, bundle, g.cfg.Identity.EncPub)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	rec := httptest.NewRecorder()
	g.handlePASNativeInbound(rec, newSignedInboundRequest(t, g, requester.ID), env, shnsdk.Token{Operation: "pas-submit", Subject: pciA, CorrelationID: corr}, bundle, "pa.pas@2.0")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d %s, want the framed refusal", rec.Code, rec.Body)
	}
	hdr, body, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantCorrelationTaken(t, hdr.Status, body)
	if *calls != 0 {
		t.Fatalf("the payer was asked %d times", *calls)
	}
}

// TestPASUpdate_AnotherPatientsTokenHandsThePayerTheUpdatesOwnPatient: the
// amendment's claim and ledger lookups run under the payer's binding of the
// member the amendment names, not the token's.
func TestPASUpdate_AnotherPatientsTokenHandsThePayerTheUpdatesOwnPatient(t *testing.T) {
	g, requester := newInboundTestGateway(t, true)
	answer := pasBundleWithResponse(t, []byte(assemblyRealPending), []byte(assemblyRealTerminal))
	var subjects []string
	g.cfg.Responder = subjectCapture{got: &subjects, inner: pasResultResponder{result: LegResult{Response: testResponse(answer), ResponseSubjectForeign: true}}}
	pciA, _, _ := g.cfg.SoR.ResolvePatient("MBR-COVERED")
	pciB, _, _ := g.cfg.SoR.ResolvePatient("MBR-UC04")

	env := shnsdk.Envelope{}
	env.Metadata.CorrelationID, env.Metadata.Sender = "corr-update-token-for-a", requester.ID
	req := rebindPASPatient(t, originatorBuiltConformantUpdateBundle(t), "MBR-UC04")
	rec := httptest.NewRecorder()
	g.handlePASUpdateNativeInbound(rec, newSignedInboundRequest(t, g, requester.ID), env, shnsdk.Token{Subject: pciA, CorrelationID: env.Metadata.CorrelationID}, req, "pa.pas@2.0")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d %s, want the payer's answer relayed", rec.Code, rec.Body)
	}
	if len(subjects) != 1 || subjects[0] != pciB {
		t.Fatalf("the payer was handed %q, want B's own binding %q", subjects, pciB)
	}
}

// TestPASInquire_AnotherPatientsTokenDecidesNothingOfTheirs: A's authorization
// is pended; an inquiry about B arrives under A's token and the payer's answer
// states a decision under A's authorization's key. The answer is relayed, and
// nothing is recorded: A's authorization stays pended and A gains no EOB naming
// B. The control, A's own inquiry under A's token, records the decision, so the
// row can fail.
func TestPASInquire_AnotherPatientsTokenDecidesNothingOfTheirs(t *testing.T) {
	const corr = "corr-a-pended"
	for _, tc := range []struct {
		name, member string
		decided      bool
	}{
		{"control: A's own inquiry", "MBR-COVERED", true},
		{"B's inquiry under A's token", "MBR-UC04", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, requester := newInboundTestGateway(t, true)
			var events []ObserverEvent
			g.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
			store := g.cfg.Store.(*censusSoR)
			answer := inquiryFixture(t, "pas-inquiry-response-2.0.json")
			g.cfg.Responder = pasResultResponder{result: LegResult{Response: testResponse(answer), ResponseSubjectForeign: true}}
			pciA, _, _ := g.cfg.SoR.ResolvePatient("MBR-COVERED")
			if _, err := store.RecordPendedKeyed(pciA, corr, fixedClock(), PendKeys{RequesterHolder: requester.ID, ClaimResponseIDs: []string{inquiryCRKey}}); err != nil {
				t.Fatalf("A's pend: %v", err)
			}

			env := shnsdk.Envelope{}
			env.Metadata.CorrelationID, env.Metadata.Sender = "corr-inquiry-leg", requester.ID
			rec := httptest.NewRecorder()
			g.handlePASInquireInbound(rec, newSignedInboundRequest(t, g, requester.ID), env,
				shnsdk.Token{Subject: pciA, CorrelationID: env.Metadata.CorrelationID}, inquiryBundle(tc.member, "", "TRN-1", "72148"), "pa.pas@2.0")
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d %s, want the payer's answer relayed", rec.Code, rec.Body)
			}
			_, body, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, rec.Body.Bytes()))
			if err != nil || !bytes.Equal(body, answer) {
				t.Fatalf("the payer's answer was not relayed as sent (err=%v)", err)
			}
			pend, _, _ := store.PendRecordOf(pciA, corr)
			eobs, _ := store.EOBsForPatient(pciA)
			if tc.decided {
				if pend.State != PendStateDecided || len(eobs) != 1 {
					t.Fatalf("control: A's authorization %+v with %d EOBs, want decided with its EOB", pend, len(eobs))
				}
				if n := countEvents(events, PendOtherSubjectEvent) + countEvents(events, SubjectBindingDiffersEvent); n != 0 {
					t.Fatalf("control raised %d subject events", n)
				}
				return
			}
			if pend.State != PendStatePended || len(eobs) != 0 {
				t.Fatalf("B's inquiry changed A's records: authorization %+v, %d EOBs", pend, len(eobs))
			}
			// The operator is told both that the leg is attributed to another
			// patient and that the answer named an authorization it was not about.
			if countEvents(events, SubjectBindingDiffersEvent) != 1 || countEvents(events, PendOtherSubjectEvent) != 1 {
				t.Fatalf("events = %+v, want one %s and one %s", events, SubjectBindingDiffersEvent, PendOtherSubjectEvent)
			}
		})
	}
}

func countEvents(events []ObserverEvent, kind string) int {
	n := 0
	for _, e := range events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// TestPASSubmit_OwnTokenRaisesNoBindingEvent: the event is about a difference;
// a claim under its own patient's token raises none.
func TestPASSubmit_OwnTokenRaisesNoBindingEvent(t *testing.T) {
	g, requester, _, _ := eobOwnerGateway(t)
	var events []ObserverEvent
	g.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
	if status, body := submitAs(t, g, requester, "MBR-UC04", "corr-own-token"); status != http.StatusOK {
		t.Fatalf("status %d %s", status, body)
	}
	if n := countEvents(events, SubjectBindingDiffersEvent); n != 0 {
		t.Fatalf("own-token submit raised %d %s events", n, SubjectBindingDiffersEvent)
	}
}

// TestCRDSelect_AnotherPatientsTokenHandsThePayerTheRequestsOwnPatient: the
// order-select leg asks the payer's system under its binding of the request's
// member, not the token's patient.
func TestCRDSelect_AnotherPatientsTokenHandsThePayerTheRequestsOwnPatient(t *testing.T) {
	g, requester := newInboundTestGateway(t, true)
	var subjects []string
	g.cfg.Responder = subjectCapture{got: &subjects, inner: pasResultResponder{result: LegResult{Response: testResponse([]byte(`{"cards":[]}`))}}}
	pciA, _, _ := g.cfg.SoR.ResolvePatient("MBR-COVERED")
	pciB, _, _ := g.cfg.SoR.ResolvePatient("MBR-UC04")
	env := shnsdk.Envelope{}
	env.Metadata.CorrelationID, env.Metadata.Sender = "corr-crd-token-for-a", requester.ID
	rec := httptest.NewRecorder()
	g.handleCRDNativeInbound(rec, newSignedInboundRequest(t, g, requester.ID), env,
		shnsdk.Token{Subject: pciA, CorrelationID: env.Metadata.CorrelationID}, conformantCRD("MBR-UC04", "72148"), "pa.crd@2.0")
	if len(subjects) != 1 || subjects[0] != pciB {
		t.Fatalf("the payer was handed %q, want B's own binding %q (status %d %s)", subjects, pciB, rec.Code, rec.Body)
	}
}

// TestDTRNextQuestion_AnotherPatientsTokenHandsThePayerTheRoundsOwnPatient: an
// adaptive round about B under A's token reaches the payer's system under B's
// binding, and the payer's answer is relayed.
func TestDTRNextQuestion_AnotherPatientsTokenHandsThePayerTheRoundsOwnPatient(t *testing.T) {
	d := newDTRPayer(t)
	d.partner.respByPath[nextPath] = nextQuestionAnswer(t, "Patient/"+dtrOtherMember, rawItems(t, adaptiveTree(t, "1")))
	var subjects []string
	d.g.cfg.Responder = subjectCapture{got: &subjects, inner: d.g.cfg.Responder}
	pciB, _, _ := d.g.cfg.SoR.ResolvePatient(dtrOtherMember)
	got := d.sendFor(t, shnsdk.FrameOperationNextQuestion, []byte(nextQuestionQR(dtrOtherMember)), d.coveredPC)
	if got.status != http.StatusOK || d.partner.lastPath != nextPath {
		t.Fatalf("answer = %d %s (reached %q), want the payer's answer relayed", got.status, got.body, d.partner.lastPath)
	}
	if len(subjects) != 1 || subjects[0] != pciB {
		t.Fatalf("the payer was handed %q, want B's own binding %q", subjects, pciB)
	}
}

// TestPASInquire_SystemOfRecordFailureIsTheGatewaysOwn5xx: an inquiry whose
// member the payer's system of record cannot be read for is this gateway's own
// failure, answered bare with the system-of-record status — not sealed as the
// payer's answer about the request.
func TestPASInquire_SystemOfRecordFailureIsTheGatewaysOwn5xx(t *testing.T) {
	g, requester := newInboundTestGateway(t, true)
	g.cfg.SoR = routeFailureSoR{t: t, fail: "patient"}
	g.cfg.Responder = pasResultResponder{err: errors.New("the payer must not be asked")}
	env := shnsdk.Envelope{}
	env.Metadata.CorrelationID, env.Metadata.Sender = "corr-inquiry-sor-down", requester.ID
	rec := httptest.NewRecorder()
	g.handlePASInquireInbound(rec, newSignedInboundRequest(t, g, requester.ID), env,
		shnsdk.Token{Subject: "pci:any", CorrelationID: env.Metadata.CorrelationID}, inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"), "pa.pas@2.0")
	want, _ := SoRFailureResponse(errors.New("private-upstream-sentinel"))
	if rec.Code != want || rec.Code < http.StatusInternalServerError {
		t.Fatalf("status %d %s, want the bare %d", rec.Code, rec.Body, want)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("private-upstream-sentinel")) {
		t.Fatalf("the backend's own error text leaked: %s", rec.Body)
	}
}
