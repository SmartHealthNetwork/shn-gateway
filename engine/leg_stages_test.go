package engine

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net"
	"net/http"
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

// A PAS inquiry at structural spends time in every stage: its synchronous
// validation of the request and the decision's EOB, and the pend ledger's
// lookup and writes, as well as the reads, the forward and the seal. The
// stages never add up to more than the call.
func TestExchangeRecord_LegStagesValidateAndLedger(t *testing.T) {
	p, got := steppingPayer(t, EnforcementStructural)
	p.seedInquiryPend(t, "corr-submit-1")
	p.partner.respByPath[pasInquirePath] = decidedAnswer(t)
	ans := p.send(t, "pas-claim-inquire", "", inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"))
	if ans.status != http.StatusOK {
		t.Fatalf("answer %d %s", ans.status, ans.body)
	}
	rec := got.only(t)
	st := rec.Stages
	if st == nil || st.Unwrap <= 0 || st.Forward <= 0 || st.Validate <= 0 || st.Seal <= 0 || st.Ledger <= 0 || st.Write <= 0 {
		t.Fatalf("stages %+v", st)
	}
	if sum := stagesSum(st); sum > rec.Latency {
		t.Fatalf("stages sum to %v, more than the latency %v: %+v", sum, rec.Latency, *st)
	}
}

// When the answer, decided after the payer's system answered, cannot be
// authorized, the requester still gets 502 authorization failed, and the
// record and the log say why: the requester gone (cancelled), the call out
// of time (timeout), a denial (auth), or no answer (unreachable).
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
			if ans.status != http.StatusBadGateway || !strings.Contains(string(ans.body), "authorization failed") {
				t.Fatalf("answer %d %s: the Hub's answer changed", ans.status, ans.body)
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
// look an authorization up and record to record a decision.
type timedLedger struct {
	*censusSoR
	clock          *testClock
	lookup, record time.Duration
}

func (l timedLedger) LookupPended(requesterHolder string, k PendKeys, about []PendKeyRef) (PendMatch, error) {
	l.clock.Add(l.lookup)
	return l.censusSoR.LookupPended(requesterHolder, k, about)
}

func (l timedLedger) RecordDecision(subjectPCI, corrID, outcome string, decidedAt time.Time, k PendKeys, eob *EOBRecord) (PendTransition, error) {
	l.clock.Add(l.record)
	return l.censusSoR.RecordDecision(subjectPCI, corrID, outcome, decidedAt, k, eob)
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
	p.g.cfg.Store = timedLedger{censusSoR: p.store, clock: clock, lookup: lookup, record: record}
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
// an amendment names takes claim, and releasing it takes release.
type timedClaim struct {
	*censusSoR
	clock          *testClock
	claim, release time.Duration
}

func (l timedClaim) BeginClaimUpdateReason(subjectPCI, related string) (bool, PendRefusal, error) {
	l.clock.Add(l.claim)
	return l.censusSoR.BeginClaimUpdateReason(subjectPCI, related)
}

func (l timedClaim) ReleaseClaimUpdate(subjectPCI, related string) error {
	l.clock.Add(l.release)
	return l.censusSoR.ReleaseClaimUpdate(subjectPCI, related)
}

// A PAS update's ledger stage is the claim it takes on the authorization it
// amends: exactly the claim's time.
func TestExchangeRecord_LegStagesUpdateClaimExactly(t *testing.T) {
	const claim = 17 * time.Millisecond
	p, got := legStagesPayer(t, 0, 0, 0)
	clock := &testClock{now: p.g.cfg.Clock()}
	p.g.cfg.Clock = clock.Now
	n := p.g.cfg.Responder.(*nativeResponder)
	n.clock = clock.Now
	request, related := updateBundle(t)
	p.seedPend(t, related)
	n.store = timedClaim{censusSoR: p.store, clock: clock, claim: claim}
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
	const claim, release = 17 * time.Millisecond, 19 * time.Millisecond
	p, got := legStagesPayer(t, 0, 0, 0)
	clock := &testClock{now: p.g.cfg.Clock()}
	p.g.cfg.Clock = clock.Now
	n := p.g.cfg.Responder.(*nativeResponder)
	n.clock = clock.Now
	request, related := updateBundle(t)
	p.seedPend(t, related)
	n.store = timedClaim{censusSoR: p.store, clock: clock, claim: claim, release: release}
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
			for line, v := range g.cfg.ValidatorsByLine {
				g.cfg.ValidatorsByLine[line] = timedValidator{inner: v, clock: clock, d: took[line], calls: &calls}
			}
			g.cfg.Validator = g.cfg.ValidatorsByLine["2.0"]
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
	p.g.cfg.Store = timedLedger{censusSoR: p.store, clock: clock, lookup: lookup, record: record}
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
