package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

type nativeExchangeKey struct{}

func nativeRequestMedia(ctx context.Context, fallback string) string {
	if ex, ok := ctx.Value(nativeExchangeKey{}).(ExchangeContext); ok && ex.contentType != "" {
		return ex.contentType
	}
	return fallback
}

// dispatchNative prepares the exact source message before the existing authorized
// exchange. Clinical interpretation belongs to an explicit local consumer.
func (g *Gateway) dispatchNative(ctx context.Context, r *http.Request, ex ExchangeContext, request relay.Payload) (ApplicationReply, error) {
	ex, request, err := g.prepareBoundary(ctx, ex, request)
	if err != nil {
		return ApplicationReply{}, err
	}
	content := Content{WorkstreamType: workstreamPA, Payload: request, Carried: true, DeclaredVersion: ex.contractVersion, CRDHook: ex.crdHook}
	if ex.legType == "dtr-questionnaire-fetch" {
		content.Operation = ex.operation
	}
	ctx = context.WithValue(ctx, nativeExchangeKey{}, ex)
	return g.OriginateLegMessage(ctx, r, ex.recipient, ex.legType, ex.subjectPCI, ex.correlationID, ex.custodian, content)
}

func (g *Gateway) handleNativeIngress(w http.ResponseWriter, r *http.Request) {
	w, scope := g.withScope(w, relay.RoleRequester)
	if !strings.HasPrefix(r.URL.Path, "/cds-services/") {
		w = &fhirOperationWriter{w}
	}
	// Reject overflow before authenticating one-use credentials or verifying
	// context against the complete application body. Never admit a prefix.
	body, err := io.ReadAll(io.LimitReader(r.Body, shnsdk.MaxRequestBytes+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body failed"})
		return
	}
	if len(body) > shnsdk.MaxRequestBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body exceeds maximum size"})
		return
	}
	principal, ok, unavailable := g.ingressPrincipal(r)
	if !ok {
		if unavailable {
			writeStoreUnavailable(w, "ingress key store unavailable")
		} else {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "ingress authentication required"})
		}
		return
	}
	ex, err := g.resolveIngressContext(r.Context(), r, principal, body)
	if errors.Is(err, errIngressContextAbsent) {
		ex, err = g.legacyIngressContext(r.Context(), r, body)
	}
	if err != nil {
		g.relayOriginationError(w, err)
		return
	}
	if ex.correlationID == "" {
		ex.correlationID = g.ingressCorrelation(w, r)
	} else {
		w.Header().Set(CorrelationHeader, ex.correlationID)
	}
	scope.leg = ex.legType
	ctx := withFindingContext(r.Context(), findingContext{LegType: ex.legType, CorrelationID: ex.correlationID, Seam: "provider-ingress", Whose: "own"})
	request := relay.Exact(relay.NewBody(body, relay.OriginIngressRequest), ex.contentType)
	exchange := g.exchanges.Begin(workstreamPA)
	reply, err := g.dispatchNative(ctx, r, ex, request)
	outcome := "ok"
	if err != nil || reply.Status/100 != 2 {
		outcome = "error"
	}
	g.recordLeg(exchange.ID, (Leg{Type: ex.legType, Physics: paCatalog[ex.legType].Physics, Subjects: subjectsOf(ex.subjectPCI)}).Project(ex.correlationID, outcome))
	if err != nil {
		if !g.relayOriginationError(w, err) {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		}
		return
	}
	g.writeApplicationReply(w, reply, ex.legType)
}

// legacyIngressContext reads only unsigned addressing hints. Identity must come
// from an explicit authoritative integration; a local demographic hash is not
// an exchange identity. It never enriches or compares clinical payload subjects.
func (g *Gateway) legacyIngressContext(ctx context.Context, r *http.Request, body []byte) (ExchangeContext, error) {
	missing := func() (ExchangeContext, error) {
		return ExchangeContext{}, contextError(http.StatusBadRequest, "context_missing")
	}
	ex := ExchangeContext{holder: g.cfg.HolderID, contentType: r.Header.Get("Content-Type"), bodySHA256: sha256hex(body), policy: g.policy()}
	if ex.contentType == "" {
		ex.contentType = "application/fhir+json"
	}
	var patient string
	var coverages [][]byte
	var resources [][]byte
	switch {
	case strings.HasPrefix(r.URL.Path, "/cds-services/"):
		svc, _, ok := g.advertisedCDSServiceByID(strings.TrimPrefix(r.URL.Path, "/cds-services/"))
		if !ok {
			return missing()
		}
		var in struct {
			Hook    string `json:"hook"`
			Context struct {
				PatientID string `json:"patientId"`
			} `json:"context"`
			Prefetch map[string]json.RawMessage `json:"prefetch"`
		}
		if json.Unmarshal(body, &in) != nil || in.Hook != svc.Hook {
			return missing()
		}
		ex.legType, ex.operation, ex.crdHook = svc.Leg, svc.Leg, in.Hook
		patient = in.Context.PatientID
		if patient != "" && !strings.Contains(patient, "/") {
			patient = "Patient/" + patient
		}
		values := map[string][]byte{}
		for k, v := range in.Prefetch {
			values[k] = v
		}
		resources = prefetchResources(values)
		if coverage := in.Prefetch["coverage"]; len(coverage) > 0 && strings.TrimSpace(string(coverage)) != "null" {
			coverages = append(coverages, coverage)
		}
		if r.Header.Get("Content-Type") == "" {
			ex.contentType = "application/json"
		}
	case r.URL.Path == "/Questionnaire/$questionnaire-package":
		ex.legType, ex.operation = "dtr-questionnaire-fetch", shnsdk.FrameOperationQuestionnairePackage
		var in struct {
			Parameter []struct {
				Name     string          `json:"name"`
				Resource json.RawMessage `json:"resource"`
			} `json:"parameter"`
		}
		if json.Unmarshal(body, &in) != nil {
			return missing()
		}
		for _, p := range in.Parameter {
			resources = append(resources, p.Resource)
			if p.Name == "coverage" {
				coverages = append(coverages, p.Resource)
				if patient == "" {
					patient = patientRefOf(p.Resource)
				}
			}
		}
	case r.URL.Path == "/Claim/$submit" || r.URL.Path == "/Claim/$inquire":
		ex.legType, ex.operation = "pas-claim", "pas-submit"
		if r.URL.Path == "/Claim/$inquire" {
			ex.legType, ex.operation = "pas-claim-inquire", "pas-inquire"
		}
		var in struct {
			Entry []struct {
				Resource json.RawMessage `json:"resource"`
			} `json:"entry"`
		}
		if json.Unmarshal(body, &in) != nil {
			return missing()
		}
		type claimAddress struct {
			ResourceType string `json:"resourceType"`
			Patient      struct {
				Reference string `json:"reference"`
			} `json:"patient"`
			Identifier []struct {
				System string `json:"system"`
				Value  string `json:"value"`
			} `json:"identifier"`
			Related []json.RawMessage `json:"related"`
		}
		var first, operative *claimAddress
		for _, e := range in.Entry {
			resources = append(resources, e.Resource)
			var head claimAddress
			if json.Unmarshal(e.Resource, &head) != nil {
				return missing()
			}
			if head.ResourceType == "Coverage" {
				coverages = append(coverages, e.Resource)
			}
			if head.ResourceType == "Claim" {
				if first == nil {
					first = &head
				}
				if len(head.Related) > 0 && operative == nil {
					operative = &head
				}
			}
		}
		selected := first
		// A prior Claim may precede the operative amendment. Select addressing
		// after the scan, without interpreting the clinical graph or decision.
		if ex.legType == "pas-claim" && operative != nil {
			selected = operative
			ex.legType, ex.operation = "pas-claim-update", "pas-update-submit"
		}
		if selected != nil {
			patient = selected.Patient.Reference
			for _, id := range selected.Identifier {
				if id.System == "urn:shn:correlation" && id.Value != "" {
					ex.correlationID = id.Value
					break
				}
			}
		}

	default:
		return missing()
	}
	if patient == "" || g.cfg.SubjectReferenceResolver == nil {
		return missing()
	}
	ref, ok := patientReference(ex.holder, patient)
	if !ok {
		return missing()
	}
	pci, found, err := g.cfg.SubjectReferenceResolver.ResolveSubject(ctx, ref)
	if err != nil || !found || pci == "" {
		return missing()
	}
	ex.subjectPCI = pci
	for _, coverage := range coverages {
		recipient, _, status, message := g.recipientForWith(coverage, resolverFromResources(resources))
		if status != 0 || recipient == "" {
			// FR-G40 / PCV-06: a known subject with unroutable carried Coverage
			// is a payer-routing refusal, not an absent identity assertion.
			if status != 0 {
				return ExchangeContext{}, &localPayerRoutingError{status: status, message: message}
			}
			return missing()
		}
		if ex.recipient != "" && ex.recipient != recipient {
			return missing()
		}
		ex.recipient = recipient
	}
	if ex.recipient == "" {
		return missing()
	}
	return ex, nil
}

// handleNativeInbound runs after the envelope's authority, replay and ciphertext
// bindings have been verified. A participant-local effect may run only after the
// exact response envelope has been written and flushed; its outcome cannot alter
// that authoritative peer response.
func (g *Gateway) handleNativeInbound(w http.ResponseWriter, r *http.Request, leg string, env shnsdk.Envelope, tok shnsdk.Token, body []byte, answerToken string) {
	ex, ok := r.Context().Value(nativeExchangeKey{}).(ExchangeContext)
	if !ok {
		ex = ExchangeContext{holder: env.Metadata.Sender, recipient: g.cfg.HolderID, legType: leg, subjectPCI: tok.Subject, correlationID: env.Metadata.CorrelationID, operation: RequestFrameOperation(r.Context()), policy: g.policy()}
	}
	spec := paCatalog[leg]
	refuse := func(err error) {
		var ce *conformanceError
		if errors.As(err, &ce) {
			payload, err := ce.refusalPayload(leg)
			if err != nil {
				g.responderFailed(w, leg, err)
				return
			}
			g.respondLegError(w, r, spec.RespFrame, spec.RespOp, leg, ex.correlationID, LegResult{Status: ce.status, Response: payload}, tok.Subject, env.Metadata.Sender, env.Metadata.ConsentRef, answerToken)
			return
		}
		g.responderFailed(w, leg, err)
	}
	if err := g.enforceContent(r.Context(), CheckInput{Exchange: ex, Direction: "request", Body: body, DeclaredVersion: ex.contractVersion,
		finding: contentFinding(r.Context(), ex, "peer", inboundSeamFor(leg))}); err != nil {
		refuse(err)
		return
	}
	responderContext := r.Context()
	if strings.HasPrefix(leg, "pas-claim") {
		responderContext, _ = withPASLeg(responderContext, env.Metadata.Sender)
	}
	result, err := g.handleResponder(responderContext, leg, ex.correlationID, tok.Subject, body)
	committed := false
	if result.Rollback != nil {
		defer func() {
			if !committed {
				result.Rollback()
			}
		}()
	}
	if err != nil {
		g.responderFailed(w, leg, err)
		return
	}
	if result.Status != 0 {
		g.respondLegError(w, r, spec.RespFrame, spec.RespOp, leg, ex.correlationID, result, tok.Subject, env.Metadata.Sender, env.Metadata.ConsentRef, answerToken)
		return
	}
	raw, err := g.admit(result.Response, answerKey(leg, relay.OutcomeAnswered))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": errOwnershipFault})
		return
	}
	declared := stampForBuiltAnswer(result, answerToken)
	responseOwner := "own"
	if result.Response.Ownership() == relay.OwnershipEdited {
		responseOwner = "network"
	}
	if err := g.enforceContent(r.Context(), CheckInput{Exchange: ex, Direction: "response", Status: result.ApplicationStatus, Body: raw, DeclaredVersion: declared,
		finding: contentFinding(r.Context(), ex, responseOwner, inboundSeamFor(leg))}); err != nil {
		refuse(err)
		return
	}
	if leg == "pas-claim-inquire" && result.Commit == nil {
		result.Commit = g.projectPASInquiry(env.Metadata.Sender, tok.Subject, ex.correlationID, body, raw)
	}
	// Only the two baseline PAS projections own a participant-local post-delivery
	// effect. A connector cannot attach local work to another native carrier leg.
	if leg != "pas-claim" && leg != "pas-claim-inquire" {
		result.Commit = nil
	}
	if !g.respondLeg(w, r, spec.RespFrame, spec.RespOp, leg, ex.correlationID, result, tok.Subject, env.Metadata.Sender, env.Metadata.ConsentRef, answerToken) || result.Commit == nil {
		return
	}
	// The submit projection is an existing participant-owned write. Its request
	// subject is checked against the authenticated PCI after delivery, so a local
	// mismatch skips the write without turning clinical parsing into admission.
	if leg == "pas-claim" {
		if _, status, msg := g.conformantPASBindContext(r.Context(), body, tok.Subject); status != 0 {
			g.observe(ObserverEvent{Kind: "pa.local-projection-failed", Direction: "response", LegType: leg, CorrelationID: ex.correlationID, Detail: msg})
			return
		}
	} else if leg == "pas-claim-inquire" {
		facts, status, msg := parsePASInquiryFacts(body)
		if status == 0 {
			var found bool
			var err error
			var pci string
			pci, found, err = g.resolveSubjectPCI(r.Context(), facts.member, body)
			if err != nil {
				status, msg = SoRFailureResponse(err)
			} else if !found {
				status, msg = http.StatusBadRequest, "unknown member"
			} else if pci != tok.Subject {
				status, msg = http.StatusForbidden, "token subject does not match request patient"
			}
		}
		if status != 0 {
			g.observe(ObserverEvent{Kind: "pa.local-projection-failed", Direction: "response", LegType: leg, CorrelationID: ex.correlationID, Detail: msg})
			return
		}
	}
	if err := result.Commit(); err != nil {
		g.observe(ObserverEvent{Kind: "pa.local-projection-failed", Direction: "response", LegType: leg, CorrelationID: ex.correlationID, Detail: err.Error()})
		return
	}
	committed = true
}
