package app

import (
	"context"
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
