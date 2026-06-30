// pas_native.go — the CONFORMANT PAS leg (pas-claim): a relaxed full-bundle subject-bind +
// the payer-side inbound handler for a conformant Da Vinci $submit Claim Bundle (Claim +
// Patient + Coverage + payor Org + Practitioner + ServiceRequest [+ QuestionnaireResponse], in any
// order). This is the only PA $submit contract — the minimized pas-claim leg + the strict
// shnsdk.ParseClaimBundle parse it used are no longer part of the contract. The PAS analog of
// crd_native.go.
package engine

import (
	"encoding/json"
	"net/http"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// conformantPASSubjects holds what the conformant PAS bind extracted: the bound member and the QR/
// DiagnosticReport facts the sandbox adjudicator needs (the live relay leaves adjudication to the RI).
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
	if err := json.Unmarshal(bundleJSON, &probe); err != nil {
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
		var rt struct {
			ResourceType string `json:"resourceType"`
		}
		if err := json.Unmarshal(e.Resource, &rt); err != nil {
			return conformantPASSubjects{}, http.StatusBadRequest, "parse bundle entry failed"
		}
		ref := func(field string) string {
			var r map[string]json.RawMessage
			_ = json.Unmarshal(e.Resource, &r)
			var v struct {
				Reference string `json:"reference"`
			}
			_ = json.Unmarshal(r[field], &v)
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
	// Patient/MBR" not "Patient/MBR"); the sandbox keeps relative refs. pasMemberFromRef
	// reads the bare id from either form, so SHN's member bind works regardless of base
	// while the patient-consistency fence below still compares the SAME member identity.
	member := pasMemberFromRef(claimPat)
	if pasMemberFromRef(srSubject) != member ||
		pasMemberFromRef(covBene) != member {
		return conformantPASSubjects{}, http.StatusForbidden, "inconsistent patient in PAS bundle"
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
func (g *Gateway) ingressPASNativeSubjectPCI(bundleJSON []byte) (string, int, string) {
	s, status, msg := parseConformantPASSubjects(bundleJSON)
	if status != 0 {
		return "", status, msg
	}
	pci, _, found := g.cfg.SoR.ResolvePatient(s.member)
	if !found {
		return "", http.StatusBadRequest, "unknown member"
	}
	return pci, 0, ""
}

// handlePASNativeInbound serves the conformant PAS leg payer-side: decrypt, subject-bind the
// conformant request to the token (authority), forward the bundle to the responder (native relays the
// bundle byte-verbatim to the real RI's /Claim/$submit AND projects the submit-cell Store
// side-effects; sandbox adjudicates in-process AND records the same side-effects),
// and relay the response. The response member-fence (R-7) and response egress-$validate (R-8) are
// namespace-aware: on by default, standing down only for a foreign/relayed result; explained below.
//
// Store side-effects: BOTH responder paths return Commit/SideEffectFHIR — RecordPendedClaim on pend
// (the load-bearing pend→update handoff depends on it) and the FR-28/FR-34 EOB on approve/deny
// (both the native and sandbox paths). "Pure relay" is now a WIRE property only — the EOB
// is an ORTHOGONAL Store side-effect, so the native forward is no longer side-effect-free. This
// handler mirrors handlePASInbound's build-response-BEFORE-Commit ordering (review-fixes-6 #1): build
// the response leg, egress-$validate the SHN-PRODUCED EOB side-effects, run the Commit, THEN write —
// so a response-leg failure can never orphan the pended/EOB ledger. (A best-effort-CPT native submit
// with no AMA CPT returns Commit==nil/no side-effects — soft EOB — and the Commit-nil/empty
// branches simply relay.)
//
// Response member-fence (R-7) + response egress-$validate (R-8) are now NAMESPACE-AWARE:
// both run BY DEFAULT (the sandbox responder answers in SHN's OWN member namespace and
// produces SHN-shaped output — both flags false) and stand DOWN only when the responder declares the
// result foreign/relayed via markForeignRelay (native-forward path). The bound REQUEST above + the
// substrate's correlation-binding (the response reaches only this exchange's originator) remain the
// backstop on the relay path. OWD-G6 / FR-36.
//
// R-7 (fence iff !ResponseSubjectForeign): a real RI responds in its OWN patient namespace (br-payer
// returns Patient/SubscriberExample, not the request's member), so a ClaimResponse.patient ==
// bound-member check is a category error for a verbatim relay and would 403 every valid response —
// hence the member-match stands down there; the sandbox response IS member-fenced strict.
// (handleCRDNativeInbound has no response fence at all — but the reason differs: CRD cards carry no
// patient ref, whereas a ClaimResponse does.)
//
// R-8 (egress-$validate iff !ResponseRelayed): a verbatim foreign RI's Da Vinci PAS output declares
// Da Vinci PAS profiles SHN's US-Core-only validator can't resolve ("Failed to retrieve profile"),
// so $validating it would 422 a valid response — the relay stands down (br-payer validates its own
// output). The sandbox response IS egress-$validated.
//
// The SHN-PRODUCED EOB side-effect (sandbox BuildPADecisionEOB) is, by contrast, fenced + egress-
// $validated UNCONDITIONALLY — always built from the bound member, always an SHN resource (FR-36). A
// verbatim relay produces no EOB side-effect, so that loop is sandbox-only. This mirrors the DTR
// near-relay, CRD-native, and the minimized pas-claim case.
func (g *Gateway) handlePASNativeInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token) {
	bundleJSON, err := shnsdk.Open(env, g.cfg.Identity.EncPub, g.cfg.Identity.EncPriv)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decryption failed"})
		return
	}
	boundPatientRef, status, msg := g.conformantPASBind(bundleJSON, tok.Subject)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	result, err := g.cfg.Responder.Handle(r.Context(), "pas-claim", env.Metadata.CorrelationID, tok.Subject, bundleJSON)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "responder failed"})
		return
	}
	// Rollback on any pre-commit early return (the seam carries it for the update leg; submit
	// acquires no claim, so Rollback is nil today). Mirrors handlePASInbound.
	committed := false
	defer func() {
		if !committed && result.Rollback != nil {
			result.Rollback()
		}
	}()
	if result.Status != 0 {
		writeJSON(w, result.Status, map[string]string{"error": result.Message})
		return
	}
	// (C) outbound fence — two-predicate, namespace-aware: member-fence
	// the ClaimResponse iff !ResponseSubjectForeign (R-7 — a real br-payer answers in its OWN namespace,
	// so the member-match stands down for a verbatim relay; the sandbox responder answers in SHN's
	// member namespace, both flags false, so it fences strict). The SHN-produced EOB side-effect is
	// fenced UNCONDITIONALLY (always built from the bound member). Re-adds the (C) fence the minimized
	// pas-claim leg carries, before that leg is deleted (OWD-G6 prove-first).
	if status, msg := g.fenceResponseSubject("pas-claim", boundPatientRef, result); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	// Egress-$validate the RESPONSE iff !ResponseRelayed (R-8: a verbatim foreign relay carries Da
	// Vinci PAS profiles SHN's US-Core-only validator can't resolve, so a valid relay would 422). The
	// sandbox responder produces SHN-shaped output (ResponseRelayed false) → $validated, matching the
	// minimized pas-claim handler's response egress-$validate. The SHN-produced EOB side-effects are
	// $validated unconditionally in the loop below.
	if !result.ResponseRelayed {
		if status, msg := g.validateFHIR(r.Context(), result.ResponseFHIR, "egress"); status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
	}
	// Egress-$validate the SHN-PRODUCED EOB side-effects before the Store write (FR-36). The relay
	// RESPONSE itself is NOT $validated (R-8 — it may be a foreign RI's Da Vinci payload); a verbatim
	// relay carries no side-effect, so this loop is sandbox-only (matches the minimized pas-claim case).
	for _, b := range result.SideEffectFHIR {
		if status, msg := g.validateFHIR(r.Context(), b, "egress"); status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
	}
	// Build the response leg BEFORE committing payer state (review-fixes-6 #1) so a response-leg
	// failure (unknown requester, seal, encode) cannot orphan the EOB / pended-claim ledger.
	respBytes, status, msg := g.buildResponseLeg(r, "payer-coverage", "pas-response", "pas-claim", env.Metadata.CorrelationID, result.ResponseFHIR, tok.Subject, env.Metadata.Sender, "")
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	if result.Commit != nil {
		if err := result.Commit(); err != nil {
			// Store-write failure → 502 (parity with the minimized pas-claim RecordEOB/RecordPended 502).
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "holder write failed"})
			return
		}
	}
	committed = true
	writeLeg(w, respBytes)
}

// handlePASUpdateNativeInbound serves the CONFORMANT amended-re-POST leg (pas-claim-update)
// payer-side: decrypt, subject-bind + FR-32-gate the conformant update request to the token
// (conformantPASUpdateBind — authority + the supplemental-data Provenance attribution), forward the
// bundle to the responder, and relay the response. It is the UPDATE-family mirror of
// handlePASNativeInbound (the conformant SUBMIT leg): same Rollback-on-any-pre-commit-early-return +
// build-response-BEFORE-Commit ordering (review-fixes-6 #1) — the update responder DOES carry a
// Rollback (it acquires a claim via BeginClaimUpdate, like the minimized pas-claim-update leg).
//
// F-PB-R8 — NO ingress-$validate of the REQUEST bundle: the conformant Da Vinci amended re-POST is
// foreign-shaped (declares Da Vinci PAS profiles SHN's US-Core-only validator cannot resolve), so
// SHN preserves bytes + enforces authority/FR-32 here, exactly as handlePASNativeInbound and the DTR
// near-relay do. The egress-$validate loop below covers only SHN-PRODUCED SideEffectFHIR; the update
// leg builds NO EOB (only submit does), so that egress set is empty — the loop is a structural no-op
// carried for symmetry with the submit handler, NOT a place to "restore parity" with an ingress-$validate.
//
// Response member-fence (R-7) + response egress-$validate (R-8) are NAMESPACE-AWARE,
// mirror of the conformant submit leg: both run BY DEFAULT (sandbox responder, both flags
// false) and stand DOWN only when the result is declared foreign/relayed via markForeignRelay (a real
// RI responds in its OWN patient namespace, so a response-subject == bound-member check / $validating
// its Da Vinci profiles is a category error for a verbatim relay). The update leg builds NO EOB, so
// the SHN-produced-side-effect fence/$validate is a no-op here (F-PB-R8 above) — the flags exist to
// keep this leg symmetric with submit so the native relay stands the response fence/$validate down.
// (The minimized pas-claim-update leg's (C) fence stays LIVE on its own leg until that leg is deleted.)
func (g *Gateway) handlePASUpdateNativeInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token) {
	bundleJSON, err := shnsdk.Open(env, g.cfg.Identity.EncPub, g.cfg.Identity.EncPriv)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decryption failed"})
		return
	}
	boundPatientRef, status, msg := g.conformantPASUpdateBind(bundleJSON, tok.Subject)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	result, err := g.cfg.Responder.Handle(r.Context(), "pas-claim-update", env.Metadata.CorrelationID, tok.Subject, bundleJSON)
	// Arm defer-rollback-unless-committed on the returned result BEFORE checking err: the update
	// responder acquires a claim in BeginClaimUpdate and returns LegResult{Rollback: release} even
	// alongside a build error, so the claim is still released by this defer. Mirrors handlePASInbound
	// (payer.go) and handlePASNativeInbound.
	committed := false
	defer func() {
		if !committed && result.Rollback != nil {
			result.Rollback()
		}
	}()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "responder failed"})
		return
	}
	if result.Status != 0 {
		writeJSON(w, result.Status, map[string]string{"error": result.Message})
		return
	}
	// (C) outbound fence — two-predicate, namespace-aware: member-fence
	// the ClaimResponse iff !ResponseSubjectForeign (R-7), mirror of the conformant submit handler.
	// The update leg builds no EOB, so the SHN-produced-side-effect fence is a no-op here; the flag
	// keeps the leg symmetric with submit so the native relay (both flags set) stands the member-fence
	// down. Re-adds the (C) fence before the minimized pas-claim-update leg is deleted (OWD-G6).
	if status, msg := g.fenceResponseSubject("pas-claim-update", boundPatientRef, result); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	// Egress-$validate the RESPONSE iff !ResponseRelayed (R-8), mirror of the conformant submit
	// handler: the sandbox responder produces SHN-shaped output (validated); a foreign verbatim relay
	// (ResponseRelayed true) is preserved bytes-only (US-Core validator can't resolve its Da Vinci
	// profiles).
	if !result.ResponseRelayed {
		if status, msg := g.validateFHIR(r.Context(), result.ResponseFHIR, "egress"); status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
	}
	// Egress-$validate the SHN-PRODUCED side-effects before the Store write (FR-36). The update leg
	// builds NO EOB, so SideEffectFHIR is empty and this loop is a structural no-op (F-PB-R8); the
	// relay RESPONSE itself is NOT $validated (it may be a foreign RI's Da Vinci payload, R-8).
	for _, b := range result.SideEffectFHIR {
		if status, msg := g.validateFHIR(r.Context(), b, "egress"); status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
	}
	// Build the response leg BEFORE committing payer state (review-fixes-6 #1) so a response-leg
	// failure cannot orphan the claim acquired in BeginClaimUpdate (the deferred Rollback releases it).
	respBytes, status, msg := g.buildResponseLeg(r, "payer-coverage", "pas-update-response", "pas-claim-update", env.Metadata.CorrelationID, result.ResponseFHIR, tok.Subject, env.Metadata.Sender, "")
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	if result.Commit != nil {
		if err := result.Commit(); err != nil {
			// FinalizeClaimUpdate store-write failure → 502 (parity with the minimized leg).
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "holder write failed (finalize update)"})
			return
		}
	}
	committed = true
	writeLeg(w, respBytes)
}

// conformantPASBind is the payer-side authority check: the conformant request must subject-bind
// (parseConformantPASSubjects) AND its member must resolve to the inbound token's PCI. Returns
// the bound member ref ("Patient/<member>") as the first value on accept (the (C) fence's
// boundPatientRef — a namespace-aware response member-fence applies on this
// leg, fenceResponseSubject("pas-claim", …)); "" on every reject. Status 0 = accept.
func (g *Gateway) conformantPASBind(bundleJSON []byte, tokSubject string) (memberRef string, status int, msg string) {
	s, status, msg := parseConformantPASSubjects(bundleJSON)
	if status != 0 {
		return "", status, msg
	}
	pci, _, found := g.cfg.SoR.ResolvePatient(s.member)
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
	provenanceAgents   []string // Provenance.agent[].who.reference
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
func parseConformantPASUpdateFacts(bundleJSON []byte) (conformantUpdateFacts, int, string) {
	var probe struct {
		ResourceType string `json:"resourceType"`
		Entry        []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(bundleJSON, &probe); err != nil {
		return conformantUpdateFacts{}, http.StatusBadRequest, "parse claim update bundle failed"
	}
	if probe.ResourceType != "Bundle" {
		return conformantUpdateFacts{}, http.StatusBadRequest, "PAS update request is not a Bundle"
	}
	var f conformantUpdateFacts
	for _, e := range probe.Entry {
		var rt struct {
			ResourceType string `json:"resourceType"`
		}
		if err := json.Unmarshal(e.Resource, &rt); err != nil {
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
			if err := json.Unmarshal(e.Resource, &c); err != nil {
				return conformantUpdateFacts{}, http.StatusBadRequest, "parse update Claim entry failed"
			}
			if len(c.Related) > 0 {
				f.relatedClaim = c.Related[0].Claim.Identifier.Value
			}
			// Finding A: surface the Claim's own urn:shn:correlation so handlePASIngress can key
			// the pend on the partner-supplied identifier, enabling the submit→amend corr handoff.
			for _, id := range c.Identifier {
				if id.System == "urn:shn:correlation" && id.Value != "" {
					f.claimCorrelation = id.Value
					break
				}
			}
		case "QuestionnaireResponse":
			var qr struct {
				Id string `json:"id"`
			}
			if err := json.Unmarshal(e.Resource, &qr); err != nil {
				return conformantUpdateFacts{}, http.StatusBadRequest, "parse update QR entry failed"
			}
			f.qrID = qr.Id
		case "DiagnosticReport":
			var dr struct {
				Id string `json:"id"`
			}
			if err := json.Unmarshal(e.Resource, &dr); err != nil {
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
						Reference string `json:"reference"`
					} `json:"who"`
				} `json:"agent"`
				Policy []string `json:"policy"`
			}
			if err := json.Unmarshal(e.Resource, &prov); err != nil {
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
func (g *Gateway) conformantPASUpdateBind(bundleJSON []byte, tokSubject string) (memberRef string, status int, msg string) {
	memberRef, status, msg = g.conformantPASBind(bundleJSON, tokSubject)
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
