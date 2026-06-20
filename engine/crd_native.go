// crd_native.go — the CONFORMANT CRD leg (crd-order-select-native): the payer-side inbound
// handler + subject-bind for a conformant CDS Hooks order-select request (context.draftOrders
// is a FHIR Bundle). This is the rung-1 faithful pass-through path (br-provider's verbatim bytes
// → br-payer); the minimized crd-order-select leg (handleCRDInbound) is the rung-2 construct
// path and is UNTOUCHED.
package engine

import (
	"encoding/json"
	"net/http"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// extractConformantSR pulls the ServiceRequest JSON from a conformant CDS Hooks request's
// draftOrders Bundle (the first entry whose resource is a ServiceRequest).
//
// NOTE: this returns the FIRST ServiceRequest only. The payer-side bind below is
// defense-in-depth, NOT the comprehensive guard: the INGRESS (ingressCRDSubjectPCI,
// ingress_crd.go) already enforces that EVERY draftOrders entry + prefetch resource resolves
// to the bound PCI before the request is sealed, and the ingress is the sole origin of this
// leg's sealed bytes. A multi-entry Bundle with a rogue second SR is rejected at the ingress.
func extractConformantSR(reqJSON []byte) ([]byte, bool) {
	var req ingressCDSRequest
	if err := json.Unmarshal(reqJSON, &req); err != nil {
		return nil, false
	}
	sr := firstServiceRequest(req)
	return sr, sr != nil
}

// firstServiceRequest returns the first draftOrders entry whose resource is a ServiceRequest,
// from an already-parsed request (so callers that already hold the parsed request do not
// re-unmarshal). nil if none.
func firstServiceRequest(req ingressCDSRequest) []byte {
	for _, e := range req.Context.DraftOrders.Entry {
		var probe struct {
			ResourceType string `json:"resourceType"`
		}
		_ = json.Unmarshal(e.Resource, &probe)
		if probe.ResourceType == "ServiceRequest" {
			return e.Resource
		}
	}
	return nil
}

// conformantCRDBind subject-binds a conformant order-select request to tokSubject (the payer's
// inbound token PCI): the ServiceRequest subject, the Coverage beneficiary, and context.patientId
// must all reference one member resolving to tokSubject. Returns the SR JSON AND the coverage JSON
// for downstream validation (so the caller need not re-parse the request), or (nil, nil, status,
// msg). The conformant sibling of handleCRDInbound's minimized bind (payer.go) and
// bindBundleSubject (payer.go:149).
func (g *Gateway) conformantCRDBind(reqJSON []byte, tokSubject string) (srJSON, covJSON []byte, status int, msg string) {
	var req ingressCDSRequest
	if err := json.Unmarshal(reqJSON, &req); err != nil {
		return nil, nil, http.StatusBadRequest, "parse cds request failed"
	}
	srJSON = firstServiceRequest(req)
	if len(srJSON) == 0 {
		return nil, nil, http.StatusBadRequest, "no ServiceRequest in draftOrders"
	}
	covJSON = req.Prefetch["coverage"]
	srSubjectRef, err := shnsdk.ParseServiceRequestSubject(srJSON)
	if err != nil {
		return nil, nil, http.StatusBadRequest, "parse service request subject failed"
	}
	covBeneRef, err := shnsdk.ParseCoverageBeneficiary(covJSON)
	if err != nil {
		return nil, nil, http.StatusBadRequest, "parse coverage beneficiary failed"
	}
	srMember := strings.TrimPrefix(srSubjectRef, "Patient/")
	covMember := strings.TrimPrefix(covBeneRef, "Patient/")
	ctxMember := strings.TrimPrefix(req.Context.PatientID, "Patient/")
	if srMember != covMember || srMember != ctxMember {
		return nil, nil, http.StatusBadRequest, "inconsistent patient in order-select"
	}
	pci, _, found := g.cfg.SoR.ResolvePatient(srMember)
	if !found {
		return nil, nil, http.StatusBadRequest, "unknown member"
	}
	if pci != tokSubject {
		return nil, nil, http.StatusForbidden, "token subject does not match request patient"
	}
	return srJSON, covJSON, 0, ""
}

// handleCRDNativeInbound serves the conformant CRD leg: subject-bind on the conformant shape,
// ingress-validate the SR + coverage, then forward the VERBATIM conformant bytes to the responder
// (sandbox adjudicates / native forwards to the real RI). Mirrors handleCRDInbound's structure for
// the conformant shape; the existing minimized handler is untouched.
func (g *Gateway) handleCRDNativeInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token) {
	ctx := r.Context()
	reqJSON, err := shnsdk.Open(env, g.cfg.Identity.EncPub, g.cfg.Identity.EncPriv)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decryption failed"})
		return
	}
	srJSON, covJSON, status, msg := g.conformantCRDBind(reqJSON, tok.Subject)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	if status, msg := g.validateFHIR(ctx, srJSON, "ingress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	if len(covJSON) > 0 {
		if status, msg := g.validateFHIR(ctx, covJSON, "ingress"); status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
	}
	result, err := g.cfg.Responder.Handle(ctx, "crd-order-select-native", env.Metadata.CorrelationID, tok.Subject, reqJSON)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "responder failed"})
		return
	}
	if result.Status != 0 {
		writeJSON(w, result.Status, map[string]string{"error": result.Message})
		return
	}
	g.respondLeg(w, r, "payer-coverage", "crd-cards", "crd-order-select-native", env.Metadata.CorrelationID, result.ResponseFHIR, tok.Subject, env.Metadata.Sender, "")
}
