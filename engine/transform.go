// transform.go — the cross-version transform framework:
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

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// TransformFunc is one direction of one adjacent step: pure, deterministic,
// no I/O, no clock. x carries only exchange identity already known
// to the leg (correlation id, leg type, counterpart) so upcast synthesis
// stays deterministic; TransformFuncs read only CorrelationID today — LegType
// and Counterpart exist for the chain walk's own leg.transformed stamping,
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

// envelopeEgressLegs is the DTR-fetch-ONLY non-FHIR carve-out (the
// multi-version spec's recorded DTR-fetch known-gap obligation, discharged).
// Membership means egressAdapt walks
// route.Chain for the routing/observer story but never hands the bytes to a
// step function — safe only because dtr-questionnaire-fetch's payload is
// the operation's own input, carried to the payer's line unchanged: the
// ingress and the originator refuse a walk that would change a byte of it
// (handleDTRIngress, carryUnchanged), so no pa.dtr compat-manifest step
// ever rewrites it.
//
// CRD legs (crd-order-select, crd-order-dispatch) must NEVER join this set:
// their arm-3 byte-identity rests on the identity chain genuinely RUNNING
// (TestD7CRDArm3IdentityChainIsBytePreserving) — a CDS Hooks payload IS FHIR
// content the pa.crd manifest models, so carving it out here would misname
// it as an envelope, would be redundant (applyChain's nil-func identity path
// is already a byte-pure pass-through with identical reports, transform.go's
// applyChain), and would silently bypass any FUTURE real CRD module instead
// of making it run or refuse honestly. TestCRDLegsNeverJoinEnvelopeCarveOut
// pins the set's exact membership.
//
// Terminology note: the commit that discharged it calls this "proven
// safe by byte-identity guard" — there is no RUNTIME guard (no equality
// check anywhere in AdaptRequest below; out is never a copy, so one would be
// structurally unreachable). "Guard" there means
// TestEnvelopeLegChainIsByteIdenticalPassThrough, the test-time pin that
// enforces byte-identity by construction, not a production code path.
var envelopeEgressLegs = map[string]bool{"dtr-questionnaire-fetch": true}

// verbatimChainLegs are legs whose payload the compat chain WALKS — so the
// routing and observer story stays honest — and never rewrites.
//
// pas-claim-inquire is one, for two reasons that both have to hold.
//
// FIRST, the chain has nothing to do to it. Every pa.pas step models a
// difference between the SUBMIT/AMENDMENT profiles (profile-claim,
// profile-claim-update) or the response profile: the Claim.item line detail and
// the related-claim relationship, and ClaimResponse.request. An inquiry's Claim
// is profile-claim-inquiry, a third profile that states none of them — the same
// distinction the certification layer draws (workstream_pa_conformance_species.go)
// and the validator lane scopes (linefake.go). Measured: running the chain over an inquiry changed not
// one fact, only the key order of the re-marshalled JSON.
//
// SECOND, that re-marshal is exactly what an inquiry must not suffer. Its
// payload CARRIES the participant's own Patient, Coverage, provider and payer
// records as verbatim spans, sealed as embeds — that is what makes the bytes on
// the wire provably the participant's records rather than a rendering of them.
// A step that re-serialized them would reorder their keys and leave the seal
// describing a message that no longer exists.
//
// inquireContinuation keeps the runtime byte-equality check, so a future
// inquiry-aware step that DID change the bytes is never sent under a broken
// seal: the inquiry is sent as built, at its build line, or refused under the
// participant's REQUIRE_SHARED_LINE.
var verbatimChainLegs = map[string]bool{"pas-claim-inquire": true}

// AdaptRequest walks payload along route's chain: through the steps, or, on an
// envelope or verbatim leg, along them without handing the bytes to one.
func (b chainBridge) AdaptRequest(route legRoute, payload []byte, x ExchangeIdentity) ([]byte, []LossReport, func() error, error) {
	noRecord := func() error { return nil }
	var out []byte
	var reports []LossReport
	var err error
	if envelopeEgressLegs[x.LegType] || verbatimChainLegs[x.LegType] {
		// Non-FHIR carve-out (obligation discharged — see
		// envelopeEgressLegs's own doc comment and the multi-version spec's
		// recorded DTR-fetch known-gap entry): envelope legs are
		// safe BY CONSTRUCTION — the chain is walked (envelopeChainReports
		// mirrors applyChain's own walk-direction switch, below) so the
		// routing/observer story stays honest, but the step funcs never see
		// the bytes, so they can never re-marshal (and therefore never
		// reorder) the envelope's JSON. No runtime byte-equality guard
		// follows this assignment — it would be structurally unreachable
		// (out IS payload, never a copy that could diverge), the exact
		// anti-pattern obligation 2 exists to eliminate.
		// TestEnvelopeLegChainIsByteIdenticalPassThrough is the enforcement
		// fence in place of the impossible envelope $validate.
		out, reports = payload, envelopeChainReports(route.Chain, route.BuildLine)
	} else {
		out, reports, err = applyChain(route.Chain, route.BuildLine, payload, x)
	}
	if err != nil {
		// Observer honesty: a transform-chain refusal used to be observer-SILENT (every
		// egressAdapt call site precedes roundTrip, at the time the only
		// other leg.failed producer; guardPendCarry now emits on this same
		// seam too — observer.go's kinds table names all three) — a §6-grade
		// honesty gap the observer stream must not have. Route carries the
		// attempted chain (routeInfoFor) so the refusal is legible on the
		// SAME seam leg.transformed/leg.originated use, not just a bare
		// error string.
		b.g.observe(ObserverEvent{
			Kind: "leg.failed", Direction: "originate",
			LegType: x.LegType, CorrelationID: x.CorrelationID,
			Counterpart: x.Counterpart, Detail: err.Error(),
			Route: routeInfoFor(route),
		})
		return nil, nil, noRecord, err
	}
	contract := route.Chain[0].Contract
	targetLine := shnsdk.LineOf(route.Token)

	ev := transformedObserverEvent(reports)
	ev.Direction = "originate"
	ev.CorrelationID = x.CorrelationID
	ev.LegType = x.LegType
	ev.Counterpart = x.Counterpart
	if spec, ok := legSpecFor(x.LegType); ok {
		ev.AuthorityFrame = spec.ReqFrame
	}

	return out, reports, func() error {
		return b.g.recordTransformed(ev, x, contract, route.BuildLine, targetLine, route.Chain, reports, payload, out)
	}, nil
}

// errUnregisteredStep is a route's chain naming a step the compat manifest
// does not register.
var errUnregisteredStep = errors.New("the chain names a transform step the compat manifest does not register")

// registeredRederive is the registered transform's verifier (relay.Transformed):
// the compat manifest's own rows for the steps route's chain names, matched
// step for step (contract, lines, class and step functions), walked from the
// route's build line. A chain naming any step the manifest does not register
// has no verifier (errUnregisteredStep).
func registeredRederive(route legRoute, x ExchangeIdentity) (func([]byte) ([]byte, error), error) {
	steps := make([]CompatStep, 0, len(route.Chain))
	for _, s := range route.Chain {
		i := slices.IndexFunc(compatManifest, func(r CompatStep) bool {
			return r.Contract == s.Contract && r.From == s.From && r.To == s.To && r.Class == s.Class &&
				sameStep(r.Up, s.Up) && sameStep(r.Down, s.Down)
		})
		if i < 0 {
			return nil, fmt.Errorf("%w: %s %s-%s", errUnregisteredStep, s.Contract, s.From, s.To)
		}
		steps = append(steps, compatManifest[i])
	}
	return func(b []byte) ([]byte, error) {
		out, _, err := applyChain(steps, route.BuildLine, b, x)
		return out, err
	}, nil
}

// sameStep reports whether a and b are the same step function (both nil, the
// identity, or the same function).
func sameStep(a, b TransformFunc) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
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
// AdaptRequest calls this after applyChain returns, then fills in the
// remaining ObserverEvent fields — LegType, CorrelationID, Counterpart, and
// AuthorityFrame (the legSpecFor lookup for x.LegType) — on the result, and
// passes it to Gateway.observe. All four fields are filled for every walk.
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
