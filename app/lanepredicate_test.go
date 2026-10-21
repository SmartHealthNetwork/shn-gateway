package app

import (
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine"
)

// Boot's copy of the engine's predicate answers the same table: payload
// conformance checks consume validator lanes at every level but none.
func TestPayloadChecksConsumeLanes(t *testing.T) {
	for level, want := range map[engine.ConformanceEnforcement]bool{
		engine.EnforcementNone: false, engine.EnforcementObserve: true, engine.EnforcementStructural: true, engine.EnforcementStrict: true,
	} {
		if got := payloadChecksConsumeLanes(level); got != want {
			t.Errorf("payloadChecksConsumeLanes(%s) = %v, want %v", level, got, want)
		}
	}
}
