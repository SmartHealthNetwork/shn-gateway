package engine

import (
	"maps"
	"slices"
	"testing"
)

// TestInboundLegsCoverCatalog: the inbound dispatcher's legs are exactly the
// catalog's, each keyed by its own transaction type, so a new catalog leg
// cannot have its request-frame refusals written bare for want of an entry,
// and PA's are the legs its handlers answer on.
func TestInboundLegsCoverCatalog(t *testing.T) {
	if got, want := slices.Sorted(maps.Keys(inboundLegs)), catalogLegs(); !slices.Equal(got, want) {
		t.Fatalf("inbound legs %v, catalog legs %v", got, want)
	}
	for tx, leg := range inboundLegs {
		if leg.tx != tx || leg.frame == "" || leg.op == "" {
			t.Errorf("%s: incomplete entry %+v", tx, leg)
		}
	}
	// The derived legs are PA's named ones, which its handlers pass.
	for _, l := range []inboundLeg{legCRDOrderSelect, legCRDOrderDispatch, legPASClaim, legPASClaimUpdate, legPASClaimInquire,
		legEligibility, legDTR, legFederatedQuery, legPatientDTR} {
		if inboundLegs[l.tx] != l {
			t.Errorf("%s: derived %+v, the handlers answer on %+v", l.tx, inboundLegs[l.tx], l)
		}
	}
}
