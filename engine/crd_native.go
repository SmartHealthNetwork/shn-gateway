// crd_native.go — the CONFORMANT CRD leg (crd-order-select): the payer-side inbound
// handler + subject-bind for a conformant CDS Hooks order-select request (context.draftOrders
// is a FHIR Bundle). This is the only CRD coverage-discovery contract (br-provider's verbatim
// bytes → br-payer); the minimized crd-order-select leg + its handler are no longer
// part of the contract.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// firstOrder returns the first draftOrders entry whose resource is a CDS order — a ServiceRequest
// OR a DeviceRequest. The order-select hook legitimately carries either (UC-04 a G0151 PT
// ServiceRequest; UC-02 an E0250 hospital-bed DeviceRequest — br-payer's CdsResourceExtractor
// accepts both), so the leg's bind is order-type-agnostic. From an already-parsed request (so
// callers that hold the parsed request do not re-unmarshal). nil if none.
func firstOrder(req ingressCDSRequest) []byte {
	for _, e := range req.Context.DraftOrders.Entry {
		var probe struct {
			ResourceType string `json:"resourceType"`
		}
		_ = decodeMessage(e.Resource, &probe)
		if probe.ResourceType == "ServiceRequest" || probe.ResourceType == "DeviceRequest" {
			return e.Resource
		}
	}
	return nil
}

// orderSubjectRef reads subject.reference from a CDS order resource (ServiceRequest OR
// DeviceRequest — both carry the patient as subject.reference). Used by the order-select subject
// bind, which is order-type-agnostic (the SDK's ParseServiceRequestSubject is SR-locked, so the
// bind parses the subject here instead — keeping the SDK out of the payer-side gateway path).
func orderSubjectRef(orderJSON []byte) (string, bool) {
	var probe struct {
		Subject struct {
			Reference string `json:"reference"`
		} `json:"subject"`
	}
	if err := decodeMessage(orderJSON, &probe); err != nil || probe.Subject.Reference == "" {
		return "", false
	}
	return probe.Subject.Reference, true
}

// conformantCRDBind subject-binds a conformant order-select request to tokSubject (the payer's
// inbound token PCI): the ServiceRequest subject, the Coverage beneficiary, and context.patientId
// must all reference one member resolving to tokSubject. Returns the SR JSON AND the coverage JSON
// for downstream validation (so the caller need not re-parse the request), or (nil, nil, status,
// msg). The conformant sibling of handleCRDInbound's minimized bind (payer.go) and
// bindBundleSubject (payer.go:149).
func (g *Gateway) conformantCRDBindContext(ctx context.Context, reqJSON []byte, tokSubject string) (srJSON, covJSON []byte, status int, msg string) {
	var req ingressCDSRequest
	if err := decodeMessage(reqJSON, &req); err != nil {
		return nil, nil, http.StatusBadRequest, "parse cds request failed"
	}
	srJSON = firstOrder(req)
	if len(srJSON) == 0 {
		return nil, nil, http.StatusBadRequest, "no order (ServiceRequest or DeviceRequest) in draftOrders"
	}
	covJSON = req.Prefetch["coverage"]
	srSubjectRef, ok := orderSubjectRef(srJSON)
	if !ok {
		return nil, nil, http.StatusBadRequest, "parse order subject failed"
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
	pci, _, found, readErr := ReadSystemOfRecord(g.cfg.SoR).ResolvePatientContext(ctx, srMember)
	if readErr != nil {
		status, msg := SoRFailureResponse(readErr)
		return nil, nil, status, msg
	}
	if !found {
		return nil, nil, http.StatusBadRequest, "unknown member"
	}
	if pci != tokSubject {
		return nil, nil, http.StatusForbidden, "token subject does not match request patient"
	}
	return srJSON, covJSON, 0, ""
}

// CRDEmbeddedValidatedEvent is the observer event kind for a FHIR resource
// embedded in a payer's CDS Hooks answer that was validated at the served CRD
// line. The validation only observes: its outcome never changes the answer.
const CRDEmbeddedValidatedEvent = "crd.embedded.validated"

// crdEmbeddedValidation is the metadata a CRDEmbeddedValidatedEvent carries.
type crdEmbeddedValidation struct {
	Path         string `json:"path"`
	ResourceType string `json:"resourceType"`
	Line         string `json:"line"`
	Outcome      string `json:"outcome"` // valid | invalid | unavailable
}

// Bounds of the embedded validation one answer gets.
const (
	crdEmbeddedValidationMax     = 16
	crdEmbeddedValidationTimeout = 5 * time.Second
)

// observeCRDEmbedded validates each resource a CDS Hooks answer embeds (in a
// system action, or in a card suggestion's action) at the served CRD line, and
// records each outcome as a CRDEmbeddedValidatedEvent and a log line. Nothing
// is refused: a payer's content that does not validate is the payer's to fix,
// and the answer is relayed as it is.
func (g *Gateway) observeCRDEmbedded(ctx context.Context, leg, corr string, answer relay.Payload) {
	raw, err := relay.Transmit(answer, relay.Check(answerKey(leg, relay.OutcomeAnswered)))
	if err != nil {
		return // the response transmit refuses it too
	}
	type action struct {
		Resource json.RawMessage `json:"resource"`
	}
	var doc struct {
		SystemActions []action `json:"systemActions"`
		Cards         []struct {
			Suggestions []struct {
				Actions []action `json:"actions"`
			} `json:"suggestions"`
		} `json:"cards"`
	}
	if decodeMessage(raw, &doc) != nil {
		return
	}
	type embedded struct {
		path     string
		resource json.RawMessage
	}
	var found []embedded
	for i, a := range doc.SystemActions {
		found = append(found, embedded{fmt.Sprintf("systemActions[%d].resource", i), a.Resource})
	}
	for i, c := range doc.Cards {
		for j, s := range c.Suggestions {
			for k, a := range s.Actions {
				found = append(found, embedded{fmt.Sprintf("cards[%d].suggestions[%d].actions[%d].resource", i, j, k), a.Resource})
			}
		}
	}
	line := answerLineOr(ctx, "pa.crd")
	validator := g.validatorForContractLine("pa.crd", line)
	ctx, cancel := context.WithTimeout(ctx, crdEmbeddedValidationTimeout)
	defer cancel()
	if len(found) > crdEmbeddedValidationMax {
		log.Printf("gateway: %s answer embeds %d resources; the first %d are validated", leg, len(found), crdEmbeddedValidationMax)
		found = found[:crdEmbeddedValidationMax]
	}
	for _, e := range found {
		if len(e.resource) == 0 || string(e.resource) == "null" {
			continue
		}
		var head struct {
			ResourceType string `json:"resourceType"`
		}
		_ = json.Unmarshal(e.resource, &head)
		v := crdEmbeddedValidation{Path: e.path, ResourceType: head.ResourceType, Line: line}
		switch {
		case validator == nil:
			v.Outcome = "unavailable"
		default:
			res, err := validator.Validate(ctx, e.resource, "")
			switch {
			case err != nil:
				v.Outcome = "unavailable"
			case res.Valid:
				v.Outcome = "valid"
			default:
				v.Outcome = "invalid"
			}
		}
		log.Printf("gateway: %s answer %s (%s) at CRD %s: %s (observed, not enforced)", leg, v.Path, v.ResourceType, v.Line, v.Outcome)
		if g.cfg.Observer == nil {
			continue
		}
		detail, err := json.Marshal(v)
		if err != nil {
			continue
		}
		g.observe(ObserverEvent{Kind: CRDEmbeddedValidatedEvent, Direction: "validate", LegType: leg, CorrelationID: corr, Op: v.Path, Detail: string(detail)})
	}
}

// handleCRDNativeInbound serves the conformant CRD leg: subject-bind on the conformant shape,
// ingress-validate the SR + coverage, then forward the VERBATIM conformant bytes to the responder
// (an injected LegResponder decides / native forwards to the real RI). Mirrors handleCRDInbound's structure for
// the conformant shape; the existing minimized handler is untouched.
func (g *Gateway) handleCRDNativeInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, reqJSON []byte, answerTok string) {
	ctx := r.Context()
	srJSON, covJSON, status, msg := g.conformantCRDBindContext(ctx, reqJSON, tok.Subject)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	if status, msg := g.validateFHIR(ctx, srJSON, "ingress", ""); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	if len(covJSON) > 0 {
		if status, msg := g.validateFHIR(ctx, covJSON, "ingress", ""); status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
	}
	result, err := g.cfg.Responder.Handle(ctx, "crd-order-select", env.Metadata.CorrelationID, tok.Subject, reqJSON)
	if err != nil {
		g.responderFailed(w, "crd-order-select", err)
		return
	}
	if result.Status != 0 {
		g.respondLegError(w, r, "payer-coverage", "crd-cards", "crd-order-select",
			env.Metadata.CorrelationID, result, tok.Subject, env.Metadata.Sender, "", answerTok)
		return
	}
	g.observeCRDEmbedded(ctx, "crd-order-select", env.Metadata.CorrelationID, result.Response)
	g.respondLeg(w, r, "payer-coverage", "crd-cards", "crd-order-select", env.Metadata.CorrelationID, result.Response, tok.Subject, env.Metadata.Sender, "", answerTok)
}
