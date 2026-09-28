package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/internal/lanequalify"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// CertificationLaneUnavailable is the error a certification client answers
// when its line has no lane it may use yet. Nothing was dialed for the
// exchange. The verdict carries this text verbatim — it is authored here, never
// a server's bytes — so an operator reading the evidence sees which lane to
// configure, and in which state the default lane's qualification is.
type CertificationLaneUnavailable struct{ Reason string }

func (e *CertificationLaneUnavailable) Error() string {
	return "certification validator unavailable: " + e.Reason
}

// The certification client's own qualification schedule. The first attempt of
// a default lane's client waits certificationRequalifyDelay so routing's
// boot-time attempt normally settles the lane first (an address's client
// starts at once); until an attempt passes, attempts repeat every
// certificationRequalifyInterval for certificationRequalifyEager after the
// loop starts, and each re-qualification starts a new loop and a new eager
// hour (a validator that is going to come up does so within that), then every
// certificationRequalifyIdle. An attempt is bounded by
// certificationRequalifyBudget. The corpus is only run once the validator
// answers its metadata. Until then an attempt spends its budget retrying the
// metadata probe (the qualifier's own cadence: each probe waits up to 4 s, then
// 1 s passes), so a line costs 60 s bursts of about one request a second
// against a name that never resolves or a refused connection (one about every
// 5 s against a validator that accepts and hangs), 15 s apart for the first
// hour and about every 6 min after it; accepted as the price of certifying
// against a validator that comes up late, and zero once it does.
const (
	certificationRequalifyDelay    = 15 * time.Second
	certificationRequalifyInterval = 15 * time.Second
	certificationRequalifyEager    = time.Hour
	certificationRequalifyIdle     = 5 * time.Minute
	certificationRequalifyBudget   = 60 * time.Second
	// certificationRequalifyMinInterval is the least time between two
	// re-qualifications' starts after a lane stops answering: twice the eager
	// retry interval, so a lane that flaps costs at most one corpus run per
	// half-minute per line, while a restarted lane is certified against again
	// within about that long of coming back.
	certificationRequalifyMinInterval = 30 * time.Second
)

// GatedCertificationValidator certifies against a discovered default lane only
// once that lane has qualified — by routing's boot-time attempt, or by this
// client's own. The client keeps its own qualification state, independent of
// routing's DiscoveredLane, which it never changes: a lane that qualifies here
// late does not become a routing lane.
//
// Its own attempts run in a background loop from Start (the certification
// worker's start; construction starts nothing), on the schedule above, and stop at the first success from either side. No exchange waits on
// or triggers an attempt: while the lane is not ready the call answers
// CertificationLaneUnavailable at once, naming the qualification's state, and
// the first exchange after a success certifies. So a validator that comes up
// after routing's boot budget has expired is certified against within one
// interval of coming up, and a validator that never resolves costs one bounded
// attempt per interval, never a round trip per exchange. The client is this
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

	// schedule, overridable in tests
	delay, interval, eager, idle, minRequalify time.Duration

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
	ctx, cancel := context.WithCancel(context.Background())
	return &GatedCertificationValidator{
		lane: lane, client: client, qualify: qualify, reason: reason,
		now: time.Now, delay: certificationRequalifyDelay, interval: certificationRequalifyInterval,
		eager: certificationRequalifyEager, idle: certificationRequalifyIdle, minRequalify: certificationRequalifyMinInterval,
		ctx: ctx, cancel: cancel,
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
	v.startLocked(v.delay)
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
// A qualified lane that stops answering at the connection (laneStopped) may
// have restarted, and a restarted lane is warming again. That failure clears
// the qualification, routing's included for this client, and qualifies the
// lane again, at most once per minRequalify; that certification and every one
// until an attempt passes are unavailable, naming the re-qualification, and
// nothing is dialed for them. Any other failure (a 5xx, an answer that is not
// an OperationOutcome, the caller's own deadline) marks only that
// certification unavailable, and an answer that is a verdict, valid or
// invalid, changes nothing. A lane restarted between two certifications,
// with no failure in between, is not detected.
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
	ready := v.ready || (!v.stale && v.lane.Ready())
	failures, stale := v.failures, v.stale
	v.mu.Unlock()
	if ready {
		res, err := v.client.Validate(ctx, body, profile)
		if err == nil || !laneStopped(ctx, err) {
			return res, err
		}
		v.requalify()
		return shnsdk.Result{}, &CertificationLaneUnavailable{Reason: v.reason + "; " + v.requalifying}
	}
	state := v.pending
	switch {
	case failures > 0:
		state = v.failing
	case stale:
		state = v.requalifying
	}
	return shnsdk.Result{}, &CertificationLaneUnavailable{Reason: v.reason + "; " + state}
}

// laneStopped reports whether err is the lane failing at the connection: it
// refused, reset or closed the connection, could not be reached, or did not
// answer in time while the caller was still waiting. A lane that answered at
// all (any status, any body) has not stopped, and a caller that gave up
// (callerGaveUp) says nothing about the lane.
func laneStopped(ctx context.Context, err error) bool {
	if callerGaveUp(ctx) {
		return false
	}
	var answered *certificationHTTPError
	if errors.As(err, &answered) {
		return false
	}
	var op *net.OpError
	var timeout net.Error
	return errors.As(err, &op) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) ||
		(errors.As(err, &timeout) && timeout.Timeout())
}

// callerGaveUp reports whether the caller stopped waiting for the lane. Under
// the collector, a certification's own context is its candidate's, bounded by
// the certification client's own limits (certificationCandidateTimeout, inside
// certificationClientTimeout); it
// running out while the collection's context (its deadline, the worker's
// shutdown) is still live means the lane did not answer in time, not that the
// caller gave up. Any other caller gave up when its context ended.
func callerGaveUp(ctx context.Context) bool {
	if collection, ok := ctx.Value(certificationCollectionKey{}).(context.Context); ok {
		return collection.Err() != nil
	}
	return ctx.Err() != nil
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
	now := v.now()
	wait := time.Duration(0)
	if !v.requalified.IsZero() {
		if next := v.requalified.Add(v.minRequalify); next.After(now) {
			wait = next.Sub(now)
		}
	}
	v.requalified = now.Add(wait)
	v.startLocked(wait)
}

// loop runs the client's own attempts until the lane is ready from either
// side (only this client's own once a failure made it stale) or the client
// is closed.
//
// Every exit clears looping in the same lock hold that decides it, so a
// failure that arrives after the decision starts a new loop rather than
// finding this one still marked running.
func (v *GatedCertificationValidator) loop(wait time.Duration) {
	defer v.wg.Done()
	born := v.now()
	for {
		timer := time.NewTimer(wait)
		select {
		case <-v.ctx.Done():
			timer.Stop()
			v.mu.Lock()
			v.looping = false
			v.mu.Unlock()
			return
		case <-timer.C:
		}
		v.mu.Lock()
		routed := !v.stale && v.lane.Ready()
		if routed {
			v.looping = false
		}
		v.mu.Unlock()
		if routed {
			return // routing qualified it; nothing to add
		}
		if v.attempt() {
			return
		}
		wait = v.interval
		if v.now().Sub(born) > v.eager {
			wait = v.idle
		}
	}
}

// attempt runs one bounded qualification of the lane for this client alone
// and reports success. Outcome changes are logged — identity and timing only,
// never validator output — so a lane that stays down does not log per attempt.
func (v *GatedCertificationValidator) attempt() bool {
	started := v.now()
	ctx, cancel := context.WithTimeout(v.ctx, certificationRequalifyBudget)
	err := v.qualify(ctx, v.lane.Base(), v.lane.line)
	cancel()
	v.mu.Lock()
	state := "ready"
	changed := true
	if err == nil {
		v.ready, v.stale, v.looping = true, false, false // the loop exits on success
	} else {
		state = "failed"
		changed = v.failures == 0
		v.failures++
	}
	v.mu.Unlock()
	if changed {
		event, _ := json.Marshal(struct {
			Version    int     `json:"version"`
			Line       string  `json:"line"`
			Base       string  `json:"base"`
			Host       string  `json:"host"`
			State      string  `json:"state"`
			Reason     string  `json:"reason,omitempty"`
			At         string  `json:"at"`
			DurationMS float64 `json:"duration_ms"`
		}{1, v.lane.line, v.lane.Base(), lanequalify.Host(v.lane.Base()), state, lanequalify.FailureReason(err), v.now().UTC().Format(time.RFC3339Nano), float64(v.now().Sub(started)) / float64(time.Millisecond)})
		log.Printf("gateway: certification_lane_qualification %s", event)
	}
	return err == nil
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
