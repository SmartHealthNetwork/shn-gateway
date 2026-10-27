package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/internal/backoff"
	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
	"github.com/SmartHealthNetwork/shn-gateway/internal/lanequalify"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// DefaultLaneURL is the Compose service address for a native validator line.
func DefaultLaneURL(line string) string {
	return "http://shn-validator-" + strings.ReplaceAll(line, ".", "-") + ":8080/fhir"
}

// LaneQualifier proves a validator's finite synthetic corpus for one line.
type LaneQualifier func(context.Context, string, string) error

// DiscoveredLane admits a default endpoint once a qualification passes.
// Its manager supplies the lifecycle context; ordinary validation never changes readiness.
// A failed qualification is never final: the lane stays unqualified,
// reports so (State), and a later Qualify runs a fresh attempt, on the
// manager's schedule (KeepQualifying). Once one passes the lane is ready for
// the life of the process.
type DiscoveredLane struct {
	line, base string
	validator  shnsdk.Validator
	ready      atomic.Bool

	mu       sync.Mutex
	inflight chan struct{} // closed when the running attempt ends; nil when none runs
	attempts int           // attempts finished
	failures int           // consecutive failed attempts; 0 once one passes
	err      error         // the last attempt's failure; nil once one passes
	// passed is the qualification that made the lane ready: the endpoint, the
	// line and the qualifier it ran (qualifierID), which a certification client
	// compares with its own before it takes the pass as its own.
	passed lanePass
	// keeping counts the KeepQualifying loops retrying the lane now.
	keeping atomic.Int32
}

// lanePass is what one passing qualification ran: the endpoint it posted the
// corpus to, the line whose corpus it was, the qualifier that ran it, and how
// long it took.
type lanePass struct {
	base, line string
	qualifier  uintptr
	took       time.Duration
}

// qualifierID identifies a qualifier by its code. Two LaneQualifier values
// with one ID run the same function; with the line, which selects the corpus,
// that is the same qualification. That holds for a top-level function, or
// one closure value passed to both routing and certification, as the
// gateway's is. Closures of one literal are not enough: an inlined literal
// is a function of its own at each call site. Which side an attempt is for
// travels in its context (endpointcert.WithTag), not in the qualifier.
func qualifierID(q LaneQualifier) uintptr {
	if q == nil {
		return 0
	}
	return reflect.ValueOf(q).Pointer()
}

// passedWith is the qualification that made the lane ready, ok false while it
// is not.
func (d *DiscoveredLane) passedWith() (lanePass, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.passed, d.ready.Load()
}

// retrying reports whether a KeepQualifying loop is qualifying the lane now:
// its manager keeps retrying until an attempt passes.
func (d *DiscoveredLane) retrying() bool { return d.keeping.Load() > 0 }

func NewDiscoveredLane(line, base string, validator shnsdk.Validator) *DiscoveredLane {
	return &DiscoveredLane{line: line, base: base, validator: validator}
}

// Ready is a local atomic snapshot with no network activity.
func (d *DiscoveredLane) Ready() bool { return d.ready.Load() }

// Base is the endpoint the lane qualifies.
func (d *DiscoveredLane) Base() string { return d.base }

// Line is the contract line the lane serves.
func (d *DiscoveredLane) Line() string { return d.line }

// LaneState is a lane's qualification as its reports read it: no network
// activity and no validator output, only whether it is ready, how many
// attempts have finished, how many of the latest failed in a row, and the
// last failure (lanequalify.FailureReason names its kind).
type LaneState struct {
	Line, Base string
	Ready      bool
	Attempts   int
	Failures   int
	Err        error
}

// State is the lane's qualification now.
func (d *DiscoveredLane) State() LaneState {
	d.mu.Lock()
	defer d.mu.Unlock()
	return LaneState{Line: d.line, Base: d.base, Ready: d.ready.Load(), Attempts: d.attempts, Failures: d.failures, Err: d.err}
}

// Qualify runs one qualification attempt unless the lane is ready. Attempts
// never overlap: a caller that arrives while one runs waits for it and
// returns its outcome, so concurrent callers make one attempt, as they always
// have. A failure is not terminal: the next call runs a new attempt.
func (d *DiscoveredLane) Qualify(ctx context.Context, q LaneQualifier) error {
	for {
		if d.ready.Load() {
			return nil
		}
		d.mu.Lock()
		if d.ready.Load() {
			d.mu.Unlock()
			return nil
		}
		if wait := d.inflight; wait != nil {
			attempts := d.attempts
			d.mu.Unlock()
			select {
			case <-wait:
			case <-ctx.Done():
				return ctx.Err()
			}
			d.mu.Lock()
			// The attempt this caller waited for decided it.
			if d.attempts > attempts {
				err := d.err
				d.mu.Unlock()
				return err
			}
			d.mu.Unlock()
			continue
		}
		if err := ctx.Err(); err != nil {
			d.mu.Unlock()
			return err
		}
		done := make(chan struct{})
		d.inflight = done
		d.mu.Unlock()
		return d.attempt(ctx, q, done)
	}
}

// attempt runs q as the lane's one in-flight attempt and records its outcome.
// The deferred release runs even if q panics, so callers waiting on done
// are never left blocked.
func (d *DiscoveredLane) attempt(ctx context.Context, q LaneQualifier, done chan struct{}) (err error) {
	err = errLaneAttemptAborted
	started := time.Now()
	defer func() {
		d.mu.Lock()
		d.attempts++
		if err == nil {
			d.failures, d.err = 0, nil
			d.passed = lanePass{base: d.base, line: d.line, qualifier: qualifierID(q), took: time.Since(started)}
			d.ready.Store(true)
		} else {
			d.failures++
			d.err = err
		}
		d.inflight = nil
		d.mu.Unlock()
		close(done)
	}()
	// The attempt is routing's: a qualifier that records the lane's
	// certificate (MON-R25) records it under the validator kind for the line.
	err = q(endpointcert.WithTag(ctx, endpointcert.Lane(endpointcert.Validator, d.line)), d.base, d.line)
	if err == nil {
		err = ctx.Err()
	}
	return err
}

// errLaneAttemptAborted is the outcome recorded for an attempt whose
// qualifier did not return (it panicked).
var errLaneAttemptAborted = errors.New("validator lane qualification aborted")

// LaneBackoff is the schedule a validator lane's qualification retries on
// after a failure: Base after the first failure, doubling with
// each further one up to Max. Jitter spreads each wait over its upper half,
// so gateways whose lanes failed together after one deploy do not retry
// together; it returns a value in [0, 1). A nil Jitter waits the whole step.
type LaneBackoff struct {
	Base, Max time.Duration
	Jitter    func() float64
}

// Default lane retry schedule: 15 s after the first failure, doubling to 5
// minutes.
const (
	laneRetryBase = 15 * time.Second
	laneRetryMax  = 5 * time.Minute
)

// DefaultLaneBackoff is the lane retry schedule with jitter from jitter.
func DefaultLaneBackoff(jitter func() float64) LaneBackoff {
	return LaneBackoff{Base: laneRetryBase, Max: laneRetryMax, Jitter: jitter}
}

// Wait is how long to wait after failures consecutive failures (at least 1),
// on the gateway's shared schedule (internal/backoff). A Base or Max that is
// not positive is the default's, so a zero LaneBackoff never retries in a
// tight loop.
func (b LaneBackoff) Wait(failures int) time.Duration {
	if b.Base <= 0 {
		b.Base = laneRetryBase
	}
	if b.Max <= 0 {
		b.Max = laneRetryMax
	}
	return backoff.Exponential{Base: b.Base, Max: b.Max, Jitter: b.Jitter}.Wait(failures)
}

// LaneSchedule is how KeepQualifying retries: the backoff between attempts,
// each attempt's own budget, and the clock it waits on (nil: time.After).
// Changed, when set, is told the lane's state when it changes: at the first
// failure of a run of failures, when the failure's reason changes, and when
// the lane becomes ready, with the wait before the next attempt (0 when
// ready). Attempted, when set, is told every attempt's outcome.
type LaneSchedule struct {
	Backoff   LaneBackoff
	Budget    time.Duration
	After     func(time.Duration) <-chan time.Time
	Changed   func(state LaneState, next time.Duration)
	Attempted func(ready bool)
}

// KeepQualifying qualifies the lane until an attempt passes or ctx ends.
// Each attempt is bounded by s.Budget; between attempts it waits
// s.Backoff.Wait for the failures so far. It returns nil once the lane is
// ready, and otherwise ctx's error joined with the last attempt's.
func (d *DiscoveredLane) KeepQualifying(ctx context.Context, q LaneQualifier, s LaneSchedule) error {
	d.keeping.Add(1)
	defer d.keeping.Add(-1)
	after := s.After
	if after == nil {
		after = time.After
	}
	var lastReason error
	for {
		attemptCtx, cancel := ctx, context.CancelFunc(func() {})
		if s.Budget > 0 {
			attemptCtx, cancel = context.WithTimeout(ctx, s.Budget)
		}
		err := d.Qualify(attemptCtx, q)
		cancel()
		if s.Attempted != nil && ctx.Err() == nil {
			s.Attempted(err == nil)
		}
		state := d.State()
		if err == nil {
			if s.Changed != nil {
				s.Changed(state, 0)
			}
			return nil
		}
		if ctx.Err() != nil {
			return errors.Join(ctx.Err(), err)
		}
		wait := s.Backoff.Wait(state.Failures)
		if s.Changed != nil && (state.Failures == 1 || !sameFailure(lastReason, err)) {
			s.Changed(state, wait)
		}
		lastReason = err
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), err)
		case <-after(wait):
		}
	}
}

// sameFailure reports whether two attempts failed for the same kind of
// reason, so a lane that keeps failing the same way logs once.
func sameFailure(a, b error) bool {
	if a == nil || b == nil {
		return a == b
	}
	return lanequalify.FailureReason(a) == lanequalify.FailureReason(b)
}

func (d *DiscoveredLane) Validate(ctx context.Context, body []byte, profile string) (shnsdk.Result, error) {
	if !d.Ready() {
		return shnsdk.Result{}, fmt.Errorf("validator line %s has not qualified", d.line)
	}
	return d.validator.Validate(ctx, body, profile)
}

// QualifyValidatorLane is the qualification a validator lane passes before it
// is used: it waits only for a bounded FHIR metadata response, then posts the
// line's single finite readiness corpus, which also absorbs a lane's first,
// still-warming answers. Metadata availability never grants lane readiness. It
// is a LaneQualifier.
func QualifyValidatorLane(ctx context.Context, base, line string) error {
	return QualifyValidatorLaneWith(ctx, &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, base, line)
}

// QualifyValidatorLaneWith is QualifyValidatorLane with client for its
// metadata reads. The client must follow no redirect; a gateway passes one
// with the lane's endpoint kind on its transport, so each qualification
// records the lane's certificate (MON-R25). The corpus that follows is
// posted by lanequalify.Warm, whose orchestrator is the Kit's byte for byte,
// to the base that read was just made to.
func QualifyValidatorLaneWith(ctx context.Context, client *http.Client, base, line string) error {
	defer client.CloseIdleConnections()
	// The last metadata attempt's outcome, so a lane that never answers fails
	// naming why (its name does not resolve, it refuses), not only that time ran out.
	var last error
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, strings.TrimRight(base, "/")+"/metadata", nil)
		if err != nil {
			cancel()
			return err
		}
		resp, err := client.Do(req)
		if err != nil && ctx.Err() == nil {
			// Kept only while the budget runs: the attempt the budget cuts off
			// says nothing about the lane.
			last = err
		}
		if err == nil {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var metadata struct {
					ResourceType string `json:"resourceType"`
					FHIRVersion  string `json:"fhirVersion"`
				}
				if readErr != nil || len(body) > 4<<20 || json.Unmarshal(body, &metadata) != nil || metadata.ResourceType != "CapabilityStatement" || !strings.HasPrefix(metadata.FHIRVersion, "4.0.") {
					cancel()
					return lanequalify.ErrNotR4Metadata
				}
				cancel()
				if err := lanequalify.Warm(ctx, strings.TrimRight(base, "/"), line, nil); err != nil {
					return fmt.Errorf("%w: %w", lanequalify.ErrCorpus, err)
				}
				return nil
			}
			last = &lanequalify.MetadataStatusError{Status: resp.StatusCode}
		}
		cancel()
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			if last != nil {
				return fmt.Errorf("metadata unavailable: %w: %w", last, ctx.Err())
			}
			return fmt.Errorf("metadata unavailable: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func nativeContractLineCount(contract string) int {
	lines := map[string]bool{}
	for _, tok := range NativeContractVersions() {
		c, l, ok := strings.Cut(tok, "@")
		if ok && c == contract {
			lines[l] = true
		}
	}
	return len(lines)
}

// validatorForContractLine resolves the $validate lane for contract at line
// (line "" = no line in play: the canonical Validator). The lane is the one
// configured for that contract line exactly (Config.ValidatorsByContractLine):
// a discovered lane serves once it has qualified. A deployment with no lanes
// configured serves every contract line from Config.Validator; one with lanes
// is authoritative, so a contract line it did not configure (another line, or
// an unknown contract) is unlaned and resolves to nil, and its callers fail
// closed rather than validating one IG version's bytes against another's.
func (g *Gateway) validatorForContractLine(contract, line string) shnsdk.Validator {
	if line == "" {
		return g.cfg.Validator
	}
	v := g.cfg.ValidatorsByContractLine[contract+"@"+line]
	if d, ok := v.(*DiscoveredLane); ok {
		return g.readyLane(d)
	}
	if v != nil {
		return v
	}
	if len(g.cfg.ValidatorsByContractLine) > 0 {
		return nil
	}
	return g.cfg.Validator
}

// deriveContractLanes gives a caller that still sets the deprecated per-line
// lanes, and not ValidatorsByContractLine, the contract-line lanes they name.
func deriveContractLanes(cfg *Config) {
	if len(cfg.ValidatorsByContractLine) == 0 && (len(cfg.ValidatorsByLine) > 0 || len(cfg.DefaultValidatorsByLine) > 0 || len(cfg.CanonicalFallbackLines) > 0) {
		cfg.ValidatorsByContractLine = ContractLineLanes(cfg.ValidatorsByLine, cfg.DefaultValidatorsByLine, cfg.CanonicalFallbackLines, cfg.DeclaredContractVersions)
	}
}

// ContractLineLanes derives contract-line lanes (Config.ValidatorsByContractLine)
// from per-line ones: byLine's explicit lanes, defaults' discovered lanes, and
// the lines on which byLine's lane is the canonical validator lent to a
// single-line contract (fallbacks). For every contract this gateway builds
// natively (NativeContractVersions, never the SDK's set) or declared names, at
// every line any of them names:
//   - at a fallback line, a single-line contract borrows byLine's lane, and a
//     multi-line contract has only the line's discovered lane, never the
//     canonical one;
//   - at any other line, an explicit lane wins over a discovered one.
func ContractLineLanes(byLine map[string]shnsdk.Validator, defaults map[string]*DiscoveredLane, fallbacks map[string]bool, declared []string) map[string]shnsdk.Validator {
	contracts := map[string]bool{}
	for _, tok := range append(NativeContractVersions(), declared...) {
		if c, _, ok := strings.Cut(tok, "@"); ok {
			contracts[c] = true
		}
	}
	lines := map[string]bool{}
	for l := range byLine {
		lines[l] = true
	}
	for l := range defaults {
		lines[l] = true
	}
	for l := range fallbacks {
		lines[l] = true
	}
	out := map[string]shnsdk.Validator{}
	for c := range contracts {
		for l := range lines {
			key := c + "@" + l
			switch {
			case fallbacks[l] && nativeContractLineCount(c) < 2:
				// The canonical validator lent to a single-line contract: nil
				// when byLine names none, which still makes the deployment
				// laned (it serves nothing there).
				out[key] = byLine[l]
			case fallbacks[l]:
				// A multi-line contract at an alias line has only the line's
				// discovered lane: nil when there is none.
				if d := defaults[l]; d != nil {
					out[key] = d
				} else {
					out[key] = nil
				}
			case byLine[l] != nil:
				out[key] = byLine[l]
			case defaults[l] != nil:
				out[key] = defaults[l]
			default:
				// A line byLine names with no validator still makes the
				// deployment laned: it serves nothing there.
				if _, named := byLine[l]; named {
					out[key] = nil
				}
			}
		}
	}
	return out
}

// servesLine reports whether this gateway builds and answers at contract's
// line: a validator lane serves it, or no payload check consumes lanes here and
// the gateway declares it. A gateway whose payload checks consume no lane needs
// no validator to answer or continue at a line it declares (FR-G29). Where they
// do, a gateway refuses to boot with a declared line it has no lane for, so this
// is the lane alone there. Native reach and transform chains do not read it:
// a chain needs a lane, so a gateway with no validator never builds a bridged
// transform, and native reach needs one unless no payload check consumes lanes
// and the message's builder is certified at the line (buildsAt).
func (g *Gateway) servesLine(contract, line string) bool {
	if g.validatorForContractLine(contract, line) != nil {
		return true
	}
	return !payloadChecksConsumeLanes(g.conformance().Level()) && contractLineSet(g.declaredContractVersions(), contract)[line]
}

// payloadChecksConsumeLanes reports whether this gateway's payload conformance
// checks use its validator lanes at all: every level but none, which checks no
// payload. It is the one place a lane decision reads the conformance level.
// The other consumers of a lane: native reach builds at a peer's line where a
// lane serves it, or, where this predicate is false, for a builder certified at
// that line (buildsAt); module 2's Certify needs one for any chain it plans, and
// E-08 reads one for the answer's source line, at every level.
func payloadChecksConsumeLanes(level ConformanceEnforcement) bool {
	return level != EnforcementNone
}

// readyLane is a discovered lane once it has qualified, observed when the
// gateway has an observer; nil while it has not.
func (g *Gateway) readyLane(d *DiscoveredLane) shnsdk.Validator {
	if d == nil || !d.Ready() {
		return nil
	}
	if g.cfg.Observer != nil {
		return observingValidator{inner: d, g: g}
	}
	return d
}
