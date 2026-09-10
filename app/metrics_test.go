package app

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

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

// TestStoreErrorMetricHook_EmitsStoreErrorWithStoreDim: a shared-state store
// failure emits one StoreError count carrying the failing store as a dimension
// (on top of the emitter's base Env), so an outage of one seam is visible and
// attributable rather than silently absorbed by the best-effort store paths.
func TestStoreErrorMetricHook_EmitsStoreErrorWithStoreDim(t *testing.T) {
	var buf bytes.Buffer
	em := metrics.New(&buf, "SHN/Test", map[string]string{"Env": "test"}, func() time.Time { return time.Unix(0, 0) })

	storeErrorMetricHook(em, "provider-gw")("exchange")

	lines := decodeEMFLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("want 1 EMF line, got %d: %v", len(lines), lines)
	}
	m := lines[0]
	if m["StoreError"] != float64(1) {
		t.Fatalf("StoreError = %v, want 1: %v", m["StoreError"], m)
	}
	if m["Service"] != "provider-gw" || m["store"] != "exchange" || m["Env"] != "test" {
		t.Fatalf("StoreError dims wrong: %v", m)
	}
}

// TestStorePoolMetric_EmitsOneGaugePerStat: the shared-state pool is the resource every
// seam contends for, so its saturation must be visible BEFORE the store context starts
// expiring. One StorePool gauge per stat, each carrying the stat name as a dimension on
// top of the emitter's base Env, so one alarm can watch a single stream per stat.
func TestStorePoolMetric_EmitsOneGaugePerStat(t *testing.T) {
	var buf bytes.Buffer
	em := metrics.New(&buf, "SHN/Test", map[string]string{"Env": "test"}, func() time.Time { return time.Unix(0, 0) })

	emitStorePool(em, "provider-gw", storePoolStat{
		TotalConns: 7, AcquiredConns: 3, IdleConns: 4, EmptyAcquireCount: 11,
	})

	got := map[string]float64{}
	for _, m := range decodeEMFLines(t, &buf) {
		v, ok := m["StorePool"].(float64)
		if !ok {
			t.Fatalf("line is not a StorePool gauge: %v", m)
		}
		if m["Service"] != "provider-gw" || m["Env"] != "test" {
			t.Fatalf("StorePool dims wrong: %v", m)
		}
		stat, _ := m["stat"].(string)
		if stat == "" {
			t.Fatalf("StorePool line carries no stat dimension: %v", m)
		}
		got[stat] = v
	}
	want := map[string]float64{"TotalConns": 7, "AcquiredConns": 3, "IdleConns": 4, "EmptyAcquireCount": 11}
	if len(got) != len(want) {
		t.Fatalf("StorePool stats = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("StorePool[%s] = %v, want %v (a mis-mapped field reads as a healthy pool)", k, got[k], v)
		}
	}
}

// The pool-stat loop is a Run-time goroutine like the key refresh: it must sample on its
// own cadence and return on the run context, or a shut-down gateway keeps a ticker (and
// a closed pool) alive. Deterministic: the first sample is taken before any tick.
func TestStorePoolMetricLoop_SamplesThenReturnsOnCtxDone(t *testing.T) {
	var buf lockedBuffer
	em := metrics.New(&buf, "SHN/Test", map[string]string{"Env": "test"}, func() time.Time { return time.Unix(0, 0) })
	sampled := make(chan struct{}, 1)
	loop := storePoolMetricLoop(em, "provider-gw", func() storePoolStat {
		select {
		case sampled <- struct{}{}:
		default:
		}
		return storePoolStat{TotalConns: 1}
	}, time.Hour) // a cadence no test waits for: the FIRST sample is taken immediately

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { loop(ctx); close(done) }()
	<-sampled
	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second): // liveness fence only — the return is immediate
		t.Fatal("the pool-stat loop did not return on a cancelled context")
	}
	if !strings.Contains(buf.String(), `"StorePool"`) {
		t.Fatalf("no StorePool line was emitted: %q", buf.String())
	}
}

// lockedBuffer is a bytes.Buffer safe for the loop goroutine to write while the test reads.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}
