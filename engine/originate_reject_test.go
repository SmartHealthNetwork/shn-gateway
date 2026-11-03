//go:build !shn_coreonly

package engine

import (
	"context"
	"strings"
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

// REJECTION (honesty fence): an open order with NO recognized {CPT,HCPCS} product coding is
// refused at the PAS claim's build, the leg whose item names the product: the code MUST come
// from the SoR data, never be assumed. It is the fidelity of SHN's own build, whatever order
// the request states.
func TestStatedOrderFault_RefusesAnOrderWithoutProductCoding(t *testing.T) {
	noCoding := []byte(`{"resourceType":"ServiceRequest","id":"sr-x","status":"active"}`) // no code.coding
	for _, stated := range []*statedOrder{{System: "http://www.ama-assn.org/go/cpt", Code: "72148"}, {System: "https://bluebutton.cms.gov/resources/codesystem/hcpcs", Code: "E1390"}} {
		why := statedOrderFault(originationClaimInput{Order: stated}, noCoding)
		if !strings.Contains(why, "no recognized product coding") || !strings.Contains(why, "fidelity of SHN's own build") {
			t.Fatalf("statedOrderFault(no-coding order, stated %s) = %q, want the product-coding refusal", stated.Code, why)
		}
	}
}

// REJECTION (one-way-door guard): the honesty fence is NOT lane-scoped. orderSource
// used to read the system of record only under "provider-data" and BUILD the order
// from the tuple on every other lane — an order held in no participant's system, which
// no later inquiry could re-read. Every lane reads the order now, and carries the
// system's own bytes, whatever their coding: a mutation that re-narrowed the read to
// one profile would let another lane originate an order nobody holds again, and this
// row is what catches it. The product-coding fence itself (statedOrderFault) reads no
// lane.
func TestOrderSource_ReadsTheSystemsOrderOnEveryLane(t *testing.T) {
	noCoding := []byte(`{"resourceType":"ServiceRequest","id":"sr-x","status":"active"}`)
	for _, profile := range []string{"demo", "provider-data", "some-other-lane"} {
		g := &Gateway{cfg: Config{OriginationProfile: profile, SoR: fakeOrderSoR{censusSoR: newCensusSoR(), order: noCoding}}}
		order, status, msg, _ := g.orderSourceContext(context.Background(), "MBR-D-UC03", "", "")
		if status != 0 || string(order) != string(noCoding) {
			t.Fatalf("%s: orderSource = %d %q, %s; want the system's own order", profile, status, msg, order)
		}
	}
}
