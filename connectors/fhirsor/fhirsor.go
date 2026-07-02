// Package fhirsor implements engine.SystemOfRecord by reading a US Core FHIR server
// (via internal/fhirclient). All five SystemOfRecord methods are implemented:
// ResolvePatient + CoverageInforce + ClinicalContext + SupplementalReport +
// FacilityRecords. The SoR is fully wired for demo gateways.
package fhirsor

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	fhir "github.com/samply/golang-fhir-models/fhir-models/fhir"

	"github.com/SmartHealthNetwork/shn-gateway/engine"
	"github.com/SmartHealthNetwork/shn-gateway/internal/fhirclient"
)

var _ engine.SystemOfRecord = (*SoR)(nil)

// SoR reads a holder's US Core FHIR server. Locality (provider vs facility vs payer) is
// enforced by the partition URL passed to New; a single per-role SoR instance handles one
// partition (WithMemberSystem is no longer needed).
type SoR struct {
	fc *fhirclient.Client
}

// New returns a SoR over fc. The FHIR base URL embedded in fc determines partition locality.
func New(fc *fhirclient.Client) *SoR {
	return &SoR{fc: fc}
}

// NewFromURL builds a SoR over a FHIR base URL, constructing the FHIR client
// internally. This is the entry point for callers outside the gateway module
// (e.g. config-driven wiring or conformance tests) that have a URL + an optional
// pre-configured http.Client (e.g. carrying SMART Backend Services auth) rather
// than a pre-built fhirclient. hc==nil uses a default client.
func NewFromURL(baseURL string, hc *http.Client) *SoR {
	return New(fhirclient.New(baseURL, hc))
}

// resolvePatient returns the parsed Patient and its server id, or ok=false. Shared
// by ResolvePatient (demographics) and CoverageInforce (beneficiary lookup).
// Searches by shnsdk.MemberSystem identifier; partition locality is enforced by the
// base URL in s.fc.
//
// Uses context.Background because SystemOfRecord does not thread a ctx (threading
// request context/deadline into the FHIR reads is a tracked concern;
// holdersim.Client makes uncontexted calls today too). No caching: a caller that
// invokes both ResolvePatient and CoverageInforce on the same member pays two Patient
// searches — acceptable for single-request flows; revisit later if warranted.
func (s *SoR) resolvePatient(memberID string) (p fhir.Patient, id string, ok bool) {
	b, err := s.fc.Search(context.Background(), "Patient", url.Values{
		"identifier": {shnsdk.MemberSystem + "|" + memberID},
	})
	if err != nil {
		log.Printf("fhirsor: Patient search for %q: %v", memberID, err)
		return fhir.Patient{}, "", false
	}
	if b == nil || len(b.Entry) == 0 {
		return fhir.Patient{}, "", false
	}
	if err := json.Unmarshal(b.Entry[0].Resource, &p); err != nil {
		log.Printf("fhirsor: decode Patient: %v", err)
		return fhir.Patient{}, "", false
	}
	if p.Id == nil {
		return fhir.Patient{}, "", false
	}
	return p, *p.Id, true
}

// ResolvePatient turns a member id into a substrate PCI via the SAME shnsdk.ResolvePCI
// the stub uses, reading birthDate + family from the US Core Patient.
func (s *SoR) ResolvePatient(memberID string) (string, engine.Demo, bool) {
	p, _, ok := s.resolvePatient(memberID)
	if !ok {
		return "", engine.Demo{}, false
	}
	if p.BirthDate == nil || len(p.Name) == 0 || p.Name[0].Family == nil {
		log.Printf("fhirsor: Patient for %q missing birthDate/family", memberID)
		return "", engine.Demo{}, false
	}
	birth, family := *p.BirthDate, *p.Name[0].Family
	pci := shnsdk.ResolvePCI(memberID, birth, family)
	return pci, engine.Demo{BirthDate: birth, FamilyName: family}, true
}

// PatientFHIRRef returns "Patient/<store-id>" — the FHIR store's resource id for the member
// (resolved by identifier; the id may be partition-scoped). This is the resolvable subject for an
// operated $populate (which reads the store directly; the logical member ref and identifier-based
// subjects don't resolve).
func (s *SoR) PatientFHIRRef(memberID string) (string, bool) {
	_, id, ok := s.resolvePatient(memberID)
	if !ok {
		return "", false
	}
	return "Patient/" + id, true
}

// CoverageInforce reports whether the member's coverage is active. active → (true,"");
// any other status → (false,"coverage-terminated"); no coverage / unknown → (false,"").
func (s *SoR) CoverageInforce(memberID string) (bool, string) {
	_, pid, ok := s.resolvePatient(memberID)
	if !ok {
		return false, ""
	}
	b, err := s.fc.Search(context.Background(), "Coverage", url.Values{
		"beneficiary": {"Patient/" + pid},
	})
	if err != nil {
		log.Printf("fhirsor: Coverage search for %q: %v", memberID, err)
		return false, ""
	}
	if b == nil || len(b.Entry) == 0 {
		return false, ""
	}
	var cov fhir.Coverage
	if err := json.Unmarshal(b.Entry[0].Resource, &cov); err != nil {
		log.Printf("fhirsor: decode Coverage: %v", err)
		return false, ""
	}
	if cov.Status == fhir.FinancialResourceStatusCodesActive {
		return true, ""
	}
	// Single non-active default: cancelled/draft/entered-in-error all map to
	// "coverage-terminated" (the stub's only not-in-force reason string, so equivalence
	// holds). Finer per-status reasons are out of scope at this layer; a later refinement
	// could split them — do not tighten this without updating the stub-equivalence contract.
	return false, "coverage-terminated"
}

// ClinicalContext reads the provider-LOCAL DTR-prefill facts from the holder's US Core FHIR
// server (FR-15). found=false when there is no anchoring Condition (mirrors the stub's
// hasClinical). Per field: resource found => (value, "Type/id"); absent => (zero, ""). PriorSurgery
// is code-aware: searches Procedures keyed on shnsdk.ProcedureValueSet (Flag 4). The remaining
// demo-faithful heuristics are the Condition-anchor found and the ODI-presence HighDisability.
// PatientReported is the workflow-routing REQUIREMENT signal ("this case requires a
// patient-attested functional-status item" — what DTR auto-fill keys off, dtr.go), sourced from
// the SHN-local urn:shn:clinical-context|patient-reported-required Observation. This supersedes
// an earlier design that conflated this requirement flag with the QR-signature-time
// attestation ACT — the act stays patient-authored via the PHG (FR-27), unchanged; only the
// requirement signal is FHIR-sourced. The SHN-local code is a workflow convention, not a
// canonical DTR pre-fill code (no canonical code exists; same pattern as conservative-therapy-
// weeks / neuro-deficit).
func (s *SoR) ClinicalContext(memberID string) (shnsdk.ClinicalContext, bool) {
	_, pid, ok := s.resolvePatient(memberID)
	if !ok {
		return shnsdk.ClinicalContext{}, false
	}
	code, ref, hasCond := s.conditionCode(pid)
	if !hasCond {
		return shnsdk.ClinicalContext{}, false
	}
	cc := shnsdk.ClinicalContext{ConditionCode: code, ConditionRef: ref}
	if weeks, date, r, found := s.obsQuantity(pid, shnsdk.SystemSHNClinical, shnsdk.ConservativeTherapyWeeksCode); found {
		cc.ConservativeTherapyWeeks, cc.ConservativeDate, cc.ConservativeTherapyRef = weeks, date, r
	}
	if val, r, found := s.obsBool(pid, shnsdk.SystemSHNClinical, shnsdk.NeuroDeficitCode); found {
		cc.NeuroDeficit, cc.NeuroDeficitRef = val, r
	}
	if r, found := s.firstRef(pid, "DiagnosticReport", url.Values{"code": {shnsdk.SystemCPT + "|" + shnsdk.ImagingCPT}}); found {
		cc.PriorImaging, cc.PriorImagingRef = true, r
	}
	procTokens := make([]string, len(shnsdk.ProcedureValueSet))
	for i, c := range shnsdk.ProcedureValueSet {
		procTokens[i] = shnsdk.SystemSNOMED + "|" + c
	}
	if r, found := s.firstRef(pid, "Procedure", url.Values{"code": {strings.Join(procTokens, ",")}}); found {
		cc.PriorSurgery, cc.PriorSurgeryRef = true, r
	}
	if r, found := s.firstRef(pid, "Observation", url.Values{"code": {shnsdk.SystemLOINC + "|" + shnsdk.ODICode}}); found {
		cc.HighDisability, cc.HighDisabilityRef = true, r
	}
	if val, _, found := s.obsBool(pid, shnsdk.SystemSHNClinical, shnsdk.PatientReportedCode); found && val {
		cc.PatientReported = true
	}
	return cc, true
}

// firstRef returns "Type/id" of the first entry of a patient-scoped search, or ok=false.
func (s *SoR) firstRef(patientID, resourceType string, extra url.Values) (string, bool) {
	q := url.Values{"patient": {patientID}}
	for k, vs := range extra {
		q[k] = vs
	}
	b, err := s.fc.Search(context.Background(), resourceType, q)
	if err != nil {
		log.Printf("fhirsor: %s search for %q: %v", resourceType, patientID, err)
		return "", false
	}
	if b == nil || len(b.Entry) == 0 {
		return "", false
	}
	var probe struct {
		ResourceType string `json:"resourceType"`
		Id           string `json:"id"`
	}
	if json.Unmarshal(b.Entry[0].Resource, &probe) != nil || probe.Id == "" || probe.ResourceType == "" {
		return "", false
	}
	return probe.ResourceType + "/" + probe.Id, true
}

func (s *SoR) conditionCode(patientID string) (code, ref string, ok bool) {
	b, err := s.fc.Search(context.Background(), "Condition", url.Values{"patient": {patientID}})
	if err != nil {
		log.Printf("fhirsor: Condition search for %q: %v", patientID, err)
		return "", "", false
	}
	if b == nil || len(b.Entry) == 0 {
		return "", "", false
	}
	var c fhir.Condition
	if json.Unmarshal(b.Entry[0].Resource, &c) != nil || c.Id == nil || c.Code == nil {
		return "", "", false
	}
	for _, cd := range c.Code.Coding {
		if cd.System != nil && *cd.System == shnsdk.SystemICD10CM && cd.Code != nil {
			return *cd.Code, "Condition/" + *c.Id, true
		}
	}
	return "", "", false
}

func (s *SoR) obsByCode(patientID, system, code string) (fhir.Observation, string, bool) {
	b, err := s.fc.Search(context.Background(), "Observation", url.Values{
		"patient": {patientID}, "code": {system + "|" + code},
	})
	if err != nil {
		log.Printf("fhirsor: Observation(%s) search for %q: %v", code, patientID, err)
		return fhir.Observation{}, "", false
	}
	if b == nil || len(b.Entry) == 0 {
		return fhir.Observation{}, "", false
	}
	var o fhir.Observation
	if json.Unmarshal(b.Entry[0].Resource, &o) != nil || o.Id == nil {
		return fhir.Observation{}, "", false
	}
	return o, "Observation/" + *o.Id, true
}

func (s *SoR) obsQuantity(patientID, system, code string) (weeks int, date, ref string, ok bool) {
	o, r, found := s.obsByCode(patientID, system, code)
	if !found || o.ValueQuantity == nil || o.ValueQuantity.Value == nil {
		return 0, "", "", false
	}
	f, err := o.ValueQuantity.Value.Float64()
	if err != nil {
		return 0, "", "", false
	}
	if o.EffectiveDateTime != nil {
		date = *o.EffectiveDateTime
	}
	return int(f), date, r, true
}

func (s *SoR) obsBool(patientID, system, code string) (val bool, ref string, ok bool) {
	o, r, found := s.obsByCode(patientID, system, code)
	if !found || o.ValueBoolean == nil {
		return false, "", false
	}
	return *o.ValueBoolean, r, true
}

// SupplementalReport returns the provider-LOCAL supplemental report DiagnosticReport for the
// member (FR-32), found by any code in shnsdk.ReportValueSet (imaging 18748-4 or operative
// 11504-8 — Flag 3), to disambiguate it from the prior-imaging X-ray. Raw bytes to attach.
//
// The returned resource has its subject.reference rewritten to "Patient/<memberID>" so that
// payer-side bundle-consistency checks (bindBundleSubject H2/H3) match the Claim patient
// reference, which always uses the canonical member ID form. HAPI stores resources with
// client-assigned scoped IDs (e.g. "Patient/pat-mbruc04-provider") that differ from the
// member ID used throughout the substrate protocol layer.
func (s *SoR) SupplementalReport(memberID string) ([]byte, bool) {
	_, pid, ok := s.resolvePatient(memberID)
	if !ok {
		return nil, false
	}
	tokens := make([]string, len(shnsdk.ReportValueSet))
	for i, c := range shnsdk.ReportValueSet {
		tokens[i] = shnsdk.SystemLOINC + "|" + c
	}
	raw, found := s.firstResourceBytes("DiagnosticReport", url.Values{
		"patient": {pid}, "code": {strings.Join(tokens, ",")},
	})
	if !found {
		return nil, false
	}
	return rewriteSubject(raw, "Patient/"+memberID), true
}

// FacilityRecords returns the external facility's records for the member, keyed by FHIR resource
// type (FR-24, UC-05). Searched by TYPE (no code filter) — production-faithful; served by the
// facility holder's fhirsor over its own base. Unknown member / no records => false.
//
// Each resource's subject.reference is rewritten to "Patient/<memberID>" (same rationale as
// SupplementalReport: HAPI-scoped IDs differ from the canonical member ID).
func (s *SoR) FacilityRecords(memberID string) (map[string][]byte, bool) {
	_, pid, ok := s.resolvePatient(memberID)
	if !ok {
		return nil, false
	}
	out := map[string][]byte{}
	for _, rtype := range []string{"DiagnosticReport", "DocumentReference"} {
		if raw, found := s.firstResourceBytes(rtype, url.Values{"patient": {pid}}); found {
			out[rtype] = rewriteSubject(raw, "Patient/"+memberID)
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// rewriteSubject returns resourceJSON with "subject":{"reference":"<ref>"} overwritten.
// Used to normalize HAPI-internal patient IDs to the canonical "Patient/<memberID>" form
// that the substrate protocol layer uses for bundle-internal patient consistency checks
// (the payer's bindBundleSubject, H2/H3).
//
// NOTE — two refs, two rules (do NOT "simplify" this away): only the SUBJECT is canonicalized.
// The resource's OWN id is left untouched and stays server-assigned, because Flag 1 derives the
// Provenance target from it via resourceRef (DiagnosticReport/<server-id>). So in the emitted
// bundle a disclosed report carries subject=Patient/<memberID> (member-canonical, for H2/H3) AND
// is targeted by Provenance as DiagnosticReport/<server-id> (server-canonical, for Flag 1).
// Both coexist correctly; collapsing them re-breaks one of the two checks.
//
// If the JSON cannot be parsed or re-marshalled, the original bytes are returned unchanged
// (fail-open: caller's validation will catch any downstream inconsistency). An absent subject
// is ADDED, not skipped — moot in practice since US Core DiagnosticReport/DocumentReference
// always carry one.
func rewriteSubject(resourceJSON []byte, ref string) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(resourceJSON, &m); err != nil {
		return resourceJSON
	}
	sub, _ := json.Marshal(map[string]string{"reference": ref})
	m["subject"] = json.RawMessage(sub)
	out, err := json.Marshal(m)
	if err != nil {
		return resourceJSON
	}
	return out
}

// OpenOrder returns the member's open order resource bytes for headless origination (FR-A3).
// Searches DeviceRequest first (DME/HME orders, e.g. HomeOxygen), then ServiceRequest
// (procedure orders), both scoped to patient + status=active. found=false when neither yields a
// result or the patient cannot be resolved. The caller parses the product coding via
// shnsdk.ParseOrderProductCoding — the gateway never synthesizes the order.
//
// The returned order's subject.reference is rewritten to "Patient/<memberID>" (same rationale as
// SupplementalReport/FacilityRecords): HAPI stores the order with a partition-scoped subject
// (e.g. "Patient/pat-mbrox-provider"), but the substrate protocol layer + the payer-side AI-11
// order-dispatch bind resolve the patient by the canonical MEMBER id. Without this the payer-gw's
// conformantCRDDispatchBind cannot resolve the dispatched order's subject → 403 "inconsistent
// patient in order-dispatch". The order's id + performer are preserved (the handler reads them
// to build the dispatchedOrders ref + resolve the supplier).
func (s *SoR) OpenOrder(memberID string) ([]byte, bool) {
	_, pid, ok := s.resolvePatient(memberID)
	if !ok {
		return nil, false
	}
	for _, rtype := range []string{"DeviceRequest", "ServiceRequest"} {
		if raw, found := s.firstResourceBytes(rtype, url.Values{
			"patient": {pid}, "status": {"active"},
		}); found {
			return rewriteSubject(raw, "Patient/"+memberID), true
		}
	}
	return nil, false
}

// OpenCoverage returns the member's Coverage record bytes (FR-G40 routing + payload source):
// the same beneficiary-scoped Coverage search as CoverageInforce, but returning the raw
// resource bytes rather than the in-force determination. found=false when the patient cannot
// be resolved or no Coverage is on file.
func (s *SoR) OpenCoverage(memberID string) ([]byte, bool) {
	_, pid, ok := s.resolvePatient(memberID)
	if !ok {
		return nil, false
	}
	b, err := s.fc.Search(context.Background(), "Coverage", url.Values{
		"beneficiary": {"Patient/" + pid},
	})
	if err != nil {
		log.Printf("fhirsor: Coverage search for %q: %v", memberID, err)
		return nil, false
	}
	if b == nil || len(b.Entry) == 0 {
		return nil, false
	}
	return b.Entry[0].Resource, true
}

// ResolveByReference returns the raw bytes of a resource named by a relative reference
// (e.g. "Organization/dme-1") via a direct FHIR read (GET {type}/{id}).
// found=false when the resource is absent (404) or a transport/parse error occurs (logged).
// Used to resolve an order's performer (the DME supplier Organization) for headless
// order-dispatch origination.
func (s *SoR) ResolveByReference(ref string) ([]byte, bool) {
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		log.Printf("fhirsor: ResolveByReference: invalid ref %q (want Type/id)", ref)
		return nil, false
	}
	body, found, err := s.fc.Read(context.Background(), parts[0], parts[1])
	if err != nil {
		log.Printf("fhirsor: ResolveByReference %q: %v", ref, err)
		return nil, false
	}
	return body, found
}

// firstResourceBytes returns the raw bytes of the first entry of a patient-scoped search, or
// ok=false (no match / transport error, logged — fail-safe per the SystemOfRecord contract).
func (s *SoR) firstResourceBytes(resourceType string, q url.Values) ([]byte, bool) {
	b, err := s.fc.Search(context.Background(), resourceType, q)
	if err != nil {
		log.Printf("fhirsor: %s search: %v", resourceType, err)
		return nil, false
	}
	if b == nil || len(b.Entry) == 0 {
		return nil, false
	}
	return []byte(b.Entry[0].Resource), true
}
