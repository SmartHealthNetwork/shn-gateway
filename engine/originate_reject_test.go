package engine

import "testing"

// fakeOrderSoR returns a fixed open order regardless of member.
type fakeOrderSoR struct {
	*censusSoR
	order []byte
}

func (f fakeOrderSoR) OpenOrder(string) ([]byte, bool) { return f.order, true }

// OpenCoverage is inherited from the embedded censusSoR (this test drives orderSource
// directly, not a full origination handler, so OpenCoverage is never invoked).

// REJECTION (honesty fence): a provider-data open order with NO recognized {CPT,HCPCS} product
// coding must fail closed — the order code MUST come from the SoR data, never be assumed.
func TestOrderSource_RejectsOrderWithoutProductCoding(t *testing.T) {
	noCoding := []byte(`{"resourceType":"ServiceRequest","id":"sr-x","status":"active"}`) // no code.coding
	g := &Gateway{cfg: Config{OriginationProfile: "provider-data", SoR: fakeOrderSoR{censusSoR: newCensusSoR(), order: noCoding}}}
	_, status, _ := g.orderSource("MBR-X", "Patient/MBR-X", "", "", "", "")
	if status != 502 {
		t.Fatalf("no-coding order status=%d, want 502 (fail closed)", status)
	}
}

// REJECTION (one-way-door guard): demo must NOT fall into provider-data's SoR-read
// branch. orderSource's ONLY profile check is the literal "provider-data" — a mutation
// that widened it (e.g. to "anything but provider-data") would silently route the demo lane
// through OpenOrder, which the demo persona roster does not seed orders for (it builds its
// order from the originationCodes tuple — §4.3). Prove it by
// wiring a SoR whose OpenOrder always fails the honesty fence above (no product coding)
// and asserting demo still succeeds — it must never reach OpenOrder at all.
func TestOrderSource_DemoBuildsFromTuple_NeverReadsSoR(t *testing.T) {
	noCoding := []byte(`{"resourceType":"ServiceRequest","id":"sr-x","status":"active"}`)
	g := &Gateway{cfg: Config{OriginationProfile: "demo", SoR: fakeOrderSoR{censusSoR: newCensusSoR(), order: noCoding}}}
	srJSON, status, msg := g.orderSource("MBR-D-UC03", "Patient/MBR-D-UC03", systemHCPCSBuild, "L8000", DemoDisplayL8000, DemoDxL8000)
	if status != 0 {
		t.Fatalf("demo orderSource must build from the tuple (never touch the fake SoR's bad order): status=%d msg=%q", status, msg)
	}
	if len(srJSON) == 0 {
		t.Fatal("demo orderSource returned no bytes")
	}
}
