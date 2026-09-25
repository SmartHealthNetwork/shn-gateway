package engine

import "context"

// guardDefect applies the policy to a defect a relay-path check found in a
// participant's message and reports whether the message is refused. A network
// rule refuses at every level and records nothing: it is authority or message
// integrity. A content
// defect is not checked at none, is recorded as a finding at observe and
// refuses at strict, recorded there too. A content check that could not finish
// (VerdictUnavailable) is recorded as unavailable at observe and refuses at
// strict. The finding carries the rule and the
// payload's hash, never payload text. emit may be nil.
func guardDefect(ctx context.Context, pol ConformancePolicy, emit func(ConformanceFinding), kind CheckKind, rule string, v Verdict, payload []byte) bool {
	if !pol.Runs(kind, rule) {
		return false
	}
	decision := pol.Decide(kind, rule, v)
	if kind == KindContent && !networkRules[rule] && emit != nil {
		fc := findingContextFrom(ctx)
		f := ConformanceFinding{
			Kind: string(KindContent), LegType: fc.LegType, CorrelationID: fc.CorrelationID,
			Seam: fc.Seam, Whose: fc.Whose, Level: pol.Level().String(),
			Decision: decision.String(), Rule: rule, PayloadSHA256: sha256hex(payload),
		}
		if v == VerdictUnavailable {
			f.Verdict = "unavailable"
		}
		emit(f)
	}
	return decision == Refuse
}

// guard applies this gateway's own policy to an invalid verdict.
func (g *Gateway) guard(ctx context.Context, kind CheckKind, rule string, payload []byte) bool {
	return guardDefect(ctx, g.policy(), g.emitFinding, kind, rule, VerdictInvalid, payload)
}

// guardUnavailable applies this gateway's own policy to a check that could not
// finish (the system of record could not answer it).
func (g *Gateway) guardUnavailable(ctx context.Context, kind CheckKind, rule string, payload []byte) bool {
	return guardDefect(ctx, g.policy(), g.emitFinding, kind, rule, VerdictUnavailable, payload)
}
