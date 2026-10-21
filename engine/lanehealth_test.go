package engine

import (
	"context"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// What a test lane does with each request.
const (
	testLaneAnswers int32 = iota // a 200 OperationOutcome with no issues
	testLaneDrops                // closes the connection without answering
	testLaneFails                // answers 503
	testLaneHangs                // answers nothing until the caller gives up
)

// testLane is an explicitly configured validator lane, answering as mode says.
func testLane(t *testing.T, mode *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch atomic.LoadInt32(mode) {
		case testLaneDrops:
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
		case testLaneFails:
			w.WriteHeader(http.StatusServiceUnavailable)
		case testLaneHangs:
			_, _ = io.Copy(io.Discard, r.Body)
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
		default:
			w.Header().Set("Content-Type", "application/fhir+json")
			_, _ = w.Write([]byte(`{"resourceType":"OperationOutcome","issue":[]}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A lane call's error says the lane cannot be reached only when the
// connection failed; a timeout says only that it did not answer in time, and
// an answer, of any status, or a caller that gave up says nothing about it.
func TestLaneFailureOf(t *testing.T) {
	var mode int32
	lane := testLane(t, &mode)
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	call := func(base string, timeout time.Duration, ctx context.Context) error {
		v := shnsdk.NewOperationValidator(base)
		v.Client = &http.Client{Timeout: timeout}
		_, err := v.Validate(ctx, []byte(`{"resourceType":"Bundle"}`), "")
		return err
	}
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	for name, tc := range map[string]struct {
		mode int32
		base string
		ctx  context.Context
		want laneFailure
	}{
		"refused":              {base: closed.URL, want: laneUnreachable},
		"dropped":              {mode: testLaneDrops, want: laneUnreachable},
		"timed out":            {mode: testLaneHangs, want: laneTimedOut},
		"answered 503":         {mode: testLaneFails, want: laneAnswered},
		"answered":             {mode: testLaneAnswers, want: laneAnswered},
		"the caller gave up":   {mode: testLaneHangs, ctx: gone, want: laneAnswered},
		"refused, caller gone": {base: closed.URL, ctx: gone, want: laneAnswered},
	} {
		t.Run(name, func(t *testing.T) {
			atomic.StoreInt32(&mode, tc.mode)
			base, ctx := tc.base, tc.ctx
			if base == "" {
				base = lane.URL
			}
			if ctx == nil {
				ctx = context.Background()
			}
			err := call(base, 200*time.Millisecond, ctx)
			if got := laneFailureOf(ctx, err); got != tc.want {
				t.Fatalf("laneFailureOf(%v) = %d, want %d", err, got, tc.want)
			}
		})
	}
	if laneFailureOf(context.Background(), nil) != laneAnswered {
		t.Fatal("no error is not a failure")
	}
}

// probeClock drives an explicit lane's schedule and its metadata checks by
// hand: each wait is a channel the row fires, and each probe answers what the
// row says.
type probeClock struct {
	mu     sync.Mutex
	waits  chan time.Duration
	fire   chan time.Time
	probes atomic.Int32
	answer atomic.Pointer[error]
	at     time.Time
}

func onProbeClock(l *explicitLane) *probeClock {
	c := &probeClock{waits: make(chan time.Duration, 16), fire: make(chan time.Time), at: laneTestEpoch}
	c.reply(nil)
	l.now = func() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
	l.after = func(d time.Duration) <-chan time.Time { c.waits <- d; return c.fire }
	l.probe = func(ctx context.Context) error {
		c.probes.Add(1)
		return *c.answer.Load()
	}
	return c
}

func (c *probeClock) reply(err error) { c.answer.Store(&err) }

func (c *probeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// next waits for the lane's schedule to ask for a wait and returns it.
func (c *probeClock) next(t *testing.T) time.Duration {
	t.Helper()
	select {
	case d := <-c.waits:
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("the lane never scheduled its next check")
		return 0
	}
}

// eventually fails the row unless cond holds within a few seconds: the
// lane's checks run on their own goroutines.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal(what)
}

// An explicit lane starts reachable. A failure at the connection makes it
// unreachable at once; while it is, its metadata is asked for every 30 s (5
// min after the first hour), and the first answer makes it reachable again.
// An answer of any status changes nothing.
func TestExplicitLaneIsUnreachableFromItsFailureUntilItAnswers(t *testing.T) {
	var mode int32
	srv := testLane(t, &mode)
	l := newExplicitLane([]string{"2.2"}, shnsdk.NewOperationValidator(srv.URL))
	defer l.close()
	clock := onProbeClock(l)
	validate := func() error {
		_, err := l.Validate(context.Background(), []byte(`{"resourceType":"Bundle"}`), "")
		return err
	}
	if !l.laneReachable() {
		t.Fatal("a lane that has not failed is unreachable")
	}
	atomic.StoreInt32(&mode, testLaneFails)
	if validate() == nil || !l.laneReachable() {
		t.Fatal("a lane that answered 503 must stay reachable")
	}
	atomic.StoreInt32(&mode, testLaneDrops)
	if validate() == nil || l.laneReachable() {
		t.Fatal("a lane that dropped the connection must be unreachable")
	}
	if d := clock.next(t); d != explicitLaneInterval {
		t.Fatalf("first check after %s, want %s", d, explicitLaneInterval)
	}
	clock.reply(errors.New("connection refused"))
	clock.fire <- laneTestEpoch
	if d := clock.next(t); d != explicitLaneInterval || clock.probes.Load() != 1 || l.laneReachable() {
		t.Fatalf("after a failed check: next in %s, %d probes, reachable %v", d, clock.probes.Load(), l.laneReachable())
	}
	clock.advance(explicitLaneEager)
	clock.fire <- laneTestEpoch
	if d := clock.next(t); d != explicitLaneIdle {
		t.Fatalf("after the first hour: next in %s, want %s", d, explicitLaneIdle)
	}
	clock.reply(nil)
	clock.fire <- laneTestEpoch
	eventually(t, "the lane that answered its metadata stayed unreachable", l.laneReachable)
	atomic.StoreInt32(&mode, testLaneAnswers)
	if validate() != nil || !l.laneReachable() {
		t.Fatal("the recovered lane does not validate")
	}
}

// A lane that did not answer in time may only be busy: it is asked for its
// metadata once, and stays reachable when it answers; when it does not, it is
// unreachable. A second timeout inside 30 s asks nothing more.
func TestExplicitLaneThatTimedOutIsCheckedNotStopped(t *testing.T) {
	var mode int32
	srv := testLane(t, &mode)
	op := shnsdk.NewOperationValidator(srv.URL)
	op.Client = &http.Client{Timeout: 100 * time.Millisecond}
	l := newExplicitLane([]string{"2.2"}, op)
	defer l.close()
	clock := onProbeClock(l)
	atomic.StoreInt32(&mode, testLaneHangs)
	timeout := func() {
		t.Helper()
		if _, err := l.Validate(context.Background(), []byte(`{"resourceType":"Bundle"}`), ""); err == nil {
			t.Fatal("a hung lane answered")
		}
	}
	timeout()
	eventually(t, "a timeout was never checked", func() bool { return clock.probes.Load() == 1 })
	eventually(t, "the check is still running", func() bool { l.mu.Lock(); defer l.mu.Unlock(); return !l.checking })
	if !l.laneReachable() {
		t.Fatal("a lane that timed out and then answered its metadata is unreachable")
	}
	timeout()
	eventually(t, "the check is still running", func() bool { l.mu.Lock(); defer l.mu.Unlock(); return !l.checking })
	if clock.probes.Load() != 1 || !l.laneReachable() {
		t.Fatalf("a second timeout inside %s: %d probes", explicitLaneInterval, clock.probes.Load())
	}
	clock.advance(explicitLaneInterval)
	clock.reply(errors.New("i/o timeout"))
	timeout()
	eventually(t, "a lane that timed out and did not answer its metadata stayed reachable", func() bool { return !l.laneReachable() })
	if clock.probes.Load() != 2 {
		t.Fatalf("%d probes, want 2", clock.probes.Load())
	}
}

// The probe asks the lane's own metadata, on a connection of its own, and
// counts any answer.
func TestExplicitLaneProbeCountsAnyAnswer(t *testing.T) {
	var mode int32
	srv := testLane(t, &mode)
	l := newExplicitLane([]string{"2.2"}, shnsdk.NewOperationValidator(srv.URL))
	defer l.close()
	atomic.StoreInt32(&mode, testLaneFails)
	if err := l.probe(context.Background()); err != nil {
		t.Fatalf("a 503 is an answer: %v", err)
	}
	atomic.StoreInt32(&mode, testLaneDrops)
	if err := l.probe(context.Background()); err == nil {
		t.Fatal("a dropped connection is not an answer")
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	if err := newExplicitLane([]string{"2.2"}, shnsdk.NewOperationValidator(closed.URL)).probe(context.Background()); err == nil {
		t.Fatal("a refused connection is not an answer")
	}
	var opErr *net.OpError
	l2 := newExplicitLane([]string{"2.2"}, shnsdk.NewOperationValidator(closed.URL))
	if err := l2.probe(context.Background()); !errors.As(err, &opErr) {
		t.Fatalf("refused: %v, want the connection's error", err)
	}
}

// laneTestEpoch is where an explicit lane's probe clock starts.
var laneTestEpoch = time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)

// reachGateway is a 2.1 build reaching a 2.2-only payer by module 2's chain
// (arm (2) is narrowed to 2.1), its 2.2 lane an explicit one to a test lane.
func reachGateway(t *testing.T) (*Gateway, *explicitLane, *probeClock, *int32) {
	t.Helper()
	g := bridgeGateway([]string{"2.1"}, false)
	mode := new(int32)
	lane := newExplicitLane([]string{"2.2"}, shnsdk.NewOperationValidator(testLane(t, mode).URL))
	t.Cleanup(lane.close)
	clock := onProbeClock(lane)
	fake := shnsdk.NewFakeValidator()
	g.cfg.ValidatorsByContractLine = contractLanes(map[string]shnsdk.Validator{"2.0": fake, "2.1": fake, "2.2": observingValidator{inner: lane, g: g}})
	return g, lane, clock, mode
}

// loseLane makes lane unreachable the way traffic does: a $validate whose
// connection the lane drops.
func loseLane(t *testing.T, lane *explicitLane, mode *int32) {
	t.Helper()
	atomic.StoreInt32(mode, testLaneDrops)
	if _, err := lane.Validate(context.Background(), []byte(`{"resourceType":"Bundle"}`), ""); err == nil || lane.laneReachable() {
		t.Fatalf("fixture: the dropped lane: %v, reachable %v", err, lane.laneReachable())
	}
	atomic.StoreInt32(mode, testLaneAnswers)
}

// With module 2 on, a chain is planned only to a lane that can be reached:
// while the 2.2 lane cannot, the claim is carried at this gateway's own 2.1
// with no chain, fresh and at resume, or refused as routing under
// REQUIRE_SHARED_LINE, naming the lane; once the lane answers again, the
// chain is planned again.
func TestModuleTwoOn_AChainToAnUnreachableLaneIsNotPlanned(t *testing.T) {
	g, lane, clock, mode := reachGateway(t)
	route, err := g.selectLegRoute("payer-22", "pas-claim", "")
	if err != nil || route.Token != "pa.pas@2.2" || route.BuildLine != "2.1" || len(route.Chain) == 0 {
		t.Fatalf("control: route %+v, %v; want the chain from 2.1 to 2.2", route, err)
	}
	loseLane(t, lane, mode)
	route, err = g.selectLegRoute("payer-22", "pas-claim", "")
	if err != nil || route.Token != "pa.pas@2.1" || route.Chain != nil {
		t.Fatalf("lane unreachable: route %+v, %v; want carried at 2.1 with no chain", route, err)
	}
	resumed, err := g.selectResumeRoute("pa.pas@2.2", "payer-22", "pas-claim-update", "")
	if err != nil || resumed.Token != "pa.pas@2.1" || resumed.Chain != nil || resumed.Pinned != "pa.pas@2.2" {
		t.Fatalf("resume: route %+v, %v; want carried at 2.1, naming the pin, with no chain", resumed, err)
	}
	g.cfg.RequireSharedLine = true
	var rre *RouteRefusalError
	if _, err := g.selectLegRoute("payer-22", "pas-claim", ""); !errors.As(err, &rre) || rre.BridgeIssue != "no certified translation to line 2.2 (its validator lane cannot be reached)" {
		t.Fatalf("under REQUIRE_SHARED_LINE: %v, want the routing refusal naming the lane", err)
	}
	if _, err := g.selectResumeRoute("pa.pas@2.2", "payer-22", "pas-claim-update", ""); !errors.As(err, &rre) {
		t.Fatalf("resume under REQUIRE_SHARED_LINE: %v, want the routing refusal", err)
	}
	g.cfg.RequireSharedLine = false
	clock.next(t)
	clock.fire <- laneTestEpoch // the lane answers its metadata
	eventually(t, "the lane that answered stayed unreachable", lane.laneReachable)
	if route, err = g.selectLegRoute("payer-22", "pas-claim", ""); err != nil || route.Token != "pa.pas@2.2" || len(route.Chain) == 0 {
		t.Fatalf("recovered: route %+v, %v; want the chain planned again", route, err)
	}
}

// With module 2 off, New leaves every lane as configured, and an explicit lane
// says nothing; with it on, each explicitly configured lane is one explicit
// lane for every contract at its line.
func TestNewGivesExplicitLanesReachabilityOnlyWithModuleTwo(t *testing.T) {
	var mode int32
	base := newInProcessExchange(t).originator.cfg
	op22 := shnsdk.NewOperationValidator(testLane(t, &mode).URL)
	fake := shnsdk.NewFakeValidator()
	lanes := contractLanes(map[string]shnsdk.Validator{"2.1": fake, "2.2": op22})
	for _, on := range []bool{false, true} {
		cfg := base
		cfg.TranslateIGLines = on
		cfg.ValidatorsByContractLine = lanes
		g := mustNew(t, cfg)
		var seen *explicitLane
		for key, v := range g.cfg.ValidatorsByContractLine {
			_, line, _ := strings.Cut(key, "@")
			l, explicit := v.(*explicitLane)
			switch {
			case !on && v != lanes[key]:
				t.Fatalf("module 2 off: %s = %T, want the lane as configured", key, v)
			case on && line == "2.2" && (!explicit || l.inner != op22 || (seen != nil && l != seen)):
				t.Fatalf("module 2 on: %s = %T, want the one explicit lane over the configured 2.2 lane", key, v)
			case on && line != "2.2" && v != lanes[key]:
				t.Fatalf("module 2 on: %s = %T, want the test double as configured", key, v)
			}
			if explicit {
				seen = l
			}
		}
		if on != (len(g.explicitLanes) == 1) {
			t.Fatalf("module 2 on=%v: %d explicit lanes", on, len(g.explicitLanes))
		}
		if lanes["pa.pas@2.2"] != op22 {
			t.Fatal("New changed the caller's lane map")
		}
		if err := g.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// submitAcrossLines is the real submit path from a 2.1 build to a 2.2-only
// payer that approves what it receives, at level, its 2.2 lane an explicit
// one answering as mode says. It returns the outcome, the events by kind and
// the Hub hits.
func submitAcrossLines(t *testing.T, level ConformanceEnforcement, require bool, prepare func(lane *explicitLane, mode *int32)) (int, string, error, map[string]int, int, *explicitLane) {
	t.Helper()
	env := newInProcessExchange(t)
	declareRecipientVersions(t, env, []string{"pa.pas@2.2"})
	cfg := &env.originator.cfg
	cfg.DeclaredContractVersions = []string{"pa.pas@2.1"}
	cfg.EgressNativeLines = []string{"2.1"}
	cfg.TranslateIGLines = true
	cfg.RequireSharedLine = require
	cfg.ConformanceEnforcement = level
	mode := new(int32)
	lane := newExplicitLane([]string{"2.2"}, shnsdk.NewOperationValidator(testLane(t, mode).URL))
	t.Cleanup(lane.close)
	onProbeClock(lane)
	cfg.ValidatorsByContractLine = contractLanes(map[string]shnsdk.Validator{
		"2.0": shnsdk.NewFakeValidator(), "2.1": shnsdk.NewFakeValidator(), "2.2": lane,
	})
	var mu sync.Mutex
	kinds := map[string]int{}
	cfg.Observer = func(e ObserverEvent) { mu.Lock(); kinds[e.Kind]++; mu.Unlock() }
	env.payerReturns(LegResult{Response: testResponse(pasGolden(t, "claimresponse-approved.json"))})
	prepare(lane, mode)
	const (
		member      = "MBR-COVERED"
		patientRef  = "Patient/" + member
		coverageRef = "Coverage/" + member
	)
	order := pasTailServiceRequest()
	qr := []byte(`{"resourceType":"QuestionnaireResponse","id":"qr-t4","status":"completed","subject":{"reference":"` + patientRef + `"}}`)
	orderRef, _ := resourceRef(order)
	source := newRawDTRBuildSource(qr, nil, shnsdk.QRContext{PatientRef: patientRef, CoverageRef: coverageRef, OrderRef: orderRef})
	_, status, msg, err := env.originator.submitClaimAndFollow(env.ctx, env.req, pasFollowInputs{
		pci: "pci-1", orderJSON: order, source: source, patientRef: patientRef, memberSystem: shnsdk.MemberSystem,
		coverageRef: coverageRef, coverage: testMemberCoverage(member), insurer: testPayerOrganization(shnsdk.CMSPayerIdentity),
		member: member, payer: shnsdk.CMSPayerIdentity, recipient: env.payerID,
	})
	mu.Lock()
	defer mu.Unlock()
	return status, msg, err, maps.Clone(kinds), env.routeHitCount(), lane
}

// A claim whose chain would be certified on a lane that cannot be reached is
// carried at this gateway's own line and the carry recorded, at every level;
// under REQUIRE_SHARED_LINE it is refused as routing before anything is sent.
// The control, with the lane answering, walks the chain.
func TestAnUnreachableLane_TheSubmissionIsCarried(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			status, msg, err, kinds, hits, _ := submitAcrossLines(t, level, false, func(*explicitLane, *int32) {})
			if err != nil || status != 0 || hits != 1 || kinds[legTransformedKind] == 0 {
				t.Fatalf("control: %d %q %v, %d Hub hits, events %v; want the chain walked and sent", status, msg, err, hits, kinds)
			}
			status, msg, err, kinds, hits, _ = submitAcrossLines(t, level, false, func(l *explicitLane, mode *int32) { loseLane(t, l, mode) })
			if err != nil || status != 0 || hits != 1 || kinds[LineCarriedEvent] == 0 || kinds[legTransformedKind] != 0 {
				t.Fatalf("lane unreachable: %d %q %v, %d Hub hits, events %v; want the claim carried at 2.1 and the carry recorded", status, msg, err, hits, kinds)
			}
			_, _, err, _, hits, _ = submitAcrossLines(t, level, true, func(l *explicitLane, mode *int32) { loseLane(t, l, mode) })
			var rre *RouteRefusalError
			if !errors.As(err, &rre) || hits != 0 {
				t.Fatalf("REQUIRE_SHARED_LINE: %v, %d Hub hits; want the routing refusal before anything is sent", err, hits)
			}
		})
	}
}

// A lane lost after the chain was planned, between the walk and the bridged
// check, refuses the claim as SHN's own edit it could not certify, at every
// level, and nothing is sent: the exchange that finds the lane down. The lane
// is then unreachable, so later claims are carried
// (TestAnUnreachableLane_TheSubmissionIsCarried).
func TestALaneLostAfterTheWalk_TheExchangeThatFindsItIsRefused(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			status, msg, _, kinds, hits, lane := submitAcrossLines(t, level, false, func(_ *explicitLane, mode *int32) { atomic.StoreInt32(mode, testLaneDrops) })
			if status != http.StatusInternalServerError || hits != 0 || kinds[legTransformedKind] != 1 {
				t.Fatalf("lane lost after the walk: %d %q, %d Hub hits, events %v; want the walk, then 500 with nothing sent", status, msg, hits, kinds)
			}
			if lane.laneReachable() {
				t.Fatal("the lane the bridged check found down is still reachable")
			}
		})
	}
}

// The lane's check dials a connection of its own: never the one its $validate
// calls keep alive, and it keeps none.
func TestExplicitLaneCheckUsesAConnectionOfItsOwn(t *testing.T) {
	var mu sync.Mutex
	from := map[string][]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		from[r.Method] = append(from[r.Method], r.RemoteAddr)
		mu.Unlock()
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/fhir+json")
		_, _ = w.Write([]byte(`{"resourceType":"OperationOutcome","issue":[]}`))
	}))
	defer srv.Close()
	l := newExplicitLane([]string{"2.2"}, shnsdk.NewOperationValidator(srv.URL))
	defer l.close()
	for range 2 {
		if _, err := l.Validate(context.Background(), []byte(`{"resourceType":"Bundle"}`), ""); err != nil {
			t.Fatal(err)
		}
		if err := l.probe(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	posts, gets := from[http.MethodPost], from[http.MethodGet]
	if len(posts) != 2 || posts[0] != posts[1] {
		t.Fatalf("fixture: $validate from %v, want one kept-alive connection", posts)
	}
	if len(gets) != 2 || gets[0] == posts[0] || gets[1] == posts[0] || gets[0] == gets[1] {
		t.Fatalf("checks from %v, $validate from %v: want a new connection for every check", gets, posts)
	}
}

// Only module 2's planning reads whether a lane can be reached: while the 2.2
// lane is marked unreachable, a governed $validate still calls it and gets its
// verdict, and arm (2) still builds natively at 2.2.
func TestAnUnreachableLaneIsStillCalledByEverythingButModuleTwo(t *testing.T) {
	g, lane, _, mode := reachGateway(t)
	loseLane(t, lane, mode)
	v := g.validatorForContractLine("pa.pas", "2.2")
	if res, err := v.Validate(context.Background(), []byte(`{"resourceType":"Bundle"}`), ""); err != nil || !res.Valid {
		t.Fatalf("a governed $validate to the unreachable lane: %+v, %v; want the lane's own verdict", res, err)
	}
	if lane.laneReachable() {
		t.Fatal("fixture: the lane's answer made it reachable")
	}
	g.cfg.EgressNativeLines = nil // arm (2) may build at 2.2
	if route, err := g.selectLegRoute("payer-22", "pas-claim", ""); err != nil || route.Token != "pa.pas@2.2" || route.BuildLine != "2.2" || route.Chain != nil {
		t.Fatalf("arm (2): route %+v, %v; want built natively at 2.2", route, err)
	}
}

// A failure reported while an unreachable lane's check runs keeps it
// unreachable, even when that check passes: the schedule goes on.
func TestExplicitLaneFailingDuringItsCheckStaysUnreachable(t *testing.T) {
	var mode int32
	l := newExplicitLane([]string{"2.2"}, shnsdk.NewOperationValidator(testLane(t, &mode).URL))
	defer l.close()
	clock := onProbeClock(l)
	loseLane(t, l, &mode)
	clock.next(t)
	l.probe = func(context.Context) error {
		l.lost() // a $validate failed at the connection meanwhile
		return nil
	}
	clock.fire <- laneTestEpoch
	if d := clock.next(t); d != explicitLaneInterval || l.laneReachable() {
		t.Fatalf("after a passing check that a failure overlapped: next in %s, reachable %v", d, l.laneReachable())
	}
}
