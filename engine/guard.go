package engine

import "context"

// guardDefect applies the gateway's checks to a defect a relay-path check
// found in a participant's message and reports whether the message is
// refused. A network rule refuses at every level and records nothing: it is
// authority or message integrity, decided by networkRefusal before module 3
// is consulted. Every other defect is payload conformance, module 3's to
// decide (conformanceModule.content).
func guardDefect(ctx context.Context, mod conformanceModule, emit func(ConformanceFinding), kind CheckKind, rule string, v Verdict, payload []byte) bool {
	if networkRefusal(rule) {
		refuse := v != VerdictValid
		exchangeOf(ctx).checked(kind, rule, false, refuse)
		return refuse
	}
	return mod.content(ctx, emit, kind, rule, v, payload)
}

// guard applies this gateway's own checks to an invalid verdict.
func (g *Gateway) guard(ctx context.Context, kind CheckKind, rule string, payload []byte) bool {
	return guardDefect(ctx, g.conformance(), g.emitFinding, kind, rule, VerdictInvalid, payload)
}

// guardUnavailable applies this gateway's own checks to a check that could not
// finish (the system of record could not answer it).
func (g *Gateway) guardUnavailable(ctx context.Context, kind CheckKind, rule string, payload []byte) bool {
	return guardDefect(ctx, g.conformance(), g.emitFinding, kind, rule, VerdictUnavailable, payload)
}
