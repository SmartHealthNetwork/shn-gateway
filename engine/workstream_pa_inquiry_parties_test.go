//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A dependent's inquiry carries the parent the Coverage names, as the
// submission does: module 1 reads the parent's record by the submission's own
// rule (coveragePartyRecords) from the Coverage it reads again for every
// inquiry, and the SDK's inquiry builder carries it as a Patient entry of its
// own, the record's own bytes, declared as a copied span ("CoverageParties[i]").

// pendedFollowSystem is a provider gateway whose payer pended a submission for
// a member who is their own subscriber, with the pend's continuation; the
// system of record then holds the member's Coverage as sor makes it.
func pendedFollowSystem(t *testing.T, sor func(SystemOfRecord) SystemOfRecord) (*Gateway, *pasFollowStub, PASDecision) {
	t.Helper()
	gw, stub := pasFollowSystem(t, "pended")
	req := httptest.NewRequest(http.MethodPost, "/scenario/uc03", strings.NewReader(testRouteBody(gw, "/scenario/uc03", "")))
	pend, status, msg, err := gw.submitClaimAndFollow(req.Context(), req, pasFollowSubmit(0))
	if status != 0 || err != nil {
		t.Fatalf("submit: %d %q %v", status, msg, err)
	}
	if pend.Decision != PASDecisionPended || pend.Continuation == "" {
		t.Fatalf("this row starts from a pend with a continuation: %+v", pend)
	}
	gw.cfg.SoR = sor(gw.cfg.SoR)
	return gw, stub, pend
}

// inquireOnce asks the inquire route once about the continuation, through the
// exchange wrapper the mounted route runs under, and returns the answer and
// the exchange it recorded.
func inquireOnce(gw *Gateway, continuation string) (*httptest.ResponseRecorder, *exchangeRecorder) {
	x := &exchangeRecorder{}
	inq := httptest.NewRequest(http.MethodPost, "/scenario/pa/inquire", strings.NewReader(`{"continuation":"`+continuation+`","waitSeconds":0}`))
	rec := httptest.NewRecorder()
	gw.handlePAInquire(&exchangeWriter{ResponseWriter: rec, x: x}, inq.WithContext(context.WithValue(inq.Context(), exchangeRecorderKey{}, x)))
	return rec, x
}

func dependentSoR(held parentHeld) func(SystemOfRecord) SystemOfRecord {
	return func(inner SystemOfRecord) SystemOfRecord { return newDependentCoverageSoR(inner, held) }
}

// At every level, the inquire route's inquiry for a dependent carries the
// parent as a Patient entry the Coverage names as its party, the participant's
// record byte for byte, and the payer's answer comes back.
func TestPAInquire_DependentInquiryCarriesTheParent(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			gw, stub, pend := pendedFollowSystem(t, dependentSoR(parentFound))
			gw.cfg.ConformanceEnforcement = level
			stub.inquireAnswer = "approved"
			rec, _ := inquireOnce(gw, pend.Continuation)
			if rec.Code != http.StatusOK {
				t.Fatalf("inquire = %d %s, want 200", rec.Code, rec.Body)
			}
			sent := stub.sent["pas-claim-inquire"]
			if len(sent) != 1 {
				t.Fatalf("%d inquiries sent, want 1", len(sent))
			}
			parentRidesAsItsOwnEntry(t, "pas-claim-inquire", sent[0])
			if !bytes.Contains(sent[0], dependentParentRecord()) {
				t.Fatalf("the inquiry does not carry the parent's record as the participant's own bytes: %s", sent[0])
			}
			if n := patientEntries(t, sent[0]); n != 2 {
				t.Fatalf("the inquiry carries %d Patient entries, want the member's and the parent's", n)
			}
		})
	}
}

// Unchanged: a member who is their own subscriber reads no parent, and the
// inquiry carries the member's Patient alone.
func TestPAInquire_SelfSubscriberInquiryCarriesNoParent(t *testing.T) {
	var reads []string
	gw, stub, pend := pendedFollowSystem(t, func(inner SystemOfRecord) SystemOfRecord {
		s := newDependentCoverageSoR(inner, parentFound)
		s.subscriber = "Patient/" + pasFollowMember
		return readsLogged{dependentCoverageSoR: s, reads: &reads}
	})
	stub.inquireAnswer = "approved"
	rec, _ := inquireOnce(gw, pend.Continuation)
	if rec.Code != http.StatusOK {
		t.Fatalf("inquire = %d %s, want 200", rec.Code, rec.Body)
	}
	// The inquiry reads the member's own Patient, and no one as a party:
	// the Coverage's subscriber is the member.
	var own int
	for _, ref := range reads {
		switch ref {
		case "Patient/" + pasFollowMember:
			own++
		case "Patient/" + dependentParentID:
			t.Fatalf("read %s for a member who is their own subscriber", ref)
		}
	}
	if own != 1 {
		t.Fatalf("read the member's Patient %d times (%v), want once: the subscriber naming the member is no party", own, reads)
	}
	sent := stub.sent["pas-claim-inquire"]
	if len(sent) != 1 || patientEntries(t, sent[0]) != 1 {
		t.Fatalf("want one inquiry carrying the member's Patient alone; sent %d", len(sent))
	}
}

// readsLogged is a dependentCoverageSoR whose reads by reference are the
// inner system's (the member's own Patient among them), each logged.
type readsLogged struct {
	dependentCoverageSoR
	reads *[]string
}

func (s readsLogged) ResolveByReferenceContext(ctx context.Context, ref string) ([]byte, bool, error) {
	*s.reads = append(*s.reads, ref)
	return s.refReadFails.ResolveByReferenceContext(ctx, ref)
}

func (s readsLogged) ResolveByReference(ref string) ([]byte, bool) {
	raw, found, _ := s.ResolveByReferenceContext(context.Background(), ref)
	return raw, found
}

// REJECTION, at every level: on the inquire route, a parent the system does
// not hold is refused, 422, the fidelity of SHN's own build at the build
// stage, in the SDK's words; a read of the parent the system fails to answer
// is that backend failure. No inquiry is sent either way.
func TestPAInquire_DependentParentUnavailable(t *testing.T) {
	for _, row := range []struct {
		name   string
		held   parentHeld
		status int
		check  func(*testing.T, *exchangeRecorder, string)
	}{
		{"not held", parentNotHeld, http.StatusUnprocessableEntity, func(t *testing.T, x *exchangeRecorder, body string) {
			if x.outcome != ExchangeRefused || x.rule != RefusalFidelity || x.refusalStage != vocab.RefusalStageBuild {
				t.Fatalf("recorded %q/%q at %q, want refused fidelity at %s", x.outcome, x.rule, x.refusalStage, vocab.RefusalStageBuild)
			}
			if !strings.Contains(body, "no Patient record of that person was supplied") {
				t.Fatalf("body %s does not give the SDK's reason", body)
			}
		}},
		{"read fails", parentReadFails, http.StatusServiceUnavailable, upstreamRecorded},
		{"another record answered", parentAnotherRecord, http.StatusBadGateway, upstreamRecorded},
	} {
		for _, level := range allLevels {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				gw, stub, pend := pendedFollowSystem(t, dependentSoR(row.held))
				gw.cfg.ConformanceEnforcement = level
				rec, x := inquireOnce(gw, pend.Continuation)
				if rec.Code != row.status {
					t.Fatalf("inquire = %d %s, want %d", rec.Code, rec.Body, row.status)
				}
				if stub.sawInquiry {
					t.Fatal("an inquiry was sent without the parent the Coverage names")
				}
				row.check(t, x, rec.Body.String())
			})
		}
	}
}

// upstreamRecorded fails unless the exchange was recorded as the system of
// record's failure to answer, never as a refusal.
func upstreamRecorded(t *testing.T, x *exchangeRecorder, _ string) {
	t.Helper()
	if x.outcome != ExchangeUpstreamError {
		t.Fatalf("recorded %q/%q, want an upstream error: the system failed to answer, never a refusal", x.outcome, x.rule)
	}
}

// A route's own wait keeps the payer's pend when the inquiry cannot carry the
// parent: the inquiry is not sent, the failure is observed, and nothing is
// recorded as a refusal of the request the pend answered.
func TestFollow_DependentParentUnavailableKeepsThePend(t *testing.T) {
	fastPASInquire(t)
	for held, why := range map[parentHeld]string{
		parentNotHeld:       "no Patient record of that person was supplied",
		parentReadFails:     "read the coverage's parties from the system of record",
		parentAnotherRecord: "read the coverage's parties from the system of record",
	} {
		gw, stub, pend := pendedFollowSystem(t, dependentSoR(held))
		var events []ObserverEvent
		gw.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
		store, _ := gw.continuations()
		cont, _, err := store.ReadContinuation("provider", pend.Continuation)
		if err != nil {
			t.Fatal(err)
		}
		x := &exchangeRecorder{}
		req := httptest.NewRequest(http.MethodPost, "/scenario/uc03", nil)
		ctx := context.WithValue(req.Context(), exchangeRecorderKey{}, x)
		out, status, msg, err := gw.followPended(ctx, pend, PASWaitMax, gw.inquiryFor(req.WithContext(ctx), cont))
		if status != 0 || err != nil || out.Decision != PASDecisionPended || out.Continuation != pend.Continuation {
			t.Fatalf("held %d: %+v %d %q %v, want the pend kept", held, out, status, msg, err)
		}
		if stub.sawInquiry {
			t.Fatalf("held %d: an inquiry was sent without the parent", held)
		}
		if x.outcome == ExchangeRefused {
			t.Fatalf("held %d: recorded refused %s on an answer that keeps the pend", held, x.rule)
		}
		var failed bool
		for _, e := range events {
			failed = failed || e.Kind == "pa.inquiry-failed" && strings.Contains(e.Detail, why)
		}
		if !failed {
			t.Fatalf("held %d: no pa.inquiry-failed event saying %q (%v)", held, why, events)
		}
	}
}

// inquiryRecordBody names a party's record by an index that names one, and
// nothing else: the set of inputs an inquiry's embeds may name stays closed.
// The records differ in length, so the length a name answers tells which.
func TestInquiryRecordBody_NamesEachPartyByIndex(t *testing.T) {
	records := shnsdk.PASInquiryRecords{Patient: []byte(`{"p":1}`), CoverageParties: [][]byte{[]byte(`{"a":1}`), []byte(`{"bb":22}`)}}
	for input, want := range map[string]string{
		"CoverageParties[0]":                    `{"a":1}`,
		"CoverageParties[1]":                    `{"bb":22}`,
		"CoverageParties[2]":                    "",
		"CoverageParties[01]":                   "",
		"CoverageParties[-1]":                   "",
		"CoverageParties[]":                     "",
		"CoverageParties[0":                     "",
		"CoverageParties[+1]":                   "",
		"CoverageParties[ 0]":                   "",
		"CoverageParties[0 ]":                   "",
		"CoverageParties[٠]":                    "",
		"CoverageParties[99999999999999999999]": "",
		"CoverageParties[0]]":                   "",
		"CoverageParties":                       "",
		"coverageParties[0]":                    "",
		"Party":                                 "",
	} {
		body, ok := inquiryRecordBody(records, input)
		switch {
		case want == "" && ok:
			t.Errorf("%q named a record", input)
		case want != "" && (!ok || body.Len() != len(want)):
			t.Errorf("%q = %v, %d bytes, want %s", input, ok, body.Len(), want)
		}
	}
}

// A dependent whose system names them by its own record id: the inquiry's
// Coverage, re-keyed, names the member as beneficiary and the parent as
// subscriber, and the inquiry carries the parent as an entry of its own beside
// the member, with no participant-local id anywhere.
func TestPAInquire_DependentUnderALocalIDCarriesTheParent(t *testing.T) {
	gw, stub := pasFollowSystem(t, "pended")
	sor := newLocalIDSoR(gw.cfg.SoR, pasFollowMember)
	sor.subscriber = "Patient/" + dependentParentID
	gw.cfg.SoR = sor
	req := httptest.NewRequest(http.MethodPost, "/scenario/uc03", strings.NewReader(testRouteBody(gw, "/scenario/uc03", "")))
	pend, status, msg, err := gw.submitClaimAndFollow(req.Context(), req, pasFollowSubmit(0))
	if status != 0 || err != nil {
		t.Fatalf("submit: %d %q %v", status, msg, err)
	}
	stub.inquireAnswer = "approved"
	rec, _ := inquireOnce(gw, pend.Continuation)
	if rec.Code != http.StatusOK {
		t.Fatalf("inquire = %d %s, want 200", rec.Code, rec.Body)
	}
	sent := stub.sent["pas-claim-inquire"]
	if len(sent) != 1 {
		t.Fatalf("%d inquiries sent, want 1", len(sent))
	}
	if bytes.Contains(sent[0], []byte(localPatientID)) {
		t.Fatalf("the inquiry carries the system's own record id: %s", sent[0])
	}
	parentRidesAsItsOwnEntry(t, "pas-claim-inquire", sent[0])
	if n := patientEntries(t, sent[0]); n != 2 {
		t.Fatalf("the inquiry carries %d Patient entries, want the member's and the parent's", n)
	}
}

// The parties are read from the Coverage after the member's re-key, as the
// submission reads them: a Coverage whose beneficiary names the member by the
// system's own record id and whose subscriber names the member by the member
// id is the member's own, so no one is read as a party. Read before the
// re-key, the subscriber would look like another person and be read.
func TestPAInquire_PartiesReadAfterTheMemberReKey(t *testing.T) {
	var reads []string
	gw, stub := pasFollowSystem(t, "pended")
	sor := newLocalIDSoR(gw.cfg.SoR, pasFollowMember)
	sor.subscriber = "Patient/" + pasFollowMember
	gw.cfg.SoR = localReadsLogged{localIDSoR: sor, reads: &reads}
	req := httptest.NewRequest(http.MethodPost, "/scenario/uc03", strings.NewReader(testRouteBody(gw, "/scenario/uc03", "")))
	pend, status, msg, err := gw.submitClaimAndFollow(req.Context(), req, pasFollowSubmit(0))
	if status != 0 || err != nil {
		t.Fatalf("submit: %d %q %v", status, msg, err)
	}
	reads = nil
	stub.inquireAnswer = "approved"
	rec, _ := inquireOnce(gw, pend.Continuation)
	if rec.Code != http.StatusOK {
		t.Fatalf("inquire = %d %s, want 200", rec.Code, rec.Body)
	}
	for _, ref := range reads {
		if ref == "Patient/"+pasFollowMember {
			t.Fatalf("read %s as a party: the subscriber names the member once the beneficiary is re-keyed (reads %v)", ref, reads)
		}
	}
	if sent := stub.sent["pas-claim-inquire"]; len(sent) != 1 || patientEntries(t, sent[0]) != 1 {
		t.Fatalf("want one inquiry carrying the member's Patient alone")
	}
}

// localReadsLogged logs every read by reference a localIDSoR answers.
type localReadsLogged struct {
	localIDSoR
	reads *[]string
}

func (s localReadsLogged) ResolveByReferenceContext(ctx context.Context, ref string) ([]byte, bool, error) {
	*s.reads = append(*s.reads, ref)
	return s.localIDSoR.ResolveByReferenceContext(ctx, ref)
}

func (s localReadsLogged) ResolveByReference(ref string) ([]byte, bool) {
	raw, found, _ := s.ResolveByReferenceContext(context.Background(), ref)
	return raw, found
}
