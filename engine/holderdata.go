package engine

import (
	"sync"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// Demo holds the demographics a holder knows about a member, used to derive the
// substrate PCI (AI-5: PCI is computed from member + demographics, never the bare
// member ID).
type Demo struct {
	BirthDate, FamilyName string
}

// StubHolderData is the DEMO SystemOfRecord + Store implementation: a hermetic in-memory
// store with personas for UC-01 and the DTR/PAS slice. Its persona data
// (stubPersonas) is read-only/immutable demo data; the auth-number store is
// mutable and guarded by a mutex. Construct with NewStubHolderData.
//
// A real HAPI-FHIR-backed (or DB) adapter plugs in behind the SystemOfRecord + Store
// seams with NO gateway change (purely additive); that production adapter is
// the deferred Phase-2 work.
type StubHolderData struct {
	mu          sync.Mutex
	authNumbers map[string]string
	// pendedClaims is the payer-side pended-claim ledger keyed by
	// subjectPCI + "|" + correlationID. The value is the status: claimPended (awaiting
	// supplemental data) or claimInProgress (a ClaimUpdate is being adjudicated for
	// it). An absent key is "no such claim" (never pended, or already approved).
	// Metadata only (FR-21/FR-6; AI-1-compatible).
	pendedClaims map[string]claimStatus
	// eobsByPCI is the payer-side PA-decision EOB store keyed by subject PCI
	// (UC-08 Patient Access API, FR-28). Metadata/decision only — AI-1-compatible.
	eobsByPCI map[string][][]byte
	eobByID   map[string][]byte
}

type claimStatus int

const (
	claimPended     claimStatus = iota + 1 // awaiting supplemental data
	claimInProgress                        // a ClaimUpdate is mid-adjudication
)

// NewStubHolderData returns a ready-to-use StubHolderData with an initialized
// auth-number store and pended-claim ledger.
func NewStubHolderData() *StubHolderData {
	return &StubHolderData{
		authNumbers:  make(map[string]string),
		pendedClaims: make(map[string]claimStatus),
		eobsByPCI:    make(map[string][][]byte),
		eobByID:      make(map[string][]byte),
	}
}

// pendedKey is the ledger key for a (subjectPCI, correlationID) pair.
func pendedKey(subjectPCI, correlationID string) string {
	return subjectPCI + "|" + correlationID
}

// RecordPendedClaim records a pended claim. Safe for concurrent use.
func (d *StubHolderData) RecordPendedClaim(subjectPCI, correlationID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pendedClaims[pendedKey(subjectPCI, correlationID)] = claimPended
	return nil
}

// BeginClaimUpdate ATOMICALLY claims a pended claim for a ClaimUpdate: if it is
// currently pended it transitions it to in-progress and returns true; otherwise
// (never pended, already approved, or another update already in progress) it
// returns false. This single test-and-set is the FR-6 current-state authority check
// AND the mutual-exclusion that serializes concurrent updates for the same claim —
// only one update can be in flight. The caller must pair it with FinalizeClaimUpdate
// (on approval) or ReleaseClaimUpdate (on any non-approval). Safe for concurrent use.
func (d *StubHolderData) BeginClaimUpdate(subjectPCI, correlationID string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	k := pendedKey(subjectPCI, correlationID)
	if d.pendedClaims[k] != claimPended {
		return false, nil
	}
	d.pendedClaims[k] = claimInProgress
	return true, nil
}

// ReleaseClaimUpdate returns an in-progress claim to pended (a ClaimUpdate did NOT
// approve — e.g. still insufficient or a validation error — so a later, complete
// amendment can still transition it). Safe for concurrent use.
func (d *StubHolderData) ReleaseClaimUpdate(subjectPCI, correlationID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	k := pendedKey(subjectPCI, correlationID)
	if d.pendedClaims[k] == claimInProgress {
		d.pendedClaims[k] = claimPended
	}
	return nil
}

// FinalizeClaimUpdate completes the pended→approved transition: it removes the
// claim so a replayed update for it finds nothing (replay protection). Safe for
// concurrent use.
func (d *StubHolderData) FinalizeClaimUpdate(subjectPCI, correlationID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.pendedClaims, pendedKey(subjectPCI, correlationID))
	return nil
}

// RecordEOB stores a PA-decision EOB for a patient (keyed by subject PCI) and
// indexes it by EOB id for read-by-id. Stores a COPY of the bytes. Safe for
// concurrent use.
func (d *StubHolderData) RecordEOB(subjectPCI, eobID string, eobJSON []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	cp := make([]byte, len(eobJSON))
	copy(cp, eobJSON)
	d.eobsByPCI[subjectPCI] = append(d.eobsByPCI[subjectPCI], cp)
	d.eobByID[eobID] = cp
	return nil
}

// EOBsForPatient returns all stored EOBs for a patient PCI (search), or ok=false
// when none are stored. Returns defensive copies (a fresh slice of fresh byte
// slices) so a caller cannot mutate stored state. Safe for concurrent use.
func (d *StubHolderData) EOBsForPatient(subjectPCI string) ([][]byte, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	v, ok := d.eobsByPCI[subjectPCI]
	if !ok || len(v) == 0 {
		return nil, false
	}
	out := make([][]byte, len(v))
	for i, b := range v {
		cp := make([]byte, len(b))
		copy(cp, b)
		out[i] = cp
	}
	return out, true
}

// EOBByID returns one stored EOB by its id (read), or ok=false. Returns a
// defensive copy so a caller cannot mutate stored bytes. Safe for concurrent use.
func (d *StubHolderData) EOBByID(eobID string) ([]byte, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	v, ok := d.eobByID[eobID]
	if !ok {
		return nil, false
	}
	cp := make([]byte, len(v))
	copy(cp, v)
	return cp, true
}

// Reset clears all MUTABLE holder state — the auth-number store and the
// pended-claim ledger — back to clean synthetic state (the demo reset contract).
// The read-only persona fixtures are unaffected. Safe for concurrent use.
func (d *StubHolderData) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.authNumbers = make(map[string]string)
	d.pendedClaims = make(map[string]claimStatus)
	d.eobsByPCI = make(map[string][][]byte)
	d.eobByID = make(map[string][]byte)
}

type persona struct {
	demo    Demo
	inforce bool
	reason  string
	// clinical is the provider-LOCAL clinical context; hasClinical is false
	// for personas with no clinical data to auto-fill from.
	clinical    shnsdk.ClinicalContext
	hasClinical bool
}

var stubPersonas = map[string]persona{
	"MBR-COVERED": {
		demo:    Demo{BirthDate: "1975-04-02", FamilyName: "Johansson"},
		inforce: true,
		reason:  "",
		clinical: shnsdk.ClinicalContext{
			ConditionCode:            "M51.16",
			ConditionRef:             "Condition/cond-m5116",
			ConservativeTherapyWeeks: 6,
			ConservativeTherapyRef:   "Observation/obs-pt-weeks",
			ConservativeDate:         "2026-05-20",
			NeuroDeficit:             false,
			NeuroDeficitRef:          "Observation/obs-neuro",
			PriorImaging:             true,
			PriorImagingRef:          "DiagnosticReport/dr-xray",
		},
		hasClinical: true,
	},
	"MBR-NOTCOVERED": {
		demo:    Demo{BirthDate: "1980-09-15", FamilyName: "Reyes"},
		inforce: false,
		reason:  "coverage-terminated",
	},
	// MBR-UC04 (Maria Chen) — UC-04 pended-on-missing-DiagnosticReport (FR-35/39).
	// PriorSurgery=true triggers the pend; weeks=6 means it approves once the
	// operative report (SupplementalReport) is attached via ClaimUpdate (FR-32).
	"MBR-UC04": {
		demo:    Demo{BirthDate: "1982-11-03", FamilyName: "Chen"},
		inforce: true,
		clinical: shnsdk.ClinicalContext{
			ConditionCode:            "M51.16",
			ConditionRef:             "Condition/cond-m5116",
			ConservativeTherapyWeeks: 6,
			ConservativeTherapyRef:   "Observation/obs-pt-weeks",
			ConservativeDate:         "2026-05-20",
			NeuroDeficit:             false,
			NeuroDeficitRef:          "Observation/obs-neuro",
			PriorImaging:             true,
			PriorImagingRef:          "DiagnosticReport/dr-xray",
			PriorSurgery:             true,
			PriorSurgeryRef:          "Procedure/proc-laminectomy",
		},
		hasClinical: true,
	},
	// MBR-UC06 (David Reyes) — UC-06 pended-on-missing-attested-functional-status
	// (FR-35/39). HighDisability=true triggers the pend; weeks=6 means it approves
	// once the clinician-attested functional-status item is supplied (manual entry
	// path — no SupplementalReport for this member).
	"MBR-UC06": {
		demo:    Demo{BirthDate: "1969-07-21", FamilyName: "Reyes"},
		inforce: true,
		clinical: shnsdk.ClinicalContext{
			ConditionCode:            "M51.16",
			ConditionRef:             "Condition/cond-m5116",
			ConservativeTherapyWeeks: 6,
			ConservativeTherapyRef:   "Observation/obs-pt-weeks",
			ConservativeDate:         "2026-05-20",
			NeuroDeficit:             false,
			NeuroDeficitRef:          "Observation/obs-neuro",
			PriorImaging:             true,
			PriorImagingRef:          "DiagnosticReport/dr-xray",
			HighDisability:           true,
			HighDisabilityRef:        "Observation/obs-odi",
		},
		hasClinical: true,
	},
	// MBR-UC05 (Linda Johansson) — UC-05 federated EXTERNAL retrieval. PriorSurgery
	// pends the PA; the operative report is NOT local (SupplementalReport returns
	// false), forcing the consent-gated federated query to metro-spine.
	"MBR-UC05": {
		demo:    Demo{BirthDate: "1968-03-12", FamilyName: "Johansson"},
		inforce: true,
		clinical: shnsdk.ClinicalContext{
			ConditionCode:            "M51.16",
			ConditionRef:             "Condition/cond-m5116",
			ConservativeTherapyWeeks: 6,
			ConservativeTherapyRef:   "Observation/obs-pt-weeks",
			ConservativeDate:         "2026-05-20",
			NeuroDeficit:             false,
			NeuroDeficitRef:          "Observation/obs-neuro",
			PriorImaging:             true,
			PriorImagingRef:          "DiagnosticReport/dr-xray",
			PriorSurgery:             true,
			PriorSurgeryRef:          "Procedure/proc-microdiscectomy",
		},
		hasClinical: true,
	},
	// MBR-UC08 — UC-08 denial-driver: only 4 weeks of conservative therapy (< 6),
	// no prior surgery, not high-disability → Adjudicate returns Denied (FR-22/35).
	// NeuroDeficitRef is included so the auto-filled QR carries a non-empty
	// information-origin sourceReference (a missing Ref produces an empty reference
	// that fails live FHIR validation — real UC-05 bug, condition M51.16).
	"MBR-UC08": {
		demo:    Demo{BirthDate: "1971-02-09", FamilyName: "Okafor"},
		inforce: true,
		clinical: shnsdk.ClinicalContext{
			ConditionCode:            "M51.16",
			ConditionRef:             "Condition/cond-m5116",
			ConservativeTherapyWeeks: 4,
			ConservativeTherapyRef:   "Observation/obs-pt-weeks",
			ConservativeDate:         "2026-05-20",
			NeuroDeficit:             false,
			NeuroDeficitRef:          "Observation/obs-neuro",
			PriorImaging:             true,
			PriorImagingRef:          "DiagnosticReport/dr-xray",
		},
		hasClinical: true,
	},
	// MBR-UC07HCPCS: the in-process mirror of the two-RI L8000 DV-approve.
	// HCPCS order (L8000); a DIRECT CRD→DTR→PAS approve (NOT patient-authorship — that
	// distinctive stays UC-07/MBR-UC07). DEF-4 stub (AI-9): it answers the REUSED lumbar
	// questionnaire (clinical fixture smell — a HCPCS order on an MSK questionnaire — accepted;
	// the fidelity that matters is the HCPCS order code → HCPCS EOB → render). weeks=6, no
	// pend trigger → SandboxAdjudicate approves on the first submit.
	"MBR-UC07HCPCS": {
		demo:    Demo{BirthDate: "1977-01-30", FamilyName: "Nakamura"},
		inforce: true,
		clinical: shnsdk.ClinicalContext{
			ConditionCode:            "M51.16",
			ConditionRef:             "Condition/cond-m5116",
			ConservativeTherapyWeeks: 6,
			ConservativeTherapyRef:   "Observation/obs-pt-weeks",
			ConservativeDate:         "2026-05-20",
			NeuroDeficit:             false,
			NeuroDeficitRef:          "Observation/obs-neuro",
			PriorImaging:             true,
			PriorImagingRef:          "DiagnosticReport/dr-xray",
		},
		hasClinical: true,
	},
	// MBR-UC07 (Nadia Haddad) — UC-07 patient-entry: pends on a missing PATIENT-reported
	// (patient-attested) functional status; approves once the patient authors + attests
	// it via the Trust-operated PHG (FR-18/27). NeuroDeficitRef is included so the
	// auto-filled QR carries a non-empty information-origin sourceReference (a missing
	// Ref produces an empty reference that fails live FHIR validation — real UC-05 bug).
	"MBR-UC07": {
		demo:    Demo{BirthDate: "1990-08-25", FamilyName: "Haddad"},
		inforce: true,
		clinical: shnsdk.ClinicalContext{
			ConditionCode:            "M51.16",
			ConditionRef:             "Condition/cond-m5116",
			ConservativeTherapyWeeks: 6,
			ConservativeTherapyRef:   "Observation/obs-pt-weeks",
			ConservativeDate:         "2026-05-20",
			NeuroDeficit:             false,
			NeuroDeficitRef:          "Observation/obs-neuro",
			PriorImaging:             true,
			PriorImagingRef:          "DiagnosticReport/dr-xray",
			PatientReported:          true,
		},
		hasClinical: true,
	},
	// MBR-UC05-NOCONSENT — Linda's no-consent twin: same shape, distinct demographics
	// (distinct PCI) so consentsvc has NO standing permit → the federated query is
	// denied and the PA stays pended (the no-consent branch, DoD).
	"MBR-UC05-NOCONSENT": {
		demo:    Demo{BirthDate: "1968-03-12", FamilyName: "Johansson-Noconsent"},
		inforce: true,
		clinical: shnsdk.ClinicalContext{
			ConditionCode:            "M51.16",
			ConditionRef:             "Condition/cond-m5116",
			ConservativeTherapyWeeks: 6,
			ConservativeTherapyRef:   "Observation/obs-pt-weeks",
			NeuroDeficit:             false,
			PriorImaging:             true,
			PriorImagingRef:          "DiagnosticReport/dr-xray",
			PriorSurgery:             true,
			PriorSurgeryRef:          "Procedure/proc-microdiscectomy",
		},
		hasClinical: true,
	},
}

// ResolvePatient returns the member's PCI and demographics. Unknown members yield
// found=false.
func (d *StubHolderData) ResolvePatient(memberID string) (pci string, demo Demo, found bool) {
	p, ok := stubPersonas[memberID]
	if !ok {
		return "", Demo{}, false
	}
	pci = shnsdk.ResolvePCI(memberID, p.demo.BirthDate, p.demo.FamilyName)
	return pci, p.demo, true
}

// PatientFHIRRef — the in-memory stub uses logical refs (no FHIR store / scoped ids).
func (d *StubHolderData) PatientFHIRRef(memberID string) (string, bool) {
	if _, ok := stubPersonas[memberID]; !ok {
		return "", false
	}
	return "Patient/" + memberID, true
}

// CoverageInforce answers whether the member's coverage is in force. Unknown
// members are treated as not in force.
func (d *StubHolderData) CoverageInforce(memberID string) (inforce bool, reason string) {
	p, ok := stubPersonas[memberID]
	if !ok {
		return false, ""
	}
	return p.inforce, p.reason
}

// ClinicalContext returns the provider-LOCAL clinical context for a member.
// Members with no clinical data (not-covered, unknown) yield found=false.
func (d *StubHolderData) ClinicalContext(memberID string) (shnsdk.ClinicalContext, bool) {
	p, ok := stubPersonas[memberID]
	if !ok || !p.hasClinical {
		return shnsdk.ClinicalContext{}, false
	}
	return p.clinical, true
}

// StoreAuthNumber records the payer-issued pre-auth number for a service request
// reference. Safe for concurrent use.
func (d *StubHolderData) StoreAuthNumber(serviceRequestRef, preAuthRef string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.authNumbers[serviceRequestRef] = preAuthRef
	return nil
}

// AuthNumber returns a previously stored pre-auth number, or found=false. Safe
// for concurrent use.
func (d *StubHolderData) AuthNumber(serviceRequestRef string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ref, ok := d.authNumbers[serviceRequestRef]
	return ref, ok
}

// SupplementalReport returns the provider-LOCAL supplemental DiagnosticReport
// for MBR-UC04 (UC-04, FR-32). All other members yield found=false.
// NOTE: MBR-UC05 deliberately yields false — the operative report is EXTERNAL
// (held by metro-spine), which is what forces the consent-gated federated query.
func (d *StubHolderData) SupplementalReport(memberID string) ([]byte, bool) {
	if memberID != "MBR-UC04" {
		return nil, false
	}
	raw, err := shnsdk.BuildDiagnosticReport("dr-uc04-operative", "Patient/MBR-UC04", "72148", "MRI lumbar spine w/o contrast")
	if err != nil {
		return nil, false
	}
	return raw, true
}

// FacilityRecords returns metro-spine's held records for MBR-UC05 (UC-05): the
// operative DiagnosticReport and its DocumentReference. All other members yield
// found=false. The provider does NOT hold these — they are retrieved by the
// consent-gated federated query (the non-aggregation showcase).
func (d *StubHolderData) FacilityRecords(memberID string) (map[string][]byte, bool) {
	if memberID != "MBR-UC05" {
		return nil, false
	}
	patientRef := "Patient/MBR-UC05"
	dr, err := shnsdk.BuildDiagnosticReport("dr-uc05-operative", patientRef, "72148", "Operative report — lumbar microdiscectomy")
	if err != nil {
		return nil, false
	}
	docref, err := shnsdk.BuildDocumentReference("docref-uc05-operative", patientRef, "DiagnosticReport/dr-uc05-operative")
	if err != nil {
		return nil, false
	}
	return map[string][]byte{"DiagnosticReport": dr, "DocumentReference": docref}, true
}
