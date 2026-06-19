// composite.go — the compositeResponder routes a CONFIG-DERIVED set of legs to a
// native LegResponder and every other leg to a fallback (design §5.1). Read-only legs
// forward whenever native mode is on; the PAS pair forwards only when pasNative is set,
// toggled BOTH-OR-NEITHER so a native submit + sandbox amendment can never split
// decision authority (design §3).
package engine

import "context"

type compositeResponder struct {
	native    LegResponder
	fallback  LegResponder
	forwarded map[string]bool
}

var _ LegResponder = (*compositeResponder)(nil)

// NewCompositeResponder routes the read-only legs (always) and — when pasNative — the
// PAS pair to native; everything else to fallback.
func NewCompositeResponder(native, fallback LegResponder, pasNative bool) LegResponder {
	forwarded := map[string]bool{
		"coverage-eligibility":    true,
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
