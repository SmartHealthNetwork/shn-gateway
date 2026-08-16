package app

import (
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
