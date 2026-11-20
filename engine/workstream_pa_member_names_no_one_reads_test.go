//go:build !shn_coreonly

package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A read refused because its member names no one (errMemberNamesNoOne) is the
// request's, never the system of record's: no caller turns it into the
// system's invalid response, an origination records it on authority, not as
// the system's failure, and the observer sees no read, since none was made.

func TestSafeReadErr_KeepsAMemberThatNamesNoOne(t *testing.T) {
	if err := safeReadErr(fmt.Errorf("read: %w", errMemberNamesNoOne)); !errors.Is(err, errMemberNamesNoOne) {
		t.Fatalf("safeReadErr: %v; want errMemberNamesNoOne", err)
	}
	var readErr *SoRReadError
	if err := safeReadErr(errors.New("backend said something private")); !errors.As(err, &readErr) || readErr.Kind != SoRInvalidResponse || strings.Contains(err.Error(), "private") {
		t.Fatalf("safeReadErr of another failure: %v; want the system's safe invalid response", err)
	}
}

// Each read a caller returns as its own keeps the member's refusal: the
// populator's and the home-oxygen evidence's clinical-context read.
func TestReadsForAMemberThatNamesNoOneStayTheRequests(t *testing.T) {
	ctx := context.Background()
	pkg := []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Questionnaire","id":"q","status":"active","url":"http://example.org/q"}}]}`)
	for _, member := range []string{"", " ", "Patient/"} {
		if _, _, err := newManagedPopulator(newCensusSoR()).Populate(ctx, pkg, PopulateContext{Member: member}); !errors.Is(err, errMemberNamesNoOne) {
			t.Errorf("Populate(member %q): %v; want errMemberNamesNoOne", member, err)
		}
		g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
		if _, err := g.homeOxygenAutoFillEvidenceContext(ctx, member, []byte(`{"resourceType":"QuestionnaireResponse"}`)); !errors.Is(err, errMemberNamesNoOne) {
			t.Errorf("homeOxygenAutoFillEvidenceContext(member %q): %v; want errMemberNamesNoOne", member, err)
		}
	}
}

// An origination answering a member that names no one refuses it 400 on
// authority, with the reason the binding records; another failed read is the
// system's failure, an upstream error, as before.
func TestFailOriginRead_AMemberThatNamesNoOneIsRefusedOnAuthority(t *testing.T) {
	x := &exchangeRecorder{}
	rec := httptest.NewRecorder()
	if !failOriginReadErr(context.WithValue(context.Background(), exchangeRecorderKey{}, x), rec, fmt.Errorf("read: %w", errMemberNamesNoOne)) {
		t.Fatal("failOriginReadErr did not answer")
	}
	if rec.Code != http.StatusBadRequest || !strings.HasPrefix(answerText(rec.Body.String()), msgMemberNamesNoOne) {
		t.Fatalf("answer %d %s; want 400 %q", rec.Code, rec.Body, msgMemberNamesNoOne)
	}
	if x.outcome == ExchangeUpstreamError || x.guardRule != RefusalAuthority || x.guardReason != reasonMemberNamesNoOne {
		t.Fatalf("recorded outcome %q, rule %q, reason %q; want an authority refusal, never the system's failure", x.outcome, x.guardRule, x.guardReason)
	}
	control := &exchangeRecorder{}
	failOriginReadErr(context.WithValue(context.Background(), exchangeRecorderKey{}, control), httptest.NewRecorder(), &SoRReadError{Kind: SoRUnavailable})
	if control.outcome != ExchangeUpstreamError {
		t.Fatalf("an unavailable system recorded %q; want upstream error", control.outcome)
	}
}

// The observer sees a read only when one was made: a member that names no one
// is refused before any, so no sor.read event names the system's failure.
func TestObservingSoR_NoReadEventForAMemberThatNamesNoOne(t *testing.T) {
	var events []ObserverEvent
	o := observingSoR{inner: newCensusSoR(), clock: time.Now, observer: func(e ObserverEvent) { events = append(events, e) }}
	ctx := context.Background()
	for _, member := range []string{"", " ", "Patient/"} {
		events = nil
		_, _, _, err := o.ResolvePatientContext(ctx, member)
		_, _, err2 := o.CoverageInforceContext(ctx, member)
		_, err3 := o.OpenCoverageContext(ctx, member)
		if !errors.Is(err, errMemberNamesNoOne) || !errors.Is(err2, errMemberNamesNoOne) || !errors.Is(err3, errMemberNamesNoOne) {
			t.Fatalf("member %q: %v, %v, %v; want errMemberNamesNoOne", member, err, err2, err3)
		}
		if len(events) != 0 {
			t.Fatalf("member %q: events %+v; want none", member, events)
		}
	}
	events = nil
	o.ResolvePatientContext(ctx, "MBR-COVERED")
	if len(events) != 1 || events[0].Kind != "sor.read" {
		t.Fatalf("control: events %+v; want one sor.read", events)
	}
}

// A request sent alone that names its patient or reference only in another
// case is refused for that first, even when its reference is also blank: the
// SDK's lenient reader (from shn-sdk v1.15.0) refuses the blank member, and
// the gateway reads the exact names before it answers so. A payer gateway and
// an shnsdk.Responder then answer it alike (test/sdkparity).
func TestEligibilityAnotherCaseIsRefusedBeforeABlankReference(t *testing.T) {
	const head = `{"resourceType":"CoverageEligibilityRequest","status":"active","purpose":["validation"],"created":"2026-01-01","insurer":{"reference":"Organization/payer"},`
	for body, want := range map[string]string{
		head + `"Patient":{"reference":"Patient/"}}`: `eligibility request names "Patient" for "patient": member names are case-sensitive, so the request names no patient to bind`,
		head + `"patient":{"REFERENCE":""}}`:         `eligibility request names "REFERENCE" for "reference": member names are case-sensitive, so the request names no patient to bind`,
		head + `"patient":{"reference":"Patient/"}}`: errEligibilityNamesNoOne,
	} {
		subs, err := eligibilitySubmissions([]byte(body))
		if err != nil || len(subs) != 1 {
			t.Fatalf("%s: %d submissions, %v", body, len(subs), err)
		}
		_, err = eligibilitySubmissionMember(subs[0])
		if got, ok := eligibilityNamedInAnotherCase(err); !ok || got != want {
			t.Errorf("%s: refused as %q (%v); want %q", body, got, err, want)
		}
	}
}

// A request past the strict reader's own limits (here a byte not well-formed
// UTF-8), naming its patient by a reference that names no one, is refused with
// eligibility's text, whichever shn-sdk release the gateway is built with:
// never as a patient named only in another case.
func TestEligibilityPastTheStrictLimitsABlankReferenceNamesNoOne(t *testing.T) {
	for _, ref := range []string{"Patient/ ", "Patient/Patient/", "Patient/\t", " "} {
		body := `{"resourceType":"CoverageEligibilityRequest","status":"active","purpose":["validation"],"created":"2026-01-01","note":"` + "\xff" + `","insurer":{"reference":"Organization/payer"},"patient":{"reference":"` + strings.ReplaceAll(ref, "\t", `\t`) + `"}}`
		subs, err := eligibilitySubmissions([]byte(body))
		if err != nil || len(subs) != 1 {
			t.Fatalf("%q: %d submissions, %v", ref, len(subs), err)
		}
		_, err = eligibilitySubmissionMember(subs[0])
		if got, ok := eligibilityNamedInAnotherCase(err); !ok || got != errEligibilityNamesNoOne {
			t.Errorf("%q: refused as %q (%v); want %q", ref, got, err, errEligibilityNamesNoOne)
		}
	}
}

// Every origination fallback that answers a read's failure answers a member
// that names no one as the request's: 400, recorded on authority with the
// binding's reason, never the system's failure or the gateway's own.
func TestOriginationFallbacksAnswerAMemberThatNamesNoOne(t *testing.T) {
	err := fmt.Errorf("read: %w", errMemberNamesNoOne)
	for name, answer := range map[string]func(context.Context, http.ResponseWriter){
		"failPopulate":        func(ctx context.Context, w http.ResponseWriter) { (&Gateway{}).failPopulate(ctx, w, "payer", err) },
		"failEvidenceLinkage": func(ctx context.Context, w http.ResponseWriter) { failEvidenceLinkage(ctx, w, err) },
	} {
		x := &exchangeRecorder{}
		rec := httptest.NewRecorder()
		answer(context.WithValue(context.Background(), exchangeRecorderKey{}, x), rec)
		if rec.Code != http.StatusBadRequest || !strings.Contains(answerText(rec.Body.String()), msgMemberNamesNoOne) {
			t.Errorf("%s: answer %d %s; want 400 naming %q", name, rec.Code, rec.Body, msgMemberNamesNoOne)
		}
		if x.outcome != "" || x.guardRule != RefusalAuthority || x.guardReason != reasonMemberNamesNoOne {
			t.Errorf("%s: recorded outcome %q, rule %q, reason %q; want an authority refusal", name, x.outcome, x.guardRule, x.guardReason)
		}
	}
}

// A headless order dispatch for a member that names no one is refused 400 on
// authority before any leg, at every level: its open order is never read for
// it, and the refusal is never recorded as the system's failure.
func TestHandleDispatch_AMemberThatNamesNoOne(t *testing.T) {
	for _, member := range []string{"Patient/", "Patient/ "} {
		for _, level := range allLevels {
			t.Run(strings.ReplaceAll(member, " ", "_")+"/"+level.String(), func(t *testing.T) {
				fix := newMBROXDispatchFixture(t)
				mountOriginationRoutes(fix.gw)
				fix.gw.cfg.ConformanceEnforcement = level
				rec, recs := serveRecorded(t, fix.gw, "POST /scenario/dispatch", testRouteBody(fix.gw, "/scenario/dispatch", `"member":"`+member+`"`))
				if rec.Code != http.StatusBadRequest || !strings.HasPrefix(answerText(rec.Body.String()), msgMemberNamesNoOne) || len(fix.stub.legTypes) != 0 {
					t.Fatalf("answer %d %s legs %v; want 400 %q and no leg", rec.Code, rec.Body, fix.stub.legTypes, msgMemberNamesNoOne)
				}
				wantRefusal(t, onlyRecord(t, recs), http.StatusBadRequest, RefusedByProviderGateway, RefusalAuthority)
			})
		}
	}
}
