// order_build.go — gateway-local stub for deferral D-PCB-1 (build-side product-coding gap). shnsdk.BuildServiceRequest
// is CPT-system-locked, so the gateway cannot build a HCPCS order via the published SDK.
// This gateway-local GENERIC builder takes the system explicitly — the EXACT shape the SDK
// will eventually get (BuildServiceRequestCoded). When a real partner consumer needs to build
// HCPCS orders via the SDK, lift this into sdk/order.go (additive) and honor the D-PCB-1
// rejection test before recording it closed. Parity-pinned to shnsdk.BuildServiceRequest for
// CPT (test/sdkparity/order_coded_parity_test.go) so it cannot drift before the pull-in.
package engine

import (
	"encoding/json"

	fhir "github.com/samply/golang-fhir-models/fhir-models/fhir"
)

// These system URI constants MUST equal the canonical wire values pinned in sdk/order.go
// and accepted by shnsdk.ParseServiceRequestProductCoding. The FHIR validator does NOT
// check system URI values, so a drift would silently produce a wrong-system order.
// Guarded by TestBuildServiceRequestCoded_SystemConstsAnchoredToParser (order_build_test.go).
const (
	systemCPTBuild              = "http://www.ama-assn.org/go/cpt"
	systemICD10Build            = "http://hl7.org/fhir/sid/icd-10-cm"
	systemHCPCSBuild            = "http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets"
	profileUSCoreServiceRequest = "http://hl7.org/fhir/us/core/StructureDefinition/us-core-servicerequest"
)

// BuildServiceRequestCoded builds a DRAFT US-Core ServiceRequest with an explicit procedure
// code system (CPT or HCPCS). Generic form of shnsdk.BuildServiceRequest (deferral D-PCB-1).
func BuildServiceRequestCoded(system, code, display, dxCode, patientRef string) ([]byte, error) {
	sr := fhir.ServiceRequest{
		Meta:   &fhir.Meta{Profile: []string{profileUSCoreServiceRequest}},
		Status: fhir.RequestStatusDraft,
		Intent: fhir.RequestIntentOrder,
		Code: &fhir.CodeableConcept{
			Coding: []fhir.Coding{
				{
					System:  strPtr(system),
					Code:    strPtr(code),
					Display: strPtr(display),
				},
			},
		},
		ReasonCode: []fhir.CodeableConcept{
			{
				Coding: []fhir.Coding{
					{
						System: strPtr(systemICD10Build),
						Code:   strPtr(dxCode),
					},
				},
			},
		},
		Subject: fhir.Reference{
			Reference: strPtr(patientRef),
		},
	}
	return json.Marshal(sr)
}

func strPtr(s string) *string { return &s }
