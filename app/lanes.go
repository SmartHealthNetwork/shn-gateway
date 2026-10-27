//go:build !shn_coreonly

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/checks"
	"github.com/SmartHealthNetwork/shn-gateway/engine"
	"github.com/SmartHealthNetwork/shn-gateway/internal/lanequalify"
	"github.com/SmartHealthNetwork/shn-gateway/telemetry"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

const defaultLaneStartupBudget = 600 * time.Second

// defaultLaneAttemptBudget bounds one qualification attempt of a default lane:
// long enough for the corpus on a lane several gateways qualify at once after
// a deploy, where 60 s was not.
const defaultLaneAttemptBudget = 120 * time.Second

// laneRetry is how a default lane retries its qualification. The zero
// value is the default: defaultLaneStartupBudget for a lane boot waits for,
// defaultLaneAttemptBudget per attempt, engine.DefaultLaneBackoff with random
// jitter, on the wall clock. Tests shorten it.
type laneRetry struct {
	startup, attempt time.Duration
	backoff          *engine.LaneBackoff
	after            func(time.Duration) <-chan time.Time
}

func (r *laneRetry) schedule() (startup time.Duration, s engine.LaneSchedule) {
	startup, s = defaultLaneStartupBudget, engine.LaneSchedule{Budget: defaultLaneAttemptBudget, Backoff: engine.DefaultLaneBackoff(rand.Float64)}
	if r == nil {
		return startup, s
	}
	if r.startup > 0 {
		startup = r.startup
	}
	if r.attempt > 0 {
		s.Budget = r.attempt
	}
	if r.backoff != nil {
		s.Backoff = *r.backoff
	}
	s.After = r.after
	return startup, s
}

// defaultValidatorLanesNone is FHIR_DEFAULT_VALIDATOR_LANES's one value: this
// gateway's network has no Compose default validator services, so it never
// probes their names. Unset keeps the default lanes.
const defaultValidatorLanesNone = "none"

// laneManager owns every default qualification worker for one gateway lifecycle.
// A worker retries until its lane qualifies or the manager closes.
type laneManager struct {
	defaults  map[string]*engine.DiscoveredLane
	fallbacks map[string]bool
	cancel    context.CancelFunc
	workers   sync.WaitGroup
	// attempted, once set (the metrics wiring, after boot), is told each
	// attempt's outcome by line.
	attempted atomic.Pointer[func(line string, ready bool)]
}

func (m *laneManager) Close() {
	if m != nil {
		m.cancel()
		m.workers.Wait()
	}
}

func checkValidatorLaneURL(name, base string) error {
	if err := checkOptionalURL(name, base); err != nil {
		return err
	}
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("invalid %s %q: validator endpoint requires a host", name, base)
	}
	return nil
}

func discoverValidatorLanes(ctx context.Context, getenv func(string) string, declared []string, canonical shnsdk.Validator, cfg config, resolve func(string) string, qualify engine.LaneQualifier) (map[string]shnsdk.Validator, *laneManager, error) {
	startupBudget, schedule := cfg.laneRetry.schedule()
	life, cancel := context.WithCancel(ctx)
	startup, cancelStartup := context.WithTimeout(life, startupBudget)
	m := &laneManager{defaults: map[string]*engine.DiscoveredLane{}, fallbacks: map[string]bool{}, cancel: func() { cancelStartup(); cancel() }}
	fail := func(err error) (map[string]shnsdk.Validator, *laneManager, error) { m.Close(); return nil, nil, err }
	linesPerContract := map[string]map[string]bool{}
	for _, tok := range engine.NativeContractVersions() {
		contract, line, _ := strings.Cut(tok, "@")
		if linesPerContract[contract] == nil {
			linesPerContract[contract] = map[string]bool{}
		}
		linesPerContract[contract][line] = true
	}
	// required are the declared default lanes boot waits for. Where no payload
	// check consumes a lane, none is waited for: every default qualifies in the
	// background, and a declared line is served before its lane qualifies, or
	// if it never does.
	required := map[string]bool{}
	for _, tok := range declared {
		contract, line, _ := strings.Cut(tok, "@")
		if len(linesPerContract[contract]) > 1 && payloadChecksConsumeLanes(cfg.ConformanceEnforcement) {
			required[line] = true
		}
	}
	supplied := cfg
	for _, entry := range []struct {
		line  string
		value *string
	}{{"2.1", &supplied.FHIRValidateURL21}, {"2.2", &supplied.FHIRValidateURL22}} {
		if *entry.value != "" {
			if err := checkValidatorLaneURL("FHIR_VALIDATE_URL_"+strings.ReplaceAll(entry.line, ".", "_"), *entry.value); err != nil {
				return fail(err)
			}
			continue
		}
		if fakeValidator(getenv) {
			continue
		}
		if cfg.DefaultValidatorLanes == defaultValidatorLanesNone {
			// A network without the Compose validator services (a hosted tenant):
			// no default lane exists to probe, so none is created. A declared line
			// then needs its FHIR_VALIDATE_URL_<line> (validatorLanesForDeclared),
			// except at none.
			continue
		}
		base := resolve(entry.line)
		if err := checkValidatorLaneURL("default validator "+entry.line, base); err != nil {
			return fail(err)
		}
		m.defaults[entry.line] = engine.NewDiscoveredLane(entry.line, base, taggedValidator(cfg.certs, entry.line, base))
		*entry.value = base
	}
	lanes, err := validatorLanesForDeclared(getenv, declared, canonical, supplied)
	if err != nil {
		return fail(err)
	}
	for line := range m.defaults {
		delete(lanes, line)
	}
	// A single-line contract's line (none is native today) rides the canonical validator
	// only as a fallback alias, which the engine never lends to a multi-line
	// contract at that line. With default lanes the line's default was removed
	// above; with none, validatorLanesForDeclared has already aliased it, and it
	// must be marked the same way, or PA traffic at that line would validate
	// against the canonical IG.
	canonicalLine := shnsdk.LineOf(engine.CanonicalContractVersion())
	explicit := map[string]string{"2.1": cfg.FHIRValidateURL21, "2.2": cfg.FHIRValidateURL22}
	aliasOnly := func(line string) bool {
		return cfg.DefaultValidatorLanes == defaultValidatorLanesNone && !fakeValidator(getenv) && line != canonicalLine && explicit[line] == ""
	}
	for _, tok := range declared {
		contract, line, _ := strings.Cut(tok, "@")
		if len(linesPerContract[contract]) < 2 && (lanes[line] == nil || aliasOnly(line)) && canonical != nil {
			lanes[line] = canonical
			m.fallbacks[line] = true
		}
	}
	// qualifyLane retries d's qualification until it passes or ctx ends. It logs
	// the lane's state changes only (started, the first failure of a run and
	// a change of its reason, ready), never each attempt, so a lane that stays
	// down cannot flood the log; attempts are counted by the metric instead.
	qualifyLane := func(ctx context.Context, line string, d *engine.DiscoveredLane) error {
		started := time.Now()
		emit := func(state, reason string, attempts int, next time.Duration) {
			// Only the synthetic default's identity, the host it dials, lifecycle
			// timing and a failure's kind are emitted; validator outcomes and error
			// text never enter this serialized log path (lanequalify.FailureReason).
			event, _ := json.Marshal(struct {
				Version    int     `json:"version"`
				Line       string  `json:"line"`
				Base       string  `json:"base"`
				Host       string  `json:"host"`
				State      string  `json:"state"`
				Reason     string  `json:"reason,omitempty"`
				Attempts   int     `json:"attempts,omitempty"`
				NextRetryS float64 `json:"next_retry_s,omitempty"`
				At         string  `json:"at"`
				DurationMS float64 `json:"duration_ms"`
			}{1, line, resolve(line), lanequalify.Host(resolve(line)), state, reason, attempts, next.Seconds(), time.Now().UTC().Format(time.RFC3339Nano), float64(time.Since(started)) / float64(time.Millisecond)})
			log.Printf("gateway: validator_qualification %s", event)
		}
		emit("started", "", 0, 0)
		s := schedule
		s.Changed = func(st engine.LaneState, next time.Duration) {
			if st.Ready {
				emit("ready", "", st.Attempts, 0)
				return
			}
			emit("failed", lanequalify.FailureReason(st.Err), st.Attempts, next)
		}
		s.Attempted = func(ready bool) {
			if hook := m.attempted.Load(); hook != nil {
				(*hook)(line, ready)
			}
		}
		if err := d.KeepQualifying(ctx, qualify, s); err != nil {
			if last := d.State().Err; last != nil && life.Err() == nil {
				// Boot gave up on a lane it waits for: its last failure says why,
				// as the failed event already logged it.
				return last
			}
			return err
		}
		return nil
	}
	// Declared defaults finish before admission, retrying inside the startup
	// budget. Explicit overrides retain their existing URL-only startup behavior
	// and never enter this qualification path.
	for _, line := range []string{"2.1", "2.2"} {
		d := m.defaults[line]
		if d == nil || !required[line] {
			continue
		}
		if err := qualifyLane(startup, line, d); err != nil {
			return fail(fmt.Errorf("gateway: declared validator line %s at %s failed qualification: %w", line, resolve(line), err))
		}
	}
	// Every other default qualifies in the background, for the gateway's life:
	// a lane that fails keeps retrying on the backoff until it passes.
	for line, d := range m.defaults {
		if required[line] {
			continue
		}
		m.workers.Add(1)
		go func() { defer m.workers.Done(); _ = qualifyLane(life, line, d) }()
	}
	return lanes, m, nil
}

// qualifyDefaultLane is the qualification a default lane passes before routing
// admits it (engine.QualifyValidatorLane).
func qualifyDefaultLane(ctx context.Context, base, line string) error {
	return engine.QualifyValidatorLane(ctx, base, line)
}

// checkTargets are the /internal/checks results for the default lanes, one
// "validator-lane-<line>" each, read from the lane's state: nothing is
// dialed and no attempt is started.
func (m *laneManager) checkTargets() []checks.Target {
	if m == nil {
		return nil
	}
	var out []checks.Target
	for _, line := range telemetry.IGLines {
		d := m.defaults[line]
		if d == nil {
			continue
		}
		out = append(out, checks.Target{ID: "validator-lane-" + line, Kind: checks.KindValidatorLane, URL: d.Base(), LaneState: func() (bool, string) { return laneReport(d.State()) }})
	}
	return out
}

// laneReport is a lane's state as its check reports it: ok once qualified,
// otherwise whether it is still on its first attempt or retrying after
// failures, with the attempt count and the last failure's kind
// (lanequalify.FailureReason, never validator output).
func laneReport(st engine.LaneState) (bool, string) {
	switch {
	case st.Ready:
		return true, ""
	case st.Failures == 0:
		return false, fmt.Sprintf("qualifying (attempt %d)", st.Attempts+1)
	}
	return false, fmt.Sprintf("failed, retrying (%d attempts): %s", st.Attempts, lanequalify.FailureReason(st.Err))
}

// laneGauges is every validator lane's state for the ValidatorLane gauge: the
// default lanes routing admits, and, where certification runs (certifying,
// every level but none), the gated certification clients, of a default lane
// or of a configured address, which qualify on their own. At none no
// certification client starts, so none is reported: a gauge stuck at 0 there
// would read as a lane left unqualified.
func laneGauges(m *laneManager, certification map[string]shnsdk.Validator, certifying bool) []laneGauge {
	var out []laneGauge
	for _, line := range telemetry.IGLines {
		if m != nil {
			if d := m.defaults[line]; d != nil {
				out = append(out, laneGauge{line: line, lane: "routing", ready: d.Ready()})
			}
		}
		if v, ok := certification[line].(*engine.GatedCertificationValidator); ok && certifying {
			out = append(out, laneGauge{line: line, lane: "certification", ready: v.Ready()})
		}
	}
	return out
}
