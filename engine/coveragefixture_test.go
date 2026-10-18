package engine

import (
	"bytes"
	"encoding/json"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// containedPayorCoverage is a member's Coverage record as a synthetic fixture
// system holds it (id "c1", the MB identifier, a contained payer Organization
// the payor references): fixture data a stand-in system of record returns,
// byte-identical to what the deprecated shnsdk.BuildCoverageWithPayer used to
// build. The root module's twin is pasfixture.ContainedPayorCoverage.
// The error is always nil; the shape matches the builder call sites it replaces.
func containedPayorCoverage(patientRef, memberID string, payer shnsdk.PayerIdentifier) ([]byte, error) {
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	return []byte(`{"beneficiary":{"reference":` + q(patientRef) + `},"contained":[{"id":"cms-payer","identifier":[{"system":` +
		q(payer.System) + `,"value":` + q(payer.Value) + `}],"name":"Centers for Medicare and Medicaid Services","resourceType":"Organization"}],` +
		`"id":"c1","identifier":[{"type":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/v2-0203","code":"MB"}]},"system":"urn:shn:coverage","value":` +
		q(memberID) + `}],"meta":{"profile":["http://hl7.org/fhir/us/core/StructureDefinition/us-core-coverage"]},"payor":[{"reference":"#cms-payer"}],` +
		`"relationship":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/subscriber-relationship","code":"self"}]},"resourceType":"Coverage","status":"active"}`), nil
}

func TestContainedPayorCoverageMatchesTheDeprecatedBuilder(t *testing.T) {
	payer := shnsdk.PayerIdentifier{System: "urn:oid:2.16.840.1.113883.6.300", Value: "00078"}
	want, err := shnsdk.BuildCoverageWithPayer("Patient/MBR-1", "MBR-1", payer)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := containedPayorCoverage("Patient/MBR-1", "MBR-1", payer); !bytes.Equal(got, want) {
		t.Fatalf("fixture drifted:\n got %s\nwant %s", got, want)
	}
}
