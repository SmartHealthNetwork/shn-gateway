package engine

import (
	"strings"
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

// noCodingSoR embeds the standard stub and overrides OpenOrder to return a ServiceRequest whose
// code is in a NON-{CPT,HCPCS} system — so ParseOrderProductCoding finds no recognized product
// coding. Proves orderSource fails closed (502) rather than originating an order whose code does
// not trace to a recognized product coding (the provider-data honesty guard).
type noCodingSoR struct {
	*StubHolderData
}

func (s *noCodingSoR) OpenOrder(memberID string) ([]byte, bool) {
	// A syntactically valid ServiceRequest whose only coding is SNOMED (not CPT/HCPCS).
	return []byte(`{"resourceType":"ServiceRequest","id":"sr-nocode","status":"active","intent":"order","code":{"coding":[{"system":"http://snomed.info/sct","code":"123456","display":"not a product code"}]},"subject":{"reference":"Patient/MBR-X"}}`), true
}

func TestOrderSource_ProviderDataOrderNoRecognizedCoding(t *testing.T) {
	g := &Gateway{cfg: Config{OriginationProfile: "provider-data", SoR: &noCodingSoR{NewStubHolderData()}}}
	_, status, msg := g.orderSource("MBR-X", "Patient/MBR-X", "", "", "", "")
	if status != 502 {
		t.Fatalf("orderSource(provider-data, order w/ no recognized coding) status=%d msg=%q, want 502", status, msg)
	}
	if !strings.Contains(msg, "no recognized product coding") {
		t.Fatalf("orderSource msg=%q, want it to mention 'no recognized product coding'", msg)
	}
}
