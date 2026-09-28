package engine

import (
	"maps"
	"slices"
	"testing"
)

// TestInboundLegsCoverCatalog: the inbound dispatcher's legs are exactly the
// catalog's, each keyed by its own transaction type, so a new catalog leg
// cannot have its request-frame refusals written bare for want of an entry.
func TestInboundLegsCoverCatalog(t *testing.T) {
	if got, want := slices.Sorted(maps.Keys(inboundLegs)), slices.Sorted(maps.Keys(paCatalog)); !slices.Equal(got, want) {
		t.Fatalf("inbound legs %v, catalog legs %v", got, want)
	}
	for tx, leg := range inboundLegs {
		if leg.tx != tx || leg.frame == "" || leg.op == "" {
			t.Errorf("%s: incomplete entry %+v", tx, leg)
		}
	}
}
