package engine

import "testing"

// fakeOrderSoR returns a fixed open order regardless of member.
type fakeOrderSoR struct {
	*StubHolderData
	order []byte
}

func (f fakeOrderSoR) OpenOrder(string) ([]byte, bool) { return f.order, true }

// REJECTION (honesty fence): a provider-data open order with NO recognized {CPT,HCPCS} product
// coding must fail closed — the order code MUST come from the SoR data, never be assumed.
func TestOrderSource_RejectsOrderWithoutProductCoding(t *testing.T) {
	noCoding := []byte(`{"resourceType":"ServiceRequest","id":"sr-x","status":"active"}`) // no code.coding
	g := &Gateway{cfg: Config{OriginationProfile: "provider-data", SoR: fakeOrderSoR{StubHolderData: NewStubHolderData(), order: noCoding}}}
	_, status, _ := g.orderSource("MBR-X", "Patient/MBR-X", "", "", "", "")
	if status != 502 {
		t.Fatalf("no-coding order status=%d, want 502 (fail closed)", status)
	}
}
