package engine

import (
	"testing"
)

// composite/sandbox orderSource returns exactly BuildServiceRequestCoded(tuple) bytes —
// byte-identical to the pre-refactor call (composite must not regress).
func TestOrderSource_CompositeBuildsFromTuple(t *testing.T) {
	g := &Gateway{cfg: Config{OriginationProfile: "composite"}}
	patientRef := "Patient/MBR-COVERED"
	want, err := BuildServiceRequestCoded(systemHCPCSBuild, "E0250", "Hospital bed", "Z74.01", patientRef)
	if err != nil {
		t.Fatalf("baseline build: %v", err)
	}
	got, status, msg := g.orderSource("MBR-COVERED", patientRef, systemHCPCSBuild, "E0250", "Hospital bed", "Z74.01")
	if status != 0 {
		t.Fatalf("orderSource status=%d msg=%q, want 0", status, msg)
	}
	if string(got) != string(want) {
		t.Fatalf("orderSource(composite) bytes differ from BuildServiceRequestCoded — composite would regress")
	}
}

// provider-data orderSource reads the SoR open order; fail-closed when there is no order.
func TestOrderSource_ProviderDataNoOrder(t *testing.T) {
	g := &Gateway{cfg: Config{OriginationProfile: "provider-data", SoR: NewStubHolderData()}}
	_, status, _ := g.orderSource("MBR-X", "Patient/MBR-X", "", "", "", "")
	if status != 502 {
		t.Fatalf("orderSource(provider-data, no order) status=%d, want 502", status)
	}
}
