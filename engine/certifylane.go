package engine

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

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

// The certification client's own qualification schedule. The first attempt
// waits certificationRequalifyDelay so routing's boot-time attempt normally
// settles the lane first; while the lane has never qualified, attempts repeat
// every certificationRequalifyInterval for certificationRequalifyEager after
// construction (a validator that is going to come up does so within that), then
// every certificationRequalifyIdle. An attempt is bounded by
// certificationRequalifyBudget. The corpus is only run once the validator
// answers its metadata. Against a name that never resolves, an attempt spends
// its budget retrying the metadata probe once a second (the qualifier's own
// cadence), so for the first hour a line costs about one name lookup per
// second and afterwards about one per five minutes; accepted as the price of
// certifying against a validator that comes up late, and zero once it does.
const (
	certificationRequalifyDelay    = 15 * time.Second
	certificationRequalifyInterval = 15 * time.Second
	certificationRequalifyEager    = time.Hour
	certificationRequalifyIdle     = 5 * time.Minute
	certificationRequalifyBudget   = 60 * time.Second
)

// GatedCertificationValidator certifies against a discovered default lane only
// once that lane has qualified — by routing's boot-time attempt, or by this
// client's own. The client keeps its own qualification state, independent of
// routing's DiscoveredLane, which it never changes: a lane that qualifies here
// late does not become a routing lane.
//
// Its own attempts run in a background loop from construction, on the schedule
// above, and stop at the first success from either side. No exchange waits on
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
	now     func() time.Time

	// schedule, overridable in tests
	delay, interval, eager, idle time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	ready    bool
	failures int
}

// NewGatedCertificationValidator gates a certification client on lane's
// qualification; reason is the configuration the verdict names while the lane
// is not ready (the state of the qualification is appended), and qualify is the
// same qualifier routing uses for a default lane. With a qualifier the client's
// own attempts start in the background at once.
func NewGatedCertificationValidator(lane *DiscoveredLane, qualify LaneQualifier, reason string) *GatedCertificationValidator {
	ctx, cancel := context.WithCancel(context.Background())
	v := &GatedCertificationValidator{
		lane: lane, client: NewCertificationOperationValidator(lane.Base()), qualify: qualify, reason: reason,
		now: time.Now, delay: certificationRequalifyDelay, interval: certificationRequalifyInterval,
		eager: certificationRequalifyEager, idle: certificationRequalifyIdle, ctx: ctx, cancel: cancel,
	}
	if qualify != nil {
		v.wg.Add(1)
		go v.loop()
	}
	return v
}

// Validate certifies through the gated client once the lane is ready; until
// then it answers the lane-unavailable error immediately.
func (v *GatedCertificationValidator) Validate(ctx context.Context, body []byte, profile string) (shnsdk.Result, error) {
	if v.lane.Ready() {
		return v.client.Validate(ctx, body, profile)
	}
	v.mu.Lock()
	ready, failures := v.ready, v.failures
	v.mu.Unlock()
	if ready {
		return v.client.Validate(ctx, body, profile)
	}
	state := "default lane qualification pending"
	if failures > 0 {
		state = "default lane qualification failed, retrying"
	}
	return shnsdk.Result{}, &CertificationLaneUnavailable{Reason: v.reason + "; " + state}
}

// loop runs the client's own attempts until the lane is ready from either
// side or the client is closed.
func (v *GatedCertificationValidator) loop() {
	defer v.wg.Done()
	born := v.now()
	wait := v.delay
	for {
		timer := time.NewTimer(wait)
		select {
		case <-v.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if v.lane.Ready() {
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
		v.ready = true
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
			State      string  `json:"state"`
			At         string  `json:"at"`
			DurationMS float64 `json:"duration_ms"`
		}{1, v.lane.line, v.lane.Base(), state, v.now().UTC().Format(time.RFC3339Nano), float64(v.now().Sub(started)) / float64(time.Millisecond)})
		log.Printf("gateway: certification_lane_qualification %s", event)
	}
	return err == nil
}

// Endpoint is the lane's base URL, what the gated client dials once qualified.
func (v *GatedCertificationValidator) Endpoint() string { return v.client.BaseURL }

// Close stops the background attempts and releases the client's pool.
func (v *GatedCertificationValidator) Close() {
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
