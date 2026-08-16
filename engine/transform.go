// transform.go — the cross-version transform framework (spec 2026-08-10 §5):
// pure, deterministic per-adjacent-line step functions and the chain
// executor that composes them. This is engine-internal machinery, not
// published-SDK surface — no sdk twin, no sdkparity row.
//
// A "step" bridges ONE adjacent pair of lines for ONE contract, in both
// directions (Up = low->high, Down = high->low). Steps compose into chains
// (see compat.go's chainFor) to bridge non-adjacent lines (e.g. 2.0->2.2 via
// 2.0->2.1->2.1->2.2). Every step is pure: no I/O, no clock, no randomness —
// the same (payload, ExchangeIdentity) input always produces the same
// (payload, LossReport) output, so a chain's overall result is deterministic
// by construction (pinned by TestApplyChainDeterministic).
package engine

import "strings"

// TransformFunc is one direction of one adjacent step: pure, deterministic,
// no I/O, no clock (spec §5). x carries only exchange identity already known
// to the leg (correlation id, leg type, counterpart) so upcast synthesis
// stays deterministic; TransformFuncs read only CorrelationID today — LegType
// and Counterpart exist for egressAdapt's own leg.transformed stamping,
// not because any step function consumes them.
type TransformFunc func(payload []byte, x ExchangeIdentity) ([]byte, LossReport, error)

// ExchangeIdentity is exchange identity already known to the leg at build
// time. LegType and Counterpart are additive: every existing
// TransformFunc and every earlier literal that sets only CorrelationID
// keeps compiling and keeps its (zero-value) behavior unchanged.
type ExchangeIdentity struct{ CorrelationID, LegType, Counterpart string }

type StepClass string // "full" | "carry" | "gated"

// The three StepClass values a CompatStep row may declare. D1b's chain
// ranking orders them best-to-worst: full beats carry beats gated.
const (
	StepFull  StepClass = "full"  // lossless mapping, both directions honest
	StepCarry StepClass = "carry" // some content moved into shn-carried-content, round-trippable
	StepGated StepClass = "gated" // no honest byte-level source for a required element — refuses
)

type CompatStep struct {
	Contract string // "pa.pas"
	From, To string // adjacent lines, low→high: {"2.0","2.1"}
	Class    StepClass
	Up, Down TransformFunc // nil ⇒ identity, permitted only when Class=="full"
}

type LossReport struct {
	Module      string      `json:"module"` // "pa.pas 2.1->2.2"
	Source      string      `json:"source"`
	Target      string      `json:"target"`
	Carried     []LossEntry `json:"carried,omitempty"`     // moved into shn-carried-content
	Synthesized []LossEntry `json:"synthesized,omitempty"` // deterministically minted (upcast-mandatory)
}
type LossEntry struct {
	Path   string `json:"path"`             // FHIRPath-ish locator of the element
	Detail string `json:"detail,omitempty"` // e.g. "authorizationNumber carried; source line 2.2"
}

// applyChain runs the steps in order (Up when walking low→high, Down when
// high→low), accumulating one LossReport per step. Any step error aborts —
// the caller treats it as the typed semantic-change refusal. No partial
// output or partial report slice ever escapes an aborted chain (both return
// values are nil on error) — the loss policy's "refuse only on semantic
// change, never silently drop" invariant depends on this at the caller.
func applyChain(steps []CompatStep, from string, payload []byte, x ExchangeIdentity) ([]byte, []LossReport, error) {
	cur := payload
	curLine := from
	var reports []LossReport

	for _, step := range steps {
		var fn TransformFunc
		var nextLine string
		switch curLine {
		case step.From:
			fn = step.Up
			nextLine = step.To
		case step.To:
			fn = step.Down
			nextLine = step.From
		default:
			return nil, nil, &chainDisconnectedError{Contract: step.Contract, From: step.From, To: step.To, CurLine: curLine}
		}

		if fn == nil {
			// nil ⇒ identity (permitted only for Class=="full" — enforced by
			// the compat-manifest coverage pin, not re-checked here).
			reports = append(reports, LossReport{
				Module: step.Contract + " " + curLine + "->" + nextLine,
				Source: curLine,
				Target: nextLine,
			})
			curLine = nextLine
			continue
		}

		out, report, err := fn(cur, x)
		if err != nil {
			return nil, nil, err
		}
		cur = out
		reports = append(reports, report)
		curLine = nextLine
	}

	return cur, reports, nil
}

// envelopeChainReports walks chain from buildLine — mirroring applyChain's
// own walk-direction switch above (and observer.go's routeInfoFor, the
// observer-facing twin of this same walk) — WITHOUT invoking any step
// function: egressAdapt's envelope carve-out exists precisely so the
// step funcs never see envelope bytes. One empty-content
// LossReport{Module, Source, Target} per step, in walk order: honest that
// the chain was walked (the routing/observer story), honest that no
// Carried/Synthesized content moved (there is none — the declared carve-out
// means content was never touched).
func envelopeChainReports(chain []CompatStep, buildLine string) []LossReport {
	if len(chain) == 0 {
		return nil
	}
	reports := make([]LossReport, 0, len(chain))
	curLine := buildLine
	for _, step := range chain {
		nextLine := step.To
		if curLine == step.To {
			nextLine = step.From
		}
		// curLine == step.From (or an unreached/disconnected row — chainFor
		// never produces one; applyChain's own default case treats it as a
		// caller bug, not a data-driven state this walk needs to handle)
		// keeps nextLine == step.To, set above.
		reports = append(reports, LossReport{
			Module: step.Contract + " " + curLine + "->" + nextLine,
			Source: curLine, Target: nextLine,
		})
		curLine = nextLine
	}
	return reports
}

// legTransformedKind is the ObserverEvent.Kind a leg emits once a transform
// chain has produced its output (observer.go's doc table). A named
// constant here — unlike the other Kinds, which stay inline string literals
// at their gateway.go emission sites (leg.refused, leg.downgrade, ...) —
// because the construction seam here and the routing wiring that calls it
// must agree on the exact string without duplicating it by hand.
const legTransformedKind = "leg.transformed"

// transformedObserverEvent builds the ObserverEvent a leg emits after a
// transform chain bridges it to the peer's contract line: Kind
// leg.transformed, Detail = the chain's module summary — each report's
// Module, comma-joined in step order — so an inspector sees which steps ran
// without needing the full LossReport/Provenance payload.
//
// g.egressAdapt calls this after applyChain returns, then fills in the
// remaining ObserverEvent fields — LegType, CorrelationID, Counterpart, and
// AuthorityFrame (the paCatalog lookup for x.LegType) — on the result, and
// passes it to Gateway.observe. All four fields are filled at every
// one of egressAdapt's call sites.
func transformedObserverEvent(reports []LossReport) ObserverEvent {
	modules := make([]string, 0, len(reports))
	for _, r := range reports {
		modules = append(modules, r.Module)
	}
	return ObserverEvent{Kind: legTransformedKind, Detail: strings.Join(modules, ", ")}
}

// chainDisconnectedError signals a chain whose steps do not form a connected
// path from the starting line — a caller bug (chainFor never produces such a
// chain), not a data-driven refusal.
type chainDisconnectedError struct {
	Contract, From, To, CurLine string
}

func (e *chainDisconnectedError) Error() string {
	return "applyChain: step " + e.Contract + " " + e.From + "->" + e.To + " does not connect to current line " + e.CurLine
}
