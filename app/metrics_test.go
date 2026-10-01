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

// An involved patient a leg left out counts once in InvolvedOmitted, with the
// reason as a dimension, so an operator sees what the audit record lacks.
func TestInvolvedOmittedMetricHook_EmitsReasonDim(t *testing.T) {
	var buf bytes.Buffer
	em := metrics.New(&buf, "SHN/Test", map[string]string{"Env": "test"}, func() time.Time { return time.Unix(0, 0) })

	involvedOmittedMetricHook(em, "payer-gw")("authorization-refused")

	lines := decodeEMFLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("want 1 EMF line, got %d: %v", len(lines), lines)
	}
	m := lines[0]
	if m["InvolvedOmitted"] != float64(1) || m["Service"] != "payer-gw" || m["reason"] != "authorization-refused" || m["Env"] != "test" {
		t.Fatalf("InvolvedOmitted line wrong: %v", m)
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

// TestExchangeMetricHook: an exchange emits Exchange and ExchangeLatency; a call
// to the participant's own system adds BackendCall, BackendLatency and, when the
// system gave no answer of its own, BackendError. Values outside the engine's
// closed lists fold into "other", and nothing else of the record (operation,
// parties, ids) becomes a dimension.
func TestExchangeMetricHook(t *testing.T) {
	for _, c := range []struct {
		name        string
		rec         engine.ExchangeRecord
		wantLines   map[string]map[string]string // metric → dims beyond Env and Service
		backendErr  bool
		exchangeLat float64
	}{
		{
			name: "payer answered from its backend",
			rec: engine.ExchangeRecord{Direction: engine.DirectionInbound, Exchange: "pas-claim", Outcome: engine.ExchangeAnswered, Latency: 1500 * time.Millisecond,
				Backend: &engine.BackendCall{Status: 200, Latency: 900 * time.Millisecond}},
			wantLines: map[string]map[string]string{
				"Exchange":        {"direction": "inbound", "exchange": "pas-claim", "outcome": "answered"},
				"ExchangeLatency": {"direction": "inbound", "exchange": "pas-claim"},
				"BackendCall":     {"exchange": "pas-claim", "class": "ok"},
				"BackendLatency":  {"exchange": "pas-claim"},
			},
			exchangeLat: 1500,
		},
		{
			name: "payer backend call cut short by the request's end",
			rec: engine.ExchangeRecord{Direction: engine.DirectionInbound, Exchange: "pas-claim", Outcome: engine.ExchangeOther, Latency: 29 * time.Second,
				Backend: &engine.BackendCall{Latency: 29 * time.Second, ErrorClass: engine.BackendCancelled}},
			wantLines: map[string]map[string]string{
				"Exchange":        {"direction": "inbound", "exchange": "pas-claim", "outcome": "other"},
				"ExchangeLatency": {"direction": "inbound", "exchange": "pas-claim"},
				"BackendCall":     {"exchange": "pas-claim", "class": "cancelled"},
				"BackendLatency":  {"exchange": "pas-claim"},
			},
			exchangeLat: 29000,
		},
		{
			name: "payer backend unreachable",
			rec: engine.ExchangeRecord{Direction: engine.DirectionInbound, Exchange: "crd-order-select", Outcome: engine.ExchangeUpstreamError, Latency: 30 * time.Second,
				Backend: &engine.BackendCall{Latency: 30 * time.Second, ErrorClass: engine.BackendTimeout}},
			wantLines: map[string]map[string]string{
				"Exchange":        {"direction": "inbound", "exchange": "crd-order-select", "outcome": "upstream-error"},
				"ExchangeLatency": {"direction": "inbound", "exchange": "crd-order-select"},
				"BackendCall":     {"exchange": "crd-order-select", "class": "timeout"},
				"BackendLatency":  {"exchange": "crd-order-select"},
			},
			backendErr:  true,
			exchangeLat: 30000,
		},
		{
			name: "payer backend answered 4xx",
			rec: engine.ExchangeRecord{Direction: engine.DirectionInbound, Exchange: "dtr-questionnaire-fetch", Outcome: engine.ExchangeUpstreamError,
				Backend: &engine.BackendCall{Status: 404, Latency: 40 * time.Millisecond, ErrorClass: engine.BackendHTTP4xx}},
			wantLines: map[string]map[string]string{
				"Exchange":        {"direction": "inbound", "exchange": "dtr-questionnaire-fetch", "outcome": "upstream-error"},
				"ExchangeLatency": {"direction": "inbound", "exchange": "dtr-questionnaire-fetch"},
				"BackendCall":     {"exchange": "dtr-questionnaire-fetch", "class": "http-4xx"},
				"BackendLatency":  {"exchange": "dtr-questionnaire-fetch"},
			},
		},
		{
			name: "backend failure with no recorded duration",
			rec: engine.ExchangeRecord{Direction: engine.DirectionInbound, Exchange: "coverage-eligibility", Outcome: engine.ExchangeUpstreamError,
				Backend: &engine.BackendCall{ErrorClass: engine.ExchangeOther}},
			wantLines: map[string]map[string]string{
				"Exchange":        {"direction": "inbound", "exchange": "coverage-eligibility", "outcome": "upstream-error"},
				"ExchangeLatency": {"direction": "inbound", "exchange": "coverage-eligibility"},
				"BackendCall":     {"exchange": "coverage-eligibility", "class": "other"},
			},
			backendErr: true,
		},
		{
			// The payer gateway's own work spent its deadline and the operation
			// was not sent: the backend is the last read, here the CDS service
			// listing, which answered.
			name: "deadline spent after a usable read",
			rec: engine.ExchangeRecord{Direction: engine.DirectionInbound, Exchange: "crd-order-select", Outcome: engine.ExchangeOther, Latency: 25 * time.Second,
				Backend: &engine.BackendCall{Status: 200, Latency: 80 * time.Millisecond}},
			wantLines: map[string]map[string]string{
				"Exchange":        {"direction": "inbound", "exchange": "crd-order-select", "outcome": "other"},
				"ExchangeLatency": {"direction": "inbound", "exchange": "crd-order-select"},
				"BackendCall":     {"exchange": "crd-order-select", "class": "ok"},
				"BackendLatency":  {"exchange": "crd-order-select"},
			},
			exchangeLat: 25000,
		},
		{
			// The same, when the listing's re-read failed and the gateway used
			// the listing it last read: that failed read is the backend, and it
			// counts in BackendError although the operation was never sent.
			name: "deadline spent after a failed listing re-read",
			rec: engine.ExchangeRecord{Direction: engine.DirectionInbound, Exchange: "crd-order-select", Outcome: engine.ExchangeOther, Latency: 25 * time.Second,
				Backend: &engine.BackendCall{Status: 503, Latency: 80 * time.Millisecond, ErrorClass: engine.BackendHTTP5xx}},
			wantLines: map[string]map[string]string{
				"Exchange":        {"direction": "inbound", "exchange": "crd-order-select", "outcome": "other"},
				"ExchangeLatency": {"direction": "inbound", "exchange": "crd-order-select"},
				"BackendCall":     {"exchange": "crd-order-select", "class": "http-5xx"},
				"BackendLatency":  {"exchange": "crd-order-select"},
			},
			backendErr:  true,
			exchangeLat: 25000,
		},
		{
			name: "refused before the backend",
			rec: engine.ExchangeRecord{Direction: engine.DirectionIngress, Exchange: "pas-claim", Outcome: engine.ExchangeRefused, RefusedBy: engine.RefusedByProviderGateway, Rule: engine.RefusalAuthentication,
				Operation: "order-sign", Sender: "holder-a", Recipient: "holder-b", CorrelationID: "leg-1", Trace: "trace-1"},
			wantLines: map[string]map[string]string{
				"Exchange":        {"direction": "ingress", "exchange": "pas-claim", "outcome": "refused"},
				"ExchangeLatency": {"direction": "ingress", "exchange": "pas-claim"},
			},
		},
		{
			name: "values outside the closed lists",
			rec: engine.ExchangeRecord{Direction: "sideways", Exchange: "caller-chosen", Outcome: "made-up",
				Backend: &engine.BackendCall{Latency: time.Millisecond, ErrorClass: "novel"}},
			wantLines: map[string]map[string]string{
				"Exchange":        {"direction": "other", "exchange": "other", "outcome": "other"},
				"ExchangeLatency": {"direction": "other", "exchange": "other"},
				"BackendCall":     {"exchange": "other", "class": "other"},
				"BackendLatency":  {"exchange": "other"},
			},
			backendErr: true,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			em := metrics.New(&buf, "SHN/Preview", map[string]string{"Env": "shn-preview"}, nil)
			exchangeMetricHook(em, "shn-cloud-acme")(c.rec)
			got := map[string]map[string]any{}
			for _, m := range decodeEMFLines(t, &buf) {
				aws := m["_aws"].(map[string]any)["CloudWatchMetrics"].([]any)[0].(map[string]any)
				name := aws["Metrics"].([]any)[0].(map[string]any)["Name"].(string)
				if _, dup := got[name]; dup {
					t.Fatalf("%s emitted twice", name)
				}
				got[name] = m
			}
			want := len(c.wantLines)
			if c.backendErr {
				want++
			}
			if len(got) != want {
				t.Fatalf("metrics=%v want %d", keysOf(got), want)
			}
			for name, dims := range c.wantLines {
				m, ok := got[name]
				if !ok {
					t.Fatalf("%s not emitted", name)
				}
				dimNames := m["_aws"].(map[string]any)["CloudWatchMetrics"].([]any)[0].(map[string]any)["Dimensions"].([]any)[0].([]any)
				if len(dimNames) != len(dims)+2 {
					t.Errorf("%s dimensions=%v want Env, Service and %v", name, dimNames, dims)
				}
				if m["Env"] != "shn-preview" || m["Service"] != "shn-cloud-acme" {
					t.Errorf("%s base dims: %v", name, m)
				}
				for k, v := range dims {
					if m[k] != v {
						t.Errorf("%s %s=%v want %s", name, k, m[k], v)
					}
				}
			}
			if c.exchangeLat != 0 && got["ExchangeLatency"]["ExchangeLatency"] != c.exchangeLat {
				t.Errorf("ExchangeLatency=%v want %v ms", got["ExchangeLatency"]["ExchangeLatency"], c.exchangeLat)
			}
			if be, ok := got["BackendError"]; ok != c.backendErr {
				t.Errorf("BackendError emitted=%v want %v", ok, c.backendErr)
			} else if ok && (be["Service"] != "shn-cloud-acme" || len(be["_aws"].(map[string]any)["CloudWatchMetrics"].([]any)[0].(map[string]any)["Dimensions"].([]any)[0].([]any)) != 2) {
				t.Errorf("BackendError must be one stream per gateway: %v", be)
			}
			for _, leak := range []string{"order-sign", "holder-a", "holder-b", "leg-1", "trace-1"} {
				if strings.Contains(buf.String(), leak) {
					t.Errorf("%q reached a metric line", leak)
				}
			}
		})
	}
}

// TestExchangeMetricBound: every combination of the engine's closed lists, and
// nothing beyond them, is what a gateway can report; the documented bound is
// that count.
func TestExchangeMetricBound(t *testing.T) {
	var buf bytes.Buffer
	em := metrics.New(&buf, "SHN/Preview", map[string]string{"Env": "shn-preview"}, nil)
	hook := exchangeMetricHook(em, "shn-cloud-acme")
	classes := append([]string{""}, engine.BackendErrorClasses...)
	for _, d := range append(engine.ExchangeDirections, "junk") {
		for _, x := range append(engine.ExchangeKinds, "junk") {
			for _, o := range append(engine.ExchangeOutcomes, "junk") {
				for _, cl := range append(classes, "junk") {
					hook(engine.ExchangeRecord{Direction: d, Exchange: x, Outcome: o, Backend: &engine.BackendCall{Latency: time.Millisecond, ErrorClass: cl}})
				}
			}
		}
	}
	series := map[string]bool{}
	for _, m := range decodeEMFLines(t, &buf) {
		aws := m["_aws"].(map[string]any)["CloudWatchMetrics"].([]any)[0].(map[string]any)
		key := aws["Metrics"].([]any)[0].(map[string]any)["Name"].(string)
		for _, d := range aws["Dimensions"].([]any)[0].([]any) {
			key += "|" + d.(string) + "=" + m[d.(string)].(string)
		}
		series[key] = true
	}
	if len(series) != exchangeSeriesBound {
		t.Fatalf("distinct series=%d, exchangeSeriesBound=%d", len(series), exchangeSeriesBound)
	}
	// The count RUNNING.md states for today's lists.
	if exchangeSeriesBound != 311 {
		t.Fatalf("exchangeSeriesBound=%d; update the bound in docs/RUNNING.md \"Hosted tenant exchange metrics\" and this pin", exchangeSeriesBound)
	}
}

func TestExchangeObservers(t *testing.T) {
	if exchangeObservers() != nil || exchangeObservers(nil, nil) != nil {
		t.Fatal("no consumer must leave ExchangeObserved nil, so the engine records nothing")
	}
	var order []string
	f := exchangeObservers(func(engine.ExchangeRecord) { order = append(order, "a") }, nil, func(engine.ExchangeRecord) { order = append(order, "b") })
	f(engine.ExchangeRecord{})
	if strings.Join(order, ",") != "a,b" {
		t.Fatalf("order=%v", order)
	}
}

func keysOf(m map[string]map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
