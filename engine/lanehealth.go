package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/internal/lanequalify"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// lanehealth.go is whether an explicitly configured validator lane
// (FHIR_VALIDATE_URL_<line>, or FHIR_VALIDATE_URL's lane at its line) can be
// reached, for module 2 alone: a chain is
// planned to a line only while its lane can certify what the walk makes
// (design §3.3, Lanes). A discovered lane has its own signal, its
// qualification (DiscoveredLane); an explicit lane was never qualified, so it
// starts reachable, as it boots today, and only its own failures change that.
//
// Every line-scoped $validate through the lane reports to it, whoever runs it:
// a governed check, arm (2)'s build, module 2's bridged check. A failure at the connection
// (laneUnreachable) makes the lane unreachable at once. A lane that did not
// answer in time (laneTimedOut) may only be busy, so it is asked for its
// metadata once, on a connection of its own: it stays reachable if it answers.
// While it is unreachable, its metadata is asked for in the background on the
// schedule below, and the first answer, of any status, makes it reachable
// again: the state it booted in, never a qualification it never ran. A lane
// that answers before it has warmed up can then still fail the first bridged
// check (a timeout): that exchange is refused, and the lane stays reachable.
//
// Nothing but module 2's Reach reads the signal (laneReachable): governed
// checks and arm (2) call the lane exactly as before. The exchange that finds
// a lane down after its chain was planned (the bridged check after the walk)
// is refused, as SHN's own edit it could not certify; the legs after it are
// carried.

// The background schedule while a lane is unreachable: every
// explicitLaneInterval for explicitLaneEager after it became unreachable, then
// every explicitLaneIdle. explicitLaneInterval is also the least time between
// two checks of a lane that timed out.
const (
	explicitLaneInterval = 30 * time.Second
	explicitLaneEager    = time.Hour
	explicitLaneIdle     = 5 * time.Minute
)

// explicitLane is an explicitly configured validator lane and whether it can
// be reached.
type explicitLane struct {
	lines []string // every line the lane serves, for its log lines
	inner *shnsdk.OperationValidator
	// probe asks the lane for its metadata once (laneAnswers); now and after
	// are the schedule's clock. Tests replace them.
	probe func(context.Context) error
	now   func() time.Time
	after func(time.Duration) <-chan time.Time

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu sync.Mutex
	// unreachable: the lane failed at the connection, or did not answer in
	// time and then did not answer its metadata, and has not answered its
	// metadata since. looping: the background checks are running. checked is
	// when a timeout last started a check. failures counts the failures
	// reported, so a check that passed does not make reachable a lane that
	// failed again while it ran.
	unreachable, looping, closed bool
	checking                     bool
	checked                      time.Time
	failures                     int
}

// newExplicitLane is the explicit lane inner serving lines, reachable until it
// fails. Its checks dial a connection of their own each time, never one the
// lane's $validate calls hold, and keep none.
func newExplicitLane(lines []string, inner *shnsdk.OperationValidator) *explicitLane {
	client := ownConnectionClient()
	ctx, cancel := context.WithCancel(context.Background())
	return &explicitLane{
		lines: lines, inner: inner,
		probe: func(ctx context.Context) error { return laneAnswers(ctx, client, inner.BaseURL) },
		now:   time.Now, after: time.After,
		ctx: ctx, cancel: cancel,
	}
}

// Validate is the lane's $validate. Its failure reports to the lane.
func (l *explicitLane) Validate(ctx context.Context, body []byte, profile string) (shnsdk.Result, error) {
	res, err := l.inner.Validate(ctx, body, profile)
	switch laneFailureOf(ctx, err) {
	case laneUnreachable:
		l.lost()
	case laneTimedOut:
		l.timedOut()
	}
	return res, err
}

// laneReachable reports whether module 2 may plan a chain certified on this
// lane. It does no network work.
func (l *explicitLane) laneReachable() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return !l.unreachable
}

// lost makes the lane unreachable and starts its background checks.
func (l *explicitLane) lost() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failures++
	if l.closed || l.unreachable {
		return
	}
	l.unreachable = true
	l.logState("validator_lane_unreachable")
	if !l.looping {
		l.looping = true
		l.wg.Add(1)
		go l.loop()
	}
}

// timedOut checks a lane that did not answer in time, at most once per
// explicitLaneInterval: one metadata request, and the lane is lost unless it
// answers.
func (l *explicitLane) timedOut() {
	l.mu.Lock()
	now := l.now()
	if l.closed || l.unreachable || l.checking || (!l.checked.IsZero() && now.Before(l.checked.Add(explicitLaneInterval))) {
		l.mu.Unlock()
		return
	}
	l.checking, l.checked = true, now
	l.wg.Add(1)
	l.mu.Unlock()
	go func() {
		defer l.wg.Done()
		err := l.probe(l.ctx)
		l.mu.Lock()
		l.checking = false
		l.mu.Unlock()
		if err != nil && l.ctx.Err() == nil {
			l.lost()
		}
	}()
}

// loop asks an unreachable lane for its metadata on the schedule until it
// answers or the lane is closed.
func (l *explicitLane) loop() {
	defer l.wg.Done()
	since := l.now()
	for {
		wait := explicitLaneInterval
		if l.now().Sub(since) >= explicitLaneEager {
			wait = explicitLaneIdle
		}
		select {
		case <-l.ctx.Done():
			l.mu.Lock()
			l.looping = false
			l.mu.Unlock()
			return
		case <-l.after(wait):
		}
		l.mu.Lock()
		failures := l.failures
		l.mu.Unlock()
		if l.probe(l.ctx) != nil {
			continue
		}
		l.mu.Lock()
		if l.failures != failures {
			// The lane failed again while it was checked.
			l.mu.Unlock()
			continue
		}
		if l.ctx.Err() == nil {
			l.unreachable = false
			l.logState("validator_lane_reachable")
		}
		l.looping = false
		l.mu.Unlock()
		return
	}
}

// close stops the lane's checks and waits for them.
func (l *explicitLane) close() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	l.cancel()
	l.wg.Wait()
}

// logState writes one line for the lane's change of state; l.mu is held.
func (l *explicitLane) logState(event string) {
	line, _ := json.Marshal(struct {
		Version int      `json:"version"`
		Lines   []string `json:"lines"`
		Host    string   `json:"host"`
		At      string   `json:"at"`
	}{1, l.lines, lanequalify.Host(l.inner.BaseURL), l.now().UTC().Format(time.RFC3339Nano)})
	log.Printf("gateway: %s %s", event, line)
}

// ownConnectionClient is the client a lane's metadata check uses: it dials a
// connection of its own for each request, never one a lane's $validate calls
// hold, keeps none, and follows no redirect.
func ownConnectionClient() *http.Client {
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment}
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = t.Clone() // an embedder may have replaced the default
	}
	transport.DisableKeepAlives, transport.ForceAttemptHTTP2 = true, false
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// explicitLaneProbeBudget bounds one metadata request to a lane.
const explicitLaneProbeBudget = 4 * time.Second

// laneAnswers asks the lane at base for its metadata once, within
// explicitLaneProbeBudget: nil when it answers at all, whatever the status or
// body, and the transport's error when it does not.
func laneAnswers(ctx context.Context, client *http.Client, base string) error {
	ctx, cancel := context.WithTimeout(ctx, explicitLaneProbeBudget)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/metadata", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<20))
	return resp.Body.Close()
}

// laneFailure is what a lane call's error says about the lane.
type laneFailure int

const (
	// laneAnswered: nothing about the lane's reachability. It answered (any
	// status, any body), the error is not the connection's, or the caller
	// gave up.
	laneAnswered laneFailure = iota
	// laneTimedOut: the lane did not answer in time while the caller still
	// waited. A lane that is busy, not stopped, does this too.
	laneTimedOut
	// laneUnreachable: the lane failed at the connection: it refused, reset or
	// closed it, or could not be reached.
	laneUnreachable
)

// laneFailureOf classifies err, a lane call's error under ctx.
func laneFailureOf(ctx context.Context, err error) laneFailure {
	if err == nil || callerGaveUp(ctx) {
		return laneAnswered
	}
	var answered *certificationHTTPError
	if errors.As(err, &answered) {
		return laneAnswered
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return laneTimedOut
	}
	var op *net.OpError
	if errors.As(err, &op) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) {
		return laneUnreachable
	}
	return laneAnswered
}

// laneReachable reports whether v, a lane validatorForContractLine resolved,
// may be planned to by module 2: any lane but an explicit one that cannot be
// reached.
func laneReachable(v shnsdk.Validator) bool {
	if r, ok := v.(interface{ laneReachable() bool }); ok {
		return r.laneReachable()
	}
	return true
}

// explicitLanes gives each explicitly configured lane in lanes its
// reachability: a fresh map, every key that held the same lane holding the
// same explicitLane, and the explicitLanes made, for Close. A discovered lane
// keeps its own signal; a validator that is not an OperationValidator (a test
// double) has no connection to fail.
func explicitLanes(lanes map[string]shnsdk.Validator) (map[string]shnsdk.Validator, []*explicitLane) {
	served := map[*shnsdk.OperationValidator]map[string]bool{}
	for key, v := range lanes {
		if op, ok := v.(*shnsdk.OperationValidator); ok {
			if served[op] == nil {
				served[op] = map[string]bool{}
			}
			_, line, _ := strings.Cut(key, "@")
			served[op][line] = true
		}
	}
	made := map[*shnsdk.OperationValidator]*explicitLane{}
	var all []*explicitLane
	for op, lines := range served {
		made[op] = newExplicitLane(slices.Sorted(maps.Keys(lines)), op)
		all = append(all, made[op])
	}
	out := make(map[string]shnsdk.Validator, len(lanes))
	for key, v := range lanes {
		if op, ok := v.(*shnsdk.OperationValidator); ok {
			out[key] = made[op]
			continue
		}
		out[key] = v
	}
	return out, all
}
