// pas_native.go — the CONFORMANT PAS leg (pas-claim): a relaxed full-bundle subject-bind +
// the payer-side inbound handler for a conformant Da Vinci $submit Claim Bundle (Claim +
// Patient + Coverage + payor Org + Practitioner + ServiceRequest [+ QuestionnaireResponse], in any
// order). This is the only PA $submit contract — the minimized pas-claim leg + the strict
// shnsdk.ParseClaimBundle parse it used are no longer part of the contract. The PAS analog of
// crd_native.go.
package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// conformantPASSubjects holds what the conformant PAS bind extracted: the bound member and the QR/
// DiagnosticReport facts an in-process adjudicator needs (the live relay leaves adjudication to the RI).
type conformantPASSubjects struct {
	member string // the bound member id (Claim.patient, sans "Patient/")
	qrJSON []byte // the QuestionnaireResponse resource, or nil (R-5: optional on this leg)
	srJSON []byte // the ServiceRequest resource (REQUIRED, R-4) — the EOB's CPT source
	hasDR  bool   // a DiagnosticReport entry is present (FR-20 pended branch)
}

// parseConformantPASSubjects does ONE pass over a conformant PAS Claim Bundle, indexing entries by
// resourceType. Unlike the deleted strict shnsdk.ParseClaimBundle (which rejected any entry outside
// Claim/QR/SR/DR/Provenance), it TOLERATES the full conformant entry set (Patient, Coverage, payor
// Organization, Practitioner, PractitionerRole) while binding every patient reference to ONE member:
// Claim.patient + ServiceRequest.subject + Coverage.beneficiary (REQUIRED, R-4) — and
// QuestionnaireResponse.subject + DiagnosticReport.subject WHEN PRESENT (R-5: a real br-payer
// $submit may carry no QR). Engine-local (no SDK symbol).
func parseConformantPASSubjects(bundleJSON []byte) (conformantPASSubjects, int, string) {
	var probe struct {
		ResourceType string `json:"resourceType"`
		Entry        []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := decodeMessage(bundleJSON, &probe); err != nil {
		return conformantPASSubjects{}, http.StatusBadRequest, "parse claim bundle failed"
	}
	if probe.ResourceType != "Bundle" {
		return conformantPASSubjects{}, http.StatusBadRequest, "PAS request is not a Bundle"
	}
	var (
		s                          conformantPASSubjects
		claimPat                   string
		srSubject                  string
		qrSubject                  string
		covBene                    string
		drSubject                  string
		haveClaim, haveSR, haveCov bool
	)
	for _, e := range probe.Entry {
		// The patient-bearing members are read by their exact names: a member
		// that matches one only when case is ignored is refused, because the
		// recipient would not read it.
		var rt struct {
			ResourceType string          `json:"resourceType"`
			Patient      json.RawMessage `json:"patient"`
			Subject      json.RawMessage `json:"subject"`
			Beneficiary  json.RawMessage `json:"beneficiary"`
		}
		if err := decodeMessage(e.Resource, &rt); err != nil {
			return conformantPASSubjects{}, http.StatusBadRequest, "parse bundle entry failed"
		}
		ref := func(field string) string {
			raw := map[string]json.RawMessage{"patient": rt.Patient, "subject": rt.Subject, "beneficiary": rt.Beneficiary}[field]
			var v struct {
				Reference string `json:"reference"`
			}
			_ = decodeMessage(raw, &v)
			return v.Reference
		}
		switch rt.ResourceType {
		case "Claim":
			// Bind the member from the OPERATIVE Claim — the FIRST Claim entry, exactly as br-payer
			// selects it (PasBundleValidator.validateCommon → getEntryFirstRep). A br-payer-targeting
			// PAS Claim Update also carries the PRIOR Claim as a non-first linkage entry (a minimal
			// {resourceType,id,identifier} with no patient — sdk buildPriorClaimEntry); it must NOT
			// clobber the operative Claim.patient. First-Claim-wins.
			if !haveClaim {
				claimPat, haveClaim = ref("patient"), true
			}
		case "ServiceRequest", "DeviceRequest":
			// The ORDER entry — a procedure ServiceRequest OR a DME DeviceRequest (HomeOxygen
			// provider-data lane; the SDK PAS builder carries the order as convergence-sr or
			// convergence-dr per resourceType). Both bind the order subject the same way; haveSR
			// gates "an order is present" regardless of order resource type.
			srSubject, haveSR, s.srJSON = ref("subject"), true, e.Resource
		case "Coverage":
			covBene, haveCov = ref("beneficiary"), true
		case "QuestionnaireResponse":
			qrSubject, s.qrJSON = ref("subject"), e.Resource
		case "DiagnosticReport":
			drSubject, s.hasDR = ref("subject"), true
		default:
			// Patient / Organization / Practitioner / PractitionerRole / Provenance — tolerated.
		}
	}
	if !haveClaim || claimPat == "" {
		return conformantPASSubjects{}, http.StatusBadRequest, "PAS bundle missing Claim.patient"
	}
	if !haveSR {
		return conformantPASSubjects{}, http.StatusBadRequest, "PAS bundle missing order (ServiceRequest or DeviceRequest)"
	}
	if !haveCov || covBene == "" {
		return conformantPASSubjects{}, http.StatusBadRequest, "PAS bundle missing Coverage.beneficiary"
	}
	// Extract the member id tolerantly: the br-payer-targeting lane (provider-data) ABSOLUTIZES
	// bundle refs so a real Da Vinci payer (br-payer) resolves them ("https://shn.example/fhir/
	// Patient/MBR" not "Patient/MBR"); the mirrored demo lane keeps relative refs. pasMemberFromRef
	// reads the bare id from either form, so SHN's member bind works regardless of base
	// while the patient-consistency fence below still compares the SAME member identity.
	member := pasMemberFromRef(claimPat)
	if pasMemberFromRef(srSubject) != member ||
		pasMemberFromRef(covBene) != member {
		return conformantPASSubjects{}, http.StatusForbidden, "inconsistent patient in PAS bundle"
	}
	// A QR with no subject could carry answers adjudicated for a different
	// patient, so when a QR is present its subject is REQUIRED. Identical to the
	// published SDK Responder's fence (sdk bindConformantClaimSubject), which
	// enforced this first — this twin had been left behind (twin-fence corpus:
	// upd-qr-missing-subject).
	if s.qrJSON != nil && qrSubject == "" {
		return conformantPASSubjects{}, http.StatusForbidden, "PAS bundle QuestionnaireResponse missing subject"
	}
	if qrSubject != "" && pasMemberFromRef(qrSubject) != member {
		return conformantPASSubjects{}, http.StatusForbidden, "inconsistent patient in PAS bundle"
	}
	if s.hasDR && pasMemberFromRef(drSubject) != member {
		return conformantPASSubjects{}, http.StatusForbidden, "inconsistent patient in PAS bundle"
	}
	s.member = member
	return s, 0, ""
}

// pasBundleCoverage returns the FIRST Coverage resource in a conformant PAS Claim Bundle, or nil
// when none is present (routing then fails closed at recipientFor → 422). Engine-local; the PAS
// ingress derives the payer HOLDER from the inbound bundle's Coverage — no default (FR-G40).
func pasBundleCoverage(bundleJSON []byte) []byte {
	var probe struct {
		Entry []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := decodeMessage(bundleJSON, &probe); err != nil {
		return nil
	}
	for _, e := range probe.Entry {
		var rt struct {
			ResourceType string `json:"resourceType"`
		}
		if decodeMessage(e.Resource, &rt) == nil && rt.ResourceType == "Coverage" {
			return e.Resource
		}
	}
	return nil
}

// pasMemberFromRef returns the bare member id from a Patient reference, tolerating BOTH a
// relative ref ("Patient/MBR") and an absolute fullUrl ("https://host/base/Patient/MBR").
// The br-payer-targeting lane (provider-data) absolutizes bundle refs (so br-payer resolves them);
// SHN's member bind must read the id regardless of base. Identical to the old strings.TrimPrefix(...,"Patient/")
// for the relative case (LastIndex hits index 0). A ref with no "Patient/" segment is returned
// unchanged, so the downstream ResolvePatient fails closed (unknown member).
func pasMemberFromRef(ref string) string {
	if i := strings.LastIndex(ref, "Patient/"); i >= 0 {
		return ref[i+len("Patient/"):]
	}
	return ref
}

// ingressPASNativeSubjectPCI resolves the bound member of a conformant PAS bundle to a pci
// (origination side). Mirrors ingressCRDSubjectPCI.
func (g *Gateway) ingressPASNativeSubjectPCIContext(ctx context.Context, bundleJSON []byte) (string, int, string) {
	s, status, msg := parseConformantPASSubjects(bundleJSON)
	if status != 0 {
		return "", status, msg
	}
	pci, found, readErr := g.resolveSubjectPCI(ctx, s.member, bundleJSON)
	if readErr != nil {
		status, msg := SoRFailureResponse(readErr)
		return "", status, msg
	}
	if !found {
		return "", http.StatusBadRequest, "unknown member"
	}
	return pci, 0, ""
}

// handlePASNativeInbound delivers a verified native submission through the shared boundary.
func (g *Gateway) handlePASNativeInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, bundleJSON []byte, answerTok string) {
	g.handleNativeInbound(w, r, "pas-claim", env, tok, bundleJSON, answerTok)
}

// handlePASUpdateNativeInbound delivers a verified amendment without local pend admission.
func (g *Gateway) handlePASUpdateNativeInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, bundleJSON []byte, answerTok string) {
	g.handleNativeInbound(w, r, "pas-claim-update", env, tok, bundleJSON, answerTok)
}

// conformantPASBind is the payer-side authority check: the conformant request must subject-bind
// (parseConformantPASSubjects) AND its member must resolve to the inbound token's PCI. Returns
// the bound member ref ("Patient/<member>") as the first value on accept (the (C) fence's
// boundPatientRef — a namespace-aware response member-fence applies on this
// leg, fenceResponseSubject("pas-claim", …)); "" on every reject. Status 0 = accept.
func (g *Gateway) conformantPASBindContext(ctx context.Context, bundleJSON []byte, tokSubject string) (memberRef string, status int, msg string) {
	s, status, msg := parseConformantPASSubjects(bundleJSON)
	if status != 0 {
		return "", status, msg
	}
	pci, found, readErr := g.resolveSubjectPCI(ctx, s.member, bundleJSON)
	if readErr != nil {
		status, msg := SoRFailureResponse(readErr)
		return "", status, msg
	}
	if !found {
		return "", http.StatusBadRequest, "unknown member"
	}
	if pci != tokSubject {
		return "", http.StatusForbidden, "token subject does not match request patient"
	}
	return "Patient/" + s.member, 0, ""
}

// conformantUpdateFacts is the CONFORMANT analog of shnsdk.ClaimBundle's FR-32 exposure
// (sdk/pasresponder.go) — the cross-resource facts parseConformantPASSubjects does NOT surface
// (it returns only {member, qrJSON, srJSON, hasDR}). These are exactly the fields the inbound
// update gate enforces against (the FR-32 arms mirror payer.go:393-424).
type conformantUpdateFacts struct {
	provenanceJSON     []byte   // the Provenance resource bytes, or nil (FR-32: REQUIRED on the update leg)
	provenanceAgents   []string // Provenance.agent[].who reference or qualified business identifier
	provenanceTargets  []string // Provenance.target[].reference
	provenancePolicies []string // Provenance.policy[] (UC-05 consent cite — surfaced, not yet enforced)
	hasDR              bool     // a DiagnosticReport entry is present (DR-variant supplemental data)
	diagnosticReportID string   // DiagnosticReport.id
	qrID               string   // QuestionnaireResponse.id (the QR-variant supplemental data)
	relatedClaim       string   // Claim.related[0].claim.identifier.value (the amendment's distinguishing field, FR-21)
	claimCorrelation   string   // Claim.identifier[].value where system=="urn:shn:correlation", or "" (Finding A: partner-supplied leg corr)
}

// parseConformantPASUpdateFacts does ONE pass over a conformant amended-re-POST Bundle, extracting the
// FR-32 cross-resource facts parseConformantPASSubjects does NOT surface, for the CONFORMANT shape: it
// tolerates the full conformant entry set (Patient/Coverage/Org/Practitioner are present and ignored,
// like parseConformantPASSubjects) and reads Claim.related[prior], the supplemental DiagnosticReport id,
// the amended QR id, and the Provenance agents/targets/policies. Returns (facts, 0, "") on a parseable
// Bundle; (_, 400, msg) on malformed. Engine-local (no SDK symbol).
//
// WHICH Claim entry the facts come from: a bundle can carry MORE THAN ONE Claim. On the reference-payer
// (PayerOrgEntry) lane the sdk appends the ORIGINAL SUBMIT's Claim as a resolvable bundle ENTRY
// after the operative update Claim (shnsdk buildPriorClaimEntry — so the update Claim's
// related[].claim.reference resolves for a real Da Vinci payer), and that prior entry's only
// identifier is urn:shn:correlation|<original submit corr>. So relatedClaim and claimCorrelation
// are read from THE OPERATIVE UPDATE CLAIM — the (first) Claim entry carrying related[] — and, when
// no Claim carries related[] (a plain initial submit), from the FIRST Claim entry. A later Claim
// entry never overwrites either: letting the prior-Claim entry win threaded the SUBMIT's
// correlation onto the AMEND's envelope in handlePASIngress, which the Hub's replay guard rejected
// as a duplicate — the partner saw 502 {"error":"hub routing failed"} on every amendment.
func parseConformantPASUpdateFacts(bundleJSON []byte) (conformantUpdateFacts, int, string) {
	var probe struct {
		ResourceType string `json:"resourceType"`
		Entry        []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := decodeMessage(bundleJSON, &probe); err != nil {
		return conformantUpdateFacts{}, http.StatusBadRequest, "parse claim update bundle failed"
	}
	if probe.ResourceType != "Bundle" {
		return conformantUpdateFacts{}, http.StatusBadRequest, "PAS update request is not a Bundle"
	}
	var f conformantUpdateFacts
	// Claim-entry selection state (see the doc comment): the first Claim entry's own correlation,
	// and the operative update Claim's — the first Claim entry carrying related[]. Resolved after
	// the pass so a LATER Claim entry (the sdk's prior-Claim entry) can never overwrite either.
	var (
		firstClaimCorr     string
		sawClaim           bool
		operativeClaimCorr string
		sawOperativeClaim  bool
	)
	for _, e := range probe.Entry {
		var rt struct {
			ResourceType string `json:"resourceType"`
		}
		if err := decodeMessage(e.Resource, &rt); err != nil {
			return conformantUpdateFacts{}, http.StatusBadRequest, "parse update bundle entry failed"
		}
		switch rt.ResourceType {
		case "Claim":
			var c struct {
				Identifier []struct {
					System string `json:"system"`
					Value  string `json:"value"`
				} `json:"identifier"`
				Related []struct {
					Claim struct {
						Identifier struct {
							Value string `json:"value"`
						} `json:"identifier"`
					} `json:"claim"`
				} `json:"related"`
			}
			if err := decodeMessage(e.Resource, &c); err != nil {
				return conformantUpdateFacts{}, http.StatusBadRequest, "parse update Claim entry failed"
			}
			// Finding A: surface the Claim's own urn:shn:correlation so handlePASIngress can key
			// the pend on the partner-supplied identifier, enabling the submit→amend corr handoff.
			corr := ""
			for _, id := range c.Identifier {
				if id.System == "urn:shn:correlation" && id.Value != "" {
					corr = id.Value
					break
				}
			}
			if !sawClaim {
				sawClaim, firstClaimCorr = true, corr
			}
			// The operative update Claim is the one carrying related[prior]; the sdk's prior-Claim
			// entry carries none, so it can never claim this slot.
			if len(c.Related) > 0 && !sawOperativeClaim {
				sawOperativeClaim, operativeClaimCorr = true, corr
				f.relatedClaim = c.Related[0].Claim.Identifier.Value
			}
		case "QuestionnaireResponse":
			var qr struct {
				Id string `json:"id"`
			}
			if err := decodeMessage(e.Resource, &qr); err != nil {
				return conformantUpdateFacts{}, http.StatusBadRequest, "parse update QR entry failed"
			}
			f.qrID = qr.Id
		case "DiagnosticReport":
			var dr struct {
				Id string `json:"id"`
			}
			if err := decodeMessage(e.Resource, &dr); err != nil {
				return conformantUpdateFacts{}, http.StatusBadRequest, "parse update DiagnosticReport entry failed"
			}
			f.hasDR = true
			f.diagnosticReportID = dr.Id
		case "Provenance":
			f.provenanceJSON = e.Resource
			var prov struct {
				Target []struct {
					Reference string `json:"reference"`
				} `json:"target"`
				Agent []struct {
					Who struct {
						Reference  string `json:"reference"`
						Identifier struct {
							System string `json:"system"`
							Value  string `json:"value"`
						} `json:"identifier"`
					} `json:"who"`
				} `json:"agent"`
				Policy []string `json:"policy"`
			}
			if err := decodeMessage(e.Resource, &prov); err != nil {
				return conformantUpdateFacts{}, http.StatusBadRequest, "parse update Provenance entry failed"
			}
			for _, tgt := range prov.Target {
				if tgt.Reference != "" {
					f.provenanceTargets = append(f.provenanceTargets, tgt.Reference)
				}
			}
			for _, a := range prov.Agent {
				if a.Who.Reference != "" {
					f.provenanceAgents = append(f.provenanceAgents, a.Who.Reference)
				} else if id := a.Who.Identifier; strings.TrimSpace(id.Value) != "" && (id.System == "http://hl7.org/fhir/sid/us-npi" || id.System == "http://smarthealth.network/ids/holder") {
					f.provenanceAgents = append(f.provenanceAgents, id.System+"|"+id.Value)
				}
			}
			for _, p := range prov.Policy {
				if p != "" {
					f.provenancePolicies = append(f.provenancePolicies, p)
				}
			}
		default:
			// Patient / Coverage / ServiceRequest / Organization / Practitioner / PractitionerRole —
			// tolerated (parseConformantPASSubjects already binds their subjects).
		}
	}
	if sawOperativeClaim {
		f.claimCorrelation = operativeClaimCorr
	} else {
		f.claimCorrelation = firstClaimCorr
	}
	return f, 0, ""
}

// conformantPASUpdateBind is the payer-side authority + FR-32 check for the CONFORMANT update leg
// (pas-claim-update). The request must subject-bind (conformantPASBind — three-way patient
// bind + token-PCI match), AND carry a Provenance with an agent that TARGETS the supplemental
// resource — the DiagnosticReport when present, else the amended QuestionnaireResponse. This mirrors
// the minimized leg's FR-32 enforcement (payer.go:393-424) for the conformant shape: a Provenance
// for an unrelated/wrong-id resource, or with no agent, does not attribute the evidence and is
// rejected. Returns the bound member ref ("Patient/<member>", from conformantPASBind) as the first
// value on accept (the (C) fence's boundPatientRef — a namespace-aware response
// member-fence applies on this leg too); "" on every reject. Status 0 = accept.
func (g *Gateway) conformantPASUpdateBindContext(ctx context.Context, bundleJSON []byte, tokSubject string) (memberRef string, status int, msg string) {
	memberRef, status, msg = g.conformantPASBindContext(ctx, bundleJSON, tokSubject)
	if status != 0 {
		return "", status, msg
	}
	f, status, msg := parseConformantPASUpdateFacts(bundleJSON)
	if status != 0 {
		return "", status, msg
	}
	if f.provenanceJSON == nil {
		return "", http.StatusForbidden, "ClaimUpdate missing Provenance"
	}
	if len(f.provenanceAgents) == 0 {
		return "", http.StatusForbidden, "ClaimUpdate Provenance missing agent"
	}
	var wantTarget string
	if f.hasDR {
		if f.diagnosticReportID == "" {
			return "", http.StatusForbidden, "supplemental DiagnosticReport missing id"
		}
		wantTarget = "DiagnosticReport/" + f.diagnosticReportID
	} else {
		if f.qrID == "" {
			return "", http.StatusForbidden, "supplemental QuestionnaireResponse missing id"
		}
		wantTarget = "QuestionnaireResponse/" + f.qrID
	}
	for _, ref := range f.provenanceTargets {
		// Tolerate the br-payer-targeting lane's ABSOLUTE refs: absolutizeBundleRefs rewrites
		// Provenance.target to its absolute fullUrl (".../DiagnosticReport/<id>") so a real
		// Da Vinci payer resolves it. Match either the relative wantTarget or any ref ending
		// in "/<wantTarget>" — same absolutization-tolerance as pasMemberFromRef.
		if ref == wantTarget || strings.HasSuffix(ref, "/"+wantTarget) {
			return memberRef, 0, ""
		}
	}
	return "", http.StatusForbidden, "ClaimUpdate Provenance does not target the supplemental data"
}

// stampForBuiltAnswer preserves an explicit producer declaration for a relayed
// answer, leaving it absent when the producer supplied none. An answer built by
// this gateway is stamped at its build line. All response legs share this rule.
func stampForBuiltAnswer(result LegResult, answerTok string) string {
	if result.ResponseRelayed() {
		return result.ResponseContractVersion
	}
	return answerTok
}

// validatePASResult applies the local policy to authored and relayed answers.
// A relayed answer's independent declaration selects its line; an absent
// declaration cannot borrow the request's line as certification evidence.
func (g *Gateway) validatePASResult(ctx context.Context, result LegResult, answerTok, leg string) (int, string) {
	responseFHIR, err := g.admit(result.Response, answerKey(leg, relay.OutcomeAnswered))
	if err != nil {
		return http.StatusInternalServerError, errOwnershipFault
	}
	if result.ResponseRelayed() {
		if g.policy().Action(CheckDeep) != CheckEnforce {
			return 0, ""
		}
		answerTok = result.ResponseContractVersion
		if !strings.HasPrefix(answerTok, "pa.pas@") {
			return http.StatusServiceUnavailable, "conformance_unavailable: response version unavailable"
		}
	}
	return g.validateFHIRForContract(ctx, responseFHIR, "egress", "pa.pas", shnsdk.LineOf(answerTok), "")
}
