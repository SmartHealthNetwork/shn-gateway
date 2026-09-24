package engine

// eobowner_test.go — one decision EOB id per patient (eobowner.go): the submit
// leg refuses a correlation id that already names another patient's
// authorization BEFORE the payer is asked, and a decision whose EOB id another
// patient's exchange took in the meantime relays unrecorded, on the submit and
// the inquiry legs alike.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// countingPASResponder counts the payer's pas-claim answers — a call here is the
// payer acting on the claim — and can run a write while the payer answers
// (another exchange landing mid-flight) and attach the leg's own ledger Commit.
type countingPASResponder struct {
	inner  LegResponder
	calls  *int
	during func(corrID string)
	commit func(ctx context.Context, corrID, subjectPCI string) func() error
}

func (r countingPASResponder) Handle(ctx context.Context, leg, corrID, subjectPCI string, requestFHIR []byte) (LegResult, error) {
	*r.calls++
	if r.during != nil {
		r.during(corrID)
	}
	result, err := r.inner.Handle(ctx, leg, corrID, subjectPCI, requestFHIR)
	if err == nil && r.commit != nil {
		result.Commit = r.commit(ctx, corrID, subjectPCI)
	}
	return result, err
}

// eobOwnerGateway is a payer gateway over the census fixture, its store, and a
// counter of the payer's answers.
func eobOwnerGateway(t *testing.T) (*Gateway, inboundTestRequester, *censusSoR, *int) {
	t.Helper()
	g, requester := newInboundTestGateway(t, true)
	store, ok := g.cfg.Store.(*censusSoR)
	if !ok {
		t.Fatalf("fixture store is %T", g.cfg.Store)
	}
	calls := new(int)
	g.cfg.Responder = countingPASResponder{inner: approvingPASResponder{clock: g.cfg.Clock}, calls: calls}
	return g, requester, store, calls
}

func memberPCI(t *testing.T, g *Gateway, member string) string {
	t.Helper()
	pci, _, ok := g.cfg.SoR.ResolvePatient(member)
	if !ok {
		t.Fatalf("%s not resolvable in the census fixture", member)
	}
	return pci
}

// submitAs drives the conformant submit leg for member under corrID and returns
// the status and body the requester receives: the framed application answer, or
// the raw non-2xx of a gateway fault.
func submitAs(t *testing.T, g *Gateway, requester inboundTestRequester, member, corrID string) (int, []byte) {
	t.Helper()
	bundle := conformantPASBundleWithQR(t, member)
	env, err := shnsdk.Seal(shnsdk.Metadata{
		Sender: requester.ID, Recipient: "payer", TransactionType: "pas-claim", AuthorityFrame: "payer-coverage",
		Timestamp: g.cfg.Clock().Format(time.RFC3339), CorrelationID: corrID,
	}, bundle, g.cfg.Identity.EncPub)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	tok := shnsdk.Token{Operation: "pas-submit", Subject: memberPCI(t, g, member), CorrelationID: corrID}
	rec := httptest.NewRecorder()
	g.handlePASNativeInbound(rec, newSignedInboundRequest(t, g, requester.ID), env, tok, bundle, "pa.pas@2.0")
	if rec.Code != http.StatusOK {
		return rec.Code, rec.Body.Bytes()
	}
	hdr, body, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("decode the framed answer: %v", err)
	}
	return hdr.Status, body
}

func wantCorrelationTaken(t *testing.T, status int, body []byte) {
	t.Helper()
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want the framed 409; body=%s", status, body)
	}
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil || e.Error != refusalCorrelationTaken {
		t.Fatalf("body = %s, want error %q", body, refusalCorrelationTaken)
	}
}

// TestPASSubmit_CorrelationTakenRefusedBeforeThePayer: a submit whose correlation
// id already names another patient's authorization — a decision EOB filed for
// that patient, or that patient's authorization still awaiting its decision — is
// the payer gateway's framed 409, and the payer is never asked.
func TestPASSubmit_CorrelationTakenRefusedBeforeThePayer(t *testing.T) {
	const corr = "corr-shared"
	otherEOB := []byte(`{"resourceType":"ExplanationOfBenefit","id":"other"}`)
	for _, tc := range []struct {
		name string
		seed func(t *testing.T, s *censusSoR, other string)
	}{
		{"another patient's decision EOB is filed under the id", func(t *testing.T, s *censusSoR, other string) {
			if err := s.RecordEOB(other, decisionEOBID(corr), otherEOB); err != nil {
				t.Fatal(err)
			}
		}},
		{"another patient's authorization is pended under the correlation", func(t *testing.T, s *censusSoR, other string) {
			if _, err := s.RecordPendedKeyed(other, corr, fixedClock(), PendKeys{RequesterHolder: "requester", RequestIDs: []string{"urn:shn:claim|CLM-OTHER"}}); err != nil {
				t.Fatal(err)
			}
		}},
		{"another patient's amendment is in progress under the correlation", func(t *testing.T, s *censusSoR, other string) {
			if err := s.RecordPendedClaim(other, corr); err != nil {
				t.Fatal(err)
			}
			if claimed, err := s.BeginClaimUpdate(other, corr); err != nil || !claimed {
				t.Fatalf("begin = %v,%v", claimed, err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, requester, store, calls := eobOwnerGateway(t)
			other := memberPCI(t, g, "MBR-UC04")
			tc.seed(t, store, other)
			status, body := submitAs(t, g, requester, "MBR-COVERED", corr)
			wantCorrelationTaken(t, status, body)
			if *calls != 0 {
				t.Fatalf("the payer was asked %d times; a refused correlation must never reach it", *calls)
			}
			self := memberPCI(t, g, "MBR-COVERED")
			if _, found, _ := store.PendRecordOf(self, corr); found {
				t.Fatal("the refused submit wrote a ledger row")
			}
			if got, found := store.EOBsForPatient(self); found {
				t.Fatalf("the refused submit filed %d EOBs", len(got))
			}
		})
	}

	// Controls: the same correlation id is not taken by the patient's OWN EOB
	// or pend, nor by another patient's DECIDED authorization with no EOB — each
	// reaches the payer and is answered.
	for _, tc := range []struct {
		name string
		seed func(t *testing.T, s *censusSoR, self, other string)
	}{
		{"one's own EOB under the id", func(t *testing.T, s *censusSoR, self, _ string) {
			if err := s.RecordEOB(self, decisionEOBID(corr), otherEOB); err != nil {
				t.Fatal(err)
			}
		}},
		{"one's own pend under the correlation", func(t *testing.T, s *censusSoR, self, _ string) {
			if err := s.RecordPendedClaim(self, corr); err != nil {
				t.Fatal(err)
			}
		}},
		{"another patient's decided authorization with no EOB", func(t *testing.T, s *censusSoR, _, other string) {
			if _, err := s.RecordDecision(other, corr, PendOutcomeApproved, fixedClock(), nil); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run("control: "+tc.name, func(t *testing.T) {
			g, requester, store, calls := eobOwnerGateway(t)
			tc.seed(t, store, memberPCI(t, g, "MBR-COVERED"), memberPCI(t, g, "MBR-UC04"))
			if status, body := submitAs(t, g, requester, "MBR-COVERED", corr); status != http.StatusOK || *calls != 1 {
				t.Fatalf("status=%d calls=%d, want 200 from the payer; body=%s", status, *calls, body)
			}
		})
	}
}

// eobOwnerUnreadable and pendedUnreadable are stores whose pre-forward lookup
// fails: an outage, not an answer.
type eobOwnerUnreadable struct{ *censusSoR }

func (eobOwnerUnreadable) EOBOwner(string) (string, bool, error) {
	return "", false, errors.New("eob index unavailable")
}

type pendedUnreadable struct{ *censusSoR }

func (pendedUnreadable) PendedForOtherSubject(string, string) (string, bool, error) {
	return "", false, errors.New("pend index unavailable")
}

// TestPASSubmit_CorrelationLookupOutage: a store that cannot say whether the
// correlation id is taken fails the submit as every other store failure on the
// leg does — the gateway's own raw 502, counted — and the payer is not asked.
func TestPASSubmit_CorrelationLookupOutage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store func(*censusSoR) Store
	}{
		{"EOB owner", func(s *censusSoR) Store { return eobOwnerUnreadable{s} }},
		{"pended authorization", func(s *censusSoR) Store { return pendedUnreadable{s} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, requester, store, calls := eobOwnerGateway(t)
			g.cfg.Store = tc.store(store)
			var counted []string
			g.cfg.StoreErrorMetric = func(s string) { counted = append(counted, s) }
			status, body := submitAs(t, g, requester, "MBR-COVERED", "corr-outage")
			if status != http.StatusBadGateway || !bytes.Contains(body, []byte(refusalHolderReadFailed)) {
				t.Fatalf("status=%d body=%s, want the raw 502 %q", status, body, refusalHolderReadFailed)
			}
			if *calls != 0 {
				t.Fatalf("the payer was asked %d times during a store outage", *calls)
			}
			if len(counted) != 1 || counted[0] != storeErrPended {
				t.Fatalf("store errors counted = %v, want one %q", counted, storeErrPended)
			}
		})
	}
}

// capabilityFreeStore hides every optional capability of the store it wraps.
type capabilityFreeStore struct{ Store }

// TestPASSubmit_NoLookupCapabilityNoPreCheck: a Store without the optional
// lookups is not pre-checked — the payer is asked, and the write-time refusal is
// that store's guard.
func TestPASSubmit_NoLookupCapabilityNoPreCheck(t *testing.T) {
	g, requester, store, calls := eobOwnerGateway(t)
	if err := store.RecordEOB(memberPCI(t, g, "MBR-UC04"), decisionEOBID("corr-shared"), []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	g.cfg.Store = capabilityFreeStore{store}
	if status, body := submitAs(t, g, requester, "MBR-COVERED", "corr-shared"); status != http.StatusOK || *calls != 1 {
		t.Fatalf("status=%d calls=%d body=%s, want the payer asked and answered", status, *calls, body)
	}
}

// TestPASSubmit_EOBTakenMidFlightRelaysUnrecorded is the race the pre-forward
// check cannot close: another patient's decision EOB lands under this
// exchange's id while the payer answers. The payer has acted, so its answer
// relays; the decision is not recorded, the other patient's EOB is untouched, and
// the operator is told — metadata only.
func TestPASSubmit_EOBTakenMidFlightRelaysUnrecorded(t *testing.T) {
	const corr = "corr-race"
	g, requester, store, calls := eobOwnerGateway(t)
	other := memberPCI(t, g, "MBR-UC04")
	self := memberPCI(t, g, "MBR-COVERED")
	otherEOB := []byte(`{"resourceType":"ExplanationOfBenefit","id":"other"}`)
	var events []ObserverEvent
	g.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
	g.cfg.Responder = countingPASResponder{
		inner: approvingPASResponder{clock: g.cfg.Clock},
		calls: calls,
		during: func(corrID string) {
			if err := store.RecordEOB(other, decisionEOBID(corrID), otherEOB); err != nil {
				t.Errorf("seed the mid-flight EOB: %v", err)
			}
		},
		commit: func(ctx context.Context, corrID, subjectPCI string) func() error {
			return recordPASDecision(ctx, store, subjectPCI, corrID, PendOutcomeApproved, fixedClock(),
				&EOBRecord{SubjectPCI: subjectPCI, EOBID: decisionEOBID(corrID), JSON: []byte(`{"resourceType":"ExplanationOfBenefit","id":"self"}`)})
		},
	}
	status, body := submitAs(t, g, requester, "MBR-COVERED", corr)
	if status != http.StatusOK || *calls != 1 {
		t.Fatalf("status=%d calls=%d body=%s, want the payer's answer relayed", status, *calls, body)
	}
	if parsed, err := shnsdk.ParseClaimResponse(body); err != nil || parsed.Outcome != "approved" {
		t.Fatalf("relayed body is not the payer's approval: %+v err=%v", parsed, err)
	}
	if _, found, _ := store.PendRecordOf(self, corr); found {
		t.Fatal("the decision was recorded although its EOB id belongs to another patient")
	}
	if got, found := store.EOBByID(decisionEOBID(corr)); !found || !bytes.Equal(got, otherEOB) {
		t.Fatalf("the other patient's EOB changed: %s", got)
	}
	if got, found := store.EOBsForPatient(self); found {
		t.Fatalf("the patient gained %d EOBs from an unrecorded decision", len(got))
	}
	wantNotRecordedEvent(t, events, "pas-claim", "pas-submit", corr, corr, self, other)
}

func wantNotRecordedEvent(t *testing.T, events []ObserverEvent, leg, op, legCorr, authCorr string, pcis ...string) {
	t.Helper()
	var found []ObserverEvent
	for _, e := range events {
		if e.Kind == pendDecisionNotRecordedEvent {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("events = %+v, want one %s", events, pendDecisionNotRecordedEvent)
	}
	e := found[0]
	if e.LegType != leg || e.Op != op || e.CorrelationID != legCorr {
		t.Fatalf("event = %+v, want leg %s, op %s, correlation %s", e, leg, op, legCorr)
	}
	if want := "authorization " + authCorr + ": " + reasonEOBOwnedElsewhere; e.Detail != want {
		t.Fatalf("event detail = %q, want %q", e.Detail, want)
	}
	for _, pci := range pcis {
		if strings.Contains(e.Detail, pci) || len(e.Payload) != 0 {
			t.Fatalf("the event carries more than metadata: %+v", e)
		}
	}
}

// TestPASInquire_EOBTakenRelaysUnrecorded: the inquiry leg learns a decision
// whose EOB id another patient's exchange already took. Refusing the RECORD, not
// the relay: the write succeeds with nothing recorded (so the handler relays the
// payer's answer), the authorization stays pended, the other patient's EOB is
// untouched, and the operator is told.
func TestPASInquire_EOBTakenRelaysUnrecorded(t *testing.T) {
	f := newInquiryLedgerFixture(t, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{inquiryCRKey}})
	const other = "pci:MBR-OTHER"
	otherEOB := []byte(`{"resourceType":"ExplanationOfBenefit","id":"other"}`)
	if err := f.store.RecordEOB(other, decisionEOBID(f.corr), otherEOB); err != nil {
		t.Fatal(err)
	}
	var events []ObserverEvent
	f.g.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
	f.apply(t, inquiryRequester, decidedAnswer(t)) // fails the test if the write errs
	if got := f.state(t); got.State != PendStatePended || got.Outcome != "" {
		t.Fatalf("the decision was recorded although its EOB id belongs to another patient: %+v", got)
	}
	if got, found := f.store.EOBByID(decisionEOBID(f.corr)); !found || !bytes.Equal(got, otherEOB) {
		t.Fatalf("the other patient's EOB changed: %s", got)
	}
	if got, found := f.store.EOBsForPatient(f.subject); found {
		t.Fatalf("the patient gained %d EOBs from an unrecorded decision", len(got))
	}
	wantNotRecordedEvent(t, events, "pas-claim-inquire", "pas-inquire", "corr-inquiry-leg", f.corr, f.subject, other)
}

// TestPASCorrelationCollision_InquiryNotStuck is the whole scenario the
// pre-forward check exists for: patient B's authorization is pended under a
// correlation id, patient A then submits under the SAME id, and B later
// inquires. A's submit is refused before the payer is asked, so A's decision
// never takes the EOB id B's decision will be filed under — and B's inquiry
// records B's decision and EOB.
func TestPASCorrelationCollision_InquiryNotStuck(t *testing.T) {
	const corr = "corr-shared"
	g, requester, store, calls := eobOwnerGateway(t)
	pciA, pciB := memberPCI(t, g, "MBR-COVERED"), memberPCI(t, g, "MBR-UC04")
	if _, err := store.RecordPendedKeyed(pciB, corr, fixedClock(), PendKeys{RequesterHolder: requester.ID, ClaimResponseIDs: []string{inquiryCRKey}}); err != nil {
		t.Fatalf("B's pend: %v", err)
	}

	status, body := submitAs(t, g, requester, "MBR-COVERED", corr)
	wantCorrelationTaken(t, status, body)
	if *calls != 0 {
		t.Fatalf("the payer was asked %d times about A's colliding submit", *calls)
	}

	facts, fstatus, msg := parsePASInquiryFacts(inquiryBundle("MBR-UC04", "", "TRN-1", "72148"))
	if fstatus != 0 {
		t.Fatalf("B's inquiry refused: %d %s", fstatus, msg)
	}
	result := LegResult{}
	commit, _ := g.inquiryLedgerEffect(requester.ID, pciB, "Patient/MBR-UC04", "corr-inquiry-leg", facts, decidedAnswer(t), &result)
	if commit == nil {
		t.Fatal("B's inquiry produced no ledger write")
	}
	if err := commit(); err != nil {
		t.Fatalf("B's decision: %v", err)
	}
	rec, found, err := store.PendRecordOf(pciB, corr)
	if err != nil || !found || rec.State != PendStateDecided || rec.Outcome != PendOutcomeApproved {
		t.Fatalf("B's authorization = %+v (found=%v err=%v), want decided/approved", rec, found, err)
	}
	if owner, found, _ := store.EOBOwner(decisionEOBID(corr)); !found || owner != pciB {
		t.Fatalf("EOB %s is filed for %q, want B", decisionEOBID(corr), owner)
	}
	if got, found := store.EOBsForPatient(pciA); found {
		t.Fatalf("A gained %d EOBs", len(got))
	}
}
