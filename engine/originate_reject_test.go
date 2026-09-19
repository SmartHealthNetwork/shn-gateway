package engine

import (
	"context"
	"testing"
)

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
	_, status, _ := g.orderSourceContext(context.Background(), "MBR-X", "", "")
	if status != 502 {
		t.Fatalf("no-coding order status=%d, want 502 (fail closed)", status)
	}
}

// REJECTION (one-way-door guard): the honesty fence is NOT lane-scoped. orderSource
// used to read the system of record only under "provider-data" and BUILD the order
// from the tuple on every other lane — an order held in no participant's system, which
// no later inquiry could re-read. Every lane reads the order now, so the fence that
// refuses an order with no recognized {CPT,HCPCS} product coding must bite on every
// lane too: a mutation that re-narrowed the read to one profile would let another lane
// originate an order nobody holds again, and this row is what catches it.
func TestOrderSource_HonestyFenceAppliesToEveryLane(t *testing.T) {
	noCoding := []byte(`{"resourceType":"ServiceRequest","id":"sr-x","status":"active"}`)
	for _, profile := range []string{"demo", "provider-data", "some-other-lane"} {
		g := &Gateway{cfg: Config{OriginationProfile: profile, SoR: fakeOrderSoR{censusSoR: newCensusSoR(), order: noCoding}}}
		_, status, msg := g.orderSourceContext(context.Background(), "MBR-D-UC03", "", "")
		if status != 502 {
			t.Fatalf("%s: no-coding order status=%d msg=%q, want 502 (fail closed on every lane)", profile, status, msg)
		}
	}
}
