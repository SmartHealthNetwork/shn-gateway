package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A payer's own system slower than the responder's deadline is its timeout:
// the requester is answered this gateway's own framed 504, the network's
// refusal on its own primitives, never anything the payer's system said, and
// the exchange is recorded as the payer system's timeout. At every level, for
// every forwarded leg.
func TestBackendDeadline_SlowSystemIsItsOwnTimeout(t *testing.T) {
	type row struct {
		leg, operation string
		body           func(t *testing.T, p *levelPayer) []byte
	}
	rows := []row{
		{leg: "crd-order-select", body: func(*testing.T, *levelPayer) []byte { return conformantCRD("MBR-COVERED", "72148") }},
		{leg: "dtr-questionnaire-fetch", operation: shnsdk.FrameOperationQuestionnairePackage, body: func(*testing.T, *levelPayer) []byte { return dtrFetchReq }},
		{leg: "pas-claim", body: func(t *testing.T, _ *levelPayer) []byte { return originatorBuiltConformantBundle(t, "MBR-COVERED") }},
		{leg: "pas-claim-inquire", body: func(*testing.T, *levelPayer) []byte { return inquiryBundle("MBR-COVERED", "", "TRN-1", "72148") }},
		{leg: "coverage-eligibility", body: func(t *testing.T, _ *levelPayer) []byte { return eligibilityRequest(t, dtrFrameMember) }},
	}
	for _, level := range allLevels {
		for _, r := range rows {
			t.Run(fmt.Sprintf("%s/%s", r.leg, level), func(t *testing.T) {
				p := eligibilityPayer(t, level, true)
				WithBackendDeadline(250 * time.Millisecond)(p.g.cfg.Responder.(*nativeResponder))
				var got exchangeRecords
				p.g.cfg.ExchangeObserved = got.observe
				body := r.body(t, p)
				p.partner.delay = 2 * time.Second
				for path := range p.partner.respByPath {
					p.partner.respByPath[path] = []byte(`{"payer":"answer that must never reach the requester"}`)
				}
				start := time.Now()
				ans := p.send(t, r.leg, r.operation, body)
				if waited := time.Since(start); waited > time.Second {
					t.Fatalf("answered after %s; the deadline did not bound the call", waited)
				}
				if ans.status != http.StatusGatewayTimeout || !ans.framed {
					t.Fatalf("answer %d (framed %v) %s, want this gateway's framed 504", ans.status, ans.framed, ans.body)
				}
				var refusal struct {
					Error string `json:"error"`
				}
				if err := json.Unmarshal(ans.body, &refusal); err != nil || refusal.Error != errUpstreamTimedOut {
					t.Fatalf("answer body %s, want the gateway's own refusal %q", ans.body, errUpstreamTimedOut)
				}
				if bytes.Contains(ans.body, []byte("payer")) && !bytes.Contains(ans.body, []byte("payer's system")) {
					t.Fatalf("the refusal carries the payer's bytes: %s", ans.body)
				}
				rec := got.only(t)
				if rec.Outcome != ExchangeUpstreamError || rec.Backend == nil || rec.Backend.ErrorClass != BackendTimeout {
					t.Fatalf("record %s backend %+v, want the payer system's timeout", rec.Outcome, rec.Backend)
				}
			})
		}
	}
}

// Within its deadline the payer's system answers as ever: the deadline
// changes nothing about an answer in time.
func TestBackendDeadline_AnswerInTimeIsRelayed(t *testing.T) {
	p := newLevelPayer(t, EnforcementObserve)
	WithBackendDeadline(5 * time.Second)(p.g.cfg.Responder.(*nativeResponder))
	p.partner.delay = 20 * time.Millisecond
	ans := p.send(t, "crd-order-select", "", conformantCRD("MBR-COVERED", "72148"))
	if want := p.partner.respByPath[crdSelectPath]; ans.status != http.StatusOK || !bytes.Equal(ans.body, want) {
		t.Fatalf("answer %d %s, want the payer's answer relayed exactly", ans.status, ans.body)
	}
}

// With no deadline of its own, a slow call is bounded only by its client and
// the request, as before.
func TestBackendDeadline_NoneOfItsOwn(t *testing.T) {
	n := NewNativeResponder(nil, "http://payer.test", "", nil, fixedClock, WithBackendDeadline(0))
	if n.backendDeadline != 0 {
		t.Fatalf("deadline %s", n.backendDeadline)
	}
}

// A timeout is answered 504, and says whether the payer's system may have
// acted on the request; any other failure keeps its 502.
func TestResponderFailure_TimeoutIsA504(t *testing.T) {
	g := &Gateway{}
	for name, row := range map[string]struct {
		err    error
		status int
		msg    string
	}{
		"timed out, written":     {&upstreamFailure{err: errors.New("x"), sent: true, timedOut: true}, http.StatusGatewayTimeout, errUpstreamTimedOut},
		"timed out, not written": {&upstreamFailure{err: errors.New("x"), timedOut: true}, http.StatusGatewayTimeout, errUpstreamNotReachedInTime},
		"no usable answer":       {&upstreamFailure{err: errors.New("x"), sent: true}, http.StatusBadGateway, errUpstreamNoUsableAnswer},
		"not reached":            {&upstreamFailure{err: errors.New("x")}, http.StatusBadGateway, errUpstreamNotReached},
	} {
		if status, msg := g.responderFailure("crd-order-select", row.err); status != row.status || msg != row.msg {
			t.Errorf("%s: %d %q, want %d %q", name, status, msg, row.status, row.msg)
		}
	}
}

// forwardOnce posts one CRD hook through n inside a recorded inbound exchange
// whose leg arrived at arrived, and returns the forward's error and record.
func forwardOnce(t *testing.T, n *nativeResponder, ctx context.Context, arrived time.Time) (error, ExchangeRecord) {
	t.Helper()
	var got exchangeRecords
	var fwdErr error
	g := &Gateway{cfg: Config{ExchangeObserved: got.observe, Role: "payer"}}
	g.recordExchange(RouteSubstrateInbound, DirectionInbound, func(w http.ResponseWriter, r *http.Request) {
		_, _, fwdErr = n.post(withLegArrival(r.Context(), arrived), n.baseURL, "/cds-services/shn-order-select", relay.Exact(relay.NewBody([]byte(`{}`), relay.OriginPeerFrame), "application/json"), "crd-order-select", "crd")
		exchangeOf(r.Context()).decided(ExchangeUpstreamError, "", "")
		w.WriteHeader(http.StatusGatewayTimeout)
	})(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx))
	return fwdErr, got.only(t)
}

// slowSystem answers each operation after delay, reading the request first.
func slowSystem(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-time.After(delay):
			_, _ = w.Write([]byte(`{"cards":[]}`))
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The deadline counts from the leg's arrival, so the work before the call
// (the member's lookup, a listing read, a token) uses it up too: a call that
// starts late still times out while the requester is waiting.
func TestBackendDeadline_CountsFromTheLegsArrival(t *testing.T) {
	srv := slowSystem(t, 400*time.Millisecond)
	n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", nil, fixedClock, WithBackendDeadline(500*time.Millisecond))
	start := time.Now()
	err, rec := forwardOnce(t, n, context.Background(), start.Add(-300*time.Millisecond))
	var up *upstreamFailure
	if !errors.As(err, &up) || !up.timedOut || time.Since(start) > 350*time.Millisecond {
		t.Fatalf("after %s: %v, want the deadline 500ms after an arrival 300ms ago", time.Since(start), err)
	}
	if rec.Backend == nil || rec.Backend.ErrorClass != BackendTimeout {
		t.Fatalf("backend %+v", rec.Backend)
	}
	// Counted from the call's own start, the same answer would be in time.
	if err, _ := forwardOnce(t, n, context.Background(), time.Now()); err != nil {
		t.Fatalf("a call arriving now: %v", err)
	}
}

// A token endpoint as slow as the deadline is the system's timeout, not its
// credentials: the record says timeout, and the requester reads that the
// system could not be reached in time.
func TestBackendDeadline_SlowTokenIsATimeout(t *testing.T) {
	token := slowSystem(t, 2*time.Second)
	sys := slowSystem(t, 0)
	hc, err := smartauth.NewHTTPClient(smartauth.Config{TokenURL: token.URL, ClientID: "client", ClientSecret: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	n := NewNativeResponder(hc, sys.URL, "shn-order-select", nil, fixedClock, WithBackendDeadline(200*time.Millisecond))
	err, rec := forwardOnce(t, n, context.Background(), time.Now())
	var up *upstreamFailure
	if !errors.As(err, &up) || !up.timedOut || up.sent {
		t.Fatalf("%v, want a timeout before the request was sent", err)
	}
	if status, msg := (&Gateway{}).responderFailure("crd-order-select", err); status != http.StatusGatewayTimeout || msg != errUpstreamNotReachedInTime {
		t.Fatalf("answered %d %q", status, msg)
	}
	if rec.Backend == nil || rec.Backend.ErrorClass != BackendTimeout {
		t.Fatalf("backend %+v, want timeout, not auth", rec.Backend)
	}
}

// With a deadline set, a requester that leaves first is still the call cut
// short: cancelled, and the exchange is not the system's error.
func TestBackendDeadline_RequesterLeavingFirstIsCancelled(t *testing.T) {
	srv := slowSystem(t, 2*time.Second)
	n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", nil, fixedClock, WithBackendDeadline(10*time.Second))
	// The requester goes away: its request is cancelled, as net/http does on
	// a disconnect.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(100*time.Millisecond, cancel)
	err, rec := forwardOnce(t, n, ctx, time.Now())
	var up *upstreamFailure
	if !errors.As(err, &up) || up.timedOut {
		t.Fatalf("%v, want the call cut short, not a timeout", err)
	}
	if rec.Backend == nil || rec.Backend.ErrorClass != BackendCancelled || rec.Outcome != ExchangeOther {
		t.Fatalf("record %s backend %+v", rec.Outcome, rec.Backend)
	}
}

// When the gateway's own work before the call (validation, the member's
// lookup) used the whole deadline, the payer's system is not asked: nothing
// is sent, no backend call is recorded (so BackendError does not count it),
// and the requester reads that the payer's gateway ran out of time, never
// that the payer's system did.
func TestBackendDeadline_SpentBeforeTheCallIsNotThePayers(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"cards":[]}`))
	}))
	t.Cleanup(srv.Close)
	n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", nil, fixedClock, WithBackendDeadline(500*time.Millisecond))
	err, rec := forwardOnce(t, n, context.Background(), time.Now().Add(-time.Second))
	var spent *deadlineSpent
	var up *upstreamFailure
	if !errors.As(err, &spent) || errors.As(err, &up) {
		t.Fatalf("%v, want the gateway's own spent deadline, not an upstream failure", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("the payer's system was called %d times", calls.Load())
	}
	if rec.Backend != nil || rec.BackendCalls != 0 {
		t.Fatalf("backend %+v in %d calls, want none", rec.Backend, rec.BackendCalls)
	}
	if status, msg := (&Gateway{}).responderFailure("crd-order-select", err); status != http.StatusGatewayTimeout || msg != errDeadlineSpent {
		t.Fatalf("answered %d %q", status, msg)
	}
}

// Through the whole inbound path: a deadline the gateway's own work used up
// (here, any work at all) leaves the payer's system unasked, and the exchange
// is the gateway's other, never the payer's upstream error.
func TestBackendDeadline_SpentDeadlineSettlesAsOther(t *testing.T) {
	p := newLevelPayer(t, EnforcementObserve)
	// A payer with no system of record makes no read of its own, so the
	// record would show any call at all.
	p.g.cfg.SoR = NoSystemOfRecord()
	WithBackendDeadline(time.Nanosecond)(p.g.cfg.Responder.(*nativeResponder))
	var got exchangeRecords
	p.g.cfg.ExchangeObserved = got.observe
	ans := p.sendAs(t, "crd-order-select", "", conformantCRD(strangerMember, "72148"), nosorTokenSubject)
	if ans.status != http.StatusGatewayTimeout || !ans.framed || !bytes.Contains(ans.body, []byte("did not receive it")) {
		t.Fatalf("answer %d (framed %v) %s", ans.status, ans.framed, ans.body)
	}
	// The CDS service listing may be read on the way; the operation is not sent.
	if p.partner.lastPath != "" {
		t.Fatalf("the operation was sent to the payer's system (%s)", p.partner.lastPath)
	}
	rec := got.only(t)
	if rec.Outcome != ExchangeOther {
		t.Fatalf("record %s backend %+v, want other", rec.Outcome, rec.Backend)
	}
	if b := rec.Backend; b != nil && (b.ErrorClass != "" || b.Status/100 != 2) {
		t.Fatalf("backend %+v: only the listing's read may be recorded, never a failed forward", b)
	}
}
