package app

import (
	"encoding/json"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/diagnostics"
	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
)

// accessLinePrefix begins the one log line a gateway writes per exchange.
const accessLinePrefix = "gateway: access: "

// accessLineFor renders an exchange record as its access line.
func accessLineFor(r engine.ExchangeRecord) diagnostics.AccessLine {
	line := diagnostics.AccessLine{
		Time: r.Start.UTC(), CorrelationID: r.CorrelationID, Trace: r.Trace, CallID: r.CallID,
		Direction: r.Direction, Route: r.Route, Exchange: r.Exchange, Operation: r.Operation,
		ContractLine: r.ContractLine, Sender: r.Sender, Recipient: r.Recipient,
		RequestCiphertextHash: r.RequestCiphertextHash, ResponseCiphertextHash: r.ResponseCiphertextHash,
		Outcome: r.Outcome, Status: r.Status, LatencyMs: r.Latency.Milliseconds(),
		Findings: diagnostics.AccessFindings{Count: r.Findings.Count, Refused: r.Findings.Refused, Kinds: r.Findings.Kinds},
	}
	if r.Outcome == engine.ExchangeRefused {
		line.Refusal = &diagnostics.AccessRefusal{By: r.RefusedBy, Rule: r.Rule}
	}
	if b := r.Backend; b != nil {
		line.Backend = &diagnostics.AccessBackend{Status: b.Status, LatencyMs: b.Latency.Milliseconds(), ErrorClass: b.ErrorClass, Calls: r.BackendCalls}
	}
	return line
}

// accessHook returns the engine.Config.ExchangeObserved consumer that writes
// each exchange's access line with logf, always, and, when emit is set (the
// gateway's diagnostic capture is configured), publishes the same line as a
// metadata-only diagnostics event.
func accessHook(logf func(string, ...any), emit func(diagnostics.Event) bool, clock func() time.Time) func(engine.ExchangeRecord) {
	return func(r engine.ExchangeRecord) {
		line := accessLineFor(r)
		b, err := json.Marshal(line)
		if err != nil {
			return
		}
		logf("%s%s", accessLinePrefix, b)
		if emit == nil {
			return
		}
		emit(diagnostics.Event{
			Time: clock(), Kind: diagnostics.KindAccess, CallID: line.CallID, CorrelationID: line.CorrelationID,
			RequestCiphertextHash: line.RequestCiphertextHash, Sender: line.Sender, Recipient: line.Recipient,
			LegType: line.Exchange, ContractLine: line.ContractLine, Status: line.Status,
			DurationNanos: r.Latency.Nanoseconds(), Detail: string(b),
		})
	}
}
