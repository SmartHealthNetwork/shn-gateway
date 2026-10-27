package personas

// hcpcsSystem is the HCPCS code system every demo order is coded in. The
// engine pins it equal to the system its order builder codes them in.
const hcpcsSystem = "http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets"

// Demo-profile display/dx constants (§4.3's family map), so internal/fhirseed's
// MBR-D-UC0N persona roster seeds the SAME clinical text these tuples
// originate — one family fact, not two.
const (
	DemoDisplayE0250 = "Hospital bed fixed height with side rails and mattress"
	DemoDxE0250      = "M62.81"

	DemoDisplayL8000 = "Breast prosthesis, mastectomy bra"
	DemoDxL8000      = "Z90.10"

	DemoDisplayG0151 = "Services of a qualified physical therapist in the home health setting, each 15 minutes"
	DemoDxG0151      = "I63.9"

	DemoDisplayJ3490 = "Unclassified drugs - investigational gene therapy agent"
	DemoDxJ3490      = "D57.1"

	// DemoDisplayE1390UC03/DemoDxE1390UC03 — R3's re-key: UC-03's demo arm off the oxygen
	// family (E1390 oxygen concentrator), matching the provider-data UC-03 persona's own
	// diagnosis (J44.9, COPD — internal/fhirseed.go's MBR-PD-UC03 comment) so the two lanes'
	// UC-03 tell the same clinical story off their own distinct personas/observations.
	DemoDisplayE1390UC03 = "Oxygen concentrator, single delivery port, capable of delivering 85 percent or greater oxygen concentration"
	DemoDxE1390UC03      = "J44.9"
)

// DemoOrderTuple is one demo-lane scenario's originated order — system + code (HCPCS) +
// display + diagnosis (ICD-10-CM). A caller (accountsvc's discovery descriptor, the
// seed) derives a persona's advertised order from the SAME source the gateway's demo
// lane originates from, instead of hand-copying the tuple a second time — verdict,
// order and family table move together or not at all.
type DemoOrderTuple struct {
	System  string
	Code    string
	Display string
	Dx      string
}

// DemoOrderSet is one DemoOrderTuple per UC scenario the demo lane originates.
type DemoOrderSet struct {
	UC02, UC03, UC04, UC05, UC06, UC07, UC07HCPCS, UC08 DemoOrderTuple
	// UC03Bridge is the kit-bridging-visualization demo's own order tuple
	// (uc03BridgeOrder), deliberately DECOUPLED from UC03 — see
	// uc03BridgeOrder's comment. It is in this set for the same reason the
	// rest is: the bridge personas' orders are seeded into the participant's
	// own system, and the seed must derive from this one table rather than
	// re-state the tuple.
	UC03Bridge DemoOrderTuple
}

// uc03BridgeOrder is the kit-bridging-visualization demo's OWN literal order tuple —
// decoupled from the UC03 tuple (R3): the line-carry arm demonstrates a
// pair with no certified translation being carried, which does not depend on the
// family/code carried; the bridge-demo arm reuses the SAME literal since both arms
// share one body. Kept literally UNCHANGED (L8000, order-select) rather than
// following UC-03's oxygen re-key, so this demo-only exhibit stays byte-identical
// to before R3.
var uc03BridgeOrder = DemoOrderTuple{hcpcsSystem, "L8000", DemoDisplayL8000, DemoDxL8000}

// DemoOrderCodes returns the per-UC order tuples the demo lane originates: the
// family-keyed re-key onto br-payer's code-keyed families — E0250/E1390/G0151/J3490
// (+ UC07HCPCS's own L8000), driven off the MBR-D-UC0N persona roster. There is
// exactly one tuple set, and every verdict it drives traces to a mirrored
// reference-payer family.
//
// R3: UC03 DECOUPLES from UC07HCPCS (they used to share the L8000 tuple — a happenstance
// of a retired stub's reuse). UC03 re-keys onto E1390 (an oxygen-family order-DISPATCH
// scenario — its DTR round trip genuinely auto-fills, register §11 ruling (b)); UC07HCPCS
// keeps L8000 UNCHANGED (still order-SELECT, still the L8000 approve exhibit) —
// neither loses a leg or a distinctive by this split.
func DemoOrderCodes() DemoOrderSet {
	l8000 := DemoOrderTuple{hcpcsSystem, "L8000", DemoDisplayL8000, DemoDxL8000}
	g0151 := DemoOrderTuple{hcpcsSystem, "G0151", DemoDisplayG0151, DemoDxG0151}
	return DemoOrderSet{
		UC02:      DemoOrderTuple{hcpcsSystem, "E0250", DemoDisplayE0250, DemoDxE0250},
		UC03:      DemoOrderTuple{hcpcsSystem, "E1390", DemoDisplayE1390UC03, DemoDxE1390UC03},
		UC04:      g0151,
		UC05:      g0151,
		UC06:      g0151,
		UC07:      g0151,
		UC07HCPCS: l8000,
		UC08:      DemoOrderTuple{hcpcsSystem, "J3490", DemoDisplayJ3490, DemoDxJ3490},

		UC03Bridge: uc03BridgeOrder,
	}
}
