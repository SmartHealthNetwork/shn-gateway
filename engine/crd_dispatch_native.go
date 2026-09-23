// crd_dispatch_native.go — the conformant crd-order-dispatch leg: the payer-side inbound handler +
// AI-11 subject-bind for an order-dispatch CDS Hooks request (context.dispatchedOrders + performer,
// resolved from prefetch — NOT draftOrders). The order-dispatch sibling of crd_native.go. The card
// is advisory (payer Org wins br-payer's First([Organization])); the leg's job is to advertise the
// questionnaire-package + carry the request to br-payer's order-dispatch-crd.
package engine

import (
	"context"
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
		if decodeMessage(raw, &probe) == nil && probe.ResourceType != "Bundle" && probe.ID == id {
			return raw, true
		}
		var b struct {
			ResourceType string `json:"resourceType"`
			Entry        []struct {
				FullURL  string          `json:"fullUrl"`
				Resource json.RawMessage `json:"resource"`
			} `json:"entry"`
		}
		if decodeMessage(raw, &b) == nil && b.ResourceType == "Bundle" {
			for _, e := range b.Entry {
				var rp struct {
					ID string `json:"id"`
				}
				_ = decodeMessage(e.Resource, &rp)
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
	if decodeMessage(cov, &b) == nil {
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
func (g *Gateway) conformantCRDDispatchBindContext(ctx context.Context, reqJSON []byte, tokSubject string) (orderJSON, covJSON []byte, status int, msg string) {
	var req dispatchCDSRequest
	if err := decodeMessage(reqJSON, &req); err != nil {
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
	pci, ok, readErr := g.resolveSubjectPCI(ctx, member, reqJSON)
	if readErr != nil {
		status, msg := SoRFailureResponse(readErr)
		return nil, nil, status, msg
	}
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
		rp, ok, readErr := g.resolveSubjectPCI(ctx, m, reqJSON)
		if readErr != nil {
			status, msg := SoRFailureResponse(readErr)
			return nil, nil, status, msg
		}
		if !ok || rp != pci {
			return nil, nil, http.StatusForbidden, "inconsistent patient in order-dispatch"
		}
	}
	// Coverage beneficiary (when present) must bind to the same pci.
	if ben := coverageBeneficiaryFromPrefetch(covJSON); ben != "" {
		m := strings.TrimPrefix(ben, "Patient/")
		rp, ok, readErr := g.resolveSubjectPCI(ctx, m, reqJSON)
		if readErr != nil {
			status, msg := SoRFailureResponse(readErr)
			return nil, nil, status, msg
		}
		if !ok || rp != pci {
			return nil, nil, http.StatusForbidden, "inconsistent patient in order-dispatch"
		}
	}
	return firstOrder, covJSON, 0, ""
}

// handleCRDDispatchInbound delegates verified dispatch delivery to the shared native boundary.
func (g *Gateway) handleCRDDispatchInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, reqJSON []byte, answerTok string) {
	g.handleNativeInbound(w, r, "crd-order-dispatch", env, tok, reqJSON, answerTok)
}
