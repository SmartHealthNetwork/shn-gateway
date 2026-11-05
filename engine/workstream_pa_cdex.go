//go:build !shn_coreonly

package engine

import (
	"context"
	"fmt"
	"net/http"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
	"github.com/SmartHealthNetwork/shn-gateway/pa/cdexroute"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The CDex documentation submission (CDex 2.1.0 $submit-attachment): a
// provider's system sends documentation for a prior authorization to a payer,
// through its own gateway, the Hub and the payer's gateway, to the payer's own
// [base]/$submit-attachment. The submission and the payer's answer are carried
// as sent. The network refuses one only on its own primitives: a submission
// that names no registered payer to route to (routing), and one whose
// MemberId it cannot read, so it cannot identify the patient it must check
// authority for (D121).

const legCDexSubmit = "cdex-attachment-submit"

var legCDexAttachmentSubmit = inboundLeg{"payer-coverage", "cdex-attachment-submit-response", legCDexSubmit}

// The CDex submission's ingress routes, as exchange records name them: the
// plain endpoint, routed by the submission's PayerId, and a payer's own
// return path, routed to the payer the path names.
const (
	RouteCDexSubmit       = "cdex-submit"
	RouteCDexReturnSubmit = "cdex-return-submit"
)

// CDexPayersPath is the provider gateway's listing of the payers a
// submission can be addressed to, each with its return path.
const CDexPayersPath = "/cdex/payers"

// cdexLine is the one CDex line the network carries (D127). Its submission
// rides no declared contract line (the leg's catalog contract is ""); the
// line names only the endpoint a payer publishes for it.
const cdexLine = "2.1"

// cdexEndpointContract is the name a payer's published CDex endpoint is kept
// under in the native responder's endpoint evidence ("pa.cdex@2.1"), read
// from the HRex endpoint code davinci_cdex_attachsubmit_endpoint.
const cdexEndpointContract = "pa.cdex"

// cdexEndpointCode is the HRex 1.2.0 endpoint code of a CDex
// $submit-attachment endpoint (CodeSystem hrex-endpoint-discovery).
const cdexEndpointCode = "davinci_cdex_attachsubmit_endpoint"

// PayerIdDiffersEvent is the observer event a provider gateway raises when a
// submission sent to one payer's return path names another payer in its
// PayerId. The submission is carried to the payer the path names; the event
// carries no identifier.
const PayerIdDiffersEvent = "cdex.payer-id-differs"

// cdexSubmission is what the network reads of a $submit-attachment: the
// member the patient is bound by and the payer it is addressed to. Nothing
// else of the submission is read or changed.
type cdexSubmission struct {
	member string
	// payer is the one payer the PayerId values name, nil when there are
	// none or two that differ; payers is every PayerId value, in order.
	payer  *shnsdk.PayerIdentifier
	payers []shnsdk.PayerIdentifier
}

// cdexParameters is the part of a Parameters resource the reader decodes: each
// parameter's name and its identifier value.
type cdexParameters struct {
	ResourceType string `json:"resourceType"`
	Parameter    []struct {
		Name            string `json:"name"`
		ValueIdentifier *struct {
			System string `json:"system"`
			Value  string `json:"value"`
		} `json:"valueIdentifier"`
	} `json:"parameter"`
}

// errCDexSubmissionTwoWays is the refusal of a submission that repeats a
// member name.
const errCDexSubmissionTwoWays = "the submission can be read two ways: it repeats a member name"

// errCDexNoMember is a submission the gateway cannot read one MemberId from.
const errCDexNoMember = "cannot identify the patient: the submission carries no readable MemberId"

// readCDexSubmission reads the member and payer of a $submit-attachment. A
// body that repeats a member name is refused on message integrity, at every
// level. A body that is not a Parameters resource, or carries no MemberId
// value, or two that differ, names no one patient, and is refused: the
// network cannot identify the patient it must check authority for (D121).
// Two PayerId values that differ name no one payer; payer is then nil, and the
// submission is routed as one that names none.
func readCDexSubmission(ctx context.Context, body []byte) (cdexSubmission, int, string) {
	var p cdexParameters
	if err := decodeMessage(body, &p); err != nil {
		if readsTwoWays(err) {
			exchangeOf(ctx).refusingFor(RefusalIntegrity, reasonCDexSubmissionTwoWays)
			return cdexSubmission{}, http.StatusBadRequest, errCDexSubmissionTwoWays
		}
		exchangeOf(ctx).refusingFor(RefusalAuthority, reasonCDexNoMember)
		return cdexSubmission{}, http.StatusBadRequest, errCDexNoMember
	}
	var s cdexSubmission
	members := map[string]bool{}
	for _, param := range p.Parameter {
		if param.ValueIdentifier == nil || param.ValueIdentifier.Value == "" {
			continue
		}
		switch param.Name {
		case "MemberId":
			members[param.ValueIdentifier.Value] = true
			s.member = param.ValueIdentifier.Value
		case "PayerId":
			s.payers = append(s.payers, shnsdk.PayerIdentifier{System: param.ValueIdentifier.System, Value: param.ValueIdentifier.Value})
		}
	}
	if p.ResourceType != "Parameters" || len(members) != 1 {
		exchangeOf(ctx).refusingFor(RefusalAuthority, reasonCDexNoMember)
		return cdexSubmission{}, http.StatusBadRequest, errCDexNoMember
	}
	if len(s.payers) > 0 {
		one := s.payers[0]
		for _, other := range s.payers[1:] {
			if other != one {
				return s, 0, ""
			}
		}
		s.payer = &one
	}
	return s, 0, ""
}

// ---- the provider gateway's ingress ----

// handleCDexSubmitIngress is the provider-facing $submit-attachment: at the
// plain endpoint (POST /$submit-attachment) it is routed by the submission's
// PayerId; at a payer's return path (POST /cdex/payer/{payer}, with or
// without /$submit-attachment) to the payer the path names. Either way the
// submission is carried as sent, and the payer's answer comes back exactly as
// the payer wrote it.
func (g *Gateway) handleCDexSubmitIngress(w http.ResponseWriter, r *http.Request) {
	w, scope := g.withScope(w, relay.RoleRequester)
	w = &fhirOperationWriter{w}
	if g.ingressAuthRefused(w, r) {
		return
	}
	leg := legCDexSubmit
	// Every refusal from here is recorded as this leg's.
	exchangeOf(r.Context()).leg(leg)
	// A submission holds the budget for large bodies until it is answered:
	// its stated length, or the whole ceiling when it states none, taken before
	// any of it is read.
	release, ok := g.holdAttachment(w, r)
	if !ok {
		return
	}
	defer release()
	// The submission is read up to this gateway's attachment ceiling, and one
	// over it is refused 413 as routing before anything parses it. A read that
	// failed keeps its deadline: net/http discards what is left of the body
	// before it answers, and that must not outlast the stall.
	endRead := limitAttachmentRead(w)
	body, ok := readIngressBody(w, r, g.attachmentCeiling())
	if !ok {
		return
	}
	endRead()
	builder, ok := g.ingressBuilder(w, r, leg)
	if !ok {
		return
	}
	r = r.WithContext(withFindingContext(r.Context(), findingContext{LegType: leg, Seam: "provider-ingress", Whose: "own"}))
	sub, status, msg := readCDexSubmission(r.Context(), body)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	recipient, status, msg, reason := g.cdexRecipient(r, sub)
	if status != 0 {
		exchangeOf(r.Context()).routedFor(status, reason)
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	// A file over the largest the payer declares its system accepts is
	// refused before anything is sent: the payer said it cannot take it.
	if status, msg := g.attachmentOverDeclared(recipient, body); status != 0 {
		exchangeOf(r.Context()).refusingFor(RefusalRouting, reasonAttachmentOverDeclared)
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	// The patient is bound by the MemberId: the provider's own patient when its
	// system of record holds the member, otherwise the identifier derived from
	// it, so a member the payer does not hold is carried (unknown is not empty).
	pci, found, readErr := g.resolveSubjectPCI(r.Context(), sub.member, body)
	if readErr != nil {
		status, msg := subjectBindingFailure(r.Context(), readErr)
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	if !found {
		// Only a participant that requires known members gets here: a check
		// it opted into, recorded as conformance.
		exchangeOf(r.Context()).refusing(RefusalConformance)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown member"})
		return
	}
	scope.leg = leg
	ex := g.exchanges.Begin(workstreamPA)
	child := g.ingressCorrelation(w, r)
	request, ok := sealModuleRequest(w, builder, relay.Exact(relay.NewBody(body, relay.OriginIngressRequest), "application/fhir+json"), body)
	if !ok {
		return
	}
	content := Content{WorkstreamType: workstreamPA, Payload: request, Carried: true, OnBehalf: builder != ""}
	var stated statedAnswer
	var answerType string
	answer, err := g.OriginateLeg(withStatedAnswerSink(withAnswerMediaTypeSink(r.Context(), &answerType), &stated), r, recipient, leg, pci, child, "", content)
	spec, _ := legSpecFor(leg)
	legProj := Leg{Type: leg, Physics: spec.Physics, Content: content, Subjects: []string{pci}}
	if err != nil {
		g.recordLeg(ex.ID, legProj.Project(child, "error"))
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	answerCtx := withFindingContext(r.Context(), findingContext{LegType: leg, CorrelationID: child, Seam: "provider-ingress", Whose: "peer"})
	if repeatsAMember(answerCtx, answer) {
		g.recordLeg(ex.ID, legProj.Project(child, "error"))
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the payer's answer can be read two ways: it repeats a member name"})
		return
	}
	g.recordLeg(ex.ID, legProj.Project(child, "answered"))
	// The payer's own media type, as on the other carried legs.
	ct := carriedFHIRMediaType(answerType)
	if ct == "" {
		ct = mediaFHIRJSON
	}
	g.writeRelayedAnswer(w, stated, ct,
		relay.Exact(relay.NewBody(answer, relay.OriginPeerFrame), ct),
		relay.Key{Leg: leg, Role: relay.RoleRequester, Direction: relay.DirectionResponse, Outcome: relay.OutcomeAnswered})
}

// cdexRecipient is the payer a submission is routed to. At a payer's return
// path it is that payer, which must be a registered payer; a PayerId that
// names another payer, or none the router knows, is carried and the
// difference observed, one event for the submission. At the plain
// endpoint it is the registered payer the PayerId names; none, or one naming
// no registered payer, is refused (routing): there is no default payer.
func (g *Gateway) cdexRecipient(r *http.Request, sub cdexSubmission) (string, int, string, refusalReason) {
	if path := r.PathValue("payer"); path != "" {
		if e, ok := g.cfg.Reg.Lookup(path); !ok || e.Role != "payer" {
			return "", http.StatusUnprocessableEntity, errCDexReturnPathNoPayer, reasonCDexReturnPathNoPayer
		}
		for _, p := range sub.payers {
			if named, ok := g.resolvePayer(p); !ok || named != path {
				g.observe(ObserverEvent{Kind: PayerIdDiffersEvent, Direction: "ingress", LegType: legCDexSubmit})
				break
			}
		}
		return path, 0, "", ""
	}
	if sub.payer == nil {
		return "", http.StatusUnprocessableEntity, errCDexSubmissionNoPayer, reasonCDexSubmissionNoPayer
	}
	holder, ok := g.resolvePayer(*sub.payer)
	if !ok {
		return "", http.StatusUnprocessableEntity, fmt.Sprintf("no registered payer for identifier %s|%s", sub.payer.System, sub.payer.Value), reasonUnregisteredPayer
	}
	return holder, 0, "", ""
}

// The routing refusals of a submission that names no payer to route to.
const (
	errCDexReturnPathNoPayer = "this return path names no registered payer"
	errCDexSubmissionNoPayer = "the submission names no payer to route to: send it to a payer's return path, or name one PayerId"
)

// resolvePayer is the payer router's holder for p, false with no router.
func (g *Gateway) resolvePayer(p shnsdk.PayerIdentifier) (string, bool) {
	if g.cfg.PayerRouter == nil {
		return "", false
	}
	return g.cfg.PayerRouter.Resolve(p)
}

// cdexPayer is one entry of the provider gateway's payer listing.
type cdexPayer struct {
	Participant      string                   `json:"participant"`
	PayerIdentifiers []shnsdk.PayerIdentifier `json:"payerIdentifiers"`
	ReturnBase       string                   `json:"returnBase"`
}

// cdexPayers lists every registered payer that takes the documentation
// submission (payerTakesDocumentation, the one test the pend re-key reads too), in
// participant id order, with the return path a submission to it is addressed to
// (cdexroute.ReturnBase): a listed path works, and none that always refuses is
// advertised. The registry is the one the
// gateway converged from the registrar before it served (a gateway that cannot
// converge it does not start), so a registry with no payer lists none. A
// return path that cannot be built is an error, never a shorter list.
func (g *Gateway) cdexPayers() ([]cdexPayer, error) {
	out := []cdexPayer{}
	for _, id := range g.cfg.Reg.IDs() {
		e, ok := g.cfg.Reg.Lookup(id)
		if !ok || e.Role != "payer" || !g.payerTakesDocumentation(id) {
			continue
		}
		base, err := cdexroute.ReturnBase(g.cfg.IngressBaseURL, id)
		if err != nil {
			return nil, err
		}
		ids := append([]shnsdk.PayerIdentifier{}, e.PayerIDs...)
		out = append(out, cdexPayer{Participant: id, PayerIdentifiers: ids, ReturnBase: base})
	}
	return out, nil
}

// handleCDexPayers is GET /cdex/payers: the payers a provider's system can
// address a submission to, each with its return path.
func (g *Gateway) handleCDexPayers(w http.ResponseWriter, r *http.Request) {
	if g.ingressAuthRefused(w, r) {
		return
	}
	payers, err := g.cdexPayers()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"payers": payers})
}

// ---- the payer gateway ----

// handleCDexSubmitInbound serves the submission payer-side. Authority is
// evaluated independently here, as on every leg: the payer binds the member
// the submission names by its own system (bindInboundSubject), and a
// submission it cannot read a MemberId from is refused. The submission goes to
// the payer's own $submit-attachment exactly, and its answer is relayed as the
// payer wrote it.
func (g *Gateway) handleCDexSubmitInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, body []byte, answerTok string) {
	ctx := r.Context()
	// A submission over this gateway's own attachment ceiling is refused 413
	// as routing, before anything parses it or the payer's system is called.
	if int64(len(body)) > g.attachmentCeiling() {
		exchangeOf(ctx).refusing(RefusalRouting)
		g.refuseInbound(w, r, legCDexAttachmentSubmit, env, tok, answerTok, http.StatusRequestEntityTooLarge, errRequestTooLarge, nil)
		return
	}
	sub, status, msg := readCDexSubmission(ctx, body)
	if status != 0 {
		g.refuseInbound(w, r, legCDexAttachmentSubmit, env, tok, answerTok, status, msg, nil)
		return
	}
	subjectPCI, status, msg := g.bindInboundSubject(ctx, sub.member, body)
	if status != 0 {
		g.refuseInbound(w, r, legCDexAttachmentSubmit, env, tok, answerTok, status, msg, nil)
		return
	}
	g.noteSubjectBinding(ctx, legCDexSubmit, env.Metadata.CorrelationID, tok.Subject, subjectPCI)
	result, err := g.cfg.Responder.Handle(ctx, legCDexSubmit, env.Metadata.CorrelationID, subjectPCI, body)
	if err != nil {
		g.responderFailed(w, r, legCDexAttachmentSubmit, env, tok, answerTok, err)
		return
	}
	if result.Status != 0 {
		g.respondLegError(w, r, "payer-coverage", "cdex-attachment-submit-response", legCDexSubmit,
			env.Metadata.CorrelationID, result, tok.Subject, env.Metadata.Sender, "", answerTok)
		return
	}
	g.respondLeg(w, r, "payer-coverage", "cdex-attachment-submit-response", legCDexSubmit, env.Metadata.CorrelationID,
		result.Response, result.stated(), tok.Subject, env.Metadata.Sender, "", answerTok)
}

// cdexSubmitURL resolves the payer's $submit-attachment: under the CDex base
// the payer publishes in its own
// .well-known/davinci-configuration (HRex 1.2.0 defines the
// davinci_cdex_attachsubmit_endpoint value as the base the operation is
// invoked on; kept only when same-origin with the payer's base,
// SetEndpointEvidence), else under the payer's base (PAYER_DAVINCI_BASE_URL),
// with the operation's name, as CDex dictates.
func (n *nativeResponder) cdexSubmitURL() string {
	base := n.baseURL
	n.evMu.RLock()
	if u, ok := n.endpointEvidence[cdexEndpointContract+"@"+cdexLine]; ok {
		base = u
	}
	n.evMu.RUnlock()
	return publishedOperationURL(base, "/$submit-attachment")
}

// forwardCDexSubmit sends a submission to the payer's own $submit-attachment
// exactly, and relays whatever comes back with the payer's own media type: a
// 2xx OperationOutcome, informational or not, and an error alike.
func (n *nativeResponder) forwardCDexSubmit(ctx context.Context, in relay.Body) (LegResult, error) {
	request, err := restoredRequest(in, "application/fhir+json", n.adaptiveRestore(ctx, in))
	if err != nil {
		return LegResult{}, err
	}
	up, bad, err := n.post(endpointcert.WithTag(ctx, endpointcert.Of(endpointcert.PayerDavinci)), n.cdexSubmitURL(), "",
		request, legCDexSubmit, "CDex submit-attachment")
	if err != nil {
		return LegResult{}, err
	}
	if bad.Status != 0 {
		return bad, nil
	}
	return LegResult{Response: relay.Exact(up.body, up.contentType), AnswerStatus: up.status, AnswerHeaders: up.headers, ResponseSubjectForeign: true}, nil
}
