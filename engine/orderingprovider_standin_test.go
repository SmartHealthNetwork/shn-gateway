package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/fhirseed"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// TestOrderingProviderStandInMatchesTheSeededRecord: the census stand-in serves
// the SAME requesting-provider record the baked demo seed writes into a real
// participant's system.
//
// A stand-in more permissive — or simply different — than the thing it stands in
// for is green here and red live. This one is load-bearing: every order the
// gateway authors names OrderingProviderRef, and the party the prior
// authorization identifies is whatever that reference resolves to. If the
// stand-in served an organization with a different NPI, every hermetic row would
// pass while a live payer stored an authorization under a party this repository
// never seeded.
func TestOrderingProviderStandInMatchesTheSeededRecord(t *testing.T) {
	var bundle struct {
		Entry []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(fhirseed.DemoProviderPersonasBundle(), &bundle); err != nil {
		t.Fatalf("read the baked demo provider seed: %v", err)
	}
	wantID := strings.TrimPrefix(OrderingProviderRef, "Organization/")
	var seeded []byte
	for _, e := range bundle.Entry {
		var head struct {
			ResourceType string `json:"resourceType"`
			ID           string `json:"id"`
		}
		if json.Unmarshal(e.Resource, &head) == nil && head.ResourceType == "Organization" && head.ID == wantID {
			seeded = e.Resource
		}
	}
	if seeded == nil {
		t.Fatalf("the baked demo seed carries no %s — every order this gateway authors names it, so every origination would be refused", OrderingProviderRef)
	}
	if got := shnsdk.PASProviderNPI(seeded); got != censusOrderingProviderNPI {
		t.Fatalf("the seeded requesting provider carries NPI %q and this package's stand-in serves %q — hermetically green, live wrong",
			got, censusOrderingProviderNPI)
	}
	// And the stand-in really does serve it under that reference.
	served, ok := newCensusSoR().ResolveByReference(OrderingProviderRef)
	if !ok {
		t.Fatalf("the stand-in does not hold %s", OrderingProviderRef)
	}
	if got := shnsdk.PASProviderNPI(served); got != censusOrderingProviderNPI {
		t.Fatalf("the stand-in served NPI %q", got)
	}
}
