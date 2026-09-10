// Package fhirsor implements engine.SystemOfRecord by reading a US Core FHIR server
// (via internal/fhirclient). Contextual reads preserve backend errors; legacy
// SystemOfRecord methods discard them only for compatibility.
package fhirsor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
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
// Request context reaches each nested read. Reads are not cached.
func (s *SoR) resolvePatient(ctx context.Context, memberID string) (p fhir.Patient, id string, ok bool, readErr error) {
	b, err := s.fc.Search(ctx, "Patient", url.Values{
		"identifier": {shnsdk.MemberSystem + "|" + memberID},
	})
	if err != nil {
		return fhir.Patient{}, "", false, safeReadError(err)
	}
	if b != nil {
		if len(b.Entry) > 1 || (b.Total != nil && int(*b.Total) != len(b.Entry)) {
			return fhir.Patient{}, "", false, invalidResponse()
		}
		for _, link := range b.Link {
			if link.Relation == "next" {
				return fhir.Patient{}, "", false, invalidResponse()
			}
		}
	}
	if b == nil || len(b.Entry) == 0 {
		return fhir.Patient{}, "", false, nil
	}
	if err := json.Unmarshal(b.Entry[0].Resource, &p); err != nil {
		return fhir.Patient{}, "", false, invalidResponse()
	}
	if p.Id == nil || *p.Id == "" {
		return fhir.Patient{}, "", false, invalidResponse()
	}
	return p, *p.Id, true, nil
}

// ResolvePatient turns a member id into a substrate PCI via the SAME shnsdk.ResolvePCI
// the stub uses, reading birthDate + family from the US Core Patient.
func (s *SoR) ResolvePatientContext(ctx context.Context, memberID string) (string, engine.Demo, bool, error) {
	p, _, ok, err := s.resolvePatient(ctx, memberID)
	if err != nil {
		return "", engine.Demo{}, false, safeReadError(err)
	}
	if !ok {
		return "", engine.Demo{}, false, nil
	}
	if p.BirthDate == nil || len(p.Name) == 0 || p.Name[0].Family == nil || *p.BirthDate == "" || *p.Name[0].Family == "" {
		return "", engine.Demo{}, false, invalidResponse()
	}
	birth, family := *p.BirthDate, *p.Name[0].Family
	pci := shnsdk.ResolvePCI(memberID, birth, family)
	return pci, engine.Demo{BirthDate: birth, FamilyName: family}, true, nil
}

// PatientFHIRRef returns "Patient/<store-id>" — the FHIR store's resource id for the member
// (resolved by identifier; the id may be partition-scoped). This is the resolvable subject for an
// operated $populate (which reads the store directly; the logical member ref and identifier-based
// subjects don't resolve).
func (s *SoR) PatientFHIRRefContext(ctx context.Context, memberID string) (string, bool, error) {
	_, id, ok, err := s.resolvePatient(ctx, memberID)
	if err != nil {
		return "", false, safeReadError(err)
	}
	if !ok {
		return "", false, nil
	}
	return "Patient/" + id, true, nil
}

// CoverageInforce reports whether the member's coverage is active. active → (true,"");
// any other status → (false,"coverage-terminated"); no coverage / unknown → (false,"").
func (s *SoR) CoverageInforceContext(ctx context.Context, memberID string) (bool, string, error) {
	_, pid, ok, err := s.resolvePatient(ctx, memberID)
	if err != nil {
		return false, "", safeReadError(err)
	}
	if !ok {
		return false, "", nil
	}
	b, err := s.fc.Search(ctx, "Coverage", url.Values{
		"beneficiary": {"Patient/" + pid},
	})
	if err != nil {
		return false, "", safeReadError(err)
	}
	if b == nil || len(b.Entry) == 0 {
		return false, "", nil
	}
	// The model's zero enum is active, so absence must be checked before decoding.
	var status struct {
		Status *string `json:"status"`
	}
	if json.Unmarshal(b.Entry[0].Resource, &status) != nil || status.Status == nil || *status.Status == "" {
		return false, "", invalidResponse()
	}
	var cov fhir.Coverage
	if err := json.Unmarshal(b.Entry[0].Resource, &cov); err != nil {
		return false, "", invalidResponse()
	}
	if cov.Status == fhir.FinancialResourceStatusCodesActive {
		return true, "", nil
	}
	// Single non-active default: cancelled/draft/entered-in-error all map to
	// "coverage-terminated" (the stub's only not-in-force reason string, so equivalence
	// holds). Finer per-status reasons are out of scope at this layer; a later refinement
	// could split them — do not tighten this without updating the stub-equivalence contract.
	return false, "coverage-terminated", nil
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
//
// OxygenSaturationPct/ArterialPaO2mmHg (task-B, register §4/FR-17) — the HomeOxygen-family
// facts UC-03's re-key (R3) reads: real Observation searches by LOINC 59408-5 (pulse-ox
// O₂-sat) and 2703-7 (arterial PaO₂), mirroring internal/fixturesor's hermetic equivalent
// field-for-field. Before this, this live connector never read them at all, so
// gateway/engine's homeOxygenAutoFillEvidence cross-check was structurally DEAD against any
// real FHIR server: cc.OxygenSaturationRef/ArterialPaO2Ref were always "" here, so no QR
// answer could ever be attributed Origin="auto" outside the hermetic fixture SoR — the
// stand-in was MORE capable than the real thing. Absent Observation yields
// the honest zero value ("", ""), never fabricated; the caller's cross-check then falls back
// to unattributed (neither auto nor a forced manual answer), exactly the fixture's contract.
// A subsidiary read error returns no clinical context.
func (s *SoR) ClinicalContextContext(ctx context.Context, memberID string) (shnsdk.ClinicalContext, bool, error) {
	_, pid, ok, err := s.resolvePatient(ctx, memberID)
	if err != nil {
		return shnsdk.ClinicalContext{}, false, safeReadError(err)
	}
	if !ok {
		return shnsdk.ClinicalContext{}, false, nil
	}
	code, ref, hasCond, err := s.conditionCode(ctx, pid)
	if err != nil {
		return shnsdk.ClinicalContext{}, false, safeReadError(err)
	}
	if !hasCond {
		return shnsdk.ClinicalContext{}, false, nil
	}
	cc := shnsdk.ClinicalContext{ConditionCode: code, ConditionRef: ref}
	weeks, date, r, found, err := s.obsQuantity(ctx, pid, shnsdk.SystemSHNClinical, shnsdk.ConservativeTherapyWeeksCode)
	if err != nil {
		return shnsdk.ClinicalContext{}, false, safeReadError(err)
	}
	if found {
		cc.ConservativeTherapyWeeks, cc.ConservativeDate, cc.ConservativeTherapyRef = weeks, date, r
	}
	val, r, found, err := s.obsBool(ctx, pid, shnsdk.SystemSHNClinical, shnsdk.NeuroDeficitCode)
	if err != nil {
		return shnsdk.ClinicalContext{}, false, safeReadError(err)
	}
	if found {
		cc.NeuroDeficit, cc.NeuroDeficitRef = val, r
	}
	r, found, err = s.firstRef(ctx, pid, "DiagnosticReport", url.Values{"code": {shnsdk.SystemCPT + "|" + shnsdk.ImagingCPT}})
	if err != nil {
		return shnsdk.ClinicalContext{}, false, safeReadError(err)
	}
	if found {
		cc.PriorImaging, cc.PriorImagingRef = true, r
	}
	procTokens := make([]string, len(shnsdk.ProcedureValueSet))
	for i, c := range shnsdk.ProcedureValueSet {
		procTokens[i] = shnsdk.SystemSNOMED + "|" + c
	}
	r, found, err = s.firstRef(ctx, pid, "Procedure", url.Values{"code": {strings.Join(procTokens, ",")}})
	if err != nil {
		return shnsdk.ClinicalContext{}, false, safeReadError(err)
	}
	if found {
		cc.PriorSurgery, cc.PriorSurgeryRef = true, r
	}
	r, found, err = s.firstRef(ctx, pid, "Observation", url.Values{"code": {shnsdk.SystemLOINC + "|" + shnsdk.ODICode}})
	if err != nil {
		return shnsdk.ClinicalContext{}, false, safeReadError(err)
	}
	if found {
		cc.HighDisability, cc.HighDisabilityRef = true, r
	}
	val, _, found, err = s.obsBool(ctx, pid, shnsdk.SystemSHNClinical, shnsdk.PatientReportedCode)
	if err != nil {
		return shnsdk.ClinicalContext{}, false, safeReadError(err)
	}
	if found && val {
		cc.PatientReported = true
	}
	// R3 — HomeOxygen-family facts (UC-03's re-key), read the SAME way as the fields above:
	// a code-token Observation search, first match, quantity value + reference. See the doc
	// comment above for why this closes the live-fidelity gap.
	quantity, _, r, found, err := s.obsQuantity(ctx, pid, shnsdk.SystemLOINC, shnsdk.OxygenSaturationLOINC)
	if err != nil {
		return shnsdk.ClinicalContext{}, false, safeReadError(err)
	}
	if found {
		cc.OxygenSaturationPct, cc.OxygenSaturationRef = strconv.Itoa(quantity), r
	}
	quantity, _, r, found, err = s.obsQuantity(ctx, pid, shnsdk.SystemLOINC, shnsdk.ArterialPaO2LOINC)
	if err != nil {
		return shnsdk.ClinicalContext{}, false, safeReadError(err)
	}
	if found {
		cc.ArterialPaO2mmHg, cc.ArterialPaO2Ref = strconv.Itoa(quantity), r
	}
	return cc, true, nil
}

// firstRef returns "Type/id" of the first entry of a patient-scoped search, or ok=false.
func (s *SoR) firstRef(ctx context.Context, patientID, resourceType string, extra url.Values) (string, bool, error) {
	q := url.Values{"patient": {patientID}}
	for k, vs := range extra {
		q[k] = vs
	}
	b, err := s.fc.Search(ctx, resourceType, q)
	if err != nil {
		return "", false, safeReadError(err)
	}
	if b == nil || len(b.Entry) == 0 {
		return "", false, nil
	}
	var probe struct {
		ResourceType string `json:"resourceType"`
		Id           string `json:"id"`
	}
	if json.Unmarshal(b.Entry[0].Resource, &probe) != nil || probe.Id == "" || probe.ResourceType == "" {
		return "", false, invalidResponse()
	}
	return probe.ResourceType + "/" + probe.Id, true, nil
}

func (s *SoR) conditionCode(ctx context.Context, patientID string) (code, ref string, ok bool, readErr error) {
	b, err := s.fc.Search(ctx, "Condition", url.Values{"patient": {patientID}})
	if err != nil {
		return "", "", false, safeReadError(err)
	}
	if b == nil || len(b.Entry) == 0 {
		return "", "", false, nil
	}
	var c fhir.Condition
	if json.Unmarshal(b.Entry[0].Resource, &c) != nil || c.Id == nil {
		return "", "", false, invalidResponse()
	}
	if c.Code == nil {
		return "", "", false, nil
	}
	for _, cd := range c.Code.Coding {
		if cd.System != nil && *cd.System == shnsdk.SystemICD10CM && cd.Code != nil {
			return *cd.Code, "Condition/" + *c.Id, true, nil
		}
	}
	return "", "", false, nil
}

func (s *SoR) obsByCode(ctx context.Context, patientID, system, code string) (fhir.Observation, string, bool, error) {
	b, err := s.fc.Search(ctx, "Observation", url.Values{
		"patient": {patientID}, "code": {system + "|" + code},
	})
	if err != nil {
		return fhir.Observation{}, "", false, safeReadError(err)
	}
	if b == nil || len(b.Entry) == 0 {
		return fhir.Observation{}, "", false, nil
	}
	var o fhir.Observation
	if json.Unmarshal(b.Entry[0].Resource, &o) != nil || o.Id == nil {
		return fhir.Observation{}, "", false, invalidResponse()
	}
	return o, "Observation/" + *o.Id, true, nil
}

func (s *SoR) obsQuantity(ctx context.Context, patientID, system, code string) (weeks int, date, ref string, ok bool, readErr error) {
	o, r, found, err := s.obsByCode(ctx, patientID, system, code)
	if err != nil {
		return 0, "", "", false, safeReadError(err)
	}
	if !found || o.ValueQuantity == nil || o.ValueQuantity.Value == nil {
		return 0, "", "", false, nil
	}
	f, err := o.ValueQuantity.Value.Float64()
	if err != nil {
		return 0, "", "", false, safeReadError(err)
	}
	if o.EffectiveDateTime != nil {
		date = *o.EffectiveDateTime
	}
	return int(f), date, r, true, nil
}

func (s *SoR) obsBool(ctx context.Context, patientID, system, code string) (val bool, ref string, ok bool, readErr error) {
	o, r, found, err := s.obsByCode(ctx, patientID, system, code)
	if err != nil {
		return false, "", false, safeReadError(err)
	}
	if !found || o.ValueBoolean == nil {
		return false, "", false, nil
	}
	return *o.ValueBoolean, r, true, nil
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
func (s *SoR) SupplementalReportContext(ctx context.Context, memberID string) ([]byte, bool, error) {
	_, pid, ok, err := s.resolvePatient(ctx, memberID)
	if err != nil {
		return nil, false, safeReadError(err)
	}
	if !ok {
		return nil, false, nil
	}
	tokens := make([]string, len(shnsdk.ReportValueSet))
	for i, c := range shnsdk.ReportValueSet {
		tokens[i] = shnsdk.SystemLOINC + "|" + c
	}
	raw, found, err := s.firstResourceBytes(ctx, "DiagnosticReport", url.Values{
		"patient": {pid}, "code": {strings.Join(tokens, ",")},
	})
	if err != nil {
		return nil, false, safeReadError(err)
	}
	if !found {
		return nil, false, nil
	}
	return rewriteSubject(raw, "Patient/"+memberID), true, nil
}

// FacilityRecords returns the external facility's records for the member, keyed by FHIR resource
// type (FR-24, UC-05). Searched by TYPE (no code filter) — production-faithful; served by the
// facility holder's fhirsor over its own base. Unknown member / no records => false.
//
// Each resource's subject.reference is rewritten to "Patient/<memberID>" (same rationale as
// SupplementalReport: HAPI-scoped IDs differ from the canonical member ID).
func (s *SoR) FacilityRecordsContext(ctx context.Context, memberID string) (map[string][]byte, bool, error) {
	_, pid, ok, err := s.resolvePatient(ctx, memberID)
	if err != nil {
		return nil, false, safeReadError(err)
	}
	if !ok {
		return nil, false, nil
	}
	out := map[string][]byte{}
	for _, rtype := range []string{"DiagnosticReport", "DocumentReference"} {
		raw, found, err := s.firstResourceBytes(ctx, rtype, url.Values{"patient": {pid}})
		if err != nil {
			return nil, false, safeReadError(err)
		}
		if found {
			out[rtype] = rewriteSubject(raw, "Patient/"+memberID)
		}
	}
	if len(out) == 0 {
		return nil, false, nil
	}
	return out, true, nil
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
func (s *SoR) OpenOrderContext(ctx context.Context, memberID string) ([]byte, bool, error) {
	_, pid, ok, err := s.resolvePatient(ctx, memberID)
	if err != nil {
		return nil, false, safeReadError(err)
	}
	if !ok {
		return nil, false, nil
	}
	for _, rtype := range []string{"DeviceRequest", "ServiceRequest"} {
		raw, found, err := s.firstResourceBytes(ctx, rtype, url.Values{
			"patient": {pid}, "status": {"active"},
		})
		if err != nil {
			return nil, false, safeReadError(err)
		}
		if found {
			return rewriteSubject(raw, "Patient/"+memberID), true, nil
		}
	}
	return nil, false, nil
}

// OpenCoverage returns the member's Coverage record bytes (FR-G40 routing + payload source):
// the same beneficiary-scoped Coverage search as CoverageInforce, but returning the raw
// resource bytes rather than the in-force determination. found=false when the patient cannot
// be resolved or no Coverage is on file.
func (s *SoR) OpenCoverageContext(ctx context.Context, memberID string) ([]byte, bool, error) {
	_, pid, ok, err := s.resolvePatient(ctx, memberID)
	if err != nil {
		return nil, false, safeReadError(err)
	}
	if !ok {
		return nil, false, nil
	}
	b, err := s.fc.Search(ctx, "Coverage", url.Values{
		"beneficiary": {"Patient/" + pid},
	})
	if err != nil {
		return nil, false, safeReadError(err)
	}
	if b == nil || len(b.Entry) == 0 {
		return nil, false, nil
	}
	return b.Entry[0].Resource, true, nil
}

// ResolveByReference returns the raw bytes of a resource named by a relative reference
// (e.g. "Organization/dme-1") via a direct FHIR read (GET {type}/{id}).
// A direct 404 is absence; transport and malformed-response errors remain errors.
// Used to resolve an order's performer (the DME supplier Organization) for headless
// order-dispatch origination.
func (s *SoR) ResolveByReferenceContext(ctx context.Context, ref string) ([]byte, bool, error) {
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, false, nil
	}
	body, found, err := s.fc.Read(ctx, parts[0], parts[1])
	if err != nil {
		return nil, false, safeReadError(err)
	}
	return body, found, nil
}

// firstResourceBytes returns the raw bytes of the first entry of a patient-scoped search, or
// ok=false for a true empty result; backend failures return an error.
func (s *SoR) firstResourceBytes(ctx context.Context, resourceType string, q url.Values) ([]byte, bool, error) {
	b, err := s.fc.Search(ctx, resourceType, q)
	if err != nil {
		return nil, false, safeReadError(err)
	}
	if b == nil || len(b.Entry) == 0 {
		return nil, false, nil
	}
	return []byte(b.Entry[0].Resource), true, nil
}

var _ engine.ContextSystemOfRecord = (*SoR)(nil)

func (s *SoR) ResolvePatient(memberID string) (string, engine.Demo, bool) {
	v0, v1, v2, _ := s.ResolvePatientContext(context.Background(), memberID)
	return v0, v1, v2
}

func (s *SoR) PatientFHIRRef(memberID string) (string, bool) {
	v0, v1, _ := s.PatientFHIRRefContext(context.Background(), memberID)
	return v0, v1
}

func (s *SoR) CoverageInforce(memberID string) (bool, string) {
	v0, v1, _ := s.CoverageInforceContext(context.Background(), memberID)
	return v0, v1
}

func (s *SoR) ClinicalContext(memberID string) (shnsdk.ClinicalContext, bool) {
	v0, v1, _ := s.ClinicalContextContext(context.Background(), memberID)
	return v0, v1
}

func (s *SoR) SupplementalReport(memberID string) ([]byte, bool) {
	v0, v1, _ := s.SupplementalReportContext(context.Background(), memberID)
	return v0, v1
}

func (s *SoR) FacilityRecords(memberID string) (map[string][]byte, bool) {
	v0, v1, _ := s.FacilityRecordsContext(context.Background(), memberID)
	return v0, v1
}

func (s *SoR) OpenOrder(memberID string) ([]byte, bool) {
	v0, v1, _ := s.OpenOrderContext(context.Background(), memberID)
	return v0, v1
}

func (s *SoR) OpenCoverage(memberID string) ([]byte, bool) {
	v0, v1, _ := s.OpenCoverageContext(context.Background(), memberID)
	return v0, v1
}

func (s *SoR) ResolveByReference(ref string) ([]byte, bool) {
	v0, v1, _ := s.ResolveByReferenceContext(context.Background(), ref)
	return v0, v1
}
