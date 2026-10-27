//go:build !shn_coreonly

package app

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
	"github.com/SmartHealthNetwork/shn-gateway/telemetry"
)

// The hooks below record into the telemetry registry, the one recording point
// (MON-R16); its sinks decide what reaches EMF and OpenTelemetry. Each is
// split from build() for testability.

// legMetricHook returns the engine.Config.LegMetric callback: one
// shn.gateway.legs count per origination-leg event. In EMF that is LegOutcome
// {Service, role, outcome}, plus the LegError rollup on failed|unreachable so
// the per-service leg-error alarm watches a single stream; denied is a policy
// decision and deliberately NOT an error.
func legMetricHook(reg *telemetry.Registry) func(string) {
	return func(outcome string) {
		reg.Add(telemetry.Legs, 1, outcome)
	}
}

// exchangeMetricHook returns an engine.Config.ExchangeObserved callback that
// reports every exchange this gateway answers, both directions:
// shn.gateway.exchanges {direction, exchange, outcome} and
// shn.gateway.exchange.duration {direction, exchange}; and, when its
// participant's own system was called, shn.gateway.backend.calls {exchange,
// class} ("ok" for a usable answer) and shn.gateway.backend.duration
// {exchange} (when its duration was recorded). In EMF those are Exchange,
// ExchangeLatency, BackendCall, BackendLatency and the single-stream
// BackendError rollup of the classes that mean the system gave no usable
// answer. Every attribute is one of the engine's closed lists
// (engine.ExchangeKinds and the rest), so a gateway reports at most
// exchangeSeriesBound EMF series; the leg's operation, parties and ids never
// become attributes.
//
// It also reports the exchange additions: a refusal by its rule and the party
// that refused (shn.gateway.refusals, MON-R21); the time in each stage the
// exchange entered (shn.gateway.exchange.stage.duration, exchangeStages,
// MON-R22); and its request and answer body sizes when they are known
// (MON-R23).
func exchangeMetricHook(reg *telemetry.Registry) func(engine.ExchangeRecord) {
	return func(r engine.ExchangeRecord) {
		reg.Add(telemetry.Exchanges, 1, r.Direction, r.Exchange, r.Outcome)
		reg.RecordDuration(telemetry.ExchangeDuration, r.Latency, r.Direction, r.Exchange)
		if r.Outcome == engine.ExchangeRefused {
			reg.Add(telemetry.Refusals, 1, r.Direction, r.Exchange, r.Rule, r.RefusedBy)
		}
		for _, st := range exchangeStages(r) {
			reg.RecordDuration(telemetry.ExchangeStageDuration, st.d, r.Direction, r.Exchange, st.stage)
		}
		if r.RequestBytes != nil {
			reg.Record(telemetry.ExchangeRequestSize, float64(*r.RequestBytes), r.Direction, r.Exchange)
		}
		if r.ResponseBytes != nil {
			reg.Record(telemetry.ExchangeResponseSize, float64(*r.ResponseBytes), r.Direction, r.Exchange)
		}
		if r.Backend == nil {
			return
		}
		class := telemetry.BackendOK
		if r.Backend.ErrorClass != "" {
			class = r.Backend.ErrorClass
			if class == telemetry.BackendOK {
				// An error class is never the usable answer's class.
				class = engine.ExchangeOther
			}
		}
		reg.Add(telemetry.BackendCalls, 1, r.Exchange, class)
		if r.Backend.Latency > 0 {
			// A call whose duration was not recorded reports none, rather than 0 ms.
			reg.RecordDuration(telemetry.BackendDuration, r.Backend.Latency, r.Exchange)
		}
	}
}

// stageTime is the time an exchange spent in one stage.
type stageTime struct {
	stage string
	d     time.Duration
}

// exchangeStages is where an exchange's time went (MON-R22), each stage it
// entered: on an inbound leg, its seven measured stages (engine.LegStages)
// and rest, the latency none of them holds; on an ingress call, its network
// wait (engine.ExchangeRecord.Network) and self, the latency less that wait.
// The stages sum to the exchange's latency. A stage the exchange did not
// enter (0) is not reported, nor is a derived stage of 0; a derived stage is
// never negative. An inbound leg whose stages were not recorded, or a
// direction outside the closed list, reports none: its split is not known.
func exchangeStages(r engine.ExchangeRecord) []stageTime {
	var out []stageTime
	add := func(stage string, d time.Duration) {
		if d > 0 {
			out = append(out, stageTime{stage, d})
		}
	}
	switch r.Direction {
	case engine.DirectionIngress:
	case engine.DirectionInbound:
		if r.Stages == nil {
			return nil
		}
	default:
		return nil
	}
	if st := r.Stages; r.Direction == engine.DirectionInbound {
		measured := []stageTime{
			{telemetry.StageUnwrap, st.Unwrap}, {telemetry.StageReads, st.Reads}, {telemetry.StageForward, st.Forward},
			{telemetry.StageValidate, st.Validate}, {telemetry.StageSeal, st.Seal}, {telemetry.StageLedger, st.Ledger},
			{telemetry.StageWrite, st.Write},
		}
		rest := r.Latency
		for _, m := range measured {
			add(m.stage, m.d)
			rest -= m.d
		}
		add(telemetry.StageRest, rest)
		return out
	}
	self := r.Latency
	if n := r.Network; n != nil {
		add(telemetry.StageNetwork, n.Latency)
		self -= n.Latency
	}
	add(telemetry.StageSelf, self)
	return out
}

// activeRequestHook returns the engine.Config.ActiveRequestMetric callback:
// shn.gateway.active_requests {direction} moves by each call's arrival and
// answer (MON-R23). In EMF its peak each minute is the ActiveRequests gauge
// (activeRequestsMetricLoop).
func activeRequestHook(reg *telemetry.Registry) func(direction string, delta int) {
	return func(direction string, delta int) {
		reg.Add(telemetry.ActiveRequests, float64(delta), direction)
	}
}

// activeRequestsMetricLoop writes the EMF ActiveRequests gauge, the peak in
// flight since the last sample, every interval until ctx is done: the first
// sample covers the first interval. Run starts it, never build().
func activeRequestsMetricLoop(emf *telemetry.EMF, interval time.Duration) func(context.Context) {
	return func(ctx context.Context) {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				emf.SampleActiveRequests()
			}
		}
	}
}

// findingMetricHook returns the engine.Config.FindingMetric callback: one
// shn.gateway.conformance.findings count per finding recorded, with its leg,
// level, kind, recorded decision and tolerated flag (MON-R24).
func findingMetricHook(reg *telemetry.Registry) func(engine.FindingMetric) {
	return func(f engine.FindingMetric) {
		reg.Add(telemetry.ConformanceFindings, 1, f.Exchange, f.Level, f.Kind, f.Action, strconv.FormatBool(f.Tolerated))
	}
}

// validatorMetricHook returns the engine.Config.ValidatorMetric callback:
// shn.gateway.validator.duration {ig_line, lane} per $validate call
// (MON-R24).
func validatorMetricHook(reg *telemetry.Registry) func(line, lane string, took time.Duration) {
	return func(line, lane string, took time.Duration) {
		reg.RecordDuration(telemetry.ValidatorDuration, took, line, lane)
	}
}

// observeCheckDroppedHook returns the engine.Config.ObserveCheckDroppedMetric
// callback: shn.gateway.conformance.checks.dropped (MON-R24).
func observeCheckDroppedHook(reg *telemetry.Registry) func() {
	return func() { reg.Add(telemetry.ConformanceChecksDropped, 1) }
}

// exchangeSeriesBound is the most EMF series exchangeMetricHook can report for
// one gateway: Exchange, ExchangeLatency, BackendCall (the error classes plus
// ok), BackendLatency and BackendError; and the exchange additions:
// ExchangeRefused (without the exchange, which only its OpenTelemetry
// attributes carry) and ExchangeRefusedByRule, ExchangeStageLatency (an inbound
// leg's seven stages and rest, an ingress call's network and self) and the two
// body sizes. TestExchangeMetricBound pins it to the lists.
var exchangeSeriesBound = len(engine.ExchangeDirections)*len(engine.ExchangeKinds)*len(engine.ExchangeOutcomes) +
	len(engine.ExchangeDirections)*len(engine.ExchangeKinds) +
	len(engine.ExchangeKinds)*(len(engine.BackendErrorClasses)+1) +
	len(engine.ExchangeKinds) + 1 +
	len(engine.ExchangeDirections)*len(engine.RefusalRules)*len(engine.RefusalParties) +
	len(engine.RefusalRules) +
	len(engine.ExchangeKinds)*(8+2) +
	2*len(engine.ExchangeDirections)*len(engine.ExchangeKinds)

// conformanceSeriesBound is the most EMF series the other metrics added with
// the exchange additions can report for one gateway, beside
// exchangeSeriesBound: ConformanceFinding by level, kind, action and tolerated
// (a bool's own spelling, never other), without the exchange, as
// ExchangeRefused is; and ValidatorLatency by line and lane, each other
// dimension folding a value outside its list to other (MON-R24);
// ActiveRequests by direction (MON-R23); and ObserveCheckDropped.
// TestConformanceMetricBound pins it to the lists.
var conformanceSeriesBound = (len(engine.ConformanceLevels)+1)*(len(engine.CheckKinds)+1)*
	(len(engine.FindingActions)+1)*len(telemetry.Booleans) +
	(len(telemetry.IGLines)+1)*(len(telemetry.LaneKinds)+1) +
	len(engine.ExchangeDirections) - 1 +
	1

// exchangeObservers joins the ExchangeObserved consumers into one callback,
// each run in order; nil when there are none, so the engine skips recording.
// A consumer that panics is recovered alone: the ones after it still run.
func exchangeObservers(fs ...func(engine.ExchangeRecord)) func(engine.ExchangeRecord) {
	var live []func(engine.ExchangeRecord)
	for _, f := range fs {
		if f != nil {
			live = append(live, f)
		}
	}
	if len(live) == 0 {
		return nil
	}
	return func(r engine.ExchangeRecord) {
		for _, f := range live {
			func() {
				defer func() {
					if p := recover(); p != nil {
						// The panic's value, quoted so nothing it carries can
						// start a line of its own.
						log.Printf("gateway: an exchange observer panicked: %q", fmt.Sprint(p))
					}
				}()
				f(r)
			}()
		}
	}
}

// storeErrorMetricHook returns the engine.Config.StoreErrorMetric callback: one
// shn.gateway.store.errors count (EMF StoreError) per shared-state store
// failure, with the failing store as an attribute. The seams differ in what a
// failure costs — the exchange correlation seam is best-effort and never fails
// the request it rode in on, while the replay record and the ingress signing key
// refuse the request with a 503 — but every path that absorbs or refuses on a
// store error counts here, so one counter (by store) is what makes an outage
// visible and attributable. engine.Config.StoreErrorMetric's doc lists every
// site, including the two the Postgres stores report through their own error
// hook.
func storeErrorMetricHook(reg *telemetry.Registry) func(store string) {
	return func(store string) {
		reg.Add(telemetry.StoreErrors, 1, store)
	}
}

// involvedOmittedMetricHook counts each involved patient a leg left out of its
// list, by reason (engine.Config.InvolvedMetric).
func involvedOmittedMetricHook(reg *telemetry.Registry) func(reason string) {
	return func(reason string) {
		reg.Add(telemetry.InvolvedOmitted, 1, reason)
	}
}

// tokenMetricHook returns the engine.Config.TokenMetric callback: each
// Authorization Framework token acquisition's duration
// (shn.gateway.authorization.token.duration, EMF TokenLatency) and one
// count by outcome (shn.gateway.authorization.tokens, EMF TokenAcquisition).
func tokenMetricHook(reg *telemetry.Registry) func(outcome string, d time.Duration) {
	return func(outcome string, d time.Duration) {
		reg.RecordDuration(telemetry.TokenLatency, d)
		reg.Add(telemetry.TokenAcquisition, 1, outcome)
	}
}

// storePoolStat is the slice of pgxpool.Stat this gateway reports. Named fields rather
// than the pgxpool type so the emission is testable without a database (pgxpool.Stat's
// fields are unexported and it can only be produced by a live pool).
type storePoolStat struct {
	TotalConns        int32 // open connections (idle + acquired + constructing)
	AcquiredConns     int32 // in use right now
	IdleConns         int32 // open and free
	EmptyAcquireCount int64 // acquires that had to WAIT for a connection — the saturation signal
}

// emitStorePool records the pool's connections by state and its lifetime count of
// acquires that waited. In EMF each is one StorePool gauge dimensioned by the stat name,
// so a per-stat alarm watches a single stream; EmptyAcquireCount is cumulative for the
// process, so an alarm reads its rate.
func emitStorePool(reg *telemetry.Registry, st storePoolStat) {
	reg.Record(telemetry.StorePoolConnections, float64(st.TotalConns), "total")
	reg.Record(telemetry.StorePoolConnections, float64(st.AcquiredConns), "acquired")
	reg.Record(telemetry.StorePoolConnections, float64(st.IdleConns), "idle")
	reg.Record(telemetry.StorePoolEmptyAcquires, float64(st.EmptyAcquireCount))
}

// storePoolMetricLoop returns the Run-time sampler for the shared-state pool: one
// emission immediately (so a short-lived task still reports once) and then every
// interval, until ctx is done. build() never starts it — Run does, beside the ingress
// key refresh, for the same reason: the boot gate drives build() and must not inherit a
// ticker. Fire-and-forget like every other emission here; it never fails a request.
func storePoolMetricLoop(reg *telemetry.Registry, stat func() storePoolStat, interval time.Duration) func(context.Context) {
	return func(ctx context.Context) {
		t := time.NewTicker(interval)
		defer t.Stop()
		emitStorePool(reg, stat())
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				emitStorePool(reg, stat())
			}
		}
	}
}

// auditMetrics reports the audit outbox (auditappend.Metrics):
// shn.gateway.audit.appends; .retries {reason} and .drops {reason}, each
// reason one of auditappend's closed lists; .queue.depth, a gauge each
// minute; and .endpoint.unknown, the depth each minute while discovery names
// no audit endpoint. In EMF a drop also writes AuditAppendDropped, the alarm
// metric: the rollup of every drop on one {Env, Service} stream (the LegError
// precedent: a single-stream dimension map, no metric math): a record left
// the outbox without the Plane's acceptance. A registry with no sink records
// nothing; the unknown endpoint still shows in /internal/checks and at boot.
type auditMetrics struct {
	reg *telemetry.Registry
}

func (m auditMetrics) Sent() { m.reg.Add(telemetry.AuditAppends, 1) }

func (m auditMetrics) Retried(reason string) { m.reg.Add(telemetry.AuditRetries, 1, reason) }

func (m auditMetrics) Dropped(reason string, n int) {
	m.reg.Add(telemetry.AuditDrops, float64(n), reason)
}

func (m auditMetrics) Depth(n int) { m.reg.Record(telemetry.AuditQueueDepth, float64(n)) }

func (m auditMetrics) EndpointUnknown(depth int) {
	m.reg.Record(telemetry.AuditEndpointUnknown, float64(depth))
}

// laneGauge is one validator lane's qualification for the ValidatorLane gauge:
// its line, which kind of lane it is ("routing", the default lane routing
// admits; "certification", the client module 3 certifies through), and
// whether it is qualified.
type laneGauge struct {
	line, lane string
	ready      bool
}

// emitValidatorLanes records one shn.gateway.validator.lane.qualified gauge
// (EMF ValidatorLane) per lane, 1 when it is qualified and 0 when not, so an
// alarm on a 0 that lasts sees a lane left unqualified rather than a quiet
// log.
func emitValidatorLanes(reg *telemetry.Registry, lanes []laneGauge) {
	for _, l := range lanes {
		v := 0.0
		if l.ready {
			v = 1
		}
		reg.Record(telemetry.ValidatorLaneQualified, v, l.line, l.lane)
	}
}

// validatorLaneAttemptHook counts each qualification attempt of a default
// lane by its outcome, "ready" or "failed".
func validatorLaneAttemptHook(reg *telemetry.Registry) func(line string, ready bool) {
	return func(line string, ready bool) {
		outcome := "failed"
		if ready {
			outcome = "ready"
		}
		reg.Add(telemetry.ValidatorLaneAttempts, 1, line, "routing", outcome)
	}
}

// validatorLaneMetricLoop records the lanes' gauges at once and then every
// interval until ctx is done, as storePoolMetricLoop does for the pool.
func validatorLaneMetricLoop(reg *telemetry.Registry, lanes func() []laneGauge, interval time.Duration) func(context.Context) {
	return func(ctx context.Context) {
		t := time.NewTicker(interval)
		defer t.Stop()
		emitValidatorLanes(reg, lanes())
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				emitValidatorLanes(reg, lanes())
			}
		}
	}
}
