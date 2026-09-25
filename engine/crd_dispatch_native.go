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
// ONE member, bound by this payer's own system. performer (the supplier Organization) is non-patient —
// required-present but not subject-fenced. Returns the resolved order + coverage JSON for the caller's
// ingress-$validate and the payer's binding of the member, or (nil, nil, "", status, msg).
//
// The request's subject is context.patientId: it must be present, and it is bound by this payer's
// own system (bindInboundSubject; RuleSubjectPCI when the participant requires known members).
// These, and a request that cannot be read, refuse at every level.
// The rest is the request's own content, not checked at none, recorded at observe and refused at
// strict with strict's status and body: a value of the wrong type outside context.patientId and
// prefetch, no dispatchedOrders, no performer and a dispatched order the prefetch does not carry
// (RuleRequestShape); a dispatched order naming no patient, and an
// order or coverage naming another patient (RulePatientMixed). When the system of record cannot
// answer that consistency check, strict keeps its failure and below strict the request is
// forwarded as sent.
func (g *Gateway) conformantCRDDispatchBindContext(ctx context.Context, reqJSON []byte) (orderJSON, covJSON []byte, pci string, status int, msg string) {
	// The subject (context.patientId) and the prefetch the dispatched orders and
	// the coverage are read from must read; a value of the wrong type anywhere
	// else is the request's own shape (RuleRequestShape). The hook is read by the
	// responder on its own (selectCRDService).
	refuses := func(rule string) bool { return g.guard(ctx, KindContent, rule, reqJSON) }
	var req dispatchCDSRequest
	var core struct {
		Context struct {
			PatientID string `json:"patientId"`
		} `json:"context"`
		Prefetch map[string]json.RawMessage `json:"prefetch"`
	}
	if err := decodeContent(reqJSON, &req, &core, func() bool { return refuses(RuleRequestShape) }); err != nil {
		return nil, nil, "", http.StatusBadRequest, "parse cds request failed"
	}
	if req.Context.PatientID == "" {
		return nil, nil, "", http.StatusBadRequest, "missing context.patientId"
	}
	if len(req.Context.DispatchedOrders) == 0 && refuses(RuleRequestShape) {
		return nil, nil, "", http.StatusBadRequest, "no dispatchedOrders"
	}
	if req.Context.Performer == "" && refuses(RuleRequestShape) {
		return nil, nil, "", http.StatusBadRequest, "missing performer"
	}
	covJSON = req.Prefetch["coverage"]
	member := strings.TrimPrefix(req.Context.PatientID, "Patient/")
	// pci is the payer's binding of the context member, for the payload's own
	// consistency and for the responder.
	pci, status, msg = g.bindInboundSubject(ctx, member, reqJSON)
	if status != 0 {
		return nil, nil, "", status, msg
	}
	// sameSubject reports whether the patient ref binds to pci. When the
	// system of record cannot answer, strict refuses with its failure; below
	// strict the check could not finish and the request is forwarded as sent
	// (the second return is then true, with no refusal).
	sameSubject := func(ref string) (bool, int, string) {
		rp, ok, readErr := g.resolveSubjectPCI(ctx, strings.TrimPrefix(ref, "Patient/"), reqJSON)
		if readErr != nil {
			if g.guardUnavailable(ctx, KindContent, RulePatientMixed, reqJSON) {
				status, msg := SoRFailureResponse(readErr)
				return false, status, msg
			}
			return true, 0, ""
		}
		return ok && rp == pci, 0, ""
	}
	// Fence EVERY dispatched order's subject to the bound pci (AI-11: every patient-bearing field) —
	// the handler forwards ALL dispatched orders verbatim, so fencing only the first would let a
	// second, wrong-patient order ride through. Mirrors ingressCRDSubjectPCI's iterate-all rigor.
	var firstOrder []byte
	for _, ordRef := range req.Context.DispatchedOrders {
		order, found := findInPrefetchByRef(req.Prefetch, ordRef)
		if !found {
			if refuses(RuleRequestShape) {
				return nil, nil, "", http.StatusBadRequest, "dispatched order not resolvable from prefetch"
			}
			continue
		}
		if firstOrder == nil {
			firstOrder = order
		}
		subj := patientRefOf(order)
		if subj == "" {
			if refuses(RulePatientMixed) {
				return nil, nil, "", http.StatusForbidden, "dispatched order missing patient subject"
			}
			continue
		}
		same, status, msg := sameSubject(subj)
		if status != 0 {
			return nil, nil, "", status, msg
		}
		if !same && refuses(RulePatientMixed) {
			return nil, nil, "", http.StatusForbidden, "inconsistent patient in order-dispatch"
		}
	}
	// Coverage beneficiary (when present) must bind to the same pci.
	if ben := coverageBeneficiaryFromPrefetch(covJSON); ben != "" {
		same, status, msg := sameSubject(ben)
		if status != 0 {
			return nil, nil, "", status, msg
		}
		if !same && refuses(RulePatientMixed) {
			return nil, nil, "", http.StatusForbidden, "inconsistent patient in order-dispatch"
		}
	}
	return firstOrder, covJSON, pci, 0, ""
}

// handleCRDDispatchInbound serves the conformant crd-order-dispatch leg. Mirrors handleCRDNativeInbound:
// subject-bind, ingress-validate the resolved DeviceRequest + coverage (validateFHIR respects A4's R-8
// skip on br-payer-targeting lanes), then forward the verbatim request to the responder.
func (g *Gateway) handleCRDDispatchInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, reqJSON []byte, answerTok string) {
	ctx := r.Context()
	orderJSON, _, subjectPCI, status, msg := g.conformantCRDDispatchBindContext(ctx, reqJSON)
	if status != 0 {
		g.refuseInbound(w, r, legCRDOrderDispatch, env, tok, answerTok, status, msg, nil)
		return
	}
	g.noteSubjectBinding("crd-order-dispatch", env.Metadata.CorrelationID, tok.Subject, subjectPCI)
	// Ingress-$validate the resolved DeviceRequest (SHN-shaped order; US Core warns-passes an
	// unprofiled type). We deliberately do NOT $validate the COVERAGE here: for order-dispatch the
	// coverage rides as a PREFETCH BUNDLE whose entry fullUrls are the relative "Type/id" form
	// br-payer's findInBundle resolves by — but a US-Core $validate rejects a relative fullUrl
	// ("must be an absolute URL"). That bundle is a relayed prefetch container carrying the payer's
	// OWN Org (R-8: SHN doesn't $validate relayed foreign bytes), and the AI-11 bind already
	// subject-fenced the coverage beneficiary. (The bare-Coverage order-select path still validates
	// its coverage — that one is not a bundle.)
	// Below strict a request carried with no resolvable order has none to validate.
	if len(orderJSON) > 0 {
		if status, msg := g.validateFHIR(ctx, orderJSON, "ingress", ""); status != 0 {
			g.refuseInbound(w, r, legCRDOrderDispatch, env, tok, answerTok, status, msg, nil)
			return
		}
	}
	result, err := g.cfg.Responder.Handle(ctx, "crd-order-dispatch", env.Metadata.CorrelationID, subjectPCI, reqJSON)
	if err != nil {
		g.responderFailed(w, r, legCRDOrderDispatch, env, tok, answerTok, err)
		return
	}
	if result.Status != 0 {
		g.respondLegError(w, r, "payer-coverage", "crd-dispatch-cards", "crd-order-dispatch",
			env.Metadata.CorrelationID, result, tok.Subject, env.Metadata.Sender, "", answerTok)
		return
	}
	g.observeCRDEmbedded(ctx, "crd-order-dispatch", env.Metadata.CorrelationID, result.Response)
	g.respondLeg(w, r, "payer-coverage", "crd-dispatch-cards", "crd-order-dispatch", env.Metadata.CorrelationID, result.Response, tok.Subject, env.Metadata.Sender, "", answerTok)
}
