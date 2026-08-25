package engine

import (
	"strings"
	"testing"
)

// A non-provider-data orderSource returns exactly BuildServiceRequestCoded(tuple)
// bytes — byte-identical to the pre-refactor call (the tuple lanes must not regress).
func TestOrderSource_DefaultBuildsFromTuple(t *testing.T) {
	g := &Gateway{cfg: Config{OriginationProfile: "demo"}}
	patientRef := "Patient/MBR-COVERED"
	want, err := BuildServiceRequestCoded(systemCPTBuild, "72148", "MRI lumbar spine w/o contrast", "M51.16", patientRef)
	if err != nil {
		t.Fatalf("baseline build: %v", err)
	}
	got, status, msg := g.orderSource("MBR-COVERED", patientRef, systemCPTBuild, "72148", "MRI lumbar spine w/o contrast", "M51.16")
	if status != 0 {
		t.Fatalf("orderSource status=%d msg=%q, want 0", status, msg)
	}
	if string(got) != string(want) {
		t.Fatalf("orderSource(demo) bytes differ from BuildServiceRequestCoded — the tuple lane would regress")
	}
}

// provider-data orderSource reads the SoR open order; fail-closed when there is no order.
func TestOrderSource_ProviderDataNoOrder(t *testing.T) {
	g := &Gateway{cfg: Config{OriginationProfile: "provider-data", SoR: newCensusSoR()}}
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
	*censusSoR
}

func (s *noCodingSoR) OpenOrder(memberID string) ([]byte, bool) {
	// A syntactically valid ServiceRequest whose only coding is SNOMED (not CPT/HCPCS).
	return []byte(`{"resourceType":"ServiceRequest","id":"sr-nocode","status":"active","intent":"order","code":{"coding":[{"system":"http://snomed.info/sct","code":"123456","display":"not a product code"}]},"subject":{"reference":"Patient/MBR-X"}}`), true
}

// OpenCoverage is inherited from the embedded censusSoR (this test drives orderSource
// directly, not a full origination handler, so OpenCoverage is never invoked).

func TestOrderSource_ProviderDataOrderNoRecognizedCoding(t *testing.T) {
	g := &Gateway{cfg: Config{OriginationProfile: "provider-data", SoR: &noCodingSoR{newCensusSoR()}}}
	_, status, msg := g.orderSource("MBR-X", "Patient/MBR-X", "", "", "", "")
	if status != 502 {
		t.Fatalf("orderSource(provider-data, order w/ no recognized coding) status=%d msg=%q, want 502", status, msg)
	}
	if !strings.Contains(msg, "no recognized product coding") {
		t.Fatalf("orderSource msg=%q, want it to mention 'no recognized product coding'", msg)
	}
}
