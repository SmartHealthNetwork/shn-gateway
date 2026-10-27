package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"
)

// Module 3's finding emitter: a check's finding counted on the call's
// exchange record, bound to its leg, and written to the log and the observer
// with its diagnostics redacted. The finding's wire shape is finding.go's
// ConformanceFinding.

// emitFindingIn is emitFinding for a check made while answering a call: the
// call's exchange record counts the finding too.
func (g *Gateway) emitFindingIn(ctx context.Context, f ConformanceFinding) {
	countingEmit(ctx, g.emitFinding)(f)
}

// countingEmit wraps a finding emitter so the exchange record of the call ctx
// belongs to counts each finding it writes, and names the rule of one that
// refuses. nil stays nil: an unwired emitter records nothing.
func countingEmit(ctx context.Context, emit func(ConformanceFinding)) func(ConformanceFinding) {
	if emit == nil {
		return nil
	}
	x := exchangeOf(ctx)
	return func(f ConformanceFinding) {
		emit(bindFinding(ctx, f))
		if x != nil {
			x.checked(CheckKind(f.Kind), f.Rule, true, f.Decision == Refuse.String())
		}
	}
}

// bindFinding ties a finding to the call ctx belongs to: its binding fields
// and, at observe, its leg's tally.
func bindFinding(ctx context.Context, f ConformanceFinding) ConformanceFinding {
	if fc := findingContextFrom(ctx); fc.Edit != "" {
		f.Edit, f.SourcePayloadSHA256 = string(fc.Edit), fc.SourceSHA256
	}
	f.binding = findingBindingFrom(ctx)
	f.tally = legTallyFrom(ctx)
	return f
}

// emitFinding is the one place a governed check's finding is written to both
// carriers, and the one place the redaction rule in ConformanceFinding's
// comment is enforced: f.Issues arrives raw and is replaced with
// certificationIssueMetadata's authored summary (count + size + hash) before
// anything is marshalled, so the redacted form — never the raw diagnostics —
// is what the log line and the observer event actually carry. It is called
// before any decision is acted on, at every level that runs the check, so the
// record of what was seen never depends on what was done about it.
func (g *Gateway) emitFinding(f ConformanceFinding) {
	f.Issues = certificationIssueMetadata(f.Issues)
	if m := g.cfg.FindingMetric; m != nil {
		m(FindingMetric{Exchange: f.LegType, Level: f.Level, Kind: f.Kind, Action: f.Decision, Tolerated: f.Tolerated})
	}
	b, err := json.Marshal(f)
	if err != nil {
		return
	}
	log.Printf("gateway: conformance: %s", b)
	g.observe(ObserverEvent{
		Kind:          ConformanceObservedEvent,
		Direction:     "validate",
		LegType:       f.LegType,
		CorrelationID: f.CorrelationID,
		Detail:        string(b),
	})
	g.diagnosticFinding(f, string(b))
}

// validatorMetric reports one $validate call's time (Config.ValidatorMetric).
func (g *Gateway) validatorMetric(line, lane string, took time.Duration) {
	if m := g.cfg.ValidatorMetric; m != nil {
		m(line, lane, took)
	}
}

// routingValidatorTook reports one $validate call on the routing lane, the
// one a workstream's own check of a payload makes (Config.ValidatorMetric).
func (g *Gateway) routingValidatorTook(line string, took time.Duration) {
	g.validatorMetric(line, ValidatorLaneRouting, took)
}

// findingIssuesShown bounds the issue list a strict refusal body echoes, the
// same bound the CDS Hooks certifier already applies (workstream_pa_crd_forward.go).
const findingIssuesShown = 5

// boundIssues is the ONLY path by which validator diagnostic text leaves this
// gateway: the strict refusal body returned to the sender (§5). A finding
// never carries these strings.
func boundIssues(issues []string) []string {
	if len(issues) <= findingIssuesShown {
		return issues
	}
	out := append([]string{}, issues[:findingIssuesShown]...)
	return append(out, fmt.Sprintf("and %d more", len(issues)-findingIssuesShown))
}
