package engine

import (
	"context"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"net/http"
	"strings"
)

// validatePASConsumption protects a participant's local workflow independently
// of optional conformance policy. The authenticated response leg correlates an
// answer with the dispatch; payload patient identity and any asserted request
// linkage must independently agree before a decision can drive local actions.
func (g *Gateway) validatePASConsumption(ctx context.Context, sub pasSubmission, pci, payer string) (int, string) {
	if err := shnsdk.ValidatePASResponseLinkage(sub.bundleJSON, sub.respJSON); err != nil {
		return http.StatusBadGateway, "claim response request linkage unavailable"
	}
	status, msg := g.validateConsumedPatient(ctx, sub.respJSON, sub.leg, pci, payer)
	if status != http.StatusServiceUnavailable {
		return status, msg
	}
	// The provider cannot resolve an arbitrary payer-local Patient/id. For an
	// authored transaction, an authenticated response may instead echo the
	// precise patient of the sent Claim. That echo is usable only when the sent
	// Patient's identifier still resolves in this holder's source to the
	// authorized PCI and the response identifies the exact request.
	return g.validateRetainedPatientEcho(ctx, sub.bundleJSON, sub.respJSON, pci)
}

func (g *Gateway) validateRetainedPatientEcho(ctx context.Context, sent, answer []byte, pci string) (int, string) {
	unavailable := func() (int, string) {
		return http.StatusServiceUnavailable, "received response patient linkage unavailable"
	}
	if pci == "" || g.cfg.HolderID == "" || g.cfg.SubjectReferenceResolver == nil {
		return unavailable()
	}
	request, ok := structuralObject(CheckInput{Body: sent})
	if !ok || !resourceIs(request, "Bundle") {
		return unavailable()
	}
	entries, _ := request["entry"].([]any)
	type sentClaim struct {
		resource                map[string]any
		id, fullURL, patientRef string
	}
	var claims []sentClaim
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		resource, _ := entry["resource"].(map[string]any)
		if resourceIs(resource, "Claim") {
			patient, _ := resource["patient"].(map[string]any)
			patientRef, _ := patient["reference"].(string)
			id, _ := resource["id"].(string)
			fullURL, _ := entry["fullUrl"].(string)
			claims = append(claims, sentClaim{resource, id, fullURL, patientRef})
		}
	}
	if len(claims) == 0 || len(claims) > 2 {
		return unavailable()
	}
	claim := claims[0]
	if len(claims) == 2 {
		// PAS 2.1+ amendments include the original submitted Claim after the
		// operative Claim. The first Claim is operative by PAS, and the SDK
		// already certified the response's request against that sent Claim.
		// Require its unique relation to the retained second Claim, with the
		// same exact patient reference, before using its Patient for an echo.
		prior := claims[1]
		if prior.id == "" || prior.id == claim.id || claim.patientRef == "" || claim.patientRef != prior.patientRef {
			return unavailable()
		}
		matches := 0
		related, _ := claim.resource["related"].([]any)
		for _, raw := range related {
			relation, _ := raw.(map[string]any)
			linked, _ := relation["claim"].(map[string]any)
			ref, _ := linked["reference"].(string)
			if ref != "" && (ref == "Claim/"+prior.id || prior.fullURL != "" && ref == prior.fullURL) {
				matches++
			}
		}
		if matches != 1 {
			return unavailable()
		}
	}
	patientRef := claim.patientRef
	if patientRef == "" {
		return unavailable()
	}
	var sentPatient map[string]any
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		resource, _ := entry["resource"].(map[string]any)
		if !resourceIs(resource, "Patient") {
			continue
		}
		id, _ := resource["id"].(string)
		fullURL, _ := entry["fullUrl"].(string)
		if id == "" || patientRef != "Patient/"+id && patientRef != fullURL {
			continue
		}
		if sentPatient != nil {
			return unavailable() // ambiguous sent Patient identity
		}
		sentPatient = resource
	}
	if sentPatient == nil {
		return unavailable()
	}
	response, ok := structuralObject(CheckInput{Body: answer})
	if !ok {
		return unavailable()
	}
	var decision map[string]any
	if resourceIs(response, "ClaimResponse") {
		decision = response
	} else if resourceIs(response, "Bundle") {
		answerEntries, _ := response["entry"].([]any)
		for _, raw := range answerEntries {
			entry, _ := raw.(map[string]any)
			resource, _ := entry["resource"].(map[string]any)
			if resourceIs(resource, "ClaimResponse") {
				if decision != nil {
					return unavailable()
				}
				decision = resource
			}
		}
	}
	if decision == nil {
		return unavailable()
	}
	requestLink, _ := decision["request"].(map[string]any)
	if requestLink == nil { // SDK permits absent linkage for transport, not local use
		return unavailable()
	}
	patient, _ := decision["patient"].(map[string]any)
	responseRef, _ := patient["reference"].(string)
	if responseRef == "" {
		return unavailable()
	}
	if resourceIs(response, "Bundle") && !consistentPASResponseSubjects(answer) {
		return http.StatusBadGateway, "received response patient does not match the workflow"
	}
	if responseRef != patientRef {
		// Different reference text does not prove a different person across
		// holder namespaces. A source-proven contradiction was refused by the
		// direct check; an unresolved alternate reference cannot authorize this
		// local action through the exact-echo fallback.
		return unavailable()
	}
	identifiers, _ := sentPatient["identifier"].([]any)
	linked := false
	for _, raw := range identifiers {
		identifier, _ := raw.(map[string]any)
		system, _ := identifier["system"].(string)
		value, _ := identifier["value"].(string)
		if system == "" || value == "" {
			continue
		}
		resolved, found, err := g.cfg.SubjectReferenceResolver.ResolveSubject(ctx, PatientReference{g.cfg.HolderID, system, value})
		if err != nil {
			return unavailable()
		}
		if found {
			if resolved == "" || resolved != pci {
				return http.StatusBadGateway, "sent request patient does not match the workflow"
			}
			linked = true
		}
	}
	if !linked {
		return unavailable()
	}
	return 0, ""
}

func (g *Gateway) validateConsumedPatient(ctx context.Context, body []byte, leg, pci, producer string) (int, string) {
	input := CheckInput{Exchange: ExchangeContext{holder: g.cfg.HolderID, recipient: producer, subjectPCI: pci, legType: leg}, Direction: "response", Status: 200, Body: body}
	root, ok := structuralObject(input)
	if !ok {
		return http.StatusBadGateway, "received response unreadable"
	}
	// Submit/update parsing and linkage require exactly one direct decision.
	// Other resources cannot supply a patient missing from that decision.
	var decision map[string]any
	if resourceIs(root, "ClaimResponse") {
		decision = root
	} else if resourceIs(root, "Bundle") {
		entries, _ := root["entry"].([]any)
		for _, entry := range entries {
			e, _ := entry.(map[string]any)
			r, _ := e["resource"].(map[string]any)
			if resourceIs(r, "ClaimResponse") {
				if decision != nil {
					return http.StatusBadGateway, "received response decision ambiguous"
				}
				decision = r
			}
		}
	}
	if decision == nil {
		return http.StatusBadGateway, "received response decision unavailable"
	}
	result := g.checkSubjectConsistencyResource(ctx, input, decision)
	if result.State == CheckValid {
		// Preserve the existing independent checks on the other consumed
		// resources after the decision itself has authoritative patient proof.
		result = g.checkSubjectConsistency(ctx, input)
	}
	switch result.State {
	case CheckValid:
		// An embedded Patient's identifier may bind in source even when the
		// decision's own reference points into an unknown namespace. The
		// decision reference needs its own authoritative link; otherwise only
		// the exact sent-Claim echo below can bind the local action.
		return g.validateDirectDecisionPatient(ctx, decision, pci, producer)
	case CheckInvalid:
		return http.StatusBadGateway, "received response patient does not match the workflow"
	default:
		return http.StatusServiceUnavailable, "received response patient linkage unavailable"
	}
}

func (g *Gateway) validateDirectDecisionPatient(ctx context.Context, decision map[string]any, pci, producer string) (int, string) {
	unavailable := func() (int, string) {
		return http.StatusServiceUnavailable, "received response patient linkage unavailable"
	}
	patient, _ := decision["patient"].(map[string]any)
	value, _ := patient["reference"].(string)
	// Contained and Bundle-local UUID references have no independent holder
	// namespace. Reaching this helper with CheckValid means the scoped Patient
	// was resolved through its source-issued identifier already.
	if strings.HasPrefix(value, "#") || strings.HasPrefix(value, "urn:uuid:") {
		return 0, ""
	}
	ref, ok := patientReference(producer, value)
	if !ok || g.cfg.SubjectReferenceResolver == nil || pci == "" {
		return unavailable()
	}
	resolved, found, err := g.cfg.SubjectReferenceResolver.ResolveSubject(ctx, ref)
	if err != nil || !found || resolved == "" {
		return unavailable()
	}
	if resolved != pci {
		return http.StatusBadGateway, "received response patient does not match the workflow"
	}
	return 0, ""
}

func (g *Gateway) validateInquiryPatient(ctx context.Context, selected shnsdk.PASInquirySelection, pci, payer string, sent []byte) (int, string) {
	resource, ok := structuralObject(CheckInput{Body: selected.Response})
	if !ok {
		return http.StatusBadGateway, "selected inquiry response unreadable"
	}
	result := g.checkSubjectConsistencyResource(ctx, CheckInput{Exchange: ExchangeContext{holder: g.cfg.HolderID, recipient: payer, subjectPCI: pci, legType: "pas-claim-inquire"}, Direction: "response", Status: 200, Body: selected.Bundle}, resource)
	switch result.State {
	case CheckValid:
		if status, msg := g.validateDirectDecisionPatient(ctx, resource, pci, payer); status != http.StatusServiceUnavailable {
			return status, msg
		}
		return g.validateRetainedPatientEcho(ctx, sent, selected.Response, pci)
	case CheckInvalid:
		return http.StatusBadGateway, "received response patient does not match the workflow"
	default:
		// The continuation has independently verified the response's request
		// against the original Claim before this call. The newly authored
		// inquiry's Patient is re-read from this holder's source, and can bind
		// an exact echoed patient reference to that same authorized PCI.
		return g.validateRetainedPatientEcho(ctx, sent, selected.Response, pci)
	}
}
