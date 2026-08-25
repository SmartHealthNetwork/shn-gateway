package engine

// orderTuple is one originated order's {system, code, display, dx}.
type orderTuple struct{ system, code, display, dx string }

// originationProfile holds the per-UC order tuples the demo lane originates (§4.3's
// br-payer family map, HCPCS-coded). The provider-data lane reads its orders from the SoR
// (orderSource → OpenOrder), not from these tuples, so it does not key on them. The demo
// lane builds its order from the tuple (orderSource's non-provider-data branch) — no SoR
// order needed, only a seeded Patient/Coverage per member.
type originationProfile struct {
	uc02, uc03, uc04, uc05, uc06, uc07, uc07hcpcs, uc08 orderTuple
}

// Demo-profile display/dx constants (§4.3's family map). Exported so
// internal/fhirseed's MBR-D-UC0N persona roster seeds the SAME
// clinical text these tuples originate — one family fact, not two.
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

// originationCodes returns the per-UC order tuples: the family-keyed re-key onto
// br-payer's code-keyed families — E0250/E1390/G0151/J3490 (+ uc07hcpcs's own L8000),
// driven off the MBR-D-UC0N persona roster. It takes no profile argument any more: the
// CPT/lumbar tuple retired with the in-process payer stub (§4.1/§4.3), so there is
// exactly one tuple set and every verdict it drives traces to a mirrored reference-payer
// family.
//
// R3: uc03 DECOUPLES from uc07hcpcs (they used to share the L8000 tuple — a happenstance
// of the retired stub's reuse, pinned as "must be equal" by a test that predates either
// scenario's real shape). uc03 re-keys onto E1390 (an oxygen-family order-DISPATCH
// scenario — its DTR round trip genuinely auto-fills, register §11 ruling (b)); uc07hcpcs
// keeps L8000 UNCHANGED (still order-SELECT, still the L8000 approve + Patient Access
// read-back exhibit) — neither loses a leg or a distinctive by this split.
func originationCodes() originationProfile {
	l8000 := orderTuple{systemHCPCSBuild, "L8000", DemoDisplayL8000, DemoDxL8000}
	g0151 := orderTuple{systemHCPCSBuild, "G0151", DemoDisplayG0151, DemoDxG0151}
	return originationProfile{
		uc02:      orderTuple{systemHCPCSBuild, "E0250", DemoDisplayE0250, DemoDxE0250},
		uc03:      orderTuple{systemHCPCSBuild, "E1390", DemoDisplayE1390UC03, DemoDxE1390UC03},
		uc04:      g0151,
		uc05:      g0151,
		uc06:      g0151,
		uc07:      g0151,
		uc07hcpcs: l8000,
		uc08:      orderTuple{systemHCPCSBuild, "J3490", DemoDisplayJ3490, DemoDxJ3490},
	}
}

// DemoOrderTuple is one demo-lane scenario's originated order — system + code (HCPCS) +
// display + diagnosis (ICD-10-CM) — exported so a caller OUTSIDE this package (accountsvc's
// discovery descriptor) can derive a persona's advertised order from the SAME source
// originationCodes() itself builds from, instead of hand-copying the tuple a second time —
// verdict, order and family table move together or not at all. Field names are exported
// verbatim from orderTuple's own shape.
type DemoOrderTuple struct {
	System  string
	Code    string
	Display string
	Dx      string
}

// DemoOrderSet is the exported mirror of originationProfile — one DemoOrderTuple per UC
// scenario the demo lane originates.
type DemoOrderSet struct {
	UC02, UC03, UC04, UC05, UC06, UC07, UC07HCPCS, UC08 DemoOrderTuple
}

// DemoOrderCodes returns the demo lane's per-UC order tuples for callers outside this
// package. It is the exported form of originationCodes() — a thin field-by-field copy of
// the SAME orderTuple values that function returns, never a second, independently
// maintained table. A future edit to originationCodes() (or to the DemoDisplay*/DemoDx*
// constants above) is automatically reflected here.
func DemoOrderCodes() DemoOrderSet {
	c := originationCodes()
	conv := func(o orderTuple) DemoOrderTuple {
		return DemoOrderTuple{System: o.system, Code: o.code, Display: o.display, Dx: o.dx}
	}
	return DemoOrderSet{
		UC02:      conv(c.uc02),
		UC03:      conv(c.uc03),
		UC04:      conv(c.uc04),
		UC05:      conv(c.uc05),
		UC06:      conv(c.uc06),
		UC07:      conv(c.uc07),
		UC07HCPCS: conv(c.uc07hcpcs),
		UC08:      conv(c.uc08),
	}
}
