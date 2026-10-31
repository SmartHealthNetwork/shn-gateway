//go:build !shn_coreonly

package app

import (
	"bytes"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
	"github.com/SmartHealthNetwork/shn-gateway/recording"
	"github.com/SmartHealthNetwork/shn-gateway/telemetry"
	"github.com/SmartHealthNetwork/shn-gateway/telemetry/stats"
)

// captureSink keeps every measurement the registry hands it.
type captureSink struct {
	mu sync.Mutex
	ms []telemetry.Measurement
}

func (c *captureSink) Record(m telemetry.Measurement) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ms = append(c.ms, m)
}

// of returns the measurements of inst.
func (c *captureSink) of(inst *telemetry.Instrument) []telemetry.Measurement {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []telemetry.Measurement
	for _, m := range c.ms {
		if m.Instrument == inst {
			out = append(out, m)
		}
	}
	return out
}

// MON-R22: an exchange's stages sum to its latency exactly, on every leg; a
// stage it did not enter is not reported, and a derived stage is never
// negative or 0.
func TestExchangeStages_MONR22(t *testing.T) {
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	for _, row := range []struct {
		name string
		rec  engine.ExchangeRecord
		want []stageTime
		sums bool
	}{
		{"inbound, every stage and the rest", engine.ExchangeRecord{Direction: engine.DirectionInbound, Latency: ms(100),
			Stages: &engine.LegStages{Unwrap: ms(1), Reads: ms(2), Forward: ms(3), Validate: ms(4), Seal: ms(5), Ledger: ms(6), Write: ms(7)}},
			[]stageTime{{"unwrap", ms(1)}, {"reads", ms(2)}, {"forward", ms(3)}, {"validate", ms(4)}, {"seal", ms(5)}, {"ledger", ms(6)}, {"write", ms(7)}, {"rest", ms(72)}}, true},
		{"inbound, stages it did not enter", engine.ExchangeRecord{Direction: engine.DirectionInbound, Latency: ms(50),
			Stages: &engine.LegStages{Unwrap: ms(10), Forward: ms(30)}},
			[]stageTime{{"unwrap", ms(10)}, {"forward", ms(30)}, {"rest", ms(10)}}, true},
		{"inbound, nothing outside the stages", engine.ExchangeRecord{Direction: engine.DirectionInbound, Latency: ms(40),
			Stages: &engine.LegStages{Unwrap: ms(10), Forward: ms(30)}},
			[]stageTime{{"unwrap", ms(10)}, {"forward", ms(30)}}, true},
		{"inbound, stages over the latency: no negative rest", engine.ExchangeRecord{Direction: engine.DirectionInbound, Latency: ms(30),
			Stages: &engine.LegStages{Unwrap: ms(10), Forward: ms(30)}},
			[]stageTime{{"unwrap", ms(10)}, {"forward", ms(30)}}, false},
		{"ingress, the network and self", engine.ExchangeRecord{Direction: engine.DirectionIngress, Latency: ms(90),
			Network: &engine.NetworkWait{Latency: ms(60), Calls: 1}},
			[]stageTime{{"network", ms(60)}, {"self", ms(30)}}, true},
		{"ingress, nothing sent to the Hub", engine.ExchangeRecord{Direction: engine.DirectionIngress, Latency: ms(5)},
			[]stageTime{{"self", ms(5)}}, true},
		{"ingress, all of it the network", engine.ExchangeRecord{Direction: engine.DirectionIngress, Latency: ms(60),
			Network: &engine.NetworkWait{Latency: ms(60), Calls: 2}},
			[]stageTime{{"network", ms(60)}}, true},
		{"inbound with no stages recorded: not known", engine.ExchangeRecord{Direction: engine.DirectionInbound, Latency: ms(5)}, nil, false},
		{"a direction outside the list", engine.ExchangeRecord{Direction: "sideways", Latency: ms(5)}, nil, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			got := exchangeStages(row.rec)
			if !slices.Equal(got, row.want) {
				t.Fatalf("stages %v, want %v", got, row.want)
			}
			var sum time.Duration
			for _, st := range got {
				if st.d <= 0 || !slices.Contains(telemetry.Stages, st.stage) {
					t.Fatalf("stage %v: not positive, or not in the closed list", st)
				}
				sum += st.d
			}
			if row.sums && sum != row.rec.Latency {
				t.Fatalf("stages sum to %v, latency %v", sum, row.rec.Latency)
			}
		})
	}
}

// MON-R21, MON-R23: a refused exchange is counted by its rule and the party
// that refused, and only a refused one; a body size is recorded only when it
// is known.
func TestExchangeMetricHook_RefusalsAndSizes(t *testing.T) {
	sink := &captureSink{}
	hook := exchangeMetricHook(telemetry.New(sink))
	n := int64(512)
	hook(engine.ExchangeRecord{Direction: engine.DirectionInbound, Exchange: "pas-claim", Outcome: engine.ExchangeRefused,
		RefusedBy: engine.RefusedByPayerGateway, Rule: engine.RefusalConsent, ResponseBytes: &n})
	hook(engine.ExchangeRecord{Direction: engine.DirectionIngress, Exchange: "pas-claim", Outcome: engine.ExchangeAnswered})
	r := sink.of(telemetry.Refusals)
	if len(r) != 1 || !slices.Equal(r[0].Attributes, []string{engine.DirectionInbound, "pas-claim", engine.RefusalConsent, engine.RefusedByPayerGateway, telemetry.RefusalStageNone}) {
		t.Fatalf("refusals %+v", r)
	}
	if req, resp := sink.of(telemetry.ExchangeRequestSize), sink.of(telemetry.ExchangeResponseSize); len(req) != 0 || len(resp) != 1 || resp[0].Value != 512 {
		t.Fatalf("request sizes %+v, response sizes %+v; want only the one known answer size", req, resp)
	}
}

// MON-R21: a requester's leg the Authorization Framework refused because it
// could not record the decision is one exchange, outcome refused, and one
// refusal on the audit rule by the Framework.
func TestExchangeMetricHook_AuditRefusalByTheFramework(t *testing.T) {
	sink := &captureSink{}
	hook := exchangeMetricHook(telemetry.New(sink))
	hook(engine.ExchangeRecord{Direction: engine.DirectionIngress, Exchange: "crd-order-select", Outcome: engine.ExchangeRefused,
		RefusedBy: engine.RefusedByAuthorizationFramework, Rule: engine.RefusalAudit})
	ex := sink.of(telemetry.Exchanges)
	if len(ex) != 1 || !slices.Contains(ex[0].Attributes, engine.ExchangeRefused) {
		t.Fatalf("exchanges %+v, want one refused", ex)
	}
	r := sink.of(telemetry.Refusals)
	if len(r) != 1 || !slices.Equal(r[0].Attributes, []string{engine.DirectionIngress, "crd-order-select", engine.RefusalAudit, engine.RefusedByAuthorizationFramework, telemetry.RefusalStageNone}) {
		t.Fatalf("refusals %+v", r)
	}
}

// A fidelity refusal is counted with its stage: the OpenTelemetry refusals
// instrument names it (shn.refusal.stage), and EMF writes
// ExchangeRefusedByRule{rule=fidelity}, the refused-fidelity alarm's metric,
// only for one at stage verify (an edit SHN built failing its own
// verification). A build-stage refusal (SHN refusing to build from what the
// request stated) counts in ExchangeRefused with its dimensions unchanged, and
// in the OpenTelemetry instrument with stage build. Every other rule's
// refusal is stage none and keeps its ExchangeRefusedByRule line.
func TestExchangeMetricHook_FidelityStage(t *testing.T) {
	for _, row := range []struct {
		name, rule, stage string
		otelStage         string
		byRule            bool
	}{
		{"verify", engine.RefusalFidelity, recording.RefusalStageVerify, recording.RefusalStageVerify, true},
		{"build", engine.RefusalFidelity, recording.RefusalStageBuild, recording.RefusalStageBuild, false},
		{"fidelity naming no stage", engine.RefusalFidelity, "", engine.ExchangeOther, true},
		{"another rule", engine.RefusalRouting, "", telemetry.RefusalStageNone, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			var buf bytes.Buffer
			sink := &captureSink{}
			hook := exchangeMetricHook(telemetry.New(telemetry.NewEMF(goldenEmitter(&buf), "svc", "provider"), sink))
			hook(engine.ExchangeRecord{Direction: engine.DirectionIngress, Exchange: "pas-claim", Outcome: engine.ExchangeRefused,
				RefusedBy: engine.RefusedByProviderGateway, Rule: row.rule, RefusalStage: row.stage, Status: 422})
			r := sink.of(telemetry.Refusals)
			if len(r) != 1 || !slices.Equal(r[0].Attributes, []string{engine.DirectionIngress, "pas-claim", row.rule, engine.RefusedByProviderGateway, row.otelStage}) {
				t.Fatalf("refusals %+v, want stage %s", r, row.otelStage)
			}
			lines := map[string]map[string]any{}
			for _, l := range decodeEMFLines(t, &buf) {
				name := l["_aws"].(map[string]any)["CloudWatchMetrics"].([]any)[0].(map[string]any)["Metrics"].([]any)[0].(map[string]any)["Name"].(string)
				lines[name] = l
			}
			refused, ok := lines["ExchangeRefused"]
			if !ok || refused["rule"] != row.rule || refused["by"] != engine.RefusedByProviderGateway || refused["direction"] != engine.DirectionIngress {
				t.Fatalf("ExchangeRefused %v, want it written for every refusal", refused)
			}
			if _, has := refused["stage"]; has {
				t.Fatalf("ExchangeRefused carries a stage dimension: %v", refused)
			}
			byRule, ok := lines["ExchangeRefusedByRule"]
			if ok != row.byRule {
				t.Fatalf("ExchangeRefusedByRule written=%v, want %v (%v)", ok, row.byRule, byRule)
			}
			if ok {
				dims := byRule["_aws"].(map[string]any)["CloudWatchMetrics"].([]any)[0].(map[string]any)["Dimensions"].([]any)[0].([]any)
				if byRule["rule"] != row.rule || len(dims) != 3 {
					t.Fatalf("ExchangeRefusedByRule %v, want {Env, Service, rule} on rule %s", byRule, row.rule)
				}
			}
		})
	}
}

// MON-R23: EMF's ActiveRequests is the peak in flight since the last sample,
// for each direction, a known 0 included; OpenTelemetry gets the moves.
func TestActiveRequestsEMFPeak_MONR23(t *testing.T) {
	var buf bytes.Buffer
	emf := telemetry.NewEMF(goldenEmitter(&buf), "svc", "payer")
	sink := &captureSink{}
	active := activeRequestHook(telemetry.New(emf, sink))
	active(engine.DirectionInbound, 1)
	active(engine.DirectionInbound, 1)
	active(engine.DirectionInbound, -1)
	if buf.Len() != 0 {
		t.Fatalf("a move wrote an EMF line: %s", buf.String())
	}
	if moves := sink.of(telemetry.ActiveRequests); len(moves) != 3 {
		t.Fatalf("%d moves reached the other sink, want 3", len(moves))
	}
	peaks := func() map[string]float64 {
		out := map[string]float64{}
		for _, l := range decodeEMFLines(t, &buf) {
			out[l["direction"].(string)] = l["ActiveRequests"].(float64)
		}
		buf.Reset()
		return out
	}
	emf.SampleActiveRequests()
	if got := peaks(); got[engine.DirectionInbound] != 2 || got[engine.DirectionIngress] != 0 || len(got) != 2 {
		t.Fatalf("first sample %v, want inbound 2 and ingress 0", got)
	}
	// The next sample starts from the level still in flight.
	emf.SampleActiveRequests()
	if got := peaks(); got[engine.DirectionInbound] != 1 || got[engine.DirectionIngress] != 0 {
		t.Fatalf("second sample %v, want inbound 1", got)
	}
	if strings.Contains(buf.String(), "other") {
		t.Fatal("a sample named the other direction")
	}
}

// An origination call (a route of engine.ExchangeOriginationRoutes) whose
// answer took 30 s counts as an exchange and keeps its outcome, but feeds no
// exchange duration, no stage duration and no /internal/stats latency: its
// time is a flow of legs, not one exchange's answer. A call on an ingress
// route of the same kind still feeds all three.
func TestExchangeMetricHook_OriginationLatencyIsNotADuration(t *testing.T) {
	if len(engine.ExchangeOriginationRoutes) == 0 {
		t.Fatal("this build declares no origination route")
	}
	now := time.Unix(1_761_350_400, 0)
	agg := stats.New("replica-1", func() time.Time { return now })
	sink := &captureSink{}
	hook := exchangeMetricHook(telemetry.New(agg, sink))
	ingress := engine.ExchangeRecord{Direction: engine.DirectionIngress, Route: engine.RouteCRD, Exchange: "crd-order-select", Outcome: engine.ExchangeAnswered,
		Latency: time.Second, Network: &engine.NetworkWait{Latency: 600 * time.Millisecond, Calls: 1}}
	hook(ingress)
	series := func() stats.ExchangeSeries {
		for _, s := range agg.Report().Windows[0].Exchanges {
			if s.Direction == engine.DirectionIngress && s.Exchange == "crd-order-select" {
				return s
			}
		}
		t.Fatal("no ingress crd-order-select series in /internal/stats")
		return stats.ExchangeSeries{}
	}
	before := series()
	durations, stages := len(sink.of(telemetry.ExchangeDuration)), len(sink.of(telemetry.ExchangeStageDuration))
	if durations != 1 || stages != 2 || before.Count != 1 || before.Latency.Sketch.Count() != 1 {
		t.Fatalf("control: %d durations, %d stages, stats count %d sketch %d; want 1, 2 (network, self), 1, 1", durations, stages, before.Count, before.Latency.Sketch.Count())
	}

	origination := ingress
	origination.Route = engine.ExchangeOriginationRoutes[0]
	origination.Latency = 30 * time.Second
	origination.Network = &engine.NetworkWait{Latency: 29 * time.Second, Calls: 3}
	hook(origination)
	after := series()
	if got := len(sink.of(telemetry.Exchanges)); got != 2 {
		t.Fatalf("%d exchanges counted, want the origination counted too", got)
	}
	if got := len(sink.of(telemetry.ExchangeDuration)); got != durations {
		t.Fatalf("the origination recorded an exchange duration (%d, was %d)", got, durations)
	}
	if got := len(sink.of(telemetry.ExchangeStageDuration)); got != stages {
		t.Fatalf("the origination recorded stage durations (%d, was %d)", got, stages)
	}
	if after.Count != 2 || after.Outcomes[engine.ExchangeAnswered] != 2 {
		t.Fatalf("/internal/stats count %d outcomes %v, want the origination counted", after.Count, after.Outcomes)
	}
	if after.Latency.Sketch.Count() != 1 || *after.Latency.P95 != *before.Latency.P95 || *after.Latency.Max != *before.Latency.Max {
		t.Fatalf("/internal/stats latency moved: sketch %d p95 %v max %v, was %v / %v", after.Latency.Sketch.Count(), *after.Latency.P95, *after.Latency.Max, *before.Latency.P95, *before.Latency.Max)
	}
}
