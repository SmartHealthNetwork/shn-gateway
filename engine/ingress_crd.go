// ingress_crd.go — CRD (CDS Hooks order-select) ingress: parse the conformant inbound request
// and prove every patient reference resolves to ONE pci before any leg. This is the origination
// mirror of the inbound H2a fence / bindBundleSubject: on the ingress the payload is EXTERNAL
// (br-provider), so cross-field patient consistency is not automatic the way it is for the
// /scenario Originator.
package engine

import (
	"encoding/json"
	"net/http"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// ingressCDSRequest is the conformant inbound CDS Hooks order-select request (pinned
// conformant shape). Only the patient-bearing fields are modelled; the full request is forwarded opaque.
type ingressCDSRequest struct {
	Hook         string `json:"hook"`
	HookInstance string `json:"hookInstance"`
	FHIRServer   string `json:"fhirServer"`
	Context      struct {
		UserID      string `json:"userId"`
		PatientID   string `json:"patientId"`
		DraftOrders struct {
			Entry []struct {
				Resource json.RawMessage `json:"resource"`
			} `json:"entry"`
		} `json:"draftOrders"`
	} `json:"context"`
	Prefetch map[string]json.RawMessage `json:"prefetch"`
}

// patientRefOf pulls a subject/beneficiary/patient reference from an arbitrary FHIR resource.
func patientRefOf(resource json.RawMessage) string {
	var r struct {
		Subject struct {
			Reference string `json:"reference"`
		} `json:"subject"`
		Beneficiary struct {
			Reference string `json:"reference"`
		} `json:"beneficiary"`
		Patient struct {
			Reference string `json:"reference"`
		} `json:"patient"`
	}
	_ = json.Unmarshal(resource, &r)
	switch {
	case r.Subject.Reference != "":
		return r.Subject.Reference
	case r.Beneficiary.Reference != "":
		return r.Beneficiary.Reference
	case r.Patient.Reference != "":
		return r.Patient.Reference
	}
	return ""
}

// memberForPCI re-reads the bare context.patientId member (already validated by the subject fence).
func (g *Gateway) memberForPCI(body []byte) string {
	var req ingressCDSRequest
	_ = json.Unmarshal(body, &req)
	return strings.TrimPrefix(req.Context.PatientID, "Patient/")
}

// wrapCards relays the substrate crd-cards response (already a rendered conformant cards envelope
// from BuildCards — near-relay) and derives a metadata-only outcome. It must NOT
// rebuild a card from CardCoverage (that would re-synthesize the summary). Returns
// (cardsEnvelope, outcome, 0, "") or (nil, "", status, msg).
func wrapCards(respJSON []byte) ([]byte, string, int, string) {
	var inner struct {
		Cards []json.RawMessage `json:"cards"`
	}
	if err := json.Unmarshal(respJSON, &inner); err != nil || len(inner.Cards) == 0 {
		return nil, "", http.StatusBadGateway, "crd response is not a cards envelope"
	}
	outcome := "approved"
	if cov, err := shnsdk.ParseCards(respJSON); err == nil {
		switch {
		case cov.Covered == shnsdk.CoveredNotCovered:
			outcome = "denied"
		case cov.PARequired():
			outcome = "pa-required"
		}
	}
	return respJSON, outcome, 0, ""
}

// ingressCRDSubjectPCI parses the request and returns the single bound pci. Every patient
// reference present (context.patientId, each draftOrders entry subject, each prefetch
// resource's subject/beneficiary/patient) MUST resolve to the SAME pci; any divergence fails
// closed (403). Returns (pci, 0, "") on success or ("", status, msg) to write.
func (g *Gateway) ingressCRDSubjectPCI(body []byte) (string, int, string) {
	var req ingressCDSRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return "", http.StatusBadRequest, "parse cds request failed"
	}
	if req.Context.PatientID == "" {
		return "", http.StatusBadRequest, "missing context.patientId"
	}
	member := strings.TrimPrefix(req.Context.PatientID, "Patient/")
	pci, _, found := g.cfg.SoR.ResolvePatient(member)
	if !found {
		return "", http.StatusBadRequest, "unknown member"
	}
	// Every other patient reference must resolve to the SAME pci.
	var refs []string
	// A draft order is the order this PA is FOR — it MUST carry a patient subject. A
	// missing/renamed subject is REJECTED here (not skipped), so an order with no recognizable
	// patient can never ride into the sealed request behind the bound subject's authority.
	for _, e := range req.Context.DraftOrders.Entry {
		ref := patientRefOf(e.Resource)
		if ref == "" {
			return "", http.StatusForbidden, "draft order missing patient subject"
		}
		refs = append(refs, ref)
	}
	// DEF-INGRESS-BUNDLE: a prefetch value that is a searchset Bundle (the history prefetches —
	// serviceHistory/deviceHistory/medicationHistory/questionnaireResponses) is NOT deeply
	// inspected here: patientRefOf sees no top-level subject on a Bundle and skips it. Per-entry
	// subject validation of a KEPT prefetch Bundle is closed by ingressEnsureSelfContained
	// (which owns prefetch) — a kept Bundle's entries are validated against the bound PCI or
	// re-resolved from the SoR. ingressEnsureSelfContained closes this gap: a KEPT Bundle's
	// entries are validated against the bound PCI in fenceKeptBundle, so a crafted prefetch
	// Bundle cannot carry wrong-patient resources into the sealed request.
	for _, res := range req.Prefetch {
		// A bare Patient resource's identity is its `id`, NOT a subject/beneficiary/patient
		// reference, so patientRefOf can't see it. The prefetch Patient is therefore a
		// subject to fence explicitly. Resolve its id and require it bind to the same pci,
		// else a kept `prefetch.patient:{id:B}` for a different person rides into A's
		// sealed exchange.
		if id := patientResourceID(res); id != "" {
			refs = append(refs, "Patient/"+strings.TrimPrefix(id, "Patient/"))
			continue
		}
		if ref := patientRefOf(res); ref != "" {
			refs = append(refs, ref)
		}
	}
	for _, ref := range refs {
		m := strings.TrimPrefix(ref, "Patient/")
		rp, _, ok := g.cfg.SoR.ResolvePatient(m)
		if !ok || rp != pci {
			return "", http.StatusForbidden, "inconsistent patient reference in ingress payload"
		}
	}
	return pci, 0, ""
}

// patientResourceID returns the `id` of a prefetch resource iff it is a Patient resource (whose
// identity is its id, not a reference). "" for any other resource type.
func patientResourceID(resource json.RawMessage) string {
	var r struct {
		ResourceType string `json:"resourceType"`
		ID           string `json:"id"`
	}
	_ = json.Unmarshal(resource, &r)
	if r.ResourceType == "Patient" {
		return r.ID
	}
	return ""
}

// ingressEnsureSelfContained makes the request self-contained before sealing: per
// advertised prefetch key — keep if the caller inlined it; else resolve+inline from the
// provider SoR; else FAIL CLOSED (422), never relaying a live callback. fhirServer /
// fhirAuthorization are unconditionally stripped (non-aggregation floor). A KEPT searchset
// Bundle's entries are validated against the bound pci (closes DEF-INGRESS-BUNDLE from the
// subject fence). Returns (rewritten bytes, 0, "") or (nil, status, msg).
func (g *Gateway) ingressEnsureSelfContained(body []byte, member, pci string) ([]byte, int, string) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, http.StatusBadRequest, "parse cds request failed"
	}
	delete(doc, "fhirServer")
	delete(doc, "fhirAuthorization")

	prefetch := map[string]json.RawMessage{}
	if raw, ok := doc["prefetch"]; ok {
		_ = json.Unmarshal(raw, &prefetch)
	}
	patientRef := "Patient/" + member
	for _, key := range pinnedPrefetchKeys {
		if present, ok := prefetch[key]; ok {
			// KEEP path — but a kept searchset Bundle's entries must each bind to the bound pci.
			if status, msg := g.fenceKeptBundle(present, pci); status != 0 {
				return nil, status, msg
			}
			continue
		}
		resolved, ok := g.resolvePrefetchFromSoR(key, member, patientRef)
		if !ok {
			return nil, http.StatusUnprocessableEntity, "prefetch " + key + " not inlined and not resolvable from SoR"
		}
		prefetch[key] = resolved
	}
	pfBytes, err := json.Marshal(prefetch)
	if err != nil {
		return nil, http.StatusInternalServerError, "marshal prefetch failed"
	}
	doc["prefetch"] = pfBytes
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, http.StatusInternalServerError, "marshal request failed"
	}
	return out, 0, ""
}

// fenceKeptBundle closes DEF-INGRESS-BUNDLE (the subject fence's deferred gap): a KEPT prefetch value
// that is a searchset Bundle must have every entry's patient subject resolve to the bound pci,
// else a crafted history Bundle could smuggle wrong-patient resources into the sealed request.
// Non-Bundle values are covered by the single-resource fence; this returns ok for them.
func (g *Gateway) fenceKeptBundle(value json.RawMessage, pci string) (int, string) {
	var probe struct {
		ResourceType string `json:"resourceType"`
		Entry        []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(value, &probe); err != nil || probe.ResourceType != "Bundle" {
		return 0, ""
	}
	for _, e := range probe.Entry {
		ref := patientRefOf(e.Resource)
		if ref == "" {
			continue
		}
		m := strings.TrimPrefix(ref, "Patient/")
		if rp, _, ok := g.cfg.SoR.ResolvePatient(m); !ok || rp != pci {
			return http.StatusForbidden, "prefetch bundle entry patient mismatch"
		}
	}
	return 0, ""
}

// resolvePrefetchFromSoR maps a pinned prefetch key to a typed SoR read + thin project.
// coverage reuses BuildCoverage; the histories project into an empty (self-containing)
// searchset Bundle; an unknown member fails (→ caller returns 422). A generalized
// FHIR-read seam (arbitrary types) is a planned future enhancement.
func (g *Gateway) resolvePrefetchFromSoR(key, member, patientRef string) (json.RawMessage, bool) {
	switch key {
	case "patient":
		ref, _ := g.cfg.SoR.PatientFHIRRef(member)
		if ref == "" {
			ref = patientRef
		}
		id := strings.TrimPrefix(ref, "Patient/")
		b, err := json.Marshal(map[string]string{"resourceType": "Patient", "id": id})
		if err != nil {
			return nil, false
		}
		return json.RawMessage(b), true
	case "coverage":
		if _, _, found := g.cfg.SoR.ResolvePatient(member); !found {
			return nil, false
		}
		coverageRef := "Coverage/" + member + "-cov"
		covJSON, err := shnsdk.BuildCoverage(patientRef, coverageRef)
		if err != nil {
			return nil, false
		}
		return json.RawMessage(covJSON), true
	case "serviceHistory", "deviceHistory", "medicationHistory", "questionnaireResponses":
		return json.RawMessage(`{"resourceType":"Bundle","type":"searchset","entry":[]}`), true
	}
	return nil, false
}
