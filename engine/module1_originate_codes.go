//go:build !shn_coreonly

package engine

import "github.com/SmartHealthNetwork/shn-gateway/pa/personas"

// orderTuple is one originated order's {system, code, display, dx}.
type orderTuple struct{ system, code, display, dx string }

// orderTupleOf is a persona's order tuple (package personas) as module 1
// builds from it.
func orderTupleOf(o personas.DemoOrderTuple) orderTuple {
	return orderTuple{system: o.System, code: o.Code, display: o.Display, dx: o.Dx}
}

// originationProfile holds the per-UC order tuples the demo lane originates (§4.3's
// br-payer family map, HCPCS-coded). The provider-data lane reads its orders from the SoR
// (orderSource → OpenOrder), not from these tuples, so it does not key on them. The demo
// lane builds its order from the tuple (orderSource's non-provider-data branch) — no SoR
// order needed, only a seeded Patient/Coverage per member.
type originationProfile struct {
	uc02, uc03, uc04, uc05, uc06, uc07, uc07hcpcs, uc08 orderTuple
}

// originationCodes returns the per-UC order tuples the demo lane originates:
// the personas' one table (personas.DemoOrderCodes), field for field.
func originationCodes() originationProfile {
	c := personas.DemoOrderCodes()
	return originationProfile{
		uc02:      orderTupleOf(c.UC02),
		uc03:      orderTupleOf(c.UC03),
		uc04:      orderTupleOf(c.UC04),
		uc05:      orderTupleOf(c.UC05),
		uc06:      orderTupleOf(c.UC06),
		uc07:      orderTupleOf(c.UC07),
		uc07hcpcs: orderTupleOf(c.UC07HCPCS),
		uc08:      orderTupleOf(c.UC08),
	}
}
