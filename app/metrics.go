package app

import (
	"context"
	"fmt"
	"log"
	"time"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
	metrics "github.com/SmartHealthNetwork/shn-sdk/metrics"
)

// legMetricHook returns the engine.Config.LegMetric callback: one LegOutcome
// count per origination-leg event (dims Service/role/outcome on top of the
// emitter's base Env), plus a LegError rollup on failed|unreachable so the
// per-service leg-error alarm watches a single stream ({Env, Service} dim map —
// no metric math). denied is a policy decision and deliberately NOT an error.
// Split from build() for testability.
func legMetricHook(em *metrics.Emitter, service, role string) func(string) {
	return func(outcome string) {
		em.EmitCount("LegOutcome", 1, map[string]string{"Service": service, "role": role, "outcome": outcome})
		if outcome == engine.LegOutcomeFailed || outcome == engine.LegOutcomeUnreachable {
			em.EmitCount("LegError", 1, map[string]string{"Service": service})
		}
	}
}

// exchangeMetricHook returns an engine.Config.ExchangeObserved callback that
// reports every exchange this gateway answers, both directions, as metrics:
// Exchange {direction, exchange, outcome} and ExchangeLatency
// {direction, exchange}; and, when its participant's own system was called,
// BackendCall {exchange, class} ("ok" for a usable answer), BackendLatency
// {exchange} (when its duration was recorded) and the single-stream BackendError
// rollup of the classes that mean the system gave no usable answer. Every dimension is one of the engine's closed
// lists (engine.ExchangeKinds and the rest), so a gateway reports at most
// exchangeSeriesBound series; the leg's operation, parties and ids never become
// dimensions.
func exchangeMetricHook(em *metrics.Emitter, service string) func(engine.ExchangeRecord) {
	return func(r engine.ExchangeRecord) {
		direction := closedDim(engine.ExchangeDirections, r.Direction)
		exchange := closedDim(engine.ExchangeKinds, r.Exchange)
		em.EmitCount("Exchange", 1, map[string]string{"Service": service, "direction": direction, "exchange": exchange, "outcome": closedDim(engine.ExchangeOutcomes, r.Outcome)})
		em.EmitLatency("ExchangeLatency", float64(r.Latency.Milliseconds()), map[string]string{"Service": service, "direction": direction, "exchange": exchange})
		if r.Backend == nil {
			return
		}
		class := backendOK
		if r.Backend.ErrorClass != "" {
			class = closedDim(engine.BackendErrorClasses, r.Backend.ErrorClass)
		}
		em.EmitCount("BackendCall", 1, map[string]string{"Service": service, "exchange": exchange, "class": class})
		if r.Backend.Latency > 0 {
			// A call whose duration was not recorded reports none, rather than 0 ms.
			em.EmitLatency("BackendLatency", float64(r.Backend.Latency.Milliseconds()), map[string]string{"Service": service, "exchange": exchange})
		}
		if !backendNotAnError[class] {
			em.EmitCount("BackendError", 1, map[string]string{"Service": service})
		}
	}
}

// backendOK is the BackendCall class of a usable answer from the participant's
// own system.
const backendOK = "ok"

// backendNotAnError are the BackendCall classes that do not count in
// BackendError: those in which the participant's system gave an answer of its
// own, and a call the request's end cut short, the requester leaving before
// the gateway's deadline for that system (a system slower than the deadline
// is its timeout, which counts). Every other class counts.
var backendNotAnError = map[string]bool{backendOK: true, engine.BackendHTTP3xx: true, engine.BackendHTTP4xx: true, engine.BackendCancelled: true}

// exchangeSeriesBound is the most series exchangeMetricHook can report for one
// gateway: Exchange, ExchangeLatency, BackendCall (the error classes plus ok),
// BackendLatency and BackendError. TestExchangeMetricBound pins it to the lists.
var exchangeSeriesBound = len(engine.ExchangeDirections)*len(engine.ExchangeKinds)*len(engine.ExchangeOutcomes) +
	len(engine.ExchangeDirections)*len(engine.ExchangeKinds) +
	len(engine.ExchangeKinds)*(len(engine.BackendErrorClasses)+1) +
	len(engine.ExchangeKinds) + 1

// closedDim is v when list names it, else the list's catch-all.
func closedDim(list []string, v string) string {
	for _, x := range list {
		if x == v {
			return v
		}
	}
	return engine.ExchangeOther
}

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
// StoreError count per shared-state store failure, with the failing store as a
// dimension on top of the emitter's base Env. The seams differ in what a failure
// costs — the exchange correlation seam is best-effort and never fails the request it
// rode in on, while the replay record and the ingress signing key refuse the request
// with a 503 — but every path that absorbs or refuses on a store error
// counts here, so one counter (dimensioned by store) is what makes an outage visible
// and attributable. engine.Config.StoreErrorMetric's doc lists every site, including
// the two the Postgres stores report through their own error hook. Split from build()
// for testability, exactly like legMetricHook above.
func storeErrorMetricHook(em *metrics.Emitter, service string) func(store string) {
	return func(store string) {
		em.EmitCount("StoreError", 1, map[string]string{"Service": service, "store": store})
	}
}

// involvedOmittedMetricHook counts each involved patient a leg left out of its
// list, by reason (engine.Config.InvolvedMetric).
func involvedOmittedMetricHook(em *metrics.Emitter, service string) func(reason string) {
	return func(reason string) {
		em.EmitCount("InvolvedOmitted", 1, map[string]string{"Service": service, "reason": reason})
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

// emitStorePool emits one StorePool gauge per stat, each dimensioned by the stat name on
// top of the emitter's base Env, so a per-stat alarm watches a single stream. Counts, not
// latencies: EmptyAcquireCount is cumulative for the process, so an alarm reads its rate.
func emitStorePool(em *metrics.Emitter, service string, st storePoolStat) {
	for _, s := range []struct {
		name string
		val  float64
	}{
		{"TotalConns", float64(st.TotalConns)},
		{"AcquiredConns", float64(st.AcquiredConns)},
		{"IdleConns", float64(st.IdleConns)},
		{"EmptyAcquireCount", float64(st.EmptyAcquireCount)},
	} {
		em.EmitGauge("StorePool", s.val, "Count", map[string]string{"Service": service, "stat": s.name})
	}
}

// storePoolMetricLoop returns the Run-time sampler for the shared-state pool: one
// emission immediately (so a short-lived task still reports once) and then every
// interval, until ctx is done. build() never starts it — Run does, beside the ingress
// key refresh, for the same reason: the boot gate drives build() and must not inherit a
// ticker. Fire-and-forget like every other emission here; it never fails a request.
func storePoolMetricLoop(em *metrics.Emitter, service string, stat func() storePoolStat, interval time.Duration) func(context.Context) {
	return func(ctx context.Context) {
		t := time.NewTicker(interval)
		defer t.Stop()
		emitStorePool(em, service, stat())
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				emitStorePool(em, service, stat())
			}
		}
	}
}
