package engine

import shnsdk "github.com/SmartHealthNetwork/shn-sdk"

// SystemOfRecord is the read seam over the holder's backing system of record (FR-1).
// The provider side resolves members and reads provider-LOCAL clinical context; the
// payer side reads coverage status; the facility side serves targeted records; the PHG
// side reads patient demographics for the patient surface. EVERY deployment supplies one:
// gateway/connectors/fhirsor (a FHIR/SMART-Backend-Services client over the holder's own
// US Core server) or a partner implementation. The engine ships none — the in-process
// persona census retired with the in-process payer stub (§4.1).
type SystemOfRecord interface {
	ResolvePatient(memberID string) (pci string, demo Demo, found bool)
	// PatientFHIRRef returns the FHIR Patient reference ("Patient/<id>") as known to the
	// backing FHIR store — the resolvable subject for an operated SDC Questionnaire/$populate
	// (which reads the store directly and cannot resolve the logical member ref or an
	// identifier-based subject). For a real FHIR SoR this is the store's (possibly scoped)
	// resource id; the in-memory stub returns the logical "Patient/<member>". found=false ⇒
	// the caller falls back to the logical ref.
	PatientFHIRRef(memberID string) (ref string, found bool)
	// CoverageInforce reads the US Core Coverage RECORD (CMS-0057: Coverage is a
	// FHIR record on the Patient/Provider Access APIs). The eligibility
	// DETERMINATION is the payer's decision — made by Adjudicator.Eligibility,
	// which consults this record (the record-vs-determination split:
	// an Adjudicator that has no verdict of its own for a member delegates straight here).
	CoverageInforce(memberID string) (inforce bool, reason string)
	ClinicalContext(memberID string) (shnsdk.ClinicalContext, bool)
	SupplementalReport(memberID string) ([]byte, bool)
	FacilityRecords(memberID string) (records map[string][]byte, found bool)
	// OpenOrder returns the member's open order resource bytes (a ServiceRequest or
	// DeviceRequest) for headless origination. found=false when none. The caller parses the
	// product coding via shnsdk.ParseOrderProductCoding — the gateway never synthesizes the order.
	OpenOrder(memberID string) (orderJSON []byte, found bool)
	// OpenCoverage returns the member's in-force Coverage record bytes (the payer-of-record
	// resource) for headless origination — the routing identity and the wire payload both
	// derive from it (FR-G40). found=false when none.
	OpenCoverage(memberID string) (coverageJSON []byte, found bool)
	// ResolveByReference returns the bytes of a resource named by a relative reference
	// (e.g. "Organization/dme-1") from the FHIR SoR. found=false when absent. Used to resolve
	// an order's performer (the DME supplier Organization) for headless order-dispatch origination.
	ResolveByReference(ref string) (resourceJSON []byte, found bool)
}

// Store is the gateway's own durable business state (metadata/decision only —
// AI-1-compatible: the payer tracking its own claims/EOBs/auth-numbers, never a
// cross-holder clinical record). The partner does not implement this; today it is the
// in-memory stub (demo) or delegated to holdersim (separated stack); a gateway-owned
// Postgres implementation is a later edge slice.
type Store interface {
	StoreAuthNumber(serviceRequestRef, preAuthRef string) error
	AuthNumber(serviceRequestRef string) (string, bool)

	RecordPendedClaim(subjectPCI, correlationID string) error
	BeginClaimUpdate(subjectPCI, correlationID string) (bool, error)
	ReleaseClaimUpdate(subjectPCI, correlationID string) error
	FinalizeClaimUpdate(subjectPCI, correlationID string) error

	RecordEOB(subjectPCI, eobID string, eobJSON []byte) error
	EOBsForPatient(subjectPCI string) ([][]byte, bool)
	EOBByID(eobID string) ([]byte, bool)
}

// The in-memory Store is the shipped default behind the Store seam; every
// SystemOfRecord is supplied by the deployment (gateway/connectors/fhirsor against a
// real US Core server, or a partner's own implementation) — the gateway ships no
// persona census of its own (§4.1).
var _ Store = (*MemStore)(nil)
