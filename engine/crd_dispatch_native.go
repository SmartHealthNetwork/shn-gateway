// crd_dispatch_native.go — the conformant crd-order-dispatch leg: the payer-side inbound handler +
// AI-11 subject-bind for an order-dispatch CDS Hooks request (context.dispatchedOrders + performer,
// resolved from prefetch — NOT draftOrders). The order-dispatch sibling of crd_native.go. The card
// is advisory (payer Org wins br-payer's First([Organization])); the leg's job is to advertise the
// questionnaire-package + carry the request to br-payer's order-dispatch-crd.
package engine

import (
	"encoding/json"
	"net/http"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

type dispatchCDSRequest struct {
	Hook    string `json:"hook"`
	Context struct {
		PatientID        string   `json:"patientId"`
		DispatchedOrders []string `json:"dispatchedOrders"`
		Performer        string   `json:"performer"`
	} `json:"context"`
	Prefetch map[string]json.RawMessage `json:"prefetch"`
}

// findInPrefetchByRef returns the first prefetch resource (direct or inside a Bundle) whose id or
// fullUrl matches ref. Mirrors br-payer's ResourceResolver.findInPrefetch so the gateway subject-
// fences the dispatched order the SAME way the payer resolves it.
func findInPrefetchByRef(prefetch map[string]json.RawMessage, ref string) ([]byte, bool) {
	id := ref[strings.LastIndex(ref, "/")+1:]
	for _, raw := range prefetch {
		var probe struct {
			ResourceType string `json:"resourceType"`
			ID           string `json:"id"`
		}
		if json.Unmarshal(raw, &probe) == nil && probe.ResourceType != "Bundle" && probe.ID == id {
			return raw, true
		}
		var b struct {
			ResourceType string `json:"resourceType"`
			Entry        []struct {
				FullURL  string          `json:"fullUrl"`
				Resource json.RawMessage `json:"resource"`
			} `json:"entry"`
		}
		if json.Unmarshal(raw, &b) == nil && b.ResourceType == "Bundle" {
			for _, e := range b.Entry {
				var rp struct {
					ID string `json:"id"`
				}
				_ = json.Unmarshal(e.Resource, &rp)
				if rp.ID == id || e.FullURL == ref || strings.HasSuffix(e.FullURL, "/"+id) {
					return e.Resource, true
				}
			}
		}
	}
	return nil, false
}

// coverageBeneficiaryFromPrefetch reads Coverage.beneficiary from the coverage prefetch (bare
// Coverage or a Bundle of one). "" if absent. Uses the shared patientRefOf helper.
func coverageBeneficiaryFromPrefetch(cov json.RawMessage) string {
	if ref := patientRefOf(cov); ref != "" {
		return ref
	}
	var b struct {
		Entry []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if json.Unmarshal(cov, &b) == nil {
		for _, e := range b.Entry {
			if ref := patientRefOf(e.Resource); ref != "" {
				return ref
			}
		}
	}
	return ""
}

// conformantCRDDispatchBind is the AI-11 authority check for an order-dispatch request: the dispatched
// order's (DeviceRequest) subject, the Coverage beneficiary, and context.patientId must all resolve to
// ONE member == the token subject. performer (the supplier Organization) is non-patient — required-
// present but not subject-fenced. Returns the resolved order + coverage JSON for the caller's
// ingress-$validate, or (nil,nil,status,msg).
func (g *Gateway) conformantCRDDispatchBind(reqJSON []byte, tokSubject string) (orderJSON, covJSON []byte, status int, msg string) {
	var req dispatchCDSRequest
	if err := json.Unmarshal(reqJSON, &req); err != nil {
		return nil, nil, http.StatusBadRequest, "parse cds request failed"
	}
	if req.Context.PatientID == "" {
		return nil, nil, http.StatusBadRequest, "missing context.patientId"
	}
	if len(req.Context.DispatchedOrders) == 0 {
		return nil, nil, http.StatusBadRequest, "no dispatchedOrders"
	}
	if req.Context.Performer == "" {
		return nil, nil, http.StatusBadRequest, "missing performer"
	}
	covJSON = req.Prefetch["coverage"]
	member := strings.TrimPrefix(req.Context.PatientID, "Patient/")
	pci, _, ok := g.cfg.SoR.ResolvePatient(member)
	if !ok {
		return nil, nil, http.StatusBadRequest, "unknown member"
	}
	if pci != tokSubject {
		return nil, nil, http.StatusForbidden, "token subject does not match request patient"
	}
	// Fence EVERY dispatched order's subject to the bound pci (AI-11: every patient-bearing field) —
	// the handler forwards ALL dispatched orders verbatim, so fencing only the first would let a
	// second, wrong-patient order ride through. Mirrors ingressCRDSubjectPCI's iterate-all rigor.
	var firstOrder []byte
	for _, ordRef := range req.Context.DispatchedOrders {
		order, found := findInPrefetchByRef(req.Prefetch, ordRef)
		if !found {
			return nil, nil, http.StatusBadRequest, "dispatched order not resolvable from prefetch"
		}
		if firstOrder == nil {
			firstOrder = order
		}
		subj := patientRefOf(order)
		if subj == "" {
			return nil, nil, http.StatusForbidden, "dispatched order missing patient subject"
		}
		m := strings.TrimPrefix(subj, "Patient/")
		rp, _, ok := g.cfg.SoR.ResolvePatient(m)
		if !ok || rp != pci {
			return nil, nil, http.StatusForbidden, "inconsistent patient in order-dispatch"
		}
	}
	// Coverage beneficiary (when present) must bind to the same pci.
	if ben := coverageBeneficiaryFromPrefetch(covJSON); ben != "" {
		m := strings.TrimPrefix(ben, "Patient/")
		if rp, _, ok := g.cfg.SoR.ResolvePatient(m); !ok || rp != pci {
			return nil, nil, http.StatusForbidden, "inconsistent patient in order-dispatch"
		}
	}
	return firstOrder, covJSON, 0, ""
}

// firstDispatchedOrder extracts the first order-dispatch order (a DeviceRequest OR
// ServiceRequest) from the raw dispatch CDS request's prefetch — mirrors
// conformantCRDDispatchBind's own resolution, minus the AI-11 subject-fence (the bind already
// ran it before the sandbox responder's crd-order-dispatch case is ever reached). Used only to
// read the order's product coding for the sandbox's OrderSelect decision (D-S7K-13,
// responder-parity correction) — never a subject-authority source.
func firstDispatchedOrder(reqJSON []byte) ([]byte, bool) {
	var req dispatchCDSRequest
	if err := json.Unmarshal(reqJSON, &req); err != nil {
		return nil, false
	}
	if len(req.Context.DispatchedOrders) == 0 {
		return nil, false
	}
	return findInPrefetchByRef(req.Prefetch, req.Context.DispatchedOrders[0])
}

// handleCRDDispatchInbound serves the conformant crd-order-dispatch leg. Mirrors handleCRDNativeInbound:
// subject-bind, ingress-validate the resolved DeviceRequest + coverage (validateFHIR respects A4's R-8
// skip on br-payer-targeting lanes), then forward the verbatim request to the responder.
func (g *Gateway) handleCRDDispatchInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token) {
	ctx := r.Context()
	reqJSON, err := shnsdk.Open(env, g.cfg.Identity.EncPub, g.cfg.Identity.EncPriv)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decryption failed"})
		return
	}
	orderJSON, _, status, msg := g.conformantCRDDispatchBind(reqJSON, tok.Subject)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	// Ingress-$validate the resolved DeviceRequest (SHN-shaped order; US Core warns-passes an
	// unprofiled type). We deliberately do NOT $validate the COVERAGE here: for order-dispatch the
	// coverage rides as a PREFETCH BUNDLE whose entry fullUrls are the relative "Type/id" form
	// br-payer's findInBundle resolves by — but a US-Core $validate rejects a relative fullUrl
	// ("must be an absolute URL"). That bundle is a relayed prefetch container carrying the payer's
	// OWN Org (R-8: SHN doesn't $validate relayed foreign bytes), and the AI-11 bind already
	// subject-fenced the coverage beneficiary. (The bare-Coverage order-select path still validates
	// its coverage — that one is not a bundle.)
	if status, msg := g.validateFHIR(ctx, orderJSON, "ingress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	result, err := g.cfg.Responder.Handle(ctx, "crd-order-dispatch", env.Metadata.CorrelationID, tok.Subject, reqJSON)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "responder failed"})
		return
	}
	if result.Status != 0 {
		writeJSON(w, result.Status, map[string]string{"error": result.Message})
		return
	}
	g.respondLeg(w, r, "payer-coverage", "crd-dispatch-cards", "crd-order-dispatch", env.Metadata.CorrelationID, result.ResponseFHIR, tok.Subject, env.Metadata.Sender, "")
}
