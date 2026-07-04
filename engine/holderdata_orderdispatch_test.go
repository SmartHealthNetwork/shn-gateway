package engine

import (
	"encoding/json"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// orderDispatchFixtures names the two provider-data seed personas that drive the
// crd-order-dispatch leg (originate_homeoxygen.go's originateDispatch — see its doc
// comment "It serves any seeded order-dispatch member: HomeOxygen = MBR-OX (E0431),
// UC-03 = MBR-PD-UC03 (E1390)"). Every other shnsdk.ProviderDataPersonas() fixture
// (uc01/uc01-nc/uc02/uc02-payerb/uc04/uc05/uc05-nc/uc06/uc07/uc08) drives the
// crd-order-select or PAS-tail legs instead — unaffected by D-S7K-13.
var orderDispatchFixtures = []string{"homeoxygen", "uc03"}

// TestStubPersonas_CoverOrderDispatchSeeds is the stub-vs-seeded persona-equivalence
// pin (Kit S7, D-S7K-13): the in-process memstub payer (StubHolderData/stubPersonas —
// what every gateway process boots on when FHIR_DATA_URL is unset, incl.
// test/harness.go's payer and Kit's counterparty in test/kitlive) must
// ResolvePatient every provider-data order-dispatch persona's member id, exactly like
// the hosted payer already does (internal/fhirseed's payer-tenant Coverage carries
// MBR-OX/MBR-PD-UC03 — see fhirseed.go's demographics + seedCoverage tables). Without
// this, the payer-side crd-order-dispatch inbound bind (conformantCRDDispatchBind,
// crd_dispatch_native.go) 400s "unknown member" even though the provider side (reading
// off a real or bring-your-own FHIR SoR) resolves the member fine — the drift D-S7K-13
// closes.
//
// The fixture NAME set is checked against shnsdk.ProviderDataPersonas() (the SDK's own
// exported census) so a rename there fails this test loudly instead of silently
// desyncing; the MEMBER ID asserted against the stub is read straight off each
// fixture's seeded Patient (urn:shn:member identifier) — never a hand-typed MBR-*
// literal — so a future persona addition can't silently drift the two censuses apart.
func TestStubPersonas_CoverOrderDispatchSeeds(t *testing.T) {
	all := shnsdk.ProviderDataPersonas()
	known := make(map[string]bool, len(all))
	for _, name := range all {
		known[name] = true
	}

	stub := NewStubHolderData()
	for _, name := range orderDispatchFixtures {
		if !known[name] {
			t.Fatalf("orderDispatchFixtures references %q, which is no longer in shnsdk.ProviderDataPersonas() %v — update the fixture list", name, all)
		}
		member := memberFromProviderDataBundle(t, name)
		if _, _, found := stub.ResolvePatient(member); !found {
			t.Errorf("stubPersonas has no entry for %q (provider-data persona %q): the in-process payer would 400 %q on the crd-order-dispatch leg (D-S7K-13)",
				member, name, "unknown member")
		}
	}
}

// memberFromProviderDataBundle loads a provider-data seed persona's transaction Bundle
// (shnsdk.ProviderDataBundle) and returns its Patient resource's urn:shn:member
// identifier value. Fails the test if the bundle carries no such Patient.
func memberFromProviderDataBundle(t *testing.T, persona string) string {
	t.Helper()
	raw, err := shnsdk.ProviderDataBundle(persona)
	if err != nil {
		t.Fatalf("shnsdk.ProviderDataBundle(%q): %v", persona, err)
	}
	var bundle struct {
		Entry []struct {
			Resource struct {
				ResourceType string `json:"resourceType"`
				Identifier   []struct {
					System string `json:"system"`
					Value  string `json:"value"`
				} `json:"identifier"`
			} `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(raw, &bundle); err != nil {
		t.Fatalf("parse provider-data bundle %q: %v", persona, err)
	}
	for _, e := range bundle.Entry {
		if e.Resource.ResourceType != "Patient" {
			continue
		}
		for _, id := range e.Resource.Identifier {
			if id.System == "urn:shn:member" {
				return id.Value
			}
		}
	}
	t.Fatalf("provider-data bundle %q: no Patient with a urn:shn:member identifier", persona)
	return ""
}
