//go:build !shn_coreonly

package engine

import (
	"maps"
	"slices"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/internal/legtaxonomy"
)

// TestLegInvolvement_IsTheLegTaxonomys: on every leg of every taxonomy this
// build compiles in, the engine's involvement (legInvolvement, each
// workstream's own table) is the leg's involved column, request and answer:
// neither where the row has none. The taxonomy is the gateway's embedded copy
// of the network's leg taxonomy, byte-identical to the root module's (its
// hash fence), so a change to the column or to an adapter's table alone fails
// here. Every leg a compiled-in catalog declares has a row, and every leg the
// PA adapter's table names is one, so neither side can name a leg the other
// lacks.
func TestLegInvolvement_IsTheLegTaxonomys(t *testing.T) {
	rows := map[string]*legtaxonomy.Involvement{}
	for _, tx := range append([]legtaxonomy.Taxonomy{legtaxonomy.PA(), legtaxonomy.FQS()}, profileTaxonomies()...) {
		for _, l := range tx.Legs {
			rows[l.TransactionType] = l.Involved
		}
	}
	involved := 0
	for _, leg := range slices.Sorted(maps.Keys(rows)) {
		var wantRequest, wantAnswer bool
		if v := rows[leg]; v != nil {
			wantRequest, wantAnswer = v.Request, v.Answer
			involved++
		}
		if request, answer := legInvolvement(leg); request != wantRequest || answer != wantAnswer {
			t.Errorf("leg %q: the engine's involvement is request %v, answer %v; the taxonomy's involved column says request %v, answer %v", leg, request, answer, wantRequest, wantAnswer)
		}
	}
	if involved == 0 {
		t.Fatal("no taxonomy leg has an involved column: the fence would hold vacuously")
	}
	for _, w := range workstreams {
		for _, leg := range slices.Sorted(maps.Keys(w.catalog())) {
			if _, ok := rows[leg]; !ok {
				t.Errorf("workstream %s: catalog leg %q has no taxonomy row to hold its involvement to", w.id(), leg)
			}
		}
	}
	for _, leg := range slices.Sorted(maps.Keys(paRequestInvolvedLegs)) {
		if _, ok := rows[leg]; !ok {
			t.Errorf("the PA adapter's involved table names %q, which is no taxonomy leg", leg)
		}
	}
}
