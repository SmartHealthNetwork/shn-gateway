//go:build !shn_coreonly

package engine

import (
	"encoding/json"
	"sort"
	"testing"
)

// TestRecordTransformed_ProvenanceRidesTheObserverForEveryContract pins that
// no contract tolerates a carried transform Provenance today: for every
// contract a compiled-in workstream's catalog names, the Provenance a
// transform builds rides the observer event, never the payload. A contract
// tolerates a carried Provenance only with live evidence; its workstream then
// answers through a workstream method, added with that evidence.
func TestRecordTransformed_ProvenanceRidesTheObserverForEveryContract(t *testing.T) {
	seen := map[string]bool{}
	var contracts []string
	for _, w := range workstreams {
		for _, spec := range w.catalog() {
			if spec.Contract != "" && !seen[spec.Contract] {
				seen[spec.Contract] = true
				contracts = append(contracts, spec.Contract)
			}
		}
	}
	sort.Strings(contracts)
	if len(contracts) == 0 {
		t.Fatal("no workstream catalog names a contract: the row would pass vacuously")
	}
	for _, contract := range contracts {
		t.Run(contract, func(t *testing.T) {
			var observed []ObserverEvent
			g := &Gateway{cfg: Config{
				HolderID: "test-holder", Clock: fixedEgressClock,
				Observer: func(e ObserverEvent) { observed = append(observed, e) },
			}}
			body := []byte(`{"resourceType":"Bundle","type":"collection"}`)
			ev := ObserverEvent{Kind: legTransformedKind, Detail: contract + " 2.0->2.1"}
			if err := g.recordTransformed(ev, ExchangeIdentity{CorrelationID: "corr-" + contract},
				contract, "2.0", "2.1", nil, nil, body, body); err != nil {
				t.Fatalf("recordTransformed: %v", err)
			}
			if len(observed) != 1 || observed[0].Kind != legTransformedKind {
				t.Fatalf("observed = %+v, want exactly one leg.transformed event", observed)
			}
			var prov struct {
				ResourceType string `json:"resourceType"`
			}
			if err := json.Unmarshal(observed[0].Payload, &prov); err != nil || prov.ResourceType != "Provenance" {
				t.Fatalf("%s: the observer event must carry the transform Provenance, got %q (err %v)",
					contract, observed[0].Payload, err)
			}
		})
	}
}
