package engine

import "testing"

// TestOriginationCodes_Demo verifies the per-UC tuples exactly match §4.3's UC→family
// map: uc02=E0250, uc03=E1390 (R3 re-key onto the oxygen family, register §11 ruling
// (b)), uc04=uc05=uc06=uc07=G0151 (uc07 patient lane), uc07hcpcs=L8000 (its OWN tuple,
// decoupled from uc03 by R3 — see below), uc08=J3490 — every tuple HCPCS-coded
// (systemHCPCSBuild).
//
// The retired sibling row pinned the CPT/lumbar tuple and its default; both died with
// the in-process payer stub (§4.1) — there is now
// exactly one tuple set and originationCodes takes no profile.
func TestOriginationCodes_Demo(t *testing.T) {
	d := originationCodes()

	cases := []struct {
		name string
		got  orderTuple
		code string
	}{
		{"uc02", d.uc02, "E0250"},
		{"uc03", d.uc03, "E1390"},
		{"uc04", d.uc04, "G0151"},
		{"uc05", d.uc05, "G0151"},
		{"uc06", d.uc06, "G0151"},
		{"uc07", d.uc07, "G0151"},
		{"uc07hcpcs", d.uc07hcpcs, "L8000"},
		{"uc08", d.uc08, "J3490"},
	}
	for _, c := range cases {
		if c.got.system != systemHCPCSBuild {
			t.Errorf("demo %s.system = %q, want systemHCPCSBuild", c.name, c.got.system)
		}
		if c.got.code != c.code {
			t.Errorf("demo %s.code = %q, want %q", c.name, c.got.code, c.code)
		}
		if c.got.display == "" || c.got.dx == "" {
			t.Errorf("demo %s = %+v, display/dx must not be empty", c.name, c.got)
		}
	}

	// uc04/uc05/uc06/uc07 share the SAME G0151 tuple (§4.3: one family, four branch
	// drivers — pend→amend, pend+federated+amend, clinician attest, patient attest).
	if d.uc04 != d.uc05 || d.uc05 != d.uc06 || d.uc06 != d.uc07 {
		t.Fatalf("demo uc04/uc05/uc06/uc07 must share one G0151 tuple: %+v %+v %+v %+v",
			d.uc04, d.uc05, d.uc06, d.uc07)
	}
	// R3: uc07hcpcs DECOUPLES from uc03 — they used to be pinned equal (a happenstance of
	// the retired stub's L8000 reuse), but uc03 now rides a different family/hook shape
	// (order-DISPATCH E1390) while uc07hcpcs keeps its OWN order-SELECT L8000 exhibit
	// (the L8000 approve + Patient Access read-back proof) unchanged. Asserting they now
	// DIFFER, not match, is the row this decoupling replaces.
	if d.uc07hcpcs == d.uc03 {
		t.Fatalf("demo uc07hcpcs must NOT equal uc03's tuple any more (R3 decouples them): uc07hcpcs=%+v uc03=%+v", d.uc07hcpcs, d.uc03)
	}
	if d.uc07hcpcs.display != DemoDisplayL8000 || d.uc07hcpcs.dx != DemoDxL8000 {
		t.Fatalf("demo uc07hcpcs does not match its OWN DemoDisplayL8000/DemoDxL8000: %+v", d.uc07hcpcs)
	}
	// The exported display/dx constants (the demo persona roster reuses these) match what the map built.
	if d.uc02.display != DemoDisplayE0250 || d.uc02.dx != DemoDxE0250 {
		t.Fatalf("demo uc02 does not match DemoDisplayE0250/DemoDxE0250: %+v", d.uc02)
	}
	if d.uc03.display != DemoDisplayE1390UC03 || d.uc03.dx != DemoDxE1390UC03 {
		t.Fatalf("demo uc03 does not match DemoDisplayE1390UC03/DemoDxE1390UC03: %+v", d.uc03)
	}
	if d.uc04.display != DemoDisplayG0151 || d.uc04.dx != DemoDxG0151 {
		t.Fatalf("demo uc04 does not match DemoDisplayG0151/DemoDxG0151: %+v", d.uc04)
	}
	if d.uc08.display != DemoDisplayJ3490 || d.uc08.dx != DemoDxJ3490 {
		t.Fatalf("demo uc08 does not match DemoDisplayJ3490/DemoDxJ3490: %+v", d.uc08)
	}
}

// TestDemoOrderCodes_MirrorsOriginationCodes pins that the EXPORTED DemoOrderCodes() is a
// pure field-by-field copy of the unexported
// originationCodes() this package already trusts — the seam accountsvc's discovery
// descriptor derives a persona's advertised order through, so it can never independently
// drift from the tuples the engine itself originates.
func TestDemoOrderCodes_MirrorsOriginationCodes(t *testing.T) {
	want := originationCodes()
	got := DemoOrderCodes()

	conv := func(o orderTuple) DemoOrderTuple {
		return DemoOrderTuple{System: o.system, Code: o.code, Display: o.display, Dx: o.dx}
	}
	cases := []struct {
		name string
		got  DemoOrderTuple
		want DemoOrderTuple
	}{
		{"uc02", got.UC02, conv(want.uc02)},
		{"uc03", got.UC03, conv(want.uc03)},
		{"uc04", got.UC04, conv(want.uc04)},
		{"uc05", got.UC05, conv(want.uc05)},
		{"uc06", got.UC06, conv(want.uc06)},
		{"uc07", got.UC07, conv(want.uc07)},
		{"uc07hcpcs", got.UC07HCPCS, conv(want.uc07hcpcs)},
		{"uc08", got.UC08, conv(want.uc08)},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("DemoOrderCodes().%s = %+v, want %+v (originationCodes().%s)", c.name, c.got, c.want, c.name)
		}
	}
}
