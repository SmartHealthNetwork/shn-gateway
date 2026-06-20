// composite.go — the compositeResponder routes a CONFIG-DERIVED set of legs to a
// native LegResponder and every other leg to a fallback. CRD and DTR forward whenever
// native mode is on; the PAS pair forwards only when pasNative is set, toggled
// BOTH-OR-NEITHER so a native submit + sandbox amendment can never split decision
// authority. The coverage-eligibility leg always routes to the fallback (managed):
// none of the common Da Vinci RIs implement CoverageEligibilityRequest adjudication;
// the composite still accepts eligibility but the managed connector makes the decision.
package engine

import "context"

type compositeResponder struct {
	native    LegResponder
	fallback  LegResponder
	forwarded map[string]bool
}

var _ LegResponder = (*compositeResponder)(nil)

// NewCompositeResponder routes the Da Vinci legs (crd-order-select,
// dtr-questionnaire-fetch) to native and — when pasNative — the PAS pair; everything
// else (including coverage-eligibility) routes to fallback. coverage-eligibility is
// always managed: no common Da Vinci RI implements CoverageEligibilityRequest adjudication.
func NewCompositeResponder(native, fallback LegResponder, pasNative bool) LegResponder {
	forwarded := map[string]bool{
		"crd-order-select":        true,
		"dtr-questionnaire-fetch": true,
	}
	if pasNative {
		forwarded["pas-claim"] = true
		forwarded["pas-claim-update"] = true
	}
	return &compositeResponder{native: native, fallback: fallback, forwarded: forwarded}
}

func (c *compositeResponder) Handle(ctx context.Context, leg, corrID, subjectPCI string, requestFHIR []byte) (LegResult, error) {
	if c.forwarded[leg] {
		return c.native.Handle(ctx, leg, corrID, subjectPCI, requestFHIR)
	}
	return c.fallback.Handle(ctx, leg, corrID, subjectPCI, requestFHIR)
}
