// Package scaffold is a runnable Tier-3 starter for partners whose backend is NOT a
// FHIR server (HL7v2, X12, SQL, SOAP). It implements engine.SystemOfRecord with one
// wired demo persona so a clone boots and runs UC-01/UC-03 out of the box; every read
// body carries a // TODO(partner): marker showing where the real backend call goes.
//
// This is a DEMO/TEMPLATE connector. It is deliberately NOT wired into the gateway's
// build() env switch (gateway/app/app.go — the `if cfg.FHIRDataURL == ""` selection of
// memstub vs the generic FHIR connector) — a template carrying demo persona data must never be config-selectable
// in a production image. Reach it by clone-and-edit (see README) or the Tier-3 override
// test (test/tier3override). Real vendor connectors are the opposite: they SHOULD ship in
// the image and be config-selected (the future connector-registry track).
package scaffold

import (
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"

	"github.com/SmartHealthNetwork/shn-gateway/engine"
)

var _ engine.SystemOfRecord = (*Scaffold)(nil)

// Scaffold is a single-persona, in-memory SystemOfRecord skeleton. A partner replaces
// each method body with a read against their system of record.
type Scaffold struct{}

// New returns a Scaffold. A real connector's constructor takes a backend handle
// (DB pool, SOAP/X12 client, FHIR base URL) — see the README.
func New() *Scaffold { return &Scaffold{} }

// scaffoldMember is the one wired demo persona (UC-01/UC-03 subject MBR-COVERED).
// Demographics are IDENTICAL to the substrate stub (gateway/engine/holderdata.go:181)
// so the derived PCI matches and the provider→payer leg correlates. The ClinicalContext
// is identical EXCEPT ConservativeTherapyWeeks (9, not stub's 6) — the deliberate,
// observable marker the Tier-3 override test asserts surfaces in the DTR/PAS exchange.
// The marker is non-gating: weeks flows into the DTR QR/PAS Claim as documentation only
// (never an adjudication threshold — approval is driven by CoverageInforce), so its value
// is freely observable without changing the outcome.
const (
	scaffoldMember    = "MBR-COVERED"
	scaffoldBirthDate = "1975-04-02"
	scaffoldFamily    = "Johansson"
)

func scaffoldClinical() shnsdk.ClinicalContext {
	return shnsdk.ClinicalContext{
		ConditionCode:            "M51.16",
		ConditionRef:             "Condition/cond-m5116",
		ConservativeTherapyWeeks: 9, // marker: distinct from the stub's 6
		ConservativeTherapyRef:   "Observation/obs-pt-weeks",
		ConservativeDate:         "2026-05-20",
		NeuroDeficit:             false,
		NeuroDeficitRef:          "Observation/obs-neuro",
		PriorImaging:             true,
		PriorImagingRef:          "DiagnosticReport/dr-xray",
	}
}

// ResolvePatient returns the member's substrate PCI + demographics (AI-5: PCI is derived
// from member + demographics, never the bare member id).
func (s *Scaffold) ResolvePatient(memberID string) (pci string, demo engine.Demo, found bool) {
	// TODO(partner): look up memberID in your system of record; map to demographics.
	if memberID != scaffoldMember {
		return "", engine.Demo{}, false
	}
	demo = engine.Demo{BirthDate: scaffoldBirthDate, FamilyName: scaffoldFamily}
	return shnsdk.ResolvePCI(memberID, demo.BirthDate, demo.FamilyName), demo, true
}

// CoverageInforce reads the US Core Coverage RECORD (CMS-0057). The eligibility
// DETERMINATION is the payer's Adjudicator; this only reports the record state.
func (s *Scaffold) CoverageInforce(memberID string) (inforce bool, reason string) {
	// TODO(partner): read the member's coverage record from your backend.
	if memberID != scaffoldMember {
		return false, "unknown-member"
	}
	return true, ""
}

// ClinicalContext returns the structured clinical facts the gateway maps into the DTR
// QuestionnaireResponse and the PAS Claim.
func (s *Scaffold) ClinicalContext(memberID string) (shnsdk.ClinicalContext, bool) {
	// TODO(partner): assemble ClinicalContext from your clinical records.
	if memberID != scaffoldMember {
		return shnsdk.ClinicalContext{}, false
	}
	return scaffoldClinical(), true
}

// SupplementalReport returns a partner-specific supplemental document (e.g. operative
// report) for the UC-04 pended→approved path. The scaffold persona has none.
func (s *Scaffold) SupplementalReport(memberID string) ([]byte, bool) {
	// TODO(partner): return a supplemental document if your backend has one.
	return nil, false
}

// FacilityRecords returns targeted records for the UC-05 federated-query path. The
// scaffold persona (a provider subject) serves none.
func (s *Scaffold) FacilityRecords(memberID string) (records map[string][]byte, found bool) {
	// TODO(partner): on a facility deployment, return the requested records.
	return nil, false
}
