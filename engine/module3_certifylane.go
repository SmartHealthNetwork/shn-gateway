package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
	"github.com/SmartHealthNetwork/shn-gateway/internal/lanequalify"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// CertificationLaneUnavailable is the error a certification client answers
// when its line has no lane it may use yet. The verdict carries this text
// verbatim — it is authored here, never a server's bytes — so an operator
// reading the evidence sees which lane to configure, and in which state the
// default lane's qualification is.
type CertificationLaneUnavailable struct {
	Reason string
	// Called says the lane was called for the exchange and did not answer (it
	// now re-qualifies): a $validate call was made. Otherwise nothing was
	// dialed.
	Called bool
}

func (e *CertificationLaneUnavailable) Error() string {
	return "certification validator unavailable: " + e.Reason
}

// The certification client's own qualification schedule. A default lane's
// client defers to routing: while routing's lane is retrying its own
// qualification (KeepQualifying), the client posts nothing, and once routing's
// lane passes with the same endpoint, line and qualifier as the client's, that
// pass is the client's first qualification (handOff). Otherwise, as for a
// client configured by address, it runs its own attempts. A default lane's
// client first waits certificationRequalifyDelay plus a random part of the
// retry base, so the gateways a deploy restarts together do not start
// together; an address's client starts at once. Until an attempt passes, attempts repeat on the gateway's
// shared backoff (LaneBackoff: 15 s after the first failure, doubling to 5 min,
// each wait drawn from the upper half of its step), so a lane that many
// gateways qualify at once is not retried in lockstep. An attempt is bounded by
// certificationRequalifyBudget; one its budget cuts off is discarded, never
// resumed (lanequalify.Warm: a fresh validator process is the recovery
// boundary, so rows a lane answered before it restarted say nothing about the
// lane now). The corpus is only run once the validator answers its metadata.
// Until then an attempt spends its budget retrying the metadata probe (the
// qualifier's own cadence: each probe waits up to 4 s, then 1 s passes), so a
// line costs 120 s bursts of about one request a second against a name that
// never resolves or a refused connection, on the backoff above; accepted as the
// price of certifying against a validator that comes up late, and zero once it
// does. A re-qualification after a qualified lane stops answering is always the
// client's own run.
const (
	certificationRequalifyDelay = 15 * time.Second
	// certificationRequalifyBudget is routing's attempt budget too
	// (defaultLaneAttemptBudget): long enough for the corpus on a lane several
	// gateways qualify at once after a deploy, where 60 s was not. The two
	// constants are separate; handOff compares a pass's time with this one, so
	// routing's pass is handed over only if it fits this budget.
	certificationRequalifyBudget = 120 * time.Second
	// certificationRequalifyMinInterval is the least time between two
	// re-qualifications' starts after a lane stops answering, so a lane that
	// flaps costs at most one corpus run per half-minute per line, while a
	// restarted lane is certified against again within about that long of
	// coming back.
	certificationRequalifyMinInterval = 30 * time.Second
)

// certificationJitter is the random source of a certification client's retry
// waits and start spread; the engine's tests replace it.
var certificationJitter = rand.Float64

// GatedCertificationValidator certifies against a discovered default lane only
// once that lane has qualified — by routing's qualification, when it ran the
// same endpoint, line and qualifier (handOff), or by this client's own. The
// client keeps its own qualification state, independent of routing's
// DiscoveredLane, which it never changes: a lane that qualifies here late does
// not become a routing lane.
//
// Its own attempts run in a background loop from Start (the certification
// worker's start; construction starts nothing), on the schedule above, and stop at the first success from either side. No exchange waits on
// or triggers an attempt: while the lane is not ready the call answers
// CertificationLaneUnavailable at once, naming the qualification's state, and
// the first exchange after a success certifies. So a validator that comes up
// after routing's boot budget has expired is certified against within one
// retry wait of coming up (at most about 5 min), and a validator that never
// resolves costs one bounded attempt per wait, never a round trip per exchange. The client is this
// collector's own bounded connection pool.
type GatedCertificationValidator struct {
	lane    *DiscoveredLane
	client  *shnsdk.OperationValidator
	qualify LaneQualifier
	reason  string
	// pending, failing and requalifying name the qualification's state in the
	// verdict: none has finished yet, the last one failed and another will
	// follow, or the lane failed to answer after qualifying and is being
	// qualified again.
	pending, failing, requalifying string
	now                            func() time.Time

	// line is the line the client certifies, which a pass it takes from routing
	// must have qualified.
	line string

	// schedule, overridable in tests: the first attempt's delay, the retry
	// backoff (its Jitter also spreads each start), the least time between
	// re-qualifications, and the clock the loop waits on.
	delay, minRequalify, budget time.Duration
	backoff                     LaneBackoff
	after                       func(time.Duration) <-chan time.Time

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	ready    bool
	failures int
	// stale: the lane failed to answer after it qualified, so neither this
	// client's earlier qualification nor routing's counts until an attempt of
	// this client's own passes again.
	stale bool
	// looping: an attempt loop is running and has not yet decided to exit;
	// closed: Close has begun.
	looping, closed bool
	// requalified is when the last re-qualification was scheduled to start.
	requalified time.Time
	// probe asks the lane for its metadata once, on a connection of its own
	// (laneAnswers); tests replace it. checking: a check a timeout started is
	// running. checked is when a timeout last started one.
	// requalifications counts re-qualifications, so a check that a newer
	// one overtook does not re-qualify the lane again.
	probe            func(context.Context) error
	checking         bool
	checked          time.Time
	requalifications int
	// started: Start has run; nothing is attempted before it.
	started bool
	// unstartedLogged: the one certification_lane_not_started line has been
	// written.
	unstartedLogged bool
}

// CertificationNotStartedReason is the reason a gated client that was never
// started records for every certification (the verdict reads "certification
// validator unavailable: " and this): an embedder that builds one without
// engine.New must call Start, or the lane is never qualified.
const CertificationNotStartedReason = "certification validator not started (call Start)"

// NewGatedCertificationValidator gates a certification client on lane's
// qualification; reason is the configuration the verdict names while the lane
// is not ready (the state of the qualification is appended), and qualify is the
// same qualifier routing uses for a default lane. Construction starts nothing and
// dials nothing: the client's own attempts start in the background when Start
// runs (the certification worker's start). Embedders must pass a qualifier: a
// client without a qualifier never qualifies the lane itself, so once a lane
// that routing qualified stops answering, the line stays unavailable
// ("default lane re-qualifying") for the life of the client.
func NewGatedCertificationValidator(lane *DiscoveredLane, qualify LaneQualifier, reason string) *GatedCertificationValidator {
	v := newGatedCertification(lane, NewCertificationOperationValidator(lane.Base()), qualify, reason)
	v.pending, v.failing, v.requalifying = "default lane qualification pending", "default lane qualification failed, retrying", "default lane re-qualifying"
	return v
}

// CertificationClient is the client v certifies through, for the gateway to
// tag with its endpoint kind before Start (TagValidatorClient).
func (v *GatedCertificationValidator) CertificationClient() *shnsdk.OperationValidator {
	return v.client
}

// NewQualifiedCertificationValidator gates a certification client configured
// by address on the qualification a default lane passes before routing admits
// it (qualify, the same qualifier): a lane that has not passed it may still be
// warming, and what it answers then is not a verdict on the payload. The lane
// is this client's own: nothing else qualifies it, and its qualification never
// makes it a routing lane. Construction starts nothing and dials nothing; once
// Start runs, attempts start at once, on the schedule above. Until one passes,
// the verdict is "certification validator unavailable: lane not qualified;
// qualification pending" (or "…; qualification failed, retrying") and nothing
// is dialed for the exchange. With no qualifier the lane never
// qualifies.
func NewQualifiedCertificationValidator(line string, client *shnsdk.OperationValidator, qualify LaneQualifier) *GatedCertificationValidator {
	v := newGatedCertification(NewDiscoveredLane(line, client.BaseURL, nil), client, qualify, "lane not qualified")
	v.pending, v.failing, v.requalifying = "qualification pending", "qualification failed, retrying", "re-qualifying"
	v.delay = 0 // no routing attempt will qualify this lane first
	return v
}

// newGatedCertification builds a gated client on the standard schedule
// without starting its attempts.
func newGatedCertification(lane *DiscoveredLane, client *shnsdk.OperationValidator, qualify LaneQualifier, reason string) *GatedCertificationValidator {
	serviceValidator(client) // a certification client follows no redirect
	ctx, cancel := context.WithCancel(context.Background())
	return &GatedCertificationValidator{
		lane: lane, client: client, qualify: qualify, reason: reason, line: lane.Line(),
		now: time.Now, delay: certificationRequalifyDelay, minRequalify: certificationRequalifyMinInterval, budget: certificationRequalifyBudget,
		backoff: DefaultLaneBackoff(certificationJitter), after: time.After,
		ctx: ctx, cancel: cancel,
		// Built per check from the client as it is then, so the endpoint kind
		// TagValidatorClient gives it before Start reaches the check too.
		probe: func(ctx context.Context) error { return laneAnswers(ctx, ownConnectionClient(client), client.BaseURL) },
	}
}

// Start begins the client's own qualification attempts, once: the
// certification worker runs it when evidence collection starts, so a gateway
// that collects none (CONFORMANCE_ENFORCEMENT=none) never dials the lane. A
// later call does nothing.
func (v *GatedCertificationValidator) Start() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.started {
		return
	}
	v.started = true
	wait := v.delay
	if wait > 0 {
		wait += v.spread() // a default lane's client; an address's starts at once
	}
	v.startLocked(wait)
}

// spread is a random part of the retry base, added to each start of the
// attempt loop so the clients a deploy restarts together start apart.
func (v *GatedCertificationValidator) spread() time.Duration {
	if v.backoff.Jitter == nil {
		return 0
	}
	base := v.backoff.Base
	if base <= 0 {
		base = laneRetryBase
	}
	return time.Duration(v.backoff.Jitter() * float64(base))
}

// handOff reports whether routing's lane passed a qualification this client
// takes as its own: the lane is ready, and its pass posted the corpus to the
// endpoint this client dials, for this client's line, with this client's
// qualifier, within this client's own attempt budget (routing's is the same
// length today; a longer one would not hand over a slower pass). Any difference and the client qualifies the lane itself. A
// client built without a qualifier can run none of its own: for it, routing's
// pass on its endpoint and line is the only qualification there is, as its
// constructor says.
func (v *GatedCertificationValidator) handOff() bool {
	pass, ok := v.lane.passedWith()
	if !ok || pass.base != v.client.BaseURL || pass.line != v.line {
		return false
	}
	if v.qualify == nil {
		return true
	}
	return pass.qualifier == qualifierID(v.qualify) && pass.took <= v.budget
}

// startLocked starts the attempt loop, first attempt after wait, unless the
// client has not been started, the loop is running, the client is closed, or
// there is no qualifier.
func (v *GatedCertificationValidator) startLocked(wait time.Duration) {
	if !v.started || v.qualify == nil || v.looping || v.closed {
		return
	}
	v.looping = true
	v.wg.Add(1)
	go v.loop(wait)
}

// Validate certifies through the gated client once the lane is ready; until
// then it answers the lane-unavailable error immediately. A client that was
// never started (and is not closed) answers CertificationNotStartedReason
// instead, dials nothing, and logs one certification_lane_not_started line, at
// its first certification.
//
// A qualified lane that fails at the connection (laneUnreachable) may have
// restarted, and a restarted lane is warming again. That failure clears the
// qualification, routing's included for this client, and qualifies the lane
// again, at most once per minRequalify; that certification and every one
// until an attempt passes are unavailable, naming the re-qualification, and
// nothing is dialed for them. A lane that did not answer in time
// (laneTimedOut) may only be busy: that certification fails as it did, and
// the lane is asked for its metadata (timedOut), re-qualifying only if it
// does not answer. Any other failure (a 5xx, an answer that is not an
// OperationOutcome, the caller's own deadline) marks only that certification
// unavailable, and an answer that is a verdict, valid or invalid, changes
// nothing. A lane restarted between two certifications, with no failure in
// between, is not detected.
func (v *GatedCertificationValidator) Validate(ctx context.Context, body []byte, profile string) (shnsdk.Result, error) {
	v.mu.Lock()
	if !v.started && !v.closed {
		// Never started: nothing will ever qualify the lane. Say so, once in
		// the log and on every verdict, and dial nothing.
		first := !v.unstartedLogged
		v.unstartedLogged = true
		v.mu.Unlock()
		if first {
			event, _ := json.Marshal(struct {
				Version int    `json:"version"`
				Line    string `json:"line"`
				Base    string `json:"base"`
				Host    string `json:"host"`
				At      string `json:"at"`
			}{1, v.lane.line, v.lane.Base(), lanequalify.Host(v.lane.Base()), v.now().UTC().Format(time.RFC3339Nano)})
			log.Printf("gateway: certification_lane_not_started %s", event)
		}
		return shnsdk.Result{}, &CertificationLaneUnavailable{Reason: CertificationNotStartedReason}
	}
	ready := v.ready || (!v.stale && v.handOff())
	failures, stale := v.failures, v.stale
	v.mu.Unlock()
	if ready {
		res, err := v.client.Validate(ctx, body, profile)
		switch laneFailureOf(ctx, err) {
		case laneUnreachable:
			v.requalify()
			return shnsdk.Result{}, &CertificationLaneUnavailable{Reason: v.reason + "; " + v.requalifying, Called: true}
		case laneTimedOut:
			v.timedOut()
		}
		return res, err
	}
	state := v.pending
	switch {
	case failures > 0:
		state = v.failing
	case stale:
		state = v.requalifying
	case v.lane.retrying():
		state = v.deferring(v.lane.State())
	}
	return shnsdk.Result{}, &CertificationLaneUnavailable{Reason: v.reason + "; " + state}
}

// deferring names the qualification a client that defers to routing waits on:
// routing's, pending until its first attempt fails, then failed and retrying,
// with routing's attempts so far and its last failure's kind, as routing's own
// validator_qualification line names it.
func (v *GatedCertificationValidator) deferring(s LaneState) string {
	if s.Failures == 0 {
		return v.pending + " (routing qualifying)"
	}
	return fmt.Sprintf("%s (routing attempt %d: %s)", v.failing, s.Attempts, lanequalify.FailureReason(s.Err))
}

// timedOut checks a qualified lane that did not answer in time, at most once
// per minRequalify: one metadata request on a connection of its own, which a
// certification queued on the client's one connection does not wait behind.
// The lane re-qualifies unless it answers; an answer of any status leaves it
// qualified. The outcome is logged (certification_lane_timeout_check).
func (v *GatedCertificationValidator) timedOut() {
	v.mu.Lock()
	now := v.now()
	if v.closed || v.stale || v.checking || (!v.checked.IsZero() && now.Before(v.checked.Add(v.minRequalify))) {
		v.mu.Unlock()
		return
	}
	v.checking, v.checked = true, now
	generation := v.requalifications
	v.wg.Add(1)
	v.mu.Unlock()
	go func() {
		defer v.wg.Done()
		// checking clears only once the outcome is applied, so a check is
		// never seen finished before its re-qualification.
		defer func() {
			v.mu.Lock()
			v.checking = false
			v.mu.Unlock()
		}()
		started := v.now()
		err := v.probe(v.ctx)
		if v.ctx.Err() != nil {
			return // closed while checking
		}
		event, _ := json.Marshal(struct {
			Version    int     `json:"version"`
			Line       string  `json:"line"`
			Host       string  `json:"host"`
			Answered   bool    `json:"answered"`
			At         string  `json:"at"`
			DurationMS float64 `json:"duration_ms"`
		}{1, v.lane.line, lanequalify.Host(v.client.BaseURL), err == nil, v.now().UTC().Format(time.RFC3339Nano), float64(v.now().Sub(started)) / float64(time.Millisecond)})
		log.Printf("gateway: certification_lane_timeout_check %s", event)
		if err != nil {
			v.mu.Lock()
			overtaken := v.requalifications != generation
			v.mu.Unlock()
			if !overtaken {
				v.requalify()
			}
		}
	}()
}

// requalify clears the lane's qualification after it stopped answering and
// qualifies it again: at once, or when minRequalify has passed since the last
// re-qualification was scheduled. A re-qualification already pending is left
// to finish.
func (v *GatedCertificationValidator) requalify() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.stale {
		return
	}
	v.stale, v.ready, v.failures = true, false, 0
	v.requalifications++
	now := v.now()
	wait := time.Duration(0)
	if !v.requalified.IsZero() {
		if next := v.requalified.Add(v.minRequalify); next.After(now) {
			wait = next.Sub(now)
		}
	}
	wait += v.spread()
	v.requalified = now.Add(wait)
	v.startLocked(wait)
}

// loop runs the client's own attempts until the lane is ready from either
// side (only this client's own once a failure made it stale) or the client
// is closed. While routing's lane is retrying and has not passed, and the
// client's qualification is not stale, it posts nothing and checks again on
// the next wait.
//
// Every exit clears looping in the same lock hold that decides it, so a
// failure that arrives after the decision starts a new loop rather than
// finding this one still marked running.
func (v *GatedCertificationValidator) loop(wait time.Duration) {
	defer v.wg.Done()
	attempts, failures := 0, 0
	var lastReason string
	for {
		select {
		case <-v.ctx.Done():
			v.mu.Lock()
			v.looping = false
			v.mu.Unlock()
			return
		case <-v.after(wait):
		}
		v.mu.Lock()
		stale := v.stale
		routed := !stale && v.handOff()
		if routed {
			v.looping = false
		}
		v.mu.Unlock()
		if routed {
			return // routing's identical qualification passed; nothing to add
		}
		if !stale && v.lane.retrying() && !v.lane.Ready() {
			wait = v.backoff.Base + v.spread() // routing is qualifying this lane: defer to it
			continue
		}
		// Routing may have passed since the check above: its identical pass
		// spares this client a corpus run of its own.
		v.mu.Lock()
		if !v.stale && v.handOff() {
			v.looping = false
			v.mu.Unlock()
			return
		}
		v.mu.Unlock()
		attempts++
		started := v.now()
		ctx, cancel := context.WithTimeout(v.ctx, v.budget)
		// The attempt is certification's: its certificate is recorded under
		// the certify kind for the line (MON-R25).
		err := v.qualify(endpointcert.WithTag(ctx, endpointcert.Lane(endpointcert.Certify, v.lane.line)), v.lane.Base(), v.lane.line)
		cancel()
		if err == nil {
			v.mu.Lock()
			v.ready, v.stale, v.looping, v.failures = true, false, false, 0 // the loop exits on success
			v.mu.Unlock()
			v.logAttempt("ready", nil, attempts, 0, started)
			return
		}
		failures++
		v.mu.Lock()
		v.failures++
		v.mu.Unlock()
		wait = v.backoff.Wait(failures)
		if reason := lanequalify.FailureReason(err); failures == 1 || reason != lastReason {
			v.logAttempt("failed", err, attempts, wait, started)
			lastReason = reason
		}
	}
}

// logAttempt logs a change of the client's qualification state: identity,
// timing, the attempts this loop has made and the wait before the next one,
// never validator output, so a lane that stays down does not log per attempt.
func (v *GatedCertificationValidator) logAttempt(state string, err error, attempts int, next time.Duration, started time.Time) {
	event, _ := json.Marshal(struct {
		Version    int     `json:"version"`
		Line       string  `json:"line"`
		Base       string  `json:"base"`
		Host       string  `json:"host"`
		State      string  `json:"state"`
		Reason     string  `json:"reason,omitempty"`
		Attempts   int     `json:"attempts"`
		NextRetryS float64 `json:"next_retry_s,omitempty"`
		At         string  `json:"at"`
		DurationMS float64 `json:"duration_ms"`
	}{1, v.lane.line, v.lane.Base(), lanequalify.Host(v.lane.Base()), state, lanequalify.FailureReason(err), attempts, next.Seconds(), v.now().UTC().Format(time.RFC3339Nano), float64(v.now().Sub(started)) / float64(time.Millisecond)})
	log.Printf("gateway: certification_lane_qualification %s", event)
}

// Ready reports whether the client would certify now: its own qualification
// passed, or routing's did and no failure has made it stale since. Local
// state only, for the lane's reports.
func (v *GatedCertificationValidator) Ready() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.ready || (!v.stale && v.handOff())
}

// Endpoint is the lane's base URL, what the gated client dials once qualified.
func (v *GatedCertificationValidator) Endpoint() string { return v.client.BaseURL }

// Close stops the background attempts and releases the client's pool.
func (v *GatedCertificationValidator) Close() {
	v.mu.Lock()
	v.closed = true
	v.mu.Unlock()
	v.cancel()
	v.wg.Wait()
	v.CloseIdleConnections()
}

// CloseIdleConnections releases the gated client's pool.
func (v *GatedCertificationValidator) CloseIdleConnections() {
	if v.client.Client != nil {
		v.client.Client.CloseIdleConnections()
	}
}
