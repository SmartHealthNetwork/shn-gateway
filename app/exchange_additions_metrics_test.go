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
	"github.com/SmartHealthNetwork/shn-gateway/telemetry"
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
	if len(r) != 1 || !slices.Equal(r[0].Attributes, []string{engine.DirectionInbound, "pas-claim", engine.RefusalConsent, engine.RefusedByPayerGateway}) {
		t.Fatalf("refusals %+v", r)
	}
	if req, resp := sink.of(telemetry.ExchangeRequestSize), sink.of(telemetry.ExchangeResponseSize); len(req) != 0 || len(resp) != 1 || resp[0].Value != 512 {
		t.Fatalf("request sizes %+v, response sizes %+v; want only the one known answer size", req, resp)
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
