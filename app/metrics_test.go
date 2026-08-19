package app

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
	metrics "github.com/SmartHealthNetwork/shn-sdk/metrics"
)

// decodeEMFLines parses each stdout line as JSON and returns the per-line maps.
func decodeEMFLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ln := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if ln == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("non-JSON EMF line %q: %v", ln, err)
		}
		out = append(out, m)
	}
	return out
}

// TestLegMetricHook_EmitsLegOutcomeAndErrorRollup: every outcome emits one
// LegOutcome line with dims {Env, Service, outcome, role}; failed and
// unreachable ALSO emit the single-stream LegError rollup the per-service
// alarms watch (denied deliberately does not — a policy decision, not an error).
func TestLegMetricHook_EmitsLegOutcomeAndErrorRollup(t *testing.T) {
	var buf bytes.Buffer
	em := metrics.New(&buf, "SHN/Preview", map[string]string{"Env": "shn-preview"}, nil)
	hook := legMetricHook(em, "provider-data-gw", "provider")

	for _, o := range []string{
		engine.LegOutcomeRouted, engine.LegOutcomeAnswered, engine.LegOutcomeDenied,
		engine.LegOutcomeFailed, engine.LegOutcomeUnreachable,
	} {
		hook(o)
	}

	lines := decodeEMFLines(t, &buf)
	var legOutcome, legError int
	for _, m := range lines {
		switch {
		case m["LegOutcome"] != nil:
			legOutcome++
			if m["Service"] != "provider-data-gw" || m["Env"] != "shn-preview" || m["role"] != "provider" {
				t.Fatalf("LegOutcome dims wrong: %v", m)
			}
		case m["LegError"] != nil:
			legError++
			if m["Service"] != "provider-data-gw" || m["Env"] != "shn-preview" {
				t.Fatalf("LegError dims wrong: %v", m)
			}
			if _, hasRole := m["role"]; hasRole {
				t.Fatalf("LegError must NOT carry role (single-stream alarm dim map {Env,Service}): %v", m)
			}
		}
	}
	if legOutcome != 5 {
		t.Fatalf("want 5 LegOutcome lines, got %d", legOutcome)
	}
	if legError != 2 {
		t.Fatalf("want 2 LegError lines (failed+unreachable only), got %d", legError)
	}
}
