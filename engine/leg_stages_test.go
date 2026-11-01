//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// legStagesPayer is a recording payer whose clock only the fakes move: its
// own system takes listing to answer the listing and forward to answer the
// operation, and the Authorization Framework takes authorize to answer, so
// every stage's duration is exact.
func legStagesPayer(t *testing.T, listing, forward, authorize time.Duration) (*levelPayer, *exchangeRecords) {
	t.Helper()
	p, got := newRecordingLevelPayer(t, EnforcementObserve)
	clock := &testClock{now: p.g.cfg.Clock()}
	p.g.cfg.Clock = clock.Now
	// The responder reads the gateway's clock, as the app builds it.
	p.g.cfg.Responder.(*nativeResponder).clock = clock.Now
	inner := p.partner.srv.Config.Handler
	p.partner.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			clock.Add(listing)
		} else {
			clock.Add(forward)
		}
		inner.ServeHTTP(w, r)
	})
	p.g.cfg.Client = &http.Client{Transport: stagedTransport{inner: p.g.cfg.Client.Transport, before: func(*http.Request) (*http.Response, error) {
		clock.Add(authorize)
		return nil, nil
	}}}
	return p, got
}

// stagedTransport runs before ahead of each call: a non-nil answer or error
// from it is the call's.
type stagedTransport struct {
	inner  http.RoundTripper
	before func(*http.Request) (*http.Response, error)
}

func (s stagedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if resp, err := s.before(r); resp != nil || err != nil {
		return resp, err
	}
	return s.inner.RoundTrip(r)
}

func stagesSum(s *LegStages) time.Duration {
	return s.Unwrap + s.Reads + s.Forward + s.Validate + s.Seal + s.Ledger + s.Write
}

// An inbound leg's record says where its time went: each read of the payer's
// own system, the forward, and sealing and authorizing the answer, each
// exactly; the stages sum to the call's latency, since nothing else took
// time.
func TestExchangeRecord_LegStages(t *testing.T) {
	const listing, forward, authorize = 3 * time.Millisecond, 40 * time.Millisecond, 7 * time.Millisecond
	p, got := legStagesPayer(t, listing, forward, authorize)
	ans := p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
	if ans.status != http.StatusOK {
		t.Fatalf("answer %d %s", ans.status, ans.body)
	}
	rec := got.only(t)
	st := rec.Stages
	if st == nil {
		t.Fatal("an inbound leg's record has no stages")
	}
	if st.Reads != listing || st.Forward != forward || st.Seal != authorize {
		t.Fatalf("stages %+v, want reads %v, forward %v, seal %v", *st, listing, forward, authorize)
	}
	if rec.Latency != stagesSum(st) {
		t.Fatalf("stages sum to %v, latency %v: %+v", stagesSum(st), rec.Latency, *st)
	}
	// The reads (the listing, the member's record) and the forward are all
	// counted; the forward is the Backend.
	if rec.BackendCalls < 2 || rec.Backend.Latency != forward {
		t.Fatalf("backend %+v, %d calls", rec.Backend, rec.BackendCalls)
	}
}

// An ingress call carries no stages: they describe the answer to a leg.
func TestExchangeRecord_IngressHasNoStages(t *testing.T) {
	x := &exchangeRecorder{now: time.Now}
	x.rec.Direction = DirectionIngress
	x.stage(func(s *LegStages, d time.Duration) { s.Seal += d })()
	x.unwrapDone()
	if x.rec.Stages != nil {
		t.Fatalf("stages %+v", x.rec.Stages)
	}
}

// steppingClock moves a millisecond on every read, so every stage that reads
// it takes time.
type steppingClock struct{ testClock }

func (c *steppingClock) Now() time.Time {
	c.Add(time.Millisecond)
	return c.testClock.Now()
}

// steppingPayer is a recording payer at level whose gateway and responder
// read a steppingClock.
func steppingPayer(t *testing.T, level ConformanceEnforcement) (*levelPayer, *exchangeRecords) {
	t.Helper()
	p, got := newRecordingLevelPayer(t, level)
	clock := &steppingClock{testClock{now: p.g.cfg.Clock()}}
	p.g.cfg.Clock = clock.Now
	p.g.cfg.Responder.(*nativeResponder).clock = clock.Now
	return p, got
}

// MON-R22: a stage the exchange did not enter is not reported. At none no
// conformance check runs, so the leg spends no time validating and its record
// has no validate stage; at observe the check is queued, and at structural
// judged, on the leg's own time, so it has one.
func TestExchangeRecord_NoValidateStageWhereNoCheckRuns_MONR22(t *testing.T) {
	for _, c := range []struct {
		level ConformanceEnforcement
		want  bool
	}{{EnforcementNone, false}, {EnforcementObserve, true}, {EnforcementStructural, true}} {
		t.Run(c.level.String(), func(t *testing.T) {
			p, got := steppingPayer(t, c.level)
			if ans := p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148")); ans.status != http.StatusOK {
				t.Fatalf("answer %d %s", ans.status, ans.body)
			}
			rec := got.only(t)
			st := rec.Stages
			if st == nil || (st.Validate > 0) != c.want {
				t.Fatalf("stages %+v, want a validate stage %t", st, c.want)
			}
			if rec.Latency < stagesSum(st) {
				t.Fatalf("stages sum to %v, past the latency %v", stagesSum(st), rec.Latency)
			}
		})
	}
}

// A leg refused before its dispatch spent its time unwrapping.
func TestExchangeRecord_RefusedLegIsUnwrap(t *testing.T) {
	p, got := steppingPayer(t, EnforcementObserve)
	other, _ := genED25519(t)
	p.g.cfg.AuthzPub = other
	p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
	rec := got.only(t)
	if rec.Stages == nil || rec.Stages.Unwrap <= 0 || rec.Stages.Unwrap != rec.Latency-rec.Stages.Write || rec.Stages.Forward != 0 || rec.Stages.Seal != 0 {
		t.Fatalf("stages %+v, latency %v", rec.Stages, rec.Latency)
	}
}

// A leg refused before its dispatch with a framed answer (an operation
// header its transaction type does not define) seals that answer: the seal is
// its own stage, not counted again in the unwrap, and the stages never add up
// to more than the call.
func TestExchangeRecord_FramedRefusalBeforeDispatch(t *testing.T) {
	p, got := steppingPayer(t, EnforcementObserve)
	ans := p.send(t, "crd-order-select", "questionnaire-package", conformantCRD("MBR-COVERED", "72148"))
	if ans.status != http.StatusBadRequest || !ans.framed {
		t.Fatalf("answer %d framed=%v %s", ans.status, ans.framed, ans.body)
	}
	rec := got.only(t)
	st := rec.Stages
	if st == nil || st.Seal <= 0 || st.Unwrap <= 0 || st.Forward != 0 {
		t.Fatalf("stages %+v", st)
	}
	if sum := stagesSum(st); sum > rec.Latency {
		t.Fatalf("stages sum to %v, more than the latency %v: %+v", sum, rec.Latency, *st)
	}
}

// A PAS inquiry at structural spends time in the pend ledger's lookup and
// writes, as well as the reads, the forward and the seal. It runs no
// synchronous check: the payer's answer is relayed, not certified, and the
// gateway builds nothing from it (a check's own time is
// TestExchangeRecord_LegStagesValidateExactly). The stages never add up to
// more than the call.
func TestExchangeRecord_LegStagesLedger(t *testing.T) {
	p, got := steppingPayer(t, EnforcementStructural)
	p.seedInquiryPend(t, "corr-submit-1")
	p.partner.respByPath[pasInquirePath] = decidedAnswer(t)
	ans := p.send(t, "pas-claim-inquire", "", inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"))
	if ans.status != http.StatusOK {
		t.Fatalf("answer %d %s", ans.status, ans.body)
	}
	rec := got.only(t)
	st := rec.Stages
	if st == nil || st.Unwrap <= 0 || st.Forward <= 0 || st.Validate != 0 || st.Seal <= 0 || st.Ledger <= 0 || st.Write <= 0 {
		t.Fatalf("stages %+v", st)
	}
	if sum := stagesSum(st); sum > rec.Latency {
		t.Fatalf("stages sum to %v, more than the latency %v: %+v", sum, rec.Latency, *st)
	}
}

// When the answer, decided after the payer's system answered, cannot be
// authorized, the payer gateway answers the forward with its attested 502
// (X-SHN-Delivered: yes: the system answered), and the record and the log
// say why: the call out of time (timeout), a denial (auth), no answer
// (unreachable), or any other failure (other), the Framework's refusal on the
// audit rule included: the answer's own class list does not name rules. The
// requester gone (cancelled) is said the same way, on the plain 502: nobody is
// listening for an attestation.
func TestExchangeRecord_AnswerErrorClass(t *testing.T) {
	var logs bytes.Buffer
	var mu sync.Mutex
	prev := log.Writer()
	log.SetOutput(writerFunc(func(b []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return logs.Write(b) }))
	t.Cleanup(func() { log.SetOutput(prev) })
	for class, fail := range map[string]func(cancel context.CancelFunc) (*http.Response, error){
		BackendCancelled: func(cancel context.CancelFunc) (*http.Response, error) {
			cancel()
			return nil, context.Canceled
		},
		BackendTimeout: func(context.CancelFunc) (*http.Response, error) {
			return nil, &net.OpError{Op: "dial", Err: timeoutError{}}
		},
		BackendAuth: func(context.CancelFunc) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusForbidden, Body: http.NoBody, Header: http.Header{}}, nil
		},
		ExchangeUnreachable: func(context.CancelFunc) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody, Header: http.Header{}}, nil
		},
		ExchangeOther: func(context.CancelFunc) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{"error":"decision audit failed","rule":"audit"}`))}, nil
		},
	} {
		t.Run(class, func(t *testing.T) {
			p, got := newRecordingLevelPayer(t, EnforcementObserve)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p.ctx = ctx
			p.g.cfg.Client = &http.Client{Transport: stagedTransport{inner: p.g.cfg.Client.Transport, before: func(r *http.Request) (*http.Response, error) {
				resp, err := fail(cancel)
				if resp != nil {
					resp.Request = r
				}
				return resp, err
			}}}
			ans := p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
			if attested := class != BackendCancelled; ans.status != http.StatusBadGateway || ans.framed || (ans.header.Get(HubDeliveredHeader) == "yes") != attested {
				t.Fatalf("answer %d %s (delivered %q), want the 502, attested %v", ans.status, ans.body, ans.header.Get(HubDeliveredHeader), attested)
			}
			rec := got.only(t)
			if rec.AnswerError != class || rec.Backend == nil || rec.Backend.ErrorClass != "" {
				t.Fatalf("answer error %q, backend %+v; want %q after a usable backend answer", rec.AnswerError, rec.Backend, class)
			}
			mu.Lock()
			line := "crd-order-select answer not authorized: " + class + " (correlation " + ans.corr + ")"
			found := strings.Contains(logs.String(), line)
			mu.Unlock()
			if !found {
				t.Fatalf("no log line %q in:\n%s", line, logs.String())
			}
		})
	}
}

// An answered leg records no answer error.
func TestExchangeRecord_NoAnswerErrorWhenAuthorized(t *testing.T) {
	p, got := newRecordingLevelPayer(t, EnforcementObserve)
	p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
	if rec := got.only(t); rec.AnswerError != "" {
		t.Fatalf("answer error %q", rec.AnswerError)
	}
}

// timedLedger is the test system of record whose pend ledger takes lookup to
// look an authorization up and record to record a decision. It holds the whole
// ledger, so the two it times are its own.
type timedLedger struct {
	*censusSoR
	PendLedger
	clock          *testClock
	lookup, record time.Duration
}

func (l timedLedger) LookupPended(requesterHolder string, k PendKeys) (PendMatch, error) {
	l.clock.Add(l.lookup)
	return l.PendLedger.LookupPended(requesterHolder, k)
}

func (l timedLedger) RecordDecision(subjectPCI, corrID, outcome string, decidedAt time.Time, k PendKeys) (PendTransition, error) {
	l.clock.Add(l.record)
	return l.PendLedger.RecordDecision(subjectPCI, corrID, outcome, decidedAt, k)
}

// A PAS inquiry's ledger stage is its lookup of the authorization the answer
// is about and its write of the decision, each timed: exactly their sum.
func TestExchangeRecord_LegStagesLedgerExactly(t *testing.T) {
	const lookup, record = 11 * time.Millisecond, 13 * time.Millisecond
	p, got := legStagesPayer(t, 0, 0, 0)
	clock := &testClock{now: p.g.cfg.Clock()}
	p.g.cfg.Clock = clock.Now
	p.g.cfg.Responder.(*nativeResponder).clock = clock.Now
	p.seedInquiryPend(t, "corr-submit-1")
	p.g.cfg.Store = timedLedger{censusSoR: p.store, PendLedger: testLedger(p.store), clock: clock, lookup: lookup, record: record}
	p.partner.respByPath[pasInquirePath] = decidedAnswer(t)
	ans := p.send(t, "pas-claim-inquire", "", inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"))
	if ans.status != http.StatusOK {
		t.Fatalf("answer %d %s", ans.status, ans.body)
	}
	rec := got.only(t)
	if rec.Stages == nil || rec.Stages.Ledger != lookup+record {
		t.Fatalf("ledger stage %+v, want %v", rec.Stages, lookup+record)
	}
}

// timedClaim is the test system of record whose claim on the authorization
// an amendment names takes claim, and releasing it takes release. It holds the
// whole ledger, so the claim it times is its own.
type timedClaim struct {
	*censusSoR
	PendLedger
	clock          *testClock
	claim, release time.Duration
}

func (l timedClaim) BeginClaimUpdateReason(subjectPCI, related string) (bool, PendRefusal, error) {
	l.clock.Add(l.claim)
	return l.PendLedger.BeginClaimUpdateReason(subjectPCI, related)
}

func (l timedClaim) ReleaseClaimUpdate(subjectPCI, related string) error {
	l.clock.Add(l.release)
	return l.censusSoR.ReleaseClaimUpdate(subjectPCI, related)
}

// A PAS update's ledger stage is the claim it takes on the authorization it
// amends: exactly the claim's time.
func TestExchangeRecord_LegStagesUpdateClaimExactly(t *testing.T) {
	t.Parallel()
	const claim = 17 * time.Millisecond
	p, got := legStagesPayer(t, 0, 0, 0)
	clock := &testClock{now: p.g.cfg.Clock()}
	p.g.cfg.Clock = clock.Now
	n := p.g.cfg.Responder.(*nativeResponder)
	n.clock = clock.Now
	request, related := updateBundle(t)
	p.seedPend(t, related)
	n.paNative().store = timedClaim{censusSoR: p.store, PendLedger: testLedger(p.store), clock: clock, claim: claim}
	ans := p.send(t, "pas-claim-update", "", request)
	if ans.status != http.StatusOK {
		t.Fatalf("answer %d %s", ans.status, ans.body)
	}
	rec := got.only(t)
	if rec.Stages == nil || rec.Stages.Ledger != claim {
		t.Fatalf("ledger stage %+v, want %v", rec.Stages, claim)
	}
}

// A PAS update the payer's system refuses releases the claim it took: the
// release is the ledger's write, so the ledger stage is exactly the claim and
// the release.
func TestExchangeRecord_LegStagesUpdateReleaseExactly(t *testing.T) {
	t.Parallel()
	const claim, release = 17 * time.Millisecond, 19 * time.Millisecond
	p, got := legStagesPayer(t, 0, 0, 0)
	clock := &testClock{now: p.g.cfg.Clock()}
	p.g.cfg.Clock = clock.Now
	n := p.g.cfg.Responder.(*nativeResponder)
	n.clock = clock.Now
	request, related := updateBundle(t)
	p.seedPend(t, related)
	n.paNative().store = timedClaim{censusSoR: p.store, PendLedger: testLedger(p.store), clock: clock, claim: claim, release: release}
	inner := p.partner.srv.Config.Handler
	p.partner.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == pasSubmitPath {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		inner.ServeHTTP(w, r)
	})
	ans := p.send(t, "pas-claim-update", "", request)
	if ans.status == http.StatusOK {
		t.Fatalf("answer %d %s, want the payer's refusal relayed", ans.status, ans.body)
	}
	rec := got.only(t)
	if rec.Stages == nil || rec.Stages.Ledger != claim+release {
		t.Fatalf("ledger stage %+v, want %v", rec.Stages, claim+release)
	}
}

// Whatever the leg, a responder's claim released because the answer was not
// committed is the ledger's write: a submit's or an inquiry's release (a
// participant's own responder may take one) is timed as an update's is.
func TestExchangeRecord_LegStagesReleaseOnEveryLeg(t *testing.T) {
	const release = 23 * time.Millisecond
	for leg, body := range map[string]func(t *testing.T) []byte{
		"pas-claim":         func(t *testing.T) []byte { return originatorBuiltConformantBundle(t, "MBR-COVERED") },
		"pas-claim-inquire": func(*testing.T) []byte { return inquiryBundle("MBR-COVERED", "", "TRN-1", "72148") },
	} {
		t.Run(leg, func(t *testing.T) {
			p, got := legStagesPayer(t, 0, 0, 0)
			clock := &testClock{now: p.g.cfg.Clock()}
			p.g.cfg.Clock = clock.Now
			var released atomic.Int32
			p.g.cfg.Responder = pasResultResponder{result: LegResult{Status: http.StatusConflict, Message: "held", Rollback: func() {
				released.Add(1)
				clock.Add(release)
			}}}
			p.send(t, leg, "", body(t))
			rec := got.only(t)
			if released.Load() != 1 || rec.Stages == nil || rec.Stages.Ledger != release {
				t.Fatalf("%d releases, stages %+v, want the ledger stage %v", released.Load(), rec.Stages, release)
			}
		})
	}
}

// timedValidator takes d for every check and counts them.
type timedValidator struct {
	inner shnsdk.Validator
	clock *testClock
	d     time.Duration
	calls *atomic.Int32
	// only, when set, limits the time and the count to the checks of payloads
	// that contain it: the checks a leg queues at observe run at once, on one
	// clock, so only one check may move it.
	only string
}

func (v timedValidator) Validate(ctx context.Context, resource []byte, profile string) (shnsdk.Result, error) {
	if v.only == "" || bytes.Contains(resource, []byte(v.only)) {
		v.calls.Add(1)
		v.clock.Add(v.d)
	}
	return v.inner.Validate(ctx, resource, profile)
}

// failsOn is a validator lane that is down for the payloads that contain
// marker and finds every other payload valid.
type failsOn struct{ marker string }

func (f failsOn) Validate(_ context.Context, resource []byte, _ string) (shnsdk.Result, error) {
	if bytes.Contains(resource, []byte(f.marker)) {
		return shnsdk.Result{}, errors.New("lane down")
	}
	return shnsdk.Result{Valid: true}, nil
}

// Every synchronous check is in the validate stage, the CRD answer's
// embedded resources included: at structural its time is exactly what the
// validator took.
func TestExchangeRecord_LegStagesValidateExactly(t *testing.T) {
	const d = 5 * time.Millisecond
	p, got := newRecordingLevelPayer(t, EnforcementStructural)
	clock := &testClock{now: p.g.cfg.Clock()}
	p.g.cfg.Clock = clock.Now
	p.g.cfg.Responder.(*nativeResponder).clock = clock.Now
	var calls atomic.Int32
	p.g.cfg.Validator = timedValidator{inner: p.g.cfg.Validator, clock: clock, d: d, calls: &calls}
	ans := p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
	if ans.status != http.StatusOK {
		t.Fatalf("answer %d %s", ans.status, ans.body)
	}
	rec := got.only(t)
	if n := calls.Load(); n == 0 || rec.Stages == nil || rec.Stages.Validate != time.Duration(n)*d {
		t.Fatalf("validate stage %+v, want %d checks × %v", rec.Stages, n, d)
	}
}

// frozenPayer is a recording payer at level whose clock only the fakes move,
// with a validator that takes d for every check (of a payload containing
// only, when set).
func frozenPayer(t *testing.T, level ConformanceEnforcement, inner shnsdk.Validator, d time.Duration, only string) (*levelPayer, *exchangeRecords, *testClock, *atomic.Int32) {
	t.Helper()
	p, got := newRecordingLevelPayer(t, level)
	clock := &testClock{now: p.g.cfg.Clock()}
	p.g.cfg.Clock = clock.Now
	p.g.cfg.Responder.(*nativeResponder).clock = clock.Now
	var calls atomic.Int32
	if inner == nil {
		inner = p.g.cfg.Validator
	}
	p.g.cfg.Validator = timedValidator{inner: inner, clock: clock, d: d, calls: &calls, only: only}
	return p, got, clock, &calls
}

// Each conformance finding carries the time its $validate calls took: inline
// at structural, off the leg's path at observe (the finding is made when the
// validator answers), and to its failure for a validator that could not
// judge.
func TestFindingCarriesValidatorTime(t *testing.T) {
	const d = 7 * time.Millisecond
	// Only the request's check judges the order, and only it takes time: the
	// leg's other checks find their payloads valid and record nothing.
	const order = `"ServiceRequest"`
	for name, row := range map[string]struct {
		level   ConformanceEnforcement
		v       shnsdk.Validator
		verdict string
	}{
		"inline, invalid":       {EnforcementStructural, &shnsdk.FakeValidator{RejectIfContains: order}, ""},
		"off the path, invalid": {EnforcementObserve, &shnsdk.FakeValidator{RejectIfContains: order}, ""},
		"an outage":             {EnforcementObserve, failsOn{marker: order}, "unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			p, _, _, calls := frozenPayer(t, row.level, row.v, d, order)
			p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
			p.mu.Lock()
			defer p.mu.Unlock()
			var fhir []ConformanceFinding
			for _, f := range p.findings {
				if f.Kind == string(KindFHIRIngress) || f.Kind == string(KindFHIREgress) {
					fhir = append(fhir, f)
				}
			}
			// An outage may be tried on more than one lane: the time is every
			// call's, each taking d.
			n := int64(calls.Load())
			if len(fhir) != 1 || n == 0 || fhir[0].Verdict != row.verdict || fhir[0].ValidatorMs != n*d.Milliseconds() || (row.verdict == "" && n != 1) {
				t.Fatalf("%d calls of the order's check, FHIR findings %+v: want one, verdict %q and validatorMs %d per call", n, fhir, row.verdict, d.Milliseconds())
			}
		})
	}
}

// A candidate-line certification's finding carries each line's own $validate
// time (to its failure for a line that could not judge), and validatorMs
// their sum: a lane's time is never another's.
func TestFindingCarriesEachLinesValidatorTime(t *testing.T) {
	took := map[string]time.Duration{"2.0": 2 * time.Millisecond, "2.1": 3 * time.Millisecond, "2.2": 5 * time.Millisecond}
	for _, tc := range []struct {
		name     string
		verdicts map[string]scriptedVerdict
		want     []LineVerdictSummary
	}{
		{"valid on another line", map[string]scriptedVerdict{"2.0": scriptStructural, "2.1": scriptValid, "2.2": scriptValid},
			[]LineVerdictSummary{{Line: "2.0", Verdict: "structural", Ms: 2}, {Line: "2.2", Verdict: "valid", Ms: 5}}},
		{"no line answered", map[string]scriptedVerdict{"2.0": scriptOutage, "2.1": scriptOutage, "2.2": scriptOutage},
			[]LineVerdictSummary{{Line: "2.0", Verdict: "unavailable", Ms: 2}, {Line: "2.2", Verdict: "unavailable", Ms: 5}, {Line: "2.1", Verdict: "unavailable", Ms: 3}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, _, findings := payerIngressGateway(t, providerLanes[0], EnforcementStructural, tc.verdicts)
			clock := &testClock{now: g.cfg.Clock()}
			g.cfg.Clock = clock.Now
			var calls atomic.Int32
			// One timed validator per line, shared by every contract at it.
			timed := map[string]shnsdk.Validator{}
			for key, v := range g.cfg.ValidatorsByContractLine {
				line := shnsdk.LineOf(key)
				if timed[line] == nil {
					timed[line] = timedValidator{inner: v, clock: clock, d: took[line], calls: &calls}
				}
				g.cfg.ValidatorsByContractLine[key] = timed[line]
			}
			g.cfg.Validator = timed["2.0"]
			if status, msg := drainedPayerIngress(g, answerCtx("pas-claim"), []byte(answerBundle), "2.0", "pa.pas", partner); status != 0 {
				t.Fatalf("status %d %q, want relayed", status, msg)
			}
			var sum int64
			for _, l := range tc.want {
				sum += l.Ms
			}
			if len(*findings) != 1 || !reflect.DeepEqual((*findings)[0].Lines, tc.want) || (*findings)[0].ValidatorMs != sum {
				t.Fatalf("findings %+v, want lines %+v and validatorMs %d", *findings, tc.want, sum)
			}
		})
	}
}

// A PAS inquiry whose every slow step is a fake's: its stages are exactly the
// time each took, and they sum exactly to the call's latency.
func TestExchangeRecord_LegStagesInquiryExactly(t *testing.T) {
	const listing, forward, authorize, check, lookup, record = 2 * time.Millisecond, 30 * time.Millisecond, 4 * time.Millisecond, 3 * time.Millisecond, 5 * time.Millisecond, 6 * time.Millisecond
	p, got := legStagesPayer(t, listing, forward, authorize)
	clock := &testClock{now: p.g.cfg.Clock()}
	p.g.cfg.Clock = clock.Now
	p.g.cfg.Responder.(*nativeResponder).clock = clock.Now
	p.seedInquiryPend(t, "corr-submit-1")
	var calls atomic.Int32
	p.g.cfg.Validator = timedValidator{inner: p.g.cfg.Validator, clock: clock, d: check, calls: &calls}
	p.g.cfg.Store = timedLedger{censusSoR: p.store, PendLedger: testLedger(p.store), clock: clock, lookup: lookup, record: record}
	// The partner and the Authorization Framework move this clock too.
	inner := p.partner.srv.Config.Handler
	var reads, authorizations atomic.Int32
	p.partner.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			reads.Add(1)
			clock.Add(listing)
		} else {
			clock.Add(forward)
		}
		inner.ServeHTTP(w, r)
	})
	p.g.cfg.Client = &http.Client{Transport: stagedTransport{inner: p.g.cfg.Client.Transport.(stagedTransport).inner, before: func(*http.Request) (*http.Response, error) {
		authorizations.Add(1)
		clock.Add(authorize)
		return nil, nil
	}}}
	p.partner.respByPath[pasInquirePath] = decidedAnswer(t)
	ans := p.send(t, "pas-claim-inquire", "", inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"))
	if ans.status != http.StatusOK {
		t.Fatalf("answer %d %s", ans.status, ans.body)
	}
	rec := got.only(t)
	st := rec.Stages
	if st == nil || st.Reads != time.Duration(reads.Load())*listing || st.Forward != forward || st.Ledger != lookup+record ||
		st.Validate != time.Duration(calls.Load())*check || st.Seal != time.Duration(authorizations.Load())*authorize || authorizations.Load() == 0 {
		t.Fatalf("stages %+v (%d reads, %d checks, %d authorizations)", st, reads.Load(), calls.Load(), authorizations.Load())
	}
	if sum := stagesSum(st); sum != rec.Latency {
		t.Fatalf("stages sum to %v, latency %v: %+v", sum, rec.Latency, *st)
	}
}

// A real client timeout on the answer's authorization is timeout; a denial
// that came back after the request ended is cancelled: the requester had
// gone, whatever the call answered.
func TestExchangeRecord_AnswerErrorClassPrecedence(t *testing.T) {
	t.Parallel()
	t.Run("a client timeout", func(t *testing.T) {
		p, got := newRecordingLevelPayer(t, EnforcementObserve)
		p.g.cfg.Client = &http.Client{Timeout: 30 * time.Millisecond, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			<-r.Context().Done()
			return nil, r.Context().Err()
		})}
		p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
		if rec := got.only(t); rec.AnswerError != BackendTimeout {
			t.Fatalf("answer error %q, want timeout", rec.AnswerError)
		}
	})
	t.Run("a denial after the request ended", func(t *testing.T) {
		p, got := newRecordingLevelPayer(t, EnforcementObserve)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		p.ctx = ctx
		p.g.cfg.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			cancel()
			return &http.Response{StatusCode: http.StatusForbidden, Body: http.NoBody, Header: http.Header{}, Request: r}, nil
		})}
		p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
		if rec := got.only(t); rec.AnswerError != BackendCancelled {
			t.Fatalf("answer error %q, want cancelled", rec.AnswerError)
		}
	})
}

// afterTheHub is the time the originator spends on a leg once the Hub's answer
// is in hand (networkTimedExchange): it is this gateway's own time, never the
// network's. readingTheHub is the time reading the Hub's answer body takes:
// the network's, since the wait runs until the answer is read.
const (
	afterTheHub   = 3 * time.Millisecond
	readingTheHub = 2 * time.Millisecond
)

// clockedBody moves clock by d on its first Read: an answer body that takes
// time to arrive.
type clockedBody struct {
	io.ReadCloser
	clock *testClock
	d     time.Duration
	once  sync.Once
}

func (b *clockedBody) Read(p []byte) (int, error) {
	b.once.Do(func() { b.clock.Add(b.d) })
	return b.ReadCloser.Read(p)
}

// bodyTransport hands every /route answer's body to wrap.
type bodyTransport struct {
	inner http.RoundTripper
	wrap  func(io.ReadCloser) io.ReadCloser
}

func (b bodyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := b.inner.RoundTrip(r)
	if err == nil && resp != nil && strings.HasSuffix(r.URL.Path, "/route") {
		resp.Body = b.wrap(resp.Body)
	}
	return resp, err
}

// networkTimedExchange is an in-process exchange whose originator's clock only
// the fakes move: each request to the Hub's /route takes hub before inner
// answers it, and reading its answer's body takes readingTheHub; each other
// call (the Authorization Framework's) takes authorize; and the leg's
// handling once the Hub's answer or failure is back takes afterTheHub. So the
// network stage is exact, and must run until the answer was read and stop
// there. authorizes counts the other calls.
func networkTimedExchange(t *testing.T, hub, authorize time.Duration, inner func(*inProcessExchange) http.RoundTripper) (*inProcessExchange, *exchangeRecords, *atomic.Int32) {
	t.Helper()
	env := newInProcessExchange(t)
	var got exchangeRecords
	env.originator.cfg.ExchangeObserved = got.observe
	clock := &testClock{now: env.originator.cfg.Clock()}
	env.originator.cfg.Clock = clock.Now
	next := http.RoundTripper(env.substrate)
	if inner != nil {
		next = inner(env)
	}
	// The leg's outcome is observed once the Hub's answer is read (or the
	// request failed): the time after it is the gateway's own.
	observed := env.originator.cfg.Observer
	env.originator.cfg.Observer = func(e ObserverEvent) {
		if e.Kind == "leg.response" || e.Kind == "leg.failed" {
			clock.Add(afterTheHub)
		}
		if observed != nil {
			observed(e)
		}
	}
	var authorizes atomic.Int32
	next = bodyTransport{inner: next, wrap: func(rc io.ReadCloser) io.ReadCloser {
		return &clockedBody{ReadCloser: rc, clock: clock, d: readingTheHub}
	}}
	env.originator.cfg.Client = &http.Client{Transport: stagedTransport{inner: next, before: func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/route") {
			clock.Add(hub)
		} else {
			authorizes.Add(1)
			clock.Add(authorize)
		}
		return nil, nil
	}}}
	return env, &got, &authorizes
}

// MON-R22: an ingress call's network stage is exactly its wait on the Hub and
// the peer, one request, and the rest of the call is this gateway's own time:
// the Authorization Framework's before the request, and its handling of the
// answer after it, which the network stage excludes.
func TestExchangeRecord_IngressNetworkStage_MONR22(t *testing.T) {
	const hub, authorize = 40 * time.Millisecond, 7 * time.Millisecond
	env, got, authorizes := networkTimedExchange(t, hub, authorize, nil)
	env.payerReturns(LegResult{})
	w := httptest.NewRecorder()
	env.originator.ingressRoute(RouteCRD)(w, env.crdIngressRequest(t))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	rec := got.only(t)
	if rec.Network == nil || rec.Network.Latency != hub+readingTheHub || rec.Network.Calls != 1 {
		t.Fatalf("network %+v, want %v and %v reading the answer, over 1 call", rec.Network, hub, readingTheHub)
	}
	if self := rec.Latency - rec.Network.Latency; authorizes.Load() == 0 || self != time.Duration(authorizes.Load())*authorize+afterTheHub {
		t.Fatalf("self %v (latency %v), want %d authorizations × %v and %v after the Hub", self, rec.Latency, authorizes.Load(), authorize, afterTheHub)
	}
}

// MON-R22: a request handed to the transport is the network's time whatever
// the Hub made of it, and the wait ends on every way the request returns: the
// Hub refused it, the Hub was not reached, or its answer could not be read.
// The leg's handling after that is not the network's.
func TestExchangeRecord_IngressNetworkStageOnFailure_MONR22(t *testing.T) {
	const hub, authorize = 25 * time.Millisecond, time.Millisecond
	for name, row := range map[string]struct {
		hub  hubAnswers
		wait time.Duration
	}{
		"hub refusal":       {hubAnswers{status: 409, reason: "replay detected", delivered: "no"}, hub + readingTheHub},
		"hub not reached":   {hubAnswers{fail: true}, hub},
		"answer unreadable": {hubAnswers{unreadable: true}, hub + readingTheHub},
	} {
		t.Run(name, func(t *testing.T) {
			h := row.hub
			env, got, authorizes := networkTimedExchange(t, hub, authorize, func(env *inProcessExchange) http.RoundTripper {
				h.next = env.substrate
				return h
			})
			w := httptest.NewRecorder()
			env.originator.ingressRoute(RouteCRD)(w, env.crdIngressRequest(t))
			rec := got.only(t)
			if w.Code/100 == 2 || rec.Network == nil || rec.Network.Latency != row.wait || rec.Network.Calls != 1 {
				t.Fatalf("answer %d, network %+v, want %v over 1 call", w.Code, rec.Network, row.wait)
			}
			if self := rec.Latency - rec.Network.Latency; self != time.Duration(authorizes.Load())*authorize+afterTheHub {
				t.Fatalf("self %v, want %d authorizations × %v and %v after the Hub", self, authorizes.Load(), authorize, afterTheHub)
			}
		})
	}
}

// MON-R22: an ingress call refused before it sent anything to the Hub waited
// on no network, and records none rather than a zero wait.
func TestExchangeRecord_IngressRefusedBeforeTheHubHasNoNetwork_MONR22(t *testing.T) {
	env, got, _ := networkTimedExchange(t, time.Second, 0, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/Claim/$submit", strings.NewReader(pasIngressBundle("99999", "")))
	env.originator.ingressRoute(RoutePAS)(w, r)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if rec := got.only(t); rec.Network != nil {
		t.Fatalf("network %+v on a call refused before the Hub", rec.Network)
	}
}

// MON-R22: the network stage is an ingress call's: an inbound leg's time is in
// its LegStages.
func TestExchangeRecord_InboundHasNoNetwork_MONR22(t *testing.T) {
	p, got := newRecordingLevelPayer(t, EnforcementObserve)
	p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
	if rec := got.only(t); rec.Network != nil {
		t.Fatalf("network %+v on an inbound leg", rec.Network)
	}
	x := &exchangeRecorder{now: func() time.Time { t.Fatal("an inbound leg's network stage read the clock"); return time.Time{} }}
	x.rec.Direction = DirectionInbound
	x.network()()
	if x.rec.Network != nil {
		t.Fatalf("network %+v", x.rec.Network)
	}
}

// MON-R22: requests to the Hub outstanding together are timed once, so the
// stage never exceeds the call; time between requests is not the network's.
func TestExchangeRecorder_NetworkOverlapTimedOnce_MONR22(t *testing.T) {
	clock := &testClock{now: time.Unix(1700000000, 0)}
	x := &exchangeRecorder{now: clock.Now}
	x.rec.Direction = DirectionIngress
	a := x.network()
	clock.Add(5 * time.Millisecond)
	b := x.network()
	clock.Add(5 * time.Millisecond)
	a()
	clock.Add(10 * time.Millisecond)
	b()
	if n := x.rec.Network; n == nil || n.Latency != 20*time.Millisecond || n.Calls != 2 {
		t.Fatalf("network %+v, want 20ms over 2 calls", n)
	}
	clock.Add(7 * time.Millisecond)
	c := x.network()
	clock.Add(3 * time.Millisecond)
	c()
	if n := x.rec.Network; n.Latency != 23*time.Millisecond || n.Calls != 3 {
		t.Fatalf("network %+v, want 23ms over 3 calls", n)
	}
}

// MON-R22: a request to the Hub still outstanding when the answer is written
// waited until then.
func TestExchangeRecord_NetworkOutstandingAtTheAnswer_MONR22(t *testing.T) {
	clock := &testClock{now: time.Unix(1700000000, 0)}
	var got exchangeRecords
	g := &Gateway{cfg: Config{Clock: clock.Now, ExchangeObserved: got.observe, HolderID: "provider"}}
	g.recordExchange(RouteCRD, DirectionIngress, func(w http.ResponseWriter, r *http.Request) {
		clock.Add(2 * time.Millisecond)
		exchangeOf(r.Context()).network()
		clock.Add(9 * time.Millisecond)
		w.WriteHeader(http.StatusBadGateway)
	})(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))
	rec := got.only(t)
	if rec.Network == nil || rec.Network.Latency != 9*time.Millisecond || rec.Latency != 11*time.Millisecond {
		t.Fatalf("network %+v, latency %v, want 9ms of 11ms", rec.Network, rec.Latency)
	}
}

// panicsOnRoute panics on a request to the Hub's /route.
type panicsOnRoute struct{ next http.RoundTripper }

func (p panicsOnRoute) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/route") {
		panic("transport panicked")
	}
	return p.next.RoundTrip(r)
}

// MON-R22: a Hub request whose transport panics is still recorded: the call
// panics on to the server, its record is emitted, and the request's wait is
// settled when the call ended: the Hub's time, which is all that passed after
// the request was handed to the transport.
func TestExchangeRecord_IngressNetworkStageOnPanic_MONR22(t *testing.T) {
	const hub = 25 * time.Millisecond
	env, got, _ := networkTimedExchange(t, hub, time.Millisecond, func(env *inProcessExchange) http.RoundTripper {
		return panicsOnRoute{next: env.substrate}
	})
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the transport's panic did not reach the caller")
			}
		}()
		env.originator.ingressRoute(RouteCRD)(httptest.NewRecorder(), env.crdIngressRequest(t))
	}()
	rec := got.only(t)
	if rec.Network == nil || rec.Network.Calls != 1 || rec.Network.Latency != hub || rec.Network.Latency > rec.Latency {
		t.Fatalf("network %+v, latency %v: want one request, its wait %v, within the latency", rec.Network, rec.Latency, hub)
	}
}

// exchangeShape is what an exchange carries that decides the EMF lines it
// writes (MON-R47): from its record, which stages took time, whether the
// network wait or a body size is known, and the outcome, refusal and backend
// call; and what it reported through the engine's metric hooks beside the
// record. Byte counts and durations are left out: they do not change the
// lines.
type exchangeShape struct {
	Name          string        `json:"name"`
	Source        string        `json:"source"`
	Level         string        `json:"level"`
	Direction     string        `json:"direction"`
	Exchange      string        `json:"exchange"`
	Outcome       string        `json:"outcome"`
	Rule          string        `json:"rule,omitempty"`
	RefusedBy     string        `json:"refusedBy,omitempty"`
	Backend       *shapeBackend `json:"backend,omitempty"`
	Stages        []string      `json:"stages,omitempty"`
	Rest          bool          `json:"rest,omitempty"`
	NetworkCalls  int           `json:"networkCalls,omitempty"`
	Self          bool          `json:"self,omitempty"`
	RequestBytes  bool          `json:"requestBytes,omitempty"`
	ResponseBytes bool          `json:"responseBytes,omitempty"`
	// What the exchange reported through the engine's metric hooks
	// (shapeReports): each writes a line of its own.
	Legs           []string `json:"legs,omitempty"`
	Tokens         []string `json:"tokens,omitempty"`
	Involved       []string `json:"involved,omitempty"`
	StoreErrors    []string `json:"storeErrors,omitempty"`
	ValidatorCalls int      `json:"validatorCalls,omitempty"`
	Findings       int      `json:"findings,omitempty"`
	ObserveDropped int      `json:"observeDropped,omitempty"`
}

type shapeBackend struct {
	Status     int    `json:"status,omitempty"`
	ErrorClass string `json:"errorClass,omitempty"`
}

// shapeReports is what an exchange reports through the engine's metric hooks
// beside its record; an observe check reports from its worker.
type shapeReports struct {
	mu                                  sync.Mutex
	legs, tokens, involved, storeErrors []string
	validator, findings, dropped        int
}

// wire sets every metric hook of g's config to record into r, and fails on
// one it does not know: a hook the EMF cost does not count is a line it
// leaves out.
func (r *shapeReports) wire(t *testing.T, g *Gateway) {
	t.Helper()
	lock := func(f func()) { r.mu.Lock(); defer r.mu.Unlock(); f() }
	g.cfg.LegMetric = func(o string) { lock(func() { r.legs = append(r.legs, o) }) }
	g.cfg.TokenMetric = func(o string, _ time.Duration) { lock(func() { r.tokens = append(r.tokens, o) }) }
	g.cfg.InvolvedMetric = func(reason string) { lock(func() { r.involved = append(r.involved, reason) }) }
	g.cfg.StoreErrorMetric = func(store string) { lock(func() { r.storeErrors = append(r.storeErrors, store) }) }
	g.cfg.ValidatorMetric = func(string, string, time.Duration) { lock(func() { r.validator++ }) }
	g.cfg.FindingMetric = func(FindingMetric) { lock(func() { r.findings++ }) }
	g.cfg.ObserveCheckDroppedMetric = func() { lock(func() { r.dropped++ }) }
	// The requests in flight are sampled once a minute: no line per exchange.
	g.cfg.ActiveRequestMetric = func(string, int) {}
	cfg := reflect.ValueOf(g.cfg)
	for n := range cfg.NumField() {
		f := cfg.Type().Field(n)
		if strings.HasSuffix(f.Name, "Metric") && f.Type.Kind() == reflect.Func && cfg.Field(n).IsNil() {
			t.Fatalf("engine.Config.%s is a metric hook the exchange shapes do not capture", f.Name)
		}
	}
}

// shapeOf is rec's shape, with what r recorded.
func shapeOf(name, source string, level ConformanceEnforcement, rec ExchangeRecord, r *shapeReports) exchangeShape {
	sh := exchangeShape{Name: name, Source: source, Level: level.String(), Direction: rec.Direction, Exchange: rec.Exchange, Outcome: rec.Outcome,
		Rule: rec.Rule, RefusedBy: rec.RefusedBy, RequestBytes: rec.RequestBytes != nil, ResponseBytes: rec.ResponseBytes != nil}
	if rec.Backend != nil {
		sh.Backend = &shapeBackend{Status: rec.Backend.Status, ErrorClass: rec.Backend.ErrorClass}
	}
	if st := rec.Stages; st != nil {
		for _, s := range []struct {
			name string
			d    time.Duration
		}{{"unwrap", st.Unwrap}, {"reads", st.Reads}, {"forward", st.Forward}, {"validate", st.Validate}, {"seal", st.Seal}, {"ledger", st.Ledger}, {"write", st.Write}} {
			if s.d > 0 {
				sh.Stages = append(sh.Stages, s.name)
			}
		}
		sh.Rest = rec.Latency > stagesSum(st)
	}
	if rec.Direction == DirectionIngress {
		var wait time.Duration
		if rec.Network != nil {
			sh.NetworkCalls, wait = rec.Network.Calls, rec.Network.Latency
		}
		sh.Self = rec.Latency > wait
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	sh.Legs, sh.Tokens, sh.Involved, sh.StoreErrors = r.legs, r.tokens, r.involved, r.storeErrors
	sh.ValidatorCalls, sh.Findings, sh.ObserveDropped = r.validator, r.findings, r.dropped
	return sh
}

// exchangeShapesPath is the golden of the exchange shapes the EMF cost is
// measured over (gateway/app TestEMFCost_MON_R47 reads it). Set
// SHN_UPDATE_EXCHANGE_SHAPES=1 to rewrite it.
const exchangeShapesPath = "testdata/exchange_shapes.json"

// MON-R47: the exchange shapes the EMF cost is measured over, each driven
// through the engine as the two reference implementations' use cases drive it
// (the payer's gateway answering each PA leg from the network, the
// provider's gateway calling the Hub for its own system), with a clock that
// moves on every read, so every stage the engine enters takes time, and every
// metric hook recorded. Each names the test that drives the same exchange and
// checks its record, with that test's level where it differs. The golden is
// the shapes exactly: a change to a shape is a change to every gateway's EMF
// volume.
func TestExchangeShapes_MON_R47(t *testing.T) {
	var shapes []exchangeShape
	stepping := func(g *Gateway) {
		clock := &steppingClock{testClock{now: time.Unix(1_700_000_000, 0)}}
		g.cfg.Clock = clock.Now
		if r, ok := g.cfg.Responder.(*nativeResponder); ok {
			r.clock = clock.Now
		}
	}
	payer := func(t *testing.T, level ConformanceEnforcement) (*levelPayer, *exchangeRecords, *shapeReports) {
		p := eligibilityPayer(t, level, true)
		var got exchangeRecords
		p.g.cfg.ExchangeObserved = got.observe
		r := &shapeReports{}
		r.wire(t, p.g)
		stepping(p.g)
		return p, &got, r
	}
	// The payer's gateway, from the network, at none (the default) unless a
	// row says: each PA leg answered by the payer's own system, refused, and
	// the payer's own system failing.
	for _, c := range []struct {
		name, source, leg, operation string
		level                        ConformanceEnforcement
		setup                        func(t *testing.T, p *levelPayer)
		body                         func(t *testing.T) []byte
	}{
		{"inbound crd-order-select answered", "TestExchangeRecord_InboundAnswered (at observe)", "crd-order-select", "", EnforcementNone, nil,
			func(*testing.T) []byte { return conformantCRD("MBR-COVERED", "72148") }},
		{"inbound crd-order-select answered at observe", "TestExchangeRecord_InboundAnswered", "crd-order-select", "", EnforcementObserve, nil,
			func(*testing.T) []byte { return conformantCRD("MBR-COVERED", "72148") }},
		{"inbound dtr-questionnaire-fetch answered", "TestExchangeRecord_InboundOperationAndLine (at observe)", "dtr-questionnaire-fetch", shnsdk.FrameOperationQuestionnairePackage, EnforcementNone, nil,
			func(*testing.T) []byte { return dtrFetchReq }},
		{"inbound pas-claim answered (pended)", "TestExchangeRecord_RecipientEdits", "pas-claim", "", EnforcementNone, nil,
			func(*testing.T) []byte { return []byte(pasIngressBundle("00001", "")) }},
		{"inbound pas-claim-inquire answered", "TestExchangeRecord_LegStagesInquiryExactly (at observe)", "pas-claim-inquire", "", EnforcementNone,
			func(t *testing.T, p *levelPayer) { p.seedInquiryPend(t, "corr-submit-1") },
			func(*testing.T) []byte { return inquiryBundle("MBR-COVERED", "", "TRN-1", "72148") }},
		{"inbound coverage-eligibility answered", "TestEligibility_DeclaredForwardsAndRelaysExactly", "coverage-eligibility", "", EnforcementNone,
			func(_ *testing.T, p *levelPayer) {
				p.partner.respByPath[eligibilityPath] = payersEligibilityAnswer("Patient/" + dtrFrameMember)
			},
			func(t *testing.T) []byte { return eligibilityRequest(t, dtrFrameMember) }},
		{"inbound crd-order-select refused by the payer's gateway at strict (conformance)", "TestExchangeRecord_InboundConformance", "crd-order-select", "", EnforcementStrict, nil,
			func(t *testing.T) []byte {
				return crdSelectWith(t, `{"fullUrl":"urn:uuid:sr1","resource":{"resourceType":"ServiceRequest","id":"sr1","status":"draft","intent":"order","subject":{"reference":"Patient/MBR-COVERED"},"code":{"coding":[{"system":"http://www.ama-assn.org/go/cpt","code":"72148"}]}}}`, ``)
			}},
		{"inbound crd-order-select, the payer's own system answers http-5xx", "TestExchangeRecord_InboundBackendFailures (at observe)", "crd-order-select", "", EnforcementNone,
			func(_ *testing.T, p *levelPayer) { p.partner.status = http.StatusServiceUnavailable },
			func(*testing.T) []byte { return conformantCRD("MBR-COVERED", "72148") }},
		// No test captures a 4xx from the payer's own system: it is driven
		// as the 5xx is.
		{"inbound crd-order-select, the payer's own system answers http-4xx", "none (driven as TestExchangeRecord_InboundBackendFailures's http-5xx)", "crd-order-select", "", EnforcementNone,
			func(_ *testing.T, p *levelPayer) { p.partner.status = http.StatusNotFound },
			func(*testing.T) []byte { return conformantCRD("MBR-COVERED", "72148") }},
		{"inbound crd-order-select, the payer's own system times out", "TestBackendDeadline_SlowSystemIsItsOwnTimeout", "crd-order-select", "", EnforcementNone,
			func(_ *testing.T, p *levelPayer) {
				WithBackendDeadline(50 * time.Millisecond)(p.g.cfg.Responder.(*nativeResponder))
				p.partner.delay = time.Second
			},
			func(*testing.T) []byte { return conformantCRD("MBR-COVERED", "72148") }},
	} {
		p, got, r := payer(t, c.level)
		if c.setup != nil {
			c.setup(t, p)
		}
		p.send(t, c.leg, c.operation, c.body(t))
		// An observe check reports from its worker: flush it.
		_ = p.g.Close()
		shapes = append(shapes, shapeOf(c.name, c.source, c.level, got.only(t), r))
	}
	{
		p, got, r := payer(t, EnforcementNone)
		w := httptest.NewRecorder()
		p.g.inboundRoute()(w, inboundRequest(t, p, "crd-order-select", func(_ *levelPayer, e *shnsdk.Envelope) { e.Metadata.Recipient = "someone-else" }, nil))
		shapes = append(shapes, shapeOf("inbound crd-order-select refused by the payer's gateway before dispatch (routing)", "TestExchangeRecord_InboundRefusalRules (at observe)", EnforcementNone, got.only(t), r))
	}
	// The provider's gateway, for its own system, at none: answered, refused
	// before the Hub, refused by the Hub, and the recipient failing. An
	// ingress call's lines do not depend on its leg.
	provider := func(t *testing.T) (*inProcessExchange, *exchangeRecords, *shapeReports) {
		env := newInProcessExchange(t)
		env.originator.cfg.ConformanceEnforcement = EnforcementNone
		var got exchangeRecords
		env.originator.cfg.ExchangeObserved = got.observe
		r := &shapeReports{}
		r.wire(t, env.originator)
		stepping(env.originator)
		return env, &got, r
	}
	level := func(env *inProcessExchange) ConformanceEnforcement { return env.originator.conformance().Level() }
	{
		env, got, r := provider(t)
		env.payerReturns(LegResult{})
		env.originator.ingressRoute(RouteCRD)(httptest.NewRecorder(), env.crdIngressRequest(t))
		shapes = append(shapes, shapeOf("ingress crd-order-select answered", "TestExchangeRecord_IngressAnsweredAndUpstreamError (at strict)", level(env), got.only(t), r))
	}
	{
		env, got, r := provider(t)
		env.originator.ingressRoute(RoutePAS)(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/Claim/$submit", strings.NewReader(pasIngressBundle("99999", ""))))
		shapes = append(shapes, shapeOf("ingress pas-claim refused by the provider's gateway (routing)", "TestExchangeRecord_IngressRoutingRefusal (at strict)", level(env), got.only(t), r))
	}
	{
		// No client credentials: refused before the body is read.
		var got exchangeRecords
		g := &Gateway{cfg: Config{ExchangeObserved: got.observe, HolderID: "provider", ConformanceEnforcement: EnforcementNone}}
		r := &shapeReports{}
		r.wire(t, g)
		stepping(g)
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
		req.SetPathValue("id", "shn-order-sign")
		g.ingressRoute(RouteCRD)(httptest.NewRecorder(), req)
		shapes = append(shapes, shapeOf("ingress crd-order-select refused by the provider's gateway (authentication)", "TestExchangeRecord_IngressAuthenticationRefusal (at strict)", g.conformance().Level(), got.only(t), r))
	}
	for _, c := range []struct {
		name string
		hub  hubAnswers
	}{
		{"ingress crd-order-select refused by the Hub (replay)", hubAnswers{status: 409, reason: "replay detected", delivered: "no"}},
		{"ingress crd-order-select, the recipient answers 5xx", hubAnswers{status: 502, reason: "forward to recipient failed: the recipient answered 500", delivered: "unknown"}},
	} {
		env, got, r := provider(t)
		c.hub.next = env.substrate
		env.originator.cfg.Client = &http.Client{Transport: c.hub}
		env.originator.ingressRoute(RouteCRD)(httptest.NewRecorder(), env.crdIngressRequest(t))
		shapes = append(shapes, shapeOf(c.name, "TestExchangeRecord_IngressHubOutcomes (at strict)", level(env), got.only(t), r))
	}

	// Every source a shape names is a test of this package.
	var src strings.Builder
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f.Name(), "_test.go") {
			b, err := os.ReadFile(f.Name())
			if err != nil {
				t.Fatal(err)
			}
			src.Write(b)
		}
	}
	for _, sh := range shapes {
		name := strings.TrimPrefix(sh.Source, "none (driven as ")
		name, _, _ = strings.Cut(name, " ")
		name = strings.TrimSuffix(name, "'s")
		if !strings.Contains(src.String(), "\nfunc "+name+"(t *testing.T) {") {
			t.Errorf("%s names %q, which is no test of this package", sh.Name, name)
		}
	}

	got, err := json.MarshalIndent(shapes, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if os.Getenv("SHN_UPDATE_EXCHANGE_SHAPES") == "1" {
		if err := os.WriteFile(exchangeShapesPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(exchangeShapesPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the exchange shapes differ from %s (SHN_UPDATE_EXCHANGE_SHAPES=1 rewrites it, and the EMF cost with it):\n%s", exchangeShapesPath, fmt.Sprint(string(got)))
	}
}
